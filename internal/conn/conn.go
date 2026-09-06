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
// The policy (applySession) is re-applied on EVERY scan checkout, not
// remembered per physical connection. A CONNECTION_ID memo cannot be
// the identity of a physical session: the server's counter resets
// across a restart, and a recycled ID on a NEW physical connection
// must not inherit the old one's "initialized" mark (the new session
// has no policy in it). applySession is idempotent, so the re-apply
// costs a handful of cheap SETs per checkout — safety over saved round
// trips. A connection the policy cannot be applied to is closed and
// never handed out.
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

// applySession enforces the read-only safety net and best-effort guardrails
// that may not exist on compatible layers. It is idempotent: every statement
// it runs may be re-executed on the same session without observable effect
// (the sql_mode flags in particular are appended at most once, see
// addSQLModeFlags).
//
// Read-only is enforced two-tier: MySQL proper only has a GLOBAL read_only,
// so a session SET fails with ER_VARIABLE_IS_READONLY (1229); the fallback is
// a session default transaction character (READ ONLY), which also covers
// implicit autocommit statements. TiDB inverts the problem: read_only is
// GLOBAL-only there as well (1229), and SET SESSION TRANSACTION READ ONLY is
// a disabled no-op (1235, unless tidb_enable_noop_functions is set), so both
// tiers fail. By default mtdiff then refuses to continue: a read pool the
// server cannot keep read-only is not acceptable silently. With
// allowUnenforcedReadOnly (--allow-unenforced-readonly) it proceeds instead,
// printing a per-connection warning: mtdiff still only issues SELECTs on
// these connections, the accepted risk is that the server could not stop
// other statements from a shared account.
func applySession(ctx context.Context, c *sql.Conn, allowUnenforced bool) error {
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
	applyGuardrails(ctx, c)
	return nil
}

// applyGuardrails applies the best-effort session guardrails without the
// read-only enforcement. It is shared by the read-only pools (via
// applySession) and the destination write pool (which is intentionally not
// read-only — see OpenWriter in writer.go).
func applyGuardrails(ctx context.Context, c *sql.Conn) {
	bestEffort(ctx, c, "SET SESSION innodb_lock_wait_timeout = 5")
	bestEffort(ctx, c, "SET SESSION max_execution_time = 300000")
	addSQLModeFlags(ctx, c)
}

// addSQLModeFlags ensures the zero-date guard flags are in the session
// sql_mode. Unlike a blind CONCAT-append (which grew the mode by 31
// characters per execution until it exceeded sql_max_mode_size, 255 on
// 8.0, and warned on every later statement), it appends each flag at most
// once, so re-applying the policy to a connection is a no-op.
func addSQLModeFlags(ctx context.Context, c *sql.Conn) {
	var mode string
	if err := c.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode").Scan(&mode); err != nil {
		fmt.Fprintf(os.Stderr, "warn: SELECT @@SESSION.sql_mode failed: %v\n", err)
		return
	}
	var add []string
	for _, flag := range []string{"NO_ZERO_DATE", "NO_ZERO_IN_DATE"} {
		if !strings.Contains(mode, flag) {
			add = append(add, flag)
		}
	}
	if len(add) == 0 {
		return
	}
	// Join, don't concatenate literals: an empty current mode must not
	// render as an empty string literal glued onto the new one.
	newMode := strings.Join(append([]string{mode}, add...), ",")
	bestEffort(ctx, c, "SET SESSION sql_mode = '"+strings.ReplaceAll(newMode, "'", "''")+"'")
}

func bestEffort(ctx context.Context, c *sql.Conn, stmt string) {
	if _, err := c.ExecContext(ctx, stmt); err != nil {
		fmt.Fprintf(os.Stderr, "warn: %s failed: %v\n", stmt, err)
	}
}

// AcquireScan returns a dedicated scan connection whose session safety
// policy (read-only enforcement and guardrails) is in effect. The
// policy is re-applied on EVERY checkout, not remembered per physical
// connection: a CONNECTION_ID memo cannot be a session's identity (the
// server's counter resets across a restart, and a recycled ID on a NEW
// physical connection must not inherit the old one's initialization),
// and applySession is idempotent, so the cost is a handful of SETs per
// checkout. A connection the policy cannot be applied to is closed and
// NOT handed out. Callers must Close it when done.
func (s *Side) AcquireScan(ctx context.Context) (*sql.Conn, error) {
	c, err := s.scan.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: acquire scan connection: %w", s.Name, err)
	}
	if err := applySession(ctx, c, s.allowUnforced); err != nil {
		c.Close()
		return nil, fmt.Errorf("%s: init scan connection: %w", s.Name, err)
	}
	return c, nil
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
// safety policy in effect. Same contract as AcquireScan: the policy is
// re-applied on EVERY checkout, not remembered per physical connection
// (a CONNECTION_ID memo cannot be a session's identity — see the Side
// doc), and a connection the policy cannot be applied to is closed and
// NOT handed out. Callers must Close it when done.
//
// A checkout that comes back DEAD (killed by the server, network loss,
// a restart — the policy's SETs fail with a bad-connection error) is
// not a policy refusal: the dead connection is closed, a FRESH one is
// checked out and the FULL policy re-applied to it before the checkout
// may be used (fresh connection → policy → query, never the reverse),
// once. A second dead connection, or a genuine policy refusal on the
// fresh one, is an error: nothing unguarded is ever handed out.
func (s *Side) AcquireControl(ctx context.Context) (*sql.Conn, error) {
	c, err := s.ctl.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: acquire control connection: %w", s.Name, err)
	}
	if err := applySession(ctx, c, s.allowUnforced); err != nil {
		if !DeadConn(err) {
			c.Close()
			return nil, fmt.Errorf("%s: init control connection: %w", s.Name, err)
		}
		_ = c.Close()
		c, err = s.ctl.Conn(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: replace dead control connection: %w", s.Name, err)
		}
		if err := applySession(ctx, c, s.allowUnforced); err != nil {
			c.Close()
			return nil, fmt.Errorf("%s: init control connection: %w", s.Name, err)
		}
	}
	return c, nil
}

// ControlQueryer is a Queryer bound to the control pool: one
// policy-applied checkout, with transparent dead-connection recovery.
// A dead physical connection (server KILL, network partition, a
// restart) surfaces as a bad-connection error on the next query; the
// old connection is then closed, a FRESH one checked out — the policy
// applied FIRST, via AcquireControl — and the query retried exactly
// once on it. Query-first / policy-later is impossible: the retry runs
// only on a connection whose policy application already succeeded, and
// a session whose replacement fails is sticky-dead (every later query
// fails loudly; the Row queries below surface the dead driver error).
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
		t.err = t.swap(ctx)
		if t.err == nil {
			rows, err = t.cn.QueryContext(ctx, query, args...)
		}
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

// swap discards the dead connection and takes a fresh one: the new
// checkout re-applies the full policy before the retried query may run.
// On failure the session keeps the (dead) connection, so later queries
// fail loudly instead of hitting a nil.
func (t *ControlQueryer) swap(ctx context.Context) error {
	old := t.cn
	fresh, err := t.side.AcquireControl(ctx)
	if err != nil {
		t.cn = old
		return fmt.Errorf("%s: replace dead control connection: %w", t.side.Name, err)
	}
	_ = old.Close()
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
