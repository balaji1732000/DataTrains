ALTER TABLE processing_jobs
  ADD COLUMN leased_by text,
  ADD COLUMN lease_expires_at timestamptz,
  ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();

CREATE UNIQUE INDEX processing_jobs_one_active_type_per_session
  ON processing_jobs (session_id, job_type)
  WHERE state IN ('queued', 'leased');

CREATE TABLE validation_results (
  id text PRIMARY KEY,
  job_id text NOT NULL UNIQUE REFERENCES processing_jobs(id),
  session_id text NOT NULL REFERENCES sessions(id),
  valid boolean NOT NULL,
  errors jsonb NOT NULL DEFAULT '[]',
  normalized_key text,
  schema_version text NOT NULL,
  validator_version text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((valid AND normalized_key IS NOT NULL) OR (NOT valid AND normalized_key IS NULL))
);

CREATE INDEX validation_results_session_idx ON validation_results (session_id, created_at DESC);
