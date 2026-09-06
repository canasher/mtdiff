package conn

import (
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"mtdiff/internal/config"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		in         string
		wantFamily string
		wantPrec   int
		wantScale  int
	}{
		{in: "bigint(20)", wantFamily: FamINT},
		{in: "bigint(20) unsigned", wantFamily: FamUINT},
		{in: "int(11)", wantFamily: FamINT},
		{in: "tinyint(1)", wantFamily: FamINT},
		{in: "mediumint unsigned", wantFamily: FamUINT},
		{in: "decimal(10,2)", wantFamily: FamDECIMAL, wantPrec: 10, wantScale: 2},
		{in: "DECIMAL(4)", wantFamily: FamDECIMAL, wantPrec: 4, wantScale: 0},
		{in: "numeric(8,3)", wantFamily: FamDECIMAL, wantPrec: 8, wantScale: 3},
		{in: "float", wantFamily: FamFLOAT},
		{in: "double", wantFamily: FamDOUBLE},
		{in: "real", wantFamily: FamDOUBLE},
		{in: "date", wantFamily: FamDATE},
		{in: "datetime", wantFamily: FamDATETIME, wantScale: 0},
		{in: "datetime(3)", wantFamily: FamDATETIME, wantScale: 3},
		{in: "timestamp(6)", wantFamily: FamTIMESTAMP, wantScale: 6},
		{in: "time(2)", wantFamily: FamTIME, wantScale: 2},
		{in: "year", wantFamily: FamYEAR},
		{in: "enum('a','b')", wantFamily: FamENUM},
		{in: "set('x','y')", wantFamily: FamSET},
		{in: "char(10)", wantFamily: FamSTR},
		{in: "varchar(255)", wantFamily: FamSTR},
		{in: "text", wantFamily: FamSTR},
		{in: "longtext", wantFamily: FamSTR},
		{in: "blob", wantFamily: FamBYTES},
		{in: "varbinary(16)", wantFamily: FamBYTES},
		{in: "json", wantFamily: FamJSON},
		{in: "bit(1)", wantFamily: FamBIT},
		{in: "weird(3)", wantFamily: FamSTR}, // unknown falls back to byte-exact
	}
	for _, tt := range tests {
		f, p, s := classify(tt.in)
		if f != tt.wantFamily || p != tt.wantPrec || s != tt.wantScale {
			t.Errorf("classify(%q) = (%s,%d,%d), want (%s,%d,%d)", tt.in, f, p, s, tt.wantFamily, tt.wantPrec, tt.wantScale)
		}
	}
}

func TestQuoteIdent(t *testing.T) {
	if got := quoteIdent("orders"); got != "`orders`" {
		t.Errorf("quoteIdent = %q", got)
	}
	if got := quoteIdent("we`ird"); got != "`we``ird`" {
		t.Errorf("quoteIdent = %q", got)
	}
}

func TestBuildDSN(t *testing.T) {
	// Round-trip through the driver's OWN parser: the DSN is built by
	// mysql.Config.FormatDSN and must come back byte for byte through
	// mysql.ParseDSN. The hand-rolled net/url builder percent-encoded
	// the password ("s3:cret" -> the literal "s3%3Acret" the server
	// rejected) and appended the database name raw (a name containing
	// "/" or "?" broke the path/query boundary); neither is allowed
	// anymore.
	ep := config.Endpoint{Host: "10.0.0.1", Port: 3307, User: "root", Database: "dbA"}
	for _, pw := range []string{"normal", "s3:cret", "p@ss", "p/a?b#c", "sp ace", "中文密码", "p@ss:word/a?b"} {
		ep.Password = pw
		cfg, err := mysql.ParseDSN(BuildDSN(ep, 0))
		if err != nil {
			t.Fatalf("ParseDSN(BuildDSN password %q): %v", pw, err)
		}
		if cfg.Passwd != pw {
			t.Errorf("password %q round-trip = %q", pw, cfg.Passwd)
		}
		if cfg.User != "root" {
			t.Errorf("user round-trip = %q, want root", cfg.User)
		}
	}
	for _, db := range []string{"normal_db", "db/a", "db?x", "db%x", "中文库名"} {
		ep.Password = "p@ss:word/a?b" // the worst case from both halves
		ep.Database = db
		cfg, err := mysql.ParseDSN(BuildDSN(ep, 0))
		if err != nil {
			t.Fatalf("ParseDSN(BuildDSN database %q): %v", db, err)
		}
		if cfg.DBName != db {
			t.Errorf("database %q round-trip = %q", db, cfg.DBName)
		}
		if cfg.Passwd != "p@ss:word/a?b" {
			t.Errorf("password round-trip (database %q) = %q", db, cfg.Passwd)
		}
	}
	// an @ in the USERNAME survives when a password is present (the
	// parser splits on the last @); a password-less @-username is a
	// driver limitation, documented, not a regression
	ep2 := config.Endpoint{Host: "h", Port: 1, User: "u@x", Password: "pw", Database: "d"}
	if cfg, err := mysql.ParseDSN(BuildDSN(ep2, 0)); err != nil || cfg.User != "u@x" || cfg.Passwd != "pw" {
		t.Errorf("user u@x round-trip = (%q,%q,%v)", cfg.User, cfg.Passwd, err)
	}
	// a password-less DSN keeps the old root@tcp( form
	ep3 := config.Endpoint{Host: "10.0.0.1", Port: 3307, User: "root", Database: "dbA"}
	if got := BuildDSN(ep3, 0); !strings.Contains(got, "root@tcp(10.0.0.1:3307)/dbA") {
		t.Errorf("BuildDSN no-password = %q", got)
	}
	// the connection parameters must not have regressed
	cfg, err := mysql.ParseDSN(BuildDSN(config.Endpoint{Host: "h", Port: 1, User: "u", Password: "p", Database: "d"}, 1<<20))
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	checks := []struct {
		name string
		ok   bool
	}{
		{"parseTime", cfg.ParseTime},
		{"loc=UTC", cfg.Loc != nil && cfg.Loc == time.UTC},
		{"interpolateParams=false", !cfg.InterpolateParams},
		{"timeout=10s", cfg.Timeout == 10*time.Second},
		{"readTimeout=10m", cfg.ReadTimeout == 10*time.Minute},
		{"maxAllowedPacket", cfg.MaxAllowedPacket == 1<<20},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("parameter %s regressed: %q", c.name, BuildDSN(config.Endpoint{Host: "h", Port: 1, User: "u", Password: "p", Database: "d"}, 1<<20))
		}
	}
	// charset=utf8mb4 (the private charsets field is asserted through the
	// DSN text; the driver's charset applier is what writes it)
	dsn := BuildDSN(config.Endpoint{Host: "h", Port: 1, User: "u", Password: "p", Database: "d"}, 0)
	if !strings.Contains(dsn, "charset=utf8mb4") {
		t.Errorf("charset=utf8mb4 missing from DSN: %q", dsn)
	}
	// the writer DSN only differs in a longer network write timeout: a
	// multi-row INSERT batch can take far longer to send than a query.
	wcfg, err := mysql.ParseDSN(BuildWriterDSN(config.Endpoint{Host: "h", Port: 1, User: "u", Password: "s3:cret", Database: "d"}, 0))
	if err != nil {
		t.Fatalf("ParseDSN(BuildWriterDSN): %v", err)
	}
	if wcfg.WriteTimeout != 600*time.Second {
		t.Errorf("writer writeTimeout = %v, want 10m", wcfg.WriteTimeout)
	}
	if wcfg.Passwd != "s3:cret" {
		t.Errorf("writer password round-trip = %q", wcfg.Passwd)
	}
}

func col(name, family string) Column {
	return Column{Name: name, Family: family, RawType: family + "(x)"}
}

func TestCompatible(t *testing.T) {
	src := &Schema{Table: "t", Cols: []Column{col("id", FamINT), col("v", FamDECIMAL), col("ts", FamDATETIME), col("s", FamSTR)}}
	// identical
	dst := &Schema{Table: "t", Cols: []Column{col("id", FamINT), col("v", FamDECIMAL), col("ts", FamDATETIME), col("s", FamSTR)}}
	if warns, err := Compatible(src, dst, CompatOpts{}); err != nil {
		t.Errorf("identical schemas rejected: %v (warns %v)", err, warns)
	}
	// numeric widening: warning, not error
	dst2 := &Schema{Table: "t", Cols: []Column{col("id", FamUINT), col("v", FamDECIMAL), col("ts", FamDATETIME), col("s", FamSTR)}}
	warns, err := Compatible(src, dst2, CompatOpts{})
	if err != nil {
		t.Errorf("INT vs UINT should be tolerated: %v", err)
	} else if len(warns) == 0 {
		t.Error("INT vs UINT should warn")
	}
	// datetime vs timestamp: error by default, ok with AllowTZSwap
	dst3 := &Schema{Table: "t", Cols: []Column{col("id", FamINT), col("v", FamDECIMAL), col("ts", FamTIMESTAMP), col("s", FamSTR)}}
	if _, err := Compatible(src, dst3, CompatOpts{}); err == nil {
		t.Error("DATETIME vs TIMESTAMP must error by default")
	}
	if _, err := Compatible(src, dst3, CompatOpts{AllowTZSwap: true}); err != nil {
		t.Errorf("DATETIME vs TIMESTAMP with AllowTZSwap: %v", err)
	}
	// json vs text: hard error
	dst4 := &Schema{Table: "t", Cols: []Column{col("id", FamINT), col("v", FamDECIMAL), col("ts", FamDATETIME), col("s", FamJSON)}}
	if _, err := Compatible(src, dst4, CompatOpts{}); err == nil {
		t.Error("STR vs JSON must error")
	}
	// name mismatch
	dst5 := &Schema{Table: "t", Cols: []Column{col("ID", FamINT), col("v", FamDECIMAL), col("ts", FamDATETIME), col("s", FamSTR)}}
	if _, err := Compatible(src, dst5, CompatOpts{}); err == nil {
		t.Error("case-different column names must error")
	}
	// strict mode
	if _, err := Compatible(src, dst2, CompatOpts{Strict: true}); err == nil {
		t.Error("strict mode must reject INT vs UINT")
	}
}

func TestParseShowCreateAutoInc(t *testing.T) {
	cases := []struct {
		name         string
		create       string
		wantCol      string
		wantExplicit int64
		wantHasExp   bool
	}{
		{
			name: "explicit counter, backticked auto-inc column",
			create: "CREATE TABLE `t` (\n" +
				"  `id` int NOT NULL AUTO_INCREMENT,\n" +
				"  `code` varchar(16) NOT NULL,\n" +
				"  PRIMARY KEY (`id`)\n" +
				") ENGINE=InnoDB AUTO_INCREMENT=12001 DEFAULT CHARSET=utf8mb4",
			wantCol: "id", wantExplicit: 12001, wantHasExp: true,
		},
		{
			name: "no explicit counter (default follows max(id)+1)",
			create: "CREATE TABLE `t` (\n" +
				"  `id` int NOT NULL AUTO_INCREMENT,\n" +
				"  PRIMARY KEY (`id`)\n" +
				") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
			wantCol: "id", wantExplicit: 0, wantHasExp: false,
		},
		{
			name: "no auto-increment column at all",
			create: "CREATE TABLE `t` (\n" +
				"  `id` int NOT NULL,\n" +
				"  PRIMARY KEY (`id`)\n" +
				") ENGINE=InnoDB",
			wantCol: "", wantExplicit: 0, wantHasExp: false,
		},
		{
			name: "varchar auto-inc column with comment (paren in the type)",
			create: "CREATE TABLE `t` (\n" +
				"  `id` varchar(16) NOT NULL AUTO_INCREMENT COMMENT 'the id',\n" +
				"  PRIMARY KEY (`id`)\n" +
				") ENGINE=InnoDB AUTO_INCREMENT=7",
			wantCol: "id", wantExplicit: 7, wantHasExp: true,
		},
		{
			name:    "single-line output",
			create:  "CREATE TABLE `t` (`id` int NOT NULL AUTO_INCREMENT) ENGINE=InnoDB AUTO_INCREMENT=42 DEFAULT CHARSET=utf8mb4",
			wantCol: "id", wantExplicit: 42, wantHasExp: true,
		},
		{
			name: "bare (unbackticked) column definition",
			create: "CREATE TABLE t (\n" +
				"  id int NOT NULL AUTO_INCREMENT,\n" +
				"  PRIMARY KEY (id)\n" +
				") ENGINE=InnoDB AUTO_INCREMENT=5",
			wantCol: "id", wantExplicit: 5, wantHasExp: true,
		},
		{
			name: "explicit counter below the data maximum still parsed",
			create: "CREATE TABLE `t` (\n" +
				"  `id` bigint NOT NULL AUTO_INCREMENT,\n" +
				"  PRIMARY KEY (`id`)\n" +
				") ENGINE=InnoDB AUTO_INCREMENT=3 DEFAULT CHARSET=utf8mb4",
			wantCol: "id", wantExplicit: 3, wantHasExp: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			col, explicit, hasExplicit := parseShowCreateAutoInc(c.create)
			if col != c.wantCol {
				t.Errorf("col = %q, want %q", col, c.wantCol)
			}
			if explicit != c.wantExplicit {
				t.Errorf("explicit = %d, want %d", explicit, c.wantExplicit)
			}
			if hasExplicit != c.wantHasExp {
				t.Errorf("hasExplicit = %v, want %v", hasExplicit, c.wantHasExp)
			}
		})
	}
}

func TestFirstIdent(t *testing.T) {
	cases := map[string]string{
		"  `id` int NOT NULL AUTO_INCREMENT,": "id",
		"`my col` int AUTO_INCREMENT":         "my col",
		"  id bigint NOT NULL AUTO_INCREMENT": "id",
		"_under int AUTO_INCREMENT":           "_under",
	}
	for in, want := range cases {
		if got := firstIdent(in); got != want {
			t.Errorf("firstIdent(%q) = %q, want %q", in, got, want)
		}
	}
}
