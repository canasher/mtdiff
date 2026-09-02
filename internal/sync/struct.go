package sync

import (
	"fmt"
	"strings"

	"mtdiff/internal/conn"
)

// ChangeKind classifies one structure modification.
type ChangeKind int

const (
	ChangeAddColumn ChangeKind = iota
	ChangeModifyColumn
	ChangeDropColumn
	ChangeAddIndex
	ChangeDropIndex
)

// Change is one structure modification to apply on the destination.
// For AddColumn the position in the source table is recorded (First, or
// After the named column) so the destination's physical column order ends
// up matching the source's — the comparison aligns columns by position.
type Change struct {
	Kind  ChangeKind
	Col   conn.ColMeta // Add/Modify: the source definition; Drop: name only
	Idx   conn.Index
	First bool
	After string
	Why   string // short reason, shown in the report
}

// DiffStructure computes the changes that bring dst's structure in line
// with src's: missing columns are added, drifted columns re-defined,
// destination-only columns dropped, and the primary/unique key sets
// aligned. Indexes are compared by column sequence, not by name: an index
// name is an implementation detail, the columns it covers are the
// structure. A rename is not detected — it becomes a drop plus an add,
// which is safe under the disposable-destination semantics.
//
// It returns an error when a source definition cannot be reproduced on the
// destination (an expression default): the caller then fails the table
// instead of guessing.
func DiffStructure(src, dst *conn.Struct) ([]Change, error) {
	srcCols := make(map[string]bool, len(src.Cols))
	for _, c := range src.Cols {
		srcCols[c.Name] = true
	}
	dstCols := make(map[string]conn.ColMeta, len(dst.Cols))
	for _, c := range dst.Cols {
		dstCols[c.Name] = c
	}

	var out []Change
	for i, sc := range src.Cols {
		dc, ok := dstCols[sc.Name]
		switch {
		case !ok:
			if err := addable(sc); err != nil {
				return nil, err
			}
			ch := Change{Kind: ChangeAddColumn, Col: sc, Why: fmt.Sprintf("column %s missing on destination", sc.Name)}
			if i == 0 {
				ch.First = true
			} else {
				ch.After = src.Cols[i-1].Name
			}
			out = append(out, ch)
		case colDiffers(sc, dc):
			if err := addable(sc); err != nil {
				return nil, err
			}
			out = append(out, Change{Kind: ChangeModifyColumn, Col: sc, Why: colWhy(sc, dc)})
		}
	}
	for _, dc := range dst.Cols {
		if !srcCols[dc.Name] {
			out = append(out, Change{Kind: ChangeDropColumn, Col: dc, Why: fmt.Sprintf("column %s not in source", dc.Name)})
		}
	}

	srcIdx := make(map[string]conn.Index, len(src.Indexes))
	for _, ix := range src.Indexes {
		srcIdx[indexKey(ix.Cols)] = ix
	}
	dstIdx := make(map[string]conn.Index, len(dst.Indexes))
	for _, ix := range dst.Indexes {
		dstIdx[indexKey(ix.Cols)] = ix
	}
	for _, di := range dst.Indexes {
		if _, ok := srcIdx[indexKey(di.Cols)]; !ok {
			out = append(out, Change{Kind: ChangeDropIndex, Idx: di, Why: fmt.Sprintf("index %s (%s) not in source", di.Name, strings.Join(di.Cols, ", "))})
		}
	}
	for _, si := range src.Indexes {
		if _, ok := dstIdx[indexKey(si.Cols)]; !ok {
			if si.Name == "PRIMARY" {
				out = append(out, Change{Kind: ChangeAddIndex, Idx: si, Why: fmt.Sprintf("primary key (%s) missing on destination", strings.Join(si.Cols, ", "))})
			} else {
				out = append(out, Change{Kind: ChangeAddIndex, Idx: si, Why: fmt.Sprintf("unique index (%s) missing on destination", strings.Join(si.Cols, ", "))})
			}
		}
	}
	return out, nil
}

// colDiffers reports whether a column's definition drifted. Comments are
// deliberately not compared: they are cosmetic and unreliable across
// compatible layers, so a re-defined column simply carries the source's
// comment along (see renderColDef) without a drift on its own.
//
// A DATETIME/TIMESTAMP swap is also not a drift here: converting the
// column rewrites the stored values, which is a data decision the operator
// makes with --allow-tz-swap, not something the structure sync may do
// silently.
func colDiffers(sc, dc conn.ColMeta) bool {
	if (sc.Family == conn.FamDATETIME && dc.Family == conn.FamTIMESTAMP) ||
		(sc.Family == conn.FamTIMESTAMP && dc.Family == conn.FamDATETIME) {
		return false
	}
	if !strings.EqualFold(sc.RawType, dc.RawType) {
		return true
	}
	if sc.Nullable != dc.Nullable {
		return true
	}
	if sc.HasDefault != dc.HasDefault || sc.Default != dc.Default {
		return true
	}
	if sc.AutoInc != dc.AutoInc || sc.OnUpdate != dc.OnUpdate {
		return true
	}
	if sc.Charset != dc.Charset || sc.Collation != dc.Collation {
		return true
	}
	return false
}

// colWhy renders a short drift description for the report.
func colWhy(sc, dc conn.ColMeta) string {
	switch {
	case !strings.EqualFold(sc.RawType, dc.RawType):
		return fmt.Sprintf("type %s -> %s", dc.RawType, sc.RawType)
	case sc.Nullable != dc.Nullable:
		return fmt.Sprintf("nullability %v -> %v", dc.Nullable, sc.Nullable)
	case sc.HasDefault != dc.HasDefault || sc.Default != dc.Default:
		return fmt.Sprintf("default %q -> %q", dc.Default, sc.Default)
	case sc.AutoInc != dc.AutoInc:
		return fmt.Sprintf("auto_increment %v -> %v", dc.AutoInc, sc.AutoInc)
	case sc.OnUpdate != dc.OnUpdate:
		return "on-update clause differs"
	case sc.Charset != dc.Charset || sc.Collation != dc.Collation:
		return fmt.Sprintf("charset/collation %s/%s -> %s/%s", dc.Charset, dc.Collation, sc.Charset, sc.Collation)
	}
	return "definition differs"
}

// addable rejects source definitions the destination cannot be given
// faithfully. An expression default ("(expr)", MySQL 8.0.13+) is stored in
// information_schema unevaluated; re-emitting it would mean trusting the
// expression to still exist on the other server, so the table fails the
// structure sync instead of guessing.
func addable(c conn.ColMeta) error {
	if c.HasDefault && strings.HasPrefix(c.Default, "(") {
		return fmt.Errorf("column %s has an expression default (%s) that cannot be reproduced on the destination; re-run with --no-sync-schema",
			c.Name, c.Default)
	}
	return nil
}

// filterStruct removes ignored columns from a structure (and any index that
// references one), so the structure sync skips them exactly like the data
// comparison does. An index touching an ignored column is left untouched on
// the destination even when the source has no such index.
func filterStruct(s *conn.Struct, ignore map[string]bool) *conn.Struct {
	if len(ignore) == 0 {
		return s
	}
	out := &conn.Struct{Table: s.Table}
	for _, c := range s.Cols {
		if !ignore[c.Name] {
			out.Cols = append(out.Cols, c)
		}
	}
	for _, ix := range s.Indexes {
		touched := false
		for _, c := range ix.Cols {
			if ignore[c] {
				touched = true
				break
			}
		}
		if !touched {
			out.Indexes = append(out.Indexes, ix)
		}
	}
	return out
}

// indexKey keys a unique index by its column sequence (case-insensitive),
// which is what makes two key sets equal — the name is not.
func indexKey(cols []string) string {
	return strings.ToLower(strings.Join(cols, "\x00"))
}

// RenderDDL renders the changes as DDL for the destination table: one
// ALTER TABLE with all clauses. InnoDB executes it atomically (all-or-
// nothing), so a failure never leaves a half-migrated structure; the
// single round trip also keeps a mid-list failure visible in one server
// error. Clause order is deliberate: drop indexes before the columns they
// reference, and add them after the columns exist (a primary key may only
// be added to columns that are already in place).
//
// It returns nil when there is nothing to do.
func RenderDDL(table string, changes []Change) []string {
	if len(changes) == 0 {
		return nil
	}
	var dropIdx, dropCol, modifyCol, addCol, addIdx []string
	for _, ch := range changes {
		switch ch.Kind {
		case ChangeDropIndex:
			dropIdx = append(dropIdx, renderDropIndex(ch.Idx))
		case ChangeDropColumn:
			dropCol = append(dropCol, "DROP COLUMN "+conn.QuoteIdent(ch.Col.Name))
		case ChangeModifyColumn:
			modifyCol = append(modifyCol, "MODIFY COLUMN "+renderColDef(ch.Col))
		case ChangeAddColumn:
			addCol = append(addCol, renderAddColumn(ch))
		case ChangeAddIndex:
			addIdx = append(addIdx, renderAddIndex(ch.Idx))
		}
	}
	clauses := make([]string, 0, len(dropIdx)+len(dropCol)+len(modifyCol)+len(addCol)+len(addIdx))
	clauses = append(clauses, dropIdx...)
	clauses = append(clauses, dropCol...)
	clauses = append(clauses, modifyCol...)
	clauses = append(clauses, addCol...)
	clauses = append(clauses, addIdx...)
	return []string{"ALTER TABLE " + conn.QuoteIdent(table) + " " + strings.Join(clauses, ", ")}
}

// renderColDef re-emits a source column definition for use in ADD/MODIFY
// COLUMN. The default is re-emitted verbatim: information_schema stores
// defaults as declared (0, 'x', CURRENT_TIMESTAMP), so round-tripping the
// text is faithful. The comment is carried along even though it is not
// diffed, so a re-defined column keeps the source's comment instead of
// silently losing it.
func renderColDef(c conn.ColMeta) string {
	var b strings.Builder
	b.WriteString(conn.QuoteIdent(c.Name))
	b.WriteByte(' ')
	b.WriteString(c.RawType)
	if !c.Nullable {
		b.WriteString(" NOT NULL")
	}
	if c.HasDefault {
		b.WriteString(" DEFAULT ")
		b.WriteString(c.Default)
	}
	if c.OnUpdate {
		b.WriteString(" ON UPDATE CURRENT_TIMESTAMP")
	}
	if c.AutoInc {
		b.WriteString(" AUTO_INCREMENT")
	}
	if c.Charset != "" {
		b.WriteString(" CHARACTER SET ")
		b.WriteString(c.Charset)
		if c.Collation != "" {
			b.WriteString(" COLLATE ")
			b.WriteString(c.Collation)
		}
	}
	if c.Comment != "" {
		b.WriteString(" COMMENT ")
		b.WriteString(quoteSQLString(c.Comment))
	}
	return b.String()
}

func renderAddColumn(ch Change) string {
	def := "ADD COLUMN " + renderColDef(ch.Col)
	if ch.First {
		return def + " FIRST"
	}
	if ch.After != "" {
		return def + " AFTER " + conn.QuoteIdent(ch.After)
	}
	return def
}

func renderAddIndex(ix conn.Index) string {
	cols := make([]string, len(ix.Cols))
	for i, c := range ix.Cols {
		cols[i] = conn.QuoteIdent(c)
	}
	list := "(" + strings.Join(cols, ", ") + ")"
	if ix.Name == "PRIMARY" {
		return "ADD PRIMARY KEY " + list
	}
	return "ADD UNIQUE " + list
}

func renderDropIndex(ix conn.Index) string {
	if ix.Name == "PRIMARY" {
		return "DROP PRIMARY KEY"
	}
	return "DROP INDEX " + conn.QuoteIdent(ix.Name)
}

// quoteSQLString renders a string as a quoted SQL literal (backslashes
// escaped, quotes doubled — the write connection runs with the default
// sql_mode, i.e. without NO_BACKSLASH_ESCAPES).
func quoteSQLString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "'", "''")
	return "'" + s + "'"
}
