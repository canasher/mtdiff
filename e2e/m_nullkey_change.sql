-- P1-2 regression: change the NULL-key row only. t_nullkey has no PK and a
-- UNIQUE-but-NULLABLE key column; auto key selection must reject it (keyless
-- multiset semantics). The old code selected it and excluded the NULL row
-- from every chunk predicate, so this change was missed.
UPDATE t_nullkey SET b = 'CHANGED' WHERE a IS NULL;
