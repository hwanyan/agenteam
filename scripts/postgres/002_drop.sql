-- 回滚脚本：按依赖关系反向删除，便于本地开发重建 schema。
-- 它不会删除 agenteamdb 这个数据库本身，也不会删除 agenteam 用户，
-- 只是把 001_schema.sql 建出来的表结构清空，方便改完 001_schema.sql（比如加字段、调索引）
-- 后，先跑一遍 002_drop.sql 清掉旧表，再重新跑 001_schema.sql 建出新表结构——本质是给
-- 开发调试用的，生产环境不应该跑这个脚本（会丢数据）。
BEGIN;

-- agents.team_id / mcp_tools.team_id 的外键约束是建表时内联声明的，
-- 随 DROP TABLE 自动一并删除，这里不需要单独 ALTER TABLE ... DROP CONSTRAINT。
DROP TABLE IF EXISTS agents;
DROP TABLE IF EXISTS teams;
DROP TABLE IF EXISTS mcp_tools;
DROP TYPE IF EXISTS agent_status;
DROP TYPE IF EXISTS agent_kind;

COMMIT;
