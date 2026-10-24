# Go 重写方案：connector + server

日期：2026-10-24　｜　基于真实代码扫描（6 路调查 + 2 份架构 + 1 份合成）

---

## 摘要

结论：两端一起用 Go 重写可行，但这不是「把 Python 翻译成 Go」，而是「冻结协议 + 重建并发核心 + 砍掉约 12% 历史包袱」。硬前提三条：协议先冻结；server 先切、connector 后切，禁止同一窗口切两端；第一版不拆 Redis。违反任何一条，就会把一次迁移变成三场同时进行的迁移（换语言、换并发模型、换存储/事件模型）。

路径根：server = D:/Develop/PyCharmProject/Agents-Anywhere-main/server/agent_server/；connector = D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/；contracts = D:/Develop/PyCharmProject/Agents-Anywhere-main/contracts/。

必须先纠正三个事实：
1. SessionView 的 runtimeId 并不是「snapshot 拿不到」。core/protocol.py:46 的声明确实缺 runtimeId/runtimeType/runtimeName，但基类 _ProtocolSnapshotExtensionModel 是 extra=allow，api/sessions.py:992 又把 models.SessionView 的 model_dump 直接传入，所以线上 JSON 实际带 runtimeId，server/tests/test_runtime_discovery_recovery.py:58 明确断言 snapshot.session.runtimeId 存在。真正的问题是 schema/声明漂移和依赖副作用，不是运行时缺字段。Go 必须声明完整超集，不能按 contracts schema 生成 struct。
2. phone-pane 插件和 Electron Desktop 不在本次重写范围，但它们是 connector 的外部契约。插件桥只能继续走 v1 文件协议（design 2 的 HTTP 主通道必须等插件升级）；Desktop 通过 MeshCall LocalRouter 控制 connector，纯 Go 无法与未改的 Desktop 互操作。要么保留一个只做 Control/Logs 的 Python 边车，要么同批改 Desktop。所谓 connector 100% Go 在第一版不成立。
3. 设计稿建议 v1 删 Redis，本方案不采纳。同时改语言 + 去 Redis 是两个独立风险；而且终端 registry 与 pending timeline 现在只存在 Redis（terminal_broker.py:21 的 24h TTL、AOF everysec 最多丢 1 秒）。v1 保留 Redis、同 key 同语义，Coordinator 接口化，phase 8 再摘。

目标形态：server 单进程 Go + PostgreSQL（v2_44 基线）+ Redis（暂时保留）+ 每会话 actor；connector 单二进制 + SQLite + 插件文件桥/Windows 控制台注入/SDK 三条投递。客户端（iOS/Web/Android/Desktop）零改动。审批与事件日志的一等模型（session_events/approvals）作为 phase 6 的 additive 变更，不进入切换版；切换版继续用 notice 投影，因为客户端只认它。

代价：基准约 300-320 人日（server 100、connector 170-180、联调/契约/切换 30-40），含 25% 缓冲约 375-400 人日。3 名 Go 工程师约 6.5-7.5 个月到 Python 完全下线；2 人约 10-12 个月。比「server 先留 Python、只改 connector」多约 110-170 人日和 2-3 个月，并且多一个「双端同时无稳定参照」的风险。

值不值：如果目标是长期单二进制、统一并发模型、2C2G 常驻内存下降、摆脱 Python 依赖，值得；如果目标是短期降风险或省人力，不值得，应先只改 connector。用户已经决定一起改，本方案按「值得但更贵」来排，不粉饰。

---

## 模块判断汇总

共判断 203 个模块：keep-semantics 116，drop 35，rewrite 52

### 直接丢弃（35）

- **api/agents.py** → discard
  - 只返回 410 提示改用 connector runtime 路由；当前 Web/iOS/Android 零调用
  - 风险：若仍需兼容旧发布版客户端，只能在反向代理层留 2 条 410
- **api/connector_shell.py** → discard
  - 全仓库除 server/tests 外无任何客户端调用；终端能力已由 terminals-v2 覆盖，属于没人用的接口
  - 风险：若未来要“无 PTY 的一次性命令”，语义在 services/connector_shell.py，可后补，不必现在移植
- **api/sessions_terminal.py** → discard
  - 当前 Web/iOS/Android 全部改用 connector 级 terminals-v2；仅 _deprecated/web 使用，属于被取代的重复实现
  - 风险：若需兼容已发布旧客户端，需确认版本窗口；新客户端不需要
- **core/interactions.py** → discard
  - 当前 server 无任何引用，仅 _deprecated 的 services/interactions.py 与 tests 使用；线上审批已改由 runtime notices 承载
  - 风险：若未来要做服务端状态机，可参考但不要现在移植
- **core/session_states.py** → discard
  - 只被 services/session_states.py 使用，而该 service 除 tests 外无任何实例化；线上状态直接来自 connector runtime state
  - 风险：不要误以为它是线上状态机；重写时状态值集合（idle/waiting/running/...）仍要保留在 protocol 里
- **event_preparation.py** → discard；只保留「准入超限→1013 断开，让客户端重连恢复」的产品行为
  - 进程池是绕 Python GIL 的方案；在 2C2G 上多养两个 Python 进程反而更贵。Go 编码 JSON 不需要进程池
  - 风险：直接删掉而不给等价的有界队列，会让超大消息在慢 socket 上阻塞整个广播循环
- **runtimes/claude/sdk_driver.py** → discard
  - 只有 infra/runtimes/claude/__init__.py 和 tests/test_claude_sdk_driver.py 引用，运行时没有任何调用方；服务端从不驱动引擎，执行始终在 connector
  - 风险：没有风险；若未来要让服务端直连 SDK，那是新需求而不是移植
- **session_states** → discard
  - 全仓库无调用者；core/session_states 也只被它引用。真正在用的状态迁移是 repository 的 set_session_status 与通知 handler，不需要这层。
  - 风险：无。删除前确认 external 引用为零；若要恢复“状态派生”能力，应作为 repository 内的单一函数，而不是独立 service。
- **timeline_effects** → discard（职责并入 approval 事件投影器）
  - apply_approval_resolution_to_target_item 全仓库无调用者；v2 审批是一等事件，timeline item 上的 approval 状态应由投影器重建，不应双写两套真相。
  - 风险：无。注意别把“审批状态要出现在工具卡上”这个 UI 需求一起丢掉——它应由 projection 从 approval 事件派生。
- **repository_ports** → discard（在各 use case 包内定义 1-3 个方法的窄接口，或直接用具体 store 类型）
  - 这是重复实现与过度抽象的典型：接口形状大量重复组合（SessionRunRepository 同时继承 5 个 Protocol），实际约束在 infra 实现里。Go 不需要 659 行镜像层，按消费方定义小接口即可。
  - 风险：删除时不要连带丢掉“写路径必须带 session_revision_fence”的约束——它应成为 store 的显式装饰器/中间件，而不是注释。
- **多实例/多 runtime 机制（instance_binding + supervisor + instance_models + supervisor_models）** → discard。Go 用常量 Runtime="claude"、RuntimeID 透传/默认 "claude"；若将来真要多实例，只在 API 边界加映射，不做装饰层。
  - 当前只有 Claude 一个 provider 且 instance_policy=single/max_instances=1，runtimeId 恒等于 runtimeType，所有 760 行转换都是恒等映射；资源 claim、rti_ 校验、multi 实例容量、config revision 冲突对单实例 Claude 没有真实问题要解决。唯一需要保留的是线协议上的 runtime/runtimeId 字段和“服务器可能记住一个非 claude 的 runtimeId”这一事实。
  - 风险：Server 会按它记录的 runtimeId 发 session.* 请求；如果历史 DB 里存在 rti_ 形式或非 claude 的 ID，Go 不能因为“不认识实例”就拒绝，应当把它当作 Claude 实例处理，并在首个 scoped 调用时用持久化配置懒启动。
- **runtime_protocol/filesystem.py** → discard
  - 全仓库无调用者（只有 _reference 时代可能用过），且与当前单实例 Claude 无关，是资源 claim 机制的残片。
- **server/catalogs.py（旧目录转换）** → discard
  - 全仓库只有 _reference/claude/sdk_adapter.py 引用，现役代码零调用；模型/权限目录已由 runtimes/claude/catalogs 直接产出。
- **core/unicode.py + core/preferences.py** → discard（JSON 编码前用 utf8.Valid 清理即可）
  - Go 的 encoding/json 会把非法 UTF-8/孤立代理转成 U+FFFD，不需要 Python 这套手工替换；preferences 是空实现，现役代码没有任何有内容的偏好来源，属于历史残留。
- **protocol/1.0 handshake** → discard；Go 侧不需要 Handshake 类型。若要做协议版本协商，应重新设计成实际接入路径的一部分。
  - 死契约：server 与 connector 都没有任何路由/调用，只有 core/protocol.py 的模型定义、export 脚本和 server/tests/test_protocol_models.py 的单测；真实注册走 WS + Bearer + runtime.discover。
  - 风险：无运行时风险；唯一成本是若未来要恢复握手，需要重新定义并补 fixtures。
- **local-machine/1.0** → discard 作为契约；Go 迁移器只读一次旧文件，合并 connectorIds 后写 2.0 并打 legacyMachineMigrated。
  - README 自己声明已被 2.0 取代；只剩一次性迁移读取需求。
  - 风险：完全删除迁移读取会让老用户丢 connectorIds 历史；但这属于一次性兼容代码，不是长期契约。
- **编码格式决策：JSON vs protobuf** → encoding/json / json.RawMessage；优化走 delta/checkpoint、批量、gzip/permessage-deflate，而不是换二进制。
  - protobuf 方案应丢弃：开放 JSON 载荷（timeline.content/source、catalog metadata、notice context/uiSchema、capability parameters、runtime config schema/uiSchema）、fixtures 明确要求保留未知扩展字段、三端客户端本次不改、JSON Schema 是契约真源、Struct 只有 double 无法无损表达任意 JSON 整数。
  - 风险：若坚持 protobuf，需要同时重写 iOS/Web/Android 客户端与契约生成链，且 fixtures 无法继续验证兼容性；收益只在长文本带宽，而长文本应走 delta/blob ref。
- **Connector 参考实现目录（_reference + _deprecated）** → discard；若需要当规格，转成 docs 里的 md，不要进 Go 仓库
  - 目录自带 README 明写「Active Connector code must not import modules from this directory」；runtime_protocol 重构后已无任何活动代码 import（我的 AST import 图确认 _reference 全部 0 引用，只有 _reference 内部互相引用）。共 14 个 py 文件、3,334 行（含 60KB 的 sdk_adapter.py）。它不是死代码的意外，而是被显式废弃的迁移素材。
  - 风险：重写时唯一值得抢救的是 runtimes/claude/domain/ 之外的 turn/approval 状态机细节；但 audit 文档已确认 domain/ 是更好的规格书，_reference 里没有独有语义。
- **Server shell 执行子系统** → discard；长任务执行如仍需要，用 terminal.create 的 command 参数承载
  - 三端客户端零调用：web-next 里 grep 'shell/exec' 与 'shell/tasks' 无命中，ios/android 无命中，desktop-workbench 也无。服务器侧 shell.* 方法字符串只出现在 services/connector_shell.py 自身，connector 的 shell.task.start/cancel 只被该服务调用——形成一条完整的闭环死链。shell.exec 的能力已被 terminals-v2 的 command/args/profile 参数覆盖。合计 786 行。
  - 风险：connector_realtime.py 里有 shell.task.started/completed 的 WS 分支和 ShellTaskManager 注入；两端要同时删，否则会保留半条链路。
- **V1 终端 API（/connectors/{id}/terminals 与 /sessions/{id}/terminals）** → 只保留 v2 语义：terminal.create/write/resize/close/rename/setPersistent/list/snapshot + relay.connect；v1 状态机 discard
  - 新版客户端全部只打 terminals-v2：web-next/features/dashboard/api.ts 里 connectorTerminalList/Create/Rename/Close/Resize（v1 五个方法）定义了但全文件零调用，UI 只用 *V2；android TerminalApi.kt 四条请求全带 -v2；ios 无 terminals。services/terminal.py(466 行) 的引用方只有这些 v1 路由与 sessions_terminal.py(214 行)。v2 的流式走 services/terminal_relay.py + terminal_broker，不依赖 TerminalService。
  - 风险：删 services/terminal.py 时务必留下 terminal_connector_scope_id()——services/terminal_relay.py:20 在用它；ServerTerminal 的 rename 语义在 v2 里由 connector 侧实现，不要丢。
- **Server 内置 Claude SDK 驱动（未接线的 seam）** → discard；同时把 claude-agent-sdk 从 server 依赖中移除
  - 只被同包 __init__.py import，而 infra/runtimes/claude/__init__.py 本身零引用（AST 图）；全仓唯一消费者是 tests/test_claude_sdk_driver.py。文件 docstring 自相矛盾地写着『The project does not yet depend on claude-agent-sdk』，而 server/pyproject.toml 的 dependencies 里就有 claude-agent-sdk。与既定架构（Claude 只跑在 connector 本机）直接冲突。
  - 风险：无业务风险；确认没有外部脚本 import agent_server.infra.runtimes 即可。顺带能显著缩小 server 镜像。
- **迁移库里的 unversioned 数据库取证识别（classify_database）** → 只保留一条路径：`schema_version` 表 + 单一基线 DDL（或一次性 pg_dump 导入脚本）；classify_database discard
  - 474 行里约 250 行是嵌套的列名探测（projects 约束名 uq_projects_user_connector_workspace、外键 SET NULL vs RESTRICT、dsh_agents 是否还在……），存在的唯一理由是『早期部署不带 alembic_version』这段历史。Go 版不可能逐版本复刻 44 个 migration 的中间态；这段代码在 Go 里要重写一遍等于把历史包袱搬过去。
  - 风险：如果有用户库确实没打过 alembic_version，删掉就没法自动升级——必须先做一次线上盘点（SELECT count(*) FROM alembic_version），或提供一个独立的 Python 脚本来完成最后一次 stamp。
- **仓储层 Protocol 全集（repository_ports）** → discard；Go 侧按用例定义 3-5 个 2-3 方法的接口（如 SessionReader、TimelineWriter、ConnectorCaller），不要 1:1 翻译
  - 659 行 Protocol，实现只有 infra/repositories/facade.py 的 Store 一个（测试里也没有 fake 实现，grep 'repository_ports' 在 tests 下零命中）。其中 ProjectRepository、InteractionResolutionRepository 连引用都没有。它的真实价值是『service 依赖面文档』，而这个价值在 Go 里由小接口自然获得。
  - 风险：重写时最容易丢的是 Protocol 名里隐含的**事务边界**：例如 ConnectorIngestRepository.session_revision_fence()、DeviceRuntimeRepository.connector_lifecycle() 这些上下文管理器，它们是并发正确性契约，不是抽象噪音。这些必须逐条转成 Go 的显式锁/事务，而不是当成接口删掉。
- **OAuth 客户端动态注册 CRUD** → 只保留写死的两个 first-party client 常量 + get client by id；oauth_clients 表可以不要
  - list_oauth_clients / create_oauth_client / delete_oauth_client 三个方法全仓零调用（只有定义）；对应的 OAuthClientCreateRequest、OAuthClientListResponse、OAuthClientView 模型也无端点引用，且 api 层没有注册任何管理这些客户端的路由。实际生效的是 core/oauth_clients.py 里的 FIRST_PARTY_OAUTH_CLIENTS 字典 + 启动时 seed。
  - 风险：如果未来真要做第三方客户端接入，需要保留 redirect_uri 精确匹配这条安全约束（allows_redirect 用 == 而不是前缀匹配），别在简化时退化成宽松匹配。
- **sessions.dsh_archive_legacy 列** → discard；新表不要这一列
  - v2_13 迁移回填 UPDATE sessions SET dsh_archive_legacy=1 WHERE runtime='dsh' AND archived=1，但此后：唯一的写入是 sessions.py:63 恒写 0；7 处只是出现在 SELECT 列清单里；SessionView 没有对应字段（grep 'archiveLegacy|dshArchive' 三端零命中）。消费逻辑已被 source_state 模型取代。
  - 风险：无。若历史库要原样迁过去，先一次性 UPDATE 回填 archived 即可，无需保留列。
- **legacy_import_archive 表** → discard；如需留档，在切换前导出成一份 JSON/CSV 存档文件，不要留在业务库里
  - 写入方只有 migrations/versions/v2_2.py，读取方：无（全仓 3 处引用 = schema 定义 + v2_2 写入 + classify_database 判断某库是否已到 v2_2）。它保存的正是多运行时时代的 agent_* 配置，与 Go 版只有 claude 的模型无关。
  - 风险：这是唯一有『数据不可再生』性质的一项；删表等于放弃 v1 配置的可回溯性。建议先导出到对象存储/文件再删，而不是直接 drop。
- **/agents/* 410 墓碑路由** → discard；若担心旧 App 版本体验，用一个统一的 404 handler + 提示头即可，不必保留路由
  - /agents 在三端活代码里零调用；仅 ios/_deprecated/API/APIClient.swift 有旧引用（该目录本身已废弃）。两个 handler 除了鉴权就是 raise 410，不产生任何副作用或数据。同一模式还出现在 connectors.py:123。
  - 风险：如果市场上还有 1.x 客户端，410 会变成 404——错误码变化可能被客户端当成『服务器异常』而不是『功能下线』。要么接受，要么在 Go 里留一个 5 行的 mux 分支。
- **core/interactions.py 交互状态机** → discard（真正要移植的是 InteractionNotificationHandler 里的 notice 语义）
  - 全仓零 import（AST 图与 grep 都确认），也不被测试引用。实际处理 notice.upsert 的是 services/connector_notifications.py 的 InteractionNotificationHandler，它自己校验状态，没走这里。
  - 风险：无。注意别误以为『状态机已在别处复用』——没有，Go 里审批状态机要照 audit 文档重新设计（notice 不能当唯一载体）。
- **Session 状态机（services/session_states.py + core/session_states.py）** → discard
  - services/session_states.py 只被 tests/test_session_states.py import；core/session_states.py 只被 services/session_states.py import。生产路径的会话状态由 connector_notifications 的 session.state.updated 直接落库（SessionStateNotificationHandler），不走这两个模块。155 行纯死代码 + 28KB 的 transitions 表。
  - 风险：两套状态语义并存本身是风险：删掉这套前先确认 SessionStateNotificationHandler 的 _v2_session_status 白名单与 SESSION_TRANSITIONS 一致，否则会掩盖一个真实的漂移。
- **timeline_effects.py 与 AttachmentMaterializer** → discard；审批回写语义在 audit 文档里已被『notice 只留 input_request / 审批要有独立 resolutionId』替代
  - services/timeline_effects.py 全仓零引用（46 行，含 timeline_content_hash）。AttachmentMaterializer 只在 services/attachments.py:202 定义，无任何实例化。
  - 风险：如果将来真要做『审批结果体现在 timeline 卡片上』，这是唯一一份参考实现——重写时按新事件模型重做，不要照抄它按字符串比较 status 的写法。
- **S3 文件后端与 FileStorage ABC** → 只保留本地磁盘实现；接口收敛成 3 个函数（Write/Read/Delete），不要保留 backend 枚举与工厂
  - 部署侧 docker/docker-compose.postgres.yml:73 写死 AGENT_SERVER_FILES_BACKEND: local；单机单租户决策下 S3 分支永远不会开。S3 实现还依赖一个非 PyPI 的 GitHub 包（pyproject 里 [tool.uv.sources] 指向 github.com/t4wefan/httpx-s3-client），仓库里甚至还有一份 vendored 拷贝（_reference/httpx-s3-client，99KB）。
  - 风险：重写时别丢 local.py 里的路径穿越校验（session_id 禁止 '.'、'..'、'/'、'\\'、'\0'）与 sidecar metadata 的原子写；这两个是安全/一致性，不是抽象。
- **connector runtime_protocol/filesystem.py** → discard
  - 27 行，全仓零 import（唯一的同名命中是 server/local_rpc.py 的注释文字）。是资源声明（RuntimeResourceClaim）设计里没被用上的一半。
  - 风险：无。若未来要做多实例互斥写同一 workspace，这个函数反映的是真实需求（symlink 折叠、大小写归一），届时按 Go filepath 重写。
- **connector server/catalogs.py** → discard；catalog 组装放 provider 里
  - 唯一 import 方是 connector/_reference/claude/sdk_adapter.py（已废弃）与 tests/test_protocol_contract_compatibility.py；活动路径的 catalog 由 runtimes/claude/catalogs/reader.py + provider_config.claude_capabilities() 产生。212 行里有 3 个函数在活动代码中零引用。
  - 风险：测试若只测这个模块，删了会掉覆盖——先把该测试改成打 reader.py 的真实输出，别顺手删掉契约测试。
- **Connector 一次性目录/锁迁移代码** → discard；如需照顾存量用户，写一个独立的 `anywhere migrate` 一次性命令（或安装器步骤）
  - 全部是『从上一代安装目录迁移』的一次性逻辑：paths._merge_legacy_data_dir 每次启动都跑、LEGACY_SQLITE_NAMES 只用于删除旧库；runtime_owner.state_transaction 的 legacyMachineMigrated 分支在迁移完成后恒为 True 短路；cli.py:161/210 与 control.py:60 传的 legacy_paths 只有 connector-runtime.json 一个。
  - 风险：重写时不能丢的是**单实例锁本身**：RuntimeLease 的 PID+进程启动时间校验、跨进程 file lock、owner_alive 判定——这是防止两个 connector 同时驱动同一 session 的关键。只删 legacy_* 部分。
- **contracts/local-machine/1.0** → discard（v2 契约里的 ConnectorRecord 结构才是要实现的目标）
  - 文档开头自述『历史契约，保留供旧版本迁移参考。当前实现使用 v2 契约』；v2 已改成 connector 独占维护 connector-runtime.json。Go 版没有理由同时支持两个本机记录格式。
  - 风险：若存量桌面版仍在写 machine.json，connector 需要读一次做迁移（正是上面 paths/runtime_owner 那段 legacy 代码）；两者要一起决定去留，别只删契约不删代码或反之。

### 要重写（52）

- **api/connector_terminal.py** → pkg/api/terminals.go 仅 v2 路由 + pkg/terminal/relay
  - 只保留 v2 家族（create/list/rename/persistence/close/resize/write/snapshot/stream）；旧版 6 条在所有当前客户端零调用，_normalize_terminal_v2_view 也是为旧响应形状做的兼容层
  - 风险：relay 的 attach/standalone 两种模式、persistent 租约、output seq/replay、?token= 鉴权、关闭最后一个客户端即回收
- **api/oauth.py** → pkg/api/oauth_server.go + pkg/oauth/authorize
  - PKCE+S256 与 mobile/desktop 回调 scheme 是真实兼容点；但动态 OAuth client 的 DB CRUD 完全没有路由暴露，api/oauth.py:99 的 first_party 检查让其永远不可达，应删掉这层并只保留两个硬编码 first-party 客户端
  - 风险：token 端点必须是 form-encoded、code 5 分钟单次、redirect_uri 精确匹配、S256-only、authorize 同时保留 GET 重定向与 POST JSON 两种
- **core/models.py** → 按领域拆 pkg/api/*/dto.go；协议类型统一用 pkg/protocol；删除 Approval*、OAuthCallbackResponse、OAuthClientCreate/List、BulkArchiveRequest、ConnectorFsListRequest、FsListRequest、NoticeListResponse、ConnectorSessionResolution、Shell*
  - 不能整体照搬：105 个是活路由 DTO、约 30 个是嵌套、约 13 个已死；且与 core/protocol.py 重复定义 SessionView/SessionRuntimeState，字段已经漂移（models 有 runtimeId，protocol 没有）
  - 风险：字段名/nullable/default/extra=forbid 的逐字段对齐；runtimeId 在 snapshot 与 list 的不一致必须先决策；客户端用 generated TS/Swift/Kotlin 做契约测试
- **core/process_settings.py** → pkg/config/config.go（单进程；保留 AGENT_SERVER_HOST/PORT/REDIS_URL 等同名 env）
  - workers>1 需要 Redis、single_instance 与多 worker 互斥是 Python 多进程运行时产物；Go 单进程 goroutine 模型下这套校验没有意义，但 env 名称要保留兼容
  - 风险：部署脚本/Compose 仍会传 workers 等变量；忽略前要确认不会改变行为；event_workers 应映射为 ingest 并发度
- **db/migrations.py** → goose/atlas + 0001 no-op 基线（v2_44）+ 结构校验；沿用 advisory lock ID 0x414147454E545332；启动校验 alembic_version==v2_44 且 go head 匹配
  - Alembic 机制无法搬进 Go，但并发迁移锁、精确版本门禁、数据迁移只执行一次这三条语义必须保留；classify_database 的 v2_0..v2_33 形状探测是一次性历史包袱
  - 风险：丢锁会双实例同时 DDL；丢门禁会让半迁移库启动；重跑 v2_40/v2_44 的数据迁移会二次改数据/删行
- **redis_coordinator.py** → Coordinator 接口：local（per-session actor/mutex）+ pg（pg_advisory_xact_lock + 行更新）；Redis 后端保留为可选多实例实现
  - 单实例部署用不到分布式锁；但「持有 token 才能原子变更」的语义必须保留。Lua 是为了在没有事务的 Redis 上做 CAS，Go 用 PG 事务或 actor 即可
  - 风险：把 fence 降级成普通 mutex 会丢跨进程顺序；把「检查 token」和「写入」拆成两步会重现它用 Lua 修掉的 WATCH 竞态
- **shared_message.py** → publish 时同步/单 goroutine 生成 []byte 与事件元数据，随消息结构体传指针；不需要缓存任务
  - 语义（每广播只解析/编码一次、事件 id 去重、能力指纹）是对的，但 asyncio.Task/shield 是 Python 特有实现
  - 风险：Go 版若每个 socket 各自 json.Marshal，会在长回答/大工具卡上放大 CPU——这正是 SharedMessage 解决的问题
- **terminal_broker.py** → 单实例内存 registry + ring buffer（按字节上限淘汰）；终端真源改为 connector 的 terminal.list；租约保留用于 relay/browser
  - 双路径维护成本高，且分布式路径每条输出都要拿 Redis 锁并整份重写 Terminal JSON（451-503），在 2C2G 上是性能灾难；但滚动缓冲、seq 重放、租约过期的语义要保留
  - 风险：终端列表目前只存在 Redis（24h TTL），删 Redis 后服务端重启列表会空；必须补 terminal.list 重建或建 PG 表，否则 Web 终端页会凭空消失
- **fs_downloads.py** → 内存 transfer registry（token + TTL）+ http.Flusher 流式写；保留 8 chunk 的背压与 300s 过期
  - 用 Redis 列表把内存队列硬做成跨进程；单实例用 io.Pipe / 有界 chan 即可，还能省掉一次 base64 往返
  - 风险：去掉有界队列会让 connector 的快读把整文件读进内存；TTL 语义要保留，否则旧令牌可重放
- **repositories/facade.py + repositories/__init__.py** → 显式的 Store struct，字段注入 Timeline fence/publisher 与 ConnectorLifecycle 接口；删除 mixin 与 *Facade 间接层
  - mixin 组合是 Python 的代码组织技巧，不是业务语义；但绑定点（fence/publisher/sealer）是 TimelineWriteBuffer 与仓储之间的关键接线
  - 风险：store_support 的 star-import 里藏着权限/序列化/标题辅助函数，翻译时容易漏（尤其 _clean_workspace_path 与 _client_session_title）
- **repositories/store_support.py** → WithSessionRevisionFence(ctx, sid, op, fn) 中间件；密码/令牌辅助放 auth 包
  - 装饰器语义是核心：所有单会话写都必须包在 fence 内，并在结果变化时发布；其余部分是杂物，应拆分
  - 风险：忘记「有变化才发布」会产生无效广播；忘记 previous.updatedSeq 比较会重复推送同一状态
- **repositories/sessions.py** → session repo：保留 resolve_connector_session_id/binding、list_sessions_page 的复合游标、unread 公式；inventory 三阶段合并成一次 upsert + 差异计算
  - 绑定查询、未读公式、分页游标是真实需求；dsh_archive_legacy、source 扫描兼容、runtime/runtime_id 双写默认值、标题双来源是多年叠加的历史堆积，应该趁重写删掉
  - 风险：unread = latest_turn_end_seq > last_read_seq、以及 mark_read 只在 latest_turn_end_seq <= last_read_seq 时推进（timeline.py:542-549），极易被写成无条件已读；绑定查询丢 takeover 优先级会导致 /clear 后绑错会话
- **repositories/device_runtimes.py** → device_runtime + runtime_catalog 两模型；删除 retired 与 _stored_v2_descriptor 兼容分支
  - 经历 v2_14 拆表、v2_36 墓碑、v2_44 清占位三次改版，discovery/config/catalog 三份模型重叠；真正需要的是「每 connector 若干已配置实例 + active + status + config」
  - 风险：Web 端 212 处使用 runtimeType/runtimeId，字段不能删；把「未配置」与「空配置」混为一谈会改变 runtime 选择行为（schema.py:103-105 有明确注释）
- **connector_notifications** → internal/ingest/notification_router.go（按事件 kind 路由）+ internal/timeline/projector.go（去重/版本）+ internal/session/source.go
  - handler 链 + IngestEffect 两段式是过度抽象；notice 同时承载审批与普通通知，与 v2“approval 一等事件、notice 只留 input_request”直接冲突；还带多运行时/旧协议残留（session.updated 旧选择字段、hidden/localArchived/localDeleted 归档启发式、全局剥离 turnId）。但 item 级去重与版本语义是真实需求，应搬进独立 projector，而不是逐行翻译这个 1603 行文件。
  - 风险：contentHash 短路（同 id 同 hash 不分配 seq 不推送）、新 item 的 orderSeq=max+1、snapshot 时 timeline_reset_seq 栅栏、connector/runtime 绑定校验（丢了会跨 runtime 串写）、清单 scan_token 只处理本轮行且 partial 不标 missing。
- **connector_ingest** → internal/ingest/batch.go（逐事件 accepted/rejected/死信）+ internal/fanout/session.go
  - _publish_effects 把一个 session 的 items/runtimeState/session/capabilitySet/notices/catalogs 拼进同一个 nextSeq 信封，正是审计指出的“一个 sequence 挂多个事件”根源；HTTP 路径遇到 NotificationValidationError 直接整批 400（api/connector_ingress.py:443），与逐条 accepted/rejected 的目标冲突。v2 应逐事件持久化、各自分配 seq，fanout 统一在写库后做。
  - 风险：runtime_not_configured 准入必须在写库前；runtime.statusChanged 不能产生 session effect；deferred_timeline_only（纯 buffer 事件）不走 session fence、同步事件才走 fence 的区分；accepted_sequence 取 max 用于信封 nextSeq。
- **event_recovery** → internal/recovery/service.go（fence + head 快照 + 单事件单 seq）
  - 审计已拍板删掉 3 次稳定性重试，改为 session fence 内取 head、只读 <=head；当前实现还在同一 seq 上同时挂 session.meta.updated 与 runtime.capability.updated，eventId=evt_{seq}_{hash}，同一事实重放 id 不稳定，跨端无法幂等。
  - 风险：after>head 与 after<timeline_reset_seq 都必须 snapshotRequired；has_more 分页；cursor 只接受 seq:N 且不超协议上限。
- **shell_tasks** → internal/shell/tasks.go（map+mutex + per-task channel；多实例再叠 Redis pubsub）
  - 同一语义写了两套（本地 Event / Redis pubsub），2C2G 单实例部署下分布式分支是纯负担；应改为进程内 channel，仅在明确多实例时再加 Redis 广播。
  - 风险：先订阅再读状态（防止完成发生在 GET 与 SUBSCRIBE 之间）；TTL=max(300, timeout+60)；session/connector 归属校验；complete 要区分 cancelled/failed 的非法值归一为 failed。
- **terminal** → internal/terminal/service.go（统一 scope 概念，RPC 只用一种传输）
  - 同一能力有两套几乎逐行重复的 API（create/create_for_connector、close/close_for_connector、write/write_for_connector、resize*、rename*），底层一条走 RPC、一条走 WS relay；保留语义但要收敛成单一 terminal service，否则 Go 版会复制双倍代码。
  - 风险：陈旧终端清理（同 ephemeralGroupId 保留，其余关闭）；relay token 绑定；连接代际校验；关闭失败也要本地移除；result.closed=true 时视为 404。
- **Claude 顶层编排与 Provider** → internal/claude（Config/Discover/Start/Stop/Runtime），cmd/anywhere-cli 负责装配；runtime.discover 返回的 configSchema 仍用 JSON 生成，直接喂给 Server/Web 设备页。
  - 真实问题（CLI 路径发现、配置校验、模型/命令目录、单实例生命周期、审批 endpoint 与 transcript watcher 的启停顺序）都存在，但 RuntimeProvider/多 provider 包装对唯一 Claude provider 是空壳（runtimes/providers.py 只注册 ClaudeProvider）。runtime.py 792 行里真正的复杂度来自把 bridge、terminal、SDK、history、approval 五套东西塞进一个对象，Go 应按“一个 Claude runtime + 显式 turn/delivery 服务”拆分，而不是照搬类结构。
  - 风险：配置字段 executablePath/environment/modelGateway/customModels/idleTimeoutSeconds 与 schema revision/uiSchema 是 Server 和 Web 设备页在消费的，不能省；gateway 的 ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN + ANTHROPIC_API_KEY="" 以及临时 settings 文件语义要原样保留。
- **Claude 回合状态机（turns/*）** → internal/claude/turn（Controller + Execution + per-delivery TurnDriver），internal/claude/delivery/{plugin,terminal,sdk}。
  - 并发正确性和回合归属是真实问题：execution_lock/finalization_lock、queued_execution、审批 notice 生命周期都必须保留语义。但实现里混入了大量猜测式补偿：90 秒没看到新 turn 就判 idle、3600 秒兜底、用 previous_turn_id 猜归属、bridge 中断只写 abort.txt、terminal 中断只取消 watcher、turn_claude_* 自铸 ID。lifecycle.py 里约 200 行 Cron/scheduled-task 维护（AA_MAINTENANCE_DONE、reconcile_tasks）没有任何产品界面消费，Go 不应迁。
  - 风险：必须保住：同一 session 串行、排队回合不丢、审批 future 可取消并关闭 notice、回合结束前 flush 该 turn 的全部 item、bridge 路径的附件写入 .phone-uploads 并改写 prompt（模型靠 Read 才能看到文件）。不要保留 90s/3600s 的经验值。
- **插件文件桥（plugin_bridge + approval_server）** → internal/bridge/v1（文件编解码与兼容）+ internal/bridge/hub（每会话队列/令牌/审批 HTTP，Go net/http）。
  - 文件协议本身是唯一能触达引擎内部（$.prompt.submit、turn.step、PermissionRequest）的通道，必须保留；但现实现把机器级单队列、单 outbox、单 token 当作会话级设施：两终端同开会各自 submit 同一消息、outbox 的 turnId 会被别的会话覆盖、auth.json 最后写者胜出；queue 从不删除，_next_seq 每次 submit 全目录扫描；available() 只凭 30s 心跳。审批端还没有超时/EOF 处理，handler 异常直接翻译成 deny。Go 版应改成“按 external session 分目录 + 每实例 token + 显式 messageId/sessionId ack”，文件格式作为 v1 兼容层保留。
  - 风险：插件 v1 的字段名和路径必须逐字兼容：queue {id,action,text|command,args,queuedAt,token}、outbox {turnId,status(streaming|done|idle),text,chunks,ts,model,effort,cwd,git,usage}、tools entries {id,tool,input,done,output,isError}、abort.txt 毫秒时间戳、heartbeat.txt 毫秒 epoch、approval.json {port,token}、审批响应 {decision,reason,updatedInput}。还要补 queue 消费后归档/删除，否则目录会无限增长。
- **Claude SDK 投递（sdk/*）** → internal/claude/sdk：exec.Command(claude, -p --input-format stream-json --output-format stream-json --include-partial-messages --replay-user-messages --resume ...)+双向 control 协议；或 internal/claude/sdk-sidecar。
  - 语义真实（没有终端或插件时，SDK 是唯一能发起并流式接收回合的路径），但实现依赖 Python SDK，Go 无法复用。必须在 Go 里直接实现 `claude` CLI 的 stream-json 输入输出与 control_request/control_response（can_use_tool 就是控制协议上的回调），或短期保留一个只做 SDK 传输的 Python 边车；“保留 Python 边车”是迁移手段，不是最终架构。
  - 风险：最容易丢的是 can_use_tool 的阻塞审批、replay-user-messages 带来的原生 user UUID（clientMessageId 对账靠它）、include-partial-messages 的 StreamEvent、PreToolUse hook（Cron 维护和“保持权限流打开”）、resume/cwd/permissionMode/effort/model/env/cli_path 全套 options，以及 stderr 脱敏与失败信息。
- **会话发现与读取（sessions/*）** → internal/claude/session（Lister/Reader/Cache），transcript JSONL 解析器与 SDK 路径共用 timeline projector。
  - 当前完全依赖 claude-agent-sdk 的 list_sessions/get_session_info/get_session_messages，Go 没有这套 API，必须改成直接扫描并解析 ~/.claude/projects/*/*.jsonl（标题、cwd、mtime、消息归一化）或走 CLI。sessions/cache.py 的本地 timeline 缓存与 mark_synced 语义可保留；scheduled.py 是 Cron 遗留，产品没有消费面，Go 删掉。
  - 风险：session meta 的排序时间、标题、cwd 决定 Web 列表顺序；_without_maintenance_messages（AA 维护 prompt）、synthetic control（task-notification / [Request interrupted by user] / No response requested.）必须继续过滤，否则手机时间线会出现假消息；live session 与 history session 的去重规则不能丢。
- **runtime_protocol 契约（host/protocol/provider）** → internal/protocol.Host（方法集按实际使用裁剪）+ internal/claude.Runtime 接口；不再有 Provider 抽象。
  - Host 是真实端口：它负责 runtimeId 注入、turnId 剥离、sync state 命名空间和 notice/timeline 的线格式，必须保留语义。AgentRuntime 的 30 个默认 raise 方法对唯一 Claude 太宽，Go 直接把 Claude 需要的接口收窄；Provider 的 discover/validate/create 三段式可并进 claude 包。
  - 风险：Host 的方法语义不能简化成“一个 emit”：session_meta_upsert/state_update/turnEnded/notice_upsert/timeline_sync/timeline_item_upsert 各自的上报通道和字段不同；sync_state_read/write 的 key 必须继续是 runtime 前缀（claude/...）。
- **后端 WebSocket 连接与生命周期（server/client.py）** → internal/backend/conn.go（Conn + reconnector + heartbeat + capability publisher）。
  - 连接语义是真实约束：单连接、90s open_timeout（Server 正在 drain 旧连接）、loopback 不走代理、20s 心跳对 60s 服务端超时、4409 重复连接、1008/4001 凭证失效、WS 发送失败回退 ingest、关闭时 2s 有界 flush。Go 用 gorilla/websocket 或 nhooyr 重写，但状态机要更显式，不能靠嵌套 try/finally。
  - 风险：最容易丢的是“ingest 有 pending 时所有通知改走 HTTP 以保序”、发送帧过大回退 ingest、以及关闭时的最终 flush；还有 access token 提前 60s 刷新的单飞锁。重连过于激进会与 Server 的 drain 打架，形成 livelock。
- **HTTP 上行 ingest（server/ingest.py + snapshot_body.py）** → internal/backend/ingest.go（Batcher、RetryQueue、SnapshotPager、DeadLetter）。
  - 线协议和分页语义是真实约束，必须保留；但错误语义是错的：flush_loop 捕获 ConnectorIngestRejectedError 后把整个 _inflight 清空，一个永久 rejected 事件会丢掉同批所有已接受的持久事实，与审计要求的“逐事件 accepted/rejected + 死信”相反。Go 要保留批量/重试/分页，重写拒绝处理。
  - 风险：首片必须带 session.meta.upsert 且 timeline.sync complete=true，后续片 complete=false 且 item 顺序保持 orderSeq；首片失败不能重发（会清掉后续）；rejected 中的 index 必须与发送顺序对应；timeline.sync 永远走 HTTP 不走 WS。
- **后端 RPC 方法表与分发（dispatch + runtime_rpc* + local_rpc）** → internal/backend/methods.go（map[string]Handler + 显式 req/resp struct）。
  - 方法名、参数名、响应形状（ok/code/message/runtime/runtimeId）都是 Server 硬编码消费的，必须保留；但 1,780 行里大量是 RuntimeScope/RuntimeInstanceSpec 校验、只允许特定字段、payload 装饰，对单 Claude 可以压成一张显式方法表。`_resume_runtime_instance` 用 config={} 自动启动 Claude 是隐患，Go 应改成读取持久化配置后懒启动。
  - 风险：Server 的调用超时：session.capabilities/notices 10s，send_message/interrupt/interaction.respond 30s。任何方法都不能在返回前做长任务；session.sync 必须立即 accepted+后台。未知参数要容忍（Server 会多发 timelineAttachments）。错误码要稳定（unsupported/claude_turn_already_running 等会被 Server 转成 409）。
- **后台扫描器（server/runtime_sync.py）** → internal/backend/scanner.go（InventoryScan + SessionSync + CatalogPush）。
  - 这是插件缺席时的兜底主干，也是 Server 判断“哪些本机会话消失”的唯一来源（inventory complete），语义必须保留。实现可以更简单：Go 用单 goroutine 循环 + 每 runtime 一个 cursor，不必复制 recovery_generation 的多层字典。
  - 风险：inventory 的 scanToken 必须 16-128 字符且 begin/complete 成对；complete 里要带全部会话（Server 用它标记删除）；只有发布成功后才能提交 sync marker；active 会话只推 meta/state，不推 snapshot；complete=true 的替换快照在当前代码里从未产生，Go 要显式设计删除路径。
- **Runtime Host 适配与 payload 投影（server/runtime_host.py + runtime_rpc_payloads.py）** → internal/backend/host.go（按方法构造 payload）+ internal/protocol/encode.go。
  - 投影规则本身是强约束（Server 对 turnId 和 runtime 身份极敏感），但实现是 485 行的转发样板。Go 应把它做成一组小函数 + 一个 Host 实现，保留每条规则。
  - 风险：Server 对所有通知（除 session.turnEnded）递归删除 turnId/turn_id，并会拒绝 approval.requested 与旧 catalog 通知；Go 若在 timeline item 里保留 turnId 到 Server 会被静默剥掉，别指望服务端按 turn 归属。runtime.capability.updated 与 runtime.catalog.updated 的方法名/字段也不能换。
- **同步状态与运行时存储（server/sync_state.py + runtime_storage.py）** → internal/store（SQLite：sync_state、runtime_kv、pending_messages、outbox；一次性 import 旧 JSON）。
  - 持久化需求真实（断线/重启后不重放、不丢绑定），但 JSON 全量重写 + 1s flush 的形态已被审计判定为应换 SQLite（outbox/状态/绑定同库，WAL，逐条事务）。per-runtime 目录迁移是一次性历史包袱，但现有用户数据必须一次性导入。
  - 风险：key 命名空间（claude/history/cursor/{id}、claude/scheduled/sessions、claude/client-messages/...）要保留，否则旧数据读不到；flush 必须原子且失败不回退已提交 cursor；迁移要先校验两份文件再发布目录。
- **本地文件操作（local/file_ops.py + common.py + ops.py）** → internal/local/files（Root/Resolve/Browse/Read/Write/PrepareDownload）。
  - 接口字段被 Web 文件面板消费，必须保留；但实现是同步整文件读写，且 resolve_path 只做绝对化、不做 root 包含校验——绝对路径和盘符浏览是有意行为（Web 能浏览整机），不能在 Go 里误加沙箱把功能弄坏，也不能误以为它是安全边界。Go 应改成流式读写 + 原子替换 + 明确“root 只是默认起点”的注释。
  - 风险：Windows 空 path 返回盘符列表；targetType=missing 时回退到最近存在目录；ifMatch 的“文件消失但 ifMatch 为空也算过期”规则；sha256/mediaType 字段；readText 的 binary 检测（NUL）与 truncated 标志。安全边界必须写清：挡不住同用户恶意进程，真正边界是 connector token + Server 授权。
- **Connector 配置与路径（core/config.py + control_config.py + paths.py + json_kv.py）** → internal/config（Config、Load/Save、DataDir），internal/store 取代 JsonKeyValueStore。
  - 配置字段和默认值被 CLI、Desktop 控制面、Server 统计共同消费，需保留；但 pydantic/自写 JSON KV/旧 .agent-server 迁移是 Python 形态。Go 用 struct + encoding/json，KV 并入 SQLite，旧 ~/.agent-server 迁移可只做一次检测（当前用户早已迁完）。
  - 风险：connector.json 的 camelCase 字段与 0600 权限；AGENT_CONNECTOR_* 环境变量优先级；--config 路径可重定位但运行时锁固定在 ~/.agents-anywhere；数据目录 0700。
- **运行日志与控制面日志（core/runtime_logs.py + logging.py）** → internal/logging（Ring + RotatingFile + LogsService）。
  - 日志查看是 Desktop 的真实功能，但跨进程 journal 的存在理由是“多个 connector 进程都可能当 RPC host”，与新架构的单实例锁冲突。Go 可以简化为进程内环形 buffer + 按日文件（带大小轮转），只在实现 Logs 服务时保留 seq/since/clear 语义。
  - 风险：如果 Desktop 不改，Logs RPC 的 seq 语义（客户端带 lastSeq 追赶）和字段（time/level/name/message/details/exception）必须保留；日志里仍要脱敏 token。
- **本机控制面 Mesh（mesh/*）** → internal/control（loopback HTTP/named pipe + token，方法同名）或 meshcall-go 兼容层；短期 internal/control-sidecar。
  - 功能真实（Desktop 的启动/配对/日志全靠它），但 Python meshcall 运行时在 Go 里不存在。若 Desktop 不改，Go 必须重新实现 MeshCall LocalRouter 的线协议/鉴权，或临时保留 Python 边车；若 Desktop 同期改，可换成 loopback HTTP + token。方法名和错误码（connector_already_running/connector_busy/connector_error）是契约。
  - 风险：最大风险是互操作：Desktop 现在按 ready 行的 endpoint/key 建立 MeshCall；Go 若不实现同一协议，Desktop 会完全看不到 connector。stdin EOF 退出、重启同一 endpoint/key、角色权限（host 只能 call，connector 可 register）这些语义也不能丢。
- **CLI 与控制控制器（cli.py, control.py, launch.py）** → cmd/anywhere-cli（start/pair/configure/rpc）+ internal/control.Controller。
  - 命令面、退出码、配对流程是用户可见契约，必须保留；但 argparse/asyncio 嵌套任务/启动器元数据可以大幅简化。Go 用 cobra/flag + context，控制器用一个 mutex + 显式 state。launch.py 的 command() 实际无人调用，只需保留路径发现。
  - 风险：start 冲突退出码 2；pair 的 code 展示与 600s 轮询；rpc 模式 stdout 只写一行 ready、stdin EOF 退出；SHELL_LIFETIME_WARNING 可保留；stop 只断后端连接但不释放进程所有权。
- **protocol/1.0 session-snapshot-response** → Go SessionSnapshot 必须输出全部键：approvals 固定空数组，runtimeCapabilities 保留结构，catalogs 至少 model/permission；session/state 用完整字段（含 runtimeId/runtimeType/runtimeName/runtimeTypeDisplayName/effectiveRunMode/runtimeSettings）而不是缩减版。
  - 结构里混了真实需求（timeline/cursor/notices/effectiveCapabilities/catalogs）与历史包袱（approvals 永远 []、runtimeCapabilities 无客户端读、协议 SessionView 是 models.SessionView 的缩减复制且靠 extra=allow 漏字段）。真实状态见 api/sessions.py:992-1008 与 core/protocol.py:205-217。
  - 风险：删除 approvals/runtimeCapabilities 键、省略 state:null、或只输出 schema 字段，会让 iOS V2SessionTimeline.swift:17-28 整页解码失败；Web 则会出现 runtime 名称/类型空白。
- **protocol/1.0 fixtures 与契约测试** → Go contract tests + 扩充 fixtures；Swift/TS 加同一批 fixtures 的解码测试；把 Go 序列化输出与 manifest 做字节级对比。
  - 覆盖严重不足：只有 5 个 protocol fixtures（2 个 invalid），没有 snapshot、ws-ticket、event-recovery、能力扩展字段样例；README 声称 Server/Connector/Web 共用，实际只有 server 测试在读，iOS/Web 无 fixture 回放。
  - 风险：不补 fixtures，schema 与真实线上字段（runtimeId/metadata）会继续两张皮，Go 重写后再发现已晚。
- **Web 客户端字段消费与类型声明** → Go 响应 struct 以这份文件的“实际读取字段 + iOS 必填字段”的并集为准，而不是以 contracts schema 为准；后续若重写 Web，再把类型收敛回生成类型。
  - 类型文件重复定义协议且既过严又过松：Notice.updatedAt 服务端根本不发、approvals 完全没声明、runtimeCapabilities 声明但无读取、SessionView 一堆字段只搬运；同时它又包含 schema 没有的 runtimeId/metadata/effectiveRunMode 等真实字段。它不是可靠真源，但代表真实客户端需求。
  - 风险：只按 TS 类型做 Go struct 会漏掉 iOS 必填键；只按 schema 做会漏掉 Web 的 runtimeId/metadata。
- **Dashboard WebSocket 快照（未入契约）** → Go dashboardws：第一版继续发 {type,connectors,projects,sessions,runtimes,sessionPages,serverTime} 全量；把帧结构写进 contracts/protocol 或独立 schema。
  - 真实需求是一直在用的全量状态推送，但实现是“每次变更重建全量快照”，在 2C2G 上是明显浪费；同时该帧从未进入 contracts，sessionPages 永远固定 false/null、iOS 必填却从不读。短期语义必须保留，长期应改为 revision+条件 GET，但那需要改客户端。
  - 风险：删 sessionPages/runtimes/type 或改成增量会让 iOS 解码失败/Web 设备页不刷新；过早改 revision-only 需要同时改三端。
- **进程池事件预处理（EventPreparationPool）** → 把 prepare_session_events 做成一个纯函数，在广播 goroutine 里同步调用；不需要 worker pool，也不需要 3 个调参环境变量（EVENT_THRESHOLD_BYTES / QUEUE_ITEMS / QUEUE_BYTES）
  - 这是 Python 专属的性能补丁：AGENT_SERVER_EVENT_WORKERS 默认 2，在 2 核 2GB 上等于 uvicorn 1 进程 + 2 个事件进程（每个都要重新 import 整个应用），而它要解决的问题只是『把一条 256KB 以上的消息序列化一次』。语义本身（每条广播消息只派生一次、由订阅者共享）是真实的，属于 docs 里说的『per-broadcast derived data』。
  - 风险：必须保留：1) 同一广播字符串只派生一次（SharedMessage 的『相同字符串两次发布仍是两个对象、不得因同 sequence 抑制状态迁移』注释）；2) 超阈值时的背压上限（当前 32MB/32 条）改成有界 channel 即可；3) 派生失败时不得广播半成品。
- **Connector RPC 错误翻译（三份实现）** → 一个 `func (c *ConnectorClient) Call(ctx, method, params) (json.RawMessage, *ConnectorError)`，用 errors.As 在 HTTP handler 层统一映射成 409/502/504；删除 ConnectorGateway 类型
  - ConnectorGateway.request 与 services/workspace.py 的 request_connector/request_connector_bound 三段 except 完全同构；grep 'except ConnectorOfflineError' 在 server 里有 22 处、分布在 7 个文件（api/sessions.py 8 处、api/connector_runtimes.py、services/device_runtimes.py、services/session_run.py）。ConnectorGateway 本身只被 deps.py 两个依赖使用，其它路径绕过它直接拿裸异常。另外 connector_rpc.py 里的 ConnectorRpcPort Protocol 与 connector_gateway.py 的 ConnectorRpcTransport Protocol 签名一模一样，是同一抽象的第二次声明。
  - 风险：重写时最容易丢的是 409 vs 502 vs 504 的区分依据（connector 离线=409、连接器返回错误=502、超时=504），以及 ConnectorRpcError 里保留 code+message 两个字段供前端分支——压成一个 error 会让客户端丢掉可判定的 code。
- **HTTP ingest 与 WS notification 的双份后处理** → 一个 `ApplyNotification(ctx, env Envelope) (Effect, error)` + 一个 `ApplyBatch` 循环，HTTP 与 WS 共用；publish 阶段统一在调用方收口
  - ingest()（79-186 行）与 handle_notification_message()（187-234 行）里，protocol.capabilitiesUpdated / runtime.capability.updated 的 republish 条件、publish_dashboard_changed 的调用几乎逐行重复，只是批量版多了 rejected 数组和 unconfigured_runtimes 过滤；runtime.statusChanged 在每个入口各特判一次。真正的差异（批量 vs 单条、是否回写 accepted 序列）只该是参数，不该是两份代码。
  - 风险：别把 audit 文档明确要求的语义一起丢掉：ingest 的『逐事件 accepted/rejected + 死信、不得一条坏事件清空整批』是修复后的正确行为；WS 路径对 runtime.statusChanged 带 connection_id 的绑定也是并发正确性的一部分。另外两条入口的重放幂等键不同，合并时要显式保留。
- **RuntimeName 五值枚举与 KNOWN_RUNTIME_TYPES** → single source of truth：从 contracts 生成一个 `type RuntimeType string` 常量集，只含 claude；server 与 connector 都引用同一份生成物
  - 实际只有 claude 一个 runtime（connector 侧 providers.py 只返回 ClaudeProvider），但枚举在 3 个地方各写一遍：server/core/models.py:22-23（_PROTOCOL_1_RUNTIME_NAMES + LegacyRuntimeName）、connector/server/protocol.py:15、contracts 的 4 个 schema（handshake-request / capability-set / capabilities-response / model-catalog-response）。KNOWN_RUNTIME_TYPES = {claude, opencode, acp} 只被 tests/test_runtime_identity.py 引用，且包含两个不存在的 runtime。
  - 风险：protocol 1.0 的 enum 是已发布契约，收窄会让旧客户端解析失败。若还要兼容已发布 App，枚举值必须保留、但服务端应把非 claude 的值当成『不支持的 runtime』显式拒绝，而不是继续在代码里给它们留分支。
- **connector launch.py 的未用分支** → Go 里用 exec.Command + 显式 .ps1/.cmd 分支，约 30 行；不要保留 Launcher 三态抽象
  - 104 行里 command_name、existing_launch_targets、path_exists_for_launch 三个公开函数零引用，_cmd_quote 也零引用；真正在用的只有 launch_target + expand_vars。这套 powershell -File / -Command 包装是 Windows 图形启动的历史包袱。
  - 风险：PowerShell 参数引用规则（单引号内 '' 转义）是踩过坑的，重写时保留 _powershell_quote 的语义，否则带空格的路径会炸。
- **RuntimeProvider / RuntimeHostClient 抽象层** → 不要接口：一个 `type ClaudeProvider struct` + 一个 `ClaudeHost` 具体类型；如果为了测试要接口，只抽 3-4 个方法
  - RuntimeProvider ABC 只有一个实现（ClaudeProvider，runtimes/providers.py 返回单元素元组）；RuntimeHostClient ABC 也只有一个实现（ConnectorRuntimeHost），基类里大量方法是 raise NotImplementedError 的默认桩。descriptor 的 implementation_type / recommendation_rank / recommended / resource_claims 全部为单 provider 而设的通用性。
  - 风险：provider 边界里真正有价值的不是接口，而是**多实例**语义：runtimeId 是 opaque rti_*、instance name 的 NFKC+casefold 唯一性、instance_policy 的单实例互斥。这些必须原样搬到 Go 的 struct 校验里。
- **MeshCall 本机控制通道（connector/mesh/）** → 一个本地 HTTP/WS 控制端点（/control/state、/control/start、/control/stop、/logs/stream），桌面壳改成通用 fetch/WebSocket；meshcall 依赖 discard
  - 941 行 + 一个额外依赖 meshcall==0.3.0，只服务 desktop-workbench（electron/connector-supervisor.ts 直接 import meshcall，并用 MeshCall 生成的 connector-control.ts / connector-logs.ts）。对『启动 connector + 看日志』来说这是第二套 RPC 框架，且它替换的正是更早的 stdio JSON-RPC。
  - 风险：desktop-workbench 已生成强类型客户端并依赖 ready-line + 端口握手协议；重写必须同批改桌面壳，否则桌面版直接不可用。另外 mesh/logs.py 的日志环形缓冲与脱敏（log_redaction）是安全相关，别只搬传输。
- **LocalOps 纯转发壳** → `map[string]func(ctx, json.RawMessage) (any, error)` 直接注册 fs/terminal 的实现，不要 LocalOps 中间对象
  - 整文件 90 行没有一行逻辑，全是 `return await self.x.y(params)`；同时 LocalRpcHandler.dispatch 又是一串 if/elif 映射到这些同名方法，构成第二层转发。Go 里可以做一次 map[string]handlerFunc 查表。
  - 风险：notify 注入是双向的（shell 与 terminal 共享同一个 Notify），重写时别丢——terminal 的 exited/output 通知靠它发散。
- **重名的 ClaudeBackgroundTasks（两个）** → 拆成 `taskgroup.go`（runner 所有权）与 `sdktasks.go`（agent 子任务跟踪），命名区分开
  - 两个同名 dataclass 语义完全不同（一个是 asyncio 任务所有权，一个是 SDK 任务 ID 集合），跨模块 import 时极易混淆；这是包结构演化留下的命名撞车，不是设计。
  - 风险：SDK 侧 active_ids 的终态集合 {completed,failed,stopped,killed} 与 patch/status/data 三级取值是实测出来的，重写时别简化成单一字段。
- **TimelineWriteBuffer 里的 _HotpathProbe 与调参环境变量** → 删掉 probe；Go 用 net/http/pprof + runtime/trace 覆盖同一需求。保留 accept/flush/1s 物化/session_fence 本体
  - 这是给 Python 热路径做性能取证用的临时仪器（每个 mark 一次 bool 判断 + 每 delta 一个 phases 列表），默认关闭但结构性地存在于全仓最核心的并发模块里。同族还有 AGENT_SERVER_LOCK_SLOW_MS、TIMELINE_MAX_LANES、LANE_IDLE_SECONDS 等 5 个只为调 Python 表现而存在的旋钮。
  - 风险：这个模块是 audit 文档点名的『全仓库最核心的并发正确性资产』：session_fence + snapshot cursor 自洽 + 预分配 revision 区间（lease_session_revision_range）必须逐语义照搬；只删探针，别顺手重排 flush 顺序。
- **ProtocolEventEnvelope.eventId 的 sequence+hash 铸法** → eventId 由生产者铸造（producerId + producerSeq），服务端只做入库去重；`seq` 仍是服务端单调量但不再参与 id
  - audit 文档第七节已把它列为应丢弃项：sequence 是服务端分配的，同一事实重放会得到不同 eventId，跨端无法幂等去重；events.py:240 与 event_recovery.py:180 还各自按 (sequence, eventId) 排序，等于把一个不该存在的字段拉进排序键。
  - 风险：改 id 铸造方会动到客户端去重逻辑（web-next 的 session-event-buffer 按 item 只留最新 + clientMessageId 对账）；必须与 Web/iOS 同批改，并保留 cursor 的 base64 语义（parse_event_cursor/event_cursor）。
- **服务端 Windows/POSIX 路径归一化** → 协议改成传 {root, relPath}，由 connector 的 filepath.Join/Clean 解析；server 只做『不得逃逸 root』的校验（用 filepath.IsLocal 语义）
  - 同一件事在 connector 侧又写了一遍：local/common.resolve_path 用 pathlib 做 expanduser + 相对拼接 + resolve。服务器不该知道目标机器的路径规则——connector 才是唯一知道 root 和真实存在的机器；两端各一套规则必然漂移（server 处理 `C:` 盘符、`\\` UNC、`~` 的方式与 pathlib 不同）。
  - 风险：安全检查不能跟着一起下放：『path 必须落在 root 内』的判定如果只在 connector 做，恶意 server 或重放请求就能绕过——保留 server 侧的逃逸校验，只是不再自己拼接。
- **会话归档三态字段（userArchived / archiveSource）** → 会话只保留 archived + sourceAvailability 两组正交状态；userArchived/archiveSource 从 wire 里删（或保留字段但由 archived 派生，不再存储）
  - 唯一构造点 sessions.py:1906-1911 把 userArchived=aa_archived、archiveSource="user" if aa_archived else None 写死——runtime/both 两个分支从未产生；消费点只有 session_run.py:972 一处 `if session.userArchived and session.archiveSource == "user"`，等价于 `if archived`。这是 DSH/source_state 重构没删干净的半成品。
  - 风险：这是 wire 字段，Web 的 workspace-context.tsx 与 dashboard/types.ts 在读 userArchived/archiveSource；直接删字段会让前端类型校验失败。要么同批改前端，要么保留但标 deprecated。

### 保留语义（116）

- **api/admin.py** → net/http handler + pkg/api/admin.go；require_admin 包装器
  - 单租户不等于删管理面；/admin/settings 与 /admin/users 被 web-next 实际调用，自保护规则（不能改自己角色/禁用/删除自己）是真实安全语义
  - 风险：passwordVerifier+salt 存储格式、角色/禁用自保护、409/422 错误码
- **api/admin_usage.py** → net/http handler + pkg/services/usage_report.go（SQL 聚合）
  - Web ops dashboard 真实消费；事件分类、时区日聚合、库存快照、分位数都是已定义语义
  - 风险：按请求时区把 UTC hour 映射成天、分位数/留存/库存 snapshot 的算法；这是 Go 化最容易被砍但会直接让 Web 报表变空的一块
- **api/announcements.py** → net/http handler + announcement repo
  - Web 登录前公告门与 admin 页都调用；enabled 必须有 markdown 的校验是产品语义
  - 风险：公开端点必须 no-store；publishedAt 为空时不得返回内容
- **api/auth.py** → pkg/api/auth.go + pkg/auth（token/password）+ pkg/oauthlogin
  - 全部被 Web/iOS/Android 消费；bootstrap setup token、邮箱验证、密码 verifier 双通道、手机端 token audience 都是真实兼容与安全约束
  - 风险：passwordVerifier/salt 协议、setup token 一次性、重置邮箱防枚举、mobile refresh 30 天 audience、ISO 字符串按字典序比较 expires_at（auth.py:625）
- **api/client_ws.py** → POST /api/v2/ws-ticket + pkg/ws/tickets.go
  - 浏览器无法在 WS 带 Authorization；60 秒一次性 + session/dashboard 单 scope + 归属校验是真实安全需求
  - 风险：dashboard/session 二选一校验、session 归属检查、clientId 透传、Redis GETDEL 跨实例语义
- **api/connector_common.py** → pkg/api/connectors/common.go 的 helper
  - 所有 connector 级文件/终端/运行时端点共用；404 不暴露他人设备、409 offline 是客户端已依赖的错误语义
  - 风险：错误码不能统一成 500；connector_scope_id 的 browse_<id> 前缀是 fs/terminal 作用域的一部分
- **api/connector_files.py** → pkg/api/connector_files.go + pkg/fs/relay
  - 文件树、预览、write-if-match、流式中转都是 Web/iOS 在用；preview open(5m/单次)→access(15m) 的 token 链解决浏览器下载不能带 header 的真实问题
  - 风险：Windows root='' 特例、路径越界校验、单次 preview token 消费、transfer 与 connector 上传的握手、Content-Disposition 编码
- **api/connector_ingress.py** → pkg/api/connector_ingress.go + pkg/ingest/pump.go（每会话 goroutine + 有界并发 + 代际取消）
  - 全项目并发正确性最密集处：_ConnectorNotificationPump 的按 session 分 lane 保序、itemUpsert 版本合并、runtime generation 失效取消、断连 drain；ingest 是 WS notification 失败的真实回落路径（connector/client.py:374）
  - 风险：per-session 顺序、同 item 旧版本丢弃、runtime 禁用时取消 inflight、disconnect 时 10s drain、同一 connector 只允许一个 WS（4409）、access token 先选锁再鉴权
- **api/connector_runtimes.py** → pkg/api/runtimes.go + pkg/runtime/control
  - Web 设备页与 Android 设备管理全量使用；runtimeType/runtimeId 生命周期与配置 schema 是真实多运行时能力，不是 codex/dsh 残留
  - 风险：single/multiple 与 maxInstances 不变量、unavailable 必须给 reason、connector 返回非法 payload 要 502 而非 500、connector 级与 session 级 catalog 两套路径都要留
- **api/connectors.py** → pkg/api/connectors.go
  - 设备管理主链；revoke 在 lifecycle 锁内轮换 token 并踢掉活动 WS、delete 级联清理是数据一致性语义
  - 风险：revoke 与在线连接的竞态、delete 级联、preferences 仅 iOS 使用、410 路由直接删
- **api/dashboard_stream.py** → pkg/api/dashboard_ws.go + pkg/realtime/dashboard
  - Web/iOS/Android 的侧栏与会话列表都靠它；快照消息形状是客户端已实现的状态机
  - 风险：ticket scope=dashboard、每次 changed 重建全量快照在 2C2G 上很贵（可优化但不能改消息形状）、keepalive 与 recovery 1012
- **api/error_handlers.py** → net/http 中央错误中间件 + 自定义 error 类型
  - 409/504/502/500 与 404/409 的区分被客户端用于重试和提示，是真实契约
  - 风险：detail 的 JSON 形状（code/message）必须保持
- **api/pairing.py** → pkg/api/pairing.go + pairing repo
  - connector 首次上线与 Web 配对弹窗都依赖；start 公开、claim 需用户 token、poll 以 pairingId 为 bearer 的三段式是真实 onboarding 流程
  - 风险：claim 必须把 connector 绑定到当前用户并返回一次性 connectorToken；poll 过期/已 claim 状态；ttl 默认 600s
- **api/projects.py** → pkg/api/projects.go
  - Web/iOS/Android 都有项目侧栏与归档操作；pinned、sidebar 计数、名称冲突是客户端可见语义
  - 风险：project_name_conflict 的 409 code、删除时 detach 会话数、archive scope=active/archived/all
- **api/server_push_websocket.py** → pkg/ws/push.go：goroutine + context 取消 + 关闭错误白名单
  - 处理客户端断开、已关闭 socket 的 send 错误、recovery signal 触发 1012 重连，是真实的并发/资源正确性问题
  - 风险：1012 语义、cancel 后不要吞掉真实 panic、关闭后 send 的 RuntimeError 白名单
- **api/service.py** → pkg/api/admin_service.go
  - Web 管理页展示版本/DB/协调模式/uptime；运维可见性真实需要
  - 风险：x-forwarded 头解析、不泄露 DB 密码、databasePath 只到 host:port/db
- **api/sessions.py** → pkg/api/sessions.go + pkg/session/service.go
  - 所有客户端的主交互面；seq/eventCursor、snapshot 聚合、write-buffer fence、live RPC 回退、command_outcome_unknown 都是真实一致性语义
  - 风险：snapshot 的 eventCursor 自洽、timeline 三种 mode、WS 事件去重与 capability 指纹、approvals 恒空、运行时 RPC 超时回退、archive/read 的批量上限 200
- **api/sessions_fs.py** → pkg/api/attachments.go + pkg/storage
  - Web/iOS/Android 都上传附件；25MiB×5、sha256、local open URL 是真实约束
  - 风险：上传总大小限制、空文件拒绝、/local 签名 token 绑定 sessionId+fileId、CSP/Content-Disposition
- **api/shares.py** → pkg/api/shares.go + share repo
  - Web 分享页与 Android 都在用；快照不可变、allowedFileIds 白名单、仅允许 assistant message 分享是真实安全/数据语义
  - 风险：快照生成时排除 None 字段、附件白名单递归提取、公开端点无鉴权但必须校验 fileId 属于分享
- **core/announcement.py** → pkg/core/announcement.go structs
  - 公开公告与 admin 设置共用；enabled 必须有非空 markdown
  - 风险：strict/forbid unknown 字段与 20k 长度上限
- **core/api_namespace.py** → const APIV2Prefix = "/api/v2"
  - 所有客户端 URL 已固定 /api/v2
  - 风险：不要改成 /v2 或带尾斜杠
- **core/auth.py** → pkg/auth/token.go + password.go（crypto/hmac、sha256、pbkdf2）
  - token 字节格式、PBKDF2 参数、passwordVerifier 语义是跨端兼容硬约束，不是实现细节
  - 风险：connector token 的 id.exp.sig 格式、user token 的 HS256 header/payload、AGENT_SERVER_SECRET 默认值、PBKDF2 迭代次数 env 兼容、base64url 去 padding
- **core/capabilities.py** → pkg/core/capabilities.go
  - capabilityId 是客户端与 connector 共同识别的协议字符串，supported/available/allowed 三态是真实语义
  - 风险：字符串不能改名；缺省 capability 视为不可用
- **core/catalogs.py** → pkg/core/catalogs.go
  - id/selectionId 唯一、最多一个 default、model 必须有 selectionId 或 reasoning 是真实协议约束，客户端选择器依赖
  - 风险：重复 id 必须拒绝而不是去重；reasoning selectionId 也要全局唯一
- **core/connector_presence.py** → pkg/presence/lease.go
  - 被 connector_rpc 用于在线租约，字段简单但参与在线判定
  - 风险：instance_id/connection_id/ready 三字段语义
- **core/device_runtime.py** → pkg/core/device_runtime.go + jsonschema 库（如 santhosh-tekuri/jsonschema）
  - contracts/runtime-control/2.0 的线格式与校验不变量；配置 schema 由 connector 提供，服务端必须校验 Draft 2020-12 并拒绝远程 $ref
  - 风险：strict/extra=forbid、JS safe integer 上限、single⇔maxInstances=1、远程 $ref 拒绝、schema 256KB 上限；Go 的 JSON Schema 依赖是这块唯一的重依赖
- **core/email_settings.py** → pkg/core/email_settings.go
  - admin 设置页与 email-code 开关使用；apiKey 只进不出
  - 风险：apiKey 永不序列化、clearApiKey 语义
- **core/events.py** → pkg/core/events.go
  - seq:N cursor、eventId 去重、timeline/runtime/notice/catalog/refetch 七类事件是 clients 的重连恢复语义
  - 风险：eventId 由 type+payload 哈希生成，换算法会导致客户端重复；cursor 禁止前导零；capability 事件指纹抑制不能丢
- **core/oauth_clients.py** → pkg/oauth/first_party.go
  - mobile/desktop 的回调 URI scheme 已固化在客户端二进制里，不能改
  - 风险：agents-anywhere://oauth/callback 与 agents-anywhere-desktop://oauth/callback 精确匹配
- **core/protocol.py** → pkg/protocol/v1（建议直接由 contracts JSON Schema 生成 Go struct）
  - contracts/protocol/1.0 的 manifest 明确 source=此文件，web-next/src/generated 由它生成；这是唯一必须逐字段照搬的模型层
  - 风险：extra=allow 的扩展字段、序列化时默认值必填、PROTOCOL_MAX_REVISION=2^53-1、eventCursor 正则、snapshot 的 approvals 恒空但字段必须存在
- **core/runtime_identity.py** → pkg/runtime/identity.go
  - Runtime Control 2.0 与 sessions 绑定都依赖；legacy runtime==runtimeId 的回退是已有数据兼容语义
  - 风险：小写归一化、保留 rti_ 前缀、长度 64/128、legacy 判定、connector 返回 identity 不一致要 502
- **core/setup_token.py** → pkg/auth/setup_token.go（进程内 mutex；单实例可去掉 Redis 协调）
  - 防止公网首个访客抢占管理员，是真实安全机制；TTL 自动再生与 consume 语义被 /auth/config 与 /auth/register 依赖
  - 风险：日志 banner 格式（运维靠它复制）、15 分钟 TTL、过期自动再生、成功后一次性失效
- **core/timeline.py** → pkg/timeline/merge.go
  - 被 timeline_write_buffer 与 repo 使用，决定 item revision/seq 是否推进，直接影响客户端去重与事件
  - 风险：只比较 contentHash 不比较 content、重复 id 取批内最后一条、revision=max(incoming, existing+1)
- **core/usage.py** → pkg/usage/events.go
  - 9 种事件字符串、dedupe_key、按小时聚合是报表 SQL 的输入契约
  - 风险：event_type 字符串不可改；occurred_at 必须 UTC ISO；device_os/resume 维度；历史数据要能继续读
- **core/utc.py** → pkg/core/utc.go：time.Now().UTC().Format("2006-01-02T15:04:05.000000Z07:00") 风格的 Z 输出
  - 所有响应的 serverTime 格式；且 auth.py:625 用字符串字典序比较过期时间，格式变化会破坏逻辑
  - 风险：必须是固定长度 UTC + Z，不能输出 +00:00 或本地时区；微秒精度要一致
- **db/schema.py** → pgx + sqlc/goose 生成的 struct；时间保持 string，修订号 int64；基线以线上 v2_44 库为准
  - 表结构是与既有数据和未改动客户端之间的硬契约：Text 时间列、Text JSON、BigInteger 修订号都不能换类型；payload_json 是读取权威
  - 风险：换成 timestamptz/jsonb 会破坏客户端字符串比较与现有 fixtures；把 payload_json 与索引列改成单写会读回旧内容
- **db/engine.py + db/__init__.py** → pgxpool，保留同名环境变量；__init__ 的 re-export 在 Go 里不需要
  - 连接池参数是运维契约，AGENT_SERVER_DB_* 环境变量已被部署脚本使用
  - 风险：默认 10+20 个连接在 2G 内存上偏大，Go 版应下调并保留可配
- **ws_tickets.py** → 进程内 map[string]ticket + TTL 清理；多实例时才用 Redis GETDEL
  - 简单、正确、够用；是 WS 鉴权不把长期 token 放进 URL 的关键
  - 风险：改成可重复使用或放宽 scope 校验会让 ticket 变成会话劫持入口
- **timeline_broker.py** → 进程内 hub：订阅表 map[string]map[*sub]struct{} + 每 socket 有界 buffered chan；发布方 encode 一次生成 []byte
  - 扇出、单次编码、refetch 退化、去抖都是真实需求；不保留的是 Redis 传输层而不是这些语义
  - 风险：丢最旧帧后必须让客户端能凭 seq 发现缺口；1MB/4 的 refetch 阈值不能省，否则大工具卡会撑爆广播路径
- **timeline_store.py** → pgx 手写 SQL 或 sqlc；保留 ON CONFLICT ... DO UPDATE ... WHERE excluded.updated_seq >= timeline_items.updated_seq
  - 单调守卫、稳定排序（orderSeq, updatedSeq, id）、分页接口都是并发正确性与协议语义的一部分
  - 风险：payload_json 才是读取权威，索引列只是影子；Go 若只更新列不更新 JSON，读接口会返回旧内容
- **terminal_stream_hub.py** → 进程内 hub + 每连接 buffered chan；删除 Redis terminal-stream 分支
  - 这是「慢浏览器不能拖垮 relay」的正确做法，逻辑与语言无关
  - 风险：丢掉 seq 去重会造成终端重复输出；丢掉 overflow 断开会让内存随慢客户端增长
- **connector_rpc.py** → 进程内 conn registry + request id → chan response；多实例路由删除；租约 TTL 改为本地 last_seen + 5s watchdog
  - 单连接租约、代际校验、drain 都是真实并发与安全需求（重连风暴、旧连接回写状态）；只有跨实例路由是单实例不需要的
  - 风险：去掉租约会出现双连接双写；去掉 connection_id 代际校验会让旧连接在重连后继续应用 runtime 状态（services/device_runtimes 用 expected_connection_id 防这个）
- **connector_gateway.py** → sentinel error + errors.Is/As 包装
  - 错误分类直接决定 HTTP 状态码与客户端重试策略，是 API 契约
  - 风险：把 offline 与 timeout 混成 500 会让客户端在连接器离线时无限重试
- **files/base.py + local.py + s3.py + __init__.py** → FileStore 接口 + OS/S3 实现（官方 AWS SDK v2 或 minio-go）
  - 双后端与 sidecar 元数据模型是真实需求；附件没有数据库表，这些文件就是唯一真源
  - 风险：S3 后端依赖私有 gitlab 的 httpx-s3-client，Go 没有对应包；路径校验必须原样保留（防穿越）
- **repositories/timeline.py** → 同一事务内 pg_advisory_xact_lock(hash(sid)) + SELECT ... FOR UPDATE；其余 SQL 语义照搬
  - 租约、水位、reset 围栏、revision 归一化、orderSeq 稳定性都是并发正确性核心；只有锁的获取方式可以改
  - 风险：现在 _timeline_lock（589-605）在独立连接上持 session 级 advisory lock，等于每次写占两个池连接；Go 若照抄会在并发下耗尽连接池。advisory lock 键必须与 Python 版一致（sha256 前 8 字节 signed）
- **repositories/connectors.py** → connector repo；token 哈希保持 SHA-256，访问令牌保持 HMAC-SHA256 格式（core/auth.py:19-60）
  - 配对与令牌轮换是安全边界；revoked 检查贯穿所有查询；已配对设备不能因为重写掉线
  - 风险：访问令牌格式或密钥派生一改，所有在线 connector 会集体 401；撤销检查漏一处就是越权
- **repositories/projects.py** → project repo；保留 (user,connector,workspace_key) 与 (user,name) 两个唯一约束
  - 项目是会话的强制前置（每个 session 必须有 project），唯一约束与 RESTRICT 外键是数据一致性核心
  - 风险：把 RESTRICT 改成 CASCADE 会连带删掉会话与时间线；workspace_key 归一化规则（大小写/分隔符/设备平台）必须逐字照搬
- **repositories/users.py** → user repo + x/crypto/pbkdf2 + hmac；令牌格式保持 pbkdf2_sha256$salt$b64
  - 认证不能改语义（已有用户与已登录设备）；密码是 PBKDF2-SHA256 120k 轮，Go 的 x/crypto/pbkdf2 可以直接兼容
  - 风险：迭代次数或编码方式一变，所有存量用户无法登录；删除用户时「至少保留一个管理员」的检查不能丢
- **repositories/email_accounts.py** → email repo + 同一套哈希/窗口语义
  - 验证码哈希、nonce、失败窗口是安全逻辑，不是可简化项
  - 风险：把失败计数或窗口放宽会打开暴力破解；验证码哈希算法换了会让未过期验证码全部失效
- **repositories/oauth.py** → oauth repo；PKCE 校验与 code 单次消费保持原子
  - Web 与 iOS 的原生 OAuth 登录在用（web-next/components/auth/*），协议与存储不能变
  - 风险：授权码消费不是原子的会导致重放；provider/subject 主键语义不能改成 user_id
- **repositories/usage.py** → usage repo；SQL 聚合可保留，或改成物化视图（但输出字段名不能变）
  - append-only + dedupe_key 唯一约束是幂等边界；管理后台直接消费这些聚合
  - 风险：去掉 dedupe_key 会让重试的连接器事件重复计费/重复计数
- **repositories/shares.py** → share repo，保持 snapshot 不可变
  - 分享是「创建时快照」，不是实时视图；web 的 /share 页面在读这份 JSON
  - 风险：改成实时读取当前会话会改变已发出链接的内容与隐私边界
- **repositories/attachments.py** → attachment repo + SELECT ... FOR SHARE OF sessions；文件 I/O 走 FileStore
  - 写围栏保证「删除设备时不会留下孤儿 blob」，是真实的竞态修复
  - 风险：把 FOR SHARE 换成普通读会重现孤儿文件；session 归属校验（connector.revoked=0）不能漏
- **repositories/active_runs.py + active_runs_facade.py** → active run repo；也可以合并进 session repo，但表保留
  - 语义与 sessions.status / runtime state cache 不同：这里是平台侧的「正在跑」事实，管理端统计直接 select count
  - 风险：删表会让 usage 报表的 active_runs 指标归零；用 sessions.status 代替会把「终端里用户自己跑的回合」算成平台活动
- **repositories/instance_settings.py + instance_settings_facade.py** → instance settings repo（key/value/updated_at）
  - 部署配置的唯一持久化位置，管理端在改
  - 风险：默认值写错会意外打开注册；键名必须逐字照搬（SETTING_* 常量在 store_support.py:81-84）
- **repositories/protocol_catalogs.py** → catalog repo；保留 revision CAS 与身份不可变校验
  - 三态语义决定连接器是否重发目录；id 不可变检查防止 runtime 身份漂移
  - 风险：把 stale 当成 conflict 会让连接器无限重发；把 idempotent 当成 accepted 会触发不必要的 WS 广播
- **timeline_write_buffer** → internal/timeline/buffer.go（per-session mutex + pending map + flush loop + fence）
  - 审计明确认定 session_fence + 1s 物化是全仓库最核心的并发正确性资产，保证 snapshot cursor 自洽并避免 30Hz 写库，不是历史包袱。实现可以换，语义必须原样保留。
  - 风险：fence 可重入（现在靠 ContextVar 短路，Go 要显式 fence token）、published watermark 落后时补 refetch 边界、clear_snapshot 只删与快照一致的版本（CAS）、lane 淘汰不能淘汰持锁 lane、Redis epoch 变化要清 lane 缓存、flush_all 一个 lane 失败不能中断其他 lane。
- **session_revision_allocator** → internal/timeline/revision.go（单实例可用进程内 counter + DB 租约，保留 published 水位与 repair 信号）
  - 解决“先推送后落库”下的重号/跳号与跨实例唯一性，是真实的数据一致性约束；租约机制保证 Redis 崩溃后已发出去的号不会二次使用。
  - 风险：分配前必须检查 head>published 否则抛 repair；seal/floor 只能单调上调；PROTOCOL_MAX_REVISION 溢出；Redis 锁所有权现在是隐式的（*_while_lock_owned 只传 key），Go 要显式 token 或互斥。
- **session_run** → internal/session/command.go + internal/session/admission.go
  - 这些是真实业务约束（project/workspace 必须匹配、runtime 必须 ensure_running、未 takeover 只读、capability 缺失不许写、附件 sha256/size 校验、失败必须 clear_active_run）。但外层同步 RPC 返回结果、legacy create_session 分支、inline base64 附件属于旧协议壳，v2 命令/ack 形态下要改。
  - 风险：所有失败路径都要 clear_active_run；必须校验 connector 返回的 runtime binding；capability 查找的 runtimeId 精确优先、无 runtimeId 的 runtime 级 fallback；attachment 的 sha256/size 与 allowedMimeTypes 校验。
- **device_runtimes** → internal/runtime/manager.go
  - 这是一个真实状态机：连接代际校验防旧连接写入、delete 失败回滚 ingress、start 期间拒绝重复 start、active 状态忽略 discover 覆盖。不是多运行时残留，是多实例 runtime 的必要管理。
  - 风险：锁序 connector_lifecycle → runtime_lock；is_connection_id_current 的每个检查点；delete 异常时用剩余配置恢复 ingress；_settle_runtime_sessions 必须在 session fence 内并发布 refetch；named instance 的 requiredForNamedInstance 合并。
- **effective_capabilities** → internal/capabilities/effective.go + internal/capabilities/publisher.go
  - 这是“手机按钮能不能点”的唯一准入真相，涉及协议兼容与权限语义；WeakKeyDictionary 只是实现细节，可替换。
  - 风险：4 级查找优先级（session+runtimeId → session → runtime+runtimeId → runtime）；unavailableReason 判定顺序（unsupported → offline → not taken over）；revision=max(session.updatedSeq, runtime revision)；批量发布必须可被更新一轮取代，否则旧能力会覆盖新能力。
- **connector_presence** → internal/presence/presence.go
  - 在线状态是路由与能力可用性的前置条件，语义简单且真实。
  - 风险：批量 online_statuses 与逐条 fallback 的结果必须一致；投影只改 status，不能覆盖其他字段。
- **session_meta_projection** → internal/session/projection.go
  - 纯投影，移动端列表状态依赖它；逻辑 38 行，没有抽象负担。
  - 风险：runtime cache 只能覆盖 externalSessionId/status，不能覆盖 title/cwd 等 connector 权威字段。
- **session_runtime_state_cache** → internal/cache/runtimestate.go（map+mutex 或 Redis Lua）
  - runtime state 是可重建的派生数据，但“旧状态不许覆盖新状态”与刷新去重是真实并发需求；单实例可退化成带 TTL 的内存 map。
  - 风险：相等 updatedSeq 允许覆盖；discard 必须发生在删除会话/停止 runtime/删除 connector 时；refresh_guard 的 timeout=0 语义（拿不到锁就跳过）。
- **ingest_effects** → internal/ingest/effect.go（Go struct，建议与重写后的事件模型合并，减少字段）
  - 字段本身表达了“这次通知产生了什么”，但作为独立模块是过度抽象；语义要保留，结构可并入 ingest 包。
  - 风险：timeline_pending / timeline_published / deferred_timeline_only 三个布尔共同决定是否走 fence、是否立即推、是否等 flush，合并时最容易丢。
- **connector_realtime** → internal/connector/realtime.go
  - 是真实数据通路（终端 PTY 字节、shell 任务完成），且必须在通知链最前面短路，避免进入业务 handler。
  - 风险：base64 解码失败吞掉但 exit 仍要传播；exitCode 必须是 int；shell task 的 connector 不匹配要忽略而不是报错。
- **connector_rpc** → internal/connector/errors.go（typed errors + errors.Is/As）
  - 错误分类直接决定 API 返回 409/502/504，是协议兼容的一部分；文件本身很小。
  - 风险：HTTP 状态映射：offline→409、upstream→502、timeout→504、protocol→502。
- **connector_files** → internal/connector/files.go
  - 真实文件传输链路，涉及 token/transfer 生命周期，不能拍平。
  - 风险：prepareDownload 返回值必须校验是 dict；size/sha256 缺失时的默认值；transfer token 与 uploadUrl 的绑定。
- **connector_shell** → internal/shell/service.go
  - 任务取消语义是真实资源清理需求；超时值换算与 310s 上限是协议约束。
  - 风险：等待超时/取消必须 cancel connector 侧任务（否则泄漏进程）；start 失败要 abandon；exec 超时按 timeoutMs+5s。
- **terminal_relay** → internal/terminal/relay.go
  - 幂等 attach（已注册且已绑定就直接返回）、写请求绝不重试，都是真实正确性约束。
  - 风险：先查后锁再查的两阶段幂等；terminal_not_found 映射 404；异常时 remove 注册；wait_connector 超时 10s。
- **workspace** → internal/workspace/path.go（filepath.VolumeName/UNC 处理）
  - connector 在 Windows 本机，路径语义必须逐条保留；这是纯函数，最适合直译。
  - 风险：`~` 不展开原样返回；`/D:/x` 去前导斜杠；无盘符的根相对路径拼到 root 盘符；cwd 为空必须报错。
- **catalogs** → internal/catalog/service.go
  - selection→model/permission 的映射是客户端选择的协议语义；但摄入路径与通知 handler 重复，重写时应只留一条。
  - 风险：catalog 未就绪时返回 catalog_unavailable；无效 selection 返回 invalid_selection；revision 冲突必须报错。
- **model_catalog** → internal/catalog/model.go
  - 纯查找，客户端选择协议的一环。
  - 风险：reasoningItems 的 selectionId 与 model 的 selectionId 同域，先匹配 model 再匹配 reasoning。
- **permission_catalog** → internal/catalog/permission.go
  - 纯查找，协议语义。
  - 风险：返回的是 {permissionMode: id} 而不是 id 本身，别改形状。
- **attachments** → internal/attachment/service.go
  - 路径穿越校验与blob 完整性是安全需求；签名 URL 是协议字段。
  - 风险：file_id 含 / 或 \\ 或非 file_ 前缀一律 KeyError；读取后必须复核 sha256/size；connector 读取必须先校验 session 归属。
- **oauth** → internal/auth/oauth.go
  - 安全边界（state 防 CSRF、签名 token、clientSecret 不外泄）必须原样；实现可直译。
  - 风险：state 必须校验 provider；public config 必须剔除 clientSecret；用户名截断/补足规则（3-32 字符）与 USERNAME_RE 一致。
- **email_delivery** → internal/mail/resend.go
  - 安全需求明确（错误脱敏、sender 校验、6 位数字码）；外部依赖可在 Go 用 net/http 直译。
  - 风险：错误信息不能带 provider 响应；fromAddress 的 CRLF 注入校验；apiKey 的 ASCII/空白校验。
- **setup_tokens** → internal/auth/setup_token.go
  - 初始化安全路径，防重放/防陈旧值回写；单实例可退化为内存+过期，但比较与消费语义要保留。
  - 风险：本地值过期或已被消费时绝不能把陈旧值重新写回 Redis；比较必须常量时间；consume 失败不阻塞管理员创建。
- **announcements** → internal/announcement/service.go
  - 纯数据变换 + 一个跨 worker 锁，客户端用它判断是否显示新公告。
  - 风险：内容未变直接返回原对象（不推进 publishedAt）；enabled=false 时保留旧 publishedAt；+1ms 单调性。
- **dashboard_events** → internal/fanout/dashboard.go
  - 是移动端列表刷新的唯一信号，轻量；reason 字段被有意忽略。
  - 风险：查不到 user 时必须静默返回；只发信号不发全量快照（审计已要求改成 revision/ETag）。
- **runtime_ingress** → internal/ingest/admission.go
  - 防止 runtime 删除后延迟到达的写入把会话写回来，是数据一致性需求；虽然实现是纯谓词，语义不能丢。
  - 风险：runtimeId 缺失时按 metadata.runtimeId → runtime 回退；无 runtimeId 且存在未配置 runtime 时全部拒绝；只拦 session./timeline. 前缀。
- **usage_events** → internal/usage/recorder.go + internal/usage/events.go
  - 统计是产品功能，dedupe_key 与节流窗口是真实需求；但它是旁路，绝不能影响主链路。
  - 风险：写库失败必须吞掉；start 前同步写（测试确定性）；turn duration 超过 6h 返回 null；同一 session 最近 message 时间的内存缓存。
- **usage_report** → internal/usage/report.go（时区用 time.LoadLocation，需要嵌入 tzdata）
  - 纯数据变换（输入 SQL 聚合行，输出响应 DTO），与运行时/协议耦合低，可 1:1 直译；不是历史包袱。
  - 风险：半小时/45 分钟时区的日边界归并规则、广窗口（rolling 29 天、retention 30 天）、live vs snapshot 的选择、百分比/分位算法。
- **usage_maintenance** → internal/usage/maintenance.go（time.Ticker + 单实例锁）
  - 幂等快照 + 保留期是真实运维需求，逻辑很小。
  - 风险：快照按日期存在性幂等；裁剪按 hour bucket 字符串比较；循环异常不能退出。
- **connector_deletion** → internal/connector/deletion.go
  - 删除顺序与可重试性是数据一致性需求（避免残留 pending 投影和持有连接）；pending 状态必须落库而不是只在内存。
  - 风险：只处理显式 pending（状态 2），不碰 legacy revoked（1）；KeyError 视为别的 worker 已完成；失败保留 pending 下轮重试；删完后发 dashboard.changed。
- **Transcript 历史同步（history/* + watch/* + background）** → internal/claude/history（Tailer、CursorStore、Syncer）+ internal/claude/watch。
  - 这是插件缺席时时间线仍能出现的命脉，也是断线恢复与“终端里用户自己打字”的唯一事实来源；逻辑小且解决真实问题。Go 可用 fsnotify + 250ms 轮询兜底，cursor 语义必须照搬。
  - 风险：cursor 的指纹（lastModified/fileSize/messageCount/lastMessageUuid）决定增量边界；active SDK 回合要跳过历史同步，但 terminal 回合必须同步；替换快照在 active 时要延后且不提交 checkpoint；一条会话失败不能拖垮其他会话。
- **时间线投影（timeline/*）** → internal/claude/timeline（Projector、StreamAccumulator、ToolContent 映射）。
  - 业务语义真实且被 Web 深度消费：工具卡、Diff、agent_call 父子关系、reasoning 折叠、流式 partial 覆盖都依赖 content.kind 和稳定 id。Python 实现只是数据转换，Go 可逐字段照搬。
  - 风险：item id 与 contentHash 是持久化键（见 ID 兼容条目）；tool item 必须以 tool_use_id 为唯一键，call 与 result 不能生成两条；agent_call 的 parentItemId 决定子代理卡片嵌套；file_change 的 diff 文本格式（---/+++/@@）被 Web DiffPanel 解析；orderSeq 必须单调且在 bridge partial 与历史合并时复用。
- **Claude 领域模型（domain/* 除 pending_messages）** → internal/claude/domain（ApprovalNotice、InputRequest、PermissionCatalog、ModelCatalog、Capabilities、Selection、Session/Execution）。
  - 这些是纯业务语义，与 Python 无关，是 Go 重写最好的规格书；notice 的 actionId、context.approvalStatus/inputStatus、blocking 形状被 Web 审批卡直接读取，不能改。但 capabilities 里有两条错误语义要在 Go 修正：commands 只看 terminal_attach.enabled()（Windows），而 bridge 也能执行命令；session.send_message 在 active 时标记 unavailable，实际 bridge 路径会排队接受。
  - 风险：actionId 必须是 approve/reject/submit/cancel；AskUserQuestion 的 answers 映射（多选用逗号连接、问题文本做 key）和 ExitPlanMode 的 updatedInput 回显；权限 selection id 与模型/effort selection id 被持久化，不能变；send/interrupt 的 available 规则要按真实投递能力算。
- **clientMessageId 对账（domain/pending_messages.py）** → internal/claude/pending（SQLite 表 pending/matched，匹配函数保留 bisect 式“最近/最早”策略）。
  - 这是乐观 UI 去重与“历史里那条 user 消息就是我刚发的那条”的唯一 join key，属于真实数据一致性需求。算法不依赖 Python，可照搬；但 512 行里一半是 JSON 持久化样板，Go 用 SQLite 表可缩到几十行。
  - 风险：文本归一化（逐行 rstrip + 整体 strip）、附件 echo 的 "Attached files:\n" 前缀处理、未解析 session 的暂存与 bind_external_session 迁移、128/1000 的有界裁剪。少了任何一个，重启后会出现重复 user 气泡或错误合并。
- **通知投影（notifications/*）** → internal/claude/notify（NoticeRegistry、StateReporter、TimelineSink）。
  - 薄但真实：notice 需要本地可查询以支持 respond_interaction 和 close_open_interaction_notices；state cache 的合并/去重避免每次 token 都上报。Go 直接照语义实现，不需要单独 4 个文件。
  - 风险：TERMINAL_NOTICE_STATUSES（closed/resolved/cancelled/expired）决定哪些卡还挂在手机上；notice upsert 必须先落本地注册表再发网络，否则 respond 会找不到 notice。
- **Windows 控制台注入与会话发现** → internal/winconsole（独立 helper 子进程执行注入）+ internal/claude/terminal（session 发现、spawn、wait-idle）。golang.org/x/sys/windows 的 FreeConsole/AttachConsole/CreateFile/WriteConsoleInputW/VkKeyScanW。
  - 这是 Windows 上把消息送进“用户正在看的同一个进程”的唯一路径，任何纯 SDK/PTY 方案都替代不了 TUI 可见性和上下文一致性。必须用 Go 重实现 Win32 调用，但语义（只对 live interactive、按 sessionId 匹配、pid 存活、Enter 用 \r、idle 连续 3 次才算结束）保留。trust-dialog 写入是用户明确授权的窄口子，不能扩大。
  - 风险：最大坑是 FreeConsole 会把 connector 自己的控制台也摘掉：如果 connector 从终端启动，注入后它自己的 stderr 就没了。Go 应把注入放进短命 helper 子进程，而不是主进程线程。另需处理 Windows Terminal/ConPTY 下 AttachConsole 的行为、UTF-16 代理对、VkKeyScanW=-1 的字符、以及 status=busy→idle 的抖动。
- **模型/命令目录发现（catalogs/*）** → internal/claude/catalog（Discovery 接口，可由 SDK transport 或 `claude --help`/初始化探测实现）。
  - 让模型选择器跟随 CLI 升级和账号权限，而不是维护硬编码表，这是真实产品价值；commands 也来自 CLI 才能包含 skills/plugins。Go 若实现了 SDK control 协议可直接复用；否则只能退回静态表 + customModels，功能会退化。
  - 风险：catalog revision 在 Server 侧有 catalog_revision_conflict 校验（内容变但 revision 不增会被拒）；selection id 是持久化的；静态表只是 fallback，不能让它覆盖 CLI 结果。
- **runtime_protocol 线格式模型（models/timeline/interactions/agent_calls/attachments）** → internal/protocol/timeline（struct + 自定义 MarshalJSON）；internal/protocol/session。
  - 这是 Server/Web 真正消费的 JSON 契约，必须逐字段保留；但可以裁剪到 Claude 实际生产的子集：message(markdown)、tool(command/file_change/agent_call/mcp/tool_call/tool_result/web_search/input_request)、system(reasoning/system/error/unknown)。turn.start/turn.end、marker/artifact、MultimodalMessageContent、PermissionToolContent 没有任何生产者（host.py 还专门过滤 turn.*），Go 删掉。
  - 风险：字段名与枚举拼写（orderSeq、contentHash、inProgress、waiting_approval、itemId/itemType/clientMessageId、source.runtime）不能改；content 的 JSON 形状直接进入 contentHash，改 key 顺序虽会被 sort_keys 抵消，但增删字段会改变哈希并触发全量重发。interactions 的表单解析（question_id/option_id、allow_custom）是 AskUserQuestion 回填的基础。
- **后端 RPC 帧通道（server/rpc.py）** → internal/backend/rpc.go（Frame、sendQueue、generation、critical semaphore、sanitizer）。
  - 这些是并发正确性资产：WebSocket 并发写会崩、慢 RPC 会饿死交互审批、旧连接的 response 不能写给新连接、日志里不能出现 token/环境变量。Go 应逐条保留，只是换成 goroutine + channel。
  - 风险：CRITICAL_RPC_METHODS（interaction.respond、session.notices）必须保留独立配额，否则流式/终端 RPC 会阻塞审批；send_json 必须串行；900KB 限制要保住，超限转 HTTP；脱敏 key 列表（token/password/environment/authorization...）不能删。
- **通知合并（server/notification_coalescer.py）** → internal/backend/coalesce.go（per-session channel + 50ms timer + barrier）。
  - 这是真实的带宽/CPU 优化，也是顺序屏障；审计明确建议保留语义并把窗口降到 50ms、加批量上限。Python 的 per-session lock + task 结构在 Go 里用每 session actor 更自然。
  - 风险：屏障语义：非 assistant 的 timeline.itemUpsert（工具/系统）以及 session.state/notice 必须先 flush 该 session 的 pending，否则手机会先看到“工具卡”再看到之前的文字，顺序错乱；session 关闭时要 flush 而不是丢弃。
- **终端中继（server/terminal_relay.py）** → internal/backend/terminal_relay.go（帧编解码 + relay loop）。
  - 这是终端面板的线协议，帧类型和 seq/baseSeq 补发语义是真实约束；PTY 输出绝不能 await 网络（否则终端卡顿）。Go 用 goroutine 读 PTY + 有界环形 buffer + WS 写协程即可，语义照搬。
  - 风险：start/ready/replay/output/exit/response/error 帧的字段名；fromSeq 增量与 baseSeq 回退到全量 replay 的边界；关闭码 1008/4401/4404 不重连；attach 模式 vs create 模式的销毁语义；release 不能把新连接的 output sink 摘掉。
- **传输/配对/认证/URL 辅助（server/transfers.py, pairing.py, auth.py, urls.py）** → internal/backend/httpapi（Auth、Pairing、Attachments、URLs）。
  - 都是对 Server 现有 HTTP API 的薄封装，路径与头格式是硬约定；Go 用 net/http 直接实现即可，逻辑几乎不变。
  - 风险：路径前缀 /api/v2；认证头 `Connector <id>:<token>` 与 Bearer accessToken 两种不能混；WS 的 X-Device-OS；loopback 不读代理环境变量；附件下载 300s 超时与 X-File-Name/Content-Type 头。
- **本地终端 PTY（local/terminal.py + terminal_platforms.py + terminal_records.py）** → internal/local/terminal（Backend + Scrollback + Reaper），平台实现 conpty_unix.go / conpty_windows.go。
  - 这是产品终端面板的后端，生命周期和 seq 语义真实且与 Claude 投递无关。Go 用 ConPTY 是原生一等公民（CreatePseudoConsole/ResizePseudoConsole/ClosePseudoConsole），Unix 用 creack/pty；TTL、persistent lease、relayOnly 语义照搬。
  - 风险：seq/baseSeq 与 scrollback 截断关系（截断后首包必须能触发 replay）、persistent 的 24h lease 与 idle TTL、relayOnly 时不再发全局 notification、写 PTY 要用阻塞线程/goroutine 不能卡事件循环、关闭记录 15 分钟保留以让面板看到 exit。
- **本地 Shell（local/shell.py）** → internal/local/shell（Exec、TaskManager）。
  - Server 的 shell 面板直接消费这些字段与通知，超时/取消/截断是真实行为。Go 用 os/exec + context + 进程组（Unix Setpgid / Windows taskkill /T /F）即可。
  - 风险：Windows 用 taskkill /T /F 杀整棵树；Unix 用 killpg；输出只保留前 64k 且带 truncated 标志；task 取消与超时都要发 shell.task.completed；cwd 必须落在 root 下解析。
- **单实例所有权与机器记录（core/runtime_owner.py）** → internal/owner（RuntimeLease、StateTransaction、processIdentity）。
  - 这是 contracts/local-machine/2.0 的硬契约，Desktop 会并发写同一文件；PID 复用、僵尸进程、权限失败都必须按契约处理，不能用普通文件锁替代（跨进程/跨语言）。Go 需逐字段复刻 JSON 与租约探测协议。
  - 风险：最容易被简化掉的是“锁记录本身是一个 127.0.0.1 监听端口 + token 应答”，不是 flock；只检查 pid 会被 PID 复用击穿；未知字段要保留；Desktop 写 desktop/onboarding 时不能覆盖 runtime/connectorIds。
- **持久化 ID / 哈希兼容层（session_identity + protocol_selection_id + timeline hashes）** → internal/protocol/ids.go（sessionID/selectionID/itemID/contentHash 四个函数 + canonical JSON helper）。
  - 这些值已经写进 Server 数据库、Web 本地缓存和会话配置里，是升级兼容的硬约束。Go 必须逐字节复刻公式与 JSON 规范化，否则升级后所有会话会被当成新会话、已选模型失效、时间线全量重发。
  - 风险：精确公式：session `sess_{runtime}_` + sha256("{connector_id}:{runtime}:{external_session_id}")[:24]；selection `sel_{catalog}_` + base64url(sha256("1:{runtime}:{catalog}:{canonical_json}"))[:24]（去 padding）；item `claude_msg_/claude_tool_/claude_system_` + sha256(json.dumps(parts, ensure_ascii=False, sort_keys=True, separators=(",",":")))[:24]；contentHash = "sha256:" + sha256(canonical {type,status,role,content})。Go 的 encoding/json 要 SetEscapeHTML(false)，并确保切片/映射序列化与 Python 一致。
- **protocol/1.0 契约目录与生成链** → Go internal/protocol 包：手写/生成 Go struct，字段名与 JSON tag 严格固定；保留 manifest 与 JSON Schema 导出（Go 生成器或保留一个只做导出的 Pydantic 影子模型）。
  - 协议兼容、eventId/cursor、snapshot、自检 sha256 都是真实需求；但 schema 只导出 Pydantic 声明字段，extra=allow 的 runtimeId/metadata/SessionView 扩展字段没进 schema（core/protocol.py:40-90 vs core/models.py:822-915），不能把 schema 当完整真源。
  - 风险：Go 的 omitempty 会省略 null/默认字段，iOS 直接解码失败；改字段名或重签 sha256 会让 Web protocol:check 失效。
- **protocol/1.0 capability-set / capabilities-response** → Go Capability/CapabilitySet/CapabilitiesResponse；metadata/parameters 用 map[string]any 或 json.RawMessage；显式增加 runtimeId 字段；version/parameters/scope 即使不读也必须输出（iOS 视为必填）。
  - 能力门控是真实产品行为（Web session-composer、iOS SessionChatModel/ConversationSettings 都按 supported&&available&&allowed 判断）；但 schema 漏掉 runtimeId 和 metadata，而 Web 依赖 metadata.allowedMimeTypes 和 runtimeId 作用域（components/session/capabilities.ts:35,57），服务端靠 Pydantic extra 才把它们带出去（services/effective_capabilities.py:244-256）。
  - 风险：按现有 schema 生成 Go struct 会静默丢掉 runtimeId/metadata，导致 runtime 实例作用域匹配失效、MIME 限制永远不生效；删 version/parameters 键会让 iOS 解码失败。
- **protocol/1.0 model-catalog / permission-catalog** → Go Catalog/CatalogItem；metadata 为开放 map；selectionId 生成算法照搬 protocol.py 的 sha256/base64url 规则；enabled/disabledReason 继续放在 metadata，不要新增顶层字段。
  - selectionId 是会话选择持久化的唯一键，metadata 里的 enabled/disabledReason/i18n/provider/model/reasoningEffort 是真实 UI 行为；服务端与 connector 各自有 protocol_selection_id 算法，必须逐字一致。catalog.runtime/revision 与 fullModelId 客户端不读。
  - 风险：selectionId 算法漂移会让已保存 selections 全部失效；丢掉 metadata 会让禁用项/i18n/推理强度选择失效；顶层 enabled 从未被服务端发送，别按 schema 硬造。
- **protocol/1.0 event-envelope** → Go Event struct + per-session sequencer；保持 sequence/cursor 同源、eventId 至少全局唯一；payload 用 map[string]any。
  - 真实顺序/去重协议：多个事件可共用同一 sequence，客户端按 >= 接受、按 eventId 去重且对 live projection 豁免；服务端 eventId 目前是 evt_{sequence}_{hash}（core/events.py:43-62）。
  - 风险：把多事件同 seq 改成一事件一 seq、或对 session.meta/runtime.state/capability/catalog 做 eventId 去重，会丢掉 A→B→A 的合法状态变化。
- **protocol/1.0 event-recovery-response** → Go recovery service；等价 session fence + 只读 <= head；nextCursor=当前 head；不补 runtime state/notices（由客户端 live 读）。
  - 真实恢复正确性：在读一致性上取 head、timeline_reset 触发 snapshotRequired、has_more 触发 snapshotRequired，并在同 seq 合成 session.meta.updated 与 runtime.capability.updated（event_recovery.py:82-186）；3 次稳定性重试可以换成 session_fence 内 <= head 的语义，但响应语义不能变。
  - 风险：少发合成的 meta/capability 事件会让恢复后会话名/能力陈旧；把 nextCursor 设成最后一条事件而不是 head 会造成重复或缺口；serverTime 键 iOS 必填。
- **protocol/1.0 ws-ticket** → Go ticket manager（Redis 或内存单机）+ WS 握手时原子 consume；scope 校验 exactly-one；失败 close code 1008。
  - 真实安全边界：ticket 单次消费、短过期、scope 二选一，避免把长期 Bearer 放进 WS URL；两端都按 query ticket 连接。
  - 风险：ticket 可重复使用或 scope 校验放松会造成越权订阅；iOS 要求 expiresAt/serverTime 键存在。
- **runtime-control/2.0** → Go internal/runtimecontrol 包：同 9 个结构、同 RPC 方法名、同 additionalProperties:false 约束；JSON 编解码；validate.py 对应的 Go 测试或继续用 Python validator。
  - 真实在用：connector/connector/server/runtime_rpc.py 实现，server/agent_server/services/device_runtimes.py 调用；9 个 schema + 27 fixtures + validate.py 是仓库里质量最高的契约。语义包括 runtimeId 不可变、runtime 必须等于 runtimeType、legacy 实例 runtimeId==runtime、configRevision 安全整数、单/多实例上限。
  - 风险：丢失 runtimeId==runtime 的 legacy 校验、把 single/multiple 写成 singleton、或用不安全的 revision（时间戳/哈希）会让已配置实例无法启停。
- **local-machine/2.0** → Go internal/localstate：精确复刻文件路径、字段、.lock.json {version,port,pid,token,startedAt}、只回 token 的 TCP lease、5s 超时/20ms 重试、迁移逻辑；Windows 注意 SO_EXCLUSIVEADDRUSE、原子 rename/ACL。
  - 真实并发正确性与跨进程互操作：PID+进程启动时间+可执行文件校验、OS 分配端口的 token lease、O_EXCL 发布、0600+fsync+原子替换、旧 machine.json 迁移；Desktop 侧的 Node 实现已按同一协议写好，Go connector 不能单方面换锁。
  - 风险：换用 Go 自带文件锁/命名互斥会与 Desktop 不兼容，出现双 connector 或记录互相覆盖；PID 复用/启动时间比较写错会误判占用。
- **Web 生成的 TS 类型与 protocol:check** → 保留生成器；Go 成为真源后增加 Go→JSON Schema 导出，或保留 Pydantic 影子导出器；manifest sha256 重签。
  - 是客户端编译契约与 schema 新鲜度闸门；但 11 个生成文件里只有 6 个被 import，handshake-response、event-envelope、capability-set、model-catalog、permission-catalog 都是死文件，间接证明契约有冗余。
  - 风险：Go 侧改了字段但没重跑生成器，TS 类型与实际响应漂移，前端在编译期完全看不出来。
- **Web 事件缓冲/序列/乐观时间线** → 不是 Go 包，而是 Go server 必须满足并在契约测试中固化的语义（多事件同 seq、complete item upsert、clientMessageId 回显、nextSeq 高水位）。
  - 这是事实上的客户端契约：sequence>=游标接受、live projection 不做 eventId 去重、33ms 固定窗口合并同 item、快照保留 updatedSeq>nextSeq、clientMessageId join；逻辑本身正确，客户端本次不改。
  - 风险：服务端若把 item_updated 改成部分内容、跳过 clientMessageId、或打乱 seq，Web 会出现重复气泡/丢消息/整页重取。
- **iOS 严格解码模型** → Go struct 的这些字段不用 omitempty，nullable 对象要么给 null 要么给值但键必须在；默认值（pinned=false、userArchived=false、sourceAvailability=available、latestTurnEndSeq=0）必须显式输出。
  - 合成 Decodable 把很多“没人读”的键变成硬依赖：V2SessionSnapshot 必填 approvals/runtimeCapabilities/catalogs/eventCursor/serverTime；V2SessionMeta 必填 connectorStatus/userArchived/sourceAvailability/unread/lastReadSeq/latestTurnEndSeq/updatedSeq；V2RuntimeState 必填 sessionId/runtime/updatedSeq/createdAt/updatedAt；V2RuntimeCapability 必填 capabilityId/version/scope/supported/available/allowed/parameters；V2WebSocketTicket 必填 expiresAt/serverTime。
  - 风险：任何一个必填键缺失都会让 App 整页解码失败；这是 Go 重写最容易造成“服务端 200 但 App 白屏”的地方。
- **iOS projection/repository 的恢复状态机** → Go server 的 sessionstream/recovery 契约测试要覆盖：seq 连续、多事件同 seq、snapshotRequired、恢复期间不推进游标、live state 端点独立于 /events。
  - 真实并发/恢复正确性：sequence>=游标、非 live 事件按 eventId 去重（2048 环形）、sequence>cursor+1 触发 reconcile、recovery 后重新读 live state 并用 receivedAt 屏障丢弃更旧事件；这些直接约束 Go 服务端的 seq 分配与 /events 行为。
  - 风险：Go 若给每个事件独立自增 seq 但漏号，iOS 每次重连都触发全量 reconcile；若 /events 返回顺序错乱或重复，会导致时间线跳变。
- **HTTP REST / auth / error 表面（未入契约）** → Go router/middleware 逐项复刻路径、query、状态码与错误 JSON；上传/下载/StreamingResponse 与终端 WS relay 都要有对应实现。
  - 客户端依赖：camelCase、204 空响应、Bearer、401/403/404/409/422、{"detail": string|{code,message}}、GET 的 Retry-After、multipart 上传、下载同源重定向、CORS（默认只允许 127.0.0.1/localhost 任意端口）。这些都没写进 contracts，但都是外部契约。
  - 风险：Go 框架默认错误体、422→400、204 带 body、路径复数化（connector→connectors）都会直接打断 Web/iOS 的既有错误处理与调用。
- **Connector/Project/Runtime Inventory REST 形状（未入契约）** → Go struct 逐字段照搬 Pydantic 模型与 alias（populate_by_name + by_alias=True），保留 displayName 兼容别名与 schema 别名。
  - 客户端大量读取：ConnectorView（含 connectorKind/deviceOs）、ProjectView、DeviceRuntimeView（runtimeId/runtimeType/displayName/typeDisplayName/present/available/configured/active/status/discovery/metadata/schema/uiSchema/defaults/capabilities/config/error/phase）、RuntimeTypeView（instancePolicy/maxInstances/configSchema 等）；其中 displayName 是 Web/Android 的兼容别名，schema 用 alias="schema"。
  - 风险：用 Go 习惯把 schema 写成 Schema/schemaJson、把 runtimeType 写成 runtime_type、或删 displayName，会让 Web 设备页和 iOS 运行时配置直接不可用。
- **phone-pane 插件桥文件 + 审批 HTTP（未入契约）** → Go internal/pluginbridge：同样的文件和 127.0.0.1 HTTP；token 轮换与 approval.json 发布；Windows 原子替换与半写 JSON 容忍；按 toolUseId 索引 pending。
  - 这是外部进程契约且不在 contracts/；本次只重写 connector+server，插件若不同步重写，Go connector 必须保留文件路径、队列项 {id,action,text|command,args,queuedAt,token}、outbox 字段、审批请求 {token,toolUseId,tool,input,action} 与响应 {decision:allow|deny|ask|ignored,reason,updatedInput}。审批端点是安全边界：loopback + per-process token，阻塞到手机/终端决定。
  - 风险：任何路径/字段/响应形状变化都会让 phone-pane 静默失效；另外设计文档要求“无超时循环复用 requestId”，当前协议只有 toolUseId、没有 requestId，若按设计实现必须同时改插件，超出本次重写范围。

---

## 不要复用的逻辑

- notice 承载审批状态机与唯一真相：v2_3 删 approvals、v2_7 删 notices（tests/test_database_migrations.py:2012 断言当前 schema 没有这两张表）；api/sessions.py:1000 的 snapshot 永远 approvals=[]，notices 每次向 connector RPC session.notices 现拉（api/sessions.py:1778-1826）。connector 离线时手机看不到待审批，审批会丢。不要把新逻辑建在 notice 上；v1 只为客户端兼容保留 notice 投影，phase 6 加 approvals 表与会话事件。
- 时间线项即一切：turn、approval、session state、catalog 变更都被塞进 timeline/notice 投影，服务端没有不可变事实日志；恢复路径无法重建审批与回合，contentHash 还完全信任 connector。新架构不要把新事实建模成 timeline item；timeline_items 只做客户端投影。
- connector_ingest._publish_effects 的一个 seq 挂多个事件：items、runtimeState、capabilitySet、notices、catalogs 共用同一个 nextSeq，eventId 又是 evt_{seq}_{payload hash}；同一事实重放若分配了新 seq 就会得到新 id。v1 保持 wire 形状，但服务端要加 producerId+producerSeq 的入库幂等，不能靠 eventId 做重放去重。
- 把 Redis 当唯一真源：终端 registry 只在 Redis（terminal_broker.py:21 的 24h TTL），pending timeline 只在 Redis，presence 租约、ticket、pub/sub 也在 Redis。单实例重启或 AOF 丢失会静默丢内容。v1 保留 Redis 作为兼容实现，但 Go 代码必须能跑 local backend，且必须有启动 timeline_reset 与 terminal.list 重建方案，否则永远摘不掉。
- 多运行时残留：RuntimeName 五值枚举（codex/claude/opencode/acp/dsh）在 server/core/models.py、connector/server/protocol.py、contracts 四处各写一遍；KNOWN_RUNTIME_TYPES 含两个不存在的 runtime；sessions.runtime 与 runtime_id 双写；connector_runtime_types 的 discovery/config/ui/metadata 四坨 JSON 只是转发。不要移植这些分支；wire enum 保留兼容，但服务端把非 claude 显式判 unsupported。
- instance_binding + supervisor + instance_models + supervisor_models（约 1,860 行）：单 Claude、instance_policy=single 下全是恒等映射；资源 claim、multi 实例容量、config revision 冲突没有真实问题要解决。全部丢弃，只在 API 边界做 runtimeId 透传。
- repository_ports（659 行 20+ Protocol）、facade 的 12 个 mixin、RuntimeProvider/RuntimeHostClient ABC、FileStorage ABC、LocalOps 90 行转发、runtime_rpc 的 1,780 行参数装饰：都是单实现或纯转发。Go 不要复刻形状，按用例定义 2-3 方法窄接口；但 fence/lifecycle 的上下文管理器语义必须显式保留。
- EventPreparationPool、_HotpathProbe、AGENT_SERVER_EVENT_WORKERS/EVENT_THRESHOLD_BYTES/LOCK_SLOW_MS 这类 Python 调参：进程池是绕 GIL 的历史方案，2C2G 上多养两个 Python 进程更贵。Go 只保留「每条广播只编码一次」和「超限 1013 断开」的行为。
- server shell 子系统（api/connector_shell.py + services/connector_shell.py + services/shell_tasks.py + connector local/shell.py，约 786 行）：三端客户端零调用，只被彼此调用，形成完整闭环死链；terminal.create 的 command 参数已覆盖一次性命令。两端一起删。
- v1 终端 API（/sessions/{id}/terminals* 6 条 + /connectors/{cid}/terminals 旧 6 条 + TerminalService 466 行）：Web/Android 只打 terminals-v2，iOS 无 terminals；web-next 里 connectorTerminalList/Create/Rename/Close/Resize 五个包装函数无调用点。删 v1，但 terminal_broker、terminal_stream_hub、terminal_relay 必须留，v2 stream 依赖它们。
- 90 秒没新 turn 就判 idle、3600 秒兜底、turn_claude_* 自铸 ID、previous_turn_id 猜归属：这些是经验补偿，不是协议。Go 改成基于心跳/事件进展的 stall watchdog，只把回合置 error/interrupted 并释放，绝不猜完成。
- bridge 机器级单 queue、单 outbox.json、单 tools.json、单 auth.json、单 token：两个终端同开时 queue 消息会被各自 submit、outbox 的 turnId 互相覆盖、auth 最后写者胜出；queue 从不删除、_next_seq 每次全目录扫描；审批遍历 session_store 取第一个 plugin_bridge 会话。不要照搬；按 externalSessionId 分实例、持久化计数器、ack、归档。
- HTTP ingest 捕获 ConnectorIngestRejectedError 后把整个 _inflight 清空：一个永久 rejected 事件会丢掉同批所有已接受的持久事实。改成逐事件 accepted/rejected，可重试的回队头，永久错误进死信。
- migrations.py 的 classify_database（约 250 行形状探测）：只识到 v2_33，head 是 v2_44；这是无 alembic_version 老库的一次性历史路径。Go 只保留显式版本断言，不要复刻 44 版中间态探测。
- connector rpc 的 _resume_runtime_instance 用 config={} 自动 runtime.start：绕过用户配置和 runtime.stop，是为了掩盖实例只在进程内存的问题。Go 必须从持久化配置懒启动。
- server 侧 services/workspace.py 自己拼 Windows/POSIX 路径：connector 才是唯一知道 root 和真实文件的机器，两端各一套规则必然漂移。v1 为了 server-first 切换暂时保留，phase 8 改成 {root,relPath} 由 connector 解析；server 只保留「不得逃逸 root」的校验。
- SessionView 双定义并依赖 Pydantic extra=allow：core/protocol.py:46 与 core/models.py:822 字段漂移，snapshot 靠 extra=allow 把 runtimeId 漏出去。不要按 contracts schema 生成 Go struct，也不要删 iOS 当必填的键；声明一份完整超集。
- S3/FileStorage 双后端：部署写死 local，S3 依赖私有 GitHub 包且从未启用。只留本地磁盘，接口收敛成 Write/Read/Delete；local.py 的路径穿越校验与 sidecar 原子写必须保留。
- 动态 OAuth client CRUD（infra/repositories/oauth.py 的 list/create/delete + OAuthClient* 模型）：全仓零调用，api/oauth.py:99 的 first_party 检查让它们永远不可达。只保留两个第一方客户端常量与 get by id。
- legacy 表/列的持续读写：legacy_import_archive 只被 v2_2 写入、无人读；retired_device_runtimes 只有一处写入且 v2_44 已改成直接删占位行；sessions.dsh_archive_legacy 只写 0、只出现在 SELECT；userArchived/archiveSource 的 runtime/both 分支从未产生。停止读写，物理删除延后；wire 键保留但由 archived 派生。
- 把 7 个 WebSocket 端点统一成一种鉴权、或把协议换 protobuf：四个 WS 鉴权方式不同（ticket、Bearer、relay token、?token= JWT），统一会打断客户端；protobuf 无法保留开放 JSON 扩展字段，且要同时改三端。v1 保持 7 个端点与 JSON。
- 把 web-next/src/features/dashboard/types.ts 当协议真源：它既过严又过松（Notice.updatedAt 服务端不发、approvals 未声明、runtimeCapabilities 声明但不读，同时含 schema 没有的 runtimeId/metadata/effectiveRunMode）。Go 以「实际读取字段 + iOS 必填字段」的并集为准。
- Python 的无界 asyncio.Queue、无 WS 帧大小限制、慢客户端不设上限：这是 2C2G 上的真实风险，不是可复用语义。Go 必须每会话/每连接有界队列 + 字节级准入 + 超限断开，持久事实进死信而不是丢。
- 每次 dashboard.changed 重建全量快照（dashboard_stream.py:119-157）：帧形状要留，实现不要留；改成内存物化视图 + 250ms 合并 + 单次编码。
- 多 worker/Redis 协调作为默认架构：workers>1 才强制 Redis、single_instance 互斥是 Python 多进程产物。Go 单进程默认；保留 Coordinator 接口，但不要把多 worker 的假设带进 actor 设计。

## 必须保留的语义

- session_fence 的 flush-then-read 语义：进入时先 flush 该会话 pending，再允许读 <= head；退出时把分配器 floor 到持久 seq，若有未发布修订补一条 refetch 边界；必须可重入（Go 用 context 里的 fence token，嵌套直接复用）。全仓 25 处调用，是 snapshot cursor 自洽的唯一入口。
- 修订号租约与 published 水位：PG 的 seq_allocated_high 按 4096 区间租用，分配前检查 head>published 触发 repair；seal/floor 只能单调上调；所有 seq/revision 校验 <= 2^53-1；Redis 崩溃后不得复用已观测修订号。
- 1 秒批量物化：accept 预分配 seq 并立即发布 live，sweeper 每秒一个事务 upsert 变更项、把 sessions.seq 推到 max、按条件推进 last_read_seq；禁止每条 delta 写 PG。
- timeline item 的合并规则：同一 id 同 contentHash 视为 unchanged，不分配 seq、不推送；revision=max(入参,旧+1)；已有 item 的 orderSeq 永不重排；同批重复 id 取最后一条；pending 清理必须 CAS，只删自己快照里仍未被覆盖的值。
- 每会话 FIFO lane：同一 session 严格有序，不同 session 并行，connector 级通知单独一条 lane；timeline.itemUpsert 按 itemId 合并，旧版本在执行前丢弃；lane 队列必须有界，满时拒绝或断链让 connector outbox 兜底。
- 同一 connector 只允许一条 WS：重复连接 close 4409；connection_id 代际校验贯穿 runtime 状态写入与终端归属；断连先 begin_drain 停止新 RPC、保留租约、10 秒内排空已收通知，再 unregister。
- 启动恢复：无 Redis 或 Redis 丢失时，对 seq_allocated_high > seq 的会话执行 timeline_reset_seq = seq_allocated_high，并把 seq 抬到 seq_allocated_high、updated_seq 取最大；只写 reset 不抬 seq 会制造 11..4096 的缺口，iOS 的 sequence > cursor+1 会无限 reconcile。
- seq 语义：一个 session 只有一个单调不减的整数序列空间；event.sequence、cursor seq:N、timeline.nextSeq、TimelineItem.updatedSeq 同源；多个事件可以共用同一 seq（客户端接受 >=），但整数 seq 本身不能跳号。
- snapshot 的 nextSeq 必须是 session 高水位：不能小于任何已下发 item.updatedSeq；客户端会保留 updatedSeq > snapshot.nextSeq 的本地项。eventCursor 必须与 items 自洽。
- timeline_reset 围栏：complete=true 的 timeline.sync 是整段替换，推进 timeline_reset_seq；之后到达的 buffered item 若 seq 小于该栅栏必须丢弃；快照内容未变则完全不写。
- 终端的 seq/replay/scrollback 语义：每终端 512KB 环形缓冲、chunk seq、baseSeq 回退全量 replay、慢浏览器有界队列 overflow 直接断开、PTY 输出绝不 await 网络；relay attach 两阶段查-锁-查幂等，写请求不重试，断开不关 PTY。
- dashboard 全量快照的键与语义：type、connectors、projects、sessions、runtimes、sessionPages、serverTime 必须全在；客户端按 per-session updatedSeq 防旧快照覆盖，删除用「列表里没有」表达；15 秒 keepalive。改成增量需要同时改三端，v1 不做。
- /api/v2 路径、camelCase、nullable 显式 null、必填键不得 omitempty、错误体 {detail: string 或 {code,message}}、401/403/404/409/422、GET 的 Retry-After、multipart、204 空响应、同源下载重定向、CORS 默认只允许 127.0.0.1/localhost 任意端口。
- iOS 合成 Decodable 的必填键：snapshot 的 approvals、runtimeCapabilities、catalogs、eventCursor、serverTime；SessionMeta 的 connectorStatus、userArchived、sourceAvailability、unread、lastReadSeq、latestTurnEndSeq、updatedSeq；Capability 的 capabilityId、version、parameters、scope、supported、available、allowed；ws-ticket 的 expiresAt、serverTime；dashboard 的 sessionPages。缺一个就整页解码失败。
- capability 必须是 effective 投影：带 runtimeId、metadata、4 级查找优先级（session+runtimeId → session → runtime+runtimeId → runtime）、unavailableReason 顺序（unsupported → offline → not taken over）、revision=max(session.updatedSeq, runtime revision)；原始 runtime capability set 不能直接下发。
- catalog 的 selectionId 与 metadata：selectionId 是会话选择持久化唯一键，算法必须逐字节一致；metadata 的 enabled/disabledReason/i18n/provider/model/reasoningEffort 是真实 UI 行为；revision CAS 的 accepted/stale/idempotent/conflict 三态不能合并。
- 时间格式：所有响应固定长度 UTC+Z，微秒精度一致；不能用 Go time.Time 默认 MarshalJSON（会输出 +00:00 或不同精度）。auth.py:625 用字符串字典序比较 expires_at，格式一变过期判断会静默出错。
- 认证字节格式：connector access token 的 id.exp.sig HMAC、user HS256 JWT 的 header/payload、通用 signed token 承载 mobile-login/refresh、PBKDF2-SHA256 120k、pbkdf2_sha256$salt$b64、passwordVerifier+salt 双通道、mobile refresh 30 天 audience、AGENT_SERVER_SECRET 默认值。改任何一项都是全端登录失败。
- WS ticket：60 秒、一次性、scope 只能 session 或 dashboard 二选一、SHA-256 存哈希、消费必须原子；失败 close 1008。不能把长期 Bearer 放进 WS URL。
- event envelope：protocolVersion、eventId、sequence、cursor、type、sessionId、emittedAt、payload 全部保留；cursor 只接受 seq:N 且不超协议上限；session.meta/runtime.state/capability/catalog 四类 live 投影不做 eventId 去重（A→B→A 合法）；timeline item 事件必须是完整 item upsert，不能改成 token delta。
- connector 线协议：WS 帧的 request/response/notification 形状、900KB 帧上限、close code 语义（4409 重复、1012 恢复、1013 过载、1008 鉴权、4001/4401 凭证失效）、HTTP ingest 的 /api/v2/connector/ingest 路径与 accepted/rejected 形状、Connector id:token 与 Bearer 两种头、loopback 不读代理、X-Device-OS。server 先切时 Python connector 必须能原样工作。
- phone-pane 插件 v1 文件契约：~/.claude/phone-bridge 下 queue/<n>.json 的 {id,action,text|command,args,queuedAt,token}、outbox.json 的 turnId/status/text/chunks/ts/model/effort/cwd/git/usage、tools.json、abort.txt 毫秒时间戳、heartbeat.txt 毫秒 epoch、approval.json {port,token}、审批响应 {decision,reason,updatedInput}。路径或字段一变插件静默失效。
- Desktop 的 contracts/local-machine/2.0：~/.agents-anywhere/connector-runtime.json 的 PID+进程启动时间占用判断、OS 分配端口 + token 的短期文件事务、O_EXCL 发布、0600+fsync+原子替换、旧 machine.json 迁移；不能用 Go 文件锁替代，否则与 Electron 并发写会互相覆盖。
- 持久化 ID/hash 公式逐字节一致：sessionId = sess_{runtime}_ + sha256(connector:runtime:external)[:24]；selectionId = sel_{catalog}_ + base64url_nopad(sha256(1:runtime:catalog:canonical_json))[:24]；itemId 的 claude_msg_/claude_tool_/claude_system_ + sha256(canonical parts)[:24]；contentHash = sha256: + sha256(canonical {type,status,role,content})；approvalId 由 session/external/turn/tool/input 哈希。canonical JSON 必须对齐 Python 的 ensure_ascii=False、sort_keys、separators、浮点格式、孤立代理转义。
- SessionView 的完整超集：id、connectorId、projectId、connectorStatus、runtime、runtimeId、runtimeType、runtimeName、runtimeTypeDisplayName、externalSessionId、title、cwd、status、takeover、pinned/pinnedAt、archived/archivedAt、userArchived、sourceAvailability(+reason/updatedAt)、sourceObservationOrigin、archiveSource、unread、lastReadSeq、latestTurnEndSeq、lastSyncedAt、sourceObservedAt、lastActivityAt、lastItemAt、lastItemOrderSeq、sortAt、updatedSeq。所有 list/meta/snapshot 输出同一份。
- 项目是会话强制前置：每个 session 必须有 project；(user,connector,workspace_key) 与 (user,name) 两个唯一约束；sessions.project_id 的 RESTRICT 外键不能被改成 CASCADE；workspace_key 归一化规则（大小写、分隔符、平台）逐字照搬。
- connector 撤销与删除：revoke 在 connector_lifecycle 锁内轮换 token、踢掉活动 WS；delete 级联清理；所有查询都检查 revoked=0；token 哈希与访问令牌格式不能变。
- 附件一致性：上传写围栏对 sessions 行 FOR SHARE，防止删除设备留下孤儿 blob；file_id 必须 file_ 前缀且不含 /、\、..；读取后复核 sha256/size；preview token 单次消费；connector 读取先校验 session 归属。
- timeline_items 双写：payload_json 是读取权威，类型化列只服务索引/排序；写入必须同时维护两者；upsert 必须带 excluded.updated_seq >= 旧值 的单调守卫。只维护列会读回旧内容，只维护 JSON 会丢索引。
- 用量与分享的数据语义：usage_events 的 append-only + dedupe_key 唯一约束是幂等边界；session_shares 的 snapshot_json 不可变，allowedFileIds 白名单，公开附件必须校验 fileId 属于该分享。
- 安全默认值：setup token 只打印日志一次、15 分钟 TTL、成功后一次性失效；邮箱验证码对未知邮箱返回同样结果；email apiKey 只进不出；OAuth public config 不含 clientSecret；OAuth code 5 分钟单次、redirect_uri 精确匹配、S256-only。
- 审批与通知的 v1 兼容行为：snapshot 的 approvals 键必须存在且为 []；notices 仍通过 session.notices RPC 现取；runtime.notice.updated 与 runtime.notice.snapshot 的形状不变。phase 6 加一等审批时，必须继续投影成同样的 notice，而不是让客户端改协议。

---

## 与已定决策的冲突（需要你确认）

### 1. 存储：SQLite vs PostgreSQL

你已决定用 SQLite。方案的结论是 server 保留 PostgreSQL，理由是：

- 现有数据就在 PG 里（v2_44 基线），换存储要额外写迁移与校验；
- 同时换语言 + 换存储是两个独立风险叠加；
- 上一轮我说 SQLite 合适，指的是**单机单租户的部署形态**，这点没变；
  但把「语言」和「存储」一起换，出问题时无法判断是哪一处。

**折中建议**：connector 的本地状态用 SQLite（本来就该）；server 第一版仍用 PG，
存储层做成窄接口，等 Go 稳定后单独做一次「PG → SQLite」的迁移。
如果你的判断是「就是要一步到位」，也能做，但工期要加，且第一版没有回滚参照。

### 2. Redis：删 vs 留一版

你要求删。方案建议 v1 保留，理由是：终端注册表（24h TTL）和 pending timeline
目前只存在 Redis 里，直接删会丢终端与未落盘的时间线。

**折中建议**：v1 保留 Redis 但把它收敛到一个 `Coordinator` 接口后面，
phase 8 再换成进程内实现并移除容器。这样「删 Redis」这件事仍然会发生，只是晚一步。

### 3. connector 不能 100% Go（第一版）

- Electron Desktop 通过 MeshCall LocalRouter 控制 connector，纯 Go 无法与未改的 Desktop 互操作；
- phone-pane 插件只认 v1 文件桥，插件升级前不能换协议。

**折中建议**：v1 保留一个只做 Control/Logs 的 Python 边车，其余全 Go。

---

## 落地顺序

- Phase 0（1-2 周）：冻结契约与基线。冻结 protocol/1.0、connector RPC v1、phone-pane 文件桥 v1、local-machine/2.0；录制脱敏后的真实 HTTP/WS/connector 流量作为回放集；把 Alembic 冻结在 v2_44 并打 tag；从线上库 pg_dump 生成 Go 基线结构断言；建立 Go/Python 双端契约测试与路由快照测试。退出标准：同一批 fixtures 在 Python 与契约校验器上全绿；任何协议改动只能新增字段。
- Phase 1（3 周）：Go 骨架 + 只读面。config、pgx pool、protocol/v1、错误中间件、CORS/SPA、auth 读路径；用只读 PG 角色起一个影子 Go 实例，代理不导流，回放 GET/WS snapshot 与 Python 对比。退出标准：所有 GET 端点归一化 JSON diff 为零；WS snapshot/dashboard 帧逐键一致；路由表无重复 pattern。
- Phase 2（2-3 周）：认证与实时鉴权。login/register/email-code/oauth/pairing/setup token、users/admin、ws-ticket、session WS 与 dashboard WS。退出标准：黄金 token 向量（connector HMAC、user JWT、signed token、PBKDF2）逐字节一致；iOS/Web 登录与 WS 连接在 staging 通过；ticket 单次消费与 scope 校验通过。
- Phase 3（3-4 周）：timeline + ingest + connector ingress（最高风险）。per-session actor、revision 租约、session_fence、1 秒物化、repair、启动恢复；connector WS/HTTP ingest/RPC、presence、runtime generation、drain、runtimes/catalogs/capabilities、commands。退出标准：kill -9、Redis flush、connector 重连、重复 WS、慢消费者、大工具卡六类故障注入通过；seq 连续且 snapshot.nextSeq 自洽；同一 session 顺序与 item 合并正确。
- Phase 4（3 周）：其余写路径。send/steer/interrupt/respond、attachments/fs、terminal v2/relay、projects、shares、archive、usage、admin。退出标准：真实 Windows connector 端到端跑通发送/流式/审批/附件/终端/文件；Web/iOS/Android 已发布版本冒烟全绿；死路由确认零调用。
- Phase 5（1 周 + 72 小时浸泡）：server 切换。停 Python writer，翻反向代理到 Go，保留 Python 可随时启动；观察错误率、p95、WS 重连、seq 缺口、409/502/504、timeline 行数与 payload hash。退出标准：72 小时无 P1；回滚演练在 10 分钟内完成；客户端无改动。
- Phase 6（2-3 周）：服务端清理与 additive 目标架构。删除 18 条死路由、13 个死模型、shell/v1 terminal/agents 的 Go 代码；新增 session_events、approvals、dead_letters（只增表，不改旧表语义）；把审批投影成现有 notice；dashboard 物化视图；然后 phase 8 再摘 Redis。退出标准：新表对 Python 完全透明；审批离线可见且客户端无改动；回滚仍可用。
- Phase 7（与 server 并行开发，server 切换后执行，6-8 周）：connector Go 重写与切换。先做 protocol/ids/canonical JSON/SQLite/localstate，再做 session actor + SDK/terminal 两条投递闭环，再做 bridge v1 与 backend；在单台 Windows 上停 Python、跑 migrate、起 Go、验证插件/终端/SDK/文件。退出标准：24 小时浸泡；三条投递路径都能起回合/中断/收尾；插件 v1 与 Desktop 控制面可用；回滚到 Python 用同一份 JSON 数据。
- Phase 8（1-2 周）：摘 Redis 与去边车。先在生产观察 local backend 的启动 timeline_reset 与 terminal.list 重建；再切 Coordinator=local；最后移除 Redis 容器。若 Desktop 仍未改，保留 Control/Logs Python 边车并明确标注为临时件；若 SDK 仍是边车，同样标注并排期。退出标准：无 Redis 跑 2 周无终端丢失/无 seq 静默跳过；Python 完全下线。
- Phase 9（持续）：契约与运维收尾。Go 成为 schema 真源，重签 manifest，web-next protocol:check 通过；把 dashboard 帧、REST 错误面、phone-pane 文件桥补进 contracts；建立发布前「三端 fixtures 回放 + Windows E2E」门禁。退出标准：任何协议改动都必须同时更新 Go struct、contracts、fixtures 与生成类型。

---

## 兼容与回滚

一、协议冻结，而不是「边写边改」。server 与 connector 都实现同一份 v1 契约：HTTP 路径/query/状态码/错误体、WS envelope 与 close code、connector RPC 方法名/参数/响应、phone-pane 文件桥、local-machine/2.0。允许新增可选字段与新增事件 kind，不允许删键、改键名、把 422 变 400、把 null 变省略、把多事件同 seq 改成一事件一 seq。Python 实现是 oracle：Go 的每个响应都要与 Python 归一化 JSON 逐键对比。

二、部署拓扑与切换。反向代理（Caddy/nginx）在前，Python 与 Go 各自绑定不同端口；同一时刻只有一个 writer。server 切换用「停 Python → 起 Go → 翻 upstream」的短窗口（分钟级），不做按路由分流，因为 timeline/ingest 必须单写。只读影子可以按路由跑：Go 用只读 PG 角色接同一库，回放 GET/WS snapshot，不接写流量。connector 切换用「停 Python connector → 跑 migrate → 起 Go connector」的分钟级窗口；单实例 owner 锁不允许双跑，所以不要承诺零停机。

三、切换顺序。开发可以并行，切换必须串行：先 server 后 connector。理由：server 是中心，能影子对比；Python connector 可以作为稳定客户端验证 Go server 的 ingress/RPC；等 server 稳定后再换 connector，故障面被隔离。禁止同一周切两端，否则出问题无法判断是哪一端。

四、数据迁移。server 不需要迁数据：同一个 PostgreSQL，Go 只做 v2_44 基线 + 结构断言；切换版不跑任何破坏性迁移，不重跑 v2_40/v2_44 的数据迁移。Go 的 goose 使用与 Python 相同的 advisory lock ID（0x414147454E545332），启动门禁双读 alembic_version==v2_44 且 goose head 匹配。connector 用一次性 `migrate --from <python-data-dir>` 把 sync_state/runtime_storage/pending_messages/outbox 导入 SQLite；源 JSON 不删，成功后写 migrated_from_json 标记并校验行数。connector.json 与 connector-runtime.json 仍是文件，格式不变。

五、回滚。server：停 Go、起 Python、翻 upstream；切换版只写现有表和现有列，新增表/列全部 nullable 或 additive，Python 忽略即可；切换前做 pg_dump 全量备份。Redis 使用与 Python 相同的 key 名与 TTL，回滚时 terminal registry、ticket、pending 尽量可读；切换前对所有会话做一次 fence/flush，确保 pending 为空。connector：Go 只写 SQLite，不改 JSON 源；切换前快照 ~/.claude/phone-bridge 与 Python 数据目录；回滚时恢复快照并重启 Python。Go 消费过的 queue 文件必须归档而不是删除，避免回滚丢消息。

六、客户端与外部消费者。iOS/Web/Android 零改动是硬门禁：发布前用已发布版本的 App/网页跑登录、会话列表、发送、流式、审批、附件、终端、分享、管理页。Desktop 必须单独验证：connector-runtime.json 的端口 token 租约与 MeshCall Control/Logs；如果 v1 不实现 mesh 协议，就保留 Python 控制面边车，并在发布说明里写清楚。phone-pane 插件只验证 v1 文件桥；HTTP 主通道不在本次范围。

七、灰度观测与回滚判据。必须埋点：HTTP 5xx/4xx、p95、WS 重连率、connector 离线次数、seq 缺口数、timeline item 行数与 payload hash、snapshot 与 /events 的 nextCursor 一致性、审批 notice 数量、终端 attach 失败率。回滚判据提前写死：任一 P1（登录失败、消息丢失、审批丢失、seq 静默跳号、connector 无法重连）立即回滚；错误率 >2% 持续 10 分钟回滚；WS 重连率 >5% 回滚。

八、明确不做的事。不在切换版拆 Redis；不在切换版引入 approval.requested 一等事件；不在切换版把 dashboard 改增量；不在切换版统一 7 个 WS 鉴权；不在切换版把 connector RPC 改成新形状；不在切换版删物理表/列。所有这些都是 phase 6/8 的 additive 变更，必须与客户端或 Desktop/插件升级同批。

---

## 工作量

基准 300-320 人日，含 25% 缓冲 375-400 人日。

| 工作项 | 人日 |
|---|---|
| server 骨架、config、protocol/v1、路由快照、契约测试 | 16 |
| PG store、goose、v2_44 基线、advisory lock | 12 |
| auth/OAuth/pairing/setup/email/admin/users | 16 |
| sessions REST + snapshot + events recovery | 14 |
| timeline actor/revision/fence/flush/启动恢复 | 16 |
| connector ingress/WS/RPC/presence/ingest | 18 |
| runtimes/catalogs/capabilities/commands | 10 |
| files/fs/attachments/shares | 8 |
| terminal v2/relay/stream/hub | 10 |
| dashboard/fanout/keepalive | 6 |
| usage/report/maintenance/announcements/service | 6 |
| 契约 fixtures、流量回放、三端冒烟 | 10 |
| 故障注入、浸泡、负载、Windows E2E | 12 |
| 切流、回滚演练、可观测、文档 | 8 |
| server 小计 | 约 100（90-115） |
| connector 骨架、config、localstate、SQLite、迁移 | 12 |
| protocol/ids/canonical JSON/contentHash golden | 12 |
| bridge v1 文件协议、会话隔离、ack、审批门 | 14 |
| session actor、状态机、turn 生命周期、finalize、watchdog | 18 |
| delivery plugin | 8 |
| delivery terminal + console-injector + spawn/trust | 12 |
| delivery SDK（原生 CLI 控制协议） | 22 |
| history/transcript/watch/sessions/syncer/cursor | 18 |
| timeline projector | 8 |
| backend WS/RPC/ingest/coalesce/scanner/方法表 | 22 |
| local terminal PTY + relay | 12 |
| local files（shell 删除） | 6 |
| control 面（Desktop sidecar 或 mesh 兼容） | 10 |
| connector 测试、浸泡、Windows E2E、文档 | 20 |
| connector 小计 | 约 194（上限；若 SDK/control 用边车可降到 165-175） |
| 联合契约、双跑、切换、回滚、三端/桌面验证 | 30-40 |
| 基准合计 | 约 310-330 |
| 含 25% 缓冲 | 约 390-410 |

日历：3 名熟练 Go 工程师约 6.5-7.5 个月到 Python 完全下线（含 4-6 周浸泡与回滚缓冲）；2 人约 10-12 个月。若只改 connector、server 留 Python：约 180-200 人日，3 人 4-5 个月。两端一起改多约 110-170 人日、多 2-3 个月，且多两次切换与一个双栈维护期。

最容易超支的三块：SDK 原生控制协议（+10-15 天）、Windows 注入/ConPTY（+5-10 天）、Desktop mesh 兼容（+5-15 天或改 Desktop）。排期时不要按乐观值算。

---

## 主要风险

- 【高】Go 没有官方 Claude Agent SDK。SDK 投递路径要么自己实现 claude CLI 的 stream-json 与 control_request/control_response（can_use_tool 是控制协议上的阻塞回调），要么保留 Python 边车。做错会导致无终端/无插件时完全不能起回合。缓解：先做协议探测与最小闭环，接口化 delivery，边车作为受控回退；不要把它放到最后。
- 【高】协议字节兼容。Go 的 omitempty/零值会省略 iOS 合成 Decodable 的必填键；时间格式、错误体、422、null 语义任一变化都会造成 App 白屏或 Web 静默 undefined。缓解：protocol 包禁止必填键 omitempty，nullable 用指针显式 null；三端 fixtures 回放；上线前用已发布 App 冒烟。
- 【高】timeline actor + 启动恢复。丢掉 contentHash 短路、orderSeq 稳定、pending CAS、fence 可重入、或只写 timeline_reset 不抬 seq，会导致重复项、时间线重排、客户端游标越过丢失项。缓解：把 tests/test_timeline_write_buffer.py 逐条移植成 Go 表驱动测试，再加 kill -9/Redis flush 故障注入。
- 【高】两端同时切换、没有稳定参照。Python server + Python connector 原本可以互相当 oracle；两端都 Go 后，契约理解偏差会同时出现在两端。缓解：严格 server-first、connector 后切；Python 保留一个版本周期；录制流量回放；禁止同一周切两端。
- 【高】Desktop MeshCall 互操作。Go connector 不自带 mesh 线协议时，未改的 Electron Desktop 完全看不到 connector（不能启动/配对/看日志）。缓解：v1 保留只做 Control/Logs 的 Python 边车，或同批改 Desktop；不要假设 mesh 可以轻松用 HTTP 替代。
- 【高】phone-pane 插件 v1 文件契约。路径、queue 字段、outbox 字段、approval.json/审批响应任一变化都会让插件静默失效；当前 bridge 还是机器级单队列，多终端会串会话。缓解：v1 逐字保留字段；按 externalSessionId 分实例；queue 消费后归档；多活实例时禁用 v1 bridge 并回落 terminal/SDK。
- 【高】Windows 控制台注入。FreeConsole 若留在主进程会让 connector 永久丢失自己的 stderr；Windows Terminal/ConPTY 下 AttachConsole 可能失败。缓解：注入放进短命 helper 子进程；失败时明确回落 SDK，且若该会话已有活终端则报「终端占用」，不要静默双跑。
- 【高】持久化 ID 与 contentHash 字节兼容。canonical JSON 的浮点格式、非 ASCII、孤立 UTF-16 代理、字段顺序任一差异都会让升级后所有本机会话变成新会话、已选模型失效、时间线全量重发。缓解：internal/protocol/canonical 单独受测；用真实 transcript 与插件输出做 golden；Go 用 SetEscapeHTML(false)+UseNumber+自定义代理转义。
- 【中高】删 Redis 的时机。v1 保留 Redis 降低风险，但 phase 8 摘除时仍有终端 registry 与 pending timeline 两个洞；没有启动 timeline_reset 和 terminal.list 重建就会静默丢终端/丢流式项。缓解：先在生产用 local backend 观察 2 周，再摘；重建失败返回可重试错误而不是空列表。
- 【中高】2C2G 内存与背压。dashboard 全量快照、慢 WS 客户端、大工具卡、多终端、附件流都可能把内存打满；Python 现状还有无界队列。缓解：GOMEMLIMIT、每订阅 4MB/256 帧、全局 pending 64MB、文件流式、超限 1013/503；dashboard 物化视图 + 250ms 合并。
- 【中高】迁移与双 DDL。两份 DDL（migrations/schema_v2_0.py 与 infra/db/schema.py）不一致；Go runner 与 Alembic 双跑会同时 DDL；重跑 v2_40/v2_44 会二次改数据。缓解：冻结 Alembic 在 v2_44、基线从线上 pg_dump 生成、同 advisory lock、双版本门禁、切换版不做破坏性迁移。
- 【中高】认证字节格式。connector HMAC、user HS256、signed token、PBKDF2 120k、passwordVerifier+salt 任一变化都是全端登录失败或所有 connector 401。缓解：对 core/auth.py 做黄金向量测试；保留 AGENT_SERVER_SECRET 与 AGENT_SERVER_PASSWORD_ITERATIONS；不要换 bcrypt/argon2。
- 【中】审批与 notice 的兼容。v1 继续用 notice 投影，phase 6 加 approvals 表与新事件；如果投影 revision 不单调或 approvalId 不幂等，会出现重复卡片或状态回退。缓解：approvalId 由稳定哈希、resolutionId 唯一、notice revision 单调；离线场景用真实设备回归。
- 【中】终端 v2 与 relay。删掉 v1 TerminalService 时容易误删 terminal_broker/terminal_stream_hub；丢掉 seq/replay/overflow 语义会造成重复输出或内存随慢客户端增长。缓解：只删 v1 路由与 TerminalService，保留 broker/stream hub/relay；seq 与 replay 做契约测试。
- 【中】测试与 CI 覆盖。iOS 严格解码和 Windows 注入在 Linux CI 上测不出来；contracts fixtures 只有 5 个，覆盖不足。缓解：macOS runner 跑 Swift fixture 解码；Windows runner 跑注入/ConPTY；补 snapshot/ws-ticket/recovery/capability extension fixtures；发布门禁必须包含三端回放。
- 【中】工期与团队。35k + 25k 行、两次切换、双栈维护，1-2 人做不完；最容易低估的是 SDK、Windows、Desktop 三块。缓解：按 3 人排、把 SDK/Desktop 单独立项、预留 25% 缓冲；如果只有 2 人，先只改 connector。

---

## 全量成本（诚实版）

诚实说：两端一起改，比「server 先留 Python、只改 connector」多出来的不是 30%，而是大约 60-90% 的总工作量，外加一个「没有稳定参照系」的风险。

基准账（人日）：
- server Go 重写：100（合理区间 90-115）。包含骨架/config/protocol、PG store 与 v2_44 基线、auth/OAuth/pairing、sessions REST、timeline actor、connector ingress、runtimes、files、terminal v2、dashboard、usage、契约与故障注入。
- connector Go 重写：170-180（design 2 估 178，合理区间 150-200）。其中 bridge v2 HTTP 的 25 人日在「插件不改」的前提下不能算，但 v1 文件桥的会话隔离/ack/归档仍要 10-12 人日；SDK 原生 CLI 控制协议 20 人日，若用 Python 边车降到 6-8 人日但会留下长期维护面。
- 联调/契约/切换：30-40。包含录制流量回放、golden fixtures、Windows 端到端、双跑与回滚演练、iOS/Web/Android/Desktop 冒烟。
- 基准合计：300-320 人日；加 25% 缓冲：375-400 人日。

日历：
- 3 名熟练 Go 工程师：约 6.5-7.5 个月到 Python 完全下线。关键路径是 protocol/ID 兼容 → timeline actor/ingress → connector bridge/SDK；不是 CRUD。
- 2 人：约 10-12 个月。1 人：不建议，超过 18 个月且单点风险不可接受。

与「只改 connector」的比较：
- 只改 connector：约 180-200 人日（含联调），3 人约 4-5 个月；server 继续 Python，客户端不动，风险集中在 connector 的 SDK/插件/Windows。
- 两端一起改：多约 110-170 人日、多 2-3 个月；并且多出三类成本：
  1. 双语言双栈期：两套 CI、两套依赖、两套部署脚本，团队上下文切换，速度损失约 20-30%，持续 4-6 个月。
  2. 双端同时失去稳定参照：Python server + Python connector 原本可以互为 oracle；两端都换后，只能靠冻结的 v1 契约与录制流量，任何契约理解偏差会同时出现在两端。必须用「Python 作为 oracle、Go 单向替换」的纪律来抵消。
  3. 切换面翻倍：server 切一次、connector 切一次，两次都要回滚演练；connector 切换还有分钟级停机（单实例 owner 锁不允许双跑）。

隐藏成本（最容易漏算）：
- Desktop 的 MeshCall：不实现 mesh 线协议就必须保留 Python 控制面边车；这会让「connector 100% Go」变成「connector 主体 Go + 控制面 Python」。若同批改 Desktop，再加 10-15 人日和一次桌面发版。
- phone-pane 插件：若想上 HTTP 主通道，插件要同步改，另计 15-20 人日；本次不改，所以 bridge v2 的收益拿不到。
- Windows 控制台注入与 ConPTY 只能在 Windows runner 上做端到端；Linux CI 给的是虚假安全感。至少预留 8-10 人日的 Windows 专项。
- 迁移与回滚演练：不是文档工作，是真实环境演练；至少 5-8 人日。
- 2C2G 上的浸泡与调优：dashboard 全量快照、慢 WS、长回答、大工具卡、多终端；至少 10 人日。

结论：这个方案更贵，但如果目标是长期单二进制、统一并发模型、摆脱 Python，值得。它不是「省人力」的方案，是「用 6-8 个月换未来 3 年维护成本」的方案。若团队只有 1-2 人，或未来 6 个月有强交付压力，应该只改 connector，server 留 Python；这不是保守，是算账。


---

# 最终决策（覆盖上面的方案建议）

2026-10-24 用户决定，**以下三条以本节为准**：

1. **server 直接用 SQLite**，不用 PostgreSQL。
2. **不要 Redis**。
3. **connector 全部 Go，不留 Python 边车**。

## 这三条各自意味着什么

### SQLite（server）

- 需要一次性数据迁移：PG → SQLite。要么写导出脚本（保留历史），要么从空库开始（不保留）。
- 失去「影子实例对比」这个验证手段（原本靠 Go 与 Python 连同一个 PG 逐键对比）。
  替代方案：录制真实流量回放 + fixtures 逐键比对，仍可做，但覆盖面弱一些。
- 需要自己实现：并发写串行化、WAL checkpoint 策略、备份/恢复。
- 预计增加 **15–25 人日**，并提高 timeline 并发正确性的实现难度
  （原本靠 PG 的事务与 advisory lock，SQLite 下要另设计）。

### 去 Redis

原本只存在 Redis 里的两样东西必须换地方：

- **终端注册表**（当前 24h TTL）
- **pending timeline**（未落盘的时间线缓冲）

方案：这两者进 SQLite（与服务端同库不同表）或进程内 + 定期快照。
预计增加 **10–15 人日**，并需要在故障注入里补「进程崩溃后终端与未落盘时间线能否恢复」的用例。

### connector 不留 Python

这条影响最大，有两个必须补齐的东西：

1. **Claude Agent SDK 没有 Go 版。**
   必须自己实现 `claude` CLI 的 `stream-json` 与控制协议
   （`control_request` / `control_response`，`can_use_tool` 是阻塞回调）。
   预计 **30–50 人日**，且是全项目最高风险项之一。

2. **Electron Desktop 通过 MeshCall 控制 connector。**
   要么在 Go 里重新实现 mesh 线协议，要么同批修改 Desktop。
   预计 **20–40 人日**（取决于能否拿到 mesh 协议规格）。

## 修订后的工作量

| 项 | 原估 | 增量 | 合计 |
|---|---|---|---|
| server | 100 | +15–25（SQLite） | 115–125 |
| Redis 移除 | 0（原计划 v1 保留） | +10–15 | 10–15 |
| connector | 170–180 | +50–90（SDK + mesh） | 220–270 |
| 联调/契约/切换 | 30–40 | +10（失去 PG 影子对比） | 40–50 |
| **合计** | **300–320** | **+85–140** | **≈ 385–460** |
| 含 25% 缓冲 | 375–400 | | **≈ 480–575 人日** |

日历：3 人约 **8–10 个月**，2 人约 **12–15 个月**。

## 由此产生的新前置条件

- **Desktop 必须先定**：改 Desktop，还是 Go 重写 mesh。这件事不定，connector 无法开工。
- **PG 历史数据是否保留**要先定，否则迁移脚本没法写。
- 失去 PG 影子对比后，**流量回放集必须先建立**，否则 server 切换没有验证手段。

这三条不定，后面的 phase 0 都无法退出。


## 追加决策（2026-10-24）

6. **不做桌面端**（Electron Desktop 不在本次范围）。
7. **PG 里的历史数据不要了**，Go 版从空库开始。
8. **Agent SDK 的 Go 缺口可以解决**，做法如下。

### 不逆向 SDK，改用 CLI 的公开面

Python/TypeScript SDK 本质是 `claude` CLI 的包装：spawn 子进程、
用 `--output-format stream-json` 读、用 `--input-format stream-json` 写。
所以 Go 不需要移植 SDK，只需要自己做同样的事：

```
spawn:  claude -p --input-format stream-json --output-format stream-json
                 --include-partial-messages --resume <id>
                 --permission-prompt-tool mcp__bridge__approve
                 --mcp-config <生成的配置>
读:     stdout 逐行 NDJSON（system / assistant / user / result / stream_event）
写:     stdin 逐行 NDJSON（user 消息）
审批:   自带的 MCP 服务端实现 approve 工具，阻塞等 connector 的决定
```

关键点：**审批走 `--permission-prompt-tool`，指向我们自己实现的 MCP 工具**，
不逆向 SDK 私有的 `control_request` 协议。该参数在盘点时已实测可用
（隐藏参数，只对 `-p` 生效）。

MCP 服务端形态：同一个 Go 二进制加一个子命令（`connector mcp-approve`），
由 `claude` 以 stdio 拉起，内部通过 127.0.0.1 与 connector 主进程通信。
不需要 cgo，也不需要额外依赖。

代价：SDK 相关从 30–50 人日降到 **20–30 人日**
（不必逆向私有协议，但仍要覆盖 resume、中断、流式聚合、子进程生命周期与僵尸回收）。

### 空库开始的含义

- 不需要写 PG → SQLite 的迁移脚本。
- 旧会话、旧时间线、旧设备全部作废；客户端需要重新配对设备。
- 好处：schema 可以直接按目标形态设计，不用背历史包袱。
- 注意：**上线切换前不要清 PG**，留到 Go 版验证通过再删，作为最后回滚手段。

### 修订后的工作量

| 项 | 上次估算 | 本次修正 |
|---|---|---|
| server（含 SQLite） | 115–125 | 105–115（无迁移脚本） |
| 去 Redis | 10–15 | 10–15 |
| connector | 220–270 | 205–235（SDK 20–30，无 Desktop） |
| 联调/契约/切换 | 40–50 | 40–50 |
| **合计** | 385–460 | **≈ 360–415** |
| 含 25% 缓冲 | 480–575 | **≈ 450–520 人日** |

日历：3 人约 **7.5–9 个月**，2 人约 **11–13 个月**。

### 仍然必须先定的一件事

**流量回放集**。用了 SQLite 又没有影子对比，这是切换前唯一的验证手段：
录一批真实的 HTTP / WS / connector RPC 流量，Go 版逐条回放，输出与预期逐键比对。
