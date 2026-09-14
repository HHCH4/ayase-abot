package sqlite

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
)

func TestSQLiteInvocationPersistsRuntimeConfigSnapshotAndDigest(t *testing.T) {
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo := store.RuntimeRepository()
	now := time.Now().UTC().Truncate(time.Microsecond)
	resolved := agent.RuntimeConfigSnapshot{
		Version: agent.RuntimeConfigSnapshotVersion, AgentDefinitionVersion: "kernel-runtime-v2", AppName: "abot",
		ProviderID: "demo", ModelID: "model", ProviderProtocol: "openai-compatible", PersonaID: "persona:default",
		InstructionDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Options:           agent.RuntimeOptionsSnapshot{CompactionRatio: 0.8},
	}
	encodedBytes, err := json.Marshal(resolved)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(encodedBytes)
	want := agentruntime.Invocation{ID: "inv-config-snapshot-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "conversation", ProviderID: "demo", ModelID: "model", ConfigSnapshot: encoded, Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetInvocation(context.Background(), want.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConfigSnapshot != encoded || got.ConfigSnapshotDigest != agent.RuntimeConfigSnapshotDigest(encoded) {
		t.Fatalf("SQLite 配置快照往返错误: %#v", got)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	got, err = store.RuntimeRepository().GetInvocation(context.Background(), want.ID)
	if err != nil || got.ConfigSnapshot != encoded || got.ConfigSnapshotDigest != agent.RuntimeConfigSnapshotDigest(encoded) {
		t.Fatalf("SQLite 重启后配置快照错误: %#v err=%v", got, err)
	}
}
