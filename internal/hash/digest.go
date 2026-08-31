// Package hash turns canonical row bytes into chunk and table fingerprints.
//
// Ordered path (keyed tables): the chunk digest is a sequence hash over the
// per-row hashes in key order. Unordered path (keyless tables): a four-
// statistic multiset fingerprint (count, sum, xor, sum-of-squares) that is
// independent of row order. The table fingerprint folds chunk digests
// sorted by chunk ID, so it is bit-identical regardless of concurrency.
package hash

import (
	"encoding/binary"
	"sort"

	"github.com/cespare/xxhash/v2"
)

// ChunkDigest is the fingerprint of one scanned chunk.
type ChunkDigest struct {
	ID      int
	Count   uint64
	Order   uint64 // ordered path: sequence hash of row hashes
	Sum     uint64 // unordered path: ΣH
	Xor     uint64 // unordered path: ⊕H
	SumSq   uint64 // unordered path: ΣH·H (mod 2^64)
	Ordered bool
}

// Accumulator incrementally builds a ChunkDigest from canonical row bytes.
type Accumulator struct {
	chunk ChunkDigest
	seq   *xxhash.Digest
}

// NewAccumulator creates an accumulator for the chunk with the given ID.
func NewAccumulator(id int, ordered bool) *Accumulator {
	return &Accumulator{
		chunk: ChunkDigest{ID: id, Ordered: ordered},
		seq:   xxhash.New(),
	}
}

// AddRow hashes one canonical row and folds it into the running digest.
func (a *Accumulator) AddRow(canonical []byte) {
	h := xxhash.Sum64(canonical)
	a.chunk.Count++
	if a.chunk.Ordered {
		var tmp [8]byte
		binary.LittleEndian.PutUint64(tmp[:], h)
		a.seq.Write(tmp[:])
	} else {
		a.chunk.Sum += h
		a.chunk.Xor ^= h
		a.chunk.SumSq += h * h
	}
}

// Digest returns the finished chunk fingerprint.
func (a *Accumulator) Digest() ChunkDigest {
	if a.chunk.Ordered {
		a.chunk.Order = a.seq.Sum64()
	}
	return a.chunk
}

// TableFingerprint folds chunk digests into a 16-byte table fingerprint.
// Chunk order is normalized (sorted by ID) internally, so the result is
// independent of the order in which chunks were scanned. With secure false
// only the lower 64 bits are used (as in pt-table-checksum); secure true
// adds a second seeded hash for 128 bits.
func TableFingerprint(chunks []ChunkDigest, ordered, secure bool) [16]byte {
	sorted := make([]ChunkDigest, len(chunks))
	copy(sorted, chunks)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	buf := make([]byte, 0, len(sorted)*32)
	if ordered {
		for _, c := range sorted {
			buf = appendU64(buf, c.Count)
			buf = appendU64(buf, c.Order)
		}
	} else {
		// Aggregate the multiset statistics across ALL chunks before
		// fingerprinting: the result is then independent of chunk
		// partitioning (sums add, xors fold) as well as of chunk order.
		var count, sum, xor, sumSq uint64
		for _, c := range sorted {
			count += c.Count
			sum += c.Sum
			xor ^= c.Xor
			sumSq += c.SumSq
		}
		buf = appendU64(buf, count)
		buf = appendU64(buf, sum)
		buf = appendU64(buf, xor)
		buf = appendU64(buf, sumSq)
	}
	h1 := xxhash.New()
	h1.Write(buf)
	var out [16]byte
	binary.LittleEndian.PutUint64(out[:8], h1.Sum64())
	if secure {
		h2 := xxhash.NewWithSeed(0x9E3779B97F4A7C15)
		h2.Write(buf)
		binary.LittleEndian.PutUint64(out[8:], h2.Sum64())
	}
	return out
}

func appendU64(buf []byte, v uint64) []byte {
	start := len(buf)
	buf = append(buf, make([]byte, 8)...)
	binary.LittleEndian.PutUint64(buf[start:], v)
	return buf
}
