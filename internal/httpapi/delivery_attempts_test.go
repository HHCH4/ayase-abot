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

func TestRuntimeDeliveryAttemptsAPIIsBoundedAndMetadataOnly(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 23, 50, 0, 0, time.UTC)
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
	invocation := agentruntime.Invocation{ID: "inv-attempt-api", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	attempt, err := agentruntime.NewRuntimeDeliveryAttempt(agentruntime.RuntimeDeliveryKindConfig, "runtime-a", "runtime-b", "api-delivery", invocation.ID, "api-outbox", 1, 1, "api-worker", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	attemptRepo := repo.(agentruntime.RuntimeDeliveryAttemptRepository)
	if _, err := attemptRepo.BeginRuntimeDeliveryAttempt(ctx, attempt, now, time.Minute); err != nil {
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
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/delivery-attempts?kind=config&limit=1", nil)
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("attempt API status=%d body=%s", response.Code, response.Body.String())
	}
	var items []agentruntime.RuntimeDeliveryAttempt
	if err := json.Unmarshal(response.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != attempt.ID || items[0].Phase != agentruntime.RuntimeDeliveryAttemptStarted {
		t.Fatalf("attempt API 返回错误: %#v", items)
	}
	body := response.Body.String()
	if strings.Contains(body, "api-worker") || strings.Contains(body, "secret") {
		t.Fatalf("attempt API 不应暴露 lease owner 或 secret: %s", body)
	}
	bad := httptest.NewRecorder()
	badRequest := httptest.NewRequest(http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/delivery-attempts?kind=unknown", nil)
	handler.ServeHTTP(bad, badRequest)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("未知 kind 应返回 400: status=%d body=%s", bad.Code, bad.Body.String())
	}
}
