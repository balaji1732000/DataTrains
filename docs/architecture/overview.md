# V1 architecture

The control plane stores relational workflow state in PostgreSQL. Binary capture artifacts are addressed by logical keys through a `BlobStore`; the V1 adapter maps those keys under `data/blobstore` without exposing physical paths to domain code.

Raw session artifacts are immutable. Workers read `raw/sessions/<id>/...` and write normalized representations under `derived/sessions/<id>/...`. Dataset releases are deterministic products under `releases/<id>/...`.

The collector always shows recording state, requires versioned consent and preflight checks, and never captures the clipboard by default.

