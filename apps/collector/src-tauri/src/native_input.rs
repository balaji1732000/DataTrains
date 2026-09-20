use collector_core::{CaptureContext, InputEventKind};

pub struct CapturedInput {
    pub context: CaptureContext,
    pub event: InputEventKind,
    pub collector_owned: bool,
}

#[cfg(target_os = "linux")]
mod linux {
    use super::CapturedInput;
    use collector_core::{CaptureContext, InputEventKind};
    use std::io::{BufRead, BufReader};
    use std::process::{Child, Command, Stdio};
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::sync::{Arc, Mutex, mpsc};
    use std::thread::{self, JoinHandle};
    use std::time::{Duration, Instant};

    pub struct NativeInputCapture {
        child: Arc<Mutex<Child>>,
        input_thread: Option<JoinHandle<()>>,
        watcher_thread: Option<JoinHandle<()>>,
        dispatch_thread: Option<JoinHandle<()>>,
        sender: Option<mpsc::Sender<CapturedInput>>,
        stop_signal: Arc<AtomicBool>,
    }

    impl NativeInputCapture {
        pub fn start(
            handler: impl Fn(CapturedInput) + Send + Sync + 'static,
        ) -> Result<Self, String> {
            require_x11_session()?;
            let mut process = Command::new("xinput")
                .args(["test-xi2", "--root"])
                .stdin(Stdio::null())
                .stdout(Stdio::piped())
                .stderr(Stdio::null())
                .spawn()
                .map_err(|error| format!("start X11 input capture: {error}"))?;
            let stdout = process
                .stdout
                .take()
                .ok_or_else(|| "X11 input capture did not expose an event stream".to_string())?;
            thread::sleep(Duration::from_millis(50));
            if let Some(status) = process
                .try_wait()
                .map_err(|error| format!("check X11 input capture: {error}"))?
            {
                return Err(format!(
                    "X11 input capture exited immediately with {status}"
                ));
            }

            let child = Arc::new(Mutex::new(process));
            let stop_signal = Arc::new(AtomicBool::new(false));
            let context = Arc::new(Mutex::new(foreground_context()));
            let (sender, receiver) = mpsc::channel::<CapturedInput>();

            let dispatch_thread = thread::Builder::new()
                .name("trajectory-input-dispatch".into())
                .spawn(move || {
                    while let Ok(event) = receiver.recv() {
                        handler(event);
                    }
                })
                .map_err(|error| format!("start input dispatcher: {error}"))?;

            let input_sender = sender.clone();
            let input_context = Arc::clone(&context);
            let input_stop = Arc::clone(&stop_signal);
            let input_thread = thread::Builder::new()
                .name("trajectory-x11-input".into())
                .spawn(move || {
                    let mut parser = XInputParser::default();
                    for line in BufReader::new(stdout).lines().map_while(Result::ok) {
                        if input_stop.load(Ordering::Acquire) {
                            break;
                        }
                        if let Some(event) = parser.push(&line) {
                            emit(&input_sender, &input_context, event);
                        }
                    }
                    if let Some(event) = parser.finish() {
                        emit(&input_sender, &input_context, event);
                    }
                })
                .map_err(|error| format!("start X11 input reader: {error}"))?;

            let watcher_sender = sender.clone();
            let watcher_context = Arc::clone(&context);
            let watcher_stop = Arc::clone(&stop_signal);
            let watcher_thread = thread::Builder::new()
                .name("trajectory-x11-focus-watcher".into())
                .spawn(move || {
                    while !watcher_stop.load(Ordering::Acquire) {
                        let current = foreground_context();
                        let changed = watcher_context
                            .lock()
                            .map(|mut previous| {
                                if *previous == current {
                                    false
                                } else {
                                    *previous = current.clone();
                                    true
                                }
                            })
                            .unwrap_or(false);
                        if changed {
                            let _ = watcher_sender.send(CapturedInput {
                                context: current.0.clone(),
                                event: InputEventKind::AppFocus {
                                    window_title: current.0.window_title.clone(),
                                },
                                collector_owned: current.1,
                            });
                        }
                        thread::sleep(Duration::from_millis(100));
                    }
                })
                .map_err(|error| format!("start X11 focus watcher: {error}"))?;

            Ok(Self {
                child,
                input_thread: Some(input_thread),
                watcher_thread: Some(watcher_thread),
                dispatch_thread: Some(dispatch_thread),
                sender: Some(sender),
                stop_signal,
            })
        }

        pub fn stop(mut self) -> Result<(), String> {
            self.stop_inner()
        }

        fn stop_inner(&mut self) -> Result<(), String> {
            if self.input_thread.is_none() {
                return Ok(());
            }
            self.stop_signal.store(true, Ordering::Release);
            let mut failure = None;
            if let Ok(mut child) = self.child.lock()
                && child.try_wait().ok().flatten().is_none()
                && let Err(error) = child.kill()
            {
                failure = Some(format!("stop X11 input capture: {error}"));
            }
            if let Some(thread) = self.input_thread.take()
                && thread.join().is_err()
            {
                failure.get_or_insert_with(|| "X11 input reader panicked".into());
            }
            if let Some(thread) = self.watcher_thread.take()
                && thread.join().is_err()
            {
                failure.get_or_insert_with(|| "X11 focus watcher panicked".into());
            }
            self.sender.take();
            if let Some(thread) = self.dispatch_thread.take()
                && thread.join().is_err()
            {
                failure.get_or_insert_with(|| "input dispatcher panicked".into());
            }
            failure.map_or(Ok(()), Err)
        }
    }

    impl Drop for NativeInputCapture {
        fn drop(&mut self) {
            let _ = self.stop_inner();
        }
    }

    pub fn probe() -> bool {
        NativeInputCapture::start(|_| {})
            .and_then(NativeInputCapture::stop)
            .is_ok()
    }

    fn require_x11_session() -> Result<(), String> {
        let session = std::env::var("XDG_SESSION_TYPE").unwrap_or_default();
        if !session.eq_ignore_ascii_case("x11") {
            return Err(
                "Global input capture is blocked by Wayland. Sign out and choose Ubuntu on Xorg."
                    .into(),
            );
        }
        if std::env::var("DISPLAY").is_err() {
            return Err("The X11 DISPLAY variable is missing".into());
        }
        Ok(())
    }

    fn emit(
        sender: &mpsc::Sender<CapturedInput>,
        context: &Arc<Mutex<(CaptureContext, bool)>>,
        event: InputEventKind,
    ) {
        let (context, collector_owned) = context.lock().map(|value| value.clone()).unwrap_or((
            CaptureContext {
                application: "unknown".into(),
                window_title: String::new(),
            },
            false,
        ));
        let _ = sender.send(CapturedInput {
            context,
            event,
            collector_owned,
        });
    }

    #[derive(Default)]
    struct XInputParser {
        current: Option<XInputBlock>,
        last_motion: Option<Instant>,
    }

    #[derive(Default)]
    struct XInputBlock {
        kind: String,
        detail: Option<i32>,
        root: Option<(i32, i32)>,
    }

    impl XInputParser {
        fn push(&mut self, line: &str) -> Option<InputEventKind> {
            if line.starts_with("EVENT type") {
                let completed = self.take_event();
                self.current = Some(XInputBlock {
                    kind: line
                        .split_once('(')
                        .and_then(|(_, rest)| rest.split_once(')'))
                        .map(|(kind, _)| kind.trim().to_string())
                        .unwrap_or_default(),
                    ..XInputBlock::default()
                });
                return completed;
            }
            let block = self.current.as_mut()?;
            let trimmed = line.trim();
            if let Some(value) = trimmed.strip_prefix("detail:") {
                block.detail = value.trim().parse().ok();
            } else if let Some(value) = trimmed.strip_prefix("root:") {
                let mut coordinates = value.trim().split('/');
                block.root = coordinates
                    .next()
                    .and_then(|x| x.parse::<f64>().ok())
                    .zip(coordinates.next().and_then(|y| y.parse::<f64>().ok()))
                    .map(|(x, y)| (x.round() as i32, y.round() as i32));
            }
            None
        }

        fn finish(&mut self) -> Option<InputEventKind> {
            self.take_event()
        }

        fn take_event(&mut self) -> Option<InputEventKind> {
            let block = self.current.take()?;
            match block.kind.as_str() {
                "KeyPress" => block.detail.map(|code| InputEventKind::KeyDown {
                    key: format!("X11_{code}"),
                }),
                "KeyRelease" => block.detail.map(|code| InputEventKind::KeyUp {
                    key: format!("X11_{code}"),
                }),
                "ButtonPress" => button_event(block.detail?, true),
                "ButtonRelease" => button_event(block.detail?, false),
                "Motion" => {
                    let now = Instant::now();
                    if self.last_motion.is_some_and(|previous| {
                        now.duration_since(previous) < Duration::from_millis(16)
                    }) {
                        return None;
                    }
                    self.last_motion = Some(now);
                    block.root.map(|(x, y)| InputEventKind::MouseMove { x, y })
                }
                _ => None,
            }
        }
    }

    fn button_event(detail: i32, pressed: bool) -> Option<InputEventKind> {
        let button = match detail {
            1 => "left",
            2 => "middle",
            3 => "right",
            8 => "x1",
            9 => "x2",
            4 if pressed => {
                return Some(InputEventKind::Scroll {
                    delta_x: 0,
                    delta_y: 120,
                });
            }
            5 if pressed => {
                return Some(InputEventKind::Scroll {
                    delta_x: 0,
                    delta_y: -120,
                });
            }
            6 if pressed => {
                return Some(InputEventKind::Scroll {
                    delta_x: -120,
                    delta_y: 0,
                });
            }
            7 if pressed => {
                return Some(InputEventKind::Scroll {
                    delta_x: 120,
                    delta_y: 0,
                });
            }
            4..=7 => return None,
            _ => return None,
        };
        Some(if pressed {
            InputEventKind::MouseDown {
                button: button.into(),
            }
        } else {
            InputEventKind::MouseUp {
                button: button.into(),
            }
        })
    }

    fn foreground_context() -> (CaptureContext, bool) {
        let root = command_output("xprop", &["-root", "_NET_ACTIVE_WINDOW"]);
        let Some(window) = root
            .as_deref()
            .and_then(|value| value.split_whitespace().last())
            .filter(|value| *value != "0x0")
        else {
            return unknown_context();
        };
        let properties = command_output(
            "xprop",
            &["-id", window, "_NET_WM_PID", "_NET_WM_NAME", "WM_NAME"],
        )
        .unwrap_or_default();
        let process_id = properties
            .lines()
            .find(|line| line.starts_with("_NET_WM_PID"))
            .and_then(|line| line.rsplit_once('='))
            .and_then(|(_, value)| value.trim().parse::<u32>().ok());
        let window_title = properties
            .lines()
            .find(|line| line.starts_with("_NET_WM_NAME") || line.starts_with("WM_NAME"))
            .and_then(|line| line.split_once('='))
            .map(|(_, value)| value.trim().trim_matches('"').to_string())
            .unwrap_or_default();
        let application = process_id
            .and_then(|pid| command_output("ps", &["-p", &pid.to_string(), "-o", "comm="]))
            .map(|value| value.trim().to_string())
            .filter(|value| !value.is_empty())
            .unwrap_or_else(|| "unknown".into());
        (
            CaptureContext {
                application,
                window_title,
            },
            process_id == Some(std::process::id()),
        )
    }

    fn command_output(program: &str, arguments: &[&str]) -> Option<String> {
        Command::new(program)
            .args(arguments)
            .output()
            .ok()
            .filter(|output| output.status.success())
            .map(|output| String::from_utf8_lossy(&output.stdout).into_owned())
    }

    fn unknown_context() -> (CaptureContext, bool) {
        (
            CaptureContext {
                application: "unknown".into(),
                window_title: String::new(),
            },
            false,
        )
    }

    #[cfg(test)]
    mod tests {
        use super::*;

        #[test]
        fn parses_key_pointer_and_scroll_events_without_text() {
            let mut parser = XInputParser::default();
            assert_eq!(parser.push("EVENT type 2 (KeyPress)"), None);
            assert_eq!(parser.push("    detail: 38"), None);
            assert_eq!(
                parser.push("EVENT type 3 (KeyRelease)"),
                Some(InputEventKind::KeyDown {
                    key: "X11_38".into()
                })
            );
            assert_eq!(parser.push("    detail: 38"), None);
            assert_eq!(
                parser.push("EVENT type 4 (ButtonPress)"),
                Some(InputEventKind::KeyUp {
                    key: "X11_38".into()
                })
            );
            assert_eq!(parser.push("    detail: 4"), None);
            assert_eq!(
                parser.finish(),
                Some(InputEventKind::Scroll {
                    delta_x: 0,
                    delta_y: 120
                })
            );
        }

        #[test]
        fn parses_motion_coordinates() {
            let mut parser = XInputParser::default();
            assert_eq!(parser.push("EVENT type 6 (Motion)"), None);
            assert_eq!(parser.push("    root: 120.20/33.80"), None);
            assert_eq!(
                parser.finish(),
                Some(InputEventKind::MouseMove { x: 120, y: 34 })
            );
        }
    }
}

#[cfg(target_os = "linux")]
pub use linux::{NativeInputCapture, probe};

#[cfg(target_os = "macos")]
mod macos {
    use super::CapturedInput;
    use collector_core::{CaptureContext, InputEventKind};
    use core_foundation::base::{CFGetTypeID, CFRelease, CFTypeRef, TCFType};
    use core_foundation::runloop::{CFRunLoop, kCFRunLoopDefaultMode};
    use core_foundation::string::{CFString, CFStringGetTypeID, CFStringRef};
    use core_graphics::event::{
        CGEvent, CGEventTap, CGEventTapLocation, CGEventTapOptions, CGEventTapPlacement,
        CGEventType, CallbackResult, EventField,
    };
    use std::ffi::c_void;
    use std::ptr;
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::sync::{Arc, Mutex, mpsc};
    use std::thread::{self, JoinHandle};
    use std::time::{Duration, Instant};

    type AXUIElementRef = *const c_void;
    type AXError = i32;
    const AX_ERROR_SUCCESS: AXError = 0;

    #[link(name = "ApplicationServices", kind = "framework")]
    unsafe extern "C" {
        static kAXFocusedApplicationAttribute: CFStringRef;
        static kAXFocusedWindowAttribute: CFStringRef;
        static kAXTitleAttribute: CFStringRef;

        fn AXUIElementCreateSystemWide() -> AXUIElementRef;
        fn AXUIElementCopyAttributeValue(
            element: AXUIElementRef,
            attribute: CFStringRef,
            value: *mut CFTypeRef,
        ) -> AXError;
        fn AXUIElementGetPid(element: AXUIElementRef, pid: *mut i32) -> AXError;
    }

    pub struct NativeInputCapture {
        hook_thread: Option<JoinHandle<()>>,
        watcher_thread: Option<JoinHandle<()>>,
        dispatch_thread: Option<JoinHandle<()>>,
        sender: Option<mpsc::Sender<CapturedInput>>,
        stop_signal: Arc<AtomicBool>,
    }

    impl NativeInputCapture {
        pub fn start(
            handler: impl Fn(CapturedInput) + Send + Sync + 'static,
        ) -> Result<Self, String> {
            let stop_signal = Arc::new(AtomicBool::new(false));
            let context = Arc::new(Mutex::new(foreground_context()));
            let (sender, receiver) = mpsc::channel::<CapturedInput>();
            let dispatch_thread = thread::Builder::new()
                .name("trajectory-input-dispatch".into())
                .spawn(move || {
                    while let Ok(event) = receiver.recv() {
                        handler(event);
                    }
                })
                .map_err(|error| format!("start input dispatcher: {error}"))?;

            let hook_sender = sender.clone();
            let hook_context = Arc::clone(&context);
            let hook_stop = Arc::clone(&stop_signal);
            let (ready_sender, ready_receiver) = mpsc::sync_channel::<Result<(), String>>(1);
            let hook_thread = thread::Builder::new()
                .name("trajectory-macos-input".into())
                .spawn(move || {
                    let ready_for_loop = ready_sender.clone();
                    let last_motion = Arc::new(Mutex::new(None::<Instant>));
                    let callback_motion = Arc::clone(&last_motion);
                    let result = CGEventTap::with_enabled(
                        CGEventTapLocation::Session,
                        CGEventTapPlacement::TailAppendEventTap,
                        CGEventTapOptions::ListenOnly,
                        observed_event_types(),
                        move |_proxy, event_type, event| {
                            if let Some(input) = translate_event(
                                event_type,
                                event,
                                &callback_motion,
                            ) {
                                emit(&hook_sender, &hook_context, input);
                            }
                            CallbackResult::Keep
                        },
                        || {
                            let _ = ready_for_loop.send(Ok(()));
                            while !hook_stop.load(Ordering::Acquire) {
                                CFRunLoop::run_in_mode(
                                    unsafe { kCFRunLoopDefaultMode },
                                    Duration::from_millis(100),
                                    true,
                                );
                            }
                        },
                    );
                    if result.is_err() {
                        let _ = ready_sender.send(Err(
                            "macOS denied passive input monitoring. Enable DataTrains in System Settings > Privacy & Security > Accessibility and Input Monitoring, then restart it."
                                .into(),
                        ));
                    }
                })
                .map_err(|error| format!("start macOS input listener: {error}"))?;

            match ready_receiver.recv_timeout(Duration::from_secs(5)) {
                Ok(Ok(())) => {}
                Ok(Err(error)) => {
                    drop(sender);
                    let _ = hook_thread.join();
                    let _ = dispatch_thread.join();
                    return Err(error);
                }
                Err(error) => {
                    stop_signal.store(true, Ordering::Release);
                    drop(sender);
                    let _ = hook_thread.join();
                    let _ = dispatch_thread.join();
                    return Err(format!("macOS input listener did not initialize: {error}"));
                }
            }

            let watcher_sender = sender.clone();
            let watcher_context = Arc::clone(&context);
            let watcher_stop = Arc::clone(&stop_signal);
            let watcher_thread = thread::Builder::new()
                .name("trajectory-macos-focus-watcher".into())
                .spawn(move || {
                    while !watcher_stop.load(Ordering::Acquire) {
                        let current = foreground_context();
                        let changed = watcher_context
                            .lock()
                            .map(|mut previous| {
                                if *previous == current {
                                    false
                                } else {
                                    *previous = current.clone();
                                    true
                                }
                            })
                            .unwrap_or(false);
                        if changed {
                            let _ = watcher_sender.send(CapturedInput {
                                context: current.0.clone(),
                                event: InputEventKind::AppFocus {
                                    window_title: current.0.window_title.clone(),
                                },
                                collector_owned: current.1,
                            });
                        }
                        thread::sleep(Duration::from_millis(100));
                    }
                })
                .map_err(|error| format!("start macOS focus watcher: {error}"))?;

            Ok(Self {
                hook_thread: Some(hook_thread),
                watcher_thread: Some(watcher_thread),
                dispatch_thread: Some(dispatch_thread),
                sender: Some(sender),
                stop_signal,
            })
        }

        pub fn stop(mut self) -> Result<(), String> {
            self.stop_inner()
        }

        fn stop_inner(&mut self) -> Result<(), String> {
            if self.hook_thread.is_none() {
                return Ok(());
            }
            self.stop_signal.store(true, Ordering::Release);
            let mut failure = None;
            if let Some(thread) = self.hook_thread.take()
                && thread.join().is_err()
            {
                failure = Some("macOS input listener panicked".into());
            }
            if let Some(thread) = self.watcher_thread.take()
                && thread.join().is_err()
            {
                failure.get_or_insert_with(|| "macOS focus watcher panicked".into());
            }
            self.sender.take();
            if let Some(thread) = self.dispatch_thread.take()
                && thread.join().is_err()
            {
                failure.get_or_insert_with(|| "input dispatcher panicked".into());
            }
            failure.map_or(Ok(()), Err)
        }
    }

    impl Drop for NativeInputCapture {
        fn drop(&mut self) {
            let _ = self.stop_inner();
        }
    }

    pub fn probe() -> bool {
        NativeInputCapture::start(|_| {})
            .and_then(NativeInputCapture::stop)
            .is_ok()
    }

    fn observed_event_types() -> Vec<CGEventType> {
        vec![
            CGEventType::LeftMouseDown,
            CGEventType::LeftMouseUp,
            CGEventType::RightMouseDown,
            CGEventType::RightMouseUp,
            CGEventType::OtherMouseDown,
            CGEventType::OtherMouseUp,
            CGEventType::MouseMoved,
            CGEventType::LeftMouseDragged,
            CGEventType::RightMouseDragged,
            CGEventType::OtherMouseDragged,
            CGEventType::ScrollWheel,
            CGEventType::KeyDown,
            CGEventType::KeyUp,
        ]
    }

    fn translate_event(
        event_type: CGEventType,
        event: &CGEvent,
        last_motion: &Mutex<Option<Instant>>,
    ) -> Option<InputEventKind> {
        match event_type {
            CGEventType::LeftMouseDown => mouse_button("left", true),
            CGEventType::LeftMouseUp => mouse_button("left", false),
            CGEventType::RightMouseDown => mouse_button("right", true),
            CGEventType::RightMouseUp => mouse_button("right", false),
            CGEventType::OtherMouseDown => mouse_button(
                &macos_button(event.get_integer_value_field(EventField::MOUSE_EVENT_BUTTON_NUMBER)),
                true,
            ),
            CGEventType::OtherMouseUp => mouse_button(
                &macos_button(event.get_integer_value_field(EventField::MOUSE_EVENT_BUTTON_NUMBER)),
                false,
            ),
            CGEventType::MouseMoved
            | CGEventType::LeftMouseDragged
            | CGEventType::RightMouseDragged
            | CGEventType::OtherMouseDragged => {
                let now = Instant::now();
                let mut previous = last_motion.lock().ok()?;
                if previous
                    .is_some_and(|value| now.duration_since(value) < Duration::from_millis(16))
                {
                    return None;
                }
                *previous = Some(now);
                let point = event.location();
                Some(InputEventKind::MouseMove {
                    x: point.x.round() as i32,
                    y: point.y.round() as i32,
                })
            }
            CGEventType::ScrollWheel => Some(InputEventKind::Scroll {
                delta_x: event.get_integer_value_field(EventField::SCROLL_WHEEL_EVENT_DELTA_AXIS_2)
                    as i32,
                delta_y: event.get_integer_value_field(EventField::SCROLL_WHEEL_EVENT_DELTA_AXIS_1)
                    as i32,
            }),
            CGEventType::KeyDown => Some(InputEventKind::KeyDown {
                key: format!(
                    "MAC_{}",
                    event.get_integer_value_field(EventField::KEYBOARD_EVENT_KEYCODE)
                ),
            }),
            CGEventType::KeyUp => Some(InputEventKind::KeyUp {
                key: format!(
                    "MAC_{}",
                    event.get_integer_value_field(EventField::KEYBOARD_EVENT_KEYCODE)
                ),
            }),
            _ => None,
        }
    }

    fn mouse_button(button: &str, pressed: bool) -> Option<InputEventKind> {
        Some(if pressed {
            InputEventKind::MouseDown {
                button: button.into(),
            }
        } else {
            InputEventKind::MouseUp {
                button: button.into(),
            }
        })
    }

    fn macos_button(number: i64) -> String {
        match number {
            2 => "middle".into(),
            value => format!("button_{value}"),
        }
    }

    fn emit(
        sender: &mpsc::Sender<CapturedInput>,
        context: &Arc<Mutex<(CaptureContext, bool)>>,
        event: InputEventKind,
    ) {
        let (context, collector_owned) = context.lock().map(|value| value.clone()).unwrap_or((
            CaptureContext {
                application: "unknown".into(),
                window_title: String::new(),
            },
            false,
        ));
        let _ = sender.send(CapturedInput {
            context,
            event,
            collector_owned,
        });
    }

    fn foreground_context() -> (CaptureContext, bool) {
        unsafe {
            let system = AXUIElementCreateSystemWide();
            if system.is_null() {
                return unknown_context();
            }
            let application = copy_attribute(system, kAXFocusedApplicationAttribute);
            CFRelease(system as CFTypeRef);
            let Some(application) = application else {
                return unknown_context();
            };
            let application_element = application as AXUIElementRef;
            let application_name = copy_string_attribute(application_element, kAXTitleAttribute)
                .unwrap_or_else(|| "unknown".into());
            let window_title = copy_attribute(application_element, kAXFocusedWindowAttribute)
                .and_then(|window| {
                    let title = copy_string_attribute(window as AXUIElementRef, kAXTitleAttribute)
                        .unwrap_or_default();
                    CFRelease(window);
                    (!title.is_empty()).then_some(title)
                })
                .unwrap_or_default();
            let mut process_id = 0_i32;
            let owns_focus = AXUIElementGetPid(application_element, &mut process_id)
                == AX_ERROR_SUCCESS
                && process_id == std::process::id() as i32;
            CFRelease(application);
            (
                CaptureContext {
                    application: application_name,
                    window_title,
                },
                owns_focus,
            )
        }
    }

    unsafe fn copy_attribute(element: AXUIElementRef, attribute: CFStringRef) -> Option<CFTypeRef> {
        let mut value: CFTypeRef = ptr::null();
        if unsafe { AXUIElementCopyAttributeValue(element, attribute, &mut value) }
            == AX_ERROR_SUCCESS
            && !value.is_null()
        {
            Some(value)
        } else {
            None
        }
    }

    unsafe fn copy_string_attribute(
        element: AXUIElementRef,
        attribute: CFStringRef,
    ) -> Option<String> {
        let value = unsafe { copy_attribute(element, attribute) }?;
        if unsafe { CFGetTypeID(value) } != unsafe { CFStringGetTypeID() } {
            unsafe { CFRelease(value) };
            return None;
        }
        let string = unsafe { CFString::wrap_under_create_rule(value as CFStringRef) };
        Some(string.to_string())
    }

    fn unknown_context() -> (CaptureContext, bool) {
        (
            CaptureContext {
                application: "unknown".into(),
                window_title: String::new(),
            },
            false,
        )
    }
}

#[cfg(target_os = "macos")]
pub use macos::{NativeInputCapture, probe};

#[cfg(target_os = "windows")]
mod windows {
    use super::CapturedInput;
    use collector_core::{CaptureContext, InputEventKind};
    use std::ffi::OsString;
    use std::os::windows::ffi::OsStringExt;
    use std::path::PathBuf;
    use std::ptr::null_mut;
    use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};
    use std::sync::{Arc, Mutex, OnceLock, mpsc};
    use std::thread::{self, JoinHandle};
    use std::time::Duration;
    use windows_sys::Win32::Foundation::{CloseHandle, LPARAM, LRESULT, WPARAM};
    use windows_sys::Win32::System::Threading::{
        GetCurrentThreadId, OpenProcess, PROCESS_QUERY_LIMITED_INFORMATION,
        QueryFullProcessImageNameW,
    };
    use windows_sys::Win32::UI::WindowsAndMessaging::{
        CallNextHookEx, GetForegroundWindow, GetMessageW, GetWindowTextLengthW, GetWindowTextW,
        GetWindowThreadProcessId, HC_ACTION, KBDLLHOOKSTRUCT, MSG, MSLLHOOKSTRUCT, PM_NOREMOVE,
        PeekMessageW, PostThreadMessageW, SetWindowsHookExW, UnhookWindowsHookEx, WH_KEYBOARD_LL,
        WH_MOUSE_LL, WM_KEYDOWN, WM_KEYUP, WM_LBUTTONDOWN, WM_LBUTTONUP, WM_MBUTTONDOWN,
        WM_MBUTTONUP, WM_MOUSEHWHEEL, WM_MOUSEMOVE, WM_MOUSEWHEEL, WM_QUIT, WM_RBUTTONDOWN,
        WM_RBUTTONUP, WM_SYSKEYDOWN, WM_SYSKEYUP, WM_XBUTTONDOWN, WM_XBUTTONUP,
    };

    static EVENT_SENDER: OnceLock<Mutex<Option<mpsc::Sender<CapturedInput>>>> = OnceLock::new();
    static LAST_CONTEXT: OnceLock<Mutex<Option<(CaptureContext, bool)>>> = OnceLock::new();
    static LAST_MOUSE_MOVE_MS: AtomicU32 = AtomicU32::new(0);

    pub struct NativeInputCapture {
        thread_id: u32,
        hook_thread: Option<JoinHandle<()>>,
        watcher_thread: Option<JoinHandle<()>>,
        dispatch_thread: Option<JoinHandle<()>>,
        stop_signal: Arc<AtomicBool>,
    }

    impl NativeInputCapture {
        pub fn start(
            handler: impl Fn(CapturedInput) + Send + Sync + 'static,
        ) -> Result<Self, String> {
            let (event_sender, event_receiver) = mpsc::channel();
            let global = EVENT_SENDER.get_or_init(|| Mutex::new(None));
            let mut sender_guard = global
                .lock()
                .map_err(|_| "native input sender lock is poisoned".to_string())?;
            if sender_guard.is_some() {
                return Err("a native input hook is already active".into());
            }
            *sender_guard = Some(event_sender.clone());
            drop(sender_guard);
            *LAST_CONTEXT
                .get_or_init(|| Mutex::new(None))
                .lock()
                .map_err(|_| "native input context lock is poisoned".to_string())? = None;

            let handler = Arc::new(handler);
            let dispatch_thread = thread::Builder::new()
                .name("trajectory-input-dispatch".into())
                .spawn(move || {
                    while let Ok(event) = event_receiver.recv() {
                        handler(event);
                    }
                })
                .map_err(|error| {
                    clear_sender();
                    format!("start native input dispatcher: {error}")
                })?;

            let (startup_sender, startup_receiver) = mpsc::sync_channel(1);
            let hook_thread = thread::Builder::new()
                .name("trajectory-input-hooks".into())
                .spawn(move || unsafe {
                    let thread_id = GetCurrentThreadId();
                    let mut message = MSG::default();
                    PeekMessageW(&mut message, null_mut(), 0, 0, PM_NOREMOVE);
                    let keyboard =
                        SetWindowsHookExW(WH_KEYBOARD_LL, Some(keyboard_hook), null_mut(), 0);
                    if keyboard.is_null() {
                        let _ = startup_sender.send(Err(format!(
                            "install keyboard hook: {}",
                            std::io::Error::last_os_error()
                        )));
                        clear_sender();
                        return;
                    }
                    let mouse = SetWindowsHookExW(WH_MOUSE_LL, Some(mouse_hook), null_mut(), 0);
                    if mouse.is_null() {
                        UnhookWindowsHookEx(keyboard);
                        let _ = startup_sender.send(Err(format!(
                            "install mouse hook: {}",
                            std::io::Error::last_os_error()
                        )));
                        clear_sender();
                        return;
                    }
                    if startup_sender.send(Ok(thread_id)).is_err() {
                        UnhookWindowsHookEx(mouse);
                        UnhookWindowsHookEx(keyboard);
                        clear_sender();
                        return;
                    }
                    while GetMessageW(&mut message, null_mut(), 0, 0) > 0 {}
                    UnhookWindowsHookEx(mouse);
                    UnhookWindowsHookEx(keyboard);
                    clear_sender();
                })
                .map_err(|error| {
                    clear_sender();
                    format!("start native input hook thread: {error}")
                })?;

            match startup_receiver.recv() {
                Ok(Ok(thread_id)) => {
                    let stop_signal = Arc::new(AtomicBool::new(false));
                    let watcher_stop = Arc::clone(&stop_signal);
                    let watcher_thread = match thread::Builder::new()
                        .name("trajectory-focus-watcher".into())
                        .spawn(move || {
                            while !watcher_stop.load(Ordering::Acquire) {
                                let (context, collector_owned) = foreground_context();
                                emit_focus_if_changed(&event_sender, context, collector_owned);
                                thread::sleep(Duration::from_millis(100));
                            }
                        }) {
                        Ok(thread) => thread,
                        Err(error) => {
                            stop_signal.store(true, Ordering::Release);
                            unsafe {
                                PostThreadMessageW(thread_id, WM_QUIT, 0, 0);
                            }
                            let _ = hook_thread.join();
                            clear_sender();
                            let _ = dispatch_thread.join();
                            return Err(format!("start foreground context watcher: {error}"));
                        }
                    };
                    Ok(Self {
                        thread_id,
                        hook_thread: Some(hook_thread),
                        watcher_thread: Some(watcher_thread),
                        dispatch_thread: Some(dispatch_thread),
                        stop_signal,
                    })
                }
                Ok(Err(error)) => {
                    let _ = hook_thread.join();
                    let _ = dispatch_thread.join();
                    Err(error)
                }
                Err(error) => {
                    clear_sender();
                    let _ = hook_thread.join();
                    let _ = dispatch_thread.join();
                    Err(format!("native input hook did not start: {error}"))
                }
            }
        }

        pub fn stop(mut self) -> Result<(), String> {
            self.stop_inner()
        }

        fn stop_inner(&mut self) -> Result<(), String> {
            if self.hook_thread.is_none() {
                return Ok(());
            }
            self.stop_signal.store(true, Ordering::Release);
            let posted = unsafe { PostThreadMessageW(self.thread_id, WM_QUIT, 0, 0) };
            let mut failure = (posted == 0).then(|| {
                format!(
                    "stop native input hook: {}",
                    std::io::Error::last_os_error()
                )
            });
            if let Some(thread) = self.hook_thread.take()
                && thread.join().is_err()
            {
                failure.get_or_insert_with(|| "native input hook thread panicked".into());
            }
            if let Some(thread) = self.watcher_thread.take()
                && thread.join().is_err()
            {
                failure.get_or_insert_with(|| "foreground context watcher thread panicked".into());
            }
            clear_sender();
            if let Some(thread) = self.dispatch_thread.take()
                && thread.join().is_err()
            {
                failure.get_or_insert_with(|| "native input dispatcher thread panicked".into());
            }
            failure.map_or(Ok(()), Err)
        }
    }

    impl Drop for NativeInputCapture {
        fn drop(&mut self) {
            let _ = self.stop_inner();
        }
    }

    pub fn probe() -> bool {
        NativeInputCapture::start(|_| {})
            .and_then(NativeInputCapture::stop)
            .is_ok()
    }

    unsafe extern "system" fn keyboard_hook(code: i32, wparam: WPARAM, lparam: LPARAM) -> LRESULT {
        if code == HC_ACTION as i32 {
            let data = unsafe { &*(lparam as *const KBDLLHOOKSTRUCT) };
            if data.flags & 0x10 == 0 {
                let key = format!("VK_{:02X}", data.vkCode);
                let event = match wparam as u32 {
                    WM_KEYDOWN | WM_SYSKEYDOWN => Some(InputEventKind::KeyDown { key }),
                    WM_KEYUP | WM_SYSKEYUP => Some(InputEventKind::KeyUp { key }),
                    _ => None,
                };
                if let Some(event) = event {
                    dispatch(event);
                }
            }
        }
        unsafe { CallNextHookEx(null_mut(), code, wparam, lparam) }
    }

    unsafe extern "system" fn mouse_hook(code: i32, wparam: WPARAM, lparam: LPARAM) -> LRESULT {
        if code == HC_ACTION as i32 {
            let data = unsafe { &*(lparam as *const MSLLHOOKSTRUCT) };
            if data.flags & 0x01 == 0 {
                let message = wparam as u32;
                let event = match message {
                    WM_MOUSEMOVE => {
                        let previous = LAST_MOUSE_MOVE_MS.swap(data.time, Ordering::Relaxed);
                        (data.time.wrapping_sub(previous) >= 16).then_some(
                            InputEventKind::MouseMove {
                                x: data.pt.x,
                                y: data.pt.y,
                            },
                        )
                    }
                    WM_LBUTTONDOWN => Some(InputEventKind::MouseDown {
                        button: "left".into(),
                    }),
                    WM_LBUTTONUP => Some(InputEventKind::MouseUp {
                        button: "left".into(),
                    }),
                    WM_RBUTTONDOWN => Some(InputEventKind::MouseDown {
                        button: "right".into(),
                    }),
                    WM_RBUTTONUP => Some(InputEventKind::MouseUp {
                        button: "right".into(),
                    }),
                    WM_MBUTTONDOWN => Some(InputEventKind::MouseDown {
                        button: "middle".into(),
                    }),
                    WM_MBUTTONUP => Some(InputEventKind::MouseUp {
                        button: "middle".into(),
                    }),
                    WM_XBUTTONDOWN => Some(InputEventKind::MouseDown {
                        button: xbutton(data.mouseData),
                    }),
                    WM_XBUTTONUP => Some(InputEventKind::MouseUp {
                        button: xbutton(data.mouseData),
                    }),
                    WM_MOUSEWHEEL => Some(InputEventKind::Scroll {
                        delta_x: 0,
                        delta_y: wheel_delta(data.mouseData),
                    }),
                    WM_MOUSEHWHEEL => Some(InputEventKind::Scroll {
                        delta_x: wheel_delta(data.mouseData),
                        delta_y: 0,
                    }),
                    _ => None,
                };
                if let Some(event) = event {
                    dispatch(event);
                }
            }
        }
        unsafe { CallNextHookEx(null_mut(), code, wparam, lparam) }
    }

    fn dispatch(event: InputEventKind) {
        let Some(sender) = EVENT_SENDER
            .get()
            .and_then(|slot| slot.lock().ok())
            .and_then(|slot| slot.clone())
        else {
            return;
        };
        let (context, collector_owned) = foreground_context();
        emit_focus_if_changed(&sender, context.clone(), collector_owned);
        let _ = sender.send(CapturedInput {
            context,
            event,
            collector_owned,
        });
    }

    fn emit_focus_if_changed(
        sender: &mpsc::Sender<CapturedInput>,
        context: CaptureContext,
        collector_owned: bool,
    ) {
        if let Ok(mut last) = LAST_CONTEXT.get_or_init(|| Mutex::new(None)).lock()
            && last.as_ref() != Some(&(context.clone(), collector_owned))
        {
            let _ = sender.send(CapturedInput {
                context: context.clone(),
                event: InputEventKind::AppFocus {
                    window_title: context.window_title.clone(),
                },
                collector_owned,
            });
            *last = Some((context, collector_owned));
        }
    }

    fn foreground_context() -> (CaptureContext, bool) {
        unsafe {
            let window = GetForegroundWindow();
            if window.is_null() {
                return (
                    CaptureContext {
                        application: "unknown".into(),
                        window_title: String::new(),
                    },
                    false,
                );
            }
            let title_length = GetWindowTextLengthW(window).max(0) as usize;
            let mut title_buffer = vec![0u16; title_length.saturating_add(1)];
            let written =
                GetWindowTextW(window, title_buffer.as_mut_ptr(), title_buffer.len() as i32).max(0)
                    as usize;
            let window_title = String::from_utf16_lossy(&title_buffer[..written]);
            let mut process_id = 0;
            GetWindowThreadProcessId(window, &mut process_id);
            let collector_owned = process_id == std::process::id();
            let process = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, 0, process_id);
            let application = if process.is_null() {
                "unknown".into()
            } else {
                let mut path_buffer = vec![0u16; 32_768];
                let mut path_length = path_buffer.len() as u32;
                let queried = QueryFullProcessImageNameW(
                    process,
                    0,
                    path_buffer.as_mut_ptr(),
                    &mut path_length,
                );
                CloseHandle(process);
                if queried == 0 {
                    "unknown".into()
                } else {
                    PathBuf::from(OsString::from_wide(&path_buffer[..path_length as usize]))
                        .file_name()
                        .map(|name| name.to_string_lossy().into_owned())
                        .unwrap_or_else(|| "unknown".into())
                }
            };
            (
                CaptureContext {
                    application,
                    window_title,
                },
                collector_owned,
            )
        }
    }

    fn clear_sender() {
        if let Some(sender) = EVENT_SENDER.get()
            && let Ok(mut slot) = sender.lock()
        {
            *slot = None;
        }
    }

    fn wheel_delta(mouse_data: u32) -> i32 {
        ((mouse_data >> 16) as u16 as i16) as i32
    }

    fn xbutton(mouse_data: u32) -> String {
        format!("x{}", (mouse_data >> 16) & 0xffff)
    }
}

#[cfg(target_os = "windows")]
pub use windows::{NativeInputCapture, probe};
