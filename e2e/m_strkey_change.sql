-- t_strkey on the dst: values drift on three rows whose KEYS carry
-- backslashes/quotes/CJK (the read predicates must address them).
SET SESSION sql_mode = CONCAT(@@SESSION.sql_mode, ',NO_BACKSLASH_ESCAPES');
UPDATE t_strkey SET v = 'v01x' WHERE k = 'a\b';
UPDATE t_strkey SET v = 'v04x' WHERE k = '中文\测试';
UPDATE t_strkey SET v = 'v05x' WHERE k = 'C:\abc\def';
