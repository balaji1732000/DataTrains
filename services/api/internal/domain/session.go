package domain

import (
	"errors"
	"fmt"
	"time"
)

type SessionState string

const (
	SessionCreated        SessionState = "CREATED"
	SessionAssigned       SessionState = "ASSIGNED"
	SessionReady          SessionState = "READY"
	SessionRecording      SessionState = "RECORDING"
	SessionFinalizing     SessionState = "FINALIZING"
	SessionUploading      SessionState = "UPLOADING"
	SessionSubmitted      SessionState = "SUBMITTED"
	SessionProcessing     SessionState = "PROCESSING"
	SessionReadyForReview SessionState = "READY_FOR_REVIEW"
	SessionAccepted       SessionState = "ACCEPTED"
	SessionReleased       SessionState = "RELEASED"
	SessionFailed         SessionState = "FAILED"
	SessionRejected       SessionState = "REJECTED"
	SessionReworkRequired SessionState = "REWORK_REQUIRED"
	SessionCancelled      SessionState = "CANCELLED"
	SessionDeleted        SessionState = "DELETED"
)

var ErrInvalidTransition = errors.New("invalid session transition")

var allowedTransitions = map[SessionState]map[SessionState]struct{}{
	SessionCreated:        setOf(SessionAssigned, SessionCancelled),
	SessionAssigned:       setOf(SessionReady, SessionCancelled),
	SessionReady:          setOf(SessionRecording, SessionCancelled),
	SessionRecording:      setOf(SessionFinalizing, SessionFailed, SessionCancelled),
	SessionFinalizing:     setOf(SessionUploading, SessionFailed),
	SessionUploading:      setOf(SessionSubmitted, SessionFailed),
	SessionSubmitted:      setOf(SessionProcessing, SessionFailed),
	SessionProcessing:     setOf(SessionReadyForReview, SessionFailed),
	SessionReadyForReview: setOf(SessionAccepted, SessionRejected, SessionReworkRequired),
	SessionReworkRequired: setOf(SessionReady, SessionCancelled),
	SessionAccepted:       setOf(SessionReleased),
}

func setOf(states ...SessionState) map[SessionState]struct{} {
	result := make(map[SessionState]struct{}, len(states))
	for _, state := range states {
		result[state] = struct{}{}
	}
	return result
}

type Session struct {
	ID           string
	AssignmentID string
	State        SessionState
	UpdatedAt    time.Time
	AuditEvents  []AuditEvent
}

type TransitionCommand struct {
	To        SessionState
	Actor     string
	RequestID string
	Timestamp time.Time
}

func (session *Session) Transition(command TransitionCommand) error {
	if session.ID == "" {
		return errors.New("session ID is required")
	}
	if command.Actor == "" || command.RequestID == "" || command.Timestamp.IsZero() {
		return errors.New("transition actor, request ID, and timestamp are required")
	}

	if _, allowed := allowedTransitions[session.State][command.To]; !allowed {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, session.State, command.To)
	}

	from := session.State
	session.State = command.To
	session.UpdatedAt = command.Timestamp.UTC()
	session.AuditEvents = append(session.AuditEvents, AuditEvent{
		Actor:     command.Actor,
		Action:    "session.transitioned",
		Resource:  "session/" + session.ID,
		Timestamp: command.Timestamp.UTC(),
		RequestID: command.RequestID,
		Metadata: map[string]string{
			"from": string(from),
			"to":   string(command.To),
		},
	})
	return nil
}
