import { randomUUID } from "node:crypto";
import { NextRequest } from "next/server";
import { applicationBaseURL, webAuthEnabled } from "@/lib/oidc";
import { clearPendingInvitation, readPendingInvitation, validAccessToken } from "@/lib/session";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

export async function POST(request: NextRequest) {
  if (!webAuthEnabled() || request.headers.get("origin") !== applicationBaseURL().origin) {
    return Response.json({ error: { message: "Invitation request was rejected" } }, { status: 403 });
  }
  const [invitationToken, accessToken] = await Promise.all([readPendingInvitation(), validAccessToken()]);
  if (!accessToken) return Response.json({ error: { message: "Sign in is required" } }, { status: 401 });
  if (!invitationToken) return Response.json({ error: { message: "Invitation link is missing or expired" } }, { status: 400 });

  const base = process.env.TRAJECTORY_API_URL;
  if (!base) return Response.json({ error: { message: "The DataTrains API is not configured" } }, { status: 503 });
  try {
    const upstream = await fetch(new URL("/v1/invitations/accept", base), {
      method: "POST",
      cache: "no-store",
      headers: {
        Authorization: `Bearer ${accessToken}`,
        "Content-Type": "application/json",
        "X-Request-ID": randomUUID(),
      },
      body: JSON.stringify({ token: invitationToken }),
    });
    const payload: unknown = await upstream.json();
    if (upstream.ok) await clearPendingInvitation();
    return Response.json(payload, { status: upstream.status });
  } catch {
    return Response.json({ error: { message: "The DataTrains API is unavailable" } }, { status: 503 });
  }
}
