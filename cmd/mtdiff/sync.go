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
	cmp          diffOpts // shared comparison flags (own instance, not the global diff)
	apply        bool
	yes          bool
	batchSize    int
	sampleLimit  int
	noSyncSchema bool
}

var (
	syncFlags connFlags
	syncOpt   syncOpts
)

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Make the destination match the source (default: dry-run; --apply writes, destination only)",
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
		c.Opts.SampleLimit = o.sampleLimit
	}
	if cmd.Flags().Changed("no-sync-schema") {
		c.Opts.NoSyncSchema = o.noSyncSchema
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
	src, err := conn.OpenSide(connectCtx, "src", cfg.Src, cfg.Opts.MaxAllowedPacket, cfg.Opts.Parallel)
	if err != nil {
		cancelConnect()
		return failf(ExitRuntimeErr, "%v", err)
	}
	defer src.Close()
	dst, err := conn.OpenSide(connectCtx, "dst", cfg.Dst, cfg.Opts.MaxAllowedPacket, cfg.Opts.Parallel)
	if err != nil {
		cancelConnect()
		return failf(ExitRuntimeErr, "%v", err)
	}
	defer dst.Close()
	cancelConnect()

	tables, err := resolveTables(ctx, cfg, src, dst)
	if err != nil {
		return err
	}
	if !syncOpt.cmp.jsonOut {
		fmt.Printf("src: %s (%s)\n", src.Masked(), src.Version)
		fmt.Printf("dst: %s (%s)\n", dst.Masked(), dst.Version)
	}

	runner := msync.NewRunner(src, dst, msync.Options{
		Cmp:         buildComparerOpts(cfg),
		Batch:       cfg.Opts.BatchSize,
		SampleLimit: cfg.Opts.SampleLimit,
		MaxPacket:   cfg.Opts.MaxAllowedPacket,
		SyncSchema:  !cfg.Opts.NoSyncSchema,
		Progress:    progressLog, // forwarded to the comparer (pre-pass + verification) by NewRunner
	})
	results, err := runner.PrePass(ctx, tables)
	if err != nil {
		return failf(ExitRuntimeErr, "%v", err)
	}

	// Dry-run: show the plan for every table, write nothing.
	if !syncOpt.apply {
		syncResults := make([]msync.TableSync, 0, len(results))
		for _, r := range results {
			syncResults = append(syncResults, runner.PlanTable(ctx, r))
		}
		// a misconfiguration (keyless + --where, ignoring a key column) is
		// an argument error in the dry run too, not a runtime failure
		if err := planArgErr(syncResults); err != nil {
			return err
		}
		printSyncReport(syncResults, false)
		return syncDryRunExit(syncResults)
	}

	// Apply: decide the plan for everything first (the plans drive the
	// confirmation summary), then confirm before any write connection
	// exists. Only the decision is computed here (no row re-scan):
	// ApplyTable re-plans and rescans right before writing, so planning
	// the ops now would scan the differing chunks twice for nothing.
	plans := make([]msync.TableSync, len(results))
	for i, r := range results {
		plans[i] = runner.PlanSummary(ctx, r)
	}
	// a plan that is an argument error (e.g. keyless + --where) stops the
	// run before any write
	if err := planArgErr(plans); err != nil {
		return err
	}
	if allSkip(plans) {
		// nothing to write: no confirmation prompt and no write
		// connection at all
		printSyncReport(plans, false)
		return nil
	}
	proceed, err := confirmApply(syncOpt.apply, syncOpt.yes, syncSummary(plans))
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
	syncResults := make([]msync.TableSync, 0, len(plans))
	synced := make([]string, 0)
	for i, r := range results {
		if plans[i].Mode == "SKIP" {
			syncResults = append(syncResults, plans[i])
			continue
		}
		ts := runner.ApplyTable(ctx, r, ap)
		syncResults = append(syncResults, ts)
		if ts.Status == "APPLIED" {
			synced = append(synced, ts.Name)
		}
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
	printSyncReport(syncResults, true)
	return syncApplyExit(syncResults)
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

func syncSummary(plans []msync.TableSync) string {
	var full, row, skip, fail int
	var names []string
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
		default:
			fail++
		}
	}
	summary := fmt.Sprintf("sync would modify %d of %d tables (%d truncate+resync, %d row-level, %d failed)",
		full+row, len(plans), full, row, fail)
	if len(names) > 0 {
		summary += ": " + strings.Join(names, ", ")
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
