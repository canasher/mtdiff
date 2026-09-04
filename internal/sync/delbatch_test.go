package sync

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"mtdiff/internal/conn"
)

type helper interface{ Helper() }

func singleKeySchema(t helper) *conn.Schema {
	t.Helper()
	return &conn.Schema{
		Table: "t",
		Cols: []conn.Column{
			{Name: "id", Family: conn.FamINT, RawType: "int"},
			{Name: "v", Family: conn.FamSTR, RawType: "varchar(10)"},
		},
		Key:         []string{"id"},
		KeySource:   "primary",
		KeyIsUnique: true,
	}
}

func compositeKeySchema() *conn.Schema {
	return &conn.Schema{
		Table: "t",
		Cols: []conn.Column{
			{Name: "k1", Family: conn.FamSTR, RawType: "varchar(10)", Nullable: true},
			{Name: "k2", Family: conn.FamINT, RawType: "int"},
			{Name: "v", Family: conn.FamINT, RawType: "int"},
		},
		Key:         []string{"k1", "k2"},
		KeySource:   "explicit",
		KeyIsUnique: false,
	}
}

// TestDeleteBatchExecSingleColumn is spec item 15's single-key half: a
// batch of int keys renders one IN-list with one bound placeholder per
// key (no per-row round trip — P2).
func TestDeleteBatchExecSingleColumn(t *testing.T) {
	b := NewBuilder("t", singleKeySchema(t))
	stmt, args, err := b.DeleteBatchExec([][]any{{int64(1)}, {int64(2)}, {int64(3)}})
	if err != nil {
		t.Fatal(err)
	}
	if stmt != "DELETE FROM `t` WHERE `id` IN (?, ?, ?)" {
		t.Errorf("DeleteBatchExec = %q", stmt)
	}
	if len(args) != 3 || args[0] != int64(1) || args[2] != int64(3) {
		t.Errorf("args = %#v", args)
	}
}

// TestDeleteBatchExecNULL is spec item 17: a NULL key component travels
// as IS NULL (IN never matches NULL), mixed with a bound value in the
// same batch; an all-NULL batch is the bare IS NULL term alone (a
// unique key has at most one NULL row, so the term addresses exactly
// the batch).
func TestDeleteBatchExecNULL(t *testing.T) {
	s := &conn.Schema{
		Table: "t",
		Cols: []conn.Column{
			{Name: "id", Family: conn.FamINT, RawType: "int", Nullable: true},
			{Name: "v", Family: conn.FamSTR, RawType: "varchar(10)"},
		},
		Key:         []string{"id"},
		KeySource:   "primary",
		KeyIsUnique: true,
	}
	b := NewBuilder("t", s)
	stmt, args, err := b.DeleteBatchExec([][]any{{int64(1)}, {nil}, {int64(2)}, {nil}})
	if err != nil {
		t.Fatal(err)
	}
	if stmt != "DELETE FROM `t` WHERE `id` IN (?, ?) OR `id` IS NULL" {
		t.Errorf("mixed NULL batch = %q", stmt)
	}
	if len(args) != 2 || args[0] != int64(1) || args[1] != int64(2) {
		t.Errorf("args = %#v, want the non-NULL keys only", args)
	}

	stmt, args, err = b.DeleteBatchExec([][]any{{nil}, {nil}})
	if err != nil {
		t.Fatal(err)
	}
	if stmt != "DELETE FROM `t` WHERE `id` IS NULL" {
		t.Errorf("all-NULL batch = %q", stmt)
	}
	if len(args) != 0 {
		t.Errorf("all-NULL batch args = %#v, want none", args)
	}
}

// TestDeleteBatchExecComposite is spec item 15: a composite key renders
// the OR of per-row AND tuples (IN takes single values only), with a
// NULL component as IS NULL.
func TestDeleteBatchExecComposite(t *testing.T) {
	b := NewBuilder("t", compositeKeySchema())
	stmt, args, err := b.DeleteBatchExec([][]any{
		{"a", int64(1)},
		{nil, int64(2)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stmt != "DELETE FROM `t` WHERE (`k1` = ? AND `k2` = ?) OR (`k1` IS NULL AND `k2` = ?)" {
		t.Errorf("composite DeleteBatchExec = %q", stmt)
	}
	want := []any{"a", int64(1), int64(2)}
	if len(args) != len(want) {
		t.Fatalf("args = %#v, want 3", args)
	}
	for i, w := range want {
		if args[i] != w {
			t.Errorf("args[%d] = %#v, want %#v", i, args[i], w)
		}
	}
}

// TestDeleteBatchExecAdversarialValue is spec item 16 at the statement
// level: under NO_BACKSLASH_ESCAPES the values must never be rendered
// into the statement text (a backslash or quote in a key would break
// the SQL if interpolated) — the statement carries placeholders only
// and the raw values travel as bound arguments, byte for byte.
func TestDeleteBatchExecAdversarialValue(t *testing.T) {
	s := &conn.Schema{
		Table: "t",
		Cols: []conn.Column{
			{Name: "id", Family: conn.FamSTR, RawType: "varchar(64)"},
			{Name: "v", Family: conn.FamINT, RawType: "int"},
		},
		Key:         []string{"id"},
		KeySource:   "primary",
		KeyIsUnique: true,
	}
	b := NewBuilder("t", s)
	hostile := []string{
		`C:\abc\def`,
		`'); DROP TABLE t; --`,
		`中文\测试'`,
		"x' OR '1'='1",
	}
	keys := make([][]any, len(hostile))
	for i, h := range hostile {
		keys[i] = []any{[]byte(h)} // the driver delivers strings as []byte
	}
	stmt, args, err := b.DeleteBatchExec(keys)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hostile {
		if strings.Contains(stmt, h) || strings.Contains(stmt, `\`) || strings.Contains(stmt, "'") {
			t.Errorf("statement text must carry no data: %s", stmt)
		}
	}
	if n := strings.Count(stmt, "?"); n != len(hostile) {
		t.Errorf("placeholders = %d, want %d: %s", n, len(hostile), stmt)
	}
	for i, h := range hostile {
		if args[i] != h {
			t.Errorf("arg %d = %#v, want the raw value %#v", i, args[i], h)
		}
	}
}

// TestDeleteBatchExecPlaceholderCount is spec item 18: the statement's
// placeholder count is exactly the batch's parameter count (one per key
// component), so a caller that sizes the batch by deleteBatchCap never
// exceeds the bind-parameter budget.
func TestDeleteBatchExecPlaceholderCount(t *testing.T) {
	mk := func(n int, keyCols int) *conn.Schema {
		cols := make([]conn.Column, keyCols+1)
		for i := 0; i < keyCols; i++ {
			cols[i] = conn.Column{Name: fmt.Sprintf("k%d", i), Family: conn.FamINT, RawType: "int"}
		}
		cols[keyCols] = conn.Column{Name: "v", Family: conn.FamINT, RawType: "int"}
		key := make([]string, keyCols)
		for i := 0; i < keyCols; i++ {
			key[i] = fmt.Sprintf("k%d", i)
		}
		return &conn.Schema{Table: "t", Cols: cols, Key: key, KeySource: "explicit", KeyIsUnique: false}
	}
	for _, keyCols := range []int{1, 2, 7} {
		b := NewBuilder("t", mk(4, keyCols))
		keys := make([][]any, 9)
		for i := range keys {
			k := make([]any, keyCols)
			for j := range k {
				k[j] = int64(i*10 + j)
			}
			keys[i] = k
		}
		stmt, args, err := b.DeleteBatchExec(keys)
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(stmt, "?"); n != 9*keyCols {
			t.Errorf("keyCols=%d: placeholders = %d, want %d", keyCols, n, 9*keyCols)
		}
		if len(args) != 9*keyCols {
			t.Errorf("keyCols=%d: args = %d, want %d", keyCols, len(args), 9*keyCols)
		}
	}
}

// TestDeleteBatchCap is spec item 18's sizing rule: the batch is the
// configured batch capped by the bind-parameter budget per key component,
// floored at 1 (a key wider than the whole budget still deletes, one row
// per statement).
func TestDeleteBatchCap(t *testing.T) {
	cases := []struct {
		batch, keyCols, want int
	}{
		{500, 1, 500},      // below the budget: unchanged
		{50000, 1, 50000},  // still below
		{100000, 1, 60000}, // capped at the budget
		{100000, 2, 30000}, // composite: halved
		{100000, 7, 8571},  // wide composite key
		{1, 60001, 1},      // wider than the budget: floor 1
		{0, 1, 1},          // zero batch: floor 1
		{0, 0, 1},          // keyless caller: floor 1
	}
	for _, c := range cases {
		if got := deleteBatchCap(c.batch, c.keyCols); got != c.want {
			t.Errorf("deleteBatchCap(%d, %d) = %d, want %d", c.batch, c.keyCols, got, c.want)
		}
	}
}

// TestDeleteBatchExecEmptyAndKeyless pins the two hard errors: an empty
// batch is a caller bug (a zero-key DELETE is meaningless), and a keyless
// builder panics (a keyless table never reaches the row-level path —
// DecidePlan routes it to FULL).
func TestDeleteBatchExecEmptyAndKeyless(t *testing.T) {
	b := NewBuilder("t", singleKeySchema(t))
	if _, _, err := b.DeleteBatchExec(nil); err == nil {
		t.Error("an empty batch must error")
	}
	keyless := &conn.Schema{
		Table: "t",
		Cols:  []conn.Column{{Name: "v", Family: conn.FamINT, RawType: "int"}},
	}
	defer func() {
		if recover() == nil {
			t.Error("a keyless DeleteBatchExec must panic")
		}
	}()
	NewBuilder("t", keyless).DeleteBatchExec([][]any{{int64(1)}})
}

// fakeResult is a sql.Result that reports a fixed affected-row count.
type fakeResult int64

func (r fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (r fakeResult) RowsAffected() (int64, error) { return int64(r), nil }

// recordExec records every statement the executor runs and lets the test
// fail one of them (mid-stream failure, spec item 19).
type recordExec struct {
	stmts  []string
	args   [][]any
	failAt int // fail the Nth statement (1-based), 0 = never
	failed bool
}

func (r *recordExec) exec(stmt string, args ...any) (sql.Result, error) {
	if r.failAt > 0 && len(r.stmts) == r.failAt-1 {
		r.failed = true
		return nil, fmt.Errorf("injected failure on statement %d", len(r.stmts)+1)
	}
	r.stmts = append(r.stmts, stmt)
	r.args = append(r.args, args)
	// affected rows the way the server would report them: a batched
	// delete touches one row per key placeholder, a multi-row insert one
	// per value tuple, everything else one
	na := int64(1)
	switch {
	case strings.HasPrefix(stmt, "DELETE"):
		na = int64(strings.Count(stmt, "?"))
	case strings.HasPrefix(stmt, "INSERT"):
		na = int64(strings.Count(stmt, "), (") + 1)
	}
	return fakeResult(na), nil
}

// TestRunOpsGroupBatchedDeletes is the P2 no-per-row-RTT guarantee at the
// executor level: a group of 7 deletes with a batch of 3 issues three
// batched statements (3+3+1), never seven single-row DELETEs.
func TestRunOpsGroupBatchedDeletes(t *testing.T) {
	b := NewBuilder("t", singleKeySchema(t))
	ops := make([]op, 7)
	for i := range ops {
		ops[i] = op{kind: opDelete, key: []any{int64(i)}}
	}
	re := &recordExec{}
	st := &Stats{Table: "t"}
	c := &groupCounts{}
	if err := runOpsGroup(ops, b, 3, deleteBatchCap(3, 1), 0, st, c, re.exec); err != nil {
		t.Fatal(err)
	}
	if len(re.stmts) != 3 {
		t.Fatalf("statements = %d, want 3 (batches of 3+3+1): %v", len(re.stmts), re.stmts)
	}
	if re.stmts[0] != "DELETE FROM `t` WHERE `id` IN (?, ?, ?)" {
		t.Errorf("first statement = %q", re.stmts[0])
	}
	if c.del != 7 || c.ins != 0 || c.upd != 0 {
		t.Errorf("counts = del %d / ins %d / upd %d, want 7/0/0", c.del, c.ins, c.upd)
	}
	if st.MaxBufferedDeleteKeys != 3 {
		t.Errorf("MaxBufferedDeleteKeys = %d, want 3 (the batch, not the table)", st.MaxBufferedDeleteKeys)
	}
}

// TestRunOpsGroupDeletePrecedesDependentOps pins the engine order: a key
// group's deletes flush before the first update or insert of the group
// (unique slots freed first), and an interleaved update splits the delete
// batch at its position.
func TestRunOpsGroupDeletePrecedesDependentOps(t *testing.T) {
	b := NewBuilder("t", singleKeySchema(t))
	row := func(id int64, v string) []any { return []any{id, v} } // Cols order: id, v
	ops := []op{
		{kind: opDelete, key: []any{int64(1)}},
		{kind: opUpdate, key: []any{int64(2)}, rows: [][]any{row(2, "b")}},
		{kind: opDelete, key: []any{int64(3)}},
		{kind: opInsert, rows: [][]any{row(4, "d")}},
	}
	re := &recordExec{}
	st := &Stats{Table: "t"}
	c := &groupCounts{}
	if err := runOpsGroup(ops, b, 100, deleteBatchCap(100, 1), 0, st, c, re.exec); err != nil {
		t.Fatal(err)
	}
	// delete(1) | update(2) | delete(3) | insert(4); a one-key batch
	// renders the degenerate IN-list
	want := []string{
		"DELETE FROM `t` WHERE `id` IN (?)",
		"UPDATE `t` SET `v` = ? WHERE `id` = ?",
		"DELETE FROM `t` WHERE `id` IN (?)",
		"INSERT INTO `t` (`id`, `v`) VALUES (?, ?)",
	}
	if len(re.stmts) != len(want) {
		t.Fatalf("statements = %d, want %d: %v", len(re.stmts), len(want), re.stmts)
	}
	for i, w := range want {
		if re.stmts[i] != w {
			t.Errorf("statement %d = %q, want %q", i, re.stmts[i], w)
		}
	}
	if c.del != 2 || c.upd != 1 || c.ins != 1 {
		t.Errorf("counts = del %d / upd %d / ins %d, want 2/1/1", c.del, c.upd, c.ins)
	}
}

// TestRunOpsGroupRewriteBarrier is the opRewrite ordering: the group's own
// deletes flush BEFORE its re-inserts (the re-inserts need the unique
// slots the deletes free), and pending deletes from earlier ops flush
// before the rewrite's delete batch.
func TestRunOpsGroupRewriteBarrier(t *testing.T) {
	s := &conn.Schema{
		Table: "t",
		Cols: []conn.Column{
			{Name: "id", Family: conn.FamINT, RawType: "int"},
			{Name: "code", Family: conn.FamSTR, RawType: "varchar(10)"},
			{Name: "n", Family: conn.FamINT, RawType: "int"},
		},
		Key:         []string{"id"},
		KeySource:   "primary",
		KeyIsUnique: true,
	}
	b := NewBuilder("t", s)
	ops := []op{
		{kind: opDelete, key: []any{int64(1)}},
		{kind: opRewrite, key: []any{int64(2)}, delKeys: [][]any{{int64(2)}, {int64(5)}},
			rows: [][]any{{int64(2), "x", int64(20)}, {int64(5), "y", int64(50)}}},
	}
	re := &recordExec{}
	st := &Stats{Table: "t"}
	c := &groupCounts{}
	if err := runOpsGroup(ops, b, 100, deleteBatchCap(100, 1), 0, st, c, re.exec); err != nil {
		t.Fatal(err)
	}
	// delete(1) | delete(2,5) | insert(2,5); a one-key batch renders
	// the degenerate IN-list
	want := []string{
		"DELETE FROM `t` WHERE `id` IN (?)",
		"DELETE FROM `t` WHERE `id` IN (?, ?)",
		"INSERT INTO `t` (`id`, `code`, `n`) VALUES (?, ?, ?), (?, ?, ?)",
	}
	if len(re.stmts) != len(want) {
		t.Fatalf("statements = %d, want %d: %v", len(re.stmts), len(want), re.stmts)
	}
	for i, w := range want {
		if re.stmts[i] != w {
			t.Errorf("statement %d = %q, want %q", i, re.stmts[i], w)
		}
	}
	// the re-inserts carry the rewrite group's rows, bound as data
	last := re.args[len(re.args)-1]
	if len(last) != 6 || last[0] != int64(2) || last[1] != "x" || last[5] != int64(50) {
		t.Errorf("re-insert args = %#v", last)
	}
	if c.del != 3 || c.ins != 2 {
		t.Errorf("counts = del %d / ins %d, want 3/2", c.del, c.ins)
	}
}

// TestRunOpsGroupMidStreamFailure is spec item 19 at the executor level:
// a failure on the second statement stops the group — the statements
// already run are the committed progress (the caller's transaction
// rollback is what undoes the failing group), nothing after the failure
// is attempted, and the group's counters credit only the completed
// statements.
func TestRunOpsGroupMidStreamFailure(t *testing.T) {
	b := NewBuilder("t", singleKeySchema(t))
	ops := []op{
		{kind: opDelete, key: []any{int64(1)}},
		{kind: opDelete, key: []any{int64(2)}}, // same batch as op 1 (batch 100)
		{kind: opUpdate, key: []any{int64(3)}, rows: [][]any{{int64(3), "c", int64(30)}}},
		{kind: opInsert, rows: [][]any{{int64(4), "d", int64(40)}}},
	}
	re := &recordExec{failAt: 2} // the batched delete runs, the update fails
	st := &Stats{Table: "t"}
	c := &groupCounts{}
	err := runOpsGroup(ops, b, 100, deleteBatchCap(100, 1), 0, st, c, re.exec)
	if err == nil || !re.failed {
		t.Fatalf("expected the injected failure, err = %v", err)
	}
	// both deletes share one batch (batch 100): one statement ran, then
	// the update failed — nothing after the failure is attempted
	if len(re.stmts) != 1 {
		t.Fatalf("statements after the failure = %d, want 1 (the batched delete only): %v", len(re.stmts), re.stmts)
	}
	// only the completed statements are credited
	if c.del != 2 || c.upd != 0 || c.ins != 0 {
		t.Errorf("counts past the failure = del %d / upd %d / ins %d, want 2/0/0", c.del, c.upd, c.ins)
	}
}

// TestRunOpsGroupOversizedRow is the max_allowed_packet guard: a single
// row that exceeds the rendered-bytes budget is an explicit error (a row
// cannot be split across statements), not a silent split or a crash.
func TestRunOpsGroupOversizedRow(t *testing.T) {
	b := NewBuilder("t", singleKeySchema(t))
	big := strings.Repeat("x", 100)
	ops := []op{{kind: opInsert, rows: [][]any{{int64(1), big}}}}
	re := &recordExec{}
	st := &Stats{Table: "t"}
	c := &groupCounts{}
	err := runOpsGroup(ops, b, 100, deleteBatchCap(100, 1), 10, st, c, re.exec)
	if err == nil || !strings.Contains(err.Error(), "max-allowed-packet") {
		t.Fatalf("oversized row must error with the packet hint, got: %v", err)
	}
	if len(re.stmts) != 0 {
		t.Errorf("an oversized row must not execute, statements = %v", re.stmts)
	}
}
