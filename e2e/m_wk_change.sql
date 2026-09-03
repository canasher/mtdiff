-- t_wk on the dst: one row's k changed, so the table actually differs
-- (an identical table is skipped before its plan is even computed, which
-- would mask the --where + non-unique --key rejection).
UPDATE t_wk SET k = 99 WHERE id = 1;
