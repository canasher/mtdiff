package chunk

import (
	"database/sql/driver"
	"testing"
)

func TestIntBoundaries(t *testing.T) {
	cs := intBoundaries(0, 99, 10)
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
	cs = intBoundaries(5, 5, 10)
	if len(cs) != 1 || cs[0].Lo[0] != int64(5) || cs[0].Hi[0] != int64(5) || !cs[0].LoIncl {
		t.Errorf("single value: %+v", cs)
	}

	// range not divisible by n: step = ceil(10/4) = 3 -> [0,2] (2,5] (5,8] (8,9]
	cs = intBoundaries(0, 9, 4)
	if len(cs) != 4 {
		t.Fatalf("got %d chunks, want 4", len(cs))
	}
	if cs[2].Lo[0] != int64(5) || cs[2].Hi[0] != int64(8) || cs[3].Lo[0] != int64(8) || cs[3].Hi[0] != int64(9) {
		t.Errorf("uneven split wrong: %+v", cs)
	}

	// n larger than range: no empty chunks
	cs = intBoundaries(10, 12, 100)
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
	cs := intBoundaries(1, 7, 3)
	assertCover(t, 1, 7, 3, cs)
	if len(cs) != 3 {
		t.Fatalf("want 3 chunks, got %d: %+v", len(cs), cs)
	}
	if cs[0].Lo[0] != int64(1) || cs[0].Hi[0] != int64(3) {
		t.Errorf("first chunk wrong: %+v", cs[0])
	}

	// 100 values (0..99) in 11 chunks (span 99 = 9x11): old formula ended at 98
	cs = intBoundaries(0, 99, 11)
	assertCover(t, 0, 99, 11, cs)
	if len(cs) != 10 {
		t.Fatalf("want 10 non-empty chunks, got %d: %+v", len(cs), cs)
	}

	// 90001 values (1..90001) in 10 chunks (span 90000 = 9000x10):
	// the plan's reproduction shape; row 90001 used to be unscanned.
	cs = intBoundaries(1, 90001, 10)
	assertCover(t, 1, 90001, 10, cs)
	if len(cs) != 10 {
		t.Fatalf("want 10 chunks, got %d", len(cs))
	}
	if cs[0].Hi[0] != int64(9001) {
		t.Errorf("first chunk must end at 9001, got %d", cs[0].Hi[0])
	}
}

func TestPredicate(t *testing.T) {
	c := Chunk{ID: 1, Lo: []driver.Value{int64(5)}, Hi: []driver.Value{int64(10)}}
	if got := c.Predicate([]string{"id"}, ""); got != "`id` > 5 AND `id` <= 10" {
		t.Errorf("predicate = %q", got)
	}
	c.LoIncl = true
	if got := c.Predicate([]string{"id"}, ""); got != "`id` >= 5 AND `id` <= 10" {
		t.Errorf("predicate = %q", got)
	}
	if got := c.Predicate([]string{"id"}, "status = 1"); got != "`id` >= 5 AND `id` <= 10 AND (status = 1)" {
		t.Errorf("predicate with where = %q", got)
	}
	// no bounds at all (keyless whole-table chunk)
	if got := (Chunk{ID: 0}).Predicate(nil, ""); got != "" {
		t.Errorf("empty predicate = %q", got)
	}
	// composite exclusive lower bound / inclusive upper bound
	cc := Chunk{
		Lo: []driver.Value{"a", int64(2)},
		Hi: []driver.Value{"b", int64(3)},
	}
	want := "(`a` > 'a' OR (`a` = 'a' AND `b` > 2)) AND (`a` < 'b' OR (`a` = 'b' AND `b` <= 3))"
	if got := cc.Predicate([]string{"a", "b"}, ""); got != want {
		t.Errorf("composite predicate = %q, want %q", got, want)
	}
}

// TestPredicateNULLBounds covers the NULL-bound rendering for explicit keys
// on nullable columns: NULL key rows sort first in MySQL order, and plain
// comparisons (k >= NULL) would silently exclude them from every chunk.
func TestPredicateNULLBounds(t *testing.T) {
	// single nullable column, first chunk [NULL .. 5]: covers the NULL rows
	// plus every non-NULL value at or below 5
	c := Chunk{ID: 0, Lo: []driver.Value{nil}, LoIncl: true, Hi: []driver.Value{int64(5)}}
	if got := c.Predicate([]string{"a"}, ""); got != "(`a` IS NULL OR `a` <= 5)" {
		t.Errorf("NULL-min single column = %q", got)
	}
	// whole key space NULL (all values NULL): no predicate
	c = Chunk{ID: 0, Lo: []driver.Value{nil}, LoIncl: true, Hi: []driver.Value{nil}}
	if got := c.Predicate([]string{"a"}, ""); got != "" {
		t.Errorf("all-NULL key must have empty predicate, got %q", got)
	}
	// exclusive NULL lower bound (sample landed on a NULL row): strictly
	// greater than NULL means exactly the non-NULL rows
	c = Chunk{ID: 1, Lo: []driver.Value{nil}, Hi: []driver.Value{int64(5)}}
	if got := c.Predicate([]string{"a"}, ""); got != "`a` IS NOT NULL AND `a` <= 5" {
		t.Errorf("exclusive NULL-min single column = %q", got)
	}
	c = Chunk{ID: 1, Lo: []driver.Value{nil}}
	if got := c.Predicate([]string{"a"}, ""); got != "`a` IS NOT NULL" {
		t.Errorf("exclusive NULL-min, no upper bound = %q", got)
	}
	// composite lower bound with a NULL leading column: (a IS NULL AND b >= 2)
	// or any non-NULL a; upper bound plain
	c = Chunk{
		Lo: []driver.Value{nil, int64(2)}, LoIncl: true,
		Hi: []driver.Value{int64(5), int64(3)},
	}
	want := "((`a` IS NULL AND `b` >= 2) OR `a` IS NOT NULL) AND (`a` < 5 OR (`a` = 5 AND `b` <= 3))"
	if got := c.Predicate([]string{"a", "b"}, ""); got != want {
		t.Errorf("composite NULL lower bound = %q, want %q", got, want)
	}
	// composite bound with a NULL last column, inclusive lower: the
	// all-NULL row is the minimum of the suffix, so the suffix term is a
	// tautology; exclusive lower must still exclude the bound row itself
	c = Chunk{Lo: []driver.Value{int64(1), nil}, LoIncl: true, Hi: []driver.Value{int64(2), int64(3)}}
	if got := c.Predicate([]string{"a", "b"}, ""); got != "(`a` > 1 OR (`a` = 1 AND 1=1)) AND (`a` < 2 OR (`a` = 2 AND `b` <= 3))" {
		t.Errorf("inclusive NULL last column = %q", got)
	}
	c.LoIncl = false
	if got := c.Predicate([]string{"a", "b"}, ""); got != "(`a` > 1 OR (`a` = 1 AND `b` IS NOT NULL)) AND (`a` < 2 OR (`a` = 2 AND `b` <= 3))" {
		t.Errorf("exclusive NULL last column = %q", got)
	}
	// NULL upper bound component (only when the leading value is NULL too):
	// (a IS NULL AND b <= 2); a non-NULL a is always greater
	c = Chunk{Lo: []driver.Value{nil, int64(1)}, LoIncl: true, Hi: []driver.Value{nil, int64(2)}}
	want = "((`a` IS NULL AND `b` >= 1) OR `a` IS NOT NULL) AND (`a` IS NULL AND `b` <= 2)"
	if got := c.Predicate([]string{"a", "b"}, ""); got != want {
		t.Errorf("NULL upper bound component = %q, want %q", got, want)
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
		{true, "1"},
		{"it's", `'it''s'`},
		{`a\b`, `'a\\b'`},
		{[]byte{0xDE, 0xAD}, "X'dead'"},
	}
	for _, tt := range tests {
		if got := literal(tt.v); got != tt.want {
			t.Errorf("literal(%v) = %q, want %q", tt.v, got, tt.want)
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
