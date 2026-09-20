# Production readiness record

The authoritative machine-readable gate ledger is
[`production-readiness.json`](production-readiness.json). It prevents a local
build, synthetic soak, or unsigned package from being mistaken for a production
release. Every production gate starts open and may move to `complete` only when
its repeatable evidence is attached to the reviewed release record.

Run:

```bash
npm run readiness:production
npm run readiness:production -- --json
npm run readiness:production -- --assert-ready
```

The normal command validates the ledger and its local evidence paths, then
reports all open gates. `--assert-ready` is the promotion guard: it exits with a
failure until every gate is complete. Do not weaken or bypass it in a release
workflow.

## Evidence rules

- A source file or passing local test proves implementation, not deployment.
- A hosted CI URL must identify the reviewed commit and show every mandatory job.
- Cloud evidence records resource identifiers, environment, configuration
  digests, test time, result, and operator; it never contains secret values.
- Physical collector evidence records the signed artifact digest, OS/hardware,
  permissions tested, capture mode, recovery cases, and tester.
- Legal, privacy, security, incident, and support documents name their approving
  owner and effective version before the gate can close.
- Staging and pilot results contain aggregate metrics and sanitized failure
  references, never contributor credentials or raw sensitive content.

## Promotion rule

The release operator runs `npm run readiness:production -- --assert-ready` from
the exact reviewed commit. Production promotion is permitted only when that
command succeeds and the recorded artifact digests match the deployed API,
worker, web interface, and all three signed collector packages.

At present the ledger intentionally reports zero completed production gates.
The repository contains substantial local engineering evidence, but cloud
provisioning, provider decisions, named approvals, signed physical-platform
tests, the real staging soak, and the controlled pilot remain required.
