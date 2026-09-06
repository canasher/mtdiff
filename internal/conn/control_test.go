package conn

// Unit tests for the CONTROL-connection policy (R6-1), against the fake
// server of fake_driver_test.go: the control pool's physical connection
// can be replaced (a server KILL, a network partition, a restart) and
// the replacement must come back policy-initialized before it may serve
// a single metadata query. A recycled CONNECTION_ID (the server's
// counter reset across a restart) must not exempt the fresh session —
// there is no memo to consult, the policy is simply re-applied.
//
// A connection the policy cannot be applied to is closed and never
// handed out, and a refused connection must serve no query.

import (
	"context"
	"strings"
	"testing"
)

// The control-side counterpart of TestAcquireScanReappliesPolicyOn
// RecycledID: the restart hands the SAME CONNECTION_ID to a fresh
// physical session; the replacement control connection must still
// receive the full policy (from an empty start) before the metadata
// query may run on it.
func TestControlReappliesPolicyOnRecycledID(t *testing.T) {
	srv := &fakeServer{fixedID: 7} // every physical connection reports id 7
	side := openFakeSide(t, srv)
	ctx := context.Background()

	first := srv.conns[0] // the control connection (it ran the VERSION probe)
	q, err := side.Control(ctx)
	if err != nil {
		t.Fatalf("first control checkout: %v", err)
	}
	q.Close()
	if len(first.execs) == 0 {
		t.Fatal("the first control checkout must have applied the policy")
	}

	// the restart: the physical connection is killed; the pool opens a
	// NEW one, which reports the SAME connection id
	first.kill()

	// a control metadata query after the restart: the checkout finds the
	// dead connection (the policy's SETs cannot run on it), the session
	// swaps in the fresh one — the full policy re-applied BEFORE the
	// query may run on it
	q2, err := side.Control(ctx)
	if err != nil {
		t.Fatalf("control checkout after the restart (dead conn replaced, policy re-applied): %v", err)
	}
	defer q2.Close()
	if len(srv.conns) != 3 {
		t.Fatalf("the pool must have opened a NEW physical control connection, got %d conns", len(srv.conns))
	}
	second := srv.conns[2]
	if second.state["innodb_lock_wait_timeout"] != "5" || second.state["txn_read_only"] != "1" ||
		!strings.Contains(second.state["sql_mode"], "NO_ZERO_DATE") {
		t.Fatalf("the recycled-ID control connection was handed out WITHOUT the policy: state=%v", second.state)
	}
	if second.connID() != first.connID() {
		t.Fatalf("the scenario requires a RECYCLED id (both %d), got %d vs %d",
			first.connID(), second.connID(), first.connID())
	}
	var one int
	if err := OneRow(ctx, q2, "SELECT 1", []any{&one}); err != nil {
		t.Fatalf("metadata query on the replacement control connection: %v", err)
	}
	ran := false
	for _, qq := range second.queries {
		if strings.Contains(qq, "SELECT 1") {
			ran = true
		}
	}
	if !ran {
		t.Fatalf("the metadata query must run on the replacement connection; its queries=%v", second.queries)
	}
}

// Repeated checkout: even the SAME physical control connection is
// re-checked on every checkout — an out-of-band session reset cannot
// leave the pool serving an unguarded session.
func TestControlReappliesPolicyOnSamePhysicalCheckout(t *testing.T) {
	srv := &fakeServer{fixedID: 7}
	side := openFakeSide(t, srv)
	ctx := context.Background()

	q, err := side.Control(ctx)
	if err != nil {
		t.Fatalf("first checkout: %v", err)
	}
	q.Close()
	first := srv.conns[0]
	if first.state["innodb_lock_wait_timeout"] != "5" {
		t.Fatalf("the first checkout must have set the guardrail, state=%v", first.state)
	}

	// an out-of-band reset: the guardrail and the read-only tier go
	// back to the server defaults on the same physical session
	first.state["innodb_lock_wait_timeout"] = "50"
	first.state["txn_read_only"] = ""

	q2, err := side.Control(ctx) // single-connection pool: the same physical connection
	if err != nil {
		t.Fatalf("second checkout: %v", err)
	}
	defer q2.Close()
	if len(srv.conns) != 2 {
		t.Fatalf("the same physical connection must be reused, got %d conns", len(srv.conns))
	}
	if first.state["innodb_lock_wait_timeout"] != "5" {
		t.Fatalf("the second checkout must re-apply the guardrail, state=%v", first.state)
	}
	if first.state["txn_read_only"] != "1" {
		t.Fatalf("the second checkout must re-apply the read-only tier, state=%v", first.state)
	}
}

// A connection the policy can no longer be applied to is closed and NOT
// handed out, and a refused connection serves no query — even one that
// was initialized before the backend started refusing the read-only
// tiers.
func TestControlPolicyFailureNoQueryNoHandout(t *testing.T) {
	srv := &fakeServer{fixedID: 7}
	side := openFakeSide(t, srv)
	ctx := context.Background()

	q, err := side.Control(ctx)
	if err != nil {
		t.Fatalf("initial checkout: %v", err)
	}
	q.Close()

	// from here on the backend rejects BOTH read-only tiers
	srv.refuseRO = true

	if _, err := side.Control(ctx); err == nil {
		t.Fatal("a connection the policy cannot be applied to must NOT be handed out")
	} else if !strings.Contains(err.Error(), "read-only") {
		t.Errorf("the refusal must name the read-only enforcement, got: %v", err)
	}
	// the refused connection is never handed out, no matter how often a
	// checkout is tried: it may sit back in the pool (Conn.Close
	// returns, not discards), but every checkout re-applies the policy
	// and refuses again — an unguarded session cannot be checked out
	for i := 0; i < 3; i++ {
		if _, err := side.Control(ctx); err == nil {
			t.Fatalf("checkout %d: every checkout of the unguardable connection must be refused (refuseRO is still on)", i)
		}
	}
	// and while refused it served no query: only the OpenSide probes
	// and the (failed) guardrail probe ran on it
	for _, qq := range srv.conns[0].queries {
		if !strings.Contains(qq, "SELECT 1") && !strings.Contains(qq, "SELECT VERSION()") &&
			!strings.Contains(strings.ToUpper(qq), "SELECT @@SESSION.SQL_MODE") {
			t.Fatalf("the refused connection served a query: %q", qq)
		}
	}
}
