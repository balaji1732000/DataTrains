ALTER TABLE processing_jobs DROP CONSTRAINT processing_jobs_state_check;
UPDATE processing_jobs SET state='dead_letter' WHERE state='failed';
ALTER TABLE processing_jobs ADD CONSTRAINT processing_jobs_state_check
  CHECK (state IN ('queued', 'leased', 'completed', 'dead_letter'));

ALTER TABLE processing_jobs
  ADD COLUMN dead_lettered_at timestamptz,
  ADD COLUMN manual_requeues integer NOT NULL DEFAULT 0 CHECK (manual_requeues >= 0);

UPDATE processing_jobs
SET dead_lettered_at=COALESCE(finished_at, updated_at)
WHERE state='dead_letter';

ALTER TABLE processing_jobs ADD CONSTRAINT processing_jobs_dead_letter_time_check
  CHECK ((state='dead_letter' AND dead_lettered_at IS NOT NULL)
      OR (state<>'dead_letter' AND dead_lettered_at IS NULL));

CREATE INDEX processing_jobs_dead_letter_idx
  ON processing_jobs (dead_lettered_at DESC, id)
  WHERE state='dead_letter';

ALTER TABLE processing_jobs ENABLE ROW LEVEL SECURITY;
