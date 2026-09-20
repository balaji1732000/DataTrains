# Cloud Build and Google runtime foundation — 2026-09-14

Project: `datatrains-production` (`963738422674`)  
Registry: `asia-southeast1-docker.pkg.dev/datatrains-production/datatrains`

## Immutable control-plane image

- Cloud Build ID: `1efab0a2-6f62-4ab0-96c6-6d3a6d08aafe`
- Result: `SUCCESS`
- Build configuration: `infra/google/cloudbuild.yaml`
- Immutable image:
  `asia-southeast1-docker.pkg.dev/datatrains-production/datatrains/datatrains-control-plane@sha256:b42683352a9ec9766eea3472eda1ab7fcd934b71887ad378581138105618b2d7`
- Console evidence:
  `https://console.cloud.google.com/cloud-build/builds/1efab0a2-6f62-4ab0-96c6-6d3a6d08aafe?project=963738422674`

The build ran the Go test suite before image construction, verified the bundled
`ffprobe` runtime tool, and pushed a unique build-ID tag to the immutable-tag
Artifact Registry repository.

## Identities

The following keyless service accounts exist:

- `datatrains-build` — build-only identity
- `datatrains-api` — Cloud Run API runtime
- `datatrains-worker` — validation/redaction worker runtime
- `datatrains-release` — migration and release jobs
- `datatrains-scheduler` — invokes only the worker job

The build identity can read the Cloud Build source bucket, write the DataTrains
Artifact Registry repository, and write build logs. Runtime identities do not
inherit the build identity.

## Managed-secret inventory

The following Secret Manager containers exist with no secret values recorded in
this evidence file:

- `datatrains-database-owner-url`
- `datatrains-database-runtime-url`
- `datatrains-r2-access-key-id`
- `datatrains-r2-secret-access-key`

The release identity can access only the database owner URL. The API and worker
identities can access only the runtime database URL and R2 credentials.

## Remaining Cloud Run evidence

- Add production secret versions from approved Supabase and R2 resources.
- Deploy and verify the migration job, API service and drain-mode worker job.
- Configure the scheduler and alerts.
- Exercise health, authentication, authorization, uploads, processing and
  rollback against the live staging environment.
