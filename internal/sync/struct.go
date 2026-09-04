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
// srcDef and dstDef are each side's default collation for its database
// (conn.DefaultCollation; "" when unknown). They let the comparison tell
// an explicitly chosen collation apart from "left to the backend's
// default" — see colDiffers.
//
// It returns an error when a source definition cannot be reproduced on the
// destination (a generated column — the expression is a cross-backend
// promise; or an expression default): the caller then fails the table
// instead of guessing.
func DiffStructure(src, dst *conn.Struct, srcDef, dstDef string) ([]Change, error) {
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
		case colDiffers(sc, dc, srcDef, dstDef):
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
//
// Two cross-backend cosmetic differences are not drifts either: integer
// display widths (5.7/TiDB report "int(11)" where 8.0 reports "int") and
// collations that are each side's own database default — information
// schema cannot say whether a collation was declared, so "both sides left
// it to the default" is the closest test available, and re-emitting the
// source's default collation DDL at every sync would be noise. An
// explicitly declared collation (different from the side's own default)
// still drifts against a different one.
func colDiffers(sc, dc conn.ColMeta, srcDef, dstDef string) bool {
	if (sc.Family == conn.FamDATETIME && dc.Family == conn.FamTIMESTAMP) ||
		(sc.Family == conn.FamTIMESTAMP && dc.Family == conn.FamDATETIME) {
		return false
	}
	// A column becoming generated (or ceasing to be) is a semantic change
	// the raw type alone would not reveal: the generated-ness is part of
	// the definition. The EXPRESSION is part of it too (P1-1): two
	// generated columns of the same storage that compute different values
	// are a drift, and a drift with an unreadable side is a safe refusal
	// (addable) — never a silent match.
	if sc.Generated != dc.Generated ||
		(sc.Generated && !strings.EqualFold(sc.GenStorage, dc.GenStorage)) ||
		(sc.Generated && dc.Generated && genExprDiffers(sc, dc)) {
		return true
	}
	if normalizeIntType(sc.RawType) != normalizeIntType(dc.RawType) {
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
	if sc.Charset != dc.Charset {
		return true
	}
	if sc.Collation != dc.Collation && !defaultCollationsMatch(sc, dc, srcDef, dstDef) {
		return true
	}
	return false
}

// genExprDiffers compares two generated columns' generation expressions
// (P1-1). Normalization is deliberately conservative: it folds
// surrounding whitespace and outermost paren wrapping, and NOTHING else
// (no AST re-print, no identifier case-folding, no operator rewriting) —
// an expression that differs in any other way is a real difference. An
// expression either side cannot read ("" — the backend does not expose
// GENERATION_EXPRESSION) never matches a readable one: not comparable is
// not the same as equal, so the pair counts as a drift and the structure
// sync refuses rather than guessing. Two unreadable expressions degrade
// to the Generated/GenStorage check only.
func genExprDiffers(sc, dc conn.ColMeta) bool {
	return normalizeGenerationExpr(sc.GenExpr) != normalizeGenerationExpr(dc.GenExpr)
}

// normalizeGenerationExpr folds the cosmetic differences a backend may
// introduce when it re-prints the same expression: surrounding
// whitespace and whole-string paren wrapping, peeled while the string
// stays fully wrapped ("((a)+(b))" -> "(a)+(b)", then it stops: the
// remainder is not one balanced outer pair). NOTHING else is normalized
// (no AST re-print, no identifier case-folding, no operator rewriting).
// Expressions that contain a quote are trimmed only — a parenthesis
// inside a string literal would defeat the balance scan, and
// under-normalizing is the safe direction (a cosmetic match missed is a
// drift reported; a different expression declared equal is data
// corruption).
func normalizeGenerationExpr(expr string) string {
	e := strings.TrimSpace(expr)
	if strings.ContainsAny(e, "'\"") {
		return e
	}
	for strings.HasPrefix(e, "(") && strings.HasSuffix(e, ")") && outerParens(e) {
		e = strings.TrimSpace(e[1 : len(e)-1])
	}
	return e
}

// outerParens reports whether the whole string is wrapped in ONE balanced
// pair of parentheses (the opener at index 0 closes at the final
// position): "(a) + (b)" is, "(a)+b" is not.
func outerParens(s string) bool {
	depth := 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i == len(s)-1
			}
		}
	}
	return false
}

// normalizeIntType folds integer display widths into the bare type
// ("int(11)" -> "int", "int(10) unsigned" -> "int unsigned"). MySQL 8.0
// stopped reporting display widths while 5.7 and TiDB still do, and they
// never change the storage. Only single-argument integer types are
// affected: decimal(10,2) keeps its spec, as do char/varchar lengths and
// enum/set value lists.
func normalizeIntType(rawType string) string {
	l := strings.ToLower(strings.TrimSpace(rawType))
	i := strings.IndexByte(l, '(')
	if i < 0 {
		return l
	}
	base := strings.TrimSpace(l[:i])
	unsigned := strings.HasSuffix(l, "unsigned")
	switch base {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint":
		if unsigned {
			return base + " unsigned"
		}
		return base
	}
	return l
}

// defaultCollationsMatch reports whether the collation difference between
// two columns of the same charset is explainable by the backends'
// different defaults: both sides' values equal their own database
// default. A missing default ("" — unknown) disables the test and the
// difference counts as drift, the strict pre-existing behavior.
func defaultCollationsMatch(sc, dc conn.ColMeta, srcDef, dstDef string) bool {
	return srcDef != "" && dstDef != "" &&
		strings.EqualFold(sc.Collation, srcDef) &&
		strings.EqualFold(dc.Collation, dstDef)
}

// colWhy renders a short drift description for the report.
func colWhy(sc, dc conn.ColMeta) string {
	switch {
	case sc.Generated != dc.Generated:
		return fmt.Sprintf("generated column %v -> %v", dc.Generated, sc.Generated)
	case sc.Generated && !strings.EqualFold(sc.GenStorage, dc.GenStorage):
		return fmt.Sprintf("generated storage %s -> %s", dc.GenStorage, sc.GenStorage)
	case sc.Generated && genExprDiffers(sc, dc):
		if sc.GenExpr == "" || dc.GenExpr == "" {
			return "generated expression unreadable on one side"
		}
		return "generated expression differs"
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
// faithfully. A generated column is the first case (P0-2): the generation
// expression is a cross-backend promise (it may reference other columns or
// server functions that do not exist, or do not behave alike, on the other
// side), and a re-defined column would silently LOSE the expression — so
// the structure sync refuses instead of guessing; the operator aligns the
// schema manually or turns the structure sync off. An expression default
// ("(expr)", MySQL 8.0.13+) is stored in information_schema unevaluated;
// re-emitting it would mean trusting the expression to still exist on the
// other server, so the table fails the structure sync instead of guessing.
func addable(c conn.ColMeta) error {
	if c.Generated {
		storage := c.GenStorage
		if storage == "" {
			storage = "GENERATED"
		}
		return fmt.Errorf("column %s is a generated column (%s) that the structure sync cannot reproduce on the destination; align the schema manually or use --no-sync-schema",
			c.Name, storage)
	}
	if c.HasDefault && strings.HasPrefix(c.Default, "(") {
		return fmt.Errorf("column %s has an expression default (%s) that cannot be reproduced on the destination; re-run with --no-sync-schema",
			c.Name, c.Default)
	}
	return nil
}

// generatedCols lists a structure's generated columns ("" when none). The
// CREATE path uses it: a table with a generated column cannot be created
// on the destination faithfully (see addable), so the create plan is
// refused instead of emitting a structure that silently lacks the
// expression.
func generatedCols(s *conn.Struct) []string {
	var out []string
	for _, c := range s.Cols {
		if c.Generated {
			out = append(out, c.Name)
		}
	}
	return out
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

// UsableKeyOf picks the key an introspected structure offers for chunking,
// with the same rule as conn.SelectKey: the primary key first, then the
// first unique index whose columns are all NOT NULL (a unique index on a
// nullable column cannot address rows reliably). nil when no usable key
// exists. The structure-sync path uses it to decide whether a repaired
// (re-created) table can go back to row-level sync.
func UsableKeyOf(s *conn.Struct) []string {
	nullable := make(map[string]bool, len(s.Cols))
	for _, c := range s.Cols {
		nullable[c.Name] = c.Nullable
	}
	var uniques []conn.Index
	for _, ix := range s.Indexes {
		if ix.Name == "PRIMARY" {
			return ix.Cols
		}
		if ix.Unique {
			uniques = append(uniques, ix)
		}
	}
	for _, ix := range uniques {
		allNotNULL := true
		for _, c := range ix.Cols {
			if nullable[c] {
				allNotNULL = false
				break
			}
		}
		if allNotNULL {
			return ix.Cols
		}
	}
	return nil
}

// RenderDDL renders the changes as DDL for the destination table:
// normally one ALTER TABLE with all clauses. InnoDB executes it atomically
// (all-or-nothing), so a failure never leaves a half-migrated structure;
// the single round trip also keeps a mid-list failure visible in one
// server error. Clause order is deliberate: drop indexes before the
// columns they reference, and add them after the columns exist (a primary
// key may only be added to columns that are already in place).
//
// Two exceptions, both found on TiDB, may split the batch:
//
//   - an index change whose columns are re-defined (MODIFY) or added in
//     this same batch runs in a follow-up statement after the column work
//     has been applied: TiDB rejects two operations on one column in a
//     single DDL job (Error 8200 "Unsupported operate same column"),
//     e.g. MODIFY COLUMN id …, ADD PRIMARY KEY (id);
//   - an index drop confined to columns that are themselves dropped is not
//     emitted at all: both backends remove an index together with its
//     column, so an explicit drop would fail once the column is gone.
func RenderDDL(table string, changes []Change) []string {
	if len(changes) == 0 {
		return nil
	}
	// Column names re-defined or added in this batch: an index on one of
	// them cannot share a statement with the column work (see above).
	touched := make(map[string]bool)
	// Columns being dropped: their indexes die with them, no explicit drop.
	dropped := make(map[string]bool)
	for _, ch := range changes {
		switch ch.Kind {
		case ChangeModifyColumn, ChangeAddColumn:
			touched[ch.Col.Name] = true
		case ChangeDropColumn:
			dropped[ch.Col.Name] = true
		}
	}
	refsAny := func(cols []string, set map[string]bool) bool {
		for _, c := range cols {
			if set[c] {
				return true
			}
		}
		return false
	}
	var dropIdx, dropCol, modifyCol, addCol, addIdx, deferIdx []string
	for _, ch := range changes {
		switch ch.Kind {
		case ChangeDropIndex:
			switch {
			case refsAny(ch.Idx.Cols, dropped):
				// the index is removed together with its column
			case refsAny(ch.Idx.Cols, touched):
				deferIdx = append(deferIdx, renderDropIndex(ch.Idx))
			default:
				dropIdx = append(dropIdx, renderDropIndex(ch.Idx))
			}
		case ChangeDropColumn:
			dropCol = append(dropCol, "DROP COLUMN "+conn.QuoteIdent(ch.Col.Name))
		case ChangeModifyColumn:
			modifyCol = append(modifyCol, "MODIFY COLUMN "+renderColDef(ch.Col))
		case ChangeAddColumn:
			addCol = append(addCol, renderAddColumn(ch))
		case ChangeAddIndex:
			if refsAny(ch.Idx.Cols, touched) {
				deferIdx = append(deferIdx, renderAddIndex(ch.Idx))
				continue
			}
			addIdx = append(addIdx, renderAddIndex(ch.Idx))
		}
	}
	clauses := make([]string, 0, len(dropIdx)+len(dropCol)+len(modifyCol)+len(addCol)+len(addIdx))
	clauses = append(clauses, dropIdx...)
	clauses = append(clauses, dropCol...)
	clauses = append(clauses, modifyCol...)
	clauses = append(clauses, addCol...)
	clauses = append(clauses, addIdx...)
	out := []string{"ALTER TABLE " + conn.QuoteIdent(table) + " " + strings.Join(clauses, ", ")}
	if len(deferIdx) > 0 {
		out = append(out, "ALTER TABLE "+conn.QuoteIdent(table)+" "+strings.Join(deferIdx, ", "))
	}
	return out
}

// renderColDef re-emits a source column definition for use in ADD/MODIFY
// COLUMN and CREATE TABLE. The default is re-emitted verbatim:
// information_schema stores defaults as declared (0, 'x', CURRENT_TIMESTAMP),
// so round-tripping the text is faithful. The comment is carried along even
// though it is not diffed, so a re-defined column keeps the source's comment
// instead of silently losing it.
//
// Clause order follows the server grammar: CHARACTER SET / COLLATE attach
// directly to the data type and must precede NOT NULL (the server rejects
// "varchar(16) NOT NULL CHARACTER SET …"), while COMMENT comes last.
func renderColDef(c conn.ColMeta) string {
	var b strings.Builder
	b.WriteString(conn.QuoteIdent(c.Name))
	b.WriteByte(' ')
	b.WriteString(c.RawType)
	if c.Charset != "" {
		b.WriteString(" CHARACTER SET ")
		b.WriteString(c.Charset)
		if c.Collation != "" {
			b.WriteString(" COLLATE ")
			b.WriteString(c.Collation)
		}
	}
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

// RenderCreateTable renders a CREATE TABLE statement that reproduces the
// source structure on the destination: the columns (in source order, via
// the same definition renderer the ALTER path uses), the primary key and
// the unique indexes. The optional table options carry the source's
// storage engine and — when the source's next AUTO_INCREMENT value is
// known — the table's starting auto-increment counter, so a freshly
// created table already converges on the auto-increment state (see the
// table-state reconciliation). hasAutoInc is false when the source table
// has no auto-increment column (information_schema reports NULL): in that
// case no AUTO_INCREMENT option is emitted (a bare value would be
// meaningless).
func RenderCreateTable(table string, s *conn.Struct, engine string, autoInc int64, hasAutoInc bool) string {
	var b strings.Builder
	b.WriteString("CREATE TABLE " + conn.QuoteIdent(table) + " (\n")
	lines := make([]string, 0, len(s.Cols)+len(s.Indexes))
	for _, c := range s.Cols {
		lines = append(lines, "  "+renderColDef(c))
	}
	for _, ix := range s.Indexes {
		cols := make([]string, len(ix.Cols))
		for i, c := range ix.Cols {
			cols[i] = conn.QuoteIdent(c)
		}
		list := "(" + strings.Join(cols, ", ") + ")"
		if ix.Name == "PRIMARY" {
			lines = append(lines, "  PRIMARY KEY "+list)
		} else {
			lines = append(lines, "  UNIQUE KEY "+conn.QuoteIdent(ix.Name)+" "+list)
		}
	}
	b.WriteString(strings.Join(lines, ",\n"))
	b.WriteString("\n)")
	if engine != "" {
		b.WriteString(" ENGINE=" + engine)
	}
	if hasAutoInc {
		fmt.Fprintf(&b, " AUTO_INCREMENT=%d", autoInc)
	}
	b.WriteString(";")
	return b.String()
}

// DestructiveDDL reports whether a DDL statement is irreversible or
// lossy: it drops data (DROP TABLE, DROP COLUMN) or removes a constraint
// (DROP PRIMARY KEY, DROP INDEX). The confirmation summary and the dry-run
// report must surface these separately from the rest, because they are the
// statements an operator would most want to see before an --apply.
func DestructiveDDL(stmt string) bool {
	u := strings.ToUpper(stmt)
	return strings.Contains(u, "DROP TABLE") || strings.Contains(u, "DROP COLUMN") ||
		strings.Contains(u, "DROP PRIMARY KEY") || strings.Contains(u, "DROP INDEX")
}
