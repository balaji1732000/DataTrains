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
	"trajectory.local/api/internal/worker"
)

func TestAutomaticRetentionRechecksLateLegalHoldAndPurgesAllClasses(t *testing.T) {
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
		INSERT INTO organizations (id, name) VALUES ('org_retention', 'Retention Test');
		INSERT INTO accounts (id, display_name, email) VALUES ('retention-admin', 'Retention Admin', 'retention@example.com');
		INSERT INTO organization_memberships (organization_id, account_id, role)
		VALUES ('org_retention', 'retention-admin', 'admin');
		INSERT INTO projects (id, organization_id, name, target_trajectories)
		VALUES ('proj_retention', 'org_retention', 'Retention Project', 1);
		INSERT INTO task_templates (id, project_id, name, goal, category, difficulty, specification)
		VALUES ('tmpl_retention', 'proj_retention', 'Retention Template', 'Retain safely', 'test', 'beginner', '{}');
		INSERT INTO tasks (id, template_id, goal) VALUES ('task_retention', 'tmpl_retention', 'Retain safely');
		INSERT INTO contributors (id, display_name) VALUES ('contrib_retention', 'Retention Contributor');
		INSERT INTO assignments (id, task_id, contributor_id) VALUES ('assign_retention', 'task_retention', 'contrib_retention');
		INSERT INTO sessions (id, assignment_id, state, created_at, updated_at)
		VALUES ('sess_retention', 'assign_retention', 'RELEASED', now()-interval '48 hours', now()-interval '48 hours');
		INSERT INTO artifacts (id, session_id, logical_key, sha256, size_bytes, media_type, created_at)
		VALUES ('artifact_retention', 'sess_retention', 'raw/sessions/sess_retention/video/000001.mp4', repeat('a',64), 7, 'video/mp4', now()-interval '48 hours');
		INSERT INTO processing_jobs (id, session_id, job_type, state, attempt, available_at, finished_at, updated_at)
		VALUES ('job_retention', 'sess_retention', 'validate_trajectory', 'completed', 1, now()-interval '48 hours', now()-interval '48 hours', now()-interval '48 hours');
		INSERT INTO validation_results
		  (id, job_id, session_id, valid, errors, normalized_key, schema_version, validator_version, created_at)
		VALUES ('validation_retention', 'job_retention', 'sess_retention', true, '[]',
		  'derived/sessions/sess_retention/trajectory.json', 'trajectory/v1', 'validator-v1', now()-interval '48 hours');
		INSERT INTO redaction_plans
		  (id, session_id, reviewer_id, version, schema_version, document, document_hash, request_id, created_at)
		VALUES ('redaction_retention', 'sess_retention', 'reviewer-retention', 1, 'redaction/v1',
		  '{"schema_version":"redaction/v1","segments":[{"segment_id":"000001","source_key":"raw/sessions/sess_retention/video/000001.mp4","decision":"redact","regions":[{"start_ns":0,"end_ns":1,"x":0,"y":0,"width":1,"height":1,"kind":"personal_data"}]}]}',
		  repeat('e',64), 'req-redaction-retention', now()-interval '47 hours');
		INSERT INTO redaction_jobs
		  (id, plan_id, session_id, state, attempt, available_at, output_manifest_key, created_at, updated_at, finished_at)
		VALUES ('redaction_job_retention', 'redaction_retention', 'sess_retention', 'completed', 1,
		  now()-interval '47 hours', 'derived/sessions/sess_retention/redactions/redaction_retention/manifest.json',
		  now()-interval '47 hours', now()-interval '47 hours', now()-interval '47 hours');
		INSERT INTO dataset_releases
		  (id, project_id, name, schema_version, exporter_version, pipeline_version,
		   source_session_ids, configuration_hash, manifest_hash, bundle_hash, bundle_size, created_at, object_keys)
		VALUES ('rel_retention', 'proj_retention', 'retention-release', 'trajectory/v1', 'test', 'test',
		  '["sess_retention"]', repeat('b',64), repeat('c',64), repeat('d',64), 7, now()-interval '48 hours',
		  '["bundles/rel_retention.zip","releases/rel_retention/README.md","releases/rel_retention/checksums.sha256","releases/rel_retention/manifest.json","releases/rel_retention/schema.json","releases/rel_retention/trajectories.jsonl","releases/rel_retention/trajectories/sess_retention.json","releases/rel_retention/videos/sess_retention/000001.mp4"]');
		INSERT INTO release_sessions (release_id, session_id, ordinal)
		VALUES ('rel_retention', 'sess_retention', 0);
		INSERT INTO retention_policies
		  (organization_id, raw_days, derived_days, release_days, updated_by, created_at, updated_at)
		VALUES ('org_retention', 1, 1, 1, 'retention-admin', now(), now());`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	blobs, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{
		"raw/sessions/sess_retention/video/000001.mp4",
		"derived/sessions/sess_retention/trajectory.json",
		"derived/sessions/sess_retention/actions.jsonl",
		"derived/sessions/sess_retention/redactions/redaction_retention/manifest.json",
		"derived/sessions/sess_retention/redactions/redaction_retention/000001.mp4",
		"releases/rel_retention/README.md",
		"releases/rel_retention/checksums.sha256",
		"releases/rel_retention/manifest.json",
		"releases/rel_retention/schema.json",
		"releases/rel_retention/trajectories.jsonl",
		"releases/rel_retention/trajectories/sess_retention.json",
		"releases/rel_retention/videos/sess_retention/000001.mp4",
		"bundles/rel_retention.zip",
	}
	for _, key := range keys {
		if _, err := blobs.Put(ctx, key, strings.NewReader("fixture")); err != nil {
			t.Fatal(err)
		}
	}
	queued, err := store.QueueExpiredRetention(ctx, "retention-worker", now, 100)
	if err != nil || queued != 3 {
		t.Fatalf("queued retention jobs=%d err=%v", queued, err)
	}
	hold, err := store.PlaceLegalHold(ctx, "sess_retention", "Late preservation order", "retention-admin", "req-hold", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	schema, err := os.ReadFile("../../../../packages/trajectory-schema/schemas/trajectory-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	validator, err := worker.NewValidator(blobs, schema)
	if err != nil {
		t.Fatal(err)
	}
	processor, err := worker.NewProcessor(store, blobs, validator)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		processed, err := processor.ProcessNext(ctx, "retention-worker")
		if err != nil || !processed {
			t.Fatalf("blocking retention job: processed=%v err=%v", processed, err)
		}
	}
	var blocked int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM retention_purge_requests WHERE state='blocked'`).Scan(&blocked); err != nil || blocked != 3 {
		t.Fatalf("blocked retention jobs=%d err=%v", blocked, err)
	}
	snapshot, err := store.GetQueueSnapshot(ctx, time.Now().UTC())
	if err != nil || snapshot.RetentionBlocked != 3 || snapshot.ProcessingQueued != 0 {
		t.Fatalf("operational queue snapshot=%#v err=%v", snapshot, err)
	}
	for _, key := range keys {
		if exists, err := blobs.Exists(ctx, key); err != nil || !exists {
			t.Fatalf("late legal hold did not preserve %s: exists=%v err=%v", key, exists, err)
		}
	}
	if err := store.ReleaseLegalHold(ctx, "sess_retention", hold.ID, "retention-admin", "req-release-hold", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for index := range 3 {
		processed, err := processor.ProcessNext(ctx, "retention-worker")
		if err != nil || !processed {
			var states string
			_ = store.Pool().QueryRow(ctx, `SELECT string_agg(resource_type || ':' || state, ',' ORDER BY resource_type) FROM retention_purge_requests`).Scan(&states)
			t.Fatalf("retention purge %d: processed=%v err=%v states=%s", index, processed, err, states)
		}
	}
	for _, key := range keys {
		if exists, err := blobs.Exists(ctx, key); err != nil || exists {
			t.Fatalf("expired object %s exists=%v err=%v", key, exists, err)
		}
	}
	var completed, rawPurged, derivedPurged, redactionPurged, releasePurged, auditEvents int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM retention_purge_requests WHERE state='completed'`).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM artifacts WHERE session_id='sess_retention' AND purged_at IS NOT NULL`).Scan(&rawPurged); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM validation_results WHERE session_id='sess_retention' AND purged_at IS NOT NULL`).Scan(&derivedPurged); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM redaction_jobs WHERE session_id='sess_retention' AND purged_at IS NOT NULL`).Scan(&redactionPurged); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM dataset_releases WHERE id='rel_retention' AND purged_at IS NOT NULL`).Scan(&releasePurged); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action LIKE 'retention.purge_%'`).Scan(&auditEvents); err != nil {
		t.Fatal(err)
	}
	if completed != 3 || rawPurged != 1 || derivedPurged != 1 || redactionPurged != 1 || releasePurged != 1 || auditEvents != 9 {
		t.Fatalf("retention result completed=%d raw=%d derived=%d redaction=%d release=%d audits=%d", completed, rawPurged, derivedPurged, redactionPurged, releasePurged, auditEvents)
	}

	// A policy extension after a job was queued must cancel that stale snapshot.
	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO tasks (id, template_id, goal) VALUES ('task_retention_extended', 'tmpl_retention', 'Keep longer');
		INSERT INTO assignments (id, task_id, contributor_id) VALUES ('assign_retention_extended', 'task_retention_extended', 'contrib_retention');
		INSERT INTO sessions (id, assignment_id, state, created_at, updated_at)
		VALUES ('sess_retention_extended', 'assign_retention_extended', 'ACCEPTED', now()-interval '48 hours', now()-interval '48 hours');
		INSERT INTO artifacts (id, session_id, logical_key, sha256, size_bytes, media_type, created_at)
		VALUES ('artifact_retention_extended', 'sess_retention_extended', 'raw/sessions/sess_retention_extended/video/000001.mp4', repeat('e',64), 7, 'video/mp4', now()-interval '48 hours');`); err != nil {
		t.Fatal(err)
	}
	extendedKey := "raw/sessions/sess_retention_extended/video/000001.mp4"
	if _, err := blobs.Put(ctx, extendedKey, strings.NewReader("fixture")); err != nil {
		t.Fatal(err)
	}
	queued, err = store.QueueExpiredRetention(ctx, "retention-worker", time.Now().UTC(), 100)
	if err != nil || queued != 1 {
		t.Fatalf("queued stale-policy test jobs=%d err=%v", queued, err)
	}
	policy, err := store.GetRetentionPolicy(ctx, "org_retention")
	if err != nil {
		t.Fatal(err)
	}
	policy.RawDays = 3650
	if _, err := store.UpsertRetentionPolicy(ctx, policy, "retention-admin", "req-extend-retention", time.Now().UTC().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimRetentionPurge(ctx, "retention-worker", time.Minute, time.Now().UTC().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	var cancelled int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM retention_purge_requests WHERE resource_id='sess_retention_extended' AND state='cancelled'`).Scan(&cancelled); err != nil || cancelled != 1 {
		t.Fatalf("cancelled stale retention jobs=%d err=%v", cancelled, err)
	}
	if exists, err := blobs.Exists(ctx, extendedKey); err != nil || !exists {
		t.Fatalf("policy extension did not preserve object: exists=%v err=%v", exists, err)
	}
	policy.RawDays = 1
	if _, err := store.UpsertRetentionPolicy(ctx, policy, "retention-admin", "req-shorten-retention", time.Now().UTC().Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	queued, err = store.QueueExpiredRetention(ctx, "retention-worker", time.Now().UTC().Add(4*time.Second), 100)
	if err != nil || queued != 1 {
		t.Fatalf("requeued current-policy jobs=%d err=%v", queued, err)
	}
	leased, err := store.ClaimRetentionPurge(ctx, "retention-worker", time.Minute, time.Now().UTC().Add(5*time.Second))
	if err != nil || leased.State != "leased" {
		t.Fatalf("leased current-policy job=%#v err=%v", leased, err)
	}
	policy.RawDays = 3650
	if _, err := store.UpsertRetentionPolicy(ctx, policy, "retention-admin", "req-race-policy", time.Now().UTC().Add(6*time.Second)); !errors.Is(err, database.ErrRetentionPurgeInProgress) {
		t.Fatalf("policy write during purge error=%v", err)
	}
	if _, err := store.PlaceLegalHold(ctx, "sess_retention_extended", "Too late after purge lease", "retention-admin", "req-race-hold", time.Now().UTC().Add(6*time.Second)); !errors.Is(err, database.ErrRetentionPurgeInProgress) {
		t.Fatalf("hold write during purge error=%v", err)
	}
	if err := blobs.Purge(ctx, extendedKey); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteRetentionPurge(ctx, leased.ID, "retention-worker", time.Now().UTC().Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(NewWithBlobStore(store, blobs, schema).Handler())
	defer server.Close()
	getJSON(t, server.Client(), server.URL+"/v1/releases/rel_retention/bundle", http.StatusGone)
}
