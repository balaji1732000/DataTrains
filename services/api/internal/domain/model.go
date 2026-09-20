package domain

import (
	"encoding/json"
	"time"
)

// IDs are opaque strings at the domain boundary. Persistence adapters are
// responsible for generating and parsing their chosen sortable ID format.
type Organization struct{ ID, Name string }
type Project struct {
	ID, OrganizationID, Name string
	TargetTrajectories       int
}
type TaskTemplate struct{ ID, ProjectID, Name, Goal, Category, Difficulty string }
type Task struct{ ID, TemplateID, Goal string }
type Contributor struct{ ID, DisplayName string }
type Assignment struct {
	ID, TaskID, ContributorID string
	CreatedAt                 time.Time
}
type ConsentDocument struct {
	ID, Version, TextHash string
	EffectiveDate         time.Time
}
type ConsentAcceptance struct {
	ID, ContributorID, DocumentID, ClientVersion string
	AcceptedAt                                   time.Time
}
type Artifact struct {
	ID, SessionID, Key, SHA256, MediaType string
	Size                                  int64
}
type Review struct {
	ID, SessionID, ReviewerID, RubricVersion, Decision, Comments, PIIReview string
	Scores                                                                  json.RawMessage
	RedactionDocument                                                       *RedactionDocument
	CreatedAt                                                               time.Time
}
type ProcessingJob struct {
	ID, SessionID, JobType, State, LastError string
	Attempt, ManualRequeues                  int
	AvailableAt, LeasedAt, FinishedAt        time.Time
	LeasedBy                                 string
	LeaseExpiresAt, DeadLetteredAt           time.Time
}
type ValidationResult struct {
	ID, JobID, SessionID, NormalizedKey, SchemaVersion, ValidatorVersion string
	Valid                                                                bool
	Errors                                                               []string
	CreatedAt                                                            time.Time
}
type LegalHold struct {
	ID, OrganizationID, SessionID, Reason, PlacedBy string
	PlacedAt                                        time.Time
	ReleasedBy                                      string
	ReleasedAt                                      time.Time
}
type RetentionPolicy struct {
	OrganizationID                    string
	RawDays, DerivedDays, ReleaseDays int
	UpdatedBy                         string
	CreatedAt, UpdatedAt              time.Time
}
type DeletionRequest struct {
	ID, OrganizationID, SessionID, Reason, State string
	ObjectKeys                                   []string
	Attempt, ManualRequeues                      int
	AvailableAt, RequestedAt, LeaseExpiresAt     time.Time
	LeasedBy, RequestedBy, LastError             string
}
type RetentionPurgeRequest struct {
	ID, OrganizationID, ResourceType, ResourceID, State string
	ObjectKeys                                          []string
	Attempt                                             int
	PolicyUpdatedAt, AvailableAt, RequestedAt           time.Time
	LeaseExpiresAt, CompletedAt                         time.Time
	LeasedBy, LastError                                 string
}
type RedactionRegion struct {
	StartNS, EndNS      int64
	X, Y, Width, Height float64
	Kind                string
}
type RedactionSegment struct {
	SegmentID, SourceKey, Decision string
	Regions                        []RedactionRegion
}
type RedactionDocument struct {
	SchemaVersion string
	Segments      []RedactionSegment
}
type RedactionPlan struct {
	ID, SessionID, ReviewerID, SchemaVersion, DocumentHash string
	Version                                                int
	Document                                               RedactionDocument
	CreatedAt                                              time.Time
}
type RedactionJob struct {
	ID, PlanID, SessionID, State, LeasedBy, OutputManifestKey, LastError string
	Attempt, ManualRequeues                                              int
	AvailableAt, LeaseExpiresAt, CreatedAt, UpdatedAt                    time.Time
	FinishedAt, DeadLetteredAt, PurgedAt                                 time.Time
}
type DatasetRelease struct {
	ID, ProjectID, Name, Profile, SchemaVersion, ExporterVersion, PipelineVersion string
	ConfigurationHash, ManifestHash                                               string
	BundleHash                                                                    string
	BundleSize                                                                    int64
	SourceSessionIDs, ObjectKeys                                                  []string
	CreatedAt, PurgedAt                                                           time.Time
}

type AuditEvent struct {
	Actor     string
	Action    string
	Resource  string
	Timestamp time.Time
	RequestID string
	Metadata  map[string]string
}
