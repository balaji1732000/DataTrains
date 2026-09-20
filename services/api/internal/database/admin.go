package database

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"trajectory.local/api/internal/domain"
)

type ProjectSummary struct {
	ID, OrganizationID, Name                                          string
	TargetTrajectories                                                int
	TaskCount, SessionCount                                           int
	Assigned, Recording                                               int
	Processing, ReadyForReview, Accepted, Rejected, Released, Deleted int
}

func (store *Store) ListProjects(ctx context.Context) ([]ProjectSummary, error) {
	return store.listProjects(ctx, nil)
}

func (store *Store) ListProjectsForOrganizations(ctx context.Context, organizationIDs []string) ([]ProjectSummary, error) {
	if len(organizationIDs) == 0 {
		return []ProjectSummary{}, nil
	}
	return store.listProjects(ctx, organizationIDs)
}

func (store *Store) listProjects(ctx context.Context, organizationIDs []string) ([]ProjectSummary, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT p.id, p.organization_id, p.name, p.target_trajectories,
		       count(DISTINCT t.id), count(DISTINCT s.id),
		       count(DISTINCT s.id) FILTER (WHERE s.state IN ('CREATED','ASSIGNED','READY')),
		       count(DISTINCT s.id) FILTER (WHERE s.state IN ('RECORDING','FINALIZING','UPLOADING')),
		       count(DISTINCT s.id) FILTER (WHERE s.state IN ('SUBMITTED','PROCESSING')),
		       count(DISTINCT s.id) FILTER (WHERE s.state='READY_FOR_REVIEW'),
		       count(DISTINCT s.id) FILTER (WHERE s.state='ACCEPTED'),
		       count(DISTINCT s.id) FILTER (WHERE s.state IN ('REJECTED','REWORK_REQUIRED','FAILED','CANCELLED')),
		       count(DISTINCT s.id) FILTER (WHERE s.state='RELEASED'),
		       count(DISTINCT s.id) FILTER (WHERE s.state='DELETED')
		FROM projects p
		LEFT JOIN task_templates tt ON tt.project_id=p.id
		LEFT JOIN tasks t ON t.template_id=tt.id
		LEFT JOIN assignments a ON a.task_id=t.id
		LEFT JOIN sessions s ON s.assignment_id=a.id
		WHERE ($1::text[] IS NULL OR p.organization_id=ANY($1))
		GROUP BY p.id
		ORDER BY p.created_at DESC, p.id`, organizationIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := make([]ProjectSummary, 0)
	for rows.Next() {
		var project ProjectSummary
		if err := rows.Scan(
			&project.ID, &project.OrganizationID, &project.Name, &project.TargetTrajectories,
			&project.TaskCount, &project.SessionCount, &project.Assigned, &project.Recording,
			&project.Processing, &project.ReadyForReview, &project.Accepted, &project.Rejected,
			&project.Released, &project.Deleted,
		); err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	return projects, rows.Err()
}

type ProjectSession struct {
	SessionID, TaskID, Goal, ContributorID, ContributorName string
	State                                                   domain.SessionState
	UpdatedAt                                               time.Time
}

func (store *Store) ListProjectSessions(ctx context.Context, projectID string) ([]ProjectSession, error) {
	var exists bool
	if err := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id=$1)`, projectID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := store.pool.Query(ctx, `
		SELECT s.id, t.id, t.goal, c.id, c.display_name, s.state, s.updated_at
		FROM sessions s
		JOIN assignments a ON a.id=s.assignment_id
		JOIN contributors c ON c.id=a.contributor_id
		JOIN tasks t ON t.id=a.task_id
		JOIN task_templates tt ON tt.id=t.template_id
		WHERE tt.project_id=$1
		ORDER BY s.updated_at DESC, s.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessions := make([]ProjectSession, 0)
	for rows.Next() {
		var session ProjectSession
		if err := rows.Scan(&session.SessionID, &session.TaskID, &session.Goal, &session.ContributorID, &session.ContributorName, &session.State, &session.UpdatedAt); err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (store *Store) GetProject(ctx context.Context, projectID string) (domain.Project, error) {
	var project domain.Project
	err := store.pool.QueryRow(ctx, `SELECT id, organization_id, name, target_trajectories FROM projects WHERE id=$1`, projectID).
		Scan(&project.ID, &project.OrganizationID, &project.Name, &project.TargetTrajectories)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Project{}, ErrNotFound
	}
	return project, err
}

type CampaignSpec struct {
	OrganizationName, ProjectName                       string
	TargetTrajectories                                  int
	TemplateName, Goal, Category, Difficulty            string
	TemplateSpecification, InputAssets, ExpectedOutputs json.RawMessage
	ContributorName                                     string
}

type BootstrappedCampaign struct {
	OrganizationID, ProjectID, TemplateID, TaskID string
	ContributorID, AssignmentID, SessionID        string
}

type ProductionCampaignSpec struct {
	OrganizationID, ContributorID                       string
	ProjectName                                         string
	TargetTrajectories                                  int
	TemplateName, Goal, Category, Difficulty            string
	TemplateSpecification, InputAssets, ExpectedOutputs json.RawMessage
}

func (store *Store) CreateProductionCampaign(ctx context.Context, spec ProductionCampaignSpec, actor, requestID string, now time.Time) (BootstrappedCampaign, error) {
	prefixes := []string{"proj", "tmpl", "task", "assign", "sess"}
	identifiers := make([]string, len(prefixes))
	for index, prefix := range prefixes {
		identifier, err := newID(prefix)
		if err != nil {
			return BootstrappedCampaign{}, err
		}
		identifiers[index] = identifier
	}
	result := BootstrappedCampaign{
		OrganizationID: spec.OrganizationID, ContributorID: spec.ContributorID,
		ProjectID: identifiers[0], TemplateID: identifiers[1], TaskID: identifiers[2],
		AssignmentID: identifiers[3], SessionID: identifiers[4],
	}
	session := domain.Session{ID: result.SessionID, AssignmentID: result.AssignmentID, State: domain.SessionCreated}
	if err := session.Transition(domain.TransitionCommand{To: domain.SessionAssigned, Actor: actor, RequestID: requestID, Timestamp: now}); err != nil {
		return BootstrappedCampaign{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return BootstrappedCampaign{}, err
	}
	defer tx.Rollback(ctx)
	var contributorAllowed bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM contributors c
			JOIN organization_memberships m ON m.account_id=c.account_id
			WHERE c.id=$1 AND c.status='active' AND m.organization_id=$2
			  AND m.role='contributor' AND m.status='active'
		)`, spec.ContributorID, spec.OrganizationID).Scan(&contributorAllowed); err != nil {
		return BootstrappedCampaign{}, err
	}
	if !contributorAllowed {
		return BootstrappedCampaign{}, ErrNotFound
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO projects (id, organization_id, name, target_trajectories, created_at) VALUES ($1,$2,$3,$4,$5)`, []any{result.ProjectID, spec.OrganizationID, spec.ProjectName, spec.TargetTrajectories, now.UTC()}},
		{`INSERT INTO task_templates (id, project_id, name, goal, category, difficulty, specification, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, []any{result.TemplateID, result.ProjectID, spec.TemplateName, spec.Goal, spec.Category, spec.Difficulty, string(spec.TemplateSpecification), now.UTC()}},
		{`INSERT INTO tasks (id, template_id, goal, input_assets, expected_outputs, created_at) VALUES ($1,$2,$3,$4,$5,$6)`, []any{result.TaskID, result.TemplateID, spec.Goal, string(spec.InputAssets), string(spec.ExpectedOutputs), now.UTC()}},
		{`INSERT INTO assignments (id, task_id, contributor_id, created_at) VALUES ($1,$2,$3,$4)`, []any{result.AssignmentID, result.TaskID, spec.ContributorID, now.UTC()}},
		{`INSERT INTO sessions (id, assignment_id, state, created_at, updated_at) VALUES ($1,$2,$3,$4,$4)`, []any{result.SessionID, result.AssignmentID, session.State, now.UTC()}},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement.query, statement.args...); err != nil {
			return BootstrappedCampaign{}, err
		}
	}
	for _, event := range []domain.AuditEvent{
		{Actor: actor, Action: "project.created", Resource: "project/" + result.ProjectID, Timestamp: now.UTC(), RequestID: requestID},
		{Actor: actor, Action: "task_template.created", Resource: "task-template/" + result.TemplateID, Timestamp: now.UTC(), RequestID: requestID},
		{Actor: actor, Action: "task.created", Resource: "task/" + result.TaskID, Timestamp: now.UTC(), RequestID: requestID},
		{Actor: actor, Action: "task.assigned", Resource: "assignment/" + result.AssignmentID, Timestamp: now.UTC(), RequestID: requestID, Metadata: map[string]string{"task_id": result.TaskID, "contributor_id": spec.ContributorID}},
		session.AuditEvents[0],
	} {
		if err := insertAudit(ctx, tx, event); err != nil {
			return BootstrappedCampaign{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return BootstrappedCampaign{}, err
	}
	return result, nil
}

type OrganizationContributor struct {
	ID, DisplayName, Email string
}

func (store *Store) ListOrganizationContributors(ctx context.Context, organizationID string) ([]OrganizationContributor, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT c.id, c.display_name, COALESCE(a.email, '')
		FROM contributors c
		JOIN accounts a ON a.id=c.account_id
		JOIN organization_memberships m ON m.account_id=a.id
		WHERE m.organization_id=$1 AND m.role='contributor' AND m.status='active'
		  AND a.status='active' AND c.status='active'
		ORDER BY lower(c.display_name), c.id`, organizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	contributors := make([]OrganizationContributor, 0)
	for rows.Next() {
		var contributor OrganizationContributor
		if err := rows.Scan(&contributor.ID, &contributor.DisplayName, &contributor.Email); err != nil {
			return nil, err
		}
		contributors = append(contributors, contributor)
	}
	return contributors, rows.Err()
}

func (store *Store) BootstrapCampaign(ctx context.Context, spec CampaignSpec, actor, requestID string, now time.Time) (BootstrappedCampaign, error) {
	prefixes := []string{"org", "proj", "tmpl", "task", "contrib", "assign", "sess"}
	identifiers := make([]string, len(prefixes))
	for index, prefix := range prefixes {
		identifier, err := newID(prefix)
		if err != nil {
			return BootstrappedCampaign{}, err
		}
		identifiers[index] = identifier
	}
	result := BootstrappedCampaign{
		OrganizationID: identifiers[0], ProjectID: identifiers[1], TemplateID: identifiers[2],
		TaskID: identifiers[3], ContributorID: identifiers[4], AssignmentID: identifiers[5], SessionID: identifiers[6],
	}
	session := domain.Session{ID: result.SessionID, AssignmentID: result.AssignmentID, State: domain.SessionCreated}
	if err := session.Transition(domain.TransitionCommand{To: domain.SessionAssigned, Actor: actor, RequestID: requestID, Timestamp: now}); err != nil {
		return BootstrappedCampaign{}, err
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return BootstrappedCampaign{}, err
	}
	defer tx.Rollback(ctx)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO organizations (id, name, created_at) VALUES ($1,$2,$3)`, []any{result.OrganizationID, spec.OrganizationName, now.UTC()}},
		{`INSERT INTO projects (id, organization_id, name, target_trajectories, created_at) VALUES ($1,$2,$3,$4,$5)`, []any{result.ProjectID, result.OrganizationID, spec.ProjectName, spec.TargetTrajectories, now.UTC()}},
		{`INSERT INTO task_templates (id, project_id, name, goal, category, difficulty, specification, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, []any{result.TemplateID, result.ProjectID, spec.TemplateName, spec.Goal, spec.Category, spec.Difficulty, string(spec.TemplateSpecification), now.UTC()}},
		{`INSERT INTO tasks (id, template_id, goal, input_assets, expected_outputs, created_at) VALUES ($1,$2,$3,$4,$5,$6)`, []any{result.TaskID, result.TemplateID, spec.Goal, string(spec.InputAssets), string(spec.ExpectedOutputs), now.UTC()}},
		{`INSERT INTO contributors (id, display_name, created_at) VALUES ($1,$2,$3)`, []any{result.ContributorID, spec.ContributorName, now.UTC()}},
		{`INSERT INTO assignments (id, task_id, contributor_id, created_at) VALUES ($1,$2,$3,$4)`, []any{result.AssignmentID, result.TaskID, result.ContributorID, now.UTC()}},
		{`INSERT INTO sessions (id, assignment_id, state, created_at, updated_at) VALUES ($1,$2,$3,$4,$4)`, []any{result.SessionID, result.AssignmentID, session.State, now.UTC()}},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement.query, statement.args...); err != nil {
			return BootstrappedCampaign{}, err
		}
	}
	audits := []domain.AuditEvent{
		{Actor: actor, Action: "organization.created", Resource: "organization/" + result.OrganizationID, Timestamp: now.UTC(), RequestID: requestID},
		{Actor: actor, Action: "project.created", Resource: "project/" + result.ProjectID, Timestamp: now.UTC(), RequestID: requestID},
		{Actor: actor, Action: "task_template.created", Resource: "task-template/" + result.TemplateID, Timestamp: now.UTC(), RequestID: requestID},
		{Actor: actor, Action: "task.created", Resource: "task/" + result.TaskID, Timestamp: now.UTC(), RequestID: requestID},
		{Actor: actor, Action: "contributor.created", Resource: "contributor/" + result.ContributorID, Timestamp: now.UTC(), RequestID: requestID},
		{Actor: actor, Action: "task.assigned", Resource: "assignment/" + result.AssignmentID, Timestamp: now.UTC(), RequestID: requestID, Metadata: map[string]string{"task_id": result.TaskID, "contributor_id": result.ContributorID}},
		session.AuditEvents[0],
	}
	for _, event := range audits {
		if err := insertAudit(ctx, tx, event); err != nil {
			return BootstrappedCampaign{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return BootstrappedCampaign{}, err
	}
	return result, nil
}
