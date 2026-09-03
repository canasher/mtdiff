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
