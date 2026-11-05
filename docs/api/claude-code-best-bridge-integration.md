# Claude Code Best 前台 bridge 融合基线

本文记录当前融合工作中已核查的前台控制语义。目标是将 Claude Code Best 的 Runtime 直接连接到 Go 服务，不以旧 phone-pane 插件作为新链路的执行基础。

## 模型控制响应

前台 bridge 的 `control_request` 使用 `request_id` 关联 `control_response`。当请求的 `subtype` 为 `set_model`：

- 已注册 `onSetModel` 回调：调用该回调，并返回 `success`，表示控制处理程序已接受该请求。
- 未注册回调：返回 `error`，不得返回 `success`，因为当前 Runtime 没有执行模型切换。
- `success` 本身不证明后续模型请求已经成功，也不保证服务端供应商路由已生效。融合方案需要单独提供可观察的状态及后续请求结果。

此修正不新增请求字段或响应类型；调用方仍需处理已有的 `success` 和 `error`。该行为依赖本地 Claude Code Best 源码更新，不能认为未升级的 Runtime 已具备此修正。

## 尚未完成的范围

Go 的 RCS/ACP 接入、用户与设备映射、服务端消息队列、审批竞争、模式实际回读、桌面应用与手机端融合仍需逐项实现。上游 `set_permission_mode` 的成功回复也不能自动等同于状态已回读；不可据此宣传整个远程切换链路已完成。

## 当前源码排查中的融合要点

- 原系统用于发送消息的 `submit_outbox`、connector request ID、提交回执及 turn 监控是一条实际链路；`go/internal/connector/queue_integration.go` 中的 `StartQueueConsumers` 是占位实现，不能把它误认为已接入的消息消费链路，也不能因此断定它就是用户所遇问题的根因。
- 原系统在 `session_open.go` 和各平台实现中已有打开可见终端的逻辑，但依赖原有 Runtime 与插件的存活判断。适配 Claude Code Best 时，应复用安全的目录检查和启动能力，同时替换存活判断、启动命令和会话接入方式，不能只更改可执行文件名。
- 原系统 `childEnvironment` 启用代理时清理了部分 provider 变量；Claude Code Best 的 `getAPIProvider` 还会读取 OpenAI、Gemini、Grok、Foundry 的环境变量，且 `settings.modelType` 优先于这些环境变量。融合时必须同时处理 settings 和环境变量；只设置 `ANTHROPIC_BASE_URL`、或只清理环境变量，都不足以证明请求走了 Go 模型代理。
- 上游 RCS 的 worker delivery 路由当前返回成功但不记录投递状态。融合后需以实际关联的 Runtime 回执建立状态，不能把该响应直接映射为消息已执行。
- 上游 REPL bridge 受 `BRIDGE_MODE` 和只读 `outboundOnly` 等条件影响；需在能力协商中反映，避免把只读连接显示为可控制。
- 现有 Go `gateway.go` 已包括 Anthropic 和 OpenAI Chat Completions 路径；应核实和补齐其适配范围，不应重复建立第二套供应商系统。
