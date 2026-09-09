-- t_esc on the dst: one backslash value changed, one row deleted. Seeded
-- with NO_BACKSLASH_ESCAPES so the literals stay literal (the value for
-- id=2 becomes the three characters a\z). After the sync the full table
-- must match the src byte for byte (checked via HEX on both sides).
SET SESSION sql_mode = CONCAT(@@SESSION.sql_mode, ',NO_BACKSLASH_ESCAPES');
UPDATE t_esc SET val = 'a\z' WHERE id = 2;
DELETE FROM t_esc WHERE id = 6;
