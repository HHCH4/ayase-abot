package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
)

func TestSQLiteRuntimeConfigMigrationIsAtomicCASIdempotentAndDurable(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	baseRepo := store.RuntimeRepository()
	migrationRepo, ok := baseRepo.(agentruntime.RuntimeConfigMigrationCommitRepository)
	if !ok {
		t.Fatal("SQLite Runtime 未实现配置迁移提交接口")
	}
	outboxRepo, ok := baseRepo.(agentruntime.RuntimeEventOutboxRepository)
	if !ok {
		t.Fatal("SQLite Runtime 未实现 event outbox 接口")
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	old := sqliteMigrationSnapshot(t, "old instruction", "demo", "model")
	newValue := sqliteMigrationSnapshot(t, "new instruction", "demo", "model")
	invocation := agentruntime.Invocation{ID: "inv-config-migration-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "conversation", ProviderID: "demo", ModelID: "model", ConfigSnapshot: old, Status: agentruntime.InvocationWaitingApproval, CreatedAt: now, UpdatedAt: now}
	if err := baseRepo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	oldDigest := agent.RuntimeConfigSnapshotDigest(old)
	newDigest := agent.RuntimeConfigSnapshotDigest(newValue)
	commit := agentruntime.RuntimeConfigMigrationCommit{
		InvocationID: invocation.ID, ExpectedDigest: oldDigest, NewSnapshot: newValue, IdempotencyKey: "sqlite-migration-1",
		Event: agentruntime.AgentEvent{ID: agentruntime.RuntimeConfigMigrationEventID(invocation.ID, oldDigest, newDigest, "sqlite-migration-1"), InvocationID: invocation.ID, Type: agentruntime.EventRuntimeConfigMigrated, Timestamp: now},
	}
	migrated, event, err := migrationRepo.CommitRuntimeConfigMigration(ctx, commit)
	if err != nil {
		t.Fatalf("SQLite 配置迁移失败: %v", err)
	}
	if migrated.ConfigSnapshotDigest != newDigest || event.Type != agentruntime.EventRuntimeConfigMigrated || event.Sequence != 1 {
		t.Fatalf("SQLite 迁移结果错误: invocation=%#v event=%#v", migrated, event)
	}
	outbox, err := outboxRepo.GetRuntimeEventOutbox(ctx, runtimeEventOutboxIDForMigrationTest(t, event.ID))
	if err != nil || outbox.EventID != event.ID || outbox.Status != agentruntime.RuntimeEventOutboxQueued {
		t.Fatalf("迁移事件 outbox 错误: %#v err=%v", outbox, err)
	}

	repeated, repeatedEvent, err := migrationRepo.CommitRuntimeConfigMigration(ctx, commit)
	if err != nil || repeated.ConfigSnapshotDigest != newDigest || repeatedEvent.ID != event.ID {
		t.Fatalf("SQLite 重复迁移应幂等: invocation=%#v event=%#v err=%v", repeated, repeatedEvent, err)
	}
	events, err := baseRepo.ListEvents(ctx, invocation.ID, 0, 20)
	if err != nil || len(events) != 1 {
		t.Fatalf("重复迁移不应追加事件: len=%d err=%v", len(events), err)
	}
	third := sqliteMigrationSnapshot(t, "third instruction", "demo", "model")
	thirdDigest := agent.RuntimeConfigSnapshotDigest(third)
	if _, _, err := migrationRepo.CommitRuntimeConfigMigration(ctx, agentruntime.RuntimeConfigMigrationCommit{
		InvocationID: invocation.ID, ExpectedDigest: oldDigest, NewSnapshot: third, IdempotencyKey: "stale",
		Event: agentruntime.AgentEvent{ID: agentruntime.RuntimeConfigMigrationEventID(invocation.ID, oldDigest, thirdDigest, "stale"), InvocationID: invocation.ID, Type: agentruntime.EventRuntimeConfigMigrated, Timestamp: now},
	}); !errors.Is(err, agentruntime.ErrConflict) {
		t.Fatalf("SQLite 过期 digest 应拒绝，实际=%v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	got, err := store.RuntimeRepository().GetInvocation(ctx, invocation.ID)
	if err != nil || got.ConfigSnapshotDigest != newDigest || got.ConfigSnapshot != newValue {
		t.Fatalf("SQLite 重启后迁移快照错误: %#v err=%v", got, err)
	}
}

func TestSQLiteRuntimeConfigMigrationRollsBackOnEventConflict(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	baseRepo := store.RuntimeRepository()
	migrationRepo, ok := baseRepo.(agentruntime.RuntimeConfigMigrationCommitRepository)
	if !ok {
		t.Fatal("SQLite Runtime 未实现配置迁移提交接口")
	}
	now := time.Now().UTC()
	old := sqliteMigrationSnapshot(t, "old", "demo", "model")
	newValue := sqliteMigrationSnapshot(t, "new", "demo", "model")
	invocation := agentruntime.Invocation{ID: "inv-config-migration-sqlite-rollback", UserID: "user", ConversationID: "conversation", SessionID: "conversation", ProviderID: "demo", ModelID: "model", ConfigSnapshot: old, Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := baseRepo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	oldDigest := agent.RuntimeConfigSnapshotDigest(old)
	newDigest := agent.RuntimeConfigSnapshotDigest(newValue)
	eventID := agentruntime.RuntimeConfigMigrationEventID(invocation.ID, oldDigest, newDigest, "rollback")
	if _, err := baseRepo.AppendEvent(ctx, agentruntime.AgentEvent{ID: eventID, InvocationID: invocation.ID, Type: agentruntime.EventRuntimeNotice, Timestamp: now}); err != nil {
		t.Fatal(err)
	}
	_, _, err = migrationRepo.CommitRuntimeConfigMigration(ctx, agentruntime.RuntimeConfigMigrationCommit{
		InvocationID: invocation.ID, ExpectedDigest: oldDigest, NewSnapshot: newValue, IdempotencyKey: "rollback",
		Event: agentruntime.AgentEvent{ID: eventID, InvocationID: invocation.ID, Type: agentruntime.EventRuntimeConfigMigrated, Timestamp: now},
	})
	if !errors.Is(err, agentruntime.ErrConflict) {
		t.Fatalf("SQLite 事件冲突应回滚，实际=%v", err)
	}
	got, err := baseRepo.GetInvocation(ctx, invocation.ID)
	if err != nil || got.ConfigSnapshotDigest != oldDigest || got.ConfigSnapshot != old {
		t.Fatalf("SQLite 事件冲突后快照被部分更新: %#v err=%v", got, err)
	}
	events, err := baseRepo.ListEvents(ctx, invocation.ID, 0, 20)
	if err != nil || len(events) != 1 || events[0].Type != agentruntime.EventRuntimeNotice {
		t.Fatalf("SQLite 事件冲突不应留下第二条事件: %#v err=%v", events, err)
	}
}

func sqliteMigrationSnapshot(t *testing.T, instruction, providerID, modelID string) string {
	t.Helper()
	hash := sha256.Sum256([]byte(instruction))
	snapshot := agent.RuntimeConfigSnapshot{
		Version: agent.RuntimeConfigSnapshotVersion, AgentDefinitionVersion: "kernel-runtime-v2", AppName: "abot",
		ProviderID: providerID, ModelID: modelID, ProviderProtocol: "openai-compatible", PersonaID: "persona:default",
		InstructionDigest: "sha256:" + hex.EncodeToString(hash[:]),
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func runtimeEventOutboxIDForMigrationTest(t *testing.T, eventID string) string {
	t.Helper()
	outbox, err := agentruntime.NewRuntimeEventOutbox(agentruntime.AgentEvent{ID: eventID, InvocationID: "invocation", Sequence: 1, Type: agentruntime.EventRuntimeConfigMigrated, Timestamp: time.Unix(0, 0).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	return outbox.ID
}
