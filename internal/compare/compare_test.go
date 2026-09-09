package compare

import (
	"context"
	"errors"
	"fmt"
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
	next := func() ([]any, bool) {
		if drained >= drillMaxRows+5 {
			return nil, false
		}
		drained++
		return []any{int64(drained)}, true
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
	nextSmall := func() ([]any, bool) {
		if small >= 2 {
			return nil, false
		}
		small++
		return []any{int64(1)}, true
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

// TestLookupKeyCollision covers the P3-#19 fix: string key components
// containing the " | " join separator used to collide across component
// boundaries in the keyed drill-down map. Components are now quoted.
func TestLookupKeyCollision(t *testing.T) {
	a := lookupKey([]any{"a", "b | c"})
	b := lookupKey([]any{"a | b", "c"})
	if a == b {
		t.Errorf("separator collision across components: both render as %q", a)
	}
	// a string and the same text as a number must stay distinct too
	if s := lookupKey([]any{"42"}); s == lookupKey([]any{int64(42)}) {
		t.Errorf("string \"42\" must not collide with int 42: %q", s)
	}
	// long components stay bounded (shortened before quoting)
	long := strings.Repeat("x", 500)
	if k := lookupKey([]any{long}); len(k) > 200 {
		t.Errorf("long key component must be shortened, got %d chars", len(k))
	}
}

// TestMultisetDiffKinds covers the P3-#20 fix: a keyless row present on
// both sides with differing multiplicity is a COUNT_DIFF, not an arbitrary
// MISSING_IN_DST.
func TestMultisetDiffKinds(t *testing.T) {
	d := &DrillDown{}
	mk := func(vals string, n int) *rowRec { return &rowRec{vals: vals, n: n} }
	src := map[string]*rowRec{"r1": mk("v1", 2), "r2": mk("v2", 1), "r3": mk("v3", 1)}
	dst := map[string]*rowRec{"r1": mk("v1", 3), "r2": mk("v2", 1), "r4": mk("v4", 1)}
	out := d.multisetDiff(src, dst, 10)
	if len(out) != 3 {
		t.Fatalf("want 3 differences, got %d: %+v", len(out), out)
	}
	byRow := map[string]RowDiff{}
	for _, r := range out {
		if r.SrcVals != "" {
			byRow[r.SrcVals] = r
		} else {
			byRow[r.DstVals] = r
		}
	}
	if r, ok := byRow["v1 x2"]; !ok || r.Kind != RowCountDiff {
		t.Errorf("both-sides count difference must be COUNT_DIFF, got %+v", r)
	}
	if r, ok := byRow["v3 x1"]; !ok || r.Kind != RowMissingInDst || r.DstVals != "" {
		t.Errorf("src-only row must be MISSING_IN_DST, got %+v", r)
	}
	if r, ok := byRow["v4 x1"]; !ok || r.Kind != RowMissingInSrc || r.SrcVals != "" {
		t.Errorf("dst-only row must be MISSING_IN_SRC, got %+v", r)
	}
}

// TestApplyKey covers the explicit --key override, the nullable-key
// warning, and the explicit-key uniqueness resolution (P0-1/P1-1): a
// resolver that proves the key unique (a real PK / NOT NULL UNIQUE
// index) flips KeyIsUnique on that side only, a resolver that denies it
// warns and keeps the key non-unique, and a failing resolver degrades to
// non-unique with a warning (conservative: an unproven key must never be
// treated as a row address).
func TestApplyKey(t *testing.T) {
	// no explicit key: schemas untouched, no warnings
	src, dst := schema("t", "a", "b"), schema("t", "a", "b")
	if warns := applyKey(src, dst, nil, nil); len(warns) != 0 || src.KeySource != "" || dst.KeySource != "" {
		t.Errorf("no key must leave schemas untouched: %v / %q", warns, src.KeySource)
	}

	// explicit key on a nullable column (no resolver: uniqueness unproven):
	// both sides switched, non-unique, one nullable warning
	src = schemaWithNullable("t", "k", "v")
	dst = schema("t", "k", "v")
	warns := applyKey(src, dst, []string{"k"}, nil)
	if src.KeySource != "explicit" || dst.KeySource != "explicit" {
		t.Errorf("key source must be explicit: %q / %q", src.KeySource, dst.KeySource)
	}
	if src.KeyIsUnique || dst.KeyIsUnique {
		t.Error("an unproven explicit key must not claim uniqueness")
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "column k is nullable") {
		t.Errorf("nullable key column must warn once, got %v", warns)
	}

	// explicit key on NOT NULL columns, proven unique on both sides
	// (P1-1: an explicit --key naming a real PK is recognized): no
	// warnings, KeyIsUnique on both sides
	src, dst = schema("t", "k", "v"), schema("t", "k", "v")
	warns = applyKey(src, dst, []string{"k"}, func(string) (bool, error) { return true, nil })
	if len(warns) != 0 {
		t.Errorf("proven-unique NOT NULL key must not warn: %v", warns)
	}
	if !src.KeyIsUnique || !dst.KeyIsUnique {
		t.Error("a proven-unique explicit key must be unique on both sides")
	}
	if src.Key[0] != "k" || dst.Key[0] != "k" {
		t.Errorf("key must be overridden on both sides: %v / %v", src.Key, dst.Key)
	}

	// proven unique on one side only: that side is unique, the other is
	// non-unique with a warning
	src, dst = schema("t", "k", "v"), schema("t", "k", "v")
	warns = applyKey(src, dst, []string{"k"}, func(side string) (bool, error) {
		return side == "src", nil
	})
	if !src.KeyIsUnique || dst.KeyIsUnique {
		t.Errorf("uniqueness is per side: src=%v dst=%v", src.KeyIsUnique, dst.KeyIsUnique)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "not a PRIMARY KEY or NOT NULL UNIQUE index on the dst side") {
		t.Errorf("the non-unique side must warn, got %v", warns)
	}

	// a resolver that fails: the key is treated as NON-unique with a
	// warning (conservative default, never an assumption)
	src, dst = schema("t", "k", "v"), schema("t", "k", "v")
	warns = applyKey(src, dst, []string{"k"}, func(string) (bool, error) { return false, errors.New("catalog down") })
	if src.KeyIsUnique || dst.KeyIsUnique {
		t.Error("an unresolvable explicit key must stay non-unique")
	}
	if len(warns) != 2 || !strings.Contains(warns[0], "could not be resolved") {
		t.Errorf("an unresolvable explicit key must warn per side, got %v", warns)
	}
}

func TestKeyFamilies(t *testing.T) {
	s := &conn.Schema{
		Table: "t",
		Cols:  []conn.Column{{Name: "a", Family: conn.FamINT, RawType: "int"}, {Name: "b", Family: conn.FamSTR, RawType: "varchar(10)"}},
		Key:   []string{"a", "b"},
	}
	if got := KeyFamilies(s); len(got) != 2 || got[0] != conn.FamINT || got[1] != conn.FamSTR {
		t.Errorf("KeyFamilies = %v, want [int str]", got)
	}
	// a key column missing from Cols (should not happen) yields an empty slot,
	// not a panic
	s2 := &conn.Schema{Table: "t", Cols: []conn.Column{{Name: "a", Family: conn.FamINT}}, Key: []string{"x", "a"}}
	if got := KeyFamilies(s2); len(got) != 2 || got[0] != "" || got[1] != conn.FamINT {
		t.Errorf("KeyFamilies missing column = %v", got)
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

// TestSkipRowScan covers the P3-#14 rule: differing row counts already make
// the table DIFFERENT, so planning and row scans are skipped — unless the
// operator asked for --drill row-level detail.
func TestSkipRowScan(t *testing.T) {
	cases := []struct {
		src, dst int64
		drill    bool
		want     bool
	}{
		{100, 100, false, false}, // equal counts: full comparison
		{100, 101, false, true},  // insert on dst: skip
		{101, 100, false, true},  // insert on src: skip
		{0, 5, false, true},
		{5, 0, false, true},
		{100, 101, true, false}, // drill wants row detail: still scan
		{101, 100, true, false}, // ditto
		{0, 0, false, false},    // both empty: nothing to skip (no chunks)
	}
	for _, c := range cases {
		if got := skipRowScan(c.src, c.dst, c.drill); got != c.want {
			t.Errorf("skipRowScan(%d, %d, drill=%v) = %v, want %v", c.src, c.dst, c.drill, got, c.want)
		}
	}
}

// TestPickScanError covers error attribution for the concurrent two-side
// scan (P3-#13): a side whose scan merely reported context cancellation
// (because the other side failed first) must not be blamed.
func TestPickScanError(t *testing.T) {
	canceled := context.Canceled
	wrapped := fmt.Errorf("query: %w", context.Canceled)
	cases := []struct {
		name     string
		src, dst error
		wantSide string // "", "src", "dst"
	}{
		{"both nil", nil, nil, ""},
		{"src fails", fmt.Errorf("boom"), nil, "src"},
		{"dst fails", nil, fmt.Errorf("boom"), "dst"},
		{"src canceled, dst real error", canceled, fmt.Errorf("boom"), "dst"},
		{"src real error, dst canceled", fmt.Errorf("boom"), wrapped, "src"},
		{"both canceled", canceled, wrapped, "src"}, // first non-nil wins
	}
	for _, c := range cases {
		err := pickScanError(c.src, c.dst)
		if c.wantSide == "" {
			if err != nil {
				t.Errorf("%s: want nil error, got %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.wantSide+" scan:") {
			t.Errorf("%s: want %s scan error, got %v", c.name, c.wantSide, err)
		}
	}
}

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

// The differing-chunk list must come out in CHUNK-ID ORDER no matter how
// the digest maps happen to iterate: the sync plan derives its APPLY
// ORDER from this list, and the cross-chunk unique-holder verdict is only
// sound when chunks apply sequentially in KEY order. A random order would
// apply the writer of a small-chunk unique value move before the chunk
// that frees its slot, and the unique index rejects the write (the flake
// behind TestUniqueHolderParallelOneDoesNotDeadlock). The digests live in
// maps, whose iteration order is randomized per run — so the fold is
// repeated many times against the same maps.
func TestFoldDigestsDiffChunksSorted(t *testing.T) {
	const n = 8
	chunks := make([]chunk.Chunk, n)
	src := make(map[int]mhash.ChunkDigest, n)
	dst := make(map[int]mhash.ChunkDigest, n)
	for i := 0; i < n; i++ {
		chunks[i] = chunk.Chunk{ID: i}
		src[i] = chunkDigest(true, i, 5)
		dst[i] = chunkDigest(true, i, 6) // every chunk differs
	}
	for i := 0; i < 200; i++ {
		res := TableResult{Name: "t", Status: "OK"}
		foldDigests(&res, chunks, src, dst, 5*n, 6*n, true, false)
		if res.Status != "DIFFERENT" || len(res.DiffChunks) != n {
			t.Fatalf("iteration %d: want DIFFERENT with %d diffs, got %s / %d", i, n, res.Status, len(res.DiffChunks))
		}
		for j := 1; j < len(res.DiffChunks); j++ {
			if res.DiffChunks[j-1].ID >= res.DiffChunks[j].ID {
				t.Fatalf("iteration %d: the diff list must be in ascending chunk order, got %v", i, idsOf(res.DiffChunks))
			}
		}
	}
}

func idsOf(ds []ChunkDiff) []int {
	out := make([]int, len(ds))
	for i, d := range ds {
		out[i] = d.ID
	}
	return out
}
