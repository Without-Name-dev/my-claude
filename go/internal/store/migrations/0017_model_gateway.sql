-- Model gateway (see docs/api/model-gateway.md): user-owned third-party
-- providers, per-device defaults, per-session routes, switch operations, and
-- the credentials desktop Claude sessions use to reach the gateway.
--
-- Provider credentials are stored in the clear by explicit product decision:
-- they are never echoed back by any API, never logged, and never placed in
-- URLs. Only the server data plane reads credential_plaintext.

CREATE TABLE model_providers (
  id                   TEXT PRIMARY KEY,
  user_id              TEXT NOT NULL
                         REFERENCES users(id) ON DELETE CASCADE,
  name                 TEXT NOT NULL,
  protocol             TEXT NOT NULL
                         CHECK (protocol IN ('anthropic_messages', 'openai_chat_completions')),
  base_url             TEXT NOT NULL,
  auth_type            TEXT NOT NULL DEFAULT 'api_key'
                         CHECK (auth_type IN ('api_key', 'bearer')),
  credential_plaintext TEXT NOT NULL DEFAULT '',
  model_id             TEXT NOT NULL,
  model_display_name   TEXT NOT NULL DEFAULT '',
  capabilities_json    TEXT NOT NULL DEFAULT '{}',
  enabled              INTEGER NOT NULL DEFAULT 1
                         CHECK (enabled IN (0, 1)),
  revision             INTEGER NOT NULL DEFAULT 1,
  created_at           INTEGER NOT NULL,
  updated_at           INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_model_providers_user ON model_providers(user_id, updated_at DESC);

-- One default provider per connector per owning account.
CREATE TABLE connector_model_defaults (
  connector_id TEXT PRIMARY KEY
                 REFERENCES connectors(id) ON DELETE CASCADE,
  user_id      TEXT NOT NULL
                 REFERENCES users(id) ON DELETE CASCADE,
  provider_id  TEXT NOT NULL
                 REFERENCES model_providers(id) ON DELETE CASCADE,
  revision     INTEGER NOT NULL DEFAULT 1,
  updated_at   INTEGER NOT NULL
) STRICT;

-- One route per server session. mode 'inherit_device_default' follows the
-- connector default; 'explicit' pins this session to target_provider_id.
-- Deleting the provider nulls the target: the route then reports unavailable
-- and never silently falls back to another provider.
CREATE TABLE session_model_routes (
  session_id           TEXT PRIMARY KEY
                         REFERENCES sessions(id) ON DELETE CASCADE,
  user_id              TEXT NOT NULL
                         REFERENCES users(id) ON DELETE CASCADE,
  mode                 TEXT NOT NULL DEFAULT 'inherit_device_default'
                         CHECK (mode IN ('inherit_device_default', 'explicit')),
  target_provider_id   TEXT
                         REFERENCES model_providers(id) ON DELETE SET NULL,
  target_revision      INTEGER,
  applied_provider_id  TEXT,
  applied_revision     INTEGER,
  applied_model_id     TEXT,
  status               TEXT NOT NULL DEFAULT 'unknown'
                         CHECK (status IN ('not_connected', 'unknown',
                                           'waiting_for_turn_end', 'applying',
                                           'applied', 'failed')),
  status_detail        TEXT,
  updated_at           INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_session_model_routes_provider ON session_model_routes(target_provider_id);

-- One asynchronous route switch (per session, or per connector when applying
-- a device default across sessions). Status is honest: 'applied' means the
-- route is recorded and the next turn will use it, never that an upstream
-- model request succeeded.
CREATE TABLE route_switch_operations (
  id                  TEXT PRIMARY KEY,
  user_id             TEXT NOT NULL
                        REFERENCES users(id) ON DELETE CASCADE,
  scope               TEXT NOT NULL
                        CHECK (scope IN ('session', 'connector')),
  session_id          TEXT
                        REFERENCES sessions(id) ON DELETE CASCADE,
  connector_id        TEXT,
  target_provider_id  TEXT,
  target_revision     INTEGER,
  idempotency_key     TEXT,
  status              TEXT NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'waiting_for_turn_end',
                                          'applied', 'failed', 'cancelled')),
  failure             TEXT,
  created_at          INTEGER NOT NULL,
  updated_at          INTEGER NOT NULL,
  completed_at        INTEGER
) STRICT;

CREATE INDEX idx_route_switch_operations_user ON route_switch_operations(user_id, created_at DESC);

-- A retried switch with the same idempotency key must never create a second
-- operation. The key is scoped to one session: reusing it for a different
-- conversation is a different switch. NULL keys (server-initiated changes)
-- are unlimited.
CREATE UNIQUE INDEX uq_route_switch_operations_idem
  ON route_switch_operations(user_id, session_id, idempotency_key)
  WHERE idempotency_key IS NOT NULL;

-- Every served request updates the session's queued operations by status.
CREATE INDEX idx_route_switch_operations_session
  ON route_switch_operations(session_id, status);

-- Revocable credentials that identify a desktop Claude process to the
-- gateway. Only the hash is stored; the plaintext token is returned to the
-- connector exactly once at issuance.
CREATE TABLE gateway_credentials (
  id           TEXT PRIMARY KEY,
  connector_id TEXT NOT NULL
                 REFERENCES connectors(id) ON DELETE CASCADE,
  user_id      TEXT NOT NULL
                 REFERENCES users(id) ON DELETE CASCADE,
  token_hash   TEXT NOT NULL UNIQUE,
  token_prefix TEXT NOT NULL DEFAULT '',
  status       TEXT NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active', 'revoked')),
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER,
  revoked_at   INTEGER
) STRICT;

CREATE INDEX idx_gateway_credentials_connector ON gateway_credentials(connector_id);

-- Minimal per-request summary written once when a gateway request finishes;
-- streaming chunks never touch this table. No message bodies, no credentials.
CREATE TABLE gateway_request_log (
  id                  TEXT PRIMARY KEY,
  user_id             TEXT NOT NULL,
  connector_id        TEXT NOT NULL,
  session_id          TEXT,
  external_session_id TEXT,
  provider_id         TEXT,
  model_id            TEXT,
  provider_revision   INTEGER,
  started_at          INTEGER NOT NULL,
  finished_at         INTEGER,
  status              TEXT NOT NULL
                        CHECK (status IN ('streaming', 'completed', 'failed', 'cancelled')),
  upstream_status     INTEGER,
  bytes_in            INTEGER NOT NULL DEFAULT 0,
  bytes_out           INTEGER NOT NULL DEFAULT 0,
  upstream_request_id TEXT,
  error               TEXT
) STRICT;

CREATE INDEX idx_gateway_request_log_session ON gateway_request_log(session_id, started_at DESC);
CREATE INDEX idx_gateway_request_log_user ON gateway_request_log(user_id, started_at DESC);
