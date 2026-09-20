import type { WebSession } from "@/lib/session";

export function AccountMenu({ session }: { session: WebSession | null }) {
  if (!session) return <span className="localBadge"><i /> Local V1</span>;
  return (
    <span className="accountMenu">
      <span>{session.name || session.email || "Signed in"}</span>
      <form action="/api/auth/logout" method="post"><button type="submit" className="textButton">Sign out</button></form>
    </span>
  );
}
