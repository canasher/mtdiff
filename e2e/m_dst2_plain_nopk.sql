-- dstdb2.t_plain: the destination copy loses its PRIMARY KEY (the columns
-- and the data stay identical). The structure repair must restore the key,
-- and the repaired table goes back to row-level sync (not a blind full
-- resync). Idempotent: recreated from scratch.
SET time_zone = '+00:00';
DROP TABLE IF EXISTS t_plain;
CREATE TABLE t_plain (
  id  INT NOT NULL,
  val VARCHAR(32)
) ENGINE=InnoDB;

INSERT INTO t_plain (id, val) VALUES (1, 'p1'), (2, 'p2'), (3, 'p3');
