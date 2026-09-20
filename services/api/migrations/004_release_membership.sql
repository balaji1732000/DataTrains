CREATE TABLE release_sessions (
  release_id text NOT NULL REFERENCES dataset_releases(id),
  session_id text NOT NULL UNIQUE REFERENCES sessions(id),
  ordinal integer NOT NULL CHECK (ordinal >= 0),
  PRIMARY KEY (release_id, session_id),
  UNIQUE (release_id, ordinal)
);

CREATE INDEX release_sessions_session_idx ON release_sessions (session_id);
