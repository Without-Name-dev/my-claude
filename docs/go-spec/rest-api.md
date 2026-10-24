# Agents Anywhere Python Server → Go REST API 规格

> 本次提取覆盖了 **142/142** 条路由（139 条由 `APIRouter` 注册 + 3 条 `app.py` 内直接注册的 health 路由），以下 **0** 条未覆盖。
>
> 另有 2 条静态站点回退路由（`GET /`、`GET /{path:path}`，仅当环境变量 `AGENT_SERVER_STATIC_DIR` 设置时才注册），以及 `/_next`、`/assets`、`/brand` 三个静态目录挂载，它们不是产品 API，未计入 142 条。
>
> **提取期间源码发生并发变更**：`app.py`（17:45）、`deps.py`（17:45）、`services/connector_realtime.py`（17:45）、`core/models.py`（17:45）、`api/connector_terminal.py`（17:46）在本次任务进行中被外部修改；`api/connector_shell.py` 与 `services/shell_tasks.py` 被删除。本规格以 **2026-10-24 17:46 之后**的文件状态为准，并记录了 Web 客户端仍在调用但服务端已移除的 4 个 terminals-v2 路由（见 18.1）。
>
> 提取范围：`server/agent_server/app.py`、`deps.py`、`core/{models,auth,protocol,events,device_runtime,announcement,api_namespace}.py`、`api/` 全部模块、`services/{session_run,event_recovery,attachments,connector_files,terminal,terminal_relay,workspace,connector_presence,session_meta_projection,announcements,usage_report,device_runtimes}.py`、`infra/{ws_tickets,fs_downloads,connector_rpc}.py`、`infra/repositories/{sessions,connectors,projects}.py`。客户端调用面通过 `web-next/src`、`android/`、`ios/` 的 API 层交叉核对。
>
> 凡是从代码无法确定的，均显式标注 `UNKNOWN` 并说明检查位置。

---

## 0. 全局约定

### 0.1 前缀与挂载点

- 唯一全局前缀：`/api/v2`（`core/api_namespace.py:4` 的 `API_V2_PREFIX`）。
- **没有第二个 API 挂载点**。`app.py:358-379` 中所有 router 都只以 `prefix=API_V2_PREFIX` 挂载一次。
- 客户端（Web/Android/iOS）和 connector 都使用 `/api/v2`；`docs/api/namespace.md` 也明确要求不能把 `/api/v2` 写进 `serverUrl`。
- Web 静态站点根路径 `/`、locale 路径、`/_next/*` 等不属于 API。
- FastAPI 默认 `redirect_slashes=True`：例如 `GET /api/v2/sessions/` 会 307 到 `/api/v2/sessions`。

### 0.2 鉴权方式

| 名称 | Header / 参数 | 说明 |
|---|---|---|
| user token | `Authorization: Bearer <JWT>` | `core/auth.py:108-113` 生成，HS256 HMAC，`sub`=userId，默认 7 天；`deps.py:151-183` 校验。 |
| user token（查询参数） | `?token=<JWT>` | 仅用于附件 `open`、终端 WS、`/sessions/local/...` 等无法设置 Header 的场景。 |
| connector token（注册/换票） | `Authorization: Connector <connectorId>:<connectorToken>` | 仅 `POST /api/v2/connector/auth`（`api/connector_ingress.py:944-954`）。 |
| connector access token | `Authorization: Bearer <connectorId>.<exp>.<sig>` | connector 专用短期 token（900 秒，`core/auth.py:12,21-56`）。 |
| signed token | query/body 字段 | `create_signed_token`/`verify_signed_token`，HMAC-SHA256；用于 `ws-ticket`、fs preview、本地附件、移动登录。 |
| 无鉴权 | — | 见各端点标注。 |

`current_user_id` 只解码 JWT，不做 DB 查询；`current_user` 额外查库并拒绝已删除（401）或 `disabled`（403）账号。`require_admin` 再要求 `role == "admin"`（403）。

### 0.3 Content-Type

- 绝大多数请求/响应：`application/json`。
- `POST /api/v2/sessions/{id}/attachments`：`multipart/form-data`，字段名 `files`（可多个）。
- `POST /api/v2/oauth/token`：`application/x-www-form-urlencoded`（FastAPI `Form(...)`）。
- 文件下载端点返回二进制流。

### 0.4 错误响应结构

- `HTTPException(detail="文本")` → `{"detail":"文本"}`。
- `HTTPException(detail={...})` → `{"detail":{...}}`，常见对象为 `{"code":"...","message":"..."}`。
- 请求体/参数校验失败（Pydantic / FastAPI）→ HTTP 422，默认结构：
  ```json
  {"detail":[{"type":"string_type","loc":["body","field"],"msg":"...","input":"..."}]}
  ```
  （不同 Pydantic 版本可能附带 `ctx`/`url`。）
- Redis 协调层不可用 → HTTP 503，`{"detail":{"code":"coordination_unavailable","message":"Redis coordination is unavailable"}}`（`app.py:296-309`）。
- Connector 服务异常全局映射（`api/error_handlers.py:19-31`）：
  - `ConnectorUnavailableError` → 409
  - `ConnectorRequestTimeoutError` → 504
  - `ConnectorUpstreamError` / `ConnectorProtocolError` → 502
  - 其它 → 500
  - body 均为 `{"detail": <detail>}`。
- Workspace 异常 → 404（session not found）或 409（`api/error_handlers.py:34-39`）。

### 0.5 时间、数值与命名

- 所有服务端生成的时间字段都是 **ISO-8601 UTC 字符串**，形如 `2026-10-24T09:30:00.123456Z`（`core/utc.py:6-7`）。HTTP JSON 中**没有 epoch 时间戳**。
- 相对时长用整数秒：`expiresIn`、`expires_in`、`retryAfter`、`uptimeSeconds`。
- `sequence`、`updatedSeq`、`orderSeq`、`revision` 等为整数；协议上限 `9007199254740991`。
- 线上字段名：除下面列出的例外，**全部是 camelCase**。
  - 例外 1：`OAuthTokenResponse` 与 `OAuthMetadataResponse` 全部 snake_case（`access_token`、`token_type`、`expires_in`、`refresh_token`、`authorization_endpoint`、`token_endpoint`、`response_types_supported` 等）。
  - 例外 2：`GET/POST /oauth/authorize` 的 query/body 字段是 snake_case（`response_type`、`client_id`、`redirect_uri`、`code_challenge`、`code_challenge_method`）。
  - 例外 3：`NoticeActionInput` 的 Pydantic 字段名是 `schema_`，**alias 为 `schema`**；FastAPI 默认 `response_model_by_alias=True`，线上实际字段是 `schema`。
  - 例外 4：`RuntimeTypeView` / `DeviceRuntimeView` 的 `schema_` 同样以 `schema` 输出。
- 所有 Pydantic 响应模型**默认包含所有声明字段**（`response_model_exclude_none` 未开启）：可空字段在 JSON 里是 `null`，不是省略。个别透传对象（`connectorResult`、`result`）保留 connector 发来的原始结构。

### 0.6 分页与游标

只有两个列表端点做游标分页：

1. `GET /api/v2/sessions`
2. `GET /api/v2/projects/{project_id}/sessions`

参数：`limit`（默认 100，范围 1..100），`cursor`（可选，非空字符串）。

游标格式（`infra/repositories/sessions.py:101-151`）：
- `base64url(JSON)`，去掉 `=` padding；
- JSON：`{"v":1,"p":<0|1 pinned>,"s":"<sortAt>","o":<latestItemOrderSeq 或 -1>,"u":<updatedSeq>,"i":"<sessionId>"}`；
- 服务端不透明处理；非法游标 → 400 `{"detail":"invalid session cursor"}`。

注意：`GET /api/v2/sessions/list` 返回**全量 inventory，不分页**。管理员用户列表使用 `limit`/`offset`，不是游标。

### 0.7 状态码约定

- 200：成功（含 `POST /sessions`、`POST /sessions/create-and-start`）。
- 201：`POST /admin/users`、`POST /connectors/{id}/runtimes`、`POST /sessions/{id}/shares`。
- **202：不存在。**
- 204 空体：`POST /auth/change-password`、`DELETE /connectors/{id}`、`DELETE /admin/users/{user_id}`。
- 302：`GET /sessions/{id}/attachments/{file_id}/open`。
- 307：OAuth 的 `GET /oauth/authorize`、`GET /auth/oauth/callback`（`RedirectResponse` 默认状态）。
- 400：非法分页游标、非法/已用的 preview token。
- 401 / 403 / 404 / 409 / 413 / 422 / 429 / 500 / 502 / 503 / 504：见各端点。

---

## 1. 共享响应模型

这些模型被多个端点复用。字段顺序即 `core/models.py` / `core/protocol.py` 中声明顺序。

### 1.1 `ConnectorView`（`core/models.py:93-102`）

| 字段 | 类型 | 可空 | 说明 |
|---|---|---|---|
| `id` | string | 否 | `conn_...` |
| `userId` | string | 否 | 所有者 |
| `name` | string | 否 | |
| `connectorKind` | `"desktop" \| "cli"` | 否 | |
| `deviceOs` | `"macos" \| "windows" \| "linux"` | **是** | connector WS 上报前为 null |
| `status` | `"offline" \| "online"` | 否 | 响应中按 RPC presence 实时重算 |
| `lastSeenAt` | string | **是** | ISO |
| `createdAt` | string | 否 | ISO |
| `updatedAt` | string | 否 | ISO |

### 1.2 `SessionView`（`core/models.py:800-851`）

| 字段 | 类型 | 可空 | 说明 |
|---|---|---|---|
| `id` | string | 否 | |
| `connectorId` | string | 否 | |
| `projectId` | string | **是** | |
| `connectorStatus` | `"offline" \| "online"` | 否 | 实时重算 |
| `runtime` | string | 否 | 规范化后的 runtime type |
| `runtimeId` | string | 否 | validator 保证非空；缺省等于 runtime |
| `runtimeType` | string | **是**（validator 后实际非空） | 等于 `runtime` |
| `runtimeName` | string | **是** | |
| `runtimeTypeDisplayName` | string | **是** | |
| `externalSessionId` | string | **是** | |
| `title` | string | **是** | |
| `cwd` | string | **是** | |
| `status` | `SessionStatus` | 否 | `idle/waiting/pending/running/stopping/waiting_approval/error/blocked` |
| `takeover` | bool | 否 | |
| `pinned` | bool | 否 | 默认 false |
| `pinnedAt` | string | **是** | |
| `archived` | bool | 否 | |
| `archivedAt` | string | **是** | |
| `userArchived` | bool | 否 | |
| `sourceAvailability` | `available/archived/unavailable/deleted/missing/unknown` | 否 | |
| `sourceAvailabilityReason` | string | **是** | |
| `sourceAvailabilityUpdatedAt` | string | **是** | |
| `sourceObservationOrigin` | `event/inventory/operation` | **是** | |
| `archiveSource` | `user/runtime/both` | **是** | |
| `unread` | bool | 否 | |
| `lastReadSeq` | int | 否 | |
| `latestTurnEndSeq` | int | 否 | |
| `lastSyncedAt` | string | **是** | |
| `sourceObservedAt` | string | **是** | |
| `lastActivityAt` | string | **是** | |
| `lastItemAt` | string | **是** | |
| `lastItemOrderSeq` | int | **是** | |
| `sortAt` | string | **是** | |
| `updatedSeq` | int | 否 | 会话级修订号 |

### 1.3 `SessionRuntimeState`（`core/models.py:871-894`）

| 字段 | 类型 | 可空 |
|---|---|---|
| `sessionId` | string | 否 |
| `runtime` | string | 否 |
| `runtimeId` | string | 否（validator 后非空） |
| `runtimeType` | string | 是（validator 后非空） |
| `externalSessionId` | string | 是 |
| `status` | `SessionStatus` | 否，默认 `idle` |
| `selections` | `{string: string \| null}` | 否，默认 `{}` |
| `statusReason` | string | 是 |
| `error` | object | 是 |
| `metadata` | object | 否，默认 `{}` |
| `updatedSeq` | int | 否 |
| `createdAt` | string | 否 |
| `updatedAt` | string | 否 |

### 1.4 `TimelineItem`（`core/models.py:989-1018`）

| 字段 | 类型 | 可空 |
|---|---|---|
| `id` | string | 否 |
| `sessionId` | string | 否 |
| `type` | `message/tool/artifact/marker/system` | 否 |
| `status` | `pending/running/waiting_approval/done/failed/cancelled/interrupted` | 否 |
| `role` | `user/assistant/system/tool` | 是 |
| `content` | any | 否，默认 `{}` |
| `source` | object：`runtime`(string)、`sessionId`、`itemId`、`itemType`、`event`、`derivedKey`、`clientMessageId` 均可空 | 否 |
| `orderSeq` | int | 否 |
| `revision` | int | 否，默认 1 |
| `contentHash` | string | 否 |
| `createdAt` | string | 否（`TimelineItem` 覆盖为非空） |
| `updatedAt` | string | 否 |
| `completedAt` | string | 是 |
| `updatedSeq` | int | 否 |

### 1.5 `NoticeIn`（`core/models.py:1103-1127`）

`noticeId`(str, 非空)、`type`(`notification/interaction`)、`sessionId`(str)、`source`(object；`runtime`、`component`、`approvalId`、`timelineItemId`、`operationId` 均可空)、`title`(str)、`message`(str|null)、`severity`(`info/success/warning/error`，默认 `info`)、`status`(`open/responding/response_accepted/resolving/resolved/closed/expired/cancelled/failed`，默认 `open`)、`interactionType`(`approval/execution_error/confirmation/input_request/unknown`|null)、`blocking`(`{scope:"session",targetId}`|null)、`responseRequired`(bool，默认 false)、`actions`(array，默认 `[]`)、`context`(object，默认 `{}`)、`metadata`(object，默认 `{}`)、`expiresAt`(str|null)、`revision`(int，默认 1)、`createdAt`(str|null)、`resolvedAt`(str|null)。

`actions[]` 元素：`actionId`(str)、`label`(str)、`style`(`primary/secondary/danger`，默认 `secondary`)、`input`=`{required:bool=false, schema:object|null, uiSchema:object|null}`（线上字段名 `schema`）。

`Notice`（`models.py:1124`）额外含 `updatedSeq:int`、`createdAt:str`（非空）、`updatedAt:str`。

### 1.6 `ProjectView`（`core/models.py:143-158`）

`id`、`userId`、`connectorId`、`name`、`workspacePath`(str)、`manuallyCreated`(bool=false)、`pinned`(bool=false)、`pinnedAt`(str|null)、`activeSessionCount`(int=0)、`sidebarSessionCounts`=`{active:int=0,archived:int=0}`、`lastActivityAt`(str|null)、`createdAt`、`updatedAt`。

### 1.7 `TerminalView`（`core/models.py:1299-1318`）

`terminalId`、`sessionId`、`label`、`root`、`cwd`、`cols`(int)、`rows`(int)、`purpose`(仅 `"user"`)、`pid`(int|null)、`status`(`starting/running/exited`，默认 `running`)、`exitCode`(int|null)、`scrollbackBytes`(int=0)、`scrollbackSeq`(int=0)、`ephemeralGroupId`(str|null)、`persistent`(bool=false)、`createdAt`(str)。

### 1.8 `DeviceRuntimeView`（`core/device_runtime.py:161-190`）

`connectorId`、`runtimeId`、`runtimeType`、`name`、`displayName`（兼容别名，与 name 同值）、`typeDisplayName`、`present`(bool)、`available`(bool)、`reason`(str|null)、`configured`(bool)、`active`(bool)、`status`(`stopped/discovering/available/unavailable/validating/starting/running/stopping/error/unknown`)、`discovery`(object)、`metadata`(object={})、`schema`(object|null，alias)、`uiSchema`(object)、`defaults`(object)、`capabilities`(object)、`config`(object|null)、`error`(object|null)、`phase`(object|null)、`lastDiscoveredAt`、`createdAt`、`updatedAt`。

### 1.9 `RuntimeTypeView`（`core/device_runtime.py:128-152`）

`connectorId`、`runtimeType`、`implementationType`(str|null)、`displayName`、`description`(str|null)、`present`(bool)、`available`(bool)、`reason`(str|null)、`recommended`(bool)、`recommendationRank`(int|null)、`discovery`(object)、`configSchema`(object|null)、`schema`(object|null)、`uiSchema`(object)、`defaults`(object)、`capabilities`(object)、`metadata`(object)、`instancePolicy`(`single|multiple`)、`maxInstances`(int|null)、`lastDiscoveredAt`、`createdAt`、`updatedAt`。

### 1.10 `ProtocolCapabilitySet`（`core/protocol.py:116-137`）

`revision`(int 0..9007199254740991)、`capabilities[]`：
`capabilityId`、`version`(默认 "1")、`scope`(`runtime|session`)、`runtime`(str|null)、`sessionId`(str|null)、`supported`(bool=true)、`available`(bool=true)、`allowed`(bool=true)、`unavailableReason`(str|null)、`parameters`(object={})。
模型 `extra="allow"`，connector 额外下发的 `runtimeId` 等字段会原样保留。

### 1.11 `RpcResponsePayload`（`core/models.py:1366-1369`）

`ok`(bool)、`result`(any，可为 null)、`error`(`{code:string,message:string}`|null)。**成功响应也会输出 `"error": null`**。

### 1.12 其它通用响应

- `BulkArchiveResponse`（`models.py:921-924`）：`sessions: SessionView[]`、`notFound: string[]`、`serverTime`。
- `ArchiveAllResponse`（`models.py:935-938`）：`sessions: SessionView[]`、`affected: int`、`serverTime`。
- `ProjectSessionListResponse`（`models.py:941-945`）：`sessions`、`hasMore: bool`、`nextCursor: string|null`、`serverTime`。
- `SessionResponse`（`models.py:948-950`）：`session: SessionView`、`serverTime`。
- `ProtocolTimelineResponse`（`protocol.py:197-202`）：`sessionId`、`items: TimelineItem[]`、`nextSeq:int`、`hasMore:bool`、`serverTime`。
- `ProtocolTimelineSnapshot`（`protocol.py:191-194`）：`items`、`nextSeq:int`、`hasMore:bool`。
- `ProtocolSessionSnapshotResponse`（`protocol.py:205-215`）：`session: SessionView`、`state: SessionRuntimeState|null`、`timeline: ProtocolTimelineSnapshot`、`approvals: Approval[]`（当前恒为 `[]`）、`notices: NoticeIn[]`、`effectiveCapabilities: ProtocolCapabilitySet`、`runtimeCapabilities: ProtocolCapabilitySet`、`catalogs: {model?:object, permission?:object}`（缺失时 key 被省略，不是 null）、`eventCursor: "seq:<n>"`、`serverTime`。
- `ProtocolEventEnvelope`（`protocol.py:241-249`）：`protocolVersion:"1.0"`、`eventId`、`sequence:int`、`cursor:"seq:<n>"`、`type:string`、`sessionId:string`、`emittedAt`、`payload:object`。
- `ProtocolEventRecoveryResponse`（`protocol.py:252-256`）：`events: ProtocolEventEnvelope[]`、`nextCursor`、`snapshotRequired:bool`、`serverTime`。

---

## 2. 认证 `/auth/*`

实现：`server/agent_server/api/auth.py`。所有路径再加 `/api/v2` 前缀。模型在 `core/models.py:210-319, 527-668`。

### 2.1 `GET /api/v2/auth/config`

- 鉴权：无。
- Query：无。
- 响应 `AuthConfigResponse`（`models.py:265-279`）：
  `emailVerificationRequired:bool`、`needsBootstrap:bool`、`registrationOpen:bool`、`oauthRegistrationOpen:bool`、`oauthEnabled:bool`、`oauthProviderLabel:string|null`、`passwordResetEnabled:bool`、`setupTokenExpiresAt:string|null`（仅 `needsBootstrap=true` 时有值）、`serverTime`。
- 错误：500（DB/Redis 异常）。
- 实现：`api/auth.py:81-111`。

### 2.2 `POST /api/v2/auth/password-salt`

- 鉴权：无。
- Body `AuthPasswordSaltRequest`：`email:string`（必填，≤320）。
- 响应 `AuthPasswordSaltResponse`：`salt:string`、`serverTime`。未知邮箱也返回随机 salt（防枚举）。
- 错误：422（邮箱格式）、500。
- 实现：`api/auth.py:114-123`。

### 2.3 `POST /api/v2/auth/register`

- 鉴权：无；首次引导需要 `setupToken`。
- Body `AuthRequest`（`models.py:210-219`）：
  - `email:string` 必填 ≤320；
  - `displayName:string|null` ≤64；
  - `code:string|null` ≤6（邮箱验证码）；
  - `password:string|null`；
  - `passwordVerifier:string|null`；
  - `passwordSalt:string|null`（提供 verifier 时必填）；
  - `setupToken:string|null`（仅首个用户引导时必填）。
- 行为：无用户时创建 admin 并消耗 setup token；否则要求 `registrationOpen`，创建 member。
- 响应 `AuthResponse`（`models.py:243-251`）：`userId`、`email:string|null`、`emailVerified:bool`、`displayName:string`、`role:"admin"|"member"`、`accessToken:string`、`tokenType:"bearer"`、`serverTime`。
- 错误：401（setup token 错/过期）、403（注册关闭）、409（用户已存在）、422、429（邮箱验证码限流，`Retry-After: 60`）。
- 实现：`api/auth.py:126-179`。

### 2.4 `POST /api/v2/auth/login`

- 鉴权：无。
- Body `AuthRequest`（只用 `email`、`password`、`passwordVerifier`）。
- 响应 `AuthResponse`（同上）。
- 错误：401 `{"detail":"invalid credentials"}`、422、500。
- 实现：`api/auth.py:182-192`。

### 2.5 `POST /api/v2/auth/email-code`

- 鉴权：可选 Bearer（`purpose=bind` 时必须）。
- Body `EmailCodeRequest`（`models.py:282-286`）：`email:string` 必填 ≤320；`purpose:"register"|"bind"|"reset"` 必填；`setupToken:string|null`；`pendingToken:string|null`（OAuth 注册场景）。
- 响应 `EmailCodeResponse`：`expiresIn:int=600`、`retryAfter:int=60`、`serverTime`。
- 错误：401（bind 未登录）、403（注册关闭）、409（密码重置关闭/邮件未启用）、422、429（`Retry-After: 60`）、502（邮件发送失败）。
- 实现：`api/auth.py:195-237`。

### 2.6 `PUT /api/v2/auth/me/email`

- 鉴权：Bearer user token（`current_user`，DB 校验）。
- Body `BindEmailRequest`：`email:string` 必填 ≤320；`code:string|null` ≤6。
- 响应 `AuthMeResponse`。
- 错误：401、403（账号禁用）、409（邮箱占用）、422。
- 实现：`api/auth.py:240-251`。

### 2.7 `PUT /api/v2/auth/me/profile`

- 鉴权：Bearer user token。
- Body `UpdateProfileRequest`：`displayName:string` 必填，长度 1..64（会 strip）。
- 响应 `AuthMeResponse`：`userId`、`email:string|null`、`emailVerified:bool`、`displayName:string`、`role`、`disabled:bool`、`avatar:string|null`、`serverTime`。
- 错误：401、403、422。
- 实现：`api/auth.py:254-262`。

### 2.8 `GET /api/v2/auth/me`

- 鉴权：Bearer user token。
- 响应 `AuthMeResponse`。
- 错误：401（无效/用户不存在）、403（禁用）。
- 实现：`api/auth.py:385-387`。

### 2.9 `PUT /api/v2/auth/me/avatar`

- 鉴权：Bearer user token。
- Body `UpdateAvatarRequest`：`avatar:string` 必填，必须匹配 `^data:image/(png|jpeg|jpg|webp|gif);base64,[A-Za-z0-9+/=]+$`，总长 ≤262144 字节。
- 响应 `AuthMeResponse`（`avatar` 为 data URL）。
- 错误：413（超过 256KB）、422（格式非法）、401/403。
- 实现：`api/auth.py:396-411`。

### 2.10 `DELETE /api/v2/auth/me/avatar`

- 鉴权：Bearer user token。
- Body：无。
- 响应 `AuthMeResponse`，`avatar:null`。
- 错误：401/403。
- 实现：`api/auth.py:414-420`。

### 2.11 `POST /api/v2/auth/change-password`

- 鉴权：两种模式：
  1. Bearer user token（改自己的密码，不要求旧密码）；
  2. 无 Bearer 且 body 有 `email`+`code`（忘记密码重置，要求实例开启 password reset 且邮件启用）。
- Body `ChangePasswordRequest`（`models.py:304-313`）：`oldPassword:string|null`、`oldPasswordVerifier:string|null`、`newPassword:string|null`、`newPasswordVerifier:string|null`、`newPasswordSalt:string|null`、`email:string|null` ≤320、`code:string|null` ≤6。
- 成功：**204，空体**。
- 错误：401、403、409（重置关闭）、422（缺少新密码或 salt / 验证码错误）。
- 实现：`api/auth.py:423-449`；重置分支 `api/auth.py:578-594`。

### 2.12 `POST /api/v2/auth/mobile-login/qr`

- 鉴权：Bearer user token（Web 端为手机登录生成二维码）。
- Body：无（客户端传 `{}`）。
- 响应 `MobileLoginQrCreateResponse`：`userId:string`、`loginToken:string`（signed，120 秒）、`expiresAt:string`、`serverTime`。
- 错误：401/403。
- 实现：`api/auth.py:452-478`；常量 `auth.py:75-78`。

### 2.13 `POST /api/v2/auth/mobile-login/request`

- 鉴权：无（signed `loginToken` 自证）。
- Body `MobileLoginRequestRequest`：`userId:string` 必填、`loginToken:string` 必填、`deviceName:string|null`。
- 响应 `MobileLoginStatusResponse`（`models.py:640-647`）：`status` ∈ `pending_scan|pending_web_confirm|approved|rejected|expired|consumed`；`userId/deviceName/expiresAt/requestedAt/approvedAt` 均可空；`serverTime`。
- 错误：401（token 无效/过期/sub 不匹配）。
- 实现：`api/auth.py:534-549`。

### 2.14 `POST /api/v2/auth/mobile-login/status`

- 鉴权：无。
- Body `MobileLoginStatusRequest`：`loginToken:string` 必填。
- 响应 `MobileLoginStatusResponse`。
- 错误：404 `{"detail":"mobile login token not found"}`。
- 实现：`api/auth.py:552-560`。

### 2.15 `POST /api/v2/auth/mobile-login/confirm`

- 鉴权：Bearer user token。
- Body `MobileLoginConfirmRequest`：`loginToken:string` 必填、`approved:bool=true`。
- 响应 `MobileLoginStatusResponse`。
- 错误：401（token 无效/sub 不匹配）、403。
- 实现：`api/auth.py:563-575`。

### 2.16 `POST /api/v2/auth/mobile-login/exchange`

- 鉴权：无（signed login token）。
- Body `MobileLoginExchangeRequest`：`userId:string`、`loginToken:string` 均必填。
- 响应 `MobileLoginExchangeResponse`：`auth: AuthResponse`、`refreshToken:string`（30 天 signed）、`expiresAt:string`、`serverTime`。
- 错误：401（无效/过期/已消费）、403（账号禁用）。
- 实现：`api/auth.py:481-511`。

### 2.17 `POST /api/v2/auth/mobile-login/refresh`

- 鉴权：无（signed refresh token）。
- Body `MobileLoginRefreshRequest`：`refreshToken:string` 必填。
- 响应 `AuthResponse`。
- 错误：401（无效/过期）、403（禁用）。
- 实现：`api/auth.py:514-531`。

### 2.18 `GET /api/v2/auth/oauth/start`

- 鉴权：无。
- Query：`returnTo:string|null`（用于推导回调 origin 和前端回跳）。
- 响应 `OAuthStartResponse`：`authorizeUrl:string`、`serverTime`。
- 错误：404（OAuth 未配置/配置错误）。
- 实现：`api/auth.py:265-280`。

### 2.19 `GET /api/v2/auth/oauth/callback`

- 鉴权：无。
- Query：`code:string|null`、`state:string|null`、`error:string|null`。
- 响应：307 重定向到前端 `returnTo`（或站点根），query 追加上：
  - 失败：`oauth_error=<message>`；
  - 已绑定：`oauth_status=authenticated`、`oauth_pending=<pendingToken>`、`oauth_email`、`oauth_display_name`；
  - 新用户/需绑定：`oauth_status=needs_password|needs_registration`、`oauth_pending`、`oauth_email`、`oauth_display_name`。
- 实现：`api/auth.py:283-339`。

### 2.20 `POST /api/v2/auth/oauth/finalize`

- 鉴权：无。
- Body `OAuthFinalizeRequest`：`pendingToken:string` 必填；`email:string|null` ≤320；`displayName:string|null` ≤64；`code:string|null` ≤6；`password:string|null`；`passwordVerifier:string|null`；`passwordSalt:string|null`；`setPassword:bool=false`。
- 响应 `OAuthFinalizeResponse`：`auth: AuthResponse`、`serverTime`。
- 错误：401（pending token 过期/密码错误）、403（OAuth 注册关闭）、409、422。
- 实现：`api/auth.py:342-382`。

---

## 3. OAuth 服务器 `/oauth/*` 与 `/.well-known/*`

实现：`server/agent_server/api/oauth.py`。

### 3.1 `GET /api/v2/.well-known/oauth-authorization-server`

- 鉴权：无。
- 响应 `OAuthMetadataResponse`（snake_case）：`issuer`、`authorization_endpoint`、`token_endpoint`、`response_types_supported:["code"]`、`grant_types_supported:["authorization_code"]`、`code_challenge_methods_supported:["S256"]`。
- 实现：`api/oauth.py:26-36`。

### 3.2 `GET /api/v2/oauth/authorize`

- 鉴权：Bearer user token（注意：即使是 GET 重定向端点，也经 `current_user` 依赖，浏览器直开需带 Header）。
- Query（snake_case，全部必填除非标注）：`response_type`、`client_id`、`redirect_uri`、`code_challenge`、`code_challenge_method`(默认 `S256`)、`scope`(默认 "")、`state`(可选)。
- 响应：307 重定向到 `redirect_uri?code=<code>&state=<state>`。
- 错误：401/403（用户）、404（OAuth client 不存在）、422（`response_type != code`、redirect_uri 不允许）。
- 实现：`api/oauth.py:39-62,85-120`。

### 3.3 `POST /api/v2/oauth/authorize`

- 鉴权：Bearer user token。
- Body `OAuthAuthorizeRequest`（snake_case）：`response_type`、`client_id`、`redirect_uri`、`code_challenge` 必填；`code_challenge_method` 默认 `S256`；`scope` 默认 ""；`state:string|null`。
- 响应 `OAuthAuthorizeResponse`：`redirectUrl:string`、`serverTime`。
- 错误：同 3.2。
- 实现：`api/oauth.py:65-82`。

### 3.4 `POST /api/v2/oauth/token`

- 鉴权：无。
- Content-Type：`application/x-www-form-urlencoded`。
- Form 字段（全部必填）：`grant_type`、`code`、`client_id`、`redirect_uri`、`code_verifier`。
- 响应 `OAuthTokenResponse`（snake_case）：`access_token`、`token_type:"Bearer"`、`expires_in:int`（604800）、`scope:string`、`refresh_token:null`（当前恒为 null）。
- 错误：400（code/verifier 无效）、404（client）、422（grant_type 或 redirect_uri 非法）。
- 实现：`api/oauth.py:123-152`。

---

## 4. 配对 `/pairing/*`

实现：`server/agent_server/api/pairing.py`；模型 `core/models.py:705-737`。

### 4.1 `POST /api/v2/pairing/start`

- 鉴权：无。
- Body `PairingStartRequest`：`serverUrl:string|null`；`ttlSeconds:int=600`（无范围校验）。
- 响应 `PairingStartResponse`：`pairingId:"pair_..."`、`code:"000000"` 六位数字、`expiresAt:string`、`serverTime`。
- 错误：500。
- 实现：`api/pairing.py:21-29`；`infra/repositories/connectors.py:265-287`。

### 4.2 `POST /api/v2/pairing/claim`

- 鉴权：Bearer user token。
- Body `PairingClaimRequest`：`code:string` 必填；`name:string="Codex Connector"`；`serverUrl:string|null`；`connectorId:string|null`；`connectorToken:string|null`（后两者必须同时提供）。
- 响应 `PairingClaimResponse`：`status:"claimed"`、`connector: ConnectorView`（当前实现非 null）。
- 错误：404（code 不存在）、409（已过期/已领取/凭证无效）。
- 实现：`api/pairing.py:32-52`；`infra/repositories/connectors.py:290-346`。

### 4.3 `POST /api/v2/pairing/poll`

- 鉴权：无。
- Body `PairingPollRequest`：`pairingId:string` 必填。
- 响应 `PairingPollResponse`：`status:string`（`pending|claimed|expired|consumed`）、`config:{serverUrl,connectorId,connectorToken}|null`（仅首次 `claimed` 时返回，之后 status 变 `consumed` 且 config 为 null）、`expiresAt:string|null`。
- 错误：404（pairingId 不存在）、409（claimed 但缺少 config）。
- 实现：`api/pairing.py:55-62`；`infra/repositories/connectors.py:348-384`。

---

## 5. 客户端 WebSocket 票据 `/ws-ticket`

### 5.1 `POST /api/v2/ws-ticket`

- 鉴权：Bearer user token。
- Body `ProtocolWsTicketRequest`（`core/protocol.py:218-238`）：
  - `clientId:string` 必填；
  - `scope` 必填，且 `{sessionId?:string, dashboard?:bool}` 中**恰好一个**（否则 Pydantic 422）。
- 响应 `ProtocolWsTicketResponse`：`ticket:"wst_..."`、`expiresAt:string`（60 秒）、`serverTime`。
- 行为：`scope.dashboard=true` 时 scope 为 `"dashboard"`；否则校验 sessionId 归属，scope 为 sessionId。
- 错误：401、403、404（session 不存在）、422。
- 实现：`api/client_ws.py:21-43`；票 TTL `infra/ws_tickets.py:31`。

---

## 6. Connector 管理 `/connectors/*`

实现：`server/agent_server/api/connectors.py`。另有 runtimes / files / terminal / shell 子模块分别在后文。

### 6.1 `GET /api/v2/connectors`

- 鉴权：Bearer user token。
- Query：无。
- 响应 `ConnectorListResponse`：`connectors: ConnectorView[]`（仅未撤销，按 `updatedAt DESC, createdAt DESC`）、`serverTime`。`status` 按实时 RPC presence 计算。
- 错误：401/403/500。
- 实现：`api/connectors.py:46-56`。

### 6.2 `POST /api/v2/connectors`

- 鉴权：Bearer user token。
- Body `ConnectorCreateRequest`（`models.py:105-108`）：`name:string="Codex Connector"`；`connectorKind:"desktop"|"cli"="cli"`；`installationId:UUID|null`。
- 响应 `ConnectorCreateResponse`：`connector: ConnectorView`、`connectorToken:string`、`tokenPrefix:string`（token 前 12 字符）。**没有 serverTime**。
- 错误：401/403、409（installation 已删除/冲突）、422（UUID 非法）。
- 实现：`api/connectors.py:59-84`；`infra/repositories/connectors.py:224-262`。

### 6.3 `GET /api/v2/connectors/{connector_id}`

- 鉴权：Bearer user token；必须 owner。
- 响应 `ConnectorResponse`：`connector`、`serverTime`。
- 错误：404（不存在或不属于调用者）。
- 实现：`api/connectors.py:87-103`。

### 6.4 `GET /api/v2/connectors/{connector_id}/protocol/capabilities`

- 鉴权：Bearer user token；owner。
- 响应：**恒为 410**，body：
  ```json
  {"detail":{"code":"connector_protocol_capabilities_route_removed",
             "message":"Connector protocol capability reads were removed from the v2 target API.",
             "use":"/connectors/{connector_id}/runtimes/{runtimeId}/capabilities"}}
  ```
- 错误：404（非 owner）、410。
- 实现：`api/connectors.py:106-134`（`include_in_schema=False`）。

### 6.5 `PATCH /api/v2/connectors/{connector_id}`

- 鉴权：Bearer user token；owner。
- Body `ConnectorUpdateRequest`：`name:string|null`（仅非 null 时更新）。
- 响应 `ConnectorResponse`。
- 错误：404、422、401/403。
- 实现：`api/connectors.py:137-162`。

### 6.6 `DELETE /api/v2/connectors/{connector_id}`

- 鉴权：Bearer user token；owner。
- Body：无。
- 成功：**204 空体**；级联删除 connector 数据、终端 relay、时间线缓存等。
- 错误：404。
- 实现：`api/connectors.py:165-186`。

### 6.7 `POST /api/v2/connectors/{connector_id}/revoke`

- 鉴权：Bearer user token；owner。
- Body：无（客户端传 `{}`）。
- 行为：轮换 connector token，断开当前 connector WS。
- 响应 `ConnectorRevokeResponse`：`connector`、`connectorToken`（新 token）、`tokenPrefix`、`serverTime`。
- 错误：404。
- 实现：`api/connectors.py:189-220`。

### 6.8 `GET /api/v2/connectors/{connector_id}/preferences`

- 鉴权：Bearer user token；owner。
- 响应 `ConnectorPreferencesResponse`：`connectorId:string`、`preferences:object`（connector 侧任意 JSON，缺省 `{}`）、`serverTime`。
- 错误：404。
- 实现：`api/connectors.py:223-240`。没有对应的写接口暴露。

### 6.9 `POST /api/v2/connectors/{connector_id}/sessions/archive-all`

- 鉴权：Bearer user token；owner。
- Body `ArchiveAllRequest`（`models.py:930-933`）：`archived:bool` 必填；`scope:"active"|"archived"|"all"="active"`。
- 响应 `ArchiveAllResponse`：`sessions`、`affected`、`serverTime`。
- 错误：404、422（非法 scope）。
- 实现：`api/connectors.py:243-268`。

---

## 7. 会话 `/sessions/*`

实现：`server/agent_server/api/sessions.py`。响应模型见第 1 节。

### 7.1 `POST /api/v2/sessions`

- 鉴权：Bearer user token。
- Body `SessionCreateRequest`（`models.py:740-759`，`extra="forbid"`）：
  - `connectorId:string` 必填；
  - `projectId:string` 必填，≥1；
  - `runtime:string="claude"`（RuntimeName 校验）；
  - `runtimeId:string|null`（缺省 `runtime`）；
  - `externalSessionId:string|null`；
  - `title:string|null`；
  - `cwd:string|null`（缺省使用 project.workspacePath；若提供必须与 project workspace 一致）；
  - `selections:{string:string|null}={}`。
- 行为：当前实现**只允许导入已存在的 runtime 会话**；`externalSessionId` 为 null 时直接拒绝。
- 响应：普通 dict（无 response_model）：
  ```json
  {"session": SessionView, "connectorResult": null}
  ```
  没有 `serverTime`。
- 错误：404（connector/project 不存在）、409（runtime 路由冲突/connector offline）、422（`project_required`、`project_workspace_mismatch`、"new sessions must use /sessions/create-and-start"）、502。
- 实现：`api/sessions.py:347-374`；`services/session_run.py:119-163`。

### 7.2 `POST /api/v2/sessions/create-and-start`

- 鉴权：Bearer user token。
- Body `SessionCreateAndStartRequest`（`models.py:775-797`，`extra="forbid"`）：
  - `connectorId:string` 必填；
  - `projectId:string` 必填；
  - `runtime:string="claude"`；
  - `runtimeId:string|null`；
  - `title:string|null`；
  - `cwd:string|null`；
  - `content:string` 必填；
  - `selections:{string:string|null}={}`；
  - `runtimeOptions:object={}`（最多 16 个键）；
  - `attachments:InlineAttachmentRef[]=[]`（最多 10 个）；
  - `clientMessageId:string|null`。
- `InlineAttachmentRef`：`fileId` 1..64、`name` 1..512、`mediaType` 默认 `application/octet-stream` ≤255、`size:int|null` ≥0、`sha256:string|null` ≤128、`contentBase64:string` 非空。服务端会校验 base64、size、sha256。
- 响应：普通 dict：
  ```json
  {"session": SessionView, "connectorResult": <connector raw>, "attachments": [<fileId,name,mediaType,size,sha256>, ...]}
  ```
  `attachments` 键始终存在（无附件时为 `[]`）；没有 `serverTime`。
- 错误：404（connector/project/runtime/attachment 不存在）、409（connector offline、runtime capability 不可用、附件 MIME 不支持）、422（project 绑定/附件内容非法）、502（connector 返回非法结果）、504。
- 实现：`api/sessions.py:377-414`；`services/session_run.py:165-334`。

### 7.3 `GET /api/v2/sessions`

- 鉴权：Bearer user token。
- Query：
  - `archived:bool=false`（true 只返回已归档）；
  - `limit:int=100`，范围 1..100；
  - `cursor:string|null`，≥1。
- 响应 `SessionListResponse`（普通 dict）：`sessions: SessionView[]`、`hasMore:bool`、`nextCursor:string|null`、`serverTime`。
- 排序：`pinned DESC, sortAt DESC, latestItemOrderSeq DESC, updatedSeq DESC, id DESC`。
- 错误：400（游标非法）、401/403。
- 实现：`api/sessions.py:417-457`；分页 `infra/repositories/sessions.py:750-819`。

### 7.4 `GET /api/v2/sessions/list`

- 鉴权：Bearer user token。
- Query：无。
- 响应：普通 dict `{"sessions": SessionView[], "serverTime": string}`。
- 行为：返回该用户**全部** session inventory（含 archived），不分页；用于 Dashboard 分组。
- 错误：401/403。
- 实现：`api/sessions.py:460-475`。

### 7.5 `GET /api/v2/sessions/{session_id}/meta`

- 鉴权：Bearer user token；owner。
- 响应 `SessionResponse`。
- 错误：404。
- 实现：`api/sessions.py:478-497`。

### 7.6 `PATCH /api/v2/sessions/{session_id}/meta`

- 鉴权：Bearer user token；owner。
- Body `SessionPatchRequest`（`models.py:915-918`）：`title:string|null`（1..255）、`pinned:bool|null`、`archived:bool|null`。三者都可省略；全为 null 时不做修改并返回当前状态。
- 响应 `SessionResponse`。
- 错误：404、422（title 非法）。
- 实现：`api/sessions.py:500-551`。

### 7.7 `POST /api/v2/sessions/archive`

- 鉴权：Bearer user token。
- Body：**JSON 字符串数组**，元素数 1..200（不是对象）。
- 响应 `BulkArchiveResponse`。
- 错误：422（数量越界）、401。
- 实现：`api/sessions.py:596-611`。

### 7.8 `POST /api/v2/sessions/unarchive`

- 同 7.7，`archived=false`。
- 实现：`api/sessions.py:614-629`。

### 7.9 `POST /api/v2/sessions/read`

- 鉴权：Bearer user token。
- Body：**JSON 字符串数组**，1..200。
- 响应 `BulkArchiveResponse`（返回更新后的 sessions，含 `lastReadSeq/unread` 等）。
- 错误：422。
- 实现：`api/sessions.py:632-665`。

### 7.10 `GET /api/v2/sessions/{session_id}/runtime/state`

- 鉴权：Bearer user token；owner。
- 行为：connector 在线时实时 RPC `session.state`；离线时回退缓存/DB。
- 响应 `SessionRuntimeStateResponse`：`state: SessionRuntimeState`、`serverTime`。
- 错误：404、502（runtime 绑定非法）。
- 实现：`api/sessions.py:673-694`；实时读取 `sessions.py:1631-1655`。

### 7.11 `GET /api/v2/sessions/{session_id}/runtime/capabilities`

- 鉴权：Bearer user token；owner。
- 行为：直接向 connector 读取 session capability facts，并派生 effective capability set；**无离线回退**。
- 响应 `ProtocolCapabilitiesResponse`：`connectorId`、`capabilitySet: ProtocolCapabilitySet`、`serverTime`。
- 错误：404、409（connector offline）、504（`runtime_capabilities_timeout`）、502（RPC/非法 capability set）。
- 实现：`api/sessions.py:697-721`；`sessions.py:1713-1743`。

### 7.12 `GET /api/v2/sessions/{session_id}/runtime/catalogs/model`

- 鉴权：Bearer user token；owner。
- 行为：确保 runtime 处于 running，然后 RPC `runtime.modelCatalog`，`limit=200`。
- 响应 `ProtocolModelCatalogResponse`：`catalog:{runtime,revision,models[]}`、`serverTime`。
  `models[]`：`displayName`、`id`、`selectionId|null`、`description|null`、`default`、`reasoningItems[]`、`metadata`。
- 错误：404、409、422、502、504、503（runtime 未配置/未启动/启动中，`DeviceRuntimeError`）。
- 实现：`api/sessions.py:724-755`。

### 7.13 `GET /api/v2/sessions/{session_id}/runtime/catalogs/permission`

- 同 7.12，RPC `runtime.permissionCatalog`。
- 响应 `ProtocolPermissionCatalogResponse`：`catalog:{runtime,revision,permissions[]}`、`serverTime`。
  `permissions[]`：`displayName`、`id`、`selectionId`、`description|null`、`default`、`metadata`。
- 实现：`api/sessions.py:758-789`。

### 7.14 `PATCH /api/v2/sessions/{session_id}/runtime/selections`

- 鉴权：Bearer user token；owner。
- Body `SessionSelectionPatchRequest`（`models.py:902-905`，`extra="forbid"`）：`selections:{string:string|null}`，键数 1..32。
- 行为：要求 `takeover=true`、connector online、runtime running；按 `model`/`permission` 键检查对应 capability。
- 响应 `SessionSelectionPatchResponse`：`ok:true`、`state: SessionRuntimeState|null`、`connectorResult:object|null`、`serverTime`。
- 错误：404、409（只读/离线/capability 不可用）、422、502、504。
- 实现：`api/sessions.py:792-826`；`services/session_run.py:571-630`。

### 7.15 `GET /api/v2/sessions/{session_id}/timeline`

- 鉴权：Bearer user token；owner。
- Query：
  - `afterSeq:int=0`（alias `afterSeq`，≥0；仅 `mode=changes` 使用）；
  - `beforeOrderSeq:int|null`（alias `beforeOrderSeq`，≥1；`mode=history` 必填）；
  - `mode:"latest"|"changes"|"history"="latest"`；
  - `limit:int=100`，1..500。
- 响应 `ProtocolTimelineResponse`。
- 错误：404、422（history 缺 `beforeOrderSeq`）。
- 实现：`api/sessions.py:829-876`。

### 7.16 `GET /api/v2/sessions/{session_id}/snapshot`

- 鉴权：Bearer user token；owner。
- Query：`limit:int=100`，1..500。
- 行为：首次水合；runtime state 用缓存/DB 快速返回，后台再向 connector 刷新；catalogs 独立读取；notices 读取失败时返回 `[]`。
- 响应 `ProtocolSessionSnapshotResponse`（见 1.12）。`approvals` 当前恒为 `[]`；`catalogs` 只包含成功读取到的 `model`/`permission` 键。
- 错误：404。
- 实现：`api/sessions.py:886-1014`。

### 7.17 `GET /api/v2/sessions/{session_id}/events`

- 鉴权：Bearer user token；owner。
- Query：`after:string="seq:0"`，格式必须为 `^seq:(0|[1-9][0-9]*)$`。
- 响应 `ProtocolEventRecoveryResponse`。`snapshotRequired=true` 时 `events=[]`。
- 错误：404、422（游标非法/超出上限）。
- 实现：`api/sessions.py:1022-1038`；`services/event_recovery.py:60-195`。

### 7.18 `WS /api/v2/sessions/{session_id}/ws`

- 鉴权：`?ticket=<ws-ticket>`（session scope）。缺失/无效/无权 → close code `1008`（reason `missing ticket`/`invalid ticket`/`session not found`）。
- 连接后第一条帧是 `ProtocolEventEnvelope`，`type="session.subscribed"`，`payload={"clientId": string, "eventCursor": "seq:<n>"}`。
- 之后推送 `ProtocolEventEnvelope`；每 15 秒无消息发送 `{"type":"keepalive","serverTime":...}`。
- 事件类型（至少）：`timeline.item_created`、`timeline.item_updated`、`timeline.snapshot`、`runtime.state.updated`、`session.meta.updated`、`runtime.capability.updated`、`runtime.catalog.updated`、`runtime.notice.updated`、`runtime.notice.snapshot`、`session.refetch_required`。
- 服务端会按 `eventId` 去重，并对 `runtime.capability.updated` / 当前态投影做相邻去重。
- 背压溢出时 close `1013`（reason `server busy; reconnect to recover`）。
- 实现：`api/sessions.py:1041-1148`；事件构造 `core/events.py`。

### 7.19 `POST /api/v2/sessions/{session_id}/takeover`

- 鉴权：Bearer user token；owner。
- Body：无。
- 响应 `TakeoverResponse`：只有 `{"session": SessionView}`，**没有 serverTime**。
- 错误：404。
- 实现：`api/sessions.py:1151-1180`。

### 7.20 `DELETE /api/v2/sessions/{session_id}/takeover`

- 鉴权：Bearer user token；owner。
- 响应 `TakeoverResponse`（同上）。
- 错误：404。
- 实现：`api/sessions.py:1183-1212`。

### 7.21 `GET /api/v2/sessions/{session_id}/runtime/commands`

- 鉴权：Bearer user token；owner。
- Query：`query:string|null`（≤4096）、`limit:int=100`（1..1000）。
- 行为：要求 capability `session.commands` 可用；RPC `session.commands`（timeout 30s）。
- 响应 `SessionCommandListResponse`：`commands: RuntimeCommandView[]`、`serverTime`。
  `RuntimeCommandView`：`id`、`title`、`description|null`、`aliases:string[]`、`category|null`、`scope:string`(默认 "session")、`enabled:bool`、`disabledReason|null`、`acceptsArgs:bool`、`argsSchema:object|null`、`metadata:object`。
- 错误：404、409（capability 不可用/offline）、502（RPC 或非法 command list）、500（未映射的 RPC Timeout）。
- 实现：`api/sessions.py:1242-1295`。

### 7.22 `POST /api/v2/sessions/{session_id}/runtime/commands`

- 鉴权：Bearer user token；owner。
- Body `SessionCommandRequest`（`models.py:1151-1156`，`extra="forbid"`）：`command:string` 必填，1..64，匹配 `^[A-Za-z][A-Za-z0-9_-]*$`；`args:string[]=[]` 最多 32；`raw:string|null` ≤4096。
- 响应 `SessionCommandResponse`：`command`、`ok:bool`、`code:string|null`、`message:string|null`、`result:any`、`session:SessionView|null`、`serverTime`。
- 特殊：connector offline/timeout 或响应不合规时**返回 200**：
  ```json
  {"command":"...","ok":false,"code":"command_outcome_unknown",
   "message":"Command outcome is unknown. Refresh session state before taking further action.",
   "result":{"executionState":"unknown","retryable":false},"session":null,"serverTime":"..."}
  ```
- 错误：404、409（capability/offline）、422、502。
- 实现：`api/sessions.py:1298-1378`。

### 7.23 `POST /api/v2/sessions/{session_id}/runtime/messages`

- 鉴权：Bearer user token；owner。
- Body `MessageCreateRequest`（`models.py:1140-1148`，`extra="forbid"`）：
  - `content:string` 必填（无 min_length）；
  - `attachments:AttachmentRef[]=[]`，最多 10；`AttachmentRef.fileId` 1..64；
  - `clientMessageId:string|null`。
- 行为：要求 `takeover=true`、connector online、runtime 状态为 `idle`/`error`、capability `session.send_message`，附件另需 `runtime.attachment`。
- 响应 `RpcResponsePayload`：`ok:true`、`result`=connector 原始结果、`error:null`。
- 错误：404（session/attachment）、409（archived/只读/offline/session busy/capability/MIME）、422、502、504。
- 实现：`api/sessions.py:1381-1413`；`services/session_run.py:399-494`。

### 7.24 `POST /api/v2/sessions/{session_id}/runtime/interrupt`

- 鉴权：Bearer user token；owner。
- Body：无。
- 行为：要求 takeover、capability `session.interrupt`、runtime running。
- 响应 `RpcResponsePayload`。
- 错误：404、409、422、502、504。
- 实现：`api/sessions.py:1416-1447`；`services/session_run.py:914-958`。

### 7.25 `POST /api/v2/sessions/{session_id}/runtime/steer`

- 鉴权：Bearer user token；owner。
- Body `SessionSteerRequest`（`models.py:1193-1198`，`extra="forbid"`）：`content:string` 必填；`attachments:AttachmentRef[]=[]` 最多 10；`clientMessageId:string|null`。
- 行为：要求 takeover、connector online、runtime `running`、capability `session.steer`。
- 响应 `RpcResponsePayload`。
- 错误：404、409、422、502、504。
- 实现：`api/sessions.py:1450-1486`；`services/session_run.py:632-706`。

### 7.26 `GET /api/v2/sessions/{session_id}/runtime/notices`

- 鉴权：Bearer user token；owner。
- Query：无。
- Behavior：RPC `session.notices`（timeout 10s 默认）。
- 响应 `RuntimeNoticeListResponse`：`notices: NoticeIn[]`（注意不是 `Notice`，**没有** `updatedSeq/updatedAt`）、`serverTime`。
- 错误：404、409（offline）、504（`runtime_notices_timeout`）、502（`invalid_runtime_notices`）。
- 实现：`api/sessions.py:1489-1504`；`sessions.py:1778-1826`。

### 7.27 `POST /api/v2/sessions/{session_id}/runtime/notices/{notice_id}/respond`

- 鉴权：Bearer user token；owner。
- 路径参数：`notice_id:string`。
- Body `InteractionRespondRequest`（`models.py:1135-1137`）：`actionId:string` 必填；`input:object|null`。
- 行为：要求 capability `session.interaction.approval`；会把该 notice 的 `context` 与用户 `input` 浅合并（`approvalSource` 再深合并一层），RPC `interaction.respond`。
- 响应 `RpcResponsePayload`。
- 错误：
  - 404（session 不存在）；
  - 409（capability 不可用、connector offline、RPC `conflict`）；
  - 502（其它 RPC 错误）；
  - **200 + `ok:false`**：RPC code 为 `not_found`、`notice_not_found`、`interaction_not_found`、`request_not_found`、`approval_not_found` 时，body `{"ok":false,"result":null,"error":{"code":"...","message":"..."}}`。
- 实现：`api/sessions.py:1507-1585`；合并逻辑 `sessions.py:1829-1895`。

### 7.28 `POST /api/v2/sessions/{session_id}/sync`

- 鉴权：Bearer user token；owner。
- Body：无。
- 前置：connector online、session 有 `externalSessionId`、runtime 可路由且 running。
- 行为：RPC `session.sync`（timeout 60s）。
- 响应 `RpcResponsePayload`：`ok:true`、`result`=connector 结果。
- 错误：404、409（offline / 无 external id）、502、503/422（runtime 状态）。
- 实现：`api/sessions.py:1588-1628`。

---

## 8. 附件与文件

### 8.1 会话附件（`api/sessions_fs.py`）

#### 8.1.1 `POST /api/v2/sessions/{session_id}/attachments`

- 鉴权：Bearer user token；owner。
- Content-Type：`multipart/form-data`；字段名 `files`，可重复。
- 限制：单请求最多 5 个文件；单文件 ≤25 MiB；空文件 422。
- 响应 `UserUploadResponse`：`attachments: UploadedAttachment[]`、`serverTime`。
  `UploadedAttachment`：`fileId`、`sessionId`、`name`、`size:int`、`sha256`、`mediaType`、`createdAt`、`downloadUrl`（相对路径 `/api/v2/sessions/{id}/attachments/{fileId}`）、`openUrl`（相对路径 `.../open`）。
- 错误：404（session）、413（单文件过大）、422（无文件/超过 5 个/空文件）。
- 实现：`api/sessions_fs.py:106-161`；视图 `services/attachments.py:141-158`。

#### 8.1.2 `GET /api/v2/sessions/{session_id}/attachments/{file_id}`

- 鉴权：Bearer user token；owner。
- 响应 `FsDownloadResponse`：`fileId`、`sessionId`、`path`、`name`、`size:int`、`sha256`、`contentBase64:string`、`createdAt`、`serverTime`。
- 错误：404（session/file）、500（文件完整性/存储错误）。
- 实现：`api/sessions_fs.py:31-48`。

#### 8.1.3 `GET /api/v2/sessions/{session_id}/attachments/{file_id}/open`

- 鉴权：`Authorization: Bearer <JWT>` **或** `?token=<JWT>`（`api/sessions_fs.py:184-198`）。
- 响应：**302** 重定向到签名 URL；本地存储时为 `/api/v2/sessions/local/{session_id}/{file_id}?token=<signed>`（5 分钟）；S3 等后端返回其 presigned URL。
- 错误：404、401。
- 实现：`api/sessions_fs.py:51-70`；`services/attachments.py:92-125`。

#### 8.1.4 `GET /api/v2/sessions/local/{session_id}/{file_id}`（内部/签名）

- 鉴权：`?token=<signed local_file token>`（5 分钟）。**不是给客户端直接调用的稳定 API**，只作为 `open` 的重定向目标。
- 响应：二进制，`Content-Type` 为存储的 mediaType；headers：`Content-Disposition: inline; ...`、`X-File-Name`、`X-File-Sha256`。
- 错误：401（token 无效/绑定不匹配）、404、500。
- 实现：`api/sessions_fs.py:73-103`；token 常量 `services/attachments.py:15-17`。

### 8.2 Connector 文件系统（`api/connector_files.py`）

所有 `/{connector_id}/fs/*` 接口都要求 connector 属于调用者；除 `preview-token`/`preview-session` 外都要求 connector 在线（离线 409）。

#### 8.2.1 `POST /api/v2/connectors/{connector_id}/fs/list`

- 鉴权：Bearer user token；owner + online。
- Query：`root:string|null`（≥1）。
- Body：任意 JSON 对象（默认 `{}`）。服务端读取 `body.root`（query `root` 优先）和 `body.path`（默认 `"."`，必须是 string）。
- 行为：路径按 connector OS 规范化（Windows 空 path 保留空串），RPC `fs.readDir`。
- 响应 `RpcResponsePayload`：`result` 由 connector 返回；Web 客户端期望 `{path:string, entries:array, truncated?:bool, targetPath?:string|null, targetType?:string|null}`，`entries[]` 的字段由 connector 定义。
- 错误：404（connector）、409（offline）、422（root 缺失/path 非 string）、502/504/500（RPC）。
- 实现：`api/connector_files.py:57-91`。

#### 8.2.2 `POST /api/v2/connectors/{connector_id}/fs/read`

- 鉴权：Bearer user token；owner + online。
- Query：`root:string` **必填**（≥1）。
- Body `FsReadRequest`：`path:string` 必填 ≥1。
- 行为：RPC `fs.prepareDownload`，随后创建 TTL 300 秒的下载 relay。
- 响应 `RpcResponsePayload`，`result` = connector prepareDownload 结果并追加：
  `transferId:string`、`token:string`、`downloadUrl:string`（`/api/v2/connectors/{id}/fs/transfers/{transferId}?token=...`）。
  Web/iOS 期望还包含 `path`、`name`、`size`、`sha256`、`mediaType`。
- 错误：404、409、422、502、504、500。
- 实现：`api/connector_files.py:94-125`；`services/connector_files.py:22-56`。

#### 8.2.3 `POST /api/v2/connectors/{connector_id}/fs/preview-token`

- 鉴权：Bearer user token；owner（**不要求在线**）。
- Query：`root:string` 必填 ≥1。
- Body `FsReadRequest`：`path:string` 必填 ≥1。
- 响应 `FsPreviewTokenCreateResponse`：`previewToken:string`（signed，5 分钟，单次使用）、`expiresAt:string`、`serverTime`。
- 错误：404、422、401。
- 实现：`api/connector_files.py:128-163`。

#### 8.2.4 `POST /api/v2/connectors/fs/preview-session`

- 鉴权：**无**（凭 `previewToken`）。
- Body `FsPreviewSessionRequest`：`previewToken:string` 必填 ≥1。
- 行为：校验并消费 preview token；签发 15 分钟 `preview_access` token。
- 响应 `FsPreviewSessionResponse`：`previewAccessToken:string`、`expiresAt:string`、`connectorId:string`、`root:string`、`path:string`、`serverTime`。
- 错误：400（token 无效/已使用/过期）。
- 实现：`api/connector_files.py:166-211`。

#### 8.2.5 `POST /api/v2/connectors/fs/preview/readText`

- 鉴权：**无**（凭 `previewAccessToken`），但内部仍校验 connector owner + online。
- Body `FsPreviewReadTextRequest`：`previewAccessToken:string` 必填；`maxBytes:int=1048576`，1..4194304。
- 响应 `FsReadTextResponse`：`path`、`name`、`size:int`、`sha256`、`encoding`、`content`、`truncated:bool`、`binary:bool`、`serverTime`。
- 错误：401（access token 无效）、404、409（offline）、422（maxBytes 越界）、502/504。
- 实现：`api/connector_files.py:214-239`。

#### 8.2.6 `POST /api/v2/connectors/fs/preview/read`

- 鉴权：**无**（凭 `previewAccessToken`），内部校验 owner + online。
- Body `FsPreviewReadRequest`：`previewAccessToken:string` 必填。
- 响应 `RpcResponsePayload`，`result` 追加 `transferId`、`token`、`downloadUrl`（下载 URL 同时带 `previewAccessToken`）。
- 错误：401、404、409、502/504。
- 实现：`api/connector_files.py:242-280`。

#### 8.2.7 `GET /api/v2/connectors/{connector_id}/fs/transfers/{transfer_id}`

- 鉴权（二选一）：
  - `?previewAccessToken=<token>`（优先，忽略 Authorization）；
  - `Authorization: Bearer <user JWT>`。
- Query：`token:string` **必填**（transfer token）；`previewAccessToken:string|null`。
- 响应：二进制流；headers：
  `Content-Disposition: attachment; filename=...; filename*=UTF-8''...`、`X-File-Name`、`X-File-Sha256`、`X-File-Size`；`Content-Type` 为 transfer.mediaType 或 `application/octet-stream`。
- 错误：401（无有效凭证）、404（transfer 不存在/connector 不匹配/preview 不匹配）、409/502/504（请求 connector 上传失败）。
- 说明：服务端先让 connector 通过 `PUT /api/v2/connector/fs/transfers/{transfer_id}` 上传内容，再向浏览器流式转发。
- 实现：`api/connector_files.py:283-333`。

#### 8.2.8 `POST /api/v2/connectors/{connector_id}/fs/readText`

- 鉴权：Bearer user token；owner + online。
- Query：`root:string` 必填 ≥1。
- Body `FsReadTextRequest`：`path:string` 必填 ≥1；`maxBytes:int=1048576`，1..4194304。
- 响应 `FsReadTextResponse`（同 8.2.5）。
- 错误：404、409、422、502/504。
- 实现：`api/connector_files.py:336-359`。

#### 8.2.9 `POST /api/v2/connectors/{connector_id}/fs/write`

- 鉴权：Bearer user token；owner + online。
- Query：`root:string` 必填 ≥1。
- Body `FsWriteRequest`：`path:string` 必填；`content:string` 必填；`encoding:"utf8"|"utf-8"="utf8"`；`ifMatch:string|null`（恰好 64 字符，乐观并发 sha256；不匹配时 connector 侧返回 HTTP 412）。
- 响应 `RpcResponsePayload`；Web 客户端期望 `result={path,encoding,bytesWritten:int,sha256}`。
- 错误：404、409、412、422、502/504。
- 实现：`api/connector_files.py:362-385`。

---

## 9. 终端

### 9.1 Connector 终端 v2（重点，`api/connector_terminal.py`）

通用：
- 全部要求 Bearer user token + connector owner + **connector online**。
- 响应统一为 `RpcResponsePayload`；`result` 是 connector 返回对象，服务端会补齐/规范化 `TerminalView` 字段（`terminalId`、`sessionId="browse_<connectorId>"`、`root`、`cwd`、`label`、`purpose="user"`、`pid:null`、`cols=80`、`rows=24`、`status="running"`、`exitCode:null`、`scrollbackBytes=0`、`scrollbackSeq=0`、`persistent=false`、`createdAt`）。
- 常见错误：404（connector 不存在/非 owner）、409（offline）、502（connector RPC）、504（timeout）。

#### 9.1.1 `POST /api/v2/connectors/{connector_id}/terminals-v2`

- Query：`root:string` **必填** ≥1。
- Body `TerminalCreateRequest`（`models.py:1321-1335`）：
  `cwd:string|null`（相对 root，缺省 "."）、`shell:string|null`、`command:string|null`（≥1）、`args:string[]|null`（最多 100）、`profile:string|null` ≤64、`cols:int=80`（1..500）、`rows:int=24`（1..200）、`label:string|null` ≤64、`ephemeralGroupId:string|null`（1..96，**v2 服务端接收但不转发给 connector**）、`persistent:bool=false`、`env:{string:string}|null`。
- 行为：服务端生成 `terminalId="trm_..."`，RPC `terminal.create`，`outputTransport="relay"`，并记录 root/cwd 元数据。
- 响应：`result` 为规范化后的 terminal 对象。
- 实现：`api/connector_terminal.py:117-170`。

#### 9.1.2 `GET /api/v2/connectors/{connector_id}/terminals-v2`

- Query：无。
- 行为：RPC `terminal.list`（`sessionId="browse_<connectorId>"`），合并 DB 中记录的 root/cwd，prune 已消失的 terminal 元数据。
- 响应：`result={"terminals": TerminalView[]}`（可能带 connector 的额外字段）。
- 实现：`api/connector_terminal.py:187-231`。

#### 9.1.3 `PATCH /api/v2/connectors/{connector_id}/terminals-v2/{terminal_id}`

- Body `TerminalPatchRequest`：`label:string` 必填 1..64。
- 行为：RPC `terminal.rename`。
- 响应：`result` 为规范化 terminal。
- 实现：`api/connector_terminal.py:254-290`。

> **已从当前服务端移除（客户端仍在调用）**：`PATCH .../terminals-v2/{id}/persistence`、`POST .../terminals-v2/{id}/resize`、`POST .../terminals-v2/{id}/write`、`GET .../terminals-v2/{id}/snapshot` 在 2026-10-24 17:46 的 `api/connector_terminal.py` 中已不存在（对应的 `TerminalPersistenceRequest` 模型也已从 `core/models.py` 删除）。`web-next/src/features/dashboard/api.ts:599-663` 仍会调用这 4 个路径，因此当前会得到 404。Go 实现若要兼容现有 Web，需要决定是否恢复或同步删除客户端调用。

#### 9.1.4 `DELETE /api/v2/connectors/{connector_id}/terminals-v2/{terminal_id}`

- Body：无。
- 行为：RPC `terminal.close`，清理 relay 与本地 root 元数据。
- 响应 `RpcResponsePayload`。
- 实现：`api/connector_terminal.py:311-339`。

#### 9.1.5 `WS /api/v2/connectors/{connector_id}/terminals-v2/{terminal_id}/stream`

- 鉴权：`?token=<user JWT>` 或 `Authorization: Bearer`；缺失/无效 close `4401`，connector 非 owner close `4404`。
- Query：`fromSeq:int=0`。
- 服务端帧：
  - `{"type":"replay","data":"<base64>","seq":<int>}`（snapshot 有数据时）；
  - `{"type":"error","code":<http status>,"message":"..."}`；
  - `{"type":"exit","exitCode":<int|null>,"reason":"exit"}`（连接时 terminal 已 exited）；
  - `{"type":"pong"}`。
- 客户端帧：
  - `{"type":"input","data":"<base64>"}`；
  - `{"type":"resize","cols":int,"rows":int}`（服务端夹取 1..500 / 1..200）；
  - `{"type":"ping"}`。
- 实现：`api/connector_terminal.py:363-486`。

### 9.2 Connector 终端 legacy（`/connectors/{id}/terminals`）

Web 旧面板仍调用 list/create；新代码走 v2。

#### 9.2.1 `POST /api/v2/connectors/{connector_id}/terminals`

- 鉴权：Bearer user token；owner + online。
- Query：`root:string` 必填 ≥1。
- Body：`TerminalCreateRequest`（同上）。
- 响应 `TerminalResponse`：`terminal: TerminalView`、`serverTime`。
- 错误：404、409、502/504。
- 实现：`api/connector_terminal.py:99-113`。

#### 9.2.2 `GET /api/v2/connectors/{connector_id}/terminals`

- 鉴权：Bearer user token；owner（不要求 online）。
- 响应 `TerminalListResponse`：`terminals: TerminalView[]`、`serverTime`。
- 实现：`api/connector_terminal.py:172-183`。

#### 9.2.3 `PATCH /api/v2/connectors/{connector_id}/terminals/{terminal_id}`

- Body `TerminalPatchRequest`（label 1..64）。响应 `TerminalResponse`。
- 实现：`api/connector_terminal.py:234-251`。

#### 9.2.4 `DELETE /api/v2/connectors/{connector_id}/terminals/{terminal_id}`

- 响应 `TerminalResponse`（返回关闭前的快照）。
- 实现：`api/connector_terminal.py:293-308`。

#### 9.2.5 `POST /api/v2/connectors/{connector_id}/terminals/{terminal_id}/resize`

- Body `TerminalResizeRequest`。响应 `TerminalResponse`。
- 实现：`api/connector_terminal.py:342-360`。

#### 9.2.6 `WS /api/v2/connectors/{connector_id}/terminals/{terminal_id}/stream`

- 鉴权：`?token=<JWT>` 或 Bearer；close `4401`/`4404`。
- Query：`fromSeq:int=0`。
- 客户端帧：`input`(data base64)、`resize`(cols/rows)、`ping`。
- 服务端输出帧：由 `TerminalBroker`/connector relay 定义，**本仓库未在 Python 侧定义 schema → UNKNOWN**（检查了 `api/connector_terminal.py:489-564` 与 `infra/terminal_broker.py`；只确认 `pong`）。
- 实现：`api/connector_terminal.py:489-564`。

### 9.3 会话终端（legacy，`api/sessions_terminal.py`）

当前 Web/Android/iOS 的 API 层未调用这些路径（已核对 `web-next/src/features/dashboard/api.ts`、`android/.../SessionsApi.kt`、`ios/.../V2SessionAPI.swift`）；保留为产品 API。

路径与行为：
- `POST /api/v2/sessions/{session_id}/terminals`，Body `TerminalCreateRequest`，响应 `TerminalResponse`（`sessions_terminal.py:47-57`）。
- `GET /api/v2/sessions/{session_id}/terminals`，响应 `TerminalListResponse`（`:60-69`）。
- `PATCH /api/v2/sessions/{session_id}/terminals/{terminal_id}`，Body `{label}`，响应 `TerminalResponse`（`:72-83`）。
- `DELETE /api/v2/sessions/{session_id}/terminals/{terminal_id}`，响应 `TerminalResponse`（`:86-96`）。
- `POST /api/v2/sessions/{session_id}/terminals/{terminal_id}/resize`，Body `{cols,rows}`，响应 `TerminalResponse`（`:99-109`）。
- `WS /api/v2/sessions/{session_id}/terminals/{terminal_id}/stream?token=<JWT>&fromSeq=0`（`api/sessions_terminal.py:116-214`）；close `4401`（无/无效 token）、`4404`（session 或 terminal 不存在）；客户端帧 `input`/`resize`/`ping`；服务端输出帧 schema **UNKNOWN**（只确认 `pong`）。
- 错误：404（session/terminal）、409/502/504（connector）。

---

## 10. 时间线、消息与运行时（汇总入口）

上述 7.15–7.27 已逐条给出。补充关键语义：

- `snapshot` 是首屏 hydration；`timeline` 是 durable 项读取；`events` 是断线恢复；`ws` 是实时推送。
- 事件游标统一为 `seq:<n>`，`n` 是会话级 revision（`core/events.py:20-40`）。
- `eventCursor` 与 `nextCursor` 都来自 `get_session_seq`。
- `timeline` 的 `mode`：
  - `latest`：最近 N 条；
  - `changes`：`updatedSeq > afterSeq`；
  - `history`：`orderSeq < beforeOrderSeq`。
- `snapshot.notices` 读取失败时静默为 `[]`（`api/sessions.py:1898-1905`）。
- `runtime/messages`、`steer`、`interrupt`、`respond` 成功后都会 best-effort 发布 session protocol 更新（`session.meta.updated` / `runtime.state.updated` 等）。

---

## 11. Dashboard

### 11.1 `WS /api/v2/dashboard/ws`

- 鉴权：`?ticket=<ws-ticket>`（dashboard scope）。缺失/无效 close `1008`。
- 首帧：`{"type":"dashboard.snapshot", ...}`（`api/dashboard_stream.py:40-90`）：
  - `connectors: ConnectorView[]`（effective status）；
  - `projects: ProjectView[]`；
  - `sessions: SessionView[]`（全量 inventory，含 archived；effective status / 缓存 runtime state 投影）；
  - `runtimes: DeviceRuntimeView[]`（以 alias 序列化，含 `schema`）；
  - `sessionPages: {"active":{"hasMore":false,"nextCursor":null},"archived":{"hasMore":false,"nextCursor":null}}`；
  - `serverTime:string`。
- 之后：收到 `dashboard.changed` 失效消息时重新发送完整 snapshot；空闲 15 秒发送 `{"type":"keepalive","serverTime":...}`。
- 实现：`api/dashboard_stream.py:93-165`。

---

## 12. 项目 `/projects/*`

实现：`server/agent_server/api/projects.py`；模型 `core/models.py:138-204`。

### 12.1 `GET /api/v2/projects`

- 鉴权：Bearer user token。
- 响应 `ProjectListResponse`：`projects: ProjectView[]`、`serverTime`。按 pinned、最近活动排序。
- 实现：`api/projects.py:40-48`。

### 12.2 `POST /api/v2/projects`

- 鉴权：Bearer user token。
- Body `ProjectCreateRequest`（`models.py:161-171`，`extra="forbid"`，strip whitespace）：
  `name:string` 1..255 必填；`connectorId:string` 必填 ≥1；`workspacePath:string` 1..4096 必填；`manuallyCreated:bool=true`；`attachMatchingSessions:bool=false`（仅为兼容旧客户端，服务端忽略）。
- 响应 `ProjectCreateResponse`：`project: ProjectView`、`attachedSessions:int=0`、`serverTime`。
- 错误：404（connector）、409 `{"detail":{"code":"project_name_conflict","message":"..."}}`、422。
- 实现：`api/projects.py:51-89`。

### 12.3 `PATCH /api/v2/projects/{project_id}`

- 鉴权：Bearer user token；owner。
- Body `ProjectPatchRequest`（`models.py:174-184`，`extra="forbid"`）：`name:string|null` 1..255、`pinned:bool|null`；至少一个非 null，否则 422。
- 响应 `ProjectResponse`：`project`、`serverTime`。
- 错误：404、409（重名）、422。
- 实现：`api/projects.py:92-126`。

### 12.4 `DELETE /api/v2/projects/{project_id}`

- 鉴权：Bearer user token；owner。
- 响应 `ProjectDeleteResponse`：`projectId`、`detachedSessions:int`、`serverTime`。
- 错误：404、409（例如仍有约束）。
- 实现：`api/projects.py:129-152`。

### 12.5 `GET /api/v2/projects/{project_id}/sessions`

- 鉴权：Bearer user token；owner。
- Query：`archived:bool=false`；`limit:int=100`（1..100）；`cursor:string|null`（≥1）。
- 响应 `ProjectSessionListResponse`。
- 错误：404、400（游标非法）。
- 实现：`api/projects.py:155-192`。

### 12.6 `POST /api/v2/projects/{project_id}/sessions/archive-all`

- 鉴权：Bearer user token；owner。
- Body `ArchiveAllRequest`（`{archived:bool, scope?}`）。
- 响应 `ArchiveAllResponse`。
- 错误：404、422。
- 实现：`api/projects.py:195-232`。

---

## 13. 分享 `/public/shares/*` 与 `/sessions/{id}/shares`

实现：`server/agent_server/api/shares.py`；模型 `core/models.py:953-1027`。

### 13.1 `POST /api/v2/sessions/{session_id}/shares`

- 鉴权：Bearer user token；owner。
- Body `SessionShareCreateRequest`（`extra="forbid"`）：
  - `scope:"message"|"session"` 必填；
  - `itemIds:string[]=[]`，最多 100。
  - 约束：`message` 必须非空；`session` 必须为空；不得重复。
- 行为：`message` 分享只允许 `type=="message" && role=="assistant"` 的时间线项，否则 422；找不到 item → 404。
- 响应 **201** `SessionShareCreateResponse`：`shareId`、`sharePath`（`/share/{id}`，不带 `/api/v2`）、`shareUrl`（public origin + sharePath）、`scope`、`createdAt`。
- 错误：404（session/item）、422（scope/item 约束/无内容）。
- 实现：`api/shares.py:30-102`。

### 13.2 `GET /api/v2/public/shares/{share_id}`

- 鉴权：无。
- 响应 `PublicSessionShareResponse`：`shareId`、`scope`、`session:{id,title|null,runtime,runtimeName|null,cwd|null}`、`items: TimelineItem[]`、`createdAt`、`serverTime`。
- 错误：404（share 不存在）、500（快照损坏）。
- 实现：`api/shares.py:105-127`。

> 注意：创建分享时，快照中的 item 使用 `exclude_none=True` 存储（`shares.py:83`）；读取时会重新 `TimelineItem.model_validate`，因此响应仍会补回声明字段的默认值/null。

### 13.3 `GET /api/v2/public/shares/{share_id}/attachments/{file_id}`

- 鉴权：无；但 `file_id` 必须在创建分享时扫描出的 `allowedFileIds` 内。
- 响应：二进制，`Content-Type` 为 mediaType，`Content-Disposition: inline`，`Cache-Control: private, max-age=300`。
- 错误：404（share/file 不存在或未授权）、500。
- 实现：`api/shares.py:130-157`。

---

## 14. 公告 `/announcement`

实现：`server/agent_server/api/announcements.py`；模型 `core/announcement.py`。

### 14.1 `GET /api/v2/announcement`

- 鉴权：无。
- 响应 `AnnouncementResponse`：`{"announcement": {"markdown": string, "publishedAt": string} | null}`。未启用或无 `publishedAt` 时 `announcement` 为 **null**。
- Header：`Cache-Control: no-store`。
- 实现：`api/announcements.py:30-42`。

### 14.2 `GET /api/v2/admin/announcement`

- 鉴权：Bearer user token + admin。
- 响应 `AnnouncementSettings`：`enabled:bool`、`markdown:string`、`publishedAt:string|null`。
- 错误：401/403。
- 实现：`api/announcements.py:45-50`。

### 14.3 `PUT /api/v2/admin/announcement`

- 鉴权：admin。
- Body `AnnouncementUpdate`（`extra="forbid"`, strict）：`enabled:bool` 必填；`markdown:string` 必填 ≤20000；`enabled=true` 时 markdown 不能为空（422）。
- 响应 `AnnouncementSettings`（启用时 `publishedAt` 单调递增）。
- 错误：401/403/422。
- 实现：`api/announcements.py:53-57`。

---

## 15. 管理/只读运维接口（客户端页面会调用）

### 15.1 `GET /api/v2/admin/settings`

- 鉴权：admin。
- 响应 `InstanceSettingsView`（`models.py:546-551`）：
  `registrationOpen:bool`、`oauthRegistrationOpen:bool`、`passwordResetEnabled:bool`、`oauth: OAuthProviderPublicConfig|null`、`email: EmailSettingsView`。
  `OAuthProviderPublicConfig`：`enabled`、`provider`、`label`、`authorizeUrl`、`tokenUrl`、`userInfoUrl`、`clientId`、`scopes`、`usernameClaim`、`subjectClaim`、`emailClaim`、`nameClaim`。
  `EmailSettingsView`：`enabled:bool`、`fromAddress:string`、`apiKeyConfigured:bool`（不返回 apiKey 本身）。
- 实现：`api/admin.py:29-31,149-156`。

### 15.2 `PATCH /api/v2/admin/settings`

- 鉴权：admin。
- Body `InstanceSettingsUpdateRequest`（`models.py:554-559`）：`registrationOpen:bool|null`、`oauthRegistrationOpen:bool|null`、`passwordResetEnabled:bool|null`、`oauth: OAuthProviderConfigUpdate|null`（含 `clientSecret:string|null`）、`email: EmailSettingsUpdate|null`（`enabled/fromAddress/apiKey/clearApiKey`）。
- 响应 `InstanceSettingsView`。
- 错误：401/403/422。
- 实现：`api/admin.py:34-52`。

### 15.3 `GET /api/v2/admin/users`

- 鉴权：admin。
- Query：`limit:int|null`（1..1000；省略时返回全部）、`offset:int=0`（≥0）。
- 响应 `AdminUserListResponse`：`users: UserView[]`、`total:int|null`（实现总是填总数）、`serverTime`。
- 实现：`api/admin.py:58-69`。

### 15.4 `POST /api/v2/admin/users`

- 鉴权：admin。
- Body `AdminUserCreateRequest`：`email` ≤320、`displayName` 1..64、`code:string|null` ≤6、`password`、`passwordVerifier`、`passwordSalt`、`role:"admin"|"member"="member"`、`skipEmailVerification:bool=false`。
- 响应 **201** `UserView`：`userId`、`email:string|null`、`emailVerified:bool`、`displayName`、`role`、`disabled:bool`、`avatar:string|null`、`createdAt`、`updatedAt`。
- 错误：409（已存在）、422、401/403。
- 实现：`api/admin.py:72-92`。

### 15.5 `PATCH /api/v2/admin/users/{user_id}`

- 鉴权：admin。
- Body `AdminUserUpdateRequest`：`displayName:string|null` ≤64、`role|null`、`disabled:bool|null`、`password`、`passwordVerifier`、`passwordSalt`。
- 响应 `UserView`。
- 错误：404、409（不能改自己的 role/禁用自己、非法变更）、422。
- 实现：`api/admin.py:95-129`。

### 15.6 `DELETE /api/v2/admin/users/{user_id}`

- 鉴权：admin。
- 成功：**204 空体**。
- 错误：404、409（不能删除自己）。
- 实现：`api/admin.py:132-146`。

### 15.7 `GET /api/v2/admin/usage/report`

- 鉴权：admin。
- Query（alias）：`from: date|null`、`to: date|null`、`tz:string="UTC"`。缺省 `to=今天(tz)`、`from=to-29 天`；范围最多 366 天，`from<=to`。
- 响应 `UsageReportResponse`（`models.py:507-516`）：
  `range: UsageRange`（`fromDate,toDate,timezone,dataSince:string|null,generatedAt`）、
  `summary: UsageSummary`（`activeUsers,newUsers,avgDau,wau,mau,totalUsers,messages,sessions,sessionViews,sessionsViewed,sessionsReadOnly,turns,turnFailureRate:float|null,turnDurationMs:UsagePercentiles,approvalsRequested,approvalsResolved,approvalWaitMs, runtimeErrors,connectorsOnline,totalConnectors`）、
  `daily: UsageDailyPoint[]`、
  `hourOfDay: UsageHourPoint[]`、
  `users: UsageUsersSection`（`segments,messageHistogram,retention`）、
  `runtimes: UsageRuntimeRow[]`、
  `devices: UsageDevicesSection`、
  `quality: UsageQualitySection`、
  `serverTime`。
  百分位对象 `UsagePercentiles`：`p50/p90/p99:int|null`、`samples:int`。
- 错误：422（时区非法/日期范围非法）、401/403。
- 实现：`api/admin_usage.py:22-39`；模型 `models.py:361-516`。

### 15.8 `GET /api/v2/admin/usage/live`

- 鉴权：admin。
- 响应 `UsageLiveResponse`：`onlineConnectors:int`、`totalConnectors:int`、`runningTurns:int`、`activeUsers15m:int`、`serverTime`。
- 实现：`api/admin_usage.py:42-47`。

### 15.9 `GET /api/v2/admin/service`

- 鉴权：admin。
- 响应 `ServiceInfoResponse`（`models.py:320-330`）：`endpoint`、`version`、`database:"PostgreSQL"`、`databasePath:string|null`（host:port/dbname，不含凭证）、`databaseSchemaVersion`、`coordination:"memory"|"redis"`、`instanceId`、`startedAt`、`uptimeSeconds:int`、`serverTime`。
- 实现：`api/service.py:17-47`。

---

## 16. 健康检查

实现：`app.py:311-356`。

### 16.1 `GET /api/v2/health` 与 `GET /api/v2/health/live`

- 鉴权：无。同一 handler。
- 响应：`{"status":"ok","version":"2.1.0","serverTime":"<ISO>"}`。
- 实现：`app.py:311-314`。

### 16.2 `GET /api/v2/health/ready`

- 鉴权：无。
- 响应 200 或 503：
  ```json
  {"status":"ready"|"not_ready",
   "checks":{
     "database":{"status":"ok","schemaVersion":"..."} | {"status":"error","message":"..."},
     "redis":{"status":"ok"} | {"status":"error","message":"..."} | {"status":"not_configured"},
     "realtime":{"status":"ok"} | {"status":"error","message":"subscription unavailable"}
   },
   "instanceId":"srv_...",
   "serverTime":"<ISO>"}
  ```
- 实现：`app.py:316-356`。

---

## 17. 仅 connector / 服务端内部调用（不面向客户端）

实现：`server/agent_server/api/connector_ingress.py`。这些路由不对 Web/Android/iOS 暴露，Go 实现时只需保持内部契约。

| 方法 | 路径 | 鉴权 | 用途 |
|---|---|---|---|
| `POST` | `/api/v2/connector/auth` | `Authorization: Connector <connectorId>:<connectorToken>` | 换取 900 秒 Bearer connector access token；响应 `{accessToken, expiresIn}`（`connector_ingress.py:410-424`）。 |
| `POST` | `/api/v2/connector/ingest` | Bearer connector access token | 批量通知上报。Body `{notifications:[{method:string,params:object}]}`（至少 1）；响应 `{accepted:int,rejected:[{index,method,code,message,errorType|null}],serverTime}`；400 为通知校验错误（`connector_ingress.py:431-447`）。 |
| `GET` | `/api/v2/connector/sessions/{session_id}/attachments/{file_id}/content` | Bearer connector access token | connector 下载用户上传附件；二进制 + `X-File-Name`、`X-File-Sha256`；404（`connector_ingress.py:450-486`）。 |
| `PUT` | `/api/v2/connector/fs/transfers/{transfer_id}` | Bearer connector access token + `?token=<transfer token>` | connector 上传待转发文件；请求体为原始字节流；响应 `{"status":"accepted"}`；404（`connector_ingress.py:489-510`）。 |
| `WS` | `/api/v2/connector/ws` | Bearer connector access token | connector 主长连接。消息只有 `{"type":"response",...}`（RPC 响应）和 `{"type":"notification",...}`（通知）；close 码 `1008`（token 无效/被撤销）、`4409`（重复连接或所有权丢失）。完整 RPC/notification schema 属于 connector 协议，**UNKNOWN in this file**（检查了 `connector_ingress.py:520-718,930-942`，协议细节在 `connector/` 与 `contracts/`）。 |
| `WS` | `/api/v2/connector/terminals/{terminal_id}/relay` | `?token=<terminal relay_token>` | connector 的终端 relay 通道；token 不匹配/terminal 不存在 close `1008`（`connector_ingress.py:823+`）。消息 schema 属于 relay 协议，**UNKNOWN in this file**。 |

其它“内部/非客户端”项：

- `GET /api/v2/sessions/local/{session_id}/{file_id}`：签名 token 下载，只作为 `/attachments/{id}/open` 的 302 目标（见 8.1.4）。
- `GET /api/v2/agents/{runtime}/model-catalog`、`GET /api/v2/agents/{runtime}/permission-catalog`：已移除路由，鉴权后恒返回 410（`api/agents.py:26-71`，`include_in_schema=False`）。`runtime` 仅接受 `codex|claude|opencode|acp|dsh`（Pydantic 校验，否则 422）。
- `GET /api/v2/connectors/{connector_id}/protocol/capabilities`：已移除路由，410（见 6.4）。

---

## 18. 当前客户端未使用的产品路由（仍属 API）

以下路由存在且可用，但在 `web-next/src`、`android/`、`ios/` 的 API 层没有调用点：

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST/GET/PATCH/DELETE` | `/api/v2/sessions/{session_id}/terminals*` | legacy 会话终端（见 9.3）。 |
| `WS` | `/api/v2/sessions/{session_id}/terminals/{terminal_id}/stream` | legacy 会话终端流（见 9.3）。 |
| `GET` | `/api/v2/agents/{runtime}/*-catalog` | 410，见第 17 节。 |

### 18.1 客户端仍在调用、但当前服务端已移除的路由

这些路径在 `web-next/src/features/dashboard/api.ts` 中仍有调用函数，但 2026-10-24 17:46 的 `api/connector_terminal.py` 已不再注册它们，当前会返回 404：

| 方法 | 路径（前缀 `/api/v2`） | Web 客户端位置 |
|---|---|---|
| `PATCH` | `/connectors/{connector_id}/terminals-v2/{terminal_id}/persistence` | `api.ts:599-610` |
| `POST` | `/connectors/{connector_id}/terminals-v2/{terminal_id}/resize` | `api.ts:626-638` |
| `POST` | `/connectors/{connector_id}/terminals-v2/{terminal_id}/write` | `api.ts:640-651` |
| `GET` | `/connectors/{connector_id}/terminals-v2/{terminal_id}/snapshot` | `api.ts:653-663` |

另外，`api/connector_shell.py` 与 `services/shell_tasks.py` 已整体删除；如果 Go 目标仍需 shell 能力，需要从历史版本恢复契约，本规格不再覆盖其细节。

---

## 19. Connector Runtime 管理（客户端会调用，补充细节）

实现：`server/agent_server/api/connector_runtimes.py`；模型 `core/device_runtime.py:39-251`。

统一鉴权：Bearer user token；connector owner。错误映射：`DeviceRuntimeError.status_code`（404/409/422/502/503），body 为 `{"detail": {code,message}}`。

### 19.1 `GET /api/v2/connectors/{connector_id}/runtimes`

- 响应 `DeviceRuntimeListResponse`：`connectorId`、`runtimes: DeviceRuntimeView[]`、`serverTime`。
- 实现：`connector_runtimes.py:44-61`。

### 19.2 `POST /api/v2/connectors/{connector_id}/runtimes/discover`

- Body：无。响应同上。
- 实现：`connector_runtimes.py:64-81`。

### 19.3 `GET /api/v2/connectors/{connector_id}/runtime-types`

- 响应 `RuntimeTypeListResponse`：`connectorId`、`runtimeTypes: RuntimeTypeView[]`、`serverTime`。
- 实现：`connector_runtimes.py:84-104`。

### 19.4 `POST /api/v2/connectors/{connector_id}/runtime-types/discover`

- Body：无。响应同上。
- 实现：`connector_runtimes.py:107-127`。

### 19.5 `POST /api/v2/connectors/{connector_id}/runtimes`

- Body `RuntimeInstanceCreateRequest`（`extra="forbid"`）：`runtimeType:string`、`name:string`、`config:object` 均必填；`active:bool=false`。
- 响应 **201** `DeviceRuntimeView`（裸对象，不包裹）。
- 实现：`connector_runtimes.py:130-151`。

### 19.6 `GET /api/v2/connectors/{connector_id}/runtimes/{runtime_id}`

- 响应 `DeviceRuntimeView`。
- 实现：`connector_runtimes.py:154-171`。

### 19.7 `PATCH /api/v2/connectors/{connector_id}/runtimes/{runtime_id}`

- Body `RuntimeInstancePatchRequest`：`name:string` 必填。
- 响应 `DeviceRuntimeView`。
- 实现：`connector_runtimes.py:174-193`。

### 19.8 `PUT /api/v2/connectors/{connector_id}/runtimes/{runtime_id}/config`

- Body `RuntimeConfigPutRequest`：`config:object={}`。
- 响应 `DeviceRuntimeView`。
- 错误：422 时 detail 可能包含 `issues:[{path,message,validator|null}]`（`RuntimeConfigValidationError`）。
- 实现：`connector_runtimes.py:196-215`。

### 19.9 `PUT /api/v2/connectors/{connector_id}/runtimes/{runtime_id}/active`

- Body `RuntimeActivePutRequest`：`active:bool` 必填。
- 响应 `DeviceRuntimeView`。
- 实现：`connector_runtimes.py:218-237`。

### 19.10 `DELETE /api/v2/connectors/{connector_id}/runtimes/{runtime_id}/config`

- Body：无。删除配置并返回最终未配置状态的 `DeviceRuntimeView`。
- 实现：`connector_runtimes.py:240-258`。

### 19.11 `GET /api/v2/connectors/{connector_id}/runtimes/{runtime_id}/capabilities`

- 响应 `ProtocolCapabilitiesResponse`。
- 错误：404、409、422、502、503、504。
- 实现：`connector_runtimes.py:261-291`。

### 19.12 `GET /api/v2/connectors/{connector_id}/runtimes/{runtime_id}/catalogs/model`

- 响应 `ProtocolModelCatalogResponse`。
- 实现：`connector_runtimes.py:294-322`。

### 19.13 `GET /api/v2/connectors/{connector_id}/runtimes/{runtime_id}/catalogs/permission`

- 响应 `ProtocolPermissionCatalogResponse`。
- 实现：`connector_runtimes.py:325-353`。

### 19.14 `GET /api/v2/connectors/{connector_id}/runtimes/{runtime_id}/commands`

- 响应 `RuntimeCommandListResponse`：`commands: RuntimeCommandView[]`、`serverTime`。
- 实现：`connector_runtimes.py:356-382`。

---

## 20. 完整路由清单（142 条）

图例：鉴权 `U`=user Bearer，`A`=admin，`N`=无，`C`=connector token，`S`=signed token/ticket。客户端列表示在代码中找到调用点。

| # | 方法 | 路径（前缀 `/api/v2`） | 鉴权 | 客户端 | 实现 |
|---:|---|---|---|---|---|
| 1 | GET | `/health` | N | 全部 | app.py:311 |
| 2 | GET | `/health/live` | N | Android | app.py:312 |
| 3 | GET | `/health/ready` | N | — | app.py:316 |
| 4 | GET | `/auth/config` | N | Web/Android/iOS | api/auth.py:81 |
| 5 | POST | `/auth/password-salt` | N | Web | api/auth.py:114 |
| 6 | POST | `/auth/register` | N | Web | api/auth.py:126 |
| 7 | POST | `/auth/login` | N | Web | api/auth.py:182 |
| 8 | POST | `/auth/email-code` | 可选 U | Web/Android/iOS | api/auth.py:195 |
| 9 | PUT | `/auth/me/email` | U | Web/Android/iOS | api/auth.py:240 |
| 10 | PUT | `/auth/me/profile` | U | Web/Android/iOS | api/auth.py:254 |
| 11 | GET | `/auth/oauth/start` | N | Web | api/auth.py:265 |
| 12 | GET | `/auth/oauth/callback` | N | 浏览器 | api/auth.py:283 |
| 13 | POST | `/auth/oauth/finalize` | N | Web | api/auth.py:342 |
| 14 | GET | `/auth/me` | U | Web/Android/iOS | api/auth.py:385 |
| 15 | PUT | `/auth/me/avatar` | U | Web/Android/iOS | api/auth.py:396 |
| 16 | DELETE | `/auth/me/avatar` | U | Web/Android/iOS | api/auth.py:414 |
| 17 | POST | `/auth/change-password` | U 或 email+code | Web/Android/iOS | api/auth.py:423 |
| 18 | POST | `/auth/mobile-login/qr` | U | Web | api/auth.py:452 |
| 19 | POST | `/auth/mobile-login/exchange` | N | Android | api/auth.py:481 |
| 20 | POST | `/auth/mobile-login/refresh` | N | Android | api/auth.py:514 |
| 21 | POST | `/auth/mobile-login/request` | N | Android | api/auth.py:534 |
| 22 | POST | `/auth/mobile-login/status` | N | Web/Android | api/auth.py:552 |
| 23 | POST | `/auth/mobile-login/confirm` | U | Web | api/auth.py:563 |
| 24 | GET | `/.well-known/oauth-authorization-server` | N | — | api/oauth.py:26 |
| 25 | GET | `/oauth/authorize` | U | Web | api/oauth.py:39 |
| 26 | POST | `/oauth/authorize` | U | Web | api/oauth.py:65 |
| 27 | POST | `/oauth/token` | N | Android | api/oauth.py:123 |
| 28 | POST | `/pairing/start` | N | Web | api/pairing.py:21 |
| 29 | POST | `/pairing/claim` | U | Web/Android/iOS | api/pairing.py:32 |
| 30 | POST | `/pairing/poll` | N | Web | api/pairing.py:55 |
| 31 | POST | `/ws-ticket` | U | Web/Android/iOS | api/client_ws.py:21 |
| 32 | GET | `/connectors` | U | 全部 | api/connectors.py:46 |
| 33 | POST | `/connectors` | U | 全部 | api/connectors.py:59 |
| 34 | GET | `/connectors/{connector_id}` | U | 全部 | api/connectors.py:87 |
| 35 | GET | `/connectors/{connector_id}/protocol/capabilities` | U | 旧客户端 | api/connectors.py:106 |
| 36 | PATCH | `/connectors/{connector_id}` | U | 全部 | api/connectors.py:137 |
| 37 | DELETE | `/connectors/{connector_id}` | U | 全部 | api/connectors.py:165 |
| 38 | POST | `/connectors/{connector_id}/revoke` | U | 全部 | api/connectors.py:189 |
| 39 | GET | `/connectors/{connector_id}/preferences` | U | iOS | api/connectors.py:223 |
| 40 | POST | `/connectors/{connector_id}/sessions/archive-all` | U | Web/Android/iOS | api/connectors.py:243 |
| 41 | GET | `/connectors/{connector_id}/runtimes` | U | Web/Android/iOS | connector_runtimes.py:44 |
| 42 | POST | `/connectors/{connector_id}/runtimes/discover` | U | Web/Android/iOS | connector_runtimes.py:64 |
| 43 | GET | `/connectors/{connector_id}/runtime-types` | U | Web/Android/iOS | connector_runtimes.py:84 |
| 44 | POST | `/connectors/{connector_id}/runtime-types/discover` | U | Web/Android/iOS | connector_runtimes.py:107 |
| 45 | POST | `/connectors/{connector_id}/runtimes` | U | Web/Android/iOS | connector_runtimes.py:130 |
| 46 | GET | `/connectors/{connector_id}/runtimes/{runtime_id}` | U | iOS/Web | connector_runtimes.py:154 |
| 47 | PATCH | `/connectors/{connector_id}/runtimes/{runtime_id}` | U | iOS/Web | connector_runtimes.py:174 |
| 48 | PUT | `/connectors/{connector_id}/runtimes/{runtime_id}/config` | U | Web/Android/iOS | connector_runtimes.py:196 |
| 49 | PUT | `/connectors/{connector_id}/runtimes/{runtime_id}/active` | U | Web/Android/iOS | connector_runtimes.py:218 |
| 50 | DELETE | `/connectors/{connector_id}/runtimes/{runtime_id}/config` | U | Web/Android/iOS | connector_runtimes.py:240 |
| 51 | GET | `/connectors/{connector_id}/runtimes/{runtime_id}/capabilities` | U | Web/Android/iOS | connector_runtimes.py:261 |
| 52 | GET | `/connectors/{connector_id}/runtimes/{runtime_id}/catalogs/model` | U | Web/Android/iOS | connector_runtimes.py:294 |
| 53 | GET | `/connectors/{connector_id}/runtimes/{runtime_id}/catalogs/permission` | U | Web/Android/iOS | connector_runtimes.py:325 |
| 54 | GET | `/connectors/{connector_id}/runtimes/{runtime_id}/commands` | U | iOS/Web | connector_runtimes.py:356 |
| 55 | POST | `/connectors/{connector_id}/fs/list` | U | Web/Android/iOS | connector_files.py:57 |
| 56 | POST | `/connectors/{connector_id}/fs/read` | U | iOS | connector_files.py:94 |
| 57 | POST | `/connectors/{connector_id}/fs/preview-token` | U | Web/iOS | connector_files.py:128 |
| 58 | POST | `/connectors/fs/preview-session` | N（signed） | Web | connector_files.py:166 |
| 59 | POST | `/connectors/fs/preview/readText` | N（signed） | Web | connector_files.py:214 |
| 60 | POST | `/connectors/fs/preview/read` | N（signed） | Web | connector_files.py:242 |
| 61 | GET | `/connectors/{connector_id}/fs/transfers/{transfer_id}` | U 或 signed | Web/iOS | connector_files.py:283 |
| 62 | POST | `/connectors/{connector_id}/fs/readText` | U | Web/Android/iOS | connector_files.py:336 |
| 63 | POST | `/connectors/{connector_id}/fs/write` | U | Web | connector_files.py:362 |
| 64 | POST | `/connectors/{connector_id}/terminals` | U | Web（旧面板） | connector_terminal.py:99 |
| 65 | POST | `/connectors/{connector_id}/terminals-v2` | U | Web/Android | connector_terminal.py:116 |
| 66 | GET | `/connectors/{connector_id}/terminals` | U | Web（旧面板） | connector_terminal.py:172 |
| 67 | GET | `/connectors/{connector_id}/terminals-v2` | U | Web/Android | connector_terminal.py:186 |
| 68 | PATCH | `/connectors/{connector_id}/terminals/{terminal_id}` | U | Web（旧面板） | connector_terminal.py:234 |
| 69 | PATCH | `/connectors/{connector_id}/terminals-v2/{terminal_id}` | U | Web | connector_terminal.py:254 |
| 70 | DELETE | `/connectors/{connector_id}/terminals/{terminal_id}` | U | Web（旧面板） | connector_terminal.py:293 |
| 71 | DELETE | `/connectors/{connector_id}/terminals-v2/{terminal_id}` | U | Web/Android | connector_terminal.py:311 |
| 72 | POST | `/connectors/{connector_id}/terminals/{terminal_id}/resize` | U | Web（旧面板） | connector_terminal.py:342 |
| 73 | WS | `/connectors/{connector_id}/terminals-v2/{terminal_id}/stream`（WS） | U（query/header） | Web/Android | connector_terminal.py:363 |
| 74 | WS | `/connectors/{connector_id}/terminals/{terminal_id}/stream`（WS） | U（query/header） | — | connector_terminal.py:489 |
| 75 | POST | `/connector/auth` | C | connector | connector_ingress.py:410 |
| 76 | POST | `/connector/ingest` | C | connector | connector_ingress.py:431 |
| 77 | GET | `/connector/sessions/{session_id}/attachments/{file_id}/content` | C | connector | connector_ingress.py:450 |
| 78 | PUT | `/connector/fs/transfers/{transfer_id}` | C | connector | connector_ingress.py:489 |
| 79 | WS | `/connector/ws` | C | connector | connector_ingress.py:520 |
| 80 | WS | `/connector/terminals/{terminal_id}/relay` | relay token | connector | connector_ingress.py:823 |
| 81 | GET | `/sessions` | U | 全部 | sessions.py:417 |
| 82 | POST | `/sessions` | U | iOS/Web | sessions.py:347 |
| 83 | POST | `/sessions/create-and-start` | U | Web/Android/iOS | sessions.py:377 |
| 84 | GET | `/sessions/list` | U | Web/Android/iOS | sessions.py:460 |
| 85 | GET | `/sessions/{session_id}/meta` | U | 全部 | sessions.py:478 |
| 86 | PATCH | `/sessions/{session_id}/meta` | U | 全部 | sessions.py:500 |
| 87 | POST | `/sessions/archive` | U | 全部 | sessions.py:596 |
| 88 | POST | `/sessions/unarchive` | U | 全部 | sessions.py:614 |
| 89 | POST | `/sessions/read` | U | 全部 | sessions.py:632 |
| 90 | GET | `/sessions/{session_id}/runtime/state` | U | Web/Android/iOS | sessions.py:673 |
| 91 | GET | `/sessions/{session_id}/runtime/capabilities` | U | Web/Android/iOS | sessions.py:697 |
| 92 | GET | `/sessions/{session_id}/runtime/catalogs/model` | U | Web/Android/iOS | sessions.py:724 |
| 93 | GET | `/sessions/{session_id}/runtime/catalogs/permission` | U | Web/Android/iOS | sessions.py:758 |
| 94 | PATCH | `/sessions/{session_id}/runtime/selections` | U | Web/Android/iOS | sessions.py:792 |
| 95 | GET | `/sessions/{session_id}/timeline` | U | 全部 | sessions.py:829 |
| 96 | GET | `/sessions/{session_id}/snapshot` | U | 全部 | sessions.py:886 |
| 97 | GET | `/sessions/{session_id}/events` | U | Web/Android/iOS | sessions.py:1022 |
| 98 | WS | `/sessions/{session_id}/ws`（WS） | S（ticket） | 全部 | sessions.py:1041 |
| 99 | POST | `/sessions/{session_id}/takeover` | U | 全部 | sessions.py:1151 |
| 100 | DELETE | `/sessions/{session_id}/takeover` | U | 全部 | sessions.py:1183 |
| 101 | GET | `/sessions/{session_id}/runtime/commands` | U | Web/Android/iOS | sessions.py:1242 |
| 102 | POST | `/sessions/{session_id}/runtime/commands` | U | Web/Android/iOS | sessions.py:1298 |
| 103 | POST | `/sessions/{session_id}/runtime/messages` | U | 全部 | sessions.py:1381 |
| 104 | POST | `/sessions/{session_id}/runtime/interrupt` | U | Web/Android/iOS | sessions.py:1416 |
| 105 | POST | `/sessions/{session_id}/runtime/steer` | U | Web/Android/iOS | sessions.py:1450 |
| 106 | GET | `/sessions/{session_id}/runtime/notices` | U | Web/Android/iOS | sessions.py:1489 |
| 107 | POST | `/sessions/{session_id}/runtime/notices/{notice_id}/respond` | U | Web/Android/iOS | sessions.py:1507 |
| 108 | POST | `/sessions/{session_id}/sync` | U | Web/iOS | sessions.py:1588 |
| 109 | GET | `/sessions/{session_id}/attachments/{file_id}` | U | Android/iOS | sessions_fs.py:31 |
| 110 | GET | `/sessions/{session_id}/attachments/{file_id}/open` | U 或 token | Web/Android/iOS | sessions_fs.py:51 |
| 111 | POST | `/sessions/{session_id}/attachments` | U | Web/Android/iOS | sessions_fs.py:106 |
| 112 | GET | `/sessions/local/{session_id}/{file_id}` | S | 内部 | sessions_fs.py:73 |
| 113 | POST | `/sessions/{session_id}/terminals` | U | — | sessions_terminal.py:47 |
| 114 | GET | `/sessions/{session_id}/terminals` | U | — | sessions_terminal.py:60 |
| 115 | PATCH | `/sessions/{session_id}/terminals/{terminal_id}` | U | — | sessions_terminal.py:72 |
| 116 | DELETE | `/sessions/{session_id}/terminals/{terminal_id}` | U | — | sessions_terminal.py:86 |
| 117 | POST | `/sessions/{session_id}/terminals/{terminal_id}/resize` | U | — | sessions_terminal.py:99 |
| 118 | WS | `/sessions/{session_id}/terminals/{terminal_id}/stream` | U（query） | — | sessions_terminal.py:116 |
| 119 | POST | `/sessions/{session_id}/shares` | U | Web/Android/iOS | shares.py:30 |
| 120 | GET | `/public/shares/{share_id}` | N | Web/Android/iOS | shares.py:105 |
| 121 | GET | `/public/shares/{share_id}/attachments/{file_id}` | N | Web | shares.py:130 |
| 122 | GET | `/announcement` | N | 全部 | announcements.py:30 |
| 123 | GET | `/admin/announcement` | A | Web | announcements.py:45 |
| 124 | PUT | `/admin/announcement` | A | Web | announcements.py:53 |
| 125 | GET | `/admin/settings` | A | Web | admin.py:29 |
| 126 | PATCH | `/admin/settings` | A | Web | admin.py:34 |
| 127 | GET | `/admin/users` | A | Web | admin.py:58 |
| 128 | POST | `/admin/users` | A | Web | admin.py:72 |
| 129 | PATCH | `/admin/users/{user_id}` | A | Web | admin.py:95 |
| 130 | DELETE | `/admin/users/{user_id}` | A | Web | admin.py:132 |
| 131 | GET | `/admin/usage/report` | A | Web | admin_usage.py:22 |
| 132 | GET | `/admin/usage/live` | A | Web | admin_usage.py:42 |
| 133 | GET | `/admin/service` | A | Web | service.py:17 |
| 134 | GET | `/projects` | U | 全部 | projects.py:40 |
| 135 | POST | `/projects` | U | Web/Android/iOS | projects.py:51 |
| 136 | PATCH | `/projects/{project_id}` | U | 全部 | projects.py:92 |
| 137 | DELETE | `/projects/{project_id}` | U | Web/Android/iOS | projects.py:129 |
| 138 | GET | `/projects/{project_id}/sessions` | U | 全部 | projects.py:155 |
| 139 | POST | `/projects/{project_id}/sessions/archive-all` | U | Web/Android/iOS | projects.py:195 |
| 140 | GET | `/agents/{runtime}/model-catalog` | U | — | agents.py:26 |
| 141 | GET | `/agents/{runtime}/permission-catalog` | U | — | agents.py:50 |
| 142 | WS | `/dashboard/ws` | S（ticket） | 全部 | dashboard_stream.py:93 |

> 计数说明：第 73/74/79/80/98/118/142 为 WebSocket 路由；其余为 HTTP。`GET /health` 与 `GET /health/live` 绑定同一个函数但算两条路由。
---

## 21. 仍需在 Go 实现中确认的点（`UNKNOWN` 汇总）

1. **connector RPC / notification payload schema**：`/connector/ws` 的 `response` / `notification` 消息内容，以及终端 relay 消息，未在 Python 服务端定义完整 schema；本规格只覆盖 HTTP/WS 外壳。检查位置：`api/connector_ingress.py:930-942,823-930`、`contracts/`、`connector/`。
2. **legacy 终端 WS 输出帧**：`/connectors/{id}/terminals/{tid}/stream` 与 `/sessions/{id}/terminals/{tid}/stream` 的二进制/JSON 输出帧由 `TerminalBroker` 与 connector 决定；Python 侧只定义了 `pong`。检查位置：`api/connector_terminal.py:489-564`、`api/sessions_terminal.py:116-214`、`infra/terminal_broker.py`。
3. **connector 文件系统结果细节**：`fs/list`、`fs/write`、`fs/prepareDownload` 的 `result` 由 connector 返回，Python 只透传或做最小追加；字段名以上游 connector 为准。检查位置：`api/connector_files.py:57-91,362-385`、`services/connector_files.py`。
4. **OAuth provider 返回的用户信息映射**：`usernameClaim/subjectClaim/emailClaim/nameClaim` 是实例配置；具体 provider 行为在运行时决定。检查位置：`services/oauth.py`、`core/oauth_clients.py`。
5. **`TerminalView.purpose`**：服务端 Pydantic 模型只允许 `"user"`；Web 客户端类型里还写着 `"primary_claude"`，但 Python 响应不会产生该值。以服务端为准。
