# Linux production-candidate packages — 2026-09-20

The Linux packages were built locally on the Linux development host with the
production Cloud Run API, Auth0 issuer, API audience, native public client ID,
GitHub connection name, and revocation endpoint embedded at compile time. No
confidential credential is embedded.

| Artifact | Size | SHA-256 |
| --- | ---: | --- |
| `DataTrains Collector_0.1.0_amd64.deb` | 6,760,244 bytes | `da8cb5108ea6d0e231bae5cc80131545fe5e49cc016bda84085baa7422787feb` |
| `DataTrains Collector_0.1.0_amd64.AppImage` | 80,951,800 bytes | `d0545f54a6eef2003d74b3ffca986f23d4f0a3a3e5189e4a33033af46db76b8a` |

Before packaging, all TypeScript checks, ESLint checks, and JavaScript unit
tests passed. Native Rust tests and Clippy also passed. Binary inspection
confirmed the intended public production API, issuer, audience, GitHub
connection, revocation endpoint, and native client ID.

These files are unsigned production candidates. They must not be promoted as a
public production download until package signing, clean-install verification,
Xorg capture acceptance, Wayland fail-closed verification, upgrade, rollback,
and uninstall evidence are complete.
