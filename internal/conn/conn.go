// Package conn manages MySQL connections, session safety policy and schema
// introspection.
package conn

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"mtdiff/internal/config"
)

// Side is one endpoint (source or destination) under comparison.
//
// It holds two pools: a control pool (MaxOpenConns=1) for introspection
// and planning queries, and a scan pool (MaxOpenConns=parallel) whose
// dedicated connections are pinned to workers so the session safety policy
// stays in effect for the whole scan.
//
// The policy (applySession: the +00:00 time-zone pin, the read-only
// tiers, the guardrails) is re-applied on EVERY scan checkout, not
// remembered per physical connection. A CONNECTION_ID memo cannot be
// the identity of a physical session: the server's counter resets
// across a restart, and a recycled ID on a NEW physical connection
// must not inherit the old one's "initialized" mark (the new session
// has no policy in it). applySession is idempotent, so the re-apply
// costs a handful of cheap SETs per checkout — safety over saved round
// trips. A connection the policy cannot be applied to (or that is
// dead) is closed and never handed out — checkoutChecked bounds the
// replacements so one AcquireScan/AcquireControl call is the whole
// recovery.
type Side struct {
	Name          string
	Version       string
	ep            config.Endpoint
	scan          *sql.DB
	ctl           *sql.DB
	allowUnforced bool
}

// poolConfig builds the driver configuration for one pool. The DSN is
// assembled by the DRIVER ITSELF (mysql.Config + FormatDSN), never by
// hand: the old net/url.UserPassword builder percent-encoded the
// password (a password of "s3:cret" became the literal "s3%3Acret" the
// server then rejected) and the database name was appended raw, so a
// name containing "/" or "?" broke the DSN's path/query boundary. The
// driver's formatter and parser agree on the grammar by construction —
// credentials are written verbatim and ParseDSN recovers them by the
// LAST '/' and LAST '@' (a password containing ':', '@', '/', '?' or
// '#' round-trips byte for byte; see the BuildDSN tests).
//
// parseTime=true&loc=UTC is mandatory: both sides must interpret
// timestamps in UTC or TIMESTAMP columns spanning time zones produce
// false positives.
//
// time_zone is pinned to +00:00 for EVERY physical connection, twice:
// (1) at connect time — the driver's Params map makes it execute
// "SET time_zone = '+00:00'" before the connection is usable (a failure
// there fails the connect); (2) on every checkout, by the session
// policy (applySession / the writer's checkout), which re-verifies and
// re-applies it. parseTime+loc=UTC alone is NOT a session-timezone pin:
// a TIMESTAMP column is TEXT-formatted by the server in the SESSION
// time_zone before the driver ever sees it, so a session left at the
// server's default zone (which may differ between the two endpoints)
// shifts every TIMESTAMP by that zone's offset — two rows displaying
// the same string can be different instants, and equal instants can
// compare different (P0-2). cfg.Loc=UTC and @@session.time_zone=+00:00
// are independent; both are required.
func poolConfig(ep config.Endpoint, maxAllowedPacket, writeTimeoutSec int) *mysql.Config {
	cfg := mysql.NewConfig()
	cfg.User = ep.User
	cfg.Passwd = ep.Password
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port))
	cfg.DBName = ep.Database
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.Timeout = 10 * time.Second
	cfg.ReadTimeout = 10 * time.Minute
	cfg.WriteTimeout = time.Duration(writeTimeoutSec) * time.Second
	// interpolateParams=false (the driver default, stated deliberately):
	// parameters travel to the server and are bound as DATA there, so a
	// value is never rendered into the statement text on the client — the
	// only way string values with backslashes/quotes stay intact under
	// NO_BACKSLASH_ESCAPES (P0-3).
	cfg.InterpolateParams = false
	// Pin the session time zone on every physical connection at connect
	// time (the driver's Params map; see the poolConfig doc for why
	// parseTime+loc=UTC alone is not a pin, and why the checkout policy
	// re-verifies and re-applies it on top).
	cfg.Params = map[string]string{"time_zone": "'+00:00'"}
	if maxAllowedPacket > 0 {
		cfg.MaxAllowedPacket = maxAllowedPacket
	}
	// the driver's formal charset API (the private charsets field has no
	// other public setter): wire-identical to the old charset=utf8mb4
	if err := cfg.Apply(mysql.Charset("utf8mb4", "")); err != nil {
		// unreachable: the Charset applier only assigns fields
		panic(fmt.Sprintf("apply charset: %v", err))
	}
	return cfg
}

// BuildDSN assembles the driver DSN via the driver's own formatter
// (poolConfig; the round-trip guarantee is pinned by TestBuildDSN
// through mysql.ParseDSN).
func BuildDSN(ep config.Endpoint, maxAllowedPacket int) string {
	return poolConfig(ep, maxAllowedPacket, 10).FormatDSN()
}

// BuildWriterDSN is BuildDSN with a longer network write timeout: sending a
// multi-row INSERT batch can take far longer than a plain query send.
func BuildWriterDSN(ep config.Endpoint, maxAllowedPacket int) string {
	return poolConfig(ep, maxAllowedPacket, 600).FormatDSN()
}

// openPool is the pool constructor. A test seam: unit tests swap in a
// fake driver to exercise the checkout/policy logic without a server;
// production always opens the real go-sql-driver/mysql. A fresh
// connector per pool: NewConnector normalizes the config for itself,
// so the two pools never share driver state.
var openPool = func(cfg *mysql.Config) (*sql.DB, error) {
	c, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	return sql.OpenDB(c), nil
}

// OpenSide opens both pools, preconditions the control connection and
// pre-warms the scan pool, then verifies the server answers.
//
// The pre-warm is a latency optimization, NOT the correctness mechanism:
// it opens `parallel` DISTINCT physical connections up front (holding each
// until all are open, so the idle pool cannot hand one connection back for
// several iterations). Correctness comes from AcquireScan, which re-applies
// the idempotent policy on EVERY checkout — a connection opened lazily
// later, or replacing a dead one, gets its policy on first use.
//
// allowUnenforcedReadOnly is config.Options.AllowUnenforcedReadOnly; see
// applySession for what it relaxes.
func OpenSide(ctx context.Context, name string, ep config.Endpoint, maxAllowedPacket, parallel int, allowUnenforcedReadOnly bool) (*Side, error) {
	if parallel < 1 {
		parallel = 1
	}
	scanDB, err := openPool(poolConfig(ep, maxAllowedPacket, 10))
	if err != nil {
		return nil, err
	}
	ctlDB, err := openPool(poolConfig(ep, maxAllowedPacket, 10))
	if err != nil {
		scanDB.Close()
		return nil, err
	}
	scanDB.SetMaxOpenConns(parallel)
	scanDB.SetMaxIdleConns(parallel)
	// Connections are dedicated for the whole run; never recycle mid-scan.
	scanDB.SetConnMaxLifetime(0)
	ctlDB.SetMaxOpenConns(1)
	ctlDB.SetConnMaxLifetime(0)

	c, err := ctlDB.Conn(ctx)
	if err != nil {
		scanDB.Close()
		ctlDB.Close()
		return nil, fmt.Errorf("%s: connect to %s: %w", name, ep.MaskedDSN(), err)
	}
	if err := applySession(ctx, c, allowUnenforcedReadOnly); err != nil {
		c.Close()
		scanDB.Close()
		ctlDB.Close()
		return nil, fmt.Errorf("%s: %s: %w", name, ep.MaskedDSN(), err)
	}
	// Use QueryRowContext, not QueryContext: discarding an unclosed *Rows on
	// a dedicated *Conn keeps its closemu read lock held forever, and a
	// later ErrBadConn close deadlocks on the write lock (hangs the process).
	var one int
	if err := c.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		c.Close()
		scanDB.Close()
		ctlDB.Close()
		return nil, fmt.Errorf("%s: connect to %s: %w", name, ep.MaskedDSN(), err)
	}
	var version string
	if err := c.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		c.Close()
		scanDB.Close()
		ctlDB.Close()
		return nil, fmt.Errorf("%s: SELECT VERSION() (%s): %w", name, ep.MaskedDSN(), err)
	}
	c.Close() // returns the pre-conditioned connection to the pool

	// Hold every acquired connection until all `parallel` are open, so the
	// idle pool cannot recycle one physical connection for several
	// iterations (database/sql's Conn() takes the idle one back the moment
	// it is returned); release them all, already policy-applied, at the end.
	prewarmed := make([]*sql.Conn, 0, parallel)
	for i := 0; i < parallel; i++ {
		sc, err := scanDB.Conn(ctx)
		if err != nil {
			scanDB.Close()
			ctlDB.Close()
			return nil, fmt.Errorf("%s: pre-warm scan pool: %s: %w", name, ep.MaskedDSN(), err)
		}
		if err := applySession(ctx, sc, allowUnenforcedReadOnly); err != nil {
			sc.Close()
			scanDB.Close()
			ctlDB.Close()
			return nil, fmt.Errorf("%s: pre-warm scan pool: %s: %w", name, ep.MaskedDSN(), err)
		}
		prewarmed = append(prewarmed, sc)
	}
	for _, sc := range prewarmed {
		sc.Close() // back to the idle pool (AcquireScan re-applies the policy anyway)
	}
	return &Side{
		Name:          name,
		Version:       version,
		ep:            ep,
		scan:          scanDB,
		ctl:           ctlDB,
		allowUnforced: allowUnenforcedReadOnly,
	}, nil
}

// applySession enforces the session safety policy. It is idempotent:
// every statement it runs may be re-executed on the same session without
// observable effect (the sql_mode flags in particular are appended at
// most once, see addSQLModeFlags).
//
// The policy is three tiers, in order:
//
//  1. the time zone pin (REQUIRED, no fallback): the session time_zone
//     must be +00:00, or TIMESTAMP values are server-local text and a
//     pair of endpoints with different default zones compares instants
//     wrong (P0-2). This is a CORRECTNESS requirement, not a
//     best-effort one: a session that refuses the pin (or is dead) is
//     NOT handed out — not even under allowUnenforcedReadOnly, which
//     relaxes ONLY the read-only tiers below.
//  2. read-only, enforced two-tier: MySQL proper only has a GLOBAL
//     read_only, so a session SET fails with ER_VARIABLE_IS_READONLY
//     (1229); the fallback is a session default transaction character
//     (READ ONLY), which also covers implicit autocommit statements.
//     TiDB inverts the problem: read_only is GLOBAL-only there as well
//     (1229), and SET SESSION TRANSACTION READ ONLY is a disabled no-op
//     (1235, unless tidb_enable_noop_functions is set), so both tiers
//     fail. By default mtdiff then refuses to continue: a read pool the
//     server cannot keep read-only is not acceptable silently. With
//     allowUnenforcedReadOnly (--allow-unenforced-readonly) it proceeds
//     instead, printing a per-connection warning: mtdiff still only
//     issues SELECTs on these connections, the accepted risk is that
//     the server could not stop other statements from a shared account.
//  3. the guardrails (see applyGuardrails). Their failure is NOT
//     ignored (P2-8): applyGuardrails returns an error only when the
//     session is DEAD (an unsupported variable is a warning inside
//     guardrail, not an error), and a dead session handed out would
//     serve its first real query as a bad-connection error. Propagated
//     so the bounded checkout helper can replace it.
func applySession(ctx context.Context, c *sql.Conn, allowUnenforced bool) error {
	if _, err := c.ExecContext(ctx, "SET SESSION time_zone = '+00:00'"); err != nil {
		// a DEAD session fails this first: the error (sentinel or
		// "invalid connection" message) is wrapped with %w so the
		// bounded checkout helper can tell a dead session apart from a
		// genuine refusal and replace it
		return fmt.Errorf("cannot pin session time_zone to +00:00 (TIMESTAMP values would be compared in the server's default zone): %w", err)
	}
	if _, err := c.ExecContext(ctx, "SET SESSION read_only = ON"); err != nil {
		if _, err2 := c.ExecContext(ctx, "SET SESSION TRANSACTION READ ONLY"); err2 != nil {
			if !allowUnenforced {
				// the FIRST tier error is wrapped (%w), not formatted
				// (%v): a dead connection reports itself as a sentinel
				// (driver.ErrBadConn / sql.ErrConnDone), and the
				// caller must be able to errors.Is it apart from a
				// genuine policy refusal
				return fmt.Errorf("refusing to continue: cannot enforce read-only session (read_only: %w; transaction read only: %v)", err, err2)
			}
			fmt.Fprintf(os.Stderr, "warn: cannot enforce a read-only session on this backend (read_only: %v; transaction read only: %v); continuing per --allow-unenforced-readonly, read connections issue SELECTs only\n", err, err2)
		}
	}
	if err := applyGuardrails(ctx, c); err != nil {
		return fmt.Errorf("apply session guardrails: %w", err)
	}
	return nil
}

// applyGuardrails applies the best-effort session guardrails (lock-wait
// timeout, statement timeout, zero-date sql_mode flags) and reports a
// DEAD-CONNECTION failure. A guardrail the backend does not SUPPORT
// (MariaDB has no max_execution_time; a proxy layer may lack either
// variable) is a warning, and the remaining guardrails still run — that
// is the best-effort part. A statement that fails because the SESSION
// is dead (a KILLed socket, a dropped network) is returned so the
// caller can REPLACE the connection instead of handing out a dead one
// (see Writer.Conn / checkoutChecked). It is shared by the read-only
// pools (via applySession, which PROPAGATES the result — a dead session
// must be replaced, not handed out) and the destination write pool (the
// writer's checkout).
func applyGuardrails(ctx context.Context, c *sql.Conn) error {
	if err := guardrail(ctx, c, "SET SESSION innodb_lock_wait_timeout = 5"); err != nil {
		return err
	}
	if err := guardrail(ctx, c, "SET SESSION max_execution_time = 300000"); err != nil {
		return err
	}
	return addSQLModeFlags(ctx, c)
}

// guardrail runs one best-effort guardrail statement and distinguishes
// its two failure modes: UNSUPPORTED (the backend lacks the variable)
// is a warning — the remaining guardrails still run; DEAD is returned
// to the caller, which must replace the connection rather than use it.
func guardrail(ctx context.Context, c *sql.Conn, stmt string) error {
	if _, err := c.ExecContext(ctx, stmt); err != nil {
		if DeadConn(err) {
			return err
		}
		fmt.Fprintf(os.Stderr, "warn: %s failed: %v\n", stmt, err)
	}
	return nil
}

// addSQLModeFlags ensures the zero-date guard flags are in the session
// sql_mode. Unlike a blind CONCAT-append (which grew the mode by 31
// characters per execution until it exceeded sql_max_mode_size, 255 on
// 8.0, and warned on every later statement), it appends each flag at most
// once, so re-applying the policy to a connection is a no-op. A dead
// session (the SELECT probe or the SET failing on a dead socket) is
// returned; a failed/unsupported statement is a warning.
func addSQLModeFlags(ctx context.Context, c *sql.Conn) error {
	var mode string
	if err := c.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode").Scan(&mode); err != nil {
		if DeadConn(err) {
			return err
		}
		fmt.Fprintf(os.Stderr, "warn: SELECT @@SESSION.sql_mode failed: %v\n", err)
		return nil
	}
	var add []string
	for _, flag := range []string{"NO_ZERO_DATE", "NO_ZERO_IN_DATE"} {
		if !strings.Contains(mode, flag) {
			add = append(add, flag)
		}
	}
	if len(add) == 0 {
		return nil
	}
	// Join, don't concatenate literals: an empty current mode must not
	// render as an empty string literal glued onto the new one.
	newMode := strings.Join(append([]string{mode}, add...), ",")
	return guardrail(ctx, c, "SET SESSION sql_mode = '"+strings.ReplaceAll(newMode, "'", "''")+"'")
}

// checkoutChecked is the ONE bounded checkout/replacement rule for a
// dedicated pool, shared by the scan pool (AcquireScan), the control
// pool (AcquireControl) and the destination write pool (Writer's
// checkout) so the three paths cannot drift:
//
//   - the session policy (init) is re-applied on EVERY checkout, not
//     remembered per physical connection: a CONNECTION_ID memo cannot
//     be a session's identity (the server's counter resets across a
//     restart, and a recycled ID on a NEW physical connection must not
//     inherit the old one's "initialized" mark), and the policy is
//     idempotent, so the cost is a handful of cheap SETs per checkout;
//   - a checkout the policy cannot be applied to is closed and NEVER
//     handed out. If the failure says the SESSION IS DEAD (a KILLed
//     idle socket, a dropped network), a FRESH checkout is tried
//     instead, bounded to THREE attempts total (never a loop): the
//     dead connection is closed, the next checkout re-applies the
//     full policy from scratch, and only a live, policy-initialized
//     session is returned. A NON-dead policy failure (a genuine
//     refusal: the backend will not take the read-only tiers, will
//     not pin the time zone) is an immediate error — no replacement,
//     no handout, no warning-and-continue.
//
// ONE call to AcquireScan/AcquireControl/Writer.Conn is therefore the
// ENTIRE dead-connection recovery: callers must not (and cannot usefully)
// wrap them in "for DeadConn(err) { re-acquire }" retry loops — a failed
// call has already exhausted its bounded replacements.
//
// Why three attempts, not two (the "one replacement" minimum): the real
// driver reports a KILLed IDLE socket in two phases. The FIRST
// operation on it (the policy's first SET) returns its plain
// ErrInvalidConn — the client's write went out, the read got EOF/RST —
// and database/sql only releases a pinned connection on
// driver.ErrBadConn, so when the dead connection is closed it goes BACK
// to the single-slot pool, and the next checkout is the SAME physical
// connection. Only the driver's NEXT operation on it hits its closed
// check (driver.ErrBadConn), which is what finally discards it — the
// third checkout is the first one that is a genuinely NEW physical
// session. A single replacement would fail exactly in the case this
// exists for: the pool's only slot was just KILLed.
func checkoutChecked(ctx context.Context, name string, pool *sql.DB, kind string, init func(context.Context, *sql.Conn) error) (*sql.Conn, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		c, err := pool.Conn(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: acquire %s connection: %w", name, kind, err)
		}
		if err := init(ctx, c); err != nil {
			// a non-dead failure (a policy REFUSAL) must never
			// trigger a replacement: close and report
			if !DeadConn(err) {
				c.Close()
				return nil, fmt.Errorf("%s: init %s connection: %w", name, kind, err)
			}
			lastErr = err
			_ = c.Close()
			continue
		}
		return c, nil
	}
	return nil, fmt.Errorf("%s: replace dead %s connection (3 dead sessions in a row): %v", name, kind, lastErr)
}

// AcquireScan returns a dedicated scan connection whose session safety
// policy (time-zone pin, read-only enforcement, guardrails) is in
// effect — see checkoutChecked for the single-call bounded replacement
// contract. Callers must Close it when done.
func (s *Side) AcquireScan(ctx context.Context) (*sql.Conn, error) {
	return checkoutChecked(ctx, s.Name, s.scan, "scan",
		func(ctx context.Context, c *sql.Conn) error { return applySession(ctx, c, s.allowUnforced) })
}

// DeadConn reports a dead-connection error: the driver's bad-connection
// marker, or MySQL's lost-connection family in the message (the driver
// wraps server-side disconnects as "invalid connection: Lost
// connection to MySQL server ..."). A pinned sql.Conn does NOT get
// database/sql's one automatic retry (that only exists on DB-level
// methods), so a caller holding a pinned connection must take a FRESH
// one from the pool — AcquireScan re-initializes it if it is new.
func DeadConn(err error) bool {
	if err == nil {
		return false
	}
	// driver.ErrBadConn: the driver reported a dead connection.
	// sql.ErrConnDone: database/sql itself discarded the connection
	// (a failed operation on a dead socket closes it; later operations
	// on the wrapper report this sentinel).
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "bad connection") ||
		strings.Contains(msg, "invalid connection") ||
		strings.Contains(msg, "Lost connection")
}

// Queryer is the read seam metadata and planning queries run on:
// *sql.DB, *sql.Conn and *sql.Tx all satisfy it. Production code gets a
// Queryer from Side.Control (a policy-applied control session with
// dead-connection recovery) — never from a raw pool handle, whose
// physical connections a server KILL can replace WITHOUT the session
// safety policy being re-applied.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// AcquireControl returns the control connection with the full session
// safety policy in effect — the same single-call bounded replacement
// contract as AcquireScan (see checkoutChecked): a checkout that comes
// back DEAD (killed by the server, network loss, a restart — the
// policy's SETs fail with a bad-connection error) is not a policy
// refusal: the dead connection is closed, a FRESH one is checked out
// and the FULL policy re-applied to it before the checkout may be used
// (fresh connection → policy → query, never the reverse), bounded to
// three attempts; a genuine policy refusal, or three dead sessions in a
// row, is an error: nothing unguarded is ever handed out. Callers must
// Close it when done.
func (s *Side) AcquireControl(ctx context.Context) (*sql.Conn, error) {
	return checkoutChecked(ctx, s.Name, s.ctl, "control",
		func(ctx context.Context, c *sql.Conn) error { return applySession(ctx, c, s.allowUnforced) })
}

// ControlQueryer is a Queryer bound to the control pool: one
// policy-applied checkout, with transparent dead-connection recovery.
// A dead physical connection (server KILL, network partition, a
// restart) surfaces as a bad-connection error on the next query; the
// session then swaps in a FRESH one — the policy applied FIRST, via
// AcquireControl — and the query is retried exactly once on it.
// Query-first / policy-later is impossible: the retry runs only on a
// connection whose policy application already succeeded, and a session
// whose replacement fails is sticky-dead (every later query fails
// loudly with the replacement/policy error, not the original dead one).
//
// The swap closes the dead checkout BEFORE checking out the
// replacement (P1-2): the control pool holds a SINGLE connection
// (MaxOpenConns=1) and this session is pinning it. A dead-connection
// error that is NOT driver.ErrBadConn — the go-sql-driver reports a
// KILLed idle socket's read failure as its plain ErrInvalidConn, and
// database/sql only releases a pinned connection on ErrBadConn — would
// otherwise keep the slot pinned, and a swap that checked out the
// replacement first would wait on its own slot forever (a self-
// deadlock). Closing first frees the slot in every case (a truly
// discarded connection just has nothing left to return).
//
// A Row query on this session does not auto-recover: database/sql
// surfaces the dead-connection error at Scan time, past the point this
// type can intercept. Production control queries therefore go through
// QueryContext (the schema and planner queries below); Row queries on a
// ControlQueryer are for tests and must not follow a failed swap.
type ControlQueryer struct {
	side    *Side
	cn      *sql.Conn
	retried bool  // the dead-connection swap already happened
	err     error // sticky: the swap failed, no usable control connection
}

var _ Queryer = (*ControlQueryer)(nil)

// Control checks out one policy-applied control session (the
// AcquireControl contract) wrapped for dead-connection recovery. The
// control pool holds a SINGLE physical connection, so a session must
// not be held across another control acquisition on the same side
// (that would self-deadlock, as a pinned scan connection at
// parallel=1 did): use WithControl, which releases before returning.
func (s *Side) Control(ctx context.Context) (*ControlQueryer, error) {
	cn, err := s.AcquireControl(ctx)
	if err != nil {
		return nil, err
	}
	return &ControlQueryer{side: s, cn: cn}, nil
}

// WithControl acquires one control session, runs fn on it and releases
// the session before fn's result is returned.
func (s *Side) WithControl(ctx context.Context, fn func(q Queryer) error) error {
	q, err := s.Control(ctx)
	if err != nil {
		return err
	}
	defer q.Close()
	return fn(q)
}

func (t *ControlQueryer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if t.err != nil {
		return nil, t.err
	}
	rows, err := t.cn.QueryContext(ctx, query, args...)
	if err != nil && !t.retried && DeadConn(err) {
		t.retried = true
		if err := t.swap(ctx); err != nil {
			// the replacement (or its policy) failed: report THAT
			// error, not the original dead-connection error — a caller
			// seeing the sticky failure must learn why the session is
			// unusable, not that one query hit a dead socket
			t.err = err
			return nil, err
		}
		return t.cn.QueryContext(ctx, query, args...)
	}
	return rows, err
}

func (t *ControlQueryer) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.cn.QueryRowContext(ctx, query, args...)
}

// Close returns the session's connection to the pool. Idempotent.
func (t *ControlQueryer) Close() error {
	cn := t.cn
	t.cn = nil
	if cn == nil {
		return nil
	}
	return cn.Close()
}

// swap discards the dead connection and takes a fresh one, whose
// checkout re-applies the full policy (AcquireControl) before the
// retried query may run. The ORDER is the point (P1-2): the old
// checkout is closed BEFORE the replacement is checked out. This
// session pins the control pool's single slot, and a dead connection
// whose error is not driver.ErrBadConn (the driver's plain
// ErrInvalidConn on a KILLed idle socket) is NOT released by
// database/sql — acquiring first would wait on our own slot forever.
// On failure the session keeps the (closed) handle, so later queries
// fail loudly instead of hitting a nil.
func (t *ControlQueryer) swap(ctx context.Context) error {
	old := t.cn
	t.cn = nil
	if old != nil {
		_ = old.Close() // free the pool slot before the replacement checkout
	}
	fresh, err := t.side.AcquireControl(ctx)
	if err != nil {
		t.cn = old // the closed handle: later queries fail loudly via t.err
		return fmt.Errorf("%s: replace dead control connection: %w", t.side.Name, err)
	}
	t.cn = fresh
	return nil
}

// OneRow scans the single row a metadata query must return (the Row
// form, on a QueryContext so a dead control connection is recovered
// transparently, see ControlQueryer). dest takes pointers, one per
// column; a missing row is sql.ErrNoRows.
func OneRow(ctx context.Context, q Queryer, query string, dest []any, args ...any) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	if err := rows.Scan(dest...); err != nil {
		return err
	}
	return rows.Err()
}

// Masked returns the redacted endpoint description for logs and reports.
func (s *Side) Masked() string { return s.ep.MaskedDSN() }

// Close releases both pools.
func (s *Side) Close() error {
	err1 := s.scan.Close()
	err2 := s.ctl.Close()
	if err1 != nil {
		return err1
	}
	return err2
}
