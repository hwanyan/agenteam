// Package idgen 提供简单的唯一 ID 生成能力，避免引入额外的第三方依赖。
package idgen

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// New 生成形如 "{prefix}-{12位随机hex}" 的唯一 ID。
func New(prefix string) string {
	buf := make([]byte, 8)
	// crypto/rand 在极端情况下可能返回 err，这里降级为时间戳无法保证唯一性，
	// 因此直接 panic 更安全，属于不可恢复的系统级错误。
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Errorf("idgen: read random bytes: %w", err))
	}
	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(buf))
}
