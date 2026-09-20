use crate::{SessionSpool, SpoolError};
use serde::{Deserialize, Serialize};
use std::io::Write;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::thread;
use std::time::{Duration, Instant};

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum CaptureSource {
    WindowsDesktop,
    MacOsScreen {
        device: String,
    },
    X11Display {
        display: String,
        width: u32,
        height: u32,
    },
}

pub trait SegmentProcess: Send {
    fn stop(&mut self) -> Result<(), SpoolError>;
}

pub trait SegmentFactory: Send {
    fn start(
        &mut self,
        output: &Path,
        source: &CaptureSource,
    ) -> Result<Box<dyn SegmentProcess>, SpoolError>;
}

pub struct FfmpegSegmentFactory {
    binary: PathBuf,
    frame_rate: u16,
}

impl FfmpegSegmentFactory {
    pub fn new(binary: impl Into<PathBuf>) -> Self {
        Self {
            binary: binary.into(),
            frame_rate: 30,
        }
    }

    pub fn command_arguments(&self, output: &Path, source: &CaptureSource) -> Vec<String> {
        let mut arguments = vec![
            "-hide_banner".into(),
            "-loglevel".into(),
            "error".into(),
            "-y".into(),
        ];
        match source {
            CaptureSource::WindowsDesktop => arguments.extend(
                [
                    "-f",
                    "gdigrab",
                    "-framerate",
                    &self.frame_rate.to_string(),
                    "-i",
                    "desktop",
                ]
                .map(String::from),
            ),
            CaptureSource::MacOsScreen { device } => arguments.extend([
                "-f".into(),
                "avfoundation".into(),
                "-framerate".into(),
                self.frame_rate.to_string(),
                "-capture_cursor".into(),
                "1".into(),
                "-capture_mouse_clicks".into(),
                "0".into(),
                "-i".into(),
                format!("{device}:none"),
            ]),
            CaptureSource::X11Display {
                display,
                width,
                height,
            } => arguments.extend([
                "-f".into(),
                "x11grab".into(),
                "-framerate".into(),
                self.frame_rate.to_string(),
                "-video_size".into(),
                format!("{width}x{height}"),
                "-i".into(),
                display.clone(),
            ]),
        }
        arguments.extend([
            "-an".into(),
            "-c:v".into(),
            "libx264".into(),
            "-preset".into(),
            "veryfast".into(),
            "-pix_fmt".into(),
            "yuv420p".into(),
            output.display().to_string(),
        ]);
        arguments
    }
}

impl SegmentFactory for FfmpegSegmentFactory {
    fn start(
        &mut self,
        output: &Path,
        source: &CaptureSource,
    ) -> Result<Box<dyn SegmentProcess>, SpoolError> {
        let mut command = Command::new(&self.binary);
        command
            .args(self.command_arguments(output, source))
            .stdin(Stdio::piped())
            .stdout(Stdio::null())
            .stderr(Stdio::piped());
        let child = command
            .spawn()
            .map_err(|error| SpoolError::Process(format!("start FFmpeg: {error}")))?;
        Ok(Box::new(ChildSegmentProcess { child }))
    }
}

struct ChildSegmentProcess {
    child: Child,
}

impl Drop for ChildSegmentProcess {
    fn drop(&mut self) {
        if !matches!(self.child.try_wait(), Ok(Some(_))) {
            let _ = self.child.kill();
            let _ = self.child.wait();
        }
    }
}

impl SegmentProcess for ChildSegmentProcess {
    fn stop(&mut self) -> Result<(), SpoolError> {
        if let Some(mut stdin) = self.child.stdin.take() {
            let _ = stdin.write_all(b"q\n");
            let _ = stdin.flush();
        }
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            match self.child.try_wait() {
                Ok(Some(status)) if status.success() => return Ok(()),
                Ok(Some(status)) => {
                    return Err(SpoolError::Process(format!("FFmpeg exited with {status}")));
                }
                Ok(None) if Instant::now() < deadline => thread::sleep(Duration::from_millis(50)),
                Ok(None) => {
                    self.child
                        .kill()
                        .map_err(|error| SpoolError::Process(format!("stop FFmpeg: {error}")))?;
                    let _ = self.child.wait();
                    return Err(SpoolError::Process(
                        "FFmpeg did not stop cleanly within five seconds".into(),
                    ));
                }
                Err(error) => return Err(SpoolError::Process(format!("wait for FFmpeg: {error}"))),
            }
        }
    }
}

pub struct SegmentedRecorder<F: SegmentFactory> {
    factory: F,
    source: CaptureSource,
    process: Option<Box<dyn SegmentProcess>>,
}

impl<F: SegmentFactory> SegmentedRecorder<F> {
    pub fn new(factory: F, source: CaptureSource) -> Self {
        Self {
            factory,
            source,
            process: None,
        }
    }

    pub fn start(&mut self, spool: &mut SessionSpool, start_ns: u64) -> Result<(), SpoolError> {
        if self.process.is_some() {
            return Err(SpoolError::Process(
                "capture process is already active".into(),
            ));
        }
        let (_, output) = spool.begin_video_segment(start_ns)?;
        match self.factory.start(&output, &self.source) {
            Ok(process) => {
                self.process = Some(process);
                Ok(())
            }
            Err(error) => {
                let _ = spool.abort_video_segment();
                Err(error)
            }
        }
    }

    pub fn rotate(
        &mut self,
        spool: &mut SessionSpool,
        timestamp_ns: u64,
    ) -> Result<(), SpoolError> {
        self.stop_active(spool, timestamp_ns)?;
        self.start(spool, timestamp_ns)
    }

    pub fn pause(&mut self, spool: &mut SessionSpool, timestamp_ns: u64) -> Result<(), SpoolError> {
        self.stop_active(spool, timestamp_ns)?;
        spool.pause(timestamp_ns)
    }

    pub fn resume(
        &mut self,
        spool: &mut SessionSpool,
        timestamp_ns: u64,
    ) -> Result<(), SpoolError> {
        spool.resume()?;
        self.start(spool, timestamp_ns)
    }

    pub fn stop(&mut self, spool: &mut SessionSpool, timestamp_ns: u64) -> Result<(), SpoolError> {
        self.stop_active(spool, timestamp_ns)
    }

    fn stop_active(
        &mut self,
        spool: &mut SessionSpool,
        timestamp_ns: u64,
    ) -> Result<(), SpoolError> {
        let mut process = self
            .process
            .take()
            .ok_or_else(|| SpoolError::Process("no capture process is active".into()))?;
        process.stop()?;
        spool.finish_video_segment(timestamp_ns)?;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{ConsentProof, PreflightCheck, PreflightReport, RecordingPolicy, SessionSpec};
    use std::fs;
    use std::sync::{Arc, Mutex};

    struct FakeFactory {
        stopped: Arc<Mutex<u32>>,
    }
    struct FakeProcess {
        output: PathBuf,
        stopped: Arc<Mutex<u32>>,
    }

    impl SegmentFactory for FakeFactory {
        fn start(
            &mut self,
            output: &Path,
            _: &CaptureSource,
        ) -> Result<Box<dyn SegmentProcess>, SpoolError> {
            Ok(Box::new(FakeProcess {
                output: output.into(),
                stopped: self.stopped.clone(),
            }))
        }
    }
    impl SegmentProcess for FakeProcess {
        fn stop(&mut self) -> Result<(), SpoolError> {
            fs::write(&self.output, b"fake video")?;
            *self.stopped.lock().expect("fake lock") += 1;
            Ok(())
        }
    }

    #[test]
    fn ffmpeg_commands_are_explicit_30_fps_h264_capture() {
        let factory = FfmpegSegmentFactory::new("ffmpeg");
        let x11 = factory.command_arguments(
            Path::new("000001.mp4"),
            &CaptureSource::X11Display {
                display: ":0".into(),
                width: 1920,
                height: 1080,
            },
        );
        assert!(x11.windows(2).any(|pair| pair == ["-framerate", "30"]));
        assert!(x11.windows(2).any(|pair| pair == ["-c:v", "libx264"]));
        assert!(x11.contains(&"x11grab".to_string()));
        let windows =
            factory.command_arguments(Path::new("000001.mp4"), &CaptureSource::WindowsDesktop);
        assert!(windows.contains(&"gdigrab".to_string()));
        let macos = factory.command_arguments(
            Path::new("000001.mp4"),
            &CaptureSource::MacOsScreen {
                device: "Capture screen 0".into(),
            },
        );
        assert!(macos.contains(&"avfoundation".to_string()));
        assert!(macos.contains(&"Capture screen 0:none".to_string()));
        assert!(
            macos
                .windows(2)
                .any(|pair| pair == ["-capture_cursor", "1"])
        );
    }

    #[test]
    fn recorder_closes_each_segment_before_pause_and_resume() {
        let root = temporary_directory("segmented");
        let mut spool = fixture_spool(&root);
        spool.start().unwrap();
        let stopped = Arc::new(Mutex::new(0));
        let mut recorder = SegmentedRecorder::new(
            FakeFactory {
                stopped: stopped.clone(),
            },
            CaptureSource::WindowsDesktop,
        );
        recorder.start(&mut spool, 0).unwrap();
        recorder.pause(&mut spool, 10).unwrap();
        recorder.resume(&mut spool, 20).unwrap();
        recorder.stop(&mut spool, 30).unwrap();
        assert_eq!(*stopped.lock().unwrap(), 2);
        assert!(root.join("sessions/sess_test/video/000001.mp4").exists());
        assert!(root.join("sessions/sess_test/video/000002.mp4").exists());
        fs::remove_dir_all(root).unwrap();
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn dropping_a_live_segment_process_kills_and_reaps_it() {
        let child = Command::new("sleep").arg("30").spawn().unwrap();
        let process_id = child.id();
        drop(ChildSegmentProcess { child });
        assert!(!Path::new(&format!("/proc/{process_id}")).exists());
    }

    fn fixture_spool(root: &Path) -> SessionSpool {
        SessionSpool::create(
            root,
            SessionSpec {
                session_id: "sess_test".into(),
                task_id: "task_test".into(),
                required_application: "Photo Editor".into(),
                client_version: "0.1.0".into(),
            },
            ConsentProof {
                document_id: "consent_1".into(),
                version: "v1".into(),
                text_hash: "a".repeat(64),
                accepted_at_unix_ms: 1,
            },
            PreflightReport {
                checked_at_unix_ms: 1,
                checks: [
                    "screen_capture",
                    "input_capture",
                    "disk_space",
                    "required_application",
                    "output_folder",
                    "collector_version",
                ]
                .into_iter()
                .map(|name| PreflightCheck {
                    name: name.into(),
                    passed: true,
                    detail: "ok".into(),
                })
                .collect(),
            },
            RecordingPolicy::safe_default("Photo Editor"),
        )
        .unwrap()
    }

    fn temporary_directory(label: &str) -> PathBuf {
        let path = std::env::temp_dir().join(format!("trajectory-{label}-{}", std::process::id()));
        let _ = fs::remove_dir_all(&path);
        fs::create_dir_all(&path).unwrap();
        path
    }
}
