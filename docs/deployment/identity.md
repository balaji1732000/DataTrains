# Production identity and invitations

DataTrains uses an external OpenID Connect provider for login, passwords, MFA,
recovery, and token revocation. The Go API remains the authorization authority.
It does not trust a role, contributor ID, or organization supplied by a client.

## Provider contract

The selected provider must support authorization-code flow with PKCE, verified
email claims, refresh-token rotation, MFA, account recovery, logout/revocation,
and JWT tokens whose issuer and audience are stable. The API configuration is:

```text
TRAJECTORY_AUTH_MODE=oidc
TRAJECTORY_OIDC_ISSUER=https://issuer.example.com
TRAJECTORY_OIDC_AUDIENCE=datatrains-api
```

At startup, the API discovers the provider. On every protected request it
validates signature, issuer, audience, and expiry. It resolves the verified
`(issuer, subject)` pair to an internal `accounts` row and active organization
memberships. A token claim cannot grant a DataTrains role.

The Vercel web application is a confidential OpenID client. Configure its exact
callback as `https://APP_DOMAIN/api/auth/callback` and its exact post-logout URL
as `https://APP_DOMAIN/login`. Its server-only settings are:

```text
DATATRAINS_AUTH_MODE=oidc
DATATRAINS_APP_BASE_URL=https://APP_DOMAIN
DATATRAINS_OIDC_ISSUER=https://issuer.example.com
DATATRAINS_OIDC_CLIENT_ID=WEB_CLIENT_ID
DATATRAINS_OIDC_CLIENT_SECRET=WEB_CLIENT_SECRET
DATATRAINS_OIDC_AUDIENCE=datatrains-api
DATATRAINS_OIDC_SCOPES=openid profile email offline_access
DATATRAINS_OIDC_CONNECTION=github
DATATRAINS_SESSION_SECRET=AT_LEAST_32_RANDOM_CHARACTERS
TRAJECTORY_API_URL=https://API_DOMAIN
```

The web app creates fresh PKCE, state, and nonce values for every login. The
configured connection is requested with `prompt=login` so an existing Auth0
session from another identity provider cannot bypass the GitHub-only policy. The
callback validates all three and stores tokens only inside an encrypted,
`HttpOnly`, `Secure`, same-site cookie. The browser-facing control-plane proxy
removes caller-supplied identity headers and adds the access token on the
server. State-changing proxy requests also require the configured application
origin. Sign-out revokes the refresh token through the provider's discovered
revocation endpoint before clearing the local cookie; a provider failure is
reported after local sign-out. Rotate `DATATRAINS_SESSION_SECRET` to invalidate
every web session.

## Native collector client

The Windows, macOS, and Linux collector is a public OpenID client. Register it
separately from the web application. It uses authorization-code flow with PKCE
and opens the provider in the user's system browser. The provider must allow
loopback callbacks in the form `http://127.0.0.1:<random-port>/callback`; do not
register a fixed desktop port or put a client secret in the installer.

Embed these public values when creating a production collector package:

```text
DATATRAINS_AUTH_MODE=oidc
DATATRAINS_API_URL=https://API_DOMAIN
DATATRAINS_OIDC_ISSUER=https://issuer.example.com
DATATRAINS_OIDC_CLIENT_ID=NATIVE_CLIENT_ID
DATATRAINS_OIDC_AUDIENCE=datatrains-api
DATATRAINS_OIDC_CONNECTION=github
DATATRAINS_OIDC_REVOCATION_URL=https://issuer.example.com/oauth/revoke
```

Production builds prefer embedded values over runtime environment variables.
The application creates new PKCE, state, and nonce values for each login,
rejects redirects during provider discovery and token requests, validates the
ID token and access-token binding, and requires a refresh token. The access
token exists only in native memory. The rotating refresh token is stored in the
operating-system credential vault (Windows Credential Manager, macOS Keychain,
or the Linux Secret Service) and is never exposed to the webview or
`localStorage`.

All production collector API calls pass through the native layer. It accepts
only relative, versioned `/v1/` paths and adds the bearer token after refreshing
it when necessary. Sign-out calls the configured revocation endpoint and then
deletes the local credential. A remote-revocation failure is shown to the user
after local sign-out so operations can investigate the provider.

Provider acceptance evidence must cover successful login, MFA, recovery,
cancelled login, invalid/expired state, refresh rotation, revoked membership,
remote logout/revocation, and a second login after local credential deletion on
each supported operating system.

## First administrator

After migrations, run `/app/trajectory-bootstrap-admin` as a one-off Cloud Run
job. Supply these values through managed job configuration; the database URL is
a secret and the other fields are operational configuration:

```text
TRAJECTORY_BOOTSTRAP_ADMIN_ISSUER
TRAJECTORY_BOOTSTRAP_ADMIN_SUBJECT
TRAJECTORY_BOOTSTRAP_ADMIN_EMAIL
TRAJECTORY_BOOTSTRAP_ADMIN_DISPLAY_NAME
TRAJECTORY_BOOTSTRAP_ORGANIZATION_NAME
```

The command is idempotent for the issuer/subject pair. It creates or reuses the
organization, grants the internal account an administrator membership, and
writes an append-only audit event.

## Professional invitation flow

1. An organization administrator calls `POST /v1/invitations` with an email,
   role, organization, and lifetime of at most seven days.
2. The API stores only SHA-256 of a random 256-bit invitation token and returns
   the clear token once for delivery through the approved communication system.
3. The recipient authenticates at the OpenID provider and calls
   `POST /v1/invitations/accept` with the one-time token.
4. The API requires a provider-verified email matching the invitation, consumes
   the token atomically, creates the internal account and membership, and, for
   contributors, creates the contributor profile.
5. `GET /v1/me` returns the internal account, contributor profile, and active
   organization memberships needed by the user interface.

Unknown identities fail closed everywhere except the invitation-acceptance
endpoint. Used, revoked, mismatched, unverified, and expired invitations return
the same generic error so the endpoint does not disclose account state.

## Authorization boundary

Production requests never use `X-Actor-ID`. The API enforces roles and
organization ownership for projects, sessions, processing jobs, reviews,
releases, and contributor assignment history. Local development keeps the
header-based adapter only so existing offline fixtures remain easy to run.
