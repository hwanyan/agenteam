// Package httpauth 提供前端使用的登录 / 注册 HTTP 接口。
//
// hwan 认证服务只提供 gRPC，不对外提供 HTTP，因此由 agenteam 作为代理：
// 浏览器 → agenteam（HTTP/JSON）→ hwan 认证服务（gRPC，携带服务凭证）。
//
// 路由（均为 POST，请求/响应 JSON，字段为 camelCase，与网关其它接口一致）：
//
//	/v1/auth:sendVerifyCode  发送邮箱验证码                 （公开）
//	/v1/auth:register        注册，返回 token + user       （公开）
//	/v1/auth:login           登录，返回 token + user       （公开）
//	/v1/auth:refresh         用 refreshToken 换新的 token  （公开，身份由 refreshToken 证明）
//	/v1/auth:logout          登出，吊销当前登录链路          （携带 Bearer token，允许已过期）
//
// 路径风格与 agently 保持一致：「资源:动作」（Google API 自定义方法风格）。
//
// 这些路径必须挂载在鉴权中间件之外：登录前没有 Access token，刷新 / 登出时 Access Token 往往已经过期。
package httpauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hwanyan/agenteam/internal/httpmiddleware"
	authpb "github.com/hwanyan/proto/auth"
)

// 本包处理的路径：/v1/auth:动作
const (
	PathSendVerifyCode = "/v1/auth:sendVerifyCode"
	PathRegister       = "/v1/auth:register"
	PathLogin          = "/v1/auth:login"
	PathRefresh        = "/v1/auth:refresh"
	PathLogout         = "/v1/auth:logout"
)

// Paths 本包处理的全部路径，供上层逐个挂载到 mux
// （含 ":" 的路径无法用 ServeMux 的前缀模式统一匹配，所以显式列出）
var Paths = []string{PathSendVerifyCode, PathRegister, PathLogin, PathRefresh, PathLogout}

// maxBodyBytes 请求体上限。登录 / 注册只有几个短字段，64KB 绰绰有余，防止被灌大包
const maxBodyBytes = 64 << 10

// Service 认证能力（由 internal/authclient.Client 实现）
type Service interface {
	SendVerifyCode(ctx context.Context, email, purpose string) (string, error)
	Register(ctx context.Context, req *authpb.RegisterRequest) (*authpb.AuthResponse, error)
	Login(ctx context.Context, username, password string) (*authpb.AuthResponse, error)
	Refresh(ctx context.Context, refreshToken string) (*authpb.AuthResponse, error)
	Logout(ctx context.Context, token string) error
}

// Handler 登录 / 注册 HTTP 处理器
type Handler struct {
	svc Service
}

// New 创建处理器
func New(svc Service) *Handler { return &Handler{svc: svc} }

// ---- 请求 / 响应结构 ----

type sendVerifyCodeReq struct {
	Email   string `json:"email"`
	Purpose string `json:"purpose"`
}

type registerReq struct {
	Username   string `json:"username"`
	Email      string `json:"email"`
	Password   string `json:"password"`
	Nickname   string `json:"nickname"`
	VerifyCode string `json:"verifyCode"`
}

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type userJSON struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Email     string `json:"email"`
	Nickname  string `json:"nickname"`
	AvatarURL string `json:"avatarUrl"`
	// Role 平台级角色（user / admin）。仅供前端决定是否展示“全部团队”等管理员入口；
	// 真正的授权始终由后端按 Access Token 里的 roles 判断，不依赖前端。
	Role string `json:"role"`
}

type refreshReq struct {
	RefreshToken string `json:"refreshToken"`
}

type authResp struct {
	Token        string   `json:"token"`        // Access Token（短寿命），以 Authorization: Bearer 携带
	RefreshToken string   `json:"refreshToken"` // Refresh Token，仅用于调用 /v1/auth:refresh；每次刷新都会轮换
	ExpiresIn    int64    `json:"expiresIn"`    // Access Token 有效期（秒）
	User         userJSON `json:"user"`
}

// ServeHTTP 按路径分发
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 登录态相关响应绝不能被浏览器或中间代理缓存
	w.Header().Set("Cache-Control", "no-store")

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, status.Error(codes.InvalidArgument, "仅支持 POST 方法"), http.StatusMethodNotAllowed)
		return
	}

	switch r.URL.Path {
	case PathSendVerifyCode:
		h.sendVerifyCode(w, r)
	case PathRegister:
		h.register(w, r)
	case PathLogin:
		h.login(w, r)
	case PathRefresh:
		h.refresh(w, r)
	case PathLogout:
		h.logout(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) sendVerifyCode(w http.ResponseWriter, r *http.Request) {
	var req sendVerifyCodeReq
	if !decode(w, r, &req) {
		return
	}
	if req.Email == "" {
		writeError(w, status.Error(codes.InvalidArgument, "邮箱地址不能为空"), 0)
		return
	}
	msg, err := h.svc.SendVerifyCode(r.Context(), req.Email, req.Purpose)
	if err != nil {
		writeError(w, err, 0)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": msg})
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if !decode(w, r, &req) {
		return
	}
	resp, err := h.svc.Register(r.Context(), &authpb.RegisterRequest{
		Username:   req.Username,
		Email:      req.Email,
		Password:   req.Password,
		Nickname:   req.Nickname,
		VerifyCode: req.VerifyCode,
	})
	if err != nil {
		writeError(w, err, 0)
		return
	}
	writeJSON(w, http.StatusOK, toAuthResp(resp))
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if !decode(w, r, &req) {
		return
	}
	resp, err := h.svc.Login(r.Context(), req.Username, req.Password)
	if err != nil {
		writeError(w, err, 0)
		return
	}
	writeJSON(w, http.StatusOK, toAuthResp(resp))
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshReq
	if !decode(w, r, &req) {
		return
	}
	if req.RefreshToken == "" {
		writeError(w, status.Error(codes.Unauthenticated, "refreshToken 不能为空"), 0)
		return
	}
	resp, err := h.svc.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		writeError(w, err, 0)
		return
	}
	writeJSON(w, http.StatusOK, toAuthResp(resp))
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	token, ok := httpmiddleware.BearerToken(r.Header.Get("Authorization"))
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="agenteam"`)
		writeError(w, status.Error(codes.Unauthenticated, "缺少 Token"), 0)
		return
	}
	if err := h.svc.Logout(r.Context(), token); err != nil {
		writeError(w, err, 0)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "已退出登录"})
}

// ---- 辅助 ----

func toAuthResp(r *authpb.AuthResponse) authResp {
	u := r.GetUser()
	return authResp{
		Token:        r.GetToken(),
		RefreshToken: r.GetRefreshToken(),
		ExpiresIn:    r.GetExpiresIn(),
		User: userJSON{
			ID:        u.GetId(),
			Username:  u.GetUsername(),
			Email:     u.GetEmail(),
			Nickname:  u.GetNickname(),
			AvatarURL: u.GetAvatarUrl(),
			Role:      u.GetRole(),
		},
	}
}

// decode 解析 JSON 请求体，失败时已写入 400 响应并返回 false
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			writeError(w, status.Error(codes.InvalidArgument, "请求体过大"), http.StatusRequestEntityTooLarge)
		case errors.Is(err, io.EOF):
			writeError(w, status.Error(codes.InvalidArgument, "请求体不能为空"), 0)
		default:
			writeError(w, status.Error(codes.InvalidArgument, "请求体不是合法的 JSON"), 0)
		}
		return false
	}
	return true
}

// writeError 输出与 grpc-gateway 一致的错误格式：{"code": <gRPC code>, "message": "..."}。
// forceStatus 非 0 时覆盖 HTTP 状态码，否则按 gRPC code 映射。
// Internal / Unknown 等非面向用户的错误不透出细节。
func writeError(w http.ResponseWriter, err error, forceStatus int) {
	st := status.Convert(err)
	httpStatus := runtime.HTTPStatusFromCode(st.Code())
	if forceStatus != 0 {
		httpStatus = forceStatus
	}

	msg := st.Message()
	switch st.Code() {
	case codes.Internal, codes.Unknown, codes.DataLoss, codes.Unimplemented:
		log.Printf("httpauth: 认证服务返回内部错误: code=%s msg=%s", st.Code(), st.Message())
		msg = "服务内部错误，请稍后重试"
	}
	if st.Code() == codes.Unavailable {
		w.Header().Set("Retry-After", "1")
	}

	writeJSON(w, httpStatus, map[string]any{"code": int32(st.Code()), "message": msg})
}

func writeJSON(w http.ResponseWriter, httpStatus int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(v)
}
