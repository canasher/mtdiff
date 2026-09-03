-- Whole-database sync seeds (src side, database srcdb2). The matching dst
-- database (dstdb2) starts EMPTY: in whole-database mode (no --tables) the
-- sync must discover these tables on the source and create every one of
-- them on the destination.
SET time_zone = '+00:00';
SET SESSION cte_max_recursion_depth = 1000000;

-- t_ai: auto-increment PK; the counter is pushed far above the data
-- (AUTO_INCREMENT = 1500) so the table STATE is a real thing to converge.
DROP TABLE IF EXISTS t_ai;
CREATE TABLE t_ai (
  id  INT PRIMARY KEY AUTO_INCREMENT,
  val VARCHAR(32)
) ENGINE=InnoDB;

INSERT INTO t_ai (id, val)
WITH RECURSIVE seq(n) AS (
  SELECT 1
  UNION ALL
  SELECT n + 1 FROM seq WHERE n < 100
)
SELECT n, CONCAT('a', n) FROM seq;

ALTER TABLE t_ai AUTO_INCREMENT = 1500;

-- t_plain: plain integer PK, no auto-increment.
DROP TABLE IF EXISTS t_plain;
CREATE TABLE t_plain (
  id  INT PRIMARY KEY,
  val VARCHAR(32)
) ENGINE=InnoDB;

INSERT INTO t_plain (id, val) VALUES (1, 'p1'), (2, 'p2'), (3, 'p3');

-- t_idx: carries a plain (non-unique) index. Plain indexes are outside the
-- synced structure scope: the destination copy is created without it, the
-- comparison ignores it, and no DDL is ever planned for it.
DROP TABLE IF EXISTS t_idx;
CREATE TABLE t_idx (
  id  INT PRIMARY KEY,
  val VARCHAR(32),
  KEY idx_val (val)
) ENGINE=InnoDB;

INSERT INTO t_idx (id, val) VALUES (1, 'x1'), (2, 'x2');

-- t_new: primary key plus a unique key — a created table must reproduce
-- both.
DROP TABLE IF EXISTS t_new;
CREATE TABLE t_new (
  id   INT PRIMARY KEY,
  code VARCHAR(16) NOT NULL,
  UNIQUE KEY u_code (code)
) ENGINE=InnoDB;

INSERT INTO t_new (id, code) VALUES (1, 'c1'), (2, 'c2'), (3, 'c3');
