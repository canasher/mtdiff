-- P1-1 regression: change the max id row only. With --chunk-size 10000 the
-- span 90000 is divisible by the chunk count, so the old off-by-one left
-- row 90001 out of every chunk on BOTH sides and this change was missed.
UPDATE t_chunk SET val = 'CHANGED' WHERE id = 90001;
