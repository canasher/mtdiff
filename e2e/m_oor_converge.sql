-- t_mut (no --where, equal counts): the dst copy loses 3 rows and gains 3
-- rows outside the source's key range (src holds id 1..1000). Row-level
-- with the out-of-range scan must converge in the first round — no
-- TRUNCATE, no second-round escalation to a full resync.
SET time_zone = '+00:00';
SET SESSION cte_max_recursion_depth = 1000000;
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

DELETE FROM t_mut WHERE id IN (997, 998, 999);
INSERT INTO t_mut VALUES (1001, 'v1001', '2024-01-01 00:00:00'),
                        (1002, 'v1002', '2024-01-01 00:00:00'),
                        (1003, 'v1003', '2024-01-01 00:00:00');
