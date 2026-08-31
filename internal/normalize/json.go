package normalize

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// normalizeJSON canonicalizes a JSON document: keys sorted at every level
// (json.Marshal sorts map keys), numbers rendered in shortest round-trip
// form (json.Number "1.10" -> float64 -> "1.1"), compact spacing.
//
// Note: numbers are normalized via float64, so integer values beyond ~17
// significant digits may collapse; use raw byte comparison (the default)
// when such precision matters.
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
		if f, err := strconv.ParseFloat(string(t), 64); err == nil {
			return f
		}
		return string(t)
	}
	return v
}
