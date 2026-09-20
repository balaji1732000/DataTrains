package httpapi

import (
	"errors"
	"net/http"

	"trajectory.local/api/internal/database"
	"trajectory.local/api/internal/domain"
)

type redactionRegionInput struct {
	StartNS int64   `json:"start_ns"`
	EndNS   int64   `json:"end_ns"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Width   float64 `json:"width"`
	Height  float64 `json:"height"`
	Kind    string  `json:"kind"`
}

type redactionSegmentInput struct {
	SegmentID string                 `json:"segment_id"`
	SourceKey string                 `json:"source_key"`
	Decision  string                 `json:"decision"`
	Regions   []redactionRegionInput `json:"regions"`
}

func (api *API) createRedactionPlan(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SchemaVersion string                  `json:"schema_version"`
		Segments      []redactionSegmentInput `json:"segments"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	document := domain.RedactionDocument{SchemaVersion: input.SchemaVersion, Segments: make([]domain.RedactionSegment, len(input.Segments))}
	for index, segment := range input.Segments {
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
	plan, job, err := api.store.CreateRedactionPlan(r.Context(), domain.RedactionPlan{
		SessionID: r.PathValue("session_id"), ReviewerID: meta.actor, Document: document,
	}, meta.actor, meta.requestID, api.now())
	if errors.Is(err, database.ErrRedactionEligibility) {
		writeError(w, http.StatusUnprocessableEntity, "redaction_ineligible", err.Error())
		return
	}
	if errors.Is(err, database.ErrInvalidRedactionPlan) {
		writeError(w, http.StatusBadRequest, "invalid_redaction_plan", err.Error())
		return
	}
	if errors.Is(err, database.ErrRedactionConflict) {
		writeError(w, http.StatusConflict, "redaction_request_conflict", err.Error())
		return
	}
	if errors.Is(err, database.ErrDeletionPending) {
		writeError(w, http.StatusConflict, "deletion_pending", err.Error())
		return
	}
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"plan": redactionPlanResponse(plan), "job": redactionJobResponse(job)})
}

func (api *API) listRedactionPlans(w http.ResponseWriter, r *http.Request) {
	plans, err := api.store.ListRedactionPlans(r.Context(), r.PathValue("session_id"))
	if respondStoreError(w, err) {
		return
	}
	items := make([]map[string]any, len(plans))
	for index, plan := range plans {
		items[index] = redactionPlanResponse(plan)
	}
	writeJSON(w, http.StatusOK, map[string]any{"plans": items})
}

func (api *API) listDeadLetterRedactionJobs(w http.ResponseWriter, r *http.Request) {
	current, _ := principal(r)
	var organizationIDs []string
	if !current.Local {
		organizationIDs = current.Access.OrganizationIDsForRoles("admin")
	}
	jobs, err := api.store.ListDeadLetterRedactionJobs(r.Context(), organizationIDs)
	if respondStoreError(w, err) {
		return
	}
	items := make([]map[string]any, len(jobs))
	for index, job := range jobs {
		items[index] = redactionJobResponse(job)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": items})
}

func (api *API) retryDeadLetterRedactionJob(w http.ResponseWriter, r *http.Request) {
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	job, err := api.store.RetryDeadLetterRedactionJob(r.Context(), r.PathValue("job_id"), meta.actor, meta.requestID, api.now())
	if errors.Is(err, database.ErrRedactionNotDead) {
		writeError(w, http.StatusConflict, "redaction_job_not_dead_lettered", err.Error())
		return
	}
	if errors.Is(err, database.ErrDeletionPending) {
		writeError(w, http.StatusConflict, "deletion_pending", err.Error())
		return
	}
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, redactionJobResponse(job))
}

func redactionPlanResponse(plan domain.RedactionPlan) map[string]any {
	segments := make([]map[string]any, len(plan.Document.Segments))
	for index, segment := range plan.Document.Segments {
		regions := make([]map[string]any, len(segment.Regions))
		for regionIndex, region := range segment.Regions {
			regions[regionIndex] = map[string]any{
				"start_ns": region.StartNS, "end_ns": region.EndNS,
				"x": region.X, "y": region.Y, "width": region.Width, "height": region.Height,
				"kind": region.Kind,
			}
		}
		segments[index] = map[string]any{
			"segment_id": segment.SegmentID, "source_key": segment.SourceKey,
			"decision": segment.Decision, "regions": regions,
		}
	}
	return map[string]any{
		"id": plan.ID, "session_id": plan.SessionID, "reviewer_id": plan.ReviewerID,
		"version": plan.Version, "schema_version": plan.SchemaVersion,
		"document_hash": plan.DocumentHash, "segments": segments, "created_at": plan.CreatedAt,
	}
}

func redactionJobResponse(job domain.RedactionJob) map[string]any {
	response := map[string]any{
		"id": job.ID, "plan_id": job.PlanID, "session_id": job.SessionID,
		"state": job.State, "attempt": job.Attempt, "available_at": job.AvailableAt,
		"output_manifest_key": job.OutputManifestKey,
	}
	if job.ManualRequeues > 0 {
		response["manual_requeues"] = job.ManualRequeues
	}
	if job.LastError != "" {
		response["last_error"] = job.LastError
	}
	if !job.DeadLetteredAt.IsZero() && job.DeadLetteredAt.Unix() > 0 {
		response["dead_lettered_at"] = job.DeadLetteredAt
	}
	return response
}
