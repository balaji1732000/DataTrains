package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"trajectory.local/api/internal/database"
	"trajectory.local/api/internal/domain"
)

func (api *API) listReviewQueue(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if supplied := r.URL.Query().Get("limit"); supplied != "" {
		parsed, err := strconv.Atoi(supplied)
		if err != nil || parsed < 1 || parsed > 200 {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 200")
			return
		}
		limit = parsed
	}
	current, _ := principal(r)
	var items []database.ReviewQueueItem
	var err error
	if current.Local {
		items, err = api.store.ListReviewQueue(r.Context(), api.now(), limit)
	} else {
		items, err = api.store.ListReviewQueueForOrganizations(r.Context(), api.now(), limit, current.Access.OrganizationIDsForRoles("admin", "reviewer"))
	}
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": items})
}

func (api *API) claimReview(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ReviewerID   string `json:"reviewer_id"`
		LeaseSeconds int    `json:"lease_seconds"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	if input.LeaseSeconds == 0 {
		input.LeaseSeconds = 30 * 60
	}
	if input.ReviewerID == "" || input.ReviewerID != meta.actor {
		writeError(w, http.StatusForbidden, "reviewer_mismatch", "reviewers may only claim work for their own actor identity")
		return
	}
	claim, err := api.store.ClaimReview(r.Context(), r.PathValue("session_id"), input.ReviewerID, meta.actor, meta.requestID, time.Duration(input.LeaseSeconds)*time.Second, api.now())
	if errors.Is(err, database.ErrReviewClaimed) {
		writeError(w, http.StatusConflict, "review_already_claimed", err.Error())
		return
	}
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": claim.SessionID, "reviewer_id": claim.ReviewerID,
		"claimed_at": claim.ClaimedAt, "expires_at": claim.ExpiresAt,
	})
}

func (api *API) releaseReview(w http.ResponseWriter, r *http.Request) {
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	reviewerID := r.URL.Query().Get("reviewer_id")
	if reviewerID == "" || reviewerID != meta.actor {
		writeError(w, http.StatusForbidden, "reviewer_mismatch", "reviewers may only release their own claim")
		return
	}
	err := api.store.ReleaseReview(r.Context(), r.PathValue("session_id"), reviewerID, meta.actor, meta.requestID, api.now())
	if errors.Is(err, database.ErrReviewLease) {
		writeError(w, http.StatusConflict, "review_lease_invalid", err.Error())
		return
	}
	if respondStoreError(w, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *API) submitReview(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ReviewerID    string          `json:"reviewer_id"`
		RubricVersion string          `json:"rubric_version"`
		Scores        json.RawMessage `json:"scores"`
		Comments      string          `json:"comments"`
		Decision      string          `json:"decision"`
		PIIReview     string          `json:"pii_review"`
		RedactionPlan *struct {
			SchemaVersion string                  `json:"schema_version"`
			Segments      []redactionSegmentInput `json:"segments"`
		} `json:"redaction_plan"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	if input.ReviewerID == "" || input.ReviewerID != meta.actor {
		writeError(w, http.StatusForbidden, "reviewer_mismatch", "reviewers may only submit reviews for their own actor identity")
		return
	}
	var redactionDocument *domain.RedactionDocument
	if input.RedactionPlan != nil {
		document := domain.RedactionDocument{
			SchemaVersion: input.RedactionPlan.SchemaVersion,
			Segments:      make([]domain.RedactionSegment, len(input.RedactionPlan.Segments)),
		}
		for index, segment := range input.RedactionPlan.Segments {
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
		redactionDocument = &document
	}
	review, session, err := api.store.SubmitReview(r.Context(), domain.Review{
		SessionID: r.PathValue("session_id"), ReviewerID: input.ReviewerID,
		RubricVersion: input.RubricVersion, Scores: input.Scores, Comments: input.Comments,
		Decision: input.Decision, PIIReview: input.PIIReview, RedactionDocument: redactionDocument,
	}, meta.actor, meta.requestID, api.now())
	if errors.Is(err, database.ErrReviewLease) {
		writeError(w, http.StatusConflict, "review_lease_invalid", err.Error())
		return
	}
	if errors.Is(err, database.ErrPIIGate) {
		writeError(w, http.StatusUnprocessableEntity, "pii_gate_failed", err.Error())
		return
	}
	if errors.Is(err, database.ErrInvalidRedactionPlan) || errors.Is(err, database.ErrRedactionEligibility) {
		writeError(w, http.StatusUnprocessableEntity, "redaction_plan_rejected", err.Error())
		return
	}
	if errors.Is(err, database.ErrDeletionPending) {
		writeError(w, http.StatusConflict, "deletion_pending", err.Error())
		return
	}
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"review": reviewResponse(review), "session": sessionResponse(session),
		"redaction_queued": redactionDocument != nil,
	})
}

func (api *API) listReviews(w http.ResponseWriter, r *http.Request) {
	reviews, err := api.store.ListReviews(r.Context(), r.PathValue("session_id"))
	if respondStoreError(w, err) {
		return
	}
	items := make([]map[string]any, 0, len(reviews))
	for _, review := range reviews {
		items = append(items, reviewResponse(review))
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": items})
}

func reviewResponse(review domain.Review) map[string]any {
	var scores any
	_ = json.Unmarshal(review.Scores, &scores)
	return map[string]any{
		"id": review.ID, "session_id": review.SessionID, "reviewer_id": review.ReviewerID,
		"rubric_version": review.RubricVersion, "scores": scores, "comments": review.Comments,
		"decision": review.Decision, "pii_review": review.PIIReview, "created_at": review.CreatedAt,
	}
}
