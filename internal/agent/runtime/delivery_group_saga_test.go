package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRuntimeDeliveryGroupSagaStateMachineIsMonotonicAndDecisionBound(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	completed := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupCompleted)
	completed.InvocationID = "inv-saga-commit"
	completed.ID = RuntimeDeliveryGroupID(completed.Source, completed.Destination, completed.InvocationID, completed.Members)
	commit, err := NewRuntimeDeliveryGroupSaga(completed, now)
	if err != nil {
		t.Fatal(err)
	}
	if commit.Decision != RuntimeDeliveryGroupSagaCommit || commit.State != RuntimeDeliveryGroupSagaDecided || commit.RemotePhase != RuntimeDeliveryGroupSettlementRemoteAbsent || commit.Revision != 1 {
		t.Fatalf("commit saga 初始状态错误: %#v", commit)
	}
	if _, err := repo.EnqueueRuntimeDeliveryGroupSaga(ctx, commit); err != nil {
		t.Fatal(err)
	}
	advance := func(state RuntimeDeliveryGroupSagaState, remote RuntimeDeliveryGroupSettlementRemotePhase) RuntimeDeliveryGroupSaga {
		t.Helper()
		updated, duplicate, advanceErr := repo.AdvanceRuntimeDeliveryGroupSaga(ctx, commit.ID, commit.Revision, state, remote, "", now.Add(time.Duration(commit.Revision)*time.Second))
		if advanceErr != nil || duplicate {
			t.Fatalf("推进 saga %s 失败: value=%#v duplicate=%v err=%v", state, updated, duplicate, advanceErr)
		}
		commit = updated
		return updated
	}
	advance(RuntimeDeliveryGroupSagaPreparing, "")
	advance(RuntimeDeliveryGroupSagaPrepared, RuntimeDeliveryGroupSettlementRemotePrepared)
	advance(RuntimeDeliveryGroupSagaCommitting, "")
	final := advance(RuntimeDeliveryGroupSagaCommitted, RuntimeDeliveryGroupSettlementRemoteCommitted)
	if final.CompletedAt == nil || final.State != RuntimeDeliveryGroupSagaCommitted || final.RemotePhase != RuntimeDeliveryGroupSettlementRemoteCommitted || final.Revision != 5 {
		t.Fatalf("commit saga terminal 状态错误: %#v", final)
	}
	duplicate, isDuplicate, err := repo.AdvanceRuntimeDeliveryGroupSaga(ctx, final.ID, final.Revision, final.State, final.RemotePhase, "", now.Add(time.Minute))
	if err != nil || !isDuplicate || duplicate.Revision != final.Revision {
		t.Fatalf("terminal 重复推进应幂等: %#v duplicate=%v err=%v", duplicate, isDuplicate, err)
	}
	if _, _, err := repo.AdvanceRuntimeDeliveryGroupSaga(ctx, final.ID, final.Revision-1, RuntimeDeliveryGroupSagaCommitted, RuntimeDeliveryGroupSettlementRemoteCommitted, "", now.Add(time.Minute)); !errors.Is(err, ErrConflict) {
		t.Fatalf("旧 revision 必须拒绝: %v", err)
	}
	if _, _, err := repo.AdvanceRuntimeDeliveryGroupSaga(ctx, final.ID, final.Revision, RuntimeDeliveryGroupSagaPreparing, RuntimeDeliveryGroupSettlementRemotePrepared, "", now.Add(time.Minute)); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal saga 不应回退: %v", err)
	}

	failed := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupFailed)
	failed.InvocationID = "inv-saga-abort"
	failed.ID = RuntimeDeliveryGroupID(failed.Source, failed.Destination, failed.InvocationID, failed.Members)
	abort, err := NewRuntimeDeliveryGroupSaga(failed, now)
	if err != nil || abort.Decision != RuntimeDeliveryGroupSagaAbort {
		t.Fatalf("abort saga 初始状态错误: %#v err=%v", abort, err)
	}
	if _, err := repo.EnqueueRuntimeDeliveryGroupSaga(ctx, abort); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		state  RuntimeDeliveryGroupSagaState
		remote RuntimeDeliveryGroupSettlementRemotePhase
	}{
		{RuntimeDeliveryGroupSagaPreparing, ""},
		{RuntimeDeliveryGroupSagaPrepared, RuntimeDeliveryGroupSettlementRemotePrepared},
		{RuntimeDeliveryGroupSagaAborting, ""},
		{RuntimeDeliveryGroupSagaAborted, RuntimeDeliveryGroupSettlementRemoteAborted},
	} {
		updated, _, advanceErr := repo.AdvanceRuntimeDeliveryGroupSaga(ctx, abort.ID, abort.Revision, step.state, step.remote, "", now.Add(2*time.Minute))
		if advanceErr != nil {
			t.Fatalf("abort saga 推进 %s 失败: %v", step.state, advanceErr)
		}
		abort = updated
	}
	if abort.CompletedAt == nil || abort.State != RuntimeDeliveryGroupSagaAborted {
		t.Fatalf("abort saga terminal 状态错误: %#v", abort)
	}
	if _, _, err := repo.AdvanceRuntimeDeliveryGroupSaga(ctx, abort.ID, abort.Revision, RuntimeDeliveryGroupSagaCommitting, "", "", now); !errors.Is(err, ErrConflict) {
		t.Fatalf("abort saga 不应进入 committing: %v", err)
	}
}

func TestRuntimeDeliveryGroupSagaOppositeTerminalRequiresCompensation(t *testing.T) {
	now := time.Date(2026, 9, 13, 11, 0, 0, 0, time.UTC)
	group := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupCompleted)
	saga, err := NewRuntimeDeliveryGroupSaga(group, now)
	if err != nil {
		t.Fatal(err)
	}
	updated, duplicate, err := AdvanceRuntimeDeliveryGroupSaga(saga, saga.Revision, RuntimeDeliveryGroupSagaCompensationRequired, RuntimeDeliveryGroupSettlementRemoteAborted, "destination already aborted", now.Add(time.Second))
	if err != nil || duplicate || updated.State != RuntimeDeliveryGroupSagaCompensationRequired || updated.RemotePhase != RuntimeDeliveryGroupSettlementRemoteAborted || updated.CompletedAt != nil {
		t.Fatalf("opposite terminal 应进入 compensation_required: %#v duplicate=%v err=%v", updated, duplicate, err)
	}
	if _, _, err := AdvanceRuntimeDeliveryGroupSaga(updated, updated.Revision, RuntimeDeliveryGroupSagaDecided, RuntimeDeliveryGroupSettlementRemoteAbsent, "", now.Add(2*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("compensation_required 不应回退: %v", err)
	}
	if _, _, err := AdvanceRuntimeDeliveryGroupSaga(saga, saga.Revision, RuntimeDeliveryGroupSagaAborting, "", "", now.Add(time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("commit decision 不应进入 aborting: %v", err)
	}
}

func TestMemoryRuntimeDeliveryGroupSagaConcurrentCASHasOneWinner(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	group := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupCompleted)
	saga, err := NewRuntimeDeliveryGroupSaga(group, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeDeliveryGroupSaga(ctx, saga); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var successes atomic.Int32
	var conflicts atomic.Int32
	for _, state := range []RuntimeDeliveryGroupSagaState{RuntimeDeliveryGroupSagaPreparing, RuntimeDeliveryGroupSagaPrepared} {
		wg.Add(1)
		go func(state RuntimeDeliveryGroupSagaState) {
			defer wg.Done()
			_, _, advanceErr := repo.AdvanceRuntimeDeliveryGroupSaga(ctx, saga.ID, saga.Revision, state, "", "", now.Add(time.Second))
			switch {
			case advanceErr == nil:
				successes.Add(1)
			case errors.Is(advanceErr, ErrConflict):
				conflicts.Add(1)
			default:
				t.Errorf("并发 saga CAS 返回意外错误: %v", advanceErr)
			}
		}(state)
	}
	wg.Wait()
	if successes.Load() != 1 || conflicts.Load() != 1 {
		t.Fatalf("同一 saga revision 只能一个推进成功: successes=%d conflicts=%d", successes.Load(), conflicts.Load())
	}
}

func TestCoordinatorPersistsAndRepairsRuntimeDeliveryGroupSaga(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	repo := NewMemoryRepository()
	group := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupCompleted)
	if _, err := repo.EnqueueRuntimeDeliveryGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	transport := &testRuntimeDeliveryGroupTransport{}
	coordinator := &Coordinator{repo: repo, deliveryGroupTransport: transport, deliveryGroupSettlementWorkerID: "saga-worker"}
	coordinator.ReconcileRuntimeDeliveryGroups(ctx, now)
	saga, err := repo.GetRuntimeDeliveryGroupSaga(ctx, RuntimeDeliveryGroupSagaID(group.ID, RuntimeDeliveryGroupSagaCommit))
	if err != nil || saga.State != RuntimeDeliveryGroupSagaCommitted || saga.RemotePhase != RuntimeDeliveryGroupSettlementRemoteCommitted {
		t.Fatalf("Coordinator 未收口 commit saga: %#v err=%v", saga, err)
	}
	if transport.prepare.Load() != 1 || transport.commit.Load() != 1 {
		t.Fatalf("首次 settlement 调用次数错误: prepare=%d commit=%d", transport.prepare.Load(), transport.commit.Load())
	}

	// Simulate a crash after the saga terminal fact was committed but before the
	// queue Complete CAS. Recovery must complete the queue without another
	// remote side effect.
	crashGroup := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupCompleted)
	crashGroup.InvocationID = "inv-saga-crash"
	crashGroup.ID = RuntimeDeliveryGroupID(crashGroup.Source, crashGroup.Destination, crashGroup.InvocationID, crashGroup.Members)
	if _, err := repo.EnqueueRuntimeDeliveryGroup(ctx, crashGroup); err != nil {
		t.Fatal(err)
	}
	settlement, err := NewRuntimeDeliveryGroupSettlement(crashGroup, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeDeliveryGroupSettlement(ctx, settlement); err != nil {
		t.Fatal(err)
	}
	crashSaga, err := NewRuntimeDeliveryGroupSaga(crashGroup, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeDeliveryGroupSaga(ctx, crashSaga); err != nil {
		t.Fatal(err)
	}
	crashSaga, _, err = repo.AdvanceRuntimeDeliveryGroupSaga(ctx, crashSaga.ID, crashSaga.Revision, RuntimeDeliveryGroupSagaCommitted, RuntimeDeliveryGroupSettlementRemoteCommitted, "", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := repo.ClaimRuntimeDeliveryGroupSettlement(ctx, "saga-crash-worker", now.Add(time.Second), time.Minute)
	if err != nil || !ok {
		t.Fatalf("crash settlement claim 失败: %#v ok=%v err=%v", claimed, ok, err)
	}
	coordinator.processRuntimeDeliveryGroupSettlementClaim(ctx, repo, repo, claimed, "saga-crash-worker", now.Add(2*time.Second), transport, nil, nil)
	settled, err := repo.GetRuntimeDeliveryGroupSettlement(ctx, settlement.ID)
	if err != nil || settled.Status != RuntimeDeliveryGroupSettlementCompleted {
		t.Fatalf("saga terminal recovery 未完成 settlement: %#v err=%v", settled, err)
	}
	if transport.prepare.Load() != 1 || transport.commit.Load() != 1 {
		t.Fatalf("saga terminal recovery 不应重复远端副作用: prepare=%d commit=%d", transport.prepare.Load(), transport.commit.Load())
	}
}

type oppositeTerminalRuntimeDeliveryGroupTransport struct {
	testRuntimeDeliveryGroupTransport
}

func (t *oppositeTerminalRuntimeDeliveryGroupTransport) ReconcileRuntimeDeliveryGroup(_ context.Context, envelope RuntimeDeliveryGroupEnvelope) (RuntimeDeliveryGroupRemoteStatus, error) {
	return NewRuntimeDeliveryGroupRemoteStatus(envelope, RuntimeDeliveryGroupTransactionAborted, true, time.Now().UTC())
}

func TestCoordinatorMarksOppositeRuntimeDeliveryGroupSagaForCompensation(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	group := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupCompleted)
	if _, err := repo.EnqueueRuntimeDeliveryGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	transport := &oppositeTerminalRuntimeDeliveryGroupTransport{}
	coordinator := &Coordinator{repo: repo, deliveryGroupTransport: transport, deliveryGroupSettlementWorkerID: "saga-compensation-worker"}
	coordinator.ReconcileRuntimeDeliveryGroups(ctx, time.Now().UTC())
	saga, err := repo.GetRuntimeDeliveryGroupSaga(ctx, RuntimeDeliveryGroupSagaID(group.ID, RuntimeDeliveryGroupSagaCommit))
	if err != nil || saga.State != RuntimeDeliveryGroupSagaCompensationRequired || saga.RemotePhase != RuntimeDeliveryGroupSettlementRemoteAborted {
		t.Fatalf("opposite terminal 未进入 compensation_required: %#v err=%v", saga, err)
	}
	settlement, err := repo.GetRuntimeDeliveryGroupSettlement(ctx, RuntimeDeliveryGroupSettlementID(group.ID, RuntimeDeliveryGroupSettlementCommit))
	if err != nil || settlement.Status != RuntimeDeliveryGroupSettlementFailed {
		t.Fatalf("opposite terminal settlement 未 failed: %#v err=%v", settlement, err)
	}
	if transport.prepare.Load() != 0 || transport.commit.Load() != 0 || transport.abort.Load() != 0 {
		t.Fatalf("opposite terminal 不应继续调用远端写操作: prepare=%d commit=%d abort=%d", transport.prepare.Load(), transport.commit.Load(), transport.abort.Load())
	}
}

type preparedStatusRuntimeDeliveryGroupTransport struct {
	testRuntimeDeliveryGroupTransport
	status atomic.Int32
}

func (t *preparedStatusRuntimeDeliveryGroupTransport) ReconcileRuntimeDeliveryGroup(_ context.Context, envelope RuntimeDeliveryGroupEnvelope) (RuntimeDeliveryGroupRemoteStatus, error) {
	t.status.Add(1)
	return NewRuntimeDeliveryGroupRemoteStatus(envelope, RuntimeDeliveryGroupTransactionPrepared, true, time.Now().UTC())
}

func TestCoordinatorResumesPreparedRuntimeDeliveryGroupSaga(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	group := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupCompleted)
	if _, err := repo.EnqueueRuntimeDeliveryGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	transport := &preparedStatusRuntimeDeliveryGroupTransport{}
	coordinator := &Coordinator{repo: repo, deliveryGroupTransport: transport, deliveryGroupSettlementWorkerID: "saga-prepared-worker"}
	coordinator.ReconcileRuntimeDeliveryGroups(ctx, time.Now().UTC())
	saga, err := repo.GetRuntimeDeliveryGroupSaga(ctx, RuntimeDeliveryGroupSagaID(group.ID, RuntimeDeliveryGroupSagaCommit))
	if err != nil || saga.State != RuntimeDeliveryGroupSagaCommitted || saga.RemotePhase != RuntimeDeliveryGroupSettlementRemoteCommitted {
		t.Fatalf("prepared remote 后 saga 未完成 commit: %#v err=%v", saga, err)
	}
	settlement, err := repo.GetRuntimeDeliveryGroupSettlement(ctx, RuntimeDeliveryGroupSettlementID(group.ID, RuntimeDeliveryGroupSettlementCommit))
	if err != nil || settlement.Status != RuntimeDeliveryGroupSettlementCompleted || settlement.RemotePhase != RuntimeDeliveryGroupSettlementRemoteCommitted {
		t.Fatalf("prepared remote 后 settlement 未完成: %#v err=%v", settlement, err)
	}
	if transport.status.Load() != 1 || transport.prepare.Load() != 1 || transport.commit.Load() != 1 || transport.abort.Load() != 0 {
		t.Fatalf("prepared remote 的调用次数错误: status=%d prepare=%d commit=%d abort=%d", transport.status.Load(), transport.prepare.Load(), transport.commit.Load(), transport.abort.Load())
	}
}

func TestCoordinatorPersistsAbortRuntimeDeliveryGroupSaga(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	group := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupFailed)
	if _, err := repo.EnqueueRuntimeDeliveryGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	transport := &testRuntimeDeliveryGroupTransport{}
	coordinator := &Coordinator{repo: repo, deliveryGroupTransport: transport, deliveryGroupSettlementWorkerID: "saga-abort-worker"}
	coordinator.ReconcileRuntimeDeliveryGroups(ctx, time.Now().UTC())
	saga, err := repo.GetRuntimeDeliveryGroupSaga(ctx, RuntimeDeliveryGroupSagaID(group.ID, RuntimeDeliveryGroupSagaAbort))
	if err != nil || saga.State != RuntimeDeliveryGroupSagaAborted || saga.RemotePhase != RuntimeDeliveryGroupSettlementRemoteAborted {
		t.Fatalf("failed source group 未收口 abort saga: %#v err=%v", saga, err)
	}
	settlement, err := repo.GetRuntimeDeliveryGroupSettlement(ctx, RuntimeDeliveryGroupSettlementID(group.ID, RuntimeDeliveryGroupSettlementAbort))
	if err != nil || settlement.Status != RuntimeDeliveryGroupSettlementCompleted || settlement.Phase != RuntimeDeliveryGroupSettlementAbort {
		t.Fatalf("abort settlement 未完成: %#v err=%v", settlement, err)
	}
	if transport.prepare.Load() != 1 || transport.abort.Load() != 1 || transport.commit.Load() != 0 {
		t.Fatalf("abort settlement 的调用次数错误: prepare=%d commit=%d abort=%d", transport.prepare.Load(), transport.commit.Load(), transport.abort.Load())
	}
}
