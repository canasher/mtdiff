// Package conn manages MySQL connections, session safety policy and schema
// introspection.
package conn

import (
	"context"
	"database/sql"
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
// It holds two pools: a control pool (MaxOpenConns=1) for introspection and
// planning queries, and a scan pool (MaxOpenConns=parallel) whose dedicated
// connections are pinned to workers so the session safety policy stays in
// effect for the whole scan. OpenSide pre-warms the scan pool (the policy
// is applied once per connection there), so AcquireScan is a plain checkout.
type Side struct {
	Name    string
	Version string
	ep      config.Endpoint
	scan    *sql.DB
	ctl     *sql.DB
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
	fmt.Fprintf(&b, "?parseTime=true&loc=UTC&charset=utf8mb4&timeout=10s&readTimeout=10m&writeTimeout=%ds", writeTimeoutSec)
	if maxAllowedPacket > 0 {
		fmt.Fprintf(&b, "&maxAllowedPacket=%d", maxAllowedPacket)
	}
	return b.String()
}

// OpenSide opens both pools, preconditions the control connection and
// pre-warms the scan pool with the session policy applied, then verifies the
// server answers.
//
// The scan pool is pre-warmed (one policy application per connection) so
// that AcquireScan is a plain checkout. Re-applying the policy on every
// checkout cost a full SET round-trip per chunk, and the sql_mode append is
// not idempotent: re-applying it grew the session sql_mode by 31 characters
// per checkout until it exceeded sql_max_mode_size (255 on 8.0) and every
// subsequent chunk printed a stderr warning.
//
// allowUnenforcedReadOnly is config.Options.AllowUnenforcedReadOnly; see
// applySession for what it relaxes.
func OpenSide(ctx context.Context, name string, ep config.Endpoint, maxAllowedPacket, parallel int, allowUnenforcedReadOnly bool) (*Side, error) {
	if parallel < 1 {
		parallel = 1
	}
	dsn := BuildDSN(ep, maxAllowedPacket)
	scanDB, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	ctlDB, err := sql.Open("mysql", dsn)
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
		sc.Close() // back to the idle pool, policy already in effect
	}
	return &Side{Name: name, Version: version, ep: ep, scan: scanDB, ctl: ctlDB}, nil
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

// AcquireScan returns a dedicated scan connection. The session policy is
// applied exactly once per connection, during the pool pre-warm in
// OpenSide, so this is a plain checkout (no SET round-trip per chunk).
// Callers must Close it when done.
func (s *Side) AcquireScan(ctx context.Context) (*sql.Conn, error) {
	c, err := s.scan.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: acquire scan connection: %w", s.Name, err)
	}
	return c, nil
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
