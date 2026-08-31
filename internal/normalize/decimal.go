package normalize

import "strings"

// normalizeDecimal canonicalizes a decimal string without ever going through
// float64 (which would systematically misrepresent values like 0.1).
// Rules: trim, drop sign for zero, strip leading zeros in the integer part,
// strip trailing zeros in the fractional part.
//
//	"1.10" -> "1.1"    "0.10" -> "0.1"
//	"-0"   -> "0"     "007"  -> "7"
//	"123.450" -> "123.45"
func normalizeDecimal(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "0"
	}
	neg := false
	switch s[0] {
	case '-':
		neg = true
		s = s[1:]
	case '+':
		s = s[1:]
	}
	intpart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intpart, frac = s[:i], s[i+1:]
	}
	intpart = strings.TrimLeft(intpart, "0")
	if intpart == "" {
		intpart = "0"
	}
	frac = strings.TrimRight(frac, "0")
	if neg && intpart == "0" && frac == "" {
		neg = false
	}
	out := intpart
	if frac != "" {
		out += "." + frac
	}
	if neg {
		out = "-" + out
	}
	return out
}
