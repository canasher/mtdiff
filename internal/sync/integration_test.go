package sync

// Real-MySQL regression for the two destructive re-gates (run by
// e2e/run_e2e.sh against the e2e's srcdb2/dstdb2 pair; the plain
// `go test ./...` skips them — see skipNoE2EDSNs).
//
// These tests own two throwaway tables (t_droprace, t_xid) in the e2e
// databases and drop them again on the way out: they must not
// disturb the other e2e scenarios that use the same pair.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"mtdiff/internal/compare"
	"mtdiff/internal/config"
	"mtdiff/internal/conn"
)

// skipNoE2EDSNs skips unless the e2e harness exported the second
// database pair's DSNs ("user:pass@host:port/database").
func skipNoE2EDSNs(t *testing.T) (srcDSN, dstDSN string) {
	t.Helper()
	srcDSN = os.Getenv("MTDIFF_E2E_DSN_SRC")
	dstDSN = os.Getenv("MTDIFF_E2E_DSN_DST")
	if srcDSN == "" || dstDSN == "" {
		t.Skip("MTDIFF_E2E_DSN_SRC / MTDIFF_E2E_DSN_DST not set (run via e2e/run_e2e.sh)")
	}
	return
}

// dsnToEndpoint parses the e2e DSN shorthand "user:pass@host:port/db"
// (a password-less "user@host:port/db" is accepted too).
func dsnToEndpoint(t *testing.T, dsn string) config.Endpoint {
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

// openRaw opens an UNguarded connection (the guarded production sides
// are read-only and would refuse the test's own setup DDL). The test
// owns its tables and cleans them up.
func openRaw(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	ep := dsnToEndpoint(t, dsn)
	// the e2e DSNs carry no characters the driver needs escaped; the
	// driver takes the address as tcp(host:port), not host:port
	db, err := sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s:%d)/%s", ep.User, ep.Password, ep.Host, ep.Port, ep.Database))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		t.Fatalf("ping raw: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func execRaw(t *testing.T, db *sql.DB, stmt string) {
	t.Helper()
	if _, err := db.Exec(stmt); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}

func rawRows(t *testing.T, db *sql.DB, query string) map[int64]string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var v string
		if err := rows.Scan(&id, &v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[id] = v
	}
	return out
}

// newE2ERunner opens the guarded sides, the write connection and a
// runner for the pair. parallel=2: the unique-holder check (holdersOf)
// acquires a SECOND scan connection from a side whose first one the
// table's plan already holds, so a 1-connection pool deadlocks against
// it (a pre-existing liveness edge of --parallel 1, noted in the
// round-4 report).
func newE2ERunner(t *testing.T, srcDSN, dstDSN string) (*Runner, *Applier) {
	t.Helper()
	ctx := context.Background()
	srcSide, err := conn.OpenSide(ctx, "src", dsnToEndpoint(t, srcDSN), 0, 2, false)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	t.Cleanup(func() { srcSide.Close() })
	dstSide, err := conn.OpenSide(ctx, "dst", dsnToEndpoint(t, dstDSN), 0, 2, false)
	if err != nil {
		t.Fatalf("open dst: %v", err)
	}
	t.Cleanup(func() { dstSide.Close() })
	w, err := conn.OpenWriter(ctx, "dst", dsnToEndpoint(t, dstDSN), 0)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	runner := NewRunner(srcSide, dstSide, Options{
		Cmp:             compare.Options{Parallel: 2, ChunkSize: 1},
		Batch:           100,
		SampleLimit:     3,
		SyncSchema:      true,
		AllowRowRewrite: true,
	})
	return runner, &Applier{W: w, Src: srcSide, Batch: 100, MaxBytes: 1 << 20}
}

// TestDropRaceRealMySQL is the DROP TOCTOU re-gate against real MySQL:
// (1) fresh facts agree -> the DROP runs and verifies; (2) the source
// creates the table after the plan -> REFUSED, the destination row
// survives (the real-MySQL equivalent of "DROP executor called 0
// times"); (3) the source's re-check query fails (the side is closed)
// -> fail closed, destination untouched; (4) the destination table
// vanished out-of-band -> converged without executing anything.
func TestDropRaceRealMySQL(t *testing.T) {
	srcDSN, dstDSN := skipNoE2EDSNs(t)
	ctx := context.Background()
	srcRaw := openRaw(t, srcDSN)
	dstRaw := openRaw(t, dstDSN)
	runner, ap := newE2ERunner(t, srcDSN, dstDSN)

	const table = "t_droprace"
	seedDst := func() {
		execRaw(t, dstRaw, "DROP TABLE IF EXISTS "+table)
		execRaw(t, dstRaw, "CREATE TABLE "+table+" (id INT PRIMARY KEY, v VARCHAR(16) NOT NULL)")
		execRaw(t, dstRaw, "INSERT INTO "+table+" VALUES (1, 'keep-me')")
	}
	dstCount := func() int {
		var n int
		err := dstRaw.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n)
		if err != nil {
			if strings.Contains(err.Error(), "doesn't exist") {
				return 0 // the table was dropped: that IS the 0-row state
			}
			t.Fatalf("dst count: %v", err)
		}
		return n
	}
	defer execRaw(t, srcRaw, "DROP TABLE IF EXISTS "+table)
	defer execRaw(t, dstRaw, "DROP TABLE IF EXISTS "+table)

	// (1) normal drop: the source lacks the table, the destination has it
	seedDst()
	ts := runner.ApplyDrop(ctx, table, ap)
	if ts.Status != "APPLIED" || ts.Verified != "OK" {
		t.Fatalf("normal drop: status=%s err=%s", ts.Status, ts.Error)
	}
	if n := dstCount(); n != 0 {
		t.Fatalf("table must be gone after the drop, still has %d rows", n)
	}

	// (2) the source created the table after the plan was confirmed:
	// refuse, and the destination row must survive
	seedDst()
	execRaw(t, srcRaw, "CREATE TABLE "+table+" (id INT PRIMARY KEY, v VARCHAR(16) NOT NULL)")
	execRaw(t, srcRaw, "INSERT INTO "+table+" VALUES (1, 'src-side')")
	ts = runner.ApplyDrop(ctx, table, ap)
	if ts.Status != "FAILED" {
		t.Fatalf("source appeared: status=%s, want FAILED", ts.Status)
	}
	if !strings.Contains(ts.Error, "appeared after the plan was confirmed") || !strings.Contains(ts.Error, "DROP was not executed") {
		t.Errorf("refusal must explain the stale plan: %s", ts.Error)
	}
	if n := dstCount(); n != 1 {
		t.Fatalf("the destination must be untouched after the refusal, rows=%d", n)
	}
	v := ""
	if err := dstRaw.QueryRow("SELECT v FROM " + table + " WHERE id=1").Scan(&v); err != nil || v != "keep-me" {
		t.Fatalf("the surviving row must be intact, got %q (%v)", v, err)
	}
	execRaw(t, srcRaw, "DROP TABLE IF EXISTS "+table)

	// (3) the source's re-check query fails (its side is closed):
	// fail closed, destination untouched
	seedDst()
	runner.Src.Close()
	ts = runner.ApplyDrop(ctx, table, ap)
	if ts.Status != "FAILED" || !strings.Contains(ts.Error, "fail closed") {
		t.Fatalf("metadata failure: status=%s err=%s, want FAILED / fail closed", ts.Status, ts.Error)
	}
	if n := dstCount(); n != 1 {
		t.Fatalf("the destination must be untouched, rows=%d", n)
	}

	// (4) the destination table vanished out-of-band: converged, nothing
	// executed (re-open the source side closed in (3))
	runner2, ap2 := newE2ERunner(t, srcDSN, dstDSN)
	execRaw(t, dstRaw, "DROP TABLE IF EXISTS "+table)
	ts = runner2.ApplyDrop(ctx, table, ap2)
	if ts.Status != "APPLIED" || ts.Verified != "OK" {
		t.Fatalf("dst gone: status=%s err=%s, want APPLIED / OK (converged)", ts.Status, ts.Error)
	}
}

// TestScopeEscalationRealMySQL is the destructive-scope re-gate against
// real MySQL, on the reachable escalation: a confirmed ROWLEVEL plan
// (no TRUNCATE shown). Between the confirmation and the apply, the
// destination is re-mutated into a cross-chunk unique swap — a drift no
// row-level order can apply, which the re-plan escalates to the
// order-independent FULL resync (TRUNCATE + reload). The apply must
// REFUSE the escalation (the confirmed plan showed no TRUNCATE), touch
// nothing, and stop the table; then the destination is restored to its
// pre-confirmation state and the SAME confirmed plan must converge the
// table (a smaller/safer re-plan is always allowed).
//
// (The rewrite-GROUP identity gate of the same scope is covered by unit
// tests: a destructive no-op-holder rewrite group is not constructible
// on a real, consistent, constraint-enforcing source — see the
// round-4 completion report.)
func TestScopeEscalationRealMySQL(t *testing.T) {
	srcDSN, dstDSN := skipNoE2EDSNs(t)
	ctx := context.Background()
	srcRaw := openRaw(t, srcDSN)
	dstRaw := openRaw(t, dstDSN)
	runner, ap := newE2ERunner(t, srcDSN, dstDSN)

	const table = "t_xid"
	execRaw(t, srcRaw, "DROP TABLE IF EXISTS "+table)
	execRaw(t, dstRaw, "DROP TABLE IF EXISTS "+table)
	defer execRaw(t, srcRaw, "DROP TABLE IF EXISTS "+table)
	defer execRaw(t, dstRaw, "DROP TABLE IF EXISTS "+table)
	execRaw(t, srcRaw, "CREATE TABLE "+table+" (id INT PRIMARY KEY, v VARCHAR(8) NOT NULL UNIQUE)")
	execRaw(t, dstRaw, "CREATE TABLE "+table+" (id INT PRIMARY KEY, v VARCHAR(8) NOT NULL UNIQUE)")
	// one row per chunk (chunk size 1, set on the runner), so the 1<->2
	// value swap below crosses chunk commits and no row-level order can
	// apply it. Row 1 and row 4 drift in the pre-pass, so the confirmed
	// plan's re-plan targets cover the chunks the swap later touches.
	execRaw(t, srcRaw, "INSERT INTO "+table+" VALUES (1,'A'),(2,'B'),(3,'C'),(4,'D')")
	execRaw(t, dstRaw, "INSERT INTO "+table+" VALUES (1,'Z'),(2,'B'),(3,'C'),(4,'X')")

	// preflight (read-only): row 4 differs -> a plain ROWLEVEL plan with
	// no destructive scope at all
	results, err := runner.PrePass(ctx, []string{table})
	if err != nil {
		t.Fatalf("pre-pass: %v", err)
	}
	conf := runner.PlanTable(ctx, results[0])
	if conf.Mode != "ROWLEVEL" || conf.Scope.FullResync || conf.Rewrites != 0 {
		t.Fatalf("the scenario must preflight to a plain row-level plan, got mode=%s scope=%+v rewrites=%d (%s)",
			conf.Mode, conf.Scope, conf.Rewrites, conf.Error)
	}

	// re-mutate the destination (the TOCTOU window): rows 1,2 now swap
	// their unique values across chunks (dst 1->B, 2->A). Three-step
	// temporary-value swap: each step frees the value the next step
	// needs, so the UNIQUE column stays legal throughout.
	for _, s := range []string{
		"UPDATE " + table + " SET v='__t1__' WHERE id=1",
		"UPDATE " + table + " SET v='A' WHERE id=2",
		"UPDATE " + table + " SET v='B' WHERE id=1",
	} {
		execRaw(t, dstRaw, s)
	}
	ts := runner.ApplyTable(ctx, results[0], ap, conf)
	if ts.Status != "FAILED" {
		t.Fatalf("escalation: status=%s err=%s, want FAILED (stopped)", ts.Status, ts.Error)
	}
	if !strings.Contains(ts.Error, "full resync") {
		t.Errorf("refusal must name the full resync it refuses: %s", ts.Error)
	}
	if !strings.Contains(ts.Error, "Re-run") {
		t.Errorf("refusal must say to re-run so the new plan is confirmed: %s", ts.Error)
	}
	if got := rawRows(t, dstRaw, "SELECT id, v FROM "+table); got[1] != "B" || got[2] != "A" || got[3] != "C" || got[4] != "X" {
		t.Fatalf("the refused apply must not touch the destination, got %v", got)
	}
	if got := rawRows(t, srcRaw, "SELECT id, v FROM "+table); got[1] != "A" || got[2] != "B" || got[3] != "C" || got[4] != "D" {
		t.Fatalf("the source must be untouched, got %v", got)
	}

	// positive control: restore rows 1,2 (the inverse three-step swap) —
	// the re-plan shrinks to the single row-4 update the confirmed plan
	// already showed, and the same confirmed plan converges the table
	for _, s := range []string{
		"UPDATE " + table + " SET v='__t1__' WHERE id=1",
		"UPDATE " + table + " SET v='B' WHERE id=2",
		"UPDATE " + table + " SET v='A' WHERE id=1",
	} {
		execRaw(t, dstRaw, s)
	}
	results2, err := runner.PrePass(ctx, []string{table})
	if err != nil {
		t.Fatalf("re-pass: %v", err)
	}
	ts = runner.ApplyTable(ctx, results2[0], ap, conf)
	if ts.Status != "APPLIED" {
		t.Fatalf("the shrunken re-plan must converge: status=%s err=%s", ts.Status, ts.Error)
	}
	if got := rawRows(t, dstRaw, "SELECT id, v FROM "+table); got[1] != "A" || got[2] != "B" || got[3] != "C" || got[4] != "D" {
		t.Fatalf("convergence must match the source, dst=%v", got)
	}
}
