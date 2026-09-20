package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"trajectory.local/api/internal/domain"
)

var ErrInvalidRetentionPolicy = errors.New("organization and retention periods from 1 to 3650 days are required")

func (store *Store) GetRetentionPolicy(ctx context.Context, organizationID string) (domain.RetentionPolicy, error) {
	var policy domain.RetentionPolicy
	err := store.pool.QueryRow(ctx, `
		SELECT organization_id, raw_days, derived_days, release_days, updated_by, created_at, updated_at
		FROM retention_policies WHERE organization_id=$1`, organizationID).Scan(
		&policy.OrganizationID, &policy.RawDays, &policy.DerivedDays, &policy.ReleaseDays,
		&policy.UpdatedBy, &policy.CreatedAt, &policy.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RetentionPolicy{}, ErrNotFound
	}
	return policy, err
}

func (store *Store) UpsertRetentionPolicy(ctx context.Context, policy domain.RetentionPolicy, actor, requestID string, now time.Time) (domain.RetentionPolicy, error) {
	if policy.OrganizationID == "" || policy.RawDays < 1 || policy.RawDays > 3650 ||
		policy.DerivedDays < 1 || policy.DerivedDays > 3650 || policy.ReleaseDays < 1 || policy.ReleaseDays > 3650 {
		return domain.RetentionPolicy{}, ErrInvalidRetentionPolicy
	}
	policy.UpdatedBy = actor
	policy.UpdatedAt = now.UTC()
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return domain.RetentionPolicy{}, err
	}
	defer tx.Rollback(ctx)
	var purgeLeased bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM retention_purge_requests WHERE organization_id=$1 AND state='leased'
	)`, policy.OrganizationID).Scan(&purgeLeased); err != nil {
		return domain.RetentionPolicy{}, err
	}
	if purgeLeased {
		return domain.RetentionPolicy{}, ErrRetentionPurgeInProgress
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO retention_policies
		  (organization_id, raw_days, derived_days, release_days, updated_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$6)
		ON CONFLICT (organization_id) DO UPDATE SET
		  raw_days=EXCLUDED.raw_days, derived_days=EXCLUDED.derived_days,
		  release_days=EXCLUDED.release_days, updated_by=EXCLUDED.updated_by,
		  updated_at=EXCLUDED.updated_at
		RETURNING created_at`, policy.OrganizationID, policy.RawDays, policy.DerivedDays,
		policy.ReleaseDays, actor, policy.UpdatedAt).Scan(&policy.CreatedAt)
	if err != nil {
		return domain.RetentionPolicy{}, err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: actor, Action: "retention_policy.updated", Resource: "organization/" + policy.OrganizationID,
		Timestamp: policy.UpdatedAt, RequestID: requestID,
		Metadata: map[string]string{
			"raw_days": fmt.Sprint(policy.RawDays), "derived_days": fmt.Sprint(policy.DerivedDays),
			"release_days": fmt.Sprint(policy.ReleaseDays),
		},
	}); err != nil {
		return domain.RetentionPolicy{}, err
	}
	return policy, tx.Commit(ctx)
}
