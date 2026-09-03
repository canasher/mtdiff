package sync

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"sort"
	"strings"

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
	Name      string
	Mode      string // "SKIP" | "FULL" | "ROWLEVEL" | "ERROR"
	SrcRows   int64
	DstRows   int64
	Inserts   int
	Updates   int
	Deletes   int
	Chunks    int
	Truncated bool
	SampleSQL []string
	// SchemaChanged is set when the destination's structure drifted from
	// the source's and the DDL below aligns it before the data sync.
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
}

func NewRunner(src, dst *conn.Side, o Options) *Runner {
	if o.Cmp.Progress == nil {
		o.Cmp.Progress = o.Progress
	}
	return &Runner{Src: src, Dst: dst, o: o, cmp: compare.NewComparer(o.Cmp)}
}

// PrePass runs the (read-only) comparison over the given tables.
func (r *Runner) PrePass(ctx context.Context, tables []string) ([]compare.TableResult, error) {
	return r.cmp.Compare(ctx, r.Src, r.Dst, tables)
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
// diffs columns and primary/unique indexes. It returns (nil, nil) when the
// structure sync is disabled or the structures already match, (plan, nil)
// when DDL is needed, and (nil, err) when the structure cannot be planned
// (a side's table missing, or a source definition that cannot be
// reproduced — an expression default).
func (r *Runner) SchemaPlanFor(ctx context.Context, table string) (*StructPlan, error) {
	if !r.o.SyncSchema {
		return nil, nil
	}
	srcS, err := conn.IntrospectStructure(ctx, r.Src.Ctl(), table)
	if err != nil {
		return nil, err
	}
	dstS, err := conn.IntrospectStructure(ctx, r.Dst.Ctl(), table)
	if err != nil {
		return nil, err
	}
	srcS = filterStruct(srcS, r.o.Cmp.Normalize.IgnoreCols)
	dstS = filterStruct(dstS, r.o.Cmp.Normalize.IgnoreCols)
	changes, err := DiffStructure(srcS, dstS)
	if err != nil {
		return nil, err
	}
	if len(changes) == 0 {
		return nil, nil
	}
	reasons := make([]string, 0, len(changes))
	for _, ch := range changes {
		reasons = append(reasons, ch.Why)
	}
	return &StructPlan{DDL: RenderDDL(table, changes), Reasons: reasons}, nil
}

// ApplyStructureTable syncs a structure-drifted table: truncate the
// destination first (so the DDL runs on an empty table and an added
// NOT NULL column without a default is always safe), apply the DDL, then
// re-compare (the structures now match, so the comparison — and the
// post-sync verification — succeed) and stream every source row back in.
func (r *Runner) ApplyStructureTable(ctx context.Context, res compare.TableResult, sp *StructPlan, ap *Applier) TableSync {
	ts := TableSync{Name: res.Name, SchemaChanged: true, SchemaSQL: sp.DDL, DstRows: 0}
	fail := func(format string, args ...any) TableSync {
		ts.Mode, ts.Status, ts.Error = "ERROR", "FAILED", fmt.Sprintf(format, args...)
		return ts
	}
	if r.o.Progress != nil {
		r.o.Progress("%-24s structure: %d DDL statement(s): %s", res.Name, len(sp.DDL), strings.Join(sp.Reasons, "; "))
	}
	if err := ap.execDirect(ctx, "TRUNCATE TABLE "+conn.QuoteIdent(res.Name)); err != nil {
		return fail("truncate before structure sync: %v", err)
	}
	ts.Truncated = true
	for _, stmt := range sp.DDL {
		if err := ap.execDirect(ctx, stmt); err != nil {
			return fail("structure sync: %v", err)
		}
	}
	fresh, err := r.cmp.Compare(ctx, r.Src, r.Dst, []string{res.Name})
	if err != nil {
		return fail("recompare after structure sync: %v", err)
	}
	fres := fresh[0]
	if fres.Status == "ERROR" {
		return fail("structure sync applied but recompare failed: %s", fres.Error)
	}
	srcTotal, err := r.Count(ctx, r.Src, res.Name)
	if err != nil {
		return fail("recount src after structure sync: %v", err)
	}
	p, err := r.prepare(ctx, fres)
	if err != nil {
		ts.ArgErr = errors.Is(err, ErrMisconfigured)
		return fail("%v", err)
	}
	st := &Stats{Table: res.Name, Mode: "FULL"}
	ap.resync(ctx, st, p.b, p.srcS, compare.KeyFamilies(p.srcS), srcTotal)
	ts.Mode = "FULL"
	ts.SrcRows = srcTotal
	ts.Inserts, ts.Chunks = st.Inserts, st.Chunks
	if st.Error != "" {
		ts.Status, ts.Error = "FAILED", st.Error
		return ts
	}
	ts.Status = "APPLIED"
	return ts
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

// PlanSummary computes only the plan DECISION for one pre-pass result
// (mode, reason, pre-pass counts, misconfiguration errors) without the
// row-level re-scan: the --apply path uses it for the confirmation
// summary, because ApplyTable re-plans, recounts and rescans right before
// writing anyway — planning the ops here too would scan the differing
// chunks twice for nothing the confirmation shows.
func (r *Runner) PlanSummary(ctx context.Context, res compare.TableResult) TableSync {
	ts := TableSync{Name: res.Name, SrcRows: res.SrcRows, DstRows: res.DstRows}
	if sp, err := r.SchemaPlanFor(ctx, res.Name); err != nil {
		ts.Mode, ts.Status, ts.Error = "ERROR", "FAILED", err.Error()
		return ts
	} else if sp != nil {
		if r.o.Cmp.Where != "" {
			ts.Mode, ts.Status, ts.ArgErr, ts.Error = "ERROR", "FAILED", true, errWhereDrift
			return ts
		}
		// a drifted structure forces the full resync: row-level
		// addressing is invalid once the shape of the table changes
		ts.Mode, ts.Status, ts.SchemaChanged, ts.SchemaSQL = "FULL", "PLANNED", true, sp.DDL
		return ts
	}
	switch res.Status {
	case "OK":
		ts.Mode, ts.Status = "SKIP", "SKIPPED"
		return ts
	case "ERROR":
		ts.Mode, ts.Status, ts.Error = "ERROR", "FAILED", res.Error
		return ts
	}
	p, err := r.prepare(ctx, res)
	if err != nil {
		ts.Mode, ts.Status, ts.Error, ts.ArgErr = "ERROR", "FAILED", err.Error(), errors.Is(err, ErrMisconfigured)
		return ts
	}
	if p.plan.Mode == ModeError {
		ts.Mode, ts.Status, ts.Error, ts.ArgErr = "ERROR", "FAILED", p.plan.Error, p.plan.ArgErr
		return ts
	}
	ts.Mode, ts.Status = string(p.plan.Mode), "PLANNED"
	return ts
}

// PlanTable computes the dry-run outcome for one pre-pass result: mode,
// counts and sample statements, without writing anything.
func (r *Runner) PlanTable(ctx context.Context, res compare.TableResult) TableSync {
	ts := TableSync{Name: res.Name, SrcRows: res.SrcRows, DstRows: res.DstRows}
	if sp, err := r.SchemaPlanFor(ctx, res.Name); err != nil {
		ts.Mode, ts.Status, ts.Error = "ERROR", "FAILED", err.Error()
		return ts
	} else if sp != nil {
		if r.o.Cmp.Where != "" {
			ts.Mode, ts.Status, ts.ArgErr, ts.Error = "ERROR", "FAILED", true, errWhereDrift
			return ts
		}
		// structure drift: show the DDL, then the full-resync sample.
		// The pre-pass of a structure-mismatched table never counted, so
		// count the source fresh for the sample line.
		srcTotal, err := r.Count(ctx, r.Src, res.Name)
		if err != nil {
			ts.Mode, ts.Status, ts.Error = "ERROR", "FAILED", err.Error()
			return ts
		}
		ts.SrcRows = srcTotal
		ts.Mode, ts.Status, ts.SchemaChanged, ts.SchemaSQL = "FULL", "PLANNED", true, sp.DDL
		ts.SampleSQL = append(append([]string{}, sp.DDL...),
			"TRUNCATE TABLE "+conn.QuoteIdent(res.Name),
			fmt.Sprintf("-- then INSERT all %d source rows in batches of %d", srcTotal, r.o.Batch))
		return ts
	}
	switch res.Status {
	case "OK":
		ts.Mode, ts.Status = "SKIP", "SKIPPED"
		return ts
	case "ERROR":
		ts.Mode, ts.Status, ts.Error = "ERROR", "FAILED", res.Error
		return ts
	}
	p, err := r.prepare(ctx, res)
	if err != nil {
		ts.Mode, ts.Status, ts.Error, ts.ArgErr = "ERROR", "FAILED", err.Error(), errors.Is(err, ErrMisconfigured)
		return ts
	}
	if p.plan.Mode == ModeError {
		ts.Mode, ts.Status, ts.Error, ts.ArgErr = "ERROR", "FAILED", p.plan.Error, p.plan.ArgErr
		return ts
	}
	switch p.plan.Mode {
	case ModeFull:
		ts.Mode, ts.Status = "FULL", "PLANNED"
		ts.SampleSQL = r.fullSamples(p.b, res.SrcRows)
	case ModeRowLevel:
		ops, err := r.RowOps(ctx, p, res, res.SrcRows, res.DstRows)
		if errors.Is(err, errEscalateFull) {
			// the plan went stale; the dry-run shows what apply would do
			ts.Mode, ts.Status = "FULL", "PLANNED"
			ts.SampleSQL = r.fullSamples(p.b, res.SrcRows)
			return ts
		}
		if err != nil {
			ts.Mode, ts.Status, ts.Error = "ERROR", "FAILED", err.Error()
			return ts
		}
		ts.Mode, ts.Status = "ROWLEVEL", "PLANNED"
		ts.Inserts, ts.Updates, ts.Deletes = Counts(ops)
		ts.Chunks = len(ops)
		ts.SampleSQL = r.samples(p.b, ops, r.o.SampleLimit)
	}
	return ts
}

// ApplyTable executes the sync for one pre-pass result on the destination
// write connection. It recounts both sides right before writing and
// escalates a ROWLEVEL plan to FULL when the fresh counts say the
// destination now has more rows than the source (without --where; a
// filtered table can never be truncated).
func (r *Runner) ApplyTable(ctx context.Context, res compare.TableResult, ap *Applier) TableSync {
	ts := TableSync{Name: res.Name, SrcRows: res.SrcRows, DstRows: res.DstRows}
	fail := func(format string, args ...any) TableSync {
		ts.Mode, ts.Status, ts.Error = "ERROR", "FAILED", fmt.Sprintf(format, args...)
		return ts
	}
	if sp, err := r.SchemaPlanFor(ctx, res.Name); err != nil {
		return fail("%v", err)
	} else if sp != nil {
		if r.o.Cmp.Where != "" {
			ts.ArgErr = true
			return fail("%s", errWhereDrift)
		}
		return r.ApplyStructureTable(ctx, res, sp, ap)
	}
	switch res.Status {
	case "OK":
		ts.Mode, ts.Status = "SKIP", "SKIPPED"
		return ts
	case "ERROR":
		ts.Status, ts.Error = "FAILED", res.Error
		ts.Mode = "ERROR"
		return ts
	}
	p, err := r.prepare(ctx, res)
	if err != nil {
		ts.ArgErr = errors.Is(err, ErrMisconfigured)
		return fail("%v", err)
	}
	if p.plan.Mode == ModeError {
		ts.ArgErr = p.plan.ArgErr
		return fail("%s", p.plan.Error)
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
		if freshDst > freshSrc && r.o.Cmp.Where == "" {
			// the destination grew past the source since the pre-pass:
			// row-level deletes would not get the table to match anyway
			mode = ModeFull
		} else {
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
		ts.Status = "APPLIED"
	case ModeRowLevel:
		st := &Stats{Table: res.Name, Mode: "ROWLEVEL"}
		ap.ApplyOps(ctx, st, p.b, ops)
		ts.Mode, ts.Inserts, ts.Updates, ts.Deletes = "ROWLEVEL", st.Inserts, st.Updates, st.Deletes
		ts.Chunks = st.Chunks
		if st.Error != "" {
			ts.Status, ts.Error = "FAILED", st.Error
			return ts
		}
		ts.Status = "APPLIED"
	}
	return ts
}
