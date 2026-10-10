// 本文件承载"自定义 MCP 工具"的 PostgreSQL 存储实现。
//
// 区别于 internal/options.MCPTools() 提供的平台内置静态清单，这里是某个团队自行
// 接入的 MCP 工具（携带 base_url / api_key / timeout_seconds 等接入参数），按团队隔离、
// 仅对归属团队的 Agent 可见可选（而非全局共享），与 teams/agents 一样属于强关系型
// 配置数据，因此同样落在 PostgreSQL。
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hwanyan/agenteam/internal/store"
	agenteamv1 "github.com/hwanyan/agenteam/pb/gen"
)

// pgUniqueViolation 是 PostgreSQL 唯一约束冲突（duplicate key）的 SQLSTATE。
const pgUniqueViolation = "23505"

// mcpToolTeamBaseURLConstraint 是 mcp_tools 表 (team_id, base_url) 唯一约束的名称，
// 与 scripts/postgres/001_schema.sql、20261010_mcp_tools_team_base_url_unique.sql 保持一致。
const mcpToolTeamBaseURLConstraint = "uq_mcp_tools_team_base_url"

// CreateMcpTool 在指定团队下接入一个自定义 MCP 工具。tool.Id/tool.TeamId 必须由调用方
// （internal/service）预先生成/校验好——默认 id 由 internal/tools.NewMCPToolID 生成随机
// 短 id，本层不负责生成默认 id，只负责落库；apiKey 明文只落库，任何查询都不会回显。
// 若 id 与已存在的自定义工具冲突，返回 store.ErrAlreadyExists；若同一团队下 base_url 已被
// 接入过，返回 store.ErrMcpToolBaseURLTaken。
func (s *Store) CreateMcpTool(ctx context.Context, tool *agenteamv1.McpTool, apiKey string) error {
	if tool.Id == "" {
		return fmt.Errorf("postgres: mcp tool id 不能为空，应由调用方生成后再落库")
	}
	if tool.TeamId == "" {
		return fmt.Errorf("postgres: mcp tool team_id 不能为空")
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO mcp_tools (id, team_id, name, base_url, api_key, timeout_seconds, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, tool.Id, tool.TeamId, tool.Name, tool.BaseUrl, apiKey, tool.TimeoutSeconds, tool.CreatedAt, tool.UpdatedAt)
	if err != nil {
		if constraint, ok := uniqueViolationConstraint(err); ok {
			if constraint == mcpToolTeamBaseURLConstraint {
				return fmt.Errorf("同一团队下已接入过相同的 base_url: %s: %w", tool.BaseUrl, store.ErrMcpToolBaseURLTaken)
			}
			return fmt.Errorf("mcp tool id 已存在: %s: %w", tool.Id, store.ErrAlreadyExists)
		}
		return fmt.Errorf("postgres: insert mcp tool: %w", err)
	}
	return nil
}

// ListMcpToolsByTeam 返回指定团队下已接入的全部自定义 MCP 工具，按创建时间升序排列；
// api_key 只转换为 ApiKeySet 标记，不返回明文。
func (s *Store) ListMcpToolsByTeam(ctx context.Context, teamID string) ([]*agenteamv1.McpTool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, team_id, name, base_url, (api_key <> ''), timeout_seconds, created_at, updated_at
		FROM mcp_tools WHERE team_id = $1 ORDER BY created_at ASC
	`, teamID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list mcp tools by team: %w", err)
	}
	defer rows.Close()

	out := make([]*agenteamv1.McpTool, 0)
	for rows.Next() {
		var t agenteamv1.McpTool
		if err := rows.Scan(&t.Id, &t.TeamId, &t.Name, &t.BaseUrl, &t.ApiKeySet, &t.TimeoutSeconds, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scan mcp tool row: %w", err)
		}
		out = append(out, &t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: iterate mcp tools: %w", err)
	}
	return out, nil
}

// GetMcpTool 按 id 查询单个自定义 MCP 工具，同时返回明文 api_key（供 service 层判断
// "更新时是否需要保留现有密钥"使用；对外响应前调用方必须自行脱敏，只暴露 ApiKeySet）。
func (s *Store) GetMcpTool(ctx context.Context, id string) (*agenteamv1.McpTool, string, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, team_id, name, base_url, api_key, timeout_seconds, created_at, updated_at
		FROM mcp_tools WHERE id = $1
	`, id)
	var t agenteamv1.McpTool
	var apiKey string
	if err := row.Scan(&t.Id, &t.TeamId, &t.Name, &t.BaseUrl, &apiKey, &t.TimeoutSeconds, &t.CreatedAt, &t.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", store.ErrNotFound
		}
		return nil, "", fmt.Errorf("postgres: scan mcp tool: %w", err)
	}
	t.ApiKeySet = apiKey != ""
	return &t, apiKey, nil
}

// UpdateMcpTool 更新一个自定义 MCP 工具的配置。apiKey 为 nil 表示"不修改现有密钥"
// （SQL 中沿用旧值），非 nil（包括空字符串，代表显式清空）则覆盖为新值。
// team_id / created_at 不可通过本方法修改（工具的归属团队在创建后固定不变）。
func (s *Store) UpdateMcpTool(ctx context.Context, tool *agenteamv1.McpTool, apiKey *string) error {
	var (
		tag pgconn.CommandTag
		err error
	)
	if apiKey != nil {
		tag, err = s.pool.Exec(ctx, `
			UPDATE mcp_tools SET name = $1, base_url = $2, api_key = $3, timeout_seconds = $4, updated_at = $5
			WHERE id = $6
		`, tool.Name, tool.BaseUrl, *apiKey, tool.TimeoutSeconds, tool.UpdatedAt, tool.Id)
	} else {
		tag, err = s.pool.Exec(ctx, `
			UPDATE mcp_tools SET name = $1, base_url = $2, timeout_seconds = $3, updated_at = $4
			WHERE id = $5
		`, tool.Name, tool.BaseUrl, tool.TimeoutSeconds, tool.UpdatedAt, tool.Id)
	}
	if err != nil {
		if constraint, ok := uniqueViolationConstraint(err); ok && constraint == mcpToolTeamBaseURLConstraint {
			return fmt.Errorf("同一团队下已接入过相同的 base_url: %s: %w", tool.BaseUrl, store.ErrMcpToolBaseURLTaken)
		}
		return fmt.Errorf("postgres: update mcp tool: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// DeleteMcpTool 在同一事务内删除一个自定义 MCP 工具，并把所属团队下所有 Agent 的
// agents.mcp_tools 里对它的引用移除。
//
// agents.mcp_tools 是 TEXT[]，只存工具 id，和 mcp_tools 表之间没有外键，数据库不会
// 自动联动；不在这里同步清理的话，Agent 配置里就会残留已不存在的工具 id。
// 受影响的 Agent 同时 version+1、刷新 updated_at（等价于一次配置重载）。
// 返回被清理了引用的 Agent id 列表；工具不存在返回 store.ErrNotFound。
func (s *Store) DeleteMcpTool(ctx context.Context, id string) ([]string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // 已 Commit 时 Rollback 为 no-op

	var teamID string
	if err := tx.QueryRow(ctx, `DELETE FROM mcp_tools WHERE id = $1 RETURNING team_id`, id).Scan(&teamID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: delete mcp tool: %w", err)
	}

	// 仅清理同团队 Agent：自定义工具按团队隔离，其他团队的 Agent 不可能绑定过它。
	rows, err := tx.Query(ctx, `
		UPDATE agents
		SET mcp_tools = array_remove(mcp_tools, $1::text), version = version + 1, updated_at = $3
		WHERE team_id = $2 AND $1::text = ANY(mcp_tools)
		RETURNING id
	`, id, teamID, time.Now().Unix())
	if err != nil {
		return nil, fmt.Errorf("postgres: clean agent mcp_tools refs: %w", err)
	}
	affected := make([]string, 0)
	for rows.Next() {
		var agentID string
		if err := rows.Scan(&agentID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("postgres: scan affected agent id: %w", err)
		}
		affected = append(affected, agentID)
	}
	rows.Close() // 必须在 Commit 前读完并关闭，否则同一连接上的后续操作会失败
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: iterate affected agents: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: commit delete mcp tool: %w", err)
	}
	return affected, nil
}

// uniqueViolationConstraint 判断错误是否为 PostgreSQL 唯一约束冲突，并返回触发冲突的
// 约束名（用于区分"id 主键重复"与"(team_id, base_url) 唯一约束冲突"两种语义不同的冲突）。
func uniqueViolationConstraint(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return pgErr.ConstraintName, true
	}
	return "", false
}
