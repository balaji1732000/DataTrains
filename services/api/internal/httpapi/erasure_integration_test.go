package httpapi

import (
	"context"
	"errors"
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

func TestLegalHoldBlocksDeletionAndWorkerPurgesOnlyAfterRelease(t *testing.T) {
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
	if _, err := store.Pool().Exec(ctx, `
		TRUNCATE organizations, accounts, audit_events RESTART IDENTITY CASCADE;
		INSERT INTO organizations (id, name) VALUES ('org_erase', 'Erasure Test');
		INSERT INTO accounts (id, display_name, email) VALUES ('integration-test', 'Test Admin', 'erase-admin@example.com');
		INSERT INTO organization_memberships (organization_id, account_id, role)
		VALUES ('org_erase', 'integration-test', 'admin');
		INSERT INTO projects (id, organization_id, name, target_trajectories)
		VALUES ('proj_erase', 'org_erase', 'Erasure Project', 1);
		INSERT INTO task_templates (id, project_id, name, goal, category, difficulty, specification)
		VALUES ('tmpl_erase', 'proj_erase', 'Erasure Template', 'Erase data', 'test', 'beginner', '{}');
		INSERT INTO tasks (id, template_id, goal) VALUES ('task_erase', 'tmpl_erase', 'Erase data');
		INSERT INTO contributors (id, display_name) VALUES ('contrib_erase', 'Erasure Contributor');
		INSERT INTO assignments (id, task_id, contributor_id) VALUES ('assign_erase', 'task_erase', 'contrib_erase');
		INSERT INTO sessions (id, assignment_id, state) VALUES ('sess_erase', 'assign_erase', 'REJECTED');
		INSERT INTO artifacts (id, session_id, logical_key, sha256, size_bytes, media_type)
		VALUES ('artifact_erase', 'sess_erase', 'raw/sessions/sess_erase/video/000001.mp4', repeat('a',64), 7, 'video/mp4');
		INSERT INTO processing_jobs (id, session_id, job_type, state, attempt, available_at, finished_at, updated_at)
		VALUES ('job_erase', 'sess_erase', 'validate_trajectory', 'completed', 1, now(), now(), now());
		INSERT INTO validation_results
		  (id, job_id, session_id, valid, errors, normalized_key, schema_version, validator_version)
		VALUES ('validation_erase', 'job_erase', 'sess_erase', true, '[]',
		  'derived/sessions/sess_erase/trajectory.json', 'trajectory/v1', 'validator-v1');
		INSERT INTO redaction_plans
		  (id, session_id, reviewer_id, version, schema_version, document, document_hash, request_id)
		VALUES ('redaction_erase', 'sess_erase', 'reviewer-erase', 1, 'redaction/v1',
		  '{"schema_version":"redaction/v1","segments":[{"segment_id":"000001","source_key":"raw/sessions/sess_erase/video/000001.mp4","decision":"redact","regions":[{"start_ns":0,"end_ns":1,"x":0,"y":0,"width":1,"height":1,"kind":"personal_data"}]}]}',
		  repeat('e',64), 'req-redaction-erase');
		INSERT INTO redaction_jobs
		  (id, plan_id, session_id, state, attempt, available_at, output_manifest_key, finished_at)
		VALUES ('redaction_job_erase', 'redaction_erase', 'sess_erase', 'completed', 1, now(),
		  'derived/sessions/sess_erase/redactions/redaction_erase/manifest.json', now());`); err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for key, content := range map[string]string{
		"raw/sessions/sess_erase/video/000001.mp4":                             "capture",
		"derived/sessions/sess_erase/trajectory.json":                          "{}",
		"derived/sessions/sess_erase/actions.jsonl":                            "{}\n",
		"derived/sessions/sess_erase/redactions/redaction_erase/manifest.json": "{}",
		"derived/sessions/sess_erase/redactions/redaction_erase/000001.mp4":    "redacted",
	} {
		if _, err := blobs.Put(ctx, key, strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}
	schema, err := os.ReadFile("../../../../packages/trajectory-schema/schemas/trajectory-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewWithBlobStore(store, blobs, schema).Handler())
	defer server.Close()

	hold := postJSONAs(t, server.Client(), server.URL+"/v1/sessions/sess_erase/legal-holds", map[string]any{
		"reason": "Preserve evidence for an active dispute",
	}, "admin-local", http.StatusCreated)
	postJSONAs(t, server.Client(), server.URL+"/v1/sessions/sess_erase/legal-holds", map[string]any{
		"reason": "A duplicate active hold must be rejected",
	}, "admin-local", http.StatusConflict)
	postJSON(t, server.Client(), server.URL+"/v1/sessions/sess_erase/deletion-requests", map[string]any{
		"reason": "Approved privacy erasure request",
	}, http.StatusConflict)
	deleteAs(t, server.Client(), server.URL+"/v1/sessions/sess_erase/legal-holds/"+hold["id"].(string), "admin-local", http.StatusNoContent)
	requested := postJSON(t, server.Client(), server.URL+"/v1/sessions/sess_erase/deletion-requests", map[string]any{
		"reason": "Approved privacy erasure request",
	}, http.StatusAccepted)
	if requested["object_count"] != float64(5) || requested["state"] != "queued" {
		t.Fatalf("deletion request = %#v", requested)
	}
	postJSON(t, server.Client(), server.URL+"/v1/sessions/sess_erase/deletion-requests", map[string]any{
		"reason": "A duplicate erasure request must be rejected",
	}, http.StatusConflict)
	if _, err := store.Pool().Exec(ctx, `
		UPDATE deletion_requests SET state='dead_letter', attempt=3,
		last_error='temporary object-store outage', updated_at=now()
		WHERE id=$1`, requested["id"]); err != nil {
		t.Fatal(err)
	}
	retried := postJSON(t, server.Client(), server.URL+"/v1/deletion-requests/"+requested["id"].(string)+"/retry", map[string]any{}, http.StatusOK)
	if retried["state"] != "queued" || retried["attempt"] != float64(0) || retried["manual_requeues"] != float64(1) {
		t.Fatalf("retried deletion request = %#v", retried)
	}
	if _, err := store.Pool().Exec(ctx, `UPDATE sessions SET state='ACCEPTED' WHERE id='sess_erase'`); err != nil {
		t.Fatal(err)
	}
	_, err = store.CommitRelease(ctx, domain.DatasetRelease{
		ID: "rel_erase_race", ProjectID: "proj_erase", Name: "must-not-publish",
		Profile: "trajectory_only", SchemaVersion: "trajectory/v1", ExporterVersion: "test", PipelineVersion: "test",
		SourceSessionIDs: []string{"sess_erase"}, ConfigurationHash: strings.Repeat("b", 64), ManifestHash: strings.Repeat("c", 64),
		BundleHash: strings.Repeat("d", 64), BundleSize: 1,
		ObjectKeys: []string{"bundles/rel_erase_race.zip", "releases/rel_erase_race/README.md", "releases/rel_erase_race/checksums.sha256", "releases/rel_erase_race/manifest.json", "releases/rel_erase_race/schema.json", "releases/rel_erase_race/trajectories.jsonl", "releases/rel_erase_race/trajectories/sess_erase.json"},
	}, "admin-local", "req-release-race", time.Now())
	if !errors.Is(err, database.ErrReleaseEligibility) {
		t.Fatalf("release with active deletion request error = %v", err)
	}

	validator, err := worker.NewValidator(blobs, schema)
	if err != nil {
		t.Fatal(err)
	}
	processor, err := worker.NewProcessor(store, blobs, validator)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `UPDATE sessions SET state='RELEASED' WHERE id='sess_erase'`); err != nil {
		t.Fatal(err)
	}
	processed, err := processor.ProcessNext(ctx, "erasure-worker")
	if err != nil || !processed {
		t.Fatalf("state-drift deletion processing = %v, %v", processed, err)
	}
	if exists, err := blobs.Exists(ctx, "raw/sessions/sess_erase/video/000001.mp4"); err != nil || !exists {
		t.Fatalf("state drift did not preserve raw object: exists=%v err=%v", exists, err)
	}
	postJSON(t, server.Client(), server.URL+"/v1/deletion-requests/"+requested["id"].(string)+"/retry", map[string]any{}, http.StatusConflict)
	if _, err := store.Pool().Exec(ctx, `UPDATE sessions SET state='ACCEPTED' WHERE id='sess_erase'`); err != nil {
		t.Fatal(err)
	}
	retried = postJSON(t, server.Client(), server.URL+"/v1/deletion-requests/"+requested["id"].(string)+"/retry", map[string]any{}, http.StatusOK)
	if retried["manual_requeues"] != float64(2) {
		t.Fatalf("second deletion retry = %#v", retried)
	}
	lateHold := postJSON(t, server.Client(), server.URL+"/v1/sessions/sess_erase/legal-holds", map[string]any{
		"reason": "A new dispute arrived before the worker started",
	}, http.StatusCreated)
	processed, err = processor.ProcessNext(ctx, "erasure-worker")
	if err != nil || !processed {
		t.Fatalf("held deletion processing = %v, %v", processed, err)
	}
	if exists, err := blobs.Exists(ctx, "raw/sessions/sess_erase/video/000001.mp4"); err != nil || !exists {
		t.Fatalf("legal hold did not preserve raw object: exists=%v err=%v", exists, err)
	}
	blocked := getJSON(t, server.Client(), server.URL+"/v1/deletion-requests", http.StatusOK)["deletion_requests"].([]any)
	if blocked[0].(map[string]any)["state"] != "blocked" {
		t.Fatalf("held deletion request = %#v", blocked)
	}
	deleteAs(t, server.Client(), server.URL+"/v1/sessions/sess_erase/legal-holds/"+lateHold["id"].(string), "integration-test", http.StatusNoContent)

	processed, err = processor.ProcessNext(ctx, "erasure-worker")
	if err != nil || !processed {
		t.Fatalf("deletion processing = %v, %v", processed, err)
	}
	for _, key := range []string{
		"raw/sessions/sess_erase/video/000001.mp4",
		"derived/sessions/sess_erase/trajectory.json",
		"derived/sessions/sess_erase/actions.jsonl",
		"derived/sessions/sess_erase/redactions/redaction_erase/manifest.json",
		"derived/sessions/sess_erase/redactions/redaction_erase/000001.mp4",
	} {
		if exists, err := blobs.Exists(ctx, key); err != nil || exists {
			t.Fatalf("purged object %s exists=%v err=%v", key, exists, err)
		}
	}
	deleted := getJSON(t, server.Client(), server.URL+"/v1/sessions/sess_erase", http.StatusOK)
	if deleted["state"] != "DELETED" {
		t.Fatalf("tombstoned session = %#v", deleted)
	}
	requests := getJSON(t, server.Client(), server.URL+"/v1/deletion-requests", http.StatusOK)["deletion_requests"].([]any)
	if len(requests) != 1 || requests[0].(map[string]any)["state"] != "completed" {
		t.Fatalf("completed deletion requests = %#v", requests)
	}
	var artifacts, redactionPlans, redactionJobs, auditEvents int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM artifacts WHERE session_id='sess_erase'`).Scan(&artifacts); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM redaction_plans WHERE session_id='sess_erase'`).Scan(&redactionPlans); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM redaction_jobs WHERE session_id='sess_erase'`).Scan(&redactionJobs); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action IN ('legal_hold.placed','legal_hold.released','session.deletion_requested','session.deletion_blocked','session.deletion_dead_letter','session.deletion_manual_retry','session.deleted')`).Scan(&auditEvents); err != nil {
		t.Fatal(err)
	}
	if artifacts != 0 || redactionPlans != 0 || redactionJobs != 0 || auditEvents != 10 {
		t.Fatalf("post-erasure artifacts=%d redaction_plans=%d redaction_jobs=%d audit_events=%d", artifacts, redactionPlans, redactionJobs, auditEvents)
	}
}
