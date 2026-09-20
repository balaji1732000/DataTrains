import "server-only";

import * as oidc from "openid-client";

let configurationPromise: Promise<oidc.Configuration> | undefined;

export function webAuthEnabled() {
  return process.env.DATATRAINS_AUTH_MODE === "oidc";
}

export function applicationBaseURL() {
  const fallback = process.env.NODE_ENV === "production" ? undefined : "http://localhost:3000";
  const value = process.env.DATATRAINS_APP_BASE_URL ?? fallback;
  if (!value) throw new Error("DATATRAINS_APP_BASE_URL is required in production");
  const url = new URL(value);
  if (url.username || url.password || url.search || url.hash || (url.pathname !== "/" && url.pathname !== "")) {
    throw new Error("DATATRAINS_APP_BASE_URL must be an origin without credentials, path, query, or fragment");
  }
  if (process.env.NODE_ENV === "production" && url.protocol !== "https:") {
    throw new Error("DATATRAINS_APP_BASE_URL must use HTTPS in production");
  }
  if (url.protocol !== "https:" && !(url.protocol === "http:" && ["localhost", "127.0.0.1", "::1"].includes(url.hostname))) {
    throw new Error("unencrypted application origins are restricted to localhost");
  }
  return url;
}

export function callbackURL() {
  return new URL("/api/auth/callback", applicationBaseURL());
}

export function safeReturnTo(value: string | null) {
  if (!value || !value.startsWith("/") || value.startsWith("//") || value.includes("\\") || value.length > 512) {
    return "/";
  }
  const parsed = new URL(value, applicationBaseURL());
  return parsed.origin === applicationBaseURL().origin ? `${parsed.pathname}${parsed.search}${parsed.hash}` : "/";
}

function required(name: string) {
  const value = process.env[name]?.trim();
  if (!value) throw new Error(`${name} is required when OpenID authentication is enabled`);
  return value;
}

export function oidcClientID() {
  return required("DATATRAINS_OIDC_CLIENT_ID");
}

export async function oidcConfiguration() {
  if (!configurationPromise) {
    const issuer = new URL(required("DATATRAINS_OIDC_ISSUER"));
    if (issuer.protocol !== "https:" || issuer.username || issuer.password || issuer.search || issuer.hash) {
      throw new Error("DATATRAINS_OIDC_ISSUER must be a clean HTTPS URL");
    }
    const clientID = oidcClientID();
    const clientSecret = required("DATATRAINS_OIDC_CLIENT_SECRET");
    const method = process.env.DATATRAINS_OIDC_TOKEN_AUTH_METHOD ?? "client_secret_basic";
    if (!new Set(["client_secret_basic", "client_secret_post"]).has(method)) {
      throw new Error("DATATRAINS_OIDC_TOKEN_AUTH_METHOD must be client_secret_basic or client_secret_post");
    }
    const authentication = method === "client_secret_post"
      ? oidc.ClientSecretPost(clientSecret)
      : oidc.ClientSecretBasic(clientSecret);
    configurationPromise = oidc.discovery(
      issuer,
      clientID,
      { client_secret: clientSecret, token_endpoint_auth_method: method },
      authentication,
    );
  }
  return configurationPromise;
}

export function authorizationParameters(codeChallenge: string, state: string, nonce: string) {
  const parameters: Record<string, string> = {
    redirect_uri: callbackURL().href,
    scope: process.env.DATATRAINS_OIDC_SCOPES ?? "openid profile email offline_access",
    response_type: "code",
    code_challenge: codeChallenge,
    code_challenge_method: "S256",
    state,
    nonce,
  };
  const audience = process.env.DATATRAINS_OIDC_AUDIENCE?.trim();
  if (audience) parameters.audience = audience;
  const connection = process.env.DATATRAINS_OIDC_CONNECTION?.trim();
  if (connection) {
    if (!/^[A-Za-z0-9_-]{1,128}$/.test(connection)) {
      throw new Error("DATATRAINS_OIDC_CONNECTION must be a valid OpenID connection name");
    }
    parameters.connection = connection;
    parameters.prompt = "login";
  }
  return parameters;
}

export { oidc };
