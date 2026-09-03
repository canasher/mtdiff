-- t_oorc drift on the dst: five rows deleted, one changed, out-of-range
-- rows (0,5)/(11,0) plus the strict boundary probes (1,0) below min
-- (1,1) and (10,4) above max (10,3).
DELETE FROM t_oorc WHERE (a, b) IN ((2, 2), (3, 1), (4, 3), (5, 2), (6, 1));
UPDATE t_oorc SET val = 'changed7-2' WHERE a = 7 AND b = 2;
INSERT INTO t_oorc VALUES (0, 5, 'x05'), (11, 0, 'x110'), (1, 0, 'b10'), (10, 4, 'b104');
