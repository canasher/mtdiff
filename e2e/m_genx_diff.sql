-- t_genx on the dst: the SAME generated column with a DIFFERENT
-- expression (val * 3): the structure comparison must detect the
-- expression drift and refuse (P1-1).
DROP TABLE IF EXISTS t_genx;
CREATE TABLE t_genx (
  id  INT PRIMARY KEY,
  val INT,
  dbl INT GENERATED ALWAYS AS (val * 3) STORED
) ENGINE=InnoDB;
INSERT INTO t_genx (id, val) VALUES (1, 10), (2, 20);
