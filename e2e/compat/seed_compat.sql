-- Compatibility seed for non-8.0 backends (MySQL 5.7, TiDB). MySQL 5.7
-- has no recursive CTEs, so rows are generated from a one-digit helper
-- table joined against itself; the same file works on TiDB as-is.
-- _n is kept for the mutation scripts (they reseed drifted tables).
SET time_zone = '+00:00';

DROP TABLE IF EXISTS _n;
CREATE TABLE _n (d INT) ENGINE=InnoDB;
INSERT INTO _n VALUES (0), (1), (2), (3), (4), (5), (6), (7), (8), (9);

-- t_compat: 1000 rows; row-level sync, --where, zero-match scenarios.
DROP TABLE IF EXISTS t_compat;
CREATE TABLE t_compat (
  id INT PRIMARY KEY,
  name VARCHAR(32),
  amt DECIMAL(10,2),
  ts TIMESTAMP NULL DEFAULT NULL,
  updated_at TIMESTAMP NULL DEFAULT NULL
) ENGINE=InnoDB;
INSERT INTO t_compat (id, name, amt, ts, updated_at)
SELECT a.d * 100 + b.d * 10 + c.d + 1,
       CONCAT('r', a.d * 100 + b.d * 10 + c.d + 1),
       (a.d * 100 + b.d * 10 + c.d + 1) * 1.5,
       '2024-01-01 00:00:00',
       '2024-01-01 00:00:00'
FROM _n a, _n b, _n c;

-- t_oor: 100 rows, INT PK; the out-of-range int-key scenario.
DROP TABLE IF EXISTS t_oor;
CREATE TABLE t_oor (
  id  INT PRIMARY KEY,
  val VARCHAR(32)
) ENGINE=InnoDB;
INSERT INTO t_oor (id, val)
SELECT a.d * 10 + b.d + 1, CONCAT('v', a.d * 10 + b.d + 1)
FROM _n a, _n b;

-- t_oorc: 30 rows, composite PK (a,b), a=1..10 x b=1..3; the
-- out-of-range composite-key scenario (strict boundary probes).
DROP TABLE IF EXISTS t_oorc;
CREATE TABLE t_oorc (
  a   INT,
  b   INT,
  val VARCHAR(32),
  PRIMARY KEY (a, b)
) ENGINE=InnoDB;
INSERT INTO t_oorc (a, b, val)
SELECT x.d + 1, y.d + 1, CONCAT('c', x.d + 1, '-', y.d + 1)
FROM _n x, _n y WHERE y.d < 3;

-- t_oorn: no PK, nullable key column a plus duplicate NULL-key rows;
-- addressed with an explicit --key a (auto selection rejects nullable).
DROP TABLE IF EXISTS t_oorn;
CREATE TABLE t_oorn (
  a INT,
  v VARCHAR(20)
) ENGINE=InnoDB;
INSERT INTO t_oorn VALUES (NULL, 'n1'), (NULL, 'n2'), (1, 'x'), (2, 'y'), (3, 'z');

-- t_struct: 100 rows; the structure-sync scenario drifts the dst copy
-- (m_struct_drift.sql) and expects the sync to realign it.
DROP TABLE IF EXISTS t_struct;
CREATE TABLE t_struct (
  id   INT PRIMARY KEY,
  name VARCHAR(32),
  amt  DECIMAL(10,2),
  ts   TIMESTAMP NULL DEFAULT NULL
) ENGINE=InnoDB;
INSERT INTO t_struct (id, name, amt, ts)
SELECT a.d * 10 + b.d + 1, CONCAT('s', a.d * 10 + b.d + 1),
       (a.d * 10 + b.d + 1) * 1.5, '2024-01-01 00:00:00'
FROM _n a, _n b;

-- t_keyless: 50 rows; the key-drift scenario removes the dst's PRIMARY
-- KEY only (m_keyless_drift.sql).
DROP TABLE IF EXISTS t_keyless;
CREATE TABLE t_keyless (
  id  INT PRIMARY KEY,
  val VARCHAR(32)
) ENGINE=InnoDB;
INSERT INTO t_keyless (id, val)
SELECT a.d * 10 + b.d + 1, CONCAT('v', a.d * 10 + b.d + 1)
FROM _n a, _n b WHERE a.d < 5;

-- t_nopk: 20 rows, no key on either side; the keyless full-resync
-- scenario (m_nopk.sql mutates the dst copy).
DROP TABLE IF EXISTS t_nopk;
CREATE TABLE t_nopk (
  id  INT,
  val VARCHAR(20)
) ENGINE=InnoDB;
INSERT INTO t_nopk (id, val)
SELECT a.d * 2 + b.d + 1, CONCAT('p', a.d * 2 + b.d + 1)
FROM _n a, _n b WHERE b.d < 2;
