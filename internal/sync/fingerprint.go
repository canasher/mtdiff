package sync

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"time"
)

// This file implements the IDENTITY of a destructive row-rewrite group —
// the P0 answer to "the user confirmed a rewrite of group A/B, may the
// apply rewrite group C/D instead?". A COUNT cannot answer that (the
// re-plan can keep the count and change the group), so the confirmed
// scope carries a fingerprint per rewrite group, and the apply-time
// re-plan may only run groups whose fingerprints are a SUBSET of the
// confirmed set.
//
// The encoding never renders values as display SQL and never uses
// fmt %v as an identity: each value is a TYPE-TAGGED, length-prefixed
// byte encoding (so int 5, the string "5", 5.0 and the byte 0x35 can
// never collide), and the group's row identities are SORTED before
// hashing (map/row order must not change the fingerprint). The digest
// is one-way, so the fingerprint does not leak business values.

// canonicalValue encodes one driver value canonically. The tags keep
// the encoding injective across the value types the MySQL driver
// returns (int64 / uint64 / float64 / string / []byte / time.Time /
// bool / nil); a value of an unknown type falls back to a tagged
// %T+%v encoding, which cannot collide with any tagged type.
func canonicalValue(v any) []byte {
	var b []byte
	tag := func(t byte, payload ...[]byte) []byte {
		b = append(b, t)
		for _, p := range payload {
			var lp [4]byte
			binary.BigEndian.PutUint32(lp[:], uint32(len(p)))
			b = append(b, lp[:]...)
			b = append(b, p...)
		}
		return b
	}
	var raw []byte
	var u64 [8]byte
	switch vv := v.(type) {
	case nil:
		return []byte{0x00}
	case bool:
		if vv {
			raw = []byte{1}
		} else {
			raw = []byte{0}
		}
		return tag(0x01, raw)
	case int64:
		binary.BigEndian.PutUint64(u64[:], uint64(vv))
		return tag(0x02, u64[:])
	case uint64:
		binary.BigEndian.PutUint64(u64[:], vv)
		return tag(0x03, u64[:])
	case float64:
		binary.BigEndian.PutUint64(u64[:], math.Float64bits(vv))
		return tag(0x04, u64[:])
	case string:
		return tag(0x05, []byte(vv))
	case []byte:
		return tag(0x06, vv)
	case time.Time:
		return tag(0x07, []byte(vv.UTC().Format(time.RFC3339Nano)))
	default:
		// unreachable for MySQL driver values; the tag keeps it
		// injective against every typed encoding above
		return tag(0x08, []byte(fmt.Sprintf("%T|%v", vv, vv)))
	}
}

// keyIdentity encodes one key row: each component canonically, joined
// with a separator byte that is length-prefixed away (the components
// carry their own lengths, so the join is unambiguous).
func keyIdentity(vals []any) []byte {
	var b []byte
	for i, v := range vals {
		if i > 0 {
			b = append(b, 0x1F)
		}
		b = append(b, canonicalValue(v)...)
	}
	return b
}

// rewriteFingerprint is the stable identity of one destructive
// rewrite group: the triggering constraint(s)' column names plus the
// group's destination key rows, in an order-independent form. It is a
// sha256 digest — stable for the same group under any row or map
// iteration order, distinct for different groups (the tagged,
// length-prefixed encoding is injective), and leak-free (no value or
// column name survives the hash).
func rewriteFingerprint(constraintCols [][]string, keys [][]any) string {
	// constraint identity: each constraint's columns, in the order
	// collected (constraint index order), NUL-joined
	var cid []byte
	for i, cols := range constraintCols {
		if i > 0 {
			cid = append(cid, 0)
		}
		for j, c := range cols {
			if j > 0 {
				cid = append(cid, 0x1F)
			}
			cid = append(cid, c...)
		}
	}
	// row identities: sorted, so group membership — not row order —
	// defines the group
	ids := make([][]byte, 0, len(keys))
	for _, k := range keys {
		ids = append(ids, keyIdentity(k))
	}
	sort.Slice(ids, func(i, j int) bool {
		return string(ids[i]) < string(ids[j])
	})
	h := sha256.New()
	h.Write(cid)
	h.Write([]byte{0x1E})
	for i, id := range ids {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write(id)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// fingerprintSubset reports whether every fingerprint in current is
// present in confirmed — the rewrite-scope rule: the apply-time re-plan
// may run the confirmed groups (all, some, or none) and nothing else.
func fingerprintSubset(current, confirmed []string) bool {
	ok := make(map[string]struct{}, len(confirmed))
	for _, c := range confirmed {
		ok[c] = struct{}{}
	}
	for _, c := range current {
		if _, found := ok[c]; !found {
			return false
		}
	}
	return true
}

// fingerprintNew counts the current group(s) absent from the confirmed
// set — the "new" number the refusal message shows (never the keys
// themselves: the message must not leak business values).
func fingerprintNew(current, confirmed []string) int {
	ok := make(map[string]struct{}, len(confirmed))
	for _, c := range confirmed {
		ok[c] = struct{}{}
	}
	n := 0
	for _, c := range current {
		if _, found := ok[c]; !found {
			n++
		}
	}
	return n
}
