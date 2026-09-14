package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/provider"
	"Abot/internal/storage/sqlite"
)

func TestRuntimeDeliveryGroupsAPIIsBoundedAndQueryable(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 14, 0, 0, 0, time.UTC)
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	registry, err := provider.NewRegistry(ctx, store.ProviderRepository(), map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{SessionService: store.SessionService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-group-api", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	group, err := agentruntime.NewRuntimeDeliveryGroup("runtime-a", "runtime-b", invocation.ID, []agentruntime.RuntimeDeliveryGroupMember{{Kind: agentruntime.RuntimeDeliveryKindEvent, OutboxID: "event-api", DeliveryID: "event-api"}, {Kind: agentruntime.RuntimeDeliveryKindConfig, OutboxID: "config-api", DeliveryID: "config-api"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	groupRepo := repo.(agentruntime.RuntimeDeliveryGroupRepository)
	if _, err := groupRepo.EnqueueRuntimeDeliveryGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	server := NewServer(nil, nil)
	server.SetRuntimeCoordinator(coordinator)
	handler := server.Handler()

	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/v1/runtime/delivery-groups/"+group.ID, nil))
	if get.Code != http.StatusOK {
		t.Fatalf("group GET status=%d body=%s", get.Code, get.Body.String())
	}
	var loaded agentruntime.RuntimeDeliveryGroup
	if err := json.Unmarshal(get.Body.Bytes(), &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.ID != group.ID || len(loaded.Members) != 2 || loaded.LeaseOwner != "" {
		t.Fatalf("group GET 返回错误/暴露私有 lease: %#v", loaded)
	}
	list := httptest.NewRecorder()
	handler.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/delivery-groups?status=queued&limit=1", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("group list status=%d body=%s", list.Code, list.Body.String())
	}
	var items []agentruntime.RuntimeDeliveryGroup
	if err := json.Unmarshal(list.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != group.ID {
		t.Fatalf("group list 返回错误: %#v", items)
	}
	bad := httptest.NewRecorder()
	handler.ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/delivery-groups?status=unknown", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("未知 group status 应返回 400: status=%d body=%s", bad.Code, bad.Body.String())
	}
}
