-- t_u3 on the dst: the 3-row CYCLE (1->B, 2->C, 3->A): src holds
-- (1,'A')(2,'B')(3,'C'). No order of per-row UPDATEs applies a cycle
-- (every step duplicates a still-held value): the destructive rewrite
-- is refused by default (P0-2). (The seed must dodge the unique index:
-- swap through temporary values.)
UPDATE t_u3 SET u = '__t1__' WHERE id = 1;
UPDATE t_u3 SET u = '__t2__' WHERE id = 2;
UPDATE t_u3 SET u = '__t3__' WHERE id = 3;
UPDATE t_u3 SET u = 'B' WHERE id = 1;
UPDATE t_u3 SET u = 'C' WHERE id = 2;
UPDATE t_u3 SET u = 'A' WHERE id = 3;
