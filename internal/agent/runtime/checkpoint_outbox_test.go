package runtime

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemoryRuntimeCheckpointDeliveryOutboxIsDurableLeasedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Date(2026, 9, 13, 18, 0, 0, 0, time.UTC)
	invocation := Invocation{ID: "inv-checkpoint-outbox", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	snapshot := checkpointDeliveryTestSnapshot(t, invocation.ID, now)
	checkpoint, event, outbox, err := repo.CommitRuntimeSnapshotWithCheckpoint(ctx, snapshot, AgentEvent{ID: "checkpoint-snapshot-event", InvocationID: invocation.ID, Type: EventRuntimeSnapshot, Timestamp: now}, "runtime-a", "runtime-b")
	if err != nil {
		t.Fatalf("snapshot/checkpoint 原子提交失败: %v", err)
	}
	if checkpoint.Revision != 1 || event.Sequence != 1 || outbox.ID == "" || outbox.Status != RuntimeCheckpointDeliveryOutboxQueued {
		t.Fatalf("原子提交结果错误: snapshot=%#v event=%#v outbox=%#v", checkpoint, event, outbox)
	}
	stored, err := repo.GetRuntimeCheckpointDeliveryOutbox(ctx, outbox.ID)
	if err != nil || !stored.Matches(outbox) {
		t.Fatalf("checkpoint outbox 读取错误: %#v err=%v", stored, err)
	}
	stored.Projection.Workflow.Boundaries[0].PendingWaitIDs[0] = "mutated"
	again, err := repo.GetRuntimeCheckpointDeliveryOutbox(ctx, outbox.ID)
	if err != nil || again.Projection.Workflow.Boundaries[0].PendingWaitIDs[0] == "mutated" {
		t.Fatalf("outbox projection 必须防御性复制: %#v err=%v", again, err)
	}
	if _, err := repo.EnqueueRuntimeCheckpointDeliveryOutbox(ctx, outbox); err != nil {
		t.Fatalf("相同 checkpoint outbox 重复 enqueue 应幂等: %v", err)
	}
	forged := outbox
	forged.ID = "forged-checkpoint-outbox"
	if _, err := repo.EnqueueRuntimeCheckpointDeliveryOutbox(ctx, forged); !errors.Is(err, ErrConflict) {
		t.Fatalf("伪造 checkpoint outbox ID 必须拒绝: %v", err)
	}
	forgedSequence, err := NewRuntimeCheckpointDeliveryOutbox(outbox.Source, outbox.Destination, checkpoint, outbox.EventSequence+100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeCheckpointDeliveryOutbox(ctx, forgedSequence); !errors.Is(err, ErrConflict) {
		t.Fatalf("不存在的 event high-water mark 必须拒绝: %v", err)
	}

	claimed, ok, err := repo.ClaimRuntimeCheckpointDeliveryOutbox(ctx, "checkpoint-worker-a", now, time.Minute)
	if err != nil || !ok || claimed.Attempt != 1 || claimed.Status != RuntimeCheckpointDeliveryOutboxProcessing {
		t.Fatalf("claim checkpoint outbox 错误: %#v ok=%v err=%v", claimed, ok, err)
	}
	if done, err := repo.CompleteRuntimeCheckpointDeliveryOutbox(ctx, claimed.ID, "checkpoint-worker-b", now); err != nil || done {
		t.Fatalf("错误 worker 不能 complete: done=%v err=%v", done, err)
	}
	if retried, err := repo.RetryRuntimeCheckpointDeliveryOutbox(ctx, claimed.ID, claimed.LeaseOwner, now, "api_key=secret-value"); err != nil || !retried {
		t.Fatalf("有效 worker retry 失败: retried=%v err=%v", retried, err)
	}
	queued, err := repo.GetRuntimeCheckpointDeliveryOutbox(ctx, claimed.ID)
	if err != nil || queued.Status != RuntimeCheckpointDeliveryOutboxQueued || !queued.AvailableAt.After(now) || strings.Contains(queued.LastError, "secret-value") || !strings.Contains(queued.LastError, "[REDACTED]") {
		t.Fatalf("retry 后 checkpoint outbox 边界错误: %#v err=%v", queued, err)
	}
	claimed, ok, err = repo.ClaimRuntimeCheckpointDeliveryOutbox(ctx, "checkpoint-worker-b", queued.AvailableAt.Add(time.Nanosecond), time.Minute)
	if err != nil || !ok {
		t.Fatalf("checkpoint outbox 应可在退避后接管: %#v ok=%v err=%v", claimed, ok, err)
	}
	if done, err := repo.CompleteRuntimeCheckpointDeliveryOutbox(ctx, claimed.ID, claimed.LeaseOwner, claimed.LeaseExpiresAt.Add(-time.Nanosecond)); err != nil || !done {
		t.Fatalf("当前 lease owner 应可 complete: done=%v err=%v", done, err)
	}
	completed, err := repo.GetRuntimeCheckpointDeliveryOutbox(ctx, claimed.ID)
	if err != nil || completed.Status != RuntimeCheckpointDeliveryOutboxCompleted || completed.LeaseOwner != "" {
		t.Fatalf("checkpoint outbox completed 状态错误: %#v err=%v", completed, err)
	}
}

func TestCoordinatorRebuildRuntimeSnapshotEnqueuesCheckpointOutbox(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Date(2026, 9, 13, 18, 30, 0, 0, time.UTC)
	invocation := Invocation{ID: "inv-checkpoint-rebuild", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	coordinator.SetRuntimeCheckpointDeliveryTransport("runtime-a", "runtime-b", checkpointDeliveryTransportFunc(func(context.Context, RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
		return RuntimeCheckpointDeliveryReceipt{}, nil
	}))
	if _, err := coordinator.RebuildRuntimeSnapshot(ctx, invocation.ID); err != nil {
		t.Fatalf("Coordinator rebuild 应原子登记 checkpoint outbox: %v", err)
	}
	outboxRepo := any(repo).(RuntimeCheckpointDeliveryOutboxRepository)
	items, err := outboxRepo.ListRuntimeCheckpointDeliveryOutbox(ctx, invocation.ID, RuntimeCheckpointDeliveryOutboxQueued, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("Coordinator rebuild 未登记 checkpoint outbox: %#v err=%v", items, err)
	}
	events, err := repo.ListEvents(ctx, invocation.ID, 0, 10)
	if err != nil || len(events) != 1 || events[0].Type != EventRuntimeSnapshot || events[0].Sequence != items[0].EventSequence {
		t.Fatalf("checkpoint outbox 必须绑定同一 runtime.snapshot high-water: events=%#v outbox=%#v err=%v", events, items, err)
	}
}

type checkpointDeliveryTransportFunc func(context.Context, RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error)

func (f checkpointDeliveryTransportFunc) Deliver(ctx context.Context, envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
	return f(ctx, envelope, projection)
}

func TestCoordinatorDispatchesRuntimeCheckpointOutboxWithAtLeastOnceBoundary(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Date(2026, 9, 13, 19, 0, 0, 0, time.UTC)
	invocation := Invocation{ID: "inv-checkpoint-dispatch", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	snapshot := checkpointDeliveryTestSnapshot(t, invocation.ID, now)
	_, _, outbox, err := repo.CommitRuntimeSnapshotWithCheckpoint(ctx, snapshot, AgentEvent{ID: "checkpoint-dispatch-event", InvocationID: invocation.ID, Type: EventRuntimeSnapshot, Timestamp: now}, "runtime-a", "runtime-b")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	coordinator := &Coordinator{repo: repo, checkpointOutboxWorkerID: "checkpoint-dispatch-worker", checkpointDeliverySource: "runtime-a", checkpointDeliveryDestination: "runtime-b"}
	coordinator.checkpointDeliveryTransport = checkpointDeliveryTransportFunc(func(_ context.Context, envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
		calls.Add(1)
		if envelope.DeliveryID != outbox.DeliveryID || envelope.SnapshotRevision != outbox.SnapshotRevision || envelope.EventSequence != outbox.EventSequence {
			t.Fatalf("transport envelope identity 错误: %#v", envelope)
		}
		if projection.InvocationID != invocation.ID || projection.SnapshotRevision != outbox.SnapshotRevision || projection.EventSequence != outbox.EventSequence {
			t.Fatalf("transport projection identity 错误: %#v", projection)
		}
		return RuntimeCheckpointDeliveryReceipt{Version: envelope.Version, DeliveryID: envelope.DeliveryID, InvocationID: envelope.InvocationID, SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence, SnapshotDigest: envelope.SnapshotDigest}, nil
	})
	coordinator.dispatchDueRuntimeCheckpointOutbox(ctx, now)
	if calls.Load() != 1 {
		t.Fatalf("checkpoint transport 调用次数=%d", calls.Load())
	}
	completed, err := repo.GetRuntimeCheckpointDeliveryOutbox(ctx, outbox.ID)
	if err != nil || completed.Status != RuntimeCheckpointDeliveryOutboxCompleted {
		t.Fatalf("checkpoint dispatch 未完成 outbox: %#v err=%v", completed, err)
	}
	coordinator.dispatchDueRuntimeCheckpointOutbox(ctx, now.Add(time.Minute))
	if calls.Load() != 1 {
		t.Fatalf("已完成 checkpoint 不应重复发送: calls=%d", calls.Load())
	}
}

func TestCoordinatorCheckpointDispatchRetriesAndRedactsErrors(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Date(2026, 9, 13, 20, 0, 0, 0, time.UTC)
	invocation := Invocation{ID: "inv-checkpoint-retry", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	snapshot := checkpointDeliveryTestSnapshot(t, invocation.ID, now)
	_, _, outbox, err := repo.CommitRuntimeSnapshotWithCheckpoint(ctx, snapshot, AgentEvent{ID: "checkpoint-retry-event", InvocationID: invocation.ID, Type: EventRuntimeSnapshot, Timestamp: now}, "runtime-a", "runtime-b")
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, checkpointOutboxWorkerID: "checkpoint-retry-worker", checkpointDeliverySource: "runtime-a", checkpointDeliveryDestination: "runtime-b"}
	coordinator.checkpointDeliveryTransport = checkpointDeliveryTransportFunc(func(context.Context, RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
		return RuntimeCheckpointDeliveryReceipt{}, errors.New("api_key=secret-value")
	})
	coordinator.dispatchDueRuntimeCheckpointOutbox(ctx, now)
	queued, err := repo.GetRuntimeCheckpointDeliveryOutbox(ctx, outbox.ID)
	if err != nil || queued.Status != RuntimeCheckpointDeliveryOutboxQueued || strings.Contains(queued.LastError, "secret-value") || !strings.Contains(queued.LastError, "[REDACTED]") {
		t.Fatalf("失败 checkpoint retry 应有界脱敏: %#v err=%v", queued, err)
	}
}

func TestCoordinatorCheckpointDispatchRejectsMismatchedReceipt(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Date(2026, 9, 13, 20, 30, 0, 0, time.UTC)
	invocation := Invocation{ID: "inv-checkpoint-receipt", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	_, _, outbox, err := repo.CommitRuntimeSnapshotWithCheckpoint(ctx, checkpointDeliveryTestSnapshot(t, invocation.ID, now), AgentEvent{ID: "checkpoint-receipt-event", InvocationID: invocation.ID, Type: EventRuntimeSnapshot, Timestamp: now}, "runtime-a", "runtime-b")
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, checkpointOutboxWorkerID: "checkpoint-receipt-worker", checkpointDeliverySource: "runtime-a", checkpointDeliveryDestination: "runtime-b"}
	coordinator.checkpointDeliveryTransport = checkpointDeliveryTransportFunc(func(context.Context, RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
		return RuntimeCheckpointDeliveryReceipt{Version: RuntimeCheckpointDeliveryEnvelopeVersion, DeliveryID: "wrong"}, nil
	})
	coordinator.dispatchDueRuntimeCheckpointOutbox(ctx, now)
	queued, err := repo.GetRuntimeCheckpointDeliveryOutbox(ctx, outbox.ID)
	if err != nil || queued.Status != RuntimeCheckpointDeliveryOutboxQueued || !strings.Contains(queued.LastError, "receipt metadata") {
		t.Fatalf("错误 receipt 不得释放 checkpoint lease: %#v err=%v", queued, err)
	}
}
