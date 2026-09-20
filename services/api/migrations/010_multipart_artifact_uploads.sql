CREATE TABLE artifact_multipart_uploads (
  id text PRIMARY KEY,
  session_id text NOT NULL REFERENCES sessions(id),
  logical_key text NOT NULL UNIQUE,
  storage_key text NOT NULL UNIQUE,
  storage_upload_id text NOT NULL,
  sha256 char(64) NOT NULL,
  size_bytes bigint NOT NULL CHECK (size_bytes > 0),
  media_type text NOT NULL,
  part_size_bytes bigint NOT NULL CHECK (part_size_bytes >= 5242880),
  state text NOT NULL CHECK (state IN ('active', 'completed', 'aborted')),
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX artifact_multipart_uploads_expiry_idx
  ON artifact_multipart_uploads (state, expires_at);

CREATE TABLE artifact_multipart_parts (
  upload_id text NOT NULL REFERENCES artifact_multipart_uploads(id) ON DELETE CASCADE,
  part_number integer NOT NULL CHECK (part_number BETWEEN 1 AND 10000),
  etag text NOT NULL,
  size_bytes bigint NOT NULL CHECK (size_bytes > 0),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (upload_id, part_number)
);

ALTER TABLE artifact_multipart_uploads ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_multipart_parts ENABLE ROW LEVEL SECURITY;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'anon') THEN
    REVOKE ALL PRIVILEGES ON artifact_multipart_uploads, artifact_multipart_parts FROM anon;
  END IF;
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'authenticated') THEN
    REVOKE ALL PRIVILEGES ON artifact_multipart_uploads, artifact_multipart_parts FROM authenticated;
  END IF;
END
$$;
