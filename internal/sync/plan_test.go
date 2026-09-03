package sync

import (
	"testing"

	"mtdiff/internal/compare"
)

func result(status string, src, dst int64, chunks ...int) compare.TableResult {
	r := compare.TableResult{Name: "t", SrcRows: src, DstRows: dst, Status: status}
	if status == "ERROR" {
		r.Error = "boom"
	}
	for _, id := range chunks {
		r.DiffChunks = append(r.DiffChunks, compare.ChunkDiff{ID: id})
	}
	return r
}

func TestDecidePlan(t *testing.T) {
	cases := []struct {
		name     string
		res      compare.TableResult
		srcKeyed bool
		dstKeyed bool
		where    string
		wantMode Mode
		wantErr  bool
	}{
		{"identical skips", result("OK", 10, 10), true, true, "", ModeSkip, false},
		{"error passes through", result("ERROR", 0, 0), true, true, "", ModeError, true},
		{"keyless with where is an error", result("DIFFERENT", 10, 10), false, true, "x=1", ModeError, true},
		{"keyless differs: full resync", result("DIFFERENT", 10, 10, 0), false, true, "", ModeFull, false},
		{"dst more rows: full resync", result("DIFFERENT", 10, 12), true, true, "", ModeFull, false},
		{"dst more rows but filtered: row-level", result("DIFFERENT", 10, 12, 1), true, true, "x=1", ModeRowLevel, false},
		{"dst fewer rows: row-level", result("DIFFERENT", 12, 10), true, true, "", ModeRowLevel, false},
		{"equal counts: row-level with chunk list", result("DIFFERENT", 10, 10, 1, 3), true, true, "", ModeRowLevel, false},
		// counts differ without --drill: the pre-pass skips planning, so the
		// chunk list stays empty and the engine must rescan everything
		{"count mismatch: row-level, no chunks", result("DIFFERENT", 12, 10), true, true, "", ModeRowLevel, false},
		// a keyless destination has no row addresses: the full resync is the
		// only mode that can converge, and --where cannot be honored at all
		{"keyed src, keyless dst: full resync", result("DIFFERENT", 10, 10, 0), true, false, "", ModeFull, false},
		{"keyed src, keyless dst, with where is an error", result("DIFFERENT", 10, 10), true, false, "x=1", ModeError, true},
		{"keyless src, keyed dst: full resync", result("DIFFERENT", 10, 10, 0), false, true, "", ModeFull, false},
		{"keyless on both sides, with where is an error", result("DIFFERENT", 10, 10), false, false, "x=1", ModeError, true},
	}
	for _, tc := range cases {
		p := DecidePlan(tc.res, tc.srcKeyed, tc.dstKeyed, tc.where)
		if p.Mode != tc.wantMode {
			t.Errorf("%s: mode = %s, want %s (plan %+v)", tc.name, p.Mode, tc.wantMode, p)
		}
		if (p.Error != "") != tc.wantErr {
			t.Errorf("%s: error = %q, wantErr %v", tc.name, p.Error, tc.wantErr)
		}
		if p.Table != tc.res.Name {
			t.Errorf("%s: table = %q", tc.name, p.Table)
		}
	}
}

func TestDecidePlanChunkList(t *testing.T) {
	p := DecidePlan(result("DIFFERENT", 10, 10, 0, 2, 4), true, true, "")
	if len(p.Chunks) != 3 || p.Chunks[0] != 0 || p.Chunks[1] != 2 || p.Chunks[2] != 4 {
		t.Errorf("chunks = %v, want [0 2 4]", p.Chunks)
	}
	// a count-mismatch table (no planned chunks) keeps an empty list
	p = DecidePlan(result("DIFFERENT", 12, 10), true, true, "")
	if len(p.Chunks) != 0 {
		t.Errorf("count-mismatch chunks = %v, want none", p.Chunks)
	}
}
