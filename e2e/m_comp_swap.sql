-- t_comp on the dst: the WHOLE composite tuple of rows 1 and 2 swaps
-- ((a,b) <-> (b,a)): a true unique-tuple conflict (P1-5). Rows 3/4
-- stay as seeded. (Through distinct temporary tuples to dodge uk_ab.)
UPDATE t_comp SET a = '__z1__', b = '__z1__' WHERE id = 1;
UPDATE t_comp SET a = '__z2__', b = '__z2__' WHERE id = 2;
UPDATE t_comp SET a = 'b', b = 'a' WHERE id = 1;
UPDATE t_comp SET a = 'a', b = 'b' WHERE id = 2;
