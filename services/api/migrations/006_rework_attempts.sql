ALTER TABLE sessions
  ADD COLUMN rework_of_session_id text REFERENCES sessions(id);

CREATE UNIQUE INDEX sessions_one_rework_successor
  ON sessions (rework_of_session_id)
  WHERE rework_of_session_id IS NOT NULL;

-- Preserve reviewed captures and create a fresh assignment attempt for any
-- rework decisions made before this migration was installed.
INSERT INTO sessions (id, assignment_id, state, rework_of_session_id, created_at, updated_at)
SELECT
  'sess_' || md5(original.id || ':rework:v1'),
  original.assignment_id,
  'ASSIGNED',
  original.id,
  now(),
  now()
FROM sessions original
WHERE original.state = 'REWORK_REQUIRED'
  AND NOT EXISTS (
    SELECT 1 FROM sessions successor
    WHERE successor.rework_of_session_id = original.id
  )
ON CONFLICT DO NOTHING;
