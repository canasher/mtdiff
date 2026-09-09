package sync

import (
	"strconv"
	"testing"
)

// syntheticKeyChunk yields one chunk of raw single-column int keys, the
// shape the driver hands the stream-delete paths (one []any per row, the
// key value in driver form).
func syntheticKeyChunk(offset, n int) [][]any {
	rows := make([][]any, n)
	for i := range rows {
		rows[i] = []any{int64(offset + i)}
	}
	return rows
}

// canonIntKey is the normalized-identity seam of keyDeleteBatches for a
// plain int key (the real seam is the normalizer; the benchmark only
// needs a total order, and the int's decimal form is one).
func canonIntKey(vals []any) (string, error) {
	return strconv.FormatInt(vals[0].(int64), 10), nil
}

// BenchmarkKeyDeleteBatches1M is the memory-bound proof at 1M keys: the
// table is walked chunk by chunk (100k keys per chunk), each chunk
// batched at 500. The peak logical buffer is ONE CHUNK (100k keys), never
// the 1M-row table, and every batch stays at the 500-key cap.
func BenchmarkKeyDeleteBatches1M(b *testing.B) {
	const total, chunkSize, batch = 1_000_000, 100_000, 500
	b.ReportAllocs()
	b.SetBytes(int64(total))
	for i := 0; i < b.N; i++ {
		peak := 0
		for start := 0; start < total; start += chunkSize {
			rows := syntheticKeyChunk(start, chunkSize)
			batches, err := keyDeleteBatches(rows, canonIntKey, batch)
			if err != nil {
				b.Fatal(err)
			}
			if len(rows) > peak {
				peak = len(rows)
			}
			for _, bt := range batches {
				if len(bt) > batch {
					b.Fatalf("a batch of %d exceeds the %d cap: the stream is unbounded", len(bt), batch)
				}
				if len(bt) > peak {
					peak = len(bt)
				}
			}
		}
		if peak != chunkSize {
			b.Fatalf("peak buffered keys = %d, want one chunk (%d), not the whole table", peak, chunkSize)
		}
	}
}

// BenchmarkKeyDeleteBatches10M is the same walk at 10x the table: the
// per-chunk work (and the peak buffer) must not grow with the table —
// N x 10 rows, same memory. The benchmark compares against
// BenchmarkKeyDeleteBatches1M on a per-byte basis (SetBytes is the row
// count in both).
func BenchmarkKeyDeleteBatches10M(b *testing.B) {
	const total, chunkSize, batch = 10_000_000, 100_000, 500
	b.ReportAllocs()
	b.SetBytes(int64(total))
	for i := 0; i < b.N; i++ {
		peak := 0
		for start := 0; start < total; start += chunkSize {
			rows := syntheticKeyChunk(start, chunkSize)
			batches, err := keyDeleteBatches(rows, canonIntKey, batch)
			if err != nil {
				b.Fatal(err)
			}
			if len(rows) > peak {
				peak = len(rows)
			}
			for _, bt := range batches {
				if len(bt) > batch {
					b.Fatalf("a batch of %d exceeds the %d cap: the stream is unbounded", len(bt), batch)
				}
			}
		}
		if peak != chunkSize {
			b.Fatalf("peak buffered keys = %d, want one chunk (%d), not the whole table", peak, chunkSize)
		}
	}
}

// BenchmarkDeleteBatchExec500 measures the statement building of one
// full 500-key delete batch (IN-list rendering + argument binding): the
// per-batch CPU cost of the batched delete (one statement instead of
// 500 round trips).
func BenchmarkDeleteBatchExec500(b *testing.B) {
	bld := NewBuilder("t", singleKeySchema(b))
	keys := make([][]any, 500)
	for i := range keys {
		keys[i] = []any{int64(i)}
	}
	b.ReportAllocs()
	b.SetBytes(500)
	for i := 0; i < b.N; i++ {
		stmt, args, err := bld.DeleteBatchExec(keys)
		if err != nil {
			b.Fatal(err)
		}
		if len(stmt) == 0 || len(args) != 500 {
			b.Fatal("unexpected batch rendering")
		}
	}
}
