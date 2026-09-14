package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

func TestSQLiteRuntimeConfigDirectoryRebindApplyPersistsConfirmationQueueAndInbox(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	now := time.Date(2026, 9, 13, 17, 0, 0, 0, time.UTC)
	snapshotText, configDigest := sqliteRebindSnapshot(t)
	invocation := agentruntime.Invocation{ID: "inv-sqlite-rebind-apply", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, ConfigSnapshot: snapshotText, ConfigSnapshotDigest: configDigest, CreatedAt: now, UpdatedAt: now}
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, agentruntime.AgentEvent{ID: "sqlite-rebind-apply-event", InvocationID: invocation.ID, Type: agentruntime.EventInvocationWaiting, Timestamp: now}); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	snapshot := sqliteCheckpointSnapshot(invocation.ID, 1, now)
	if _, err := repo.(agentruntime.RuntimeSnapshotRepository).SaveRuntimeSnapshot(ctx, snapshot); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	plan, err := agentruntime.NewRuntimeConfigDirectoryRebindPlan("runtime-source", invocation, []string{"runtime-target"}, []agentruntime.RuntimeConfigDirectoryRebindCapability{sqliteRebindCapability("runtime-target", 2)}, "", configDigest, "sqlite-apply-plan", now)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	planRepo := repo.(agentruntime.RuntimeConfigDirectoryRebindPlanRepository)
	if _, err := planRepo.EnqueueRuntimeConfigDirectoryRebindPlan(ctx, plan); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	confirmationRepo := repo.(agentruntime.RuntimeConfigDirectoryRebindConfirmationRepository)
	confirmation, err := confirmationRepo.CreateRuntimeConfigDirectoryRebindConfirmation(ctx, plan, "sqlite-confirm", now)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	applyRepo := repo.(agentruntime.RuntimeConfigDirectoryRebindApplyRepository)
	confirmationDigest, err := agentruntime.RuntimeConfigDirectoryRebindConfirmationDigest(confirmation)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	projection, err := agentruntime.NewRuntimeCheckpointProjection(snapshot, 1)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	snapshotDigest, err := agentruntime.RuntimeCheckpointProjectionDigest(projection)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	apply := agentruntime.RuntimeConfigDirectoryRebindApply{
		ID:     agentruntime.RuntimeConfigDirectoryRebindApplyID(plan.ID, "runtime-target", snapshot.Revision, 1, snapshotDigest, "sqlite-apply"),
		PlanID: plan.ID, ConfirmationID: confirmation.ID, ConfirmationDigest: confirmationDigest, Source: plan.Source, Destination: "runtime-target", InvocationID: plan.InvocationID,
		BoundaryStatus: plan.BoundaryStatus, ConfigSnapshotDigest: configDigest, SnapshotRevision: snapshot.Revision, EventSequence: 1, SnapshotDigest: snapshotDigest,
		Projection: projection, Status: agentruntime.RuntimeConfigDirectoryRebindApplyQueued, AvailableAt: now, CreatedAt: now, UpdatedAt: now, IdempotencyKey: "sqlite-apply",
	}
	saved, err := repo.(agentruntime.RuntimeConfigDirectoryRebindApplyCommitRepository).CommitRuntimeConfigDirectoryRebindApply(ctx, confirmation.ID, apply, now)
	if err != nil {
		_ = store.Close()
		t.Fatalf("SQLite confirmation/apply 原子提交失败: %v", err)
	}
	if saved.ID != apply.ID || saved.Status != agentruntime.RuntimeConfigDirectoryRebindApplyQueued {
		_ = store.Close()
		t.Fatalf("SQLite apply 结果错误: %#v", saved)
	}
	encoded, err := json.Marshal(saved)
	if err != nil || strings.Contains(string(encoded), "sqlite-apply") {
		_ = store.Close()
		t.Fatalf("SQLite apply JSON 不得泄露幂等键: %s err=%v", encoded, err)
	}
	claimed, ok, err := applyRepo.ClaimRuntimeConfigDirectoryRebindApply(ctx, "sqlite-apply-worker", now, time.Minute)
	if err != nil || !ok || claimed.Attempt != 1 {
		_ = store.Close()
		t.Fatalf("SQLite apply claim 错误: %#v ok=%v err=%v", claimed, ok, err)
	}
	if done, err := applyRepo.CompleteRuntimeConfigDirectoryRebindApply(ctx, claimed.ID, "wrong-worker", now); err != nil || done {
		_ = store.Close()
		t.Fatalf("错误 worker 不得完成 SQLite apply: done=%v err=%v", done, err)
	}
	if done, err := applyRepo.CompleteRuntimeConfigDirectoryRebindApply(ctx, claimed.ID, claimed.LeaseOwner, now); err != nil || !done {
		_ = store.Close()
		t.Fatalf("SQLite apply complete 失败: done=%v err=%v", done, err)
	}
	envelope, err := saved.Envelope(now.Add(time.Second))
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	inboxRepo := repo.(agentruntime.RuntimeConfigDirectoryRebindInbox)
	duplicate, err := inboxRepo.AcceptRuntimeConfigDirectoryRebind(ctx, envelope, saved.Projection)
	if err != nil || duplicate {
		_ = store.Close()
		t.Fatalf("SQLite inbox 首次写入错误: duplicate=%v err=%v", duplicate, err)
	}
	duplicate, err = inboxRepo.AcceptRuntimeConfigDirectoryRebind(ctx, envelope, saved.Projection)
	if err != nil || !duplicate {
		_ = store.Close()
		t.Fatalf("SQLite inbox 重复请求必须幂等: duplicate=%v err=%v", duplicate, err)
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
	got, err := reopened.(agentruntime.RuntimeConfigDirectoryRebindApplyRepository).GetRuntimeConfigDirectoryRebindApply(ctx, apply.ID)
	if err != nil || got.Status != agentruntime.RuntimeConfigDirectoryRebindApplyCompleted || got.Projection.InvocationID != invocation.ID {
		t.Fatalf("SQLite 重启后 apply 不可读: %#v err=%v", got, err)
	}
	storedInbox, err := reopened.(agentruntime.RuntimeConfigDirectoryRebindInbox).GetRuntimeConfigDirectoryRebind(ctx, apply.ID)
	if err != nil || storedInbox.ApplyID != apply.ID || storedInbox.Projection.InvocationID != invocation.ID {
		t.Fatalf("SQLite 重启后 inbox 不可读: %#v err=%v", storedInbox, err)
	}
	confirmationAfter, err := reopened.(agentruntime.RuntimeConfigDirectoryRebindConfirmationRepository).GetRuntimeConfigDirectoryRebindConfirmation(ctx, confirmation.ID)
	if err != nil || confirmationAfter.Status != agentruntime.RuntimeConfigDirectoryRebindConfirmationConsumed {
		t.Fatalf("SQLite 重启后 confirmation 状态错误: %#v err=%v", confirmationAfter, err)
	}
}

func TestSQLiteRuntimeConfigDirectoryRebindApplyRejectsConfirmationReuseAndProjectionTamper(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	now := time.Date(2026, 9, 13, 18, 0, 0, 0, time.UTC)
	snapshotText, configDigest := sqliteRebindSnapshot(t)
	invocation := agentruntime.Invocation{ID: "inv-sqlite-rebind-apply-tamper", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, ConfigSnapshot: snapshotText, ConfigSnapshotDigest: configDigest, CreatedAt: now, UpdatedAt: now}
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo := store.RuntimeRepository()
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, agentruntime.AgentEvent{ID: "sqlite-rebind-apply-tamper-event", InvocationID: invocation.ID, Type: agentruntime.EventInvocationWaiting, Timestamp: now}); err != nil {
		t.Fatal(err)
	}
	snapshot := sqliteCheckpointSnapshot(invocation.ID, 1, now)
	if _, err := repo.(agentruntime.RuntimeSnapshotRepository).SaveRuntimeSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	plan, err := agentruntime.NewRuntimeConfigDirectoryRebindPlan("runtime-source", invocation, []string{"runtime-target"}, []agentruntime.RuntimeConfigDirectoryRebindCapability{sqliteRebindCapability("runtime-target", 1)}, "", configDigest, "tamper-plan", now)
	if err != nil {
		t.Fatal(err)
	}
	planRepo := repo.(agentruntime.RuntimeConfigDirectoryRebindPlanRepository)
	if _, err := planRepo.EnqueueRuntimeConfigDirectoryRebindPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	confirmationRepo := repo.(agentruntime.RuntimeConfigDirectoryRebindConfirmationRepository)
	confirmation, err := confirmationRepo.CreateRuntimeConfigDirectoryRebindConfirmation(ctx, plan, "tamper-confirm", now)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := newSQLiteRebindCoordinator(t, store)
	apply, err := coordinator.ApplyRuntimeConfigDirectoryRebind(ctx, plan.ID, confirmation.ID, "tamper-apply", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.ApplyRuntimeConfigDirectoryRebind(ctx, plan.ID, confirmation.ID, "tamper-apply-2", now); !errors.Is(err, agentruntime.ErrRuntimeConfigDirectoryRebindConfirmationConflict) {
		t.Fatalf("confirmation 重用必须拒绝: %v", err)
	}
	tampered := apply.Projection
	tampered.Budget.ToolCallsUsed++
	envelope, err := apply.Envelope(now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.(agentruntime.RuntimeConfigDirectoryRebindInbox).AcceptRuntimeConfigDirectoryRebind(ctx, envelope, tampered); !errors.Is(err, agentruntime.ErrRuntimeConfigDirectoryRebindApplyAuth) {
		t.Fatalf("projection tamper 必须被 SQLite inbox 拒绝: %v", err)
	}
}
