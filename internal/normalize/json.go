package normalize

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// normalizeJSON canonicalizes a JSON document: keys sorted at every level
// (json.Marshal sorts map keys), numbers in EXACT canonical form (see
// canonicalJSONNumber — no float64 round trip, so values beyond ~17
// significant digits stay distinct), compact spacing. The JSON TYPE of
// every value is preserved: a number stays a JSON NUMBER (json.Number,
// marshaled without quotes) and a string stays a JSON STRING — the
// normalized document of {"n":1} and of {"n":"1"} are DIFFERENT
// ({"n":1} vs {"n":"1"}): a JSON number is not a JSON string, and
// collapsing the two into one rendering is a silent false identical.
//
// The whole normalization is FAIL-CLOSED on any number the bounded
// canonical grammar cannot render (see canonicalJSONNumber): if one token
// in the document is outside the supported range, the ENTIRE document
// returns an error. Preserving that token's raw lexical form would both
// skip its canonicalization (two equal values spelled 1e99… vs 10e98…
// would then compare different — defeating --normalize-json's semantic
// promise) and silently compare non-canonical text. So a value that cannot
// be proven canonical makes the comparison fail, never guess.
func normalizeJSON(b []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	canonical, err := canonicalizeJSON(v)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(canonical)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// canonicalizeJSON recursively canonicalizes a decoded JSON tree. It
// returns an error if ANY number token in the tree is outside the bounded
// canonical-number range (see canonicalJSONNumber). One bad number
// poisons the whole document on purpose: --normalize-json promised
// semantic number normalization, and the only alternatives to a value we
// cannot canonicalize are "compare it raw" (breaks equal values spelled
// differently) or "drop it" (fabricates equality) — both wrong, so the
// comparison fails instead.
func canonicalizeJSON(v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			cv, err := canonicalizeJSON(val)
			if err != nil {
				return nil, err
			}
			out[k] = cv
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			cv, err := canonicalizeJSON(val)
			if err != nil {
				return nil, err
			}
			out[i] = cv
		}
		return out, nil
	case json.Number:
		// exact: the number text is canonicalized without ever passing
		// through float64 (a ParseFloat round trip collapses
		// 9007199254740992 and 9007199254740993 — both round to the
		// same float64 — into one canonical form). The result STAYS a
		// json.Number in the canonical tree: json.Marshal renders it as
		// a JSON NUMBER (no quotes), so the type survives the
		// normalization ({"n":1} != {"n":"1"}). The canonical token is
		// itself a valid JSON number (plain or d.ddd e ±E), so the raw
		// rendering is well-formed.
		n, err := canonicalJSONNumber(string(t))
		if err != nil {
			return nil, err
		}
		return json.Number(n), nil
	}
	return v, nil
}

// canonicalJSONNumber renders one JSON number token in exact canonical
// form (the shared grammar of canonical.go): the value is reduced to
// (significant digits, decimal power of ten) and rendered without loss.
// Equal values always render identically (1, 1.0, 1.00 and 1e0 are all
// "1"); distinct values always render differently, at ANY magnitude
// (9007199254740992 vs 9007199254740993, 27-digit integers, huge
// exponents — 1e+100000000 stays compact, never expanded).
//
// The JSON decoder accepts arbitrary-length exponent text, so a token can
// be well-formed JSON yet still beyond the range mtdiff's bounded
// canonical-number policy can render (decimalParts rejects it). Such a
// token makes this return an ERROR, not a guess: callers must fail closed
// on that error, because preserving the raw token would both skip
// canonicalization and compare non-canonical lexical forms.
func canonicalJSONNumber(tok string) (string, error) {
	neg, digits, power, ok := decimalParts(tok)
	if !ok {
		return "", fmt.Errorf("JSON number is outside the supported canonical range (unrenderable exponent): %q", tok)
	}
	return renderCanonicalNumber(neg, digits, power), nil
}
