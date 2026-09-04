package sync

import (
	"strings"
	"testing"
)

// confirmed is a TableSync carrying the scope the preflight recorded.
func confirmed(mode Mode, scope DestructiveScope, rewrites int) TableSync {
	return TableSync{Mode: string(mode), Scope: scope, Rewrites: rewrites}
}

// TestScopeGateRowLevelNeverTruncates is spec item 1: a confirmed
// ROWLEVEL plan whose apply-time re-plan escalates (stale plan, data
// moved, a cross-chunk swap under --allow-row-rewrite) must be stopped,
// never TRUNCATEd. The gate is the single decision point between the
// re-plan and any destructive statement.
func TestScopeGateRowLevelNeverTruncates(t *testing.T) {
	conf := confirmed("ROWLEVEL", DestructiveScope{}, 0)
	what := scopeGate(ModeFull, rowWork{}, conf)
	if what == "" {
		t.Fatal("a re-plan that escalates to the full resync must be gated on a confirmed row-level plan")
	}
	if !strings.Contains(what, "TRUNCATE") {
		t.Errorf("gate must name the TRUNCATE it refuses, got: %s", what)
	}
	// the same re-plan is fine when the confirmed plan is the FULL one
	// that showed the TRUNCATE (spec item 6)
	confFull := confirmed("FULL", DestructiveScope{FullResync: true}, 0)
	if what := scopeGate(ModeFull, rowWork{}, confFull); what != "" {
		t.Errorf("a confirmed full plan may run the full resync, got: %s", what)
	}
}

// TestScopeGateRewriteCount is spec items 2 and 4: a confirmed plan that
// showed no rewrites refuses any rewrite the re-plan needs (a unique
// swap that appeared, or a cross-chunk conflict), and a plan that showed
// N refuses more than N (the difference means data moved). Shrinkage is
// always allowed: a confirmed plan that showed rewrites may run fewer or
// none (the data converged).
func TestScopeGateRewriteCount(t *testing.T) {
	cases := []struct {
		name     string
		conf     TableSync
		mode     Mode
		rewrites int
		wantGate bool
	}{
		{"no rewrite confirmed, re-plan needs one", confirmed("ROWLEVEL", DestructiveScope{}, 0), ModeRowLevel, 1, true},
		{"no rewrite confirmed, re-plan needs three", confirmed("ROWLEVEL", DestructiveScope{}, 0), ModeRowLevel, 3, true},
		{"rewrite confirmed, re-plan needs more", confirmed("ROWLEVEL", DestructiveScope{RowRewrite: true}, 2), ModeRowLevel, 5, true},
		{"rewrite confirmed, re-plan needs the same", confirmed("ROWLEVEL", DestructiveScope{RowRewrite: true}, 2), ModeRowLevel, 2, false},
		{"rewrite confirmed, re-plan needs fewer (converged)", confirmed("ROWLEVEL", DestructiveScope{RowRewrite: true}, 2), ModeRowLevel, 0, false},
		{"no rewrites at all", confirmed("ROWLEVEL", DestructiveScope{}, 0), ModeRowLevel, 0, false},
		{"confirmed full resync, row-level re-plan with no rewrites (shrink)", confirmed("FULL", DestructiveScope{FullResync: true}, 0), ModeRowLevel, 0, false},
	}
	for _, c := range cases {
		what := scopeGate(c.mode, rowWork{rewrites: c.rewrites}, c.conf)
		if (what != "") != c.wantGate {
			t.Errorf("%s: gate = %q, want gated=%v", c.name, what, c.wantGate)
		}
	}
}

// TestScopeGateMessageNamesTheDifference pins the operator-facing text:
// the refusal must say what the re-plan needs and what the confirmed
// plan showed, so the operator can see the scope expansion at a glance.
func TestScopeGateMessageNamesTheDifference(t *testing.T) {
	conf := confirmed("ROWLEVEL", DestructiveScope{RowRewrite: true}, 2)
	what := scopeGate(ModeRowLevel, rowWork{rewrites: 5}, conf)
	if !strings.Contains(what, "5") || !strings.Contains(what, "2") {
		t.Errorf("gate must name both counts, got: %s", what)
	}
	if !strings.Contains(what, "DELETE+INSERT") {
		t.Errorf("gate must name the operation it refuses, got: %s", what)
	}
}

// TestScopeGateFullShownButNotConfirmed covers the inverse of item 6:
// the confirmed plan is ROWLEVEL (no TRUNCATE shown) and the pre-pass
// itself planned FULL (e.g. the two sides' keys stopped agreeing between
// the pre-pass and the apply). The re-plan is FULL; the gate stops it.
func TestScopeGateFullShownButNotConfirmed(t *testing.T) {
	conf := confirmed("ROWLEVEL", DestructiveScope{}, 0)
	if what := scopeGate(ModeFull, rowWork{}, conf); what == "" {
		t.Fatal("a FULL re-plan must be gated when the confirmed plan showed no TRUNCATE")
	}
}
