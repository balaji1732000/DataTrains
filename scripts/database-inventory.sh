#!/usr/bin/env bash
set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
database_url="${DATATRAINS_DATABASE_URL:-}"

if [[ -z "$database_url" ]]; then
  echo "DATATRAINS_DATABASE_URL is required." >&2
  exit 2
fi

if command -v psql >/dev/null 2>&1; then
  psql_bin="$(command -v psql)"
elif [[ -x "$repository_root/.tools/postgres/usr/lib/postgresql/16/bin/psql" ]]; then
  psql_bin="$repository_root/.tools/postgres/usr/lib/postgresql/16/bin/psql"
else
  echo "psql is required to inventory a database." >&2
  exit 127
fi

"$psql_bin" -d "$database_url" -X -v ON_ERROR_STOP=1 -At -F $'\t' <<'SQL'
SELECT 'migration', name, checksum_sha256
FROM schema_migrations
ORDER BY name;

SELECT 'table', table_name, row_count::text
FROM (
  SELECT 'accounts' table_name, count(*) row_count FROM accounts
  UNION ALL SELECT 'artifact_multipart_parts', count(*) FROM artifact_multipart_parts
  UNION ALL SELECT 'artifact_multipart_uploads', count(*) FROM artifact_multipart_uploads
  UNION ALL SELECT 'artifacts', count(*) FROM artifacts
  UNION ALL SELECT 'assignments', count(*) FROM assignments
  UNION ALL SELECT 'audit_events', count(*) FROM audit_events
  UNION ALL SELECT 'consent_acceptances', count(*) FROM consent_acceptances
  UNION ALL SELECT 'consent_documents', count(*) FROM consent_documents
  UNION ALL SELECT 'contributors', count(*) FROM contributors
  UNION ALL SELECT 'dataset_releases', count(*) FROM dataset_releases
  UNION ALL SELECT 'deletion_requests', count(*) FROM deletion_requests
  UNION ALL SELECT 'invitations', count(*) FROM invitations
  UNION ALL SELECT 'legal_holds', count(*) FROM legal_holds
  UNION ALL SELECT 'oidc_identities', count(*) FROM oidc_identities
  UNION ALL SELECT 'organization_memberships', count(*) FROM organization_memberships
  UNION ALL SELECT 'organizations', count(*) FROM organizations
  UNION ALL SELECT 'processing_jobs', count(*) FROM processing_jobs
  UNION ALL SELECT 'projects', count(*) FROM projects
  UNION ALL SELECT 'redaction_jobs', count(*) FROM redaction_jobs
  UNION ALL SELECT 'redaction_plans', count(*) FROM redaction_plans
  UNION ALL SELECT 'release_sessions', count(*) FROM release_sessions
  UNION ALL SELECT 'retention_policies', count(*) FROM retention_policies
  UNION ALL SELECT 'retention_purge_requests', count(*) FROM retention_purge_requests
  UNION ALL SELECT 'review_claims', count(*) FROM review_claims
  UNION ALL SELECT 'reviews', count(*) FROM reviews
  UNION ALL SELECT 'sessions', count(*) FROM sessions
  UNION ALL SELECT 'task_templates', count(*) FROM task_templates
  UNION ALL SELECT 'tasks', count(*) FROM tasks
  UNION ALL SELECT 'validation_results', count(*) FROM validation_results
) inventory
ORDER BY table_name;
SQL
