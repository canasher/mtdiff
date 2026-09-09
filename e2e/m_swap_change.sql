-- t_swap on the dst: one row's NON-key column changed while the unique
-- key value (v) stays put. With an explicit --key v (a NOT NULL UNIQUE
-- column), a recognized-unique key must converge this with a plain
-- row-level UPDATE; group-replace semantics (a non-unique key) would
-- delete the v='A' group and re-insert it instead.
UPDATE t_swap SET id = 99 WHERE id = 2;
