-- 回滚脚本：按依赖关系反向删除，便于本地开发重建 schema。
-- 它不会删除 agenteamdb 这个数据库本身，也不会删除 agenteam 用户，
-- 只是把 001_schema.sql 建出来的表结构清空，方便改完 001_schema.sql（比如加字段、调索引）
-- 后，先跑一遍 002_drop.sql 清掉旧表，再重新跑 001_schema.sql 建出新表结构——本质是给
-- 开发调试用的，生产环境不应该跑这个脚本（会丢数据）。
BEGIN;

ALTER TABLE IF EXISTS teams DROP CONSTRAINT IF EXISTS fk_teams_main_agent;
ALTER TABLE IF EXISTS agents DROP CONSTRAINT IF EXISTS fk_agents_team;

DROP TABLE IF EXISTS agents;
DROP TABLE IF EXISTS teams;
DROP TYPE IF EXISTS agent_status;
DROP TYPE IF EXISTS agent_kind;

COMMIT;
