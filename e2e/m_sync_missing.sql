-- t_mut: dst is missing 100 rows (id 900..999) -> sync must insert them
-- row-level (destination has fewer rows than source: no TRUNCATE).
DELETE FROM t_mut WHERE id > 899;
