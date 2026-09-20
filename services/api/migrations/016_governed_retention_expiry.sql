ALTER TABLE artifacts
  ADD COLUMN purged_at timestamptz;

ALTER TABLE validation_results
  ADD COLUMN purged_at timestamptz;

ALTER TABLE dataset_releases
  ADD COLUMN purged_at timestamptz;

CREATE TABLE retention_purge_requests (
  id text PRIMARY KEY,
  organization_id text NOT NULL REFERENCES organizations(id),
  resource_type text NOT NULL CHECK (resource_type IN ('session_raw', 'session_derived', 'release')),
  resource_id text NOT NULL,
  policy_updated_at timestamptz NOT NULL,
  object_keys jsonb NOT NULL CHECK (jsonb_typeof(object_keys)='array'),
  state text NOT NULL CHECK (state IN ('queued', 'leased', 'completed', 'blocked', 'cancelled', 'dead_letter')),
  attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
  available_at timestamptz NOT NULL,
  leased_by text,
  lease_expires_at timestamptz,
  requested_at timestamptz NOT NULL,
  completed_at timestamptz,
  last_error text,
  updated_at timestamptz NOT NULL,
  CHECK ((state='leased' AND leased_by IS NOT NULL AND lease_expires_at IS NOT NULL)
      OR (state<>'leased' AND leased_by IS NULL AND lease_expires_at IS NULL)),
  CHECK ((state='completed' AND completed_at IS NOT NULL)
      OR (state<>'completed' AND completed_at IS NULL))
);

CREATE UNIQUE INDEX retention_purge_one_active_resource
  ON retention_purge_requests (resource_type, resource_id)
  WHERE state IN ('queued', 'leased', 'blocked');
CREATE INDEX retention_purge_claim_idx
  ON retention_purge_requests (state, available_at, id);
CREATE INDEX artifacts_retention_idx
  ON artifacts (session_id, created_at) WHERE purged_at IS NULL;
CREATE INDEX validation_results_retention_idx
  ON validation_results (session_id, created_at) WHERE purged_at IS NULL;
CREATE INDEX dataset_releases_retention_idx
  ON dataset_releases (project_id, created_at) WHERE purged_at IS NULL;

ALTER TABLE retention_purge_requests ENABLE ROW LEVEL SECURITY;
CREATE POLICY datatrains_runtime_all ON retention_purge_requests
  FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'anon') THEN
    REVOKE ALL PRIVILEGES ON retention_purge_requests FROM anon;
  END IF;
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'authenticated') THEN
    REVOKE ALL PRIVILEGES ON retention_purge_requests FROM authenticated;
  END IF;
END
$$;
