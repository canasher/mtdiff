package normalize

import (
	"fmt"
	"strconv"
	"strings"
)

// jsonPlainMaxDigits: while the plain rendering stays this short it is
// used (1, 0.1, 123456789012345678901234567890... up to the limit);
// beyond it the value renders as a normalized coefficient + exponent
// (1e+100000000), NEVER expanded — a number like 1e100000000 must not
// become a 100 MB string of zeros. The cap bounds the INTEGER length of
// the token; a small value with a large negative power (0.00001) may
// render a few digits longer in its plain form, as it always has.
const jsonPlainMaxDigits = 20

// maxRenderableExponent: the largest |power-of-ten| we render. Beyond it
// the value is so far from 1 that no canonical decimal form is useful, and
// the exponent arithmetic (negation, +len(digits)-1) would approach the Go
// int limit — so we fail closed (a clear error) instead of risking an
// overflow that could produce a wrong canonical value. (An exponent that
// does not even fit a Go int is already rejected by decimalParts'
// strconv.Atoi above.)
const maxRenderableExponent = 1 << 60

// decimalParts parses an exact decimal token — a plain decimal
// ("123.45", "-0.001", "007") or an exponent form ("1e6", "-1.2E-3") —
// into its canonical (sign, digits, power-of-ten) form: the value is
// digits*10^power, with the digits carrying no leading zero and no
// trailing zero, so the triple is UNIQUE per value (1, 1.0, 1.00 and
// 1e0 all reduce to ("1", 0)). A value of zero is ("0", 0) with the sign
// dropped. ok is false when the token is not a decimal (the float
// sentinels "NaN"/"±Inf" are not decimals either — the caller decides
// what they mean). The parsing is pure string arithmetic: no float64
// anywhere, so values beyond ~17 significant digits stay exact.
func decimalParts(s string) (neg bool, digits string, power int, ok bool) {
	t := strings.TrimSpace(s)
	neg = strings.HasPrefix(t, "-")
	if neg || strings.HasPrefix(t, "+") {
		t = t[1:]
	}
	// the exponent part (a token like "1e5.2" is not a decimal: the
	// exponent text must be a plain signed integer)
	exp := 0
	if i := strings.IndexAny(t, "eE"); i >= 0 {
		e, err := strconv.Atoi(t[i+1:])
		if err != nil || t[i+1:] == "" {
			return false, "", 0, false
		}
		if e > maxRenderableExponent || e < -maxRenderableExponent {
			return false, "", 0, false
		}
		exp = e
		t = t[:i]
	}
	if t == "" {
		return false, "", 0, false
	}
	// digits, and at most ONE dot (a second dot is malformed, not a
	// decimal: "1..2" must be refused, not silently re-parsed)
	dots := 0
	for _, r := range t {
		if r == '.' {
			dots++
			continue
		}
		if r < '0' || r > '9' {
			return false, "", 0, false
		}
	}
	if dots > 1 {
		return false, "", 0, false
	}
	// the fraction: the digits right of the dot contribute to the power
	dotPos := 0
	if i := strings.IndexByte(t, '.'); i >= 0 {
		dotPos = len(t) - i - 1
		t = t[:i] + t[i+1:]
	}
	// value = t * 10^(exp - dotPos); leading zeros carry no value
	t = strings.TrimLeft(t, "0")
	power = exp - dotPos
	// trailing zeros fold into the power (110 * 10^-2 == 11 * 10^-1)
	if trimmed := strings.TrimRight(t, "0"); len(trimmed) < len(t) {
		power += len(t) - len(trimmed)
		t = trimmed
	}
	if t == "" {
		return false, "0", 0, true // the value is zero: the sign is dropped
	}
	return neg, t, power, true
}

// renderCanonicalNumber renders (neg, digits, power) in the shared
// canonical decimal grammar: the value is plain while it fits
// jsonPlainMaxDigits (the token carries no "e"), and beyond that a
// normalized coefficient + exponent (d.ddd e ±E) — compact at ANY
// magnitude, never expanded. Equal values render identically (the
// (digits, power) pair after reduction is unique per value); distinct
// values render differently (the plain and scientific renderings are
// disjoint: scientific carries an "e", plain never does).
func renderCanonicalNumber(neg bool, digits string, power int) string {
	if digits == "0" {
		return "0"
	}
	var out string
	if power >= 0 && len(digits)+power <= jsonPlainMaxDigits {
		out = digits + strings.Repeat("0", power)
	} else if power < 0 {
		shift := -power
		// Use the plain "0.xxx" (or "12.3") form ONLY while it stays
		// short. A large |power| must not expand into an O(|power|) run of
		// zeros — 1e-100000000 must stay the compact "1e-100000000", never
		// a ~100 MB "0.000...1". The plain length is len(digits)+1 when the
		// dot sits inside the digits (shift < len(digits)) and 2+shift when
		// it does not ("0." + (shift-len) zeros + digits); beyond the
		// plain limit it falls through to the scientific form below.
		plainLen := len(digits) + 1
		if shift >= len(digits) {
			plainLen = 2 + shift
		}
		if plainLen <= jsonPlainMaxDigits {
			if shift < len(digits) {
				cut := len(digits) - shift
				if frac := digits[cut:]; frac != "" {
					out = digits[:cut] + "." + frac
				} else {
					out = digits
				}
			} else {
				out = "0." + strings.Repeat("0", shift-len(digits)) + digits
			}
		}
	}
	if out == "" {
		// scientific: d.ddd e ±E (the decimal point sits after the first
		// digit, so the exponent is power + len(digits) - 1)
		e := power + len(digits) - 1
		if len(digits) == 1 {
			out = digits + "e" + formatExp(e)
		} else {
			out = digits[0:1] + "." + digits[1:] + "e" + formatExp(e)
		}
	}
	if neg {
		out = "-" + out
	}
	return out
}

// formatExp renders an exponent with an explicit sign (e+6 / e-3).
func formatExp(e int) string {
	if e < 0 {
		return strconv.Itoa(e)
	}
	return "+" + strconv.Itoa(e)
}

// canonicalNumber renders an exact decimal token (plain "[-]d[.d]" or
// exponent "[-]d[.d][eE±]d") in the shared canonical decimal grammar —
// the ONE payload grammar every numeric family passes through, so the
// SAME value renders identically no matter which family carried it
// (INT 1000000 == DECIMAL 1000000.000 == DOUBLE 1e+06: all "1000000"),
// while DISTINCT values always render differently (the (digits, power)
// reduction is unique per value). This is the second half of the shared
// tagNUMERIC fix: the tag alone was not enough while the payloads
// disagreed on the grammar ("1e+06" vs "1000000").
//
// The float sentinels ("NaN", "+Inf", "-Inf") pass through unchanged:
// a MySQL 8.0 DOUBLE can store them and no canonical decimal exists for
// them (they are already distinct from every finite rendering). Any
// other non-decimal token is an ERROR, not a guess: a payload that
// cannot be canonicalized must surface, never silently become a value.
func canonicalNumber(tok string) (string, error) {
	switch tok {
	case "NaN", "+Inf", "-Inf":
		return tok, nil
	}
	neg, digits, power, ok := decimalParts(tok)
	if !ok {
		return "", fmt.Errorf("not a canonical decimal: %q", tok)
	}
	return renderCanonicalNumber(neg, digits, power), nil
}
