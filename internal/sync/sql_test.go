package sync

import (
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
