package mtdiff

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"mtdiff/internal/compare"
	"mtdiff/internal/config"
	"mtdiff/internal/conn"
	"mtdiff/internal/normalize"
	"mtdiff/internal/report"
)

type diffOpts struct {
	tables, excludeTables string
	key                   string
	where                 string
	ignoreColumns         string
	parallel              int
	chunkSize             int
	drillLimit            int
	maxAllowedPacket      int
	tolerance             float64
	snapshot              bool
	drill                 bool
	noTrim                bool
	foldCase              bool
	normalizeJSON         bool
	allowTZSwap           bool
	strictTypes           bool
	secure                bool
	jsonOut               bool
}

var (
	diffFlags connFlags
	diff      diffOpts
)

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// applyCmpOptions overlays explicit CLI comparison options onto the config
// (which may have come from a YAML file). Zero/false/empty flag values mean
// "not given" — except parallel and chunk-size, where 0 is not a legal
// value: an explicit 0 (detected via Flags().Changed) is an argument error,
// not "use default". The sync subcommand reuses it via its own diffOpts
// instance.
func applyCmpOptions(cmd *cobra.Command, o *diffOpts, c *config.Config) error {
	if cmd.Flags().Changed("parallel") {
		if o.parallel < 1 {
			return fmt.Errorf("--parallel must be >= 1 (got %d)", o.parallel)
		}
		c.Opts.Parallel = o.parallel
	}
	if cmd.Flags().Changed("chunk-size") {
		if o.chunkSize < 1 {
			return fmt.Errorf("--chunk-size must be >= 1 (got %d)", o.chunkSize)
		}
		c.Opts.ChunkSize = o.chunkSize
	}
	// Unlike parallel/chunk-size above, an explicit 0 is meaningful here
	// (0 = exact floats / default packet size / default drill limit), so
	// "was it given" is the question — Flags().Changed, not a zero check,
	// or a YAML value would survive a flag that only wants to reset it.
	if cmd.Flags().Changed("tolerance") {
		c.Opts.Tolerance = o.tolerance
	}
	if cmd.Flags().Changed("drill-limit") {
		c.Opts.DrillLimit = o.drillLimit
	}
	if cmd.Flags().Changed("max-allowed-packet") {
		c.Opts.MaxAllowedPacket = o.maxAllowedPacket
	}
	if o.tables != "" {
		c.Opts.Tables = splitList(o.tables)
	}
	if o.excludeTables != "" {
		c.Opts.ExcludeTables = splitList(o.excludeTables)
	}
	if o.key != "" {
		c.Opts.Key = splitList(o.key)
	}
	if o.where != "" {
		c.Opts.Where = o.where
	}
	if o.ignoreColumns != "" {
		c.Opts.IgnoreColumns = splitList(o.ignoreColumns)
	}
	if o.snapshot {
		c.Opts.Snapshot = true
	}
	if o.drill {
		c.Opts.Drill = true
	}
	if o.noTrim {
		c.Opts.NoTrim = true
	}
	if o.foldCase {
		c.Opts.FoldCase = true
	}
	if o.normalizeJSON {
		c.Opts.NormalizeJSON = true
	}
	if o.allowTZSwap {
		c.Opts.AllowTZSwap = true
	}
	if o.strictTypes {
		c.Opts.StrictTypes = true
	}
	if o.secure {
		c.Opts.Secure = true
	}
	return nil
}

func buildComparerOpts(c *config.Config) compare.Options {
	ignore := make(map[string]bool, len(c.Opts.IgnoreColumns))
	for _, col := range c.Opts.IgnoreColumns {
		ignore[col] = true
	}
	return compare.Options{
		Parallel:   c.Opts.Parallel,
		ChunkSize:  c.Opts.ChunkSize,
		Snapshot:   c.Opts.Snapshot,
		Secure:     c.Opts.Secure,
		Drill:      c.Opts.Drill,
		DrillLimit: c.Opts.DrillLimit,
		Key:        c.Opts.Key,
		Where:      c.Opts.Where,
		Compat: conn.CompatOpts{
			Strict:      c.Opts.StrictTypes,
			AllowTZSwap: c.Opts.AllowTZSwap,
		},
		Normalize: normalize.Options{
			Tolerance:     c.Opts.Tolerance,
			TrimTrailing:  !c.Opts.NoTrim,
			FoldCase:      c.Opts.FoldCase,
			NormalizeJSON: c.Opts.NormalizeJSON,
			AllowTZSwap:   c.Opts.AllowTZSwap,
			IgnoreCols:    ignore,
		},
	}
}

// diffRunE implements the diff command. It is shared with the root command so
// that `mtdiff --src ... --dst ...` works without the explicit subcommand.
func diffRunE(cmd *cobra.Command, _ []string) error {
	cfg, err := diffFlags.build(makePrompt())
	if err != nil {
		return err
	}
	if err := applyCmpOptions(cmd, &diff, cfg); err != nil {
		return failf(ExitArgErr, "%v", err)
	}
	// Same order as build(): validate before defaults rewrite unset values.
	if err := cfg.Validate(); err != nil {
		return failf(ExitArgErr, "%v", err)
	}
	cfg.ApplyDefaults()
	ctx := context.Background()
	// The connect phase is bounded: a broken port forward or a stalled
	// handshake must fail fast, not hang the whole run. Scans use ctx
	// (unbounded) because large tables legitimately take a long time.
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

	tables, err := resolveTables(ctx, cfg, src, dst)
	if err != nil {
		return err
	}
	if !diff.jsonOut {
		fmt.Printf("src: %s (%s)\n", src.Masked(), src.Version)
		fmt.Printf("dst: %s (%s)\n", dst.Masked(), dst.Version)
	}
	cmpOpts := buildComparerOpts(cfg)
	cmpOpts.Progress = progressLog
	comparer := compare.NewComparer(cmpOpts)
	results, err := comparer.Compare(ctx, src, dst, tables)
	if err != nil {
		return failf(ExitRuntimeErr, "%v", err)
	}
	if diff.jsonOut {
		report.JSON(os.Stdout, results)
	} else {
		report.Text(os.Stdout, results)
	}
	anyErr, anyDiff := false, false
	for _, r := range results {
		if r.Status == "ERROR" {
			anyErr = true
		}
		if r.Differing() {
			anyDiff = true
		}
	}
	switch {
	case anyErr:
		return &ExitError{Code: ExitRuntimeErr}
	case anyDiff:
		return &ExitError{Code: ExitDifferent}
	}
	return nil
}

var diffCmd = &cobra.Command{
	Use:   "diff",
	Short: "Compare tables between two endpoints",
	RunE:  diffRunE,
}

// resolveTables picks the tables to compare: --tables if given, otherwise the
// intersection of both sides minus --exclude-tables.
func resolveTables(ctx context.Context, cfg *config.Config, src, dst *conn.Side) ([]string, error) {
	if len(cfg.Opts.Tables) > 0 {
		return cfg.Opts.Tables, nil
	}
	srcTables, err := conn.ListTables(ctx, src.Ctl())
	if err != nil {
		return nil, failf(ExitRuntimeErr, "src: %v", err)
	}
	dstTables, err := conn.ListTables(ctx, dst.Ctl())
	if err != nil {
		return nil, failf(ExitRuntimeErr, "dst: %v", err)
	}
	excl := make(map[string]bool, len(cfg.Opts.ExcludeTables))
	for _, t := range cfg.Opts.ExcludeTables {
		excl[t] = true
	}
	set := make(map[string]bool, len(dstTables))
	for _, t := range dstTables {
		set[t] = true
	}
	var out []string
	for _, t := range srcTables {
		if set[t] && !excl[t] {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil, failf(ExitArgErr, "no common tables to compare (use --tables to specify)")
	}
	return out, nil
}

// bindCmpFlags registers the comparison flags (bound to the given diffOpts
// instance) on a command. The same flags are registered on the `diff`
// subcommand, the root command (which runs diff directly, so `mtdiff
// --src ...` and `mtdiff diff --src ...` are equivalent) and the `sync`
// subcommand (which reuses the whole comparison surface).
func bindCmpFlags(cmd *cobra.Command, o *diffOpts) {
	f := cmd.Flags()
	f.StringVar(&o.tables, "tables", "", "tables to compare, comma-separated (default: all common tables)")
	f.StringVar(&o.excludeTables, "exclude-tables", "", "tables to skip, comma-separated")
	f.StringVar(&o.key, "key", "", "key columns to chunk by, comma-separated (default: PK/unique)")
	f.StringVar(&o.where, "where", "", "extra WHERE filter applied to both sides")
	f.StringVar(&o.ignoreColumns, "ignore-columns", "", "columns to exclude from comparison, comma-separated")
	f.IntVar(&o.parallel, "parallel", 0, "concurrent chunk scans (default 4)")
	f.IntVar(&o.chunkSize, "chunk-size", 0, "target rows per chunk (default 10000)")
	f.IntVar(&o.drillLimit, "drill-limit", 0, "max example rows per differing chunk (default 10)")
	f.IntVar(&o.maxAllowedPacket, "max-allowed-packet", 0, "max packet size in bytes (default: driver limit)")
	f.Float64Var(&o.tolerance, "tolerance", 0, "float/double comparison tolerance (0 = exact)")
	f.BoolVar(&o.snapshot, "snapshot", false, "scan each table under a consistent snapshot (slower, stable under writes)")
	f.BoolVar(&o.drill, "drill", false, "show example differing rows (uses --drill-limit)")
	f.BoolVar(&o.noTrim, "no-trim", false, "do not trim trailing spaces from strings")
	f.BoolVar(&o.foldCase, "fold-case", false, "compare strings case-insensitively")
	f.BoolVar(&o.normalizeJSON, "normalize-json", false, "canonicalize JSON values (sorted keys, normalized numbers)")
	f.BoolVar(&o.allowTZSwap, "allow-tz-swap", false, "allow DATETIME/TIMESTAMP type swaps, compared as UTC instants")
	f.BoolVar(&o.strictTypes, "strict-types", false, "require byte-identical column types")
	f.BoolVar(&o.secure, "secure", false, "use 128-bit fingerprints instead of 64-bit")
	f.BoolVar(&o.jsonOut, "json", false, "output JSON report")
}

func init() {
	diffFlags.bind(diffCmd)
	bindCmpFlags(diffCmd, &diff)
	rootCmd.AddCommand(diffCmd)
}
