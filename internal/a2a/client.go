// Package a2a 实现与外部 A2A（Agent2Agent）协议 Agent 的对接能力。
//
// A2A 是一个开放标准，用于在独立的 AI Agent 系统之间进行发现与通信
// （规范见 https://a2a-protocol.org/latest/specification/）。本包只实现平台
// 作为"客户端"接入外部 A2A Agent 所需的最小子集：
//   - Agent Card 发现：GET {endpoint_url}/.well-known/agent-card.json，
//     用于校验对端可达性，并回填名称/描述/技能/是否支持流式等展示信息。
//   - 发送消息（非流式）：POST {endpoint_url}，JSON-RPC 2.0 方法 "message/send"，
//     用于把用户在工作区中的一条消息转发给外部 Agent，并取回其文本回复。
//   - 发送消息（流式）：POST {endpoint_url}，JSON-RPC 2.0 方法 "message/stream"，
//     以 SSE（Server-Sent Events）方式持续接收对端产生的增量文本，仅当对端
//     Agent Card 声明 capabilities.streaming=true 时才应调用。
//
// 鉴权：支持 "bearer"（HTTP Authorization: Bearer <token>）与无鉴权两种标准 A2A
// 鉴权方式；此外还支持部分对端采用的"TenantID + Token"双因子鉴权模型（额外要求
// X-A2A-Tenant-Id 请求头，见 Config.TenantID），覆盖当前平台的实际接入场景。
package a2a

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hwanyan/agenteam/internal/idgen"
)

// agentCardPath 是 A2A 规范约定的 Agent Card 发现路径。
const agentCardPath = "/.well-known/agent-card.json"

// tenantHeader 是"TenantID + Token"双因子鉴权模型中，调用方声明自身身份所使用的
// 请求头名，需与 Authorization: Bearer <token> 配合使用（约定见 Config.TenantID）。
const tenantHeader = "X-A2A-Tenant-Id"

// requestTimeout 是对外部 A2A Agent 发起非流式 HTTP 请求的超时时间。
const requestTimeout = 30 * time.Second

// AgentCard 是 GET {endpoint_url}/.well-known/agent-card.json 返回的对端能力描述，
// 这里只解析平台需要用到的字段子集。
type AgentCard struct {
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	Capabilities AgentCapabilities `json:"capabilities"`
	Skills       []AgentSkill      `json:"skills"`
}

// AgentCapabilities 对应 Agent Card 中的 capabilities 字段子集。
type AgentCapabilities struct {
	Streaming bool `json:"streaming"`
}

// AgentSkill 对应 Agent Card 中 skills 数组的元素子集。
type AgentSkill struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Config 是发起一次 A2A 调用所需的连接信息。
type Config struct {
	EndpointURL string // 外部 Agent 的服务地址（不含 /.well-known/... 后缀）
	AuthScheme  string // 目前支持 "" / "bearer"
	AuthToken   string
	// TenantID 对应部分对端要求的 X-A2A-Tenant-Id 请求头（"TenantID + Token"双因子
	// 鉴权模型，如本项目姊妹项目 agently 的 A2A 服务端实现）：与 AuthToken 搭配使用，
	// 二者缺一不可。留空则不发送该头，兼容标准 A2A 单因子/无鉴权场景。
	TenantID string
}

// Client 是 A2A 协议客户端，负责 Agent Card 发现与消息发送（非流式/流式）。
type Client struct {
	httpCli       *http.Client // 用于非流式请求，带固定超时
	streamHTTPCli *http.Client // 用于流式请求，不设超时，生命周期由调用方 ctx 控制
}

// New 创建一个 A2A Client。
func New() *Client {
	return &Client{
		httpCli:       &http.Client{Timeout: requestTimeout},
		streamHTTPCli: &http.Client{}, // 流式对话可能持续较长时间，不设置超时
	}
}

// DiscoverAgentCard 向外部 Agent 发起 Agent Card 发现请求，用于校验连通性并
// 获取对端的展示信息（名称/描述/技能/是否支持流式）。
func (c *Client) DiscoverAgentCard(ctx context.Context, cfg Config) (*AgentCard, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.EndpointURL), "/")
	if base == "" {
		return nil, fmt.Errorf("a2a: endpoint_url 不能为空")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+agentCardPath, nil)
	if err != nil {
		return nil, fmt.Errorf("a2a: 构造 agent card 请求失败: %w", err)
	}
	applyAuth(req, cfg)

	resp, err := c.httpCli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("a2a: 请求 agent card 失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("a2a: 读取 agent card 响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("a2a: agent card 请求返回非 200 状态码 %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var card AgentCard
	if err := json.Unmarshal(body, &card); err != nil {
		return nil, fmt.Errorf("a2a: 解析 agent card 失败: %w", err)
	}
	if card.Name == "" {
		return nil, fmt.Errorf("a2a: agent card 缺少 name 字段，可能不是合法的 A2A Agent")
	}
	return &card, nil
}

// jsonrpcRequest / jsonrpcResponse 是 A2A 规范中 JSON-RPC 2.0 绑定的通用信封。
type jsonrpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *jsonrpcError   `json:"error"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// messagePart / a2aMessage 对应 A2A 规范中 Message.parts（这里只使用 text part）。
type messagePart struct {
	Kind string `json:"kind"` // "text"
	Text string `json:"text"`
}

type a2aMessage struct {
	MessageID string        `json:"messageId"`
	Role      string        `json:"role"` // "user" | "agent"
	Parts     []messagePart `json:"parts"`
}

type sendMessageParams struct {
	Message a2aMessage `json:"message"`
}

// sendMessageResult 兼容 A2A 规范中 message/send 两种可能的返回形态：
// 直接返回 Message（简单交互），或返回 Task（此时取 status.message / 首个 artifact 的文本）。
type sendMessageResult struct {
	// 直接 Message 响应
	Role  string        `json:"role"`
	Parts []messagePart `json:"parts"`

	// Task 响应
	Status *taskStatus `json:"status"`
}

type taskStatus struct {
	State   string      `json:"state"` // TaskState，如 "TASK_STATE_COMPLETED" / 旧版本 "completed" 等
	Message *a2aMessage `json:"message"`
}

// SendMessage 通过 A2A 协议向外部 Agent 发送一条消息（JSON-RPC 方法 "message/send"），
// 返回其文本回复。当前只取回复中的纯文本 parts 并拼接，不处理文件/结构化 part。
func (c *Client) SendMessage(ctx context.Context, cfg Config, content string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.EndpointURL), "/")
	if base == "" {
		return "", fmt.Errorf("a2a: endpoint_url 不能为空")
	}

	rpcReq := jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      idgen.New("a2a-req"),
		Method:  "message/send",
		Params: sendMessageParams{
			Message: a2aMessage{
				MessageID: idgen.New("a2a-msg"),
				Role:      "user",
				Parts:     []messagePart{{Kind: "text", Text: content}},
			},
		},
	}
	payload, err := json.Marshal(rpcReq)
	if err != nil {
		return "", fmt.Errorf("a2a: 序列化 message/send 请求失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("a2a: 构造 message/send 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	applyAuth(req, cfg)

	resp, err := c.httpCli.Do(req)
	if err != nil {
		return "", fmt.Errorf("a2a: 调用外部 agent 失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("a2a: 读取外部 agent 响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("a2a: 外部 agent 返回非 200 状态码 %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var rpcResp jsonrpcResponse
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return "", fmt.Errorf("a2a: 解析 message/send 响应失败: %w", err)
	}
	if rpcResp.Error != nil {
		return "", fmt.Errorf("a2a: 外部 agent 返回错误 (code=%d): %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	var result sendMessageResult
	if err := json.Unmarshal(rpcResp.Result, &result); err != nil {
		return "", fmt.Errorf("a2a: 解析 message/send result 失败: %w", err)
	}

	// 解析结果
	if text := joinTextParts(result.Parts); text != "" {
		return text, nil
	}
	// 解析的结果为空时进一步解析 Task
	if result.Status != nil && result.Status.Message != nil {
		if text := joinTextParts(result.Status.Message.Parts); text != "" {
			return text, nil
		}
	}
	return "", fmt.Errorf("a2a: 外部 agent 响应中未包含可展示的文本内容")
}

func joinTextParts(parts []messagePart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(p.Text)
	}
	return b.String()
}

// StreamChunk 是 SendMessageStream 推送的一个增量片段。
//
// 消费方应持续从 SendMessageStream 返回的 channel 中读取，直到 channel 被关闭；
// 若某个 chunk 携带非空 Err，代表流式过程中发生错误，之后不会再有更多 chunk 写入
// 该 channel，消费方应终止读取并按错误处理。
type StreamChunk struct {
	Delta string
	Err   error
}

// streamEvent 是 SSE 流中一次 JSON-RPC 响应携带的 result（即 A2A 规范的 StreamResponse），
// 同时兼容两种在实际生态中都存在的序列化风格：
//   - 较新的 1.0 规范：按 oneof 字段名嵌套，如 {"statusUpdate": {...}}；
//   - 目前主流 SDK（0.2.x/0.3.x 系列）仍在使用的扁平化风格：用 "kind" 判别字段，
//     内容字段展开在同一层，如 {"kind": "status-update", "status": {...}, "final": true}。
//
// 为兼容两种写法，这里把两种风格下可能出现的字段都平铺声明，实际使用哪一套字段
// 由 Kind 是否为空 + 是否存在 StatusUpdate/ArtifactUpdate 嵌套对象来判断。
type streamEvent struct {
	// 扁平风格判别字段，如 "message" / "task" / "status-update" / "artifact-update"
	Kind string `json:"kind"`

	// 直接 Message（不区分风格，两者字段一致）
	Role  string        `json:"role"`
	Parts []messagePart `json:"parts"`

	// oneof 嵌套风格
	Task           *taskSnapshot            `json:"task"`
	Message        *a2aMessage              `json:"message"`
	StatusUpdate   *taskStatusUpdateEvent   `json:"statusUpdate"`
	ArtifactUpdate *taskArtifactUpdateEvent `json:"artifactUpdate"`

	// 扁平风格下，status-update / artifact-update 事件的内容字段直接展开在本层
	Status   *taskStatus `json:"status"`
	Artifact *artifact   `json:"artifact"`
	Final    bool        `json:"final"` // 扁平风格标记该事件是否为流的最后一个事件（1.0 规范已移除，靠 status.state 终态判断）
}

type taskSnapshot struct {
	Status *taskStatus `json:"status"`
}

type taskStatusUpdateEvent struct {
	Status *taskStatus `json:"status"`
	Final  bool        `json:"final"`
}

type taskArtifactUpdateEvent struct {
	Artifact *artifact `json:"artifact"`
}

type artifact struct {
	Parts []messagePart `json:"parts"`
}

// terminalTaskStates 是 A2A 规范中任务的终态/中断态集合（涵盖 1.0 规范枚举值与
// 0.2.x/0.3.x 系列常见的小写字符串写法），流式响应遇到这些状态时应停止读取。
var terminalTaskStates = map[string]bool{
	"TASK_STATE_COMPLETED":      true,
	"TASK_STATE_FAILED":         true,
	"TASK_STATE_CANCELED":       true,
	"TASK_STATE_REJECTED":       true,
	"TASK_STATE_INPUT_REQUIRED": true,
	"TASK_STATE_AUTH_REQUIRED":  true,
	"completed":                 true,
	"failed":                    true,
	"canceled":                  true,
	"rejected":                  true,
	"input-required":            true,
	"auth-required":             true,
}

// extractText 从一个 streamEvent 中提取本次应转发的增量文本（若有），
// 兼容直接 Message / Task 快照 / statusUpdate / artifactUpdate 四种事件形态，
// 以及 oneof 嵌套风格与扁平 "kind" 判别字段风格。
func (e *streamEvent) extractText() string {
	// 直接 Message 响应（流中唯一一个事件，随即关闭）。
	if len(e.Parts) > 0 {
		return joinTextParts(e.Parts)
	}
	if e.Message != nil {
		return joinTextParts(e.Message.Parts)
	}
	// Task 快照 / statusUpdate：文本携带在 status.message 中。
	if e.Task != nil && e.Task.Status != nil && e.Task.Status.Message != nil {
		return joinTextParts(e.Task.Status.Message.Parts)
	}
	if e.StatusUpdate != nil && e.StatusUpdate.Status != nil && e.StatusUpdate.Status.Message != nil {
		return joinTextParts(e.StatusUpdate.Status.Message.Parts)
	}
	if e.Status != nil && e.Status.Message != nil {
		return joinTextParts(e.Status.Message.Parts)
	}
	// artifactUpdate：文本携带在 artifact.parts 中。
	if e.ArtifactUpdate != nil && e.ArtifactUpdate.Artifact != nil {
		return joinTextParts(e.ArtifactUpdate.Artifact.Parts)
	}
	if e.Artifact != nil {
		return joinTextParts(e.Artifact.Parts)
	}
	return ""
}

// isTerminal 判断该事件是否代表流应当结束（任务进入终态/中断态，
// 或扁平风格下显式携带 final=true；直接 Message 响应本身不携带终态信息，
// 由调用方在读到该事件后自行判断"流中只应有一个 Message"并结束）。
func (e *streamEvent) isTerminal() bool {
	if e.Final {
		return true
	}
	if e.StatusUpdate != nil && e.StatusUpdate.Final {
		return true
	}
	state := ""
	switch {
	case e.Task != nil && e.Task.Status != nil:
		state = e.Task.Status.State
	case e.StatusUpdate != nil && e.StatusUpdate.Status != nil:
		state = e.StatusUpdate.Status.State
	case e.Status != nil:
		state = e.Status.State
	}
	return terminalTaskStates[state]
}

// SendMessageStream 通过 A2A 协议以流式方式向外部 Agent 发送一条消息
// （JSON-RPC 方法 "message/stream"），以 SSE 持续读取对端产生的增量文本并写入
// 返回的 channel，直到收到终态事件或流关闭。仅当对端 Agent Card 声明
// capabilities.streaming=true 时才应调用本方法，否则应改用 SendMessage。
//
// 返回的 channel 会在流结束（正常或异常）后被关闭；若发生错误，会作为最后一个
// StreamChunk（Err 非空）写入 channel，之后不再有更多 chunk。
func (c *Client) SendMessageStream(ctx context.Context, cfg Config, content string) (<-chan StreamChunk, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.EndpointURL), "/")
	if base == "" {
		return nil, fmt.Errorf("a2a: endpoint_url 不能为空")
	}

	rpcReq := jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      idgen.New("a2a-req"),
		Method:  "message/stream",
		Params: sendMessageParams{
			Message: a2aMessage{
				MessageID: idgen.New("a2a-msg"),
				Role:      "user",
				Parts:     []messagePart{{Kind: "text", Text: content}},
			},
		},
	}
	payload, err := json.Marshal(rpcReq)
	if err != nil {
		return nil, fmt.Errorf("a2a: 序列化 message/stream 请求失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("a2a: 构造 message/stream 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	applyAuth(req, cfg)

	resp, err := c.streamHTTPCli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("a2a: 调用外部 agent 失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("a2a: 外部 agent 返回非 200 状态码 %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	out := make(chan StreamChunk)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		if err := readSSE(ctx, resp.Body, out); err != nil {
			select {
			case out <- StreamChunk{Err: err}:
			case <-ctx.Done():
			}
		}
	}()
	return out, nil
}

// readSSE 逐行解析 SSE 响应体：SSE 事件以空行分隔，每个事件可能包含多个
// "data: xxx" 行（规范约定应以换行拼接后再整体解析），这里对每个事件只取其
// 拼接后的 data 内容整体反序列化为一个 JSON-RPC Response。
func readSSE(ctx context.Context, body io.Reader, out chan<- StreamChunk) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var dataLines []string
	flush := func() (done bool, err error) {
		if len(dataLines) == 0 {
			return false, nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if data == "" || data == "[DONE]" {
			return false, nil
		}

		var rpcResp jsonrpcResponse
		if err := json.Unmarshal([]byte(data), &rpcResp); err != nil {
			return false, fmt.Errorf("a2a: 解析 message/stream 事件失败: %w", err)
		}
		if rpcResp.Error != nil {
			return true, fmt.Errorf("a2a: 外部 agent 返回错误 (code=%d): %s", rpcResp.Error.Code, rpcResp.Error.Message)
		}

		var event streamEvent
		if err := json.Unmarshal(rpcResp.Result, &event); err != nil {
			return false, fmt.Errorf("a2a: 解析 message/stream result 失败: %w", err)
		}

		if text := event.extractText(); text != "" {
			select {
			case out <- StreamChunk{Delta: text}:
			case <-ctx.Done():
				return true, ctx.Err()
			}
		}

		// 直接 Message 响应（无 Task/statusUpdate 包裹）意味着这是简单交互，
		// 流中只应有这一个事件，收到后即可结束。
		isDirectMessage := event.Kind == "message" || (len(event.Parts) > 0 && event.Task == nil && event.StatusUpdate == nil && event.ArtifactUpdate == nil)
		return isDirectMessage || event.isTerminal(), nil
	}

	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "":
			done, err := flush()
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		default:
			// 忽略 "event:" / "id:" / "retry:" / 注释行等其他 SSE 字段。
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("a2a: 读取 message/stream 响应失败: %w", err)
	}
	// 流正常关闭（无论是否以空行收尾），处理最后一个尚未 flush 的事件。
	_, err := flush()
	return err
}

func applyAuth(req *http.Request, cfg Config) {
	if tid := strings.TrimSpace(cfg.TenantID); tid != "" {
		req.Header.Set(tenantHeader, tid)
	}
	if cfg.AuthToken == "" {
		return
	}
	switch strings.ToLower(strings.TrimSpace(cfg.AuthScheme)) {
	case "bearer", "":
		req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	default:
		req.Header.Set("Authorization", cfg.AuthScheme+" "+cfg.AuthToken)
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
