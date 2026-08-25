-- Description: - PostgreSQL 数据库初始化脚本
-- 创建独立数据库 agenteamdb
-- 执行身份：postgres 超级用户
--   psql -U postgres -f 000_initial.sql
-- 创建数据库用户（如已存在请保持注释）
-- CREATE USER agenteam WITH PASSWORD '************';

-- 创建数据库
CREATE DATABASE agenteamdb OWNER agenteam;

-- 连接到 agenteamdb 数据库
\c agenteamdb;

-- 授予用户 agenteam 对 agenteamdb 数据库的所有权限
GRANT ALL PRIVILEGES ON DATABASE agenteamdb TO agenteam;

-- 授予用户 agenteam 对 public 模式的所有权限
GRANT ALL PRIVILEGES ON SCHEMA public TO agenteam;