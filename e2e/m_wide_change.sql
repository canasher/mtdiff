-- t_wide on the dst: a few column drifts across the 120 columns.
UPDATE t_wide SET c01 = 99, c60 = 99, c120 = 99 WHERE id = 1;
