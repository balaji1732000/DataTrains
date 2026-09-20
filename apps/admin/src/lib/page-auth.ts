import "server-only";

import { redirect } from "next/navigation";
import { safeReturnTo, webAuthEnabled } from "@/lib/oidc";
import { readSession, type WebSession } from "@/lib/session";

export async function requirePageSession(returnTo: string): Promise<WebSession | null> {
  if (!webAuthEnabled()) return null;
  const session = await readSession();
  if (!session) {
    redirect(`/login?return_to=${encodeURIComponent(safeReturnTo(returnTo))}`);
  }
  return session;
}
