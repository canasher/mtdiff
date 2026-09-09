-- t_wheresparse on the dst: a value drift on a FILTERED row
-- (n % 100 = 0 matches the filter g < 1).
UPDATE t_wheresparse SET v = 'w01x' WHERE k = 'k00100';
