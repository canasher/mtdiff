package conn

// Real-server regression for the per-checkout policy re-apply (R5-4),
// run by the e2e harness (e2e/run_e2e.sh) via MTDIFF_E2E_DSN_SRC and
// skipped by the plain `go test ./...` (no server available).
//
// A KILLED scan connection must be replaced, and the replacement — a
// fresh physical session that starts at the SERVER DEFAULTS (lock wait
// 50, no zero-date sql_mode flags) — must come back policy-initialized:
// only AcquireScan's per-checkout re-apply turns it into the read-only,
// guardrailed session the tool is allowed to hand out.

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"mtdiff/internal/config"
)

// e2eEndpoint parses the harness DSN shorthand "user:pass@host:port/db"
// (a password-less "user@host:port/db" is accepted too).
func e2eEndpoint(t *testing.T, dsn string) config.Endpoint {
	t.Helper()
	cred, db, ok := strings.Cut(dsn, "/")
	if !ok || db == "" {
		t.Fatalf("DSN %q: missing /database", dsn)
	}
	userpass, hostport, _ := strings.Cut(cred, "@")
	if hostport == "" {
		t.Fatalf("DSN %q: missing @host:port", dsn)
	}
	user, pass, _ := strings.Cut(userpass, ":")
	host, port, _ := strings.Cut(hostport, ":")
	if user == "" || host == "" {
		t.Fatalf("DSN %q: unparseable", dsn)
	}
	portN, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("DSN %q: bad port %q", dsn, port)
	}
	return config.Endpoint{Host: host, Port: portN, User: user, Password: pass, Database: db}
}

func TestRealScanReplacementReinitialized(t *testing.T) {
	dsn := os.Getenv("MTDIFF_E2E_DSN_SRC")
	if dsn == "" {
		t.Skip("MTDIFF_E2E_DSN_SRC not set (run via e2e/run_e2e.sh)")
	}
	ctx := context.Background()
	ep := e2eEndpoint(t, dsn)

	// an UNGUARDED connection for the kill itself (the guarded side
	// would refuse the DDL-ish KILL... KILL is not DDL, but the read
	// session cannot be trusted to do test housekeeping)
	raw, err := sql.Open("mysql", BuildDSN(ep, 0))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if err := raw.Ping(); err != nil {
		raw.Close()
		t.Fatalf("ping raw: %v", err)
	}
	defer raw.Close()
	var selfID int64
	if err := raw.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&selfID); err != nil {
		t.Fatalf("raw connection id: %v", err)
	}

	side, err := OpenSide(ctx, "src", ep, 0, 1, false)
	if err != nil {
		t.Fatalf("open side: %v", err)
	}
	defer side.Close()

	// first checkout: the policy is in effect (guardrail 5, not the
	// server default 50, and the time zone pinned to +00:00)
	c1, err := side.AcquireScan(ctx)
	if err != nil {
		t.Fatalf("first checkout: %v", err)
	}
	var wait int
	if err := c1.QueryRowContext(ctx, "SELECT @@SESSION.innodb_lock_wait_timeout").Scan(&wait); err != nil {
		t.Fatalf("lock wait: %v", err)
	}
	if wait != 5 {
		t.Fatalf("the first checkout must be policy-initialized, innodb_lock_wait_timeout=%d", wait)
	}
	var tz string
	if err := c1.QueryRowContext(ctx, "SELECT @@SESSION.time_zone").Scan(&tz); err != nil {
		t.Fatalf("time zone: %v", err)
	}
	if tz != "+00:00" {
		t.Fatalf("the first checkout must pin the session time zone, time_zone=%q", tz)
	}
	c1.Close()

	// KILL every physical connection of the side's database (the
	// pre-warmed scan connection and the control connection, both
	// sleeping in their pools). The side's pools recreate what they
	// need; the test only checks out scan connections.
	var ids []int64
	rows, err := raw.QueryContext(ctx, "SELECT ID FROM information_schema.PROCESSLIST WHERE DB = ? AND COMMAND = 'Sleep'", ep.Database)
	if err != nil {
		t.Fatalf("processlist: %v", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("processlist scan: %v", err)
		}
		if id == selfID {
			continue
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) < 1 {
		t.Fatalf("the side's physical connections are not visible in PROCESSLIST (db=%s)", ep.Database)
	}
	for _, id := range ids {
		if _, err := raw.ExecContext(ctx, "KILL "+strconv.FormatInt(id, 10)); err != nil {
			t.Fatalf("kill %d: %v", id, err)
		}
	}
	// KILL is acknowledged before the connection actually dies: wait
	// until the killed connections are gone from PROCESSLIST, so the
	// next checkout is guaranteed to see a dead socket
	deadline := time.Now().Add(10 * time.Second)
	for {
		n := 0
		for _, id := range ids {
			var one int
			err := raw.QueryRowContext(ctx, "SELECT 1 FROM information_schema.PROCESSLIST WHERE ID = ?", id).Scan(&one)
			if err == nil {
				n++
			}
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("killed connections still visible in PROCESSLIST after 10s")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// P2-8: ONE AcquireScan call is the entire recovery. The checkout
	// may surface the dead connection (its policy's first SET fails on
	// it — the driver's two-phase dead report takes up to two
	// operations on the KILLed socket) and replaces it internally,
	// bounded to three attempts; no caller-side retry loop. The
	// handed-out connection must be a NEW physical session,
	// policy-initialized.
	c2, err := side.AcquireScan(ctx)
	if err != nil {
		t.Fatalf("single AcquireScan call after the KILL must recover (bounded internal replacement): %v", err)
	}
	defer c2.Close()
	// the handed-out connection must NOT be one of the killed ones
	// (its id is a fresh server counter value): a killed socket cannot
	// run the policy's SETs, so a policy-initialized checkout that is
	// also killed is impossible
	var newID int64
	if err := c2.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&newID); err != nil {
		t.Fatalf("replacement connection id: %v", err)
	}
	for _, id := range ids {
		if id == newID {
			t.Fatalf("the checkout returned a KILLED connection (id %d) as if it were a fresh, policy-initialized session", id)
		}
	}
	// and the fresh session — which starts at the SERVER DEFAULTS —
	// must carry the policy: only the per-checkout re-apply produces
	// these values
	if err := c2.QueryRowContext(ctx, "SELECT @@SESSION.innodb_lock_wait_timeout").Scan(&wait); err != nil {
		t.Fatalf("replacement lock wait: %v", err)
	}
	if wait != 5 {
		t.Fatalf("the REPLACEMENT connection was not policy-initialized: innodb_lock_wait_timeout=%d (server default is 50)", wait)
	}
	var mode string
	if err := c2.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode").Scan(&mode); err != nil {
		t.Fatalf("replacement sql_mode: %v", err)
	}
	if !strings.Contains(mode, "NO_ZERO_DATE") {
		t.Fatalf("the replacement connection lacks the sql_mode guardrail: %q", mode)
	}
	// the replacement's session time zone must be pinned too (P0-2):
	// a TIMESTAMP value scanned through a session in the server's
	// default zone would be server-local text
	if err := c2.QueryRowContext(ctx, "SELECT @@SESSION.time_zone").Scan(&tz); err != nil {
		t.Fatalf("replacement time zone: %v", err)
	}
	if tz != "+00:00" {
		t.Fatalf("the REPLACEMENT connection must pin the session time zone, time_zone=%q (server default may differ)", tz)
	}
}

// TestRealControlReplacementReinitialized is the control-side counterpart
// of TestRealScanReplacementReinitialized (R6-1): the control pool's
// physical connection is KILLed and the replacement — a fresh physical
// session at the SERVER DEFAULTS — must come back policy-initialized
// (read-only tier plus guardrails) before it may serve a metadata
// query. The pre-R6-1 code applied the policy ONCE at OpenSide time;
// a replaced control connection served metadata queries UNGUARDED.
func TestRealControlReplacementReinitialized(t *testing.T) {
	dsn := os.Getenv("MTDIFF_E2E_DSN_SRC")
	if dsn == "" {
		t.Skip("MTDIFF_E2E_DSN_SRC not set (run via e2e/run_e2e.sh)")
	}
	ctx := context.Background()
	ep := e2eEndpoint(t, dsn)

	raw, err := sql.Open("mysql", BuildDSN(ep, 0))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if err := raw.Ping(); err != nil {
		raw.Close()
		t.Fatalf("ping raw: %v", err)
	}
	defer raw.Close()

	side, err := OpenSide(ctx, "src", ep, 0, 1, false)
	if err != nil {
		t.Fatalf("open side: %v", err)
	}
	defer side.Close()

	// first control checkout: the policy is in effect, and we learn the
	// control connection's physical id
	c1, err := side.AcquireControl(ctx)
	if err != nil {
		t.Fatalf("first control checkout: %v", err)
	}
	var ctlID int64
	if err := c1.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&ctlID); err != nil {
		t.Fatalf("control connection id: %v", err)
	}
	var wait int
	if err := c1.QueryRowContext(ctx, "SELECT @@SESSION.innodb_lock_wait_timeout").Scan(&wait); err != nil {
		t.Fatalf("lock wait: %v", err)
	}
	if wait != 5 {
		t.Fatalf("the first checkout must be policy-initialized, innodb_lock_wait_timeout=%d", wait)
	}
	c1.Close() // idle in the control pool

	// KILL the control connection (from the unguarded raw connection)
	if _, err := raw.ExecContext(ctx, "KILL "+strconv.FormatInt(ctlID, 10)); err != nil {
		t.Fatalf("kill %d: %v", ctlID, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var one int
		err := raw.QueryRowContext(ctx, "SELECT 1 FROM information_schema.PROCESSLIST WHERE ID = ?", ctlID).Scan(&one)
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("killed control connection still visible in PROCESSLIST after 10s")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// P2-8: ONE AcquireControl call is the entire recovery — the
	// bounded checkout replaces the dead session internally (the
	// driver's two-phase dead report may consume up to two attempts on
	// the KILLed socket) and hands out a NEW physical session,
	// policy-initialized. No caller-side retry loop.
	c2, err := side.AcquireControl(ctx)
	if err != nil {
		t.Fatalf("single AcquireControl call after the KILL must recover (bounded internal replacement): %v", err)
	}
	defer c2.Close()
	var newID int64
	if err := c2.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&newID); err != nil {
		t.Fatalf("replacement connection id: %v", err)
	}
	if newID == ctlID {
		t.Fatalf("the checkout returned the KILLED connection (id %d) as if it were fresh", newID)
	}
	// the fresh session starts at the SERVER DEFAULTS; only the
	// per-checkout re-apply produces these values
	if err := c2.QueryRowContext(ctx, "SELECT @@SESSION.innodb_lock_wait_timeout").Scan(&wait); err != nil {
		t.Fatalf("replacement lock wait: %v", err)
	}
	if wait != 5 {
		t.Fatalf("the REPLACEMENT control connection was not policy-initialized: innodb_lock_wait_timeout=%d (server default is 50)", wait)
	}
	var mode string
	if err := c2.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode").Scan(&mode); err != nil {
		t.Fatalf("replacement sql_mode: %v", err)
	}
	if !strings.Contains(mode, "NO_ZERO_DATE") {
		t.Fatalf("the replacement control connection lacks the sql_mode guardrail: %q", mode)
	}
	// the replacement's session time zone must be pinned too (P0-2)
	var tz string
	if err := c2.QueryRowContext(ctx, "SELECT @@SESSION.time_zone").Scan(&tz); err != nil {
		t.Fatalf("replacement time zone: %v", err)
	}
	if tz != "+00:00" {
		t.Fatalf("the REPLACEMENT control connection must pin the session time zone, time_zone=%q (server default may differ)", tz)
	}
	// read-only state, when the backend exposes it as a session
	// variable (MySQL proper has no such variable; MariaDB does)
	var ro int
	if err := c2.QueryRowContext(ctx, "SELECT @@SESSION.transaction_read_only").Scan(&ro); err == nil {
		if ro != 1 {
			t.Fatalf("the replacement control connection is not read-only: transaction_read_only=%d", ro)
		}
	}
	// and it actually works: a metadata query runs on the replacement
	if _, err := TableExists(ctx, c2, "information_schema"); err != nil {
		t.Fatalf("metadata query on the replacement control connection: %v", err)
	}
}

// TestRealActiveControlKillRecovers is the real-server counterpart of
// TestControlActiveInvalidConnDoesNotDeadlock (P1-2): the control
// session is held ACTIVE (checked out, never closed) while its physical
// connection is KILLed, and the next query through that SAME session
// must transparently recover — no Close, no re-Control, no caller-side
// retry loop — on a NEW physical session with the full policy
// re-applied. This is deliberately different from
// TestRealControlReplacementReinitialized, which closes the session
// first and re-acquires: the pre-fix swap deadlocked only when the
// dead session was still pinning the pool's single slot.
func TestRealActiveControlKillRecovers(t *testing.T) {
	dsn := os.Getenv("MTDIFF_E2E_DSN_SRC")
	if dsn == "" {
		t.Skip("MTDIFF_E2E_DSN_SRC not set (run via e2e/run_e2e.sh)")
	}
	ctx := context.Background()
	ep := e2eEndpoint(t, dsn)

	raw, err := sql.Open("mysql", BuildDSN(ep, 0))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if err := raw.Ping(); err != nil {
		raw.Close()
		t.Fatalf("ping raw: %v", err)
	}
	defer raw.Close()

	side, err := OpenSide(ctx, "src", ep, 0, 1, false)
	if err != nil {
		t.Fatalf("open side: %v", err)
	}
	defer side.Close()

	// an ACTIVE control session: checked out and held, never closed
	q, err := side.Control(ctx)
	if err != nil {
		t.Fatalf("control checkout: %v", err)
	}
	defer q.Close()

	// the physical id, learned through the session itself
	var oldID int64
	if err := OneRow(ctx, q, "SELECT CONNECTION_ID()", []any{&oldID}); err != nil {
		t.Fatalf("CONNECTION_ID through the active session: %v", err)
	}

	// KILL the session's physical connection while q still holds it
	if _, err := raw.ExecContext(ctx, "KILL "+strconv.FormatInt(oldID, 10)); err != nil {
		t.Fatalf("kill %d: %v", oldID, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var one int
		err := raw.QueryRowContext(ctx, "SELECT 1 FROM information_schema.PROCESSLIST WHERE ID = ?", oldID).Scan(&one)
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("killed control connection still visible in PROCESSLIST after 10s")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// watchdog for the recovery: the pre-fix swap would have waited on
	// the pool's single slot — pinned by this very session — until the
	// context gave up
	wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// the query goes through the SAME active session: no Close, no
	// re-Control, no caller-side retry loop
	var one int
	if err := OneRow(wctx, q, "SELECT 1", []any{&one}); err != nil {
		t.Fatalf("the active session's query must recover after the KILL: %v", err)
	}
	// the recovery must run on a NEW physical session
	var newID int64
	if err := OneRow(wctx, q, "SELECT CONNECTION_ID()", []any{&newID}); err != nil {
		t.Fatalf("CONNECTION_ID after the KILL: %v", err)
	}
	if newID == oldID {
		t.Fatalf("the session recovered on the KILLED connection (id %d)", oldID)
	}
	// the fresh session must be policy-initialized again
	var wait int
	if err := OneRow(wctx, q, "SELECT @@SESSION.innodb_lock_wait_timeout", []any{&wait}); err != nil {
		t.Fatalf("lock wait after the KILL: %v", err)
	}
	if wait != 5 {
		t.Fatalf("the replacement control session was not policy-initialized: innodb_lock_wait_timeout=%d (server default is 50)", wait)
	}
	var mode string
	if err := OneRow(wctx, q, "SELECT @@SESSION.sql_mode", []any{&mode}); err != nil {
		t.Fatalf("sql_mode after the KILL: %v", err)
	}
	if !strings.Contains(mode, "NO_ZERO_DATE") {
		t.Fatalf("the replacement control session lacks the sql_mode guardrail: %q", mode)
	}
	// the recovered session's time zone must be pinned (P0-2)
	var tz string
	if err := OneRow(wctx, q, "SELECT @@SESSION.time_zone", []any{&tz}); err != nil {
		t.Fatalf("time zone after the KILL: %v", err)
	}
	if tz != "+00:00" {
		t.Fatalf("the recovered control session must pin the time zone, time_zone=%q (server default may differ)", tz)
	}
	// and a real metadata query runs on the recovered active session
	if _, err := TableExists(wctx, q, "information_schema"); err != nil {
		t.Fatalf("metadata query on the recovered active session: %v", err)
	}
}

// TestRealWriterReplacementReinitialized is the writer-side counterpart
// (R6-3): the single write connection is KILLed and the replacement —
// a fresh physical session at the SERVER DEFAULTS — must come back
// guardrailed (Conn() re-applies the best-effort guardrails on every
// checkout), and the re-connected writer must still be able to run a
// normal destination transaction.
func TestRealWriterReplacementReinitialized(t *testing.T) {
	dsn := os.Getenv("MTDIFF_E2E_DSN_DST")
	if dsn == "" {
		t.Skip("MTDIFF_E2E_DSN_DST not set (run via e2e/run_e2e.sh)")
	}
	ctx := context.Background()
	ep := e2eEndpoint(t, dsn)

	raw, err := sql.Open("mysql", BuildWriterDSN(ep, 0))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if err := raw.Ping(); err != nil {
		raw.Close()
		t.Fatalf("ping raw: %v", err)
	}
	defer raw.Close()

	w, err := OpenWriter(ctx, "dst", ep, 0)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer w.Close()

	c1, err := w.Conn(ctx)
	if err != nil {
		t.Fatalf("first checkout: %v", err)
	}
	var writerID int64
	if err := c1.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&writerID); err != nil {
		t.Fatalf("writer connection id: %v", err)
	}
	var wait int
	if err := c1.QueryRowContext(ctx, "SELECT @@SESSION.innodb_lock_wait_timeout").Scan(&wait); err != nil {
		t.Fatalf("lock wait: %v", err)
	}
	if wait != 5 {
		t.Fatalf("the first checkout must be guardrailed, innodb_lock_wait_timeout=%d", wait)
	}
	var tz string
	if err := c1.QueryRowContext(ctx, "SELECT @@SESSION.time_zone").Scan(&tz); err != nil {
		t.Fatalf("time zone: %v", err)
	}
	if tz != "+00:00" {
		t.Fatalf("the first checkout must pin the session time zone, time_zone=%q", tz)
	}
	c1.Close() // idle in the writer pool

	if _, err := raw.ExecContext(ctx, "KILL "+strconv.FormatInt(writerID, 10)); err != nil {
		t.Fatalf("kill %d: %v", writerID, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var one int
		err := raw.QueryRowContext(ctx, "SELECT 1 FROM information_schema.PROCESSLIST WHERE ID = ?", writerID).Scan(&one)
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("killed writer connection still visible in PROCESSLIST after 10s")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// P2-8: ONE Conn() call is the entire recovery — the bounded
	// checkout (the same rule the apply path uses) replaces the dead
	// session internally and hands out a live, policy-initialized one.
	// No caller-side retry loop.
	c2, err := w.Conn(ctx)
	if err != nil {
		t.Fatalf("single Conn() call after the KILL must recover (bounded internal replacement): %v", err)
	}
	defer c2.Close()
	var newID int64
	if err := c2.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&newID); err != nil {
		t.Fatalf("replacement connection id: %v", err)
	}
	if newID == writerID {
		t.Fatalf("the checkout returned the KILLED connection (id %d) as if it were fresh", newID)
	}
	if err := c2.QueryRowContext(ctx, "SELECT @@SESSION.innodb_lock_wait_timeout").Scan(&wait); err != nil {
		t.Fatalf("replacement lock wait: %v", err)
	}
	if wait != 5 {
		t.Fatalf("the REPLACEMENT writer connection was not guardrailed: innodb_lock_wait_timeout=%d (server default is 50)", wait)
	}
	var mode string
	if err := c2.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode").Scan(&mode); err != nil {
		t.Fatalf("replacement sql_mode: %v", err)
	}
	if !strings.Contains(mode, "NO_ZERO_DATE") {
		t.Fatalf("the replacement writer connection lacks the sql_mode guardrail: %q", mode)
	}
	// the replacement's session time zone must be pinned (P0-2): a
	// TIMESTAMP written through a session in the server's default zone
	// would be interpreted in that zone
	if err := c2.QueryRowContext(ctx, "SELECT @@SESSION.time_zone").Scan(&tz); err != nil {
		t.Fatalf("replacement time zone: %v", err)
	}
	if tz != "+00:00" {
		t.Fatalf("the REPLACEMENT writer connection must pin the session time zone, time_zone=%q (server default may differ)", tz)
	}
	// the re-connected writer must still work end to end: a normal
	// destination transaction
	if _, err := c2.ExecContext(ctx, "CREATE TABLE t_wkill (id INT PRIMARY KEY, v VARCHAR(8) NOT NULL)"); err != nil {
		t.Fatalf("writer DDL after the replacement: %v", err)
	}
	defer func() {
		c2.ExecContext(ctx, "DROP TABLE IF EXISTS t_wkill")
	}()
	if _, err := c2.ExecContext(ctx, "INSERT INTO t_wkill VALUES (1, 'x')"); err != nil {
		t.Fatalf("writer INSERT after the replacement: %v", err)
	}
	var n int
	if err := c2.QueryRowContext(ctx, "SELECT COUNT(*) FROM t_wkill").Scan(&n); err != nil {
		t.Fatalf("writer SELECT after the replacement: %v", err)
	}
	if n != 1 {
		t.Fatalf("writer transaction after the replacement saw %d rows, want 1", n)
	}
}

// TestRealTimezonePinnedAllSessions is the P0-2 production-path check:
// every session the tool hands to real work — scan, control and the
// destination writer — must run with @@SESSION.time_zone='+00:00',
// regardless of the server's default zone. parseTime+loc=UTC alone does
// NOT guarantee this (a TIMESTAMP value is text-formatted by the server
// in the session zone before the driver sees it), so the pin is
// asserted on the live sessions, not just in the DSN.
func TestRealTimezonePinnedAllSessions(t *testing.T) {
	srcDSN := os.Getenv("MTDIFF_E2E_DSN_SRC")
	dstDSN := os.Getenv("MTDIFF_E2E_DSN_DST")
	if srcDSN == "" || dstDSN == "" {
		t.Skip("MTDIFF_E2E_DSN_SRC/DST not set (run via e2e/run_e2e.sh)")
	}
	ctx := context.Background()

	check := func(t *testing.T, label string, cn *sql.Conn) {
		t.Helper()
		var tz string
		if err := cn.QueryRowContext(ctx, "SELECT @@SESSION.time_zone").Scan(&tz); err != nil {
			t.Fatalf("%s: SELECT time_zone: %v", label, err)
		}
		if tz != "+00:00" {
			t.Fatalf("%s: the session time zone must be pinned to +00:00 (the server default zone would shift TIMESTAMP values), time_zone=%q", label, tz)
		}
	}

	src, err := OpenSide(ctx, "src", e2eEndpoint(t, srcDSN), 0, 1, false)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	defer src.Close()
	sc, err := src.AcquireScan(ctx)
	if err != nil {
		t.Fatalf("scan checkout: %v", err)
	}
	check(t, "src scan", sc)
	sc.Close()
	ctl, err := src.AcquireControl(ctx)
	if err != nil {
		t.Fatalf("control checkout: %v", err)
	}
	check(t, "src control", ctl)
	ctl.Close()

	w, err := OpenWriter(ctx, "dst", e2eEndpoint(t, dstDSN), 0)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer w.Close()
	wc, err := w.Conn(ctx)
	if err != nil {
		t.Fatalf("writer checkout: %v", err)
	}
	check(t, "dst writer", wc)
	wc.Close()
}
