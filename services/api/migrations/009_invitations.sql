ALTER TABLE oidc_identities
  ADD COLUMN email_verified_at_link_time boolean NOT NULL DEFAULT false;

CREATE UNIQUE INDEX accounts_email_unique
  ON accounts (lower(email))
  WHERE email IS NOT NULL;

CREATE TABLE invitations (
  id text PRIMARY KEY,
  organization_id text NOT NULL REFERENCES organizations(id),
  role text NOT NULL CHECK (role IN ('admin', 'reviewer', 'contributor')),
  email text NOT NULL,
  token_hash char(64) NOT NULL UNIQUE,
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'accepted', 'revoked', 'expired')),
  invited_by text NOT NULL REFERENCES accounts(id),
  expires_at timestamptz NOT NULL,
  accepted_by text REFERENCES accounts(id),
  accepted_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK (expires_at > created_at),
  CHECK ((status = 'accepted' AND accepted_by IS NOT NULL AND accepted_at IS NOT NULL)
      OR (status <> 'accepted' AND accepted_by IS NULL AND accepted_at IS NULL))
);

CREATE INDEX invitations_pending_email_idx
  ON invitations (lower(email), expires_at)
  WHERE status = 'pending';

ALTER TABLE invitations ENABLE ROW LEVEL SECURITY;
