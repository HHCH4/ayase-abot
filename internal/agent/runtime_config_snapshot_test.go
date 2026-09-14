package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"Abot/internal/provider"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
)

func TestRuntimeConfigSnapshotIsBoundedAndMetadataOnly(t *testing.T) {
	secretInstruction := "仅供内部使用 api_key=sk-do-not-persist"
	secretURL := "https://user:password@example.invalid/v1"
	runtime := RuntimeOptions{
		AIEnabled: true, AITemperature: 0.7, AITopP: 1, AIMaxOutputTokens: 512, AIRequestRetries: 2,
		Instruction: secretInstruction, MessagePromptPrefix: "Bearer should-not-persist",
		CompactionEnabled: true, CompactionRatio: 0.8, CompactionSafetyTokens: 128, CompactionRetentionEvents: 5,
		CompactionUnknownWindowTokens: 4096, AgentMaxToolCalls: 10, ToolSchemaBudgetTokens: 2048,
		WorkspaceEnabled: true, WorkspaceReadEnabled: true, WorkspaceWriteEnabled: true, WorkspaceExecEnabled: true,
		WorkspaceGitEnabled: true, WorkspaceCommandTimeoutSecs: 60, MessageStreamingEnabled: true,
		MemoryEnabled: true, MemoryAutoRetrieve: true, MemoryMaxResults: 8,
	}
	resolved := provider.ResolvedModel{
		Provider: provider.Provider{ID: "demo", Name: "Demo", BaseURL: secretURL, APIKey: "sk-secret", Protocol: provider.ProtocolOpenAICompatible, OpenAIFormat: provider.OpenAIFormatResponses},
		Model:    provider.Model{ID: "model", Enabled: true, ContextWindow: 32768, MaxOutputTokens: 4096},
	}
	_, encoded, err := BuildRuntimeConfigSnapshot("abot", runtime, resolved, "workspace-1", "src/main.go", "catalog-1")
	if err != nil {
		t.Fatalf("生成配置快照失败: %v", err)
	}
	if len(encoded) > MaxRuntimeConfigSnapshotBytes {
		t.Fatalf("配置快照超出上限: %d", len(encoded))
	}
	for _, forbidden := range []string{secretInstruction, secretURL, "sk-secret", "Bearer should-not-persist"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("配置快照不应包含敏感或用户文本 %q: %s", forbidden, encoded)
		}
	}
	parsed, err := ParseRuntimeConfigSnapshot(encoded)
	if err != nil {
		t.Fatalf("配置快照解析失败: %v", err)
	}
	if parsed.ProviderID != "demo" || parsed.ModelID != "model" || parsed.ProviderOpenAIFormat != string(provider.OpenAIFormatResponses) || parsed.ToolCatalogRevision != "catalog-1" {
		t.Fatalf("配置快照核心 metadata 错误: %#v", parsed)
	}
	if got := RuntimeConfigSnapshotDigest(encoded); got == "" || got != RuntimeConfigSnapshotDigest(encoded+" ") {
		t.Fatalf("配置快照 digest 不稳定: %q", got)
	}
	changed := runtime
	changed.AITemperature = 0.2
	_, changedEncoded, err := BuildRuntimeConfigSnapshot("abot", changed, resolved, "workspace-1", "src/main.go", "catalog-1")
	if err != nil {
		t.Fatal(err)
	}
	changedSnapshot, err := ParseRuntimeConfigSnapshot(changedEncoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRuntimeConfigSnapshot(encoded, changedSnapshot); !errors.Is(err, ErrRuntimeConfigSnapshotMismatch) {
		t.Fatalf("配置变化应被拒绝: %v", err)
	}
}

func TestParseRuntimeConfigSnapshotRejectsUnknownTrailingAndOversizedJSON(t *testing.T) {
	base := RuntimeConfigSnapshot{
		Version: RuntimeConfigSnapshotVersion, AgentDefinitionVersion: "kernel-runtime-v2", AppName: "abot",
		ProviderID: "demo", ModelID: "model", ProviderProtocol: string(provider.ProtocolOpenAICompatible), PersonaID: "persona:default",
		InstructionDigest: runtimeSnapshotTextDigest("instruction"), Options: RuntimeOptionsSnapshot{CompactionRatio: 0.8},
	}
	encoded, err := jsonMarshalForSnapshot(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRuntimeConfigSnapshot(encoded + `{"unexpected":true}`); !errors.Is(err, ErrInvalidRuntimeConfigSnapshot) {
		t.Fatalf("尾部 JSON 应拒绝: %v", err)
	}
	if _, err := ParseRuntimeConfigSnapshot(strings.TrimSuffix(encoded, "}") + `,"unexpected":true}`); !errors.Is(err, ErrInvalidRuntimeConfigSnapshot) {
		t.Fatalf("未知字段应拒绝: %v", err)
	}
	if _, err := ParseRuntimeConfigSnapshot(strings.Repeat("x", MaxRuntimeConfigSnapshotBytes+1)); !errors.Is(err, ErrInvalidRuntimeConfigSnapshot) {
		t.Fatalf("超大快照应拒绝: %v", err)
	}
}

func TestKernelRejectsChangedRuntimeConfigSnapshotBeforeProvider(t *testing.T) {
	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "Demo", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", Enabled: true}}},
	}}
	adapter := &kernelTestAdapter{}
	registry, err := provider.NewRegistry(context.Background(), repo, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: adapter})
	if err != nil {
		t.Fatal(err)
	}
	currentInstruction := "initial instruction"
	var captured string
	kernel, err := NewKernel(Config{
		AppName: "config-snapshot-kernel", SessionService: session.InMemoryService(), Providers: registry,
		RuntimeConfigResolver: func(context.Context, string, string, string) (RuntimeOptions, error) {
			return RuntimeOptions{AIEnabled: true, ProviderID: "demo", ModelID: "model", Instruction: currentInstruction}, nil
		},
		ConfigSnapshotObserver: func(_ context.Context, _ string, encoded string) error { captured = encoded; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	first := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{UserID: "user", InvocationID: "inv-config", SessionID: "session-config", ProviderID: "demo", ModelID: "model", Message: "first"}))
	if first.err != nil || captured == "" {
		t.Fatalf("首次运行未捕获配置快照: err=%v snapshot=%q", first.err, captured)
	}
	if adapter.model == nil || adapter.model.calls != 1 {
		t.Fatalf("首次 provider 调用次数=%v", adapter.model)
	}
	currentInstruction = "changed instruction"
	second := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{UserID: "user", InvocationID: "inv-config", SessionID: "session-config", ProviderID: "demo", ModelID: "model", ConfigSnapshot: captured, Message: "resume"}))
	if !errors.Is(second.err, ErrRuntimeConfigSnapshotMismatch) {
		t.Fatalf("配置变化应在 provider 前拒绝: %v", second.err)
	}
	if adapter.model.calls != 1 {
		t.Fatalf("配置变化后不应再次调用 provider: %d", adapter.model.calls)
	}
}

func TestResolveRuntimeConfigSnapshotWarmsFreshToolRegistry(t *testing.T) {
	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "Demo", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", Enabled: true}}},
	}}
	newKernel := func(registry *ToolRegistry) *Kernel {
		kernel, err := NewKernel(Config{
			AppName: "config-snapshot-warm", SessionService: session.InMemoryService(), Providers: mustTestProviderRegistry(t, repo),
			Tools: []tool.Tool{registryTestTool{name: "echo", desc: "echo"}}, ToolRegistry: registry,
			RuntimeConfigResolver: func(context.Context, string, string, string) (RuntimeOptions, error) {
				return RuntimeOptions{AIEnabled: true, ProviderID: "demo", ModelID: "model", Instruction: "stable"}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return kernel
	}
	firstRegistry := NewToolRegistry()
	first := newKernel(firstRegistry)
	if result := collectKernelEvents(first.Run(context.Background(), ChatRequest{UserID: "user", InvocationID: "inv-warm", SessionID: "session-warm", ProviderID: "demo", ModelID: "model", Message: "first"})); result.err != nil {
		t.Fatal(result.err)
	}
	wantRevision := firstRegistry.CatalogRevision()
	if wantRevision == "" {
		t.Fatal("首次运行未建立工具 catalog revision")
	}
	second := newKernel(NewToolRegistry())
	_, encoded, err := second.ResolveRuntimeConfigSnapshot(context.Background(), ChatRequest{UserID: "user", SessionID: "session-warm", ProviderID: "demo", ModelID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := ParseRuntimeConfigSnapshot(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ToolCatalogRevision != wantRevision {
		t.Fatalf("恢复前应先 warm 工具 catalog: got=%q want=%q", snapshot.ToolCatalogRevision, wantRevision)
	}
}

func mustTestProviderRegistry(t *testing.T, repo *kernelTestRepository) *provider.Registry {
	t.Helper()
	registry, err := provider.NewRegistry(context.Background(), repo, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: &kernelTestAdapter{}})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

// jsonMarshalForSnapshot keeps this test independent of the production
// canonicalization helper while using the same encoding contract.
func jsonMarshalForSnapshot(snapshot RuntimeConfigSnapshot) (string, error) {
	encoded, err := json.Marshal(snapshot)
	return string(encoded), err
}
