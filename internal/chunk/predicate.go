package chunk

import (
	"database/sql/driver"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
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
		parts[i] = literal(v)
	}
	return strings.Join(parts, ",")
}

// Predicate renders the chunk's WHERE clause for the given key columns.
// extraWhere (an optional --where filter) is ANDed in.
//
// NULL key values (an explicit --key on a nullable column) sort before any
// value in MySQL order. A NULL lower bound therefore means "all NULL rows
// plus every non-NULL value at or below the upper bound" (inclusive) or
// "every non-NULL value at or below the upper bound" (exclusive) — plain
// comparisons cannot express that (k >= NULL is always UNKNOWN), so it is
// rendered with explicit IS NULL terms. A NULL upper bound (only when every
// key value is NULL) needs no term at all.
func (c Chunk) Predicate(keyCols []string, extraWhere string) string {
	var parts []string
	if c.Lo != nil && len(keyCols) == 1 && c.Lo[0] == nil {
		k := ident(keyCols[0])
		switch {
		case c.Hi != nil && c.Hi[0] != nil && c.LoIncl:
			// NULL minimum (first chunk): NULL rows plus values <= Hi.
			parts = append(parts, fmt.Sprintf("(%s IS NULL OR %s <= %s)", k, k, literal(c.Hi[0])))
		case c.Hi != nil && c.Hi[0] != nil:
			// Exclusive: NULL is the minimum, so strictly-greater rows are
			// exactly the non-NULL rows.
			parts = append(parts, fmt.Sprintf("%s IS NOT NULL AND %s <= %s", k, k, literal(c.Hi[0])))
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
					parts = append(parts, fmt.Sprintf("%s %s %s", ident(keyCols[i]), op, literal(c.Lo[i])))
				}
			case len(keyCols) == 1:
				parts = append(parts, fmt.Sprintf("%s %s %s", ident(keyCols[0]), op, literal(c.Lo[0])))
			default:
				parts = append(parts, "("+rowCompare(keyCols, c.Lo, op)+")")
			}
		}
		if c.Hi != nil {
			switch {
			case c.HiPrefix > 0 && c.HiPrefix < len(keyCols):
				for i := 0; i < c.HiPrefix; i++ {
					parts = append(parts, fmt.Sprintf("%s <= %s", ident(keyCols[i]), literal(c.Hi[i])))
				}
			case len(keyCols) == 1 && c.Hi[0] == nil:
				// All keys NULL: no effective upper bound.
			case len(keyCols) == 1:
				parts = append(parts, fmt.Sprintf("%s <= %s", ident(keyCols[0]), literal(c.Hi[0])))
			default:
				parts = append(parts, "("+rowCompare(keyCols, c.Hi, "<=")+")")
			}
		}
	}
	if extraWhere != "" {
		parts = append(parts, "("+extraWhere+")")
	}
	return strings.Join(parts, " AND ")
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
func rowCompare(cols []string, vals []driver.Value, op string) string {
	lower := op == ">" || op == ">="
	var term func(i int) string
	term = func(i int) string {
		col := ident(cols[i])
		v := vals[i]
		last := i == len(cols)-1
		if v == nil {
			switch {
			case lower && op == ">=" && last:
				// NULL is the minimum: with the equal prefix every row
				// (NULL or not) is at or above the bound.
				return "1=1"
			case lower && op == ">" && last:
				// Exclusive bound: the all-NULL row is the bound itself
				// and must stay out.
				return fmt.Sprintf("%s IS NOT NULL", col)
			case lower:
				return fmt.Sprintf("(%s IS NULL AND %s) OR %s IS NOT NULL", col, term(i+1), col)
			case last:
				return fmt.Sprintf("%s IS NULL", col)
			default:
				return fmt.Sprintf("%s IS NULL AND %s", col, term(i+1))
			}
		}
		lit := literal(v)
		if last {
			return fmt.Sprintf("%s %s %s", col, op, lit)
		}
		strict := ">"
		if !lower {
			strict = "<"
		}
		return fmt.Sprintf("%s %s %s OR (%s = %s AND %s)", col, strict, lit, col, lit, term(i+1))
	}
	return term(0)
}

// ident backtick-quotes an identifier.
func ident(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// literal renders a driver value as a SQL literal.
func literal(v driver.Value) string {
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
		return quoteString(t.Format("2006-01-02 15:04:05.999"))
	}
	return "NULL"
}

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
