package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseShorthand(t *testing.T) {
	tests := []struct {
		in      string
		want    Endpoint
		wantErr bool
	}{
		{in: "root:secret@10.0.0.1:3307/dbA",
			want: Endpoint{Host: "10.0.0.1", Port: 3307, User: "root", Password: "secret", Database: "dbA", passwordSet: true}},
		{in: "root@localhost/mydb",
			want: Endpoint{Host: "localhost", Port: 3306, User: "root", Database: "mydb"}},
		// an explicit empty password segment marks a password-less server
		// (TiDB's default root) and suppresses the prompt
		{in: "root:@localhost/mydb",
			want: Endpoint{Host: "localhost", Port: 3306, User: "root", Database: "mydb", passwordSet: true}},
		{in: "192.168.1.5",
			want: Endpoint{Host: "192.168.1.5", Port: 3306}},
		{in: "  u:p@h:1/db  ",
			want: Endpoint{Host: "h", Port: 1, User: "u", Password: "p", Database: "db", passwordSet: true}},
		{in: "u:p @h:1/db", wantErr: true}, // inner whitespace is a typo trap
		{in: "host:badport/db", wantErr: true},
		{in: "/db", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseShorthand(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseShorthand(%q): expected error, got %+v", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseShorthand(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseShorthand(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestMaskedDSN(t *testing.T) {
	e := Endpoint{Host: "h", Port: 3306, User: "u", Password: "s3cr3t", Database: "db"}
	if got := e.MaskedDSN(); got != "u:***@h:3306/db" {
		t.Errorf("MaskedDSN() = %q", got)
	}
	e2 := Endpoint{Host: "h", User: "u"}
	if got := e2.MaskedDSN(); got != "u@h" {
		t.Errorf("MaskedDSN() = %q, want no port/password for empty", got)
	}
	e3 := Endpoint{Host: "h", Port: 1, User: "u", PasswordEnv: "X"}
	if got := e3.MaskedDSN(); got != "u:***@h:1" {
		t.Errorf("MaskedDSN() = %q, want redaction when only env set", got)
	}
}

func TestLoadFileEnvExpansion(t *testing.T) {
	t.Setenv("MTDIFF_TEST_HOST", "envhost")
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	content := `
src:
  host: ${MTDIFF_TEST_HOST}
  port: 3307
  user: u
  password_env: MTDIFF_TEST_PWD
  database: dbA
options:
  parallel: 8
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MTDIFF_TEST_PWD", "pw-from-env")
	c, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Src.Host != "envhost" {
		t.Errorf("env expansion failed: %+v", c.Src)
	}
	if c.Src.Port != 3307 || c.Src.Database != "dbA" {
		t.Errorf("yaml fields not parsed: %+v", c.Src)
	}
	if c.Opts.Parallel != 8 {
		t.Errorf("options not parsed: %+v", c.Opts)
	}
	if err := c.Src.ResolvePassword(nil); err != nil {
		t.Fatal(err)
	}
	if c.Src.Password != "pw-from-env" {
		t.Errorf("ResolvePassword: got %q", c.Src.Password)
	}
}

// validConfig is a fully-populated config exercising the option fields a
// real file carries; strict parsing must keep accepting it.
const validConfig = `
src:
  host: h1
  port: 3306
  user: u
  password: p
  database: db1
dst:
  host: h2
  user: u
  database: db2
options:
  tables: [t1, t2]
  exclude_tables: [t3]
  parallel: 8
  chunk_size: 5000
  sample_limit: 3
  batch_size: 2000
  no_sync_schema: true
  allow_unenforced_readonly: true
  allow_structure_truncate: true
  allow_row_rewrite: true
`

func TestParseStrictValidConfig(t *testing.T) {
	c, err := Parse([]byte(validConfig))
	if err != nil {
		t.Fatalf("a legal config must parse: %v", err)
	}
	if c.Src.Host != "h1" || c.Dst.Database != "db2" {
		t.Errorf("fields not parsed: %+v", c)
	}
	if c.Opts.Parallel != 8 || c.Opts.ChunkSize != 5000 || c.Opts.BatchSize != 2000 {
		t.Errorf("options not parsed: %+v", c.Opts)
	}
	if c.Opts.SampleLimit == nil || *c.Opts.SampleLimit != 3 {
		t.Errorf("sample_limit not parsed: %+v", c.Opts.SampleLimit)
	}
	if !c.Opts.NoSyncSchema || !c.Opts.AllowUnenforcedReadOnly ||
		!c.Opts.AllowStructureTruncate || !c.Opts.AllowRowRewrite {
		t.Errorf("destructive/behavior flags not parsed: %+v", c.Opts)
	}
}

// The strict-parser regressions: every misspelling below used to be
// SILENTLY IGNORED — e.g. "exclude_table" (singular) would drop out and
// the excluded table would enter the sync/drop set. Each must now fail
// at parse time.
func TestParseStrictUnknownFields(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		wantSub string
	}{
		{
			name: "unknown top-level field",
			doc: `
srcc:
  host: h
`,
			wantSub: "srcc",
		},
		{
			name: "unknown endpoint field",
			doc: `
src:
  host: h
  hostname: also-h
`,
			wantSub: "hostname",
		},
		{
			name: "unknown options field",
			doc: `
src:
  host: h
dst:
  host: d
options:
  exclude_table:
    - audit_log
`,
			wantSub: "exclude_table",
		},
		{
			name: "misspelled destructive option",
			doc: `
src:
  host: h
dst:
  host: d
options:
  allow_structure_trancate: true
`,
			wantSub: "allow_structure_trancate",
		},
		{
			name:    "misspelled option in a full config",
			doc:     "options:\n  allow_unenforced_readonly: true\n  allow_unenforced_readonl: true\n",
			wantSub: "allow_unenforced_readonl",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(tc.doc))
			if err == nil {
				t.Fatalf("must refuse unknown fields, parsed %+v", c)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q must name the unknown field %q", err, tc.wantSub)
			}
		})
	}
}

func TestParseStrictMultipleDocuments(t *testing.T) {
	_, err := Parse([]byte(validConfig + "\n---\ndst:\n  host: other\n"))
	if err == nil {
		t.Fatal("a second YAML document must be refused (reading only the first would hide it)")
	}
	if !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Errorf("error %q must say a second document was found", err)
	}
}

// Env expansion must keep working through the strict decoder (the
// expansion is a structure-aware substitution of the parsed tree, and
// the strict decode must not regress it).
func TestLoadFileStrictWithEnvExpansion(t *testing.T) {
	t.Setenv("MTDIFF_TEST_HOST2", "envhost2")
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	content := "src:\n  host: ${MTDIFF_TEST_HOST2}\ndst:\n  host: d\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(path)
	if err != nil {
		t.Fatalf("env expansion through the strict parser must not break: %v", err)
	}
	if c.Src.Host != "envhost2" {
		t.Errorf("env expansion failed: %+v", c.Src)
	}
}

func TestResolvePasswordPriority(t *testing.T) {
	t.Setenv("MTDIFF_TEST_PWD2", "envpw")
	// env beats prompt
	e := Endpoint{User: "u", Host: "h", PasswordEnv: "MTDIFF_TEST_PWD2", Password: "yampw"}
	prompted := false
	if err := e.ResolvePassword(func(string) (string, error) { prompted = true; return "promptpw", nil }); err != nil {
		t.Fatal(err)
	}
	if e.Password != "envpw" || prompted {
		t.Errorf("env should win: pw=%q prompted=%v", e.Password, prompted)
	}
	// pre-filled password beats prompt
	e2 := Endpoint{User: "u", Host: "h", Password: "yampw"}
	if err := e2.ResolvePassword(func(string) (string, error) { prompted = true; return "promptpw", nil }); err != nil {
		t.Fatal(err)
	}
	if e2.Password != "yampw" || prompted {
		t.Errorf("yaml password should win: pw=%q prompted=%v", e2.Password, prompted)
	}
	// prompt used only when nothing else
	e3 := Endpoint{User: "u", Host: "h"}
	if err := e3.ResolvePassword(func(string) (string, error) { prompted = true; return "promptpw", nil }); err != nil {
		t.Fatal(err)
	}
	if e3.Password != "promptpw" || !prompted {
		t.Errorf("prompt should be used: pw=%q", e3.Password)
	}
}

// TestResolvePasswordExplicitEmpty pins the "user:@host" semantics: an
// explicitly empty password (a password-less server, e.g. TiDB's default
// root) must not prompt, while an absent password segment still does.
func TestResolvePasswordExplicitEmpty(t *testing.T) {
	e, err := ParseShorthand("root:@localhost/mydb")
	if err != nil {
		t.Fatal(err)
	}
	prompted := false
	if err := e.ResolvePassword(func(string) (string, error) { prompted = true; return "x", nil }); err != nil {
		t.Fatal(err)
	}
	if prompted || e.Password != "" {
		t.Errorf("explicit empty password must not prompt: pw=%q prompted=%v", e.Password, prompted)
	}

	e2, err := ParseShorthand("root@localhost/mydb")
	if err != nil {
		t.Fatal(err)
	}
	prompted = false
	if err := e2.ResolvePassword(func(string) (string, error) { prompted = true; return "x", nil }); err != nil {
		t.Fatal(err)
	}
	if !prompted {
		t.Errorf("absent password should prompt")
	}
}

// TestResolvePasswordEnvMissing covers the P3-#11 behavior: a password_env
// naming an unset variable is a configuration error, not a silent
// fallback to a password-less connection.
func TestResolvePasswordEnvMissing(t *testing.T) {
	t.Setenv("MTDIFF_TEST_UNSET_PWD", "")
	os.Unsetenv("MTDIFF_TEST_UNSET_PWD")
	e := Endpoint{User: "u", Host: "h", PasswordEnv: "MTDIFF_TEST_UNSET_PWD"}
	err := e.ResolvePassword(nil)
	if err == nil {
		t.Fatalf("unset password_env variable must error, connected password-less instead (pw=%q)", e.Password)
	}
	if e.Password != "" {
		t.Errorf("password must stay empty on error, got %q", e.Password)
	}

	// a set-but-empty variable resolves to an empty password without error
	t.Setenv("MTDIFF_TEST_EMPTY_PWD", "")
	e2 := Endpoint{User: "u", Host: "h", PasswordEnv: "MTDIFF_TEST_EMPTY_PWD"}
	if err := e2.ResolvePassword(nil); err != nil {
		t.Fatalf("set-but-empty variable must not error: %v", err)
	}
	if e2.Password != "" {
		t.Errorf("empty variable must yield empty password, got %q", e2.Password)
	}
}

// TestAllowUnenforcedReadOnlyYAML pins the YAML spelling of the read-only
// guard opt-in (config files cannot set the unexported passwordSet, but this
// is a normal exported option).
func TestAllowUnenforcedReadOnlyYAML(t *testing.T) {
	var o Options
	if err := yaml.Unmarshal([]byte("allow_unenforced_readonly: true\n"), &o); err != nil {
		t.Fatal(err)
	}
	if !o.AllowUnenforcedReadOnly {
		t.Errorf("allow_unenforced_readonly not parsed: %+v", o)
	}
}

// The R5-2 regressions: the ${ENV} substitution runs on the PARSED
// value scalars only, byte-exactly, and can never touch structure.

// Special characters in an environment value must survive the
// parse-substitute-reencode round trip byte for byte (the raw-text era
// broke on a '#' comment, a 'a: b' colon, an embedded quote, a real
// newline, a backslash or non-ASCII bytes).
func TestEnvExpansionSpecialChars(t *testing.T) {
	values := []string{
		`p#ss`,       // '#' starts a comment in raw text
		`a: b`,       // a colon+space is a mapping separator
		`a"b`,        // an embedded quote
		"abc\ndef",   // a real newline
		`p\w`,        // a backslash (escape sequences)
		`中文密码`,       // non-ASCII bytes
		`${LITERAL}`, // literal ${} text inside the VALUE is final (no re-expansion)
		`tab\there`,  // a real tab
		`'single'`,   // embedded single quotes
	}
	for i, v := range values {
		t.Run(fmt.Sprintf("value%02d", i), func(t *testing.T) {
			name := fmt.Sprintf("MTDIFF_TEST_SPECIAL_%02d", i)
			t.Setenv(name, v)
			path := filepath.Join(t.TempDir(), "cfg.yaml")
			content := fmt.Sprintf("src:\n  host: h\n  password: ${%s}\ndst:\n  host: d\n", name)
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			c, err := LoadFile(path)
			if err != nil {
				t.Fatalf("a special-character value must not break parsing: %v", err)
			}
			if c.Src.Password != v {
				t.Errorf("byte-exact round trip failed:\n got  %q\n want %q", c.Src.Password, v)
			}
		})
	}
}

// An environment value carrying YAML structure (a newline plus an
// options block) must land ENTIRELY in the one value it replaced: the
// destination flag it "injects" must not be set, and the value itself
// must be byte-exact. Under the raw-text substitution this flipped
// allow_structure_truncate on.
func TestEnvExpansionStructuralInjection(t *testing.T) {
	t.Setenv("MTDIFF_TEST_INJECT", "abc\noptions:\n  allow_structure_truncate: true")
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	content := "src:\n  host: h\n  password: ${MTDIFF_TEST_INJECT}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(path)
	if err != nil {
		t.Fatalf("a value containing newlines/colons is still a value: %v", err)
	}
	if c.Src.Password != "abc\noptions:\n  allow_structure_truncate: true" {
		t.Errorf("the value must be byte-exact, got %q", c.Src.Password)
	}
	if c.Opts.AllowStructureTruncate {
		t.Error("the environment value INJECTED a second options block and flipped a safety flag — structure must be un-injectable")
	}
}

// A reference to an UNSET variable is a parse error naming the
// variable (fail closed): the raw-text era substituted a silent empty
// string, and the failure surfaced later as an incomprehensible
// server-side auth error.
func TestEnvExpansionUnsetError(t *testing.T) {
	os.Unsetenv("MTDIFF_TEST_DEFINITELY_UNSET")
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	content := "src:\n  host: h\n  password: ${MTDIFF_TEST_DEFINITELY_UNSET}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(path)
	if err == nil {
		t.Fatalf("an unset variable must fail closed, parsed %+v", c)
	}
	if !strings.Contains(err.Error(), "MTDIFF_TEST_DEFINITELY_UNSET") {
		t.Errorf("the error must name the unset variable, got %q", err)
	}
}

// Substituted text is final: a value that CONTAINS a ${OTHER} sequence
// (OTHER set, for contrast) is not expanded a second time.
func TestEnvExpansionNoRecursiveExpansion(t *testing.T) {
	t.Setenv("MTDIFF_TEST_OUTER", "${MTDIFF_TEST_INNER}")
	t.Setenv("MTDIFF_TEST_INNER", "expanded-second-time")
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	content := "src:\n  host: h\n  password: ${MTDIFF_TEST_OUTER}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Src.Password != "${MTDIFF_TEST_INNER}" {
		t.Errorf("substituted text must be final, got %q", c.Src.Password)
	}
}

// Mapping KEYS are never expansion targets: a ${...} in a key position
// stays literal. Here it surfaces as the KnownFields error for an
// unknown field — NOT as an environment error (the variable is unset
// on purpose: had the key been expanded, the failure would have named
// the variable instead of the field).
func TestEnvExpansionKeysNeverExpanded(t *testing.T) {
	os.Unsetenv("MTDIFF_TEST_KEYVAR")
	_, err := Parse([]byte("src:\n  ${MTDIFF_TEST_KEYVAR}: x\n"))
	if err == nil {
		t.Fatal("an unknown (literal) field must be refused by KnownFields")
	}
	if strings.Contains(err.Error(), "is not set") {
		t.Errorf("a mapping key was env-expanded (structure injection): %v", err)
	}
	if !strings.Contains(err.Error(), "MTDIFF_TEST_KEYVAR") {
		t.Errorf("the KnownFields error must name the literal key, got %q", err)
	}
}

// Sequence items are values too: expansion must reach into lists.
func TestEnvExpansionSequenceItems(t *testing.T) {
	t.Setenv("MTDIFF_TEST_TBL", "audit_log")
	c, err := Parse([]byte("src:\n  host: h\ndst:\n  host: d\noptions:\n  exclude_tables:\n    - ${MTDIFF_TEST_TBL}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Opts.ExcludeTables) != 1 || c.Opts.ExcludeTables[0] != "audit_log" {
		t.Errorf("sequence items must expand, got %v", c.Opts.ExcludeTables)
	}
}

// The R6-4 regressions: a LONE ${ENV} on a TYPED field (int/bool/float
// Go type — see typedEnvFields) decodes as that type: `parallel: ${P}`
// with P=8 is the int 8, not the string "8" (the string-only expansion
// of R5 left it as "8", which the strict decode refused). The
// coercion is of the isolated scalar only: it can never read or create
// structure, quoted placeholders stay deliberate strings, and
// untyped fields keep their value as a string byte for byte.

func TestEnvExpansionTypedScalars(t *testing.T) {
	t.Setenv("MTDIFF_TEST_PORT", "3307")
	t.Setenv("MTDIFF_TEST_PAR", "8")
	t.Setenv("MTDIFF_TEST_SNAP", "true")
	t.Setenv("MTDIFF_TEST_TOL", "0.001")
	t.Setenv("MTDIFF_TEST_ST", "true")
	t.Setenv("MTDIFF_TEST_ARW", "false")
	c, err := Parse([]byte(`
src:
  host: h
  port: ${MTDIFF_TEST_PORT}
dst:
  host: d
options:
  parallel: ${MTDIFF_TEST_PAR}
  snapshot: ${MTDIFF_TEST_SNAP}
  tolerance: ${MTDIFF_TEST_TOL}
  allow_structure_truncate: ${MTDIFF_TEST_ST}
  allow_row_rewrite: ${MTDIFF_TEST_ARW}
`))
	if err != nil {
		t.Fatalf("typed env scalars must parse: %v", err)
	}
	if c.Src.Port != 3307 {
		t.Errorf("port: ${ENV}=3307 must decode as the int 3307, got %d", c.Src.Port)
	}
	if c.Opts.Parallel != 8 {
		t.Errorf("parallel: ${ENV}=8 must decode as the int 8, got %d", c.Opts.Parallel)
	}
	if !c.Opts.Snapshot {
		t.Errorf("snapshot: ${ENV}=true must decode as true")
	}
	if c.Opts.Tolerance != 0.001 {
		t.Errorf("tolerance: ${ENV}=0.001 must decode as 0.001, got %v", c.Opts.Tolerance)
	}
	if !c.Opts.AllowStructureTruncate {
		t.Errorf("allow_structure_truncate: ${ENV}=true must decode as true")
	}
	if c.Opts.AllowRowRewrite {
		t.Errorf("allow_row_rewrite: ${ENV}=false must decode as false (not the default)")
	}
}

// A substituted value that does not parse for the field's type is a
// configuration error naming the variable and the value (fail closed).
func TestEnvExpansionTypedInvalid(t *testing.T) {
	cases := []struct {
		name, field, env, value string
	}{
		{"int", "parallel", "MTDIFF_TEST_BAD_INT", "not-a-number"},
		{"float", "tolerance", "MTDIFF_TEST_BAD_FLOAT", "1e"},
		{"bool", "snapshot", "MTDIFF_TEST_BAD_BOOL", "maybe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			_, err := Parse([]byte("src:\n  host: h\noptions:\n  " + tc.field + ": ${" + tc.env + "}\n"))
			if err == nil {
				t.Fatalf("a value that does not parse for %s must fail closed", tc.field)
			}
			if !strings.Contains(err.Error(), tc.env) || !strings.Contains(err.Error(), tc.value) {
				t.Errorf("the error must name the variable and its value, got %q", err)
			}
		})
	}
}

// A value that LOOKS like a mapping must not create the second key:
// on a bool field the isolated scalar simply fails to parse (the
// structural-injection value of R5-2, re-tested against the typed
// path), and on an untyped field it is still just a value.
func TestEnvExpansionTypedInjectionRefused(t *testing.T) {
	t.Setenv("MTDIFF_TEST_ALLOWX", "true\nother_key: x")
	if _, err := Parse([]byte("src:\n  host: h\noptions:\n  allow_row_rewrite: ${MTDIFF_TEST_ALLOWX}\n")); err == nil {
		t.Fatal("a value containing a new mapping line for a bool field must fail closed (no new field may appear)")
	} else if !strings.Contains(err.Error(), "MTDIFF_TEST_ALLOWX") {
		t.Errorf("the error must name the variable, got %q", err)
	}
	c, err := Parse([]byte("src:\n  host: h\n  password: ${MTDIFF_TEST_ALLOWX}\n"))
	if err != nil {
		t.Fatalf("on an untyped field the value is still a value: %v", err)
	}
	if c.Src.Password != "true\nother_key: x" {
		t.Errorf("byte-exact value required, got %q", c.Src.Password)
	}
}

// UNTYPED (string) fields keep the substituted value a string, byte
// for byte — even when it looks like a number.
func TestEnvExpansionUntypedKeysStayString(t *testing.T) {
	t.Setenv("MTDIFF_TEST_H8", "8")
	c, err := Parse([]byte("src:\n  host: ${MTDIFF_TEST_H8}\n"))
	if err != nil {
		t.Fatalf("a string field taking a numeric env value must stay a string: %v", err)
	}
	if c.Src.Host != "8" {
		t.Errorf("host must be the string \"8\", got %q", c.Src.Host)
	}
}

// Sequence items have no typed field: the same value is a plain string
// there (and decodes as one).
func TestEnvExpansionSequenceItemsStayString(t *testing.T) {
	t.Setenv("MTDIFF_TEST_SEQ", "8")
	c, err := Parse([]byte("src:\n  host: h\ndst:\n  host: d\noptions:\n  tables:\n    - ${MTDIFF_TEST_SEQ}\n"))
	if err != nil {
		t.Fatalf("sequence items have no typed field and must stay strings: %v", err)
	}
	if len(c.Opts.Tables) != 1 || c.Opts.Tables[0] != "8" {
		t.Errorf("got %v", c.Opts.Tables)
	}
}

// An explicit quote is a deliberate STRING: `parallel: "${P}"` is not
// coerced (the strict decode refuses the string for an int field —
// fail closed), while a quoted placeholder on a string field is legal.
func TestEnvExpansionQuotedPlaceholderStaysString(t *testing.T) {
	t.Setenv("MTDIFF_TEST_QP", "8")
	if _, err := Parse([]byte("src:\n  host: h\noptions:\n  parallel: \"${MTDIFF_TEST_QP}\"\n")); err == nil {
		t.Fatal(`a quoted placeholder on a typed field must not be coerced (it stays the string "8", refused by the strict decoder)`)
	}
	c, err := Parse([]byte("src:\n  host: h\n  user: \"${MTDIFF_TEST_QP}\"\n"))
	if err != nil {
		t.Fatalf("a quoted placeholder on a string field must parse: %v", err)
	}
	if c.Src.User != "8" {
		t.Errorf("got %q", c.Src.User)
	}
}

// A placeholder that is PART of a larger value is a string: no typed
// coercion (the strict decode refuses it on a typed field), and on a
// string field it is just concatenation.
func TestEnvExpansionPartialSubstitutionStaysString(t *testing.T) {
	t.Setenv("MTDIFF_TEST_PS", "8")
	if _, err := Parse([]byte("src:\n  host: h\noptions:\n  parallel: x${MTDIFF_TEST_PS}\n")); err == nil {
		t.Fatal("a partial substitution on a typed field must stay a string and be refused, not coerced")
	}
	c, err := Parse([]byte("src:\n  host: h\n  user: x${MTDIFF_TEST_PS}\n"))
	if err != nil {
		t.Fatalf("partial substitution on a string field: %v", err)
	}
	if c.Src.User != "x8" {
		t.Errorf("got %q", c.Src.User)
	}
}

func TestValidateAndDefaults(t *testing.T) {
	c := &Config{Src: Endpoint{Host: "a"}, Dst: Endpoint{Host: "b"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	c.ApplyDefaults()
	if c.Src.Port != 3306 || c.Opts.Parallel != 4 || c.Opts.ChunkSize != 10000 || c.Opts.DrillLimit != 10 || c.Opts.BatchSize != 1000 || c.SampleLimitOr(0) != 5 {
		t.Errorf("defaults wrong: %+v", c)
	}
	c2 := &Config{Src: Endpoint{Host: "a"}}
	if err := c2.Validate(); err == nil {
		t.Error("missing dst host should fail")
	}

	// P3-#10: explicitly negative values must be reported by Validate
	// (the old order ran ApplyDefaults first and silently rewrote them).
	c3 := &Config{Src: Endpoint{Host: "a"}, Dst: Endpoint{Host: "b"}}
	c3.Opts.Parallel = -1
	if err := c3.Validate(); err == nil {
		t.Error("negative parallel must fail validation")
	}
	c3.Opts.Parallel = 0
	c3.Opts.ChunkSize = -5
	if err := c3.Validate(); err == nil {
		t.Error("negative chunk_size must fail validation")
	}
	c3.Opts.ChunkSize = 0
	c3.Opts.DrillLimit = -1
	if err := c3.Validate(); err == nil {
		t.Error("negative drill_limit must fail validation")
	}
	c3.Opts.DrillLimit = 0
	c3.Opts.Tolerance = -1e-9
	if err := c3.Validate(); err == nil {
		t.Error("negative tolerance must fail validation")
	}
	c3.Opts.Tolerance = 0
	c3.Opts.BatchSize = -1
	if err := c3.Validate(); err == nil {
		t.Error("negative batch_size must fail validation")
	}
	c3.Opts.BatchSize = 0
	neg := -1
	c3.Opts.SampleLimit = &neg
	if err := c3.Validate(); err == nil {
		t.Error("negative sample_limit must fail validation")
	}

	// explicit sample_limit 0 means "show no samples": it is legal and
	// must survive ApplyDefaults (unlike every other zero, it is not
	// "unset")
	c5 := &Config{Src: Endpoint{Host: "a"}, Dst: Endpoint{Host: "b"}}
	zero := 0
	c5.Opts.SampleLimit = &zero
	if err := c5.Validate(); err != nil {
		t.Errorf("explicit sample_limit 0 must be accepted: %v", err)
	}
	c5.ApplyDefaults()
	if c5.SampleLimitOr(-1) != 0 {
		t.Errorf("explicit sample_limit 0 must survive ApplyDefaults, got %d", c5.SampleLimitOr(-1))
	}

	// P3-#17: a positive but tiny chunk_size is rejected (0 is still
	// "unset" and legal — it receives the default).
	c4 := &Config{Src: Endpoint{Host: "a"}, Dst: Endpoint{Host: "b"}}
	c4.Opts.ChunkSize = 5
	if err := c4.Validate(); err == nil {
		t.Errorf("chunk_size 5 is below the minimum of %d, must fail", MinChunkSize)
	}
	c4.Opts.ChunkSize = MinChunkSize
	if err := c4.Validate(); err != nil {
		t.Errorf("chunk_size %d must be accepted: %v", MinChunkSize, err)
	}
}

// TestValidateRejectsNonFiniteTolerance pins the config-entry gate for
// the silent-false-identical tolerance (P0-1): NaN/±Inf (and negative
// finite values) are configuration errors, and only 0 or a finite
// positive value is legal. The Go-level construction covers the CLI
// flag path (--tolerance inf / nan reach the same Validate through
// pflag's ParseFloat).
func TestValidateRejectsNonFiniteTolerance(t *testing.T) {
	base := func() *Config {
		return &Config{Src: Endpoint{Host: "a"}, Dst: Endpoint{Host: "b"}}
	}
	for _, tol := range []float64{math.Inf(1), math.Inf(-1), math.NaN()} {
		c := base()
		c.Opts.Tolerance = tol
		if err := c.Validate(); err == nil {
			t.Errorf("tolerance %v must fail Validate (a non-finite tolerance would normalize every float to the same rendering)", tol)
		} else if !strings.Contains(err.Error(), "tolerance") {
			t.Errorf("the error must name the tolerance: %v", err)
		}
	}
	// 0 (bit-exact) and finite positive values stay legal
	for _, tol := range []float64{0, 1e-9, 0.5} {
		c := base()
		c.Opts.Tolerance = tol
		if err := c.Validate(); err != nil {
			t.Errorf("tolerance %v must be accepted: %v", tol, err)
		}
	}
}

// TestYAMLNonFiniteToleranceFailsClosed covers the YAML construction
// layer: .inf / -.inf / .nan decode as legal floats, so Parse accepts
// the document — the config as a whole must still fail closed at the
// Validate gate (every command path runs it before ApplyDefaults), and
// a finite value keeps working.
func TestYAMLNonFiniteToleranceFailsClosed(t *testing.T) {
	for _, text := range []string{
		"src:\n  host: a\ndst:\n  host: b\noptions:\n  tolerance: .inf\n",
		"src:\n  host: a\ndst:\n  host: b\noptions:\n  tolerance: -.inf\n",
		"src:\n  host: a\ndst:\n  host: b\noptions:\n  tolerance: .nan\n",
	} {
		c, err := Parse([]byte(text))
		if err != nil {
			continue // Parse itself refusing is fine: it fails closed either way
		}
		if err := c.Validate(); err == nil {
			t.Errorf("YAML %q must fail closed (non-finite tolerance), got a usable config", text)
		} else if !strings.Contains(err.Error(), "tolerance") {
			t.Errorf("the error must name the tolerance: %v", err)
		}
	}
	c, err := Parse([]byte("src:\n  host: a\ndst:\n  host: b\noptions:\n  tolerance: 0.001\n"))
	if err != nil {
		t.Fatalf("finite YAML tolerance must parse: %v", err)
	}
	if c.Opts.Tolerance != 0.001 {
		t.Fatalf("finite YAML tolerance = %v, want 0.001", c.Opts.Tolerance)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("finite YAML tolerance must pass Validate: %v", err)
	}
}
