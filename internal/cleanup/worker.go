// Package cleanup 提供“团队软删除”之后的后台清理 worker。
//
// DeleteTeam 只在 PostgreSQL 里软删除团队与 Agent（teams.deleted_at），随即返回成功；
// 跨库的收尾工作由本包异步完成：
//  1. 卸载团队下全部 Agent 的运行态缓存（Redis）；
//  2. 软删除该团队的聊天记录（MongoDB）；
//  3. 把团队标记为清理完成（teams.cleaned_at）。
//
// 待清理队列就是 PG 中 deleted_at IS NOT NULL AND cleaned_at IS NULL 的团队：只有三步全部
// 成功才会标记完成，任何一步失败都会留在队列里，下一轮自动重试，因此不需要消息队列。
// 三步均幂等，多实例同时运行同一轮清理只会重复做无副作用的工作，无需分布式锁。
package cleanup

import (
	"context"
	"log"
	"time"

	"github.com/hwanyan/agenteam/internal/store"
)

const (
	// DefaultInterval 是两轮清理之间的默认间隔。
	DefaultInterval = 30 * time.Second
	// DefaultBatchSize 是每轮最多处理的团队数。
	DefaultBatchSize = 50
)

// Store 是清理 worker 依赖的存储能力（store.Store 已实现）。
type Store interface {
	ListDeletedTeamsPendingCleanup(ctx context.Context, limit int) ([]*store.DeletedTeam, error)
	CleanupDeletedTeam(ctx context.Context, id string) error
}

// Unloader 卸载 Agent 的运行态缓存（runtime.Manager 已实现）。
type Unloader interface {
	Unload(ctx context.Context, agentID string) error
}

// Worker 周期性清理已软删除的团队。
type Worker struct {
	store     Store
	unloader  Unloader
	interval  time.Duration
	batchSize int
}

// New 创建清理 worker；interval / batchSize 小于等于 0 时使用默认值。
func New(st Store, unloader Unloader, interval time.Duration, batchSize int) *Worker {
	if interval <= 0 {
		interval = DefaultInterval
	}
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}
	return &Worker{store: st, unloader: unloader, interval: interval, batchSize: batchSize}
}

// Run 阻塞运行：立即清理一轮，之后按 interval 周期执行，直到 ctx 被取消。
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if n, err := w.RunOnce(ctx); err != nil {
			log.Printf("cleanup: list pending teams: %v", err)
		} else if n > 0 {
			log.Printf("cleanup: cleaned %d deleted team(s)", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RunOnce 执行一轮清理，返回本轮成功清理的团队数。
// 单个团队清理失败只记录日志并跳过（留待下一轮重试），不影响同批次其他团队；
// 只有拉取待清理列表失败才返回 error。
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	teams, err := w.store.ListDeletedTeamsPendingCleanup(ctx, w.batchSize)
	if err != nil {
		return 0, err
	}
	cleaned := 0
	for _, t := range teams {
		if err := w.cleanTeam(ctx, t); err != nil {
			log.Printf("cleanup: team %s will be retried: %v", t.ID, err)
			continue
		}
		cleaned++
	}
	return cleaned, nil
}

func (w *Worker) cleanTeam(ctx context.Context, t *store.DeletedTeam) error {
	for _, agentID := range t.AgentIDs {
		if err := w.unloader.Unload(ctx, agentID); err != nil {
			return err
		}
	}
	return w.store.CleanupDeletedTeam(ctx, t.ID)
}
