// Package llm 封装与大模型对话的能力。
//
// 实现基于 DeepSeek 官方推荐的 Go SDK（github.com/cohesion-org/deepseek-go），
// 直接调用 DeepSeek 的 Chat Completions 接口，而非自行拼装 HTTP 请求。
//
// 安全说明：API Key（及可选的 Base URL 覆盖）均只允许通过环境变量配置
// （遵循 secrets 只走 env 的原则），不接受来自用户请求的动态取值。本包不直接读取
// 环境变量，而是由调用方（main.go）从 internal/config.Config（其取值全部来自
// 环境变量）中取出后传入 New：
//   - DeepSeekAPIKey  ：DeepSeek 平台申请的密钥（https://platform.deepseek.com/api_keys）
//   - DeepSeekBaseURL ：可选，自定义 API Base URL，默认 https://api.deepseek.com/
//
// 本文件只包含对外的接口层（类型定义 + Client 接口 + 构造入口 New）；
// 具体实现（DeepSeekClient / EchoClient）见同包下的 impl.go。
package llm

import "context"

// Message 是对话中的一条消息。
type Message struct {
	Role    string // "system" | "user" | "assistant"
	Content string
}

// ChatRequest 是一次对话请求。
type ChatRequest struct {
	Model        string
	SystemPrompt string
	History      []Message
	UserMessage  string
	// Tools 是本次对话中模型可选择调用的工具（function calling），目前用于团队内
	// "主 Agent 将请求委派给某个子 Agent" 的路由场景；为空时行为与原来完全一致。
	Tools []Tool
}

// ToolParameterProperty 描述 Tool 的一个入参（JSON Schema 的简化子集，
// 足以覆盖当前平台内工具调用的需要：字符串类型 + 可选枚举取值）。
type ToolParameterProperty struct {
	Type        string   // 取值类型，如 "string"
	Description string   // 供模型理解该参数含义
	Enum        []string // 可选：限定取值范围（如合法的子 Agent ID 列表）
}

// Tool 描述一个可供模型在本轮对话中选择调用的工具/函数，遵循 OpenAI 兼容的
// function calling 协议（DeepSeek 等主流模型 API 均支持）。
type Tool struct {
	Name        string
	Description string
	Properties  map[string]ToolParameterProperty
	Required    []string
}

// ToolCall 是模型决定调用的某个工具及其入参（Arguments 为该工具入参的 JSON 字符串，
// 由调用方按约定的参数结构自行解析）。
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ChatResult 是一次非流式对话的结果：
//   - 若模型直接作答，Content 非空、ToolCalls 为空；
//   - 若模型决定调用某个 Tool（如委派给子 Agent），ToolCalls 非空，Content 通常为空，
//     调用方需自行执行对应逻辑后决定下一步（当前平台只走一轮委派，不做多轮 tool loop）。
type ChatResult struct {
	Content   string
	ToolCalls []ToolCall
}

// StreamChunk 是流式对话中的一个增量片段。
//
// 消费方应持续从 ChatStream 返回的 channel 中读取，直到 channel 被关闭；
// 若某个 chunk 携带非空 Err，代表流式过程中发生错误，消费方应终止读取并将
// 已累积的内容 + 该错误一并处理（DeepSeek 与 Echo 两种实现均保证 Err 非空时
// 之后不会再有更多 chunk 写入该 channel）。
type StreamChunk struct {
	Delta string
	Err   error
}

// Client 定义与 LLM 对话的能力，方便替换/测试。
// 主 Agent 与平台上创建的所有子 Agent 均通过此接口选用各自配置的模型并发起对话。
type Client interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResult, error)
	// ChatStream 以流式方式发起对话，返回的 channel 会持续推送增量文本片段，
	// 供上层（如 WorkspaceService.SendMessageStream）实时转发给前端，实现打字机效果。
	ChatStream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error)
}

// New 根据配置构建 LLM 客户端（apiKey/baseURL 来自 internal/config.Config，
// 其取值最终都来自环境变量）。若 apiKey 为空，则降级为本地 Echo 客户端，
// 方便无密钥场景下跑通整套流程。
//
// 具体实现见 impl.go 中的 DeepSeekClient / EchoClient。
func New(apiKey, baseURL string) Client {
	if cli := newDeepSeekClient(apiKey, baseURL); cli != nil {
		return cli
	}
	return &EchoClient{}
}
