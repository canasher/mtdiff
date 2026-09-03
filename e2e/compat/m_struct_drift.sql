-- t_struct on the dst drifts: column amt dropped, id's type changed
-- (int -> bigint), an extra column added, the PRIMARY KEY removed. The
-- data on the shared columns stays identical to the src. Recreated from
-- scratch so the script is idempotent. (No CTE: 5.7 has none.)
SET time_zone = '+00:00';
DROP TABLE IF EXISTS t_struct;
CREATE TABLE t_struct (
  id    BIGINT,
  name  VARCHAR(32),
  ts    TIMESTAMP NULL DEFAULT NULL,
  extra VARCHAR(10)
) ENGINE=InnoDB;

INSERT INTO t_struct (id, name, ts, extra)
SELECT a.d * 10 + b.d + 1, CONCAT('s', a.d * 10 + b.d + 1),
       '2024-01-01 00:00:00', 'x'
FROM _n a, _n b;
