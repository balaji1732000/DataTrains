import { ReviewWorkspace } from "@/components/ReviewWorkspace";
import { AccountMenu } from "@/components/AccountMenu";
import { webAuthEnabled } from "@/lib/oidc";
import { requirePageSession } from "@/lib/page-auth";
import Link from "next/link";

export const dynamic = "force-dynamic";

export default async function DashboardPage() {
  const identityMode = webAuthEnabled();
  const session = await requirePageSession("/");
  return (
    <main>
      <header className="topbar">
        <div>
          <p className="eyebrow">Trajectory Platform</p>
          <h1>Review console</h1>
          <p className="subtitle">Inspect synchronized capture evidence and make an auditable QA decision.</p>
        </div>
        <nav><Link className="navLink" href="/manage">Manage projects</Link><AccountMenu session={session} /></nav>
      </header>
      <ReviewWorkspace identityMode={identityMode} />
    </main>
  );
}
