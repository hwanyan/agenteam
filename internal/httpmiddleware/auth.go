// Package httpmiddleware 提供 HTTP 网关层的通用中间件（Bearer 用户 Token 鉴权）。
//
// 本服务不处理 CORS：生产环境前端与 API 经 nginx 同源访问，本地开发通过前端 dev server 的 proxy 转发，
// 浏览器始终视为同源请求。若将来需要跨源访问（第三方站点、分域部署），再引入精确来源白名单的 CORS 中间件。
package httpmiddleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/hwanyan/agenteam/internal/authclient"
	"github.com/hwanyan/agenteam/internal/authctx"
)

// TokenVerifier 校验用户 JWT 的能力（由 hwan 认证服务提供，见 internal/authclient）
type TokenVerifier interface {
	VerifyToken(ctx context.Context, token string) (*authclient.Identity, error)
}

// UserFromContext 取出鉴权中间件注入的当前用户。
func UserFromContext(ctx context.Context) (*authclient.Identity, bool) {
	return authctx.FromContext(ctx)
}

const (
	unauthenticatedBody = `{"code":16,"message":"unauthenticated: missing or invalid bearer token"}`
	forbiddenBody       = `{"code":7,"message":"permission denied: account disabled"}`
	unavailableBody     = `{"code":14,"message":"authentication service unavailable, please retry later"}`
)

// Auth 返回要求 `Authorization: Bearer <用户 Token>` 的鉴权中间件。
//
// Token 交给 hwan 认证服务校验，成功后把用户身份注入 context，可通过 UserFromContext 读取。
// 认证服务不可达时返回 503 而不是 401，避免服务抖动把所有用户踢下线。
// verifier 为 nil 时所有请求都会被拒绝（fail-closed）。
func Auth(verifier TokenVerifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := BearerToken(r.Header.Get("Authorization"))
		if !ok || verifier == nil {
			writeUnauthenticated(w)
			return
		}

		identity, err := verifier.VerifyToken(r.Context(), token)
		switch {
		case err == nil:
			next.ServeHTTP(w, r.WithContext(authctx.WithIdentity(r.Context(), identity)))
		case errors.Is(err, authclient.ErrForbidden):
			writeJSON(w, http.StatusForbidden, forbiddenBody)
		case errors.Is(err, authclient.ErrUnavailable):
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusServiceUnavailable, unavailableBody)
		default:
			writeUnauthenticated(w)
		}
	})
}

func writeUnauthenticated(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="agenteam"`)
	writeJSON(w, http.StatusUnauthorized, unauthenticatedBody)
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// BearerToken 从 Authorization 头中提取 Bearer token（scheme 不区分大小写）。
func BearerToken(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}
