package mtdiff

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"mtdiff/internal/config"
	"mtdiff/internal/conn"
	"mtdiff/internal/report"
	msync "mtdiff/internal/sync"
)

type syncOpts struct {
	cmp            diffOpts // shared comparison flags (own instance, not the global diff)
	apply          bool
	yes            bool
	batchSize      int
	sampleLimit    int
	noSyncSchema   bool
	structTruncate bool
	rowRewrite     bool
}

var (
	syncFlags connFlags
	syncOpt   syncOpts
)

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Make the destination match the source: missing tables created, extra tables dropped, data and AUTO_INCREMENT state converged (dry-run; --apply writes, destination only)",
	RunE:  syncRunE,
}

func init() {
	syncFlags.bind(syncCmd)
	bindCmpFlags(syncCmd, &syncOpt.cmp)
	f := syncCmd.Flags()
	f.BoolVar(&syncOpt.apply, "apply", false, "perform the writes (default: dry-run, no writes at all)")
	f.BoolVar(&syncOpt.yes, "yes", false, "skip the confirmation prompt")
	f.IntVar(&syncOpt.batchSize, "batch-size", 0, "rows per multi-row INSERT / commit granularity (default 1000)")
	f.IntVar(&syncOpt.sampleLimit, "sample-limit", 0, "sample SQL statements shown per table in a dry-run (default 5)")
	f.BoolVar(&syncOpt.noSyncSchema, "no-sync-schema", false, "do not align the destination table structure before the data sync (default: structure is synced first, shown in the dry run)")
	f.BoolVar(&syncOpt.structTruncate, "allow-structure-truncate", false, "if the in-place structure ALTER fails, truncate the destination table and re-apply the DDL on it (default: the failure stops the table with its data preserved)")
	f.BoolVar(&syncOpt.rowRewrite, "allow-row-rewrite", false, "permit the destructive row rewrite (DELETE+INSERT) for a unique-value swap/cycle/holder (default: the table is refused, because the rewrite fires FK/trigger side effects). It authorizes the row rewrite only: a cross-chunk swap becomes a full-resync plan (TRUNCATE + reload), which the apply executes only when the confirmed plan showed that TRUNCATE — a confirmed row-level plan never escalates to it in the same run")
	rootCmd.AddCommand(syncCmd)
}

// applySyncOpts overlays the sync flags (and the shared comparison flags)
// onto the config. batch-size and sample-limit follow the parallel/
// chunk-size convention: an explicit value (Flags().Changed) must be legal,
// while 0 means "unset, apply default".
func applySyncOpts(cmd *cobra.Command, o *syncOpts, c *config.Config) error {
	if err := applyCmpOptions(cmd, &o.cmp, c); err != nil {
		return err
	}
	if cmd.Flags().Changed("batch-size") {
		if o.batchSize < 1 {
			return fmt.Errorf("--batch-size must be >= 1 (got %d)", o.batchSize)
		}
		c.Opts.BatchSize = o.batchSize
	}
	if cmd.Flags().Changed("sample-limit") {
		if o.sampleLimit < 0 {
			return fmt.Errorf("--sample-limit must be >= 0 (got %d)", o.sampleLimit)
		}
		// explicit: 0 is legal (show no samples) and must survive
		// ApplyDefaults
		v := o.sampleLimit
		c.Opts.SampleLimit = &v
	}
	if cmd.Flags().Changed("no-sync-schema") {
		c.Opts.NoSyncSchema = o.noSyncSchema
	}
	if cmd.Flags().Changed("allow-structure-truncate") {
		c.Opts.AllowStructureTruncate = o.structTruncate
	}
	if cmd.Flags().Changed("allow-row-rewrite") {
		c.Opts.AllowRowRewrite = o.rowRewrite
	}
	return nil
}

type confirmResult int

const (
	confirmProceed confirmResult = iota
	confirmPrompt
	confirmArgErr
)

// confirmDecision is the pure part of the --apply gate: dry-run or --yes
// proceed without asking; a non-terminal stdin cannot confirm, so --apply
// without --yes there is an argument error; otherwise an interactive prompt.
func confirmDecision(apply, yes, tty bool) confirmResult {
	if !apply || yes {
		return confirmProceed
	}
	if !tty {
		return confirmArgErr
	}
	return confirmPrompt
}

// confirmApply gates the write phase. It returns (true, nil) when the writes
// may start, (false, nil) when the user declined the prompt, and an error
// for the non-TTY case (the caller exits 3).
func confirmApply(apply, yes bool, summary string) (bool, error) {
	switch confirmDecision(apply, yes, stdinIsTTY()) {
	case confirmProceed:
		return true, nil
	case confirmArgErr:
		return false, failf(ExitArgErr, "--apply without --yes requires a terminal for confirmation; add --yes to proceed")
	case confirmPrompt:
		fmt.Fprintf(os.Stderr, "%s\nProceed? [y/N] ", summary)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		line = strings.ToLower(strings.TrimSpace(line))
		return line == "y" || line == "yes", nil
	}
	return true, nil
}

// syncRunE implements the sync command: a read-only pre-pass finds what
// differs, the plan is printed (dry-run) or — with --apply and a
// confirmation — executed on the destination only, then the synced tables
// are re-compared to verify the result.
func syncRunE(cmd *cobra.Command, _ []string) error {
	cfg, err := syncFlags.build(makePrompt())
	if err != nil {
		return err
	}
	if err := applySyncOpts(cmd, &syncOpt, cfg); err != nil {
		return failf(ExitArgErr, "%v", err)
	}
	// Same order as build(): validate before defaults rewrite unset values.
	if err := cfg.Validate(); err != nil {
		return failf(ExitArgErr, "%v", err)
	}
	cfg.ApplyDefaults()

	ctx := context.Background()
	connectCtx, cancelConnect := context.WithTimeout(ctx, 2*time.Minute)
	src, err := conn.OpenSide(connectCtx, "src", cfg.Src, cfg.Opts.MaxAllowedPacket, cfg.Opts.Parallel, cfg.Opts.AllowUnenforcedReadOnly)
	if err != nil {
		cancelConnect()
		return failf(ExitRuntimeErr, "%v", err)
	}
	defer src.Close()
	dst, err := conn.OpenSide(connectCtx, "dst", cfg.Dst, cfg.Opts.MaxAllowedPacket, cfg.Opts.Parallel, cfg.Opts.AllowUnenforcedReadOnly)
	if err != nil {
		cancelConnect()
		return failf(ExitRuntimeErr, "%v", err)
	}
	defer dst.Close()
	cancelConnect()

	tables, extra, err := resolveSyncTables(ctx, cfg, src, dst)
	if err != nil {
		return err
	}
	if !syncOpt.cmp.jsonOut {
		fmt.Printf("src: %s (%s)\n", src.Masked(), src.Version)
		fmt.Printf("dst: %s (%s)\n", dst.Masked(), dst.Version)
	}

	runner := msync.NewRunner(src, dst, msync.Options{
		Cmp:                    buildComparerOpts(cfg),
		Batch:                  cfg.Opts.BatchSize,
		SampleLimit:            cfg.SampleLimitOr(5),
		MaxPacket:              cfg.Opts.MaxAllowedPacket,
		SyncSchema:             !cfg.Opts.NoSyncSchema,
		AllowStructureTruncate: cfg.Opts.AllowStructureTruncate,
		AllowRowRewrite:        cfg.Opts.AllowRowRewrite,
		Progress:               progressLog, // forwarded to the comparer (pre-pass + verification) by NewRunner
	})
	results, err := runner.PrePass(ctx, tables)
	if err != nil {
		return failf(ExitRuntimeErr, "%v", err)
	}
	// Destination-only tables (whole-database mode only): the destination
	// is a disposable copy of the source, so they are converged away with
	// a DROP TABLE. In dry runs they are listed, never executed.
	dropPlans := make([]msync.TableSync, 0, len(extra))
	for _, t := range extra {
		dropPlans = append(dropPlans, msync.DropPlanFor(t))
	}

	// Dry-run: show the plan for every table, write nothing.
	if !syncOpt.apply {
		syncResults := make([]msync.TableSync, 0, len(results)+len(dropPlans))
		for _, r := range results {
			syncResults = append(syncResults, runner.PlanTable(ctx, r))
		}
		syncResults = append(syncResults, dropPlans...)
		// a misconfiguration (keyless + --where, ignoring a key column) is
		// an argument error in the dry run too, not a runtime failure
		if err := planArgErr(syncResults); err != nil {
			return err
		}
		printSyncReport(syncResults, false)
		return syncDryRunExit(syncResults)
	}

	// Apply: plan everything first (the plans drive the confirmation
	// summary), then confirm before any write connection exists. The
	// plan here is the PREFLIGHT, not a decision shortcut: it runs the
	// same row planning the apply re-runs, so the destructive scope the
	// user confirms (a full resync with its TRUNCATE, a row rewrite) is
	// computed from a real plan. ApplyTable re-plans right before
	// writing and may only stay within the confirmed scope — the
	// preflight's extra scan is deliberate (safety over speed).
	dataPlans := make([]msync.TableSync, len(results))
	for i, r := range results {
		dataPlans[i] = runner.PlanTable(ctx, r)
	}
	allPlans := append(append([]msync.TableSync{}, dataPlans...), dropPlans...)
	// a plan that is an argument error (e.g. keyless + --where) stops the
	// run before any write
	if err := planArgErr(allPlans); err != nil {
		return err
	}
	if allSkip(allPlans) {
		// nothing to write: no confirmation prompt and no write
		// connection at all
		printSyncReport(allPlans, false)
		return nil
	}
	proceed, err := confirmApply(syncOpt.apply, syncOpt.yes, syncSummary(allPlans, cfg.Opts.AllowStructureTruncate))
	if err != nil {
		return err
	}
	if !proceed {
		return failf(ExitArgErr, "aborted by the user; no writes were made")
	}
	w, err := conn.OpenWriter(ctx, "dst", cfg.Dst, cfg.Opts.MaxAllowedPacket)
	if err != nil {
		return failf(ExitRuntimeErr, "%v", err)
	}
	defer w.Close()

	ap := &msync.Applier{
		W:        w,
		Src:      src,
		Batch:    cfg.Opts.BatchSize,
		MaxBytes: batchByteBudget(cfg.Opts.MaxAllowedPacket),
		Progress: progressLog,
	}
	syncResults := make([]msync.TableSync, 0, len(allPlans))
	synced := make([]string, 0)
	for i, r := range results {
		if dataPlans[i].Mode == "SKIP" {
			syncResults = append(syncResults, dataPlans[i])
			continue
		}
		// dataPlans[i] is the CONFIRMED plan (preflight): the apply may
		// shrink it but not expand its destructive scope
		ts := runner.ApplyTable(ctx, r, ap, dataPlans[i])
		syncResults = append(syncResults, ts)
		if ts.Status == "APPLIED" {
			synced = append(synced, ts.Name)
		}
	}
	// Drop the destination-only tables (whole-database mode). A dropped
	// table is verified by its absence, not by a re-comparison.
	for _, p := range dropPlans {
		ts := runner.ApplyDrop(ctx, p.Name, ap)
		syncResults = append(syncResults, ts)
	}
	// verify: re-compare exactly the tables that were written
	if len(synced) > 0 {
		verified, err := runner.Verify(ctx, synced)
		if err != nil {
			return failf(ExitRuntimeErr, "verification: %v", err)
		}
		vmap := make(map[string]string, len(verified))
		for _, v := range verified {
			vmap[v.Name] = v.Status
		}
		for i := range syncResults {
			if s, ok := vmap[syncResults[i].Name]; ok {
				syncResults[i].Verified = s
			}
		}
	}
	// and the table state (the next AUTO_INCREMENT value): of every
	// table that was written, and of STATE plans that write nothing
	// (an unfixable divergence — the destination's counter above the
	// source's — is reported through the check, not skipped). A
	// backend without the column reports "" — out of scope, never a
	// failure.
	for i := range syncResults {
		s := &syncResults[i]
		if s.Mode == "DROP" {
			continue
		}
		if s.Status == "APPLIED" || s.Mode == "STATE" {
			s.StateVerified = runner.VerifyState(ctx, s.Name)
		}
	}
	printSyncReport(syncResults, true)
	return syncApplyExit(syncResults)
}

// resolveSyncTables picks the tables the sync converges. With --tables it
// is strictly those (and nothing else — that mode never plans drops,
// even for tables the destination has extra). Otherwise it is
// whole-database mode: the expected set is the source's BASE TABLE set,
// and the destination's base tables the source lacks (minus the
// excluded ones) come back as extras, each planning a DROP TABLE.
// --where disables drop planning: a row-level filter must not drop
// whole tables.
func resolveSyncTables(ctx context.Context, cfg *config.Config, src, dst *conn.Side) (tables, extra []string, err error) {
	if len(cfg.Opts.Tables) > 0 {
		return cfg.Opts.Tables, nil, nil
	}
	var srcTables, dstTables []string
	if err := src.WithControl(ctx, func(q conn.Queryer) error {
		var err error
		srcTables, err = conn.ListBaseTables(ctx, q)
		return err
	}); err != nil {
		return nil, nil, failf(ExitRuntimeErr, "src: %v", err)
	}
	if err := dst.WithControl(ctx, func(q conn.Queryer) error {
		var err error
		dstTables, err = conn.ListBaseTables(ctx, q)
		return err
	}); err != nil {
		return nil, nil, failf(ExitRuntimeErr, "dst: %v", err)
	}
	tables, extra = syncTableSets(srcTables, dstTables, cfg.Opts.ExcludeTables, cfg.Opts.Where == "")
	if len(tables) == 0 && len(extra) == 0 {
		return nil, nil, failf(ExitArgErr, "no tables to sync (the source database is empty; use --tables to specify)")
	}
	return tables, extra, nil
}

// syncTableSets splits the two databases' base-table lists into the set
// to sync (the source's tables minus the excluded) and the extras (the
// destination's tables the source lacks, minus the excluded). allowDrops
// is false under --where: a filtered run never plans whole-table drops.
func syncTableSets(srcTables, dstTables, exclude []string, allowDrops bool) (tables, extra []string) {
	excl := make(map[string]bool, len(exclude))
	for _, t := range exclude {
		excl[t] = true
	}
	srcSet := make(map[string]bool, len(srcTables))
	for _, t := range srcTables {
		srcSet[t] = true
	}
	for _, t := range srcTables {
		if !excl[t] {
			tables = append(tables, t)
		}
	}
	if !allowDrops {
		return tables, nil
	}
	for _, t := range dstTables {
		if !srcSet[t] && !excl[t] {
			extra = append(extra, t)
		}
	}
	return tables, extra
}

// batchByteBudget is the rendered-bytes limit for one multi-row INSERT:
// half the configured max_allowed_packet when set, otherwise 4 MiB.
func batchByteBudget(maxAllowedPacket int) int {
	if maxAllowedPacket > 0 {
		return maxAllowedPacket / 2
	}
	return 4 << 20
}

// planArgErr reports the first misconfigured plan (keyless + --where,
// --ignore-columns naming a key column) as an argument error: no flag
// combination can make that table syncable. The sync package marks these
// plans explicitly (TableSync.ArgErr) — no string matching across the
// package boundary.
func planArgErr(plans []msync.TableSync) error {
	for _, p := range plans {
		if p.Mode == "ERROR" && p.Status == "FAILED" && p.ArgErr {
			return failf(ExitArgErr, "%s: %s", p.Name, p.Error)
		}
	}
	return nil
}

// allSkip reports whether every plan is a SKIP (nothing would be written).
func allSkip(plans []msync.TableSync) bool {
	if len(plans) == 0 {
		return false
	}
	for _, p := range plans {
		if p.Mode != "SKIP" {
			return false
		}
	}
	return true
}

// syncSummary renders the pre-write confirmation: the count of what the
// sync would do, and the destructive statements (DROP TABLE, DROP
// COLUMN, DROP INDEX, DROP PRIMARY KEY) listed separately — they are the
// irreversible ones and must not be hidden behind "N statements will be
// executed". The destructive row rewrites (DELETE+INSERT to free a
// unique slot) get their own section: they touch rows the user did not
// ask to change, and they run only because this very summary showed
// them (P0-3).
//
// allowStructureTruncate is the RESOLVED value (CLI flag over YAML
// config over default) — the same value the runner uses to decide. The
// summary must never read the CLI global: a YAML
// allow_structure_truncate: true with the flag unset must warn here,
// or the user would confirm a run whose apply may TRUNCATE a table
// without the summary ever saying so.
func syncSummary(plans []msync.TableSync, allowStructureTruncate bool) string {
	var full, row, skip, create, drop, state, stateNote, fail int
	var names, destructive, rewrites []string
	for _, p := range plans {
		switch p.Mode {
		case "SKIP":
			skip++
		case "FULL":
			full++
			if p.SchemaChanged {
				names = append(names, p.Name+" (structure+resync)")
			} else {
				names = append(names, p.Name+" (truncate+resync)")
			}
		case "ROWLEVEL":
			row++
			names = append(names, p.Name)
			if p.Rewrites > 0 {
				rewrites = append(rewrites, fmt.Sprintf("%s (%d row group(s))", p.Name, p.Rewrites))
			}
		case "CREATE":
			create++
			names = append(names, p.Name+" (create table)")
		case "DROP":
			drop++
		case "STATE":
			state++
			if p.StateNote != "" {
				// an unfixable divergence: reported, but nothing is written
				stateNote++
				names = append(names, p.Name+" (state diverged, not fixable by a row-level sync)")
			} else {
				names = append(names, p.Name+" (auto-increment state)")
			}
		default:
			fail++
		}
		for _, s := range p.SchemaSQL {
			if msync.DestructiveDDL(s) {
				destructive = append(destructive, s)
			}
		}
	}
	summary := fmt.Sprintf("sync would modify %d of %d tables (%d truncate+resync, %d row-level, %d create, %d drop, %d state, %d failed)",
		full+row+create+drop+state-stateNote, len(plans), full, row, create, drop, state, fail)
	if stateNote > 0 {
		summary += fmt.Sprintf("; %d table state divergence(s) reported but not fixable by a row-level sync", stateNote)
	}
	if len(names) > 0 {
		summary += ": " + strings.Join(names, ", ")
	}
	if allowStructureTruncate {
		summary += "\nNOTE: allow_structure_truncate is set (flag or config) — if an in-place structure ALTER fails, the destination table is TRUNCATED and the DDL re-applied on the empty table"
	}
	if len(destructive) > 0 {
		summary += fmt.Sprintf("\nDESTRUCTIVE: %d irreversible statement(s) will be executed:", len(destructive))
		for _, s := range destructive {
			summary += "\n  " + s
		}
	}
	if len(rewrites) > 0 {
		summary += fmt.Sprintf("\nDESTRUCTIVE ROW REWRITE: %d table(s) will DELETE and re-INSERT whole row groups to free unique slots (FK/trigger side effects fire on rows the sync did not otherwise touch):", len(rewrites))
		for _, s := range rewrites {
			summary += "\n  " + s
		}
	}
	return summary
}

func printSyncReport(res []msync.TableSync, applied bool) {
	if syncOpt.cmp.jsonOut {
		report.SyncJSON(os.Stdout, applied, res)
	} else {
		report.SyncText(os.Stdout, applied, res)
	}
}

func syncDryRunExit(res []msync.TableSync) error {
	anyFailed, anyPending := false, false
	for _, r := range res {
		switch r.Status {
		case "FAILED":
			anyFailed = true
		case "PLANNED":
			anyPending = true
		}
	}
	switch {
	case anyFailed:
		return &ExitError{Code: ExitRuntimeErr}
	case anyPending:
		return &ExitError{Code: ExitDifferent}
	}
	return nil
}

func syncApplyExit(res []msync.TableSync) error {
	anyFailed, anyDiff := false, false
	for _, r := range res {
		switch {
		case r.Status == "FAILED":
			anyFailed = true
		case r.Verified == "DIFFERENT" || r.Verified == "ERROR":
			anyDiff = true
		// the rows converged but the table state (AUTO_INCREMENT) did not
		case r.StateVerified == "DIFFERENT":
			anyDiff = true
		}
	}
	switch {
	case anyFailed:
		return &ExitError{Code: ExitRuntimeErr}
	case anyDiff:
		return &ExitError{Code: ExitDifferent}
	}
	return nil
}
