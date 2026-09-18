// Package tools 提供项目内公共的小型工具函数（目前主要是唯一 ID 生成），
// 避免引入额外的第三方依赖。此前叫作 idgen，随着职责不再局限于"生成 ID"这一件事
// （例如 NewMCPToolID 还承担了"生成短小、可读的随机 ID"这类更通用的诉求），
// 改名为语义更宽泛的 tools。
package tools

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// New 生成形如 "{prefix}-{16位随机hex}" 的唯一 ID，用于内部系统 id（team/agent/msg 等），
// 不要求可读性、只要求足够的随机性与唯一性。
func New(prefix string) string {
	buf := make([]byte, 8)
	// crypto/rand 在极端情况下可能返回 err，这里降级为时间戳无法保证唯一性，
	// 因此直接 panic 更安全，属于不可恢复的系统级错误。
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Errorf("tools: read random bytes: %w", err))
	}
	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(buf))
}

// mcpToolIDCharset 是 NewMCPToolID 生成随机后缀时使用的字符集：小写字母 + 数字。
// 不做易混淆字符（如 0/o、1/l）过滤以保持实现简单。
const mcpToolIDCharset = "abcdefghijklmnopqrstuvwxyz0123456789"

// mcpToolIDSuffixLen 是 NewMCPToolID 生成的随机后缀长度。
const mcpToolIDSuffixLen = 6

// NewMCPToolID 生成形如 "{prefix}_{6位随机数字、字母}" 的唯一 ID，
// 例如 NewMCPToolID("mcp_tool") 可能返回 "mcp_tool_sc1so3"。
//
// 用于用户自行接入的自定义 MCP 工具等、需要短小可读 id 的场景（区别于 New()
// 生成的 16 位十六进制后缀——那种更长但基本不会冲突）。由于随机空间明显更小
// （36^6 ≈ 21.8 亿），理论上存在碰撞可能，调用方应结合存储层的唯一约束处理冲突
// （如捕获写入时的"已存在"错误后重新生成并重试）。
func NewMCPToolID(prefix string) string {
	buf := make([]byte, mcpToolIDSuffixLen)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Errorf("tools: read random bytes: %w", err))
	}
	suffix := make([]byte, mcpToolIDSuffixLen)
	for i, b := range buf {
		suffix[i] = mcpToolIDCharset[int(b)%len(mcpToolIDCharset)]
	}
	return fmt.Sprintf("%s-%s", prefix, suffix)
}
