package runtime

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"Abot/internal/agent"
	"Abot/internal/provider"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// TestCoordinatorContextRecoveryPersistsNoticeAndTrace exercises the full
// Runtime boundary: the Kernel trims one historical request after a provider
// context-limit error, Coordinator persists the bounded recovery notice, and
// Trace derives the retry span/metric from that durable event.
func TestCoordinatorContextRecoveryPersistsNoticeAndTrace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		appName        = "runtime-context-recovery-test"
		userID         = "runtime-context-recovery-user"
		conversationID = "runtime-context-recovery-conversation"
	)
	model := &runtimeContextRecoveryModel{}
	providerRepo := &runtimeProviderRepository{providers: map[string]provider.Provider{
		"demo": {
			ID: "demo", Name: "demo", BaseURL: "http://127.0.0.1:9999/v1",
			Protocol: provider.ProtocolOpenAICompatible,
			Models:   []provider.Model{{ID: "model", DisplayName: "model", Enabled: true, ContextWindow: 4096, MaxOutputTokens: 128}},
		},
	}}
	registry, err := provider.NewRegistry(ctx, providerRepo, map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: &runtimeContextRecoveryAdapter{model: model},
	})
	if err != nil {
		t.Fatalf("创建 context recovery Registry 失败: %v", err)
	}
	kernel, err := agent.NewKernel(agent.Config{
		AppName: appName, SessionService: session.InMemoryService(), Providers: registry,
		Instruction: "只回复固定测试文本。",
	})
	if err != nil {
		t.Fatalf("创建 context recovery Kernel 失败: %v", err)
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

	waitTerminal := func(id string) Invocation {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			item, getErr := coordinator.GetInvocation(ctx, id)
			if getErr != nil {
				t.Fatalf("读取 Invocation %s 失败: %v", id, getErr)
			}
			if item.Status.Terminal() {
				return item
			}
			time.Sleep(10 * time.Millisecond)
		}
		item, _ := coordinator.GetInvocation(ctx, id)
		t.Fatalf("等待 Invocation %s 终态超时: %#v", id, item)
		return Invocation{}
	}

	// Two completed turns provide removable user/model history for the third
	// request. Keeping each admission sequential also covers Coordinator's
	// same-conversation active-invocation guard.
	for index, message := range []string{"历史消息一", "历史消息二"} {
		item, startErr := coordinator.StartInvocation(ctx, agent.ChatRequest{
			UserID: userID, ConversationID: conversationID, SessionID: conversationID,
			ProviderID: "demo", ModelID: "model", Message: message, Stream: true,
		})
		if startErr != nil {
			t.Fatalf("启动历史 Invocation %d 失败: %v", index+1, startErr)
		}
		completed := waitTerminal(item.ID)
		if completed.Status != InvocationCompleted {
			t.Fatalf("历史 Invocation %d 未成功完成: %#v", index+1, completed)
		}
	}

	item, err := coordinator.StartInvocation(ctx, agent.ChatRequest{
		UserID: userID, ConversationID: conversationID, SessionID: conversationID,
		ProviderID: "demo", ModelID: "model", Message: "触发上下文恢复", Stream: true,
	})
	if err != nil {
		t.Fatalf("启动恢复 Invocation 失败: %v", err)
	}
	completed := waitTerminal(item.ID)
	if completed.Status != InvocationCompleted {
		t.Fatalf("恢复 Invocation 未成功完成: %#v", completed)
	}

	if calls := model.callCount(); calls != 4 {
		t.Fatalf("两轮历史加一次失败/恢复应只有 4 次 provider call，实际=%d", calls)
	}
	model.mu.Lock()
	requestLengths := append([]int(nil), model.requestLengths...)
	model.mu.Unlock()
	if len(requestLengths) != 4 || requestLengths[3] >= requestLengths[2] {
		t.Fatalf("恢复请求应移除旧历史: request contents=%v", requestLengths)
	}
	manifests, err := repo.ListContextManifests(ctx, item.ID)
	if err != nil {
		t.Fatalf("读取 context manifest 失败: %v", err)
	}
	trimmedManifest := false
	for _, manifest := range manifests {
		for _, warning := range manifest.Warnings {
			if warning == "context_history_trimmed_once" {
				trimmedManifest = true
				if len(manifest.Excluded) == 0 {
					t.Fatalf("受控重建 manifest 缺少 excluded 历史: %#v", manifest)
				}
			}
		}
	}
	if !trimmedManifest {
		t.Fatalf("Runtime 未保存 context_history_trimmed_once manifest: %#v", manifests)
	}

	events, err := repo.ListEvents(ctx, item.ID, 0, 200)
	if err != nil {
		t.Fatalf("读取 Runtime 事件失败: %v", err)
	}
	var notices []AgentEvent
	for _, event := range events {
		if event.Type == EventRuntimeNotice && event.Data != nil && event.Data["code"] == "context_limit_exceeded" {
			notices = append(notices, event)
		}
	}
	if len(notices) != 1 {
		t.Fatalf("应持久化恰好一个 context-limit notice: %#v", notices)
	}
	notice := notices[0]
	if notice.Data["attempt"] != 1 || notice.Data["recovered"] != true {
		t.Fatalf("context-limit notice 状态错误: %#v", notice.Data)
	}
	if callID, _ := notice.Data["model_call_id"].(string); callID == "" {
		t.Fatalf("context-limit notice 缺少 model_call_id: %#v", notice.Data)
	}
	if _, leaked := notice.Data["error"]; leaked {
		t.Fatalf("context-limit notice 不应复制 provider 原始错误: %#v", notice.Data)
	}
	snapshot, err := coordinator.GetRuntimeSnapshot(ctx, item.ID)
	if err != nil {
		t.Fatalf("读取终态 Runtime Snapshot 失败: %v", err)
	}
	if snapshot.Phase != string(InvocationCompleted) || snapshot.Budget.LastManifestID == "" {
		t.Fatalf("终态 Snapshot 未保留恢复后的 Manifest/phase: %#v", snapshot)
	}
	var snapshotEvent, terminalEvent AgentEvent
	for _, event := range events {
		if event.Type == EventRuntimeSnapshot {
			snapshotEvent = event
		}
		if event.Type == EventInvocationCompleted {
			terminalEvent = event
		}
	}
	if notice.Sequence <= 0 || snapshotEvent.Sequence <= notice.Sequence || terminalEvent.Sequence <= snapshotEvent.Sequence {
		t.Fatalf("恢复 notice、终态 snapshot、终态事件顺序不正确: notice=%d snapshot=%d terminal=%d", notice.Sequence, snapshotEvent.Sequence, terminalEvent.Sequence)
	}

	trace, err := coordinator.GetInvocationTrace(ctx, item.ID)
	if err != nil {
		t.Fatalf("读取 Invocation Trace 失败: %v", err)
	}
	if trace.Metrics.ContextLimitRetries != 1 {
		t.Fatalf("Trace context-limit retry metric 错误: %#v", trace.Metrics)
	}
	var recoverySpan *TraceSpan
	for index := range trace.Spans {
		if trace.Spans[index].Name == "context.rebuild" {
			recoverySpan = &trace.Spans[index]
			break
		}
	}
	if recoverySpan == nil || recoverySpan.Status != "completed" || recoverySpan.Outcome != "success" || recoverySpan.Attributes["recovered"] != true || recoverySpan.Attributes["attempt"] != 1 {
		t.Fatalf("Trace context.rebuild span 错误: %#v", recoverySpan)
	}
	if trace.Metrics.RuntimeNoticeCodes == nil || len(trace.Metrics.RuntimeNoticeCodes) == 0 || trace.Metrics.RuntimeNoticeCodes[0] != "context_limit_exceeded" {
		t.Fatalf("Trace 未保留 context-limit notice code: %#v", trace.Metrics.RuntimeNoticeCodes)
	}
}

// TestCoordinatorContextRecoveryFailsClosedAfterOneRebuild verifies the
// negative boundary of the same flow: when the rebuilt request is still too
// large, Runtime records both attempts and fails the Invocation without an
// outer generic retry that could duplicate provider side effects.
func TestCoordinatorContextRecoveryFailsClosedAfterOneRebuild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		appName        = "runtime-context-recovery-failure-test"
		userID         = "runtime-context-recovery-failure-user"
		conversationID = "runtime-context-recovery-failure-conversation"
	)
	model := &runtimeContextRecoveryModel{alwaysLimit: true, limitAfter: 3}
	providerRepo := &runtimeProviderRepository{providers: map[string]provider.Provider{
		"demo": {
			ID: "demo", Name: "demo", BaseURL: "http://127.0.0.1:9999/v1",
			Protocol: provider.ProtocolOpenAICompatible,
			Models:   []provider.Model{{ID: "model", DisplayName: "model", Enabled: true, ContextWindow: 4096, MaxOutputTokens: 128}},
		},
	}}
	registry, err := provider.NewRegistry(ctx, providerRepo, map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: &runtimeContextRecoveryAdapter{model: model},
	})
	if err != nil {
		t.Fatalf("创建 context recovery failure Registry 失败: %v", err)
	}
	kernel, err := agent.NewKernel(agent.Config{
		AppName: appName, SessionService: session.InMemoryService(), Providers: registry,
		Instruction: "只回复固定测试文本。",
	})
	if err != nil {
		t.Fatalf("创建 context recovery failure Kernel 失败: %v", err)
	}
	repo := NewMemoryRepository()
	coordinator, err := NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatalf("创建 failure Coordinator 失败: %v", err)
	}
	if err := coordinator.Start(ctx); err != nil {
		t.Fatalf("启动 failure Coordinator 失败: %v", err)
	}
	defer coordinator.Close()

	waitTerminal := func(id string) Invocation {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			item, getErr := coordinator.GetInvocation(ctx, id)
			if getErr != nil {
				t.Fatalf("读取 failure Invocation %s 失败: %v", id, getErr)
			}
			if item.Status.Terminal() {
				return item
			}
			time.Sleep(10 * time.Millisecond)
		}
		item, _ := coordinator.GetInvocation(ctx, id)
		t.Fatalf("等待 failure Invocation %s 终态超时: %#v", id, item)
		return Invocation{}
	}

	for index, message := range []string{"历史消息一", "历史消息二"} {
		item, startErr := coordinator.StartInvocation(ctx, agent.ChatRequest{
			UserID: userID, ConversationID: conversationID, SessionID: conversationID,
			ProviderID: "demo", ModelID: "model", Message: message, Stream: true,
		})
		if startErr != nil {
			t.Fatalf("启动 failure 历史 Invocation %d 失败: %v", index+1, startErr)
		}
		completed := waitTerminal(item.ID)
		if completed.Status != InvocationCompleted {
			t.Fatalf("failure 历史 Invocation %d 未成功完成: %#v", index+1, completed)
		}
	}

	item, err := coordinator.StartInvocation(ctx, agent.ChatRequest{
		UserID: userID, ConversationID: conversationID, SessionID: conversationID,
		ProviderID: "demo", ModelID: "model", Message: "触发不可恢复上下文超限", Stream: true,
	})
	if err != nil {
		t.Fatalf("启动 failure recovery Invocation 失败: %v", err)
	}
	failed := waitTerminal(item.ID)
	if failed.Status != InvocationFailed || !strings.Contains(failed.Error, "maximum context length exceeded") {
		t.Fatalf("不可恢复上下文超限应进入 failed 终态: %#v", failed)
	}
	if calls := model.callCount(); calls != 4 {
		t.Fatalf("两轮历史加一次失败/重建失败应只有 4 次 provider call，实际=%d", calls)
	}
	model.mu.Lock()
	requestLengths := append([]int(nil), model.requestLengths...)
	model.mu.Unlock()
	if len(requestLengths) != 4 || requestLengths[3] >= requestLengths[2] {
		t.Fatalf("失败重建请求仍应使用收缩后的历史: request contents=%v", requestLengths)
	}

	events, err := repo.ListEvents(ctx, item.ID, 0, 200)
	if err != nil {
		t.Fatalf("读取 failure Runtime 事件失败: %v", err)
	}
	var notices []AgentEvent
	for _, event := range events {
		if event.Type == EventRuntimeNotice && event.Data != nil && event.Data["code"] == "context_limit_exceeded" {
			notices = append(notices, event)
		}
	}
	if len(notices) != 2 {
		t.Fatalf("失败恢复应持久化恰好两次 context-limit notice: %#v", notices)
	}
	if notices[0].Data["attempt"] != 1 || notices[0].Data["recovered"] != true || notices[1].Data["attempt"] != 2 || notices[1].Data["recovered"] != false {
		t.Fatalf("失败恢复 notice 状态错误: %#v", notices)
	}
	for _, notice := range notices {
		if _, leaked := notice.Data["error"]; leaked {
			t.Fatalf("context-limit notice 不应复制 provider 原始错误: %#v", notice.Data)
		}
	}

	snapshot, err := coordinator.GetRuntimeSnapshot(ctx, item.ID)
	if err != nil {
		t.Fatalf("读取 failure 终态 Runtime Snapshot 失败: %v", err)
	}
	if snapshot.Phase != string(InvocationFailed) || len(snapshot.Blockers) == 0 {
		t.Fatalf("失败 Invocation 的终态 Snapshot 应保留 blocker: %#v", snapshot)
	}
	var snapshotEvent, terminalEvent AgentEvent
	for _, event := range events {
		if event.Type == EventRuntimeSnapshot {
			snapshotEvent = event
		}
		if event.Type == EventInvocationFailed {
			terminalEvent = event
		}
	}
	if notices[0].Sequence <= 0 || notices[1].Sequence <= notices[0].Sequence || snapshotEvent.Sequence <= notices[1].Sequence || terminalEvent.Sequence <= snapshotEvent.Sequence {
		t.Fatalf("失败恢复事件顺序不正确: first=%d second=%d snapshot=%d terminal=%d", notices[0].Sequence, notices[1].Sequence, snapshotEvent.Sequence, terminalEvent.Sequence)
	}

	trace, err := coordinator.GetInvocationTrace(ctx, item.ID)
	if err != nil {
		t.Fatalf("读取 failure Invocation Trace 失败: %v", err)
	}
	var root TraceSpan
	if len(trace.Spans) > 0 {
		root = trace.Spans[0]
	}
	if trace.Metrics.ContextLimitRetries != 2 || root.ErrorCode != "runtime_failed" {
		t.Fatalf("失败恢复 Trace metric/root error 错误: metrics=%#v root=%#v", trace.Metrics, root)
	}
	var recoveredSpan, failedSpan *TraceSpan
	for index := range trace.Spans {
		span := &trace.Spans[index]
		if span.Name != "context.rebuild" {
			continue
		}
		if span.Attributes["recovered"] == true {
			recoveredSpan = span
		} else {
			failedSpan = span
		}
	}
	if recoveredSpan == nil || recoveredSpan.Status != "completed" || failedSpan == nil || failedSpan.Status != "failed" || failedSpan.ErrorCode != "context_limit_exceeded" {
		t.Fatalf("失败恢复 context.rebuild spans 错误: recovered=%#v failed=%#v", recoveredSpan, failedSpan)
	}
}

type runtimeContextRecoveryAdapter struct {
	model *runtimeContextRecoveryModel
}

func (a *runtimeContextRecoveryAdapter) BuildModel(context.Context, provider.Provider, string) (adkmodel.LLM, error) {
	return a.model, nil
}

func (a *runtimeContextRecoveryAdapter) TestConnection(context.Context, provider.Provider) (provider.ProbeResult, error) {
	return provider.ProbeResult{Message: "连接正常"}, nil
}

func (a *runtimeContextRecoveryAdapter) DiscoverModels(context.Context, provider.Provider) ([]provider.Model, error) {
	return nil, nil
}

type runtimeContextRecoveryModel struct {
	mu             sync.Mutex
	calls          int
	requestLengths []int
	alwaysLimit    bool
	limitAfter     int
}

func (m *runtimeContextRecoveryModel) Name() string { return "runtime-context-recovery-model" }

func (m *runtimeContextRecoveryModel) GenerateContent(_ context.Context, request *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	m.mu.Lock()
	m.calls++
	call := m.calls
	alwaysLimit := m.alwaysLimit
	limitAfter := m.limitAfter
	length := 0
	if request != nil {
		length = len(request.Contents)
	}
	m.requestLengths = append(m.requestLengths, length)
	m.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		limit := call == 3
		if alwaysLimit && (limitAfter <= 0 || call >= limitAfter) {
			limit = true
		}
		if limit {
			yield(nil, errors.New("maximum context length exceeded"))
			return
		}
		text := "正常回复"
		if call == 4 {
			text = "恢复成功"
		}
		yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText(text, genai.RoleModel)}, nil)
	}
}

func (m *runtimeContextRecoveryModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}
