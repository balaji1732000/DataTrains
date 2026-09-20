# Windows V1 acceptance

This is the final physical acceptance run for the Windows-first collector. The automated suite proves the control plane, worker, review, exporter, validator, collector core, and Windows executable build. This checklist proves the operating-system integration that cannot be exercised on the Linux development host.

## Test host

- Windows 11 with current WebView2 Runtime.
- FFmpeg available as `ffmpeg.exe` on `PATH`, including `gdigrab` and `libx264`.
- Node.js 24, Go 1.27, Rust stable, and Docker Desktop for building the complete local stack.
- A controlled test application and synthetic input/output files. Do not use personal accounts or real credentials.

## Build the collector

From PowerShell at the repository root:

```powershell
npm ci
npm run collector:package:windows
```

The unsigned V1 installer is written under `apps/collector/src-tauri/target/release/bundle/nsis/`. CI also publishes this directory as the `trajectory-collector-windows` artifact.

## Run the local services

Start PostgreSQL:

```powershell
docker compose -f infra/docker/compose.yaml up -d
```

Open three PowerShell windows at the repository root and set the same environment in each:

```powershell
$env:TRAJECTORY_DATABASE_URL = "postgres://trajectory:trajectory_local_only@127.0.0.1:5432/trajectory?sslmode=disable"
```

Then run one command per window:

```powershell
scripts/run-go.sh run ./cmd/api
scripts/run-go.sh run ./cmd/worker
npm run admin:dev
```

If Git Bash is unavailable, run the Go commands from `services/api` with `go run ./cmd/api` and `go run ./cmd/worker`.

## Physical acceptance checklist

- [ ] Create a project, template, task, contributor, and assignment in the admin dashboard.
- [ ] Confirm the collector displays the task goal, process name, input assets, expected output, capture signals, privacy warning, and finish criteria.
- [ ] Accept the current versioned consent document and pass every native preflight check.
- [ ] Record at least 60 seconds in the allowlisted test application and confirm the recording indicator remains visible.
- [ ] Generate mouse movement, clicks, scrolling, and key down/up events without entering sensitive text.
- [ ] Focus a denied application or password-titled window and confirm recording pauses automatically.
- [ ] Resume, manually pause, and resume again without losing the completed segment.
- [ ] Terminate the collector during a segment, reopen it, recover the session paused, and continue recording.
- [ ] Attach the expected output, finish, and submit. Restart once after sealing to verify upload retry discovery.
- [ ] Confirm the worker advances the session to `READY_FOR_REVIEW`.
- [ ] Replay each video segment in the admin dashboard and jump from actions to matching video times.
- [ ] Verify output evidence, complete the rubric, pass the PII gate, and accept the session.
- [ ] Create a release and download `manifest.json`, `README.md`, `schema.json`, `checksums.sha256`, and `trajectories.jsonl`.
- [ ] Run `npm run validate:release -- <release-directory>` and receive `VALID RELEASE`.
- [ ] Close the collector while recording and verify no `ffmpeg.exe` process remains.

Record the Windows version, FFmpeg version, collector commit, failures, and release ID with the test result.
