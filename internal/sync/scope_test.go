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

// TestScopeGateRewriteIdentity is the P0 rewrite rule: authorization is
// by GROUP IDENTITY, not count. The re-plan may run the confirmed groups
// (all, some, or none) and nothing else — a re-plan that keeps the COUNT
// but swaps the group ({A,B} -> {A,C}) is an expansion and must stop.
func TestScopeGateRewriteIdentity(t *testing.T) {
	cases := []struct {
		name     string
		conf     TableSync
		mode     Mode
		current  []string
		wantGate bool
	}{
		{"no rewrite confirmed, re-plan needs one", confirmed("ROWLEVEL", DestructiveScope{}, 0), ModeRowLevel, []string{"A"}, true},
		{"no rewrite confirmed, re-plan needs three", confirmed("ROWLEVEL", DestructiveScope{}, 0), ModeRowLevel, []string{"A", "B", "C"}, true},
		{"same group, confirmed and current", confirmed("ROWLEVEL", DestructiveScope{RowRewrite: true, RewriteFingerprints: []string{"A", "B"}}, 2), ModeRowLevel, []string{"A", "B"}, false},
		{"same group, current in swapped order (order is not identity)", confirmed("ROWLEVEL", DestructiveScope{RowRewrite: true, RewriteFingerprints: []string{"A", "B"}}, 2), ModeRowLevel, []string{"B", "A"}, false},
		{"subset of the confirmed groups (converged)", confirmed("ROWLEVEL", DestructiveScope{RowRewrite: true, RewriteFingerprints: []string{"A", "B"}}, 2), ModeRowLevel, []string{"A"}, false},
		{"nothing left to rewrite (fully converged)", confirmed("ROWLEVEL", DestructiveScope{RowRewrite: true, RewriteFingerprints: []string{"A", "B"}}, 2), ModeRowLevel, nil, false},
		{"SAME COUNT but a different group: {A,B} -> {A,C}", confirmed("ROWLEVEL", DestructiveScope{RowRewrite: true, RewriteFingerprints: []string{"A", "B"}}, 2), ModeRowLevel, []string{"A", "C"}, true},
		{"one confirmed group, one unconfirmed group: {A,B} -> {C}", confirmed("ROWLEVEL", DestructiveScope{RowRewrite: true, RewriteFingerprints: []string{"A", "B"}}, 2), ModeRowLevel, []string{"C"}, true},
		{"a superset of the confirmed groups: {A,B} -> {A,B,C}", confirmed("ROWLEVEL", DestructiveScope{RowRewrite: true, RewriteFingerprints: []string{"A", "B"}}, 2), ModeRowLevel, []string{"A", "B", "C"}, true},
		{"no rewrites at all", confirmed("ROWLEVEL", DestructiveScope{}, 0), ModeRowLevel, nil, false},
		{"confirmed full resync, row-level re-plan with no rewrites (shrink)", confirmed("FULL", DestructiveScope{FullResync: true}, 0), ModeRowLevel, nil, false},
	}
	for _, c := range cases {
		what := scopeGate(c.mode, rowWork{rewrites: len(c.current), rewriteFPs: c.current}, c.conf)
		if (what != "") != c.wantGate {
			t.Errorf("%s: gate = %q, want gated=%v", c.name, what, c.wantGate)
		}
	}
}

// TestScopeGateMessageLeadsWithCountsAndNoValues pins the operator-facing
// text: the refusal names the counts (confirmed / current / new) so the
// operator sees the scope change at a glance, and it must NEVER print
// the keys themselves — the message is shown to the operator, not to
// the data, and key values are business data.
func TestScopeGateMessageNamesTheDifference(t *testing.T) {
	// real fingerprints from real key material: the gate message must
	// carry only the digest-derived counts, never "Alice" or 1001
	fa := rewriteFingerprint([][]string{{"u"}}, [][]any{{"Alice"}})
	fb := rewriteFingerprint([][]string{{"u"}}, [][]any{{"Bob"}})
	fc := rewriteFingerprint([][]string{{"u"}}, [][]any{{int64(1001)}})
	conf := confirmed("ROWLEVEL", DestructiveScope{RowRewrite: true, RewriteFingerprints: []string{fa, fb}}, 2)
	what := scopeGate(ModeRowLevel, rowWork{rewrites: 3, rewriteFPs: []string{fa, fb, fc}}, conf)
	if !strings.Contains(what, "3") || !strings.Contains(what, "2") || !strings.Contains(what, "1") {
		t.Errorf("gate must name confirmed/current/new counts, got: %s", what)
	}
	if !strings.Contains(what, "DELETE+INSERT") {
		t.Errorf("gate must name the operation it refuses, got: %s", what)
	}
	for _, leak := range []string{"Alice", "Bob", "1001", fa, fb, fc} {
		if strings.Contains(what, leak) {
			t.Errorf("gate must not leak key material (%q), got: %s", leak, what)
		}
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
