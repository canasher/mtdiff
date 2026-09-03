-- dstdb2: a table the source does not have. In whole-database mode the
-- destination is a disposable copy of the source, so it is dropped.
DROP TABLE IF EXISTS t_extra;
CREATE TABLE t_extra (
  id   INT PRIMARY KEY,
  note VARCHAR(64)
) ENGINE=InnoDB;
INSERT INTO t_extra (id, note) VALUES (1, 'should not survive a whole-database sync');
