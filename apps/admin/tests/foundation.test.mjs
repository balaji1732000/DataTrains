import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

test("admin health route remains machine-readable", async () => {
  const source = await readFile(new URL("../src/app/api/health/route.ts", import.meta.url), "utf8");
  assert.match(source, /status:\s*"ok"/);
});

test("review console uses the live queue and enforces the PII acceptance gate", async () => {
  const source = await readFile(new URL("../src/components/ReviewWorkspace.tsx", import.meta.url), "utf8");
  assert.match(source, /\/v1\/review-queue/);
  assert.match(source, /review-claim/);
  assert.match(source, /piiReview !== "passed"/);
  assert.match(source, /Keep recordings private/);
  assert.match(source, /Prepare reviewed video/);
  assert.match(source, /redaction_plan:\s*redactionPlan/);
  assert.match(source, /validRedactionDrafts/);
  assert.match(source, /<Timeline/);
});

test("control-plane proxy preserves range requests for video review", async () => {
  const source = await readFile(new URL("../src/app/api/control-plane/[...path]/route.ts", import.meta.url), "utf8");
  assert.match(source, /"range"/);
  assert.match(source, /upstream\.body/);
  assert.match(source, /"traceparent"/);
  assert.match(source, /"x-cloud-trace-context"/);
});

test("production web identity uses PKCE and a server-managed encrypted session", async () => {
  const login = await readFile(new URL("../src/app/api/auth/login/route.ts", import.meta.url), "utf8");
  const oidc = await readFile(new URL("../src/lib/oidc.ts", import.meta.url), "utf8");
  const callback = await readFile(new URL("../src/app/api/auth/callback/route.ts", import.meta.url), "utf8");
  const session = await readFile(new URL("../src/lib/session.ts", import.meta.url), "utf8");
  assert.match(login, /randomPKCECodeVerifier/);
  assert.match(login, /randomState/);
  assert.match(login, /randomNonce/);
  assert.match(oidc, /DATATRAINS_OIDC_CONNECTION/);
  assert.match(oidc, /parameters\.connection = connection/);
  assert.match(oidc, /parameters\.prompt = "login"/);
  assert.match(callback, /expectedState/);
  assert.match(callback, /expectedNonce/);
  assert.match(session, /A256GCM/);
  assert.match(session, /httpOnly:\s*true/);
  assert.match(session, /refreshTokenGrant/);
  assert.doesNotMatch(session, /localStorage/);
});

test("web sign-out revokes the refresh token before clearing the session", async () => {
  const source = await readFile(new URL("../src/app/api/auth/logout/route.ts", import.meta.url), "utf8");
  assert.match(source, /readSession/);
  assert.match(source, /tokenRevocation/);
  assert.match(source, /token_type_hint: "refresh_token"/);
  assert.match(source, /clearSession/);
});

test("production control-plane proxy derives authorization from the server session", async () => {
  const source = await readFile(new URL("../src/app/api/control-plane/[...path]/route.ts", import.meta.url), "utf8");
  assert.match(source, /validAccessToken/);
  assert.match(source, /headers\.set\("authorization", `Bearer \$\{token\}`\)/);
  assert.match(source, /request\.headers\.get\("origin"\)/);
  const forwarded = source.match(/const forwardedRequestHeaders = \[([\s\S]*?)\] as const;/)?.[1] ?? "";
  assert.doesNotMatch(forwarded, /"x-actor-id"/);
  assert.match(source, /request\.headers\.get\("x-actor-id"\) \?\? "local-ui"/);
});

test("project operations use live creation, assignment, and release APIs", async () => {
  const source = await readFile(new URL("../src/components/OperationsWorkspace.tsx", import.meta.url), "utf8");
  for (const endpoint of ["/v1/campaigns", "/v1/projects", "/releases"]) {
    assert.match(source, new RegExp(endpoint.replaceAll("/", "\\/")));
  }
  assert.match(source, /input_assets:\s*form\.inputAssets/);
  assert.match(source, /finish_criteria:\s*form\.finishCriteria/);
  assert.match(source, /processing-jobs\/dead-letter/);
  assert.match(source, /redaction-jobs\/dead-letter/);
  assert.match(source, /Retry safely/);
  assert.match(source, /processing-jobs\/\$\{encodeURIComponent\(jobID\)\}\/retry/);
  assert.match(source, /v1\/legal-holds/);
  assert.match(source, /v1\/deletion-requests/);
  assert.match(source, /v1\/retention-purge-requests/);
  assert.match(source, /Retry retention purge/);
  assert.match(source, /Retry redaction/);
  assert.match(source, /Erase artifacts/);
  assert.match(source, /cannot be undone/);
  assert.doesNotMatch(source, /Target trajectories<\/span>\s*<strong>100/);
});
