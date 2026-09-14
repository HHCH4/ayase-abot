package sqlite

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

func TestSQLiteRuntimeCheckpointDeliveryOutboxIsAtomicDurableAndLeased(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	now := time.Date(2026, 9, 13, 21, 0, 0, 0, time.UTC)
	invocation := agentruntime.Invocation{ID: "inv-checkpoint-outbox-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	snapshot := sqliteCheckpointSnapshot(invocation.ID, 1, now)
	commitRepo, ok := repo.(agentruntime.RuntimeSnapshotCheckpointCommitRepository)
	if !ok {
		_ = store.Close()
		t.Fatal("SQLite Runtime Repository 未实现 RuntimeSnapshotCheckpointCommitRepository")
	}
	saved, event, outbox, err := commitRepo.CommitRuntimeSnapshotWithCheckpoint(ctx, snapshot, agentruntime.AgentEvent{ID: "checkpoint-outbox-snapshot-event", InvocationID: invocation.ID, Type: agentruntime.EventRuntimeSnapshot, Timestamp: now}, "runtime-a", "runtime-b")
	if err != nil {
		_ = store.Close()
		t.Fatalf("Snapshot/checkpoint SQLite 原子提交失败: %v", err)
	}
	if saved.Revision != 1 || event.Sequence != 1 || outbox.ID == "" || outbox.Status != agentruntime.RuntimeCheckpointDeliveryOutboxQueued {
		_ = store.Close()
		t.Fatalf("SQLite 原子提交结果错误: snapshot=%#v event=%#v outbox=%#v", saved, event, outbox)
	}
	outboxRepo, ok := repo.(agentruntime.RuntimeCheckpointDeliveryOutboxRepository)
	if !ok {
		_ = store.Close()
		t.Fatal("SQLite Runtime Repository 未实现 RuntimeCheckpointDeliveryOutboxRepository")
	}
	got, err := outboxRepo.GetRuntimeCheckpointDeliveryOutbox(ctx, outbox.ID)
	if err != nil || !got.Matches(outbox) {
		_ = store.Close()
		t.Fatalf("SQLite checkpoint outbox 读取错误: %#v err=%v", got, err)
	}
	if _, err := outboxRepo.EnqueueRuntimeCheckpointDeliveryOutbox(ctx, outbox); err != nil {
		_ = store.Close()
		t.Fatalf("SQLite 相同 checkpoint outbox 重复 enqueue 应幂等: %v", err)
	}
	if _, err := outboxRepo.EnqueueRuntimeCheckpointDeliveryOutbox(ctx, func() agentruntime.RuntimeCheckpointDeliveryOutbox {
		forged := outbox
		forged.ID = "forged-checkpoint-outbox"
		return forged
	}()); !errors.Is(err, agentruntime.ErrConflict) {
		_ = store.Close()
		t.Fatalf("SQLite 伪造 checkpoint outbox ID 必须拒绝: %v", err)
	}

	claimed, ok, err := outboxRepo.ClaimRuntimeCheckpointDeliveryOutbox(ctx, "sqlite-checkpoint-worker-a", now, time.Minute)
	if err != nil || !ok || claimed.Attempt != 1 || claimed.Status != agentruntime.RuntimeCheckpointDeliveryOutboxProcessing {
		_ = store.Close()
		t.Fatalf("SQLite claim checkpoint outbox 错误: %#v ok=%v err=%v", claimed, ok, err)
	}
	if done, err := outboxRepo.CompleteRuntimeCheckpointDeliveryOutbox(ctx, claimed.ID, "wrong-worker", now); err != nil || done {
		_ = store.Close()
		t.Fatalf("SQLite 错误 worker 不能 complete: done=%v err=%v", done, err)
	}
	if retried, err := outboxRepo.RetryRuntimeCheckpointDeliveryOutbox(ctx, claimed.ID, claimed.LeaseOwner, now, "api_key=secret-value"); err != nil || !retried {
		_ = store.Close()
		t.Fatalf("SQLite checkpoint retry 失败: retried=%v err=%v", retried, err)
	}
	queued, err := outboxRepo.GetRuntimeCheckpointDeliveryOutbox(ctx, claimed.ID)
	if err != nil || queued.Status != agentruntime.RuntimeCheckpointDeliveryOutboxQueued || !queued.AvailableAt.After(now) || strings.Contains(queued.LastError, "secret-value") || !strings.Contains(queued.LastError, "[REDACTED]") {
		_ = store.Close()
		t.Fatalf("SQLite checkpoint retry 边界错误: %#v err=%v", queued, err)
	}
	claimed, ok, err = outboxRepo.ClaimRuntimeCheckpointDeliveryOutbox(ctx, "sqlite-checkpoint-worker-b", queued.AvailableAt.Add(time.Nanosecond), time.Minute)
	if err != nil || !ok {
		_ = store.Close()
		t.Fatalf("SQLite checkpoint outbox 应可退避后接管: %#v ok=%v err=%v", claimed, ok, err)
	}
	if done, err := outboxRepo.CompleteRuntimeCheckpointDeliveryOutbox(ctx, claimed.ID, claimed.LeaseOwner, claimed.LeaseExpiresAt.Add(-time.Nanosecond)); err != nil || !done {
		_ = store.Close()
		t.Fatalf("SQLite 当前 lease owner 应可 complete: done=%v err=%v", done, err)
	}
	completed, err := outboxRepo.GetRuntimeCheckpointDeliveryOutbox(ctx, claimed.ID)
	if err != nil || completed.Status != agentruntime.RuntimeCheckpointDeliveryOutboxCompleted || completed.LeaseOwner != "" {
		_ = store.Close()
		t.Fatalf("SQLite completed checkpoint outbox 状态错误: %#v err=%v", completed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	outboxRepo = store.RuntimeRepository().(agentruntime.RuntimeCheckpointDeliveryOutboxRepository)
	items, err := outboxRepo.ListRuntimeCheckpointDeliveryOutbox(ctx, invocation.ID, agentruntime.RuntimeCheckpointDeliveryOutboxCompleted, 10)
	if err != nil || len(items) != 1 || items[0].ID != outbox.ID {
		t.Fatalf("SQLite 重启后 checkpoint outbox 不可读: %#v err=%v", items, err)
	}
}

func TestSQLiteRuntimeCheckpointDeliveryOutboxRollbackOnInvalidRoute(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	now := time.Date(2026, 9, 13, 22, 0, 0, 0, time.UTC)
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-checkpoint-outbox-rollback", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	commitRepo := repo.(agentruntime.RuntimeSnapshotCheckpointCommitRepository)
	_, _, _, err = commitRepo.CommitRuntimeSnapshotWithCheckpoint(ctx, sqliteCheckpointSnapshot(invocation.ID, 1, now), agentruntime.AgentEvent{ID: "checkpoint-rollback-event", InvocationID: invocation.ID, Type: agentruntime.EventRuntimeSnapshot, Timestamp: now}, "", "runtime-b")
	if !errors.Is(err, agentruntime.ErrInvalidRuntimeCheckpointDelivery) {
		t.Fatalf("空 source 必须拒绝并回滚: %v", err)
	}
	snapshotRepo := repo.(agentruntime.RuntimeSnapshotRepository)
	if _, err := snapshotRepo.GetRuntimeSnapshot(ctx, invocation.ID); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("checkpoint route 校验失败后不应留下 Snapshot: %v", err)
	}
	events, err := repo.ListEvents(ctx, invocation.ID, 0, 20)
	if err != nil || len(events) != 0 {
		t.Fatalf("checkpoint route 校验失败后不应留下事件: %#v err=%v", events, err)
	}
}

func TestSQLiteRuntimeCheckpointDeliveryOutboxClaimHasSingleWinner(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	now := time.Date(2026, 9, 13, 23, 0, 0, 0, time.UTC)
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-checkpoint-outbox-race", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = repo.(agentruntime.RuntimeSnapshotCheckpointCommitRepository).CommitRuntimeSnapshotWithCheckpoint(ctx, sqliteCheckpointSnapshot(invocation.ID, 1, now), agentruntime.AgentEvent{ID: "checkpoint-race-event", InvocationID: invocation.ID, Type: agentruntime.EventRuntimeSnapshot, Timestamp: now}, "runtime-a", "runtime-b")
	if err != nil {
		t.Fatal(err)
	}
	outboxRepo := repo.(agentruntime.RuntimeCheckpointDeliveryOutboxRepository)
	type result struct {
		claimed bool
		err     error
	}
	results := make(chan result, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			claimed, ok, claimErr := outboxRepo.ClaimRuntimeCheckpointDeliveryOutbox(ctx, "race-worker-"+string(rune('a'+index)), now, time.Minute)
			if claimErr == nil && ok {
				_ = claimed
			}
			results <- result{claimed: ok, err: claimErr}
		}(i)
	}
	wg.Wait()
	close(results)
	winners := 0
	for item := range results {
		if item.err != nil && !strings.Contains(item.err.Error(), "database is locked") {
			t.Fatalf("SQLite 并发 claim 失败: %v", item.err)
		}
		if item.claimed {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("SQLite checkpoint outbox 必须只有一个 claim winner: %d", winners)
	}
}
