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

// OpenWriter opens the single-connection destination write pool. The DSN
// is built by BuildWriterDSN (parseTime=true&loc=UTC stays mandatory so
// TIMESTAMP values round-trip identically through time.Time and back). The
// session gets the same best-effort guardrails as the read pools (lock
// wait timeout, statement timeout, zero-date sql_mode flags) but no
// read-only enforcement.
func OpenWriter(ctx context.Context, name string, ep config.Endpoint, maxAllowedPacket int) (*Writer, error) {
	db, err := openPool(poolConfig(ep, maxAllowedPacket, 600))
	if err != nil {
		return nil, err
	}
	// Dedicated for the whole run, like the Side pools: never recycle.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	c, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: connect to %s: %w", name, ep.MaskedDSN(), err)
	}
	applyGuardrails(ctx, c)
	var one int
	if err := c.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		c.Close()
		db.Close()
		return nil, fmt.Errorf("%s: connect to %s: %w", name, ep.MaskedDSN(), err)
	}
	var version string
	if err := c.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		c.Close()
		db.Close()
		return nil, fmt.Errorf("%s: SELECT VERSION() (%s): %w", name, ep.MaskedDSN(), err)
	}
	c.Close() // back to the idle pool

	return &Writer{Name: name, Version: version, db: db}, nil
}

// Conn returns the dedicated write connection. The caller must Close it
// when done (it returns to the pool).
//
// The guardrails are re-applied on EVERY checkout, not just at
// OpenWriter time: if the physical connection dies (network failure, a
// KILL, a server restart) database/sql replaces it with a fresh session
// that starts at the SERVER DEFAULTS (lock wait 50, no zero-date
// sql_mode flags) — only the per-checkout re-apply puts the guardrails
// back. applyGuardrails is best-effort by design (the writer is allowed
// to write; the guardrails cap damage, they are not a correctness
// gate), so a guardrail that cannot be applied is reported, not an
// error: the checkout still succeeds.
func (w *Writer) Conn(ctx context.Context) (*sql.Conn, error) {
	c, err := w.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: acquire write connection: %w", w.Name, err)
	}
	applyGuardrails(ctx, c)
	return c, nil
}

// Close releases the pool.
func (w *Writer) Close() error {
	return w.db.Close()
}
