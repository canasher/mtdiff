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
	"net/url"
	"os"
	"strconv"
	"strings"

	_ "github.com/go-sql-driver/mysql"

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

// BuildDSN assembles the driver DSN. parseTime=true&loc=UTC is mandatory:
// both sides must interpret timestamps in UTC or TIMESTAMP columns spanning
// time zones produce false positives.
func BuildDSN(ep config.Endpoint, maxAllowedPacket int) string {
	return buildDSN(ep, maxAllowedPacket, 10)
}

// BuildWriterDSN is BuildDSN with a longer network write timeout: sending a
// multi-row INSERT batch can take far longer than a plain query send.
func BuildWriterDSN(ep config.Endpoint, maxAllowedPacket int) string {
	return buildDSN(ep, maxAllowedPacket, 600)
}

func buildDSN(ep config.Endpoint, maxAllowedPacket, writeTimeoutSec int) string {
	cred := url.User(ep.User)
	if ep.Password != "" {
		cred = url.UserPassword(ep.User, ep.Password)
	}
	hostport := net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port))
	var b strings.Builder
	b.WriteString(cred.String())
	b.WriteString("@tcp(")
	b.WriteString(hostport)
	b.WriteString(")/")
	b.WriteString(ep.Database)
	// interpolateParams=false (the driver default, stated deliberately):
	// parameters travel to the server and are bound as DATA there, so a
	// value is never rendered into the statement text on the client — the
	// only way string values with backslashes/quotes stay intact under
	// NO_BACKSLASH_ESCAPES (P0-3).
	fmt.Fprintf(&b, "?parseTime=true&loc=UTC&charset=utf8mb4&timeout=10s&readTimeout=10m&writeTimeout=%ds&interpolateParams=false", writeTimeoutSec)
	if maxAllowedPacket > 0 {
		fmt.Fprintf(&b, "&maxAllowedPacket=%d", maxAllowedPacket)
	}
	return b.String()
}

// openDB is the pool constructor. A test seam: unit tests swap in a
// fake driver to exercise the checkout/policy logic without a server;
// production always opens the real go-sql-driver/mysql.
var openDB = func(name, dsn string) (*sql.DB, error) { return sql.Open(name, dsn) }

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
	dsn := BuildDSN(ep, maxAllowedPacket)
	scanDB, err := openDB("mysql", dsn)
	if err != nil {
		return nil, err
	}
	ctlDB, err := openDB("mysql", dsn)
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
				return fmt.Errorf("refusing to continue: cannot enforce read-only session (read_only: %v; transaction read only: %v)", err, err2)
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
	if errors.Is(err, driver.ErrBadConn) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "bad connection") ||
		strings.Contains(msg, "invalid connection") ||
		strings.Contains(msg, "Lost connection")
}

// Ctl returns the control pool for introspection/planning queries.
func (s *Side) Ctl() *sql.DB { return s.ctl }

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
