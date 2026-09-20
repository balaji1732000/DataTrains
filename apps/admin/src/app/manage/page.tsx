import Link from "next/link";
import { AccountMenu } from "@/components/AccountMenu";
import { OperationsWorkspace } from "@/components/OperationsWorkspace";
import { webAuthEnabled } from "@/lib/oidc";
import { requirePageSession } from "@/lib/page-auth";

export const dynamic = "force-dynamic";

export default async function ManagePage() {
  const identityMode = webAuthEnabled();
  const session = await requirePageSession("/manage");
  return (
    <main>
      <header className="topbar">
        <div>
          <p className="eyebrow">Trajectory Platform</p>
          <h1>Project operations</h1>
          <p className="subtitle">Create a collection campaign, assign its first task, and publish accepted work.</p>
        </div>
        <nav><Link className="navLink" href="/">Review queue</Link><AccountMenu session={session} /></nav>
      </header>
      <OperationsWorkspace identityMode={identityMode} />
    </main>
  );
}
