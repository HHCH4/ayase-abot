package runtime

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These transports deliberately fail Commit after a valid Prepare.  They
// model the narrow failure window where the source outbox is about to become
// terminal and prove that the optional compensation hook is attempted once,
// with the exact immutable envelope that was prepared.

type eventFinalAttemptAbortTransport struct {
	prepareCalls atomic.Int32
	commitCalls  atomic.Int32
	abortCalls   atomic.Int32
	preparedID   atomic.Value
}

func (t *eventFinalAttemptAbortTransport) Deliver(context.Context, RuntimeEventOutbox, AgentEvent) error {
	return errors.New("one-phase delivery should not be called")
}

func (t *eventFinalAttemptAbortTransport) RuntimeEventDeliveryRoute() (string, string) {
	return "runtime-a", "runtime-b"
}

func (t *eventFinalAttemptAbortTransport) BuildEnvelope(outbox RuntimeEventOutbox, event AgentEvent) (RuntimeEventDeliveryEnvelope, error) {
	return NewRuntimeEventDeliveryEnvelope("runtime-a", "runtime-b", outbox, event)
}

func (t *eventFinalAttemptAbortTransport) Prepare(_ context.Context, envelope RuntimeEventDeliveryEnvelope) (RuntimeEventDeliveryReceipt, error) {
	t.prepareCalls.Add(1)
	t.preparedID.Store(envelope.DeliveryID)
	return RuntimeEventDeliveryReceipt{
		Version: envelope.Version, DeliveryID: envelope.DeliveryID, EventID: envelope.EventID,
		InvocationID: envelope.InvocationID, Sequence: envelope.Sequence, Type: envelope.Type,
		EventDigest: envelope.EventDigest, Phase: RuntimeEventDeliveryTransactionPhasePrepared,
	}, nil
}

func (t *eventFinalAttemptAbortTransport) Commit(_ context.Context, envelope RuntimeEventDeliveryEnvelope) (RuntimeEventDeliveryReceipt, error) {
	t.commitCalls.Add(1)
	return RuntimeEventDeliveryReceipt{
		Version: envelope.Version, DeliveryID: envelope.DeliveryID, EventID: envelope.EventID,
		InvocationID: envelope.InvocationID, Sequence: envelope.Sequence, Type: envelope.Type,
		EventDigest: envelope.EventDigest, Phase: RuntimeEventDeliveryTransactionPhaseCommitted,
	}, errors.New("remote commit failed after prepare")
}

func (t *eventFinalAttemptAbortTransport) Abort(_ context.Context, envelope RuntimeEventDeliveryEnvelope) (RuntimeEventDeliveryReceipt, error) {
	t.abortCalls.Add(1)
	if got, _ := t.preparedID.Load().(string); got != envelope.DeliveryID {
		return RuntimeEventDeliveryReceipt{}, errors.New("abort envelope does not match prepared envelope")
	}
	return RuntimeEventDeliveryReceipt{
		Version: envelope.Version, DeliveryID: envelope.DeliveryID, EventID: envelope.EventID,
		InvocationID: envelope.InvocationID, Sequence: envelope.Sequence, Type: envelope.Type,
		EventDigest: envelope.EventDigest, Phase: RuntimeEventDeliveryTransactionPhaseAborted,
	}, nil
}

func TestCoordinatorCompensatesEventOnFinalTransactionalFailure(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	invocation := eventOutboxInvocation(t, repo, "inv-event-compensation")
	event, err := repo.AppendEvent(ctx, AgentEvent{ID: "event-compensation", InvocationID: invocation.ID, Type: EventRuntimeNotice, Timestamp: now})
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := repo.GetRuntimeEventOutbox(ctx, eventOutboxIDForTest(event.ID))
	if err != nil {
		t.Fatal(err)
	}
	setRuntimeEventOutboxFinalAttempt(repo, outbox.ID, now)
	transport := &eventFinalAttemptAbortTransport{}
	coordinator := &Coordinator{repo: repo, eventOutboxWorkerID: "event-compensation-worker", deliveryCompensationWorkerID: "event-compensation-durable-worker"}
	coordinator.SetRuntimeEventDeliveryTransport(transport)
	coordinator.dispatchDueRuntimeEventOutbox(ctx, now)

	stored, err := repo.GetRuntimeEventOutbox(ctx, outbox.ID)
	if err != nil || stored.Status != RuntimeEventOutboxFailed {
		t.Fatalf("最终 commit 失败后 source outbox 必须 failed: %#v err=%v", stored, err)
	}
	if transport.prepareCalls.Load() != 1 || transport.commitCalls.Load() != 1 || transport.abortCalls.Load() != 1 {
		t.Fatalf("最终失败应恰好执行 prepare/commit/abort 一次: %d/%d/%d", transport.prepareCalls.Load(), transport.commitCalls.Load(), transport.abortCalls.Load())
	}
	compensations, err := repo.ListRuntimeDeliveryCompensations(ctx, invocation.ID, RuntimeDeliveryKindEvent, "", 10)
	if err != nil || len(compensations) != 1 || compensations[0].Status != RuntimeDeliveryCompensationQueued || compensations[0].DeliveryID != outbox.ID {
		t.Fatalf("最终失败必须登记 durable event compensation: %#v err=%v", compensations, err)
	}
	coordinator.dispatchDueRuntimeCompensationsBounded(ctx, now, 1)
	compensation, err := repo.GetRuntimeDeliveryCompensation(ctx, compensations[0].ID)
	if err != nil || compensation.Status != RuntimeDeliveryCompensationCompleted || transport.abortCalls.Load() != 2 {
		t.Fatalf("durable event compensation 应幂等完成并再执行一次 Abort: %#v aborts=%d err=%v", compensation, transport.abortCalls.Load(), err)
	}
	journal, err := repo.GetRuntimeDeliveryAttempt(ctx, RuntimeDeliveryAttemptID(RuntimeDeliveryKindEvent, "runtime-a", "runtime-b", outbox.ID))
	if err != nil || journal.Phase != RuntimeDeliveryAttemptFailed {
		t.Fatalf("source phase journal 必须保留 failed: %#v err=%v", journal, err)
	}
}

type checkpointFinalAttemptAbortTransport struct {
	prepareCalls atomic.Int32
	commitCalls  atomic.Int32
	abortCalls   atomic.Int32
	preparedID   atomic.Value
}

func (t *checkpointFinalAttemptAbortTransport) Deliver(context.Context, RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
	return RuntimeCheckpointDeliveryReceipt{}, errors.New("one-phase delivery should not be called")
}

func (t *checkpointFinalAttemptAbortTransport) Prepare(_ context.Context, envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
	t.prepareCalls.Add(1)
	t.preparedID.Store(envelope.DeliveryID)
	if projection.InvocationID != envelope.InvocationID {
		return RuntimeCheckpointDeliveryReceipt{}, errors.New("projection identity mismatch")
	}
	return RuntimeCheckpointDeliveryReceipt{
		Version: envelope.Version, DeliveryID: envelope.DeliveryID, InvocationID: envelope.InvocationID,
		SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence,
		SnapshotDigest: envelope.SnapshotDigest, Phase: RuntimeCheckpointDeliveryTransactionPhasePrepared,
	}, nil
}

func (t *checkpointFinalAttemptAbortTransport) Commit(_ context.Context, envelope RuntimeCheckpointDeliveryEnvelope, _ RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
	t.commitCalls.Add(1)
	return RuntimeCheckpointDeliveryReceipt{
		Version: envelope.Version, DeliveryID: envelope.DeliveryID, InvocationID: envelope.InvocationID,
		SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence,
		SnapshotDigest: envelope.SnapshotDigest, Phase: RuntimeCheckpointDeliveryTransactionPhaseCommitted,
	}, errors.New("remote commit failed after prepare")
}

func (t *checkpointFinalAttemptAbortTransport) Abort(_ context.Context, envelope RuntimeCheckpointDeliveryEnvelope, _ RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
	t.abortCalls.Add(1)
	if got, _ := t.preparedID.Load().(string); got != envelope.DeliveryID {
		return RuntimeCheckpointDeliveryReceipt{}, errors.New("abort envelope does not match prepared envelope")
	}
	return RuntimeCheckpointDeliveryReceipt{
		Version: envelope.Version, DeliveryID: envelope.DeliveryID, InvocationID: envelope.InvocationID,
		SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence,
		SnapshotDigest: envelope.SnapshotDigest, Phase: RuntimeCheckpointDeliveryTransactionPhaseAborted,
	}, nil
}

func TestCoordinatorCompensatesCheckpointOnFinalTransactionalFailure(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 10, 1, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-checkpoint-compensation", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	_, _, outbox, err := repo.CommitRuntimeSnapshotWithCheckpoint(ctx, checkpointDeliveryTestSnapshot(t, invocation.ID, now), AgentEvent{ID: "checkpoint-compensation-event", InvocationID: invocation.ID, Type: EventRuntimeSnapshot, Timestamp: now}, "runtime-a", "runtime-b")
	if err != nil {
		t.Fatal(err)
	}
	setRuntimeCheckpointOutboxFinalAttempt(repo, outbox.ID, now)
	transport := &checkpointFinalAttemptAbortTransport{}
	coordinator := &Coordinator{repo: repo, checkpointOutboxWorkerID: "checkpoint-compensation-worker", checkpointDeliverySource: "runtime-a", checkpointDeliveryDestination: "runtime-b", checkpointDeliveryTransport: transport, deliveryCompensationWorkerID: "checkpoint-compensation-durable-worker"}
	coordinator.dispatchDueRuntimeCheckpointOutbox(ctx, now)

	stored, err := repo.GetRuntimeCheckpointDeliveryOutbox(ctx, outbox.ID)
	if err != nil || stored.Status != RuntimeCheckpointDeliveryOutboxFailed {
		t.Fatalf("最终 commit 失败后 checkpoint outbox 必须 failed: %#v err=%v", stored, err)
	}
	if transport.prepareCalls.Load() != 1 || transport.commitCalls.Load() != 1 || transport.abortCalls.Load() != 1 {
		t.Fatalf("checkpoint 最终失败应恰好执行 prepare/commit/abort 一次: %d/%d/%d", transport.prepareCalls.Load(), transport.commitCalls.Load(), transport.abortCalls.Load())
	}
	compensations, err := repo.ListRuntimeDeliveryCompensations(ctx, invocation.ID, RuntimeDeliveryKindCheckpoint, "", 10)
	if err != nil || len(compensations) != 1 || compensations[0].Status != RuntimeDeliveryCompensationQueued || compensations[0].DeliveryID != outbox.DeliveryID {
		t.Fatalf("最终失败必须登记 durable checkpoint compensation: %#v err=%v", compensations, err)
	}
	coordinator.dispatchDueRuntimeCompensationsBounded(ctx, now, 1)
	compensation, err := repo.GetRuntimeDeliveryCompensation(ctx, compensations[0].ID)
	if err != nil || compensation.Status != RuntimeDeliveryCompensationCompleted || transport.abortCalls.Load() != 2 {
		t.Fatalf("durable checkpoint compensation 应幂等完成并再执行一次 Abort: %#v aborts=%d err=%v", compensation, transport.abortCalls.Load(), err)
	}
	journal, err := repo.GetRuntimeDeliveryAttempt(ctx, RuntimeDeliveryAttemptID(RuntimeDeliveryKindCheckpoint, "runtime-a", "runtime-b", outbox.DeliveryID))
	if err != nil || journal.Phase != RuntimeDeliveryAttemptFailed {
		t.Fatalf("checkpoint phase journal 必须保留 failed: %#v err=%v", journal, err)
	}
}

type configFinalAttemptAbortTransport struct {
	prepareCalls atomic.Int32
	commitCalls  atomic.Int32
	abortCalls   atomic.Int32
	preparedID   atomic.Value
}

func (t *configFinalAttemptAbortTransport) Deliver(context.Context, RuntimeConfigDeliveryEnvelope, RuntimeConfigDeliveryProjection) (RuntimeConfigDeliveryReceipt, error) {
	return RuntimeConfigDeliveryReceipt{}, errors.New("one-phase delivery should not be called")
}

func (t *configFinalAttemptAbortTransport) Prepare(_ context.Context, envelope RuntimeConfigDeliveryEnvelope, _ RuntimeConfigDeliveryProjection) (RuntimeConfigDeliveryReceipt, error) {
	t.prepareCalls.Add(1)
	t.preparedID.Store(envelope.DeliveryID)
	return RuntimeConfigDeliveryReceipt{
		Version: envelope.Version, DeliveryID: envelope.DeliveryID, InvocationID: envelope.InvocationID,
		ExpectedDigest: envelope.ExpectedDigest, SnapshotDigest: envelope.SnapshotDigest,
		SnapshotVersion: envelope.SnapshotVersion, Phase: RuntimeConfigDeliveryTransactionPhasePrepared,
	}, nil
}

func (t *configFinalAttemptAbortTransport) Commit(_ context.Context, envelope RuntimeConfigDeliveryEnvelope, _ RuntimeConfigDeliveryProjection) (RuntimeConfigDeliveryReceipt, error) {
	t.commitCalls.Add(1)
	return RuntimeConfigDeliveryReceipt{
		Version: envelope.Version, DeliveryID: envelope.DeliveryID, InvocationID: envelope.InvocationID,
		ExpectedDigest: envelope.ExpectedDigest, SnapshotDigest: envelope.SnapshotDigest,
		SnapshotVersion: envelope.SnapshotVersion, Phase: RuntimeConfigDeliveryTransactionPhaseCommitted,
	}, errors.New("remote commit failed after prepare")
}

func (t *configFinalAttemptAbortTransport) Abort(_ context.Context, envelope RuntimeConfigDeliveryEnvelope, _ RuntimeConfigDeliveryProjection) (RuntimeConfigDeliveryReceipt, error) {
	t.abortCalls.Add(1)
	if got, _ := t.preparedID.Load().(string); got != envelope.DeliveryID {
		return RuntimeConfigDeliveryReceipt{}, errors.New("abort envelope does not match prepared envelope")
	}
	return RuntimeConfigDeliveryReceipt{
		Version: envelope.Version, DeliveryID: envelope.DeliveryID, InvocationID: envelope.InvocationID,
		ExpectedDigest: envelope.ExpectedDigest, SnapshotDigest: envelope.SnapshotDigest,
		SnapshotVersion: envelope.SnapshotVersion, Phase: RuntimeConfigDeliveryTransactionPhaseAborted,
	}, nil
}

func TestCoordinatorCompensatesConfigOnFinalTransactionalFailure(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 10, 2, 0, 0, time.UTC)
	old := migrationSnapshot(t, "compensation-old", "demo", "model")
	target := migrationSnapshot(t, "compensation-target", "demo", "model")
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-config-compensation", UserID: "user", ConversationID: "conversation", SessionID: "conversation", ConfigSnapshot: old, Status: InvocationWaitingUser, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	oldDigest := runtimeSnapshotDigestForTest(old)
	targetDigest := runtimeSnapshotDigestForTest(target)
	_, _, outbox, err := repo.CommitRuntimeConfigMigrationWithDelivery(ctx, RuntimeConfigMigrationCommit{
		InvocationID: invocation.ID, ExpectedDigest: oldDigest, NewSnapshot: target, IdempotencyKey: "compensation-1",
		Event: AgentEvent{ID: RuntimeConfigMigrationEventID(invocation.ID, oldDigest, targetDigest, "compensation-1"), Type: EventRuntimeConfigMigrated, Timestamp: now},
	}, "runtime-a", "runtime-b")
	if err != nil {
		t.Fatal(err)
	}
	setRuntimeConfigOutboxFinalAttempt(repo, outbox.ID, now)
	transport := &configFinalAttemptAbortTransport{}
	coordinator := &Coordinator{repo: repo, configOutboxWorkerID: "config-compensation-worker", configDeliverySource: "runtime-a", configDeliveryDestination: "runtime-b", configDeliveryTransport: transport, deliveryCompensationWorkerID: "config-compensation-durable-worker"}
	coordinator.dispatchDueRuntimeConfigOutbox(ctx, now)

	stored, err := repo.GetRuntimeConfigDeliveryOutbox(ctx, outbox.ID)
	if err != nil || stored.Status != RuntimeConfigDeliveryOutboxFailed {
		t.Fatalf("最终 commit 失败后 config outbox 必须 failed: %#v err=%v", stored, err)
	}
	if transport.prepareCalls.Load() != 1 || transport.commitCalls.Load() != 1 || transport.abortCalls.Load() != 1 {
		t.Fatalf("config 最终失败应恰好执行 prepare/commit/abort 一次: %d/%d/%d", transport.prepareCalls.Load(), transport.commitCalls.Load(), transport.abortCalls.Load())
	}
	compensations, err := repo.ListRuntimeDeliveryCompensations(ctx, invocation.ID, RuntimeDeliveryKindConfig, "", 10)
	if err != nil || len(compensations) != 1 || compensations[0].Status != RuntimeDeliveryCompensationQueued || compensations[0].DeliveryID != outbox.DeliveryID {
		t.Fatalf("最终失败必须登记 durable config compensation: %#v err=%v", compensations, err)
	}
	coordinator.dispatchDueRuntimeCompensationsBounded(ctx, now, 1)
	compensation, err := repo.GetRuntimeDeliveryCompensation(ctx, compensations[0].ID)
	if err != nil || compensation.Status != RuntimeDeliveryCompensationCompleted || transport.abortCalls.Load() != 2 {
		t.Fatalf("durable config compensation 应幂等完成并再执行一次 Abort: %#v aborts=%d err=%v", compensation, transport.abortCalls.Load(), err)
	}
	journal, err := repo.GetRuntimeDeliveryAttempt(ctx, RuntimeDeliveryAttemptID(RuntimeDeliveryKindConfig, "runtime-a", "runtime-b", outbox.DeliveryID))
	if err != nil || journal.Phase != RuntimeDeliveryAttemptFailed {
		t.Fatalf("config phase journal 必须保留 failed: %#v err=%v", journal, err)
	}
}

type rejectionFinalAttemptAbortTransport struct {
	prepareCalls atomic.Int32
	commitCalls  atomic.Int32
	abortCalls   atomic.Int32
	preparedID   atomic.Value
}

func (t *rejectionFinalAttemptAbortTransport) Deliver(context.Context, RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeApprovalRejectionDeliveryReceipt, error) {
	return RuntimeApprovalRejectionDeliveryReceipt{}, errors.New("one-phase delivery should not be called")
}

func (t *rejectionFinalAttemptAbortTransport) Prepare(_ context.Context, envelope RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeApprovalRejectionDeliveryReceipt, error) {
	t.prepareCalls.Add(1)
	t.preparedID.Store(envelope.DeliveryID)
	return RuntimeApprovalRejectionDeliveryReceipt{
		Version: envelope.Version, DeliveryID: envelope.DeliveryID, ApprovalID: envelope.ApprovalID,
		InvocationID: envelope.InvocationID, ToolCallID: envelope.ToolCallID, OperationID: envelope.OperationID,
		Decision: envelope.Decision, ReasonDigest: envelope.ReasonDigest, Phase: RuntimeApprovalRejectionDeliveryTransactionPhasePrepared,
	}, nil
}

func (t *rejectionFinalAttemptAbortTransport) Commit(_ context.Context, envelope RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeApprovalRejectionDeliveryReceipt, error) {
	t.commitCalls.Add(1)
	return RuntimeApprovalRejectionDeliveryReceipt{
		Version: envelope.Version, DeliveryID: envelope.DeliveryID, ApprovalID: envelope.ApprovalID,
		InvocationID: envelope.InvocationID, ToolCallID: envelope.ToolCallID, OperationID: envelope.OperationID,
		Decision: envelope.Decision, ReasonDigest: envelope.ReasonDigest, Phase: RuntimeApprovalRejectionDeliveryTransactionPhaseCommitted,
	}, errors.New("remote commit failed after prepare")
}

func (t *rejectionFinalAttemptAbortTransport) Abort(_ context.Context, envelope RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeApprovalRejectionDeliveryReceipt, error) {
	t.abortCalls.Add(1)
	if got, _ := t.preparedID.Load().(string); got != envelope.DeliveryID {
		return RuntimeApprovalRejectionDeliveryReceipt{}, errors.New("abort envelope does not match prepared envelope")
	}
	return RuntimeApprovalRejectionDeliveryReceipt{
		Version: envelope.Version, DeliveryID: envelope.DeliveryID, ApprovalID: envelope.ApprovalID,
		InvocationID: envelope.InvocationID, ToolCallID: envelope.ToolCallID, OperationID: envelope.OperationID,
		Decision: envelope.Decision, ReasonDigest: envelope.ReasonDigest, Phase: RuntimeApprovalRejectionDeliveryTransactionPhaseAborted,
	}, nil
}

func TestCoordinatorCompensatesApprovalRejectionOnFinalTransactionalFailure(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 10, 3, 0, 0, time.UTC)
	repo, approval, invocation := rejectionDeliveryFixture(t, ApprovalRejected)
	outbox, err := NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", approval, invocation, "compensation rejection", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeApprovalRejectionDeliveryOutbox(ctx, outbox); err != nil {
		t.Fatal(err)
	}
	setRuntimeApprovalRejectionOutboxFinalAttempt(repo, outbox.ID, now)
	transport := &rejectionFinalAttemptAbortTransport{}
	coordinator := &Coordinator{repo: repo, rejectionOutboxWorkerID: "rejection-compensation-worker", rejectionDeliverySource: "runtime-a", rejectionDeliveryDestination: "runtime-b", rejectionDeliveryTransport: transport, deliveryCompensationWorkerID: "rejection-compensation-durable-worker"}
	coordinator.dispatchDueRuntimeApprovalRejectionOutbox(ctx, now)

	stored, err := repo.GetRuntimeApprovalRejectionDeliveryOutbox(ctx, outbox.ID)
	if err != nil || stored.Status != RuntimeApprovalRejectionDeliveryOutboxFailed {
		t.Fatalf("最终 commit 失败后 rejection outbox 必须 failed: %#v err=%v", stored, err)
	}
	if transport.prepareCalls.Load() != 1 || transport.commitCalls.Load() != 1 || transport.abortCalls.Load() != 1 {
		t.Fatalf("rejection 最终失败应恰好执行 prepare/commit/abort 一次: %d/%d/%d", transport.prepareCalls.Load(), transport.commitCalls.Load(), transport.abortCalls.Load())
	}
	compensations, err := repo.ListRuntimeDeliveryCompensations(ctx, invocation.ID, RuntimeDeliveryKindRejection, "", 10)
	if err != nil || len(compensations) != 1 || compensations[0].Status != RuntimeDeliveryCompensationQueued || compensations[0].DeliveryID != outbox.DeliveryID {
		t.Fatalf("最终失败必须登记 durable rejection compensation: %#v err=%v", compensations, err)
	}
	coordinator.dispatchDueRuntimeCompensationsBounded(ctx, now, 1)
	compensation, err := repo.GetRuntimeDeliveryCompensation(ctx, compensations[0].ID)
	if err != nil || compensation.Status != RuntimeDeliveryCompensationCompleted || transport.abortCalls.Load() != 2 {
		t.Fatalf("durable rejection compensation 应幂等完成并再执行一次 Abort: %#v aborts=%d err=%v", compensation, transport.abortCalls.Load(), err)
	}
	journal, err := repo.GetRuntimeDeliveryAttempt(ctx, RuntimeDeliveryAttemptID(RuntimeDeliveryKindRejection, "runtime-a", "runtime-b", outbox.DeliveryID))
	if err != nil || journal.Phase != RuntimeDeliveryAttemptFailed {
		t.Fatalf("rejection phase journal 必须保留 failed: %#v err=%v", journal, err)
	}
}

func TestCoordinatorDispatchesDurableEventCompensationAfterSourceFailure(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 10, 4, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	invocation := eventOutboxInvocation(t, repo, "inv-event-compensation-durable")
	event, err := repo.AppendEvent(ctx, AgentEvent{ID: "event-compensation-durable", InvocationID: invocation.ID, Type: EventRuntimeNotice, Timestamp: now})
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := repo.GetRuntimeEventOutbox(ctx, eventOutboxIDForTest(event.ID))
	if err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	failed := repo.eventOutbox[outbox.ID]
	failed.Status = RuntimeEventOutboxFailed
	failed.Attempt = MaxRuntimeEventOutboxAttempts
	failed.AvailableAt = now
	failed.UpdatedAt = now
	failed.LeaseOwner = ""
	failed.LeaseExpiresAt = nil
	repo.eventOutbox[outbox.ID] = failed
	repo.mu.Unlock()
	compensation, err := NewRuntimeDeliveryCompensation(RuntimeDeliveryKindEvent, "runtime-a", "runtime-b", outbox.ID, invocation.ID, outbox.ID, "", MaxRuntimeEventOutboxAttempts, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeDeliveryCompensation(ctx, compensation); err != nil {
		t.Fatal(err)
	}
	transport := &eventFinalAttemptAbortTransport{}
	transport.preparedID.Store(outbox.ID)
	claimed, ok, err := repo.ClaimRuntimeDeliveryCompensation(ctx, "crashed-compensation-worker", now, time.Second)
	if err != nil || !ok || claimed.Attempt != 1 {
		t.Fatalf("模拟 source 崩溃前 claim 失败: %#v ok=%v err=%v", claimed, ok, err)
	}
	coordinator := &Coordinator{repo: repo, deliveryCompensationWorkerID: "durable-compensation-worker", eventDeliveryTransport: transport}
	coordinator.dispatchDueRuntimeCompensationsBounded(ctx, now.Add(2*time.Second), 1)
	stored, err := repo.GetRuntimeDeliveryCompensation(ctx, compensation.ID)
	if err != nil || stored.Status != RuntimeDeliveryCompensationCompleted || stored.Attempt != 2 {
		t.Fatalf("durable compensation 应完成: %#v err=%v", stored, err)
	}
	if transport.abortCalls.Load() != 1 {
		t.Fatalf("durable compensation 应调用一次 Abort，实际=%d", transport.abortCalls.Load())
	}
}

type eventCommittedConflictAbortTransport struct {
	*eventFinalAttemptAbortTransport
}

func (t *eventCommittedConflictAbortTransport) Abort(context.Context, RuntimeEventDeliveryEnvelope) (RuntimeEventDeliveryReceipt, error) {
	return RuntimeEventDeliveryReceipt{}, ErrConflict
}

func TestCoordinatorMarksDurableCompensationFailedWhenDestinationAlreadyCommitted(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 10, 4, 30, 0, time.UTC)
	repo := NewMemoryRepository()
	invocation := eventOutboxInvocation(t, repo, "inv-event-compensation-committed")
	event, err := repo.AppendEvent(ctx, AgentEvent{ID: "event-compensation-committed", InvocationID: invocation.ID, Type: EventRuntimeNotice, Timestamp: now})
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := repo.GetRuntimeEventOutbox(ctx, eventOutboxIDForTest(event.ID))
	if err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	failed := repo.eventOutbox[outbox.ID]
	failed.Status = RuntimeEventOutboxFailed
	failed.Attempt = MaxRuntimeEventOutboxAttempts
	failed.AvailableAt = now
	failed.UpdatedAt = now
	failed.LeaseOwner = ""
	failed.LeaseExpiresAt = nil
	repo.eventOutbox[outbox.ID] = failed
	repo.mu.Unlock()
	compensation, err := NewRuntimeDeliveryCompensation(RuntimeDeliveryKindEvent, "runtime-a", "runtime-b", outbox.ID, invocation.ID, outbox.ID, "", MaxRuntimeEventOutboxAttempts, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeDeliveryCompensation(ctx, compensation); err != nil {
		t.Fatal(err)
	}
	transport := &eventCommittedConflictAbortTransport{eventFinalAttemptAbortTransport: &eventFinalAttemptAbortTransport{}}
	transport.preparedID.Store(outbox.ID)
	coordinator := &Coordinator{repo: repo, deliveryCompensationWorkerID: "durable-compensation-conflict-worker", eventDeliveryTransport: transport}
	coordinator.dispatchDueRuntimeCompensationsBounded(ctx, now, 1)
	stored, err := repo.GetRuntimeDeliveryCompensation(ctx, compensation.ID)
	if err != nil || stored.Status != RuntimeDeliveryCompensationFailed || !strings.Contains(stored.LastError, "冲突") {
		t.Fatalf("目的端已 committed 时 compensation 必须 terminal failed: %#v err=%v", stored, err)
	}
}

func TestCoordinatorRetriesDurableCompensationWhenTransportIsUnavailable(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 10, 4, 45, 0, time.UTC)
	repo := NewMemoryRepository()
	invocation := eventOutboxInvocation(t, repo, "inv-event-compensation-unavailable")
	event, err := repo.AppendEvent(ctx, AgentEvent{ID: "event-compensation-unavailable", InvocationID: invocation.ID, Type: EventRuntimeNotice, Timestamp: now})
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := repo.GetRuntimeEventOutbox(ctx, eventOutboxIDForTest(event.ID))
	if err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	failed := repo.eventOutbox[outbox.ID]
	failed.Status = RuntimeEventOutboxFailed
	failed.Attempt = MaxRuntimeEventOutboxAttempts
	failed.AvailableAt = now
	failed.UpdatedAt = now
	failed.LeaseOwner = ""
	failed.LeaseExpiresAt = nil
	repo.eventOutbox[outbox.ID] = failed
	repo.mu.Unlock()
	compensation, err := NewRuntimeDeliveryCompensation(RuntimeDeliveryKindEvent, "runtime-a", "runtime-b", outbox.ID, invocation.ID, outbox.ID, "", MaxRuntimeEventOutboxAttempts, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeDeliveryCompensation(ctx, compensation); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, deliveryCompensationWorkerID: "durable-compensation-unavailable-worker"}
	coordinator.dispatchDueRuntimeCompensationsBounded(ctx, now, 1)
	stored, err := repo.GetRuntimeDeliveryCompensation(ctx, compensation.ID)
	if err != nil || stored.Status != RuntimeDeliveryCompensationQueued || !stored.AvailableAt.After(now) || stored.LastError == "" {
		t.Fatalf("transport 不可用应进入 bounded retry，而非永久失败: %#v err=%v", stored, err)
	}
}

func TestCoordinatorDefersDurableCompensationUntilSourceLeaseIsSafe(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 10, 5, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	invocation := eventOutboxInvocation(t, repo, "inv-event-compensation-gate")
	event, err := repo.AppendEvent(ctx, AgentEvent{ID: "event-compensation-gate", InvocationID: invocation.ID, Type: EventRuntimeNotice, Timestamp: now})
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := repo.GetRuntimeEventOutbox(ctx, eventOutboxIDForTest(event.ID))
	if err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	processing := repo.eventOutbox[outbox.ID]
	processing.Status = RuntimeEventOutboxProcessing
	processing.Attempt = MaxRuntimeEventOutboxAttempts
	lease := now.Add(time.Minute)
	processing.LeaseOwner = "source-worker"
	processing.LeaseExpiresAt = &lease
	processing.UpdatedAt = now
	repo.eventOutbox[outbox.ID] = processing
	repo.mu.Unlock()
	compensation, err := NewRuntimeDeliveryCompensation(RuntimeDeliveryKindEvent, "runtime-a", "runtime-b", outbox.ID, invocation.ID, outbox.ID, "", MaxRuntimeEventOutboxAttempts, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeDeliveryCompensation(ctx, compensation); err != nil {
		t.Fatal(err)
	}
	transport := &eventFinalAttemptAbortTransport{}
	transport.preparedID.Store(outbox.ID)
	coordinator := &Coordinator{repo: repo, deliveryCompensationWorkerID: "durable-compensation-gate-worker", eventDeliveryTransport: transport}
	coordinator.dispatchDueRuntimeCompensationsBounded(ctx, now, 1)
	stored, err := repo.GetRuntimeDeliveryCompensation(ctx, compensation.ID)
	if err != nil || stored.Status != RuntimeDeliveryCompensationQueued || !stored.AvailableAt.After(now) {
		t.Fatalf("source lease 未失效时应 defer: %#v err=%v", stored, err)
	}
	if transport.abortCalls.Load() != 0 {
		t.Fatalf("source lease 仍有效时不应 Abort，实际=%d", transport.abortCalls.Load())
	}
	repo.mu.Lock()
	failed := repo.eventOutbox[outbox.ID]
	failed.Status = RuntimeEventOutboxFailed
	failed.LeaseOwner = ""
	failed.LeaseExpiresAt = nil
	failed.UpdatedAt = now.Add(time.Second)
	repo.eventOutbox[outbox.ID] = failed
	repo.mu.Unlock()
	coordinator.dispatchDueRuntimeCompensationsBounded(ctx, now.Add(2*time.Second), 1)
	stored, err = repo.GetRuntimeDeliveryCompensation(ctx, compensation.ID)
	if err != nil || stored.Status != RuntimeDeliveryCompensationCompleted || transport.abortCalls.Load() != 1 {
		t.Fatalf("source failed 后应执行并完成 Abort: %#v aborts=%d err=%v", stored, transport.abortCalls.Load(), err)
	}
}

func setRuntimeEventOutboxFinalAttempt(repo *MemoryRepository, id string, now time.Time) {
	repo.mu.Lock()
	item := repo.eventOutbox[id]
	item.Attempt = MaxRuntimeEventOutboxAttempts - 1
	item.Status = RuntimeEventOutboxQueued
	item.AvailableAt = now
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	repo.eventOutbox[id] = item
	repo.mu.Unlock()
}

func setRuntimeCheckpointOutboxFinalAttempt(repo *MemoryRepository, id string, now time.Time) {
	repo.mu.Lock()
	item := repo.checkpointOutbox[id]
	item.Attempt = MaxRuntimeCheckpointDeliveryOutboxAttempts - 1
	item.Status = RuntimeCheckpointDeliveryOutboxQueued
	item.AvailableAt = now
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	repo.checkpointOutbox[id] = item
	repo.mu.Unlock()
}

func setRuntimeConfigOutboxFinalAttempt(repo *MemoryRepository, id string, now time.Time) {
	repo.mu.Lock()
	item := repo.configDeliveryOutbox[id]
	item.Attempt = MaxRuntimeConfigDeliveryAttempts - 1
	item.Status = RuntimeConfigDeliveryOutboxQueued
	item.AvailableAt = now
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	repo.configDeliveryOutbox[id] = item
	repo.mu.Unlock()
}

func setRuntimeApprovalRejectionOutboxFinalAttempt(repo *MemoryRepository, id string, now time.Time) {
	repo.mu.Lock()
	item := repo.rejectionOutbox[id]
	item.Attempt = MaxRuntimeApprovalRejectionDeliveryAttempts - 1
	item.Status = RuntimeApprovalRejectionDeliveryOutboxQueued
	item.AvailableAt = now
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	repo.rejectionOutbox[id] = item
	repo.mu.Unlock()
}
