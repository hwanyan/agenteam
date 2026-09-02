package service

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hwanyan/agenteam/internal/a2a"
	"github.com/hwanyan/agenteam/internal/idgen"
	"github.com/hwanyan/agenteam/internal/options"
	"github.com/hwanyan/agenteam/internal/store"
	agenteamv1 "github.com/hwanyan/agenteam/pb/gen"
)

// AgentServer 实现 AgentService。
type AgentServer struct {
	agenteamv1.UnimplementedAgentServiceServer
	*Deps
}

// NewAgentServer 创建 AgentServer。
func NewAgentServer(deps *Deps) *AgentServer {
	return &AgentServer{Deps: deps}
}

// GetAgent 按 id 查询 Agent。
func (s *AgentServer) GetAgent(ctx context.Context, req *agenteamv1.GetAgentRequest) (*agenteamv1.GetAgentResponse, error) {
	agent, err := s.Store.GetAgent(ctx, req.Id)
	if err != nil {
		return nil, agentNotFoundOrErr(err)
	}
	return &agenteamv1.GetAgentResponse{Agent: redactAgent(agent)}, nil
}

// ListAgents 返回指定团队下的全部 Agent（含主 Agent）。
func (s *AgentServer) ListAgents(ctx context.Context, req *agenteamv1.ListAgentsRequest) (*agenteamv1.ListAgentsResponse, error) {
	if _, err := s.Store.GetTeam(ctx, req.TeamId); err != nil {
		return nil, teamNotFoundOrErr(err)
	}
	agents, err := s.Store.ListAgentsByTeam(ctx, req.TeamId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "查询团队 agent 列表失败: %v", err)
	}
	for _, a := range agents {
		redactAgent(a)
	}
	return &agenteamv1.ListAgentsResponse{Agents: agents}, nil
}

// CreateAgent 在指定团队下创建一个新的非主 Agent，并触发服务端加载。
// 根据 req.Kind 支持两种创建方式：
//   - AGENT_KIND_PROMPT（默认，未指定 kind 时的兼容行为）：本地 Prompt + LLM 驱动。
//   - AGENT_KIND_A2A：通过 A2A 协议链接一个外部 Agent 提供方，仅需 name + a2a_config.endpoint_url，
//     prompt/model/mcp_tools/skills 对该方式无效。
func (s *AgentServer) CreateAgent(ctx context.Context, req *agenteamv1.CreateAgentRequest) (*agenteamv1.CreateAgentResponse, error) {
	if _, err := s.Store.GetTeam(ctx, req.TeamId); err != nil {
		return nil, teamNotFoundOrErr(err)
	}
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "agent 名称不能为空")
	}

	kind := req.Kind
	if kind == agenteamv1.AgentKind_AGENT_KIND_UNSPECIFIED {
		kind = agenteamv1.AgentKind_AGENT_KIND_PROMPT
	}

	now := time.Now().Unix()
	agent := &agenteamv1.Agent{
		Id:        idgen.New("agent"),
		TeamId:    req.TeamId,
		Name:      req.Name,
		IsMain:    false,
		Kind:      kind,
		CreatedAt: now,
		UpdatedAt: now,
	}

	switch kind {
	case agenteamv1.AgentKind_AGENT_KIND_A2A:
		if req.A2AConfig == nil || req.A2AConfig.EndpointUrl == "" {
			return nil, status.Error(codes.InvalidArgument, "A2A 接入地址（endpoint_url）不能为空")
		}
		agent.A2AConfig = &agenteamv1.A2AConfig{
			EndpointUrl: req.A2AConfig.EndpointUrl,
			AuthScheme:  req.A2AConfig.AuthScheme,
			AuthToken:   req.A2AConfig.AuthToken,
			TenantId:    req.A2AConfig.TenantId,
		}
	default:
		if req.Prompt == "" {
			return nil, status.Error(codes.InvalidArgument, "agent prompt 不能为空")
		}
		model := req.Model
		if model == "" {
			model = options.DefaultModel
		}
		agent.Prompt = req.Prompt
		agent.Model = model
		agent.McpTools = append([]string{}, req.McpTools...)
		agent.Skills = append([]string{}, req.Skills...)
	}

	agent, err := s.Runtime.Load(ctx, agent)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "加载 agent 失败: %v", err)
	}
	if err := s.Store.SaveAgent(ctx, agent); err != nil {
		return nil, status.Errorf(codes.Internal, "保存 agent 配置失败: %v", err)
	}

	return &agenteamv1.CreateAgentResponse{Agent: redactAgent(agent)}, nil
}

// DiscoverA2AAgent 探测一个 A2A 外部 Agent：发起 Agent Card 发现请求并返回展示信息，
// 不产生任何持久化副作用，供前端在创建/保存 A2A Agent 前先行校验连通性。
func (s *AgentServer) DiscoverA2AAgent(ctx context.Context, req *agenteamv1.DiscoverA2AAgentRequest) (*agenteamv1.DiscoverA2AAgentResponse, error) {
	if req.EndpointUrl == "" {
		return nil, status.Error(codes.InvalidArgument, "endpoint_url 不能为空")
	}
	card, err := s.A2A.DiscoverAgentCard(ctx, a2a.Config{
		EndpointURL: req.EndpointUrl,
		AuthScheme:  req.AuthScheme,
		AuthToken:   req.AuthToken,
		TenantID:    req.TenantId,
	})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "连接 A2A Agent 失败: %v", err)
	}
	skills := make([]string, 0, len(card.Skills))
	for _, sk := range card.Skills {
		if sk.Name != "" {
			skills = append(skills, sk.Name)
		} else {
			skills = append(skills, sk.ID)
		}
	}
	return &agenteamv1.DiscoverA2AAgentResponse{
		RemoteAgentName:   card.Name,
		RemoteDescription: card.Description,
		RemoteSkills:      skills,
		Streaming:         card.Capabilities.Streaming,
	}, nil
}

// UpdateAgent 保存 Agent 配置并触发服务端重新加载。
// Agent 的创建方式（Kind）创建后不可变更，仍按原有 Kind 对应的字段集校验：
//   - AGENT_KIND_PROMPT：沿用 name/prompt/model/mcp_tools/skills。
//   - AGENT_KIND_A2A：通过 req.A2aConfig 更新接入配置；req.A2aConfig.AuthToken 为空
//     表示“不修改现有凭证”（沿用已保存的 auth_token），非空则覆盖，避免前端每次
//     保存都必须回传明文凭证。
func (s *AgentServer) UpdateAgent(ctx context.Context, req *agenteamv1.UpdateAgentRequest) (*agenteamv1.UpdateAgentResponse, error) {
	agent, err := s.Store.GetAgent(ctx, req.Id)
	if err != nil {
		return nil, agentNotFoundOrErr(err)
	}

	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "agent 名称不能为空")
	}

	updated := &agenteamv1.Agent{
		Id:        agent.Id,
		TeamId:    agent.TeamId,
		Name:      req.Name,
		IsMain:    agent.IsMain,
		Kind:      agent.Kind,
		Version:   agent.Version,
		CreatedAt: agent.CreatedAt,
	}

	switch agent.Kind {
	case agenteamv1.AgentKind_AGENT_KIND_A2A:
		if req.A2AConfig == nil || req.A2AConfig.EndpointUrl == "" {
			return nil, status.Error(codes.InvalidArgument, "A2A 接入地址（endpoint_url）不能为空")
		}
		authToken := req.A2AConfig.AuthToken
		if authToken == "" && agent.A2AConfig != nil {
			authToken = agent.A2AConfig.AuthToken // 前端未回传凭证时，沿用已保存的值
		}
		updated.A2AConfig = &agenteamv1.A2AConfig{
			EndpointUrl: req.A2AConfig.EndpointUrl,
			AuthScheme:  req.A2AConfig.AuthScheme,
			AuthToken:   authToken,
			TenantId:    req.A2AConfig.TenantId,
		}
	default:
		if req.Prompt == "" {
			return nil, status.Error(codes.InvalidArgument, "agent prompt 不能为空")
		}
		updated.Prompt = req.Prompt
		updated.Model = req.Model
		updated.McpTools = append([]string{}, req.McpTools...)
		updated.Skills = append([]string{}, req.Skills...)
	}

	updated, err = s.Runtime.Load(ctx, updated)
	if err != nil {
		_ = s.Store.SaveAgent(ctx, updated) // 落盘 ERROR 状态，便于前端展示加载失败原因
		return nil, status.Errorf(codes.InvalidArgument, "重新加载 agent 失败: %v", err)
	}
	if err := s.Store.SaveAgent(ctx, updated); err != nil {
		return nil, status.Errorf(codes.Internal, "保存 agent 配置失败: %v", err)
	}

	return &agenteamv1.UpdateAgentResponse{Agent: redactAgent(updated)}, nil
}

// DeleteAgent 删除一个非主 Agent，并清理其运行态缓存。
// 主 Agent 与所属团队生命周期绑定，不允许通过本接口删除（返回 InvalidArgument），
// 只能随团队一起被 DeleteTeam 删除。
func (s *AgentServer) DeleteAgent(ctx context.Context, req *agenteamv1.DeleteAgentRequest) (*agenteamv1.DeleteAgentResponse, error) {
	if err := s.Store.DeleteAgent(ctx, req.Id); err != nil {
		if errors.Is(err, store.ErrMainAgentProtected) {
			return nil, status.Error(codes.InvalidArgument, "主 Agent 不能被删除")
		}
		return nil, agentNotFoundOrErr(err)
	}
	// 同步清理该 Agent 的运行态缓存，避免残留脏数据。
	_ = s.Runtime.Unload(ctx, req.Id)
	return &agenteamv1.DeleteAgentResponse{}, nil
}

// ListModelOptions 返回可选的 LLM 模型清单。
func (s *AgentServer) ListModelOptions(_ context.Context, _ *agenteamv1.ListModelOptionsRequest) (*agenteamv1.ListModelOptionsResponse, error) {
	return &agenteamv1.ListModelOptionsResponse{Models: options.Models()}, nil
}

// ListMcpToolOptions 返回可选的 MCP 工具清单。
func (s *AgentServer) ListMcpToolOptions(_ context.Context, _ *agenteamv1.ListMcpToolOptionsRequest) (*agenteamv1.ListMcpToolOptionsResponse, error) {
	return &agenteamv1.ListMcpToolOptionsResponse{Tools: options.MCPTools()}, nil
}

// ListSkillOptions 返回可选的 Skill 清单。
func (s *AgentServer) ListSkillOptions(_ context.Context, _ *agenteamv1.ListSkillOptionsRequest) (*agenteamv1.ListSkillOptionsResponse, error) {
	return &agenteamv1.ListSkillOptionsResponse{Skills: options.Skills()}, nil
}

func agentNotFoundOrErr(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return status.Error(codes.NotFound, "agent 不存在")
	}
	return status.Errorf(codes.Internal, "%v", err)
}

// redactAgent 清除响应中 A2AConfig.AuthToken 的明文值，只保留 AuthTokenSet 标记
// 告知前端"是否已配置凭证"，避免密钥随任意查询接口泄露给前端（原地修改并返回，
// 便于在 return 语句处链式调用）。
func redactAgent(agent *agenteamv1.Agent) *agenteamv1.Agent {
	if agent != nil && agent.A2AConfig != nil {
		agent.A2AConfig.AuthTokenSet = agent.A2AConfig.AuthToken != ""
		agent.A2AConfig.AuthToken = ""
	}
	return agent
}
