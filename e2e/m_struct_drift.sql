-- t_struct on the dst drifts from the src: column amt dropped, id's type
-- changed (int -> bigint), an extra column added, the PRIMARY KEY removed.
-- The data on the shared columns stays identical to the src, so the sync
-- must converge by the structure pre-step alone. Recreated from scratch so
-- the script is idempotent (the sql() helper re-runs it on a transient
-- connection failure).
SET time_zone = '+00:00';
SET SESSION cte_max_recursion_depth = 1000000;
DROP TABLE IF EXISTS t_struct;
CREATE TABLE t_struct (
  id    BIGINT,
  name  VARCHAR(32),
  ts    TIMESTAMP,
  extra VARCHAR(10)
) ENGINE=InnoDB;

INSERT INTO t_struct (id, name, ts, extra)
WITH RECURSIVE seq(n) AS (
  SELECT 1
  UNION ALL
  SELECT n + 1 FROM seq WHERE n < 100
)
SELECT n, CONCAT('s', n), '2024-01-01 00:00:00', 'x' FROM seq;
