ALTER TABLE submit_outbox
  ADD COLUMN enqueue_seq INTEGER NOT NULL DEFAULT 0;

-- rowid reflects the actual INSERT order. created_at only has millisecond
-- precision, while a timeline item's updated_seq changes as its status moves
-- from pending to running/done, so neither can reconstruct enqueue order.
UPDATE submit_outbox SET enqueue_seq = rowid;

DROP INDEX idx_submit_outbox_connector_pending;
CREATE INDEX idx_submit_outbox_connector_pending
  ON submit_outbox(connector_id, state, session_id, enqueue_seq);
