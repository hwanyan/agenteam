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

// Store 是业务层依赖的统一存储接口。
type Store interface {
	// CreateTeam 原子性地创建团队及其主 Agent（两者互相引用，需保证一致性）。
	CreateTeam(ctx context.Context, team *pb.Team, mainAgent *pb.Agent) error
	// GetTeam 按 id 查询团队。
	GetTeam(ctx context.Context, id string) (*pb.Team, error)
	// ListTeams 返回全部团队，按创建时间升序排列。
	ListTeams(ctx context.Context) ([]*pb.Team, error)
	// DeleteTeam 删除团队及其全部 Agent、聊天记录。
	DeleteTeam(ctx context.Context, id string) error

	// SaveAgent 新增或更新一个 Agent（保存配置并落库最新 version/status）。
	SaveAgent(ctx context.Context, agent *pb.Agent) error
	// GetAgent 按 id 查询 Agent。
	GetAgent(ctx context.Context, id string) (*pb.Agent, error)
	// DeleteAgent 删除一个非主 Agent；若目标是主 Agent 则返回 ErrMainAgentProtected。
	DeleteAgent(ctx context.Context, id string) error

	// AppendMessage 追加一条工作区聊天记录。
	AppendMessage(ctx context.Context, msg *pb.ChatMessage) error
	// ListMessages 返回指定团队的历史聊天记录，按时间升序排列。
	ListMessages(ctx context.Context, teamID string) ([]*pb.ChatMessage, error)
}

// TeamAgentStore 是 Store 中“团队 / Agent”相关能力的子接口，由 PostgreSQL 实现。
type TeamAgentStore interface {
	CreateTeam(ctx context.Context, team *pb.Team, mainAgent *pb.Agent) error
	GetTeam(ctx context.Context, id string) (*pb.Team, error)
	ListTeams(ctx context.Context) ([]*pb.Team, error)
	// DeleteTeamAndAgents 仅删除关系型库中的团队与其 Agent（不涉及聊天记录），由上层组合实现负责协调跨库删除。
	DeleteTeamAndAgents(ctx context.Context, id string) error
	SaveAgent(ctx context.Context, agent *pb.Agent) error
	GetAgent(ctx context.Context, id string) (*pb.Agent, error)
	DeleteAgent(ctx context.Context, id string) error
}

// MessageStore 是 Store 中“聊天记录”相关能力的子接口，由 MongoDB 实现。
type MessageStore interface {
	AppendMessage(ctx context.Context, msg *pb.ChatMessage) error
	ListMessages(ctx context.Context, teamID string) ([]*pb.ChatMessage, error)
	// DeleteByTeam 删除指定团队的全部聊天记录。
	DeleteByTeam(ctx context.Context, teamID string) error
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

// DeleteTeam 删除团队及其 Agent（PostgreSQL，级联）与聊天记录（MongoDB）。
// 两个数据库之间没有分布式事务，这里采用“先删关系型主数据，再清理聊天记录”的顺序：
// 万一后半步失败，至多残留孤立的聊天记录，不会影响团队/Agent 数据的一致性，
// 属于可接受的最终一致性权衡。
func (c *composite) DeleteTeam(ctx context.Context, id string) error {
	if err := c.teams.DeleteTeamAndAgents(ctx, id); err != nil {
		return err
	}
	return c.messages.DeleteByTeam(ctx, id)
}

func (c *composite) SaveAgent(ctx context.Context, agent *pb.Agent) error {
	return c.teams.SaveAgent(ctx, agent)
}

func (c *composite) GetAgent(ctx context.Context, id string) (*pb.Agent, error) {
	return c.teams.GetAgent(ctx, id)
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
