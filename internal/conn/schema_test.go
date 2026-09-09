package conn

import "testing"

// TestKeySequenceUnique pins the explicit --key resolution (P0-1/P1-1):
// the key is unique only on an EXACT ordered match of the primary key or
// of a unique index whose columns are all NOT NULL. A prefix of a
// composite unique index, a different order, a nullable member, an extra
// column, or a plain column are all non-unique — a filtered row-level
// sync must refuse them, and the engine must replace groups instead of
// updating rows.
func TestKeySequenceUnique(t *testing.T) {
	pk := []string{"id"}
	uniques := [][]string{{"code", "region"}, {"email"}}
	nullable := map[string]bool{"email": true, "region": false, "code": false, "id": false}

	cases := []struct {
		name string
		key  []string
		want bool
	}{
		{"primary key", []string{"id"}, true},
		{"primary key case-insensitive", []string{"ID"}, true},
		{"unique not-null index, exact order", []string{"code", "region"}, true},
		{"unique index case-insensitive", []string{"Code", "REGION"}, true},
		{"prefix of composite unique index", []string{"code"}, false},
		{"composite order swapped", []string{"region", "code"}, false},
		{"unique index with nullable member", []string{"email"}, false},
		{"extra column beyond the index", []string{"id", "x"}, false},
		{"shorter than the index", []string{"id", "x"}[:1], true}, // = pk, sanity
		{"plain column", []string{"name"}, false},
		{"empty key", []string{}, false},
	}
	for _, c := range cases {
		if got := keySequenceUnique(pk, uniques, nullable, c.key); got != c.want {
			t.Errorf("%s: keySequenceUnique(%v) = %v, want %v", c.name, c.key, got, c.want)
		}
	}

	// no primary key, only a nullable unique index: nothing is unique
	if keySequenceUnique(nil, [][]string{{"email"}}, nullable, []string{"email"}) {
		t.Error("a unique index on a nullable column is not a unique row address")
	}
	// no indexes at all: nothing is unique
	if keySequenceUnique(nil, nil, nullable, []string{"id"}) {
		t.Error("a table with no unique index has no unique key")
	}
}

// TestGeneratedColumn pins the EXTRA-based generated-column detection
// (P0-2): both storage spellings are recognized, a plain column and an
// expression DEFAULT ("DEFAULT_GENERATED") are not.
func TestGeneratedColumn(t *testing.T) {
	cases := []struct {
		extra     string
		generated bool
		storage   string
	}{
		{"VIRTUAL GENERATED", true, "VIRTUAL"},
		{"STORED GENERATED", true, "STORED"},
		{"auto_increment", false, ""},
		{"DEFAULT_GENERATED", false, ""},
		{"on update CURRENT_TIMESTAMP", false, ""},
		{"", false, ""},
	}
	for _, c := range cases {
		g, s := generatedColumn(c.extra)
		if g != c.generated || s != c.storage {
			t.Errorf("generatedColumn(%q) = (%v, %q), want (%v, %q)", c.extra, g, s, c.generated, c.storage)
		}
	}
}

// TestAccumulateUniqueConstraints is the regression for the zero-value
// cursor: a table whose STATISTICS holds a SINGLE unique index used to
// index the (empty) accumulated slice on its first row and panic. It
// also pins the folding rules: functional parts skipped, non-unique
// indexes ignored, columns in SEQ order, PRIMARY kept first.
func TestAccumulateUniqueConstraints(t *testing.T) {
	// The exact panic shape: one row, the primary key.
	single := []statRow{{name: "PRIMARY", nonUnique: "0", seq: 1, col: "id", colValid: true}}
	got := accumulateUniqueConstraints(single)
	if len(got) != 1 || got[0].Name != "PRIMARY" || len(got[0].Cols) != 1 || got[0].Cols[0] != "id" {
		t.Fatalf("single PK: got %+v", got)
	}

	// No unique index at all: only a plain secondary index.
	none := []statRow{
		{name: "idx_v", nonUnique: "1", seq: 1, col: "v", colValid: true},
	}
	if got := accumulateUniqueConstraints(none); len(got) != 0 {
		t.Fatalf("no unique: got %+v", got)
	}

	// Empty input (no STATISTICS rows) must not panic.
	if got := accumulateUniqueConstraints(nil); len(got) != 0 {
		t.Fatalf("empty: got %+v", got)
	}

	// Composite PK + a unique index + a functional part + a non-unique
	// index, out of name order to exercise the cursor reset between
	// constraints.
	mixed := []statRow{
		{name: "PRIMARY", nonUnique: "0", seq: 1, col: "a", colValid: true},
		{name: "PRIMARY", nonUnique: "0", seq: 2, col: "b", colValid: true},
		{name: "func_idx", nonUnique: "0", seq: 1, col: "", colValid: false}, // functional
		{name: "uk_e", nonUnique: "0", seq: 1, col: "e", colValid: true},
		{name: "plain", nonUnique: "1", seq: 1, col: "z", colValid: true},
	}
	got = accumulateUniqueConstraints(mixed)
	if len(got) != 2 {
		t.Fatalf("mixed: want 2 constraints, got %d: %+v", len(got), got)
	}
	// sort puts PRIMARY first
	if got[0].Name != "PRIMARY" {
		t.Fatalf("PRIMARY must sort first: %+v", got)
	}
	if len(got[0].Cols) != 2 || got[0].Cols[0] != "a" || got[0].Cols[1] != "b" {
		t.Fatalf("PK cols: got %v", got[0].Cols)
	}
	rest := got[1]
	if rest.Name != "uk_e" || len(rest.Cols) != 1 || rest.Cols[0] != "e" {
		t.Fatalf("uk_e: got %+v", rest)
	}
}
