-- t_nu on the dst: plain value changes on a UNIQUE-but-nullable column
-- (row 2: NULL -> 'y', row 3: 'z' -> 'w'). The column is UNIQUE and
-- TWO rows seeded NULL: repeated NULLs are legal and NULL tuples never
-- occupy a slot (P1-5), so both changes must stay plain UPDATEs — no
-- destructive rewrite, no DELETE.
UPDATE t_nu SET u = 'y' WHERE id = 2;
UPDATE t_nu SET u = 'w' WHERE id = 3;
