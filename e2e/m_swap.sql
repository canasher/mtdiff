-- t_swap on the dst: the two rows' values are exchanged. src holds
-- (1,'B'),(2,'A'); the dst now holds (1,'A'),(2,'B'). Converging the dst
-- to the src requires an intermediate state in which both 'A' and 'B'
-- would exist twice, violating the unique index — the sync must convert
-- the pair to delete+insert instead of updating it in place.
-- (The seed itself must dodge the unique index: swap through a temporary
-- value, three steps.)
UPDATE t_swap SET v = '__tmp__' WHERE id = 1;
UPDATE t_swap SET v = 'B' WHERE id = 2;
UPDATE t_swap SET v = 'A' WHERE id = 1;
