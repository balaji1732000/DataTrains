package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"trajectory.local/api/internal/authn"
	"trajectory.local/api/internal/database"
)

type staticAuthenticator struct{ identity authn.Identity }

func (authenticator staticAuthenticator) Authenticate(context.Context, *http.Request) (authn.Identity, error) {
	return authenticator.identity, nil
}

func TestOIDCIdentityIsMappedServerSideAndRoleChecked(t *testing.T) {
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
		INSERT INTO organizations (id, name) VALUES ('org_auth_test', 'Auth Test');
		INSERT INTO accounts (id, display_name, email) VALUES ('acct_reviewer', 'Review Person', 'reviewer@example.com');
		INSERT INTO oidc_identities (id, account_id, issuer, subject)
		VALUES ('ident_reviewer', 'acct_reviewer', 'https://identity.example.com', 'external-subject');
		INSERT INTO organization_memberships (organization_id, account_id, role)
		VALUES ('org_auth_test', 'acct_reviewer', 'reviewer');`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveOIDCIdentity(ctx, "https://identity.example.com", "external-subject", time.Now()); err != nil {
		t.Fatalf("resolve seeded reviewer: %v", err)
	}

	server := httptest.NewServer(New(store).WithAuthenticator(staticAuthenticator{identity: authn.Identity{
		Issuer: "https://identity.example.com", Subject: "external-subject",
	}}).Handler())
	defer server.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL+"/v1/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Actor-ID", "admin-spoof")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var profile map[string]any
	if err := json.NewDecoder(response.Body).Decode(&profile); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || profile["account_id"] != "acct_reviewer" {
		t.Fatalf("unexpected mapped profile status=%d body=%#v", response.StatusCode, profile)
	}

	request, err = http.NewRequest(http.MethodPost, server.URL+"/v1/organizations", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Actor-ID", "admin-spoof")
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("reviewer used an admin endpoint with status %d", response.StatusCode)
	}
}

func TestUnknownOIDCIdentityFailsClosed(t *testing.T) {
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
	server := httptest.NewServer(New(store).WithAuthenticator(staticAuthenticator{identity: authn.Identity{
		Issuer: "https://identity.example.com", Subject: "not-invited",
	}}).Handler())
	defer server.Close()

	response, err := server.Client().Get(server.URL + "/v1/me")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown identity status=%d, want 403", response.StatusCode)
	}
}

func TestVerifiedOIDCInvitationCreatesContributorProfile(t *testing.T) {
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
		INSERT INTO organizations (id, name) VALUES ('org_invite_test', 'Invite Test');
		INSERT INTO accounts (id, display_name, email) VALUES ('acct_admin', 'Admin', 'admin@example.com');
		INSERT INTO oidc_identities (id, account_id, issuer, subject, email_verified_at_link_time)
		VALUES ('ident_admin', 'acct_admin', 'https://identity.example.com', 'admin-subject', true);
		INSERT INTO organization_memberships (organization_id, account_id, role)
		VALUES ('org_invite_test', 'acct_admin', 'admin');`); err != nil {
		t.Fatal(err)
	}

	adminServer := httptest.NewServer(New(store).WithAuthenticator(staticAuthenticator{identity: authn.Identity{
		Issuer: "https://identity.example.com", Subject: "admin-subject", Email: "admin@example.com", EmailVerified: true,
	}}).Handler())
	invitation := postJSON(t, adminServer.Client(), adminServer.URL+"/v1/invitations", map[string]any{
		"organization_id": "org_invite_test", "email": "professional@example.com",
		"role": "contributor", "expires_in_hours": 24,
	}, http.StatusCreated)
	adminServer.Close()

	contributorServer := httptest.NewServer(New(store).WithAuthenticator(staticAuthenticator{identity: authn.Identity{
		Issuer: "https://identity.example.com", Subject: "professional-subject",
		Email: "professional@example.com", Name: "Professional Contributor", EmailVerified: true,
	}}).Handler())
	defer contributorServer.Close()
	accepted := postJSON(t, contributorServer.Client(), contributorServer.URL+"/v1/invitations/accept", map[string]any{
		"token": invitation["token"],
	}, http.StatusCreated)
	if accepted["role"] != "contributor" || accepted["contributor_id"] == nil {
		t.Fatalf("unexpected invitation acceptance: %#v", accepted)
	}
	profile := getJSON(t, contributorServer.Client(), contributorServer.URL+"/v1/me", http.StatusOK)
	if profile["account_id"] != accepted["account_id"] || profile["contributor_id"] != accepted["contributor_id"] {
		t.Fatalf("invited profile does not resolve: accepted=%#v profile=%#v", accepted, profile)
	}
	postJSON(t, contributorServer.Client(), contributorServer.URL+"/v1/invitations/accept", map[string]any{
		"token": invitation["token"],
	}, http.StatusForbidden)
}

func TestOIDCAdministratorCannotCrossOrganizationBoundary(t *testing.T) {
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
		INSERT INTO organizations (id, name) VALUES
			('org_boundary_a', 'Boundary A'),
			('org_boundary_b', 'Boundary B');
		INSERT INTO accounts (id, display_name, email)
		VALUES ('acct_boundary_admin', 'Boundary Admin', 'boundary-admin@example.com');
		INSERT INTO oidc_identities (id, account_id, issuer, subject, email_verified_at_link_time)
		VALUES ('ident_boundary_admin', 'acct_boundary_admin', 'https://identity.example.com', 'boundary-admin-subject', true);
		INSERT INTO organization_memberships (organization_id, account_id, role)
		VALUES ('org_boundary_a', 'acct_boundary_admin', 'admin');
		INSERT INTO projects (id, organization_id, name, target_trajectories) VALUES
			('project_boundary_a', 'org_boundary_a', 'Visible project', 10),
			('project_boundary_b', 'org_boundary_b', 'Hidden project', 10);`); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(New(store).WithAuthenticator(staticAuthenticator{identity: authn.Identity{
		Issuer: "https://identity.example.com", Subject: "boundary-admin-subject",
		Email: "boundary-admin@example.com", EmailVerified: true,
	}}).Handler())
	defer server.Close()

	projects := getJSON(t, server.Client(), server.URL+"/v1/projects", http.StatusOK)
	items, ok := projects["projects"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("organization-filtered projects = %#v", projects)
	}
	project, ok := items[0].(map[string]any)
	if !ok || project["id"] != "project_boundary_a" {
		t.Fatalf("unexpected visible project = %#v", items[0])
	}

	getJSON(t, server.Client(), server.URL+"/v1/projects/project_boundary_b/sessions", http.StatusForbidden)
	postJSON(t, server.Client(), server.URL+"/v1/invitations", map[string]any{
		"organization_id": "org_boundary_b", "email": "other@example.com",
		"role": "contributor", "expires_in_hours": 24,
	}, http.StatusForbidden)
	policyInput := map[string]any{"raw_days": 30, "derived_days": 90, "release_days": 365}
	putJSON(t, server.Client(), server.URL+"/v1/organizations/org_boundary_b/retention-policy", policyInput, http.StatusForbidden)
	savedPolicy := putJSON(t, server.Client(), server.URL+"/v1/organizations/org_boundary_a/retention-policy", policyInput, http.StatusOK)
	if savedPolicy["raw_days"] != float64(30) || savedPolicy["release_days"] != float64(365) {
		t.Fatalf("saved retention policy = %#v", savedPolicy)
	}
	loadedPolicy := getJSON(t, server.Client(), server.URL+"/v1/organizations/org_boundary_a/retention-policy", http.StatusOK)
	if loadedPolicy["derived_days"] != float64(90) {
		t.Fatalf("loaded retention policy = %#v", loadedPolicy)
	}
}

func TestOIDCAdministratorCreatesAtomicCampaignForInvitedContributor(t *testing.T) {
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
		INSERT INTO organizations (id, name) VALUES ('org_campaign_a', 'Campaign A'), ('org_campaign_b', 'Campaign B');
		INSERT INTO accounts (id, display_name, email) VALUES
			('acct_campaign_admin', 'Admin', 'admin@example.com'),
			('acct_campaign_a', 'Contributor A', 'a@example.com'),
			('acct_campaign_b', 'Contributor B', 'b@example.com');
		INSERT INTO oidc_identities (id, account_id, issuer, subject, email_verified_at_link_time)
		VALUES ('ident_campaign_admin', 'acct_campaign_admin', 'https://identity.example.com', 'campaign-admin', true);
		INSERT INTO organization_memberships (organization_id, account_id, role) VALUES
			('org_campaign_a', 'acct_campaign_admin', 'admin'),
			('org_campaign_a', 'acct_campaign_a', 'contributor'),
			('org_campaign_b', 'acct_campaign_b', 'contributor');
		INSERT INTO contributors (id, display_name, account_id) VALUES
			('contrib_campaign_a', 'Contributor A', 'acct_campaign_a'),
			('contrib_campaign_b', 'Contributor B', 'acct_campaign_b');`); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(store).WithAuthenticator(staticAuthenticator{identity: authn.Identity{
		Issuer: "https://identity.example.com", Subject: "campaign-admin",
	}}).Handler())
	defer server.Close()

	contributors := getJSON(t, server.Client(), server.URL+"/v1/organizations/org_campaign_a/contributors", http.StatusOK)
	items, ok := contributors["contributors"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["id"] != "contrib_campaign_a" {
		t.Fatalf("organization contributor list = %#v", contributors)
	}
	input := map[string]any{
		"organization_id": "org_campaign_a", "contributor_id": "contrib_campaign_a",
		"project_name": "Production project", "target_trajectories": 10,
		"template_name": "Production task", "goal": "Create an output", "category": "desktop.productivity",
		"difficulty": "intermediate", "required_application": "chrome", "finish_criteria": "Save the result",
		"input_assets": []any{}, "expected_outputs": []any{map[string]any{"name": "result.png", "media_type": "image/png"}},
	}
	created := postJSON(t, server.Client(), server.URL+"/v1/campaigns", input, http.StatusCreated)
	if created["project_id"] == nil || created["session_id"] == nil || created["contributor_id"] != "contrib_campaign_a" {
		t.Fatalf("production campaign response = %#v", created)
	}
	input["contributor_id"] = "contrib_campaign_b"
	postJSON(t, server.Client(), server.URL+"/v1/campaigns", input, http.StatusNotFound)
	input["organization_id"] = "org_campaign_b"
	postJSON(t, server.Client(), server.URL+"/v1/campaigns", input, http.StatusForbidden)
}

func TestOIDCAdministratorDeadLetterRecoveryIsOrganizationScoped(t *testing.T) {
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
		INSERT INTO organizations (id, name) VALUES ('org_dlq_api_a', 'DLQ A'), ('org_dlq_api_b', 'DLQ B');
		INSERT INTO accounts (id, display_name, email) VALUES ('acct_dlq_admin', 'DLQ Admin', 'dlq-admin@example.com');
		INSERT INTO oidc_identities (id, account_id, issuer, subject, email_verified_at_link_time)
		VALUES ('ident_dlq_admin', 'acct_dlq_admin', 'https://identity.example.com', 'dlq-admin', true);
		INSERT INTO organization_memberships (organization_id, account_id, role)
		VALUES ('org_dlq_api_a', 'acct_dlq_admin', 'admin');
		INSERT INTO projects (id, organization_id, name, target_trajectories) VALUES
		  ('proj_dlq_api_a', 'org_dlq_api_a', 'Project A', 1),
		  ('proj_dlq_api_b', 'org_dlq_api_b', 'Project B', 1);
		INSERT INTO task_templates (id, project_id, name, goal, category, difficulty, specification) VALUES
		  ('tmpl_dlq_api_a', 'proj_dlq_api_a', 'Template A', 'Recover A', 'test', 'beginner', '{}'),
		  ('tmpl_dlq_api_b', 'proj_dlq_api_b', 'Template B', 'Recover B', 'test', 'beginner', '{}');
		INSERT INTO tasks (id, template_id, goal) VALUES
		  ('task_dlq_api_a', 'tmpl_dlq_api_a', 'Recover A'),
		  ('task_dlq_api_b', 'tmpl_dlq_api_b', 'Recover B');
		INSERT INTO contributors (id, display_name) VALUES
		  ('contrib_dlq_api_a', 'Contributor A'), ('contrib_dlq_api_b', 'Contributor B'),
		  ('contrib_erase_api_a', 'Erase Contributor A'), ('contrib_erase_api_b', 'Erase Contributor B');
		INSERT INTO assignments (id, task_id, contributor_id) VALUES
		  ('assign_dlq_api_a', 'task_dlq_api_a', 'contrib_dlq_api_a'),
		  ('assign_dlq_api_b', 'task_dlq_api_b', 'contrib_dlq_api_b'),
		  ('assign_erase_api_a', 'task_dlq_api_a', 'contrib_erase_api_a'),
		  ('assign_erase_api_b', 'task_dlq_api_b', 'contrib_erase_api_b');
		INSERT INTO sessions (id, assignment_id, state) VALUES
		  ('sess_dlq_api_a', 'assign_dlq_api_a', 'FAILED'),
		  ('sess_dlq_api_b', 'assign_dlq_api_b', 'FAILED'),
		  ('sess_erase_api_a', 'assign_erase_api_a', 'FAILED'),
		  ('sess_erase_api_b', 'assign_erase_api_b', 'FAILED');
		INSERT INTO processing_jobs
		  (id, session_id, job_type, state, attempt, available_at, finished_at, dead_lettered_at, updated_at, error) VALUES
		  ('job_dlq_api_a', 'sess_dlq_api_a', 'validate_trajectory', 'dead_letter', 3, now(), now(), now(), now(), 'A failed'),
		  ('job_dlq_api_b', 'sess_dlq_api_b', 'validate_trajectory', 'dead_letter', 3, now(), now(), now(), now(), 'B failed');
		INSERT INTO deletion_requests
		  (id, organization_id, session_id, reason, object_keys, state, attempt, available_at,
		   requested_by, requested_at, updated_at, last_error) VALUES
		  ('delete_dlq_api_a', 'org_dlq_api_a', 'sess_erase_api_a', 'Erase A', '[]', 'dead_letter', 3, now(), 'acct_dlq_admin', now(), now(), 'R2 A failed'),
		  ('delete_dlq_api_b', 'org_dlq_api_b', 'sess_erase_api_b', 'Erase B', '[]', 'dead_letter', 3, now(), 'acct_dlq_admin', now(), now(), 'R2 B failed');
		INSERT INTO retention_purge_requests
		  (id, organization_id, resource_type, resource_id, policy_updated_at, object_keys,
		   state, attempt, available_at, requested_at, updated_at, last_error) VALUES
		  ('retention_dlq_api_a', 'org_dlq_api_a', 'session_raw', 'sess_erase_api_a', now(), '[]', 'dead_letter', 3, now(), now(), now(), 'R2 retention A failed'),
		  ('retention_dlq_api_b', 'org_dlq_api_b', 'session_raw', 'sess_erase_api_b', now(), '[]', 'dead_letter', 3, now(), now(), now(), 'R2 retention B failed');`); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(New(store).WithAuthenticator(staticAuthenticator{identity: authn.Identity{
		Issuer: "https://identity.example.com", Subject: "dlq-admin",
	}}).Handler())
	defer server.Close()

	listed := getJSON(t, server.Client(), server.URL+"/v1/processing-jobs/dead-letter", http.StatusOK)
	jobs, ok := listed["jobs"].([]any)
	if !ok || len(jobs) != 1 || jobs[0].(map[string]any)["id"] != "job_dlq_api_a" {
		t.Fatalf("organization-scoped dead-letter jobs = %#v", listed)
	}
	postJSON(t, server.Client(), server.URL+"/v1/processing-jobs/job_dlq_api_b/retry", map[string]any{}, http.StatusForbidden)
	recovered := postJSON(t, server.Client(), server.URL+"/v1/processing-jobs/job_dlq_api_a/retry", map[string]any{}, http.StatusOK)
	if recovered["state"] != "queued" || recovered["manual_requeues"] != float64(1) {
		t.Fatalf("recovered job = %#v", recovered)
	}

	deletions := getJSON(t, server.Client(), server.URL+"/v1/deletion-requests", http.StatusOK)
	deletionItems, ok := deletions["deletion_requests"].([]any)
	if !ok || len(deletionItems) != 1 || deletionItems[0].(map[string]any)["id"] != "delete_dlq_api_a" {
		t.Fatalf("organization-scoped deletion requests = %#v", deletions)
	}
	postJSON(t, server.Client(), server.URL+"/v1/deletion-requests/delete_dlq_api_b/retry", map[string]any{}, http.StatusForbidden)
	retriedDeletion := postJSON(t, server.Client(), server.URL+"/v1/deletion-requests/delete_dlq_api_a/retry", map[string]any{}, http.StatusOK)
	if retriedDeletion["state"] != "queued" || retriedDeletion["manual_requeues"] != float64(1) {
		t.Fatalf("recovered deletion request = %#v", retriedDeletion)
	}
	retentionPurges := getJSON(t, server.Client(), server.URL+"/v1/retention-purge-requests", http.StatusOK)
	retentionItems, ok := retentionPurges["retention_purge_requests"].([]any)
	if !ok || len(retentionItems) != 1 || retentionItems[0].(map[string]any)["id"] != "retention_dlq_api_a" {
		t.Fatalf("organization-scoped retention purges = %#v", retentionPurges)
	}
	postJSON(t, server.Client(), server.URL+"/v1/retention-purge-requests/retention_dlq_api_b/retry", map[string]any{}, http.StatusForbidden)
	retriedRetention := postJSON(t, server.Client(), server.URL+"/v1/retention-purge-requests/retention_dlq_api_a/retry", map[string]any{}, http.StatusOK)
	if retriedRetention["state"] != "queued" || retriedRetention["attempt"] != float64(0) {
		t.Fatalf("recovered retention purge = %#v", retriedRetention)
	}
	postJSON(t, server.Client(), server.URL+"/v1/sessions/sess_erase_api_b/legal-holds", map[string]any{
		"reason": "Cross-organization hold must fail",
	}, http.StatusForbidden)
}
