// Package sync turns a mtdiff comparison into a plan that makes the
// destination match the source: row-level INSERT/UPDATE/DELETE, or — when
// neither side offers a usable key — TRUNCATE plus a full resync from the
// source. Extra rows on the destination (even many) are row-level DELETEs
// as long as the rows can be addressed by key: the row counts never decide
// the mode. A table the destination is missing is created (structure sync
// on, no --where) before its data is synced, and the table's state (the
// next AUTO_INCREMENT value) is reconciled as the last step.
//
// The package never writes by itself: it decides (DecidePlan), computes the
// per-row operations (Engine) and renders the SQL (Builder). Execution
// happens in the Applier, which only exists after the user confirmed
// --apply, on a connection that is opened for the destination only.
package sync

import (
	"fmt"
	"sort"

	"mtdiff/internal/compare"
)

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
// srcKeyed / dstKeyed are len(schema.Key) > 0 per side after the --key
// override; srcUnique / dstUnique are schema.KeyIsUnique per side (an
// explicit --key resolved against the index catalog — see
// conn.ExplicitKeyIsUnique; an auto-selected key is always unique). key
// is the comma-joined key name, for the rejection message. keysAgree is
// true when the usable keys are the SAME columns in the SAME order on
// both sides (keyAgree): with --no-sync-schema the two sides may legally
// drift into different key shapes (e.g. PK (a,b) vs (b,a)), and the
// source's key bounds must never be rendered against a differently
// shaped destination key — such a table gets the full resync (or an
// argument error with --where) instead of row-level addressing.
//
// A row-level plan targets destination rows by their key values, so both
// sides need a usable key: with a keyless destination there are no row
// addresses at all (and no columns to render chunk bounds against), so
// only the full resync can converge. The row counts play NO part in the
// decision: extra rows on the destination (one stray row, or a whole
// range beyond the source's) are addressed by key and deleted, never a
// reason to resync the table.
//
// --where is the one case where uniqueness matters for SAFETY, not just
// efficiency: a filtered row-level sync deletes destination rows whose
// key the (filtered) source scan no longer shows, and with a NON-unique
// key a key value addresses a GROUP of rows — rows the filter excluded
// die with the group (the filter cannot be appended to the DELETE without
// changing what the sync converges). A --where row-level sync therefore
// requires a unique row address on BOTH sides; anything else is an
// argument error, rejected in the dry run and in the apply BEFORE any
// write connection exists. Without --where a non-unique key is safe: the
// engine replaces whole key groups (delete + insert) instead of updating
// single rows.
//
// keyOrderOK (conn.KeyOrderCompatible) is the ORDERING-semantics half of
// keysAgree: same key names is not enough, a string key orders by its
// collation and the families can drift under --no-sync-schema. With the
// names agreeing but the ordering incompatible, row-level addressing
// (and the --where filter on top of it) is refused: without --where the
// table FAILS CLOSED (the message says how to fix it — align the key
// family/collation, or let the structure sync align it), with --where it
// is an argument error. The structure-sync pre-step repairs the case it
// can (it aligns the destination's key type and collation with the
// source's) and the post-repair prepare sees compatible keys, so this
// gate fires only for a pair the structure sync did not (and will not)
// repair.
//
// FULL mode is only produced without a --where filter: TRUNCATE is a
// whole-table operation and cannot honor a filter (a keyless side +
// --where is an error instead).
func DecidePlan(res compare.TableResult, srcKeyed, dstKeyed, srcUnique, dstUnique bool, key, where string, keysAgree, keyOrderOK bool) Plan {
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
	if where != "" && !(srcKeyed && dstKeyed) {
		p.Mode, p.Error, p.ArgErr = ModeError,
			"no usable key on both sides and --where is set: cannot sync a keyless table with a filter", true
		return p
	}
	if where != "" && !(srcUnique && dstUnique) {
		p.Mode, p.Error, p.ArgErr = ModeError,
			fmt.Sprintf("--where requires a unique row-addressing key for sync; key (%s) is not PRIMARY KEY or NOT NULL UNIQUE", key), true
		return p
	}
	if where != "" && srcKeyed && dstKeyed && !keysAgree {
		p.Mode, p.Error, p.ArgErr = ModeError,
			fmt.Sprintf("--where requires the SAME usable key on both sides; the destination's key differs from the source's (%s) — filtered row addressing is impossible", key), true
		return p
	}
	if where != "" && srcKeyed && dstKeyed && keysAgree && !keyOrderOK {
		// P0-3: the names agree but the ordering semantics do not
		// (collation/family drift): a filtered row-level sync would
		// delete destination rows the filter cannot be applied to
		p.Mode, p.Error, p.ArgErr = ModeError,
			"--where requires compatible key ordering on both sides (the same key family and, for string keys, the same effective collation); the destination key orders rows differently — refusing a filtered row-level sync", true
		return p
	}
	switch {
	case !srcKeyed || !dstKeyed:
		p.Mode, p.Reason = ModeFull, "no usable key on both sides: truncate + full resync"
	case srcKeyed && dstKeyed && !keysAgree:
		// Keyed on both sides but the keys differ (names or order, e.g.
		// PK (a,b) vs (b,a) under --no-sync-schema): the source's key
		// bounds cannot be rendered against the destination's key
		// columns, so row-level addressing is not possible. A full
		// resync touches no keys on the destination and is safe.
		p.Mode, p.Reason = ModeFull, fmt.Sprintf("usable keys differ between the sides (source %s): truncate + full resync", key)
	case srcKeyed && dstKeyed && !keyOrderOK:
		// P0-3: the key NAMES agree but the ordering semantics do not
		// (a string key's collation differs — "Z" < "a" on one side,
		// the reverse on the other — or the families drifted): the
		// source's min/max and chunk bounds would address the WRONG
		// destination rows (a wrong out-of-range delete on a parent
		// cascades to its FK children). Row-level addressing is
		// forbidden, and the table fails closed instead of silently
		// choosing a destructive fallback: the message says how to fix
		// it. (A full resync would be order-independent, but it is a
		// destructive operation the operator did not ask for on a pair
		// whose key semantics the tool cannot prove.)
		p.Mode, p.Error = ModeError,
			"row-level sync requires identical key ordering semantics on both sides (the same key family and, for string keys, the same effective collation); the destination key orders rows differently and the source's key bounds cannot be used against it — align the key family/collation, or run without --no-sync-schema so the structure sync aligns it"
		return p
	default:
		p.Mode, p.Reason = ModeRowLevel, "row-level sync"
		for _, cd := range res.DiffChunks {
			p.Chunks = append(p.Chunks, cd.ID)
		}
		// The plan's chunk order IS the apply order (the executor runs
		// the groups in slice order). The cross-chunk unique-holder
		// verdict is only sound when chunks apply sequentially in KEY
		// ORDER — a safe value move relies on the earlier chunk freeing
		// the unique slot before the later chunk's write lands. Pin the
		// order here regardless of how the chunk list was assembled
		// (the pre-pass already emits it sorted; this is the safety
		// backstop).
		sort.Ints(p.Chunks)
	}
	return p
}
