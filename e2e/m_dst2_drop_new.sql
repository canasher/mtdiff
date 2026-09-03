-- dstdb2: remove t_new (the source still has it) to re-enter the
-- "missing on the destination" state.
DROP TABLE IF EXISTS t_new;
