package domain

import (
	"errors"
	"testing"
	"time"
)

func TestValidTransitionChangesStateAndAppendsAuditEvent(t *testing.T) {
	now := time.Date(2026, 9, 7, 1, 2, 3, 0, time.FixedZone("MYT", 8*60*60))
	session := Session{ID: "sess_001", State: SessionReady}

	err := session.Transition(TransitionCommand{
		To: SessionRecording, Actor: "contrib_001", RequestID: "req_001", Timestamp: now,
	})
	if err != nil {
		t.Fatalf("Transition() error = %v", err)
	}
	if session.State != SessionRecording {
		t.Fatalf("state = %q, want %q", session.State, SessionRecording)
	}
	if len(session.AuditEvents) != 1 {
		t.Fatalf("audit events = %d, want 1", len(session.AuditEvents))
	}
	event := session.AuditEvents[0]
	if event.Metadata["from"] != "READY" || event.Metadata["to"] != "RECORDING" {
		t.Fatalf("audit metadata = %#v", event.Metadata)
	}
	if event.Timestamp.Location() != time.UTC {
		t.Fatalf("audit timestamp location = %v, want UTC", event.Timestamp.Location())
	}
}

func TestInvalidTransitionLeavesSessionUntouched(t *testing.T) {
	session := Session{ID: "sess_001", State: SessionReleased}
	err := session.Transition(TransitionCommand{
		To: SessionRecording, Actor: "admin_001", RequestID: "req_002", Timestamp: time.Now(),
	})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("error = %v, want ErrInvalidTransition", err)
	}
	if session.State != SessionReleased || len(session.AuditEvents) != 0 {
		t.Fatalf("invalid transition mutated session: %#v", session)
	}
}
