package conn

import (
	"fmt"
	"strings"
)

// KeyOrderCompatible reports whether two schemas' usable keys sort rows
// in the SAME order on both endpoints — the precondition for sharing
// key-derived bounds across the pair: chunk boundaries, the source
// min/max that bound the destination's out-of-range deletes, and
// row-level key addressing all assume "the Nth key value on the source
// orders the destination the same way".
//
// Per key component (in index order) the check is:
//
//   - the SAME column name (exact — keyAgree already enforces this; the
//     check is repeated so this function is self-contained);
//   - the SAME family: INT and VARCHAR order values differently even
//     when both sides agree on names, and a family drift under
//     --no-sync-schema is exactly the case this gates;
//   - for string families: the SAME EFFECTIVE collation. A string
//     key's order is the collation's: "Z" < "a" in utf8mb4_bin,
//     "a" < "Z" in utf8mb4_general_ci — the same two key values
//     partition a range predicate differently on each endpoint, so
//     source bounds rendered against the destination address the wrong
//     rows (a wrong out-of-range delete on a parent cascades to its FK
//     children and re-inserts the parent over them).
//
// The effective collation is the column's COLLATION_NAME (for a column
// without an explicit COLLATE clause that is the backend's DEFAULT
// collation for the charset — which legitimately DIFFERS between
// backends: 8.0's utf8mb4_0900_ai_ci vs 5.7's utf8mb4_general_ci).
// An explicit collation on one side and a default on the other is NOT
// comparable (the default is the other backend's, unknown here), so a
// pair with one side's collation empty is incompatible; two sides both
// empty are byte-order types (e.g. CHAR(n) BINARY, which reports no
// collation at all) and order identically — anything else with an
// unknown collation is a refusal, not a guess.
//
// A mismatch means the pair must never share source bounds against the
// destination: the comparison degrades to whole-table unordered
// multisets (like the keyless fallback), and the sync fails closed
// instead of row-level addressing (see DecidePlan). The structure
// sync's pre-step repairs the case it can repair — it aligns the
// destination's key column type and collation with the source's — and
// the post-repair introspection sees identical collations, so a
// structure-synced pair passes this gate on the fresh prepare.
func KeyOrderCompatible(a, b *Schema) (bool, string) {
	if len(a.Key) != len(b.Key) {
		return false, fmt.Sprintf("key column count differs (%d vs %d)", len(a.Key), len(b.Key))
	}
	for i, name := range a.Key {
		if name != b.Key[i] {
			return false, fmt.Sprintf("key column %d differs (%s vs %s)", i+1, name, b.Key[i])
		}
		ca, okA := columnOf(a, name)
		cb, okB := columnOf(b, name)
		if !okA || !okB {
			return false, fmt.Sprintf("key column %s not found in the schema", name)
		}
		if ca.Family != cb.Family {
			return false, fmt.Sprintf("key column %s: family %s on one side vs %s on the other", name, ca.Family, cb.Family)
		}
		if ca.Family != FamSTR {
			// non-string families order by their exact values
			// (byte-exact ints and decimals, the temporal families by
			// instant, BIT/BYTES bytewise): no collation to compare
			continue
		}
		if ca.Collation == "" && cb.Collation == "" {
			// no effective collation on either side: only byte-order
			// types (the BINARY attribute) sort identically without a
			// collation to name
			if isByteOrder(ca.RawType) && isByteOrder(cb.RawType) {
				continue
			}
			return false, fmt.Sprintf("key column %s: no effective collation on either side and the types are not byte-ordered (%s vs %s)", name, ca.RawType, cb.RawType)
		}
		if ca.Collation == "" || cb.Collation == "" {
			return false, fmt.Sprintf("key column %s: one side has an explicit collation, the other the backend default (unknown here) (%q vs %q)", name, ca.Collation, cb.Collation)
		}
		if !strings.EqualFold(ca.Collation, cb.Collation) {
			return false, fmt.Sprintf("key column %s: collation %s vs %s", name, ca.Collation, cb.Collation)
		}
	}
	return true, ""
}

func columnOf(s *Schema, name string) (Column, bool) {
	for _, c := range s.Cols {
		if c.Name == name {
			return c, true
		}
	}
	return Column{}, false
}

// isByteOrder reports a column type whose values order bytewise with no
// collation semantics: the binary base types, or a string type carrying
// the BINARY attribute (CHAR(n) BINARY reports no COLLATION_NAME).
func isByteOrder(rawType string) bool {
	l := strings.ToLower(strings.TrimSpace(rawType))
	base := l
	if i := strings.IndexByte(l, '('); i >= 0 {
		base = l[:i]
	}
	switch base {
	case "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob":
		return true
	}
	return strings.Contains(l, "binary")
}
