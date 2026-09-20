#!/usr/bin/env bash
set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
database_url="${DATATRAINS_RESTORE_DATABASE_URL:-}"
restore_environment="${DATATRAINS_RESTORE_ENVIRONMENT:-staging}"
confirmation="${DATATRAINS_CONFIRM_ISOLATED_RESTORE:-}"
expected_inventory="${DATATRAINS_EXPECTED_INVENTORY:-}"
runtime_dir="$(mktemp -d "${TMPDIR:-/tmp}/datatrains-restore-check.XXXXXX")"

cleanup() {
  rm -rf -- "$runtime_dir"
}
trap cleanup EXIT INT TERM

if [[ -z "$database_url" ]]; then
  echo "DATATRAINS_RESTORE_DATABASE_URL is required." >&2
  exit 2
fi
if [[ "$restore_environment" != "local" && "$restore_environment" != "staging" ]]; then
  echo "DATATRAINS_RESTORE_ENVIRONMENT must be local or staging; production targets are refused." >&2
  exit 2
fi
if [[ "$confirmation" != "I_UNDERSTAND_THIS_IS_AN_ISOLATED_RESTORE" ]]; then
  echo "Restore verification runs migrations on its target." >&2
  echo "Set DATATRAINS_CONFIRM_ISOLATED_RESTORE=I_UNDERSTAND_THIS_IS_AN_ISOLATED_RESTORE only for an isolated restore project." >&2
  exit 2
fi
if [[ -n "$expected_inventory" && ! -f "$expected_inventory" ]]; then
  echo "Expected inventory file is unavailable: $expected_inventory" >&2
  exit 2
fi

if command -v psql >/dev/null 2>&1; then
  psql_bin="$(command -v psql)"
elif [[ -x "$repository_root/.tools/postgres/usr/lib/postgresql/16/bin/psql" ]]; then
  psql_bin="$repository_root/.tools/postgres/usr/lib/postgresql/16/bin/psql"
else
  echo "psql is required to verify a restore." >&2
  exit 127
fi

cd "$repository_root"
TRAJECTORY_ENVIRONMENT="$restore_environment" \
TRAJECTORY_DATABASE_URL="$database_url" \
TRAJECTORY_DATABASE_MAX_CONNS=1 \
TRAJECTORY_MIGRATIONS_DIR="$repository_root/services/api/migrations" \
  scripts/run-go.sh run ./cmd/migrate

"$psql_bin" -d "$database_url" -X -v ON_ERROR_STOP=1 <<'SQL'
DO $$
DECLARE invalid_count bigint;
BEGIN
  SELECT count(*) INTO invalid_count
  FROM schema_migrations
  WHERE checksum_sha256 IS NULL OR checksum_sha256 !~ '^[0-9a-f]{64}$';
  IF invalid_count <> 0 THEN
    RAISE EXCEPTION '% migration records have invalid checksums', invalid_count;
  END IF;

  SELECT count(*) INTO invalid_count
  FROM pg_class c
  JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname='public' AND c.relkind='r'
    AND c.relname <> 'schema_migrations' AND NOT c.relrowsecurity;
  IF invalid_count <> 0 THEN
    RAISE EXCEPTION '% application tables do not have row-level security', invalid_count;
  END IF;

  SELECT count(*) INTO invalid_count
  FROM pg_constraint
  WHERE contype='f' AND NOT convalidated;
  IF invalid_count <> 0 THEN
    RAISE EXCEPTION '% foreign-key constraints are not validated', invalid_count;
  END IF;

  SELECT count(*) INTO invalid_count
  FROM dataset_releases release
  WHERE jsonb_array_length(release.source_session_ids) <>
    (SELECT count(*) FROM release_sessions member WHERE member.release_id=release.id);
  IF invalid_count <> 0 THEN
    RAISE EXCEPTION '% releases have inconsistent membership inventory', invalid_count;
  END IF;

  SELECT count(*) INTO invalid_count
  FROM dataset_releases release
  WHERE jsonb_typeof(release.object_keys)<>'array'
     OR jsonb_array_length(release.object_keys)<7
     OR NOT release.object_keys ? ('bundles/' || release.id || '.zip')
     OR NOT release.object_keys ? ('releases/' || release.id || '/manifest.json');
  IF invalid_count <> 0 THEN
    RAISE EXCEPTION '% releases have incomplete object inventories', invalid_count;
  END IF;

  SELECT count(*) INTO invalid_count
  FROM redaction_jobs job
  JOIN redaction_plans plan ON plan.id=job.plan_id
  WHERE job.state='completed' AND job.purged_at IS NULL AND (
    job.output_manifest_key IS NULL
    OR job.output_manifest_key <> 'derived/sessions/' || job.session_id || '/redactions/' || plan.id || '/manifest.json'
  );
  IF invalid_count <> 0 THEN
    RAISE EXCEPTION '% completed redaction jobs have invalid output manifests', invalid_count;
  END IF;

  IF NOT EXISTS (
    SELECT 1 FROM pg_trigger
    WHERE tgrelid='redaction_plans'::regclass
      AND tgname='redaction_plans_immutable' AND tgenabled<>'D'
  ) THEN
    RAISE EXCEPTION 'redaction-plan immutability trigger is missing or disabled';
  END IF;

  SELECT count(*) INTO invalid_count
  FROM deletion_requests request
  JOIN sessions session ON session.id=request.session_id
  WHERE request.state='completed' AND session.state<>'DELETED';
  IF invalid_count <> 0 THEN
    RAISE EXCEPTION '% completed deletion requests lack a deleted session tombstone', invalid_count;
  END IF;

  SELECT count(*) INTO invalid_count
  FROM retention_purge_requests request
  WHERE request.state='completed' AND (
    (request.resource_type='session_raw' AND EXISTS (
      SELECT 1 FROM artifacts artifact
      WHERE artifact.session_id=request.resource_id AND artifact.purged_at IS NULL
    ))
    OR (request.resource_type='session_derived' AND EXISTS (
      SELECT 1 FROM validation_results result
      WHERE result.session_id=request.resource_id AND result.purged_at IS NULL
    ))
    OR (request.resource_type='session_derived' AND EXISTS (
      SELECT 1 FROM redaction_jobs job
      WHERE job.session_id=request.resource_id AND job.state='completed' AND job.purged_at IS NULL
    ))
    OR (request.resource_type='release' AND EXISTS (
      SELECT 1 FROM dataset_releases release
      WHERE release.id=request.resource_id AND release.purged_at IS NULL
    ))
  );
  IF invalid_count <> 0 THEN
    RAISE EXCEPTION '% completed retention purges lack metadata tombstones', invalid_count;
  END IF;

  IF NOT EXISTS (
    SELECT 1 FROM pg_trigger
    WHERE tgrelid='audit_events'::regclass
      AND tgname='audit_events_append_only' AND tgenabled<>'D'
  ) THEN
    RAISE EXCEPTION 'append-only audit trigger is missing or disabled';
  END IF;
END
$$;

BEGIN;
INSERT INTO audit_events (actor, action, resource, occurred_at, request_id, metadata)
VALUES ('restore-verifier', 'restore.synthetic', 'restore/synthetic', now(), 'restore-verifier', '{}');
DO $$
DECLARE mutation_blocked boolean := false;
BEGIN
  BEGIN
    UPDATE audit_events SET actor='mutated' WHERE request_id='restore-verifier';
  EXCEPTION WHEN OTHERS THEN
    IF SQLERRM = 'audit_events is append-only' THEN
      mutation_blocked := true;
    ELSE
      RAISE;
    END IF;
  END;
  IF NOT mutation_blocked THEN
    RAISE EXCEPTION 'audit event mutation was not blocked';
  END IF;
END
$$;
ROLLBACK;
SQL

DATATRAINS_DATABASE_URL="$database_url" scripts/database-inventory.sh >"$runtime_dir/actual.inventory"
if [[ -n "$expected_inventory" ]]; then
  if ! diff -u -- "$expected_inventory" "$runtime_dir/actual.inventory"; then
    echo "Restored row counts or migration checksums differ from the pre-backup inventory." >&2
    exit 1
  fi
fi

cat "$runtime_dir/actual.inventory"
echo "DATATRAINS DATABASE RESTORE VERIFICATION PASSED"
