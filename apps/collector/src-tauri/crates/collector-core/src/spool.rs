use crate::{CaptureContext, InputEventKind, PolicyDecision, RecordingPolicy, TimedInputEvent};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::fmt::{Display, Formatter};
use std::fs::{self, File, OpenOptions};
use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::time::{SystemTime, UNIX_EPOCH};

const REQUIRED_PREFLIGHT_CHECKS: [&str; 6] = [
    "screen_capture",
    "input_capture",
    "disk_space",
    "required_application",
    "output_folder",
    "collector_version",
];

#[derive(Debug)]
pub enum SpoolError {
    Io(std::io::Error),
    Serialization(serde_json::Error),
    InvalidIdentifier(String),
    InvalidConsent(String),
    PreflightFailed(Vec<String>),
    UnsafePolicy,
    InvalidStage {
        from: SpoolStage,
        operation: &'static str,
    },
    PolicyDenied(PolicyDecision),
    NonMonotonicEvent {
        previous: u64,
        next: u64,
    },
    InvalidSegment(String),
    InvalidOutput(String),
    Process(String),
}

impl Display for SpoolError {
    fn fmt(&self, formatter: &mut Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Io(error) => write!(formatter, "I/O error: {error}"),
            Self::Serialization(error) => write!(formatter, "serialization error: {error}"),
            Self::InvalidIdentifier(value) => write!(formatter, "invalid identifier: {value}"),
            Self::InvalidConsent(reason) => write!(formatter, "invalid consent: {reason}"),
            Self::PreflightFailed(checks) => {
                write!(formatter, "preflight failed: {}", checks.join(", "))
            }
            Self::UnsafePolicy => {
                write!(formatter, "recording policy violates V1 safety invariants")
            }
            Self::InvalidStage { from, operation } => {
                write!(formatter, "cannot {operation} while session is {from:?}")
            }
            Self::PolicyDenied(decision) => {
                write!(formatter, "capture denied by policy: {decision:?}")
            }
            Self::NonMonotonicEvent { previous, next } => write!(
                formatter,
                "event timestamp moved backwards: {previous} -> {next}"
            ),
            Self::InvalidSegment(reason) => write!(formatter, "invalid video segment: {reason}"),
            Self::InvalidOutput(reason) => write!(formatter, "invalid output: {reason}"),
            Self::Process(reason) => write!(formatter, "capture process error: {reason}"),
        }
    }
}

impl std::error::Error for SpoolError {}
impl From<std::io::Error> for SpoolError {
    fn from(value: std::io::Error) -> Self {
        Self::Io(value)
    }
}
impl From<serde_json::Error> for SpoolError {
    fn from(value: serde_json::Error) -> Self {
        Self::Serialization(value)
    }
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct SessionSpec {
    pub session_id: String,
    pub task_id: String,
    pub required_application: String,
    pub client_version: String,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct ConsentProof {
    pub document_id: String,
    pub version: String,
    pub text_hash: String,
    pub accepted_at_unix_ms: u64,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct PreflightCheck {
    pub name: String,
    pub passed: bool,
    pub detail: String,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct PreflightReport {
    pub checked_at_unix_ms: u64,
    pub checks: Vec<PreflightCheck>,
}

impl PreflightReport {
    pub fn failures(&self) -> Vec<String> {
        let mut failures = Vec::new();
        for required in REQUIRED_PREFLIGHT_CHECKS {
            match self.checks.iter().find(|check| check.name == required) {
                Some(check) if check.passed => {}
                Some(_) => failures.push(required.to_string()),
                None => failures.push(format!("{required}:missing")),
            }
        }
        failures
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum SpoolStage {
    Ready,
    Recording,
    Paused,
    Finalizing,
    Finalized,
    Failed,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct VideoSegment {
    pub segment_id: String,
    pub path: String,
    pub start_ns: u64,
    pub end_ns: u64,
    pub size: u64,
    pub sha256: String,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
struct ActiveVideoSegment {
    index: u32,
    start_ns: u64,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
struct PersistedState {
    schema_version: String,
    spec: SessionSpec,
    consent: ConsentProof,
    preflight: PreflightReport,
    policy: RecordingPolicy,
    stage: SpoolStage,
    created_at_unix_ms: u64,
    started_at_unix_ms: Option<u64>,
    elapsed_ns: u64,
    last_event_ns: Option<u64>,
    active_video_segment: Option<ActiveVideoSegment>,
    video_segments: Vec<VideoSegment>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct ArtifactManifestEntry {
    pub path: String,
    pub size: u64,
    pub sha256: String,
    pub media_type: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct CaptureManifest {
    pub schema_version: String,
    pub session_id: String,
    pub task_id: String,
    pub client_version: String,
    pub started_at_unix_ms: u64,
    pub duration_ns: u64,
    pub consent: ConsentProof,
    pub privacy: RecordingPolicy,
    pub video_segments: Vec<VideoSegment>,
    pub artifacts: Vec<ArtifactManifestEntry>,
}

pub struct SessionSpool {
    directory: PathBuf,
    state: PersistedState,
}

impl SessionSpool {
    pub fn create(
        root: impl AsRef<Path>,
        spec: SessionSpec,
        consent: ConsentProof,
        preflight: PreflightReport,
        policy: RecordingPolicy,
    ) -> Result<Self, SpoolError> {
        validate_identifier(&spec.session_id)?;
        validate_identifier(&spec.task_id)?;
        validate_consent(&consent)?;
        let failures = preflight.failures();
        if !failures.is_empty() {
            return Err(SpoolError::PreflightFailed(failures));
        }
        if policy.clipboard_capture
            || !policy.recording_indicator_required
            || !policy.task_scoped
            || policy.allowed_applications.is_empty()
        {
            return Err(SpoolError::UnsafePolicy);
        }
        let directory = root.as_ref().join("sessions").join(&spec.session_id);
        fs::create_dir_all(directory.parent().expect("session directory has parent"))?;
        fs::create_dir(&directory)?;
        fs::create_dir(directory.join("video"))?;
        fs::create_dir(directory.join("events"))?;
        fs::create_dir(directory.join("outputs"))?;
        let state = PersistedState {
            schema_version: "spool/v1".into(),
            spec,
            consent,
            preflight,
            policy,
            stage: SpoolStage::Ready,
            created_at_unix_ms: unix_ms()?,
            started_at_unix_ms: None,
            elapsed_ns: 0,
            last_event_ns: None,
            active_video_segment: None,
            video_segments: Vec::new(),
        };
        let spool = Self { directory, state };
        spool.persist_state()?;
        Ok(spool)
    }

    pub fn open(directory: impl AsRef<Path>) -> Result<Self, SpoolError> {
        let directory = directory.as_ref().to_path_buf();
        let state = serde_json::from_reader(File::open(directory.join("state.json"))?)?;
        Ok(Self { directory, state })
    }

    pub fn directory(&self) -> &Path {
        &self.directory
    }
    pub fn stage(&self) -> SpoolStage {
        self.state.stage
    }
    pub fn session_id(&self) -> &str {
        &self.state.spec.session_id
    }
    pub fn elapsed_ns(&self) -> u64 {
        self.state.elapsed_ns
    }
    pub fn policy(&self) -> &RecordingPolicy {
        &self.state.policy
    }

    pub fn checkpoint_elapsed(&mut self, elapsed_ns: u64) -> Result<(), SpoolError> {
        if !matches!(self.state.stage, SpoolStage::Recording | SpoolStage::Paused) {
            return Err(SpoolError::InvalidStage {
                from: self.state.stage,
                operation: "checkpoint elapsed time",
            });
        }
        if elapsed_ns < self.state.elapsed_ns {
            return Err(SpoolError::NonMonotonicEvent {
                previous: self.state.elapsed_ns,
                next: elapsed_ns,
            });
        }
        self.state.elapsed_ns = elapsed_ns;
        self.persist_state()
    }

    pub fn prepare_recovery(&mut self) -> Result<(), SpoolError> {
        if self.state.stage == SpoolStage::Finalized {
            return Err(SpoolError::InvalidStage {
                from: self.state.stage,
                operation: "recover finalized session",
            });
        }
        if let Some(active) = self.state.active_video_segment.clone() {
            let path = self
                .directory
                .join("video")
                .join(format!("{:06}.mp4", active.index));
            if path.exists() && fs::metadata(&path)?.len() > 0 {
                let end_ns = self.state.elapsed_ns.max(active.start_ns.saturating_add(1));
                self.finish_video_segment(end_ns)?;
            } else {
                self.abort_video_segment()?;
            }
        }
        self.state.stage = SpoolStage::Paused;
        self.persist_state()
    }

    pub fn start(&mut self) -> Result<(), SpoolError> {
        self.require_stage(SpoolStage::Ready, "start recording")?;
        self.state.stage = SpoolStage::Recording;
        self.state.started_at_unix_ms = Some(unix_ms()?);
        self.persist_state()
    }

    pub fn pause(&mut self, elapsed_ns: u64) -> Result<(), SpoolError> {
        self.require_stage(SpoolStage::Recording, "pause recording")?;
        if self.state.active_video_segment.is_some() {
            return Err(SpoolError::InvalidSegment(
                "active segment must be stopped before pausing".into(),
            ));
        }
        self.state.elapsed_ns = elapsed_ns;
        self.state.stage = SpoolStage::Paused;
        self.persist_state()
    }

    pub fn resume(&mut self) -> Result<(), SpoolError> {
        self.require_stage(SpoolStage::Paused, "resume recording")?;
        self.state.stage = SpoolStage::Recording;
        self.persist_state()
    }

    pub fn begin_video_segment(&mut self, start_ns: u64) -> Result<(u32, PathBuf), SpoolError> {
        self.require_stage(SpoolStage::Recording, "begin video segment")?;
        if self.state.active_video_segment.is_some() {
            return Err(SpoolError::InvalidSegment(
                "a video segment is already active".into(),
            ));
        }
        let index = self.state.video_segments.len() as u32 + 1;
        self.state.active_video_segment = Some(ActiveVideoSegment { index, start_ns });
        self.persist_state()?;
        let filename = format!("{index:06}.mp4");
        let event_path = self
            .directory
            .join("events")
            .join(format!("{index:06}.jsonl"));
        OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(event_path)?
            .sync_all()?;
        Ok((index, self.directory.join("video").join(filename)))
    }

    pub fn abort_video_segment(&mut self) -> Result<(), SpoolError> {
        let active = self
            .state
            .active_video_segment
            .take()
            .ok_or_else(|| SpoolError::InvalidSegment("no active segment".into()))?;
        let _ = fs::remove_file(
            self.directory
                .join("events")
                .join(format!("{:06}.jsonl", active.index)),
        );
        let _ = fs::remove_file(
            self.directory
                .join("video")
                .join(format!("{:06}.mp4", active.index)),
        );
        self.persist_state()
    }

    pub fn finish_video_segment(&mut self, end_ns: u64) -> Result<VideoSegment, SpoolError> {
        let active = self
            .state
            .active_video_segment
            .take()
            .ok_or_else(|| SpoolError::InvalidSegment("no active segment".into()))?;
        if end_ns <= active.start_ns {
            self.state.active_video_segment = Some(active);
            return Err(SpoolError::InvalidSegment(
                "segment end must be after its start".into(),
            ));
        }
        let relative = format!("video/{:06}.mp4", active.index);
        let path = self.directory.join(&relative);
        let metadata = fs::metadata(&path)?;
        if metadata.len() == 0 {
            self.state.active_video_segment = Some(active);
            return Err(SpoolError::InvalidSegment("video file is empty".into()));
        }
        let segment = VideoSegment {
            segment_id: format!("{:06}", active.index),
            path: relative,
            start_ns: active.start_ns,
            end_ns,
            size: metadata.len(),
            sha256: sha256_file(&path)?,
        };
        self.state.elapsed_ns = end_ns;
        self.state.video_segments.push(segment.clone());
        self.persist_state()?;
        Ok(segment)
    }

    pub fn record_event(
        &mut self,
        timestamp_ns: u64,
        context: CaptureContext,
        event: InputEventKind,
    ) -> Result<(), SpoolError> {
        self.require_stage(SpoolStage::Recording, "record input event")?;
        let decision = self.state.policy.evaluate(&context);
        if decision != PolicyDecision::Allowed {
            return Err(SpoolError::PolicyDenied(decision));
        }
        if let Some(previous) = self.state.last_event_ns
            && timestamp_ns < previous
        {
            return Err(SpoolError::NonMonotonicEvent {
                previous,
                next: timestamp_ns,
            });
        }
        let active = self.state.active_video_segment.as_ref().ok_or_else(|| {
            SpoolError::InvalidSegment("input events require an active video segment".into())
        })?;
        let timed = TimedInputEvent {
            timestamp_ns,
            application: context.application,
            event,
        };
        let mut encoded = serde_json::to_vec(&timed)?;
        encoded.push(b'\n');
        let event_path = self
            .directory
            .join("events")
            .join(format!("{:06}.jsonl", active.index));
        let mut file = OpenOptions::new().append(true).open(event_path)?;
        file.write_all(&encoded)?;
        file.sync_data()?;
        self.state.last_event_ns = Some(timestamp_ns);
        self.state.elapsed_ns = self.state.elapsed_ns.max(timestamp_ns);
        self.persist_state()
    }

    pub fn begin_finalization(&mut self, elapsed_ns: u64) -> Result<(), SpoolError> {
        if !matches!(self.state.stage, SpoolStage::Recording | SpoolStage::Paused) {
            return Err(SpoolError::InvalidStage {
                from: self.state.stage,
                operation: "begin finalization",
            });
        }
        if self.state.active_video_segment.is_some() {
            return Err(SpoolError::InvalidSegment(
                "active segment must be stopped before finalization".into(),
            ));
        }
        self.state.elapsed_ns = elapsed_ns;
        self.state.stage = SpoolStage::Finalizing;
        self.persist_state()
    }

    pub fn finalize(&mut self, output_files: &[PathBuf]) -> Result<CaptureManifest, SpoolError> {
        self.require_stage(SpoolStage::Finalizing, "finalize session")?;
        if self.state.video_segments.is_empty() {
            return Err(SpoolError::InvalidSegment(
                "at least one video segment is required".into(),
            ));
        }
        if let Some(last) = self.state.last_event_ns
            && last > self.state.elapsed_ns
        {
            return Err(SpoolError::NonMonotonicEvent {
                previous: self.state.elapsed_ns,
                next: last,
            });
        }
        let mut artifacts = Vec::new();
        for segment in &self.state.video_segments {
            artifacts.push(artifact_for(&self.directory, &segment.path, "video/mp4")?);
            let event_path = format!("events/{}.jsonl", segment.segment_id);
            ensure_nonempty_jsonl(&self.directory.join(&event_path))?;
            artifacts.push(artifact_for(
                &self.directory,
                &event_path,
                "application/x-ndjson",
            )?);
        }
        for source in output_files {
            let filename = safe_output_name(source)?;
            let relative = format!("outputs/{filename}");
            let destination = self.directory.join(&relative);
            fs::copy(source, &destination)?;
            let media_type = media_type_for(&filename);
            artifacts.push(artifact_for(&self.directory, &relative, media_type)?);
        }
        artifacts.sort_by(|left, right| left.path.cmp(&right.path));
        let manifest = CaptureManifest {
            schema_version: "capture/v1".into(),
            session_id: self.state.spec.session_id.clone(),
            task_id: self.state.spec.task_id.clone(),
            client_version: self.state.spec.client_version.clone(),
            started_at_unix_ms: self
                .state
                .started_at_unix_ms
                .unwrap_or(self.state.created_at_unix_ms),
            duration_ns: self.state.elapsed_ns,
            consent: self.state.consent.clone(),
            privacy: self.state.policy.clone(),
            video_segments: self.state.video_segments.clone(),
            artifacts,
        };
        atomic_json(&self.directory.join("manifest.json"), &manifest)?;
        self.state.stage = SpoolStage::Finalized;
        self.persist_state()?;
        Ok(manifest)
    }

    fn require_stage(
        &self,
        expected: SpoolStage,
        operation: &'static str,
    ) -> Result<(), SpoolError> {
        if self.state.stage != expected {
            return Err(SpoolError::InvalidStage {
                from: self.state.stage,
                operation,
            });
        }
        Ok(())
    }

    fn persist_state(&self) -> Result<(), SpoolError> {
        atomic_json(&self.directory.join("state.json"), &self.state)
    }
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct RecoveredSession {
    pub session_id: String,
    pub task_id: String,
    pub stage: SpoolStage,
    pub elapsed_ns: u64,
    pub directory: PathBuf,
}

pub fn discover_recoverable_sessions(
    root: impl AsRef<Path>,
) -> Result<Vec<RecoveredSession>, SpoolError> {
    let sessions = root.as_ref().join("sessions");
    if !sessions.exists() {
        return Ok(Vec::new());
    }
    let mut recovered = Vec::new();
    for entry in fs::read_dir(sessions)? {
        let directory = entry?.path();
        if !directory.is_dir() || !directory.join("state.json").exists() {
            continue;
        }
        let spool = SessionSpool::open(&directory)?;
        if spool.stage() != SpoolStage::Finalized {
            recovered.push(RecoveredSession {
                session_id: spool.state.spec.session_id.clone(),
                task_id: spool.state.spec.task_id.clone(),
                stage: spool.stage(),
                elapsed_ns: spool.elapsed_ns(),
                directory,
            });
        }
    }
    recovered.sort_by(|left, right| left.session_id.cmp(&right.session_id));
    Ok(recovered)
}

pub fn discover_pending_submissions(
    root: impl AsRef<Path>,
) -> Result<Vec<RecoveredSession>, SpoolError> {
    let sessions = root.as_ref().join("sessions");
    if !sessions.exists() {
        return Ok(Vec::new());
    }
    let mut pending = Vec::new();
    for entry in fs::read_dir(sessions)? {
        let directory = entry?.path();
        if !directory.is_dir()
            || !directory.join("state.json").exists()
            || !directory.join("manifest.json").exists()
            || directory.join("submission.json").exists()
        {
            continue;
        }
        let spool = SessionSpool::open(&directory)?;
        if spool.stage() == SpoolStage::Finalized {
            pending.push(RecoveredSession {
                session_id: spool.state.spec.session_id.clone(),
                task_id: spool.state.spec.task_id.clone(),
                stage: spool.stage(),
                elapsed_ns: spool.elapsed_ns(),
                directory,
            });
        }
    }
    pending.sort_by(|left, right| left.session_id.cmp(&right.session_id));
    Ok(pending)
}

fn validate_identifier(value: &str) -> Result<(), SpoolError> {
    if value.is_empty()
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
    {
        return Err(SpoolError::InvalidIdentifier(value.into()));
    }
    Ok(())
}

fn validate_consent(consent: &ConsentProof) -> Result<(), SpoolError> {
    if consent.document_id.is_empty()
        || consent.version.is_empty()
        || consent.accepted_at_unix_ms == 0
    {
        return Err(SpoolError::InvalidConsent(
            "document, version, and acceptance timestamp are required".into(),
        ));
    }
    if consent.text_hash.len() != 64
        || !consent
            .text_hash
            .bytes()
            .all(|byte| byte.is_ascii_hexdigit())
    {
        return Err(SpoolError::InvalidConsent(
            "text_hash must be a SHA-256 hex digest".into(),
        ));
    }
    Ok(())
}

fn unix_ms() -> Result<u64, SpoolError> {
    let millis = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_err(|error| SpoolError::Process(error.to_string()))?
        .as_millis();
    u64::try_from(millis).map_err(|_| SpoolError::Process("system time exceeds u64".into()))
}

fn atomic_json(path: &Path, value: &impl Serialize) -> Result<(), SpoolError> {
    let temporary = path.with_extension("json.tmp");
    let mut file = OpenOptions::new()
        .create(true)
        .truncate(true)
        .write(true)
        .open(&temporary)?;
    serde_json::to_writer_pretty(&mut file, value)?;
    file.write_all(b"\n")?;
    file.sync_all()?;
    fs::rename(temporary, path)?;
    Ok(())
}

fn sha256_file(path: &Path) -> Result<String, SpoolError> {
    let mut file = File::open(path)?;
    let mut hasher = Sha256::new();
    let mut buffer = [0_u8; 64 * 1024];
    loop {
        let read = file.read(&mut buffer)?;
        if read == 0 {
            break;
        }
        hasher.update(&buffer[..read]);
    }
    Ok(format!("{:x}", hasher.finalize()))
}

fn artifact_for(
    root: &Path,
    relative: &str,
    media_type: &str,
) -> Result<ArtifactManifestEntry, SpoolError> {
    let path = root.join(relative);
    let size = fs::metadata(&path)?.len();
    if size == 0 {
        return Err(SpoolError::InvalidOutput(format!(
            "artifact is empty: {relative}"
        )));
    }
    Ok(ArtifactManifestEntry {
        path: relative.into(),
        size,
        sha256: sha256_file(&path)?,
        media_type: media_type.into(),
    })
}

fn ensure_nonempty_jsonl(path: &Path) -> Result<(), SpoolError> {
    if fs::metadata(path)?.len() == 0 {
        let mut file = OpenOptions::new().append(true).open(path)?;
        // A blank JSONL line represents an interval with no permitted input.
        // Readers already ignore blank lines, while artifact storage requires
        // every uploaded file to contain at least one byte.
        file.write_all(b"\n")?;
        file.sync_all()?;
    }
    Ok(())
}

fn safe_output_name(source: &Path) -> Result<String, SpoolError> {
    if !source.is_file() {
        return Err(SpoolError::InvalidOutput(format!(
            "output does not exist: {}",
            source.display()
        )));
    }
    let filename = source
        .file_name()
        .and_then(|name| name.to_str())
        .ok_or_else(|| SpoolError::InvalidOutput("output filename is invalid UTF-8".into()))?;
    if filename.is_empty() || filename == "." || filename == ".." {
        return Err(SpoolError::InvalidOutput("unsafe output filename".into()));
    }
    let extension = source
        .extension()
        .and_then(|value| value.to_str())
        .unwrap_or("")
        .to_lowercase();
    if matches!(
        extension.as_str(),
        "exe" | "dll" | "bat" | "cmd" | "sh" | "app" | "msi"
    ) {
        return Err(SpoolError::InvalidOutput(
            "executable outputs are forbidden".into(),
        ));
    }
    Ok(filename.into())
}

fn media_type_for(filename: &str) -> &'static str {
    match Path::new(filename)
        .extension()
        .and_then(|value| value.to_str())
        .unwrap_or("")
        .to_lowercase()
        .as_str()
    {
        "png" => "image/png",
        "jpg" | "jpeg" => "image/jpeg",
        "pdf" => "application/pdf",
        "json" => "application/json",
        "txt" => "text/plain",
        _ => "application/octet-stream",
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn creation_requires_every_preflight_check() {
        let root = temporary_directory("preflight");
        let mut report = passing_preflight();
        report.checks.retain(|check| check.name != "input_capture");
        let result = SessionSpool::create(
            &root,
            spec("sess_preflight"),
            consent(),
            report,
            RecordingPolicy::safe_default("Photo Editor"),
        );
        assert!(
            matches!(result, Err(SpoolError::PreflightFailed(checks)) if checks == ["input_capture:missing"])
        );
        fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn events_are_monotonic_and_sensitive_contexts_are_not_written() {
        let root = temporary_directory("events");
        let mut spool = fixture_spool(&root, "sess_events");
        spool.start().unwrap();
        let (_, video) = spool.begin_video_segment(0).unwrap();
        spool
            .record_event(
                10,
                allowed_context(),
                InputEventKind::MouseMove { x: 10, y: 20 },
            )
            .unwrap();
        let denied = spool.record_event(
            11,
            CaptureContext {
                application: "Photo Editor".into(),
                window_title: "Enter password".into(),
            },
            InputEventKind::KeyDown { key: "A".into() },
        );
        assert!(matches!(
            denied,
            Err(SpoolError::PolicyDenied(
                PolicyDecision::DeniedSensitiveContext
            ))
        ));
        let backwards = spool.record_event(
            9,
            allowed_context(),
            InputEventKind::MouseUp {
                button: "left".into(),
            },
        );
        assert!(matches!(
            backwards,
            Err(SpoolError::NonMonotonicEvent {
                previous: 10,
                next: 9
            })
        ));
        fs::write(video, b"video").unwrap();
        spool.finish_video_segment(20).unwrap();
        let events =
            fs::read_to_string(root.join("sessions/sess_events/events/000001.jsonl")).unwrap();
        assert!(events.contains("mouse_move"));
        assert!(!events.contains("KeyDown"));
        assert!(!events.contains("password"));
        fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn finalization_hashes_segments_events_and_outputs_then_becomes_immutable() {
        let root = temporary_directory("finalize");
        let mut spool = fixture_spool(&root, "sess_finalize");
        spool.start().unwrap();
        let (_, video) = spool.begin_video_segment(0).unwrap();
        spool
            .record_event(
                5,
                allowed_context(),
                InputEventKind::MouseDown {
                    button: "left".into(),
                },
            )
            .unwrap();
        fs::write(video, b"video bytes").unwrap();
        spool.finish_video_segment(10).unwrap();
        spool.begin_finalization(10).unwrap();
        let output = root.join("final.png");
        fs::write(&output, b"png bytes").unwrap();
        let manifest = spool.finalize(&[output]).unwrap();
        assert_eq!(spool.stage(), SpoolStage::Finalized);
        assert_eq!(manifest.schema_version, "capture/v1");
        assert_eq!(manifest.artifacts.len(), 3);
        assert!(
            manifest
                .artifacts
                .iter()
                .all(|artifact| artifact.sha256.len() == 64 && artifact.size > 0)
        );
        assert!(root.join("sessions/sess_finalize/manifest.json").exists());
        assert!(matches!(
            spool.resume(),
            Err(SpoolError::InvalidStage {
                from: SpoolStage::Finalized,
                ..
            })
        ));
        fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn finalization_preserves_segments_without_input_as_blank_jsonl() {
        let root = temporary_directory("finalize-without-input");
        let mut spool = fixture_spool(&root, "sess_finalize_without_input");
        spool.start().unwrap();
        let (_, video) = spool.begin_video_segment(0).unwrap();
        fs::write(video, b"video bytes").unwrap();
        spool.finish_video_segment(10).unwrap();
        spool.begin_finalization(10).unwrap();
        let output = root.join("final.png");
        fs::write(&output, b"png bytes").unwrap();

        let manifest = spool.finalize(&[output]).unwrap();

        let event_path = root.join("sessions/sess_finalize_without_input/events/000001.jsonl");
        assert_eq!(fs::read(&event_path).unwrap(), b"\n");
        assert!(manifest.artifacts.iter().any(|artifact| {
            artifact.path == "events/000001.jsonl"
                && artifact.media_type == "application/x-ndjson"
                && artifact.size == 1
        }));
        fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn unfinished_sessions_are_discovered_after_restart() {
        let root = temporary_directory("recovery");
        let mut recoverable = fixture_spool(&root, "sess_recover");
        recoverable.start().unwrap();
        let mut finalized = fixture_spool(&root, "sess_done");
        finalized.start().unwrap();
        let (_, video) = finalized.begin_video_segment(0).unwrap();
        finalized
            .record_event(
                5,
                allowed_context(),
                InputEventKind::MouseUp {
                    button: "left".into(),
                },
            )
            .unwrap();
        fs::write(video, b"video").unwrap();
        finalized.finish_video_segment(10).unwrap();
        finalized.begin_finalization(10).unwrap();
        let output = root.join("done.png");
        fs::write(&output, b"png").unwrap();
        finalized.finalize(&[output]).unwrap();
        let recovered = discover_recoverable_sessions(&root).unwrap();
        assert_eq!(recovered.len(), 1);
        assert_eq!(recovered[0].session_id, "sess_recover");
        fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn finalized_sessions_remain_pending_until_submission_is_recorded() {
        let root = temporary_directory("pending-submission");
        let mut spool = fixture_spool(&root, "sess_pending");
        spool.start().unwrap();
        let (_, video) = spool.begin_video_segment(0).unwrap();
        spool
            .record_event(
                5,
                allowed_context(),
                InputEventKind::MouseUp {
                    button: "left".into(),
                },
            )
            .unwrap();
        fs::write(video, b"video").unwrap();
        spool.finish_video_segment(10).unwrap();
        spool.begin_finalization(10).unwrap();
        let output = root.join("pending.png");
        fs::write(&output, b"png").unwrap();
        spool.finalize(&[output]).unwrap();

        let pending = discover_pending_submissions(&root).unwrap();
        assert_eq!(pending.len(), 1);
        assert_eq!(pending[0].session_id, "sess_pending");
        fs::write(pending[0].directory.join("submission.json"), b"{}").unwrap();
        assert!(discover_pending_submissions(&root).unwrap().is_empty());
        fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn crash_recovery_preserves_a_nonempty_partial_segment_and_pauses() {
        let root = temporary_directory("partial-recovery");
        let mut spool = fixture_spool(&root, "sess_partial");
        spool.start().unwrap();
        let (_, video) = spool.begin_video_segment(0).unwrap();
        fs::write(video, b"recoverable video bytes").unwrap();
        spool.checkpoint_elapsed(25).unwrap();
        drop(spool);
        let mut recovered = SessionSpool::open(root.join("sessions/sess_partial")).unwrap();
        recovered.prepare_recovery().unwrap();
        assert_eq!(recovered.stage(), SpoolStage::Paused);
        assert_eq!(recovered.state.video_segments.len(), 1);
        assert_eq!(recovered.state.video_segments[0].end_ns, 25);
        fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn recovery_returns_an_interrupted_finalization_to_paused() {
        let root = temporary_directory("finalization-recovery");
        let mut spool = fixture_spool(&root, "sess_finalization_recovery");
        spool.start().unwrap();
        let (_, video) = spool.begin_video_segment(0).unwrap();
        fs::write(video, b"video bytes").unwrap();
        spool.finish_video_segment(25).unwrap();
        spool.begin_finalization(25).unwrap();
        spool.prepare_recovery().unwrap();
        assert_eq!(spool.stage(), SpoolStage::Paused);
        fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn executable_outputs_are_rejected() {
        let root = temporary_directory("output-safety");
        let executable = root.join("payload.exe");
        fs::write(&executable, b"not executable").unwrap();
        assert!(matches!(
            safe_output_name(&executable),
            Err(SpoolError::InvalidOutput(_))
        ));
        fs::remove_dir_all(root).unwrap();
    }

    fn fixture_spool(root: &Path, session_id: &str) -> SessionSpool {
        SessionSpool::create(
            root,
            spec(session_id),
            consent(),
            passing_preflight(),
            RecordingPolicy::safe_default("Photo Editor"),
        )
        .unwrap()
    }

    fn spec(session_id: &str) -> SessionSpec {
        SessionSpec {
            session_id: session_id.into(),
            task_id: "task_test".into(),
            required_application: "Photo Editor".into(),
            client_version: "0.1.0".into(),
        }
    }

    fn consent() -> ConsentProof {
        ConsentProof {
            document_id: "consent_1".into(),
            version: "v1".into(),
            text_hash: "a".repeat(64),
            accepted_at_unix_ms: 1,
        }
    }

    fn passing_preflight() -> PreflightReport {
        PreflightReport {
            checked_at_unix_ms: 1,
            checks: REQUIRED_PREFLIGHT_CHECKS
                .into_iter()
                .map(|name| PreflightCheck {
                    name: name.into(),
                    passed: true,
                    detail: "ok".into(),
                })
                .collect(),
        }
    }

    fn allowed_context() -> CaptureContext {
        CaptureContext {
            application: "Photo Editor".into(),
            window_title: "Synthetic product asset".into(),
        }
    }

    fn temporary_directory(label: &str) -> PathBuf {
        let path = std::env::temp_dir().join(format!("trajectory-{label}-{}", std::process::id()));
        let _ = fs::remove_dir_all(&path);
        fs::create_dir_all(&path).unwrap();
        path
    }
}
