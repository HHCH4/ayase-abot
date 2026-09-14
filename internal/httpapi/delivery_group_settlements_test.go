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

func TestRuntimeDeliveryGroupSettlementsAPIIsBoundedAndMetadataOnly(t *testing.T) {
	ctx := context.Background()
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
	now := time.Now().UTC().Truncate(time.Millisecond)
	invocation := agentruntime.Invocation{ID: "inv-group-settlement-api", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationCompleted, CreatedAt: now, UpdatedAt: now}
	if err := store.RuntimeRepository().CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	group, err := agentruntime.NewRuntimeDeliveryGroup("runtime-a", "runtime-b", invocation.ID, []agentruntime.RuntimeDeliveryGroupMember{{Kind: agentruntime.RuntimeDeliveryKindEvent, OutboxID: "event-settlement-api", DeliveryID: "event-settlement-api"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	group.Members[0].Status = agentruntime.RuntimeDeliveryGroupMemberCompleted
	group.Status = agentruntime.RuntimeDeliveryGroupCompleted
	group.CompletedAt = &now
	groupRepo := store.RuntimeRepository().(agentruntime.RuntimeDeliveryGroupRepository)
	if _, err := groupRepo.EnqueueRuntimeDeliveryGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	settlement, err := agentruntime.NewRuntimeDeliveryGroupSettlement(group, now)
	if err != nil {
		t.Fatal(err)
	}
	settlementRepo := store.RuntimeRepository().(agentruntime.RuntimeDeliveryGroupSettlementRepository)
	if _, err := settlementRepo.EnqueueRuntimeDeliveryGroupSettlement(ctx, settlement); err != nil {
		t.Fatal(err)
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, store.RuntimeRepository())
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	server := NewServer(nil, nil)
	server.SetRuntimeCoordinator(coordinator)
	list := httptest.NewRecorder()
	server.Handler().ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/delivery-group-settlements?status=queued&limit=1", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("settlement list status=%d body=%s", list.Code, list.Body.String())
	}
	var items []agentruntime.RuntimeDeliveryGroupSettlement
	if err := json.Unmarshal(list.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != settlement.ID || items[0].LeaseOwner != "" {
		t.Fatalf("settlement list 返回错误/暴露 lease: %#v", items)
	}
	bad := httptest.NewRecorder()
	server.Handler().ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/delivery-group-settlements?status=unknown", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("未知 settlement status 应返回 400: status=%d body=%s", bad.Code, bad.Body.String())
	}
}

func TestRuntimeDeliveryGroupSagaAPIIsBoundedAndMetadataOnly(t *testing.T) {
	ctx := context.Background()
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
	now := time.Now().UTC().Truncate(time.Millisecond)
	invocation := agentruntime.Invocation{ID: "inv-group-saga-api", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationCompleted, CreatedAt: now, UpdatedAt: now}
	if err := store.RuntimeRepository().CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	group, err := agentruntime.NewRuntimeDeliveryGroup("runtime-a", "runtime-b", invocation.ID, []agentruntime.RuntimeDeliveryGroupMember{
		{Kind: agentruntime.RuntimeDeliveryKindEvent, OutboxID: "event-saga-api", DeliveryID: "event-saga-api", Status: agentruntime.RuntimeDeliveryGroupMemberCompleted},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	group.Status = agentruntime.RuntimeDeliveryGroupCompleted
	group.CompletedAt = &now
	groupRepo := store.RuntimeRepository().(agentruntime.RuntimeDeliveryGroupRepository)
	if _, err := groupRepo.EnqueueRuntimeDeliveryGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	saga, err := agentruntime.NewRuntimeDeliveryGroupSaga(group, now)
	if err != nil {
		t.Fatal(err)
	}
	sagaRepo := store.RuntimeRepository().(agentruntime.RuntimeDeliveryGroupSagaRepository)
	if _, err := sagaRepo.EnqueueRuntimeDeliveryGroupSaga(ctx, saga); err != nil {
		t.Fatal(err)
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, store.RuntimeRepository())
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	server := NewServer(nil, nil)
	server.SetRuntimeCoordinator(coordinator)
	list := httptest.NewRecorder()
	server.Handler().ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/delivery-group-sagas?state=decided&limit=1", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("saga list status=%d body=%s", list.Code, list.Body.String())
	}
	var items []agentruntime.RuntimeDeliveryGroupSaga
	if err := json.Unmarshal(list.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != saga.ID || items[0].State != agentruntime.RuntimeDeliveryGroupSagaDecided {
		t.Fatalf("saga list 返回错误: %#v", items)
	}
	encoded := list.Body.String()
	if strings.Contains(encoded, "lease") || strings.Contains(encoded, "secret") {
		t.Fatalf("saga API 不应暴露 lease/secret metadata: %s", encoded)
	}
	bad := httptest.NewRecorder()
	server.Handler().ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/delivery-group-sagas?state=unknown", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("未知 saga state 应返回 400: status=%d body=%s", bad.Code, bad.Body.String())
	}
}
