use serde::{Deserialize, Serialize};

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct RecordingPolicy {
    pub allowed_applications: Vec<String>,
    pub denied_applications: Vec<String>,
    pub denied_window_terms: Vec<String>,
    pub clipboard_capture: bool,
    pub recording_indicator_required: bool,
    pub task_scoped: bool,
}

impl RecordingPolicy {
    pub fn safe_default(required_application: impl Into<String>) -> Self {
        Self {
            allowed_applications: vec![required_application.into()],
            denied_applications: vec![
                "1password".into(),
                "bitwarden".into(),
                "keepass".into(),
                "banking".into(),
                "mail".into(),
                "outlook".into(),
                "slack".into(),
                "whatsapp".into(),
                "telegram".into(),
            ],
            denied_window_terms: vec![
                "password".into(),
                "passcode".into(),
                "authentication".into(),
                "sign in".into(),
                "bank".into(),
                "private message".into(),
            ],
            clipboard_capture: false,
            recording_indicator_required: true,
            task_scoped: true,
        }
    }

    pub fn evaluate(&self, context: &CaptureContext) -> PolicyDecision {
        if self.clipboard_capture || !self.recording_indicator_required || !self.task_scoped {
            return PolicyDecision::UnsafeConfiguration;
        }
        let application = context.application.to_lowercase();
        let window_title = context.window_title.to_lowercase();
        if self
            .denied_applications
            .iter()
            .any(|term| application.contains(&term.to_lowercase()))
            || self
                .denied_window_terms
                .iter()
                .any(|term| window_title.contains(&term.to_lowercase()))
        {
            return PolicyDecision::DeniedSensitiveContext;
        }
        if !self.allowed_applications.is_empty()
            && !self
                .allowed_applications
                .iter()
                .any(|allowed| application.contains(&allowed.to_lowercase()))
        {
            return PolicyDecision::DeniedNotAllowlisted;
        }
        PolicyDecision::Allowed
    }
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct CaptureContext {
    pub application: String,
    pub window_title: String,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PolicyDecision {
    Allowed,
    DeniedSensitiveContext,
    DeniedNotAllowlisted,
    UnsafeConfiguration,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn safe_policy_allows_only_the_task_application() {
        let policy = RecordingPolicy::safe_default("Photo Editor");
        assert_eq!(
            policy.evaluate(&CaptureContext {
                application: "Photo Editor Pro".into(),
                window_title: "Demo asset".into()
            }),
            PolicyDecision::Allowed
        );
        assert_eq!(
            policy.evaluate(&CaptureContext {
                application: "Terminal".into(),
                window_title: "Demo asset".into()
            }),
            PolicyDecision::DeniedNotAllowlisted
        );
        assert_eq!(
            policy.evaluate(&CaptureContext {
                application: "Photo Editor Pro".into(),
                window_title: "Enter password".into()
            }),
            PolicyDecision::DeniedSensitiveContext
        );
        assert!(!policy.clipboard_capture);
    }
}
