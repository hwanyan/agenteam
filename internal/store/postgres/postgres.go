// Package postgres 提供 teams / agents 的 PostgreSQL 存储实现。
//
// team 与其主 agent 存在循环引用（team.main_agent_id -> agents.id，
// agent.team_id -> teams.id），对应的外键在 scripts/postgres/0001_init.sql 中
// 声明为 DEFERRABLE INITIALLY DEFERRED，因此可以在同一事务内先插入 team、
// 再插入 agent（或反之），提交时才校验引用完整性。
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hwanyan/agenteam/internal/store"
	agenteamv1 "github.com/hwanyan/agenteam/pb/gen"
)

// Store 是 store.TeamAgentStore 的 PostgreSQL 实现。
type Store struct {
	pool *pgxpool.Pool
}

// execer 是 *pgxpool.Pool 与 pgx.Tx 的公共子集，用于在“单条语句”与
// “事务内多条语句”两种场景下复用同一段写入逻辑。
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// New 创建 PostgreSQL Store，并做一次连通性检查。
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close 关闭连接池。
func (s *Store) Close() {
	s.pool.Close()
}

// CreateTeam 在同一事务内原子创建团队及其主 Agent。
func (s *Store) CreateTeam(ctx context.Context, team *agenteamv1.Team, mainAgent *agenteamv1.Agent) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // 已 Commit 时 Rollback 为 no-op

	if _, err := tx.Exec(ctx, `
		INSERT INTO teams (id, name, main_agent_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5)
	`, team.Id, team.Name, team.MainAgentId, team.CreatedAt, team.UpdatedAt); err != nil {
		return fmt.Errorf("postgres: insert team: %w", err)
	}

	if err := insertAgent(ctx, tx, mainAgent); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit create team: %w", err)
	}
	return nil
}

// GetTeam 按 id 查询团队。
func (s *Store) GetTeam(ctx context.Context, id string) (*agenteamv1.Team, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, name, main_agent_id, created_at, updated_at FROM teams WHERE id = $1
	`, id)
	var t agenteamv1.Team
	if err := row.Scan(&t.Id, &t.Name, &t.MainAgentId, &t.CreatedAt, &t.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: scan team: %w", err)
	}
	return &t, nil
}

// ListTeams 返回全部团队，按创建时间升序排列。
func (s *Store) ListTeams(ctx context.Context) ([]*agenteamv1.Team, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, main_agent_id, created_at, updated_at FROM teams ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("postgres: list teams: %w", err)
	}
	defer rows.Close()

	out := make([]*agenteamv1.Team, 0)
	for rows.Next() {
		var t agenteamv1.Team
		if err := rows.Scan(&t.Id, &t.Name, &t.MainAgentId, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scan team row: %w", err)
		}
		out = append(out, &t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: iterate teams: %w", err)
	}
	return out, nil
}

// DeleteTeamAndAgents 删除团队（其下 Agent 通过外键 ON DELETE CASCADE 自动清理）。
func (s *Store) DeleteTeamAndAgents(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM teams WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("postgres: delete team: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// SaveAgent 新增或更新一个 Agent。
func (s *Store) SaveAgent(ctx context.Context, agent *agenteamv1.Agent) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO agents (id, team_id, name, prompt, model, mcp_tools, skills, is_main, version, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (id) DO UPDATE SET
			name       = EXCLUDED.name,
			prompt     = EXCLUDED.prompt,
			model      = EXCLUDED.model,
			mcp_tools  = EXCLUDED.mcp_tools,
			skills     = EXCLUDED.skills,
			version    = EXCLUDED.version,
			status     = EXCLUDED.status,
			updated_at = EXCLUDED.updated_at
	`,
		agent.Id, agent.TeamId, agent.Name, agent.Prompt, agent.Model,
		agent.McpTools, agent.Skills, agent.IsMain, agent.Version, agent.Status.String(),
		agent.CreatedAt, agent.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: upsert agent: %w", err)
	}
	return nil
}

// GetAgent 按 id 查询 Agent。
func (s *Store) GetAgent(ctx context.Context, id string) (*agenteamv1.Agent, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, team_id, name, prompt, model, mcp_tools, skills, is_main, version, status, created_at, updated_at
		FROM agents WHERE id = $1
	`, id)
	return scanAgent(row)
}

// ListAgentsByTeam 返回指定团队下的全部 Agent（含主 Agent），主 Agent 排在最前，
// 其余按创建时间升序排列。
func (s *Store) ListAgentsByTeam(ctx context.Context, teamID string) ([]*agenteamv1.Agent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, team_id, name, prompt, model, mcp_tools, skills, is_main, version, status, created_at, updated_at
		FROM agents WHERE team_id = $1 ORDER BY is_main DESC, created_at ASC
	`, teamID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list agents by team: %w", err)
	}
	defer rows.Close()

	out := make([]*agenteamv1.Agent, 0)
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: iterate agents: %w", err)
	}
	return out, nil
}

func scanAgent(row pgx.Row) (*agenteamv1.Agent, error) {
	var (
		a         agenteamv1.Agent
		statusStr string
	)
	if err := row.Scan(
		&a.Id, &a.TeamId, &a.Name, &a.Prompt, &a.Model,
		&a.McpTools, &a.Skills, &a.IsMain, &a.Version, &statusStr,
		&a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: scan agent: %w", err)
	}
	a.Status = agenteamv1.AgentStatus(agenteamv1.AgentStatus_value[statusStr])
	return &a, nil
}

// DeleteAgent 删除一个非主 Agent。主 Agent（is_main=true）受 SQL 条件保护，
// 即使调用方传入主 Agent 的 id，也不会被删除，而是返回 store.ErrMainAgentProtected，
// 从数据库层面兜底“主 Agent 不可单独删除”这一约束，不依赖业务层校验。
func (s *Store) DeleteAgent(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM agents WHERE id = $1 AND is_main = FALSE`, id)
	if err != nil {
		return fmt.Errorf("postgres: delete agent: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	// 未删除任何行：要么 agent 不存在，要么是主 Agent 被 is_main = FALSE 条件挡住。
	_, err = s.GetAgent(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.ErrNotFound
		}
		return err
	}
	return store.ErrMainAgentProtected
}

func insertAgent(ctx context.Context, q execer, agent *agenteamv1.Agent) error {
	_, err := q.Exec(ctx, `
		INSERT INTO agents (id, team_id, name, prompt, model, mcp_tools, skills, is_main, version, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`,
		agent.Id, agent.TeamId, agent.Name, agent.Prompt, agent.Model,
		agent.McpTools, agent.Skills, agent.IsMain, agent.Version, agent.Status.String(),
		agent.CreatedAt, agent.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: insert agent: %w", err)
	}
	return nil
}
