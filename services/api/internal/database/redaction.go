package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"trajectory.local/api/internal/domain"
)

var redactionSegmentID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

var (
	ErrRedactionEligibility = errors.New("redaction requires an accepted, PII-approved session and a complete decision for every video segment")
	ErrRedactionPlanMissing = errors.New("approved redaction plan is missing")
	ErrRedactionConflict    = errors.New("request ID already exists with different redaction content")
	ErrInvalidRedactionPlan = errors.New("redaction plan is invalid")
	ErrNoRedactionJob       = errors.New("no redaction job is available")
	ErrRedactionJobLease    = errors.New("redaction job lease is not owned by this worker")
	ErrRedactionNotDead     = errors.New("redaction job is not dead-lettered")
)

type redactionDocumentJSON struct {
	SchemaVersion string                 `json:"schema_version"`
	Segments      []redactionSegmentJSON `json:"segments"`
}

type redactionSegmentJSON struct {
	SegmentID string                `json:"segment_id"`
	SourceKey string                `json:"source_key"`
	Decision  string                `json:"decision"`
	Regions   []redactionRegionJSON `json:"regions"`
}

type redactionRegionJSON struct {
	StartNS int64   `json:"start_ns"`
	EndNS   int64   `json:"end_ns"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Width   float64 `json:"width"`
	Height  float64 `json:"height"`
	Kind    string  `json:"kind"`
}

func canonicalRedactionDocument(document domain.RedactionDocument, sessionID string) ([]byte, error) {
	if document.SchemaVersion != "redaction/v1" || len(document.Segments) < 1 || len(document.Segments) > 1000 {
		return nil, errors.New("redaction/v1 and between 1 and 1000 segment decisions are required")
	}
	encoded := redactionDocumentJSON{SchemaVersion: document.SchemaVersion, Segments: make([]redactionSegmentJSON, len(document.Segments))}
	seen := make(map[string]bool, len(document.Segments))
	for index, segment := range document.Segments {
		if len(segment.SegmentID) > 128 || !redactionSegmentID.MatchString(segment.SegmentID) {
			return nil, fmt.Errorf("segment %d has an invalid segment_id", index)
		}
		prefix := "raw/sessions/" + sessionID + "/video/"
		if !strings.HasPrefix(segment.SourceKey, prefix) || seen[segment.SourceKey] {
			return nil, fmt.Errorf("segment %d source_key is not a unique video in this session", index)
		}
		seen[segment.SourceKey] = true
		if segment.Decision != "publish_as_is" && segment.Decision != "redact" {
			return nil, fmt.Errorf("segment %d decision must be publish_as_is or redact", index)
		}
		if (segment.Decision == "publish_as_is" && len(segment.Regions) != 0) || (segment.Decision == "redact" && len(segment.Regions) == 0) {
			return nil, fmt.Errorf("segment %d decision does not match its regions", index)
		}
		if len(segment.Regions) > 100 {
			return nil, fmt.Errorf("segment %d exceeds 100 redaction regions", index)
		}
		regions := make([]redactionRegionJSON, len(segment.Regions))
		for regionIndex, region := range segment.Regions {
			if err := validateRedactionRegion(region); err != nil {
				return nil, fmt.Errorf("segment %d region %d: %w", index, regionIndex, err)
			}
			regions[regionIndex] = redactionRegionJSON{
				StartNS: region.StartNS, EndNS: region.EndNS, X: region.X, Y: region.Y,
				Width: region.Width, Height: region.Height, Kind: region.Kind,
			}
		}
		encoded.Segments[index] = redactionSegmentJSON{
			SegmentID: segment.SegmentID, SourceKey: segment.SourceKey,
			Decision: segment.Decision, Regions: regions,
		}
	}
	sort.Slice(encoded.Segments, func(left, right int) bool {
		return encoded.Segments[left].SourceKey < encoded.Segments[right].SourceKey
	})
	return json.Marshal(encoded)
}

func validateRedactionRegion(region domain.RedactionRegion) error {
	if region.StartNS < 0 || region.EndNS <= region.StartNS {
		return errors.New("time range must be finite, non-negative, and increasing")
	}
	values := []float64{region.X, region.Y, region.Width, region.Height}
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return errors.New("geometry must contain finite normalized numbers")
		}
	}
	if region.X < 0 || region.Y < 0 || region.Width <= 0 || region.Height <= 0 || region.X+region.Width > 1 || region.Y+region.Height > 1 {
		return errors.New("geometry must be a positive rectangle within normalized [0,1] bounds")
	}
	switch region.Kind {
	case "personal_data", "credential", "unrelated_content", "other":
	default:
		return errors.New("kind must be personal_data, credential, unrelated_content, or other")
	}
	return nil
}

func decodeRedactionDocument(raw []byte) (domain.RedactionDocument, error) {
	var encoded redactionDocumentJSON
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&encoded); err != nil {
		return domain.RedactionDocument{}, err
	}
	document := domain.RedactionDocument{SchemaVersion: encoded.SchemaVersion, Segments: make([]domain.RedactionSegment, len(encoded.Segments))}
	for index, segment := range encoded.Segments {
		regions := make([]domain.RedactionRegion, len(segment.Regions))
		for regionIndex, region := range segment.Regions {
			regions[regionIndex] = domain.RedactionRegion{
				StartNS: region.StartNS, EndNS: region.EndNS, X: region.X, Y: region.Y,
				Width: region.Width, Height: region.Height, Kind: region.Kind,
			}
		}
		document.Segments[index] = domain.RedactionSegment{
			SegmentID: segment.SegmentID, SourceKey: segment.SourceKey,
			Decision: segment.Decision, Regions: regions,
		}
	}
	return document, nil
}

func (store *Store) CreateRedactionPlan(ctx context.Context, plan domain.RedactionPlan, actor, requestID string, now time.Time) (domain.RedactionPlan, domain.RedactionJob, error) {
	if plan.SessionID == "" || plan.ReviewerID == "" || plan.ReviewerID != actor || requestID == "" {
		return domain.RedactionPlan{}, domain.RedactionJob{}, errors.New("session, matching reviewer actor, and request ID are required")
	}
	canonical, err := canonicalRedactionDocument(plan.Document, plan.SessionID)
	if err != nil {
		return domain.RedactionPlan{}, domain.RedactionJob{}, fmt.Errorf("%w: %v", ErrInvalidRedactionPlan, err)
	}
	digest := sha256.Sum256(canonical)
	plan.DocumentHash = hex.EncodeToString(digest[:])
	plan.SchemaVersion = "redaction/v1"

	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return domain.RedactionPlan{}, domain.RedactionJob{}, err
	}
	defer tx.Rollback(ctx)

	if existing, job, found, lookupErr := redactionByRequest(ctx, tx, plan.SessionID, requestID); lookupErr != nil {
		return domain.RedactionPlan{}, domain.RedactionJob{}, lookupErr
	} else if found {
		if existing.DocumentHash != plan.DocumentHash || existing.ReviewerID != plan.ReviewerID {
			return domain.RedactionPlan{}, domain.RedactionJob{}, ErrRedactionConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.RedactionPlan{}, domain.RedactionJob{}, err
		}
		return existing, job, nil
	}

	var state domain.SessionState
	var acceptedReview bool
	if err := tx.QueryRow(ctx, `
		SELECT s.state, EXISTS(
			SELECT 1 FROM reviews r WHERE r.session_id=s.id AND r.decision='accepted' AND r.pii_review='passed'
		)
		FROM sessions s WHERE s.id=$1 FOR UPDATE`, plan.SessionID).Scan(&state, &acceptedReview); errors.Is(err, pgx.ErrNoRows) {
		return domain.RedactionPlan{}, domain.RedactionJob{}, ErrNotFound
	} else if err != nil {
		return domain.RedactionPlan{}, domain.RedactionJob{}, err
	}
	if state != domain.SessionAccepted || !acceptedReview {
		return domain.RedactionPlan{}, domain.RedactionJob{}, ErrRedactionEligibility
	}
	rows, err := tx.Query(ctx, `SELECT logical_key FROM artifacts WHERE session_id=$1 AND media_type='video/mp4' AND purged_at IS NULL ORDER BY logical_key`, plan.SessionID)
	if err != nil {
		return domain.RedactionPlan{}, domain.RedactionJob{}, err
	}
	videoKeys := make([]string, 0)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return domain.RedactionPlan{}, domain.RedactionJob{}, err
		}
		videoKeys = append(videoKeys, key)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return domain.RedactionPlan{}, domain.RedactionJob{}, err
	}
	rows.Close()
	var deletionPending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM deletion_requests WHERE session_id=$1 AND state IN ('queued','leased','blocked','dead_letter')
	)`, plan.SessionID).Scan(&deletionPending); err != nil {
		return domain.RedactionPlan{}, domain.RedactionJob{}, err
	}
	if deletionPending {
		return domain.RedactionPlan{}, domain.RedactionJob{}, ErrDeletionPending
	}
	plannedKeys := make([]string, len(plan.Document.Segments))
	for index, segment := range plan.Document.Segments {
		plannedKeys[index] = segment.SourceKey
	}
	sort.Strings(plannedKeys)
	if len(videoKeys) == 0 || len(videoKeys) != len(plannedKeys) {
		return domain.RedactionPlan{}, domain.RedactionJob{}, ErrRedactionEligibility
	}
	for index := range videoKeys {
		if videoKeys[index] != plannedKeys[index] {
			return domain.RedactionPlan{}, domain.RedactionJob{}, ErrRedactionEligibility
		}
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version),0)+1 FROM redaction_plans WHERE session_id=$1`, plan.SessionID).Scan(&plan.Version); err != nil {
		return domain.RedactionPlan{}, domain.RedactionJob{}, err
	}
	plan.ID, err = newID("redaction")
	if err != nil {
		return domain.RedactionPlan{}, domain.RedactionJob{}, err
	}
	jobID, err := newID("job")
	if err != nil {
		return domain.RedactionPlan{}, domain.RedactionJob{}, err
	}
	plan.CreatedAt = now.UTC()
	job := domain.RedactionJob{
		ID: jobID, PlanID: plan.ID, SessionID: plan.SessionID, State: "queued",
		AvailableAt: now.UTC(), CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO redaction_plans (id,session_id,reviewer_id,version,schema_version,document,document_hash,request_id,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		plan.ID, plan.SessionID, plan.ReviewerID, plan.Version, plan.SchemaVersion,
		canonical, plan.DocumentHash, requestID, plan.CreatedAt); err != nil {
		return domain.RedactionPlan{}, domain.RedactionJob{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO redaction_jobs (id,plan_id,session_id,state,available_at,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$5,$5)`, job.ID, job.PlanID, job.SessionID, job.State, job.AvailableAt); err != nil {
		return domain.RedactionPlan{}, domain.RedactionJob{}, err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: actor, Action: "redaction_plan.approved", Resource: "redaction-plan/" + plan.ID,
		Timestamp: now.UTC(), RequestID: requestID,
		Metadata: map[string]string{"session_id": plan.SessionID, "version": fmt.Sprint(plan.Version), "document_hash": plan.DocumentHash},
	}); err != nil {
		return domain.RedactionPlan{}, domain.RedactionJob{}, err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: actor, Action: "redaction_job.queued", Resource: "redaction-job/" + job.ID,
		Timestamp: now.UTC(), RequestID: requestID,
		Metadata: map[string]string{"session_id": plan.SessionID, "plan_id": plan.ID},
	}); err != nil {
		return domain.RedactionPlan{}, domain.RedactionJob{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.RedactionPlan{}, domain.RedactionJob{}, err
	}
	return plan, job, nil
}

func redactionByRequest(ctx context.Context, tx pgx.Tx, sessionID, requestID string) (domain.RedactionPlan, domain.RedactionJob, bool, error) {
	var plan domain.RedactionPlan
	var job domain.RedactionJob
	var raw []byte
	err := tx.QueryRow(ctx, `
		SELECT p.id,p.session_id,p.reviewer_id,p.version,p.schema_version,p.document,p.document_hash,p.created_at,
		       j.id,j.plan_id,j.session_id,j.state,j.attempt,j.manual_requeues,j.available_at,
		       COALESCE(j.leased_by,''),COALESCE(j.lease_expires_at,'epoch'::timestamptz),
		       COALESCE(j.output_manifest_key,''),COALESCE(j.error,''),j.created_at,j.updated_at,
		       COALESCE(j.finished_at,'epoch'::timestamptz),COALESCE(j.dead_lettered_at,'epoch'::timestamptz)
		FROM redaction_plans p JOIN redaction_jobs j ON j.plan_id=p.id
		WHERE p.session_id=$1 AND p.request_id=$2`, sessionID, requestID).
		Scan(&plan.ID, &plan.SessionID, &plan.ReviewerID, &plan.Version, &plan.SchemaVersion, &raw, &plan.DocumentHash, &plan.CreatedAt,
			&job.ID, &job.PlanID, &job.SessionID, &job.State, &job.Attempt, &job.ManualRequeues, &job.AvailableAt,
			&job.LeasedBy, &job.LeaseExpiresAt, &job.OutputManifestKey, &job.LastError, &job.CreatedAt, &job.UpdatedAt,
			&job.FinishedAt, &job.DeadLetteredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RedactionPlan{}, domain.RedactionJob{}, false, nil
	}
	if err != nil {
		return domain.RedactionPlan{}, domain.RedactionJob{}, false, err
	}
	plan.Document, err = decodeRedactionDocument(raw)
	return plan, job, true, err
}

func (store *Store) ListRedactionPlans(ctx context.Context, sessionID string) ([]domain.RedactionPlan, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT id,session_id,reviewer_id,version,schema_version,document,document_hash,created_at
		FROM redaction_plans WHERE session_id=$1 ORDER BY version DESC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	plans := make([]domain.RedactionPlan, 0)
	for rows.Next() {
		var plan domain.RedactionPlan
		var raw []byte
		if err := rows.Scan(&plan.ID, &plan.SessionID, &plan.ReviewerID, &plan.Version, &plan.SchemaVersion, &raw, &plan.DocumentHash, &plan.CreatedAt); err != nil {
			return nil, err
		}
		plan.Document, err = decodeRedactionDocument(raw)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, rows.Err()
}

func (store *Store) GetRedactionPlan(ctx context.Context, planID string) (domain.RedactionPlan, error) {
	var plan domain.RedactionPlan
	var raw []byte
	err := store.pool.QueryRow(ctx, `
		SELECT id,session_id,reviewer_id,version,schema_version,document,document_hash,created_at
		FROM redaction_plans WHERE id=$1`, planID).
		Scan(&plan.ID, &plan.SessionID, &plan.ReviewerID, &plan.Version, &plan.SchemaVersion, &raw, &plan.DocumentHash, &plan.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RedactionPlan{}, ErrNotFound
	}
	if err != nil {
		return domain.RedactionPlan{}, err
	}
	plan.Document, err = decodeRedactionDocument(raw)
	return plan, err
}

func (store *Store) ClaimRedactionJob(ctx context.Context, workerID string, leaseDuration time.Duration, now time.Time) (domain.RedactionJob, error) {
	if strings.TrimSpace(workerID) == "" || leaseDuration <= 0 {
		return domain.RedactionJob{}, errors.New("worker ID and positive lease duration are required")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.RedactionJob{}, err
	}
	defer tx.Rollback(ctx)
	var job domain.RedactionJob
	err = tx.QueryRow(ctx, `
		SELECT id,plan_id,session_id,state,attempt,manual_requeues,available_at,created_at,updated_at
		FROM redaction_jobs
		WHERE (state='queued' AND available_at <= $1)
		   OR (state='leased' AND lease_expires_at <= $1)
		ORDER BY available_at,id FOR UPDATE SKIP LOCKED LIMIT 1`, now.UTC()).
		Scan(&job.ID, &job.PlanID, &job.SessionID, &job.State, &job.Attempt, &job.ManualRequeues,
			&job.AvailableAt, &job.CreatedAt, &job.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RedactionJob{}, ErrNoRedactionJob
	}
	if err != nil {
		return domain.RedactionJob{}, err
	}
	var state domain.SessionState
	if err := tx.QueryRow(ctx, `SELECT state FROM sessions WHERE id=$1 FOR UPDATE`, job.SessionID).Scan(&state); err != nil {
		return domain.RedactionJob{}, err
	}
	if state != domain.SessionAccepted {
		return domain.RedactionJob{}, fmt.Errorf("%w: redaction job requires ACCEPTED, current state is %s", domain.ErrInvalidTransition, state)
	}
	var deletionPending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM deletion_requests WHERE session_id=$1 AND state IN ('queued','leased','blocked','dead_letter')
	)`, job.SessionID).Scan(&deletionPending); err != nil {
		return domain.RedactionJob{}, err
	}
	if deletionPending {
		return domain.RedactionJob{}, ErrDeletionPending
	}
	job.State = "leased"
	job.Attempt++
	job.LeasedBy = workerID
	job.LeaseExpiresAt = now.UTC().Add(leaseDuration)
	job.UpdatedAt = now.UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE redaction_jobs SET state='leased',attempt=$1,leased_by=$2,lease_expires_at=$3,
		updated_at=$4,error=NULL WHERE id=$5`, job.Attempt, workerID, job.LeaseExpiresAt, job.UpdatedAt, job.ID); err != nil {
		return domain.RedactionJob{}, err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: workerID, Action: "redaction_job.leased", Resource: "redaction-job/" + job.ID,
		Timestamp: now.UTC(), RequestID: job.ID,
		Metadata: map[string]string{"session_id": job.SessionID, "plan_id": job.PlanID, "attempt": fmt.Sprint(job.Attempt)},
	}); err != nil {
		return domain.RedactionJob{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.RedactionJob{}, err
	}
	return job, nil
}

func (store *Store) CompleteRedactionJob(ctx context.Context, jobID, workerID, outputManifestKey string, now time.Time) error {
	if jobID == "" || workerID == "" || outputManifestKey == "" {
		return errors.New("job, worker, and output manifest are required")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var sessionID, planID, state, leasedBy string
	if err := tx.QueryRow(ctx, `
		SELECT session_id,plan_id,state,COALESCE(leased_by,'') FROM redaction_jobs WHERE id=$1 FOR UPDATE`, jobID).
		Scan(&sessionID, &planID, &state, &leasedBy); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	expected := "derived/sessions/" + sessionID + "/redactions/" + planID + "/manifest.json"
	if state != "leased" || leasedBy != workerID {
		return ErrRedactionJobLease
	}
	if outputManifestKey != expected {
		return errors.New("redaction output manifest key is outside its immutable plan prefix")
	}
	if _, err := tx.Exec(ctx, `
		UPDATE redaction_jobs SET state='completed',output_manifest_key=$1,finished_at=$2,
		updated_at=$2,leased_by=NULL,lease_expires_at=NULL,error=NULL WHERE id=$3`, outputManifestKey, now.UTC(), jobID); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: workerID, Action: "redaction_job.completed", Resource: "redaction-job/" + jobID,
		Timestamp: now.UTC(), RequestID: jobID,
		Metadata: map[string]string{"session_id": sessionID, "plan_id": planID, "output_manifest_key": outputManifestKey},
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (store *Store) RequeueRedactionJob(ctx context.Context, jobID, workerID, message string, maxAttempts int, now time.Time) error {
	if maxAttempts < 1 {
		return errors.New("max attempts must be positive")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var state, leasedBy string
	var attempt int
	if err := tx.QueryRow(ctx, `SELECT state,COALESCE(leased_by,''),attempt FROM redaction_jobs WHERE id=$1 FOR UPDATE`, jobID).
		Scan(&state, &leasedBy, &attempt); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if state != "leased" || leasedBy != workerID {
		return ErrRedactionJobLease
	}
	action := "redaction_job.requeued"
	if attempt < maxAttempts {
		availableAt := now.UTC().Add(time.Duration(attempt*attempt) * time.Second)
		if _, err := tx.Exec(ctx, `
			UPDATE redaction_jobs SET state='queued',available_at=$1,updated_at=$2,error=$3,
			leased_by=NULL,lease_expires_at=NULL WHERE id=$4`, availableAt, now.UTC(), message, jobID); err != nil {
			return err
		}
	} else {
		action = "redaction_job.dead_lettered"
		if _, err := tx.Exec(ctx, `
			UPDATE redaction_jobs SET state='dead_letter',updated_at=$1,finished_at=$1,dead_lettered_at=$1,
			error=$2,leased_by=NULL,lease_expires_at=NULL WHERE id=$3`, now.UTC(), message, jobID); err != nil {
			return err
		}
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: workerID, Action: action, Resource: "redaction-job/" + jobID,
		Timestamp: now.UTC(), RequestID: jobID,
		Metadata: map[string]string{"attempt": fmt.Sprint(attempt), "error": message},
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (store *Store) LatestCompletedRedaction(ctx context.Context, sessionID string) (domain.RedactionPlan, domain.RedactionJob, error) {
	var plan domain.RedactionPlan
	var job domain.RedactionJob
	var raw []byte
	err := store.pool.QueryRow(ctx, `
		SELECT p.id,p.session_id,p.reviewer_id,p.version,p.schema_version,p.document,p.document_hash,p.created_at,
		       j.id,j.plan_id,j.session_id,j.state,j.attempt,j.manual_requeues,j.available_at,
		       COALESCE(j.output_manifest_key,''),j.created_at,j.updated_at,COALESCE(j.finished_at,'epoch'::timestamptz)
		FROM redaction_plans p JOIN redaction_jobs j ON j.plan_id=p.id
		WHERE p.session_id=$1 AND j.state='completed' AND j.purged_at IS NULL
		ORDER BY p.version DESC LIMIT 1`, sessionID).
		Scan(&plan.ID, &plan.SessionID, &plan.ReviewerID, &plan.Version, &plan.SchemaVersion, &raw, &plan.DocumentHash, &plan.CreatedAt,
			&job.ID, &job.PlanID, &job.SessionID, &job.State, &job.Attempt, &job.ManualRequeues, &job.AvailableAt,
			&job.OutputManifestKey, &job.CreatedAt, &job.UpdatedAt, &job.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RedactionPlan{}, domain.RedactionJob{}, ErrRedactionPlanMissing
	}
	if err != nil {
		return domain.RedactionPlan{}, domain.RedactionJob{}, err
	}
	plan.Document, err = decodeRedactionDocument(raw)
	return plan, job, err
}

func (store *Store) ListDeadLetterRedactionJobs(ctx context.Context, organizationIDs []string) ([]domain.RedactionJob, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT j.id,j.plan_id,j.session_id,j.state,j.attempt,j.manual_requeues,j.available_at,
		       COALESCE(j.leased_by,''),COALESCE(j.lease_expires_at,'epoch'::timestamptz),
		       COALESCE(j.output_manifest_key,''),COALESCE(j.error,''),j.created_at,j.updated_at,
		       COALESCE(j.finished_at,'epoch'::timestamptz),COALESCE(j.dead_lettered_at,'epoch'::timestamptz),
		       COALESCE(j.purged_at,'epoch'::timestamptz)
		FROM redaction_jobs j
		JOIN sessions s ON s.id=j.session_id
		JOIN assignments a ON a.id=s.assignment_id
		JOIN tasks t ON t.id=a.task_id
		JOIN task_templates tt ON tt.id=t.template_id
		JOIN projects p ON p.id=tt.project_id
		WHERE j.state='dead_letter' AND ($1::text[] IS NULL OR p.organization_id=ANY($1))
		ORDER BY j.dead_lettered_at DESC,j.id`, organizationIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := make([]domain.RedactionJob, 0)
	for rows.Next() {
		var job domain.RedactionJob
		if err := rows.Scan(&job.ID, &job.PlanID, &job.SessionID, &job.State, &job.Attempt,
			&job.ManualRequeues, &job.AvailableAt, &job.LeasedBy, &job.LeaseExpiresAt,
			&job.OutputManifestKey, &job.LastError, &job.CreatedAt, &job.UpdatedAt,
			&job.FinishedAt, &job.DeadLetteredAt, &job.PurgedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (store *Store) RetryDeadLetterRedactionJob(ctx context.Context, jobID, actor, requestID string, now time.Time) (domain.RedactionJob, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return domain.RedactionJob{}, err
	}
	defer tx.Rollback(ctx)
	var job domain.RedactionJob
	if err := tx.QueryRow(ctx, `
		SELECT id,plan_id,session_id,state,attempt,manual_requeues FROM redaction_jobs WHERE id=$1 FOR UPDATE`, jobID).
		Scan(&job.ID, &job.PlanID, &job.SessionID, &job.State, &job.Attempt, &job.ManualRequeues); errors.Is(err, pgx.ErrNoRows) {
		return domain.RedactionJob{}, ErrNotFound
	} else if err != nil {
		return domain.RedactionJob{}, err
	}
	if job.State != "dead_letter" {
		return domain.RedactionJob{}, ErrRedactionNotDead
	}
	var sessionState domain.SessionState
	var deletionPending bool
	if err := tx.QueryRow(ctx, `SELECT s.state,EXISTS(
		SELECT 1 FROM deletion_requests d WHERE d.session_id=s.id AND d.state IN ('queued','leased','blocked','dead_letter')
	) FROM sessions s WHERE s.id=$1 FOR UPDATE`, job.SessionID).Scan(&sessionState, &deletionPending); err != nil {
		return domain.RedactionJob{}, err
	}
	if sessionState != domain.SessionAccepted {
		return domain.RedactionJob{}, fmt.Errorf("%w: redaction retry requires ACCEPTED, current state is %s", domain.ErrInvalidTransition, sessionState)
	}
	if deletionPending {
		return domain.RedactionJob{}, ErrDeletionPending
	}
	if _, err := tx.Exec(ctx, `
		UPDATE redaction_jobs SET state='queued',attempt=0,manual_requeues=manual_requeues+1,
		available_at=$1,leased_by=NULL,lease_expires_at=NULL,output_manifest_key=NULL,error=NULL,
		finished_at=NULL,dead_lettered_at=NULL,updated_at=$1 WHERE id=$2`, now.UTC(), job.ID); err != nil {
		return domain.RedactionJob{}, err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: actor, Action: "redaction_job.manual_retry", Resource: "redaction-job/" + job.ID,
		Timestamp: now.UTC(), RequestID: requestID,
		Metadata: map[string]string{"session_id": job.SessionID, "plan_id": job.PlanID, "previous_attempts": fmt.Sprint(job.Attempt)},
	}); err != nil {
		return domain.RedactionJob{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.RedactionJob{}, err
	}
	job.State = "queued"
	job.Attempt = 0
	job.ManualRequeues++
	job.AvailableAt = now.UTC()
	job.UpdatedAt = now.UTC()
	job.DeadLetteredAt = time.Time{}
	job.FinishedAt = time.Time{}
	job.LastError = ""
	return job, nil
}

func (store *Store) GetRedactionJobOrganization(ctx context.Context, jobID string) (string, error) {
	var organizationID string
	err := store.pool.QueryRow(ctx, `
		SELECT p.organization_id FROM redaction_jobs j
		JOIN sessions s ON s.id=j.session_id JOIN assignments a ON a.id=s.assignment_id
		JOIN tasks t ON t.id=a.task_id JOIN task_templates tt ON tt.id=t.template_id
		JOIN projects p ON p.id=tt.project_id WHERE j.id=$1`, jobID).Scan(&organizationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return organizationID, err
}
