package runtime

import (
	"context"
	"iter"
	"strings"
	"testing"
	"time"

	"Abot/internal/agent"
	"Abot/internal/provider"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// TestCoordinatorPersistsCompactionUsageAsSeparateEvent verifies the complete
// observation boundary: ADK's summarizer usage is emitted by Kernel, stored by
// Coordinator, and remains separate from the user-facing model usage in both
// Trace and the lightweight Usage projection.
func TestCoordinatorPersistsCompactionUsageAsSeparateEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		appName    = "runtime-compaction-usage-test"
		userID     = "runtime-compaction-user"
		sessionID  = "runtime-compaction-session"
		invocation = "runtime-compaction-invocation"
	)
	sessions := session.InMemoryService()
	created, err := sessions.Create(ctx, &session.CreateRequest{AppName: appName, UserID: userID, SessionID: sessionID})
	if err != nil {
		t.Fatalf("创建测试 Session 失败: %v", err)
	}
	// Keep enough prior history to force the ADK threshold path while leaving
	// the model context window large enough for the current request itself.
	oldUser := session.NewEvent(ctx, "old-invocation")
	oldUser.Author = "user"
	oldUser.LLMResponse.Content = genai.NewContentFromText(strings.Repeat("历史上下文 ", 1800), genai.RoleUser)
	if err := sessions.AppendEvent(ctx, created.Session, oldUser); err != nil {
		t.Fatalf("写入历史用户事件失败: %v", err)
	}
	oldAssistant := session.NewEvent(ctx, "old-invocation")
	oldAssistant.Author = "assistant"
	oldAssistant.LLMResponse.Content = genai.NewContentFromText("历史回复", genai.RoleModel)
	if err := sessions.AppendEvent(ctx, created.Session, oldAssistant); err != nil {
		t.Fatalf("写入历史助手事件失败: %v", err)
	}

	providerRepo := &runtimeProviderRepository{providers: map[string]provider.Provider{
		"demo": {
			ID: "demo", Name: "demo", BaseURL: "http://127.0.0.1:9999/v1",
			Protocol: provider.ProtocolOpenAICompatible,
			Models:   []provider.Model{{ID: "compaction-model", DisplayName: "compaction-model", Enabled: true, ContextWindow: 10000}},
		},
	}}
	registry, err := provider.NewRegistry(ctx, providerRepo, map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: &runtimeCompactionUsageAdapter{},
	})
	if err != nil {
		t.Fatalf("创建供应商 Registry 失败: %v", err)
	}
	kernel, err := agent.NewKernel(agent.Config{
		AppName: appName, SessionService: sessions, Providers: registry,
		EnableCompaction: true, CompactionRatio: 0.1, CompactionSafety: 1, CompactionRetention: 1,
	})
	if err != nil {
		t.Fatalf("创建 Kernel 失败: %v", err)
	}
	repo := NewMemoryRepository()
	coordinator, err := NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatalf("创建 Coordinator 失败: %v", err)
	}
	if err := coordinator.Start(ctx); err != nil {
		t.Fatalf("启动 Coordinator 失败: %v", err)
	}
	defer coordinator.Close()

	item, err := coordinator.StartInvocation(ctx, agent.ChatRequest{
		UserID: userID, ConversationID: sessionID, SessionID: sessionID,
		ProviderID: "demo", ModelID: "compaction-model", Message: "继续回答", Stream: true,
	})
	if err != nil {
		t.Fatalf("启动 Invocation 失败: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := coordinator.GetInvocation(ctx, item.ID)
		if getErr != nil {
			t.Fatalf("读取 Invocation 失败: %v", getErr)
		}
		if current.Status.Terminal() {
			if current.Status != InvocationCompleted {
				t.Fatalf("Invocation 未成功完成: %#v", current)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, err := coordinator.GetInvocation(ctx, item.ID)
	if err != nil || current.Status != InvocationCompleted {
		t.Fatalf("等待 Invocation 终态超时: %#v err=%v", current, err)
	}

	events, err := repo.ListEvents(ctx, item.ID, 0, 200)
	if err != nil {
		t.Fatalf("读取 Runtime 事件失败: %v", err)
	}
	var compactionEvents []AgentEvent
	var normalUsageEvents int
	for _, event := range events {
		if event.Type != EventUsageUpdated {
			continue
		}
		if usageEventScope(event) == "compaction" {
			compactionEvents = append(compactionEvents, event)
		} else {
			normalUsageEvents++
		}
	}
	if len(compactionEvents) != 1 {
		t.Fatalf("应持久化恰好一个压缩 usage 事件: %#v", compactionEvents)
	}
	compactionData := compactionEvents[0].Data
	if compactionData["scope"] != "compaction" {
		t.Fatalf("压缩 usage 事件 scope 错误: %#v", compactionData)
	}
	usage, ok := compactionData["usage"].(map[string]any)
	if !ok || usage["promptTokenCount"] != float64(30) || usage["totalTokenCount"] != float64(36) {
		t.Fatalf("压缩 usage 应保留规范化 metadata: %#v", compactionData)
	}
	if _, leaked := compactionData["content"]; leaked {
		t.Fatalf("压缩 usage 事件不应包含摘要正文: %#v", compactionData)
	}
	if normalUsageEvents == 0 {
		t.Fatalf("主模型 usage 事件缺失，无法验证两类调用隔离: %#v", events)
	}

	trace, err := coordinator.GetInvocationTrace(ctx, item.ID)
	if err != nil {
		t.Fatalf("读取 Invocation Trace 失败: %v", err)
	}
	if trace.Usage.ModelCalls != normalUsageEvents || trace.Usage.Compaction.ModelCalls != 1 || trace.Usage.Compaction.UnknownCalls != 0 {
		t.Fatalf("Trace usage 未隔离普通模型与压缩调用: normal_events=%d trace=%#v", normalUsageEvents, trace.Usage)
	}
	if trace.Usage.Compaction.PromptTokens.Value != 30 || !trace.Usage.Compaction.PromptTokens.Known || trace.Usage.Compaction.TotalTokens.Value != 36 || !trace.Usage.Compaction.TotalTokens.Known {
		t.Fatalf("Trace 压缩 token 统计错误: %#v", trace.Usage.Compaction)
	}
	if trace.Metrics.CompactionModelCalls != 1 || trace.Metrics.CompactionUnknownUsageCalls != 0 {
		t.Fatalf("Trace 压缩 metrics 错误: %#v", trace.Metrics)
	}
	usageProjection, err := coordinator.GetInvocationUsage(ctx, item.ID)
	if err != nil {
		t.Fatalf("读取 Invocation Usage 失败: %v", err)
	}
	if usageProjection.ModelCalls != trace.Usage.ModelCalls || usageProjection.Compaction.ModelCalls != trace.Usage.Compaction.ModelCalls {
		t.Fatalf("轻量 Usage projection 与 Trace 不一致: usage=%#v trace=%#v", usageProjection, trace.Usage)
	}
}

type runtimeCompactionUsageAdapter struct{}

func (a *runtimeCompactionUsageAdapter) BuildModel(_ context.Context, _ provider.Provider, modelID string) (model.LLM, error) {
	return &runtimeCompactionUsageModel{name: modelID}, nil
}

func (a *runtimeCompactionUsageAdapter) TestConnection(context.Context, provider.Provider) (provider.ProbeResult, error) {
	return provider.ProbeResult{Message: "连接正常"}, nil
}

func (a *runtimeCompactionUsageAdapter) DiscoverModels(context.Context, provider.Provider) ([]provider.Model, error) {
	return nil, nil
}

type runtimeCompactionUsageModel struct {
	name string
}

func (m *runtimeCompactionUsageModel) Name() string { return m.name }

func (m *runtimeCompactionUsageModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content: genai.NewContentFromText("测试回复", genai.RoleModel),
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount: 30, CandidatesTokenCount: 6, TotalTokenCount: 36,
			},
		}, nil)
	}
}
