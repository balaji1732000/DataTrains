# Trajectory Platform

Local-first V1 for creating, recording, reviewing, and exporting consented computer-use trajectories.

## Repository layout

- `apps/admin`: Next.js administrator and reviewer dashboard
- `apps/collector`: React UI and Tauri desktop shell
- `services/api`: Go control-plane API
- `services/worker`: asynchronous Go processing worker
- `packages/trajectory-schema`: canonical versioned trajectory schema
- `tools/dataset-validator`: standalone trajectory/release validation CLI
- `storage/local`: runtime local blob storage (ignored by Git)
- `infra/docker`: local PostgreSQL
- `docs`: architecture, ADRs, security, and privacy decisions

## Development

```bash
npm install
npm run check
npm test
npm run build
npm run admin:dev
npm run collector:core:check
npm run collector:core:test
npm run collector:tauri:check
npm run worker:run
```

Run the complete automated V1 acceptance suite in an isolated local test database:

```bash
npm run acceptance:v1
```

Validate a trajectory:

```bash
npm run validate:trajectory -- packages/trajectory-schema/fixtures/valid-trajectory.json
```

Validate a materialized release directory (for the local adapter this is under `data/blobstore/releases/<release-id>`):

```bash
npm run validate:release -- data/blobstore/releases/<release-id>
```

Start PostgreSQL (requires Docker):

```bash
npm run infra:up
```

For the repository-local PostgreSQL 16 development runtime used in this workspace:

```bash
npm run postgres:start
npm run go:test:integration
TRAJECTORY_API_ADDRESS=127.0.0.1:8080 scripts/run-go.sh run ./cmd/api
```

In local mode the API applies unapplied SQL migrations at startup. Mutating
requests require `X-Actor-ID`; callers may supply `X-Request-ID`, otherwise the
API creates one. Production uses verified OpenID bearer tokens and resolves all
account, role, contributor, and organization access server-side; see
[production identity](docs/deployment/identity.md).

During `UPLOADING`, raw session artifacts are accepted by the hash-gated artifact endpoint. Submission verifies the immutable blobs and atomically creates a durable validation job. The worker leases jobs, validates the canonical schema and artifact references, derives reviewer-friendly actions and writes normalized JSON/JSONL, then advances valid sessions to `READY_FOR_REVIEW`.

The admin review console loads the live queue, takes exclusive renewable claims, streams MP4 evidence with byte ranges, synchronizes events to playback, and enforces a passed PII review before acceptance. Accepted sessions can be exported through the release API; release IDs, ordering, `README.md`, embedded schema, checksum inventory, manifests, per-trajectory JSON, and JSONL are deterministic for a fixed configuration.

The collector core can be tested independently of the desktop webview. It enforces preflight and exact versioned consent, writes recoverable session state, records monotonic NDJSON events only in allowed application contexts, controls FFmpeg H.264 segments, hashes artifacts, and creates `capture/v1` manifests. The Windows desktop layer uses native low-level input hooks plus a foreground-context watcher, stores virtual-key codes rather than typed text, automatically pauses outside the task allowlist, and fails closed if hooks cannot be installed. Sealed captures remain discoverable for upload retry until a submission receipt is durably stored.

The PostgreSQL integration suite includes a repeatable 100-trajectory synthetic run through release creation. A physical Windows capture/replay soak is still required before declaring the desktop recording path production-ready.

On Windows, `npm run collector:package:windows` creates the NSIS installer. See [Windows V1 acceptance](docs/windows-acceptance.md) for prerequisites and the final physical capture checklist.

On Linux, `npm run collector:desktop:dev` starts the native desktop application
and `npm run collector:package:linux` creates an installable `.deb` package.
Recording currently requires an Xorg session so global screen and input capture
remain complete and auditable; see [Linux collector](docs/linux.md).

The current implementation is milestone-based. See [V1 status](docs/V1_STATUS.md) for verified capabilities and remaining gates.

Production operations are documented in the [incident-response runbook](docs/operations/incident-response.md), [contributor-support runbook](docs/operations/contributor-support.md), [threat model](docs/security/threat-model.md), and [draft contributor privacy notice](docs/privacy/privacy-notice.md). These documents require named owner and legal/security approval before an external pilot.

The production deployment and cross-platform release target is defined in
[DataTrains Production V1](docs/PRODUCTION_V1.md). The production goal extends
the verified local-first V1 without weakening its consent, immutability,
recoverability, or deterministic-release requirements. See the
[collector release procedure](docs/deployment/collector-release.md) for the
identity, signing, packaging, promotion, and platform acceptance gates.

The machine-checked [production readiness record](docs/operations/production-readiness.md)
refuses promotion while any cloud, approval, physical-platform, soak, or pilot
gate remains open.
