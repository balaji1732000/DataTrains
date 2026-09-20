mod event;
mod policy;
mod recorder;
mod spool;

pub use event::{InputEventKind, TimedInputEvent};
pub use policy::{CaptureContext, PolicyDecision, RecordingPolicy};
pub use recorder::{CaptureSource, FfmpegSegmentFactory, SegmentedRecorder};
pub use spool::{
    ArtifactManifestEntry, CaptureManifest, ConsentProof, PreflightCheck, PreflightReport,
    RecoveredSession, SessionSpec, SessionSpool, SpoolError, SpoolStage, VideoSegment,
    discover_pending_submissions, discover_recoverable_sessions,
};
