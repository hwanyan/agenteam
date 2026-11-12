// Package authclient 封装对 hwan 认证服务（AuthService）的 gRPC 调用。
//
// agenteam 不持有用户表与 JWT 密钥，注册 / 登录 / 验证码 / Token 校验 / 登出都委托给 hwan 认证服务。
// 每个出站 RPC 都会携带服务调用方凭证（authorization: Bearer <service_token>），
// 认证服务据此确认「调用者是 agenteam」。
package authclient

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/hwanyan/authkit/verifier"
	authpb "github.com/hwanyan/proto/auth"
)

const (
	// verifyTimeout 强校验（回源）的超时
	verifyTimeout = 3 * time.Second
	// callTimeout 注册 / 登录 / 登出等一般调用的超时
	callTimeout = 5 * time.Second
	// sendMailTimeout 发送验证码需要等待 SMTP 投递，留得更长
	sendMailTimeout = 30 * time.Second

	unavailableMsg = "认证服务暂不可用，请稍后重试"

	// reasonInvalidServiceToken 认证服务在服务凭证缺失/无效时附带的 ErrorInfo.Reason
	reasonInvalidServiceToken = "INVALID_SERVICE_TOKEN"
)

// 校验失败的分类，供鉴权中间件映射为 HTTP 状态码。
var (
	// ErrUnauthenticated Token 无效或已过期（前端应先 Refresh，失败再重新登录）
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrForbidden 账号已被禁用（仅强校验 VerifyTokenStrict 会返回）
	ErrForbidden = errors.New("forbidden")
	// ErrUnavailable 认证服务不可达、超时或服务凭证被拒绝
	// （与「Token 无效」区分，避免服务抖动或部署配置错误时把用户踢下线）
	ErrUnavailable = errors.New("auth service unavailable")
)

// Identity 校验通过后的用户身份（来自 Access Token 载荷）
type Identity struct {
	UserID   string
	Username string
	Roles    []string // 平台级角色（user / admin）；团队内角色（owner / member 等）由 agenteam 自己的库维护
}

// HasRole 是否拥有指定的平台级角色
func (i *Identity) HasRole(role string) bool { return slices.Contains(i.Roles, role) }

// serviceTokenCreds 把服务调用方凭证附加到每个出站 RPC 的 metadata
type serviceTokenCreds string

func (t serviceTokenCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(t)}, nil
}

// RequireTransportSecurity 返回 false：认证服务部署在内网，当前以明文 gRPC 通信。
// 若跨网络/跨主机部署，应改用 TLS，避免凭证被窃听。
func (serviceTokenCreds) RequireTransportSecurity() bool { return false }

// Client hwan 认证服务 gRPC 客户端
type Client struct {
	conn     *grpc.ClientConn
	client   authpb.AuthServiceClient
	verifier *verifier.Verifier // Access Token 本地验签（公钥取自认证服务 GetJWKS，带缓存）
}

// New 创建认证服务客户端。
//   - addr:  认证服务 gRPC 地址，如 127.0.0.1:50054
//   - token: 调用方凭证，对应认证服务 server.services 里 name 为 agenteam 那一项的 token
//
// grpc.NewClient 是惰性连接：不会真正拨号，认证服务暂时不可用不会阻塞 agenteam 启动；
// 实际调用时才建立连接，不可用时返回 ErrUnavailable。
func New(addr, token string) (*Client, error) {
	if addr == "" {
		return nil, fmt.Errorf("认证服务地址不能为空")
	}
	if token == "" {
		return nil, fmt.Errorf("认证服务调用方凭证不能为空")
	}
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(serviceTokenCreds(token)),
	)
	if err != nil {
		return nil, fmt.Errorf("创建认证服务 gRPC 客户端失败: %w", err)
	}
	client := authpb.NewAuthServiceClient(conn)
	return &Client{conn: conn, client: client, verifier: verifier.New(client)}, nil
}

// Close 关闭连接
func (c *Client) Close() error { return c.conn.Close() }

// VerifyToken 在本地校验 Access Token（签名、过期、iss、aud），返回用户身份。
// 不回源认证服务、不查库；公钥由 verifier 缓存，首次使用及密钥轮换时才会调用 GetJWKS。
// 失败时返回 ErrUnauthenticated / ErrUnavailable 之一（可用 errors.Is 判断）。
//
// 因为是本地验签，账号被禁用、角色变更最多滞后一个 Access Token 有效期才对已签发的 Token 生效；
// 需要即时结论的敏感操作请用 VerifyTokenStrict。
func (c *Client) VerifyToken(ctx context.Context, token string) (*Identity, error) {
	claims, err := c.verifier.Verify(ctx, token)
	if err != nil {
		if errors.Is(err, verifier.ErrKeysUnavailable) {
			log.Printf("authclient: 无法取得 Token 验签公钥: %v", err)
			return nil, ErrUnavailable
		}
		return nil, ErrUnauthenticated
	}
	return &Identity{UserID: claims.UserID(), Username: claims.Username, Roles: claims.Roles}, nil
}

// VerifyTokenStrict 在本地验签之外，再回源认证服务确认用户当前仍存在且未被禁用（强校验）。
// 比 VerifyToken 多一次 RPC，仅用于敏感操作（如删除团队、修改凭证）。
func (c *Client) VerifyTokenStrict(ctx context.Context, token string) (*Identity, error) {
	id, err := c.VerifyToken(ctx, token)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	if _, err := c.client.VerifyToken(ctx, &authpb.VerifyTokenRequest{Token: token}); err != nil {
		st := status.Convert(err)
		if isInvalidServiceToken(st) {
			log.Printf("authclient: VerifyToken 被拒绝：服务凭证无效，请检查与认证服务 server.services（agenteam 项）的 token 是否一致")
			return nil, ErrUnavailable
		}
		switch st.Code() {
		case codes.Unauthenticated:
			return nil, ErrUnauthenticated
		case codes.PermissionDenied:
			return nil, ErrForbidden
		default:
			log.Printf("authclient: VerifyToken 调用失败: code=%s msg=%s", st.Code(), st.Message())
			return nil, ErrUnavailable
		}
	}
	return id, nil
}

// Refresh 用 Refresh Token 换取新的 Access Token + 新的 Refresh Token（轮换）
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*authpb.AuthResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	resp, err := c.client.Refresh(ctx, &authpb.RefreshRequest{RefreshToken: refreshToken})
	if err != nil {
		return nil, translate("Refresh", err)
	}
	return resp, nil
}

// SendVerifyCode 发送邮箱验证码，返回认证服务给出的提示文案
func (c *Client) SendVerifyCode(ctx context.Context, email, purpose string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, sendMailTimeout)
	defer cancel()

	resp, err := c.client.SendVerifyCode(ctx, &authpb.SendVerifyCodeRequest{Email: email, Purpose: purpose})
	if err != nil {
		return "", translate("SendVerifyCode", err)
	}
	return resp.GetMessage(), nil
}

// Register 用户注册
func (c *Client) Register(ctx context.Context, req *authpb.RegisterRequest) (*authpb.AuthResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	resp, err := c.client.Register(ctx, req)
	if err != nil {
		return nil, translate("Register", err)
	}
	return resp, nil
}

// Login 用户登录（用户名或邮箱）
func (c *Client) Login(ctx context.Context, username, password string) (*authpb.AuthResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	resp, err := c.client.Login(ctx, &authpb.LoginRequest{Username: username, Password: password})
	if err != nil {
		return nil, translate("Login", err)
	}
	return resp, nil
}

// Logout 登出：通知认证服务吊销该登录链路下的全部 Refresh Token，之后无法再刷新。
// Token 允许已过期（由认证服务校验签名）。幂等。
//
// 注意：Access Token 在业务服务本地验签、没有「已登出名单」，所以已签发的 Access Token 在自然过期前
// （最长 access_ttl，默认 15 分钟）仍能通过验签，这是有意接受的取舍；敏感操作请用 VerifyTokenStrict 强校验。
func (c *Client) Logout(ctx context.Context, token string) error {
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if _, err := c.client.Logout(callCtx, &authpb.LogoutRequest{Token: token}); err != nil {
		return translate("Logout", err)
	}
	return nil
}

// translate 把调用错误整理成可直接对外暴露的 gRPC status 错误：
//   - 连接类 / 超时类错误：详情写日志，对外统一为 Unavailable，避免泄露内网地址等信息
//   - 服务凭证被认证服务拒绝：属于部署问题，写日志并按 Unavailable 返回
//   - 其它业务错误（带 code 与面向用户的文案）：原样透传
func translate(method string, err error) error {
	st := status.Convert(err)
	if isInvalidServiceToken(st) {
		log.Printf("authclient: %s 被拒绝：服务凭证无效，请检查与认证服务 server.services（agenteam 项）的 token 是否一致", method)
		return status.Error(codes.Unavailable, unavailableMsg)
	}
	switch st.Code() {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		log.Printf("authclient: %s 调用失败: code=%s msg=%s", method, st.Code(), st.Message())
		return status.Error(codes.Unavailable, unavailableMsg)
	}
	return err
}

// isInvalidServiceToken 判断错误是否为认证服务因服务凭证无效而拒绝（通过 ErrorInfo.Reason 识别）
func isInvalidServiceToken(st *status.Status) bool {
	if st.Code() != codes.Unauthenticated {
		return false
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetReason() == reasonInvalidServiceToken {
			return true
		}
	}
	return false
}
