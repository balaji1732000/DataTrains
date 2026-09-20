# Reviewed video and redaction

DataTrains has two release profiles. `trajectory_only` is the safe default and
never includes screen recordings or submitted output binaries. The optional
`redacted_video` profile includes only worker-produced video derived from an
explicit reviewer decision for every source segment. Raw recordings always
remain private review evidence.

## Reviewer flow

When accepting a PII-approved trajectory, the reviewer chooses one of these
paths:

1. **Keep recordings private.** No redaction plan is created and only a
   trajectory-only release can include the session.
2. **Prepare reviewed video.** Every source segment must be explicitly approved
   as shown or assigned one or more black-mask rectangles with a bounded time
   interval and reason.

Acceptance and the optional redaction plan are committed in one serializable
database transaction. A missing segment, invalid rectangle, pending erasure,
or failed PII gate rolls back the whole decision. Plans use `redaction/v1`, are
canonicalized and SHA-256 hashed, and cannot be updated or deleted except by
the governed session-erasure transaction. A later correction creates a new
version.

## Worker and publication boundary

The worker leases a durable redaction job only while the session is accepted.
For each segment it:

- re-inspects the raw object with bounded `ffprobe`;
- confirms every mask begins within the media duration (a safe end-time
  overhang continues the mask through the final frame);
- applies deterministic opaque black rectangles with bundled `ffmpeg`;
- removes audio and encodes H.264/yuv420p MP4;
- writes only under the plan-specific `derived/` prefix; and
- records dimensions, duration, size, SHA-256, source decision, and an output
  manifest tied to the immutable plan hash.

Publication fails closed unless every accepted session has a completed,
unpurged plan and every output exactly matches its approved segment, source
key, decision, object path, byte size, and SHA-256. The release exporter copies
only those derived files, records their exact inventory, and leaves every
`raw/` object outside the release. The standalone validator rejects unexpected
video paths, media types, bytes without an MP4 header, missing sessions, and
checksum or inventory differences.

## Recovery, retention, and erasure

Automatic retries are bounded. Exhausted redaction work appears in the
organization-scoped administrator recovery queue and can be retried only by an
audited manual action. Session erasure cannot start while redaction is queued
or leased. Completed or partially written dead-letter output paths are included
in the erasure inventory. Automatic derived-data expiry deletes redaction
videos and manifests and writes a `purged_at` tombstone, which prevents later
publication.

Do not enable `redacted_video` in production until staging evidence proves
mask placement, multi-segment coverage, retry recovery, retention, erasure,
release validation, and reviewer training using representative recordings.
Rectangular manual masks are a V1 control; automated detection is not a
substitute for human review.
