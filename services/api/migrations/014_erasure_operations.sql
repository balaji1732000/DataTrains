-- Governance actors are immutable identity labels in the audit trail. They are
-- intentionally not ownership references: removing or consolidating an account
-- must never cascade into, or prevent, retention and erasure records.
ALTER TABLE retention_policies
  DROP CONSTRAINT IF EXISTS retention_policies_updated_by_fkey;
ALTER TABLE legal_holds
  DROP CONSTRAINT IF EXISTS legal_holds_placed_by_fkey,
  DROP CONSTRAINT IF EXISTS legal_holds_released_by_fkey;
ALTER TABLE deletion_requests
  DROP CONSTRAINT IF EXISTS deletion_requests_requested_by_fkey;

ALTER TABLE deletion_requests
  ADD COLUMN manual_requeues integer NOT NULL DEFAULT 0 CHECK (manual_requeues >= 0);
