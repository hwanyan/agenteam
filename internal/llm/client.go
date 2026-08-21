// Package llm 封装与大模型对话的能力。
//
// 默认实现是一个兼容 OpenAI Chat Completions 协议的 HTTP 客户端，
// 可通过环境变量 AGENTEAM_LLM_BASE_URL / AGENTEAM_LLM_API_KEY 指向任意
// 兼容 OpenAI 协议的服务商（OpenAI / DeepSeek / 通义千问 / 智谱等均提供兼容接口）。
//
// 安全说明：Base URL 与 API Key 均只允许通过环境变量配置（遵循 secrets 只走 env
// 的原则），不接受来自用户请求的动态取值，避免 SSRF 风险。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

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
}

// Client 定义与 LLM 对话的能力，方便替换/测试。
type Client interface {
	Chat(ctx context.Context, req ChatRequest) (string, error)
}

// NewFromEnv 根据环境变量构建 LLM 客户端。
// 若未配置 AGENTEAM_LLM_API_KEY，则降级为本地 Echo 客户端，方便无密钥场景下跑通整套流程。
func NewFromEnv() Client {
	apiKey := os.Getenv("AGENTEAM_LLM_API_KEY")
	if apiKey == "" {
		return &EchoClient{}
	}
	baseURL := os.Getenv("AGENTEAM_LLM_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAICompatClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		httpCli: &http.Client{Timeout: 60 * time.Second},
	}
}

// OpenAICompatClient 是兼容 OpenAI Chat Completions 协议的实现。
type OpenAICompatClient struct {
	baseURL string
	apiKey  string
	httpCli *http.Client
}

type chatCompletionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatCompletionRequest struct {
	Model    string                  `json:"model"`
	Messages []chatCompletionMessage `json:"messages"`
}

type chatCompletionResponse struct {
	Choices []struct {
		Message chatCompletionMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *OpenAICompatClient) Chat(ctx context.Context, req ChatRequest) (string, error) {
	msgs := make([]chatCompletionMessage, 0, len(req.History)+2)
	if req.SystemPrompt != "" {
		msgs = append(msgs, chatCompletionMessage{Role: "system", Content: req.SystemPrompt})
	}
	for _, h := range req.History {
		msgs = append(msgs, chatCompletionMessage{Role: h.Role, Content: h.Content})
	}
	msgs = append(msgs, chatCompletionMessage{Role: "user", Content: req.UserMessage})

	body, err := json.Marshal(chatCompletionRequest{Model: req.Model, Messages: msgs})
	if err != nil {
		return "", fmt.Errorf("llm: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("llm: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpCli.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("llm: call chat completions: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("llm: read response: %w", err)
	}

	var out chatCompletionResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("llm: unmarshal response: %w", err)
	}
	if out.Error != nil {
		return "", fmt.Errorf("llm: upstream error: %s", out.Error.Message)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("llm: upstream status %d: %s", resp.StatusCode, string(data))
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("llm: empty choices in response")
	}
	return out.Choices[0].Message.Content, nil
}

// EchoClient 是无需任何密钥即可运行的本地兜底实现，
// 用于演示 Agent 的 prompt / 模型 / 工具 / Skill 配置是如何生效的，
// 而不依赖任何真实的外部大模型服务。
type EchoClient struct{}

func (c *EchoClient) Chat(_ context.Context, req ChatRequest) (string, error) {
	var b strings.Builder
	b.WriteString("[本地演示模式，未配置 AGENTEAM_LLM_API_KEY，以下为模拟回复]\n\n")
	if req.SystemPrompt != "" {
		b.WriteString("我是按照如下 System Prompt 运作的 Agent：\n")
		b.WriteString(req.SystemPrompt)
		b.WriteString("\n\n")
	}
	b.WriteString("收到你的消息：")
	b.WriteString(req.UserMessage)
	b.WriteString("\n\n若要接入真实模型，请设置环境变量 AGENTEAM_LLM_API_KEY（以及可选的 AGENTEAM_LLM_BASE_URL）后重启服务。")
	return b.String(), nil
}
