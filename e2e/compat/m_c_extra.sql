-- dstdb: a table the source does not have -> whole-database sync must
-- drop it (destructive). Idempotent: recreated from scratch.
SET time_zone = '+00:00';
DROP TABLE IF EXISTS t_c_extra;
CREATE TABLE t_c_extra (
  id  INT PRIMARY KEY,
  val VARCHAR(20)
) ENGINE=InnoDB;
INSERT INTO t_c_extra (id, val) VALUES (1, 'x1'), (2, 'x2'), (3, 'x3');
