package config

import (
	"os"
	"strings"
)

// Config 应用配置，全部来自环境变量（敏感信息仅从环境读取，不接受任何用户输入覆盖）。
type Config struct {
	HTTPAddr string // 对外 HTTP/JSON (gateway) 监听地址
	GRPCAddr string // 内部 gRPC 监听地址（仅 loopback）

	// PostgreSQL：存储 teams / agents 等关系型配置数据。
	PostgresDSN string

	// Redis：存储 Agent 运行态热缓存（KV）。连接串形如 redis://[:password@]host:port/db
	RedisDSN string

	// MongoDB：存储聊天记录（chat_messages，文档型）。
	MongoURI      string
	MongoDatabase string

	// DeepSeek：LLM 对话能力，详见 internal/llm。未设置 DeepSeekAPIKey 时，
	// internal/llm 会降级为本地 Echo 客户端，无需任何密钥即可跑通整套流程。
	DeepSeekAPIKey  string
	DeepSeekBaseURL string

	// 鉴权：HTTP 网关要求请求携带 `Authorization: Bearer <token>`。
	// AuthTokens 为合法 token 列表（逗号分隔，便于轮换时新旧并存）。
	// 未配置任何 token 时服务拒绝启动（见 main.go）。
	AuthTokens []string
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// splitCSV 按逗号拆分并去除空白与空项。
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Load 从环境变量加载配置，提供合理默认值便于本地开发。
// 数据库连接串/密码/密钥等敏感信息只允许通过环境变量注入。
func Load() *Config {
	return &Config{
		AuthTokens: splitCSV(os.Getenv("AGENTEAM_AUTH_TOKENS")),

		HTTPAddr: getenv("AGENTEAM_HTTP_ADDR", ":8080"),
		GRPCAddr: getenv("AGENTEAM_GRPC_ADDR", "127.0.0.1:9090"),

		PostgresDSN: getenv("AGENTEAM_POSTGRES_DSN",
			"postgres://localhost:5432/agenteam?sslmode=disable"),

		MongoURI:      getenv("AGENTEAM_MONGO_URI", "mongodb://127.0.0.1:27017"),
		MongoDatabase: getenv("AGENTEAM_MONGO_DATABASE", "agenteam"),

		RedisDSN: getenv("AGENTEAM_REDIS_DSN", "redis://127.0.0.1:6379/0"),

		DeepSeekAPIKey:  getenv("DEEPSEEK_API_KEY", ""),
		DeepSeekBaseURL: getenv("DEEPSEEK_BASE_URL", ""),
	}
}
