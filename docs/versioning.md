# 版本号规则

Agents Anywhere 的产品版本使用 `MAJOR.MINOR.PATCH`，三段各有明确含义：

| 段 | 示例 | 含义 | 兼容性承诺 |
| --- | --- | --- | --- |
| `PATCH` | `2.0.0` → `2.0.1` | 各端的小版本更新：修复、体验改进、只在单端内部的新功能 | 不改变跨端接口；同一 `MAJOR.MINOR` 下任意端的任意 PATCH 版本可以互相配合 |
| `MINOR` | `2.0.x` → `2.1.0` | 接口更新：Server API、实时事件、Runtime/本机协议、`contracts/` 下的契约发生变化 | 同一 `MAJOR` 内尽量向后兼容；需要写明各端的最低配套版本 |
| `MAJOR` | `2.x.x` → `3.0.0` | 架构变更：Runtime、API、存储或客户端架构重写，例如 v1 → v2 | 不保证兼容，需要提供[升级指南](upgrading.md) |

## PATCH：各端独立发布

- Desktop、Android、iOS、Web、Server、Connector 可以各自发布 PATCH 版本，不需要同时发版。例如 Android 发布 `2.0.1` 时，Desktop 可以仍是 `2.0.0`。
- 同一 `2.0` 线上不同端的 PATCH 号不对齐，也不代表功能相同或需要同步升级。
- 只有 PATCH 允许不同；各端的 `MAJOR.MINOR` 必须保持一致。
- PATCH 不能新增、删除或改变其他端依赖的字段、事件、端点或协议语义。如果一个“单端修复”需要另一端配合修改，就说明它是接口更新，应当升 MINOR。
- Server 的 PATCH 可以包含不影响接口的数据库迁移，例如索引或内部表结构调整。数据库 revision 仍然独立编号，见下文。

## MINOR：接口更新

以下变化都需要升 MINOR：

- `/api/v2` 下端点、请求或响应字段的新增、删除或语义变化。
- 实时事件（WebSocket）类型或载荷的变化。
- `contracts/` 下 Runtime control、本机 Connector 记录等契约的变化。
- 有效能力（capabilities）新增了需要客户端识别的项。

发布 MINOR 时应当：

1. 在发布说明中列出接口变化，并写明各端的最低配套版本（例如“Android 需 ≥ 2.1.0 才能使用排队消息”）。
2. 旧客户端遇到新 Server 时，能降级或忽略新字段，而不是出错。
3. **所有端**都要把版本号更新到 `2.1.0`，包括本次没有功能改动的端，这样 MINOR 号始终表示同一套接口；之后的修复继续按 PATCH 独立递增。

## MAJOR：架构变更

- API namespace 跟随 MAJOR，例如 `2.x` 对应 `/api/v2`。
- 需要编写迁移或升级指南，并在发布说明中说明旧版本的数据、Connector 和客户端怎样处理。
- 和 MINOR 一样，**所有端**都要把版本号更新到 `3.0.0`。
- `_deprecated/` 用于保留上一代架构的参考代码，不参与当前构建。

## 独立编号的其他版本

以下编号不跟随产品版本，也不能用来推断兼容性：

| 编号 | 位置 | 规则 |
| --- | --- | --- |
| 数据库 revision / schema version | `go/internal/store/migrations/NNNN_*.sql` | 每次 schema 变化新增一个文件，按文件名顺序应用 |
| 契约版本 | `contracts/<name>/<version>/` | 按各契约自身演进；有不兼容变化时新建版本目录 |

## 各端版本号的位置

| 端 | 文件 | 字段 |
| --- | --- | --- |
| Server | `go/` 构建元数据 | 模块版本或 `vcs.revision`；`/api/v2/admin/service` 的 `version` 返回该值（本地构建显示 `devel-<revision>`） |
| iOS | `ios/Agents Anywhere/Agents Anywhere.xcodeproj/project.pbxproj` | `MARKETING_VERSION` |
| Web | `web-next/package.json` | `version`（`src/lib/demo-api.ts` 中的演示版本号也要同步） |
| Connector | `go/` 构建元数据 | 模块版本或 `vcs.revision` |

升 MINOR 或 MAJOR 时，逐项检查上表中的每一个位置。

Python 参考实现的版本仍记录在 `server/pyproject.toml`、`server/agent_server/app.py` 和 `connector/pyproject.toml`，但这些版本不代表生产 Go 二进制。
