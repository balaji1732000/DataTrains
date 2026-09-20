import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { URL } from "node:url";

test("collector onboarding exposes privacy and explicit consent boundaries", async () => {
  const source = await readFile(new URL("../src/CollectorApp.tsx", import.meta.url), "utf8");
  assert.match(source, /Not recording/);
  assert.match(source, /Clipboard content is never captured by default/);
  assert.match(source, /consentDocument\.version/);
  assert.match(source, /run_preflight/);
  assert.match(source, /disabled=\{!props\.allChecksPass/);
  assert.match(source, /Unfinished recording found/);
  assert.match(source, /start_capture/);
  assert.match(source, /pause_capture/);
  assert.match(source, /finish_capture/);
  assert.match(source, /submit_finalized_capture/);
  assert.match(source, /Select output/);
  assert.match(source, /Task history/);
  assert.match(source, /COMPLETED/);
  assert.match(source, /UNDER REVIEW/);
  assert.match(source, /REWORK REQUESTED/);
  assert.match(source, /trajectory\.contributorId/);
  assert.match(source, /Sign out \/ Switch contributor/);
});

test("production collector keeps OpenID credentials in the native security boundary", async () => {
  const [ui, auth, submission] = await Promise.all([
    readFile(new URL("../src/CollectorApp.tsx", import.meta.url), "utf8"),
    readFile(new URL("../src-tauri/src/auth.rs", import.meta.url), "utf8"),
    readFile(new URL("../src-tauri/src/submission.rs", import.meta.url), "utf8"),
  ]);

  assert.match(ui, /authenticated_api_request/);
  assert.match(ui, /Production identity is available only in the native DataTrains Collector/);
  assert.doesNotMatch(ui, /localStorage\.setItem\([^)]*(?:access|refresh)[_-]?token/i);

  assert.match(auth, /PkceCodeChallenge::new_random_sha256/);
  assert.match(auth, /CsrfToken::new_random/);
  assert.match(auth, /Nonce::new_random/);
  assert.match(auth, /127\.0\.0\.1/);
  assert.match(auth, /open system browser for sign-in/);
  assert.match(auth, /redirect\(oidc_reqwest::redirect::Policy::none\(\)\)/);
  assert.match(auth, /AccessTokenHash::from_token/);
  assert.match(auth, /KEYRING_SERVICE/);
  assert.match(auth, /operating-system credential vault/);
  assert.match(auth, /revoke_refresh_token/);
  assert.match(auth, /bearer_auth\(access_token\)/);

  assert.match(submission, /#\[serde\(skip\)\]\s+pub access_token/);
  assert.match(submission, /builder\.bearer_auth\(token\)/);
});
