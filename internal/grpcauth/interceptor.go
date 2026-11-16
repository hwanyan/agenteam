// Package grpcauth 提供 gRPC 服务端的 Bearer 鉴权拦截器。
//
// 为什么 gRPC 服务端要自己再校验一次 Token：
//   - Workspace 含流式接口，HTTP 网关对它不能进程内直调，而是真实拨号到本地 gRPC 端口。
//     HTTP 中间件注入在请求 context 里的用户身份不会跨越这次 gRPC 调用，只有 authorization 头
//     （grpc-gateway 会原样转成同名 metadata）会被带过来，服务端需据此重新取得身份；
//   - gRPC 端口本身不经过 HTTP 中间件，直连它的任何客户端都不应绕过鉴权。
//
// Access Token 在本地验签（公钥带缓存），重复校验没有网络开销。
package grpcauth

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/hwanyan/agenteam/internal/authclient"
	"github.com/hwanyan/agenteam/internal/authctx"
	"github.com/hwanyan/agenteam/internal/httpmiddleware"
)

// TokenVerifier 校验用户 JWT 的能力（internal/authclient.Client 已实现）。
type TokenVerifier interface {
	VerifyToken(ctx context.Context, token string) (*authclient.Identity, error)
}

// Unary 返回一元 RPC 的鉴权拦截器：校验 authorization metadata 中的 Bearer Token，并把身份注入 context。
// verifier 为 nil 时所有请求都会被拒绝（fail-closed）。
func Unary(verifier TokenVerifier) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, err := authenticate(ctx, verifier)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// Stream 返回流式 RPC 的鉴权拦截器，行为与 Unary 一致；流的 Context() 会携带用户身份。
func Stream(verifier TokenVerifier) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, err := authenticate(ss.Context(), verifier)
		if err != nil {
			return err
		}
		return handler(srv, &identityStream{ServerStream: ss, ctx: ctx})
	}
}

// identityStream 覆盖 Context()，让流式 handler 通过 stream.Context() 取到带身份的 context。
type identityStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *identityStream) Context() context.Context { return s.ctx }

func authenticate(ctx context.Context, verifier TokenVerifier) (context.Context, error) {
	if verifier == nil {
		return nil, status.Error(codes.Unauthenticated, "unauthenticated: missing or invalid bearer token")
	}
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	if len(values) == 0 {
		return nil, status.Error(codes.Unauthenticated, "unauthenticated: missing or invalid bearer token")
	}
	token, ok := httpmiddleware.BearerToken(values[0])
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "unauthenticated: missing or invalid bearer token")
	}

	identity, err := verifier.VerifyToken(ctx, token)
	switch {
	case err == nil:
		return authctx.WithIdentity(ctx, identity), nil
	case errors.Is(err, authclient.ErrForbidden):
		return nil, status.Error(codes.PermissionDenied, "permission denied: account disabled")
	case errors.Is(err, authclient.ErrUnavailable):
		return nil, status.Error(codes.Unavailable, "authentication service unavailable, please retry later")
	default:
		return nil, status.Error(codes.Unauthenticated, "unauthenticated: missing or invalid bearer token")
	}
}
