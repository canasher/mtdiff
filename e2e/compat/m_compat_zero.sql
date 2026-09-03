-- t_compat: rows beyond the zero-match filter's domain (id >= 1000000
-- matches nothing on the src). The filtered sync must delete them from
-- the dst via the destination-delete path.
INSERT INTO t_compat (id, name, amt, ts, updated_at)
VALUES (1000001, 'z1', 1, '2024-01-01 00:00:00', '2024-01-01 00:00:00'),
       (1000002, 'z2', 2, '2024-01-01 00:00:00', '2024-01-01 00:00:00'),
       (1000003, 'z3', 3, '2024-01-01 00:00:00', '2024-01-01 00:00:00');
