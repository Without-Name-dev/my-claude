# 插件桥重构：审计与五层方案

日期：2026-10-24　｜　范围：Claude 插件、connector、server、手机端、Web 端

---

## 摘要

两个并行调查：一份是对**当前已实现代码**的审计（4 路独立审计 + 对抗复核），
一份是**五层重构方案**（插件/协议/手机/Web 各自设计后合成）。

最重要的三条：

1. **`classic.PermissionRequest` 的返回结构写错了**——少包一层 `decision`，
   按类型定义手机审批本不该生效。已修复（详见第一节）。
2. **子代理（subagent）的回合没有过滤**——子代理结束时会把主回合标记为完成、
   清空活动 turnId，导致手机 Stop 失效、主答案被截断。已修复。
3. **`$.http.fetch` 有 30 秒硬超时**——这是官方宿主的限制，
   「审批挂在一次 HTTP 请求上等人点」的设计撑不过 30 秒。**架构上必须改**，见第五节。

---

## 一、本次已修复（改动已写入插件并校验通过）

| # | 问题 | 改法 |
|---|---|---|
| 1 | `classic.PermissionRequest` 返回 `{behavior}`，类型要求 `{decision:{behavior}}` | 补上 `decision` 包装 |
| 2 | `turn.step` 把子代理的文本并进主答案，并反复清空缓冲 | `e.agentId` 存在时直接透传 |
| 3 | `turn.complete` 被子代理触发，清空 `activeTurnId`、把 outbox 标 done | 同上，`e.agentId` 直接返回 |
| 4 | `turn.start` 被子代理覆盖 `activeTurnId` | 同上 |
| 5 | `flush` 的 `writing` 守卫会丢弃 `turn.complete` 的收尾写 | 改成 dirty 标记 + 循环补写 |

验证：`claude plugin validate` 通过。**尚未做端到端回归**（你睡觉期间无法测），
醒来请先跑一遍：手机发消息 → 流式 → 工具卡 → 审批 → 中断。

---

## 二、审计发现（按严重度）

共 72 条，其中 17 条经过对抗复核。

### 严重（6）

**classic.PermissionRequest 返回了 SDK canUseTool 形状，手机审批/拒绝永远不生效** ✅复核通过

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 474-477
- 场景：手机点 approve/reject 后 hook 返回 {behavior:'allow'} / {behavior:'deny',message}。classic.PermissionRequest 的返回契约是 ClassicResultOf['classic.PermissionRequest'] = Pick<ClassicResult,'block'|'preventContinuation'|'stopReason'|'decision'>，决定必须放在 result.decision 里（types/claude-code.d.ts:1392、7502-7513；插件目录 .claude-plugin/types/claude-code/index.d.ts 同样如此）。返回 {behavior:...} 时引擎读不到 decision：要么该字段被忽略、要么 hook 因形状不合法被跳过（.catch 再 next(e)），两种情况下都由引擎自己的本地审批框/策略继续决定。结果：手机上批准后工具不会立即开跑（本地还要再批一次），手机上拒绝后工具仍可能因本地批准而运行；同时 connector 已把通知标记为 resolved（runtime.py:224-238），手机显示“已处理”。这类形状混淆很可能来自 connector 的 SDK 路径 permission_result_from_decision() 返回的 {'behavior':'allow'}（domain/approvals.py:94）。
- 建议：改为 return { decision: { behavior: 'allow' } } 和 return { decision: { behavior: 'deny', message: 'Rejected from the phone' } }。另外把 tsc -p 纳入校验：插件目录已有 tsconfig.json 和 .claude-plugin/types，这个对象字面量的多余属性会被 TS 直接报错。

**bridge 完全没有会话身份：任何进程里的插件都会接收任意会话的消息，outbox 与审批也会串会话**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/turns/actions.py` : 104-125；另见 connector/runtimes/claude/bridge/plugin_bridge.py:147-150、phone-pane/hooks/register.tsx:271-292、runtime.py:208-233
- 场景：机器上同时有 6 个活会话（~/.claude/sessions），phone-pane 只在其中一个进程里加载，但 heartbeat.txt/queue/outbox.json 都是整机共享。给会话 B 发消息时 start_turn 只检查 plugin_bridge.available()（仅读 heartbeat 时间）和 session.external_session_id；submit() 的 queue 载荷只有 id/text/queuedAt/token，没有 sessionId，插件 pollInbox 把消息无条件 $.prompt.submit(asUser:true) 进自己所在的会话 A。outbox 同样没有 sessionId，_watch_bridge_turn 会把 A 的回合文本当成 B 的答案推给手机；runtime._answer_bridge_approval 则在所有会话中挑第一个 delivery==plugin_bridge 的会话处理审批，两个 bridge 回合并行时审批卡会挂到错误会话，approve 也会按到错误回合。
- 建议：插件用 $.session.id() 把会话 id 写进 heartbeat/outbox/审批请求（classic.PermissionRequest 的 e.session_id 也直接可用）；submit() 载荷带目标 external_session_id，插件不匹配就拒绝并回写 ack；available()/read 按 external_session_id 过滤；审批按 payload 的 sessionId（找不到再退回 turnId）定位会话，找不到返回 ask。

**插件把 subagent 的 turn.step/turn.complete/tool.call 当成主回合：流式文本被清空重写、回合提前 done、abort 失效**

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 481-528（另 402-421）
- 场景：类型契约明确（.claude-plugin/types/claude-code/index.d.ts）：subagent 的每个 step 带 agentId、不触发 turn.start，但每个 hook 都会看到它的 step 和 complete；TurnCompleteFields.agentId 注明 “Every hook sees a subagent's turn”。主回合里只要调用 Agent/Task，subagent 的 turnId 与 outTurn 不同 → 481 行先把 outText 清空、outTurn 改成 subagent id 并 flush 新 turnId；连接器 _watch_bridge_turn 看到 turnId 变了就当自己的新回合，把 subagent 的文本作为主答案推给手机；subagent 的 turn.complete 又把 outStatus 设成 done，连接器立刻 _finish_bridge_turn 置 idle 并停止跟随，而主回合还在跑。同时 activeTurnId 被 517 行清空，pollAbort 此后把 $.turn.abort 请求全部丢弃。tool.call 也没过滤 agentId，subagent 的工具卡被塞进主回合（主 transcript 未必包含它们，可能永久孤儿）。plugin.log 里 49 条 turn done 对应约 44 条提交、outbox 在多个 turnId 间来回，与 subagent 干扰一致。
- 建议：turn.step/turn.complete/tool.call 开头 `if (e.agentId) return next(e)`（streaming hook 就原样 yield），只有 agentId===undefined 的主循环才可改 outText/outStatus/activeTurnId/toolEntries；turn.start 本来就只对主循环触发。

**队列消息不携带会话 id，任何会话的插件都会消费：跨会话执行与重复提交**

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 6-16, 241-310（配合 D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/bridge/plugin_bridge.py:147-211）
- 场景：手机给会话 B 发消息时，connector 只检查 plugin_bridge.available()（全局 heartbeat，plugin_bridge.py:147-150），submit() 写的 payload 只有 id/text/queuedAt/token（plugin_bridge.py:190-195），没有 sessionId。只要机器上任一终端加载了 phone-pane，它就会读到这条消息并 $.prompt.submit，于是会话 B 的话在会话 A 里执行；同时 B 的 _watch_bridge_turn 读同一个 outbox，把 A 的回答写进 B 的时间线。若两个会话都装了插件，两边各自读同一个 cursor.txt 再写（register.tsx:246-305），同一条消息会被提交到两个会话；后写者还可能把 cursor 写回较小值，导致已消费消息被重复提交。token 也无法阻止，因为两个实例读的是同一个 auth.json。
- 建议：payload 增加 sessionId（插件侧用 await $.session.id() 填），插件消费前比对自身会话 id，不匹配就不取；cursor 改为每会话一份（cursor-<sessionId>.txt）或干脆删除消费过的队列文件并用原子 rename/独占创建认领；connector 的 start_turn 只允许投递到它确认有该会话插件的目录。

**outbox/tools/abort 全局单份，多会话时回答与工具调用互相覆盖、stop 杀错回合**

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 6-16, 179-222, 394-400, 515-528（配合 D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/turns/actions.py:377-425, 537-547）
- 场景：A、B 两个会话都跑插件时，它们写同一个 outbox.json/tools.json/abort.txt。A 的 watcher 判定回合归属只比较 turn_id != submission.previous_turn_id（actions.py:381-385），不校验会话或提交 id，于是 B 开始回合后 A 的 watcher 会把 B 的流式文本当成 A 的答案推给手机，并可能把 A 的 execution 提前置 None；tools.json 同理把 B 的工具卡片混进 A。手机在 A 点停止时 connector 只写全局 abort.txt（actions.py:540），哪个插件先轮询哪个回合被中止，很可能是 B。approval 也一样：runtime.py:208-214 只挑“第一个 delivery==plugin_bridge 的会话”，两个 bridge 回合并存时审批会挂到错误会话。
- 建议：bridge 目录按会话分区（bridge/<sessionId>/outbox.json 等），或所有文件都带 sessionId 并由读写双方校验；abort.txt 带 target sessionId/turnId；_answer_bridge_approval 用请求里的 sessionId 选会话而不是扫描第一个；_watch_bridge_turn 在每次轮询时校验 outbox.sessionId == 自己。

**子代理的 turn.step / turn.complete 未按 agentId 过滤，主回合被提前判定完成**

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 481-528
- 场景：模型调用 Task/Agent 子代理时，引擎把子代理的 turn.step（TurnStepInput.agentId 存在、turnId 是子代理自己的，见 .claude-plugin/types/claude-code/index.d.ts:13438-13471）也送进这个 hook；插件没有 `e.agentId !== undefined` 的检查。turn.step 里 `if (outTurn !== e.turnId)` 会清空 outText、把 outTurn 换成子代理 turnId 并 flush，手机上已流出的主答案被替换成子代理内部输出；随后子代理的 turn.complete（TurnCompleteFields.agentId，index.d.ts:13307-13315）触发 turn.complete 处理器，`outStatus='done'` 并清空 activeTurnId，connector 的 watcher 看到 finished 就 _finish_bridge_turn（execution=None、idle），主回合余下的回答再也收不到；abort 也失效，手机还能在终端仍在跑时再发一条形成并发回合。
- 建议：所有 turn.* 处理器开头 `if (e.agentId !== undefined) return next(e)`（typings 的示例 index.d.ts:2771-2773 就是这么写的）；turn.step 生成器对子代理只原样 yield，不碰 outText/outTurn/outbox；turn.complete 只在主循环清 activeTurnId。

### 高（25）

**turn.complete 不区分 subagent 回合：清空 activeTurnId 并把 outbox 标成 done** ✅复核通过

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 515-519
- 场景：主回合里 Task/子代理结束时，引擎会向每个 hook 发该子代理自己的 turn.complete（types:13303-13315，'Every hook sees a subagent's turn'）。handler 无条件 517 activeTurnId='' 、518 outStatus='done'、519 void flush。于是主回合还在跑：(a) 手机 Stop 写的 abort 在 195-201 被当成“没有活动回合”丢掉（实机 plugin.log 已有 'abort pending with no active turn'）；(b) connector 的 _watch_bridge_turn 读到 box.finished 立即 break（actions.py:415-416）并 _finish_bridge_turn，手机端回合提前结束、后续主回合文本不再推送。实机 plugin.log 末尾在一个主回合内出现两次 'turn done'（b4c4b8ac chunks=44 之后紧跟 244935d2 chunks=0），就是这个路径。
- 建议：handler 开头 `if (e.agentId) return next(e)`，只有主线回合（无 agentId）可以改 activeTurnId/outStatus；日志改用 e.turnId 并带上 reason/agentId。

**turn.step 把 subagent 的步骤并进同一个 outbox，切回合时清空主答案文本** ✅复核通过

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 481-493
- 场景：子代理的 turn.step 带自己的 turnId（types:13440-13442）。485 行 'outTurn !== e.turnId' 就在主回合→子代理→主回合之间反复清空 outText/outChunks 并 flush 新 turnId。connector 只排除提交时的 previous_turn_id（actions.py:381-385），并不校验 turnId 是否是它正在跟的那个回合，于是子代理的文本会被当作主答案 partial 推送；主回合恢复后文本从空开始，比已推送的更短，connector 会 revision++ 用短文本覆盖手机上的答案。实机 outbox.json 在一个主回合运行期间 turnId 在 b4c4b8ac… 与 623ffcf2… 之间跳变、status=streaming、chunks=0，与这个路径一致。
- 建议：在 generator 开头 `if (e.agentId) { for await (const c of next(e)) yield c; return }`（或按 turnId 维护 Map，只把主线回合的流写进 outbox），子代理的 chunk 仍然原样下传。

**flush 的 writing 守卫会静默丢弃写请求，turn.complete 的收尾快照可能永远不落盘** ✅复核通过

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 158-160, 519
- 场景：turn.complete 里是 `void flush($)`；若此刻 120ms 节流的流式 flush 正在写，flush 的 `if (writing) return` 直接丢掉这次请求，且之后没有文本 chunk 再触发 flush。outbox 就永远停在 status='streaming'。connector 的 _watch_bridge_turn 会一直跟到 BRIDGE_TURN_TIMEOUT_SECONDS=3600（actions.py:33,376），期间 session.execution 仍被占用，新的手机消息会被 claude_turn_already_running 拒绝（actions.py:97），手机端的回合可能显示“运行中”最长一小时。写失败时同样没有重试（catch 后只是 writing=false，注释说的“下一次 flush 重试”对收尾这次并不存在）。
- 建议：flush 改成 dirty 标记循环：writing 时置 dirty 并返回，当前写结束若 dirty 再写一次；turn.complete 用 await flush($)（或 ticker 在 status==='done' 且没有新 turn 时持续重写）。最好的做法是给 outbox 写一个单调序号，connector 只在序号前进时采用。
- 置信度：likely

**CURSOR 缺失/被截断时 pollInbox 从 0 开始，会把 queue 里的全部历史消息重新提交** ✅复核通过

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 245-250, 300-305
- 场景：connector 从不删除 queue 文件（_next_seq 取 max+1，plugin_bridge.py:153-161；实机 queue 里 1.json–44.json 全在），所以 cursor.txt 是唯一去重手段。删除 cursor.txt（开发调试很常见），或整文件重写被热重载/worker 替换打断留下截断值（plugin.log 里已实锤截断写），parseInt 得到 0 或较小值 → 44 条历史消息全部以 $.prompt.submit({asUser:true}) 重新提交，模型收到几十个“用户回合”，消耗 token 且完全不是用户此刻说的。
- 建议：CURSOR 不存在且队列非空时，把游标初始化为当前最大 n（说明是新插件接手旧队列）而不是 0；游标写两份（cursor.txt + cursor.bak，读两边取 max）或改成追加式；connector 消费后删除/归档 queue 文件，避免目录无限增长。
- 置信度：likely

**审批 HTTP 请求被宿主 30 秒硬超时，长等待下手机审批失效并与本地审批脱节** ✅复核通过

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 448-469
- 场景：$.http.fetch 的 HttpInit 没有 timeoutMs（types:5217-5250），宿主会在 30000ms 后 abort（实机 plugin.log: '$.http.fetch(http://127.0.0.1:59125/approval) aborted: no complete answer within 30000ms'）。人看手机超过 30 秒才点，插件走 catch → next(e)，引擎弹本地审批框；而 connector 的 request_tool_approval 仍在等 future（interactions.py:208-211 无超时），手机卡片仍挂着。之后用户在手机上点“拒绝”，connector 只把通知标成 reject，对已经交回引擎的这次工具调用毫无作用——本地若批准，工具照跑，用户在手机上明确拒绝却被无视。这是“一个人工确认挂在 $ 调用上”的设计与宿主 30s 上限的硬冲突，不是能靠调参数解决的。
- 建议：插件侧在 fetch 超时/失败时 POST 一个明确的 cancel（让 connector 撤回/作废手机卡片并记录），不要留下会误导的已决通知；根本上要改架构：审批不能依赖一个最长 30s 的 $ 调用等待人类，应由 connector 直接应答引擎的审批通道（或 phone 端在 30s 内给出 ask 之外的快速回执），插件这个 hook 只做观察/转发。

**队列无会话归属：两个终端里的插件会各自 submit 同一条消息（重复执行 / 注入到错误会话）** ✅复核通过

- 位置：`connector/runtimes/claude/bridge/plugin_bridge.py` : 189-204
- 场景：用户同时开着两个 Claude Code 终端（都加载了 phone-pane），connector 里能看到两个 session。手机向 session A 发一条消息，submit 只写 queue/<n>.json（payload 里只有 id/text/queuedAt/token，没有 target session），插件侧 pollInbox 只看 token、不看会话，且 CURSOR 是同一个文件。两个插件的 400ms tick 独立运行，只要它们在同一窗口内各读一次 cursor（窗口≈pollInbox 的耗时/400ms），就会对同一个 n.json 各调用一次 $.prompt.submit：消息在 A、B 两个仓库/会话里各执行一次，产生两笔真实工具调用和两份账单；B 的 outbox 还会覆盖 A 的 turnId，connector 的 watcher 把 B 的文本当成 A 的答案上推。
- 建议：把消息绑定到会话：submit 时把 external_session_id 写进 payload 和文件名（queue/<session>/<n>.json），插件用自己进程的 session 标识（approval.json 里下发，或启动时由 connector 写入 session 文件）只读自己的子目录；cursor 也按会话分开（cursor.<session>.txt）。只做全局队列的话，至少要让插件在提交前用 O_EXCL 抢一个 claim 文件（queue/<n>.claim），抢不到就跳过，避免重复提交。
- 置信度：likely

**多 connector 进程时 auth.json 令牌互相覆盖，被拒消息的 cursor 仍被推进，消息永久丢失**

- 位置：`connector/runtimes/claude/bridge/plugin_bridge.py` : 90-103
- 场景：同一台机器上跑了两个 connector 进程（重复启动、或不同 checkout 各跑一份）。每个进程首次 submit 时 queue_token() 用自己的随机令牌覆盖 ~/.claude/phone-bridge/auth.json。插件 expectedToken() 每次轮询都重新读该文件，于是后启动的进程胜出；先启动的进程排进队列的消息 token 不匹配，插件 register.tsx 里走 refused 分支——但它随后仍然 `cursor = item.n` 并把 cursor 写回。结果：该消息被永久跳过，connector 侧 submit 返回成功、时间线里已出现 user 气泡、然后空等 90s（BRIDGE_START_TIMEOUT）转 idle，日志只有插件本地的 plugin.log 一行。用户看到的是“消息发出去了但模型没反应”。
- 建议：令牌不要由“最后写者”独占：auth.json 存 {tokens:[{id,pid,ts}...]}，插件接受任意未过期条目；写端用 O_EXCL 创建后再改为只读共享，进程退出时删除自己的条目。更简单的是把 bridge 目录按 connector 实例分隔（queue/<pid>/），插件按 approval.json 里给出的实例目录工作。另外插件侧“被拒”不应推进 cursor，应停在该文件等令牌变化。

**审批 handler 挂起期间不读 socket：插件断开/进程退出后 pending future、手机卡片和 waiting_approval 状态永不清理** ✅复核通过

- 位置：`connector/runtimes/claude/bridge/approval_server.py` : 94-106
- 场景：_handle 在 `await self.handler(request)`（runtime.py:224 → interactions.py:209 `await future`，无 wait_for）期间不再读 reader。此时终端被关闭、插件进程退出或 hook 被引擎按 10s 预算中止，socket 收到 FIN 也没有人去读，_handle 不会得知。只要手机上没人点，future 就永远 pending：_interaction_futures / _bridge_approvals 各留一条、notice 一直挂在 _notices 里、session_state 停在 waiting_approval，手机端那张审批卡既不能撤销也无法通过终端回答来消除（终端自己的权限对话框走的是引擎路径，不会回调 connector）。connector 重启前这些问题会一直累积。
- 建议：给 handler 包 asyncio.wait_for(...)，同时用一个后台 task 监控 reader.at_eof()/readexactly(1) 的 EOF：客户端一断就 cancel handler 并清理 notice（调用 interactions 的取消/关闭通知路径）；超时时返回 {"decision":"ask"} 让引擎自己问，并把卡片撤掉。
- 置信度：likely

**outbox 是全局单文件且 previous_turn_id 在锁外读取，并发提交时两个会话会认领同一个 turn**

- 位置：`connector/runtimes/claude/bridge/plugin_bridge.py` : 184-211
- 场景：outbox.json 只有一个，watcher 只用 turnId != previous_turn_id 判断“是不是我的”。两个会话（手机 + Web，或两个用户）在插件还没写出新 turnId 的窗口内先后 submit：A 在 t=0 读到 previous=X，B 在 t=50ms 读到 previous 仍是 X（turn.step 还没 flush），两条消息分别排队。插件先跑 A 的 turn，outbox.turnId 变成 TA：A、B 两个 watcher 都判为“ours”，把同一段文本各自 upsert 到两个 turn 的时间线里；B 的 turn 真正开始时（TB），B 的 watcher 已经因 box.finished 或 start_deadline 退出，TB 的文本再也不会流式上推，只能靠收尾的 history sync 补。
- 建议：让 outbox 携带归属：插件在 payload 里回写消耗的消息 id（payload.id 已经有 message_id），outbox 增加 turnId+messageId+sessionId，watcher 用 messageId 精确匹配而不是 turnId 差值；previous 快照挪到 _SEQ_LOCK 内读取，保证“读到的旧 turn 一定早于本条消息”。
- 置信度：likely

**回合相关性只靠 previous_turn_id 猜测，90s 没出现新 turn 就把回合当成结束**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/turns/actions.py` : 372-425（381-385、417-422）；plugin_bridge.py:184-210
- 场景：submit() 时读一次 outbox 的 turnId 作为 previous，watcher 把任何 turn_id != previous 的快照当成自己的回合。真实时序里至少四种会错：(a) 用户在终端同时打了一条消息，outbox 先出现用户自己的 turn，连接器把它的文本推给手机，真正的手机回合稍后开始却没人跟；(b) $.prompt.submit 是“会话空闲后才跑”（类型注释明确），终端忙时手机消息只是排队，90s 后 start_deadline 触发 break 并 _finish_bridge_turn 置 idle，之后消息才执行，手机没有流式，且此时新消息可以再进来；(c) submit 前读到半截 outbox/读取失败 → previous=''，上一个回合的 done 快照被当成新回合；(d) 插件崩溃留下旧 outbox，下一个 watcher 直接认领。BridgeSubmission.queued_at 算了却从未被使用（plugin_bridge.py:168/209），说明相关性设计没落地。
- 建议：queue payload 带 messageId+sessionId；插件在 turn.start（它带 text+turnId）把最近一次已提交的 messageId 与 turnId 一起写进 outbox 作为 ack；watcher 只认 messageId 匹配的回合，previous_turn_id/queued_at 只作辅助。没有 ack 时不要把 90s 超时当成回合完成，改为返回 queued 状态并继续等（或用 heartbeat/ts 区分“排队中”和“丢失”）。

**插件不检查 $.prompt.submit 的返回值，且无论提交成败都推进 cursor：消息可静默丢失而连接器返回成功**

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 285-305
- 场景：PromptSubmitResult 明确有 {drop} 变体（被 UserPromptSubmit/settings hook 拒绝时 submit 正常 resolve），也可能 throw。代码 292 行 await 完不检查返回值，295 的 catch 只写日志，随后 300-305 无条件 `cursor = item.n` 并写 cursor。连接器侧 plugin_bridge.submit 只要文件 rename 成功就返回 BridgeSubmission，start_turn 发布 user item 并返回 ok。结果：手机显示“已发送”，90s 后静默变 idle，模型从未收到；cursor 已越过该文件，重启也不补偿。
- 建议：检查 submit 结果是否含 drop、失败时不推进 cursor；插件把 (seq, id, accepted/drop) 写进 ack 文件，连接器在返回前等待 ack，ack 失败则返回 ok=false 并发 error notice；至少把 drop 的 reason 回传。

**插件消费队列不原子：cursor 写失败/多实例回退会把同一条消息重复提交给模型**

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 241-305（264-305）
- 场景：pollInbox 先读 cursor，再逐条 submit，最后写 cursor；写失败被 302-304 的 catch 吞掉（注释“retried next tick”其实是重放整条消息），下一轮用旧 cursor 再次提交同一文件。多个 poller（17 次 session.start 加载、6 个活会话、热重载残留实例）还会互相回退：慢实例在快实例写 cursor=27 之后写回 26，27 就会被再次提交。plugin.log 第 65/66 行正是连续的两次 `submitted 27.json chars=1`。重复提交等于让模型把同一句话执行两次（重复工具调用/写文件/计费）。
- 建议：单消费者 + 原子认领：提交前把文件 rename 到 claimed/（或 O_EXCL 锁文件），成功后再删/移走；cursor 写前重新读并取 max，保证单调；崩溃恢复时不重放已认领项，或至少用 messageId 在插件侧去重。

**流式 assistant 气泡永远停在 running，且与 transcript 最终消息是两个不同 id，手机永久多一条重复消息**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/turns/actions.py` : 367-416、489-517；reader.py:344/375-397
- 场景：_watch_bridge_turn 用 `bridge_<sid>_<turnId>` 作为 item.id、status='running' 推送部分文本；box.finished 时 415-416 直接 break，从不把该 item 标成 done。_finish_bridge_turn 随后 sync 历史，历史项由 _history_items_from_messages 用 native message id 生成（reader.py:375-397），turn_id 来自 transcript 的 user message uuid（reader.py:344），与 bridge item 的 id/turn_id 都不同。syncer 的 snapshot.complete=False，server 走 sync_timeline_items 只 upsert 不删除（server/.../connector_notifications.py:823-855），于是两条都永久留在 timeline：手机先看到一条永远转圈的 partial，再看到正式答案；order_seq 还可能让正式答案排在 partial 前面。
- 建议：回合结束（finished/超时/中断）时对同一 item 做最后一次 status='done' upsert；更彻底的是在 _finish_bridge_turn 里用历史项 id 覆盖 partial（或让 projector 支持 turn→item 覆盖表），或用 complete=True 的同步把它清掉。

**bridge 回合的中断是空操作：插件死了或 abort 失败时会话卡 running 最长 1 小时，期间所有新消息被拒**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/turns/actions.py` : 537-547（另 519-536、372-376）
- 场景：interrupt_session 对 delivery==plugin_bridge 只调用 request_abort() 写 abort.txt 就返回 {interrupted: requested:true}，既不取消 execution.task，也不清 session.execution，也不起停止计时。_watch_bridge_turn 只在 3600s deadline 或 outbox.finished 时退出，循环里从不看插件是否还活着（heartbeat/available）。插件进程被关、CLI 被杀、$.turn.abort 抛错时 abort.txt 永远没人消费，手机显示“已中断”但会话仍 running，start_turn 继续返回 claude_turn_already_running 直到整小时超时——用户没有任何办法从手机恢复。
- 建议：interrupt 时记录中断时间，让 watcher 在 N 秒内没看到 outbox done/aborted 就 cancel task 并走 runner.finish_execution(interrupted)；watch 循环定期检查 plugin_bridge.available()，心跳消失直接结束并置 idle/error；返回值应反映 abort 是否真的执行（读 abort-result.txt 或等 ack）。

**审批 HTTP 没有超时/断连检测：插件 30s 超时离开后连接器 future、连接和 notice 永久悬挂，会话停在 waiting_approval**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/bridge/approval_server.py` : 81-114；另见 interactions.py:191-211、actions.py:489-517、register.tsx:431-479
- 场景：插件用 $.http.fetch 发 ask，宿主默认 30s 超时（HttpInit 没有 timeout 选项），超时后插件 catch 掉转去 next(e) 走终端审批（plugin.log: 'no complete answer within 30000ms'）。但 BridgeApprovalServer._handle 还在 await handler，handler 在 await request_tool_approval 的 future；它不检测 reader EOF/写失败，而该 future 只有 respond_interaction 能 resolve，bridge 回合结束时 _finish_bridge_turn 又不调用 close_open_interaction_notices。结果：_interaction_futures/_bridge_approvals/_connections/notice 全部泄漏，手机留下一张永远可点的审批卡，状态停在 waiting_approval 直到回合结束；回合结束后卡仍然 open。副问题：_handle 的 except（97-102）把任何 handler 异常翻译成 {decision:'deny'}，连接器 RPC 故障会变成“拒绝工具”，本应返回 ask 让终端接手。
- 建议：审批等待加 <30s 的硬超时并返回 {decision:'ask'}，同时 cancel/expire future 并把 notice 转成 expired（新增 interactions.expire）；_handle 检测 EOF/写失败时做同样清理；handler 异常返回 ask 而非 deny；_finish_bridge_turn 里调用 close_open_interaction_notices。更稳的是改成插件轮询 decision 文件，摆脱 30s 限制。

**classic.PermissionRequest 的返回形状缺少 decision 外壳，手机 allow/deny 不生效**

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 474-478
- 场景：模块级 classic hook 的返回类型是 ClassicResultOf['classic.PermissionRequest']，即 `{ decision: PermissionRequestDecision, block?, ... }`（index.d.ts:1282-1393，其中 decision 注释明确对应 hookSpecificOutput.decision）。插件返回的是 SDK canUseTool 那套 `{behavior:'allow'}` / `{behavior:'deny',message}`，没有 decision 字段。引擎读不到决定，只能当作静默 hook（skipped/回落终端），于是手机点 allow 后终端仍弹审批；手机点 deny 后终端仍可能批准并执行工具，用户以为已经拒绝了。
- 建议：改为 `return { decision: { behavior: 'allow' } }` 与 `return { decision: { behavior: 'deny', message: 'Rejected from the phone' } }`；可参考旧的 approval-hook.ts 输出 hookSpecificOutput.decision 的形态，并给 handler 加类型标注让 tsc 校验。
- 置信度：likely

**审批没有超时和取消路径，手机离线会把会话永久卡死；回合结束时也不关闭已弹出的审批卡**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/turns/interactions.py` : 208-211（配合 C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx:448-478, bridge/approval_server.py:97-102, turns/actions.py:489-517）
- 场景：手机离线或没人点：request_tool_approval 无限 await future，approval_server 的 handler 无限 await，插件侧的 $.http.fetch 也没有超时（HttpInit 没有 timeout 字段，且 hook 预算在 $ 调用进行时冻结），于是引擎的权限检查永远不返回，这个会话既不能继续也不能结束，只能靠人在终端按 Esc。回合被外部中止/结束 时 _finish_bridge_turn 也不调用 close_open_interaction_notices，已经推到手机的 blocking 审批卡（domain/approvals.py:46 设置 blocking+response_required）会一直挂着，响应也没有人接收。
- 建议：给 request_tool_approval 加可配置超时（例如 120s），超时把 notice_transition 到 expired/closed 并返回一个“未决定”标记；插件端用 AbortController/$.clock 给 fetch 设超时后 return next(e)；_finish_bridge_turn 与 abort 路径都调用 close_open_interaction_notices；connector 重启/停止时也走同一关闭路径。

**$.prompt.submit 的 {drop} 与异常未校验，消息被静默丢弃且 cursor 照常推进**

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 285-305
- 场景：UserPromptSubmit 的设置 hook 拒绝（prompt.submit 返回 {drop: reason}，typings index.d.ts:9103-9145）或 submit 抛错时，插件只写一行 note，然后仍然 `cursor = item.n` 并写 cursor.txt。消息再也不会被投递；connector 的 _watch_bridge_turn 等 90s 后静默置 idle，手机只看到自己的消息、没有回答也没有任何错误提示（数据实际丢失）。
- 建议：检查 submit 返回值：drop 时写一个 failed 状态文件（或 outbox 的 error 字段）让 connector 把错误卡片推给手机，并且不要推进 cursor；抛错时回退/不推进 cursor 并重试有限次数；成功后才写 cursor。

**用 previous_turn_id 猜回合归属，用户同时在终端输入时把终端回合的回答推到手机**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/turns/actions.py` : 174-211, 372-425（配合 C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx:267-306）
- 场景：submit() 只记录 outbox 此刻的 turnId（plugins_bridge.py:184-186, 208-211），插件最多 400ms 后才消费。如果用户在终端紧接着发了一条 prompt，或上一个回合的 outbox 正好在 submit 时被非原子写坏（BridgeOutbox.read 返回 None → previous_turn_id=""），watcher 会把第一个 turnId 不同的回合当成自己的：把用户终端的回答流进手机的 bridge item（item_id 固定为 bridge_<session>_<turn>），真正的追问回合反而没人跟；90s 后 _finish_bridge_turn 把会话置 idle 而终端回合还在跑。插件从未记录“哪条 queue 消息产生了这个回合”。
- 建议：插件在消费消息后在 outbox 里写 msgId（connector 提交时分配的 id），turn.start 时再写 engine turnId 与提交文本；connector 只接受 outbox.msgId == 自己提交的 id 的回合，并在回合开始前用 e.text/msgId 双重比对；开始超时时间内没出现就上报错误而不是静默 idle。
- 置信度：likely

**带附件时同时绕开 bridge 和终端注入，改走第二个 SDK 进程 resume 同一会话**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/turns/actions.py` : 104-110, 135-144, 166-197
- 场景：任务假设的“附件回落终端注入”与代码不符：bridge 分支要求 `not attachments`（104-110），terminal 分支同样要求 `not attachments`（135-144），所以带图片的消息直接落到 SDK runner.drive_turn（166-185）。对一个正在终端里运行的 session，connection_for 会为同一 external_session_id 新建/复用 Claude SDK 子进程（lifecycle.py:215-285），与终端进程并发写同一个 transcript；手机看到的回答来自这个影子进程，终端里的人看不到，两边还可能各跑一个回合、互相覆盖历史。整个过程没有任何提示。
- 建议：明确产品语义：要么把图片落盘后用 @路径文本经 bridge 提交（prompt.submit 不展开图片但可以提交路径），要么对在终端中打开的会话直接返回“当前会话在终端中，暂不支持附件”的错误；不要静默走 SDK 并发 resume。

**/clear（或 resume 到别的会话）后会话 id 变化，bridge 不更新 external_session_id，历史同步写错文件**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/turns/actions.py` : 294-356, 501-506（配合 C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx:189-195）
- 场景：用户在终端 /clear，引擎换了一个 session/transcript id，但同一进程里的 phone-pane 继续消费队列，队列 payload 里没有 sessionId。connector 的 session.external_session_id 仍是旧的，_finish_bridge_turn 用旧 id 调 sync_session_timeline（actions.py:501-506），把旧会话的历史整段推回手机；新会话的回答既不在流式里也同步不到正确会话。connector 明明有 _update_external_session_id（lifecycle.py:852-868），bridge 路径从不调用。
- 建议：插件在 submit 和 turn.start 时把 await $.session.id() 写进队列/outbox；connector 发现与 session.external_session_id 不同就调用 _update_external_session_id 并按新 id 同步；/clear 后清空 outbox/activeTurnId/toolEntries 并让 connector 重置该会话的 cursor。
- 置信度：likely

**流式 partial item 用自造 id，history sync 用官方 message id，手机上出现重复且永远 running 的回答**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/turns/actions.py` : 367, 394-409, 489-517（对比 timeline/stream.py:98-117；服务端 D:/Develop/PyCharmProject/Agents-Anywhere-main/server/agent_server/services/connector_notifications.py:799-876）
- 场景：SDK 路径的 partial item id = stable_message_item_id(transcript message.id)，最后的 final item 用同一个 id 覆盖它；bridge 路径却用 `bridge_<session>_<turn>`。回合结束后 history sync 产生的是 claude_msg_...（sessions/reader.py:375-393, timeline/messages.py:836-841），而服务端 sync_timeline_items 只 upsert 不删除（complete=False，不走 replace），于是旧 partial 以 status=running 留在手机上，回答显示两遍，并且永远像在生成。每个 bridge 回合都会发生。
- 建议：让插件把引擎侧 assistant message id（turn.step 的响应里有）带到 outbox，partial item 用 stable_message_item_id 生成；或在 _finish_bridge_turn 里先发布一条 status=complete 的 final item（参考 stream_accumulator.final_item_id/next_final_revision），history 到达时覆盖同一 id。

**connector 每次重启换 token，queue 里未消费的旧 token 消息被插件跳过并推进 cursor → 丢消息**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/bridge/plugin_bridge.py` : 90-103（配合 C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx:274-280, 300-305）
- 场景：手机连发两条，connector 把两条都写进 queue，插件只消费了第一条；此时 connector 重启（升级/闪断）→ auth.json 换成新 token。插件下一轮读到第二条，`msg.token !== expected` → accepted=false，但仍然 `cursor = item.n` 并写 cursor.txt。这条用户消息永久丢失，手机/服务端拿不到错误（服务端可能已有乐观气泡）。token 写盘失败时更糟：queue 里带新 token，auth.json 却是旧的，此后所有消息都被拒。
- 建议：token 持久化到磁盘（不要每个进程新 mint）或改用安装级稳定 key；插件把 token 不匹配视为“稍后重试”并保留 cursor，同时对超过一定年龄仍不匹配的消息写 failed 状态让 connector 上报；cursor 只在成功 submit 后推进。

**队列消息不过期、不绑定会话，插件进程死亡后的消息会被后来的另一个会话执行**

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 267-306（配合 bridge/plugin_bridge.py:174-211）
- 场景：插件进程被杀或终端关闭后 30s 内 heartbeat 仍然新鲜，手机消息照旧入队但没人消费。几小时后用户在另一个 terminal 打开一个加载了 phone-pane 的会话，插件读到这些旧文件就逐条 $.prompt.submit，把几小时前发给另一个会话的话在这个会话里执行（asUser 模式，模型会当成用户原话）。payload 里有 queuedAt，但插件从不检查。
- 建议：payload 带 sessionId 并校验；queuedAt 超过 N 秒（例如 60s）直接丢弃并写 failed；消费前用原子 rename 认领文件，避免被后来的实例误取。

**abort 只写文件不验证结果；热重载后 activeTurnId 为空，stop 实际无效且残留请求会误杀下一条消息**

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 33, 186-221, 394-396（配合 turns/actions.py:537-547）
- 场景：回合进行中插件热重载 → 模块级 activeTurnId 被重置为 ''；turn.start 不会重放，turn.step 只更新 outTurn。手机点停止时 pollAbort 认为“没有活动回合”，不消费 stamp、只写一条 note，connector 的 request_abort 返回 True，手机显示已停止，但引擎里的回合继续跑。更糟的是 stamp 被保留，等用户下一条消息 turn.start 之后立即被 abort（新消息一发出就被杀）。子代理 turn.complete（见第 3 条）也会提前清空 activeTurnId。另外 interrupt_session 只要文件写成功就返回 interrupted=requested=true，从不读 abort-result.txt 或 outbox 确认。
- 建议：activeTurnId 在 turn.step/tool.call 里也从 e.turnId 补上；没有活动回合时丢弃 stamp；abort 未命中要写 abort-result 让 connector 上报；connector 等 abort-result.txt（或 outbox 状态变化）再向手机报告成功，超时则报失败。

### 中（30）

**无活动回合时 abort 时间戳被无限期保留，会误杀下一个回合** ✅复核通过

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 192-205
- 场景：用户在回合刚结束（或子代理 complete 把 activeTurnId 清空、或热重载）时点 Stop，abort.txt 写入时间戳。195-201 的分支故意不消费它（注释：可能落在 turn.start 之前）。十分钟后用户发新消息，回合一启动，tick 读到同一个 stamp > lastAbort → 立刻 $.turn.abort，新回合刚开头就被杀，用户只看到莫名其妙的中断。
- 建议：保留“刚落在 turn.start 前”的窄窗口即可：只接受最近 5–10 秒内的 stamp，更旧的丢弃并记日志；或者让 connector 把 abort 写成 {ts, expectedTurnId/requestId}，插件核对后再执行。
- 置信度：likely

**currentApproval 只有一个槽位，并发 PermissionRequest 互相覆盖/清空** ✅复核通过

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 112, 436-439, 469-472
- 场景：并行的多个工具（以及并行子代理里的工具）各自触发 classic.PermissionRequest 时会同时挂起：第二个覆盖 currentApproval；先返回的那个在 finally 里把它置 null，于是面板上第二个请求的 approve/reject 点击时 sendPaneDecision 在 137-139 直接 return，按钮完全没反应；approval atom 也只显示其中一个问题。手机路径不受影响（connector 每个请求一条 notice），坏的是本地面板和“两个表面同时显示”的承诺。
- 建议：用 Map<toolUseId, {tool}> 维护待决集合，按钮回调带上 toolUseId，finally 只删自己的 key；ui.render 渲染待决列表（或至少显示“还有 N 个”）。
- 置信度：guess

**bridge 目录硬编码，忽略连接器的 AGENT_CONNECTOR_CLAUDE_BRIDGE_DIR，且不按会话隔离** ✅复核通过

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 6-16
- 场景：connector 支持 AGENT_CONNECTOR_CLAUDE_BRIDGE_DIR（plugin_bridge.py:37,54-58），插件却固定 C:/Users/25640/.claude/phone-bridge。一旦用自定义目录，queue/cursor/outbox/auth/approval 全读写到两处，表现为心跳“正常”但消息永远不进、审批 ECONNREFUSED。其次所有文件名都不带 session id：同一个 mod 在两个会话加载（CLAUDE_CODE_PLUGIN_DIRS、安装后、或用户在另一个终端开了同一目录）时共享 cursor/outbox/abort——一个 abort 会同时杀两个会话的回合，一个会话的 outbox/tools 会被另一个覆盖。
- 建议：用 `await $.env.get("AGENT_CONNECTOR_CLAUDE_BRIDGE_DIR")` 读同一目录（缺省再从 USERPROFILE 拼），并把 session_id 放进目录或文件名（queue/<sid>/、outbox-<sid>.json）；connector 侧用同一命名。

**消息无法投递时仍推进 cursor：token 缺失/解析失败/白空格/submit 被拒都是静默丢失** ✅复核通过

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 270-305
- 场景：(a) connector 首次写 auth.json 失败时 queue_token() 只在 _TOKEN is None 时写一次、之后不再重试（plugin_bridge.py:90-103），插件 expectedToken 返回 null，275 行 `!expected` 把每条消息都判成 token 不匹配、记一行日志、cursor 前进 → 入站永久失效直到重启 connector；(b) connector 重启换了 token，重启前 <400ms 内刚入队的旧 token 消息被拒并消费；(c) 被别的 prompt.submit hook/策略拒绝的提交只记日志，cursor 照样前进，永不重试；(d) JSON 解析失败或纯空白文本也在 281-285 静默消费。手机端完全不知道这些消息没送达。
- 建议：只有在“token 存在且明确不匹配”（确定是别人写的）时才消费；expected 为 null 或 submit 失败时不推进 cursor（下一轮重试），或把该文件标记为 pending 并告警；对空白消息给出日志而不是无声丢弃。

**热重载清空模块变量：abort 目标/回合文本丢失，durable 的 approval atom 会留下僵尸按钮** ✅复核通过

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 23-40, 439-472, 515-519
- 场景：文档明确 reload 是 fresh load，模块变量从零开始而 $.state 保留。开发期实机 plugin.log 有 17 次 'session.start plugin loaded'：中途 reload 后 activeTurnId='' → 手机 Stop 丢；outTurn/outText 清空 → 下一次 turn.step 把 outbox 文本从新前缀重写（connector 覆盖手机上的 partial）；relaod 时若有审批挂着，finally 不一定执行，$.state 里的 approval 字符串会永远留着，面板一直显示一个不存在的请求，按钮点了只会 'ignored'。实机日志里的 'turn done turnId= chunks=0' 就是 outTurn 为空时写出的。
- 建议：把 activeTurnId/outTurn（以及每 120ms flush 一次的 outText 尾部）放进 $.state，重载后能接上；session.start 时清空 approval atom；审批请求带一个 request id，按钮回传该 id 并核对。

**终端本地正在跑 turn 时从手机发消息：插件并入当前 turn，turnId 不变，watcher 永远等不到新 turn**

- 位置：`connector/runtimes/claude/bridge/plugin_bridge.py` : 184-211
- 场景：用户在终端手打了一条 prompt，Claude 正在流式输出（connector 的 session.execution 为 None，因为它不是 connector 发起的）。此时手机发来一条消息：actions.py 的 guard 看不到执行中的 turn，于是 submit 成功，previous_turn_id 正好等于当前正在跑的 turnId。插件 $.prompt.submit 把消息排进引擎队列，引擎按“忙时排队”在同一个 turn 之后处理，turn.step 的 e.turnId 可能不变化（或变化但 flush 晚于 watcher 的 90s 判定）→ watcher 一直判 is_ours=False，90s 后放弃并置 idle，手机端这条消息和它的回答都不会出现（本地终端里却真的发生了）。
- 建议：submit 之前用 outbox.status=='streaming' 且 turn 不是本 connector 发起的作为“忙”信号，拒绝提交并告知调用方（或写入队列但标记 needsNewTurn）；同时让插件在接管/排队时把 queue 消息 id 写进 outbox，connector 用 messageId 而不是 turnId 判定归属。
- 置信度：guess

**MAX_BODY_BYTES=256KB 对大 tool_input 直接 400，手机端静默看不到这次审批**

- 位置：`connector/runtimes/claude/bridge/approval_server.py` : 133-134
- 场景：插件把 e.tool_input 原样放进请求体（register.tsx 的 classic.PermissionRequest，没有像 toolInputOf 那样截断）。一次 Write 写大文件、或 Edit 带 300KB 的 old_string/new_string 时，Content-Length > 256KB，_read_request 返回 None，_handle 回 400 {"error":"bad request"}。插件 catch 不到（HTTP 200/400 都算成功），JSON.parse 后没有 decision，于是 decision='' 落到 next(e)——手机和 Web 永远收不到这张审批卡，只有终端弹窗，且两端都没有任何错误日志。
- 建议：插件侧对 tool_input 做与 toolInputOf 相同的大小裁剪（例如 8KB/字段，并加 truncated 标记）；或把上限提高到能容纳最大的合法输入（如 2MB）并单独对超限返回结构化 {"error":"too_large"}，让插件 note() 一行而不是静默。

**HTTP 首行、方法、路径、Content-Type 全不校验**

- 位置：`connector/runtimes/claude/bridge/approval_server.py` : 120-122
- 场景：_read_request 读了首行直接丢弃（line = await reader.readline() 后没有任何使用）。因此 GET/任意 method、任意 path（包括 /favicon.ico）、带 JSON body 的请求都会被当成审批请求执行；浏览器跨站发起的 text/plain POST 只要能拿到 token（approval.json 同用户可读）就能 settle 一个审批。模块 docstring 声称“answers nothing but the decision it was asked for”，实际没有路径/来源约束，也没有 Content-Type 检查，本地任意进程可以 POST {action:'cancel', toolUseId, decision:'allow'} 直接放行一个工具。
- 建议：解析并严格校验请求行（POST /approval HTTP/1.0|1.1），校验 Host 为 127.0.0.1[:port]、Content-Type 为 application/json；不匹配直接 404/405。同时把 token 比较换成 hmac.compare_digest（低价值但一行）。

**header/body 读取无超时、无上限：半开连接会长期占住一个 task 和 fd** ✅复核通过

- 位置：`connector/runtimes/claude/bridge/approval_server.py` : 120-135
- 场景：本机进程（或卡死/被 suspend 的插件）建立连接后只发半行、或声明 Content-Length: 1000 只发 1 字节就不再发：readline()/readexactly() 无限等待，_handle 永远不返回，task 留在 _connections、fd 不释放，直到 connector stop。同时 header 循环没有条数上限，超长 header（>64KiB）会让 readline 抛 ValueError，被兜底 except 记一条 ERROR 堆栈（日志噪音）且不回任何响应。_connections 也没有并发上限。
- 建议：给每次 readline/readexactly 包 asyncio.wait_for（例如 10s 总预算），限制 header 条数与总字节（如 64 条 / 32KB），超限回 431 并关闭；用 asyncio.Semaphore 限制并发连接数；ValueError/LimitOverrunError 单独捕获，按 400 处理而不是打堆栈。

**endpoint 文件发布失败（含 port 为 0）被吞掉，插件静默回落到终端提问**

- 位置：`connector/runtimes/claude/bridge/approval_server.py` : 54-63
- 场景：start() 里 mkdir/write_text/os.replace 任一步抛 OSError（目录权限、杀软锁、磁盘满、或 bridge_dir 指向一个只读路径），只 logger.warning 就继续，server 已经在监听、token 也已生成。磁盘上留下的是上一进程的 approval.json（旧 port 已关闭、旧 token），插件 readEndpoint 读到后每次审批都 fetch 一个死端口 → 抛错 → next(e)，手机端再也收不到审批，connector 侧只有一行 warning。若 sockets 为空导致 self.port==0，写进去的是 port 0，插件因 0 为假值当作“没有端点”，同样是静默回落。
- 建议：发布失败时把 server close 掉并让 start() 返回 False/抛异常，由 runtime 明确记录“审批通道不可用”；启动前先删除旧的 approval.json（或写入带 pid 的字段，插件校验 pid 存活）；self.port == 0 视为失败而不是继续。

**stop() 无条件删除 endpoint 文件、取消连接任务但不 await；多实例会互删**

- 位置：`connector/runtimes/claude/bridge/approval_server.py` : 65-79
- 场景：1) 两个 connector 共享 bridge 目录时，A.stop() 会把 B 刚发布的 approval.json 删掉，B 的 server 还在监听但插件读不到端点，所有审批静默回落。2) stop() 只 task.cancel() 然后 _connections.clear()，从不 await；被取消的 handler 的 finally 里还有 await writer.wait_closed()，任务可能在 runtime 后续 stop 阶段被销毁，出现 'Task was destroyed but it is pending'，写响应前的 close 也不保证完成。3) 取消 handler 会让 request_tool_approval 的 future 被 cancel，但已经推给手机的 notice 不会被撤。
- 建议：stop() 先读取 approval.json 确认 port/token 是自己写的那份再 unlink；用 await asyncio.gather(*tasks, return_exceptions=True) 收敛连接任务；取消前对每个 pending 的 _bridge_approvals 调一次 notice 撤销/关闭，保证手机卡片不悬挂。

**_next_seq 失败退化为 0，或 queue 被清理而 cursor.txt 保留：新消息编号落在 cursor 之下，被永久忽略**

- 位置：`connector/runtimes/claude/bridge/plugin_bridge.py` : 153-161
- 场景：两个触发：(a) queue_dir().iterdir() 抛 OSError（目录暂时不可读/被占用）时返回 0，若此时 _LAST_SEQ 也是 0（进程刚起），seq=1，os.replace 直接覆盖旧的 1.json；插件只处理 name > cursor 的文件，1 号早被跳过，这条消息永远不会被执行。(b) 用户/清理脚本删掉了 queue/*.json 但留下 cursor.txt（比如 500），新进程 _next_seq=0 → 从 1 开始写，1..500 全部 ≤ cursor，插件看都不看，同步地 connector 还会为每条消息空等 90s。两种情况都没有任何错误暴露。
- 建议：把 cursor.txt 作为 seq 下界读入（seq = max(_LAST_SEQ, _next_seq(), read_cursor())），并持久化一个 seq-floor 文件；_next_seq 遇 OSError 不要返回 0，应抛给调用方让本次 submit 失败（返回 None，走终端回退）而不是猜一个会撞车的编号。

**BridgeOutbox.read 对 ts 无类型保护，合法 JSON 也能抛出未捕获异常**

- 位置：`connector/runtimes/claude/bridge/plugin_bridge.py` : 250-255
- 场景：chunks 用 isinstance 守卫，ts 却直接 `float(data.get("ts") or 0.0)`。outbox.json 被旧版本/其它工具/手工改写成 {"ts":"2026-10-24"} 时抛 ValueError，{"ts":[]} 时抛 TypeError。try/except ValueError 只包住 json.loads，所以异常直接冒出去：submit() 里没有 try，会一路打断 start_turn（用户点发送报错）；watcher 里则被外层 except Exception 吞掉并立刻收尾，本轮答案不再流式上推。
- 建议：与 chunks 一致：value = data.get("ts"); ts=float(value) if isinstance(value,(int,float)) else 0.0，外层 try 同时捕获 (ValueError, TypeError)。

**request_abort 只写文件就返回成功；插件不在或已死时 interrupt 谎报成功，会话可卡到 1 小时** ✅复核通过

- 位置：`connector/runtimes/claude/bridge/plugin_bridge.py` : 122-131
- 场景：bridge turn 运行中插件崩溃/终端被关，heartbeat 在 30s 内仍是新鲜的，outbox 冻结在 status=streaming。用户点“停止”：interrupt_session 走 request_abort()，写 abort.txt 后无条件返回 True，UI 显示已中断；但没人消费该文件，_watch_bridge_turn 因为已经 follows=True 不再受 start_deadline 约束，会一直轮询到 BRIDGE_TURN_TIMEOUT_SECONDS=3600，期间该 session 的 start_turn 一直返回 claude_turn_already_running。abort-result.txt / abort.txt 也都没人清。
- 建议：request_abort 写完后短轮询 abort-result.txt（或 heartbeat 是否推进）确认插件已消费，超时返回 False 让上层报“未能中断”；watcher 侧用 outbox.ts 与 heartbeat_age() 判活，超过若干秒无变化就直接收尾；插件在 turn.complete/abort 后清理 abort 文件。
- 置信度：likely

**热路径同步整文件读写：outbox.json/tools.json 以 5Hz 全量读+解析，queue 目录只增不减，每次 submit 全目录扫描**

- 位置：`connector/runtimes/claude/bridge/plugin_bridge.py` : 106-119, 153-161, 232-255
- 场景：actions.py 的 watcher 每 200ms 调一次 BridgeOutbox.read + read_tools，二者都是同步 read_text + json.loads，且直接在事件循环里执行。tools.json 最多 60 条、每条 14 个字段各可达 8000 字符，单文件可到 MB 级，5Hz 解析会周期性阻塞事件循环（同一循环上还有 phone 消息与审批）；队列文件插件从不删除（只推进 cursor），跑几天后 queue 目录上千个文件，每次 submit 的 _next_seq 都要 iterdir 全扫一遍，且 submit/queue_token 的 write_text、os.replace 也是同步 I/O。
- 建议：tools.json 用 (mtime, size) 缓存，未变化就复用上次解析结果；outbox 只有 turnId/status/chunks 变化时才重新解析 text；把文件 I/O 放到 asyncio.to_thread；队列文件由插件消费后删除（或 connector 按 cursor 批量清理），_next_seq 改成持久化的 seq counter 而不是目录扫描。

**_finish_bridge_turn 先清 session.execution 再 await 历史同步，窗口内新回合会被随后的 idle 状态覆盖**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/turns/actions.py` : 495-517
- 场景：499 行在未持有 session.execution_lock 的情况下把 execution 置 None，然后 await sync_session_timeline（读 transcript + 多次 host RPC，可达数百 ms 到数秒）。这期间 start_turn 拿到锁、看到 execution 为 None，启动新回合并把状态推成 running；旧 watcher 同步返回后 512-517 无条件 session_state_update(idle)，把新回合在手机/能力位上报成空闲，stop 按钮消失，用户再发消息会得到 claude_turn_already_running。若新回合也是 bridge 回合，旧 watcher 的 sync 还可能和新回合的 live item 交叉写 timeline。
- 建议：不要手写收尾：调用 runner.finish_execution(session, execution, terminal_event)（它持 execution_lock 并校验 execution 身份）；若必须“先放锁再 sync”，则 sync 后重新取锁并只在 session.execution 仍为 None 时才发 idle，并用 execution.finalization_lock 串行化。

**watch 循环里任何一个 host RPC 失败都会让连接器放弃跟随，并把仍在运行的回合置 idle**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/turns/actions.py` : 394-432
- 场景：timeline_item_upsert / _publish_bridge_tools 的任何异常（连接器与 server 断线、server 5xx、host RPC 超时）都会被 428 的宽 except 捕获，finally 立刻 _finish_bridge_turn：清 execution、发 idle、sync。可 Claude 还在跑，这个回合之后的流式文本和工具卡都不再跟踪；用户可以马上再发一条，产生两个未关联的回合，后续 outbox 又会被下一个 watcher 误认。
- 建议：把“跟随失败”和“回合结束”分开：对 upsert/tools 的异常做指数退避重试，只在连续失败超过阈值后结束；结束时应发 error/unknown 状态并在恢复后补一次 sync，而不是当正常完成置 idle。

**_start_bridge_turn/_start_terminal_turn 在已经发出 running 之后失败，会清掉 execution 但把 running 永久留在缓存里**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/turns/actions.py` : 314-348（另 219-259）
- 场景：两个方法先 session_state_update(running)（316-321 / 220-225），随后才 publish_user_message / watcher.start / create_task。任何一步抛错（host RPC 失败、时间线写失败）走到 except BaseException：清 session.execution、set finished、re-raise，但不再发 idle/error。RuntimeSessionStateCache.update 在 await host 之前就写入本地 _states（runtime_protocol/state_cache.py:97-106），所以本地缓存也停在 running，get_session_state 会一直返回 running 直到下一次成功回合覆盖，手机端同样看到 running。bridge 分支此时 prompt 已进队列，终端最终会执行它，用户却收到失败。
- 建议：except 里先 session_state_update(idle/error) 再 re-raise；bridge 分支应在 submit 之前完成所有可能失败的初始化，失败时写 cancel/abort 标记或让插件不消费；用 try/finally 保证状态收敛。

**request_tool_approval 把 future 注册和 notice/state 发布放在 try/finally 之外，on_notice 或 notice_upsert 抛异常会永久泄漏 future 并留下 open notice**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/turns/interactions.py` : 191-211（另 240-260）
- 场景：194 行先写 _interaction_futures[notice_id]，195 的 await notice_upsert、196-197 的同步 on_notice、198 的 await session_state_update 都在 await future 的 try 之外。任何一步抛错（host 断线、on_notice 回调抛异常——runtime 的 remember 目前不抛，但接口允许）函数直接退出：future 永远没人 await、finally 不会 pop，_interaction_futures 永久增长，registry 里那条 open notice 也不会关闭；request_user_input 的 _input_requests 同样泄漏。
- 建议：把 future 注册、notice 发布、state 更新都放进 try，finally 统一 pop future 和 input request；on_notice 用 try/except 包住（失败只记日志）。

**respond_interaction 在 done() 检查与 set_result 之间有多个 await，可能撞上 close_open_interaction_notices 抛 InvalidStateError**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/turns/interactions.py` : 63-101
- 场景：respond_interaction 先 future.done() 检查（64），然后 await notice_upsert(responding)（100）等多步 RPC，最后 future.set_result（101）。同一时刻 finish_execution 持锁调用 close_open_interaction_notices，把同一个 future set_result 成 reject。手机再点批准就会在 101 抛 InvalidStateError，RPC 500，notice 停在 responding，用户看到“操作失败”。
- 建议：set_result 前再判 if not future.done()，或包 try/except InvalidStateError；更好的是让 future 只有一个 resolver（决定先写 notice，再由单一收尾统一 resolve）。

**bridge/terminal 回合结束从不发 session.turnEnded，server 的 latest_turn_end_seq 不前进，会话永远不会有未读/通知**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/turns/actions.py` : 489-517（另 277-282）；lifecycle.py:836
- 场景：SDK 路径在 lifecycle.publish_terminal_state 里调用 host.session_turn_ended（lifecycle.py:836），bridge 和 terminal 路径只发 session_state_update(idle)。server 端 unread 完全由 latest_turn_end_seq > last_read_seq 决定（server/.../infra/repositories/sessions.py:1912），而 latest_turn_end_seq 只在处理 session.turnEnded 时更新（timeline.py:461）。所以插件送达的回合结束后：会话列表 unread 永远 false、dashboard 的 session_changed/usage 事件缺失，手机不会提示“有新回复”。
- 建议：_finish_bridge_turn 和 _watch_terminal_turn 结束前调用 host.session_turn_ended(turn_id=execution.turn_id, outcome='completed'/'interrupted', metadata={'source': ...})，或直接复用 runner.publish_terminal_state。

**Transcript 监听服务只增不删：24h 到期的 watcher 和首次 start() 失败的 watcher 永远不会被重建，活会话静默失去实时同步**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/watch/service.py` : 61-80；另 tailer.py:67-78、101-102
- 场景：74 行先把 watcher 放进 _watchers 再 await start()；start() 在 transcript 文件还不存在时返回 False，但 entry 已经留下，之后 `external in self._watchers` 永远跳过它。watcher._run 到 MAX_WATCHER_LIFETIME_SECONDS=86400 就 return，条目同样留在字典里，_run 不会重建。于是该会话在 connector 重启前不再有亚秒级同步，只剩周期扫描；被删除/归档的会话条目也不会清理，500 个会话时字典和轮询都长期占用。
- 建议：start() 返回 False 时不入库；给 watcher 记 started_at，刷新时移除 done()/过期条目并重建；会话从 list 消失时 stop 并删除；用 generation 标记避免热更新串。

**_finish_bridge_turn 在 history sync 之后无条件写 idle，会盖掉新回合的 running**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/turns/actions.py` : 489-517
- 场景：上一个 bridge 回合结束：先 `session.execution = None`（499），再做 history sync（可能几十到几百 ms）。用户看到答案立刻从手机发下一条（非常常见），start_turn 看到 execution=None 就开了新回合并把状态设为 running；旧的 _finish_bridge_turn 随后 session_state_update(idle)，手机显示空闲、capabilities 说没有活动回合，而引擎正在跑。这条新回合的审批/停止判断都会受影响。
- 建议：在 512 的 idle 更新前用 execution_lock 重新检查 session.execution is None（或比较 execution 身份），只有自己仍是当前 execution 时才置 idle；idle 更新与 execution 释放放在同一临界区。

**审批 handler 内部异常时返回 deny，违背“绝不替用户拒绝”的设计**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/bridge/approval_server.py` : 97-102
- 场景：request_tool_approval 内部抛异常（notice_upsert 到 server 的 RPC 失败、session_state 更新失败等）→ _handle 捕获后返回 {"decision":"deny"} → 插件按 deny 处理，模型收到 "Rejected from the phone"。用户根本没看到审批卡，工具被拒；而且插件只认 decision，忽略 reason，原始错误被丢弃。plugin 注释明确写着失败时应回落到引擎自己的答案。
- 建议：handler 异常时返回 {"decision":"ask"} 让引擎回落到终端，并单独 host.runtime_error；只有用户明确操作才返回 allow/deny。

**AskUserQuestion 在 bridge 路径被当成普通审批，手机无法作答，回合会一直 running**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/runtime.py` : 221-231（对比 turns/interactions.py:146-160）
- 场景：SDK 路径对 AskUserQuestion 特意走 request_user_input（选项/输入），bridge 路径一律 request_tool_approval，手机只显示 Approve/Reject。点 Approve 只是允许该工具执行，问题真正弹在终端，手机端回合一直 running、无法完成（除非有人在终端回答）；点 Reject 则问题被直接拒绝。
- 建议：在 _answer_bridge_approval 里识别 tool == 'AskUserQuestion' 并走 request_user_input 的 notice（把 questions 传过去），把用户的选择作为 updatedInput 回传给插件；插件按 PermissionRequestDecision.updatedInput 返回。
- 置信度：likely

**transcript watcher 达到 max_lifetime 后不会被移除/重建，watch 服务字典只增不减**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/watch/service.py` : 61-80（配合 history/tailer.py:100-102）
- 场景：ClaudeTranscriptWatcher 到 24h 直接 return，但 service 的 _watchers 只按 external id 判重、从不删除；此后该会话永远不会再有 watcher（快路径永久失效，只剩周期扫描），长期运行下每个出现过的会话都留一条永不清理的记录，session 被删除也不会释放。
- 建议：watcher 结束时通过 done callback 从 _watchers 移除；refresh 时清理不在 list_sessions 里的条目；接近 max_lifetime 时先停再重建。

**queue 目录无界增长，插件每 400ms 两次全量 list，connector 每次 submit O(n) 扫描**

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 253-263, 330-331（配合 bridge/plugin_bridge.py:153-161）
- 场景：队列文件从不删除（实测目录里已有 44 个），插件每 400ms 在 tick() 里 list 一次、pollInbox 里再 list 一次；文件数增长后每个会话每秒要做两次 O(n) 目录扫描与 stat。connector 每次 submit 还要 iterdir 全部文件算 max（_next_seq），多会话时放大。
- 建议：消费后删除或移到 archive/<sessionId>/；用持久化的单调 seq 计数器文件替代扫描；tick 里复用 pollInbox 的 list 结果；空闲时降低轮询频率（例如 1s），有活跃回合时再加快。

**tools.json 每次工具事件全量重写（实测 126KB），connector 回合期间 5Hz 全量 JSON parse**

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 103-109, 402-419（配合 turns/actions.py:434-448）
- 场景：每个 tool.call 前后各写一次整个 entries 数组（每项 input 最多 8KB、最多 60 项），实测 phone-bridge/tools.json 已达 126KB；bridge 回合期间 connector 每 200ms 读一次并 json.loads 全部，约 600KB/s 的解析量，还拖慢每个工具调用（写入是 await 的）。
- 建议：改成增量协议（append-only jsonl 或单独 per-tool 文件），input 截断更小；connector 用 size/mtime 跳过未变化的读取，只处理新增条目。

**AGENT_CONNECTOR_CLAUDE_BRIDGE_DIR 对插件无效（路径硬编码），配置后 bridge 静默失效**

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 6（配合 bridge/plugin_bridge.py:54-58）
- 场景：connector 支持用 AGENT_CONNECTOR_CLAUDE_BRIDGE_DIR 自定义目录，但插件永远用 C:/Users/25640/.claude/phone-bridge。配置了该 env 的机器上 connector 读写别处，插件在默认目录写 heartbeat、读 queue，available() 永远 false（或读到残留的旧 heartbeat），bridge 静默不工作、每次回落终端注入且没有提示。
- 建议：插件从同一环境变量/配置读取路径，或删掉 connector 的这个 env；启动时校验 approval.json/auth.json 与插件目录一致并写日志。

**asUser:true + 磁盘上的 queue token，使同用户进程（包括模型自己的 Bash）能伪造“用户原话”**

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 285-292（配合 bridge/plugin_bridge.py:29-34, 90-103）
- 场景：auth.json 里存着 queue token，任何以该用户运行的进程都能读写；插件把它当作用户自己的话提交（asUser 会去掉插件框架）。模型通过 Bash 读取 token、写 queue/N.json 后，下一轮会把这段文本读成“用户自己说的”，从而在指令/授权层面冒充用户。代码注释只讨论了“同用户进程本就能做任何事”，没有覆盖模型自注入这条链。
- 建议：token 放到模型不可读的位置或改用只允许插件连接的 loopback socket + 一次性 nonce；asUser 提交内容带不可伪造的来源标记；对 Bash 写入 bridge 目录做检测/限权。
- 置信度：likely

### 低（11）

**所有 bridge 文件都是整文件 read-modify-write，没有原子替换；实机 plugin.log 已出现截断 + 0x00 的 torn write**

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 42-57, 103-109, 158-177, 300-305
- 场景：note()/flush()/writeTools()/cursor 写都是整文件重写，没有 temp+rename（$.fs 也没有 rename）。实机 plugin.log 偏移约 2168 处有 'turn done turnId=952b80f8-…-e45254ae6' 被截断成 33 字符后跟 108 个 0x00 再接着后续内容——热重载/worker 替换时进行中的写被打断是真实发生的。日志只是诊断损失，但同样的写法用在 cursor.txt 上被截断会回退游标（触发上面的重复提交），outbox/tools 被截断会变成非法 JSON 被 connector 当成 None/[]（工具卡丢失直到历史同步）。
- 建议：$.fs 无 rename，做不到真原子；至少：(1) 读 cursor 时严格校验 /^\d+$/，不合法就用备份文件/取最大值；(2) cursor 双写取 max；(3) 重要文件内容带长度或校验和，读时校验；(4) 长文件（tools.json/plugin.log）改成追加式，读最后一条有效记录，避免整文件重写。

**turn.complete 的审计行用 outTurn 而不是 e.turnId**

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 522-526
- 场景：注释说“每个完成的回合留一行可对账的记录”，但写的是模块变量 outTurn：reload 后为空、子代理步骤后是子代理的 id、无 step 的回合是上一个回合的 id。实机 plugin.log 已有 'turn done turnId= chunks=0 chars=0 head=' 这样的行，对账时无法定位回合。
- 建议：用 e.turnId（并带 e.agentId、e.reason）；需要正文时用 e.answer 而不是 outText。

**审批端点与队列 token 都信任同目录下可改写的文件：同用户进程可自动放行工具/冒充用户**

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 114-122, 275-292, 448-463
- 场景：同用户的任意进程（被入侵的编辑器扩展、脚本、另一个 agent）改写 approval.json 指向自己的端口并回 {"decision":"allow"}，插件就会对 PermissionRequest 返回 allow（形状修好之后），绕过人工审批；读 auth.json 后还能往 queue 塞一条带 token 的消息，以 {asUser:true} 的身份对模型说话。connector 的注释承认 token 不是对抗恶意本机进程的边界，但插件侧也没有额外校验，一旦这套东西常驻，任何能写用户目录的进程就拿到了一个审批后门和“用户的话”。
- 建议：明确并记录威胁模型；至少校验 approval.json 的所有者/不是符号链接（$.fs.stat resolve），或让 connector 与插件共享每会话 secret 且不落在同一可写目录；接受 same-user 风险时就不要把手机审批宣传成安全边界，并在手机卡片上注明“本机进程可代答”。

**void 掉的 promise 没有 catch，tick/flush/ui.open 的拒绝会变成 unhandled rejection**

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 338, 351, 377-381, 492, 519
- 场景：tick 里第二条 `await update($, feed, …)`（333）若 reject（host 忙、state 拒绝），tick 返回的 promise reject 而 `void tick()` 没人接；同理 `void flush($)`、`void $.ui.open(...)`、按钮里的 `void sendPaneDecision(...)`。在 hooks worker 中 unhandled rejection 可能被引擎当成模块/钩子故障处理，流式期间会表现为插件被跳过、后续 chunk 不再上报。
- 建议：`void tick().catch(err => note($, 'tick failed: ' + err))`，flush 的调用点统一 `.catch(...)`；ui.open 至少忽略但记录原因。
- 置信度：likely

**submit 接受纯空白消息，插件 trim 后丢弃却推进 cursor，消息静默消失**

- 位置：`connector/runtimes/claude/bridge/plugin_bridge.py` : 182
- 场景：手机端发来 "   " 或 "\n\n"（误触、语音转写失败、粘贴空行）。connector 的 `if not text` 放行，写进队列并进入 _start_bridge_turn（时间线上先出现一条 user 气泡）；插件 pollInbox 里 `if (accepted && text.trim())` 为假，不 submit，但仍然 cursor=item.n 写回。结果：消息既不执行也不报错，connector 空等 90s 后置 idle。
- 建议：submit 用 `if not text.strip(): return None`（由调用方明确告知“内容为空”而不是走 bridge），或在 payload 里回写被丢弃的 id 让 connector 撤掉气泡并报错。

**heartbeat_age 把“不存在/损坏/半写”一律当 None，available() 会抖动并把消息切到终端路径**

- 位置：`connector/runtimes/claude/bridge/plugin_bridge.py` : 134-150
- 场景：插件用 $.fs.write 直接覆盖 heartbeat.txt（先截断再写）。connector 恰好在截断后、写入前读文件得到空串 → float('') ValueError → 返回 None → available() 为 False，actions.py 于是把这条消息改走 terminal_attach 注入；反之刚启动时文件存在但内容为空也会误判。会话中偶发地“这一条走终端、下一条走插件”，两条路径的时间线形态不同，用户会看到消息位置/流式效果突然变化。
- 建议：区分 OSError 与 ValueError：前者视为“无心跳”，后者重试一次再判；插件侧心跳改成 temp+os.replace 原子写（与 queue 一致），并把 pid 一并写入以便 connector 校验进程是否还活着。

**审批请求体 256KB 上限让大参数工具静默退回终端审批；完整 tool_input 无界塞进 notice**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/bridge/approval_server.py` : 26、133；另 domain/approvals.py:63
- 场景：插件把完整 e.tool_input（含 Write 的 content、大 diff、MCP 参数）POST 过来；_read_request 对 content-length > 256KB 直接返回 None → 400，插件解析不到 allow/deny → next(e)，手机端永远不会出现这次审批（用户以为链路坏了）。反过来，<=256KB 的输入会被 approval_notice 原样塞进 context.toolInput 发给 server/手机，大 diff 让 notice 推送变慢或被限制。
- 建议：插件发送前截断/摘要 tool_input（保留 command/file_path/diff 头等），连接器按同一规则再裁；body 超限时返回结构化 {decision:'ask'} 而不是 400；完整输入走临时文件引用。
- 置信度：likely

**pane 的 cancel 请求可能早于 remember 注册，被当成 ignored 丢掉，手机端审批卡继续挂着**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/runtime.py` : 217-234
- 场景：request_tool_approval 先 await notice_upsert（一次到 server 的 RPC）再同步调用 on_notice=remember（interactions.py:195-197）。插件那边按钮在 ask fetch 发出后立刻显示（register.tsx:440），用户可以在 RPC 往返期间点“同意”；sendPaneDecision 以 action:'cancel' 进来时 _bridge_approvals 还没有 toolUseId → _cancel_bridge_approval 返回 {decision:'ignored'}，pane 的这次点击丢失，ask 仍挂着等手机，直到 30s 超时或手机再点。
- 建议：在发起 notice_upsert 之前就登记 pending（先 remember(notice_id) 再 await 发布），或让 cancel 也能通过 turnId+toolName 查找；未知时返回可重试状态而不是 ignored。
- 置信度：likely

**架构测试可用普通 import 语法绕过：from connector import runtime 和相对 import 都不会命中禁用列表**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/tests/test_connector_architecture.py` : 77-85、94-105、108-118
- 场景：_imported_modules 只记录 node.module。`from connector import runtime` 记录的是 'connector'，永远不会和 FORBIDDEN_ACTIVE_IMPORTS 里的 'connector.runtime' 比较；`from .. import claude` 的 module 是 None（level>0），直接跳过。因此已废弃的本地 runtime/adapter 可以零告警地被重新引入，测试给出虚假安全感。另外 token 扫描是源码子串匹配：注释/日志里出现 'approval.requested' 会误报，字符串拼接又能绕过；也没有测试约束新 bridge 层的边界（plugin_bridge/approval_server 被谁引用、actions 直接做文件 I/O 等）。
- 建议：用 ruff 的 flake8-tidy-imports（TID banned-api）或 AST 展开 from-import（对每个 alias 组装 module + '.' + alias.name，并按 node.level 解析相对导入）；token 检查基于 AST 字面量/属性访问而不是源码子串；补 bridge 边界测试。

**pane 的回合状态不是 atom，UI 不会随回合状态刷新**

- 位置：`C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane/hooks/register.tsx` : 363-391, 330-331
- 场景：`turn: {outStatus}` 读的是模块变量，渲染只在 atom 变化时触发；tick 每 400ms 用同样的字符串 update(feed)（'queued files: N'），多数实现会跳过无变化更新，于是 pane 长期显示旧状态/空的 turn，用户无法从 pane 判断回合是否在跑。
- 建议：把 outStatus 做成 atom（随 flush 一起 update），feed 带上时间/递增计数保证变化，或显式调用 $.ui.invalidate 触发重绘。
- 置信度：likely

**approval server 请求读取没有超时和头部上限**

- 位置：`D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude/bridge/approval_server.py` : 116-140
- 场景：本机任意进程连上 127.0.0.1 的 ephemeral port 后不发完整请求（或发无限 header），协程和 socket 会一直挂着；反复连接可耗尽 fd/任务。虽然只绑 loopback，但同用户进程（包括被 prompt 注入的模型 Bash）可以刷。
- 建议：用 asyncio.wait_for 包裹 _read_request（例如 10s），限制 header 行数与总字节；空闲连接超时关闭。

---

## 三、功能缺口

审计对照了 337 项清单的 result.features/result.gaps，重点是桥路线新增部分。路径约定：插件根 P=C:/Users/25640/.claude/dev-mods/cc9c02fa-3212-43e8-a554-80b45ec345f0/phone-pane（hooks/register.tsx、types/claude-code/index.d.ts），connector 根 C=D:/Develop/PyCharmProject/Agents-Anywhere-main/connector/connector/runtimes/claude（bridge/*、turns/actions.py、turns/interactions.py、runtime.py、watch/service.py、background.py）。

结论：文字消息、token 流、工具卡、审批、中断已经能工作；但整套桥是按「单终端、单会话、单轮」写死的——桥文件和心跳没有 sessionId，审批处理不按会话路由。插件也只用了引擎 API 的一小部分：turn.complete 的 usage/duration/reason、session.measure、$.session.id/version、$.command.run、$.session.compact、PermissionRequest 的 permission_suggestions 全都没用。因此真正缺的不是再写一条链路，而是：(1) 每会话隔离/路由；(2) 回合结果、错误、会话身份的可观测；(3) 非文本/非审批交互（图片、@、AskUserQuestion、plan、rewind）；(4) 投递/重连可靠性；(5) 版本兼容、灰度、回滚、指标等运维面。多数修复落在 plugin+connector；服务端 TimelineType 已有 system/marker 且 content 是 Any，主要补持久化与转发，客户端补渲染。原生 image block 和 AskUserQuestion 答案注入受当前插件 API 限制，需要验证，或接受「文件路径 + 工具拦截」的降级方案。

### 优先顺序

1. 1. 每会话隔离与路由（session:concurrency-isolation）：桥文件/心跳/审批全部没有 sessionId，多开终端会串消息、串审批、串中断；这是多会话并行的地基，必须先做。
2. 2. 回合结果可观测（usage/context/cost/耗时 + error/refusal/停滞）：turn.complete 和 session.measure 已经提供数据，插件只需写入 outbox，改动小、用户立刻可见。
3. 3. 桥路线附件/图片：去掉 not attachments 门禁，复用现成的 materialize + 文件注记，避免手机发图时另起 SDK 进程、终端看不到。
4. 4. @ 文件引用：prompt.submit 不展开 @，插件用 $.fs 解析并内联，否则手机上 @ 文件完全无效。
5. 5. 会话控制面同步（模型/effort/权限模式 + /clear、/resume、slash 命令）：当前 selections 只改 connector 内存，手机显示与实际执行不一致。
6. 6. 审批本地回退 + 超时 + always-allow：窄终端 pane 不显示、手机离线时回合会无限挂起，permission_suggestions/updatedPermissions 被丢弃。
7. 7. AskUserQuestion + plan 审批转发：终端对话框阻塞回合，手机连问题文本和计划内容都看不到。
8. 8. 消息排队/steer + 可靠投递（ack/去重/重试/queue 裁剪/重启对账）：忙碌时 409、提交失败仍前进、token 轮换丢消息、cursor 坏写重放历史。
9. 9. 压缩与上下文提示 + /compact：hook PreCompact/PostCompact + session.measure，手机才能看到压缩边界和上下文将满提示。
10. 10. 版本/能力握手 + 灰度/回滚 + 可观测性：避免 Claude Code 升级后静默失效，并让插件在 connector 关闭时真正停掉、可观测。

### 明细

- **多会话并行隔离与按会话路由（session:concurrency-isolation、integrations:remote-control-live-sync）**（plugin（用 $.session.id() 建每会话子目录，所有文件写 sessionId）+ connector（按 external_session_id 路由；心跳、abort、outbox、审批全部带 sessionId），large）
  - 为什么：桥的全部状态写死在共享目录：hooks/register.tsx:6-16 里 queue/cursor/outbox/heartbeat/abort/tools 各只有一份，plugin_bridge.py:54-87 也只有全局一份。两个终端都加载插件时：(a) 共用一个 cursor，两个插件会把同一条 queue 消息各提交一次（register.tsx:241-310）；(b) outbox/tools/abort 互相覆盖；(c) available() 只检查任意一个 heartbeat（plugin_bridge.py:134-150），A 会话的消息会被投进 B 的终端；终端关闭后 30s 内 heartbeat 仍新鲜，消息会滞留在 queue 里，被之后打开的任意会话消费；(d) 审批处理忽略 payload 的 turnId，直接挑第一个 plugin_bridge 会话（runtime.py:208-215），审批卡会挂到错的会话。触发：用户同时开两个 Claude 终端（本机常见），从手机给 A 发消息或批准权限。
- **回合结果与状态可观测：usage/context/cost/耗时、错误/拒绝/中止、停滞检测（transcript:usage-tokens、transcript:cost-state、tui:context-usage、tui:cmd-usage-cost、transcript:api-errors）**（plugin（outbox 增加 status=error/aborted、reason/refusal/usage/durationMs；hook session.measure）+ connector（outbox.ts/心跳停滞 30-60s 判定、错误 notice、usage system item）+ server（TimelineItem content 已是 Any，可直接存）+ client（token/费用/上下文 chip），small）
  - 为什么：引擎已经给了数据：turn.complete 带 usage/durationMs/reason/refusal/isAborted（types/claude-code/index.d.ts:13284-13323），session.measure 事件带 context/rateLimits/cost（11103-11129）；但插件只写 status='done' 和文本（register.tsx:515-527），outbox 没有这些字段（plugin_bridge.py:214-255）。结果：手机看不到 token 占用/费用/耗时，模型 refusal 或 API error 只表现为空回复。另一个触发：flush 的 writing 守卫（register.tsx:158-177）会让 turn.complete 的 done 快照在恰好有在途写时被丢掉；connector 只在 status=='done' 时收尾（plugin_bridge.py:229-230），且一旦 follows 就没有停滞检测（actions.py:376-425），只能等 BRIDGE_TURN_TIMEOUT_SECONDS=3600（actions.py:33）。插件在 turn 中崩溃/被卸载时同样停在这里。
- **附件与图片（tui:image-paste、tui:drag-drop-files、transcript:block-image-document、integrations:remote-control-attachments）**（connector（复用 materialize_claude_attachments + content_with_attachment_notes，去掉桥分支的 not attachments 门禁）+ plugin（未���支持 bytes 前读文件路径）+ client（多图预览/删除），small）
  - 为什么：桥分支和终端分支都用 not attachments 把附件排除（actions.py:104-109、135-144），于是手机发图时会绕过活终端，另起一个 SDK 进程（通常 resume 同一 external_session_id）来跑同一会话：终端看不到、两个进程写同一 transcript；没有终端时则走 SDK。触发：用户在终端开着某会话时从手机发一张截图。connector 其实已有现成的下载/落盘/文本注记（attachments.py:31-110、lifecycle.py:359-365），只要在桥分支 materialize 后把 Attached files: <path> 随 content 交给 plugin_bridge.submit 即可。原生 image block 走不通：prompt.submit 的 attachments 只有 type/mediaType/filename 描述（types/claude-code/index.d.ts:9036-9053），并明确不展开粘贴图片（9027-9028）；文件路径让模型 Read 是最接近桌面的替代。
- **@ 文件引用与补全（tui:at-file-reference）**（plugin（解析 @path，$.fs.read/stat 内联，或通过另一个 prompt.submit hook 注入 context）+ connector（按 cwd 建文件索引/补全 RPC）+ client（@ 选择器），medium）
  - 为什么：prompt.submit 文档明确：插件提交的 prompt 不展开 @file mentions（types/claude-code/index.d.ts:9027-9028）。手机发出的 @src/a.ts 只是普通文字，模型拿不到文件内容；插件没有用 $.fs 做解析，connector 也没有文件索引/补全 RPC（服务端只有 readDir/readText）。触发：用户从手机 @ 一个文件并发送，期望和桌面一样把文件内容带进上下文。
- **AskUserQuestion 与 plan 模式交互（permissions:ask-user-question、tui:plan-mode-approval、permissions:plan-mode-and-approval、tools:plan-mode）**（plugin（至少转发 questions/plan 内容并展示；答案注入需验证，或拦截 tool.call 自行返回工具结果）+ connector（notice kind=question/plan，承载选项）+ client（选项/多选/计划 diff），large）
  - 为什么：插件只挂 classic.PermissionRequest（register.tsx:431-479），没有 ui.render AskUserQuestion；AskUserQuestion 是工具对话框，通常不经过权限请求，回合会一直等终端里的人回答，手机只看到一张运行中的工具卡；而且 toolInputOf 是白名单（register.tsx:66-89），不含 questions/options，连问题文本都没有。即使某些版本把它当权限请求，也只能 allow/deny，无法提交选项/多选/Other。Plan 模式同样：ExitPlanMode 只有通用 allow/deny，没有计划文件内容、没有 auto-accept edits/继续规划等选项；plan_mode 附件（types/claude-code/index.d.ts:8141-8165）也不转发。把答案写回 AskUserQuestion 的通道在当前插件 API 里没有文档化入口（未验证是否有隐藏路径）。
- **会话控制面同步：模型/effort/权限模式、/clear、/resume、slash 命令执行（config:cmd-model、tui:model-effort-indicator、permissions:mode-switching、session:clear、session:resume、tools:custom-slash-commands）**（plugin（$.session.model() 读实际值；用 config.set 或 command.run 下发 model；permission_mode/effort 从 classic hook 的 e.permission_mode/e.effort 回传；每回合写 sessionId；命令走 plugin API）+ connector（意图下发、实际状态回写、id 变化重绑）+ server（更新会话绑定），medium）
  - 为什么：_start_bridge_turn 只把 selections 记进 session.selections（actions.py:314），没有任何代码把 model/effort/permission mode 送进终端进程；update_session_selections 也只改 connector 内存（selections.py:26-61）。触发：手机切到 Opus，手机状态显示 Opus，但插件提交的回合仍由终端当前模型执行；用户在终端 Shift+Tab 换模式，手机仍显示旧模式。同类问题：插件从不写 $.session.id()（types/claude-code/index.d.ts:2800-2803），outbox/heartbeat 无 sessionId，终端 /clear 或 /resume 后 connector 仍按旧 external_session_id 收尾和同步（actions.py:501-506），新会话的回合可能不显示或写错会话。slash 命令现在只能 runtime.execute_command 注入终端（runtime.py:506-516），插件有 $.command.list/run 和 $.session.compact 却没用，命令结果/状态变化没有结构化回传。
  - 依赖：版本/能力握手（探测 config/command API 是否可用）
- **审批可靠性：本地回退、超时、always-allow/updatedPermissions（permissions:approval-dialog、permissions:persist-allow-rule、permissions:sync-wait-timeout-notification）**（plugin（转发 permission_suggestions、支持 updatedPermissions/updatedInput；pane 不可见时与原生对话框竞速而不是吞掉；尽量对齐 tool_use_id）+ connector（approve_for_session、超时 default-deny、notice/推送、决策审计），medium）
  - 为什么：classic.PermissionRequest 把请求 POST 给 connector 后一直等 HTTP；只有请求失败才 next(e)（register.tsx:431-479）。pane 是 unasked 打开的，按 API 规则 <144 列不显示，而 connector 的 request_tool_approval 对 future 没有超时（interactions.py:208-211）。触发：手机离线/不答，且终端宽度不足 144 列 → 终端用户既看不到原生权限框（被插件接管）也点不到 pane 按钮，回合无限挂起。另 PermissionRequest 自带 permission_suggestions，但 hook 输入没有 tool_use_id（types/claude-code/index.d.ts:7515-7521，插件在 register.tsx:436-438 用 tool+时间戳伪造），审批卡无法与工具卡对齐；插件也完全忽略 permission_suggestions，返回 allow 时没有 updatedPermissions/updatedInput，所以桥路径永远没有「不再询问」、不能改命令；approval notice 也只有 approve/reject（approvals.py:48-51）。
- **消息排队/steer 与可靠投递、断线重启恢复（tui:message-queue、sdk:queued-messages、transcript:queue-operation、integrations:remote-control-live-sync）**（connector（队列模型、ack/裁剪/保留窗口、启动重签 token、超时重试、重启时读 outbox/tools 对账）+ plugin（按 id 去重、原子推进 cursor、outbox 加单调序列或 append-only 事件以便重放）+ client（排队中/已发送/失败状态），large）
  - 为什么：忙碌时 start_turn 直接返回 claude_turn_already_running（actions.py:93-99），没有排队/steer；游标即使提交失败也前进（register.tsx:295-305），消息静默丢失；submit 写入的 message id 插件从不读取（plugin_bridge.py:190-195 / register.tsx:271-280），无法去重；connector 重启换 token 后，队列里旧 token 的消息被拒绝并跳过（register.tsx:275-277）。queue 文件永不删除，cursor 是就地覆盖写（register.tsx:302）且读失败回退 0（246-249）——一次断电/坏写会把全部历史消息重新当用户输入提交。桥回合只存在内存里（actions.py:358-432），connector 中途重启后没有 execution 可挂，outbox 停在 streaming，手机停在 running，直到周期历史同步补最终答案；token 级流无法恢复。
  - 依赖：多会话隔离（目录/token 设计）
- **压缩与上下文提示、/compact 控制（tui:cmd-compact-autocompact、session:compact-manual、session:compact-auto、transcript:compact-boundary、transcript:compact-summary）**（plugin（PreCompact/PostCompact + session.measure + 提供 compact 命令）+ connector（发 marker/system item）+ server（TimelineType 已有 marker）+ client（上下文将满横幅、压缩摘要卡），medium）
  - 为什么：插件没有 hook classic.PreCompact/PostCompact，也没有 hook session.measure，outbox 里没有 compact 边界或 context 百分比（register.tsx:18-36、158-177）。手机既看不到压缩发生/摘要，也不会在上下文将满时被提示；用户想 /compact 只能由 runtime.execute_command 注入终端（runtime.py:506-516），输出靠 15s transcript watcher（553-575）；插件本身有 $.session.compact()（types/claude-code/index.d.ts:2871-2882）可以直接执行。
  - 依赖：回合结果可观测（usage/context）
- **版本/能力握手、灰度、回滚、可观测性与队列生命周期（运行保障，无对应 feature id）**（plugin（session.start 写 hello/状态文件：plugin/protocol/engine version + capabilities + sessionId；所有 hook 先查开关；pane 仅在启用时开；日志带 ids）+ connector（版本校验与降级、per-session allowlist/灰度、指标、queue 裁剪、降低轮询），medium）
  - 为什么：queue/outbox/tools/heartbeat 都没有 protocol/engine/plugin 版本字段（plugin_bridge.py:164-255、register.tsx:18-36），插件也不上报 $.session.version()（types/claude-code/index.d.ts:2854-2870）。Claude Code 升级后 hook 名/字段一变，插件可能照常加载、heartbeat 照常新鲜，connector 继续走桥路径，但流式/审批静默失效（例如 turn.step 的 chunk kind 改名后 outText 永远为空）。插件没有 kill switch：即使 connector 端关掉 AGENT_CONNECTOR_CLAUDE_PLUGIN_BRIDGE，插件仍会在每个 session.start 开 pane（register.tsx:351）、每 400ms 轮询（339）、每次 tool.call 写 tools.json（402-421），无法干净回滚，也不能按会话灰度。可观测性也缺：plugin.log 行没有时间戳/sessionId/turnId（42-57），没有队列深度、提交延迟、停滞、解析失败指标；queue 永不裁剪，_next_seq 每次 submit 全目录扫描（plugin_bridge.py:153-161），outbox 每 120ms 全量重写最多 20KB、tools.json 每个 tool 事件全量重写，长期运行 IO 线性增长。
  - 依赖：多会话隔离（ids）
- **流式内容完整性：thinking、长回答、工具结构化结果、MCP 参数（transcript:block-thinking、mcp:output-limits、transcript:persisted-tool-output）**（plugin（转发 thinking、完整 result/分文件存大输出、通用 input）+ connector（reasoning/artifact/完整结果投影）+ client（thinking 折叠、大输出查看器），medium）
  - 为什么：turn.step 只保留 kind==='text'（register.tsx:496-508），thinking chunk 丢弃；outText 只留最后 20k（168），长回答在流式过程中会从头消失；toolInputOf 是白名单（66-89），MCP/自定义工具参数、AskUserQuestion 的 questions 全部丢失；工具结果只存 ran.text 的前 2000 字（417），结构化 result（Edit diff、Bash stdout/stderr、图片、persistedOutputPath）都没有。触发：长代码回答、MCP 工具调用、需要看 thinking 或大输出的场景。最终答案通常能靠 turn 结束后的历史同步补齐，但流式期间手机看到的是残缺内容。
- **历史回溯/rewind 与检查点（session:rewind-checkpoint、tui:cmd-rewind、transcript:file-history、sdk:file-checkpointing）**（connector（file-history/transcript 检查点与回退）+ server（接口/存储）+ client（检查点列表/文件 diff）；plugin 目前无能为力，large）
  - 为什么：插件 API 没有 checkpoint/rewind 入口（types/claude-code/index.d.ts 中搜索无 rewind），connector 也没有 file-history 读取或 /rewind RPC；桌面版的双 Esc/回退、文件恢复在手机/Web 上完全不可用。/rewind 是交互式选择器，注入终端也无法远程操作。只能从 connector 侧解析 ~/.claude/file-history 与 transcript 分支，或等引擎提供 API；这是桌面版差距最大、且当前桥完全无法承接的一块。

---

## 四、五层架构

# 五层合一架构

## 先给判断

四份设计里有三处真冲突、两处重复造、一处过度设计。合成后的立场：

- **主链路**：客户端 -> server -> connector -> 插件 -> 引擎。事件模型以「不可变事实 + 可重建投影」为准，不是「时间线项即一切」。
- **插件↔connector**：HTTP 长轮询为主、编号文件为降级。协议层「文件为主」的理由（插件不能 append/rename）恰恰是走 HTTP 的理由，不是反对 HTTP 的理由。
- **客户端**：原生双端，但顺序是 Web（参考实现）-> iOS -> Android；不做 Flutter/RN。
- **语言**：connector 换 Go；server 先用 Python 承载 v2，Go 化留触发阈值，不做双语言大爆炸。
- **流式**：ephemeral delta + durable checkpoint 双通道；移动端必须实现 deltaSeq 状态机，不能用「10Hz 全量 item」当唯一事实。

## 总图

```
==============================================================================
 L5/L4  客户端   Web(Next.js CSR)   iOS(SwiftUI)   Android(Compose)
         SQLite/IndexedDB: timeline_items, cursor, outbox, notices
         reducer: deltaSeq 连续则追加 / checkpoint 整段覆盖 / 事件按 id 去重
         1 条 WS: dashboard + 当前会话；Web 多标签用 Web Locks 选 leader 代连
                              |
                              | WSS /api/v2/stream (subscribe/ack/ping) + REST /api/v2/*
                              v
==============================================================================
 L3    server (现 FastAPI，Go 可选)   2C2G
         ticket(一次性 60s) -> subscribe(cursor)
         eventId 幂等 -> 分配 session seq -> session_events(append-only)
         投影: content.checkpoint -> TimelineItem(revision, orderSeq)
         扇出: 持久事件 + 30Hz ephemeral delta；dashboard 只发 revision
         命令路由: REST -> connector WS；审批 resolve -> connector RPC
                              |
                              | WS 下行命令 / WS+HTTP 上行事件批(ack)
                              v
==============================================================================
 L2    connector (Go, Windows 常驻, 只绑 127.0.0.1)
         registry: 扫 sessions/*/state.json；同 session 多实例选 active/standby
         本地 hub: 每实例 token + 长轮询队列 + events ingest + approval
         projector: (sessionId,turnId,step,block) -> itemId；历史 alias 合并
         outbox(SQLite): 持久事实先落盘再上行；断连时本地合成 checkpoint
         delivery: plugin(L0-L2) / 终端注入 / SDK resume(L3-L4)
                              |
                              | 回环 HTTP：poll 20s / events POST / approval 25s 分片
                              | 降级：inbox/<seq>.json + snapshot.json
                              v
==============================================================================
 L1    插件 phone-bridge (TS, 每个 Claude Code 会话一个实例)
         session.start: 实例目录 + state/heartbeat + 异步 boot（不阻塞钩子）
         poll -> $.prompt.submit；abort -> $.turn.abort
         turn.step: 每 chunk 先 yield 给 TUI，再 33ms 聚合入队；循环内零 await
         tool.call -> tool.started/ended；PermissionRequest -> 阻塞审批
         caps 探测 / agentId 过滤 / 有界内存 / 错误环形日志
                              |
                              | 引擎 hooks（同进程）
                              v
==============================================================================
 L0    Claude Code 引擎 + 本地 TUI
         turn.start -> turn.step(token...) -> tool.call -> PermissionRequest
                     -> turn.complete
==============================================================================
```

**每层只做一种转换**：

| 层 | 输入 | 输出 | 必须守住的不变式 |
|---|---|---|---|
| L1 插件 | token chunk、hook 事件 | `content.delta` / `tool.*` / `turn.*` 事件 | 先 yield 再发送；不 await 网络；`agentId` 不进主答案 |
| L2 connector | 插件事件、transcript | 带 `itemId`/`deltaSeq` 的规范事件、有序批 | 按 `(instanceId,producerSeq)` 去重；持久事实不丢；turnId 只采纳不重铸 |
| L3 server | 规范事件 | 分配 `seq` 的事实 + `TimelineItem` 投影 | `eventId` 幂等；`turn.ended` 是屏障；快照 cursor 自洽 |
| L4/L5 客户端 | 事件流 | 行模型 | cursor 只跟 `seq`；delta 缺口不猜、只等 checkpoint |

## 一条消息的完整旅程

```
t=0ms    手机/Web 输入「把 README 改一下」
         L5: 写本地 outbox(clientMessageId, causeId)，立即渲染乐观气泡
             POST /sessions/{sid}/messages    Idempotency-Key: msg_01J...
t=20     L3: 鉴权 -> 找 session 绑定的 connector -> 在线则 WS 下发
             {"kind":"prompt.submit","commandId","causeId","data":{text,clientMessageId}}
             立刻 202 {causeId}；不返回 turnId（避免给客户端一个将被替换的假 id）
t=40     L2: 读 registry 的 caps + role 选 delivery
             L0/L1 -> 写 commands/<seq>.json 或长轮询队列
             L3/L4 -> 终端注入或 SDK resume，走原有路径
t=60     L1: 长轮询立即返回 -> 按 commandId 去重 -> $.prompt.submit({text, asUser:true})
t=120    L0: 引擎 turn.start(turnId=t_9f2c)，开始出 token
t=130    L1: 每个 chunk 原样 yield 给本地 TUI（本地零延迟）
             同时 33ms 聚合 -> content.delta(turnId, step, block, text, 无 itemId)
t=170    L2: 50ms 或 256 条成批 -> 校验 token -> 按 producerSeq 去重 -> 分配 itemId/deltaSeq
             持久事实先写 SQLite outbox -> WS 上行
t=220    L3: eventId 幂等入库 -> 分配 session seq -> 广播
             持久事件按 750ms/8KB 合成 content.checkpoint；delta 以 ephemeral 直发不落库
t=250    L5: reducer: deltaSeq 连续则 append；缺口则等 checkpoint 或拉 /events
             只 patch 那一行，p95 token->像素约 250ms
...
最后     L0: turn.complete(answer)
         L1: 发最终 content.checkpoint + turn.ended(usage, outcome)
         L2: 收到 turn.ended 前先把该 turn 所有 item flush
         L3: 屏障：先 Flush 所有 ItemStream，再给 turn.ended 分配 seq
         L5: 用 answer 整段校正流式文本，状态回 idle
```

工具调用插在同一条线上：`tool.call` 的 `next(e)` 前发 `tool.started`（含截断 8KB 的 input），`next(e)` 后发 `tool.ended`（ok/isError/ms/2KB preview）。工具输出全文不进事件，由 connector 从 transcript 取或走 blob ref。

## 审批的旅程

```
L0 引擎: 要跑 Bash("rm -rf build") -> 触发 classic.PermissionRequest
L1 插件: 读 endpoint；无 connector 立即 next(e)（终端自己弹窗）
         POST /approval {nonce, toolUseId, turnId, tool, input}
         <- 同步阻塞，hook 的 10s 预算冻结；25s 无人答则同 nonce 重发
L2 connector: approvalId = apr_<hash(session,turn,toolUseId)>
         发 approval.requested(durable) + 推送 outbox
         请求挂到 map[approvalId] 的 chan，25s 返回 pending
L3 server: 存事件 -> 扇出 -> APNs/FCM
L5 手机: ApprovalDock / 锁屏通知；点「允许」
         POST /approvals/{approvalId}/resolve {resolutionId, decision}
L3 -> L2 RPC resolve -> close(chan)
L2: 发 approval.resolved(durable)
L1: 收到 {decision:"allow"} -> return {behavior:"allow"} -> 引擎执行工具
终端 pane 同时有按钮：POST /approval/cancel，同一 nonce，先到先得、幂等
超时（默认 300s）：L2 返回 {decision:"ask"} -> L1 next(e) -> 终端弹窗接手。
绝不自动 allow，也绝不返回 updatedPermissions。
```

## 断线与恢复：三个互不替代的游标

```
客户端 cursor      = server 分配的 seq
connector outbox   = 最后 ack 的 (producerId, producerSeq) 连续前缀
插件               = command-ack seq + 最近 64 个 commandId + state 快照

客户端重连: 开 WS -> session.subscribed(cursor)
            若 cursor < minRetainedSeq 或 > head -> GET /snapshot
            否则 GET /events?after=cursor 分页补齐
            期间 WS 帧先入 buffer，恢复完按 seq 排空
connector: handshake -> 从 outbox 最小未 ack 重发；断连期间每 item 1s 本地合成 checkpoint
插件:       重读 endpoint.json -> hello -> 重发 state/snapshot -> 从 ack+1 续写命令
```

## 标识与序号：谁铸造什么

| 标识 | 铸造方 | 作用域 | 用途 |
|---|---|---|---|
| `sessionId` | server | 全局 | 客户端唯一会话键 |
| `externalSessionId` | 引擎（transcript UUID） | 单 runtime | 插件实例目录、connector 绑定 |
| `turnId` | **发起回合的一侧**（插件路径=引擎，直连=connector） | 单 session | 所有 turn/tool/approval 的归属；中间层只采纳 |
| `causeId` | 客户端 | 一次命令 | 24h 幂等窗口 |
| `clientMessageId` | 客户端 | 一条消息 | 乐观 UI 与落库消息的 join key |
| `itemId` | **connector 投影器** | 单 session | 投影唯一键；插件只给 itemRef |
| `approvalId` | **connector** | 单 turn | 审批生命周期与审计 |
| `resolutionId` | 客户端 | 一次审批动作 | 多端幂等 |
| `eventId` | 事实的第一个生产者 | 全局 | 服务端入库幂等 |
| `producerSeq` | 各生产者 | 实例内 | 上行 ack、重放 |
| `seq` | **server** | 单 session | 客户端唯一 cursor 与排序依据 |
| `revision` | server | 单 item | 投影整段替换 |
| `deltaSeq` | **connector**（按 item） | 一个 live 流 | 增量幂等追加，不是 cursor |

关键修正：插件不铸造 `deltaSeq`，只给自己的 `producerSeq`；connector 单写者按序翻译，避免两套增量编号互相打架。connector 重启时 `deltaSeq` 归零，强制发一次新 `revision` 的 checkpoint，客户端以 revision 跳变为流重置信号。

## 降级矩阵（插件缺席也必须是好产品）

```
L0 full:      全 caps -> token 流 + 工具卡 + 审批 + 中断
L1 no-stream: turn.step 注册失败 -> 手机显示「思考中」，turn.ended.answer 一次性落全文
L2 no-turn:   无 turn 钩子 -> 只提交；时间线由 connector 的 transcript tailer 生成
L3 presence:  无 $.prompt.submit -> 只报状态；connector 回落终端注入 / SDK resume
L4 absent:    无心跳 -> 完全走旧路径，客户端看到普通时间线
```

| 谁挂了 | 本地用户 | 上层看到什么 |
|---|---|---|
| 插件 | 会话继续，pane 显示错误码 | 30s 心跳 TTL 后 offline；未完成 turn 标 interrupted（以 transcript 为准） |
| connector | 插件 30s 后进 file 模式 | 客户端标 connector offline；恢复后 outbox 重放 |
| server | connector 本地继续存 | 客户端读 SQLite 缓存，顶部条「重连中」 |
| 客户端离线 | 草稿保留 | 不自动重发；恢复后按 cursor 对账 |

## 架构上的三个硬约束（照抄会死）

1. `turn.step` 的 `for await` 循环里绝不能 `await` 网络或状态写——直接拖慢本地 TUI 的逐 token 显示。所有发送走后台队列。
2. 任何一跳都可以丢增量，任何一跳都不能静默丢持久事实（turn/tool/approval/error）。丢了必须变成可见错误或死信。
3. 插件侧不能原子写、不能删文件；所以 HTTP 为主、文件为降级，GC 全部归 connector。

---

## 五、必须拍板的设计决策

- 1. 序列化格式：JSON + JSON Schema，不用 protobuf/gRPC。【推荐】五端语言多、浏览器要能直接调试、移动端不想背运行时；真正吃带宽的是流式正文，用 30Hz 小 delta（每条几十字节）解决，比换二进制协议收益大一个数量级。代价：工具大输出要多一次 blob 下载；用 outputRef 兜。
- 2. 协议来源：JSON Schema 是唯一真源，生成 Go/Python/TS 类型；不要像协议层说的那样「Go struct 为源」。【推荐】迁移期 server 还是 Python，Go 不可能当唯一源；Schema 为源才能让五端 CI 用同一批 fixtures。Go 化后再把生成方向反过来，是机械替换。
- 3. 协议版本策略：在现有 protocol 1.0 上做「1.0 + 增量」而不是推倒重来。【推荐】现有 `contracts/protocol/1.0` 的 envelope/cursor/handshake/ticket/recovery 已经能用，缺的是 turn/approval 一等事件、生产者铸造 eventId、ephemeral delta。保留 envelope，新增事件 kind，服务端双投影一个版本窗口。代价：比全新 v2 少一点整洁，但少两周返工和一次五端同时断代的窗口。
- 4. 事件模型：一等事实 + 投影，放弃「时间线项即一切」。【推荐】`turn.started/ended`、`approval.requested/resolved`、`error.raised` 必须是一等持久事件；`TimelineItem` 只做可重建投影。理由：现在 turn 结束只体现在 `latest_turn_end_seq`（未读计数），审批只靠 notice 的 upsert，恢复路径根本覆盖不到。代价：多一张事件表和写入放大，用 750ms checkpoint + 分区控制。
- 5. 插件↔connector 传输：HTTP 长轮询为主（20s），文件只做引导与降级。【推荐，明确推翻协议层的「文件为主」】插件层实测 `$.http.fetch` 可用且有 30s 硬超时，长轮询 20s 留足余量；HTTP 把提交延迟从 400ms 降到 100ms 内，空闲 IO 从每会话每秒 2.5 次目录扫描降到每 20s 一次请求。文件为主会制造大量小文件、Windows fsnotify 丢事件、插件又不能删文件，正好是最差组合。代价：connector 必须常驻并监听回环端口（本来就要）。
- 6. 文件降级协议：只用两种文件，不要两套编号体系。【推荐】下行 `inbox/<seq>.json`；上行 `snapshot.json`（250ms，尾部 20KB）+ `state.json` + `heartbeat.json` + 仅持久事实的 `journal/<seq>.json`。增量永不落文件。理由：降级态本来就该最省事；每 33ms 一个编号文件会在系统最脆弱时制造目录风暴。
- 7. 手机技术栈：原生双端（SwiftUI + Compose），不做 Flutter/RN；条件是先交 iOS、Android 二期。【推荐】这个产品的难点是流式长文本、长会话滚动、锁屏审批、后台生命周期，恰好是跨平台框架最弱、原生最强的地方；现有 iOS 的 `MarkdownStreamSplit` 和 Android 的 34ms 合批是有价值资产。只有 1-2 人时先做一端，另一端按同一份 reducer spec 移植，不要为了「双端同时」选 Flutter。
- 8. 服务端/connector 语言：connector 现在换 Go；server 先留 Python，Go 化设阈值。【推荐】connector 换 Go 收益确定：Windows 单静态二进制、无 Python 运行时、低空闲 CPU、文件监听和本地 HTTP 都是一等公民。server 在 2C2G 上用 asyncio 扛几百条 WS 和 30Hz 小 delta 是够的；同时重写两种语言再改五个客户端是最大风险源。触发阈值：p95 ingest CPU > 60%，或稳定 WS > 800 条，或事件写入成为瓶颈，再迁 ingest/fanout 两个模块。
- 9. turnId 归属：发起回合的一侧铸造，connector 不再 mint `turn_claude_*`。【推荐】插件路径直接用引擎 `e.turnId`；直连路径才由 connector 铸。`turn_claude_*` 降级为 `nativeTurnId` 只进日志。理由：两套 turn id 没有 join key，审批、工具、历史都对不上。代价：connector 的 active-run 状态机要改成 causeId 先占位、`turn.started` 后补 turnId，迁移期双读写。
- 10. 审批载体：`approval.requested/resolved` 一等事件；notice 只留 input_request 和普通通知。【推荐】审批需要独立的 expiresAt、多端幂等、resolutionId、审计；塞进 notice 的 upsert 状态机永远做不干净。移动端 ApprovalDock 改为读 approval 流，Web 同理。代价：服务端要保留一个版本窗口的 notice 投影给旧客户端。
- 11. 审批超时语义：超时回落终端，绝不自动 allow/deny。【推荐】connector 300s 到期返回 `ask`，插件 `next(e)` 让终端弹窗接手；客户端显示 expired。默认拒绝会让「人不在手机边」变成工具被静默拒绝；自动允许更不可接受。
- 12. 流式通道：30Hz ephemeral delta + 750ms durable checkpoint 双通道；移动端必须实现 delta。【推荐，明确推翻移动层「只接受 10Hz 全量 item」】移动层自己算出长回答约 50KB/s，这是它最大的带宽/耗电问题。delta 每条几十字节，同一套 reducer 加 3-5 人日即可；checkpoint 始终是权威兜底。代价：两端各写一套 deltaSeq 状态机单测，缺口必须走 stream.reset/checkpoint，不能猜。
- 13. 客户端连接拓扑：一条多路复用 `/api/v2/stream`，所有端共用。【推荐】Web 多标签用 Web Locks 选 leader + BroadcastChannel 扇出；移动端只订阅当前会话，但仍用同一端点。放弃「按会话一条 WS」，否则服务端要维护两种连接语义、两套恢复逻辑。代价：单连接爆炸半径变大，必须逐 subId 隔离错误。
- 14. itemId 归属：connector 投影器唯一铸造，插件只给 `itemRef`；历史同步用 alias 合并。【推荐】这是整个设计最容易出 bug 的地方：引擎 live chunk 不带原生 message id，历史 transcript 带，两者必须能对上。先做 3 天 POC 用真实长会话验证；若不成立，退化为「回合结束后用 transcript 全量替换该 turn 的 items」。
- 15. 桌面端：抽 `packages/workbench-ui` 与 Electron renderer 共用，不另写一套。【推荐】现在两份 session-detail.tsx（2873 vs 2891 行）是纯负债。平台差异收敛到 `PlatformAdapter`（通知/文件/窗口/PiP）。Web 端补键盘、通知、Document PiP 即可获得「桌面级」体验。
- 16. Web 部署：继续静态导出 + Caddy 直服，不引入 Node SSR。【推荐】全部页面本来就是客户端壳、数据在 auth+WS 后面、没有 SEO；Node 常驻只增加 150-250MB RSS 和一个故障面。分享页 OG 用 FastAPI 返回极简 HTML 壳。
- 17. 移动端本地存储：SQLite 增量 upsert，替换 iOS 的整文件 JSON archive。【推荐】现有 `V2LocalStore` 整文件重写、最多 8 会话/128MB，Android 干脆没有时间线缓存，离线打开是空白。SQLite 的游标/事务/增量是断线恢复的前提。
- 18. 推送范围：APNs + FCM 首版做；国内 Android 明确降级为应用内提示，厂商通道二期。【推荐，这是最大的可控范围削减】国内多数设备无 GMS，接华为/小米/OPPO/vivo/荣耀或聚合服务要再加 2-4 周和合规成本，且不影响主链路可用性。锁屏审批在 iOS 和国际 Android 先闭环。
- 19. 安全边界：每实例 token + 回环 + 目录 ACL；放弃全局 `auth.json`。【推荐】全局 token 等于给同用户任意进程一条「以用户口吻无声注入 prompt」的稳定通道。每实例 token 重启轮换、只走 header、校验 Host/Origin。文档必须写明：挡不住同用户恶意进程，这是本地威胁模型的天花板。
- 20. 插件分发：marketplace + user scope 安装。【推荐】`--plugin-dir` 只用于开发（无版本/无 userConfig/无回滚）；`CLAUDE_CODE_PLUGIN_DIRS` 只用于内部灰度。项目级 scope 不采用，这是机器级基础设施。
- 21. 范围削减：首版交付 Web + iOS + 全后端，Android 与推送放二期。【推荐】370 人日的全量在 4 人团队要 20-22 周；先让 Web 当参考客户端把协议和状态机跑死，iOS 紧随，Android 复用 fixtures 跟进。若必须五层同交付，至少把 Android 的国内推送砍掉。

---

## 六、落地顺序

- 第 0 步（10 人日，1-1.5 周）契约冻结：在 `contracts/protocol/2.0` 落 JSON Schema（21 种事件）、bridge/ingest 信封、valid/invalid fixtures；Pydantic/TS/Go 生成器先跑通，Swift/Kotlin 用同一批 fixtures 做解码测试。独立验证：五端解码全绿，未知 kind/未知字段/未知枚举值都不崩；旧 1.0 客户端仍能连。此步不动任何运行时代码，可单独合入。
- 第 1 步（35 人日，2.5 周）插件 v2 + 本地 harness：每会话实例目录、`$.session.id()` 绑定与 /clear 重绑、state/heartbeat、caps 探测、agentId 过滤、33ms 聚合、审批 nonce 重试，以及一个 300 行的 Go `bridge-harness`（只实现 hello/poll/events/approval）。独立验证：同时开 6 个 Claude 窗口，harness 终端里能看到 6 路独立流；提交/中断/审批全部闭环；杀掉一个窗口不影响其余；全程不需要 server。这是全项目风险最高的一步，必须先做。
- 第 2 步（30 人日，2 周）Go connector 本地核心：把 harness 提升为正式 connector——registry（扫 state.json、active/standby）、回环 HTTP hub、每实例 token、命令 ring、事件 ingest 去重、approval map、SQLite outbox、file 降级、transcript tailer 兜底。独立验证：用同一台机器的 CLI `fake-phone`（直接订阅 connector 的本地调试流）走完「提交->流式->工具卡->审批->中断」，connector 重启后状态不丢、命令不重放。仍不需要 server。
- 第 3 步（30 人日，2 周）server 核心 + 最小 Web 观察器：事件表、seq 分配、eventId 幂等、checkpoint 合成、snapshot fence、/events 恢复、/stream 多路复用、命令路由。Web 只做一个 200 行的调试页（订阅 + 打印 + 审批按钮）。独立验证：第一条真正的端到端链路——浏览器发一句话，终端出字，浏览器逐字看到；分别 kill 插件/connector/server，每次都能恢复且无重复。
- 第 4 步（60 人日，3 周）Web 产品客户端：Zustand 归一化 store + TanStack Query + IndexedDB outbox、行级订阅、冻结前缀 Markdown、worker 高亮、DiffPanel、ApprovalDock、多标签 leader、45s 看门狗、键盘与通知。独立验证：30 分钟长回答内存平稳；强制断网 60s/后台 10min/服务端重启三种场景无丢无重；3 个标签只建 1 条 WS，审批一处点击三处消失。
- 第 5 步（75 人日，3.5 周）iOS：SQLite 迁移、RealtimeLink 状态机、订阅-缓冲-恢复、30Hz 合帧、复用 MarkdownStreamSplit、LazyVStack + 300 行窗口、ApprovalDock、deltaSeq 归约。独立验证：飞行模式 5 分钟恢复无空洞；10Hz/30Hz 流式主线程单帧 <8ms；1000 条会话滚动 60fps；与 Web 同时打开同一会话，最终 item 快照逐字节一致。
- 第 6 步（60 人日，3 周）Android：复用同一份 reducer spec 与 fixtures；Keystore 存 token、关 allowBackup、移植稳定前缀切分、LazyColumn 窗口、审批 Dock。独立验证：同一 fixtures 回放结果与 iOS 一致；长回答流式不卡顿；明文 token 清零；与 Web/iOS 三端同时观看，文本与工具卡一致。
- 第 7 步（25 人日，1.5 周）推送与设备：服务端 push outbox、设备表、refresh 轮换与撤销；APNs category/action、FCM data-only + WorkManager；幂等键绑定 notice/approval。独立验证：真机锁屏批准/拒绝生效且只结算一次；审批在另一端解决后通知自动撤销；撤销设备后旧 refresh 立即失效。
- 第 8 步（25 人日，1.5 周）降级与硬化：逐钩子注入故障、L0-L4 矩阵、有界队列与丢弃策略、逐事件 rejected + 死信、GC 与目录上限、6 会话 x 8 小时浸泡、安全自查（Origin/Host/token/ACL）。独立验证：向每个钩子注入一次抛错，TUI 不崩、会话继续、错误码可见；模拟无 `$.prompt.submit` 后自动回落终端路径；浸泡期间内存/句柄平稳、无残留 inbox 增长。
- 第 9 步（可选，35 人日，2 周）server Go 迁移：只迁 ingest/sequencer/fanout 三个热点模块，Python 与 Go 对同一批 fixtures 产出相同 eventId/seq/投影，灰度双跑。触发条件：p95 ingest CPU > 60%、稳定 WS > 800 条或事件写入成为瓶颈。不满足就不做。

---

## 七、保留与丢弃

### 值得保留

- 插件 `hooks/register.tsx` 的钩子形状，这是全项目最值钱的一段：`turn.step` 用 async generator 逐 chunk `yield`、`tool.call` 在 `next()` 前后各发一次快照、`classic.PermissionRequest` 失败一律 `next(e)`、`pollAbort` 的 primed/lastAbort 语义。产品版照这个骨架重写，不要另起思路。
- 插件侧对 `$.fs` 约束的认知：只能整文件 write、没有 append/rename/delete。这条决定了文件只能做降级、GC 只能归 connector、读端必须容忍半写 JSON。把它写进设计文档和代码注释。
- 插件 `note()` 的串行 Promise 链（读-改-写日志防并发丢行）与 20KB 尾部截断；换成环形日志时保留「单链串行」这个做法。
- connector `runtimes/claude/domain/` 下的纯领域模型（approvals、pending_messages、permissions、selections、input_requests、session）——这些是业务语义，与 Python 无关，可逐字段翻成 Go struct，是重写时最好的规格书。
- connector `server/notification_coalescer.py` 的 100ms 合并窗口与「非文本事件先 flush 再发」的屏障语义；Go 版保留语义，把窗口调到 50ms 并加批量上限。
- connector `runtimes/claude/history/tailer.py` + `history/syncer.py` + `sessions/reader.py`：这是 L2/L4 降级路径的命脉（插件缺席时时间线仍要出现），不能因为上了插件桥就删。
- connector `terminal/attach.py` 的终端注入与 `sessions/` 的会话发现：插件不可用时的第三条 delivery，必须保留并纳入 caps 路由。
- connector `core/runtime_owner.py` 的单实例锁与 `core/json_kv.py` 的持久化；`runtime_protocol/instance_binding.py` 的实例绑定模型也值得照搬。
- server `core/protocol.py` 的 `ProtocolEventEnvelope` + `cursor` 约定、`contracts/protocol/1.0` 的 schema/manifest/validate 机制。协议 2.0 是在这上面加事件，不是另建一套。
- server `TimelineWriteBuffer` 的 `session_fence` + 1s 物化：这是全仓库最核心的并发正确性资产，保证 snapshot cursor 自洽。保留，只把「3 次稳定性重试」换成 `<= head` 语义。
- server `infra/ws_tickets.py` 的一次性 60s ticket：简单、正确、够用，继续用。
- server 的 auth/pairing/mobile-login QR、`infra/repositories/sessions.py` 的绑定查询、`connector_gateway`/`connector_rpc`、`terminal_stream_hub`——这些与本次重构无关，直接复用。
- server `dashboard_stream.py` 的变更信号（`dashboard.changed`）保留，但把「每次变更重建全量快照」改成「只发 revision + ETag」。
- Web `components/session/session-event-buffer.ts`：30Hz 固定窗口 + barrier 排空 + 同 item 只留最新一版，语义完全正确，直接复用。
- Web `optimistic-timeline` 的 `clientMessageId` 对账与服务器权威覆盖规则；Web `timeline-scroll-follow` 的 follow-tail 判定和 input/textarea 排除写法。
- Web `session-tool-cards.tsx` 的 DiffPanel（unified diff 自研解析，比 Monaco 轻得多）与工具卡形态；Monaco 的懒加载只留在文件预览。
- iOS `MarkdownStreamSplit` + `ChatMarkdownView` + `TimelineDiff` + 测量缓存：稳定前缀只解析一次、开放尾部每批重解析，这是 iOS 端最该原样保留的部分。
- iOS `V2SessionRepository` 的 projection / barrier / reconcile 语义（注意只保留语义，存储换 SQLite）；`V2RealtimeAPI` 的 45s 看门狗；Keychain 的 `AfterFirstUnlockThisDeviceOnly`。
- Android `SessionRealtimeController` 的 34ms 合批、指数退避 + 抖动、ticket、recover 分页；`SessionDetailReducer` + `SessionTimelineProjection` 的不可变 state 与单条流式消息快速路径；`SessionMessages` 的 LazyColumn 稳定 key/contentType。
- Web 现有 67 个单测与 Playwright 回放脚本：状态重构前先跑一遍录成 baseline，之后每一步都用它防回归。

### 应当丢弃

- 放弃插件里的硬编码绝对路径 `C:/Users/25640/.claude/phone-bridge` 和全局单例状态：多开窗口会互相踩踏。改为 `sessionId--instanceId` 实例目录 + `$.state`/实例文件。可以放弃是因为实例目录方案已经在插件层设计里验证过，迁移成本只是路径拼接。
- 放弃插件 400ms `$.clock.every` 全目录扫描 + 每 tick `fs.list`：6 会话下是每秒 15 次 list、90 次 entry stat，收益为零。改为 20s 长轮询、空闲 1s/流式 250ms 的降级 tick。可以放弃是因为 HTTP 通道已经验证可用。
- 放弃每 120ms 整文件重写 `outbox.json` 作为主通道：CPU/IO 随回答长度线性增长，长回答直接拖慢终端。改为 33ms 增量 + 服务端 checkpoint。可以放弃是因为 `turn.complete.answer` 始终能做最终校正。
- 放弃插件每次 `session.start` 自动 `$.ui.open` 开面板：长期使用非常打扰。改为 `$.ui.status()` 常驻一行 + `/phone` 手动打开。可以放弃是因为诊断信息在状态行和 connector 诊断页都能看到。
- 放弃不过滤 `agentId` 的 `turn.step`/`turn.complete`：这是原型里的真实 bug，子代理 token 会拼进主答案、子代理 complete 会把主回合标成 done。产品版所有 turn/tool 钩子必须按 `agentId` 分流。
- 放弃全局 `auth.json` 单 token：等于给同用户任意进程一条无声注入 prompt 的稳定通道。改为每实例 token、重启轮换、只走 header。放弃是安全的，因为实现成本只是一个随机数与文件写。
- 放弃 connector 自铸 `turn_claude_*`：它和引擎 turnId 没有 join key，审批和工具无法归属。改为采纳引擎 id、`nativeTurnId` 只进日志。可以放弃是因为所有对外暴露面都还没依赖这个 id 的语义。
- 放弃 `plugin_bridge.available()` 的「有心跳就用」启发式：必须读 registry 的 caps + role，否则 L2/L3 会话会被误判为可走插件。
- 放弃 `_answer_bridge_approval` 遍历 session_store 找第一个 `plugin_bridge` execution：多会话下会问错会话，这是现存的正确性 bug。改为 nonce/approvalId 精确索引。
- 放弃 connector ingest「一条 rejected 就整批 inflight 清空」的语义：一个坏事件会丢整批持久事实。改为逐事件 accepted/rejected + 死信，可重试的放回队头。
- 放弃把 `AGENT_CONNECTOR_CLAUDE_PLUGIN_BRIDGE` 环境变量当产品开关：Windows GUI 启动的终端经常不继承环境变量。改为配置项 + 自动探测 + caps 决定，不用环境变量做消费者路径。
- 放弃 `turn.start`/`turn.end` 作为 TimelineItem 类型：服务端 `_upsert` 已经明确抛 `unsupported_timeline_marker`，iOS 却定义了这两个类型、Web 没有，三端已经漂移。改为 `turn.started/ended` 一等事件并从 TimelineType 枚举删除。
- 放弃 `eventId = evt_{sequence}_{hash}`：sequence 是服务端事后分配的，同一事实重放会得到不同 id，跨端无法幂等。改为生产者按 `producerId + producerSeq` 铸造，服务端只负责入库去重。
- 放弃「一个 sequence 挂多个事件」（session.meta 与 capability 共用 seq）：导致 eventId 不可预测、恢复要猜。改为一个持久事件一个 seq，用分区和保留期控成本。
- 放弃 `EventRecoveryService` 的 3 次稳定性重试：它是在补偿「读的时候还在写」。改为 `session_fence` 内取 head、只读 `<= head` 的行，语义清晰且更快。
- 放弃每次 dashboard 变更给每个 socket 重建全量快照：3 个标签页 x 每次变更 x 全量 inventory 是 2C2G 上最冤枉的开销。改为 WS 只发 revision + `GET /dashboard/snapshot?ifNoneMatch=`。
- 放弃把 notice 当审批唯一载体：notice 的 upsert 状态机无法表达 expiresAt、resolutionId、多端幂等和审计。notice 只留 input_request 和普通通知。
- 放弃 Web 的 2873 行 `session-detail.tsx` 与 1476 行 `workspace-context.tsx`：状态全锁在组件里，任一快照让所有消费者重渲染。拆成 controller / view / list + Zustand + TanStack Query。可以放弃是因为已有 67 个测试可以当回归网。
- 放弃 Web 在 render 里同步跑 highlight.js、放弃每个事件 `setState` 替换整个 items 数组、放弃每帧重算 `groupTimelineItems`：这三件是流式卡顿的直接原因。改为 worker 高亮 + 行级订阅 + 只重算受影响分组。
- 放弃 Web 每标签页两条 WS、固定 1.2s/2s 重连、没有心跳看门狗：半开连接会静默卡死。改为单连接多路复用 + 指数退避抖动 + 45s 看门狗。
- 放弃 desktop-workbench 里那份 web-next renderer 拷贝：两份 2800+ 行代码只会越漂越远。改为 `packages/workbench-ui` + PlatformAdapter。
- 放弃 iOS `V2LocalStore` 的整文件 JSON archive（8 会话/128MB、2.5s 全量落盘）：改为 SQLite 增量；旧文件只在首次升级时读一次并迁移。可以放弃是因为归档格式没有外部消费者。
- 放弃 Android 明文 SharedPreferences 存 access/refresh token 和 `allowBackup=true`：这是确定的安全缺陷。改为 Keystore + EncryptedSharedPreferences/DataStore，关备份或排除目录，升级时清理旧明文。
- 放弃 Android `AgentMarkdownText` 每次文本变化全量 commonmark 重解析：长回答流式必然卡。移植稳定前缀切分 + 后台解析 + AnnotatedString 分块缓存。
- 放弃 Android 固定字段的 `RemoteSessionEventPayload` data class：未知字段被静默丢弃，新增事件表现为「状态偶尔不对」而不是报错，极难排查。改为 `JsonObject` 保留未知字段 + 按 type 投影。
- 放弃首期 Flutter/RN/KMP 统一栈：会推倒两端已经做对的增量解析与事件合批，且推送/后台/文本选择仍要写原生。KMP 只作为团队缩到 1-2 人时的后续选项，且只下沉 store/游标/SQLite，不下沉 UI。
- 放弃 Node SSR 与 protobuf 两个「看起来先进」的选项：前者在 2C2G 上只增加内存和故障面，后者在五端多语言下增加构建与排障成本，而带宽问题用 delta 已经解决。可以放弃是因为两者都不解决当前任何一个真实瓶颈。
- 放弃「没有 connector 也能独立工作」的幻想：bridge 的前提就是本机常驻 connector。不在时插件静默降级、不阻塞 Claude 本体；不要为此设计第二套独立链路。

---

## 八、工作量

总量约 370 人日（合理区间 340-400），不含服务端 Go 迁移；若做 Go 化再加 35 人日，合计约 405。

按层拆分：
| 层 | 人日 | 说明 |
|---|---|---|
| 契约 + fixtures + 生成器 | 10 | 五端解码测试，一次性投入，后面每步都靠它 |
| 插件 phone-bridge | 35 | 多实例、caps、流式、审批、降级矩阵、单测 |
| connector（Go, 聊天主链） | 30 | registry/hub/channel/approval/outbox/file 降级；终端与文件本地能力可暂留 Python 边车，不计入 |
| server（Python 承载 v2） | 45 | 事件表/seq/投影/恢复/stream + push outbox/设备/refresh 轮换 |
| Web | 90 | 含调试观察器 10；状态重构、渲染、多标签、桌面体验 |
| iOS | 75 | SQLite、同步引擎、渲染、审批、多会话 |
| Android | 60 | 复用 spec/fixtures，但 UI 与安全整改是硬工作量 |
| 联调/浸泡/安全/文档 | 25 | 故障注入、6x8 小时浸泡、GC、威胁模型文档 |

日历：
- 4 人（插件+connector / server / Web / iOS->Android）：20-22 周
- 3 人：26-28 周
- 2 人：36-40 周，此时建议砍 Android，先交 Web + iOS
- 首版可演示（Web + iOS + 全后端，不含 Android/推送）：约 290 人日，4 人 15-17 周

不含：终端 PTY 双向流、文件浏览器、会话分享、管理后台、多租户计费、官方远程能力替换。若「和桌面版一样」被解释为包含终端交互，另加 15-20 人日。

最大不确定性：协议层拆出的「插件编号文件 + itemId 映射」与「服务端 checkpoint 合成 + 快照 fence」两块，各留 2-3 天专门做故障注入，不要按乐观值排期。

---

## 九、还没定的问题

- 官方远程能力路线：Claude Code 自身已有 mobile/desktop surface、session.attach 和 bridge 相关环境变量。如果官方在 6 个月内把远程控制做成熟，这条自研链的维护成本会显得不划算。需要在第 1 步之前拿到 Anthropic 的 roadmap 信号；拿不到就保持协议层薄、connector 的 delivery 可整体替换。
- `$.http.fetch` 的 30s 是不是硬上限、会不会变短？目前只有 plugin.log 里一次 `no complete answer within 30000ms` 的实测。需要在 2.1.296 和最新版各跑一组 60s 挂起实验（记录实际 abort 时间点）。若上限降到 15s 以下，长轮询要改 8s、审批分片改 10s，请求频率翻倍。
- 同一个 transcript 被两个终端 resume 时引擎是否支持？active/standby 的整个收敛逻辑建立在这个假设上。需要实测两个 `claude --resume` 同一 session，观察是否双写、是否有锁、transcript 是否交错；若不支持，就要在 connector 侧硬性拒绝第二个实例。
- `/clear` 和 resume 在不同版本里的 `session.start` 行为是否一致？设计里假设 `/clear` 不触发 session.start、需要靠 ticker 重绑。需要跨 2-3 个 CLI 版本做矩阵测试，否则重绑逻辑会在某个版本上失效。
- 审批超时的产品语义：回落终端（本设计推荐）还是自动拒绝？这需要用户拍板，因为它决定「人离开手机 5 分钟后 Claude 是继续等还是失败」。技术上两者都能实现。
- 国内 Android 推送：接厂商通道（华为/小米/OPPO/vivo/荣耀或聚合服务，+2-4 周和合规成本）还是首版明确降级为应用内提示？需要用户按实际用户分布决定；不要默认 FCM 可达。
- 产品边界：「和桌面版一样」是否包含终端 PTY 双向流、斜杠命令、checkpoint/rewind、IDE 集成、文件浏览？如果包含，需要一条独立的终端链路，不在本次五层范围内，工期要另算。
- 租户模型：这是单用户自托管，还是多租户 SaaS？是否有多人共享/旁观/接管同一会话的需求？这会直接改写 auth、权限、事件广播和推送路由，越早定越好。
- 服务端是否接受 Python 过渡？本设计强烈建议先 Python 承载 v2、Go 化设阈值；如果用户要求一次 Go 化，工期 +35 人日，且和五端重写叠加是最大风险源。
- 事件与审批的数据保留/合规：事件表保留 7 天还是 30 天？审批推送里是否允许出现命令文本、文件路径（这些会经过 APNs/FCM）？需要明确，默认锁屏只显示「有一个待审批操作」。
- itemId 在 live 与历史 transcript 之间能否稳定合并？这是全设计最容易出 bug 的地方。需要 3 天 POC：用真实长会话 + 工具调用，验证 connector 的映射表能否把 live item 与 transcript item 对上；若不行，退化为「回合结束后整 turn 替换」。
- v1 客户端兼容窗口多久？服务端要同时维护 v1 投影（notice 审批、timeline item 事件）和 v2 事件，成本不低。若所有客户端都在本次重写，建议只保留 1 个发布周期（约 6 周）的兼容。
- 插件 marketplace 的发布主体、命名与签名：是公开 marketplace 还是私有 git marketplace？这影响 CI、升级策略和 `claude plugin install` 的命令形态。
- 两个 connector 同时运行（比如桌面应用自带的 Python connector 和新的 Go connector）时，`endpoint.json`/token 的 generation 规则由谁仲裁？现有 `runtime_owner` 单实例锁在 Go 重写后是否保留同一套语义，需要确认。


---

## 十、用户决策（2026-10-24）

1. **审批不设超时**：没有审批就一直等，不自动拒绝、不回落。
2. **暂不做 Android**：先 iOS + Web。
3. **先单租户**：多租户以后再加。

### 「无超时」怎么绕过 30 秒宿主限制

`$.http.fetch` 单次请求有 30 秒硬超时，但钩子预算只在**钩子自己的代码**运行时计时，
`$` 调用在飞行中不计。所以：

```
while (true) {
  r = await fetch(/approval, {requestId, waitMs: 25000})
  if (r.decision) return r.decision
  // 超时或 pending：继续下一轮，同一个 requestId
}
```

每一轮都是一次 `$` 调用，预算被冻结；轮次之间的自有代码只有几微秒。
**逻辑上永不超时**，等价于桌面那个一直等着的对话框。

关键约束：**必须带同一个 `requestId`**，connector 端按 requestId 复用同一个
pending future，不能每轮都新建一条 notice，否则手机端会堆满重复卡片。

### 与之配套的语义

- 手机不在线：请求一直挂着，面板按钮仍然可用——桌面答也行。
- connector 重启：requestId 与 pending 状态丢失，插件下一轮拿到 `unknown`，
  重新登记（幂等，不产生第二张卡）。
- 会话被关掉：挂起的请求随进程消失，connector 侧靠心跳超时清理。


### 追加决策（2026-10-24）

4. **存储改用 SQLite**，不再用 PostgreSQL + Redis。

依据（基于现有代码检查）：
- Redis 在现有实现里只做分布式协调（`coordination="redis" if app.state.redis.distributed else "memory"`），
  单进程单实例时自动降级到内存 —— 可直接删除。
- 单用户自托管、2C2G、单机，SQLite（WAL）够用，且不需要独立服务进程。

Go 选型：`modernc.org/sqlite`（纯 Go，无 cgo，交叉编译友好），开 WAL。

必须持久化：会话、时间线、设备与 token、同步游标。
可以只在内存：心跳/在线状态、待审批、WS ticket（重启丢失可接受，插件会重新问）。

存储层要留一个窄 `Store` interface，将来多租户/多实例时换回 PostgreSQL 只需换实现。


### 追加决策（2026-10-24）

5. **该精简的地方就精简**。重写以“够用”为准，不照搬现有结构。

精简清单（在 Go 方案中执行）：

| 现状 | 精简为 | 依据 |
|---|---|---|
| 7 个 WebSocket 端点 | 2 个：connector 上行 + 一条多路复用客户端 stream | 客户端不必管 3-4 条连接；切换会话只需 subscribe |
| 按会话一条 WS（`/sessions/{id}/ws`） | 合并进多路复用 stream | 这是“一个窗口能同步、切会话要重连”的根因 |
| `terminals` 与 `terminals-v2` 两套终端流 | 一套 | 重复实现 |
| Redis | 删除 | 单进程下已有内存回退，仅用于多实例协调 |
| PostgreSQL | SQLite | 见决策 4 |
| codex / dsh / opencode 残留 | 全删 | 已完成大部分，Go 版不带 |
| 过度抽象层（只有一个实现的接口/包装） | 合并 | 重写时一并收敛 |

不精简的部分（丢了会出事）：并发正确性（时间线 fence、事件幂等）、
协议兼容（现有客户端不重写）、安全边界（token、回环、设备撤销）。
