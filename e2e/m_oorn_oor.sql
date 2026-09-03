-- t_oorn (addressed with --key a, a nullable column): the dst copy is
-- missing the (2, 'y') row and gains an out-of-range (5, 'stray'). The
-- source's minimum key is NULL, so the lower tail is empty and only the
-- upper tail can match; the duplicate NULL-key rows must survive.
SET time_zone = '+00:00';
SET SESSION cte_max_recursion_depth = 1000000;
DROP TABLE IF EXISTS t_oorn;
CREATE TABLE t_oorn (
  a INT,
  v VARCHAR(20)
) ENGINE=InnoDB;

INSERT INTO t_oorn VALUES (NULL, 'n1'), (NULL, 'n2'), (1, 'x'), (2, 'y'), (3, 'z');

DELETE FROM t_oorn WHERE a = 2;
INSERT INTO t_oorn VALUES (5, 'stray');
