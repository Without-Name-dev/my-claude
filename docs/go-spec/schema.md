# Go 版 SQLite Schema 设计说明

**覆盖表数量：27 个现存表** = 25 个 `schema.py` 业务/运行时表 + `_deprecated_app_releases` + `alembic_version`。
Go 空库实际创建 **26 个表**（25 个业务表 + `schema_migrations`），不创建 `_deprecated_app_releases` 和 `alembic_version`。
另有 11 个已删除或已改名的 v2 历史表，以及 5 个 v1 旧表，列在附录 A，不进入 Go 重建。

- 源 schema revision：`v2_44`（schema version `2.44`）
- 主要来源：
  - `server/agent_server/infra/db/schema.py`（当前 SQLAlchemy 元数据）
  - `server/migrations/schema_v2_0.py`（v2.0 基线快照）
  - `server/migrations/versions/v2_0.py` … `v2_44.py`（增量 DDL）
  - `server/agent_server/core/models.py`（枚举/JSON 形状）
- 运行时并发实现来源：
  - `server/agent_server/infra/repositories/timeline.py`
  - `server/agent_server/infra/timeline_store.py`
  - `server/agent_server/services/session_revision_allocator.py`
  - `server/agent_server/infra/repositories/email_accounts.py`
  - `server/agent_server/infra/repositories/store_support.py`

## 0. 先看结论

现有 PostgreSQL schema 实际上已经是一个“PostgreSQL 只负责事务、锁和 SQL，值全部用 TEXT 承载”的 schema：

- **没有一列使用 `JSONB`**。所有 JSON 都是 `TEXT`，写入时用 `json.dumps(..., sort_keys=True, separators=(",", ":"))` 规范化。
- **没有一列使用 `UUID`**。所有主键都是应用生成的前缀字符串（`usr_`、`conn_`、`sess_`、`proj_`、`shr_`、`pair_`、`rti_`、`ue_` 等）。`core/models.py` 里的 `UUID` 只用于 API 入参 `installationId`，不落库。
- **没有一列使用 `TIMESTAMPTZ`**。点时间全部是 ISO-8601 UTC 字符串（`utc_now()` 生成，`+00:00` 替换为 `Z`）；邮件验证码表是例外，用 `BIGINT` Unix 秒。
- **没有 `ARRAY`、`ENUM`、`SERIAL/IDENTITY`、`CITEXT`**。列表用 JSON 数组 TEXT，枚举由 Pydantic `Literal` 在应用层校验，主键没有自增。
- 真正与 PostgreSQL 绑定的只有：
  1. `pg_advisory_lock` / `pg_advisory_xact_lock` / `pg_try_advisory_lock`
  2. `SELECT ... FOR UPDATE`、`FOR NO KEY UPDATE`、`FOR SHARE OF`
  3. `INSERT ... ON CONFLICT DO UPDATE ... WHERE ...` / `DO NOTHING`
  4. `UPDATE ... RETURNING`（SQLite 3.35+ 也支持）
  5. 一个 `coalesce(...) DESC` 表达式索引
  6. 迁移中的 `pg_indexes`、`GREATEST()`、`pg_try_advisory_lock`

因此 Go + SQLite 的移植重点不是类型转换，而是**把“多写者 + 行锁 + advisory lock + Redis 区间租约”收敛成“WAL + 单写协程 + `BEGIN IMMEDIATE` + 每会话内存互斥”**。

---

## 1. 全量表 / 列清册

以下类型是**源 PostgreSQL 中的实际类型**。SQLAlchemy 的 `Text` 在 PG 上是 `TEXT`，`BigInteger` 是 `BIGINT`，`Integer` 是 `INTEGER`，`Float` 是 `DOUBLE PRECISION`，`Boolean` 是 `BOOLEAN`。
“默认”列里的 `—` 表示没有数据库默认值，由应用写入。

### 1.1 users / auth

#### `users`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `id` | TEXT | 否 | — | PK。应用生成 `usr_` + 12 字节 hex（24 字符） |
| `password_hash` | TEXT | 否 | — | `pbkdf2_sha256$<salt>$<b64digest>`，salt 内嵌 |
| `role` | TEXT | 否 | `'member'` | `admin` / `member` |
| `disabled` | INTEGER | 否 | `0` | 0/1 |
| `email` | TEXT | 是 | — | 应用层 `strip().lower()` 后写入 |
| `email_verified_at` | TEXT | 是 | — | ISO-8601 UTC |
| `display_name` | TEXT | 否 | `''` | 1–64 字符（应用校验） |
| `avatar` | TEXT | 是 | — | data URL（`image/png;base64,...`），上传端限制约 256 KB |
| `created_at` | TEXT | 否 | — | ISO-8601 UTC |
| `updated_at` | TEXT | 否 | — | ISO-8601 UTC |

约束/索引：
- PK：`id`
- UNIQUE INDEX `idx_users_email (email)`。PG 与 SQLite 都允许任意多个 `NULL`。

#### `email_verification_codes`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `scope` | TEXT | 否 | — | PK。`sha256(email + "\0" + purpose + "\0" + user_id)` |
| `email` | TEXT | 否 | — | 已规范化的小写邮箱 |
| `purpose` | TEXT | 否 | — | `register` / `bind` / `reset` |
| `user_id` | TEXT | 否 | — | register 时为空串 `''`，因此**故意没有 FK** |
| `code_hash` | TEXT | 否 | — | `HMAC-SHA256(server_secret, "email-code:" + nonce + ":" + code)` 的 hex |
| `nonce` | TEXT | 否 | — | `token_urlsafe(24)` |
| `sent_at` | BIGINT | 否 | — | **Unix 秒**，不是 ISO 字符串 |
| `expires_at` | BIGINT | 否 | — | Unix 秒 |
| `failed_attempts` | INTEGER | 否 | `0` | 失败次数 |
| `attempt_window` | BIGINT | 否 | — | Unix 秒，失败计数窗口起点 |
| `consumed_at` | BIGINT | 是 | — | Unix 秒，一次性消费标记 |

约束/索引：
- PK：`scope`
- INDEX `idx_email_codes_sent_at (sent_at)`

#### `email_verification_limits`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `key` | TEXT | 否 | — | PK。`email:` + `sha256(email)` |
| `window_start` | BIGINT | 否 | — | Unix 秒 |
| `count` | INTEGER | 否 | — | 窗口内发信次数 |

约束/索引：
- PK：`key`
- INDEX `idx_email_limits_window (window_start)`

#### `oauth_accounts`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `provider` | TEXT | 否 | — | 复合 PK 第一部分 |
| `subject` | TEXT | 否 | — | 复合 PK 第二部分（OIDC `sub`） |
| `user_id` | TEXT | 否 | — | FK → `users.id` ON DELETE CASCADE |
| `email` | TEXT | 是 | — | IdP 返回的邮箱 |
| `display_name` | TEXT | 是 | — | IdP 返回的显示名 |
| `created_at` | TEXT | 否 | — | ISO-8601 UTC |
| `updated_at` | TEXT | 否 | — | ISO-8601 UTC |

约束/索引：
- PK：`(provider, subject)`
- FK：`user_id → users(id) ON DELETE CASCADE`
- INDEX `idx_oauth_accounts_user_id (user_id)`

#### `oauth_clients`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `id` | TEXT | 否 | — | PK，OAuth client_id |
| `name` | TEXT | 否 | — | 显示名 |
| `redirect_uris_json` | TEXT | 否 | — | JSON 字符串数组 |
| `created_at` | TEXT | 否 | — | ISO-8601 UTC |
| `updated_at` | TEXT | 否 | — | ISO-8601 UTC |

约束/索引：
- PK：`id`

#### `oauth_authorization_codes`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `code_hash` | TEXT | 否 | — | PK，授权码的 SHA-256 |
| `client_id` | TEXT | 否 | — | 源 schema 没有 FK；Go 重建建议加 FK → `oauth_clients(id)` |
| `user_id` | TEXT | 否 | — | FK → `users.id` ON DELETE CASCADE |
| `redirect_uri` | TEXT | 否 | — | 回调地址 |
| `scope` | TEXT | 否 | — | 授权范围 |
| `code_challenge` | TEXT | 否 | — | PKCE challenge |
| `code_challenge_method` | TEXT | 否 | — | 通常 `S256` |
| `expires_at` | TEXT | 否 | — | ISO-8601 UTC |
| `consumed_at` | TEXT | 是 | — | ISO-8601 UTC，一次性消费标记 |
| `created_at` | TEXT | 否 | — | ISO-8601 UTC |

约束/索引：
- PK：`code_hash`
- FK：`user_id → users(id) ON DELETE CASCADE`

#### `mobile_login_tokens`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `token_hash` | TEXT | 否 | — | PK，登录令牌 SHA-256 |
| `user_id` | TEXT | 否 | — | FK → `users.id` ON DELETE CASCADE |
| `device_name` | TEXT | 是 | — | 客户端设备名 |
| `expires_at` | TEXT | 否 | — | ISO-8601 UTC |
| `created_at` | TEXT | 否 | — | ISO-8601 UTC |
| `requested_at` | TEXT | 是 | — | 扫码请求时间；状态由这些时间戳推导，没有 status 列 |
| `approved_at` | TEXT | 是 | — | Web 端确认时间 |
| `rejected_at` | TEXT | 是 | — | Web 端拒绝时间 |
| `consumed_at` | TEXT | 是 | — | 换取 token 的时间 |

约束/索引：
- PK：`token_hash`
- FK：`user_id → users(id) ON DELETE CASCADE`

#### `instance_settings`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `key` | TEXT | 否 | — | PK |
| `value` | TEXT | 否 | — | 标量字符串或 JSON 字符串 |
| `updated_at` | TEXT | 否 | — | ISO-8601 UTC |

已知 key（来自代码）：
- `registration_open`（`"true"`/`"false"`）
- `oauth_registration_open`（`"true"`/`"false"`）
- `password_reset_enabled`（`"true"`/`"false"`）
- `oauth_provider`（JSON）
- `email_verification`（JSON）
- `public_announcement`（JSON：`{enabled, markdown, publishedAt}`）

约束/索引：
- PK：`key`

> 认证模型：用户 access token 是无状态 JWT（`HMAC-SHA256`，payload 只有 `sub`/`exp`），**没有服务端 token 表**；`mobile_login_tokens` 只用于扫码登录的短期一次性令牌。Go 版不需要额外的 sessions/token 表。

### 1.2 connectors / runtime

#### `connectors`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `id` | TEXT | 否 | — | PK。`conn_` + `token_urlsafe(10)`，或 `conn_` + `sha256(user_id:installation_id)[:24]` |
| `user_id` | TEXT | 否 | — | 源 schema **没有 FK**；Go 重建建议 FK → `users(id)` |
| `name` | TEXT | 否 | — | 设备显示名 |
| `connector_kind` | TEXT | 否 | `'cli'` | `desktop` / `cli` |
| `device_os` | TEXT | 是 | — | `macos` / `windows` / `linux` |
| `status` | TEXT | 否 | — | `offline` / `online` |
| `presence_instance_id` | TEXT | 是 | — | 当前持有 presence 的服务实例 ID |
| `presence_connection_id` | TEXT | 是 | — | 当前 WebSocket 连接 ID |
| `last_seen_at` | TEXT | 是 | — | ISO-8601 UTC |
| `token_hash` | TEXT | 否 | — | connector token 的 SHA-256 |
| `token_prefix` | TEXT | 否 | — | token 前 12 字符，用于展示 |
| `revoked` | INTEGER | 否 | `0` | 0=可用；1=已撤销；2=删除中（等待外部清理） |
| `created_at` | TEXT | 否 | — | ISO-8601 UTC |
| `updated_at` | TEXT | 否 | — | ISO-8601 UTC |
| `user_preferences` | TEXT | 是 | — | JSON，daemon 写入的本地 agent 偏好，后端只读 |

约束/索引：
- PK：`id`
- 源 schema 无索引。Go 重建建议 `idx_connectors_user (user_id, revoked)`。

#### `connector_runtime_types`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `connector_id` | TEXT | 否 | — | 复合 PK；FK → `connectors.id` CASCADE |
| `runtime_type` | TEXT | 否 | — | 复合 PK；`claude` / `codex` / `opencode` / `acp` / `dsh` 或新 provider |
| `implementation_type` | TEXT | 否 | — | 实现类型 |
| `display_name` | TEXT | 否 | — | 展示名 |
| `description` | TEXT | 是 | — | 描述 |
| `present` | INTEGER | 否 | `1` | 本机是否发现该 runtime |
| `available` | INTEGER | 否 | `1` | 是否可用 |
| `reason` | TEXT | 是 | — | 不可用原因 |
| `recommended` | INTEGER | 否 | `0` | 是否推荐 |
| `recommendation_rank` | INTEGER | 是 | — | 推荐排序 |
| `discovery_json` | TEXT | 否 | — | JSON |
| `config_schema_json` | TEXT | 是 | — | JSON Schema |
| `ui_schema_json` | TEXT | 是 | — | JSON UI Schema |
| `defaults_json` | TEXT | 否 | — | JSON |
| `capabilities_json` | TEXT | 否 | — | JSON |
| `metadata_json` | TEXT | 否 | — | JSON |
| `instance_policy` | TEXT | 否 | — | 实例策略（如 single/multiple） |
| `max_instances` | BIGINT | 是 | — | 最大实例数 |
| `last_discovered_at` | TEXT | 否 | — | ISO-8601 UTC |
| `created_at` | TEXT | 否 | — | ISO-8601 UTC |
| `updated_at` | TEXT | 否 | — | ISO-8601 UTC |

约束/索引：
- PK：`(connector_id, runtime_type)`
- FK：`connector_id → connectors(id) ON DELETE CASCADE`

#### `device_runtimes`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `connector_id` | TEXT | 否 | — | 复合 PK；FK → `connectors.id` CASCADE |
| `runtime_id` | TEXT | 否 | — | 复合 PK；`rti_` + `token_urlsafe(12)`，或 legacy 时等于 `runtime_type` |
| `runtime_type` | TEXT | 否 | — | 指向 `connector_runtime_types` |
| `name` | TEXT | 否 | — | 用户可见实例名 |
| `name_key` | TEXT | 否 | — | 规范化后的名称，用于唯一性 |
| `config_json` | TEXT | 是 | — | JSON；`NULL` 表示未配置，`{}` 表示全部用 provider 默认 |
| `active` | INTEGER | 否 | `0` | 0/1 |
| `status` | TEXT | 否 | `'stopped'` | `stopped` / `starting` / `running` / `error` |
| `error_json` | TEXT | 是 | — | JSON 错误 |
| `phase_json` | TEXT | 是 | — | JSON，非错误状态中的阶段（如 starting 时 syncing） |
| `created_at` | TEXT | 否 | — | ISO-8601 UTC |
| `updated_at` | TEXT | 否 | — | ISO-8601 UTC |

约束/索引：
- PK：`(connector_id, runtime_id)`
- UNIQUE：`(connector_id, name_key)`
- FK：`connector_id → connectors(id) ON DELETE CASCADE`
- FK：`(connector_id, runtime_type) → connector_runtime_types(connector_id, runtime_type) ON DELETE CASCADE`

> 注意 `device_runtimes` 同时有两个 FK 覆盖 `connector_id`，这是源 schema 的真实形态。

#### `retired_device_runtimes`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `connector_id` | TEXT | 否 | — | 复合 PK；FK → `connectors.id` CASCADE |
| `runtime_id` | TEXT | 否 | — | 复合 PK；已手动删除的实例 ID |
| `retired_at` | TEXT | 否 | — | ISO-8601 UTC |

约束/索引：
- PK：`(connector_id, runtime_id)`
- FK：`connector_id → connectors(id) ON DELETE CASCADE`

#### `connector_runtime_catalogs`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `connector_id` | TEXT | 否 | — | 复合 PK；FK → `connectors.id` CASCADE |
| `runtime` | TEXT | 否 | — | legacy 冗余字段，保留给旧客户端 |
| `runtime_id` | TEXT | 否 | — | 复合 PK；应用侧默认取 `runtime` |
| `catalog_type` | TEXT | 否 | — | 复合 PK，如 models/permissions |
| `revision` | BIGINT | 否 | — | catalog 修订号 |
| `catalog_json` | TEXT | 否 | — | JSON |
| `updated_at` | TEXT | 否 | — | ISO-8601 UTC |

约束/索引：
- PK：`(connector_id, runtime_id, catalog_type)`
- FK：`connector_id → connectors(id) ON DELETE CASCADE`

#### `connector_protocol_capabilities`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `connector_id` | TEXT | 否 | — | PK；FK → `connectors.id` CASCADE |
| `revision` | BIGINT | 否 | — | 能力修订号 |
| `capabilities_json` | TEXT | 否 | — | JSON |
| `updated_at` | TEXT | 否 | — | ISO-8601 UTC |

约束/索引：
- PK：`connector_id`
- FK：`connector_id → connectors(id) ON DELETE CASCADE`

#### `pairing_codes`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `id` | TEXT | 否 | — | PK，`pair_` + `token_urlsafe(10)` |
| `code` | TEXT | 否 | — | UNIQUE，6 位数字字符串 |
| `status` | TEXT | 否 | — | `pending` / `claimed` / `consumed` / `expired` |
| `server_url` | TEXT | 是 | — | connector 应连接的 URL |
| `connector_id` | TEXT | 是 | — | 认领后写入；源 schema 无 FK |
| `connector_token` | TEXT | 是 | — | **明文** connector token，poll 时返回给 connector |
| `expires_at` | TEXT | 否 | — | ISO-8601 UTC |
| `created_at` | TEXT | 否 | — | ISO-8601 UTC |
| `claimed_at` | TEXT | 是 | — | ISO-8601 UTC |
| `consumed_at` | TEXT | 是 | — | ISO-8601 UTC |

约束/索引：
- PK：`id`
- UNIQUE：`code`
- 源 schema 无其他索引。Go 重建建议 `idx_pairing_expiry (status, expires_at)` 便于清理。

#### `fs_preview_tokens`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `token_hash` | TEXT | 否 | — | PK，预览 token 的 SHA-256 |
| `user_id` | TEXT | 否 | — | FK → `users.id` CASCADE |
| `connector_id` | TEXT | 否 | — | FK → `connectors.id` CASCADE |
| `root` | TEXT | 否 | — | 允许浏览的根目录 |
| `path` | TEXT | 否 | — | 初始路径 |
| `expires_at` | TEXT | 否 | — | ISO-8601 UTC |
| `created_at` | TEXT | 否 | — | ISO-8601 UTC |
| `consumed_at` | TEXT | 是 | — | ISO-8601 UTC |

约束/索引：
- PK：`token_hash`
- FK：`user_id → users(id) ON DELETE CASCADE`
- FK：`connector_id → connectors(id) ON DELETE CASCADE`

### 1.3 sessions / projects

#### `projects`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `id` | TEXT | 否 | — | PK，`proj_` + `token_urlsafe(10)` |
| `user_id` | TEXT | 否 | — | FK → `users.id` CASCADE |
| `connector_id` | TEXT | 否 | — | FK → `connectors.id` CASCADE |
| `name` | TEXT | 否 | — | 项目名 |
| `workspace_path` | TEXT | 否 | — | 规范化后的绝对/设备路径 |
| `workspace_key` | TEXT | 否 | — | 路径 key（大小写/分隔符规范化后） |
| `manually_created` | BOOLEAN | 否 | `false` | 用户手动创建 vs 自动发现 |
| `pinned` | INTEGER | 否 | `0` | 0/1 |
| `pinned_at` | TEXT | 是 | — | ISO-8601 UTC |
| `created_at` | TEXT | 否 | — | ISO-8601 UTC |
| `updated_at` | TEXT | 否 | — | ISO-8601 UTC |

约束/索引：
- PK：`id`
- UNIQUE `uq_projects_user_connector_workspace (user_id, connector_id, workspace_key)`
- UNIQUE `uq_projects_user_name (user_id, name)`
- FK：`user_id → users(id) ON DELETE CASCADE`
- FK：`connector_id → connectors(id) ON DELETE CASCADE`
- INDEX `idx_projects_user_pinned_updated (user_id, pinned, pinned_at, updated_at)`
- INDEX `idx_projects_connector_workspace (connector_id, workspace_key)`

#### `sessions`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `id` | TEXT | 否 | — | PK，`sess_` + `token_urlsafe(10)` |
| `connector_id` | TEXT | 否 | — | FK → `connectors.id`；源 schema **无 ON DELETE**（NO ACTION） |
| `project_id` | TEXT | 是 | — | FK → `projects.id` ON DELETE RESTRICT |
| `runtime` | TEXT | 否 | — | runtime type |
| `runtime_id` | TEXT | 否 | — | runtime instance ID；应用侧缺省时等于 `runtime` |
| `origin` | TEXT | 否 | `'connector_import'` | `connector_import` / `platform` |
| `model_selection_id` | TEXT | 是 | — | 当前 model 选择 |
| `permission_selection_id` | TEXT | 是 | — | 当前 permission 选择 |
| `external_session_id` | TEXT | 是 | — | runtime 侧会话 ID |
| `title` | TEXT | 是 | — | 标题 |
| `title_source` | TEXT | 是 | — | `user` / `connector` |
| `cwd` | TEXT | 是 | — | 工作目录 |
| `status` | TEXT | 否 | — | `idle` / `waiting` / `pending` / `running` / `stopping` / `waiting_approval` / `error` / `blocked` |
| `takeover` | INTEGER | 否 | — | 0/1；是否由平台接管 |
| `pinned` | INTEGER | 否 | `0` | 0/1 |
| `pinned_at` | TEXT | 是 | — | ISO-8601 UTC |
| `archived` | INTEGER | 否 | `0` | 0/1 |
| `archived_at` | TEXT | 是 | — | ISO-8601 UTC |
| `dsh_archive_legacy` | INTEGER | 否 | `0` | DSH 旧归档兼容标记 |
| `source_state` | TEXT | 否 | `'visible'` | `available` / `archived` / `unavailable` / `deleted` / `missing` / `unknown` / `visible` / `hidden` |
| `source_state_at` | TEXT | 是 | — | ISO-8601 UTC |
| `source_state_reason` | TEXT | 是 | — | 状态原因 |
| `source_observation_origin` | TEXT | 是 | — | `event` / `inventory` / `operation` |
| `source_scan_token` | TEXT | 是 | — | 库存扫描的乐观并发令牌 |
| `last_read_seq` | BIGINT | 否 | `0` | 已读时间线水位 |
| `latest_turn_end_seq` | BIGINT | 否 | `0` | 最近一次 turn 结束的 revision |
| `timeline_reset_seq` | BIGINT | 否 | `0` | 最近一次完整快照重置的 revision |
| `last_synced_at` | TEXT | 是 | — | ISO-8601 UTC |
| `source_observed_at` | TEXT | 是 | — | ISO-8601 UTC |
| `last_activity_at` | TEXT | 是 | — | ISO-8601 UTC |
| `sort_at` | TEXT | 是 | — | 侧边栏排序时间（ISO-8601 UTC） |
| `seq` | BIGINT | 否 | — | 当前已发布的会话 revision 游标 |
| `seq_allocated_high` | BIGINT | 否 | `0` | Redis 区间租约高水位；纯 SQLite 单写可保持等于 `seq` |
| `updated_seq` | BIGINT | 否 | — | 最近变更 revision |
| `created_at` | TEXT | 否 | — | ISO-8601 UTC |
| `updated_at` | TEXT | 否 | — | ISO-8601 UTC |

约束/索引：
- PK：`id`
- FK：`connector_id → connectors(id)`（NO ACTION）
- FK：`project_id → projects(id) ON DELETE RESTRICT`
- INDEX `idx_sessions_connector_runtime_source_state (connector_id, runtime, source_state)`
- INDEX `idx_sessions_project_archived_sort (project_id, archived, pinned, sort_at)`
- Go 重建建议新增 INDEX `idx_sessions_connector_runtime_external (connector_id, runtime_id, external_session_id) WHERE external_session_id IS NOT NULL`，用于 `upsert_connector_session` 的查找。

`seq` / `updated_seq` 语义：
- 每个 session 一条独立 revision 时间线，从 0 开始。
- 平台创建 session 时 `seq = 0, updated_seq = 0`。
- connector import 新 session 时 `seq = 1, updated_seq = 1`。
- 每次时间线/状态变更按变更条数预留连续区间，返回区间起点；`seq` 与 `updated_seq` 同时推进到区间终点。

#### `session_active_runs`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `session_id` | TEXT | 否 | — | PK；FK → `sessions.id` CASCADE |
| `runtime` | TEXT | 否 | — | runtime type |
| `runtime_id` | TEXT | 否 | — | runtime instance ID；应用侧缺省时等于 `runtime` |
| `external_session_id` | TEXT | 是 | — | runtime 侧会话 ID |
| `status` | TEXT | 否 | — | 运行状态 |
| `params_json` | TEXT | 是 | — | JSON，启动参数 |
| `started_at` | TEXT | 否 | — | ISO-8601 UTC |
| `updated_at` | TEXT | 否 | — | ISO-8601 UTC |

约束/索引：
- PK：`session_id`（一个 session 最多一条活动运行）
- FK：`session_id → sessions(id) ON DELETE CASCADE`

### 1.4 timeline

#### `timeline_items`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `session_id` | TEXT | 否 | — | 复合 PK；FK → `sessions.id` CASCADE |
| `id` | TEXT | 否 | — | 复合 PK；runtime 提供的稳定 item ID |
| `type` | TEXT | 否 | — | `message` / `tool` / `artifact` / `marker` / `system` |
| `status` | TEXT | 否 | — | `pending` / `running` / `waiting_approval` / `done` / `failed` / `cancelled` / `interrupted` |
| `role` | TEXT | 是 | — | `user` / `assistant` / `system` / `tool` |
| `order_seq` | INTEGER | 否 | — | 会话内展示顺序；应用在写锁内取 `max(order_seq)+1` |
| `updated_seq` | BIGINT | 否 | — | 该 item 最近一次变更的会话 revision |
| `item_time` | TEXT | 是 | — | `max(createdAt, completedAt, updatedAt)` 的 ISO-8601 值 |
| `payload_json` | TEXT | 否 | — | 完整 `TimelineItem` JSON（含 content/source/revision/contentHash 等） |

约束/索引：
- PK：`(session_id, id)`
- FK：`session_id → sessions(id) ON DELETE CASCADE`
- INDEX `idx_timeline_items_session_updated_seq (session_id, updated_seq)`
- INDEX `idx_timeline_items_session_order_seq (session_id, order_seq)`
- INDEX `idx_timeline_items_session_latest (session_id, coalesce(item_time, '') DESC, order_seq DESC, updated_seq DESC)`
- INDEX `idx_timeline_items_item_time_type_role (item_time, type, role)`

### 1.5 shares

#### `session_shares`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `id` | TEXT | 否 | — | PK，`shr_` + `token_urlsafe(24)` |
| `user_id` | TEXT | 否 | — | FK → `users.id` CASCADE |
| `session_id` | TEXT | 否 | — | FK → `sessions.id` CASCADE |
| `scope` | TEXT | 否 | — | `message` / `session` |
| `snapshot_json` | TEXT | 否 | — | 不可变快照 JSON（分享时的 session + items） |
| `allowed_file_ids_json` | TEXT | 否 | `'[]'` | JSON 字符串数组 |
| `created_at` | TEXT | 否 | — | ISO-8601 UTC |

约束/索引：
- PK：`id`
- FK：`user_id → users.id ON DELETE CASCADE`
- FK：`session_id → sessions.id ON DELETE CASCADE`
- INDEX `idx_session_shares_user_created (user_id, created_at)`
- INDEX `idx_session_shares_session_created (session_id, created_at)`

### 1.6 terminals

#### `connector_terminal_roots`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `connector_id` | TEXT | 否 | — | 复合 PK；FK → `connectors.id` CASCADE |
| `terminal_id` | TEXT | 否 | — | 复合 PK，`trm_` + 随机串 |
| `session_id` | TEXT | 否 | — | 源 schema **没有 FK**，仅索引 |
| `root` | TEXT | 否 | — | 终端允许的根目录 |
| `cwd` | TEXT | 否 | — | 当前工作目录 |
| `created_at` | TEXT | 否 | — | ISO-8601 UTC |
| `updated_at` | TEXT | 否 | — | ISO-8601 UTC |

约束/索引：
- PK：`(connector_id, terminal_id)`
- FK：`connector_id → connectors(id) ON DELETE CASCADE`
- INDEX `idx_connector_terminal_roots_session (connector_id, session_id)`

> 终端的实时状态（pid、scrollback、cols/rows、lease）在 `TerminalBroker` 内存里，不落库；这张表只持久化“终端根目录”。

### 1.7 usage（admin 报表用，本次可只建表）

#### `usage_events`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `id` | TEXT | 否 | — | PK，`ue_` + `uuid4().hex` |
| `occurred_at` | TEXT | 否 | — | ISO-8601 UTC |
| `hour_bucket` | TEXT | 否 | — | `occurred_at[:13]`，即 `YYYY-MM-DDTHH` |
| `event_type` | TEXT | 否 | — | `user.active` / `session.created` / `session.viewed` / `message.sent` / `turn.ended` / `approval.requested` / `approval.resolved` / `runtime.error` / `connector.online` |
| `user_id` | TEXT | 是 | — | FK → `users.id` ON DELETE SET NULL |
| `connector_id` | TEXT | 是 | — | 故意无 FK，设备删除后历史仍保留 |
| `session_id` | TEXT | 是 | — | 故意无 FK，会话删除后历史仍保留 |
| `runtime_type` | TEXT | 是 | — | runtime type |
| `runtime_id` | TEXT | 是 | — | runtime instance ID |
| `device_os` | TEXT | 是 | — | `macos` / `windows` / `linux` |
| `outcome` | TEXT | 是 | — | 事件结果 |
| `duration_ms` | INTEGER | 是 | — | 持续时间 |
| `dedupe_key` | TEXT | 是 | — | 幂等键；`NULL` 表示不去重 |

约束/索引：
- PK：`id`
- UNIQUE `uq_usage_events_dedupe_key (dedupe_key)`
- FK：`user_id → users.id ON DELETE SET NULL`
- INDEX `idx_usage_events_hour (hour_bucket)`
- INDEX `idx_usage_events_type_hour (event_type, hour_bucket)`
- INDEX `idx_usage_events_session_time (session_id, occurred_at)`
- INDEX `idx_usage_events_user_hour (user_id, hour_bucket)`

#### `usage_inventory_snapshots`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `snapshot_date` | TEXT | 否 | — | 复合 PK，`YYYY-MM-DD` |
| `metric_key` | TEXT | 否 | — | 复合 PK，如 `users.total` |
| `dimension` | TEXT | 否 | `''` | 复合 PK，维度值 |
| `value` | FLOAT | 否 | — | 指标值 |
| `computed_at` | TEXT | 否 | — | ISO-8601 UTC |

约束/索引：
- PK：`(snapshot_date, metric_key, dimension)`

### 1.8 维护 / 遗留表

#### `legacy_import_archive`

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `source_table` | TEXT | 否 | — | 复合 PK |
| `row_key` | TEXT | 否 | — | 复合 PK，payload JSON 的 SHA-256 |
| `payload_json` | TEXT | 否 | — | 旧行 JSON |
| `archived_at` | TEXT | 否 | — | ISO-8601 UTC |

约束/索引：
- PK：`(source_table, row_key)`

> Go 空库重建不需要这张表；它是 v1→v2 数据归档的产物。

#### `_deprecated_app_releases`

由 v2.16 的 `app_releases` 在 v2.32 改名而来，没有任何代码路径使用。

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `platform` | TEXT | 否 | — | 复合 PK |
| `version_code` | INTEGER | 否 | — | 复合 PK |
| `version_name` | TEXT | 否 | — | 版本名 |
| `download_url` | TEXT | 是 | — | 下载地址 |
| `sha256` | TEXT | 是 | — | 安装包哈希 |
| `published` | INTEGER | 否 | `0` | 0/1 |
| `created_at` | TEXT | 否 | — | ISO-8601 UTC |
| `updated_at` | TEXT | 否 | — | ISO-8601 UTC |

约束/索引：
- PK：`(platform, version_code)`

> Go 空库重建**不要创建**。

#### `alembic_version`

Alembic 自建表。

| 列 | 类型 | NULL | 默认 | 说明 |
|---|---|---:|---|---|
| `version_num` | VARCHAR(32) | 否 | — | PK，当前 revision（当前为 `v2_44`） |

约束/索引：
- PK：`version_num`

> Go 版用 `schema_migrations` 或 `PRAGMA user_version` 替代，不保留 Alembic 表。

---

## 2. PostgreSQL 专有特性与 SQLite 替代

### 2.1 特性总表

| PG 特性 | 本 schema 是否使用 | 出现在哪里 | SQLite 方案 |
|---|---|---|---|
| `JSONB` | 否 | 所有 `*_json` 列都是 `TEXT` | 继续 `TEXT`；需要查询时加生成列/表达式索引 |
| `UUID` 类型 | 否 | 无列；API 只有 `installationId: UUID` | 继续 `TEXT`；Go 用 `crypto/rand` + 前缀 |
| `TIMESTAMPTZ` | 否 | 无列；时间戳是 ISO-8601 `TEXT` | 推荐 `INTEGER` Unix ms；或固定宽度 ISO-8601 `TEXT` |
| `ARRAY` | 否 | 列表都是 JSON 数组 `TEXT` | `TEXT` + `json_each()` / 应用层解析 |
| `ENUM` | 否 | 枚举在 `core/models.py` 的 `Literal` 中 | `TEXT` + `CHECK`（可选） |
| `SERIAL` / `IDENTITY` | 否 | 所有 PK 都由应用生成 | 不需要；见 2.6 |
| advisory lock | 是 | 时间线每会话锁、bootstrap、迁移锁 | 进程内 keyed mutex + `BEGIN IMMEDIATE` + 文件锁 |
| `ON CONFLICT` | 是 | timeline upsert、usage 去重、email 计数 | SQLite UPSERT 语法基本同形 |
| 部分索引 | 否 | 源 schema 没有 | SQLite 3.8+ 支持；建议用于 external_session_id |
| `CITEXT` | 否 | 邮箱在应用层 `lower()` | 保持应用层规范化；不要依赖 `NOCASE` |
| `RETURNING` | 是 | `_reserve_session_revisions`、email limits | SQLite 3.35+ 支持 |
| `FOR UPDATE` / `FOR SHARE` | 是 | 见 3.2 | `BEGIN IMMEDIATE` + 单写；读写分离 |
| 表达式索引 | 是 | `idx_timeline_items_session_latest` | SQLite 3.9+ 支持表达式索引 |
| `GREATEST()` | 仅迁移 | v2.24 回填 | SQLite 用 `max(a,b,...)` 或 Go 计算 |
| `pg_indexes` | 仅迁移 | v2.35 | 迁移期用 `PRAGMA index_list` / 直接 `CREATE INDEX IF NOT EXISTS` |

### 2.2 JSON：现状、查询需求与索引

所有 JSON 列清单：

| 表.列 | 内容 | 是否按 JSON 字段查询/排序 | 建议 |
|---|---|---|---|
| `connectors.user_preferences` | 本地 agent 偏好 | 否，整块读写 | `TEXT`，不加索引 |
| `connector_runtime_types.discovery_json` | 发现结果 | 否 | `TEXT` |
| `connector_runtime_types.config_schema_json` | JSON Schema | 否 | `TEXT` |
| `connector_runtime_types.ui_schema_json` | UI Schema | 否 | `TEXT` |
| `connector_runtime_types.defaults_json` | 默认配置 | 否 | `TEXT` |
| `connector_runtime_types.capabilities_json` | 能力 | 否 | `TEXT` |
| `connector_runtime_types.metadata_json` | 元数据 | 否 | `TEXT` |
| `device_runtimes.config_json` | 实例配置 | 否 | `TEXT` |
| `device_runtimes.error_json` | 错误 | 否 | `TEXT` |
| `device_runtimes.phase_json` | 启动阶段 | 否 | `TEXT` |
| `connector_runtime_catalogs.catalog_json` | 模型/权限目录 | 否 | `TEXT` |
| `connector_protocol_capabilities.capabilities_json` | 协议能力 | 否 | `TEXT` |
| `session_active_runs.params_json` | 启动参数 | 否 | `TEXT` |
| `session_shares.snapshot_json` | 分享快照 | 否 | `TEXT` |
| `session_shares.allowed_file_ids_json` | 文件 ID 数组 | 否，整组读取 | `TEXT` |
| `instance_settings.value` | 部分 key 是 JSON | 仅按 key 读整块 | `TEXT` |
| `timeline_items.payload_json` | 完整 timeline item | 否；`type/status/role/order_seq/item_time` 已单独成列 | `TEXT` |
| `legacy_import_archive.payload_json` | 旧行 | 否 | `TEXT`（Go 不建） |

结论：**不需要 JSON 索引**。如果 Go 版以后要按 payload 里的 `source.derivedKey`、`contentHash` 等字段查询，推荐两种做法：

1. 提升为正式列（首选，和现有 `type/status/role/order_seq/item_time` 的做法一致）：
```sql
ALTER TABLE timeline_items
  ADD COLUMN derived_key TEXT GENERATED ALWAYS AS (json_extract(payload_json, '$.source.derivedKey')) STORED;
CREATE INDEX idx_timeline_items_derived_key ON timeline_items(session_id, derived_key);
```

2. 全文检索：用 FTS5 external-content 表，不要把 `LIKE '%...%'` 压到 `payload_json` 上。

JSON 有效性：SQLite 3.38+ 内置 JSON1，可以加
```sql
CHECK (json_valid(payload_json))
```
但这不是必须的；应用层已经用规范 JSON 写入。若加 CHECK，注意批量导入历史脏数据时会被拒绝。

写入规范要和 Python 保持一致：
- `ensure_ascii=False`
- `sort_keys=True`
- `separators=(",", ":")`
- 不写 `NaN`/`Infinity`

Go 里对应：
```go
buf, err := json.Marshal(v) // Go 的 map key 已排序；结构体按字段顺序
```
如果要做到和 Python 逐字节一致以便 `contentHash`/`dedupe_key` 稳定，建议：
- 用 `json.Encoder` 并 `SetEscapeHTML(false)`，否则 Go 默认会做 HTML 转义（尖括号和 & 会变成 `\u` 转义序列），和 Python `ensure_ascii=False` 不一致；
- 去掉 `Encoder.Encode` 追加的尾部换行；
- 对 map 使用确定性序列化（`encoding/json` 已对 map key 排序）；
- 禁止 `omitempty` 之外的字段顺序/空白差异。

```go
var buf bytes.Buffer
enc := json.NewEncoder(&buf)
enc.SetEscapeHTML(false)
if err := enc.Encode(v); err != nil { return "", err }
out := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
```

### 2.3 UUID：TEXT 还是 BLOB

**结论：全部用 `TEXT`，不要用 `BLOB`。**

原因：
- 现有所有 ID 都是带前缀的可读字符串，不是 UUID：`usr_`、`conn_`、`sess_`、`proj_`、`shr_`、`pair_`、`trm_`、`rti_`、`file_`、`ue_`、`sht_`、`cxt_`。
- API/前端/日志/URL 都直接使用这些字符串。
- `BLOB` 会让日志、调试、SQLite CLI、JSON 序列化都变麻烦，且需要额外的编解码层。
- **没有任何列需要真正的 128-bit UUID 语义**。

现有代码的 ID 生成方式：

| 实体 | 生成方式 | 示例 |
|---|---|---|
| user | `usr_` + `secrets.token_hex(12)` | `usr_ab12...` |
| connector | `conn_` + `token_urlsafe(10)`，或 `conn_` + `sha256(user_id:installation_id)[:24]` | `conn_xY...` |
| session | `sess_` + `token_urlsafe(10)` | `sess_xY...` |
| project | `proj_` + `token_urlsafe(10)` | `proj_xY...` |
| share | `shr_` + `token_urlsafe(24)` | `shr_xY...` |
| pairing | `pair_` + `token_urlsafe(10)`，code 为 6 位数字 | `pair_xY...` / `042731` |
| runtime instance | `rti_` + `token_urlsafe(12)` | `rti_xY...` |
| terminal | `trm_` + `token_urlsafe(10)` 或 `token_urlsafe(18)` | `trm_xY...` |
| attachment | `file_` + `token_urlsafe(12)` | `file_xY...` |
| usage event | `ue_` + `uuid4().hex` | `ue_3f2a...` |
| shell task | `sht_` + `token_urlsafe(12)` | `sht_xY...` |
| connector token | `cxt_` + `token_urlsafe(32)` | `cxt_xY...` |

Go 实现建议：
```go
func newID(prefix string, n int) string {
    b := make([]byte, n)
    if _, err := rand.Read(b); err != nil { panic(err) }
    return prefix + base64.RawURLEncoding.EncodeToString(b)
}
```
- 对 `usage_events` 想要和 Python 一样的 32 位 hex：`"ue_" + hex.EncodeToString(b16)`。
- **不要**把 `installation_id` 直接当主键；它只参与 `sha256(user_id:installation_id)` 的确定性派生。
- SQLite 的 `TEXT` 主键是 B-tree 比较，前缀会让相邻 ID 在索引里聚集；这不是问题，因为查询都带完整主键或其它过滤条件。

### 2.4 时间戳：INTEGER(ms) 还是 TEXT

**建议：新 Go 版统一用 `INTEGER` Unix 毫秒（UTC）；只有“日期/小时桶”继续用 `TEXT`。**

理由：
1. SQLite 没有原生时间类型。`TEXT` 的比较是字节比较，只有格式完全固定才正确。现有 Python `utc_now()` 是 `datetime.now(UTC).isoformat()`，微秒为 0 时会省略小数位，导致 `...:00Z` 和 `...:00.123Z` 的字典序与时间序不一致。Go 重写正好可以消除这个隐患。
2. `INTEGER` 是 8 字节定长，索引更小，范围查询、`min/max`、排序都更快，不受时区/格式/精度影响。
3. 现有 API 的 `serverTime`/`createdAt` 本来就在边界做字符串格式化，Go 只需在 JSON 层 `time.UnixMilli(v).UTC().Format(time.RFC3339Nano)`。
4. 邮件验证码表已经是整数（Unix 秒），改成毫秒只需改应用代码，不改变“整数时间”的设计方向。

例外：以下列是**聚合键/日历键**，必须保持 `TEXT`：
- `usage_events.hour_bucket`：`YYYY-MM-DDTHH`
- `usage_inventory_snapshots.snapshot_date`：`YYYY-MM-DD`
- 如果保留 `platform_user_activity.activity_date`（已删除）：`YYYY-MM-DD`

如果必须保留 PostgreSQL 数据/快照 1:1，或者不想改 API 序列化，则用 `TEXT`，但**必须固定宽度**：
```
YYYY-MM-DDTHH:MM:SS.sssZ   // 24 字符，始终带毫秒
```
这样字典序 == 时间序。不要混用“有小数/无小数”的 ISO 字符串。

本文件第 3 节的推荐 DDL 采用 `INTEGER` 毫秒。

### 2.5 ARRAY / ENUM

- **ARRAY：不存在。** 所有列表都是 JSON 数组 TEXT：`redirect_uris_json`、`allowed_file_ids_json`、`choices_json`、`actions_json` 等。SQLite 继续 TEXT；需要展开时用 `json_each()`：
```sql
SELECT value FROM json_each((SELECT allowed_file_ids_json FROM session_shares WHERE id = ?));
```
- **ENUM：数据库层不存在。** 枚举值在 `core/models.py` 的 `Literal` 中，DB 只存 TEXT。Go 重建建议：
  - 主要状态加 `CHECK (... IN (...))`，把契约写进 schema；
  - 对可能扩展的 provider/runtime type、event_type、outcome 不加快照式 CHECK，避免每次新增枚举都要迁移；
  - 应用层仍然做白名单校验（对应 Python 的 Pydantic `Literal`）。

推荐加 CHECK 的列：
- `users.role`
- `connectors.connector_kind`、`connectors.device_os`
- `sessions.status`、`sessions.origin`、`sessions.source_state`、`sessions.source_observation_origin`、`sessions.title_source`
- `timeline_items.type`、`timeline_items.status`、`timeline_items.role`
- `session_shares.scope`
- `pairing_codes.status`
- `email_verification_codes.purpose`
- `usage_inventory_snapshots` 的 metric_key 不建议 CHECK

不建议加 CHECK 的列：
- `connector_runtime_types.runtime_type` / `implementation_type`（新 provider 会扩展）
- `usage_events.event_type` / `outcome`
- `device_runtimes.instance_policy`
- `sessions.status` 如果你希望未来加状态而不迁移；但它是核心协议状态，建议加。

### 2.6 SERIAL / IDENTITY / 自增 / 序列

- **本 schema 没有任何 `SERIAL`、`IDENTITY`、`nextval()`、数据库序列。**
- 所有主键都是应用生成的 TEXT。
- `timeline_items.order_seq` 不是数据库序列：它在每会话写锁内用 `SELECT max(order_seq)` + 1 分配（见 `_max_timeline_order_seq` / `_live_order_seq_for_upsert`）。
- `sessions.seq` 也不是数据库序列：它是每个 session 自己的 revision 计数器，由应用在事务里 `+N`。
- 邮件验证码的时间是 Unix 整数，但也不是自增。

**SQLite 的 `AUTOINCREMENT` 必要性：不需要。**

解释：
- `INTEGER PRIMARY KEY` 本身就是 rowid 别名，插入时自动分配，**不加 `AUTOINCREMENT`** 也能自增。
- 加 `AUTOINCREMENT` 的唯一作用是保证“删除后的 rowid 永不复用”，代价是多一个 `sqlite_sequence` 表、每次插入多一次写、以及额外的锁开销。
- 本 schema 没有任何表依赖“ID 永不复用”的整数主键；会话 revision 的“不复用”是由应用计数器保证的，不由 SQLite rowid 保证。
- 如果 Go 版新增纯本地表需要整数主键，优先 `INTEGER PRIMARY KEY`（不写 `AUTOINCREMENT`）；只有明确需要“绝不复用 ID”时才用 `AUTOINCREMENT`。

### 2.7 advisory lock

当前位置：

| 位置 | 锁 | 用途 |
|---|---|---|
| `repositories/timeline.py:585-600` | `pg_advisory_lock(hash(session_id))` | 单会话时间线写入串行化 |
| `repositories/timeline.py:618-622` | 上述 key 的计算 | SHA-256 前 8 字节转 signed int64 |
| `repositories/email_accounts.py:352` | `pg_advisory_xact_lock(4141454)` | 首次 bootstrap 创建第一个用户 |
| `infra/db/migrations.py:74-101` | `pg_try_advisory_lock(0x414147454E545332)` + unlock | 多实例迁移互斥 |

SQLite 替代：
- **每会话时间线锁**：进程内 `map[string]*sync.Mutex`（带引用计数/过期清理，避免无限增长）+ 单写事务。多进程部署时，SQLite 本身只允许一个写者，`BEGIN IMMEDIATE` 会串行化；但仍建议只让一个进程写。
- **bootstrap 锁**：`BEGIN IMMEDIATE` + `SELECT COUNT(*) FROM users` + 插入。单写事务已经原子；不需要额外锁。若要跨进程，用文件锁。
- **迁移锁**：用 OS 文件锁（`flock` / `LockFileEx`）包住整个迁移过程，再执行 DDL。SQLite 没有 advisory lock。
- **不要**在 SQLite 里模拟 `pg_advisory_lock` 的“连接级可重入锁”；Go 的 `sync.Mutex` 不是可重入的，保持“一个函数只加一次锁”。

### 2.8 ON CONFLICT

当前位置：

| 位置 | 语句 | 语义 |
|---|---|---|
| `infra/timeline_store.py:131-142` | `INSERT ... ON CONFLICT (session_id, id) DO UPDATE SET ... WHERE excluded.updated_seq >= timeline_items.updated_seq` | 时间线 item 幂等 upsert；旧 revision 不覆盖新 revision |
| `infra/repositories/usage.py:41` | `INSERT ... ON CONFLICT DO NOTHING` | usage 事件按 `dedupe_key` 去重 |
| `infra/repositories/email_accounts.py:178,211` | `ON CONFLICT DO NOTHING` | 创建限流行、验证码占位行 |
| `infra/repositories/store_support.py` / `instance_settings.py` | 先 SELECT 再 INSERT/UPDATE（不是 ON CONFLICT） | Go 版建议改成 UPSERT |

SQLite 写法：
- 同形 UPSERT（3.24+）：
```sql
INSERT INTO timeline_items (session_id, id, type, status, role, order_seq, updated_seq, item_time, payload_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_id, id) DO UPDATE SET
  type        = excluded.type,
  status      = excluded.status,
  role        = excluded.role,
  order_seq   = excluded.order_seq,
  updated_seq = excluded.updated_seq,
  item_time   = excluded.item_time,
  payload_json= excluded.payload_json
WHERE excluded.updated_seq >= timeline_items.updated_seq;
```
- 去重：
```sql
INSERT INTO usage_events (id, occurred_at, hour_bucket, event_type, user_id, connector_id,
                          session_id, runtime_type, runtime_id, device_os, outcome,
                          duration_ms, dedupe_key)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(dedupe_key) DO NOTHING;
```
注意：
  - `dedupe_key IS NULL` 的行永远不会冲突（PG/SQLite 都把 NULL 视为互不相等）。这正是源语义：`None` 表示每次出现都记录。
  - 不要用 `INSERT OR IGNORE` 代替带目标列的 `ON CONFLICT`，因为 `OR IGNORE` 会静默忽略 NOT NULL、CHECK、UNIQUE 等约束错误，排障困难。
- 设置项 UPSERT：
```sql
INSERT INTO instance_settings (key, value, updated_at)
VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET
  value = excluded.value,
  updated_at = excluded.updated_at;
```

### 2.9 部分索引、表达式索引、CITEXT

- **部分索引**：源 schema 没有。Go 版建议加一个：
```sql
CREATE INDEX idx_sessions_connector_runtime_external
  ON sessions (connector_id, runtime_id, external_session_id)
  WHERE external_session_id IS NOT NULL;
```
  它精确覆盖 `upsert_connector_session` / `resolve_session` 的查找条件，且比全表索引小。
- **表达式索引**：源 schema 有：
```sql
CREATE INDEX idx_timeline_items_session_latest
  ON timeline_items (session_id, coalesce(item_time, '') DESC, order_seq DESC, updated_seq DESC);
```
  SQLite 3.9+ 支持表达式索引。注意：
  - 表达式必须是确定性的；`coalesce` 可以。
  - 查询必须使用同样的表达式才能命中索引。如果 Go 版把 `item_time` 改成 `INTEGER`，索引改成 `coalesce(item_time, 0) DESC`，并且查询也用 `coalesce(item_time, 0)`，或者确认 `DESC` 的 NULL 排序与 `0` 一致。
  - SQLite 对 `DESC` 索引的支持没有问题；但混合 ASC/DESC 的查询可能无法命中，保持查询的排序方向与索引一致最稳。
- **CITEXT**：源 schema 没有。邮箱在写入前已经 `strings.ToLower(strings.TrimSpace(...))`。Go 版：
  - 继续在应用层规范化；
  - 唯一索引直接建在规范化后的 `email` 上；
  - **不要**用 `COLLATE NOCASE` 作为唯一性保证：SQLite 的 `NOCASE` 只折叠 ASCII A-Z，对 Unicode 邮箱不完整；而 PG 的 `CITEXT` 也不是默认行为。
  - 如果担心历史数据大小写不一致，迁移时先 `UPDATE users SET email = lower(trim(email))`，再建唯一索引。

### 2.10 其他 SQL 差异

| 源写法 | SQLite 写法 | 说明 |
|---|---|---|
| `UPDATE ... RETURNING col` | 同形，3.35+ | 用于 revision 分配、email 限流 |
| `SELECT ... FOR UPDATE` | 删掉，改 `BEGIN IMMEDIATE` | 见 3.3 |
| `SELECT ... FOR NO KEY UPDATE` / `FOR SHARE OF` | 删掉 | SQLite WAL 没有行级共享锁 |
| `GREATEST(a,b)` | `max(a,b)`（SQLite 的 `max` 是标量函数） | 仅迁移回填用 |
| `pg_indexes` | `PRAGMA index_list(table)` | 迁移幂等可用 `CREATE INDEX IF NOT EXISTS` |
| `BOOLEAN` | `INTEGER` + `CHECK (x IN (0,1))` | SQLite 没有布尔类型 |
| `BIGINT` | `INTEGER` | SQLite 整数统一 64-bit |
| `DOUBLE PRECISION` | `REAL` | |
| `VARCHAR(32)` | `TEXT` | SQLite 不强制长度 |
| 默认大小写敏感比较 | 默认 `BINARY`，行为一致 | 不要无意中加 `NOCASE` |
| `NULL` 排序 | SQLite 中 `NULL` 最小 | `DESC` 时 NULL 在最后，和 PG 默认一致 |

---

## 3. 并发写设计

### 3.1 当前 PostgreSQL 的 revision 分配

核心表：`sessions.seq`、`sessions.updated_seq`、`sessions.seq_allocated_high`、`timeline_items.updated_seq`。

当前流程（`repositories/timeline.py`）：

1. 进入 `_timeline_lock(session_id)`：对 `sha256(session_id)` 的前 8 字节取 signed int64，执行 `pg_advisory_lock(key)`。这是**连接级**锁，保证同一 session 的写入串行。
2. 进入一个数据库事务。
3. `SELECT seq, seq_allocated_high FROM sessions WHERE id = ? FOR UPDATE`。
4. `allocated_floor = max(seq, seq_allocated_high)`。
5. 如果 `allocated_floor + count > 2^53 - 1`，抛出溢出。
6. 可选地 `seal_session_revision_range`，把 Redis 租约作废。
7. `UPDATE sessions SET seq = next, updated_seq = next, seq_allocated_high = next, updated_at = ? WHERE id = ? AND seq_allocated_high = <expected_high> RETURNING seq`。
   - 这是**乐观 fence**：如果另一个事务改了 `seq_allocated_high`，`rowcount = 0`，抛 `RuntimeError("session revision allocation fence changed")`。
8. 返回 `next - count + 1` 作为区间起点。
9. 在同一个事务里 upsert `timeline_items` 并提交。

分布式模式下（`SessionRevisionAllocator`）：
- Redis 是低延迟的 head counter。
- PostgreSQL 用 `lease_session_revision_range` 一次租 4096 个 revision（只推进 `seq_allocated_high`，不推进 `seq`）。
- 每个 session 还有 Redis 锁 `session-revision:<id>`，保证分配、暂存、发布顺序。
- `sessions.seq` 仍然只在“把缓冲的时间线真正落库”时推进。
- 这样即使 Redis 重启，未用完的区间会被放弃，revision 可能出现空洞；客户端必须能接受空洞。

`order_seq` 分配：
- 在同一个 session 写锁/事务里，`SELECT max(order_seq)`，新 item 取 `max + 1`。
- 更新已有 item 保持原 `order_seq`。

### 3.2 所有并发/锁点清单

| 位置 | 机制 | 保护对象 | SQLite 替代 |
|---|---|---|---|
| `timeline.py:_timeline_lock` | `pg_advisory_lock(session_id hash)` | 单 session timeline 写 | 每 session `sync.Mutex` + `BEGIN IMMEDIATE` |
| `timeline.py:_reserve_session_revisions` | `SELECT ... FOR UPDATE` + 条件 UPDATE fence + RETURNING | `sessions.seq` 区间 | `BEGIN IMMEDIATE` 内 SELECT + UPDATE；fence 可删或保留为断言 |
| `timeline.py:lease_session_revision_range` | 条件 UPDATE + RETURNING | Redis 区间租约 | 纯 SQLite 不需要；直接按需 +N |
| `timeline_store.py:upsert_many` | `ON CONFLICT ... WHERE excluded.updated_seq >= ...` | 时间线幂等 | 同形 UPSERT |
| `usage.py:insert_usage_events` | `ON CONFLICT DO NOTHING` | usage 去重 | 同形 UPSERT |
| `email_accounts.py:issue_email_code` | `ON CONFLICT DO NOTHING` + `FOR UPDATE` + 条件 UPDATE | 限流/验证码一次性 | `BEGIN IMMEDIATE` + 条件 UPDATE |
| `email_accounts.py:create_email_user` | `pg_advisory_xact_lock(4141454)` | 首个用户 bootstrap | `BEGIN IMMEDIATE` + `COUNT(*)` |
| `connectors.py:delete_connector` | `SELECT ... FOR UPDATE` | connector 删除与 FK 写竞争 | 每 connector mutex + `BEGIN IMMEDIATE`；按源顺序先删 sessions 再删 projects，不要依赖 CASCADE |
| `connectors.py:create_connector` | UNIQUE `id` + IntegrityError | installation_id 重试 | 同形 UNIQUE + `ON CONFLICT` |
| `connectors.py:claim_pairing` | 读后无条件 UPDATE（源实现有 TOCTOU） | pairing 一次性认领 | 条件 UPDATE `WHERE status='pending' AND expires_at>?` + 检查 rowcount |
| `connectors.py:poll_pairing` | 读后 UPDATE | pairing 一次性消费 | 条件 UPDATE `WHERE status='claimed'` + 检查 rowcount |
| `device_runtimes.py:create` | `FOR NO KEY UPDATE` 锁类型行 | 并发创建实例/计数 | 每 `(connector,runtime_type)` mutex + `BEGIN IMMEDIATE` |
| `projects.py` | `FOR SHARE` 锁 connector | 项目创建 vs connector 删除 | 每 connector 读锁可省略；写事务内检查 `revoked=0` |
| `attachments.py` | `FOR SHARE OF sessions` | 上传 vs 会话/设备删除 | 每 session mutex + 事务内检查；文件删除放到 SQL 提交后 |
| `protocol_catalogs.py` | `FOR UPDATE` 锁 connector | catalog revision 更新 | `BEGIN IMMEDIATE` + 条件 UPDATE |
| `sessions.py:set_session_status` | `FOR UPDATE` + expected_status 条件 UPDATE | 状态机 CAS | `BEGIN IMMEDIATE` + `UPDATE ... WHERE status = expected` |
| `sessions.py:inventory observation` | `FOR UPDATE` + `source_scan_token` CAS | 库存扫描 | `BEGIN IMMEDIATE` + 条件 UPDATE `WHERE source_scan_token = ?` |
| `migrations.py` | `pg_try_advisory_lock` | 多实例迁移 | OS 文件锁 + `BEGIN EXCLUSIVE` |
| `instance_settings` | 先查后写 | 并发设置更新 | `ON CONFLICT(key) DO UPDATE` |
| `active_runs` | 先查后写 | 单 session 一条 | `ON CONFLICT(session_id) DO UPDATE` |

### 3.3 SQLite 的总体方案（推荐架构）

```
┌─────────────────────────────────────────────────────┐
│ Go process                                          │
│                                                     │
│  HTTP/WS handlers ──► per-session mutex ──► writer   │
│                              │                │      │
│                              │         single writer │
│                              │         connection    │
│                              ▼                │      │
│                       BEGIN IMMEDIATE ────────┘      │
│                                                     │
│  read handlers ──► read pool (N connections) ──► WAL │
└─────────────────────────────────────────────────────┘
```

必须做的配置：

```sql
PRAGMA journal_mode = WAL;       -- 读写并发；必须成功返回 "wal"
PRAGMA synchronous = NORMAL;     -- WAL 下的常规选择；崩溃最多丢最近事务
PRAGMA foreign_keys = ON;        -- 每个连接都要设
PRAGMA busy_timeout = 5000;      -- 写锁等待 5s
PRAGMA temp_store = MEMORY;
PRAGMA wal_autocheckpoint = 1000;
```

Go DSN 示例：

- `modernc.org/sqlite`：
```
file:agent.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate
```
- `github.com/mattn/go-sqlite3`：
```
file:agent.db?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on&_txlock=immediate
```

如果驱动的 `_txlock` 不可靠，就在专用 `*sql.Conn` 上显式执行 `BEGIN IMMEDIATE`：

```go
func (s *Store) writeTx(ctx context.Context, fn func(context.Context, *sql.Conn) error) error {
    conn, err := s.writeDB.Conn(ctx) // writeDB.SetMaxOpenConns(1)
    if err != nil {
        return err
    }
    defer conn.Close()

    if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
        return err
    }
    committed := false
    defer func() {
        if !committed {
            _, _ = conn.ExecContext(context.Background(), "ROLLBACK")
        }
    }()

    if err := fn(ctx, conn); err != nil {
        return err
    }
    if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
        return err
    }
    committed = true
    return nil
}
```

关键规则：

1. **所有写事务都从 `BEGIN IMMEDIATE` 开始**。不要用默认的 `BEGIN`（deferred），否则“先读后写”会在升级写锁时遇到 `SQLITE_BUSY`，而且两个事务都可能先读到旧值。
2. **`database/sql` 写连接池设为 1**：`writeDB.SetMaxOpenConns(1)`。读连接池可以多开（`readDB.SetMaxOpenConns(runtime.NumCPU())` 或 4–8），连接串加 `mode=ro` 可选。
3. **每 session 一个 mutex**，保证同一 session 的 revision/order_seq 顺序稳定。不同 session 可以并行进入 SQLite 写队列，但 SQLite 仍然是单写者，所以高并发时写会排队。若写入量大，用一个 writer goroutine + channel 串行化，减少 `SQLITE_BUSY`。
4. **不要在写事务里做网络 I/O**（connector RPC、S3、文件上传）。先在事务外准备好数据，事务只做 SQL。否则会长时间持有 SQLite 写锁，拖垮所有写。
5. **`PRAGMA foreign_keys=ON` 是连接级设置**，必须通过 DSN 或连接初始化钩子保证每个连接都设置。
6. **WAL 文件放在本地磁盘**，不要放网络共享/NFS；WAL 依赖共享内存和文件锁。
7. 进程退出时执行 `PRAGMA wal_checkpoint(TRUNCATE)`；长时间运行可定期 `PRAGMA wal_checkpoint(PASSIVE)`。
8. 如果必须多进程共享同一个 SQLite 文件：只允许一个进程写，其他进程以 `mode=ro` 打开；迁移用 OS 文件锁串行。不要依赖 `busy_timeout` 解决长时间写竞争。
9. 对 `SQLITE_BUSY` / `SQLITE_LOCKED` 做有限次指数退避重试（例如 5 次，10ms 起）。`busy_timeout` 只覆盖等锁，不覆盖 WAL checkpoint 或 schema 变更导致的瞬时冲突。
10. 读事务要短。WAL 下长读不会阻塞写，但会阻止 checkpoint 回收 WAL，导致 WAL 文件持续增长。

### 3.4 revision 分配的 Go 写法

纯 SQLite 单写者不需要 `FOR UPDATE`、`seq_allocated_high` 区间租约或 Redis。在 `BEGIN IMMEDIATE` 事务里直接读-改-写即可：

```go
const maxRevision = int64(1<<53 - 1)

// 必须在 BEGIN IMMEDIATE 事务内调用。
func reserveSessionRevisions(
    ctx context.Context,
    conn *sql.Conn,
    sessionID string,
    count int64,
    nowMS int64,
) (first int64, err error) {
    if count <= 0 {
        return 0, errors.New("revision count must be positive")
    }
    var seq, high int64
    err = conn.QueryRowContext(ctx,
        `SELECT seq, seq_allocated_high FROM sessions WHERE id = ?`,
        sessionID,
    ).Scan(&seq, &high)
    if err != nil {
        return 0, err
    }
    floor := seq
    if high > floor {
        floor = high
    }
    if floor > maxRevision-count {
        return 0, errors.New("session revision exceeds protocol limit")
    }
    next := floor + count

    _, err = conn.ExecContext(ctx, `
        UPDATE sessions
           SET seq = ?, updated_seq = ?, seq_allocated_high = ?, updated_at = ?
         WHERE id = ?`,
        next, next, next, nowMS, sessionID,
    )
    if err != nil {
        return 0, err
    }
    return floor + 1, nil
}
```

要点：
- 因为整个函数在 `BEGIN IMMEDIATE` 里，SELECT 和 UPDATE 之间不会有其他写者；**不需要 `FOR UPDATE`，也不需要 `WHERE seq_allocated_high = ?` 的 fence**。
- 保留 `seq_allocated_high = next` 可以让从 PG/Redis 版本移植过来的语义继续成立；如果确定不保留 Redis 租约，可以删掉这一列，SQL 里也去掉。
- `updated_seq` 和 `seq` 同步推进到区间终点；返回的是区间起点。
- 如果一次写 N 个 item，先 `reserveSessionRevisions(..., N)`，然后第 i 个 item 用 `first+i`。
- `order_seq` 在同一个事务里单独算：
```sql
SELECT COALESCE(MAX(order_seq), 0) FROM timeline_items WHERE session_id = ?;
-- new item: max + 1
```
不要用 SQLite 的 rowid 或 `AUTOINCREMENT` 代替 `order_seq`；它是会话内顺序，不是全局 ID。

### 3.5 时间线 upsert / 幂等

SQLite 写法见 2.8。补充：
- `updated_seq >= timeline_items.updated_seq` 中的 `>=` 与源实现一致：同 revision 的重复写入允许覆盖（用于修复同 revision 内容差异）。
- 如果 `WHERE` 不成立，SQLite 不报错，`RowsAffected()` 为 0；调用方可以据此判断“旧 revision 被忽略”。
- 批量 upsert 用一条多 VALUES 语句或 prepared statement 循环；单条语句最多 999 个参数（老版本）或 32766（3.32+），按列数分批。
- `payload_json` 建议保留完整的 `TimelineItem`，并在列上冗余 `type/status/role/order_seq/item_time`，与源 schema 一致。这样列表查询不需要解析 JSON。

### 3.6 pairing / 一次性操作的条件更新

源 `claim_pairing` 是“先 SELECT 检查 status，再 UPDATE”，在 PG 下也有 TOCTOU。Go 版直接条件更新：

```sql
-- claim
UPDATE pairing_codes
   SET status = 'claimed',
       connector_id = ?,
       connector_token = ?,
       claimed_at = ?,
       server_url = COALESCE(?, server_url)
 WHERE id = ?
   AND status = 'pending'
   AND expires_at > ?;
-- RowsAffected() == 1 才算认领成功
```

```sql
-- poll consume
UPDATE pairing_codes
   SET status = 'consumed', consumed_at = ?
 WHERE id = ?
   AND status = 'claimed';
-- RowsAffected() == 1 才返回 connector config
```

同样模式适用于：
- `sessions.set_session_status`：`UPDATE ... WHERE id=? AND status=?`，检查 rowcount。
- `source_scan_token` 库存观察：`UPDATE ... WHERE id=? AND source_scan_token=?`。
- 验证码消费：`UPDATE ... WHERE scope=? AND consumed_at IS NULL AND failed_attempts < ?`，检查 rowcount。

### 3.7 唯一性与幂等键

- `usage_events.dedupe_key`：`UNIQUE`，`NULL` 不参与冲突。
- `device_runtimes (connector_id, name_key)`：UNIQUE，创建实例时用 `ON CONFLICT DO NOTHING` + 重新读取，或直接捕获唯一冲突。
- `pairing_codes.code`：UNIQUE。创建时如果冲突，重新生成 6 位码；不要用“先 SELECT 再 INSERT”作为唯一性保证。
- `users.email`：UNIQUE，多个 NULL 允许。注册时捕获唯一冲突并返回“邮箱已存在”。
- `projects` 的两个 UNIQUE：创建时捕获冲突，按业务语义返回已有项目或改名错误。

### 3.8 迁移锁与 schema 版本

SQLite 没有 advisory lock。推荐：

1. 用 OS 文件锁包住迁移（Go 可用 `github.com/gofrs/flock`；Windows 用 `LockFileEx`，Unix 用 `flock`）。
2. 迁移连接执行 `PRAGMA foreign_keys=OFF`（如果要做表重建），迁移结束再 `ON`。
3. 整个迁移放在 `BEGIN EXCLUSIVE` 里；这会阻塞其他读写，迁移应短小。
4. 版本记录二选一：
   - `PRAGMA user_version = <n>`（最简单，但只能放整数）；
   - `schema_migrations(version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`（推荐，能记录顺序和时间）。
5. 每个迁移用 `CREATE TABLE IF NOT EXISTS` / `CREATE INDEX IF NOT EXISTS` 保持幂等；但不要依赖它替代版本表。

---

## 4. 建议的 Go 版 SQLite DDL

说明：
- 这是**空库重建的最优形态**，不是逐字翻译。
- 时间戳统一为 `INTEGER` Unix 毫秒（UTC）。
- `STRICT` 需要 SQLite 3.37+；`RETURNING` 需要 3.35+；UPSERT 需要 3.24+；表达式索引需要 3.9+；部分索引需要 3.8+。建议最低要求 **SQLite 3.45+**。
- 标注 `-- 增强` 的约束是源 schema 没有、但重建时建议加的。
- 标注 `-- 可选` 的列/表可以删除。
- 建表顺序已经按 FK 依赖排好。

### 4.1 连接初始化

```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
PRAGMA temp_store = MEMORY;
PRAGMA wal_autocheckpoint = 1000;

CREATE TABLE IF NOT EXISTS schema_migrations (
  version    TEXT PRIMARY KEY,
  applied_at INTEGER NOT NULL
) STRICT;
```

### 4.2 users / auth

```sql
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

-- email 在应用层 lower+trim 后写入；多个 NULL 允许
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
```

### 4.3 connectors / runtime / pairing

```sql
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
```

### 4.4 sessions / projects

```sql
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
-- 增强：精确覆盖按 external_session_id 的查找
CREATE INDEX idx_sessions_connector_runtime_external
  ON sessions(connector_id, runtime_id, external_session_id)
  WHERE external_session_id IS NOT NULL;

-- 删除 connector 的顺序必须是：
--   DELETE FROM sessions WHERE connector_id = ?;
--   DELETE FROM projects WHERE connector_id = ?;
--   DELETE FROM connectors WHERE id = ?;
-- 因为 sessions.connector_id 是 NO ACTION，而 sessions.project_id 是 RESTRICT；
-- 不要改成两边都 CASCADE，SQLite 的级联顺序不保证先删 sessions 再删 projects。

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
```

### 4.5 timeline

```sql
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
```

如果保留 TEXT 时间戳，把 `item_time INTEGER` 改成 `item_time TEXT`，表达式索引改成 `coalesce(item_time, '') DESC`，其余不变。

配套查询要写成同样的表达式才能命中索引，例如“每个 session 的最新一条”：

```sql
SELECT * FROM timeline_items
 WHERE session_id = ?
 ORDER BY coalesce(item_time, 0) DESC, order_seq DESC, updated_seq DESC
 LIMIT 1;
```

分页查询则用 `ORDER BY order_seq DESC, updated_seq DESC, id DESC`（命中 `idx_timeline_items_session_order_seq`）或 `ORDER BY updated_seq`（命中 `idx_timeline_items_session_updated_seq`）。

### 4.6 shares

```sql
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
```

### 4.7 terminals

```sql
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
```

### 4.8 usage

```sql
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
```

### 4.9 settings / legacy

```sql
CREATE TABLE instance_settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at INTEGER NOT NULL
) STRICT;

-- Go 空库重建可完全跳过；保留仅为导入旧数据
CREATE TABLE legacy_import_archive (
  source_table TEXT NOT NULL,
  row_key      TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  archived_at  INTEGER NOT NULL,
  PRIMARY KEY (source_table, row_key)
) STRICT;
```

### 4.10 notices / approvals（当前无表，可选）

**当前 v2.44 没有 `notices` 或 `approvals` 表。** 历史：
- `approvals` 在 v2.0 基线存在，v2.3 迁移进 `notices` 后删除。
- `notices` 在 v2.0 存在，v2.6 曾被删除/重建，v2.7 最终删除。
- 现在 notice/approval 是 connector 通知事件里的**瞬态对象**（`NoticeIn` / `ApprovalIn`），经 WebSocket 广播，不落库。

因此 Go 版**默认不要建表**。只有当你需要“服务重启后仍能展示待审批/通知”时才加。可选的统一表（近似历史 `notices` 形状）：

```sql
-- 可选：仅当需要跨重启持久化 runtime notice / approval 时创建
CREATE TABLE runtime_notices (
  id                TEXT PRIMARY KEY,
  session_id        TEXT NOT NULL
                        REFERENCES sessions(id) ON DELETE CASCADE,
  type              TEXT NOT NULL CHECK (type IN ('notification','interaction')),
  status            TEXT NOT NULL,
  interaction_type  TEXT,
  blocking_json     TEXT,
  response_required INTEGER NOT NULL DEFAULT 0 CHECK (response_required IN (0,1)),
  severity          TEXT NOT NULL CHECK (severity IN ('info','success','warning','error')),
  title             TEXT NOT NULL,
  message           TEXT,
  source_json       TEXT NOT NULL,
  actions_json      TEXT NOT NULL,
  context_json      TEXT NOT NULL,
  metadata_json     TEXT NOT NULL,
  revision          INTEGER NOT NULL DEFAULT 1,
  updated_seq       INTEGER NOT NULL,
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL,
  expires_at        INTEGER,
  resolved_at       INTEGER
) STRICT;

CREATE INDEX idx_runtime_notices_session_status
  ON runtime_notices(session_id, status);
```

如果还需要独立的 approval 语义，建议不要再建 `approvals` 表，而是用 `runtime_notices` 的 `type='interaction', interaction_type='approval'`，与当前代码的 notice 模型一致。

### 4.11 attachments（当前无表，可选）

**当前没有 attachments 表。** 附件是文件系统/S3：

```
<files-root>/<session_id>/<file_id>.bin
<files-root>/<session_id>/<file_id>.json
```

`.json` 内容：
```json
{
  "fileId": "file_...",
  "sessionId": "sess_...",
  "path": "",
  "name": "report.pdf",
  "size": 12345,
  "sha256": "....",
  "mediaType": "application/pdf",
  "origin": "user",
  "createdAt": "2026-10-24T12:00:00.000000Z"
}
```

Go 版可以继续用文件系统 + sidecar，不需要表。若要把元数据放进 SQLite（推荐用于列表/清理/引用完整性），可加：

```sql
-- 可选：把附件元数据从 sidecar 迁进 SQLite
CREATE TABLE attachments (
  id           TEXT PRIMARY KEY,
  session_id   TEXT NOT NULL
                   REFERENCES sessions(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  media_type   TEXT NOT NULL DEFAULT '',
  size         INTEGER NOT NULL,
  sha256       TEXT NOT NULL,
  origin       TEXT NOT NULL DEFAULT 'user',
  storage_key  TEXT NOT NULL,
  created_at   INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_attachments_session_created
  ON attachments(session_id, created_at);
```

注意：如果保留 sidecar 文件，SQLite 行和文件写入无法在同一事务里原子提交。推荐顺序：
1. 写临时文件并 `fsync`；
2. 原子 `rename` 到最终路径；
3. `BEGIN IMMEDIATE` → 插入 SQLite 行 → `COMMIT`；
4. 如果第 3 步失败，补偿删除最终文件。

这样“行存在 ⇒ 文件存在”；崩在第 2、3 步之间只会留下可清理的孤儿文件，不会出现指向不存在文件的行。删除时反过来：先删 SQLite 行并提交，再删文件（源实现也是这样：SQL 删完后再删 blob）。

### 4.12 announcements（当前无表）

公告存在 `instance_settings` 的单行里：

```
key   = 'public_announcement'
value = {"enabled":true,"markdown":"...","publishedAt":"2026-10-24T12:00:00.000Z"}
```

- 不需要新表。
- 并发更新用 `ON CONFLICT(key) DO UPDATE`。
- 如果需要保留公告历史/审计，再考虑 `announcements` 表；当前 schema 没有。

---

## 5. 本次可以“只建表不写逻辑”的部分

按你的分组：

| 领域 | 涉及表 | 建议 | 说明 |
|---|---|---|---|
| **admin** | `users.role` / `users.disabled`；`usage_events`；`usage_inventory_snapshots` | 只保留列/表，不实现后台接口 | 没有独立的 admin 表 |
| **usage** | `usage_events`、`usage_inventory_snapshots` | 建表，可暂不写入/不聚合 | 报表逻辑在 `services/usage_report.py` / `usage_maintenance.py`，可整体后置 |
| **oauth** | `oauth_accounts`、`oauth_clients`、`oauth_authorization_codes`；`instance_settings` 的 `oauth_provider` / `oauth_registration_open` | 建表，不实现 OAuth 路由 | 不建也不影响密码登录 |
| **announcements** | 无表；`instance_settings(public_announcement)` | 保留 key 约定即可 | 不实现 admin 公告接口；前端可先返回 `null` |
| **share** | `session_shares` | 建表，不实现公开分享路由 | 快照 JSON 形状由 `SessionShareCreateRequest` 决定 |
| **mobile login** | `mobile_login_tokens` | 可选建表 | 不实现扫码登录可整表跳过 |
| **email verification** | `email_verification_codes`、`email_verification_limits` | 可选建表 | 不实现邮件注册/重置可整表跳过；注意 `user_id` 为空串 |
| **legacy** | `legacy_import_archive`、`_deprecated_app_releases` | **不要建** | 纯历史遗留 |
| **notices / approvals** | 无表 | **不要建**（除非需要持久化） | 见 4.10 |
| **attachments** | 无表 | 继续文件系统即可；可选 `attachments` 表 | 见 4.11 |
| **terminals** | `connector_terminal_roots` | 建表 | 实时终端状态在内存，不落库 |

最小可用子集（如果第一版只做核心）：

```
users, connectors, connector_runtime_types, device_runtimes,
connector_runtime_catalogs, connector_protocol_capabilities,
projects, sessions, session_active_runs, timeline_items,
pairing_codes, instance_settings, schema_migrations
```

其余表可以先建好空表，等对应功能上线再写逻辑。

---

## 6. 迁移/兼容注意事项

### 6.1 从 PostgreSQL 导数据到 SQLite

如果要做一次性数据迁移（而不是纯空库）：

1. 先按本文件 4.x 建 SQLite 表，但把时间戳列临时建成 `TEXT`（避免一次性转换所有历史值）。
2. 导出时对 JSON 列原样导出（已经是 TEXT）。
3. 时间戳统一转换：
   - 方案 A：SQLite 列保持 `TEXT`，并在 Go 中解析 ISO-8601；
   - 方案 B：`strftime('%s', ts) * 1000` 转 Unix ms（注意 SQLite 的 `strftime` 对带 `Z` 的 ISO 字符串支持；最好在 Go 里解析，避免格式差异）。
   - `email_verification_codes.sent_at / expires_at / attempt_window / consumed_at` 在源里是 **Unix 秒**，转成毫秒时乘以 1000；不要和其它 ISO 字符串走同一套解析。
4. 处理 `sessions.seq_allocated_high`：如果放弃 Redis 租约，导入后直接令 `seq_allocated_high = seq`。
5. 处理 `_deprecated_app_releases` / `legacy_import_archive`：按需跳过。
6. 导入后执行：
```sql
PRAGMA foreign_key_check;
PRAGMA integrity_check;
```
7. 重建索引（导入前可以 `DROP INDEX`，导入后 `CREATE INDEX` 更快；但本 schema 索引不多，直接导入也可）。

### 6.2 从 Redis 租约模式退回纯 SQLite

如果 Go 版不保留 Redis：
- 删除 `session_revision_allocator.py` 对应的逻辑；
- `seq_allocated_high` 可以删除，或永远等于 `seq`；
- revision 不再有“租约空洞”，每次变更严格连续；
- 每 session 的 mutex 取代 `session-revision:<id>` Redis 锁；
- `BEGIN IMMEDIATE` 取代 `SELECT ... FOR UPDATE`。

如果保留 Redis（多实例）：
- SQLite 不适合多实例共享写，建议不要这样做；
- 若一定要多实例，SQLite 只能作为单实例的持久层，Redis 仍负责跨实例协调，但写入必须回到唯一 owner 进程。

### 6.3 客户端对 revision 的假设

- `updatedSeq` 是每个 session 单调递增的 revision，不保证全局连续（Redis 租约模式下可能有空洞）。
- 客户端增量拉取条件是 `updated_seq > after_seq`，所以空洞是安全的。
- `timeline_reset_seq` 是“完整快照重置”的 fence：小于等于它的旧缓冲写入必须丢弃。
- `last_read_seq`、`latest_turn_end_seq`、`timeline_reset_seq` 都是 session 级水位，不要跨 session 比较。

---

## 附录 A：已删除/历史表（Go 不建）

| 表 | 生命周期 | 用途 | 处理 |
|---|---|---|---|
| `approvals` | v2.0 → v2.3 删除 | 旧审批表，迁入 `notices` | 不建 |
| `notices` | v2.0 → v2.7 删除 | 旧持久化通知表 | 不建；见 4.10 可选表 |
| `session_states` | v2.5 → v2.6 删除 | 旧 runtime 状态投影 | 不建；状态在 `sessions` + 内存 |
| `session_message_queue` | v2.21 → v2.22 删除 | 每会话消息队列 | 不建 |
| `user_sidebar_orders` | v2.39 → v2.41 删除 | 侧边栏拖拽顺序 | 不建 |
| `platform_user_activity` | v2.0 → v2.42 删除 | 旧用户活跃表 | 不建；由 `usage_events` 取代 |
| `dashboard_daily_metrics` | v2.0 → v2.42 删除 | 旧 dashboard 日指标 | 不建 |
| `dashboard_user_daily_facts` | v2.0 → v2.42 删除 | 旧 dashboard 用户日事实 | 不建 |
| `dashboard_settings` | v2.0 → v2.42 删除 | 旧 dashboard 设置 | 不建 |
| `android_app_releases` | v2.15 → v2.16 删除 | 旧 Android 发布表 | 不建 |
| `app_releases` | v2.16 → v2.32 改名 | 应用发布表 | 已改名 `_deprecated_app_releases`，Go 不建 |
| `agent_efforts`、`agent_models`、`agent_modes`、`device_agent_settings`、`user_agent_defaults` | v1 → v2.3 删除 | v1 旧表，归档进 `legacy_import_archive` | 不建 |

## 附录 B：当前 revision 的关键事实

- `CURRENT_SCHEMA_REVISION = "v2_44"`，`CURRENT_SCHEMA_VERSION = "2.44"`。
- 迁移锁 ID：`0x414147454E545332`（`"AAGENTS2"` 的十六进制）。
- Bootstrap advisory xact lock：`4141454`。
- `session_timeline_lock_key = int.from_bytes(sha256(session_id)[:8], "big", signed=True)`。
- `PROTOCOL_MAX_REVISION = 2**53 - 1`。
- 协议 1.0 runtime 名：`codex`、`claude`、`opencode`、`acp`、`dsh`；2.0 可发现新 provider，DB 不限制。
- `sessions.seq` 的初始值：平台创建为 0；connector import 为 1。

## 附录 C：Go schema 与源 schema 的差异清单

| 项目 | 源 PostgreSQL | 推荐 Go/SQLite | 理由 |
|---|---|---|---|
| 时间戳 | ISO-8601 TEXT | INTEGER Unix ms | 定长、无格式歧义、索引小 |
| 邮件验证码时间 | BIGINT Unix 秒 | INTEGER Unix ms | 与新时间戳统一 |
| `connectors.user_id` | 无 FK | FK → `users(id)` RESTRICT | 防孤儿；不自动删设备，删除用户前需先清理 connector |
| `sessions.connector_id` | NO ACTION | NO ACTION（保持） | 与 `sessions.project_id RESTRICT` 叠加时，改成 CASCADE 可能因 SQLite 级联顺序触发约束失败；沿用源实现的显式删除顺序 |
| `connector_terminal_roots.session_id` | 无 FK | FK → `sessions(id)` CASCADE | 避免孤儿行 |
| `oauth_authorization_codes.client_id` | 无 FK | FK → `oauth_clients(id)` CASCADE | 避免孤儿授权码 |
| `pairing_codes.connector_id` | 无 FK | FK → `connectors(id)` CASCADE | 避免孤儿配对记录 |
| `external_session_id` 查找 | 无专用索引 | 部分索引 `(connector_id, runtime_id, external_session_id)` | 高频查找 |
| `connectors.user_id` 查找 | 无索引 | `(user_id, revoked)` | 高频查找 |
| `pairing_codes` 清理 | 无索引 | `(status, expires_at)` | 过期清理 |
| `sessions.seq_allocated_high` | 有，Redis 租约 | 可选；纯 SQLite 可删或等于 `seq` | 单写不需要区间租约 |
| `sessions.takeover` / `seq` / `updated_seq` | 无 DB 默认，应用必填 | `DEFAULT 0` | 简化插入；语义不变 |
| `timeline_items.item_time` 索引 | `coalesce(item_time,'') DESC` | `coalesce(item_time, 0) DESC` | 配合 INTEGER 时间戳 |
| `AUTOINCREMENT` | 无 | 不使用 | 所有 PK 应用生成 |
| 枚举 | 应用层 Literal | TEXT + 可选 CHECK | 契约进 schema，但保留扩展性 |
| JSON | TEXT | TEXT（可选 `json_valid` CHECK） | 与源一致，无需 JSONB |

---

## 参考文件（绝对路径）

- `D:/Develop/PyCharmProject/Agents-Anywhere-main/server/agent_server/infra/db/schema.py`
- `D:/Develop/PyCharmProject/Agents-Anywhere-main/server/agent_server/infra/db/migrations.py`
- `D:/Develop/PyCharmProject/Agents-Anywhere-main/server/agent_server/infra/db/engine.py`
- `D:/Develop/PyCharmProject/Agents-Anywhere-main/server/migrations/schema_v2_0.py`
- `D:/Develop/PyCharmProject/Agents-Anywhere-main/server/migrations/versions/`（`v2_0.py` … `v2_44.py`）
- `D:/Develop/PyCharmProject/Agents-Anywhere-main/server/agent_server/core/models.py`
- `D:/Develop/PyCharmProject/Agents-Anywhere-main/server/agent_server/core/usage.py`
- `D:/Develop/PyCharmProject/Agents-Anywhere-main/server/agent_server/core/utc.py`
- `D:/Develop/PyCharmProject/Agents-Anywhere-main/server/agent_server/infra/repositories/timeline.py`
- `D:/Develop/PyCharmProject/Agents-Anywhere-main/server/agent_server/infra/timeline_store.py`
- `D:/Develop/PyCharmProject/Agents-Anywhere-main/server/agent_server/services/session_revision_allocator.py`
- `D:/Develop/PyCharmProject/Agents-Anywhere-main/server/agent_server/infra/repositories/email_accounts.py`
- `D:/Develop/PyCharmProject/Agents-Anywhere-main/server/agent_server/infra/repositories/store_support.py`
- `D:/Develop/PyCharmProject/Agents-Anywhere-main/server/agent_server/infra/repositories/attachments.py`
- `D:/Develop/PyCharmProject/Agents-Anywhere-main/server/agent_server/infra/files/local.py`
- `D:/Develop/PyCharmProject/Agents-Anywhere-main/server/agent_server/services/attachments.py`
- `D:/Develop/PyCharmProject/Agents-Anywhere-main/server/agent_server/services/announcements.py`
