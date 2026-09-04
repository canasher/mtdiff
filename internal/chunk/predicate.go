package chunk

import (
	"database/sql/driver"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"mtdiff/internal/normalize"
)

// Chunk is a key range to scan. The predicate is
//
//	(k >= Lo | k > Lo) AND k <= Hi
//
// with a nil Lo meaning "no lower bound" (first chunk) and a nil Hi meaning
// "no upper bound" (last chunk / whole table). Composite bounds are expanded
// into OR/AND terms (no row-constructor comparisons, for compatibility with
// MySQL 5.7 and compatible layers).
//
// LoPrefix/HiPrefix: when set to a count in (0, len(keyCols)), the bound
// constrains only the first that many key columns as plain column
// comparisons (no lexicographic expansion). Used for composite keys split
// arithmetically on an integer lead column, where each row's lead value
// pins it to exactly one chunk, so the trailing columns need no terms.
type Chunk struct {
	ID       int
	Lo       []driver.Value
	LoIncl   bool
	Hi       []driver.Value
	LoPrefix int
	HiPrefix int
}

// Pred is one parameterized SQL predicate: a SQL fragment with a "?"
// placeholder for every system-generated key-bound value, plus the bound
// arguments, ready for QueryContext / QueryRowContext (P0-1). The values
// travel as DATA on the server (the driver's binary protocol,
// interpolateParams off), so a string key holding backslashes, quotes or
// Chinese text is byte-exact under any sql_mode — including
// NO_BACKSLASH_ESCAPES, where rendering the value as a SQL literal would
// change the statement's meaning and move the chunk boundary. The
// operator's --where fragment, if ANDed in, stays a RAW SQL fragment by
// design (it is operator-supplied SQL, never parameterized).
type Pred struct {
	SQL  string
	Args []any
}

// RenderBound renders one key boundary for display: "-" when unbounded,
// otherwise the literal key values joined by commas.
func (c Chunk) RenderBound(lo bool) string {
	vs := c.Lo
	if !lo {
		vs = c.Hi
	}
	if vs == nil {
		return "-"
	}
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = Literal(v)
	}
	return strings.Join(parts, ",")
}

// Pred renders the chunk's WHERE clause as a parameterized predicate for
// the given key columns. extraWhere (an optional --where filter) is ANDed
// in verbatim.
//
// NULL key values (an explicit --key on a nullable column) sort before any
// value in MySQL order. A NULL lower bound therefore means "all NULL rows
// plus every non-NULL value at or below the upper bound" (inclusive) or
// "every non-NULL value at or below the upper bound" (exclusive) — plain
// comparisons cannot express that (k >= NULL is always UNKNOWN), so it is
// rendered with explicit IS NULL terms. A NULL upper bound (only when every
// key value is NULL) needs no term at all.
func (c Chunk) Pred(keyCols []string, extraWhere string) Pred {
	var parts []string
	var args []any
	if c.Lo != nil && len(keyCols) == 1 && c.Lo[0] == nil {
		k := ident(keyCols[0])
		switch {
		case c.Hi != nil && c.Hi[0] != nil && c.LoIncl:
			// NULL minimum (first chunk): NULL rows plus values <= Hi.
			parts = append(parts, fmt.Sprintf("(%s IS NULL OR %s <= ?)", k, k))
			args = append(args, c.Hi[0])
		case c.Hi != nil && c.Hi[0] != nil:
			// Exclusive: NULL is the minimum, so strictly-greater rows are
			// exactly the non-NULL rows.
			parts = append(parts, fmt.Sprintf("%s IS NOT NULL AND %s <= ?", k, k))
			args = append(args, c.Hi[0])
		case !c.LoIncl:
			// No upper bound: only the non-NULL rows.
			parts = append(parts, fmt.Sprintf("%s IS NOT NULL", k))
		}
	} else {
		if c.Lo != nil {
			op := ">"
			if c.LoIncl {
				op = ">="
			}
			switch {
			case c.LoPrefix > 0 && c.LoPrefix < len(keyCols):
				// Lead-column-only bound (composite key split on its
				// integer lead): plain column comparisons, each lead
				// column independently, no lexicographic expansion.
				for i := 0; i < c.LoPrefix; i++ {
					parts = append(parts, fmt.Sprintf("%s %s ?", ident(keyCols[i]), op))
					args = append(args, c.Lo[i])
				}
			case len(keyCols) == 1:
				parts = append(parts, fmt.Sprintf("%s %s ?", ident(keyCols[0]), op))
				args = append(args, c.Lo[0])
			default:
				s, a := rowCompare(keyCols, c.Lo, op)
				parts = append(parts, "("+s+")")
				args = append(args, a...)
			}
		}
		if c.Hi != nil {
			switch {
			case c.HiPrefix > 0 && c.HiPrefix < len(keyCols):
				for i := 0; i < c.HiPrefix; i++ {
					parts = append(parts, fmt.Sprintf("%s <= ?", ident(keyCols[i])))
					args = append(args, c.Hi[i])
				}
			case len(keyCols) == 1 && c.Hi[0] == nil:
				// All keys NULL: no effective upper bound.
			case len(keyCols) == 1:
				parts = append(parts, fmt.Sprintf("%s <= ?", ident(keyCols[0])))
				args = append(args, c.Hi[0])
			default:
				s, a := rowCompare(keyCols, c.Hi, "<=")
				parts = append(parts, "("+s+")")
				args = append(args, a...)
			}
		}
	}
	if extraWhere != "" {
		parts = append(parts, "("+extraWhere+")")
	}
	return Pred{SQL: strings.Join(parts, " AND "), Args: args}
}

// rowCompare expands a lexicographic row comparison into column terms:
//
//	lower bound (op ">" / ">="):  k1 > v1 OR (k1 = v1 AND k2 > v2 OR (...))
//	upper bound (op "<" / "<="):  k1 < w1 OR (k1 = w1 AND k2 < w2 OR (...))
//
// Inner columns always use the strict operator on the bound's side; only the
// last column uses the bound's own operator.
//
// NULL bound components (nullable key columns) get their own terms: MySQL
// orders NULLs before any value, so with the equal prefix a NULL lower bound
// means "this column is NULL and the suffix is at/above the bound, or any
// non-NULL value", and a NULL upper bound means "this column is NULL and
// the suffix is at/below". Plain comparisons cannot express that: k > NULL
// is always UNKNOWN, which would silently exclude every row.
//
// Every bound value becomes a "?" bound on the server side (P0-1); the
// returned args are in the exact order the placeholders appear in the SQL.
func rowCompare(cols []string, vals []driver.Value, op string) (string, []any) {
	lower := op == ">" || op == ">="
	var term func(i int) (string, []any)
	term = func(i int) (string, []any) {
		col := ident(cols[i])
		v := vals[i]
		last := i == len(cols)-1
		if v == nil {
			switch {
			case lower && op == ">=" && last:
				// NULL is the minimum: with the equal prefix every row
				// (NULL or not) is at or above the bound.
				return "1=1", nil
			case lower && op == ">" && last:
				// Exclusive bound: the all-NULL row is the bound itself
				// and must stay out.
				return col + " IS NOT NULL", nil
			case lower:
				s, a := term(i + 1)
				return fmt.Sprintf("(%s IS NULL AND %s) OR %s IS NOT NULL", col, s, col), a
			case last:
				return col + " IS NULL", nil
			default:
				s, a := term(i + 1)
				return fmt.Sprintf("%s IS NULL AND %s", col, s), a
			}
		}
		if last {
			return fmt.Sprintf("%s %s ?", col, op), []any{v}
		}
		strict := ">"
		if !lower {
			strict = "<"
		}
		s, a := term(i + 1)
		return fmt.Sprintf("%s %s ? OR (%s = ? AND %s)", col, strict, col, s), append([]any{v, v}, a...)
	}
	return term(0)
}

// LessThan renders the NULL-safe lexicographic "key < bound" predicate for
// a (possibly composite) key: every row strictly below the bound in MySQL
// key order (NULLs sort first). bound has one component per key column. A
// single-column all-NULL bound renders "1=0": the all-NULL row is the
// minimum, nothing sits below it.
func LessThan(keyCols []string, bound []driver.Value) Pred {
	s, a := strictCompare(keyCols, bound, true)
	return Pred{SQL: s, Args: a}
}

// GreaterThan renders the NULL-safe "key > bound" predicate, the strict
// complement of LessThan: every row strictly above the bound. A
// single-column all-NULL bound renders "col IS NOT NULL" (every non-NULL
// row sits above the all-NULL minimum); a composite all-NULL bound
// excludes only the all-NULL row itself.
func GreaterThan(keyCols []string, bound []driver.Value) Pred {
	s, a := strictCompare(keyCols, bound, false)
	return Pred{SQL: s, Args: a}
}

// strictCompare expands a STRICT lexicographic row comparison into column
// terms, the shape Chunk.Pred cannot express (a chunk is a closed interval;
// "outside [min, max]" is the disjunction of two open tails):
//
//	"key < bound":  k1 < w1 OR (k1 = w1 AND k2 < w2 OR (...))
//	"key > bound":  k1 > v1 OR (k1 = v1 AND k2 > v2 OR (...))
//
// Inner columns always use the strict operator; only the last column may
// meet the bound. It is a sibling of rowCompare, not a reuse of it: the
// inclusive upper-side NULL branches (k IS NULL) would include the bound
// row itself, which a strict comparison must exclude.
//
// NULL bound components (nullable key columns) get their own terms: MySQL
// orders NULLs before any value, so a NULL lower component keeps only the
// NULL rows (plus the suffix), and a NULL upper component keeps only the
// NULL rows of the equal prefix (the all-NULL row being the bound itself).
//
// Every bound value becomes a "?" bound on the server side (P0-1); the
// returned args are in the exact order the placeholders appear in the SQL.
func strictCompare(cols []string, bound []driver.Value, less bool) (string, []any) {
	var term func(i int) (string, []any)
	term = func(i int) (string, []any) {
		col := ident(cols[i])
		v := bound[i]
		last := i == len(cols)-1
		if v == nil {
			switch {
			case less && last:
				// the all-NULL (prefix) row IS the bound: nothing is
				// strictly below it.
				return "1=0", nil
			case less:
				// with the equal prefix only NULL rows continue into
				// the suffix (they sort below the bound's value there).
				s, a := term(i + 1)
				return fmt.Sprintf("%s IS NULL AND %s", col, s), a
			case last:
				// the all-NULL (prefix) row is the bound itself: every
				// non-NULL row sits above it.
				return col + " IS NOT NULL", nil
			default:
				s, a := term(i + 1)
				return fmt.Sprintf("(%s IS NULL AND %s) OR %s IS NOT NULL", col, s, col), a
			}
		}
		if less {
			// NULL rows sort below the bound value; a plain "col < ?"
			// is UNKNOWN for them and would silently miss them.
			if last {
				return fmt.Sprintf("(%s IS NULL OR %s < ?)", col, col), []any{v}
			}
			s, a := term(i + 1)
			return fmt.Sprintf("(%s IS NULL OR %s < ?) OR (%s = ? AND %s)", col, col, col, s), append([]any{v, v}, a...)
		}
		if last {
			return fmt.Sprintf("%s > ?", col), []any{v}
		}
		s, a := term(i + 1)
		return fmt.Sprintf("%s > ? OR (%s = ? AND %s)", col, col, s), append([]any{v, v}, a...)
	}
	return term(0)
}

// ident backtick-quotes an identifier.
func ident(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// Literal renders a driver value as a SQL literal for DISPLAY only (chunk
// bounds in reports, sample SQL). Executable predicates never render
// values into SQL text — they bind them (Pred) — so a display rendering
// that disagrees with the server's literal semantics (e.g. under
// NO_BACKSLASH_ESCAPES) can only mislead a reader, never move data.
func Literal(v driver.Value) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(t, 10)
	case uint64:
		return strconv.FormatUint(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'g', -1, 32)
	case bool:
		if t {
			return "1"
		}
		return "0"
	case string:
		return quoteString(t)
	case []byte:
		return "X'" + hex.EncodeToString(t) + "'"
	case time.Time:
		return quoteString(normalize.FormatDateTime(t))
	}
	return "NULL"
}

// quoteString renders a character value as a quoted literal (display). It
// assumes the server escapes backslashes (the default): with
// NO_BACKSLASH_ESCAPES in sql_mode a displayed literal would read
// differently than it would execute — which is exactly why executable
// predicates bind values instead of rendering them (Pred).
func quoteString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `''`)
	return "'" + s + "'"
}

// valuesEqual reports whether two key value vectors are equal (same type,
// same value).
func valuesEqual(a, b []driver.Value) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !valueEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func valueEqual(a, b driver.Value) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case int64:
		y, ok := b.(int64)
		return ok && x == y
	case uint64:
		y, ok := b.(uint64)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []byte:
		y, ok := b.([]byte)
		return ok && string(x) == string(y)
	case time.Time:
		y, ok := b.(time.Time)
		return ok && x.Equal(y)
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}
