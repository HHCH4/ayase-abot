package sqlite

import (
	"context"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

func TestSQLiteRuntimeCheckpointHighWaterBarrierAndDeferralAreDurable(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	now := time.Date(2026, 9, 13, 16, 30, 0, 0, time.UTC)
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-delivery-barrier-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	eventRepo, ok := repo.(agentruntime.RuntimeEventOutboxRepository)
	if !ok {
		_ = store.Close()
		t.Fatal("SQLite Runtime Repository 未实现 RuntimeEventOutboxRepository")
	}
	reader, ok := repo.(agentruntime.RuntimeEventOutboxConsistencyReader)
	if !ok {
		_ = store.Close()
		t.Fatal("SQLite Runtime Repository 未实现 RuntimeEventOutboxConsistencyReader")
	}
	firstEvent, err := repo.AppendEvent(ctx, agentruntime.AgentEvent{ID: "sqlite-barrier-first-event", InvocationID: invocation.ID, Type: agentruntime.EventRuntimeNotice, Timestamp: now})
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	ready, err := reader.RuntimeEventOutboxReadyThrough(ctx, invocation.ID, firstEvent.Sequence)
	if err != nil || ready {
		t.Fatalf("queued event 应阻断 SQLite high-water: ready=%v err=%v", ready, err)
	}
	first, ok, err := eventRepo.ClaimRuntimeEventOutbox(ctx, "sqlite-barrier-event-worker", now, time.Minute)
	if err != nil || !ok {
		_ = store.Close()
		t.Fatalf("首次 event claim 失败: %#v ok=%v err=%v", first, ok, err)
	}
	if done, err := eventRepo.CompleteRuntimeEventOutbox(ctx, first.ID, first.LeaseOwner, first.LeaseExpiresAt.Add(-time.Nanosecond)); err != nil || !done {
		_ = store.Close()
		t.Fatalf("完成首个 event cursor 失败: done=%v err=%v", done, err)
	}
	ready, err = reader.RuntimeEventOutboxReadyThrough(ctx, invocation.ID, firstEvent.Sequence)
	if err != nil || !ready {
		t.Fatalf("completed event 应打开 SQLite high-water: ready=%v err=%v", ready, err)
	}

	commitRepo, ok := repo.(agentruntime.RuntimeSnapshotCheckpointCommitRepository)
	if !ok {
		_ = store.Close()
		t.Fatal("SQLite Runtime Repository 未实现 RuntimeSnapshotCheckpointCommitRepository")
	}
	_, checkpointEvent, checkpoint, err := commitRepo.CommitRuntimeSnapshotWithCheckpoint(
		ctx,
		sqliteCheckpointSnapshot(invocation.ID, 1, now),
		agentruntime.AgentEvent{ID: "sqlite-barrier-checkpoint-event", InvocationID: invocation.ID, Type: agentruntime.EventRuntimeSnapshot, Timestamp: now},
		"runtime-a", "runtime-b",
	)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	ready, err = reader.RuntimeEventOutboxReadyThrough(ctx, invocation.ID, checkpoint.EventSequence)
	if err != nil || ready {
		t.Fatalf("checkpoint event queued 时 high-water 必须保持关闭: ready=%v err=%v", ready, err)
	}
	checkpointRepo, ok := repo.(agentruntime.RuntimeCheckpointDeliveryOutboxRepository)
	if !ok {
		_ = store.Close()
		t.Fatal("SQLite Runtime Repository 未实现 RuntimeCheckpointDeliveryOutboxRepository")
	}
	deferrer, ok := repo.(agentruntime.RuntimeCheckpointDeliveryOutboxDeferrer)
	if !ok {
		_ = store.Close()
		t.Fatal("SQLite Runtime Repository 未实现 RuntimeCheckpointDeliveryOutboxDeferrer")
	}
	claimedCheckpoint, ok, err := checkpointRepo.ClaimRuntimeCheckpointDeliveryOutbox(ctx, "sqlite-barrier-checkpoint-worker", now, time.Minute)
	if err != nil || !ok {
		_ = store.Close()
		t.Fatalf("checkpoint claim 失败: %#v ok=%v err=%v", claimedCheckpoint, ok, err)
	}
	deferAt := now.Add(time.Second)
	deferred, err := deferrer.DeferRuntimeCheckpointDeliveryOutbox(ctx, claimedCheckpoint.ID, claimedCheckpoint.LeaseOwner, now, deferAt, "checkpoint 等待 event high-water 完成")
	if err != nil || !deferred {
		_ = store.Close()
		t.Fatalf("SQLite checkpoint 屏障释放失败: deferred=%v err=%v", deferred, err)
	}
	queued, err := checkpointRepo.GetRuntimeCheckpointDeliveryOutbox(ctx, checkpoint.ID)
	if err != nil || queued.Status != agentruntime.RuntimeCheckpointDeliveryOutboxQueued || queued.Attempt != 0 || !queued.AvailableAt.Equal(deferAt) {
		_ = store.Close()
		t.Fatalf("SQLite checkpoint 屏障释放应保留 attempt=0: %#v err=%v", queued, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	checkpointRepo = store.RuntimeRepository().(agentruntime.RuntimeCheckpointDeliveryOutboxRepository)
	queued, err = checkpointRepo.GetRuntimeCheckpointDeliveryOutbox(ctx, checkpoint.ID)
	if err != nil || queued.Status != agentruntime.RuntimeCheckpointDeliveryOutboxQueued || queued.Attempt != 0 || !queued.AvailableAt.Equal(deferAt) {
		t.Fatalf("重启后 SQLite 屏障释放状态未持久化: %#v err=%v", queued, err)
	}
	_ = checkpointEvent
}
