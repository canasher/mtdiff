-- t_xchunk on the dst: the unique values of row 1 (chunk 1, rows 1-6 at
-- chunk size 10) and row 12 (chunk 2) are SWAPPED ('v1' <-> 'v12'): a
-- CROSS-CHUNK unique swap (P1-6) — the value row 1 takes ('v12') is held
-- by row 12 in a LATER chunk, so no per-chunk commit order applies it.
-- (Through a temporary value to dodge the unique index mid-script.)
UPDATE t_xchunk SET u = '__x__' WHERE id = 1;
UPDATE t_xchunk SET u = 'v1' WHERE id = 12;
UPDATE t_xchunk SET u = 'v12' WHERE id = 1;
