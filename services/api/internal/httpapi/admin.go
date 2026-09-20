package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"trajectory.local/api/internal/database"
)

func (api *API) bootstrapCampaign(w http.ResponseWriter, r *http.Request) {
	current, _ := principal(r)
	var input struct {
		OrganizationID      string          `json:"organization_id"`
		OrganizationName    string          `json:"organization_name"`
		ProjectName         string          `json:"project_name"`
		TargetTrajectories  int             `json:"target_trajectories"`
		TemplateName        string          `json:"template_name"`
		Goal                string          `json:"goal"`
		Category            string          `json:"category"`
		Difficulty          string          `json:"difficulty"`
		RequiredApplication string          `json:"required_application"`
		FinishCriteria      string          `json:"finish_criteria"`
		InputAssets         json.RawMessage `json:"input_assets"`
		ExpectedOutputs     json.RawMessage `json:"expected_outputs"`
		ContributorID       string          `json:"contributor_id"`
		ContributorName     string          `json:"contributor_name"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}
	meta, ok := requestMetaOrError(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(input.ProjectName) == "" || input.TargetTrajectories < 1 ||
		strings.TrimSpace(input.TemplateName) == "" || strings.TrimSpace(input.Goal) == "" || strings.TrimSpace(input.Category) == "" ||
		!oneOf(input.Difficulty, "beginner", "intermediate", "advanced") || strings.TrimSpace(input.RequiredApplication) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "complete campaign, task, application, and contributor fields are required")
		return
	}
	if current.Local && (strings.TrimSpace(input.OrganizationName) == "" || strings.TrimSpace(input.ContributorName) == "") {
		writeError(w, http.StatusBadRequest, "invalid_request", "organization_name and contributor_name are required in local mode")
		return
	}
	if !current.Local && (input.OrganizationID == "" || input.ContributorID == "") {
		writeError(w, http.StatusBadRequest, "invalid_request", "organization_id and contributor_id are required in production")
		return
	}
	if len(input.InputAssets) == 0 {
		input.InputAssets = json.RawMessage("[]")
	}
	if len(input.ExpectedOutputs) == 0 {
		input.ExpectedOutputs = json.RawMessage("[]")
	}
	var inputAssets, expectedOutputs []any
	if json.Unmarshal(input.InputAssets, &inputAssets) != nil || json.Unmarshal(input.ExpectedOutputs, &expectedOutputs) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "asset fields must contain JSON arrays")
		return
	}
	specification, err := json.Marshal(map[string]any{
		"required_application": input.RequiredApplication,
		"capture_signals":      []string{"screen", "pointer", "keys"},
		"finish_criteria":      strings.TrimSpace(input.FinishCriteria),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "serialization_failed", "campaign specification could not be created")
		return
	}
	var result database.BootstrappedCampaign
	if current.Local {
		result, err = api.store.BootstrapCampaign(r.Context(), database.CampaignSpec{
			OrganizationName: input.OrganizationName, ProjectName: input.ProjectName, TargetTrajectories: input.TargetTrajectories,
			TemplateName: input.TemplateName, Goal: input.Goal, Category: input.Category, Difficulty: input.Difficulty,
			TemplateSpecification: specification, InputAssets: input.InputAssets, ExpectedOutputs: input.ExpectedOutputs,
			ContributorName: input.ContributorName,
		}, meta.actor, meta.requestID, api.now())
	} else {
		if !current.Access.HasOrganizationRole(input.OrganizationID, "admin") {
			writeError(w, http.StatusForbidden, "forbidden", "administrators may create campaigns only in their own organization")
			return
		}
		result, err = api.store.CreateProductionCampaign(r.Context(), database.ProductionCampaignSpec{
			OrganizationID: input.OrganizationID, ContributorID: input.ContributorID,
			ProjectName: input.ProjectName, TargetTrajectories: input.TargetTrajectories,
			TemplateName: input.TemplateName, Goal: input.Goal, Category: input.Category, Difficulty: input.Difficulty,
			TemplateSpecification: specification, InputAssets: input.InputAssets, ExpectedOutputs: input.ExpectedOutputs,
		}, meta.actor, meta.requestID, api.now())
	}
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"organization_id": result.OrganizationID, "project_id": result.ProjectID, "template_id": result.TemplateID,
		"task_id": result.TaskID, "contributor_id": result.ContributorID, "assignment_id": result.AssignmentID,
		"session_id": result.SessionID, "session_state": "ASSIGNED",
	})
}

func (api *API) listProjects(w http.ResponseWriter, r *http.Request) {
	current, _ := principal(r)
	var projects []database.ProjectSummary
	var err error
	if current.Local {
		projects, err = api.store.ListProjects(r.Context())
	} else {
		projects, err = api.store.ListProjectsForOrganizations(r.Context(), current.Access.OrganizationIDsForRoles("admin", "reviewer"))
	}
	if respondStoreError(w, err) {
		return
	}
	items := make([]map[string]any, 0, len(projects))
	for _, project := range projects {
		items = append(items, map[string]any{
			"id": project.ID, "organization_id": project.OrganizationID, "name": project.Name,
			"target_trajectories": project.TargetTrajectories, "task_count": project.TaskCount,
			"session_count": project.SessionCount,
			"stage_counts": map[string]int{
				"assigned": project.Assigned, "recording": project.Recording,
				"processing": project.Processing, "ready_for_review": project.ReadyForReview,
				"accepted": project.Accepted, "rejected": project.Rejected, "released": project.Released,
				"deleted": project.Deleted,
			},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": items})
}

func (api *API) listOrganizationContributors(w http.ResponseWriter, r *http.Request) {
	organizationID := r.PathValue("organization_id")
	current, _ := principal(r)
	if !current.Local && !current.Access.HasOrganizationRole(organizationID, "admin") {
		writeError(w, http.StatusForbidden, "forbidden", "administrators may view contributors only in their own organization")
		return
	}
	contributors, err := api.store.ListOrganizationContributors(r.Context(), organizationID)
	if respondStoreError(w, err) {
		return
	}
	items := make([]map[string]string, 0, len(contributors))
	for _, contributor := range contributors {
		items = append(items, map[string]string{
			"id": contributor.ID, "display_name": contributor.DisplayName, "email": contributor.Email,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"contributors": items})
}

func (api *API) listProjectSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := api.store.ListProjectSessions(r.Context(), r.PathValue("project_id"))
	if respondStoreError(w, err) {
		return
	}
	items := make([]map[string]any, 0, len(sessions))
	for _, session := range sessions {
		items = append(items, map[string]any{
			"session_id": session.SessionID, "task_id": session.TaskID, "goal": session.Goal,
			"contributor_id": session.ContributorID, "contributor_name": session.ContributorName,
			"state": session.State, "updated_at": session.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": items})
}
