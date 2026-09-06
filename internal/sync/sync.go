package sync

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"mtdiff/internal/chunk"
	"mtdiff/internal/compare"
	"mtdiff/internal/conn"
	"mtdiff/internal/normalize"
)

// Options control the sync run. Cmp carries the comparison behavior (the
// sync reuses the comparer for the pre-pass and the post-sync verification
// verbatim).
type Options struct {
	Cmp         compare.Options
	Batch       int
	SampleLimit int
	MaxPacket   int // max_allowed_packet as configured (0 = unset)
	// SyncSchema enables the structure pre-step (on by default; the CLI
	// maps --no-sync-schema onto this): before syncing a table's data,
	// bring its destination structure in line with the source's.
	SyncSchema bool
	// AllowStructureTruncate restores the pre-P1-3 behavior as a fallback:
	// when the in-place structure ALTER fails (e.g. a new NOT NULL column
	// without a default on a non-empty table), truncate the destination
	// and re-apply the DDL on the empty table. Off by default: the
	// in-place failure then stops the table with its data preserved.
	AllowStructureTruncate bool
	// AllowRowRewrite permits the destructive row rewrite (P0-2): when a
	// unique-value swap, cycle, or holder conflict is detected within a
	// chunk, the affected rows are DELETEd and INSERTed again (a no-op
	// holder is rewritten in place) so the unique slots can be freed.
	// Off by default: such a rewrite fires FK ON DELETE CASCADE,
	// triggers and audit logs for rows the user never asked to change,
	// so the table is REFUSED instead.
	//
	// It authorizes the row rewrite ONLY (P0-2, round 3): a cross-chunk
	// swap still cannot be ordered row-level, so with the flag the plan
	// becomes the order-independent FULL resync (TRUNCATE + reload) —
	// shown in the plan and in the confirmation summary, and executed
	// only when that FULL plan (with its TRUNCATE) is what the user
	// confirmed. Without the flag the table is refused outright. The
	// flag is never a blanket authorization to TRUNCATE at runtime: a
	// confirmed row-level plan can never escalate to a full resync in
	// the same run (the apply stops with ErrReplanRequired).
	AllowRowRewrite bool
	// Progress receives long-running phase updates (pre-pass scans, apply
	// chunks, verification scans), forwarded to the comparer when the
	// caller left Cmp.Progress unset. nil = no progress output.
	Progress func(format string, args ...any)
}

// DestructiveScope is the set of destructive operations a plan may
// execute. It is computed from the plan shown to the user BEFORE the
// confirmation (the preflight runs the same row planning the apply
// re-runs), and the apply re-plans right before writing and refuses to
// execute anything outside it: a confirmed row-level plan can never
// TRUNCATE, and a rewrite appears in the summary before it may run.
type DestructiveScope struct {
	// RowRewrite: the destructive row rewrite (DELETE+INSERT) that frees
	// a unique slot, permitted by --allow-row-rewrite AND shown in the
	// confirmed plan.
	RowRewrite bool
	// FullResync: the order-independent full resync (TRUNCATE + reload),
	// shown in the confirmed plan (a keyless table, a stale or
	// cross-chunk plan with --allow-row-rewrite, or a structure repair
	// that ends on the full load).
	FullResync bool
	// RewriteFingerprints: the IDENTITY of each rewrite group the plan
	// showed (P0): one stable digest per group (triggering constraints +
	// the group's destination keys, order-independent — see
	// fingerprint.go). The apply-time re-plan may run this set or a
	// SUBSET of it, and nothing else: a re-plan that keeps the COUNT
	// but swaps the group (data moved between confirmation and the
	// re-plan) is an expansion and stops the table. The digest carries
	// no business value.
	RewriteFingerprints []string
}

// scopeGate judges the re-plan's destructive part against the CONFIRMED
// plan (P0). It returns the human-readable description of what the
// re-plan needs beyond the confirmed scope — the caller then stops the
// table (ErrReplanRequired, zero destructive statements executed) — or
// "" when the re-plan is within the confirmed scope.
//
// The two destructive axes are checked separately:
//
//   - the full resync: a re-plan that needs the TRUNCATE runs only when
//     the confirmed plan is the one that showed it (a confirmed
//     row-level plan can never TRUNCATE in the same run — P0-1);
//   - the rewrites: by IDENTITY, not count (P0). The re-plan's rewrite
//     groups must be a SUBSET of the groups the confirmed plan SHOWED:
//     the same groups, fewer of them, or none pass; a group that was
//     not in the confirmed set (data moved between the confirmation and
//     the re-plan — even with the SAME count) stops the table. A count
//     cannot prove this: confirmed {A,B} and current {A,C} both show 2.
//     The message names counts only — never key values (the refusal
//     must not leak business data).
func scopeGate(mode Mode, w rowWork, confirmed TableSync) string {
	switch {
	case mode == ModeFull && !confirmed.Scope.FullResync:
		return "the full resync (TRUNCATE + reload)"
	case mode == ModeRowLevel && !fingerprintSubset(w.rewriteFPs, confirmed.Scope.RewriteFingerprints):
		return fmt.Sprintf("%d destructive row rewrite group(s) (DELETE+INSERT) — %d of them were not part of the confirmed plan (confirmed=%d, current=%d)",
			w.rewrites, fingerprintNew(w.rewriteFPs, confirmed.Scope.RewriteFingerprints),
			confirmed.Rewrites, w.rewrites)
	}
	return ""
}

// TableSync is the per-table outcome of a sync run.
type TableSync struct {
	Name string
	// Mode is the strategy: "SKIP" | "FULL" | "ROWLEVEL" | "ERROR" |
	// "CREATE" (the destination table is missing and is created) |
	// "DROP" (a destination-only table is dropped, whole-database mode) |
	// "STATE" (only the table state, AUTO_INCREMENT, is realigned).
	Mode    string
	SrcRows int64
	DstRows int64
	Inserts int
	Updates int
	Deletes int
	Chunks  int
	// Rewrites is the number of destructive row-rewrite groups (P0-3):
	// row groups the sync deletes and re-inserts to free a unique slot.
	// It is 0 for a plan that rewrites nothing, and the confirmation
	// summary lists it separately — a rewrite is the one kind of
	// statement that touches rows the user did not ask to change.
	Rewrites int
	// Scope is the destructive scope the plan was CONFIRMED with: the
	// preflight's (read-only) plan, computed before the confirmation
	// prompt. The apply re-plans and must stay within it.
	Scope     DestructiveScope
	Truncated bool
	SampleSQL []string
	// SchemaChanged is set when the destination's structure drifted from
	// the source's and the DDL below aligns it before the data sync (or
	// the table is created / dropped, which is structure work too).
	SchemaChanged bool
	SchemaSQL     []string
	Status        string // "SKIPPED" | "PLANNED" | "APPLIED" | "FAILED"
	Error         string
	// ArgErr marks a FAILED plan that no flag combination can fix (a
	// misconfiguration, e.g. keyless + --where): the CLI maps it to an
	// argument error (exit 3) instead of a runtime failure (exit 2).
	ArgErr bool
	// Verified is the post-sync verification status for tables that were
	// synced: "OK" | "DIFFERENT" | "ERROR" ("" when not synced).
	Verified string
	// StateChanged is set when the table state (the next AUTO_INCREMENT
	// value) drifted from the source's and the DDL below realigns it.
	StateChanged bool
	StateSQL     []string
	// StateNote reports a table-state divergence no ALTER can fix
	// (the destination's counter is above the source's — a counter can
	// only be raised): no statement is planned, the run reports and
	// exits non-zero.
	StateNote string
	// StateVerified is the post-apply table-state check: "OK" |
	// "DIFFERENT" ("" when out of scope, e.g. --where or a source without
	// an auto-increment column).
	StateVerified string
}

// errEscalateFull signals that a ROWLEVEL plan went stale (the pre-pass
// diff chunks no longer line up with the re-plan — data moved between the
// two) and the table must fall back to the FULL resync.
var errEscalateFull = errors.New("data moved between the pre-pass and the row plan: escalating to full resync")

// ErrMisconfigured marks a plan failure that no flag combination can fix
// (a misconfiguration, not a runtime failure). The CLI maps it to an
// argument error (exit 3).
var ErrMisconfigured = errors.New("sync misconfiguration")

// errUniqueRewriteRefused marks a table whose unique-value conflict
// (swap / cycle / holder) needs the destructive row rewrite the operator
// did not permit (P0-2): it is a refusal, not a failure of the
// destination — the CLI reports it and exits non-zero, but the message
// tells the operator exactly which flag lifts it.
var errUniqueRewriteRefused = errors.New("unique-value conflict requires the destructive row rewrite, which is not permitted")

// ErrReplanRequired marks a table the apply STOPPED because its
// re-planned operations exceed the destructive scope the user confirmed
// (P0): the apply-time plan needs a destructive operation the confirmed
// plan did not show. No destructive escalation is executed — the table
// is left as-is (already-applied earlier tables are kept) and the
// operator re-runs so the new plan is reviewed and confirmed like any
// first plan.
var ErrReplanRequired = errors.New("the apply-time plan requires a destructive operation that was not part of the confirmed plan; no destructive escalation was executed. Re-run the dry-run/apply so the new plan can be reviewed and confirmed")

// ErrFullResyncRequired marks the specific condition behind a replan
// refusal: the re-plan (a stale row plan, or a cross-chunk unique swap)
// needs the order-independent full resync (TRUNCATE + reload). It is
// wrapped by ErrReplanRequired when the confirmed scope lacks it.
var ErrFullResyncRequired = errors.New("the table requires the order-independent full resync (TRUNCATE + reload)")

// Runner drives a sync run between two open sides.
type Runner struct {
	Src, Dst *conn.Side
	o        Options
	cmp      *compare.Comparer

	// Per-side database default collations, resolved once per run (see
	// sideDefaultCollations); "" when the query fails (strict fallback).
	sdOnce sync.Once
	sdSrc  string
	sdDst  string
	// aiWarned warns once per run when the AUTO_INCREMENT state cannot be
	// read (a backend without the column): the table-state reconciliation
	// degrades to skipped, never to a failed run.
	aiWarned sync.Once
	// Per-side table-state exactness, resolved once per run (see
	// stateExactness): whether the backend reports an EXACT next
	// AUTO_INCREMENT value, or a pre-allocated range estimate (an
	// allocator, e.g. TiDB's batch allocation), or is an unknown backend
	// decided per table by its reported gap.
	stateSrc, stateDst *sideState

	// Pre-DROP existence probes (ApplyDrop's TOCTOU re-gate): nil in
	// production, where each probe queries the side's information
	// schema; unit tests override them to drive the re-check branches.
	srcDropRecheck   func(ctx context.Context, table string) (bool, error)
	dstDropRecheck   func(ctx context.Context, table string) (bool, error)
	dstDropPostcheck func(ctx context.Context, table string) (bool, error)
}

// stateGapLimit: the largest distance a side's reported next
// AUTO_INCREMENT value may sit above max(column)+1 before the backend is
// taken to pre-allocate ID ranges (an allocator, e.g. TiDB) and the
// reported value is not an exact next value. Used only for backends the
// capability probe cannot classify (see stateExactness).
const stateGapLimit = 10000

// sideState is one side's table-state exactness, resolved once per run.
type sideState struct {
	once  sync.Once
	exact bool // the backend reports exact next values (MySQL proper, MariaDB)
	known bool // the capability was resolved (false: unknown backend)
	// inexactWarned warns once when the side is a known allocator;
	// gapWarned warns once when the per-table gap heuristic skips a
	// table on an unknown backend.
	inexactWarned sync.Once
	gapWarned     sync.Once
}

func NewRunner(src, dst *conn.Side, o Options) *Runner {
	if o.Cmp.Progress == nil {
		o.Cmp.Progress = o.Progress
	}
	return &Runner{Src: src, Dst: dst, o: o, cmp: compare.NewComparer(o.Cmp),
		stateSrc: &sideState{}, stateDst: &sideState{}}
}

// sideDefaultCollations resolves each side's database default collation
// (conn.DefaultCollation), once per run. It is best-effort: a backend
// that refuses the query degrades to the strict comparison (a collation
// difference is always reported), never to a failed run.
func (r *Runner) sideDefaultCollations(ctx context.Context) (src, dst string) {
	r.sdOnce.Do(func() {
		// best-effort: a backend that refuses the query degrades to the
		// strict comparison, never to a failed run
		_ = r.Src.WithControl(ctx, func(q conn.Queryer) error {
			var err error
			r.sdSrc, err = conn.DefaultCollation(ctx, q)
			return err
		})
		_ = r.Dst.WithControl(ctx, func(q conn.Queryer) error {
			var err error
			r.sdDst, err = conn.DefaultCollation(ctx, q)
			return err
		})
		if r.sdSrc == "" || r.sdDst == "" {
			fmt.Fprintf(os.Stderr, "warn: could not resolve the database default collation (src=%q dst=%q); collation differences are treated as drift\n", r.sdSrc, r.sdDst)
		}
	})
	return r.sdSrc, r.sdDst
}

// PrePass runs the (read-only) comparison over the given tables.
func (r *Runner) PrePass(ctx context.Context, tables []string) ([]compare.TableResult, error) {
	return r.cmp.Compare(ctx, r.Src, r.Dst, tables)
}

// mysqlVersionRE matches the plain "X.Y(.Z)" VERSION() output of MySQL
// proper (and its minor variants) — and only that. A TiDB server started
// with a compatibility version reports a plain number too, so this is
// never used alone: the allocator-variable probe runs first.
var mysqlVersionRE = regexp.MustCompile(`^\d+\.\d+`)

// stateFor picks the per-side state tracker (the sides are distinct
// objects; pointer identity is the key).
func (r *Runner) stateFor(side *conn.Side) *sideState {
	if side == r.Src {
		return r.stateSrc
	}
	return r.stateDst
}

// stateExactness resolves once per side whether the backend reports
// EXACT next AUTO_INCREMENT values. A batch allocator (TiDB: the reported
// next value is a pre-allocated range, an explicit counter below the
// range end is silently ignored, and a full resync re-allocates a new
// range) is inexact: the state cannot be converged there, so the
// reconciliation degrades to skipped for that side (a one-shot warning,
// never a failed run). MySQL proper (and MariaDB) are exact, even when
// someone explicitly raised a counter far above the data — that is a
// state divergence, not a capability limit. A backend neither signal
// classifies stays unknown: the per-table gap heuristic decides instead
// (sideStateExact), and a whole database is never disabled by one
// table's gap.
func (r *Runner) stateExactness(ctx context.Context, side *conn.Side) (exact, known bool) {
	v := strings.ToLower(side.Version)
	if strings.Contains(v, "tidb") {
		return false, true
	}
	// TiDB exposes allocator variables; a plain MySQL server does not
	// (an unknown system variable is an error, not a value).
	found := false
	_ = side.WithControl(ctx, func(q conn.Queryer) error {
		var err error
		found, err = allocatorVariablePresent(ctx, q)
		return err
	})
	if found {
		return false, true
	}
	if mysqlVersionRE.MatchString(side.Version) || strings.Contains(v, "mariadb") {
		return true, true
	}
	return false, false
}

// allocatorVariablePresent reports whether the backend exposes TiDB's
// allocator variables: a plain MySQL server answers an unknown system
// variable with an error, not a value.
func allocatorVariablePresent(ctx context.Context, q conn.Queryer) (bool, error) {
	var chunk int
	if err := conn.OneRow(ctx, q, "SELECT @@tidb_alloc_chunk_size", []any{&chunk}); err != nil {
		return false, err
	}
	return true, nil
}

// sideStateExact decides whether this side's reported value for THIS
// table is an exact next value. The result is cached per side; for an
// unknown backend the decision is per table (the reported gap), which is
// exactly why the capability is never judged globally from one random
// table.
func (r *Runner) sideStateExact(ctx context.Context, side *conn.Side, f aiFacts) bool {
	ss := r.stateFor(side)
	ss.once.Do(func() { ss.exact, ss.known = r.stateExactness(ctx, side) })
	switch {
	case ss.exact:
		return true
	case ss.known:
		ss.inexactWarned.Do(func() {
			fmt.Fprintf(os.Stderr, "warn: %s's backend pre-allocates auto-increment ID ranges (an allocator): its reported next AUTO_INCREMENT value is an estimate, the table state cannot be converged, and state reconciliation is skipped for this side\n",
				side.Name)
		})
		return false
	}
	// unknown backend: judge THIS table by its reported gap
	if f.maxPlus > 0 && f.value-f.maxPlus > stateGapLimit {
		ss.gapWarned.Do(func() {
			fmt.Fprintf(os.Stderr, "warn: %s: table %s's next AUTO_INCREMENT (%d) sits %d above the data maximum (pre-allocated ID range): the table state is not exactly comparable on this backend and is skipped for this table\n",
				side.Name, conn.QuoteIdent(f.table), f.value, f.value-f.maxPlus)
		})
		return false
	}
	return true
}

// tableState is the AUTO_INCREMENT reconciliation verdict for one table.
type tableState struct {
	ddl    string // alignment statement ("" when aligned or not applicable)
	srcVal int64
	dstVal int64
	srcHas bool // source reports a next value (has an auto-increment column)
	dstHas bool
	// aligned is the verdict: the source has no auto-increment column, or
	// both sides report the same next value.
	aligned bool
	// note explains a divergence no ALTER can fix (the destination's
	// counter is above the source's — a counter can only be raised):
	// the state is reported, never planned.
	note string
}

// aiFacts is one side's AUTO_INCREMENT state for one table, best-effort:
// a failed query (a backend without the column) degrades to readable=false
// with a one-shot warning.
type aiFacts struct {
	table    string
	value    int64 // the reported next value
	maxPlus  int64 // max(column)+1; 0 when the maximum is unreadable
	present  bool  // the side reports a next value (has an auto-increment column)
	readable bool  // the query itself succeeded
}

// tableAutoIncFacts reads one side's next AUTO_INCREMENT value plus the
// data maximum, best-effort.
func (r *Runner) tableAutoIncFacts(ctx context.Context, side *conn.Side, table string) aiFacts {
	var facts aiFacts
	err := side.WithControl(ctx, func(q conn.Queryer) error {
		var err error
		var v, mp int64
		var present bool
		v, mp, present, err = conn.TableAutoIncrementFacts(ctx, q, table)
		facts = aiFacts{table: table, value: v, maxPlus: mp, present: present, readable: true}
		return err
	})
	if err != nil {
		r.aiWarned.Do(func() {
			fmt.Fprintf(os.Stderr, "warn: cannot read the AUTO_INCREMENT state on %s (%v); table-state reconciliation is skipped\n", side.Name, err)
		})
		return aiFacts{table: table}
	}
	return facts
}

// tableAutoInc reads one side's next AUTO_INCREMENT value (the thin
// reader createPlanFor uses for a CREATE TABLE's AUTO_INCREMENT= clause).
func (r *Runner) tableAutoInc(ctx context.Context, side *conn.Side, table string) (int64, bool) {
	f := r.tableAutoIncFacts(ctx, side, table)
	return f.value, f.present
}

// stateValues reads both sides' next AUTO_INCREMENT values. It returns
// ok=false (state reconciliation is skipped for this table) under --where
// (a table-level state has no filtered meaning), when either side cannot
// read the state, when the source has no auto-increment column (no state
// to converge — no ALTER is ever emitted, per the convergence contract),
// or when a side's value is not an exact next value (an allocator, or the
// per-table gap heuristic on an unknown backend — see sideStateExact).
func (r *Runner) stateValues(ctx context.Context, table string) (srcVal, dstVal int64, ok bool) {
	if r.o.Cmp.Where != "" {
		return 0, 0, false
	}
	srcF := r.tableAutoIncFacts(ctx, r.Src, table)
	if !srcF.readable || !srcF.present {
		return 0, 0, false
	}
	dstF := r.tableAutoIncFacts(ctx, r.Dst, table)
	if !dstF.readable {
		return 0, 0, false
	}
	if !r.sideStateExact(ctx, r.Src, srcF) || !r.sideStateExact(ctx, r.Dst, dstF) {
		return 0, 0, false
	}
	return srcF.value, dstF.value, true
}

// checkState reads both sides' next AUTO_INCREMENT values and decides
// whether the destination's table state drifted and what realigns it. It
// is a no-op under --where (a table-level state has no filtered meaning),
// on a side whose state is not an exact next value (see stateValues), and
// when the source has no auto-increment column (information_schema
// reports NULL — in that case no ALTER is ever emitted, per the
// convergence contract). A destination without the column cannot be set:
// no DDL, and the state is reported unaligned (a structure drift the
// structure pre-step must repair first, or an explicit --no-sync-schema).
// When the destination's counter is ABOVE the source's, no DDL is
// planned either: an auto-increment counter can only be raised, and a
// full resync is the only thing that realigns it — the divergence is
// reported (note) and the run exits non-zero. A backend that cannot read
// the state degrades to (no DDL, aligned): the reconciliation is skipped
// with a one-shot warning, never a failed run.
func (r *Runner) checkState(ctx context.Context, table string) tableState {
	srcVal, dstVal, ok := r.stateValues(ctx, table)
	if !ok {
		return tableState{}
	}
	aligned := dstVal == srcVal
	st := tableState{srcVal: srcVal, dstVal: dstVal, srcHas: true, dstHas: true, aligned: aligned}
	if !aligned {
		if dstVal < srcVal {
			st.ddl = fmt.Sprintf("ALTER TABLE %s AUTO_INCREMENT = %d", conn.QuoteIdent(table), srcVal)
		} else {
			st.note = fmt.Sprintf("destination's next AUTO_INCREMENT (%d) is above the source's (%d): the counter cannot be lowered, a full resync realigns it",
				dstVal, srcVal)
		}
	}
	return st
}

// VerifyState re-checks the table state after the sync: OK when the
// source has no auto-increment column or both sides agree ("" when out
// of scope: --where, a state the backend cannot read, or a value that is
// not an exact next value — out of scope, never a failure).
func (r *Runner) VerifyState(ctx context.Context, table string) string {
	if r.o.Cmp.Where != "" {
		return ""
	}
	srcF := r.tableAutoIncFacts(ctx, r.Src, table)
	if !srcF.readable || !srcF.present {
		return "OK"
	}
	dstF := r.tableAutoIncFacts(ctx, r.Dst, table)
	if !dstF.readable {
		return ""
	}
	if !r.sideStateExact(ctx, r.Src, srcF) || !r.sideStateExact(ctx, r.Dst, dstF) {
		return ""
	}
	if dstF.value == srcF.value {
		return "OK"
	}
	return "DIFFERENT"
}

// missingOnDst reports whether the table exists on the source but not on
// the destination: the pre-pass erred because the destination copy is
// absent, not because of a schema mismatch or a runtime problem.
func (r *Runner) missingOnDst(ctx context.Context, table string) bool {
	var dstOK bool
	if err := r.Dst.WithControl(ctx, func(q conn.Queryer) error {
		var err error
		dstOK, err = conn.TableExists(ctx, q, table)
		return err
	}); err != nil || dstOK {
		return false
	}
	var srcOK bool
	if err := r.Src.WithControl(ctx, func(q conn.Queryer) error {
		var err error
		srcOK, err = conn.TableExists(ctx, q, table)
		return err
	}); err != nil {
		return false
	}
	return srcOK
}

// createPlanFor builds the plan for a table the destination is missing:
// a CREATE TABLE rendered from the source's structure (columns in order,
// primary key, unique indexes, engine, and the source's current next
// AUTO_INCREMENT value so the new table starts on the right counter),
// followed by the data sync the table needs once it exists (an empty
// destination: every source row is an INSERT — row-level when the source
// offers a usable key, a plain full load otherwise).
func (r *Runner) createPlanFor(ctx context.Context, table string) (*StructPlan, bool, error) {
	var srcS *conn.Struct
	if err := r.Src.WithControl(ctx, func(q conn.Queryer) error {
		var err error
		srcS, err = conn.IntrospectStructure(ctx, q, table)
		return err
	}); err != nil {
		return nil, false, err
	}
	srcS = filterStruct(srcS, r.o.Cmp.Normalize.IgnoreCols)
	if g := generatedCols(srcS); len(g) > 0 {
		// P0-2: a generated column's expression is a cross-backend promise
		// (other columns, server functions) that CREATE TABLE cannot
		// reproduce faithfully — refuse instead of emitting a structure
		// that silently lacks it.
		return nil, false, fmt.Errorf("table %s has generated column(s) (%s) that mtdiff cannot reproduce; align the schema manually or use --no-sync-schema",
			table, strings.Join(g, ", "))
	}
	autoInc, hasAuto := r.tableAutoInc(ctx, r.Src, table)
	var engine string
	if err := r.Src.WithControl(ctx, func(q conn.Queryer) error {
		var err error
		engine, err = conn.TableEngine(ctx, q, table)
		return err
	}); err != nil {
		return nil, false, err
	}
	keyed := len(r.o.Cmp.Key) > 0 || UsableKeyOf(srcS) != nil
	sp := &StructPlan{
		DDL:     []string{RenderCreateTable(table, srcS, engine, autoInc, hasAuto)},
		Reasons: []string{"table missing on destination: create"},
	}
	return sp, keyed, nil
}

// applyState realigns the destination's table state (next AUTO_INCREMENT)
// and records the verdict on ts. A failure marks the table FAILED: the
// rows may have converged but the table state has not, and the next run
// re-plans from the current facts and closes the gap.
func (r *Runner) applyState(ctx context.Context, table string, ap *Applier, ts *TableSync) {
	st := r.checkState(ctx, table)
	if st.ddl == "" {
		// no statement: an unfixable divergence (the destination's
		// counter moved above the source's, e.g. by a concurrent
		// explicit-id insert) is still reported on the table
		if st.note != "" {
			ts.StateNote = st.note
		}
		return
	}
	if err := ap.execDirect(ctx, st.ddl); err != nil {
		ts.Status, ts.Error = "FAILED", fmt.Sprintf("table state: %v", err)
		return
	}
	ts.StateChanged = true
	ts.StateSQL = []string{st.ddl}
	ts.StateNote = ""
}

// Verify re-runs the comparison (post-sync check).
func (r *Runner) Verify(ctx context.Context, tables []string) ([]compare.TableResult, error) {
	return r.cmp.Compare(ctx, r.Src, r.Dst, tables)
}

// Count returns the (filtered) row count of one table on one side.
func (r *Runner) Count(ctx context.Context, side *conn.Side, table string) (int64, error) {
	q := fmt.Sprintf("SELECT COUNT(*) FROM %s", conn.QuoteIdent(table))
	if r.o.Cmp.Where != "" {
		q += " WHERE (" + r.o.Cmp.Where + ")"
	}
	var n int64
	err := side.WithControl(ctx, func(qq conn.Queryer) error {
		var err error
		n, err = countOnQuery(ctx, qq, q)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", side.Name, table, err)
	}
	return n, nil
}

// countOnQuery runs a COUNT(*) on a Queryer (the control session in the
// production path; the snapshot connection in snapshot mode).
func countOnQuery(ctx context.Context, q conn.Queryer, query string, args ...any) (int64, error) {
	var n int64
	if err := conn.OneRow(ctx, q, query, []any{&n}, args...); err != nil {
		return 0, err
	}
	return n, nil
}

// prep is the per-table context: prepared schemas, statement builder and
// row engine, with the chosen plan.
type prep struct {
	srcS, dstS *conn.Schema
	plan       Plan
	b          *Builder
	e          *Engine
}

// keyColsOf extracts a schema's key columns (in key order).
func keyColsOf(s *conn.Schema) []conn.Column {
	byName := make(map[string]conn.Column, len(s.Cols))
	for _, c := range s.Cols {
		byName[c.Name] = c
	}
	out := make([]conn.Column, 0, len(s.Key))
	for _, k := range s.Key {
		if c, ok := byName[k]; ok {
			out = append(out, c)
		}
	}
	return out
}

// prepare runs the same schema preparation as the comparison (so the sync
// sees exactly what the comparison saw) and computes the plan. It rejects
// an --ignore-columns entry naming a key column: the row operations address
// rows by key, and ignoring a key column would silently break targeting.
func (r *Runner) prepare(ctx context.Context, res compare.TableResult) (*prep, error) {
	srcS, dstS, _, err := compare.PrepareSchemas(ctx, r.Src, r.Dst, res.Name, r.o.Cmp.Key, r.o.Cmp.Normalize.IgnoreCols, r.o.Cmp.Compat)
	if err != nil {
		return nil, err
	}
	if r.o.Cmp.Normalize.IgnoreCols != nil {
		for _, k := range srcS.Key {
			if r.o.Cmp.Normalize.IgnoreCols[k] {
				return nil, fmt.Errorf("%w: table %s: --ignore-columns names key column %q, which the sync needs for row targeting",
					ErrMisconfigured, res.Name, k)
			}
		}
	}
	// The engine's row addressing is safe only when the key addresses a
	// SINGLE row on BOTH sides (an UPDATE by a key that matches several
	// rows would rewrite all of them): the engine is "unique" when both
	// sides agree (a non-unique key replaces whole key groups instead of
	// updating single rows). DecidePlan gets the per-side facts so it can
	// also reject the unsafe --where combinations.
	engineUnique := srcS.KeyIsUnique && dstS.KeyIsUnique
	// Ignoring a unique-constraint column is rejected like ignoring a
	// key column: the swap protection cannot watch a column the
	// comparison drops, and a unique value would move unseen (P1-5).
	if r.o.Cmp.Normalize.IgnoreCols != nil {
		for _, s := range []*conn.Schema{srcS, dstS} {
			for _, u := range s.UniqueConstraints {
				for _, n := range u.Cols {
					if r.o.Cmp.Normalize.IgnoreCols[n] {
						return nil, fmt.Errorf("%w: table %s: --ignore-columns names unique column %q (constraint %s): the unique-swap protection needs it, so the sync cannot be proven safe",
							ErrMisconfigured, res.Name, n, u.Name)
					}
				}
			}
		}
	}
	p := &prep{
		srcS: srcS,
		dstS: dstS,
		plan: DecidePlan(res, len(srcS.Key) > 0, len(dstS.Key) > 0, srcS.KeyIsUnique, dstS.KeyIsUnique,
			strings.Join(srcS.Key, ","), r.o.Cmp.Where, keyAgree(srcS, dstS)),
		b: NewBuilder(res.Name, srcS),
	}
	p.e = NewEngine(
		normalize.NewNormalizer(srcS.Cols, r.o.Cmp.Normalize),
		normalize.NewNormalizer(dstS.Cols, r.o.Cmp.Normalize),
		normalize.NewNormalizer(keyColsOf(srcS), r.o.Cmp.Normalize),
		normalize.NewNormalizer(keyColsOf(dstS), r.o.Cmp.Normalize),
		engineUnique, keyColsOf(srcS), srcS.Cols, dstS.Cols, srcS.UniqueConstraints, dstS.UniqueConstraints)
	return p, nil
}

// rowWorkKind selects the execution path of the row-level work.
type rowWorkKind int

const (
	rowWorkNone      rowWorkKind = iota // nothing to do
	rowWorkChunks                       // the ordinary path: the differing chunks' ops
	rowWorkDstDelete                    // empty source match set: stream-delete the destination's
	// match set, chunk by chunk
)

// rowWork is the READ-ONLY plan of one table's row-level work: what the
// preflight shows the user (and what the confirmation covers) and what
// the apply re-plans through the very same function before writing.
//
// Memory model: the ordinary path buffers ONLY the differing chunks
// (O(delta) — a table with a few changed chunks holds just those rows).
// The two large-delete paths buffer nothing per row: an empty-source
// match set is streamed away chunk by chunk (O(chunk)), and the
// out-of-range rows are streamed away by keyset pagination (O(batch)).
// The one residual is a fully diverged table, where the differing
// chunks are the whole table and the ops buffer every row — the
// documented maximum of the ordinary path.
type rowWork struct {
	kind rowWorkKind
	// rowWorkChunks: the differing chunks' ops, in chunk order (the
	// applier commits one transaction per chunk group).
	ops [][]op
	// dstDel is the destination match-set row count (rowWorkDstDelete):
	// the executor streams the deletes in chunk/batch-sized
	// transactions — the count comes from the COUNT, not a key scan.
	dstDel int64
	// oorDel is the destination row count strictly OUTSIDE the source's
	// key range (a COUNT, not a scan): the executor streams them away
	// in batch-sized transactions BEFORE any in-range write.
	oorDel int64
	// rewrites is the number of destructive row-rewrite groups the plan
	// shows (0 when the plan rewrites nothing).
	rewrites int
	// rewriteFPs is the IDENTITY of those groups (one fingerprint each,
	// see fingerprint.go): the confirmed scope carries this set, and the
	// apply-time re-plan may run a SUBSET of it and nothing else (P0).
	rewriteFPs []string
	// maxChunkOps is the largest single chunk group's op count (the
	// ordinary path's per-group memory peak, O(chunk delta)).
	maxChunkOps int
}

// PlanRowWork computes the READ-ONLY row-level plan for a planned
// table: it re-plans on the source (with the freshly recounted source
// row count, not the pre-pass one — a source that grew in between must
// not silently turn the re-plan into a handful of over-sized chunks)
// and buffers both sides of the planned chunks only (the pre-pass
// already proved the matching chunks identical). When the pre-pass
// planned no chunks the differing chunks are unknown and every chunk is
// rescaned.
//
// It returns the work to execute (ApplyRowWork) without writing
// anything: the preflight calls it for the confirmation summary, and
// the apply calls it right before writing — the same plan, so the
// destructive scope the user confirmed is the one the apply checks
// against (P0-3).
//
// When the source re-plans to no chunk but the destination still has
// rows (only possible with a --where filter: zero source matches, N
// destination matches), the work is rowWorkDstDelete: the executor
// stream-deletes the destination's match set chunk by chunk (a
// filtered table cannot be truncated, so its only path to
// convergence is emptying the match set).
//
// Rows whose key falls strictly outside the source's key range are
// never covered by the in-range chunks; the plan counts them (COUNT,
// not a scan) and the executor stream-deletes them first, in
// batch-sized transactions. Without a filter this keeps the first
// round from escalating to a full resync; with a filter it is the
// only path to convergence for out-of-range rows.
func (r *Runner) PlanRowWork(ctx context.Context, p *prep, res compare.TableResult, freshSrc, freshDst int64) (rowWork, error) {
	planner := chunk.Planner{
		Table:       res.Name,
		KeyCols:     p.srcS.Key,
		KeyFamilies: compare.KeyFamilies(p.srcS),
		ChunkSize:   r.o.Cmp.ChunkSize,
		Where:       r.o.Cmp.Where,
	}
	var chunks []chunk.Chunk
	if err := r.Src.WithControl(ctx, func(q conn.Queryer) error {
		var err error
		chunks, err = planner.Plan(ctx, q, freshSrc)
		return err
	}); err != nil {
		return rowWork{}, fmt.Errorf("re-plan: %w", err)
	}
	if len(chunks) == 0 {
		if freshDst <= 0 {
			return rowWork{kind: rowWorkNone}, nil
		}
		// the destination match set is the whole work. The executor
		// streams it (COUNT + chunk-sized key scans + batched
		// DELETEs); the plan scans no keys at all — a 100M-row match
		// set is not scanned for a count.
		return rowWork{kind: rowWorkDstDelete, dstDel: freshDst}, nil
	}
	byID := make(map[int]chunk.Chunk, len(chunks))
	for _, ch := range chunks {
		byID[ch.ID] = ch
	}
	targets := make([]chunk.Chunk, 0, len(chunks))
	if len(p.plan.Chunks) > 0 {
		for _, id := range p.plan.Chunks {
			ch, ok := byID[id]
			if !ok {
				// the pre-pass saw a differing chunk that the re-plan no
				// longer produces: data moved between the two passes
				return rowWork{}, errEscalateFull
			}
			targets = append(targets, ch)
		}
	} else {
		targets = chunks
	}
	out := make([][]op, 0, len(targets))
	var maxChunkOps int
	// Unique-constraint verification (P0-2 / P1-5 / P1-6). In-chunk: a
	// swap, cycle or holder conflict needs the destructive row rewrite
	// (DELETE+INSERT), refused unless --allow-row-rewrite is set.
	// Cross-chunk: a unique value whose current holder sits in another
	// chunk cannot be ordered across the per-chunk commits; the check
	// resolves each foreign holder with targeted point queries (O(delta),
	// never O(table)) and, when it cannot prove the slot is freed,
	// refuses (without the flag) or escalates to the FULL resync (with
	// it — a filtered table can never be fully resynced, so there it
	// always refuses).
	filtered := r.o.Cmp.Where != ""
	// oorActive: the UNFILTERED out-of-range pass runs (keyed on both
	// sides, keys agreeing). It is the one server-side proof a
	// foreign out-of-range unique holder can be deleted before any
	// in-range write (see crossChunkCheck / classifyHolder).
	oorActive := !filtered && len(p.srcS.Key) > 0 && len(p.dstS.Key) > 0 && keyAgree(p.srcS, p.dstS)
	// The source's key extremes (--where applied): the cross-chunk
	// holder check's global range and the out-of-range pass's bounds,
	// read once.
	var loV, hiV []driver.Value
	oorPass := freshSrc > 0 && len(p.srcS.Key) > 0 && len(p.dstS.Key) > 0 && keyAgree(p.srcS, p.dstS)
	if len(p.e.uc) > 0 || oorPass {
		var minV, maxV []driver.Value
		if err := r.Src.WithControl(ctx, func(q conn.Queryer) error {
			var err error
			minV, maxV, err = planner.Extremes(ctx, q)
			return err
		}); err != nil {
			return rowWork{}, fmt.Errorf("key extremes: %w", err)
		}
		loV, hiV = minV, maxV
	}
	// The out-of-range row count (COUNT, not a scan): the executor
	// stream-deletes these rows FIRST (before any in-range write) — the
	// in-range chunks only cover the source's key span, yet an
	// out-of-range destination row can still block an in-range INSERT
	// (it can hold a unique value an in-range row takes). Deleting only
	// frees unique slots, so committing them before any in-range write
	// makes the sequence collision-free.
	var oorDel int64
	if oorPass && loV != nil && hiV != nil {
		var err error
		oorDel, err = r.countOOR(ctx, p, loV, hiV)
		if err != nil {
			return rowWork{}, fmt.Errorf("out-of-range count: %w", err)
		}
	}
	// The out-of-range flag the holders query carries per row (the
	// UNFILTERED pass only — a filtered pass cannot prove an unfiltered
	// holder is removed): the same parameterized predicate the delete
	// pass scans with, so a flag-marked holder is provably deleted
	// before any in-range write.
	var oorFlag chunk.Pred
	if len(p.e.uc) > 0 && oorActive && loV != nil && hiV != nil {
		oorFlag, _ = r.oorPredicate(p, loV, hiV)
	}
	// One scan connection per side for the whole table (P2-1): the
	// per-chunk scans reuse them instead of churning the pool, and a
	// connection that dies mid-scan is replaced (the pool re-initializes
	// it) with the chunk retried once.
	srcCn, err := r.Src.AcquireScan(ctx)
	if err != nil {
		return rowWork{}, err
	}
	defer func() { srcCn.Close() }()
	dstCn, err := r.Dst.AcquireScan(ctx)
	if err != nil {
		return rowWork{}, err
	}
	defer func() { dstCn.Close() }()
	for _, ch := range targets {
		srcM, err := p.e.scanSideConn(ctx, srcCn, p.srcS, p.e.srcRow, p.e.srcKey, ch, r.o.Cmp.Where)
		if err != nil && conn.DeadConn(err) {
			srcCn.Close()
			if srcCn, err = r.Src.AcquireScan(ctx); err != nil {
				return rowWork{}, fmt.Errorf("src chunk %d: re-acquire scan connection: %w", ch.ID, err)
			}
			srcM, err = p.e.scanSideConn(ctx, srcCn, p.srcS, p.e.srcRow, p.e.srcKey, ch, r.o.Cmp.Where)
		}
		if err != nil {
			return rowWork{}, fmt.Errorf("src chunk %d: %w", ch.ID, err)
		}
		dstM, err := p.e.scanSideConn(ctx, dstCn, p.dstS, p.e.dstRow, p.e.dstKey, ch, r.o.Cmp.Where)
		if err != nil && conn.DeadConn(err) {
			dstCn.Close()
			if dstCn, err = r.Dst.AcquireScan(ctx); err != nil {
				return rowWork{}, fmt.Errorf("dst chunk %d: re-acquire scan connection: %w", ch.ID, err)
			}
			dstM, err = p.e.scanSideConn(ctx, dstCn, p.dstS, p.e.dstRow, p.e.dstKey, ch, r.o.Cmp.Where)
		}
		if err != nil {
			return rowWork{}, fmt.Errorf("dst chunk %d: %w", ch.ID, err)
		}
		ops, rewrite := p.e.Diff(srcM, dstM)
		if rewrite && !r.o.AllowRowRewrite {
			return rowWork{}, fmt.Errorf("%w: table %s has a unique-value conflict (a swap, cycle or holder) that per-row writes cannot order; converging it requires a destructive row rewrite (DELETE+INSERT), which is disabled by default because FK/trigger side effects cannot be proven safe — re-run with --allow-row-rewrite to permit it",
				errUniqueRewriteRefused, res.Name)
		}
		if len(p.e.uc) > 0 {
			// on the pinned connections: at parallel=1 the pool holds no
			// second connection, a checkout here would self-deadlock
			v, err := p.e.crossChunkCheck(ctx, srcCn, dstCn, p.srcS, p.dstS, ch, dstM, ops, loV, hiV, oorFlag, oorActive)
			if err != nil {
				return rowWork{}, fmt.Errorf("unique holder check, chunk %d: %w", ch.ID, err)
			}
			switch v {
			case crossConflict:
				if !r.o.AllowRowRewrite {
					return rowWork{}, fmt.Errorf("%w: table %s swaps a unique value between rows of different chunks (chunk %d); no row-level order applies it safely. Re-run with --allow-row-rewrite to plan the order-independent full resync (shown in the plan and confirmed before it runs)",
						errUniqueRewriteRefused, res.Name, ch.ID)
				}
				// With the flag the table's plan becomes the order-
				// independent FULL resync. The flag authorizes planning
				// it — not executing it blindly: the apply runs it only
				// when the FULL plan (with its TRUNCATE) is what the
				// user confirmed, otherwise it stops the table (P0-2).
				return rowWork{}, fmt.Errorf("table %s swaps a unique value between rows of different chunks (chunk %d): row-level writes cannot order this safely; the plan becomes the full resync (TRUNCATE + reload), which the apply executes only if the confirmed plan showed it: %w",
					res.Name, ch.ID, errEscalateFull)
			case crossDuplicate:
				return rowWork{}, fmt.Errorf("table %s: the source holds one unique value in two different rows; the destination's unique index cannot hold both — fix the source data before syncing", res.Name)
			}
		}
		out = append(out, ops)
		if len(ops) > maxChunkOps {
			maxChunkOps = len(ops)
		}
	}
	w := rowWork{kind: rowWorkChunks, ops: out, oorDel: oorDel, rewrites: rewriteCount(out), rewriteFPs: rewriteFingerprints(out), maxChunkOps: maxChunkOps}
	return w, nil
}

// keyDeleteBatches prepares one chunk's raw key rows for the batched
// deletes: it sorts them by normalized key identity (a small in-chunk
// sort, never a whole-range sort — rows that fold to one identity delete
// together) and cuts them into batches of at most cap. It is the entire
// per-chunk state of the stream-delete paths: the chunk's own keys plus
// one batch (O(chunk) / O(batch) — never the table).
func keyDeleteBatches(rows [][]any, canon func(vals []any) (string, error), cap int) ([][][]any, error) {
	if cap < 1 {
		cap = 1
	}
	type keyed struct {
		canon string
		kv    []any
	}
	kr := make([]keyed, len(rows))
	for i, vals := range rows {
		c, err := canon(vals)
		if err != nil {
			return nil, err
		}
		kr[i] = keyed{canon: c, kv: vals}
	}
	sort.Slice(kr, func(i, j int) bool { return kr[i].canon < kr[j].canon })
	out := make([][][]any, 0, (len(rows)+cap-1)/cap)
	for start := 0; start < len(kr); start += cap {
		end := start + cap
		if end > len(kr) {
			end = len(kr)
		}
		batch := make([][]any, 0, end-start)
		for _, k := range kr[start:end] {
			batch = append(batch, k.kv)
		}
		out = append(out, batch)
	}
	return out, nil
}

// streamKeyDeletes deletes a side's key match set in chunk-sized,
// batch-sized transactions: plan the side, scan ONE chunk of key
// columns, batch-DELETE it (DeleteBatchExec), commit, release, next
// chunk. Peak memory is one chunk of key vectors plus one delete batch
// (O(chunk), independent of the match set's size): an empty-source
// match set over a 100M-row table streams, it is never buffered.
//
// The chunk's keys are sorted by normalized key identity before
// batching (a small in-chunk sort, never a whole-range sort), so rows
// that fold to one normalized identity delete as one batch.
func (r *Runner) streamKeyDeletes(ctx context.Context, p *prep, side *conn.Side, s *conn.Schema, total int64, ap *Applier, st *Stats) error {
	planner := chunk.Planner{
		Table:       s.Table,
		KeyCols:     s.Key,
		KeyFamilies: compare.KeyFamilies(s),
		ChunkSize:   r.o.Cmp.ChunkSize,
		Where:       r.o.Cmp.Where,
	}
	var chunks []chunk.Chunk
	if err := side.WithControl(ctx, func(q conn.Queryer) error {
		var err error
		chunks, err = planner.Plan(ctx, q, total)
		return err
	}); err != nil {
		return fmt.Errorf("re-plan %s: %w", side.Name, err)
	}
	b, cap := r.deleteBatch(p)
	if cap <= 0 {
		return nil // keyless: the caller never plans this path without a key
	}
	step := len(chunks) / 10
	if step < 1 {
		step = 1
	}
	buf := make([]byte, 0, 256)
	canon := func(vals []any) (string, error) {
		c, err := p.e.dstKey.Normalize(vals, buf)
		if err != nil {
			return "", err
		}
		buf = c[:0]
		return string(c), nil
	}
	for i, ch := range chunks {
		rows, err := p.e.scanKeyRows(ctx, side, s, ch.Pred(s.Key, ""), r.o.Cmp.Where)
		if err != nil {
			return fmt.Errorf("%s chunk %d: %w", side.Name, ch.ID, err)
		}
		// one transaction per batch of key vectors: a mid-stream failure
		// rolls back that batch only, the completed batches stay, and a
		// re-run converges (the deletes are key-addressed and
		// idempotent). The canon buffer is reused across rows (the
		// sorted keys are string copies).
		batches, err := keyDeleteBatches(rows, canon, cap)
		if err != nil {
			return fmt.Errorf("%s chunk %d: %w", side.Name, ch.ID, err)
		}
		for _, cur := range batches {
			if len(cur) > st.MaxBufferedDeleteKeys {
				st.MaxBufferedDeleteKeys = len(cur)
			}
			if err := ap.applyTx(ctx, func(tx *sql.Tx) error {
				stmt, args, err := b.DeleteBatchExec(cur)
				if err != nil {
					return err
				}
				n, err := tx.ExecContext(ctx, stmt, args...)
				if err != nil {
					return err
				}
				na, _ := n.RowsAffected()
				st.Deletes += int(na)
				return nil
			}); err != nil {
				return fmt.Errorf("%s chunk %d: %w", side.Name, ch.ID, err)
			}
		}
		st.Chunks++
		if r.o.Progress != nil && (i+1)%step == 0 {
			r.o.Progress("  %-24s stream-delete %3d%% (%d/%d chunks, %d rows deleted)", s.Table, 100*(i+1)/len(chunks), i+1, len(chunks), st.Deletes)
		}
	}
	return nil
}

// allNil reports whether every value is nil (the all-NULL key row).
func allNil(v []driver.Value) bool {
	for _, x := range v {
		if x != nil {
			return false
		}
	}
	return true
}

// keyAgree reports whether both sides use the same key: same columns, in
// the same order, by name. (Column drift with different names but
// compatible types passes the pre-pass compatibility check positionally,
// so only the structure pre-step or this check can catch it.)
func keyAgree(a, b *conn.Schema) bool {
	if len(a.Key) != len(b.Key) {
		return false
	}
	for i := range a.Key {
		if a.Key[i] != b.Key[i] {
			return false
		}
	}
	return true
}

// oorPredicate renders the "key strictly outside [min, max]" predicate for
// the destination's key columns, bounded by the source's minimum and
// maximum key values: LessThan(min) OR GreaterThan(max), parameterized
// (P0-1) — the bound values are the SOURCE's raw key values and are bound
// as data on the server, never rendered into the SQL text. The
// comparison is strict on purpose — a row equal to a bound is in range and
// belongs to the chunk diff. A side that renders empty is dropped; when
// both do (a single-column all-NULL minimum bounds everything below) ok is
// false.
func (r *Runner) oorPredicate(p *prep, minV, maxV []driver.Value) (chunk.Pred, bool) {
	// a composite key's strict comparison joins its column terms with a
	// top-level OR; wrap it when the two tails are ORed together (a single
	// term, or a fully parenthesized group like "(`a` IS NULL OR ...)",
	// is safe to OR as-is)
	wrap := func(s string) string {
		if strings.Contains(s, " OR ") && !isParenGroup(s) {
			return "(" + s + ")"
		}
		return s
	}
	var sides []string
	var args []any
	if !allNil(minV) {
		// the all-NULL row is the minimum: nothing sits below it
		lt := chunk.LessThan(p.dstS.Key, minV)
		if lt.SQL != "1=0" {
			sides = append(sides, wrap(lt.SQL))
			args = append(args, lt.Args...)
		}
	}
	gt := chunk.GreaterThan(p.dstS.Key, maxV)
	if gt.SQL != "1=0" {
		sides = append(sides, wrap(gt.SQL))
		args = append(args, gt.Args...)
	}
	if len(sides) == 0 {
		return chunk.Pred{}, false
	}
	return chunk.Pred{SQL: strings.Join(sides, " OR "), Args: args}, true
}

// isParenGroup reports whether the whole string is one parenthesized group
// (an unbalanced or trailing-paren string is not).
func isParenGroup(s string) bool {
	if len(s) < 2 || s[0] != '(' || s[len(s)-1] != ')' {
		return false
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i == len(s)-1
			}
		}
	}
	return false
}

// deleteBatch is the statement builder and the per-batch key limit for
// the streaming delete paths (P2): the configured batch, further capped
// by the bind-parameter budget (one placeholder per key component — a
// wide composite key shrinks its batch automatically).
func (r *Runner) deleteBatch(p *prep) (*Builder, int) {
	if len(p.dstS.Key) == 0 {
		return nil, 0
	}
	return p.b, deleteBatchCap(r.o.Batch, len(p.dstS.Key))
}

// countOOR counts the destination rows whose key falls strictly outside
// the source's key range (a COUNT, not a scan): the preflight reports it
// without reading a single key — a 100M-row destination against a
// 50M..60M source span reports ~40M out-of-range rows from one COUNT.
// The --where filter applies (a filtered run deletes only the out-of-
// range rows the filter matches).
func (r *Runner) countOOR(ctx context.Context, p *prep, minV, maxV []driver.Value) (int64, error) {
	pred, ok := r.oorPredicate(p, minV, maxV)
	if !ok {
		return 0, nil
	}
	q := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE (%s)",
		conn.QuoteIdent(p.dstS.Table), pred.SQL)
	if r.o.Cmp.Where != "" {
		q += " AND (" + r.o.Cmp.Where + ")"
	}
	var n int64
	if err := r.Dst.WithControl(ctx, func(qq conn.Queryer) error {
		var err error
		n, err = countOnQuery(ctx, qq, q, pred.Args...)
		return err
	}); err != nil {
		return 0, fmt.Errorf("dst %s: %w", p.dstS.Table, err)
	}
	return n, nil
}

// keySamples reads up to limit key rows from one side (ORDER BY the key,
// LIMIT limit — a bounded sample, never a full scan): the dry run shows
// what the stream deletes will address without reading the table.
// Rendered as literals for DISPLAY only (read-side; the deletes bind).
func (r *Runner) keySamples(ctx context.Context, side *conn.Side, s *conn.Schema, limit int) ([]string, error) {
	if limit <= 0 || len(s.Key) == 0 {
		return nil, nil
	}
	idents := make([]string, len(s.Key))
	fams := make([]string, len(s.Key))
	pos := make(map[string]int, len(s.Cols))
	for i, c := range s.Cols {
		pos[strings.ToLower(c.Name)] = i
	}
	for i, k := range s.Key {
		idents[i] = conn.QuoteIdent(k)
		fams[i] = s.Cols[pos[strings.ToLower(k)]].Family
	}
	q := fmt.Sprintf("SELECT %s FROM %s", strings.Join(idents, ", "), conn.QuoteIdent(s.Table))
	if r.o.Cmp.Where != "" {
		q += " WHERE (" + r.o.Cmp.Where + ")"
	}
	q += " ORDER BY " + strings.Join(idents, ", ") + fmt.Sprintf(" LIMIT %d", limit)
	var out []string
	err := side.WithControl(ctx, func(qq conn.Queryer) error {
		rows, err := qq.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		vals := make([]driver.Value, len(s.Key))
		ptrs := make([]any, len(s.Key))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			for i := range vals {
				vals[i] = nil
			}
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			parts := make([]string, len(vals))
			for i, v := range vals {
				parts[i] = literalFor(fams[i], v)
			}
			out = append(out, "("+strings.Join(parts, ", ")+")")
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// keysetCursor renders the strict "key > cursor" predicate with BINDABLE
// arguments (bindArg per key-column family — a BIT or an unsigned-BIGINT
// cursor binds the way it scanned, never as a raw byte blob): the
// keyset pagination's advance term (page n+1 is "key > last row of page
// n"; strict, so the cursor row itself is never re-read). The shape
// mirrors the chunk package's strictCompare "greater" expansion; NULL
// components (impossible for a usable key — primary key or NOT NULL
// unique — kept for completeness) take the same terms.
func (r *Runner) keysetCursor(p *prep, cursor []any) (string, []any) {
	keyCols := p.dstS.Key
	fams := make([]string, len(keyCols))
	pos := make(map[string]int, len(p.dstS.Cols))
	for i, c := range p.dstS.Cols {
		pos[strings.ToLower(c.Name)] = i
	}
	for i, k := range keyCols {
		fams[i] = p.dstS.Cols[pos[strings.ToLower(k)]].Family
	}
	var term func(i int) (string, []any)
	term = func(i int) (string, []any) {
		col := conn.QuoteIdent(keyCols[i])
		v := cursor[i]
		last := i == len(cursor)-1
		if v == nil {
			switch {
			case last:
				// the all-NULL (prefix) row is the cursor itself: every
				// non-NULL row sits above it
				return col + " IS NOT NULL", nil
			default:
				s, a := term(i + 1)
				return fmt.Sprintf("(%s IS NULL AND %s) OR %s IS NOT NULL", col, s, col), a
			}
		}
		b := bindArg(fams[i], v)
		if last {
			return fmt.Sprintf("%s > ?", col), []any{b}
		}
		s, a := term(i + 1)
		return fmt.Sprintf("%s > ? OR (%s = ? AND %s)", col, col, s), append([]any{b, b}, a...)
	}
	return term(0)
}

// streamOORTail walks one out-of-range key tail (base is "key < srcMin"
// or "key > srcMax", already parameterized) with KEYSET PAGINATION: page
// n+1 adds "key > <last row of page n>" — no OFFSET (a deep OFFSET scan
// re-reads the whole prefix per page), no whole-range in-memory sort
// (the ORDER BY is an index walk the server does). The page IS the
// delete batch: scan a page, batch-DELETE it in one transaction,
// commit, advance, release. Peak memory is one page of key vectors
// (O(batch)): a 90M-row tail streams, it is never buffered.
//
// Deleting while walking is safe: the cursor only moves forward (key >
// cursor), the deleted rows are exactly the ones already walked, and a
// row ahead of the cursor cannot move behind it (its key is its
// identity). A mid-stream failure rolls back the page's transaction
// only; the completed pages stay, and a re-run converges the rest (the
// deletes are key-addressed and idempotent).
func (r *Runner) streamOORTail(ctx context.Context, p *prep, base chunk.Pred, ap *Applier, st *Stats) error {
	b, cap := r.deleteBatch(p)
	if cap <= 0 {
		return nil
	}
	cn, err := r.Dst.AcquireScan(ctx)
	if err != nil {
		return err
	}
	defer cn.Close()
	idents := make([]string, len(p.dstS.Key))
	for i, k := range p.dstS.Key {
		idents[i] = conn.QuoteIdent(k)
	}
	var cursor []any
	for {
		conds := []string{"(" + base.SQL + ")"}
		args := append([]any{}, base.Args...)
		if cursor != nil {
			cur, cargs := r.keysetCursor(p, cursor)
			conds = append(conds, "("+cur+")")
			args = append(args, cargs...)
		}
		if r.o.Cmp.Where != "" {
			conds = append(conds, "("+r.o.Cmp.Where+")")
		}
		q := fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY %s LIMIT ?",
			strings.Join(idents, ", "), conn.QuoteIdent(p.dstS.Table), strings.Join(conds, " AND "), strings.Join(idents, ", "))
		args = append(args, cap)
		rows, err := cn.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		vals := make([]any, len(p.dstS.Key))
		ptrs := make([]any, len(p.dstS.Key))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		var keys [][]any
		for rows.Next() {
			for i := range vals {
				vals[i] = nil
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return err
			}
			cp := make([]any, len(vals))
			copy(cp, vals)
			keys = append(keys, cp)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(keys) == 0 {
			break
		}
		if len(keys) > st.MaxBufferedDeleteKeys {
			st.MaxBufferedDeleteKeys = len(keys)
		}
		page := keys
		if err := ap.applyTx(ctx, func(tx *sql.Tx) error {
			stmt, bargs, err := b.DeleteBatchExec(page)
			if err != nil {
				return err
			}
			n, err := tx.ExecContext(ctx, stmt, bargs...)
			if err != nil {
				return err
			}
			na, _ := n.RowsAffected()
			st.Deletes += int(na)
			return nil
		}); err != nil {
			return err
		}
		if len(keys) < cap {
			break
		}
		last := keys[len(keys)-1]
		cursor = make([]any, len(last))
		copy(cursor, last)
	}
	return nil
}

// streamOORDeletes deletes the destination rows whose key falls strictly
// outside the source's key range: the lower tail (key < srcMin, walked
// ascending from the smallest key) and the upper tail (key > srcMax),
// each keyset-paginated (streamOORTail). It is a no-op for a keyless
// table, a key the two sides disagree on (the bounds are the SOURCE's
// key values rendered against the DESTINATION's key columns — with
// --no-sync-schema the keys can drift, e.g. PK (a, b) vs (b, a), and
// rendering them against the wrong columns would delete in-range rows),
// and a source that emptied (nil extremes).
//
// With a --where filter, out-of-range rows that do not match the filter
// are left in place: a filtered table cannot be truncated, so they are
// the one documented residual (the verification reports the table
// DIFFERENT and a plain, unfiltered comparison shows them).
func (r *Runner) streamOORDeletes(ctx context.Context, p *prep, minV, maxV []driver.Value, ap *Applier, st *Stats) error {
	if len(p.dstS.Key) == 0 || minV == nil || maxV == nil || !keyAgree(p.srcS, p.dstS) {
		return nil
	}
	if !allNil(minV) {
		// the all-NULL row is the minimum: nothing sits below it
		lt := chunk.LessThan(p.dstS.Key, minV)
		if lt.SQL != "1=0" {
			if err := r.streamOORTail(ctx, p, lt, ap, st); err != nil {
				return err
			}
		}
	}
	gt := chunk.GreaterThan(p.dstS.Key, maxV)
	if gt.SQL != "1=0" {
		if err := r.streamOORTail(ctx, p, gt, ap, st); err != nil {
			return err
		}
	}
	return nil
}

// ApplyRowWork executes a planned row work (PlanRowWork) on the
// destination write connection, in the destructive-safe order:
//
//  1. the out-of-range deletes, streamed (keyset-paginated, batch
//     transactions) — they must commit BEFORE any in-range write: the
//     in-range chunks only cover the source's key span, yet an
//     out-of-range destination row can still block an in-range INSERT
//     (it can hold a unique value an in-range row takes); deletes only
//     free unique slots, so committing them first makes the sequence
//     collision-free. The extremes are re-read at apply time (the OOR
//     set is what it IS when the deletes run, not when the plan was
//     made);
//  2. the work itself: for an empty-source match set the stream-delete
//     of the destination's match set (chunk-sized transactions),
//     otherwise the differing chunks' ops (the applier's per-chunk
//     transactions).
//
// A mid-stream failure stops the work: the completed transactions stay
// (they converged), the failed one rolls back, and a re-run converges
// the rest — every statement is key-addressed and idempotent.
func (r *Runner) ApplyRowWork(ctx context.Context, p *prep, w rowWork, ap *Applier, st *Stats) {
	if w.kind == rowWorkNone {
		return
	}
	st.MaxBufferedOps = w.maxChunkOps
	if w.kind == rowWorkChunks {
		if w.oorDel > 0 {
			sp := chunk.Planner{
				Table:       p.srcS.Table,
				KeyCols:     p.srcS.Key,
				KeyFamilies: compare.KeyFamilies(p.srcS),
				Where:       r.o.Cmp.Where,
			}
			var minV, maxV []driver.Value
			if err := r.Src.WithControl(ctx, func(q conn.Queryer) error {
				var err error
				minV, maxV, err = sp.Extremes(ctx, q)
				return err
			}); err != nil {
				st.Error = fmt.Sprintf("key extremes: %v", err)
				return
			}
			if err := r.streamOORDeletes(ctx, p, minV, maxV, ap, st); err != nil {
				st.Error = fmt.Sprintf("out-of-range deletes: %v", err)
				return
			}
		}
		ap.ApplyOps(ctx, st, p.b, w.ops)
		return
	}
	// rowWorkDstDelete: the source match set is empty — stream-delete
	// the destination's (possibly filtered) match set, chunk by chunk.
	freshDst, err := r.Count(ctx, r.Dst, p.dstS.Table)
	if err != nil {
		st.Error = fmt.Sprintf("recount dst: %v", err)
		return
	}
	if freshDst <= 0 {
		return // it converged in the meantime
	}
	if err := r.streamKeyDeletes(ctx, p, r.Dst, p.dstS, freshDst, ap, st); err != nil {
		st.Error = fmt.Sprintf("stream delete: %v", err)
	}
}

// StructPlan is the structure-sync verdict for one table: nil (from
// SchemaPlanFor) means the structures already match or the sync is
// disabled; a non-nil plan carries the DDL that aligns the destination.
type StructPlan struct {
	DDL     []string
	Reasons []string // one per change, for the confirmation summary
}

// errWhereDrift is the (argument) error for a structure-drifted table under
// --where: aligning the structure requires the full-resync truncation, and
// a filtered table can never be truncated — no flag combination repairs
// it, so it is an argument error like keyless + --where.
const errWhereDrift = "structure drift on a --where-filtered table cannot be synced: align the destination structure manually, or re-run without --where"

// SchemaPlanFor plans the structure pre-step for one table (read-only): it
// introspects both sides, removes --ignore-columns entries from both, and
// diffs columns and primary/unique indexes. It returns (nil, srcS, nil)
// when the structure sync is disabled or the structures already match
// (srcS is the source structure, nil when it could not be read), (plan,
// srcS, nil) when DDL is needed, and (nil, nil, err) when the structure
// cannot be planned (a side's table missing, or a source definition that
// cannot be reproduced — an expression default). The source structure is
// returned because a repaired destination will have exactly this shape:
// the post-repair data strategy (row-level vs full) is decided from it.
// structurePlan computes the structure plan for one table from a FRESH
// introspection of both sides (nil plan when the structures already
// agree), plus the source structure. It is the re-plan seam (P1-2): a
// plan is valid only for the schema facts it was computed from — after
// any DDL failure the old plan is stale and must be recomputed, never
// replayed.
func (r *Runner) structurePlan(ctx context.Context, table string) (*StructPlan, *conn.Struct, error) {
	var srcS, dstS *conn.Struct
	if err := r.Src.WithControl(ctx, func(q conn.Queryer) error {
		var err error
		srcS, err = conn.IntrospectStructure(ctx, q, table)
		return err
	}); err != nil {
		return nil, nil, err
	}
	if err := r.Dst.WithControl(ctx, func(q conn.Queryer) error {
		var err error
		dstS, err = conn.IntrospectStructure(ctx, q, table)
		return err
	}); err != nil {
		return nil, nil, err
	}
	srcS = filterStruct(srcS, r.o.Cmp.Normalize.IgnoreCols)
	dstS = filterStruct(dstS, r.o.Cmp.Normalize.IgnoreCols)
	srcDef, dstDef := r.sideDefaultCollations(ctx)
	changes, err := DiffStructure(srcS, dstS, srcDef, dstDef)
	if err != nil {
		return nil, nil, err
	}
	if len(changes) == 0 {
		return nil, srcS, nil
	}
	reasons := make([]string, 0, len(changes))
	for _, ch := range changes {
		reasons = append(reasons, ch.Why)
	}
	return &StructPlan{DDL: RenderDDL(table, changes), Reasons: reasons}, srcS, nil
}

// SchemaPlanFor is the read-side (dry-run/verify) structure plan.
func (r *Runner) SchemaPlanFor(ctx context.Context, table string) (*StructPlan, *conn.Struct, error) {
	if !r.o.SyncSchema {
		return nil, nil, nil
	}
	return r.structurePlan(ctx, table)
}

// execDDL runs the structure DDL statements in order, returning the
// index of the statement that failed (-1 when all succeeded). A
// MULTI-STATEMENT DDL is NOT atomic: every statement before the failed
// one may already have been applied (only a single ALTER rolls back as a
// unit), so the caller must re-plan from a fresh introspection rather
// than replay the original plan (P1-2).
func execDDL(ctx context.Context, ap *Applier, ddl []string) (int, error) {
	for i, stmt := range ddl {
		if err := ap.execDirect(ctx, stmt); err != nil {
			return i, err
		}
	}
	return -1, nil
}

// ApplyStructureTable syncs a structure-drifted table (P1-3): apply the
// DDL IN PLACE on the destination — the data stays put. A SINGLE ALTER
// is atomic (a failed statement leaves the table exactly as it was), but
// a MULTI-STATEMENT plan is not: when statement N fails, statements
// before it may already have been applied (P1-2). The default on failure
// is to stop the table FAILED with its data preserved, saying exactly
// that; with --allow-structure-truncate the fallback is TRUNCATE, then
// RE-PLAN from a fresh introspection of both sides (the old plan is
// stale — replaying it would re-run the statements that already
// applied) and execute only the DDL still missing. Either way the data
// is then converged — see convergeAfterDDL for the re-read / re-plan:
// a key the repair restores (e.g. the primary key) puts the table back
// on row-level sync, only a still-keyless pair stays on the full load.
// The table state (AUTO_INCREMENT) is reconciled last; the caller
// verifies.
func (r *Runner) ApplyStructureTable(ctx context.Context, res compare.TableResult, sp *StructPlan, ap *Applier, scope DestructiveScope) TableSync {
	ts := TableSync{Name: res.Name, SchemaChanged: true, SchemaSQL: sp.DDL, DstRows: 0}
	if r.o.Progress != nil {
		r.o.Progress("%-24s structure: in-place ALTER, %d DDL statement(s): %s", res.Name, len(sp.DDL), strings.Join(sp.Reasons, "; "))
	}
	preTruncated := false
	idx, err := execDDL(ctx, ap, sp.DDL)
	if err != nil {
		if !r.o.AllowStructureTruncate {
			var msg string
			if len(sp.DDL) == 1 {
				// a single ALTER rolled back atomically: the schema is
				// unchanged, the data untouched
				msg = fmt.Sprintf(
					"structure sync: the in-place ALTER rolled back atomically (schema and destination data unchanged): %v — align the schema manually, or re-run with --allow-structure-truncate", err)
			} else {
				// a multi-statement DDL is NOT atomic: the statements
				// before the failed one may already have been applied
				msg = fmt.Sprintf(
					"structure sync: DDL statement %d of %d failed (%v); one or more prior DDL statements may already have been applied — destination data was not truncated; re-run (it re-plans from the current schema) or align the schema manually",
					idx+1, len(sp.DDL), err)
			}
			ts.Mode, ts.Status, ts.Error = "ERROR", "FAILED", msg
			return ts
		}
		if err2 := ap.execDirect(ctx, "TRUNCATE TABLE "+conn.QuoteIdent(res.Name)); err2 != nil {
			ts.Mode, ts.Status, ts.Error = "ERROR", "FAILED", fmt.Sprintf(
				"structure sync: in-place DDL failed (%v) and the truncate fallback failed (%v); destination data preserved", err, err2)
			return ts
		}
		ts.Truncated = true
		preTruncated = true
		// The failed pass may have left the structure half-migrated, so
		// the original plan is stale: re-introspect BOTH sides, re-diff,
		// and execute only the DDL that is still missing. Never replay
		// the original statements (an already-applied ADD COLUMN would
		// fail again on the empty table).
		sp2, _, err2 := r.structurePlan(ctx, res.Name)
		if err2 != nil {
			ts.Mode, ts.Status, ts.Error = "ERROR", "FAILED", fmt.Sprintf(
				"structure sync (after truncate): re-planning the remaining DDL failed: %v", err2)
			return ts
		}
		applied := append([]string{}, sp.DDL[:idx]...)
		if sp2 != nil {
			applied = append(applied, sp2.DDL...)
			if i2, err3 := execDDL(ctx, ap, sp2.DDL); err3 != nil {
				ts.Mode, ts.Status, ts.Error = "ERROR", "FAILED", fmt.Sprintf(
					"structure sync (after truncate): the re-planned DDL failed at statement %d of %d: %v", i2+1, len(sp2.DDL), err3)
				return ts
			}
		}
		ts.SchemaSQL = applied
	}
	return r.convergeAfterDDL(ctx, res.Name, "structure sync", ap, &ts, preTruncated, scope)
}

// convergeAfterDDL converges a table whose structure work (a CREATE, or
// the structure-sync DDL) has just been applied: it re-runs the
// comparison, re-reads the metadata and re-plans from the new structure
// (schema repair changes what the data sync can do — a restored key goes
// back to row-level sync, a still-keyless pair stays on the full load),
// writes the data, and reconciles the table state (the next
// AUTO_INCREMENT value; an ALTER rebuild may reset it, the state step
// closes the gap). preTruncated tells whether the destination is already
// empty (a fresh CREATE, or the truncate fallback) — a full load after an
// IN-PLACE ALTER must TRUNCATE itself, right before streaming the source
// (P1-3: the structure path truncates only for a confirmed full resync).
// The data may have converged while the table state has not: any failed
// step leaves the table FAILED and the next run re-plans from the current
// facts.
// scope is the confirmed destructive scope (from the preflight plan the
// user saw): the in-apply data path may TRUNCATE (the full-load
// fallback) or run row rewrites only within it (P0).
func (r *Runner) convergeAfterDDL(ctx context.Context, table, label string, ap *Applier, ts *TableSync, preTruncated bool, scope DestructiveScope) TableSync {
	fail := func(format string, args ...any) TableSync {
		ts.Mode, ts.Status, ts.Error = "ERROR", "FAILED", fmt.Sprintf(format, args...)
		return *ts
	}
	fresh, err := r.cmp.Compare(ctx, r.Src, r.Dst, []string{table})
	if err != nil {
		return fail("recompare after %s: %v", label, err)
	}
	fres := fresh[0]
	if fres.Status == "ERROR" {
		return fail("%s applied but recompare failed: %s", label, fres.Error)
	}
	if fres.Status == "OK" {
		// An empty source table: the work alone converged it.
		if ts.Mode != "CREATE" {
			ts.Mode = "SKIP"
		}
		ts.Status = "APPLIED"
		r.applyState(ctx, table, ap, ts)
		return *ts
	}
	p, err := r.prepare(ctx, fres)
	if err != nil {
		ts.ArgErr = errors.Is(err, ErrMisconfigured)
		return fail("%v", err)
	}
	if p.plan.Mode == ModeError {
		ts.ArgErr = p.plan.ArgErr
		return fail("%s", p.plan.Error)
	}
	srcTotal, err := r.Count(ctx, r.Src, table)
	if err != nil {
		return fail("recount src after %s: %v", label, err)
	}
	ts.SrcRows = srcTotal
	if p.plan.Mode == ModeRowLevel {
		// fres.DstRows, not 0: after an in-place ALTER the destination
		// still holds rows (an empty source with a non-empty destination
		// must come back as key-addressed deletes)
		w, err := r.PlanRowWork(ctx, p, fres, srcTotal, fres.DstRows)
		switch {
		case errors.Is(err, errEscalateFull):
			// A stale plan (data moved between the passes) needs the full
			// load — which TRUNCATEs. It runs only within the confirmed
			// scope; otherwise the table stops (P0-1).
			if !scope.FullResync {
				return fail("%s (%s): table %s — the re-plan after the %s requires the full resync, which the confirmed plan did not show; the table was stopped and no TRUNCATE was executed. Re-run so the new plan can be reviewed and confirmed",
					ErrReplanRequired, ErrFullResyncRequired, table, label)
			}
			// fall through to the plain full load
		case err != nil:
			return fail("%v", err)
		default:
			if w.rewrites > 0 && !scope.RowRewrite {
				// the structure preflight never planned rows, so the
				// confirmed scope carries no rewrites: any rewrite here
				// is new destructive scope, refused (P0)
				return fail("%s: table %s — the re-plan after the %s needs %d destructive row rewrite group(s) (DELETE+INSERT), which the confirmed plan did not show; the table was stopped and no rewrite was executed. Re-run so the new plan can be reviewed and confirmed",
					ErrReplanRequired, table, label, w.rewrites)
			}
			st := &Stats{Table: table, Mode: "ROWLEVEL"}
			r.ApplyRowWork(ctx, p, w, ap, st)
			if ts.Mode != "CREATE" {
				ts.Mode = "ROWLEVEL"
			}
			ts.Inserts, ts.Updates, ts.Deletes = st.Inserts, st.Updates, st.Deletes
			ts.Chunks = st.Chunks
			if st.Error != "" {
				ts.Status, ts.Error = "FAILED", st.Error
				return *ts
			}
			ts.Status = "APPLIED"
			r.applyState(ctx, table, ap, ts)
			return *ts
		}
	}
	st := &Stats{Table: table, Mode: "FULL"}
	if !preTruncated {
		// the in-place structure ALTER kept the destination's data; a
		// full load re-writes every source row, so the table is wiped
		// right before the stream (the only TRUNCATE in the structure
		// path, and only for a confirmed full resync)
		if err := ap.execDirect(ctx, "TRUNCATE TABLE "+conn.QuoteIdent(table)); err != nil {
			return fail("truncate before full resync: %v", err)
		}
		ts.Truncated = true
	}
	// Otherwise the caller emptied the table (the truncate fallback, or
	// a freshly created one) and the resync must not truncate again.
	ap.resync(ctx, st, p.b, p.srcS, compare.KeyFamilies(p.srcS), srcTotal)
	if ts.Mode != "CREATE" {
		ts.Mode = "FULL"
	}
	ts.Inserts, ts.Chunks = st.Inserts, st.Chunks
	if st.Error != "" {
		ts.Status, ts.Error = "FAILED", st.Error
		return *ts
	}
	ts.Status = "APPLIED"
	r.applyState(ctx, table, ap, ts)
	return *ts
}

// applyCreate executes the CREATE plan: create the table on the
// destination write connection, then converge the data like any other
// table (an empty destination is all INSERTs — row-level when the source
// offers a usable key, a plain full load otherwise). A successfully
// created table is not a success until its data and table state have
// converged: any failed step leaves it FAILED (the caller re-verifies).
func (r *Runner) applyCreate(ctx context.Context, res compare.TableResult, d decision, ap *Applier, scope DestructiveScope) TableSync {
	ts := d.ts
	if r.o.Progress != nil {
		r.o.Progress("%-24s create: %s", res.Name, strings.Join(d.sp.Reasons, "; "))
	}
	for _, stmt := range d.sp.DDL {
		if err := ap.execDirect(ctx, stmt); err != nil {
			ts.Status, ts.Error = "FAILED", fmt.Sprintf("create table: %v", err)
			return ts
		}
	}
	// a freshly created table is empty: no truncate anywhere on this path
	return r.convergeAfterDDL(ctx, res.Name, "create", ap, &ts, true, scope)
}

// samples renders up to limit sample statements from the ops. Each op kind
// present gets one sample first (a dry run that plans deletes must show a
// delete, not five inserts), the remaining slots fill in order (deterministic:
// the groups are in chunk order, the ops within a group are sorted by key
// identity).
func (r *Runner) samples(b *Builder, chunked [][]op, limit int) []string {
	if limit <= 0 {
		return nil
	}
	var all []op
	for _, ops := range chunked {
		all = append(all, ops...)
	}
	out := make([]string, 0, limit)
	taken := make(map[int]bool, limit)
	render := func(i int) {
		o := all[i]
		switch o.kind {
		case opInsert:
			out = append(out, b.Insert(o.rows[0]))
		case opUpdate:
			out = append(out, b.Update(o.key, o.rows[0]))
		case opDelete:
			out = append(out, b.Delete(o.key))
		case opRewrite:
			// one op, two statements: the destination row is deleted by
			// key and re-inserted (the unique slot is freed, the data is
			// unchanged) — show both halves so the dry run accounts for it.
			out = append(out, b.Delete(o.key)+
				"  -- unique-slot rewrite, then: "+b.Insert(o.rows[0]))
		}
		taken[i] = true
	}
	seen := make(map[opKind]bool)
	for i, o := range all {
		if len(out) == limit {
			break
		}
		if !seen[o.kind] {
			seen[o.kind] = true
			render(i)
		}
	}
	for i := range all {
		if len(out) == limit {
			break
		}
		if !taken[i] {
			render(i)
		}
	}
	return out
}

// fullSamples renders the dry-run sample for a FULL plan.
func (r *Runner) fullSamples(b *Builder, srcTotal int64) []string {
	return []string{
		b.Truncate(),
		fmt.Sprintf("-- then INSERT all %d source rows in batches of %d", srcTotal, r.o.Batch),
	}
}

// decision is the outcome of planDecision: the report-ready plan plus the
// internals the dry-run samples and the apply path need.
type decision struct {
	ts     TableSync
	p      *prep // prepared schemas and plan for data work (nil otherwise)
	sp     *StructPlan
	create bool // the destination table is created before the data sync
	keyed  bool // create only: the source offers a usable key
}

// planDecision computes the sync DECISION for one pre-pass result without
// the row-level re-scan: PlanTable adds the row work (and its samples)
// on top, and ApplyTable routes on it (re-running the decision fresh
// right before writing).
//
// A table missing on the destination becomes a CREATE (structure sync on,
// no --where). A structure drift becomes the DDL, and the post-repair data
// strategy is decided from the SOURCE structure (a repaired destination
// has exactly that shape: a usable key there restores row-level sync). An
// identical table still gets its table state (AUTO_INCREMENT) checked. A
// differing table goes through prepare + DecidePlan, with the table-state
// fix appended when needed.
func (r *Runner) planDecision(ctx context.Context, res compare.TableResult) decision {
	d := decision{ts: TableSync{Name: res.Name, SrcRows: res.SrcRows, DstRows: res.DstRows}}
	fail := func(argErr bool, format string, args ...any) decision {
		d.ts.Mode, d.ts.Status, d.ts.Error = "ERROR", "FAILED", fmt.Sprintf(format, args...)
		d.ts.ArgErr = argErr
		return d
	}
	// A table missing on the destination: with the structure sync on and
	// no --where it is created before its data is synced (the pre-pass
	// erred on introspection, not on data); under --where (a row-level
	// filter) or with the structure sync off it cannot be created, so it
	// fails with a message that says why, instead of the raw
	// introspection error.
	if res.Status == "ERROR" && r.missingOnDst(ctx, res.Name) {
		switch {
		case r.o.SyncSchema && r.o.Cmp.Where == "":
			sp, keyed, err := r.createPlanFor(ctx, res.Name)
			if err != nil {
				return fail(false, "create plan: %v", err)
			}
			srcTotal, err := r.Count(ctx, r.Src, res.Name)
			if err != nil {
				return fail(false, "count src: %v", err)
			}
			d.create, d.sp, d.keyed = true, sp, keyed
			d.ts.Mode, d.ts.Status = "CREATE", "PLANNED"
			d.ts.SchemaChanged = true
			d.ts.SchemaSQL = sp.DDL
			d.ts.SrcRows = srcTotal
			return d
		case r.o.Cmp.Where != "":
			return fail(false, "table %s is missing on the destination: --where is a row-level filter and does not create tables", res.Name)
		default:
			return fail(false, "table %s is missing on the destination and the structure sync is off (--no-sync-schema): cannot create it", res.Name)
		}
	}
	sp, srcS, err := r.SchemaPlanFor(ctx, res.Name)
	if err != nil {
		return fail(false, "%v", err)
	}
	if sp != nil {
		if r.o.Cmp.Where != "" {
			return fail(true, "%s", errWhereDrift)
		}
		// Structure repair changes what the data sync can do: the
		// destination takes the source's shape, so a usable key the source
		// has (primary key or NOT-NULL unique index) makes the repaired
		// table row-addressable again and it goes back to row-level sync
		// instead of a blind full resync. The apply path re-reads the
		// metadata after the repair and re-decides from the actual key
		// state (convergeAfterDDL).
		mode := "FULL"
		if len(r.o.Cmp.Key) > 0 || (srcS != nil && UsableKeyOf(srcS) != nil) {
			mode = "ROWLEVEL"
		}
		d.sp = sp
		d.ts.Mode, d.ts.Status = mode, "PLANNED"
		d.ts.SchemaChanged = true
		d.ts.SchemaSQL = sp.DDL
		return d
	}
	switch res.Status {
	case "OK":
		st := r.checkState(ctx, res.Name)
		if st.ddl != "" {
			d.ts.Mode, d.ts.Status = "STATE", "PLANNED"
			d.ts.StateChanged = true
			d.ts.StateSQL = []string{st.ddl}
			return d
		}
		if st.note != "" {
			// a divergence no row-level operation can fix: planned (the
			// run is non-zero) but carries no statement — nothing is
			// written for it, the state check reports it
			d.ts.Mode, d.ts.Status = "STATE", "PLANNED"
			d.ts.StateNote = st.note
			return d
		}
		d.ts.Mode, d.ts.Status = "SKIP", "SKIPPED"
		return d
	case "ERROR":
		d.ts.Mode, d.ts.Status, d.ts.Error = "ERROR", "FAILED", res.Error
		return d
	}
	p, err := r.prepare(ctx, res)
	if err != nil {
		return fail(errors.Is(err, ErrMisconfigured), "%v", err)
	}
	if p.plan.Mode == ModeError {
		d.ts.Mode, d.ts.Status, d.ts.Error, d.ts.ArgErr = "ERROR", "FAILED", p.plan.Error, p.plan.ArgErr
		return d
	}
	d.p = p
	d.ts.Mode, d.ts.Status = string(p.plan.Mode), "PLANNED"
	if st := r.checkState(ctx, res.Name); st.ddl != "" {
		d.ts.StateChanged = true
		d.ts.StateSQL = []string{st.ddl}
	} else if st.note != "" {
		d.ts.StateNote = st.note
	}
	return d
}

// createDataNote renders the dry-run note for what a created table's data
// sync will look like.
func createDataNote(keyed bool, rows int64, batch int) string {
	if keyed {
		return fmt.Sprintf("-- then row-level INSERT of all %d source rows in batches of %d", rows, batch)
	}
	return fmt.Sprintf("-- then INSERT all %d source rows in batches of %d", rows, batch)
}

// PlanTable computes the plan for one pre-pass result: mode, counts and
// sample statements, without writing anything. For a row-level table it
// is the PREFLIGHT (P0-3): it runs the same row planning the apply
// re-runs, so the destructive scope the user confirms — a full resync
// for a stale or cross-chunk plan, a destructive row rewrite — is
// computed from a real plan and recorded in TableSync.Scope. The apply
// re-plans right before writing and may only stay within it. The
// preflight costs the differing chunks one extra scan; that is
// deliberate — the confirmation must cover what actually runs.
func (r *Runner) PlanTable(ctx context.Context, res compare.TableResult) TableSync {
	d := r.planDecision(ctx, res)
	if d.ts.Status == "FAILED" {
		return d.ts
	}
	scope := DestructiveScope{}
	switch {
	case d.create:
		// the CREATE DDL is already shown on the DDL line: the sample is
		// only what the data sync does once the table exists. A freshly
		// created table is empty, so the data path is all INSERTs — no
		// destructive operation is in scope.
		ts := d.ts
		ts.SampleSQL = []string{createDataNote(d.keyed, ts.SrcRows, r.o.Batch)}
		ts.Scope = scope
		return ts
	case d.sp != nil:
		// structure drift: the DDL is already shown on the DDL lines; the
		// sample is what the data sync does after the in-place repair
		// (P1-3: no truncate by default). A still-keyless pair ends on
		// the full load, which truncates right before the reload — shown
		// in the sample, so the scope carries the full resync. The
		// pre-pass of a structure-mismatched table never counted, so
		// count the source fresh for the sample line.
		ts := d.ts
		srcTotal, err := r.Count(ctx, r.Src, res.Name)
		if err != nil {
			ts.Mode, ts.Status, ts.Error = "ERROR", "FAILED", err.Error()
			return ts
		}
		ts.SrcRows = srcTotal
		if ts.Mode == "ROWLEVEL" {
			// after the repair the data path is row-level: no TRUNCATE is
			// shown, so none is in scope (a stale apply-time re-plan
			// that would need one stops the table instead of truncating)
			ts.SampleSQL = []string{createDataNote(true, srcTotal, r.o.Batch)}
		} else {
			scope.FullResync = true
			ts.SampleSQL = []string{"TRUNCATE TABLE " + conn.QuoteIdent(res.Name),
				createDataNote(false, srcTotal, r.o.Batch)}
		}
		ts.Scope = scope
		return ts
	case d.p != nil:
		ts := d.ts
		if d.ts.Mode == "ROWLEVEL" {
			w, err := r.PlanRowWork(ctx, d.p, res, res.SrcRows, res.DstRows)
			if errors.Is(err, errEscalateFull) {
				if r.o.Cmp.Where != "" {
					// the plan went stale and a filtered table cannot be
					// fully resynced: apply will refuse it (runtime
					// error, zero writes), so the dry run must not show
					// a TRUNCATE that can never run — mirror the
					// refusal instead (exit 2, like the apply).
					ts.Status, ts.Error = "FAILED", err.Error()+" (a filtered table cannot be fully resynced)"
					return ts
				}
				// the plan went stale; the dry-run shows what apply would do
				ts.Mode, ts.Status = "FULL", "PLANNED"
				scope.FullResync = true
				ts.SampleSQL = r.fullSamples(d.p.b, res.SrcRows)
				ts.Scope = scope
				return ts
			}
			if err != nil {
				ts.Mode, ts.Status, ts.Error = "ERROR", "FAILED", err.Error()
				return ts
			}
			switch w.kind {
			case rowWorkDstDelete:
				// empty source match set: the apply streams the deletes
				// (chunk-sized transactions). The dry run shows the
				// COUNT plus a bounded key sample — it never scans the
				// whole match set (a 100M-row empty-source match set is
				// not scanned for a dry run).
				dpl := chunk.Planner{
					Table:       d.p.dstS.Table,
					KeyCols:     d.p.dstS.Key,
					KeyFamilies: compare.KeyFamilies(d.p.dstS),
					ChunkSize:   r.o.Cmp.ChunkSize,
					Where:       r.o.Cmp.Where,
				}
				_ = r.Dst.WithControl(ctx, func(q conn.Queryer) error {
					var err error
					var chunks []chunk.Chunk
					chunks, err = dpl.Plan(ctx, q, w.dstDel)
					if err == nil {
						ts.Chunks = len(chunks)
					}
					return err
				})
				ts.Deletes = int(w.dstDel)
				ts.SampleSQL = []string{fmt.Sprintf("STREAM DELETE ~%d destination rows by key (chunk-sized transactions)", w.dstDel)}
				if ks, err := r.keySamples(ctx, r.Dst, d.p.dstS, r.o.SampleLimit); err == nil {
					ts.SampleSQL = append(ts.SampleSQL, ks...)
				}
				if w.oorDel > 0 {
					ts.SampleSQL = append(ts.SampleSQL, fmt.Sprintf("STREAM DELETE ~%d out-of-range destination rows by key (keyset-paginated batches)", w.oorDel))
				}
			case rowWorkNone:
				// it converged in the meantime: nothing to show
			default:
				ts.Inserts, ts.Updates, ts.Deletes = Counts(w.ops)
				ts.Deletes += int(w.oorDel)
				ts.Chunks = len(w.ops)
				ts.SampleSQL = r.samples(d.p.b, w.ops, r.o.SampleLimit)
				if w.oorDel > 0 {
					ts.SampleSQL = append(ts.SampleSQL, fmt.Sprintf("STREAM DELETE %d out-of-range destination rows by key (keyset-paginated batches, committed before the in-range writes)", w.oorDel))
				}
				if n := w.rewrites; n > 0 {
					// the destructive rewrite is shown SEPARATELY in the
					// confirmation summary — it is the only kind of
					// statement that touches rows the user did not ask to
					// change (FK/trigger side effects ride on it). The
					// scope carries it, so the apply may run rewrites up
					// to what the plan showed — no more.
					scope.RowRewrite = true
					ts.Rewrites = n
					scope.RewriteFingerprints = w.rewriteFPs
					ts.SampleSQL = append(ts.SampleSQL, fmt.Sprintf("DESTRUCTIVE ROW REWRITE: %d row group(s) are deleted and re-inserted to free unique slots (permitted by --allow-row-rewrite)", n))
				}
			}
		} else {
			scope.FullResync = true
			ts.SampleSQL = r.fullSamples(d.p.b, res.SrcRows)
		}
		ts.Scope = scope
		return ts
	}
	return d.ts
}

// ApplyTable executes the sync for one pre-pass result on the
// destination write connection. A table missing on the destination is
// created first (structure sync on, no --where); a structure-drifted
// table is realigned and re-planned (ApplyStructureTable); everything
// else is re-counted right before writing and routed through DecidePlan.
// The row counts never force a full resync: extra rows on the
// destination are row-level DELETEs.
//
// confirmed is the plan the user CONFIRMED (the preflight, read-only):
// its Scope is the destructive budget this run may use for the table.
// The re-plan may SHRINK it (the data converged: fewer writes, no
// rewrite, a full plan that no longer needs the TRUNCATE) but never
// expand it — a confirmed row-level plan can never TRUNCATE in this
// run, and a rewrite runs only when the confirmed plan showed one (P0).
// A scope expansion stops the table with ErrReplanRequired: zero
// destructive statements for it, the operator re-runs and confirms the
// new plan like any first plan.
func (r *Runner) ApplyTable(ctx context.Context, res compare.TableResult, ap *Applier, confirmed TableSync) TableSync {
	d := r.planDecision(ctx, res)
	if d.ts.Status == "FAILED" {
		return d.ts
	}
	if d.create {
		return r.applyCreate(ctx, res, d, ap, confirmed.Scope)
	}
	if d.sp != nil {
		// planDecision already rejected --where (argument error).
		return r.ApplyStructureTable(ctx, res, d.sp, ap, confirmed.Scope)
	}
	if d.p == nil {
		if d.ts.Mode == "STATE" {
			// only the table state is realigned: re-check right before
			// writing (the values may have moved since the plan)
			r.applyState(ctx, res.Name, ap, &d.ts)
			if d.ts.Status != "FAILED" {
				if len(d.ts.StateSQL) > 0 {
					d.ts.Status = "APPLIED"
				} else {
					// an unfixable divergence (the destination's counter
					// is above the source's): nothing was written, the
					// state check below reports it
					d.ts.Status = "SKIPPED"
				}
			}
			return d.ts
		}
		return d.ts // SKIP
	}
	p := d.p
	ts := d.ts
	fail := func(format string, args ...any) TableSync {
		ts.Mode, ts.Status, ts.Error = "ERROR", "FAILED", fmt.Sprintf(format, args...)
		return ts
	}
	mode := p.plan.Mode
	var w rowWork
	if mode == ModeRowLevel {
		freshSrc, err := r.Count(ctx, r.Src, res.Name)
		if err != nil {
			return fail("recount src: %v", err)
		}
		freshDst, err := r.Count(ctx, r.Dst, res.Name)
		if err != nil {
			return fail("recount dst: %v", err)
		}
		// the report shows the counts as of the apply, not the pre-pass
		ts.SrcRows, ts.DstRows = freshSrc, freshDst
		w, err = r.PlanRowWork(ctx, p, res, freshSrc, freshDst)
		switch {
		case errors.Is(err, errEscalateFull) && r.o.Cmp.Where != "":
			return fail("%v (a filtered table cannot be fully resynced)", err)
		case errors.Is(err, errEscalateFull):
			// the re-plan needs the order-independent full resync (stale
			// plan or a cross-chunk swap with --allow-row-rewrite). It
			// runs ONLY when the confirmed plan is the one that shows
			// the TRUNCATE; a confirmed row-level plan is stopped
			// instead — never TRUNCATEd (P0-1).
			if !confirmed.Scope.FullResync {
				return fail("%s (%s): table %s — the re-plan requires the full resync, which the confirmed plan did not show; the table was stopped and no TRUNCATE was executed. Re-run so the new plan can be reviewed and confirmed",
					ErrReplanRequired, ErrFullResyncRequired, res.Name)
			}
			mode = ModeFull
		case err != nil:
			return fail("%v", err)
		}
	}
	// the destructive-scope gate (P0): whatever the re-plan produced,
	// the destructive part of it must be within the confirmed scope —
	// see scopeGate. An expansion stops the table; nothing destructive
	// for it runs this run.
	if what := scopeGate(mode, w, confirmed); what != "" {
		return fail("%s: table %s — the re-plan needs %s, which the confirmed plan did not show; the table was stopped and no destructive statement was executed. Re-run so the new plan can be reviewed and confirmed",
			ErrReplanRequired, res.Name, what)
	}
	switch mode {
	case ModeFull:
		srcTotal, err := r.Count(ctx, r.Src, res.Name)
		if err != nil {
			return fail("recount src: %v", err)
		}
		ts.SrcRows = srcTotal
		st := &Stats{Table: res.Name, Mode: "FULL"}
		ap.ApplyFull(ctx, st, p.b, p.srcS, compare.KeyFamilies(p.srcS), srcTotal)
		ts.Mode, ts.Truncated = "FULL", st.Truncated
		ts.Inserts, ts.Deletes, ts.Chunks = st.Inserts, st.Deletes, st.Chunks
		if st.Error != "" {
			ts.Status, ts.Error = "FAILED", st.Error
			return ts
		}
	case ModeRowLevel:
		st := &Stats{Table: res.Name, Mode: "ROWLEVEL"}
		r.ApplyRowWork(ctx, p, w, ap, st)
		ts.Mode, ts.Inserts, ts.Updates, ts.Deletes = "ROWLEVEL", st.Inserts, st.Updates, st.Deletes
		ts.Chunks = st.Chunks
		if st.Error != "" {
			ts.Status, ts.Error = "FAILED", st.Error
			return ts
		}
	}
	ts.Status = "APPLIED"
	r.applyState(ctx, res.Name, ap, &ts)
	return ts
}

// DropPlanFor renders the (unexecuted) plan for dropping a destination-only
// table: whole-database mode treats the destination as a disposable copy of
// the source, so a table the source does not have is converged away. The
// caller must only produce these plans in whole-database mode (--tables and
// --where off): --tables scopes the run to the named tables and never
// drops anything else, and a filtered run cannot drop whole tables.
func DropPlanFor(table string) TableSync {
	return TableSync{
		Name:          table,
		Mode:          "DROP",
		Status:        "PLANNED",
		SchemaChanged: true,
		// IF EXISTS keeps a re-run convergent when the table vanished
		// between the discovery and the drop
		SchemaSQL: []string{"DROP TABLE IF EXISTS " + conn.QuoteIdent(table)},
	}
}

// ApplyDrop drops one destination-only table on the destination write
// connection, then confirms the table is gone (a DROP that reports success
// but leaves the table is still a failure). A failure marks the table
// FAILED: the next run re-discovers it and converges it again.
//
// TOCTOU re-gate: the pre-pass discovered this table as destination-only
// at an EARLIER moment, and the source may have created it since (or the
// destination table may have vanished out-of-band). A DROP is a
// destructive statement, so both facts are re-queried right before it
// executes, and any metadata anomaly vetoes it (fail closed):
//
//   - the source NOW HAS the table: the confirmed plan is stale — it said
//     "drop a table the source lacks". REFUSE (ErrReplanRequired): the
//     destination is untouched, nothing was executed, and the re-run
//     reviews the new plan (the table may now be a normal sync target).
//   - either re-check query fails: fail closed, DROP not executed.
//   - the destination table is already GONE: the goal state (absence)
//     was achieved without this run — converged, nothing executed.
func (r *Runner) ApplyDrop(ctx context.Context, table string, ap *Applier) TableSync {
	ts := DropPlanFor(table)
	if r.o.Progress != nil {
		r.o.Progress("%-24s drop (destination-only table)", table)
	}
	srcExists, err := r.dropProbe(ctx, r.Src, table, r.srcDropRecheck)
	if err != nil {
		ts.Status, ts.Error = "FAILED", fmt.Sprintf("re-check source before drop: %v — fail closed, DROP not executed", err)
		return ts
	}
	if srcExists {
		ts.Status, ts.Error = "FAILED", fmt.Sprintf("%s: source table %q appeared after the plan was confirmed — the destination table is untouched and the DROP was not executed. Re-run to re-plan: the table may now be a normal sync target",
			ErrReplanRequired, table)
		return ts
	}
	dstExists, err := r.dropProbe(ctx, r.Dst, table, r.dstDropRecheck)
	if err != nil {
		ts.Status, ts.Error = "FAILED", fmt.Sprintf("re-check destination before drop: %v — fail closed, DROP not executed", err)
		return ts
	}
	if !dstExists {
		if r.o.Progress != nil {
			r.o.Progress("%-24s drop: already absent (converged, nothing executed)", table)
		}
		ts.Status, ts.Verified = "APPLIED", "OK"
		return ts
	}
	if err := ap.execDirect(ctx, ts.SchemaSQL[0]); err != nil {
		ts.Status, ts.Error = "FAILED", fmt.Sprintf("drop table: %v", err)
		return ts
	}
	exists, err := r.dropProbe(ctx, r.Dst, table, r.dstDropPostcheck)
	if err != nil {
		ts.Status, ts.Error = "FAILED", fmt.Sprintf("verify drop: %v", err)
		return ts
	}
	if exists {
		ts.Status, ts.Error = "FAILED", "table still exists after DROP TABLE"
		return ts
	}
	ts.Status, ts.Verified = "APPLIED", "OK"
	return ts
}

// dropProbe is one side's table-existence check around a DROP: the unit
// hooks (srcDropRecheck / dstDropRecheck / dstDropPostcheck) drive the
// branches in tests; nil in production, where it queries the side's
// information schema.
func (r *Runner) dropProbe(ctx context.Context, side *conn.Side, table string, hook func(context.Context, string) (bool, error)) (bool, error) {
	if hook != nil {
		return hook(ctx, table)
	}
	var exists bool
	if err := side.WithControl(ctx, func(q conn.Queryer) error {
		var err error
		exists, err = conn.TableExists(ctx, q, table)
		return err
	}); err != nil {
		return false, err
	}
	return exists, nil
}
