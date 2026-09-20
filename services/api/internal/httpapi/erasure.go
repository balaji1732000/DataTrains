package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"trajectory.local/api/internal/database"
	"trajectory.local/api/internal/domain"
)

func (api *API) placeLegalHold(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Reason string `json:"reason"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}
	meta, err := metadata(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", err.Error())
		return
	}
	hold, err := api.store.PlaceLegalHold(r.Context(), r.PathValue("session_id"), input.Reason, meta.actor, meta.requestID, api.now())
	if errors.Is(err, database.ErrDeletionNotAllowed) || errors.Is(err, database.ErrLegalHoldExists) ||
		errors.Is(err, database.ErrRetentionPurgeInProgress) || (err != nil && strings.Contains(err.Error(), "already erasing")) {
		writeError(w, http.StatusConflict, "legal_hold_conflict", err.Error())
		return
	}
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, legalHoldResponse(hold))
}

func (api *API) releaseLegalHold(w http.ResponseWriter, r *http.Request) {
	meta, err := metadata(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", err.Error())
		return
	}
	err = api.store.ReleaseLegalHold(r.Context(), r.PathValue("session_id"), r.PathValue("hold_id"), meta.actor, meta.requestID, api.now())
	if respondStoreError(w, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *API) requestSessionDeletion(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Reason string `json:"reason"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}
	meta, err := metadata(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", err.Error())
		return
	}
	request, err := api.store.RequestSessionDeletion(r.Context(), r.PathValue("session_id"), input.Reason, meta.actor, meta.requestID, api.now())
	if errors.Is(err, database.ErrLegalHoldActive) {
		writeError(w, http.StatusConflict, "legal_hold_active", err.Error())
		return
	}
	if errors.Is(err, database.ErrDeletionNotAllowed) {
		writeError(w, http.StatusConflict, "session_not_deletable", err.Error())
		return
	}
	if errors.Is(err, database.ErrDeletionPending) {
		writeError(w, http.StatusConflict, "deletion_already_pending", err.Error())
		return
	}
	if errors.Is(err, database.ErrRedactionInProgress) {
		writeError(w, http.StatusConflict, "redaction_in_progress", err.Error())
		return
	}
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusAccepted, deletionRequestResponse(request))
}

func (api *API) retryDeadLetterDeletionRequest(w http.ResponseWriter, r *http.Request) {
	meta, err := metadata(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", err.Error())
		return
	}
	request, err := api.store.RetryDeadLetterDeletionRequest(r.Context(), r.PathValue("deletion_id"), meta.actor, meta.requestID, api.now())
	if errors.Is(err, database.ErrDeletionNotDead) {
		writeError(w, http.StatusConflict, "deletion_not_dead_lettered", err.Error())
		return
	}
	if errors.Is(err, database.ErrLegalHoldActive) {
		writeError(w, http.StatusConflict, "legal_hold_active", err.Error())
		return
	}
	if errors.Is(err, database.ErrDeletionNotAllowed) {
		writeError(w, http.StatusConflict, "session_not_deletable", err.Error())
		return
	}
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, deletionRequestResponse(request))
}

func (api *API) listDeletionRequests(w http.ResponseWriter, r *http.Request) {
	current, _ := principal(r)
	var organizationIDs []string
	if !current.Local {
		organizationIDs = current.Access.OrganizationIDsForRoles("admin")
	}
	requests, err := api.store.ListDeletionRequests(r.Context(), organizationIDs)
	if respondStoreError(w, err) {
		return
	}
	items := make([]map[string]any, 0, len(requests))
	for _, request := range requests {
		items = append(items, deletionRequestResponse(request))
	}
	writeJSON(w, http.StatusOK, map[string]any{"deletion_requests": items})
}

func (api *API) listActiveLegalHolds(w http.ResponseWriter, r *http.Request) {
	current, _ := principal(r)
	var organizationIDs []string
	if !current.Local {
		organizationIDs = current.Access.OrganizationIDsForRoles("admin")
	}
	holds, err := api.store.ListActiveLegalHolds(r.Context(), organizationIDs)
	if respondStoreError(w, err) {
		return
	}
	items := make([]map[string]any, 0, len(holds))
	for _, hold := range holds {
		items = append(items, legalHoldResponse(hold))
	}
	writeJSON(w, http.StatusOK, map[string]any{"legal_holds": items})
}

func legalHoldResponse(hold domain.LegalHold) map[string]any {
	return map[string]any{
		"id": hold.ID, "organization_id": hold.OrganizationID, "session_id": hold.SessionID,
		"reason": hold.Reason, "placed_by": hold.PlacedBy, "placed_at": hold.PlacedAt,
	}
}

func deletionRequestResponse(request domain.DeletionRequest) map[string]any {
	response := map[string]any{
		"id": request.ID, "organization_id": request.OrganizationID,
		"session_id": request.SessionID, "reason": request.Reason, "state": request.State,
		"object_count": len(request.ObjectKeys), "attempt": request.Attempt,
		"manual_requeues": request.ManualRequeues,
		"requested_by":    request.RequestedBy, "requested_at": request.RequestedAt,
	}
	if request.LastError != "" {
		response["last_error"] = request.LastError
	}
	return response
}
