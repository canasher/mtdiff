-- t_comp on the dst: a plain value drift (row 3, b p -> p9). 'x'
-- repeats across rows 3 and 4 (different b): a composite UNIQUE(a,b)
-- must not treat the repeated a as a conflict (P1-5).
UPDATE t_comp SET b = 'p9' WHERE id = 3;
