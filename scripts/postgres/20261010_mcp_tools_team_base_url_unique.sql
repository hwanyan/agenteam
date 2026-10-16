-- 变更：mcp_tools 表新增 (team_id, base_url) 唯一约束——一个团队下同一个 base_url 只能被引入一次。
-- （2026-10-16 起该约束已被 20261016_mcp_tools_base_url_partial_unique.sql 改为部分唯一索引，
--  新环境直接跑 001_schema.sql 即可，无需执行本脚本。）
-- 约束名 uq_mcp_tools_team_base_url 与后端 internal/store/postgres/mcp_tool.go 中的
-- mcpToolTeamBaseURLConstraint 保持一致（后端据此区分 "id 冲突" 与 "base_url 重复"）。
--
-- 命名规范：自 2026-10-10 起，所有对已有表结构的 DDL 变更脚本统一命名为
-- 20261010_***_***.sql（日期_变更简写），001_schema.sql 只保留最新的完整建表语句。
--
-- 执行前请先确认没有历史重复数据，否则 ADD CONSTRAINT 会失败：
--   SELECT team_id, base_url, count(*) FROM mcp_tools GROUP BY team_id, base_url HAVING count(*) > 1;
-- 若有结果，请先手动删除/修改重复行再执行本脚本。

\c agenteamdb agenteam;

BEGIN;

ALTER TABLE mcp_tools
    ADD CONSTRAINT uq_mcp_tools_team_base_url UNIQUE (team_id, base_url);

COMMIT;
