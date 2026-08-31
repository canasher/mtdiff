-- P1-3 regression: .100 (100ms) vs .010 (10ms). The old trailing-zero strip
-- of the microsecond count rendered both as ".1", so this difference was
-- missed; the fixed renderer distinguishes "0.1" from "0.01".
UPDATE t_fracsec SET d = '2024-01-01 00:00:00.010' WHERE id = 1;
