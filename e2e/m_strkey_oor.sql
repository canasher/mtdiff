-- t_strkey on the dst: a STRAY row whose key sorts ABOVE the source's
-- maximum key (src max is '末\z'): an out-of-range string-key delete.
SET SESSION sql_mode = CONCAT(@@SESSION.sql_mode, ',NO_BACKSLASH_ESCAPES');
INSERT INTO t_strkey VALUES ('末\末', 'stray\row');
