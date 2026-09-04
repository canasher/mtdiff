package sync

import (
	"reflect"
	"testing"

	"mtdiff/internal/conn"
)

func keysetSchemas(keyCols []string, nullable ...bool) *conn.Schema {
	cols := make([]conn.Column, len(keyCols))
	for i, k := range keyCols {
		fam := conn.FamINT
		raw := "int"
		if i > 0 {
			fam, raw = conn.FamSTR, "varchar(10)"
		}
		cols[i] = conn.Column{Name: k, Family: fam, RawType: raw, Nullable: nullable != nil && nullable[0]}
	}
	return &conn.Schema{Table: "t", Cols: cols, Key: keyCols, KeySource: "explicit", KeyIsUnique: false}
}

// TestKeysetCursor is the spec item 12-14 engine: the keyset
// pagination cursor is a STRICT "key > last row" predicate (no OFFSET —
// a deep OFFSET re-reads the prefix per page) rendered with the same
// lexicographic expansion as the chunk bounds, bound as parameters.
func TestKeysetCursor(t *testing.T) {
	r := &Runner{}
	cases := []struct {
		name    string
		s       *conn.Schema
		cursor  []any
		wantSQL string
		wantArg []any
	}{
		{"single int key", keysetSchemas([]string{"id"}), []any{int64(42)},
			"`id` > ?", []any{int64(42)}},
		{"single varchar key", keysetSchemas([]string{"id"}, true), []any{"abc"},
			"`id` > ?", []any{"abc"}},
		{"composite: lead greater-or-equal expansion",
			keysetSchemas([]string{"k1", "k2"}), []any{"a", int64(3)},
			"`k1` > ? OR (`k1` = ? AND `k2` > ?)", []any{"a", "a", int64(3)}},
		{"composite: NULL lead, non-NULL tail",
			keysetSchemas([]string{"k1", "k2"}), []any{nil, int64(3)},
			"(`k1` IS NULL AND `k2` > ?) OR `k1` IS NOT NULL", []any{int64(3)}},
		{"composite: NULL tail",
			keysetSchemas([]string{"k1", "k2"}), []any{"a", nil},
			"`k1` > ? OR (`k1` = ? AND `k2` IS NOT NULL)", []any{"a", "a"}},
		{"all-NULL single key: every non-NULL row is above it",
			keysetSchemas([]string{"id"}), []any{nil},
			"`id` IS NOT NULL", nil},
		{"all-NULL composite",
			keysetSchemas([]string{"k1", "k2"}), []any{nil, nil},
			"(`k1` IS NULL AND `k2` IS NOT NULL) OR `k1` IS NOT NULL", nil},
	}
	for _, c := range cases {
		p := &prep{dstS: c.s}
		gotSQL, gotArg := r.keysetCursor(p, c.cursor)
		if gotSQL != c.wantSQL {
			t.Errorf("%s: cursor = %q, want %q", c.name, gotSQL, c.wantSQL)
		}
		if !reflect.DeepEqual(gotArg, c.wantArg) {
			t.Errorf("%s: args = %#v, want %#v", c.name, gotArg, c.wantArg)
		}
	}
}
