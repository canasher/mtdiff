package config

import (
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
// expansion is a raw-text substitution BEFORE parsing, but the decoder
// change must not regress it).
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
