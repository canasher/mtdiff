-- Same row count, one value changed: only the multiset digest can catch it.
UPDATE t_nopk SET v = v + 100 WHERE w = 'dup-0' LIMIT 1;
