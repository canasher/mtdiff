-- t_structfail on the dst: the column is widened and a value is stored
-- that does not fit the src type (DECIMAL(4,2) tops out at 99.99). The
-- in-place structure ALTER (back to DECIMAL(4,2)) must therefore FAIL,
-- and the default behavior must keep the data instead of truncating.
ALTER TABLE t_structfail MODIFY amt DECIMAL(10,2);
UPDATE t_structfail SET amt = 12345.67 WHERE id = 1;
