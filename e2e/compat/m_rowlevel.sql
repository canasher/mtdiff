-- t_compat data drift on the dst (row counts stay equal at 1000):
-- three rows deleted, two values changed, two rows added beyond the
-- source's key range (1..1000) — the sync must delete those (out-of-range
-- cleanup), not insert them.
DELETE FROM t_compat WHERE id IN (7, 17, 27);
UPDATE t_compat SET amt = amt + 100 WHERE id IN (47, 470);
INSERT INTO t_compat (id, name, amt, ts, updated_at)
VALUES (1001, 'r1001', 999.50, '2024-01-01 00:00:00', '2024-01-01 00:00:00'),
       (1002, 'r1002', 998.50, '2024-01-01 00:00:00', '2024-01-01 00:00:00');
