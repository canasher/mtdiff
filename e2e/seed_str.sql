-- t_strkey: a VARCHAR primary key holding backslashes, quotes and CJK.
-- Seeded with NO_BACKSLASH_ESCAPES so the literals stay literal
-- (loaded on BOTH sides; the sync's parameterized read predicates must
-- address these keys byte-exact under any server sql_mode, P0-1).
SET SESSION sql_mode = CONCAT(@@SESSION.sql_mode, ',NO_BACKSLASH_ESCAPES');
DROP TABLE IF EXISTS t_strkey;
CREATE TABLE t_strkey (
  k VARCHAR(128) PRIMARY KEY,
  v VARCHAR(128)
) ENGINE=InnoDB;
INSERT INTO t_strkey VALUES
  ('a\b', 'v01'),
  ('a\\b', 'v02'),
  ('a''b', 'v03'),
  ('中文\测试', 'v04'),
  ('C:\abc\def', 'v05'),
  ('z\\末', 'v06'),
  ('p\q', 'v07'),
  ('x''y', 'v08'),
  ('m\n', 'v09'),
  ('o''p', 'v10'),
  ('尾\部', 'v11'),
  ('末\z', 'v12');
