package hash

import "testing"

func acc(id int, ordered bool, rows ...string) ChunkDigest {
	a := NewAccumulator(id, ordered)
	for _, r := range rows {
		a.AddRow([]byte(r))
	}
	return a.Digest()
}

func TestUnorderedPathRowOrderIndependent(t *testing.T) {
	f1 := TableFingerprint([]ChunkDigest{acc(0, false, "a", "b", "c")}, false, false)
	f2 := TableFingerprint([]ChunkDigest{acc(0, false, "c", "b", "a")}, false, false)
	if f1 != f2 {
		t.Error("unordered path must be independent of row order")
	}
	// re-partitioning the same multiset must not change the fingerprint
	f3 := TableFingerprint([]ChunkDigest{acc(0, false, "a"), acc(1, false, "b", "c")}, false, false)
	if f1 != f3 {
		t.Error("unordered path must be independent of chunk partitioning")
	}
}

func TestOrderedPathRowOrderDependent(t *testing.T) {
	f1 := TableFingerprint([]ChunkDigest{acc(0, true, "a", "b")}, true, false)
	f2 := TableFingerprint([]ChunkDigest{acc(0, true, "b", "a")}, true, false)
	if f1 == f2 {
		t.Error("ordered path must depend on row order")
	}
}

func TestFingerprintIndependentOfChunkCompletionOrder(t *testing.T) {
	c0, c1, c2 := acc(0, true, "a"), acc(1, true, "b"), acc(2, true, "c")
	f1 := TableFingerprint([]ChunkDigest{c0, c1, c2}, true, false)
	f2 := TableFingerprint([]ChunkDigest{c2, c0, c1}, true, false)
	if f1 != f2 {
		t.Error("table fingerprint must not depend on chunk completion order")
	}
}

func TestSumSqClosesSumXorGap(t *testing.T) {
	// {0,3} and {1,2} have equal count, sum and xor; only sum-of-squares differs.
	a := []ChunkDigest{{ID: 0, Count: 2, Sum: 3, Xor: 3, SumSq: 0 + 9, Ordered: false}}
	b := []ChunkDigest{{ID: 0, Count: 2, Sum: 3, Xor: 3, SumSq: 1 + 4, Ordered: false}}
	if f1, f2 := TableFingerprint(a, false, false), TableFingerprint(b, false, false); f1 == f2 {
		t.Error("SumSq must distinguish equal-sum/xor multisets")
	}
}

func TestSecureVsDefault(t *testing.T) {
	c := []ChunkDigest{acc(0, false, "x")}
	f1 := TableFingerprint(c, false, false)
	f2 := TableFingerprint(c, false, true)
	if f1 == f2 {
		t.Error("secure fingerprint must use the full 128 bits")
	}
	for i := 8; i < 16; i++ {
		if f1[i] != 0 {
			t.Error("default fingerprint must zero the upper half")
		}
	}
}
