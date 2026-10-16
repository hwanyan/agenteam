// Package store 定义 Agent Runtime 平台的数据存储接口。
//
// 存储按数据形态拆分到不同数据库（详见 scripts/README.md）：
//   - PostgreSQL：teams / agents，强关系型配置数据（internal/store/postgres）。
//   - MongoDB：chat_messages，按团队追加、按时间读取的日志型数据（internal/store/mongostore）。
//   - Redis：Agent 运行态热缓存，属于 internal/cache 而非本包（不是“持久化配置”，是运行态）。
//
// 业务层只依赖本包定义的 Store 接口，具体由哪些数据库承载对业务层透明。
package store

import (
	"context"
	"errors"

	pb "github.com/hwanyan/agenteam/pb/gen"
)

// ErrNotFound 表示目标资源不存在。
var ErrNotFound = errors.New("not found")

// ErrMainAgentProtected 表示尝试单独删除主 Agent。
// 主 Agent 的生命周期与所属团队绑定（team.main_agent_id 引用它），
// 不允许通过 DeleteAgent 单独删除，只能随 DeleteTeam 一起删除。
var ErrMainAgentProtected = errors.New("main agent cannot be deleted individually")

// ErrAlreadyExists 表示尝试创建的资源 id 已存在（如自定义 MCP 工具 id 冲突）。
var ErrAlreadyExists = errors.New("already exists")

// ErrMcpToolBaseURLTaken 表示同一团队下已经接入过相同 base_url 的自定义 MCP 工具
// （对应 mcp_tools 表 (team_id, base_url) 唯一约束）：一个团队下同一个 base_url 只能被引入一次。
var ErrMcpToolBaseURLTaken = errors.New("mcp tool base_url already exists in team")

// Store 是业务层依赖的统一存储接口。
type Store interface {
	// CreateTeam 原子性地创建团队及其主 Agent（两者互相引用，需保证一致性）。
	CreateTeam(ctx context.Context, team *pb.Team, mainAgent *pb.Agent) error
	// GetTeam 按 id 查询团队。
	GetTeam(ctx context.Context, id string) (*pb.Team, error)
	// ListTeams 返回全部团队，按创建时间升序排列。
	ListTeams(ctx context.Context) ([]*pb.Team, error)
	// DeleteTeam 软删除团队及其全部 Agent、自定义 MCP 工具（单个 PG 事务，成功即对外不可见）。
	// 聊天记录的软删除与运行态缓存卸载不在此同步完成，而是由后台清理 worker
	// （internal/cleanup）通过 ListDeletedTeamsPendingCleanup / CleanupDeletedTeam 异步完成。
	DeleteTeam(ctx context.Context, id string) error
	// ListDeletedTeamsPendingCleanup 返回已软删除、尚未完成清理的团队（按删除时间升序，最多 limit 个）。
	ListDeletedTeamsPendingCleanup(ctx context.Context, limit int) ([]*DeletedTeam, error)
	// CleanupDeletedTeam 软删除指定团队的聊天记录，并把团队标记为清理完成。幂等，失败后可安全重试。
	CleanupDeletedTeam(ctx context.Context, id string) error
	// SaveAgent 新增或更新一个 Agent（保存配置并落库最新 version/status）。
	SaveAgent(ctx context.Context, agent *pb.Agent) error
	// GetAgent 按 id 查询 Agent。
	GetAgent(ctx context.Context, id string) (*pb.Agent, error)
	// ListAgentsByTeam 返回指定团队下的全部 Agent（含主 Agent），按创建时间升序排列。
	ListAgentsByTeam(ctx context.Context, teamID string) ([]*pb.Agent, error)
	// DeleteAgent 删除一个非主 Agent；若目标是主 Agent 则返回 ErrMainAgentProtected。
	DeleteAgent(ctx context.Context, id string) error
	// CreateMcpTool 在指定团队下接入一个自定义 MCP 工具；tool.Id 必须由调用方
	// （internal/service，通过 internal/tools.NewMCPToolID）预先生成好。apiKey 为明文，
	// 仅落库使用，任何查询接口都不会回显（对应 tool.ApiKeySet 只读标记）。若 id 与已存在的
	// 自定义工具冲突，返回 ErrAlreadyExists。
	CreateMcpTool(ctx context.Context, tool *pb.McpTool, apiKey string) error
	// ListMcpToolsByTeam 返回指定团队下已接入的全部自定义 MCP 工具（不含
	// internal/options 提供的平台内置静态清单），按创建时间升序排列。
	// 自定义 MCP 工具按团队隔离，不同团队互不可见。
	ListMcpToolsByTeam(ctx context.Context, teamID string) ([]*pb.McpTool, error)
	// GetMcpTool 按 id 查询单个自定义 MCP 工具（含明文 api_key，仅供服务层内部读取/更新时
	// 判断"是否需要保留现有密钥"使用；对外响应前必须调用方自行脱敏）。
	GetMcpTool(ctx context.Context, id string) (*pb.McpTool, string, error)
	// UpdateMcpTool 更新一个自定义 MCP 工具的配置；apiKey 为空指针表示"不修改现有密钥"，
	// 非空则覆盖为新值（区分"传空字符串以清空密钥"与"未提供密钥"两种语义）。
	UpdateMcpTool(ctx context.Context, tool *pb.McpTool, apiKey *string) error
	// DeleteMcpTool 软删除一个自定义 MCP 工具（释放其 base_url，同团队可重新接入），并在同一事务内把所属团队下所有 Agent 的
	// agents.mcp_tools 里对它的引用一并移除（该数组只存 id、没有外键，不清理就会残留失效 id）。
	// 返回被清理了引用的 Agent id 列表；工具不存在返回 ErrNotFound。
	DeleteMcpTool(ctx context.Context, id string) (affectedAgentIDs []string, err error)

	// AppendMessage 追加一条工作区聊天记录。
	AppendMessage(ctx context.Context, msg *pb.ChatMessage) error
	// ListMessages 返回指定团队的历史聊天记录，按时间升序排列。
	ListMessages(ctx context.Context, teamID string) ([]*pb.ChatMessage, error)
}

// TeamAgentStore 是 Store 中“团队 / Agent / 自定义 MCP 工具”相关能力的子接口，由 PostgreSQL 实现。
type TeamAgentStore interface {
	CreateTeam(ctx context.Context, team *pb.Team, mainAgent *pb.Agent) error
	GetTeam(ctx context.Context, id string) (*pb.Team, error)
	ListTeams(ctx context.Context) ([]*pb.Team, error)
	// SoftDeleteTeam 仅软删除关系型库中的团队与其 Agent（不涉及聊天记录），不存在或已删除返回 ErrNotFound。
	SoftDeleteTeam(ctx context.Context, id string) error
	// ListDeletedTeamsPendingCleanup 返回已软删除、尚未完成清理的团队及其全部 Agent id。
	ListDeletedTeamsPendingCleanup(ctx context.Context, limit int) ([]*DeletedTeam, error)
	// MarkTeamCleaned 标记团队清理完成（幂等）。
	MarkTeamCleaned(ctx context.Context, id string) error
	SaveAgent(ctx context.Context, agent *pb.Agent) error
	GetAgent(ctx context.Context, id string) (*pb.Agent, error)
	ListAgentsByTeam(ctx context.Context, teamID string) ([]*pb.Agent, error)
	DeleteAgent(ctx context.Context, id string) error
	CreateMcpTool(ctx context.Context, tool *pb.McpTool, apiKey string) error
	ListMcpToolsByTeam(ctx context.Context, teamID string) ([]*pb.McpTool, error)
	GetMcpTool(ctx context.Context, id string) (*pb.McpTool, string, error)
	UpdateMcpTool(ctx context.Context, tool *pb.McpTool, apiKey *string) error
	DeleteMcpTool(ctx context.Context, id string) ([]string, error)
}

// MessageStore 是 Store 中“聊天记录”相关能力的子接口，由 MongoDB 实现。
type MessageStore interface {
	AppendMessage(ctx context.Context, msg *pb.ChatMessage) error
	ListMessages(ctx context.Context, teamID string) ([]*pb.ChatMessage, error)
	// SoftDeleteByTeam 软删除指定团队的全部聊天记录（幂等）；被软删除的记录不会再被 ListMessages 返回。
	SoftDeleteByTeam(ctx context.Context, teamID string) error
}

// DeletedTeam 是一个已软删除、等待后台清理的团队。
type DeletedTeam struct {
	ID string
	// AgentIDs 是该团队下全部 Agent 的 id（含已软删除的），用于清理运行态缓存。
	AgentIDs []string
}

// composite 将“关系型库(Team/Agent)”与“文档库(ChatMessage)”组合为统一的 Store，
// 对跨库操作（如删除团队需要同时清理聊天记录）做协调。
type composite struct {
	teams    TeamAgentStore
	messages MessageStore
}

// New 组合 PostgreSQL 与 MongoDB 两个子存储，构建统一的 Store。
func New(teams TeamAgentStore, messages MessageStore) Store {
	return &composite{teams: teams, messages: messages}
}

func (c *composite) CreateTeam(ctx context.Context, team *pb.Team, mainAgent *pb.Agent) error {
	return c.teams.CreateTeam(ctx, team, mainAgent)
}

func (c *composite) GetTeam(ctx context.Context, id string) (*pb.Team, error) {
	return c.teams.GetTeam(ctx, id)
}

func (c *composite) ListTeams(ctx context.Context) ([]*pb.Team, error) {
	return c.teams.ListTeams(ctx)
}

// DeleteTeam 软删除团队及其 Agent（PostgreSQL 单事务）。
// 两个数据库之间没有分布式事务，因此这里只做 PG 一侧：PG 里 teams.deleted_at 本身就是
// “待清理”的可靠记录，聊天记录（MongoDB）的软删除交给后台 worker 通过
// ListDeletedTeamsPendingCleanup + CleanupDeletedTeam 异步完成并重试，最终一致，
// 不再有“PG 已删、Mongo 失败后无人重试”的问题。
func (c *composite) DeleteTeam(ctx context.Context, id string) error {
	return c.teams.SoftDeleteTeam(ctx, id)
}

func (c *composite) ListDeletedTeamsPendingCleanup(ctx context.Context, limit int) ([]*DeletedTeam, error) {
	return c.teams.ListDeletedTeamsPendingCleanup(ctx, limit)
}

// CleanupDeletedTeam 先软删除聊天记录，成功后才标记团队清理完成：
// 任何一步失败都不会标记，团队会留在待清理队列里等待下一轮重试；两步均幂等。
func (c *composite) CleanupDeletedTeam(ctx context.Context, id string) error {
	if err := c.messages.SoftDeleteByTeam(ctx, id); err != nil {
		return err
	}
	return c.teams.MarkTeamCleaned(ctx, id)
}

func (c *composite) SaveAgent(ctx context.Context, agent *pb.Agent) error {
	return c.teams.SaveAgent(ctx, agent)
}

func (c *composite) GetAgent(ctx context.Context, id string) (*pb.Agent, error) {
	return c.teams.GetAgent(ctx, id)
}

func (c *composite) ListAgentsByTeam(ctx context.Context, teamID string) ([]*pb.Agent, error) {
	return c.teams.ListAgentsByTeam(ctx, teamID)
}

func (c *composite) DeleteAgent(ctx context.Context, id string) error {
	return c.teams.DeleteAgent(ctx, id)
}

func (c *composite) AppendMessage(ctx context.Context, msg *pb.ChatMessage) error {
	return c.messages.AppendMessage(ctx, msg)
}

func (c *composite) ListMessages(ctx context.Context, teamID string) ([]*pb.ChatMessage, error) {
	return c.messages.ListMessages(ctx, teamID)
}

func (c *composite) CreateMcpTool(ctx context.Context, tool *pb.McpTool, apiKey string) error {
	return c.teams.CreateMcpTool(ctx, tool, apiKey)
}

func (c *composite) ListMcpToolsByTeam(ctx context.Context, teamID string) ([]*pb.McpTool, error) {
	return c.teams.ListMcpToolsByTeam(ctx, teamID)
}

func (c *composite) GetMcpTool(ctx context.Context, id string) (*pb.McpTool, string, error) {
	return c.teams.GetMcpTool(ctx, id)
}

func (c *composite) UpdateMcpTool(ctx context.Context, tool *pb.McpTool, apiKey *string) error {
	return c.teams.UpdateMcpTool(ctx, tool, apiKey)
}

func (c *composite) DeleteMcpTool(ctx context.Context, id string) ([]string, error) {
	return c.teams.DeleteMcpTool(ctx, id)
}
