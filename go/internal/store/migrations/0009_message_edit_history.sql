-- Message editing support: track edit history and allow regenerating from edited messages

-- Track original message content before edits
CREATE TABLE IF NOT EXISTS message_edit_history (
    session_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    edit_seq INTEGER NOT NULL,
    content_json TEXT NOT NULL,
    edited_at INTEGER NOT NULL,
    PRIMARY KEY (session_id, message_id, edit_seq)
);

CREATE INDEX idx_message_edit_history_session ON message_edit_history(session_id, message_id);

-- Track which messages were edited to help with timeline truncation
ALTER TABLE timeline_items ADD COLUMN edited_at INTEGER;
ALTER TABLE timeline_items ADD COLUMN edit_count INTEGER DEFAULT 0;
