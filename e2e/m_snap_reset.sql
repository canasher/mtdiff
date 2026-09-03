-- t_snap on the dst: rows 1 and 5 back to the seed values (the churn
-- client wrote 'x1' on row 1, m_snap_drift.sql wrote 'drift' on row 5).
UPDATE t_snap SET v = CONCAT('s', id) WHERE id IN (1, 5);
