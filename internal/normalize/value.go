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
		return strconv.AppendInt(nil, i, 10), nil
	case conn.FamUINT:
		u, ok := v.(uint64)
		if !ok {
			return nil, fmt.Errorf("expected uint64, got %T", v)
		}
		return strconv.AppendUint(nil, u, 10), nil
	case conn.FamDECIMAL:
		s, ok := asString(v)
		if !ok {
			return nil, fmt.Errorf("expected decimal bytes, got %T", v)
		}
		return []byte(normalizeDecimal(s)), nil
	case conn.FamFLOAT:
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
		return []byte(formatFloat(f64, n.opts.Tolerance, 32)), nil
	case conn.FamDOUBLE:
		f, ok := v.(float64)
		if !ok {
			return nil, fmt.Errorf("expected float64, got %T", v)
		}
		return []byte(formatFloat(f, n.opts.Tolerance, 64)), nil
	case conn.FamDATE:
		t, ok := v.(time.Time)
		if !ok {
			return nil, fmt.Errorf("expected time.Time, got %T", v)
		}
		return []byte(t.Format("2006-01-02")), nil
	case conn.FamTIME:
		d, ok := v.(time.Duration)
		if !ok {
			return nil, fmt.Errorf("expected time.Duration, got %T", v)
		}
		return []byte(formatMySQLTime(d)), nil
	case conn.FamDATETIME, conn.FamTIMESTAMP:
		t, ok := v.(time.Time)
		if !ok {
			return nil, fmt.Errorf("expected time.Time, got %T", v)
		}
		return []byte(formatMySQLDateTime(t)), nil
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

// formatFloat renders a float canonically. Without tolerance the shortest
// round-trip representation is used (bit-exact comparison). With tolerance
// the value is quantized to the grid first: every value landing in the same
// cell produces the same float64 (N * tol), so the rendering is identical for
// all in-tolerance values.
func formatFloat(v, tol float64, prec int) string {
	if math.IsNaN(v) {
		return "NaN" // MySQL has no NaN; defensive only
	}
	if tol > 0 {
		q := v / tol
		if !math.IsInf(q, 0) && math.Abs(q) < 9.2e18 {
			n64 := int64(math.Round(q))
			v = float64(n64) * tol
		} else {
			// Beyond the representable grid: saturate by sign.
			v = math.Copysign(math.Inf(1), v)
		}
	}
	if v == 0 {
		v = 0 // normalize -0
	}
	return strconv.FormatFloat(v, 'g', -1, prec)
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

// formatMySQLDateTime renders as Y-m-d H:i:s[.f]; fractional seconds are
// present only when non-zero, so equal instants render identically across
// different column precisions.
func formatMySQLDateTime(t time.Time) string {
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
