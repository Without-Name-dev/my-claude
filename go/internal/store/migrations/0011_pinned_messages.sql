-- Pin messages functionality: allow users to pin important messages to the top

-- Add pinned_message_ids to sessions to track pinned messages (JSON array)
ALTER TABLE sessions ADD COLUMN pinned_message_ids TEXT DEFAULT '[]';

-- Index for sessions with pinned messages
CREATE INDEX idx_sessions_pinned ON sessions(id) WHERE pinned_message_ids != '[]';
