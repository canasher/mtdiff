package conn

import (
	"strings"
	"testing"
)

func strCol(name, rawType, collation string) Column {
	return Column{Name: name, Family: FamSTR, RawType: rawType, Collation: collation}
}

func intCol(name string) Column {
	return Column{Name: name, Family: FamINT, RawType: "int"}
}

func keySchema(key []string, cols ...Column) *Schema {
	return &Schema{Key: key, Cols: cols}
}

func TestKeyOrderCompatible(t *testing.T) {
	cases := []struct {
		name    string
		a, b    *Schema
		want    bool
		wantSub string // the reason must name the cause
	}{
		{
			"same explicit collation",
			keySchema([]string{"k"}, strCol("k", "varchar(16)", "utf8mb4_general_ci")),
			keySchema([]string{"k"}, strCol("k", "varchar(32)", "utf8mb4_general_ci")),
			true, "",
		},
		{
			"collation names compare case-insensitively",
			keySchema([]string{"k"}, strCol("k", "varchar(16)", "UTF8MB4_BIN")),
			keySchema([]string{"k"}, strCol("k", "varchar(16)", "utf8mb4_bin")),
			true, "",
		},
		{
			"bin vs general_ci: the spec's false-identical pair",
			keySchema([]string{"k"}, strCol("k", "varchar(16)", "utf8mb4_bin")),
			keySchema([]string{"k"}, strCol("k", "varchar(16)", "utf8mb4_general_ci")),
			false, "collation",
		},
		{
			"backend defaults differ (8.0 vs 5.7): not comparable as equal",
			keySchema([]string{"k"}, strCol("k", "varchar(16)", "utf8mb4_0900_ai_ci")),
			keySchema([]string{"k"}, strCol("k", "varchar(16)", "utf8mb4_general_ci")),
			false, "collation",
		},
		{
			"explicit collation vs backend default (one side empty)",
			keySchema([]string{"k"}, strCol("k", "varchar(16)", "utf8mb4_bin")),
			keySchema([]string{"k"}, strCol("k", "varchar(16)", "")),
			false, "default",
		},
		{
			"both empty, byte-ordered (CHAR BINARY): compatible",
			keySchema([]string{"k"}, strCol("k", "char(16) binary", "")),
			keySchema([]string{"k"}, strCol("k", "char(16) binary", "")),
			true, "",
		},
		{
			"both empty, not byte-ordered: incompatible",
			keySchema([]string{"k"}, strCol("k", "varchar(16)", "")),
			keySchema([]string{"k"}, strCol("k", "varchar(16)", "")),
			false, "collation",
		},
		{
			"family drift (INT vs VARCHAR)",
			keySchema([]string{"k"}, intCol("k")),
			keySchema([]string{"k"}, strCol("k", "varchar(16)", "utf8mb4_bin")),
			false, "family",
		},
		{
			"int keys need no collation",
			keySchema([]string{"k"}, intCol("k")),
			keySchema([]string{"k"}, intCol("k")),
			true, "",
		},
		{
			"composite: second component collation differs",
			keySchema([]string{"a", "b"}, intCol("a"), strCol("b", "varchar(16)", "utf8mb4_bin")),
			keySchema([]string{"a", "b"}, intCol("a"), strCol("b", "varchar(16)", "utf8mb4_general_ci")),
			false, "b",
		},
		{
			"composite: crossed order is a name mismatch",
			keySchema([]string{"a", "b"}, intCol("a"), intCol("b")),
			keySchema([]string{"b", "a"}, intCol("a"), intCol("b")),
			false, "differs",
		},
		{
			"key column count differs",
			keySchema([]string{"a"}, intCol("a")),
			keySchema([]string{"a", "b"}, intCol("a"), intCol("b")),
			false, "count",
		},
	}
	for _, tc := range cases {
		ok, why := KeyOrderCompatible(tc.a, tc.b)
		if ok != tc.want {
			t.Errorf("%s: KeyOrderCompatible = %v, want %v (reason: %s)", tc.name, ok, tc.want, why)
		}
		if tc.wantSub != "" && !strings.Contains(why, tc.wantSub) {
			t.Errorf("%s: reason %q must name %q", tc.name, why, tc.wantSub)
		}
	}
}
