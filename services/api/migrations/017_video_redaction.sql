CREATE TABLE redaction_plans (
  id text PRIMARY KEY,
  session_id text NOT NULL REFERENCES sessions(id),
  reviewer_id text NOT NULL,
  version integer NOT NULL CHECK (version > 0),
  schema_version text NOT NULL CHECK (schema_version='redaction/v1'),
  document jsonb NOT NULL,
  document_hash char(64) NOT NULL,
  request_id text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (session_id, version),
  UNIQUE (session_id, request_id)
);

CREATE TABLE redaction_jobs (
  id text PRIMARY KEY,
  plan_id text NOT NULL UNIQUE REFERENCES redaction_plans(id),
  session_id text NOT NULL REFERENCES sessions(id),
  state text NOT NULL CHECK (state IN ('queued', 'leased', 'completed', 'dead_letter')),
  attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
  manual_requeues integer NOT NULL DEFAULT 0 CHECK (manual_requeues >= 0),
  available_at timestamptz NOT NULL DEFAULT now(),
  leased_by text,
  lease_expires_at timestamptz,
  output_manifest_key text,
  error text,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz,
  dead_lettered_at timestamptz,
  CHECK ((state='completed' AND output_manifest_key IS NOT NULL AND finished_at IS NOT NULL)
      OR state<>'completed'),
  CHECK ((state='dead_letter' AND dead_lettered_at IS NOT NULL)
      OR (state<>'dead_letter' AND dead_lettered_at IS NULL))
);

CREATE INDEX redaction_plans_session_idx
  ON redaction_plans (session_id, version DESC);
CREATE INDEX redaction_jobs_claim_idx
  ON redaction_jobs (available_at, id)
  WHERE state IN ('queued', 'leased');
CREATE INDEX redaction_jobs_dead_letter_idx
  ON redaction_jobs (dead_lettered_at DESC, id)
  WHERE state='dead_letter';

ALTER TABLE redaction_plans ENABLE ROW LEVEL SECURITY;
ALTER TABLE redaction_jobs ENABLE ROW LEVEL SECURITY;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname='datatrains_runtime') THEN
    GRANT SELECT, INSERT, UPDATE, DELETE ON redaction_plans, redaction_jobs TO datatrains_runtime;
    CREATE POLICY datatrains_runtime_all ON redaction_plans
      FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
    CREATE POLICY datatrains_runtime_all ON redaction_jobs
      FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
  END IF;
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname='anon') THEN
    REVOKE ALL PRIVILEGES ON redaction_plans, redaction_jobs FROM anon;
  END IF;
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname='authenticated') THEN
    REVOKE ALL PRIVILEGES ON redaction_plans, redaction_jobs FROM authenticated;
  END IF;
END
$$;
