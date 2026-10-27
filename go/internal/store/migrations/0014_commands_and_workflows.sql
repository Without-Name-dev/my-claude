-- Session configuration and mode settings
-- Support for effort level, model selection, fast mode, and auto mode

CREATE TABLE IF NOT EXISTS session_configs (
    session_id TEXT PRIMARY KEY,
    effort_level TEXT DEFAULT 'medium' CHECK(effort_level IN ('low', 'medium', 'high', 'xhigh', 'max', 'auto')),
    model_name TEXT DEFAULT 'claude-sonnet-5-5',
    fast_mode INTEGER DEFAULT 0,
    auto_mode INTEGER DEFAULT 0,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_session_configs_updated ON session_configs(updated_at);

-- Agent execution tracking for workflow visualization
CREATE TABLE IF NOT EXISTS agent_executions (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    parent_id TEXT, -- NULL for root agents
    label TEXT NOT NULL,
    phase TEXT,
    status TEXT NOT NULL CHECK(status IN ('pending', 'running', 'completed', 'error', 'cancelled')),
    started_at INTEGER,
    finished_at INTEGER,
    token_count INTEGER DEFAULT 0,
    error_message TEXT,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE,
    FOREIGN KEY (parent_id) REFERENCES agent_executions(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_agent_executions_session ON agent_executions(session_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_agent_executions_parent ON agent_executions(parent_id);

-- Workflow runs tracking
CREATE TABLE IF NOT EXISTS workflow_runs (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    name TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('running', 'completed', 'error', 'cancelled')),
    current_phase TEXT,
    phase_count INTEGER DEFAULT 0,
    agent_count INTEGER DEFAULT 0,
    total_tokens INTEGER DEFAULT 0,
    started_at INTEGER NOT NULL,
    finished_at INTEGER,
    error_message TEXT,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_workflow_runs_session ON workflow_runs(session_id, started_at DESC);
