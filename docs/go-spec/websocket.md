# Agents Anywhere WebSocket 协议规格（面向 Go 实现）

本文只依据当前仓库代码整理，不描述“应该怎样”。所有结论均可在下列路径中找到对应实现：

- Server：`server/agent_server/api/*.py`、`server/agent_server/infra/*.py`、`server/agent_server/services/*.py`
- Connector：`connector/connector/server/*.py`、`connector/connector/local/*.py`
- 客户端：`web-next/src/`、`android/app/src/main/java/`、`ios/Agents Anywhere/Agents Anywhere/`
- 已有说明文档：`docs/api/realtime.md`（仅作交叉验证，冲突时以代码为准）

---

## 0. 覆盖清单

| # | 端点 | 代码实现 | 本文状态 |
|---|---|---|---|
| 1 | `WS /api/v2/sessions/{session_id}/ws` | `api/sessions.py:1041` | 已覆盖 |
| 2 | `WS /api/v2/connectors/{connector_id}/ws` | **不存在** | 已核实并说明（见 §6） |
| 3 | `WS /api/v2/connector/ws` | `api/connector_ingress.py:520` | 已覆盖 |
| 4 | `WS /api/v2/connectors/{connector_id}/terminals-v2/{terminal_id}/stream` | `api/connector_terminal.py:504` | 已覆盖 |
| 5 | `WS /api/v2/connectors/{connector_id}/terminals/{terminal_id}/stream` | `api/connector_terminal.py:630` | 已覆盖 |
| 6 | `WS /api/v2/sessions/{session_id}/terminals/{terminal_id}/stream` | `api/sessions_terminal.py:116` | 已覆盖 |
| 7 | `WS /api/v2/connector/terminals/{terminal_id}/relay` | `api/connector_ingress.py:823` | 已覆盖 |
| 附 | `WS /api/v2/dashboard/ws` | `api/dashboard_stream.py:93` | 已覆盖（dashboard 推送确实走 WS） |
| 附 | `POST /api/v2/ws-ticket` | `api/client_ws.py:21` | 已覆盖（所有客户端 WS ticket 的来源） |

核实“不存在”的方法：枚举了 `server/agent_server` 下全部 `@router.websocket`，核对 `app.py` 的 `include_router` 列表，并搜索了 web/android/ios 客户端代码与 `docs/api/realtime.md`、`docs/api/namespace.md`。`/api/v2/connector/ws` 是唯一的 connector 接入 WS；不存在按 connector id 区分路径的 `/connectors/{id}/ws`。

---

## 1. 通用约定

### 1.1 传输与帧格式

- 所有端点都只使用 **WebSocket 文本帧**，内容是 UTF-8 JSON 对象。代码中没有 `send_bytes` / 二进制协议。
- 除两个例外，帧本身就是一个 JSON 对象，没有统一的 `{type, payload}` 外壳：
  - 例外 A：session WS 的持久事件使用 `ProtocolEventEnvelope` 信封（`protocolVersion/eventId/sequence/cursor/type/sessionId/emittedAt/payload`）。
  - 例外 B：connector WS 使用 JSON-RPC 风格信封（`type=request/response/notification`）。
- 时间字符串统一为 UTC ISO-8601，`+00:00` 被替换成 `Z`（`core/utc.py`）。示例：`2026-10-24T08:00:00.123456Z`。
- JSON 数值上限：协议常量 `PROTOCOL_MAX_REVISION = 9007199254740991`（2^53-1）。`sequence`、`revision`、`updatedSeq` 都必须落在这个范围内。
- 服务端所有 WS 发送使用 Starlette `send_json` / `send_text`；文本帧中的 JSON 不保证对象 key 顺序。

### 1.2 鉴权方式总览

| 端点 | 鉴权位置 | 凭据 | 单次消费 | 有效期 |
|---|---|---|---|---|
| session WS | query `?ticket=` | `wst_...` WS ticket | **是** | 60s |
| dashboard WS | query `?ticket=` | `wst_...` WS ticket | **是** | 60s |
| connector WS | HTTP header `Authorization: Bearer <connector access token>` | connector access token | 否 | 默认 900s，由 connector 刷新 |
| connector terminal v2 stream | query `?token=` 或 `Authorization: Bearer` | 用户 access token | 否 | 用户 token 自带 `exp` |
| connector terminal v1 stream | 同上 | 用户 access token | 否 | 同上 |
| session terminal v1 stream | 同上 | 用户 access token | 否 | 同上 |
| terminal relay | query `?token=` | 每终端 `relay_token`（`secrets.token_urlsafe(32)`） | 否 | 跟随 terminal 记录（默认 24h，attach 模式下持续续期） |

**没有首帧鉴权**，也没有 `Sec-WebSocket-Protocol` 子协议鉴权。所有鉴权都在握手阶段完成；失败直接 close。

用户 access token 的校验是 `verify_user_access_token`（`core/auth.py`）：三段式 HMAC-SHA256，检查 `exp`，取 `sub` 作为 user_id。terminal 端点会先 `urllib.parse.unquote(token)` 再校验。

connector access token 的校验是 `verify_connector_access_token`（格式 `{connector_id}.{expires_at}.{sig}`，HMAC-SHA256，默认 900s）。WS 入口另外会查数据库确认 connector 未撤销/未删除。

### 1.3 WS ticket（`POST /api/v2/ws-ticket`）

请求需要用户 Bearer token。请求体：

```json
{
  "clientId": "web",
  "scope": { "sessionId": "ses_xxx" }
}
```

或：

```json
{
  "clientId": "web-dashboard",
  "scope": { "dashboard": true }
}
```

`scope` 必须“有且只有一个”字段（`ProtocolWsTicketScope` 校验），否则 422。`sessionId` 作用域会先 `get_session(sessionId, user_id)`，不存在返回 404。

响应：

```json
{ "ticket": "wst_xxx", "expiresAt": "2026-10-24T08:01:00Z", "serverTime": "..." }
```

实现细节（`infra/ws_tickets.py`）：

- ticket 原文 `wst_` + `token_urlsafe(32)`；服务端只存 `sha256(ticket)`。
- TTL 固定 60 秒（`ClientWsTicketManager(ttl_seconds=60.0)`），不可通过 API 配置。
- 本地模式：进程内 dict，`consume` 时 `pop` + 单调时钟过期检查 → **单次消费**。
- Redis 模式：`SET key value EX ceil(60)`，`consume` 用 `GETDEL` → **单次消费**；有效期以 Redis TTL 为准。
- 消费时还要求 ticket 的 `scope` 与端点要求完全一致（session id 或 `"dashboard"`），不一致返回 `None`。
- 因此：**Go 客户端每次重连都必须重新 POST /ws-ticket**，不能复用 ticket。

### 1.4 关闭码（全系统）

| code | 出现位置 | 含义 |
|---|---|---|
| 1000 | 客户端主动关闭；服务端未显式使用 | 正常关闭 |
| 1008 | session WS、dashboard WS、connector WS、relay | 策略拒绝：ticket 缺失/无效、session 不存在、connector token 无效/已撤销、relay token 无效、terminal 不存在、connector 不存在 |
| 1012 | session/dashboard（Redis 订阅中断）、connector WS（心跳超时、所有权丢失、服务端关闭）、connector RPC 关闭 socket | 服务重启/中断，应重连并恢复 |
| 1013 | session WS（事件准备容量耗尽）、terminal v2 hub（慢消费者溢出） | 服务繁忙/过载，应退避重连 |
| 4001 | connector WS | connector 被删除或 token 被撤销（connector 客户端据此停止重连） |
| 4401 | terminal stream（v1/v2） | 用户 access token 缺失或无效 |
| 4404 | terminal stream（v1/v2） | connector/session/terminal 不存在或不归当前用户所有 |
| 4409 | connector WS | 同一 connector 已有活动连接，或连接所有权在握手期间丢失 |
| 1006 | 客户端观察到的异常关闭 | 没有收到 close 帧（网络/进程崩溃）；客户端按可重连处理 |

未在代码中定义 4408/4410 等自定义码。

### 1.5 心跳总览

| 通道 | 心跳 | 超时/判定 |
|---|---|---|
| session WS | 服务端在队列 15s 无消息时发送 `{"type":"keepalive","serverTime":...}` | 服务端不主动超时；iOS 客户端 45s 无任何帧判死，每 15s 检查；Android OkHttp 20s ping；Web 无应用层 ping |
| dashboard WS | 同上，15s keepalive | 同上 |
| connector WS | connector 每 20s 发 `connector.heartbeat` notification（`heartbeatSeconds` 可配） | 服务端任何入站帧都刷新 `last_seen`；60s 无帧判定过期；watchdog 每 5s 扫描一次；Redis lease TTL 60s |
| terminal v1/v2 stream | 无服务端主动心跳；客户端可发 `{"type":"ping"}`，服务端回 `{"type":"pong"}` | 无超时；v2 hub 单帧发送超时 5s；v1 无发送超时 |
| terminal relay | 无应用层心跳 | attach 模式 connector 自动重连（0.5s→5s 退避）；服务端 relay lease 30s，每 10s 续期；丢失则服务端关闭 relay socket |

服务端代码中没有设置 Uvicorn/Starlette 的协议级 ping 参数；是否存在部署层默认值属于部署配置，仓库内 **UNKNOWN**。

---

## 2. `WS /api/v2/sessions/{session_id}/ws`（客户端会话流）

实现：`api/sessions.py:1041-1148`。

### 2.1 握手与鉴权

1. 读取 query `ticket`；缺失 → close 1008 `"missing ticket"`。
2. `tickets.consume(ticket, session_id=session_id)`；失败 → close 1008 `"invalid ticket"`。
3. 再次校验 `db.get_session(session_id, user_id=ticket.user_id)`；不存在 → close 1008 `"session not found"`。
4. `accept()`。
5. `broker.register(session_id)` 得到队列（`maxsize=256`）。
6. 在 `timeline_write_buffer.session_fence(session_id)` 下读取 `next_seq = db.get_session_seq(session_id)`。
7. 发送 `session.subscribed`。
8. 进入推送循环。

注意：**WS 不发送初始 snapshot**。初始状态（`session/state/timeline/.../eventCursor`）由 HTTP `GET /api/v2/sessions/{session_id}/snapshot`（`ProtocolSessionSnapshotResponse`）提供，WS 只负责 `session.subscribed` + 增量事件。服务端不要求先取 snapshot 再连 WS；Web 客户端是**并发**发起 snapshot 请求和 WS 连接，并在 snapshot 未就绪时缓冲 WS 事件、就绪后再应用（`web-next/src/components/session-detail.tsx:1128-1165`）。对应 snapshot 接口见 `api/sessions.py:886`。

### 2.2 事件信封

持久事件（`core/protocol.py:241`）：

```json
{
  "protocolVersion": "1.0",
  "eventId": "evt_123_ab12cd34ef56",
  "sequence": 123,
  "cursor": "seq:123",
  "type": "timeline.item_updated",
  "sessionId": "ses_xxx",
  "emittedAt": "2026-10-24T08:00:00.000000Z",
  "payload": { }
}
```

字段规则（`core/events.py`）：

- `eventId = "evt_" + sequence + "_" + sha256(json([type, payload], sort_keys=True, default=str)).hexdigest()[:12]`
- `cursor = "seq:" + sequence`
- `sequence` 是**会话级 revision**，不是“一条事件 +1”。同一个 `sequence` 可以对应多条事件（例如同一次变更里的 `session.meta.updated` + `runtime.capability.updated` + 多条 `timeline.item_*`）。
- `session.subscribed` 也使用同一信封；其 `sequence` 是当前 `get_session_seq()`，`payload = {"clientId": ticket.client_id, "eventCursor": "seq:N"}`。
- `keepalive` **不是**信封，而是裸对象 `{"type":"keepalive","serverTime":"..."}`，每 15s 队列空闲时发送。

### 2.3 服务端 → 客户端：全部消息类型

| `type` | `payload` | 触发条件 |
|---|---|---|
| `session.subscribed` | `{clientId, eventCursor}` | 连接建立后立即发送一次。`eventCursor` = 当时会话 seq |
| `timeline.item_created` | `{item: TimelineItem}` | 时间线 item 的 `revision <= 1`；来自 `timeline.itemUpsert` / `timeline.sync` / 会话失效通知 |
| `timeline.item_updated` | `{item: TimelineItem}` | 时间线 item 的 `revision > 1` |
| `timeline.snapshot` | `{items: [TimelineItem]}` | `replace_timeline_snapshot`（`timeline.sync` 且 `complete=true`）且变更 item 数 ≤ 100，`nextSeq > 0` |
| `runtime.state.updated` | `{state: SessionRuntimeState}` | 失效信封里带 `runtimeState`；`sequence` 取 `state.updatedSeq`，否则取信封 `nextSeq` |
| `session.meta.updated` | `{session: SessionView}` | 失效信封里带 `session`；`sequence` 取 `session.updatedSeq`，否则取 `nextSeq` |
| `runtime.capability.updated` | `{capabilitySet: ProtocolCapabilitySet}` | 失效信封里带 `capabilitySet`，且同一信封里有 `session` |
| `runtime.notice.snapshot` | `{notices: [Notice]}` | `noticesReset` 且 `nextSeq > 0` |
| `runtime.notice.updated` | `{notice: Notice}` | 非 reset 的逐条 notice；`sequence` 取 `notice.updatedSeq`，否则 `nextSeq` |
| `runtime.catalog.updated` | `{catalogType: "model"\|"permission", catalog: {...}}` | 失效信封里带 `catalogs` 且 `nextSeq > 0` |
| `session.refetch_required` | `{eventCursor}` | 实时消息过大被替换为 refetch 标记；或时间线变更超过 100 条无法内联推送。客户端应走 HTTP 恢复/重新拉 snapshot |
| `keepalive`（裸对象） | `{type:"keepalive", serverTime}` | 队列 15s 无消息 |

消息过滤/去重（服务端在发送前执行，Go 实现若要完全复刻行为需注意）：

1. `runtime.capability.updated`：按 capability 语义指纹去重，连续相同指纹不发送；A→B→A 会发送。指纹算法见 `core/events.py:capability_event_semantic_fingerprint`（对 `capabilitySet.capabilities` 每条 canonical JSON 排序后再 canonical JSON）。
2. “实时投影”类型 `runtime.catalog.updated`、`runtime.state.updated`、`session.meta.updated`：只抑制**相邻重复 eventId**；`runtime.catalog.updated` 的 key 还带 `catalogType`。
3. 其余事件：按 `eventId` 去重，窗口 1024 条（FIFO 淘汰）。

`TimelineItem` / `SessionView` / `SessionRuntimeState` / `Notice` 的字段见 `core/models.py`、`core/protocol.py:46-98`、`core/protocol.py:218-255`。关键字段：
`TimelineItem` 有 `id`、`type`、`revision`、`updatedSeq`、`source` 等；`SessionView` 有 `id/connectorId/runtime/status/updatedSeq/...`；`SessionRuntimeState` 有 `sessionId/runtime/status/selections/updatedSeq/...`。

### 2.4 客户端 → 服务端

**没有定义任何客户端消息。** `run_server_push_until_disconnect` 会持续调用 `websocket.receive()` 消费入站帧，只识别 `websocket.disconnect`，其他帧全部丢弃（`api/server_push_websocket.py:42-52`）。因此 Go 客户端不要依赖发送 ping 得到响应。

### 2.5 seq / cursor / 断线恢复

服务端侧：

- 当前游标 = `seq:{get_session_seq(session_id)}`；`get_session_seq` 是会话持久 revision。
- `session.subscribed.payload.eventCursor` 是订阅时游标。
- 恢复不走 WS，而是 HTTP：`GET /api/v2/sessions/{session_id}/events?after=seq:N`（`api/sessions.py:1022`，实现 `services/event_recovery.py`）。
- 恢复响应 `ProtocolEventRecoveryResponse`：

```json
{ "events": [ProtocolEventEnvelope...], "nextCursor": "seq:N", "snapshotRequired": false, "serverTime": "..." }
```

恢复算法（必须按此实现才能与现有客户端兼容）：

1. `after_sequence = parse_event_cursor(after)`；格式必须是 `seq:` + 无前导零的十进制，且 ≤ 2^53-1，否则 422。
2. `current_sequence = get_session_seq()`。
3. 若 `after_sequence > current_sequence` → `snapshotRequired=true`，`nextCursor=seq:current`。
4. 若 `after_sequence == current_sequence` → 返回两条事件（同一 sequence）：`session.meta.updated`（当前 session）与 `runtime.capability.updated`（当前 effective capabilitySet），`snapshotRequired=false`。
5. 若 `after_sequence < get_timeline_reset_seq()` → `snapshotRequired=true`，`nextCursor=seq:max(current, reset)`。
6. 读取 `after` 之后的 timeline（limit 500，`DEFAULT_RECOVERY_LIMIT`），最多重试 3 次直到前后 `get_session_seq` 稳定；不稳定 → `snapshotRequired=true`。
7. `has_more=true`（超过 500 条）→ `snapshotRequired=true`（不返回部分事件）。
8. 否则返回 `timeline_events_from_items(...)`；若 `session.updatedSeq > after`，追加同 sequence 的 `session.meta.updated` + `runtime.capability.updated`。
9. 事件按 `(sequence, eventId)` 排序；`nextCursor = seq:current_sequence`。

snapshot 与增量的关系：

- **初始/恢复 snapshot 是 HTTP 响应**（`GET .../snapshot`），包含 `timeline.items`、`timeline.nextSeq`、`eventCursor`；WS 不重发它。
- WS 里的 `timeline.snapshot` 是**增量流中的替换事件**（`items` 全量替换本地 timeline），只在内联推送可承受（≤100 条变更）时出现；超过阈值改为 `session.refetch_required`，由客户端走 HTTP 恢复。
- `timeline.item_created` / `timeline.item_updated` 都是 upsert；客户端按 item `id` + `revision` 合并，`updatedSeq` 与事件 `sequence` 对齐。
- 其它 `runtime.*` / `session.meta.updated` 事件是“当前状态投影”，不是持久事件日志；恢复时以 HTTP snapshot/`/events` 的结果为准。

`nextSeq` / `nextCursor` / `eventCursor` 的计算：

- session snapshot 的 `timeline.nextSeq` = 事务围栏内 `get_session_seq`。
- session snapshot 的 `eventCursor` = `"seq:" + timeline.nextSeq`（`api/sessions.py:1012`）。客户端用它初始化本地 cursor。
- 恢复的 `nextCursor` = 恢复结束时的 `get_session_seq`。
- 没有“事件条数”游标；只有会话 revision。

客户端重连实践（用于兼容性参考）：

- Web：socket close 后固定 1200ms 重连；重新申请 ticket；等 `session.subscribed` 后调用 `/events` 从当前 cursor 恢复；`snapshotRequired=true` 时重新拉 session snapshot。见 `web-next/src/components/session-detail.tsx:1090-1130`。
- Android：指数退避，最大 30s；`session.subscribed` 后分页恢复；`snapshotRequired` 时拉 snapshot。见 `feature/realtime/SessionRealtimeController.kt`。
- iOS：1/2/4/8/15s 退避；`session.subscribed` 后 `reconcile`；收到 `session.refetch_required` 或 `event.sequence > projection.sequence + 1` 时重新对账。见 `V2SessionRepository.swift`、`V2SessionProjection.swift`。
- 服务端并不因为“序号跳变”自动要求 snapshot；只有 timeline reset、has_more、游标超前、恢复期间 revision 不稳定才 `snapshotRequired=true`。

### 2.6 慢消费者、背压、溢出

- 每个 WS 订阅者一个 `asyncio.Queue(maxsize=256)`（`TimelineBroker.register`）。
- 队列满时：**丢弃最旧的一条**，再放入最新一条（`_fan_out_queues`）。不会因为慢消费者断开连接。
- 大消息处理：实时信封若字符长度 ≥ 1MiB/4 且 UTF-8 字节数 > 1MiB（`MAX_LIVE_MESSAGE_BYTES`），服务端把消息替换为 `{"sessionId","nextSeq","refetch":true}`，最终变成 `session.refetch_required`。
- 事件准备：
  - 仅当 `AGENT_SERVER_EVENT_WORKERS > 0`（默认 2）时启用进程池；
  - 阈值 `AGENT_SERVER_EVENT_THRESHOLD_BYTES` 默认 256 KiB；
  - 队列上限默认 32 条 / 32 MiB；
  - 超出容量或进程池不可用 → 对该 socket close 1013 `"server busy; reconnect to recover"`，客户端应重连并通过 `/events` 恢复。
- 发送阻塞：出站循环直接 `await websocket.send_json/send_text`，没有单条发送超时；断开由独立的 receive 任务检测。
- Redis 中断（分布式模式）：`TimelineBroker.connection_lost` 触发 close 1012 `"realtime interrupted; reconnect to recover"`。

---

## 3. `WS /api/v2/dashboard/ws`（Dashboard 推送）

实现：`api/dashboard_stream.py:93-165`。

### 3.1 鉴权

- query `ticket` 必须存在 → 否则 close 1008 `"missing ticket"`。
- `tickets.consume_scope(ticket, scope="dashboard")` 必须成功 → 否则 close 1008 `"invalid ticket"`。
- 单次消费、60s 有效，同 §1.3。
- 没有 session/owner 校验；ticket 本身绑定 user_id。

### 3.2 消息

服务端 → 客户端只有两种：

1. **`dashboard.snapshot`（完整快照）**

```json
{
  "type": "dashboard.snapshot",
  "connectors": [Connector...],
  "projects": [Project...],
  "sessions": [SessionView...],
  "runtimes": [DeviceRuntimeView...],
  "sessionPages": {
    "active":   { "hasMore": false, "nextCursor": null },
    "archived": { "hasMore": false, "nextCursor": null }
  },
  "serverTime": "..."
}
```

- 连接建立后**立即发送一次**。
- 之后收到 `dashboard.changed` 失效消息时，重新构建并发送完整快照（不是增量）。
- 构建时会做 `with_effective_connector_statuses`、`project_session_meta_for_dashboard` 等，可能触发 connector 在线状态查询。
- `sessionPages` 当前永远是 `hasMore=false / nextCursor=null`。

2. **`keepalive`**

```json
{ "type": "keepalive", "serverTime": "..." }
```

队列 15s 无消息时发送。

客户端 → 服务端：**无定义消息**，入站帧被忽略（同 `run_server_push_until_disconnect`）。

### 3.3 触发源与合并

- `services/dashboard_events.py:publish_dashboard_changed` 生成 `{"type":"dashboard.changed","serverTime":...}` 发布给该用户。`reason` 参数不会进入 payload。
- Broker 每个用户一个队列 `maxsize=256`；满时丢最旧。
- 发送循环收到 `dashboard.changed` 后，会**把队列里剩余消息全部取空，只保留最后一条**，然后只发一次新快照（合并抖动）。
- 任何非 `dashboard.changed` 或非法 JSON 都被忽略。

### 3.4 断线/恢复

- 无 seq/cursor，无增量恢复。语义就是“重连后重新拿完整快照”。
- Redis 订阅中断时 close 1012，客户端重连即可。
- Web 客户端：close 后 2s 重连；初次连接 2.5s 未收到快照则退化为 HTTP 拉取（`workspace-context.tsx:748-820`）。
- iOS/Android 同样把 `dashboard.snapshot` 当完整状态替换。

---

## 4. `WS /api/v2/connector/ws`（Connector 接入）

实现：`api/connector_ingress.py:520-725`，RPC 管理器 `infra/connector_rpc.py`，connector 侧 `connector/server/client.py` + `rpc.py`。

### 4.1 鉴权与握手

请求头：

```http
Authorization: Bearer <connector access token>
X-Device-OS: macos | windows | linux     # 可选
```

`X-Device-OS` 只接受这三个值，其他值归一化为 `None`。

服务端握手顺序（每一步失败都会关闭 socket）：

1. 从 token 前缀解析 connector_id（仅用于拿 lifecycle 锁）。
2. `async with db.connector_lifecycle(connector_id)` 串行化凭据/删除变更。
3. `_require_active_connector` 校验 token 与数据库状态；失败 close 1008 `"invalid connector access token"`。
4. `manager.register(connector_id, websocket, ready=False)`：
   - 已有同 id 的**活跃**连接或 draining 连接 → `DuplicateConnectorConnectionError` → close 4409 `"connector id already connected"`；
   - 已有心跳超时的旧连接 → 关闭旧 socket（1012），接管；
   - Redis 分布式模式用 lease key 抢占，抢占失败 → 4409。
5. 创建通知泵（`_ConnectorNotificationPump`），启动它。
6. `db.record_connector_connection(...)`；若 connector 已被撤销 → close 1008 `"connector was revoked"`。
7. `record_connector_activity`。
8. `accept()`。
9. `manager.mark_ready(connection)`；所有权丢失 → close 4409 `"connector ownership was lost"`。

connector 客户端的握手超时是 90s（`websockets.connect(open_timeout=90)`），因为服务端可能正在 draining 上一个连接。

没有应用层 hello/handshake 帧。`ProtocolHandshakeRequest` / `ProtocolHandshakeResponse` 模型在 server 与 connector 的 `protocol.py` 中都有定义，但仓库内**没有任何调用点**，属于未使用/预留定义；连接建立时不协商协议版本。session 事件信封里的 `protocolVersion` 默认固定为 `"1.0"`。

### 4.2 帧格式（JSON-RPC 风格）

所有帧都是文本 JSON，对象顶层必须有 `type`。

**服务端 → connector**：只有 `request` 和 close 帧。

```json
{
  "id": "rpc_<token_urlsafe(10)>",
  "type": "request",
  "method": "session.state",
  "params": { }
}
```

- `id`：服务端生成，connector 必须原样回传。
- 没有服务端心跳帧；没有服务端通知帧。
- 跨实例路由（Redis）内部信封 `{type,requestId,sourceInstanceId,connectionId,...}` **不会**出现在 connector WS 上；拥有连接的服务实例会把它转换成本地 `request` 帧。

**connector → 服务端**：`response` 和 `notification`。

```json
{ "id": "rpc_xxx", "type": "response", "ok": true, "result": { } }
```

```json
{ "id": "rpc_xxx", "type": "response", "ok": false,
  "error": { "code": "terminal_not_found", "message": "..." } }
```

```json
{ "type": "notification", "method": "session.state.updated", "params": { } }
```

- 未知 `type` 被忽略。
- 任何入站帧都会调用 `manager.touch` 刷新心跳/lease。
- 服务端不回 ACK；通知是否被业务接受，connector 无法从 WS 得知（错误只记录在 server 日志）。批量/需要回执的场景走 HTTP `POST /api/v2/connector/ingest`。

### 4.3 服务端 → connector 的全部消息

| 帧 | 字段 | 说明 |
|---|---|---|
| `request` | `id, type:"request", method, params` | 调用 connector RPC，见 §5 方法表 |
| close 1008 | `reason` | token 无效、connector 被撤销 |
| close 4409 | `reason` | 重复连接 / 所有权丢失 |
| close 1012 | `reason` | 心跳超时、所有权丢失、服务端关闭 |
| close 4001 | `reason` | connector 被删除/吊销（`_disconnect_local`） |

### 4.4 connector → 服务端的全部消息

| `type` | 字段 | 服务端行为 |
|---|---|---|
| `response` | `id, ok, result` 或 `id, ok:false, error:{code,message}` | `resolve_response` 用 `id` 匹配 pending future；`ok=true` 返回 `result`，否则抛 `ConnectorRpcError(code,message)`。匹配不到（超时后迟到）→ 丢弃 |
| `notification` | `method, params` | 入队到通知泵，按会话串行应用；见 §5.3 通知表 |

### 4.5 心跳、超时、重复连接

- connector 每 20s 发送 `connector.heartbeat`（`client.py:_heartbeat_loop`；`heartbeatSeconds` 默认 20）。
- 服务端对**任意**入站帧刷新 `last_seen_monotonic`，并在 Redis 模式刷新 60s lease。
- `_connector_presence_watchdog` 每 5s 调 `expire_stale()`；超过 60s 无帧：
  - 标记 invalidated、失败所有 pending RPC、关闭 socket 1012 `"connector heartbeat timed out"`；
  - 清理该连接创建的 ephemeral terminals；
  - 发布 dashboard 变更。
- `heartbeat_timeout_seconds` 默认 60（`ConnectorRpcManager` 构造参数，当前 `app.py` 未覆盖）。
- 重复连接：活跃连接存在 → 4409；旧连接仅心跳超时/已 draining → 接管。
- 断开时：`begin_drain` 将 lease 标记 draining（停止路由新 RPC），失败 pending 调用，最多等 10s 让已接收通知落库，然后 `unregister` 释放 lease。
- connector 客户端重连策略：`reconnectSeconds` 默认 3；遇到 4001 且 reason 含 invalid/revok/delet/credential 视为凭据永久失效并停止；1008 会重连（用于短期 access token 刷新）；4409 也会重连（等待服务端 drain 完）。

### 4.6 背压与并发

服务端入站通知泵（`_ConnectorNotificationPump`）：

- `asyncio.Queue()` **无界**；没有丢弃（除 `timeline.itemUpsert` 的合并）。
- 按 `sessionId` 分 lane；同一 lane 严格 FIFO；无 sessionId 的通知共用 `"connector"` lane。
- 不同 lane 可并发，最多 `AGENT_SERVER_NOTIFICATION_CONCURRENCY`（默认 4）个。
- `timeline.itemUpsert` 以 `item.id` 为 key 合并：排队中的旧版本被跳过后再执行（coalescing）。不会合并其他方法。
- 如果 session 对应的 runtime 未配置，`session.*`/`timeline.*` 通知会被直接丢弃并计数 `obsolete`（`services/runtime_ingress.py`）。
- 单条通知处理异常只记录日志，不关闭 WS。

服务端出站 RPC：

- 每个连接一个 `asyncio.Lock` 串行化发送；直接 `await websocket.send_json(...)`，没有发送超时。
- 发送抛 `RuntimeError/OSError` → `unregister`，调用方得到 `ConnectorOfflineError("connector disconnected")`。
- 单次 RPC 的等待超时由调用点指定，见 §5；超时不会取消 connector 侧执行（connector 的 dispatch 是独立 task，文档明确“never cancels slow dispatch work”）。

connector 侧出站：

- 一个发送队列 + 单个发送 worker（`ConnectorRpcChannel`），出站帧串行发送。
- 通知单帧上限 900 KiB（`CONNECTOR_WS_MAX_NOTIFICATION_BYTES`）；超限抛 `ConnectorWebSocketFrameTooLarge`，`send_backend_notification` 会回退到 HTTP ingest。响应帧没有大小上限。
- 入站请求在独立 task 中处理，不阻塞读循环；普通方法并发上限 8，关键方法 `interaction.respond` / `session.notices` 并发上限 2。
- 连接被清理后，旧 generation 的请求响应会被丢弃。

---

## 5. Connector ↔ Server RPC 方法表

### 5.1 传输与错误约定

- 方法名是扁平字符串（`runtime.start`、`session.state`、`terminal.write`、`fs.readText` …）。
- `params` 必须是 JSON 对象；connector 对缺失/类型错误抛异常。
- 成功响应：`{"id","type":"response","ok":true,"result":<任意 JSON>}`。
- 失败响应：`{"id","type":"response","ok":false,"error":{"code":<string>,"message":<string>}}`。
  - `code` 来自 `RuntimeProtocolError.code`，否则取异常类名（例如 `ValueError`、`KeyError`）。
  - 没有 `error.type` 字段（`type` 只出现在服务端跨实例 Redis 路由错误中，不在 WS 上）。
- 服务端把失败映射为 `ConnectorRpcError(code, message)`。
- 连接不在线 → `ConnectorOfflineError`；等待超时 → `TimeoutError`。上层 HTTP 端点按各自策略映射（常见：409 offline、502 upstream、504 timeout、422 invalid_config）。Go 实现如果只做 WS 层，应按这三个异常语义处理。
- **超时只存在于服务端等待侧**；connector 不设请求超时，会继续执行，迟到的 response 被丢弃。
- 各方法之间没有顺序保证；同一连接上可并发多个 request，响应可能乱序（靠 `id` 关联）。
- `session.sync` 是特例：connector 立即返回 `{accepted:true, background:true, ...}`，真正同步在后台执行。

### 5.2 方法表

“服务端超时”是当前 server 调用点传入的值（秒）；标 `—` 表示仓库内没有 server 调用点。

#### 5.2.1 Runtime 控制

| 方法 | params | result | 服务端超时 |
|---|---|---|---|
| `runtime.discover` | `{}`（必须为空；有参数 connector 报错） | `{runtimeTypes:[RuntimeTypeDescriptor...]}` | 90（`AGENT_SERVER_RUNTIME_RPC_TIMEOUT_SECONDS`，默认 90） |
| `runtime.configSchema` | `{runtime, runtimeId}` | `{configSchema:{runtime,revision,schema,uiSchema,defaults,metadata}, runtime, runtimeId}` | —（无 server 调用点，UNKNOWN 使用方） |
| `runtime.config` | `{runtime, runtimeId}` | `{runtimeId, running, config：null 或 {runtime,runtimeId?,revision,values,schema,uiSchema,metadata}, runtime, runtimeId}` | —（无 server 调用点） |
| `runtime.validateConfig` | `{runtime, runtimeId, name, config, configRevision}` | `{runtimeId, valid:true, runtime, runtimeId}` | 90 |
| `runtime.start` | `{runtime, runtimeId, name, config, configRevision}` | `{runtimeId, status, error?, phase?, runtime, runtimeId}` | 90 |
| `runtime.stop` | `{runtime, runtimeId}` | `{runtimeId, status:"stopped", runtime, runtimeId}` | 90 |
| `runtime.capabilities` | `{runtime, runtimeId}` | `{capabilitySet:{runtime?,runtimeId?,revision,sessionId?,connectorId?,capabilities:[...],metadata?}, runtime, runtimeId}` | 30 |
| `runtime.commands` | `{runtime, runtimeId, limit?}` | `{commands:[RuntimeCommand...], runtime, runtimeId}` | 30 |
| `runtime.modelCatalog` | `{runtime, runtimeId, query?, limit?}` | `{catalog:{runtime,runtimeId?,revision,models:[...]}, runtime, runtimeId}` | 30 |
| `runtime.permissionCatalog` | `{runtime, runtimeId, query?, limit?}` | `{catalog:{runtime,runtimeId?,revision,permissions:[...]}, runtime, runtimeId}` | 30 |

`RuntimeTypeDescriptor` 字段（`runtime_rpc_payloads.py`）：`runtimeType, displayName, description, available, reason, recommended, recommendationRank, implementationType, configSchema, capabilities, metadata, instancePolicy, maxInstances`。

`runtime.start` 的 `error` / `phase`：`error` 只在 `status=error` 时有意义；`phase` 是 `{code,message,retryable?}` 形式的进行中步骤（例如 `syncing`）。

#### 5.2.2 Session / Turn / Interaction

| 方法 | params | result | 服务端超时 |
|---|---|---|---|
| `session.discover` | `{runtime, runtimeId, limit?, cursor?, force?}` | `{sessions:[SessionMeta...], nextCursor:null, runtime, runtimeId}` | —（无 server 调用点） |
| `session.create` | `{runtime, runtimeId, sessionId, content, title?, cwd?, selections?, attachments?, clientMessageId?, runtimeOptions?}` | operation result（`{ok?,code?,message?,sourceState?,...}`） | 60 |
| `session.sync` | `{sessionId, runtime, runtimeId, externalSessionId?}` | 立即返回 `{accepted:true, background:true, sessionId, externalSessionId, runtime, runtimeId}`；真正的同步在后台执行并发通知（`sync_session_snapshot` 的返回值不经过 RPC 响应） | 60 |
| `session.state` | `{sessionId, runtime, runtimeId, externalSessionId?}` | `{state: SessionState|null, runtime, runtimeId}` | 10（`AGENT_SERVER_SESSION_RPC_TIMEOUT_SECONDS` 默认 10；另一处固定 10） |
| `session.capabilities` | `{sessionId, runtime, runtimeId, externalSessionId?}` | `{capabilitySet, runtime, runtimeId}` | 10 |
| `session.notices` | `{sessionId, runtime, runtimeId, externalSessionId?}` | `{notices:[SessionNoticePayload...], runtime, runtimeId}` | 10 |
| `session.selections.update` | `{sessionId, runtime, runtimeId, externalSessionId?, selections:{scope:selectionId\|null}}` | operation result | 30 |
| `session.commands` | `{sessionId, runtime, runtimeId, externalSessionId?, query?, limit?}` | `{commands:[RuntimeCommand...], runtime, runtimeId}` | 30 |
| `session.command.execute` | `{sessionId, runtime, runtimeId, externalSessionId?, command, raw?, args?}` | `{command, ok, code, message, result}` | 30 |
| `interaction.respond` | `{sessionId, runtime, runtimeId, externalSessionId?, noticeId, actionId, inputData?}` | operation result | 30 |
| `session.send_message` | `{sessionId, runtime, runtimeId, externalSessionId?, content, cwd?, selections?, attachments?, clientMessageId?}` | operation result | 30（默认） |
| `session.steer` | `{sessionId, runtime, runtimeId, externalSessionId?, content, attachments?, clientMessageId?}` | operation result | 30（默认） |
| `session.interrupt` | `{sessionId, runtime, runtimeId, reason?}` | operation result | 30（默认） |

补充：

- `attachments[]` 元素只能通过 `fileId` 引用，禁止 `contentBase64`（connector 会报错）；字段 `{fileId, name?, mediaType?, size?, sha256?}`。
- `selections` 是 `{scope: selectionId|null}`。
- `runtimeOptions` 最多 16 个字段。
- operation result 的公共形状：`{ok?:bool, code?:string, message?:string, sourceState?:{...}, ...业务字段}`；`session.create`/`send_message`/`steer`/`interrupt`/`selections.update`/`interaction.respond` 都走它。
- server 会检查 `session.command.execute` 的结果形状；不合法时返回 `command_outcome_unknown` 语义。

#### 5.2.3 Local capability：filesystem / shell / terminal

| 方法 | params | result | 服务端超时 |
|---|---|---|---|
| `fs.prepareDownload` | `{sessionId, root, path}` | `{path, name, size, sha256, mediaType}` | 30 |
| `fs.uploadPreparedDownload` | `{sessionId, transferId, token, uploadUrl, root, path}` | `{transferId, uploadStarted:true}`（异步真正 PUT 上传） | 10 |
| `fs.writeFile` | `{sessionId, root, path, content, encoding:"utf8"\|"utf-8", ifMatch?}` | `{path, encoding:"utf8", bytesWritten, sha256}` | 30 |
| `fs.readDir` | `{sessionId, root, path}` | `{path, entries:[{name,path,type:"file"\|"directory"\|"other",size}], truncated, targetPath, targetType}` | 30 |
| `fs.readText` | `{sessionId, root, path, maxBytes?}` | `{path, name, size, sha256, encoding:"utf8", content, truncated, binary}` | 30 |
| `shell.exec` | `{sessionId, root, cwd, command, timeoutMs}` | `{cwd, command, exitCode, timedOut, durationMs, stdout, stderr, stdoutTruncated, stderrTruncated}` | `min(timeoutMs/1000 + 5, 310)` |
| `shell.task.start` | `{taskId, sessionId, root, cwd, command, timeoutMs}` | `{taskId, sessionId, status:"running"}` | 10 |
| `shell.task.cancel` | `{taskId, sessionId}` | `{taskId, sessionId, cancelled:bool}` | 5 |
| `terminal.create` | `{terminalId, sessionId, root, cwd, shell, command?, args?, profile?, cols, rows, env?, label?, persistent?, outputTransport?}` | terminal view（见下） | 15 |
| `terminal.write` | `{terminalId, dataBase64}` | `{terminalId, bytesWritten}` | 10 |
| `terminal.resize` | `{terminalId, cols, rows}` | `{terminalId, cols, rows}`，终端已关闭时 `{terminalId, closed:true}` | 10 |
| `terminal.close` | `{terminalId}` | `{terminalId, closed:true}`（幂等） | 10（清理路径 5） |
| `terminal.rename` | `{terminalId, label}` | terminal view | 10 |
| `terminal.setPersistent` | `{terminalId, persistent}` | terminal view | 10 |
| `terminal.list` | `{sessionId?}` | `{terminals:[terminal view...]}` | 10 |
| `terminal.release` | `{terminalId}` | `{terminalId, released:true}` | —（无 server 调用点；connector relay 关闭时内部调用） |
| `terminal.snapshot` | `{terminalId, fromSeq?}` | `{terminal:{...}, baseSeq, seq, dataBase64, outputs:[{seq,dataBase64}]}` | —（无 server 调用点；v2 relay 内部用） |
| `terminal.relay.connect` | `{terminalId, sessionId, token, mode?: "attach"}` | `{terminalId, connecting:true}` | 15 |

terminal view（connector 本地）：

```json
{
  "terminalId": "...", "sessionId": "...", "label": "...", "purpose": "user",
  "pid": 1234, "cols": 80, "rows": 24, "cwd": "...", "shell": "...",
  "closed": false, "status": "running", "exitCode": null,
  "scrollbackBytes": 0, "scrollbackSeq": 0, "persistent": false,
  "createdAt": "2026-10-24T08:00:00Z"
}
```

`terminal.create` 的 `outputTransport: "relay"` 会把本地终端标记为 `relayOnly`：不通过 `terminal.output` 通知推送输出；输出留在本地 scrollback，等 relay 接入后经 relay 发送。v2 HTTP create 使用这个模式。

`fs.*` 的 `root` 语义：connector 用 `Path(root)` 解析；相对路径拼到 root 下。`maxBytes` 上限 4 MiB；`readDir` 最多 500 条。

`terminal.snapshot` 细节（`connector/local/terminal.py`）：

- `seq` 是该终端最后一个 chunk 序号；`baseSeq` 是 scrollback 中第一个块之前的序号。
- `outputs` 只包含 `seq > fromSeq` 的块。
- `dataBase64`：`include_scrollback=true`（默认）或 `fromSeq < baseSeq` 时返回完整 scrollback，否则为空字符串。

### 5.3 connector → server 通知方法表

通知帧：`{"type":"notification","method":"...","params":{...}}`。服务端没有逐条 ACK。

传输选择：`timeline.sync` **固定走 HTTP `POST /api/v2/connector/ingest`**，不会出现在 WS 上；其他通知优先走 WS，WS 发送失败或已有 pending ingest 时回退到 HTTP ingest。两种传输使用相同的 method/params，服务端处理逻辑一致（`connector/server/client.py:notification_requires_ingest`）。

| method | params 关键字段 | 服务端处理 |
|---|---|---|
| `connector.heartbeat` | `{}` | 刷新 connector 活动时间/lease |
| `connector.preferencesUpdated` | 任意 dict（本地偏好快照） | 存 connector preferences |
| `protocol.capabilitiesUpdated` | `{revision, capabilities:[{capabilityId,version?,scope?,runtime?,supported?,available?,allowed?,...}]}` | 校验为 `ProtocolCapabilitySet`，更新 connector 协议能力；变化时重算并推送 session 能力 |
| `runtime.capability.updated` | `{runtime, runtimeId?, sessionId?, revision, capabilities:[...], metadata?}` | 合并到 connector 能力集；无 `sessionId` 时视为 runtime 级并触发会话能力重算 |
| `runtime.catalog.updated` | `{runtime, runtimeId, catalogType:"model"\|"permission", catalog}` | 更新 catalog revision；同 revision 内容冲突 → 校验错误 |
| `session.meta.upsert` | `{sessionId, runtime, runtimeId?, externalSessionId?, title?, cwd?, lastActivityAt?, metadata?, sourceState?}` | upsert session meta；可创建新 session |
| `session.updated` | 同 `session.meta.upsert`（兼容名） | 同上；拒绝 legacy selection 字段 |
| `session.source.updated` | `{sessionId, externalSessionId?, runtime, runtimeId?, availability, reason?, observedAt?, observationOrigin?}` | 更新 source availability |
| `session.state.updated` | `{sessionId, runtime, runtimeId?, externalSessionId?, status, statusReason?, error?, selections?, metadata?}` | upsert `SessionRuntimeState`；`running` 会开 active run，`idle/error` 清除 |
| `session.turnEnded` | `{sessionId, runtime, runtimeId?, externalSessionId?, turnId?, outcome:"completed"\|"interrupted"\|"cancelled"\|"failed", metadata?}` | 记录 turn 结束、usage |
| `session.inventory.begin` | `{runtime, runtimeId?, scanToken}` | 开始 inventory（`scanToken` 16–128 字符） |
| `session.inventory.complete` | `{runtime, runtimeId?, scanToken, sessions:[...], complete?}` | 完成 inventory，批量 upsert |
| `timeline.sync` | `{sessionId, runtime, runtimeId?, externalSessionId?, items:[...], complete, metadata?, sourceObservedAt?}` | **connector 固定走 HTTP ingest，不走 WS**（`INGEST_ONLY_NOTIFICATION_METHODS`）；服务端 WS 也支持处理 |
| `timeline.itemUpsert` | `{sessionId, runtime?, runtimeId?, item:{id,type,revision,updatedSeq,source,...}}` | upsert 单条 timeline item；`turn.start/turn.end` 被拒绝 |
| `notice.upsert` | `{noticeId, sessionId, runtime, type, title, message?, severity?, status?, interactionType?, blocking?, responseRequired?, actions?, source?, context?, metadata?, runtimeId?}` | upsert interaction/notification notice |
| `runtime.error` | `{runtime, runtimeId?, code, message, sessionId?, externalSessionId?, details?}` | 记录 runtime 错误通知 |
| `runtime.statusChanged` | `{runtime, runtimeId, status, error?, phase?}` | 更新 runtime 实例状态（从 WS 直接处理，不经过普通 handler 表） |
| `shell.task.started` | `{taskId, sessionId, status:"running"}` | 标记 shell task running |
| `shell.task.completed` | `{taskId, sessionId, status, result?\|error?}` | 完成/失败/取消 shell task |
| `terminal.output` | `{terminalId, sessionId, seq, dataBase64}` | fanout 到 terminal v1 订阅者；同时喂给 v2 hub |
| `terminal.exited` | `{terminalId, sessionId, exitCode, reason}` | fanout `exit` 到 v1 订阅者；同时喂给 v2 hub |

明确被拒绝/不支持的旧通知：`approval.requested`、`protocol.modelCatalogUpdated`、`protocol.permissionCatalogUpdated`。未知 method 被静默忽略（返回空 effect）。

通知的服务端顺序保证：同一 `sessionId` 严格按到达顺序；不同 session 可并发。

---

## 6. `WS /api/v2/connectors/{connector_id}/ws` 的核实结论

**当前代码中不存在这个路由。** 核实范围：

- `server/agent_server/api/*.py` 中所有 `@router.websocket`：只有 §0 表中列出的 8 条（含 dashboard）。
- `app.py:361-382` 的 `include_router` 列表：`connectors.router` 里没有 WS 路由。
- `connector/connector/server/urls` 与 `client.py`：connector 只连接 `api_v2_path("/connector/ws")`。
- `web-next/src`、`android/app/src`、`ios/...`：客户端没有连接 `/connectors/{id}/ws`。
- `docs/api/realtime.md:186`、`docs/api/namespace.md:60`：只记录 `WS /api/v2/connector/ws`。

因此：

- connector 接入只有 `WS /api/v2/connector/ws` 一条；connector 身份来自 Bearer token，不在 URL 里。
- `/api/v2/connectors/{id}/...` 下只有用户侧的 HTTP 终端/文件接口和 §8/§9 的终端 WS。
- 是否存在“计划中的按 id 路由”属于产品计划，代码无法证明；本文不做推测。

---

## 7. `WS /api/v2/connectors/{connector_id}/terminals-v2/{terminal_id}/stream`

实现：`api/connector_terminal.py:504-627`。

### 7.1 鉴权与前置条件

1. query `token` 或 `Authorization: Bearer <token>`；都没有 → close 4401。
2. `verify_user_access_token(unquote(token))` 失败 → close 4401。
3. `require_owned_connector(connector_id, user_id, db)` 失败 → close 4404。
4. `accept()`。
5. `hub.attach(connector_id, terminal_id, websocket)`（先注册，后拿快照）。
6. 通过 `TerminalRelayService.request(...)` 向 connector 请求快照；这会确保 attach-mode relay 已建立（见 §10）。
7. 发送 `replay`（如果快照有 `dataBase64`）。
8. `hub.mark_ready(..., snapshot_seq=seq)`。
9. 如果快照 terminal 状态是 `exited`，发送 `exit`。
10. 进入 duplex 循环。

`fromSeq` query 参数：`int >= 0`，默认 0。传给 connector relay 的 `{"type":"snapshot","fromSeq":N}`。

终端来源要求：v2 流使用 attach-mode relay。终端可以是 v2 HTTP create（`terminal.create` + `outputTransport:"relay"`，首次流式连接时由 `TerminalRelayService.ensure` 注册为 `relay_mode="attach"`），也可以是本地已存在、尚未注册到 broker 的终端；如果 broker 里的终端是 v1 create 模式，`ensure` 的 `_validate` 会抛 404 `"terminal not found"`，WS 收到错误帧后结束。

### 7.2 服务端 → 客户端消息

| 消息 | 字段 | 触发 |
|---|---|---|
| `replay` | `{type:"replay", data:<base64>, seq:<int>}` | 只要快照的 `dataBase64` 是字符串（可能为空串）就立即发一次。`seq` 是 connector 快照的 `seq`，不是 `fromSeq`；如果 connector 没返回 int `seq`，回退为 `fromSeq` |
| `output` | `{type:"output", data:<base64>, seq:<int>}` | relay 转发 output 帧；hub 保证只发送 `seq > last_seq`，并更新 `last_seq` |
| `exit` | `{type:"exit", exitCode:<int|null>, reason:<string>}` | 快照为 exited，或 relay 转发 exit |
| `error` | `{type:"error", code:<int|string>, message:<string>}` | 快照请求失败，或 input/resize 无法发到 relay |
| `pong` | `{type:"pong"}` | 收到客户端 `{"type":"ping"}` |

注意：

- `replay` 是直接在 accept/snapshot 之后 `send_json` 的，**不经过** hub 队列；`mark_ready` 之后队列中的 output 才被冲刷。
- relay 转发给浏览器的 `replay` / `output` / `exit` / `error` 保持 connector 帧的字段（hub 只校验类型并做 seq 过滤）。
- `hub.publish_relay` 只转发 `output/replay/exit/error`，其他类型（`ready`、`response`）被服务端消费，不下发。

### 7.3 客户端 → 服务端消息

| 消息 | 字段 | 服务端行为 |
|---|---|---|
| `input` | `{type:"input", data:<base64 string>}` | `broker.send_to_connector(terminal_id, {"type":"input","data":...})`；失败发 `error`。非字符串 `data` 被忽略 |
| `resize` | `{type:"resize", cols:<int>, rows:<int>}` | 缺省 80/24；clamp 到 cols 1–500、rows 1–200；转发 `{"type":"resize","cols","rows"}`；失败发 `error` |
| `ping` | `{type:"ping"}` | 回 `{"type":"pong"}` |
| 其他 | — | 忽略 |

`input` / `resize` 是 **fire-and-forget**：没有 `requestId`，服务端不等 connector 回执。

### 7.4 恢复与断线

- 客户端用 query `fromSeq` 请求重放；服务端/connector 的 replay 是**整段 scrollback**（当前实现 `snapshot` 默认 `include_scrollback=true`），客户端收到 replay 应重置终端缓冲再写入。
- 序号单调：connector 每个输出 chunk 一个递增 `seq`；hub 对 `seq <= last_seq` 的 output/replay 直接丢弃。
- 断开只 `hub.detach`，**不关闭 PTY**。
- 重连时再次 `ensure` relay；如果 relay 还在，直接复用；否则重新 `terminal.relay.connect`（attach 模式）。

### 7.5 背压/溢出

- 每个浏览者一个有界队列 `maxsize=128`（`terminal_stream_hub._ClientState`）。
- 队列满：设置 `overflow=True` 并唤醒 `ready`；writer 任务看到 overflow 立即退出，随后 `detach` 并 `close(code=1013)`（无 reason）。
- writer 对每帧 `asyncio.wait_for(send_json, timeout=5)`；发送异常/超时 → detach + close 1013。
- writer 在 `mark_ready` 之前一直等待 `state.ready`；`mark_ready` 会设置 `last_seq = snapshot_seq` 并唤醒。
- 速率限制/每会话合并：不存在；每个输出 chunk 一帧。

---

## 8. `WS /api/v2/connectors/{connector_id}/terminals/{terminal_id}/stream`（v1，connector 作用域）

实现：`api/connector_terminal.py:630-706`。

### 8.1 鉴权与前置条件

同 §7.1 的 1–3 步（4401/4404）。额外条件：

- 终端必须已存在：`broker.get(terminal_id)`；
- `term.session_id == terminal_connector_scope_id(connector_id)`，即 `"browse_<connector_id>"`；
- 否则 close 4404。

数据来源是 connector 的 **relay create 模式**：终端必须通过 v1 HTTP `POST /connectors/{id}/terminals` 创建（内部 `terminal.relay.connect` 不带 `mode` → `relay_mode="create"`）。

### 8.2 握手

1. `accept()`；
2. `broker.attach_client(terminal_id, websocket, from_seq=fromSeq)`：
   - 若有 scrollback，发送一条 `replay`；
   - 若终端已 exited，发送 `exit`；
   - 注册 client state；
3. `broker.send_to_connector(terminal_id, {"type":"attach"})`（connector create 模式不识别 `attach`，会忽略）；
4. duplex 循环。

### 8.3 服务端 → 客户端消息

| 消息 | 字段 | 触发 |
|---|---|---|
| `replay` | `{type:"replay", data:<base64>, seq:<int>}` | attach 时有 `chunk.seq > fromSeq` 的 scrollback。`data` 是这些 chunk 拼接后的单段；`seq` 是终端当前 `last_seq`（`snapshot_seq`） |
| `output` | `{type:"output", data:<base64>, seq:<int>}` | connector relay 的 `output` 帧经 `broker.on_output` fanout；`seq <= client.last_seq` 丢弃 |
| `exit` | `{type:"exit", exitCode:<int|null>, reason:<string>}` | attach 时已 exited，或 relay `exit` 经 `broker.on_exited` fanout |
| `pong` | `{type:"pong"}` | 客户端 ping |

v1 没有 `error` 消息类型从 broker fanout；input/resize 发送失败通常直接 `break` 关闭循环。

### 8.4 客户端 → 服务端消息

| 消息 | 字段 | 服务端行为 |
|---|---|---|
| `input` | `{type:"input", data:<base64>}` | 转发给 connector relay `{"type":"input","data":...}`；失败 `break` |
| `resize` | `{type:"resize", cols, rows}` | 缺省为终端当前 cols/rows；clamp 1–500 / 1–200；先 `broker.resize(...)` 更新本地，再转发 connector；失败 `break`。注意：`int(message.get("cols"))` 在 `try` 之外，非数字会导致异常冒泡并关闭 socket |
| `ping` | `{type:"ping"}` | 回 `pong` |
| 其他 | — | 忽略 |

### 8.5 断开

- `detach_client`；
- 若 `term.purpose == "user"` 且没有其他客户端：调用 `terminal_service.close_for_connector`：向 relay 发 `{"type":"close"}`，然后 `broker.remove(terminal_id)`（同时关闭本地终端记录/relay）。

### 8.6 背压

- 与 v1 session terminal 相同：`TerminalBroker._fan_out` 对每个客户端**同步** `await websocket.send_json`，没有队列、没有超时。
- 因此一个慢客户端会阻塞同终端其他客户端的 fanout，也会阻塞上游 `on_output` 的处理路径；发送异常时该客户端被 detach。
- scrollback 上限 256 KiB；超出丢最旧 chunk。

---

## 9. `WS /api/v2/sessions/{session_id}/terminals/{terminal_id}/stream`（v1，session 作用域）

实现：`api/sessions_terminal.py:116-214`。

### 9.1 鉴权与前置条件

1. query `token` 或 `Authorization: Bearer`；无 token / 校验失败 → close 4401。
2. `db.get_session(session_id, user_id)` 失败 → close 4404。
3. `broker.get(terminal_id)` 必须存在，且 `term.session_id == session.id`，否则 4404。
4. `accept()`。
5. `broker.attach_client(terminal_id, websocket, from_seq=fromSeq)`（发 replay/exit）。
6. duplex 循环。

该端点的输出链路是 **connector 通知**，不是 relay：
`terminal.output` notification → `ConnectorRealtimeService` → `TerminalBroker.on_output` → fanout。
因此终端必须通过 session 终端 HTTP API 创建（内部 `terminal.create`，不带 `outputTransport=relay`）。

### 9.2 消息

服务端 → 客户端：`replay` / `output` / `exit` / `pong`，语义与 §8.3 相同。

客户端 → 服务端：

| 消息 | 字段 | 服务端行为 |
|---|---|---|
| `input` | `{type:"input", data:<base64>}` | 调用 `TerminalService.write` → RPC `terminal.write`（timeout 10）。`TerminalNotFoundError` → 直接 break；其他异常 → 发 `{"type":"error","code":<status_code|500>,"message":...}`，发送失败则 break |
| `resize` | `{type:"resize", cols, rows}` | 缺省为终端当前值；clamp 1–500 / 1–200；调用 `TerminalService.resize_dimensions` → RPC `terminal.resize`（timeout 10），随后更新 broker 尺寸；错误处理同上。`int(...)` 解析同样在 `try` 之外，非数字会导致异常冒泡并关闭 socket |
| `ping` | `{type:"ping"}` | 回 `pong` |
| 其他 | — | 忽略 |

### 9.3 断开与背压

- `detach_client`；若 `purpose=="user"` 且无其他客户端 → `terminal_service.close(...)`（RPC `terminal.close` + broker remove）。
- 背压同 §8.6：同步 fanout、无队列、无发送超时。

---

## 10. `WS /api/v2/connector/terminals/{terminal_id}/relay`

实现：`api/connector_ingress.py:823-923`；connector 侧 `connector/server/terminal_relay.py`。

### 10.1 鉴权与握手

1. query `token` 必须存在 → 否则 close 1008。
2. `broker.get(terminal_id)` 必须存在 → 否则 1008 `"terminal not found"`。
3. `term.relay_token == token` → 否则 1008 `"invalid terminal relay token"`。token 是终端注册时生成的 `secrets.token_urlsafe(32)`，**不是单次消费**，终端存活期间可重复使用/重连。
4. 如果 `term.relay_mode == "attach"`，要求 connector 记录存在（`store.get_connector`），否则 1008 `"connector not found"`。
5. `accept()`。
6. 立即发送 `start` 帧（见下）。
7. `broker.attach_connector(terminal_id, token, websocket)`：
   - token 再校验一次（`secrets.compare_digest`）；
   - 若已有旧 relay socket，关闭旧 socket；
   - 设置 relay lease（Redis 模式 `SET terminal-relay:{id} <lease_id> EX 30`）；
   - 标记 ready。
   失败 → close 1008 `"invalid terminal relay token"`。
8. 进入读循环。

`relay_mode` 取值：`"create"`（v1 connector terminal）或 `"attach"`（v2 terminal / 已有 PTY 的附加）。

### 10.2 服务端 → connector 的帧

| 帧 | 字段 | 说明 |
|---|---|---|
| `start` | `{type:"start", mode, terminalId, sessionId, root, cwd, shell, command, args, profile, cols, rows, env, persistent}` | 连接后第一帧；`mode` 是 `"create"` 或 `"attach"`；`shell` 可能为 `null` |
| `input` | `{type:"input", data:<base64>}` | 来自浏览器或 v1 stream |
| `resize` | `{type:"resize", cols, rows}` | 来自浏览器或 v1 stream |
| `snapshot` | `{type:"snapshot", fromSeq:<int>}`，可能带 `requestId` | v2 建立流时请求快照；带 `requestId` 时 connector 必须回 `response` |
| `close` | `{type:"close"}` | 服务端主动关闭终端 |
| `attach` | `{type:"attach"}` | v1 connector stream 连接时发送；**connector 的 relay 读循环没有 `attach` 分支，会忽略** |

connector 侧校验：收到的第一帧必须是 `type=="start"` 且 `start.terminalId == terminal_id`，否则抛 `RuntimeError` 并断开。connector relay 客户端入站 `max_size=4 MiB`。

`broker.request_relay` 会为请求附加 `requestId`（`token_urlsafe(18)`），超时 10s；超时 → `TerminalRelayError("terminal relay request timed out", 504)`。

### 10.3 connector → 服务端的帧

connector relay 的 `read_commands` 识别 `input/resize/snapshot/close`；输出/事件由 writer 发送。

| 帧 | 字段 | `create` 模式 | `attach` 模式 |
|---|---|---|---|
| `ready` | `{type:"ready", pid:<int|null>}` | `broker.mark_running(pid)` | `broker.mark_running(pid)` |
| `replay` | `{type:"replay", seq:<int>, data:<base64>}` | base64 解码 → `broker.on_output(seq)` → 对浏览器变为 `output` | `hub.publish_relay` → 浏览器收到 `replay`（`seq <= last_seq` 丢弃） |
| `output` | `{type:"output", seq:<int>, data:<base64>}` | 同上，`broker.on_output` → `output` | `hub.publish_relay` → 浏览器收到 `output` |
| `exit` | `{type:"exit", exitCode:<int|null>, reason:<string>}` | `broker.on_exited` → 浏览器收到 `exit`；然后结束循环 | 转发给 hub；若 `reason=="closed"`，`broker.remove` 并结束 |
| `error` | `{type:"error", code:<int|string>, message:<string>}` | **忽略**（create 模式分支没有 error 处理） | 转发给 hub → 浏览器收到 `error` |
| `response` | `{type:"response", requestId, ok, result\|error}` | **忽略**（create 模式没有 pending 请求） | `broker.relay_response`，按 `requestId` resolve `request_relay` future；`ok=false` 时 `error.code` 若为 400–599 用作 HTTP 状态，否则 502 |

connector 在 `attach` 模式连接后会立即发送：

1. `ready`（pid 来自 snapshot）
2. `replay`（完整 scrollback，`seq` = snapshot seq）

在 `create` 模式连接后发送：

1. `ready`
2. `replay`
3. 之后每 30s 或收到输出变化时，用 `snapshot(fromSeq=last_seq, include_scrollback=False)` 拉增量：如果 `last_seq < baseSeq` 发 `replay`，否则逐 chunk 发 `output`；退出时发 `exit`。

### 10.4 心跳/租约/重连

- 服务端 relay lease：TTL 30s，后台每 10s 续期（`RELAY_REFRESH_SECONDS`）；续期失败 → 关闭 relay socket 并从 `_relay_sockets` 移除。
- connector attach 模式：socket 异常/关闭后无限重连，退避 0.5s → 5s；重连前调用本地 `snapshot` 确认 PTY 还在（`TerminalNotFoundError` 则退出）。
- connector create 模式：不自动重连（异常直接抛出，由上层处理）。
- 服务端收到 1008/4401/4404：connector 视为终止，不再重连。
- 关闭时 `broker.detach_connector` 清除 lease；attach 模式下不销毁 PTY。

### 10.5 背压

- 服务端→connector 的 `send_to_connector` 在本地模式直接 `await websocket.send_json`（无队列、无超时）；分布式模式通过 Redis publish，不保证背压。
- connector 侧 relay 发送通过 `send_lock` 串行化，`await ws.send(...)`，无队列；PTY 读线程只把数据放进本地 scrollback 并 `changed.set()`，不等待网络（`terminal_relay.py` 注释明确）。
- 浏览器侧背压在 §7.5 / §8.6 处理。

---

## 11. 终端 seq / replay / 状态共享语义（v1 与 v2 的差异）

### 11.1 序号来源

- connector 本地终端：`record["seq"]` 从 0 开始，每读到一块 PTY 输出 +1；`terminal.output` / relay `output` 都带这个 seq。
- 服务端 `TerminalBroker`：`term.last_seq` 随 `on_output` 更新；`scrollback` 上限 256 KiB。
- v2 hub：每个浏览器 client 的 `last_seq` 初始为 snapshot seq；只投递 `seq > last_seq` 的 output/replay。
- v1 broker：每个浏览器 client 的 `last_seq` 初始为 attach 时的 `snapshot_seq`；只投递 `seq > last_seq` 的 output；exit 在 snapshot 已 exited 时不重复发送。

### 11.2 replay 语义

- v1 `attach_client(from_seq)`：`replay_bytes` 返回所有 `chunk.seq > from_seq` 的 chunk 拼接；作为**一条** `replay`，`seq = term.last_seq`。所以客户端不能把 `replay.seq` 当作“这段数据的首 seq”。
- v2：快照由 connector `terminal.snapshot(fromSeq)` 生成；默认包含完整 scrollback，作为一条 `replay`，`seq = snapshot.seq`。
- 客户端收到 `replay` 应清空/重置终端显示，再写入 `data`。

### 11.3 终端生命周期

- `TerminalBroker` 记录 TTL：24h（`TERMINAL_TTL_SECONDS`）；exited 记录 1h。
- 浏览器 lease：30s，服务端每 10s 续期；`has_clients` 用 lease 判断是否还有浏览器。
- connector 本地终端：idle TTL 30 分钟（可配 `AGENT_CONNECTOR_TERMINAL_IDLE_TTL_SECONDS`）；closed TTL 15 分钟；最多 32 条记录；本地 scrollback 512 KiB。
- 进程退出后 `terminal.exited` 会带 `exitCode` 与 `reason`（`exit` / `closed` / `idle_timeout` / `pump_error:...`）。

---

## 12. Go 实现要点清单（按代码行为）

1. **ticket 必须每次重连重新申请**；session/dashboard 的 ticket 作用域必须匹配，单次消费，60s。
2. session/dashboard 的 WS **不发送 ACK/心跳请求**，入站帧无意义；只处理服务端消息。
3. session WS 事件不能用 `sequence+1` 判断丢包；同一 sequence 可有多条事件。用 `cursor`/`/events` 恢复，用 `eventId` 去重。
4. `session.subscribed` 是“服务端已注册订阅”的信号；恢复应在收到它之后进行（现有三端都这么做）。
5. `snapshotRequired=true` 时不要尝试拼接增量，重新拉 session snapshot。
6. connector WS 的 `request.id` 必须原样回传；错误体只允许 `{code,message}`。
7. connector 侧对慢 RPC 不应因服务端超时而取消本地执行（现有实现明确不取消）。
8. v2 terminal 必须先发 `replay` 再 `mark_ready`；否则早到的 output 会和 replay 顺序错乱。
9. v2 terminal 队列满/发送超时 → 主动 close 1013；v1 terminal 没有队列，慢客户端会阻塞 fanout。
10. relay 的 `start` 是服务端第一帧；connector 必须先解析它拿到 `mode/terminalId`，再处理后续命令。
11. relay `snapshot` 带 `requestId` 时必须回 `response`；`input/resize/close` 不需要回。
12. 所有自定义 close code 都是“应用层语义”，标准库/框架可能只暴露 code + reason；不要依赖 ping/pong 判断 connector 存活。

---

## 13. UNKNOWN / 无法从代码确认的点

| 项 | 结论 | 已查位置 |
|---|---|---|
| `WS /api/v2/connectors/{connector_id}/ws` | 不存在 | 所有 `@router.websocket`、`app.py` include_router、三端客户端、`docs/api/*` |
| `runtime.configSchema` / `runtime.config` / `session.discover` 的服务端调用方 | connector 实现了方法；server 仓库内没有调用点，实际使用方 UNKNOWN | `grep "\"runtime.configSchema\""` 等全 server 目录；`connector/server/runtime_rpc.py` |
| `runtime.refetch_required` | 文档目标里出现过，但当前 server 不发送；Android 有处理分支 | `core/events.py`、全 server grep、`docs/api/session-api-current-gap.md` |
| `ProtocolHandshakeRequest/Response` | 有模型定义，但无任何调用点；握手实际不存在 | `server/agent_server/core/protocol.py:105-113`、`connector/connector/server/protocol.py:58-64`，全仓库 grep |
| 协议级 WebSocket ping/pong 间隔 | 代码未设置；Uvicorn 部署默认值不在仓库内，UNKNOWN | `app.py`、各 WS handler |
| WebSocket 最大帧/最大消息大小（server 入站） | 代码未设置；取决于 ASGI server 配置，UNKNOWN | 各 WS handler；connector 客户端有 `max_size`（relay 4 MiB）但服务端无 |
| relay token 轮换 | 未发现轮换逻辑；只随终端记录 TTL 失效 | `infra/terminal_broker.py:71`、`api/connector_ingress.py:837` |
| `runtime.statusChanged` 是否会在 connector WS 上产生 ACK | 不会；通知无 ACK | `connector_ingress.py:_read_connector_messages`、通知泵 |
| dashboard.changed 的 `reason` 是否下发 | 不下发；`reason` 被丢弃 | `services/dashboard_events.py`、`infra/timeline_broker.py:publish_dashboard` |
| session WS 的 `runtime.refetch_required` | 不发送 | `core/events.py:events_from_invalidation` |
| `terminal.list` / `terminal.snapshot` / `terminal.release` 的 server 调用点 | server 没有直接 RPC 调用；v2 走 relay socket，release 由 connector relay 内部调用 | `grep "terminal.list"` 等；`services/terminal_relay.py` |
| 未列出的 v1 connector 终端创建 | `POST /connectors/{id}/terminals`（HTTP）内部走 `terminal.relay.connect`，无 `mode` → create 模式 | `api/connector_terminal.py:120-150`、`services/terminal.py:170-180` |

