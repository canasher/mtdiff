package sync

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The ApplyDrop TOCTOU re-gate (P0): the pre-pass discovered the table
// as destination-only at an earlier moment; the source may have created
// it since. A DROP is a destructive statement, so it is re-gated on
// fresh facts, and every refusal below is verified by the DROP executor
// never being CALLED — not by the error text alone.

// newDropTest wires an Applier whose execHook records every statement
// that reaches the executor, and leaves the Runner's probes unset so a
// case that forgets to set one fails loudly instead of touching a side.
func newDropTest(t *testing.T) (*Runner, *Applier, *[]string) {
	t.Helper()
	calls := []string{}
	r := &Runner{}
	ap := &Applier{}
	ap.execHook = func(ctx context.Context, query string) error {
		calls = append(calls, query)
		return nil
	}
	return r, ap, &calls
}

func dropCalls(calls []string) int {
	n := 0
	for _, q := range calls {
		if strings.Contains(q, "DROP TABLE") {
			n++
		}
	}
	return n
}

// TestApplyDropRefusesWhenSourceAppeared: the source created the table
// after the plan was confirmed. The drop must be REFUSED
// (ErrReplanRequired semantics: re-plan, re-confirm), the destination
// untouched — zero DROP executor calls — and the message tells the
// operator what happened and what to do.
func TestApplyDropRefusesWhenSourceAppeared(t *testing.T) {
	r, ap, calls := newDropTest(t)
	r.srcDropRecheck = func(context.Context, string) (bool, error) { return true, nil }
	r.dstDropRecheck = func(context.Context, string) (bool, error) { return true, nil }

	ts := r.ApplyDrop(context.Background(), "late_table", ap)
	if ts.Status != "FAILED" {
		t.Fatalf("source appeared: status = %q, want FAILED", ts.Status)
	}
	if !strings.Contains(ts.Error, "appeared after the plan was confirmed") {
		t.Errorf("must say the source table appeared: %s", ts.Error)
	}
	if !strings.Contains(ts.Error, "DROP was not executed") {
		t.Errorf("must state the DROP did not run: %s", ts.Error)
	}
	if !strings.Contains(ts.Error, "Re-run") {
		t.Errorf("must say to re-run so the new plan is reviewed: %s", ts.Error)
	}
	if n := dropCalls(*calls); n != 0 {
		t.Errorf("the DROP executor must not be called, got %d calls: %v", n, *calls)
	}
}

// TestApplyDropFailsClosedOnMetadataError: a re-check query that fails
// (connection loss, permission, backend without the schema) must VETO
// the destructive statement — fail closed, zero executor calls.
func TestApplyDropFailsClosedOnMetadataError(t *testing.T) {
	r, ap, calls := newDropTest(t)
	r.srcDropRecheck = func(context.Context, string) (bool, error) {
		return false, errors.New("source connection lost")
	}
	ts := r.ApplyDrop(context.Background(), "late_table", ap)
	if ts.Status != "FAILED" {
		t.Fatalf("metadata error: status = %q, want FAILED", ts.Status)
	}
	if !strings.Contains(ts.Error, "fail closed") {
		t.Errorf("must name the fail-closed decision: %s", ts.Error)
	}
	if n := dropCalls(*calls); n != 0 {
		t.Errorf("a metadata error must veto the DROP, got %d calls", n)
	}

	// the same rule applies to the destination re-check
	r2, ap2, calls2 := newDropTest(t)
	r2.srcDropRecheck = func(context.Context, string) (bool, error) { return false, nil }
	r2.dstDropRecheck = func(context.Context, string) (bool, error) {
		return false, errors.New("destination connection lost")
	}
	ts2 := r2.ApplyDrop(context.Background(), "late_table", ap2)
	if ts2.Status != "FAILED" || !strings.Contains(ts2.Error, "fail closed") {
		t.Errorf("destination metadata error: %+v / %s", ts2.Status, ts2.Error)
	}
	if n := dropCalls(*calls2); n != 0 {
		t.Errorf("a metadata error must veto the DROP, got %d calls", n)
	}
}

// TestApplyDropConvergesWhenDstGone: the destination table vanished
// out-of-band. The goal state (absence) is already reached — converged,
// nothing executed, and NOT a destructive failure.
func TestApplyDropConvergesWhenDstGone(t *testing.T) {
	r, ap, calls := newDropTest(t)
	r.srcDropRecheck = func(context.Context, string) (bool, error) { return false, nil }
	r.dstDropRecheck = func(context.Context, string) (bool, error) { return false, nil }

	ts := r.ApplyDrop(context.Background(), "vanished", ap)
	if ts.Status != "APPLIED" || ts.Verified != "OK" {
		t.Errorf("dst gone: %+v / %q, want APPLIED / OK (converged, not a failure)", ts.Status, ts.Verified)
	}
	if ts.Error != "" {
		t.Errorf("convergence carries no error, got: %s", ts.Error)
	}
	if n := dropCalls(*calls); n != 0 {
		t.Errorf("nothing may execute, got %d DROP calls", n)
	}
}

// TestApplyDropNormalPath: fresh facts agree with the confirmed plan
// (source lacks the table, destination has it, it is gone after the
// DROP) — the statement executes exactly once and verifies clean.
func TestApplyDropNormalPath(t *testing.T) {
	r, ap, calls := newDropTest(t)
	r.srcDropRecheck = func(context.Context, string) (bool, error) { return false, nil }
	r.dstDropRecheck = func(context.Context, string) (bool, error) { return true, nil }
	r.dstDropPostcheck = func(context.Context, string) (bool, error) { return false, nil }

	ts := r.ApplyDrop(context.Background(), "extra_table", ap)
	if ts.Status != "APPLIED" || ts.Verified != "OK" {
		t.Fatalf("normal drop: %+v / %q, want APPLIED / OK", ts.Status, ts.Verified)
	}
	if n := dropCalls(*calls); n != 1 {
		t.Fatalf("the DROP must execute exactly once, got %d calls: %v", n, *calls)
	}
	if !strings.Contains((*calls)[0], "extra_table") {
		t.Errorf("DROP statement must name the table: %s", (*calls)[0])
	}
}

// TestApplyDropStaleVerifyKeepsFailing: a DROP that reports success but
// leaves the table is still a failure (the post-drop verify re-queries
// and sees it).
func TestApplyDropStaleVerifyKeepsFailing(t *testing.T) {
	r, ap, calls := newDropTest(t)
	r.srcDropRecheck = func(context.Context, string) (bool, error) { return false, nil }
	r.dstDropRecheck = func(context.Context, string) (bool, error) { return true, nil }
	r.dstDropPostcheck = func(context.Context, string) (bool, error) { return true, nil }

	ts := r.ApplyDrop(context.Background(), "stubborn", ap)
	if ts.Status != "FAILED" || !strings.Contains(ts.Error, "still exists") {
		t.Errorf("table left behind: %+v / %s, want FAILED / still exists", ts.Status, ts.Error)
	}
	if n := dropCalls(*calls); n != 1 {
		t.Errorf("the DROP itself ran once, got %d", n)
	}
}
