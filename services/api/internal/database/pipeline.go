package database

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"trajectory.local/api/internal/domain"
)

var (
	ErrArtifactConflict = errors.New("artifact metadata conflicts with an existing raw artifact")
	ErrArtifactsMissing = errors.New("submission requires trajectory, video, and event artifacts")
	ErrNoJobAvailable   = errors.New("no processing job is available")
	ErrJobLease         = errors.New("processing job lease is not owned by this worker")
	ErrJobNotDeadLetter = errors.New("processing job is not in the dead-letter queue")
)

func validateArtifact(artifact domain.Artifact) error {
	prefix := "raw/sessions/" + artifact.SessionID + "/"
	if artifact.SessionID == "" || !strings.HasPrefix(artifact.Key, prefix) || len(artifact.Key) == len(prefix) {
		return fmt.Errorf("artifact key must be scoped under %s", prefix)
	}
	decoded, err := hex.DecodeString(artifact.SHA256)
	if err != nil || len(decoded) != 32 {
		return errors.New("artifact sha256 must be a 64-character hexadecimal digest")
	}
	if artifact.Size < 1 || strings.TrimSpace(artifact.MediaType) == "" {
		return errors.New("artifact size and media type are required")
	}
	return nil
}

func (store *Store) EnsureArtifactUploadAllowed(ctx context.Context, sessionID string) error {
	var state domain.SessionState
	err := store.pool.QueryRow(ctx, `SELECT state FROM sessions WHERE id=$1`, sessionID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if state != domain.SessionUploading {
		return fmt.Errorf("%w: artifact upload requires UPLOADING, current state is %s", domain.ErrInvalidTransition, state)
	}
	return nil
}

func (store *Store) RegisterArtifact(ctx context.Context, artifact domain.Artifact, actor, requestID string, now time.Time) (domain.Artifact, error) {
	if err := validateArtifact(artifact); err != nil {
		return domain.Artifact{}, err
	}
	identifier, err := newID("artifact")
	if err != nil {
		return domain.Artifact{}, err
	}
	artifact.ID = identifier
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return domain.Artifact{}, err
	}
	defer tx.Rollback(ctx)

	var state domain.SessionState
	if err := tx.QueryRow(ctx, `SELECT state FROM sessions WHERE id=$1 FOR UPDATE`, artifact.SessionID).Scan(&state); errors.Is(err, pgx.ErrNoRows) {
		return domain.Artifact{}, ErrNotFound
	} else if err != nil {
		return domain.Artifact{}, err
	}
	if state != domain.SessionUploading {
		return domain.Artifact{}, fmt.Errorf("%w: artifact registration requires UPLOADING, current state is %s", domain.ErrInvalidTransition, state)
	}

	commandTag, err := tx.Exec(ctx, `
		INSERT INTO artifacts (id, session_id, logical_key, sha256, size_bytes, media_type, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (logical_key) DO NOTHING`,
		artifact.ID, artifact.SessionID, artifact.Key, artifact.SHA256, artifact.Size, artifact.MediaType, now.UTC())
	if err != nil {
		return domain.Artifact{}, err
	}
	if commandTag.RowsAffected() == 0 {
		var existing domain.Artifact
		if err := tx.QueryRow(ctx, `SELECT id, session_id, logical_key, sha256, size_bytes, media_type FROM artifacts WHERE logical_key=$1`, artifact.Key).
			Scan(&existing.ID, &existing.SessionID, &existing.Key, &existing.SHA256, &existing.Size, &existing.MediaType); err != nil {
			return domain.Artifact{}, err
		}
		if existing.SessionID != artifact.SessionID || existing.SHA256 != artifact.SHA256 || existing.Size != artifact.Size || existing.MediaType != artifact.MediaType {
			return domain.Artifact{}, ErrArtifactConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.Artifact{}, err
		}
		return existing, nil
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: actor, Action: "artifact.registered", Resource: "artifact/" + artifact.ID,
		Timestamp: now.UTC(), RequestID: requestID,
		Metadata: map[string]string{"session_id": artifact.SessionID, "logical_key": artifact.Key, "sha256": artifact.SHA256},
	}); err != nil {
		return domain.Artifact{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Artifact{}, err
	}
	return artifact, nil
}

func (store *Store) ListArtifacts(ctx context.Context, sessionID string) ([]domain.Artifact, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT id, session_id, logical_key, sha256, size_bytes, media_type
		FROM artifacts WHERE session_id=$1 ORDER BY logical_key`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	artifacts := make([]domain.Artifact, 0)
	for rows.Next() {
		var artifact domain.Artifact
		if err := rows.Scan(&artifact.ID, &artifact.SessionID, &artifact.Key, &artifact.SHA256, &artifact.Size, &artifact.MediaType); err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, rows.Err()
}

func (store *Store) GetArtifact(ctx context.Context, sessionID, key string) (domain.Artifact, error) {
	var artifact domain.Artifact
	err := store.pool.QueryRow(ctx, `
		SELECT id, session_id, logical_key, sha256, size_bytes, media_type
		FROM artifacts WHERE session_id=$1 AND logical_key=$2`, sessionID, key).
		Scan(&artifact.ID, &artifact.SessionID, &artifact.Key, &artifact.SHA256, &artifact.Size, &artifact.MediaType)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Artifact{}, ErrNotFound
	}
	return artifact, err
}

func (store *Store) GetNormalizedTrajectoryKey(ctx context.Context, sessionID string) (string, error) {
	var key string
	err := store.pool.QueryRow(ctx, `
		SELECT normalized_key FROM validation_results
		WHERE session_id=$1 AND valid ORDER BY created_at DESC, id DESC LIMIT 1`, sessionID).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return key, err
}

func (store *Store) SubmitSession(ctx context.Context, sessionID, actor, requestID string, now time.Time) (domain.Session, domain.ProcessingJob, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return domain.Session{}, domain.ProcessingJob{}, err
	}
	defer tx.Rollback(ctx)
	var session domain.Session
	if err := tx.QueryRow(ctx, `SELECT id, assignment_id, state, updated_at FROM sessions WHERE id=$1 FOR UPDATE`, sessionID).
		Scan(&session.ID, &session.AssignmentID, &session.State, &session.UpdatedAt); errors.Is(err, pgx.ErrNoRows) {
		return domain.Session{}, domain.ProcessingJob{}, ErrNotFound
	} else if err != nil {
		return domain.Session{}, domain.ProcessingJob{}, err
	}
	if session.State != domain.SessionUploading {
		return domain.Session{}, domain.ProcessingJob{}, fmt.Errorf("%w: submission requires UPLOADING, current state is %s", domain.ErrInvalidTransition, session.State)
	}
	var hasTrajectory, hasVideo, hasEvents bool
	if err := tx.QueryRow(ctx, `
		SELECT
			COALESCE(bool_or(logical_key=$2), false),
			COALESCE(bool_or(media_type='video/mp4'), false),
			COALESCE(bool_or(media_type='application/x-ndjson'), false)
		FROM artifacts WHERE session_id=$1`, sessionID, "raw/sessions/"+sessionID+"/trajectory.json").
		Scan(&hasTrajectory, &hasVideo, &hasEvents); err != nil {
		return domain.Session{}, domain.ProcessingJob{}, err
	}
	if !hasTrajectory || !hasVideo || !hasEvents {
		return domain.Session{}, domain.ProcessingJob{}, ErrArtifactsMissing
	}
	if err := session.Transition(domain.TransitionCommand{To: domain.SessionSubmitted, Actor: actor, RequestID: requestID, Timestamp: now}); err != nil {
		return domain.Session{}, domain.ProcessingJob{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET state=$1, updated_at=$2 WHERE id=$3`, session.State, session.UpdatedAt, session.ID); err != nil {
		return domain.Session{}, domain.ProcessingJob{}, err
	}
	jobID, err := newID("job")
	if err != nil {
		return domain.Session{}, domain.ProcessingJob{}, err
	}
	job := domain.ProcessingJob{ID: jobID, SessionID: session.ID, JobType: "validate_trajectory", State: "queued", AvailableAt: now.UTC()}
	if _, err := tx.Exec(ctx, `
		INSERT INTO processing_jobs (id, session_id, job_type, state, available_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$5)`, job.ID, job.SessionID, job.JobType, job.State, job.AvailableAt); err != nil {
		return domain.Session{}, domain.ProcessingJob{}, err
	}
	if err := insertAudit(ctx, tx, session.AuditEvents[len(session.AuditEvents)-1]); err != nil {
		return domain.Session{}, domain.ProcessingJob{}, err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: actor, Action: "processing_job.queued", Resource: "processing-job/" + job.ID,
		Timestamp: now.UTC(), RequestID: requestID, Metadata: map[string]string{"session_id": session.ID, "job_type": job.JobType},
	}); err != nil {
		return domain.Session{}, domain.ProcessingJob{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Session{}, domain.ProcessingJob{}, err
	}
	return session, job, nil
}

func (store *Store) ClaimValidationJob(ctx context.Context, workerID string, leaseDuration time.Duration, now time.Time) (domain.ProcessingJob, error) {
	if strings.TrimSpace(workerID) == "" || leaseDuration <= 0 {
		return domain.ProcessingJob{}, errors.New("worker ID and positive lease duration are required")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.ProcessingJob{}, err
	}
	defer tx.Rollback(ctx)
	var job domain.ProcessingJob
	err = tx.QueryRow(ctx, `
		SELECT id, session_id, job_type, state, attempt, available_at
		FROM processing_jobs
		WHERE job_type='validate_trajectory'
		  AND ((state='queued' AND available_at <= $1) OR (state='leased' AND lease_expires_at <= $1))
		ORDER BY available_at, id
		FOR UPDATE SKIP LOCKED
		LIMIT 1`, now.UTC()).Scan(&job.ID, &job.SessionID, &job.JobType, &job.State, &job.Attempt, &job.AvailableAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ProcessingJob{}, ErrNoJobAvailable
	}
	if err != nil {
		return domain.ProcessingJob{}, err
	}
	job.State = "leased"
	job.Attempt++
	job.LeasedBy = workerID
	job.LeaseExpiresAt = now.UTC().Add(leaseDuration)
	if _, err := tx.Exec(ctx, `
		UPDATE processing_jobs SET state='leased', attempt=$1, leased_by=$2, lease_expires_at=$3,
		started_at=COALESCE(started_at,$4), updated_at=$4, error=NULL WHERE id=$5`,
		job.Attempt, workerID, job.LeaseExpiresAt, now.UTC(), job.ID); err != nil {
		return domain.ProcessingJob{}, err
	}
	var session domain.Session
	if err := tx.QueryRow(ctx, `SELECT id, assignment_id, state, updated_at FROM sessions WHERE id=$1 FOR UPDATE`, job.SessionID).
		Scan(&session.ID, &session.AssignmentID, &session.State, &session.UpdatedAt); err != nil {
		return domain.ProcessingJob{}, err
	}
	if session.State == domain.SessionSubmitted {
		if err := session.Transition(domain.TransitionCommand{To: domain.SessionProcessing, Actor: workerID, RequestID: job.ID, Timestamp: now}); err != nil {
			return domain.ProcessingJob{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE sessions SET state=$1, updated_at=$2 WHERE id=$3`, session.State, session.UpdatedAt, session.ID); err != nil {
			return domain.ProcessingJob{}, err
		}
		if err := insertAudit(ctx, tx, session.AuditEvents[len(session.AuditEvents)-1]); err != nil {
			return domain.ProcessingJob{}, err
		}
	} else if session.State != domain.SessionProcessing {
		return domain.ProcessingJob{}, fmt.Errorf("%w: cannot lease validation for session in %s", domain.ErrInvalidTransition, session.State)
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: workerID, Action: "processing_job.leased", Resource: "processing-job/" + job.ID,
		Timestamp: now.UTC(), RequestID: job.ID, Metadata: map[string]string{"attempt": fmt.Sprint(job.Attempt)},
	}); err != nil {
		return domain.ProcessingJob{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ProcessingJob{}, err
	}
	return job, nil
}

func (store *Store) CompleteValidationJob(ctx context.Context, jobID, workerID string, result domain.ValidationResult, now time.Time) (domain.Session, error) {
	if result.SchemaVersion == "" || result.ValidatorVersion == "" || (result.Valid && result.NormalizedKey == "") {
		return domain.Session{}, errors.New("validation result is incomplete")
	}
	resultID, err := newID("validation")
	if err != nil {
		return domain.Session{}, err
	}
	result.ID = resultID
	result.JobID = jobID
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.Session{}, err
	}
	defer tx.Rollback(ctx)
	var sessionID, state, leasedBy string
	if err := tx.QueryRow(ctx, `SELECT session_id, state, COALESCE(leased_by,'') FROM processing_jobs WHERE id=$1 FOR UPDATE`, jobID).
		Scan(&sessionID, &state, &leasedBy); errors.Is(err, pgx.ErrNoRows) {
		return domain.Session{}, ErrNotFound
	} else if err != nil {
		return domain.Session{}, err
	}
	if state != "leased" || leasedBy != workerID {
		return domain.Session{}, ErrJobLease
	}
	result.SessionID = sessionID
	var session domain.Session
	if err := tx.QueryRow(ctx, `SELECT id, assignment_id, state, updated_at FROM sessions WHERE id=$1 FOR UPDATE`, sessionID).
		Scan(&session.ID, &session.AssignmentID, &session.State, &session.UpdatedAt); err != nil {
		return domain.Session{}, err
	}
	target := domain.SessionReadyForReview
	if !result.Valid {
		target = domain.SessionFailed
	}
	if err := session.Transition(domain.TransitionCommand{To: target, Actor: workerID, RequestID: jobID, Timestamp: now}); err != nil {
		return domain.Session{}, err
	}
	errorsJSON, err := json.Marshal(result.Errors)
	if err != nil {
		return domain.Session{}, err
	}
	var normalizedKey any
	if result.Valid {
		normalizedKey = result.NormalizedKey
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO validation_results (id, job_id, session_id, valid, errors, normalized_key, schema_version, validator_version, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, result.ID, jobID, sessionID, result.Valid, string(errorsJSON), normalizedKey, result.SchemaVersion, result.ValidatorVersion, now.UTC()); err != nil {
		return domain.Session{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE processing_jobs SET state='completed', finished_at=$1, updated_at=$1, lease_expires_at=NULL, error=NULL WHERE id=$2`, now.UTC(), jobID); err != nil {
		return domain.Session{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET state=$1, updated_at=$2 WHERE id=$3`, session.State, session.UpdatedAt, session.ID); err != nil {
		return domain.Session{}, err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: workerID, Action: "validation.completed", Resource: "session/" + sessionID,
		Timestamp: now.UTC(), RequestID: jobID, Metadata: map[string]string{"valid": fmt.Sprint(result.Valid), "result_id": result.ID},
	}); err != nil {
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

func (store *Store) RequeueValidationJob(ctx context.Context, jobID, workerID, message string, maxAttempts int, now time.Time) error {
	if maxAttempts < 1 {
		return errors.New("max attempts must be positive")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var sessionID, state, leasedBy string
	var attempt int
	if err := tx.QueryRow(ctx, `SELECT session_id, state, COALESCE(leased_by,''), attempt FROM processing_jobs WHERE id=$1 FOR UPDATE`, jobID).
		Scan(&sessionID, &state, &leasedBy, &attempt); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if state != "leased" || leasedBy != workerID {
		return ErrJobLease
	}
	if attempt < maxAttempts {
		availableAt := now.UTC().Add(time.Duration(attempt*attempt) * time.Second)
		if _, err := tx.Exec(ctx, `
			UPDATE processing_jobs SET state='queued', available_at=$1, leased_by=NULL,
			lease_expires_at=NULL, updated_at=$2, error=$3 WHERE id=$4`, availableAt, now.UTC(), message, jobID); err != nil {
			return err
		}
		if err := insertAudit(ctx, tx, domain.AuditEvent{
			Actor: workerID, Action: "processing_job.requeued", Resource: "processing-job/" + jobID,
			Timestamp: now.UTC(), RequestID: jobID, Metadata: map[string]string{"attempt": fmt.Sprint(attempt), "error": message},
		}); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE processing_jobs SET state='dead_letter', finished_at=$1, dead_lettered_at=$1,
		updated_at=$1, leased_by=NULL, lease_expires_at=NULL, error=$2 WHERE id=$3`, now.UTC(), message, jobID); err != nil {
		return err
	}
	var session domain.Session
	if err := tx.QueryRow(ctx, `SELECT id, assignment_id, state, updated_at FROM sessions WHERE id=$1 FOR UPDATE`, sessionID).
		Scan(&session.ID, &session.AssignmentID, &session.State, &session.UpdatedAt); err != nil {
		return err
	}
	if err := session.Transition(domain.TransitionCommand{To: domain.SessionFailed, Actor: workerID, RequestID: jobID, Timestamp: now}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET state=$1, updated_at=$2 WHERE id=$3`, session.State, session.UpdatedAt, session.ID); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: workerID, Action: "processing_job.dead_lettered", Resource: "processing-job/" + jobID,
		Timestamp: now.UTC(), RequestID: jobID, Metadata: map[string]string{"attempt": fmt.Sprint(attempt), "error": message},
	}); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, session.AuditEvents[len(session.AuditEvents)-1]); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (store *Store) GetProcessingJob(ctx context.Context, jobID string) (domain.ProcessingJob, error) {
	var job domain.ProcessingJob
	err := store.pool.QueryRow(ctx, `
		SELECT id, session_id, job_type, state, attempt, available_at,
		       COALESCE(leased_by,''), COALESCE(lease_expires_at, 'epoch'::timestamptz),
		       COALESCE(error,''), manual_requeues,
		       COALESCE(started_at, 'epoch'::timestamptz),
		       COALESCE(finished_at, 'epoch'::timestamptz),
		       COALESCE(dead_lettered_at, 'epoch'::timestamptz)
		FROM processing_jobs WHERE id=$1`, jobID).
		Scan(&job.ID, &job.SessionID, &job.JobType, &job.State, &job.Attempt,
			&job.AvailableAt, &job.LeasedBy, &job.LeaseExpiresAt, &job.LastError,
			&job.ManualRequeues, &job.LeasedAt, &job.FinishedAt, &job.DeadLetteredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ProcessingJob{}, ErrNotFound
	}
	return job, err
}

func (store *Store) ListDeadLetterJobs(ctx context.Context, organizationIDs []string) ([]domain.ProcessingJob, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT j.id, j.session_id, j.job_type, j.state, j.attempt, j.available_at,
		       COALESCE(j.leased_by,''), COALESCE(j.lease_expires_at, 'epoch'::timestamptz),
		       COALESCE(j.error,''), j.manual_requeues,
		       COALESCE(j.started_at, 'epoch'::timestamptz),
		       COALESCE(j.finished_at, 'epoch'::timestamptz),
		       COALESCE(j.dead_lettered_at, 'epoch'::timestamptz)
		FROM processing_jobs j
		JOIN sessions s ON s.id=j.session_id
		JOIN assignments a ON a.id=s.assignment_id
		JOIN tasks t ON t.id=a.task_id
		JOIN task_templates tt ON tt.id=t.template_id
		JOIN projects p ON p.id=tt.project_id
		WHERE j.state='dead_letter'
		  AND ($1::text[] IS NULL OR p.organization_id=ANY($1))
		ORDER BY j.dead_lettered_at DESC, j.id`, organizationIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := make([]domain.ProcessingJob, 0)
	for rows.Next() {
		var job domain.ProcessingJob
		if err := rows.Scan(&job.ID, &job.SessionID, &job.JobType, &job.State,
			&job.Attempt, &job.AvailableAt, &job.LeasedBy, &job.LeaseExpiresAt,
			&job.LastError, &job.ManualRequeues, &job.LeasedAt, &job.FinishedAt,
			&job.DeadLetteredAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (store *Store) RetryDeadLetterJob(ctx context.Context, jobID, actor, requestID string, now time.Time) (domain.ProcessingJob, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return domain.ProcessingJob{}, err
	}
	defer tx.Rollback(ctx)
	var sessionID, state string
	var attempt int
	if err := tx.QueryRow(ctx, `
		SELECT session_id, state, attempt FROM processing_jobs WHERE id=$1 FOR UPDATE`, jobID).
		Scan(&sessionID, &state, &attempt); errors.Is(err, pgx.ErrNoRows) {
		return domain.ProcessingJob{}, ErrNotFound
	} else if err != nil {
		return domain.ProcessingJob{}, err
	}
	if state != "dead_letter" {
		return domain.ProcessingJob{}, ErrJobNotDeadLetter
	}
	var sessionState domain.SessionState
	if err := tx.QueryRow(ctx, `SELECT state FROM sessions WHERE id=$1 FOR UPDATE`, sessionID).Scan(&sessionState); err != nil {
		return domain.ProcessingJob{}, err
	}
	if sessionState != domain.SessionFailed {
		return domain.ProcessingJob{}, fmt.Errorf("%w: dead-letter retry requires FAILED session, current state is %s", domain.ErrInvalidTransition, sessionState)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE processing_jobs
		SET state='queued', attempt=0, available_at=$1, leased_by=NULL,
		    lease_expires_at=NULL, finished_at=NULL, dead_lettered_at=NULL,
		    error=NULL, manual_requeues=manual_requeues+1, updated_at=$1
		WHERE id=$2`, now.UTC(), jobID); err != nil {
		return domain.ProcessingJob{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET state='PROCESSING', updated_at=$1 WHERE id=$2`, now.UTC(), sessionID); err != nil {
		return domain.ProcessingJob{}, err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: actor, Action: "processing_job.manual_retry", Resource: "processing-job/" + jobID,
		Timestamp: now.UTC(), RequestID: requestID,
		Metadata: map[string]string{"previous_attempts": fmt.Sprint(attempt), "session_id": sessionID},
	}); err != nil {
		return domain.ProcessingJob{}, err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: actor, Action: "session.processing_recovered", Resource: "session/" + sessionID,
		Timestamp: now.UTC(), RequestID: requestID,
		Metadata: map[string]string{"from": string(domain.SessionFailed), "to": string(domain.SessionProcessing), "job_id": jobID},
	}); err != nil {
		return domain.ProcessingJob{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ProcessingJob{}, err
	}
	return store.GetProcessingJob(ctx, jobID)
}
