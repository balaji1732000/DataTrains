# GitHub production repository — 2026-09-20

This record contains repository and workflow identifiers only. No GitHub token,
OAuth secret, production credential, or captured contributor artifact is
included.

## Repository

- Owner: `balaji1732000`
- Repository: `balaji1732000/DataTrains`
- Visibility: private
- Default branch: `main`
- Initial production commit: `1c4cdfe8cdd8bc6a15c1358df1033cd20570c82a`
- Clean-workspace build-order fix: `8ef9ace5d81b9f4d60ea6bf837725c8f3542b28a`
- Clean-CI portability fix: `4e3ffcb`
- macOS arm64 Accessibility-link fix: `5e876c1`

The initial source upload excluded environment files, generated build outputs,
local PostgreSQL state, recorded sessions, dependency trees, Vercel state, and
the local production runtime configuration. A pre-push scan found no known
confidential production values or high-confidence private-key/token patterns in
the committed files.

## Hosted workflows

- General validation: `.github/workflows/ci.yml`
- Native collector candidates:
  `.github/workflows/collector-production-candidate.yml`
- First candidate run: GitHub Actions run `35511832173`; failed consistently on
  every platform because the local trajectory-schema package was not built
  before clean-workspace type checking.
- Corrected candidate run: GitHub Actions run `35511945802`, commit `8ef9ace`;
  completed unsuccessfully. Its macOS arm64 job passed JavaScript checks and
  Rust tests, then failed while linking three Accessibility `kAX*` CFString
  globals. Linux and Windows did not start because GitHub reported a failed
  account payment or an Actions spending-limit restriction.
- The macOS source fix creates the three Accessibility attribute CFStrings at
  runtime, avoiding a dependency on SDK globals that are not exported by the
  current arm64 runner framework.
- Replacement candidate run: GitHub Actions run `35514055678`, commit
  `5e876c1`; queued at the time of this evidence update. The run is expected to
  remain unable to start Linux and Windows until the repository owner's GitHub
  Actions billing or spending-limit restriction is resolved.

Unsigned workflow artifacts remain engineering candidates. Branch protection,
required review, signed Windows and Linux packages, macOS signing/notarization,
and physical clean-machine acceptance are separate release gates.
