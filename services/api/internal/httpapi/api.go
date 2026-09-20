package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"trajectory.local/api/internal/authn"
	"trajectory.local/api/internal/blobstore"
	"trajectory.local/api/internal/database"
	"trajectory.local/api/internal/domain"
	"trajectory.local/api/internal/id"
	"trajectory.local/api/internal/releases"
)

type API struct {
	store          *database.Store
	blobs          blobstore.BlobStore
	exporter       *releases.Exporter
	now            func() time.Time
	allowedOrigins map[string]bool
	authenticator  authn.Authenticator
	uploadTTL      time.Duration
	logger         *slog.Logger
	traceProject   string
}

func New(store *database.Store) *API {
	return &API{store: store, now: time.Now, authenticator: authn.HeaderAuthenticator{}, uploadTTL: 15 * time.Minute, logger: discardLogger()}
}

func NewWithBlobStore(store *database.Store, blobs blobstore.BlobStore, trajectorySchema []byte) *API {
	exporter, _ := releases.NewExporter(store, blobs, trajectorySchema)
	return &API{store: store, blobs: blobs, exporter: exporter, now: time.Now, allowedOrigins: defaultAllowedOrigins(), authenticator: authn.HeaderAuthenticator{}, uploadTTL: 15 * time.Minute, logger: discardLogger()}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func (api *API) WithAuthenticator(authenticator authn.Authenticator) *API {
	api.authenticator = authenticator
	return api
}

func (api *API) WithUploadAuthorizationTTL(ttl time.Duration) *API {
	api.uploadTTL = ttl
	return api
}

func (api *API) WithLogger(logger *slog.Logger) *API {
	if logger != nil {
		api.logger = logger
	}
	return api
}

func (api *API) WithTraceProject(projectID string) *API {
	api.traceProject = strings.TrimSpace(projectID)
	return api
}

func (api *API) WithAllowedOrigins(origins []string) *API {
	api.allowedOrigins = make(map[string]bool, len(origins))
	for _, origin := range origins {
		api.allowedOrigins[origin] = true
	}
	return api
}

func (api *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", api.live)
	mux.HandleFunc("GET /readyz", api.health)
	mux.HandleFunc("GET /healthz", api.health)
	mux.HandleFunc("GET /v1/me", api.me)
	mux.HandleFunc("POST /v1/invitations", api.authorize(api.createInvitation, "admin"))
	mux.HandleFunc("POST /v1/invitations/accept", api.acceptInvitation)
	mux.HandleFunc("GET /v1/organizations/{organization_id}/contributors", api.authorize(api.listOrganizationContributors, "admin"))
	mux.HandleFunc("GET /v1/organizations/{organization_id}/retention-policy", api.authorize(api.getRetentionPolicy, "admin"))
	mux.HandleFunc("PUT /v1/organizations/{organization_id}/retention-policy", api.authorize(api.updateRetentionPolicy, "admin"))
	mux.HandleFunc("POST /v1/organizations", api.authorize(api.createOrganization, "admin"))
	mux.HandleFunc("POST /v1/campaigns", api.authorize(api.bootstrapCampaign, "admin"))
	mux.HandleFunc("POST /v1/projects", api.authorize(api.createProject, "admin"))
	mux.HandleFunc("GET /v1/projects", api.authorize(api.listProjects, "admin", "reviewer"))
	mux.HandleFunc("GET /v1/projects/{project_id}/sessions", api.authorizeProject(api.listProjectSessions, "admin", "reviewer"))
	mux.HandleFunc("POST /v1/task-templates", api.authorize(api.createTaskTemplate, "admin"))
	mux.HandleFunc("POST /v1/tasks", api.authorize(api.createTask, "admin"))
	mux.HandleFunc("POST /v1/contributors", api.authorize(api.createContributor, "admin"))
	mux.HandleFunc("POST /v1/assignments", api.authorize(api.createAssignment, "admin"))
	mux.HandleFunc("POST /v1/consent-documents", api.authorize(api.createConsentDocument, "admin"))
	mux.HandleFunc("GET /v1/consent-documents/current", api.authorize(api.currentConsentDocument, "contributor"))
	mux.HandleFunc("POST /v1/consent-acceptances", api.authorize(api.acceptConsent, "contributor"))
	mux.HandleFunc("POST /v1/sessions/{session_id}/preflight", api.authorizeSession(api.completePreflight, "contributor"))
	mux.HandleFunc("POST /v1/sessions/{session_id}/transitions", api.authorizeSession(api.transitionSession, "contributor"))
	mux.HandleFunc("PUT /v1/sessions/{session_id}/artifacts/{artifact_path...}", api.authorizeSession(api.uploadArtifact, "contributor"))
	mux.HandleFunc("POST /v1/sessions/{session_id}/multipart-uploads", api.authorizeSession(api.startMultipartArtifactUpload, "contributor"))
	mux.HandleFunc("POST /v1/sessions/{session_id}/multipart-uploads/{upload_id}/parts/{part_number}/authorize", api.authorizeSession(api.authorizeMultipartArtifactPart, "contributor"))
	mux.HandleFunc("POST /v1/sessions/{session_id}/multipart-uploads/{upload_id}/parts/{part_number}/complete", api.authorizeSession(api.completeMultipartArtifactPart, "contributor"))
	mux.HandleFunc("POST /v1/sessions/{session_id}/multipart-uploads/{upload_id}/complete", api.authorizeSession(api.completeMultipartArtifactUpload, "contributor"))
	mux.HandleFunc("DELETE /v1/sessions/{session_id}/multipart-uploads/{upload_id}", api.authorizeSession(api.abortMultipartArtifactUpload, "contributor"))
	mux.HandleFunc("GET /v1/sessions/{session_id}/artifacts/{artifact_path...}", api.authorizeSession(api.downloadArtifact, "admin", "reviewer", "contributor"))
	mux.HandleFunc("HEAD /v1/sessions/{session_id}/artifacts/{artifact_path...}", api.authorizeSession(api.downloadArtifact, "admin", "reviewer", "contributor"))
	mux.HandleFunc("POST /v1/sessions/{session_id}/submission", api.authorizeSession(api.submitSession, "contributor"))
	mux.HandleFunc("GET /v1/sessions/{session_id}/trajectory", api.authorizeSession(api.downloadNormalizedTrajectory, "admin", "reviewer", "contributor"))
	mux.HandleFunc("GET /v1/sessions/{session_id}", api.authorizeSession(api.getSession, "admin", "reviewer", "contributor"))
	mux.HandleFunc("GET /v1/processing-jobs/{job_id}", api.authorizeJob(api.getProcessingJob, "admin", "reviewer", "contributor"))
	mux.HandleFunc("GET /v1/processing-jobs/dead-letter", api.authorize(api.listDeadLetterJobs, "admin"))
	mux.HandleFunc("POST /v1/processing-jobs/{job_id}/retry", api.authorizeJob(api.retryDeadLetterJob, "admin"))
	mux.HandleFunc("POST /v1/sessions/{session_id}/legal-holds", api.authorizeSession(api.placeLegalHold, "admin"))
	mux.HandleFunc("DELETE /v1/sessions/{session_id}/legal-holds/{hold_id}", api.authorizeSession(api.releaseLegalHold, "admin"))
	mux.HandleFunc("POST /v1/sessions/{session_id}/deletion-requests", api.authorizeSession(api.requestSessionDeletion, "admin"))
	mux.HandleFunc("GET /v1/deletion-requests", api.authorize(api.listDeletionRequests, "admin"))
	mux.HandleFunc("POST /v1/deletion-requests/{deletion_id}/retry", api.authorizeDeletion(api.retryDeadLetterDeletionRequest, "admin"))
	mux.HandleFunc("GET /v1/retention-purge-requests", api.authorize(api.listRetentionPurgeRequests, "admin"))
	mux.HandleFunc("POST /v1/retention-purge-requests/{request_id}/retry", api.authorize(api.retryDeadLetterRetentionPurge, "admin"))
	mux.HandleFunc("GET /v1/legal-holds", api.authorize(api.listActiveLegalHolds, "admin"))
	mux.HandleFunc("GET /v1/review-queue", api.authorize(api.listReviewQueue, "admin", "reviewer"))
	mux.HandleFunc("POST /v1/sessions/{session_id}/review-claim", api.authorizeSession(api.claimReview, "reviewer"))
	mux.HandleFunc("DELETE /v1/sessions/{session_id}/review-claim", api.authorizeSession(api.releaseReview, "reviewer"))
	mux.HandleFunc("POST /v1/sessions/{session_id}/reviews", api.authorizeSession(api.submitReview, "reviewer"))
	mux.HandleFunc("GET /v1/sessions/{session_id}/reviews", api.authorizeSession(api.listReviews, "admin", "reviewer", "contributor"))
	mux.HandleFunc("POST /v1/sessions/{session_id}/redaction-plans", api.authorizeSession(api.createRedactionPlan, "reviewer"))
	mux.HandleFunc("GET /v1/sessions/{session_id}/redaction-plans", api.authorizeSession(api.listRedactionPlans, "admin", "reviewer"))
	mux.HandleFunc("GET /v1/redaction-jobs/dead-letter", api.authorize(api.listDeadLetterRedactionJobs, "admin"))
	mux.HandleFunc("POST /v1/redaction-jobs/{job_id}/retry", api.authorizeRedactionJob(api.retryDeadLetterRedactionJob, "admin"))
	mux.HandleFunc("POST /v1/projects/{project_id}/releases", api.authorizeProject(api.createRelease, "admin"))
	mux.HandleFunc("GET /v1/releases/{release_id}", api.authorizeRelease(api.getRelease, "admin", "reviewer"))
	mux.HandleFunc("GET /v1/releases/{release_id}/files/{file_path...}", api.authorizeRelease(api.downloadReleaseFile, "admin", "reviewer"))
	mux.HandleFunc("HEAD /v1/releases/{release_id}/files/{file_path...}", api.authorizeRelease(api.downloadReleaseFile, "admin", "reviewer"))
	mux.HandleFunc("GET /v1/releases/{release_id}/bundle", api.authorizeRelease(api.downloadReleaseBundle, "admin", "reviewer"))
	mux.HandleFunc("HEAD /v1/releases/{release_id}/bundle", api.authorizeRelease(api.downloadReleaseBundle, "admin", "reviewer"))
	mux.HandleFunc("GET /v1/contributors/{contributor_id}/assignments", api.authorizeContributor(api.listContributorAssignments, "contributor"))
	return api.requestHeaders(api.accessLog(api.cors(api.authenticate(mux))))
}

func (api *API) cors(next http.Handler) http.Handler {
	allowedOrigins := api.allowedOrigins
	if allowedOrigins == nil {
		allowedOrigins = defaultAllowedOrigins()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if allowedOrigins[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Actor-ID, X-Request-ID, X-Artifact-SHA256")
		}
		if r.Method == http.MethodOptions {
			if !allowedOrigins[origin] {
				writeError(w, http.StatusForbidden, "origin_not_allowed", "collector origin is not allowed")
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func defaultAllowedOrigins() map[string]bool {
	return map[string]bool{
		"http://localhost:1420":   true,
		"http://127.0.0.1:1420":   true,
		"tauri://localhost":       true,
		"https://tauri.localhost": true,
	}
}

func (api *API) requestHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if requestID == "" {
			generated, err := id.New("req")
			if err != nil {
				writeError(w, http.StatusInternalServerError, "request_id_failed", "request could not be initialized")
				return
			}
			requestID = generated
			r.Header.Set("X-Request-ID", requestID)
		} else if !validRequestID(requestID) {
			writeError(w, http.StatusBadRequest, "invalid_request_id", "X-Request-ID contains unsupported characters")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Request-ID", requestID)
		trace, err := requestTrace(r)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "trace_id_failed", "request tracing could not be initialized")
			return
		}
		w.Header().Set("Traceparent", trace.traceparent())
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), traceContextKey{}, trace)))
	})
}

type responseStatusWriter struct {
	http.ResponseWriter
	status int
}

func (writer *responseStatusWriter) WriteHeader(status int) {
	if writer.status != 0 {
		return
	}
	writer.status = status
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *responseStatusWriter) Write(body []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	return writer.ResponseWriter.Write(body)
}

func (writer *responseStatusWriter) Unwrap() http.ResponseWriter { return writer.ResponseWriter }

func (api *API) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		writer := &responseStatusWriter{ResponseWriter: w}
		next.ServeHTTP(writer, r)
		status := writer.status
		if status == 0 {
			status = http.StatusOK
		}
		attributes := []any{
			"request_id", r.Header.Get("X-Request-ID"), "method", r.Method,
			"path", r.URL.Path, "status", status, "duration_ms", time.Since(started).Milliseconds(),
		}
		if trace, ok := traceFromContext(r.Context()); ok {
			attributes = append(attributes, "trace_id", trace.TraceID, "span_id", trace.SpanID, "trace_sampled", trace.Sampled)
			if api.traceProject != "" {
				attributes = append(attributes, "logging.googleapis.com/trace", "projects/"+api.traceProject+"/traces/"+trace.TraceID)
			}
		}
		api.logger.InfoContext(r.Context(), "http_request", attributes...)
	})
}

type traceContextKey struct{}

type traceCorrelation struct {
	TraceID, SpanID string
	Sampled         bool
}

func (trace traceCorrelation) traceparent() string {
	flags := "00"
	if trace.Sampled {
		flags = "01"
	}
	return "00-" + trace.TraceID + "-" + trace.SpanID + "-" + flags
}

func traceFromContext(ctx context.Context) (traceCorrelation, bool) {
	value, ok := ctx.Value(traceContextKey{}).(traceCorrelation)
	return value, ok
}

func requestTrace(r *http.Request) (traceCorrelation, error) {
	if trace, ok := parseTraceparent(r.Header.Get("Traceparent")); ok {
		return trace, nil
	}
	if trace, ok := parseCloudTrace(r.Header.Get("X-Cloud-Trace-Context")); ok {
		return trace, nil
	}
	return newTraceCorrelation()
}

func parseTraceparent(value string) (traceCorrelation, bool) {
	parts := strings.Split(strings.TrimSpace(value), "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 ||
		!validHex(parts[1]) || !validHex(parts[2]) || !validHex(parts[3]) || allZero(parts[1]) || allZero(parts[2]) {
		return traceCorrelation{}, false
	}
	flags, err := strconv.ParseUint(parts[3], 16, 8)
	if err != nil {
		return traceCorrelation{}, false
	}
	return traceCorrelation{TraceID: strings.ToLower(parts[1]), SpanID: strings.ToLower(parts[2]), Sampled: flags&1 == 1}, true
}

func parseCloudTrace(value string) (traceCorrelation, bool) {
	value = strings.TrimSpace(value)
	slash := strings.IndexByte(value, '/')
	if slash != 32 || !validHex(value[:slash]) || allZero(value[:slash]) {
		return traceCorrelation{}, false
	}
	remainder := value[slash+1:]
	options := ""
	if semicolon := strings.IndexByte(remainder, ';'); semicolon >= 0 {
		options = remainder[semicolon+1:]
		remainder = remainder[:semicolon]
	}
	span, err := strconv.ParseUint(remainder, 10, 64)
	if err != nil || span == 0 {
		return traceCorrelation{}, false
	}
	return traceCorrelation{
		TraceID: strings.ToLower(value[:slash]), SpanID: fmt.Sprintf("%016x", span),
		Sampled: strings.Contains(options, "o=1"),
	}, true
}

func newTraceCorrelation() (traceCorrelation, error) {
	traceID := make([]byte, 16)
	spanID := make([]byte, 8)
	if _, err := rand.Read(traceID); err != nil {
		return traceCorrelation{}, err
	}
	if _, err := rand.Read(spanID); err != nil {
		return traceCorrelation{}, err
	}
	return traceCorrelation{TraceID: hex.EncodeToString(traceID), SpanID: hex.EncodeToString(spanID)}, nil
}

func validHex(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil
}

func allZero(value string) bool {
	return strings.Trim(value, "0") == ""
}

func validRequestID(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("._:-", character) {
			continue
		}
		return false
	}
	return true
}

func (api *API) health(w http.ResponseWriter, r *http.Request) {
	if err := api.store.Pool().Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database_unavailable", "database is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"service": "trajectory-api", "status": "ok", "database": "ok"})
}

func (api *API) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"service": "trajectory-api", "status": "ok"})
}

type principalContextKey struct{}

type requestPrincipal struct {
	Identity authn.Identity
	Access   database.IdentityAccess
	Local    bool
}

type requestMetadata struct{ actor, requestID string }

func (api *API) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		identity, err := api.authenticator.Authenticate(r.Context(), r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "valid authentication is required")
			return
		}
		principal := requestPrincipal{Identity: identity}
		if identity.Issuer == authn.ModeLocal {
			principal.Local = true
			principal.Access = database.IdentityAccess{
				AccountID: identity.Subject,
				Memberships: []database.OrganizationMembership{
					{OrganizationID: "*", Role: "admin"},
					{OrganizationID: "*", Role: "reviewer"},
					{OrganizationID: "*", Role: "contributor"},
				},
			}
		} else {
			principal.Access, err = api.store.ResolveOIDCIdentity(r.Context(), identity.Issuer, identity.Subject, api.now())
			if errors.Is(err, database.ErrNotFound) {
				if r.URL.Path != "/v1/invitations/accept" {
					writeError(w, http.StatusForbidden, "identity_not_provisioned", "this account has not been invited to DataTrains")
					return
				}
				principal.Access = database.IdentityAccess{Memberships: []database.OrganizationMembership{}}
			}
			if err != nil && !errors.Is(err, database.ErrNotFound) {
				writeError(w, http.StatusServiceUnavailable, "identity_lookup_failed", "account access could not be verified")
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal)))
	})
}

func principal(r *http.Request) (requestPrincipal, bool) {
	value, ok := r.Context().Value(principalContextKey{}).(requestPrincipal)
	return value, ok
}

func (api *API) authorize(next http.HandlerFunc, roles ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		current, ok := principal(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "valid authentication is required")
			return
		}
		for _, role := range roles {
			if current.Access.HasRole(role) {
				next(w, r)
				return
			}
		}
		writeError(w, http.StatusForbidden, "forbidden", "your account does not have permission for this action")
	}
}

func (api *API) authorizeSession(next http.HandlerFunc, roles ...string) http.HandlerFunc {
	return api.authorize(func(w http.ResponseWriter, r *http.Request) {
		current, _ := principal(r)
		if current.Local {
			next(w, r)
			return
		}
		access, err := api.store.GetSessionAccess(r.Context(), r.PathValue("session_id"))
		if respondStoreError(w, err) {
			return
		}
		for _, role := range roles {
			if role == "contributor" && current.Access.ContributorID != nil &&
				*current.Access.ContributorID == access.ContributorID &&
				current.Access.HasOrganizationRole(access.OrganizationID, "contributor") {
				next(w, r)
				return
			}
			if role != "contributor" && current.Access.HasOrganizationRole(access.OrganizationID, role) {
				next(w, r)
				return
			}
		}
		writeError(w, http.StatusForbidden, "forbidden", "this session belongs to another account or organization")
	}, roles...)
}

func (api *API) authorizeProject(next http.HandlerFunc, roles ...string) http.HandlerFunc {
	return api.authorize(func(w http.ResponseWriter, r *http.Request) {
		current, _ := principal(r)
		if current.Local {
			next(w, r)
			return
		}
		organizationID, err := api.store.GetProjectOrganization(r.Context(), r.PathValue("project_id"))
		if respondStoreError(w, err) {
			return
		}
		if current.Access.HasOrganizationRole(organizationID, roles...) {
			next(w, r)
			return
		}
		writeError(w, http.StatusForbidden, "forbidden", "this project belongs to another organization")
	}, roles...)
}

func (api *API) authorizeRelease(next http.HandlerFunc, roles ...string) http.HandlerFunc {
	return api.authorize(func(w http.ResponseWriter, r *http.Request) {
		current, _ := principal(r)
		if current.Local {
			next(w, r)
			return
		}
		organizationID, err := api.store.GetReleaseOrganization(r.Context(), r.PathValue("release_id"))
		if respondStoreError(w, err) {
			return
		}
		if current.Access.HasOrganizationRole(organizationID, roles...) {
			next(w, r)
			return
		}
		writeError(w, http.StatusForbidden, "forbidden", "this release belongs to another organization")
	}, roles...)
}

func (api *API) authorizeJob(next http.HandlerFunc, roles ...string) http.HandlerFunc {
	return api.authorize(func(w http.ResponseWriter, r *http.Request) {
		current, _ := principal(r)
		if current.Local {
			next(w, r)
			return
		}
		access, err := api.store.GetProcessingJobAccess(r.Context(), r.PathValue("job_id"))
		if respondStoreError(w, err) {
			return
		}
		for _, role := range roles {
			if role == "contributor" && current.Access.ContributorID != nil &&
				*current.Access.ContributorID == access.ContributorID &&
				current.Access.HasOrganizationRole(access.OrganizationID, "contributor") {
				next(w, r)
				return
			}
			if role != "contributor" && current.Access.HasOrganizationRole(access.OrganizationID, role) {
				next(w, r)
				return
			}
		}
		writeError(w, http.StatusForbidden, "forbidden", "this processing job belongs to another account or organization")
	}, roles...)
}

func (api *API) authorizeRedactionJob(next http.HandlerFunc, roles ...string) http.HandlerFunc {
	return api.authorize(func(w http.ResponseWriter, r *http.Request) {
		current, _ := principal(r)
		if current.Local {
			next(w, r)
			return
		}
		organizationID, err := api.store.GetRedactionJobOrganization(r.Context(), r.PathValue("job_id"))
		if respondStoreError(w, err) {
			return
		}
		if current.Access.HasOrganizationRole(organizationID, roles...) {
			next(w, r)
			return
		}
		writeError(w, http.StatusForbidden, "forbidden", "this redaction job belongs to another organization")
	}, roles...)
}

func (api *API) authorizeDeletion(next http.HandlerFunc, roles ...string) http.HandlerFunc {
	return api.authorize(func(w http.ResponseWriter, r *http.Request) {
		current, _ := principal(r)
		if current.Local {
			next(w, r)
			return
		}
		organizationID, err := api.store.GetDeletionRequestOrganization(r.Context(), r.PathValue("deletion_id"))
		if respondStoreError(w, err) {
			return
		}
		if current.Access.HasOrganizationRole(organizationID, roles...) {
			next(w, r)
			return
		}
		writeError(w, http.StatusForbidden, "forbidden", "this deletion request belongs to another organization")
	}, roles...)
}

func (api *API) authorizeContributor(next http.HandlerFunc, roles ...string) http.HandlerFunc {
	return api.authorize(func(w http.ResponseWriter, r *http.Request) {
		current, _ := principal(r)
		if current.Local || (current.Access.ContributorID != nil && *current.Access.ContributorID == r.PathValue("contributor_id")) {
			next(w, r)
			return
		}
		writeError(w, http.StatusForbidden, "forbidden", "contributors may open only their own assignments")
	}, roles...)
}

func (api *API) me(w http.ResponseWriter, r *http.Request) {
	current, ok := principal(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "valid authentication is required")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id": current.Access.AccountID, "display_name": current.Access.DisplayName,
		"email": current.Access.Email, "contributor_id": current.Access.ContributorID,
		"memberships": current.Access.Memberships,
	})
}

func metadata(r *http.Request) (requestMetadata, error) {
	current, ok := principal(r)
	if !ok || current.Access.AccountID == "" {
		return requestMetadata{}, errors.New("authenticated identity is required")
	}
	requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
	if requestID == "" {
		generated, err := id.New("req")
		if err != nil {
			return requestMetadata{}, err
		}
		requestID = generated
	}
	return requestMetadata{actor: current.Access.AccountID, requestID: requestID}, nil
}

func decodeRequest(w http.ResponseWriter, r *http.Request, destination any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

func requestMetaOrError(w http.ResponseWriter, r *http.Request) (requestMetadata, bool) {
	meta, err := metadata(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return requestMetadata{}, false
	}
	return meta, true
}

func (api *API) createOrganization(w http.ResponseWriter, r *http.Request) {
	current, _ := principal(r)
	if !current.Local {
		writeError(w, http.StatusForbidden, "bootstrap_only", "new organizations are created through the controlled administrator bootstrap process")
		return
	}
	var input struct {
		Name string `json:"name"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(input.Name) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "name is required")
		return
	}
	organization, err := api.store.CreateOrganization(r.Context(), input.Name, meta.actor, meta.requestID, api.now())
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": organization.ID, "name": organization.Name})
}

func (api *API) createProject(w http.ResponseWriter, r *http.Request) {
	var input struct {
		OrganizationID string `json:"organization_id"`
		Name           string `json:"name"`
		Target         int    `json:"target_trajectories"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	if input.OrganizationID == "" || strings.TrimSpace(input.Name) == "" || input.Target < 1 {
		writeError(w, http.StatusBadRequest, "invalid_request", "organization_id, name, and positive target_trajectories are required")
		return
	}
	current, _ := principal(r)
	if !current.Local && !current.Access.HasOrganizationRole(input.OrganizationID, "admin") {
		writeError(w, http.StatusForbidden, "forbidden", "administrators may create projects only in their own organization")
		return
	}
	project, err := api.store.CreateProject(r.Context(), input.OrganizationID, input.Name, input.Target, meta.actor, meta.requestID, api.now())
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": project.ID, "organization_id": project.OrganizationID, "name": project.Name, "target_trajectories": project.TargetTrajectories})
}

func (api *API) createTaskTemplate(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ProjectID     string          `json:"project_id"`
		Name          string          `json:"name"`
		Goal          string          `json:"goal"`
		Category      string          `json:"category"`
		Difficulty    string          `json:"difficulty"`
		Specification json.RawMessage `json:"specification"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	if input.ProjectID == "" || input.Name == "" || input.Goal == "" || input.Category == "" || !oneOf(input.Difficulty, "beginner", "intermediate", "advanced") || len(input.Specification) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "project_id, name, goal, category, supported difficulty, and specification are required")
		return
	}
	current, _ := principal(r)
	if !current.Local {
		organizationID, err := api.store.GetProjectOrganization(r.Context(), input.ProjectID)
		if respondStoreError(w, err) {
			return
		}
		if !current.Access.HasOrganizationRole(organizationID, "admin") {
			writeError(w, http.StatusForbidden, "forbidden", "this project belongs to another organization")
			return
		}
	}
	template, err := api.store.CreateTaskTemplate(r.Context(), domain.TaskTemplate{ProjectID: input.ProjectID, Name: input.Name, Goal: input.Goal, Category: input.Category, Difficulty: input.Difficulty}, input.Specification, meta.actor, meta.requestID, api.now())
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": template.ID, "project_id": template.ProjectID, "name": template.Name, "goal": template.Goal, "category": template.Category, "difficulty": template.Difficulty})
}

func (api *API) createTask(w http.ResponseWriter, r *http.Request) {
	var input struct {
		TemplateID      string          `json:"template_id"`
		Goal            string          `json:"goal"`
		InputAssets     json.RawMessage `json:"input_assets"`
		ExpectedOutputs json.RawMessage `json:"expected_outputs"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	if input.TemplateID == "" || input.Goal == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "template_id and goal are required")
		return
	}
	current, _ := principal(r)
	if !current.Local {
		organizationID, err := api.store.GetTaskTemplateOrganization(r.Context(), input.TemplateID)
		if respondStoreError(w, err) {
			return
		}
		if !current.Access.HasOrganizationRole(organizationID, "admin") {
			writeError(w, http.StatusForbidden, "forbidden", "this task template belongs to another organization")
			return
		}
	}
	if len(input.InputAssets) == 0 {
		input.InputAssets = json.RawMessage("[]")
	}
	if len(input.ExpectedOutputs) == 0 {
		input.ExpectedOutputs = json.RawMessage("[]")
	}
	task, err := api.store.CreateTask(r.Context(), input.TemplateID, input.Goal, input.InputAssets, input.ExpectedOutputs, meta.actor, meta.requestID, api.now())
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": task.ID, "template_id": task.TemplateID, "goal": task.Goal})
}

func (api *API) createContributor(w http.ResponseWriter, r *http.Request) {
	current, _ := principal(r)
	if !current.Local {
		writeError(w, http.StatusForbidden, "invitation_required", "production contributors must be created through a verified invitation")
		return
	}
	var input struct {
		DisplayName string `json:"display_name"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(input.DisplayName) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "display_name is required")
		return
	}
	contributor, err := api.store.CreateContributor(r.Context(), input.DisplayName, meta.actor, meta.requestID, api.now())
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": contributor.ID, "display_name": contributor.DisplayName})
}

func (api *API) createAssignment(w http.ResponseWriter, r *http.Request) {
	var input struct {
		TaskID        string `json:"task_id"`
		ContributorID string `json:"contributor_id"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	if input.TaskID == "" || input.ContributorID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "task_id and contributor_id are required")
		return
	}
	current, _ := principal(r)
	if !current.Local {
		organizationID, err := api.store.GetTaskOrganization(r.Context(), input.TaskID)
		if respondStoreError(w, err) {
			return
		}
		if !current.Access.HasOrganizationRole(organizationID, "admin") {
			writeError(w, http.StatusForbidden, "forbidden", "this task belongs to another organization")
			return
		}
		allowed, err := api.store.ContributorHasOrganization(r.Context(), input.ContributorID, organizationID)
		if respondStoreError(w, err) {
			return
		}
		if !allowed {
			writeError(w, http.StatusForbidden, "contributor_membership_required", "the contributor is not active in this organization")
			return
		}
	}
	result, err := api.store.AssignTask(r.Context(), input.TaskID, input.ContributorID, meta.actor, meta.requestID, api.now())
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"assignment_id": result.Assignment.ID, "session_id": result.Session.ID, "session_state": result.Session.State})
}

func (api *API) createConsentDocument(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Version       string     `json:"version"`
		Body          string     `json:"body"`
		EffectiveDate *time.Time `json:"effective_date"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	if input.Version == "" || input.Body == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "version and body are required")
		return
	}
	effective := api.now()
	if input.EffectiveDate != nil {
		effective = *input.EffectiveDate
	}
	document, err := api.store.CreateConsentDocument(r.Context(), input.Version, input.Body, meta.actor, meta.requestID, effective, api.now())
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": document.ID, "version": document.Version, "text_hash": document.TextHash, "effective_date": document.EffectiveDate})
}

func (api *API) currentConsentDocument(w http.ResponseWriter, r *http.Request) {
	document, err := api.store.CurrentConsentDocument(r.Context(), api.now())
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": document.ID, "version": document.Version, "text_hash": document.TextHash,
		"body": document.Body, "effective_date": document.EffectiveDate,
	})
}

func (api *API) acceptConsent(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SessionID     string `json:"session_id"`
		ContributorID string `json:"contributor_id"`
		DocumentID    string `json:"document_id"`
		ClientVersion string `json:"client_version"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	if input.SessionID == "" || input.ContributorID == "" || input.DocumentID == "" || input.ClientVersion == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "session_id, contributor_id, document_id, and client_version are required")
		return
	}
	current, _ := principal(r)
	if !current.Local && (current.Access.ContributorID == nil || *current.Access.ContributorID != input.ContributorID) {
		writeError(w, http.StatusForbidden, "forbidden", "contributors may accept consent only for their own profile")
		return
	}
	acceptance, err := api.store.AcceptConsent(r.Context(), input.SessionID, input.ContributorID, input.DocumentID, input.ClientVersion, meta.actor, meta.requestID, api.now())
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": acceptance.ID, "accepted_at": acceptance.AcceptedAt})
}

func (api *API) completePreflight(w http.ResponseWriter, r *http.Request) {
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	session, err := api.store.TransitionSession(r.Context(), r.PathValue("session_id"), domain.SessionReady, meta.actor, meta.requestID, api.now())
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse(session))
}

func (api *API) transitionSession(w http.ResponseWriter, r *http.Request) {
	var input struct {
		State domain.SessionState `json:"state"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	session, err := api.store.TransitionSession(r.Context(), r.PathValue("session_id"), input.State, meta.actor, meta.requestID, api.now())
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse(session))
}

func (api *API) getSession(w http.ResponseWriter, r *http.Request) {
	session, err := api.store.GetSession(r.Context(), r.PathValue("session_id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse(session))
}

func (api *API) listContributorAssignments(w http.ResponseWriter, r *http.Request) {
	assignments, err := api.store.ListContributorAssignments(r.Context(), r.PathValue("contributor_id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"assignments": assignments})
}

func sessionResponse(session domain.Session) map[string]any {
	return map[string]any{"id": session.ID, "assignment_id": session.AssignmentID, "state": session.State, "updated_at": session.UpdatedAt}
}

func oneOf(value string, values ...string) bool {
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func respondStoreError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, database.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "resource was not found")
	case errors.Is(err, domain.ErrInvalidTransition), errors.Is(err, database.ErrConsentMissing):
		writeError(w, http.StatusConflict, "invalid_transition", err.Error())
	default:
		writeError(w, http.StatusConflict, "persistence_error", "request conflicts with current state")
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		panic(fmt.Sprintf("encode response: %v", err))
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
