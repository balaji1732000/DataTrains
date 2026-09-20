import { NextRequest, NextResponse } from "next/server";
import { applicationBaseURL, webAuthEnabled } from "@/lib/oidc";
import { readSession, writePendingInvitation } from "@/lib/session";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

export async function GET(request: NextRequest) {
  if (!webAuthEnabled()) return NextResponse.redirect(applicationBaseURL());
  const token = request.nextUrl.searchParams.get("token") ?? "";
  try {
    await writePendingInvitation(token);
  } catch {
    return NextResponse.redirect(new URL("/login?error=invalid_invitation", applicationBaseURL()));
  }
  if (await readSession()) return NextResponse.redirect(new URL("/invite/accept", applicationBaseURL()));
  return NextResponse.redirect(new URL("/api/auth/login?return_to=%2Finvite%2Faccept", applicationBaseURL()));
}
