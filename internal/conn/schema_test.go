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
