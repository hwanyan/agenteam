package cleanup

import (
	"context"
	"errors"
	"testing"

	"github.com/hwanyan/agenteam/internal/store"
)

type fakeStore struct {
	pending    []*store.DeletedTeam
	cleaned    []string
	cleanupErr map[string]error
}

func (f *fakeStore) ListDeletedTeamsPendingCleanup(_ context.Context, limit int) ([]*store.DeletedTeam, error) {
	return f.pending, nil
}

func (f *fakeStore) CleanupDeletedTeam(_ context.Context, id string) error {
	if err := f.cleanupErr[id]; err != nil {
		return err
	}
	f.cleaned = append(f.cleaned, id)
	return nil
}

type fakeUnloader struct {
	unloaded  []string
	failAgent string
}

func (f *fakeUnloader) Unload(_ context.Context, agentID string) error {
	if agentID == f.failAgent {
		return errors.New("redis down")
	}
	f.unloaded = append(f.unloaded, agentID)
	return nil
}

func TestRunOnce_CleansAllTeams(t *testing.T) {
	st := &fakeStore{pending: []*store.DeletedTeam{
		{ID: "t1", AgentIDs: []string{"a1", "a2"}},
		{ID: "t2", AgentIDs: []string{"a3"}},
	}}
	un := &fakeUnloader{}

	n, err := New(st, un, 0, 0).RunOnce(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("RunOnce = %d, %v; want 2, nil", n, err)
	}
	if len(un.unloaded) != 3 || len(st.cleaned) != 2 {
		t.Fatalf("unloaded=%v cleaned=%v", un.unloaded, st.cleaned)
	}
}

func TestRunOnce_FailedTeamIsSkippedAndNotMarked(t *testing.T) {
	st := &fakeStore{
		pending: []*store.DeletedTeam{
			{ID: "t1", AgentIDs: []string{"a1"}}, // Unload 失败
			{ID: "t2", AgentIDs: []string{"a2"}}, // Mongo 清理失败
			{ID: "t3", AgentIDs: []string{"a3"}}, // 正常
		},
		cleanupErr: map[string]error{"t2": errors.New("mongo down")},
	}
	un := &fakeUnloader{failAgent: "a1"}

	n, err := New(st, un, 0, 0).RunOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("RunOnce = %d, %v; want 1, nil", n, err)
	}
	if len(st.cleaned) != 1 || st.cleaned[0] != "t3" {
		t.Fatalf("cleaned = %v; want [t3]", st.cleaned)
	}
}
