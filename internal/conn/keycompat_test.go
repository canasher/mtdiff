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

func enumCol(name, rawType string) Column {
	return Column{Name: name, Family: FamENUM, RawType: rawType}
}

func setCol(name, rawType string) Column {
	return Column{Name: name, Family: FamSET, RawType: rawType}
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

// TestKeyOrderCompatibleEnumSet pins the P0-2 fix: ENUM/SET keys order
// by the member list's DEFINITION ORDER, not by the value — the old
// blanket "same family, not a string, therefore compatible" allowed
// ENUM('b','a') vs ENUM('a','b') (reversed orderings on the same data),
// and shared key bounds would address the wrong rows. The gate now
// requires EXACTLY equal member definitions (same members, same order);
// anything else — or a definition it cannot parse — fails closed.
func TestKeyOrderCompatibleEnumSet(t *testing.T) {
	cases := []struct {
		name    string
		a, b    *Schema
		want    bool
		wantSub string
	}{
		{
			"ENUM member order reversed: incompatible",
			keySchema([]string{"k"}, enumCol("k", "enum('b','a')")),
			keySchema([]string{"k"}, enumCol("k", "enum('a','b')")),
			false, "member definitions",
		},
		{
			"ENUM identical definitions: compatible",
			keySchema([]string{"k"}, enumCol("k", "enum('b','a')")),
			keySchema([]string{"k"}, enumCol("k", "enum('b','a')")),
			true, "",
		},
		{
			"ENUM type-name case differs, members identical: compatible",
			keySchema([]string{"k"}, enumCol("k", "ENUM('a','b')")),
			keySchema([]string{"k"}, enumCol("k", "enum('a','b')")),
			true, "",
		},
		{
			"ENUM member case differs: incompatible (a different definition)",
			keySchema([]string{"k"}, enumCol("k", "enum('A','b')")),
			keySchema([]string{"k"}, enumCol("k", "enum('a','b')")),
			false, "member definitions",
		},
		{
			"ENUM one member added: incompatible",
			keySchema([]string{"k"}, enumCol("k", "enum('a','b')")),
			keySchema([]string{"k"}, enumCol("k", "enum('a','b','c')")),
			false, "member definitions",
		},
		{
			"SET member order reversed: incompatible",
			keySchema([]string{"k"}, setCol("k", "set('a','b')")),
			keySchema([]string{"k"}, setCol("k", "set('b','a')")),
			false, "member definitions",
		},
		{
			"SET identical definitions: compatible",
			keySchema([]string{"k"}, setCol("k", "set('a','b')")),
			keySchema([]string{"k"}, setCol("k", "set('a','b')")),
			true, "",
		},
		{
			"ENUM escaped quote inside a member: parsed, compared unescaped",
			keySchema([]string{"k"}, enumCol("k", "enum('a\\'b','c')")),
			keySchema([]string{"k"}, enumCol("k", "enum('a\\'b','c')")),
			true, "",
		},
		{
			"ENUM escaped vs unescaped member: incompatible",
			keySchema([]string{"k"}, enumCol("k", "enum('a\\'b')")),
			keySchema([]string{"k"}, enumCol("k", "enum('ab')")),
			false, "member definitions",
		},
		{
			"ENUM unparseable definition: fail closed",
			keySchema([]string{"k"}, enumCol("k", "enum('a','b'")),
			keySchema([]string{"k"}, enumCol("k", "enum('a','b')")),
			false, "member definitions",
		},
		{
			"ENUM vs non-ENUM text on one side: family gate fires",
			keySchema([]string{"k"}, enumCol("k", "enum('a','b')")),
			keySchema([]string{"k"}, strCol("k", "varchar(16)", "utf8mb4_bin")),
			false, "family",
		},
		{
			"composite: int + ENUM with reversed members: incompatible",
			keySchema([]string{"a", "k"}, intCol("a"), enumCol("k", "enum('b','a')")),
			keySchema([]string{"a", "k"}, intCol("a"), enumCol("k", "enum('a','b')")),
			false, "member definitions",
		},
		{
			"DECIMAL key: value-ordered, same family: compatible",
			keySchema([]string{"k"}, Column{Name: "k", Family: FamDECIMAL, RawType: "decimal(10,2)"}),
			keySchema([]string{"k"}, Column{Name: "k", Family: FamDECIMAL, RawType: "decimal(20,6)"}),
			true, "",
		},
		{
			"JSON key: not proven compatible, fail closed",
			keySchema([]string{"k"}, Column{Name: "k", Family: FamJSON, RawType: "json"}),
			keySchema([]string{"k"}, Column{Name: "k", Family: FamJSON, RawType: "json"}),
			false, "not proven",
		},
	}
	for i, tc := range cases {
		ok, why := KeyOrderCompatible(tc.a, tc.b)
		if ok != tc.want {
			t.Errorf("case %d (%s): KeyOrderCompatible = %v, want %v (reason: %s)", i, tc.name, ok, tc.want, why)
		}
		if tc.wantSub != "" && !strings.Contains(why, tc.wantSub) {
			t.Errorf("case %d (%s): reason %q must name %q", i, tc.name, why, tc.wantSub)
		}
	}
}

// TestEnumSetMembers pins the member-list parser: definition order is
// preserved, quotes are stripped, the \\' and ” escapes are unescaped,
// and anything that is not the introspection rendering is a refusal.
func TestEnumSetMembers(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		ok   bool
	}{
		{`enum('a','b')`, []string{"a", "b"}, true},
		{`set('x','y','z')`, []string{"x", "y", "z"}, true},
		{`ENUM('A','B')`, []string{"A", "B"}, true}, // member case is kept
		{`enum('a\'b','c')`, []string{"a'b", "c"}, true},
		{`enum('a''b','c')`, []string{"a'b", "c"}, true},
		{`enum('a,b','c')`, []string{"a,b", "c"}, true}, // comma inside a member
		{`enum('')`, []string{""}, true},
		{`enum('a','b'`, nil, false},            // unclosed
		{`enum(a,'b')`, nil, false},             // unquoted member
		{`enum('a', 'b') trailing`, nil, false}, // trailing junk
		{`varchar(16)`, nil, false},             // not an enum/set
		{`enum()`, nil, false},                  // no members
	}
	for _, tc := range cases {
		got, ok := enumSetMembers(tc.in)
		if ok != tc.ok {
			t.Errorf("enumSetMembers(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if tc.ok && !membersEqual(got, tc.want) {
			t.Errorf("enumSetMembers(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestKeyRangeChunkable pins the RANGE-ADDRESSABILITY gate. The crucial
// case is an ENUM/SET key with IDENTICAL definitions on both sides: the
// cross-side check (KeyOrderCompatible) passes — the members agree — yet
// the key is still not range-addressable, because on a SINGLE side the
// ORDER BY order (member definition) and the WHERE comparison order
// (member-name collation) disagree. That is the property the cross-side
// gate cannot see, and the one that made a diverged enum-keyed table
// compare identical (both sides' range chunk selected zero rows).
func TestKeyRangeChunkable(t *testing.T) {
	cases := []struct {
		name    string
		a, b    *Schema
		want    bool
		wantSub string
	}{
		{
			"ENUM key, identical definitions: cross-side OK but NOT range-addressable",
			keySchema([]string{"k"}, enumCol("k", "enum('b','a')")),
			keySchema([]string{"k"}, enumCol("k", "enum('b','a')")),
			false, "ENUM",
		},
		{
			"SET key, identical definitions: NOT range-addressable",
			keySchema([]string{"k"}, setCol("k", "set('a','b')")),
			keySchema([]string{"k"}, setCol("k", "set('a','b')")),
			false, "SET",
		},
		{
			"ENUM member order reversed: NOT range-addressable (also cross-side)",
			keySchema([]string{"k"}, enumCol("k", "enum('b','a')")),
			keySchema([]string{"k"}, enumCol("k", "enum('a','b')")),
			false, "",
		},
		{
			"composite int + ENUM: NOT range-addressable",
			keySchema([]string{"a", "k"}, intCol("a"), enumCol("k", "enum('a','b')")),
			keySchema([]string{"a", "k"}, intCol("a"), enumCol("k", "enum('a','b')")),
			false, "ENUM",
		},
		{
			"int key: range-addressable",
			keySchema([]string{"k"}, intCol("k")),
			keySchema([]string{"k"}, intCol("k")),
			true, "",
		},
		{
			"DECIMAL key: range-addressable",
			keySchema([]string{"k"}, Column{Name: "k", Family: FamDECIMAL, RawType: "decimal(10,2)"}),
			keySchema([]string{"k"}, Column{Name: "k", Family: FamDECIMAL, RawType: "decimal(20,6)"}),
			true, "",
		},
		{
			"string key, same collation: range-addressable",
			keySchema([]string{"k"}, strCol("k", "varchar(16)", "utf8mb4_bin")),
			keySchema([]string{"k"}, strCol("k", "varchar(32)", "utf8mb4_bin")),
			true, "",
		},
		{
			"string key, collations differ: NOT range-addressable (subsumes KeyOrderCompatible)",
			keySchema([]string{"k"}, strCol("k", "varchar(16)", "utf8mb4_bin")),
			keySchema([]string{"k"}, strCol("k", "varchar(16)", "utf8mb4_general_ci")),
			false, "collation",
		},
		{
			"key columns differ in count: NOT range-addressable",
			keySchema([]string{"k"}, intCol("k")),
			keySchema([]string{"k", "j"}, intCol("k"), intCol("j")),
			false, "",
		},
	}
	for i, tc := range cases {
		ok, why := KeyRangeChunkable(tc.a, tc.b)
		if ok != tc.want {
			t.Errorf("case %d (%s): KeyRangeChunkable = %v, want %v (reason: %s)", i, tc.name, ok, tc.want, why)
		}
		if tc.wantSub != "" && !strings.Contains(why, tc.wantSub) {
			t.Errorf("case %d (%s): reason %q must name %q", i, tc.name, why, tc.wantSub)
		}
	}
}
