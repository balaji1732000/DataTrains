import { NextRequest, NextResponse } from "next/server";
import { applicationBaseURL, callbackURL, oidc, oidcConfiguration, webAuthEnabled } from "@/lib/oidc";
import { takeOIDCTransaction, writeSession } from "@/lib/session";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

export async function GET(request: NextRequest) {
  if (!webAuthEnabled()) return NextResponse.redirect(applicationBaseURL());
  const transaction = await takeOIDCTransaction();
  if (!transaction) {
    return NextResponse.redirect(new URL("/login?error=session_expired", applicationBaseURL()));
  }
  try {
    const currentURL = callbackURL();
    currentURL.search = request.nextUrl.search;
    const tokens = await oidc.authorizationCodeGrant(
      await oidcConfiguration(),
      currentURL,
      {
        pkceCodeVerifier: transaction.codeVerifier,
        expectedState: transaction.state,
        expectedNonce: transaction.nonce,
        idTokenExpected: true,
      },
    );
    const claims = tokens.claims();
    if (!claims?.sub) throw new Error("verified ID token has no subject");
    await writeSession({
      accessToken: tokens.access_token,
      refreshToken: tokens.refresh_token,
      accessTokenExpiresAt: Date.now() + (tokens.expires_in ?? 300) * 1_000,
      subject: claims.sub,
      email: typeof claims.email === "string" ? claims.email : undefined,
      name: typeof claims.name === "string" ? claims.name : undefined,
    });
    return NextResponse.redirect(new URL(transaction.returnTo, applicationBaseURL()));
  } catch {
    return NextResponse.redirect(new URL("/login?error=authentication_failed", applicationBaseURL()));
  }
}
