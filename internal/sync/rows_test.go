package sync

import (
	"reflect"
	"testing"

	"mtdiff/internal/conn"
	"mtdiff/internal/normalize"
)

func keyedCols(nullableKey bool) []conn.Column {
	return []conn.Column{
		{Name: "id", Family: conn.FamINT, RawType: "int", Nullable: nullableKey},
		{Name: "v", Family: conn.FamSTR, RawType: "varchar(10)"},
		{Name: "w", Family: conn.FamINT, RawType: "int"},
	}
}

func colsFor(cols []conn.Column, names ...string) []conn.Column {
	byName := make(map[string]conn.Column, len(cols))
	for _, c := range cols {
		byName[c.Name] = c
	}
	out := make([]conn.Column, len(names))
	for i, n := range names {
		out[i] = byName[n]
	}
	return out
}

func newTestEngine(t *testing.T, opts normalize.Options, unique bool, cols []conn.Column) *Engine {
	t.Helper()
	return newTestEngineWithUnique(t, opts, unique, cols, cols, nil)
}

// newTestEngineWithUnique builds an engine over possibly different
// source/destination schemas and a set of unique (non-key) columns.
func newTestEngineWithUnique(t *testing.T, opts normalize.Options, unique bool, cols, dstCols []conn.Column, uniqueCols map[string]bool) *Engine {
	t.Helper()
	return NewEngine(
		normalize.NewNormalizer(cols, opts),
		normalize.NewNormalizer(dstCols, opts),
		normalize.NewNormalizer(colsFor(cols, "id"), opts),
		normalize.NewNormalizer(colsFor(dstCols, "id"), opts),
		unique, colsFor(cols, "id"), cols, dstCols, uniqueCols)
}

// nextOf adapts a row slice to the iterator seam (mirrors the DB scan
// loop, which hands out one reused buffer per row).
func nextOf(rows [][]any) func() ([]any, bool) {
	i := 0
	return func() ([]any, bool) {
		if i >= len(rows) {
			return nil, false
		}
		r := rows[i]
		i++
		return r, true
	}
}

func TestDiffUniqueKey(t *testing.T) {
	e := newTestEngine(t, normalize.DefaultOptions(), true, keyedCols(false))
	src := [][]any{
		{int64(1), "a", int64(10)},
		{int64(2), "b", int64(20)},
		{int64(3), "c", int64(30)},
	}
	dst := [][]any{
		{int64(1), "a", int64(10)},
		{int64(2), "z", int64(20)},
		{int64(9), "d", int64(90)},
	}
	srcM, err := e.buffer(e.srcRow, e.srcKey, nextOf(src))
	if err != nil {
		t.Fatal(err)
	}
	dstM, err := e.buffer(e.dstRow, e.dstKey, nextOf(dst))
	if err != nil {
		t.Fatal(err)
	}
	ops := e.Diff(srcM, dstM)
	if ins, upd, del := Counts([][]op{ops}); ins != 1 || upd != 1 || del != 1 {
		t.Fatalf("counts = %d/%d/%d, want 1/1/1", ins, upd, del)
	}
	// deterministic order: sorted key identity 2, 3, 9
	want := []op{
		{kind: opUpdate, key: []any{int64(2)}, rows: [][]any{{int64(2), "b", int64(20)}}},
		{kind: opInsert, key: []any{int64(3)}, rows: [][]any{{int64(3), "c", int64(30)}}},
		{kind: opDelete, key: []any{int64(9)}},
	}
	if !reflect.DeepEqual(ops, want) {
		t.Errorf("ops = %+v, want %+v", ops, want)
	}
}

func TestDiffTrailingSpaceAndFoldCase(t *testing.T) {
	// default options trim trailing spaces: "x " == "x", no op
	e := newTestEngine(t, normalize.DefaultOptions(), true, keyedCols(false))
	src := [][]any{{int64(1), "x ", int64(10)}}
	dst := [][]any{{int64(1), "x", int64(10)}}
	srcM, _ := e.buffer(e.srcRow, e.srcKey, nextOf(src))
	dstM, _ := e.buffer(e.dstRow, e.dstKey, nextOf(dst))
	if ops := e.Diff(srcM, dstM); len(ops) != 0 {
		t.Errorf("trimmed rows must compare equal, got %d ops", len(ops))
	}
	// without trimming they differ: one update
	e = newTestEngine(t, normalize.Options{}, true, keyedCols(false))
	srcM, _ = e.buffer(e.srcRow, e.srcKey, nextOf(src))
	dstM, _ = e.buffer(e.dstRow, e.dstKey, nextOf(dst))
	if ops := e.Diff(srcM, dstM); len(ops) != 1 || ops[0].kind != opUpdate {
		t.Errorf("untrimmed rows must yield one update, got %+v", ops)
	}
	// case folding
	e = newTestEngine(t, normalize.Options{TrimTrailing: true, FoldCase: true}, true, keyedCols(false))
	srcM, _ = e.buffer(e.srcRow, e.srcKey, nextOf([][]any{{int64(1), "Abc", int64(10)}}))
	dstM, _ = e.buffer(e.dstRow, e.dstKey, nextOf([][]any{{int64(1), "abc", int64(10)}}))
	if ops := e.Diff(srcM, dstM); len(ops) != 0 {
		t.Errorf("folded-case rows must compare equal, got %d ops", len(ops))
	}
}

func TestDiffNonUniqueReplace(t *testing.T) {
	e := newTestEngine(t, normalize.DefaultOptions(), false, keyedCols(false))
	src := [][]any{
		{int64(1), "a", int64(10)},
		{int64(1), "b", int64(11)},
		{int64(2), "c", int64(12)},
	}
	dst := [][]any{
		{int64(1), "a", int64(10)},
		{int64(2), "z", int64(13)},
	}
	srcM, _ := e.buffer(e.srcRow, e.srcKey, nextOf(src))
	dstM, _ := e.buffer(e.dstRow, e.dstKey, nextOf(dst))
	ops := e.Diff(srcM, dstM)
	if ins, upd, del := Counts([][]op{ops}); ins != 3 || upd != 0 || del != 2 {
		t.Fatalf("counts = %d/%d/%d, want 3/0/2 (group replace)", ins, upd, del)
	}
	// within each group deletes must precede inserts (unique slots freed first)
	var sawInsertBeforeDelete bool
	seenDelete := false
	for _, o := range ops {
		if o.kind == opDelete {
			seenDelete = true
		} else if o.kind == opInsert && !seenDelete {
			sawInsertBeforeDelete = true
		}
	}
	if sawInsertBeforeDelete {
		t.Error("an insert must not precede the deletes of its key group")
	}
}

func TestDiffNULLKeyComponent(t *testing.T) {
	e := newTestEngine(t, normalize.DefaultOptions(), true, keyedCols(true))
	src := [][]any{{nil, "a", int64(10)}}
	dst := [][]any{{nil, "b", int64(10)}}
	srcM, err := e.buffer(e.srcRow, e.srcKey, nextOf(src))
	if err != nil {
		t.Fatal(err)
	}
	dstM, err := e.buffer(e.dstRow, e.dstKey, nextOf(dst))
	if err != nil {
		t.Fatal(err)
	}
	ops := e.Diff(srcM, dstM)
	if len(ops) != 1 || ops[0].kind != opUpdate {
		t.Fatalf("want one update for the NULL-key row, got %+v", ops)
	}
	if ops[0].key[0] != nil {
		t.Errorf("NULL key component must stay nil: %v", ops[0].key)
	}
	if !reflect.DeepEqual(ops[0].rows[0], []any{nil, "a", int64(10)}) {
		t.Errorf("update payload = %v, want source row", ops[0].rows[0])
	}
}

// uniqueColsTest is the swap-protection test shape: id (PK), code (a
// UNIQUE non-key column), n.
func uniqueColsTest(t *testing.T) []conn.Column {
	t.Helper()
	return []conn.Column{
		{Name: "id", Family: conn.FamINT, RawType: "int"},
		{Name: "code", Family: conn.FamSTR, RawType: "varchar(10)"},
		{Name: "n", Family: conn.FamINT, RawType: "int"},
	}
}

// TestDiffUniqueValueSwap is the P1-4 regression: src (1,B)(2,A) vs
// dst (1,A)(2,B) with code unique. No order of per-row UPDATEs can apply
// this (each UPDATE collides with the other row's still-current value),
// so the chunk must come back as deletes-first, then inserts — never as
// two bare updates.
func TestDiffUniqueValueSwap(t *testing.T) {
	cols := uniqueColsTest(t)
	e := newTestEngineWithUnique(t, normalize.DefaultOptions(), true, cols, cols,
		map[string]bool{"code": true})
	src := [][]any{
		{int64(1), "B", int64(10)},
		{int64(2), "A", int64(20)},
	}
	dst := [][]any{
		{int64(1), "A", int64(10)},
		{int64(2), "B", int64(20)},
	}
	srcM, _ := e.buffer(e.srcRow, e.srcKey, nextOf(src))
	dstM, _ := e.buffer(e.dstRow, e.dstKey, nextOf(dst))
	ops := e.Diff(srcM, dstM)
	if ins, upd, del := Counts([][]op{ops}); ins != 2 || upd != 0 || del != 2 {
		t.Fatalf("swap must be delete+insert per row: ins/upd/del = %d/%d/%d, want 2/0/2", ins, upd, del)
	}
	// every delete must precede every insert (unique slots freed first)
	lastDel, firstIns := -1, len(ops)
	for i, o := range ops {
		switch o.kind {
		case opDelete:
			lastDel = i
		case opInsert:
			if i < firstIns {
				firstIns = i
			}
		}
	}
	if lastDel >= firstIns {
		t.Errorf("a delete must not follow an insert in a swapped chunk: %+v", ops)
	}
}

// TestDiffUniqueOneWayMoveStaysUpdate: values move one way (row 1 takes
// "B", row 2 takes "D") and nothing else in the chunk holds them: no
// collision, so the classic per-row UPDATEs stay in force (no
// over-conversion).
func TestDiffUniqueOneWayMoveStaysUpdate(t *testing.T) {
	cols := uniqueColsTest(t)
	e := newTestEngineWithUnique(t, normalize.DefaultOptions(), true, cols, cols,
		map[string]bool{"code": true})
	src := [][]any{
		{int64(1), "B", int64(10)},
		{int64(2), "D", int64(20)},
		{int64(3), "C", int64(30)},
	}
	dst := [][]any{
		{int64(1), "A", int64(10)},
		{int64(2), "C", int64(20)},
		{int64(3), "C", int64(30)},
	}
	srcM, _ := e.buffer(e.srcRow, e.srcKey, nextOf(src))
	dstM, _ := e.buffer(e.dstRow, e.dstKey, nextOf(dst))
	ops := e.Diff(srcM, dstM)
	if ins, upd, del := Counts([][]op{ops}); ins != 0 || upd != 2 || del != 0 {
		t.Fatalf("no collision: keep two plain updates, got ins/upd/del = %d/%d/%d", ins, upd, del)
	}
}

// TestDiffUniqueHolderRewrite: row 1 takes the value "B" that the
// UNCHANGED row 2 currently holds. The hot path must rewrite row 2
// (delete + re-insert of the identical row) so its slot is freed before
// row 1's insert runs.
func TestDiffUniqueHolderRewrite(t *testing.T) {
	cols := uniqueColsTest(t)
	e := newTestEngineWithUnique(t, normalize.DefaultOptions(), true, cols, cols,
		map[string]bool{"code": true})
	src := [][]any{
		{int64(1), "B", int64(10)},
		{int64(2), "B", int64(20)},
	}
	dst := [][]any{
		{int64(1), "A", int64(10)},
		{int64(2), "B", int64(20)},
	}
	srcM, _ := e.buffer(e.srcRow, e.srcKey, nextOf(src))
	dstM, _ := e.buffer(e.dstRow, e.dstKey, nextOf(dst))
	ops := e.Diff(srcM, dstM)
	var rewrites, dels, ins int
	for _, o := range ops {
		switch o.kind {
		case opRewrite:
			rewrites++
		case opDelete:
			dels++
		case opInsert:
			ins++
		}
	}
	// row 1: converted update (delete+insert); row 2: rewritten no-op —
	// the rewrite is ONE op (delete the key, re-insert the identical row),
	// so by op kind the chunk is: 1 delete (row 1), 1 rewrite (row 2,
	// counted once), 1 insert (row 1). No plain update survives a hot
	// chunk.
	if rewrites != 1 || dels != 1 || ins != 1 {
		t.Fatalf("want 1 rewrite, 1 delete, 1 insert; got rewrites=%d dels=%d ins=%d: %+v", rewrites, dels, ins, ops)
	}
}

// TestDiffThreeCycleSwap: a 3-cycle (A->B->C->A) is unorderable like the
// 2-cycle and must come back fully converted (no updates).
func TestDiffThreeCycleSwap(t *testing.T) {
	cols := uniqueColsTest(t)
	e := newTestEngineWithUnique(t, normalize.DefaultOptions(), true, cols, cols,
		map[string]bool{"code": true})
	src := [][]any{
		{int64(1), "B", int64(10)},
		{int64(2), "C", int64(20)},
		{int64(3), "A", int64(30)},
	}
	dst := [][]any{
		{int64(1), "A", int64(10)},
		{int64(2), "B", int64(20)},
		{int64(3), "C", int64(30)},
	}
	srcM, _ := e.buffer(e.srcRow, e.srcKey, nextOf(src))
	dstM, _ := e.buffer(e.dstRow, e.dstKey, nextOf(dst))
	ops := e.Diff(srcM, dstM)
	if ins, upd, del := Counts([][]op{ops}); ins != 3 || upd != 0 || del != 3 {
		t.Fatalf("3-cycle must be 3 deletes + 3 inserts, got ins/upd/del = %d/%d/%d", ins, upd, del)
	}
}

// newTestEngineKeyed is newTestEngineWithUnique with a selectable
// addressing key: a non-PK unique key makes the PK a unique NON-key
// column, whose values can collide across groups.
func newTestEngineKeyed(t *testing.T, unique bool, cols []conn.Column, uniqueCols map[string]bool, keyName string) *Engine {
	t.Helper()
	return NewEngine(
		normalize.NewNormalizer(cols, normalize.DefaultOptions()),
		normalize.NewNormalizer(cols, normalize.DefaultOptions()),
		normalize.NewNormalizer(colsFor(cols, keyName), normalize.DefaultOptions()),
		normalize.NewNormalizer(colsFor(cols, keyName), normalize.DefaultOptions()),
		unique, colsFor(cols, keyName), cols, cols, uniqueCols)
}

// TestDiffUniqueNonPKKeyPKCollision is the e2e t_swap regression: PK id +
// UNIQUE code, addressed by --key code. src (1,'B'),(2,'A') vs dst
// (1,'B'),(2,'Z'): the 'A' insert needs PK value 2, currently held by the
// dst row (2,'Z') that a LATER group deletes — classic group-order
// emission would hit the PK (duplicate key). The protection must treat
// the PK like any unique column and emit deletes before inserts.
func TestDiffUniqueNonPKKeyPKCollision(t *testing.T) {
	cols := uniqueColsTest(t)
	e := newTestEngineKeyed(t, true, cols, map[string]bool{"id": true, "code": true}, "code")
	src := [][]any{
		{int64(1), "B", int64(10)},
		{int64(2), "A", int64(20)},
	}
	dst := [][]any{
		{int64(1), "B", int64(10)},
		{int64(2), "Z", int64(30)},
	}
	srcM, _ := e.buffer(e.srcRow, e.srcKey, nextOf(src))
	dstM, _ := e.buffer(e.dstRow, e.dstKey, nextOf(dst))
	ops := e.Diff(srcM, dstM)
	if ins, upd, del := Counts([][]op{ops}); ins != 1 || upd != 0 || del != 1 {
		t.Fatalf("want 1 insert + 1 delete, got ins/upd/del = %d/%d/%d: %+v", ins, upd, del, ops)
	}
	lastDel, firstIns := -1, len(ops)
	for i, o := range ops {
		switch o.kind {
		case opDelete:
			lastDel = i
		case opInsert:
			if i < firstIns {
				firstIns = i
			}
		}
	}
	if lastDel >= firstIns {
		t.Errorf("the delete of the PK-holding row must precede the insert: %+v", ops)
	}
}
