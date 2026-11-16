-- 变更：新增团队成员体系——team_members（团队内角色）与 user_profiles（用户名镜像），并把存量团队回填到指定用户名下。
--
--   - team_members：(team_id, user_id, role)，role 为团队级角色 owner / member，由 agenteam 自己维护；
--     与 auth 服务 Access Token 里的平台级角色（user / admin）是两套互不相干的概念。
--   - user_profiles：user_id -> username 的最小镜像。auth 服务没有“按用户名查用户”的接口，
--     邀请成员靠它把用户名解析成 user_id；之后每次该用户通过鉴权访问本服务时会自动刷新。
--   - 回填：升级前创建的团队没有任何成员，授权上线后将无人可访问。本脚本把所有“尚无成员”的未删除团队
--     统一指定给 :owner_user_id 作为 owner。该用户的用户名会在其下次登录访问后由服务自动补进 user_profiles，
--     在此之前成员列表里该行的用户名为空。
--
-- 用法（owner_user_id 为 auth 服务中用户的 id，即 Access Token 的 sub，必填；需要 PostgreSQL 13+ 的 psql）：
--   psql "postgres://agenteam:***@localhost:5432/agenteamdb" -v owner_user_id='<user_id>' \
--        -f scripts/postgres/20261112_team_members.sql
--
-- 幂等：表/索引用 IF NOT EXISTS；回填只针对“尚无任何成员”的团队，重复执行不会重复写入，
-- 也不会覆盖已手工调整过的成员关系。

\set ON_ERROR_STOP on

-- 缺少参数时以非 0 退出码失败（而不是静默退出），避免自动化部署把“什么都没做”误判为成功。
\if :{?owner_user_id}
\else
  \echo '缺少参数 owner_user_id，示例：psql ... -v owner_user_id=<user_id> -f scripts/postgres/20261112_team_members.sql'
  DO $$ BEGIN RAISE EXCEPTION '缺少参数 owner_user_id'; END $$;
\endif

\c agenteamdb agenteam;

BEGIN;

CREATE TABLE IF NOT EXISTS team_members (
    team_id    TEXT   NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    user_id    TEXT   NOT NULL,
    role       TEXT   NOT NULL CHECK (role IN ('owner', 'member')),
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    PRIMARY KEY (team_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_team_members_user_id ON team_members (user_id);

CREATE TABLE IF NOT EXISTS user_profiles (
    user_id    TEXT PRIMARY KEY,
    username   TEXT   NOT NULL,
    updated_at BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_user_profiles_username ON user_profiles (lower(username));

-- 存量团队回填：尚无成员的未删除团队 -> owner_user_id 作为 owner。
INSERT INTO team_members (team_id, user_id, role, created_at, updated_at)
SELECT t.id, :'owner_user_id', 'owner', extract(epoch FROM now())::bigint, extract(epoch FROM now())::bigint
FROM teams t
WHERE t.deleted_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM team_members m WHERE m.team_id = t.id);

COMMIT;
