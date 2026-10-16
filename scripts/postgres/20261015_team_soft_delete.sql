-- 变更：团队 / Agent / 自定义 MCP 工具改为软删除。
--   - teams.deleted_at / agents.deleted_at / mcp_tools.deleted_at：软删除时间（Unix 秒），NULL 表示未删除。
--   - teams.cleaned_at：后台清理 worker 完成该团队清理（Agent 运行态卸载、聊天记录软删除）的时间。
--   - idx_teams_pending_cleanup：覆盖“已软删除、待清理”团队的部分索引，供 worker 轮询。
-- 现有数据 deleted_at / cleaned_at 均为 NULL，即视为未删除，无需回填。
--
-- 注意：软删除后团队行不再物理删除，agents / mcp_tools 的 team_id 外键 ON DELETE CASCADE 不会触发，
-- 子资源由 DeleteTeam 的同一事务显式软删除（见 internal/store/postgres/postgres.go 的 SoftDeleteTeam）。
-- 被软删除的 mcp_tools 行仍保留 api_key 明文，直到后续物理清理。

\c agenteamdb agenteam;

BEGIN;

ALTER TABLE teams     ADD COLUMN IF NOT EXISTS deleted_at BIGINT;
ALTER TABLE teams     ADD COLUMN IF NOT EXISTS cleaned_at BIGINT;
ALTER TABLE agents    ADD COLUMN IF NOT EXISTS deleted_at BIGINT;
ALTER TABLE mcp_tools ADD COLUMN IF NOT EXISTS deleted_at BIGINT;

CREATE INDEX IF NOT EXISTS idx_teams_pending_cleanup ON teams (deleted_at)
    WHERE deleted_at IS NOT NULL AND cleaned_at IS NULL;

COMMIT;
