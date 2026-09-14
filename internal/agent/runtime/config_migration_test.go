package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"Abot/internal/agent"
	"Abot/internal/provider"
	"google.golang.org/adk/v2/session"
)

func TestMigrateRuntimeConfigUsesDigestCASAndIsIdempotent(t *testing.T) {
	providerRepo := &runtimeProviderRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", BaseURL: "http://127.0.0.1:1", Protocol: provider.ProtocolOpenAICompatible,
			Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true}}},
	}}
	registry, err := provider.NewRegistry(context.Background(), providerRepo, map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: &runtimeApprovalAdapter{model: &runtimeApprovalModel{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	instruction := "stable instruction"
	kernel, err := agent.NewKernel(agent.Config{
		AppName: "config-migration", SessionService: session.InMemoryService(), Providers: registry,
		RuntimeConfigResolver: func(context.Context, string, string, string) (agent.RuntimeOptions, error) {
			return agent.RuntimeOptions{AIEnabled: true, ProviderID: "demo", ModelID: "model", Instruction: instruction}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, oldEncoded, err := kernel.ResolveRuntimeConfigSnapshot(context.Background(), agent.ChatRequest{UserID: "user", ConversationID: "conversation", SessionID: "conversation", ProviderID: "demo", ModelID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	oldDigest := agent.RuntimeConfigSnapshotDigest(oldEncoded)
	now := time.Now().UTC()
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-config-migration", UserID: "user", ConversationID: "conversation", SessionID: "conversation", ProviderID: "demo", ModelID: "model", ConfigSnapshot: oldEncoded, Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{kernel: kernel, repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	instruction = "changed instruction"
	migrated, err := coordinator.MigrateRuntimeConfig(context.Background(), invocation.ID, oldDigest, "migration-1")
	if err != nil {
		t.Fatalf("配置迁移失败: %v", err)
	}
	if migrated.ConfigSnapshotDigest == "" || migrated.ConfigSnapshotDigest == oldDigest || migrated.ConfigSnapshot == oldEncoded {
		t.Fatalf("迁移未替换配置快照: %#v", migrated)
	}
	if strings.Contains(migrated.ConfigSnapshot, instruction) || strings.Contains(migrated.ConfigSnapshot, "changed instruction") {
		t.Fatalf("配置快照不应包含指令正文: %s", migrated.ConfigSnapshot)
	}
	events, err := repo.ListEvents(context.Background(), invocation.ID, 0, 20)
	if err != nil || len(events) != 1 || events[0].Type != EventRuntimeConfigMigrated {
		t.Fatalf("迁移事件缺失或类型错误: %#v err=%v", events, err)
	}
	if events[0].Data["from_digest"] != oldDigest || events[0].Data["to_digest"] != migrated.ConfigSnapshotDigest || events[0].Data["idempotency_key"] != "migration-1" {
		t.Fatalf("迁移事件 metadata 错误: %#v", events[0].Data)
	}
	if _, err := repo.GetRuntimeEventOutbox(context.Background(), eventOutboxIDForTest(events[0].ID)); err != nil {
		t.Fatalf("迁移事件未登记 event outbox: %v", err)
	}

	// Reusing the same request after a committed migration is idempotent and
	// does not append a second audit event.
	repeated, err := coordinator.MigrateRuntimeConfig(context.Background(), invocation.ID, oldDigest, "migration-1")
	if err != nil {
		t.Fatalf("重复配置迁移应幂等成功: %v", err)
	}
	if repeated.ConfigSnapshotDigest != migrated.ConfigSnapshotDigest {
		t.Fatalf("重复迁移改变了目标 digest: %#v", repeated)
	}
	events, err = repo.ListEvents(context.Background(), invocation.ID, 0, 20)
	if err != nil || len(events) != 1 {
		t.Fatalf("重复迁移不应追加事件: len=%d err=%v", len(events), err)
	}

	if _, err := coordinator.MigrateRuntimeConfig(context.Background(), invocation.ID, "sha256:"+strings.Repeat("0", 64), "other"); !errors.Is(err, ErrConflict) {
		t.Fatalf("过期 digest 应被拒绝，实际=%v", err)
	}
	unchanged, err := repo.GetInvocation(context.Background(), invocation.ID)
	if err != nil || unchanged.ConfigSnapshotDigest != migrated.ConfigSnapshotDigest {
		t.Fatalf("过期请求不应改变快照: %#v err=%v", unchanged, err)
	}
}

func TestMemoryRuntimeConfigMigrationRollsBackOnEventIdentityConflict(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	old := migrationSnapshot(t, "old-instruction", "demo", "model")
	newValue := migrationSnapshot(t, "new-instruction", "demo", "model")
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-config-migration-rollback", UserID: "user", ConversationID: "conversation", SessionID: "conversation", ProviderID: "demo", ModelID: "model", ConfigSnapshot: old, Status: InvocationWaitingApproval, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	oldDigest := agent.RuntimeConfigSnapshotDigest(old)
	newDigest := agent.RuntimeConfigSnapshotDigest(newValue)
	eventID := RuntimeConfigMigrationEventID(invocation.ID, oldDigest, newDigest, "rollback")
	if _, err := repo.AppendEvent(ctx, AgentEvent{ID: eventID, InvocationID: invocation.ID, Type: EventRuntimeNotice, Timestamp: now}); err != nil {
		t.Fatal(err)
	}
	_, _, err := repo.CommitRuntimeConfigMigration(ctx, RuntimeConfigMigrationCommit{
		InvocationID: invocation.ID, ExpectedDigest: oldDigest, NewSnapshot: newValue, IdempotencyKey: "rollback",
		Event: AgentEvent{ID: eventID, InvocationID: invocation.ID, Type: EventRuntimeConfigMigrated, Timestamp: now},
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("事件身份冲突应回滚迁移，实际=%v", err)
	}
	got, err := repo.GetInvocation(ctx, invocation.ID)
	if err != nil || got.ConfigSnapshotDigest != oldDigest || got.ConfigSnapshot != old {
		t.Fatalf("事件冲突后配置快照被部分更新: %#v err=%v", got, err)
	}
}

func migrationSnapshot(t *testing.T, instruction, providerID, modelID string) string {
	t.Helper()
	snapshot := agent.RuntimeConfigSnapshot{
		Version: agent.RuntimeConfigSnapshotVersion, AgentDefinitionVersion: "kernel-runtime-v2", AppName: "abot",
		ProviderID: providerID, ModelID: modelID, ProviderProtocol: "openai-compatible", PersonaID: "persona:default",
		InstructionDigest: "sha256:" + strings.Repeat("1", 64),
	}
	digest := sha256.Sum256([]byte(instruction))
	snapshot.InstructionDigest = "sha256:" + hex.EncodeToString(digest[:])
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
