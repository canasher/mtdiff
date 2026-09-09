package normalize

import (
	"database/sql/driver"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"

	"mtdiff/internal/conn"
)

// encodeValue converts one driver value to its canonical payload according to
// the column metadata. The driver's concrete Go type is checked against the
// column family: the go-sql-driver returns a fixed type per MySQL type, so a
// mismatch means something is wrong and must surface, not be guessed.
func (n *Normalizer) encodeValue(c conn.Column, v driver.Value) ([]byte, error) {
	switch c.Family {
	case conn.FamINT:
		i, ok := v.(int64)
		if !ok {
			return nil, fmt.Errorf("expected int64, got %T", v)
		}
		// exact decimal through the shared canonical grammar: an int64
		// renders in <= 19 digits without trailing zeros, so the
		// canonicalizer is an identity here — but the SAME grammar the
		// other numeric families use, so INT 1000000 and DOUBLE 1e+06
		// both end up "1000000" (see canonicalNumber)
		out, err := canonicalNumber(strconv.FormatInt(i, 10))
		if err != nil {
			return nil, err
		}
		return []byte(out), nil
	case conn.FamUINT:
		// The driver delivers a 64-bit UNSIGNED value in THREE shapes
		// depending on the protocol path: the text protocol yields
		// uint64; the binary protocol yields int64 while the value still
		// fits the signed range (<= math.MaxInt64) and a decimal STRING
		// beyond it (the driver renders those instead of fitting them).
		// All three are the same value; rendering is the exact decimal
		// text through the shared canonical grammar either way (a uint64
		// renders in <= 20 digits: the canonicalizer never widens it),
		// so cross-family numeric equality (tagNUMERIC) holds.
		switch u := v.(type) {
		case int64:
			out, err := canonicalNumber(strconv.FormatInt(u, 10))
			if err != nil {
				return nil, err
			}
			return []byte(out), nil
		case uint64:
			out, err := canonicalNumber(strconv.FormatUint(u, 10))
			if err != nil {
				return nil, err
			}
			return []byte(out), nil
		case string:
			if !isDecimalUint(u) {
				return nil, fmt.Errorf("expected uint64, got %T", v)
			}
			out, err := canonicalNumber(u)
			if err != nil {
				return nil, err
			}
			return []byte(out), nil
		default:
			return nil, fmt.Errorf("expected uint64, got %T", v)
		}
	case conn.FamDECIMAL:
		s, ok := asString(v)
		if !ok {
			return nil, fmt.Errorf("expected decimal bytes, got %T", v)
		}
		// exact decimal through the shared canonical grammar — NEVER via
		// float64: DECIMAL 0.00001 and DOUBLE 0.00001 both end up
		// "0.00001", and a DECIMAL beyond ~17 digits stays exact
		out, err := canonicalNumber(s)
		if err != nil {
			return nil, err
		}
		return []byte(out), nil
	case conn.FamFLOAT:
		if !usableTolerance(n.opts.Tolerance) {
			return nil, toleranceRefused(n.opts.Tolerance)
		}
		// The driver delivers FLOAT as float32 or float64 depending on
		// version/parameters; accept both (float32 is exact in float64).
		var f64 float64
		switch f := v.(type) {
		case float32:
			f64 = float64(f)
		case float64:
			f64 = f
		default:
			return nil, fmt.Errorf("expected float, got %T", v)
		}
		// tolerance quantization (unchanged — see formatFloat) →
		// shortest round-trip decimal → the SHARED canonical grammar:
		// DOUBLE 1e+06 and INT 1000000 both end up "1000000". The
		// quantization stays exact-or-keep (no saturation); only the
		// final rendering is unified across the numeric families.
		q := formatFloat(f64, n.opts.Tolerance, 32)
		out, err := canonicalNumber(q)
		if err != nil {
			return nil, err
		}
		return []byte(out), nil
	case conn.FamDOUBLE:
		if !usableTolerance(n.opts.Tolerance) {
			return nil, toleranceRefused(n.opts.Tolerance)
		}
		f, ok := v.(float64)
		if !ok {
			return nil, fmt.Errorf("expected float64, got %T", v)
		}
		q := formatFloat(f, n.opts.Tolerance, 64)
		out, err := canonicalNumber(q)
		if err != nil {
			return nil, err
		}
		return []byte(out), nil
	case conn.FamDATE:
		t, ok := v.(time.Time)
		if !ok {
			return nil, fmt.Errorf("expected time.Time, got %T", v)
		}
		return []byte(t.Format("2006-01-02")), nil
	case conn.FamTIME:
		// The driver delivers a MySQL TIME column as the TEXT grammar
		// [-]HHH:MM:SS[.ffffff] — a string on the binary protocol, []byte
		// on the text protocol (it has no time.Time equivalent, so there
		// is no time.Duration path in production); a duration is accepted
		// for tests and direct use.
		switch t := v.(type) {
		case time.Duration:
			return []byte(formatMySQLTime(t)), nil
		case string:
			d, err := parseMySQLTime(t)
			if err != nil {
				return nil, err
			}
			return []byte(formatMySQLTime(d)), nil
		case []byte:
			d, err := parseMySQLTime(string(t))
			if err != nil {
				return nil, err
			}
			return []byte(formatMySQLTime(d)), nil
		default:
			return nil, fmt.Errorf("expected MySQL TIME text (string/[]byte) or time.Duration, got %T", v)
		}
	case conn.FamDATETIME, conn.FamTIMESTAMP:
		t, ok := v.(time.Time)
		if !ok {
			return nil, fmt.Errorf("expected time.Time, got %T", v)
		}
		return []byte(FormatDateTime(t)), nil
	case conn.FamYEAR:
		i, ok := v.(int64)
		if !ok {
			return nil, fmt.Errorf("expected int64, got %T", v)
		}
		return strconv.AppendInt(nil, i, 10), nil
	case conn.FamENUM, conn.FamSET:
		s, ok := asString(v)
		if !ok {
			return nil, fmt.Errorf("expected string bytes, got %T", v)
		}
		return []byte(n.stringOpts(s)), nil
	case conn.FamSTR:
		// The driver may deliver string or []byte depending on the exact
		// column type (VARCHAR often comes back as []byte); accept both.
		s, ok := asString(v)
		if !ok {
			return nil, fmt.Errorf("expected string bytes, got %T", v)
		}
		return []byte(n.stringOpts(s)), nil
	case conn.FamBYTES:
		b, ok := v.([]byte)
		if !ok {
			return nil, fmt.Errorf("expected []byte, got %T", v)
		}
		return b, nil
	case conn.FamJSON:
		b, ok := asBytes(v)
		if !ok {
			return nil, fmt.Errorf("expected json bytes, got %T", v)
		}
		if n.opts.NormalizeJSON {
			return normalizeJSON(b)
		}
		return b, nil
	case conn.FamBIT:
		b, ok := v.([]byte)
		if !ok {
			return nil, fmt.Errorf("expected bit bytes, got %T", v)
		}
		return formatBit(b), nil
	}
	return nil, fmt.Errorf("unknown column family %q", c.Family)
}

// stringOpts applies the configured string options (trim / case-fold).
func (n *Normalizer) stringOpts(s string) string {
	if n.opts.TrimTrailing {
		s = strings.TrimRightFunc(s, unicode.IsSpace)
	}
	if n.opts.FoldCase {
		s = strings.ToLower(s)
	}
	return s
}

// usableTolerance reports a tolerance the float quantizer may use: exactly
// 0 (bit-exact) or a finite positive value. This is the SECOND line of
// defense against a silent false identical — the FIRST is Config.Validate,
// which refuses NaN/±Inf/negative at the config entry. With tol=+Inf the
// quantization is v/Inf = 0 and 0*Inf = NaN, so every DISTINCT float value
// would normalize to the same rendering "NaN" and compare equal: a full
// diff of a divergent table reports CONVERGED. A non-finite tolerance must
// therefore refuse to normalize (an error the caller surfaces), never fall
// back to a comparison the operator did not configure.
func usableTolerance(tol float64) bool {
	// NaN fails both comparisons (NaN == 0 and NaN > 0 are false);
	// -Inf and negative finite values fail tol > 0; +Inf fails IsInf.
	return tol == 0 || (tol > 0 && !math.IsInf(tol, 0))
}

func toleranceRefused(tol float64) error {
	return fmt.Errorf("tolerance %v is not usable (it must be 0 or a finite positive value): refusing to normalize float values — a non-finite tolerance would collapse every distinct value to the same rendering", tol)
}

// formatFloat renders a float canonically. Without tolerance the shortest
// round-trip representation is used (bit-exact comparison). With tolerance
// the value is quantized to the grid first: every value landing in the same
// cell produces the same float64 (N * tol), so the rendering is identical
// for all in-tolerance values.
//
// Precondition (enforced by the callers, see usableTolerance): tol is 0 or
// a finite positive value. A non-finite tol must NOT reach this function:
// with tol=+Inf it would quantize every distinct value to 0*Inf = NaN and
// render them all identically.
//
// Large-magnitude quantization is exact-or-keep, never saturating: the
// quantized cell N*tol can OVERFLOW float64 (a value of 1e300 against a
// tolerance of 1e10), and the old code replaced such values with a
// Copysign(Inf) sentinel — two distinct LEGITIMATE values (1e10 and 2e10
// at tolerance 1e-9) both rendered "+Inf" and compared equal: a silent
// false identical on ordinary finite data. A value whose cell cannot be
// represented stays at its exact (bit-distinguishing) rendering instead:
// keeping the exact value can only make MORE values compare different,
// never fewer — the conservative direction.
func formatFloat(v, tol float64, prec int) string {
	if math.IsNaN(v) {
		return "NaN" // MySQL has no NaN; defensive only
	}
	if tol > 0 {
		q := v / tol
		if !math.IsInf(q, 0) && !math.IsNaN(q) {
			cand := math.Round(q) * tol
			if !math.IsInf(cand, 0) && !math.IsNaN(cand) {
				v = cand
			}
			// the grid overflows float64 at this magnitude: keep v exact
		}
	}
	if v == 0 {
		v = 0 // normalize -0
	}
	return strconv.FormatFloat(v, 'g', -1, prec)
}

// parseMySQLTime parses MySQL's TIME text grammar [-]HHH:MM:SS[.ffffff] —
// what the driver returns for a TIME column (string/[]byte, never a Go
// duration) — into a duration. The hour field has 1-3 digits (MySQL's range
// is -838:59:59.999999..838:59:59.999999), minutes and seconds two; the
// fractional part has 1-6 digits (padded right to microseconds).
func parseMySQLTime(s string) (time.Duration, error) {
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var intPart, frac string
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i+1:]
		if len(frac) == 0 || len(frac) > 6 {
			return 0, fmt.Errorf("invalid MySQL TIME value %q: fraction must have 1-6 digits", s)
		}
	} else {
		intPart = s
	}
	parts := strings.Split(intPart, ":")
	if len(parts) != 3 || len(parts[0]) < 1 || len(parts[0]) > 3 ||
		len(parts[1]) != 2 || len(parts[2]) != 2 {
		return 0, fmt.Errorf("invalid MySQL TIME value %q: want [-]HHH:MM:SS[.ffffff]", s)
	}
	for _, r := range parts[0] + parts[1] + parts[2] + frac {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid MySQL TIME value %q", s)
		}
	}
	h, _ := strconv.Atoi(parts[0])
	m, _ := strconv.Atoi(parts[1])
	sec, _ := strconv.Atoi(parts[2])
	if m > 59 || sec > 59 {
		return 0, fmt.Errorf("invalid MySQL TIME value %q: minute/second out of range", s)
	}
	d := (time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec)*time.Second)
	if frac != "" {
		f := frac + strings.Repeat("0", 6-len(frac))
		fv, _ := strconv.Atoi(f)
		d += time.Duration(fv) * time.Microsecond
	}
	if neg {
		d = -d
	}
	return d, nil
}

// formatMySQLTime renders MySQL TIME (a duration) as H:MM:SS[.f], matching
// MySQL's own canonical form including the negative range (-838:59:59).
func formatMySQLTime(d time.Duration) string {
	neg := d < 0
	if neg {
		d = -d
	}
	h := d / time.Hour
	m := (d % time.Hour) / time.Minute
	s := (d % time.Minute) / time.Second
	f := (d % time.Second) / time.Microsecond
	out := fmt.Sprintf("%d:%02d:%02d", h, m, s)
	if f != 0 {
		// Zero-pad the microsecond count to 6 digits before trimming
		// trailing zeros: on the raw count, 1ms (1000), 10ms (10000) and
		// 100ms (100000) all trim to "1" and would compare equal.
		frac := fmt.Sprintf("%06d", f)
		out += "." + strings.TrimRight(frac, "0")
	}
	if neg {
		out = "-" + out
	}
	return out
}

// FormatDateTime renders as Y-m-d H:i:s[.f]; fractional seconds are
// present only when non-zero, so equal instants render identically across
// different column precisions. It keeps up to 6 fractional digits (MySQL's
// maximum precision): a lossy rendering here would also corrupt sync
// writes and chunk-boundary literals for sub-second-precision columns.
func FormatDateTime(t time.Time) string {
	out := t.Format("2006-01-02 15:04:05")
	if ns := t.Nanosecond(); ns != 0 {
		// Zero-pad the microsecond count to 6 digits before trimming
		// trailing zeros (see formatMySQLTime for the collision it avoids).
		frac := fmt.Sprintf("%06d", ns/1000)
		out += "." + strings.TrimRight(frac, "0")
	}
	return out
}

// formatBit renders BIT(n) bytes as the big-endian bit string with leading
// zero bits stripped, so the same numeric value compares equal across
// differing bit widths (bit(1) 1 == bit(8) 1). All-zero values render as "0".
func formatBit(b []byte) []byte {
	var sb strings.Builder
	for _, x := range b {
		for shift := 7; shift >= 0; shift-- {
			if x>>uint(shift)&1 == 1 {
				sb.WriteByte('1')
			} else {
				sb.WriteByte('0')
			}
		}
	}
	if s := strings.TrimLeft(sb.String(), "0"); s != "" {
		return []byte(s)
	}
	return []byte("0")
}

// isDecimalUint reports whether s is a plain unsigned decimal integer
// (the driver's rendering of a BIGINT UNSIGNED value beyond
// math.MaxInt64: digits only, no sign, no separators).
func isDecimalUint(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func asString(v driver.Value) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case []byte:
		return string(t), true
	}
	return "", false
}

func asBytes(v driver.Value) ([]byte, bool) {
	switch t := v.(type) {
	case []byte:
		return t, true
	case string:
		return []byte(t), true
	}
	return nil, false
}
