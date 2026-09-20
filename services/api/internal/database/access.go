package database

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

type SessionAccess struct {
	OrganizationID string
	ContributorID  string
}

func (store *Store) GetSessionAccess(ctx context.Context, sessionID string) (SessionAccess, error) {
	var access SessionAccess
	err := store.pool.QueryRow(ctx, `
		SELECT p.organization_id, a.contributor_id
		FROM sessions s
		JOIN assignments a ON a.id=s.assignment_id
		JOIN tasks t ON t.id=a.task_id
		JOIN task_templates tt ON tt.id=t.template_id
		JOIN projects p ON p.id=tt.project_id
		WHERE s.id=$1`, sessionID).Scan(&access.OrganizationID, &access.ContributorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionAccess{}, ErrNotFound
	}
	return access, err
}

func (store *Store) GetProjectOrganization(ctx context.Context, projectID string) (string, error) {
	var organizationID string
	err := store.pool.QueryRow(ctx, `SELECT organization_id FROM projects WHERE id=$1`, projectID).Scan(&organizationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return organizationID, err
}

func (store *Store) GetTaskTemplateOrganization(ctx context.Context, templateID string) (string, error) {
	var organizationID string
	err := store.pool.QueryRow(ctx, `
		SELECT p.organization_id
		FROM task_templates tt JOIN projects p ON p.id=tt.project_id
		WHERE tt.id=$1`, templateID).Scan(&organizationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return organizationID, err
}

func (store *Store) GetTaskOrganization(ctx context.Context, taskID string) (string, error) {
	var organizationID string
	err := store.pool.QueryRow(ctx, `
		SELECT p.organization_id
		FROM tasks t
		JOIN task_templates tt ON tt.id=t.template_id
		JOIN projects p ON p.id=tt.project_id
		WHERE t.id=$1`, taskID).Scan(&organizationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return organizationID, err
}

func (store *Store) ContributorHasOrganization(ctx context.Context, contributorID, organizationID string) (bool, error) {
	var allowed bool
	err := store.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1
			FROM contributors c
			JOIN organization_memberships m ON m.account_id=c.account_id
			WHERE c.id=$1 AND m.organization_id=$2 AND m.role='contributor'
			  AND c.status='active' AND m.status='active'
		)`, contributorID, organizationID).Scan(&allowed)
	return allowed, err
}

func (store *Store) GetReleaseOrganization(ctx context.Context, releaseID string) (string, error) {
	var organizationID string
	err := store.pool.QueryRow(ctx, `
		SELECT p.organization_id
		FROM dataset_releases r JOIN projects p ON p.id=r.project_id
		WHERE r.id=$1`, releaseID).Scan(&organizationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return organizationID, err
}

func (store *Store) GetProcessingJobAccess(ctx context.Context, jobID string) (SessionAccess, error) {
	var sessionID string
	err := store.pool.QueryRow(ctx, `SELECT session_id FROM processing_jobs WHERE id=$1`, jobID).Scan(&sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionAccess{}, ErrNotFound
	}
	if err != nil {
		return SessionAccess{}, err
	}
	return store.GetSessionAccess(ctx, sessionID)
}
