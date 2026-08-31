-- Same multiset as the seed, inserted in reverse row order.
SET SESSION cte_max_recursion_depth = 1000000;
DROP TABLE IF EXISTS t_nopk;
CREATE TABLE t_nopk (
  v DECIMAL(10,2),
  w VARCHAR(50)
) ENGINE=InnoDB;

INSERT INTO t_nopk (v, w)
WITH RECURSIVE seq(n) AS (
  SELECT 19999
  UNION ALL
  SELECT n - 1 FROM seq WHERE n > 0
)
SELECT MOD(n, 1000) * 0.5, CONCAT('dup-', MOD(n, 50)) FROM seq;
