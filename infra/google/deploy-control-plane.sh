#!/usr/bin/env bash
set -euo pipefail

project_id="${DATATRAINS_GCP_PROJECT:-datatrains-production}"
region="${DATATRAINS_GCP_REGION:-asia-southeast1}"
image_digest="${DATATRAINS_IMAGE_DIGEST:-}"
runtime_file="${DATATRAINS_RUNTIME_ENV_FILE:-infra/google/runtime.production.yaml}"
api_service_account="${DATATRAINS_API_SERVICE_ACCOUNT:-datatrains-api@${project_id}.iam.gserviceaccount.com}"
worker_service_account="${DATATRAINS_WORKER_SERVICE_ACCOUNT:-datatrains-worker@${project_id}.iam.gserviceaccount.com}"
release_service_account="${DATATRAINS_RELEASE_SERVICE_ACCOUNT:-datatrains-release@${project_id}.iam.gserviceaccount.com}"
scheduler_service_account="${DATATRAINS_SCHEDULER_SERVICE_ACCOUNT:-datatrains-scheduler@${project_id}.iam.gserviceaccount.com}"
worker_schedule="${DATATRAINS_WORKER_SCHEDULE:-* * * * *}"
database_owner_secret="${DATATRAINS_DATABASE_OWNER_SECRET:-datatrains-database-owner-url}"
database_runtime_secret="${DATATRAINS_DATABASE_RUNTIME_SECRET:-datatrains-database-runtime-url}"
r2_access_secret="${DATATRAINS_R2_ACCESS_SECRET:-datatrains-r2-access-key-id}"
r2_secret_secret="${DATATRAINS_R2_SECRET_SECRET:-datatrains-r2-secret-access-key}"

expected_prefix="${region}-docker.pkg.dev/${project_id}/datatrains/datatrains-control-plane@sha256:"
digest_suffix="${image_digest#"$expected_prefix"}"
if [[ "$digest_suffix" == "$image_digest" || ! "$digest_suffix" =~ ^[0-9a-f]{64}$ ]]; then
  echo "DATATRAINS_IMAGE_DIGEST must be an immutable ${expected_prefix}<64 lowercase hex characters> reference." >&2
  exit 2
fi
if [[ ! -f "$runtime_file" ]]; then
  echo "Runtime environment file not found: $runtime_file" >&2
  echo "Copy infra/google/runtime.production.example.yaml, replace every placeholder, and keep the resulting file untracked." >&2
  exit 2
fi
if grep -Eq 'ACCOUNT_ID|YOUR_TENANT|\.example($|[^a-z])' "$runtime_file"; then
  echo "Runtime environment file still contains placeholder values: $runtime_file" >&2
  exit 2
fi
if [[ "$worker_schedule" == *$'\n'* || -z "$worker_schedule" ]]; then
  echo "DATATRAINS_WORKER_SCHEDULE must be a non-empty single-line cron schedule." >&2
  exit 2
fi

gcloud services enable run.googleapis.com cloudscheduler.googleapis.com iam.googleapis.com secretmanager.googleapis.com \
  --project "$project_id"

ensure_service_account() {
  local email="$1"
  local default_id="$2"
  local display_name="$3"
  if gcloud iam service-accounts describe "$email" --project "$project_id" >/dev/null 2>&1; then
    return
  fi
  if [[ "$email" != "${default_id}@${project_id}.iam.gserviceaccount.com" ]]; then
    echo "Custom service account does not exist: $email" >&2
    exit 2
  fi
  gcloud iam service-accounts create "$default_id" \
    --project "$project_id" \
    --display-name "$display_name"
}

ensure_service_account "$api_service_account" datatrains-api "DataTrains API runtime"
ensure_service_account "$worker_service_account" datatrains-worker "DataTrains worker runtime"
ensure_service_account "$release_service_account" datatrains-release "DataTrains release and migration jobs"
ensure_service_account "$scheduler_service_account" datatrains-scheduler "DataTrains worker scheduler"

for secret in "$database_owner_secret" "$database_runtime_secret" "$r2_access_secret" "$r2_secret_secret"; do
  if ! gcloud secrets describe "$secret" --project "$project_id" >/dev/null 2>&1; then
    echo "Required Secret Manager secret does not exist: $secret" >&2
    exit 2
  fi
done

grant_secret_access() {
  local secret="$1"
  local member="$2"
  gcloud secrets add-iam-policy-binding "$secret" \
    --project "$project_id" \
    --member "serviceAccount:${member}" \
    --role roles/secretmanager.secretAccessor >/dev/null
}

grant_secret_access "$database_owner_secret" "$release_service_account"
grant_secret_access "$database_runtime_secret" "$api_service_account"
grant_secret_access "$database_runtime_secret" "$worker_service_account"
for secret in "$r2_access_secret" "$r2_secret_secret"; do
  grant_secret_access "$secret" "$api_service_account"
  grant_secret_access "$secret" "$worker_service_account"
done

gcloud run jobs deploy datatrains-migrate \
  --project "$project_id" \
  --region "$region" \
  --image "$image_digest" \
  --command /app/trajectory-migrate \
  --service-account "$release_service_account" \
  --set-env-vars TRAJECTORY_ENVIRONMENT=production,TRAJECTORY_MIGRATIONS_DIR=/app/migrations \
  --set-secrets "TRAJECTORY_DATABASE_URL=${database_owner_secret}:latest" \
  --tasks 1 \
  --max-retries 0 \
  --task-timeout 600s

gcloud run jobs execute datatrains-migrate \
  --project "$project_id" \
  --region "$region" \
  --wait

gcloud run deploy datatrains-api \
  --project "$project_id" \
  --region "$region" \
  --image "$image_digest" \
  --service-account "$api_service_account" \
  --env-vars-file "$runtime_file" \
  --set-secrets "TRAJECTORY_DATABASE_URL=${database_runtime_secret}:latest,TRAJECTORY_R2_ACCESS_KEY_ID=${r2_access_secret}:latest,TRAJECTORY_R2_SECRET_ACCESS_KEY=${r2_secret_secret}:latest" \
  --cpu 1 \
  --memory 512Mi \
  --concurrency 20 \
  --min-instances 0 \
  --max-instances 3 \
  --timeout 900s \
  --allow-unauthenticated

gcloud run jobs deploy datatrains-worker \
  --project "$project_id" \
  --region "$region" \
  --image "$image_digest" \
  --command /app/trajectory-worker \
  --service-account "$worker_service_account" \
  --env-vars-file "$runtime_file" \
  --set-secrets "TRAJECTORY_DATABASE_URL=${database_runtime_secret}:latest,TRAJECTORY_R2_ACCESS_KEY_ID=${r2_access_secret}:latest,TRAJECTORY_R2_SECRET_ACCESS_KEY=${r2_secret_secret}:latest" \
  --cpu 1 \
  --memory 1Gi \
  --tasks 1 \
  --max-retries 0 \
  --task-timeout 3600s

gcloud run jobs add-iam-policy-binding datatrains-worker \
  --project "$project_id" \
  --region "$region" \
  --member "serviceAccount:${scheduler_service_account}" \
  --role roles/run.invoker

# Execute once during the release so a bad worker configuration fails before
# the schedule is installed or updated.
gcloud run jobs execute datatrains-worker \
  --project "$project_id" \
  --region "$region" \
  --wait

scheduler_uri="https://run.googleapis.com/v2/projects/${project_id}/locations/${region}/jobs/datatrains-worker:run"
if gcloud scheduler jobs describe datatrains-worker-every-minute \
  --project "$project_id" --location "$region" >/dev/null 2>&1; then
  gcloud scheduler jobs update http datatrains-worker-every-minute \
    --project "$project_id" \
    --location "$region" \
    --schedule "$worker_schedule" \
    --time-zone Etc/UTC \
    --uri "$scheduler_uri" \
    --http-method POST \
    --oauth-service-account-email "$scheduler_service_account" \
    --attempt-deadline 60s
else
  gcloud scheduler jobs create http datatrains-worker-every-minute \
    --project "$project_id" \
    --location "$region" \
    --schedule "$worker_schedule" \
    --time-zone Etc/UTC \
    --uri "$scheduler_uri" \
    --http-method POST \
    --oauth-service-account-email "$scheduler_service_account" \
    --attempt-deadline 60s
fi

echo "Deployed migrations, datatrains-api, datatrains-worker, and its scheduler from $image_digest"
