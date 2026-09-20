ALTER TABLE reviews
  ADD COLUMN pii_review text NOT NULL DEFAULT 'pending'
  CHECK (pii_review IN ('pending', 'passed', 'failed'));

CREATE TABLE review_claims (
  session_id text PRIMARY KEY REFERENCES sessions(id),
  reviewer_id text NOT NULL,
  claimed_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL,
  CHECK (expires_at > claimed_at)
);

CREATE INDEX review_claims_expiry_idx ON review_claims (expires_at);
