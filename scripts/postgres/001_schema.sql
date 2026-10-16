-- Agent Runtime 平台 —— PostgreSQL 初始化脚本
--
-- 承载强关系型的配置数据：团队（teams）与 Agent（agents）。
-- 本脚本只包含建表语句（CREATE TYPE / CREATE TABLE / CREATE INDEX），不使用
-- ALTER TABLE：
--   - agents.team_id / mcp_tools.team_id 直接在建表时内联 REFERENCES teams (id)，
--     因为 teams 表总是先于它们创建，无需事后补约束。
--   - teams.main_agent_id 不声明外键约束：team 与其主 agent 存在"循环引用"
--     （team.main_agent_id -> agents.id，agent.team_id -> teams.id），而 agents
--     表要晚于 teams 表创建，建表语句里无法内联引用一个还不存在的表，若要同时
--     满足该约束只能事后用 ALTER TABLE 补一条 DEFERRABLE 外键。这里选择直接不做
--     该约束：这一不变量（主 Agent 必定存在）已经由应用层保证——CreateTeam 在
--     同一事务内原子创建团队及其主 Agent；DeleteAgent 显式拒绝删除 is_main=true
--     的 Agent，只能随 DeleteTeam 一起删除（见
--     internal/store/postgres/postgres.go）。
--
-- 聊天记录（ChatMessage）使用 MongoDB 存储，见 scripts/mongo/。
-- Agent 运行态热缓存使用 Redis，无需预置 schema。

-- 以 agenteam 身份连接到 agenteamdb 数据库
\c agenteamdb agenteam;

BEGIN;

CREATE TYPE agent_status AS ENUM (
    'AGENT_STATUS_UNSPECIFIED',
    'AGENT_STATUS_LOADED',
    'AGENT_STATUS_RELOADING',
    'AGENT_STATUS_ERROR'
);

-- Agent 的创建/接入方式：本地 Prompt+LLM 驱动，或通过 A2A 协议链接外部 Agent。
CREATE TYPE agent_kind AS ENUM (
    'AGENT_KIND_UNSPECIFIED',
    'AGENT_KIND_PROMPT',
    'AGENT_KIND_A2A'
);

CREATE TABLE IF NOT EXISTS teams (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    main_agent_id TEXT NOT NULL,
    created_at    BIGINT NOT NULL,
    updated_at    BIGINT NOT NULL,
    -- 软删除：DeleteTeam 只写 deleted_at（Unix 秒），所有读取均过滤 deleted_at IS NULL。
    deleted_at    BIGINT,
    -- 清理完成标记：后台清理 worker（internal/cleanup）完成 Agent 运行态卸载、聊天记录软删除后写入，
    -- deleted_at IS NOT NULL AND cleaned_at IS NULL 即“待清理”队列。
    cleaned_at    BIGINT
);

CREATE TABLE IF NOT EXISTS agents (
    id         TEXT PRIMARY KEY,
    team_id    TEXT NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    prompt     TEXT NOT NULL DEFAULT '',
    model      TEXT NOT NULL DEFAULT '',
    mcp_tools  TEXT[] NOT NULL DEFAULT '{}',
    skills     TEXT[] NOT NULL DEFAULT '{}',
    is_main    BOOLEAN NOT NULL DEFAULT FALSE,
    version    BIGINT NOT NULL DEFAULT 0,
    status     agent_status NOT NULL DEFAULT 'AGENT_STATUS_UNSPECIFIED',
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    kind       agent_kind NOT NULL DEFAULT 'AGENT_KIND_PROMPT',
    -- 仅 kind = 'AGENT_KIND_A2A' 时非空，承载对外部 A2A Agent 的接入配置
    -- （endpoint_url / auth_scheme / auth_token）与只读展示信息
    -- （remote_agent_name / remote_description / remote_skills / streaming）。
    a2a_config JSONB,
    -- 软删除：随团队软删除一起写入（Unix 秒），所有读取均过滤 deleted_at IS NULL。
    deleted_at BIGINT
);

CREATE INDEX IF NOT EXISTS idx_agents_team_id ON agents (team_id);
CREATE INDEX IF NOT EXISTS idx_teams_created_at ON teams (created_at);
-- 部分索引：只覆盖“已软删除、待清理”的团队，供清理 worker 轮询。
CREATE INDEX IF NOT EXISTS idx_teams_pending_cleanup ON teams (deleted_at)
    WHERE deleted_at IS NOT NULL AND cleaned_at IS NULL;

-- 自定义 MCP 工具：按团队隔离，仅对归属团队的 Agent 可见可选（与内置静态清单
-- internal/options.MCPTools() 合并后一起展示），不同团队互不可见。
-- id 为空时由服务端（internal/service.AgentServer.CreateMcpTool）通过
-- internal/tools.NewMCPToolID 生成 "mcp_tool_<6位随机数字/字母>" 形式的默认值
-- 后再落库，本表不负责生成默认 id。
CREATE TABLE IF NOT EXISTS mcp_tools (
    id              TEXT PRIMARY KEY,
    team_id         TEXT NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    base_url        TEXT NOT NULL,
    api_key         TEXT NOT NULL DEFAULT '',
    timeout_seconds INTEGER NOT NULL DEFAULT 30,
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL,
    -- 软删除：随团队软删除一起写入（Unix 秒），所有读取均过滤 deleted_at IS NULL。
    deleted_at      BIGINT
);

-- 一个团队下同一个 base_url 只能被引入一次（后端据此索引名区分 id 冲突与 base_url 重复）。
-- 部分唯一索引：只约束未软删除的行，软删除后同一团队可用相同 base_url 重新接入。
CREATE UNIQUE INDEX IF NOT EXISTS uq_mcp_tools_team_base_url
    ON mcp_tools (team_id, base_url) WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_mcp_tools_team_id ON mcp_tools (team_id);
CREATE INDEX IF NOT EXISTS idx_mcp_tools_created_at ON mcp_tools (created_at);

COMMIT;
