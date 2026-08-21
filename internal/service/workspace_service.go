package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hwanyan/agenteam/internal/idgen"
	"github.com/hwanyan/agenteam/internal/llm"
	"github.com/hwanyan/agenteam/internal/options"
	agenteamv1 "github.com/hwanyan/agenteam/pb/gen"
)

// maxMessageLen 限制单条用户消息长度，避免异常大请求打满内存/上游 LLM。
const maxMessageLen = 8000

// historyWindow 是构造对话上下文时携带的最近历史消息条数。
const historyWindow = 20

// WorkspaceServer 实现 WorkspaceService。
type WorkspaceServer struct {
	agenteamv1.UnimplementedWorkspaceServiceServer
	*Deps
}

// NewWorkspaceServer 创建 WorkspaceServer。
func NewWorkspaceServer(deps *Deps) *WorkspaceServer {
	return &WorkspaceServer{Deps: deps}
}

// SendMessage 向团队的主 Agent 发送一条消息并返回其回复。
func (s *WorkspaceServer) SendMessage(ctx context.Context, req *agenteamv1.SendMessageRequest) (*agenteamv1.SendMessageResponse, error) {
	content := strings.TrimSpace(req.Content)
	if content == "" {
		return nil, status.Error(codes.InvalidArgument, "消息内容不能为空")
	}
	if len(content) > maxMessageLen {
		return nil, status.Errorf(codes.InvalidArgument, "消息内容过长，最大支持 %d 字符", maxMessageLen)
	}

	team, err := s.Store.GetTeam(ctx, req.TeamId)
	if err != nil {
		return nil, teamNotFoundOrErr(err)
	}
	agent, err := s.Store.GetAgent(ctx, team.MainAgentId)
	if err != nil {
		return nil, agentNotFoundOrErr(err)
	}

	now := time.Now().Unix()
	userMsg := &agenteamv1.ChatMessage{
		Id:        idgen.New("msg"),
		TeamId:    team.Id,
		AgentId:   agent.Id,
		Role:      agenteamv1.MessageRole_MESSAGE_ROLE_USER,
		Content:   content,
		CreatedAt: now,
	}
	if err := s.Store.AppendMessage(ctx, userMsg); err != nil {
		return nil, status.Errorf(codes.Internal, "保存消息失败: %v", err)
	}

	existing, err := s.Store.ListMessages(ctx, team.Id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "查询历史消息失败: %v", err)
	}
	history := buildHistory(existing)
	reply, err := s.LLM.Chat(ctx, llm.ChatRequest{
		Model:        agent.Model,
		SystemPrompt: composeSystemPrompt(agent),
		History:      history,
		UserMessage:  content,
	})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "调用模型失败: %v", err)
	}

	agentMsg := &agenteamv1.ChatMessage{
		Id:        idgen.New("msg"),
		TeamId:    team.Id,
		AgentId:   agent.Id,
		Role:      agenteamv1.MessageRole_MESSAGE_ROLE_AGENT,
		Content:   reply,
		CreatedAt: time.Now().Unix(),
	}
	if err := s.Store.AppendMessage(ctx, agentMsg); err != nil {
		return nil, status.Errorf(codes.Internal, "保存回复失败: %v", err)
	}

	return &agenteamv1.SendMessageResponse{UserMessage: userMsg, AgentMessage: agentMsg}, nil
}

// ListMessages 返回团队的历史聊天记录。
func (s *WorkspaceServer) ListMessages(ctx context.Context, req *agenteamv1.ListMessagesRequest) (*agenteamv1.ListMessagesResponse, error) {
	if _, err := s.Store.GetTeam(ctx, req.TeamId); err != nil {
		return nil, teamNotFoundOrErr(err)
	}
	msgs, err := s.Store.ListMessages(ctx, req.TeamId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "查询历史消息失败: %v", err)
	}
	return &agenteamv1.ListMessagesResponse{Messages: msgs}, nil
}

// buildHistory 排除最新一条（刚追加的用户消息本身会作为 UserMessage 单独传入），
// 并截取最近 historyWindow 条作为对话上下文。
func buildHistory(all []*agenteamv1.ChatMessage) []llm.Message {
	if len(all) <= 1 {
		return nil
	}
	msgs := all[:len(all)-1]
	if len(msgs) > historyWindow {
		msgs = msgs[len(msgs)-historyWindow:]
	}
	out := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		role := "user"
		if m.Role == agenteamv1.MessageRole_MESSAGE_ROLE_AGENT {
			role = "assistant"
		}
		out = append(out, llm.Message{Role: role, Content: m.Content})
	}
	return out
}

// composeSystemPrompt 将 Agent 的 Prompt 与其绑定的 MCP 工具 / Skill 组合成完整的 system prompt。
//
// 注意：当前平台尚未接入真实的 MCP 工具调用协议，这里仅将工具/技能的名称与描述
// 作为上下文告知模型“有哪些能力可用”，便于后续演进为真正的 function calling / MCP 调用。
func composeSystemPrompt(agent *agenteamv1.Agent) string {
	var b strings.Builder
	b.WriteString(agent.Prompt)

	if len(agent.McpTools) > 0 {
		b.WriteString("\n\n你可以在需要时说明会使用以下工具（当前平台工具调用能力建设中，尚未真正接入执行）：\n")
		writeToolList(&b, agent.McpTools, options.MCPTools())
	}
	if len(agent.Skills) > 0 {
		b.WriteString("\n\n你具备以下技能：\n")
		writeToolList(&b, agent.Skills, options.Skills())
	}
	return b.String()
}

func writeToolList(b *strings.Builder, ids []string, all []*agenteamv1.ToolOption) {
	byID := make(map[string]*agenteamv1.ToolOption, len(all))
	for _, o := range all {
		byID[o.Id] = o
	}
	for _, id := range ids {
		if opt, ok := byID[id]; ok {
			fmt.Fprintf(b, "- %s：%s\n", opt.Name, opt.Description)
		} else {
			fmt.Fprintf(b, "- %s\n", id)
		}
	}
}
