# V1 status

This file records evidence, not aspiration. A box is checked only after the capability has an automated or repeatable verification path.

This document tracks the local-first proof. The cloud deployment, production
operations, and macOS/Windows/Linux release gates are tracked separately in
[DataTrains Production V1](PRODUCTION_V1.md).

## Milestones

- [ ] Foundation: all required toolchains, apps, services, database, and CI checks run
- [x] Canonical `trajectory/v1` JSON Schema and standalone single-trajectory validator
- [x] Domain model and session state machine
- [x] BlobStore and immutable local adapter
- [x] Admin project/task/assignment workflow
- [x] Collector onboarding, consent, and preflight workflow
- [ ] Resilient segmented screen/input capture
- [ ] Finalization, hashing, submission, and crash recovery
- [x] Asynchronous validation worker
- [x] Synchronized reviewer and QA workflow
- [x] Deterministic release exporter and release validator
- [x] 100-trajectory synthetic reliability and security run

## Verified commands

```bash
npm run check
npm test
npm run build
npm run validate:trajectory -- packages/trajectory-schema/fixtures/valid-trajectory.json
npm run validate:release -- data/blobstore/releases/<release-id>
npm run postgres:start
npm run go:test:integration
npm run collector:core:check
npm run collector:core:test
npm run collector:tauri:check
npm run collector:tauri:test-compile
npm run collector:tauri:build
npm run acceptance:v1
```

The foundation remains open until the packaged Tauri shell is launched on a Windows test host. All repository-local JavaScript, Go, PostgreSQL, Rust-core, and Windows cross-compilation checks pass on the development host.

## Current evidence

- `trajectory/v1` accepts the canonical fixture and rejects missing fields and unknown action types.
- PostgreSQL integration covers organization, project, template, task, contributor, assignment, versioned consent, preflight, and `READY -> RECORDING` through HTTP.
- Go tests reject `RELEASED -> RECORDING`; database session transitions update state and append audit metadata in one transaction.
- A database trigger rejects update or deletion of audit events, including by the table owner.
- Supabase-oriented migrations deny its public Data API roles and create a dedicated `datatrains_runtime` group with explicit RLS policies. Integration verifies that role cannot administer roles/databases, bypass RLS, or mutate/delete audit history while still accessing application data through its policy.
- Database migrations are serialized and SHA-256 pinned. Re-running unchanged history is idempotent, while modifying an applied SQL file fails closed; integration verifies both paths.
- The restore verifier applies checksum-pinned forward migrations only to an explicitly confirmed non-production restore, validates RLS, foreign keys, release membership, deletion tombstones, and append-only audit enforcement, and can compare every application-table count and migration checksum with a pre-backup inventory. Its local rehearsal passes without retaining the synthetic audit record.
- LocalBlobStore tests prove hashing, round trips, traversal rejection, and immutable `raw/` keys.
- The Go API applies migrations, starts, confirms database health, and responds on the local host.
- Admin and collector web assets compile, and source-level UI tests cover the live admin workflow, review safeguards, collector consent boundary, capture controls, and submission path.
- Production web and collector identity use authorization-code/PKCE. Web tokens remain in encrypted secure server cookies; collector refresh tokens remain in the operating-system credential vault, access tokens remain in native memory, and the native webview bridge permits only contributor operations.
- The collector retrieves assignments and the currently effective consent document, shows supplied assets and finish criteria, stores an exact version/hash acceptance, runs native preflight, and advances backend state through the API.
- The pure Rust collector core has tests for required preflight checks, allow/deny policy, monotonic events, segmented process control, pause/resume, crash recovery, output safety, SHA-256 manifests, finalized-session immutability, and discovery of sealed captures awaiting submission.
- The complete Tauri command layer passes strict Windows-target Clippy, compiles its tests, and links a release Windows executable. Windows CI runs the native collector tests, builds an NSIS installer, and publishes it as an artifact.
- The Windows collector uses low-level keyboard/mouse hooks, ignores injected input, polls foreground context independently, stores virtual-key identifiers rather than reconstructed text, and fails closed when native hooks cannot start.
- Windows screen capture uses one-minute FFmpeg H.264 segments. The collector keeps its recording badge window above the task, exposes immediate pause/finish controls, and automatically pauses when focus leaves the allowlisted task application or enters a denied sensitive context.
- Real Windows, macOS, and Linux screen/input runs remain acceptance gates for the resilient-capture and finalization milestones. Linux desktop linking requires GTK/WebKit development packages on the packaging host; macOS signing/notarization and physical permission checks require Apple hardware and an Apple Developer identity.
- The production upload protocol persists multipart state and completed part ETags in PostgreSQL, resumes only missing 16 MiB parts, assembles bytes under a random staging key, verifies the full byte count and SHA-256, and only then promotes to an immutable raw key and registers the artifact. PostgreSQL integration simulates an interrupted two-part upload and verifies the committed bytes.
- Submission requires canonical trajectory, MP4, and NDJSON artifacts and creates one durable validation job in the same transaction as `UPLOADING -> SUBMITTED`.
- The collector verifies every sealed local hash, creates the canonical trajectory, prefers the resumable direct-to-R2 protocol, falls back to the local hash-gated API only when multipart upload is unavailable, and persists a local submission receipt. Interrupted recordings reopen paused; sealed-but-unsubmitted captures reopen and recover server-persisted upload progress.
- A Windows-native collector test exercises a sealed video/events/output manifest against a local mock API, verifies the canonical trajectory upload and submission response, and proves the receipt is persisted. The Linux acceptance run cross-compiles this test; Windows CI executes it.
- Graceful application exit pauses and checkpoints an active session, while the FFmpeg process guard kills and reaps any recorder process that survives normal shutdown.
- The Go worker leases with `FOR UPDATE SKIP LOCKED`, retries transient failures, validates `trajectory/v1`, verifies raw hashes and cross-references, derives click actions and 10 Hz mouse movement without reconstructing typed text, writes deterministic derived JSON and JSONL, and advances valid sessions through `PROCESSING -> READY_FOR_REVIEW`.
- The production worker also streams each submitted segment through a bundled, bounded `ffprobe` process. It requires MP4/MOV with one H.264 video stream, positive dimensions and duration, and reasonable agreement with the declared segment duration; corrupt media is rejected before review. Tests cover a generated valid H.264 segment, corrupt bytes, a missing probe executable, and propagation into validation findings.
- Production JavaScript dependencies report zero audit findings. `govulncheck` found the affected `golang.org/x/text` 0.29.0 transitive path, it was upgraded to fixed 0.39.0, all Go/PostgreSQL tests remained green, and the repeated scan reports no reachable Go vulnerabilities. Rust and container audits remain mandatory CI gates.
- Exhausted processing jobs enter a distinct, timestamped dead-letter queue. Organization administrators can inspect the last failure and invoke an audited manual retry; integration proves cross-organization jobs are hidden and cannot be retried.
- Administrator erasure is durable, asynchronous, organization-scoped, and audited. The worker alone can cross the immutable raw-object boundary; it rechecks session state and legal hold before purging, dead-letters failures, and leaves a `DELETED` tombstone. Integration covers duplicate requests, a hold added after queuing, state drift, safe manual retry, cross-organization denial, and deletion-versus-release exclusion.
- The administrator console and organization-scoped API configure independent raw, derived, and release retention periods with audited changes and cross-organization denial. The worker creates separate durable expiry jobs every 15 minutes, snapshots the triggering policy and object inventory, rechecks eligibility and late legal holds at lease time, cancels stale jobs after policy changes, blocks conflicting policy/hold writes once purging begins, retries and dead-letters failures, and leaves `purged_at` metadata tombstones. PostgreSQL integration proves late-hold blocking/resume, all three storage classes, policy-extension cancellation, exact object removal, audit history, and HTTP 410 for expired releases. Raw/derived/release R2 lifecycle deletion remains disabled because it would bypass these guarantees.
- Review queue claims are exclusive and releasable; PostgreSQL integration proves claim conflicts, range-capable evidence reads, the PII acceptance gate, persisted rubric scores, and `READY_FOR_REVIEW -> ACCEPTED`.
- The admin console consumes the live review APIs and synchronizes the video player, segment selection, action timeline, outputs, and review form. Recordings stay private by default; reviewers can optionally make an explicit per-segment publish-as-shown or timed rectangular-mask decision.
- Optional `redacted_video` release support is fail-closed end to end. Acceptance and a complete `redaction/v1` plan commit atomically; database triggers prevent in-place plan mutation; the worker reinspects video, applies deterministic black masks, strips audio, records an integrity manifest, and exposes exhausted work through an organization-scoped audited recovery queue. Release export verifies every plan/output/source/hash mapping, while retention and explicit erasure inventory the generated videos and manifests and prevent purged work from being republished.
- Release export derives a stable ID and configuration hash from sorted accepted sessions, writes `README.md`, `schema.json`, individual trajectory JSON, aggregate JSONL, exact `checksums.sha256`, and a hashed manifest, streams a deterministic ZIP without buffering the dataset in memory, records manifest/bundle integrity and membership transactionally, and advances sources to `RELEASED`.
- The standalone release validator verifies inventory, sizes, SHA-256 digests, exact checksum-file contents, embedded schema identity, configuration identity, trajectory schemas, source order, exact JSON/JSONL agreement, release profiles, redaction references, video paths/media types, and MP4 headers; tamper tests reject modified trajectory and video payloads.
- PostgreSQL integration completes 100 synthetic trajectories through task creation, shared versioned consent, assignment, upload, asynchronous processing, semantic normalization, exclusive review, PII-gated acceptance, deterministic release, and release-file verification in one repeatable run.
- `npm run acceptance:v1` creates or reuses an isolated `trajectory_acceptance` database, runs every JavaScript/Go/Rust check plus the 100-trajectory test, links the Windows collector, starts the real API, verifies database health, and cleans up services it started.
- The acceptance runner and CI serialize PostgreSQL integration packages because they intentionally reset a shared ephemeral database. The complete acceptance gate passed after this deterministic test isolation was enforced.
- The API validates and propagates W3C/Cloud Trace context, returns a trace response header, and correlates structured Cloud Logging entries without recording request bodies, credentials, or object URLs. The drain worker emits privacy-safe aggregate queue snapshots. The Cloud Run release helper provisions a least-privilege one-minute scheduler and performs an immediate worker execution; a separate idempotent helper configures API 5xx, worker-failure, dead-letter, and stale-queue alerts once an operator supplies approved notification channels.
- Engineering drafts now define the V1 threat model, contributor privacy notice, incident response, and contributor support flow. Named on-call ownership, jurisdiction-specific legal approval, alert destinations, and an incident exercise remain external production gates.
- The production-readiness ledger maps all 14 release gates to repository evidence and explicit remaining work. Its validation command checks every referenced file, while its promotion mode fails closed until cloud, approval, physical-platform, real-soak, and pilot evidence has moved every gate to complete.
