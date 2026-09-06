// Package compare orchestrates table comparison: introspect, plan chunks,
// stream both sides in parallel, and fold chunk digests into a verdict.
package compare

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"golang.org/x/sync/errgroup"

	"mtdiff/internal/chunk"
	"mtdiff/internal/conn"
	mhash "mtdiff/internal/hash"
	"mtdiff/internal/normalize"
)

// Options control comparison behavior.
type Options struct {
	Parallel  int
	ChunkSize int
	// Snapshot reads each side of each table at one point in time (P1-5):
	// per side, the COUNT, the key extremes (the chunk plan), every chunk
	// scan and the drill-down scans run on ONE dedicated connection inside
	// ONE read transaction (serial; slower, but stable under concurrent
	// writes). Consistency is per side: the two sides take their
	// snapshots at different instants, which protects each side from
	// concurrent writes but does not make the pair point-in-time with
	// respect to each other.
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

	// Snapshot mode (P1-5): each side takes its snapshot on one dedicated
	// connection — the COUNT, the key extremes (the chunk plan), the
	// chunk scans and the drill-downs all run on it, so the whole table is
	// read at one point in time per side (see Options.Snapshot for the
	// per-side caveat).
	var srcSnap, dstSnap *sql.Conn
	if c.opts.Snapshot {
		srcSnap, err = c.beginSnapshotTx(ctx, src, name)
		if err != nil {
			return fail("src snapshot: %v", err)
		}
		defer c.endSnapshotTx(ctx, srcSnap)
		dstSnap, err = c.beginSnapshotTx(ctx, dst, name)
		if err != nil {
			return fail("dst snapshot: %v", err)
		}
		defer c.endSnapshotTx(ctx, dstSnap)
	}
	countOn := func(side *conn.Side, snap *sql.Conn) (int64, error) {
		if snap != nil {
			return c.countQ(ctx, snap, side.Name, name)
		}
		return c.count(ctx, side, name)
	}
	srcTotal, err := countOn(src, srcSnap)
	if err != nil {
		return fail("src count: %v", err)
	}
	dstTotal, err := countOn(dst, dstSnap)
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
	} else if len(srcSchema.Key) > 0 && !sameKeySequence(srcSchema.Key, dstSchema.Key) {
		// Both sides are keyed, but the keys differ (names or order —
		// e.g. PK (a,b) vs (b,a) under --no-sync-schema): the planner
		// renders the source's key bounds into predicates that run
		// against the destination's key columns, so a crossed key shape
		// partitions the destination rows wrong. Same fallback as the
		// keyless case: order-independent whole-table multisets.
		keyMismatch = true
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("usable keys differ between the sides (src %s vs dst %s): comparing as keyless whole-table multisets",
				strings.Join(srcSchema.Key, ","), strings.Join(dstSchema.Key, ",")))
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
		// the plan's extremes run on a policy-applied control session
		// (dead-connection recovery included); snapshot mode keeps the
		// snapshot connection — a dead snapshot transaction is a hard
		// failure, not a re-plan
		var planQ chunk.Querier
		if srcSnap != nil {
			planQ = srcSnap // the extremes must come from the snapshot read
		} else {
			q, err := src.Control(ctx)
			if err != nil {
				return fail("control: %v", err)
			}
			defer q.Close()
			planQ = q
		}
		chunks, err = p.Plan(ctx, planQ, srcTotal)
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
		srcDigests, srcErr = c.scanSide(gctx, src, srcSnap, sc, srcSchema, chunks, name)
		return srcErr
	})
	g.Go(func() error {
		dstDigests, dstErr = c.scanSide(gctx, dst, dstSnap, dc, dstSchema, chunks, name)
		return dstErr
	})
	_ = g.Wait()
	if err := pickScanError(srcErr, dstErr); err != nil {
		return fail("%v", err)
	}

	byID := foldDigests(&res, chunks, srcDigests, dstDigests, srcTotal, dstTotal, ordered, c.opts.Secure)
	if c.opts.Drill && len(res.DiffChunks) > 0 {
		// snapshot mode: the drill-down scans ride the still-open
		// snapshot transactions, so the example rows show the same
		// point in time the digest was computed at
		dd := &DrillDown{srcCN: srcSnap, dstCN: dstSnap}
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

// scanSide streams every chunk of one side, in parallel. When snapConn is
// set (snapshot mode) the whole table is scanned serially on that one
// connection, inside the read transaction the caller opened (and still
// holds): every read of the table happens at one point in time (P1-5).
func (c *Comparer) scanSide(ctx context.Context, side *conn.Side, snapConn *sql.Conn, sc *chunk.Scanner, schema *conn.Schema, chunks []chunk.Chunk, table string) (map[int]mhash.ChunkDigest, error) {
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
	if snapConn != nil {
		for _, ch := range chunks {
			d, err := sc.Scan(ctx, snapConn, schema, ch, c.opts.Where)
			if err != nil {
				return nil, err
			}
			out[ch.ID] = d
			report()
		}
		// the transaction (and its release) is the caller's: the
		// drill-down may still be reading on it
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
			// One scan connection per worker for the whole side
			// (P2-1): acquiring per chunk churns the pool and pays
			// the checkout bookkeeping on every chunk. If the
			// pinned connection dies mid-scan, take a fresh one
			// (the pool re-initializes it) and retry the chunk
			// once.
			cn, err := side.AcquireScan(gctx)
			if err != nil {
				return err
			}
			defer func() { cn.Close() }()
			for ch := range jobs {
				d, err := sc.Scan(gctx, cn, schema, ch, c.opts.Where)
				if err != nil && conn.DeadConn(err) {
					cn.Close()
					if cn, err = side.AcquireScan(gctx); err != nil {
						return err
					}
					d, err = sc.Scan(gctx, cn, schema, ch, c.opts.Where)
				}
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
	q, err := side.Control(ctx)
	if err != nil {
		return 0, err
	}
	defer q.Close()
	return c.countQ(ctx, q, side.Name, table)
}

// countQ is the COUNT on an arbitrary read seam (snapshot mode runs it on
// the snapshot connection, not the control connection).
func (c *Comparer) countQ(ctx context.Context, q chunk.Querier, sideName, table string) (int64, error) {
	qry := fmt.Sprintf("SELECT COUNT(*) FROM %s", conn.QuoteIdent(table))
	if c.opts.Where != "" {
		qry += " WHERE (" + c.opts.Where + ")"
	}
	var n int64
	rows, err := q.QueryContext(ctx, qry)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", sideName, table, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("%s %s: %w", sideName, table, err)
		}
		return 0, fmt.Errorf("%s %s: %w", sideName, table, sql.ErrNoRows)
	}
	if err := rows.Scan(&n); err != nil {
		return 0, fmt.Errorf("%s %s: %w", sideName, table, err)
	}
	return n, nil
}

// beginSnapshotTx takes one dedicated scan connection and opens a read
// transaction on it: that connection becomes the side's snapshot scope
// for one table — the COUNT, the key extremes, the chunk scans and the
// drill-downs all run on it, so the whole table is read at one point in
// time.
//
// The guarantee is FORCED, not inherited from the server default
// (P1-4): the session isolation level is set to REPEATABLE READ before
// the transaction begins, so a server or session configured for READ
// COMMITTED cannot silently weaken the snapshot to per-statement reads
// (a non-snapshot level would let COUNT, extremes and the chunk scans
// each see a different point in time and the digest could mix them).
// Only when the session is verified to be REPEATABLE READ does a plain
// START TRANSACTION suffice: in REPEATABLE READ every read in a
// transaction sees the same snapshot (MySQL pins it on the first read,
// TiDB pins the transaction's read timestamp on its first statement),
// and all reads here run on this one connection in this one
// transaction. A backend where neither the SET nor a verifiable
// REPEATABLE READ session is possible makes --snapshot a REFUSAL
// (unsupported), never a silent downgrade.
func (c *Comparer) beginSnapshotTx(ctx context.Context, side *conn.Side, table string) (*sql.Conn, error) {
	cn, err := side.AcquireScan(ctx)
	if err != nil {
		return nil, err
	}
	if err := beginSnapshotTx(ctx, cn); err != nil {
		cn.Close()
		return nil, fmt.Errorf("%s %s: --snapshot: %w", side.Name, table, err)
	}
	return cn, nil
}

// beginSnapshotTx forces the snapshot semantics on one dedicated
// connection: REPEATABLE READ for the session, then a transaction on it.
// The explicit "START TRANSACTION WITH CONSISTENT SNAPSHOT" is tried
// first (it pins the snapshot at the BEGIN, before any read); a backend
// that rejects the clause falls back to a plain START TRANSACTION, which
// is equally strict once the session is verified REPEATABLE READ (the
// snapshot pins on the first read, and every read of the table runs in
// this transaction on this connection).
func beginSnapshotTx(ctx context.Context, cn *sql.Conn) error {
	if _, err := cn.ExecContext(ctx, "SET SESSION TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
		// The SET is refused: the session may already run REPEATABLE
		// READ (a no-op SET is accepted, so a refusal means something
		// else — verify rather than assume). A session that cannot be
		// shown to be REPEATABLE READ cannot honor --snapshot.
		iso, isoErr := currentIsolation(ctx, cn)
		if isoErr != nil || !strings.EqualFold(iso, "REPEATABLE-READ") {
			return fmt.Errorf("cannot enforce REPEATABLE READ on this backend (set: %v; isolation: %v%v): --snapshot is unsupported here, run without it",
				err, iso, func() string {
					if isoErr != nil {
						return " unreadable"
					}
					return ""
				}())
		}
	}
	if _, err := cn.ExecContext(ctx, "START TRANSACTION WITH CONSISTENT SNAPSHOT"); err != nil {
		if _, err2 := cn.ExecContext(ctx, "START TRANSACTION"); err2 != nil {
			return fmt.Errorf("cannot open a snapshot transaction on this backend (%v; %v): --snapshot is unsupported here, run without it", err, err2)
		}
	}
	return nil
}

// currentIsolation reads the session's current isolation level (the
// modern name first; tx_isolation is the 5.7 spelling, removed from
// later 8.0 releases, so both are tried).
func currentIsolation(ctx context.Context, cn *sql.Conn) (string, error) {
	for _, v := range []string{"@@SESSION.transaction_isolation", "@@SESSION.tx_isolation"} {
		var iso string
		if err := cn.QueryRowContext(ctx, "SELECT "+v).Scan(&iso); err == nil {
			return iso, nil
		}
	}
	return "", fmt.Errorf("isolation level unreadable")
}

// endSnapshotTx ends the snapshot scope: the transaction was read-only,
// so a rollback is a no-op that only releases the connection. Best
// effort — a broken connection still closes.
func (c *Comparer) endSnapshotTx(ctx context.Context, cn *sql.Conn) {
	if cn == nil {
		return
	}
	_, _ = cn.ExecContext(ctx, "ROLLBACK")
	_ = cn.Close()
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
//
// The differing-chunk list is emitted in CHUNK-ID ORDER (the digests live
// in a map, whose iteration order is random): the sync plan derives its
// APPLY ORDER from this list, and the cross-chunk unique-holder verdict
// is only sound when chunks apply sequentially in key order (a safe
// cross-chunk value move relies on the earlier chunk freeing the slot
// first). A random order would apply the writer before the releaser and
// the unique index would reject it.
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
	diffIDs := make([]int, 0, len(srcDigests))
	for id, sd := range srcDigests {
		dd, ok := dstDigests[id]
		if ok && digestsEqual(sd, dd) {
			continue
		}
		diffIDs = append(diffIDs, id)
	}
	sort.Ints(diffIDs)
	for _, id := range diffIDs {
		sd := srcDigests[id]
		dd, ok := dstDigests[id]
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

// applyKey overrides the selected key with an explicit --key on both
// sides. The explicit key's UNIQUENESS is resolved against each side's
// index catalog (conn.ExplicitKeyIsUnique): an exact ordered match of the
// primary key or of a unique index whose columns are all NOT NULL is
// unique — the row-level sync can UPDATE by it (P1-1: an explicit --key
// naming a real PK is recognized, not silently downgraded to group
// replacement). Anything else (a non-unique column, a prefix of a
// composite unique index, a unique index with a nullable column) is
// non-unique: the sync engine replaces key groups instead of updating
// single rows, and a --where row-level sync is refused outright (see
// sync.DecidePlan). A catalog query that cannot be resolved is treated as
// non-unique with a warning — conservative, never assumed.
//
// It returns warnings for key columns that are nullable: NULL key rows
// are handled by the special NULL-bound predicates, but the operator
// should know they are in play (and a NOT NULL key would be more robust).
// applyKey overrides the selected key with an explicit --key on both
// sides. resolve reports, per side ("src"/"dst"), whether the explicit key
// is a unique row address there (in production: conn.ExplicitKeyIsUnique
// against that side's index catalog; nil in unit tests, where the key
// stays the conservative non-unique default). An exact ordered match of
// the primary key or of a unique index whose columns are all NOT NULL is
// unique — the row-level sync can UPDATE by it (P1-1: an explicit --key
// naming a real PK is recognized, not silently downgraded to group
// replacement). Anything else (a non-unique column, a prefix of a
// composite unique index, a unique index with a nullable column), or a
// resolution that fails, is non-unique: the sync engine replaces key
// groups instead of updating single rows, and a --where row-level sync is
// refused outright (see sync.DecidePlan).
//
// It returns warnings for key columns that are nullable: NULL key rows
// are handled by the special NULL-bound predicates, but the operator
// should know they are in play (and a NOT NULL key would be more robust).
func applyKey(src, dst *conn.Schema, key []string, resolve func(side string) (bool, error)) []string {
	if len(key) == 0 {
		return nil
	}
	src.Key, src.KeySource = key, "explicit"
	dst.Key, dst.KeySource = key, "explicit"
	var warns []string
	for _, side := range []struct {
		name   string
		schema *conn.Schema
	}{{"src", src}, {"dst", dst}} {
		unique := false
		if resolve != nil {
			var err error
			unique, err = resolve(side.name)
			if err != nil {
				// unresolvable: not proven unique → group replacement, warn
				warns = append(warns, fmt.Sprintf(
					"explicit key (%s) uniqueness could not be resolved on the %s side (%v): the key is treated as NON-unique",
					strings.Join(key, ","), side.name, err))
			} else if !unique {
				warns = append(warns, fmt.Sprintf(
					"explicit key (%s) is not a PRIMARY KEY or NOT NULL UNIQUE index on the %s side: rows may share it, the sync replaces key groups instead of updating rows",
					strings.Join(key, ","), side.name))
			}
		}
		side.schema.KeyIsUnique = unique
	}
	return append(warns, append(keyNullabilityWarns(src), keyNullabilityWarns(dst)...)...)
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

// sameKeySequence reports whether both sides' usable keys are the same
// columns in the same order. A mere presence check (both keyed) is not
// enough: with --no-sync-schema the sides may legally drift into
// different key shapes (PK (a,b) vs (b,a), or a PK vs a UNIQUE index on
// different columns), and a keyed row match rendered against the wrong
// key shape pairs the wrong rows.
func sameKeySequence(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
