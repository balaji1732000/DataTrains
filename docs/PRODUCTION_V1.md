# DataTrains Production V1 goal

## Objective

Deliver a secure, recoverable, observable production application that lets an
administrator create structured computer-use tasks, invited professional
contributors complete those tasks in a consented desktop collector, reviewers
inspect synchronized evidence and request rework or accept a submission, and an
administrator publish a deterministic, independently verifiable dataset.

Production V1 must support installable collectors for macOS, Windows, and Linux.
No platform is considered supported until its native permission, capture,
recovery, packaging, signing, update, and physical acceptance tests pass.

## Production architecture

| Component | Production responsibility |
| --- | --- |
| Vercel | Next.js administrator and reviewer interfaces |
| OpenID Connect provider | Login, invitation, MFA, session recovery, and identity claims |
| Go API on Google Cloud Run | Authorization, projects, assignments, sessions, reviews, upload authorization, and audit events |
| Supabase PostgreSQL | Users, organization memberships, roles, tasks, session metadata, artifact inventory, jobs, reviews, and release metadata |
| Cloudflare R2 | Immutable recordings, screenshots, event JSON/JSONL, outputs, manifests, derived artifacts, and exports |
| Go worker on Google Cloud Run | Integrity validation, media inspection, normalization, privacy checks, redaction workflow, and deterministic dataset generation |
| Tauri collector | Explicit consent, native preflight, task-scoped recording, crash recovery, resumable upload, and contributor status/history |

The Collector and web interfaces never receive database or R2 credentials.
They authenticate with OpenID Connect and call the Go API. The API issues
short-lived, object-scoped upload authorization so large artifacts travel
directly from the Collector to R2.

## Required user journeys

### Contributor

1. Accept an invitation and sign in through the system browser using OpenID
   Connect authorization-code flow with PKCE.
2. Review the assigned task, versioned consent, capture boundary, required
   application, inputs, expected output, and finish criteria.
3. Pass native permission and environment preflight.
4. Start, visibly pause/resume, and finish a task-scoped capture.
5. Recover safely after a crash without losing completed segments.
6. Select and verify the expected output.
7. Upload directly and resumably to R2, seal the immutable manifest, and see
   processing, review, rework, accepted, completed, or released status.

### Reviewer

1. Sign in with a reviewer membership.
2. Claim one review atomically.
3. Inspect task instructions, output, video segments, synchronized actions,
   validation results, and privacy findings.
4. Accept, reject, or request rework with a versioned rubric and explanation.
5. Keep recordings private by default or approve a complete, immutable
   per-segment redaction plan in the same transaction as acceptance.
6. Preserve every decision as append-only history.

### Administrator

1. Manage organizations, projects, contributors, reviewers, task templates,
   consent documents, and assignments.
2. Monitor upload, processing, review, quality, and failure queues.
3. Publish only accepted trajectories into a deterministic release.
4. Download a deterministic ZIP export with schema, provenance, inventory, and checksums that
   passes the standalone validator.
5. Apply retention, legal hold, deletion, access, and audit policies.

## Desktop support matrix

| Platform | Native implementation | Production gate |
| --- | --- | --- |
| Windows 10/11 | Windows Graphics/FFmpeg-compatible segmented video, low-level permitted input hooks, foreground policy enforcement | Signed installer; screen/input permission and denial tests; crash recovery; upgrade/rollback; physical task and upload acceptance |
| macOS supported releases | ScreenCaptureKit-compatible segmented video, Accessibility permission for permitted input events, active-application policy enforcement | Apple Developer signing and notarization; permission onboarding; crash recovery; upgrade/rollback; Intel/Apple Silicon decision and physical acceptance |
| Linux supported distributions | PipeWire/desktop portal video on Wayland where available; X11 capture on Xorg; input capture only through mechanisms explicitly permitted by the active desktop session | Signed package/repository strategy; Ubuntu validation; Xorg physical acceptance; Wayland capability documented and fail-closed when required telemetry is unavailable; crash recovery and upgrade/rollback |

Wayland does not permit unrestricted global input observation. Production V1
must not claim equivalent Wayland input capture unless an explicit portal or
task-application integration provides it. A task whose required telemetry is
unavailable must fail preflight rather than silently produce incomplete data.

## Security and privacy invariants

- Recording is explicit, visible, task-scoped, and immediately pausable.
- No covert capture, clipboard capture by default, password reconstruction, or
  deliberate collection from password managers, banking, private messaging,
  authentication dialogs, or other denied contexts.
- OpenID identity is mapped server-side to an internal contributor ID. A caller
  cannot choose its identity or role through headers or request fields.
- Authorization is enforced in the Go API for every organization-scoped
  resource; user-editable token claims are never trusted for roles.
- Raw artifacts are immutable. Processing writes only derived objects. Release
  profile `trajectory_only` excludes video; `redacted_video` fails closed unless
  every video is tied to a completed, integrity-checked reviewer plan.
- Every upload is scoped, short-lived, size/type checked, checksum verified,
  idempotent, and tied to an authorized session.
- Database, R2, OpenID, signing, and deployment credentials remain in managed
  secret stores and never enter client bundles or repository history.
- Retention and deletion are asynchronous, auditable, and tested, including
  derived objects and exports where policy allows deletion.

## Production acceptance gates

Production V1 is complete only when all gates have repeatable evidence. The
machine-readable mirror and promotion guard are documented in
[Production readiness record](operations/production-readiness.md).

- [ ] Repository history, protected production branch, review requirements,
  dependency scanning, reproducible lockfiles, and CI for all components.
- [ ] Separate local, staging, and production configuration with managed
  secrets and no production credentials on developer machines or clients.
- [ ] OpenID login, logout, invitation, MFA-compatible recovery, revocation,
  organization membership, and contributor/reviewer/admin authorization.
- [ ] Supabase migrations, RLS/least privilege, connection pooling, point-in-time
  recovery or documented backup target, restore rehearsal, and database alerts.
- [ ] Private R2 buckets, direct multipart upload, retry/resume, checksums,
  immutability, CORS, lifecycle/retention, deletion, and access audit tests.
- [ ] Cloud Run API and worker deploys with health/readiness behavior, bounded
  database pools, controlled autoscaling, idempotent jobs, retries, dead-letter
  handling, structured logs, metrics, traces, alerts, and rollback.
- [ ] Vercel preview/staging/production deployment with secure server-side
  sessions, strict browser headers, API allowlists, and end-to-end authorization.
- [ ] Windows collector installation, signed release, physical capture,
  permission denial, focus policy, crash recovery, resumable upload, update,
  rollback, and uninstall verification.
- [ ] macOS collector installation, signing/notarization, native capture,
  permission denial, focus policy, crash recovery, resumable upload, update,
  rollback, and uninstall verification.
- [ ] Linux collector installation, supported-session detection, native capture,
  permission denial, focus policy, crash recovery, resumable upload, update,
  rollback, and uninstall verification.
- [ ] Review, rework, resubmission, acceptance, rejection, release publication,
  deterministic export, and independent validator end-to-end tests.
- [ ] Threat model, privacy notice, consent evidence, data processing terms,
  incident response, contributor support, and operational runbooks reviewed.
- [ ] Staging soak of at least 100 complete trajectories with no unexplained
  artifact loss, state divergence, cross-tenant access, or invalid release.
- [ ] Controlled external pilot across all supported platforms with measured
  completion, failure, upload recovery, review, and privacy outcomes.

## Delivery order

1. Version control, CI, environments, container build, and production contracts.
2. Supabase control database and OpenID identity/authorization.
3. R2 object storage and direct resumable upload protocol.
4. Cloud Run API, worker, migrations, observability, and rollback.
5. Vercel administrator/reviewer deployment and browser security.
6. Collector production protocol and secure desktop session storage.
7. Windows production release.
8. macOS native capture and production release.
9. Linux Xorg and Wayland-capability production release.
10. Security/recovery exercises, 100-trajectory staging soak, external pilot,
    and production release approval.

Current repository evidence for automatic retention, legal hold, and erasure is
documented in [Retention, legal hold, and erasure](deployment/data-governance.md).
The gate remains open until real R2 provisioning and staging exercises pass.
The optional recording-publication boundary is documented in
[Reviewed video and redaction](deployment/video-redaction.md).

## Explicit exclusions

Production V1 does not include a contributor marketplace, payments, social
features, covert monitoring, generalized employee surveillance, or AI-generated
task/review decisions. These require separate product, legal, security, and
operational goals.
