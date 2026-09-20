# Linux collector engineering packages — 2026-09-14

This record proves that the DataTrains Collector can be packaged on the current
Linux development host. It is engineering evidence only. The artifacts are not
signed and have not completed the clean-machine production acceptance gate.

## Build host

- Operating system: Ubuntu 24.04.3 LTS (Noble Numbat)
- Architecture: x86_64
- Desktop session: X11, Ubuntu GNOME
- Collector version: 0.1.0

## Reproducible commands

```text
npm run collector:package:linux
npm run collector:package:linux:appimage
```

`scripts/package-collector-linux.sh` uses normal system dependencies when they
are installed. On this development host it discovers the ignored, repository-
local Rust, compiler, GTK/WebKit, OpenSSL, and pkgconf toolchain. The AppImage
path additionally stages matching host GTK runtime modules into that ignored
dependency root for `linuxdeploy`.

## Produced artifacts

| Artifact | Bytes | SHA-256 |
| --- | ---: | --- |
| `DataTrains Collector_0.1.0_amd64.deb` | 6,759,368 | `be96fb56bc7caa2f274346233152d32ee400201e349d3c7e89b5e7f23697c3f7` |
| `DataTrains Collector_0.1.0_amd64.AppImage` | 80,947,704 | `f90ed5c4df5fa1f43f4377b19110e5d813a28a1e2918bd53a46a2205218d3210` |

The Debian control archive reports package `data-trains-collector`, version
`0.1.0`, architecture `amd64`, and runtime dependencies on GTK 3 and WebKitGTK
4.1. The packaged executable has no unresolved dynamic libraries on this build
host. The AppImage is a static PIE launcher containing the assembled application
filesystem.

## Input integrity

- `package-lock.json`: `64db24a630e674a0da34ed17707309ee25163306d557036a8f6e20ac43ad8a43`
- `apps/collector/src-tauri/Cargo.lock`: `ecf16be75c867c87db49345d9bc90cff63ab68503ee341591f8d8e713768222e`
- `scripts/package-collector-linux.sh`: `74bf568cf7bddd469b53845551d73b98116e574ae42dab01a62ecd12144f4cca`

## Remaining production proof

- Embed the final production API and Auth0 public-client values from a reviewed
  release commit.
- Sign the selected Linux distribution artifact and publish it through the
  approved update channel.
- Install on clean supported Ubuntu machines and record Xorg capture, Wayland
  capability/fail-closed behavior, permission denial, crash recovery, upgrade,
  rollback, and uninstall results.

