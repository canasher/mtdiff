package chunk

import (
	"database/sql/driver"
	"math"
	"strings"
	"testing"
	"time"

	"mtdiff/internal/conn"
)

func TestIntBoundaries(t *testing.T) {
	cs := intBoundaries(0, 99, 10, false)
	if len(cs) != 10 {
		t.Fatalf("got %d chunks, want 10", len(cs))
	}
	if !cs[0].LoIncl || cs[0].Lo[0] != int64(0) || cs[0].Hi[0] != int64(9) {
		t.Errorf("first chunk wrong: %+v", cs[0])
	}
	for i := 1; i < len(cs); i++ {
		if cs[i].LoIncl {
			t.Errorf("chunk %d must have exclusive lower bound", i)
		}
		// exclusive lower bound equals the previous chunk's inclusive upper
		if got, want := cs[i].Lo[0], cs[i-1].Hi[0]; got != want {
			t.Errorf("gap/overlap between %d and %d: %+v / %+v", i-1, i, cs[i-1], cs[i])
		}
	}
	if last := cs[len(cs)-1].Hi[0]; last != int64(99) {
		t.Errorf("last chunk must end at 99, got %d", last)
	}

	// single value
	cs = intBoundaries(5, 5, 10, false)
	if len(cs) != 1 || cs[0].Lo[0] != int64(5) || cs[0].Hi[0] != int64(5) || !cs[0].LoIncl {
		t.Errorf("single value: %+v", cs)
	}

	// range not divisible by n: step = ceil(10/4) = 3 -> [0,2] (2,5] (5,8] (8,9]
	cs = intBoundaries(0, 9, 4, false)
	if len(cs) != 4 {
		t.Fatalf("got %d chunks, want 4", len(cs))
	}
	if cs[2].Lo[0] != int64(5) || cs[2].Hi[0] != int64(8) || cs[3].Lo[0] != int64(8) || cs[3].Hi[0] != int64(9) {
		t.Errorf("uneven split wrong: %+v", cs)
	}

	// n larger than range: no empty chunks
	cs = intBoundaries(10, 12, 100, false)
	if len(cs) != 3 {
		t.Errorf("expected 3 non-empty chunks, got %d", len(cs))
	}
}

// assertCover checks that the chunks form an exact, ordered partition of
// [lo, hi]: first lower bound inclusive at lo, no gap, no overlap, and the
// last upper bound is exactly hi (the regression target of the
// divisible-span off-by-one: the max key used to fall between chunks).
func assertCover(t *testing.T, lo, hi, n int64, cs []Chunk) {
	t.Helper()
	if len(cs) == 0 || len(cs) > int(n) {
		t.Fatalf("intBoundaries(%d, %d, %d): %d chunks", lo, hi, n, len(cs))
	}
	if !cs[0].LoIncl || cs[0].Lo[0] != lo {
		t.Fatalf("first chunk must be [%d, ...]: %+v", lo, cs[0])
	}
	for i := 1; i < len(cs); i++ {
		if cs[i].LoIncl {
			t.Errorf("chunk %d must have exclusive lower bound: %+v", i, cs[i])
		}
		if cs[i].Lo[0] != cs[i-1].Hi[0] {
			t.Errorf("gap/overlap between %d and %d: %+v / %+v", i-1, i, cs[i-1], cs[i])
		}
	}
	if last := cs[len(cs)-1].Hi[0]; last != hi {
		t.Errorf("last chunk must end at %d, got %d (max key unscanned)", hi, last)
	}
}

// TestIntBoundariesDivisible covers the divisible-span cases where the old
// ceil((hi-lo)/n) step left the maximum key value out of every chunk.
func TestIntBoundariesDivisible(t *testing.T) {
	// 7 values in 3 chunks (hi-lo=6, 6%3==0): old formula gave [1,2] (2,4] (4,6]
	cs := intBoundaries(1, 7, 3, false)
	assertCover(t, 1, 7, 3, cs)
	if len(cs) != 3 {
		t.Fatalf("want 3 chunks, got %d: %+v", len(cs), cs)
	}
	if cs[0].Lo[0] != int64(1) || cs[0].Hi[0] != int64(3) {
		t.Errorf("first chunk wrong: %+v", cs[0])
	}

	// 100 values (0..99) in 11 chunks (span 99 = 9x11): old formula ended at 98
	cs = intBoundaries(0, 99, 11, false)
	assertCover(t, 0, 99, 11, cs)
	if len(cs) != 10 {
		t.Fatalf("want 10 non-empty chunks, got %d: %+v", len(cs), cs)
	}

	// 90001 values (1..90001) in 10 chunks (span 90000 = 9000x10):
	// the plan's reproduction shape; row 90001 used to be unscanned.
	cs = intBoundaries(1, 90001, 10, false)
	assertCover(t, 1, 90001, 10, cs)
	if len(cs) != 10 {
		t.Fatalf("want 10 chunks, got %d", len(cs))
	}
	if cs[0].Hi[0] != int64(9001) {
		t.Errorf("first chunk must end at 9001, got %d", cs[0].Hi[0])
	}
}

// TestIntBoundariesOverflow covers P1: a BIGINT span wider than MaxInt64
// values (the old "hi-lo+n" overflowed int64). The split must still be an
// exact partition, including across the signed boundary.
func TestIntBoundariesOverflow(t *testing.T) {
	// (driver.Value is an interface: comparisons must go through typed
	// variables, or constant conversions change the dynamic type)
	maxInt64 := int64(math.MaxInt64)
	minInt64 := int64(math.MinInt64)
	halfSpan := int64(4611686018427387903)    // 2^62-1
	quarterSpan := int64(4611686018427387904) // 2^62

	// 0..MaxInt64: 2^63 values in 2 chunks (span 2^63, step 2^62)
	cs := intBoundaries(0, maxInt64, 2, false)
	assertCover(t, 0, maxInt64, 2, cs)
	if len(cs) != 2 {
		t.Fatalf("want 2 chunks, got %d: %+v", len(cs), cs)
	}
	if cs[0].Hi[0] != halfSpan {
		t.Errorf("first chunk must end at 2^62-1, got %v", cs[0].Hi[0])
	}
	if last := cs[1].Hi[0]; last != maxInt64 {
		t.Errorf("last chunk must end at MaxInt64, got %v (max key unscanned)", last)
	}

	// MinInt64..0: 2^63 values across the signed boundary
	cs = intBoundaries(minInt64, int64(0), 2, false)
	assertCover(t, minInt64, int64(0), 2, cs)
	if len(cs) != 2 {
		t.Fatalf("want 2 chunks, got %d: %+v", len(cs), cs)
	}
	if cs[0].Hi[0] != -quarterSpan {
		t.Errorf("first chunk must end at -2^62, got %v", cs[0].Hi[0])
	}
	if last := cs[len(cs)-1].Hi[0]; last != int64(0) {
		t.Errorf("last chunk must end at 0, got %v", last)
	}

	// a small negative range (the old formula's negative hi-lo case)
	cs = intBoundaries(-100, -90, 3, false)
	assertCover(t, -100, -90, 3, cs)
	if len(cs) != 3 {
		t.Fatalf("want 3 chunks, got %d: %+v", len(cs), cs)
	}

	// the old overflow case: span MaxInt64+6 (hi-lo overflows int64) —
	// spanSafe must refuse it so the planner falls back to sampling
	if spanSafe(-5, math.MaxInt64) {
		t.Error("a negative lo with a range wider than MaxInt64 must be unsafe")
	}
	if spanSafe(math.MinInt64, math.MaxInt64) {
		t.Error("the full int64 span (2^64 values) must be unsafe")
	}
	if !spanSafe(0, math.MaxInt64) {
		t.Error("0..MaxInt64 (span 2^63) must be safe")
	}
	if !spanSafe(-5, math.MaxInt64-10) {
		t.Error("a negative lo with a narrow range must be safe")
	}
}

// TestPlanIntOverflowGuard pins the planner-level guard: out-of-int64 or
// overflow-wide spans fall back to sampling instead of arithmetic.
func TestPlanIntOverflowGuard(t *testing.T) {
	p := &Planner{KeyCols: []string{"id"}, KeyFamilies: []string{conn.FamINT}}
	cases := []struct {
		name   string
		minV   []driver.Value
		maxV   []driver.Value
		wantOK bool
	}{
		{"wide signed span", []driver.Value{int64(-5)}, []driver.Value{int64(math.MaxInt64)}, false},
		{"full int64 range", []driver.Value{int64(math.MinInt64)}, []driver.Value{int64(math.MaxInt64)}, false},
		{"0..MaxInt64", []driver.Value{int64(0)}, []driver.Value{int64(math.MaxInt64)}, true},
		{"ordinary span", []driver.Value{int64(3)}, []driver.Value{int64(300)}, true},
		{"uint64 past MaxInt64", []driver.Value{uint64(math.MaxUint64)}, []driver.Value{uint64(math.MaxUint64)}, false},
	}
	for _, c := range cases {
		_, ok := p.planInt(c.minV, c.maxV, 4)
		if ok != c.wantOK {
			t.Errorf("%s: planInt ok = %v, want %v", c.name, ok, c.wantOK)
		}
	}
}

// TestIntBoundariesLeadPrefix covers the P3-#15 case: a composite key
// arithmetically split on its integer lead column. The span 30000 (1..30001)
// is divisible by the chunk count (4): the exact shape where an off-by-one
// step would strand the maximum lead value, and every bound must carry the
// lead-column-only prefix markers.
func TestIntBoundariesLeadPrefix(t *testing.T) {
	cs := intBoundaries(1, 30001, 4, true)
	assertCover(t, 1, 30001, 4, cs)
	if len(cs) != 4 {
		t.Fatalf("want 4 chunks, got %d: %+v", len(cs), cs)
	}
	for i, c := range cs {
		if c.LoPrefix != 1 || c.HiPrefix != 1 {
			t.Errorf("chunk %d must be lead-column-only (prefix 1): %+v", i, c)
		}
		// lead bounds hold exactly one value (the lead), not a key vector
		if len(c.Lo) != 1 || len(c.Hi) != 1 {
			t.Errorf("chunk %d bounds must be single lead values: %+v", i, c)
		}
	}
	if cs[0].Hi[0] != int64(7501) {
		t.Errorf("first chunk must end at 7501, got %d", cs[0].Hi[0])
	}
	if last := cs[3].Hi[0]; last != int64(30001) {
		t.Errorf("last chunk must end at 30001, got %d (max lead unscanned)", last)
	}
}

// TestPredicateLeadPrefix covers the P3-#15 rendering: a lead-column-only
// bound is a plain column comparison on the lead column (no lexicographic
// expansion), and the bound display shows just the lead value.
func TestPredicateLeadPrefix(t *testing.T) {
	c := Chunk{
		ID: 1,
		Lo: []driver.Value{int64(7501)}, LoIncl: false, LoPrefix: 1,
		Hi: []driver.Value{int64(15002)}, HiPrefix: 1,
	}
	if got := c.Pred([]string{"a", "b"}, ""); got.SQL != "`a` > ? AND `a` <= ?" || !sameArgs(got.Args, int64(7501), int64(15002)) {
		t.Errorf("lead-prefix predicate = %q args %v", got.SQL, got.Args)
	}
	c.Lo, c.LoIncl = []driver.Value{int64(1)}, true
	if got := c.Pred([]string{"a", "b"}, ""); got.SQL != "`a` >= ? AND `a` <= ?" || !sameArgs(got.Args, int64(1), int64(15002)) {
		t.Errorf("inclusive lead-prefix predicate = %q args %v", got.SQL, got.Args)
	}
	if got, want := c.RenderBound(true), "1"; got != want {
		t.Errorf("bound display must show the lead value only: %q, want %q", got, want)
	}
	// a prefix equal to the key width is not a prefix: full lexicographic
	c2 := Chunk{
		Lo: []driver.Value{int64(1), int64(2)}, LoIncl: true, LoPrefix: 2,
	}
	if got := c2.Pred([]string{"a", "b"}, ""); got.SQL != "(`a` > ? OR (`a` = ? AND `b` >= ?))" || !sameArgs(got.Args, int64(1), int64(1), int64(2)) {
		t.Errorf("full-width prefix must stay lexicographic: %q args %v", got.SQL, got.Args)
	}
}

// sameArgs reports whether the bound arguments equal want, in order.
func sameArgs(got []any, want ...any) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if !valueEqual(got[i], want[i]) {
			return false
		}
	}
	return true
}

// TestKeyOrder covers the latent composite-extremes bug: the sort
// direction must be repeated on every key column. A bare trailing "DESC"
// binds to the last column only, so "a, b DESC" returns the minimum a
// (with its maximum b) as the "last" row — collapsing a composite key's
// whole range to a single row on both sides (silent false "identical").
func TestKeyOrder(t *testing.T) {
	if got, want := keyOrder([]string{"`a`"}, "DESC"), "`a` DESC"; got != want {
		t.Errorf("single column: %q, want %q", got, want)
	}
	if got, want := keyOrder([]string{"`a`", "`b`"}, "DESC"), "`a` DESC, `b` DESC"; got != want {
		t.Errorf("composite must repeat DESC on every column: %q, want %q", got, want)
	}
	if got, want := keyOrder([]string{"`a`", "`b`", "`c`"}, "ASC"), "`a` ASC, `b` ASC, `c` ASC"; got != want {
		t.Errorf("composite ASC: %q, want %q", got, want)
	}
}

// TestToDriverValues pins the keyRow/sample conversion: nil must pass
// through as nil (an all-NULL key row), driver values keep their dynamic
// type.
func TestToDriverValues(t *testing.T) {
	got := toDriverValues([]any{nil, int64(5), "x", []byte("b")})
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4", len(got))
	}
	if got[0] != nil {
		t.Errorf("nil must stay nil, got %#v", got[0])
	}
	if v, ok := got[1].(int64); !ok || v != 5 {
		t.Errorf("int64(5): got %#v", got[1])
	}
	if v, ok := got[2].(string); !ok || v != "x" {
		t.Errorf(`"x": got %#v`, got[2])
	}
	if v, ok := got[3].([]byte); !ok || len(v) != 1 || v[0] != 'b' {
		t.Errorf(`[]byte("b"): got %#v`, got[3])
	}
}

func TestPredicate(t *testing.T) {
	c := Chunk{ID: 1, Lo: []driver.Value{int64(5)}, Hi: []driver.Value{int64(10)}}
	if got := c.Pred([]string{"id"}, ""); got.SQL != "`id` > ? AND `id` <= ?" || !sameArgs(got.Args, int64(5), int64(10)) {
		t.Errorf("predicate = %q args %v", got.SQL, got.Args)
	}
	c.LoIncl = true
	if got := c.Pred([]string{"id"}, ""); got.SQL != "`id` >= ? AND `id` <= ?" || !sameArgs(got.Args, int64(5), int64(10)) {
		t.Errorf("predicate = %q args %v", got.SQL, got.Args)
	}
	// the operator's --where stays a RAW fragment, ANDed in after the
	// parameterized key bounds (never itself parameterized)
	if got := c.Pred([]string{"id"}, "status = 1"); got.SQL != "`id` >= ? AND `id` <= ? AND (status = 1)" || !sameArgs(got.Args, int64(5), int64(10)) {
		t.Errorf("predicate with where = %q args %v", got.SQL, got.Args)
	}
	// no bounds at all (keyless whole-table chunk)
	if got := (Chunk{ID: 0}).Pred(nil, ""); got.SQL != "" || got.Args != nil {
		t.Errorf("empty predicate = %q args %v", got.SQL, got.Args)
	}
	// composite exclusive lower bound / inclusive upper bound: the bound
	// value is bound TWICE in the lower term (k > v OR (k = v AND ...))
	cc := Chunk{
		Lo: []driver.Value{"a", int64(2)},
		Hi: []driver.Value{"b", int64(3)},
	}
	got := cc.Pred([]string{"a", "b"}, "")
	wantSQL := "(`a` > ? OR (`a` = ? AND `b` > ?)) AND (`a` < ? OR (`a` = ? AND `b` <= ?))"
	if got.SQL != wantSQL || !sameArgs(got.Args, "a", "a", int64(2), "b", "b", int64(3)) {
		t.Errorf("composite predicate = %q args %v, want %q", got.SQL, got.Args, wantSQL)
	}
}

// TestPredicateStringKey pins P0-1: a character key value with backslashes
// and quotes never enters the SQL text — it is a bound argument, so the
// statement is byte-exact under any sql_mode, including
// NO_BACKSLASH_ESCAPES (where a rendered literal would change meaning).
func TestPredicateStringKey(t *testing.T) {
	c := Chunk{ID: 1, Lo: []driver.Value{`a\b`}, LoIncl: true, Hi: []driver.Value{`C:\abc\def'`}}
	got := c.Pred([]string{"k"}, "")
	if strings.Contains(got.SQL, `a\b`) || strings.Contains(got.SQL, `def`) || strings.Contains(got.SQL, "'") {
		t.Errorf("the key value must never appear in the SQL text: %q", got.SQL)
	}
	if got.SQL != "`k` >= ? AND `k` <= ?" {
		t.Errorf("predicate = %q, want two placeholders", got.SQL)
	}
	if !sameArgs(got.Args, `a\b`, `C:\abc\def'`) {
		t.Errorf("args = %v, want the raw values verbatim", got.Args)
	}
}

// TestPredicateNULLBounds covers the NULL-bound rendering for explicit keys
// on nullable columns: NULL key rows sort first in MySQL order, and plain
// comparisons (k >= NULL) would silently exclude them from every chunk.
func TestPredicateNULLBounds(t *testing.T) {
	// single nullable column, first chunk [NULL .. 5]: covers the NULL rows
	// plus every non-NULL value at or below 5
	c := Chunk{ID: 0, Lo: []driver.Value{nil}, LoIncl: true, Hi: []driver.Value{int64(5)}}
	if got := c.Pred([]string{"a"}, ""); got.SQL != "(`a` IS NULL OR `a` <= ?)" || !sameArgs(got.Args, int64(5)) {
		t.Errorf("NULL-min single column = %q args %v", got.SQL, got.Args)
	}
	// whole key space NULL (all values NULL): no predicate
	c = Chunk{ID: 0, Lo: []driver.Value{nil}, LoIncl: true, Hi: []driver.Value{nil}}
	if got := c.Pred([]string{"a"}, ""); got.SQL != "" {
		t.Errorf("all-NULL key must have empty predicate, got %q", got.SQL)
	}
	// exclusive NULL lower bound (sample landed on a NULL row): strictly
	// greater than NULL means exactly the non-NULL rows
	c = Chunk{ID: 1, Lo: []driver.Value{nil}, Hi: []driver.Value{int64(5)}}
	if got := c.Pred([]string{"a"}, ""); got.SQL != "`a` IS NOT NULL AND `a` <= ?" || !sameArgs(got.Args, int64(5)) {
		t.Errorf("exclusive NULL-min single column = %q args %v", got.SQL, got.Args)
	}
	c = Chunk{ID: 1, Lo: []driver.Value{nil}}
	if got := c.Pred([]string{"a"}, ""); got.SQL != "`a` IS NOT NULL" {
		t.Errorf("exclusive NULL-min, no upper bound = %q", got.SQL)
	}
	// composite lower bound with a NULL leading column: (a IS NULL AND b >= 2)
	// or any non-NULL a; upper bound plain
	c = Chunk{
		Lo: []driver.Value{nil, int64(2)}, LoIncl: true,
		Hi: []driver.Value{int64(5), int64(3)},
	}
	got := c.Pred([]string{"a", "b"}, "")
	wantSQL := "((`a` IS NULL AND `b` >= ?) OR `a` IS NOT NULL) AND (`a` < ? OR (`a` = ? AND `b` <= ?))"
	if got.SQL != wantSQL || !sameArgs(got.Args, int64(2), int64(5), int64(5), int64(3)) {
		t.Errorf("composite NULL lower bound = %q args %v, want %q", got.SQL, got.Args, wantSQL)
	}
	// composite bound with a NULL last column, inclusive lower: the
	// all-NULL row is the minimum of the suffix, so the suffix term is a
	// tautology; exclusive lower must still exclude the bound row itself
	c = Chunk{Lo: []driver.Value{int64(1), nil}, LoIncl: true, Hi: []driver.Value{int64(2), int64(3)}}
	if got := c.Pred([]string{"a", "b"}, ""); got.SQL != "(`a` > ? OR (`a` = ? AND 1=1)) AND (`a` < ? OR (`a` = ? AND `b` <= ?))" {
		t.Errorf("inclusive NULL last column = %q", got.SQL)
	} else if !sameArgs(got.Args, int64(1), int64(1), int64(2), int64(2), int64(3)) {
		t.Errorf("inclusive NULL last column args = %v", got.Args)
	}
	c.LoIncl = false
	if got := c.Pred([]string{"a", "b"}, ""); got.SQL != "(`a` > ? OR (`a` = ? AND `b` IS NOT NULL)) AND (`a` < ? OR (`a` = ? AND `b` <= ?))" {
		t.Errorf("exclusive NULL last column = %q", got.SQL)
	}
	// NULL upper bound component (only when the leading value is NULL too):
	// (a IS NULL AND b <= 2); a non-NULL a is always greater
	c = Chunk{Lo: []driver.Value{nil, int64(1)}, LoIncl: true, Hi: []driver.Value{nil, int64(2)}}
	got = c.Pred([]string{"a", "b"}, "")
	wantSQL = "((`a` IS NULL AND `b` >= ?) OR `a` IS NOT NULL) AND (`a` IS NULL AND `b` <= ?)"
	if got.SQL != wantSQL || !sameArgs(got.Args, int64(1), int64(2)) {
		t.Errorf("NULL upper bound component = %q args %v, want %q", got.SQL, got.Args, wantSQL)
	}
}

func TestLiteral(t *testing.T) {
	tests := []struct {
		v    driver.Value
		want string
	}{
		{nil, "NULL"},
		{int64(-42), "-42"},
		{uint64(18446744073709551615), "18446744073709551615"},
		{float64(1.5), "1.5"},
		{float32(2.25), "2.25"},
		{true, "1"},
		{false, "0"},
		{"it's", `'it''s'`},
		{`a\b`, `'a\\b'`},
		{[]byte{0xDE, 0xAD}, "X'dead'"},
		{time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC), `'2026-08-31 23:59:59'`},
		// DATETIME(6) precision must survive the literal (sync writes and
		// chunk bounds go through this rendering)
		{time.Date(2026, 8, 31, 23, 59, 59, 123456000, time.UTC), `'2026-08-31 23:59:59.123456'`},
	}
	for _, tt := range tests {
		if got := Literal(tt.v); got != tt.want {
			t.Errorf("Literal(%v) = %q, want %q", tt.v, got, tt.want)
		}
	}
}

func TestValuesEqual(t *testing.T) {
	if !valuesEqual([]driver.Value{int64(1), "a"}, []driver.Value{int64(1), "a"}) {
		t.Error("equal values must be equal")
	}
	if valuesEqual([]driver.Value{int64(1)}, []driver.Value{int64(2)}) {
		t.Error("different values must differ")
	}
	if valuesEqual([]driver.Value{int64(1)}, nil) {
		t.Error("length mismatch must differ")
	}
}

// TestRenderLessThan pins the strict "key < bound" rendering (the out-of-
// range delete's low side). Strictness is load-bearing: a row EQUAL to the
// bound is in range and must not be selected, and a NULL component of the
// key sorts below the bound and must be selected (a plain "col < lit" is
// UNKNOWN for it).
func TestRenderLessThan(t *testing.T) {
	tests := []struct {
		key      []string
		bound    []driver.Value
		want     string
		wantArgs []any
	}{
		{
			key:      []string{"a"},
			bound:    []driver.Value{int64(5)},
			want:     "(`a` IS NULL OR `a` < ?)",
			wantArgs: []any{int64(5)},
		},
		{
			// all-NULL single-column bound: the minimum, nothing below it
			key:   []string{"a"},
			bound: []driver.Value{nil},
			want:  "1=0",
		},
		{
			key:      []string{"a", "b"},
			bound:    []driver.Value{int64(1), int64(2)},
			want:     "(`a` IS NULL OR `a` < ?) OR (`a` = ? AND (`b` IS NULL OR `b` < ?))",
			wantArgs: []any{int64(1), int64(1), int64(2)},
		},
		{
			// NULL leading bound component: only NULL rows continue into
			// the (strict) suffix
			key:      []string{"a", "b"},
			bound:    []driver.Value{nil, int64(2)},
			want:     "`a` IS NULL AND (`b` IS NULL OR `b` < ?)",
			wantArgs: []any{int64(2)},
		},
		{
			// NULL last bound component: a row equal in the prefix with a
			// NULL last component EQUALS the bound — the suffix is empty
			key:      []string{"a", "b"},
			bound:    []driver.Value{int64(1), nil},
			want:     "(`a` IS NULL OR `a` < ?) OR (`a` = ? AND 1=0)",
			wantArgs: []any{int64(1), int64(1)},
		},
		{
			// all-NULL composite bound: the all-NULL row is the bound
			key:   []string{"a", "b"},
			bound: []driver.Value{nil, nil},
			want:  "`a` IS NULL AND 1=0",
		},
		{
			// a string bound travels as bound data (P0-1): the value must
			// never appear in the SQL text, and the arg is the raw value
			key:      []string{"a"},
			bound:    []driver.Value{"it's \\ 中文"},
			want:     "(`a` IS NULL OR `a` < ?)",
			wantArgs: []any{"it's \\ 中文"},
		},
	}
	for _, tt := range tests {
		got := LessThan(tt.key, tt.bound)
		if got.SQL != tt.want {
			t.Errorf("LessThan(%v, %v) = %q, want %q", tt.key, tt.bound, got.SQL, tt.want)
		}
		if tt.wantArgs != nil && !sameArgs(got.Args, tt.wantArgs...) {
			t.Errorf("LessThan(%v, %v) args = %v, want %v", tt.key, tt.bound, got.Args, tt.wantArgs)
		}
	}
}

// TestRenderGreaterThan pins the strict "key > bound" rendering (the
// out-of-range delete's high side).
func TestRenderGreaterThan(t *testing.T) {
	tests := []struct {
		key      []string
		bound    []driver.Value
		want     string
		wantArgs []any
	}{
		{
			key:      []string{"a"},
			bound:    []driver.Value{int64(5)},
			want:     "`a` > ?",
			wantArgs: []any{int64(5)},
		},
		{
			// all-NULL bound: every non-NULL row sits above the minimum
			key:   []string{"a"},
			bound: []driver.Value{nil},
			want:  "`a` IS NOT NULL",
		},
		{
			key:      []string{"a", "b"},
			bound:    []driver.Value{int64(1), int64(2)},
			want:     "`a` > ? OR (`a` = ? AND `b` > ?)",
			wantArgs: []any{int64(1), int64(1), int64(2)},
		},
		{
			key:      []string{"a", "b"},
			bound:    []driver.Value{nil, int64(2)},
			want:     "(`a` IS NULL AND `b` > ?) OR `a` IS NOT NULL",
			wantArgs: []any{int64(2)},
		},
		{
			// NULL last bound component: the equal-prefix all-NULL row is
			// the bound itself and must stay out
			key:      []string{"a", "b"},
			bound:    []driver.Value{int64(1), nil},
			want:     "`a` > ? OR (`a` = ? AND `b` IS NOT NULL)",
			wantArgs: []any{int64(1), int64(1)},
		},
		{
			// all-NULL composite bound: only the all-NULL row is excluded
			key:   []string{"a", "b"},
			bound: []driver.Value{nil, nil},
			want:  "(`a` IS NULL AND `b` IS NOT NULL) OR `a` IS NOT NULL",
		},
	}
	for _, tt := range tests {
		got := GreaterThan(tt.key, tt.bound)
		if got.SQL != tt.want {
			t.Errorf("GreaterThan(%v, %v) = %q, want %q", tt.key, tt.bound, got.SQL, tt.want)
		}
		if tt.wantArgs != nil && !sameArgs(got.Args, tt.wantArgs...) {
			t.Errorf("GreaterThan(%v, %v) args = %v, want %v", tt.key, tt.bound, got.Args, tt.wantArgs)
		}
	}
}
