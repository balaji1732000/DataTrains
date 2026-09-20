ALTER TABLE sessions DROP CONSTRAINT sessions_state_check;
ALTER TABLE sessions ADD CONSTRAINT sessions_state_check CHECK (state IN (
  'CREATED', 'ASSIGNED', 'READY', 'RECORDING', 'FINALIZING', 'UPLOADING',
  'SUBMITTED', 'PROCESSING', 'READY_FOR_REVIEW', 'ACCEPTED', 'RELEASED',
  'FAILED', 'REJECTED', 'REWORK_REQUIRED', 'CANCELLED', 'DELETED'
));

CREATE TABLE retention_policies (
  organization_id text PRIMARY KEY REFERENCES organizations(id),
  raw_days integer NOT NULL CHECK (raw_days BETWEEN 1 AND 3650),
  derived_days integer NOT NULL CHECK (derived_days BETWEEN 1 AND 3650),
  release_days integer NOT NULL CHECK (release_days BETWEEN 1 AND 3650),
  updated_by text NOT NULL REFERENCES accounts(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE legal_holds (
  id text PRIMARY KEY,
  organization_id text NOT NULL REFERENCES organizations(id),
  session_id text NOT NULL REFERENCES sessions(id),
  reason text NOT NULL CHECK (length(reason) BETWEEN 3 AND 2000),
  placed_by text NOT NULL REFERENCES accounts(id),
  placed_at timestamptz NOT NULL,
  released_by text REFERENCES accounts(id),
  released_at timestamptz,
  CHECK ((released_by IS NULL AND released_at IS NULL)
      OR (released_by IS NOT NULL AND released_at IS NOT NULL))
);
CREATE UNIQUE INDEX legal_holds_one_active_per_session
  ON legal_holds (session_id) WHERE released_at IS NULL;
CREATE INDEX legal_holds_organization_idx
  ON legal_holds (organization_id, placed_at DESC);

CREATE TABLE deletion_requests (
  id text PRIMARY KEY,
  organization_id text NOT NULL REFERENCES organizations(id),
  session_id text NOT NULL REFERENCES sessions(id),
  reason text NOT NULL CHECK (length(reason) BETWEEN 3 AND 2000),
  object_keys jsonb NOT NULL CHECK (jsonb_typeof(object_keys)='array'),
  state text NOT NULL CHECK (state IN ('queued', 'leased', 'completed', 'blocked', 'dead_letter')),
  attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
  available_at timestamptz NOT NULL,
  leased_by text,
  lease_expires_at timestamptz,
  requested_by text NOT NULL REFERENCES accounts(id),
  requested_at timestamptz NOT NULL,
  completed_at timestamptz,
  last_error text,
  updated_at timestamptz NOT NULL,
  CHECK ((state='leased' AND leased_by IS NOT NULL AND lease_expires_at IS NOT NULL)
      OR (state<>'leased' AND leased_by IS NULL AND lease_expires_at IS NULL)),
  CHECK ((state='completed' AND completed_at IS NOT NULL)
      OR (state<>'completed' AND completed_at IS NULL))
);
CREATE UNIQUE INDEX deletion_requests_one_active_per_session
  ON deletion_requests (session_id)
  WHERE state IN ('queued', 'leased', 'blocked');
CREATE INDEX deletion_requests_claim_idx
  ON deletion_requests (state, available_at, id);

ALTER TABLE retention_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE legal_holds ENABLE ROW LEVEL SECURITY;
ALTER TABLE deletion_requests ENABLE ROW LEVEL SECURITY;
CREATE POLICY datatrains_runtime_all ON retention_policies FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON legal_holds FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON deletion_requests FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'anon') THEN
    REVOKE ALL PRIVILEGES ON retention_policies, legal_holds, deletion_requests FROM anon;
  END IF;
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'authenticated') THEN
    REVOKE ALL PRIVILEGES ON retention_policies, legal_holds, deletion_requests FROM authenticated;
  END IF;
END
$$;
