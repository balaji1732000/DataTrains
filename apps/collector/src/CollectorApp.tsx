import { invoke, isTauri } from "@tauri-apps/api/core";
import { open } from "@tauri-apps/plugin-dialog";
import { useEffect, useRef, useState } from "react";

type CaptureStage = "READY" | "RECORDING" | "PAUSED" | "FINALIZING" | "FINALIZED" | "FAILED";
type SessionState =
  | "ASSIGNED" | "READY" | "RECORDING" | "FINALIZING" | "UPLOADING" | "SUBMITTED"
  | "PROCESSING" | "READY_FOR_REVIEW" | "ACCEPTED" | "RELEASED" | "FAILED"
  | "REJECTED" | "REWORK_REQUIRED" | "CANCELLED" | "DELETED";
type PreflightCheck = { name: string; passed: boolean; detail: string };
type PreflightReport = { checked_at_unix_ms: number; checks: PreflightCheck[] };
type ExpectedOutput = { name?: string; media_type?: string };
type InputAsset = { key?: string; name?: string };
type Assignment = {
  assignment_id: string;
  session_id: string;
  session_state: SessionState;
  task_id: string;
  template_id: string;
  goal: string;
  category: string;
  difficulty: "beginner" | "intermediate" | "advanced";
  required_application: string;
  input_assets: InputAsset[];
  expected_outputs: ExpectedOutput[];
  specification: { capture_signals?: string[]; finish_criteria?: string };
  rework_of_session_id?: string;
  rework_comments?: string;
  review_decision?: string;
  review_comments?: string;
  reviewed_at?: string;
  consent_document_id?: string;
  consent_version?: string;
  consent_text_hash?: string;
  consent_accepted_at?: string;
};
type ConsentDocument = {
  id: string;
  version: string;
  text_hash: string;
  body: string;
  effective_date: string;
};
type RecoveredSession = {
  session_id: string;
  task_id: string;
  stage: CaptureStage;
  elapsed_ns: number;
  directory: string;
};
type CaptureStatus = {
  session_id: string;
  stage: CaptureStage;
  elapsed_ns: number;
  recording_visible: boolean;
  pause_reason: string | null;
};
type CaptureSource =
  | { type: "windows_desktop" }
  | { type: "mac_os_screen"; device: string }
  | { type: "x11_display"; display: string; width: number; height: number };
type CaptureManifest = {
  schema_version: string;
  session_id: string;
  task_id: string;
  duration_ns: number;
  artifacts: Array<{ path: string; size: number; sha256: string; media_type: string }>;
};
type SubmissionReceipt = {
  session_id: string;
  job_id: string;
  uploaded_artifacts: number;
  state: string;
};
type CollectorConfiguration = { auth_mode: "local" | "oidc"; api_base: string };
type AuthProfile = { account_id: string; display_name: string; email?: string; contributor_id?: string };

const checkLabels: Readonly<Record<string, string>> = {
  screen_capture: "Screen capture",
  input_capture: "Input capture",
  disk_space: "Disk space",
  required_application: "Required application",
  output_folder: "Output folder",
  collector_version: "Collector version",
};

type Step = "login" | "assignments" | "task" | "consent" | "preflight" | "ready";

export function CollectorApp() {
  const [step, setStep] = useState<Step>("login");
  const [apiBase, setApiBase] = useState(() => localStorage.getItem("trajectory.apiBase") || "http://127.0.0.1:8080");
  const [contributorId, setContributorId] = useState(() => localStorage.getItem("trajectory.contributorId") || "");
  const [configuration, setConfiguration] = useState<CollectorConfiguration | null>(null);
  const [authProfile, setAuthProfile] = useState<AuthProfile | null>(null);
  const [assignments, setAssignments] = useState<Assignment[]>([]);
  const [assignment, setAssignment] = useState<Assignment | null>(null);
  const [consentDocument, setConsentDocument] = useState<ConsentDocument | null>(null);
  const [consented, setConsented] = useState(false);
  const [preflight, setPreflight] = useState<PreflightReport | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [checking, setChecking] = useState(false);
  const [operating, setOperating] = useState(false);
  const [recoverable, setRecoverable] = useState<RecoveredSession[]>([]);
  const [capture, setCapture] = useState<CaptureStatus | null>(null);
  const [outputFiles, setOutputFiles] = useState<string[]>([]);
  const [manifest, setManifest] = useState<CaptureManifest | null>(null);
  const [receipt, setReceipt] = useState<SubmissionReceipt | null>(null);
  const intervalBusy = useRef(false);
  const nextRotationNs = useRef(60_000_000_000);
  const allChecksPass = preflight?.checks.every((check) => check.passed) === true;
  const progress = ({ login: 10, assignments: 20, task: 35, consent: 55, preflight: 80, ready: 100 } as const)[step];
  const isRecording = capture?.stage === "RECORDING";
  const identityMode = configuration?.auth_mode === "oidc";

  useEffect(() => {
    if (!isTauri()) return;
    void invoke<RecoveredSession[]>("recoverable_sessions").then(setRecoverable).catch(() => setRecoverable([]));
  }, []);

  useEffect(() => {
    if (!isTauri()) return;
    let active = true;
    void invoke<CollectorConfiguration>("collector_configuration")
      .then(async (current) => {
        if (!active) return;
        setConfiguration(current);
        setApiBase(current.api_base);
        if (current.auth_mode !== "oidc") return;
        localStorage.removeItem("trajectory.apiBase");
        localStorage.removeItem("trajectory.contributorId");
        const profile = await invoke<AuthProfile | null>("restore_session");
        if (!active || !profile?.contributor_id) return;
        setAuthProfile(profile);
        setContributorId(profile.contributor_id);
        await loadWorkspace(current.api_base, profile.contributor_id, true);
      })
      .catch((reason: unknown) => { if (active) setError(String(reason)); });
    return () => { active = false; };
  }, []);

  useEffect(() => {
    if (!capture || manifest || operating || !isTauri()) return;
    const timer = window.setInterval(() => {
      if (intervalBusy.current) return;
      intervalBusy.current = true;
      void invoke<CaptureStatus>("checkpoint_capture")
        .then(async (current) => {
          if (current.stage === "RECORDING" && current.elapsed_ns >= nextRotationNs.current) {
            const rotated = await invoke<CaptureStatus>("rotate_capture");
            nextRotationNs.current = rotated.elapsed_ns + 60_000_000_000;
            setCapture(rotated);
          } else {
            setCapture(current);
          }
        })
        .catch((reason: unknown) => setError(String(reason)))
        .finally(() => {
          intervalBusy.current = false;
        });
    }, 1_000);
    return () => window.clearInterval(timer);
  }, [capture?.session_id, manifest, operating]);

  async function connect() {
    if (!apiBase.trim() || (!identityMode && !contributorId.trim())) return;
    setOperating(true);
    setError(null);
    try {
      if (identityMode) {
        const profile = authProfile ?? await invoke<AuthProfile>("sign_in");
        if (!profile.contributor_id) throw new Error("This account does not have an active contributor profile.");
        setAuthProfile(profile);
        setContributorId(profile.contributor_id);
        await loadWorkspace(apiBase, profile.contributor_id, true);
      } else {
        await loadWorkspace(apiBase, contributorId.trim(), false);
        localStorage.setItem("trajectory.apiBase", apiBase.trim());
        localStorage.setItem("trajectory.contributorId", contributorId.trim());
      }
    } catch (reason) {
      setError(String(reason));
    } finally {
      setOperating(false);
    }
  }

  async function loadWorkspace(base: string, contributor: string, production: boolean) {
    const [assignmentResponse, currentConsent] = await Promise.all([
      collectorAPIRequest<{ assignments: Assignment[] }>(production, base, `/v1/contributors/${encodeURIComponent(contributor)}/assignments`),
      collectorAPIRequest<ConsentDocument>(production, base, "/v1/consent-documents/current"),
    ]);
    setAssignments(assignmentResponse.assignments);
    setConsentDocument(currentConsent);
    setStep("assignments");
  }

  function selectAssignment(selected: Assignment) {
    setAssignment(selected);
    setConsented(hasCurrentConsent(selected, consentDocument));
    setPreflight(null);
    setStep("task");
  }

  async function signOut() {
    let warning: string | null = null;
    if (identityMode && isTauri()) {
      try {
        const result = await invoke<{ warning?: string }>("sign_out");
        warning = result.warning ?? null;
      } catch (reason) {
        setError(String(reason));
        return;
      }
    }
    localStorage.removeItem("trajectory.contributorId");
    setContributorId("");
    setAuthProfile(null);
    setAssignments([]);
    setAssignment(null);
    setConsentDocument(null);
    setConsented(false);
    setPreflight(null);
    setError(warning);
    setStep("login");
  }

  async function acceptConsent() {
    if (!assignment || !consentDocument) return;
    setOperating(true);
    setError(null);
    try {
      let acceptedAt = assignment.consent_accepted_at;
      if (!hasCurrentConsent(assignment, consentDocument)) {
        const accepted = await collectorAPIRequest<{ id: string; accepted_at: string }>(
          identityMode,
          apiBase,
          "/v1/consent-acceptances",
          {
            method: "POST",
            actor: contributorId,
            body: {
              session_id: assignment.session_id,
              contributor_id: contributorId,
              document_id: consentDocument.id,
              client_version: "0.1.0",
            },
          },
        );
        acceptedAt = accepted.accepted_at;
      }
      if (!acceptedAt) throw new Error("Consent acceptance did not include a timestamp");
      const updated: Assignment = {
        ...assignment,
        consent_document_id: consentDocument.id,
        consent_version: consentDocument.version,
        consent_text_hash: consentDocument.text_hash,
        consent_accepted_at: acceptedAt,
      };
      setAssignment(updated);
      setAssignments((items) => items.map((item) => item.session_id === updated.session_id ? updated : item));
      setStep("preflight");
    } catch (reason) {
      setError(String(reason));
    } finally {
      setOperating(false);
    }
  }

  async function runPreflight() {
    if (!assignment) return;
    setChecking(true);
    setError(null);
    if (!isTauri()) {
      setPreflight(null);
      setError("Native checks require the Tauri desktop collector. The browser preview cannot start recording.");
      setChecking(false);
      return;
    }
    try {
      const report = await invoke<PreflightReport>("run_preflight", {
        requiredApplication: assignment.required_application,
      });
      setPreflight(report);
      if (report.checks.every((check) => check.passed) && assignment.session_state === "ASSIGNED") {
        await collectorAPIRequest(identityMode, apiBase, `/v1/sessions/${assignment.session_id}/preflight`, {
          method: "POST",
          actor: contributorId,
          body: {},
        });
        updateAssignmentState("READY");
      }
    } catch (reason) {
      setError(String(reason));
    } finally {
      setChecking(false);
    }
  }

  async function startCapture() {
    if (!assignment || !preflight || !allChecksPass || !assignment.consent_document_id || !assignment.consent_version || !assignment.consent_text_hash || !assignment.consent_accepted_at) return;
    setOperating(true);
    setError(null);
    try {
      const policy = await invoke<Record<string, unknown>>("collector_policy", {
        requiredApplication: assignment.required_application,
      });
      const source = await invoke<CaptureSource>("capture_source");
      const status = await invoke<CaptureStatus>("start_capture", {
        request: {
          spec: {
            session_id: assignment.session_id,
            task_id: assignment.task_id,
            required_application: assignment.required_application,
            client_version: "0.1.0",
          },
          consent: {
            document_id: assignment.consent_document_id,
            version: assignment.consent_version,
            text_hash: assignment.consent_text_hash,
            accepted_at_unix_ms: Date.parse(assignment.consent_accepted_at),
          },
          preflight,
          policy,
          source,
        },
      });
      try {
        await transitionSession("RECORDING");
      } catch (reason) {
        const paused = await invoke<CaptureStatus>("pause_capture").catch(() => status);
        setCapture(paused);
        throw reason;
      }
      nextRotationNs.current = 60_000_000_000;
      setCapture(status);
    } catch (reason) {
      setError(String(reason));
    } finally {
      setOperating(false);
    }
  }

  async function runCaptureOperation(command: "pause_capture" | "resume_capture") {
    if (!assignment) return;
    setOperating(true);
    setError(null);
    try {
      if (command === "resume_capture" && assignment.session_state === "READY") {
        await transitionSession("RECORDING");
      }
      setCapture(await invoke<CaptureStatus>(command));
    } catch (reason) {
      setError(String(reason));
    } finally {
      setOperating(false);
    }
  }

  async function chooseOutputs() {
    const selected = await open({
      title: "Select the task output",
      multiple: true,
      directory: false,
    });
    setOutputFiles(selected ? (Array.isArray(selected) ? selected : [selected]) : []);
  }

  async function finishCapture() {
    if (!assignment || outputFiles.length === 0) return;
    setOperating(true);
    setError(null);
    try {
      const completed = await invoke<CaptureManifest>("finish_capture", { outputFiles });
      setManifest(completed);
      setCapture(null);
      setRecoverable([]);
      await submitCapture(completed);
    } catch (reason) {
      setError(String(reason));
    } finally {
      setOperating(false);
    }
  }

  async function submitCapture(completed = manifest) {
    if (!assignment || !completed) return;
    setOperating(true);
    setError(null);
    try {
      let state = assignment.session_state;
      if (state === "RECORDING") {
        await transitionSession("FINALIZING");
        state = "FINALIZING";
      }
      if (state === "FINALIZING") {
        await transitionSession("UPLOADING");
      }
      const submitted = await invoke<SubmissionReceipt>("submit_finalized_capture", {
        request: {
          api_base: apiBase,
          actor: contributorId,
          session_id: assignment.session_id,
          contributor_id: contributorId,
          task: {
            task_id: assignment.task_id,
            template_id: assignment.template_id,
            goal: assignment.goal,
            category: assignment.category,
            difficulty: assignment.difficulty,
          },
          environment: browserEnvironment(),
        },
      });
      setReceipt(submitted);
      updateAssignmentState("SUBMITTED");
    } catch (reason) {
      setError(String(reason));
    } finally {
      setOperating(false);
    }
  }

  async function recoverSession(session: RecoveredSession) {
    setOperating(true);
    setError(null);
    try {
      const recoveredAssignment = assignments.find((item) => item.session_id === session.session_id);
      if (!recoveredAssignment) {
        throw new Error("Connect with the contributor ID for this session before recovering it.");
      }
      setAssignment(recoveredAssignment);
      setConsented(hasCurrentConsent(recoveredAssignment, consentDocument));
      if (session.stage === "FINALIZED") {
        setManifest(await invoke<CaptureManifest>("load_finalized_capture", { sessionId: session.session_id }));
        setCapture(null);
        setRecoverable([]);
        return;
      }
      const source = await invoke<CaptureSource>("capture_source");
      const status = await invoke<CaptureStatus>("recover_session", {
        directory: session.directory,
        source,
      });
      nextRotationNs.current = status.elapsed_ns + 60_000_000_000;
      setCapture(status);
      setRecoverable([]);
    } catch (reason) {
      setError(String(reason));
    } finally {
      setOperating(false);
    }
  }

  async function transitionSession(state: SessionState) {
    if (!assignment) throw new Error("No assignment is selected");
    await collectorAPIRequest(identityMode, apiBase, `/v1/sessions/${assignment.session_id}/transitions`, {
      method: "POST",
      actor: contributorId,
      body: { state },
    });
    updateAssignmentState(state);
  }

  function updateAssignmentState(state: SessionState) {
    setAssignment((current) => current ? { ...current, session_state: state } : current);
    setAssignments((items) => items.map((item) => assignment && item.session_id === assignment.session_id ? { ...item, session_state: state } : item));
  }

  return (
    <main>
      <header className="appHeader">
        <div>
          <p className="eyebrow">Trajectory Collector</p>
          <h1>{assignment ? `Task ${assignment.task_id}` : "Contributor workspace"}</h1>
        </div>
        <span className={`safeBadge ${isRecording ? "recording" : ""}`} aria-live="polite">
          <i /> {isRecording ? "Recording" : "Not recording"}
        </span>
      </header>

      {!capture && !manifest ? (
        <div className="progressTrack" aria-label={`Onboarding ${progress}% complete`}>
          <span style={{ width: `${progress}%` }} />
        </div>
      ) : null}

      {recoverable.length > 0 && !capture ? (
        <RecoveryBanner session={recoverable[0]!} operating={operating} onRecover={() => void recoverSession(recoverable[0]!)} />
      ) : null}

      {capture ? (
        <CapturePanel
          capture={capture}
          outputFiles={outputFiles}
          operating={operating}
          onPause={() => void runCaptureOperation("pause_capture")}
          onResume={() => void runCaptureOperation("resume_capture")}
          onChooseOutputs={() => void chooseOutputs()}
          onFinish={() => void finishCapture()}
        />
      ) : manifest ? (
        <CompletedPanel manifest={manifest} receipt={receipt} operating={operating} onSubmit={() => void submitCapture()} />
      ) : step === "login" ? (
        <LoginPanel
          apiBase={apiBase}
          contributorId={contributorId}
          identityMode={identityMode}
          operating={operating}
          setApiBase={setApiBase}
          setContributorId={setContributorId}
          onConnect={() => void connect()}
        />
      ) : step === "assignments" ? (
        <AssignmentsPanel assignments={assignments} operating={operating} identityName={authProfile?.display_name} onRefresh={() => void connect()} onSelect={selectAssignment} onSignOut={() => void signOut()} />
      ) : assignment && consentDocument ? (
        <Onboarding
          step={step}
          assignment={assignment}
          consentDocument={consentDocument}
          consented={consented}
          preflight={preflight}
          checking={checking}
          allChecksPass={allChecksPass}
          operating={operating}
          setStep={setStep}
          setConsented={setConsented}
          acceptConsent={() => void acceptConsent()}
          runPreflight={() => void runPreflight()}
          startCapture={() => void startCapture()}
        />
      ) : null}

      {error ? <p className="errorMessage" role="alert">{error}</p> : null}
    </main>
  );
}

function LoginPanel(props: {
  apiBase: string;
  contributorId: string;
  identityMode: boolean;
  operating: boolean;
  setApiBase: (value: string) => void;
  setContributorId: (value: string) => void;
  onConnect: () => void;
}) {
  return (
    <section className="card">
      <p className="eyebrow">Contributor sign in</p>
      <h2>Open your assignment</h2>
      {props.identityMode ? (
        <p className="lede">Continue in your system browser. Your password, MFA, and account recovery stay with the identity provider.</p>
      ) : (
        <>
          <p className="lede">Local V1 uses the contributor ID issued by the local admin. No external identity provider is required.</p>
          <div className="formStack">
            <label className="fieldLabel">Local API<input value={props.apiBase} onChange={(event) => props.setApiBase(event.target.value)} /></label>
            <label className="fieldLabel">Contributor ID<input value={props.contributorId} onChange={(event) => props.setContributorId(event.target.value)} placeholder="contrib_…" /></label>
          </div>
        </>
      )}
      <button type="button" disabled={props.operating || !props.apiBase.trim() || (!props.identityMode && !props.contributorId.trim())} onClick={props.onConnect}>{props.operating ? "Connecting…" : props.identityMode ? "Sign in securely" : "Load assignments"}</button>
    </section>
  );
}

function AssignmentsPanel({ assignments, operating, identityName, onRefresh, onSelect, onSignOut }: {
  assignments: Assignment[];
  operating: boolean;
  identityName?: string | undefined;
  onRefresh: () => void;
  onSelect: (assignment: Assignment) => void;
  onSignOut: () => void;
}) {
  const available = assignments.filter((item) => item.session_state === "ASSIGNED" || item.session_state === "READY");
  const history = assignments.filter((item) => item.session_state !== "ASSIGNED" && item.session_state !== "READY").reverse();
  return (
    <section className="card">
      <p className="eyebrow">Assignments</p>
      <h2>{available.length ? "Choose a task" : history.length ? "All caught up" : "No tasks are ready"}</h2>
      {identityName ? <p className="lede">Signed in as {identityName}</p> : null}
      {!available.length && history.length ? <p className="lede">You have no active tasks. Reviewed and submitted work appears below.</p> : null}
      <div className="assignmentList">
        {available.map((item) => (
          <button type="button" className="assignmentRow" key={item.assignment_id} onClick={() => onSelect(item)}>
            <span><strong>{item.goal}</strong><small>{item.required_application} · {item.difficulty}</small></span>
            <b>{item.rework_of_session_id ? "REWORK" : item.session_state}</b>
          </button>
        ))}
      </div>
      <div className="assignmentActions">
        <button type="button" className="secondary" disabled={operating} onClick={onRefresh}>{operating ? "Refreshing…" : "Refresh tasks"}</button>
        <button type="button" className="secondary" disabled={operating} onClick={onSignOut}>Sign out / Switch contributor</button>
      </div>
      {history.length ? (
        <section className="assignmentHistory">
          <p className="eyebrow">Task history</p>
          <div className="assignmentList">
            {history.map((item) => (
              <div className="assignmentRow historyRow" key={item.session_id}>
                <span>
                  <strong>{item.goal}</strong>
                  <small>{assignmentStatusDetail(item)}</small>
                  {item.review_comments ? <small>Reviewer: {item.review_comments}</small> : null}
                </span>
                <b>{assignmentStatusLabel(item.session_state)}</b>
              </div>
            ))}
          </div>
        </section>
      ) : null}
    </section>
  );
}

function assignmentStatusLabel(state: SessionState) {
  if (state === "ACCEPTED") return "COMPLETED";
  if (state === "RELEASED") return "PUBLISHED";
  if (state === "SUBMITTED" || state === "PROCESSING" || state === "READY_FOR_REVIEW") return "UNDER REVIEW";
  if (state === "REWORK_REQUIRED") return "REWORK REQUESTED";
  if (state === "REJECTED") return "NOT ACCEPTED";
  if (state === "FAILED") return "PROCESSING FAILED";
  if (state === "CANCELLED") return "CANCELLED";
  if (state === "DELETED") return "DATA DELETED";
  return "IN PROGRESS";
}

function assignmentStatusDetail(item: Assignment) {
  if (item.session_state === "ACCEPTED") return "Your submission was reviewed and accepted.";
  if (item.session_state === "RELEASED") return "Your accepted submission was included in a release.";
  if (item.session_state === "READY_FOR_REVIEW") return "Processing passed. Waiting for reviewer decision.";
  if (item.session_state === "SUBMITTED" || item.session_state === "PROCESSING") return "Your submission is being processed.";
  if (item.session_state === "REWORK_REQUIRED") return "A corrected attempt has been requested.";
  if (item.session_state === "REJECTED") return "The reviewer did not accept this submission.";
  if (item.session_state === "FAILED") return "The submission could not be processed.";
  if (item.session_state === "CANCELLED") return "This attempt was cancelled.";
  if (item.session_state === "DELETED") return "The retained artifacts for this attempt were deleted under the approved data policy.";
  return "This attempt is still in progress.";
}

type OnboardingProps = {
  step: Step;
  assignment: Assignment;
  consentDocument: ConsentDocument;
  consented: boolean;
  preflight: PreflightReport | null;
  checking: boolean;
  allChecksPass: boolean;
  operating: boolean;
  setStep: (step: Step) => void;
  setConsented: (value: boolean) => void;
  acceptConsent: () => void;
  runPreflight: () => void;
  startCapture: () => void;
};

function Onboarding(props: OnboardingProps) {
  const task = props.assignment;
  if (props.step === "task") {
    return (
      <section className="card">
        <p className="eyebrow">Assigned task</p>
        <h2>{task.goal}</h2>
        <dl className="taskGrid">
          <div><dt>Required app</dt><dd>{task.required_application}</dd></div>
          <div><dt>Expected output</dt><dd>{task.expected_outputs.map((output) => output.name).filter(Boolean).join(", ") || "Task output"}</dd></div>
          <div><dt>Input assets</dt><dd>{task.input_assets.length || "None"}</dd></div>
          <div><dt>Capture</dt><dd>{task.specification.capture_signals?.join(", ") || "Screen and permitted input"}</dd></div>
        </dl>
        {task.input_assets.length ? (
          <div className="taskDetails"><strong>Supplied assets</strong><ul>{task.input_assets.map((asset, index) => <li key={`${asset.key ?? asset.name ?? "asset"}-${index}`}>{asset.name ?? asset.key ?? `Asset ${index + 1}`}</li>)}</ul></div>
        ) : null}
        <div className="taskDetails"><strong>Finish when</strong><p>{task.specification.finish_criteria ?? "The expected output has been created and saved."}</p></div>
        {task.rework_of_session_id ? (
          <aside className="warning"><strong>Rework requested.</strong> {task.rework_comments || "Review the task requirements and submit a corrected recording."}</aside>
        ) : null}
        <aside className="warning"><strong>Controlled data only.</strong> Use the supplied assets and test account. Never enter passwords or personal information while recording.</aside>
        <button type="button" onClick={() => props.setStep("consent")}>Review consent</button>
      </section>
    );
  }
  if (props.step === "consent") {
    return (
      <section className="card">
        <p className="eyebrow">Consent document {props.consentDocument.version}</p>
        <h2>Recording is explicit and task-scoped</h2>
        <p className="consentText">{props.consentDocument.body}</p>
        <ul>
          <li>A permanent indicator remains visible while recording.</li>
          <li>You can pause or stop immediately. Clipboard content is never captured by default.</li>
          <li>Denied applications pause collection and flag the session for review.</li>
        </ul>
        <label className="consentRow">
          <input type="checkbox" checked={props.consented} onChange={(event) => props.setConsented(event.target.checked)} />
          <span>I consent under document <code>{props.consentDocument.id}</code>.</span>
        </label>
        <div className="actions">
          <button type="button" className="secondary" onClick={() => props.setStep("task")}>Back</button>
          <button type="button" disabled={!props.consented || props.operating} onClick={props.acceptConsent}>{props.operating ? "Saving…" : "Accept and continue"}</button>
        </div>
      </section>
    );
  }
  if (props.step === "preflight") {
    const checks = props.preflight?.checks ?? Object.keys(checkLabels).map((name) => ({ name, passed: false, detail: "Not checked" }));
    return (
      <section className="card">
        <p className="eyebrow">Environment preflight</p>
        <h2>Verify before capture</h2>
        <div className="checkList">
          {checks.map((check) => (
            <div key={check.name}>
              <span className={check.passed ? "check passed" : "check"}>{check.passed ? "✓" : "·"}</span>
              <div><strong>{checkLabels[check.name] ?? check.name}</strong><small>{check.detail}</small></div>
            </div>
          ))}
        </div>
        <div className="actions">
          <button type="button" className="secondary" onClick={props.runPreflight} disabled={props.checking}>{props.checking ? "Checking…" : "Run checks"}</button>
          <button type="button" disabled={!props.allChecksPass} onClick={() => props.setStep("ready")}>Continue</button>
        </div>
      </section>
    );
  }
  return (
    <section className="card readyCard">
      <p className="eyebrow">Ready</p>
      <h2>All safety checks passed</h2>
      <p className="lede">Starting creates a recoverable local spool. Focus outside the allowed task app pauses capture automatically.</p>
      <button type="button" disabled={!props.allChecksPass || props.operating} onClick={props.startCapture}>{props.operating ? "Starting…" : "Start task"}</button>
    </section>
  );
}

function RecoveryBanner({ session, operating, onRecover }: { session: RecoveredSession; operating: boolean; onRecover: () => void }) {
  const finalized = session.stage === "FINALIZED";
  return (
    <aside className="recoveryBanner" role="status">
      <strong>{finalized ? "Sealed capture awaiting submission." : "Unfinished recording found."}</strong>
      <span>{session.task_id} · {formatDuration(session.elapsed_ns)} · {session.stage}</span>
      <small>{finalized ? "The immutable local artifacts are intact and ready for an upload retry." : "The session files are intact. Recovery opens it paused so you decide when recording resumes."}</small>
      <button type="button" className="secondary" disabled={operating} onClick={onRecover}>{finalized ? "Resume submission" : "Recover session"}</button>
    </aside>
  );
}

function CapturePanel(props: {
  capture: CaptureStatus;
  outputFiles: string[];
  operating: boolean;
  onPause: () => void;
  onResume: () => void;
  onChooseOutputs: () => void;
  onFinish: () => void;
}) {
  const recording = props.capture.stage === "RECORDING";
  return (
    <section className={`card captureCard ${recording ? "active" : "paused"}`}>
      <p className="eyebrow">{recording ? "Capture active" : "Capture paused"}</p>
      <h2>{formatDuration(props.capture.elapsed_ns)}</h2>
      <p className="lede">Session <code>{props.capture.session_id}</code></p>
      {props.capture.pause_reason ? <aside className="pauseNotice">{props.capture.pause_reason}</aside> : null}
      <div className="captureActions">
        {recording ? (
          <button type="button" className="danger" disabled={props.operating} onClick={props.onPause}>Pause recording</button>
        ) : (
          <button type="button" disabled={props.operating} onClick={props.onResume}>Resume recording</button>
        )}
        <button type="button" className="secondary" disabled={props.operating} onClick={props.onChooseOutputs}>Select output</button>
      </div>
      <div className="selectedOutputs">
        <strong>Task output</strong>
        {props.outputFiles.length ? props.outputFiles.map((path) => <span key={path}>{fileName(path)}</span>) : <span>No output selected</span>}
      </div>
      <button type="button" className="finishButton" disabled={props.operating || props.outputFiles.length === 0} onClick={props.onFinish}>Finish and seal capture</button>
      <small className="safetyNote">Screen video is segmented every minute. A crash leaves completed segments recoverable.</small>
    </section>
  );
}

function CompletedPanel({ manifest, receipt, operating, onSubmit }: { manifest: CaptureManifest; receipt: SubmissionReceipt | null; operating: boolean; onSubmit: () => void }) {
  return (
    <section className="card completedCard">
      <p className="eyebrow">Capture sealed</p>
      <h2>{receipt ? "Submitted for processing" : "Local artifacts are sealed"}</h2>
      <p className="lede">{manifest.artifacts.length} immutable artifacts · {formatDuration(manifest.duration_ns)} · manifest {manifest.schema_version}</p>
      {receipt ? <aside className="submissionNotice">Job <code>{receipt.job_id}</code> · {receipt.uploaded_artifacts} artifacts uploaded</aside> : <button type="button" disabled={operating} onClick={onSubmit}>{operating ? "Uploading…" : "Retry upload and submit"}</button>}
      <div className="manifestList">
        {manifest.artifacts.map((artifact) => (
          <div key={artifact.path}><strong>{artifact.path}</strong><span>{formatBytes(artifact.size)} · {artifact.sha256.slice(0, 12)}…</span></div>
        ))}
      </div>
    </section>
  );
}

function browserEnvironment() {
  const scaleFactor = window.devicePixelRatio || 1;
  return {
    os: operatingSystem(),
    os_version: navigator.userAgent,
    locale: browserLocale(),
    timezone: Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC",
    display: {
      width: Math.max(320, Math.round(window.screen.width * scaleFactor)),
      height: Math.max(240, Math.round(window.screen.height * scaleFactor)),
      scale_factor: scaleFactor,
    },
  };
}

function browserLocale() {
  const locale = (navigator.language || "").trim().replace("_", "-");
  if (locale.length < 2 || /^(c(?:\..*)?|posix)$/i.test(locale)) return "en-US";
  return locale;
}

function operatingSystem(): "windows" | "linux" | "macos" {
  const platform = `${navigator.userAgent} ${navigator.platform}`.toLowerCase();
  if (platform.includes("linux")) return "linux";
  if (platform.includes("mac")) return "macos";
  return "windows";
}

type APIOptions = { method?: "POST"; actor?: string; body?: unknown };

async function collectorAPIRequest<T = unknown>(production: boolean, base: string, path: string, options: APIOptions = {}): Promise<T> {
  if (production) {
    if (!isTauri()) throw new Error("Production identity is available only in the native DataTrains Collector.");
    return invoke<T>("authenticated_api_request", {
      request: { method: options.method ?? "GET", path, body: options.body },
    });
  }
  return apiRequest<T>(base, path, options);
}

async function apiRequest<T = unknown>(base: string, path: string, options: APIOptions = {}): Promise<T> {
  const request: RequestInit = {
    method: options.method ?? "GET",
    headers: {
      "Content-Type": "application/json",
      ...(options.actor ? { "X-Actor-ID": options.actor } : {}),
    },
  };
  if (options.body !== undefined) request.body = JSON.stringify(options.body);
  const response = await fetch(`${base.replace(/\/$/, "")}${path}`, request);
  const payload = await response.json() as T & { error?: { message?: string } };
  if (!response.ok) throw new Error(payload.error?.message ?? `API request failed (${response.status})`);
  return payload;
}

function fileName(path: string) {
  return path.split(/[\\/]/).at(-1) ?? path;
}

function formatDuration(nanoseconds: number) {
  const totalSeconds = Math.floor(nanoseconds / 1_000_000_000);
  return `${Math.floor(totalSeconds / 60)}m ${(totalSeconds % 60).toString().padStart(2, "0")}s`;
}

function formatBytes(bytes: number) {
  if (bytes < 1_000) return `${bytes} B`;
  if (bytes < 1_000_000) return `${(bytes / 1_000).toFixed(1)} KB`;
  return `${(bytes / 1_000_000).toFixed(1)} MB`;
}

function hasCurrentConsent(assignment: Assignment, document: ConsentDocument | null) {
  return Boolean(document
    && assignment.consent_document_id === document.id
    && assignment.consent_version === document.version
    && assignment.consent_text_hash === document.text_hash
    && assignment.consent_accepted_at);
}
