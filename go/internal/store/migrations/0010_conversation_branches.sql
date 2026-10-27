-- Conversation branching: allow creating independent conversation branches from any message

-- Track conversation branches
CREATE TABLE IF NOT EXISTS conversation_branches (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    branch_id TEXT NOT NULL,
    parent_branch_id TEXT,
    branch_point_item_id TEXT NOT NULL,
    branch_point_order_seq INTEGER NOT NULL,
    name TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    last_activity_at INTEGER,
    PRIMARY KEY (session_id, branch_id),
    FOREIGN KEY (session_id, parent_branch_id) REFERENCES conversation_branches(session_id, branch_id) ON DELETE CASCADE
) STRICT;

CREATE INDEX idx_conversation_branches_session ON conversation_branches(session_id, created_at);
CREATE INDEX idx_conversation_branches_parent ON conversation_branches(session_id, parent_branch_id);

-- Add branch_id to timeline_items to support branch isolation
ALTER TABLE timeline_items ADD COLUMN branch_id TEXT;

-- Index for efficient branch queries
CREATE INDEX idx_timeline_items_session_branch ON timeline_items(session_id, branch_id, order_seq);

-- Track active branch per session
ALTER TABLE sessions ADD COLUMN active_branch_id TEXT;
