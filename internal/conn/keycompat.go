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
//
//   - the SAME family: INT and VARCHAR order values differently even
//     when both sides agree on names, and a family drift under
//     --no-sync-schema is exactly the case this gates;
//
//   - the ORDERING SEMANTICS of that family, per the explicit gate
//     below. The principle is "allow only what can be PROVEN to order
//     identically on both endpoints", never "allow unless a mismatch is
//     found":
//
//   - value-ordered families (the integers, the exact decimals, the
//     floats, the temporals, the byte-ordered types): the sort
//     position of a value is a pure function of the VALUE itself, so
//     the family agreement above already pins the semantics on both
//     endpoints — a DECIMAL's order is its exact numeric value on
//     every server, a TIME's its duration, a BINARY/VARBINARY/BIT
//     value its byte/bit content. No collation or member list to
//     compare;
//
//   - string families: the SAME EFFECTIVE collation. A string key's
//     order is the collation's: "Z" < "a" in utf8mb4_bin, "a" < "Z"
//     in utf8mb4_general_ci — the same two key values partition a
//     range predicate differently on each endpoint, so source bounds
//     rendered against the destination address the wrong rows (a
//     wrong out-of-range delete on a parent cascades to its FK
//     children and re-inserts the parent over them). The effective
//     collation is the column's COLLATION_NAME (for a column without
//     an explicit COLLATE clause that is the backend's DEFAULT
//     collation for the charset — which legitimately DIFFERS between
//     backends: 8.0's utf8mb4_0900_ai_ci vs 5.7's
//     utf8mb4_general_ci). An explicit collation on one side and a
//     default on the other is NOT comparable (the default is the
//     other backend's, unknown here), so a pair with one side's
//     collation empty is incompatible; two sides both empty are
//     byte-order types (e.g. CHAR(n) BINARY, which reports no
//     collation at all) and order identically — anything else with an
//     unknown collation is a refusal, not a guess;
//
//   - ENUM / SET: the sort position is NOT a function of the value —
//     it is the value's position in the member LIST AS DEFINED (an
//     ENUM compares by member index, a SET by the bit position its
//     member definition carries). Two sides may agree on family and
//     on the data while DEFINING the members in different orders
//     (ENUM('b','a') vs ENUM('a','b'): b < a on one side, a < b on
//     the other) — the orderings are then REVERSED, and shared key
//     bounds address the wrong rows. The pair is compatible only when
//     the member definitions are proven EXACTLY equal (same members,
//     same order, unescaped); a definition that cannot be parsed is a
//     refusal, not a guess (fail closed). The structure sync repairs
//     the case it can repair — it re-emits the source's type
//     verbatim — and the post-repair introspection sees identical
//     definitions;
//
//   - everything else (JSON, any type this version does not
//     classify): a refusal, not a guess.
//
// A mismatch means the pair must never share source bounds against the
// destination: the comparison degrades to whole-table unordered
// multisets (like the keyless fallback), and the sync fails closed
// instead of row-level addressing (see DecidePlan). The structure
// sync's pre-step repairs the case it can repair — it aligns the
// destination's key column type and collation with the source's — and
// the post-repair introspection sees identical definitions, so a
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
		switch ca.Family {
		case FamINT, FamUINT, FamYEAR, FamDATE, FamDATETIME, FamTIMESTAMP,
			FamTIME, FamDECIMAL, FamFLOAT, FamDOUBLE, FamBYTES, FamBIT:
			// value-ordered: the sort position is a pure function of
			// the value itself (exact numeric value, calendar/duration
			// instant, byte/bit content) — the family agreement above
			// already pins the semantics on both endpoints
			continue
		case FamSTR:
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
		case FamENUM, FamSET:
			// definition-ordered: the sort position comes from the
			// member list's DEFINITION ORDER (ENUM by member index, SET
			// by the bit position of the member), which can differ
			// between the endpoints even with identical data
			if !enumSetMembersEqual(ca.RawType, cb.RawType) {
				return false, fmt.Sprintf("key column %s: %s member definitions differ (%s vs %s): ordering follows the member DEFINITION order, which the endpoints disagree on", name, ca.Family, ca.RawType, cb.RawType)
			}
		default:
			return false, fmt.Sprintf("key column %s: key ordering semantics for family %s are not proven compatible on both endpoints", name, ca.Family)
		}
	}
	return true, ""
}

// KeyRangeChunkable reports whether the pair's key can be used for RANGE
// chunking: the planner renders key bounds into WHERE comparisons (and the
// sync's out-of-range pass renders key-range predicates) that must select
// exactly the rows the key order says they are in the range.
//
// It subsumes KeyOrderCompatible (the cross-endpoint half) and adds the
// per-side property that the single-compatibility gate cannot see: the
// column's ORDER BY order must equal its WHERE-comparison order. That
// holds for the value-ordered families (a WHERE comparison of two values
// of such a column agrees with the ORDER BY of those values) but NOT for
// ENUM/SET: MySQL orders an ENUM column by DEFINITION order in ORDER BY
// (ENUM('b','a') sorts b, then a), yet compares it against a string by
// the member NAME's collation order (there 'b' > 'a'). For such a key the
// planner's [min..max] bounds are an EMPTY interval in the WHERE while
// covering the whole table in the ORDER BY: both sides scan zero rows,
// their empty digests are identical, and a diverged table compares
// IDENTICAL (a silent false identical, reproduced on MySQL 8.0: for
// enum('b','a') "k >= 'b' AND k <= 'a'" selects no row, and "k >= 'a'"
// selects both). A key with an ENUM/SET column (in any position of a
// composite key, where the tie-break terms have the same problem) is not
// range-chunkable: the table compares as a whole-table multiset and the
// sync converges it by full resync instead (see DecidePlan).
func KeyRangeChunkable(a, b *Schema) (bool, string) {
	if ok, why := KeyOrderCompatible(a, b); !ok {
		return false, why
	}
	if ok, why := keyRangeAddressable(a); !ok {
		return false, why
	}
	if ok, why := keyRangeAddressable(b); !ok {
		return false, why
	}
	return true, ""
}

// KeyRangeAddressable reports whether a SINGLE side's key can be used for
// RANGE chunking: none of its key columns is an ENUM/SET. It is the
// per-side half of KeyRangeChunkable, exposed on its own because the
// full-resync path plans ONE side's source rows (it has no pair to
// cross-check): a key whose column is an ENUM/SET must be streamed as a
// whole-table (keyless) chunk, or its [min..max] range chunk is empty in
// the WHERE and the source rows are silently lost.
func KeyRangeAddressable(s *Schema) (bool, string) {
	return keyRangeAddressable(s)
}

func keyRangeAddressable(s *Schema) (bool, string) {
	for _, name := range s.Key {
		c, ok := columnOf(s, name)
		if !ok {
			return false, fmt.Sprintf("key column %s not found in the schema", name)
		}
		if c.Family == FamENUM || c.Family == FamSET {
			return false, fmt.Sprintf("key column %s is %s: ORDER BY orders the members by definition while a WHERE comparison orders them by the member name's collation, so key ranges cannot be addressed", name, c.Family)
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

// enumSetMembersEqual reports whether two ENUM/SET column type
// definitions name the SAME members in the SAME order — the only
// property that pins the sort order of the family (the ENUM's member
// index and the SET's bit position are both defined by position in the
// member list; ENUM('b','a') and ENUM('a','b') sort the same DATA in
// opposite orders). The members are compared as exact, unescaped values
// in definition order; a type name that cannot be parsed as an ENUM/SET
// definition is NOT equal to anything (fail closed).
func enumSetMembersEqual(a, b string) bool {
	ma, okA := enumSetMembers(a)
	mb, okB := enumSetMembers(b)
	return okA && okB && membersEqual(ma, mb)
}

func membersEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// enumSetMembers parses the member list out of an ENUM/SET column type
// (the information_schema COLUMN_TYPE rendering: "enum('a','b')",
// "set('x','y')") and returns the members as declared — quotes stripped,
// the backslash (\') and doubled (”) quote escapes unescaped — in
// DEFINITION ORDER. Only the type name is case-folded; the member values
// are compared exactly as declared (a case difference is a different
// definition, and a different definition is a refusal, not a guess).
// ok is false when the text is not a parseable ENUM/SET definition: the
// caller must refuse, never assume.
func enumSetMembers(rawType string) ([]string, bool) {
	l := strings.TrimSpace(rawType)
	open := strings.IndexByte(l, '(')
	if open < 0 || !strings.HasSuffix(l, ")") {
		return nil, false
	}
	base := strings.ToLower(strings.TrimSpace(l[:open]))
	if base != "enum" && base != "set" {
		return nil, false
	}
	inner := l[open+1 : len(l)-1]
	// Members are single-quoted, comma-separated at the top level (a
	// comma INSIDE a quoted member is part of the value, not a
	// separator). Anything else — an unquoted member, an unclosed quote,
	// trailing junk — is not the introspection rendering: refuse.
	var members []string
	i := 0
	for {
		for i < len(inner) && (inner[i] == ' ' || inner[i] == '\t') {
			i++
		}
		if i == len(inner) {
			break
		}
		if inner[i] != '\'' {
			return nil, false
		}
		i++
		var sb strings.Builder
		closed := false
		for i < len(inner) {
			c := inner[i]
			switch {
			case c == '\\':
				if i+1 >= len(inner) {
					return nil, false
				}
				sb.WriteByte(inner[i+1])
				i += 2
			case c == '\'':
				if i+1 < len(inner) && inner[i+1] == '\'' {
					sb.WriteByte('\'') // doubled-quote escape
					i += 2
				} else {
					closed = true
					i++
				}
			default:
				sb.WriteByte(c)
				i++
			}
			if closed {
				break
			}
		}
		if !closed {
			return nil, false
		}
		members = append(members, sb.String())
		for i < len(inner) && (inner[i] == ' ' || inner[i] == '\t') {
			i++
		}
		if i == len(inner) {
			break
		}
		if inner[i] != ',' {
			return nil, false
		}
		i++
	}
	if len(members) == 0 {
		return nil, false
	}
	return members, true
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
