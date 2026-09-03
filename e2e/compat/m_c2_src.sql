-- srcdb: a table the destination does not have -> whole-database sync
-- must create it on the destination.
SET time_zone = '+00:00';
DROP TABLE IF EXISTS t_c2;
CREATE TABLE t_c2 (
  id  INT PRIMARY KEY,
  val VARCHAR(20)
) ENGINE=InnoDB;
INSERT INTO t_c2 (id, val)
SELECT b.d + 1, CONCAT('q', b.d + 1)
FROM _n b WHERE b.d < 10;
