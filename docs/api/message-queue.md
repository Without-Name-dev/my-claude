# Message Queue API

统一消息队列系统，支持跨设备消息同步和离线恢复。

## 端点

### POST /api/v2/sessions/:id/queue

入队新消息。

**请求体**:
```json
{
  "content": "分析这段代码",
  "attachments": ["file1.txt", "file2.py"],
  "source": "ios"
}
```

**响应**: `201 Created`
```json
{
  "id": "msg_abc123",
  "sessionId": "sess_xyz",
  "content": "分析这段代码",
  "attachments": ["file1.txt", "file2.py"],
  "status": "queued",
  "source": "ios",
  "createdAt": "2026-10-27T10:00:00Z",
  "seq": 42
}
```

### GET /api/v2/sessions/:id/queue?since=seq

获取队列消息，支持增量同步。

**查询参数**:
- `since` (可选): 返回 seq > since 的消息

**响应**: `200 OK`
```json
{
  "messages": [
    {
      "id": "msg_abc123",
      "sessionId": "sess_xyz",
      "content": "分析这段代码",
      "status": "running",
      "seq": 42,
      "startedAt": "2026-10-27T10:00:05Z",
      ...
    }
  ]
}
```

### DELETE /api/v2/sessions/:id/queue/:msgId

取消排队消息。

**响应**: `204 No Content`

### PATCH /api/v2/sessions/:id/queue/:msgId

更新消息状态（通常由 Connector 调用）。

**请求体**:
```json
{
  "status": "completed",
  "errorMessage": ""
}
```

**响应**: `204 No Content`

## WebSocket 事件

### queue.message_added
```json
{
  "type": "queue.message_added",
  "message": { ... }
}
```

### queue.message_status_changed
```json
{
  "type": "queue.message_status_changed",
  "messageId": "msg_abc123",
  "status": "running",
  "errorMessage": ""
}
```

### queue.message_completed
```json
{
  "type": "queue.message_completed",
  "messageId": "msg_abc123",
  "status": "completed"
}
```

### queue.message_cancelled
```json
{
  "type": "queue.message_cancelled",
  "messageId": "msg_abc123"
}
```

## 状态流转

```
queued → running → completed
             ↓
           error
             ↓
         cancelled
```
