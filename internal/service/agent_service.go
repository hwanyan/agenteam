package service

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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
	return &agenteamv1.GetAgentResponse{Agent: agent}, nil
}

// UpdateAgent 保存 Agent 配置并触发服务端重新加载。
func (s *AgentServer) UpdateAgent(ctx context.Context, req *agenteamv1.UpdateAgentRequest) (*agenteamv1.UpdateAgentResponse, error) {
	agent, err := s.Store.GetAgent(ctx, req.Id)
	if err != nil {
		return nil, agentNotFoundOrErr(err)
	}

	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "agent 名称不能为空")
	}
	if req.Prompt == "" {
		return nil, status.Error(codes.InvalidArgument, "agent prompt 不能为空")
	}

	updated := &agenteamv1.Agent{
		Id:        agent.Id,
		TeamId:    agent.TeamId,
		Name:      req.Name,
		Prompt:    req.Prompt,
		Model:     req.Model,
		McpTools:  append([]string{}, req.McpTools...),
		Skills:    append([]string{}, req.Skills...),
		IsMain:    agent.IsMain,
		Version:   agent.Version,
		CreatedAt: agent.CreatedAt,
	}

	updated, err = s.Runtime.Load(ctx, updated)
	if err != nil {
		_ = s.Store.SaveAgent(ctx, updated) // 落盘 ERROR 状态，便于前端展示加载失败原因
		return nil, status.Errorf(codes.InvalidArgument, "重新加载 agent 失败: %v", err)
	}
	if err := s.Store.SaveAgent(ctx, updated); err != nil {
		return nil, status.Errorf(codes.Internal, "保存 agent 配置失败: %v", err)
	}

	return &agenteamv1.UpdateAgentResponse{Agent: updated}, nil
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
