// Package main 启动 Agent Runtime 平台的后端服务：
//   - :9090 gRPC 服务
//   - :8080 HTTP（grpc-gateway，供前端以 REST/JSON 调用）
//
// 数据存储按数据形态拆分到三种数据库（详见 scripts/README.md）：
//   - PostgreSQL：teams / agents（关系型配置数据）
//   - MongoDB：chat_messages（聊天记录，文档型）
//   - Redis：Agent 运行态热缓存（KV）
package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	gwruntime "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"

	"github.com/hwanyan/agenteam/internal/cache"
	"github.com/hwanyan/agenteam/internal/config"
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

	st := store.New(pgStore, msgStore)

	deps := &service.Deps{
		Store:   st,
		Runtime: agentruntime.New(agentCache),
		LLM:     llm.NewFromEnv(),
	}

	teamSrv := service.NewTeamServer(deps)
	agentSrv := service.NewAgentServer(deps)
	workspaceSrv := service.NewWorkspaceServer(deps)

	grpcServer := grpc.NewServer()
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
	// 直接以进程内调用的方式挂载 handler，无需再拨号连接上面的 gRPC 端口。
	if err := agenteamv1.RegisterTeamServiceHandlerServer(ctx, mux, teamSrv); err != nil {
		log.Fatalf("register team gateway: %v", err)
	}
	if err := agenteamv1.RegisterAgentServiceHandlerServer(ctx, mux, agentSrv); err != nil {
		log.Fatalf("register agent gateway: %v", err)
	}
	if err := agenteamv1.RegisterWorkspaceServiceHandlerServer(ctx, mux, workspaceSrv); err != nil {
		log.Fatalf("register workspace gateway: %v", err)
	}

	log.Printf("HTTP gateway listening on %s", cfg.HTTPAddr)
	if err := http.ListenAndServe(cfg.HTTPAddr, withCORS(mux)); err != nil {
		log.Fatalf("serve http gateway: %v", err)
	}
}

// withCORS 允许本地前端开发服务器跨域调用后端 REST 接口。
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
