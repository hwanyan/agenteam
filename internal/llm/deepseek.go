// 本文件包含 Client 接口（定义于 client.go）的具体实现层：
//   - DeepSeekClient：基于 DeepSeek 官方 Go SDK 的真实实现。
//   - EchoClient     ：无需任何密钥即可运行的本地兜底实现。
package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	deepseek "github.com/cohesion-org/deepseek-go"
)

// DeepSeekClient 是基于 DeepSeek 官方 Go SDK 的真实实现。
type DeepSeekClient struct {
	cli *deepseek.Client
}

// newDeepSeekClient 尝试用给定的 apiKey/baseURL 构建 DeepSeekClient。
// apiKey 为空，或客户端初始化失败（如密钥格式问题）时返回 nil，
// 由调用方（New）降级为 EchoClient，避免整个服务无法启动。
func newDeepSeekClient(apiKey, baseURL string) *DeepSeekClient {
	if apiKey == "" {
		return nil
	}

	opts := []deepseek.Option{
		deepseek.WithTimeout(60 * time.Second),
	}
	if baseURL = strings.TrimSpace(baseURL); baseURL != "" {
		// DeepSeek SDK 内部用 BaseURL + Path 做纯字符串拼接（不会自动补分隔符，
		// Path 默认值为不带前导 "/" 的 "chat/completions"），若用户配置的
		// DEEPSEEK_BASE_URL 缺少结尾斜杠（如 "https://api.deepseek.com/v1"），会拼出
		// 形如 "https://api.deepseek.com/v1chat/completions" 的畸形地址，导致请求
		// 404。这里统一补全结尾斜杠，避免因配置差异导致的隐蔽故障。
		if !strings.HasSuffix(baseURL, "/") {
			baseURL += "/"
		}
		opts = append(opts, deepseek.WithBaseURL(baseURL))
	}

	cli, err := deepseek.NewClientWithOptions(apiKey, opts...)
	if err != nil {
		// 密钥格式等问题导致真实客户端初始化失败时，打印错误信息便于定位配置问题，
		// 由调用方决定降级策略。
		fmt.Printf("llm: 初始化 DeepSeek 客户端失败（%v），已降级为本地 Echo 客户端\n", err)
		return nil
	}
	return &DeepSeekClient{cli: cli}
}

func (c *DeepSeekClient) Chat(ctx context.Context, req ChatRequest) (string, error) {
	resp, err := c.cli.CreateChatCompletion(ctx, &deepseek.ChatCompletionRequest{
		Model:    resolveModel(req.Model),
		Messages: buildMessages(req),
	})
	if err != nil {
		return "", fmt.Errorf("llm: deepseek chat completion: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("llm: empty choices in response")
	}
	return resp.Choices[0].Message.Content, nil
}

// ChatStream 对接 DeepSeek 官方 SDK 的流式 Chat Completions 接口
// （CreateChatCompletionStream + Recv 循环），将每个 chunk 的增量内容转发到返回的 channel。
func (c *DeepSeekClient) ChatStream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error) {
	stream, err := c.cli.CreateChatCompletionStream(ctx, &deepseek.StreamChatCompletionRequest{
		Model:    resolveModel(req.Model),
		Messages: buildMessages(req),
		Stream:   true,
	})
	if err != nil {
		return nil, fmt.Errorf("llm: deepseek chat completion stream: %w", err)
	}

	out := make(chan StreamChunk)
	go func() {
		defer close(out)
		defer stream.Close()
		for {
			resp, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				out <- StreamChunk{Err: fmt.Errorf("llm: deepseek stream recv: %w", err)}
				return
			}
			for _, choice := range resp.Choices {
				if choice.Delta.Content == "" {
					continue
				}
				select {
				case out <- StreamChunk{Delta: choice.Delta.Content}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// buildMessages 将 ChatRequest 中的 system prompt / 历史消息 / 本轮用户输入
// 拼装为 DeepSeek SDK 所需的消息列表，供 Chat 与 ChatStream 共用。
func buildMessages(req ChatRequest) []deepseek.ChatCompletionMessage {
	msgs := make([]deepseek.ChatCompletionMessage, 0, len(req.History)+2)
	if req.SystemPrompt != "" {
		msgs = append(msgs, deepseek.ChatCompletionMessage{
			Role:    deepseek.ChatMessageRoleSystem,
			Content: req.SystemPrompt,
		})
	}
	for _, h := range req.History {
		role := deepseek.ChatMessageRoleUser
		if h.Role == "assistant" {
			role = deepseek.ChatMessageRoleAssistant
		}
		msgs = append(msgs, deepseek.ChatCompletionMessage{Role: role, Content: h.Content})
	}
	msgs = append(msgs, deepseek.ChatCompletionMessage{
		Role:    deepseek.ChatMessageRoleUser,
		Content: req.UserMessage,
	})
	return msgs
}

func resolveModel(model string) string {
	if model == "" {
		return deepseek.DeepSeekChat
	}
	return model
}

// EchoClient 是无需任何密钥即可运行的本地兜底实现，
// 用于演示 Agent 的 prompt / 模型 / 工具 / Skill 配置是如何生效的，
// 而不依赖任何真实的外部大模型服务。
type EchoClient struct{}

func (c *EchoClient) Chat(_ context.Context, req ChatRequest) (string, error) {
	return echoReply(req), nil
}

// echoStreamChunkSize 是 Echo 客户端模拟流式输出时，每个 chunk 携带的字符数。
const echoStreamChunkSize = 6

// ChatStream 将完整的模拟回复按固定大小切成若干片段，
// 并以短暂间隔逐个写入 channel，模拟真实流式输出的“打字机”效果，
// 便于在未配置 DEEPSEEK_API_KEY 时也能验证前端的流式对话界面。
func (c *EchoClient) ChatStream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error) {
	reply := echoReply(req)
	out := make(chan StreamChunk)
	go func() {
		defer close(out)
		runes := []rune(reply)
		for i := 0; i < len(runes); i += echoStreamChunkSize {
			end := min(i+echoStreamChunkSize, len(runes))
			select {
			case out <- StreamChunk{Delta: string(runes[i:end])}:
			case <-ctx.Done():
				return
			}
			select {
			case <-time.After(30 * time.Millisecond):
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func echoReply(req ChatRequest) string {
	var b strings.Builder
	b.WriteString("[本地演示模式，未配置 DEEPSEEK_API_KEY，以下为模拟回复]\n\n")
	if req.SystemPrompt != "" {
		b.WriteString("我是按照如下 System Prompt 运作的 Agent：\n")
		b.WriteString(req.SystemPrompt)
		b.WriteString("\n\n")
	}
	b.WriteString("收到你的消息：")
	b.WriteString(req.UserMessage)
	b.WriteString("\n\n若要接入真实的 DeepSeek 模型，请设置环境变量 DEEPSEEK_API_KEY（以及可选的 DEEPSEEK_BASE_URL）后重启服务。")
	return b.String()
}
