package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"

	"trajectory.local/api/internal/blobstore"
	"trajectory.local/api/internal/database"
	"trajectory.local/api/internal/domain"
)

const maxArtifactBytes int64 = 20 << 30

type artifactUploadRequest struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type"`
}

func validateArtifactUploadRequest(w http.ResponseWriter, input artifactUploadRequest) bool {
	if !safeArtifactPath(input.Path) {
		writeError(w, http.StatusBadRequest, "invalid_artifact_path", "artifact path must be a clean relative path")
		return false
	}
	if input.SizeBytes < 1 || input.SizeBytes > maxArtifactBytes {
		writeError(w, http.StatusBadRequest, "invalid_artifact_size", "artifact must be non-empty and no larger than 20 GiB")
		return false
	}
	decoded, err := hex.DecodeString(input.SHA256)
	if err != nil || len(decoded) != sha256.Size || input.SHA256 != strings.ToLower(input.SHA256) {
		writeError(w, http.StatusBadRequest, "invalid_artifact_hash", "sha256 must contain a lowercase SHA-256 hex digest")
		return false
	}
	parsed, _, err := mime.ParseMediaType(input.MediaType)
	if err != nil || parsed != input.MediaType {
		writeError(w, http.StatusBadRequest, "invalid_media_type", "media_type must be a normalized MIME type")
		return false
	}
	return true
}

func (api *API) uploadArtifact(w http.ResponseWriter, r *http.Request) {
	if api.blobs == nil {
		writeError(w, http.StatusServiceUnavailable, "blob_store_unavailable", "artifact storage is not configured")
		return
	}
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	sessionID := r.PathValue("session_id")
	artifactPath := r.PathValue("artifact_path")
	if !safeArtifactPath(artifactPath) {
		writeError(w, http.StatusBadRequest, "invalid_artifact_path", "artifact path must be a clean relative path")
		return
	}
	if err := api.store.EnsureArtifactUploadAllowed(r.Context(), sessionID); respondStoreError(w, err) {
		return
	}
	expectedHash := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Artifact-SHA256")))
	if decoded, err := hex.DecodeString(expectedHash); err != nil || len(decoded) != sha256.Size {
		writeError(w, http.StatusBadRequest, "invalid_artifact_hash", "X-Artifact-SHA256 must contain a SHA-256 hex digest")
		return
	}
	if r.ContentLength == 0 || r.ContentLength > maxArtifactBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_artifact_size", "artifact must be non-empty and no larger than 20 GiB")
		return
	}
	mediaType := "application/octet-stream"
	if supplied := r.Header.Get("Content-Type"); supplied != "" {
		parsed, _, err := mime.ParseMediaType(supplied)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_media_type", "Content-Type is invalid")
			return
		}
		mediaType = parsed
	}

	temporary, err := os.CreateTemp("", "trajectory-artifact-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "artifact_staging_failed", "could not create artifact staging file")
		return
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	defer temporary.Close()
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(temporary, hash), http.MaxBytesReader(w, r.Body, maxArtifactBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "artifact_read_failed", err.Error())
		return
	}
	if size < 1 {
		writeError(w, http.StatusBadRequest, "invalid_artifact_size", "artifact must be non-empty")
		return
	}
	actualHash := hex.EncodeToString(hash.Sum(nil))
	if actualHash != expectedHash {
		writeError(w, http.StatusUnprocessableEntity, "artifact_hash_mismatch", "uploaded bytes do not match X-Artifact-SHA256")
		return
	}
	if err := temporary.Sync(); err != nil {
		writeError(w, http.StatusInternalServerError, "artifact_staging_failed", "could not sync staged artifact")
		return
	}
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		writeError(w, http.StatusInternalServerError, "artifact_staging_failed", "could not rewind staged artifact")
		return
	}
	key := "raw/sessions/" + sessionID + "/" + artifactPath
	info, err := api.blobs.Put(r.Context(), key, temporary)
	if errors.Is(err, blobstore.ErrImmutableRaw) {
		info, err = api.blobs.Stat(r.Context(), key)
		if err == nil && (info.SHA256 != expectedHash || info.Size != size) {
			err = database.ErrArtifactConflict
		}
	}
	if err != nil {
		if errors.Is(err, database.ErrArtifactConflict) {
			writeError(w, http.StatusConflict, "artifact_conflict", err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "artifact_store_failed", "artifact could not be committed")
		}
		return
	}
	artifact, err := api.store.RegisterArtifact(r.Context(), domain.Artifact{
		SessionID: sessionID,
		Key:       key,
		SHA256:    info.SHA256,
		Size:      info.Size,
		MediaType: mediaType,
	}, meta.actor, meta.requestID, api.now())
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, artifactResponse(artifact))
}

func (api *API) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	if api.blobs == nil {
		writeError(w, http.StatusServiceUnavailable, "blob_store_unavailable", "artifact storage is not configured")
		return
	}
	sessionID := r.PathValue("session_id")
	artifactPath := r.PathValue("artifact_path")
	if !safeArtifactPath(artifactPath) {
		writeError(w, http.StatusBadRequest, "invalid_artifact_path", "artifact path must be a clean relative path")
		return
	}
	key := "raw/sessions/" + sessionID + "/" + artifactPath
	artifact, err := api.store.GetArtifact(r.Context(), sessionID, key)
	if respondStoreError(w, err) {
		return
	}
	api.serveBlob(w, r, artifact.Key, artifact.MediaType, artifact.Size, artifact.SHA256)
}

func (api *API) downloadNormalizedTrajectory(w http.ResponseWriter, r *http.Request) {
	if api.blobs == nil {
		writeError(w, http.StatusServiceUnavailable, "blob_store_unavailable", "artifact storage is not configured")
		return
	}
	key, err := api.store.GetNormalizedTrajectoryKey(r.Context(), r.PathValue("session_id"))
	if respondStoreError(w, err) {
		return
	}
	info, err := api.blobs.Stat(r.Context(), key)
	if err != nil {
		writeError(w, http.StatusNotFound, "artifact_not_found", "normalized trajectory is unavailable")
		return
	}
	api.serveBlob(w, r, key, "application/json", info.Size, info.SHA256)
}

func (api *API) serveBlob(w http.ResponseWriter, r *http.Request, key, mediaType string, size int64, sha256 string) {
	start, end, partial, err := parseByteRange(r.Header.Get("Range"), size)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "invalid_range", err.Error())
		return
	}
	reader, err := api.blobs.Get(r.Context(), key)
	if err != nil {
		writeError(w, http.StatusNotFound, "artifact_not_found", "artifact bytes are unavailable")
		return
	}
	defer reader.Close()
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", `"`+sha256+`"`)
	w.Header().Set("Cache-Control", "private, immutable, max-age=31536000")
	length := end - start + 1
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.WriteHeader(http.StatusPartialContent)
	}
	if r.Method == http.MethodHead {
		if !partial {
			w.WriteHeader(http.StatusOK)
		}
		return
	}
	if start > 0 {
		if _, err := io.CopyN(io.Discard, reader, start); err != nil {
			return
		}
	}
	_, _ = io.CopyN(w, reader, length)
}

func parseByteRange(value string, size int64) (start int64, end int64, partial bool, err error) {
	if size < 1 {
		return 0, 0, false, errors.New("artifact is empty")
	}
	if value == "" {
		return 0, size - 1, false, nil
	}
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return 0, 0, false, errors.New("only one bytes range is supported")
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 {
		return 0, 0, false, errors.New("invalid bytes range")
	}
	if parts[0] == "" {
		suffix, parseErr := strconv.ParseInt(parts[1], 10, 64)
		if parseErr != nil || suffix < 1 {
			return 0, 0, false, errors.New("invalid bytes suffix range")
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, size - 1, true, nil
	}
	start, parseErr := strconv.ParseInt(parts[0], 10, 64)
	if parseErr != nil || start < 0 || start >= size {
		return 0, 0, false, errors.New("range start is outside the artifact")
	}
	end = size - 1
	if parts[1] != "" {
		end, parseErr = strconv.ParseInt(parts[1], 10, 64)
		if parseErr != nil || end < start {
			return 0, 0, false, errors.New("invalid range end")
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, true, nil
}

func (api *API) submitSession(w http.ResponseWriter, r *http.Request) {
	if api.blobs == nil {
		writeError(w, http.StatusServiceUnavailable, "blob_store_unavailable", "artifact storage is not configured")
		return
	}
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	sessionID := r.PathValue("session_id")
	artifacts, err := api.store.ListArtifacts(r.Context(), sessionID)
	if err != nil {
		if respondStoreError(w, err) {
			return
		}
	}
	for _, artifact := range artifacts {
		info, statErr := api.blobs.Stat(r.Context(), artifact.Key)
		if statErr != nil || info.SHA256 != artifact.SHA256 || info.Size != artifact.Size {
			writeError(w, http.StatusConflict, "artifact_verification_failed", fmt.Sprintf("stored artifact %s is missing or does not match registered metadata", artifact.Key))
			return
		}
	}
	session, job, err := api.store.SubmitSession(r.Context(), sessionID, meta.actor, meta.requestID, api.now())
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"session": sessionResponse(session),
		"job":     processingJobResponse(job),
	})
}

func (api *API) getProcessingJob(w http.ResponseWriter, r *http.Request) {
	job, err := api.store.GetProcessingJob(r.Context(), r.PathValue("job_id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, processingJobResponse(job))
}

func (api *API) listDeadLetterJobs(w http.ResponseWriter, r *http.Request) {
	current, _ := principal(r)
	var organizationIDs []string
	if !current.Local {
		organizationIDs = current.Access.OrganizationIDsForRoles("admin")
	}
	jobs, err := api.store.ListDeadLetterJobs(r.Context(), organizationIDs)
	if respondStoreError(w, err) {
		return
	}
	items := make([]map[string]any, 0, len(jobs))
	for _, job := range jobs {
		items = append(items, processingJobResponse(job))
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": items})
}

func (api *API) retryDeadLetterJob(w http.ResponseWriter, r *http.Request) {
	meta, err := metadata(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", err.Error())
		return
	}
	job, err := api.store.RetryDeadLetterJob(r.Context(), r.PathValue("job_id"), meta.actor, meta.requestID, api.now())
	if errors.Is(err, database.ErrJobNotDeadLetter) {
		writeError(w, http.StatusConflict, "job_not_dead_lettered", err.Error())
		return
	}
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, processingJobResponse(job))
}

func safeArtifactPath(value string) bool {
	return value != "" && !strings.HasPrefix(value, "/") && !strings.Contains(value, "\\") && path.Clean(value) == value && value != "." && !strings.HasPrefix(value, "../")
}

func artifactResponse(artifact domain.Artifact) map[string]any {
	return map[string]any{
		"id": artifact.ID, "session_id": artifact.SessionID, "logical_key": artifact.Key,
		"sha256": artifact.SHA256, "size_bytes": artifact.Size, "media_type": artifact.MediaType,
	}
}

func processingJobResponse(job domain.ProcessingJob) map[string]any {
	response := map[string]any{
		"id": job.ID, "session_id": job.SessionID, "job_type": job.JobType,
		"state": job.State, "attempt": job.Attempt, "available_at": job.AvailableAt,
	}
	if job.LeasedBy != "" {
		response["leased_by"] = job.LeasedBy
		response["lease_expires_at"] = job.LeaseExpiresAt
	}
	if job.LastError != "" {
		response["last_error"] = job.LastError
	}
	if job.ManualRequeues > 0 {
		response["manual_requeues"] = job.ManualRequeues
	}
	if !job.FinishedAt.IsZero() && job.FinishedAt.Unix() > 0 {
		response["finished_at"] = job.FinishedAt
	}
	if !job.DeadLetteredAt.IsZero() && job.DeadLetteredAt.Unix() > 0 {
		response["dead_lettered_at"] = job.DeadLetteredAt
	}
	return response
}
