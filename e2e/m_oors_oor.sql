-- t_oors (VARCHAR PK): the dst copy loses 3 rows, 1 row is changed, and 3
-- keys outside the source's range are added ('a0' below 'k001', 'z9' and
-- 'k099' above 'k050') — the quoted-string literal path for character keys.
SET time_zone = '+00:00';
SET SESSION cte_max_recursion_depth = 1000000;
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

DELETE FROM t_oors WHERE k IN ('k010', 'k020', 'k030');
UPDATE t_oors SET val = 'x40' WHERE k = 'k040';
INSERT INTO t_oors VALUES ('a0', 'oor-low'), ('z9', 'oor-high-z'), ('k099', 'oor-high-k');
