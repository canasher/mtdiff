package sync

import (
	"errors"
	"fmt"
	"strings"

	"mtdiff/internal/conn"
)

// Builder renders the sync statements for one table.
//
// Cols is the compared column set (ignored columns already removed) in
// source order, key columns included; every value rendered here is the RAW
// driver value scanned from the source (or, for DELETE, from the
// destination) — never a normalized/canonical form, which would mutate data
// (e.g. a folded-case string or a re-canonicalized decimal).
type Builder struct {
	Table   string
	Cols    []string
	SetCols []string      // compared non-key columns, in Cols order
	cols    []conn.Column // parallel to Cols: full metadata (family drives value rendering)
	keyIdx  []int         // position of each key column in Cols, in key order
	setIdx  []int         // position of each SetCol in Cols, in SetCols order
}

// NewBuilder builds the statement builder for a table's compared schema.
func NewBuilder(table string, schema *conn.Schema) *Builder {
	b := &Builder{Table: table}
	keySet := make(map[string]bool, len(schema.Key))
	for _, k := range schema.Key {
		keySet[k] = true
	}
	for i, c := range schema.Cols {
		b.Cols = append(b.Cols, c.Name)
		b.cols = append(b.cols, c)
		if keySet[c.Name] {
			b.keyIdx = append(b.keyIdx, i)
		} else {
			b.SetCols = append(b.SetCols, c.Name)
			b.setIdx = append(b.setIdx, i)
		}
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

// Insert renders a single-row INSERT of the full source row.
func (b *Builder) Insert(vals []any) string {
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		conn.QuoteIdent(b.Table), colList(b.Cols), b.valList(vals))
}

// InsertBatch renders one multi-row INSERT. Rows must have len(b.Cols)
// values each; an empty batch is an error (a zero-row INSERT is meaningless
// and some servers reject it).
func (b *Builder) InsertBatch(rows [][]any) (string, error) {
	if len(rows) == 0 {
		return "", errors.New("empty INSERT batch")
	}
	tuples := make([]string, len(rows))
	for i, r := range rows {
		if len(r) != len(b.Cols) {
			return "", fmt.Errorf("row %d has %d values, want %d", i, len(r), len(b.Cols))
		}
		tuples[i] = "(" + b.valList(r) + ")"
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES %s",
		conn.QuoteIdent(b.Table), colList(b.Cols), strings.Join(tuples, ", ")), nil
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

// valList renders a full row's values, each per its column's family.
func (b *Builder) valList(vals []any) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = literalFor(b.cols[i].Family, v)
	}
	return strings.Join(parts, ", ")
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
