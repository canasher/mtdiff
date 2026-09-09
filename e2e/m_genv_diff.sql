-- t_genv on the dst: the same expression re-defined as STORED (the
-- source is VIRTUAL): a storage-type drift, refused (P1-1).
DROP TABLE IF EXISTS t_genv;
CREATE TABLE t_genv (
  id  INT PRIMARY KEY,
  val INT,
  dbl INT GENERATED ALWAYS AS (val + 1) STORED
) ENGINE=InnoDB;
INSERT INTO t_genv (id, val) VALUES (1, 10), (2, 20);
