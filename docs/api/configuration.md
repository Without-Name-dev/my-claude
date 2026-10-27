# Session Configuration and Commands API

会话配置管理和斜杠命令执行。

## 配置管理

### GET /api/v2/sessions/:id/config

获取会话配置。

**响应**: `200 OK`
```json
{
  "sessionId": "sess_xyz",
  "effortLevel": "medium",
  "modelName": "claude-sonnet-5-5",
  "fastMode": false,
  "autoMode": false,
  "updatedAt": "2026-10-27T10:00:00Z"
}
```

### PATCH /api/v2/sessions/:id/config

更新会话配置。

**请求体** (所有字段可选):
```json
{
  "effortLevel": "high",
  "modelName": "claude-opus-5-5",
  "fastMode": true,
  "autoMode": false
}
```

**响应**: `200 OK`
```json
{
  "sessionId": "sess_xyz",
  "effortLevel": "high",
  "modelName": "claude-opus-5-5",
  "fastMode": true,
  "autoMode": false,
  "updatedAt": "2026-10-27T10:05:00Z"
}
```

## 命令执行

### POST /api/v2/sessions/:id/commands/execute

执行斜杠命令。

**请求体**:
```json
{
  "command": "/effort high"
}
```

**响应**: `200 OK`
```json
{
  "command": "effort",
  "result": {
    "effortLevel": "high",
    "message": "Effort level set to high"
  }
}
```

## 支持的命令

### /effort <level>
设置推理级别。

**级别**: `low` | `medium` | `high` | `xhigh` | `max` | `auto`

**示例**: `/effort high`

### /model <name>
切换模型。

**模型**: `opus` | `sonnet` | `haiku` | `opus-5.5` | `sonnet-5.5` | `haiku-5.5`

**示例**: `/model opus`

### /fast [on|off]
切换快速模式。

**示例**: `/fast` (切换) 或 `/fast on` (开启)

### /auto mode <on|off>
设置自动模式。

**示例**: `/auto mode on`

### /compact
压缩对话历史。

### /clear
清空当前会话。

### /help
显示命令帮助。

### /config
显示当前配置。

### /workflows
显示工作流运行状态。

## WebSocket 事件

### config.changed
```json
{
  "type": "config.changed",
  "key": "effortLevel",
  "value": "high"
}
```

### config.updated
```json
{
  "type": "config.updated",
  "config": {
    "sessionId": "sess_xyz",
    "effortLevel": "high",
    ...
  }
}
```

## Agent 和 Workflow API

### GET /api/v2/sessions/:id/agents

获取 Agent 执行记录。

**响应**: `200 OK`
```json
{
  "agents": [
    {
      "id": "agent_001",
      "sessionId": "sess_xyz",
      "parentId": null,
      "label": "代码分析",
      "phase": "Review",
      "status": "completed",
      "startedAt": "2026-10-27T10:00:00Z",
      "finishedAt": "2026-10-27T10:02:30Z",
      "tokenCount": 1500,
      "errorMessage": ""
    }
  ]
}
```

### GET /api/v2/sessions/:id/workflows

获取 Workflow 运行记录。

**响应**: `200 OK`
```json
{
  "workflows": [
    {
      "id": "wf_001",
      "sessionId": "sess_xyz",
      "name": "代码审查工作流",
      "status": "running",
      "currentPhase": "Verify",
      "phaseCount": 3,
      "agentCount": 5,
      "totalTokens": 8500,
      "startedAt": "2026-10-27T10:00:00Z",
      "finishedAt": null,
      "errorMessage": ""
    }
  ]
}
```

## Agent 状态

- `pending`: 等待执行
- `running`: 正在执行
- `completed`: 已完成
- `error`: 执行失败
- `cancelled`: 已取消

## Workflow 状态

- `running`: 运行中
- `completed`: 已完成
- `error`: 失败
- `cancelled`: 已取消
