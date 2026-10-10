package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hwanyan/agenteam/internal/a2a"
	"github.com/hwanyan/agenteam/internal/options"
	"github.com/hwanyan/agenteam/internal/store"
	"github.com/hwanyan/agenteam/internal/tools"
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
		Id:        tools.New("agent"),
		TeamId:    req.TeamId,
		Name:      req.Name,
		IsMain:    false,
		Kind:      kind,
		McpTools:  []string{}, // AGENT_KIND_A2A 对该字段无意义，保持非 nil 空切片而非 nil
		Skills:    []string{}, // 同上，避免 pgx 把 nil slice 编码为 SQL NULL 违反 NOT NULL 约束
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
		McpTools:  []string{}, // AGENT_KIND_A2A 对该字段无意义，保持非 nil 空切片而非 nil
		Skills:    []string{}, // 同上，避免 pgx 把 nil slice 编码为 SQL NULL 违反 NOT NULL 约束
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

// ListMcpToolOptions 返回可选的 MCP 工具清单：平台内置静态清单
// （internal/options.MCPTools()）与指定团队自行接入的自定义 MCP 工具
// （Store.ListMcpToolsByTeam）合并后的结果；自定义工具的 Description 展示其
// base_url，IsCustom 标记为 true，方便前端区分展示"编辑"入口。自定义工具按团队
// 隔离，不同团队互不可见。
//
// req.TeamId 允许为空：用于"新增团队"表单场景（团队尚未创建，无 team_id 可传），
// 此时只返回平台内置静态清单，不含任何自定义工具（因为自定义工具此时也确实不可能
// 归属于一个还不存在的团队）。
func (s *AgentServer) ListMcpToolOptions(ctx context.Context, req *agenteamv1.ListMcpToolOptionsRequest) (*agenteamv1.ListMcpToolOptionsResponse, error) {
	merged := append([]*agenteamv1.ToolOption{}, options.MCPTools()...)
	if req.TeamId == "" {
		return &agenteamv1.ListMcpToolOptionsResponse{Tools: merged}, nil
	}
	if _, err := s.Store.GetTeam(ctx, req.TeamId); err != nil {
		return nil, teamNotFoundOrErr(err)
	}
	custom, err := s.Store.ListMcpToolsByTeam(ctx, req.TeamId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "查询自定义 MCP 工具失败: %v", err)
	}
	for _, t := range custom {
		merged = append(merged, &agenteamv1.ToolOption{Id: t.Id, Name: t.Name, Description: t.BaseUrl, IsCustom: true})
	}
	return &agenteamv1.ListMcpToolOptionsResponse{Tools: merged}, nil
}

// ListSkillOptions 返回可选的 Skill 清单。
func (s *AgentServer) ListSkillOptions(_ context.Context, _ *agenteamv1.ListSkillOptionsRequest) (*agenteamv1.ListSkillOptionsResponse, error) {
	return &agenteamv1.ListSkillOptionsResponse{Skills: options.Skills()}, nil
}

// CreateMcpTool 在指定团队下接入一个新的自定义 MCP 工具；成功后会与内置工具合并
// 出现在该团队的 ListMcpToolOptions 返回的可选清单中，仅供该团队的 Agent 选择绑定
// （按团队隔离，不同团队互不可见）。
//
// 字段规则：
//   - team_id 必填，必须是已存在的团队。
//   - id 不对外开放指定入口，始终由服务端通过 internal/tools.NewMCPToolID 生成
//     "mcp_tool_<6位随机数字/字母>" 形式的 id（如 "mcp_tool_sc1so3"）。
//   - name / base_url 必填。
//   - api_key 非必填；服务端不会在任何响应中回显明文，只反映 ApiKeySet 标记。
//   - timeout_seconds 非必填，留空(0)则默认 30 秒。
func (s *AgentServer) CreateMcpTool(ctx context.Context, req *agenteamv1.CreateMcpToolRequest) (*agenteamv1.CreateMcpToolResponse, error) {
	if req.TeamId == "" {
		return nil, status.Error(codes.InvalidArgument, "team_id 不能为空")
	}
	if _, err := s.Store.GetTeam(ctx, req.TeamId); err != nil {
		return nil, teamNotFoundOrErr(err)
	}
	// 首尾空白不参与比较：避免 " https://a/mcp" 与 "https://a/mcp" 绕过 (team_id, base_url) 唯一约束。
	name := strings.TrimSpace(req.Name)
	baseURL := strings.TrimSpace(req.BaseUrl)
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "MCP 工具名称不能为空")
	}
	if baseURL == "" {
		return nil, status.Error(codes.InvalidArgument, "base_url 不能为空")
	}

	timeout := req.TimeoutSeconds
	if timeout <= 0 {
		timeout = options.DefaultMcpToolTimeoutSeconds
	}

	now := time.Now().Unix()
	tool := &agenteamv1.McpTool{
		TeamId:         req.TeamId,
		Name:           name,
		BaseUrl:        baseURL,
		TimeoutSeconds: timeout,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	// id 始终由 tools.NewMCPToolID 随机生成（36^6 ≈ 21.8 亿的随机空间，
	// 理论上仍存在极小概率的冲突），遇到 id 冲突（store.ErrAlreadyExists）时重新生成并重试；
	// 同一团队下 base_url 重复（store.ErrMcpToolBaseURLTaken）重试无意义，直接返回给用户。
	const maxAutoIDAttempts = 10
	for attempt := 1; ; attempt++ {
		tool.Id = tools.NewMCPToolID("mcp_tool")
		err := s.Store.CreateMcpTool(ctx, tool, req.ApiKey)
		if err == nil {
			break
		}
		if errors.Is(err, store.ErrMcpToolBaseURLTaken) {
			return nil, status.Error(codes.AlreadyExists, "该团队下已接入过相同 base_url 的 MCP 工具，同一个 base_url 只能引入一次")
		}
		if !errors.Is(err, store.ErrAlreadyExists) {
			return nil, status.Errorf(codes.Internal, "接入 MCP 工具失败: %v", err)
		}
		if attempt >= maxAutoIDAttempts {
			return nil, status.Errorf(codes.Internal, "生成唯一 id 失败，请重试: %v", err)
		}
	}
	tool.ApiKeySet = req.ApiKey != ""
	return &agenteamv1.CreateMcpToolResponse{Tool: tool}, nil
}

// GetMcpTool 按 id 查询单个自定义 MCP 工具的详情，供前端编辑表单回填；
// api_key 不会在响应中回显明文，只反映 ApiKeySet 标记。
func (s *AgentServer) GetMcpTool(ctx context.Context, req *agenteamv1.GetMcpToolRequest) (*agenteamv1.GetMcpToolResponse, error) {
	tool, _, err := s.Store.GetMcpTool(ctx, req.Id)
	if err != nil {
		return nil, mcpToolNotFoundOrErr(err)
	}
	return &agenteamv1.GetMcpToolResponse{Tool: tool}, nil
}

// UpdateMcpTool 更新一个自定义 MCP 工具的配置信息（名称/base_url/密钥/超时时间）；
// 工具的归属团队（team_id）创建后不可变更。api_key 为空表示"不修改现有密钥"（沿用
// 已保存的值），非空则覆盖，避免前端每次保存都必须回传明文密钥。
func (s *AgentServer) UpdateMcpTool(ctx context.Context, req *agenteamv1.UpdateMcpToolRequest) (*agenteamv1.UpdateMcpToolResponse, error) {
	existing, _, err := s.Store.GetMcpTool(ctx, req.Id)
	if err != nil {
		return nil, mcpToolNotFoundOrErr(err)
	}
	name := strings.TrimSpace(req.Name)
	baseURL := strings.TrimSpace(req.BaseUrl)
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "MCP 工具名称不能为空")
	}
	if baseURL == "" {
		return nil, status.Error(codes.InvalidArgument, "base_url 不能为空")
	}

	timeout := req.TimeoutSeconds
	if timeout <= 0 {
		timeout = options.DefaultMcpToolTimeoutSeconds
	}

	updated := &agenteamv1.McpTool{
		Id:             existing.Id,
		TeamId:         existing.TeamId,
		Name:           name,
		BaseUrl:        baseURL,
		TimeoutSeconds: timeout,
		CreatedAt:      existing.CreatedAt,
		UpdatedAt:      time.Now().Unix(),
	}

	var apiKey *string
	if req.ApiKey != "" {
		apiKey = &req.ApiKey
	}
	if err := s.Store.UpdateMcpTool(ctx, updated, apiKey); err != nil {
		return nil, mcpToolNotFoundOrErr(err)
	}
	if apiKey != nil {
		updated.ApiKeySet = *apiKey != ""
	} else {
		updated.ApiKeySet = existing.ApiKeySet
	}
	return &agenteamv1.UpdateMcpToolResponse{Tool: updated}, nil
}

func mcpToolNotFoundOrErr(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return status.Error(codes.NotFound, "MCP 工具不存在")
	}
	if errors.Is(err, store.ErrMcpToolBaseURLTaken) {
		return status.Error(codes.AlreadyExists, "该团队下已接入过相同 base_url 的 MCP 工具，同一个 base_url 只能引入一次")
	}
	return status.Errorf(codes.Internal, "%v", err)
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
