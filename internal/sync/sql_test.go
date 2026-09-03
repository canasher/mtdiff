package sync

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"mtdiff/internal/conn"
)

func testSchema(t *testing.T) *conn.Schema {
	t.Helper()
	return &conn.Schema{
		Table: "t_orders",
		Cols: []conn.Column{
			{Name: "id", Family: conn.FamINT, RawType: "int"},
			{Name: "name", Family: conn.FamSTR, RawType: "varchar(50)"},
			{Name: "amt", Family: conn.FamDECIMAL, RawType: "decimal(10,2)"},
			{Name: "note", Family: conn.FamSTR, RawType: "text", Nullable: true},
			{Name: "created", Family: conn.FamDATETIME, RawType: "datetime"},
		},
		Key:         []string{"id"},
		KeySource:   "primary",
		KeyIsUnique: true,
	}
}

func TestNewBuilderColumnSplit(t *testing.T) {
	b := NewBuilder("t", testSchema(t))
	if strings.Join(b.Cols, ",") != "id,name,amt,note,created" {
		t.Errorf("Cols = %v", b.Cols)
	}
	if strings.Join(b.SetCols, ",") != "name,amt,note,created" {
		t.Errorf("SetCols = %v", b.SetCols)
	}
	if strings.Join(b.KeyCols(), ",") != "id" {
		t.Errorf("KeyCols = %v", b.KeyCols())
	}
}

func TestBuilderInsert(t *testing.T) {
	b := NewBuilder("t", testSchema(t))
	// Driver values as actually scanned: DECIMAL and character columns
	// arrive as []byte and are rendered as quoted character strings (a hex
	// blob would make the server reject the DECIMAL / misread the text).
	got := b.Insert([]any{int64(7), "it's", []byte("1.50"), []byte("héllo"), time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)})
	want := "INSERT INTO `t` (`id`, `name`, `amt`, `note`, `created`) VALUES (7, 'it''s', '1.50', 'héllo', '2026-01-02 03:04:05')"
	if got != want {
		t.Errorf("Insert = %q, want %q", got, want)
	}
	// NULL value
	if got := b.Insert([]any{int64(1), nil, nil, nil, nil}); got != "INSERT INTO `t` (`id`, `name`, `amt`, `note`, `created`) VALUES (1, NULL, NULL, NULL, NULL)" {
		t.Errorf("Insert NULLs = %q", got)
	}
}

func TestBuilderInsertBatch(t *testing.T) {
	b := NewBuilder("t", testSchema(t))
	got, err := b.InsertBatch([][]any{{int64(1), "a", []byte("1"), nil, nil}, {int64(2), "b", []byte("2"), nil, nil}})
	if err != nil {
		t.Fatal(err)
	}
	want := "INSERT INTO `t` (`id`, `name`, `amt`, `note`, `created`) VALUES (1, 'a', '1', NULL, NULL), (2, 'b', '2', NULL, NULL)"
	if got != want {
		t.Errorf("InsertBatch = %q, want %q", got, want)
	}
	if _, err := b.InsertBatch(nil); err == nil {
		t.Error("empty batch must be an error")
	}
	if _, err := b.InsertBatch([][]any{{int64(1), "a"}}); err == nil {
		t.Error("short row must be an error")
	}
}

func TestBuilderUpdate(t *testing.T) {
	b := NewBuilder("t", testSchema(t))
	got := b.Update([]any{int64(7)}, []any{int64(7), "new", []byte("2.50"), nil, nil})
	want := "UPDATE `t` SET `name`='new', `amt`='2.50', `note`=NULL, `created`=NULL WHERE `id` = 7"
	if got != want {
		t.Errorf("Update = %q, want %q", got, want)
	}
}

func TestBuilderDelete(t *testing.T) {
	b := NewBuilder("t", testSchema(t))
	if got := b.Delete([]any{int64(9)}); got != "DELETE FROM `t` WHERE `id` = 9" {
		t.Errorf("Delete = %q", got)
	}
}

func TestBuilderKeyWhereNULL(t *testing.T) {
	s := &conn.Schema{
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
	b := NewBuilder("t", s)
	if got := b.Delete([]any{nil, int64(3)}); got != "DELETE FROM `t` WHERE `k1` IS NULL AND `k2` = 3" {
		t.Errorf("Delete NULL composite = %q", got)
	}
	if got := b.Update([]any{"x", nil}, []any{"x", nil, int64(1)}); got != "UPDATE `t` SET `v`=1 WHERE `k1` = 'x' AND `k2` IS NULL" {
		t.Errorf("Update NULL composite = %q", got)
	}
}

// TestBuilderKeyIndexOrder pins the case where the key's index order differs
// from the column order (e.g. PRIMARY KEY (b, a) over columns (a, b)). The
// key VALUES travel in index order, so the WHERE must pair each value to its
// own column; pairing in column-ordinal order swaps them and addresses the
// wrong row (silent corruption).
func TestBuilderKeyIndexOrder(t *testing.T) {
	s := &conn.Schema{
		Table: "t",
		Cols: []conn.Column{
			{Name: "a", Family: conn.FamINT, RawType: "int"},
			{Name: "b", Family: conn.FamINT, RawType: "int"},
			{Name: "v", Family: conn.FamSTR, RawType: "varchar(10)"},
		},
		Key:         []string{"b", "a"}, // index order, NOT column order
		KeySource:   "primary",
		KeyIsUnique: true,
	}
	b := NewBuilder("t", s)
	if strings.Join(b.KeyCols(), ",") != "b,a" {
		t.Fatalf("KeyCols = %v, want [b a]", b.KeyCols())
	}
	// key values in KEY order: b=9, a=4
	if got := b.Delete([]any{int64(9), int64(4)}); got != "DELETE FROM `t` WHERE `b` = 9 AND `a` = 4" {
		t.Errorf("Delete = %q (values swapped?)", got)
	}
	if got := b.Update([]any{int64(9), int64(4)}, []any{int64(4), int64(9), "x"}); got != "UPDATE `t` SET `v`='x' WHERE `b` = 9 AND `a` = 4" {
		t.Errorf("Update = %q (values swapped?)", got)
	}
}

// TestBuilderGeneratedKeyColumn pins that a generated column used in the key
// stays in the WHERE clause (it is readable) but is excluded from every write
// list (it is never written, P0-2). A key of a single generated column must
// not panic and must still yield a complete WHERE.
func TestBuilderGeneratedKeyColumn(t *testing.T) {
	s := &conn.Schema{
		Table: "t",
		Cols: []conn.Column{
			{Name: "g", Family: conn.FamINT, RawType: "int", Generated: true, GenStorage: "STORED"},
			{Name: "x", Family: conn.FamINT, RawType: "int"},
			{Name: "v", Family: conn.FamSTR, RawType: "varchar(10)"},
		},
		Key:         []string{"g"},
		KeySource:   "explicit",
		KeyIsUnique: true,
	}
	b := NewBuilder("t", s)
	// single generated-column key: no panic, WHERE still complete
	if got := b.Delete([]any{int64(5)}); got != "DELETE FROM `t` WHERE `g` = 5" {
		t.Errorf("Delete = %q", got)
	}
	if got := b.Update([]any{int64(5)}, []any{int64(5), int64(7), "x"}); got != "UPDATE `t` SET `x`=7, `v`='x' WHERE `g` = 5" {
		t.Errorf("Update = %q", got)
	}
	// INSERT writes x and v, never the generated column
	if got, _ := b.InsertBatch([][]any{{int64(5), int64(7), "x"}}); got != "INSERT INTO `t` (`x`, `v`) VALUES (7, 'x')" {
		t.Errorf("InsertBatch = %q", got)
	}
}

// TestBuilderCompositeGeneratedKey pins the mixed case: a composite key that
// includes a generated column member. Both members must appear in the WHERE
// (in key order); only the plain member is written.
func TestBuilderCompositeGeneratedKey(t *testing.T) {
	s := &conn.Schema{
		Table: "t",
		Cols: []conn.Column{
			{Name: "x", Family: conn.FamINT, RawType: "int"},
			{Name: "g", Family: conn.FamINT, RawType: "int", Generated: true, GenStorage: "STORED"},
			{Name: "v", Family: conn.FamSTR, RawType: "varchar(10)"},
		},
		Key:         []string{"g", "x"}, // generated member first
		KeySource:   "explicit",
		KeyIsUnique: true,
	}
	b := NewBuilder("t", s)
	if strings.Join(b.KeyCols(), ",") != "g,x" {
		t.Fatalf("KeyCols = %v, want [g x]", b.KeyCols())
	}
	if got := b.Delete([]any{int64(9), int64(4)}); got != "DELETE FROM `t` WHERE `g` = 9 AND `x` = 4" {
		t.Errorf("Delete = %q", got)
	}
	// INSERT writes x (plain key) and v, never the generated member
	if got, _ := b.InsertBatch([][]any{{int64(4), int64(9), "x"}}); got != "INSERT INTO `t` (`x`, `v`) VALUES (4, 'x')" {
		t.Errorf("InsertBatch = %q", got)
	}
}

func TestBuilderTruncate(t *testing.T) {
	b := NewBuilder("t", testSchema(t))
	if got := b.Truncate(); got != "TRUNCATE TABLE `t`" {
		t.Errorf("Truncate = %q", got)
	}
}

// TestLiteralForFamilies pins the family-aware rendering: only genuine binary
// columns become hex blobs; character-family values the driver delivers as
// []byte become quoted strings; BIT becomes a bit literal. A regression here
// silently corrupts synced data (e.g. a DECIMAL rendered as a hex blob is
// rejected by the server).
func TestLiteralForFamilies(t *testing.T) {
	cases := []struct {
		fam, in string
		v       any
		want    string
	}{
		{conn.FamINT, "1", int64(1), "1"},
		{conn.FamUINT, "7", uint64(7), "7"},
		{conn.FamFLOAT, "4.5", float32(4.5), "4.5"},
		{conn.FamDOUBLE, "5.25", float64(5.25), "5.25"},
		{conn.FamYEAR, "2024", int64(2024), "2024"},
		{conn.FamDATETIME, "dt", time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), "'2024-01-02 03:04:05'"},
		{conn.FamDECIMAL, "3.50", []byte("3.50"), "'3.50'"},
		{conn.FamDECIMAL, "3.50", "3.50", "'3.50'"},
		{conn.FamSTR, "hello", []byte("hello"), "'hello'"},
		{conn.FamSTR, "héllo", []byte("héllo"), "'héllo'"},
		{conn.FamENUM, "a", []byte("a"), "'a'"},
		{conn.FamSET, "x", []byte("x"), "'x'"},
		{conn.FamJSON, "{}", []byte(`{"k":1}`), `'{"k":1}'`},
		{conn.FamTIME, "12:34:56", []byte("12:34:56"), "'12:34:56'"},
		{conn.FamBIT, "0b1010", []byte{0x0a}, "b'00001010'"},
		{conn.FamBYTES, "binary", []byte{0x01, 0x02, 0x03}, "X'010203'"},
		{conn.FamDECIMAL, "nil", nil, "NULL"},
	}
	for _, c := range cases {
		if got := literalFor(c.fam, c.v); got != c.want {
			t.Errorf("literalFor(%s, %v) = %q, want %q", c.fam, c.v, got, c.want)
		}
	}
}

// TestBuilderMixedFamilies renders a row spanning the tricky families and
// checks the full INSERT, including a BIT and a BLOB in the same statement.
func TestBuilderMixedFamilies(t *testing.T) {
	s := &conn.Schema{
		Table: "t",
		Cols: []conn.Column{
			{Name: "id", Family: conn.FamINT, RawType: "int"},
			{Name: "amt", Family: conn.FamDECIMAL, RawType: "decimal(10,2)"},
			{Name: "tag", Family: conn.FamENUM, RawType: "enum('a','b')"},
			{Name: "blob", Family: conn.FamBYTES, RawType: "varbinary(16)"},
			{Name: "bit", Family: conn.FamBIT, RawType: "bit(8)"},
		},
		Key: []string{"id"},
	}
	b := NewBuilder("t", s)
	got := b.Insert([]any{int64(1), []byte("9.99"), []byte("b"), []byte{0xde, 0xad}, []byte{0x01}})
	want := "INSERT INTO `t` (`id`, `amt`, `tag`, `blob`, `bit`) VALUES (1, '9.99', 'b', X'dead', b'00000001')"
	if got != want {
		t.Errorf("Insert = %q, want %q", got, want)
	}
}

func TestBuilderQuotedIdentifiers(t *testing.T) {
	s := &conn.Schema{
		Table: "we`ird",
		Cols:  []conn.Column{{Name: "a`b", Family: conn.FamINT, RawType: "int"}, {Name: "v", Family: conn.FamINT, RawType: "int"}},
		Key:   []string{"a`b"},
	}
	b := NewBuilder("we`ird", s)
	if got := b.Delete([]any{int64(1)}); got != "DELETE FROM `we``ird` WHERE `a``b` = 1" {
		t.Errorf("backtick escaping = %q", got)
	}
}

// TestBindArg pins the raw-value -> bindable-argument conversion (P0-3):
// character-family values the driver delivers as []byte bind as strings,
// BIT binds as its decimal value (the byte pattern is meaningless as a
// string), a uint64 past int64 range binds as a decimal string.
func TestBindArg(t *testing.T) {
	cases := []struct {
		fam  string
		in   any
		want any
	}{
		{conn.FamINT, int64(7), int64(7)},
		{conn.FamINT, int(7), int64(7)},
		{conn.FamUINT, uint64(7), int64(7)},
		{conn.FamUINT, uint64(math.MaxInt64) + 1, "9223372036854775808"},
		{conn.FamUINT, uint64(math.MaxUint64), "18446744073709551615"},
		{conn.FamSTR, []byte("héllo"), "héllo"},
		{conn.FamSTR, []byte(`C:\abc\def`), `C:\abc\def`},
		{conn.FamSTR, []byte("a'b"), "a'b"},
		{conn.FamJSON, []byte(`{"path":"C:\\abc"}`), `{"path":"C:\\abc"}`},
		{conn.FamDECIMAL, []byte("3.50"), "3.50"},
		{conn.FamENUM, []byte("a"), "a"},
		{conn.FamBIT, []byte{0x0a}, "10"},
		{conn.FamBIT, []byte{0xff, 0x01}, "65281"},
		{conn.FamBYTES, []byte{0xde, 0xad}, "\xde\xad"},
		{conn.FamDATETIME, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{conn.FamSTR, nil, nil},
	}
	for _, c := range cases {
		if got := bindArg(c.fam, c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("bindArg(%s, %v) = %#v, want %#v", c.fam, c.in, got, c.want)
		}
	}
}

// TestExecMethodsAreParameterized pins P0-3: the executable statements
// carry no rendered value — only "?" placeholders and a separate argument
// list (a value like C:\abc\def cannot be mangled client-side because it
// is never text in the statement).
func TestExecMethodsAreParameterized(t *testing.T) {
	b := NewBuilder("t", testSchema(t))
	row := []any{int64(7), `C:\abc\def`, []byte("1.50"), []byte(`中文\测试'`), time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}

	stmt, args := b.InsertExec(row)
	if strings.Contains(stmt, "\\") || strings.Contains(stmt, "中文") || strings.Contains(stmt, "'") {
		t.Errorf("executable INSERT must not render any value: %s", stmt)
	}
	if stmt != "INSERT INTO `t` (`id`, `name`, `amt`, `note`, `created`) VALUES (?, ?, ?, ?, ?)" {
		t.Errorf("InsertExec = %q", stmt)
	}
	if len(args) != 5 || args[1] != `C:\abc\def` || args[3] != "中文\\测试'" {
		t.Errorf("InsertExec args = %#v", args)
	}

	batch, bargs, err := b.InsertBatchExec([][]any{row, row})
	if err != nil {
		t.Fatal(err)
	}
	if batch != "INSERT INTO `t` (`id`, `name`, `amt`, `note`, `created`) VALUES (?, ?, ?, ?, ?), (?, ?, ?, ?, ?)" {
		t.Errorf("InsertBatchExec = %q", batch)
	}
	if len(bargs) != 10 {
		t.Errorf("InsertBatchExec args = %d, want 10", len(bargs))
	}

	u, uargs := b.UpdateExec([]any{int64(7)}, row)
	if strings.Contains(u, `\`) || strings.Contains(u, "中文") {
		t.Errorf("executable UPDATE must not render any value: %s", u)
	}
	if u != "UPDATE `t` SET `name` = ?, `amt` = ?, `note` = ?, `created` = ? WHERE `id` = ?" {
		t.Errorf("UpdateExec = %q", u)
	}
	if len(uargs) != 5 {
		t.Errorf("UpdateExec args = %d, want 5", len(uargs))
	}

	d, dargs := b.DeleteExec([]any{int64(7)})
	if d != "DELETE FROM `t` WHERE `id` = ?" || len(dargs) != 1 || dargs[0] != int64(7) {
		t.Errorf("DeleteExec = %q args=%v", d, dargs)
	}
}

// TestBuilderGeneratedColumnExcludedFromWrites pins P0-2: a generated
// column is COMPARED (it stays in Cols) but never appears in the column
// lists of an INSERT or in the SET clause of an UPDATE (a STORED column
// rejects explicit values; a VIRTUAL one has no storage).
func TestBuilderGeneratedColumnExcludedFromWrites(t *testing.T) {
	s := &conn.Schema{
		Table: "t",
		Cols: []conn.Column{
			{Name: "id", Family: conn.FamINT, RawType: "int"},
			{Name: "price", Family: conn.FamDECIMAL, RawType: "decimal(10,2)"},
			{Name: "total", Family: conn.FamDECIMAL, RawType: "decimal(12,2)", Generated: true, GenStorage: "STORED"},
			{Name: "label", Family: conn.FamSTR, RawType: "varchar(10)", Generated: true, GenStorage: "VIRTUAL"},
		},
		Key:         []string{"id"},
		KeySource:   "primary",
		KeyIsUnique: true,
	}
	b := NewBuilder("t", s)
	if strings.Join(b.Cols, ",") != "id,price,total,label" {
		t.Errorf("generated columns must stay COMPARED: Cols = %v", b.Cols)
	}
	if strings.Join(b.writeCols, ",") != "id,price" {
		t.Errorf("write columns must exclude generated columns: %v", b.writeCols)
	}
	if strings.Join(b.SetCols, ",") != "price" {
		t.Errorf("SET columns must exclude generated columns: %v", b.SetCols)
	}
	got := b.Insert([]any{int64(1), []byte("9.99"), []byte("19.98"), "x"})
	want := "INSERT INTO `t` (`id`, `price`) VALUES (1, '9.99')"
	if got != want {
		t.Errorf("Insert = %q, want %q", got, want)
	}
	got = b.Update([]any{int64(1)}, []any{int64(1), []byte("8.88"), []byte("17.76"), "y"})
	want = "UPDATE `t` SET `price`='8.88' WHERE `id` = 1"
	if got != want {
		t.Errorf("Update = %q, want %q", got, want)
	}
	stmt, args := b.InsertExec([]any{int64(1), []byte("9.99"), []byte("19.98"), "x"})
	if stmt != "INSERT INTO `t` (`id`, `price`) VALUES (?, ?)" || len(args) != 2 {
		t.Errorf("InsertExec = %q args=%v", stmt, args)
	}
}
