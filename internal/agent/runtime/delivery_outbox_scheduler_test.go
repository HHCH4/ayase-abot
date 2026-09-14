package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCoordinatorDispatchesUnifiedDeliveryOutboxesFairlyAndTakesOverExpiredLeases(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Date(2026, 9, 13, 21, 0, 0, 0, time.UTC)
	invocation := Invocation{ID: "inv-unified-delivery", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	const eventBacklog = 40
	for index := 0; index < eventBacklog; index++ {
		if _, err := repo.AppendEvent(ctx, AgentEvent{
			ID: "unified-event-" + string(rune('a'+index)), InvocationID: invocation.ID,
			Type: EventRuntimeNotice, Timestamp: now.Add(time.Duration(index) * time.Millisecond),
			Data: map[string]any{"index": index},
		}); err != nil {
			t.Fatalf("追加 event backlog 失败: %v", err)
		}
	}
	_, _, checkpointOutbox, err := repo.CommitRuntimeSnapshotWithCheckpoint(
		ctx,
		checkpointDeliveryTestSnapshot(t, invocation.ID, now),
		AgentEvent{ID: "unified-checkpoint-event", InvocationID: invocation.ID, Type: EventRuntimeSnapshot, Timestamp: now},
		"runtime-source", "runtime-destination",
	)
	if err != nil {
		t.Fatalf("登记 checkpoint outbox 失败: %v", err)
	}

	// Leave one cursor of each type in processing so the live scheduler must
	// take over an expired lease instead of only consuming fresh queued rows.
	eventRepo := RuntimeEventOutboxRepository(repo)
	claimedEvent, ok, err := eventRepo.ClaimRuntimeEventOutbox(ctx, "dead-event-worker", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("模拟 event 崩溃窗口失败: item=%#v ok=%v err=%v", claimedEvent, ok, err)
	}
	checkpointRepo := RuntimeCheckpointDeliveryOutboxRepository(repo)
	claimedCheckpoint, ok, err := checkpointRepo.ClaimRuntimeCheckpointDeliveryOutbox(ctx, "dead-checkpoint-worker", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("模拟 checkpoint 崩溃窗口失败: item=%#v ok=%v err=%v", claimedCheckpoint, ok, err)
	}

	var eventCalls atomic.Int32
	var checkpointCalls atomic.Int32
	coordinator := &Coordinator{
		repo:                          repo,
		eventOutboxWorkerID:           "live-event-worker",
		checkpointOutboxWorkerID:      "live-checkpoint-worker",
		checkpointDeliverySource:      "runtime-source",
		checkpointDeliveryDestination: "runtime-destination",
	}
	coordinator.eventOutboxHandler = func(_ context.Context, item RuntimeEventOutbox, event AgentEvent) error {
		if item.EventID != event.ID || event.InvocationID != invocation.ID {
			return errors.New("event metadata mismatch")
		}
		eventCalls.Add(1)
		return nil
	}
	coordinator.checkpointDeliveryTransport = checkpointDeliveryTransportFunc(func(_ context.Context, envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
		if envelope.DeliveryID != checkpointOutbox.DeliveryID || projection.InvocationID != invocation.ID {
			return RuntimeCheckpointDeliveryReceipt{}, errors.New("checkpoint metadata mismatch")
		}
		checkpointCalls.Add(1)
		return RuntimeCheckpointDeliveryReceipt{
			Version: envelope.Version, DeliveryID: envelope.DeliveryID, InvocationID: envelope.InvocationID,
			SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence, SnapshotDigest: envelope.SnapshotDigest,
		}, nil
	})

	// The source outboxes are both expired at this instant. One round must
	// claim each kind even though the event queue contains substantially more
	// work, proving that the shared dispatcher is not event-starved.
	dispatchAt := now.Add(time.Minute)
	coordinator.DispatchDueRuntimeDeliveries(ctx, dispatchAt)
	if got := checkpointCalls.Load(); got != 1 {
		t.Fatalf("统一调度首轮应接管并投递 checkpoint 一次，实际 %d", got)
	}
	if got := eventCalls.Load(); got != 32 {
		t.Fatalf("统一调度首轮应处理有界的 32 个 event cursor，实际 %d", got)
	}
	completedCheckpoint, err := checkpointRepo.GetRuntimeCheckpointDeliveryOutbox(ctx, checkpointOutbox.ID)
	if err != nil || completedCheckpoint.Status != RuntimeCheckpointDeliveryOutboxCompleted {
		t.Fatalf("checkpoint 过期 lease 未被统一调度接管完成: %#v err=%v", completedCheckpoint, err)
	}
	if completedCheckpoint.LeaseOwner != "" {
		t.Fatalf("completed checkpoint 不应保留旧 lease: %#v", completedCheckpoint)
	}

	// A second bounded pass drains the remaining event backlog without
	// redelivering the already completed checkpoint.
	coordinator.DispatchDueRuntimeDeliveries(ctx, dispatchAt.Add(time.Second))
	if got := eventCalls.Load(); got != eventBacklog+1 { // +1 is runtime.snapshot event
		t.Fatalf("第二轮应清空剩余 event backlog，实际累计 %d/%d", got, eventBacklog+1)
	}
	if got := checkpointCalls.Load(); got != 1 {
		t.Fatalf("已完成 checkpoint 不应被共享调度器重复投递: %d", got)
	}
}

func TestCoordinatorConcurrentUnifiedDeliveryDispatchHasSingleWinners(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Date(2026, 9, 13, 22, 0, 0, 0, time.UTC)
	invocation := Invocation{ID: "inv-unified-race", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	const eventCount = 12
	for index := 0; index < eventCount; index++ {
		if _, err := repo.AppendEvent(ctx, AgentEvent{
			ID: "unified-race-event-" + string(rune('a'+index)), InvocationID: invocation.ID,
			Type: EventRuntimeNotice, Timestamp: now.Add(time.Duration(index) * time.Millisecond),
		}); err != nil {
			t.Fatalf("追加 event 失败: %v", err)
		}
	}
	_, _, _, err := repo.CommitRuntimeSnapshotWithCheckpoint(
		ctx,
		checkpointDeliveryTestSnapshot(t, invocation.ID, now),
		AgentEvent{ID: "unified-race-checkpoint-event", InvocationID: invocation.ID, Type: EventRuntimeSnapshot, Timestamp: now},
		"runtime-source", "runtime-destination",
	)
	if err != nil {
		t.Fatal(err)
	}

	var eventCalls atomic.Int32
	var checkpointCalls atomic.Int32
	coordinator := &Coordinator{
		repo:                          repo,
		eventOutboxWorkerID:           "race-event-worker",
		checkpointOutboxWorkerID:      "race-checkpoint-worker",
		checkpointDeliverySource:      "runtime-source",
		checkpointDeliveryDestination: "runtime-destination",
	}
	coordinator.eventOutboxHandler = func(_ context.Context, _ RuntimeEventOutbox, _ AgentEvent) error {
		eventCalls.Add(1)
		return nil
	}
	coordinator.checkpointDeliveryTransport = checkpointDeliveryTransportFunc(func(_ context.Context, envelope RuntimeCheckpointDeliveryEnvelope, _ RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
		checkpointCalls.Add(1)
		return RuntimeCheckpointDeliveryReceipt{Version: envelope.Version, DeliveryID: envelope.DeliveryID, InvocationID: envelope.InvocationID, SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence, SnapshotDigest: envelope.SnapshotDigest}, nil
	})

	var wg sync.WaitGroup
	dispatchAt := now.Add(time.Second)
	for index := 0; index < 4; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			coordinator.DispatchDueRuntimeDeliveries(ctx, dispatchAt)
		}()
	}
	wg.Wait()
	if got := eventCalls.Load(); got != eventCount+1 { // +1 is runtime.snapshot event
		t.Fatalf("并发统一调度应让每个 event cursor 只有一个 winner，实际 %d/%d", got, eventCount+1)
	}
	if got := checkpointCalls.Load(); got != 1 {
		t.Fatalf("并发统一调度应让 checkpoint cursor 只有一个 winner，实际 %d", got)
	}
	eventRepo := RuntimeEventOutboxRepository(repo)
	events, err := eventRepo.ListRuntimeEventOutbox(ctx, invocation.ID, RuntimeEventOutboxCompleted, 100)
	if err != nil || len(events) != eventCount+1 {
		t.Fatalf("并发统一调度未完成全部 event cursor: len=%d err=%v", len(events), err)
	}
}
