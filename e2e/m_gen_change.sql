-- t_gen on the dst: one val changed. The generated column re-derives
-- from val on both sides, so only val may appear in the planned UPDATE —
-- the generated column is compared but never written.
UPDATE t_gen SET val = val + 100 WHERE id = 1;
