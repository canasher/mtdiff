-- t_bigint on the dst: the MAX BIGINT row's value changed. The table's
-- key span (MinInt64..MaxInt64) is wider than MaxInt64 values, so the
-- planner must refuse arithmetic chunking and sample instead; the change
-- to the extreme row must still be detected and converged.
UPDATE t_bigint SET val = 'maxX' WHERE id = 9223372036854775807;
