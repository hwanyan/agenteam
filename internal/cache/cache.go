// Package cache 提供 Agent 运行态热缓存能力，由 Redis 承载（KV 存储）。
//
// 这里的“Agent 运行态”专指“当前生效、已通过校验并加载完成”的 Agent 配置快照，
// 访问模式是纯粹的按 agentID 整块读/写，没有任何范围查询或 join 需求，
// 是典型的 KV 场景；用 Redis 承载还能让该状态在多实例部署下被所有实例共享，
// 不再像最初实现那样只存在于单个进程的内存 map 里。
package cache

import "context"

// AgentSnapshot 是缓存中的 Agent 运行态快照，字段与 agenteamv1.Agent 对应，
// 使用独立类型而非直接缓存 proto 消息，避免 cache 包反向依赖具体业务 pb 结构以外的细节。
type AgentSnapshot struct {
	ID        string
	TeamID    string
	Name      string
	Prompt    string
	Model     string
	McpTools  []string
	Skills    []string
	IsMain    bool
	Version   int64
	Status    string // AgentStatus 枚举的字符串形式，如 "AGENT_STATUS_LOADED"
	CreatedAt int64
	UpdatedAt int64
}

// AgentCache 定义 Agent 运行态缓存的读写能力。
type AgentCache interface {
	// Set 写入/覆盖指定 Agent 的运行态快照。
	Set(ctx context.Context, snapshot *AgentSnapshot) error
	// Get 读取指定 Agent 的运行态快照；不存在时 ok 为 false。
	Get(ctx context.Context, agentID string) (snapshot *AgentSnapshot, ok bool, err error)
	// Delete 删除指定 Agent 的运行态快照（如 Agent/团队被删除时调用）。
	Delete(ctx context.Context, agentID string) error
}
