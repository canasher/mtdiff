package sync

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"mtdiff/internal/chunk"
	"mtdiff/internal/conn"
)

// Stats reports what was actually committed for one table. On a mid-apply
// failure Error is set and the counters show the committed progress of the
// chunk groups that completed before the failure (the failing group was
// rolled back and contributes nothing).
type Stats struct {
	Table     string
	Mode      string
	Truncated bool
	Inserts   int
	Updates   int
	Deletes   int
	Chunks    int
	Error     string
	// MaxBufferedDeleteKeys is the peak number of destination key
	// vectors held at once in a streaming delete pass (the O(chunk)/
	// O(batch) bound — it must stay at the batch size, not grow with
	// the table). MaxBufferedOps is the peak ops buffered for one
	// chunk group (the ordinary-row-level O(chunk-delta) bound).
	MaxBufferedDeleteKeys int
	MaxBufferedOps        int
}

// Applier executes sync statements on the destination's dedicated write
// connection, reading source data through the source's read-only scan
// pool. One chunk group is one transaction: a failure rolls back that
// group's writes only, records the error and stops the table.
type Applier struct {
	W        *conn.Writer
	Src      *conn.Side
	Batch    int // rows per multi-row INSERT
	MaxBytes int // rendered-bytes budget per multi-row INSERT
	// Progress receives apply phase updates (truncate, committed-chunk
	// percentages). nil = no progress output.
	Progress func(format string, args ...any)

	// execHook, when set, replaces execDirect's statement execution
	// (unit tests count calls and force failures); nil in production.
	execHook func(ctx context.Context, query string) error
}

// ApplyFull runs the FULL mode: TRUNCATE the destination table, then
// stream every source row into it in chunk-sized transactions. srcTotal is
// the freshly re-counted source row count (the caller recounts right
// before applying). The schema's key columns may be empty (keyless
// tables): the planner then emits one whole-table chunk.
func (a *Applier) ApplyFull(ctx context.Context, st *Stats, b *Builder, schema *conn.Schema, keyFams []string, srcTotal int64) {
	// TRUNCATE is DDL (implicit commit): run it alone, outside any
	// transaction, after all earlier chunk transactions have finished.
	if err := a.execDirect(ctx, b.Truncate()); err != nil {
		st.Error = fmt.Sprintf("truncate: %v", err)
		return
	}
	st.Truncated = true
	a.resync(ctx, st, b, schema, keyFams, srcTotal)
}

// resync streams every source row into a destination table that is already
// empty (the caller has truncated it), in chunk-sized transactions. The
// structure-sync path reuses it after it has truncated and re-shaped the
// table: the resync must not truncate again.
func (a *Applier) resync(ctx context.Context, st *Stats, b *Builder, schema *conn.Schema, keyFams []string, srcTotal int64) {
	var chunks []chunk.Chunk
	if srcTotal > 0 {
		p := chunk.Planner{
			Table:       schema.Table,
			KeyCols:     schema.Key,
			KeyFamilies: keyFams,
			ChunkSize:   a.Batch,
		}
		if err := a.Src.WithControl(ctx, func(q conn.Queryer) error {
			var err error
			chunks, err = p.Plan(ctx, q, srcTotal)
			return err
		}); err != nil {
			st.Error = fmt.Sprintf("plan: %v", err)
			return
		}
	}
	st.Chunks = len(chunks)
	step := len(chunks) / 10
	if step < 1 {
		step = 1
	}
	for i, ch := range chunks {
		err := a.applyTx(ctx, func(tx *sql.Tx) error {
			return a.streamChunk(ctx, tx, st, b, schema, ch)
		})
		if err != nil {
			st.Error = fmt.Sprintf("chunk %d: %v", ch.ID, err)
			return
		}
		if a.Progress != nil && (i+1)%step == 0 {
			a.Progress("  %-24s resync %3d%% (%d/%d chunks, %d rows)", schema.Table, 100*(i+1)/len(chunks), i+1, len(chunks), st.Inserts)
		}
	}
}

// deleteBatchCap is the key-row limit of one batched DELETE (P2): the
// configured batch, further capped by the bind-parameter budget — a
// key batch binds one placeholder per key COMPONENT, so a composite
// key of k columns shrinks the batch to maxBindParams/k. The floor is
// 1: even a key wider than the whole budget deletes one row per
// statement rather than failing.
func deleteBatchCap(batch, keyCols int) int {
	cap := batch
	if keyCols > 0 {
		if c := maxBindParams / keyCols; cap > c {
			cap = c
		}
	}
	if cap < 1 {
		cap = 1
	}
	return cap
}

// batchCap is the row limit of one multi-row INSERT (P2-3): the
// configured Batch, further capped by the bind-parameter budget
// (maxBindParams / writable columns) — a wide table shrinks its batch
// automatically, while the MaxBytes flush condition still applies on top.
// A table so wide that ONE row already exceeds the budget is an explicit
// error: a row cannot be split across statements.
func (a *Applier) batchCap(b *Builder) (int, error) {
	cols := b.WritableCols()
	if cols > maxBindParams {
		return 0, fmt.Errorf("table %s has %d writable columns: a single row already exceeds the %d bind-parameter budget — the row cannot be split across statements", b.Table, cols, maxBindParams)
	}
	batch := a.Batch
	if cols > 0 {
		if cap := maxBindParams / cols; batch > cap {
			batch = cap
		}
	}
	if batch < 1 {
		batch = 1
	}
	return batch, nil
}

// ApplyOps runs the ROWLEVEL mode over the ops grouped by chunk: one
// transaction per group, so a failure rolls back that group's writes only
// and the counters count committed rows only. Within a group, DELETEs
// accumulate into a batch and execute as one statement (DeleteBatchExec,
// no per-row round trip — P2), flushed before the first non-delete op
// (a key group's deletes still precede its inserts, keeping unique slots
// free; a rewrite's re-inserts run only after its own deletes), UPDATEs
// execute one statement at a time in engine order, and INSERTs are
// batched as multi-row statements and flushed on the batch limits (row
// count, or rendered bytes to protect max_allowed_packet) or at the end.
func (a *Applier) ApplyOps(ctx context.Context, st *Stats, b *Builder, chunked [][]op) {
	batch, err := a.batchCap(b)
	if err != nil {
		st.Error = err.Error()
		return
	}
	delCap := deleteBatchCap(batch, len(b.keyIdx))
	step := len(chunked) / 10
	if step < 1 {
		step = 1
	}
	for gi, ops := range chunked {
		if len(ops) == 0 {
			continue
		}
		c := &groupCounts{}
		err := a.applyTx(ctx, func(tx *sql.Tx) error {
			return runOpsGroup(ops, b, batch, delCap, a.MaxBytes, st, c,
				func(stmt string, args ...any) (sql.Result, error) {
					return tx.ExecContext(ctx, stmt, args...)
				})
		})
		if err != nil {
			st.Error = fmt.Sprintf("chunk group %d: %v", gi, err)
			return
		}
		st.Inserts += c.ins
		st.Updates += c.upd
		st.Deletes += c.del
		st.Chunks++
		if a.Progress != nil && (gi+1)%step == 0 {
			a.Progress("  %-24s row-level %3d%% (%d/%d chunks, %d rows written)", st.Table,
				100*(gi+1)/len(chunked), gi+1, len(chunked), st.Inserts+st.Updates+st.Deletes)
		}
	}
}

// groupCounts is one chunk group's committed counters, credited to the
// Stats only after the group's transaction commits.
type groupCounts struct {
	ins, upd, del int
}

// runOpsGroup executes one chunk group's ops through exec (one bound
// statement per flush) in the engine order that keeps unique slots free:
// DELETEs accumulate into a batch and run as one statement (DeleteBatchExec
// — no per-row round trip, P2), flushed at the cap, before the first
// non-delete op, and before a rewrite's re-inserts (they need the slots
// the deletes free); UPDATEs run one statement at a time; INSERTs batch
// and flush on the row-count and rendered-bytes limits (max_allowed_packet)
// or at the end.
//
// exec is the statement seam: production passes the group transaction's
// ExecContext, a failing exec stops the group (the caller rolls the
// transaction back — the completed groups before it stand), and the tests
// pass a recorder. c is credited per successful statement; st.MaxBuffered
// DeleteKeys tracks the widest delete batch actually run.
func runOpsGroup(ops []op, b *Builder, batch, delCap, maxBytes int, st *Stats, c *groupCounts, exec func(string, ...any) (sql.Result, error)) error {
	var pending [][]any
	var pendingBytes int
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		// the executable INSERT is parameterized (P0-3): the values
		// travel as bound arguments, never rendered into the statement
		// text
		stmt, args, err := b.InsertBatchExec(pending)
		if err != nil {
			return err
		}
		if _, err := exec(stmt, args...); err != nil {
			return err
		}
		c.ins += len(pending)
		pending = nil
		pendingBytes = 0
		return nil
	}
	addInsert := func(row []any) error {
		rb := b.rowBytes(row)
		if maxBytes > 0 && len(pending) == 0 && rb > maxBytes {
			return fmt.Errorf("a single row renders to %d bytes, over the %d-byte budget: raise --max-allowed-packet", rb, maxBytes)
		}
		if len(pending) > 0 && (len(pending) >= batch || (maxBytes > 0 && pendingBytes+rb > maxBytes)) {
			if err := flush(); err != nil {
				return err
			}
		}
		pending = append(pending, row)
		pendingBytes += rb
		return nil
	}
	var delBatch [][]any
	flushDeletes := func() error {
		if len(delBatch) == 0 {
			return nil
		}
		cur := delBatch
		delBatch = delBatch[:0]
		if len(cur) > st.MaxBufferedDeleteKeys {
			st.MaxBufferedDeleteKeys = len(cur)
		}
		stmt, args, err := b.DeleteBatchExec(cur)
		if err != nil {
			return err
		}
		n, err := exec(stmt, args...)
		if err != nil {
			return err
		}
		na, _ := n.RowsAffected()
		c.del += int(na)
		return nil
	}
	for _, o := range ops {
		switch o.kind {
		case opDelete:
			delBatch = append(delBatch, o.key)
			if len(delBatch) >= delCap {
				if err := flushDeletes(); err != nil {
					return err
				}
			}
		case opUpdate:
			// a pending delete precedes this update in engine order:
			// flush before it runs
			if err := flushDeletes(); err != nil {
				return err
			}
			stmt, args := b.UpdateExec(o.key, o.rows[0])
			if _, err := exec(stmt, args...); err != nil {
				return err
			}
			c.upd++
		case opRewrite:
			// the unique-value-swap protection rewrites a no-op holder:
			// delete the whole destination group, then re-insert the same
			// rows (carried in the op, read from the source). Every row's
			// raw key is deleted — the group's rows can carry distinct
			// raw keys that only fold together under the normalizer, so a
			// single-key delete would leave the rest behind. The re-inserts
			// run only after the group's own deletes (they free the unique
			// slots).
			if err := flushDeletes(); err != nil {
				return err
			}
			for _, k := range o.delKeys {
				delBatch = append(delBatch, k)
				if len(delBatch) >= delCap {
					if err := flushDeletes(); err != nil {
						return err
					}
				}
			}
			if err := flushDeletes(); err != nil {
				return err
			}
			for _, row := range o.rows {
				if err := addInsert(row); err != nil {
					return err
				}
			}
		case opInsert:
			if err := flushDeletes(); err != nil {
				return err
			}
			if err := addInsert(o.rows[0]); err != nil {
				return err
			}
		}
	}
	if err := flushDeletes(); err != nil {
		return err
	}
	return flush()
}

// streamChunk reads one source chunk row by row and batch-INSERTs it.
// A batch flushes at Batch rows or when the rendered statement exceeds
// MaxBytes (protects max_allowed_packet on BLOB-heavy tables).
func (a *Applier) streamChunk(ctx context.Context, tx *sql.Tx, st *Stats, b *Builder, schema *conn.Schema, ch chunk.Chunk) error {
	batch, err := a.batchCap(b)
	if err != nil {
		return err
	}
	cn, err := a.Src.AcquireScan(ctx)
	if err != nil {
		return err
	}
	defer cn.Close()

	idents := make([]string, len(schema.Cols))
	for i, c := range schema.Cols {
		idents[i] = conn.QuoteIdent(c.Name)
	}
	query := fmt.Sprintf("SELECT %s FROM %s", strings.Join(idents, ", "), conn.QuoteIdent(schema.Table))
	// parameterized: the key bounds are bound on the server side (P0-1)
	pred := ch.Pred(schema.Key, "")
	if pred.SQL != "" {
		query += " WHERE " + pred.SQL
	}
	rows, err := cn.QueryContext(ctx, query, pred.Args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	vals := make([]any, len(schema.Cols))
	ptrs := make([]any, len(schema.Cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	var pending [][]any
	var pendingBytes int
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		// parameterized: the streamed rows travel as bound arguments
		stmt, args, err := b.InsertBatchExec(pending)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
			return err
		}
		st.Inserts += len(pending)
		pending = nil
		pendingBytes = 0
		return nil
	}
	for {
		if !rows.Next() {
			break
		}
		for i := range vals {
			vals[i] = nil
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		rowBytes := b.rowBytes(vals)
		if a.MaxBytes > 0 && len(pending) == 0 && rowBytes > a.MaxBytes {
			return fmt.Errorf("a single row renders to %d bytes, over the %d-byte budget: raise --max-allowed-packet", rowBytes, a.MaxBytes)
		}
		if len(pending) > 0 && (len(pending) >= batch || (a.MaxBytes > 0 && pendingBytes+rowBytes > a.MaxBytes)) {
			if err := flush(); err != nil {
				return err
			}
		}
		cp := make([]any, len(vals))
		copy(cp, vals)
		pending = append(pending, cp)
		pendingBytes += rowBytes
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return flush()
}

func (a *Applier) applyTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	cn, err := a.W.Conn(ctx)
	if err != nil {
		return err
	}
	defer cn.Close()
	tx, err := cn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// execDirect runs a statement outside any transaction (used for the
// TRUNCATE, which is DDL and commits implicitly).
func (a *Applier) execDirect(ctx context.Context, query string) error {
	if a.execHook != nil {
		return a.execHook(ctx, query)
	}
	cn, err := a.W.Conn(ctx)
	if err != nil {
		return err
	}
	defer cn.Close()
	_, err = cn.ExecContext(ctx, query)
	return err
}
