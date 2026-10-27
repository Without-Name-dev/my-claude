-- Enable FTS5 for full-text search on timeline messages (simplified version)
-- This migration is intentionally minimal to avoid compatibility issues

-- Create FTS5 virtual table
CREATE VIRTUAL TABLE IF NOT EXISTS timeline_search_fts USING fts5(
  session_id UNINDEXED,
  item_id UNINDEXED,
  content
);
