package httpapi

import (
	"errors"
	"net/http"

	"trajectory.local/api/internal/database"
	"trajectory.local/api/internal/domain"
)

func (api *API) getRetentionPolicy(w http.ResponseWriter, r *http.Request) {
	organizationID := r.PathValue("organization_id")
	current, _ := principal(r)
	if !current.Local && !current.Access.HasOrganizationRole(organizationID, "admin") {
		writeError(w, http.StatusForbidden, "forbidden", "administrators may view retention only for their own organization")
		return
	}
	policy, err := api.store.GetRetentionPolicy(r.Context(), organizationID)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, retentionPolicyResponse(policy))
}

func (api *API) updateRetentionPolicy(w http.ResponseWriter, r *http.Request) {
	organizationID := r.PathValue("organization_id")
	current, _ := principal(r)
	if !current.Local && !current.Access.HasOrganizationRole(organizationID, "admin") {
		writeError(w, http.StatusForbidden, "forbidden", "administrators may update retention only for their own organization")
		return
	}
	var input struct {
		RawDays     int `json:"raw_days"`
		DerivedDays int `json:"derived_days"`
		ReleaseDays int `json:"release_days"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	policy, err := api.store.UpsertRetentionPolicy(r.Context(), domain.RetentionPolicy{
		OrganizationID: organizationID, RawDays: input.RawDays,
		DerivedDays: input.DerivedDays, ReleaseDays: input.ReleaseDays,
	}, meta.actor, meta.requestID, api.now())
	if errors.Is(err, database.ErrInvalidRetentionPolicy) {
		writeError(w, http.StatusBadRequest, "invalid_retention_policy", err.Error())
		return
	}
	if errors.Is(err, database.ErrRetentionPurgeInProgress) {
		writeError(w, http.StatusConflict, "retention_purge_in_progress", err.Error())
		return
	}
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, retentionPolicyResponse(policy))
}

func retentionPolicyResponse(policy domain.RetentionPolicy) map[string]any {
	return map[string]any{
		"organization_id": policy.OrganizationID, "raw_days": policy.RawDays,
		"derived_days": policy.DerivedDays, "release_days": policy.ReleaseDays,
		"updated_by": policy.UpdatedBy, "created_at": policy.CreatedAt, "updated_at": policy.UpdatedAt,
	}
}

func (api *API) listRetentionPurgeRequests(w http.ResponseWriter, r *http.Request) {
	current, _ := principal(r)
	var organizationIDs []string
	if !current.Local {
		organizationIDs = current.Access.OrganizationIDsForRoles("admin")
	}
	requests, err := api.store.ListRetentionPurgeRequests(r.Context(), organizationIDs)
	if respondStoreError(w, err) {
		return
	}
	items := make([]map[string]any, 0, len(requests))
	for _, request := range requests {
		items = append(items, retentionPurgeResponse(request))
	}
	writeJSON(w, http.StatusOK, map[string]any{"retention_purge_requests": items})
}

func (api *API) retryDeadLetterRetentionPurge(w http.ResponseWriter, r *http.Request) {
	requestID := r.PathValue("request_id")
	organizationID, err := api.store.GetRetentionPurgeOrganization(r.Context(), requestID)
	if respondStoreError(w, err) {
		return
	}
	current, _ := principal(r)
	if !current.Local && !current.Access.HasOrganizationRole(organizationID, "admin") {
		writeError(w, http.StatusForbidden, "forbidden", "this retention purge belongs to another organization")
		return
	}
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	request, err := api.store.RetryDeadLetterRetentionPurge(r.Context(), requestID, meta.actor, meta.requestID, api.now())
	if errors.Is(err, database.ErrRetentionNotDead) {
		writeError(w, http.StatusConflict, "retention_purge_not_dead_lettered", err.Error())
		return
	}
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, retentionPurgeResponse(request))
}

func retentionPurgeResponse(request domain.RetentionPurgeRequest) map[string]any {
	return map[string]any{
		"id": request.ID, "organization_id": request.OrganizationID,
		"resource_type": request.ResourceType, "resource_id": request.ResourceID,
		"state": request.State, "attempt": request.Attempt,
		"object_count": len(request.ObjectKeys), "policy_updated_at": request.PolicyUpdatedAt,
		"available_at": request.AvailableAt, "requested_at": request.RequestedAt,
		"last_error": request.LastError,
	}
}
