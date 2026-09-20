# Production collector release

DataTrains ships one native collector identity and protocol across Windows,
macOS, and Linux. A package is a production candidate only when it is built from
a tagged, reviewed commit with the public OpenID and API settings embedded at
compile time. No OpenID client secret, Cloudflare credential, database URL, or
Google credential belongs in a collector package.

## Release inputs

The release operator records the commit and supplies:

```text
DATATRAINS_AUTH_MODE=oidc
DATATRAINS_API_URL=https://API_DOMAIN
DATATRAINS_OIDC_ISSUER=https://OIDC_ISSUER
DATATRAINS_OIDC_CLIENT_ID=NATIVE_PUBLIC_CLIENT_ID
DATATRAINS_OIDC_AUDIENCE=datatrains-api
DATATRAINS_OIDC_REVOCATION_URL=https://OIDC_ISSUER/oauth/revoke
```

The same API, issuer, audience, and public native-client ID must be embedded in
all packages for one environment. Staging and production use different OpenID
clients and endpoints. Keep signing credentials in the release service's
protected secret store.

## Platform artifacts and gates

| Platform | Candidate artifact | Required release proof |
| --- | --- | --- |
| Windows 10/11 | Signed NSIS `.exe` | Publisher signature, clean install/uninstall, Credential Manager session, capture and denial tests, crash/upload recovery, upgrade and rollback |
| macOS | Signed and notarized universal or separately declared `.dmg` | Gatekeeper acceptance, Keychain session, Screen Recording and Accessibility onboarding/denial, capture recovery, upgrade and rollback |
| Ubuntu Linux | Signed `.deb`; AppImage may be an additional channel | Package signature/repository decision, Secret Service session, Xorg capture acceptance, explicit Wayland fail-closed result where telemetry is unavailable, upgrade and rollback |

Unsigned CI artifacts are engineering evidence only and must never be presented
as production downloads. Publishing remains disabled until every platform's
physical checklist is attached to the release record.

## Candidate procedure

1. Run JavaScript, Go, Rust, PostgreSQL integration, vulnerability, and package
   checks from a clean checkout.
2. Build each platform on its native runner with the production public settings
   above embedded in the binary.
3. Sign Windows and Linux artifacts; sign and notarize the macOS artifact.
4. Record SHA-256 digests, source commit, dependency lockfile digests, signing
   identity, OpenID client ID, API origin, and build runner image.
5. Install on a clean supported machine and execute the platform acceptance
   checklist, including credential deletion and remote token revocation.
6. Promote the exact tested digests to the download channel. Rollback restores
   the previous signed digests; it never rebuilds an older tag.

The initial production release remains blocked until the OpenID provider,
signing identities, supported OS versions, and update channel have been approved.
