package sqlite

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

func sqliteSagaTestGroup(t *testing.T, invocationID string, status agentruntime.RuntimeDeliveryGroupStatus, now time.Time) agentruntime.RuntimeDeliveryGroup {
	t.Helper()
	group, err := agentruntime.NewRuntimeDeliveryGroup("runtime-a", "runtime-b", invocationID, []agentruntime.RuntimeDeliveryGroupMember{
		{Kind: agentruntime.RuntimeDeliveryKindEvent, OutboxID: "event-saga-" + invocationID, DeliveryID: "event-saga-" + invocationID, Status: agentruntime.RuntimeDeliveryGroupMemberCompleted},
		{Kind: agentruntime.RuntimeDeliveryKindConfig, OutboxID: "config-saga-" + invocationID, DeliveryID: "config-saga-" + invocationID, Status: agentruntime.RuntimeDeliveryGroupMemberCompleted},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	group.Status = status
	group.UpdatedAt = now
	if status == agentruntime.RuntimeDeliveryGroupFailed {
		group.Members[1].Status = agentruntime.RuntimeDeliveryGroupMemberFailed
		group.Members[1].LastError = "destination rejected"
		group.CompletedAt = nil
		group.LastError = "destination rejected"
	} else {
		group.CompletedAt = &now
	}
	return group
}

func TestSQLiteRuntimeDeliveryGroupSagaIsDurableAndCASProtected(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	invocationID := "inv-saga-sqlite"
	if err := repo.CreateInvocation(ctx, agentruntime.Invocation{ID: invocationID, UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationCompleted, CreatedAt: now, UpdatedAt: now}); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	group := sqliteSagaTestGroup(t, invocationID, agentruntime.RuntimeDeliveryGroupCompleted, now)
	saga, err := agentruntime.NewRuntimeDeliveryGroupSaga(group, now)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	sagaRepo := repo.(agentruntime.RuntimeDeliveryGroupSagaRepository)
	stored, err := sagaRepo.EnqueueRuntimeDeliveryGroupSaga(ctx, saga)
	if err != nil || !stored.MatchesIdentity(saga) {
		_ = store.Close()
		t.Fatalf("SQLite saga enqueue 失败: %#v err=%v", stored, err)
	}
	duplicate, err := sagaRepo.EnqueueRuntimeDeliveryGroupSaga(ctx, saga)
	if err != nil || !duplicate.MatchesIdentity(saga) {
		_ = store.Close()
		t.Fatalf("SQLite saga 重复 enqueue 应幂等: %#v err=%v", duplicate, err)
	}
	for _, step := range []struct {
		state  agentruntime.RuntimeDeliveryGroupSagaState
		remote agentruntime.RuntimeDeliveryGroupSettlementRemotePhase
	}{
		{agentruntime.RuntimeDeliveryGroupSagaPreparing, ""},
		{agentruntime.RuntimeDeliveryGroupSagaPrepared, agentruntime.RuntimeDeliveryGroupSettlementRemotePrepared},
		{agentruntime.RuntimeDeliveryGroupSagaCommitting, ""},
		{agentruntime.RuntimeDeliveryGroupSagaCommitted, agentruntime.RuntimeDeliveryGroupSettlementRemoteCommitted},
	} {
		updated, duplicate, advanceErr := sagaRepo.AdvanceRuntimeDeliveryGroupSaga(ctx, saga.ID, saga.Revision, step.state, step.remote, "", now.Add(time.Duration(saga.Revision)*time.Second))
		if advanceErr != nil || duplicate {
			t.Fatalf("SQLite saga 推进 %s 失败: %#v duplicate=%v err=%v", step.state, updated, duplicate, advanceErr)
		}
		saga = updated
	}
	if saga.State != agentruntime.RuntimeDeliveryGroupSagaCommitted || saga.CompletedAt == nil || saga.Revision != 5 {
		t.Fatalf("SQLite saga terminal 状态错误: %#v", saga)
	}
	if _, _, err := sagaRepo.AdvanceRuntimeDeliveryGroupSaga(ctx, saga.ID, saga.Revision-1, agentruntime.RuntimeDeliveryGroupSagaCommitted, agentruntime.RuntimeDeliveryGroupSettlementRemoteCommitted, "", now); !errors.Is(err, agentruntime.ErrConflict) {
		t.Fatalf("SQLite saga 旧 revision 必须拒绝: %v", err)
	}
	listed, err := sagaRepo.ListRuntimeDeliveryGroupSagas(ctx, invocationID, agentruntime.RuntimeDeliveryGroupSagaCommitted, 10)
	if err != nil || len(listed) != 1 || listed[0].ID != saga.ID {
		t.Fatalf("SQLite saga list 错误: %#v err=%v", listed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	reopened := store.RuntimeRepository().(agentruntime.RuntimeDeliveryGroupSagaRepository)
	loaded, err := reopened.GetRuntimeDeliveryGroupSaga(ctx, saga.ID)
	if err != nil || loaded.State != agentruntime.RuntimeDeliveryGroupSagaCommitted || loaded.RemotePhase != agentruntime.RuntimeDeliveryGroupSettlementRemoteCommitted || loaded.CompletedAt == nil || loaded.Revision != saga.Revision {
		t.Fatalf("SQLite saga close/reopen 后状态丢失: %#v err=%v", loaded, err)
	}
	if _, err := reopened.ListRuntimeDeliveryGroupSagas(ctx, "missing-invocation", "", 10); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("未知 invocation 应拒绝 saga list: %v", err)
	}
}

func TestSQLiteRuntimeDeliveryGroupSagaConcurrentCASHasOneWinner(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo := store.RuntimeRepository()
	invocationID := "inv-saga-sqlite-concurrent"
	if err := repo.CreateInvocation(ctx, agentruntime.Invocation{ID: invocationID, UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationCompleted, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	group := sqliteSagaTestGroup(t, invocationID, agentruntime.RuntimeDeliveryGroupCompleted, now)
	saga, err := agentruntime.NewRuntimeDeliveryGroupSaga(group, now)
	if err != nil {
		t.Fatal(err)
	}
	sagaRepo := repo.(agentruntime.RuntimeDeliveryGroupSagaRepository)
	if _, err := sagaRepo.EnqueueRuntimeDeliveryGroupSaga(ctx, saga); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, state := range []agentruntime.RuntimeDeliveryGroupSagaState{agentruntime.RuntimeDeliveryGroupSagaPreparing, agentruntime.RuntimeDeliveryGroupSagaPrepared} {
		wg.Add(1)
		go func(state agentruntime.RuntimeDeliveryGroupSagaState) {
			defer wg.Done()
			_, _, advanceErr := sagaRepo.AdvanceRuntimeDeliveryGroupSaga(ctx, saga.ID, saga.Revision, state, "", "", now.Add(time.Second))
			results <- advanceErr
		}(state)
	}
	wg.Wait()
	close(results)
	var successes, conflicts int
	for advanceErr := range results {
		switch {
		case advanceErr == nil:
			successes++
		case errors.Is(advanceErr, agentruntime.ErrConflict):
			conflicts++
		default:
			t.Fatalf("SQLite 并发 saga CAS 返回意外错误: %v", advanceErr)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("SQLite 同一 saga revision 只能一个推进成功: successes=%d conflicts=%d", successes, conflicts)
	}
}
