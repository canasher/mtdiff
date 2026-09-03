-- t_oorn drift on the dst (addressed with --key a): the row (2, 'y')
-- deleted, an out-of-range row (5, 'stray') added. The NULL-key rows
-- must survive.
DELETE FROM t_oorn WHERE a = 2 AND v = 'y';
INSERT INTO t_oorn VALUES (5, 'stray');
