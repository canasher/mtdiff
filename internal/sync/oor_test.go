package sync

import (
	"database/sql/driver"
	"strings"
	"testing"
	"time"

	"mtdiff/internal/chunk"
	"mtdiff/internal/conn"
)

func TestKeyLits(t *testing.T) {
	lits := keyLits([]string{conn.FamINT, conn.FamUINT, conn.FamSTR, conn.FamBYTES, conn.FamDECIMAL, conn.FamBIT, conn.FamTIME})
	cases := []struct {
		lit  chunk.LiteralFunc
		v    driver.Value
		want string
	}{
		{lits[0], int64(42), "42"},
		{lits[1], uint64(7), "7"},
		// character values arrive as []byte and must be quoted, not hexed
		{lits[2], []byte("a'b"), `'a''b'`},
		// genuine binary stays a hex blob
		{lits[3], []byte{0x01, 0x02}, "X'0102'"},
		// decimal / time arrive as []byte too: quoted, not hexed
		{lits[4], []byte("1.50"), `'1.50'`},
		{lits[5], []byte{0b11000000}, `b'11000000'`},
		{lits[6], []byte("12:34:56"), `'12:34:56'`},
	}
	for i, c := range cases {
		if got := c.lit(c.v); got != c.want {
			t.Errorf("case %d: got %s, want %s", i, got, c.want)
		}
	}
}

func intSchema(key ...string) *conn.Schema {
	cols := make([]conn.Column, len(key))
	for i, k := range key {
		cols[i] = conn.Column{Name: k, Family: conn.FamINT, RawType: "int"}
	}
	return &conn.Schema{Table: "t", Cols: cols, Key: key}
}

func TestOorPredicate(t *testing.T) {
	r := &Runner{}
	cases := []struct {
		name      string
		srcS, dst *conn.Schema
		minV, max []driver.Value
		want      string
		ok        bool
	}{
		{
			name: "single int column",
			srcS: intSchema("id"), dst: intSchema("id"),
			minV: []driver.Value{int64(1)}, max: []driver.Value{int64(100)},
			want: "(`id` IS NULL OR `id` < 1) OR `id` > 100",
			ok:   true,
		},
		{
			name: "all-NULL minimum: only the upper tail can match",
			srcS: intSchema("id"), dst: intSchema("id"),
			minV: []driver.Value{nil}, max: []driver.Value{nil},
			want: "`id` IS NOT NULL",
			ok:   true,
		},
		{
			name: "composite key",
			srcS: intSchema("a", "b"), dst: intSchema("a", "b"),
			minV: []driver.Value{int64(1), int64(2)}, max: []driver.Value{int64(9), int64(9)},
			want: "((`a` IS NULL OR `a` < 1) OR (`a` = 1 AND (`b` IS NULL OR `b` < 2))) OR (`a` > 9 OR (`a` = 9 AND `b` > 9))",
			ok:   true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &prep{srcS: c.srcS, dstS: c.dst}
			got, ok := r.oorPredicate(p, c.minV, c.max)
			if ok != c.ok || got != c.want {
				t.Errorf("got (%q, %v), want (%q, %v)", got, ok, c.want, c.ok)
			}
		})
	}
}

// TestOorPredicateFamilyLiterals pins the family-aware rendering: a
// decimal key must be compared as a quoted string, not a hex blob.
func TestOorPredicateFamilyLiterals(t *testing.T) {
	r := &Runner{}
	srcS := &conn.Schema{
		Table: "t",
		Cols:  []conn.Column{{Name: "k", Family: conn.FamDECIMAL, RawType: "decimal(10,2)"}},
		Key:   []string{"k"},
	}
	dstS := &conn.Schema{
		Table: "d",
		Cols:  []conn.Column{{Name: "k", Family: conn.FamDECIMAL, RawType: "decimal(10,2)"}},
		Key:   []string{"k"},
	}
	got, ok := r.oorPredicate(&prep{srcS: srcS, dstS: dstS},
		[]driver.Value{[]byte("5.00")}, []driver.Value{[]byte("9.99")})
	if !ok {
		t.Fatal("ok = false, want true")
	}
	want := "(`k` IS NULL OR `k` < '5.00') OR `k` > '9.99'"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestKeyAgree pins the out-of-range guard: the src key values are
// rendered against the dst's key columns, so the keys must agree by name
// and order (equal length with a different composition is NOT agreement).
func TestKeyAgree(t *testing.T) {
	s := func(key ...string) *conn.Schema {
		return &conn.Schema{Key: key}
	}
	cases := []struct {
		name string
		a, b *conn.Schema
		want bool
	}{
		{"identical single", s("id"), s("id"), true},
		{"identical composite", s("a", "b"), s("a", "b"), true},
		{"no key on either side", s(), s(), true},
		{"swapped composite", s("a", "b"), s("b", "a"), false},
		{"different name same length", s("id", "region"), s("id", "zone"), false},
		{"different length", s("a", "b"), s("a"), false},
		{"one side keyless", s("id"), s(), false},
	}
	for _, c := range cases {
		if got := keyAgree(c.a, c.b); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestGroupOps(t *testing.T) {
	mk := func(n int) []op {
		ops := make([]op, n)
		for i := range ops {
			ops[i] = op{kind: opDelete, key: []any{int64(i)}}
		}
		return ops
	}

	g := groupOps(mk(5), 2)
	if len(g) != 3 || len(g[0]) != 2 || len(g[1]) != 2 || len(g[2]) != 1 {
		t.Fatalf("sizes = %v, want [2 2 1]", groupSizes(g))
	}
	if g[0][0].key[0] != int64(0) || g[2][0].key[0] != int64(4) {
		t.Errorf("ops not preserved in order: %v / %v", g[0][0].key, g[2][0].key)
	}

	if g := groupOps(nil, 3); g != nil {
		t.Errorf("nil ops must give nil groups, got %v", g)
	}
	if g := groupOps(mk(3), 0); len(g) != 1 || len(g[0]) != 3 {
		t.Errorf("batch 0 must be one group of all, got %v", groupSizes(g))
	}
	if g := groupOps(mk(3), 10); len(g) != 1 || len(g[0]) != 3 {
		t.Errorf("batch above the count must be one group, got %v", groupSizes(g))
	}
}

func groupSizes(g [][]op) []int {
	out := make([]int, len(g))
	for i, o := range g {
		out[i] = len(o)
	}
	return out
}

// TestSamplesOnePerKind pins the dry-run sample selection: each op kind
// present gets one sample before the remaining slots fill in order, so a
// plan dominated by inserts still shows its deletes.
func TestSamplesOnePerKind(t *testing.T) {
	r := &Runner{}
	b := NewBuilder("t", testSchema(t))
	row := func(id int64) []any {
		return []any{id, "n", []byte("1.00"), nil, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
	}
	ins, upd, del := op{kind: opInsert, key: []any{int64(1)}, rows: [][]any{row(1)}},
		op{kind: opUpdate, key: []any{int64(2)}, rows: [][]any{row(2)}},
		op{kind: opDelete, key: []any{int64(3)}}
	chunked := [][]op{
		{ins, upd, del},
		{
			op{kind: opInsert, key: []any{int64(4)}, rows: [][]any{row(4)}},
			op{kind: opInsert, key: []any{int64(5)}, rows: [][]any{row(5)}},
			op{kind: opUpdate, key: []any{int64(6)}, rows: [][]any{row(6)}},
			op{kind: opDelete, key: []any{int64(7)}},
		},
	}

	got := r.samples(b, chunked, 3)
	if len(got) != 3 {
		t.Fatalf("limit 3: got %d samples, want 3", len(got))
	}
	for i, want := range []string{"VALUES (1", "SET", "DELETE"} {
		if !strings.Contains(got[i], want) {
			t.Errorf("limit 3, sample %d = %q, want a sample containing %q", i, got[i], want)
		}
	}
	if !strings.Contains(got[1], "`id` = 2") || !strings.Contains(got[2], "`id` = 3") {
		t.Errorf("limit 3: first samples must be the first insert/update/delete: %v", got)
	}

	got = r.samples(b, chunked, 6)
	if len(got) != 6 {
		t.Fatalf("limit 6: got %d samples, want 6", len(got))
	}
	if !strings.Contains(got[3], "VALUES (4") || !strings.Contains(got[4], "VALUES (5") {
		t.Errorf("limit 6: remaining slots must fill in order: %v", got)
	}

	if got := r.samples(b, chunked, 0); got != nil {
		t.Errorf("limit 0 must render nothing, got %v", got)
	}
}
