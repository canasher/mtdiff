// Package sync turns a mtdiff comparison into a plan that makes the
// destination match the source: row-level INSERT/UPDATE/DELETE, or — when
// the destination has more rows than the source, or has no usable key —
// TRUNCATE plus a full resync from the source.
//
// The package never writes by itself: it decides (DecidePlan), computes the
// per-row operations (Engine) and renders the SQL (Builder). Execution
// happens in the Applier, which only exists after the user confirmed
// --apply, on a connection that is opened for the destination only.
package sync

import "mtdiff/internal/compare"

// Mode is the strategy chosen for one table.
type Mode string

const (
	ModeSkip     Mode = "SKIP"     // the pre-pass found the table identical
	ModeFull     Mode = "FULL"     // TRUNCATE the destination, resync everything
	ModeRowLevel Mode = "ROWLEVEL" // INSERT / UPDATE / DELETE per row
	ModeError    Mode = "ERROR"    // the table cannot be synced
)

// Plan is the sync strategy for one table.
type Plan struct {
	Table   string
	Mode    Mode
	SrcRows int64
	DstRows int64
	// Chunks lists the chunk IDs (from the pre-pass) whose rows must be
	// re-scanned for a ROWLEVEL plan. An empty list means "rescan every
	// chunk": the pre-pass skips chunk planning when the row counts differ
	// (the count difference alone already makes the table differ), so the
	// differing chunks are unknown and the engine covers the whole table.
	Chunks []int
	// Reason is a one-line human explanation for the report.
	Reason string
	// Error is set when Mode is ERROR.
	Error string
	// ArgErr marks an ERROR plan that is a misconfiguration (no flag
	// combination can fix it) rather than a runtime failure.
	ArgErr bool
}

// DecidePlan picks the strategy for one table from its comparison result.
// It is pure (no I/O) so the decision rules are unit-testable.
//
// hasUsableKey is len(schema.Key) > 0 after the --key override. The FULL
// mode is only produced without a --where filter: TRUNCATE is a whole-table
// operation and cannot honor a filter (keyless + --where is an error
// instead).
func DecidePlan(res compare.TableResult, hasUsableKey bool, where string) Plan {
	p := Plan{Table: res.Name, SrcRows: res.SrcRows, DstRows: res.DstRows}
	switch res.Status {
	case "OK":
		p.Mode, p.Reason = ModeSkip, "identical, nothing to do"
		return p
	case "ERROR":
		p.Mode, p.Error = ModeError, res.Error
		return p
	}
	// The table is DIFFERENT.
	if where != "" && !hasUsableKey {
		p.Mode, p.Error, p.ArgErr = ModeError,
			"table has no usable key and --where is set: cannot sync a filtered keyless table", true
		return p
	}
	switch {
	case !hasUsableKey:
		p.Mode, p.Reason = ModeFull, "no usable key: truncate + full resync"
	case res.DstRows > res.SrcRows && where == "":
		p.Mode, p.Reason = ModeFull, "destination has more rows than source: truncate + full resync"
	default:
		p.Mode, p.Reason = ModeRowLevel, "row-level sync"
		for _, cd := range res.DiffChunks {
			p.Chunks = append(p.Chunks, cd.ID)
		}
	}
	return p
}
