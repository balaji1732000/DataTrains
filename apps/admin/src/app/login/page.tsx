import Link from "next/link";
import { redirect } from "next/navigation";
import { safeReturnTo, webAuthEnabled } from "@/lib/oidc";
import { readSession } from "@/lib/session";

type Props = { searchParams: Promise<{ error?: string; warning?: string; return_to?: string }> };

export const dynamic = "force-dynamic";

export default async function LoginPage({ searchParams }: Props) {
  if (!webAuthEnabled()) redirect("/");
  const { error, warning, return_to: requestedReturnTo } = await searchParams;
  const returnTo = safeReturnTo(requestedReturnTo ?? null);
  if (await readSession()) redirect(returnTo);
  return (
    <main className="loginPage">
      <section className="loginCard">
        <p className="eyebrow">DataTrains</p>
        <h1>Sign in to your workspace</h1>
        <p className="subtitle">Use your invited professional account. Passwords, MFA, and account recovery are handled securely by the identity provider.</p>
        {error ? <p className="errorText">Sign-in could not be completed. Please start again.</p> : null}
        {warning === "revocation_failed" ? <p className="errorText">You were signed out locally, but the identity provider could not confirm remote session revocation. Contact support if this was unexpected.</p> : null}
        <Link className="primaryLink" href={`/api/auth/login?return_to=${encodeURIComponent(returnTo)}`}>Continue to sign in</Link>
      </section>
    </main>
  );
}
