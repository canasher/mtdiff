package sync

import (
	"strings"
	"testing"

	"mtdiff/internal/chunk"
	"mtdiff/internal/conn"
)

// TestHoldersQueryArgOrder pins the t_mut regression: the holders query's
// out-of-range flag column sits in the SELECT list, so its placeholders
// appear TEXTUALLY BEFORE the WHERE clause — and the driver binds
// positionally in order of appearance. The flag's args must therefore
// LEAD the argument list, with the tuple args following in term order.
// The pre-fix code appended the flag args last: the bindings shifted,
// the WHERE addressed the extreme values instead of the written tuples,
// and a harmless three-row update table was refused as a "cross-chunk
// unique swap" (false positive) — or worse, on other shapes, the check
// could have missed a real holder (false negative).
func TestHoldersQueryArgOrder(t *testing.T) {
	s := &conn.Schema{
		Table: "t",
		Cols: []conn.Column{
			{Name: "id", Family: conn.FamINT, RawType: "int"},
			{Name: "v", Family: conn.FamSTR, RawType: "varchar(10)"},
		},
		Key:         []string{"id"},
		KeySource:   "primary",
		KeyIsUnique: true,
	}
	c := uniqueConstraint{cols: []string{"id"}, srcIdx: []int{0}, dstIdx: []int{0}}
	colIdents := []string{conn.QuoteIdent("id")}
	tuples := map[string][]any{
		"0\x1f101": {int64(101)},
		"0\x1f102": {int64(102)},
		"0\x1f103": {int64(103)},
	}
	keys := []string{"0\x1f101", "0\x1f102", "0\x1f103"}
	// the out-of-range flag: (id < 1 OR id > 1000) — two bound extremes
	oorFlag := chunk.Pred{SQL: "(`id` IS NULL OR `id` < ?) OR `id` > ?", Args: []any{int64(1), int64(1000)}}

	query, args := holdersQuery(s, c, colIdents, tuples, keys, 0, 3, oorFlag, true)
	if !strings.Contains(query, "WHERE") || !strings.Contains(query, "SELECT") {
		t.Fatalf("malformed query: %s", query)
	}
	// exactly one placeholder per bound arg
	if n := strings.Count(query, "?"); n != len(args) {
		t.Fatalf("placeholders = %d, args = %d: %s", n, len(args), query)
	}
	// the FLAG args lead (their ?s are in the SELECT list, before the
	// WHERE), the tuple args follow in sorted term order
	want := []any{int64(1), int64(1000), int64(101), int64(102), int64(103)}
	if len(args) != len(want) {
		t.Fatalf("args = %#v, want %d entries", args, len(want))
	}
	for i, w := range want {
		if args[i] != w {
			t.Errorf("args[%d] = %#v, want %#v (order: flag args first, then tuple args)", i, args[i], w)
		}
	}

	// without the flag (the pass is off) the constant 0 column is
	// selected and only the tuple args are bound
	query, args = holdersQuery(s, c, colIdents, tuples, keys, 0, 3, chunk.Pred{}, false)
	if strings.Contains(query, "?") && len(args) != 3 {
		t.Errorf("no-flag: args = %#v, want the 3 tuple args only", args)
	}
	if len(args) != 3 {
		t.Fatalf("no-flag args = %#v, want 3", args)
	}
}

// TestHoldersQueryCompositeFlagOrder extends the arg-order contract to a
// composite constraint: the flag args still lead, and each tuple's
// components follow the constraint's column order.
func TestHoldersQueryCompositeFlagOrder(t *testing.T) {
	s := &conn.Schema{
		Table: "t",
		Cols: []conn.Column{
			{Name: "k1", Family: conn.FamSTR, RawType: "varchar(10)"},
			{Name: "k2", Family: conn.FamINT, RawType: "int"},
			{Name: "v", Family: conn.FamINT, RawType: "int"},
		},
		Key:         []string{"k1", "k2"},
		KeySource:   "explicit",
		KeyIsUnique: false,
	}
	c := uniqueConstraint{cols: []string{"k2", "k1"}, srcIdx: []int{1, 0}, dstIdx: []int{1, 0}}
	colIdents := []string{conn.QuoteIdent("k2"), conn.QuoteIdent("k1")}
	tuples := map[string][]any{"0\x1fa1": {int64(7), "x"}}
	keys := []string{"0\x1fa1"}
	oorFlag := chunk.Pred{SQL: "`k1` > ?", Args: []any{"abc"}}

	query, args := holdersQuery(s, c, colIdents, tuples, keys, 0, 1, oorFlag, true)
	if n := strings.Count(query, "?"); n != len(args) {
		t.Fatalf("placeholders = %d, args = %d: %s", n, len(args), query)
	}
	want := []any{"abc", int64(7), "x"} // flag arg, then the tuple's components in constraint order
	if len(args) != len(want) {
		t.Fatalf("args = %#v, want %d entries", args, len(want))
	}
	for i, w := range want {
		if args[i] != w {
			t.Errorf("args[%d] = %#v, want %#v", i, args[i], w)
		}
	}
}
