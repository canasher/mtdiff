-- t_oor: the dst copy loses 4 rows, 2 rows are changed, and 4 rows with
-- keys outside the source's key range (id 0 below the minimum, 101..103
-- above the maximum) are added. Equal row counts keep the plan row-level:
-- only the out-of-range delete scan can converge those 4 rows.
SET time_zone = '+00:00';
SET SESSION cte_max_recursion_depth = 1000000;
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

DELETE FROM t_oor WHERE id IN (10, 20, 30, 40);
UPDATE t_oor SET val = CONCAT('x', id) WHERE id IN (50, 60);
INSERT INTO t_oor VALUES (0, 'oor-low'), (101, 'oor-high-1'), (102, 'oor-high-2'), (103, 'oor-high-3');
