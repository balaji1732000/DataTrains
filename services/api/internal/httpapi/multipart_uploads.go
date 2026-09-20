package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"trajectory.local/api/internal/blobstore"
	"trajectory.local/api/internal/database"
	"trajectory.local/api/internal/domain"
	"trajectory.local/api/internal/id"
)

const (
	multipartPartSize int64 = 16 << 20
	multipartStateTTL       = 7 * 24 * time.Hour
)

var multipartETagPattern = regexp.MustCompile(`^"[A-Za-z0-9_-]{1,128}"$`)

type multipartPartRequest struct {
	SizeBytes int64  `json:"size_bytes"`
	ETag      string `json:"etag"`
}

func (api *API) startMultipartArtifactUpload(w http.ResponseWriter, r *http.Request) {
	authorizer, ok := api.blobs.(blobstore.MultipartUploadAuthorizer)
	if !ok {
		writeError(w, http.StatusNotImplemented, "multipart_upload_unavailable", "resumable direct upload is not configured for this environment")
		return
	}
	var input artifactUploadRequest
	if !decodeRequest(w, r, &input) || !validateArtifactUploadRequest(w, input) {
		return
	}
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	sessionID := r.PathValue("session_id")
	if err := api.store.EnsureArtifactUploadAllowed(r.Context(), sessionID); respondStoreError(w, err) {
		return
	}
	key := "raw/sessions/" + sessionID + "/" + input.Path
	if artifact, err := api.store.GetArtifact(r.Context(), sessionID, key); err == nil {
		if artifact.SHA256 != input.SHA256 || artifact.Size != input.SizeBytes || artifact.MediaType != input.MediaType {
			writeError(w, http.StatusConflict, "artifact_conflict", "an artifact already exists at this path with different metadata")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"state": "completed", "artifact": artifactResponse(artifact)})
		return
	} else if !errors.Is(err, database.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "upload_lookup_failed", "artifact upload state could not be read")
		return
	}

	now := api.now().UTC()
	existing, err := api.store.FindMultipartUpload(r.Context(), sessionID, key)
	if err == nil {
		matches := existing.SHA256 == input.SHA256 && existing.Size == input.SizeBytes && existing.MediaType == input.MediaType
		if existing.State == "active" && existing.ExpiresAt.After(now) {
			if !matches {
				writeError(w, http.StatusConflict, "upload_conflict", "an active upload already exists at this path with different metadata")
				return
			}
			api.writeMultipartUpload(r.Context(), w, http.StatusOK, existing)
			return
		}
		if existing.State == "completed" {
			writeError(w, http.StatusConflict, "upload_already_completed", "this upload completed but its artifact registration is unavailable")
			return
		}
		if existing.State == "active" {
			_ = authorizer.AbortMultipartUpload(r.Context(), existing.StorageKey, existing.StorageUploadID)
			if err := api.store.SetMultipartUploadState(r.Context(), existing.ID, "active", "aborted", now); err != nil {
				writeError(w, http.StatusConflict, "upload_restart_failed", "the expired upload could not be restarted")
				return
			}
			existing.State = "aborted"
		}
		stageID, generateErr := id.New("stage")
		if generateErr != nil {
			writeError(w, http.StatusInternalServerError, "upload_start_failed", "staging identity could not be generated")
			return
		}
		existing.StorageKey = "staging/multipart/" + stageID
		existing.SHA256, existing.Size, existing.MediaType = input.SHA256, input.SizeBytes, input.MediaType
		existing.PartSize, existing.ExpiresAt = multipartPartSize, now.Add(multipartStateTTL)
		existing.StorageUploadID, err = authorizer.CreateMultipartUpload(r.Context(), existing.StorageKey, input.MediaType, input.SHA256)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "upload_start_failed", "resumable upload could not be created")
			return
		}
		restartedStorageKey, restartedStorageUploadID := existing.StorageKey, existing.StorageUploadID
		existing, err = api.store.RestartMultipartUpload(r.Context(), existing, meta.actor, meta.requestID, now)
		if err != nil {
			_ = authorizer.AbortMultipartUpload(r.Context(), restartedStorageKey, restartedStorageUploadID)
			writeError(w, http.StatusConflict, "upload_restart_failed", "resumable upload state changed while it was restarting")
			return
		}
		api.writeMultipartUpload(r.Context(), w, http.StatusCreated, existing)
		return
	}
	if !errors.Is(err, database.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "upload_lookup_failed", "artifact upload state could not be read")
		return
	}

	stageID, err := id.New("stage")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "upload_start_failed", "staging identity could not be generated")
		return
	}
	upload := database.MultipartUpload{
		SessionID: sessionID, Key: key, StorageKey: "staging/multipart/" + stageID,
		SHA256: input.SHA256, Size: input.SizeBytes, MediaType: input.MediaType,
		PartSize: multipartPartSize, ExpiresAt: now.Add(multipartStateTTL),
	}
	upload.StorageUploadID, err = authorizer.CreateMultipartUpload(r.Context(), upload.StorageKey, upload.MediaType, upload.SHA256)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "upload_start_failed", "resumable upload could not be created")
		return
	}
	newStorageKey, newStorageUploadID := upload.StorageKey, upload.StorageUploadID
	upload, created, err := api.store.CreateMultipartUpload(r.Context(), upload, meta.actor, meta.requestID, now)
	if err != nil {
		_ = authorizer.AbortMultipartUpload(r.Context(), newStorageKey, newStorageUploadID)
		writeError(w, http.StatusInternalServerError, "upload_start_failed", "resumable upload state could not be saved")
		return
	}
	if !created {
		_ = authorizer.AbortMultipartUpload(r.Context(), newStorageKey, newStorageUploadID)
		if upload.SHA256 != input.SHA256 || upload.Size != input.SizeBytes || upload.MediaType != input.MediaType || upload.State != "active" {
			writeError(w, http.StatusConflict, "upload_conflict", "upload state changed while the upload was starting")
			return
		}
		api.writeMultipartUpload(r.Context(), w, http.StatusOK, upload)
		return
	}
	api.writeMultipartUpload(r.Context(), w, http.StatusCreated, upload)
}

func (api *API) authorizeMultipartArtifactPart(w http.ResponseWriter, r *http.Request) {
	authorizer, ok := api.blobs.(blobstore.MultipartUploadAuthorizer)
	if !ok {
		writeError(w, http.StatusNotImplemented, "multipart_upload_unavailable", "resumable direct upload is not configured for this environment")
		return
	}
	var input multipartPartRequest
	if !decodeRequest(w, r, &input) {
		return
	}
	upload, partNumber, expected, ok := api.validMultipartPart(w, r, input.SizeBytes)
	if !ok {
		return
	}
	authorization, err := authorizer.AuthorizeMultipartPart(r.Context(), upload.StorageKey, upload.StorageUploadID, partNumber, expected, api.uploadTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "part_authorization_failed", "multipart part could not be authorized")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"part_number": partNumber, "size_bytes": expected, "url": authorization.URL,
		"method": authorization.Method, "headers": authorization.Headers, "expires_at": authorization.ExpiresAt,
	})
}

func (api *API) completeMultipartArtifactPart(w http.ResponseWriter, r *http.Request) {
	var input multipartPartRequest
	if !decodeRequest(w, r, &input) {
		return
	}
	upload, partNumber, expected, ok := api.validMultipartPart(w, r, input.SizeBytes)
	if !ok {
		return
	}
	if !multipartETagPattern.MatchString(input.ETag) {
		writeError(w, http.StatusBadRequest, "invalid_part_etag", "etag must be a quoted storage entity tag")
		return
	}
	part := database.MultipartUploadPart{Number: partNumber, ETag: input.ETag, Size: expected}
	if err := api.store.RecordMultipartUploadPart(r.Context(), upload.ID, part, api.now()); respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, part)
}

func (api *API) completeMultipartArtifactUpload(w http.ResponseWriter, r *http.Request) {
	authorizer, ok := api.blobs.(blobstore.MultipartUploadAuthorizer)
	if !ok {
		writeError(w, http.StatusNotImplemented, "multipart_upload_unavailable", "resumable direct upload is not configured for this environment")
		return
	}
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	sessionID := r.PathValue("session_id")
	if err := api.store.EnsureArtifactUploadAllowed(r.Context(), sessionID); respondStoreError(w, err) {
		return
	}
	upload, err := api.store.GetMultipartUpload(r.Context(), sessionID, r.PathValue("upload_id"))
	if respondStoreError(w, err) {
		return
	}
	if upload.State != "active" || !upload.ExpiresAt.After(api.now()) {
		writeError(w, http.StatusConflict, "upload_not_active", "multipart upload is not active")
		return
	}
	parts, err := api.store.ListMultipartUploadParts(r.Context(), upload.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "upload_parts_unavailable", "multipart upload parts could not be read")
		return
	}
	totalParts := multipartPartCount(upload.Size, upload.PartSize)
	if len(parts) != totalParts {
		writeError(w, http.StatusConflict, "upload_incomplete", fmt.Sprintf("multipart upload has %d of %d required parts", len(parts), totalParts))
		return
	}
	completed := make([]blobstore.MultipartPart, len(parts))
	for index, part := range parts {
		expected := multipartExpectedPartSize(upload.Size, upload.PartSize, int32(index+1))
		if part.Number != int32(index+1) || part.Size != expected {
			writeError(w, http.StatusConflict, "upload_incomplete", "multipart upload parts are incomplete or inconsistent")
			return
		}
		completed[index] = blobstore.MultipartPart{Number: part.Number, ETag: part.ETag}
	}
	info, rawErr := api.blobs.Stat(r.Context(), upload.Key)
	rawReady := rawErr == nil && info.Size == upload.Size && info.SHA256 == upload.SHA256
	if rawErr == nil && !rawReady {
		writeError(w, http.StatusConflict, "artifact_conflict", "immutable storage already contains different bytes at this artifact path")
		return
	}
	if !rawReady {
		staged, stagedErr := api.blobs.Stat(r.Context(), upload.StorageKey)
		stagedReady := stagedErr == nil && staged.Size == upload.Size && staged.SHA256 == upload.SHA256
		if stagedErr == nil && !stagedReady {
			_ = api.blobs.Delete(r.Context(), upload.StorageKey)
			_ = api.store.SetMultipartUploadState(r.Context(), upload.ID, "active", "aborted", api.now())
			writeError(w, http.StatusUnprocessableEntity, "uploaded_artifact_mismatch", "uploaded bytes do not match the declared size and SHA-256")
			return
		}
		if !stagedReady {
			if err := authorizer.CompleteMultipartUpload(r.Context(), upload.StorageKey, upload.StorageUploadID, completed); err != nil {
				writeError(w, http.StatusUnprocessableEntity, "upload_completion_failed", "storage rejected the multipart completion")
				return
			}
			staged, stagedErr = api.blobs.Stat(r.Context(), upload.StorageKey)
			if stagedErr != nil || staged.Size != upload.Size || staged.SHA256 != upload.SHA256 {
				_ = api.blobs.Delete(r.Context(), upload.StorageKey)
				_ = api.store.SetMultipartUploadState(r.Context(), upload.ID, "active", "aborted", api.now())
				writeError(w, http.StatusUnprocessableEntity, "uploaded_artifact_mismatch", "uploaded bytes do not match the declared size and SHA-256")
				return
			}
		}
		if err := authorizer.PromoteMultipartUpload(r.Context(), upload.StorageKey, upload.Key); err != nil && !errors.Is(err, blobstore.ErrImmutableRaw) {
			writeError(w, http.StatusInternalServerError, "upload_promotion_failed", "verified artifact could not be committed to immutable storage")
			return
		}
	}
	info, err = api.blobs.Stat(r.Context(), upload.Key)
	if err != nil || info.Size != upload.Size || info.SHA256 != upload.SHA256 {
		writeError(w, http.StatusUnprocessableEntity, "uploaded_artifact_mismatch", "committed artifact does not match the declared size and SHA-256")
		return
	}
	_ = api.blobs.Delete(r.Context(), upload.StorageKey)
	artifact, err := api.store.RegisterArtifact(r.Context(), domain.Artifact{
		SessionID: sessionID, Key: upload.Key, SHA256: info.SHA256, Size: info.Size, MediaType: upload.MediaType,
	}, meta.actor, meta.requestID, api.now())
	if respondStoreError(w, err) {
		return
	}
	if err := api.store.SetMultipartUploadState(r.Context(), upload.ID, "active", "completed", api.now()); err != nil {
		writeError(w, http.StatusInternalServerError, "upload_state_failed", "artifact was committed but upload state could not be finalized")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"state": "completed", "artifact": artifactResponse(artifact)})
}

func (api *API) abortMultipartArtifactUpload(w http.ResponseWriter, r *http.Request) {
	authorizer, ok := api.blobs.(blobstore.MultipartUploadAuthorizer)
	if !ok {
		writeError(w, http.StatusNotImplemented, "multipart_upload_unavailable", "resumable direct upload is not configured for this environment")
		return
	}
	sessionID := r.PathValue("session_id")
	if err := api.store.EnsureArtifactUploadAllowed(r.Context(), sessionID); respondStoreError(w, err) {
		return
	}
	upload, err := api.store.GetMultipartUpload(r.Context(), sessionID, r.PathValue("upload_id"))
	if respondStoreError(w, err) {
		return
	}
	if upload.State != "active" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := authorizer.AbortMultipartUpload(r.Context(), upload.StorageKey, upload.StorageUploadID); err != nil {
		writeError(w, http.StatusInternalServerError, "upload_abort_failed", "multipart upload could not be aborted")
		return
	}
	if err := api.store.SetMultipartUploadState(r.Context(), upload.ID, "active", "aborted", api.now()); respondStoreError(w, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *API) validMultipartPart(w http.ResponseWriter, r *http.Request, suppliedSize int64) (database.MultipartUpload, int32, int64, bool) {
	sessionID := r.PathValue("session_id")
	if err := api.store.EnsureArtifactUploadAllowed(r.Context(), sessionID); respondStoreError(w, err) {
		return database.MultipartUpload{}, 0, 0, false
	}
	upload, err := api.store.GetMultipartUpload(r.Context(), sessionID, r.PathValue("upload_id"))
	if respondStoreError(w, err) {
		return database.MultipartUpload{}, 0, 0, false
	}
	if upload.State != "active" || !upload.ExpiresAt.After(api.now()) {
		writeError(w, http.StatusConflict, "upload_not_active", "multipart upload is not active")
		return database.MultipartUpload{}, 0, 0, false
	}
	part64, err := parsePositiveInt32(r.PathValue("part_number"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_part_number", "part number must be a positive integer")
		return database.MultipartUpload{}, 0, 0, false
	}
	partNumber := int32(part64)
	expected := multipartExpectedPartSize(upload.Size, upload.PartSize, partNumber)
	if expected < 1 || suppliedSize != expected {
		writeError(w, http.StatusBadRequest, "invalid_part_size", "part size does not match this upload's part plan")
		return database.MultipartUpload{}, 0, 0, false
	}
	return upload, partNumber, expected, true
}

func (api *API) writeMultipartUpload(ctx context.Context, w http.ResponseWriter, status int, upload database.MultipartUpload) {
	parts, err := api.store.ListMultipartUploadParts(ctx, upload.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "upload_parts_unavailable", "multipart upload parts could not be read")
		return
	}
	writeJSON(w, status, map[string]any{
		"upload_id": upload.ID, "state": upload.State, "part_size_bytes": upload.PartSize,
		"total_parts": multipartPartCount(upload.Size, upload.PartSize), "expires_at": upload.ExpiresAt,
		"uploaded_parts": parts,
	})
}

func multipartPartCount(size, partSize int64) int {
	if size < 1 || partSize < 1 {
		return 0
	}
	return int((size + partSize - 1) / partSize)
}

func multipartExpectedPartSize(size, partSize int64, partNumber int32) int64 {
	total := multipartPartCount(size, partSize)
	if partNumber < 1 || int(partNumber) > total {
		return 0
	}
	if int(partNumber) < total {
		return partSize
	}
	return size - int64(total-1)*partSize
}

func parsePositiveInt32(value string) (int64, error) {
	var parsed int64
	_, err := fmt.Sscan(value, &parsed)
	if err != nil || parsed < 1 || parsed > 10_000 || fmt.Sprint(parsed) != value {
		return 0, errors.New("invalid positive integer")
	}
	return parsed, nil
}
