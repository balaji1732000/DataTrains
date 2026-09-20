-- Keep the redaction tables reachable only by the dedicated backend role even
-- if an external migration runner registered 017 before the role migration.
GRANT SELECT, INSERT, UPDATE, DELETE ON redaction_plans, redaction_jobs
  TO datatrains_runtime;

-- Supabase provides these Data API roles, while local and CI PostgreSQL
-- installations may not. Revoke them when present without making the
-- production schema impossible to test on a stock PostgreSQL server.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'anon') THEN
    REVOKE ALL PRIVILEGES ON redaction_plans, redaction_jobs FROM anon;
  END IF;
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'authenticated') THEN
    REVOKE ALL PRIVILEGES ON redaction_plans, redaction_jobs FROM authenticated;
  END IF;
END
$$;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_policies
    WHERE schemaname='public'
      AND tablename='redaction_plans'
      AND policyname='datatrains_runtime_all'
  ) THEN
    CREATE POLICY datatrains_runtime_all ON redaction_plans
      FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
  END IF;

  IF NOT EXISTS (
    SELECT 1 FROM pg_policies
    WHERE schemaname='public'
      AND tablename='redaction_jobs'
      AND policyname='datatrains_runtime_all'
  ) THEN
    CREATE POLICY datatrains_runtime_all ON redaction_jobs
      FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
  END IF;
END
$$;

-- Trigger functions resolve database objects only through an explicit trusted
-- path. This prevents a caller-controlled schema from shadowing referenced
-- names while retaining the public.sessions lookup used by governed erasure.
ALTER FUNCTION public.prevent_audit_event_mutation()
  SET search_path = pg_catalog, public;
ALTER FUNCTION public.enforce_redaction_plan_immutability()
  SET search_path = pg_catalog, public;

-- PostgreSQL does not automatically index the referencing side of foreign
-- keys. Cover every advisor-reported foreign key used by joins or deletes.
CREATE INDEX IF NOT EXISTS artifact_multipart_uploads_session_id_idx
  ON artifact_multipart_uploads (session_id);
CREATE INDEX IF NOT EXISTS assignments_contributor_id_idx
  ON assignments (contributor_id);
CREATE INDEX IF NOT EXISTS consent_acceptances_document_id_idx
  ON consent_acceptances (document_id);
CREATE INDEX IF NOT EXISTS deletion_requests_organization_id_idx
  ON deletion_requests (organization_id);
CREATE INDEX IF NOT EXISTS invitations_accepted_by_idx
  ON invitations (accepted_by);
CREATE INDEX IF NOT EXISTS invitations_invited_by_idx
  ON invitations (invited_by);
CREATE INDEX IF NOT EXISTS invitations_organization_id_idx
  ON invitations (organization_id);
CREATE INDEX IF NOT EXISTS projects_organization_id_idx
  ON projects (organization_id);
CREATE INDEX IF NOT EXISTS retention_purge_requests_organization_id_idx
  ON retention_purge_requests (organization_id);
CREATE INDEX IF NOT EXISTS reviews_session_id_idx
  ON reviews (session_id);
CREATE INDEX IF NOT EXISTS sessions_assignment_id_idx
  ON sessions (assignment_id);
CREATE INDEX IF NOT EXISTS sessions_consent_acceptance_id_idx
  ON sessions (consent_acceptance_id);
CREATE INDEX IF NOT EXISTS task_templates_project_id_idx
  ON task_templates (project_id);
CREATE INDEX IF NOT EXISTS tasks_template_id_idx
  ON tasks (template_id);
