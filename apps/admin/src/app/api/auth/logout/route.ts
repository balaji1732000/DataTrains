import { NextRequest, NextResponse } from "next/server";
import { applicationBaseURL, oidc, oidcClientID, oidcConfiguration } from "@/lib/oidc";
import { clearSession, readSession } from "@/lib/session";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

export async function POST(request: NextRequest) {
  if (request.headers.get("origin") !== applicationBaseURL().origin) {
    return Response.json({ error: { code: "invalid_origin", message: "Sign-out request was rejected" } }, { status: 403 });
  }
  const session = await readSession();
  let configuration: Awaited<ReturnType<typeof oidcConfiguration>> | undefined;
  let revocationFailed = false;
  try {
    configuration = await oidcConfiguration();
    if (session?.refreshToken) {
      await oidc.tokenRevocation(configuration, session.refreshToken, { token_type_hint: "refresh_token" });
    }
  } catch {
    revocationFailed = Boolean(session?.refreshToken);
  }
  await clearSession();
  const home = new URL("/login", applicationBaseURL());
  if (revocationFailed) home.searchParams.set("warning", "revocation_failed");
  try {
    const destination = oidc.buildEndSessionUrl(configuration ?? await oidcConfiguration(), {
      client_id: oidcClientID(),
      post_logout_redirect_uri: home.href,
    });
    return NextResponse.redirect(destination, 303);
  } catch {
    return NextResponse.redirect(home, 303);
  }
}
