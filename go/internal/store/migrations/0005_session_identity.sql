-- Enforce one session row per runtime session.
--
-- UpsertDiscoveredSession used to read on the read pool and insert in a later
-- write transaction, with no constraint behind the identity. Two overlapping
-- inventory scans (or two connector sockets during a reconnect) could insert
-- the same Claude conversation twice. Fold any existing duplicates into the
-- most recently updated row, then let the unique index serialise new writes.

UPDATE sessions SET external_session_id = NULL WHERE external_session_id = '';

CREATE TEMP TABLE session_dupes AS
SELECT duplicate_id, keeper_id
FROM (
  SELECT
    id AS duplicate_id,
    first_value(id) OVER (
      PARTITION BY connector_id, runtime, external_session_id
      ORDER BY updated_seq DESC, updated_at DESC, id ASC
    ) AS keeper_id,
    row_number() OVER (
      PARTITION BY connector_id, runtime, external_session_id
      ORDER BY updated_seq DESC, updated_at DESC, id ASC
    ) AS position
  FROM sessions
  WHERE external_session_id IS NOT NULL
)
WHERE position > 1;

-- Keep the duplicate's timeline: an item whose id already exists on the keeper
-- row is discarded, the rest move across before the duplicate is deleted.
UPDATE OR IGNORE timeline_items
SET session_id = (
  SELECT d.keeper_id FROM session_dupes d WHERE d.duplicate_id = timeline_items.session_id
)
WHERE session_id IN (SELECT duplicate_id FROM session_dupes);

DELETE FROM timeline_items
WHERE session_id IN (SELECT duplicate_id FROM session_dupes);

UPDATE session_shares
SET session_id = (
  SELECT d.keeper_id FROM session_dupes d WHERE d.duplicate_id = session_shares.session_id
)
WHERE session_id IN (SELECT duplicate_id FROM session_dupes);

UPDATE OR IGNORE connector_terminal_roots
SET session_id = (
  SELECT d.keeper_id FROM session_dupes d WHERE d.duplicate_id = connector_terminal_roots.session_id
)
WHERE session_id IN (SELECT duplicate_id FROM session_dupes);

UPDATE OR IGNORE session_active_runs
SET session_id = (
  SELECT d.keeper_id FROM session_dupes d WHERE d.duplicate_id = session_active_runs.session_id
)
WHERE session_id IN (SELECT duplicate_id FROM session_dupes);

DELETE FROM sessions WHERE id IN (SELECT duplicate_id FROM session_dupes);

DROP TABLE session_dupes;

CREATE UNIQUE INDEX uq_sessions_connector_runtime_external
  ON sessions(connector_id, runtime, external_session_id)
  WHERE external_session_id IS NOT NULL;
