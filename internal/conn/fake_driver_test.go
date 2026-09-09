package conn

// Unit tests for the scan-connection policy (R5-4) against a FAKE
// server: the policy must be re-applied on EVERY checkout, a recycled
// CONNECTION_ID on a NEW physical connection must not inherit the old
// connection's initialization, and a connection the policy cannot be
// applied to must never be handed out.
//
// The fake models the read side of a MySQL server: it assigns every
// physical connection the SAME connection id (the recycled-ID
// scenario a server restart produces) and rejects the session-level
// read_only tier (MySQL proper: GLOBAL-only), so the policy falls to
// the session-transaction tier plus the guardrails.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/go-sql-driver/mysql"

	"mtdiff/internal/config"
)

func init() {
	sql.Register("fake-mtdiff", fakeDriver{})
}

// the fake must satisfy the optional driver interfaces database/sql
// prefers over the Prepare fallback (the names must be *Context):
var (
	_ driver.ExecerContext  = (*fakeConn)(nil)
	_ driver.QueryerContext = (*fakeConn)(nil)
)

// activeFake is the server the registered driver instance serves (the
// tests run sequentially; each test installs its own before use).
var activeFake *fakeServer

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) {
	return activeFake.newConn(), nil
}

// fakeServer is the model: one shared connection id for every physical
// connection, per-connection session state, kill-able connections.
type fakeServer struct {
	mu       sync.Mutex
	next     int // physical connections opened so far
	conns    []*fakeConn
	fixedID  int64 // the id EVERY physical connection reports
	refuseRO bool  // reject BOTH read-only tiers (the session is not read-only)
	// refuseTZ rejects the time-zone pin (the session stays in the
	// server's default zone). Unlike the read-only tiers this has NO
	// --allow-unenforced escape: the pin is a correctness requirement
	refuseTZ bool
	// killErr is what operations on a KILLED connection return
	// (default: driver.ErrBadConn, which database/sql auto-releases;
	// a test may set the driver's plain ErrInvalidConn, which it does
	// not — that is the real driver's report for a KILLed idle
	// socket's read failure, see TestControlActiveInvalidConn
	// DoesNotDeadlock)
	killErr error
}

func (s *fakeServer) newConn() *fakeConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	c := &fakeConn{srv: s, id: s.next, state: map[string]string{}}
	s.conns = append(s.conns, c)
	return c
}

// killError is the configured dead-connection error.
func (s *fakeServer) killError() error {
	if s.killErr != nil {
		return s.killErr
	}
	return driver.ErrBadConn
}

// deadError is what an operation on a KILLed connection returns. The
// FIRST operation after the KILL reports the configured dead-connection
// error (default: driver.ErrBadConn; a test sets the driver's plain
// ErrInvalidConn — the real driver's report for a KILLed IDLE socket:
// the client's write succeeds, the read gets EOF/RST, and the driver
// returns its unclassified sentinel, so database/sql does NOT release
// the pinned connection). The real driver closes the connection after
// such a read failure (readPacket → mc.close), so every LATER operation
// on the same physical connection hits the driver's closed check and
// reports driver.ErrBadConn — which database/sql discards.
func (c *fakeConn) deadError() error {
	if !c.sawDead {
		c.sawDead = true
		return c.srv.killError()
	}
	return driver.ErrBadConn
}

type fakeConn struct {
	srv *fakeServer
	id  int // physical connection number
	// the server assigns the SAME connection id to every connection
	// (fakeServer.fixedID): a recycled id on a fresh physical session
	mu      sync.Mutex
	state   map[string]string
	execs   []string
	queries []string
	dead    bool
	closed  bool
	// sawDead: the KILLed connection has already reported its first
	// dead-connection error (see deadError)
	sawDead bool
}

func (c *fakeConn) connID() int64 { return c.srv.fixedID }

func (c *fakeConn) kill() {
	c.mu.Lock()
	c.dead = true
	c.mu.Unlock()
}

func (c *fakeConn) alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.dead && !c.closed
}

func (c *fakeConn) setLocked(key, val string) {
	c.state[key] = val
	c.execs = append(c.execs, "set "+key)
}

// ExecContext models the server side of the policy's SET statements.
func (c *fakeConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead || c.closed {
		// the real driver reports a killed connection as a bad
		// connection; database/sql discards it from the pool
		return nil, c.deadError()
	}
	u := strings.ToUpper(strings.TrimSpace(query))
	switch {
	case u == "SET SESSION TIME_ZONE = '+00:00'":
		if c.srv.refuseTZ {
			return nil, errors.New("fake: time_zone pin refused (session stays in the server default zone)")
		}
		c.setLocked("time_zone", "+00:00")
	case u == "SET SESSION READ_ONLY = ON":
		// MySQL proper: read_only is GLOBAL-only; the session SET fails
		return nil, errors.New("fake: ER_VARIABLE_IS_READONLY (read_only)")
	case u == "SET SESSION TRANSACTION READ ONLY":
		if c.srv.refuseRO {
			return nil, errors.New("fake: disabled no-op (read-only cannot be enforced)")
		}
		c.setLocked("txn_read_only", "1")
	case u == "SET SESSION INNODB_LOCK_WAIT_TIMEOUT = 5":
		c.setLocked("innodb_lock_wait_timeout", "5")
	case u == "SET SESSION MAX_EXECUTION_TIME = 300000":
		c.setLocked("max_execution_time", "300000")
	case strings.HasPrefix(u, "SET SESSION SQL_MODE = '"):
		// the guardrail's idempotent sql_mode append
		v := strings.TrimSuffix(u[len("SET SESSION SQL_MODE = '"):], "'")
		c.setLocked("sql_mode", v)
	default:
		return nil, nil // unknown best-effort statement: accepted
	}
	return driver.RowsAffected(0), nil
}

// QueryContext serves the small SELECTs the open/checkout paths run.
func (c *fakeConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead || c.closed {
		return nil, c.deadError()
	}
	c.queries = append(c.queries, query)
	switch strings.ToUpper(strings.TrimSpace(query)) {
	case "SELECT 1":
		return &fakeRows{vals: []driver.Value{int64(1)}}, nil
	case "SELECT VERSION()":
		return &fakeRows{vals: []driver.Value{"8.0.99-fake"}}, nil
	case "SELECT @@SESSION.SQL_MODE":
		return &fakeRows{vals: []driver.Value{c.state["sql_mode"]}}, nil
	case "SELECT @@SESSION.TIME_ZONE":
		return &fakeRows{vals: []driver.Value{c.state["time_zone"]}}, nil
	}
	return nil, errors.New("fake: unexpected query " + query)
}

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fake: Prepare unused (QueryerContext/ExecerContext implemented)")
}

func (c *fakeConn) Begin() (driver.Tx, error) { return fakeTx{}, nil }

func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

type fakeRows struct {
	vals []driver.Value
	done bool
}

func (r *fakeRows) Columns() []string { return []string{"v"} }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.vals[0]
	return nil
}

// useFakeDB routes the pool constructors through the fake server.
func useFakeDB(t *testing.T, srv *fakeServer) {
	t.Helper()
	old := openPool
	openPool = func(cfg *mysql.Config) (*sql.DB, error) {
		// the fake driver ignores its DSN (activeFake selects the server)
		return sql.Open("fake-mtdiff", "")
	}
	activeFake = srv
	t.Cleanup(func() { openPool = old; activeFake = nil })
}

func fakeEndpoint() config.Endpoint {
	return config.Endpoint{Host: "fake", Port: 3306, User: "u", Database: "d"}
}

// openFakeSide opens a side against the fake with parallel=1: the fake
// server then holds exactly two physical connections, in order — the
// control connection (it ran SELECT VERSION()) and the pre-warmed scan
// connection.
func openFakeSide(t *testing.T, srv *fakeServer) *Side {
	t.Helper()
	useFakeDB(t, srv)
	side, err := OpenSide(context.Background(), "src", fakeEndpoint(), 0, 1, false)
	if err != nil {
		t.Fatalf("OpenSide against the fake: %v", err)
	}
	t.Cleanup(func() { side.Close() })
	if len(srv.conns) != 2 {
		t.Fatalf("the fake must hold exactly 2 physical connections (1 ctl + 1 scan), got %d", len(srv.conns))
	}
	scan := srv.conns[1]
	for _, q := range scan.queries {
		if strings.Contains(q, "SELECT VERSION()") {
			t.Fatal("connection order changed: the scan connection ran the ctl probe")
		}
	}
	return side
}

// The recycled-ID scenario: the server restarts; the old physical
// connection is gone, and the counter hands the SAME CONNECTION_ID to
// a FRESH session. The fresh session (empty state) must still receive
// the full policy on its first checkout — a memo keyed by the old
// connection's id would have skipped it.
func TestAcquireScanReappliesPolicyOnRecycledID(t *testing.T) {
	srv := &fakeServer{fixedID: 7} // every physical connection reports id 7
	side := openFakeSide(t, srv)
	ctx := context.Background()

	first := srv.conns[1]
	c1, err := side.AcquireScan(ctx)
	if err != nil {
		t.Fatalf("first checkout: %v", err)
	}
	c1.Close()
	if len(first.execs) == 0 {
		t.Fatal("the first checkout must have applied the policy")
	}

	// the restart: the physical connection is killed; the pool will
	// open a NEW one, which reports the SAME connection id
	first.kill()

	// P2-8: ONE AcquireScan call is the entire recovery — the bounded
	// checkout replaces the dead session internally (a dead socket
	// fails the policy's first SET) and hands out the fresh physical
	// connection with the full policy; no caller-side retry loop
	c2, err := side.AcquireScan(ctx)
	if err != nil {
		t.Fatalf("single AcquireScan call after the KILL must recover on a bounded internal replacement: %v", err)
	}
	defer c2.Close()
	if len(srv.conns) != 3 {
		t.Fatalf("the pool must have opened a NEW physical connection, got %d conns", len(srv.conns))
	}
	second := srv.conns[2]
	// the fresh session started with NO policy in it
	if second.state["time_zone"] != "+00:00" || second.state["innodb_lock_wait_timeout"] != "5" ||
		second.state["txn_read_only"] != "1" || !strings.Contains(second.state["sql_mode"], "NO_ZERO_DATE") {
		t.Fatalf("the recycled-ID connection was handed out WITHOUT the policy: state=%v", second.state)
	}
	if second.connID() != first.connID() {
		t.Fatalf("the scenario requires a RECYCLED id (both %d), got %d vs %d", first.connID(), second.connID(), first.connID())
	}
}

// Repeated checkout: even the SAME physical connection is re-checked on
// every checkout — an out-of-band session reset (a proxy in the middle,
// a session taken over) cannot leave the pool serving an unguarded
// session.
func TestAcquireScanReappliesPolicyOnEveryCheckout(t *testing.T) {
	srv := &fakeServer{fixedID: 7}
	side := openFakeSide(t, srv)
	ctx := context.Background()

	c1, err := side.AcquireScan(ctx)
	if err != nil {
		t.Fatalf("first checkout: %v", err)
	}
	c1.Close()
	first := srv.conns[1]
	if first.state["innodb_lock_wait_timeout"] != "5" {
		t.Fatalf("the first checkout must have set the guardrail, state=%v", first.state)
	}
	if first.state["time_zone"] != "+00:00" {
		t.Fatalf("the first checkout must have pinned the time zone, state=%v", first.state)
	}

	// an out-of-band reset: the guardrail, the read-only tier and the
	// time zone go back to the server defaults on the same physical
	// session
	first.state["innodb_lock_wait_timeout"] = "50"
	first.state["txn_read_only"] = ""
	first.state["time_zone"] = "+08:00"

	c2, err := side.AcquireScan(ctx) // pool of one: the same physical connection
	if err != nil {
		t.Fatalf("second checkout: %v", err)
	}
	defer c2.Close()
	if len(srv.conns) != 2 {
		t.Fatalf("the same physical connection must be reused, got %d conns", len(srv.conns))
	}
	if first.state["innodb_lock_wait_timeout"] != "5" {
		t.Fatalf("the second checkout must re-apply the guardrail, state=%v", first.state)
	}
	if first.state["txn_read_only"] != "1" {
		t.Fatalf("the second checkout must re-apply the read-only tier, state=%v", first.state)
	}
	if first.state["time_zone"] != "+00:00" {
		t.Fatalf("the second checkout must RE-PIN the time zone (the session-reset defense), state=%v", first.state)
	}
}

// A connection the policy can no longer be applied to is closed and NOT
// handed out — even one that was initialized before the backend started
// refusing the read-only tiers.
func TestAcquireScanRefusesWhenPolicyCannotBeApplied(t *testing.T) {
	srv := &fakeServer{fixedID: 7}
	side := openFakeSide(t, srv)
	ctx := context.Background()

	c1, err := side.AcquireScan(ctx)
	if err != nil {
		t.Fatalf("initial checkout: %v", err)
	}
	c1.Close()

	// from here on the backend rejects BOTH read-only tiers
	srv.refuseRO = true

	if _, err := side.AcquireScan(ctx); err == nil {
		t.Fatal("a connection the policy cannot be applied to must NOT be handed out")
	}
	// the poisoned connection must not sit in the pool: the next
	// checkout opens a fresh one, which also fails the policy
	if _, err := side.AcquireScan(ctx); err == nil {
		t.Fatal("the replacement connection must fail the policy too (refuseRO is still on)")
	}
}

// P0-2: the time-zone pin is a CORRECTNESS requirement and has no
// --allow-unenforced escape. Even a side opened with
// allowUnenforcedReadOnly=true (the read-only tiers are only warnings
// there) must NOT hand out a session whose time_zone the backend
// refuses to pin: a TIMESTAMP value would then be server-local text and
// a cross-timezone endpoint pair would compare instants wrong.
func TestAcquireScanRefusesTimezonePin(t *testing.T) {
	srv := &fakeServer{fixedID: 7}
	useFakeDB(t, srv)
	side, err := OpenSide(context.Background(), "src", fakeEndpoint(), 0, 1, true)
	if err != nil {
		t.Fatalf("open side (allowUnenforced): %v", err)
	}
	t.Cleanup(func() { side.Close() })
	ctx := context.Background()

	// a normal checkout pins the zone (and, with allowUnenforced on,
	// the read-only tiers are only warnings, not refusals)
	c1, err := side.AcquireScan(ctx)
	if err != nil {
		t.Fatalf("checkout with the pin working: %v", err)
	}
	c1.Close()
	if srv.conns[1].state["time_zone"] != "+00:00" {
		t.Fatalf("the checkout must pin the time zone, state=%v", srv.conns[1].state)
	}

	// from here on the backend refuses the time-zone pin
	srv.refuseTZ = true
	if _, err := side.AcquireScan(ctx); err == nil {
		t.Fatal("a session whose time_zone cannot be pinned must NOT be handed out (no --allow-unenforced escape for the pin)")
	}
	if _, err := side.AcquireControl(ctx); err == nil {
		t.Fatal("the control checkout must refuse the unpinnable session too")
	}
	// the refusal is NOT a dead connection: no replacement spin — the
	// same live connection keeps failing the policy on every checkout
	if _, err := side.AcquireScan(ctx); err == nil {
		t.Fatal("every checkout of the unpinnable connection must be refused (refuseTZ is still on)")
	}
}
