-- t_genx on the dst: re-created IDENTICAL to the source (same
-- expression, same storage): no structure drift (P1-1).
DROP TABLE IF EXISTS t_genx;
CREATE TABLE t_genx (
  id  INT PRIMARY KEY,
  val INT,
  dbl INT GENERATED ALWAYS AS (val * 2) STORED
) ENGINE=InnoDB;
INSERT INTO t_genx (id, val) VALUES (1, 10), (2, 20);
