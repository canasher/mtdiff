package sync

// Real-MySQL regression for the writer dead-session recovery
// (P1/P2-3), run by e2e/run_e2e.sh against the e2e's srcdb2/dstdb2
// pair (the plain `go test ./...` skips it — see skipNoE2EDSNs).
//
// The test goes through the PRODUCTION apply path (Applier.ApplyFull:
// the TRUNCATE through execDirect, the chunk resyncs through applyTx)
// instead of a hand-rolled Conn/retry loop: the writer's idle physical
// session is KILLed out from under the pool, and a single apply call
// must recover transparently — the replacement happens in Writer.Conn
// BEFORE any transaction starts, and nothing after a BeginTx is ever
// replayed (an unknown commit outcome must not be auto-retried).

import (
	"context"
	"strconv"
	"testing"
	"time"

	"mtdiff/internal/compare"
	"mtdiff/internal/conn"
)

// TestRealWriterKillReconnectApplyPath: KILL the writer's idle physical
// session, then run ONE production ApplyFull. The caller never
// re-acquires a write connection by hand; the transaction starts and
// commits on the replacement session, which must be a NEW physical
// session with the guardrails re-applied.
func TestRealWriterKillReconnectApplyPath(t *testing.T) {
	srcDSN, dstDSN := skipNoE2EDSNs(t)
	ctx := context.Background()

	srcDB := openRaw(t, srcDSN)
	dstDB := openRaw(t, dstDSN)
	execRaw(t, srcDB, "DROP TABLE IF EXISTS t_wapply")
	execRaw(t, dstDB, "DROP TABLE IF EXISTS t_wapply")
	execRaw(t, srcDB, "CREATE TABLE t_wapply (id INT PRIMARY KEY, v VARCHAR(8) NOT NULL)")
	execRaw(t, srcDB, "INSERT INTO t_wapply VALUES (1, 'a'), (2, 'b'), (3, 'c')")
	execRaw(t, dstDB, "CREATE TABLE t_wapply (id INT PRIMARY KEY, v VARCHAR(8) NOT NULL)")
	t.Cleanup(func() {
		srcDB.Exec("DROP TABLE IF EXISTS t_wapply")
		dstDB.Exec("DROP TABLE IF EXISTS t_wapply")
	})

	_, ap := newE2ERunner(t, srcDSN, dstDSN)

	// the writer's idle physical session: guardrailed, then left
	// sleeping in its pool
	c, err := ap.W.Conn(ctx)
	if err != nil {
		t.Fatalf("writer checkout: %v", err)
	}
	var writerID int64
	if err := c.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&writerID); err != nil {
		t.Fatalf("writer id: %v", err)
	}
	var wait int
	if err := c.QueryRowContext(ctx, "SELECT @@SESSION.innodb_lock_wait_timeout").Scan(&wait); err != nil {
		t.Fatalf("writer lock wait: %v", err)
	}
	if wait != 5 {
		t.Fatalf("the idle writer session must be guardrailed, innodb_lock_wait_timeout=%d (server default is 50)", wait)
	}
	c.Close() // idle in the writer pool

	// KILL the idle writer session from under the pool
	if _, err := dstDB.Exec("KILL " + strconv.FormatInt(writerID, 10)); err != nil {
		t.Fatalf("kill %d: %v", writerID, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var one int
		err := dstDB.QueryRow("SELECT 1 FROM information_schema.PROCESSLIST WHERE ID = ?", writerID).Scan(&one)
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("killed writer still visible in PROCESSLIST after 10s")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// ONE production apply call (execDirect TRUNCATE + applyTx chunk
	// resyncs): no manual re-Conn anywhere on this path
	var srcTotal int64
	if err := srcDB.QueryRow("SELECT COUNT(*) FROM t_wapply").Scan(&srcTotal); err != nil {
		t.Fatalf("source count: %v", err)
	}
	schema, err := conn.IntrospectTable(ctx, srcDB, "t_wapply")
	if err != nil {
		t.Fatalf("introspect t_wapply: %v", err)
	}
	b := NewBuilder("t_wapply", schema)
	st := &Stats{Table: "t_wapply", Mode: "FULL"}
	ap.ApplyFull(ctx, st, b, schema, compare.KeyFamilies(schema), srcTotal)
	if st.Error != "" {
		t.Fatalf("ApplyFull after the KILL must succeed without a manual re-Conn, got: %v", st.Error)
	}
	if !st.Truncated || st.Inserts != int(srcTotal) {
		t.Fatalf("ApplyFull: truncated=%v inserts=%d, want truncated=true inserts=%d", st.Truncated, st.Inserts, srcTotal)
	}
	got := rawRows(t, dstDB, "SELECT id, v FROM t_wapply ORDER BY id")
	if len(got) != int(srcTotal) || got[1] != "a" || got[2] != "b" || got[3] != "c" {
		t.Fatalf("destination rows after the apply: %v", got)
	}

	// the writer pool now holds a NEW physical session, guardrailed
	c2, err := ap.W.Conn(ctx)
	if err != nil {
		t.Fatalf("writer checkout after the apply: %v", err)
	}
	defer c2.Close()
	var newID int64
	if err := c2.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&newID); err != nil {
		t.Fatalf("new writer id: %v", err)
	}
	if newID == writerID {
		t.Fatalf("the apply recovered on the KILLED writer connection (id %d)", writerID)
	}
	if err := c2.QueryRowContext(ctx, "SELECT @@SESSION.innodb_lock_wait_timeout").Scan(&wait); err != nil {
		t.Fatalf("new writer lock wait: %v", err)
	}
	if wait != 5 {
		t.Fatalf("the replacement writer session is not guardrailed, innodb_lock_wait_timeout=%d (server default is 50)", wait)
	}
}
