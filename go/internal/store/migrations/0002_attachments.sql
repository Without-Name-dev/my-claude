-- Attachment metadata index. The blob and its sidecar stay on disk; this row
-- exists so session cleanup can find the files without walking the upload tree.
-- Sidecar writes and SQLite writes cannot share a transaction, so callers write
-- the files first and compensate by deleting them when the insert fails.
CREATE TABLE attachments (
  id          TEXT PRIMARY KEY,
  session_id  TEXT NOT NULL
                  REFERENCES sessions(id) ON DELETE CASCADE,
  name        TEXT NOT NULL,
  media_type  TEXT NOT NULL DEFAULT '',
  size        INTEGER NOT NULL,
  sha256      TEXT NOT NULL,
  origin      TEXT NOT NULL DEFAULT 'user',
  storage_key TEXT NOT NULL,
  created_at  INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_attachments_session_created
  ON attachments(session_id, created_at);
