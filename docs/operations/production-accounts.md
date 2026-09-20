# Production provider ownership

This record fixes the provider/account mapping used for DataTrains Production.
Operators must verify the visible account and resource identifiers before any
deployment, credential, billing, or irreversible action.

| Provider | Production account or organization | Production resource |
| --- | --- | --- |
| Google Cloud | `yuneekwayai@gmail.com` | Project `datatrains-production` (`963738422674`) in `asia-southeast1` |
| Cloudflare | `sampathbalaji777@gmail.com` | Private R2 bucket `datatrains-production-artifacts` in the account selected by that login |
| Auth0 | `sampathbalaji777@gmail.com` | Team `balaji's Team`; tenant `dev-pvoa8kc6eirqwbti.au.auth0.com` |
| Supabase | Organization `balaji1732000's Org` (`tdsaspmiwwyfctdzaipb`) | Project `DataTrains Production` (`mfmeeyvtwybmhyulouif`) in `ap-southeast-1` |
| Vercel | GitHub-authenticated account `balaji1732000` | Project `datatrains-admin` in team `balaji1732000s-projects`; production alias `https://datatrains-admin.vercel.app` |
| GitHub | GitHub account `balaji1732000` | Private repository `balaji1732000/DataTrains`; OAuth application `DataTrains Production` for the Auth0 GitHub social connection |

The Supabase management connection reports organization membership and project
metadata but does not expose the member email. Do not infer or substitute an
email address for Supabase without separately verifying it in the Supabase
dashboard.

Use workload identities at runtime. Human provider accounts are for controlled
administration only; no human account credential belongs in source control,
collector builds, Vercel client bundles, or container images.
