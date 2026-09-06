package sync

import (
	"context"
	"database/sql/driver"
	"fmt"
	"reflect"
	"testing"

	"mtdiff/internal/chunk"
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
// source/destination schemas and a set of single-column unique
// constraints (the map's keys, one constraint per column).
func newTestEngineWithUnique(t *testing.T, opts normalize.Options, unique bool, cols, dstCols []conn.Column, uniqueCols map[string]bool) *Engine {
	t.Helper()
	return NewEngine(
		normalize.NewNormalizer(cols, opts),
		normalize.NewNormalizer(dstCols, opts),
		normalize.NewNormalizer(colsFor(cols, "id"), opts),
		normalize.NewNormalizer(colsFor(dstCols, "id"), opts),
		unique, colsFor(cols, "id"), cols, dstCols, uniqMap(srcUniq(cols, uniqueCols)), uniqMap(srcUniq(dstCols, uniqueCols)))
}

// srcUniq builds single-column unique constraints for the named columns
// present in cols.
func srcUniq(cols []conn.Column, names map[string]bool) []conn.UniqueConstraint {
	if len(names) == 0 {
		return nil
	}
	out := make([]conn.UniqueConstraint, 0, len(names))
	for _, c := range cols {
		if names[c.Name] {
			out = append(out, conn.UniqueConstraint{Name: "uk_" + c.Name, Cols: []string{c.Name}})
		}
	}
	return out
}

func uniqMap(u []conn.UniqueConstraint) []conn.UniqueConstraint { return u }

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
	ops, _ := e.Diff(srcM, dstM)
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
	if ops, _ := e.Diff(srcM, dstM); len(ops) != 0 {
		t.Errorf("trimmed rows must compare equal, got %d ops", len(ops))
	}
	// without trimming they differ: one update
	e = newTestEngine(t, normalize.Options{}, true, keyedCols(false))
	srcM, _ = e.buffer(e.srcRow, e.srcKey, nextOf(src))
	dstM, _ = e.buffer(e.dstRow, e.dstKey, nextOf(dst))
	if ops, _ := e.Diff(srcM, dstM); len(ops) != 1 || ops[0].kind != opUpdate {
		t.Errorf("untrimmed rows must yield one update, got %+v", ops)
	}
	// case folding
	e = newTestEngine(t, normalize.Options{TrimTrailing: true, FoldCase: true}, true, keyedCols(false))
	srcM, _ = e.buffer(e.srcRow, e.srcKey, nextOf([][]any{{int64(1), "Abc", int64(10)}}))
	dstM, _ = e.buffer(e.dstRow, e.dstKey, nextOf([][]any{{int64(1), "abc", int64(10)}}))
	if ops, _ := e.Diff(srcM, dstM); len(ops) != 0 {
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
	ops, _ := e.Diff(srcM, dstM)
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
	ops, _ := e.Diff(srcM, dstM)
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
	ops, rewrite := e.Diff(srcM, dstM)
	if !rewrite {
		t.Error("a 2-row swap must report the destructive row rewrite (P0-2)")
	}
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
	ops, rewrite := e.Diff(srcM, dstM)
	if rewrite {
		t.Error("a collision-free move must keep plain updates (no destructive rewrite)")
	}
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
	ops, rewrite := e.Diff(srcM, dstM)
	if !rewrite {
		t.Error("a holder conflict must report the destructive row rewrite (P0-2)")
	}
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
	ops, rewrite := e.Diff(srcM, dstM)
	if !rewrite {
		t.Error("a 3-cycle must report the destructive row rewrite (P0-2)")
	}
	if ins, upd, del := Counts([][]op{ops}); ins != 3 || upd != 0 || del != 3 {
		t.Fatalf("3-cycle must be 3 deletes + 3 inserts, got ins/upd/del = %d/%d/%d", ins, upd, del)
	}
}

// newTestEngineKeyed is newTestEngineWithUnique with a selectable
// addressing key: a non-PK unique key makes the PK a unique NON-key
// column, whose values can collide across groups.
func newTestEngineKeyed(t *testing.T, unique bool, cols []conn.Column, uniqueCols map[string]bool, keyName string) *Engine {
	t.Helper()
	u := srcUniq(cols, uniqueCols)
	return NewEngine(
		normalize.NewNormalizer(cols, normalize.DefaultOptions()),
		normalize.NewNormalizer(cols, normalize.DefaultOptions()),
		normalize.NewNormalizer(colsFor(cols, keyName), normalize.DefaultOptions()),
		normalize.NewNormalizer(colsFor(cols, keyName), normalize.DefaultOptions()),
		unique, colsFor(cols, keyName), cols, cols, u, u)
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
	ops, rewrite := e.Diff(srcM, dstM)
	if !rewrite {
		t.Error("a PK collision must report the destructive row rewrite (P0-2)")
	}
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

// compositeCols is the P1-5 test shape: id (PK) plus two columns that a
// composite UNIQUE(a,b) covers.
func compositeCols(t *testing.T) []conn.Column {
	t.Helper()
	return []conn.Column{
		{Name: "id", Family: conn.FamINT, RawType: "int"},
		{Name: "a", Family: conn.FamSTR, RawType: "varchar(10)"},
		{Name: "b", Family: conn.FamSTR, RawType: "varchar(10)"},
	}
}

func newTestEngineWithConstraints(t testing.TB, cols, dstCols []conn.Column, uniq []conn.UniqueConstraint) *Engine {
	t.Helper()
	return NewEngine(
		normalize.NewNormalizer(cols, normalize.DefaultOptions()),
		normalize.NewNormalizer(dstCols, normalize.DefaultOptions()),
		normalize.NewNormalizer(colsFor(cols, "id"), normalize.DefaultOptions()),
		normalize.NewNormalizer(colsFor(dstCols, "id"), normalize.DefaultOptions()),
		true, colsFor(cols, "id"), cols, dstCols, uniq, uniq)
}

// TestDiffCompositeUniqueNoFalseConflict is the P1-5 regression: a
// composite UNIQUE(a,b) must NOT make a or b individually unique — a
// value of a that repeats across rows (with a different b) is not a
// conflict, so the chunk keeps its plain UPDATEs (no destructive
// rewrite).
func TestDiffCompositeUniqueNoFalseConflict(t *testing.T) {
	cols := compositeCols(t)
	e := newTestEngineWithConstraints(t, cols, cols, []conn.UniqueConstraint{
		{Name: "uk_ab", Cols: []string{"a", "b"}},
	})
	src := [][]any{
		{int64(1), "x", "p"},
		{int64(2), "x", "q"},
	}
	dst := [][]any{
		{int64(1), "x", "z"},
		{int64(2), "x", "q"},
	}
	srcM, _ := e.buffer(e.srcRow, e.srcKey, nextOf(src))
	dstM, _ := e.buffer(e.dstRow, e.dstKey, nextOf(dst))
	ops, rewrite := e.Diff(srcM, dstM)
	if rewrite {
		t.Fatalf("a repeated composite MEMBER is not a conflict: no rewrite expected: %+v", ops)
	}
	if len(ops) != 1 || ops[0].kind != opUpdate {
		t.Fatalf("want one plain update, got %+v", ops)
	}
}

// TestDiffCompositeTrueSwap: the whole TUPLE swaps between two rows —
// that IS a conflict for UNIQUE(a,b), and the chunk is a destructive
// rewrite (P0-2: reported, refused without --allow-row-rewrite).
func TestDiffCompositeTrueSwap(t *testing.T) {
	cols := compositeCols(t)
	e := newTestEngineWithConstraints(t, cols, cols, []conn.UniqueConstraint{
		{Name: "uk_ab", Cols: []string{"a", "b"}},
	})
	src := [][]any{
		{int64(1), "a", "b"},
		{int64(2), "b", "a"},
	}
	dst := [][]any{
		{int64(1), "b", "a"},
		{int64(2), "a", "b"},
	}
	srcM, _ := e.buffer(e.srcRow, e.srcKey, nextOf(src))
	dstM, _ := e.buffer(e.dstRow, e.dstKey, nextOf(dst))
	ops, rewrite := e.Diff(srcM, dstM)
	if !rewrite {
		t.Fatalf("a true tuple swap must be a destructive rewrite: %+v", ops)
	}
}

// TestDiffTwoUniqueNoCrossCollision is the P1-5 regression: two
// separate constraints (UNIQUE(email) + UNIQUE(phone)) must not
// cross-collide — an email value equal to another row's phone value is
// fine, the chunk stays on plain updates.
func TestDiffTwoUniqueNoCrossCollision(t *testing.T) {
	cols := []conn.Column{
		{Name: "id", Family: conn.FamINT, RawType: "int"},
		{Name: "email", Family: conn.FamSTR, RawType: "varchar(64)"},
		{Name: "phone", Family: conn.FamSTR, RawType: "varchar(32)"},
	}
	e := newTestEngineWithConstraints(t, cols, cols, []conn.UniqueConstraint{
		{Name: "uk_email", Cols: []string{"email"}},
		{Name: "uk_phone", Cols: []string{"phone"}},
	})
	src := [][]any{
		{int64(1), "x@e", "911"},
		{int64(2), "911", "x@e"}, // email "911" == row 1's phone: different constraint
	}
	dst := [][]any{
		{int64(1), "p@e", "111"},
		{int64(2), "q@e", "222"},
	}
	srcM, _ := e.buffer(e.srcRow, e.srcKey, nextOf(src))
	dstM, _ := e.buffer(e.dstRow, e.dstKey, nextOf(dst))
	ops, rewrite := e.Diff(srcM, dstM)
	if rewrite {
		t.Fatalf("different constraints must not cross-collide: %+v", ops)
	}
	if len(ops) != 2 {
		t.Fatalf("want two plain updates, got %+v", ops)
	}
}

// TestDiffNullableUniqueNULLNoFalseConflict: a NULL in a unique column
// occupies no slot (MySQL allows repeated NULLs): two rows moving to
// NULL, past a no-op holder, are not a conflict.
func TestDiffNullableUniqueNULLNoFalseConflict(t *testing.T) {
	cols := []conn.Column{
		{Name: "id", Family: conn.FamINT, RawType: "int"},
		{Name: "u", Family: conn.FamSTR, RawType: "varchar(10)", Nullable: true},
	}
	e := newTestEngineWithConstraints(t, cols, cols, []conn.UniqueConstraint{
		{Name: "uk_u", Cols: []string{"u"}},
	})
	src := [][]any{
		{int64(1), nil},
		{int64(2), nil},
		{int64(3), "z"},
	}
	dst := [][]any{
		{int64(1), "a"},
		{int64(2), "b"},
		{int64(3), "z"},
	}
	srcM, _ := e.buffer(e.srcRow, e.srcKey, nextOf(src))
	dstM, _ := e.buffer(e.dstRow, e.dstKey, nextOf(dst))
	ops, rewrite := e.Diff(srcM, dstM)
	if rewrite {
		t.Fatalf("NULL tuples occupy no unique slot: no rewrite expected: %+v", ops)
	}
	if len(ops) != 2 {
		t.Fatalf("want two plain updates, got %+v", ops)
	}
}

// TestValueCmp pins the cross-chunk ordering primitive: MySQL key order
// (NULLs first), the numeric pairs, character/binary bytes, and the
// conservative "incomparable" answer (mixed or unknown types must never
// be reported as ordered).
func TestValueCmp(t *testing.T) {
	cases := []struct {
		a, b any
		cmp  int
		ok   bool
	}{
		{nil, nil, 0, true},
		{nil, int64(1), -1, true},
		{int64(1), nil, 1, true},
		{int64(1), int64(2), -1, true},
		{int64(2), int64(2), 0, true},
		{uint64(1), uint64(2), -1, true},
		{int64(-1), uint64(1), -1, true}, // negatives sort first
		{int64(1), uint64(1), 0, true},
		{"a", "b", -1, true},
		{[]byte("a"), "a", 0, true},
		{[]byte("a\x01"), []byte("a\x02"), -1, true},
		{float64(1), float64(2), 0, false}, // incomparable, not ordered
		{int64(1), "1", 0, false},          // mixed: not ordered
	}
	for i, c := range cases {
		if got, ok := valueCmp(c.a, c.b); ok != c.ok || (ok && got != c.cmp) {
			t.Errorf("case %d (%v, %v) = (%d, %v), want (%d, %v)", i, c.a, c.b, got, ok, c.cmp, c.ok)
		}
	}
}

// TestHolderPosition pins the chunk-boundary classification, including
// the lead-prefix case (the bound pins only the lead columns: equality
// on the lead with an unbounded suffix is "unknown", never "inside").
func TestHolderPosition(t *testing.T) {
	e := &Engine{}
	base := chunk.Chunk{Lo: []driver.Value{int64(1)}, LoIncl: true, Hi: []driver.Value{int64(10)}}
	if p := e.holderPosition([]any{int64(0)}, base); p != holderBefore {
		t.Errorf("below: got %v", p)
	}
	if p := e.holderPosition([]any{int64(11)}, base); p != holderAfter {
		t.Errorf("above: got %v", p)
	}
	if p := e.holderPosition([]any{int64(5)}, base); p != holderInside {
		t.Errorf("inside: got %v", p)
	}
	// exclusive lower bound: the bound row itself belongs to the
	// previous chunk, so it sorts BEFORE this one
	excl := chunk.Chunk{Lo: []driver.Value{int64(1)}, LoIncl: false, Hi: []driver.Value{int64(10)}}
	if p := e.holderPosition([]any{int64(1)}, excl); p != holderBefore {
		t.Errorf("exclusive bound row: got %v", p)
	}
	// lead-prefix EXCLUSIVE bound (int-keyed chunk i>0: Lo = prevHi-1,
	// the bound lead belongs to the previous chunk): equal lead is
	// strictly before, never unknown
	pref := chunk.Chunk{
		Lo: []driver.Value{int64(7501)}, LoIncl: false, LoPrefix: 1,
		Hi: []driver.Value{int64(15002)}, HiPrefix: 1,
	}
	if p := e.holderPosition([]any{int64(7501), "x"}, pref); p != holderBefore {
		t.Errorf("exclusive lead bound, equal lead: got %v", p)
	}
	if p := e.holderPosition([]any{int64(7500), "x"}, pref); p != holderBefore {
		t.Errorf("below lead: got %v", p)
	}
	if p := e.holderPosition([]any{int64(15003), "x"}, pref); p != holderAfter {
		t.Errorf("above lead: got %v", p)
	}
	// lead-prefix INCLUSIVE bound (the first int-keyed chunk: Lo = min,
	// equal lead may be inside or below — the suffix is unbounded):
	// unknown, the caller must not assume either side
	prefIncl := chunk.Chunk{
		Lo: []driver.Value{int64(7501)}, LoIncl: true, LoPrefix: 1,
		Hi: []driver.Value{int64(15002)}, HiPrefix: 1,
	}
	if p := e.holderPosition([]any{int64(7501), "x"}, prefIncl); p != holderUnknown {
		t.Errorf("inclusive lead bound, equal lead: got %v", p)
	}
	if p := e.holderPosition([]any{int64(7502), "x"}, prefIncl); p != holderInside {
		t.Errorf("inside inclusive lead range: got %v", p)
	}
	// unbounded first chunk: nothing is "before"
	first := chunk.Chunk{Hi: []driver.Value{int64(10)}}
	if p := e.holderPosition([]any{int64(1)}, first); p != holderInside {
		t.Errorf("first chunk inside: got %v", p)
	}
}

// TestKeyCmpIncomparable: a component pair the comparison cannot order
// (e.g. a float, which key columns do not yield but a custom backend
// might) must come back ok=false — the caller treats that as "position
// unknown", never as "equal" or "ordered".
func TestKeyCmpIncomparable(t *testing.T) {
	if _, ok := keyCmp([]any{float64(1)}, []any{float64(2)}, -1); ok {
		t.Error("floats must be incomparable")
	}
	if c, ok := keyCmp([]any{int64(1), "a"}, []any{int64(2), "b"}, -1); !ok || c != -1 {
		t.Errorf("ordered prefix must decide: got (%d, %v)", c, ok)
	}
}

// TestKeyOrderKnown pins the whitelist: only key types the driver yields
// in a form whose client order equals the server's key order are
// order-known. Decimal/float keys arrive as digit strings ("9" bytes
// before "10", numerically reversed), TIME as variable-width text,
// ENUM/SET by label though the server orders by index — all must be
// UNKNOWN (the cross-chunk holder check escalates instead of guessing).
func TestKeyOrderKnown(t *testing.T) {
	col := func(fam, raw, coll string) conn.Column {
		return conn.Column{Name: "k", Family: fam, RawType: raw, Collation: coll}
	}
	sch := func(c conn.Column) *conn.Schema {
		return &conn.Schema{Table: "t", Cols: []conn.Column{c}, Key: []string{"k"}}
	}
	cases := []struct {
		name string
		s    *conn.Schema
		want bool
	}{
		{"nil schema", nil, false},
		{"no key", &conn.Schema{Table: "t", Cols: []conn.Column{col(conn.FamINT, "int", "")}}, false},
		{"int pk", sch(col(conn.FamINT, "int", "")), true},
		{"uint pk", sch(col(conn.FamUINT, "bigint unsigned", "")), true},
		{"year pk", sch(col(conn.FamYEAR, "year", "")), true},
		{"datetime pk", sch(col(conn.FamDATETIME, "datetime(3)", "")), true},
		{"date pk", sch(col(conn.FamDATE, "date", "")), true},
		{"binary pk", sch(col(conn.FamBYTES, "varbinary(16)", "")), true},
		{"bit pk", sch(col(conn.FamBIT, "bit(8)", "")), true},
		{"varchar binary collation", sch(col(conn.FamSTR, "varchar(16)", "utf8mb4_bin")), true},
		{"varchar ci collation", sch(col(conn.FamSTR, "varchar(16)", "utf8mb4_0900_ai_ci")), false},
		{"decimal pk", sch(col(conn.FamDECIMAL, "decimal(10,2)", "")), false},
		{"float pk", sch(col(conn.FamFLOAT, "float", "")), false},
		{"double pk", sch(col(conn.FamDOUBLE, "double", "")), false},
		{"time pk", sch(col(conn.FamTIME, "time", "")), false},
		{"enum pk", sch(col(conn.FamENUM, "enum('a','b')", "")), false},
		{"set pk", sch(col(conn.FamSET, "set('a','b')", "")), false},
		{"json pk", sch(col(conn.FamJSON, "json", "")), false},
		{"key column missing from cols", &conn.Schema{
			Table: "t", Cols: []conn.Column{col(conn.FamINT, "int", "")}, Key: []string{"other"},
		}, false},
	}
	// a composite key where only one member is order-unknown must be unknown
	composite := &conn.Schema{
		Table: "t",
		Cols: []conn.Column{
			{Name: "a", Family: conn.FamINT, RawType: "int"},
			{Name: "d", Family: conn.FamDECIMAL, RawType: "decimal(10,2)"},
		},
		Key: []string{"a", "d"},
	}
	cases = append(cases, struct {
		name string
		s    *conn.Schema
		want bool
	}{"composite with a decimal member", composite, false})
	for _, c := range cases {
		if got := keyOrderKnown(c.s); got != c.want {
			t.Errorf("%s: keyOrderKnown = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestClassifyHolderOorFlag is the regression for the t_swap e2e
// (sync key v, PK id, an out-of-range row holding the PK value an
// in-range insert needs): the holder's SERVER-SIDE out-of-range flag
// (the out-of-range predicate evaluated in the holders query itself) is
// the safety proof, not a client-side key comparison. A case-insensitive
// key must still clear a flagged holder (the out-of-range pass deletes
// it before any in-range write) yet refuse an unflagged one (its chunk
// position is not provable client-side); an order-known key turns an
// unflagged holder the client orders OUTSIDE the source range into a
// conflict (a --where residual or a data race), never a false safe. All
// cases resolve before any database access (nil Sides).
func TestClassifyHolderOorFlag(t *testing.T) {
	cols := []conn.Column{
		{Name: "id", Family: conn.FamINT, RawType: "int"},
		{Name: "u", Family: conn.FamSTR, RawType: "varchar(10)", Nullable: true},
	}
	e := newTestEngineWithConstraints(t, cols, cols, []conn.UniqueConstraint{
		{Name: "uk_u", Cols: []string{"u"}},
	})
	h := []any{int64(99)}
	buf := make([]byte, 0, 16)
	id, err := e.dstKey.Normalize(h, buf)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	idS := string(id)
	ciS := &conn.Schema{ // a case-insensitive character key: not order-known
		Table: "t",
		Cols:  []conn.Column{{Name: "id", Family: conn.FamSTR, RawType: "varchar(10)", Collation: "utf8mb4_0900_ai_ci"}},
		Key:   []string{"id"},
	}
	intS := &conn.Schema{
		Table: "t",
		Cols:  []conn.Column{{Name: "id", Family: conn.FamINT, RawType: "int"}},
		Key:   []string{"id"},
	}
	c := e.uc[0]
	run := func(srcS *conn.Schema, targeted map[string]bool, oor bool, lo, hi []driver.Value, inChunk, oorActive bool) (crossChunkVerdict, error) {
		dstM := map[string][]*srow{}
		if inChunk {
			dstM[idS] = []*srow{{vals: h}}
		}
		// nil for the pinned source connection: none of the cases below
		// reach the source point query (holderInOtherChunk is the only
		// path that uses it)
		return e.classifyHolderConn(context.Background(), nil, srcS, chunk.Chunk{}, dstM, 0, c, holderRow{key: h, oor: oor}, map[string][]any{}, lo, hi, targeted, oorActive, keyOrderKnown(srcS))
	}
	cases := []struct {
		name      string
		srcS      *conn.Schema
		targeted  map[string]bool
		oor       bool
		lo, hi    []driver.Value
		inChunk   bool
		oorActive bool
		want      crossChunkVerdict
	}{
		{"targeted holder is safe (ci key)", ciS, map[string]bool{idS: true}, false, nil, nil, false, true, crossSafe},
		{"unaddressed in-chunk holder conflicts", intS, nil, false, nil, nil, true, true, crossConflict},
		{"no out-of-range pass: foreign holder conflicts", intS, nil, false, nil, nil, false, false, crossConflict},
		{"server-flagged holder is safe (ci key, no client order needed)", ciS, nil, true, nil, nil, false, true, crossSafe},
		{"unflagged in-range holder on a ci key is not provable", ciS, nil, false, nil, nil, false, true, crossConflict},
		{"unflagged holder the client orders out of range conflicts (int key)", intS, nil, false, []driver.Value{int64(1)}, []driver.Value{int64(50)}, false, true, crossConflict},
	}
	for _, cse := range cases {
		v, err := run(cse.srcS, cse.targeted, cse.oor, cse.lo, cse.hi, cse.inChunk, cse.oorActive)
		if err != nil {
			t.Errorf("%s: unexpected DB access or error: %v", cse.name, err)
			continue
		}
		if v != cse.want {
			t.Errorf("%s: got %v, want %v", cse.name, v, cse.want)
		}
	}
}

// TestWrittenTuplesNullAndDedup pins the O(delta) tracking: NULL tuples
// are not tracked, and identical tuples are deduplicated.
func TestWrittenTuplesNullAndDedup(t *testing.T) {
	cols := []conn.Column{
		{Name: "id", Family: conn.FamINT, RawType: "int"},
		{Name: "u", Family: conn.FamSTR, RawType: "varchar(10)", Nullable: true},
	}
	e := newTestEngineWithConstraints(t, cols, cols, []conn.UniqueConstraint{
		{Name: "uk_u", Cols: []string{"u"}},
	})
	ops := []op{
		{kind: opInsert, key: []any{int64(1)}, rows: [][]any{{int64(1), "x"}}},
		{kind: opInsert, key: []any{int64(2)}, rows: [][]any{{int64(2), "x"}}}, // dedup
		{kind: opInsert, key: []any{int64(3)}, rows: [][]any{{int64(3), nil}}}, // NULL: untracked
		{kind: opDelete, key: []any{int64(9)}},
	}
	w, _ := e.writtenTuples(ops)
	if len(w) != 1 {
		t.Fatalf("one constraint: %d", len(w))
	}
	if len(w[0]) != 1 {
		t.Fatalf("one deduplicated non-NULL tuple: %d (%v)", len(w[0]), w[0])
	}
}

// TestCrossChunkTrackingScalesWithDelta is the P1-6 memory-bound proof:
// the tracking structure holds only the tuples the ops actually write
// (O(delta)), never a table-wide owner map. A 10M-row table whose only
// diff is one row yields ONE tracked entry; a delta past the tracking
// cap escalates (the guard returns before any query — the nil Sides
// below prove no database access is attempted on that path).
func TestCrossChunkTrackingScalesWithDelta(t *testing.T) {
	cols := []conn.Column{
		{Name: "id", Family: conn.FamINT, RawType: "int"},
		{Name: "u", Family: conn.FamSTR, RawType: "varchar(10)"},
	}
	e := newTestEngineWithConstraints(t, cols, cols, []conn.UniqueConstraint{
		{Name: "uk_u", Cols: []string{"u"}},
	})
	// 10M-row table, one row differs: Diff emits a single op.
	ops := []op{{kind: opUpdate, key: []any{int64(1)}, rows: [][]any{{int64(1), "x"}}}}
	w, overflow := e.writtenTuples(ops)
	if overflow {
		t.Fatal("one tuple must not overflow the cap")
	}
	if len(w[0]) != 1 {
		t.Fatalf("one differing row must track exactly one tuple, tracked %d", len(w[0]))
	}
	// A delta past the cap: the guard must escalate WITHOUT touching the
	// database (nil Sides would panic on any query).
	big := make([]op, maxTrackedTuples+1)
	for i := range big {
		big[i] = op{kind: opUpdate, key: []any{int64(i)}, rows: [][]any{{int64(i), fmt.Sprint(i)}}}
	}
	v, err := e.crossChunkCheck(context.Background(), nil, nil, nil, nil, chunk.Chunk{}, nil, big, nil, nil, chunk.Pred{}, true)
	if err != nil {
		t.Fatalf("cap guard must not error without a database: %v", err)
	}
	if v != crossConflict {
		t.Fatalf("a delta past the tracking cap must escalate, got %v", v)
	}
}

// BenchmarkCrossChunkEscalateLargeDelta measures the 1M-row-scale
// escalation path (1M distinct written tuples): the check must stay in
// milliseconds — it is a cap test on the delta, not a table scan.
func BenchmarkCrossChunkEscalateLargeDelta(b *testing.B) {
	cols := []conn.Column{
		{Name: "id", Family: conn.FamINT, RawType: "int"},
		{Name: "u", Family: conn.FamSTR, RawType: "varchar(10)"},
	}
	e := newTestEngineWithConstraints(b, cols, cols, []conn.UniqueConstraint{
		{Name: "uk_u", Cols: []string{"u"}},
	})
	ops := make([]op, 1_000_000)
	for i := range ops {
		ops[i] = op{kind: opUpdate, key: []any{int64(i)}, rows: [][]any{{int64(i), fmt.Sprint(i)}}}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if v, _ := e.crossChunkCheck(context.Background(), nil, nil, nil, nil, chunk.Chunk{}, nil, ops, nil, nil, chunk.Pred{}, true); v != crossConflict {
			b.Fatal("expected escalation")
		}
	}
}
