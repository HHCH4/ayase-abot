package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type barrierEventTransport struct {
	calls atomic.Int32
}

func (t *barrierEventTransport) Deliver(context.Context, RuntimeEventOutbox, AgentEvent) error {
	if t.calls.Add(1) == 1 {
		return errors.New("temporary event delivery failure")
	}
	return nil
}

func (t *barrierEventTransport) RuntimeEventDeliveryRoute() (string, string) {
	return "runtime-a", "runtime-b"
}

type barrierCheckpointTransport struct {
	calls atomic.Int32
}

func (t *barrierCheckpointTransport) Deliver(_ context.Context, envelope RuntimeCheckpointDeliveryEnvelope, _ RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
	t.calls.Add(1)
	return RuntimeCheckpointDeliveryReceipt{
		Version: envelope.Version, DeliveryID: envelope.DeliveryID, InvocationID: envelope.InvocationID,
		SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence, SnapshotDigest: envelope.SnapshotDigest,
	}, nil
}

func TestCoordinatorCheckpointWaitsForEventHighWaterBeforeSending(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 15, 0, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-delivery-barrier", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, AgentEvent{ID: "barrier-before-checkpoint", InvocationID: invocation.ID, Type: EventRuntimeNotice, Timestamp: now}); err != nil {
		t.Fatal(err)
	}
	_, _, checkpoint, err := repo.CommitRuntimeSnapshotWithCheckpoint(
		ctx,
		checkpointDeliveryTestSnapshot(t, invocation.ID, now),
		AgentEvent{ID: "barrier-checkpoint-event", InvocationID: invocation.ID, Type: EventRuntimeSnapshot, Timestamp: now},
		"runtime-a", "runtime-b",
	)
	if err != nil {
		t.Fatal(err)
	}

	events := &barrierEventTransport{}
	checkpoints := &barrierCheckpointTransport{}
	coordinator := &Coordinator{
		repo: repo, eventOutboxWorkerID: "barrier-event-worker", checkpointOutboxWorkerID: "barrier-checkpoint-worker",
		checkpointDeliverySource: "runtime-a", checkpointDeliveryDestination: "runtime-b", checkpointDeliveryTransport: checkpoints,
	}
	coordinator.SetRuntimeEventDeliveryTransport(events)

	// The first event delivery fails; the scheduler may still drain the next
	// event cursor, but the checkpoint must be released without spending its
	// attempt budget while the event high-water mark is pending.
	coordinator.DispatchDueRuntimeDeliveries(ctx, now)
	if got := events.calls.Load(); got != 2 {
		t.Fatalf("首轮应尝试两个 event cursor（首个失败、下一个成功），实际 %d", got)
	}
	if got := checkpoints.calls.Load(); got != 0 {
		t.Fatalf("event high-water 未完成时不应发送 checkpoint，实际 %d", got)
	}
	deferred, err := repo.GetRuntimeCheckpointDeliveryOutbox(ctx, checkpoint.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deferred.Status != RuntimeCheckpointDeliveryOutboxQueued || deferred.Attempt != 0 || !deferred.AvailableAt.After(now) {
		t.Fatalf("checkpoint 屏障释放应保留 attempt=0 并退避: %#v", deferred)
	}

	// Once the event retry is due, the scheduler drains both event cursors and
	// only then sends the checkpoint carrying their high-water mark.
	dispatchAt := now.Add(time.Second)
	coordinator.DispatchDueRuntimeDeliveries(ctx, dispatchAt)
	if got := events.calls.Load(); got != 3 { // first failure + two successful event cursors
		t.Fatalf("第二轮应完成两个 event cursor，累计 %d", got)
	}
	if got := checkpoints.calls.Load(); got != 1 {
		t.Fatalf("事件高水位完成后应发送一次 checkpoint，实际 %d", got)
	}
	completed, err := repo.GetRuntimeCheckpointDeliveryOutbox(ctx, checkpoint.ID)
	if err != nil || completed.Status != RuntimeCheckpointDeliveryOutboxCompleted {
		t.Fatalf("checkpoint 屏障完成后状态错误: %#v err=%v", completed, err)
	}
}

func TestMemoryRuntimeEventOutboxReadyThroughTreatsFailedCursorAsBlocking(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 16, 0, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-delivery-barrier-failed", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	event, err := repo.AppendEvent(ctx, AgentEvent{ID: "barrier-failed-event", InvocationID: invocation.ID, Type: EventRuntimeNotice, Timestamp: now})
	if err != nil {
		t.Fatal(err)
	}
	outboxRepo := RuntimeEventOutboxRepository(repo)
	claimed, ok, err := outboxRepo.ClaimRuntimeEventOutbox(ctx, "barrier-failed-worker", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim event 失败: %#v ok=%v err=%v", claimed, ok, err)
	}
	for attempt := 0; attempt < MaxRuntimeEventOutboxAttempts; attempt++ {
		if _, err := outboxRepo.RetryRuntimeEventOutbox(ctx, claimed.ID, claimed.LeaseOwner, claimed.LeaseExpiresAt.Add(-time.Nanosecond), "failed"); err != nil {
			t.Fatal(err)
		}
		item, getErr := outboxRepo.GetRuntimeEventOutbox(ctx, claimed.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if item.Status == RuntimeEventOutboxFailed {
			break
		}
		claimed, ok, err = outboxRepo.ClaimRuntimeEventOutbox(ctx, "barrier-failed-worker", item.AvailableAt.Add(time.Nanosecond), time.Minute)
		if err != nil || !ok {
			t.Fatalf("失败重试后的 event cursor 未能再次 claim: %#v ok=%v err=%v", claimed, ok, err)
		}
	}
	ready, err := repo.RuntimeEventOutboxReadyThrough(ctx, invocation.ID, event.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("failed event cursor 必须阻断 checkpoint high-water")
	}
}
