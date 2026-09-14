package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/provider"
	"Abot/internal/storage/sqlite"
)

func TestRuntimeDeliveryCompensationsAPIIsBoundedAndMetadataOnly(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Second)
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
	invocation := agentruntime.Invocation{ID: "inv-compensation-api", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	item, err := agentruntime.NewRuntimeDeliveryCompensation(agentruntime.RuntimeDeliveryKindCheckpoint, "runtime-a", "runtime-b", "api-compensation-delivery", invocation.ID, "checkpoint-outbox", "group-api", 8, now)
	if err != nil {
		t.Fatal(err)
	}
	compRepo := repo.(agentruntime.RuntimeDeliveryCompensationRepository)
	if _, err := compRepo.EnqueueRuntimeDeliveryCompensation(ctx, item); err != nil {
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
	list := httptest.NewRecorder()
	handler.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/delivery-compensations?kind=checkpoint&status=queued&limit=1", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("compensation list status=%d body=%s", list.Code, list.Body.String())
	}
	if strings.Contains(list.Body.String(), "lease_owner") || strings.Contains(list.Body.String(), "lease_expires_at") {
		t.Fatalf("compensation API 暴露 lease 私有字段: %s", list.Body.String())
	}
	var items []agentruntime.RuntimeDeliveryCompensation
	if err := json.Unmarshal(list.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != item.ID || items[0].Status != agentruntime.RuntimeDeliveryCompensationQueued {
		t.Fatalf("compensation list 返回错误: %#v", items)
	}
	bad := httptest.NewRecorder()
	handler.ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/delivery-compensations?status=unknown", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("未知 compensation status 应返回 400: status=%d body=%s", bad.Code, bad.Body.String())
	}
	badKind := httptest.NewRecorder()
	handler.ServeHTTP(badKind, httptest.NewRequest(http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/delivery-compensations?kind=unknown", nil))
	if badKind.Code != http.StatusBadRequest {
		t.Fatalf("未知 compensation kind 应返回 400: status=%d body=%s", badKind.Code, badKind.Body.String())
	}
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/api/v1/invocations/missing-compensation-invocation/delivery-compensations", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("缺失 invocation 应返回 404: status=%d body=%s", missing.Code, missing.Body.String())
	}
}
