package normalize

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// normalizeJSON canonicalizes a JSON document: keys sorted at every level
// (json.Marshal sorts map keys), numbers in EXACT canonical form (see
// canonicalJSONNumber — no float64 round trip, so values beyond ~17
// significant digits stay distinct), compact spacing.
func normalizeJSON(b []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	out, err := json.Marshal(canonicalizeJSON(v))
	if err != nil {
		return nil, err
	}
	return out, nil
}

func canonicalizeJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = canonicalizeJSON(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = canonicalizeJSON(val)
		}
		return out
	case json.Number:
		// exact: the number text is canonicalized without ever passing
		// through float64 (a ParseFloat round trip collapses
		// 9007199254740992 and 9007199254740993 — both round to the
		// same float64 — into one canonical form)
		return canonicalJSONNumber(string(t))
	}
	return v
}

// jsonPlainMaxDigits: while the plain rendering stays this short it is
// used (1, 0.1, 123456789012345678901234567890... up to the limit);
// beyond it the value renders as a normalized coefficient + exponent
// (1e+100000000), NEVER expanded — a number like 1e100000000 must not
// become a 100 MB string of zeros.
const jsonPlainMaxDigits = 20

// canonicalJSONNumber renders one JSON number token in exact canonical
// form: the value is reduced to (significant digits, decimal power of
// ten) and rendered without loss. Equal values always render identically
// (1, 1.0, 1.00 and 1e0 are all "1"); distinct values always render
// differently, at ANY magnitude (9007199254740992 vs 9007199254740993,
// 27-digit integers, huge exponents) — the (digits, power) pair after
// reduction is unique per value, and the plain / scientific renderings
// are disjoint from each other (scientific carries an "e", plain never
// does).
func canonicalJSONNumber(tok string) string {
	s := tok
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	// the exponent part
	exp := 0
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		e, err := strconv.Atoi(s[i+1:])
		if err != nil || s[i+1:] == "" {
			return tok // malformed: the decoder would have rejected it; keep as-is
		}
		exp = e
		s = s[:i]
	}
	// the fraction: the digits right of the dot contribute to the power
	dotPos := 0
	if i := strings.IndexByte(s, '.'); i >= 0 {
		dotPos = len(s) - i - 1
		s = s[:i] + s[i+1:]
	}
	// value = s * 10^(exp - dotPos); leading zeros carry no value
	s = strings.TrimLeft(s, "0")
	power := exp - dotPos
	// trailing zeros fold into the power (110 * 10^-2 == 11 * 10^-1)
	if t := strings.TrimRight(s, "0"); len(t) < len(s) {
		power += len(s) - len(t)
		s = t
	}
	if s == "" {
		return "0" // also covers "-0"
	}
	var out string
	if power >= 0 && len(s)+power <= jsonPlainMaxDigits {
		out = s + strings.Repeat("0", power)
	} else if power < 0 {
		shift := -power
		if len(s) <= jsonPlainMaxDigits {
			if shift < len(s) {
				cut := len(s) - shift
				if frac := s[cut:]; frac != "" {
					out = s[:cut] + "." + frac
				} else {
					out = s
				}
			} else {
				out = "0." + strings.Repeat("0", shift-len(s)) + s
			}
		}
	}
	if out == "" {
		// scientific: d.ddd e ±E (the decimal point sits after the first
		// digit, so the exponent is power + len(s) - 1)
		e := power + len(s) - 1
		if len(s) == 1 {
			out = s + "e" + formatExp(e)
		} else {
			out = s[0:1] + "." + s[1:] + "e" + formatExp(e)
		}
	}
	if neg && out != "0" {
		out = "-" + out
	}
	return out
}

func formatExp(e int) string {
	if e < 0 {
		return strconv.Itoa(e)
	}
	return "+" + strconv.Itoa(e)
}
