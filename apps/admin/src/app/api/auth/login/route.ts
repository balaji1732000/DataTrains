import { NextRequest, NextResponse } from "next/server";
import { applicationBaseURL, authorizationParameters, oidc, oidcConfiguration, safeReturnTo, webAuthEnabled } from "@/lib/oidc";
import { writeOIDCTransaction } from "@/lib/session";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

export async function GET(request: NextRequest) {
  if (!webAuthEnabled()) return NextResponse.redirect(applicationBaseURL());
  const configuration = await oidcConfiguration();
  const codeVerifier = oidc.randomPKCECodeVerifier();
  const codeChallenge = await oidc.calculatePKCECodeChallenge(codeVerifier);
  const state = oidc.randomState();
  const nonce = oidc.randomNonce();
  const returnTo = safeReturnTo(request.nextUrl.searchParams.get("return_to"));
  await writeOIDCTransaction({ state, nonce, codeVerifier, returnTo });
  const destination = oidc.buildAuthorizationUrl(
    configuration,
    authorizationParameters(codeChallenge, state, nonce),
  );
  return NextResponse.redirect(destination);
}
