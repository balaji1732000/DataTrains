package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"trajectory.local/api/internal/blobstore"
	"trajectory.local/api/internal/database"
	"trajectory.local/api/internal/worker"
)

func TestHundredTrajectorySyntheticReliabilityRun(t *testing.T) {
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

	organization := postJSON(t, client, server.URL+"/v1/organizations", map[string]any{"name": "Reliability Lab"}, http.StatusCreated)
	project := postJSON(t, client, server.URL+"/v1/projects", map[string]any{
		"organization_id": organization["id"], "name": "100 trajectory soak", "target_trajectories": 100,
	}, http.StatusCreated)
	template := postJSON(t, client, server.URL+"/v1/task-templates", map[string]any{
		"project_id": project["id"], "name": "Synthetic edit", "goal": "Produce the controlled output.",
		"category": "desktop.synthetic", "difficulty": "beginner",
		"specification": map[string]any{"required_application": "fixture.exe", "capture_signals": []string{"screen", "pointer", "keys"}},
	}, http.StatusCreated)
	contributor := postJSON(t, client, server.URL+"/v1/contributors", map[string]any{"display_name": "Soak Contributor"}, http.StatusCreated)
	consent := postJSON(t, client, server.URL+"/v1/consent-documents", map[string]any{
		"version": "soak-consent-v1", "body": "I consent to the synthetic reliability capture.",
	}, http.StatusCreated)

	validator, err := worker.NewValidator(blobs, schemaDocument)
	if err != nil {
		t.Fatal(err)
	}
	processor, err := worker.NewProcessor(store, blobs, validator)
	if err != nil {
		t.Fatal(err)
	}

	sessionIDs := make([]string, 0, 100)
	for index := 0; index < 100; index++ {
		task := postJSON(t, client, server.URL+"/v1/tasks", map[string]any{
			"template_id": template["id"], "goal": fmt.Sprintf("Complete synthetic task %03d.", index+1),
			"input_assets": []any{}, "expected_outputs": []any{map[string]any{"name": "result.txt", "media_type": "text/plain"}},
		}, http.StatusCreated)
		assignment := postJSON(t, client, server.URL+"/v1/assignments", map[string]any{
			"task_id": task["id"], "contributor_id": contributor["id"],
		}, http.StatusCreated)
		sessionID := assignment["session_id"].(string)
		sessionIDs = append(sessionIDs, sessionID)
		postJSON(t, client, server.URL+"/v1/consent-acceptances", map[string]any{
			"session_id": sessionID, "contributor_id": contributor["id"], "document_id": consent["id"], "client_version": "0.1.0",
		}, http.StatusCreated)
		postJSON(t, client, server.URL+"/v1/sessions/"+sessionID+"/preflight", map[string]any{}, http.StatusOK)
		for _, state := range []string{"RECORDING", "FINALIZING", "UPLOADING"} {
			postJSON(t, client, server.URL+"/v1/sessions/"+sessionID+"/transitions", map[string]any{"state": state}, http.StatusOK)
		}
		video := putArtifact(t, client, server.URL, sessionID, "video/000001.mp4", fmt.Sprintf("video-%03d", index), "video/mp4")
		putArtifact(t, client, server.URL, sessionID, "events/000001.jsonl", "{}\n", "application/x-ndjson")
		output := putArtifact(t, client, server.URL, sessionID, "outputs/result.txt", fmt.Sprintf("result-%03d", index), "text/plain")
		trajectory := pipelineTrajectory(sessionID, task["id"].(string), template["id"].(string), contributor["id"].(string), consent["id"].(string), video, output)
		trajectory["task"].(map[string]any)["category"] = "desktop.synthetic"
		trajectory["task"].(map[string]any)["difficulty"] = "beginner"
		trajectory["privacy"].(map[string]any)["consent_version"] = "soak-consent-v1"
		trajectory["actions"] = []any{
			map[string]any{"timestamp_ns": 100_000_000, "type": "mouse_move", "x": 50, "y": 60},
			map[string]any{"timestamp_ns": 110_000_000, "type": "mouse_down", "button": "left"},
			map[string]any{"timestamp_ns": 120_000_000, "type": "mouse_up", "button": "left"},
		}
		trajectoryBytes, err := json.Marshal(trajectory)
		if err != nil {
			t.Fatal(err)
		}
		putArtifact(t, client, server.URL, sessionID, "trajectory.json", string(trajectoryBytes), "application/json")
		postJSON(t, client, server.URL+"/v1/sessions/"+sessionID+"/submission", map[string]any{}, http.StatusAccepted)
		if processed, err := processor.ProcessNext(ctx, "soak-worker"); err != nil || !processed {
			t.Fatalf("trajectory %d processing = %v, %v", index, processed, err)
		}
		if exists, err := blobs.Exists(ctx, "derived/sessions/"+sessionID+"/actions.jsonl"); err != nil || !exists {
			t.Fatalf("trajectory %d semantic actions exist = %v, %v", index, exists, err)
		}
		postJSONAs(t, client, server.URL+"/v1/sessions/"+sessionID+"/review-claim", map[string]any{
			"reviewer_id": "soak-reviewer", "lease_seconds": 900,
		}, "soak-reviewer", http.StatusOK)
		postJSONAs(t, client, server.URL+"/v1/sessions/"+sessionID+"/reviews", map[string]any{
			"reviewer_id": "soak-reviewer", "rubric_version": "rubric-v1",
			"scores":   map[string]any{"goal_completion": 40, "output_correctness": 30, "process_validity": 20, "data_quality": 10},
			"comments": "Synthetic reliability pass", "decision": "accepted", "pii_review": "passed",
		}, "soak-reviewer", http.StatusCreated)
	}

	release := postJSON(t, client, server.URL+"/v1/projects/"+project["id"].(string)+"/releases", map[string]any{
		"name": "synthetic-100-v1", "session_ids": sessionIDs,
	}, http.StatusCreated)["release"].(map[string]any)
	manifest := getJSON(t, client, server.URL+"/v1/releases/"+release["id"].(string)+"/files/manifest.json", http.StatusOK)
	if sources := manifest["source_session_ids"].([]any); len(sources) != 100 {
		t.Fatalf("release source count = %d, want 100", len(sources))
	}
	for _, required := range []string{"README.md", "schema.json", "checksums.sha256", "trajectories.jsonl"} {
		if exists, err := blobs.Exists(ctx, "releases/"+release["id"].(string)+"/"+required); err != nil || !exists {
			t.Fatalf("release file %s exists = %v, %v", required, exists, err)
		}
	}
	projects := getJSON(t, client, server.URL+"/v1/projects", http.StatusOK)["projects"].([]any)
	if released := projects[0].(map[string]any)["stage_counts"].(map[string]any)["released"]; released != float64(100) {
		t.Fatalf("released count = %v, want 100", released)
	}
}
