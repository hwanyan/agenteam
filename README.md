# Agent Runtime 平台 · 后端

后端基于 Go + gRPC + grpc-gateway 实现，proto 定义位于 `pb/proto/`，
生成的 Go 代码位于 `pb/gen/`（通过 `pb/gen-proto.sh` 生成，package 名为 `agenteamv1`）。

## 目录结构

```
main.go                    启动入口：同时监听 gRPC(:9090) 与 HTTP 网关(:8080)
internal/store              统一存储接口 Store，及跨库协调逻辑（如删团队时协调 Postgres/Mongo）
internal/store/postgres      teams / agents / mcp_tools / team_members 的 PostgreSQL 实现
internal/store/mongostore    chat_messages 的 MongoDB 实现
internal/authclient          hwan 认证服务 gRPC 客户端（本地验签 Access Token）
internal/authctx             在 context 中携带当前用户身份（HTTP 中间件与 gRPC 拦截器共用）
internal/httpmiddleware      HTTP 网关 Bearer 鉴权中间件
internal/grpcauth            gRPC 服务端 Bearer 鉴权拦截器（Workspace 流式走拨号转发，需在此重新校验）
internal/cleanup             团队软删除后的后台清理 worker
internal/cache               Agent 运行态热缓存接口 + Redis 实现
internal/runtime             Agent 运行态管理：配置校验、加载/重新加载、版本号（底层用 internal/cache）
internal/llm                 LLM 客户端：client.go 为接口层（Client/ChatRequest/StreamChunk/New），
                              impl.go 为实现层（DeepSeekClient 基于官方 Go SDK；EchoClient 为无密钥兜底实现）
internal/a2a                 A2A（Agent2Agent）协议客户端：Agent Card 发现 + message/send（非流式）+
                              message/stream（SSE 流式），用于将平台内 Agent 链接到外部 A2A Agent 提供方
internal/config              统一配置加载：从环境变量读取所有配置项（含 DeepSeek API Key）
internal/options             可选模型 / MCP 工具 / Skill 的静态清单
internal/service             gRPC 服务实现（TeamService / AgentService / WorkspaceService）
internal/tools                简单唯一 ID 生成（New / NewMCPToolID）
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

- **Team（团队）**：创建时自动生成一个 `is_main=true` 的主 Agent，创建方式与普通 Agent 一致，
  由 `CreateTeamRequest.kind` 决定（未指定时默认 `AGENT_KIND_PROMPT`，`prompt`/`model` 留空则
  使用平台默认值，兼容旧客户端仅传 `name` 的用法）：主 Agent 既可以是本地 Prompt 驱动，也可以
  直接 `kind=AGENT_KIND_A2A` 链接一个外部 A2A Agent 提供方作为主 Agent（校验逻辑与
  `AgentService.CreateAgent` 对 A2A 方式的处理一致）。前端「新增团队」表单已提供与「新增 Agent」
  弹窗一致的创建方式切换 UI。
  注意：若主 Agent 走 A2A 方式，`internal/service.resolveDelegationTools` 会直接短路——委派机制
  依赖向主 Agent 下发本地 function calling 工具，无法对不受控的外部 A2A Agent 生效，因此
  A2A 主 Agent 的团队即便还有其他 Agent，也不具备"团队内多 Agent 委派"能力，只能作为单一
  Agent 使用（这也是本项目当前的已知边界，而非缺陷）。
- **Agent**：`kind` 字段区分两种创建/接入方式（创建后不可变更）：
  - `AGENT_KIND_PROMPT`（默认）：本地 Prompt + LLM 驱动，包含 name / prompt / model / mcp_tools / skills 等配置。
  - `AGENT_KIND_A2A`：通过 [A2A（Agent2Agent）协议](https://a2a-protocol.org/) 链接一个外部 Agent 提供方，
    只需 name + `a2a_config.endpoint_url`（可选 `auth_scheme`/`auth_token`/`tenant_id`），
    prompt/model/mcp_tools/skills 对该方式无效。部分第三方 A2A Agent（如姊妹项目 `agently`）
    采用“TenantID + Token”双因子鉴权模型：除 `Authorization: Bearer <token>` 外，还要求通过
    `X-A2A-Tenant-Id: <tenant_id>` 请求头显式声明调用方身份，二者缺一不可；`tenant_id`
    由对端预先分配、属非机密的身份标识（类似 AccessKeyId），与 `auth_token` 不同，
    响应中会如实回显，不需要脱敏（不经过 `redactAgent`）。
  两种方式均在保存（`CreateAgent`/`UpdateAgent`）时触发 `internal/runtime.Manager.Load`：
  校验配置合法性（A2A 方式还会实际向 `endpoint_url` 发起一次 Agent Card 发现请求校验连通性，
  并回填对端名称/描述/技能/是否支持流式到 `a2a_config` 的只读字段）→ 版本号自增 →
  状态置为 `LOADED`（失败则为 `ERROR`），并将结果写入 Redis 作为当前生效的运行态快照。
  另提供 `DiscoverA2AAgent` RPC，供前端在正式创建/保存 A2A Agent 前先行探测连通性与预览对端信息，
  不产生任何持久化副作用。
  出于安全考虑，`a2a_config.auth_token` 只在写请求（Create/UpdateAgent）中传入，
  任何服务端响应都不会回显其明文值，只通过 `auth_token_set` 标记是否已配置凭证
  （`internal/service.redactAgent` 统一脱敏）。
- **Workspace（工作区）**：`SendMessage`/`SendMessageStream` 会按 Agent 的 `kind` 分发：
  - Prompt 方式：以 Agent 当前生效配置组装 system prompt（包含绑定的 MCP 工具 / Skill 说明），
    调用 LLM 客户端得到回复；流式版本对接 DeepSeek 官方 SDK 的 `CreateChatCompletionStream`
    逐段推送增量输出。
  - A2A 方式：通过 `internal/a2a.Client` 将用户消息转发给外部 Agent：
    - 若对端 Agent Card 声明 `capabilities.streaming=true`（保存/加载时探测并缓存于
      `a2a_config.streaming`），`SendMessageStream` 会调用 `Client.SendMessageStream`
      对接 A2A 协议的 JSON-RPC 方法 `message/stream`（SSE），逐段转发对端产生的增量文本，
      与 Prompt 方式一样具有逐字打字机效果；
    - 否则（或非流式接口 `SendMessage`）退化/统一调用非流式的 `message/send`，
      一次性获取完整回复后（流式场景下）作为单个 delta 推送。
  两种方式下，用户消息与最终完整回复都会持久化到 MongoDB，历史记录格式保持一致。
  - **团队内多 Agent 委派（delegation）**：仅当发起对话的 Agent 是团队**主 Agent**、走本地
    Prompt 方式、且团队内还存在其他 Agent 时才会启用（`internal/service.resolveDelegationTools`）：
    会把团队内其他 Agent 的 `名称 + 能力摘要`（Prompt 方式取 Prompt 摘要；A2A 方式取
    `a2a_config.remote_description`）追加进主 Agent 的 system prompt，并下发一个
    `delegate_to_agent(agent_id, message)` function-calling 工具（DeepSeek 官方 SDK
    的 Tools/ToolCalls 能力）。主 Agent 判断本次请求更适合某个子 Agent 处理时会调用该工具，
    服务端据此把请求真正转发给对应子 Agent（Prompt 方式走 `ChatStream` 实时流式转发；
    A2A 方式复用既有的 `streamA2AReply`），并在回复前加上「由子 Agent「xxx」处理：」提示，
    committing 到同一条团队对话历史中。只做一层委派（子 Agent 不会再继续委派），避免委派链路
    / 死循环；未配置 `DEEPSEEK_API_KEY`（走本地 `EchoClient`）时不支持该能力，因为
    Echo 客户端不会产生真实的 ToolCalls。
  - **HTTP 网关注册方式说明**：`main.go` 中 Team/Agent 服务通过 grpc-gateway 的 in-process
    直调（`RegisterXxxHandlerServer`）挂载，无需拨号；但 Workspace 服务因含 server-streaming
    RPC，in-process 直调模式对流式转发返回 `Unimplemented`（grpc-gateway 已知限制），因此改为
    真实拨号本地 gRPC 端口（`RegisterWorkspaceServiceHandlerClient`）以支持 HTTP chunked 流式响应。

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
export AGENTEAM_AUTH_SERVICE_ADDR="127.0.0.1:50054"   # hwan 认证服务 gRPC 地址（必填）
export AGENTEAM_AUTH_SERVICE_TOKEN="<调用方凭证>"       # 须与 hwan auth 的 server.services 里 name 为 agenteam 那一项的 token 一致（必填）
go run .
```

默认监听 `:9090`（gRPC）与 `:8080`（HTTP/REST，供前端调用）。

### 可配置环境变量

`internal/config.Load()` 统一从环境变量读取所有配置项（含 DeepSeek API Key），
提供合理默认值便于本地开发；`internal/llm` 不再直接读取环境变量，而是由
`main.go` 从 `config.Config` 中取值后通过 `llm.New(apiKey, baseURL)` 传入。

| 变量 | 说明 | 默认值 |
| --- | --- | --- |
| `AGENTEAM_GRPC_ADDR` | gRPC 监听地址 | `:9090` |
| `AGENTEAM_HTTP_ADDR` | HTTP 网关监听地址 | `:8080` |
| `AGENTEAM_POSTGRES_DSN` | PostgreSQL 连接串 | `postgres://localhost:5432/agenteam?sslmode=disable` |
| `AGENTEAM_REDIS_DSN` | Redis 连接串（`redis://[:password@]host:port/db`） | `redis://127.0.0.1:6379/0` |
| `AGENTEAM_MONGO_URI` | MongoDB 连接串 | `mongodb://127.0.0.1:27017` |
| `AGENTEAM_MONGO_DATABASE` | MongoDB 数据库名 | `agenteam` |
| `DEEPSEEK_API_KEY` | DeepSeek 平台 API Key（[申请地址](https://platform.deepseek.com/api_keys)；未设置时使用本地 Echo 客户端，无需任何密钥即可跑通全流程） | 空 |
| `DEEPSEEK_BASE_URL` | 可选，自定义 DeepSeek API Base URL | `https://api.deepseek.com/` |
| `AGENTEAM_AUTH_SERVICE_ADDR` | hwan 认证服务（`hwan/app/auth`）的 gRPC 地址。**必填**，未设置时服务拒绝启动 | 无 |
| `AGENTEAM_AUTH_SERVICE_TOKEN` | 调用认证服务所用的调用方凭证，须与认证服务 `server.services` 里 `name: agenteam` 那一项的 `token` 一致（可用 `openssl rand -hex 32` 生成）。**必填**，未设置时服务拒绝启动 | 无 |

所有数据库连接串/密码/密钥均只允许通过环境变量注入，不接受任何用户请求动态指定。

### 鉴权（用户系统由 hwan 统一提供）

已移除原先的静态 token（`AGENTEAM_AUTH_TOKENS` / 前端 `VITE_AUTH_TOKEN`）。现在所有访问都必须使用**用户登录后获得的 token**。

hwan 认证服务只提供 gRPC，因此 agenteam 作为代理对前端暴露登录 / 注册接口（`internal/httpauth`）：

| 接口 | 是否需要 Bearer token | 说明 |
| --- | --- | --- |
| `POST /v1/auth:sendVerifyCode` | 否 | `{email, purpose}`，发送邮箱验证码 |
| `POST /v1/auth:register` | 否 | `{username, email, password, nickname?, verifyCode}`，返回 `{token, refreshToken, expiresIn, user}` |
| `POST /v1/auth:login` | 否 | `{username, password}`（用户名或邮箱），返回同上 |
| `POST /v1/auth:refresh` | 否 | `{refreshToken}`，换取新的 `token` + 新的 `refreshToken`（轮换，旧的立即作废）。此时 Access Token 通常已过期，身份由 Refresh Token 证明 |
| `POST /v1/auth:logout` | 携带 Bearer（允许已过期） | 吊销当前登录链路，其 Refresh Token 全部失效 |
| 其余所有 `/v1/*` | 是 | `Authorization: Bearer <Access Token>`，**本地验签**，用户身份与平台角色可通过 `httpmiddleware.UserFromContext(ctx)` 读取（`UserID` / `Username` / `Roles`） |

两层凭证互不混用：

- **调用方凭证**（`AGENTEAM_AUTH_SERVICE_TOKEN`）：agenteam 调用 hwan 认证服务时随每个 gRPC 请求以 `authorization: Bearer ...` 携带，证明「调用者是 agenteam」。
- **用户令牌**：终端用户登录后获得。Access Token（短寿命 JWT，默认 15 分钟）放在 `Authorization` 头；Refresh Token 仅用于 `/v1/auth:refresh`。

**本地验签**：agenteam 启动后首次校验时通过 `AuthService.GetJWKS` 取得 auth 服务的公钥并缓存（10 分钟刷新，遇到未知 `kid` 立即刷新），之后每个业务请求都在本进程内完成验签（签名、`exp`、`iss`、`aud`），
**不再回源 auth 服务、不查库**。agenteam 只持有公钥，无法伪造 Token。验签代码来自独立的公共库 `github.com/hwanyan/authkit/verifier`（与 auth 服务共用同一份解析规则）。

**登出只吊销 Refresh Token**：用户登出时，agenteam 把 Access Token 交给 auth（`AuthService.Logout`），auth 吊销该登录链路下的全部 Refresh Token，之后无法再续期。
agenteam 本地**没有「已登出名单」**，也不为此依赖 Redis：已签发的 Access Token 在自然过期前（最长 `access_ttl`，默认 15 分钟）仍能通过本地验签——这是纯 JWT 方案有意接受的取舍。

账号被禁用、降权、登出对**已签发的 Access Token** 都最多滞后 `access_ttl` 生效（之后无法再刷新）。需要即时结论的敏感操作请用 `authclient.VerifyTokenStrict`（会额外回源确认账号状态）。

失败时的 HTTP 状态码：

| 情形 | 状态码 |
| --- | --- |
| 缺少 / 格式错误 / Access Token 无效或已过期（前端应先 `/v1/auth:refresh`，失败再重新登录） | `401` |
| 账号已被禁用（`/v1/auth:refresh` 或强校验时） | `403` |
| 取不到验签公钥（认证服务不可达且尚无缓存）；**调用方凭证被认证服务拒绝**（部署配置错误） | `503`（带 `Retry-After`；刻意不用 401，避免服务抖动或配置错误时把所有用户踢下线，配置错误会写 ERROR 日志） |

认证服务客户端见 `internal/authclient`，鉴权中间件见 `internal/httpmiddleware`。

**gRPC 端口同样要求 Bearer 鉴权**（`internal/grpcauth` 拦截器）。原因：Workspace 含流式接口，HTTP 网关对它不能进程内直调，
而是真实拨号到本地 gRPC 端口；HTTP 中间件放在请求 context 里的用户身份不会跨越这次调用，只有 `authorization` 头会作为 metadata 传过来，
所以 gRPC 服务端自己重新本地验签并注入身份（`internal/authctx`）。这样直连 gRPC 端口也无法绕过鉴权，业务层不必相信“上游已经验过”。
即便如此，仍建议端口只监听 loopback。

### 授权：平台角色与团队角色（两套互不相干）

| | 平台级角色 | 团队级角色 |
| --- | --- | --- |
| 取值 | `user` / `admin` | `owner` / `member` |
| 回答的问题 | 这个人是不是整个 agenteam 平台的管理员 | 这个人在**某个团队**里能做什么 |
| 存放 | hwan auth 的 `users.role`，随 Access Token 的 `roles` 声明下发（`Identity.Roles`） | agenteam 自己的 `team_members(team_id, user_id, role)` 表 |

权限矩阵（非成员访问团队资源一律返回 `NotFound`，与资源不存在无法区分，不暴露团队 / Agent / 工具是否存在）：

| 操作 | member | owner | 平台 admin |
| --- | --- | --- | --- |
| 查看团队 / Agent / MCP 工具 / 成员列表 / 聊天历史，在工作区对话 | ✅ | ✅ | ✅（越过成员校验，按 owner 对待） |
| 创建 / 修改 / 删除 Agent 与 MCP 工具，管理成员（邀请 / 改角色 / 移除他人），删除团队 | ❌ `PermissionDenied` | ✅ | ✅ |
| 移除自己（退出团队） | ✅ | ✅（团队至少保留一名 owner） | — |
| `ListTeams` | 只返回我所在的团队 | 同左 | 同左；`all=true` 可返回全部团队（非管理员传 `all=true` 得到 `PermissionDenied`） |

- 创建团队时，创建者在同一事务内被记为该团队的 `owner`；团队至少保留一名 owner（并发互相降级 / 移除也安全，见 `store/postgres/member.go`）。
- **邀请成员**按**用户名**指定（`POST /v1/teams/{team_id}/members`），直接加入、无需对方确认。auth 服务没有“按用户名查用户”的接口，
  agenteam 在用户每次通过鉴权访问时（限频）把 `user_id → username` 记入 `user_profiles` 镜像，邀请靠它解析；因此**被邀请的人必须至少登录并访问过本平台一次**。
- 成员管理接口：`GET/POST /v1/teams/{team_id}/members`、`PUT/DELETE /v1/teams/{team_id}/members/{user_id}`。
- 授权实现集中在 `internal/service/authz.go`；响应里 `Team.my_role` 给出当前用户在该团队的角色，前端据此决定是否展示编辑入口。

> **升级存量环境：** 升级前创建的团队没有任何成员，授权上线后将无人可访问。请先执行
> `psql ... -v owner_user_id=<user_id> -f scripts/postgres/20261112_team_members.sql`，
> 把所有尚无成员的团队统一指定给该用户（auth 中的 user id，即 Token 的 `sub`）作为 owner；脚本幂等，缺少参数会报错退出。

> **待办：服务间通道加固。** 当前是「静态服务凭证 + 明文 gRPC」，仅适合同一内网。后续应升级为 mTLS（或至少为 auth 的 gRPC 端口启用 TLS），避免调用方凭证被窃听。详见 `hwan/app/auth/README.md`。

### 关于 CORS

后端**不处理 CORS**，要求前端与 API 对浏览器而言始终同源：

- 生产：前端静态资源与 API 经 nginx 挂在同一域名下（`/agenteam/` 与 `/api/agenteam/`）。
- 本地开发：前端 dev server（vite）通过 `proxy` 把 `/v1` 转发到后端，浏览器只访问 `localhost:5173`。

若将来需要让其他域名的前端/第三方直接跨源调用，再引入基于精确来源白名单的 CORS 中间件。

## 已知限制 / 后续可演进方向

- MCP 工具与 Skill 目前仅作为“配置元数据”传递给 LLM 作为上下文说明，
  **尚未接入真正的 MCP 协议调用**；后续可在 `internal/runtime` 中扩展为
  真正建立 MCP Server 连接并支持 function calling。
- A2A 接入目前覆盖 Agent Card 发现、`message/send`（非流式）与 `message/stream`
  （流式，SSE）；`internal/a2a` 对 SSE 事件的解析同时兼容 A2A 1.0 规范的
  oneof 嵌套风格（`{"statusUpdate": {...}}`）与目前主流 SDK（0.2.x/0.3.x 系列）
  仍在使用的扁平 `"kind"` 判别字段风格，但尚未实现 `tasks/*`（长任务轮询/取消）
  等 A2A 规范中的其他方法；鉴权也只支持 Bearer Token，尚未支持 OAuth2/mTLS 等
  A2A SecurityScheme 的其他方案。
- `DeleteTeam` 软删除团队及其 Agent / MCP 工具（PG 单事务，成功即对外不可见），聊天记录软删除与 Agent 运行态缓存卸载
  由后台清理 worker（`internal/cleanup`）异步完成并自动重试，最终一致；软删除的数据目前不会被物理清理。
- 数据归属隔离已通过 `team_members` 团队成员体系实现（见上文“授权”）。目前的取舍：邀请成员直接加入、无需对方确认；
  “我所在的团队”之外没有更细的资源级权限（同一团队内 owner / member 两档）。
