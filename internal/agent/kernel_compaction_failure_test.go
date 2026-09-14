package agent

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"

	"Abot/internal/provider"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// rejectingCompactionSessionService makes the ADK tail-retention append path
// fail while leaving normal user/model events available to the Kernel. This
// exercises the SessionService boundary where ADK deliberately degrades and
// continues the current reply.
type rejectingCompactionSessionService struct {
	session.Service
	mu          sync.Mutex
	compactions int
}

func (s *rejectingCompactionSessionService) AppendEvent(ctx context.Context, sess session.Session, event *session.Event) error {
	if event != nil && event.Actions.Compaction != nil {
		s.mu.Lock()
		s.compactions++
		s.mu.Unlock()
		return errors.New("storage unavailable api_key=secret-value")
	}
	return s.Service.AppendEvent(ctx, sess, event)
}

func (s *rejectingCompactionSessionService) countCompactions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.compactions
}

func TestKernelObservesTailCompactionAppendFailureWithoutDroppingReply(t *testing.T) {
	base := session.InMemoryService()
	service := &rejectingCompactionSessionService{Service: base}
	const appName = "compaction-observer-test"
	const userID = "user-compaction"
	const sessionID = "session-compaction"
	ctx := context.Background()
	created, err := base.Create(ctx, &session.CreateRequest{AppName: appName, UserID: userID, SessionID: sessionID})
	if err != nil {
		t.Fatalf("创建测试 Session 失败: %v", err)
	}
	// A large prior exchange makes the next tail-retention pass deterministic,
	// while the model's context window remains comfortably above the request
	// so the context-budget guard does not mask the compaction path.
	oldUser := session.NewEvent(ctx, "old-invocation")
	oldUser.Author = "user"
	oldUser.LLMResponse.Content = genaiContent(strings.Repeat("历史上下文 ", 1800), "user")
	if err := base.AppendEvent(ctx, created.Session, oldUser); err != nil {
		t.Fatalf("写入历史用户事件失败: %v", err)
	}
	oldAssistant := session.NewEvent(ctx, "old-invocation")
	oldAssistant.Author = "assistant"
	oldAssistant.LLMResponse.Content = genaiContent("历史回复", "model")
	if err := base.AppendEvent(ctx, created.Session, oldAssistant); err != nil {
		t.Fatalf("写入历史助手事件失败: %v", err)
	}

	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "演示供应商", BaseURL: "http://127.0.0.1:9999/v1", Protocol: provider.ProtocolOpenAICompatible,
			Models: []provider.Model{{ID: "demo-model", DisplayName: "演示模型", Enabled: true, ContextWindow: 10000}}},
	}}
	registry, err := provider.NewRegistry(ctx, repo, map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: &kernelTestAdapter{},
	})
	if err != nil {
		t.Fatalf("创建供应商 Registry 失败: %v", err)
	}
	var mu sync.Mutex
	var notices []struct {
		invocationID string
		cause        error
		consecutive  int
	}
	kernel, err := NewKernel(Config{
		AppName:             appName,
		SessionService:      service,
		Providers:           registry,
		EnableCompaction:    true,
		CompactionRatio:     0.1,
		CompactionSafety:    1,
		CompactionRetention: 1,
		CompactionFailureObserver: func(_ context.Context, invocationID string, cause error, consecutive int) {
			mu.Lock()
			notices = append(notices, struct {
				invocationID string
				cause        error
				consecutive  int
			}{invocationID: invocationID, cause: cause, consecutive: consecutive})
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("创建 Kernel 失败: %v", err)
	}
	result := collectKernelEvents(kernel.Run(ctx, ChatRequest{
		UserID: userID, InvocationID: "invocation-compaction", SessionID: sessionID,
		ProviderID: "demo", ModelID: "demo-model", Message: "继续回答",
	}))
	if result.err != nil {
		t.Fatalf("压缩失败不应丢弃当前回复: %v", result.err)
	}
	if result.text != "测试回复" {
		t.Fatalf("压缩失败后的回复 = %q，期望保留模型回复", result.text)
	}
	if got := service.countCompactions(); got == 0 {
		t.Fatal("测试未触发 SessionService 的压缩摘要写入")
	}
	mu.Lock()
	gotNotices := append([]struct {
		invocationID string
		cause        error
		consecutive  int
	}{}, notices...)
	mu.Unlock()
	if len(gotNotices) != 1 {
		t.Fatalf("压缩失败观测次数 = %d，期望 1: %#v", len(gotNotices), gotNotices)
	}
	if gotNotices[0].invocationID != "invocation-compaction" || gotNotices[0].consecutive != 1 {
		t.Fatalf("压缩失败观测字段错误: %#v", gotNotices[0])
	}
	if gotNotices[0].cause == nil || !strings.Contains(gotNotices[0].cause.Error(), "api_key=secret-value") {
		t.Fatalf("观测回调应收到原始错误供 Runtime 提取类型，实际: %#v", gotNotices[0].cause)
	}
}

func TestCompactionFailureTrackerDeduplicatesRunnerSignal(t *testing.T) {
	var mu sync.Mutex
	var counts []int
	tracker := newCompactionFailureTracker(func(_ context.Context, _ string, _ error, consecutive int) {
		mu.Lock()
		counts = append(counts, consecutive)
		mu.Unlock()
	}, "invocation-tracker")

	tracker.noteAppendFailure(context.Background(), errors.New("append failed"))
	tracker.noteRunnerFailure(context.Background(), errors.New("runner wrapped append failed"))
	tracker.noteEvent()
	tracker.noteRunnerFailure(context.Background(), errors.New("independent compaction failed"))

	mu.Lock()
	got := append([]int(nil), counts...)
	mu.Unlock()
	if len(got) != 2 || got[0] != 1 || got[1] != 1 {
		t.Fatalf("Tracker 未正确去重或在事件后重置连续次数: %#v", got)
	}
}

func TestKernelReportsCompactionUsageSeparately(t *testing.T) {
	base := session.InMemoryService()
	const appName = "compaction-usage-observer-test"
	const userID = "user-compaction-usage"
	const sessionID = "session-compaction-usage"
	ctx := context.Background()
	created, err := base.Create(ctx, &session.CreateRequest{AppName: appName, UserID: userID, SessionID: sessionID})
	if err != nil {
		t.Fatalf("创建测试 Session 失败: %v", err)
	}
	oldUser := session.NewEvent(ctx, "old-invocation")
	oldUser.Author = "user"
	oldUser.LLMResponse.Content = genaiContent(strings.Repeat("历史上下文 ", 1800), "user")
	if err := base.AppendEvent(ctx, created.Session, oldUser); err != nil {
		t.Fatalf("写入历史用户事件失败: %v", err)
	}
	oldAssistant := session.NewEvent(ctx, "old-invocation")
	oldAssistant.Author = "assistant"
	oldAssistant.LLMResponse.Content = genaiContent("历史回复", "model")
	if err := base.AppendEvent(ctx, created.Session, oldAssistant); err != nil {
		t.Fatalf("写入历史助手事件失败: %v", err)
	}

	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "演示供应商", BaseURL: "http://127.0.0.1:9999/v1", Protocol: provider.ProtocolOpenAICompatible,
			Models: []provider.Model{{ID: "demo-model", DisplayName: "演示模型", Enabled: true, ContextWindow: 10000}}},
	}}
	registry, err := provider.NewRegistry(ctx, repo, map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: &compactionUsageKernelAdapter{},
	})
	if err != nil {
		t.Fatalf("创建供应商 Registry 失败: %v", err)
	}
	var mu sync.Mutex
	var observed []map[string]any
	kernel, err := NewKernel(Config{
		AppName: appName, SessionService: base, Providers: registry,
		EnableCompaction: true, CompactionRatio: 0.1, CompactionSafety: 1, CompactionRetention: 1,
		CompactionUsageObserver: func(_ context.Context, invocationID string, usage map[string]any) {
			if invocationID != "invocation-compaction-usage" {
				t.Errorf("压缩 usage observer invocation ID = %q", invocationID)
			}
			mu.Lock()
			if usage != nil {
				observed = append(observed, usage)
			}
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("创建 Kernel 失败: %v", err)
	}
	result := collectKernelEvents(kernel.Run(ctx, ChatRequest{
		UserID: userID, InvocationID: "invocation-compaction-usage", SessionID: sessionID,
		ProviderID: "demo", ModelID: "demo-model", Message: "继续回答",
	}))
	if result.err != nil || result.text != "测试回复" {
		t.Fatalf("压缩 usage 观测不应改变当前回复: err=%v text=%q", result.err, result.text)
	}
	mu.Lock()
	got := append([]map[string]any(nil), observed...)
	mu.Unlock()
	if len(got) == 0 {
		t.Fatal("未观察到 ADK 压缩 summarizer 的 usage")
	}
	if value, ok := got[0]["promptTokenCount"].(float64); !ok || value != 30 {
		t.Fatalf("压缩 usage 应为脱敏 JSON metadata，实际: %#v", got[0])
	}
	if _, exists := got[0]["output"]; exists {
		t.Fatalf("压缩 usage observer 不应携带模型正文: %#v", got[0])
	}
}

type compactionUsageKernelAdapter struct{}

func (a *compactionUsageKernelAdapter) BuildModel(_ context.Context, _ provider.Provider, modelID string) (adkmodel.LLM, error) {
	return &compactionUsageKernelLLM{name: modelID}, nil
}

func (a *compactionUsageKernelAdapter) TestConnection(context.Context, provider.Provider) (provider.ProbeResult, error) {
	return provider.ProbeResult{Message: "连接正常"}, nil
}

func (a *compactionUsageKernelAdapter) DiscoverModels(context.Context, provider.Provider) ([]provider.Model, error) {
	return nil, nil
}

type compactionUsageKernelLLM struct {
	name string
}

func (m *compactionUsageKernelLLM) Name() string { return m.name }

func (m *compactionUsageKernelLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			Content: genai.NewContentFromText("测试回复", genai.RoleModel),
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount: 30, CandidatesTokenCount: 6, TotalTokenCount: 36,
			},
		}, nil)
	}
}

// genaiContent keeps the fixture readable without importing the GenAI package
// into every assertion above.
func genaiContent(text, role string) *genai.Content {
	return genai.NewContentFromText(text, genai.Role(role))
}
