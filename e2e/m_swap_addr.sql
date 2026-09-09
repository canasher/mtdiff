-- t_swap on the dst: the unique key value itself changed (v 'A' -> 'Z').
-- Addressed by --key v, the row's address moved: the only correct
-- convergence is delete + insert, never an UPDATE.
UPDATE t_swap SET v = 'Z' WHERE id = 2;
