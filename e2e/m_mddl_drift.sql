-- t_mddl on the dst: the column x and the unique index are missing,
-- and the data carries DUPLICATE codes. The structure repair is two
-- statements (add x; then the unique index on (code,x) referencing the
-- new column): statement 2 must fail on the duplicates (P1-2).
DROP TABLE IF EXISTS t_mddl;
CREATE TABLE t_mddl (
  id   INT PRIMARY KEY,
  code VARCHAR(16)
) ENGINE=InnoDB;
INSERT INTO t_mddl VALUES (1, 'A'), (2, 'A'), (3, 'B'), (4, 'A');
