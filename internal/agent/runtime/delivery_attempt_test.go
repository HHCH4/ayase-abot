package runtime

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRuntimeDeliveryAttemptMemoryPhasesAreLeasedAndRecoverable(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 23, 0, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	if err := repo.CreateInvocation(ctx, Invocation{ID: "inv-attempt", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	candidate, err := NewRuntimeDeliveryAttempt(RuntimeDeliveryKindConfig, "runtime-a", "runtime-b", "delivery-1", "inv-attempt", "outbox-1", 7, 1, "worker-a", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	started, err := repo.BeginRuntimeDeliveryAttempt(ctx, candidate, now, time.Minute)
	if err != nil || started.Phase != RuntimeDeliveryAttemptStarted || started.Revision != 1 {
		t.Fatalf("首次 begin 错误: %#v err=%v", started, err)
	}
	prepared, duplicate, err := repo.AdvanceRuntimeDeliveryAttempt(ctx, started.ID, "worker-a", started.Revision, RuntimeDeliveryAttemptPrepared, now.Add(time.Second), "")
	if err != nil || duplicate || prepared.Phase != RuntimeDeliveryAttemptPrepared {
		t.Fatalf("started→prepared 错误: %#v duplicate=%v err=%v", prepared, duplicate, err)
	}
	if _, _, err := repo.AdvanceRuntimeDeliveryAttempt(ctx, prepared.ID, "wrong-worker", prepared.Revision, RuntimeDeliveryAttemptCommitted, now.Add(2*time.Second), ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("错误 owner 不应推进 attempt: %v", err)
	}
	// The original lease is expired. A new outbox claim (attempt=2) may take
	// over, but it must preserve prepared and resume with Commit.
	takeover := candidate
	takeover.Attempt = 2
	takeover.OutboxRevision = 9
	takeover.LeaseOwner = "worker-b"
	recovered, err := repo.BeginRuntimeDeliveryAttempt(ctx, takeover, now.Add(2*time.Minute), time.Minute)
	if err != nil || recovered.Phase != RuntimeDeliveryAttemptPrepared || recovered.Attempt != 2 || recovered.LeaseOwner != "worker-b" {
		t.Fatalf("过期 lease 接管不应丢失 prepared: %#v err=%v", recovered, err)
	}
	committed, duplicate, err := repo.AdvanceRuntimeDeliveryAttempt(ctx, recovered.ID, "worker-b", recovered.Revision, RuntimeDeliveryAttemptCommitted, now.Add(2*time.Minute+time.Second), "")
	if err != nil || duplicate || committed.Phase != RuntimeDeliveryAttemptCommitted {
		t.Fatalf("prepared→committed 错误: %#v duplicate=%v err=%v", committed, duplicate, err)
	}
	completed, duplicate, err := repo.AdvanceRuntimeDeliveryAttempt(ctx, committed.ID, "worker-b", committed.Revision, RuntimeDeliveryAttemptCompleted, now.Add(2*time.Minute+2*time.Second), "")
	if err != nil || duplicate || completed.Phase != RuntimeDeliveryAttemptCompleted || completed.LeaseOwner != "" || completed.CompletedAt == nil {
		t.Fatalf("committed→completed 错误: %#v duplicate=%v err=%v", completed, duplicate, err)
	}
	if _, _, err := repo.AdvanceRuntimeDeliveryAttempt(ctx, completed.ID, "worker-b", completed.Revision, RuntimeDeliveryAttemptStarted, now.Add(2*time.Minute+3*time.Second), ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("completed 不应回退: %v", err)
	}
	items, err := repo.ListRuntimeDeliveryAttempts(ctx, "inv-attempt", RuntimeDeliveryKindConfig, 10)
	if err != nil || len(items) != 1 || items[0].Phase != RuntimeDeliveryAttemptCompleted {
		t.Fatalf("attempt 查询投影错误: %#v err=%v", items, err)
	}
}

func TestRuntimeDeliveryAttemptFailureRequiresNewOutboxAttemptAndRedacts(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 23, 10, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	first, err := NewRuntimeDeliveryAttempt(RuntimeDeliveryKindEvent, "runtime-a", "event-handler", "delivery-fail", "inv-fail", "outbox-fail", 1, 1, "worker-a", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	first, err = repo.BeginRuntimeDeliveryAttempt(ctx, first, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	failed, _, err := repo.AdvanceRuntimeDeliveryAttempt(ctx, first.ID, first.LeaseOwner, first.Revision, RuntimeDeliveryAttemptFailed, now.Add(time.Second), "api_key=secret-value")
	if err != nil || failed.Phase != RuntimeDeliveryAttemptFailed || strings.Contains(failed.LastError, "secret-value") || !strings.Contains(failed.LastError, "[REDACTED]") {
		t.Fatalf("失败 attempt 应脱敏并释放 lease: %#v err=%v", failed, err)
	}
	retry := first
	retry.Attempt = 2
	retry.OutboxRevision = 3
	retry.LeaseOwner = "worker-b"
	restarted, err := repo.BeginRuntimeDeliveryAttempt(ctx, retry, now.Add(2*time.Second), time.Minute)
	if err != nil || restarted.Phase != RuntimeDeliveryAttemptStarted || restarted.Attempt != 2 || restarted.LastError != "" {
		t.Fatalf("新 outbox attempt 应从 started 重启: %#v err=%v", restarted, err)
	}
	forged := retry
	forged.InvocationID = "another-invocation"
	if _, err := repo.BeginRuntimeDeliveryAttempt(ctx, forged, now.Add(3*time.Second), time.Minute); !errors.Is(err, ErrConflict) {
		t.Fatalf("伪造 identity 应在稳定 ID 校验时拒绝: %v", err)
	}
}

func TestCoordinatorResumesPreparedCheckpointFromAttemptJournal(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 23, 20, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-attempt-checkpoint", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	_, _, outbox, err := repo.CommitRuntimeSnapshotWithCheckpoint(ctx, checkpointDeliveryTestSnapshot(t, invocation.ID, now), AgentEvent{ID: "attempt-checkpoint-event", InvocationID: invocation.ID, Type: EventRuntimeSnapshot, Timestamp: now}, "runtime-a", "runtime-b")
	if err != nil {
		t.Fatal(err)
	}
	outboxRepo := RuntimeCheckpointDeliveryOutboxRepository(repo)
	claimed, ok, err := outboxRepo.ClaimRuntimeCheckpointDeliveryOutbox(ctx, "worker-a", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("模拟首个 claim 失败: %#v ok=%v err=%v", claimed, ok, err)
	}
	coordinator := &Coordinator{repo: repo, checkpointOutboxWorkerID: "worker-b", checkpointDeliverySource: "runtime-a", checkpointDeliveryDestination: "runtime-b"}
	journal, enabled, err := coordinator.beginRuntimeDeliveryAttempt(ctx, RuntimeDeliveryKindCheckpoint, claimed.Source, claimed.Destination, claimed.DeliveryID, claimed.InvocationID, claimed.ID, claimed.Revision, claimed.Attempt, claimed.LeaseOwner, now)
	if err != nil || !enabled {
		t.Fatalf("登记首个 attempt 失败: enabled=%v err=%v", enabled, err)
	}
	journal, _, err = coordinator.advanceRuntimeDeliveryAttempt(ctx, journal, claimed.LeaseOwner, RuntimeDeliveryAttemptPrepared, now.Add(time.Second), "")
	if err != nil || journal.Phase != RuntimeDeliveryAttemptPrepared {
		t.Fatalf("登记 prepared 失败: %#v err=%v", journal, err)
	}
	var prepareCalls, commitCalls atomic.Int32
	coordinator.checkpointDeliveryTransport = checkpointTransactionalTransportFunc{
		prepare: func(context.Context, RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
			prepareCalls.Add(1)
			return RuntimeCheckpointDeliveryReceipt{}, errors.New("prepared journal 不应重复 prepare")
		},
		commit: func(_ context.Context, envelope RuntimeCheckpointDeliveryEnvelope, _ RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
			commitCalls.Add(1)
			return RuntimeCheckpointDeliveryReceipt{Version: envelope.Version, DeliveryID: envelope.DeliveryID, InvocationID: envelope.InvocationID, SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence, SnapshotDigest: envelope.SnapshotDigest, Phase: RuntimeCheckpointDeliveryTransactionPhaseCommitted}, nil
		},
	}
	// The source lease and journal lease are expired, so worker-b must take
	// over. The dispatcher should call Commit directly and then acknowledge the
	// source cursor exactly once.
	coordinator.dispatchDueRuntimeCheckpointOutbox(ctx, now.Add(2*time.Minute))
	if prepareCalls.Load() != 0 || commitCalls.Load() != 1 {
		t.Fatalf("prepared 恢复应只调用 commit: prepare=%d commit=%d", prepareCalls.Load(), commitCalls.Load())
	}
	completed, err := outboxRepo.GetRuntimeCheckpointDeliveryOutbox(ctx, outbox.ID)
	if err != nil || completed.Status != RuntimeCheckpointDeliveryOutboxCompleted {
		t.Fatalf("prepared 恢复未完成 source outbox: %#v err=%v", completed, err)
	}
	stored, err := repo.GetRuntimeDeliveryAttempt(ctx, journal.ID)
	if err != nil || stored.Phase != RuntimeDeliveryAttemptCompleted || stored.Attempt != 2 {
		t.Fatalf("attempt journal 未收口: %#v err=%v", stored, err)
	}
}
