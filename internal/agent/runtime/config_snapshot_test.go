package runtime

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"Abot/internal/agent"
	"Abot/internal/provider"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

type configSnapshotAdapter struct{ model *configSnapshotModel }

func (a *configSnapshotAdapter) BuildModel(context.Context, provider.Provider, string) (adkmodel.LLM, error) {
	a.model = &configSnapshotModel{}
	return a.model, nil
}
func (*configSnapshotAdapter) TestConnection(context.Context, provider.Provider) (provider.ProbeResult, error) {
	return provider.ProbeResult{Message: "ok"}, nil
}
func (*configSnapshotAdapter) DiscoverModels(context.Context, provider.Provider) ([]provider.Model, error) {
	return nil, nil
}

type configSnapshotModel struct{ calls atomic.Int32 }

func (*configSnapshotModel) Name() string { return "config-snapshot-model" }
func (m *configSnapshotModel) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.calls.Add(1)
		yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText("done", genai.RoleModel)}, nil)
	}
}

func TestCoordinatorPersistsMetadataOnlyRuntimeConfigSnapshot(t *testing.T) {
	providerRepo := &runtimeProviderRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "Demo", BaseURL: "https://secret.invalid/v1", APIKey: "sk-secret", Protocol: provider.ProtocolOpenAICompatible,
			Models: []provider.Model{{ID: "model", Enabled: true}}},
	}}
	adapter := &configSnapshotAdapter{}
	registry, err := provider.NewRegistry(context.Background(), providerRepo, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: adapter})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{AppName: "runtime-config-snapshot", SessionService: session.InMemoryService(), Providers: registry, Instruction: "system secret api_key=sk-not-persist"})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewMemoryRepository()
	coordinator, err := NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := coordinator.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	invocation, err := coordinator.StartInvocation(ctx, agent.ChatRequest{UserID: "user", ConversationID: "config-snapshot-conversation", SessionID: "config-snapshot-conversation", ProviderID: "demo", ModelID: "model", Message: "run", Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var stored Invocation
	for time.Now().Before(deadline) {
		stored, err = repo.GetInvocation(ctx, invocation.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.ConfigSnapshot != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if stored.ConfigSnapshot == "" || stored.ConfigSnapshotDigest == "" {
		t.Fatalf("Invocation 未持久化配置快照: %#v", stored)
	}
	for _, forbidden := range []string{"sk-secret", "https://secret.invalid", "api_key=sk-not-persist"} {
		if containsConfigSnapshot(stored.ConfigSnapshot, forbidden) {
			t.Fatalf("配置快照泄露敏感文本 %q: %s", forbidden, stored.ConfigSnapshot)
		}
	}
	if got := agent.RuntimeConfigSnapshotDigest(stored.ConfigSnapshot); got != stored.ConfigSnapshotDigest {
		t.Fatalf("配置快照 digest 不一致: got=%q stored=%q", got, stored.ConfigSnapshotDigest)
	}
	var encodedInvocation []byte
	if encodedInvocation, err = json.Marshal(stored); err != nil {
		t.Fatal(err)
	}
	if containsConfigSnapshot(string(encodedInvocation), `"config_snapshot"`) {
		t.Fatalf("Invocation API 不应暴露原始配置快照: %s", encodedInvocation)
	}
	if !containsConfigSnapshot(string(encodedInvocation), stored.ConfigSnapshotDigest) {
		t.Fatalf("Invocation API 应暴露配置快照 digest: %s", encodedInvocation)
	}
}

func containsConfigSnapshot(value, fragment string) bool {
	return len(fragment) > 0 && strings.Contains(value, fragment)
}
