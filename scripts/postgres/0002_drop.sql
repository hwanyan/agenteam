-- 回滚脚本：按依赖关系反向删除，便于本地开发重建 schema。
BEGIN;

ALTER TABLE IF EXISTS teams DROP CONSTRAINT IF EXISTS fk_teams_main_agent;
ALTER TABLE IF EXISTS agents DROP CONSTRAINT IF EXISTS fk_agents_team;

DROP TABLE IF EXISTS agents;
DROP TABLE IF EXISTS teams;
DROP TYPE IF EXISTS agent_status;

COMMIT;
