CREATE TABLE history_import_state (
  session_id TEXT PRIMARY KEY
                 REFERENCES sessions(id) ON DELETE CASCADE,
  dirty      INTEGER NOT NULL DEFAULT 1 CHECK (dirty IN (0,1)),
  generation INTEGER NOT NULL DEFAULT 1,
  updated_at INTEGER NOT NULL
) STRICT;
