package httpapi

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/provider"
	"Abot/internal/storage/sqlite"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

type migrationHTTPAdapter struct{}

func (migrationHTTPAdapter) BuildModel(context.Context, provider.Provider, string) (adkmodel.LLM, error) {
	return migrationHTTPModel{}, nil
}

func (migrationHTTPAdapter) TestConnection(context.Context, provider.Provider) (provider.ProbeResult, error) {
	return provider.ProbeResult{Message: "ok"}, nil
}

func (migrationHTTPAdapter) DiscoverModels(context.Context, provider.Provider) ([]provider.Model, error) {
	return nil, nil
}

type migrationHTTPModel struct{}

func (migrationHTTPModel) Name() string { return "migration-http-model" }

func (migrationHTTPModel) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText("ok", genai.RoleModel)}, nil)
	}
}

func TestRuntimeConfigMigrationAPIUsesServerComputedSnapshotAndCAS(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.ProviderRepository().Save(ctx, provider.Provider{
		ID: "demo", Name: "Demo", BaseURL: "http://127.0.0.1:1", Protocol: provider.ProtocolOpenAICompatible,
		Models: []provider.Model{{ID: "model", DisplayName: "Model", Enabled: true}},
	}); err != nil {
		t.Fatal(err)
	}
	registry, err := provider.NewRegistry(ctx, store.ProviderRepository(), map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: migrationHTTPAdapter{}})
	if err != nil {
		t.Fatal(err)
	}
	instruction := "stable instruction"
	kernel, err := agent.NewKernel(agent.Config{
		AppName: "migration-http", SessionService: session.InMemoryService(), Providers: registry,
		RuntimeConfigResolver: func(context.Context, string, string, string) (agent.RuntimeOptions, error) {
			return agent.RuntimeOptions{AIEnabled: true, ProviderID: "demo", ModelID: "model", Instruction: instruction}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, oldEncoded, err := kernel.ResolveRuntimeConfigSnapshot(ctx, agent.ChatRequest{UserID: "user", ConversationID: "conversation", SessionID: "conversation", ProviderID: "demo", ModelID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	oldDigest := agent.RuntimeConfigSnapshotDigest(oldEncoded)
	now := time.Now().UTC()
	baseRepo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-config-migration-http", UserID: "user", ConversationID: "conversation", SessionID: "conversation", ProviderID: "demo", ModelID: "model", ConfigSnapshot: oldEncoded, Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := baseRepo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, baseRepo)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	server := NewServer(registry, nil)
	server.SetRuntimeCoordinator(coordinator)
	handler := server.Handler()
	instruction = "changed instruction"

	request := httptest.NewRequest(http.MethodPost, "/api/v1/invocations/"+invocation.ID+"/config-migration", strings.NewReader(`{"expected_config_snapshot_digest":"`+oldDigest+`"}`))
	request.Header.Set("Idempotency-Key", "http-migration-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("配置迁移 API 状态码错误: %d %s", response.Code, response.Body.String())
	}
	var migrated agentruntime.Invocation
	if err := json.Unmarshal(response.Body.Bytes(), &migrated); err != nil {
		t.Fatal(err)
	}
	if migrated.ConfigSnapshotDigest == oldDigest || migrated.ConfigSnapshotDigest == "" || strings.Contains(response.Body.String(), "changed instruction") {
		t.Fatalf("API 未返回安全的迁移结果: %#v body=%s", migrated, response.Body.String())
	}
	events, err := baseRepo.ListEvents(ctx, invocation.ID, 0, 20)
	if err != nil || len(events) != 1 || events[0].Type != agentruntime.EventRuntimeConfigMigrated {
		t.Fatalf("API 迁移事件异常: %#v err=%v", events, err)
	}

	// The same idempotency key can safely retry after the first commit.
	retry := httptest.NewRequest(http.MethodPost, "/api/v1/invocations/"+invocation.ID+"/config-migration", strings.NewReader(`{"expected_config_snapshot_digest":"`+oldDigest+`"}`))
	retry.Header.Set("Idempotency-Key", "http-migration-1")
	retryResponse := httptest.NewRecorder()
	handler.ServeHTTP(retryResponse, retry)
	if retryResponse.Code != http.StatusAccepted {
		t.Fatalf("API 重试应幂等成功: %d %s", retryResponse.Code, retryResponse.Body.String())
	}
	events, err = baseRepo.ListEvents(ctx, invocation.ID, 0, 20)
	if err != nil || len(events) != 1 {
		t.Fatalf("API 重试不应追加迁移事件: len=%d err=%v", len(events), err)
	}

	stale := httptest.NewRequest(http.MethodPost, "/api/v1/invocations/"+invocation.ID+"/config-migration", strings.NewReader(`{"expected_config_snapshot_digest":"`+oldDigest+`"}`))
	stale.Header.Set("Idempotency-Key", "different-migration")
	staleResponse := httptest.NewRecorder()
	handler.ServeHTTP(staleResponse, stale)
	if staleResponse.Code != http.StatusConflict {
		t.Fatalf("API 过期 digest 状态码应为 409: %d %s", staleResponse.Code, staleResponse.Body.String())
	}
}
