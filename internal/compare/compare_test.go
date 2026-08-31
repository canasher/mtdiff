package compare

import (
	"database/sql/driver"
	"strings"
	"testing"

	"mtdiff/internal/chunk"
	"mtdiff/internal/conn"
	mhash "mtdiff/internal/hash"
	"mtdiff/internal/normalize"
)

func schema(table string, cols ...string) *conn.Schema {
	s := &conn.Schema{Table: table}
	for _, name := range cols {
		s.Cols = append(s.Cols, conn.Column{Name: name, Family: conn.FamINT, RawType: "int"})
	}
	return s
}

// schemaWithNullable builds a schema whose key column is nullable.
func schemaWithNullable(table, keyCol string, rest ...string) *conn.Schema {
	s := &conn.Schema{Table: table}
	s.Cols = append(s.Cols, conn.Column{Name: keyCol, Family: conn.FamINT, RawType: "int", Nullable: true})
	for _, name := range rest {
		s.Cols = append(s.Cols, conn.Column{Name: name, Family: conn.FamINT, RawType: "int"})
	}
	return s
}

func ignoreSet(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// TestFilterIgnoredDrift covers the P2-4 regression: with --ignore-columns
// set, a destination-only column used to be silently dropped from the
// comparison (while the same drift with an empty ignore list is a hard
// error). It must now surface as an error.
func TestFilterIgnoredDrift(t *testing.T) {
	// no ignore list: dst drift is reported by Compatible, not here; the
	// fast path must return the schemas untouched
	src, dst, err := filterIgnored(schema("t", "a", "b"), schema("t", "a", "b", "c"), nil)
	if err != nil || src.Cols[1].Name != "b" || dst.Cols[2].Name != "c" {
		t.Fatalf("empty ignore must pass through: err=%v", err)
	}

	// dst-only column, ignored on neither side: hard error
	_, _, err = filterIgnored(schema("t", "a", "b"), schema("t", "a", "b", "c"), ignoreSet("x"))
	if err == nil || !strings.Contains(err.Error(), "neither compared nor listed") {
		t.Errorf("dst-only column must error, got %v", err)
	}

	// misspelled ignore name: keeps everything compared, and the dst-only
	// column is still an error (the typo must not silently absorb it)
	_, _, err = filterIgnored(schema("t", "a", "b"), schema("t", "a", "b", "c"), ignoreSet("upadted_at"))
	if err == nil {
		t.Error("misspelled ignore name must not suppress the dst-only column error")
	}

	// dst-only column that IS ignored: fine, it is excluded on purpose
	s2, d2, err := filterIgnored(schema("t", "a", "b"), schema("t", "a", "b", "c"), ignoreSet("c"))
	if err != nil {
		t.Fatalf("ignored dst-only column must pass: %v", err)
	}
	if len(s2.Cols) != 2 || len(d2.Cols) != 2 {
		t.Errorf("both sides must keep the compared columns: %v / %v", s2.Cols, d2.Cols)
	}
	if d2.Cols[1].Name != "b" {
		t.Errorf("destination order must follow source: %+v", d2.Cols)
	}

	// every source column ignored: hard error
	if _, _, err := filterIgnored(schema("t", "a", "b"), schema("t", "a", "b"), ignoreSet("a", "b")); err == nil {
		t.Error("ignoring all columns must error")
	}

	// a compared source column missing on the destination: hard error
	if _, _, err := filterIgnored(schema("t", "a", "b"), schema("t", "a"), ignoreSet("x")); err == nil {
		t.Error("missing compared column must error")
	}
}

// TestDrillDownRowCap covers the P2-7 regression: drilling a keyless
// whole-table chunk used to materialize every row of both sides in memory.
// The buffer must cap at drillMaxRows, keep draining the stream, and report
// truncated so the caller can label the result a sample.
func TestDrillDownRowCap(t *testing.T) {
	norm := normalize.NewNormalizer(
		[]conn.Column{{Name: "v", Family: conn.FamINT, RawType: "int"}},
		normalize.DefaultOptions())
	d := &DrillDown{}

	drained := 0
	next := func() ([]driver.Value, bool) {
		if drained >= drillMaxRows+5 {
			return nil, false
		}
		drained++
		return []driver.Value{int64(drained)}, true
	}
	out, truncated, err := d.bufferRows(false, 0, 1, norm, next)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Error("stream past the cap must report truncated")
	}
	if len(out) != drillMaxRows {
		t.Errorf("buffer must cap at %d rows, got %d", drillMaxRows, len(out))
	}
	if drained != drillMaxRows+5 {
		t.Errorf("iterator must keep being drained past the cap, stopped at %d", drained)
	}

	// a small stream is fully buffered and not truncated
	small := 0
	nextSmall := func() ([]driver.Value, bool) {
		if small >= 2 {
			return nil, false
		}
		small++
		return []driver.Value{int64(1)}, true
	}
	out, truncated, err = d.bufferRows(false, 0, 1, norm, nextSmall)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Error("small stream must not be truncated")
	}
	if len(out) != 1 {
		t.Fatalf("duplicate rows must fold into one entry, got %d entries", len(out))
	}
	if rec := out[norm2canonKey(out)]; rec.n != 2 {
		t.Errorf("duplicate rows must count multiplicity, got n=%d", rec.n)
	}
}

// norm2canonKey finds the single buffered key (test helper for maps).
func norm2canonKey(m map[string]*rowRec) string {
	for k := range m {
		return k
	}
	return ""
}

// TestApplyKey covers the explicit --key override and the nullable-key
// warning (P1-2): an explicit key on a column that accepts NULL must
// surface as a warning.
func TestApplyKey(t *testing.T) {
	// no explicit key: schemas untouched, no warnings
	src, dst := schema("t", "a", "b"), schema("t", "a", "b")
	if warns := applyKey(src, dst, nil); len(warns) != 0 || src.KeySource != "" || dst.KeySource != "" {
		t.Errorf("no key must leave schemas untouched: %v / %q", warns, src.KeySource)
	}

	// explicit key on a nullable column: both sides switched, one warning
	src = schemaWithNullable("t", "k", "v")
	dst = schema("t", "k", "v")
	warns := applyKey(src, dst, []string{"k"})
	if src.KeySource != "explicit" || dst.KeySource != "explicit" {
		t.Errorf("key source must be explicit: %q / %q", src.KeySource, dst.KeySource)
	}
	if src.KeyIsUnique || dst.KeyIsUnique {
		t.Error("explicit key must not claim uniqueness")
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "column k is nullable") {
		t.Errorf("nullable key column must warn once, got %v", warns)
	}

	// explicit key on NOT NULL columns: override, no warnings
	src, dst = schema("t", "k", "v"), schema("t", "k", "v")
	if warns := applyKey(src, dst, []string{"k"}); len(warns) != 0 {
		t.Errorf("NOT NULL key must not warn: %v", warns)
	}
	if src.Key[0] != "k" || dst.Key[0] != "k" {
		t.Errorf("key must be overridden on both sides: %v / %v", src.Key, dst.Key)
	}
}

// chunkDigest builds a digest with distinct statistics per (id, count).
func chunkDigest(ordered bool, id, count int) mhash.ChunkDigest {
	d := mhash.ChunkDigest{ID: id, Count: uint64(count)}
	if ordered {
		d.Ordered = true
		d.Order = uint64(count)*7 + uint64(id)
	} else {
		d.Sum = uint64(count)
		d.Xor = uint64(count + id)
		d.SumSq = uint64(count * (count + id))
	}
	return d
}

// TestFoldDigests covers the verdict folding, in particular the P2-9
// count-mismatch branch: equal chunk digests but differing row counts must
// still mark the table DIFFERENT.
func TestFoldDigests(t *testing.T) {
	// identical digests and counts: OK
	res := TableResult{Name: "t", Status: "OK"}
	byID := foldDigests(&res, []chunk.Chunk{{ID: 0}}, map[int]mhash.ChunkDigest{0: chunkDigest(true, 0, 5)}, map[int]mhash.ChunkDigest{0: chunkDigest(true, 0, 5)}, 5, 5, true, false)
	if res.Status != "OK" || len(res.DiffChunks) != 0 {
		t.Errorf("identical sides must be OK: %+v", res)
	}
	if byID == nil || len(byID) != 1 {
		t.Errorf("chunk lookup table must be returned, got %v", byID)
	}
	if res.SrcFP == "" || res.SrcFP != res.DstFP {
		t.Errorf("fingerprints must be set and equal: %q / %q", res.SrcFP, res.DstFP)
	}

	// differing counts with identical digests: DIFFERENT (the branch that
	// used to be untestable without a database)
	res = TableResult{Name: "t", Status: "OK"}
	foldDigests(&res, nil, map[int]mhash.ChunkDigest{0: chunkDigest(true, 0, 5)}, map[int]mhash.ChunkDigest{0: chunkDigest(true, 0, 5)}, 5, 6, true, false)
	if res.Status != "DIFFERENT" {
		t.Errorf("row count mismatch must be DIFFERENT, got %s", res.Status)
	}

	// differing chunk digest with equal counts: DIFFERENT + one DiffChunk
	res = TableResult{Name: "t", Status: "OK"}
	foldDigests(&res, []chunk.Chunk{{ID: 0}}, map[int]mhash.ChunkDigest{0: chunkDigest(true, 0, 5)}, map[int]mhash.ChunkDigest{0: chunkDigest(true, 0, 6)}, 5, 5, true, false)
	if res.Status != "DIFFERENT" || len(res.DiffChunks) != 1 {
		t.Errorf("differing chunk must be DIFFERENT with one diff: %+v", res)
	}

	// digest sets of different lengths: DIFFERENT, no lookup table
	res = TableResult{Name: "t", Status: "OK"}
	byID = foldDigests(&res, nil, map[int]mhash.ChunkDigest{0: chunkDigest(false, 0, 1)}, map[int]mhash.ChunkDigest{}, 1, 0, false, false)
	if res.Status != "DIFFERENT" || byID != nil {
		t.Errorf("unmatched digest sets must be DIFFERENT with nil lookup: %+v", res)
	}
}
