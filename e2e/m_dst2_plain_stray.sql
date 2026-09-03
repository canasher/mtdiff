-- dstdb2.t_plain: one stray row (id 999) beyond the source's 3 rows -> a
-- single row-level DELETE, never a full resync.
INSERT INTO t_plain (id, val) VALUES (999, 'stray');
