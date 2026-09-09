-- t_gen on the dst: the generated column is dropped (structure drift).
-- The src still carries it, so the structure sync must REFUSE the table
-- (the generation expression cannot be reproduced on the destination)
-- instead of dropping or re-adding it — and the data must be untouched.
-- Plain DROP (MySQL has no DROP COLUMN IF EXISTS; the column is seeded
-- on both sides, so the plain form always applies here).
ALTER TABLE t_gen DROP COLUMN doubled;
