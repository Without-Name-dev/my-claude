-- Takeover is no longer a mode the person turns on: driving a conversation
-- from the phone is the default. Existing rows are migrated; new rows are
-- inserted with takeover = 1 explicitly.
UPDATE sessions SET takeover = 1 WHERE takeover = 0;

-- A /clear keeps the terminal process but changes the runtime session id. The
-- conversation follows the new id; the retired one must not be discovered as
-- its own session afterwards.
CREATE TABLE IF NOT EXISTS session_rebinds (
  connector_id     TEXT NOT NULL,
  from_external_id TEXT NOT NULL,
  to_external_id   TEXT NOT NULL,
  session_id       TEXT NOT NULL,
  created_at       INTEGER NOT NULL,
  PRIMARY KEY (connector_id, from_external_id)
);

CREATE INDEX IF NOT EXISTS idx_session_rebinds_session
  ON session_rebinds(session_id);
