-- Re-key stored projects with the content-based workspace normalisation.
--
-- WorkspaceKey used to run filepath.Clean/ToSlash and lower-case only when the
-- process itself was on Windows. On the Linux server a backslash-separated
-- Windows path therefore kept its backslashes and its case, so the same
-- directory could be stored under several keys. The Go helper now normalises
-- the string regardless of host OS; existing rows are brought in line here.
--
-- Collapsing keys can make formerly distinct rows share a workspace. Sessions
-- are repointed at the oldest row of each group before the duplicates are
-- removed, and the surviving row is then re-keyed.

CREATE TEMP TABLE project_rekey AS
SELECT
  project_rowid,
  id,
  user_id,
  connector_id,
  orig,
  CASE WHEN was_windows THEN lower(cleaned) ELSE cleaned END AS new_key
FROM (
  SELECT
    rowid AS project_rowid,
    id,
    user_id,
    connector_id,
    workspace_key AS orig,
    rtrim(
      CASE
        WHEN instr(slashed, '//') > 0
          THEN replace(replace(slashed, '//', '/'), '//', '/')
        ELSE slashed
      END
    , '/') AS cleaned,
    (substr(slashed, 1, 2) = '//'
      OR (length(slashed) >= 3
          AND substr(slashed, 2, 1) = ':'
          AND substr(slashed, 3, 1) = '/'
          AND upper(substr(slashed, 1, 1)) BETWEEN 'A' AND 'Z')) AS was_windows
  FROM (
    SELECT
      rowid,
      id,
      user_id,
      connector_id,
      workspace_key,
      replace(workspace_key, '\', '/') AS slashed
    FROM projects
  )
);

-- rtrim turns the filesystem root into an empty string; path.Clean keeps "/".
UPDATE project_rekey SET new_key = '/' WHERE new_key = '' AND orig <> '';

CREATE TEMP TABLE project_keeper AS
SELECT user_id, connector_id, new_key, MIN(project_rowid) AS keeper_rowid
FROM project_rekey
GROUP BY user_id, connector_id, new_key;

UPDATE sessions
SET project_id = (
  SELECT keeper.id
  FROM project_rekey dup
  JOIN project_keeper k
    ON k.user_id = dup.user_id
   AND k.connector_id = dup.connector_id
   AND k.new_key = dup.new_key
  JOIN project_rekey keeper ON keeper.project_rowid = k.keeper_rowid
  WHERE dup.id = sessions.project_id
)
WHERE project_id IN (
  SELECT r.id
  FROM project_rekey r
  JOIN project_keeper k
    ON k.user_id = r.user_id
   AND k.connector_id = r.connector_id
   AND k.new_key = r.new_key
  WHERE r.project_rowid <> k.keeper_rowid
);

DELETE FROM projects
WHERE id IN (
  SELECT r.id
  FROM project_rekey r
  JOIN project_keeper k
    ON k.user_id = r.user_id
   AND k.connector_id = r.connector_id
   AND k.new_key = r.new_key
  WHERE r.project_rowid <> k.keeper_rowid
);

UPDATE projects
SET workspace_key = (
  SELECT r.new_key
  FROM project_rekey r
  JOIN project_keeper k ON k.keeper_rowid = r.project_rowid
  WHERE r.id = projects.id
)
WHERE id IN (
  SELECT r.id
  FROM project_rekey r
  JOIN project_keeper k ON k.keeper_rowid = r.project_rowid
);

DROP TABLE project_rekey;
DROP TABLE project_keeper;
