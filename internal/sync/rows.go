package sync

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"mtdiff/internal/chunk"
	"mtdiff/internal/conn"
	"mtdiff/internal/normalize"
)

// opKind classifies one row-level statement.
type opKind int

const (
	opInsert opKind = iota
	opUpdate
	opDelete
	// opRewrite deletes a destination key group and re-inserts the
	// source rows carried in rows: the unique-value-swap protection
	// (diffUniqueProtected) rewrites a NO-OP row that still holds a value
	// another row of the chunk is about to take, freeing the unique slot
	// without changing any data.
	opRewrite
)

// op is one row-level statement to apply to the destination.
//
// key holds the raw key values in key order: for opInsert the source row's
// key, for opUpdate/opDelete the DESTINATION row's key, and for opRewrite
// the FIRST destination row's key (the full set is in delKeys). The WHERE
// clause must address what is actually stored in the destination;
// normalized key identity can be coarser than raw values (e.g. trailing-
// space folding), so a group's rows may carry distinct raw keys. rows is
// the source row(s) to write (one for insert/update, every row of the
// group for a rewrite).
type op struct {
	kind opKind
	key  []any
	// delKeys holds the raw key of EVERY destination row the op deletes.
	// Set only for opRewrite: a no-op group's rows can carry distinct raw
	// keys (e.g. 'x' and 'x ' fold to one normalized identity, but a
	// no-pad collation still distinguishes them in SQL), so deleting only
	// the first row's key would leave the rest behind.
	delKeys [][]any
	rows    [][]any
	// rewriteFP is the rewrite group's STABLE IDENTITY (P0): the
	// confirmed scope carries one fingerprint per group, and the apply
	// may run only groups the confirmed plan showed — a re-plan that
	// keeps the count but changes the group is a scope expansion.
	// Set only for opRewrite ("" otherwise).
	rewriteFP string
}

// srow is one buffered row.
type srow struct {
	vals  []any  // raw driver values, schema.Cols order (key columns included)
	canon string // normalized bytes of the full compared column set
}

// uniqueConstraint is one unique constraint (primary key or unique index)
// mapped onto both sides' compared columns. A conflict is the whole
// TUPLE of the constraint (P1-5): a composite UNIQUE(a,b) does not make
// a or b individually unique, and two different constraints never
// cross-collide. A tuple with a NULL component never occupies a slot
// (MySQL allows repeated NULLs in a unique index), so only all-non-NULL
// tuples are tracked.
type uniqueConstraint struct {
	cols   []string // column names in index order (the constraint's identity)
	srcIdx []int    // position of each col in the source Cols, parallel
	dstIdx []int    // position of each col in the destination Cols, parallel
}

// Engine computes the row-level operations of a keyed table: it buffers one
// chunk per side and derives INSERT/UPDATE/DELETE from the key-group
// difference.
//
// Row identity is the normalizer's canonical bytes of the key columns —
// injective by the normalizer's contract, unlike the display-oriented
// lookup key of the drill-down, which truncates long string components and
// would be unsafe to drive DELETE/UPDATE targeting.
//
// Memory is bounded by the chunk size: the engine only ever buffers rows of
// one chunk at a time (keyless tables never reach it; they go through the
// FULL resync path).
type Engine struct {
	srcRow, dstRow *normalize.Normalizer // full compared columns, per side
	srcKey, dstKey *normalize.Normalizer // key columns only, per side
	unique         bool                  // PK or NOT-NULL unique index
	keyIdx         []int                 // Cols position of each key column, in key order
	// constraints (P1-5): the unique constraints of EITHER side, mapped
	// onto the compared columns of both sides (a constraint whose
	// columns are not all present on one side is dropped — the runner
	// rejects an --ignore-columns entry naming a constraint column
	// before the engine is built). They drive the unique-value-swap
	// protection in Diff and the cross-chunk holder check.
	uc []uniqueConstraint
}

// NewEngine builds the engine. Both schemas are the prepared (ignored
// columns removed) schemas; the key columns must be part of Cols (the
// runner rejects an --ignore-columns entry that names a key column).
// srcUniq / dstUniq are the sides' unique constraints (primary key
// first); the union is matched by column sequence (case-insensitively).
func NewEngine(srcRow, dstRow, srcKey, dstKey *normalize.Normalizer, unique bool, keyCols, cols, dstCols []conn.Column, srcUniq, dstUniq []conn.UniqueConstraint) *Engine {
	pos := make(map[string]int, len(cols))
	for i, c := range cols {
		pos[strings.ToLower(c.Name)] = i
	}
	dpos := make(map[string]int, len(dstCols))
	for i, c := range dstCols {
		dpos[strings.ToLower(c.Name)] = i
	}
	e := &Engine{srcRow: srcRow, dstRow: dstRow, srcKey: srcKey, dstKey: dstKey, unique: unique}
	for _, k := range keyCols {
		if i, ok := pos[strings.ToLower(k.Name)]; ok {
			e.keyIdx = append(e.keyIdx, i)
		}
	}
	// union of the two sides' constraints by column sequence: a
	// constraint missing one column on one side cannot be evaluated and
	// is dropped (the runner rejects that combination up front)
	seen := make(map[string]bool)
	for _, side := range [][]conn.UniqueConstraint{srcUniq, dstUniq} {
		for _, u := range side {
			id := strings.ToLower(strings.Join(u.Cols, "\x1f"))
			if seen[id] {
				continue
			}
			seen[id] = true
			var c uniqueConstraint
			c.cols = u.Cols
			ok := true
			for _, n := range u.Cols {
				if i, ok2 := pos[strings.ToLower(n)]; ok2 {
					c.srcIdx = append(c.srcIdx, i)
				} else {
					ok = false
					break
				}
			}
			for _, n := range u.Cols {
				if i, ok2 := dpos[strings.ToLower(n)]; ok2 {
					c.dstIdx = append(c.dstIdx, i)
				} else {
					ok = false
					break
				}
			}
			if ok {
				e.uc = append(e.uc, c)
			}
		}
	}
	return e
}

// keyVals extracts the raw key values of a row (key order).
func (e *Engine) keyVals(vals []any) []any {
	out := make([]any, len(e.keyIdx))
	for i, idx := range e.keyIdx {
		out[i] = vals[idx]
	}
	return out
}

// buffer consumes a row iterator (nil, false = stream ended) and groups the
// rows by normalized key identity. The iterator may hand out a reused
// buffer (the scan loop does), so each row's values are copied.
func (e *Engine) buffer(norm, keyNorm *normalize.Normalizer, next func() ([]any, bool)) (map[string][]*srow, error) {
	out := make(map[string][]*srow)
	buf := make([]byte, 0, 4096)
	kbuf := make([]byte, 0, 256)
	for {
		vals, ok := next()
		if !ok {
			return out, nil
		}
		canon, err := norm.Normalize(vals, buf)
		if err != nil {
			return nil, err
		}
		keyCanon, err := keyNorm.Normalize(e.keyVals(vals), kbuf)
		if err != nil {
			return nil, err
		}
		buf, kbuf = canon[:0], keyCanon[:0] // reuse the (possibly grown) buffer, as the chunk scanner does
		cp := make([]any, len(vals))
		copy(cp, vals)
		out[string(keyCanon)] = append(out[string(keyCanon)], &srow{vals: cp, canon: string(canon)})
	}
}

// scanSide streams one side's rows for one chunk through the side's
// read-only scan pool, grouped by normalized key identity. The
// connection is taken from the scan pool and closed on return; the
// sync path instead keeps one connection per side for the whole table
// (P2-1) and calls scanSideConn.
func (e *Engine) scanSide(ctx context.Context, side *conn.Side, schema *conn.Schema, norm, keyNorm *normalize.Normalizer, ch chunk.Chunk, where string) (map[string][]*srow, error) {
	cn, err := side.AcquireScan(ctx)
	if err != nil {
		return nil, err
	}
	defer cn.Close()
	return e.scanSideConn(ctx, cn, schema, norm, keyNorm, ch, where)
}

// scanSideConn is scanSide over a caller-owned scan connection.
func (e *Engine) scanSideConn(ctx context.Context, cn *sql.Conn, schema *conn.Schema, norm, keyNorm *normalize.Normalizer, ch chunk.Chunk, where string) (map[string][]*srow, error) {
	idents := make([]string, len(schema.Cols))
	for i, c := range schema.Cols {
		idents[i] = conn.QuoteIdent(c.Name)
	}
	query := fmt.Sprintf("SELECT %s FROM %s", strings.Join(idents, ", "), conn.QuoteIdent(schema.Table))
	// parameterized: the key bounds are bound on the server side (P0-1)
	pred := ch.Pred(schema.Key, where)
	if pred.SQL != "" {
		query += " WHERE " + pred.SQL
	}
	rows, err := cn.QueryContext(ctx, query, pred.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	vals := make([]any, len(schema.Cols))
	ptrs := make([]any, len(schema.Cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	next := func() (row []any, ok bool) {
		if !rows.Next() {
			return nil, false
		}
		for i := range vals {
			vals[i] = nil
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, false // rows.Err() below carries the failure
		}
		return vals, true
	}
	m, err := e.buffer(norm, keyNorm, next)
	if err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return m, nil
}

// scanKeyRows streams one side's key columns (in key order) through the
// side's read-only scan pool, under a parameterized key-bounds predicate
// (chunk.Pred, P0-1) ANDed with the optional --where filter, and returns
// each row's raw key values. It serves the out-of-range delete scan,
// which needs nothing but the destination rows' keys. The connection is
// taken from the scan pool and closed on return.
func (e *Engine) scanKeyRows(ctx context.Context, side *conn.Side, schema *conn.Schema, pred chunk.Pred, where string) ([][]any, error) {
	cn, err := side.AcquireScan(ctx)
	if err != nil {
		return nil, err
	}
	defer cn.Close()
	idents := make([]string, len(schema.Key))
	for i, k := range schema.Key {
		idents[i] = conn.QuoteIdent(k)
	}
	query := fmt.Sprintf("SELECT %s FROM %s", strings.Join(idents, ", "), conn.QuoteIdent(schema.Table))
	var conds []string
	var args []any
	if pred.SQL != "" {
		conds = append(conds, "("+pred.SQL+")")
		args = append(args, pred.Args...)
	}
	if where != "" {
		conds = append(conds, "("+where+")")
	}
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	rows, err := cn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	vals := make([]any, len(schema.Key))
	ptrs := make([]any, len(schema.Key))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	var out [][]any
	for rows.Next() {
		for i := range vals {
			vals[i] = nil
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		cp := make([]any, len(vals))
		copy(cp, vals)
		out = append(out, cp)
	}
	return out, rows.Err()
}

// Diff derives the row-level operations between two buffered sides. Ops
// are deterministic: groups are processed in sorted key-identity order, and
// within a replaced group every destination row is deleted before any
// source row is inserted (deleting first keeps unique slots free).
//
// The boolean result reports whether the chunk needed a DESTRUCTIVE ROW
// REWRITE (P0-2): a unique-value swap, cycle, or a writer past a no-op
// holder cannot be applied by per-row UPDATEs in any order, so the chunk
// is re-emitted as two phases — every DELETE first, then every INSERT
// (converted updates and rewritten no-op holders included). A rewrite
// deletes and re-inserts rows the user did not ask to change: FK
// ON DELETE CASCADE, triggers and audit logs all fire for them, so the
// runner REFUSES the table by default and only emits rewrites when
// --allow-row-rewrite is set.
func (e *Engine) Diff(srcM, dstM map[string][]*srow) ([]op, bool) {
	keys := make([]string, 0, len(srcM)+len(dstM))
	seen := make(map[string]bool, len(srcM)+len(dstM))
	for k := range srcM {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for k := range dstM {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(e.uc) == 0 {
		return e.diffPlain(keys, srcM, dstM), false
	}
	return e.diffUniqueProtected(keys, srcM, dstM)
}

// diffPlain is the classic per-group emission (no unique non-key columns:
// a unique KEY already makes every single-row update safe in any order).
func (e *Engine) diffPlain(keys []string, srcM, dstM map[string][]*srow) []op {
	ops := make([]op, 0, len(keys))
	for _, k := range keys {
		s, d := srcM[k], dstM[k]
		switch {
		case len(d) == 0:
			// destination is missing every row of this key group
			for _, r := range s {
				ops = append(ops, op{kind: opInsert, key: e.keyVals(r.vals), rows: [][]any{r.vals}})
			}
		case len(s) == 0:
			// source has no row for this key group
			for _, r := range d {
				ops = append(ops, op{kind: opDelete, key: e.keyVals(r.vals)})
			}
		case e.sameMultiset(s, d):
			// identical under the configured normalization: nothing to do
		case e.unique && len(s) == 1 && len(d) == 1:
			ops = append(ops, op{kind: opUpdate, key: e.keyVals(d[0].vals), rows: [][]any{s[0].vals}})
		default:
			// non-unique key (or a normalized key collision, e.g. case
			// folding on a case-sensitive column): replace the whole group —
			// delete every destination row, then insert every source row.
			for _, r := range d {
				ops = append(ops, op{kind: opDelete, key: e.keyVals(r.vals)})
			}
			for _, r := range s {
				ops = append(ops, op{kind: opInsert, key: e.keyVals(r.vals), rows: [][]any{r.vals}})
			}
		}
	}
	return ops
}

// tuplePart encodes one raw value as an unambiguous byte-safe token: a
// type tag plus the value's bytes in hex (character values as their
// UTF-8 bytes, binary values raw). Hex never contains the tuple
// separator, so a component holding the separator byte cannot shift the
// tuple's structure, and a constraint's tuple can never collide with
// another constraint's (the identity is namespaced by constraint index).
func tuplePart(v any) string {
	switch x := v.(type) {
	case string:
		return "s" + hex.EncodeToString([]byte(x))
	case []byte:
		return "b" + hex.EncodeToString(x)
	case int64:
		return "i" + strconv.FormatInt(x, 10)
	case uint64:
		return "u" + strconv.FormatUint(x, 10)
	case bool:
		if x {
			return "T"
		}
		return "F"
	case time.Time:
		return "t" + hex.EncodeToString([]byte(x.UTC().Format("2006-01-02 15:04:05.999999999")))
	default:
		return "g" + hex.EncodeToString([]byte(fmt.Sprintf("%v", x)))
	}
}

// tupleOf encodes the constraint's tuple (ci indexes e.uc, namespacing
// the identity) taken from one row as the collision identity, together
// with the raw component values (the holder queries bind them — a
// re-decoded identity could round-trip a value into a different type).
// A tuple with a NULL component never occupies a unique slot (MySQL
// allows repeated NULLs in a unique index), so it does not count:
// ok is false for it.
func tupleOf(ci int, c *uniqueConstraint, row []any, idx []int) (string, []any, bool) {
	parts := make([]string, len(idx))
	vals := make([]any, len(idx))
	for i, p := range idx {
		v := row[p]
		if v == nil {
			return "", nil, false
		}
		parts[i] = tuplePart(v)
		vals[i] = v
	}
	return strconv.Itoa(ci) + "\x1f" + strings.Join(parts, "\x1f"), vals, true
}

// rawEq reports whether two raw driver values are equal (the types the
// driver yields: ints, uints, strings, bytes, bools, times). It is a
// LOCAL equality: the server's collation may fold more (a case-
// insensitive match between distinct bytes), which the caller treats as
// a holder anyway (conservative — the index itself would see them as
// equal, so the write would collide regardless).
func rawEq(a, b any) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case int64:
		if y, ok := b.(int64); ok {
			return x == y
		}
		if y, ok := b.(uint64); ok {
			return y <= math.MaxInt64 && int64(y) == x
		}
		return false
	case uint64:
		if y, ok := b.(uint64); ok {
			return x == y
		}
		if y, ok := b.(int64); ok {
			return y >= 0 && x == uint64(y)
		}
		return false
	case string:
		if y, ok := b.(string); ok {
			return x == y
		}
		if y, ok := b.([]byte); ok {
			return x == string(y)
		}
		return false
	case []byte:
		if y, ok := b.([]byte); ok {
			return string(x) == string(y)
		}
		if y, ok := b.(string); ok {
			return string(x) == y
		}
		return false
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case time.Time:
		y, ok := b.(time.Time)
		return ok && x.Equal(y)
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// writtenTuples collects, per constraint, the unique tuples the chunk's
// ops WRITE (the source rows of inserts, converted updates,
// replacements and rewritten no-op groups; deletes write nothing):
// identity -> the raw component values (the holder queries bind them).
// Collection stops once the tracked total passes maxTrackedTuples (the
// caller escalates instead), so the structure is O(min(delta, cap)) —
// bounded, never O(delta) or O(table) (P1-6).
func (e *Engine) writtenTuples(ops []op) ([]map[string][]any, bool) {
	out := make([]map[string][]any, len(e.uc))
	for i := range out {
		out[i] = make(map[string][]any)
	}
	total := 0
	for _, o := range ops {
		if o.kind == opDelete {
			continue
		}
		for ci, c := range e.uc {
			for _, row := range o.rows {
				if t, vals, ok := tupleOf(ci, &c, row, c.srcIdx); ok {
					if _, dup := out[ci][t]; !dup {
						out[ci][t] = vals
						total++
						if total > maxTrackedTuples {
							return out, true // cap exceeded: stop collecting
						}
					}
				}
			}
		}
	}
	return out, false
}

// maxTrackedTuples bounds the unique tuples one chunk may write before
// the cross-chunk holder check gives up on per-holder proof and forces
// the order-independent escalation (a full resync, or a refusal for a
// filtered table).
const maxTrackedTuples = 10000

// holderBatch bounds the tuples per targeted holder query (a batch of
// 200 tuples on a wide constraint stays far below the driver's
// placeholder limit).
const holderBatch = 200

// crossChunkVerdict is the cross-chunk holder check's result.
type crossChunkVerdict int

const (
	crossSafe      crossChunkVerdict = iota // no unaddressed row holds a written tuple
	crossConflict                           // a written tuple is (or may be) held outside the chunk
	crossDuplicate                          // the source holds one unique tuple in two rows
)

// crossChunkCheck verifies that no destination row OUTSIDE this chunk's
// buffered rows still holds a unique tuple the chunk's ops write: a swap
// that spans chunk boundaries cannot be ordered across the per-chunk
// commits, so it must escalate (or be refused). The check is O(delta),
// not O(table) (P1-6): it tracks only the tuples the ops actually write,
// resolves each foreign holder with a targeted point query, and never
// buffers the table's unique values.
//
// srcCn/dstCn are the caller's PINNED scan connections: the plan holds
// one per side for the whole table, and at parallel=1 the pool holds no
// second connection to hand the holder queries (a pool checkout against
// a fully checked-out pool blocks until the caller returns — a
// self-deadlock), so the safety queries reuse the very connections the
// plan already owns. The queries run strictly between chunk scans on
// those connections, never concurrently with them. Callers without a
// pinned connection use the pool wrappers holdersOf / srcRowByKey.
//
// oorFlag is the out-of-range predicate (key < srcMin OR key > srcMax,
// parameterized) carried as a per-row flag column on the holders query
// itself (see holdersOf): a holder the flag marks out-of-range is
// deleted by the UNFILTERED out-of-range pass, which the executor
// commits before every in-range write — server-side authority, so it is
// the only proof available for a case-insensitive or decimal key. lo/hi
// are the source's GLOBAL key extremes (nil = unbounded side): a holder
// the flag marks in-range yet the client orders strictly outside them
// (data moved) is a conflict. oorActive says the unfiltered out-of-range
// pass runs at all (keyed on both sides, keys agree, no filter). A
// holder INSIDE the global range belongs to another chunk and is
// resolved against the source (see holderInOtherChunk); on a key whose
// order the client cannot reproduce (orderKnown false) that resolution
// is impossible, so any such foreign holder is a conflict: not provable
// is not safe.
func (e *Engine) crossChunkCheck(ctx context.Context, srcCn, dstCn *sql.Conn, srcS, dstS *conn.Schema, ch chunk.Chunk, dstM map[string][]*srow, ops []op, lo, hi []driver.Value, oorFlag chunk.Pred, oorActive bool) (crossChunkVerdict, error) {
	if len(e.uc) == 0 || len(ops) == 0 {
		return crossSafe, nil
	}
	written, overflow := e.writtenTuples(ops)
	if overflow {
		// the delta outgrows the tracking cap: too many written tuples to
		// prove individually, escalate rather than guess
		return crossConflict, nil
	}
	total := 0
	for _, m := range written {
		total += len(m)
	}
	if total == 0 {
		return crossSafe, nil
	}
	// the key groups this chunk's ops ADDRESS (an addressed group's
	// destination rows are deleted before any insert runs, so a holder
	// among them is safe): updates, deletes and rewrites carry the
	// destination's raw key; inserts address groups the destination
	// does not hold and cannot be holders.
	buf := make([]byte, 0, 256)
	targeted := make(map[string]bool, len(ops))
	for _, o := range ops {
		switch o.kind {
		case opUpdate, opDelete, opRewrite:
			id, err := e.dstKey.Normalize(o.key, buf)
			if err != nil {
				return crossConflict, err
			}
			buf = id[:0]
			targeted[string(id)] = true
		}
	}
	orderKnown := keyOrderKnown(srcS)
	for ci, c := range e.uc {
		if len(written[ci]) == 0 {
			continue
		}
		hits, err := e.holdersOfConn(ctx, dstCn, dstS, ci, c, written[ci], oorFlag, oorActive)
		if err != nil {
			return crossConflict, err
		}
		for _, h := range hits {
			v, err := e.classifyHolderConn(ctx, srcCn, srcS, ch, dstM, ci, c, h, written[ci], lo, hi, targeted, oorActive, orderKnown)
			if err != nil {
				return crossConflict, err
			}
			if v != crossSafe {
				return v, nil
			}
		}
	}
	return crossSafe, nil
}

// holdersOf asks the destination (UNFILTERED — a foreign holder outside
// the --where match set still occupies its unique slot) for the keys of
// every row holding one of the given all-non-NULL tuples of the
// constraint. The query is targeted and parameterized: the tuples travel
// as bound arguments (P0-1), never rendered into SQL text. Any row the
// query returns is a holder of some written tuple — exactly, or by a
// collation fold the local comparison cannot see (a conservative
// holder either way).
//
// When the unfiltered out-of-range pass runs (oorActive), each row
// carries a computed flag column — the out-of-range predicate
// (key < srcMin OR key > srcMax, parameterized) evaluated ON THE
// SERVER: the holder classification then needs no client-side key order
// and no second query (a case-insensitive or decimal key can still be
// proven safe out-of-range, and in-range stays in-range by the
// server's own comparison). The flag is a constant 0 when the pass does
// not run (the caller conflicts such holders before consulting it).
// holdersQuery renders one batch of the holders query: the key and
// constraint columns, the optional server-side out-of-range flag column,
// and the OR of the per-tuple equality terms.
//
// The argument order is part of the contract, not an implementation
// detail: the driver binds positionally in the order the placeholders
// APPEAR in the text. The flag column's placeholders sit in the SELECT
// list — textually BEFORE the WHERE clause — so its args must LEAD the
// list; the tuple args follow in term order. Appending the flag args
// after the tuple args shifts every binding (the regression the t_mut
// e2e caught: the WHERE then addressed the extreme values, not the
// written tuples).
func holdersQuery(dstS *conn.Schema, c uniqueConstraint, colIdents []string, tuples map[string][]any, keys []string, start, end int, oorFlag chunk.Pred, flagOn bool) (string, []any) {
	keyIdents := make([]string, len(dstS.Key))
	for i, k := range dstS.Key {
		keyIdents[i] = conn.QuoteIdent(k)
	}
	terms := make([]string, 0, end-start)
	args := make([]any, 0, (end-start)*len(colIdents)+len(oorFlag.Args))
	if flagOn {
		args = append(args, oorFlag.Args...)
	}
	for _, t := range keys[start:end] {
		vs := tuples[t]
		term := make([]string, len(colIdents))
		for i := range colIdents {
			term[i] = colIdents[i] + " = ?"
			args = append(args, vs[i])
		}
		terms = append(terms, "("+strings.Join(term, " AND ")+")")
	}
	sel := make([]string, 0, len(keyIdents)+len(colIdents)+1)
	sel = append(sel, keyIdents...)
	sel = append(sel, colIdents...)
	if flagOn {
		sel = append(sel, "("+oorFlag.SQL+")")
	} else {
		sel = append(sel, "0")
	}
	return fmt.Sprintf("SELECT %s FROM %s WHERE %s",
		strings.Join(sel, ", "), conn.QuoteIdent(dstS.Table), strings.Join(terms, " OR ")), args
}

// holdersOf is the pool variant: it takes a scan connection from the
// side's pool (blocking while every pooled connection is checked out —
// a caller that already PINS the pool's only connection must use
// holdersOfConn with that connection instead, see crossChunkCheck).
func (e *Engine) holdersOf(ctx context.Context, dst *conn.Side, dstS *conn.Schema, ci int, c uniqueConstraint, tuples map[string][]any, oorFlag chunk.Pred, oorActive bool) ([]holderRow, error) {
	cn, err := dst.AcquireScan(ctx)
	if err != nil {
		return nil, err
	}
	defer cn.Close()
	return e.holdersOfConn(ctx, cn, dstS, ci, c, tuples, oorFlag, oorActive)
}

// holdersOfConn is the query over a caller-owned scan connection (the
// plan's pinned connection — see crossChunkCheck).
func (e *Engine) holdersOfConn(ctx context.Context, cn *sql.Conn, dstS *conn.Schema, ci int, c uniqueConstraint, tuples map[string][]any, oorFlag chunk.Pred, oorActive bool) ([]holderRow, error) {
	colIdents := make([]string, len(c.cols))
	for i, n := range c.cols {
		colIdents[i] = conn.QuoteIdent(n)
	}
	keys := make([]string, 0, len(tuples))
	for t := range tuples {
		keys = append(keys, t)
	}
	sort.Strings(keys)
	var out []holderRow
	flagOn := oorActive && oorFlag.SQL != ""
	for start := 0; start < len(keys); start += holderBatch {
		end := start + holderBatch
		if end > len(keys) {
			end = len(keys)
		}
		query, args := holdersQuery(dstS, c, colIdents, tuples, keys, start, end, oorFlag, flagOn)
		selCount := len(dstS.Key) + len(c.cols) + 1
		rows, err := cn.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		vals := make([]any, selCount)
		ptrs := make([]any, selCount)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			for i := range vals {
				vals[i] = nil
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return nil, err
			}
			keyPart := make([]any, len(dstS.Key))
			copy(keyPart, vals[:len(dstS.Key)])
			h := holderRow{key: keyPart}
			if flagOn {
				switch f := vals[selCount-1].(type) {
				case int64:
					h.oor = f != 0
				case bool:
					h.oor = f
				}
			}
			out = append(out, h)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// holderRow is one foreign holder as the holders query returns it: the
// raw key plus the server-side out-of-range flag (see holdersOf).
type holderRow struct {
	key []any
	oor bool // the row's key is strictly outside the source's key range
}

// classifyHolderConn resolves one foreign holder (a destination key row
// the holder query matched, outside this chunk's addressed groups)
// against the chunk, the out-of-range pass and the source (over the
// caller's pinned scan connection, see crossChunkCheck). The holder's oor
// flag is the out-of-range predicate evaluated ON THE SERVER: a marked
// holder is deleted by the unfiltered out-of-range pass, which the
// executor commits before every in-range write — safe regardless of the
// key's collation (the one proof a case-insensitive or decimal key can
// get). Everything past it (the chunk positioning) requires the client
// to be able to reproduce the server's key order (orderKnown).
func (e *Engine) classifyHolderConn(ctx context.Context, srcCn *sql.Conn, srcS *conn.Schema, ch chunk.Chunk, dstM map[string][]*srow, ci int, c uniqueConstraint, h holderRow, tuples map[string][]any, lo, hi []driver.Value, targeted map[string]bool, oorActive, orderKnown bool) (crossChunkVerdict, error) {
	buf := make([]byte, 0, 256)
	id, err := e.dstKey.Normalize(h.key, buf)
	if err != nil {
		return crossConflict, err
	}
	idS := string(id)
	if targeted[idS] {
		// the holder's group is addressed by this chunk's ops: its rows
		// are deleted before any insert runs
		return crossSafe, nil
	}
	if _, inChunk := dstM[idS]; inChunk {
		// an unaddressed in-chunk group still holds a written tuple: the
		// intra-chunk check should have flagged the chunk hot — the
		// buffered rows and the live table disagree (data moved), which
		// is not provably safe
		return crossConflict, nil
	}
	if !oorActive {
		// no unfiltered out-of-range pass will run (the keys disagree,
		// or a filter is on): a foreign holder cannot be proven removed
		return crossConflict, nil
	}
	if h.oor {
		// the out-of-range pass (committed before every in-range write)
		// deletes this exact row: safe regardless of the key's collation
		return crossSafe, nil
	}
	if !orderKnown {
		// inside the global range on a key whose order the client cannot
		// reproduce (a case/accent-insensitive character collation, a
		// decimal or float key, ...): the chunk that owns the holder —
		// and whether its write lands before or after this one — is not
		// provable. Not provable is not safe.
		return crossConflict, nil
	}
	if lo != nil {
		if cmp, ok := keyCmp(h.key, toAny(lo), -1); ok && cmp < 0 {
			// the client orders it strictly below the source's minimum
			// yet the server marks it in-range: the row moved (or the
			// two disagree) — not provably deleted, a conflict
			return crossConflict, nil
		}
	}
	if hi != nil {
		if cmp, ok := keyCmp(h.key, toAny(hi), -1); ok && cmp > 0 {
			// ditto, strictly above the source's maximum
			return crossConflict, nil
		}
	}
	// inside the global range: another chunk
	return e.holderInOtherChunk(ctx, srcCn, srcS, ch, ci, c, h.key, tuples)
}

// holderInOtherChunk resolves a holder inside the source's global key
// range but outside this chunk: a targeted, parameterized point query on
// the source (unfiltered — the row matters whether or not the filter
// matches it) decides whether another chunk's write removes the tuple:
//
//   - no source row: the other chunk deletes it — safe only if that
//     chunk is already applied (the holder sorts BEFORE this chunk's
//     range); a later delete cannot free the slot in time.
//   - a source row holding one of the written tuples: the source holds
//     the unique tuple twice — a data error, reported as such.
//   - a source row holding a different tuple (or a NULL one): that
//     chunk's write removes the value, so the slot is safe.
//
// A position that cannot be established (mixed or unknown key types, a
// lead-prefix bound) is a conflict: not provable is not safe.
func (e *Engine) holderInOtherChunk(ctx context.Context, srcCn *sql.Conn, srcS *conn.Schema, ch chunk.Chunk, ci int, c uniqueConstraint, h []any, tuples map[string][]any) (crossChunkVerdict, error) {
	row, found, err := e.srcRowByKeyConn(ctx, srcCn, srcS, h, c)
	if err != nil {
		return crossConflict, err
	}
	if !found {
		switch e.holderPosition(h, ch) {
		case holderBefore:
			// an earlier chunk already deleted the row
			return crossSafe, nil
		default:
			return crossConflict, nil
		}
	}
	idx := make([]int, len(c.cols))
	for i := range idx {
		idx[i] = len(srcS.Key) + i
	}
	t, _, ok := tupleOf(ci, &c, row, idx)
	if ok {
		for w := range tuples {
			if w == t {
				// the source holds the same unique tuple in two rows: it
				// violates the constraint, no sync can converge it
				return crossDuplicate, nil
			}
		}
	}
	// A different (or a NULL) source value: the other chunk's update
	// frees the slot — but ONLY if that chunk applies BEFORE this one
	// (chunks apply in key order, sequentially). A holder that sorts
	// AFTER this chunk still holds the written value when this chunk's
	// statement runs: the unique index rejects it. Not provable is not
	// safe.
	if e.holderPosition(h, ch) != holderBefore {
		return crossConflict, nil
	}
	return crossSafe, nil
}

// holderPos is a foreign holder's position relative to a chunk's bounds.
type holderPos int

const (
	holderBefore holderPos = iota
	holderAfter
	holderInside
	holderUnknown
)

// holderPosition orders a foreign holder's key against the chunk's
// bounds (lead-prefix bounds compared on the lead columns only; an
// exhausted prefix is "unknown" — the trailing columns are unbounded).
func (e *Engine) holderPosition(h []any, ch chunk.Chunk) holderPos {
	prefix := 0
	if ch.LoPrefix > 0 && ch.LoPrefix < len(h) {
		prefix = ch.LoPrefix
	}
	if ch.HiPrefix > 0 && ch.HiPrefix < len(h) && (prefix == 0 || ch.HiPrefix < prefix) {
		prefix = ch.HiPrefix
	}
	if ch.Lo != nil {
		cmp, ok := keyCmp(h, toAny(ch.Lo), prefix)
		if !ok {
			return holderUnknown
		}
		switch {
		case cmp < 0:
			return holderBefore
		case cmp == 0 && !ch.LoIncl:
			// an exclusive bound: the bound row itself belongs to the
			// PREVIOUS chunk (whose Hi includes it), so the holder sorts
			// before this chunk — an earlier chunk's writes already ran
			return holderBefore
		case cmp == 0 && prefix > 0 && prefix < len(h):
			return holderUnknown // inclusive, equal on the lead, suffix unbounded
		}
	}
	if ch.Hi != nil {
		cmp, ok := keyCmp(h, toAny(ch.Hi), prefix)
		if !ok {
			return holderUnknown
		}
		switch {
		case cmp > 0:
			return holderAfter
		case cmp == 0 && prefix > 0 && prefix < len(h):
			return holderUnknown
		}
	}
	return holderInside
}

// srcRowByKey point-queries the source (unfiltered, parameterized key —
// NULL components as IS NULL terms) for one raw key: the key columns
// followed by the constraint's columns, so the caller can compare the
// source row's tuple. LIMIT 2: a key is unique on the source only when
// it is a unique sequence; two rows at one key is itself the "not a
// unique address" signal the caller needs, and the query must not scan
// the table. found is false when the source has no row at the key.
// srcRowByKey is the pool variant (see holdersOf): a caller that already
// pins the side's only scan connection uses srcRowByKeyConn instead.
func (e *Engine) srcRowByKey(ctx context.Context, src *conn.Side, srcS *conn.Schema, h []any, c uniqueConstraint) (row []any, found bool, err error) {
	cn, err := src.AcquireScan(ctx)
	if err != nil {
		return nil, false, err
	}
	defer cn.Close()
	return e.srcRowByKeyConn(ctx, cn, srcS, h, c)
}

// srcRowByKeyConn is the point query over a caller-owned scan
// connection (the plan's pinned connection — see crossChunkCheck).
func (e *Engine) srcRowByKeyConn(ctx context.Context, cn *sql.Conn, srcS *conn.Schema, h []any, c uniqueConstraint) (row []any, found bool, err error) {
	sel := make([]string, 0, len(srcS.Key)+len(c.cols))
	for _, k := range srcS.Key {
		sel = append(sel, conn.QuoteIdent(k))
	}
	for _, n := range c.cols {
		sel = append(sel, conn.QuoteIdent(n))
	}
	var conds []string
	args := make([]any, 0, len(h))
	for i, k := range srcS.Key {
		if h[i] == nil {
			conds = append(conds, conn.QuoteIdent(k)+" IS NULL")
		} else {
			conds = append(conds, conn.QuoteIdent(k)+" = ?")
			args = append(args, h[i])
		}
	}
	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s LIMIT 2",
		strings.Join(sel, ", "), conn.QuoteIdent(srcS.Table), strings.Join(conds, " AND "))
	rows, err := cn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	vals := make([]any, len(sel))
	ptrs := make([]any, len(sel))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	n := 0
	for rows.Next() {
		if n == 1 {
			break
		}
		for i := range vals {
			vals[i] = nil
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, false, err
		}
		cp := make([]any, len(vals))
		copy(cp, vals)
		row = cp
		found = true
		n++
	}
	return row, found, rows.Err()
}

// toAny converts a driver.Value vector to raw values (the element types
// are distinct named types, so the slice types are not assignable).
func toAny(v []driver.Value) []any {
	out := make([]any, len(v))
	for i, x := range v {
		out[i] = x
	}
	return out
}

// keyOrderKnown reports whether the client can reproduce the server's
// order of the table's key values — the one property the cross-chunk
// holder check needs to place a foreign holder (before / after / inside a
// chunk). It is a whitelist, because the driver yields most column types
// as TEXT and valueCmp falls back to byte order:
//
//   - INT/UINT/YEAR: Go integers, compared numerically — exact.
//   - DATE/DATETIME/TIMESTAMP: time.Time (parseTime is mandatory in the
//     DSN), compared chronologically — exact.
//   - BINARY/VARBINARY/BIT: raw bytes; the server orders them by byte
//     value — exact.
//   - character keys: only a binary collation (_bin, or a BINARY
//     character set) orders by bytes. A case- or accent-insensitive
//     collation orders by weight (not bytes): not reproducible.
//   - everything else is NOT reproducible: DECIMAL, FLOAT and DOUBLE
//     arrive as digit strings whose width varies with magnitude ("9"
//     sorts BEFORE "10" by bytes, reversed numerically), TIME as
//     "H:MM:SS" (variable hour width), ENUM and SET by LABEL although
//     the server orders them by index, and JSON by its own comparison
//     rules. A holder the client mis-places can be proven "safe" when it
//     frees its slot too late — a duplicate-key failure mid-apply, or
//     worse, a wrong verdict. Not provable is not safe.
func keyOrderKnown(srcS *conn.Schema) bool {
	if srcS == nil || len(srcS.Key) == 0 {
		return false
	}
	pos := make(map[string]int, len(srcS.Cols))
	for i, c := range srcS.Cols {
		pos[strings.ToLower(c.Name)] = i
	}
	for _, kn := range srcS.Key {
		i, ok := pos[strings.ToLower(kn)]
		if !ok {
			return false
		}
		c := srcS.Cols[i]
		switch c.Family {
		case conn.FamINT, conn.FamUINT, conn.FamYEAR,
			conn.FamDATE, conn.FamDATETIME, conn.FamTIMESTAMP,
			conn.FamBYTES, conn.FamBIT:
			// exact: see the comment above
		case conn.FamSTR:
			if !strings.HasSuffix(strings.ToLower(c.Collation), "_bin") &&
				!strings.Contains(strings.ToUpper(c.RawType), "BINARY") {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// keyCmp compares two raw key vectors component-wise in MySQL key order
// (NULLs first), at most n components (n <= 0: all). ok is false when a
// component pair cannot be ordered (mixed or unknown dynamic types) —
// the caller must treat that as "position unknown", never as "equal".
func keyCmp(a, b []any, n int) (int, bool) {
	if n <= 0 || n > len(a) || n > len(b) {
		n = len(a)
	}
	for i := 0; i < n; i++ {
		c, ok := valueCmp(a[i], b[i])
		if !ok {
			return 0, false
		}
		if c != 0 {
			return c, true
		}
	}
	return 0, true
}

// valueCmp orders two raw values in MySQL key order (NULLs first). Only
// the type pairs the driver actually yields for key columns are ordered
// (int64/uint64 numerics, character and binary strings, times);
// everything else is incomparable (ok=false).
func valueCmp(a, b any) (int, bool) {
	switch {
	case a == nil && b == nil:
		return 0, true
	case a == nil:
		return -1, true
	case b == nil:
		return 1, true
	}
	av, aInt := a.(int64)
	bv, bInt := b.(int64)
	au, aUint := a.(uint64)
	bu, bUint := b.(uint64)
	switch {
	case aInt && bInt:
		switch {
		case av < bv:
			return -1, true
		case av > bv:
			return 1, true
		default:
			return 0, true
		}
	case aUint && bUint:
		switch {
		case au < bu:
			return -1, true
		case au > bu:
			return 1, true
		default:
			return 0, true
		}
	}
	if (aInt || aUint) && (bInt || bUint) {
		// mixed signed/unsigned: negatives sort first, then magnitude
		aNeg := aInt && av < 0
		bNeg := bInt && bv < 0
		switch {
		case aNeg && bNeg:
			c, _ := valueCmp(av, bv)
			return c, true
		case aNeg:
			return -1, true
		case bNeg:
			return 1, true
		}
		var aU, bU uint64
		if aInt {
			aU = uint64(av)
		} else {
			aU = au
		}
		if bInt {
			bU = uint64(bv)
		} else {
			bU = bu
		}
		switch {
		case aU < bU:
			return -1, true
		case aU > bU:
			return 1, true
		default:
			return 0, true
		}
	}
	as, aStr := a.(string)
	bs, bStr := b.(string)
	ab, aBytes := a.([]byte)
	bb, bBytes := b.([]byte)
	if (aStr || aBytes) && (bStr || bBytes) {
		var ax, bx []byte
		if aStr {
			ax = []byte(as)
		} else {
			ax = ab
		}
		if bStr {
			bx = []byte(bs)
		} else {
			bx = bb
		}
		return bytes.Compare(ax, bx), true
	}
	at, aTime := a.(time.Time)
	bt, bTime := b.(time.Time)
	if aTime && bTime {
		switch {
		case at.Before(bt):
			return -1, true
		case at.After(bt):
			return 1, true
		default:
			return 0, true
		}
	}
	return 0, false
}

// diffUniqueProtected is Diff for a table with unique constraints: the
// classic per-group ops, except that when a written TUPLE is currently
// held by another row of the chunk (a swap, a cycle, or a move past a
// no-op holder) the whole chunk is re-emitted as a DELETE phase followed
// by an INSERT phase, with no-op holders of written tuples rewritten so
// their slots are freed as well. The boolean result reports that the
// emission is a destructive row rewrite (P0-2): the caller refuses the
// table unless --allow-row-rewrite is set (FK ON DELETE CASCADE,
// triggers and audit logs all fire for rewritten rows).
func (e *Engine) diffUniqueProtected(keys []string, srcM, dstM map[string][]*srow) ([]op, bool) {
	type grp struct {
		s, d []*srow
		kind int // 0 insert, 1 delete, 2 noop, 3 update, 4 replace
	}
	groups := make([]grp, 0, len(keys))
	for _, k := range keys {
		s, d := srcM[k], dstM[k]
		g := grp{s: s, d: d}
		switch {
		case len(d) == 0:
			g.kind = 0
		case len(s) == 0:
			g.kind = 1
		case e.sameMultiset(s, d):
			g.kind = 2
		case e.unique && len(s) == 1 && len(d) == 1:
			g.kind = 3
		default:
			g.kind = 4
		}
		groups = append(groups, g)
	}
	// holders maps a destination unique TUPLE to the group indices
	// holding it (an all-non-NULL tuple occurs in at most one
	// destination row per true constraint). ALL groups are registered
	// before any written tuple is checked: a writer may sit before its
	// holder in key order.
	holders := make(map[string][]int, len(e.uc)*4)
	for ci, c := range e.uc {
		for i, g := range groups {
			for _, r := range g.d {
				if t, _, ok := tupleOf(ci, &c, r.vals, c.dstIdx); ok {
					holders[t] = append(holders[t], i)
				}
			}
		}
	}
	hot := false
	for i, g := range groups {
		if g.kind == 1 || g.kind == 2 {
			continue
		}
		for ci, c := range e.uc {
			for _, r := range g.s {
				t, _, ok := tupleOf(ci, &c, r.vals, c.srcIdx)
				if !ok {
					continue
				}
				for _, h := range holders[t] {
					if h != i {
						hot = true
						break
					}
				}
			}
			if hot {
				break
			}
		}
		if hot {
			break
		}
	}
	if !hot {
		// no tuple written by a row is held by another row of this chunk:
		// the classic emission is safe in any order
		return e.diffPlain(keys, srcM, dstM), false
	}
	// hot: two-phase emission. Every destination row that can still hold
	// a tuple some row will write is deleted before any insert runs, so
	// no insert or converted update can hit a duplicate key.
	//
	// tuples written by the ops below (inserts, replaces, converted
	// updates):
	written := make([]map[string]bool, len(e.uc))
	for ci := range written {
		written[ci] = make(map[string]bool)
	}
	for _, g := range groups {
		if g.kind == 0 || g.kind == 3 || g.kind == 4 {
			for ci, c := range e.uc {
				for _, r := range g.s {
					if t, _, ok := tupleOf(ci, &c, r.vals, c.srcIdx); ok {
						written[ci][t] = true
					}
				}
			}
		}
	}
	// a no-op group holding a written tuple must be rewritten (its row
	// never moves on its own, so without the rewrite it still holds the
	// tuple when the writer's insert runs). rewriteCols records, per
	// group, the column sets of EVERY constraint that forces the rewrite
	// (constraint index order) — the input to the group's identity
	// fingerprint (P0: the confirmed scope authorizes the GROUP, not a
	// count).
	rewriteCols := make(map[int][][]string, len(groups))
	for i, g := range groups {
		if g.kind != 2 {
			continue
		}
		seen := make(map[string]bool)
		for ci, c := range e.uc {
			hit := false
			for _, r := range g.d {
				if t, _, ok := tupleOf(ci, &c, r.vals, c.dstIdx); ok && written[ci][t] {
					hit = true
					break
				}
			}
			if hit {
				id := strings.Join(c.cols, "\x00")
				if !seen[id] {
					seen[id] = true
					rewriteCols[i] = append(rewriteCols[i], c.cols)
				}
			}
		}
	}
	del := func(r *srow) op { return op{kind: opDelete, key: e.keyVals(r.vals)} }
	ins := func(r *srow) op { return op{kind: opInsert, key: e.keyVals(r.vals), rows: [][]any{r.vals}} }
	ops := make([]op, 0, len(groups))
	// phase 1: every delete (plain deletes, replaced groups, converted
	// updates, rewritten no-op groups). A rewrite carries the group's
	// source rows: the applier deletes the destination group by key and
	// re-inserts exactly those rows (a no-op group's source rows are its
	// destination rows, so the rewrite frees the unique slot and changes
	// nothing else).
	for i, g := range groups {
		switch {
		case g.kind == 1:
			for _, r := range g.d {
				ops = append(ops, del(r))
			}
		case g.kind == 3 || g.kind == 4:
			for _, r := range g.d {
				ops = append(ops, del(r))
			}
		case g.kind == 2 && len(rewriteCols[i]) > 0:
			// delete EVERY destination row of the group (not just the
			// first): the group's rows can carry distinct raw keys that
			// only fold together under the normalizer, so a single-key
			// delete would leave the rest behind.
			dk := make([][]any, 0, len(g.d))
			for _, r := range g.d {
				dk = append(dk, e.keyVals(r.vals))
			}
			ops = append(ops, op{kind: opRewrite, key: e.keyVals(g.d[0].vals), delKeys: dk, rows: rowsOf(g.s), rewriteFP: rewriteFingerprint(rewriteCols[i], dk)})
		}
	}
	// phase 2: every insert (new rows, replaced groups, converted
	// updates; rewritten groups were already re-inserted in phase 1)
	for _, g := range groups {
		switch {
		case g.kind == 0:
			for _, r := range g.s {
				ops = append(ops, ins(r))
			}
		case g.kind == 3 || g.kind == 4:
			for _, r := range g.s {
				ops = append(ops, ins(r))
			}
		}
	}
	return ops, true
}

// rowsOf renders a row slice as an op payload (one row per entry).
func rowsOf(rows []*srow) [][]any {
	out := make([][]any, len(rows))
	for i, r := range rows {
		out[i] = r.vals
	}
	return out
}

// sameMultiset reports whether both sides hold the same canonical rows with
// the same multiplicity.
func (e *Engine) sameMultiset(s, d []*srow) bool {
	if len(s) != len(d) {
		return false
	}
	sc := make([]string, len(s))
	dc := make([]string, len(d))
	for i, r := range s {
		sc[i] = r.canon
	}
	for i, r := range d {
		dc[i] = r.canon
	}
	sort.Strings(sc)
	sort.Strings(dc)
	for i := range sc {
		if sc[i] != dc[i] {
			return false
		}
	}
	return true
}

// Counts summarizes the ops (grouped by chunk) for the report. A rewrite
// counts as one delete per destination row it removes plus one insert per
// re-inserted row.
func Counts(chunked [][]op) (inserts, updates, deletes int) {
	for _, ops := range chunked {
		for _, o := range ops {
			switch o.kind {
			case opInsert:
				inserts++
			case opUpdate:
				updates++
			case opDelete:
				deletes++
			case opRewrite:
				deletes += len(o.delKeys)
				inserts += len(o.rows)
			}
		}
	}
	return
}

// rewriteCount counts the destructive row rewrites (P0-2) the ops carry:
// the dry run lists them separately in the confirmation summary.
func rewriteCount(chunked [][]op) int {
	n := 0
	for _, ops := range chunked {
		for _, o := range ops {
			if o.kind == opRewrite {
				n++
			}
		}
	}
	return n
}

// rewriteFingerprints collects the STABLE IDENTITY of the rewrite groups
// the ops carry (P0): the confirmed scope keeps this set, and the
// apply-time re-plan may run a subset of it and nothing else.
func rewriteFingerprints(chunked [][]op) []string {
	var fps []string
	for _, ops := range chunked {
		for _, o := range ops {
			if o.kind == opRewrite && o.rewriteFP != "" {
				fps = append(fps, o.rewriteFP)
			}
		}
	}
	return fps
}

// groupOps splits ops into groups of at most batch (batch < 1: one group):
// the applier commits one transaction per group, and the out-of-range
// deletes are not chunk-bounded, so the groups keep those transactions
// small.
func groupOps(ops []op, batch int) [][]op {
	if len(ops) == 0 {
		return nil
	}
	if batch < 1 {
		return [][]op{ops}
	}
	out := make([][]op, 0, (len(ops)+batch-1)/batch)
	for start := 0; start < len(ops); start += batch {
		end := start + batch
		if end > len(ops) {
			end = len(ops)
		}
		out = append(out, ops[start:end])
	}
	return out
}
