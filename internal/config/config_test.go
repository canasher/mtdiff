package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseShorthand(t *testing.T) {
	tests := []struct {
		in      string
		want    Endpoint
		wantErr bool
	}{
		{in: "root:secret@10.0.0.1:3307/dbA",
			want: Endpoint{Host: "10.0.0.1", Port: 3307, User: "root", Password: "secret", Database: "dbA"}},
		{in: "root@localhost/mydb",
			want: Endpoint{Host: "localhost", Port: 3306, User: "root", Database: "mydb"}},
		{in: "192.168.1.5",
			want: Endpoint{Host: "192.168.1.5", Port: 3306}},
		{in: "  u:p@h:1/db  ",
			want: Endpoint{Host: "h", Port: 1, User: "u", Password: "p", Database: "db"}},
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

func TestValidateAndDefaults(t *testing.T) {
	c := &Config{Src: Endpoint{Host: "a"}, Dst: Endpoint{Host: "b"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	c.ApplyDefaults()
	if c.Src.Port != 3306 || c.Opts.Parallel != 4 || c.Opts.ChunkSize != 10000 || c.Opts.DrillLimit != 10 {
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
