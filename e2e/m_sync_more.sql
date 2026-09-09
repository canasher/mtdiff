-- t_mut: dst gains 500 extra rows (id 5001..5500, 1500 vs 1000), all
-- outside the source's key range -> the extra rows converge via the
-- streaming out-of-range delete. The row counts never decide the mode:
-- no TRUNCATE, no full resync.
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

INSERT INTO t_mut (id, val, updated_at)
WITH RECURSIVE seq(n) AS (
  SELECT 1
  UNION ALL
  SELECT n + 1 FROM seq WHERE n < 500
)
SELECT 5000 + n, CONCAT('extra-', n), '2024-01-01 00:00:00' FROM seq;
