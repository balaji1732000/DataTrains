# Processing worker

The V1 worker entry point is `services/api/cmd/worker`, allowing it to share the
control-plane database and blob-store packages without duplicating a Go module.

It leases queued validation work with PostgreSQL `FOR UPDATE SKIP LOCKED`, checks
every registered immutable raw artifact against its stored size and SHA-256,
validates `trajectory/v1`, performs cross-artifact and monotonic-time checks, and
writes deterministic normalized JSON under `derived/sessions/<id>/trajectory.json`.

Run it from the repository root after starting PostgreSQL:

```bash
npm run postgres:start
npm run worker:run
```
