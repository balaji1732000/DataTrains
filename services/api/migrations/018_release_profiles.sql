ALTER TABLE dataset_releases
  ADD COLUMN release_profile text NOT NULL DEFAULT 'trajectory_only'
  CHECK (release_profile IN ('trajectory_only', 'redacted_video'));
