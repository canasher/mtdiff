package sync

import (
	"database/sql/driver"
	"reflect"
	"strings"
	"testing"
	"time"

	"mtdiff/internal/conn"
)

// TestLiteralFor pins the DISPLAY rendering of a raw driver value per
// column family. The executable write path no longer renders values into
// SQL text at all (parameterized, P0-3); this path only feeds the
// human-readable report samples and rowBytes budgeting.
func TestLiteralFor(t *testing.T) {
	cases := []struct {
		fam  string
		v    driver.Value
		want string
	}{
		{conn.FamINT, int64(42), "42"},
		{conn.FamUINT, uint64(7), "7"},
		// character values arrive as []byte and must be quoted, not hexed
		{conn.FamSTR, []byte("a'b"), `'a''b'`},
		// genuine binary stays a hex blob
		{conn.FamBYTES, []byte{0x01, 0x02}, "X'0102'"},
		// decimal / time arrive as []byte too: quoted, not hexed
		{conn.FamDECIMAL, []byte("1.50"), `'1.50'`},
		{conn.FamBIT, []byte{0b11000000}, `b'11000000'`},
		{conn.FamTIME, []byte("12:34:56"), `'12:34:56'`},
	}
	for i, c := range cases {
		if got := literalFor(c.fam, c.v); got != c.want {
			t.Errorf("case %d (%s): got %s, want %s", i, c.fam, got, c.want)
		}
	}
}

// sameBoundArgs reports whether the bound arguments equal want, in order.
func sameBoundArgs(got []any, want ...any) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			return false
		}
	}
	return true
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
		wantArgs  []any
		ok        bool
	}{
		{
			name: "single int column",
			srcS: intSchema("id"), dst: intSchema("id"),
			minV: []driver.Value{int64(1)}, max: []driver.Value{int64(100)},
			want: "(`id` IS NULL OR `id` < ?) OR `id` > ?",
			// P0-1: the bound values travel as data, never in the SQL text
			wantArgs: []any{int64(1), int64(100)},
			ok:       true,
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
			want:     "((`a` IS NULL OR `a` < ?) OR (`a` = ? AND (`b` IS NULL OR `b` < ?))) OR (`a` > ? OR (`a` = ? AND `b` > ?))",
			wantArgs: []any{int64(1), int64(1), int64(2), int64(9), int64(9), int64(9)},
			ok:       true,
		},
		{
			name: "string key: values never rendered into SQL",
			srcS: &conn.Schema{
				Table: "t",
				Cols:  []conn.Column{{Name: "k", Family: conn.FamSTR, RawType: "varchar(128)"}},
				Key:   []string{"k"},
			},
			dst: &conn.Schema{
				Table: "d",
				Cols:  []conn.Column{{Name: "k", Family: conn.FamSTR, RawType: "varchar(128)"}},
				Key:   []string{"k"},
			},
			minV: []driver.Value{`a\b'中文`}, max: []driver.Value{`z\\末`},
			want:     "(`k` IS NULL OR `k` < ?) OR `k` > ?",
			wantArgs: []any{`a\b'中文`, `z\\末`},
			ok:       true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &prep{srcS: c.srcS, dstS: c.dst}
			got, ok := r.oorPredicate(p, c.minV, c.max)
			if ok != c.ok || got.SQL != c.want {
				t.Errorf("got (%q args %v, %v), want (%q args %v, %v)",
					got.SQL, got.Args, ok, c.want, c.wantArgs, c.ok)
			}
			if c.wantArgs != nil && !sameBoundArgs(got.Args, c.wantArgs...) {
				t.Errorf("args = %v, want %v", got.Args, c.wantArgs)
			}
		})
	}
}

// TestOorPredicateBoundData pins the parameterized bound handling: a
// decimal key (which arrives as []byte) and a string key are compared via
// bound arguments — the raw bytes never enter the SQL text, so the
// predicate is byte-exact under any sql_mode (NO_BACKSLASH_ESCAPES).
func TestOorPredicateBoundData(t *testing.T) {
	r := &Runner{}
	mk := func(fam, raw, table string) *conn.Schema {
		return &conn.Schema{
			Table: table,
			Cols:  []conn.Column{{Name: "k", Family: fam, RawType: raw}},
			Key:   []string{"k"},
		}
	}
	got, ok := r.oorPredicate(&prep{srcS: mk(conn.FamDECIMAL, "decimal(10,2)", "t"), dstS: mk(conn.FamDECIMAL, "decimal(10,2)", "d")},
		[]driver.Value{[]byte("5.00")}, []driver.Value{[]byte("9.99")})
	if !ok {
		t.Fatal("ok = false, want true")
	}
	want := "(`k` IS NULL OR `k` < ?) OR `k` > ?"
	if got.SQL != want {
		t.Errorf("got %q, want %q", got.SQL, want)
	}
	if !sameBoundArgs(got.Args, []byte("5.00"), []byte("9.99")) {
		t.Errorf("args = %v, want the raw decimal bytes bound", got.Args)
	}
	if strings.Contains(got.SQL, "5.00") || strings.Contains(got.SQL, "9.99") {
		t.Errorf("bound values must not be rendered into the SQL text: %q", got.SQL)
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
