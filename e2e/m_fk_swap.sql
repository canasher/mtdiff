-- t_fk on the dst: the parent's unique codes are swapped (through a
-- temporary value to dodge the unique index). The child (t_fkc) rows
-- reference both parents with ON DELETE CASCADE: a destructive parent
-- rewrite would cascade-delete them.
UPDATE t_fk SET code = '__t__' WHERE id = 1;
UPDATE t_fk SET code = 'A' WHERE id = 2;
UPDATE t_fk SET code = 'B' WHERE id = 1;
