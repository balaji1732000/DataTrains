package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"trajectory.local/api/internal/domain"
)

var (
	ErrDeletionNotAllowed  = errors.New("this session cannot be deleted in its current state")
	ErrLegalHoldActive     = errors.New("this session is under an active legal hold")
	ErrLegalHoldExists     = errors.New("this session already has an active legal hold")
	ErrDeletionPending     = errors.New("this session already has an active deletion request")
	ErrNoDeletionRequest   = errors.New("no deletion request is available")
	ErrDeletionLease       = errors.New("deletion request lease is not owned by this worker")
	ErrDeletionNotDead     = errors.New("deletion request is not dead-lettered")
	ErrRedactionInProgress = errors.New("video redaction must finish or reach recovery before session deletion can start")
)

func (store *Store) PlaceLegalHold(ctx context.Context, sessionID, reason, actor, requestID string, now time.Time) (domain.LegalHold, error) {
	reason = strings.TrimSpace(reason)
	if len(reason) < 3 || len(reason) > 2000 {
		return domain.LegalHold{}, errors.New("legal-hold reason must contain 3 to 2000 characters")
	}
	holdID, err := newID("hold")
	if err != nil {
		return domain.LegalHold{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return domain.LegalHold{}, err
	}
	defer tx.Rollback(ctx)
	var organizationID string
	var state domain.SessionState
	if err := tx.QueryRow(ctx, `
		SELECT p.organization_id, s.state
		FROM sessions s
		JOIN assignments a ON a.id=s.assignment_id
		JOIN tasks t ON t.id=a.task_id
		JOIN task_templates tt ON tt.id=t.template_id
		JOIN projects p ON p.id=tt.project_id
		WHERE s.id=$1 FOR UPDATE OF s`, sessionID).Scan(&organizationID, &state); errors.Is(err, pgx.ErrNoRows) {
		return domain.LegalHold{}, ErrNotFound
	} else if err != nil {
		return domain.LegalHold{}, err
	}
	if state == domain.SessionDeleted {
		return domain.LegalHold{}, ErrDeletionNotAllowed
	}
	var deletionLeased bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deletion_requests WHERE session_id=$1 AND state='leased')`, sessionID).Scan(&deletionLeased); err != nil {
		return domain.LegalHold{}, err
	}
	if deletionLeased {
		return domain.LegalHold{}, errors.New("a deletion worker is already erasing this session")
	}
	var retentionLeased bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM retention_purge_requests q
		WHERE q.state='leased' AND (
		  (q.resource_type IN ('session_raw','session_derived') AND q.resource_id=$1)
		  OR (q.resource_type='release' AND EXISTS (
		    SELECT 1 FROM release_sessions rs WHERE rs.release_id=q.resource_id AND rs.session_id=$1
		  ))
		)
	)`, sessionID).Scan(&retentionLeased); err != nil {
		return domain.LegalHold{}, err
	}
	if retentionLeased {
		return domain.LegalHold{}, ErrRetentionPurgeInProgress
	}
	hold := domain.LegalHold{
		ID: holdID, OrganizationID: organizationID, SessionID: sessionID,
		Reason: reason, PlacedBy: actor, PlacedAt: now.UTC(),
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO legal_holds (id, organization_id, session_id, reason, placed_by, placed_at)
		VALUES ($1,$2,$3,$4,$5,$6)`, hold.ID, hold.OrganizationID, hold.SessionID,
		hold.Reason, hold.PlacedBy, hold.PlacedAt); err != nil {
		if constraintViolation(err, "legal_holds_one_active_per_session") {
			return domain.LegalHold{}, ErrLegalHoldExists
		}
		return domain.LegalHold{}, err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: actor, Action: "legal_hold.placed", Resource: "legal-hold/" + hold.ID,
		Timestamp: now.UTC(), RequestID: requestID,
		Metadata: map[string]string{"organization_id": organizationID, "session_id": sessionID, "reason": reason},
	}); err != nil {
		return domain.LegalHold{}, err
	}
	return hold, tx.Commit(ctx)
}

func (store *Store) ReleaseLegalHold(ctx context.Context, sessionID, holdID, actor, requestID string, now time.Time) error {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var organizationID string
	err = tx.QueryRow(ctx, `
		UPDATE legal_holds SET released_by=$1, released_at=$2
		WHERE id=$3 AND session_id=$4 AND released_at IS NULL
		RETURNING organization_id`, actor, now.UTC(), holdID, sessionID).Scan(&organizationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE deletion_requests
		SET state='queued', available_at=$1, last_error=NULL, updated_at=$1
		WHERE session_id=$2 AND state='blocked'`, now.UTC(), sessionID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE retention_purge_requests
		SET state='queued', available_at=$1, last_error=NULL, updated_at=$1
		WHERE state='blocked' AND (
		  (resource_type IN ('session_raw','session_derived') AND resource_id=$2)
		  OR (resource_type='release' AND EXISTS (
		    SELECT 1 FROM release_sessions rs WHERE rs.release_id=resource_id AND rs.session_id=$2
		  ))
		)`, now.UTC(), sessionID); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: actor, Action: "legal_hold.released", Resource: "legal-hold/" + holdID,
		Timestamp: now.UTC(), RequestID: requestID,
		Metadata: map[string]string{"organization_id": organizationID, "session_id": sessionID},
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (store *Store) RequestSessionDeletion(ctx context.Context, sessionID, reason, actor, requestID string, now time.Time) (domain.DeletionRequest, error) {
	reason = strings.TrimSpace(reason)
	if len(reason) < 3 || len(reason) > 2000 {
		return domain.DeletionRequest{}, errors.New("deletion reason must contain 3 to 2000 characters")
	}
	requestIDValue, err := newID("delete")
	if err != nil {
		return domain.DeletionRequest{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return domain.DeletionRequest{}, err
	}
	defer tx.Rollback(ctx)
	var organizationID string
	var state domain.SessionState
	if err := tx.QueryRow(ctx, `
		SELECT p.organization_id, s.state
		FROM sessions s
		JOIN assignments a ON a.id=s.assignment_id
		JOIN tasks t ON t.id=a.task_id
		JOIN task_templates tt ON tt.id=t.template_id
		JOIN projects p ON p.id=tt.project_id
		WHERE s.id=$1 FOR UPDATE OF s`, sessionID).Scan(&organizationID, &state); errors.Is(err, pgx.ErrNoRows) {
		return domain.DeletionRequest{}, ErrNotFound
	} else if err != nil {
		return domain.DeletionRequest{}, err
	}
	if !deletableSessionState(state) {
		return domain.DeletionRequest{}, ErrDeletionNotAllowed
	}
	var held bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM legal_holds WHERE session_id=$1 AND released_at IS NULL)`, sessionID).Scan(&held); err != nil {
		return domain.DeletionRequest{}, err
	}
	if held {
		return domain.DeletionRequest{}, ErrLegalHoldActive
	}
	var redactionInProgress bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM redaction_jobs WHERE session_id=$1 AND state IN ('queued','leased')
	)`, sessionID).Scan(&redactionInProgress); err != nil {
		return domain.DeletionRequest{}, err
	}
	if redactionInProgress {
		return domain.DeletionRequest{}, ErrRedactionInProgress
	}
	keys, err := sessionObjectKeys(ctx, tx, sessionID)
	if err != nil {
		return domain.DeletionRequest{}, err
	}
	encodedKeys, err := json.Marshal(keys)
	if err != nil {
		return domain.DeletionRequest{}, err
	}
	request := domain.DeletionRequest{
		ID: requestIDValue, OrganizationID: organizationID, SessionID: sessionID,
		Reason: reason, ObjectKeys: keys, State: "queued", AvailableAt: now.UTC(),
		RequestedBy: actor, RequestedAt: now.UTC(),
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO deletion_requests
		  (id, organization_id, session_id, reason, object_keys, state, available_at,
		   requested_by, requested_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,'queued',$6,$7,$6,$6)`, request.ID,
		request.OrganizationID, request.SessionID, request.Reason, encodedKeys,
		now.UTC(), actor); err != nil {
		if constraintViolation(err, "deletion_requests_one_active_per_session") {
			return domain.DeletionRequest{}, ErrDeletionPending
		}
		return domain.DeletionRequest{}, err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: actor, Action: "session.deletion_requested", Resource: "deletion-request/" + request.ID,
		Timestamp: now.UTC(), RequestID: requestID,
		Metadata: map[string]string{"organization_id": organizationID, "session_id": sessionID, "reason": reason, "object_count": fmt.Sprint(len(keys))},
	}); err != nil {
		return domain.DeletionRequest{}, err
	}
	return request, tx.Commit(ctx)
}

func deletableSessionState(state domain.SessionState) bool {
	switch state {
	case domain.SessionCreated, domain.SessionAssigned, domain.SessionReady,
		domain.SessionReadyForReview, domain.SessionAccepted, domain.SessionFailed,
		domain.SessionRejected, domain.SessionReworkRequired, domain.SessionCancelled:
		return true
	default:
		return false
	}
}

func sessionObjectKeys(ctx context.Context, tx pgx.Tx, sessionID string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT logical_key FROM artifacts WHERE session_id=$1
		UNION
		SELECT normalized_key FROM validation_results WHERE session_id=$1 AND normalized_key IS NOT NULL
		UNION
		SELECT 'derived/sessions/' || $1 || '/actions.jsonl'
		WHERE EXISTS(SELECT 1 FROM validation_results WHERE session_id=$1 AND valid)
		UNION
		SELECT 'derived/sessions/' || $1 || '/redactions/' || plan.id || '/manifest.json'
		FROM redaction_plans plan
		JOIN redaction_jobs job ON job.plan_id=plan.id AND job.state IN ('completed','dead_letter') AND job.purged_at IS NULL
		WHERE plan.session_id=$1
		UNION
		SELECT 'derived/sessions/' || $1 || '/redactions/' || plan.id || '/' || (segment->>'segment_id') || '.mp4'
		FROM redaction_plans plan
		JOIN redaction_jobs job ON job.plan_id=plan.id AND job.state IN ('completed','dead_letter') AND job.purged_at IS NULL
		CROSS JOIN LATERAL jsonb_array_elements(plan.document->'segments') segment
		WHERE plan.session_id=$1
		ORDER BY 1`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := make([]string, 0)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (store *Store) ClaimDeletionRequest(ctx context.Context, workerID string, leaseDuration time.Duration, now time.Time) (domain.DeletionRequest, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.DeletionRequest{}, err
	}
	defer tx.Rollback(ctx)
	var request domain.DeletionRequest
	var keys []byte
	var sessionState domain.SessionState
	err = tx.QueryRow(ctx, `
		SELECT d.id, d.organization_id, d.session_id, d.reason, d.object_keys, d.state, d.attempt, d.manual_requeues,
		       available_at, COALESCE(leased_by,''),
		       COALESCE(lease_expires_at, 'epoch'::timestamptz), requested_by, requested_at,
		       COALESCE(last_error,''), s.state
		FROM deletion_requests d
		JOIN sessions s ON s.id=d.session_id
		WHERE (d.state='queued' AND d.available_at <= $1)
		   OR (d.state='leased' AND d.lease_expires_at <= $1)
		ORDER BY d.available_at, d.id FOR UPDATE OF d, s SKIP LOCKED LIMIT 1`, now.UTC()).Scan(
		&request.ID, &request.OrganizationID, &request.SessionID, &request.Reason,
		&keys, &request.State, &request.Attempt, &request.ManualRequeues, &request.AvailableAt,
		&request.LeasedBy, &request.LeaseExpiresAt, &request.RequestedBy,
		&request.RequestedAt, &request.LastError, &sessionState)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DeletionRequest{}, ErrNoDeletionRequest
	}
	if err != nil {
		return domain.DeletionRequest{}, err
	}
	if err := json.Unmarshal(keys, &request.ObjectKeys); err != nil {
		return domain.DeletionRequest{}, err
	}
	if !deletableSessionState(sessionState) {
		message := "session state changed to " + string(sessionState)
		if _, err := tx.Exec(ctx, `
			UPDATE deletion_requests SET state='dead_letter', leased_by=NULL,
			lease_expires_at=NULL, last_error=$1, updated_at=$2 WHERE id=$3`, message, now.UTC(), request.ID); err != nil {
			return domain.DeletionRequest{}, err
		}
		if err := insertAudit(ctx, tx, domain.AuditEvent{
			Actor: workerID, Action: "session.deletion_dead_letter", Resource: "deletion-request/" + request.ID,
			Timestamp: now.UTC(), RequestID: request.ID,
			Metadata: map[string]string{"session_id": request.SessionID, "error": message},
		}); err != nil {
			return domain.DeletionRequest{}, err
		}
		request.State = "dead_letter"
		request.LastError = message
		return request, tx.Commit(ctx)
	}
	var held bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM legal_holds WHERE session_id=$1 AND released_at IS NULL)`, request.SessionID).Scan(&held); err != nil {
		return domain.DeletionRequest{}, err
	}
	if held {
		if _, err := tx.Exec(ctx, `
			UPDATE deletion_requests SET state='blocked', leased_by=NULL,
			lease_expires_at=NULL, last_error='active legal hold', updated_at=$1 WHERE id=$2`, now.UTC(), request.ID); err != nil {
			return domain.DeletionRequest{}, err
		}
		if err := insertAudit(ctx, tx, domain.AuditEvent{
			Actor: workerID, Action: "session.deletion_blocked", Resource: "deletion-request/" + request.ID,
			Timestamp: now.UTC(), RequestID: request.ID,
			Metadata: map[string]string{"session_id": request.SessionID, "reason": "active legal hold"},
		}); err != nil {
			return domain.DeletionRequest{}, err
		}
		request.State = "blocked"
		request.LastError = "active legal hold"
		return request, tx.Commit(ctx)
	}
	request.State = "leased"
	request.Attempt++
	request.LeasedBy = workerID
	request.LeaseExpiresAt = now.UTC().Add(leaseDuration)
	if _, err := tx.Exec(ctx, `
		UPDATE deletion_requests SET state='leased', attempt=$1, leased_by=$2,
		lease_expires_at=$3, last_error=NULL, updated_at=$4 WHERE id=$5`,
		request.Attempt, workerID, request.LeaseExpiresAt, now.UTC(), request.ID); err != nil {
		return domain.DeletionRequest{}, err
	}
	return request, tx.Commit(ctx)
}

func (store *Store) RequeueDeletionRequest(ctx context.Context, deletionID, workerID, message string, maxAttempts int, now time.Time) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var attempt int
	var leasedBy, state, sessionID string
	if err := tx.QueryRow(ctx, `SELECT attempt, COALESCE(leased_by,''), state, session_id FROM deletion_requests WHERE id=$1 FOR UPDATE`, deletionID).
		Scan(&attempt, &leasedBy, &state, &sessionID); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if state != "leased" || leasedBy != workerID {
		return ErrDeletionLease
	}
	nextState := "queued"
	availableAt := now.UTC().Add(time.Duration(attempt*attempt) * time.Second)
	if attempt >= maxAttempts {
		nextState = "dead_letter"
		availableAt = now.UTC()
	}
	if _, err := tx.Exec(ctx, `
		UPDATE deletion_requests SET state=$1, available_at=$2, leased_by=NULL,
		lease_expires_at=NULL, last_error=$3, updated_at=$4 WHERE id=$5`, nextState,
		availableAt, message, now.UTC(), deletionID); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: workerID, Action: "session.deletion_" + nextState, Resource: "deletion-request/" + deletionID,
		Timestamp: now.UTC(), RequestID: deletionID,
		Metadata: map[string]string{"session_id": sessionID, "attempt": fmt.Sprint(attempt), "error": message},
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (store *Store) CompleteDeletionRequest(ctx context.Context, deletionID, workerID string, now time.Time) error {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var sessionID, leasedBy, state string
	if err := tx.QueryRow(ctx, `SELECT session_id, COALESCE(leased_by,''), state FROM deletion_requests WHERE id=$1 FOR UPDATE`, deletionID).
		Scan(&sessionID, &leasedBy, &state); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if state != "leased" || leasedBy != workerID {
		return ErrDeletionLease
	}
	var held bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM legal_holds WHERE session_id=$1 AND released_at IS NULL)`, sessionID).Scan(&held); err != nil {
		return err
	}
	if held {
		return ErrLegalHoldActive
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sessions SET state='DELETED', consent_acceptance_id=NULL, updated_at=$1 WHERE id=$2`, now.UTC(), sessionID); err != nil {
		return err
	}
	statements := []string{
		`DELETE FROM review_claims WHERE session_id=$1`,
		`DELETE FROM reviews WHERE session_id=$1`,
		`DELETE FROM validation_results WHERE session_id=$1`,
		`DELETE FROM redaction_jobs WHERE session_id=$1`,
		`DELETE FROM redaction_plans WHERE session_id=$1`,
		`DELETE FROM artifact_multipart_uploads WHERE session_id=$1`,
		`DELETE FROM artifacts WHERE session_id=$1`,
		`DELETE FROM processing_jobs WHERE session_id=$1`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement, sessionID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE deletion_requests SET state='completed', completed_at=$1, updated_at=$1,
		leased_by=NULL, lease_expires_at=NULL, last_error=NULL WHERE id=$2`, now.UTC(), deletionID); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: workerID, Action: "session.deleted", Resource: "session/" + sessionID,
		Timestamp: now.UTC(), RequestID: deletionID,
		Metadata: map[string]string{"deletion_request_id": deletionID},
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (store *Store) GetDeletionRequestOrganization(ctx context.Context, deletionID string) (string, error) {
	var organizationID string
	err := store.pool.QueryRow(ctx, `SELECT organization_id FROM deletion_requests WHERE id=$1`, deletionID).Scan(&organizationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return organizationID, err
}

func (store *Store) RetryDeadLetterDeletionRequest(ctx context.Context, deletionID, actor, requestID string, now time.Time) (domain.DeletionRequest, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return domain.DeletionRequest{}, err
	}
	defer tx.Rollback(ctx)
	var request domain.DeletionRequest
	var keys []byte
	err = tx.QueryRow(ctx, `
		SELECT id, organization_id, session_id, reason, object_keys, state, attempt,
		       manual_requeues, available_at, requested_by, requested_at, COALESCE(last_error,'')
		FROM deletion_requests WHERE id=$1 FOR UPDATE`, deletionID).Scan(
		&request.ID, &request.OrganizationID, &request.SessionID, &request.Reason,
		&keys, &request.State, &request.Attempt, &request.ManualRequeues,
		&request.AvailableAt, &request.RequestedBy, &request.RequestedAt, &request.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DeletionRequest{}, ErrNotFound
	}
	if err != nil {
		return domain.DeletionRequest{}, err
	}
	if request.State != "dead_letter" {
		return domain.DeletionRequest{}, ErrDeletionNotDead
	}
	if err := json.Unmarshal(keys, &request.ObjectKeys); err != nil {
		return domain.DeletionRequest{}, err
	}
	var sessionState domain.SessionState
	if err := tx.QueryRow(ctx, `SELECT state FROM sessions WHERE id=$1 FOR UPDATE`, request.SessionID).Scan(&sessionState); err != nil {
		return domain.DeletionRequest{}, err
	}
	if !deletableSessionState(sessionState) {
		return domain.DeletionRequest{}, ErrDeletionNotAllowed
	}
	var held bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM legal_holds WHERE session_id=$1 AND released_at IS NULL)`, request.SessionID).Scan(&held); err != nil {
		return domain.DeletionRequest{}, err
	}
	if held {
		return domain.DeletionRequest{}, ErrLegalHoldActive
	}
	if _, err := tx.Exec(ctx, `
		UPDATE deletion_requests SET state='queued', attempt=0, manual_requeues=manual_requeues+1,
		available_at=$1, leased_by=NULL, lease_expires_at=NULL, last_error=NULL, updated_at=$1
		WHERE id=$2`, now.UTC(), deletionID); err != nil {
		return domain.DeletionRequest{}, err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: actor, Action: "session.deletion_manual_retry", Resource: "deletion-request/" + deletionID,
		Timestamp: now.UTC(), RequestID: requestID,
		Metadata: map[string]string{"session_id": request.SessionID, "previous_attempts": fmt.Sprint(request.Attempt)},
	}); err != nil {
		return domain.DeletionRequest{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.DeletionRequest{}, err
	}
	request.State = "queued"
	request.Attempt = 0
	request.ManualRequeues++
	request.AvailableAt = now.UTC()
	request.LastError = ""
	return request, nil
}

func (store *Store) ListDeletionRequests(ctx context.Context, organizationIDs []string) ([]domain.DeletionRequest, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT id, organization_id, session_id, reason, object_keys, state, attempt, manual_requeues,
		       available_at, COALESCE(leased_by,''), COALESCE(lease_expires_at, 'epoch'::timestamptz),
		       requested_by, requested_at, COALESCE(last_error,'')
		FROM deletion_requests
		WHERE $1::text[] IS NULL OR organization_id=ANY($1)
		ORDER BY requested_at DESC, id`, organizationIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	requests := make([]domain.DeletionRequest, 0)
	for rows.Next() {
		var request domain.DeletionRequest
		var keys []byte
		if err := rows.Scan(&request.ID, &request.OrganizationID, &request.SessionID,
			&request.Reason, &keys, &request.State, &request.Attempt, &request.ManualRequeues, &request.AvailableAt,
			&request.LeasedBy, &request.LeaseExpiresAt, &request.RequestedBy,
			&request.RequestedAt, &request.LastError); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(keys, &request.ObjectKeys); err != nil {
			return nil, err
		}
		requests = append(requests, request)
	}
	return requests, rows.Err()
}

func constraintViolation(err error, constraint string) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505" && postgresError.ConstraintName == constraint
}

func (store *Store) ListActiveLegalHolds(ctx context.Context, organizationIDs []string) ([]domain.LegalHold, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT id, organization_id, session_id, reason, placed_by, placed_at
		FROM legal_holds
		WHERE released_at IS NULL
		  AND ($1::text[] IS NULL OR organization_id=ANY($1))
		ORDER BY placed_at DESC, id`, organizationIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	holds := make([]domain.LegalHold, 0)
	for rows.Next() {
		var hold domain.LegalHold
		if err := rows.Scan(&hold.ID, &hold.OrganizationID, &hold.SessionID,
			&hold.Reason, &hold.PlacedBy, &hold.PlacedAt); err != nil {
			return nil, err
		}
		holds = append(holds, hold)
	}
	return holds, rows.Err()
}
