// Package httpmiddleware 提供 HTTP 网关层的通用中间件（目前为 Bearer token 鉴权）。
//
// 本服务不处理 CORS：生产环境前端与 API 经 nginx 同源访问，本地开发通过前端 dev server 的 proxy 转发，
// 浏览器始终视为同源请求。若将来需要跨源访问（第三方站点、分域部署），再引入精确来源白名单的 CORS 中间件。
package httpmiddleware

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

const unauthenticatedBody = `{"code":16,"message":"unauthenticated: missing or invalid bearer token"}`

// Auth 返回要求 `Authorization: Bearer <token>` 的鉴权中间件。
//
// token 比较采用定长摘要 + 常量时间比较，并遍历全部合法 token（不提前返回），
// 避免通过响应时间推测 token 内容或长度。tokens 为空时所有请求都会被拒绝（fail-closed）。
func Auth(tokens []string, next http.Handler) http.Handler {
	digests := make([][sha256.Size]byte, 0, len(tokens))
	for _, t := range tokens {
		if t != "" {
			digests = append(digests, sha256.Sum256([]byte(t)))
		}
	}

	valid := func(candidate string) bool {
		sum := sha256.Sum256([]byte(candidate))
		match := 0
		for i := range digests {
			match |= subtle.ConstantTimeCompare(sum[:], digests[i][:])
		}
		return match == 1
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || !valid(token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="agenteam"`)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(unauthenticatedBody))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken 从 Authorization 头中提取 Bearer token（scheme 不区分大小写）。
func bearerToken(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}
