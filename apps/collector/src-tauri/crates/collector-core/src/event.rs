use serde::{Deserialize, Serialize};

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
pub struct TimedInputEvent {
    pub timestamp_ns: u64,
    pub application: String,
    #[serde(flatten)]
    pub event: InputEventKind,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum InputEventKind {
    MouseMove { x: i32, y: i32 },
    MouseDown { button: String },
    MouseUp { button: String },
    Scroll { delta_x: i32, delta_y: i32 },
    KeyDown { key: String },
    KeyUp { key: String },
    AppFocus { window_title: String },
}
