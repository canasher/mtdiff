package conn

import (
	"strings"
	"testing"

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
	ep := config.Endpoint{Host: "10.0.0.1", Port: 3307, User: "root", Password: "s3:cret", Database: "dbA"}
	dsn := BuildDSN(ep, 0)
	want := "root:s3%3Acret@tcp(10.0.0.1:3307)/dbA?parseTime=true&loc=UTC&charset=utf8mb4&timeout=10s&readTimeout=10m&writeTimeout=10s"
	if dsn != want {
		t.Errorf("BuildDSN = %q, want %q", dsn, want)
	}
	ep.Password = ""
	if got := BuildDSN(ep, 64<<20); !strings.Contains(got, "root@tcp(") || !strings.Contains(got, "&maxAllowedPacket=67108864") {
		t.Errorf("BuildDSN no-password/maxPacket = %q", got)
	}
	// A special character in the user name must be escaped, not leak raw.
	ep2 := config.Endpoint{Host: "h", Port: 1, User: "u@x", Password: "", Database: "d"}
	if got := BuildDSN(ep2, 0); strings.Contains(got, "u@x@") {
		t.Errorf("unescaped user in DSN: %q", got)
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
