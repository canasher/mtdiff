package conn

import (
	"context"
	"database/sql"
	"fmt"
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
