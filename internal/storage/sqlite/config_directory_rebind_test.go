package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/provider"
	"google.golang.org/adk/v2/session"
)

func sqliteRebindSnapshot(t *testing.T) (string, string) {
	t.Helper()
	instructionDigest := sha256.Sum256([]byte("sqlite-rebind-instruction"))
	snapshot := agent.RuntimeConfigSnapshot{
		Version:                agent.RuntimeConfigSnapshotVersion,
		AgentDefinitionVersion: "kernel-runtime-v2",
		AppName:                "sqlite-rebind",
		ProviderID:             "demo",
		ModelID:                "model",
		ProviderProtocol:       string(provider.ProtocolOpenAICompatible),
		PersonaID:              "persona:default",
		InstructionDigest:      "sha256:" + hex.EncodeToString(instructionDigest[:]),
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	digest := agent.RuntimeConfigSnapshotDigest(string(encoded))
	if digest == "" {
		t.Fatal("SQLite rebind 测试快照必须可解析")
	}
	return string(encoded), digest
}

func newSQLiteRebindCoordinator(t *testing.T, store *Store) *agentruntime.Coordinator {
	t.Helper()
	ctx := context.Background()
	registry, err := provider.NewRegistry(ctx, store.ProviderRepository(), map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{SessionService: session.InMemoryService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, store.RuntimeRepository())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	return coordinator
}

func sqliteRebindCapability(destination string, revision int64) agentruntime.RuntimeConfigDirectoryRebindCapability {
	return agentruntime.RuntimeConfigDirectoryRebindCapability{
		Destination: destination, Revision: revision,
		SupportsRebind: true, SupportsCheckpoint: true, SupportsResume: true, SupportsConfigMaterialization: true,
	}
}

func TestSQLiteRuntimeConfigDirectoryRebindPlanPersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	snapshot, digest := sqliteRebindSnapshot(t)
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	now := time.Now().UTC().Truncate(time.Microsecond)
	invocation := agentruntime.Invocation{ID: "inv-sqlite-rebind", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingApproval, ConfigSnapshot: snapshot, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	coordinator := newSQLiteRebindCoordinator(t, store)
	catalog := agentruntime.NewMemoryRuntimeConfigDirectoryRebindRouteCatalog()
	for _, destination := range []string{"runtime-a", "runtime-b"} {
		if err := catalog.Set("runtime-source", sqliteRebindCapability(destination, 7)); err != nil {
			t.Fatal(err)
		}
	}
	coordinator.SetRuntimeConfigDirectoryRebindRouteCatalog(catalog)
	plan, err := coordinator.CreateRuntimeConfigDirectoryRebindPlan(ctx, "runtime-source", invocation.ID, []string{"runtime-b", "runtime-a"}, digest, "", "sqlite-key", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	reloaded, err := store2.RuntimeRepository().(agentruntime.RuntimeConfigDirectoryRebindPlanRepository).GetRuntimeConfigDirectoryRebindPlan(ctx, plan.ID)
	if err != nil || reloaded.ID != plan.ID || reloaded.IdempotencyKey != "sqlite-key" {
		t.Fatalf("重启后 plan metadata 错误: %#v err=%v", reloaded, err)
	}
	if items, err := store2.RuntimeRepository().(agentruntime.RuntimeConfigDirectoryRebindPlanRepository).ListRuntimeConfigDirectoryRebindPlans(ctx, "runtime-source", invocation.ID, agentruntime.RuntimeConfigDirectoryRebindValidated, 10); err != nil || len(items) != 1 {
		t.Fatalf("重启后 plan list 错误: %#v err=%v", items, err)
	}
}

func TestSQLiteRuntimeConfigDirectoryRebindPlanConcurrentIdempotencyAndConflict(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repo := store.RuntimeRepository()
	snapshot, digest := sqliteRebindSnapshot(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := repo.CreateInvocation(ctx, agentruntime.Invocation{ID: "inv-sqlite-rebind-concurrent", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, ConfigSnapshot: snapshot, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	coordinator := newSQLiteRebindCoordinator(t, store)
	catalog := agentruntime.NewMemoryRuntimeConfigDirectoryRebindRouteCatalog()
	if err := catalog.Set("runtime-source", sqliteRebindCapability("runtime-a", 1)); err != nil {
		t.Fatal(err)
	}
	coordinator.SetRuntimeConfigDirectoryRebindRouteCatalog(catalog)
	var wait sync.WaitGroup
	for index := 0; index < 8; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := coordinator.CreateRuntimeConfigDirectoryRebindPlan(ctx, "runtime-source", "inv-sqlite-rebind-concurrent", []string{"runtime-a"}, digest, "", "same-key", now); err != nil {
				t.Errorf("并发 SQLite rebind 登记失败: %v", err)
			}
		}()
	}
	wait.Wait()
	plans, err := repo.(agentruntime.RuntimeConfigDirectoryRebindPlanRepository).ListRuntimeConfigDirectoryRebindPlans(ctx, "runtime-source", "inv-sqlite-rebind-concurrent", "", 20)
	if err != nil || len(plans) != 1 {
		t.Fatalf("并发登记必须只有一个 plan: %#v err=%v", plans, err)
	}
	if _, err := coordinator.CreateRuntimeConfigDirectoryRebindPlan(ctx, "runtime-source", "inv-sqlite-rebind-concurrent", []string{"runtime-a"}, digest, "", "different-key", now); err != nil {
		// A different idempotency key is a legitimate second plan.
		t.Fatal(err)
	}
	if _, err := coordinator.CreateRuntimeConfigDirectoryRebindPlan(ctx, "runtime-source", "inv-sqlite-rebind-concurrent", []string{"runtime-a"}, digest, "", "same-key", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	replacement := "0"
	if digest[len(digest)-1] == replacement[0] {
		replacement = "1"
	}
	missingDigest := digest[:len(digest)-1] + replacement
	if _, err := coordinator.CreateRuntimeConfigDirectoryRebindPlan(ctx, "runtime-source", "inv-sqlite-rebind-concurrent", []string{"runtime-a"}, missingDigest, "", "new-key", now); !errors.Is(err, agentruntime.ErrRuntimeConfigDirectoryRebindSnapshot) {
		t.Fatalf("旧快照 digest 必须拒绝: %v", err)
	}
}
