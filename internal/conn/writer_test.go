package conn

// Unit tests for the WRITER-connection guardrails (R6-3), against the
// fake server of fake_driver_test.go: the single write connection can
// be replaced (a network failure, a KILL, a restart) and the
// replacement must come back guardrailed — Conn() re-applies the
// best-effort guardrails on EVERY checkout, not just at OpenWriter
// time. A session reset on the SAME physical connection is recovered
// by the next checkout.

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func openFakeWriter(t *testing.T, srv *fakeServer) *Writer {
	t.Helper()
	useFakeDB(t, srv)
	w, err := OpenWriter(context.Background(), "dst", fakeEndpoint(), 0)
	if err != nil {
		t.Fatalf("OpenWriter against the fake: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

// The replacement scenario: the physical writer connection is killed;
// the next checkout that actually works must be a NEW physical session
// that received the guardrails from an empty start.
func TestWriterReappliesGuardrailsOnReplacement(t *testing.T) {
	srv := &fakeServer{}
	w := openFakeWriter(t, srv)
	ctx := context.Background()

	c1, err := w.Conn(ctx)
	if err != nil {
		t.Fatalf("first checkout: %v", err)
	}
	c1.Close()
	first := srv.conns[0]
	if first.state["innodb_lock_wait_timeout"] != "5" {
		t.Fatalf("the first checkout must have set the guardrail, state=%v", first.state)
	}

	// the network failure: the physical connection is killed
	first.kill()

	// the first checkout after the kill may still hand out the dead
	// connection (the best-effort guardrails cannot detect a dead
	// socket before the first use); the caller re-acquires, exactly
	// like the apply path does on a dead writer connection
	var c2 *sql.Conn
	for i := 0; ; i++ {
		cn, err := w.Conn(ctx)
		if err != nil {
			t.Fatalf("checkout %d: %v", i, err)
		}
		var one int
		err = cn.QueryRowContext(ctx, "SELECT 1").Scan(&one)
		if err == nil {
			c2 = cn
			break
		}
		if !DeadConn(err) || i >= 5 {
			t.Fatalf("checkout %d: not a dead-connection error: %v", i, err)
		}
		cn.Close()
	}
	defer c2.Close()
	if len(srv.conns) != 2 {
		t.Fatalf("the pool must have opened a NEW physical writer connection, got %d conns", len(srv.conns))
	}
	second := srv.conns[1]
	if second.state["innodb_lock_wait_timeout"] != "5" || second.state["max_execution_time"] != "300000" ||
		!strings.Contains(second.state["sql_mode"], "NO_ZERO_DATE") {
		t.Fatalf("the replacement writer connection was handed out WITHOUT the guardrails: state=%v", second.state)
	}
}

// Repeated checkout: even the SAME physical writer connection is
// re-guardrailed on every checkout — an out-of-band session reset
// cannot leave the write pool serving an unguarded session.
func TestWriterReappliesGuardrailsOnSamePhysicalCheckout(t *testing.T) {
	srv := &fakeServer{}
	w := openFakeWriter(t, srv)
	ctx := context.Background()

	c1, err := w.Conn(ctx)
	if err != nil {
		t.Fatalf("first checkout: %v", err)
	}
	c1.Close()
	first := srv.conns[0]
	if first.state["innodb_lock_wait_timeout"] != "5" {
		t.Fatalf("the first checkout must have set the guardrail, state=%v", first.state)
	}

	// an out-of-band reset: the guardrails go back to the server
	// defaults on the same physical session
	first.state["innodb_lock_wait_timeout"] = "50"
	first.state["max_execution_time"] = ""

	c2, err := w.Conn(ctx) // single-connection pool: the same physical connection
	if err != nil {
		t.Fatalf("second checkout: %v", err)
	}
	defer c2.Close()
	if len(srv.conns) != 1 {
		t.Fatalf("the same physical connection must be reused, got %d conns", len(srv.conns))
	}
	if first.state["innodb_lock_wait_timeout"] != "5" {
		t.Fatalf("the second checkout must re-apply the guardrail, state=%v", first.state)
	}
	if first.state["max_execution_time"] != "300000" {
		t.Fatalf("the second checkout must re-apply the statement timeout, state=%v", first.state)
	}
}

// TestWriterConnDeadDuringGuardrailReacquires pins the P1/P2-3 fix:
// the first physical connection dies DURING the guardrail re-apply
// (its first SET reports a dead-connection error, the driver's plain
// ErrInvalidConn on a KILLed idle socket). Writer.Conn must NOT hand
// out that dead connection: it closes it, checks out a SECOND physical
// connection, re-applies the guardrails to it, and hands out only the
// second one — before any transaction could start on the dead one.
func TestWriterConnDeadDuringGuardrailReacquires(t *testing.T) {
	srv := &fakeServer{killErr: mysql.ErrInvalidConn}
	w := openFakeWriter(t, srv)
	ctx := context.Background()

	// the pool's first (and only) physical connection is alive at
	// checkout — then dies before the guardrails can land
	first := srv.conns[0]
	first.kill()

	c, err := w.Conn(ctx)
	if err != nil {
		t.Fatalf("Writer.Conn must recover from a dead checkout (replace once before handout), got: %v", err)
	}
	defer c.Close()
	if !first.closed {
		t.Fatal("the dead first connection must be closed — a dead session must never be handed out")
	}
	if len(srv.conns) != 2 {
		t.Fatalf("the recovery must open a SECOND physical connection, got %d conns", len(srv.conns))
	}
	second := srv.conns[1]
	if second.state["innodb_lock_wait_timeout"] != "5" || second.state["max_execution_time"] != "300000" ||
		!strings.Contains(second.state["sql_mode"], "NO_ZERO_DATE") {
		t.Fatalf("the second connection must be handed out WITH the guardrails re-applied: state=%v", second.state)
	}
	// and the handed-out connection actually works, on the second
	// physical session
	var one int
	if err := c.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("SELECT 1 on the handed-out connection: %v", err)
	}
	ran := false
	for _, qq := range second.queries {
		if strings.Contains(qq, "SELECT 1") {
			ran = true
		}
	}
	if !ran {
		t.Fatalf("the query must run on the SECOND connection (the first is dead); its queries=%v", second.queries)
	}
}
