-- Timeline order allocation reads the newest order_seq and item_time for a
-- session on every insert inside the single write transaction. The order_seq
-- half already has an index; without this one the item_time half scanned the
-- whole session, growing with every message.
CREATE INDEX IF NOT EXISTS idx_timeline_items_session_item_time
  ON timeline_items(session_id, item_time);

-- The full-text table was created but never populated, so global search always
-- returned nothing. Backfill the messages that already exist; new writes are
-- indexed from UpsertTimelineItem.
INSERT INTO timeline_search_fts(rowid, session_id, item_id, content)
SELECT rowid, session_id, id, json_extract(payload_json, '$.text')
FROM timeline_items
WHERE json_extract(payload_json, '$.text') IS NOT NULL
  AND json_extract(payload_json, '$.text') <> '';

-- The connector resolves an event that carries only a runtime id through
-- sessions(connector_id, external_session_id); without this index it scanned
-- every session the connector owns, which grows with the account.
CREATE INDEX IF NOT EXISTS idx_sessions_connector_external
  ON sessions(connector_id, external_session_id);
