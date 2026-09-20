CREATE TABLE organizations (
  id text PRIMARY KEY,
  name text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE projects (
  id text PRIMARY KEY,
  organization_id text NOT NULL REFERENCES organizations(id),
  name text NOT NULL,
  target_trajectories integer NOT NULL CHECK (target_trajectories > 0),
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE task_templates (
  id text PRIMARY KEY,
  project_id text NOT NULL REFERENCES projects(id),
  name text NOT NULL,
  goal text NOT NULL,
  category text NOT NULL,
  difficulty text NOT NULL CHECK (difficulty IN ('beginner', 'intermediate', 'advanced')),
  specification jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE tasks (
  id text PRIMARY KEY,
  template_id text NOT NULL REFERENCES task_templates(id),
  goal text NOT NULL,
  input_assets jsonb NOT NULL DEFAULT '[]',
  expected_outputs jsonb NOT NULL DEFAULT '[]',
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE contributors (
  id text PRIMARY KEY,
  display_name text NOT NULL,
  status text NOT NULL DEFAULT 'active',
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE assignments (
  id text PRIMARY KEY,
  task_id text NOT NULL REFERENCES tasks(id),
  contributor_id text NOT NULL REFERENCES contributors(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (task_id, contributor_id)
);

CREATE TABLE consent_documents (
  id text PRIMARY KEY,
  version text NOT NULL UNIQUE,
  text_hash char(64) NOT NULL,
  body text NOT NULL,
  effective_date timestamptz NOT NULL
);

CREATE TABLE consent_acceptances (
  id text PRIMARY KEY,
  contributor_id text NOT NULL REFERENCES contributors(id),
  document_id text NOT NULL REFERENCES consent_documents(id),
  accepted_at timestamptz NOT NULL,
  client_version text NOT NULL,
  UNIQUE (contributor_id, document_id)
);

CREATE TABLE sessions (
  id text PRIMARY KEY,
  assignment_id text NOT NULL REFERENCES assignments(id),
  consent_acceptance_id text REFERENCES consent_acceptances(id),
  state text NOT NULL CHECK (state IN (
    'CREATED', 'ASSIGNED', 'READY', 'RECORDING', 'FINALIZING', 'UPLOADING',
    'SUBMITTED', 'PROCESSING', 'READY_FOR_REVIEW', 'ACCEPTED', 'RELEASED',
    'FAILED', 'REJECTED', 'REWORK_REQUIRED', 'CANCELLED'
  )),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE artifacts (
  id text PRIMARY KEY,
  session_id text NOT NULL REFERENCES sessions(id),
  logical_key text NOT NULL UNIQUE,
  sha256 char(64) NOT NULL,
  size_bytes bigint NOT NULL CHECK (size_bytes > 0),
  media_type text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE processing_jobs (
  id text PRIMARY KEY,
  session_id text NOT NULL REFERENCES sessions(id),
  job_type text NOT NULL,
  state text NOT NULL CHECK (state IN ('queued', 'leased', 'completed', 'failed')),
  attempt integer NOT NULL DEFAULT 0,
  available_at timestamptz NOT NULL DEFAULT now(),
  started_at timestamptz,
  finished_at timestamptz,
  error text
);
CREATE INDEX processing_jobs_claim_idx ON processing_jobs (state, available_at);

CREATE TABLE reviews (
  id text PRIMARY KEY,
  session_id text NOT NULL REFERENCES sessions(id),
  reviewer_id text NOT NULL,
  rubric_version text NOT NULL,
  scores jsonb NOT NULL,
  comments text NOT NULL DEFAULT '',
  decision text NOT NULL CHECK (decision IN ('accepted', 'rejected', 'rework_required')),
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE dataset_releases (
  id text PRIMARY KEY,
  project_id text NOT NULL REFERENCES projects(id),
  name text NOT NULL,
  schema_version text NOT NULL,
  exporter_version text NOT NULL,
  pipeline_version text NOT NULL,
  source_session_ids jsonb NOT NULL,
  configuration_hash char(64) NOT NULL,
  manifest_hash char(64),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (project_id, name)
);

CREATE TABLE audit_events (
  sequence_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  actor text NOT NULL,
  action text NOT NULL,
  resource text NOT NULL,
  occurred_at timestamptz NOT NULL,
  request_id text NOT NULL,
  metadata jsonb NOT NULL DEFAULT '{}'
);
REVOKE UPDATE, DELETE ON audit_events FROM PUBLIC;

CREATE FUNCTION prevent_audit_event_mutation() RETURNS trigger AS $$
BEGIN
  RAISE EXCEPTION 'audit_events is append-only';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER audit_events_append_only
BEFORE UPDATE OR DELETE ON audit_events
FOR EACH ROW EXECUTE FUNCTION prevent_audit_event_mutation();
