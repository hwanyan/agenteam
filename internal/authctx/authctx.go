// Package authctx 在 context 中携带“已通过鉴权的当前用户身份”。
//
// 身份由两处入口注入，业务层（internal/service）统一从这里读取：
//   - HTTP 网关的 Bearer 鉴权中间件（internal/httpmiddleware）：Team / Agent 以进程内方式挂载，
//     直接沿用 HTTP 请求的 context；
//   - gRPC 服务端拦截器（internal/grpcauth）：Workspace 因为要支持流式，网关会真实拨号到本地 gRPC 端口，
//     context 里的值不会跨越这次调用，只有 authorization 头会作为 metadata 传过来，
//     所以必须在 gRPC 服务端重新校验 Token 并注入身份；同时直连 gRPC 端口也无法绕过鉴权。
package authctx

import (
	"context"

	"github.com/hwanyan/agenteam/internal/authclient"
)

type ctxKey struct{}

// WithIdentity 返回携带用户身份的 context。
func WithIdentity(ctx context.Context, id *authclient.Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext 取出当前用户身份；没有身份（未经过鉴权）时第二个返回值为 false。
func FromContext(ctx context.Context) (*authclient.Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(*authclient.Identity)
	return id, ok && id != nil
}
