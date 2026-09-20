package releases

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"trajectory.local/api/internal/blobstore"
	"trajectory.local/api/internal/database"
	"trajectory.local/api/internal/domain"
)

const (
	SchemaVersion               = "release/v1"
	ExporterVersion             = "trajectory-exporter/0.3.0"
	PipelineVersion             = "trajectory-pipeline/0.1.0"
	ProfileTrajectoryOnly       = "trajectory_only"
	ProfileRedactedVideo        = "redacted_video"
	maxRedactionManifest  int64 = 4 << 20
)

var (
	ErrInvalidReleaseProfile    = errors.New("release profile must be trajectory_only or redacted_video")
	ErrRedactedVideoUnavailable = errors.New("redacted-video release requires a completed, integrity-checked redaction for every session")
)

type FileEntry struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"media_type"`
}

type Manifest struct {
	SchemaVersion     string               `json:"schema_version"`
	ReleaseID         string               `json:"release_id"`
	ProjectID         string               `json:"project_id"`
	Name              string               `json:"name"`
	ReleaseProfile    string               `json:"release_profile"`
	TrajectorySchema  string               `json:"trajectory_schema"`
	ExporterVersion   string               `json:"exporter_version"`
	PipelineVersion   string               `json:"pipeline_version"`
	ConfigurationHash string               `json:"configuration_hash"`
	SourceSessionIDs  []string             `json:"source_session_ids"`
	RedactionPlans    []RedactionReference `json:"redaction_plans"`
	Files             []FileEntry          `json:"files"`
}

type RedactionReference struct {
	SessionID string `json:"session_id"`
	PlanID    string `json:"plan_id"`
	Version   int    `json:"version"`
	PlanHash  string `json:"plan_hash"`
}

type redactionOutput struct {
	SegmentID      string  `json:"segment_id"`
	SourceKey      string  `json:"source_key"`
	Decision       string  `json:"decision"`
	OutputKey      string  `json:"output_key"`
	SHA256         string  `json:"sha256"`
	Size           int64   `json:"size"`
	Width          int     `json:"width"`
	Height         int     `json:"height"`
	DurationSecond float64 `json:"duration_seconds"`
}

type redactionOutputManifest struct {
	SchemaVersion string            `json:"schema_version"`
	SessionID     string            `json:"session_id"`
	PlanID        string            `json:"plan_id"`
	PlanVersion   int               `json:"plan_version"`
	PlanHash      string            `json:"plan_hash"`
	Outputs       []redactionOutput `json:"outputs"`
}

type releaseRedaction struct {
	Reference RedactionReference
	Outputs   []redactionOutput
}

type Exporter struct {
	store            *database.Store
	blobs            blobstore.BlobStore
	trajectorySchema []byte
}

func NewExporter(store *database.Store, blobs blobstore.BlobStore, trajectorySchema []byte) (*Exporter, error) {
	if store == nil || blobs == nil || !json.Valid(trajectorySchema) {
		return nil, errors.New("database, blob store, and valid trajectory schema are required")
	}
	return &Exporter{store: store, blobs: blobs, trajectorySchema: append([]byte(nil), trajectorySchema...)}, nil
}

func (exporter *Exporter) Create(ctx context.Context, projectID, name string, requestedSessionIDs []string, actor, requestID string, now time.Time) (domain.DatasetRelease, Manifest, error) {
	return exporter.CreateWithProfile(ctx, projectID, name, ProfileTrajectoryOnly, requestedSessionIDs, actor, requestID, now)
}

func (exporter *Exporter) CreateWithProfile(ctx context.Context, projectID, name, profile string, requestedSessionIDs []string, actor, requestID string, now time.Time) (domain.DatasetRelease, Manifest, error) {
	if projectID == "" || strings.TrimSpace(name) == "" || actor == "" || requestID == "" {
		return domain.DatasetRelease{}, Manifest{}, errors.New("project, release name, actor, and request ID are required")
	}
	if profile == "" {
		profile = ProfileTrajectoryOnly
	}
	if profile != ProfileTrajectoryOnly && profile != ProfileRedactedVideo {
		return domain.DatasetRelease{}, Manifest{}, ErrInvalidReleaseProfile
	}
	name = strings.TrimSpace(name)
	sessionIDs, err := normalizedIDs(requestedSessionIDs)
	if err != nil {
		return domain.DatasetRelease{}, Manifest{}, err
	}
	if existing, lookupErr := exporter.store.GetReleaseByName(ctx, projectID, name); lookupErr == nil {
		if existing.Profile != profile || (len(sessionIDs) > 0 && !sameIDs(sessionIDs, existing.SourceSessionIDs)) {
			return domain.DatasetRelease{}, Manifest{}, database.ErrReleaseConflict
		}
		reader, readErr := exporter.blobs.Get(ctx, "releases/"+existing.ID+"/manifest.json")
		if readErr != nil {
			return domain.DatasetRelease{}, Manifest{}, readErr
		}
		defer reader.Close()
		var manifest Manifest
		if decodeErr := json.NewDecoder(reader).Decode(&manifest); decodeErr != nil {
			return domain.DatasetRelease{}, Manifest{}, decodeErr
		}
		info, statErr := exporter.blobs.Stat(ctx, "releases/"+existing.ID+"/manifest.json")
		if statErr != nil || info.SHA256 != existing.ManifestHash {
			return domain.DatasetRelease{}, Manifest{}, errors.New("existing release manifest does not match committed metadata")
		}
		bundle, bundleErr := exporter.blobs.Stat(ctx, "bundles/"+existing.ID+".zip")
		if bundleErr != nil || bundle.SHA256 != existing.BundleHash || bundle.Size != existing.BundleSize {
			return domain.DatasetRelease{}, Manifest{}, errors.New("existing release bundle does not match committed metadata")
		}
		return existing, manifest, nil
	} else if !errors.Is(lookupErr, database.ErrNotFound) {
		return domain.DatasetRelease{}, Manifest{}, lookupErr
	}
	candidates, err := exporter.store.ListReleaseCandidates(ctx, projectID, sessionIDs)
	if err != nil {
		return domain.DatasetRelease{}, Manifest{}, err
	}
	sessionIDs = make([]string, len(candidates))
	for index, candidate := range candidates {
		sessionIDs[index] = candidate.SessionID
	}
	redactions := make(map[string]releaseRedaction, len(candidates))
	redactionReferences := make([]RedactionReference, 0)
	if profile == ProfileRedactedVideo {
		for _, candidate := range candidates {
			redaction, err := exporter.loadRedaction(ctx, candidate.SessionID)
			if err != nil {
				return domain.DatasetRelease{}, Manifest{}, fmt.Errorf("%w: session %s: %v", ErrRedactedVideoUnavailable, candidate.SessionID, err)
			}
			redactions[candidate.SessionID] = redaction
			redactionReferences = append(redactionReferences, redaction.Reference)
		}
	}
	configuration := struct {
		ProjectID        string               `json:"project_id"`
		Name             string               `json:"name"`
		ReleaseProfile   string               `json:"release_profile"`
		TrajectorySchema string               `json:"trajectory_schema"`
		ExporterVersion  string               `json:"exporter_version"`
		PipelineVersion  string               `json:"pipeline_version"`
		SourceSessionIDs []string             `json:"source_session_ids"`
		RedactionPlans   []RedactionReference `json:"redaction_plans"`
	}{projectID, name, profile, "trajectory/v1", ExporterVersion, PipelineVersion, sessionIDs, redactionReferences}
	configurationBytes, err := json.Marshal(configuration)
	if err != nil {
		return domain.DatasetRelease{}, Manifest{}, err
	}
	configurationDigest := sha256.Sum256(configurationBytes)
	configurationHash := hex.EncodeToString(configurationDigest[:])
	releaseID := "rel_" + configurationHash[:32]
	prefix := "releases/" + releaseID + "/"
	files := make([]FileEntry, 0, len(candidates)+4)
	readme := []byte(fmt.Sprintf("# %s\n\nRelease ID: `%s`\n\nRelease profile: `%s`\n\nTrajectory schema: `trajectory/v1`\n\nTrajectories: %d\n", name, releaseID, profile, len(candidates)))
	if profile == ProfileTrajectoryOnly {
		readme = append(readme, []byte("\nRaw recordings and output binaries are private review evidence and are not included in this release.\n")...)
	} else {
		readme = append(readme, []byte("\nOnly reviewer-approved derived video is included; raw recordings remain private review evidence.\n")...)
	}
	readmeEntry, err := putReleaseFile(ctx, exporter.blobs, prefix, "README.md", readme, "text/markdown")
	if err != nil {
		return domain.DatasetRelease{}, Manifest{}, err
	}
	files = append(files, readmeEntry)
	schema := append([]byte(nil), exporter.trajectorySchema...)
	if len(schema) == 0 || schema[len(schema)-1] != '\n' {
		schema = append(schema, '\n')
	}
	schemaEntry, err := putReleaseFile(ctx, exporter.blobs, prefix, "schema.json", schema, "application/schema+json")
	if err != nil {
		return domain.DatasetRelease{}, Manifest{}, err
	}
	files = append(files, schemaEntry)
	var jsonLines bytes.Buffer
	for _, candidate := range candidates {
		trajectory, err := exporter.reviewedTrajectory(ctx, candidate)
		if err != nil {
			return domain.DatasetRelease{}, Manifest{}, err
		}
		relativePath := "trajectories/" + candidate.SessionID + ".json"
		info, err := exporter.blobs.Put(ctx, prefix+relativePath, bytes.NewReader(trajectory))
		if err != nil {
			return domain.DatasetRelease{}, Manifest{}, fmt.Errorf("write release trajectory %s: %w", candidate.SessionID, err)
		}
		files = append(files, FileEntry{Path: relativePath, Size: info.Size, SHA256: info.SHA256, MediaType: "application/json"})
		jsonLines.Write(bytes.TrimSpace(trajectory))
		jsonLines.WriteByte('\n')
		if profile == ProfileRedactedVideo {
			for _, video := range redactions[candidate.SessionID].Outputs {
				reader, err := exporter.blobs.Get(ctx, video.OutputKey)
				if err != nil {
					return domain.DatasetRelease{}, Manifest{}, fmt.Errorf("read redacted video %s: %w", video.OutputKey, err)
				}
				relativePath := "videos/" + candidate.SessionID + "/" + video.SegmentID + ".mp4"
				info, putErr := exporter.blobs.Put(ctx, prefix+relativePath, reader)
				closeErr := reader.Close()
				if putErr != nil {
					return domain.DatasetRelease{}, Manifest{}, fmt.Errorf("write release video %s: %w", relativePath, putErr)
				}
				if closeErr != nil {
					return domain.DatasetRelease{}, Manifest{}, closeErr
				}
				if info.SHA256 != video.SHA256 || info.Size != video.Size {
					return domain.DatasetRelease{}, Manifest{}, fmt.Errorf("redacted video %s changed while publishing", video.OutputKey)
				}
				files = append(files, FileEntry{Path: relativePath, Size: info.Size, SHA256: info.SHA256, MediaType: "video/mp4"})
			}
		}
	}
	jsonlInfo, err := exporter.blobs.Put(ctx, prefix+"trajectories.jsonl", bytes.NewReader(jsonLines.Bytes()))
	if err != nil {
		return domain.DatasetRelease{}, Manifest{}, fmt.Errorf("write release JSONL: %w", err)
	}
	files = append(files, FileEntry{Path: "trajectories.jsonl", Size: jsonlInfo.Size, SHA256: jsonlInfo.SHA256, MediaType: "application/x-ndjson"})
	sort.Slice(files, func(left, right int) bool { return files[left].Path < files[right].Path })
	var checksums bytes.Buffer
	for _, file := range files {
		fmt.Fprintf(&checksums, "%s  %s\n", file.SHA256, file.Path)
	}
	checksumEntry, err := putReleaseFile(ctx, exporter.blobs, prefix, "checksums.sha256", checksums.Bytes(), "text/plain")
	if err != nil {
		return domain.DatasetRelease{}, Manifest{}, err
	}
	files = append(files, checksumEntry)
	sort.Slice(files, func(left, right int) bool { return files[left].Path < files[right].Path })
	manifest := Manifest{
		SchemaVersion: SchemaVersion, ReleaseID: releaseID, ProjectID: projectID, Name: name,
		ReleaseProfile:   profile,
		TrajectorySchema: "trajectory/v1", ExporterVersion: ExporterVersion, PipelineVersion: PipelineVersion,
		ConfigurationHash: configurationHash, SourceSessionIDs: sessionIDs, RedactionPlans: redactionReferences, Files: files,
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return domain.DatasetRelease{}, Manifest{}, err
	}
	manifestBytes = append(manifestBytes, '\n')
	manifestInfo, err := exporter.blobs.Put(ctx, prefix+"manifest.json", bytes.NewReader(manifestBytes))
	if err != nil {
		return domain.DatasetRelease{}, Manifest{}, fmt.Errorf("write release manifest: %w", err)
	}
	bundleInfo, err := streamReleaseBundle(ctx, exporter.blobs, prefix, "bundles/"+releaseID+".zip", append(files, FileEntry{
		Path: "manifest.json", Size: manifestInfo.Size, SHA256: manifestInfo.SHA256, MediaType: "application/json",
	}))
	if err != nil {
		return domain.DatasetRelease{}, Manifest{}, err
	}
	release := domain.DatasetRelease{
		ID: releaseID, ProjectID: projectID, Name: name, Profile: profile, SchemaVersion: "trajectory/v1",
		ExporterVersion: ExporterVersion, PipelineVersion: PipelineVersion,
		SourceSessionIDs: sessionIDs, ConfigurationHash: configurationHash, ManifestHash: manifestInfo.SHA256,
		BundleHash: bundleInfo.SHA256, BundleSize: bundleInfo.Size,
	}
	release.ObjectKeys = make([]string, 0, len(files)+2)
	for _, file := range files {
		release.ObjectKeys = append(release.ObjectKeys, prefix+file.Path)
	}
	release.ObjectKeys = append(release.ObjectKeys, prefix+"manifest.json", "bundles/"+releaseID+".zip")
	sort.Strings(release.ObjectKeys)
	release, err = exporter.store.CommitRelease(ctx, release, actor, requestID, now)
	if err != nil {
		return domain.DatasetRelease{}, Manifest{}, err
	}
	return release, manifest, nil
}

func (exporter *Exporter) loadRedaction(ctx context.Context, sessionID string) (releaseRedaction, error) {
	plan, job, err := exporter.store.LatestCompletedRedaction(ctx, sessionID)
	if err != nil {
		return releaseRedaction{}, err
	}
	reader, err := exporter.blobs.Get(ctx, job.OutputManifestKey)
	if err != nil {
		return releaseRedaction{}, err
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, maxRedactionManifest+1))
	if err != nil {
		return releaseRedaction{}, err
	}
	if int64(len(raw)) > maxRedactionManifest {
		return releaseRedaction{}, errors.New("redaction manifest exceeds 4 MiB")
	}
	var manifest redactionOutputManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return releaseRedaction{}, fmt.Errorf("decode redaction manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return releaseRedaction{}, errors.New("redaction manifest contains trailing content")
	}
	if manifest.SchemaVersion != "redaction-output/v1" || manifest.SessionID != sessionID ||
		manifest.PlanID != plan.ID || manifest.PlanVersion != plan.Version || manifest.PlanHash != plan.DocumentHash ||
		len(manifest.Outputs) != len(plan.Document.Segments) {
		return releaseRedaction{}, errors.New("redaction manifest does not match its approved plan")
	}
	expectedPrefix := "derived/sessions/" + sessionID + "/redactions/" + plan.ID + "/"
	approved := make(map[string]domain.RedactionSegment, len(plan.Document.Segments))
	for _, segment := range plan.Document.Segments {
		approved[segment.SegmentID] = segment
	}
	seen := make(map[string]bool, len(manifest.Outputs))
	for _, output := range manifest.Outputs {
		approvedSegment, exists := approved[output.SegmentID]
		decodedHash, hashErr := hex.DecodeString(output.SHA256)
		if output.SegmentID == "" || seen[output.SegmentID] || !exists || output.SourceKey != approvedSegment.SourceKey || output.Decision != approvedSegment.Decision ||
			output.Size < 1 || output.Width < 1 || output.Height < 1 || output.DurationSecond <= 0 || hashErr != nil || len(decodedHash) != sha256.Size || hex.EncodeToString(decodedHash) != output.SHA256 ||
			output.OutputKey != expectedPrefix+output.SegmentID+".mp4" {
			return releaseRedaction{}, errors.New("redaction manifest contains an invalid or duplicate output")
		}
		seen[output.SegmentID] = true
		info, err := exporter.blobs.Stat(ctx, output.OutputKey)
		if err != nil || info.SHA256 != output.SHA256 || info.Size != output.Size {
			return releaseRedaction{}, fmt.Errorf("redaction output integrity mismatch: %s", output.OutputKey)
		}
	}
	sort.Slice(manifest.Outputs, func(left, right int) bool {
		return manifest.Outputs[left].SegmentID < manifest.Outputs[right].SegmentID
	})
	return releaseRedaction{
		Reference: RedactionReference{SessionID: sessionID, PlanID: plan.ID, Version: plan.Version, PlanHash: plan.DocumentHash},
		Outputs:   manifest.Outputs,
	}, nil
}

func streamReleaseBundle(ctx context.Context, blobs blobstore.BlobStore, releasePrefix, bundleKey string, entries []FileEntry) (blobstore.BlobInfo, error) {
	entries = append([]FileEntry(nil), entries...)
	sort.Slice(entries, func(left, right int) bool { return entries[left].Path < entries[right].Path })
	pipeReader, pipeWriter := io.Pipe()
	written := make(chan error, 1)
	go func() {
		archive := zip.NewWriter(pipeWriter)
		var result error
		for _, entry := range entries {
			header := &zip.FileHeader{Name: entry.Path, Method: zip.Store}
			header.SetModTime(time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC))
			header.SetMode(0o644)
			output, err := archive.CreateHeader(header)
			if err != nil {
				result = err
				break
			}
			input, err := blobs.Get(ctx, releasePrefix+entry.Path)
			if err != nil {
				result = err
				break
			}
			_, copyErr := io.Copy(output, input)
			closeErr := input.Close()
			if copyErr != nil {
				result = copyErr
				break
			}
			if closeErr != nil {
				result = closeErr
				break
			}
		}
		if closeErr := archive.Close(); result == nil {
			result = closeErr
		}
		_ = pipeWriter.CloseWithError(result)
		written <- result
	}()
	info, putErr := blobs.Put(ctx, bundleKey, pipeReader)
	if putErr != nil {
		_ = pipeReader.CloseWithError(putErr)
	}
	writeErr := <-written
	if putErr != nil {
		return blobstore.BlobInfo{}, fmt.Errorf("write release bundle: %w", putErr)
	}
	if writeErr != nil {
		return blobstore.BlobInfo{}, fmt.Errorf("build release bundle: %w", writeErr)
	}
	return info, nil
}

func putReleaseFile(ctx context.Context, blobs blobstore.BlobStore, prefix, path string, contents []byte, mediaType string) (FileEntry, error) {
	info, err := blobs.Put(ctx, prefix+path, bytes.NewReader(contents))
	if err != nil {
		return FileEntry{}, fmt.Errorf("write release file %s: %w", path, err)
	}
	return FileEntry{Path: path, Size: info.Size, SHA256: info.SHA256, MediaType: mediaType}, nil
}

func (exporter *Exporter) reviewedTrajectory(ctx context.Context, candidate database.ReleaseCandidate) ([]byte, error) {
	reader, err := exporter.blobs.Get(ctx, candidate.NormalizedKey)
	if err != nil {
		return nil, fmt.Errorf("read normalized trajectory %s: %w", candidate.SessionID, err)
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, (32<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 32<<20 {
		return nil, fmt.Errorf("normalized trajectory %s exceeds 32 MiB", candidate.SessionID)
	}
	var document map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode normalized trajectory %s: %w", candidate.SessionID, err)
	}
	scores := map[string]any{}
	if err := json.Unmarshal(candidate.Review.Scores, &scores); err != nil {
		return nil, fmt.Errorf("decode review scores: %w", err)
	}
	document["qa"] = map[string]any{
		"state": "accepted", "rubric_version": candidate.Review.RubricVersion,
		"reviews": []any{map[string]any{
			"review_id": candidate.Review.ID, "reviewer_id": candidate.Review.ReviewerID,
			"scores": scores, "comments": candidate.Review.Comments, "decision": candidate.Review.Decision,
			"pii_review": candidate.Review.PIIReview, "created_at": candidate.Review.CreatedAt.UTC().Format(time.RFC3339Nano),
		}},
	}
	privacy, ok := document["privacy"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("trajectory %s privacy object is missing", candidate.SessionID)
	}
	privacy["pii_review"] = candidate.Review.PIIReview
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func normalizedIDs(values []string) ([]string, error) {
	result := append([]string(nil), values...)
	sort.Strings(result)
	for index, value := range result {
		if value == "" {
			return nil, errors.New("session IDs cannot be empty")
		}
		if index > 0 && result[index-1] == value {
			return nil, errors.New("session IDs must be unique")
		}
	}
	return result, nil
}

func sameIDs(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
