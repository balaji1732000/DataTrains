# Production foundation — 2026-09-20

This record contains identifiers and verification results only. No credential,
refresh token, database password, client secret, or storage secret is included.

## Google Cloud Run

- Project: `datatrains-production` (`963738422674`)
- Region: `asia-southeast1`
- API service: `datatrains-api`
- Ready revision: `datatrains-api-00003-cvx`, receiving 100% of traffic
- Stable URL: `https://datatrains-api-963738422674.asia-southeast1.run.app`
- Immutable image digest:
  `sha256:99845c8910d854ae9544584cccab532f02e6a1757e0a71116ceb2a0bca54d4a9`
- `/livez` returned service status `ok`.
- `/readyz` returned service and database status `ok`.
- A preflight request from the collector origin `tauri://localhost` returned
  the matching `Access-Control-Allow-Origin` value.
- A production-origin request from `https://datatrains-admin.vercel.app`
  returned the same origin in `Access-Control-Allow-Origin` after the Vercel
  deployment was added to the API allowlist.

The `datatrains-worker` Cloud Run Job uses the same immutable image. Execution
`datatrains-worker-lsv5w` completed successfully. Cloud Scheduler job
`datatrains-worker-every-5-minutes` is enabled in `Asia/Kuala_Lumpur` and uses
the dedicated `datatrains-scheduler` service account to invoke the job.

## Managed secrets

Google Secret Manager contains separate production versions for the Supabase
runtime URL and Cloudflare R2 access-key pair. The API and worker service
accounts have secret-accessor permission; values were not written into source,
container layers, collector packages, or this evidence file.

## Cloudflare R2

- Account ID: `6b2328db785f83dbc922ac292a924393`
- Private bucket: `datatrains-production-artifacts`
- S3-compatible endpoint:
  `https://6b2328db785f83dbc922ac292a924393.r2.cloudflarestorage.com`
- Runtime credentials were rolled and their current values exist only in the
  approved temporary secure workspace and Google Secret Manager.

## Auth0

- Tenant: `dev-pvoa8kc6eirqwbti.au.auth0.com`
- API audience: `https://api.datatrains.app`
- API signing algorithm: RS256
- Web application: `DataTrains Admin`
- Native public application: `DataTrains Collector`
- Both clients have delegated access to the DataTrains API; the native package
  embeds only its public client ID and public endpoints.
- The web client permits the deployed Vercel login callback, logout URL, web
  origin, and CORS origin. An unauthenticated `/manage` request redirected to
  `/login`, and the sign-in action reached the Auth0 Universal Login page for
  `DataTrains Admin`.
- GitHub OAuth application `DataTrains Production` is owned by GitHub account
  `balaji1732000`. Its only redirect is the Auth0 tenant callback; device flow
  and wildcard redirects are disabled, and the client secret is not stored in
  the repository.
- Auth0 social connection `github` is enabled for the admin and collector
  clients. Their Google and database/password connections are disabled.
- Both clients explicitly send `connection=github` and `prompt=login`, which
  prevents an existing Auth0 session from bypassing the GitHub-only policy.
- A production authorization-code/PKCE login completed through GitHub and
  reached `/manage`. The resulting Auth0 subject was bootstrapped as the first
  administrator of the `DataTrains Production` organization by successful
  Cloud Run Job execution `datatrains-bootstrap-admin-sv8hf`.

## Vercel

- Team: `balaji1732000s-projects`
- Project: `datatrains-admin`
- Production URL: `https://datatrains-admin.vercel.app`
- Verified production deployment: `dpl_AUD2h7FSrmYhrkHQdWm7kRNiQvTd`
- The project root is `apps/admin`; production-only server environment values
  and secrets are configured in Vercel rather than bundled into the browser.
- `GET /api/health` returned HTTP 200 with service status `ok`.
- The production sign-in redirect, GitHub authorization consent, Auth0
  callback, and administrator `/manage` access were verified.

## Remaining foundation work

- Provision independent staging resources and run restore, rollback,
  authorization, storage, and browser-security exercises before closing the
  production-readiness gates.
