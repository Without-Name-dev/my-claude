-- Terminal metadata for the connectors-v2 API.
--
-- The PTY itself lives on the desktop and is never represented in SQLite; this
-- row exists so the server can authorize a stream, list the terminals a person
-- created, and survive a restart. Output history is kept in memory only: it is
-- a replay convenience, not a transcript.
CREATE TABLE connector_terminals (
  id             TEXT PRIMARY KEY,
  connector_id   TEXT NOT NULL
                     REFERENCES connectors(id) ON DELETE CASCADE,
  user_id        TEXT NOT NULL
                     REFERENCES users(id) ON DELETE CASCADE,
  session_id     TEXT NOT NULL DEFAULT '',
  label          TEXT NOT NULL DEFAULT '',
  root           TEXT NOT NULL DEFAULT '',
  cwd            TEXT NOT NULL DEFAULT '',
  shell          TEXT NOT NULL DEFAULT '',
  cols           INTEGER NOT NULL DEFAULT 80,
  rows           INTEGER NOT NULL DEFAULT 24,
  status         TEXT NOT NULL DEFAULT 'starting'
                     CHECK (status IN ('starting','running','exited')),
  exit_code      INTEGER,
  pid            INTEGER NOT NULL DEFAULT 0,
  scrollback_seq INTEGER NOT NULL DEFAULT 0,
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_connector_terminals_connector
  ON connector_terminals(connector_id, created_at);
CREATE INDEX idx_connector_terminals_user
  ON connector_terminals(user_id, created_at);
