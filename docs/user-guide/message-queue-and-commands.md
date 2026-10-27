# 消息队列使用指南

## 功能概述

统一消息队列系统让你可以在多个设备间无缝同步消息，支持离线排队和实时状态更新。

## 基本使用

### 发送消息到队列

**iOS**:
1. 在聊天界面输入消息
2. 消息会自动进入队列等待执行
3. 在输入框上方看到队列状态栏

**Web**:
1. 发送消息后会显示队列状态
2. 点击队列栏查看所有排队消息
3. 可随时取消排队中的消息

### 查看队列状态

**状态说明**:
- 🟡 **排队中** - 消息等待执行
- 🔵 **执行中** - 正在处理消息
- 🟢 **已完成** - 消息执行成功
- 🔴 **失败** - 执行出错
- ⚪ **已取消** - 用户取消

### 离线同步

当设备离线时：
1. 消息会保存在本地队列
2. 恢复网络后自动同步
3. 使用 `seq` 序号确保不丢失消息

## 斜杠命令

### 推理级别

```
/effort low       # 低努力（快速响应）
/effort medium    # 中等努力（默认）
/effort high      # 高努力（深度思考）
/effort xhigh     # 极高努力
/effort max       # 最大努力
/effort auto      # 自动调整
```

### 模型切换

```
/model opus       # Claude Opus 5.5（最强）
/model sonnet     # Claude Sonnet 5.5（均衡）
/model haiku      # Claude Haiku 5.5（快速）
```

### 模式切换

```
/fast             # 切换快速模式
/fast on          # 开启快速模式
/fast off         # 关闭快速模式

/auto mode on     # 开启自动执行
/auto mode off    # 关闭自动执行
```

### 会话管理

```
/compact          # 压缩对话历史
/clear            # 清空当前会话
/help             # 显示帮助
/config           # 查看配置
/workflows        # 查看工作流
```

## 键盘快捷键

**iOS**:
- 长按输入框 - 显示命令菜单

**Web**:
- `Cmd+K` / `Ctrl+K` - 打开命令面板
- `↑` / `↓` - 导航命令
- `Enter` - 执行命令
- `Esc` - 关闭面板

## Agent 和 Workflow 可视化

### Agent 树形视图

查看嵌套的 Agent 执行关系：
1. 点击箭头展开/折叠子 Agent
2. 查看每个 Agent 的状态和耗时
3. 查看 Token 使用量

### Workflow Dashboard

监控工作流执行：
- 当前阶段进度
- Agent 数量统计
- Token 消耗总计
- 执行时间

## 故障排除

### 消息卡在队列中

**原因**: Connector 离线或未启动队列消费者

**解决**:
1. 检查 Connector 是否在线
2. 重启 Connector
3. 查看 Connector 日志

### Redis 连接失败

**影响**: 自动降级到 SQLite，功能正常但实时性降低

**解决**:
1. 检查 Redis 服务是否运行
2. 验证 `REDIS_URL` 环境变量
3. 查看 Server 日志中的降级提示

### 离线消息未同步

**原因**: 本地 seq 与服务器不一致

**解决**:
1. 刷新页面或重启应用
2. 手动调用 API: `GET /queue?since=0`
3. 清空本地缓存重新同步

## 配置选项

### Server 环境变量

```bash
# Redis（可选，默认仅用 SQLite）
REDIS_URL=redis://localhost:6379

# 队列限制
QUEUE_MAX_SIZE=100              # 单会话最大队列长度
OFFLINE_MESSAGE_TTL=24h         # 离线消息保留时间
```

### 性能调优

**Redis 内存限制**:
```bash
# 限制 Redis 内存为 100MB
redis-server --maxmemory 100mb --maxmemory-policy allkeys-lru
```

**SQLite WAL 模式**:
自动启用，无需配置。

## 最佳实践

1. **离线工作**: 放心离线操作，消息会在恢复网络后自动同步
2. **批量任务**: 多条消息会依次执行，无需等待
3. **及时取消**: 不需要的消息应及时取消，节省资源
4. **监控队列**: 定期查看队列状态，避免积压
5. **合理配置**: 根据任务复杂度选择合适的 effort level

## API 集成

开发者可以直接调用 API：

```bash
# 入队消息
curl -X POST http://localhost:8080/api/v2/sessions/{sessionId}/queue \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"content":"分析代码","source":"api"}'

# 查询队列
curl http://localhost:8080/api/v2/sessions/{sessionId}/queue?since=0 \
  -H "Authorization: Bearer $TOKEN"

# 执行命令
curl -X POST http://localhost:8080/api/v2/sessions/{sessionId}/commands/execute \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"command":"/effort high"}'
```

详细 API 文档见 [API Reference](../api/message-queue.md)。
