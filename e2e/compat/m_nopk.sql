-- t_nopk drift on the dst (no key on either side): two rows deleted,
-- one changed, two added. The keyless pair must converge by a full
-- resync.
DELETE FROM t_nopk WHERE id IN (3, 11);
UPDATE t_nopk SET val = 'changed15' WHERE id = 15;
INSERT INTO t_nopk VALUES (21, 'q21'), (22, 'q22');
