# DataTrains Production V1 threat model

Status: engineering draft. Security and privacy owners must review and sign the
production-readiness record before an external pilot.

## Protected assets

- raw screen recordings, input events, screenshots, and submitted outputs;
- derived trajectories, reviews, release exports, and integrity hashes;
- contributor identity, organization membership, consent, and audit history;
- OpenID, Supabase, R2, signing, Vercel, and Google Cloud credentials; and
- release integrity, tenant isolation, and collector authenticity.

## Trust boundaries

1. The contributor controls their workstation and the applications visible on
   it. Captured bytes are untrusted until server validation completes.
2. The Tauri webview is untrusted relative to native credential and filesystem
   handling. Its command bridge exposes only contributor operations.
3. Vercel renders the administrator/reviewer interface but never receives
   database or R2 credentials. Access tokens stay in encrypted server cookies.
4. The Go API is the authorization boundary. It maps verified OpenID
   issuer/subject identities to server-side organization memberships.
5. Supabase stores control metadata; R2 stores private objects. Neither is
   directly accessible to browser or collector identities.
6. The worker alone validates untrusted media and crosses the governed purge
   boundary. Migration jobs alone receive database-owner credentials.

## Principal threats and controls

| Threat | Required control and evidence |
| --- | --- |
| Cross-organization access | Every resource lookup resolves organization ownership server-side; integration tests deny cross-tenant list, read, mutation, recovery, release, retention, and erasure operations. |
| Forged identity or role | Authorization-code/PKCE, issuer/audience/signature/expiry verification, random state and nonce, server-side memberships, revocation, MFA-compatible provider policy. User-editable claims never grant roles. |
| Stolen browser or desktop token | Secure encrypted web cookie, native-memory access token, OS credential-vault refresh token, short access-token lifetime, logout revocation, no tokens in logs or URLs. |
| Malicious upload | Session-scoped multipart authorization, clean server-derived keys, bounded size/type, random staging key, complete SHA-256 verification, immutable promotion, schema validation, bounded `ffprobe`. |
| Raw-object overwrite | Conditional create-only promotion and immutable `raw/` adapter behavior; only the audited governance worker may purge. |
| Sensitive content captured | Explicit visible consent, denylisted contexts, automatic focus pause, no clipboard capture, no reconstructed typed text, reviewer PII gate, raw recordings excluded from every release; trajectory-only default; optional derived video requires a complete plan, audio removal, reinspection, and masks. |
| Review or release tampering | Exclusive review claims, atomic acceptance/redaction decision, database-enforced immutable versioned plans, append-only review/audit history, PII gate, exact output-plan/hash verification, deterministic release ID/inventory/checksums/ZIP, immutable source membership. |
| Deletion bypasses legal hold | Application-owned raw/derived/release expiry queues recheck holds and policy versions at lease; conflicting hold/policy writes fail while purge is leased; R2 lifecycle deletion is forbidden for governed prefixes. |
| Worker crash or duplicate execution | PostgreSQL leases with expiry and `SKIP LOCKED`, idempotent object purge, bounded retries, dead-letter queues, manual audited recovery, scheduler and queue-age alerts. |
| Supply-chain compromise | Lockfiles, protected review workflow, npm/Go/Rust vulnerability gates, container scan, non-root image, immutable Artifact Registry tags, digest-only deployment, platform signing/notarization gates. |
| Database loss or corruption | Checksum-pinned migrations, Supabase backup target, isolated restore verifier, row-count/migration inventory comparison, RLS/FK/audit/release/tombstone checks. |
| Signing-key compromise | Keys remain in platform-managed signing systems, never in repository/CI artifacts; revoke certificates, stop distribution, rotate updater trust, and publish a clean signed build. |

## Explicitly forbidden behavior

- covert or generalized employee monitoring;
- capturing passwords, clipboard contents, authentication dialogs, banking,
  private messaging, or password-manager contexts;
- accepting caller-selected roles or organization IDs without ownership checks;
- public R2 access, wildcard credentialed CORS, or client database/R2 secrets;
- mutable production image tags or unsigned production collectors; and
- lifecycle deletion of governed `raw/`, `derived/`, `releases/`, or `bundles/`.

## Residual risks and open gates

- OS-level capture controls vary by Windows, macOS, Xorg, and Wayland and need
  physical permission-denial and sensitive-context tests.
- A compromised contributor workstation can manipulate locally captured input;
  server validation detects integrity and consistency failures but cannot prove
  the endpoint itself was trustworthy.
- Reviewer privacy decisions and manual rectangle placement remain human
  judgments. The implemented redacted-video profile must still pass staging
  exercises and privacy/legal approval before production enablement.
- Identity-provider MFA/recovery, real R2 policy, Supabase recovery, Cloud Run
  alerts, Vercel deployment, signing, and incident exercises require external
  staging evidence.

## Review record

| Role | Name | Decision | Date |
| --- | --- | --- | --- |
| Engineering owner | pending | pending | pending |
| Security owner | pending | pending | pending |
| Privacy/legal owner | pending | pending | pending |
