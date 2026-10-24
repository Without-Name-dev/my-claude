CREATE TABLE users (
  id                TEXT PRIMARY KEY,
  password_hash     TEXT NOT NULL,
  role              TEXT NOT NULL DEFAULT 'member'
                         CHECK (role IN ('admin','member')),
  disabled          INTEGER NOT NULL DEFAULT 0
                         CHECK (disabled IN (0,1)),
  email             TEXT,
  email_verified_at INTEGER,
  display_name      TEXT NOT NULL DEFAULT '',
  avatar            TEXT,
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL
) STRICT;

CREATE UNIQUE INDEX idx_users_email ON users(email);

CREATE TABLE email_verification_codes (
  scope            TEXT PRIMARY KEY,
  email            TEXT NOT NULL,
  purpose          TEXT NOT NULL
                        CHECK (purpose IN ('register','bind','reset')),
  user_id          TEXT NOT NULL,             -- register 时写入空串；故意无 FK
  code_hash        TEXT NOT NULL,
  nonce            TEXT NOT NULL,
  sent_at          INTEGER NOT NULL,          -- Unix ms
  expires_at       INTEGER NOT NULL,          -- Unix ms
  failed_attempts  INTEGER NOT NULL DEFAULT 0,
  attempt_window   INTEGER NOT NULL,          -- Unix ms
  consumed_at      INTEGER
) STRICT;

CREATE INDEX idx_email_codes_sent_at ON email_verification_codes(sent_at);

CREATE TABLE email_verification_limits (
  key           TEXT PRIMARY KEY,
  window_start  INTEGER NOT NULL,             -- Unix ms
  count         INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_email_limits_window ON email_verification_limits(window_start);

CREATE TABLE oauth_clients (
  id                 TEXT PRIMARY KEY,
  name               TEXT NOT NULL,
  redirect_uris_json TEXT NOT NULL,
  created_at         INTEGER NOT NULL,
  updated_at         INTEGER NOT NULL
) STRICT;

CREATE TABLE oauth_accounts (
  provider     TEXT NOT NULL,
  subject      TEXT NOT NULL,
  user_id      TEXT NOT NULL
                   REFERENCES users(id) ON DELETE CASCADE,
  email        TEXT,
  display_name TEXT,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL,
  PRIMARY KEY (provider, subject)
) STRICT;

CREATE INDEX idx_oauth_accounts_user_id ON oauth_accounts(user_id);

CREATE TABLE oauth_authorization_codes (
  code_hash             TEXT PRIMARY KEY,
  client_id             TEXT NOT NULL
                            REFERENCES oauth_clients(id) ON DELETE CASCADE, -- 增强
  user_id               TEXT NOT NULL
                            REFERENCES users(id) ON DELETE CASCADE,
  redirect_uri          TEXT NOT NULL,
  scope                 TEXT NOT NULL,
  code_challenge        TEXT NOT NULL,
  code_challenge_method TEXT NOT NULL,
  expires_at            INTEGER NOT NULL,
  consumed_at           INTEGER,
  created_at            INTEGER NOT NULL
) STRICT;

CREATE TABLE mobile_login_tokens (
  token_hash   TEXT PRIMARY KEY,
  user_id      TEXT NOT NULL
                   REFERENCES users(id) ON DELETE CASCADE,
  device_name  TEXT,
  expires_at   INTEGER NOT NULL,
  created_at   INTEGER NOT NULL,
  requested_at INTEGER,
  approved_at  INTEGER,
  rejected_at  INTEGER,
  consumed_at  INTEGER
) STRICT;

CREATE TABLE connectors (
  id                    TEXT PRIMARY KEY,
  user_id               TEXT NOT NULL
                            REFERENCES users(id) ON DELETE RESTRICT, -- 增强；源 schema 无 FK
  name                  TEXT NOT NULL,
  connector_kind        TEXT NOT NULL DEFAULT 'cli'
                            CHECK (connector_kind IN ('desktop','cli')),
  device_os             TEXT
                            CHECK (device_os IS NULL OR device_os IN ('macos','windows','linux')),
  status                TEXT NOT NULL
                            CHECK (status IN ('offline','online')),
  presence_instance_id  TEXT,
  presence_connection_id TEXT,
  last_seen_at          INTEGER,
  token_hash            TEXT NOT NULL,
  token_prefix          TEXT NOT NULL,
  revoked               INTEGER NOT NULL DEFAULT 0, -- 0=可用,1=已撤销,2=删除中
  created_at            INTEGER NOT NULL,
  updated_at            INTEGER NOT NULL,
  user_preferences      TEXT                        -- JSON
) STRICT;

CREATE INDEX idx_connectors_user ON connectors(user_id, revoked);

CREATE TABLE connector_runtime_types (
  connector_id         TEXT NOT NULL
                           REFERENCES connectors(id) ON DELETE CASCADE,
  runtime_type         TEXT NOT NULL,
  implementation_type  TEXT NOT NULL,
  display_name         TEXT NOT NULL,
  description          TEXT,
  present              INTEGER NOT NULL DEFAULT 1 CHECK (present IN (0,1)),
  available            INTEGER NOT NULL DEFAULT 1 CHECK (available IN (0,1)),
  reason               TEXT,
  recommended          INTEGER NOT NULL DEFAULT 0 CHECK (recommended IN (0,1)),
  recommendation_rank  INTEGER,
  discovery_json       TEXT NOT NULL,
  config_schema_json   TEXT,
  ui_schema_json       TEXT,
  defaults_json        TEXT NOT NULL,
  capabilities_json    TEXT NOT NULL,
  metadata_json        TEXT NOT NULL,
  instance_policy      TEXT NOT NULL,
  max_instances        INTEGER,
  last_discovered_at   INTEGER NOT NULL,
  created_at           INTEGER NOT NULL,
  updated_at           INTEGER NOT NULL,
  PRIMARY KEY (connector_id, runtime_type)
) STRICT;

CREATE TABLE device_runtimes (
  connector_id  TEXT NOT NULL,
  runtime_id    TEXT NOT NULL,
  runtime_type  TEXT NOT NULL,
  name          TEXT NOT NULL,
  name_key      TEXT NOT NULL,
  config_json   TEXT,       -- NULL=未配置；'{}'=使用 provider 默认
  active        INTEGER NOT NULL DEFAULT 0 CHECK (active IN (0,1)),
  status        TEXT NOT NULL DEFAULT 'stopped'
                    CHECK (status IN ('stopped','starting','running','error')),
  error_json    TEXT,
  phase_json    TEXT,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  PRIMARY KEY (connector_id, runtime_id),
  UNIQUE (connector_id, name_key),
  FOREIGN KEY (connector_id) REFERENCES connectors(id) ON DELETE CASCADE,
  FOREIGN KEY (connector_id, runtime_type)
    REFERENCES connector_runtime_types(connector_id, runtime_type)
    ON DELETE CASCADE
) STRICT;

CREATE TABLE retired_device_runtimes (
  connector_id TEXT NOT NULL
                   REFERENCES connectors(id) ON DELETE CASCADE,
  runtime_id   TEXT NOT NULL,
  retired_at   INTEGER NOT NULL,
  PRIMARY KEY (connector_id, runtime_id)
) STRICT;

CREATE TABLE connector_runtime_catalogs (
  connector_id  TEXT NOT NULL
                    REFERENCES connectors(id) ON DELETE CASCADE,
  runtime       TEXT NOT NULL,
  runtime_id    TEXT NOT NULL,
  catalog_type  TEXT NOT NULL,
  revision      INTEGER NOT NULL,
  catalog_json  TEXT NOT NULL,
  updated_at    INTEGER NOT NULL,
  PRIMARY KEY (connector_id, runtime_id, catalog_type)
) STRICT;

CREATE TABLE connector_protocol_capabilities (
  connector_id      TEXT PRIMARY KEY
                        REFERENCES connectors(id) ON DELETE CASCADE,
  revision          INTEGER NOT NULL,
  capabilities_json TEXT NOT NULL,
  updated_at        INTEGER NOT NULL
) STRICT;

CREATE TABLE fs_preview_tokens (
  token_hash   TEXT PRIMARY KEY,
  user_id      TEXT NOT NULL
                   REFERENCES users(id) ON DELETE CASCADE,
  connector_id TEXT NOT NULL
                   REFERENCES connectors(id) ON DELETE CASCADE,
  root         TEXT NOT NULL,
  path         TEXT NOT NULL,
  expires_at   INTEGER NOT NULL,
  created_at   INTEGER NOT NULL,
  consumed_at  INTEGER
) STRICT;

CREATE TABLE pairing_codes (
  id              TEXT PRIMARY KEY,
  code            TEXT NOT NULL UNIQUE,
  status          TEXT NOT NULL
                      CHECK (status IN ('pending','claimed','consumed','expired')),
  server_url      TEXT,
  connector_id    TEXT
                      REFERENCES connectors(id) ON DELETE CASCADE, -- 增强
  connector_token TEXT,       -- 明文，poll 时返回给 connector
  expires_at      INTEGER NOT NULL,
  created_at      INTEGER NOT NULL,
  claimed_at      INTEGER,
  consumed_at     INTEGER
) STRICT;

CREATE INDEX idx_pairing_expiry ON pairing_codes(status, expires_at);

CREATE TABLE projects (
  id                TEXT PRIMARY KEY,
  user_id           TEXT NOT NULL
                        REFERENCES users(id) ON DELETE CASCADE,
  connector_id      TEXT NOT NULL
                        REFERENCES connectors(id) ON DELETE CASCADE,
  name              TEXT NOT NULL,
  workspace_path    TEXT NOT NULL,
  workspace_key     TEXT NOT NULL,
  manually_created  INTEGER NOT NULL DEFAULT 0
                        CHECK (manually_created IN (0,1)),
  pinned            INTEGER NOT NULL DEFAULT 0 CHECK (pinned IN (0,1)),
  pinned_at         INTEGER,
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL,
  CONSTRAINT uq_projects_user_connector_workspace
    UNIQUE (user_id, connector_id, workspace_key),
  CONSTRAINT uq_projects_user_name
    UNIQUE (user_id, name)
) STRICT;

CREATE INDEX idx_projects_user_pinned_updated
  ON projects(user_id, pinned, pinned_at, updated_at);
CREATE INDEX idx_projects_connector_workspace
  ON projects(connector_id, workspace_key);

CREATE TABLE sessions (
  id                         TEXT PRIMARY KEY,
  connector_id               TEXT NOT NULL
                                 REFERENCES connectors(id), -- 源语义：NO ACTION；删除 connector 时先删 sessions 再删 projects
  project_id                 TEXT
                                 REFERENCES projects(id) ON DELETE RESTRICT,
  runtime                    TEXT NOT NULL,
  runtime_id                 TEXT NOT NULL,
  origin                     TEXT NOT NULL DEFAULT 'connector_import'
                                 CHECK (origin IN ('connector_import','platform')),
  model_selection_id         TEXT,
  permission_selection_id    TEXT,
  external_session_id        TEXT,
  title                      TEXT,
  title_source               TEXT
                                 CHECK (title_source IS NULL OR title_source IN ('user','connector')),
  cwd                        TEXT,
  status                     TEXT NOT NULL
                                 CHECK (status IN ('idle','waiting','pending','running',
                                                   'stopping','waiting_approval','error','blocked')),
  takeover                   INTEGER NOT NULL DEFAULT 0 CHECK (takeover IN (0,1)),
  pinned                     INTEGER NOT NULL DEFAULT 0 CHECK (pinned IN (0,1)),
  pinned_at                  INTEGER,
  archived                   INTEGER NOT NULL DEFAULT 0 CHECK (archived IN (0,1)),
  archived_at                INTEGER,
  dsh_archive_legacy         INTEGER NOT NULL DEFAULT 0
                                 CHECK (dsh_archive_legacy IN (0,1)),
  source_state               TEXT NOT NULL DEFAULT 'visible'
                                 CHECK (source_state IN ('available','archived','unavailable',
                                                         'deleted','missing','unknown','visible','hidden')),
  source_state_at            INTEGER,
  source_state_reason        TEXT,
  source_observation_origin  TEXT
                                 CHECK (source_observation_origin IS NULL
                                        OR source_observation_origin IN ('event','inventory','operation')),
  source_scan_token          TEXT,
  last_read_seq              INTEGER NOT NULL DEFAULT 0,
  latest_turn_end_seq        INTEGER NOT NULL DEFAULT 0,
  timeline_reset_seq         INTEGER NOT NULL DEFAULT 0,
  last_synced_at             INTEGER,
  source_observed_at         INTEGER,
  last_activity_at           INTEGER,
  sort_at                    INTEGER,
  seq                        INTEGER NOT NULL DEFAULT 0,
  seq_allocated_high         INTEGER NOT NULL DEFAULT 0, -- 可选：保留 Redis 租约时才需要
  updated_seq                INTEGER NOT NULL DEFAULT 0,
  created_at                 INTEGER NOT NULL,
  updated_at                 INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_sessions_connector_runtime_source_state
  ON sessions(connector_id, runtime, source_state);
CREATE INDEX idx_sessions_project_archived_sort
  ON sessions(project_id, archived, pinned, sort_at);
CREATE INDEX idx_sessions_connector_runtime_external
  ON sessions(connector_id, runtime_id, external_session_id)
  WHERE external_session_id IS NOT NULL;


CREATE TABLE session_active_runs (
  session_id          TEXT PRIMARY KEY
                          REFERENCES sessions(id) ON DELETE CASCADE,
  runtime             TEXT NOT NULL,
  runtime_id          TEXT NOT NULL,
  external_session_id TEXT,
  status              TEXT NOT NULL,
  params_json         TEXT,
  started_at          INTEGER NOT NULL,
  updated_at          INTEGER NOT NULL
) STRICT;

CREATE TABLE timeline_items (
  session_id   TEXT NOT NULL
                   REFERENCES sessions(id) ON DELETE CASCADE,
  id           TEXT NOT NULL,
  type         TEXT NOT NULL
                   CHECK (type IN ('message','tool','artifact','marker','system')),
  status       TEXT NOT NULL
                   CHECK (status IN ('pending','running','waiting_approval',
                                     'done','failed','cancelled','interrupted')),
  role         TEXT
                   CHECK (role IS NULL OR role IN ('user','assistant','system','tool')),
  order_seq    INTEGER NOT NULL,
  updated_seq  INTEGER NOT NULL,
  item_time    INTEGER,
  payload_json TEXT NOT NULL,
  PRIMARY KEY (session_id, id)
) STRICT;

CREATE INDEX idx_timeline_items_session_updated_seq
  ON timeline_items(session_id, updated_seq);
CREATE INDEX idx_timeline_items_session_order_seq
  ON timeline_items(session_id, order_seq);
CREATE INDEX idx_timeline_items_session_latest
  ON timeline_items(session_id,
                    coalesce(item_time, 0) DESC,
                    order_seq DESC,
                    updated_seq DESC);
CREATE INDEX idx_timeline_items_item_time_type_role
  ON timeline_items(item_time, type, role);


CREATE TABLE session_shares (
  id                     TEXT PRIMARY KEY,
  user_id                TEXT NOT NULL
                             REFERENCES users(id) ON DELETE CASCADE,
  session_id             TEXT NOT NULL
                             REFERENCES sessions(id) ON DELETE CASCADE,
  scope                  TEXT NOT NULL CHECK (scope IN ('message','session')),
  snapshot_json          TEXT NOT NULL,
  allowed_file_ids_json  TEXT NOT NULL DEFAULT '[]',
  created_at             INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_session_shares_user_created
  ON session_shares(user_id, created_at);
CREATE INDEX idx_session_shares_session_created
  ON session_shares(session_id, created_at);

CREATE TABLE connector_terminal_roots (
  connector_id TEXT NOT NULL
                   REFERENCES connectors(id) ON DELETE CASCADE,
  terminal_id  TEXT NOT NULL,
  session_id   TEXT NOT NULL
                   REFERENCES sessions(id) ON DELETE CASCADE, -- 增强；源无 FK
  root         TEXT NOT NULL,
  cwd          TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL,
  PRIMARY KEY (connector_id, terminal_id)
) STRICT;

CREATE INDEX idx_connector_terminal_roots_session
  ON connector_terminal_roots(connector_id, session_id);

CREATE TABLE usage_events (
  id           TEXT PRIMARY KEY,
  occurred_at  INTEGER NOT NULL,       -- Unix ms
  hour_bucket  TEXT NOT NULL,          -- YYYY-MM-DDTHH (UTC)
  event_type   TEXT NOT NULL,
  user_id      TEXT REFERENCES users(id) ON DELETE SET NULL,
  connector_id TEXT,                   -- 故意无 FK
  session_id   TEXT,                   -- 故意无 FK
  runtime_type TEXT,
  runtime_id   TEXT,
  device_os    TEXT,
  outcome      TEXT,
  duration_ms  INTEGER,
  dedupe_key   TEXT UNIQUE
) STRICT;

CREATE INDEX idx_usage_events_hour ON usage_events(hour_bucket);
CREATE INDEX idx_usage_events_type_hour ON usage_events(event_type, hour_bucket);
CREATE INDEX idx_usage_events_session_time ON usage_events(session_id, occurred_at);
CREATE INDEX idx_usage_events_user_hour ON usage_events(user_id, hour_bucket);

CREATE TABLE usage_inventory_snapshots (
  snapshot_date TEXT NOT NULL,   -- YYYY-MM-DD
  metric_key    TEXT NOT NULL,
  dimension     TEXT NOT NULL DEFAULT '',
  value         REAL NOT NULL,
  computed_at   INTEGER NOT NULL,
  PRIMARY KEY (snapshot_date, metric_key, dimension)
) STRICT;

CREATE TABLE instance_settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE legacy_import_archive (
  source_table TEXT NOT NULL,
  row_key      TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  archived_at  INTEGER NOT NULL,
  PRIMARY KEY (source_table, row_key)
) STRICT;
