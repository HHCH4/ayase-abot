package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"Abot/internal/agent"
	"Abot/internal/provider"
	"google.golang.org/adk/v2/session"
)

func rebindTestSnapshot(t *testing.T, appName string) (string, string) {
	t.Helper()
	instructionDigest := sha256.Sum256([]byte("rebind-test-instruction"))
	snapshot := agent.RuntimeConfigSnapshot{
		Version:                RuntimeConfigSnapshotVersionForTest(),
		AgentDefinitionVersion: "kernel-runtime-v2",
		AppName:                appName,
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
		parsed, parseErr := agent.ParseRuntimeConfigSnapshot(string(encoded))
		t.Fatalf("测试快照必须可解析: encoded=%s parsed=%#v err=%v", encoded, parsed, parseErr)
	}
	return string(encoded), digest
}

// RuntimeConfigSnapshotVersionForTest keeps the test helper independent of
// runtime's private validation details while still producing the current wire
// version. The value is intentionally checked against the public package.
func RuntimeConfigSnapshotVersionForTest() int { return agent.RuntimeConfigSnapshotVersion }

func newRebindTestCoordinator(t *testing.T, repo Repository) *Coordinator {
	t.Helper()
	ctx := context.Background()
	registry, err := provider.NewRegistry(ctx, &runtimeProviderRepository{providers: map[string]provider.Provider{}}, map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{SessionService: session.InMemoryService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	return coordinator
}

func rebindCapability(destination string, revision int64) RuntimeConfigDirectoryRebindCapability {
	return RuntimeConfigDirectoryRebindCapability{
		Destination: destination, Revision: revision,
		SupportsRebind: true, SupportsCheckpoint: true, SupportsResume: true, SupportsConfigMaterialization: true,
	}
}

func TestRuntimeConfigDirectoryRebindPlanValidatesBoundarySnapshotAndCapabilities(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	snapshot, digest := rebindTestSnapshot(t, "rebind-test")
	invocation := Invocation{ID: " invocation-rebind ", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingApproval, ConfigSnapshot: snapshot, ConfigSnapshotDigest: digest, CreatedAt: now, UpdatedAt: now}
	capabilities := []RuntimeConfigDirectoryRebindCapability{rebindCapability("runtime-b", 2), rebindCapability("runtime-a", 1)}
	plan, err := NewRuntimeConfigDirectoryRebindPlan("runtime-source", invocation, []string{"runtime-b", "runtime-a"}, capabilities, "", digest, "rebind-key", now)
	if err != nil {
		t.Fatal(err)
	}
	if plan.InvocationID != "invocation-rebind" || plan.BoundaryStatus != InvocationWaitingApproval || plan.Status != RuntimeConfigDirectoryRebindValidated {
		t.Fatalf("rebind 计划基本字段错误: %#v", plan)
	}
	if !equalRuntimeConfigDirectoryStrings(plan.Destinations, []string{"runtime-a", "runtime-b"}) || plan.Targets[0].Destination != "runtime-a" {
		t.Fatalf("rebind route 未稳定排序: %#v", plan)
	}
	reordered, err := NewRuntimeConfigDirectoryRebindPlan("runtime-source", invocation, []string{"runtime-a", "runtime-b"}, []RuntimeConfigDirectoryRebindCapability{rebindCapability("runtime-a", 1), rebindCapability("runtime-b", 2)}, "", digest, "rebind-key", now)
	if err != nil || reordered.ID != plan.ID {
		t.Fatalf("目的端顺序不应改变稳定 ID: first=%#v second=%#v err=%v", plan, reordered, err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"rebind-key", "message", "attachments", "secret"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("rebind API 计划不得泄露 %q: %s", forbidden, encoded)
		}
	}

	tampered := invocation
	tampered.ConfigSnapshot = strings.Replace(snapshot, "rebind-test", "tampered", 1)
	if _, err := NewRuntimeConfigDirectoryRebindPlan("runtime-source", tampered, []string{"runtime-a"}, []RuntimeConfigDirectoryRebindCapability{rebindCapability("runtime-a", 1)}, "", digest, "", now); !errors.Is(err, ErrRuntimeConfigDirectoryRebindSnapshot) {
		t.Fatalf("隐藏快照被篡改时必须拒绝: %v", err)
	}
	for _, status := range []InvocationStatus{InvocationQueued, InvocationRunning, InvocationCompleted} {
		invalid := invocation
		invalid.Status = status
		if _, err := NewRuntimeConfigDirectoryRebindPlan("runtime-source", invalid, []string{"runtime-a"}, []RuntimeConfigDirectoryRebindCapability{rebindCapability("runtime-a", 1)}, "", digest, "", now); !errors.Is(err, ErrRuntimeConfigDirectoryRebindNotEligible) {
			t.Fatalf("status=%s 不应允许 rebind: %v", status, err)
		}
	}
	leased := invocation
	owner := "worker"
	leased.LeaseOwner = owner
	if _, err := NewRuntimeConfigDirectoryRebindPlan("runtime-source", leased, []string{"runtime-a"}, []RuntimeConfigDirectoryRebindCapability{rebindCapability("runtime-a", 1)}, "", digest, "", now); !errors.Is(err, ErrRuntimeConfigDirectoryRebindNotEligible) {
		t.Fatalf("仍持有租约时必须拒绝: %v", err)
	}
	if _, err := NewRuntimeConfigDirectoryRebindPlan("runtime-source", invocation, []string{"runtime-a"}, []RuntimeConfigDirectoryRebindCapability{{Destination: "runtime-a", Revision: 1, SupportsCheckpoint: true, SupportsResume: true}}, "", digest, "", now); !errors.Is(err, ErrRuntimeConfigDirectoryRebindCapability) {
		t.Fatalf("缺少 supports_rebind 时必须拒绝: %v", err)
	}
	if _, err := NewRuntimeConfigDirectoryRebindPlan("runtime-source", invocation, []string{"runtime-a", "runtime-a"}, []RuntimeConfigDirectoryRebindCapability{rebindCapability("runtime-a", 1), rebindCapability("runtime-a", 2)}, "", digest, "", now); !errors.Is(err, ErrInvalidRuntimeConfigDirectoryRebind) {
		t.Fatalf("重复 destination 必须拒绝: %v", err)
	}
}

func TestMemoryRuntimeConfigDirectoryRebindPlanIsDurableIdempotentAndDefensive(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	snapshot, digest := rebindTestSnapshot(t, "rebind-memory")
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-rebind-memory", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, ConfigSnapshot: snapshot, Message: "private prompt", CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	coordinator := newRebindTestCoordinator(t, repo)
	catalog := NewMemoryRuntimeConfigDirectoryRebindRouteCatalog()
	for _, destination := range []string{"runtime-a", "runtime-b"} {
		if err := catalog.Set("runtime-source", rebindCapability(destination, 3)); err != nil {
			t.Fatal(err)
		}
	}
	coordinator.SetRuntimeConfigDirectoryRebindRouteCatalog(catalog)
	plan, err := coordinator.CreateRuntimeConfigDirectoryRebindPlan(ctx, "runtime-source", invocation.ID, []string{"runtime-b", "runtime-a"}, digest, "", "memory-key", now)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != RuntimeConfigDirectoryRebindValidated || plan.Revision != 1 {
		t.Fatalf("创建 rebind 计划失败: %#v", plan)
	}
	repeated, err := coordinator.CreateRuntimeConfigDirectoryRebindPlan(ctx, "runtime-source", invocation.ID, []string{"runtime-a", "runtime-b"}, digest, "", "memory-key", now.Add(time.Second))
	if err != nil || repeated.ID != plan.ID {
		t.Fatalf("相同 idempotency key 必须幂等: %#v err=%v", repeated, err)
	}
	conflict, err := coordinator.CreateRuntimeConfigDirectoryRebindPlan(ctx, "runtime-source", invocation.ID, []string{"runtime-a"}, digest, "", "memory-key", now)
	if !errors.Is(err, ErrRuntimeConfigDirectoryRebindConflict) || conflict.ID != "" {
		t.Fatalf("同 key 不同 route 必须冲突: plan=%#v err=%v", conflict, err)
	}
	listed, err := coordinator.ListRuntimeConfigDirectoryRebindPlans(ctx, "runtime-source", invocation.ID, "", 10)
	if err != nil || len(listed) != 1 {
		t.Fatalf("rebind list 错误: %#v err=%v", listed, err)
	}
	listed[0].Destinations[0] = "mutated"
	got, err := coordinator.GetRuntimeConfigDirectoryRebindPlan(ctx, plan.ID)
	if err != nil || got.Destinations[0] != "runtime-a" {
		t.Fatalf("计划必须返回防御性 route 副本: %#v err=%v", got, err)
	}

	var wait sync.WaitGroup
	for index := 0; index < 8; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := coordinator.CreateRuntimeConfigDirectoryRebindPlan(ctx, "runtime-source", invocation.ID, []string{"runtime-b", "runtime-a"}, digest, "", "concurrent-key", now); err != nil {
				t.Errorf("并发 rebind 幂等登记失败: %v", err)
			}
		}()
	}
	wait.Wait()
	all, err := coordinator.ListRuntimeConfigDirectoryRebindPlans(ctx, "runtime-source", invocation.ID, "", 20)
	if err != nil || len(all) != 2 {
		t.Fatalf("并发登记应只有两个逻辑 key: %#v err=%v", all, err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private prompt") || strings.Contains(string(encoded), "memory-key") {
		t.Fatalf("rebind 计划响应泄露 Invocation 正文或幂等键: %s", encoded)
	}
}

func TestCoordinatorRuntimeConfigDirectoryRebindRequiresCatalogAndLinkedFanout(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	snapshot, digest := rebindTestSnapshot(t, "rebind-fanout")
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-rebind-fanout", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingUser, ConfigSnapshot: snapshot, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	coordinator := newRebindTestCoordinator(t, repo)
	if _, err := coordinator.CreateRuntimeConfigDirectoryRebindPlan(ctx, "runtime-source", invocation.ID, []string{"runtime-a"}, digest, "", "", now); !errors.Is(err, ErrRuntimeConfigDirectoryRebindUnavailable) {
		t.Fatalf("未装配 route catalog 时必须 fail-closed: %v", err)
	}
	catalog := NewMemoryRuntimeConfigDirectoryRebindRouteCatalog()
	if err := catalog.Set("runtime-source", rebindCapability("runtime-a", 1)); err != nil {
		t.Fatal(err)
	}
	coordinator.SetRuntimeConfigDirectoryRebindRouteCatalog(catalog)
	entry := configDirectoryProfile(t, "rebind-linked", 1, "linked")
	fanout, children, err := NewRuntimeConfigDirectoryFanoutPlan("runtime-source", []string{"runtime-a"}, entry, "", "corr", "fanout-key", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeConfigDirectoryFanout(ctx, fanout, children); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.CreateRuntimeConfigDirectoryRebindPlan(ctx, "runtime-source", invocation.ID, []string{"runtime-a"}, digest, fanout.ID, "", now); !errors.Is(err, ErrRuntimeConfigDirectoryRebindConflict) {
		t.Fatalf("未完成 config fanout 时必须拒绝 rebind: %v", err)
	}
	claimed, ok, err := repo.ClaimRuntimeConfigDirectoryOutbox(ctx, "worker", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim linked fanout child 失败: %#v ok=%v err=%v", claimed, ok, err)
	}
	if _, err := repo.CompleteRuntimeConfigDirectoryOutbox(ctx, claimed.ID, "worker", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReconcileRuntimeConfigDirectoryFanout(ctx, fanout.ID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	plan, err := coordinator.CreateRuntimeConfigDirectoryRebindPlan(ctx, "runtime-source", invocation.ID, []string{"runtime-a"}, digest, fanout.ID, "linked-key", now.Add(2*time.Second))
	if err != nil || plan.ConfigFanoutID != fanout.ID {
		t.Fatalf("完成 config fanout 后应允许建立 linked rebind: %#v err=%v", plan, err)
	}
}
