package conn

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Column families used by the normalizer.
const (
	FamINT       = "INT"
	FamUINT      = "UINT"
	FamDECIMAL   = "DECIMAL"
	FamFLOAT     = "FLOAT"
	FamDOUBLE    = "DOUBLE"
	FamDATE      = "DATE"
	FamTIME      = "TIME"
	FamDATETIME  = "DATETIME"
	FamTIMESTAMP = "TIMESTAMP"
	FamYEAR      = "YEAR"
	FamENUM      = "ENUM"
	FamSET       = "SET"
	FamSTR       = "STR"
	FamBYTES     = "BYTES"
	FamJSON      = "JSON"
	FamBIT       = "BIT"
)

// Column describes one column of a table.
type Column struct {
	Name      string
	Family    string // one of the Fam* constants
	RawType   string // e.g. "decimal(10,2)", "varchar(255)"
	Precision int    // decimal digits
	Scale     int    // decimal scale / fractional seconds
	Collation string
	Nullable  bool
}

// Schema is the introspected structure of one table.
type Schema struct {
	Table     string
	Cols      []Column
	Key       []string // key column names in order; empty = no usable key
	KeySource string   // "primary" | "unique" | "explicit" | "none"
	// KeyIsUnique is true when the key is a primary key or a unique index.
	// For explicit non-unique keys the scanner must total-order the rows by
	// all remaining columns to keep digests deterministic.
	KeyIsUnique bool
}

// ColMeta is one column's full structure metadata (the structure-sync
// path). It extends Column with the attributes needed to re-create the
// column on the other side: default value, auto_increment, charset and
// comment.
type ColMeta struct {
	Column
	Default    string // COLUMN_DEFAULT verbatim: "NULL", a literal, CURRENT_TIMESTAMP, or an expression "(...)"
	HasDefault bool
	AutoInc    bool // EXTRA contains auto_increment
	OnUpdate   bool // EXTRA contains "on update"
	Charset    string
	Comment    string
}

// Index is one key of a table: the primary key (Name "PRIMARY") or a unique
// index, with its columns in index order. Non-unique indexes are not part
// of the structure sync (they are not needed to address rows).
type Index struct {
	Name   string
	Unique bool
	Cols   []string
}

// Struct is the full structure of one table: columns in physical order plus
// its primary/unique indexes.
type Struct struct {
	Table   string
	Cols    []ColMeta
	Indexes []Index
}

// QuoteIdent backtick-quotes an SQL identifier.
func QuoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// quoteIdent backtick-quotes an SQL identifier.
func quoteIdent(name string) string { return QuoteIdent(name) }

// classify maps a MySQL column type string to a normalized family.
// Unknown types fall back to STR (byte-exact comparison is fail-safe).
func classify(columnType string) (family string, precision, scale int) {
	l := strings.ToLower(strings.TrimSpace(columnType))
	base, rest := l, ""
	if i := strings.IndexByte(l, '('); i >= 0 {
		base, rest = l[:i], l[i+1:len(l)-1]
	}
	unsigned := strings.HasSuffix(l, "unsigned")
	base = strings.TrimSpace(strings.TrimSuffix(base, "unsigned"))
	switch base {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint":
		if unsigned {
			return FamUINT, 0, 0
		}
		return FamINT, 0, 0
	case "decimal", "dec", "numeric":
		p, s := parseDecimalSpec(rest)
		return FamDECIMAL, p, s
	case "float":
		return FamFLOAT, 0, 0
	case "double", "real":
		return FamDOUBLE, 0, 0
	case "date":
		return FamDATE, 0, 0
	case "datetime":
		return FamDATETIME, 0, parseTimeScale(rest)
	case "timestamp":
		return FamTIMESTAMP, 0, parseTimeScale(rest)
	case "time":
		return FamTIME, 0, parseTimeScale(rest)
	case "year":
		return FamYEAR, 0, 0
	case "enum":
		return FamENUM, 0, 0
	case "set":
		return FamSET, 0, 0
	case "char", "varchar", "tinytext", "text", "mediumtext", "longtext":
		return FamSTR, 0, 0
	case "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob":
		return FamBYTES, 0, 0
	case "json":
		return FamJSON, 0, 0
	case "bit":
		return FamBIT, 0, 0
	}
	return FamSTR, 0, 0
}

func parseDecimalSpec(spec string) (precision, scale int) {
	parts := strings.Split(strings.TrimSpace(spec), ",")
	if v, err := strconv.Atoi(parts[0]); err == nil {
		precision = v
	}
	if len(parts) == 2 {
		if v, err := strconv.Atoi(strings.TrimSpace(parts[1])); err == nil {
			scale = v
		}
	}
	return precision, scale
}

func parseTimeScale(spec string) int {
	if v, err := strconv.Atoi(strings.TrimSpace(spec)); err == nil {
		return v
	}
	return 0
}

// IntrospectTable reads column metadata and the usable key of a table.
func IntrospectTable(ctx context.Context, db *sql.DB, table string) (*Schema, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, COLLATION_NAME
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
		ORDER BY ORDINAL_POSITION`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	s := &Schema{Table: table}
	for rows.Next() {
		var col Column
		var nullable string
		var collation sql.NullString
		if err := rows.Scan(&col.Name, &col.RawType, &nullable, &collation); err != nil {
			return nil, err
		}
		col.Nullable = nullable == "YES"
		col.Collation = collation.String
		col.Family, col.Precision, col.Scale = classify(col.RawType)
		s.Cols = append(s.Cols, col)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(s.Cols) == 0 {
		return nil, fmt.Errorf("table %q not found in current database", table)
	}
	key, source, err := SelectKey(ctx, db, table)
	if err != nil {
		return nil, err
	}
	s.Key, s.KeySource = key, source
	s.KeyIsUnique = source == "primary" || source == "unique"
	return s, nil
}

// IntrospectStructure reads the full column metadata and the primary/unique
// indexes of a table. Unlike IntrospectTable (which serves comparison and
// stays minimal) it fetches everything the structure sync needs to re-create
// columns on the other side. Uses the MySQL 8.0 information_schema column
// names (COLUMN_DEFAULT, EXTRA); 5.7 naming differs and is not supported
// here.
func IntrospectStructure(ctx context.Context, db *sql.DB, table string) (*Struct, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, COLLATION_NAME,
		       CHARACTER_SET_NAME, COLUMN_DEFAULT, EXTRA, COLUMN_COMMENT
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
		ORDER BY ORDINAL_POSITION`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	s := &Struct{Table: table}
	for rows.Next() {
		var (
			col                     ColMeta
			nullable                string
			collation, charset, def sql.NullString
			extra, comment          string
		)
		if err := rows.Scan(&col.Name, &col.RawType, &nullable, &collation, &charset, &def, &extra, &comment); err != nil {
			return nil, err
		}
		col.Nullable = nullable == "YES"
		col.Collation = collation.String
		col.Charset = charset.String
		col.Default = def.String
		col.HasDefault = def.Valid
		col.Family, col.Precision, col.Scale = classify(col.RawType)
		col.AutoInc = strings.Contains(extra, "auto_increment")
		col.OnUpdate = strings.Contains(extra, "on update")
		col.Comment = comment
		s.Cols = append(s.Cols, col)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(s.Cols) == 0 {
		return nil, fmt.Errorf("table %q not found in current database", table)
	}
	s.Indexes, err = structureIndexes(ctx, db, table)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// structureIndexes returns the primary key and all unique indexes of the
// table, each with its columns in index order. Functional index parts (no
// physical column) are skipped, and non-unique indexes are dropped: the
// structure sync only needs the keys that address or constrain rows.
func structureIndexes(ctx context.Context, db *sql.DB, table string) ([]Index, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT INDEX_NAME, NON_UNIQUE, SEQ_IN_INDEX, COLUMN_NAME
		FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
		ORDER BY INDEX_NAME, SEQ_IN_INDEX`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Index
	var cur Index
	flush := func() {
		if cur.Name != "" {
			out = append(out, cur)
		}
		cur = Index{}
	}
	for rows.Next() {
		var (
			keyName  string
			nonUniq  string
			seqInIdx int
			colName  sql.NullString
		)
		if err := rows.Scan(&keyName, &nonUniq, &seqInIdx, &colName); err != nil {
			return nil, err
		}
		// PRIMARY is special-cased by name: older servers report its
		// NON_UNIQUE differently (the same reason SelectKey avoids SHOW INDEX).
		if keyName != "PRIMARY" && nonUniq != "0" {
			continue
		}
		if keyName != cur.Name {
			flush()
			cur = Index{Name: keyName, Unique: true}
		}
		if colName.Valid {
			cur.Cols = append(cur.Cols, colName.String)
		}
	}
	flush()
	return out, rows.Err()
}

// SelectKey picks the chunking key: primary key first, then the first unique
// index whose columns are all NOT NULL. A unique index on a nullable column
// is unusable for chunking: its NULL key rows match no chunk predicate
// (comparisons against NULL are always UNKNOWN) while COUNT(*) still counts
// them, so a change to those rows would be silently missed on both sides.
// Returns (nil, "none", nil) when no usable key exists.
//
// Uses information_schema.STATISTICS rather than SHOW INDEX: the latter's
// column count varies across MySQL versions (15 columns on 8.0.46), and
// database/sql requires the Scan destinations to match exactly.
func SelectKey(ctx context.Context, db *sql.DB, table string) ([]string, string, error) {
	nullable, err := nullableColumns(ctx, db, table)
	if err != nil {
		return nil, "", err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT INDEX_NAME, NON_UNIQUE, SEQ_IN_INDEX, COLUMN_NAME
		FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
		ORDER BY INDEX_NAME, SEQ_IN_INDEX`, table)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var pk []string
	type uniqueIdx struct {
		name string
		cols []string
	}
	var uniques []uniqueIdx
	for rows.Next() {
		var (
			keyName  string
			nonUniq  string
			seqInIdx int
			colName  sql.NullString
		)
		if err := rows.Scan(&keyName, &nonUniq, &seqInIdx, &colName); err != nil {
			return nil, "", err
		}
		if !colName.Valid {
			continue // functional index part: no physical column
		}
		switch {
		case keyName == "PRIMARY":
			pk = append(pk, colName.String)
		case nonUniq == "0":
			// Rows arrive sorted by INDEX_NAME: a new name starts a new
			// unique index, the same name extends the previous list.
			if len(uniques) > 0 && uniques[len(uniques)-1].name == keyName {
				uniques[len(uniques)-1].cols = append(uniques[len(uniques)-1].cols, colName.String)
			} else {
				uniques = append(uniques, uniqueIdx{keyName, []string{colName.String}})
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(pk) > 0 {
		return pk, "primary", nil
	}
	for _, u := range uniques {
		allNotNULL := true
		for _, c := range u.cols {
			if nullable[c] {
				allNotNULL = false
				break
			}
		}
		if allNotNULL {
			return u.cols, "unique", nil
		}
	}
	return nil, "none", nil
}

// nullableColumns returns the set of columns of the table that accept NULL
// (information_schema.COLUMNS.IS_NULLABLE = 'YES').
func nullableColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT COLUMN_NAME, IS_NULLABLE
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	set := make(map[string]bool)
	for rows.Next() {
		var (
			name     string
			nullable string
		)
		if err := rows.Scan(&name, &nullable); err != nil {
			return nil, err
		}
		if nullable == "YES" {
			set[name] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return set, nil
}

// DefaultCollation returns the collation that a column created without an
// explicit COLLATE would receive in the connection's database: the
// database-level default when the schema defines one, otherwise the server
// default. Structure sync uses it to tell an explicitly chosen collation
// from "left to the backend's default": backends disagree on what that
// default is (MySQL 8.0: utf8mb4_0900_ai_ci, TiDB: utf8mb4_bin), so two
// sides that both left it to the default are not in drift.
func DefaultCollation(ctx context.Context, db *sql.DB) (string, error) {
	var c string
	err := db.QueryRowContext(ctx,
		"SELECT COALESCE(s.DEFAULT_COLLATION_NAME, @@collation_server) "+
			"FROM information_schema.SCHEMATA s WHERE s.SCHEMA_NAME = DATABASE()").Scan(&c)
	if err == sql.ErrNoRows {
		err = db.QueryRowContext(ctx, "SELECT @@collation_server").Scan(&c)
	}
	if err != nil {
		return "", err
	}
	return c, nil
}

// ListTables returns the table names of the current database.
func ListTables(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SHOW TABLES")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		tables = append(tables, t)
	}
	return tables, rows.Err()
}

// ListBaseTables returns the BASE TABLE names of the current database (views
// and other object types are excluded: the sync reconciles regular tables
// only). Sorted for deterministic ordering.
func ListBaseTables(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT TABLE_NAME FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_TYPE = 'BASE TABLE'
		ORDER BY TABLE_NAME`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		tables = append(tables, t)
	}
	return tables, rows.Err()
}

// TableExists reports whether the table exists in the current database.
func TableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// informationSchemaAutoInc reads the AUTO_INCREMENT estimate from
// information_schema.TABLES: present is false when the backend reports
// NULL (the table has no auto-increment column); err when the query
// itself is not supported (the caller degrades to skipping the table-
// state reconciliation; see the sync package).
func informationSchemaAutoInc(ctx context.Context, db *sql.DB, table string) (value int64, present bool, err error) {
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx, `
		SELECT AUTO_INCREMENT FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table).Scan(&v); err != nil {
		if err == sql.ErrNoRows {
			return 0, false, nil
		}
		return 0, false, err
	}
	return v.Int64, v.Valid, nil
}

// showCreateAutoIncValue matches the table-level option
// ") ENGINE=InnoDB AUTO_INCREMENT=12001 ..." — present only when the
// counter was set explicitly (CREATE/ALTER with an initial value).
var showCreateAutoIncValueRe = regexp.MustCompile(`AUTO_INCREMENT=(\d+)`)

// TableAutoIncrement returns the value the server will assign to the
// table's next auto-increment row: the explicit counter (the
// AUTO_INCREMENT= clause of SHOW CREATE TABLE) when it exceeds
// max(column), otherwise max(column)+1 (1 for an empty table). The
// information_schema.TABLES estimate is not a reliable source — InnoDB
// does not refresh it when the counter changes a second time (a second
// ALTER TABLE ... AUTO_INCREMENT, or a TRUNCATE that resets it), and it
// stays stale until the table is dropped, so the explicit value and the
// column maximum are read directly. present is false when the backend
// reports NULL (the table has no auto-increment column); err when the
// state cannot be read at all (the caller degrades to skipping the
// table-state reconciliation; see the sync package).
func TableAutoIncrement(ctx context.Context, db *sql.DB, table string) (value int64, present bool, err error) {
	est, present, err := informationSchemaAutoInc(ctx, db, table)
	if err != nil || !present {
		return 0, present, err
	}
	col, explicit, hasExplicit, ok := showCreateAutoInc(ctx, db, table)
	if ok && col != "" {
		var m sql.NullInt64
		if err := db.QueryRowContext(ctx,
			"SELECT MAX("+QuoteIdent(col)+") FROM "+QuoteIdent(table)).Scan(&m); err == nil {
			next := int64(1)
			if m.Valid {
				next = m.Int64 + 1
			}
			if hasExplicit && explicit > next {
				next = explicit
			}
			return next, true, nil
		}
		if hasExplicit {
			return explicit, true, nil
		}
	} else if ok && hasExplicit {
		return explicit, true, nil
	}
	// The backend does not render a parseable SHOW CREATE (or the column
	// maximum could not be read): fall back to the estimate.
	return est, true, nil
}

// showCreateAutoIncCol matches the AUTO_INCREMENT column attribute in a
// column definition (the table option is excluded — it carries '='):
// "`id` int NOT NULL AUTO_INCREMENT". The span between the name and the
// attribute may not cross a backtick, so a match cannot run past the
// next column definition.
var showCreateAutoIncColRe = regexp.MustCompile("`([^`]*)`[^`\n]*?\\bAUTO_INCREMENT\\b(?:[^=]|$)")

// showCreateAutoInc parses the auto-increment facts out of SHOW CREATE
// TABLE: the auto-increment column name (from the column definition)
// and the explicit counter (the table-level AUTO_INCREMENT= clause).
// ok is false when the query fails or the output is unparseable.
func showCreateAutoInc(ctx context.Context, db *sql.DB, table string) (col string, explicit int64, hasExplicit, ok bool) {
	var name, create string
	if err := db.QueryRowContext(ctx, "SHOW CREATE TABLE "+QuoteIdent(table)).Scan(&name, &create); err != nil {
		return "", 0, false, false
	}
	col, explicit, hasExplicit = parseShowCreateAutoInc(create)
	return col, explicit, hasExplicit, true
}

// parseShowCreateAutoInc extracts, from a SHOW CREATE TABLE body, the
// auto-increment column name and the explicit counter (the table-level
// AUTO_INCREMENT= clause, present only when the counter was set
// explicitly).
func parseShowCreateAutoInc(create string) (col string, explicit int64, hasExplicit bool) {
	for _, line := range strings.Split(create, "\n") {
		if m := showCreateAutoIncValueRe.FindStringSubmatch(line); m != nil {
			if v, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				explicit, hasExplicit = v, true
			}
		}
	}
	if m := showCreateAutoIncColRe.FindStringSubmatch(create); m != nil {
		col = m[1]
	} else {
		// bare (unbackticked) column definitions: the line starts with
		// the identifier itself and carries the attribute without '='
		for _, line := range strings.Split(create, "\n") {
			trimmed := strings.TrimLeft(line, " \t")
			if strings.Contains(line, "AUTO_INCREMENT") && !strings.Contains(line, "AUTO_INCREMENT=") &&
				len(trimmed) > 0 && (trimmed[0] == '_' || trimmed[0] >= 'a' || trimmed[0] >= 'A') {
				col = firstIdent(line)
				break
			}
		}
	}
	return col, explicit, hasExplicit
}

// firstIdent extracts the leading identifier of a column definition line
// (backticked or bare).
func firstIdent(line string) string {
	line = strings.TrimLeft(line, " \t")
	if strings.HasPrefix(line, "`") {
		if end := strings.Index(line[1:], "`"); end >= 0 {
			return line[1 : 1+end]
		}
	}
	if i := strings.IndexAny(line, " ("); i > 0 {
		return line[:i]
	}
	return strings.TrimSpace(line)
}

// AutoIncGap probes one side's auto-increment reporting behavior with a
// read-only check: it finds the first table with an auto-increment
// column and returns how far the reported next value sits above
// max(column)+1. A large gap means the backend pre-allocates ID ranges
// (an allocator, e.g. TiDB's batch allocation): its reported next value
// is then an estimate that a plain INSERT history cannot explain, and an
// explicit counter below the allocated range's end is silently ignored —
// the table state is not exactly comparable. probed is false when the
// side has no auto-increment table (the check is inconclusive, not a
// degradation).
func AutoIncGap(ctx context.Context, db *sql.DB) (gap int64, probed bool, err error) {
	row := db.QueryRowContext(ctx, `
		SELECT TABLE_NAME, COLUMN_NAME FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND EXTRA LIKE '%auto_increment%'
		LIMIT 1`)
	var table, col string
	if err := row.Scan(&table, &col); err != nil {
		if err == sql.ErrNoRows {
			return 0, false, nil
		}
		return 0, false, err
	}
	v, present, err := TableAutoIncrement(ctx, db, table)
	if err != nil {
		return 0, false, err
	}
	if !present {
		return 0, false, nil
	}
	var m sql.NullInt64
	if err := db.QueryRowContext(ctx,
		"SELECT MAX("+QuoteIdent(col)+") FROM "+QuoteIdent(table)).Scan(&m); err != nil {
		return 0, false, err
	}
	next := int64(1)
	if m.Valid {
		next = m.Int64 + 1
	}
	if v < next {
		gap = 0
	} else {
		gap = v - next
	}
	return gap, true, nil
}

// TableEngine returns the table's storage engine from information_schema
// (empty when the backend reports none).
func TableEngine(ctx context.Context, db *sql.DB, table string) (string, error) {
	var e string
	err := db.QueryRowContext(ctx, `
		SELECT ENGINE FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table).Scan(&e)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return e, nil
}
