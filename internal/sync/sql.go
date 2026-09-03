package sync

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"mtdiff/internal/conn"
)

// Builder renders the sync statements for one table.
//
// Cols is the compared column set (ignored columns already removed) in
// source order, key columns included; every value rendered here is the RAW
// driver value scanned from the source (or, for DELETE, from the
// destination) — never a normalized/canonical form, which would mutate data
// (e.g. a folded-case string or a re-canonicalized decimal).
//
// Generated columns (P0-2) are compared but NEVER written: a STORED column
// rejects explicit values and a VIRTUAL one has no storage at all, so the
// write lists (writeCols / SetCols) carry only the plain columns while
// Cols (and cols) keep the full compared set, parallel to the raw rows.
type Builder struct {
	Table   string
	Cols    []string
	SetCols []string      // compared non-key columns, in Cols order
	cols    []conn.Column // parallel to Cols: full metadata (family drives value rendering)
	keyIdx  []int         // position of each key column in Cols, in key order
	setIdx  []int         // position of each SetCol in Cols, in SetCols order
	// the writable subset (plain columns only), in Cols order:
	writeIdx     []int
	writeCols    []string
	writeColMeta []conn.Column
}

// NewBuilder builds the statement builder for a table's compared schema.
func NewBuilder(table string, schema *conn.Schema) *Builder {
	b := &Builder{Table: table}
	colPos := make(map[string]int, len(schema.Cols))
	for i, c := range schema.Cols {
		b.Cols = append(b.Cols, c.Name)
		b.cols = append(b.cols, c)
		colPos[c.Name] = i
	}
	// The key columns in KEY (index) order, not column-ordinal order:
	// key values travel in index order (Engine.keyVals), so pairing them
	// to Cols-ordinal positions would swap the values of an index whose
	// order differs from the column order (e.g. PRIMARY KEY (b, a) over
	// columns (a, b)) and the WHERE would address the wrong row.
	// Generated columns are readable — a WHERE may reference them — so
	// they stay in keyIdx; only the write lists below skip them (P0-2).
	// A key made of generated columns alone therefore still yields a
	// complete WHERE (and no panic in Update/Delete).
	keySet := make(map[string]bool, len(schema.Key))
	for _, k := range schema.Key {
		keySet[k] = true
		if i, ok := colPos[k]; ok {
			b.keyIdx = append(b.keyIdx, i)
		}
	}
	for i, c := range schema.Cols {
		if c.Generated {
			continue // compared, never written (P0-2)
		}
		if !keySet[c.Name] {
			b.SetCols = append(b.SetCols, c.Name)
			b.setIdx = append(b.setIdx, i)
		}
		// plain key columns are written (an INSERT carries them); only
		// generated key columns are excluded from the write lists
		b.writeIdx = append(b.writeIdx, i)
		b.writeCols = append(b.writeCols, c.Name)
		b.writeColMeta = append(b.writeColMeta, c)
	}
	return b
}

func (b *Builder) keyCols() []string {
	kc := make([]string, len(b.keyIdx))
	for i, idx := range b.keyIdx {
		kc[i] = b.Cols[idx]
	}
	return kc
}

// KeyCols returns the key column names in key order (empty when keyless).
func (b *Builder) KeyCols() []string { return b.keyCols() }

// Insert renders a single-row INSERT of the source row's WRITABLE columns
// (generated columns are excluded — P0-2).
func (b *Builder) Insert(vals []any) string {
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		conn.QuoteIdent(b.Table), colList(b.writeCols), b.writeValList(vals))
}

// InsertBatch renders one multi-row INSERT of the writable columns. Rows
// must have len(b.Cols) values each (the full compared row); an empty
// batch is an error (a zero-row INSERT is meaningless and some servers
// reject it).
func (b *Builder) InsertBatch(rows [][]any) (string, error) {
	if len(rows) == 0 {
		return "", errors.New("empty INSERT batch")
	}
	tuples := make([]string, len(rows))
	for i, r := range rows {
		if len(r) != len(b.Cols) {
			return "", fmt.Errorf("row %d has %d values, want %d", i, len(r), len(b.Cols))
		}
		tuples[i] = "(" + b.writeValList(r) + ")"
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES %s",
		conn.QuoteIdent(b.Table), colList(b.writeCols), strings.Join(tuples, ", ")), nil
}

// Update renders an UPDATE of all compared non-key columns. keyVals must be
// the DESTINATION row's raw key values (in key order): rows are matched by
// normalized key identity, which can be coarser than the raw values (e.g.
// trailing-space folding), so the WHERE clause must reference what is
// actually stored in the destination. rowVals is the full source row in
// Cols order.
func (b *Builder) Update(keyVals, rowVals []any) string {
	if len(b.keyIdx) == 0 {
		// Keyless tables never reach this: DecidePlan routes them to FULL.
		panic("sync: Update on a keyless table")
	}
	sets := make([]string, len(b.SetCols))
	for i, idx := range b.setIdx {
		sets[i] = fmt.Sprintf("%s=%s", conn.QuoteIdent(b.SetCols[i]), literalFor(b.cols[idx].Family, rowVals[idx]))
	}
	return fmt.Sprintf("UPDATE %s SET %s WHERE %s",
		conn.QuoteIdent(b.Table), strings.Join(sets, ", "),
		b.keyWhere(keyVals))
}

// Delete renders a DELETE for one destination row, addressed by its raw key
// values (in key order).
func (b *Builder) Delete(keyVals []any) string {
	if len(b.keyIdx) == 0 {
		panic("sync: Delete on a keyless table")
	}
	return fmt.Sprintf("DELETE FROM %s WHERE %s",
		conn.QuoteIdent(b.Table), b.keyWhere(keyVals))
}

// Truncate renders the whole-table wipe used by the FULL mode.
func (b *Builder) Truncate() string {
	return "TRUNCATE TABLE " + conn.QuoteIdent(b.Table)
}

// keyWhere renders one equality term per key component; a NULL component
// becomes IS NULL (plain col = NULL would match nothing). Each component is
// rendered per the key column's family (a non-ASCII string key must be a
// quoted string, not a hex blob, or it would not match).
func (b *Builder) keyWhere(keyVals []any) string {
	parts := make([]string, len(b.keyIdx))
	for i, idx := range b.keyIdx {
		id := conn.QuoteIdent(b.Cols[idx])
		if i < len(keyVals) && keyVals[i] == nil {
			parts[i] = id + " IS NULL"
		} else {
			parts[i] = id + " = " + literalFor(b.cols[idx].Family, keyVals[i])
		}
	}
	return strings.Join(parts, " AND ")
}

func colList(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = conn.QuoteIdent(c)
	}
	return strings.Join(q, ", ")
}

// writeValList renders the row's writable values, each per its column's
// family (generated columns excluded).
func (b *Builder) writeValList(vals []any) string {
	parts := make([]string, len(b.writeIdx))
	for i, idx := range b.writeIdx {
		parts[i] = literalFor(b.cols[idx].Family, vals[idx])
	}
	return strings.Join(parts, ", ")
}

// bindWriteRow converts a full raw row into bindable arguments for the
// writable columns only.
func (b *Builder) bindWriteRow(vals []any) []any {
	out := make([]any, len(b.writeIdx))
	for i, idx := range b.writeIdx {
		out[i] = bindArg(b.cols[idx].Family, vals[idx])
	}
	return out
}

// rowBytes estimates the rendered size of one row in a multi-row INSERT
// (the "(, )" overhead plus every value's literal): the flushers use it as
// a max_allowed_packet budget proxy.
func (b *Builder) rowBytes(vals []any) int {
	n := 8
	for i, v := range vals {
		n += len(literalFor(b.cols[i].Family, v))
	}
	return n
}

// The Exec methods are the EXECUTABLE statements (P0-3): a parameterized
// query plus a separate argument list, always. The driver binds the
// arguments as data on the server (interpolateParams is off — see
// conn.buildDSN), so a value is never rendered into the statement text on
// the client: under NO_BACKSLASH_ESCAPES a string like C:\abc\def cannot
// be mangled, because it is not parsed as SQL at all. The literal
// renderers above stay for display (dry-run samples) and for read-side
// predicates, which are never writes.

// placeholders renders n "?", comma-joined.
func placeholders(n int) string {
	q := make([]string, n)
	for i := range q {
		q[i] = "?"
	}
	return strings.Join(q, ", ")
}

// bindArg converts one raw driver value into a bindable argument for a
// column of the given family. The driver delivers DECIMAL, ENUM, SET,
// character strings, JSON and BIT as []byte; all of those bind as strings
// (the server decodes them against the column type). BIT binds as its
// DECIMAL value: the byte pattern is meaningless as a string, and a
// decimal is what a BIT column accepts losslessly. A uint64 past
// int64 range (an unsigned BIGINT) binds as a decimal string.
func bindArg(fam string, v any) any {
	switch val := v.(type) {
	case nil:
		return nil
	case []byte:
		if fam == conn.FamBIT {
			return bitDecimal(val)
		}
		return string(val)
	case string:
		return val
	case int:
		return int64(val)
	case int8:
		return int64(val)
	case int16:
		return int64(val)
	case int32:
		return int64(val)
	case int64:
		return val
	case uint64:
		if val > math.MaxInt64 {
			return strconv.FormatUint(val, 10)
		}
		return int64(val)
	case float32:
		return float64(val)
	case float64:
		return val
	case bool:
		return val
	case time.Time:
		return val
	default:
		return fmt.Sprintf("%v", val)
	}
}

// bitDecimal renders the driver's big-endian BIT bytes as a decimal
// string (MySQL BITs are at most 64 bits wide).
func bitDecimal(b []byte) string {
	var n uint64
	for _, x := range b {
		n = n<<8 | uint64(x)
	}
	return strconv.FormatUint(n, 10)
}

// InsertExec is the parameterized single-row INSERT of the writable
// columns.
func (b *Builder) InsertExec(vals []any) (string, []any) {
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
			conn.QuoteIdent(b.Table), colList(b.writeCols), placeholders(len(b.writeIdx))),
		b.bindWriteRow(vals)
}

// InsertBatchExec is the parameterized multi-row INSERT. Rows must have
// len(b.Cols) values each; an empty batch is an error (a zero-row INSERT
// is meaningless and some servers reject it).
func (b *Builder) InsertBatchExec(rows [][]any) (string, []any, error) {
	if len(rows) == 0 {
		return "", nil, errors.New("empty INSERT batch")
	}
	tuples := make([]string, len(rows))
	args := make([]any, 0, len(rows)*len(b.writeIdx))
	for i, r := range rows {
		if len(r) != len(b.Cols) {
			return "", nil, fmt.Errorf("row %d has %d values, want %d", i, len(r), len(b.Cols))
		}
		tuples[i] = "(" + placeholders(len(b.writeIdx)) + ")"
		args = append(args, b.bindWriteRow(r)...)
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES %s",
		conn.QuoteIdent(b.Table), colList(b.writeCols), strings.Join(tuples, ", ")), args, nil
}

// UpdateExec is the parameterized UPDATE of all compared non-key columns.
// keyVals must be the DESTINATION row's raw key values (in key order):
// rows are matched by normalized key identity, which can be coarser than
// raw values (e.g. trailing-space folding), so the WHERE clause must
// reference what is actually stored in the destination. rowVals is the
// full source row in Cols order.
func (b *Builder) UpdateExec(keyVals, rowVals []any) (string, []any) {
	if len(b.keyIdx) == 0 {
		// Keyless tables never reach this: DecidePlan routes them to FULL.
		panic("sync: Update on a keyless table")
	}
	sets := make([]string, len(b.SetCols))
	args := make([]any, 0, len(b.SetCols)+len(b.keyIdx))
	for i, idx := range b.setIdx {
		sets[i] = conn.QuoteIdent(b.SetCols[i]) + " = ?"
		args = append(args, bindArg(b.cols[idx].Family, rowVals[idx]))
	}
	where, wargs := b.keyWhereExec(keyVals)
	return fmt.Sprintf("UPDATE %s SET %s WHERE %s",
		conn.QuoteIdent(b.Table), strings.Join(sets, ", "), where), append(args, wargs...)
}

// DeleteExec is the parameterized DELETE for one destination row,
// addressed by its raw key values (in key order).
func (b *Builder) DeleteExec(keyVals []any) (string, []any) {
	if len(b.keyIdx) == 0 {
		panic("sync: Delete on a keyless table")
	}
	where, args := b.keyWhereExec(keyVals)
	return fmt.Sprintf("DELETE FROM %s WHERE %s",
		conn.QuoteIdent(b.Table), where), args
}

// keyWhereExec is keyWhere's parameterized twin: one "? = ?" term per key
// component, IS NULL for a NULL component.
func (b *Builder) keyWhereExec(keyVals []any) (string, []any) {
	parts := make([]string, len(b.keyIdx))
	args := make([]any, 0, len(b.keyIdx))
	for i, idx := range b.keyIdx {
		id := conn.QuoteIdent(b.Cols[idx])
		if i < len(keyVals) && keyVals[i] == nil {
			parts[i] = id + " IS NULL"
		} else {
			parts[i] = id + " = ?"
			args = append(args, bindArg(b.cols[idx].Family, keyVals[i]))
		}
	}
	return strings.Join(parts, " AND "), args
}
