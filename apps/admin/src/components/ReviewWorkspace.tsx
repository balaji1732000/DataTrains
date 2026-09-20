"use client";

import Image from "next/image";
import { useCallback, useEffect, useRef, useState } from "react";
import { Timeline, type TrajectoryAction } from "./Timeline";

type QueueItem = {
  session_id: string;
  task_id: string;
  goal: string;
  category: string;
  difficulty: string;
  contributor_id: string;
  normalized_key: string;
  ready_since: string;
};

type ArtifactReference = { key: string; sha256: string; size: number; media_type: string };
type VideoSegment = ArtifactReference & { segment_id: string; start_ns: number; end_ns: number };
type Trajectory = {
  trajectory_id: string;
  task: { goal: string; category: string; difficulty: string };
  capture: { duration_ns: number; video_segments: VideoSegment[] };
  actions: TrajectoryAction[];
  outcome: { status: string; output_artifacts: ArtifactReference[] };
  privacy: { consent_version: string; pii_review: string; recording_visible: boolean; clipboard_captured: boolean };
};

type Decision = "accepted" | "rejected" | "rework_required";
type VideoPlanMode = "private" | "prepare";
type RedactionDecision = "review_required" | "publish_as_is" | "redact";
type RedactionRegionDraft = {
  id: string;
  startSeconds: number;
  endSeconds: number;
  xPercent: number;
  yPercent: number;
  widthPercent: number;
  heightPercent: number;
  kind: "personal_data" | "credential" | "unrelated_content" | "other";
};
type SegmentRedactionDraft = { decision: RedactionDecision; regions: RedactionRegionDraft[] };

export function ReviewWorkspace({ identityMode = false }: { identityMode?: boolean }) {
  const [queue, setQueue] = useState<QueueItem[]>([]);
  const [selected, setSelected] = useState<QueueItem | null>(null);
  const [trajectory, setTrajectory] = useState<Trajectory | null>(null);
  const [reviewerID, setReviewerID] = useState(identityMode ? "" : "reviewer-local");
  const [segmentIndex, setSegmentIndex] = useState(0);
  const [currentNS, setCurrentNS] = useState(0);
  const [taskScore, setTaskScore] = useState(5);
  const [qualityScore, setQualityScore] = useState(5);
  const [piiReview, setPIIReview] = useState("pending");
  const [comments, setComments] = useState("");
  const [videoPlanMode, setVideoPlanMode] = useState<VideoPlanMode>("private");
  const [redactionDrafts, setRedactionDrafts] = useState<Record<string, SegmentRedactionDraft>>({});
  const [busy, setBusy] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const videoRef = useRef<HTMLVideoElement>(null);
  const pendingSeekNS = useRef<number | null>(null);

  const loadQueue = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const result = await requestJSON<{ sessions: QueueItem[] }>("/api/control-plane/v1/review-queue", { cache: "no-store" });
      setQueue(result.sessions);
    } catch (caught) {
      setError(messageFor(caught));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    let active = true;
    const identity = identityMode
      ? requestJSON<{ account_id: string }>("/api/control-plane/v1/me", { cache: "no-store" })
      : Promise.resolve(null);
    void Promise.all([
      requestJSON<{ sessions: QueueItem[] }>("/api/control-plane/v1/review-queue", { cache: "no-store" }),
      identity,
    ])
      .then(([result, current]) => {
        if (active) {
          setQueue(result.sessions);
          if (current) setReviewerID(current.account_id);
        }
      })
      .catch((caught: unknown) => {
        if (active) setError(messageFor(caught));
      })
      .finally(() => {
        if (active) setLoading(false);
      });
    return () => {
      active = false;
    };
  }, [identityMode]);

  async function claim(item: QueueItem) {
    if (!reviewerID.trim()) {
      setError("Enter a reviewer ID before claiming a session.");
      return;
    }
    setBusy(true);
    setError(null);
    try {
      await requestJSON(`/api/control-plane/v1/sessions/${encodeURIComponent(item.session_id)}/review-claim`, {
        method: "POST",
        headers: mutationHeaders(reviewerID),
        body: JSON.stringify({ reviewer_id: reviewerID, lease_seconds: 1800 }),
      });
      const data = await requestJSON<Trajectory>(`/api/control-plane/v1/sessions/${encodeURIComponent(item.session_id)}/trajectory`, { cache: "no-store" });
      setSelected(item);
      setTrajectory(data);
      setSegmentIndex(0);
      setCurrentNS(0);
      setPIIReview("pending");
      setComments("");
      setVideoPlanMode("private");
      setRedactionDrafts(Object.fromEntries(data.capture.video_segments.map((item) => [
        item.segment_id,
        { decision: "review_required", regions: [] },
      ])));
    } catch (caught) {
      setError(messageFor(caught));
      await loadQueue();
    } finally {
      setBusy(false);
    }
  }

  function seek(timestampNS: number) {
    if (!trajectory) return;
    const nextIndex = trajectory.capture.video_segments.findIndex(
      (candidate) => timestampNS >= candidate.start_ns && timestampNS <= candidate.end_ns,
    );
    if (nextIndex < 0) return;
    pendingSeekNS.current = timestampNS;
    setCurrentNS(timestampNS);
    if (nextIndex === segmentIndex) applyPendingSeek();
    else setSegmentIndex(nextIndex);
  }

  function applyPendingSeek() {
    const video = videoRef.current;
    const activeSegment = trajectory?.capture.video_segments[segmentIndex];
    const target = pendingSeekNS.current;
    if (!video || !activeSegment || target === null) return;
    video.currentTime = Math.max(0, (target - activeSegment.start_ns) / 1_000_000_000);
    pendingSeekNS.current = null;
  }

  async function submit(decision: Decision) {
    if (!selected || !trajectory) return;
    setBusy(true);
    setError(null);
    try {
      const redactionPlan = decision === "accepted" && videoPlanMode === "prepare"
        ? buildRedactionPlan(trajectory, redactionDrafts)
        : undefined;
      await requestJSON(`/api/control-plane/v1/sessions/${encodeURIComponent(selected.session_id)}/reviews`, {
        method: "POST",
        headers: mutationHeaders(reviewerID),
        body: JSON.stringify({
          reviewer_id: reviewerID,
          rubric_version: "rubric-v1",
          scores: { task_success: taskScore, trajectory_quality: qualityScore },
          comments,
          decision,
          pii_review: piiReview,
          redaction_plan: redactionPlan,
        }),
      });
      setSelected(null);
      setTrajectory(null);
      await loadQueue();
    } catch (caught) {
      setError(messageFor(caught));
    } finally {
      setBusy(false);
    }
  }

  function setSegmentDecision(segmentID: string, decision: RedactionDecision) {
    setRedactionDrafts((current) => ({
      ...current,
      [segmentID]: {
        decision,
        regions: decision === "redact" ? current[segmentID]?.regions ?? [] : [],
      },
    }));
  }

  function addRegion(videoSegment: VideoSegment) {
    const durationSeconds = Math.max(0.001, (videoSegment.end_ns - videoSegment.start_ns) / 1_000_000_000);
    setRedactionDrafts((current) => ({
      ...current,
      [videoSegment.segment_id]: {
        decision: "redact",
        regions: [...(current[videoSegment.segment_id]?.regions ?? []), {
          id: crypto.randomUUID(), startSeconds: 0, endSeconds: durationSeconds,
          xPercent: 0, yPercent: 0, widthPercent: 100, heightPercent: 100,
          kind: "personal_data",
        }],
      },
    }));
  }

  function updateRegion(segmentID: string, regionID: string, field: keyof Omit<RedactionRegionDraft, "id">, value: string) {
    setRedactionDrafts((current) => ({
      ...current,
      [segmentID]: {
        ...(current[segmentID] ?? { decision: "redact", regions: [] }),
        regions: (current[segmentID]?.regions ?? []).map((region) => region.id === regionID
          ? { ...region, [field]: field === "kind" ? value : Number(value) }
          : region),
      },
    }));
  }

  function removeRegion(segmentID: string, regionID: string) {
    setRedactionDrafts((current) => ({
      ...current,
      [segmentID]: {
        ...(current[segmentID] ?? { decision: "redact", regions: [] }),
        regions: (current[segmentID]?.regions ?? []).filter((region) => region.id !== regionID),
      },
    }));
  }

  async function release() {
    if (!selected) return;
    setBusy(true);
    setError(null);
    try {
      await requestEmpty(`/api/control-plane/v1/sessions/${encodeURIComponent(selected.session_id)}/review-claim?reviewer_id=${encodeURIComponent(reviewerID)}`, {
        method: "DELETE",
        headers: mutationHeaders(reviewerID),
      });
      setSelected(null);
      setTrajectory(null);
      await loadQueue();
    } catch (caught) {
      setError(messageFor(caught));
    } finally {
      setBusy(false);
    }
  }

  if (!selected || !trajectory) {
    return (
      <section className="queueLayout">
        <aside className="queueIntro">
          <p className="eyebrow">Reviewer identity</p>
          {identityMode ? <p className="identityValue">Signed-in account<br /><code>{reviewerID || "Loading identity…"}</code></p> : (
            <label>
              <span>Actor ID</span>
              <input value={reviewerID} onChange={(event) => setReviewerID(event.target.value)} autoComplete="username" />
            </label>
          )}
          <p>Claims are exclusive for 30 minutes and every decision is appended to the audit log.</p>
        </aside>
        <section className="panel queuePanel">
          <div className="panelHeading">
            <div><p className="eyebrow">QA queue</p><h2>{loading ? "Loading…" : `${queue.length} ready for review`}</h2></div>
            <button className="secondary" type="button" onClick={() => void loadQueue()} disabled={loading}>Refresh</button>
          </div>
          {error ? <p className="errorMessage" role="alert">{error}</p> : null}
          {!loading && queue.length === 0 ? <div className="emptyQueue"><strong>Queue clear.</strong><span>Validated sessions will appear here automatically.</span></div> : null}
          <div className="queueList">
            {queue.map((item) => (
              <article key={item.session_id}>
                <div>
                  <span className="difficulty">{item.difficulty}</span>
                  <h3>{item.goal}</h3>
                  <p>{item.category} · {item.task_id} · {item.contributor_id}</p>
                </div>
                <button type="button" onClick={() => void claim(item)} disabled={busy}>Claim &amp; review</button>
              </article>
            ))}
          </div>
        </section>
      </section>
    );
  }

  const segment = trajectory.capture.video_segments[segmentIndex];
  return (
    <section className="reviewLayout">
      <div className="reviewHeader panel">
        <div><p className="eyebrow">Claimed by {reviewerID}</p><h2>{trajectory.task.goal}</h2><p>{trajectory.trajectory_id} · {trajectory.task.category}</p></div>
        <button className="secondary" type="button" onClick={() => void release()} disabled={busy}>Release &amp; return</button>
      </div>
      {error ? <p className="errorMessage" role="alert">{error}</p> : null}
      <div className="evidenceGrid">
        <section className="videoPanel panel">
          <div className="panelHeading"><div><p className="eyebrow">Screen evidence</p><h3>Segment {segmentIndex + 1} of {trajectory.capture.video_segments.length}</h3></div><span>{formatBytes(segment?.size ?? 0)}</span></div>
          {segment ? (
            <video
              ref={videoRef}
              controls
              preload="metadata"
              src={artifactURL(selected.session_id, segment.key)}
              onLoadedMetadata={applyPendingSeek}
              onTimeUpdate={(event) => setCurrentNS(segment.start_ns + Math.round(event.currentTarget.currentTime * 1_000_000_000))}
            />
          ) : <p>No video segment was provided.</p>}
          <div className="segmentTabs" aria-label="Video segments">
            {trajectory.capture.video_segments.map((item, index) => (
              <button key={item.segment_id} type="button" className={index === segmentIndex ? "active" : undefined} onClick={() => { setSegmentIndex(index); setCurrentNS(item.start_ns); }}>
                {item.segment_id}
              </button>
            ))}
          </div>
        </section>
        <Timeline actions={trajectory.actions} currentNS={currentNS} durationNS={trajectory.capture.duration_ns} onSeek={seek} />
      </div>
      <section className="outputsPanel panel">
        <div><p className="eyebrow">Outcome evidence</p><h2>{trajectory.outcome.status}</h2></div>
        <div className="outputGrid">
          {trajectory.outcome.output_artifacts.map((artifact) => (
            <article key={artifact.key}>
              {artifact.media_type.startsWith("image/") ? (
                <Image src={artifactURL(selected.session_id, artifact.key)} alt="Submitted task output" width={900} height={600} unoptimized />
              ) : <div className="filePreview">{artifact.media_type}</div>}
              <strong>{artifact.key.split("/").at(-1)}</strong>
              <span>{formatBytes(artifact.size)} · SHA-256 {artifact.sha256.slice(0, 12)}…</span>
            </article>
          ))}
        </div>
      </section>
      <section className="decisionPanel panel">
        <div><p className="eyebrow">Rubric v1</p><h2>QA decision</h2></div>
        <div className="scoreGrid">
          <label><span>Task success</span><select value={taskScore} onChange={(event) => setTaskScore(Number(event.target.value))}>{scoreOptions}</select></label>
          <label><span>Trajectory quality</span><select value={qualityScore} onChange={(event) => setQualityScore(Number(event.target.value))}>{scoreOptions}</select></label>
          <label><span>PII review</span><select value={piiReview} onChange={(event) => setPIIReview(event.target.value)}><option value="pending">Pending</option><option value="passed">Passed</option><option value="failed">Failed</option></select></label>
        </div>
        <label className="commentField"><span>Reviewer comments</span><textarea value={comments} onChange={(event) => setComments(event.target.value)} rows={4} /></label>
        <p className="privacyLine">Consent {trajectory.privacy.consent_version} · recording visible: {String(trajectory.privacy.recording_visible)} · clipboard captured: {String(trajectory.privacy.clipboard_captured)}</p>
        <div className="videoPrivacyPanel">
          <div><p className="eyebrow">Release privacy</p><h3>What may happen to the screen recording?</h3></div>
          <label className="radioCard">
            <input type="radio" name="video-plan" checked={videoPlanMode === "private"} onChange={() => setVideoPlanMode("private")} />
            <span><strong>Keep recordings private</strong><small>Default. Only the trajectory data can be released.</small></span>
          </label>
          <label className="radioCard">
            <input type="radio" name="video-plan" checked={videoPlanMode === "prepare"} onChange={() => setVideoPlanMode("prepare")} disabled={trajectory.capture.video_segments.length === 0} />
            <span><strong>Prepare reviewed video</strong><small>Make an explicit decision for every segment. Raw recordings still remain private.</small></span>
          </label>
          {videoPlanMode === "prepare" ? (
            <div className="redactionSegments">
              {trajectory.capture.video_segments.map((videoSegment) => {
                const draft = redactionDrafts[videoSegment.segment_id] ?? { decision: "review_required", regions: [] };
                const durationSeconds = Math.max(0, (videoSegment.end_ns - videoSegment.start_ns) / 1_000_000_000);
                return (
                  <article key={videoSegment.segment_id}>
                    <div className="redactionSegmentHeading">
                      <span><strong>Segment {videoSegment.segment_id}</strong><small>{durationSeconds.toFixed(2)} seconds</small></span>
                      <select aria-label={`Privacy decision for segment ${videoSegment.segment_id}`} value={draft.decision} onChange={(event) => setSegmentDecision(videoSegment.segment_id, event.target.value as RedactionDecision)}>
                        <option value="review_required">Decision required</option>
                        <option value="publish_as_is">Approved as shown</option>
                        <option value="redact">Apply black masks</option>
                      </select>
                    </div>
                    {draft.decision === "redact" ? (
                      <div className="redactionRegions">
                        {draft.regions.map((region, index) => (
                          <div className="redactionRegion" key={region.id}>
                            <div><strong>Mask {index + 1}</strong><button className="textButton" type="button" onClick={() => removeRegion(videoSegment.segment_id, region.id)}>Remove</button></div>
                            <div className="redactionGrid">
                              <NumberField label="Start (seconds)" value={region.startSeconds} min={0} max={durationSeconds} step={0.001} onChange={(value) => updateRegion(videoSegment.segment_id, region.id, "startSeconds", value)} />
                              <NumberField label="End (seconds)" value={region.endSeconds} min={0.001} max={durationSeconds} step={0.001} onChange={(value) => updateRegion(videoSegment.segment_id, region.id, "endSeconds", value)} />
                              <NumberField label="Left (%)" value={region.xPercent} min={0} max={100} step={0.1} onChange={(value) => updateRegion(videoSegment.segment_id, region.id, "xPercent", value)} />
                              <NumberField label="Top (%)" value={region.yPercent} min={0} max={100} step={0.1} onChange={(value) => updateRegion(videoSegment.segment_id, region.id, "yPercent", value)} />
                              <NumberField label="Width (%)" value={region.widthPercent} min={0.1} max={100} step={0.1} onChange={(value) => updateRegion(videoSegment.segment_id, region.id, "widthPercent", value)} />
                              <NumberField label="Height (%)" value={region.heightPercent} min={0.1} max={100} step={0.1} onChange={(value) => updateRegion(videoSegment.segment_id, region.id, "heightPercent", value)} />
                              <label><span>Reason</span><select value={region.kind} onChange={(event) => updateRegion(videoSegment.segment_id, region.id, "kind", event.target.value)}><option value="personal_data">Personal data</option><option value="credential">Credential</option><option value="unrelated_content">Unrelated content</option><option value="other">Other</option></select></label>
                            </div>
                          </div>
                        ))}
                        <button className="secondary" type="button" onClick={() => addRegion(videoSegment)}>Add mask</button>
                      </div>
                    ) : null}
                  </article>
                );
              })}
              {!validRedactionDrafts(trajectory, redactionDrafts) ? <p className="privacyWarning">Choose a decision for every segment. Each redacted segment needs at least one valid mask.</p> : null}
            </div>
          ) : null}
        </div>
        <div className="decisionActions">
          <button className="danger" type="button" onClick={() => void submit("rejected")} disabled={busy}>Reject</button>
          <button className="secondary" type="button" onClick={() => void submit("rework_required")} disabled={busy}>Request rework</button>
          <button type="button" onClick={() => void submit("accepted")} disabled={busy || piiReview !== "passed" || (videoPlanMode === "prepare" && !validRedactionDrafts(trajectory, redactionDrafts))}>Accept trajectory</button>
        </div>
      </section>
    </section>
  );
}

const scoreOptions = [1, 2, 3, 4, 5].map((score) => <option key={score} value={score}>{score} / 5</option>);

function NumberField({ label, value, min, max, step, onChange }: {
  label: string;
  value: number;
  min: number;
  max: number;
  step: number;
  onChange: (value: string) => void;
}) {
  return <label><span>{label}</span><input type="number" value={value} min={min} max={max} step={step} onChange={(event) => onChange(event.target.value)} /></label>;
}

function validRedactionDrafts(trajectory: Trajectory, drafts: Record<string, SegmentRedactionDraft>) {
  return trajectory.capture.video_segments.length > 0 && trajectory.capture.video_segments.every((segment) => {
    const draft = drafts[segment.segment_id];
    if (!draft || draft.decision === "review_required") return false;
    if (draft.decision === "publish_as_is") return draft.regions.length === 0;
    const durationSeconds = (segment.end_ns - segment.start_ns) / 1_000_000_000;
    return draft.regions.length > 0 && draft.regions.length <= 100 && draft.regions.every((region) => {
      const values = [region.startSeconds, region.endSeconds, region.xPercent, region.yPercent, region.widthPercent, region.heightPercent];
      return values.every(Number.isFinite) && region.startSeconds >= 0 && region.endSeconds > region.startSeconds && region.endSeconds <= durationSeconds + 0.000_001 &&
        region.xPercent >= 0 && region.yPercent >= 0 && region.widthPercent > 0 && region.heightPercent > 0 &&
        region.xPercent + region.widthPercent <= 100 && region.yPercent + region.heightPercent <= 100;
    });
  });
}

function buildRedactionPlan(trajectory: Trajectory, drafts: Record<string, SegmentRedactionDraft>) {
  if (!validRedactionDrafts(trajectory, drafts)) throw new Error("Complete the privacy decision for every video segment.");
  return {
    schema_version: "redaction/v1",
    segments: trajectory.capture.video_segments.map((segment) => {
      const draft = drafts[segment.segment_id]!;
      return {
        segment_id: segment.segment_id,
        source_key: segment.key,
        decision: draft.decision,
        regions: draft.regions.map((region) => ({
          start_ns: Math.round(region.startSeconds * 1_000_000_000),
          end_ns: Math.round(region.endSeconds * 1_000_000_000),
          x: region.xPercent / 100,
          y: region.yPercent / 100,
          width: region.widthPercent / 100,
          height: region.heightPercent / 100,
          kind: region.kind,
        })),
      };
    }),
  };
}

function mutationHeaders(actor: string) {
  return { "Content-Type": "application/json", "X-Actor-ID": actor, "X-Request-ID": crypto.randomUUID() };
}

async function requestJSON<T = unknown>(input: string, init?: RequestInit): Promise<T> {
  const response = await fetch(input, init);
  const payload = await response.json() as T & { error?: { message?: string } };
  if (!response.ok) throw new Error(payload.error?.message ?? `Request failed with ${response.status}`);
  return payload;
}

async function requestEmpty(input: string, init?: RequestInit) {
  const response = await fetch(input, init);
  if (response.ok) return;
  const payload = await response.json() as { error?: { message?: string } };
  throw new Error(payload.error?.message ?? `Request failed with ${response.status}`);
}

function artifactURL(sessionID: string, key: string) {
  const prefix = `raw/sessions/${sessionID}/`;
  if (!key.startsWith(prefix)) return "";
  const path = key.slice(prefix.length).split("/").map(encodeURIComponent).join("/");
  return `/api/control-plane/v1/sessions/${encodeURIComponent(sessionID)}/artifacts/${path}`;
}

function messageFor(error: unknown) {
  return error instanceof Error ? error.message : String(error);
}

function formatBytes(bytes: number) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MiB`;
}
