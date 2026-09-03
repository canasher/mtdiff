-- Data drift on the keyless dst copy of t_keyless (run after
-- m_keyless_drift.sql): three rows deleted, one value changed, three
-- rows added beyond the src's key range. The row counts stay equal
-- (50), so the comparison runs the keyless multiset scan instead of
-- short-circuiting on a count mismatch.
DELETE FROM t_keyless WHERE id IN (7, 17, 27);
UPDATE t_keyless SET val = 'changed47' WHERE id = 47;
INSERT INTO t_keyless (id, val) VALUES (51, 'e51'), (52, 'e52'), (53, 'e53');
