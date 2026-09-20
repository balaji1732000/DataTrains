# Supabase production foundation — 2026-09-20

Organization: `balaji1732000's Org` (`tdsaspmiwwyfctdzaipb`)  
Project: `DataTrains Production` (`mfmeeyvtwybmhyulouif`)  
Region: `ap-southeast-1` (Singapore)  
Status at verification: `ACTIVE_HEALTHY`

## Schema and security verification

- Applied repository migrations `001` through `022`.
- Verified 29 application tables in the `public` schema.
- Verified row-level security on all 29 application tables.
- Verified 29 policies for the dedicated `datatrains_runtime` role.
- Verified the runtime role is non-login and cannot bypass row-level security.
- Verified no table privileges are granted to Supabase `anon` or
  `authenticated` roles.
- Added the 14 foreign-key indexes reported by the database advisor.
- Fixed mutable function search paths and restored the redaction policies that
  were initially skipped by migration dependency ordering.
- Re-ran Supabase security advisors with no remaining findings.
- Re-ran performance advisors with no missing foreign-key indexes; only
  expected unused-index informational findings remained on the empty database.
- Created the login role `datatrains_runtime_login` with a 20-connection limit,
  membership in the non-login `datatrains_runtime` role, and no bypass-RLS
  capability.
- Verified the runtime login through the Supavisor session pooler in
  `ap-southeast-1` and stored its URL as a managed Google Secret Manager
  version for the Cloud Run API and worker.

No database password, connection URL, generated key, or other secret value is
stored in this evidence file.

## Remaining production database work

- Create an independent staging project.
- Add the owner migration URL to Google Secret Manager before the next schema
  migration; the deployed runtime does not receive owner credentials.
- Configure and verify backups, alerts, and a staging restore rehearsal before
  closing the database gate.
