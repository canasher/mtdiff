package sync

import (
	"context"
	"fmt"
	"sort"
	"strings"

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
)

// op is one row-level statement to apply to the destination.
//
// key holds the raw key values in key order: for opInsert the source row's
// key, for opUpdate/opDelete the DESTINATION row's key (the WHERE clause
// must address what is actually stored there; normalized key identity can
// be coarser than raw values, e.g. with trailing-space folding). rows is
// the source row(s) to write (one for insert/update).
type op struct {
	kind opKind
	key  []any
	rows [][]any
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
}

// NewEngine builds the engine. Both schemas are the prepared (ignored
// columns removed) schemas; the key columns must be part of Cols (the
// runner rejects an --ignore-columns entry that names a key column).
func NewEngine(srcRow, dstRow, srcKey, dstKey *normalize.Normalizer, unique bool, keyCols, cols []conn.Column) *Engine {
	pos := make(map[string]int, len(cols))
	for i, c := range cols {
		pos[c.Name] = i
	}
	e := &Engine{srcRow: srcRow, dstRow: dstRow, srcKey: srcKey, dstKey: dstKey, unique: unique}
	for _, k := range keyCols {
		if i, ok := pos[k.Name]; ok {
			e.keyIdx = append(e.keyIdx, i)
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

// Diff derives the row-level operations between two buffered sides. Ops
// are deterministic: groups are processed in sorted key-identity order, and
// within a replaced group every destination row is deleted before any
// source row is inserted (deleting first keeps unique slots free).
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

// Counts summarizes the ops (grouped by chunk) for the report.
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
			}
		}
	}
	return
}
