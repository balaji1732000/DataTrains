# Supabase PostgreSQL production contract

Supabase stores DataTrains control-plane records only. Browsers and collectors
never use Supabase's generated Data API, anonymous key, service-role key, or a
database credential. They call the Go API, which applies organization and role
authorization before using PostgreSQL.

## Projects and connection roles

Create separate Supabase projects for staging and production. Keep the default
`postgres` owner credential exclusively for the migration and controlled
recovery jobs. Create a distinct login for the API and worker, grant that login
membership in the migration-created `datatrains_runtime` NOLOGIN role, and put
only its TLS pooler URL in Google Secret Manager.

The runtime group role is not a superuser, cannot create roles or databases,
cannot bypass row-level security, and cannot update or delete append-only audit
events. Migration 011 grants it explicit policies on application tables while
the Supabase `anon` and `authenticated` roles remain denied. Do not grant the
runtime login membership in `service_role`, `supabase_admin`, or the database
owner role.

Use the transaction pooler for Cloud Run's short-lived, autoscaled instances.
Start with four connections per API or worker instance and three maximum API
instances. Include TLS verification in the connection URL. The migration job
uses a direct owner connection and one connection; migrations are serialized by
an advisory transaction lock.

## Provisioning checklist

1. Create staging first and record its region and Postgres major version.
2. Store the owner/direct migration URL and runtime/pooler URL as separate
   Google Secret Manager secrets. Never copy them into Vercel or a client.
3. Run all migrations with the owner URL and verify the RLS/role integration
   test against an empty staging database. Applied migration files are pinned
   by SHA-256; a changed historical file blocks deployment. Existing local
   rows without a checksum are backfilled once, so audit the repository before
   the first production migration and never edit an applied file afterward.
4. Create the runtime login with a generated password, grant
   `datatrains_runtime`, and verify it cannot assume owner, `anon`,
   `authenticated`, or administrative roles.
5. Configure connection, disk, CPU, memory, error-rate, and long-running-query
   alerts. Record the alert destination and escalation owner.
6. Enable the plan's point-in-time recovery capability, or document the exact
   backup frequency and maximum acceptable recovery-point objective if PITR is
   unavailable.

## Restore gate

A backup is not accepted until it has been restored into an isolated Supabase
project and checked. Before taking the recovery point, create a stable source
inventory without printing the connection URL:

```bash
DATATRAINS_DATABASE_URL="$SOURCE_DATABASE_URL" \
  scripts/database-inventory.sh > before-restore.inventory
```

After Supabase restores that recovery point into a separate project, verify it:

```bash
DATATRAINS_RESTORE_DATABASE_URL="$ISOLATED_RESTORE_DATABASE_URL" \
DATATRAINS_CONFIRM_ISOLATED_RESTORE=I_UNDERSTAND_THIS_IS_AN_ISOLATED_RESTORE \
DATATRAINS_EXPECTED_INVENTORY=before-restore.inventory \
  scripts/verify-database-restore.sh
```

The verifier runs checksum-pinned forward migrations, validates RLS, foreign
keys, release membership, deletion tombstones, and the append-only audit
trigger, then compares every application-table count and migration checksum.
It creates one synthetic audit event inside a transaction and rolls it back.
It does not print the database URL or retain temporary output.

Do not point the destructive Go integration suite at a restored production
copy: those tests intentionally reset their database. Run that suite against a
separate empty staging test database, then complete one contributor-to-release
flow through the application connected to the verified restore. At least
quarterly—and before first launch—record timestamps, chosen recovery point,
recovery duration, inventory comparison, release-validator result, and any
missing data.

Production promotion is blocked until the staging restore rehearsal succeeds
and named operators own backup alerts, credential rotation, incident response,
and emergency read-only access.
