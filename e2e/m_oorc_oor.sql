-- t_oorc (composite PK): the dst copy loses 5 rows, 1 row is changed, and
-- 4 rows outside the source's key range are added, including the strict
-- boundary probes (1, 0) < min (1, 1) and (10, 4) > max (10, 3). The
-- boundary rows (1, 1) and (10, 3) are left untouched: only an out-of-range
-- scan with an inclusive (wrong) predicate could delete them.
SET time_zone = '+00:00';
SET SESSION cte_max_recursion_depth = 1000000;
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

DELETE FROM t_oorc WHERE (a, b) IN ((2, 2), (5, 3), (8, 1), (9, 2), (4, 3));
UPDATE t_oorc SET val = CONCAT('x', a, b) WHERE (a, b) = (3, 3);
INSERT INTO t_oorc VALUES (0, 5, 'oor-low-a'), (11, 0, 'oor-high-a'),
                          (1, 0, 'boundary-low'), (10, 4, 'boundary-high');
