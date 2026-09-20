import type { NextRequest } from "next/server";
import { applicationBaseURL, webAuthEnabled } from "@/lib/oidc";
import { validAccessToken } from "@/lib/session";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";

type Context = { params: Promise<{ path: string[] }> };

const forwardedRequestHeaders = [
  "accept",
  "content-type",
  "range",
	"traceparent",
	"x-cloud-trace-context",
  "x-artifact-sha256",
  "x-request-id",
] as const;

const mutationMethods = new Set(["POST", "PUT", "PATCH", "DELETE"]);

async function proxy(request: NextRequest, context: Context) {
  const { path } = await context.params;
  if (path.length === 0 || path.some((part) => part === "" || part === "." || part === "..")) {
    return Response.json({ error: { code: "invalid_path", message: "Invalid control-plane path" } }, { status: 400 });
  }
  const base = process.env.TRAJECTORY_API_URL ?? "http://127.0.0.1:8080";
  const target = new URL(`/` + path.map(encodeURIComponent).join("/"), base);
  target.search = request.nextUrl.search;
  const headers = new Headers();
  for (const name of forwardedRequestHeaders) {
    const value = request.headers.get(name);
    if (value) headers.set(name, value);
  }
  if (webAuthEnabled()) {
    if (mutationMethods.has(request.method) && request.headers.get("origin") !== applicationBaseURL().origin) {
      return Response.json({ error: { code: "invalid_origin", message: "Request origin was rejected" } }, { status: 403 });
    }
    const token = await validAccessToken();
    if (!token) {
      return Response.json({ error: { code: "unauthenticated", message: "Sign in is required" } }, { status: 401 });
    }
    headers.set("authorization", `Bearer ${token}`);
  } else {
    headers.set("x-actor-id", request.headers.get("x-actor-id") ?? "local-ui");
  }
  try {
    const method = request.method;
    const body = method === "GET" || method === "HEAD" ? undefined : await request.arrayBuffer();
    const init: RequestInit = { method, headers, cache: "no-store" };
    if (body) init.body = body;
    const upstream = await fetch(target, init);
    const responseHeaders = new Headers(upstream.headers);
    responseHeaders.delete("connection");
    responseHeaders.delete("transfer-encoding");
    return new Response(method === "HEAD" ? null : upstream.body, {
      status: upstream.status,
      headers: responseHeaders,
    });
  } catch {
    return Response.json(
      { error: { code: "control_plane_unavailable", message: "The DataTrains API is unavailable" } },
      { status: 503 },
    );
  }
}

export const GET = proxy;
export const HEAD = proxy;
export const POST = proxy;
export const PUT = proxy;
export const DELETE = proxy;
