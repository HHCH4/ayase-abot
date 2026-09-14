package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

func sqliteCheckpointSnapshot(invocationID string, revision int64, now time.Time) agentruntime.RuntimeSnapshot {
	return agentruntime.RuntimeSnapshot{
		InvocationID: invocationID, Revision: revision, ContractVersion: 1, PlanRevision: revision,
		Phase: string(agentruntime.InvocationWaitingTool), WorkflowPhase: agentruntime.WorkflowPhaseWaitingTool,
		Workflow: agentruntime.WorkflowCheckpoint{Status: agentruntime.WorkflowCheckpointWaiting, CurrentBranchID: "root", ActiveBoundaryIDs: []string{"boundary-1"},
			Branches:   []agentruntime.WorkflowBranch{{ID: "root", Status: agentruntime.WorkflowBranchWaiting, HeadBoundaryID: "boundary-1", UpdatedSequence: revision}},
			Boundaries: []agentruntime.WorkflowBoundary{{ID: "boundary-1", Sequence: revision, Kind: agentruntime.WorkflowBoundaryTool, Status: agentruntime.WorkflowBoundaryWaiting, BranchID: "root", WaitIDs: []string{"wait-1"}, PendingWaitIDs: []string{"wait-1"}, CreatedAt: now}},
		}, Budget: agentruntime.BudgetStatus{ContextWindow: 8192, OutputReserve: 256}, GeneratedAt: now,
	}
}

func TestSQLiteRuntimeCheckpointDeliveryInboxPersistsMonotonicProjection(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	now := time.Date(2026, 9, 13, 17, 0, 0, 0, time.UTC)
	secret := []byte("checkpoint-delivery-test-secret")
	snapshot := sqliteCheckpointSnapshot("inv-checkpoint-sqlite", 1, now)
	envelope, projection, err := agentruntime.NewRuntimeCheckpointDeliveryEnvelope("runtime-a", "runtime-b", snapshot, 2)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err = agentruntime.SignRuntimeCheckpointDeliveryEnvelope(envelope, secret)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository().(agentruntime.RuntimeCheckpointDeliveryInbox)
	duplicate, err := repo.AcceptRuntimeCheckpointDelivery(ctx, envelope, projection)
	if err != nil || duplicate {
		t.Fatalf("首次 SQLite checkpoint 写入错误: duplicate=%v err=%v", duplicate, err)
	}
	duplicate, err = repo.AcceptRuntimeCheckpointDelivery(ctx, envelope, projection)
	if err != nil || !duplicate {
		t.Fatalf("重复 SQLite checkpoint 应幂等: duplicate=%v err=%v", duplicate, err)
	}
	stored, err := repo.GetRuntimeCheckpointDelivery(ctx, envelope.InvocationID)
	if err != nil || stored.SnapshotDigest != envelope.SnapshotDigest || stored.Projection.InvocationID != envelope.InvocationID {
		t.Fatalf("SQLite checkpoint 读取错误: %#v err=%v", stored, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo = store.RuntimeRepository().(agentruntime.RuntimeCheckpointDeliveryInbox)
	duplicate, err = repo.AcceptRuntimeCheckpointDelivery(ctx, envelope, projection)
	if err != nil || !duplicate {
		t.Fatalf("SQLite 重启后相同 checkpoint 应幂等: duplicate=%v err=%v", duplicate, err)
	}

	newSnapshot := sqliteCheckpointSnapshot(envelope.InvocationID, 2, now.Add(time.Second))
	newEnvelope, newProjection, err := agentruntime.NewRuntimeCheckpointDeliveryEnvelope("runtime-a", "runtime-b", newSnapshot, 3)
	if err != nil {
		t.Fatal(err)
	}
	newEnvelope, err = agentruntime.SignRuntimeCheckpointDeliveryEnvelope(newEnvelope, secret)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := repo.AcceptRuntimeCheckpointDelivery(ctx, newEnvelope, newProjection); err != nil || duplicate {
		t.Fatalf("更高 SQLite checkpoint 应接受: duplicate=%v err=%v", duplicate, err)
	}
	if _, err := repo.AcceptRuntimeCheckpointDelivery(ctx, envelope, projection); !errors.Is(err, agentruntime.ErrRuntimeCheckpointDeliveryStale) {
		t.Fatalf("迟到 SQLite checkpoint 必须拒绝: %v", err)
	}
	conflict := newEnvelope
	conflict.Source = "other-runtime"
	conflict, err = agentruntime.SignRuntimeCheckpointDeliveryEnvelope(conflict, secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AcceptRuntimeCheckpointDelivery(ctx, conflict, newProjection); !errors.Is(err, agentruntime.ErrConflict) {
		t.Fatalf("不同 source 不能覆盖 SQLite checkpoint: %v", err)
	}
}
