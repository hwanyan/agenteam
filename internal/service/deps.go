// Package service 实现各个 gRPC 服务，串联 store / runtime / llm 三层依赖。
package service

import (
	"github.com/hwanyan/agenteam/internal/a2a"
	"github.com/hwanyan/agenteam/internal/llm"
	"github.com/hwanyan/agenteam/internal/runtime"
	"github.com/hwanyan/agenteam/internal/store"
)

// Deps 是所有 gRPC 服务实现共享的依赖集合。
type Deps struct {
	Store   store.Store
	Runtime *runtime.Manager
	LLM     llm.Client
	A2A     *a2a.Client

	// profiles 记录用户名镜像的刷新节流状态（零值可用），见 authz.go 的 currentUser。
	profiles profileSync
}
