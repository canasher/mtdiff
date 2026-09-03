-- srcdb.t_cai: push the source's next AUTO_INCREMENT value to 5001, far
-- above the destination's 11: the table-state sync must raise the
-- destination's counter to match (a raise, valid on every backend).
ALTER TABLE t_cai AUTO_INCREMENT = 5001;
