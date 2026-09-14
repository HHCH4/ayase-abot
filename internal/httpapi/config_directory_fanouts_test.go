package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/config"
	"Abot/internal/provider"
	"Abot/internal/storage/sqlite"
	"google.golang.org/adk/v2/session"
)

func TestRuntimeConfigDirectoryFanoutAPIIsMetadataOnlyAndReconciles(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repo := store.RuntimeRepository()
	fanoutRepo := repo.(agentruntime.RuntimeConfigDirectoryFanoutRepository)
	outboxRepo := repo.(agentruntime.RuntimeConfigDirectoryOutboxRepository)
	registry, err := provider.NewRegistry(ctx, store.ProviderRepository(), map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{SessionService: session.InMemoryService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	entry, err := agentruntime.NewRuntimeConfigDirectoryProfileEntry(config.Profile{ID: "profile-fanout-api", Name: "fanout-api", Revision: 1, Values: config.Values{"safe": "value"}})
	if err != nil {
		t.Fatal(err)
	}
	plan, children, err := agentruntime.NewRuntimeConfigDirectoryFanoutPlan("runtime-a", []string{"runtime-c", "runtime-b"}, entry, "", "api-correlation", "api-idempotency", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fanoutRepo.EnqueueRuntimeConfigDirectoryFanout(ctx, plan, children); err != nil {
		t.Fatal(err)
	}
	server := NewServer(nil, nil)
	server.SetRuntimeCoordinator(coordinator)
	handler := server.Handler()
	list := callHTTP(handler, http.MethodGet, "/api/v1/runtime/config-directory/fanouts?source=runtime-a&limit=10", "")
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), "safe") {
		t.Fatalf("fanout list 应只返回 metadata: status=%d body=%s", list.Code, list.Body.String())
	}
	var listed []agentruntime.RuntimeConfigDirectoryFanoutPlan
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil || len(listed) != 1 || listed[0].ID != plan.ID || listed[0].PendingCount != 2 {
		t.Fatalf("fanout list 内容错误: %#v err=%v", listed, err)
	}
	invalid := callHTTP(handler, http.MethodGet, "/api/v1/runtime/config-directory/fanouts?status=invalid", "")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("非法 fanout status 应 400，实际=%d body=%s", invalid.Code, invalid.Body.String())
	}
	claimed, ok, err := outboxRepo.ClaimRuntimeConfigDirectoryOutbox(ctx, "api-worker", time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("fanout child claim 失败: %#v ok=%v err=%v", claimed, ok, err)
	}
	if _, err := outboxRepo.CompleteRuntimeConfigDirectoryOutbox(ctx, claimed.ID, "api-worker", time.Now().UTC().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	detail := callHTTP(handler, http.MethodGet, "/api/v1/runtime/config-directory/fanouts/"+plan.ID, "")
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"status":"partial"`) || !strings.Contains(detail.Body.String(), `"completed_count":1`) {
		t.Fatalf("fanout detail 未按 child 状态对账: status=%d body=%s", detail.Code, detail.Body.String())
	}
}
