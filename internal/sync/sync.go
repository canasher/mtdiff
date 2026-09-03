package sync

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
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
	// Progress receives long-running phase updates (pre-pass scans, apply
	// chunks, verification scans), forwarded to the comparer when the
	// caller left Cmp.Progress unset. nil = no progress output.
	Progress func(format string, args ...any)
}

// TableSync is the per-table outcome of a sync run.
type TableSync struct {
	Name string
	// Mode is the strategy: "SKIP" | "FULL" | "ROWLEVEL" | "ERROR" |
	// "CREATE" (the destination table is missing and is created) |
	// "DROP" (a destination-only table is dropped, whole-database mode) |
	// "STATE" (only the table state, AUTO_INCREMENT, is realigned).
	Mode      string
	SrcRows   int64
	DstRows   int64
	Inserts   int
	Updates   int
	Deletes   int
	Chunks    int
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
	// stateGapOnce / stateExact cache the one-shot capability probe
	// (stateReconcilable): backends that pre-allocate auto-increment ID
	// ranges (an allocator, e.g. TiDB's batch allocation) report an
	// estimate, not the exact next value — the table-state
	// reconciliation degrades to skipped there too.
	stateGapOnce sync.Once
	stateExact   bool
}

// stateGapLimit: the largest distance a side's reported next
// AUTO_INCREMENT value may sit above max(column)+1 before the backend is
// taken to pre-allocate ID ranges (an allocator, e.g. TiDB) and the
// reported value is not an exact next value.
const stateGapLimit = 10000

func NewRunner(src, dst *conn.Side, o Options) *Runner {
	if o.Cmp.Progress == nil {
		o.Cmp.Progress = o.Progress
	}
	return &Runner{Src: src, Dst: dst, o: o, cmp: compare.NewComparer(o.Cmp)}
}

// sideDefaultCollations resolves each side's database default collation
// (conn.DefaultCollation), once per run. It is best-effort: a backend
// that refuses the query degrades to the strict comparison (a collation
// difference is always reported), never to a failed run.
func (r *Runner) sideDefaultCollations(ctx context.Context) (src, dst string) {
	r.sdOnce.Do(func() {
		r.sdSrc, _ = conn.DefaultCollation(ctx, r.Src.Ctl())
		r.sdDst, _ = conn.DefaultCollation(ctx, r.Dst.Ctl())
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

// stateReconcilable resolves once per run whether the table state
// (AUTO_INCREMENT) is exactly comparable on both sides. A side whose
// reported next value sits more than stateGapLimit above max(column)+1
// pre-allocates ID ranges (an allocator, e.g. TiDB's batch allocation):
// the value it reports is an estimate, an explicit counter below the
// allocated range's end is silently ignored, and even a full resync
// re-allocates a new range — the state cannot be converged there, so the
// reconciliation degrades to skipped (a one-shot warning, never a
// failed run), like an unreadable state. An unsupported probe query
// leaves the decision to the per-table degradation in tableAutoInc.
func (r *Runner) stateReconcilable(ctx context.Context) bool {
	r.stateGapOnce.Do(func() {
		r.stateExact = true
		for _, side := range []*conn.Side{r.Src, r.Dst} {
			gap, probed, err := conn.AutoIncGap(ctx, side.Ctl())
			if err != nil {
				return
			}
			if probed && gap > stateGapLimit {
				r.stateExact = false
				fmt.Fprintf(os.Stderr, "warn: %s's auto-increment counter sits %d above the data maximum (pre-allocated ID range): the table state (AUTO_INCREMENT) is not exactly comparable on this backend and is skipped\n",
					side.Name, gap)
				return
			}
		}
	})
	return r.stateExact
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

// checkState reads both sides' next AUTO_INCREMENT values and decides
// whether the destination's table state drifted and what realigns it. It
// is a no-op under --where (a table-level state has no filtered meaning),
// on a backend that pre-allocates auto-increment ID ranges (stateReconcilable,
// a capability degradation — see there), and when the source has no
// auto-increment column (information_schema reports NULL — in that case
// no ALTER is ever emitted, per the convergence contract). A destination
// without the column cannot be set: no DDL, and the state is reported
// unaligned (a structure drift the structure pre-step must repair first,
// or an explicit --no-sync-schema).
// When the destination's counter is ABOVE the source's, no DDL is
// planned either: an auto-increment counter can only be raised, and a
// full resync is the only thing that realigns it — the divergence is
// reported (note) and the run exits non-zero. A backend that cannot read
// the state degrades to (no DDL, aligned): the reconciliation is skipped
// with a one-shot warning, never a failed run.
func (r *Runner) checkState(ctx context.Context, table string) tableState {
	if r.o.Cmp.Where != "" || !r.stateReconcilable(ctx) {
		return tableState{}
	}
	srcVal, srcHas := r.tableAutoInc(ctx, r.Src, table)
	if !srcHas {
		return tableState{srcHas: srcHas}
	}
	dstVal, dstHas := r.tableAutoInc(ctx, r.Dst, table)
	st := tableState{srcVal: srcVal, dstVal: dstVal, srcHas: true, dstHas: dstHas, aligned: dstHas && dstVal == srcVal}
	if dstHas && !st.aligned {
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
// of scope: --where, a state the backend cannot read, or a backend that
// pre-allocates auto-increment ID ranges — out of scope, never a
// failure).
func (r *Runner) VerifyState(ctx context.Context, table string) string {
	if r.o.Cmp.Where != "" || !r.stateReconcilable(ctx) {
		return ""
	}
	srcVal, srcHas := r.tableAutoInc(ctx, r.Src, table)
	if !srcHas {
		return "OK"
	}
	dstVal, dstHas := r.tableAutoInc(ctx, r.Dst, table)
	if dstHas && dstVal == srcVal {
		return "OK"
	}
	return "DIFFERENT"
}

// tableAutoInc reads one side's next AUTO_INCREMENT value, best-effort: a
// failed query (a backend without the column) degrades to (0, false) with
// a one-shot warning.
func (r *Runner) tableAutoInc(ctx context.Context, side *conn.Side, table string) (int64, bool) {
	v, ok, err := conn.TableAutoIncrement(ctx, side.Ctl(), table)
	if err != nil {
		r.aiWarned.Do(func() {
			fmt.Fprintf(os.Stderr, "warn: cannot read the AUTO_INCREMENT state on %s (%v); table-state reconciliation is skipped\n", side.Name, err)
		})
		return 0, false
	}
	return v, ok
}

// missingOnDst reports whether the table exists on the source but not on
// the destination: the pre-pass erred because the destination copy is
// absent, not because of a schema mismatch or a runtime problem.
func (r *Runner) missingOnDst(ctx context.Context, table string) bool {
	dstOK, err := conn.TableExists(ctx, r.Dst.Ctl(), table)
	if err != nil || dstOK {
		return false
	}
	srcOK, err := conn.TableExists(ctx, r.Src.Ctl(), table)
	return err == nil && srcOK
}

// createPlanFor builds the plan for a table the destination is missing:
// a CREATE TABLE rendered from the source's structure (columns in order,
// primary key, unique indexes, engine, and the source's current next
// AUTO_INCREMENT value so the new table starts on the right counter),
// followed by the data sync the table needs once it exists (an empty
// destination: every source row is an INSERT — row-level when the source
// offers a usable key, a plain full load otherwise).
func (r *Runner) createPlanFor(ctx context.Context, table string) (*StructPlan, bool, error) {
	srcS, err := conn.IntrospectStructure(ctx, r.Src.Ctl(), table)
	if err != nil {
		return nil, false, err
	}
	srcS = filterStruct(srcS, r.o.Cmp.Normalize.IgnoreCols)
	autoInc, hasAuto := r.tableAutoInc(ctx, r.Src, table)
	engine, err := conn.TableEngine(ctx, r.Src.Ctl(), table)
	if err != nil {
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
	if err := side.Ctl().QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("%s %s: %w", side.Name, table, err)
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
	p := &prep{
		srcS: srcS,
		dstS: dstS,
		plan: DecidePlan(res, len(srcS.Key) > 0, len(dstS.Key) > 0, r.o.Cmp.Where),
		b:    NewBuilder(res.Name, srcS),
	}
	p.e = NewEngine(
		normalize.NewNormalizer(srcS.Cols, r.o.Cmp.Normalize),
		normalize.NewNormalizer(dstS.Cols, r.o.Cmp.Normalize),
		normalize.NewNormalizer(keyColsOf(srcS), r.o.Cmp.Normalize),
		normalize.NewNormalizer(keyColsOf(dstS), r.o.Cmp.Normalize),
		srcS.KeyIsUnique, keyColsOf(srcS), srcS.Cols)
	return p, nil
}

// RowOps computes the row-level operations for a planned table: it re-plans
// on the source (with the freshly recounted source row count, not the
// pre-pass one — a source that grew in between must not silently turn the
// re-plan into a handful of over-sized chunks) and buffers both sides of
// the planned chunks only (the pre-pass already proved the matching chunks
// identical). When the pre-pass planned no chunks (row counts differ,
// planning was short-circuited) the differing chunks are unknown and every
// chunk is rescaned.
//
// It returns the ops grouped by chunk (in chunk order): the applier commits
// one transaction per group, so a mid-apply failure rolls back that group's
// writes only.
//
// When the source re-plans to no chunk but the destination still has rows
// (only possible with a --where filter: zero source matches, N destination
// matches), the destination side is re-planned instead and every
// destination row is deleted: a filtered table cannot be truncated, so its
// only path to convergence is emptying the destination's match set.
//
// After the chunk ops, the destination is scanned once for rows whose key
// falls strictly outside the source's key range (the chunks never cover
// them) and each is deleted individually (outOfRangeDeletes). Without a
// filter this keeps the first round from escalating to a full resync; with
// a filter it is the only path to convergence for out-of-range rows.
func (r *Runner) RowOps(ctx context.Context, p *prep, res compare.TableResult, freshSrc, freshDst int64) ([][]op, error) {
	planner := chunk.Planner{
		Table:       res.Name,
		KeyCols:     p.srcS.Key,
		KeyFamilies: compare.KeyFamilies(p.srcS),
		ChunkSize:   r.o.Cmp.ChunkSize,
		Where:       r.o.Cmp.Where,
	}
	chunks, err := planner.Plan(ctx, r.Src.Ctl(), freshSrc)
	if err != nil {
		return nil, fmt.Errorf("re-plan: %w", err)
	}
	if len(chunks) == 0 {
		if freshDst <= 0 {
			return nil, nil
		}
		return r.dstDeletes(ctx, p, freshDst)
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
				return nil, errEscalateFull
			}
			targets = append(targets, ch)
		}
	} else {
		targets = chunks
	}
	out := make([][]op, 0, len(targets))
	for _, ch := range targets {
		srcM, err := p.e.scanSide(ctx, r.Src, p.srcS, p.e.srcRow, p.e.srcKey, ch, r.o.Cmp.Where)
		if err != nil {
			return nil, fmt.Errorf("src chunk %d: %w", ch.ID, err)
		}
		dstM, err := p.e.scanSide(ctx, r.Dst, p.dstS, p.e.dstRow, p.e.dstKey, ch, r.o.Cmp.Where)
		if err != nil {
			return nil, fmt.Errorf("dst chunk %d: %w", ch.ID, err)
		}
		out = append(out, p.e.Diff(srcM, dstM))
	}
	oor, err := r.outOfRangeDeletes(ctx, p, freshSrc)
	if err != nil {
		return nil, fmt.Errorf("out-of-range scan: %w", err)
	}
	return append(out, oor...), nil
}

// dstDeletes plans the destination side and deletes every row it holds:
// the fallback for a filtered table whose source match set is empty (the
// source re-plan yields no chunks, so there is nothing to target deletes
// from). The destination's key is the same key the comparison used, so
// every row can be addressed by key.
func (r *Runner) dstDeletes(ctx context.Context, p *prep, freshDst int64) ([][]op, error) {
	planner := chunk.Planner{
		Table:       p.dstS.Table,
		KeyCols:     p.dstS.Key,
		KeyFamilies: compare.KeyFamilies(p.dstS),
		ChunkSize:   r.o.Cmp.ChunkSize,
		Where:       r.o.Cmp.Where,
	}
	chunks, err := planner.Plan(ctx, r.Dst.Ctl(), freshDst)
	if err != nil {
		return nil, fmt.Errorf("re-plan dst: %w", err)
	}
	out := make([][]op, 0, len(chunks))
	for _, ch := range chunks {
		dstM, err := p.e.scanSide(ctx, r.Dst, p.dstS, p.e.dstRow, p.e.dstKey, ch, r.o.Cmp.Where)
		if err != nil {
			return nil, fmt.Errorf("dst chunk %d: %w", ch.ID, err)
		}
		keys := make([]string, 0, len(dstM))
		for k := range dstM {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		ops := make([]op, 0)
		for _, k := range keys {
			for _, row := range dstM[k] {
				ops = append(ops, op{kind: opDelete, key: p.e.keyVals(row.vals)})
			}
		}
		out = append(out, ops)
	}
	return out, nil
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
// maximum key values: RenderLessThan(min) OR RenderGreaterThan(max). The
// comparison is strict on purpose — a row equal to a bound is in range and
// belongs to the chunk diff. A side that renders empty is dropped; when
// both do (a single-column all-NULL minimum bounds everything below) ok is
// false. The bounds are rendered with the source key columns' family-aware
// literals (a character or decimal bound rendered as a hex blob would put
// rows on the wrong side of the boundary).
func (r *Runner) oorPredicate(p *prep, minV, maxV []driver.Value) (string, bool) {
	lits := keyLits(compare.KeyFamilies(p.srcS))
	// a side joins the two tails with a top-level OR only when the key is
	// composite; wrap it then (a single term, or a fully parenthesized
	// group like "(`a` IS NULL OR ...)", is safe to OR as-is)
	wrap := func(s string) string {
		if strings.Contains(s, " OR ") && !isParenGroup(s) {
			return "(" + s + ")"
		}
		return s
	}
	var sides []string
	if !allNil(minV) {
		// the all-NULL row is the minimum: nothing sits below it
		if s := chunk.RenderLessThan(p.dstS.Key, minV, lits); s != "1=0" {
			sides = append(sides, wrap(s))
		}
	}
	if s := chunk.RenderGreaterThan(p.dstS.Key, maxV, lits); s != "1=0" {
		sides = append(sides, wrap(s))
	}
	if len(sides) == 0 {
		return "", false
	}
	return strings.Join(sides, " OR "), true
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

// outOfRangeDeletes plans the deletes of destination rows whose key falls
// strictly outside the source's key range: the row-level chunks only cover
// the source's min..max range, so such rows would otherwise never be
// touched. It reads the source's extremes (the --where filter applies, as
// to the destination scan below), scans the destination for rows outside
// them, and deletes them one by one, grouped in Batch-sized transactions
// appended after the in-range chunk groups.
//
// It is a no-op for a keyless table, an empty source (the caller took
// dstDeletes instead), a key the two sides disagree on, and a scan that
// finds nothing. With a --where filter, out-of-range rows that do not
// match the filter are left in place: a filtered table cannot be
// truncated, so they are the one documented residual (the verification
// reports the table DIFFERENT and a plain, unfiltered comparison shows
// them).
func (r *Runner) outOfRangeDeletes(ctx context.Context, p *prep, freshSrc int64) ([][]op, error) {
	if freshSrc <= 0 || len(p.srcS.Key) == 0 || len(p.dstS.Key) == 0 {
		return nil, nil
	}
	if !keyAgree(p.srcS, p.dstS) {
		// the bounds are the SOURCE's key values rendered against the
		// DESTINATION's key columns, so the two sides must agree on the
		// key by name and order (with --no-sync-schema the keys can drift
		// at equal length, e.g. PK (a, b) vs (b, a) — rendering the src
		// values against the wrong columns would delete in-range rows)
		return nil, nil
	}
	sp := chunk.Planner{
		Table:       p.srcS.Table,
		KeyCols:     p.srcS.Key,
		KeyFamilies: compare.KeyFamilies(p.srcS),
		Where:       r.o.Cmp.Where,
	}
	minV, maxV, err := sp.Extremes(ctx, r.Src.Ctl())
	if err != nil {
		return nil, err
	}
	if minV == nil || maxV == nil {
		// the source emptied between the count and the extremes read
		// (Extremes returns both nil in that case; maxV is checked too
		// because a nil bound would panic the strict comparators)
		return nil, nil
	}
	pred, ok := r.oorPredicate(p, minV, maxV)
	if !ok {
		return nil, nil
	}
	rows, err := p.e.scanKeyRows(ctx, r.Dst, p.dstS, pred, r.o.Cmp.Where)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	// deterministic order: the same normalized key identity the in-range
	// deletes are sorted by
	type keyed struct {
		canon string
		kv    []any
	}
	kr := make([]keyed, 0, len(rows))
	buf := make([]byte, 0, 256)
	for _, vals := range rows {
		canon, err := p.e.dstKey.Normalize(vals, buf)
		if err != nil {
			return nil, err
		}
		buf = canon[:0]
		kr = append(kr, keyed{canon: string(canon), kv: vals})
	}
	sort.Slice(kr, func(i, j int) bool { return kr[i].canon < kr[j].canon })
	ops := make([]op, 0, len(kr))
	for _, k := range kr {
		ops = append(ops, op{kind: opDelete, key: k.kv})
	}
	return groupOps(ops, r.o.Batch), nil
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
func (r *Runner) SchemaPlanFor(ctx context.Context, table string) (*StructPlan, *conn.Struct, error) {
	if !r.o.SyncSchema {
		return nil, nil, nil
	}
	srcS, err := conn.IntrospectStructure(ctx, r.Src.Ctl(), table)
	if err != nil {
		return nil, nil, err
	}
	dstS, err := conn.IntrospectStructure(ctx, r.Dst.Ctl(), table)
	if err != nil {
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

// ApplyStructureTable syncs a structure-drifted table: truncate the
// destination first (so the DDL runs on an empty table and an added
// NOT NULL column without a default is always safe), apply the DDL, then
// converge the data — see convergeAfterDDL for the re-read / re-plan: a
// key the repair restores (e.g. the primary key) puts the table back on
// row-level sync, only a still-keyless pair stays on the full load. The
// table state (AUTO_INCREMENT) is reconciled last; the caller verifies.
func (r *Runner) ApplyStructureTable(ctx context.Context, res compare.TableResult, sp *StructPlan, ap *Applier) TableSync {
	ts := TableSync{Name: res.Name, SchemaChanged: true, SchemaSQL: sp.DDL, DstRows: 0}
	if r.o.Progress != nil {
		r.o.Progress("%-24s structure: %d DDL statement(s): %s", res.Name, len(sp.DDL), strings.Join(sp.Reasons, "; "))
	}
	if err := ap.execDirect(ctx, "TRUNCATE TABLE "+conn.QuoteIdent(res.Name)); err != nil {
		ts.Mode, ts.Status, ts.Error = "ERROR", "FAILED", fmt.Sprintf("truncate before structure sync: %v", err)
		return ts
	}
	ts.Truncated = true
	for _, stmt := range sp.DDL {
		if err := ap.execDirect(ctx, stmt); err != nil {
			ts.Mode, ts.Status, ts.Error = "ERROR", "FAILED", fmt.Sprintf("structure sync: %v", err)
			return ts
		}
	}
	return r.convergeAfterDDL(ctx, res.Name, "structure sync", ap, &ts)
}

// convergeAfterDDL converges a table whose structure work (a CREATE, or
// the structure-sync DDL) has just been applied on an emptied
// destination: it re-runs the comparison, re-reads the metadata and
// re-plans from the new structure (schema repair changes what the data
// sync can do — a restored key goes back to row-level sync, a still-keyless
// pair stays on the full load), writes the data, and reconciles the table
// state (the next AUTO_INCREMENT value; TRUNCATE and a fresh CREATE both
// reset it). The data may have converged while the table state has not:
// any failed step leaves the table FAILED and the next run re-plans from
// the current facts.
func (r *Runner) convergeAfterDDL(ctx context.Context, table, label string, ap *Applier, ts *TableSync) TableSync {
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
		ops, err := r.RowOps(ctx, p, fres, srcTotal, 0)
		if !errors.Is(err, errEscalateFull) {
			if err != nil {
				return fail("%v", err)
			}
			st := &Stats{Table: table, Mode: "ROWLEVEL"}
			ap.ApplyOps(ctx, st, p.b, ops)
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
		// A stale plan (data moved between the passes): fall through to
		// the plain full load — the destination was emptied already, so
		// it is inserts only.
	}
	st := &Stats{Table: table, Mode: "FULL"}
	// No TRUNCATE here: the caller emptied the table (before the
	// structure DDL, or it was just created).
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
func (r *Runner) applyCreate(ctx context.Context, res compare.TableResult, d decision, ap *Applier) TableSync {
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
	return r.convergeAfterDDL(ctx, res.Name, "create", ap, &ts)
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
// the row-level re-scan: PlanTable adds the sample statements (and the
// ops) on top, PlanSummary uses it as-is for the confirmation summary,
// and ApplyTable routes on it.
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

// PlanSummary computes only the plan DECISION for one pre-pass result
// (mode, reason, pre-pass counts, misconfiguration errors) without the
// row-level re-scan: the --apply path uses it for the confirmation
// summary, because ApplyTable re-plans, recounts and rescans right before
// writing anyway — planning the ops here too would scan the differing
// chunks twice for nothing the confirmation shows.
func (r *Runner) PlanSummary(ctx context.Context, res compare.TableResult) TableSync {
	return r.planDecision(ctx, res).ts
}

// PlanTable computes the dry-run outcome for one pre-pass result: mode,
// counts and sample statements, without writing anything.
func (r *Runner) PlanTable(ctx context.Context, res compare.TableResult) TableSync {
	d := r.planDecision(ctx, res)
	if d.ts.Status == "FAILED" {
		return d.ts
	}
	switch {
	case d.create:
		// the CREATE DDL is already shown on the DDL line: the sample is
		// only what the data sync does once the table exists
		ts := d.ts
		ts.SampleSQL = []string{createDataNote(d.keyed, ts.SrcRows, r.o.Batch)}
		return ts
	case d.sp != nil:
		// structure drift: the DDL is already shown on the DDL lines; the
		// sample is the TRUNCATE the repair starts with, then what the
		// data sync does after the repair. The pre-pass of a
		// structure-mismatched table never counted, so count the source
		// fresh for the sample line.
		ts := d.ts
		srcTotal, err := r.Count(ctx, r.Src, res.Name)
		if err != nil {
			ts.Mode, ts.Status, ts.Error = "ERROR", "FAILED", err.Error()
			return ts
		}
		ts.SrcRows = srcTotal
		ts.SampleSQL = []string{"TRUNCATE TABLE " + conn.QuoteIdent(res.Name),
			createDataNote(ts.Mode == "ROWLEVEL", srcTotal, r.o.Batch)}
		return ts
	case d.p != nil:
		ts := d.ts
		if d.ts.Mode == "ROWLEVEL" {
			ops, err := r.RowOps(ctx, d.p, res, res.SrcRows, res.DstRows)
			if errors.Is(err, errEscalateFull) {
				// the plan went stale; the dry-run shows what apply would do
				ts.Mode, ts.Status = "FULL", "PLANNED"
				ts.SampleSQL = r.fullSamples(d.p.b, res.SrcRows)
				return ts
			}
			if err != nil {
				ts.Mode, ts.Status, ts.Error = "ERROR", "FAILED", err.Error()
				return ts
			}
			ts.Inserts, ts.Updates, ts.Deletes = Counts(ops)
			ts.Chunks = len(ops)
			ts.SampleSQL = r.samples(d.p.b, ops, r.o.SampleLimit)
		} else {
			ts.SampleSQL = r.fullSamples(d.p.b, res.SrcRows)
		}
		return ts
	}
	return d.ts
}

// ApplyTable executes the sync for one pre-pass result on the
// destination write connection. A table missing on the destination is
// created first (structure sync on, no --where); a structure-drifted
// table is realigned and re-planned (ApplyStructureTable); everything
// else is re-counted right before writing and routed through DecidePlan —
// the row counts never force a full resync: extra rows on the
// destination are row-level DELETEs, and the only escalation is a stale
// row plan (data moved between the pre-pass and the re-plan), which has
// no safe row-level interpretation. The table state (AUTO_INCREMENT) is
// reconciled last; the caller re-verifies.
func (r *Runner) ApplyTable(ctx context.Context, res compare.TableResult, ap *Applier) TableSync {
	d := r.planDecision(ctx, res)
	if d.ts.Status == "FAILED" {
		return d.ts
	}
	if d.create {
		return r.applyCreate(ctx, res, d, ap)
	}
	if d.sp != nil {
		// planDecision already rejected --where (argument error).
		return r.ApplyStructureTable(ctx, res, d.sp, ap)
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
	var ops [][]op
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
		ops, err = r.RowOps(ctx, p, res, freshSrc, freshDst)
		switch {
		case errors.Is(err, errEscalateFull) && r.o.Cmp.Where != "":
			return fail("%v (a filtered table cannot be fully resynced)", err)
		case errors.Is(err, errEscalateFull):
			mode = ModeFull
		case err != nil:
			return fail("%v", err)
		}
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
		ap.ApplyOps(ctx, st, p.b, ops)
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
func (r *Runner) ApplyDrop(ctx context.Context, table string, ap *Applier) TableSync {
	ts := DropPlanFor(table)
	if r.o.Progress != nil {
		r.o.Progress("%-24s drop (destination-only table)", table)
	}
	if err := ap.execDirect(ctx, ts.SchemaSQL[0]); err != nil {
		ts.Status, ts.Error = "FAILED", fmt.Sprintf("drop table: %v", err)
		return ts
	}
	exists, err := conn.TableExists(ctx, r.Dst.Ctl(), table)
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
