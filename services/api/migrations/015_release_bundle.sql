ALTER TABLE dataset_releases
  ADD COLUMN bundle_hash char(64),
  ADD COLUMN bundle_size bigint CHECK (bundle_size > 0);

ALTER TABLE dataset_releases
  ADD CONSTRAINT dataset_releases_bundle_pair CHECK (
    (bundle_hash IS NULL AND bundle_size IS NULL)
    OR (bundle_hash IS NOT NULL AND bundle_size IS NOT NULL)
  );
