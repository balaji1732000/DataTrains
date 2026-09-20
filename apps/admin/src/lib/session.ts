import "server-only";

import { createHash } from "node:crypto";
import { EncryptJWT, jwtDecrypt, type JWTPayload } from "jose";
import { cookies } from "next/headers";
import { applicationBaseURL, oidc, oidcConfiguration } from "@/lib/oidc";

const sessionLifetimeSeconds = 30 * 24 * 60 * 60;
const transactionLifetimeSeconds = 10 * 60;
const invitationLifetimeSeconds = 24 * 60 * 60;

type SessionPayload = JWTPayload & {
  kind: "session";
  accessToken: string;
  refreshToken?: string;
  accessTokenExpiresAt: number;
  subject: string;
  email?: string;
  name?: string;
};

export type WebSession = Pick<SessionPayload,
  "accessToken" | "accessTokenExpiresAt" | "subject"> & {
    refreshToken?: string | undefined;
    email?: string | undefined;
    name?: string | undefined;
  };

type TransactionPayload = JWTPayload & {
  kind: "transaction";
  state: string;
  nonce: string;
  codeVerifier: string;
  returnTo: string;
};

type OIDCTransaction = Pick<TransactionPayload, "state" | "nonce" | "codeVerifier" | "returnTo">;

type InvitationPayload = JWTPayload & {
  kind: "invitation";
  token: string;
};

let encryptionKey: Uint8Array | undefined;

function key() {
  if (!encryptionKey) {
    const secret = process.env.DATATRAINS_SESSION_SECRET?.trim();
    if (!secret || secret.length < 32) {
      throw new Error("DATATRAINS_SESSION_SECRET must contain at least 32 random characters");
    }
    encryptionKey = createHash("sha256").update(secret, "utf8").digest();
  }
  return encryptionKey;
}

function cookieNames() {
  const secure = applicationBaseURL().protocol === "https:";
  return {
    secure,
    session: secure ? "__Host-datatrains-session" : "datatrains-session",
    transaction: secure ? "__Host-datatrains-oidc" : "datatrains-oidc",
    invitation: secure ? "__Host-datatrains-invitation" : "datatrains-invitation",
  };
}

function options(maxAge: number) {
  const { secure } = cookieNames();
  return { httpOnly: true, secure, sameSite: "lax" as const, path: "/", maxAge };
}

async function seal(payload: JWTPayload, lifetime: number) {
  return new EncryptJWT(payload)
    .setProtectedHeader({ alg: "dir", enc: "A256GCM", typ: "JWT" })
    .setIssuedAt()
    .setExpirationTime(Math.floor(Date.now() / 1000) + lifetime)
    .encrypt(key());
}

async function open<T extends JWTPayload>(value: string | undefined, kind: string): Promise<T | null> {
  if (!value) return null;
  try {
    const { payload, protectedHeader } = await jwtDecrypt<T>(value, key(), {
      keyManagementAlgorithms: ["dir"],
      contentEncryptionAlgorithms: ["A256GCM"],
      clockTolerance: 5,
    });
    if (protectedHeader.typ !== "JWT" || payload.kind !== kind) return null;
    return payload;
  } catch {
    return null;
  }
}

export async function writeOIDCTransaction(transaction: OIDCTransaction) {
  const token = await seal({ kind: "transaction", ...transaction }, transactionLifetimeSeconds);
  const jar = await cookies();
  jar.set(cookieNames().transaction, token, options(transactionLifetimeSeconds));
}

export async function takeOIDCTransaction() {
  const jar = await cookies();
  const name = cookieNames().transaction;
  const transaction = await open<TransactionPayload>(jar.get(name)?.value, "transaction");
  jar.delete(name);
  return transaction;
}

export async function writeSession(session: WebSession) {
  const token = await seal({ kind: "session", ...session }, sessionLifetimeSeconds);
  if (token.length > 3_800) throw new Error("OpenID session exceeds the secure cookie size limit");
  const jar = await cookies();
  jar.set(cookieNames().session, token, options(sessionLifetimeSeconds));
}

export async function readSession(): Promise<WebSession | null> {
  const jar = await cookies();
  const payload = await open<SessionPayload>(jar.get(cookieNames().session)?.value, "session");
  if (!payload) return null;
  return {
    accessToken: payload.accessToken,
    refreshToken: payload.refreshToken,
    accessTokenExpiresAt: payload.accessTokenExpiresAt,
    subject: payload.subject,
    email: payload.email,
    name: payload.name,
  };
}

export async function clearSession() {
  const jar = await cookies();
  jar.delete(cookieNames().session);
  jar.delete(cookieNames().transaction);
}

export async function writePendingInvitation(token: string) {
  if (!/^[A-Za-z0-9_-]{43}$/.test(token)) throw new Error("invalid invitation token");
  const sealed = await seal({ kind: "invitation", token }, invitationLifetimeSeconds);
  const jar = await cookies();
  jar.set(cookieNames().invitation, sealed, options(invitationLifetimeSeconds));
}

export async function readPendingInvitation() {
  const jar = await cookies();
  const payload = await open<InvitationPayload>(jar.get(cookieNames().invitation)?.value, "invitation");
  return payload?.token ?? null;
}

export async function clearPendingInvitation() {
  const jar = await cookies();
  jar.delete(cookieNames().invitation);
}

export async function validAccessToken() {
  const session = await readSession();
  if (!session) return null;
  if (session.accessTokenExpiresAt > Date.now() + 30_000) return session.accessToken;
  if (!session.refreshToken) {
    await clearSession();
    return null;
  }
  try {
    const tokens = await oidc.refreshTokenGrant(await oidcConfiguration(), session.refreshToken);
    const claims = tokens.claims();
    const refreshed: WebSession = {
      accessToken: tokens.access_token,
      refreshToken: tokens.refresh_token ?? session.refreshToken,
      accessTokenExpiresAt: Date.now() + (tokens.expires_in ?? 300) * 1_000,
      subject: claims?.sub ?? session.subject,
      email: typeof claims?.email === "string" ? claims.email : session.email,
      name: typeof claims?.name === "string" ? claims.name : session.name,
    };
    await writeSession(refreshed);
    return refreshed.accessToken;
  } catch {
    await clearSession();
    return null;
  }
}
