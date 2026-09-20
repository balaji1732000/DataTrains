package httpapi

import (
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"trajectory.local/api/internal/database"
	"trajectory.local/api/internal/id"
)

func (api *API) createInvitation(w http.ResponseWriter, r *http.Request) {
	var input struct {
		OrganizationID string `json:"organization_id"`
		Email          string `json:"email"`
		Role           string `json:"role"`
		ExpiresInHours int    `json:"expires_in_hours"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}
	parsedEmail, err := mail.ParseAddress(strings.TrimSpace(input.Email))
	if err != nil || parsedEmail.Address != strings.TrimSpace(input.Email) ||
		input.OrganizationID == "" || !oneOf(input.Role, "admin", "reviewer", "contributor") {
		writeError(w, http.StatusBadRequest, "invalid_invitation", "organization_id, a plain email address, and a supported role are required")
		return
	}
	if input.ExpiresInHours == 0 {
		input.ExpiresInHours = 72
	}
	if input.ExpiresInHours < 1 || input.ExpiresInHours > 168 {
		writeError(w, http.StatusBadRequest, "invalid_invitation_expiry", "invitation lifetime must be between 1 and 168 hours")
		return
	}
	current, _ := principal(r)
	if !current.Local && !current.Access.HasOrganizationRole(input.OrganizationID, "admin") {
		writeError(w, http.StatusForbidden, "forbidden", "administrators may invite people only to their own organization")
		return
	}
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	expiresAt := api.now().Add(time.Duration(input.ExpiresInHours) * time.Hour)
	invitation, token, err := api.store.CreateInvitation(
		r.Context(), input.OrganizationID, parsedEmail.Address, input.Role,
		meta.actor, meta.requestID, expiresAt, api.now(),
	)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": invitation.ID, "organization_id": invitation.OrganizationID,
		"email": invitation.Email, "role": invitation.Role,
		"expires_at": invitation.ExpiresAt, "token": token,
	})
}

func (api *API) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Token string `json:"token"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}
	current, ok := principal(r)
	if !ok || current.Identity.Issuer == "" || current.Identity.Subject == "" {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "valid OpenID authentication is required")
		return
	}
	requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
	if requestID == "" {
		requestID, _ = id.New("req")
	}
	result, err := api.store.AcceptInvitation(
		r.Context(), input.Token, current.Identity.Issuer, current.Identity.Subject,
		current.Identity.Email, current.Identity.Name, requestID, current.Identity.EmailVerified, api.now(),
	)
	if errors.Is(err, database.ErrInvitationInvalid) {
		writeError(w, http.StatusForbidden, "invitation_invalid", "the invitation is invalid, expired, already used, or does not match a verified email")
		return
	}
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"account_id": result.AccountID, "organization_id": result.OrganizationID,
		"role": result.Role, "contributor_id": result.ContributorID,
	})
}
