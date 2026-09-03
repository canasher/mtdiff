-- t_keyless on the dst drifts in exactly one way: the PRIMARY KEY is
-- removed; columns and data stay identical to the src. Recreated from
-- scratch so the script is idempotent. (No CTE: 5.7 has none.)
SET time_zone = '+00:00';
DROP TABLE IF EXISTS t_keyless;
CREATE TABLE t_keyless (
  id  INT,
  val VARCHAR(32)
) ENGINE=InnoDB;

INSERT INTO t_keyless (id, val)
SELECT a.d * 10 + b.d + 1, CONCAT('v', a.d * 10 + b.d + 1)
FROM _n a, _n b WHERE a.d < 5;
