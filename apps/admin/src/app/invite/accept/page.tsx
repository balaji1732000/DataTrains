import { InviteAcceptance } from "@/components/InviteAcceptance";
import { requirePageSession } from "@/lib/page-auth";

export const dynamic = "force-dynamic";

export default async function AcceptInvitationPage() {
  await requirePageSession("/invite/accept");
  return (
    <main className="loginPage">
      <section className="loginCard">
        <p className="eyebrow">DataTrains invitation</p>
        <h1>Join your workspace</h1>
        <InviteAcceptance />
      </section>
    </main>
  );
}
