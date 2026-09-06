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

// connChecked checks out the dedicated write connection in a usable
// state — the SINGLE initialization rule for the pool, shared by
// OpenWriter and Conn so the two paths cannot drift:
//
//   - the guardrails are re-applied on EVERY checkout (R6-3): a
//     replacement physical session starts at the SERVER DEFAULTS (lock
//     wait 50, no zero-date sql_mode flags), only the re-apply puts
//     them back;
//   - a checkout that comes back DEAD (a KILLed idle session, a dropped
//     network) is NOT handed out: it is closed and a fresh checkout is
//     tried, bounded to THREE attempts total (never a loop). A non-dead
//     guardrail failure (an unsupported variable is a warning inside
//     applyGuardrails, not an error) is an error, and three dead
//     sessions in a row is an error. Only a live, guardrailed session
//     is handed out.
//
// Why three attempts, not two (the "one replacement" minimum): the real
// driver reports a KILLed IDLE socket in two phases. The FIRST
// operation on it (the first guardrail SET) returns its plain
// ErrInvalidConn — the client's write went out, the read got EOF/RST —
// and database/sql only releases a pinned connection on
// driver.ErrBadConn, so when the dead connection is closed it goes BACK
// to the single-slot pool, and the next checkout is the SAME physical
// connection. Only the driver's NEXT operation on it hits its closed
// check (driver.ErrBadConn), which is what finally discards it — the
// third checkout is the first one that is a genuinely NEW physical
// session. A single replacement would fail exactly in the case this
// exists for: the pool's only slot was just KILLed.
//
// The replacement happens BEFORE any transaction starts, which is the
// only safe place to recover: a dead connection MID-transaction (a DML
// sent, COMMIT lost in the network) has an UNKNOWN outcome, and
// replaying the transaction would double-write — the applier fails fast
// instead (see Applier.applyTx).
func (w *Writer) connChecked(ctx context.Context) (*sql.Conn, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		c, err := w.db.Conn(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: acquire write connection: %w", w.Name, err)
		}
		if err := applyGuardrails(ctx, c); err != nil {
			// applyGuardrails fails only on a DEAD session (an
			// unsupported guardrail is a warning, not an error) —
			// but check anyway: a non-dead failure must never
			// trigger a replacement
			if !DeadConn(err) {
				c.Close()
				return nil, fmt.Errorf("%s: guardrails on write connection: %w", w.Name, err)
			}
			lastErr = err
			_ = c.Close()
			continue
		}
		return c, nil
	}
	return nil, fmt.Errorf("%s: replace dead write connection (3 dead sessions in a row): %v", w.Name, lastErr)
}

// OpenWriter opens the single-connection destination write pool. The DSN
// is built by BuildWriterDSN (parseTime=true&loc=UTC stays mandatory so
// TIMESTAMP values round-trip identically through time.Time and back).
// The session gets the same best-effort guardrails as the read pools (lock
// wait timeout, statement timeout, zero-date sql_mode flags) but no
// read-only enforcement. The first checkout goes through connChecked —
// the same rule Conn uses — so the two initialization paths cannot drift.
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
