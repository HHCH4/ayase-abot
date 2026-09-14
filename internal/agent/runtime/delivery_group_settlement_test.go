package runtime

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemoryRuntimeDeliveryGroupSettlementLifecycle(t *testing.T) {
	repo := NewMemoryRepository()
	group := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupCompleted)
	settlement, err := NewRuntimeDeliveryGroupSettlement(group, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if stored, err := repo.EnqueueRuntimeDeliveryGroupSettlement(ctx, settlement); err != nil || stored.ID != settlement.ID {
		t.Fatalf("首次 enqueue 异常: %#v err=%v", stored, err)
	}
	if stored, err := repo.EnqueueRuntimeDeliveryGroupSettlement(ctx, settlement); err != nil || stored.ID != settlement.ID {
		t.Fatalf("重复 enqueue 应幂等: %#v err=%v", stored, err)
	}
	claimed, won, err := repo.ClaimRuntimeDeliveryGroupSettlement(ctx, "worker-a", time.Now().UTC(), time.Minute)
	if err != nil || !won || claimed.Attempt != 1 || claimed.Status != RuntimeDeliveryGroupSettlementProcessing {
		t.Fatalf("claim 异常: %#v won=%v err=%v", claimed, won, err)
	}
	if completed, err := repo.CompleteRuntimeDeliveryGroupSettlement(ctx, claimed.ID, "wrong-worker", time.Now().UTC()); err != nil || completed {
		t.Fatalf("错误 owner 不应 complete: completed=%v err=%v", completed, err)
	}
	if retried, err := repo.RetryRuntimeDeliveryGroupSettlement(ctx, claimed.ID, "worker-a", time.Now().UTC(), "暂时不可达"); err != nil || !retried {
		t.Fatalf("retry 异常: retried=%v err=%v", retried, err)
	}
	queued, err := repo.GetRuntimeDeliveryGroupSettlement(ctx, claimed.ID)
	if err != nil || queued.Status != RuntimeDeliveryGroupSettlementQueued || queued.Attempt != 1 || queued.LastError != "暂时不可达" {
		t.Fatalf("retry 后状态不正确: %#v err=%v", queued, err)
	}
	claimed, won, err = repo.ClaimRuntimeDeliveryGroupSettlement(ctx, "worker-b", queued.AvailableAt.Add(time.Second), time.Minute)
	if err != nil || !won || claimed.Attempt != 2 {
		t.Fatalf("过期重试 claim 异常: %#v won=%v err=%v", claimed, won, err)
	}
	if failed, err := repo.FailRuntimeDeliveryGroupSettlement(ctx, claimed.ID, "worker-b", time.Now().UTC(), "永久冲突"); err != nil || !failed {
		t.Fatalf("fail 异常: failed=%v err=%v", failed, err)
	}
	final, err := repo.GetRuntimeDeliveryGroupSettlement(ctx, claimed.ID)
	if err != nil || final.Status != RuntimeDeliveryGroupSettlementFailed || final.LeaseOwner != "" || final.CompletedAt != nil {
		t.Fatalf("终态 settlement 不正确: %#v err=%v", final, err)
	}
}

func TestCoordinatorUsesDurableRuntimeDeliveryGroupSettlementQueue(t *testing.T) {
	repo := NewMemoryRepository()
	group := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupCompleted)
	if _, err := repo.EnqueueRuntimeDeliveryGroup(context.Background(), group); err != nil {
		t.Fatal(err)
	}
	transport := &testRuntimeDeliveryGroupTransport{}
	coordinator := &Coordinator{repo: repo, deliveryGroupTransport: transport, deliveryGroupSettlementWorkerID: "settlement-worker"}
	coordinator.ReconcileRuntimeDeliveryGroups(context.Background(), time.Now().UTC())
	settlementID := RuntimeDeliveryGroupSettlementID(group.ID, RuntimeDeliveryGroupSettlementCommit)
	settlement, err := repo.GetRuntimeDeliveryGroupSettlement(context.Background(), settlementID)
	if err != nil || settlement.Status != RuntimeDeliveryGroupSettlementCompleted || settlement.Attempt != 1 {
		t.Fatalf("远程 settlement 未完成: %#v err=%v", settlement, err)
	}
	if transport.prepare.Load() != 1 || transport.commit.Load() != 1 || transport.abort.Load() != 0 {
		t.Fatalf("首次远程 settlement 调用错误: prepare=%d commit=%d abort=%d", transport.prepare.Load(), transport.commit.Load(), transport.abort.Load())
	}
	coordinator.ReconcileRuntimeDeliveryGroups(context.Background(), time.Now().UTC().Add(time.Second))
	if transport.prepare.Load() != 1 || transport.commit.Load() != 1 {
		t.Fatalf("已完成 settlement 不应重复远程调用: prepare=%d commit=%d", transport.prepare.Load(), transport.commit.Load())
	}
}

func TestCoordinatorMarksTerminalGroupAndSettlementInOneMemoryBoundary(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC().Truncate(time.Millisecond)
	group, err := NewRuntimeDeliveryGroup("runtime-a", "runtime-b", "inv-group-atomic-boundary", []RuntimeDeliveryGroupMember{
		{Kind: RuntimeDeliveryKindEvent, OutboxID: "event-atomic-boundary", DeliveryID: "event-atomic-boundary"},
		{Kind: RuntimeDeliveryKindCheckpoint, OutboxID: "checkpoint-atomic-boundary", DeliveryID: "checkpoint-atomic-boundary"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeDeliveryGroup(context.Background(), group); err != nil {
		t.Fatal(err)
	}
	transport := &testRuntimeDeliveryGroupTransport{}
	coordinator := &Coordinator{repo: repo, deliveryGroupTransport: transport}
	claimed, won, err := repo.ClaimRuntimeDeliveryGroup(context.Background(), "atomic-worker", now, time.Minute)
	if err != nil || !won {
		t.Fatalf("claim group 失败: %#v won=%v err=%v", claimed, won, err)
	}
	coordinator.markRuntimeDeliveryGroupMember(context.Background(), group.ID, RuntimeDeliveryKindEvent, "event-atomic-boundary", "event-atomic-boundary", RuntimeDeliveryGroupMemberCompleted, "", now)
	coordinator.markRuntimeDeliveryGroupMember(context.Background(), group.ID, RuntimeDeliveryKindCheckpoint, "checkpoint-atomic-boundary", "checkpoint-atomic-boundary", RuntimeDeliveryGroupMemberCompleted, "", now)
	final, err := repo.GetRuntimeDeliveryGroup(context.Background(), group.ID)
	if err != nil || final.Status != RuntimeDeliveryGroupCompleted {
		t.Fatalf("coordinator 未收口终态 group: %#v err=%v", final, err)
	}
	settlement, err := repo.GetRuntimeDeliveryGroupSettlement(context.Background(), RuntimeDeliveryGroupSettlementID(group.ID, RuntimeDeliveryGroupSettlementCommit))
	if err != nil || settlement.Status != RuntimeDeliveryGroupSettlementQueued {
		t.Fatalf("最后 member mark 必须同步登记 queued settlement: %#v err=%v", settlement, err)
	}
	if transport.prepare.Load() != 0 || transport.commit.Load() != 0 || transport.abort.Load() != 0 {
		t.Fatalf("member mark 不应在本地边界内提前调用远端 transport: prepare=%d commit=%d abort=%d", transport.prepare.Load(), transport.commit.Load(), transport.abort.Load())
	}
}

type flakyRuntimeDeliveryGroupTransport struct {
	prepare atomic.Int32
	commit  atomic.Int32
}

type batchRecordingRuntimeDeliveryGroupTransport struct {
	testRuntimeDeliveryGroupTransport
	batches atomic.Int32
}

func (t *batchRecordingRuntimeDeliveryGroupTransport) ReconcileRuntimeDeliveryGroups(_ context.Context, envelopes []RuntimeDeliveryGroupEnvelope) ([]RuntimeDeliveryGroupRemoteStatus, error) {
	t.batches.Add(1)
	statuses := make([]RuntimeDeliveryGroupRemoteStatus, len(envelopes))
	for index, envelope := range envelopes {
		status, err := NewRuntimeDeliveryGroupRemoteStatus(envelope, "absent", false, time.Time{})
		if err != nil {
			return nil, err
		}
		statuses[index] = status
	}
	return statuses, nil
}

func TestCoordinatorUsesBatchStatusReconciliationForDueSettlements(t *testing.T) {
	repo := NewMemoryRepository()
	first := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupCompleted)
	second := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupCompleted)
	second.InvocationID = "inv-group-txn-batch-coordinator-2"
	second.ID = RuntimeDeliveryGroupID(second.Source, second.Destination, second.InvocationID, second.Members)
	for _, group := range []RuntimeDeliveryGroup{first, second} {
		if _, err := repo.EnqueueRuntimeDeliveryGroup(context.Background(), group); err != nil {
			t.Fatal(err)
		}
	}
	transport := &batchRecordingRuntimeDeliveryGroupTransport{}
	coordinator := &Coordinator{repo: repo, deliveryGroupTransport: transport, deliveryGroupSettlementWorkerID: "batch-settlement-worker"}
	coordinator.ReconcileRuntimeDeliveryGroups(context.Background(), time.Now().UTC())
	if transport.batches.Load() != 1 || transport.prepare.Load() != 2 || transport.commit.Load() != 2 {
		t.Fatalf("批量 status 对账未被使用或结算不完整: batches=%d prepare=%d commit=%d", transport.batches.Load(), transport.prepare.Load(), transport.commit.Load())
	}
	for _, group := range []RuntimeDeliveryGroup{first, second} {
		settlementID := RuntimeDeliveryGroupSettlementID(group.ID, RuntimeDeliveryGroupSettlementCommit)
		settlement, err := repo.GetRuntimeDeliveryGroupSettlement(context.Background(), settlementID)
		if err != nil || settlement.Status != RuntimeDeliveryGroupSettlementCompleted || settlement.RemotePhase != RuntimeDeliveryGroupSettlementRemoteCommitted {
			t.Fatalf("批量结算终态错误: %#v err=%v", settlement, err)
		}
	}
}

func (t *flakyRuntimeDeliveryGroupTransport) BuildRuntimeDeliveryGroupEnvelope(group RuntimeDeliveryGroup, now time.Time) (RuntimeDeliveryGroupEnvelope, error) {
	return NewRuntimeDeliveryGroupEnvelope(group, now)
}

func (t *flakyRuntimeDeliveryGroupTransport) PrepareRuntimeDeliveryGroup(_ context.Context, envelope RuntimeDeliveryGroupEnvelope) (RuntimeDeliveryGroupTransactionReceipt, error) {
	if t.prepare.Add(1) == 1 {
		return RuntimeDeliveryGroupTransactionReceipt{}, errors.New("remote unavailable")
	}
	return RuntimeDeliveryGroupTransactionReceipt{Version: envelope.Version, GroupID: envelope.GroupID, InvocationID: envelope.InvocationID, MembersDigest: envelope.MembersDigest, Phase: "prepare"}, nil
}

func (t *flakyRuntimeDeliveryGroupTransport) CommitRuntimeDeliveryGroup(_ context.Context, envelope RuntimeDeliveryGroupEnvelope) (RuntimeDeliveryGroupTransactionReceipt, error) {
	t.commit.Add(1)
	return RuntimeDeliveryGroupTransactionReceipt{Version: envelope.Version, GroupID: envelope.GroupID, InvocationID: envelope.InvocationID, MembersDigest: envelope.MembersDigest, Phase: "commit"}, nil
}

func (t *flakyRuntimeDeliveryGroupTransport) AbortRuntimeDeliveryGroup(_ context.Context, envelope RuntimeDeliveryGroupEnvelope) (RuntimeDeliveryGroupTransactionReceipt, error) {
	return RuntimeDeliveryGroupTransactionReceipt{Version: envelope.Version, GroupID: envelope.GroupID, InvocationID: envelope.InvocationID, MembersDigest: envelope.MembersDigest, Phase: "abort"}, nil
}

func TestCoordinatorRetriesRuntimeDeliveryGroupSettlementAfterTransientFailure(t *testing.T) {
	repo := NewMemoryRepository()
	group := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupCompleted)
	if _, err := repo.EnqueueRuntimeDeliveryGroup(context.Background(), group); err != nil {
		t.Fatal(err)
	}
	transport := &flakyRuntimeDeliveryGroupTransport{}
	coordinator := &Coordinator{repo: repo, deliveryGroupTransport: transport, deliveryGroupSettlementWorkerID: "settlement-worker"}
	now := time.Now().UTC()
	coordinator.ReconcileRuntimeDeliveryGroups(context.Background(), now)
	settlementID := RuntimeDeliveryGroupSettlementID(group.ID, RuntimeDeliveryGroupSettlementCommit)
	queued, err := repo.GetRuntimeDeliveryGroupSettlement(context.Background(), settlementID)
	if err != nil || queued.Status != RuntimeDeliveryGroupSettlementQueued || queued.Attempt != 1 {
		t.Fatalf("瞬时失败后应进入 queued: %#v err=%v", queued, err)
	}
	coordinator.DispatchDueRuntimeDeliveries(context.Background(), queued.AvailableAt.Add(time.Second))
	settled, err := repo.GetRuntimeDeliveryGroupSettlement(context.Background(), settlementID)
	if err != nil || settled.Status != RuntimeDeliveryGroupSettlementCompleted || settled.Attempt != 2 {
		t.Fatalf("重试后 settlement 未完成: %#v err=%v", settled, err)
	}
	if transport.prepare.Load() != 2 || transport.commit.Load() != 1 {
		t.Fatalf("重试远程调用次数错误: prepare=%d commit=%d", transport.prepare.Load(), transport.commit.Load())
	}
}

func TestRuntimeDeliveryGroupSettlementProgressIsCASProtectedAndBounded(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC().Truncate(time.Millisecond)
	group := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupCompleted)
	settlement, err := NewRuntimeDeliveryGroupSettlement(group, now)
	if err != nil {
		t.Fatal(err)
	}
	if settlement.Stage != RuntimeDeliveryGroupSettlementStageQueued || settlement.RemotePhase != "" {
		t.Fatalf("新 settlement 阶段默认值错误: %#v", settlement)
	}
	if _, err := repo.EnqueueRuntimeDeliveryGroupSettlement(context.Background(), settlement); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := repo.ClaimRuntimeDeliveryGroupSettlement(context.Background(), "stage-owner", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim settlement 失败: %#v err=%v", claimed, err)
	}
	if claimed.Stage != RuntimeDeliveryGroupSettlementStageStatus || claimed.Attempt != 1 {
		t.Fatalf("claim 未进入 status stage: %#v", claimed)
	}
	progress, duplicate, err := repo.AdvanceRuntimeDeliveryGroupSettlement(context.Background(), claimed.ID, "stage-owner", claimed.Revision, RuntimeDeliveryGroupSettlementStageStatus, RuntimeDeliveryGroupSettlementRemoteAbsent, "", "", now.Add(time.Second))
	if err != nil || duplicate || progress.RemotePhase != RuntimeDeliveryGroupSettlementRemoteAbsent || progress.LastStatusAt == nil {
		t.Fatalf("status progress 异常: %#v duplicate=%v err=%v", progress, duplicate, err)
	}
	same, duplicate, err := repo.AdvanceRuntimeDeliveryGroupSettlement(context.Background(), claimed.ID, "stage-owner", progress.Revision, RuntimeDeliveryGroupSettlementStageStatus, RuntimeDeliveryGroupSettlementRemoteAbsent, "", "", now.Add(time.Second))
	if err != nil || !duplicate || same.Revision != progress.Revision {
		t.Fatalf("重复 progress 不应递增 revision: %#v duplicate=%v err=%v", same, duplicate, err)
	}
	progress, _, err = repo.AdvanceRuntimeDeliveryGroupSettlement(context.Background(), claimed.ID, "stage-owner", progress.Revision, RuntimeDeliveryGroupSettlementStagePreparing, "", "", "", now.Add(2*time.Second))
	if err != nil || progress.Stage != RuntimeDeliveryGroupSettlementStagePreparing {
		t.Fatalf("preparing stage 异常: %#v err=%v", progress, err)
	}
	progress, _, err = repo.AdvanceRuntimeDeliveryGroupSettlement(context.Background(), claimed.ID, "stage-owner", progress.Revision, RuntimeDeliveryGroupSettlementStagePrepared, RuntimeDeliveryGroupSettlementRemotePrepared, "", "", now.Add(3*time.Second))
	if err != nil || progress.Stage != RuntimeDeliveryGroupSettlementStagePrepared || progress.RemotePhase != RuntimeDeliveryGroupSettlementRemotePrepared {
		t.Fatalf("prepared stage 异常: %#v err=%v", progress, err)
	}
	progress, _, err = repo.AdvanceRuntimeDeliveryGroupSettlement(context.Background(), claimed.ID, "stage-owner", progress.Revision, RuntimeDeliveryGroupSettlementStageCommitting, "", RuntimeDeliveryGroupSettlementAlertRemoteUnavailable, "temporary upstream outage", now.Add(4*time.Second))
	if err != nil || progress.AlertCode != RuntimeDeliveryGroupSettlementAlertRemoteUnavailable || progress.AlertCount != 1 || !strings.Contains(progress.LastError, "temporary upstream outage") {
		t.Fatalf("告警 metadata 异常: %#v err=%v", progress, err)
	}
	if _, _, err := repo.AdvanceRuntimeDeliveryGroupSettlement(context.Background(), claimed.ID, "stage-owner", progress.Revision, RuntimeDeliveryGroupSettlementStageCommitting, RuntimeDeliveryGroupSettlementRemoteAbsent, "", "", now.Add(5*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("prepared 之后 remote phase 回退应拒绝: %v", err)
	}
	if _, _, err := repo.AdvanceRuntimeDeliveryGroupSettlement(context.Background(), claimed.ID, "wrong-owner", progress.Revision, RuntimeDeliveryGroupSettlementStageCommitting, RuntimeDeliveryGroupSettlementRemoteCommitted, "", "", now.Add(5*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("错误 owner 不应推进 settlement: %v", err)
	}
	if completed, err := repo.CompleteRuntimeDeliveryGroupSettlement(context.Background(), claimed.ID, "stage-owner", now.Add(6*time.Second)); err != nil || !completed {
		t.Fatalf("complete settlement 失败: completed=%v err=%v", completed, err)
	}
	final, err := repo.GetRuntimeDeliveryGroupSettlement(context.Background(), claimed.ID)
	if err != nil || final.Status != RuntimeDeliveryGroupSettlementCompleted || final.Stage != RuntimeDeliveryGroupSettlementStageCompleted || final.AlertCount != 1 {
		t.Fatalf("终态 settlement metadata 错误: %#v err=%v", final, err)
	}
}
