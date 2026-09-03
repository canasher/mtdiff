// Package compare orchestrates table comparison: introspect, plan chunks,
// stream both sides in parallel, and fold chunk digests into a verdict.
package compare

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"golang.org/x/sync/errgroup"

	"mtdiff/internal/chunk"
	"mtdiff/internal/conn"
	mhash "mtdiff/internal/hash"
	"mtdiff/internal/normalize"
)

// Options control comparison behavior.
type Options struct {
	Parallel   int
	ChunkSize  int
	Snapshot   bool
	Secure     bool
	Drill      bool // produce example rows for differing chunks
	DrillLimit int
	Compat     conn.CompatOpts
	Normalize  normalize.Options
	Key        []string // explicit key override (replaces PK/unique selection)
	Where      string
	// Progress receives long-running phase updates (per-table start, chunk
	// scan percentages) so multi-hour runs on huge tables are not a silent
	// process. nil = no progress output. The caller should write to stderr:
	// the report (text or JSON) goes to stdout and must stay untouched.
	Progress func(format string, args ...any)
}

// ChunkDiff describes one differing chunk.
type ChunkDiff struct {
	ID  int
	Lo  string
	Hi  string
	Src string
	Dst string
}

// TableResult is the outcome for one table.
type TableResult struct {
	Name       string
	SrcRows    int64
	DstRows    int64
	Status     string // OK | DIFFERENT | ERROR
	Error      string
	Chunks     int
	DiffChunks []ChunkDiff
	Rows       []RowDiff
	SrcFP      string
	DstFP      string
	Warnings   []string
}

// Differing reports whether the table is known-different (not merely errored).
func (r TableResult) Differing() bool { return r.Status == "DIFFERENT" }

// Comparer runs comparisons.
type Comparer struct {
	opts Options
}

func NewComparer(o Options) *Comparer { return &Comparer{opts: o} }

// Compare compares the given tables and returns one result per table.
func (c *Comparer) Compare(ctx context.Context, src, dst *conn.Side, tables []string) ([]TableResult, error) {
	results := make([]TableResult, 0, len(tables))
	for _, t := range tables {
		results = append(results, c.compareTable(ctx, src, dst, t))
	}
	return results, nil
}

func (c *Comparer) compareTable(ctx context.Context, src, dst *conn.Side, name string) (res TableResult) {
	res.Name, res.Status = name, "OK"
	fail := func(format string, args ...any) TableResult {
		res.Status = "ERROR"
		res.Error = fmt.Sprintf(format, args...)
		return res
	}

	srcSchema, dstSchema, warns, err := PrepareSchemas(ctx, src, dst, name, c.opts.Key, c.opts.Normalize.IgnoreCols, c.opts.Compat)
	if err != nil {
		return fail("%v", err)
	}
	res.Warnings = warns

	srcNorm := normalize.NewNormalizer(srcSchema.Cols, c.opts.Normalize)
	dstNorm := normalize.NewNormalizer(dstSchema.Cols, c.opts.Normalize)

	srcTotal, err := c.count(ctx, src, name)
	if err != nil {
		return fail("src count: %v", err)
	}
	dstTotal, err := c.count(ctx, dst, name)
	if err != nil {
		return fail("dst count: %v", err)
	}
	res.SrcRows, res.DstRows = srcTotal, dstTotal

	if skipRowScan(srcTotal, dstTotal, c.opts.Drill) {
		// Row counts differ, so the table is definitively DIFFERENT:
		// skip planning and the row scans (which on a large table means
		// streaming both sides in full). Fingerprints and chunk-level
		// detail are uninformative here. With --drill the operator paid
		// for row-level detail, so scan as before.
		res.Status = "DIFFERENT"
		if c.opts.Progress != nil {
			c.opts.Progress("%-24s: %d vs %d rows, count mismatch (scan skipped)", name, srcTotal, dstTotal)
		}
		return res
	}

	// Usable keys must agree for the chunk plan to be shared: the planner
	// renders the source's key bounds into predicates that run against the
	// destination's key columns, so a source with a key and a destination
	// without one (or the reverse) cannot be chunked by key — planning the
	// keyed side's bounds against the keyless side's schema is an
	// index-out-of-range panic, not an error. Fall back to keyless
	// whole-table mode on both sides: one unbounded chunk and
	// order-independent multiset fingerprints, so identical data still
	// compares equal and a real difference is still reported.
	keyMismatch := (len(srcSchema.Key) > 0) != (len(dstSchema.Key) > 0)
	if keyMismatch {
		res.Warnings = append(res.Warnings,
			"usable keys disagree (one side keyed, the other keyless): comparing as keyless whole-table multisets")
	}

	var chunks []chunk.Chunk
	if srcTotal > 0 || dstTotal > 0 {
		p := chunk.Planner{
			Table:       name,
			KeyCols:     srcSchema.Key,
			KeyFamilies: KeyFamilies(srcSchema),
			ChunkSize:   c.opts.ChunkSize,
			Where:       c.opts.Where,
		}
		if keyMismatch {
			p.KeyCols = nil
			p.KeyFamilies = nil
		}
		chunks, err = p.Plan(ctx, src.Ctl(), srcTotal)
		if err != nil {
			return fail("plan: %v", err)
		}
	}
	res.Chunks = len(chunks)
	// The ordered accumulator needs a deterministic row order per chunk,
	// which only the keyless fallback (no key to order by on one side)
	// must give up: both sides compare as multisets instead.
	ordered := len(srcSchema.Key) > 0 && !keyMismatch
	sc := chunk.NewScanner(srcNorm, ordered)
	dc := chunk.NewScanner(dstNorm, ordered)

	if c.opts.Progress != nil {
		c.opts.Progress("%-24s: comparing %d chunks (src %d / dst %d rows)", name, len(chunks), srcTotal, dstTotal)
	}

	// Both sides have independent pools, scanners and schemas, so scan
	// them concurrently: wall time is max(src, dst) instead of src+dst.
	var (
		srcDigests, dstDigests map[int]mhash.ChunkDigest
		srcErr, dstErr         error
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		srcDigests, srcErr = c.scanSide(gctx, src, sc, srcSchema, chunks, name)
		return srcErr
	})
	g.Go(func() error {
		dstDigests, dstErr = c.scanSide(gctx, dst, dc, dstSchema, chunks, name)
		return dstErr
	})
	_ = g.Wait()
	if err := pickScanError(srcErr, dstErr); err != nil {
		return fail("%v", err)
	}

	byID := foldDigests(&res, chunks, srcDigests, dstDigests, srcTotal, dstTotal, ordered, c.opts.Secure)
	if c.opts.Drill && len(res.DiffChunks) > 0 {
		dd := &DrillDown{}
		truncatedReported := false
		for _, dc := range res.DiffChunks {
			if len(res.Rows) >= c.opts.DrillLimit {
				break
			}
			rows, truncated, err := dd.Diff(ctx, src, dst, srcSchema, dstSchema, srcNorm, dstNorm, byID[dc.ID], c.opts.Where, c.opts.DrillLimit-len(res.Rows))
			if err != nil {
				return fail("drill-down chunk %d: %v", dc.ID, err)
			}
			res.Rows = append(res.Rows, rows...)
			if truncated && !truncatedReported {
				truncatedReported = true
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"drill-down: row buffering capped at %d rows per side; row-level results are a sample (truncated)", drillMaxRows))
			}
		}
	}
	return res
}

// scanSide streams every chunk of one side, in parallel. In snapshot mode the
// whole table is scanned on one connection inside a consistent-snapshot
// transaction (serial, but immune to concurrent writes).
func (c *Comparer) scanSide(ctx context.Context, side *conn.Side, sc *chunk.Scanner, schema *conn.Schema, chunks []chunk.Chunk, table string) (map[int]mhash.ChunkDigest, error) {
	out := make(map[int]mhash.ChunkDigest, len(chunks))
	if len(chunks) == 0 {
		return out, nil
	}
	// chunk progress: ~10 lines per side over the whole scan (throttled by
	// step), so a multi-hour scan of a huge table is not a silent process
	var done atomic.Int64
	step := len(chunks) / 10
	if step < 1 {
		step = 1
	}
	report := func() {
		if c.opts.Progress == nil {
			return
		}
		n := done.Add(1)
		if n%int64(step) == 0 {
			c.opts.Progress("  %-24s %s scan %3d%% (%d/%d chunks)", table, side.Name, 100*n/int64(len(chunks)), n, len(chunks))
		}
	}
	if c.opts.Snapshot {
		cn, err := side.AcquireScan(ctx)
		if err != nil {
			return nil, err
		}
		defer cn.Close()
		if _, err := cn.ExecContext(ctx, "START TRANSACTION WITH CONSISTENT SNAPSHOT"); err != nil {
			return nil, fmt.Errorf("%s %s: snapshot: %w", side.Name, table, err)
		}
		for _, ch := range chunks {
			d, err := sc.Scan(ctx, cn, schema, ch, c.opts.Where)
			if err != nil {
				return nil, err
			}
			out[ch.ID] = d
			report()
		}
		if _, err := cn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, fmt.Errorf("%s %s: commit: %w", side.Name, table, err)
		}
		return out, nil
	}
	parallel := c.opts.Parallel
	if parallel > len(chunks) {
		parallel = len(chunks)
	}
	if parallel < 1 {
		parallel = 1
	}
	g, gctx := errgroup.WithContext(ctx)
	jobs := make(chan chunk.Chunk, len(chunks))
	for _, ch := range chunks {
		jobs <- ch
	}
	close(jobs)
	type result struct {
		id  int
		dig mhash.ChunkDigest
	}
	results := make(chan result, len(chunks))
	for w := 0; w < parallel; w++ {
		g.Go(func() error {
			for ch := range jobs {
				cn, err := side.AcquireScan(gctx)
				if err != nil {
					return err
				}
				d, err := sc.Scan(gctx, cn, schema, ch, c.opts.Where)
				cn.Close()
				if err != nil {
					return err
				}
				report()
				results <- result{ch.ID, d}
			}
			return nil
		})
	}
	go func() {
		_ = g.Wait()
		close(results)
	}()
	for r := range results {
		out[r.id] = r.dig
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}

// skipRowScan reports whether a table whose row counts differ may skip
// planning and the row scans entirely: the count difference already makes
// the table DIFFERENT, and without --drill no row-level detail is asked
// for. With --drill the operator paid for row-level detail, so scan.
func skipRowScan(srcTotal, dstTotal int64, drill bool) bool {
	return srcTotal != dstTotal && !drill
}

// pickScanError attributes a failure of the concurrent two-side scan to a
// side. A side whose scan merely reported context cancellation (because the
// other side failed first) is not blamed: prefer a side with a real error.
func pickScanError(srcErr, dstErr error) error {
	alive := func(e error) bool { return e != nil && !errors.Is(e, context.Canceled) }
	switch {
	case alive(srcErr):
		return fmt.Errorf("src scan: %w", srcErr)
	case alive(dstErr):
		return fmt.Errorf("dst scan: %w", dstErr)
	case srcErr != nil:
		return fmt.Errorf("src scan: %w", srcErr)
	case dstErr != nil:
		return fmt.Errorf("dst scan: %w", dstErr)
	}
	return nil
}

func (c *Comparer) count(ctx context.Context, side *conn.Side, table string) (int64, error) {
	q := fmt.Sprintf("SELECT COUNT(*) FROM %s", conn.QuoteIdent(table))
	if c.opts.Where != "" {
		q += " WHERE (" + c.opts.Where + ")"
	}
	var n int64
	if err := side.Ctl().QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("%s %s: %w", side.Name, table, err)
	}
	return n, nil
}

// digestsEqual compares the statistics relevant to each digest's path.
func digestsEqual(a, b mhash.ChunkDigest) bool {
	if a.Ordered {
		return a.Count == b.Count && a.Order == b.Order
	}
	return a.Count == b.Count && a.Sum == b.Sum && a.Xor == b.Xor && a.SumSq == b.SumSq
}

func digestList(m map[int]mhash.ChunkDigest) []mhash.ChunkDigest {
	out := make([]mhash.ChunkDigest, 0, len(m))
	for _, d := range m {
		out = append(out, d)
	}
	return out
}

// foldDigests turns per-chunk digests from both sides into the table's
// fingerprints, its list of differing chunks and its verdict: differing row
// counts or any differing chunk marks the table DIFFERENT. Pure (no I/O),
// so the count-mismatch branch is unit-testable without a database. It
// returns the chunk lookup table used by drill-down (nil when the digest
// sets do not line up, in which case there are no chunks to drill).
func foldDigests(res *TableResult, chunks []chunk.Chunk, srcDigests, dstDigests map[int]mhash.ChunkDigest, srcTotal, dstTotal int64, ordered, secure bool) map[int]chunk.Chunk {
	res.SrcFP = mhash.Hex(mhash.TableFingerprint(digestList(srcDigests), ordered, secure))
	res.DstFP = mhash.Hex(mhash.TableFingerprint(digestList(dstDigests), ordered, secure))
	if len(srcDigests) != len(dstDigests) {
		res.Status = "DIFFERENT"
		return nil
	}
	byID := make(map[int]chunk.Chunk, len(chunks))
	for _, ch := range chunks {
		byID[ch.ID] = ch
	}
	for id, sd := range srcDigests {
		dd, ok := dstDigests[id]
		if ok && digestsEqual(sd, dd) {
			continue
		}
		ch := byID[id]
		d := ChunkDiff{ID: id, Lo: ch.RenderBound(true), Hi: ch.RenderBound(false), Src: mhash.HexDigest(sd)}
		if ok {
			d.Dst = mhash.HexDigest(dd)
		} else {
			d.Dst = "missing"
		}
		res.DiffChunks = append(res.DiffChunks, d)
	}
	if srcTotal != dstTotal || len(res.DiffChunks) > 0 {
		res.Status = "DIFFERENT"
	}
	return byID
}

// applyKey overrides the selected key with an explicit --key on both sides.
// It returns warnings for key columns that are nullable: NULL key rows are
// handled by the special NULL-bound predicates, but the operator should know
// they are in play (and a NOT NULL key would be more robust).
func applyKey(src, dst *conn.Schema, key []string) []string {
	if len(key) == 0 {
		return nil
	}
	src.Key, src.KeySource, src.KeyIsUnique = key, "explicit", false
	dst.Key, dst.KeySource, dst.KeyIsUnique = key, "explicit", false
	return append(keyNullabilityWarns(src), keyNullabilityWarns(dst)...)
}

func keyNullabilityWarns(s *conn.Schema) []string {
	nullable := make(map[string]bool, len(s.Cols))
	for _, c := range s.Cols {
		if c.Nullable {
			nullable[c.Name] = true
		}
	}
	var warns []string
	for _, k := range s.Key {
		if nullable[k] {
			warns = append(warns, fmt.Sprintf(
				"key column %s is nullable: NULL key rows are matched via special predicates; prefer a NOT NULL key", k))
		}
	}
	return warns
}

// filterIgnored removes ignored columns from both schemas (source order is
// kept on the destination). Any destination column that is neither compared
// nor ignored is an error, matching the hard error an un-ignored destination
// drift already produces via Compatible: silently dropping it would compare
// less data than the operator asked for.
func filterIgnored(src, dst *conn.Schema, ignore map[string]bool) (*conn.Schema, *conn.Schema, error) {
	if len(ignore) == 0 {
		return src, dst, nil
	}
	keep := make([]conn.Column, 0, len(src.Cols))
	keepByName := make(map[string]bool, len(src.Cols))
	for _, col := range src.Cols {
		if ignore[col.Name] {
			continue
		}
		keep = append(keep, col)
		keepByName[col.Name] = true
	}
	if len(keep) == 0 {
		return nil, nil, fmt.Errorf("all columns of %s are ignored", src.Table)
	}
	for _, col := range dst.Cols {
		if !keepByName[col.Name] && !ignore[col.Name] {
			return nil, nil, fmt.Errorf(
				"destination column %s is neither compared nor listed in --ignore-columns", col.Name)
		}
	}
	dstByName := make(map[string]conn.Column, len(dst.Cols))
	for _, col := range dst.Cols {
		dstByName[col.Name] = col
	}
	dstKeep := make([]conn.Column, 0, len(keep))
	for _, col := range keep {
		d, ok := dstByName[col.Name]
		if !ok {
			return nil, nil, fmt.Errorf("column %s missing on destination", col.Name)
		}
		dstKeep = append(dstKeep, d)
	}
	src2 := *src
	src2.Cols = keep
	dst2 := *dst
	dst2.Cols = dstKeep
	return &src2, &dst2, nil
}
