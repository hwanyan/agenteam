package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hwanyan/agenteam/internal/a2a"
	"github.com/hwanyan/agenteam/internal/llm"
	"github.com/hwanyan/agenteam/internal/options"
	"github.com/hwanyan/agenteam/internal/tools"
	agenteamv1 "github.com/hwanyan/agenteam/pb/gen"
)

// delegateToolName 是提供给主 Agent 的"将请求委派给团队内某个子 Agent"工具的名称。
const delegateToolName = "delegate_to_agent"

// delegateToolArgs 是 delegateToolName 工具入参的结构（由模型以 JSON 字符串形式给出）。
type delegateToolArgs struct {
	AgentID string `json:"agent_id"`
	Message string `json:"message"`
}

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
		Id:        tools.New("msg"),
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

	reply, err := s.replyFor(ctx, agent, content, existing)
	if err != nil {
		return nil, err
	}

	agentMsg := &agenteamv1.ChatMessage{
		Id:        tools.New("msg"),
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

// replyFor 按 Agent.Kind 分发获取回复的方式：
//   - AGENT_KIND_A2A：通过 A2A 协议转发给外部 Agent（internal/a2a），
//     忽略本地 model/mcp_tools/skills 等 Prompt 方式专属的配置。
//   - 其余（AGENT_KIND_PROMPT/未指定）：沿用本地 Prompt + LLM 的原有逻辑。
func (s *WorkspaceServer) replyFor(ctx context.Context, agent *agenteamv1.Agent, content string, history []*agenteamv1.ChatMessage) (string, error) {
	if agent.Kind == agenteamv1.AgentKind_AGENT_KIND_A2A {
		if agent.A2AConfig == nil || agent.A2AConfig.EndpointUrl == "" {
			return "", status.Error(codes.FailedPrecondition, "该 Agent 尚未配置 A2A 接入地址")
		}
		reply, err := s.A2A.SendMessage(ctx, a2a.Config{
			EndpointURL: agent.A2AConfig.EndpointUrl,
			AuthScheme:  agent.A2AConfig.AuthScheme,
			AuthToken:   agent.A2AConfig.AuthToken,
			TenantID:    agent.A2AConfig.TenantId,
		}, content)
		if err != nil {
			return "", status.Errorf(codes.Unavailable, "调用 A2A Agent 失败: %v", err)
		}
		return reply, nil
	}

	// delegationTools 是将提供给主 Agent 进行请求委派给团队内某个子 Agent 的工具
	// note 是需要追加到 system prompt 中的可协作 Agent 清单文本
	// siblings 是 team 内除主 Agent 之外的其他子 Agent 信息
	delegationTools, note, siblings, err := s.resolveDelegationTools(ctx, agent)
	if err != nil {
		return "", status.Errorf(codes.Internal, "查询团队 Agent 列表失败: %v", err)
	}

	systemPrompt, err := s.buildSystemPrompt(ctx, agent, note)
	if err != nil {
		return "", err
	}
	result, err := s.LLM.Chat(ctx, llm.ChatRequest{
		Model:        agent.Model,
		SystemPrompt: systemPrompt,
		History:      buildHistory(history),
		UserMessage:  content,
		Tools:        delegationTools,
	})
	if err != nil {
		return "", status.Errorf(codes.Unavailable, "调用模型失败: %v", err)
	}

	if target, delegatedMsg, ok := parseDelegation(result.ToolCalls, siblings); ok {
		if delegatedMsg == "" {
			delegatedMsg = content
		}
		// 委派只做一层：这里面确保 target 不是主 Agent，resolveDelegationTools 会直接短路，
		// 不会再尝试进一步委派，避免出现委派链路/死循环。
		reply, err := s.replyFor(ctx, target, delegatedMsg, history)
		if err != nil {
			return "", err
		}
		return delegationPrefix(target) + reply, nil
	}
	return result.Content, nil
}

// SendMessageStream 与 SendMessage 语义一致，区别在于以 gRPC server-streaming 方式
// 逐块推送模型的增量输出：
//  1. 先持久化用户消息，并作为流的第一条响应发出（不含 delta）；
//  2. 通过 llm.Client.ChatStream 持续读取增量文本，每读到一段就转发一条 delta 响应；
//  3. 流式结束后，将拼接得到的完整回复持久化为 agent 消息，作为流的最后一条响应
//     （done=true）发出。
//
// 若在拼接出任何内容之前就发生错误，直接返回错误，前端可按非流式错误处理；
// 若已经流出部分内容后才失败，则尽量把已生成的内容落库，避免用户看到的内容与
// 历史记录不一致，随后再把错误返回给 gRPC 层（HTTP 网关会以 trailer 形式携带）。
func (s *WorkspaceServer) SendMessageStream(req *agenteamv1.SendMessageRequest, stream agenteamv1.WorkspaceService_SendMessageStreamServer) error {
	ctx := stream.Context()

	content := strings.TrimSpace(req.Content)
	if content == "" {
		return status.Error(codes.InvalidArgument, "消息内容不能为空")
	}
	if len(content) > maxMessageLen {
		return status.Errorf(codes.InvalidArgument, "消息内容过长，最大支持 %d 字符", maxMessageLen)
	}

	team, err := s.Store.GetTeam(ctx, req.TeamId)
	if err != nil {
		return teamNotFoundOrErr(err)
	}
	agent, err := s.Store.GetAgent(ctx, team.MainAgentId)
	if err != nil {
		return agentNotFoundOrErr(err)
	}

	now := time.Now().Unix()
	userMsg := &agenteamv1.ChatMessage{
		Id:        tools.New("msg"),
		TeamId:    team.Id,
		AgentId:   agent.Id,
		Role:      agenteamv1.MessageRole_MESSAGE_ROLE_USER,
		Content:   content,
		CreatedAt: now,
	}
	if err := s.Store.AppendMessage(ctx, userMsg); err != nil {
		return status.Errorf(codes.Internal, "保存消息失败: %v", err)
	}
	if err := stream.Send(&agenteamv1.SendMessageStreamResponse{UserMessage: userMsg}); err != nil {
		return err
	}

	existing, err := s.Store.ListMessages(ctx, team.Id)
	if err != nil {
		return status.Errorf(codes.Internal, "查询历史消息失败: %v", err)
	}
	history := buildHistory(existing)

	// 回复的生成过程（正文 / 思考过程 / 工具调用）统一写入 rec：一边实时转发给客户端，
	// 一边记录下来，结束后随这条回复一起落库。
	rec := newStreamRecorder(stream)
	replyAgentID, genErr := s.generateStreamReply(ctx, rec, agent, content, history)
	if genErr != nil {
		rec.settleTools(toolStatusError)
		// 若已经流出部分正文后才失败，尽量把已生成的内容连同错误说明一起落库，避免用户看到的内容
		// 与历史记录不一致；一个字都还没流出时不落库，直接把错误返回给调用方。
		if rec.hasContent() {
			s.persistAgentReply(ctx, team.Id, replyAgentID, rec.contentText()+status.Convert(genErr).Message(), rec)
		}
		return genErr
	}
	rec.settleTools(toolStatusDone)

	agentMsg, err := s.persistAgentReply(ctx, team.Id, replyAgentID, rec.contentText(), rec)
	if err != nil {
		return status.Errorf(codes.Internal, "保存回复失败: %v", err)
	}
	return stream.Send(&agenteamv1.SendMessageStreamResponse{Done: true, AgentMessage: agentMsg})
}

// generateStreamReply 生成本次对话的回复，并把正文 / 思考过程 / 工具调用流式写入 rec。
// 返回实际生成回复的 Agent id（发生委派时是被委派的子 Agent，否则是团队主 Agent）。
// 本方法只负责"生成"，落库与发送 done 由调用方 SendMessageStream 统一收尾。
func (s *WorkspaceServer) generateStreamReply(ctx context.Context, rec *streamRecorder, agent *agenteamv1.Agent,
	content string, history []llm.Message) (string, error) {
	// 当团队的主 Agent 是「外部 A2A Agent」时，直接把回复生成"外包"给 streamA2AReply，
	// 用 A2A 协议对接，不再走下方本地 LLM 的那一整套流程。
	// 因此，如果主 Agent 是 A2A Agent，这个团队内就不可能存在其他子 Agent。
	if agent.Kind == agenteamv1.AgentKind_AGENT_KIND_A2A {
		return agent.Id, s.streamA2AReply(ctx, rec, agent, content)
	}

	delegationTools, note, siblings, err := s.resolveDelegationTools(ctx, agent)
	if err != nil {
		return agent.Id, status.Errorf(codes.Internal, "查询团队 Agent 列表失败: %v", err)
	}
	if len(delegationTools) == 0 {
		return agent.Id, s.streamPromptAgentReply(ctx, rec, agent, content, history)
	}

	// 团队内还存在其他可协作的 Agent：先做一次非流式的"路由决策"调用，
	// 让主 Agent 判断本次请求是否应转交给某个更专业的子 Agent；若决定委派，
	// 则实时流式转发该子 Agent 的真实回答，而不是让主 Agent 自己勉强作答
	// （这也是此前"主 Agent 感知不到子 Agent"问题的根本修复：过去 system
	// prompt 里完全没有任何子 Agent 信息，也没有任何委派/路由的执行链路，
	// 纯靠 prompt 文字要求模型"优先使用某 Agent"是不可能生效的）。
	systemPrompt, err := s.buildSystemPrompt(ctx, agent, note)
	if err != nil {
		return agent.Id, err
	}
	decision, err := s.LLM.Chat(ctx, llm.ChatRequest{
		Model:        agent.Model,
		SystemPrompt: systemPrompt,
		History:      history,
		UserMessage:  content,
		Tools:        delegationTools,
	})
	if err != nil {
		return agent.Id, status.Errorf(codes.Unavailable, "调用模型失败: %v", err)
	}
	// 路由决策这次调用本身若带有思考过程（如 Reasoner 模型），先展示出来。
	if err := typewrite(ctx, decision.Reasoning, reasoningChunkSize, reasoningChunkInterval, rec.sendReasoning); err != nil {
		return agent.Id, err
	}

	target, delegatedMsg, ok := parseDelegation(decision.ToolCalls, siblings)
	if !ok {
		// 未触发委派：路由决策调用中已经生成了完整的最终回复内容，这里按固定大小切片
		// 模拟打字机效果推送，避免为保留流式视觉效果而对同一问题重复调用一次模型
		// （重复调用不仅浪费成本，还可能生成与本次决策不一致的另一份回复）。
		return agent.Id, typewrite(ctx, decision.Content, plainStreamChunkSize, plainStreamChunkInterval, rec.sendDelta)
	}

	if delegatedMsg == "" {
		delegatedMsg = content
	}
	// 委派本身记为一次"工具调用"，让用户在回复下方看到这次请求被转交给了哪个子 Agent。
	callID, err := rec.startTool(delegationToolLabel(target), toolKindAgent, delegationArgsJSON(delegatedMsg))
	if err != nil {
		return target.Id, err
	}
	if err := rec.sendDelta(delegationPrefix(target)); err != nil {
		return target.Id, err
	}

	var runErr error
	if target.Kind == agenteamv1.AgentKind_AGENT_KIND_A2A {
		runErr = s.streamA2AReply(ctx, rec, target, delegatedMsg)
	} else {
		runErr = s.streamPromptAgentReply(ctx, rec, target, delegatedMsg, history)
	}
	if runErr != nil {
		_ = rec.finishTool(callID, toolStatusError)
		return target.Id, runErr
	}
	return target.Id, rec.finishTool(callID, toolStatusDone)
}

// streamPromptAgentReply 通过 llm.Client.ChatStream 实时获取某个本地 Prompt 驱动 Agent
// （可能是团队主 Agent 本身，也可能是委派命中的子 Agent）的回复，把正文与思考过程
// 流式写入 rec。只负责"生成"，落库与发送 done 由 SendMessageStream 统一收尾。
func (s *WorkspaceServer) streamPromptAgentReply(ctx context.Context, rec *streamRecorder, agent *agenteamv1.Agent,
	content string, history []llm.Message) error {
	systemPrompt, err := s.buildSystemPrompt(ctx, agent, "")
	if err != nil {
		return err
	}
	chunks, err := s.LLM.ChatStream(ctx, llm.ChatRequest{
		Model:        agent.Model,
		SystemPrompt: systemPrompt,
		History:      history,
		UserMessage:  content,
	})
	if err != nil {
		return status.Errorf(codes.Unavailable, "调用模型失败: %v", err)
	}

	for chunk := range chunks {
		if chunk.Err != nil {
			return status.Errorf(codes.Unavailable, "调用模型失败: %v", chunk.Err)
		}
		// 一个 chunk 只会携带思考过程或正文之一，空文本会被 recorder 忽略。
		if err := rec.sendReasoning(chunk.Reasoning); err != nil {
			return err
		}
		if err := rec.sendDelta(chunk.Delta); err != nil {
			return err
		}
	}
	return nil
}

// 模拟打字机效果时每个 chunk 携带的字符数与推送间隔：
// 正文（plain*）用于"路由决策调用已拿到完整回复"的场景；思考过程（reasoning*）同理，
// 间隔更短，因为思考内容通常更长。
const (
	plainStreamChunkSize     = 6
	plainStreamChunkInterval = 20 * time.Millisecond

	reasoningChunkSize     = 12
	reasoningChunkInterval = 10 * time.Millisecond
)

// persistAgentReply 将 Agent 的完整回复连同流式过程中的思考过程、工具调用，
// 持久化为一条聊天消息。
func (s *WorkspaceServer) persistAgentReply(ctx context.Context, teamID, agentID, content string, rec *streamRecorder) (
	*agenteamv1.ChatMessage, error) {
	agentMsg := &agenteamv1.ChatMessage{
		Id:        tools.New("msg"),
		TeamId:    teamID,
		AgentId:   agentID,
		Role:      agenteamv1.MessageRole_MESSAGE_ROLE_AGENT,
		Content:   content,
		CreatedAt: time.Now().Unix(),
		Reasoning: rec.reasoningText(),
		ToolCalls: rec.toolCallsSnapshot(),
	}
	if err := s.Store.AppendMessage(ctx, agentMsg); err != nil {
		return nil, err
	}
	return agentMsg, nil
}

// streamA2AReply 处理 A2A 方式 Agent 的流式回复：
//   - 若对端 Agent Card 声明 capabilities.streaming=true，通过 internal/a2a.Client.
//     SendMessageStream 对接 A2A 协议的 message/stream（SSE），逐段转发对端产生的
//     增量文本；
//   - 否则（对端不支持流式，或尚未探测到该能力）退化为调用非流式的 SendMessage，
//     一次性获取完整回复后作为单个 delta 推送，前端仍走同一套流式 UI，只是没有
//     逐字打字机效果。
//
// 对端在流式过程中产生的工具调用过程事件（对端 Agent 调用了某个工具）会记入 rec 的
// 工具调用列表并转发给前端；对端不支持该扩展、或走非流式路径时不会产生工具调用记录。
// 只负责"生成"，落库与发送 done 由 SendMessageStream 统一收尾。
func (s *WorkspaceServer) streamA2AReply(ctx context.Context, rec *streamRecorder, agent *agenteamv1.Agent, content string) error {
	if agent.A2AConfig == nil || agent.A2AConfig.EndpointUrl == "" {
		return status.Error(codes.FailedPrecondition, "该 Agent 尚未配置 A2A 接入地址")
	}
	cfg := a2a.Config{
		EndpointURL: agent.A2AConfig.EndpointUrl,
		AuthScheme:  agent.A2AConfig.AuthScheme,
		AuthToken:   agent.A2AConfig.AuthToken,
		TenantID:    agent.A2AConfig.TenantId,
	}

	if !agent.A2AConfig.Streaming {
		reply, err := s.A2A.SendMessage(ctx, cfg, content)
		if err != nil {
			return status.Errorf(codes.Unavailable, "调用 A2A Agent 失败: %v", err)
		}
		return rec.sendDelta(reply)
	}

	chunks, err := s.A2A.SendMessageStream(ctx, cfg, content)
	if err != nil {
		return status.Errorf(codes.Unavailable, "调用 A2A Agent 失败: %v", err)
	}
	for chunk := range chunks {
		if chunk.Err != nil {
			return status.Errorf(codes.Unavailable, "调用 A2A Agent 失败: %v", chunk.Err)
		}
		if chunk.Tool != nil {
			if err := rec.onA2ATool(chunk.Tool); err != nil {
				return err
			}
			continue
		}
		if err := rec.sendDelta(chunk.Delta); err != nil {
			return err
		}
	}
	return nil
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

// resolveMcpToolOptions 把 agent.McpTools 里记录的工具 id 解析成"当前确实存在"的工具信息：
// 内置静态清单（internal/options.MCPTools()）加上该 Agent 所属团队自行接入的自定义工具
// （按 agent.TeamId 查 mcp_tools 表），按 Agent 配置里的顺序返回。
//
// agents.mcp_tools 只存 id、和 mcp_tools 表没有外键联动，所以其中可能残留已被删除的工具 id；
// 这里按"表里真实存在"过滤，查不到的 id 直接丢弃，不会进入 prompt。
// 返回的 ToolOption 只带名称（自定义工具不带 base_url，避免把内部接入地址暴露给模型进而泄露给用户）。
func (s *WorkspaceServer) resolveMcpToolOptions(ctx context.Context, agent *agenteamv1.Agent) ([]*agenteamv1.ToolOption, error) {
	if len(agent.McpTools) == 0 {
		return nil, nil
	}
	available := make(map[string]*agenteamv1.ToolOption)
	for _, o := range options.MCPTools() {
		available[o.Id] = o
	}
	custom, err := s.Store.ListMcpToolsByTeam(ctx, agent.TeamId)
	if err != nil {
		return nil, err
	}
	for _, t := range custom {
		available[t.Id] = &agenteamv1.ToolOption{Id: t.Id, Name: t.Name, IsCustom: true}
	}
	return pickOptions(agent.McpTools, available), nil
}

// buildSystemPrompt 解析 Agent 绑定的 MCP 工具后组装完整的 system prompt。
func (s *WorkspaceServer) buildSystemPrompt(ctx context.Context, agent *agenteamv1.Agent, extra string) (string, error) {
	mcpTools, err := s.resolveMcpToolOptions(ctx, agent)
	if err != nil {
		return "", status.Errorf(codes.Internal, "查询 MCP 工具失败: %v", err)
	}
	return composeSystemPrompt(agent, mcpTools, extra), nil
}

// pickOptions 按 ids 的顺序从 available 里取出对应选项，查不到的 id 直接跳过。
func pickOptions(ids []string, available map[string]*agenteamv1.ToolOption) []*agenteamv1.ToolOption {
	out := make([]*agenteamv1.ToolOption, 0, len(ids))
	for _, id := range ids {
		if opt, ok := available[id]; ok {
			out = append(out, opt)
		}
	}
	return out
}

// composeSystemPrompt 将 Agent 的 Prompt 与其绑定的 MCP 工具 / Skill 组合成完整的 system prompt。
// mcpTools 必须是已经过 resolveMcpToolOptions 解析、确认真实存在的工具；
// extra 为需要追加在末尾的额外说明（目前用于 resolveDelegationTools 生成的"团队内可协作
// Agent 清单 + 委派说明"，为空字符串时不追加）。
//
// 工具/技能只以名称（和描述）呈现，不暴露内部 id，并明确要求模型向用户介绍时使用名称。
//
// 注意：当前平台尚未接入真实的 MCP 工具调用协议，这里仅将工具/技能的名称与描述
// 作为上下文告知模型“有哪些能力可用”，便于后续演进为真正的 function calling / MCP 调用。
func composeSystemPrompt(agent *agenteamv1.Agent, mcpTools []*agenteamv1.ToolOption, extra string) string {
	var b strings.Builder
	b.WriteString(agent.Prompt)

	skills := make(map[string]*agenteamv1.ToolOption)
	for _, o := range options.Skills() {
		skills[o.Id] = o
	}
	agentSkills := pickOptions(agent.Skills, skills)

	if len(mcpTools) > 0 {
		b.WriteString("\n\n你可以在需要时说明会使用以下工具（当前平台工具调用能力建设中，尚未真正接入执行）：\n")
		writeToolList(&b, mcpTools)
	}
	if len(agentSkills) > 0 {
		b.WriteString("\n\n你具备以下技能：\n")
		writeToolList(&b, agentSkills)
	}
	if len(mcpTools) > 0 || len(agentSkills) > 0 {
		b.WriteString("\n向用户介绍上述工具或技能时，请使用它们的名称，不要提及或展示任何内部 ID。")
	}
	if extra != "" {
		b.WriteString(extra)
	}
	return b.String()
}

// writeToolList 按"- 名称：描述"逐行写出工具/技能清单；描述为空时只写名称。不输出 id。
func writeToolList(b *strings.Builder, opts []*agenteamv1.ToolOption) {
	for _, opt := range opts {
		if opt.Description != "" {
			fmt.Fprintf(b, "- %s：%s\n", opt.Name, opt.Description)
		} else {
			fmt.Fprintf(b, "- %s\n", opt.Name)
		}
	}
}

// resolveDelegationTools 为团队主 Agent 构建"将请求委派给团队内某个子 Agent"的工具定义，
// 并给出需要追加到 system prompt 中的可协作 Agent 清单文本。
//
// 这是本次修复的核心：此前主 Agent 的 system prompt 完全不包含任何关于子 Agent 的信息
// （名称/能力都没有），模型自然无从谈起"感知"到子 Agent 的存在；即便 prompt 文字上写了
// "请优先使用某 Agent 处理"，那也只是一句空话——没有告诉模型有哪些 Agent 可选、分别能做
// 什么，更没有任何执行链路能让模型的选择真正被转发给对应 Agent 去处理。
//
// 仅在如下条件下才会真正启用委派机制（其余场景返回 nil/""/nil，行为与原有逻辑完全一致，
// 不引入任何额外开销或风险）：
//   - agent 是团队的主 Agent（只有主 Agent 承担 "路由" 职责，避免子 Agent 之间互相委派、
//     出现委派链路或死循环）；
//   - agent 走本地 Prompt + LLM 驱动（AGENT_KIND_A2A 的外部 Agent 我们无法控制其行为，
//     不具备下发工具定义的条件）；
//   - 团队内确实还存在其他 Agent（单 Agent 团队没有委派对象）。
func (s *WorkspaceServer) resolveDelegationTools(ctx context.Context, agent *agenteamv1.Agent) (
	[]llm.Tool, string, map[string]*agenteamv1.Agent, error) {
	// 跳过主 Agent 本身和外部 A2A 的 Agent
	if !agent.IsMain || agent.Kind == agenteamv1.AgentKind_AGENT_KIND_A2A {
		return nil, "", nil, nil
	}
	all, err := s.Store.ListAgentsByTeam(ctx, agent.TeamId)
	if err != nil {
		return nil, "", nil, err
	}
	siblings := make(map[string]*agenteamv1.Agent, len(all))
	ids := make([]string, 0, len(all))
	var b strings.Builder
	for _, a := range all {
		if a.Id == agent.Id {
			continue
		}
		siblings[a.Id] = a
		ids = append(ids, a.Id)
		fmt.Fprintf(&b, "- agent_id=%s，名称:「%s」，能力: %s\n", a.Id, a.Name, agentCapabilitySummary(a))
	}
	if len(siblings) == 0 {
		return nil, "", nil, nil
	}

	note := "\n\n你所在的团队中还有以下可协作的 Agent。当用户的请求属于某个 Agent 的专长范围时，" +
		"请调用 " + delegateToolName + " 工具将请求转交给它处理，而不要自己勉强回答；" +
		"若没有合适的协作 Agent，或只是常规澄清/寒暄，直接回答即可，不要调用工具：\n" + b.String()

	tool := llm.Tool{
		Name:        delegateToolName,
		Description: "将用户当前的请求转交给团队内某个更专业的 Agent 处理",
		Properties: map[string]llm.ToolParameterProperty{
			"agent_id": {Type: "string", Description: "要转交的目标 Agent ID", Enum: ids},
			"message":  {Type: "string", Description: "转交给该 Agent 的具体请求内容，可对用户原话做适当归纳；不确定时可直接沿用用户原话"},
		},
		Required: []string{"agent_id", "message"},
	}
	return []llm.Tool{tool}, note, siblings, nil
}

// agentCapabilitySummary 为团队内某个 Agent 生成一段简短的能力描述，用于告知主 Agent
// "这个 Agent 能做什么"，帮助其判断是否应该委派。
func agentCapabilitySummary(a *agenteamv1.Agent) string {
	// 外部 A2A Agent 直接使用其描述信息；本地 Agent 则截取 Prompt 前 120 字符作为描述。
	if a.Kind == agenteamv1.AgentKind_AGENT_KIND_A2A {
		if a.A2AConfig != nil && a.A2AConfig.RemoteDescription != "" {
			return a.A2AConfig.RemoteDescription
		}
		return "（外部 A2A Agent，暂无描述）"
	}
	const maxLen = 120
	p := strings.TrimSpace(a.Prompt)
	runes := []rune(p)
	if len(runes) > maxLen {
		p = string(runes[:maxLen]) + "..."
	}
	if p == "" {
		return "（暂无描述）"
	}
	return p
}

// parseDelegation 从模型返回的 ToolCalls 中解析出"委派给哪个 Agent、转交什么内容"；
// 若模型未调用委派工具，或调用了但参数无法解析/目标 Agent 不存在，返回 ok=false，
// 调用方应回退为使用模型自身生成的 Content 直接作答。
func parseDelegation(calls []llm.ToolCall, siblings map[string]*agenteamv1.Agent) (
	target *agenteamv1.Agent, message string, ok bool) {
	for _, c := range calls {
		if c.Name != delegateToolName {
			continue
		}
		var args delegateToolArgs
		if err := json.Unmarshal([]byte(c.Arguments), &args); err != nil {
			continue
		}
		if t, exists := siblings[args.AgentID]; exists {
			return t, strings.TrimSpace(args.Message), true
		}
	}
	return nil, "", false
}

// delegationPrefix 生成一段提示文本，告知用户接下来的回复实际来自哪个子 Agent，
// 便于在不改动前端展示逻辑的前提下也能直观确认委派确实发生了。
func delegationPrefix(target *agenteamv1.Agent) string {
	return fmt.Sprintf("_由子 Agent「%s」处理：_\n\n", target.Name)
}

// delegationToolLabel 是委派这次"工具调用"在界面上的展示名称（面向用户，不暴露内部的 delegate_to_agent / agent id）。
func delegationToolLabel(target *agenteamv1.Agent) string {
	return fmt.Sprintf("子 Agent「%s」", target.Name)
}

// delegationArgsJSON 把转交给子 Agent 的请求内容序列化成工具入参的 JSON 字符串。
func delegationArgsJSON(message string) string {
	b, err := json.Marshal(map[string]string{"message": message})
	if err != nil {
		return ""
	}
	return string(b)
}
