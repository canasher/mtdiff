-- t_two on the dst: plain drifts. Row 2's email '911' equals row 1's
-- phone '911': two DIFFERENT constraints must not cross-collide
-- (P1-5) — the sync stays on plain updates.
UPDATE t_two SET email = 'q@e' WHERE id = 1;
UPDATE t_two SET phone = '333' WHERE id = 2;
