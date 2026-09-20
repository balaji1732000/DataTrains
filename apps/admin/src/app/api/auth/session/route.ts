import { readSession } from "@/lib/session";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

export async function GET() {
  const session = await readSession();
  if (!session) {
    return Response.json({ authenticated: false }, { status: 401 });
  }
  return Response.json({
    authenticated: true,
    user: { subject: session.subject, email: session.email, name: session.name },
  });
}
