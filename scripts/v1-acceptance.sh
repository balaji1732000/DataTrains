#!/usr/bin/env bash
set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
runtime_dir="$(mktemp -d "${TMPDIR:-/tmp}/trajectory-v1-acceptance.XXXXXX")"
postgres_started_here=false
api_pid=""
acceptance_port="${TRAJECTORY_ACCEPTANCE_API_PORT:-18080}"
acceptance_database="trajectory_acceptance"
acceptance_database_url="postgres://trajectory@127.0.0.1:55432/${acceptance_database}?sslmode=disable"

cleanup() {
  if [[ "$api_pid" =~ ^[0-9]+$ ]] && kill -0 "$api_pid" >/dev/null 2>&1; then
    kill "$api_pid" >/dev/null 2>&1 || true
    wait "$api_pid" >/dev/null 2>&1 || true
  fi
  if [[ "$postgres_started_here" == true ]]; then
    "$repository_root/scripts/local-postgres.sh" stop >/dev/null
  fi
  rm -rf -- "$runtime_dir"
}
trap cleanup EXIT INT TERM

cd "$repository_root"

if ! scripts/local-postgres.sh status >/dev/null 2>&1; then
  npm run postgres:start
  postgres_started_here=true
fi

if ! scripts/local-postgres.sh psql -Atqc "SELECT 1 FROM pg_database WHERE datname='${acceptance_database}'" | grep -qx 1; then
  scripts/local-postgres.sh psql -v ON_ERROR_STOP=1 -c "CREATE DATABASE ${acceptance_database}"
fi

npm run check
npm test
npm run build
npm run readiness:production
npm run go:test
npm run go:build
TEST_DATABASE_URL="$acceptance_database_url" scripts/run-go.sh test -p 1 ./...
npm run collector:core:check
npm run collector:core:test
npm run collector:tauri:check
npm run collector:tauri:test-compile
npm run collector:tauri:build

scripts/run-go.sh build -o "$runtime_dir/trajectory-api" ./cmd/api
TRAJECTORY_API_ADDRESS="127.0.0.1:${acceptance_port}" \
TRAJECTORY_DATABASE_URL="$acceptance_database_url" \
TRAJECTORY_MIGRATIONS_DIR="$repository_root/services/api/migrations" \
TRAJECTORY_SCHEMA_PATH="$repository_root/packages/trajectory-schema/schemas/trajectory-v1.json" \
TRAJECTORY_BLOB_ROOT="$runtime_dir/blobstore" \
  "$runtime_dir/trajectory-api" >"$runtime_dir/api.log" 2>&1 &
api_pid="$!"

for _ in {1..50}; do
  if curl --fail --silent "http://127.0.0.1:${acceptance_port}/healthz" >"$runtime_dir/health.json"; then
    break
  fi
  if ! kill -0 "$api_pid" >/dev/null 2>&1; then
    cat "$runtime_dir/api.log" >&2
    exit 1
  fi
  sleep 0.1
done

if ! grep -q '"status":"ok"' "$runtime_dir/health.json"; then
  cat "$runtime_dir/api.log" >&2
  echo "Trajectory API health check did not become ready." >&2
  exit 1
fi

echo "V1 AUTOMATED ACCEPTANCE PASSED"
echo "This proves repository-local acceptance only. Production promotion remains governed by docs/operations/production-readiness.json."
