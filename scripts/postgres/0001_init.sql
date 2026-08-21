-- Agent Runtime 平台 —— PostgreSQL 初始化脚本
--
-- 承载强关系型的配置数据：团队（teams）与 Agent（agents）。
-- 两者存在“循环引用”：team.main_agent_id -> agents.id，agent.team_id -> teams.id，
-- 因此相关外键均声明为 DEFERRABLE INITIALLY DEFERRED，允许在同一事务内以任意顺序插入，
-- 直到事务提交时才校验，从而支持“创建团队的同时原子创建其主 Agent”。
--
-- 聊天记录（ChatMessage）使用 MongoDB 存储，见 scripts/mongo/。
-- Agent 运行态热缓存使用 Redis，无需预置 schema。

BEGIN;

CREATE TYPE agent_status AS ENUM (
    'AGENT_STATUS_UNSPECIFIED',
    'AGENT_STATUS_LOADED',
    'AGENT_STATUS_RELOADING',
    'AGENT_STATUS_ERROR'
);

CREATE TABLE IF NOT EXISTS teams (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    main_agent_id TEXT NOT NULL,
    created_at    BIGINT NOT NULL,
    updated_at    BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS agents (
    id         TEXT PRIMARY KEY,
    team_id    TEXT NOT NULL,
    name       TEXT NOT NULL,
    prompt     TEXT NOT NULL DEFAULT '',
    model      TEXT NOT NULL DEFAULT '',
    mcp_tools  TEXT[] NOT NULL DEFAULT '{}',
    skills     TEXT[] NOT NULL DEFAULT '{}',
    is_main    BOOLEAN NOT NULL DEFAULT FALSE,
    version    BIGINT NOT NULL DEFAULT 0,
    status     agent_status NOT NULL DEFAULT 'AGENT_STATUS_UNSPECIFIED',
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL
);

ALTER TABLE agents
    ADD CONSTRAINT fk_agents_team
    FOREIGN KEY (team_id) REFERENCES teams (id) ON DELETE CASCADE
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE teams
    ADD CONSTRAINT fk_teams_main_agent
    FOREIGN KEY (main_agent_id) REFERENCES agents (id) ON DELETE RESTRICT
    DEFERRABLE INITIALLY DEFERRED;

CREATE INDEX IF NOT EXISTS idx_agents_team_id ON agents (team_id);
CREATE INDEX IF NOT EXISTS idx_teams_created_at ON teams (created_at);

COMMIT;
