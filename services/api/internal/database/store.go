package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"trajectory.local/api/internal/domain"
	"trajectory.local/api/internal/id"
)

var (
	ErrNotFound          = errors.New("not found")
	ErrConsentMissing    = errors.New("session consent is required")
	ErrInvitationInvalid = errors.New("invitation is invalid or expired")
)

type Store struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	return OpenWithMaxConnections(ctx, databaseURL, 0)
}

func OpenWithMaxConnections(ctx context.Context, databaseURL string, maximum int32) (*Store, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("configure database: %w", err)
	}
	if maximum > 0 {
		config.MaxConns = maximum
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("configure database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect database: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (store *Store) Close()              { store.pool.Close() }
func (store *Store) Pool() *pgxpool.Pool { return store.pool }

func newID(prefix string) (string, error) { return id.New(prefix) }

func insertAudit(ctx context.Context, tx pgx.Tx, event domain.AuditEvent) error {
	metadata, err := json.Marshal(event.Metadata)
	if err != nil {
		return fmt.Errorf("marshal audit metadata: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO audit_events (actor, action, resource, occurred_at, request_id, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		event.Actor, event.Action, event.Resource, event.Timestamp, event.RequestID, string(metadata))
	return err
}

func (store *Store) withAudit(ctx context.Context, event domain.AuditEvent, operation func(pgx.Tx) error) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := operation(tx); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (store *Store) CreateOrganization(ctx context.Context, name, actor, requestID string, now time.Time) (domain.Organization, error) {
	identifier, err := newID("org")
	if err != nil {
		return domain.Organization{}, err
	}
	organization := domain.Organization{ID: identifier, Name: name}
	err = store.withAudit(ctx, domain.AuditEvent{Actor: actor, Action: "organization.created", Resource: "organization/" + identifier, Timestamp: now.UTC(), RequestID: requestID}, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO organizations (id, name, created_at) VALUES ($1, $2, $3)`, identifier, name, now.UTC())
		return err
	})
	return organization, err
}

func (store *Store) CreateProject(ctx context.Context, organizationID, name string, target int, actor, requestID string, now time.Time) (domain.Project, error) {
	identifier, err := newID("proj")
	if err != nil {
		return domain.Project{}, err
	}
	project := domain.Project{ID: identifier, OrganizationID: organizationID, Name: name, TargetTrajectories: target}
	err = store.withAudit(ctx, domain.AuditEvent{Actor: actor, Action: "project.created", Resource: "project/" + identifier, Timestamp: now.UTC(), RequestID: requestID}, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO projects (id, organization_id, name, target_trajectories, created_at) VALUES ($1, $2, $3, $4, $5)`, identifier, organizationID, name, target, now.UTC())
		return err
	})
	return project, err
}

func (store *Store) CreateTaskTemplate(ctx context.Context, template domain.TaskTemplate, specification json.RawMessage, actor, requestID string, now time.Time) (domain.TaskTemplate, error) {
	identifier, err := newID("tmpl")
	if err != nil {
		return domain.TaskTemplate{}, err
	}
	template.ID = identifier
	err = store.withAudit(ctx, domain.AuditEvent{Actor: actor, Action: "task_template.created", Resource: "task-template/" + identifier, Timestamp: now.UTC(), RequestID: requestID}, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO task_templates (id, project_id, name, goal, category, difficulty, specification, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, identifier, template.ProjectID, template.Name, template.Goal, template.Category, template.Difficulty, string(specification), now.UTC())
		return err
	})
	return template, err
}

func (store *Store) CreateTask(ctx context.Context, templateID, goal string, inputAssets, expectedOutputs json.RawMessage, actor, requestID string, now time.Time) (domain.Task, error) {
	identifier, err := newID("task")
	if err != nil {
		return domain.Task{}, err
	}
	task := domain.Task{ID: identifier, TemplateID: templateID, Goal: goal}
	err = store.withAudit(ctx, domain.AuditEvent{Actor: actor, Action: "task.created", Resource: "task/" + identifier, Timestamp: now.UTC(), RequestID: requestID}, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tasks (id, template_id, goal, input_assets, expected_outputs, created_at) VALUES ($1,$2,$3,$4,$5,$6)`, identifier, templateID, goal, string(inputAssets), string(expectedOutputs), now.UTC())
		return err
	})
	return task, err
}

func (store *Store) CreateContributor(ctx context.Context, displayName, actor, requestID string, now time.Time) (domain.Contributor, error) {
	identifier, err := newID("contrib")
	if err != nil {
		return domain.Contributor{}, err
	}
	contributor := domain.Contributor{ID: identifier, DisplayName: displayName}
	err = store.withAudit(ctx, domain.AuditEvent{Actor: actor, Action: "contributor.created", Resource: "contributor/" + identifier, Timestamp: now.UTC(), RequestID: requestID}, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO contributors (id, display_name, created_at) VALUES ($1,$2,$3)`, identifier, displayName, now.UTC())
		return err
	})
	return contributor, err
}

type AssignmentSession struct {
	Assignment domain.Assignment
	Session    domain.Session
}

type ConsentDocumentText struct {
	ID, Version, TextHash, Body string
	EffectiveDate               time.Time
}

func (store *Store) CurrentConsentDocument(ctx context.Context, now time.Time) (ConsentDocumentText, error) {
	var document ConsentDocumentText
	err := store.pool.QueryRow(ctx, `
		SELECT id, version, text_hash, body, effective_date
		FROM consent_documents
		WHERE effective_date <= $1
		ORDER BY effective_date DESC, id DESC
		LIMIT 1`, now.UTC()).Scan(
		&document.ID, &document.Version, &document.TextHash, &document.Body, &document.EffectiveDate,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConsentDocumentText{}, ErrNotFound
	}
	return document, err
}

func (store *Store) AssignTask(ctx context.Context, taskID, contributorID, actor, requestID string, now time.Time) (AssignmentSession, error) {
	assignmentID, err := newID("assign")
	if err != nil {
		return AssignmentSession{}, err
	}
	sessionID, err := newID("sess")
	if err != nil {
		return AssignmentSession{}, err
	}
	session := domain.Session{ID: sessionID, AssignmentID: assignmentID, State: domain.SessionCreated}
	if err := session.Transition(domain.TransitionCommand{To: domain.SessionAssigned, Actor: actor, RequestID: requestID, Timestamp: now}); err != nil {
		return AssignmentSession{}, err
	}
	assignment := domain.Assignment{ID: assignmentID, TaskID: taskID, ContributorID: contributorID, CreatedAt: now.UTC()}

	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return AssignmentSession{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO assignments (id, task_id, contributor_id, created_at) VALUES ($1,$2,$3,$4)`, assignmentID, taskID, contributorID, now.UTC()); err != nil {
		return AssignmentSession{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sessions (id, assignment_id, state, created_at, updated_at) VALUES ($1,$2,$3,$4,$4)`, sessionID, assignmentID, session.State, now.UTC()); err != nil {
		return AssignmentSession{}, err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{Actor: actor, Action: "task.assigned", Resource: "assignment/" + assignmentID, Timestamp: now.UTC(), RequestID: requestID, Metadata: map[string]string{"task_id": taskID, "contributor_id": contributorID}}); err != nil {
		return AssignmentSession{}, err
	}
	if err := insertAudit(ctx, tx, session.AuditEvents[0]); err != nil {
		return AssignmentSession{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AssignmentSession{}, err
	}
	return AssignmentSession{Assignment: assignment, Session: session}, nil
}

func (store *Store) CreateConsentDocument(ctx context.Context, version, body, actor, requestID string, effectiveAt, now time.Time) (domain.ConsentDocument, error) {
	identifier, err := newID("consent")
	if err != nil {
		return domain.ConsentDocument{}, err
	}
	digest := sha256.Sum256([]byte(body))
	document := domain.ConsentDocument{ID: identifier, Version: version, TextHash: hex.EncodeToString(digest[:]), EffectiveDate: effectiveAt.UTC()}
	err = store.withAudit(ctx, domain.AuditEvent{Actor: actor, Action: "consent_document.created", Resource: "consent-document/" + identifier, Timestamp: now.UTC(), RequestID: requestID, Metadata: map[string]string{"version": version, "text_hash": document.TextHash}}, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO consent_documents (id, version, text_hash, body, effective_date) VALUES ($1,$2,$3,$4,$5)`, identifier, version, document.TextHash, body, effectiveAt.UTC())
		return err
	})
	return document, err
}

func (store *Store) AcceptConsent(ctx context.Context, sessionID, contributorID, documentID, clientVersion, actor, requestID string, now time.Time) (domain.ConsentAcceptance, error) {
	identifier, err := newID("accept")
	if err != nil {
		return domain.ConsentAcceptance{}, err
	}
	acceptance := domain.ConsentAcceptance{ID: identifier, ContributorID: contributorID, DocumentID: documentID, ClientVersion: clientVersion, AcceptedAt: now.UTC()}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.ConsentAcceptance{}, err
	}
	defer tx.Rollback(ctx)
	var assignedContributor string
	var sessionState domain.SessionState
	var effectiveDate time.Time
	err = tx.QueryRow(ctx, `
		SELECT a.contributor_id, s.state, d.effective_date
		FROM sessions s
		JOIN assignments a ON a.id=s.assignment_id
		JOIN consent_documents d ON d.id=$2
		WHERE s.id=$1 FOR UPDATE OF s`, sessionID, documentID).Scan(&assignedContributor, &sessionState, &effectiveDate)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ConsentAcceptance{}, ErrNotFound
	}
	if err != nil {
		return domain.ConsentAcceptance{}, err
	}
	if assignedContributor != contributorID {
		return domain.ConsentAcceptance{}, fmt.Errorf("contributor does not own session")
	}
	if sessionState != domain.SessionAssigned {
		return domain.ConsentAcceptance{}, fmt.Errorf("%w: consent requires ASSIGNED, current state is %s", domain.ErrInvalidTransition, sessionState)
	}
	if effectiveDate.After(now) {
		return domain.ConsentAcceptance{}, fmt.Errorf("consent document is not yet effective")
	}
	inserted := tx.QueryRow(ctx, `
		INSERT INTO consent_acceptances (id, contributor_id, document_id, accepted_at, client_version)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (contributor_id, document_id) DO NOTHING
		RETURNING id, accepted_at, client_version`, identifier, contributorID, documentID, now.UTC(), clientVersion).
		Scan(&acceptance.ID, &acceptance.AcceptedAt, &acceptance.ClientVersion)
	if errors.Is(inserted, pgx.ErrNoRows) {
		inserted = tx.QueryRow(ctx, `
			SELECT id, accepted_at, client_version FROM consent_acceptances
			WHERE contributor_id=$1 AND document_id=$2`, contributorID, documentID).
			Scan(&acceptance.ID, &acceptance.AcceptedAt, &acceptance.ClientVersion)
	}
	if inserted != nil {
		return domain.ConsentAcceptance{}, inserted
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET consent_acceptance_id=$1, updated_at=$2 WHERE id=$3`, acceptance.ID, now.UTC(), sessionID); err != nil {
		return domain.ConsentAcceptance{}, err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{Actor: actor, Action: "consent.accepted", Resource: "session/" + sessionID, Timestamp: now.UTC(), RequestID: requestID, Metadata: map[string]string{"document_id": documentID, "acceptance_id": acceptance.ID}}); err != nil {
		return domain.ConsentAcceptance{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ConsentAcceptance{}, err
	}
	return acceptance, nil
}

func (store *Store) GetSession(ctx context.Context, sessionID string) (domain.Session, error) {
	var session domain.Session
	err := store.pool.QueryRow(ctx, `SELECT id, assignment_id, state, updated_at FROM sessions WHERE id=$1`, sessionID).Scan(&session.ID, &session.AssignmentID, &session.State, &session.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Session{}, ErrNotFound
	}
	return session, err
}

func (store *Store) TransitionSession(ctx context.Context, sessionID string, to domain.SessionState, actor, requestID string, now time.Time) (domain.Session, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return domain.Session{}, err
	}
	defer tx.Rollback(ctx)
	var session domain.Session
	var consentAcceptanceID *string
	err = tx.QueryRow(ctx, `SELECT id, assignment_id, state, updated_at, consent_acceptance_id FROM sessions WHERE id=$1 FOR UPDATE`, sessionID).Scan(&session.ID, &session.AssignmentID, &session.State, &session.UpdatedAt, &consentAcceptanceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Session{}, ErrNotFound
	}
	if err != nil {
		return domain.Session{}, err
	}
	if to == domain.SessionReady && consentAcceptanceID == nil {
		return domain.Session{}, ErrConsentMissing
	}
	if err := session.Transition(domain.TransitionCommand{To: to, Actor: actor, RequestID: requestID, Timestamp: now}); err != nil {
		return domain.Session{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET state=$1, updated_at=$2 WHERE id=$3`, session.State, session.UpdatedAt, session.ID); err != nil {
		return domain.Session{}, err
	}
	if err := insertAudit(ctx, tx, session.AuditEvents[len(session.AuditEvents)-1]); err != nil {
		return domain.Session{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Session{}, err
	}
	return session, nil
}

type ContributorAssignment struct {
	AssignmentID        string              `json:"assignment_id"`
	SessionID           string              `json:"session_id"`
	SessionState        domain.SessionState `json:"session_state"`
	TaskID              string              `json:"task_id"`
	TemplateID          string              `json:"template_id"`
	Goal                string              `json:"goal"`
	Category            string              `json:"category"`
	Difficulty          string              `json:"difficulty"`
	RequiredApplication string              `json:"required_application"`
	InputAssets         json.RawMessage     `json:"input_assets"`
	ExpectedOutputs     json.RawMessage     `json:"expected_outputs"`
	Specification       json.RawMessage     `json:"specification"`
	ReworkOfSessionID   *string             `json:"rework_of_session_id,omitempty"`
	ReworkComments      *string             `json:"rework_comments,omitempty"`
	ReviewDecision      *string             `json:"review_decision,omitempty"`
	ReviewComments      *string             `json:"review_comments,omitempty"`
	ReviewedAt          *time.Time          `json:"reviewed_at,omitempty"`
	ConsentDocumentID   *string             `json:"consent_document_id,omitempty"`
	ConsentVersion      *string             `json:"consent_version,omitempty"`
	ConsentTextHash     *string             `json:"consent_text_hash,omitempty"`
	ConsentAcceptedAt   *time.Time          `json:"consent_accepted_at,omitempty"`
}

func (store *Store) ListContributorAssignments(ctx context.Context, contributorID string) ([]ContributorAssignment, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT a.id, s.id, s.state, t.id, tt.id, t.goal, tt.category, tt.difficulty,
		       COALESCE(tt.specification->>'required_application', ''),
		       t.input_assets, t.expected_outputs, tt.specification,
		       s.rework_of_session_id, rework_review.comments,
		       latest_review.decision, latest_review.comments, latest_review.created_at,
		       cd.id, cd.version, cd.text_hash, ca.accepted_at
		FROM assignments a
		JOIN sessions s ON s.assignment_id=a.id
		JOIN tasks t ON t.id=a.task_id
		JOIN task_templates tt ON tt.id=t.template_id
		LEFT JOIN consent_acceptances ca ON ca.id=s.consent_acceptance_id
		LEFT JOIN consent_documents cd ON cd.id=ca.document_id
		LEFT JOIN LATERAL (
			SELECT r.comments
			FROM reviews r
			WHERE r.session_id=s.rework_of_session_id AND r.decision='rework_required'
			ORDER BY r.created_at DESC, r.id DESC
			LIMIT 1
		) rework_review ON true
		LEFT JOIN LATERAL (
			SELECT r.decision, r.comments, r.created_at
			FROM reviews r
			WHERE r.session_id=s.id
			ORDER BY r.created_at DESC, r.id DESC
			LIMIT 1
		) latest_review ON true
		WHERE a.contributor_id=$1 ORDER BY a.created_at, s.created_at, s.id`, contributorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	assignments := make([]ContributorAssignment, 0)
	for rows.Next() {
		var item ContributorAssignment
		if err := rows.Scan(
			&item.AssignmentID, &item.SessionID, &item.SessionState, &item.TaskID, &item.TemplateID,
			&item.Goal, &item.Category, &item.Difficulty, &item.RequiredApplication,
			&item.InputAssets, &item.ExpectedOutputs, &item.Specification,
			&item.ReworkOfSessionID, &item.ReworkComments,
			&item.ReviewDecision, &item.ReviewComments, &item.ReviewedAt,
			&item.ConsentDocumentID, &item.ConsentVersion, &item.ConsentTextHash,
			&item.ConsentAcceptedAt,
		); err != nil {
			return nil, err
		}
		assignments = append(assignments, item)
	}
	return assignments, rows.Err()
}
