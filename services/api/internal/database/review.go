package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"trajectory.local/api/internal/domain"
)

var (
	ErrReviewClaimed = errors.New("session is claimed by another reviewer")
	ErrReviewLease   = errors.New("review claim is missing, expired, or owned by another reviewer")
	ErrPIIGate       = errors.New("accepted reviews require a passed PII review")
)

type ReviewQueueItem struct {
	SessionID     string    `json:"session_id"`
	TaskID        string    `json:"task_id"`
	Goal          string    `json:"goal"`
	Category      string    `json:"category"`
	Difficulty    string    `json:"difficulty"`
	ContributorID string    `json:"contributor_id"`
	NormalizedKey string    `json:"normalized_key"`
	ReadySince    time.Time `json:"ready_since"`
}

type ReviewClaim struct {
	SessionID, ReviewerID string
	ClaimedAt, ExpiresAt  time.Time
}

func (store *Store) ListReviewQueue(ctx context.Context, now time.Time, limit int) ([]ReviewQueueItem, error) {
	return store.listReviewQueue(ctx, now, limit, nil)
}

func (store *Store) ListReviewQueueForOrganizations(ctx context.Context, now time.Time, limit int, organizationIDs []string) ([]ReviewQueueItem, error) {
	if len(organizationIDs) == 0 {
		return []ReviewQueueItem{}, nil
	}
	return store.listReviewQueue(ctx, now, limit, organizationIDs)
}

func (store *Store) listReviewQueue(ctx context.Context, now time.Time, limit int, organizationIDs []string) ([]ReviewQueueItem, error) {
	if limit < 1 || limit > 200 {
		return nil, errors.New("review queue limit must be between 1 and 200")
	}
	rows, err := store.pool.Query(ctx, `
		SELECT s.id, t.id, t.goal, tt.category, tt.difficulty, a.contributor_id,
		       vr.normalized_key, s.updated_at
		FROM sessions s
		JOIN assignments a ON a.id=s.assignment_id
		JOIN tasks t ON t.id=a.task_id
		JOIN task_templates tt ON tt.id=t.template_id
		JOIN projects p ON p.id=tt.project_id
		JOIN validation_results vr ON vr.session_id=s.id AND vr.valid
		LEFT JOIN review_claims rc ON rc.session_id=s.id AND rc.expires_at > $1
		WHERE s.state='READY_FOR_REVIEW' AND rc.session_id IS NULL
		  AND ($3::text[] IS NULL OR p.organization_id=ANY($3))
		ORDER BY s.updated_at, s.id
		LIMIT $2`, now.UTC(), limit, organizationIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ReviewQueueItem, 0)
	for rows.Next() {
		var item ReviewQueueItem
		if err := rows.Scan(&item.SessionID, &item.TaskID, &item.Goal, &item.Category, &item.Difficulty, &item.ContributorID, &item.NormalizedKey, &item.ReadySince); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (store *Store) ClaimReview(ctx context.Context, sessionID, reviewerID, actor, requestID string, lease time.Duration, now time.Time) (ReviewClaim, error) {
	if sessionID == "" || reviewerID == "" || actor != reviewerID || lease < time.Minute || lease > 8*time.Hour {
		return ReviewClaim{}, errors.New("reviewer must claim for themselves with a lease between one minute and eight hours")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return ReviewClaim{}, err
	}
	defer tx.Rollback(ctx)
	var state domain.SessionState
	if err := tx.QueryRow(ctx, `SELECT state FROM sessions WHERE id=$1 FOR UPDATE`, sessionID).Scan(&state); errors.Is(err, pgx.ErrNoRows) {
		return ReviewClaim{}, ErrNotFound
	} else if err != nil {
		return ReviewClaim{}, err
	}
	if state != domain.SessionReadyForReview {
		return ReviewClaim{}, fmt.Errorf("%w: review claiming requires READY_FOR_REVIEW, current state is %s", domain.ErrInvalidTransition, state)
	}
	claim := ReviewClaim{SessionID: sessionID, ReviewerID: reviewerID, ClaimedAt: now.UTC(), ExpiresAt: now.UTC().Add(lease)}
	tag, err := tx.Exec(ctx, `
		INSERT INTO review_claims (session_id, reviewer_id, claimed_at, expires_at)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (session_id) DO UPDATE
		SET reviewer_id=EXCLUDED.reviewer_id, claimed_at=EXCLUDED.claimed_at, expires_at=EXCLUDED.expires_at
		WHERE review_claims.expires_at <= $3 OR review_claims.reviewer_id=$2`,
		claim.SessionID, claim.ReviewerID, claim.ClaimedAt, claim.ExpiresAt)
	if err != nil {
		return ReviewClaim{}, err
	}
	if tag.RowsAffected() == 0 {
		return ReviewClaim{}, ErrReviewClaimed
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: actor, Action: "review.claimed", Resource: "session/" + sessionID,
		Timestamp: now.UTC(), RequestID: requestID, Metadata: map[string]string{"reviewer_id": reviewerID, "expires_at": claim.ExpiresAt.Format(time.RFC3339Nano)},
	}); err != nil {
		return ReviewClaim{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReviewClaim{}, err
	}
	return claim, nil
}

func (store *Store) ReleaseReview(ctx context.Context, sessionID, reviewerID, actor, requestID string, now time.Time) error {
	if sessionID == "" || reviewerID == "" || actor != reviewerID {
		return errors.New("reviewer must release their own claim")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var owner string
	if err := tx.QueryRow(ctx, `SELECT reviewer_id FROM review_claims WHERE session_id=$1 FOR UPDATE`, sessionID).Scan(&owner); errors.Is(err, pgx.ErrNoRows) {
		return ErrReviewLease
	} else if err != nil {
		return err
	}
	if owner != reviewerID {
		return ErrReviewLease
	}
	if _, err := tx.Exec(ctx, `DELETE FROM review_claims WHERE session_id=$1`, sessionID); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: actor, Action: "review.released", Resource: "session/" + sessionID,
		Timestamp: now.UTC(), RequestID: requestID, Metadata: map[string]string{"reviewer_id": reviewerID},
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (store *Store) SubmitReview(ctx context.Context, review domain.Review, actor, requestID string, now time.Time) (domain.Review, domain.Session, error) {
	if review.SessionID == "" || review.ReviewerID == "" || review.ReviewerID != actor || review.RubricVersion == "" {
		return domain.Review{}, domain.Session{}, errors.New("session, reviewer, matching actor, and rubric version are required")
	}
	if review.Decision != "accepted" && review.Decision != "rejected" && review.Decision != "rework_required" {
		return domain.Review{}, domain.Session{}, errors.New("unsupported review decision")
	}
	if review.PIIReview != "pending" && review.PIIReview != "passed" && review.PIIReview != "failed" {
		return domain.Review{}, domain.Session{}, errors.New("unsupported PII review state")
	}
	if review.Decision == "accepted" && review.PIIReview != "passed" {
		return domain.Review{}, domain.Session{}, ErrPIIGate
	}
	var redactionCanonical []byte
	var redactionHash, redactionPlanID, redactionJobID string
	if review.RedactionDocument != nil {
		if review.Decision != "accepted" || review.PIIReview != "passed" {
			return domain.Review{}, domain.Session{}, fmt.Errorf("%w: video redaction can only accompany an accepted, PII-approved review", ErrInvalidRedactionPlan)
		}
		var err error
		redactionCanonical, err = canonicalRedactionDocument(*review.RedactionDocument, review.SessionID)
		if err != nil {
			return domain.Review{}, domain.Session{}, fmt.Errorf("%w: %v", ErrInvalidRedactionPlan, err)
		}
		digest := sha256.Sum256(redactionCanonical)
		redactionHash = hex.EncodeToString(digest[:])
		redactionPlanID, err = newID("redaction")
		if err != nil {
			return domain.Review{}, domain.Session{}, err
		}
		redactionJobID, err = newID("job")
		if err != nil {
			return domain.Review{}, domain.Session{}, err
		}
	}
	var scores map[string]any
	if !json.Valid(review.Scores) || json.Unmarshal(review.Scores, &scores) != nil || len(scores) == 0 {
		return domain.Review{}, domain.Session{}, errors.New("scores must be a non-empty JSON object")
	}
	reviewID, err := newID("review")
	if err != nil {
		return domain.Review{}, domain.Session{}, err
	}
	var reworkSessionID string
	if review.Decision == "rework_required" {
		reworkSessionID, err = newID("sess")
		if err != nil {
			return domain.Review{}, domain.Session{}, err
		}
	}
	review.ID = reviewID
	review.CreatedAt = now.UTC()
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return domain.Review{}, domain.Session{}, err
	}
	defer tx.Rollback(ctx)
	var session domain.Session
	if err := tx.QueryRow(ctx, `SELECT id, assignment_id, state, updated_at FROM sessions WHERE id=$1 FOR UPDATE`, review.SessionID).
		Scan(&session.ID, &session.AssignmentID, &session.State, &session.UpdatedAt); errors.Is(err, pgx.ErrNoRows) {
		return domain.Review{}, domain.Session{}, ErrNotFound
	} else if err != nil {
		return domain.Review{}, domain.Session{}, err
	}
	if session.State != domain.SessionReadyForReview {
		return domain.Review{}, domain.Session{}, fmt.Errorf("%w: review requires READY_FOR_REVIEW, current state is %s", domain.ErrInvalidTransition, session.State)
	}
	var claimOwner string
	var expiresAt time.Time
	if err := tx.QueryRow(ctx, `SELECT reviewer_id, expires_at FROM review_claims WHERE session_id=$1 FOR UPDATE`, review.SessionID).
		Scan(&claimOwner, &expiresAt); errors.Is(err, pgx.ErrNoRows) {
		return domain.Review{}, domain.Session{}, ErrReviewLease
	} else if err != nil {
		return domain.Review{}, domain.Session{}, err
	}
	if claimOwner != review.ReviewerID || !expiresAt.After(now.UTC()) {
		return domain.Review{}, domain.Session{}, ErrReviewLease
	}
	target := map[string]domain.SessionState{
		"accepted":        domain.SessionAccepted,
		"rejected":        domain.SessionRejected,
		"rework_required": domain.SessionReworkRequired,
	}[review.Decision]
	if err := session.Transition(domain.TransitionCommand{To: target, Actor: actor, RequestID: requestID, Timestamp: now}); err != nil {
		return domain.Review{}, domain.Session{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO reviews (id, session_id, reviewer_id, rubric_version, scores, comments, decision, pii_review, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, review.ID, review.SessionID, review.ReviewerID, review.RubricVersion, string(review.Scores), review.Comments, review.Decision, review.PIIReview, review.CreatedAt); err != nil {
		return domain.Review{}, domain.Session{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET state=$1, updated_at=$2 WHERE id=$3`, session.State, session.UpdatedAt, session.ID); err != nil {
		return domain.Review{}, domain.Session{}, err
	}
	if review.RedactionDocument != nil {
		var deletionPending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM deletion_requests WHERE session_id=$1 AND state IN ('queued','leased','blocked','dead_letter')
		)`, review.SessionID).Scan(&deletionPending); err != nil {
			return domain.Review{}, domain.Session{}, err
		}
		if deletionPending {
			return domain.Review{}, domain.Session{}, ErrDeletionPending
		}
		rows, err := tx.Query(ctx, `SELECT logical_key FROM artifacts WHERE session_id=$1 AND media_type='video/mp4' AND purged_at IS NULL ORDER BY logical_key`, review.SessionID)
		if err != nil {
			return domain.Review{}, domain.Session{}, err
		}
		videoKeys := make([]string, 0)
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				return domain.Review{}, domain.Session{}, err
			}
			videoKeys = append(videoKeys, key)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return domain.Review{}, domain.Session{}, err
		}
		rows.Close()
		plannedKeys := make([]string, len(review.RedactionDocument.Segments))
		for index, segment := range review.RedactionDocument.Segments {
			plannedKeys[index] = segment.SourceKey
		}
		sort.Strings(plannedKeys)
		if len(videoKeys) == 0 || len(videoKeys) != len(plannedKeys) {
			return domain.Review{}, domain.Session{}, ErrRedactionEligibility
		}
		for index := range videoKeys {
			if videoKeys[index] != plannedKeys[index] {
				return domain.Review{}, domain.Session{}, ErrRedactionEligibility
			}
		}
		var planVersion int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version),0)+1 FROM redaction_plans WHERE session_id=$1`, review.SessionID).Scan(&planVersion); err != nil {
			return domain.Review{}, domain.Session{}, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO redaction_plans (id,session_id,reviewer_id,version,schema_version,document,document_hash,request_id,created_at)
			VALUES ($1,$2,$3,$4,'redaction/v1',$5,$6,$7,$8)`, redactionPlanID, review.SessionID,
			review.ReviewerID, planVersion, redactionCanonical, redactionHash, requestID, now.UTC()); err != nil {
			return domain.Review{}, domain.Session{}, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO redaction_jobs (id,plan_id,session_id,state,available_at,created_at,updated_at)
			VALUES ($1,$2,$3,'queued',$4,$4,$4)`, redactionJobID, redactionPlanID, review.SessionID, now.UTC()); err != nil {
			return domain.Review{}, domain.Session{}, err
		}
		if err := insertAudit(ctx, tx, domain.AuditEvent{
			Actor: review.ReviewerID, Action: "redaction_plan.approved", Resource: "redaction-plan/" + redactionPlanID,
			Timestamp: now.UTC(), RequestID: requestID,
			Metadata: map[string]string{"session_id": review.SessionID, "version": fmt.Sprint(planVersion), "document_hash": redactionHash},
		}); err != nil {
			return domain.Review{}, domain.Session{}, err
		}
		if err := insertAudit(ctx, tx, domain.AuditEvent{
			Actor: review.ReviewerID, Action: "redaction_job.queued", Resource: "redaction-job/" + redactionJobID,
			Timestamp: now.UTC(), RequestID: requestID,
			Metadata: map[string]string{"session_id": review.SessionID, "plan_id": redactionPlanID},
		}); err != nil {
			return domain.Review{}, domain.Session{}, err
		}
	}
	if reworkSessionID != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO sessions (id, assignment_id, state, rework_of_session_id, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$5)`,
			reworkSessionID, session.AssignmentID, domain.SessionAssigned, session.ID, now.UTC()); err != nil {
			return domain.Review{}, domain.Session{}, err
		}
		if err := insertAudit(ctx, tx, domain.AuditEvent{
			Actor: actor, Action: "session.rework_assigned", Resource: "session/" + reworkSessionID,
			Timestamp: now.UTC(), RequestID: requestID,
			Metadata: map[string]string{
				"assignment_id":        session.AssignmentID,
				"rework_of_session_id": session.ID,
				"review_id":            review.ID,
			},
		}); err != nil {
			return domain.Review{}, domain.Session{}, err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM review_claims WHERE session_id=$1`, review.SessionID); err != nil {
		return domain.Review{}, domain.Session{}, err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: actor, Action: "review.submitted", Resource: "review/" + review.ID,
		Timestamp: now.UTC(), RequestID: requestID,
		Metadata: map[string]string{"session_id": review.SessionID, "decision": review.Decision, "pii_review": review.PIIReview, "rubric_version": review.RubricVersion},
	}); err != nil {
		return domain.Review{}, domain.Session{}, err
	}
	if err := insertAudit(ctx, tx, session.AuditEvents[len(session.AuditEvents)-1]); err != nil {
		return domain.Review{}, domain.Session{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Review{}, domain.Session{}, err
	}
	return review, session, nil
}

func (store *Store) ListReviews(ctx context.Context, sessionID string) ([]domain.Review, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT id, session_id, reviewer_id, rubric_version, scores, comments, decision, pii_review, created_at
		FROM reviews WHERE session_id=$1 ORDER BY created_at, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reviews := make([]domain.Review, 0)
	for rows.Next() {
		var review domain.Review
		if err := rows.Scan(&review.ID, &review.SessionID, &review.ReviewerID, &review.RubricVersion, &review.Scores, &review.Comments, &review.Decision, &review.PIIReview, &review.CreatedAt); err != nil {
			return nil, err
		}
		reviews = append(reviews, review)
	}
	return reviews, rows.Err()
}
