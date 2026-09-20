#!/usr/bin/env bash
set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
project_id="${DATATRAINS_GCP_PROJECT:-datatrains-production}"
notification_channels="${DATATRAINS_NOTIFICATION_CHANNELS:-}"

if [[ -z "$notification_channels" || "$notification_channels" == *$'\n'* ]]; then
  echo "DATATRAINS_NOTIFICATION_CHANNELS is required." >&2
  echo "Use one or more Cloud Monitoring notification-channel resource names, separated by commas." >&2
  exit 2
fi

gcloud services enable logging.googleapis.com monitoring.googleapis.com \
  --project "$project_id"

upsert_log_metric() {
  local name="$1"
  local description="$2"
  local filter="$3"
  if gcloud logging metrics describe "$name" --project "$project_id" >/dev/null 2>&1; then
    gcloud logging metrics update "$name" \
      --project "$project_id" \
      --description "$description" \
      --log-filter "$filter"
  else
    gcloud logging metrics create "$name" \
      --project "$project_id" \
      --description "$description" \
      --log-filter "$filter"
  fi
}

upsert_log_metric \
  datatrains_dead_letter_snapshot \
  "Worker snapshots where any durable dead-letter queue is non-empty." \
  'resource.type="cloud_run_job" AND resource.labels.job_name="datatrains-worker" AND jsonPayload.msg="worker_queue_snapshot" AND (jsonPayload.processing_dead_letter>0 OR jsonPayload.redaction_dead_letter>0 OR jsonPayload.deletion_dead_letter>0 OR jsonPayload.retention_dead_letter>0)'

upsert_log_metric \
  datatrains_stale_queue_snapshot \
  "Worker snapshots where the oldest ready operation is over five minutes old." \
  'resource.type="cloud_run_job" AND resource.labels.job_name="datatrains-worker" AND jsonPayload.msg="worker_queue_snapshot" AND jsonPayload.oldest_ready_age_seconds>300'

upsert_alert_policy() {
  local display_name="$1"
  local policy_file="$2"
  local policy_name
  policy_name="$(gcloud monitoring policies list \
    --project "$project_id" \
    --filter "displayName=\"${display_name}\"" \
    --format 'value(name)' \
    --limit 1)"
  if [[ -n "$policy_name" ]]; then
    gcloud monitoring policies update "$policy_name" \
      --project "$project_id" \
      --policy-from-file "$policy_file" \
      --notification-channels "$notification_channels"
  else
    gcloud monitoring policies create \
      --project "$project_id" \
      --policy-from-file "$policy_file" \
      --notification-channels "$notification_channels"
  fi
}

monitoring_dir="$repository_root/infra/google/monitoring"
upsert_alert_policy "DataTrains API 5xx responses" "$monitoring_dir/api-5xx.json"
upsert_alert_policy "DataTrains worker execution failed" "$monitoring_dir/worker-failed.json"
upsert_alert_policy "DataTrains dead-letter queue is non-empty" "$monitoring_dir/dead-letter.json"
upsert_alert_policy "DataTrains queue is stale" "$monitoring_dir/stale-queue.json"

echo "DataTrains Cloud Monitoring metrics and alert policies are configured."
