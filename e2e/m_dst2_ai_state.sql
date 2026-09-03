-- dstdb2.t_ai: the data is identical, only the table state (the next
-- AUTO_INCREMENT value) drifted from the source's 1500 to 1200.
-- Recreated from scratch (no starting value) and raised to 1200: the
-- server refuses to LOWER a counter, so the drift is created on a fresh
-- table and the sync's job is to RAISE the destination to the source's
-- 1500 (the direction every backend allows).
SET time_zone = '+00:00';
SET SESSION cte_max_recursion_depth = 1000000;
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

ALTER TABLE t_ai AUTO_INCREMENT = 1200;
