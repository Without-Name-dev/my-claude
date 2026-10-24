# 升级到 v2 主线

## 先区分三个版本

本次产品版本为 **2.1.0**。`main` 现在承载 v2。本文所说的 **v1 / 旧主线** 指主线切换前的实现，源代码提交为 `6c47e419`，不再指当前 `main`。早期 [main-to-v2 文档](migrations/main-to-v2/README.md)保留作历史参考。

生产 Server 是 Go 实现（`go/cmd/aa-server`），数据存放在 `AA_DATA_DIR/server.db`（SQLite，WAL 模式），schema 迁移内嵌在 `go/internal/store/migrations/`。各端的 PATCH 版本可以不同，数据库版本不能用来推断兼容性，见[版本号规则](versioning.md)。

## 升级 Go Server

1. 记录当前二进制或提交、环境变量（`AA_DATA_DIR`、`AA_LISTEN`、`AA_PUBLIC_URL`、`AGENT_SERVER_SECRET`、`AA_WEB_DIR`）、客户端版本和数据库路径。停止服务后备份 `AA_DATA_DIR/server.db`（连同 `server.db-wal`、`server.db-shm`）和 `AA_DATA_DIR/uploads`，保留旧二进制。
2. 在隔离环境用同一份数据文件演练迁移和启动。
3. 用准备部署的同一份源码或二进制先执行迁移，再启动新版本：

   ```bash
   cd go
   go build -o bin/aa-server ./cmd/aa-server
   AA_DATA_DIR=/var/lib/agents-anywhere bin/aa-server -migrate
   AA_DATA_DIR=/var/lib/agents-anywhere bin/aa-server
   ```

   `-migrate` 应用内嵌迁移后退出；正常启动也会在打开数据库时应用未执行的迁移。迁移按文件名顺序执行，已应用版本记录在 `schema_migrations` 表；不要修改已应用的迁移文件。
4. 检查 `GET /api/v2/health/ready` 返回 `{"status":"ready"}`，再恢复入口流量和 Connector。验证账号登录、设备连接、会话恢复、消息、审批、文件和终端。
5. 使用 [2.0.3 下载入口](../README.md#下载与入口)升级客户端。当前安装包没有可用的应用内下载地址，需手动分发。

环境变量、nginx 反向代理和部署细节见 [go/README.md](../go/README.md)。

## 从 v1 部署升级

v1 数据导入只在 2.0.x 提供。仍在 v1 的部署要先检出 2.0.x 的源码，按该版本的本文档完成导入，再升级到 2.1.0。

## 回滚

保留旧二进制和升级前的 `AA_DATA_DIR` 备份。若新版已经写入数据，直接换回旧二进制不等于安全回滚：SQLite 迁移只向前应用，旧二进制可能无法理解新的 schema。默认做法是停止入口流量，恢复升级前的 `server.db` 和 `uploads` 备份，再启动旧二进制。恢复旧备份会失去切换后的新数据，需先明确如何保留这些数据。

## 升级验收

- `aa-server -migrate` 成功，`/api/v2/health/ready` 返回 `{"status":"ready"}`。
- `AA_DATA_DIR/server.db`、`uploads` 和 Connector 配置的备份与恢复已经演练。
- 实际使用的 Runtime 能发现、启动、恢复会话，并报告可用能力。
- 登录、设备连接、消息和审批、文件与终端在所用客户端可用。
- 原有账号的邮箱绑定与登录流程确认可用。
- 部署记录保留提交、二进制、安装包和验证结果，不包含凭据。

这是运维验收清单；源码主线合并和安装包发布并不证明某个生产实例已经完成上述迁移。

## 附录：Python 参考实现的 PostgreSQL / Alembic 升级

以下步骤只适用于 `server/`（Python 参考实现）的部署，不适用于生产 Go Server。Python 参考实现的 schema revision 为 `v2_44`（schema version `2.44`），使用 PostgreSQL、Redis 与 Alembic。

1. 记录当前镜像或提交、环境配置、客户端版本和数据库 revision。备份 PostgreSQL、文件存储及 Connector 配置；保留原镜像。
2. 在隔离环境演练迁移。所有旧 Server 和外部数据库写入进程都停止后，才执行生产迁移。
3. 使用准备部署的同一份源码或镜像执行迁移：

   ```bash
   cd server
   uv sync
   uv run python -m agent_server.infra.db.migrations current --verbose
   uv run python -m agent_server.infra.db.migrations upgrade
   uv run python -m agent_server.infra.db.migrations current --verbose
   ```

   以上命令使用该环境的 `AGENT_SERVER_DB_URL`。当前目标应为 `v2_44`；以后升级时，以待部署源码的 `CURRENT_SCHEMA_REVISION` 为准。

Redis 使用 AOF、`appendfsync everysec`、持久化卷和 `noeviction`。Redis 中存在尚未刷入 PostgreSQL 的 Timeline 数据，不能再按早期文档把它当成可随意清空的临时协调层。Redis ACL 还需要允许 `INFO server`。完整的 Timeline 刷写与 revision lease 语义见 [Server README](../server/README.md)；部署前结合实际数据量安排维护窗口。

Compose 的 `migrate-next` 会在新 Server 前执行迁移，但不会停止另一个 Compose 项目、旧容器或外部写入者。部署者需要先停止旧写入进程。不要把代码分支切换当成数据库迁移。
