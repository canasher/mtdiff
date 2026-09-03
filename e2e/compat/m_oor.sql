-- t_oor drift on the dst: four rows deleted, one changed, one below the
-- src minimum (0) and three above the maximum (101..103).
DELETE FROM t_oor WHERE id IN (5, 15, 25, 35);
UPDATE t_oor SET val = 'changed65' WHERE id = 65;
INSERT INTO t_oor VALUES (0, 'zero'), (101, 'h101'), (102, 'h102'), (103, 'h103');
