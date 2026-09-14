package sqlite

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

func newSQLiteMultiRebindApplyFixture(t *testing.T) (context.Context, string, *Store, *agentruntime.Coordinator, agentruntime.RuntimeConfigDirectoryRebindPlan, time.Time) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	snapshotText, digest := sqliteRebindSnapshot(t)
	invocation := agentruntime.Invocation{ID: "inv-sqlite-rebind-multi-apply", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingApproval, ConfigSnapshot: snapshotText, ConfigSnapshotDigest: digest, CreatedAt: now, UpdatedAt: now}
	repo := store.RuntimeRepository()
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, agentruntime.AgentEvent{ID: "sqlite-rebind-multi-apply-event", InvocationID: invocation.ID, Type: agentruntime.EventInvocationWaiting, Timestamp: now}); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	snapshot := sqliteCheckpointSnapshot(invocation.ID, 1, now)
	if _, err := repo.(agentruntime.RuntimeSnapshotRepository).SaveRuntimeSnapshot(ctx, snapshot); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	coordinator := newSQLiteRebindCoordinator(t, store)
	catalog := agentruntime.NewMemoryRuntimeConfigDirectoryRebindRouteCatalog()
	for _, destination := range []string{"runtime-target-a", "runtime-target-b"} {
		if err := catalog.Set("runtime-source", sqliteRebindCapability(destination, 8)); err != nil {
			_ = store.Close()
			t.Fatal(err)
		}
	}
	coordinator.SetRuntimeConfigDirectoryRebindRouteCatalog(catalog)
	plan, err := coordinator.CreateRuntimeConfigDirectoryRebindPlan(ctx, "runtime-source", invocation.ID, []string{"runtime-target-b", "runtime-target-a"}, digest, "", "sqlite-multi-plan-key", now)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	return ctx, dataDir, store, coordinator, plan, now
}

func TestSQLiteRuntimeConfigDirectoryRebindMultiApplyPersistsAcrossRestartAndReconciles(t *testing.T) {
	ctx, dataDir, store, coordinator, plan, now := newSQLiteMultiRebindApplyFixture(t)
	confirmation, err := coordinator.ConfirmRuntimeConfigDirectoryRebindMulti(ctx, plan.ID, "sqlite-multi-confirm", now)
	if err != nil {
		_ = coordinator.Close()
		_ = store.Close()
		t.Fatal(err)
	}
	parent, err := coordinator.ApplyRuntimeConfigDirectoryRebindMulti(ctx, plan.ID, confirmation.ID, "sqlite-multi-apply", now)
	if err != nil {
		_ = coordinator.Close()
		_ = store.Close()
		t.Fatal(err)
	}
	if parent.Status != agentruntime.RuntimeConfigDirectoryRebindMultiApplyQueued || len(parent.ChildApplyIDs) != 2 {
		_ = coordinator.Close()
		_ = store.Close()
		t.Fatalf("SQLite multi parent 初始状态错误: %#v", parent)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	repo2 := store2.RuntimeRepository()
	parentRepo := repo2.(agentruntime.RuntimeConfigDirectoryRebindMultiApplyRepository)
	reloaded, err := parentRepo.GetRuntimeConfigDirectoryRebindMultiApply(ctx, parent.ID)
	if err != nil || reloaded.ID != parent.ID || reloaded.PendingCount != 2 {
		t.Fatalf("SQLite 重启后 parent 错误: %#v err=%v", reloaded, err)
	}
	childRepo := repo2.(agentruntime.RuntimeConfigDirectoryRebindApplyRepository)
	children, err := childRepo.ListRuntimeConfigDirectoryRebindApplies(ctx, plan.ID, plan.InvocationID, agentruntime.RuntimeConfigDirectoryRebindApplyQueued, 10)
	if err != nil || len(children) != 2 {
		t.Fatalf("SQLite 重启后 children 错误: %#v err=%v", children, err)
	}
	for index := 0; index < 2; index++ {
		child, claimed, claimErr := childRepo.ClaimRuntimeConfigDirectoryRebindApply(ctx, "sqlite-multi-worker", time.Now().UTC().Add(time.Second), time.Minute)
		if claimErr != nil || !claimed {
			t.Fatalf("SQLite child claim 失败: %#v claimed=%v err=%v", child, claimed, claimErr)
		}
		if _, completeErr := childRepo.CompleteRuntimeConfigDirectoryRebindApply(ctx, child.ID, "sqlite-multi-worker", time.Now().UTC().Add(2*time.Second)); completeErr != nil {
			t.Fatal(completeErr)
		}
	}
	completed, err := parentRepo.ReconcileRuntimeConfigDirectoryRebindMultiApply(ctx, parent.ID, time.Now().UTC().Add(3*time.Second))
	if err != nil || completed.Status != agentruntime.RuntimeConfigDirectoryRebindMultiApplyCompleted || completed.CompletedCount != 2 || completed.CompletedAt == nil {
		t.Fatalf("SQLite parent reconcile 未完成: %#v err=%v", completed, err)
	}
	confirmationAfter, err := repo2.(agentruntime.RuntimeConfigDirectoryRebindMultiConfirmationRepository).GetRuntimeConfigDirectoryRebindMultiConfirmation(ctx, confirmation.ID)
	if err != nil || confirmationAfter.Status != agentruntime.RuntimeConfigDirectoryRebindMultiConfirmationConsumed {
		t.Fatalf("SQLite confirmation 状态错误: %#v err=%v", confirmationAfter, err)
	}
}

func buildSQLiteMultiParentForTest(t *testing.T, repo agentruntime.Repository, plan agentruntime.RuntimeConfigDirectoryRebindPlan, confirmation agentruntime.RuntimeConfigDirectoryRebindMultiConfirmation, applyKey string, now time.Time) (agentruntime.RuntimeConfigDirectoryRebindMultiApply, []agentruntime.RuntimeConfigDirectoryRebindApply, error) {
	t.Helper()
	snapshot, err := repo.(agentruntime.RuntimeSnapshotRepository).GetRuntimeSnapshot(context.Background(), plan.InvocationID)
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindMultiApply{}, nil, err
	}
	events, err := repo.(agentruntime.RuntimeCheckpointDeliverySourceRepository).ListEvents(context.Background(), plan.InvocationID, 0, 5000)
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindMultiApply{}, nil, err
	}
	var sequence int64
	for _, event := range events {
		if event.Sequence > sequence {
			sequence = event.Sequence
		}
	}
	projection, err := agentruntime.NewRuntimeCheckpointProjection(snapshot, sequence)
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindMultiApply{}, nil, err
	}
	digest, err := agentruntime.RuntimeCheckpointProjectionDigest(projection)
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindMultiApply{}, nil, err
	}
	parentID := agentruntime.RuntimeConfigDirectoryRebindMultiApplyID(plan.ID, confirmation.ID, snapshot.Revision, sequence, digest, applyKey, plan.Destinations)
	return agentruntime.NewRuntimeConfigDirectoryRebindMultiApply(plan, confirmation, parentID, snapshot, sequence, applyKey, now)
}

func TestSQLiteRuntimeConfigDirectoryRebindMultiApplyAtomicRollback(t *testing.T) {
	ctx, _, store, coordinator, plan, now := newSQLiteMultiRebindApplyFixture(t)
	defer coordinator.Close()
	defer store.Close()
	confirmation, err := coordinator.ConfirmRuntimeConfigDirectoryRebindMulti(ctx, plan.ID, "sqlite-multi-rollback-confirm", now)
	if err != nil {
		t.Fatal(err)
	}
	parent, children, err := buildSQLiteMultiParentForTest(t, store.RuntimeRepository(), plan, confirmation, "sqlite-multi-rollback-apply", now)
	if err != nil {
		t.Fatal(err)
	}
	childRepo := store.RuntimeRepository().(agentruntime.RuntimeConfigDirectoryRebindApplyRepository)
	if _, err := childRepo.EnqueueRuntimeConfigDirectoryRebindApply(ctx, children[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RuntimeRepository().(agentruntime.RuntimeConfigDirectoryRebindMultiApplyCommitRepository).CommitRuntimeConfigDirectoryRebindMultiApply(ctx, confirmation.ID, parent, children, now); !errors.Is(err, agentruntime.ErrRuntimeConfigDirectoryRebindMultiApplyConflict) {
		t.Fatalf("已有 child 冲突必须整笔回滚: %v", err)
	}
	if _, err := store.RuntimeRepository().(agentruntime.RuntimeConfigDirectoryRebindMultiApplyRepository).GetRuntimeConfigDirectoryRebindMultiApply(ctx, parent.ID); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("冲突后 parent 不得写入: %v", err)
	}
	confirmationAfter, err := store.RuntimeRepository().(agentruntime.RuntimeConfigDirectoryRebindMultiConfirmationRepository).GetRuntimeConfigDirectoryRebindMultiConfirmation(ctx, confirmation.ID)
	if err != nil || confirmationAfter.Status != agentruntime.RuntimeConfigDirectoryRebindMultiConfirmationConfirmed {
		t.Fatalf("冲突后 confirmation 必须保持 confirmed: %#v err=%v", confirmationAfter, err)
	}
}

func TestSQLiteRuntimeConfigDirectoryRebindMultiApplyConcurrentIdempotency(t *testing.T) {
	ctx, _, store, coordinator, plan, now := newSQLiteMultiRebindApplyFixture(t)
	defer coordinator.Close()
	defer store.Close()
	confirmation, err := coordinator.ConfirmRuntimeConfigDirectoryRebindMulti(ctx, plan.ID, "sqlite-multi-concurrent-confirm", now)
	if err != nil {
		t.Fatal(err)
	}
	parent, children, err := buildSQLiteMultiParentForTest(t, store.RuntimeRepository(), plan, confirmation, "sqlite-multi-concurrent-apply", now)
	if err != nil {
		t.Fatal(err)
	}
	commitRepo := store.RuntimeRepository().(agentruntime.RuntimeConfigDirectoryRebindMultiApplyCommitRepository)
	var wait sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for index := 0; index < 8; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, commitErr := commitRepo.CommitRuntimeConfigDirectoryRebindMultiApply(ctx, confirmation.ID, parent, children, now); commitErr == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wait.Wait()
	if successes != 8 {
		t.Fatalf("SQLite 相同 parent 重试必须全部幂等成功: %d", successes)
	}
	parents, err := store.RuntimeRepository().(agentruntime.RuntimeConfigDirectoryRebindMultiApplyRepository).ListRuntimeConfigDirectoryRebindMultiApplies(ctx, plan.ID, plan.InvocationID, "", 10)
	if err != nil || len(parents) != 1 {
		t.Fatalf("SQLite 并发 parent 数量错误: %#v err=%v", parents, err)
	}
	childrenStored, err := store.RuntimeRepository().(agentruntime.RuntimeConfigDirectoryRebindApplyRepository).ListRuntimeConfigDirectoryRebindApplies(ctx, plan.ID, plan.InvocationID, "", 10)
	if err != nil || len(childrenStored) != 2 {
		t.Fatalf("SQLite 并发 child 数量错误: %#v err=%v", childrenStored, err)
	}
}
