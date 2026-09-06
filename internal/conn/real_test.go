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
	// server default 50)
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

	// the first checkout may surface the dead connection (its policy
	// SETs fail; the caller re-acquires, as the plan's dead-connection
	// path does) or may transparently replace it (the driver's session
	// reset discards the dead idle connection and database/sql opens a
	// fresh one). Either way, the handed-out connection must be a NEW
	// physical session, policy-initialized.
	var c2 *sql.Conn
	for i := 0; ; i++ {
		cn, err := side.AcquireScan(ctx)
		if err == nil {
			c2 = cn
			break
		}
		if !DeadConn(err) || i >= 5 {
			t.Fatalf("checkout after the kill: %v", err)
		}
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
}
