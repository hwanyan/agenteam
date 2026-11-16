// 本文件承载“团队成员 / 团队内角色 / 用户名镜像”的 PostgreSQL 存储实现（store.MemberStore）。
//
// 这里的 role 是团队级角色（owner / member），与 auth 服务写入 Access Token 的平台级角色
// （user / admin）是两套互不相干的概念。
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hwanyan/agenteam/internal/store"
	agenteamv1 "github.com/hwanyan/agenteam/pb/gen"
)

// team_members.role 的取值，与 scripts/postgres/001_schema.sql 的 CHECK 约束保持一致。
const (
	roleOwner  = "owner"
	roleMember = "member"
)

func roleToDB(r agenteamv1.TeamRole) (string, error) {
	switch r {
	case agenteamv1.TeamRole_TEAM_ROLE_OWNER:
		return roleOwner, nil
	case agenteamv1.TeamRole_TEAM_ROLE_MEMBER:
		return roleMember, nil
	default:
		return "", fmt.Errorf("postgres: 不支持的团队角色: %s", r)
	}
}

func roleFromDB(s string) agenteamv1.TeamRole {
	switch s {
	case roleOwner:
		return agenteamv1.TeamRole_TEAM_ROLE_OWNER
	case roleMember:
		return agenteamv1.TeamRole_TEAM_ROLE_MEMBER
	default:
		return agenteamv1.TeamRole_TEAM_ROLE_UNSPECIFIED
	}
}

// ListTeamsByUser 返回用户所在的全部团队（排除已软删除），按创建时间升序，MyRole 已按该用户填充。
func (s *Store) ListTeamsByUser(ctx context.Context, userID string) ([]*agenteamv1.Team, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.name, t.main_agent_id, t.created_at, t.updated_at, m.role
		FROM team_members m
		JOIN teams t ON t.id = m.team_id
		WHERE m.user_id = $1 AND t.deleted_at IS NULL
		ORDER BY t.created_at ASC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list teams by user: %w", err)
	}
	defer rows.Close()

	out := make([]*agenteamv1.Team, 0)
	for rows.Next() {
		var (
			t    agenteamv1.Team
			role string
		)
		if err := rows.Scan(&t.Id, &t.Name, &t.MainAgentId, &t.CreatedAt, &t.UpdatedAt, &role); err != nil {
			return nil, fmt.Errorf("postgres: scan team row: %w", err)
		}
		t.MyRole = roleFromDB(role)
		out = append(out, &t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: iterate teams by user: %w", err)
	}
	return out, nil
}

// GetMemberRole 返回用户在团队内的角色；团队不存在（含已软删除）或用户不是成员返回 store.ErrNotFound。
func (s *Store) GetMemberRole(ctx context.Context, teamID, userID string) (agenteamv1.TeamRole, error) {
	var role string
	err := s.pool.QueryRow(ctx, `
		SELECT m.role
		FROM team_members m
		JOIN teams t ON t.id = m.team_id
		WHERE m.team_id = $1 AND m.user_id = $2 AND t.deleted_at IS NULL
	`, teamID, userID).Scan(&role)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return agenteamv1.TeamRole_TEAM_ROLE_UNSPECIFIED, store.ErrNotFound
		}
		return agenteamv1.TeamRole_TEAM_ROLE_UNSPECIFIED, fmt.Errorf("postgres: get member role: %w", err)
	}
	return roleFromDB(role), nil
}

// ListTeamMembers 返回团队成员：owner 在前，其余按加入时间升序；用户名取自 user_profiles 镜像，可能为空。
func (s *Store) ListTeamMembers(ctx context.Context, teamID string) ([]*agenteamv1.TeamMember, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.user_id, COALESCE(p.username, ''), m.role, m.created_at
		FROM team_members m
		JOIN teams t ON t.id = m.team_id
		LEFT JOIN user_profiles p ON p.user_id = m.user_id
		WHERE m.team_id = $1 AND t.deleted_at IS NULL
		ORDER BY (m.role = 'owner') DESC, m.created_at ASC, m.user_id ASC
	`, teamID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list team members: %w", err)
	}
	defer rows.Close()

	out := make([]*agenteamv1.TeamMember, 0)
	for rows.Next() {
		var (
			m    agenteamv1.TeamMember
			role string
		)
		if err := rows.Scan(&m.UserId, &m.Username, &role, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scan team member row: %w", err)
		}
		m.Role = roleFromDB(role)
		out = append(out, &m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: iterate team members: %w", err)
	}
	return out, nil
}

// AddTeamMember 把用户加入团队。团队不存在（含已软删除）返回 store.ErrNotFound，
// 用户已是成员返回 store.ErrAlreadyExists（不会覆盖其现有角色）。
func (s *Store) AddTeamMember(ctx context.Context, teamID, userID string, role agenteamv1.TeamRole) error {
	dbRole, err := roleToDB(role)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO team_members (team_id, user_id, role, created_at, updated_at)
		SELECT $1, $2, $3, $4, $4
		WHERE EXISTS (SELECT 1 FROM teams WHERE id = $1 AND deleted_at IS NULL)
	`, teamID, userID, dbRole, time.Now().Unix())
	if err != nil {
		if _, ok := uniqueViolationConstraint(err); ok {
			return store.ErrAlreadyExists
		}
		return fmt.Errorf("postgres: add team member: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// UpdateTeamMemberRole 修改成员角色。成员不存在返回 store.ErrNotFound；
// 若会让团队失去最后一个 owner，返回 store.ErrLastOwner。
func (s *Store) UpdateTeamMemberRole(ctx context.Context, teamID, userID string, role agenteamv1.TeamRole) error {
	dbRole, err := roleToDB(role)
	if err != nil {
		return err
	}
	return s.withOwnerGuard(ctx, teamID, userID, func(tx pgx.Tx, currentRole string, owners int) error {
		if currentRole == roleOwner && dbRole != roleOwner && owners <= 1 {
			return store.ErrLastOwner
		}
		if _, err := tx.Exec(ctx, `
			UPDATE team_members SET role = $3, updated_at = $4 WHERE team_id = $1 AND user_id = $2
		`, teamID, userID, dbRole, time.Now().Unix()); err != nil {
			return fmt.Errorf("postgres: update team member role: %w", err)
		}
		return nil
	})
}

// RemoveTeamMember 把成员移出团队。成员不存在返回 store.ErrNotFound；移除最后一个 owner 返回 store.ErrLastOwner。
func (s *Store) RemoveTeamMember(ctx context.Context, teamID, userID string) error {
	return s.withOwnerGuard(ctx, teamID, userID, func(tx pgx.Tx, currentRole string, owners int) error {
		if currentRole == roleOwner && owners <= 1 {
			return store.ErrLastOwner
		}
		if _, err := tx.Exec(ctx, `DELETE FROM team_members WHERE team_id = $1 AND user_id = $2`, teamID, userID); err != nil {
			return fmt.Errorf("postgres: remove team member: %w", err)
		}
		return nil
	})
}

// withOwnerGuard 在一个事务内先锁定该团队全部 owner 行，再锁定目标成员行，然后把目标成员当前的角色与
// 团队 owner 数量交给 fn 判断并执行修改。
//
// “团队至少保留一个 owner”是跨行不变量：若只在应用层先查后改，两个 owner 同时互相降级/移除会双双通过检查、
// 最终团队没有 owner。这里用 SELECT ... FOR UPDATE 串行化同一团队的此类操作（READ COMMITTED 下，后到者
// 等锁释放后会重新读取已变更的行，看到真实的 owner 数量）。
// 目标成员不存在（或团队已软删除）返回 store.ErrNotFound。
func (s *Store) withOwnerGuard(ctx context.Context, teamID, userID string, fn func(tx pgx.Tx, currentRole string, owners int) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // 已 Commit 时 Rollback 为 no-op

	rows, err := tx.Query(ctx, `
		SELECT m.user_id
		FROM team_members m
		JOIN teams t ON t.id = m.team_id
		WHERE m.team_id = $1 AND m.role = 'owner' AND t.deleted_at IS NULL
		ORDER BY m.user_id
		FOR UPDATE OF m
	`, teamID)
	if err != nil {
		return fmt.Errorf("postgres: lock team owners: %w", err)
	}
	owners := 0
	for rows.Next() {
		owners++
	}
	rows.Close() // 必须在同一连接上执行下一条语句前关闭
	if err := rows.Err(); err != nil {
		return fmt.Errorf("postgres: iterate team owners: %w", err)
	}

	var currentRole string
	err = tx.QueryRow(ctx, `
		SELECT m.role
		FROM team_members m
		JOIN teams t ON t.id = m.team_id
		WHERE m.team_id = $1 AND m.user_id = $2 AND t.deleted_at IS NULL
		FOR UPDATE OF m
	`, teamID, userID).Scan(&currentRole)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrNotFound
		}
		return fmt.Errorf("postgres: lock team member: %w", err)
	}

	if err := fn(tx, currentRole, owners); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit member change: %w", err)
	}
	return nil
}

// UpsertUserProfile 记录或刷新 user_id 与用户名的对应关系。
func (s *Store) UpsertUserProfile(ctx context.Context, userID, username string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO user_profiles (user_id, username, updated_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id) DO UPDATE SET username = EXCLUDED.username, updated_at = EXCLUDED.updated_at
	`, userID, username, time.Now().Unix()); err != nil {
		return fmt.Errorf("postgres: upsert user profile: %w", err)
	}
	return nil
}

// FindUserIDByUsername 按用户名（不区分大小写）解析 user_id；没有记录返回 store.ErrNotFound。
// 若镜像里同一用户名对应多个 user_id（auth 侧用户名被复用），取最近刷新的那个。
func (s *Store) FindUserIDByUsername(ctx context.Context, username string) (string, error) {
	var userID string
	err := s.pool.QueryRow(ctx, `
		SELECT user_id FROM user_profiles
		WHERE lower(username) = lower($1)
		ORDER BY updated_at DESC
		LIMIT 1
	`, username).Scan(&userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", store.ErrNotFound
		}
		return "", fmt.Errorf("postgres: find user by username: %w", err)
	}
	return userID, nil
}
