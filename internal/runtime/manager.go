// Package runtime 模拟 Agent 的"加载 / 重新加载"生命周期。
//
// 当前平台中 Agent 的执行本质上是"按最新配置实时组装 prompt 后调用 LLM"
// （PROMPT 方式）或"转发消息给外部 A2A Agent"（A2A 方式），并不存在需要长期驻留的
// 进程或连接；但为了让"保存后服务端重新加载 Agent"这一产品语义有明确的代码落点
// （并为后续引入真正的常驻执行体、工具/技能预热等能力预留扩展点），这里用一个独立
// 的 Manager 来承载"加载"动作：校验配置合法性（对 A2A 方式还会实际探测对端连通性）、
// 生成新版本号、并将编译后的运行态配置写入 Redis（internal/cache），从而在多实例
// 部署下也能共享同一份"当前生效配置"。
package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/hwanyan/agenteam/internal/a2a"
	"github.com/hwanyan/agenteam/internal/cache"
	"github.com/hwanyan/agenteam/internal/options"
	agenteamv1 "github.com/hwanyan/agenteam/pb/gen"
)

// mcpToolLister 是 Manager 校验自定义 MCP 工具 id 合法性时依赖的最小接口
// （只声明 Load 校验环节真正需要的方法，而非直接依赖完整的 store.Store，
// 保持 runtime 包职责单一、便于测试替身）。store.Store 已实现该方法，
// main.go 中可直接把组合好的 store.Store 传给 New。
type mcpToolLister interface {
	ListMcpToolsByTeam(ctx context.Context, teamID string) ([]*agenteamv1.McpTool, error)
}

// Manager 管理所有已加载的 Agent 运行态，底层由 Redis 承载。
type Manager struct {
	cache    cache.AgentCache
	a2a      *a2a.Client
	mcpTools mcpToolLister
}

// New 创建一个 Manager。mcpTools 用于在校验 Agent.McpTools 时识别用户自行接入的
// 自定义 MCP 工具 id（内置静态清单以外的部分），不能为 nil。
func New(c cache.AgentCache, mcpTools mcpToolLister) *Manager {
	return &Manager{cache: c, a2a: a2a.New(), mcpTools: mcpTools}
}

// Load 校验并加载给定的 Agent 配置，成功后置为 LOADED 状态、版本号自增，
// 并作为当前生效的运行态写入 Redis 缓存；失败则置为 ERROR 状态并返回错误。
// 返回值即为更新后的 Agent（调用方应据此持久化到 Store）。
//
// 对 kind=AGENT_KIND_A2A 的 Agent，加载过程还会向 A2AConfig.EndpointUrl 发起一次
// Agent Card 发现请求以校验连通性，并将对端回填的名称/描述/技能/流式能力写回
// agent.A2aConfig 的只读字段。
func (m *Manager) Load(ctx context.Context, agent *agenteamv1.Agent) (*agenteamv1.Agent, error) {
	if err := m.validate(ctx, agent); err != nil {
		agent.Status = agenteamv1.AgentStatus_AGENT_STATUS_ERROR
		return agent, err
	}

	if agent.Kind == agenteamv1.AgentKind_AGENT_KIND_A2A {
		if err := m.discoverAndFillA2A(ctx, agent); err != nil {
			agent.Status = agenteamv1.AgentStatus_AGENT_STATUS_ERROR
			return agent, err
		}
	}

	agent.Version++
	agent.Status = agenteamv1.AgentStatus_AGENT_STATUS_LOADED
	agent.UpdatedAt = time.Now().Unix()

	if err := m.cache.Set(ctx, toSnapshot(agent)); err != nil {
		return agent, fmt.Errorf("runtime: 写入运行态缓存失败: %w", err)
	}
	return agent, nil
}

// discoverAndFillA2A 向外部 A2A Agent 发起 Agent Card 发现请求，校验其可达性，
// 并将回填信息写入 agent.A2aConfig 的只读字段。
func (m *Manager) discoverAndFillA2A(ctx context.Context, agent *agenteamv1.Agent) error {
	cfg := agent.A2AConfig
	if cfg == nil || cfg.EndpointUrl == "" {
		return fmt.Errorf("A2A 接入地址（endpoint_url）不能为空")
	}
	card, err := m.a2a.DiscoverAgentCard(ctx, a2a.Config{
		EndpointURL: cfg.EndpointUrl,
		AuthScheme:  cfg.AuthScheme,
		AuthToken:   cfg.AuthToken,
		TenantID:    cfg.TenantId,
	})
	if err != nil {
		return fmt.Errorf("连接 A2A Agent 失败: %w", err)
	}
	cfg.RemoteAgentName = card.Name
	cfg.RemoteDescription = card.Description
	cfg.Streaming = card.Capabilities.Streaming
	skills := make([]string, 0, len(card.Skills))
	for _, sk := range card.Skills {
		if sk.Name != "" {
			skills = append(skills, sk.Name)
		} else {
			skills = append(skills, sk.ID)
		}
	}
	cfg.RemoteSkills = skills
	cfg.AuthTokenSet = cfg.AuthToken != ""
	return nil
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
	s := &cache.AgentSnapshot{
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
		Kind:      a.Kind.String(),
	}
	if a.A2AConfig != nil {
		s.A2A = &cache.A2ASnapshot{
			EndpointURL:       a.A2AConfig.EndpointUrl,
			AuthScheme:        a.A2AConfig.AuthScheme,
			AuthToken:         a.A2AConfig.AuthToken,
			TenantID:          a.A2AConfig.TenantId,
			RemoteAgentName:   a.A2AConfig.RemoteAgentName,
			RemoteDescription: a.A2AConfig.RemoteDescription,
			RemoteSkills:      a.A2AConfig.RemoteSkills,
			Streaming:         a.A2AConfig.Streaming,
		}
	}
	return s
}

func fromSnapshot(s *cache.AgentSnapshot) *agenteamv1.Agent {
	a := &agenteamv1.Agent{
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
		Kind:      agenteamv1.AgentKind(agenteamv1.AgentKind_value[s.Kind]),
	}
	if s.A2A != nil {
		a.A2AConfig = &agenteamv1.A2AConfig{
			EndpointUrl:       s.A2A.EndpointURL,
			AuthScheme:        s.A2A.AuthScheme,
			AuthToken:         s.A2A.AuthToken,
			AuthTokenSet:      s.A2A.AuthToken != "",
			TenantId:          s.A2A.TenantID,
			RemoteAgentName:   s.A2A.RemoteAgentName,
			RemoteDescription: s.A2A.RemoteDescription,
			RemoteSkills:      s.A2A.RemoteSkills,
			Streaming:         s.A2A.Streaming,
		}
	}
	return a
}

// validate 按 Agent.Kind 校验配置合法性：
//   - AGENT_KIND_PROMPT（默认，含未指定 kind 的兼容旧数据场景）：沿用原有的
//     name/prompt/model/mcp_tools/skills 校验；mcp_tools 除平台内置静态清单
//     （internal/options.MCPTools()）外，还接受该 Agent 所属团队自行接入的自定义
//     MCP 工具 id（通过 m.mcpTools 按 agent.TeamId 动态查询，见 store.Store.
//     ListMcpToolsByTeam；自定义工具按团队隔离，不允许绑定其他团队接入的工具）。
//   - AGENT_KIND_A2A：只校验 name 与 endpoint_url，其余 prompt 相关字段对该
//     方式无意义，允许为空；真正的连通性校验发生在 discoverAndFillA2A 中。
func (m *Manager) validate(ctx context.Context, agent *agenteamv1.Agent) error {
	if agent.Name == "" {
		return fmt.Errorf("agent 名称不能为空")
	}

	if agent.Kind == agenteamv1.AgentKind_AGENT_KIND_A2A {
		if agent.A2AConfig == nil || agent.A2AConfig.EndpointUrl == "" {
			return fmt.Errorf("A2A 接入地址（endpoint_url）不能为空")
		}
		return nil
	}

	// AGENT_KIND_PROMPT（含 AGENT_KIND_UNSPECIFIED，兼容未显式传 kind 的旧客户端）。
	if agent.Prompt == "" {
		return fmt.Errorf("agent prompt 不能为空")
	}
	if !options.IsValidModel(agent.Model) {
		return fmt.Errorf("不支持的模型: %s", agent.Model)
	}
	if len(agent.McpTools) > 0 {
		custom, err := m.mcpTools.ListMcpToolsByTeam(ctx, agent.TeamId)
		if err != nil {
			return fmt.Errorf("查询自定义 MCP 工具失败: %w", err)
		}
		customIDs := make(map[string]bool, len(custom))
		for _, t := range custom {
			customIDs[t.Id] = true
		}
		for _, t := range agent.McpTools {
			if !options.IsValidTool(t) && !customIDs[t] {
				return fmt.Errorf("不支持的 MCP 工具: %s", t)
			}
		}
	}
	for _, s := range agent.Skills {
		if !options.IsValidSkill(s) {
			return fmt.Errorf("不支持的 Skill: %s", s)
		}
	}
	return nil
}
