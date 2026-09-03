-- t_mut with --where: the dst copy gains 3 rows outside the source's key
-- range, 2 of which match the filter (updated_at < '2025-01-01') and one
-- of which does not — the one documented residual a filtered table can
-- never converge — plus one in-range change.
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

UPDATE t_mut SET val = 'x7' WHERE id = 7;
INSERT INTO t_mut VALUES (1001, 'v1001', '2024-06-01 00:00:00'),
                        (1002, 'v1002', '2026-06-01 00:00:00'),
                        (1003, 'v1003', '2024-07-01 00:00:00');
