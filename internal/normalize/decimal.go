package normalize

import (
	"fmt"
	"strings"
)

// normalizeDecimal canonicalizes a decimal string without ever going
// through float64 (which would systematically misrepresent values like
// 0.1). It delegates to the SHARED canonical decimal grammar
// (canonicalNumber), so a decimal's payload agrees with the payloads of
// the other numeric families — DECIMAL "0.0000100" and DOUBLE 0.00001
// both render "0.00001", INT 1000000 and DOUBLE 1e+06 both render
// "1000000" (the tagNUMERIC families share the tag AND the payload
// grammar). The value renders plain while it fits and in the shared
// d[.ddd]e±E grammar beyond that. The empty string canonicalizes to
// "0" (a NULL decimal is a different encoding: tagNULL); any other
// unparseable text is corrupt driver output and panics loudly rather
// than render a guess (production goes through encodeValue, which
// surfaces canonicalNumber's error instead).
//
//	"1.10" -> "1.1"    "0.10" -> "0.1"
//	"-0"   -> "0"     "007"  -> "7"
//	"123.450" -> "123.45"
func normalizeDecimal(s string) string {
	if strings.TrimSpace(s) == "" {
		return "0"
	}
	out, err := canonicalNumber(s)
	if err != nil {
		panic(fmt.Sprintf("unparseable decimal value %q: %v", s, err))
	}
	return out
}
