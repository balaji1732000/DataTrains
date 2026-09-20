"use client";

import { useCallback, useEffect, useState } from "react";

type Project = {
  id: string;
  organization_id: string;
  name: string;
  target_trajectories: number;
  task_count: number;
  session_count: number;
  stage_counts: Record<string, number>;
};

type ProjectSession = {
  session_id: string;
  task_id: string;
  goal: string;
  contributor_id: string;
  contributor_name: string;
  state: string;
  updated_at: string;
};

type CreatedCampaign = {
  projectID: string;
  taskID: string;
  contributorID: string;
  assignmentID: string;
  sessionID: string;
};

type Membership = { organization_id: string; role: string };
type Contributor = { id: string; display_name: string; email?: string };
type DeadLetterJob = {
  id: string;
  session_id: string;
  job_type: string;
  state: "dead_letter";
  attempt: number;
  manual_requeues?: number;
  last_error?: string;
  dead_lettered_at?: string;
};
type RedactionDeadLetterJob = {
  id: string;
  plan_id: string;
  session_id: string;
  state: "dead_letter";
  attempt: number;
  manual_requeues?: number;
  last_error?: string;
  dead_lettered_at?: string;
};
type LegalHold = { id: string; session_id: string; reason: string; placed_at: string };
type DeletionRequest = { id: string; session_id: string; state: string; reason: string; attempt: number; manual_requeues?: number; last_error?: string };
type RetentionPolicy = { organization_id: string; raw_days: number; derived_days: number; release_days: number; updated_at?: string };
type RetentionPurgeRequest = { id: string; resource_type: string; resource_id: string; state: string; attempt: number; object_count: number; last_error?: string };

const initialForm = {
  organization: "Local Dataset Lab",
  project: "Desktop Tasks V1",
  target: 100,
  template: "Task template",
  goal: "Complete the assigned task and save the expected output.",
  category: "desktop.productivity",
  difficulty: "intermediate",
  requiredApplication: "example.exe",
  inputAssets: "inputs/example.png",
  outputName: "result.png",
  outputMediaType: "image/png",
  finishCriteria: "Save the expected output and verify it opens correctly.",
  contributor: "Local contributor",
};

export function OperationsWorkspace({ identityMode = false }: { identityMode?: boolean }) {
  const [projects, setProjects] = useState<Project[]>([]);
  const [sessions, setSessions] = useState<ProjectSession[]>([]);
  const [selectedProject, setSelectedProject] = useState<Project | null>(null);
  const [actor, setActor] = useState(identityMode ? "" : "admin-local");
  const [memberships, setMemberships] = useState<Membership[]>([]);
  const [organizationID, setOrganizationID] = useState("");
  const [contributors, setContributors] = useState<Contributor[]>([]);
  const [deadLetterJobs, setDeadLetterJobs] = useState<DeadLetterJob[]>([]);
  const [redactionDeadLetters, setRedactionDeadLetters] = useState<RedactionDeadLetterJob[]>([]);
  const [legalHolds, setLegalHolds] = useState<LegalHold[]>([]);
  const [deletionRequests, setDeletionRequests] = useState<DeletionRequest[]>([]);
  const [retentionPurges, setRetentionPurges] = useState<RetentionPurgeRequest[]>([]);
  const [retentionPolicy, setRetentionPolicy] = useState<RetentionPolicy | null>(null);
  const [retentionSaved, setRetentionSaved] = useState(false);
  const [contributorID, setContributorID] = useState("");
  const [inviteEmail, setInviteEmail] = useState("");
  const [inviteRole, setInviteRole] = useState("contributor");
  const [invitationURL, setInvitationURL] = useState<string | null>(null);
  const [form, setForm] = useState(initialForm);
  const [releaseName, setReleaseName] = useState("release-v1");
  const [releaseProfile, setReleaseProfile] = useState<"trajectory_only" | "redacted_video">("trajectory_only");
  const [created, setCreated] = useState<CreatedCampaign | null>(null);
  const [manifestURL, setManifestURL] = useState<string | null>(null);
  const [bundleURL, setBundleURL] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const loadProjects = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const [response, deadLetters, redactionFailures, holds, deletions, purges] = await Promise.all([
        requestJSON<{ projects: Project[] }>("/api/control-plane/v1/projects", { cache: "no-store" }),
        requestJSON<{ jobs: DeadLetterJob[] }>("/api/control-plane/v1/processing-jobs/dead-letter", { cache: "no-store" }),
        requestJSON<{ jobs: RedactionDeadLetterJob[] }>("/api/control-plane/v1/redaction-jobs/dead-letter", { cache: "no-store" }),
        requestJSON<{ legal_holds: LegalHold[] }>("/api/control-plane/v1/legal-holds", { cache: "no-store" }),
        requestJSON<{ deletion_requests: DeletionRequest[] }>("/api/control-plane/v1/deletion-requests", { cache: "no-store" }),
        requestJSON<{ retention_purge_requests: RetentionPurgeRequest[] }>("/api/control-plane/v1/retention-purge-requests", { cache: "no-store" }),
      ]);
      setProjects(response.projects);
      setDeadLetterJobs(deadLetters.jobs);
      setRedactionDeadLetters(redactionFailures.jobs);
      setLegalHolds(holds.legal_holds);
      setDeletionRequests(deletions.deletion_requests);
      setRetentionPurges(purges.retention_purge_requests);
    } catch (caught) {
      setError(messageFor(caught));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    let active = true;
    const identity = identityMode
      ? requestJSON<{ account_id: string; memberships: Membership[] }>("/api/control-plane/v1/me", { cache: "no-store" })
      : Promise.resolve(null);
    void Promise.all([
      requestJSON<{ projects: Project[] }>("/api/control-plane/v1/projects", { cache: "no-store" }),
      requestJSON<{ jobs: DeadLetterJob[] }>("/api/control-plane/v1/processing-jobs/dead-letter", { cache: "no-store" }),
      requestJSON<{ jobs: RedactionDeadLetterJob[] }>("/api/control-plane/v1/redaction-jobs/dead-letter", { cache: "no-store" }),
      requestJSON<{ legal_holds: LegalHold[] }>("/api/control-plane/v1/legal-holds", { cache: "no-store" }),
      requestJSON<{ deletion_requests: DeletionRequest[] }>("/api/control-plane/v1/deletion-requests", { cache: "no-store" }),
      requestJSON<{ retention_purge_requests: RetentionPurgeRequest[] }>("/api/control-plane/v1/retention-purge-requests", { cache: "no-store" }),
      identity,
    ])
      .then(([response, deadLetters, redactionFailures, holds, deletions, purges, current]) => {
        if (active) {
          setProjects(response.projects);
          setDeadLetterJobs(deadLetters.jobs);
          setRedactionDeadLetters(redactionFailures.jobs);
          setLegalHolds(holds.legal_holds);
          setDeletionRequests(deletions.deletion_requests);
          setRetentionPurges(purges.retention_purge_requests);
          if (current) {
            setActor(current.account_id);
            const adminMemberships = current.memberships.filter((membership) => membership.role === "admin");
            setMemberships(adminMemberships);
            setOrganizationID((existing) => existing || adminMemberships[0]?.organization_id || "");
          }
        }
      })
      .catch((caught: unknown) => { if (active) setError(messageFor(caught)); })
      .finally(() => { if (active) setLoading(false); });
    return () => { active = false; };
  }, [identityMode]);

  useEffect(() => {
    if (!identityMode || !organizationID) return;
    let active = true;
    void requestJSON<{ contributors: Contributor[] }>(`/api/control-plane/v1/organizations/${encodeURIComponent(organizationID)}/contributors`, { cache: "no-store" })
      .then((response) => {
        if (!active) return;
        setContributors(response.contributors);
        setContributorID((existing) => response.contributors.some((item) => item.id === existing) ? existing : response.contributors[0]?.id || "");
      })
      .catch((caught: unknown) => { if (active) setError(messageFor(caught)); });
    return () => { active = false; };
  }, [identityMode, organizationID]);

  function updateForm<Key extends keyof typeof initialForm>(key: Key, value: (typeof initialForm)[Key]) {
    setForm((current) => ({ ...current, [key]: value }));
  }

  async function createCampaign(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!actor.trim() || (identityMode && (!organizationID || !contributorID))) {
      setError(identityMode ? "Select an organization and an active contributor." : "Enter an administrator actor ID.");
      return;
    }
    setBusy(true);
    setError(null);
    setCreated(null);
    try {
      const campaign = await post<{
        project_id: string; task_id: string; contributor_id: string; assignment_id: string; session_id: string;
      }>("/v1/campaigns", {
        ...(identityMode ? { organization_id: organizationID } : { organization_name: form.organization }),
        project_name: form.project,
        target_trajectories: form.target,
        template_name: form.template,
        goal: form.goal,
        category: form.category,
        difficulty: form.difficulty,
        required_application: form.requiredApplication,
        input_assets: form.inputAssets.split(",").map((key) => key.trim()).filter(Boolean).map((key) => ({ key })),
        expected_outputs: [{ name: form.outputName, media_type: form.outputMediaType }],
        finish_criteria: form.finishCriteria,
        ...(identityMode ? { contributor_id: contributorID } : { contributor_name: form.contributor }),
      }, actor);
      setCreated({
        projectID: campaign.project_id, taskID: campaign.task_id, contributorID: campaign.contributor_id,
        assignmentID: campaign.assignment_id, sessionID: campaign.session_id,
      });
      await loadProjects();
    } catch (caught) {
      setError(messageFor(caught));
    } finally {
      setBusy(false);
    }
  }

  async function createInvitation(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!organizationID || !inviteEmail.trim()) return;
    setBusy(true);
    setError(null);
    setInvitationURL(null);
    try {
      const invitation = await post<{ token: string }>("/v1/invitations", {
        organization_id: organizationID,
        email: inviteEmail.trim(),
        role: inviteRole,
        expires_in_hours: 72,
      }, actor);
      const link = new URL("/api/invitations/start", window.location.origin);
      link.searchParams.set("token", invitation.token);
      setInvitationURL(link.href);
      setInviteEmail("");
    } catch (caught) {
      setError(messageFor(caught));
    } finally {
      setBusy(false);
    }
  }

  async function inspectProject(project: Project) {
    setSelectedProject(project);
    setManifestURL(null);
    setBundleURL(null);
    setRetentionSaved(false);
    setError(null);
    try {
      const [response, policy] = await Promise.all([
        requestJSON<{ sessions: ProjectSession[] }>(`/api/control-plane/v1/projects/${encodeURIComponent(project.id)}/sessions`, { cache: "no-store" }),
        requestOptionalJSON<RetentionPolicy>(`/api/control-plane/v1/organizations/${encodeURIComponent(project.organization_id)}/retention-policy`),
      ]);
      setSessions(response.sessions);
      setRetentionPolicy(policy ?? { organization_id: project.organization_id, raw_days: 30, derived_days: 90, release_days: 365 });
    } catch (caught) {
      setError(messageFor(caught));
    }
  }

  async function createRelease() {
    if (!selectedProject) return;
    if (releaseProfile === "redacted_video" && !window.confirm("Publish only the latest completed reviewer-approved redacted videos? Raw recordings will remain private.")) return;
    setBusy(true);
    setError(null);
    try {
      const response = await post<{ release: { manifest_url: string; bundle_url: string } }>(`/v1/projects/${encodeURIComponent(selectedProject.id)}/releases`, { name: releaseName, profile: releaseProfile, session_ids: [] }, actor);
      setManifestURL(`/api/control-plane${response.release.manifest_url}`);
      setBundleURL(`/api/control-plane${response.release.bundle_url}`);
      await Promise.all([loadProjects(), inspectProject(selectedProject)]);
    } catch (caught) {
      setError(messageFor(caught));
    } finally {
      setBusy(false);
    }
  }

  async function retryDeadLetter(jobID: string) {
    setBusy(true);
    setError(null);
    try {
      await post(`/v1/processing-jobs/${encodeURIComponent(jobID)}/retry`, {}, actor);
      await loadProjects();
    } catch (caught) {
      setError(messageFor(caught));
    } finally {
      setBusy(false);
    }
  }

  async function retryRedaction(jobID: string) {
    setBusy(true);
    setError(null);
    try {
      await post(`/v1/redaction-jobs/${encodeURIComponent(jobID)}/retry`, {}, actor);
      await loadProjects();
    } catch (caught) {
      setError(messageFor(caught));
    } finally {
      setBusy(false);
    }
  }

  async function placeLegalHold(sessionID: string) {
    const reason = window.prompt("Why must this session be preserved? This reason becomes part of the audit record.");
    if (!reason?.trim()) return;
    setBusy(true);
    setError(null);
    try {
      await post(`/v1/sessions/${encodeURIComponent(sessionID)}/legal-holds`, { reason: reason.trim() }, actor);
      await loadProjects();
    } catch (caught) {
      setError(messageFor(caught));
    } finally {
      setBusy(false);
    }
  }

  async function releaseLegalHold(sessionID: string, holdID: string) {
    if (!window.confirm("Release this legal hold? A queued erasure may become eligible immediately.")) return;
    setBusy(true);
    setError(null);
    try {
      await remove(`/v1/sessions/${encodeURIComponent(sessionID)}/legal-holds/${encodeURIComponent(holdID)}`, actor);
      await loadProjects();
    } catch (caught) {
      setError(messageFor(caught));
    } finally {
      setBusy(false);
    }
  }

  async function requestDeletion(sessionID: string) {
    const reason = window.prompt("Enter the approved retention or privacy-erasure reason.");
    if (!reason?.trim()) return;
    if (!window.confirm(`Queue permanent artifact erasure for ${sessionID}? This cannot be undone after the worker completes it.`)) return;
    setBusy(true);
    setError(null);
    try {
      await post(`/v1/sessions/${encodeURIComponent(sessionID)}/deletion-requests`, { reason: reason.trim() }, actor);
      await loadProjects();
    } catch (caught) {
      setError(messageFor(caught));
    } finally {
      setBusy(false);
    }
  }

  async function retryDeletion(requestID: string) {
	  if (!window.confirm("Retry this failed permanent erasure? The legal-hold check will run again before any object is removed.")) return;
	  setBusy(true);
	  setError(null);
	  try {
	    await post(`/v1/deletion-requests/${encodeURIComponent(requestID)}/retry`, {}, actor);
	    await loadProjects();
	  } catch (caught) {
	    setError(messageFor(caught));
	  } finally {
	    setBusy(false);
	  }
  }

  async function retryRetentionPurge(requestID: string) {
    if (!window.confirm("Retry this failed retention purge? Current policy and legal holds will be checked again before deletion.")) return;
    setBusy(true);
    setError(null);
    try {
      await post(`/v1/retention-purge-requests/${encodeURIComponent(requestID)}/retry`, {}, actor);
      await loadProjects();
    } catch (caught) {
      setError(messageFor(caught));
    } finally {
      setBusy(false);
    }
  }

  async function saveRetentionPolicy(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!selectedProject || !retentionPolicy) return;
    setBusy(true);
    setError(null);
    setRetentionSaved(false);
    try {
      const saved = await put<RetentionPolicy>(`/v1/organizations/${encodeURIComponent(selectedProject.organization_id)}/retention-policy`, {
        raw_days: retentionPolicy.raw_days,
        derived_days: retentionPolicy.derived_days,
        release_days: retentionPolicy.release_days,
      }, actor);
      setRetentionPolicy(saved);
      setRetentionSaved(true);
    } catch (caught) {
      setError(messageFor(caught));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="operationsLayout">
      <section className="panel setupPanel">
        {identityMode ? (
          <>
            <div className="panelHeading"><div><p className="eyebrow">Team access</p><h2>Invite a professional</h2></div><span>Link expires in 72 hours</span></div>
            <form onSubmit={(event) => void createInvitation(event)}>
              <div className="formGrid">
                <label><span>Organization</span><select required value={organizationID} onChange={(event) => setOrganizationID(event.target.value)}>{memberships.map((membership) => <option key={membership.organization_id} value={membership.organization_id}>{membership.organization_id}</option>)}</select></label>
                <label><span>Role</span><select value={inviteRole} onChange={(event) => setInviteRole(event.target.value)}><option value="contributor">Contributor</option><option value="reviewer">Reviewer</option><option value="admin">Administrator</option></select></label>
              </div>
              <label className="commentField"><span>Professional email</span><input required type="email" autoComplete="email" value={inviteEmail} onChange={(event) => setInviteEmail(event.target.value)} placeholder="person@company.com" /></label>
              <button type="submit" disabled={busy || !organizationID}>{busy ? "Creating…" : "Create secure invitation"}</button>
            </form>
            {invitationURL ? (
              <div className="successMessage">
                <strong>Invitation ready.</strong>
                <span>Send this one-time link only to the invited person.</span>
                <input readOnly value={invitationURL} aria-label="Invitation link" />
                <button className="secondary" type="button" onClick={() => void navigator.clipboard.writeText(invitationURL)}>Copy link</button>
              </div>
            ) : null}
            <div className="sectionDivider" />
            <div className="panelHeading"><div><p className="eyebrow">New collection</p><h2>Create campaign</h2></div><span>Atomic project + assignment</span></div>
            <form onSubmit={(event) => void createCampaign(event)}>
              <div className="formGrid">
                <label><span>Contributor</span><select required value={contributorID} onChange={(event) => setContributorID(event.target.value)}><option value="">Select contributor</option>{contributors.map((contributor) => <option key={contributor.id} value={contributor.id}>{contributor.display_name}{contributor.email ? ` · ${contributor.email}` : ""}</option>)}</select></label>
                <label><span>Project</span><input required value={form.project} onChange={(event) => updateForm("project", event.target.value)} /></label>
                <label><span>Target trajectories</span><input required min={1} type="number" value={form.target} onChange={(event) => updateForm("target", Number(event.target.value))} /></label>
                <label><span>Template name</span><input required value={form.template} onChange={(event) => updateForm("template", event.target.value)} /></label>
                <label><span>Category</span><input required value={form.category} onChange={(event) => updateForm("category", event.target.value)} /></label>
                <label><span>Difficulty</span><select value={form.difficulty} onChange={(event) => updateForm("difficulty", event.target.value)}><option value="beginner">Beginner</option><option value="intermediate">Intermediate</option><option value="advanced">Advanced</option></select></label>
                <label><span>Required process</span><input required value={form.requiredApplication} onChange={(event) => updateForm("requiredApplication", event.target.value)} /></label>
                <label><span>Input asset keys</span><input value={form.inputAssets} onChange={(event) => updateForm("inputAssets", event.target.value)} placeholder="inputs/example.png, inputs/brief.pdf" /></label>
                <label><span>Expected output</span><input required value={form.outputName} onChange={(event) => updateForm("outputName", event.target.value)} /></label>
                <label><span>Output media type</span><input required value={form.outputMediaType} onChange={(event) => updateForm("outputMediaType", event.target.value)} /></label>
              </div>
              <label className="commentField"><span>Task goal</span><textarea required rows={3} value={form.goal} onChange={(event) => updateForm("goal", event.target.value)} /></label>
              <label className="commentField"><span>Finish criteria</span><textarea required rows={2} value={form.finishCriteria} onChange={(event) => updateForm("finishCriteria", event.target.value)} /></label>
              <button type="submit" disabled={busy || !contributorID}>{busy ? "Creating…" : "Create project & assignment"}</button>
            </form>
            {!contributors.length ? <p className="subtitle">Invite a contributor above. After they accept, reload this page to assign their first task.</p> : null}
            {created ? <div className="successMessage"><strong>Assignment ready.</strong><span>Session {created.sessionID}</span><small>Project {created.projectID} · Task {created.taskID} · Contributor {created.contributorID}</small></div> : null}
          </>
        ) : (
          <>
        <div className="panelHeading"><div><p className="eyebrow">New collection</p><h2>Bootstrap campaign</h2></div><span>Creates 1 assignment</span></div>
        <form onSubmit={(event) => void createCampaign(event)}>
          <div className="formGrid">
            {identityMode ? <div className="identityField"><span>Administrator account</span><code>{actor || "Loading identity…"}</code></div> : (
              <label><span>Administrator actor ID</span><input value={actor} onChange={(event) => setActor(event.target.value)} /></label>
            )}
            <label><span>Organization</span><input required value={form.organization} onChange={(event) => updateForm("organization", event.target.value)} /></label>
            <label><span>Project</span><input required value={form.project} onChange={(event) => updateForm("project", event.target.value)} /></label>
            <label><span>Target trajectories</span><input required min={1} type="number" value={form.target} onChange={(event) => updateForm("target", Number(event.target.value))} /></label>
            <label><span>Template name</span><input required value={form.template} onChange={(event) => updateForm("template", event.target.value)} /></label>
            <label><span>Category</span><input required value={form.category} onChange={(event) => updateForm("category", event.target.value)} /></label>
            <label><span>Difficulty</span><select value={form.difficulty} onChange={(event) => updateForm("difficulty", event.target.value)}><option value="beginner">Beginner</option><option value="intermediate">Intermediate</option><option value="advanced">Advanced</option></select></label>
            <label><span>Required process</span><input required value={form.requiredApplication} onChange={(event) => updateForm("requiredApplication", event.target.value)} /></label>
            <label><span>Input asset keys</span><input value={form.inputAssets} onChange={(event) => updateForm("inputAssets", event.target.value)} placeholder="inputs/example.png, inputs/brief.pdf" /></label>
            <label><span>Expected output</span><input required value={form.outputName} onChange={(event) => updateForm("outputName", event.target.value)} /></label>
            <label><span>Output media type</span><input required value={form.outputMediaType} onChange={(event) => updateForm("outputMediaType", event.target.value)} /></label>
            <label><span>Contributor</span><input required value={form.contributor} onChange={(event) => updateForm("contributor", event.target.value)} /></label>
          </div>
          <label className="commentField"><span>Task goal</span><textarea required rows={3} value={form.goal} onChange={(event) => updateForm("goal", event.target.value)} /></label>
          <label className="commentField"><span>Finish criteria</span><textarea required rows={2} value={form.finishCriteria} onChange={(event) => updateForm("finishCriteria", event.target.value)} /></label>
          <button type="submit" disabled={busy}>{busy ? "Creating…" : "Create project & assignment"}</button>
        </form>
        {created ? <div className="successMessage"><strong>Assignment ready.</strong><span>Session {created.sessionID}</span><small>Project {created.projectID} · Task {created.taskID} · Contributor {created.contributorID}</small></div> : null}
          </>
        )}
      </section>

      <section className="panel projectsPanel">
        <div className="panelHeading"><div><p className="eyebrow">Live control plane</p><h2>{loading ? "Loading…" : `${projects.length} projects`}</h2></div><button className="secondary" type="button" onClick={() => void loadProjects()} disabled={loading}>Refresh</button></div>
        {error ? <p className="errorMessage" role="alert">{error}</p> : null}
        <div className="projectList">
          {projects.map((project) => (
            <button key={project.id} type="button" className={selectedProject?.id === project.id ? "projectCard selected" : "projectCard"} onClick={() => void inspectProject(project)}>
              <span><strong>{project.name}</strong><small>{project.task_count} tasks · {project.session_count}/{project.target_trajectories} sessions</small></span>
              <span className="projectCounts"><b>{project.stage_counts.ready_for_review ?? 0} review</b><b>{project.stage_counts.accepted ?? 0} accepted</b><b>{project.stage_counts.released ?? 0} released</b></span>
            </button>
          ))}
        </div>
        {selectedProject ? (
          <div className="projectDetail">
            <div className="releaseBar">
              <label><span>Release name</span><input value={releaseName} onChange={(event) => setReleaseName(event.target.value)} /></label>
              <label><span>Release contents</span><select value={releaseProfile} onChange={(event) => setReleaseProfile(event.target.value as "trajectory_only" | "redacted_video")}><option value="trajectory_only">Trajectory data only (safe default)</option><option value="redacted_video">Reviewer-approved redacted video</option></select></label>
              <button type="button" onClick={() => void createRelease()} disabled={busy || (selectedProject.stage_counts.accepted ?? 0) === 0}>Publish accepted</button>
              {manifestURL ? <a href={manifestURL} target="_blank" rel="noreferrer">Open manifest</a> : null}
              {bundleURL ? <a href={bundleURL} download>Download ZIP</a> : null}
            </div>
            {retentionPolicy ? (
              <form className="retentionForm" onSubmit={(event) => void saveRetentionPolicy(event)}>
                <div><p className="eyebrow">Data governance</p><strong>Retention policy</strong><small>The worker checks expiry every 15 minutes and honors legal holds.</small></div>
                <label><span>Raw days</span><input required min={1} max={3650} type="number" value={retentionPolicy.raw_days} onChange={(event) => setRetentionPolicy({ ...retentionPolicy, raw_days: Number(event.target.value) })} /></label>
                <label><span>Derived days</span><input required min={1} max={3650} type="number" value={retentionPolicy.derived_days} onChange={(event) => setRetentionPolicy({ ...retentionPolicy, derived_days: Number(event.target.value) })} /></label>
                <label><span>Release days</span><input required min={1} max={3650} type="number" value={retentionPolicy.release_days} onChange={(event) => setRetentionPolicy({ ...retentionPolicy, release_days: Number(event.target.value) })} /></label>
                <button type="submit" className="secondary" disabled={busy}>Save policy</button>
                {retentionSaved ? <b className="savedBadge">Saved</b> : null}
              </form>
            ) : null}
            <div className="sessionTable" role="table" aria-label={`${selectedProject.name} sessions`}>
              {sessions.map((session) => (
                <div role="row" key={session.session_id}>
                  <span role="cell"><strong>{session.goal}</strong><small>{session.contributor_name}</small></span>
                  <code role="cell">{session.session_id}</code>
                  <b role="cell" data-state={session.state}>{session.state.replaceAll("_", " ")}</b>
                  <span role="cell" className="governanceActions">
                    {legalHolds.find((hold) => hold.session_id === session.session_id) ? (
                      <button type="button" className="secondary" disabled={busy} onClick={() => {
                        const hold = legalHolds.find((item) => item.session_id === session.session_id);
                        if (hold) void releaseLegalHold(session.session_id, hold.id);
                      }}>Release hold</button>
                    ) : <button type="button" className="secondary" disabled={busy || session.state === "DELETED"} onClick={() => void placeLegalHold(session.session_id)}>Place hold</button>}
                    <button type="button" className="danger" disabled={busy || ["RELEASED", "DELETED", "RECORDING", "FINALIZING", "UPLOADING", "SUBMITTED", "PROCESSING"].includes(session.state) || legalHolds.some((hold) => hold.session_id === session.session_id) || deletionRequests.some((request) => request.session_id === session.session_id && ["queued", "leased", "blocked"].includes(request.state))} onClick={() => void requestDeletion(session.session_id)}>Erase artifacts</button>
                  </span>
                </div>
              ))}
            </div>
          </div>
        ) : null}
        <div className="deadLetterPanel">
          <div className="panelHeading">
            <div><p className="eyebrow">Processing recovery</p><h3>{deadLetterJobs.length + redactionDeadLetters.length + deletionRequests.filter((request) => request.state === "dead_letter").length + retentionPurges.filter((request) => request.state === "dead_letter").length ? "Jobs need attention" : "No dead-letter jobs"}</h3></div>
          </div>
          {deadLetterJobs.map((job) => (
            <article key={job.id}>
              <span><strong>{job.job_type.replaceAll("_", " ")}</strong><small>Session {job.session_id} · {job.attempt} attempts{job.manual_requeues ? ` · ${job.manual_requeues} manual retries` : ""}</small>{job.last_error ? <em>{job.last_error}</em> : null}</span>
              <button type="button" className="secondary" disabled={busy} onClick={() => void retryDeadLetter(job.id)}>Retry safely</button>
            </article>
          ))}
          {redactionDeadLetters.map((job) => (
            <article key={job.id}>
              <span><strong>Video redaction</strong><small>Session {job.session_id} · {job.attempt} attempts{job.manual_requeues ? ` · ${job.manual_requeues} manual retries` : ""}</small>{job.last_error ? <em>{job.last_error}</em> : null}</span>
              <button type="button" className="secondary" disabled={busy} onClick={() => void retryRedaction(job.id)}>Retry redaction</button>
            </article>
          ))}
          {deletionRequests.filter((request) => request.state === "dead_letter").map((request) => (
            <article key={request.id}>
              <span><strong>Artifact erasure</strong><small>Session {request.session_id} · {request.attempt} attempts{request.manual_requeues ? ` · ${request.manual_requeues} manual retries` : ""}</small>{request.last_error ? <em>{request.last_error}</em> : null}</span>
              <button type="button" className="secondary" disabled={busy} onClick={() => void retryDeletion(request.id)}>Retry erasure</button>
            </article>
          ))}
          {retentionPurges.filter((request) => request.state === "dead_letter").map((request) => (
            <article key={request.id}>
              <span><strong>Retention: {request.resource_type.replaceAll("_", " ")}</strong><small>{request.resource_id} · {request.object_count} objects · {request.attempt} attempts</small>{request.last_error ? <em>{request.last_error}</em> : null}</span>
              <button type="button" className="secondary" disabled={busy} onClick={() => void retryRetentionPurge(request.id)}>Retry retention purge</button>
            </article>
          ))}
        </div>
      </section>
    </div>
  );
}

async function post<T>(path: string, body: unknown, actor: string) {
  return requestJSON<T>(`/api/control-plane${path}`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Actor-ID": actor, "X-Request-ID": crypto.randomUUID() },
    body: JSON.stringify(body),
  });
}

async function remove(path: string, actor: string) {
  const response = await fetch(`/api/control-plane${path}`, {
    method: "DELETE",
    headers: { "X-Actor-ID": actor, "X-Request-ID": crypto.randomUUID() },
  });
  if (!response.ok) {
    const payload = await response.json() as { error?: { message?: string } };
    throw new Error(payload.error?.message ?? `Request failed with ${response.status}`);
  }
}

async function put<T>(path: string, body: unknown, actor: string) {
  return requestJSON<T>(`/api/control-plane${path}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json", "X-Actor-ID": actor, "X-Request-ID": crypto.randomUUID() },
    body: JSON.stringify(body),
  });
}

async function requestOptionalJSON<T>(input: string): Promise<T | null> {
  const response = await fetch(input, { cache: "no-store" });
  if (response.status === 404) return null;
  const payload = await response.json() as T & { error?: { message?: string } };
  if (!response.ok) throw new Error(payload.error?.message ?? `Request failed with ${response.status}`);
  return payload;
}

async function requestJSON<T>(input: string, init?: RequestInit): Promise<T> {
  const response = await fetch(input, init);
  const payload = await response.json() as T & { error?: { message?: string } };
  if (!response.ok) throw new Error(payload.error?.message ?? `Request failed with ${response.status}`);
  return payload;
}

function messageFor(error: unknown) {
  return error instanceof Error ? error.message : String(error);
}
