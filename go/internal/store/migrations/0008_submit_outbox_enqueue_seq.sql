ALTER TABLE submit_outbox
  ADD COLUMN enqueue_seq INTEGER NOT NULL DEFAULT 0;

UPDATE submit_outbox
SET enqueue_seq = coalesce((
  SELECT timeline_items.updated_seq
  FROM timeline_items
  WHERE timeline_items.session_id = submit_outbox.session_id
    AND timeline_items.id = 'msg_' || submit_outbox.request_id || '_user'
), 0);

DROP INDEX idx_submit_outbox_connector_pending;
CREATE INDEX idx_submit_outbox_connector_pending
  ON submit_outbox(connector_id, state, session_id, enqueue_seq);
