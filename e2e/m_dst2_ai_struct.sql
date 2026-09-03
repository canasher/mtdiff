-- dstdb2.t_ai: structure drift (val VARCHAR(64) vs the source's VARCHAR(32))
-- on an auto-increment table, counter drifted to 1200. The repair truncates
-- and reloads (which resets the counter), so the state must be re-aligned
-- to the source's 1500 afterwards. Idempotent: recreated from scratch.
SET time_zone = '+00:00';
SET SESSION cte_max_recursion_depth = 1000000;
DROP TABLE IF EXISTS t_ai;
CREATE TABLE t_ai (
  id  INT PRIMARY KEY AUTO_INCREMENT,
  val VARCHAR(64)
) ENGINE=InnoDB;

INSERT INTO t_ai (id, val)
WITH RECURSIVE seq(n) AS (
  SELECT 1
  UNION ALL
  SELECT n + 1 FROM seq WHERE n < 100
)
SELECT n, CONCAT('a', n) FROM seq;

ALTER TABLE t_ai AUTO_INCREMENT = 1200;
