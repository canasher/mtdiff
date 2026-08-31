package normalize

import (
	"bytes"
	"database/sql/driver"
	"testing"
	"time"

	"mtdiff/internal/conn"
)

func col(name, family string) conn.Column {
	return conn.Column{Name: name, Family: family, RawType: family}
}

func TestNormalizeDecimal(t *testing.T) {
	tests := map[string]string{
		"1.10":     "1.1",
		"0.10":     "0.1",
		"-0":       "0",
		"007":      "7",
		"123.450":  "123.45",
		"0":        "0",
		"-0.00":    "0",
		" 42 ":     "42",
		"0.0":      "0",
		"-5.2500":  "-5.25",
		"100.0100": "100.01",
		"":         "0",
		"+3.0":     "3",
	}
	for in, want := range tests {
		if got := normalizeDecimal(in); got != want {
			t.Errorf("normalizeDecimal(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTLVLayout(t *testing.T) {
	n := NewNormalizer([]conn.Column{col("a", conn.FamINT), col("b", conn.FamSTR)}, DefaultOptions())
	// NULL int + string "ab"
	got, err := n.Normalize([]driver.Value{nil, "ab"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{tagNULL, tagSTR, 0x00, 0x02, 'a', 'b'}
	if !bytes.Equal(got, want) {
		t.Errorf("NULL+string row = % x, want % x", got, want)
	}
	// int 42
	got, _ = n.Normalize([]driver.Value{int64(42), "x"}, nil)
	want = []byte{tagINT, 0x00, 0x02, '4', '2', tagSTR, 0x00, 0x01, 'x'}
	if !bytes.Equal(got, want) {
		t.Errorf("int row = % x, want % x", got, want)
	}
	// BLOB containing NUL must not be confused with NULL
	blob := NewNormalizer([]conn.Column{col("b", conn.FamBYTES)}, DefaultOptions())
	got, _ = blob.Normalize([]driver.Value{[]byte{0x00, 0x01}}, nil)
	want = []byte{tagBYTES, 0x00, 0x02, 0x00, 0x01}
	if !bytes.Equal(got, want) {
		t.Errorf("blob row = % x, want % x", got, want)
	}
}

func TestNULLVsEmptyVsZero(t *testing.T) {
	n := NewNormalizer([]conn.Column{col("s", conn.FamSTR), col("i", conn.FamINT)}, DefaultOptions())
	r1, _ := n.Normalize([]driver.Value{"", int64(0)}, nil)
	r2, _ := n.Normalize([]driver.Value{nil, int64(0)}, nil)
	r3, _ := n.Normalize([]driver.Value{"  ", int64(0)}, nil) // trailing spaces trimmed by default
	if bytes.Equal(r1, r2) {
		t.Error("empty string must differ from NULL")
	}
	if !bytes.Equal(r1, r3) {
		t.Error("default trim: '  ' must equal ''")
	}
	noTrim := NewNormalizer([]conn.Column{col("s", conn.FamSTR), col("i", conn.FamINT)}, Options{})
	r4, _ := noTrim.Normalize([]driver.Value{"  ", int64(0)}, nil)
	if bytes.Equal(r1, r4) {
		t.Error("no-trim: '  ' must differ from ''")
	}
}

func TestFormatFloat(t *testing.T) {
	if got := formatFloat(0.1, 0, 32); got != "0.1" {
		t.Errorf("bit-exact 0.1 = %q", got)
	}
	if got := formatFloat(-0, 0, 64); got != "0" {
		t.Errorf("-0 must render as 0, got %q", got)
	}
	// tolerance: two values in the same cell must render identically
	a := 1.0
	b := 1.00000000005
	if gotA, gotB := formatFloat(a, 1e-9, 64), formatFloat(b, 1e-9, 64); gotA != gotB {
		t.Errorf("tolerance grid mismatch: %q vs %q", gotA, gotB)
	}
	// values in different cells must differ
	c := 1.00000001
	if gotA, gotC := formatFloat(a, 1e-9, 64), formatFloat(c, 1e-9, 64); gotA == gotC {
		t.Errorf("different cells must differ: %q", gotA)
	}
	// no tolerance: bit-exact difference is preserved
	if gotA, gotB := formatFloat(a, 0, 64), formatFloat(b, 0, 64); gotA == gotB {
		t.Errorf("bit-exact mode must distinguish %v from %v", a, b)
	}
}

func TestTimeFormatting(t *testing.T) {
	if got := formatMySQLTime(90 * 60 * 1e9); got != "1:30:00" {
		t.Errorf("TIME = %q", got)
	}
	if got := formatMySQLTime(-1 * 60 * 1e9); got != "-0:01:00" {
		t.Errorf("negative TIME = %q", got)
	}
	if got := formatMySQLTime(1*60*1e9 + 500_000_000); got != "0:01:00.5" {
		t.Errorf("fractional TIME = %q", got)
	}
	if got := formatMySQLTime(0); got != "0:00:00" {
		t.Errorf("zero TIME = %q", got)
	}
}

// TestFractionalSecondCollisions covers the P1 regression: the old code
// TrimRight-ed the RAW microsecond count, so 100µs ("100"), 1ms ("1000"),
// 10ms ("10000") and 100ms ("100000") all trimmed to "1" and compared equal.
func TestFractionalSecondCollisions(t *testing.T) {
	cases := map[time.Duration]string{
		100 * time.Microsecond: "0:00:00.0001",
		time.Millisecond:       "0:00:00.001",
		10 * time.Millisecond:  "0:00:00.01",
		100 * time.Millisecond: "0:00:00.1",
	}
	seen := make(map[string]time.Duration, len(cases))
	for d, want := range cases {
		got := formatMySQLTime(d)
		if got != want {
			t.Errorf("TIME %v = %q, want %q", d, got, want)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("TIME %v and %v both render %q (collision)", prev, d, got)
		}
		seen[got] = d
	}
	// DATETIME fractional seconds: the e2e t_fracsec pair, 100ms vs 10ms.
	base := mkTime(2024, 1, 1, 0, 0, 0)
	if got := formatMySQLDateTime(base.Add(100 * time.Millisecond)); got != "2024-01-01 00:00:00.1" {
		t.Errorf("DATETIME 100ms = %q", got)
	}
	if got := formatMySQLDateTime(base.Add(10 * time.Millisecond)); got != "2024-01-01 00:00:00.01" {
		t.Errorf("DATETIME 10ms = %q", got)
	}
	// whole seconds are unaffected
	if got := formatMySQLDateTime(base); got != "2024-01-01 00:00:00" {
		t.Errorf("DATETIME whole second = %q", got)
	}
}

func TestFormatBit(t *testing.T) {
	if got := string(formatBit([]byte{1})); got != "1" {
		t.Errorf("bit(1)=1 = %q", got)
	}
	if got := string(formatBit([]byte{0})); got != "0" {
		t.Errorf("bit(1)=0 = %q", got)
	}
	// bit(8) with value 1 must equal bit(1) with value 1
	if got := string(formatBit([]byte{0x01})); got != "1" {
		t.Errorf("bit(8)=1 = %q, want equal to bit(1)=1", got)
	}
	// 0x0080 as a 16-bit value is 128 = "10000000" after leading-zero strip
	if got := string(formatBit([]byte{0x00, 0x80})); got != "10000000" {
		t.Errorf("bit(16)=0x0080 = %q", got)
	}
}

func TestNormalizeJSON(t *testing.T) {
	a, err := normalizeJSON([]byte(`{"b": 1.10, "a": {"z": 1, "y": [1, 2]}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := normalizeJSON([]byte(`{"a":{"y":[1,2],"z":1},"b":1.1}`))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("semantically equal JSON differ: %s vs %s", a, b)
	}
	c, _ := normalizeJSON([]byte(`{"a": 2}`))
	if bytes.Equal(a, c) {
		t.Error("different JSON must differ")
	}
}

func TestStringOptions(t *testing.T) {
	fold := NewNormalizer([]conn.Column{col("s", conn.FamSTR)}, Options{TrimTrailing: true, FoldCase: true})
	a, _ := fold.Normalize([]driver.Value{"AbC   "}, nil)
	b, _ := fold.Normalize([]driver.Value{"abc"}, nil)
	if !bytes.Equal(a, b) {
		t.Errorf("trim+fold mismatch: % x vs % x", a, b)
	}
	strict := NewNormalizer([]conn.Column{col("s", conn.FamSTR)}, Options{})
	c, _ := strict.Normalize([]driver.Value{"AbC"}, nil)
	d, _ := strict.Normalize([]driver.Value{"abc"}, nil)
	if bytes.Equal(c, d) {
		t.Error("default must be case-sensitive")
	}
}

func TestRowValueTypes(t *testing.T) {
	cols := []conn.Column{
		col("i", conn.FamINT), col("u", conn.FamUINT), col("d", conn.FamDECIMAL),
		col("f", conn.FamFLOAT), col("dt", conn.FamDATETIME), col("j", conn.FamJSON),
	}
	n := NewNormalizer(cols, DefaultOptions())
	_, err := n.Normalize([]driver.Value{
		int64(1), uint64(2), []byte("3.40"), float32(1.5),
		mkTime(2024, 1, 2, 3, 4, 5), []byte(`{"x":1}`),
	}, nil)
	if err != nil {
		t.Fatalf("valid row rejected: %v", err)
	}
	// wrong concrete type must surface
	_, err = n.Normalize([]driver.Value{
		"not-an-int", uint64(2), []byte("3.4"), float32(1.5),
		mkTime(2024, 1, 2, 3, 4, 5), []byte(`{"x":1}`),
	}, nil)
	if err == nil {
		t.Error("type mismatch must error")
	}
}
