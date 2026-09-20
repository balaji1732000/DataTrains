# Retention, legal hold, and erasure

DataTrains keeps normal artifact writes immutable. Permanent removal is exposed
only through the administrator governance API and the worker's dedicated purge
boundary. The Collector, reviewer, normal upload API, and release exporter
cannot call that boundary.

## Production V1 erasure flow

1. An organization administrator opens a terminal, non-released session and
   records an approved erasure reason.
2. The API locks the session, rejects active work, active video redaction, and
   released data, rejects an active legal hold, snapshots every raw and derived object key, creates one
   durable deletion request, and appends an audit event.
3. The worker rechecks both session eligibility and legal hold after claiming a
   lease. A newly placed hold changes the request to `blocked` without touching
   storage. An unexpected state change goes to `dead_letter` without touching
   storage.
4. The worker purges only the snapshotted keys. A failure is retried with
   backoff; exhausted work appears in the administrator console for an audited
   manual retry.
5. After all keys are absent, one database transaction removes artifact,
   validation, processing, review, and redaction payload metadata; retains the immutable
   audit/deletion tombstone; and marks the session `DELETED`.

The administrator console requires an explicit reason and a second irreversible
confirmation. Duplicate active holds and duplicate active deletion requests
return HTTP 409. An active deletion request also makes the session ineligible
for release publication.

## Legal holds

Only organization administrators may place or release holds. A reason is
mandatory and both operations are audited. A hold may be placed after erasure
is queued but not after a worker has leased and begun purging it. Releasing a
hold requeues a blocked request immediately.

Legal holds are enforced by the application, so R2 lifecycle rules must not
delete `raw/`, `derived/`, or `releases/` objects independently. An object-store
lifecycle policy would bypass the database hold check. Lifecycle cleanup is
safe only for incomplete multipart and uncommitted staging data.

## Automatic retention

The administrator console and organization-scoped API store independent
`raw_days`, `derived_days`, and `release_days` values per organization. Every
change records the administrator and an audit event; cross-organization reads
and writes are rejected.

The worker sweeps for expired data every 15 minutes and creates separate durable
jobs for raw session evidence, derived session data, and release exports. Each
job snapshots its exact object keys and the policy update timestamp that caused
the expiry. Before leasing, the worker rechecks the current policy, resource
state, and every applicable legal hold. A policy change cancels the stale job;
a hold blocks it. Releasing the hold requeues the blocked work.

Only a leased job may cross the storage purge boundary. Policy edits and new
legal holds that affect an already leased purge return HTTP 409, closing the
race where policy or hold state could change after the final check. Failures
retry with backoff and then dead-letter. Completion retains release/session
metadata and audit history, adds a `purged_at` tombstone to the affected
inventory, including a redaction-job tombstone, and makes expired release
downloads return HTTP 410. A purged redaction can no longer satisfy release
eligibility.

Do not enable this worker against production data until a staging rehearsal has
confirmed the intended day values, late-hold behavior, dead-letter alerting,
and database/object-store restore procedure.

## Verified failure cases

PostgreSQL integration tests prove:

- an active or newly added hold preserves all objects;
- duplicate holds and requests are rejected;
- local development actors do not need a production account row;
- session-state drift dead-letters without purging;
- a queued erasure prevents release publication;
- organization A cannot list or retry organization B's erasure;
- organization A cannot read or change organization B's retention policy;
- a late legal hold blocks raw, derived, and release jobs without deleting an
  object;
- releasing the hold resumes every affected purge;
- extending a policy cancels a queued stale job and preserves the object;
- completed jobs mark durable metadata tombstones and expired releases return
  HTTP 410;
- raw and derived keys are removed only by the explicit purge boundary; and
- completed and partially written dead-letter redaction paths are included in
  explicit erasure, while queued or leased redaction blocks the request; and
- completion leaves an audited `DELETED` tombstone.
