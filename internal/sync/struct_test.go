package sync

import (
	"strings"
	"testing"

	"mtdiff/internal/conn"
)

// metaCol builds a ColMeta with the family set explicitly (tests need the
// family for the DATETIME/TIMESTAMP carve-out; production derives it from
// the type string via classify).
func metaCol(name, typ, fam string, nullable bool) conn.ColMeta {
	c := conn.ColMeta{Column: conn.Column{Name: name, RawType: typ, Nullable: nullable}}
	c.Family = fam
	return c
}

func kindsOf(t *testing.T, changes []Change) []ChangeKind {
	t.Helper()
	out := make([]ChangeKind, 0, len(changes))
	for _, ch := range changes {
		out = append(out, ch.Kind)
	}
	return out
}

func findChange(t *testing.T, changes []Change, kind ChangeKind) *Change {
	t.Helper()
	for i := range changes {
		if changes[i].Kind == kind {
			return &changes[i]
		}
	}
	t.Fatalf("no %v change in %v", kind, kindsOf(t, changes))
	return nil
}

func TestDiffStructureIdentical(t *testing.T) {
	src := &conn.Struct{Table: "t", Cols: []conn.ColMeta{
		metaCol("id", "int", conn.FamINT, false),
		metaCol("name", "varchar(50)", conn.FamSTR, true),
	}, Indexes: []conn.Index{{Name: "PRIMARY", Unique: true, Cols: []string{"id"}}}}
	dst := &conn.Struct{Table: "t", Cols: []conn.ColMeta{
		metaCol("id", "int", conn.FamINT, false),
		metaCol("name", "varchar(50)", conn.FamSTR, true),
	}, Indexes: []conn.Index{{Name: "PRIMARY", Unique: true, Cols: []string{"id"}}}}
	changes, err := DiffStructure(src, dst, "", "")
	if err != nil {
		t.Fatalf("DiffStructure: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("expected no changes, got %v", changes)
	}
}

func TestDiffStructureColumns(t *testing.T) {
	cases := []struct {
		name     string
		src, dst *conn.Struct
		wantNone bool // expect no changes at all
		wantKind ChangeKind
		check    func(t *testing.T, ch *Change)
	}{
		{
			name: "add column in the middle",
			src: &conn.Struct{Cols: []conn.ColMeta{
				metaCol("id", "int", conn.FamINT, false),
				metaCol("mid", "int", conn.FamINT, true),
				metaCol("end", "int", conn.FamINT, true),
			}},
			dst: &conn.Struct{Cols: []conn.ColMeta{
				metaCol("id", "int", conn.FamINT, false),
				metaCol("end", "int", conn.FamINT, true),
			}},
			wantKind: ChangeAddColumn,
			check: func(t *testing.T, ch *Change) {
				if ch.Col.Name != "mid" || ch.After != "id" || ch.First {
					t.Fatalf("wrong placement: %+v", ch)
				}
			},
		},
		{
			name: "add leading column uses FIRST",
			src: &conn.Struct{Cols: []conn.ColMeta{
				metaCol("new", "int", conn.FamINT, true),
				metaCol("id", "int", conn.FamINT, false),
			}},
			dst: &conn.Struct{Cols: []conn.ColMeta{
				metaCol("id", "int", conn.FamINT, false),
			}},
			wantKind: ChangeAddColumn,
			check: func(t *testing.T, ch *Change) {
				if !ch.First || ch.After != "" {
					t.Fatalf("wrong placement: %+v", ch)
				}
			},
		},
		{
			name:     "type drift",
			src:      &conn.Struct{Cols: []conn.ColMeta{metaCol("id", "bigint", conn.FamINT, false)}},
			dst:      &conn.Struct{Cols: []conn.ColMeta{metaCol("id", "int", conn.FamINT, false)}},
			wantKind: ChangeModifyColumn,
		},
		{
			name:     "nullability drift",
			src:      &conn.Struct{Cols: []conn.ColMeta{metaCol("id", "int", conn.FamINT, false)}},
			dst:      &conn.Struct{Cols: []conn.ColMeta{metaCol("id", "int", conn.FamINT, true)}},
			wantKind: ChangeModifyColumn,
		},
		{
			name:     "default drift",
			src:      &conn.Struct{Cols: []conn.ColMeta{{Column: conn.Column{Name: "id", RawType: "int", Nullable: false, Family: conn.FamINT}, Default: "0", HasDefault: true}}},
			dst:      &conn.Struct{Cols: []conn.ColMeta{metaCol("id", "int", conn.FamINT, false)}},
			wantKind: ChangeModifyColumn,
		},
		{
			name:     "collation drift",
			src:      &conn.Struct{Cols: []conn.ColMeta{{Column: conn.Column{Name: "n", RawType: "varchar(10)", Nullable: true, Collation: "utf8mb4_0900_ai_ci", Family: conn.FamSTR}, Charset: "utf8mb4"}}},
			dst:      &conn.Struct{Cols: []conn.ColMeta{{Column: conn.Column{Name: "n", RawType: "varchar(10)", Nullable: true, Collation: "utf8mb4_general_ci", Family: conn.FamSTR}, Charset: "utf8mb4"}}},
			wantKind: ChangeModifyColumn,
		},
		{
			name:     "auto_increment drift",
			src:      &conn.Struct{Cols: []conn.ColMeta{{Column: conn.Column{Name: "id", RawType: "int", Nullable: false, Family: conn.FamINT}, AutoInc: true}}},
			dst:      &conn.Struct{Cols: []conn.ColMeta{metaCol("id", "int", conn.FamINT, false)}},
			wantKind: ChangeModifyColumn,
		},
		{
			name: "drop destination-only column",
			src:  &conn.Struct{Cols: []conn.ColMeta{metaCol("id", "int", conn.FamINT, false)}},
			dst: &conn.Struct{Cols: []conn.ColMeta{
				metaCol("id", "int", conn.FamINT, false),
				metaCol("extra", "int", conn.FamINT, true),
			}},
			wantKind: ChangeDropColumn,
		},
		{
			name: "rename becomes drop plus add",
			src: &conn.Struct{Cols: []conn.ColMeta{
				metaCol("id", "int", conn.FamINT, false),
				metaCol("renamed", "varchar(5)", conn.FamSTR, true),
			}},
			dst: &conn.Struct{Cols: []conn.ColMeta{
				metaCol("id", "int", conn.FamINT, false),
				metaCol("old", "varchar(5)", conn.FamSTR, true),
			}},
			wantKind: ChangeAddColumn,
		},
		{
			name:     "DATETIME/TIMESTAMP swap is not a drift",
			src:      &conn.Struct{Cols: []conn.ColMeta{metaCol("ts", "timestamp", conn.FamTIMESTAMP, true)}},
			dst:      &conn.Struct{Cols: []conn.ColMeta{metaCol("ts", "datetime", conn.FamDATETIME, true)}},
			wantNone: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changes, err := DiffStructure(tc.src, tc.dst, "", "")
			if err != nil {
				t.Fatalf("DiffStructure: %v", err)
			}
			if tc.wantNone {
				if len(changes) != 0 {
					t.Fatalf("expected no changes, got %v", changes)
				}
				return
			}
			ch := findChange(t, changes, tc.wantKind)
			if tc.check != nil {
				tc.check(t, ch)
			}
		})
	}
}

func TestDiffStructureRenameEmitsBoth(t *testing.T) {
	src := &conn.Struct{Cols: []conn.ColMeta{metaCol("renamed", "varchar(5)", conn.FamSTR, true)}}
	dst := &conn.Struct{Cols: []conn.ColMeta{metaCol("old", "varchar(5)", conn.FamSTR, true)}}
	changes, err := DiffStructure(src, dst, "", "")
	if err != nil {
		t.Fatalf("DiffStructure: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("want drop+add, got %d changes: %v", len(changes), changes)
	}
	if findChange(t, changes, ChangeDropColumn).Col.Name != "old" {
		t.Fatalf("wrong drop: %+v", changes)
	}
	if findChange(t, changes, ChangeAddColumn).Col.Name != "renamed" {
		t.Fatalf("wrong add: %+v", changes)
	}
}

func TestDiffStructureIndexes(t *testing.T) {
	cases := []struct {
		name     string
		srcIdx   []conn.Index
		dstIdx   []conn.Index
		wantAdd  bool // want an AddIndex
		wantDrop bool // want a DropIndex
		dropName string
		addPK    bool
	}{
		{name: "pk drift: different columns",
			srcIdx:  []conn.Index{{Name: "PRIMARY", Unique: true, Cols: []string{"id"}}},
			dstIdx:  []conn.Index{{Name: "PRIMARY", Unique: true, Cols: []string{"name"}}},
			wantAdd: true, wantDrop: true, dropName: "PRIMARY", addPK: true},
		{name: "src pk missing on dst",
			srcIdx:  []conn.Index{{Name: "PRIMARY", Unique: true, Cols: []string{"id"}}},
			dstIdx:  nil,
			wantAdd: true, addPK: true},
		{name: "dst-only unique index",
			srcIdx:   nil,
			dstIdx:   []conn.Index{{Name: "u_x", Unique: true, Cols: []string{"x"}}},
			wantDrop: true, dropName: "u_x"},
		{name: "same columns, different names: no change",
			srcIdx: []conn.Index{{Name: "u_a", Unique: true, Cols: []string{"a"}}},
			dstIdx: []conn.Index{{Name: "u_b", Unique: true, Cols: []string{"a"}}}},
		{name: "unique satisfies the source pk by column set",
			srcIdx: []conn.Index{{Name: "PRIMARY", Unique: true, Cols: []string{"id"}}},
			dstIdx: []conn.Index{{Name: "u_id", Unique: true, Cols: []string{"id"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &conn.Struct{Indexes: tc.srcIdx}
			dst := &conn.Struct{Indexes: tc.dstIdx}
			changes, err := DiffStructure(src, dst, "", "")
			if err != nil {
				t.Fatalf("DiffStructure: %v", err)
			}
			add, drop := findChangeSafe(changes, ChangeAddIndex), findChangeSafe(changes, ChangeDropIndex)
			if tc.wantAdd != (add != nil) {
				t.Fatalf("AddIndex: want %v, got %v", tc.wantAdd, add)
			}
			if tc.wantDrop != (drop != nil) {
				t.Fatalf("DropIndex: want %v, got %v", tc.wantDrop, drop)
			}
			if drop != nil && drop.Idx.Name != tc.dropName {
				t.Fatalf("dropped %q, want %q", drop.Idx.Name, tc.dropName)
			}
			if add != nil && (add.Idx.Name == "PRIMARY") != tc.addPK {
				t.Fatalf("added %q, wantPK=%v", add.Idx.Name, tc.addPK)
			}
		})
	}
}

func findChangeSafe(changes []Change, kind ChangeKind) *Change {
	for i := range changes {
		if changes[i].Kind == kind {
			return &changes[i]
		}
	}
	return nil
}

func TestDiffStructureExpressionDefaultRefused(t *testing.T) {
	src := &conn.Struct{Cols: []conn.ColMeta{{
		Column:     conn.Column{Name: "tok", RawType: "varchar(36)", Nullable: true, Family: conn.FamSTR},
		HasDefault: true, Default: "(uuid())",
	}}}
	dst := &conn.Struct{Cols: []conn.ColMeta{metaCol("tok", "varchar(36)", conn.FamSTR, true)}}
	if _, err := DiffStructure(src, dst, "", ""); err == nil {
		t.Fatal("expected the expression default to be refused")
	}
	// An identical expression default on both sides needs no change and
	// is not refused: nothing is re-emitted.
	dst.Cols[0].HasDefault, dst.Cols[0].Default = true, "(uuid())"
	if changes, err := DiffStructure(src, dst, "", ""); err != nil || len(changes) != 0 {
		t.Fatalf("identical expression default: changes=%v err=%v", changes, err)
	}
}

func TestFilterStruct(t *testing.T) {
	s := &conn.Struct{
		Table: "t",
		Cols: []conn.ColMeta{
			metaCol("id", "int", conn.FamINT, false),
			metaCol("local", "int", conn.FamINT, true),
		},
		Indexes: []conn.Index{
			{Name: "PRIMARY", Unique: true, Cols: []string{"id"}},
			{Name: "u_l", Unique: true, Cols: []string{"local"}},
		},
	}
	out := filterStruct(s, map[string]bool{"local": true})
	if len(out.Cols) != 1 || out.Cols[0].Name != "id" {
		t.Fatalf("wrong columns: %v", out.Cols)
	}
	if len(out.Indexes) != 1 || out.Indexes[0].Name != "PRIMARY" {
		t.Fatalf("wrong indexes: %v", out.Indexes)
	}
	if same := filterStruct(s, nil); same != s {
		t.Fatal("empty ignore set must return the same struct")
	}
}

func TestRenderDDLEmpty(t *testing.T) {
	if got := RenderDDL("t", nil); got != nil {
		t.Fatalf("want nil, got %v", got)
	}
}

func TestRenderDDLClauseOrder(t *testing.T) {
	changes := []Change{
		{Kind: ChangeAddIndex, Idx: conn.Index{Name: "u_a", Unique: true, Cols: []string{"a"}}},
		{Kind: ChangeDropColumn, Col: metaCol("gone", "int", conn.FamINT, true)},
		{Kind: ChangeModifyColumn, Col: metaCol("c", "bigint", conn.FamINT, false)},
		{Kind: ChangeDropIndex, Idx: conn.Index{Name: "u_x", Unique: true, Cols: []string{"x"}}},
		{Kind: ChangeAddColumn, Col: metaCol("n", "varchar(10)", conn.FamSTR, true), After: "c"},
		{Kind: ChangeDropIndex, Idx: conn.Index{Name: "PRIMARY", Unique: true, Cols: []string{"p"}}},
	}
	got := RenderDDL("t", changes)
	want := "ALTER TABLE `t` DROP INDEX `u_x`, DROP PRIMARY KEY, DROP COLUMN `gone`, " +
		"MODIFY COLUMN `c` bigint NOT NULL, ADD COLUMN `n` varchar(10) AFTER `c`, ADD UNIQUE (`a`)"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("DDL mismatch:\n got %v\nwant %s", got, want)
	}
}

func TestRenderDDLColumnDef(t *testing.T) {
	cases := []struct {
		name string
		col  conn.ColMeta
		want string
	}{
		{
			name: "full definition (timestamp)",
			col: conn.ColMeta{
				Column:     conn.Column{Name: "ts", RawType: "timestamp", Nullable: false, Family: conn.FamTIMESTAMP},
				HasDefault: true, Default: "CURRENT_TIMESTAMP", OnUpdate: true, Comment: "created\\'s note",
			},
			want: "`ts` timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP " +
				"COMMENT 'created\\\\''s note'",
		},
		{
			// CHARACTER SET / COLLATE attach to the type and must precede
			// NOT NULL (the server rejects the other order).
			name: "full definition (varchar, charset before NOT NULL)",
			col: conn.ColMeta{
				Column:  conn.Column{Name: "s", RawType: "varchar(16)", Nullable: false, Collation: "utf8mb4_0900_ai_ci", Family: conn.FamSTR},
				Charset: "utf8mb4", Comment: "created\\'s note",
			},
			want: "`s` varchar(16) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci NOT NULL " +
				"COMMENT 'created\\\\''s note'",
		},
		{
			name: "auto increment",
			col: conn.ColMeta{
				Column:  conn.Column{Name: "id", RawType: "bigint", Nullable: false, Family: conn.FamINT},
				AutoInc: true,
			},
			want: "`id` bigint NOT NULL AUTO_INCREMENT",
		},
		{
			name: "nullable without default",
			col:  metaCol("n", "int", conn.FamINT, true),
			want: "`n` int",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderColDef(tc.col); got != tc.want {
				t.Fatalf("column def mismatch:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestRenderDDLFirstAndPK(t *testing.T) {
	got := RenderDDL("t", []Change{
		{Kind: ChangeAddColumn, Col: metaCol("new", "int", conn.FamINT, true), First: true},
		{Kind: ChangeAddIndex, Idx: conn.Index{Name: "PRIMARY", Unique: true, Cols: []string{"a", "b"}}},
	})
	if !strings.Contains(got[0], "ADD COLUMN `new` int FIRST") {
		t.Fatalf("FIRST placement missing: %s", got[0])
	}
	if !strings.Contains(got[0], "ADD PRIMARY KEY (`a`, `b`)") {
		t.Fatalf("ADD PRIMARY KEY missing: %s", got[0])
	}
}

// TestRenderDDLSplitSameColumn pins the TiDB split: two operations on one
// column in a single DDL job are rejected there (Error 8200), so the key
// on the re-defined column runs in a follow-up statement.
func TestRenderDDLSplitSameColumn(t *testing.T) {
	got := RenderDDL("t", []Change{
		{Kind: ChangeDropColumn, Col: metaCol("extra", "int", conn.FamINT, true)},
		{Kind: ChangeModifyColumn, Col: metaCol("id", "int", conn.FamINT, false)},
		{Kind: ChangeAddColumn, Col: metaCol("amt", "decimal(10,2)", conn.FamDECIMAL, true), After: "name"},
		{Kind: ChangeAddIndex, Idx: conn.Index{Name: "PRIMARY", Unique: true, Cols: []string{"id"}}},
	})
	if len(got) != 2 {
		t.Fatalf("want 2 statements, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "DROP COLUMN `extra`") ||
		!strings.Contains(got[0], "MODIFY COLUMN `id` int NOT NULL") ||
		!strings.Contains(got[0], "ADD COLUMN `amt` decimal(10,2) AFTER `name`") ||
		strings.Contains(got[0], "PRIMARY") {
		t.Fatalf("statement 1 wrong: %s", got[0])
	}
	if got[1] != "ALTER TABLE `t` ADD PRIMARY KEY (`id`)" {
		t.Fatalf("statement 2 wrong: %s", got[1])
	}
}

// TestRenderDDLSkipImplicitIndexDrop pins the implicit-drop rule: an index
// confined to dropped columns dies with them and must not be dropped
// explicitly (the drop would fail once the column is gone).
func TestRenderDDLSkipImplicitIndexDrop(t *testing.T) {
	got := RenderDDL("t", []Change{
		{Kind: ChangeDropIndex, Idx: conn.Index{Name: "u_x", Unique: true, Cols: []string{"x"}}},
		{Kind: ChangeDropColumn, Col: metaCol("x", "int", conn.FamINT, true)},
	})
	if len(got) != 1 {
		t.Fatalf("want exactly 1 statement, got %v", got)
	}
	if !strings.Contains(got[0], "DROP COLUMN `x`") {
		t.Fatalf("column drop missing: %s", got[0])
	}
	if strings.Contains(got[0], "DROP INDEX") {
		t.Fatalf("implicit index drop must not be emitted: %s", got[0])
	}
}

func TestNormalizeIntType(t *testing.T) {
	cases := map[string]string{
		"int":                 "int",
		"int(11)":             "int",
		"int unsigned":        "int unsigned",
		"int(10) unsigned":    "int unsigned",
		"bigint(20) unsigned": "bigint unsigned",
		"tinyint(1)":          "tinyint",
		"mediumint(9)":        "mediumint",
		"decimal(10,2)":       "decimal(10,2)",
		"varchar(32)":         "varchar(32)",
		"char(1)":             "char(1)",
		"enum('a','b')":       "enum('a','b')",
		"INT(11)":             "int",
	}
	for in, want := range cases {
		if got := normalizeIntType(in); got != want {
			t.Errorf("normalizeIntType(%q) = %q, want %q", in, got, want)
		}
	}
}

// strCol builds a ColMeta with charset/collation for the default-collation
// tests.
func strCol(name, collation string) conn.ColMeta {
	return conn.ColMeta{
		Column:  conn.Column{Name: name, RawType: "varchar(10)", Nullable: true, Collation: collation, Family: conn.FamSTR},
		Charset: "utf8mb4",
	}
}

// TestDiffStructureCrossBackend pins the two cross-backend normalizations
// found on the MySQL 8.0 -> TiDB compatibility run: integer display
// widths, and collations that are each side's own database default.
func TestDiffStructureCrossBackend(t *testing.T) {
	t.Run("display width is not a drift", func(t *testing.T) {
		src := &conn.Struct{Cols: []conn.ColMeta{metaCol("id", "int", conn.FamINT, false)}}
		dst := &conn.Struct{Cols: []conn.ColMeta{metaCol("id", "int(11)", conn.FamINT, false)}}
		changes, err := DiffStructure(src, dst, "", "")
		if err != nil || len(changes) != 0 {
			t.Fatalf("changes=%v err=%v", changes, err)
		}
	})
	t.Run("display width with unsigned", func(t *testing.T) {
		src := &conn.Struct{Cols: []conn.ColMeta{metaCol("id", "int unsigned", conn.FamINT, false)}}
		dst := &conn.Struct{Cols: []conn.ColMeta{metaCol("id", "int(10) unsigned", conn.FamINT, false)}}
		if changes, _ := DiffStructure(src, dst, "", ""); len(changes) != 0 {
			t.Fatalf("changes=%v", changes)
		}
	})
	t.Run("sign drift is still a drift", func(t *testing.T) {
		src := &conn.Struct{Cols: []conn.ColMeta{metaCol("id", "int", conn.FamINT, false)}}
		dst := &conn.Struct{Cols: []conn.ColMeta{metaCol("id", "int(11) unsigned", conn.FamINT, false)}}
		if changes, _ := DiffStructure(src, dst, "", ""); len(changes) != 1 {
			t.Fatalf("want 1 change, got %v", changes)
		}
	})
	// MySQL 8.0's default (utf8mb4_0900_ai_ci) vs TiDB's (utf8mb4_bin):
	// both sides left the collation to their own backend's default.
	t.Run("both sides on their own default collation", func(t *testing.T) {
		src := &conn.Struct{Cols: []conn.ColMeta{strCol("n", "utf8mb4_0900_ai_ci")}}
		dst := &conn.Struct{Cols: []conn.ColMeta{strCol("n", "utf8mb4_bin")}}
		changes, err := DiffStructure(src, dst, "utf8mb4_0900_ai_ci", "utf8mb4_bin")
		if err != nil || len(changes) != 0 {
			t.Fatalf("changes=%v err=%v", changes, err)
		}
	})
	t.Run("explicit collation still drifts", func(t *testing.T) {
		src := &conn.Struct{Cols: []conn.ColMeta{strCol("n", "utf8mb4_0900_ai_ci")}}
		dst := &conn.Struct{Cols: []conn.ColMeta{strCol("n", "utf8mb4_bin")}}
		// src's value is not its own default -> the src side chose it.
		changes, err := DiffStructure(src, dst, "utf8mb4_bin", "utf8mb4_bin")
		if err != nil || len(changes) != 1 {
			t.Fatalf("want 1 change, got %v err=%v", changes, err)
		}
	})
	t.Run("unknown defaults fall back to strict", func(t *testing.T) {
		src := &conn.Struct{Cols: []conn.ColMeta{strCol("n", "utf8mb4_0900_ai_ci")}}
		dst := &conn.Struct{Cols: []conn.ColMeta{strCol("n", "utf8mb4_bin")}}
		if changes, _ := DiffStructure(src, dst, "", ""); len(changes) != 1 {
			t.Fatalf("want 1 change, got %v", changes)
		}
	})
}

func createTableFixture() *conn.Struct {
	return &conn.Struct{
		Table: "t",
		Cols: []conn.ColMeta{
			{Column: conn.Column{Name: "id", RawType: "int", Nullable: false, Family: conn.FamINT}, AutoInc: true},
			{Column: conn.Column{Name: "code", RawType: "varchar(16)", Nullable: false, Family: conn.FamSTR},
				HasDefault: true, Default: "NULL"},
		},
		Indexes: []conn.Index{
			{Name: "PRIMARY", Unique: true, Cols: []string{"id"}},
			{Name: "u_code", Unique: true, Cols: []string{"code"}},
		},
	}
}

// TestRenderCreateTable covers the CREATE TABLE for a missing destination
// table: columns in source order, primary key, unique keys, the optional
// engine and the AUTO_INCREMENT starting value (omitted when the source
// has no auto-increment column — a bare value would be meaningless).
func TestRenderCreateTable(t *testing.T) {
	want := "CREATE TABLE `t` (\n" +
		"  `id` int NOT NULL AUTO_INCREMENT,\n" +
		"  `code` varchar(16) NOT NULL DEFAULT NULL,\n" +
		"  PRIMARY KEY (`id`),\n" +
		"  UNIQUE KEY `u_code` (`code`)\n" +
		") ENGINE=InnoDB AUTO_INCREMENT=42;"
	if got := RenderCreateTable("t", createTableFixture(), "InnoDB", 42, true); got != want {
		t.Errorf("rendered:\n%s\nwant:\n%s", got, want)
	}
	// no auto-increment column on the source: no AUTO_INCREMENT option
	got := RenderCreateTable("t", createTableFixture(), "InnoDB", 0, false)
	if strings.Contains(got, "AUTO_INCREMENT=42") {
		t.Errorf("no auto-increment column must not emit a value:\n%s", got)
	}
	if !strings.HasSuffix(got, ") ENGINE=InnoDB;") {
		t.Errorf("missing engine suffix:\n%s", got)
	}
	// unknown engine: no ENGINE clause at all
	if got := RenderCreateTable("t", createTableFixture(), "", 42, true); strings.Contains(got, "ENGINE=") {
		t.Errorf("empty engine must not be emitted:\n%s", got)
	}
}

// TestUsableKeyOf pins the key rule the post-repair re-plan relies on:
// the primary key first, then the first unique index whose columns are all
// NOT NULL (a unique index on a nullable column cannot address rows).
func TestUsableKeyOf(t *testing.T) {
	// primary key wins over a unique index
	s := &conn.Struct{
		Cols: []conn.ColMeta{metaCol("id", "int", conn.FamINT, false), metaCol("a", "int", conn.FamINT, false)},
		Indexes: []conn.Index{
			{Name: "u_a", Unique: true, Cols: []string{"a"}},
			{Name: "PRIMARY", Unique: true, Cols: []string{"id"}},
		},
	}
	if got := UsableKeyOf(s); len(got) != 1 || got[0] != "id" {
		t.Errorf("primary key must win, got %v", got)
	}
	// a NOT NULL unique index is usable
	s = &conn.Struct{
		Cols:    []conn.ColMeta{metaCol("a", "int", conn.FamINT, false)},
		Indexes: []conn.Index{{Name: "u_a", Unique: true, Cols: []string{"a"}}},
	}
	if got := UsableKeyOf(s); len(got) != 1 || got[0] != "a" {
		t.Errorf("not-null unique index, got %v", got)
	}
	// a unique index on a nullable column is not usable
	s = &conn.Struct{
		Cols:    []conn.ColMeta{metaCol("a", "int", conn.FamINT, true)},
		Indexes: []conn.Index{{Name: "u_a", Unique: true, Cols: []string{"a"}}},
	}
	if got := UsableKeyOf(s); got != nil {
		t.Errorf("nullable unique index must not be usable, got %v", got)
	}
	// a composite unique with one nullable column is not usable either
	s = &conn.Struct{
		Cols: []conn.ColMeta{metaCol("a", "int", conn.FamINT, false), metaCol("b", "int", conn.FamINT, true)},
		Indexes: []conn.Index{
			{Name: "u_ab", Unique: true, Cols: []string{"a", "b"}},
			{Name: "u_c", Unique: true, Cols: []string{"a"}},
		},
	}
	if got := UsableKeyOf(s); len(got) != 1 || got[0] != "a" {
		t.Errorf("first usable unique expected [a], got %v", got)
	}
	// no indexes at all
	s = &conn.Struct{Cols: []conn.ColMeta{metaCol("a", "int", conn.FamINT, false)}}
	if got := UsableKeyOf(s); got != nil {
		t.Errorf("keyless table, got %v", got)
	}
}

// TestDestructiveDDL covers the classifier the confirmation summary and
// the report use to surface the irreversible statements separately.
func TestDestructiveDDL(t *testing.T) {
	cases := []struct {
		stmt string
		want bool
	}{
		{"DROP TABLE `x`", true},
		{"drop table x", true}, // case-insensitive
		{"ALTER TABLE `t` DROP COLUMN `c`", true},
		{"ALTER TABLE `t` DROP PRIMARY KEY", true},
		{"ALTER TABLE `t` DROP INDEX `i`", true},
		{"ALTER TABLE `t` ADD COLUMN `c` int", false},
		{"ALTER TABLE `t` MODIFY COLUMN `c` bigint", false},
		{"ALTER TABLE `t` ADD UNIQUE (`a`)", false},
		{"ALTER TABLE `t` AUTO_INCREMENT = 42", false},
		{"CREATE TABLE `t` (id int)", false},
		{"TRUNCATE TABLE `t`", false},
	}
	for _, c := range cases {
		if got := DestructiveDDL(c.stmt); got != c.want {
			t.Errorf("DestructiveDDL(%q) = %v, want %v", c.stmt, got, c.want)
		}
	}
}
