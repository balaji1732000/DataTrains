-- The API and worker connect through a dedicated login that is a member of this
-- NOLOGIN group role. Supabase's postgres owner remains reserved for migrations.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'datatrains_runtime') THEN
    CREATE ROLE datatrains_runtime NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE
      NOINHERIT NOREPLICATION NOBYPASSRLS;
  END IF;
END
$$;

REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO datatrains_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO datatrains_runtime;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO datatrains_runtime;
REVOKE UPDATE, DELETE ON audit_events FROM datatrains_runtime;

ALTER DEFAULT PRIVILEGES IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO datatrains_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
  GRANT USAGE, SELECT ON SEQUENCES TO datatrains_runtime;

CREATE POLICY datatrains_runtime_all ON organizations FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON projects FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON task_templates FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON tasks FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON contributors FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON assignments FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON consent_documents FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON consent_acceptances FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON sessions FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON artifacts FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON processing_jobs FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON validation_results FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON review_claims FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON reviews FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON dataset_releases FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON release_sessions FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON audit_events FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON accounts FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON oidc_identities FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON organization_memberships FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON invitations FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON artifact_multipart_uploads FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
CREATE POLICY datatrains_runtime_all ON artifact_multipart_parts FOR ALL TO datatrains_runtime USING (true) WITH CHECK (true);
