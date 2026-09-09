-- Shared seed: loaded on BOTH sides. Identical file, identical data.
SET time_zone = '+00:00';
-- Recursive CTEs below generate up to 100k rows; the default
-- cte_max_recursion_depth is 1000, so raise it for this session.
SET SESSION cte_max_recursion_depth = 1000000;

-- t_large: 100k rows, integer PK, mixed types (consistency + determinism tests)
DROP TABLE IF EXISTS t_large;
CREATE TABLE t_large (
  id    BIGINT PRIMARY KEY,
  v1    DECIMAL(10,2),
  v2    DOUBLE,
  v3    VARCHAR(64),
  v4    DATETIME(3)
) ENGINE=InnoDB;

INSERT INTO t_large (id, v1, v2, v3, v4)
WITH RECURSIVE seq(n) AS (
  SELECT 1
  UNION ALL
  SELECT n + 1 FROM seq WHERE n < 100000
)
SELECT
  n,
  ROUND(1.5 * n, 2),
  n * 0.123,
  CONCAT('row-', n, '-', IF(n % 7 = 0, 'pad', 'x')),
  DATE_ADD('2024-01-01 00:00:00.000', INTERVAL n SECOND)
FROM seq;

-- t_chunk: 90001 rows, id 1..90001. The span (90000) is divisible by the
-- chunk count at --chunk-size 10000 (n=10): the regression shape for the
-- intBoundaries off-by-one, where the max id row was in no chunk on either
-- side and a change to it was silently missed.
DROP TABLE IF EXISTS t_chunk;
CREATE TABLE t_chunk (
  id  BIGINT PRIMARY KEY,
  val VARCHAR(32)
) ENGINE=InnoDB;

INSERT INTO t_chunk (id, val)
WITH RECURSIVE seq(n) AS (
  SELECT 1
  UNION ALL
  SELECT n + 1 FROM seq WHERE n < 90001
)
SELECT n, CONCAT('c', n) FROM seq;

-- t_chunkc: 30001 rows, composite PK (a, b), a = 1..30001. At --chunk-size
-- 10000 the chunk count is 4 and the span (30000) is divisible by it: the
-- regression shape for arithmetic split of a composite key on its integer
-- lead column (P3-#15) — the max lead row must land in the last chunk.
DROP TABLE IF EXISTS t_chunkc;
CREATE TABLE t_chunkc (
  a   BIGINT NOT NULL,
  b   BIGINT NOT NULL,
  val VARCHAR(32),
  PRIMARY KEY (a, b)
) ENGINE=InnoDB;

INSERT INTO t_chunkc (a, b, val)
WITH RECURSIVE seq(n) AS (
  SELECT 1
  UNION ALL
  SELECT n + 1 FROM seq WHERE n < 30001
)
SELECT n, n * 2, CONCAT('cc', n) FROM seq;

-- t_nullkey: no PK, unique-but-NULLABLE key column a (auto key selection
-- must reject it and fall back to keyless multiset semantics), plus rows
-- with NULL key values that chunk predicates must not drop.
DROP TABLE IF EXISTS t_nullkey;
CREATE TABLE t_nullkey (
  a INT UNIQUE,
  b VARCHAR(20)
) ENGINE=InnoDB;

INSERT INTO t_nullkey VALUES (NULL, 'x'), (1, 'y'), (2, 'z');

-- t_fracsec: DATETIME(3) values that collide under the old trailing-zero
-- strip of the microsecond count (100ms vs 10ms both rendered ".1").
DROP TABLE IF EXISTS t_fracsec;
CREATE TABLE t_fracsec (
  id BIGINT PRIMARY KEY,
  d  DATETIME(3)
) ENGINE=InnoDB;

INSERT INTO t_fracsec VALUES (1, '2024-01-01 00:00:00.100');

-- t_nopk: no key, 20k rows, many duplicate values (multiset semantics)
DROP TABLE IF EXISTS t_nopk;
CREATE TABLE t_nopk (
  v DECIMAL(10,2),
  w VARCHAR(50)
) ENGINE=InnoDB;

INSERT INTO t_nopk (v, w)
WITH RECURSIVE seq(n) AS (
  SELECT 1
  UNION ALL
  SELECT n + 1 FROM seq WHERE n < 20000
)
SELECT MOD(n, 1000) * 0.5, CONCAT('dup-', MOD(n, 50)) FROM seq;

-- t_mut: 1000 rows, mutated per-scenario on the dst side only
DROP TABLE IF EXISTS t_mut;
CREATE TABLE t_mut (
  id INT PRIMARY KEY,
  val VARCHAR(32),
  updated_at TIMESTAMP
) ENGINE=InnoDB;

INSERT INTO t_mut (id, val, updated_at)
WITH RECURSIVE seq(n) AS (
  SELECT 1
  UNION ALL
  SELECT n + 1 FROM seq WHERE n < 1000
)
SELECT n, CONCAT('v', n), '2024-01-01 00:00:00' FROM seq;

-- t_struct: identical on both sides; the structure-sync scenarios drift the
-- dst copy (m_struct_drift.sql) and expect the sync to realign it.
DROP TABLE IF EXISTS t_struct;
CREATE TABLE t_struct (
  id   INT PRIMARY KEY,
  name VARCHAR(32),
  amt  DECIMAL(10,2),
  ts   TIMESTAMP
) ENGINE=InnoDB;

INSERT INTO t_struct (id, name, amt, ts)
WITH RECURSIVE seq(n) AS (
  SELECT 1
  UNION ALL
  SELECT n + 1 FROM seq WHERE n < 100
)
SELECT n, CONCAT('s', n), n * 1.5, '2024-01-01 00:00:00' FROM seq;

-- t_oor: 100 rows, INT PK; the out-of-range sync scenarios mutate the dst
-- copy (m_oor_oor.sql et al.).
DROP TABLE IF EXISTS t_oor;
CREATE TABLE t_oor (
  id  INT PRIMARY KEY,
  val VARCHAR(32)
) ENGINE=InnoDB;

INSERT INTO t_oor (id, val)
WITH RECURSIVE seq(n) AS (
  SELECT 1
  UNION ALL
  SELECT n + 1 FROM seq WHERE n < 100
)
SELECT n, CONCAT('o', n) FROM seq;

-- t_oorc: composite PK (a, b), a = 1..10 x b = 1..3 (30 rows).
DROP TABLE IF EXISTS t_oorc;
CREATE TABLE t_oorc (
  a   INT NOT NULL,
  b   INT NOT NULL,
  val VARCHAR(32),
  PRIMARY KEY (a, b)
) ENGINE=InnoDB;

INSERT INTO t_oorc (a, b, val)
WITH RECURSIVE seq(n) AS (
  SELECT 1
  UNION ALL
  SELECT n + 1 FROM seq WHERE n < 30
)
SELECT FLOOR((n - 1) / 3) + 1, MOD(n - 1, 3) + 1, CONCAT('c', n) FROM seq;

-- t_oors: VARCHAR PK k001..k050 (exercises the quoted-string literal path).
DROP TABLE IF EXISTS t_oors;
CREATE TABLE t_oors (
  k   VARCHAR(16) PRIMARY KEY,
  val VARCHAR(32)
) ENGINE=InnoDB;

INSERT INTO t_oors (k, val)
WITH RECURSIVE seq(n) AS (
  SELECT 1
  UNION ALL
  SELECT n + 1 FROM seq WHERE n < 50
)
SELECT CONCAT('k', LPAD(n, 3, '0')), CONCAT('s', n) FROM seq;

-- t_oorn: no PK, nullable key column a plus duplicate NULL-key rows; the
-- scenarios address it with an explicit --key a (auto key selection
-- rejects a nullable column).
DROP TABLE IF EXISTS t_oorn;
CREATE TABLE t_oorn (
  a INT,
  v VARCHAR(20)
) ENGINE=InnoDB;

INSERT INTO t_oorn VALUES (NULL, 'n1'), (NULL, 'n2'), (1, 'x'), (2, 'y'), (3, 'z');

-- t_ignore: updated_at is identical at seed time; a later dst mutation
-- shifts it, and --ignore-columns must hide the difference.
DROP TABLE IF EXISTS t_ignore;
CREATE TABLE t_ignore (
  id INT PRIMARY KEY,
  v INT,
  updated_at TIMESTAMP
) ENGINE=InnoDB;

INSERT INTO t_ignore (id, v, updated_at)
WITH RECURSIVE seq(n) AS (
  SELECT 1
  UNION ALL
  SELECT n + 1 FROM seq WHERE n < 500
)
SELECT n, n, '2024-01-01 00:00:00' FROM seq;

-- t_keyless: identical on both sides; the key-drift scenarios
-- (m_keyless_drift.sql) remove the dst's PRIMARY KEY only — the columns
-- and the data stay identical to the src.
DROP TABLE IF EXISTS t_keyless;
CREATE TABLE t_keyless (
  id  INT PRIMARY KEY,
  val VARCHAR(32)
) ENGINE=InnoDB;

INSERT INTO t_keyless (id, val)
WITH RECURSIVE seq(n) AS (
  SELECT 1
  UNION ALL
  SELECT n + 1 FROM seq WHERE n < 50
)
SELECT n, CONCAT('v', n) FROM seq;

-- ---- data-safety regression tables (identical on both sides) ----

-- t_swap: NOT NULL UNIQUE v. The swap scenarios (m_swap.sql) exchange
-- the two rows' values: converging needs an intermediate state that
-- would violate the unique index, so the sync must convert the pair to
-- delete+insert instead of updating it in place. (v must be NOT NULL:
-- a unique index on a nullable column is deliberately NOT treated as
-- unique, same rule as the t_nullkey auto-key rejection.)
DROP TABLE IF EXISTS t_swap;
CREATE TABLE t_swap (
  id INT PRIMARY KEY,
  v  VARCHAR(16) NOT NULL UNIQUE
) ENGINE=InnoDB;
INSERT INTO t_swap VALUES (1, 'B'), (2, 'A');

-- t_bigint: BIGINT keys at the extreme ends (MinInt64, MaxInt64, ...):
-- the span is wider than MaxInt64 values, so the arithmetic chunking
-- must refuse and the sampler must split the range instead.
DROP TABLE IF EXISTS t_bigint;
CREATE TABLE t_bigint (
  id  BIGINT PRIMARY KEY,
  val VARCHAR(16)
) ENGINE=InnoDB;
-- Plain literals, no arithmetic: MySQL constant-folds expressions like
-- -9223372036854775807 - 2 and rejects them as out of range; the literal
-- -9223372036854775808 itself is accepted (the sign applies to the
-- unsigned literal).
INSERT INTO t_bigint VALUES
  (-9223372036854775808, 'min'),
  (-9223372036854775807, 'min2'),
  (1, 'mid'),
  (9223372036854775806, 'max2'),
  (9223372036854775807, 'max');

-- t_gen: val plus a STORED generated column. The generated column is
-- compared but NEVER written, and a drifted generated column is refused
-- by the structure sync (the expression cannot be reproduced).
DROP TABLE IF EXISTS t_gen;
CREATE TABLE t_gen (
  id      INT PRIMARY KEY,
  val     INT,
  doubled INT GENERATED ALWAYS AS (val * 2) STORED
) ENGINE=InnoDB;
INSERT INTO t_gen (id, val)
WITH RECURSIVE seq(n) AS (
  SELECT 1
  UNION ALL
  SELECT n + 1 FROM seq WHERE n < 10
)
SELECT n, n * 10 FROM seq;

-- t_structfail: DECIMAL(4,2) on both sides. The drift script widens the
-- dst column and stores a value that does not fit the src type, so the
-- in-place structure ALTER must fail and the existing data must survive.
DROP TABLE IF EXISTS t_structfail;
CREATE TABLE t_structfail (
  id  INT PRIMARY KEY,
  amt DECIMAL(4,2)
) ENGINE=InnoDB;
INSERT INTO t_structfail
WITH RECURSIVE seq(n) AS (
  SELECT 1
  UNION ALL
  SELECT n + 1 FROM seq WHERE n < 10
)
SELECT n, n * 0.5 FROM seq;

-- t_snap: the snapshot/concurrency scenarios churn it from a background
-- client while mtdiff reads it with --snapshot.
DROP TABLE IF EXISTS t_snap;
CREATE TABLE t_snap (
  id INT PRIMARY KEY,
  v  VARCHAR(16)
) ENGINE=InnoDB;
INSERT INTO t_snap (id, v)
WITH RECURSIVE seq(n) AS (
  SELECT 1
  UNION ALL
  SELECT n + 1 FROM seq WHERE n < 100
)
SELECT n, CONCAT('s', n) FROM seq;

-- t_wk: a NON-UNIQUE index on k (auto key selection ignores it; an
-- explicit --key k with --where must be refused, since a filtered row
-- sync would address whole key groups).
DROP TABLE IF EXISTS t_wk;
CREATE TABLE t_wk (
  id INT PRIMARY KEY,
  k  INT,
  KEY idx_k (k)
) ENGINE=InnoDB;
INSERT INTO t_wk
WITH RECURSIVE seq(n) AS (
  SELECT 1
  UNION ALL
  SELECT n + 1 FROM seq WHERE n < 20
)
SELECT n, MOD(n, 5) FROM seq;

-- t_u3: a 3-row cycle on a NOT NULL unique column: src (1,'A')(2,'B')
-- (3,'C'); the cycle drift (dst 1->B, 2->C, 3->A) is a unique-value
-- cycle — no row-level order applies it, the destructive rewrite is
-- refused by default (P0-2).
DROP TABLE IF EXISTS t_u3;
CREATE TABLE t_u3 (
  id INT PRIMARY KEY,
  u  VARCHAR(16) NOT NULL UNIQUE
) ENGINE=InnoDB;
INSERT INTO t_u3 VALUES (1, 'A'), (2, 'B'), (3, 'C');

-- t_fk / t_fkc: a child with FK ON DELETE CASCADE. A destructive row
-- rewrite of the parent's swapped rows cascades the child deletes (the
-- data loss the default refusal exists to prevent — P0-2).
DROP TABLE IF EXISTS t_fkc;
DROP TABLE IF EXISTS t_fk;
CREATE TABLE t_fk (
  id   INT PRIMARY KEY,
  code VARCHAR(16) NOT NULL UNIQUE
) ENGINE=InnoDB;
CREATE TABLE t_fkc (
  id  INT PRIMARY KEY,
  pid INT NOT NULL,
  CONSTRAINT fk_fkc_pid FOREIGN KEY (pid) REFERENCES t_fk (id) ON DELETE CASCADE
) ENGINE=InnoDB;
INSERT INTO t_fk VALUES (1, 'A'), (2, 'B');
INSERT INTO t_fkc VALUES (10, 1), (11, 2);

-- t_comp: a COMPOSITE UNIQUE(a,b): a value of a repeating across rows
-- (with a different b) is not a conflict (P1-5); a whole-tuple swap is.
DROP TABLE IF EXISTS t_comp;
CREATE TABLE t_comp (
  id INT PRIMARY KEY,
  a  VARCHAR(16),
  b  VARCHAR(16),
  UNIQUE KEY uk_ab (a, b)
) ENGINE=InnoDB;
INSERT INTO t_comp VALUES (1, 'a', 'b'), (2, 'b', 'a'), (3, 'x', 'p'), (4, 'x', 'q');

-- t_two: TWO separate unique constraints. An email value equal to
-- another row's phone value must not cross-collide (P1-5).
DROP TABLE IF EXISTS t_two;
CREATE TABLE t_two (
  id    INT PRIMARY KEY,
  email VARCHAR(32) NOT NULL UNIQUE,
  phone VARCHAR(16) NOT NULL UNIQUE
) ENGINE=InnoDB;
INSERT INTO t_two VALUES (1, 'x@e', '911'), (2, '911', 'x@e'), (3, 'p@e', '222');

-- t_nu: a NULLABLE unique column: MySQL allows repeated NULLs, so NULL
-- tuples never occupy a slot and must not false-conflict (P1-5).
DROP TABLE IF EXISTS t_nu;
CREATE TABLE t_nu (
  id INT PRIMARY KEY,
  u  VARCHAR(16) UNIQUE
) ENGINE=InnoDB;
INSERT INTO t_nu VALUES (1, NULL), (2, NULL), (3, 'z');

-- t_xchunk: 12 rows, chunk size 10 → two chunks. The drift swaps the
-- unique values of row 1 (chunk 1) and row 12 (chunk 2): a cross-chunk
-- swap that no per-chunk commit order can apply (P1-6).
DROP TABLE IF EXISTS t_xchunk;
CREATE TABLE t_xchunk (
  id INT PRIMARY KEY,
  u  VARCHAR(16) NOT NULL UNIQUE
) ENGINE=InnoDB;
INSERT INTO t_xchunk
WITH RECURSIVE seq(n) AS (
  SELECT 1
  UNION ALL
  SELECT n + 1 FROM seq WHERE n < 12
)
SELECT n, CONCAT('v', n) FROM seq;

-- t_mddl: the structure repair needs TWO statements (add x, then the
-- unique index on (code, x) — which references the new column, so it
-- runs as a follow-up statement): statement 2 fails on the destination
-- duplicate codes (P1-2).
DROP TABLE IF EXISTS t_mddl;
CREATE TABLE t_mddl (
  id   INT PRIMARY KEY,
  code VARCHAR(16),
  x    INT NOT NULL DEFAULT 0,
  UNIQUE KEY uk_cx (code, x)
) ENGINE=InnoDB;
INSERT INTO t_mddl (id, code) VALUES (1, 'A'), (2, 'B'), (3, 'C');

-- t_genx: a generated column whose EXPRESSION is part of the structure
-- comparison (P1-1): an identical expression is not a drift, a
-- different one is (and refused).
DROP TABLE IF EXISTS t_genx;
CREATE TABLE t_genx (
  id  INT PRIMARY KEY,
  val INT,
  dbl INT GENERATED ALWAYS AS (val * 2) STORED
) ENGINE=InnoDB;
INSERT INTO t_genx (id, val) VALUES (1, 10), (2, 20);

-- t_genv: a VIRTUAL generated column; the drift re-defines the SAME
-- expression as STORED (a storage-type drift, P1-1).
DROP TABLE IF EXISTS t_genv;
CREATE TABLE t_genv (
  id  INT PRIMARY KEY,
  val INT,
  dbl INT GENERATED ALWAYS AS (val + 1) VIRTUAL
) ENGINE=InnoDB;
INSERT INTO t_genv (id, val) VALUES (1, 10), (2, 20);

-- t_wide: 120 columns. One row binds 120 parameters, so the INSERT
-- batch must shrink below the 60000 bind-parameter budget (P2-3).
DROP TABLE IF EXISTS t_wide;
CREATE TABLE t_wide (
  id INT PRIMARY KEY,
  c01 INT, c02 INT, c03 INT, c04 INT, c05 INT, c06 INT, c07 INT, c08 INT, c09 INT, c10 INT,
  c11 INT, c12 INT, c13 INT, c14 INT, c15 INT, c16 INT, c17 INT, c18 INT, c19 INT, c20 INT,
  c21 INT, c22 INT, c23 INT, c24 INT, c25 INT, c26 INT, c27 INT, c28 INT, c29 INT, c30 INT,
  c31 INT, c32 INT, c33 INT, c34 INT, c35 INT, c36 INT, c37 INT, c38 INT, c39 INT, c40 INT,
  c41 INT, c42 INT, c43 INT, c44 INT, c45 INT, c46 INT, c47 INT, c48 INT, c49 INT, c50 INT,
  c51 INT, c52 INT, c53 INT, c54 INT, c55 INT, c56 INT, c57 INT, c58 INT, c59 INT, c60 INT,
  c61 INT, c62 INT, c63 INT, c64 INT, c65 INT, c66 INT, c67 INT, c68 INT, c69 INT, c70 INT,
  c71 INT, c72 INT, c73 INT, c74 INT, c75 INT, c76 INT, c77 INT, c78 INT, c79 INT, c80 INT,
  c81 INT, c82 INT, c83 INT, c84 INT, c85 INT, c86 INT, c87 INT, c88 INT, c89 INT, c90 INT,
  c91 INT, c92 INT, c93 INT, c94 INT, c95 INT, c96 INT, c97 INT, c98 INT, c99 INT, c100 INT,
  c101 INT, c102 INT, c103 INT, c104 INT, c105 INT, c106 INT, c107 INT, c108 INT, c109 INT, c110 INT,
  c111 INT, c112 INT, c113 INT, c114 INT, c115 INT, c116 INT, c117 INT, c118 INT, c119 INT, c120 INT
) ENGINE=InnoDB;
INSERT INTO t_wide (id, c01, c02, c03, c04, c05, c06, c07, c08, c09, c10, c11, c12, c13, c14, c15, c16, c17, c18, c19, c20,
  c21, c22, c23, c24, c25, c26, c27, c28, c29, c30, c31, c32, c33, c34, c35, c36, c37, c38, c39, c40, c41, c42, c43, c44, c45, c46, c47, c48, c49, c50,
  c51, c52, c53, c54, c55, c56, c57, c58, c59, c60, c61, c62, c63, c64, c65, c66, c67, c68, c69, c70, c71, c72, c73, c74, c75, c76, c77, c78, c79, c80,
  c81, c82, c83, c84, c85, c86, c87, c88, c89, c90, c91, c92, c93, c94, c95, c96, c97, c98, c99, c100, c101, c102, c103, c104, c105, c106, c107, c108, c109, c110,
  c111, c112, c113, c114, c115, c116, c117, c118, c119, c120)
VALUES
  (1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40, 41, 42, 43, 44, 45, 46, 47, 48, 49, 50, 51, 52, 53, 54, 55, 56, 57, 58, 59, 60, 61, 62, 63, 64, 65, 66, 67, 68, 69, 70, 71, 72, 73, 74, 75, 76, 77, 78, 79, 80, 81, 82, 83, 84, 85, 86, 87, 88, 89, 90, 91, 92, 93, 94, 95, 96, 97, 98, 99, 100, 101, 102, 103, 104, 105, 106, 107, 108, 109, 110, 111, 112, 113, 114, 115, 116, 117, 118, 119, 120, 121),
  (2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40, 41, 42, 43, 44, 45, 46, 47, 48, 49, 50, 51, 52, 53, 54, 55, 56, 57, 58, 59, 60, 61, 62, 63, 64, 65, 66, 67, 68, 69, 70, 71, 72, 73, 74, 75, 76, 77, 78, 79, 80, 81, 82, 83, 84, 85, 86, 87, 88, 89, 90, 91, 92, 93, 94, 95, 96, 97, 98, 99, 100, 101, 102, 103, 104, 105, 106, 107, 108, 109, 110, 111, 112, 113, 114, 115, 116, 117, 118, 119, 120, 121, 122),
  (3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40, 41, 42, 43, 44, 45, 46, 47, 48, 49, 50, 51, 52, 53, 54, 55, 56, 57, 58, 59, 60, 61, 62, 63, 64, 65, 66, 67, 68, 69, 70, 71, 72, 73, 74, 75, 76, 77, 78, 79, 80, 81, 82, 83, 84, 85, 86, 87, 88, 89, 90, 91, 92, 93, 94, 95, 96, 97, 98, 99, 100, 101, 102, 103, 104, 105, 106, 107, 108, 109, 110, 111, 112, 113, 114, 115, 116, 117, 118, 119, 120, 121, 122, 123);

-- t_wheresparse: 10000 string-keyed rows of which exactly 100 match
-- the filter (n % 100 = 0). The key is a VARCHAR so the planner uses
-- the SAMPLED split points, which must come from the FILTERED rows
-- (P2-2).
DROP TABLE IF EXISTS t_wheresparse;
CREATE TABLE t_wheresparse (
  k VARCHAR(16) PRIMARY KEY,
  g INT,
  v VARCHAR(16)
) ENGINE=InnoDB;
INSERT INTO t_wheresparse
WITH RECURSIVE seq(n) AS (
  SELECT 1
  UNION ALL
  SELECT n + 1 FROM seq WHERE n < 10000
)
SELECT CONCAT('k', LPAD(n, 5, '0')), MOD(n, 100), CONCAT('w', n) FROM seq;

-- t_esc: strings with backslashes and quotes. Seeded with
-- NO_BACKSLASH_ESCAPES so the literals below stay literal (the file ends
-- here, so the session-mode change affects no later statement). The sync
-- write path must round-trip these values byte-exact under any sql_mode.
SET SESSION sql_mode = CONCAT(@@SESSION.sql_mode, ',NO_BACKSLASH_ESCAPES');
DROP TABLE IF EXISTS t_esc;
CREATE TABLE t_esc (
  id  INT PRIMARY KEY,
  val VARCHAR(128)
) ENGINE=InnoDB;
INSERT INTO t_esc VALUES
  (1, 'C:\abc\def'),
  (2, 'a\b'),
  (3, ''''),
  (4, 'a''b'),
  (5, '{"path":"C:\\abc"}'),
  (6, '中文\测试');
