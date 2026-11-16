package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"

	gwruntime "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"

	"github.com/hwanyan/agenteam/internal/a2a"
	"github.com/hwanyan/agenteam/internal/authclient"
	"github.com/hwanyan/agenteam/internal/cache"
	"github.com/hwanyan/agenteam/internal/cleanup"
	"github.com/hwanyan/agenteam/internal/config"
	"github.com/hwanyan/agenteam/internal/grpcauth"
	"github.com/hwanyan/agenteam/internal/httpauth"
	"github.com/hwanyan/agenteam/internal/httpmiddleware"
	"github.com/hwanyan/agenteam/internal/llm"
	"github.com/hwanyan/agenteam/internal/store"
	"github.com/hwanyan/agenteam/internal/store/mongostore"
	"github.com/hwanyan/agenteam/internal/store/postgres"

	agentruntime "github.com/hwanyan/agenteam/internal/runtime"
	"github.com/hwanyan/agenteam/internal/service"
	agenteamv1 "github.com/hwanyan/agenteam/pb/gen"
)

func main() {
	cfg := config.Load()

	// 鉴权为必选项：用户注册 / 登录 / Token 校验全部由 hwan 认证服务（仅 gRPC）提供，
	// 地址与调用方凭证缺一不可，否则拒绝启动，避免无意中裸奔。
	if cfg.AuthServiceAddr == "" || cfg.AuthServiceToken == "" {
		log.Fatalf("未配置鉴权：请设置 AGENTEAM_AUTH_SERVICE_ADDR（hwan 认证服务 gRPC 地址）" +
			"和 AGENTEAM_AUTH_SERVICE_TOKEN（调用方凭证，须与认证服务 server.services 里 name 为 agenteam 那一项的 token 一致）")
	}
	connectCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// postgres 数据库
	pgStore, err := postgres.New(connectCtx, cfg.PostgresDSN)
	if err != nil {
		log.Fatalf("init postgres store: %v", err)
	}
	defer pgStore.Close()
	log.Printf("connected to PostgreSQL")

	// mongo 数据库
	msgStore, err := mongostore.New(connectCtx, cfg.MongoURI, cfg.MongoDatabase)
	if err != nil {
		log.Fatalf("init mongo store: %v", err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = msgStore.Close(closeCtx)
	}()
	log.Printf("connected to MongoDB")

	// redis
	agentCache, err := cache.NewRedisAgentCache(connectCtx, cfg.RedisDSN)
	if err != nil {
		log.Fatalf("init redis cache: %v", err)
	}
	defer agentCache.Close()
	log.Printf("connected to Redis")

	// 认证服务客户端：Access Token 在本地验签，不查库、不查 Redis
	authClient, err := authclient.New(cfg.AuthServiceAddr, cfg.AuthServiceToken)
	if err != nil {
		log.Fatalf("init auth service client: %v", err)
	}
	defer authClient.Close()
	log.Printf("auth enabled via hwan auth service at %s", cfg.AuthServiceAddr)

	st := store.New(pgStore, msgStore)
	deps := &service.Deps{
		Store:   st,
		Runtime: agentruntime.New(agentCache, st),
		LLM:     llm.New(cfg.DeepSeekAPIKey, cfg.DeepSeekBaseURL),
		A2A:     a2a.New(),
	}

	// 后台清理 worker：异步完成团队软删除后的收尾（卸载 Agent 运行态缓存、软删除聊天记录）。
	cleanupCtx, stopCleanup := context.WithCancel(context.Background())
	defer stopCleanup()
	go cleanup.New(st, deps.Runtime, cleanup.DefaultInterval, cleanup.DefaultBatchSize).Run(cleanupCtx)

	teamSrv := service.NewTeamServer(deps)
	agentSrv := service.NewAgentServer(deps)
	workspaceSrv := service.NewWorkspaceServer(deps)

	// gRPC 服务端自带 Bearer 鉴权：Workspace 走真实拨号转发，HTTP 中间件注入的用户身份不会跨越这次调用，
	// 只有 authorization 头会作为 metadata 传过来，必须在这里重新校验并注入身份；
	// 同时直连 gRPC 端口也无法绕过鉴权。
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(grpcauth.Unary(authClient)),
		grpc.StreamInterceptor(grpcauth.Stream(authClient)),
	)
	agenteamv1.RegisterTeamServiceServer(grpcServer, teamSrv)
	agenteamv1.RegisterAgentServiceServer(grpcServer, agentSrv)
	agenteamv1.RegisterWorkspaceServiceServer(grpcServer, workspaceSrv)
	// 让 grpcurl、Postman、BloomRPC、Evans 这类通用调试工具，就能在不带任何 .proto 文件的情况下
	// 直接对着这个 server 探测服务列表并发起调用。生产环境视情况关闭（避免 API 结构被随意探测）
	reflection.Register(grpcServer)

	go func() {
		lis, err := net.Listen("tcp", cfg.GRPCAddr)
		if err != nil {
			log.Fatalf("listen grpc: %v", err)
		}
		log.Printf("gRPC server listening on %s", cfg.GRPCAddr)
		if err := grpcServer.Serve(lis); err != nil {
			log.Fatalf("serve grpc: %v", err)
		}
	}()

	ctx := context.Background()
	mux := gwruntime.NewServeMux()
	// Team / Agent 均为一元 RPC，直接以进程内调用的方式挂载 handler，无需拨号。
	if err := agenteamv1.RegisterTeamServiceHandlerServer(ctx, mux, teamSrv); err != nil {
		log.Fatalf("register team gateway: %v", err)
	}
	if err := agenteamv1.RegisterAgentServiceHandlerServer(ctx, mux, agentSrv); err != nil {
		log.Fatalf("register agent gateway: %v", err)
	}
	// Workspace 含 SendMessageStream（server-streaming），grpc-gateway 的
	// in-process 直调模式（RegisterWorkspaceServiceHandlerServer）不支持流式转发
	// （见 pb/gen/workspace.pb.gw.go 中对应 handler 会直接返回 Unimplemented），
	// 因此改为真实拨号连接本地 gRPC 端口，经由 gRPC 客户端转发，以支持 HTTP chunked 流式响应。
	grpcConn, err := grpc.NewClient(dialTarget(cfg.GRPCAddr), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial local grpc for workspace gateway: %v", err)
	}
	if err := agenteamv1.RegisterWorkspaceServiceHandlerClient(ctx, mux, agenteamv1.NewWorkspaceServiceClient(grpcConn)); err != nil {
		log.Fatalf("register workspace gateway: %v", err)
	}

	// 路由分两层：
	//   - /v1/auth:*：登录、注册、发验证码、刷新、登出，由 httpauth 代理到 hwan 认证服务，不经过 Bearer 鉴权（登录前没有 token）；
	//   - 其余所有路径：先过 Bearer 鉴权，再进入 grpc-gateway。
	// 前端与 API 同源访问（生产经 nginx 反代，开发经 vite proxy），因此不处理 CORS。
	root := http.NewServeMux()
	authHandler := httpauth.New(authClient)
	for _, p := range httpauth.Paths {
		root.Handle(p, authHandler)
	}
	root.Handle("/", httpmiddleware.Auth(authClient, mux))

	log.Printf("HTTP gateway listening on %s", cfg.HTTPAddr)
	if err := http.ListenAndServe(cfg.HTTPAddr, root); err != nil {
		log.Fatalf("serve http gateway: %v", err)
	}
}

// dialTarget 将 gRPC 监听地址转换为可供客户端拨号的目标地址：
// 若地址形如 ":9090"（仅端口，用于服务端监听所有网卡），客户端拨号时需要
// 一个明确的 host，这里统一补上 127.0.0.1（网关与 gRPC server 运行在同一进程/主机内）。
func dialTarget(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	return addr
}
