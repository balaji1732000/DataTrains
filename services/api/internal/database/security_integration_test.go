package database

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestRuntimeRoleIsLeastPrivilegeAndRLSEnabled(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	store, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := Migrate(ctx, store.Pool(), "../../migrations"); err != nil {
		t.Fatal(err)
	}

	var superuser, createRole, createDatabase, bypassRLS bool
	if err := store.Pool().QueryRow(ctx, `
		SELECT rolsuper, rolcreaterole, rolcreatedb, rolbypassrls
		FROM pg_roles WHERE rolname='datatrains_runtime'`).Scan(
		&superuser, &createRole, &createDatabase, &bypassRLS,
	); err != nil {
		t.Fatal(err)
	}
	if superuser || createRole || createDatabase || bypassRLS {
		t.Fatal("runtime database role has administrative privileges")
	}

	var missingRLS int
	if err := store.Pool().QueryRow(ctx, `
		SELECT count(*)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname='public' AND c.relkind='r'
		  AND c.relname <> 'schema_migrations' AND NOT c.relrowsecurity`).Scan(&missingRLS); err != nil {
		t.Fatal(err)
	}
	if missingRLS != 0 {
		t.Fatalf("%d application tables do not have row-level security enabled", missingRLS)
	}

	var canUpdateAudit, canDeleteAudit, canInsertAudit bool
	if err := store.Pool().QueryRow(ctx, `
		SELECT
		  has_table_privilege('datatrains_runtime', 'audit_events', 'UPDATE'),
		  has_table_privilege('datatrains_runtime', 'audit_events', 'DELETE'),
		  has_table_privilege('datatrains_runtime', 'audit_events', 'INSERT')`).Scan(
		&canUpdateAudit, &canDeleteAudit, &canInsertAudit,
	); err != nil {
		t.Fatal(err)
	}
	if canUpdateAudit || canDeleteAudit || !canInsertAudit {
		t.Fatal("runtime audit-event privileges are not append-only")
	}

	tx, err := store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE datatrains_runtime`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organizations (id, name) VALUES ('org_runtime_rls_test', 'Runtime RLS test')`); err != nil {
		t.Fatalf("runtime role cannot use its explicit RLS policy: %v", err)
	}
}

func TestDeadLetterJobCanBeRecoveredOnlyThroughAuditedManualRetry(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	store, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := Migrate(ctx, store.Pool(), "../../migrations"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `
		TRUNCATE organizations, accounts, audit_events RESTART IDENTITY CASCADE;
		INSERT INTO organizations (id, name) VALUES ('org_dlq_a', 'DLQ A'), ('org_dlq_b', 'DLQ B');
		INSERT INTO projects (id, organization_id, name, target_trajectories)
		VALUES ('proj_dlq', 'org_dlq_a', 'DLQ Project', 1);
		INSERT INTO task_templates (id, project_id, name, goal, category, difficulty, specification)
		VALUES ('tmpl_dlq', 'proj_dlq', 'DLQ Template', 'Test recovery', 'test', 'beginner', '{}');
		INSERT INTO tasks (id, template_id, goal) VALUES ('task_dlq', 'tmpl_dlq', 'Test recovery');
		INSERT INTO contributors (id, display_name) VALUES ('contrib_dlq', 'DLQ Contributor');
		INSERT INTO assignments (id, task_id, contributor_id) VALUES ('assign_dlq', 'task_dlq', 'contrib_dlq');
		INSERT INTO sessions (id, assignment_id, state) VALUES ('sess_dlq', 'assign_dlq', 'FAILED');
		INSERT INTO processing_jobs
		  (id, session_id, job_type, state, attempt, available_at, finished_at, dead_lettered_at, updated_at, error)
		VALUES
		  ('job_dlq', 'sess_dlq', 'validate_trajectory', 'dead_letter', 3, now(), now(), now(), now(), 'storage unavailable');`); err != nil {
		t.Fatal(err)
	}

	visible, err := store.ListDeadLetterJobs(ctx, []string{"org_dlq_a"})
	if err != nil || len(visible) != 1 || visible[0].ID != "job_dlq" {
		t.Fatalf("dead-letter list = %#v, %v", visible, err)
	}
	hidden, err := store.ListDeadLetterJobs(ctx, []string{"org_dlq_b"})
	if err != nil || len(hidden) != 0 {
		t.Fatalf("cross-organization dead-letter list = %#v, %v", hidden, err)
	}

	now := time.Now().UTC()
	job, err := store.RetryDeadLetterJob(ctx, "job_dlq", "acct_admin", "req_manual_retry", now)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "queued" || job.Attempt != 0 || job.ManualRequeues != 1 || job.LastError != "" {
		t.Fatalf("recovered job = %#v", job)
	}
	var sessionState string
	if err := store.Pool().QueryRow(ctx, `SELECT state FROM sessions WHERE id='sess_dlq'`).Scan(&sessionState); err != nil {
		t.Fatal(err)
	}
	if sessionState != "PROCESSING" {
		t.Fatalf("recovered session state = %s", sessionState)
	}
	var auditCount int
	if err := store.Pool().QueryRow(ctx, `
		SELECT count(*) FROM audit_events
		WHERE request_id='req_manual_retry'
		  AND action IN ('processing_job.manual_retry', 'session.processing_recovered')`).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 2 {
		t.Fatalf("manual retry audit event count = %d", auditCount)
	}
	if _, err := store.RetryDeadLetterJob(ctx, "job_dlq", "acct_admin", "req_again", now); err != ErrJobNotDeadLetter {
		t.Fatalf("second retry error = %v", err)
	}
}

func TestRedactionDeadLetterRecoveryIsOrganizationScopedAndAudited(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	store, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := Migrate(ctx, store.Pool(), "../../migrations"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `
		TRUNCATE organizations, accounts, audit_events RESTART IDENTITY CASCADE;
		INSERT INTO organizations (id,name) VALUES ('org_redaction_dlq_a','Redaction A'),('org_redaction_dlq_b','Redaction B');
		INSERT INTO projects (id,organization_id,name,target_trajectories) VALUES ('proj_redaction_dlq','org_redaction_dlq_a','Redaction project',1);
		INSERT INTO task_templates (id,project_id,name,goal,category,difficulty,specification)
		VALUES ('tmpl_redaction_dlq','proj_redaction_dlq','Template','Test redaction recovery','test','beginner','{}');
		INSERT INTO tasks (id,template_id,goal) VALUES ('task_redaction_dlq','tmpl_redaction_dlq','Test recovery');
		INSERT INTO contributors (id,display_name) VALUES ('contrib_redaction_dlq','Contributor');
		INSERT INTO assignments (id,task_id,contributor_id) VALUES ('assign_redaction_dlq','task_redaction_dlq','contrib_redaction_dlq');
		INSERT INTO sessions (id,assignment_id,state) VALUES ('sess_redaction_dlq','assign_redaction_dlq','ACCEPTED');
		INSERT INTO redaction_plans (id,session_id,reviewer_id,version,schema_version,document,document_hash,request_id)
		VALUES ('plan_redaction_dlq','sess_redaction_dlq','reviewer',1,'redaction/v1',
		'{"schema_version":"redaction/v1","segments":[{"segment_id":"000001","source_key":"raw/sessions/sess_redaction_dlq/video/000001.mp4","decision":"publish_as_is","regions":[]}]}',
		repeat('a',64),'req-plan-redaction-dlq');
		INSERT INTO redaction_jobs (id,plan_id,session_id,state,attempt,available_at,error,created_at,updated_at,finished_at,dead_lettered_at)
		VALUES ('job_redaction_dlq','plan_redaction_dlq','sess_redaction_dlq','dead_letter',3,now(),'ffmpeg failed',now(),now(),now(),now());`); err != nil {
		t.Fatal(err)
	}
	visible, err := store.ListDeadLetterRedactionJobs(ctx, []string{"org_redaction_dlq_a"})
	if err != nil || len(visible) != 1 || visible[0].ID != "job_redaction_dlq" {
		t.Fatalf("redaction dead letters = %#v err=%v", visible, err)
	}
	hidden, err := store.ListDeadLetterRedactionJobs(ctx, []string{"org_redaction_dlq_b"})
	if err != nil || len(hidden) != 0 {
		t.Fatalf("cross-organization redaction dead letters = %#v err=%v", hidden, err)
	}
	organizationID, err := store.GetRedactionJobOrganization(ctx, "job_redaction_dlq")
	if err != nil || organizationID != "org_redaction_dlq_a" {
		t.Fatalf("redaction job organization=%q err=%v", organizationID, err)
	}
	if _, err := store.Pool().Exec(ctx, `UPDATE redaction_plans SET document_hash=repeat('b',64) WHERE id='plan_redaction_dlq'`); err == nil {
		t.Fatal("immutable redaction plan accepted an in-place update")
	}
	now := time.Now().UTC()
	job, err := store.RetryDeadLetterRedactionJob(ctx, "job_redaction_dlq", "admin", "req-retry-redaction", now)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "queued" || job.Attempt != 0 || job.ManualRequeues != 1 || job.LastError != "" {
		t.Fatalf("retried redaction job = %#v", job)
	}
	if _, err := store.RequestSessionDeletion(ctx, "sess_redaction_dlq", "privacy request while redaction runs", "admin", "req-delete-during-redaction", now); err != ErrRedactionInProgress {
		t.Fatalf("deletion during redaction error=%v", err)
	}
	var auditCount int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE request_id='req-retry-redaction' AND action='redaction_job.manual_retry'`).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("redaction retry audit count=%d err=%v", auditCount, err)
	}
	if _, err := store.RetryDeadLetterRedactionJob(ctx, "job_redaction_dlq", "admin", "req-again", now); err != ErrRedactionNotDead {
		t.Fatalf("second redaction retry error=%v", err)
	}
}
