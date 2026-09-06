package sync

import (
	"strings"
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
		name       string
		res        compare.TableResult
		srcKeyed   bool
		dstKeyed   bool
		srcUnique  bool
		dstUnique  bool
		keyAgree   bool // the usable keys are the same columns, in the same order, on both sides
		key        string
		where      string
		wantMode   Mode
		wantErr    bool
		wantArgErr bool // a misconfigured plan (where+non-unique/keyless), not a scan error
		wantErrSub string
	}{
		{"identical skips", result("OK", 10, 10), true, true, true, true, true, "id", "", ModeSkip, false, false, ""},
		{"error passes through", result("ERROR", 0, 0), true, true, true, true, true, "id", "", ModeError, true, false, ""},
		{"keyless with where is an error", result("DIFFERENT", 10, 10), false, true, false, true, true, "code", "x=1", ModeError, true, true, ""},
		{"keyless differs: full resync", result("DIFFERENT", 10, 10, 0), false, true, false, true, true, "code", "", ModeFull, false, false, ""},
		// extra rows on the destination are addressed by key and deleted:
		// the row counts never force a full resync
		{"dst more rows: row-level deletes", result("DIFFERENT", 10, 12), true, true, true, true, true, "id", "", ModeRowLevel, false, false, ""},
		{"dst more rows but filtered: row-level", result("DIFFERENT", 10, 12, 1), true, true, true, true, true, "id", "x=1", ModeRowLevel, false, false, ""},
		{"dst fewer rows: row-level", result("DIFFERENT", 12, 10), true, true, true, true, true, "id", "", ModeRowLevel, false, false, ""},
		{"equal counts: row-level with chunk list", result("DIFFERENT", 10, 10, 1, 3), true, true, true, true, true, "id", "", ModeRowLevel, false, false, ""},
		// counts differ without --drill: the pre-pass skips planning, so the
		// chunk list stays empty and the engine must rescan everything
		{"count mismatch: row-level, no chunks", result("DIFFERENT", 12, 10), true, true, true, true, true, "id", "", ModeRowLevel, false, false, ""},
		// a keyless destination has no row addresses: the full resync is the
		// only mode that can converge, and --where cannot be honored at all
		{"keyed src, keyless dst: full resync", result("DIFFERENT", 10, 10, 0), true, false, true, false, true, "id", "", ModeFull, false, false, ""},
		{"keyed src, keyless dst, with where is an error", result("DIFFERENT", 10, 10), true, false, true, false, true, "id", "x=1", ModeError, true, true, ""},
		{"keyless src, keyed dst: full resync", result("DIFFERENT", 10, 10, 0), false, true, false, true, true, "code", "", ModeFull, false, false, ""},
		{"keyless on both sides, with where is an error", result("DIFFERENT", 10, 10), false, false, false, false, true, "", "x=1", ModeError, true, true, ""},
		// P0-1: with a NON-unique key (an explicit --key that is not a PK
		// or NOT NULL UNIQUE index), a filtered row-level sync would delete
		// key GROUPS the filter excluded: an argument error, in the dry run
		// and in the apply alike, before any write
		{"non-unique src key + where is rejected", result("DIFFERENT", 10, 12), true, true, false, true, true, "code", "x=1", ModeError, true, true, "unique row-addressing"},
		{"non-unique dst key + where is rejected", result("DIFFERENT", 10, 12), true, true, true, false, true, "code", "x=1", ModeError, true, true, "unique row-addressing"},
		// without --where a non-unique key is safe: the engine replaces key
		// groups (delete+insert) instead of updating single rows
		{"non-unique key, unfiltered: row-level", result("DIFFERENT", 10, 12), true, true, false, false, true, "code", "", ModeRowLevel, false, false, ""},
		// the usable keys differ between the sides (names or order, e.g.
		// PK (a,b) vs (b,a) under --no-sync-schema): the source's key
		// bounds cannot be rendered against the destination's key
		// columns, so the row-level mode is impossible — the full resync
		// converges without addressing destination keys
		{"keys differ, unfiltered: full resync", result("DIFFERENT", 10, 10, 0), true, true, true, true, false, "a,b", "", ModeFull, false, false, ""},
		{"keys differ, with where is an error", result("DIFFERENT", 10, 10), true, true, true, true, false, "a,b", "x=1", ModeError, true, true, "SAME usable key"},
	}
	for _, tc := range cases {
		p := DecidePlan(tc.res, tc.srcKeyed, tc.dstKeyed, tc.srcUnique, tc.dstUnique, tc.key, tc.where, tc.keyAgree)
		if p.Mode != tc.wantMode {
			t.Errorf("%s: mode = %s, want %s (plan %+v)", tc.name, p.Mode, tc.wantMode, p)
		}
		if (p.Error != "") != tc.wantErr {
			t.Errorf("%s: error = %q, wantErr %v", tc.name, p.Error, tc.wantErr)
		}
		if tc.wantErrSub != "" && !strings.Contains(p.Error, tc.wantErrSub) {
			t.Errorf("%s: error = %q, want it to contain %q", tc.name, p.Error, tc.wantErrSub)
		}
		if tc.wantArgErr && !p.ArgErr {
			t.Errorf("%s: a misconfigured plan must be an argument error (ArgErr), plan %+v", tc.name, p)
		}
		if p.Table != tc.res.Name {
			t.Errorf("%s: table = %q", tc.name, p.Table)
		}
	}
}

func TestDecidePlanChunkList(t *testing.T) {
	p := DecidePlan(result("DIFFERENT", 10, 10, 0, 2, 4), true, true, true, true, "id", "", true)
	if len(p.Chunks) != 3 || p.Chunks[0] != 0 || p.Chunks[1] != 2 || p.Chunks[2] != 4 {
		t.Errorf("chunks = %v, want [0 2 4]", p.Chunks)
	}
	// a count-mismatch table (no planned chunks) keeps an empty list
	p = DecidePlan(result("DIFFERENT", 12, 10), true, true, true, true, "id", "", true)
	if len(p.Chunks) != 0 {
		t.Errorf("count-mismatch chunks = %v, want none", p.Chunks)
	}
	// the plan's chunk list is the APPLY ORDER: whatever order the chunk
	// list arrives in, the plan must execute the chunks in KEY ORDER
	// (the cross-chunk unique-holder verdict is only sound then)
	p = DecidePlan(result("DIFFERENT", 10, 10, 7, 2, 0, 5), true, true, true, true, "id", "", true)
	if len(p.Chunks) != 4 || p.Chunks[0] != 0 || p.Chunks[1] != 2 || p.Chunks[2] != 5 || p.Chunks[3] != 7 {
		t.Errorf("shuffled chunk list must come out in key order, got %v, want [0 2 5 7]", p.Chunks)
	}
}
