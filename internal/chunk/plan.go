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
func (p *Planner) Plan(ctx context.Context, db *sql.DB, total int64) ([]Chunk, error) {
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
	if n > 1 && p.integerSingleKey() {
		chunks, ok := p.planInt(minV, maxV, n)
		if ok {
			return chunks, nil
		}
	}
	return p.planSample(ctx, db, minV, maxV, n)
}

func (p *Planner) integerSingleKey() bool {
	return len(p.KeyCols) == 1 &&
		(p.KeyFamilies[0] == conn.FamINT || p.KeyFamilies[0] == conn.FamUINT)
}

// extremes returns the first and last key value (with WHERE applied). Both
// sides read the first/last row in key order rather than MIN/MAX: MIN and
// MAX skip NULLs, which would drop NULL key rows (which sort first in
// MySQL order) out of the plan.
func (p *Planner) extremes(ctx context.Context, db *sql.DB) ([]driver.Value, []driver.Value, error) {
	where := p.whereClause()
	lo, err := p.keyRow(ctx, db, "ASC", where)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil, fmt.Errorf("empty key range in %s", p.Table)
		}
		return nil, nil, err
	}
	hi, err := p.keyRow(ctx, db, "DESC", where)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil, fmt.Errorf("empty key range in %s", p.Table)
		}
		return nil, nil, err
	}
	return lo, hi, nil
}

func (p *Planner) keyRow(ctx context.Context, db *sql.DB, dir, where string) ([]driver.Value, error) {
	var vals []driver.Value
	err := db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT %s FROM %s%s ORDER BY %s %s LIMIT 1",
			strings.Join(p.keyIdents(), ", "), ident(p.Table), where, strings.Join(p.keyIdents(), ", "), dir)).
		Scan(p.scanDest(&vals)...)
	return vals, err
}

func (p *Planner) scanDest(dst *[]driver.Value) []any {
	*dst = make([]driver.Value, len(p.KeyCols))
	ptrs := make([]any, len(p.KeyCols))
	for i := range *dst {
		ptrs[i] = &(*dst)[i]
	}
	return ptrs
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

// planInt splits an integer key arithmetically: exact, zero extra queries.
// Returns ok=false when the values do not fit int64 (huge uint64 keys); the
// caller falls back to sampling.
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
	if !ok {
		return nil, false
	}
	return intBoundaries(lo, hi, n), true
}

// intBoundaries splits [lo, hi] into at most n contiguous, non-overlapping
// integer ranges. Pure function; the first range is inclusive at lo.
//
// The step is ceil((hi-lo+1)/n): the closed span holds hi-lo+1 values, and
// ceil((hi-lo)/n) (the tempting formula) is one short whenever (hi-lo) is
// divisible by n, leaving the maximum key value in no chunk — both sides
// then miss the same rows and an inconsistent table reports "identical".
func intBoundaries(lo, hi int64, n int64) []Chunk {
	if n < 1 {
		n = 1
	}
	if hi == lo {
		return []Chunk{{ID: 0, Lo: []driver.Value{lo}, LoIncl: true, Hi: []driver.Value{hi}}}
	}
	step := (hi - lo + n) / n
	chunks := make([]Chunk, 0, n)
	for i := int64(0); i < n; i++ {
		loV := lo + i*step
		if loV > hi {
			break // remaining ranges are empty
		}
		hiV := lo + (i+1)*step - 1
		if hiV > hi {
			hiV = hi
		}
		c := Chunk{ID: len(chunks), Hi: []driver.Value{hiV}}
		if i == 0 {
			c.Lo, c.LoIncl = []driver.Value{loV}, true
		} else {
			c.Lo, c.LoIncl = []driver.Value{loV - 1}, false
		}
		chunks = append(chunks, c)
	}
	return chunks
}

// planSample splits non-integer or composite keys by binary sampling: each
// split point is a real key value read from the data, so adjacent chunks form
// an exact partition with no overlap and no gap.
func (p *Planner) planSample(ctx context.Context, db *sql.DB, minV, maxV []driver.Value, n int64) ([]Chunk, error) {
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
func (p *Planner) sample(ctx context.Context, db *sql.DB, lo, hi []driver.Value, loIncl bool, off int64) ([]driver.Value, error) {
	c := Chunk{Lo: lo, LoIncl: loIncl, Hi: hi}
	pred := c.Predicate(p.KeyCols, "")
	var where string
	if pred != "" {
		where = " WHERE " + pred
	}
	var vals []driver.Value
	err := db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT %s FROM %s%s ORDER BY %s LIMIT 1 OFFSET %d",
			strings.Join(p.keyIdents(), ", "), ident(p.Table), where, strings.Join(p.keyIdents(), ", "), off)).
		Scan(p.scanDest(&vals)...)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return vals, nil
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
	pred := ch.Predicate(schema.Key, where)
	var whereClause string
	if pred != "" {
		whereClause = " WHERE " + pred
	}
	query := fmt.Sprintf("SELECT %s FROM %s%s%s",
		selectCols(schema.Cols), ident(schema.Table), whereClause, s.orderBy(schema))
	rows, err := c.QueryContext(ctx, query)
	if err != nil {
		return hash.ChunkDigest{}, err
	}
	defer rows.Close()

	cols := schema.Cols
	// Scan into []any, not []driver.Value: database/sql cannot store a NULL
	// (nil) into a *driver.Value destination ("unsupported Scan"). The
	// values are driver values either way; copy them across below.
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
		row := make([]driver.Value, len(vals))
		for i, v := range vals {
			row[i] = v
		}
		canon, err := s.norm.Normalize(row, buf)
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
