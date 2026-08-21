// Package runtime 模拟 Agent 的“加载 / 重新加载”生命周期。
//
// 当前平台中 Agent 的执行本质上是“按最新配置实时组装 prompt 后调用 LLM”，
// 并不存在需要长期驻留的进程或连接；但为了让“保存后服务端重新加载 Agent”
// 这一产品语义有明确的代码落点（并为后续引入真正的常驻执行体、
// 工具/技能预热等能力预留扩展点），这里用一个独立的 Manager 来承载
// “加载”动作：校验配置合法性、生成新版本号、并将编译后的运行态配置写入
// Redis（internal/cache），从而在多实例部署下也能共享同一份“当前生效配置”。
package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/hwanyan/agenteam/internal/cache"
	"github.com/hwanyan/agenteam/internal/options"
	agenteamv1 "github.com/hwanyan/agenteam/pb/gen"
)

// Manager 管理所有已加载的 Agent 运行态，底层由 Redis 承载。
type Manager struct {
	cache cache.AgentCache
}

// New 创建一个 Manager。
func New(c cache.AgentCache) *Manager {
	return &Manager{cache: c}
}

// Load 校验并加载给定的 Agent 配置，成功后置为 LOADED 状态、版本号自增，
// 并作为当前生效的运行态写入 Redis 缓存；失败则置为 ERROR 状态并返回错误。
// 返回值即为更新后的 Agent（调用方应据此持久化到 Store）。
func (m *Manager) Load(ctx context.Context, agent *agenteamv1.Agent) (*agenteamv1.Agent, error) {
	if err := validate(agent); err != nil {
		agent.Status = agenteamv1.AgentStatus_AGENT_STATUS_ERROR
		return agent, err
	}

	agent.Version++
	agent.Status = agenteamv1.AgentStatus_AGENT_STATUS_LOADED
	agent.UpdatedAt = time.Now().Unix()

	if err := m.cache.Set(ctx, toSnapshot(agent)); err != nil {
		return agent, fmt.Errorf("runtime: 写入运行态缓存失败: %w", err)
	}
	return agent, nil
}

// Get 返回当前生效的运行态配置。
func (m *Manager) Get(ctx context.Context, agentID string) (*agenteamv1.Agent, bool, error) {
	snapshot, ok, err := m.cache.Get(ctx, agentID)
	if err != nil || !ok {
		return nil, false, err
	}
	return fromSnapshot(snapshot), true, nil
}

// Unload 从运行态缓存中移除指定 Agent（如 Agent/团队被删除时调用）。
func (m *Manager) Unload(ctx context.Context, agentID string) error {
	return m.cache.Delete(ctx, agentID)
}

func toSnapshot(a *agenteamv1.Agent) *cache.AgentSnapshot {
	return &cache.AgentSnapshot{
		ID:        a.Id,
		TeamID:    a.TeamId,
		Name:      a.Name,
		Prompt:    a.Prompt,
		Model:     a.Model,
		McpTools:  a.McpTools,
		Skills:    a.Skills,
		IsMain:    a.IsMain,
		Version:   a.Version,
		Status:    a.Status.String(),
		CreatedAt: a.CreatedAt,
		UpdatedAt: a.UpdatedAt,
	}
}

func fromSnapshot(s *cache.AgentSnapshot) *agenteamv1.Agent {
	return &agenteamv1.Agent{
		Id:        s.ID,
		TeamId:    s.TeamID,
		Name:      s.Name,
		Prompt:    s.Prompt,
		Model:     s.Model,
		McpTools:  s.McpTools,
		Skills:    s.Skills,
		IsMain:    s.IsMain,
		Version:   s.Version,
		Status:    agenteamv1.AgentStatus(agenteamv1.AgentStatus_value[s.Status]),
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
	}
}

func validate(agent *agenteamv1.Agent) error {
	if agent.Name == "" {
		return fmt.Errorf("agent 名称不能为空")
	}
	if agent.Prompt == "" {
		return fmt.Errorf("agent prompt 不能为空")
	}
	if !options.IsValidModel(agent.Model) {
		return fmt.Errorf("不支持的模型: %s", agent.Model)
	}
	for _, t := range agent.McpTools {
		if !options.IsValidTool(t) {
			return fmt.Errorf("不支持的 MCP 工具: %s", t)
		}
	}
	for _, s := range agent.Skills {
		if !options.IsValidSkill(s) {
			return fmt.Errorf("不支持的 Skill: %s", s)
		}
	}
	return nil
}
