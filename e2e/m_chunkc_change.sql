-- P3-#15 regression: change the max lead-value row only. With --chunk-size
-- 10000 the span 30000 is divisible by the chunk count (4); an off-by-one
-- step would end the last chunk at a=30000 and this change would be missed
-- on both sides (silent false "identical").
UPDATE t_chunkc SET val = 'CHANGED' WHERE a = 30001;
