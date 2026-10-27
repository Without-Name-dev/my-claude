-- Message queue for unified cross-device message synchronization
-- Supports offline caching and real-time status updates

CREATE TABLE IF NOT EXISTS message_queue (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    content TEXT NOT NULL,
    attachments TEXT, -- JSON array of attachment metadata
    status TEXT NOT NULL CHECK(status IN ('queued', 'running', 'completed', 'error', 'cancelled')),
    source TEXT NOT NULL CHECK(source IN ('terminal', 'ios', 'web')),
    error_message TEXT,
    created_at INTEGER NOT NULL,
    started_at INTEGER,
    completed_at INTEGER,
    seq INTEGER NOT NULL, -- Sequence number for offline recovery
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_queue_session_status ON message_queue(session_id, status, seq);
CREATE INDEX IF NOT EXISTS idx_queue_session_seq ON message_queue(session_id, seq DESC);

-- Queue sequence counter per session
CREATE TABLE IF NOT EXISTS queue_sequences (
    session_id TEXT PRIMARY KEY,
    last_seq INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);
