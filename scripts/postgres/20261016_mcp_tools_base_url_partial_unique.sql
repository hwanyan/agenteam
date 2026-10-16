-- 变更：mcp_tools 的 (team_id, base_url) 唯一约束改为部分唯一索引（WHERE deleted_at IS NULL）。
-- 配合 DeleteMcpTool 软删除：软删除的行不再占用 base_url，同一团队可以用相同 base_url 重新接入。
-- 索引名保持 uq_mcp_tools_team_base_url，后端据此区分 "id 冲突" 与 "base_url 重复"。
--
-- 前置条件：需先执行 20261015_team_soft_delete.sql（提供 mcp_tools.deleted_at 列）。
-- 约束与索引在同一事务内切换，不会出现无约束的窗口。

\c agenteamdb agenteam;

BEGIN;

ALTER TABLE mcp_tools DROP CONSTRAINT IF EXISTS uq_mcp_tools_team_base_url;

CREATE UNIQUE INDEX IF NOT EXISTS uq_mcp_tools_team_base_url
    ON mcp_tools (team_id, base_url) WHERE deleted_at IS NULL;

COMMIT;
