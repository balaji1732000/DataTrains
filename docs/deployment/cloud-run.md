# Cloud Run control-plane deployment

The Go API, worker, and migration command are compiled into one immutable
container image. The API is the default process. Cloud Run deployments override
the command for migrations or worker execution so every component uses the
same code, schema, and migration version.

This document is an operational contract. It does not authorize a production
deployment before the OpenID, R2, database, backup, and acceptance gates in
[`PRODUCTION_V1.md`](../PRODUCTION_V1.md) pass.

## Image contents

```text
/app/trajectory-api       HTTP control plane (default)
/app/trajectory-worker    asynchronous validation/derivation worker
/app/trajectory-migrate   single-purpose database migration command
/app/trajectory-bootstrap-admin  idempotent first-organization administrator bootstrap
/app/migrations           versioned PostgreSQL migrations
/app/schema               canonical trajectory schema
```

Build from the repository root:

```bash
docker build --tag datatrains-control-plane:local .
```

## Runtime configuration

| Variable | API | Worker | Meaning |
| --- | --- | --- | --- |
| `TRAJECTORY_ENVIRONMENT` | required in cloud | required in cloud | `staging` or `production` |
| `TRAJECTORY_DATABASE_URL` | required | required | TLS Supabase runtime-role pooler URL, supplied as a secret |
| `TRAJECTORY_DATABASE_MAX_CONNS` | recommended | recommended | per-container database connection ceiling; begin with `4` |
| `TRAJECTORY_GCP_PROJECT` | recommended | unused | project ID used to correlate structured request logs with Cloud Trace |
| `TRAJECTORY_RUN_MIGRATIONS` | `false` | `false` | migrations run as a separate release step |
| `TRAJECTORY_BLOB_BACKEND` | required | required | `r2` in production; local storage is rejected in production |
| `TRAJECTORY_FFPROBE_PATH` | unused | optional | Defaults to bundled `ffprobe`; validates submitted and redacted media |
| `TRAJECTORY_FFMPEG_PATH` | unused | optional | Defaults to bundled `ffmpeg`; applies approved irreversible video masks |
| `TRAJECTORY_R2_ENDPOINT` | required for R2 | required for R2 | account S3 endpoint such as `https://ACCOUNT_ID.r2.cloudflarestorage.com` |
| `TRAJECTORY_R2_BUCKET` | required for R2 | required for R2 | private artifact bucket name |
| `TRAJECTORY_R2_ACCESS_KEY_ID` | secret | secret | least-privilege R2 S3 access key ID |
| `TRAJECTORY_R2_SECRET_ACCESS_KEY` | secret | secret | least-privilege R2 S3 secret key |
| `TRAJECTORY_AUTH_MODE` | required | unused | `oidc`; production refuses local header authentication |
| `TRAJECTORY_OIDC_ISSUER` | required | unused | exact HTTPS OpenID issuer used during discovery and token verification |
| `TRAJECTORY_OIDC_AUDIENCE` | required | unused | API audience required in every bearer token |
| `TRAJECTORY_UPLOAD_AUTHORIZATION_TTL` | optional | unused | lifetime of an object-scoped R2 PUT URL; default `15m`, maximum `1h` |
| `TRAJECTORY_ALLOWED_ORIGINS` | required | unused | comma-separated exact web/Tauri origins; wildcard is rejected |
| `TRAJECTORY_WORKER_DRAIN` | unused | optional | exit successfully when the durable queue is empty |
| `PORT` | provided by Cloud Run | unused | API listen port; converted to `0.0.0.0:PORT` |

Secret values must be sourced from managed secret storage and must not appear
in Cloud Build substitutions, client bundles, logs, shell history, or Git.
The R2 adapter uses path-style S3 requests, validates object keys, and applies
an atomic `If-None-Match: *` condition to every `raw/` write. A pre-existing raw
key is treated as immutable rather than overwritten.

The API discovers the configured OpenID provider at startup, verifies token
signature, issuer, audience, and expiry, and then resolves issuer plus subject
to an invited internal account. `X-Actor-ID` is accepted only in local mode;
it cannot choose an identity or role in production.

The API and worker database URL must use a dedicated login that inherits only
the `datatrains_runtime` group role. The migration/bootstrap jobs use a separate
owner URL. See [`supabase.md`](supabase.md) for RLS, pooler, backup, and restore
requirements.

The deployment helper expects `datatrains-database-owner-url` for migrations and
`datatrains-database-runtime-url` for the API and worker. Override their names
with `DATATRAINS_DATABASE_OWNER_SECRET` and
`DATATRAINS_DATABASE_RUNTIME_SECRET`; never reuse the owner secret for runtime.

Collectors start or resume uploads through
`POST /v1/sessions/{session_id}/multipart-uploads`. The API returns a 16 MiB
part plan and issues a short-lived PUT URL for one exact part at a time. It
persists returned part ETags in PostgreSQL, so a restarted Collector requests
only missing parts. Completion assembles an untrusted `staging/multipart/`
object; the API re-reads it, verifies the declared byte size and full SHA-256,
and only then promotes it to an immutable, create-only `raw/` key and registers
the artifact. See [`r2.md`](r2.md) for the storage contract and bucket controls.

## Build

`infra/google/cloudbuild.yaml` builds and publishes an image tagged only with
Cloud Build's unique build ID. The checked-in defaults target the production
registry created for this project:

```text
_LOCATION=asia-southeast1
_REPOSITORY=datatrains
```

Release deployments must use the resulting image digest. The build ID tag is
unique and useful for traceability, while the digest is the promotion identity.
The production registry has immutable tags enabled and intentionally does not
use `latest`.

After a digest is tested, copy
`infra/google/runtime.production.example.yaml` to the ignored
`infra/google/runtime.production.yaml`, replace every placeholder, and run
`infra/google/deploy-control-plane.sh` with `DATATRAINS_IMAGE_DIGEST` set to the
full Artifact Registry `@sha256:` reference. The release helper refuses mutable
tags and unresolved environment placeholders. It runs the migration job first,
then deploys the API and the drain-mode worker job from the same digest. It
references Secret Manager versions; it never accepts secret values as command
arguments.

## Safe release order

1. Build the image and run repository tests.
2. Run `/app/trajectory-migrate` as an authenticated one-off job.
3. Verify the migration job completed successfully.
4. Run `/app/trajectory-bootstrap-admin` once with the first administrator's
   OpenID issuer/subject and organization details supplied as job secrets.
5. Deploy a staging API revision using the immutable image digest.
6. Verify `/livez`, `/readyz`, unauthenticated rejection, `/v1/me`, and role
   enforcement.
7. Deploy or trigger the worker with the same image digest.
8. Run the staging end-to-end acceptance flow.
9. Promote the already-tested digest; do not rebuild during promotion.

API and worker startup do not run migrations outside local/test environments by
default. Migration execution takes a PostgreSQL transaction-level advisory lock
for each migration so concurrent release attempts serialize safely.

The worker image contains `ffprobe`. Every declared video segment is streamed
through it without creating a full in-memory copy. Validation requires an MP4/
MOV container, one H.264 video stream, positive dimensions and duration, and a
duration reasonably consistent with the capture manifest. Probe time and output
are bounded; corrupt or unsupported media fails validation rather than reaching
review.

The same image contains `ffmpeg` for optional reviewed-video releases. The
redaction worker applies only immutable reviewer-approved time/rectangle masks,
strips audio, re-encodes H.264 MP4, records a hashed output manifest, and writes
under a plan-specific derived prefix. See
[`video-redaction.md`](video-redaction.md).

The V1 worker is a drain-mode Cloud Run Job rather than an always-on Cloud Run
Service. The deployment helper creates or updates a one-minute Cloud Scheduler
trigger using the dedicated `datatrains-scheduler` identity, grants that
identity only Cloud Run Invoker on `datatrains-worker`, and executes the worker
once before enabling the schedule. Do not use the API or worker runtime
identities to administer the schedule. The worker leases rows atomically,
handles all available jobs, and exits when the durable queue is empty;
overlapping runs are safe but should be monitored.

After three failed attempts a validation or redaction job enters its
PostgreSQL-backed dead-letter queue. Validation failure changes its session to
`FAILED`; redaction failure keeps the accepted trajectory usable for a
trajectory-only release but blocks any redacted-video release. The administrator
dashboard lists only dead-letter jobs belonging to the administrator's
organizations. A manual retry is audited, clears terminal timestamps, resets
the attempt counter, and makes the job available to a worker. Alert on any
dead-letter job and on the age of the oldest queued job.

## Initial resource bounds

Start the API with request-based billing, zero minimum instances, three maximum
instances, one CPU, 512 MiB RAM, and four database connections per instance.
Adjust only from measured latency, saturation, and connection evidence.

Do not route video or event bodies through the Cloud Run API in production.
Collectors upload directly to R2 using short-lived, session-scoped multipart
authorization issued by the API.

## Logging and request correlation

The API emits one JSON access record per request with `request_id`, W3C/Cloud
Trace correlation, HTTP method, path, status, and duration. It validates and
preserves an incoming `traceparent` or `X-Cloud-Trace-Context`, returns a W3C
`Traceparent` response header, and adds Cloud Logging's special trace field when
`TRAJECTORY_GCP_PROJECT` is configured. It never logs query strings,
authorization headers, request bodies, presigned URLs, or artifact bytes. A
valid caller-supplied `X-Request-ID` is preserved; otherwise the API creates one
and returns it in the response. The worker logs aggregate queue snapshots at
start and drain completion; they contain counts and oldest-ready age but no
organization, contributor, task, or object identifiers.

Run `infra/google/configure-observability.sh` after the first deployment with
`DATATRAINS_NOTIFICATION_CHANNELS` set to the approved Cloud Monitoring channel
resource names. It idempotently configures API 5xx, failed worker execution,
dead-letter, and stale-queue alerts from built-in and log-based metrics. The
operator must separately configure the Supabase database saturation/backup
alerts and a public `/readyz` uptime check after the final API domain exists.
Alert destinations, escalation owners, and an incident exercise remain
production-approval gates.
