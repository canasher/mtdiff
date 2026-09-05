package sync

import (
	"math"
	"testing"
)

// TestFingerprintDeterministicUnderRowOrder: the group's IDENTITY is its
// membership — any row order must yield the same digest, so map or
// result-set ordering can never change what a confirmed group means.
func TestFingerprintDeterministicUnderRowOrder(t *testing.T) {
	cols := [][]string{{"u"}}
	ab := rewriteFingerprint(cols, [][]any{{"A"}, {"B"}})
	ba := rewriteFingerprint(cols, [][]any{{"B"}, {"A"}})
	if ab != ba {
		t.Error("row order must not change the group's fingerprint")
	}
	// the same for a three-row group, permuted
	t1 := rewriteFingerprint(cols, [][]any{{int64(1)}, {int64(2)}, {int64(3)}})
	t2 := rewriteFingerprint(cols, [][]any{{int64(3)}, {int64(1)}, {int64(2)}})
	if t1 != t2 {
		t.Error("row order must not change the group's fingerprint (int keys)")
	}
	// and for a single row (the trivially-order-independent case)
	if a := rewriteFingerprint(cols, [][]any{{"A"}}); a != rewriteFingerprint(cols, [][]any{{"A"}}) {
		t.Error("identical input must give an identical digest (hash determinism)")
	}
}

// TestFingerprintCompositeKeys: a group's rows are tuples; the encoding
// is component-wise, so (1,2) and (12,) must never collide, and two
// composite groups differ when any component differs.
func TestFingerprintCompositeKeys(t *testing.T) {
	cols := [][]string{{"a", "b"}}
	pair := rewriteFingerprint(cols, [][]any{{int64(1), int64(2)}, {int64(3), int64(4)}})
	concat := rewriteFingerprint(cols, [][]any{{int64(12)}, {int64(34)}})
	if pair == concat {
		t.Error("composite (1,2),(3,4) must not collide with (12),(34)")
	}
	other := rewriteFingerprint(cols, [][]any{{int64(1), int64(2)}, {int64(3), int64(5)}})
	if pair == other {
		t.Error("different composite rows must give different group digests")
	}
	// a one-row group vs a two-row group sharing the first row
	one := rewriteFingerprint(cols, [][]any{{int64(1), int64(2)}})
	if pair == one {
		t.Error("membership size is part of the identity")
	}
}

// TestFingerprintSpecialChars: string keys may carry any byte — quotes,
// backslashes, newlines, NUL, non-ASCII. The tagged, length-prefixed
// encoding must keep them all distinct from each other AND from any
// other value type with the same visual representation.
func TestFingerprintSpecialChars(t *testing.T) {
	cols := [][]string{{"u"}}
	vals := []any{
		"'", `\`, "\n", "中文", "\x00",
		"a'b", `a\b`, "a\nb", "a\x00b", "中文x",
	}
	seen := map[string]int{}
	for _, v := range vals {
		fp := rewriteFingerprint(cols, [][]any{{v}})
		seen[fp]++
	}
	if len(seen) != len(vals) {
		t.Errorf("special-character keys must all be distinct groups, got %d digests for %d values", len(seen), len(vals))
	}
	// the NUL byte in a key must not collide with the empty string or
	// with a value that merely CONTAINS a NUL
	if rewriteFingerprint(cols, [][]any{{"\x00"}}) == rewriteFingerprint(cols, [][]any{{""}}) {
		t.Error("NUL byte must not collide with the empty string")
	}
}

// TestFingerprintNULLAndWideTypes: NULL is its own value (not the empty
// string), and the unsigned/decimal extremes of the driver's value
// range are preserved bit-for-bit.
func TestFingerprintNULLAndWideTypes(t *testing.T) {
	cols := [][]string{{"u"}}
	if rewriteFingerprint(cols, [][]any{{nil}}) == rewriteFingerprint(cols, [][]any{{""}}) {
		t.Error("NULL must not collide with the empty string")
	}
	if rewriteFingerprint(cols, [][]any{{nil, nil}}) == rewriteFingerprint(cols, [][]any{{""}}) {
		t.Error("a NULL component must not collapse to an empty string")
	}
	maxU := rewriteFingerprint(cols, [][]any{{uint64(math.MaxUint64)}})
	maxI := rewriteFingerprint(cols, [][]any{{int64(-1)}}) // same 64 bits as MaxUint64
	if maxU == maxI {
		t.Error("uint64 max and int64 -1 share bits but not a type: the tag must keep them distinct")
	}
}

// TestFingerprintTypeInjectivity is the core anti-collision property:
// the value 5 rendered by the driver as int64, string, float64 or bytes
// are DIFFERENT values, and their group digests must differ. A %v- or
// display-SQL-based identity would collapse all four.
func TestFingerprintTypeInjectivity(t *testing.T) {
	cols := [][]string{{"u"}}
	fps := []string{
		rewriteFingerprint(cols, [][]any{{int64(5)}}),
		rewriteFingerprint(cols, [][]any{{"5"}}),
		rewriteFingerprint(cols, [][]any{{float64(5)}}),
		rewriteFingerprint(cols, [][]any{{[]byte{0x35}}}),
		rewriteFingerprint(cols, [][]any{{bool(true)}}),
	}
	seen := map[string]bool{}
	for i, fp := range fps {
		if seen[fp] {
			t.Errorf("value types collided at index %d: %s", i, fp)
		}
		seen[fp] = true
	}
	// and the constraint identity is part of the digest: the same keys
	// under a different unique constraint are a different scope
	other := rewriteFingerprint([][]string{{"v"}}, [][]any{{int64(5)}})
	if other == fps[0] {
		t.Error("the same keys under a different constraint must not share the identity")
	}
	// multiple triggering constraints on one group
	dual := rewriteFingerprint([][]string{{"a"}, {"b"}}, [][]any{{int64(5)}})
	single := rewriteFingerprint([][]string{{"a"}}, [][]any{{int64(5)}})
	if dual == single {
		t.Error("the set of triggering constraints is part of the identity")
	}
}

// TestFingerprintSubsetAndNew pins the gate's two helpers: a subset (in
// any order) passes, anything outside the confirmed set fails; and the
// "new" count is the size of the difference, for the refusal message.
func TestFingerprintSubsetAndNew(t *testing.T) {
	cols := [][]string{{"u"}}
	fA := rewriteFingerprint(cols, [][]any{{"A"}})
	fB := rewriteFingerprint(cols, [][]any{{"B"}})
	fC := rewriteFingerprint(cols, [][]any{{"C"}})

	if !fingerprintSubset([]string{fA, fB}, []string{fA, fB}) {
		t.Error("the identical set is its own subset")
	}
	if !fingerprintSubset([]string{fB, fA}, []string{fA, fB}) {
		t.Error("order must not matter for the subset check")
	}
	if !fingerprintSubset([]string{fA}, []string{fA, fB}) {
		t.Error("a single confirmed group is a subset of two")
	}
	if !fingerprintSubset(nil, []string{fA}) {
		t.Error("the empty re-plan (converged) is always allowed")
	}
	if !fingerprintSubset(nil, nil) {
		t.Error("nothing against nothing is allowed")
	}
	if fingerprintSubset([]string{fA, fC}, []string{fA, fB}) {
		t.Error("an unconfirmed group must fail the subset check")
	}
	if fingerprintSubset([]string{fC}, []string{fA, fB}) {
		t.Error("a lone unconfirmed group must fail")
	}
	if got := fingerprintNew([]string{fA, fB, fC}, []string{fA, fB}); got != 1 {
		t.Errorf("new count = 1, got %d", got)
	}
	if got := fingerprintNew([]string{fA, fB}, []string{fA, fB}); got != 0 {
		t.Errorf("no new groups, got %d", got)
	}
}
