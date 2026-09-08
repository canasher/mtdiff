package normalize

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math"
	"strconv"
	"testing"
	"time"

	"mtdiff/internal/conn"
	"mtdiff/internal/hash"
)

func col(name, family string) conn.Column {
	return conn.Column{Name: name, Family: family, RawType: family}
}

// nTime normalizes a single TIME column (the P1-5 driver-type tests).
var nTime = NewNormalizer([]conn.Column{col("v", conn.FamTIME)}, DefaultOptions())

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
	got, err := n.Normalize([]any{nil, "ab"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte{tagNULL}, tlv(tagSTR, 'a', 'b')...)
	if !bytes.Equal(got, want) {
		t.Errorf("NULL+string row = % x, want % x", got, want)
	}
	// numeric 42 (all numeric families share tagNUMERIC) + string "x"
	got, _ = n.Normalize([]any{int64(42), "x"}, nil)
	want = append(tlv(tagNUMERIC, '4', '2'), tlv(tagSTR, 'x')...)
	if !bytes.Equal(got, want) {
		t.Errorf("numeric row = % x, want % x", got, want)
	}
	// BLOB containing NUL must not be confused with NULL
	blob := NewNormalizer([]conn.Column{col("b", conn.FamBYTES)}, DefaultOptions())
	got, _ = blob.Normalize([]any{[]byte{0x00, 0x01}}, nil)
	want = tlv(tagBYTES, 0x00, 0x01)
	if !bytes.Equal(got, want) {
		t.Errorf("blob row = % x, want % x", got, want)
	}
}

// tlv renders one column value in the canonical layout (tag, 8-byte
// big-endian length, payload) — the test-side reference for the layout.
func tlv(tag byte, payload ...byte) []byte {
	out := make([]byte, 0, 1+8+len(payload))
	out = append(out, tag)
	var lenB [8]byte
	binary.BigEndian.PutUint64(lenB[:], uint64(len(payload)))
	out = append(out, lenB[:]...)
	return append(out, payload...)
}

// TestAppendTLVLargePayloadBoundary covers the P0-1 length boundary: the
// payload sizes around the old 16-bit limit (65534/65535/65536/65537)
// and 1 MiB. The encoding must stay injective there: at every size, two
// payloads of the SAME length that differ in one byte must encode
// differently, and the same payload must encode identically.
func TestAppendTLVLargePayloadBoundary(t *testing.T) {
	for _, size := range []int{65534, 65535, 65536, 65537, 1 << 20} {
		a := make([]byte, size)
		b := make([]byte, size)
		for i := range a {
			a[i] = byte(i % 251)
		}
		copy(b, a)
		b[size/2] ^= 0xFF // same length, one byte different
		ea, eb := appendTLV(nil, tagBYTES, a), appendTLV(nil, tagBYTES, b)
		if bytes.Equal(ea, eb) {
			t.Errorf("payload size %d: two distinct same-length payloads encode identically (TLV collision)", size)
		}
		if !bytes.Equal(appendTLV(nil, tagBYTES, a), ea) {
			t.Errorf("payload size %d: same payload must encode identically", size)
		}
		// adjacent sizes must not fold into each other either
		adj := make([]byte, size+1)
		copy(adj, a)
		if bytes.Equal(ea, appendTLV(nil, tagBYTES, adj)) {
			t.Errorf("payload size %d: length %d and %d encode identically", size, size, size+1)
		}
	}
}

// TestTLCollisionRegression is the deterministic P0-1 collision. Two
// columns, both FamBYTES, Q = 65533 'A' bytes: rowA puts Q plus a 3-byte
// trailer in the second column, rowB puts the 3-byte trailer plus Q in
// the first. Under the OLD 2-byte length, the 65536-byte payloads of the
// two rows both encode with length 0 (65536 mod 65536) and the whole
// rows are byte-identical — different logical rows, identical canonical
// bytes (a diff of a divergent table reports CONVERGED). The test first
// PROVES the legacy encoding collides (so the regression is meaningful),
// then pins the fixed encoding: distinct canonical bytes, and the two
// canonical rows hash to different digests through the real
// hash.Accumulator.
func TestTLCollisionRegression(t *testing.T) {
	q := bytes.Repeat([]byte{'A'}, 65533)
	rowA := []any{[]byte{}, append(append([]byte{}, q...), tagBYTES, 0, 0)}
	rowB := []any{append(append([]byte{tagBYTES, 0, 0}, q...), []byte{}...), []byte{}}
	// rowB's first column: trailer + q, exactly 65536 bytes
	if len(rowB[0].([]byte)) != 65536 || len(rowA[1].([]byte)) != 65536 {
		t.Fatalf("test shape: payloads must be 65536 bytes, got %d/%d", len(rowB[0].([]byte)), len(rowA[1].([]byte)))
	}

	// the legacy 2-byte-length encoding of the same rows: PROOF the
	// collision existed (both 65536-byte payloads encode with length 0)
	legacy := func(row []any) []byte {
		var out []byte
		for _, v := range row {
			p := v.([]byte)
			out = append(out, tagBYTES, byte(len(p)>>8), byte(len(p)))
			out = append(out, p...)
		}
		return out
	}
	if !bytes.Equal(legacy(rowA), legacy(rowB)) {
		t.Fatal("precondition: the legacy 2-byte-length encoding must collide for this shape (the regression it guards); revisit the test")
	}

	n := NewNormalizer([]conn.Column{col("a", conn.FamBYTES), col("b", conn.FamBYTES)}, DefaultOptions())
	ca, err := n.Normalize(rowA, nil)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := n.Normalize(rowB, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ca, cb) {
		t.Fatal("the fixed encoding must NOT collide: distinct rows produced identical canonical bytes")
	}
	// and through the real digest path (both ordered and unordered
	// accumulators): the chunk digests must differ
	da, db := hash.NewAccumulator(1, true), hash.NewAccumulator(1, false)
	da.AddRow(ca)
	db.AddRow(ca)
	// second accumulator pair for rowB
	ea, eb := hash.NewAccumulator(1, true), hash.NewAccumulator(1, false)
	ea.AddRow(cb)
	eb.AddRow(cb)
	if da.Digest() == ea.Digest() {
		t.Error("ordered digests must differ for the two rows")
	}
	if db.Digest() == eb.Digest() {
		t.Error("unordered digests must differ for the two rows")
	}
}

func TestNULLVsEmptyVsZero(t *testing.T) {
	n := NewNormalizer([]conn.Column{col("s", conn.FamSTR), col("i", conn.FamINT)}, DefaultOptions())
	r1, _ := n.Normalize([]any{"", int64(0)}, nil)
	r2, _ := n.Normalize([]any{nil, int64(0)}, nil)
	r3, _ := n.Normalize([]any{"  ", int64(0)}, nil) // trailing spaces trimmed by default
	if bytes.Equal(r1, r2) {
		t.Error("empty string must differ from NULL")
	}
	if !bytes.Equal(r1, r3) {
		t.Error("default trim: '  ' must equal ''")
	}
	noTrim := NewNormalizer([]conn.Column{col("s", conn.FamSTR), col("i", conn.FamINT)}, Options{})
	r4, _ := noTrim.Normalize([]any{"  ", int64(0)}, nil)
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

// TestFormatFloatLargeMagnitude pins the P0-4 fix: legitimate FINITE
// values must never saturate to an ±Inf sentinel under a finite
// tolerance. The old quantizer fell back to Copysign(Inf, v) once |v|/tol
// passed the int64 cell range, so 1e10 and 2e10 at tolerance 1e-9 both
// rendered "+Inf" and compared equal — a silent false identical on
// ordinary data. Distinct finite values must stay distinct at every
// magnitude; a value whose quantized cell overflows float64 keeps its
// exact (bit-distinguishing) rendering instead.
func TestFormatFloatLargeMagnitude(t *testing.T) {
	pairs := [][2]float64{
		{1e10, 2e10},
		{1e12, 2e12},
		{-1e10, -2e10},
		{1e10, -1e10},
		{9.1e9, 9.2e9},
		{9.1e9, 9.3e9},
		{9.2e9, 9.3e9},
		{math.MaxFloat64, math.Nextafter(math.MaxFloat64, 0)},
	}
	for _, tol := range []float64{1e-9, 1, 1e-3} {
		for _, p := range pairs {
			if gotA, gotB := formatFloat(p[0], tol, 64), formatFloat(p[1], tol, 64); gotA == gotB {
				t.Errorf("tol %v: %v and %v both render %q (false identical)", tol, p[0], p[1], gotA)
			}
		}
	}
	// FLOAT32 precision width
	n32 := NewNormalizer([]conn.Column{col("f", conn.FamFLOAT)}, Options{Tolerance: 1e-9})
	fa, err := n32.encodeValue(col("f", conn.FamFLOAT), float32(1e10))
	if err != nil {
		t.Fatal(err)
	}
	fb, err := n32.encodeValue(col("f", conn.FamFLOAT), float32(2e10))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(fa, fb) {
		t.Errorf("FLOAT32: % x and % x must differ", fa, fb)
	}
	// the within-cell promise still holds at large magnitude: two values
	// closer than the tolerance render identically
	if gotA, gotB := formatFloat(1e10, 1, 64), formatFloat(1e10+0.4, 1, 64); gotA != gotB {
		t.Errorf("values within the tolerance cell must agree: %q vs %q", gotA, gotB)
	}
	// and values further apart than the cell stay different
	if gotA, gotB := formatFloat(1e10, 1, 64), formatFloat(1e10+5, 1, 64); gotA == gotB {
		t.Errorf("values in different cells must differ, both %q", gotA)
	}
}

// pins the normalizer's defense (the second line after Config.Validate)
// against a non-finite tolerance. The test PROVES the hazard in-test: the
// legacy quantizer (pre-P0-4) collapses 1 and 999 to the same rendering
// under tol=+Inf (0*Inf = NaN for every input) — a divergent table would
// report CONVERGED. P0-4 removed the saturation so the renderer keeps
// exact values instead of inventing a sentinel, and the normalizer still
// refuses every non-finite tolerance on the normal path: an operator who
// configures tol=+Inf gets an explicit error, not silently different
// semantics.
func TestNonFiniteToleranceSilentFalseIdentical(t *testing.T) {
	// the LEGACY quantizer (pre-P0-4: v/tol, then N*tol with int64-cell
	// saturation): with tol=+Inf it is 0*Inf = NaN for every input, so
	// distinct values collapse into one rendering — the hazard the gate
	// defends against
	legacyQuantize := func(v, tol float64) float64 {
		q := v / tol
		if !math.IsInf(q, 0) && math.Abs(q) < 9.2e18 {
			return float64(int64(math.Round(q))) * tol
		}
		return math.Copysign(math.Inf(1), v)
	}
	if ra, rb := strconv.FormatFloat(legacyQuantize(1, math.Inf(1)), 'g', -1, 64),
		strconv.FormatFloat(legacyQuantize(999, math.Inf(1)), 'g', -1, 64); ra != rb {
		t.Fatalf("precondition: the legacy quantizer must collapse 1 and 999 under tol=+Inf (%q vs %q); revisit the test", ra, rb)
	}
	// and the normal path must NOT reach it: the normalizer refuses
	// every non-finite tolerance on both float families
	for _, tol := range []float64{math.Inf(1), math.Inf(-1), math.NaN()} {
		n := NewNormalizer([]conn.Column{col("f", conn.FamFLOAT), col("d", conn.FamDOUBLE)}, Options{Tolerance: tol})
		if _, err := n.Normalize([]any{float32(1), 1.0}, nil); err == nil {
			t.Errorf("tolerance %v must refuse to normalize (it would collapse every distinct value to the same rendering)", tol)
		}
		if _, err := n.encodeValue(col("d", conn.FamDOUBLE), 999.0); err == nil {
			t.Errorf("tolerance %v must refuse DOUBLE 999 (formatFloat(999, %v) would render the same as 1)", tol, tol)
		}
	}
	// legal tolerances keep working through the same path
	n := NewNormalizer([]conn.Column{col("d", conn.FamDOUBLE)}, Options{Tolerance: 1e-9})
	if _, err := n.Normalize([]any{1.0}, nil); err != nil {
		t.Errorf("finite tolerance must normalize: %v", err)
	}
	n0 := NewNormalizer([]conn.Column{col("d", conn.FamDOUBLE)}, Options{})
	if _, err := n0.Normalize([]any{1.0}, nil); err != nil {
		t.Errorf("zero tolerance (bit-exact) must normalize: %v", err)
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
// TestMySQLTimeDriverTypes covers the P1-5 fix: the go-sql-driver
// delivers a MySQL TIME column as the TEXT grammar (string on the binary
// protocol, []byte on the text protocol) — never as time.Duration. Both
// driver types must parse, render in MySQL's canonical form, and keep
// distinct values distinct.
func TestMySQLTimeDriverTypes(t *testing.T) {
	c := col("v", conn.FamTIME)
	cases := map[string]string{
		"00:00:00":         "0:00:00",
		"01:02:03":         "1:02:03",
		"-01:02:03":        "-1:02:03",
		"838:59:59":        "838:59:59",
		"-838:59:59":       "-838:59:59",
		"00:00:00.1":       "0:00:00.1",
		"00:00:00.01":      "0:00:00.01",
		"00:00:00.000001":  "0:00:00.000001",
		"1:02:03":          "1:02:03",
		"-0:01:00":         "-0:01:00",
		"0:00:00.123456":   "0:00:00.123456",
		"838:59:59.999999": "838:59:59.999999",
	}
	for in, want := range cases {
		for _, v := range []any{in, []byte(in)} { // string and []byte
			got, err := nTime.encodeValue(c, v)
			if err != nil {
				t.Errorf("parse %v (%T): %v", in, v, err)
				continue
			}
			if string(got) != want {
				t.Errorf("parse %v (%T) = %q, want %q", in, v, got, want)
			}
		}
	}
	// distinct values stay distinct (negative vs positive, fractional vs not)
	differ := [][2]string{{"00:00:00", "01:02:03"}, {"-01:02:03", "01:02:03"},
		{"00:00:00.1", "00:00:00.01"}, {"00:00:00.1", "00:00:01"}, {"-0:01:00", "0:01:00"}}
	for _, p := range differ {
		a, _ := nTime.encodeValue(c, p[0])
		b, _ := nTime.encodeValue(c, p[1])
		if bytes.Equal(a, b) {
			t.Errorf("%q and %q must differ, both % x", p[0], p[1], a)
		}
	}
	// the time.Duration path (tests / direct use) still works
	if got, err := nTime.encodeValue(c, time.Duration(90*60)*1e9); err != nil || string(got) != "1:30:00" {
		t.Errorf("duration path = %q, err %v", got, err)
	}
	// malformed input must surface, not be guessed
	for _, bad := range []any{"1:2", "1:02:03:04", "00:00:00.1234567", "12:00", "", "xx", "1:02:03.1234567"} {
		if _, err := nTime.encodeValue(c, bad); err == nil {
			t.Errorf("malformed TIME %v must error", bad)
		}
	}
}

// TestNumericTagEquality pins the P1-6 fix: the schema layer allows the
// numeric families (INT/UINT/DECIMAL/FLOAT/DOUBLE) to be compared across
// each other "after normalization", so equal VALUES across families must
// normalize to the SAME canonical bytes (one shared tagNUMERIC), while
// different values across families stay different.
func TestNumericTagEquality(t *testing.T) {
	n := NewNormalizer([]conn.Column{col("v", conn.FamINT)}, DefaultOptions())
	enc := func(fam string, v any) []byte {
		p, err := n.encodeValue(col("v", fam), v)
		if err != nil {
			t.Fatalf("encode %s %v: %v", fam, v, err)
		}
		return p
	}
	// INT 1 == UINT 1
	if a, b := enc(conn.FamINT, int64(1)), enc(conn.FamUINT, uint64(1)); !bytes.Equal(a, b) {
		t.Errorf("INT 1 vs UINT 1: % x vs % x", a, b)
	}
	// INT 1 == DECIMAL 1.00
	if a, b := enc(conn.FamINT, int64(1)), enc(conn.FamDECIMAL, []byte("1.00")); !bytes.Equal(a, b) {
		t.Errorf("INT 1 vs DECIMAL 1.00: % x vs % x", a, b)
	}
	// DECIMAL 1.0 == DOUBLE 1.0
	if a, b := enc(conn.FamDECIMAL, []byte("1.0")), enc(conn.FamDOUBLE, 1.0); !bytes.Equal(a, b) {
		t.Errorf("DECIMAL 1.0 vs DOUBLE 1.0: % x vs % x", a, b)
	}
	// INT 1 == FLOAT 1.0
	if a, b := enc(conn.FamINT, int64(1)), enc(conn.FamFLOAT, float32(1.0)); !bytes.Equal(a, b) {
		t.Errorf("INT 1 vs FLOAT 1.0: % x vs % x", a, b)
	}
	// different VALUES across families stay different
	if a, b := enc(conn.FamINT, int64(2)), enc(conn.FamDOUBLE, 1.0); bytes.Equal(a, b) {
		t.Errorf("INT 2 vs DOUBLE 1.0 must differ, both % x", a)
	}
	// a large UINT and the DOUBLE that rounds to a DIFFERENT value differ
	if a, b := enc(conn.FamUINT, uint64(18446744073709551615)), enc(conn.FamDOUBLE, 1.8e19); bytes.Equal(a, b) {
		t.Errorf("large UINT vs rounded DOUBLE must differ, both % x", a)
	}
}

// TestNumericCrossFamilyCanonicalForm pins the P1-3 fix: the shared
// tagNUMERIC only means something if the payload GRAMMAR is shared too.
// The old payloads disagreed on notation (INT 1000000 = "1000000" but
// DOUBLE 1000000 = "1e+06" from the 'g' round-trip; DECIMAL 0.00001 =
// "0.00001" but DOUBLE 0.00001 = "1e-05"), so the declared cross-family
// compatibility was still a fiction for every value whose shortest
// float rendering is not plain. Every numeric family now passes its
// exact value through the one canonical decimal grammar (INT/UINT/DECIMAL
// exact, never via float64; FLOAT/DOUBLE after tolerance quantization):
// the same value renders identically in any family, distinct values
// always render differently.
func TestNumericCrossFamilyCanonicalForm(t *testing.T) {
	n := NewNormalizer([]conn.Column{col("v", conn.FamINT)}, DefaultOptions())
	enc := func(fam string, v any) []byte {
		p, err := n.encodeValue(col("v", fam), v)
		if err != nil {
			t.Fatalf("encode %s %v: %v", fam, v, err)
		}
		return p
	}
	same := func(want string, fam string, val any) {
		got := enc(fam, val)
		if string(got) != want {
			t.Errorf("%s %v: canonical payload %q, want %q", fam, val, got, want)
		}
	}
	// INT 1000000 == UINT 1000000 == DECIMAL 1000000.000 == DOUBLE 1000000.0
	same("1000000", conn.FamINT, int64(1000000))
	same("1000000", conn.FamUINT, uint64(1000000))
	same("1000000", conn.FamDECIMAL, []byte("1000000.000"))
	same("1000000", conn.FamDOUBLE, float64(1000000))
	same("1000000", conn.FamFLOAT, float32(1000000))
	if a, b := enc(conn.FamINT, int64(1000000)), enc(conn.FamDOUBLE, float64(1000000)); !bytes.Equal(a, b) {
		t.Errorf("INT 1000000 vs DOUBLE 1000000: % x vs % x", a, b)
	}
	// DECIMAL 0.00001 == DOUBLE 0.00001 (the 'g' rendering 1e-05 used to
	// collide with nothing but the decimal's plain form)
	same("0.00001", conn.FamDECIMAL, []byte("0.00001"))
	same("0.00001", conn.FamDECIMAL, []byte("0.0000100"))
	same("0.00001", conn.FamDOUBLE, 0.00001)
	if a, b := enc(conn.FamDECIMAL, []byte("0.0000100")), enc(conn.FamDOUBLE, 0.00001); !bytes.Equal(a, b) {
		t.Errorf("DECIMAL 0.00001 vs DOUBLE 0.00001: % x vs % x", a, b)
	}
	// INT 1 == DOUBLE 1.0
	if a, b := enc(conn.FamINT, int64(1)), enc(conn.FamDOUBLE, 1.0); !bytes.Equal(a, b) {
		t.Errorf("INT 1 vs DOUBLE 1.0: % x vs % x", a, b)
	}
	// negatives across families
	same("-1000000", conn.FamINT, int64(-1000000))
	same("-1000000", conn.FamDECIMAL, []byte("-1000000.00"))
	same("-1000000", conn.FamDOUBLE, float64(-1000000))
	// different VALUES stay different across the shared grammar
	if a, b := enc(conn.FamINT, int64(1000000)), enc(conn.FamDOUBLE, float64(1000001)); bytes.Equal(a, b) {
		t.Errorf("INT 1000000 vs DOUBLE 1000001 must differ, both % x", a)
	}
	// precision boundary: DOUBLE cannot represent 9007199254740993 — the
	// exact INT and the DOUBLE that rounds to ...92 must stay different
	// (the INT stays EXACT: it never passes through float64)
	if a, b := enc(conn.FamINT, int64(9007199254740993)), enc(conn.FamDOUBLE, float64(9007199254740992)); bytes.Equal(a, b) {
		t.Errorf("INT 9007199254740993 vs DOUBLE 9007199254740992 must differ, both % x", a)
	}
	// the maximum UINT (20 digits — right at the plain-render limit)
	// must not overflow the canonicalizer and must agree across shapes
	maxU := enc(conn.FamUINT, uint64(18446744073709551615))
	if string(maxU) != "18446744073709551615" {
		t.Errorf("max UINT canonical = %q, want the exact 20-digit value", maxU)
	}
	if !bytes.Equal(maxU, enc(conn.FamUINT, "18446744073709551615")) {
		t.Errorf("max UINT: uint64 shape vs decimal-string shape disagree: % x", maxU)
	}
}

// TestCanonicalNumberGrammar pins the shared decimal grammar itself: the
// plain/scientific switch, the exactness invariants, the sentinel
// pass-through, and the refusal of anything that is not a decimal.
func TestCanonicalNumberGrammar(t *testing.T) {
	eq := func(tok, want string) {
		got, err := canonicalNumber(tok)
		if err != nil {
			t.Fatalf("canonicalNumber(%q): %v", tok, err)
		}
		if got != want {
			t.Errorf("canonicalNumber(%q) = %q, want %q", tok, got, want)
		}
	}
	// equal values, any notation, one rendering
	eq("1", "1")
	eq("1.0", "1")
	eq("1.00", "1")
	eq("1e0", "1")
	eq("1000000", "1000000")
	eq("1e6", "1000000")
	eq("1.0e6", "1000000")
	eq("0.00001", "0.00001")
	eq("1e-5", "0.00001")
	eq("0.0000100", "0.00001")
	eq("-1e6", "-1000000")
	eq("-0", "0")
	// beyond the plain limit the rendering is scientific but compact
	eq("1e100000000", "1e+100000000")
	if got, _ := canonicalNumber("123456789012345678901234567890"); len(got) > 40 {
		t.Errorf("27-digit integer expanded to %d bytes: %q", len(got), got)
	}
	// distinct values stay distinct at any magnitude
	differ := func(a, b string) {
		x, errA := canonicalNumber(a)
		y, errB := canonicalNumber(b)
		if errA != nil || errB != nil {
			t.Fatalf("canonicalNumber(%q/%q): %v %v", a, b, errA, errB)
		}
		if x == y {
			t.Errorf("distinct values must differ: %q and %q both %q", a, b, x)
		}
	}
	differ("9007199254740992", "9007199254740993")
	differ("123456789012345678901234567890", "123456789012345678901234567891")
	differ("1e100000000", "2e100000000")
	// the float sentinels pass through unchanged (MySQL 8.0 DOUBLE can
	// store them; no canonical decimal exists for them)
	eq("NaN", "NaN")
	eq("+Inf", "+Inf")
	eq("-Inf", "-Inf")
	// anything else is not a decimal: an error, never a guess
	for _, bad := range []string{"", "abc", "1x", "1.2.3", "e5", "1e", "0x10", "--1", "1..2"} {
		if _, err := canonicalNumber(bad); err == nil {
			t.Errorf("canonicalNumber(%q) must error", bad)
		}
	}
}

// TestCanonicalNumberNegativeExponents pins the bounded rendering of large
// NEGATIVE exponents (P3): a value like 1e-100000000 must stay the compact
// scientific "1e-100000000" — never expand into an O(|exponent|) string of
// zeros — and an exponent that overflows the Go int must fail closed with an
// error rather than panic, OOM, or mis-normalize.
func TestCanonicalNumberNegativeExponents(t *testing.T) {
	// the value renders to EXACTLY this canonical text AND stays compact
	// (the length is bounded by the significant digits + exponent digits,
	// not the exponent's magnitude)
	compact := func(tok, want string) {
		got, err := canonicalNumber(tok)
		if err != nil {
			t.Fatalf("canonicalNumber(%q): %v", tok, err)
		}
		if got != want {
			t.Errorf("canonicalNumber(%q) = %q, want %q", tok, got, want)
		}
		if len(got) >= 128 {
			t.Errorf("canonicalNumber(%q) rendered %d bytes, want < 128: %q", tok, len(got), got)
		}
	}
	// short negative exponents keep their plain form
	compact("1e-5", "0.00001")
	compact("0.00001", "0.00001")
	// beyond the plain limit the value switches to the compact scientific form
	compact("1e-20", "1e-20")
	compact("1e-1000", "1e-1000")
	compact("1e-100000000", "1e-100000000")
	compact("-1e-100000000", "-1e-100000000")
	// equal values reduce to one canonical text regardless of the exponent
	// spelling (10e-100000001 == 1e-100000000)
	compact("10e-100000001", "1e-100000000")
	// distinct values at the same extreme magnitude stay distinct
	x, errA := canonicalNumber("1e-100000000")
	y, errB := canonicalNumber("2e-100000000")
	if errA != nil || errB != nil {
		t.Fatalf("canonicalNumber extreme: %v %v", errA, errB)
	}
	if x == y {
		t.Errorf("distinct extreme values must differ: %q and %q both %q", "1e-100000000", "2e-100000000", x)
	}
	// an exponent that does not fit a Go int must fail closed with an error
	// (not panic, OOM, or a wrong normalization)
	for _, huge := range []string{"1e999999999999999999999999", "1e-999999999999999999999999"} {
		if _, err := canonicalNumber(huge); err == nil {
			t.Errorf("canonicalNumber(%q) must error (exponent overflow), got a value", huge)
		}
	}
}

// TestUINTDriverShapes pins the three driver shapes a 64-bit UNSIGNED value
// can arrive in (P1-6, found by the e2e): the TEXT protocol yields uint64,
// the BINARY protocol yields int64 while the value fits the signed range
// and a decimal STRING beyond math.MaxInt64. All three are the same value
// and must normalize identically (and agree with the INT family); anything
// else must surface, not be guessed.
func TestUINTDriverShapes(t *testing.T) {
	n := NewNormalizer([]conn.Column{col("v", conn.FamUINT)}, DefaultOptions())
	enc := func(fam string, v any) []byte {
		p, err := n.encodeValue(col("v", fam), v)
		if err != nil {
			t.Fatalf("encode %s %v: %v", fam, v, err)
		}
		return p
	}
	// all three driver shapes of the value 1 agree with each other and
	// with the INT family
	shapes := []any{int64(1), uint64(1), "1"}
	first := enc(conn.FamUINT, shapes[0])
	for _, s := range shapes[1:] {
		if !bytes.Equal(first, enc(conn.FamUINT, s)) {
			t.Errorf("UINT driver shapes disagree: %v", s)
		}
	}
	if !bytes.Equal(first, enc(conn.FamINT, int64(1))) {
		t.Errorf("UINT 1 (binary int64 shape) must equal INT 1")
	}
	// beyond math.MaxInt64: the string rendering and the uint64 agree
	if a, b := enc(conn.FamUINT, "18446744073709551615"), enc(conn.FamUINT, uint64(18446744073709551615)); !bytes.Equal(a, b) {
		t.Errorf("overflow-string UINT vs uint64 must agree: % x vs % x", a, b)
	}
	// malformed or wrong-typed values must error, not be guessed
	for _, bad := range []any{"12x", "-1", "", "1.5", float64(1), []byte("5"), []byte(nil)} {
		if _, err := n.encodeValue(col("v", conn.FamUINT), bad); err == nil {
			t.Errorf("FamUINT %v (%T) must error", bad, bad)
		}
	}
}

// TestNormalizeJSONExact pins the P1-7 fix: JSON numbers are canonicalized
// exactly (no float64 round trip). Equal values agree; distinct values —
// including beyond float64's ~17 significant digits and at huge exponents
// — stay distinct, without expanding into a megabyte string.
func TestNormalizeJSONExact(t *testing.T) {
	// 1, 1.0, 1.00, 1e0 all collapse to the same canonical form
	ones := []string{`{"n":1}`, `{"n":1.0}`, `{"n":1.00}`, `{"n":1e0}`}
	first, err := normalizeJSON([]byte(ones[0]))
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range ones[1:] {
		got, err := normalizeJSON([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, got) {
			t.Errorf("equal JSON numbers differ: %s vs %s", first, got)
		}
	}
	// beyond float64 precision: 9007199254740992 vs 9007199254740993
	a, _ := normalizeJSON([]byte(`{"n":9007199254740992}`))
	b, _ := normalizeJSON([]byte(`{"n":9007199254740993}`))
	if bytes.Equal(a, b) {
		t.Errorf("float64-collapsing integers must stay distinct: %s", a)
	}
	// 27-digit integers
	c, _ := normalizeJSON([]byte(`{"n":123456789012345678901234567890}`))
	d, _ := normalizeJSON([]byte(`{"n":123456789012345678901234567891}`))
	if bytes.Equal(c, d) {
		t.Errorf("27-digit integers must stay distinct: %s", c)
	}
	// huge exponent: distinct, and NOT expanded into a huge string
	e, _ := normalizeJSON([]byte(`{"n":1e100000000}`))
	f, _ := normalizeJSON([]byte(`{"n":2e100000000}`))
	if bytes.Equal(e, f) {
		t.Errorf("1e100000000 vs 2e100000000 must differ: %s", e)
	}
	if len(e) > 1000 {
		t.Errorf("huge exponent expanded to %d bytes (must stay compact): %s...", len(e), e[:min(40, len(e))])
	}
	// -0 collapses to 0
	g, _ := normalizeJSON([]byte(`{"n":-0}`))
	h, _ := normalizeJSON([]byte(`{"n":0}`))
	if !bytes.Equal(g, h) {
		t.Errorf("-0 must equal 0: %s vs %s", g, h)
	}
	// existing behavior (key sorting / nesting / arrays) must not regress
	x, _ := normalizeJSON([]byte(`{"b": 1.10, "a": {"z": 1, "y": [1, 2]}}`))
	y, _ := normalizeJSON([]byte(`{"a":{"y":[1,2],"z":1},"b":1.1}`))
	if !bytes.Equal(x, y) {
		t.Errorf("key-sorted/nested JSON must still match: %s vs %s", x, y)
	}
}

// TestNormalizeJSONPreservesNumberType pins the P0-1 fix: --normalize-json
// must preserve the JSON TYPE of every value. A JSON number is not a JSON
// string: the old code canonicalized a number into a plain Go string, and
// json.Marshal then rendered it WITH quotes, so {"n":1} and {"n":"1"} both
// normalized to {"n":"1"} — a deterministic silent false identical between
// a number and a string (the same trap, one level up, as the float64
// collapse the exact-number work fixed).
func TestNormalizeJSONPreservesNumberType(t *testing.T) {
	// normEq(a, b): the normalized documents are byte-equal
	normEq := func(a, b string) (bool, string, string) {
		x, err := normalizeJSON([]byte(a))
		if err != nil {
			t.Fatalf("normalize %s: %v", a, err)
		}
		y, err := normalizeJSON([]byte(b))
		if err != nil {
			t.Fatalf("normalize %s: %v", b, err)
		}
		return bytes.Equal(x, y), string(x), string(y)
	}
	// TYPE SEPARATION: a number never equals the string of its digits
	mustDiffer := [][2]string{
		{`{"n":1}`, `{"n":"1"}`},
		{`{"n":1.0}`, `{"n":"1"}`},
		{`{"n":-5}`, `{"n":"-5"}`},
		{`{"n":0.25}`, `{"n":"0.25"}`},
		{`{"a":{"n":1}}`, `{"a":{"n":"1"}}`},
		{`[1]`, `["1"]`},
		{`{"a":[1,2]}`, `{"a":["1","2"]}`},
		// the other JSON types are already distinct, but pin them as
		// type invariants too: booleans and null are not their names
		{`{"n":true}`, `{"n":"true"}`},
		{`{"n":false}`, `{"n":"false"}`},
		{`{"n":null}`, `{"n":"null"}`},
		{`[null]`, `["null"]`},
	}
	for i, pair := range mustDiffer {
		eq, x, y := normEq(pair[0], pair[1])
		if eq {
			t.Errorf("case %d: distinct JSON types must differ: %s vs %s", i, x, y)
		}
	}
	// the normalized document must re-parse as JSON with the ORIGINAL
	// types: a number comes back a json.Number, a string a string
	roundTripTypes := func(doc string, wantNumber bool) {
		out, err := normalizeJSON([]byte(doc))
		if err != nil {
			t.Fatalf("normalize %s: %v", doc, err)
		}
		dec := json.NewDecoder(bytes.NewReader(out))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("normalized output %s is not valid JSON: %v", out, err)
		}
		_, ok := v.(map[string]any)["n"].(json.Number)
		s, isStr := v.(map[string]any)["n"].(string)
		if wantNumber && !ok {
			t.Errorf("normalized %s: the number lost its type: %s", doc, out)
		}
		if !wantNumber && !isStr {
			t.Errorf("normalized %s: the string lost its type: %s (got %v)", doc, out, s)
		}
	}
	roundTripTypes(`{"n":1}`, true)
	roundTripTypes(`{"n":"1"}`, false)
	roundTripTypes(`{"n":1e100000000}`, true)
}

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
	if got := FormatDateTime(base.Add(100 * time.Millisecond)); got != "2024-01-01 00:00:00.1" {
		t.Errorf("DATETIME 100ms = %q", got)
	}
	if got := FormatDateTime(base.Add(10 * time.Millisecond)); got != "2024-01-01 00:00:00.01" {
		t.Errorf("DATETIME 10ms = %q", got)
	}
	// microsecond precision (DATETIME(6)): the literal path must keep all 6 digits
	if got := FormatDateTime(base.Add(123456 * time.Microsecond)); got != "2024-01-01 00:00:00.123456" {
		t.Errorf("DATETIME 123456us = %q", got)
	}
	// whole seconds are unaffected
	if got := FormatDateTime(base); got != "2024-01-01 00:00:00" {
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
	a, _ := fold.Normalize([]any{"AbC   "}, nil)
	b, _ := fold.Normalize([]any{"abc"}, nil)
	if !bytes.Equal(a, b) {
		t.Errorf("trim+fold mismatch: % x vs % x", a, b)
	}
	strict := NewNormalizer([]conn.Column{col("s", conn.FamSTR)}, Options{})
	c, _ := strict.Normalize([]any{"AbC"}, nil)
	d, _ := strict.Normalize([]any{"abc"}, nil)
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
	_, err := n.Normalize([]any{
		int64(1), uint64(2), []byte("3.40"), float32(1.5),
		mkTime(2024, 1, 2, 3, 4, 5), []byte(`{"x":1}`),
	}, nil)
	if err != nil {
		t.Fatalf("valid row rejected: %v", err)
	}
	// wrong concrete type must surface
	_, err = n.Normalize([]any{
		"not-an-int", uint64(2), []byte("3.4"), float32(1.5),
		mkTime(2024, 1, 2, 3, 4, 5), []byte(`{"x":1}`),
	}, nil)
	if err == nil {
		t.Error("type mismatch must error")
	}
}
