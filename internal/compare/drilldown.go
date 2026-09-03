package compare

import (
	"context"
	"database/sql/driver"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"mtdiff/internal/chunk"
	"mtdiff/internal/conn"
	"mtdiff/internal/normalize"
)

// RowKind classifies a row-level difference.
type RowKind string

const (
	RowChanged      RowKind = "CHANGED"
	RowMissingInDst RowKind = "MISSING_IN_DST"
	RowMissingInSrc RowKind = "MISSING_IN_SRC"
	// RowCountDiff marks a keyless row present on both sides with differing
	// multiplicity (neither side is "missing" the row itself).
	RowCountDiff RowKind = "COUNT_DIFF"
)

// RowDiff is one example row-level difference.
type RowDiff struct {
	Kind    RowKind
	Keys    string
	SrcVals string
	DstVals string
}

// DrillDown produces example rows for differing chunks. For keyed tables it
// is a key-set difference (CHANGED / MISSING_IN_*); for keyless tables it
// is a multiset difference (COUNT_DIFF, or MISSING_IN_* when a row exists
// on only one side).
type DrillDown struct{}

type rowRec struct {
	keys  string // rendered key values (keyed tables)
	vals  string // rendered compared values
	canon string // normalized compared-column bytes
	n     int    // multiplicity (keyless tables)
}

// drillMaxRows caps the rows buffered per side in drill-down. Keyed chunks
// are small by design (one chunk-size each); the cap guards keyless
// whole-table scans, where it bounds the memory of a huge keyless table.
const drillMaxRows = 100_000

// Diff compares the rows of one chunk on both sides and returns up to
// limit example differences. The boolean result reports whether either side
// was truncated to drillMaxRows: the row-level results are then a sample of
// that many rows per side, not the exact multiset difference.
func (d *DrillDown) Diff(ctx context.Context, src, dst *conn.Side, srcSchema, dstSchema *conn.Schema, srcNorm, dstNorm *normalize.Normalizer, ch chunk.Chunk, where string, limit int) ([]RowDiff, bool, error) {
	// Keyed matching only makes sense when both sides agree on a key.
	// With a keyed source and a keyless destination (structure drift,
	// --no-sync-schema) the row maps have different shapes, so buffer
	// both sides as multisets: the key columns stay in Cols on both
	// sides, so the canonical rows are still comparable.
	bothKeyed := len(srcSchema.Key) > 0 && len(dstSchema.Key) > 0
	srcRows, srcTrunc, err := d.scanRows(ctx, src, srcSchema, srcNorm, ch, where, bothKeyed)
	if err != nil {
		return nil, false, fmt.Errorf("src: %w", err)
	}
	dstRows, dstTrunc, err := d.scanRows(ctx, dst, dstSchema, dstNorm, ch, where, bothKeyed)
	if err != nil {
		return nil, false, fmt.Errorf("dst: %w", err)
	}
	if bothKeyed {
		return d.keyedDiff(srcRows, dstRows, limit), srcTrunc || dstTrunc, nil
	}
	return d.multisetDiff(srcRows, dstRows, limit), srcTrunc || dstTrunc, nil
}

// scanRows streams a chunk and buffers up to drillMaxRows rows per side.
// Keyed tables: one entry per key. Keyless tables: one entry per distinct
// canonical row plus its multiplicity. Past the cap it keeps draining the
// result set (closing it early would kill the dedicated pool connection,
// which is pre-conditioned and not cheaply replaceable) but stops
// buffering, and reports truncated=true.
func (d *DrillDown) scanRows(ctx context.Context, side *conn.Side, schema *conn.Schema, norm *normalize.Normalizer, ch chunk.Chunk, where string, keyed bool) (map[string]*rowRec, bool, error) {
	cn, err := side.AcquireScan(ctx)
	if err != nil {
		return nil, false, err
	}
	defer cn.Close()

	cols := make([]string, 0, len(schema.Key)+len(schema.Cols))
	for _, k := range schema.Key {
		cols = append(cols, conn.QuoteIdent(k))
	}
	for _, c := range schema.Cols {
		cols = append(cols, conn.QuoteIdent(c.Name))
	}
	pred := ch.Predicate(schema.Key, where)
	var whereClause string
	if pred != "" {
		whereClause = " WHERE " + pred
	}
	query := fmt.Sprintf("SELECT %s FROM %s%s%s",
		strings.Join(cols, ", "), conn.QuoteIdent(schema.Table), whereClause, drillOrderBy(schema))
	rows, err := cn.QueryContext(ctx, query)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	keyN, colN := len(schema.Key), len(schema.Cols)
	// Scan into []any (NULLs cannot be stored into *driver.Value
	// destinations); the buffer is handed to the row map as-is, no copy.
	vals := make([]any, keyN+colN)
	ptrs := make([]any, keyN+colN)
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
	out, truncated, err := d.bufferRows(keyed && keyN > 0, keyN, colN, norm, next)
	if err != nil {
		return nil, false, err
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return out, truncated, nil
}

// bufferRows consumes the row iterator (nil, false = stream ended) and
// builds the row map, buffering at most drillMaxRows rows. Each row is keyN
// key values followed by colN compared values. Past the cap it keeps
// draining the iterator without buffering and reports truncated=true.
func (d *DrillDown) bufferRows(keyed bool, keyN, colN int, norm *normalize.Normalizer, next func() ([]any, bool)) (map[string]*rowRec, bool, error) {
	out := make(map[string]*rowRec, 1024)
	buf := make([]byte, 0, 4096)
	truncated := false
	for read := 0; ; read++ {
		vals, ok := next()
		if !ok {
			break
		}
		if read >= drillMaxRows {
			truncated = true
			continue
		}
		keyVals := make([]any, keyN)
		colVals := make([]any, colN)
		for i, v := range vals {
			if i < keyN {
				keyVals[i] = v
			} else {
				colVals[i-keyN] = v
			}
		}
		canon, err := norm.Normalize(colVals, buf)
		if err != nil {
			return nil, false, err
		}
		lookup := string(canon)
		if keyed {
			lookup = lookupKey(keyVals)
		}
		rec, ok := out[lookup]
		if !ok {
			rec = &rowRec{
				canon: string(canon),
				vals:  renderValues(colVals),
				n:     1,
			}
			if keyed {
				rec.keys = renderValues(keyVals)
			}
			out[lookup] = rec
		} else {
			rec.n++
		}
	}
	return out, truncated, nil
}

// lookupKey renders a key vector as its map identity: string/byte
// components are quoted (%q) before the " | " join, so a value containing
// the separator cannot collide across component boundaries — e.g.
// ("a", "b | c") and ("a | b", "c") must stay two distinct keys. Long
// values are shortened first so the key stays bounded. Display uses
// renderValues (unquoted); this is identity only.
func lookupKey(vals []any) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		switch t := v.(type) {
		case string:
			parts[i] = strconv.Quote(shorten(t))
		case []byte:
			parts[i] = strconv.Quote(shorten(string(t)))
		default:
			parts[i] = renderValue(v)
		}
	}
	return strings.Join(parts, " | ")
}

// shorten caps a string component for display/identity purposes.
func shorten(s string) string {
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}

func (d *DrillDown) keyedDiff(srcRows, dstRows map[string]*rowRec, limit int) []RowDiff {
	if limit <= 0 {
		limit = 10
	}
	out := make([]RowDiff, 0, limit)
	add := func(r RowDiff) {
		if len(out) < limit {
			out = append(out, r)
		}
	}
	for key, s := range srcRows {
		if len(out) >= limit {
			break
		}
		t, ok := dstRows[key]
		switch {
		case !ok:
			add(RowDiff{Kind: RowMissingInDst, Keys: s.keys, SrcVals: s.vals})
		case t.canon != s.canon:
			add(RowDiff{Kind: RowChanged, Keys: s.keys, SrcVals: s.vals, DstVals: t.vals})
		}
	}
	for key, t := range dstRows {
		if len(out) >= limit {
			break
		}
		if _, ok := srcRows[key]; !ok {
			add(RowDiff{Kind: RowMissingInSrc, Keys: t.keys, DstVals: t.vals})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Keys < out[j].Keys })
	return out
}

// multisetDiff reports canonical rows whose multiplicity differs between the
// sides (keyless tables), as "row xN" on each side.
func (d *DrillDown) multisetDiff(srcRows, dstRows map[string]*rowRec, limit int) []RowDiff {
	if limit <= 0 {
		limit = 10
	}
	keys := make([]string, 0, len(srcRows)+len(dstRows))
	for k := range srcRows {
		keys = append(keys, k)
	}
	for k := range dstRows {
		if _, ok := srcRows[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]RowDiff, 0, limit)
	for _, k := range keys {
		if len(out) >= limit {
			break
		}
		s, d := srcRows[k], dstRows[k]
		if s != nil && d != nil && s.n == d.n {
			continue
		}
		// Present on only one side: missing there. Present on both with
		// different multiplicity: a count difference, not a "missing" row.
		r := RowDiff{Kind: RowCountDiff}
		switch {
		case s == nil:
			r.Kind = RowMissingInSrc
		case d == nil:
			r.Kind = RowMissingInDst
		}
		if s != nil {
			r.SrcVals = s.vals + " x" + strconv.Itoa(s.n)
		}
		if d != nil {
			r.DstVals = d.vals + " x" + strconv.Itoa(d.n)
		}
		out = append(out, r)
	}
	return out
}

// renderValues renders raw driver values for display (truncating long blobs).
func renderValues(vals []any) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = renderValue(v)
	}
	return strings.Join(parts, " | ")
}

func renderValue(v driver.Value) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(t, 10)
	case uint64:
		return strconv.FormatUint(t, 10)
	case float32:
		return strconv.FormatFloat(float64(t), 'g', -1, 32)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case string:
		return shorten(t)
	case []byte:
		s := shorten(string(t))
		if len(t) > 80 {
			return fmt.Sprintf("%q... (%d bytes)", s, len(t))
		}
		return fmt.Sprintf("%q", s)
	case time.Time:
		return t.Format("2006-01-02 15:04:05.999")
	case time.Duration:
		return t.String()
	}
	return fmt.Sprintf("%v", v)
}

// drillOrderBy mirrors the scanner's ordering so drill-down scans match digests.
func drillOrderBy(schema *conn.Schema) string {
	if len(schema.Key) == 0 {
		return ""
	}
	cols := make([]string, 0, len(schema.Cols))
	inKey := make(map[string]bool, len(schema.Key))
	for _, k := range schema.Key {
		inKey[k] = true
		cols = append(cols, conn.QuoteIdent(k))
	}
	if !schema.KeyIsUnique {
		for _, c := range schema.Cols {
			if !inKey[c.Name] {
				cols = append(cols, conn.QuoteIdent(c.Name))
			}
		}
	}
	return " ORDER BY " + strings.Join(cols, ", ")
}
