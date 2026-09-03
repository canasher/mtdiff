package sync

import (
	"context"
	"fmt"
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
}

// srow is one buffered row.
type srow struct {
	vals  []any  // raw driver values, schema.Cols order (key columns included)
	canon string // normalized bytes of the full compared column set
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
	// uniqueCols are the compared columns covered by a primary/unique
	// index on EITHER side (conservative superset; a member of a
	// composite index is treated as fully unique, which can only
	// over-convert an update into a delete+insert — always correct).
	// They drive the unique-value-swap protection in Diff (P1-4).
	uniqueCols map[string]bool
	srcColIdx  map[string]int // name -> position in the source Cols
	dstColIdx  map[string]int // name -> position in the destination Cols
}

// NewEngine builds the engine. Both schemas are the prepared (ignored
// columns removed) schemas; the key columns must be part of Cols (the
// runner rejects an --ignore-columns entry that names a key column).
func NewEngine(srcRow, dstRow, srcKey, dstKey *normalize.Normalizer, unique bool, keyCols, cols, dstCols []conn.Column, uniqueCols map[string]bool) *Engine {
	pos := make(map[string]int, len(cols))
	for i, c := range cols {
		pos[c.Name] = i
	}
	e := &Engine{srcRow: srcRow, dstRow: dstRow, srcKey: srcKey, dstKey: dstKey, unique: unique, uniqueCols: uniqueCols}
	for _, k := range keyCols {
		if i, ok := pos[k.Name]; ok {
			e.keyIdx = append(e.keyIdx, i)
		}
	}
	if len(uniqueCols) > 0 {
		e.srcColIdx = make(map[string]int, len(uniqueCols))
		for i, c := range cols {
			if uniqueCols[c.Name] {
				e.srcColIdx[c.Name] = i
			}
		}
		e.dstColIdx = make(map[string]int, len(uniqueCols))
		for i, c := range dstCols {
			if uniqueCols[c.Name] {
				e.dstColIdx[c.Name] = i
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
// read-only scan pool, grouped by normalized key identity.
func (e *Engine) scanSide(ctx context.Context, side *conn.Side, schema *conn.Schema, norm, keyNorm *normalize.Normalizer, ch chunk.Chunk, where string) (map[string][]*srow, error) {
	cn, err := side.AcquireScan(ctx)
	if err != nil {
		return nil, err
	}
	defer cn.Close()
	idents := make([]string, len(schema.Cols))
	for i, c := range schema.Cols {
		idents[i] = conn.QuoteIdent(c.Name)
	}
	query := fmt.Sprintf("SELECT %s FROM %s", strings.Join(idents, ", "), conn.QuoteIdent(schema.Table))
	if pred := ch.Predicate(schema.Key, where); pred != "" {
		query += " WHERE " + pred
	}
	rows, err := cn.QueryContext(ctx, query)
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
// side's read-only scan pool, under a free-form predicate ANDed with the
// optional --where filter, and returns each row's raw key values. It
// serves the out-of-range delete scan, which needs nothing but the
// destination rows' keys. The connection is taken from the scan pool and
// closed on return.
func (e *Engine) scanKeyRows(ctx context.Context, side *conn.Side, schema *conn.Schema, pred, where string) ([][]any, error) {
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
	if pred != "" {
		conds = append(conds, "("+pred+")")
	}
	if where != "" {
		conds = append(conds, "("+where+")")
	}
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	rows, err := cn.QueryContext(ctx, query)
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
// When the table has unique non-key columns (e.uniqueCols), a per-row
// UPDATE is only safe in isolation: a unique-value swap (src (1,B)(2,A)
// vs dst (1,A)(2,B)) cannot be applied by UPDATEs in ANY order (P1-4),
// and even a one-way move can hit the duplicate key if the current holder
// of the value is deleted later in the chunk. Diff therefore detects such
// collisions within the chunk and, when present, re-emits the chunk's ops
// as two phases — every DELETE first, then every INSERT (converted
// updates and rewritten no-op rows included), which frees all unique
// slots before any value is written. A no-op row holding a value that
// another row is about to take is REWRITTEN (delete + re-insert of the
// identical row, opRewrite) so its slot is freed too; without that, the
// rewrite of the other row would still collide with it.
func (e *Engine) Diff(srcM, dstM map[string][]*srow) []op {
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
	if len(e.uniqueCols) == 0 {
		return e.diffPlain(keys, srcM, dstM)
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

// valKey encodes a raw value as its collision-detection identity. The
// encoding is conservative by design: when in doubt two values are
// different (missing a collision only risks a loud duplicate-key failure
// at apply time — never a silent miswrite), while over-detecting only
// costs an extra delete+insert conversion, which is always correct.
func valKey(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		return "\x01" + string(x)
	case string:
		return "\x02" + x
	case int:
		return "\x03" + strconv.Itoa(x)
	case int64:
		return "\x03" + strconv.FormatInt(x, 10)
	case uint64:
		return "\x03" + strconv.FormatUint(x, 10)
	case bool:
		if x {
			return "\x06true"
		}
		return "\x06false"
	case float64:
		return "\x07" + strconv.FormatFloat(x, 'g', -1, 64)
	case time.Time:
		return "\x08" + x.UTC().Format("2006-01-02 15:04:05.999999999")
	default:
		return "\x04" + fmt.Sprintf("%v", v)
	}
}

// diffUniqueProtected is Diff for tables with unique non-key columns: the
// classic per-group ops, except that when a written value is currently
// held by another row of the chunk (a swap, a cycle, or a move past a
// row deleted later) the whole chunk is re-emitted as a DELETE phase
// followed by an INSERT phase, with no-op holders of written values
// rewritten so their slots are freed as well.
func (e *Engine) diffUniqueProtected(keys []string, srcM, dstM map[string][]*srow) []op {
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
	// holders maps a destination unique-column value to the key groups
	// holding it (a value occurs in at most one destination row when the
	// column is truly unique; the superset case can repeat it). ALL groups
	// are registered before any written value is checked: a writer may sit
	// before its holder in key order.
	holders := make(map[string][]string)
	for i, g := range groups {
		for _, r := range g.d {
			for _, idx := range e.dstColIdx {
				if v := valKey(r.vals[idx]); v != "" {
					holders[v] = append(holders[v], keys[i])
				}
			}
		}
	}
	hot := false
	for i, g := range groups {
		if g.kind == 1 || g.kind == 2 {
			continue
		}
		for _, r := range g.s {
			for _, idx := range e.srcColIdx {
				v := valKey(r.vals[idx])
				if v == "" {
					continue
				}
				for _, h := range holders[v] {
					if h != keys[i] {
						hot = true
						break
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
	}
	if !hot {
		// no value written by a row is held by another row of this chunk:
		// the classic emission is safe in any order
		return e.diffPlain(keys, srcM, dstM)
	}
	// hot: two-phase emission. Every destination row that can still hold a
	// value some row will write is deleted before any insert runs, so no
	// insert or converted update can hit a duplicate key.
	//
	// values written by the ops below (inserts, replaces, converted
	// updates):
	written := make(map[string]bool)
	for _, g := range groups {
		if g.kind == 0 || g.kind == 3 || g.kind == 4 {
			for _, r := range g.s {
				for _, idx := range e.srcColIdx {
					if v := valKey(r.vals[idx]); v != "" {
						written[v] = true
					}
				}
			}
		}
	}
	// a no-op group holding a written value must be rewritten (its row
	// never moves on its own, so without the rewrite it still holds the
	// value when the writer's insert runs):
	rewrite := make(map[int]bool, len(groups))
	for i, g := range groups {
		if g.kind != 2 {
			continue
		}
		for _, r := range g.d {
			for _, idx := range e.dstColIdx {
				if v := valKey(r.vals[idx]); v != "" && written[v] {
					rewrite[i] = true
					break
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
		case g.kind == 2 && rewrite[i]:
			// delete EVERY destination row of the group (not just the
			// first): the group's rows can carry distinct raw keys that
			// only fold together under the normalizer, so a single-key
			// delete would leave the rest behind.
			dk := make([][]any, 0, len(g.d))
			for _, r := range g.d {
				dk = append(dk, e.keyVals(r.vals))
			}
			ops = append(ops, op{kind: opRewrite, key: e.keyVals(g.d[0].vals), delKeys: dk, rows: rowsOf(g.s)})
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
	return ops
}

// rowsOf renders a row slice as an op payload (one row per entry).
func rowsOf(rows []*srow) [][]any {
	out := make([][]any, len(rows))
	for i, r := range rows {
		out[i] = r.vals
	}
	return out
}

// srcWriteOwner maps, for one chunk's source rows, every unique value to
// this chunk (the value's source owner lives here; a no-op row "owns" its
// value too, which only ever suppresses a false escalation). owner is
// shared across chunks so later chunks see earlier owners.
func (e *Engine) srcWriteOwner(srcM map[string][]*srow, chunkID int, owner map[string]int) {
	for _, rows := range srcM {
		for _, r := range rows {
			for _, idx := range e.srcColIdx {
				if v := valKey(r.vals[idx]); v != "" {
					owner[v] = chunkID
				}
			}
		}
	}
}

// crossChunkHeld reports whether this chunk's destination rows hold a
// unique value whose SOURCE owner lives in another chunk: a unique-value
// swap that spans chunk boundaries. The intra-chunk protection in
// diffUniqueProtected cannot order writes across the per-chunk commit
// boundaries, so the caller escalates the table to the FULL resync (the
// only order-independent convergence). No-op holders need no check: a
// value a no-op row holds is also held by the source at the same key, so
// the value's source owner IS that row's chunk (a unique value occurring
// twice in the source is unsyncable against a unique index and fails
// loudly at apply time instead).
func (e *Engine) crossChunkHeld(dstM map[string][]*srow, chunkID int, owner map[string]int) bool {
	for _, rows := range dstM {
		for _, r := range rows {
			for _, idx := range e.dstColIdx {
				v := valKey(r.vals[idx])
				if v == "" {
					continue
				}
				if o, ok := owner[v]; ok && o != chunkID {
					return true
				}
			}
		}
	}
	return false
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
