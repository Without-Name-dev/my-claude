CREATE TABLE submit_outbox (
  session_id          TEXT NOT NULL
                          REFERENCES sessions(id) ON DELETE CASCADE,
  request_id          TEXT NOT NULL,
  connector_id        TEXT NOT NULL
                          REFERENCES connectors(id) ON DELETE CASCADE,
  external_session_id TEXT NOT NULL DEFAULT '',
  payload_json        TEXT,
  payload_hash        TEXT NOT NULL,
  state               TEXT NOT NULL DEFAULT 'pending'
                          CHECK (state IN ('pending','accepted','failed')),
  error               TEXT,
  previous_status     TEXT NOT NULL,
  attempts            INTEGER NOT NULL DEFAULT 0,
  last_attempt_at     INTEGER,
  created_at          INTEGER NOT NULL,
  updated_at          INTEGER NOT NULL,
  PRIMARY KEY (session_id, request_id)
) STRICT;

CREATE INDEX idx_submit_outbox_connector_pending
  ON submit_outbox(connector_id, state, created_at);
