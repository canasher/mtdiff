-- t_keyless on the dst drifts in exactly one way: the PRIMARY KEY is
-- removed. The columns and the data stay identical to the src, so the
-- comparison is still possible (compatible columns) but no shared key
-- exists to plan chunks by: the diff must fall back to a keyless
-- whole-table comparison, and the sync must take the full resync.
-- Recreated from scratch so the script is idempotent (the sql() helper
-- re-runs it on a transient connection failure).
DROP TABLE IF EXISTS t_keyless;
CREATE TABLE t_keyless (
  id  INT,
  val VARCHAR(32)
) ENGINE=InnoDB;

INSERT INTO t_keyless (id, val)
WITH RECURSIVE seq(n) AS (
  SELECT 1
  UNION ALL
  SELECT n + 1 FROM seq WHERE n < 50
)
SELECT n, CONCAT('v', n) FROM seq;
