CREATE TABLE accounts (
  id text PRIMARY KEY,
  display_name text NOT NULL,
  email text,
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'disabled')),
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE oidc_identities (
  id text PRIMARY KEY,
  account_id text NOT NULL REFERENCES accounts(id),
  issuer text NOT NULL,
  subject text NOT NULL,
  email_at_link_time text,
  created_at timestamptz NOT NULL DEFAULT now(),
  last_authenticated_at timestamptz,
  UNIQUE (issuer, subject)
);

CREATE TABLE organization_memberships (
  organization_id text NOT NULL REFERENCES organizations(id),
  account_id text NOT NULL REFERENCES accounts(id),
  role text NOT NULL CHECK (role IN ('admin', 'reviewer', 'contributor')),
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'revoked')),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (organization_id, account_id, role)
);

ALTER TABLE contributors
  ADD COLUMN account_id text UNIQUE REFERENCES accounts(id);

CREATE INDEX organization_memberships_account_idx
  ON organization_memberships (account_id, status, role);

CREATE INDEX oidc_identities_account_idx ON oidc_identities (account_id);
