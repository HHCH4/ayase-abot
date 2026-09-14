package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newMultiRebindApplyFixture(t *testing.T) (context.Context, *MemoryRepository, *Coordinator, RuntimeConfigDirectoryRebindPlan, RuntimeSnapshot, time.Time) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 16, 0, 0, 0, time.UTC)
	snapshotText, digest := rebindTestSnapshot(t, "rebind-multi-apply")
	invocation := Invocation{ID: "inv-rebind-multi-apply", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, ConfigSnapshot: snapshotText, ConfigSnapshotDigest: digest, CreatedAt: now, UpdatedAt: now}
	repo := NewMemoryRepository()
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, AgentEvent{ID: "rebind-multi-apply-event", InvocationID: invocation.ID, Type: EventInvocationWaiting, Timestamp: now}); err != nil {
		t.Fatal(err)
	}
	snapshot := checkpointDeliveryTestSnapshot(t, invocation.ID, now)
	if _, err := repo.SaveRuntimeSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	capabilities := []RuntimeConfigDirectoryRebindCapability{
		{Destination: "runtime-target-b", Revision: 2, SupportsRebind: true, SupportsCheckpoint: true, SupportsResume: true, SupportsConfigMaterialization: true},
		{Destination: "runtime-target-a", Revision: 3, SupportsRebind: true, SupportsCheckpoint: true, SupportsResume: true, SupportsConfigMaterialization: true},
	}
	plan, err := NewRuntimeConfigDirectoryRebindPlan("runtime-source", invocation, []string{"runtime-target-b", "runtime-target-a"}, capabilities, "", digest, "multi-plan-key", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeConfigDirectoryRebindPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	coordinator := newRebindTestCoordinator(t, repo)
	return ctx, repo, coordinator, plan, snapshot, now
}

func TestRuntimeConfigDirectoryRebindMultiApplyAtomicCommitAndReconcile(t *testing.T) {
	ctx, repo, coordinator, plan, snapshot, now := newMultiRebindApplyFixture(t)
	confirmation, err := coordinator.ConfirmRuntimeConfigDirectoryRebindMulti(ctx, plan.ID, "multi-confirm-key", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(confirmation.Destinations) != 2 || confirmation.Destinations[0] != "runtime-target-a" {
		t.Fatalf("multi confirmation 必须按 destination 排序: %#v", confirmation)
	}
	encoded, err := json.Marshal(confirmation)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "multi-confirm-key") {
		t.Fatalf("confirmation 不得泄露幂等键: %s", encoded)
	}
	parent, err := coordinator.ApplyRuntimeConfigDirectoryRebindMulti(ctx, plan.ID, confirmation.ID, "multi-apply-key", now)
	if err != nil {
		t.Fatal(err)
	}
	if parent.Status != RuntimeConfigDirectoryRebindMultiApplyQueued || parent.PendingCount != 2 || len(parent.ChildApplyIDs) != 2 || parent.SnapshotRevision != snapshot.Revision {
		t.Fatalf("multi parent 初始状态错误: %#v", parent)
	}
	children, err := repo.ListRuntimeConfigDirectoryRebindApplies(ctx, plan.ID, plan.InvocationID, RuntimeConfigDirectoryRebindApplyQueued, 10)
	if err != nil || len(children) != 2 {
		t.Fatalf("multi child 未全部原子登记: %#v err=%v", children, err)
	}
	for _, child := range children {
		if child.ParentID != parent.ID || child.Projection.InvocationID != plan.InvocationID {
			t.Fatalf("child parent/projection 错误: %#v", child)
		}
	}
	confirmationAfter, err := repo.GetRuntimeConfigDirectoryRebindMultiConfirmation(ctx, confirmation.ID)
	if err != nil || confirmationAfter.Status != RuntimeConfigDirectoryRebindMultiConfirmationConsumed {
		t.Fatalf("multi confirmation 必须原子消费: %#v err=%v", confirmationAfter, err)
	}
	repeated, err := coordinator.ApplyRuntimeConfigDirectoryRebindMulti(ctx, plan.ID, confirmation.ID, "multi-apply-key", now.Add(time.Second))
	if err != nil || repeated.ID != parent.ID {
		t.Fatalf("multi apply 重试必须幂等: %#v err=%v", repeated, err)
	}
	if _, err := coordinator.ApplyRuntimeConfigDirectoryRebindMulti(ctx, plan.ID, confirmation.ID, "different-apply-key", now.Add(time.Second)); !errors.Is(err, ErrRuntimeConfigDirectoryRebindMultiConfirmationConflict) {
		t.Fatalf("multi confirmation 不得被第二个 apply key 重复消费: %v", err)
	}

	first, claimed, err := repo.ClaimRuntimeConfigDirectoryRebindApply(ctx, "multi-worker", now.Add(time.Second), time.Minute)
	if err != nil || !claimed {
		t.Fatalf("第一个 child claim 失败: %#v claimed=%v err=%v", first, claimed, err)
	}
	if _, err := repo.CompleteRuntimeConfigDirectoryRebindApply(ctx, first.ID, "multi-worker", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	partial, err := coordinator.ReconcileRuntimeConfigDirectoryRebindMultiApply(ctx, parent.ID, now.Add(3*time.Second))
	if err != nil || partial.Status != RuntimeConfigDirectoryRebindMultiApplyPartial || partial.CompletedCount != 1 || partial.PendingCount != 1 {
		t.Fatalf("一个 child 完成后 parent 必须 partial: %#v err=%v", partial, err)
	}
	second, claimed, err := repo.ClaimRuntimeConfigDirectoryRebindApply(ctx, "multi-worker", now.Add(4*time.Second), time.Minute)
	if err != nil || !claimed {
		t.Fatalf("第二个 child claim 失败: %#v claimed=%v err=%v", second, claimed, err)
	}
	if _, err := repo.CompleteRuntimeConfigDirectoryRebindApply(ctx, second.ID, "multi-worker", now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	completed, err := coordinator.ReconcileRuntimeConfigDirectoryRebindMultiApply(ctx, parent.ID, now.Add(6*time.Second))
	if err != nil || completed.Status != RuntimeConfigDirectoryRebindMultiApplyCompleted || completed.CompletedCount != 2 || completed.CompletedAt == nil {
		t.Fatalf("全部 child 完成后 parent 必须 completed: %#v err=%v", completed, err)
	}
}

func TestRuntimeConfigDirectoryRebindMultiApplyAtomicRollbackAndConcurrentIdempotency(t *testing.T) {
	ctx, repo, coordinator, plan, snapshot, now := newMultiRebindApplyFixture(t)
	confirmation, err := coordinator.ConfirmRuntimeConfigDirectoryRebindMulti(ctx, plan.ID, "multi-rollback-confirm", now)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := NewRuntimeCheckpointProjection(snapshot, 1)
	if err != nil {
		t.Fatal(err)
	}
	projectionDigest, err := RuntimeCheckpointProjectionDigest(projection)
	if err != nil {
		t.Fatal(err)
	}
	confirmationDigest, err := RuntimeConfigDirectoryRebindMultiConfirmationDigest(confirmation)
	if err != nil {
		t.Fatal(err)
	}
	parentID := RuntimeConfigDirectoryRebindMultiApplyID(plan.ID, confirmation.ID, snapshot.Revision, 1, projectionDigest, "multi-rollback-apply", plan.Destinations)
	parent, children, err := NewRuntimeConfigDirectoryRebindMultiApply(plan, confirmation, parentID, snapshot, 1, "multi-rollback-apply", now)
	if err != nil {
		t.Fatal(err)
	}
	children[1].ConfirmationDigest = confirmationDigest[:len(confirmationDigest)-1] + "0"
	if _, err := repo.CommitRuntimeConfigDirectoryRebindMultiApply(ctx, confirmation.ID, parent, children, now); !errors.Is(err, ErrRuntimeConfigDirectoryRebindMultiApplyConflict) {
		t.Fatalf("篡改 child 必须整笔回滚: %v", err)
	}
	if _, err := repo.GetRuntimeConfigDirectoryRebindMultiApply(ctx, parent.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("失败后 parent 不得部分写入: %v", err)
	}
	confirmationAfter, err := repo.GetRuntimeConfigDirectoryRebindMultiConfirmation(ctx, confirmation.ID)
	if err != nil || confirmationAfter.Status != RuntimeConfigDirectoryRebindMultiConfirmationConfirmed {
		t.Fatalf("失败后 confirmation 必须保持 confirmed: %#v err=%v", confirmationAfter, err)
	}
	children[1].ConfirmationDigest = confirmationDigest
	var successes int
	var wait sync.WaitGroup
	var mu sync.Mutex
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, commitErr := repo.CommitRuntimeConfigDirectoryRebindMultiApply(ctx, confirmation.ID, parent, children, now); commitErr == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wait.Wait()
	if successes != 8 {
		t.Fatalf("相同 multi parent 重试必须全部幂等成功: %d", successes)
	}
	parents, err := repo.ListRuntimeConfigDirectoryRebindMultiApplies(ctx, plan.ID, plan.InvocationID, "", 10)
	if err != nil || len(parents) != 1 {
		t.Fatalf("并发 multi commit 必须只有一个 parent: %#v err=%v", parents, err)
	}
	storedChildren, err := repo.ListRuntimeConfigDirectoryRebindApplies(ctx, plan.ID, plan.InvocationID, "", 10)
	if err != nil || len(storedChildren) != 2 {
		t.Fatalf("并发 multi commit 必须只有两个 child: %#v err=%v", storedChildren, err)
	}
}

func TestRuntimeConfigDirectoryRebindMultiApplyMissingRouteStaysQueued(t *testing.T) {
	ctx, repo, coordinator, plan, _, now := newMultiRebindApplyFixture(t)
	confirmation, err := coordinator.ConfirmRuntimeConfigDirectoryRebindMulti(ctx, plan.ID, "multi-route-confirm", now)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := coordinator.ApplyRuntimeConfigDirectoryRebindMulti(ctx, plan.ID, confirmation.ID, "multi-route-apply", now)
	if err != nil {
		t.Fatal(err)
	}
	children, err := repo.ListRuntimeConfigDirectoryRebindApplies(ctx, plan.ID, plan.InvocationID, RuntimeConfigDirectoryRebindApplyQueued, 10)
	if err != nil || len(children) != 2 {
		t.Fatalf("未配置 route 时 children 必须保持 queued: %#v err=%v", children, err)
	}
	coordinator.DispatchDueRuntimeDeliveries(ctx, now.Add(time.Second))
	queued, err := repo.ListRuntimeConfigDirectoryRebindApplies(ctx, plan.ID, plan.InvocationID, RuntimeConfigDirectoryRebindApplyQueued, 10)
	if err != nil || len(queued) != 2 {
		t.Fatalf("缺失 transport 不得消耗 child: %#v err=%v", queued, err)
	}
	latest, err := coordinator.GetRuntimeConfigDirectoryRebindMultiApply(ctx, parent.ID)
	if err != nil || latest.Status != RuntimeConfigDirectoryRebindMultiApplyQueued {
		t.Fatalf("缺失 route 时 parent 应保持 queued: %#v err=%v", latest, err)
	}
}

func TestRuntimeConfigDirectoryRebindMultiApplyDispatchesRoutesIndependently(t *testing.T) {
	ctx, repo, coordinator, plan, _, now := newMultiRebindApplyFixture(t)
	var targetACalls, targetBCalls atomic.Int32
	var targetBFailures atomic.Int32
	makeTransport := func(destination string, calls *atomic.Int32, failOnce *atomic.Int32) rebindApplyTransportFunc {
		return rebindApplyTransportFunc(func(_ context.Context, envelope RuntimeConfigDirectoryRebindApplyEnvelope, projection RuntimeCheckpointProjection) (RuntimeConfigDirectoryRebindApplyReceipt, error) {
			calls.Add(1)
			if failOnce != nil && failOnce.Add(1) == 1 {
				return RuntimeConfigDirectoryRebindApplyReceipt{}, errors.New("temporary destination outage")
			}
			if envelope.Destination != destination {
				return RuntimeConfigDirectoryRebindApplyReceipt{}, ErrRuntimeConfigDirectoryRebindApplyAuth
			}
			duplicate, err := repo.AcceptRuntimeConfigDirectoryRebind(ctx, envelope, projection)
			if err != nil {
				return RuntimeConfigDirectoryRebindApplyReceipt{}, err
			}
			return RuntimeConfigDirectoryRebindApplyReceipt{Version: RuntimeConfigDirectoryRebindApplyReceiptVersion, Source: envelope.Source, Destination: envelope.Destination, PlanID: envelope.PlanID, ApplyID: envelope.ApplyID, InvocationID: envelope.InvocationID, SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence, SnapshotDigest: envelope.SnapshotDigest, Phase: RuntimeConfigDirectoryRebindApplyReceiptAccepted, Duplicate: duplicate, IssuedAt: now}, nil
		})
	}
	coordinator.SetRuntimeConfigDirectoryRebindApplyTransportForDestination("runtime-source", "runtime-target-a", makeTransport("runtime-target-a", &targetACalls, nil))
	coordinator.SetRuntimeConfigDirectoryRebindApplyTransportForDestination("runtime-source", "runtime-target-b", makeTransport("runtime-target-b", &targetBCalls, &targetBFailures))
	confirmation, err := coordinator.ConfirmRuntimeConfigDirectoryRebindMulti(ctx, plan.ID, "multi-dispatch-confirm", now)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := coordinator.ApplyRuntimeConfigDirectoryRebindMulti(ctx, plan.ID, confirmation.ID, "multi-dispatch-apply", now)
	if err != nil {
		t.Fatal(err)
	}
	partial, err := coordinator.GetRuntimeConfigDirectoryRebindMultiApply(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if partial.Status != RuntimeConfigDirectoryRebindMultiApplyPartial || partial.CompletedCount != 1 || partial.PendingCount != 1 {
		t.Fatalf("单目标临时失败后 parent 必须 partial: %#v", partial)
	}
	if targetACalls.Load() != 1 || targetBCalls.Load() != 1 {
		t.Fatalf("首次 dispatch 应各投递一次: a=%d b=%d", targetACalls.Load(), targetBCalls.Load())
	}
	coordinator.DispatchDueRuntimeDeliveries(ctx, now.Add(2*time.Second))
	completed, err := coordinator.GetRuntimeConfigDirectoryRebindMultiApply(ctx, parent.ID)
	if err != nil || completed.Status != RuntimeConfigDirectoryRebindMultiApplyCompleted || completed.CompletedCount != 2 {
		t.Fatalf("重试成功后 parent 必须 completed: %#v err=%v", completed, err)
	}
	if targetACalls.Load() != 1 || targetBCalls.Load() != 2 {
		t.Fatalf("已完成 child 不得重放: a=%d b=%d", targetACalls.Load(), targetBCalls.Load())
	}
}

func TestRuntimeConfigDirectoryRebindMultiApplyStatusReconcilesChildrenBeforeDeliver(t *testing.T) {
	ctx, _, coordinator, plan, _, now := newMultiRebindApplyFixture(t)
	var statusCalls, deliverCalls atomic.Int32
	makeTransport := func() rebindApplyReconcileTransport {
		return rebindApplyReconcileTransport{
			deliver: func(context.Context, RuntimeConfigDirectoryRebindApplyEnvelope, RuntimeCheckpointProjection) (RuntimeConfigDirectoryRebindApplyReceipt, error) {
				deliverCalls.Add(1)
				return RuntimeConfigDirectoryRebindApplyReceipt{}, errors.New("accepted status proof 后不得 Deliver")
			},
			reconcile: func(_ context.Context, envelope RuntimeConfigDirectoryRebindApplyEnvelope, _ RuntimeCheckpointProjection) (RuntimeConfigDirectoryRebindApplyStatusProof, error) {
				statusCalls.Add(1)
				return NewRuntimeConfigDirectoryRebindApplyStatusProof(envelope, RuntimeConfigDirectoryRebindApplyStatusAccepted, true, now, now)
			},
		}
	}
	coordinator.SetRuntimeConfigDirectoryRebindApplyTransportForDestination("runtime-source", "runtime-target-a", makeTransport())
	coordinator.SetRuntimeConfigDirectoryRebindApplyTransportForDestination("runtime-source", "runtime-target-b", makeTransport())
	confirmation, err := coordinator.ConfirmRuntimeConfigDirectoryRebindMulti(ctx, plan.ID, "multi-status-confirm", now)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := coordinator.ApplyRuntimeConfigDirectoryRebindMulti(ctx, plan.ID, confirmation.ID, "multi-status-apply", now)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := coordinator.GetRuntimeConfigDirectoryRebindMultiApply(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != RuntimeConfigDirectoryRebindMultiApplyCompleted || completed.CompletedCount != 2 || completed.PendingCount != 0 {
		t.Fatalf("multi child accepted proof 应直接收口 parent: %#v", completed)
	}
	if statusCalls.Load() != 2 || deliverCalls.Load() != 0 {
		t.Fatalf("multi child 应各查询一次且不重复 Deliver: status=%d deliver=%d", statusCalls.Load(), deliverCalls.Load())
	}
}
