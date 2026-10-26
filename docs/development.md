# v2 开发指南

## 分支与环境

`main` 是 v2 主线，新功能从最新 `main` 创建分支，分支名为 `<type>/<domain>/<topic>`（type 如 `feat`、`fix`、`docs`；domain 缩写为 `fe`、`be`、`con`、`ios`、`docs`，多个 domain 用 `+` 连接），如 `feat/con+be/<topic>`。`v2` 保留用于发布过渡，不再把旧 `main` 的 API 和数据库假定套用到当前主线。历史资料见[升级指南](upgrading.md)。

生产实现是 `go/`（Go 1.27+）：`cmd/aa-server` 是 API 与 Web 静态站点的来源，`cmd/aa-connector` 驱动工作设备上的 Claude Code。Web 使用 Node.js 22 + Corepack/Yarn。Python 参考实现（`server/`、`connector/`）使用 Python 3.12+ 与 uv，不参与生产部署。各子项目独立安装依赖，不在仓库根目录运行一个假设存在的总构建命令。

## Go Server 与 Connector

编译并运行 Server：

```bash
cd go
go build -o bin/aa-server ./cmd/aa-server
go build -o bin/aa-connector ./cmd/aa-connector

AA_DATA_DIR=../.local-dev/go-data \
AA_LISTEN=127.0.0.1:8080 \
AA_PUBLIC_URL=http://127.0.0.1:8080 \
AGENT_SERVER_SECRET=dev-secret \
AA_WEB_DIR=../web-next/out \
bin/aa-server
```

首次启动会创建并迁移 `AA_DATA_DIR/server.db`；`bin/aa-server -migrate` 只应用迁移后退出。环境变量、配对流程、`AA_CONNECTOR_DATA_DIR` 和 nginx 配置见 [go/README.md](../go/README.md)。

Connector 先配对，再作为服务运行：

```bash
cd go
bin/aa-connector --pair --server http://127.0.0.1:8080
# 在客户端输入配对码后，connector.json 写入 AA_CONNECTOR_DATA_DIR（默认 ~/.agents-anywhere）
AA_CONNECTOR_DATA_DIR=~/.agents-anywhere \
AGENT_CONNECTOR_CLAUDE_PLUGIN_BRIDGE=1 \
bin/aa-connector --config ~/.agents-anywhere/connector.json
```

## 静态与构建检查

以下检查不要求打开浏览器或移动模拟器。只运行与修改有关的检查，不必为文档改动重新构建所有安装包。

Go（生产 Server / Connector）：

```bash
cd go
go build ./cmd/aa-server ./cmd/aa-connector
```

Web：

```bash
cd web-next
yarn install
yarn typecheck
yarn protocol:check
```

Python 参考实现（只在修改 `server/` 或 `connector/` 时运行语法检查）：

```bash
python -m compileall -q connector/connector server/agent_server
```

这些静态检查不验证运行时依赖、数据库迁移、OAuth 回调或远程 Runtime 会话。iOS 由 `.github/workflows/ios-unsigned-ipa.yml` 构建；本机没有 Xcode 时不能替代 iPhone 真机验证。

## Python 参考栈本地联调

`./local-up.sh` 用于 Python 参考实现的本地联调：它启动 Docker PostgreSQL/Redis、迁移数据库，再在前台运行 Python Server 和 Web。默认 Web 为 `http://127.0.0.1:5174`，Server 为 `http://127.0.0.1:8000`，PostgreSQL/Redis 端口为 `55432` / `56379`。日志在 `.local-dev/logs/`。按 Ctrl-C 停止本次启动的服务。

- `--skip-install`：复用依赖。
- `--with-connector`：一起启动 Python 参考 Connector；默认不启动。
- `--reload`：启用 Server 热重载；默认关闭以减少实时连接中断。
- `--access-log`：打印 uvicorn 逐请求访问日志；默认关闭。Server 日志级别默认为 INFO，可用 `LOGURU_LEVEL=DEBUG` 查看调试日志。
- `--listen`：监听局域网地址；默认只监听回环。
- `--reset-data`：删除本地数据库数据卷，仅在明确不要这些数据时使用。

Go Server 的本地运行方式见上面的“Go Server 与 Connector”一节。

## 构建产物

Server 与 Connector 用 `cd go && go build -o bin/aa-server ./cmd/aa-server && go build -o bin/aa-connector ./cmd/aa-connector` 构建；部署用的二进制在 `go/dist/`，不要修改。Web 静态导出用 `cd web-next && NEXT_OUTPUT=export yarn build` 生成，输出到 `web-next/out/`，由 `AA_WEB_DIR` 提供给 Go Server 或 nginx。

iOS 的构建与签名见 [iOS 工程](../ios/) 和 `.github/workflows/ios-unsigned-ipa.yml`。发布步骤见 [2.0.0 发布说明](releases/2.0.0.md)。凭据从环境或安全存储注入，不能提交到源码或文档。
