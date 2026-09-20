# DataTrains incident-response runbook

Status: engineering runbook. Add named on-call owners and notification channels,
then complete a staging exercise before an external pilot.

## Severity

- **SEV-1:** suspected cross-tenant or public data exposure, signing/identity/
  database/R2 credential compromise, recording outside the visible consent
  boundary, or corrupted production release.
- **SEV-2:** sustained API outage, failed worker schedule, queues older than five
  minutes, restore failure, or multiple contributor submissions at risk.
- **SEV-3:** isolated failed upload/job, one contributor blocked, or recoverable
  administrator/reviewer defect without exposure.

## First 15 minutes

1. Acknowledge the alert and create an incident record with UTC timestamps.
2. Assign incident commander, technical lead, communications owner, and scribe.
3. Record affected environment, service/revision/job execution, request/trace
   IDs, organizations, time window, and suspected data classes. Do not paste
   tokens, presigned URLs, captured frames, or personal data into chat/tickets.
4. Preserve evidence: place application legal holds where appropriate and save
   Cloud Audit Logs, deployment digest, database migration inventory, and
   relevant structured logs in access-controlled incident storage.
5. Contain according to the playbook below. Prefer reversible traffic/identity
   controls. Do not purge evidence during investigation.

## Containment playbooks

### Suspected exposure or authorization bypass

1. Disable the affected Vercel deployment or route API traffic to a known-good
   Cloud Run revision.
2. Revoke affected OpenID sessions and invitations; disable compromised
   accounts without deleting identity/audit history.
3. Disable R2 credentials if object access is suspected, then issue a new
   bucket-scoped credential through Secret Manager.
4. Place legal holds on affected sessions/releases and identify access through
   audit events, Cloud Audit Logs, request IDs, and trace IDs.

### Worker or queue failure

1. Confirm Cloud Scheduler last attempt and Cloud Run Job execution result.
2. Check `worker_queue_snapshot` counts and `oldest_ready_age_seconds`.
3. Verify Supabase and R2 availability before manually executing the worker.
4. Use the administrator recovery queue only after identifying the failure;
   manual retries are audited. Do not edit queue rows directly.

### Bad release

1. Stop distribution and record the immutable release ID, manifest hash, bundle
   hash, and deployed digest.
2. Run the standalone release validator on a separately downloaded copy.
3. Do not overwrite or rename an existing release. Correct source sessions and
   publish a new release name after review.

### Collector privacy or signing incident

1. Stop new invitations/downloads and revoke the affected signing certificate
   or updater key when compromise is plausible.
2. Tell contributors to pause/close the Collector; do not request raw recordings
   over email or chat.
3. Determine OS/version, collector version, task, visible recording state, and
   time window using minimal diagnostics.
4. Treat capture outside the declared boundary as SEV-1 and involve privacy/
   legal owners immediately.

## Recovery and verification

1. Deploy only an already-reviewed immutable digest or a new digest produced by
   the complete CI/security gates.
2. Verify `/livez`, `/readyz`, unauthenticated denial, one role-specific request,
   worker drain, queue age, and alert recovery.
3. For data incidents, compare a pre-backup inventory with an isolated restore
   and run `scripts/verify-database-restore.sh` before changing production.
4. Confirm no unexpected cross-tenant access, object loss, duplicate release,
   or unaudited retry/deletion occurred.

## Communication and closure

- Privacy/legal decides notification obligations and timelines; engineering
  must not promise that notification is unnecessary.
- Provide contributors with plain-language impact and steps, not internal keys
  or captured content.
- Close only after containment, recovery verification, customer/privacy
  communication decision, root cause, corrective actions, and evidence links
  are recorded.
- Run a blameless review within five business days for SEV-1/SEV-2.

## Ownership record

| Responsibility | Primary | Backup | Channel |
| --- | --- | --- | --- |
| Incident commander | pending | pending | pending |
| Infrastructure/on-call | pending | pending | pending |
| Security | pending | pending | pending |
| Privacy/legal | pending | pending | pending |
| Contributor communication | pending | pending | pending |

