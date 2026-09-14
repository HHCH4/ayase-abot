package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

func TestSQLiteRuntimeCheckpointDeliveryPrepareCommitIsAtomicAndDurable(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Millisecond)
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-checkpoint-transaction-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	snapshot := agentruntime.RuntimeSnapshot{
		InvocationID: invocation.ID, Revision: 1, Phase: string(agentruntime.InvocationWaitingTool), WorkflowPhase: agentruntime.WorkflowPhaseWaitingTool,
		Workflow:    agentruntime.WorkflowCheckpoint{Status: agentruntime.WorkflowCheckpointWaiting, CurrentBranchID: "root", Branches: []agentruntime.WorkflowBranch{{ID: "root", Status: agentruntime.WorkflowBranchWaiting, HeadBoundaryID: "boundary:tx", UpdatedSequence: 2}}, Boundaries: []agentruntime.WorkflowBoundary{{ID: "boundary:tx", Sequence: 2, Kind: agentruntime.WorkflowBoundaryTool, Status: agentruntime.WorkflowBoundaryWaiting, BranchID: "root", WaitIDs: []string{"wait-1"}, PendingWaitIDs: []string{"wait-1"}, CreatedAt: now}}, ActiveBoundaryIDs: []string{"boundary:tx"}},
		GeneratedAt: now,
	}
	envelope, projection, err := agentruntime.NewRuntimeCheckpointDeliveryEnvelope("runtime-a", "runtime-b", snapshot, 2)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	envelope, err = agentruntime.SignRuntimeCheckpointDeliveryEnvelope(envelope, []byte("checkpoint-transaction-secret"))
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	txnRepo, ok := repo.(agentruntime.RuntimeCheckpointDeliveryTransactionRepository)
	if !ok {
		_ = store.Close()
		t.Fatal("SQLite repository 未实现 checkpoint transaction repository")
	}
	if duplicate, err := txnRepo.PrepareRuntimeCheckpointDelivery(ctx, envelope, projection); err != nil || duplicate {
		_ = store.Close()
		t.Fatalf("首次 SQLite prepare 错误: duplicate=%v err=%v", duplicate, err)
	}
	if _, err := repo.(agentruntime.RuntimeCheckpointDeliveryInbox).GetRuntimeCheckpointDelivery(ctx, invocation.ID); !errors.Is(err, agentruntime.ErrNotFound) {
		_ = store.Close()
		t.Fatalf("SQLite prepare 不得提前写 inbox: %v", err)
	}
	if duplicate, err := txnRepo.PrepareRuntimeCheckpointDelivery(ctx, envelope, projection); err != nil || !duplicate {
		_ = store.Close()
		t.Fatalf("SQLite duplicate prepare 必须幂等: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := txnRepo.CommitRuntimeCheckpointDelivery(ctx, envelope, projection); err != nil || duplicate {
		_ = store.Close()
		t.Fatalf("首次 SQLite commit 错误: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := txnRepo.CommitRuntimeCheckpointDelivery(ctx, envelope, projection); err != nil || !duplicate {
		_ = store.Close()
		t.Fatalf("SQLite duplicate commit 必须幂等: duplicate=%v err=%v", duplicate, err)
	}
	stored, err := repo.(agentruntime.RuntimeCheckpointDeliveryInbox).GetRuntimeCheckpointDelivery(ctx, invocation.ID)
	if err != nil || stored.SnapshotRevision != snapshot.Revision || stored.EventSequence != 2 {
		_ = store.Close()
		t.Fatalf("SQLite commit 后 inbox 错误: %#v err=%v", stored, err)
	}
	transaction, err := repo.(interface {
		GetRuntimeCheckpointDeliveryTransaction(context.Context, string) (agentruntime.RuntimeCheckpointDeliveryTransaction, error)
	}).GetRuntimeCheckpointDeliveryTransaction(ctx, envelope.DeliveryID)
	if err != nil || transaction.Status != agentruntime.RuntimeCheckpointDeliveryTransactionCommitted {
		_ = store.Close()
		t.Fatalf("SQLite transaction 状态错误: %#v err=%v", transaction, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	reopened := store.RuntimeRepository()
	reopenedTxn := reopened.(interface {
		GetRuntimeCheckpointDeliveryTransaction(context.Context, string) (agentruntime.RuntimeCheckpointDeliveryTransaction, error)
	})
	transaction, err = reopenedTxn.GetRuntimeCheckpointDeliveryTransaction(ctx, envelope.DeliveryID)
	if err != nil || transaction.Status != agentruntime.RuntimeCheckpointDeliveryTransactionCommitted {
		t.Fatalf("重启后 SQLite transaction 不可读: %#v err=%v", transaction, err)
	}
	stored, err = reopened.(agentruntime.RuntimeCheckpointDeliveryInbox).GetRuntimeCheckpointDelivery(ctx, invocation.ID)
	if err != nil || stored.SnapshotDigest != envelope.SnapshotDigest {
		t.Fatalf("重启后 SQLite inbox 不可读: %#v err=%v", stored, err)
	}
}

func TestSQLiteRuntimeCheckpointDeliveryAbortIsDurableAndNeverPublishes(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Millisecond)
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	snapshot := agentruntime.RuntimeSnapshot{InvocationID: "inv-checkpoint-transaction-abort-sqlite", Revision: 1, Phase: string(agentruntime.InvocationWaitingTool), Workflow: agentruntime.WorkflowCheckpoint{Status: agentruntime.WorkflowCheckpointWaiting, CurrentBranchID: "root", Branches: []agentruntime.WorkflowBranch{{ID: "root", Status: agentruntime.WorkflowBranchWaiting, HeadBoundaryID: "boundary:abort"}}, Boundaries: []agentruntime.WorkflowBoundary{{ID: "boundary:abort", Sequence: 1, Kind: agentruntime.WorkflowBoundaryTool, Status: agentruntime.WorkflowBoundaryWaiting, BranchID: "root", WaitIDs: []string{"wait-1"}, PendingWaitIDs: []string{"wait-1"}}}, ActiveBoundaryIDs: []string{"boundary:abort"}}, GeneratedAt: now}
	if err := repo.CreateInvocation(ctx, agentruntime.Invocation{ID: snapshot.InvocationID, UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	envelope, projection, err := agentruntime.NewRuntimeCheckpointDeliveryEnvelope("runtime-a", "runtime-b", snapshot, 1)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	envelope, err = agentruntime.SignRuntimeCheckpointDeliveryEnvelope(envelope, []byte("checkpoint-transaction-secret"))
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	txnRepo := repo.(agentruntime.RuntimeCheckpointDeliveryTransactionRepository)
	aborter := repo.(agentruntime.RuntimeCheckpointDeliveryAbortableTransactionRepository)
	if duplicate, err := txnRepo.PrepareRuntimeCheckpointDelivery(ctx, envelope, projection); err != nil || duplicate {
		_ = store.Close()
		t.Fatalf("prepare 失败: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := aborter.AbortRuntimeCheckpointDelivery(ctx, envelope, projection); err != nil || duplicate {
		_ = store.Close()
		t.Fatalf("首次 abort 失败: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := aborter.AbortRuntimeCheckpointDelivery(ctx, envelope, projection); err != nil || !duplicate {
		_ = store.Close()
		t.Fatalf("重复 abort 必须幂等: duplicate=%v err=%v", duplicate, err)
	}
	transaction, err := repo.(interface {
		GetRuntimeCheckpointDeliveryTransaction(context.Context, string) (agentruntime.RuntimeCheckpointDeliveryTransaction, error)
	}).GetRuntimeCheckpointDeliveryTransaction(ctx, envelope.DeliveryID)
	if err != nil || transaction.Status != agentruntime.RuntimeCheckpointDeliveryTransactionAborted {
		_ = store.Close()
		t.Fatalf("abort 状态未持久化: %#v err=%v", transaction, err)
	}
	if _, err := repo.(agentruntime.RuntimeCheckpointDeliveryInbox).GetRuntimeCheckpointDelivery(ctx, snapshot.InvocationID); !errors.Is(err, agentruntime.ErrNotFound) {
		_ = store.Close()
		t.Fatalf("abort 不得写入 checkpoint inbox: %v", err)
	}
	if _, err := txnRepo.CommitRuntimeCheckpointDelivery(ctx, envelope, projection); !errors.Is(err, agentruntime.ErrConflict) {
		_ = store.Close()
		t.Fatalf("aborted transaction 不得 commit: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	restarted := store.RuntimeRepository().(agentruntime.RuntimeCheckpointDeliveryAbortableTransactionRepository)
	if duplicate, err := restarted.AbortRuntimeCheckpointDelivery(ctx, envelope, projection); err != nil || !duplicate {
		t.Fatalf("重启后 aborted transaction 应幂等: duplicate=%v err=%v", duplicate, err)
	}
}

func TestSQLiteRuntimeCheckpointDeliveryCommitRequiresPreparedAndRejectsConflict(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	now := time.Now().UTC().Truncate(time.Millisecond)
	snapshot := agentruntime.RuntimeSnapshot{InvocationID: "inv-checkpoint-transaction-conflict", Revision: 1, Phase: string(agentruntime.InvocationWaitingTool), Workflow: agentruntime.WorkflowCheckpoint{Status: agentruntime.WorkflowCheckpointActive, CurrentBranchID: "root", Branches: []agentruntime.WorkflowBranch{{ID: "root", Status: agentruntime.WorkflowBranchActive}}}, GeneratedAt: now}
	repo := store.RuntimeRepository()
	if err := repo.CreateInvocation(ctx, agentruntime.Invocation{ID: snapshot.InvocationID, UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationRunning, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	envelope, projection, err := agentruntime.NewRuntimeCheckpointDeliveryEnvelope("runtime-a", "runtime-b", snapshot, 1)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err = agentruntime.SignRuntimeCheckpointDeliveryEnvelope(envelope, []byte("checkpoint-transaction-secret"))
	if err != nil {
		t.Fatal(err)
	}
	txnRepo := repo.(agentruntime.RuntimeCheckpointDeliveryTransactionRepository)
	if _, err := txnRepo.CommitRuntimeCheckpointDelivery(ctx, envelope, projection); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("未 prepare 的 SQLite commit 必须拒绝: %v", err)
	}
	if _, err := txnRepo.PrepareRuntimeCheckpointDelivery(ctx, envelope, projection); err != nil {
		t.Fatal(err)
	}
	conflict := projection
	conflict.Budget.ToolCallsUsed++
	if _, err := txnRepo.PrepareRuntimeCheckpointDelivery(ctx, envelope, conflict); !errors.Is(err, agentruntime.ErrRuntimeCheckpointDeliveryAuth) {
		t.Fatalf("SQLite 同 delivery projection 冲突必须拒绝: %v", err)
	}
}
