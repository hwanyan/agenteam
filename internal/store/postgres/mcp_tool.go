// 本文件承载"自定义 MCP 工具"的 PostgreSQL 存储实现。
//
// 区别于 internal/options.MCPTools() 提供的平台内置静态清单，这里是用户运行时
// 自行接入的 MCP 工具（携带 base_url / api_key / timeout_seconds 等接入参数），
// 全局共享、不归属于某个团队/Agent，与 teams/agents 一样属于强关系型配置数据，
// 因此同样落在 PostgreSQL。
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hwanyan/agenteam/internal/store"
	agenteamv1 "github.com/hwanyan/agenteam/pb/gen"
)

// pgUniqueViolation 是 PostgreSQL 唯一约束冲突（duplicate key）的 SQLSTATE。
const pgUniqueViolation = "23505"

// CreateMcpTool 接入一个自定义 MCP 工具。tool.Id 必须由调用方（internal/service）
// 预先生成好——默认 id 由 internal/tools.NewMCPToolID 生成随机短 id，本层不负责
// 生成默认 id，只负责落库；apiKey 明文只落库，任何查询都不会回显。
// 若 id 与已存在的自定义工具冲突，返回 store.ErrAlreadyExists。
func (s *Store) CreateMcpTool(ctx context.Context, tool *agenteamv1.McpTool, apiKey string) error {
	if tool.Id == "" {
		return fmt.Errorf("postgres: mcp tool id 不能为空，应由调用方生成后再落库")
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO mcp_tools (id, name, base_url, api_key, timeout_seconds, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, tool.Id, tool.Name, tool.BaseUrl, apiKey, tool.TimeoutSeconds, tool.CreatedAt, tool.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("mcp tool id 已存在: %s: %w", tool.Id, store.ErrAlreadyExists)
		}
		return fmt.Errorf("postgres: insert mcp tool: %w", err)
	}
	return nil
}

// ListMcpTools 返回全部已接入的自定义 MCP 工具，按创建时间升序排列；
// api_key 只转换为 ApiKeySet 标记，不返回明文。
func (s *Store) ListMcpTools(ctx context.Context) ([]*agenteamv1.McpTool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, base_url, (api_key <> ''), timeout_seconds, created_at, updated_at
		FROM mcp_tools ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("postgres: list mcp tools: %w", err)
	}
	defer rows.Close()

	out := make([]*agenteamv1.McpTool, 0)
	for rows.Next() {
		var t agenteamv1.McpTool
		if err := rows.Scan(&t.Id, &t.Name, &t.BaseUrl, &t.ApiKeySet, &t.TimeoutSeconds, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scan mcp tool row: %w", err)
		}
		out = append(out, &t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: iterate mcp tools: %w", err)
	}
	return out, nil
}

// isUniqueViolation 判断错误是否为 PostgreSQL 唯一约束冲突（如 id 主键重复）。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == pgUniqueViolation
	}
	return false
}
