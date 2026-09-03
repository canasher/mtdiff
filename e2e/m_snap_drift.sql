-- t_snap on the dst: one row drifted (the snapshot scenario's baseline
-- difference; the background churn client edits row 1 on top of it).
UPDATE t_snap SET v = 'drift' WHERE id = 5;
