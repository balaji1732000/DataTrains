ALTER TABLE redaction_jobs
  ADD COLUMN purged_at timestamptz;

CREATE INDEX redaction_jobs_unpurged_completed_idx
  ON redaction_jobs (session_id, finished_at DESC, id)
  WHERE state='completed' AND purged_at IS NULL;
