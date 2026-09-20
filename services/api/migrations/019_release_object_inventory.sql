ALTER TABLE dataset_releases
  ADD COLUMN object_keys jsonb;

UPDATE dataset_releases r
SET object_keys = (
  SELECT jsonb_agg(key ORDER BY key)
  FROM (
    SELECT 'bundles/' || r.id || '.zip' AS key
    UNION ALL SELECT 'releases/' || r.id || '/README.md'
    UNION ALL SELECT 'releases/' || r.id || '/checksums.sha256'
    UNION ALL SELECT 'releases/' || r.id || '/manifest.json'
    UNION ALL SELECT 'releases/' || r.id || '/schema.json'
    UNION ALL SELECT 'releases/' || r.id || '/trajectories.jsonl'
    UNION ALL
    SELECT 'releases/' || r.id || '/trajectories/' || value || '.json'
    FROM jsonb_array_elements_text(r.source_session_ids)
  ) inventory
);

ALTER TABLE dataset_releases
  ALTER COLUMN object_keys SET NOT NULL,
  ADD CONSTRAINT dataset_releases_object_keys_check CHECK (
    jsonb_typeof(object_keys)='array' AND jsonb_array_length(object_keys) >= 7
  );
