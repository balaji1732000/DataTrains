package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"trajectory.local/api/internal/domain"
)

var (
	ErrNoRetentionPurge         = errors.New("no retention purge request is available")
	ErrRetentionLease           = errors.New("retention purge lease is not owned by this worker")
	ErrRetentionPurgeInProgress = errors.New("a retention worker is already purging affected data")
	ErrRetentionNotDead         = errors.New("retention purge request is not dead-lettered")
)

type retentionCandidate struct {
	organizationID, resourceType, resourceID string
	policyUpdatedAt                          time.Time
	objectKeys                               []string
}

// QueueExpiredRetention snapshots object keys into durable jobs. The worker
// rechecks the current policy and legal holds when it leases each job.
func (store *Store) QueueExpiredRetention(ctx context.Context, actor string, now time.Time, limit int) (int, error) {
	if actor == "" || limit < 1 || limit > 1000 {
		return 0, errors.New("actor and a limit from 1 to 1000 are required")
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	candidates := make([]retentionCandidate, 0, limit)
	appendSessions := func(resourceType, daysColumn, states string, derived bool) error {
		if len(candidates) >= limit {
			return nil
		}
		keyExpression := "a.logical_key"
		join := "JOIN artifacts a ON a.session_id=s.id AND a.purged_at IS NULL"
		anchor := "s.updated_at"
		if derived {
			keyExpression = "k.object_key"
			join = `JOIN validation_results vr ON vr.session_id=s.id AND vr.valid AND vr.purged_at IS NULL
			JOIN LATERAL (
			  SELECT vr.normalized_key AS object_key
			  UNION SELECT 'derived/sessions/' || s.id || '/actions.jsonl'
			  UNION
			  SELECT rj.output_manifest_key
			  FROM redaction_jobs rj
			  WHERE rj.session_id=s.id AND rj.state='completed' AND rj.output_manifest_key IS NOT NULL AND rj.purged_at IS NULL
			  UNION
			  SELECT 'derived/sessions/' || s.id || '/redactions/' || rp2.id || '/' || (segment->>'segment_id') || '.mp4'
			  FROM redaction_plans rp2
			  JOIN redaction_jobs rj2 ON rj2.plan_id=rp2.id AND rj2.state='completed' AND rj2.purged_at IS NULL
			  CROSS JOIN LATERAL jsonb_array_elements(rp2.document->'segments') segment
			  WHERE rp2.session_id=s.id
			) k ON k.object_key IS NOT NULL`
			anchor = `GREATEST(vr.created_at,s.updated_at,COALESCE((
			  SELECT max(rj3.finished_at) FROM redaction_jobs rj3
			  WHERE rj3.session_id=s.id AND rj3.state='completed' AND rj3.purged_at IS NULL
			),'epoch'::timestamptz))`
		}
		query := fmt.Sprintf(`
			SELECT p.organization_id, s.id, rp.updated_at, jsonb_agg(DISTINCT %s)
			FROM sessions s
			JOIN assignments a0 ON a0.id=s.assignment_id
			JOIN tasks t ON t.id=a0.task_id
			JOIN task_templates tt ON tt.id=t.template_id
			JOIN projects p ON p.id=tt.project_id
			JOIN retention_policies rp ON rp.organization_id=p.organization_id
			%s
			WHERE s.state IN (%s)
			  AND %s <= $1::timestamptz - (rp.%s * interval '1 day')
			  AND NOT EXISTS (SELECT 1 FROM legal_holds h WHERE h.session_id=s.id AND h.released_at IS NULL)
			  AND NOT EXISTS (
			    SELECT 1 FROM retention_purge_requests q
			    WHERE q.resource_type=$2 AND q.resource_id=s.id
			      AND q.state IN ('queued','leased','blocked','completed')
			  )
			GROUP BY p.organization_id, s.id, rp.updated_at
			ORDER BY s.id LIMIT $3`, keyExpression, join, states, anchor, daysColumn)
		rows, err := tx.Query(ctx, query, now.UTC(), resourceType, limit-len(candidates))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var candidate retentionCandidate
			var keys []byte
			candidate.resourceType = resourceType
			if err := rows.Scan(&candidate.organizationID, &candidate.resourceID, &candidate.policyUpdatedAt, &keys); err != nil {
				return err
			}
			if err := json.Unmarshal(keys, &candidate.objectKeys); err != nil {
				return err
			}
			candidates = append(candidates, candidate)
		}
		return rows.Err()
	}

	if err := appendSessions("session_raw", "raw_days", "'ACCEPTED','RELEASED','FAILED','REJECTED','REWORK_REQUIRED','CANCELLED'", false); err != nil {
		return 0, err
	}
	if err := appendSessions("session_derived", "derived_days", "'RELEASED','FAILED','REJECTED','REWORK_REQUIRED','CANCELLED'", true); err != nil {
		return 0, err
	}
	if len(candidates) < limit {
		rows, err := tx.Query(ctx, `
			SELECT p.organization_id, r.id, rp.updated_at, r.object_keys
			FROM dataset_releases r
			JOIN projects p ON p.id=r.project_id
			JOIN retention_policies rp ON rp.organization_id=p.organization_id
			WHERE r.purged_at IS NULL
			  AND r.created_at <= $1::timestamptz - (rp.release_days * interval '1 day')
			  AND NOT EXISTS (
			    SELECT 1 FROM release_sessions rs JOIN legal_holds h ON h.session_id=rs.session_id
			    WHERE rs.release_id=r.id AND h.released_at IS NULL
			  )
			  AND NOT EXISTS (
			    SELECT 1 FROM retention_purge_requests q
			    WHERE q.resource_type='release' AND q.resource_id=r.id
			      AND q.state IN ('queued','leased','blocked','completed')
			  )
			ORDER BY r.id LIMIT $2`, now.UTC(), limit-len(candidates))
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var candidate retentionCandidate
			var objectKeysJSON []byte
			candidate.resourceType = "release"
			if err := rows.Scan(&candidate.organizationID, &candidate.resourceID, &candidate.policyUpdatedAt, &objectKeysJSON); err != nil {
				rows.Close()
				return 0, err
			}
			if err := json.Unmarshal(objectKeysJSON, &candidate.objectKeys); err != nil {
				rows.Close()
				return 0, err
			}
			candidates = append(candidates, candidate)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return 0, err
		}
		rows.Close()
	}

	queued := 0
	for _, candidate := range candidates {
		id, err := newID("retention")
		if err != nil {
			return 0, err
		}
		keys, err := json.Marshal(candidate.objectKeys)
		if err != nil {
			return 0, err
		}
		result, err := tx.Exec(ctx, `
			INSERT INTO retention_purge_requests
			  (id, organization_id, resource_type, resource_id, policy_updated_at, object_keys,
			   state, available_at, requested_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,'queued',$7,$7,$7)
			ON CONFLICT DO NOTHING`, id, candidate.organizationID, candidate.resourceType,
			candidate.resourceID, candidate.policyUpdatedAt, keys, now.UTC())
		if err != nil {
			return 0, err
		}
		if result.RowsAffected() == 0 {
			continue
		}
		queued++
		if err := insertAudit(ctx, tx, domain.AuditEvent{
			Actor: actor, Action: "retention.purge_queued", Resource: "retention-purge/" + id,
			Timestamp: now.UTC(), RequestID: id,
			Metadata: map[string]string{
				"organization_id": candidate.organizationID, "resource_type": candidate.resourceType,
				"resource_id": candidate.resourceID, "object_count": fmt.Sprint(len(candidate.objectKeys)),
			},
		}); err != nil {
			return 0, err
		}
	}
	return queued, tx.Commit(ctx)
}

func (store *Store) ClaimRetentionPurge(ctx context.Context, workerID string, leaseDuration time.Duration, now time.Time) (domain.RetentionPurgeRequest, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return domain.RetentionPurgeRequest{}, err
	}
	defer tx.Rollback(ctx)
	var request domain.RetentionPurgeRequest
	var keys []byte
	err = tx.QueryRow(ctx, `
		SELECT id, organization_id, resource_type, resource_id, policy_updated_at, object_keys,
		       state, attempt, available_at, COALESCE(leased_by,''),
		       COALESCE(lease_expires_at,'epoch'::timestamptz), requested_at, COALESCE(last_error,'')
		FROM retention_purge_requests
		WHERE (state='queued' AND available_at <= $1)
		   OR (state='leased' AND lease_expires_at <= $1)
		ORDER BY available_at, id FOR UPDATE SKIP LOCKED LIMIT 1`, now.UTC()).Scan(
		&request.ID, &request.OrganizationID, &request.ResourceType, &request.ResourceID,
		&request.PolicyUpdatedAt, &keys, &request.State, &request.Attempt, &request.AvailableAt,
		&request.LeasedBy, &request.LeaseExpiresAt, &request.RequestedAt, &request.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RetentionPurgeRequest{}, ErrNoRetentionPurge
	}
	if err != nil {
		return domain.RetentionPurgeRequest{}, err
	}
	if err := json.Unmarshal(keys, &request.ObjectKeys); err != nil {
		return domain.RetentionPurgeRequest{}, err
	}

	var currentPolicy time.Time
	if err := tx.QueryRow(ctx, `SELECT updated_at FROM retention_policies WHERE organization_id=$1`, request.OrganizationID).Scan(&currentPolicy); err != nil {
		return domain.RetentionPurgeRequest{}, err
	}
	eligible, held, err := retentionStillEligible(ctx, tx, request, now)
	if err != nil {
		return domain.RetentionPurgeRequest{}, err
	}
	if !currentPolicy.Equal(request.PolicyUpdatedAt) || !eligible {
		return request, finishRetentionWithoutPurge(ctx, tx, request, workerID, "cancelled", "retention policy or eligibility changed", now)
	}
	if held {
		return request, finishRetentionWithoutPurge(ctx, tx, request, workerID, "blocked", "active legal hold", now)
	}
	request.State = "leased"
	request.Attempt++
	request.LeasedBy = workerID
	request.LeaseExpiresAt = now.UTC().Add(leaseDuration)
	if _, err := tx.Exec(ctx, `
		UPDATE retention_purge_requests SET state='leased', attempt=$1, leased_by=$2,
		lease_expires_at=$3, last_error=NULL, updated_at=$4 WHERE id=$5`, request.Attempt,
		workerID, request.LeaseExpiresAt, now.UTC(), request.ID); err != nil {
		return domain.RetentionPurgeRequest{}, err
	}
	return request, tx.Commit(ctx)
}

func retentionStillEligible(ctx context.Context, tx pgx.Tx, request domain.RetentionPurgeRequest, now time.Time) (bool, bool, error) {
	var eligible, held bool
	switch request.ResourceType {
	case "session_raw":
		err := tx.QueryRow(ctx, `
			SELECT EXISTS(
			  SELECT 1 FROM sessions s
			  JOIN assignments a ON a.id=s.assignment_id JOIN tasks t ON t.id=a.task_id
			  JOIN task_templates tt ON tt.id=t.template_id JOIN projects p ON p.id=tt.project_id
			  JOIN retention_policies rp ON rp.organization_id=p.organization_id
			  WHERE s.id=$1 AND p.organization_id=$2 AND s.state IN ('ACCEPTED','RELEASED','FAILED','REJECTED','REWORK_REQUIRED','CANCELLED')
			    AND s.updated_at <= $3::timestamptz - (rp.raw_days * interval '1 day')
			    AND EXISTS(SELECT 1 FROM artifacts ar WHERE ar.session_id=s.id AND ar.purged_at IS NULL)
			), EXISTS(SELECT 1 FROM legal_holds WHERE session_id=$1 AND released_at IS NULL)`, request.ResourceID, request.OrganizationID, now.UTC()).Scan(&eligible, &held)
		return eligible, held, err
	case "session_derived":
		err := tx.QueryRow(ctx, `
			SELECT EXISTS(
			  SELECT 1 FROM sessions s
			  JOIN assignments a ON a.id=s.assignment_id JOIN tasks t ON t.id=a.task_id
			  JOIN task_templates tt ON tt.id=t.template_id JOIN projects p ON p.id=tt.project_id
			  JOIN retention_policies rp ON rp.organization_id=p.organization_id
			  JOIN validation_results vr ON vr.session_id=s.id AND vr.valid AND vr.purged_at IS NULL
			  WHERE s.id=$1 AND p.organization_id=$2 AND s.state IN ('RELEASED','FAILED','REJECTED','REWORK_REQUIRED','CANCELLED')
			    AND vr.created_at <= $3::timestamptz - (rp.derived_days * interval '1 day')
			), EXISTS(SELECT 1 FROM legal_holds WHERE session_id=$1 AND released_at IS NULL)`, request.ResourceID, request.OrganizationID, now.UTC()).Scan(&eligible, &held)
		return eligible, held, err
	case "release":
		err := tx.QueryRow(ctx, `
			SELECT EXISTS(
			  SELECT 1 FROM dataset_releases r JOIN projects p ON p.id=r.project_id
			  JOIN retention_policies rp ON rp.organization_id=p.organization_id
			  WHERE r.id=$1 AND p.organization_id=$2 AND r.purged_at IS NULL
			    AND r.created_at <= $3::timestamptz - (rp.release_days * interval '1 day')
			), EXISTS(
			  SELECT 1 FROM release_sessions rs JOIN legal_holds h ON h.session_id=rs.session_id
			  WHERE rs.release_id=$1 AND h.released_at IS NULL
			)`, request.ResourceID, request.OrganizationID, now.UTC()).Scan(&eligible, &held)
		return eligible, held, err
	default:
		return false, false, errors.New("unknown retention resource type")
	}
}

func finishRetentionWithoutPurge(ctx context.Context, tx pgx.Tx, request domain.RetentionPurgeRequest, actor, state, reason string, now time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE retention_purge_requests SET state=$1, leased_by=NULL,
		lease_expires_at=NULL, last_error=$2, updated_at=$3 WHERE id=$4`, state, reason, now.UTC(), request.ID); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: actor, Action: "retention.purge_" + state, Resource: "retention-purge/" + request.ID,
		Timestamp: now.UTC(), RequestID: request.ID,
		Metadata: map[string]string{"resource_type": request.ResourceType, "resource_id": request.ResourceID, "reason": reason},
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (store *Store) CompleteRetentionPurge(ctx context.Context, requestID, workerID string, now time.Time) error {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var resourceType, resourceID, leasedBy, state string
	if err := tx.QueryRow(ctx, `SELECT resource_type, resource_id, COALESCE(leased_by,''), state
		FROM retention_purge_requests WHERE id=$1 FOR UPDATE`, requestID).Scan(&resourceType, &resourceID, &leasedBy, &state); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if state != "leased" || leasedBy != workerID {
		return ErrRetentionLease
	}
	var held bool
	switch resourceType {
	case "session_raw", "session_derived":
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM legal_holds WHERE session_id=$1 AND released_at IS NULL)`, resourceID).Scan(&held); err != nil {
			return err
		}
	case "release":
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM release_sessions rs JOIN legal_holds h ON h.session_id=rs.session_id WHERE rs.release_id=$1 AND h.released_at IS NULL)`, resourceID).Scan(&held); err != nil {
			return err
		}
	}
	if held {
		return ErrLegalHoldActive
	}
	switch resourceType {
	case "session_raw":
		_, err = tx.Exec(ctx, `UPDATE artifacts SET purged_at=$1 WHERE session_id=$2 AND purged_at IS NULL`, now.UTC(), resourceID)
	case "session_derived":
		if _, err = tx.Exec(ctx, `UPDATE validation_results SET purged_at=$1 WHERE session_id=$2 AND purged_at IS NULL`, now.UTC(), resourceID); err == nil {
			_, err = tx.Exec(ctx, `UPDATE redaction_jobs SET purged_at=$1 WHERE session_id=$2 AND state='completed' AND purged_at IS NULL`, now.UTC(), resourceID)
		}
	case "release":
		_, err = tx.Exec(ctx, `UPDATE dataset_releases SET purged_at=$1 WHERE id=$2 AND purged_at IS NULL`, now.UTC(), resourceID)
	default:
		err = errors.New("unknown retention resource type")
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE retention_purge_requests SET state='completed', completed_at=$1,
		leased_by=NULL, lease_expires_at=NULL, last_error=NULL, updated_at=$1 WHERE id=$2`, now.UTC(), requestID); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: workerID, Action: "retention.purge_completed", Resource: "retention-purge/" + requestID,
		Timestamp: now.UTC(), RequestID: requestID,
		Metadata: map[string]string{"resource_type": resourceType, "resource_id": resourceID},
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (store *Store) RequeueRetentionPurge(ctx context.Context, requestID, workerID, message string, maxAttempts int, now time.Time) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var attempt int
	var leasedBy, state, resourceType, resourceID string
	if err := tx.QueryRow(ctx, `SELECT attempt, COALESCE(leased_by,''), state, resource_type, resource_id
		FROM retention_purge_requests WHERE id=$1 FOR UPDATE`, requestID).Scan(&attempt, &leasedBy, &state, &resourceType, &resourceID); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if state != "leased" || leasedBy != workerID {
		return ErrRetentionLease
	}
	nextState := "queued"
	availableAt := now.UTC().Add(time.Duration(attempt*attempt) * time.Second)
	if attempt >= maxAttempts {
		nextState = "dead_letter"
		availableAt = now.UTC()
	}
	if _, err := tx.Exec(ctx, `UPDATE retention_purge_requests SET state=$1, available_at=$2,
		leased_by=NULL, lease_expires_at=NULL, last_error=$3, updated_at=$4 WHERE id=$5`, nextState,
		availableAt, message, now.UTC(), requestID); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: workerID, Action: "retention.purge_" + nextState, Resource: "retention-purge/" + requestID,
		Timestamp: now.UTC(), RequestID: requestID,
		Metadata: map[string]string{"resource_type": resourceType, "resource_id": resourceID, "attempt": fmt.Sprint(attempt), "error": message},
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (store *Store) GetRetentionPurgeOrganization(ctx context.Context, requestID string) (string, error) {
	var organizationID string
	err := store.pool.QueryRow(ctx, `SELECT organization_id FROM retention_purge_requests WHERE id=$1`, requestID).Scan(&organizationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return organizationID, err
}

func (store *Store) RetryDeadLetterRetentionPurge(ctx context.Context, requestID, actor, auditRequestID string, now time.Time) (domain.RetentionPurgeRequest, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return domain.RetentionPurgeRequest{}, err
	}
	defer tx.Rollback(ctx)
	var request domain.RetentionPurgeRequest
	var keys []byte
	err = tx.QueryRow(ctx, `
		SELECT id, organization_id, resource_type, resource_id, policy_updated_at, object_keys,
		       state, attempt, available_at, requested_at, COALESCE(last_error,'')
		FROM retention_purge_requests WHERE id=$1 FOR UPDATE`, requestID).Scan(
		&request.ID, &request.OrganizationID, &request.ResourceType, &request.ResourceID,
		&request.PolicyUpdatedAt, &keys, &request.State, &request.Attempt,
		&request.AvailableAt, &request.RequestedAt, &request.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RetentionPurgeRequest{}, ErrNotFound
	}
	if err != nil {
		return domain.RetentionPurgeRequest{}, err
	}
	if request.State != "dead_letter" {
		return domain.RetentionPurgeRequest{}, ErrRetentionNotDead
	}
	if err := json.Unmarshal(keys, &request.ObjectKeys); err != nil {
		return domain.RetentionPurgeRequest{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE retention_purge_requests SET state='queued', attempt=0,
		available_at=$1, leased_by=NULL, lease_expires_at=NULL, last_error=NULL, updated_at=$1
		WHERE id=$2`, now.UTC(), requestID); err != nil {
		return domain.RetentionPurgeRequest{}, err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: actor, Action: "retention.purge_manual_retry", Resource: "retention-purge/" + requestID,
		Timestamp: now.UTC(), RequestID: auditRequestID,
		Metadata: map[string]string{"resource_type": request.ResourceType, "resource_id": request.ResourceID, "previous_attempts": fmt.Sprint(request.Attempt)},
	}); err != nil {
		return domain.RetentionPurgeRequest{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.RetentionPurgeRequest{}, err
	}
	request.State = "queued"
	request.Attempt = 0
	request.AvailableAt = now.UTC()
	request.LastError = ""
	return request, nil
}

func (store *Store) ListRetentionPurgeRequests(ctx context.Context, organizationIDs []string) ([]domain.RetentionPurgeRequest, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT id, organization_id, resource_type, resource_id, policy_updated_at, object_keys,
		       state, attempt, available_at, COALESCE(leased_by,''),
		       COALESCE(lease_expires_at,'epoch'::timestamptz), requested_at,
		       COALESCE(completed_at,'epoch'::timestamptz), COALESCE(last_error,'')
		FROM retention_purge_requests
		WHERE $1::text[] IS NULL OR organization_id=ANY($1)
		ORDER BY requested_at DESC, id`, organizationIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	requests := make([]domain.RetentionPurgeRequest, 0)
	for rows.Next() {
		var request domain.RetentionPurgeRequest
		var keys []byte
		if err := rows.Scan(&request.ID, &request.OrganizationID, &request.ResourceType,
			&request.ResourceID, &request.PolicyUpdatedAt, &keys, &request.State,
			&request.Attempt, &request.AvailableAt, &request.LeasedBy,
			&request.LeaseExpiresAt, &request.RequestedAt, &request.CompletedAt,
			&request.LastError); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(keys, &request.ObjectKeys); err != nil {
			return nil, err
		}
		requests = append(requests, request)
	}
	return requests, rows.Err()
}
