# Agent Runtime 平台 · 后端

后端基于 Go + gRPC + grpc-gateway 实现，proto 定义位于 `pb/proto/`，
生成的 Go 代码位于 `pb/gen/`（通过 `pb/gen-proto.sh` 生成，package 名为 `agenteamv1`）。

## 目录结构

```
main.go                    启动入口：同时监听 gRPC(:9090) 与 HTTP 网关(:8080)
internal/store              统一存储接口 Store，及跨库协调逻辑（如删团队时协调 Postgres/Mongo）
internal/store/postgres      teams / agents 的 PostgreSQL 实现
internal/store/mongostore    chat_messages 的 MongoDB 实现
internal/cache               Agent 运行态热缓存接口 + Redis 实现
internal/runtime             Agent 运行态管理：配置校验、加载/重新加载、版本号（底层用 internal/cache）
internal/llm                 LLM 客户端（OpenAI 兼容协议；未配置密钥时使用本地 Echo 客户端）
internal/options             可选模型 / MCP 工具 / Skill 的静态清单
internal/service             gRPC 服务实现（TeamService / AgentService / WorkspaceService）
internal/idgen               简单唯一 ID 生成
pb/proto                     proto 源文件
pb/gen                       生成的 Go pb 代码（含 grpc-gateway）
scripts/                     数据库初始化脚本与本地开发 docker-compose（详见 scripts/README.md）
```

## 存储架构：为什么是三种数据库

Store 按数据形态拆分到不同数据库，而非塞进同一种数据库，详见 `scripts/README.md`：

| 数据库 | 承载数据 | 选型理由 |
| --- | --- | --- |
| **PostgreSQL** | `teams`、`agents` | 强关系型配置数据，team 与其主 agent 相互引用，创建时需原子写入；`internal/store/postgres` |
| **Redis** | Agent 运行态热缓存 | 纯按 id 存取的整块配置快照，无范围查询需求，典型 KV 场景，也让运行态在多实例部署下可共享；`internal/cache` |
| **MongoDB** | `chat_messages`（聊天记录） | 按团队追加、按时间读取的日志型数据，字段易演进，文档模型更合适；`internal/store/mongostore` |

业务层（`internal/service`）只依赖 `internal/store.Store` 接口和 `internal/runtime.Manager`，具体使用哪些数据库对其透明。

## 核心概念

- **Team（团队）**：创建时自动生成一个 `is_main=true` 的主 Agent。
- **Agent**：包含 name / prompt / model / mcp_tools / skills 等配置，
  保存（`UpdateAgent`）时会触发 `internal/runtime.Manager.Load`：
  校验配置合法性 → 版本号自增 → 状态置为 `LOADED`（校验失败则为 `ERROR`），
  并将结果写入 Redis 作为当前生效的运行态快照。
- **Workspace（工作区）**：`SendMessage` 会以 Agent 当前生效配置组装 system prompt
  （包含绑定的 MCP 工具 / Skill 说明），调用 LLM 客户端得到回复，并持久化整段对话到 MongoDB。

## 运行

```bash
# 0. 准备三种数据库（本地已有则跳过；也可用 scripts/docker-compose.yml 一键拉起）
#    详见 scripts/README.md
createdb agenteam
psql -d agenteam -f scripts/postgres/0001_init.sql
redis-server /opt/homebrew/etc/redis.conf --daemonize yes   # 或 brew services start redis
mongod --dbpath <your-data-dir> &                            # MongoDB 索引由后端启动时自动创建

# 1. 安装 protoc 插件（仅需一次，修改 proto 后需要重新生成时才用到）
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@v2.22.0

# 2. 生成 pb 代码（修改 proto 后需要重新执行）
bash pb/gen-proto.sh

# 3. 启动服务
export AGENTEAM_POSTGRES_DSN="postgres://localhost:5432/agenteam?sslmode=disable"
export AGENTEAM_REDIS_DSN="redis://127.0.0.1:6379/0"
export AGENTEAM_MONGO_URI="mongodb://127.0.0.1:27017"
go run .
```

默认监听 `:9090`（gRPC）与 `:8080`（HTTP/REST，供前端调用）。

### 可配置环境变量

| 变量 | 说明 | 默认值 |
| --- | --- | --- |
| `AGENTEAM_GRPC_ADDR` | gRPC 监听地址 | `:9090` |
| `AGENTEAM_HTTP_ADDR` | HTTP 网关监听地址 | `:8080` |
| `AGENTEAM_POSTGRES_DSN` | PostgreSQL 连接串 | `postgres://localhost:5432/agenteam?sslmode=disable` |
| `AGENTEAM_REDIS_DSN` | Redis 连接串（`redis://[:password@]host:port/db`） | `redis://127.0.0.1:6379/0` |
| `AGENTEAM_MONGO_URI` | MongoDB 连接串 | `mongodb://127.0.0.1:27017` |
| `AGENTEAM_MONGO_DATABASE` | MongoDB 数据库名 | `agenteam` |
| `AGENTEAM_LLM_API_KEY` | LLM API Key（未设置时使用本地 Echo 客户端，无需任何密钥即可跑通全流程） | 空 |
| `AGENTEAM_LLM_BASE_URL` | 兼容 OpenAI 协议的 Base URL（OpenAI / DeepSeek / 通义千问 / 智谱等均可） | `https://api.openai.com/v1` |

所有数据库连接串/密码/密钥均只允许通过环境变量注入，不接受任何用户请求动态指定。

## 已知限制 / 后续可演进方向

- MCP 工具与 Skill 目前仅作为“配置元数据”传递给 LLM 作为上下文说明，
  **尚未接入真正的 MCP 协议调用**；后续可在 `internal/runtime` 中扩展为
  真正建立 MCP Server 连接并支持 function calling。
- `DeleteTeam` 跨 PostgreSQL / MongoDB 两个数据库操作，非分布式事务，
  采用“先删关系型主数据、再清理聊天记录”的最终一致性策略：极端情况下
  可能残留孤立的聊天记录，但不影响团队/Agent 数据一致性。
- 未接入鉴权/多租户，`internal/service` 中的 gRPC handler 目前对所有请求一视同仁。
