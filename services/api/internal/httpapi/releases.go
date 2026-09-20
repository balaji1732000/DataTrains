package httpapi

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"trajectory.local/api/internal/database"
	"trajectory.local/api/internal/domain"
	"trajectory.local/api/internal/releases"
)

func (api *API) createRelease(w http.ResponseWriter, r *http.Request) {
	if api.exporter == nil {
		writeError(w, http.StatusServiceUnavailable, "release_exporter_unavailable", "release storage is not configured")
		return
	}
	var input struct {
		Name       string   `json:"name"`
		Profile    string   `json:"profile"`
		SessionIDs []string `json:"session_ids"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	release, manifest, err := api.exporter.CreateWithProfile(r.Context(), r.PathValue("project_id"), input.Name, input.Profile, input.SessionIDs, meta.actor, meta.requestID, api.now())
	if err != nil {
		api.logger.Error("release_creation_failed", "project_id", r.PathValue("project_id"), "profile", input.Profile, "error", err)
	}
	if errors.Is(err, database.ErrReleaseEligibility) {
		writeError(w, http.StatusUnprocessableEntity, "release_ineligible", err.Error())
		return
	}
	if errors.Is(err, releases.ErrInvalidReleaseProfile) {
		writeError(w, http.StatusBadRequest, "invalid_release_profile", err.Error())
		return
	}
	if errors.Is(err, releases.ErrRedactedVideoUnavailable) {
		writeError(w, http.StatusUnprocessableEntity, "redacted_video_unavailable", err.Error())
		return
	}
	if errors.Is(err, database.ErrReleaseConflict) {
		writeError(w, http.StatusConflict, "release_conflict", err.Error())
		return
	}
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"release": releaseResponse(release), "manifest": manifest})
}

func (api *API) getRelease(w http.ResponseWriter, r *http.Request) {
	release, err := api.store.GetRelease(r.Context(), r.PathValue("release_id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, releaseResponse(release))
}

func (api *API) downloadReleaseFile(w http.ResponseWriter, r *http.Request) {
	if api.blobs == nil {
		writeError(w, http.StatusServiceUnavailable, "blob_store_unavailable", "release storage is not configured")
		return
	}
	releaseID := r.PathValue("release_id")
	release, err := api.store.GetRelease(r.Context(), releaseID)
	if respondStoreError(w, err) {
		return
	}
	if release.PurgedAt.Year() > 1970 {
		writeError(w, http.StatusGone, "release_expired", "release files were removed by the retention policy")
		return
	}
	filePath := r.PathValue("file_path")
	if !safeArtifactPath(filePath) {
		writeError(w, http.StatusBadRequest, "invalid_release_path", "release file path must be a clean relative path")
		return
	}
	key := "releases/" + releaseID + "/" + filePath
	info, err := api.blobs.Stat(r.Context(), key)
	if err != nil {
		writeError(w, http.StatusNotFound, "release_file_not_found", "release file is unavailable")
		return
	}
	mediaType := "application/octet-stream"
	switch strings.ToLower(filepath.Ext(filePath)) {
	case ".json":
		mediaType = "application/json"
	case ".jsonl":
		mediaType = "application/x-ndjson"
	}
	api.serveBlob(w, r, key, mediaType, info.Size, info.SHA256)
}

func (api *API) downloadReleaseBundle(w http.ResponseWriter, r *http.Request) {
	if api.blobs == nil {
		writeError(w, http.StatusServiceUnavailable, "blob_store_unavailable", "release storage is not configured")
		return
	}
	release, err := api.store.GetRelease(r.Context(), r.PathValue("release_id"))
	if respondStoreError(w, err) {
		return
	}
	if release.PurgedAt.Year() > 1970 {
		writeError(w, http.StatusGone, "release_expired", "release bundle was removed by the retention policy")
		return
	}
	if release.BundleHash == "" || release.BundleSize < 1 {
		writeError(w, http.StatusNotFound, "release_bundle_not_found", "release bundle is unavailable")
		return
	}
	api.serveBlob(w, r, "bundles/"+release.ID+".zip", "application/zip", release.BundleSize, release.BundleHash)
}

func releaseResponse(release domain.DatasetRelease) map[string]any {
	return map[string]any{
		"id": release.ID, "project_id": release.ProjectID, "name": release.Name,
		"profile":        release.Profile,
		"schema_version": release.SchemaVersion, "exporter_version": release.ExporterVersion,
		"pipeline_version": release.PipelineVersion, "source_session_ids": release.SourceSessionIDs,
		"configuration_hash": release.ConfigurationHash, "manifest_hash": release.ManifestHash,
		"bundle_hash": release.BundleHash, "bundle_size": release.BundleSize,
		"purged_at":    nullableTime(release.PurgedAt),
		"created_at":   release.CreatedAt,
		"manifest_url": "/v1/releases/" + release.ID + "/files/manifest.json",
		"bundle_url":   "/v1/releases/" + release.ID + "/bundle",
	}
}

func nullableTime(value time.Time) any {
	if value.Year() <= 1970 {
		return nil
	}
	return value
}
