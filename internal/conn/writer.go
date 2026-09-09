package conn

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/go-sql-driver/mysql"

	"mtdiff/internal/config"
)

// Writer is the single dedicated write connection to a destination database.
//
// It is the only connection in the tool that is not forced read-only. The
// read-only guarantee of diff/tables (and of every scan/control connection
// the sync command opens) is untouched: OpenSide still refuses to run when
// it cannot enforce read-only. A Writer exists only for the sync command,
// is opened only after the user confirmed --apply, and only for the
// destination endpoint.
type Writer struct {
	Name    string
	Version string
	db      *sql.DB
}

// writePolicy is the writer connection's session policy: the time-zone
// pin (REQUIRED — the same correctness argument as the read pools'
// applySession: a TIMESTAMP written through a session in the server's
// default zone is interpreted in that zone, and the sync's
// "same instant on both sides" guarantee is only as good as the zone
// both endpoints' sessions actually use) and the guardrails (the writer
// is deliberately NOT read-only). A non-dead policy failure is an
// error: the connection is closed and NOT handed out.
func (w *Writer) writePolicy(ctx context.Context, c *sql.Conn) error {
	if _, err := c.ExecContext(ctx, "SET SESSION time_zone = '+00:00'"); err != nil {
		return fmt.Errorf("cannot pin session time_zone to +00:00 (TIMESTAMP values would be written in the server's default zone): %w", err)
	}
	if err := applyGuardrails(ctx, c); err != nil {
		return fmt.Errorf("apply session guardrails: %w", err)
	}
	return nil
}

// connChecked checks out the dedicated write connection in a usable
// state — it IS the shared checkoutChecked rule (see there for the
// single-call bounded-replacement contract and the two-phase
// dead-connection rationale): the writePolicy is re-applied on EVERY
// checkout, a dead checkout is closed and a fresh one tried (bounded to
// three attempts, never a loop), a non-dead policy failure is an error,
// and only a live, policy-initialized session is handed out.
//
// The replacement happens BEFORE any transaction starts, which is the
// only safe place to recover: a dead connection MID-transaction (a DML
// sent, COMMIT lost in the network) has an UNKNOWN outcome, and
// replaying the transaction would double-write — the applier fails fast
// instead (see Applier.applyTx).
func (w *Writer) connChecked(ctx context.Context) (*sql.Conn, error) {
	return checkoutChecked(ctx, w.Name, w.db, "write", w.writePolicy)
}

// OpenWriter opens the single-connection destination write pool. The
// DSN is built by BuildWriterDSN (parseTime=true&loc=UTC stays
// mandatory so TIMESTAMP values round-trip identically through
// time.Time and back, and the session time_zone is pinned to +00:00 at
// connect time — see poolConfig). The session gets the required time-
// zone pin plus the same best-effort guardrails as the read pools (lock
// wait timeout, statement timeout, zero-date sql_mode flags) but no
// read-only enforcement. The first checkout goes through connChecked —
// the same rule Conn uses — so the two initialization paths cannot
// drift.
func OpenWriter(ctx context.Context, name string, ep config.Endpoint, maxAllowedPacket int) (*Writer, error) {
	db, err := openPool(poolConfig(ep, maxAllowedPacket, 600))
	if err != nil {
		return nil, err
	}
	// Dedicated for the whole run, like the Side pools: never recycle.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	w := &Writer{Name: name, db: db}
	c, err := w.connChecked(ctx)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: connect to %s: %w", name, ep.MaskedDSN(), err)
	}
	defer c.Close()
	var one int
	if err := c.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: connect to %s: %w", name, ep.MaskedDSN(), err)
	}
	var version string
	if err := c.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: SELECT VERSION() (%s): %w", name, ep.MaskedDSN(), err)
	}
	w.Version = version
	return w, nil
}

// Conn returns the dedicated write connection in a usable state (the
// caller must Close it when done — it returns to the pool).
//
// The connection is checked out through connChecked: the guardrails
// re-applied on every checkout (R6-3), and a dead checkout (a KILLed
// idle session, a dropped network) replaced before handout — the dead
// connection is closed, a fresh one is guardrailed, and only a live,
// guardrailed session is returned. The replacement happens BEFORE any
// transaction starts, which is the only safe place: a dead connection
// mid-transaction (a DML sent, COMMIT lost in the network) has an
// UNKNOWN commit outcome, and replaying the transaction would
// double-write — the applier fails fast instead of retrying (see
// Applier.applyTx).
func (w *Writer) Conn(ctx context.Context) (*sql.Conn, error) {
	return w.connChecked(ctx)
}

// Close releases the pool.
func (w *Writer) Close() error {
	return w.db.Close()
}
