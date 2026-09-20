package httpapi

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"trajectory.local/api/internal/blobstore"
	"trajectory.local/api/internal/database"
	"trajectory.local/api/internal/domain"
	"trajectory.local/api/internal/worker"
)

func TestControlPlaneWorkflow(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	store, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := database.Migrate(ctx, store.Pool(), "../../migrations"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `TRUNCATE organizations, contributors, consent_documents, audit_events RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}

	blobs, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	schemaDocument, err := os.ReadFile("../../../../packages/trajectory-schema/schemas/trajectory-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewWithBlobStore(store, blobs, schemaDocument).Handler())
	defer server.Close()
	client := server.Client()

	organization := postJSON(t, client, server.URL+"/v1/organizations", map[string]any{"name": "Local Dataset Lab"}, http.StatusCreated)
	project := postJSON(t, client, server.URL+"/v1/projects", map[string]any{
		"organization_id": organization["id"], "name": "Photoshop Dataset V1", "target_trajectories": 100,
	}, http.StatusCreated)
	template := postJSON(t, client, server.URL+"/v1/task-templates", map[string]any{
		"project_id": project["id"], "name": "Remove background", "goal": "Remove the image background and export PNG.",
		"category": "design.image_editing", "difficulty": "intermediate",
		"specification": map[string]any{"required_application": "Photo editor", "capture_signals": []string{"screen", "pointer", "keys"}},
	}, http.StatusCreated)
	task := postJSON(t, client, server.URL+"/v1/tasks", map[string]any{
		"template_id": template["id"], "goal": "Remove the supplied product background.",
		"input_assets":     []any{map[string]any{"key": "inputs/product.png"}},
		"expected_outputs": []any{map[string]any{"name": "transparent-product.png", "media_type": "image/png"}},
	}, http.StatusCreated)
	contributor := postJSON(t, client, server.URL+"/v1/contributors", map[string]any{"display_name": "Fixture Contributor"}, http.StatusCreated)
	assignment := postJSON(t, client, server.URL+"/v1/assignments", map[string]any{
		"task_id": task["id"], "contributor_id": contributor["id"],
	}, http.StatusCreated)
	if assignment["session_state"] != "ASSIGNED" {
		t.Fatalf("session state = %v, want ASSIGNED", assignment["session_state"])
	}

	sessionID := assignment["session_id"].(string)
	postJSON(t, client, server.URL+"/v1/sessions/"+sessionID+"/preflight", map[string]any{}, http.StatusConflict)
	document := postJSON(t, client, server.URL+"/v1/consent-documents", map[string]any{
		"version": "consent-v1", "body": "I consent to explicit task-scoped recording.",
	}, http.StatusCreated)
	currentConsent := getJSON(t, client, server.URL+"/v1/consent-documents/current", http.StatusOK)
	if currentConsent["id"] != document["id"] || currentConsent["body"] != "I consent to explicit task-scoped recording." {
		t.Fatalf("unexpected current consent document: %#v", currentConsent)
	}
	postJSON(t, client, server.URL+"/v1/consent-acceptances", map[string]any{
		"session_id": sessionID, "contributor_id": contributor["id"], "document_id": document["id"], "client_version": "0.1.0",
	}, http.StatusCreated)
	ready := postJSON(t, client, server.URL+"/v1/sessions/"+sessionID+"/preflight", map[string]any{}, http.StatusOK)
	if ready["state"] != "READY" {
		t.Fatalf("session state = %v, want READY", ready["state"])
	}
	recording := postJSON(t, client, server.URL+"/v1/sessions/"+sessionID+"/transitions", map[string]any{"state": "RECORDING"}, http.StatusOK)
	if recording["state"] != "RECORDING" {
		t.Fatalf("session state = %v, want RECORDING", recording["state"])
	}

	assignments := getJSON(t, client, fmt.Sprintf("%s/v1/contributors/%s/assignments", server.URL, contributor["id"]), http.StatusOK)
	items := assignments["assignments"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["required_application"] != "Photo editor" || items[0].(map[string]any)["template_id"] != template["id"] {
		t.Fatalf("unexpected contributor assignments: %#v", items)
	}
	corsRequest, err := http.NewRequest(http.MethodOptions, server.URL+"/v1/contributors/example/assignments", nil)
	if err != nil {
		t.Fatal(err)
	}
	corsRequest.Header.Set("Origin", "tauri://localhost")
	corsResponse, err := client.Do(corsRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer corsResponse.Body.Close()
	if corsResponse.StatusCode != http.StatusNoContent || corsResponse.Header.Get("Access-Control-Allow-Origin") != "tauri://localhost" {
		t.Fatalf("unexpected collector CORS response: %d %#v", corsResponse.StatusCode, corsResponse.Header)
	}

	var auditCount int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_events`).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 11 {
		t.Fatalf("audit event count = %d, want 11", auditCount)
	}
	if _, err := store.Pool().Exec(ctx, `UPDATE audit_events SET actor='tampered' WHERE sequence_id=1`); err == nil {
		t.Fatal("audit event update unexpectedly succeeded")
	}

	postJSON(t, client, server.URL+"/v1/sessions/"+sessionID+"/transitions", map[string]any{"state": "FINALIZING"}, http.StatusOK)
	postJSON(t, client, server.URL+"/v1/sessions/"+sessionID+"/transitions", map[string]any{"state": "UPLOADING"}, http.StatusOK)
	video := putArtifact(t, client, server.URL, sessionID, "video/000001.mp4", "video bytes", "video/mp4")
	putArtifact(t, client, server.URL, sessionID, "events/000001.jsonl", "{}\n", "application/x-ndjson")
	output := putArtifact(t, client, server.URL, sessionID, "outputs/final.png", "png bytes", "image/png")
	trajectory := pipelineTrajectory(sessionID, task["id"].(string), template["id"].(string), contributor["id"].(string), document["id"].(string), video, output)
	trajectoryBytes, err := json.Marshal(trajectory)
	if err != nil {
		t.Fatal(err)
	}
	putArtifact(t, client, server.URL, sessionID, "trajectory.json", string(trajectoryBytes), "application/json")
	submission := postJSON(t, client, server.URL+"/v1/sessions/"+sessionID+"/submission", map[string]any{}, http.StatusAccepted)
	jobID := submission["job"].(map[string]any)["id"].(string)
	validator, err := worker.NewValidator(blobs, schemaDocument)
	if err != nil {
		t.Fatal(err)
	}
	processor, err := worker.NewProcessor(store, blobs, validator)
	if err != nil {
		t.Fatal(err)
	}
	processed, err := processor.ProcessNext(ctx, "integration-worker")
	if err != nil || !processed {
		t.Fatalf("process next = %v, %v", processed, err)
	}
	processedSession := getJSON(t, client, server.URL+"/v1/sessions/"+sessionID, http.StatusOK)
	if processedSession["state"] != "READY_FOR_REVIEW" {
		t.Fatalf("processed session state = %v, want READY_FOR_REVIEW", processedSession["state"])
	}
	job := getJSON(t, client, server.URL+"/v1/processing-jobs/"+jobID, http.StatusOK)
	if job["state"] != "completed" || job["attempt"] != float64(1) {
		t.Fatalf("unexpected processing job: %#v", job)
	}
	if exists, err := blobs.Exists(ctx, "derived/sessions/"+sessionID+"/trajectory.json"); err != nil || !exists {
		t.Fatalf("normalized trajectory exists = %v, %v", exists, err)
	}
	normalized := getJSON(t, client, server.URL+"/v1/sessions/"+sessionID+"/trajectory", http.StatusOK)
	if normalized["schema_version"] != "trajectory/v1" {
		t.Fatalf("unexpected normalized trajectory: %#v", normalized)
	}
	rangeRequest, err := http.NewRequest(http.MethodGet, server.URL+"/v1/sessions/"+sessionID+"/artifacts/video/000001.mp4", nil)
	if err != nil {
		t.Fatal(err)
	}
	rangeRequest.Header.Set("Range", "bytes=0-4")
	rangeRequest.Header.Set("X-Actor-ID", "integration-test")
	rangeResponse, err := client.Do(rangeRequest)
	if err != nil {
		t.Fatal(err)
	}
	rangeBody, readErr := io.ReadAll(rangeResponse.Body)
	rangeResponse.Body.Close()
	if readErr != nil || rangeResponse.StatusCode != http.StatusPartialContent || string(rangeBody) != "video" {
		t.Fatalf("range response = %d %q, %v", rangeResponse.StatusCode, rangeBody, readErr)
	}
	queue := getJSON(t, client, server.URL+"/v1/review-queue", http.StatusOK)
	if sessions := queue["sessions"].([]any); len(sessions) != 1 || sessions[0].(map[string]any)["session_id"] != sessionID {
		t.Fatalf("unexpected review queue: %#v", queue)
	}
	postJSONAs(t, client, server.URL+"/v1/sessions/"+sessionID+"/review-claim", map[string]any{"reviewer_id": "reviewer-1", "lease_seconds": 900}, "reviewer-1", http.StatusOK)
	postJSONAs(t, client, server.URL+"/v1/sessions/"+sessionID+"/review-claim", map[string]any{"reviewer_id": "reviewer-2", "lease_seconds": 900}, "reviewer-2", http.StatusConflict)
	deleteAs(t, client, server.URL+"/v1/sessions/"+sessionID+"/review-claim?reviewer_id=reviewer-1", "reviewer-1", http.StatusNoContent)
	postJSONAs(t, client, server.URL+"/v1/sessions/"+sessionID+"/review-claim", map[string]any{"reviewer_id": "reviewer-2", "lease_seconds": 900}, "reviewer-2", http.StatusOK)
	failedPIIReview := map[string]any{
		"reviewer_id": "reviewer-2", "rubric_version": "rubric-v1", "scores": map[string]any{"task_success": 5, "trajectory_quality": 4},
		"comments": "Clean result", "decision": "accepted", "pii_review": "pending",
	}
	postJSONAs(t, client, server.URL+"/v1/sessions/"+sessionID+"/reviews", failedPIIReview, "reviewer-2", http.StatusUnprocessableEntity)
	failedPIIReview["pii_review"] = "passed"
	failedPIIReview["redaction_plan"] = map[string]any{"schema_version": "redaction/v1", "segments": []any{}}
	postJSONAs(t, client, server.URL+"/v1/sessions/"+sessionID+"/reviews", failedPIIReview, "reviewer-2", http.StatusUnprocessableEntity)
	if unchanged := getJSON(t, client, server.URL+"/v1/sessions/"+sessionID, http.StatusOK); unchanged["state"] != "READY_FOR_REVIEW" {
		t.Fatalf("invalid redaction partially accepted review: %#v", unchanged)
	}
	failedPIIReview["redaction_plan"] = map[string]any{
		"schema_version": "redaction/v1",
		"segments": []any{map[string]any{
			"segment_id": "000001", "source_key": video["logical_key"],
			"decision": "publish_as_is", "regions": []any{},
		}},
	}
	accepted := postJSONAs(t, client, server.URL+"/v1/sessions/"+sessionID+"/reviews", failedPIIReview, "reviewer-2", http.StatusCreated)
	if accepted["session"].(map[string]any)["state"] != "ACCEPTED" || accepted["redaction_queued"] != true {
		t.Fatalf("reviewed session = %#v", accepted)
	}
	reviews := getJSON(t, client, server.URL+"/v1/sessions/"+sessionID+"/reviews", http.StatusOK)
	if items := reviews["reviews"].([]any); len(items) != 1 || items[0].(map[string]any)["pii_review"] != "passed" {
		t.Fatalf("unexpected reviews: %#v", reviews)
	}
	completedAssignments := getJSON(t, client, fmt.Sprintf("%s/v1/contributors/%s/assignments", server.URL, contributor["id"]), http.StatusOK)
	completedItem := completedAssignments["assignments"].([]any)[0].(map[string]any)
	if completedItem["session_state"] != "ACCEPTED" || completedItem["review_decision"] != "accepted" || completedItem["review_comments"] != "Clean result" {
		t.Fatalf("contributor completion status is missing review outcome: %#v", completedItem)
	}
	plans := getJSON(t, client, server.URL+"/v1/sessions/"+sessionID+"/redaction-plans", http.StatusOK)
	planItems := plans["plans"].([]any)
	if len(planItems) != 1 || planItems[0].(map[string]any)["version"] != float64(1) || len(planItems[0].(map[string]any)["document_hash"].(string)) != 64 {
		t.Fatalf("unexpected redaction plan history: %#v", plans)
	}
	plan := planItems[0].(map[string]any)
	var redactionJobID, redactionJobState string
	if err := store.Pool().QueryRow(ctx, `SELECT id,state FROM redaction_jobs WHERE plan_id=$1`, plan["id"]).Scan(&redactionJobID, &redactionJobState); err != nil || redactionJobState != "queued" {
		t.Fatalf("atomic redaction job id=%s state=%s err=%v", redactionJobID, redactionJobState, err)
	}
	if _, err := store.Pool().Exec(ctx, `UPDATE redaction_jobs SET state='dead_letter',attempt=3,error='ffmpeg unavailable',finished_at=now(),dead_lettered_at=now(),updated_at=now() WHERE id=$1`, redactionJobID); err != nil {
		t.Fatal(err)
	}
	failedRedactions := getJSON(t, client, server.URL+"/v1/redaction-jobs/dead-letter", http.StatusOK)["jobs"].([]any)
	if len(failedRedactions) != 1 || failedRedactions[0].(map[string]any)["id"] != redactionJobID {
		t.Fatalf("unexpected redaction dead-letter queue: %#v", failedRedactions)
	}
	retriedRedaction := postJSON(t, client, server.URL+"/v1/redaction-jobs/"+redactionJobID+"/retry", map[string]any{}, http.StatusOK)
	if retriedRedaction["state"] != "queued" || retriedRedaction["manual_requeues"] != float64(1) {
		t.Fatalf("retried redaction = %#v", retriedRedaction)
	}
	processor, err = processor.WithVideoRedaction(fixedMediaInspector{}, copyVideoRedactor{})
	if err != nil {
		t.Fatal(err)
	}
	processed, err = processor.ProcessNext(ctx, "integration-worker")
	if err != nil || !processed {
		t.Fatalf("process redaction = %v, %v", processed, err)
	}
	_, completedRedactionJob, err := store.LatestCompletedRedaction(ctx, sessionID)
	if err != nil || completedRedactionJob.ID != redactionJobID || completedRedactionJob.OutputManifestKey == "" {
		t.Fatalf("completed redaction job = %#v, %v", completedRedactionJob, err)
	}
	if exists, err := blobs.Exists(ctx, completedRedactionJob.OutputManifestKey); err != nil || !exists {
		t.Fatalf("redaction manifest exists = %v, %v", exists, err)
	}
	releaseRequest := map[string]any{"name": "photoshop-v1", "profile": "redacted_video", "session_ids": []string{sessionID}}
	releaseEnvelope := postJSON(t, client, server.URL+"/v1/projects/"+project["id"].(string)+"/releases", releaseRequest, http.StatusCreated)
	release := releaseEnvelope["release"].(map[string]any)
	releaseID := release["id"].(string)
	if release["profile"] != "redacted_video" || !strings.HasPrefix(releaseID, "rel_") || len(release["manifest_hash"].(string)) != 64 || len(release["bundle_hash"].(string)) != 64 {
		t.Fatalf("unexpected release: %#v", release)
	}
	retryEnvelope := postJSON(t, client, server.URL+"/v1/projects/"+project["id"].(string)+"/releases", releaseRequest, http.StatusCreated)
	if retryEnvelope["release"].(map[string]any)["manifest_hash"] != release["manifest_hash"] {
		t.Fatalf("idempotent release changed: %#v", retryEnvelope)
	}
	if retryEnvelope["release"].(map[string]any)["bundle_hash"] != release["bundle_hash"] {
		t.Fatalf("idempotent release bundle changed: %#v", retryEnvelope)
	}
	releaseManifest := getJSON(t, client, server.URL+"/v1/releases/"+releaseID+"/files/manifest.json", http.StatusOK)
	if releaseManifest["schema_version"] != "release/v1" || releaseManifest["release_profile"] != "redacted_video" || len(releaseManifest["source_session_ids"].([]any)) != 1 || len(releaseManifest["redaction_plans"].([]any)) != 1 {
		t.Fatalf("unexpected release manifest: %#v", releaseManifest)
	}
	filePaths := map[string]bool{}
	var expectedChecksums strings.Builder
	for _, item := range releaseManifest["files"].([]any) {
		file := item.(map[string]any)
		path := file["path"].(string)
		filePaths[path] = true
		if path != "checksums.sha256" {
			fmt.Fprintf(&expectedChecksums, "%s  %s\n", file["sha256"], path)
		}
	}
	for _, required := range []string{"README.md", "schema.json", "checksums.sha256", "trajectories.jsonl", "trajectories/" + sessionID + ".json", "videos/" + sessionID + "/000001.mp4"} {
		if !filePaths[required] {
			t.Fatalf("release manifest is missing %s: %#v", required, releaseManifest["files"])
		}
	}
	readme := getBytes(t, client, server.URL+"/v1/releases/"+releaseID+"/files/README.md", http.StatusOK)
	if !bytes.Contains(readme, []byte("# photoshop-v1")) || !bytes.Contains(readme, []byte("Only reviewer-approved derived video")) {
		t.Fatalf("unexpected release README: %q", readme)
	}
	releaseSchema := getJSON(t, client, server.URL+"/v1/releases/"+releaseID+"/files/schema.json", http.StatusOK)
	if releaseSchema["$id"] != "https://trajectory.local/schemas/trajectory-v1.json" {
		t.Fatalf("unexpected release schema: %#v", releaseSchema)
	}
	checksums := getBytes(t, client, server.URL+"/v1/releases/"+releaseID+"/files/checksums.sha256", http.StatusOK)
	if string(checksums) != expectedChecksums.String() {
		t.Fatalf("release checksums differ:\n%s\nwant:\n%s", checksums, expectedChecksums.String())
	}
	releasedTrajectory := getJSON(t, client, server.URL+"/v1/releases/"+releaseID+"/files/trajectories/"+sessionID+".json", http.StatusOK)
	if releasedTrajectory["qa"].(map[string]any)["state"] != "accepted" || releasedTrajectory["privacy"].(map[string]any)["pii_review"] != "passed" {
		t.Fatalf("released trajectory does not include accepted QA: %#v", releasedTrajectory)
	}
	bundle := getBytes(t, client, server.URL+"/v1/releases/"+releaseID+"/bundle", http.StatusOK)
	bundleDigest := sha256.Sum256(bundle)
	if fmt.Sprintf("%x", bundleDigest) != release["bundle_hash"] || float64(len(bundle)) != release["bundle_size"] {
		t.Fatalf("release bundle integrity differs: size=%d release=%#v", len(bundle), release)
	}
	archive, err := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		t.Fatalf("open release bundle: %v", err)
	}
	bundled := map[string]bool{}
	for _, file := range archive.File {
		bundled[file.Name] = true
	}
	for _, required := range []string{"README.md", "schema.json", "checksums.sha256", "manifest.json", "trajectories.jsonl", "trajectories/" + sessionID + ".json", "videos/" + sessionID + "/000001.mp4"} {
		if !bundled[required] {
			t.Fatalf("release ZIP is missing %s: %#v", required, bundled)
		}
	}
	releasedSession := getJSON(t, client, server.URL+"/v1/sessions/"+sessionID, http.StatusOK)
	if releasedSession["state"] != "RELEASED" {
		t.Fatalf("released session state = %v", releasedSession["state"])
	}
	projectList := getJSON(t, client, server.URL+"/v1/projects", http.StatusOK)
	projectItems := projectList["projects"].([]any)
	if len(projectItems) != 1 || projectItems[0].(map[string]any)["stage_counts"].(map[string]any)["released"] != float64(1) {
		t.Fatalf("unexpected project summaries: %#v", projectList)
	}
	projectSessions := getJSON(t, client, server.URL+"/v1/projects/"+project["id"].(string)+"/sessions", http.StatusOK)
	if items := projectSessions["sessions"].([]any); len(items) != 1 || items[0].(map[string]any)["state"] != "RELEASED" {
		t.Fatalf("unexpected project sessions: %#v", projectSessions)
	}
	bootstrapped := postJSON(t, client, server.URL+"/v1/campaigns", map[string]any{
		"organization_name": "Second Lab", "project_name": "Atomic Campaign", "target_trajectories": 10,
		"template_name": "Window task", "goal": "Complete a controlled task.", "category": "desktop.productivity",
		"difficulty": "beginner", "required_application": "example.exe", "finish_criteria": "Save and inspect result.txt.",
		"input_assets":     []any{map[string]any{"key": "inputs/example.txt"}},
		"expected_outputs": []any{map[string]any{"name": "result.txt", "media_type": "text/plain"}}, "contributor_name": "Second Contributor",
	}, http.StatusCreated)
	if bootstrapped["session_state"] != "ASSIGNED" {
		t.Fatalf("unexpected bootstrapped campaign: %#v", bootstrapped)
	}
	bootstrappedSession := getJSON(t, client, server.URL+"/v1/sessions/"+bootstrapped["session_id"].(string), http.StatusOK)
	if bootstrappedSession["state"] != "ASSIGNED" {
		t.Fatalf("bootstrapped session state = %v", bootstrappedSession["state"])
	}
	bootstrappedAssignments := getJSON(t, client, server.URL+"/v1/contributors/"+bootstrapped["contributor_id"].(string)+"/assignments", http.StatusOK)["assignments"].([]any)
	bootstrappedAssignment := bootstrappedAssignments[0].(map[string]any)
	if bootstrappedAssignment["specification"].(map[string]any)["finish_criteria"] != "Save and inspect result.txt." || len(bootstrappedAssignment["input_assets"].([]any)) != 1 {
		t.Fatalf("bootstrap task details were not preserved: %#v", bootstrappedAssignment)
	}
}

type fixedMediaInspector struct{}

func (fixedMediaInspector) InspectMP4(context.Context, io.Reader) (worker.MediaInfo, error) {
	return worker.MediaInfo{Codec: "h264", Width: 1920, Height: 1080, DurationSeconds: 2}, nil
}

type copyVideoRedactor struct{}

func (copyVideoRedactor) Redact(_ context.Context, input io.Reader, output io.Writer, _ []domain.RedactionRegion) error {
	_, err := io.Copy(output, input)
	return err
}

func putArtifact(t *testing.T, client *http.Client, baseURL, sessionID, artifactPath, contents, mediaType string) map[string]any {
	t.Helper()
	digest := sha256.Sum256([]byte(contents))
	request, err := http.NewRequest(http.MethodPut, baseURL+"/v1/sessions/"+sessionID+"/artifacts/"+artifactPath, bytes.NewBufferString(contents))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", mediaType)
	request.Header.Set("X-Artifact-SHA256", fmt.Sprintf("%x", digest))
	request.Header.Set("X-Actor-ID", "integration-test")
	request.Header.Set("X-Request-ID", fmt.Sprintf("req-%d", time.Now().UnixNano()))
	return doJSON(t, client, request, http.StatusCreated)
}

func pipelineTrajectory(sessionID, taskID, templateID, contributorID, consentDocumentID string, video, output map[string]any) map[string]any {
	return map[string]any{
		"schema_version": "trajectory/v1", "trajectory_id": "traj_" + sessionID,
		"task":        map[string]any{"task_id": taskID, "template_id": templateID, "goal": "Remove the supplied product background.", "category": "design.image_editing", "difficulty": "intermediate"},
		"environment": map[string]any{"os": "windows", "os_version": "11", "locale": "en-US", "timezone": "Asia/Kuala_Lumpur", "display": map[string]any{"width": 1920, "height": 1080, "scale_factor": 1}},
		"capture": map[string]any{
			"started_at": "2026-09-07T00:00:00Z", "duration_ns": 2_000_000_000, "clock": "monotonic_ns_since_session_start",
			"video_segments": []any{map[string]any{"segment_id": "000001", "key": video["logical_key"], "start_ns": 0, "end_ns": 2_000_000_000, "sha256": video["sha256"], "size": video["size_bytes"]}},
		},
		"actions":    []any{map[string]any{"timestamp_ns": 1_000_000, "type": "mouse_move", "x": 414, "y": 533}},
		"outcome":    map[string]any{"status": "success", "output_artifacts": []any{map[string]any{"key": output["logical_key"], "sha256": output["sha256"], "size": output["size_bytes"], "media_type": output["media_type"]}}},
		"qa":         map[string]any{"state": "pending", "rubric_version": "rubric-v1", "reviews": []any{}},
		"privacy":    map[string]any{"consent_document_id": consentDocumentID, "consent_version": "consent-v1", "clipboard_captured": false, "recording_visible": true, "pii_review": "pending"},
		"provenance": map[string]any{"session_id": sessionID, "contributor_id": contributorID, "collector_version": "0.1.0", "created_at": "2026-09-07T00:01:00Z"},
	}
}

func postJSON(t *testing.T, client *http.Client, url string, input any, expectedStatus int) map[string]any {
	t.Helper()
	return postJSONAs(t, client, url, input, "integration-test", expectedStatus)
}

func postJSONAs(t *testing.T, client *http.Client, url string, input any, actor string, expectedStatus int) map[string]any {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Actor-ID", actor)
	request.Header.Set("X-Request-ID", fmt.Sprintf("req-%d", time.Now().UnixNano()))
	return doJSON(t, client, request, expectedStatus)
}

func putJSON(t *testing.T, client *http.Client, url string, input any, expectedStatus int) map[string]any {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Actor-ID", "integration-test")
	request.Header.Set("X-Request-ID", fmt.Sprintf("req-%d", time.Now().UnixNano()))
	return doJSON(t, client, request, expectedStatus)
}

func getJSON(t *testing.T, client *http.Client, url string, expectedStatus int) map[string]any {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Actor-ID", "integration-test")
	return doJSON(t, client, request, expectedStatus)
}

func getBytes(t *testing.T, client *http.Client, url string, expectedStatus int) []byte {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Actor-ID", "integration-test")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != expectedStatus {
		t.Fatalf("GET %s status = %d, want %d; response=%q", url, response.StatusCode, expectedStatus, body)
	}
	return body
}

func deleteAs(t *testing.T, client *http.Client, url, actor string, expectedStatus int) {
	t.Helper()
	request, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Actor-ID", actor)
	request.Header.Set("X-Request-ID", fmt.Sprintf("req-%d", time.Now().UnixNano()))
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != expectedStatus {
		t.Fatalf("DELETE %s status = %d, want %d", url, response.StatusCode, expectedStatus)
	}
}

func doJSON(t *testing.T, client *http.Client, request *http.Request, expectedStatus int) map[string]any {
	t.Helper()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != expectedStatus {
		t.Fatalf("%s %s status = %d, want %d; response=%#v", request.Method, request.URL, response.StatusCode, expectedStatus, decoded)
	}
	return decoded
}
