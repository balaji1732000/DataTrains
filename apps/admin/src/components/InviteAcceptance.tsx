"use client";

import Link from "next/link";
import { useState } from "react";

type Accepted = { role: string; contributor_id?: string };

export function InviteAcceptance() {
  const [accepted, setAccepted] = useState<Accepted | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function accept() {
    setBusy(true);
    setError(null);
    try {
      const response = await fetch("/api/invitations/accept", { method: "POST" });
      const payload = await response.json() as Accepted & { error?: { message?: string } };
      if (!response.ok) throw new Error(payload.error?.message ?? "Invitation could not be accepted");
      setAccepted(payload);
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : String(caught));
    } finally {
      setBusy(false);
    }
  }

  if (accepted) {
    return (
      <>
        <p className="successMessage"><strong>Account ready.</strong><span>Your {accepted.role} access is active.</span></p>
        {accepted.role === "contributor" ? (
          <p className="subtitle">You can close this window and sign in to the DataTrains Collector with this same account.</p>
        ) : <Link className="primaryLink" href="/">Open DataTrains</Link>}
      </>
    );
  }
  return (
    <>
      <p className="subtitle">Confirm this invitation after signing in with the exact email address that received it.</p>
      {error ? <p className="errorText" role="alert">{error}</p> : null}
      <button type="button" onClick={() => void accept()} disabled={busy}>{busy ? "Accepting…" : "Accept invitation"}</button>
    </>
  );
}
