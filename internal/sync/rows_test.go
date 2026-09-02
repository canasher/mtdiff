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
	return NewEngine(
		normalize.NewNormalizer(cols, opts),
		normalize.NewNormalizer(cols, opts),
		normalize.NewNormalizer(colsFor(cols, "id"), opts),
		normalize.NewNormalizer(colsFor(cols, "id"), opts),
		unique, colsFor(cols, "id"), cols)
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
