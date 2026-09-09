// Package chunk plans key-range chunking and scans chunks as streams.
package chunk

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"math"
	"strings"

	"mtdiff/internal/conn"
	"mtdiff/internal/hash"
	"mtdiff/internal/normalize"
)

// Querier is the read seam the planner runs on: a control-pool session
// (conn.ControlQueryer, policy-applied with dead-connection recovery)
// or a dedicated *sql.Conn (snapshot mode, where the extremes must be
// read on the same connection — and transaction — as the chunk scans).
// QueryContext, not QueryRow: a dead connection must surface as a
// recoverable QUERY error (the control session swaps in a fresh,
// policy-applied connection and retries once); a Row's dead-connection
// error only surfaces at Scan time, unrecoverable.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Planner splits a table into key ranges.
type Planner struct {
	Table       string
	KeyCols     []string // empty => keyless whole-table chunk
	KeyFamilies []string
	ChunkSize   int // target rows per chunk
	Where       string
}

// Plan returns the chunks covering the table (total rows must come from
// COUNT(*) on the same side, with the same WHERE filter).
func (p *Planner) Plan(ctx context.Context, db Querier, total int64) ([]Chunk, error) {
	if total <= 0 {
		return nil, nil
	}
	if len(p.KeyCols) == 0 {
		return []Chunk{{ID: 0}}, nil
	}
	minV, maxV, err := p.extremes(ctx, db)
	if err != nil {
		return nil, err
	}
	n := (total + int64(p.ChunkSize) - 1) / int64(p.ChunkSize)
	if n < 1 {
		n = 1
	}
	if n > 1 && p.integerLeadKey() {
		chunks, ok := p.planInt(minV, maxV, n)
		if ok {
			return chunks, nil
		}
	}
	return p.planSample(ctx, db, minV, maxV, n)
}

// integerLeadKey reports whether the key's leading column is integer, which
// is enough for arithmetic chunking: a (possibly composite) key partitions
// exactly on the lead column's value, so the trailing columns need no terms.
func (p *Planner) integerLeadKey() bool {
	return len(p.KeyCols) > 0 &&
		(p.KeyFamilies[0] == conn.FamINT || p.KeyFamilies[0] == conn.FamUINT)
}

// Extremes returns the first and last key value in the table's key order
// (the planner's WHERE is applied to both). Both are nil and err is nil
// when the table has no rows. The planner must be keyed
// (len(KeyCols) > 0).
func (p *Planner) Extremes(ctx context.Context, db Querier) ([]driver.Value, []driver.Value, error) {
	return p.extremesMaybe(ctx, db)
}

// extremes returns the first and last key value (with WHERE applied),
// erroring when the table holds no rows. Both sides read the first/last
// row in key order rather than MIN/MAX: MIN and MAX skip NULLs, which
// would drop NULL key rows (which sort first in MySQL order) out of the
// plan.
func (p *Planner) extremes(ctx context.Context, db Querier) ([]driver.Value, []driver.Value, error) {
	lo, hi, err := p.extremesMaybe(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	if lo == nil || hi == nil {
		return nil, nil, fmt.Errorf("empty key range in %s", p.Table)
	}
	return lo, hi, nil
}

// extremesMaybe reads the first and last key row in key order (see
// extremes for why not MIN/MAX), returning (nil, nil, nil) instead of an
// error when the table holds no rows.
func (p *Planner) extremesMaybe(ctx context.Context, db Querier) ([]driver.Value, []driver.Value, error) {
	where := p.whereClause()
	lo, err := p.keyRow(ctx, db, "ASC", where)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	hi, err := p.keyRow(ctx, db, "DESC", where)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	return lo, hi, nil
}

// keyOrder renders the ORDER BY list for extremes: the direction is
// repeated on every key column, because a bare trailing direction binds to
// the last column only — "a, b DESC" sorts b descending but a ascending,
// so the "last" row returned is the minimum a (with its maximum b), not
// the maximum key.
func keyOrder(idents []string, dir string) string {
	parts := make([]string, len(idents))
	for i, id := range idents {
		parts[i] = id + " " + dir
	}
	return strings.Join(parts, ", ")
}

// keyRow returns the first/last row in key order as driver values.
func (p *Planner) keyRow(ctx context.Context, db Querier, dir, where string) ([]driver.Value, error) {
	idents := p.keyIdents()
	// Scan into []any, not []driver.Value: database/sql cannot store a NULL
	// into a *driver.Value ("unsupported Scan"), and a key row may
	// legitimately be all-NULL (an explicit --key on a nullable column).
	dest := make([]any, len(p.KeyCols))
	ptrs := make([]any, len(p.KeyCols))
	for i := range dest {
		ptrs[i] = &dest[i]
	}
	if err := keyRowOne(ctx, db,
		fmt.Sprintf("SELECT %s FROM %s%s ORDER BY %s LIMIT 1",
			strings.Join(idents, ", "), ident(p.Table), where, keyOrder(idents, dir)), ptrs); err != nil {
		return nil, err
	}
	return toDriverValues(dest), nil
}

// keyRowOne scans the single row a key query must return, via
// QueryContext so a dead connection is a recoverable query error (see
// Querier) rather than a Row error that only surfaces at Scan time.
// ptrs holds one pointer per selected column; a missing row is
// sql.ErrNoRows.
func keyRowOne(ctx context.Context, db Querier, query string, ptrs []any, args ...any) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	if err := rows.Scan(ptrs...); err != nil {
		return err
	}
	return rows.Err()
}

// toDriverValues converts a []any scan result to []driver.Value: the
// dynamic types are driver values either way (database/sql delivers them
// as driver values inside the any), and a nil stays a nil.
func toDriverValues(dest []any) []driver.Value {
	out := make([]driver.Value, len(dest))
	for i, v := range dest {
		out[i] = v
	}
	return out
}

func (p *Planner) keyIdents() []string {
	out := make([]string, len(p.KeyCols))
	for i, c := range p.KeyCols {
		out[i] = ident(c)
	}
	return out
}

func (p *Planner) whereClause() string {
	if p.Where == "" {
		return ""
	}
	return " WHERE (" + p.Where + ")"
}

// planInt splits a key arithmetically on its integer lead column: exact,
// zero extra queries. A composite key qualifies through its lead column:
// every row has exactly one lead value, so contiguous lead ranges form an
// exact partition and the bound applies to the lead column only (prefix
// bounds, not lexicographic). Returns ok=false when the lead values are
// NULL or do not fit int64 (huge uint64 keys), or when the span is too
// wide for int64 arithmetic (a BIGINT range wider than MaxInt64 values)
// — the caller falls back to sampling (which also handles NULL leads).
func (p *Planner) planInt(minV, maxV []driver.Value, n int64) ([]Chunk, bool) {
	var lo, hi int64
	var ok bool
	switch x := minV[0].(type) {
	case int64:
		if y, ok2 := maxV[0].(int64); ok2 {
			lo, hi, ok = x, y, true
		}
	case uint64:
		if y, ok2 := maxV[0].(uint64); ok2 && y <= uint64(math.MaxInt64) {
			lo, hi, ok = int64(x), int64(y), true
		}
	}
	if !ok || !spanSafe(lo, hi) || n > math.MaxInt32 {
		return nil, false
	}
	return intBoundaries(lo, hi, n, len(p.KeyCols) > 1), true
}

// spanSafe reports whether [lo, hi] can be split by intBoundaries: the
// splitter's arithmetic is exact for spans of at most 2^64-1 values, so a
// full MinInt64..MaxInt64 span (2^64 values) and a hi-lo that overflows
// int64 (a negative lo with the range wider than MaxInt64) go to the
// sampling fallback instead.
func spanSafe(lo, hi int64) bool {
	if lo == math.MinInt64 && hi == math.MaxInt64 {
		return false
	}
	return !(lo < 0 && hi > math.MaxInt64+lo)
}

// intBoundaries splits [lo, hi] into at most n contiguous, non-overlapping
// integer ranges. Pure function; the first range is inclusive at lo.
//
// The step is ceil((hi-lo+1)/n): the closed span holds hi-lo+1 values, and
// ceil((hi-lo)/n) (the tempting formula) is one short whenever (hi-lo) is
// divisible by n, leaving the maximum key value in no chunk — both sides
// then miss the same rows and an inconsistent table reports "identical".
//
// Overflow-safe (P1): the span can hold up to 2^63 values (a BIGINT key
// from 0 to MaxInt64), so the old "hi-lo+n" must not be computed in
// int64. All the math runs in uint64 (exact: spanSafe keeps the span
// below 2^64), offsets are compared against the exact hi-lo distance, and
// a boundary is emitted only after being clamped into [lo, hi], where its
// uint64 image converts back to int64 exactly.
//
// leadPrefix marks the ranges as lead-column-only bounds (composite key
// split on its integer lead): the boundary vectors hold just the lead
// value and carry LoPrefix/HiPrefix so the predicate renders plain column
// comparisons instead of lexicographic expansion.
func intBoundaries(lo, hi int64, n int64, leadPrefix bool) []Chunk {
	prefix := 0
	if leadPrefix {
		prefix = 1
	}
	if n < 1 {
		n = 1
	}
	if hi == lo {
		return []Chunk{{ID: 0, Lo: []driver.Value{lo}, LoIncl: true, Hi: []driver.Value{hi}, LoPrefix: prefix, HiPrefix: prefix}}
	}
	// A chunk count past 2^31 would let i*step wrap the uint64 ring;
	// such a table (and no sane chunk size) is better served by sampling.
	if n > math.MaxInt32 {
		return nil
	}
	uLo, uHi := uint64(lo), uint64(hi)
	// the exact hi-lo distance (the uint64 difference is the true
	// distance even across the signed boundary, spanSafe < 2^64)
	diff := uHi - uLo
	span := diff + 1
	step := (span + uint64(n) - 1) / uint64(n)
	chunks := make([]Chunk, 0, n)
	for i := int64(0); i < n; i++ {
		off := uint64(i) * step // < 2^64 (span < 2^64, n <= MaxInt32)
		if off > diff {
			break // remaining ranges are empty
		}
		endOff := off + step - 1
		if endOff > diff {
			endOff = diff
		}
		c := Chunk{ID: len(chunks), Hi: []driver.Value{int64(uLo + endOff)}, LoPrefix: prefix, HiPrefix: prefix}
		if i == 0 {
			c.Lo, c.LoIncl = []driver.Value{int64(uLo + off)}, true
		} else {
			c.Lo, c.LoIncl = []driver.Value{int64(uLo + off - 1)}, false
		}
		chunks = append(chunks, c)
	}
	return chunks
}

// planSample splits non-integer or composite keys by binary sampling: each
// split point is a real key value read from the data, so adjacent chunks form
// an exact partition with no overlap and no gap.
func (p *Planner) planSample(ctx context.Context, db Querier, minV, maxV []driver.Value, n int64) ([]Chunk, error) {
	if n < 1 {
		n = 1
	}
	var out []Chunk
	nextID := 0
	var split func(lo, hi []driver.Value, loIncl bool, rows int64) error
	split = func(lo, hi []driver.Value, loIncl bool, rows int64) error {
		if rows <= int64(p.ChunkSize) {
			out = append(out, Chunk{ID: nextID, Lo: lo, LoIncl: loIncl, Hi: hi})
			nextID++
			return nil
		}
		mid, err := p.sample(ctx, db, lo, hi, loIncl, rows/2)
		if err != nil {
			return err
		}
		if mid == nil || valuesEqual(mid, hi) {
			// Cannot refine (degenerate data, e.g. all-equal keys):
			// emit the whole interval as one chunk.
			out = append(out, Chunk{ID: nextID, Lo: lo, LoIncl: loIncl, Hi: hi})
			nextID++
			return nil
		}
		if err := split(lo, mid, loIncl, rows/2); err != nil {
			return err
		}
		return split(mid, hi, false, rows-rows/2)
	}
	if err := split(minV, maxV, true, n*int64(p.ChunkSize)); err != nil {
		return nil, err
	}
	return out, nil
}

// sample picks an interior key value at roughly the off-th row of (lo, hi].
func (p *Planner) sample(ctx context.Context, db Querier, lo, hi []driver.Value, loIncl bool, off int64) ([]driver.Value, error) {
	c := Chunk{Lo: lo, LoIncl: loIncl, Hi: hi}
	// The split point must come from the SAME row set the plan covers
	// (P2-2): the --where filter applies here exactly as it does to the
	// extremes and the chunk scans. Without it, a sparse filter would pull
	// split points from the unfiltered data and skew every chunk.
	pred := c.Pred(p.KeyCols, p.Where)
	var where string
	if pred.SQL != "" {
		where = " WHERE " + pred.SQL
	}
	// []any scan destinations: see keyRow for why not []driver.Value.
	dest := make([]any, len(p.KeyCols))
	ptrs := make([]any, len(p.KeyCols))
	for i := range dest {
		ptrs[i] = &dest[i]
	}
	err := keyRowOne(ctx, db,
		fmt.Sprintf("SELECT %s FROM %s%s ORDER BY %s LIMIT 1 OFFSET %d",
			strings.Join(p.keyIdents(), ", "), ident(p.Table), where, strings.Join(p.keyIdents(), ", "), off), ptrs,
		pred.Args...)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return toDriverValues(dest), nil
}

// Scanner streams one chunk row by row and folds rows into a chunk digest.
type Scanner struct {
	norm    *normalize.Normalizer
	ordered bool
}

// NewScanner builds a scanner. ordered selects the row-order-dependent path
// (keyed tables); keyless tables use the order-independent statistics.
func NewScanner(norm *normalize.Normalizer, ordered bool) *Scanner {
	return &Scanner{norm: norm, ordered: ordered}
}

// Scan streams the chunk and returns its digest. The connection must be
// dedicated to this scan (session policy stays in effect).
func (s *Scanner) Scan(ctx context.Context, c *sql.Conn, schema *conn.Schema, ch Chunk, where string) (hash.ChunkDigest, error) {
	// parameterized: the key bounds are bound on the server side (P0-1)
	pred := ch.Pred(schema.Key, where)
	var whereClause string
	if pred.SQL != "" {
		whereClause = " WHERE " + pred.SQL
	}
	query := fmt.Sprintf("SELECT %s FROM %s%s%s",
		selectCols(schema.Cols), ident(schema.Table), whereClause, s.orderBy(schema))
	rows, err := c.QueryContext(ctx, query, pred.Args...)
	if err != nil {
		return hash.ChunkDigest{}, err
	}
	defer rows.Close()

	cols := schema.Cols
	// Scan into []any, not []driver.Value: database/sql cannot store a NULL
	// (nil) into a *driver.Value destination ("unsupported Scan"). The
	// values are driver values either way, so the buffer goes straight to
	// Normalize (no per-row copy).
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	acc := hash.NewAccumulator(ch.ID, s.ordered)
	buf := make([]byte, 0, 8192)
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return hash.ChunkDigest{}, fmt.Errorf("scan %s chunk %d: %w", schema.Table, ch.ID, err)
		}
		canon, err := s.norm.Normalize(vals, buf)
		if err != nil {
			return hash.ChunkDigest{}, fmt.Errorf("normalize %s chunk %d: %w", schema.Table, ch.ID, err)
		}
		buf = canon[:0]
		acc.AddRow(canon)
	}
	if err := rows.Err(); err != nil {
		return hash.ChunkDigest{}, fmt.Errorf("scan %s chunk %d: %w", schema.Table, ch.ID, err)
	}
	return acc.Digest(), nil
}

// orderBy orders by the key; if the key is not proven unique, all remaining
// columns are appended so the ordering is total (equal-key rows resolve by
// their values; fully identical rows hash identically anyway).
func (s *Scanner) orderBy(schema *conn.Schema) string {
	if len(schema.Key) == 0 {
		return ""
	}
	cols := make([]string, 0, len(schema.Cols))
	inKey := make(map[string]bool, len(schema.Key))
	for _, k := range schema.Key {
		inKey[k] = true
		cols = append(cols, ident(k))
	}
	if !schema.KeyIsUnique {
		for _, c := range schema.Cols {
			if !inKey[c.Name] {
				cols = append(cols, ident(c.Name))
			}
		}
	}
	return " ORDER BY " + strings.Join(cols, ", ")
}

func selectCols(cols []conn.Column) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = ident(c.Name)
	}
	return strings.Join(out, ", ")
}
