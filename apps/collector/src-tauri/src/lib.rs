mod auth;
mod native_input;
mod submission;

use collector_core::{
    CaptureContext, CaptureManifest, CaptureSource, ConsentProof, FfmpegSegmentFactory,
    InputEventKind, PolicyDecision, PreflightCheck, PreflightReport, RecordingPolicy,
    RecoveredSession, SegmentedRecorder, SessionSpec, SessionSpool, SpoolStage,
    discover_pending_submissions, discover_recoverable_sessions,
};
use native_input::{CapturedInput, NativeInputCapture};
use serde::Deserialize;
use std::fs::{self, OpenOptions};
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::{Arc, Mutex};
use std::time::{Instant, SystemTime, UNIX_EPOCH};
use submission::{SubmissionReceipt, SubmissionRequest};
use tauri::{AppHandle, Manager, State};

struct ActiveCapture {
    spool: SessionSpool,
    recorder: SegmentedRecorder<FfmpegSegmentFactory>,
    base_elapsed_ns: u64,
    running_since: Option<Instant>,
    input: Option<NativeInputCapture>,
    pause_reason: Option<String>,
}

impl ActiveCapture {
    fn elapsed_ns(&self) -> Result<u64, String> {
        let additional = self
            .running_since
            .map(|started| started.elapsed().as_nanos())
            .unwrap_or(0);
        let additional =
            u64::try_from(additional).map_err(|_| "capture duration exceeds u64".to_string())?;
        self.base_elapsed_ns
            .checked_add(additional)
            .ok_or_else(|| "capture duration overflow".to_string())
    }
}

#[derive(Default)]
struct CollectorRuntime {
    active: Mutex<Option<Arc<Mutex<ActiveCapture>>>>,
}

#[derive(Deserialize)]
struct StartCaptureRequest {
    spec: SessionSpec,
    consent: ConsentProof,
    preflight: PreflightReport,
    policy: RecordingPolicy,
    source: CaptureSource,
}

#[derive(Deserialize)]
struct InputEventRequest {
    context: CaptureContext,
    event: InputEventKind,
}

#[derive(serde::Serialize)]
struct CaptureStatus {
    session_id: String,
    stage: SpoolStage,
    elapsed_ns: u64,
    recording_visible: bool,
    pause_reason: Option<String>,
}

#[tauri::command]
fn collector_policy(required_application: String) -> RecordingPolicy {
    RecordingPolicy::safe_default(required_application)
}

#[tauri::command]
fn capture_source() -> Result<CaptureSource, String> {
    platform_capture_source()
}

#[tauri::command]
fn run_preflight(app: AppHandle, required_application: String) -> Result<PreflightReport, String> {
    let data_directory = spool_root(&app)?;
    fs::create_dir_all(&data_directory).map_err(|error| error.to_string())?;
    let (screen_ready, screen_detail) = screen_capture_check();
    let (input_ready, input_detail) = input_capture_check();
    let application_ready = required_application_running(&required_application);
    let output_ready = directory_writable(&data_directory);
    let available_bytes = fs2::available_space(&data_directory).unwrap_or(0);
    let disk_ready = available_bytes >= 2 * 1024 * 1024 * 1024;
    let disk_detail = format!(
        "{:.1} GB available; 2.0 GB required",
        available_bytes as f64 / 1_000_000_000.0
    );
    Ok(PreflightReport {
        checked_at_unix_ms: unix_ms()?,
        checks: vec![
            check("screen_capture", screen_ready, &screen_detail),
            check("input_capture", input_ready, &input_detail),
            check("disk_space", disk_ready, &disk_detail),
            check(
                "required_application",
                application_ready,
                if application_ready {
                    "Required application process detected"
                } else {
                    "Open the required application before starting"
                },
            ),
            check(
                "output_folder",
                output_ready,
                if output_ready {
                    "Local spool is writable"
                } else {
                    "Local spool is not writable"
                },
            ),
            check("collector_version", true, env!("CARGO_PKG_VERSION")),
        ],
    })
}

#[tauri::command]
fn recoverable_sessions(app: AppHandle) -> Result<Vec<RecoveredSession>, String> {
    let root = spool_root(&app)?;
    let mut sessions = discover_recoverable_sessions(&root).map_err(|error| error.to_string())?;
    sessions.extend(discover_pending_submissions(&root).map_err(|error| error.to_string())?);
    sessions.sort_by(|left, right| left.session_id.cmp(&right.session_id));
    Ok(sessions)
}

#[tauri::command]
fn load_finalized_capture(app: AppHandle, session_id: String) -> Result<CaptureManifest, String> {
    if session_id.is_empty()
        || !session_id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
    {
        return Err("invalid session ID".into());
    }
    let directory = spool_root(&app)?.join("sessions").join(&session_id);
    let spool = SessionSpool::open(&directory).map_err(|error| error.to_string())?;
    if spool.stage() != SpoolStage::Finalized || directory.join("submission.json").exists() {
        return Err("session is not awaiting submission".into());
    }
    let manifest: CaptureManifest = serde_json::from_reader(
        fs::File::open(directory.join("manifest.json")).map_err(|error| error.to_string())?,
    )
    .map_err(|error| error.to_string())?;
    if manifest.session_id != session_id {
        return Err("finalized capture manifest does not match the session".into());
    }
    Ok(manifest)
}

#[tauri::command]
fn start_capture(
    app: AppHandle,
    runtime: State<'_, CollectorRuntime>,
    request: StartCaptureRequest,
) -> Result<CaptureStatus, String> {
    let mut guard = runtime
        .active
        .lock()
        .map_err(|_| "collector state lock is poisoned".to_string())?;
    if guard.is_some() {
        return Err("another session is already active".into());
    }
    let mut spool = SessionSpool::create(
        spool_root(&app)?,
        request.spec,
        request.consent,
        request.preflight,
        request.policy,
    )
    .map_err(|error| error.to_string())?;
    spool.start().map_err(|error| error.to_string())?;
    let mut recorder =
        SegmentedRecorder::new(FfmpegSegmentFactory::new(ffmpeg_binary()), request.source);
    if let Err(error) = recorder.start(&mut spool, 0) {
        let _ = spool.pause(0);
        return Err(error.to_string());
    }
    if let Err(error) = set_recording_indicator(&app, true) {
        let _ = recorder.pause(&mut spool, 1);
        return Err(error);
    }
    let session_id = spool.session_id().to_string();
    let active = Arc::new(Mutex::new(ActiveCapture {
        spool,
        recorder,
        base_elapsed_ns: 0,
        running_since: Some(Instant::now()),
        input: None,
        pause_reason: None,
    }));
    if let Err(error) = attach_native_input(&active, app.clone()) {
        if let Ok(mut capture) = active.lock() {
            let elapsed = capture.elapsed_ns().unwrap_or(1).max(1);
            let ActiveCapture {
                spool, recorder, ..
            } = &mut *capture;
            if recorder.pause(spool, elapsed).is_err() {
                let _ = capture.spool.prepare_recovery();
            }
        }
        let _ = set_recording_indicator(&app, false);
        return Err(error);
    }
    *guard = Some(active);
    Ok(CaptureStatus {
        session_id,
        stage: SpoolStage::Recording,
        elapsed_ns: 0,
        recording_visible: true,
        pause_reason: None,
    })
}

#[tauri::command]
fn record_input_event(
    runtime: State<'_, CollectorRuntime>,
    request: InputEventRequest,
) -> Result<(), String> {
    with_active(&runtime, |active| {
        let timestamp = active.elapsed_ns()?;
        active
            .spool
            .record_event(timestamp, request.context, request.event)
            .map_err(|error| error.to_string())
    })
}

#[tauri::command]
fn checkpoint_capture(runtime: State<'_, CollectorRuntime>) -> Result<CaptureStatus, String> {
    with_active(&runtime, |active| {
        let elapsed = active.elapsed_ns()?;
        active
            .spool
            .checkpoint_elapsed(elapsed)
            .map_err(|error| error.to_string())?;
        Ok(status(active, elapsed))
    })
}

#[tauri::command]
fn rotate_capture(
    app: AppHandle,
    runtime: State<'_, CollectorRuntime>,
) -> Result<CaptureStatus, String> {
    let handle = active_handle(&runtime)?;
    let failure = {
        let mut active = handle
            .lock()
            .map_err(|_| "active capture lock is poisoned".to_string())?;
        let elapsed = active.elapsed_ns()?.max(1);
        let ActiveCapture {
            spool, recorder, ..
        } = &mut *active;
        match recorder.rotate(spool, elapsed) {
            Ok(()) => return Ok(status(&active, elapsed)),
            Err(error) => {
                let message = format!("segment rotation failed: {error}");
                let _ = active.spool.prepare_recovery();
                active.base_elapsed_ns = elapsed;
                active.running_since = None;
                active.pause_reason = Some(message.clone());
                (message, active.input.take())
            }
        }
    };
    if let Some(input) = failure.1 {
        let _ = input.stop();
    }
    let _ = set_recording_indicator(&app, false);
    Err(failure.0)
}

#[tauri::command]
fn pause_capture(
    app: AppHandle,
    runtime: State<'_, CollectorRuntime>,
) -> Result<CaptureStatus, String> {
    let handle = active_handle(&runtime)?;
    let (capture_status, input, pause_failure) = {
        let mut active = handle
            .lock()
            .map_err(|_| "active capture lock is poisoned".to_string())?;
        let elapsed = active.elapsed_ns()?.max(1);
        let ActiveCapture {
            spool, recorder, ..
        } = &mut *active;
        let pause_failure = recorder
            .pause(spool, elapsed)
            .err()
            .map(|error| error.to_string());
        if pause_failure.is_some() {
            let _ = active.spool.prepare_recovery();
        }
        active.base_elapsed_ns = elapsed;
        active.running_since = None;
        active.pause_reason = Some(match &pause_failure {
            Some(error) => format!("Paused after recorder failure: {error}"),
            None => "Paused by contributor".into(),
        });
        (status(&active, elapsed), active.input.take(), pause_failure)
    };
    let input_result = if let Some(input) = input {
        input.stop()
    } else {
        Ok(())
    };
    let indicator_result = set_recording_indicator(&app, false);
    if let Some(error) = pause_failure {
        return Err(error);
    }
    input_result?;
    indicator_result?;
    Ok(capture_status)
}

#[tauri::command]
fn resume_capture(
    app: AppHandle,
    runtime: State<'_, CollectorRuntime>,
) -> Result<CaptureStatus, String> {
    let handle = active_handle(&runtime)?;
    set_recording_indicator(&app, true)?;
    let needs_hook = {
        let mut active = handle
            .lock()
            .map_err(|_| "active capture lock is poisoned".to_string())?;
        let elapsed = active.base_elapsed_ns;
        let ActiveCapture {
            spool, recorder, ..
        } = &mut *active;
        if let Err(error) = recorder.resume(spool, elapsed) {
            let _ = set_recording_indicator(&app, false);
            return Err(error.to_string());
        }
        active.running_since = Some(Instant::now());
        active.pause_reason = None;
        active.input.is_none()
    };
    if needs_hook && let Err(error) = attach_native_input(&handle, app.clone()) {
        if let Ok(mut active) = handle.lock() {
            let elapsed = active.elapsed_ns().unwrap_or(active.base_elapsed_ns).max(1);
            let ActiveCapture {
                spool, recorder, ..
            } = &mut *active;
            if recorder.pause(spool, elapsed).is_err() {
                let _ = active.spool.prepare_recovery();
            }
            active.base_elapsed_ns = elapsed;
            active.running_since = None;
            active.pause_reason = Some("Native input hook failed while resuming".into());
        }
        let _ = set_recording_indicator(&app, false);
        return Err(error);
    }
    let active = handle
        .lock()
        .map_err(|_| "active capture lock is poisoned".to_string())?;
    Ok(status(&active, active.base_elapsed_ns))
}

#[tauri::command]
fn recover_session(
    app: AppHandle,
    runtime: State<'_, CollectorRuntime>,
    directory: PathBuf,
    source: CaptureSource,
) -> Result<CaptureStatus, String> {
    let canonical_root =
        fs::canonicalize(spool_root(&app)?.join("sessions")).map_err(|error| error.to_string())?;
    let canonical_directory = fs::canonicalize(&directory).map_err(|error| error.to_string())?;
    if !canonical_directory.starts_with(&canonical_root) {
        return Err("recovery path is outside the collector spool".into());
    }
    let mut spool = SessionSpool::open(canonical_directory).map_err(|error| error.to_string())?;
    spool
        .prepare_recovery()
        .map_err(|error| error.to_string())?;
    let elapsed = spool.elapsed_ns();
    let session_id = spool.session_id().to_string();
    let mut guard = runtime
        .active
        .lock()
        .map_err(|_| "collector state lock is poisoned".to_string())?;
    if guard.is_some() {
        return Err("another session is already active".into());
    }
    *guard = Some(Arc::new(Mutex::new(ActiveCapture {
        spool,
        recorder: SegmentedRecorder::new(FfmpegSegmentFactory::new(ffmpeg_binary()), source),
        base_elapsed_ns: elapsed,
        running_since: None,
        input: None,
        pause_reason: Some("Recovered after an interrupted recording".into()),
    })));
    Ok(CaptureStatus {
        session_id,
        stage: SpoolStage::Paused,
        elapsed_ns: elapsed,
        recording_visible: false,
        pause_reason: Some("Recovered after an interrupted recording".into()),
    })
}

#[tauri::command]
fn finish_capture(
    app: AppHandle,
    runtime: State<'_, CollectorRuntime>,
    output_files: Vec<PathBuf>,
) -> Result<CaptureManifest, String> {
    let mut guard = runtime
        .active
        .lock()
        .map_err(|_| "collector state lock is poisoned".to_string())?;
    let handle = guard
        .take()
        .ok_or_else(|| "no session is active".to_string())?;
    drop(guard);
    let input = handle
        .lock()
        .map_err(|_| "active capture lock is poisoned".to_string())?
        .input
        .take();
    let input_result = if let Some(input) = input {
        input.stop()
    } else {
        Ok(())
    };
    if let Err(error) = input_result {
        let _ = set_recording_indicator(&app, false);
        return Err(error);
    }
    let result = (|| {
        let mut active = handle
            .lock()
            .map_err(|_| "active capture lock is poisoned".to_string())?;
        let elapsed = active.elapsed_ns()?.max(1);
        if active.spool.stage() == SpoolStage::Recording {
            let ActiveCapture {
                spool, recorder, ..
            } = &mut *active;
            if let Err(error) = recorder.stop(spool, elapsed) {
                let _ = active.spool.prepare_recovery();
                return Err(error.to_string());
            }
        }
        active
            .spool
            .begin_finalization(elapsed)
            .map_err(|error| error.to_string())?;
        active
            .spool
            .finalize(&output_files)
            .map_err(|error| error.to_string())
    })();
    let indicator_result = set_recording_indicator(&app, false);
    result.and_then(|manifest| indicator_result.map(|_| manifest))
}

#[tauri::command]
async fn submit_finalized_capture(
    app: AppHandle,
    auth_runtime: State<'_, auth::AuthRuntime>,
    mut request: SubmissionRequest,
) -> Result<SubmissionReceipt, String> {
    if request.session_id.is_empty()
        || !request
            .session_id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
    {
        return Err("invalid session ID".into());
    }
    if auth::production_enabled() {
        request.access_token = Some(auth::access_token_for_submission(&auth_runtime).await?);
    }
    let directory = spool_root(&app)?.join("sessions").join(&request.session_id);
    tauri::async_runtime::spawn_blocking(move || submission::submit(&directory, request))
        .await
        .map_err(|error| format!("submission task failed: {error}"))?
}

fn with_active<T>(
    runtime: &State<'_, CollectorRuntime>,
    operation: impl FnOnce(&mut ActiveCapture) -> Result<T, String>,
) -> Result<T, String> {
    let handle = active_handle(runtime)?;
    let mut active = handle
        .lock()
        .map_err(|_| "active capture lock is poisoned".to_string())?;
    operation(&mut active)
}

fn active_handle(
    runtime: &State<'_, CollectorRuntime>,
) -> Result<Arc<Mutex<ActiveCapture>>, String> {
    let guard = runtime
        .active
        .lock()
        .map_err(|_| "collector state lock is poisoned".to_string())?;
    guard
        .as_ref()
        .cloned()
        .ok_or_else(|| "no session is active".to_string())
}

fn status(active: &ActiveCapture, elapsed_ns: u64) -> CaptureStatus {
    CaptureStatus {
        session_id: active.spool.session_id().to_string(),
        stage: active.spool.stage(),
        elapsed_ns,
        recording_visible: active.spool.stage() == SpoolStage::Recording,
        pause_reason: active.pause_reason.clone(),
    }
}

fn attach_native_input(capture: &Arc<Mutex<ActiveCapture>>, app: AppHandle) -> Result<(), String> {
    let weak_capture = Arc::downgrade(capture);
    let indicator_app = app.clone();
    let input = NativeInputCapture::start(move |captured: CapturedInput| {
        let Some(capture) = weak_capture.upgrade() else {
            return;
        };
        let Ok(mut active) = capture.try_lock() else {
            return;
        };
        if active.spool.stage() != SpoolStage::Recording {
            return;
        }
        if captured.collector_owned {
            return;
        }
        let elapsed = match active.elapsed_ns() {
            Ok(elapsed) => elapsed.max(1),
            Err(_) => return,
        };
        match active.spool.policy().evaluate(&captured.context) {
            PolicyDecision::Allowed => {
                let _ = active
                    .spool
                    .record_event(elapsed, captured.context, captured.event);
            }
            decision => {
                let ActiveCapture {
                    spool, recorder, ..
                } = &mut *active;
                let paused = recorder.pause(spool, elapsed).is_ok()
                    || active.spool.prepare_recovery().is_ok();
                if paused {
                    active.base_elapsed_ns = elapsed;
                    active.running_since = None;
                    active.pause_reason = Some(policy_pause_reason(decision));
                    let _ = set_recording_indicator(&indicator_app, false);
                }
            }
        }
    })?;
    let mut active = capture
        .lock()
        .map_err(|_| "active capture lock is poisoned".to_string())?;
    if active.input.is_some() {
        drop(active);
        input.stop()?;
        return Err("a native input hook is already attached to this capture".into());
    }
    active.input = Some(input);
    Ok(())
}

fn policy_pause_reason(decision: PolicyDecision) -> String {
    match decision {
        PolicyDecision::Allowed => "Recording allowed".into(),
        PolicyDecision::DeniedSensitiveContext => {
            "Paused automatically: a sensitive application or window was detected".into()
        }
        PolicyDecision::DeniedNotAllowlisted => {
            "Paused automatically: focus left the task application".into()
        }
        PolicyDecision::UnsafeConfiguration => {
            "Paused automatically: the recording policy is unsafe".into()
        }
    }
}

fn spool_root(app: &AppHandle) -> Result<PathBuf, String> {
    app.path()
        .app_local_data_dir()
        .map(|path| path.join("spool"))
        .map_err(|error| error.to_string())
}

fn set_recording_indicator(app: &AppHandle, recording: bool) -> Result<(), String> {
    let window = app
        .get_webview_window("main")
        .ok_or_else(|| "main collector window is missing".to_string())?;
    window
        .set_always_on_top(recording)
        .map_err(|error| error.to_string())
}

fn shutdown_active_capture(runtime: &CollectorRuntime) {
    let handle = runtime
        .active
        .lock()
        .ok()
        .and_then(|mut active| active.take());
    let Some(handle) = handle else {
        return;
    };
    let input = handle
        .lock()
        .ok()
        .and_then(|mut active| active.input.take());
    if let Some(input) = input {
        let _ = input.stop();
    }
    let Ok(mut active) = handle.lock() else {
        return;
    };
    let elapsed = active.elapsed_ns().unwrap_or(active.base_elapsed_ns).max(1);
    if active.spool.stage() == SpoolStage::Recording {
        let ActiveCapture {
            spool, recorder, ..
        } = &mut *active;
        if recorder.pause(spool, elapsed).is_err() {
            let _ = active.spool.prepare_recovery();
        }
    } else if active.spool.stage() == SpoolStage::Finalizing {
        let _ = active.spool.prepare_recovery();
    }
}

fn check(name: &str, passed: bool, detail: &str) -> PreflightCheck {
    PreflightCheck {
        name: name.into(),
        passed,
        detail: detail.into(),
    }
}

fn screen_capture_check() -> (bool, String) {
    let ffmpeg = ffmpeg_binary();
    if !command_path_succeeds(&ffmpeg, &["-version"]) {
        return (
            false,
            format!(
                "FFmpeg is unavailable at {}. Install it or configure TRAJECTORY_FFMPEG_PATH.",
                ffmpeg.display()
            ),
        );
    }
    match platform_capture_source() {
        Ok(CaptureSource::WindowsDesktop) => {
            (true, "FFmpeg Windows desktop capture is available".into())
        }
        Ok(CaptureSource::MacOsScreen { device }) => (
            true,
            format!("FFmpeg macOS screen capture is available through {device}"),
        ),
        Ok(CaptureSource::X11Display {
            display,
            width,
            height,
        }) => (
            true,
            format!("FFmpeg X11 capture is available on {display} at {width}×{height}"),
        ),
        Err(error) => (false, error),
    }
}

fn input_capture_check() -> (bool, String) {
    #[cfg(target_os = "linux")]
    if !linux_x11_session() {
        return (
            false,
            "Wayland blocks global input observation. Sign out and choose Ubuntu on Xorg.".into(),
        );
    }
    let ready = native_input::probe();
    (
        ready,
        if ready {
            "Task-scoped native input capture is available".into()
        } else {
            "Native input capture could not start for this desktop session".into()
        },
    )
}

fn platform_capture_source() -> Result<CaptureSource, String> {
    #[cfg(target_os = "windows")]
    {
        return Ok(CaptureSource::WindowsDesktop);
    }
    #[cfg(target_os = "linux")]
    {
        if !linux_x11_session() {
            return Err(
                "Wayland screen capture is not enabled yet. Sign out and choose Ubuntu on Xorg."
                    .into(),
            );
        }
        let display = std::env::var("DISPLAY")
            .map_err(|_| "The X11 DISPLAY variable is missing".to_string())?;
        let output = Command::new("xrandr")
            .arg("--current")
            .output()
            .map_err(|error| format!("read X11 display dimensions: {error}"))?;
        if !output.status.success() {
            return Err("xrandr could not read the X11 desktop dimensions".into());
        }
        let (width, height) = parse_xrandr_dimensions(&String::from_utf8_lossy(&output.stdout))
            .ok_or_else(|| "xrandr did not report the current desktop dimensions".to_string())?;
        return Ok(CaptureSource::X11Display {
            display,
            width,
            height,
        });
    }
    #[cfg(target_os = "macos")]
    {
        let output = Command::new(ffmpeg_binary())
            .args([
                "-hide_banner",
                "-f",
                "avfoundation",
                "-list_devices",
                "true",
                "-i",
                "",
            ])
            .output()
            .map_err(|error| format!("list macOS screen capture devices: {error}"))?;
        let device_list = String::from_utf8_lossy(&output.stderr);
        let device = device_list
            .lines()
            .find_map(parse_macos_screen_device)
            .ok_or_else(|| {
                "FFmpeg did not expose a macOS screen capture device. Enable Screen Recording permission and restart DataTrains."
                    .to_string()
            })?;
        return Ok(CaptureSource::MacOsScreen { device });
    }
    #[allow(unreachable_code)]
    Err("Desktop capture is unavailable on this operating system".into())
}

#[cfg(target_os = "macos")]
fn parse_macos_screen_device(line: &str) -> Option<String> {
    let marker = "Capture screen ";
    let start = line.find(marker)?;
    let suffix = &line[start..];
    let digits = suffix[marker.len()..]
        .chars()
        .take_while(char::is_ascii_digit)
        .collect::<String>();
    (!digits.is_empty()).then(|| format!("{marker}{digits}"))
}

#[cfg(target_os = "linux")]
fn linux_x11_session() -> bool {
    std::env::var("XDG_SESSION_TYPE")
        .map(|value| value.eq_ignore_ascii_case("x11"))
        .unwrap_or(false)
}

#[cfg(any(target_os = "linux", test))]
fn parse_xrandr_dimensions(output: &str) -> Option<(u32, u32)> {
    let current = output
        .lines()
        .find(|line| line.starts_with("Screen "))?
        .split_once(" current ")?
        .1
        .split_once(", maximum")?
        .0;
    let (width, height) = current.split_once(" x ")?;
    Some((width.trim().parse().ok()?, height.trim().parse().ok()?))
}

fn required_application_running(application: &str) -> bool {
    if application.trim().is_empty() {
        return false;
    }
    if cfg!(target_os = "windows") {
        Command::new("tasklist")
            .args(["/FI", &format!("IMAGENAME eq {application}"), "/NH"])
            .output()
            .map(|output| {
                output.status.success()
                    && String::from_utf8_lossy(&output.stdout)
                        .to_lowercase()
                        .contains(&application.to_lowercase())
            })
            .unwrap_or(false)
    } else {
        command_succeeds("pgrep", &["-fi", application])
    }
}

fn command_succeeds(program: &str, arguments: &[&str]) -> bool {
    Command::new(program)
        .args(arguments)
        .output()
        .map(|output| output.status.success())
        .unwrap_or(false)
}

fn command_path_succeeds(program: &Path, arguments: &[&str]) -> bool {
    Command::new(program)
        .args(arguments)
        .output()
        .map(|output| output.status.success())
        .unwrap_or(false)
}

fn ffmpeg_binary() -> PathBuf {
    if let Some(configured) =
        std::env::var_os("TRAJECTORY_FFMPEG_PATH").filter(|value| !value.is_empty())
    {
        return configured.into();
    }
    let executable_name = if cfg!(target_os = "windows") {
        "ffmpeg.exe"
    } else {
        "ffmpeg"
    };
    if let Some(bundled) = std::env::current_exe()
        .ok()
        .and_then(|path| {
            path.parent()
                .map(|directory| directory.join(executable_name))
        })
        .filter(|path| path.is_file())
    {
        return bundled;
    }
    #[cfg(target_os = "macos")]
    for candidate in ["/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg"] {
        let path = PathBuf::from(candidate);
        if path.is_file() {
            return path;
        }
    }
    PathBuf::from(executable_name)
}

fn directory_writable(directory: &Path) -> bool {
    let probe = directory.join(".preflight-write-probe");
    let result = OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(&probe)
        .and_then(|file| file.sync_all());
    let _ = fs::remove_file(probe);
    result.is_ok()
}

fn unix_ms() -> Result<u64, String> {
    let millis = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_err(|error| error.to_string())?
        .as_millis();
    u64::try_from(millis).map_err(|_| "system time exceeds u64".into())
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    let app = tauri::Builder::default()
        .plugin(tauri_plugin_dialog::init())
        .plugin(tauri_plugin_opener::init())
        .manage(auth::AuthRuntime::default())
        .manage(CollectorRuntime::default())
        .invoke_handler(tauri::generate_handler![
            auth::collector_configuration,
            auth::sign_in,
            auth::restore_session,
            auth::sign_out,
            auth::authenticated_api_request,
            collector_policy,
            capture_source,
            run_preflight,
            recoverable_sessions,
            load_finalized_capture,
            start_capture,
            record_input_event,
            checkpoint_capture,
            rotate_capture,
            pause_capture,
            resume_capture,
            recover_session,
            finish_capture,
            submit_finalized_capture,
        ])
        .build(tauri::generate_context!())
        .expect("failed to build trajectory collector");
    app.run(|app, event| {
        if matches!(event, tauri::RunEvent::ExitRequested { .. }) {
            shutdown_active_capture(&app.state::<CollectorRuntime>());
            let _ = set_recording_indicator(app, false);
        }
    });
}

#[cfg(test)]
mod platform_tests {
    use super::parse_xrandr_dimensions;

    #[test]
    fn parses_the_full_x11_desktop_size() {
        let output = "Screen 0: minimum 16 x 16, current 3840 x 1080, maximum 32767 x 32767\n";
        assert_eq!(parse_xrandr_dimensions(output), Some((3840, 1080)));
    }
}
