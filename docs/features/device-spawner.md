# Device Spawner（设备连接器）

Spawner 是每台受控电脑上的常驻进程：主动连 Remote Control Server (RCS)，列出本机项目/历史会话，按服务端请求打开**可见的交互式 Claude Code 终端**并恢复指定会话。对话本身不经过 Spawner：终端的 REPL Bridge 直接与 RCS 通信。

## 架构

```
浏览器 ──HTTP/SSE──► RCS ◄──WebSocket── Spawner（每台电脑一个）
   │                   │
   │                   └──WS/HTTP── CLI Bridge ── 交互式终端（可见）
   │
   └── 历史读取走 RCS → Spawner → 本机 transcript（不启动终端）
```

三层身份：

| 身份 | 示例 | 谁生成 | 说明 |
|---|---|---|---|
| deviceId | `dev_ab12…` | Spawner 首次运行 | 一台电脑 |
| localSessionId | UUID | CLI | 本机历史会话，磁盘上的 transcript 名 |
| bridgeSessionId | `session_<hex>` | CLI Bridge | 每次连接 RCS 时生成，重启会变 |

Web 的稳定 URL 是 `/code/d/<deviceId>/s/<localSessionId>`，**不包含** bridgeSessionId；服务端维护 `(device, local)` → 当前 bridge 的映射，终端重启只更新映射。

## 设备协议（v1）

- 端点：`ws(s)://<rcs>/desktop/ws`
- 认证：WebSocket 子协议 `rcs.auth.<base64url(token)>`
  - 首次连接用共享 RCS API key 换发设备身份（hello 带 `secret`）；
  - 之后用设备自己的 token（即 secret）作为 bearer；
  - 服务端只保存 secret 的 SHA-256。
- 心跳：Spawner 每 15s 发 `{type:'heartbeat', ts, running:[SessionRuntimeState]}`；服务端 60s 未收到即断开。

方法（封闭枚举，绝不接受网页传来的命令行）：

| 方法 | 作用 |
|---|---|
| `list_projects` | 按 cwd 聚合本机项目 |
| `list_sessions` | 项目/全局历史会话列表（limit/offset） |
| `read_session` | 流式读取 transcript，`before` 游标向前分页 |
| `open_session` | **仅确认**请求；进度用 `operation` 事件上报 |
| `verify_state` | 核实某会话的进程/Bridge 证据 |

`open_session` 事件序列：

```
accepted → starting → terminal_spawned → bridge_connecting
         → ready | waiting_for_user → (超时) failed
```

`ready` 只在 CLI 自己把 `bridgeSessionId` 写进 `<configDir>/sessions/<pid>.json` 后才发出——服务端不猜。

## 状态语义（诚实原则）

服务端分开保存四类证据，不做推断：

- `spawnerConnected`：WebSocket 是否在线；
- `processAlive` + `pid`：登记文件 + PID 存活 + 命令行归属；
- `bridgeSessionId`：CLI 自己写入的 Bridge ID；
- `verifiedAt / reportedAt`：证据时间。

UI 规则：

- Spawner 离线 → 不推断终端已关闭；
- Bridge 断线 ≠ 终端退出 → 显示“连接未知/最后核实时间”；
- 终端活着但没开 Bridge → 不重启，提示在该终端执行 `/remote-control`；
- 历史只在设备在线时读取；离线只显示上次目录缓存。

## Open 的并发与归属

- 服务端按 `(device, localSession)` 合并并发请求，同一会话只保留一个活动 operation；
- Spawner 本地也有 per-session 锁；
- 启动前用登记文件 + PID 存活 + 命令行（`--resume <id>` 或 `cli.js`）核对归属，仅凭 PID 不算；
- 已有进程且带 Bridge → 直接复用；已有进程无 Bridge → 返回 `already_running_without_bridge`。

## Windows 可见终端

Spawner 写一个临时 `.cmd` 启动器，再由 `cmd.exe /c` 执行 `start "" "<bun>" "<cli.js>" --resume <id> --remote-control`。`start` 保证控制台程序获得新的可见窗口；不把带引号的 `start` 命令直接塞进 Node 的 spawn 参数（libcmd 的转义会让 cmd 卡住）。

环境变量只向子进程注入：

- `CLAUDE_BRIDGE_BASE_URL` = RCS 地址
- `CLAUDE_BRIDGE_OAUTH_TOKEN` = 共享 RCS API key
- `CLAUDE_CONFIG_DIR` = Spawner 管理的配置目录

清理：启动器 30s 后删除。

## Web API（uuidAuth，与现有无登录模式一致）

| 方法 | 路径 |
|---|---|
| GET | `/web/devices` |
| GET | `/web/devices/:id/projects` |
| GET | `/web/devices/:id/sessions?projectId=&limit=` |
| GET | `/web/devices/:id/sessions/:local` |
| GET | `/web/devices/:id/sessions/:local/history?before=&limit=` |
| POST | `/web/devices/:id/sessions/:local/open` |
| POST | `/web/devices/:id/sessions/:local/messages` |
| GET | `/web/operations/:id` |

`cached: true` / `updatedAt` 表示设备不可达、返回的是上次目录快照。

## 消息可靠性

- 每条用户消息有 `messageId`，服务端 outbox 持久化，终端就绪后按序 flush；
- `publishSessionEvent` 只证明“已进入服务端事件总线”，不代表终端收到；
- 有 bridge 订阅者时返回 `sent`，否则 `unconfirmed`，UI 明确显示“发送状态不确定”；
- RCS 重启后设备表、映射、outbox 从 `RCS_DATA_DIR/device-state.json` 恢复（bridge 会话本身仍是内存态）。

## 持久化

默认 `~/.ccremote/rcs-data/device-state.json`（`RCS_DATA_DIR` 可覆盖），temp + rename 原子写。包含：

- 设备记录（secret 仅 hash）
- `(device, local)` → bridge 映射
- 最近一次目录缓存
- 未完成/未确认的 outbox

## 运行

RCS：

```bash
RCS_PORT=3000 RCS_API_KEYS=<key> bun run scripts/rcs.ts
```

Spawner（首次运行前在配置里写入 `enrollKey`）：

```bash
SPAWNER_CONFIG=%LOCALAPPDATA%\CCRemote\spawner.json \
SPAWNER_LOG_DIR=%LOCALAPPDATA%\CCRemote \
bun run packages/device-spawner/src/main.ts
```

`spawner.json` 关键字段：`serverUrl`、`enrollKey`、`deviceId`、`deviceSecret`、`claudeConfigDir`、`cliPath`、`bunPath`、`allowedRoots`。文件由 Spawner 自动生成并用 owner-only ACL 收紧。

## 已知限制

- 一个 Spawner 进程管理一个 `claudeConfigDir`；多配置目录需多实例（或后续扩展）。
- 历史正文不缓存到服务端：设备离线时只能看目录，不能看正文。
- Bridge 断线后服务端无法确认终端是否仍活着，除非 Spawner 在线并重新核实。
- 无登录/无 HTTPS 是产品既定选择；任何能访问 RCS 的人都能使用 UUID 进入（风险已确认）。
- `/remote-control` 在已运行终端中是否可用取决于该 REPL 命令；不可用时只能提示用户，不自动重启。

## 实时状态与控制（2026-11-04 补充）

- **bridge_state 事件**：Bridge WS 连上/断开时，RCS 立即向 `/web/sessions/:id/events` 订阅者推送 `{type:'bridge_state', connected}`。网页据此维护连接状态，发送前**不做任何查询**；每条 Bridge 连接带唯一 token，关闭按 token 精确清除。
- **system/init 元数据**：自托管模式下 CLI 无条件发送，含当前模型、权限模式、`models[]`（模型选择器）、`command_details[]`（斜杠命令菜单）。
- **控制通道**：网页经 `/web/sessions/:id/events` 发 `control_request`（`set_model` / `set_permission_mode`），CLI 回 `control_response`，错误会显示。
- **发送后自动重开**：终端关闭 → 网页收到 `bridge_state:false` → 发送走排队+自动重开终端，消息不丢。
- **终端退出快速上报**：Spawner 监听 `<configDir>/sessions` 目录变化，~300ms 内上报 session_state，不等 15s 心跳。
- **进程归属**：`--resume` 后 PID 文件的 sessionId 可能与请求不同；Spawner 按 `--resume <id>` 命令行 + PID + bridge id 三重关联（`findLaunchPids` / `verifySession`）。
- **登录自启动（Windows）**：`scripts/install-device-spawner.ps1` 注册每用户登录计划任务（Interactive/Limited），已注册为 `CCRemote Device Spawner`；`-Uninstall` 卸载。Spawner 自带单实例锁。

## Web 客户端定位

网页端刻意保持极薄（Web + 未来 iOS 共用服务端 API）：

- 顶栏仅品牌 + 主题切换；
- `/code/` 工作台（设备 → 项目 → 会话 + 聊天）；
- `/code/d/<deviceId>/s/<localSessionId>` 稳定深链；
- `/code/<bridgeSessionId>` CLI 打印链接直达聊天；
- 旧 Dashboard、环境发现、身份/令牌面板、ACP 界面已删除（代码移除，备份在仓库外 `claude-code-backup-2026-11-04/acp-cleanup/`）。
