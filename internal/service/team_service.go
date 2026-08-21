package service

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hwanyan/agenteam/internal/idgen"
	"github.com/hwanyan/agenteam/internal/options"
	"github.com/hwanyan/agenteam/internal/store"
	agenteamv1 "github.com/hwanyan/agenteam/pb/gen"
)

const defaultMainAgentPrompt = "你是本团队的主 Agent，负责理解用户需求并给出准确、有帮助的回答。" +
	"请保持专业、简洁，如信息不足请主动向用户澄清。"

// TeamServer 实现 TeamService。
type TeamServer struct {
	agenteamv1.UnimplementedTeamServiceServer
	*Deps
}

// NewTeamServer 创建 TeamServer。
func NewTeamServer(deps *Deps) *TeamServer {
	return &TeamServer{Deps: deps}
}

// CreateTeam 创建团队，并自动创建、加载该团队的主 Agent。
func (s *TeamServer) CreateTeam(ctx context.Context, req *agenteamv1.CreateTeamRequest) (*agenteamv1.CreateTeamResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "团队名称不能为空")
	}

	now := time.Now().Unix()
	teamID := idgen.New("team")
	agentID := idgen.New("agent")

	agent := &agenteamv1.Agent{
		Id:        agentID,
		TeamId:    teamID,
		Name:      "主 Agent",
		Prompt:    defaultMainAgentPrompt,
		Model:     options.DefaultModel,
		McpTools:  []string{},
		Skills:    []string{},
		IsMain:    true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	agent, err := s.Runtime.Load(ctx, agent)
	if err != nil {
		// 主 Agent 的默认配置来自平台内置默认值，理论上不应校验失败；
		// 一旦出现说明是平台代码缺陷，返回 Internal 更贴切。
		return nil, status.Errorf(codes.Internal, "初始化主 Agent 失败: %v", err)
	}

	team := &agenteamv1.Team{
		Id:          teamID,
		Name:        req.Name,
		MainAgentId: agentID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.Store.CreateTeam(ctx, team, agent); err != nil {
		return nil, status.Errorf(codes.Internal, "创建团队失败: %v", err)
	}

	return &agenteamv1.CreateTeamResponse{Team: team, MainAgent: agent}, nil
}

// ListTeams 返回全部团队。
func (s *TeamServer) ListTeams(ctx context.Context, _ *agenteamv1.ListTeamsRequest) (*agenteamv1.ListTeamsResponse, error) {
	teams, err := s.Store.ListTeams(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "查询团队列表失败: %v", err)
	}
	return &agenteamv1.ListTeamsResponse{Teams: teams}, nil
}

// GetTeam 按 id 查询团队。
func (s *TeamServer) GetTeam(ctx context.Context, req *agenteamv1.GetTeamRequest) (*agenteamv1.GetTeamResponse, error) {
	team, err := s.Store.GetTeam(ctx, req.Id)
	if err != nil {
		return nil, teamNotFoundOrErr(err)
	}
	return &agenteamv1.GetTeamResponse{Team: team}, nil
}

// DeleteTeam 删除团队及其主 Agent、聊天记录。
func (s *TeamServer) DeleteTeam(ctx context.Context, req *agenteamv1.DeleteTeamRequest) (*agenteamv1.DeleteTeamResponse, error) {
	team, err := s.Store.GetTeam(ctx, req.Id)
	if err != nil {
		return nil, teamNotFoundOrErr(err)
	}
	if err := s.Store.DeleteTeam(ctx, req.Id); err != nil {
		return nil, teamNotFoundOrErr(err)
	}
	// 同步清理该团队主 Agent 的运行态缓存，避免残留脏数据。
	_ = s.Runtime.Unload(ctx, team.MainAgentId)
	return &agenteamv1.DeleteTeamResponse{}, nil
}

func teamNotFoundOrErr(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return status.Error(codes.NotFound, "团队不存在")
	}
	return status.Errorf(codes.Internal, "%v", err)
}
