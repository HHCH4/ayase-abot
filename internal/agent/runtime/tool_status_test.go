package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Abot/internal/agent"
	"Abot/internal/provider"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func toolStatusWaitingFixture(t *testing.T) (*MemoryRepository, Invocation, string, time.Time) {
	t.Helper()
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	invocation := Invocation{ID: "inv-tool-status", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, AgentEvent{ID: "tool-status-request", InvocationID: invocation.ID, Type: EventToolRequested, Timestamp: now, Data: map[string]any{
		"call_id": "wait-external", "name": "wait_for_external_result", "args": map[string]any{"job_id": "job-42"},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, AgentEvent{ID: "tool-status-waiting", InvocationID: invocation.ID, Type: EventInvocationWaiting, Timestamp: now.Add(time.Second), Data: map[string]any{
		"reason": "tool", "tool_call_ids": []string{"wait-external"},
	}}); err != nil {
		t.Fatal(err)
	}
	return repo, invocation, "wait-external", now
}

func TestQueryLongRunningToolStatusBindsCurrentWaitWithoutResume(t *testing.T) {
	repo, invocation, waitID, now := toolStatusWaitingFixture(t)
	ctx := context.Background()
	var received RuntimeLongRunningToolStatusQuery
	coordinator := &Coordinator{
		repo:                  repo,
		started:               true,
		rootCtx:               context.Background(),
		toolStatusSource:      "agent-runtime",
		toolStatusDestination: "tool-runtime",
		toolStatusResolver: RuntimeLongRunningToolStatusResolverFunc(func(_ context.Context, query RuntimeLongRunningToolStatusQuery) (RuntimeLongRunningToolStatus, error) {
			received = query
			return RuntimeLongRunningToolStatus{Status: RuntimeLongRunningToolStatusPending, ObservedAt: now.Add(2 * time.Second)}, nil
		}),
	}

	before, err := repo.ListEvents(ctx, invocation.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	status, err := coordinator.QueryLongRunningToolStatus(ctx, invocation.ID, waitID)
	if err != nil {
		t.Fatalf("查询 pending 状态失败: %v", err)
	}
	if status.Status != RuntimeLongRunningToolStatusPending || status.InvocationID != invocation.ID || status.WaitID != waitID || status.ToolName != "wait_for_external_result" {
		t.Fatalf("状态绑定错误: %#v", status)
	}
	if received.Source != "agent-runtime" || received.Destination != "tool-runtime" || received.InvocationID != invocation.ID || received.WaitID != waitID || received.ToolName != "wait_for_external_result" || !validRuntimeLongRunningToolStatusDigest(received.RequestDigest) {
		t.Fatalf("resolver 收到的绑定 query 错误: %#v", received)
	}
	if status.RequestDigest != received.RequestDigest {
		t.Fatalf("status/query request digest 不一致: status=%q query=%q", status.RequestDigest, received.RequestDigest)
	}
	after, err := repo.ListEvents(ctx, invocation.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	current, err := repo.GetInvocation(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != InvocationWaitingTool || len(after) != len(before) {
		t.Fatalf("状态查询不能自动恢复或追加事件: current=%#v before=%d after=%d", current, len(before), len(after))
	}
	if len(after) > 0 && after[0].Data["request_digest"] != nil {
		t.Fatalf("手工 fixture 不应伪造 request digest；查询应能兼容回退计算: %#v", after[0].Data)
	}
}

func TestRecordToolCallEventPersistsStableRequestDigest(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-tool-status-digest", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo}
	event := &AgentEvent{ID: "digest-request", InvocationID: invocation.ID, Type: EventToolRequested, Timestamp: now, Data: map[string]any{
		"call_id": "call-1", "name": "wait_for_external_result", "args": map[string]any{"job_id": "job-42"},
	}}
	if err := coordinator.recordToolCallEvent(context.Background(), invocation.ID, event); err != nil {
		t.Fatal(err)
	}
	digest, _ := event.Data["request_digest"].(string)
	if !validRuntimeLongRunningToolStatusDigest(digest) {
		t.Fatalf("tool.requested 应保存有效 request_digest: %#v", event.Data)
	}
	first := digest
	// The local ToolCall ID is deliberately ignored by the digest helper.
	event.Data["tool_call_id"] = "different-local-audit-id"
	second, err := runtimeLongRunningToolRequestDigest(invocation.ID, event.Data)
	if err != nil || first != second {
		t.Fatalf("内部 ToolCall ID 变化不得改变 request digest: first=%q second=%q err=%v", first, second, err)
	}
}

func TestQueryLongRunningToolStatusTerminalProofIsBoundedAndNoAutoReplay(t *testing.T) {
	repo, invocation, waitID, now := toolStatusWaitingFixture(t)
	ctx := context.Background()
	coordinator := &Coordinator{repo: repo, started: true, rootCtx: context.Background(), toolStatusResolver: RuntimeLongRunningToolStatusResolverFunc(func(_ context.Context, _ RuntimeLongRunningToolStatusQuery) (RuntimeLongRunningToolStatus, error) {
		return RuntimeLongRunningToolStatus{Status: RuntimeLongRunningToolStatusSucceeded, Result: map[string]any{"output": "ready"}, ObservedAt: now.Add(time.Second)}, nil
	})}
	status, err := coordinator.QueryLongRunningToolStatus(ctx, invocation.ID, waitID)
	if err != nil {
		t.Fatalf("查询 succeeded proof 失败: %v", err)
	}
	if status.Status != RuntimeLongRunningToolStatusSucceeded || !status.ResultAvailable || status.Result["output"] != "ready" || !validRuntimeLongRunningToolStatusDigest(status.ResultDigest) {
		t.Fatalf("succeeded proof 未通过有界校验: %#v", status)
	}
	current, err := repo.GetInvocation(ctx, invocation.ID)
	if err != nil || current.Status != InvocationWaitingTool {
		t.Fatalf("succeeded proof 不得自动转 queued/running: invocation=%#v err=%v", current, err)
	}
}

func TestQueryLongRunningToolStatusRejectsUnavailableAndWrongWait(t *testing.T) {
	repo, invocation, waitID, _ := toolStatusWaitingFixture(t)
	ctx := context.Background()
	withoutResolver := &Coordinator{repo: repo, started: true, rootCtx: context.Background()}
	if _, err := withoutResolver.QueryLongRunningToolStatus(ctx, invocation.ID, waitID); !errors.Is(err, ErrRuntimeLongRunningToolStatusUnavailable) {
		t.Fatalf("未配置 resolver 应明确返回 unavailable: %v", err)
	}
	coordinator := &Coordinator{repo: repo, started: true, rootCtx: context.Background(), toolStatusResolver: RuntimeLongRunningToolStatusResolverFunc(func(_ context.Context, _ RuntimeLongRunningToolStatusQuery) (RuntimeLongRunningToolStatus, error) {
		return RuntimeLongRunningToolStatus{Status: RuntimeLongRunningToolStatusUnknown, Reason: "provider timeout", ObservedAt: time.Now().UTC()}, nil
	})}
	if _, err := coordinator.QueryLongRunningToolStatus(ctx, invocation.ID, "wrong-wait"); !errors.Is(err, ErrRuntimeLongRunningToolStatusTargetMismatch) {
		t.Fatalf("错误 wait_id 应 fail-closed: %v", err)
	}
}

func TestReconcileLongRunningToolStatusKeepsUnprovenStatesReadOnly(t *testing.T) {
	for _, test := range []struct {
		name   string
		status RuntimeLongRunningToolStatusState
		reason string
	}{
		{name: "pending", status: RuntimeLongRunningToolStatusPending, reason: "pending_no_resume"},
		{name: "failed", status: RuntimeLongRunningToolStatusFailed, reason: "failed_requires_policy"},
		{name: "unknown", status: RuntimeLongRunningToolStatusUnknown, reason: "unknown_requires_manual_recovery"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo, invocation, waitID, now := toolStatusWaitingFixture(t)
			coordinator := &Coordinator{repo: repo, started: true, rootCtx: context.Background(), toolStatusResolver: RuntimeLongRunningToolStatusResolverFunc(func(_ context.Context, _ RuntimeLongRunningToolStatusQuery) (RuntimeLongRunningToolStatus, error) {
				return RuntimeLongRunningToolStatus{Status: test.status, Reason: "external result", ObservedAt: now}, nil
			})}
			before, err := repo.ListEvents(context.Background(), invocation.ID, 0, 20)
			if err != nil {
				t.Fatal(err)
			}
			result, err := coordinator.ReconcileLongRunningToolStatus(context.Background(), invocation.ID, waitID)
			if err != nil {
				t.Fatalf("%s reconcile 失败: %v", test.name, err)
			}
			if result.Resumed || result.Invocation != nil || result.Reason != test.reason || result.Status.Status != test.status {
				t.Fatalf("%s 不安全地改变了恢复边界: %#v", test.name, result)
			}
			after, err := repo.ListEvents(context.Background(), invocation.ID, 0, 20)
			if err != nil {
				t.Fatal(err)
			}
			current, err := repo.GetInvocation(context.Background(), invocation.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Status != InvocationWaitingTool || len(after) != len(before) {
				t.Fatalf("%s 证据不足时不得写入恢复: current=%#v before=%d after=%d", test.name, current, len(before), len(after))
			}
		})
	}
}

func TestReconcileLongRunningToolStatusRequiresResultBody(t *testing.T) {
	repo, invocation, waitID, now := toolStatusWaitingFixture(t)
	requestDigest := "sha256:" + strings.Repeat("a", 64)
	coordinator := &Coordinator{repo: repo, started: true, rootCtx: context.Background(), toolStatusResolver: RuntimeLongRunningToolStatusResolverFunc(func(_ context.Context, query RuntimeLongRunningToolStatusQuery) (RuntimeLongRunningToolStatus, error) {
		requestDigest = query.RequestDigest
		return RuntimeLongRunningToolStatus{Status: RuntimeLongRunningToolStatusSucceeded, ResultDigest: "sha256:" + strings.Repeat("b", 64), ObservedAt: now}, nil
	})}
	result, err := coordinator.ReconcileLongRunningToolStatus(context.Background(), invocation.ID, waitID)
	if err != nil {
		t.Fatalf("digest-only succeeded proof 不应报错: %v", err)
	}
	if result.Resumed || result.Invocation != nil || result.Reason != "result_digest_only" || result.Status.ResultAvailable {
		t.Fatalf("缺少 result 正文时不应恢复: %#v", result)
	}
	if !validRuntimeLongRunningToolStatusDigest(requestDigest) {
		t.Fatalf("resolver 未收到有效 request digest: %q", requestDigest)
	}
	current, err := repo.GetInvocation(context.Background(), invocation.ID)
	if err != nil || current.Status != InvocationWaitingTool {
		t.Fatalf("digest-only proof 不应改变 Invocation: %#v err=%v", current, err)
	}
}

func TestReconcileLongRunningToolStatusRejectsPartialMultiWaitBoundary(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-tool-status-batch", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		id   string
		name string
	}{
		{id: "wait-a", name: "wait_alpha"},
		{id: "wait-b", name: "wait_beta"},
	} {
		if _, err := repo.AppendEvent(ctx, AgentEvent{ID: "request-" + item.id, InvocationID: invocation.ID, Type: EventToolRequested, Timestamp: now, Data: map[string]any{
			"call_id": item.id, "name": item.name, "args": map[string]any{"job_id": item.id},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.AppendEvent(ctx, AgentEvent{ID: "waiting-batch", InvocationID: invocation.ID, Type: EventInvocationWaiting, Timestamp: now.Add(time.Second), Data: map[string]any{
		"reason": "tool", "tool_call_ids": []string{"wait-a", "wait-b"},
	}}); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, started: true, rootCtx: ctx, toolStatusResolver: RuntimeLongRunningToolStatusResolverFunc(func(_ context.Context, query RuntimeLongRunningToolStatusQuery) (RuntimeLongRunningToolStatus, error) {
		return RuntimeLongRunningToolStatus{Status: RuntimeLongRunningToolStatusSucceeded, Result: map[string]any{"job_id": query.WaitID, "ok": true}, ObservedAt: now}, nil
	})}
	before, err := repo.ListEvents(ctx, invocation.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.ReconcileLongRunningToolStatus(ctx, invocation.ID, "wait-a"); !errors.Is(err, ErrResumeMismatch) {
		t.Fatalf("多 wait 边界只收到一个 result 时应拒绝部分恢复: %v", err)
	}
	after, err := repo.ListEvents(ctx, invocation.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	current, err := repo.GetInvocation(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != InvocationWaitingTool || len(after) != len(before) {
		t.Fatalf("多 wait 部分恢复不得改变 Invocation/events: current=%#v before=%d after=%d", current, len(before), len(after))
	}
}

func TestReconcileLongRunningToolStatusesKeepsMixedProofsReadOnly(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-tool-status-mixed", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		id   string
		name string
	}{
		{id: "wait-a", name: "wait_alpha"},
		{id: "wait-b", name: "wait_beta"},
	} {
		if _, err := repo.AppendEvent(ctx, AgentEvent{ID: "mixed-request-" + item.id, InvocationID: invocation.ID, Type: EventToolRequested, Timestamp: now, Data: map[string]any{
			"call_id": item.id, "name": item.name, "args": map[string]any{"job_id": item.id},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.AppendEvent(ctx, AgentEvent{ID: "mixed-waiting", InvocationID: invocation.ID, Type: EventInvocationWaiting, Timestamp: now.Add(time.Second), Data: map[string]any{
		"reason": "tool", "tool_call_ids": []string{"wait-a", "wait-b"},
	}}); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, started: true, rootCtx: ctx, toolStatusResolver: RuntimeLongRunningToolStatusResolverFunc(func(_ context.Context, query RuntimeLongRunningToolStatusQuery) (RuntimeLongRunningToolStatus, error) {
		if query.WaitID == "wait-a" {
			return RuntimeLongRunningToolStatus{Status: RuntimeLongRunningToolStatusSucceeded, Result: map[string]any{"ok": true}, ObservedAt: now}, nil
		}
		return RuntimeLongRunningToolStatus{Status: RuntimeLongRunningToolStatusUnknown, Reason: "provider timeout", ObservedAt: now}, nil
	})}
	before, err := repo.ListEvents(ctx, invocation.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.ReconcileLongRunningToolStatuses(ctx, invocation.ID, []string{"wait-a", "wait-b"})
	if err != nil {
		t.Fatalf("混合 proof reconcile 失败: %v", err)
	}
	if result.Resumed || result.Invocation != nil || result.Reason != "unproven_status_no_resume" || len(result.Statuses) != 2 {
		t.Fatalf("混合 proof 必须整批只读: %#v", result)
	}
	after, err := repo.ListEvents(ctx, invocation.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	current, err := repo.GetInvocation(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != InvocationWaitingTool || len(after) != len(before) {
		t.Fatalf("混合 proof 不得改变 Invocation/events: current=%#v before=%d after=%d", current, len(before), len(after))
	}
}

func TestReconcileLongRunningToolStatusSubmitsOnlyBoundResult(t *testing.T) {
	model := &toolStatusTextModel{}
	registry, err := provider.NewRegistry(context.Background(), &runtimeProviderRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true}}},
	}}, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: &runtimeApprovalAdapter{model: model}})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{AppName: "tool-status-reconcile", SessionService: session.InMemoryService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	repo, invocation, waitID, now := toolStatusWaitingFixture(t)
	invocation.ProviderID = "demo"
	invocation.ModelID = "model"
	if err := repo.UpdateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := coordinator.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	resolverCalls := 0
	coordinator.SetRuntimeLongRunningToolStatusResolver(RuntimeLongRunningToolStatusResolverFunc(func(_ context.Context, query RuntimeLongRunningToolStatusQuery) (RuntimeLongRunningToolStatus, error) {
		resolverCalls++
		return RuntimeLongRunningToolStatus{Status: RuntimeLongRunningToolStatusSucceeded, Result: map[string]any{"output": "ready", "wait_id": query.WaitID}, ObservedAt: now}, nil
	}))
	result, err := coordinator.ReconcileLongRunningToolStatus(ctx, invocation.ID, waitID)
	if err != nil {
		t.Fatalf("succeeded proof reconcile 失败: %v", err)
	}
	if !result.Resumed || result.Invocation == nil || result.Invocation.ID != invocation.ID || result.Reason != "succeeded_result_submitted" || resolverCalls != 1 {
		t.Fatalf("succeeded proof 未形成单次显式恢复: %#v calls=%d", result, resolverCalls)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := repo.GetInvocation(ctx, invocation.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if current.Status.Terminal() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, err := repo.GetInvocation(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != InvocationCompleted {
		t.Fatalf("恢复 worker 未成功收口: %#v", current)
	}
	events, err := repo.ListEvents(ctx, invocation.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	resumed := 0
	for _, event := range events {
		if event.Type == EventInvocationResumed && event.Data != nil && event.Data["reason"] == "tool" {
			resumed++
			if event.Data["resume_source"] != "external_tool_status" || event.Data["status_query_id"] == "" || event.Data["result_digest"] == "" {
				t.Fatalf("显式外部状态恢复事件缺少 proof metadata: %#v", event.Data)
			}
		}
	}
	if resumed != 1 {
		t.Fatalf("单次 succeeded proof 只能产生一个恢复事件: %d events=%#v", resumed, events)
	}
	// The waiting boundary has already been consumed; a second explicit call
	// must fail closed rather than query/replay the external tool again.
	if _, err := coordinator.ReconcileLongRunningToolStatus(ctx, invocation.ID, waitID); !errors.Is(err, ErrResumeMismatch) {
		t.Fatalf("已消费等待边界后重复 reconcile 应拒绝: %v", err)
	}
	if resolverCalls != 1 {
		t.Fatalf("重复 reconcile 不应再次查询外部工具: calls=%d", resolverCalls)
	}
}

func TestReconcileLongRunningToolStatusesSubmitsAllBoundResultsAtomically(t *testing.T) {
	model := &toolStatusTextModel{}
	registry, err := provider.NewRegistry(context.Background(), &runtimeProviderRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true}}},
	}}, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: &runtimeApprovalAdapter{model: model}})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{AppName: "tool-status-batch-reconcile", SessionService: session.InMemoryService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-tool-status-batch-success", UserID: "user", ConversationID: "conversation", SessionID: "conversation", ProviderID: "demo", ModelID: "model", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		id   string
		name string
	}{
		{id: "wait-a", name: "wait_alpha"},
		{id: "wait-b", name: "wait_beta"},
	} {
		if _, err := repo.AppendEvent(ctx, AgentEvent{ID: "success-request-" + item.id, InvocationID: invocation.ID, Type: EventToolRequested, Timestamp: now, Data: map[string]any{
			"call_id": item.id, "name": item.name, "args": map[string]any{"job_id": item.id},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.AppendEvent(ctx, AgentEvent{ID: "success-waiting", InvocationID: invocation.ID, Type: EventInvocationWaiting, Timestamp: now.Add(time.Second), Data: map[string]any{
		"reason": "tool", "tool_call_ids": []string{"wait-a", "wait-b"},
	}}); err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	resolverCalls := 0
	coordinator.SetRuntimeLongRunningToolStatusResolver(RuntimeLongRunningToolStatusResolverFunc(func(_ context.Context, query RuntimeLongRunningToolStatusQuery) (RuntimeLongRunningToolStatus, error) {
		resolverCalls++
		return RuntimeLongRunningToolStatus{Status: RuntimeLongRunningToolStatusSucceeded, Result: map[string]any{"wait_id": query.WaitID, "ok": true}, ObservedAt: now}, nil
	}))
	result, err := coordinator.ReconcileLongRunningToolStatuses(ctx, invocation.ID, []string{"wait-b", "wait-a"})
	if err != nil {
		t.Fatalf("多 wait succeeded proof reconcile 失败: %v", err)
	}
	if !result.Resumed || result.Invocation == nil || result.Invocation.ID != invocation.ID || result.Reason != "succeeded_results_submitted" || len(result.Statuses) != 2 || resolverCalls != 2 {
		t.Fatalf("多 wait proof 未形成单次原子恢复: %#v calls=%d", result, resolverCalls)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := repo.GetInvocation(ctx, invocation.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if current.Status.Terminal() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, err := repo.GetInvocation(ctx, invocation.ID)
	if err != nil || current.Status != InvocationCompleted {
		t.Fatalf("多 wait 恢复 worker 未成功收口: invocation=%#v err=%v", current, err)
	}
	events, err := repo.ListEvents(ctx, invocation.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	resumed := 0
	for _, event := range events {
		if event.Type == EventInvocationResumed && event.Data != nil && event.Data["reason"] == "tool" {
			resumed++
			if event.Data["resume_source"] != "external_tool_status" {
				t.Fatalf("批量外部状态恢复事件缺少来源标记: %#v", event.Data)
			}
			queryIDs, ok := event.Data["status_query_ids"].([]string)
			if !ok || len(queryIDs) != 2 {
				t.Fatalf("批量外部状态恢复事件缺少 query IDs: %#v", event.Data)
			}
			resultDigests, ok := event.Data["result_digests"].([]string)
			if !ok || len(resultDigests) != 2 {
				t.Fatalf("批量外部状态恢复事件缺少 result digests: %#v", event.Data)
			}
		}
	}
	if resumed != 1 {
		t.Fatalf("多 wait 恢复只能产生一个 resumed 事件: %d events=%#v", resumed, events)
	}
	outboxRepo, ok := any(repo).(InvocationResumeOutboxRepository)
	if !ok {
		t.Fatal("Memory repository 未实现 resume outbox")
	}
	outbox, err := outboxRepo.GetInvocationResumeOutbox(ctx, invocation.ID)
	if err != nil || outbox.Status != InvocationResumeOutboxCompleted {
		t.Fatalf("多 wait 恢复 outbox 未完成: item=%#v err=%v", outbox, err)
	}
	var persisted []InvocationResumeItem
	if err := json.Unmarshal(outbox.ResponseJSON, &persisted); err != nil || len(persisted) != 2 {
		t.Fatalf("多 wait outbox 应保留完整 bounded response 集合: items=%#v err=%v", persisted, err)
	}
	if persisted[0].WaitID != "wait-b" || persisted[1].WaitID != "wait-a" {
		t.Fatalf("多 wait response 顺序应保留调用方顺序: %#v", persisted)
	}
}

type toolStatusTextModel struct{}

func (*toolStatusTextModel) Name() string { return "tool-status-text-model" }

func (*toolStatusTextModel) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText("已安全恢复", genai.RoleModel)}, nil)
	}
}

func TestRuntimeLongRunningToolStatusHTTPRoundTripAndTamper(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	now := time.Date(2026, 9, 13, 11, 0, 0, 0, time.UTC)
	lookupCalls := 0
	receiver, err := NewRuntimeLongRunningToolStatusReceiver("agent-runtime", "tool-runtime", secret, func(_ context.Context, query RuntimeLongRunningToolStatusQuery) (RuntimeLongRunningToolStatus, error) {
		lookupCalls++
		if query.WaitID != "wait-http" || query.ToolName != "wait_for_external_result" {
			return RuntimeLongRunningToolStatus{}, ErrRuntimeLongRunningToolStatusTargetMismatch
		}
		return RuntimeLongRunningToolStatus{Status: RuntimeLongRunningToolStatusSucceeded, Result: map[string]any{"output": "ready"}, ObservedAt: now}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now }
	server := httptest.NewServer(receiver)
	defer server.Close()
	transport := &RuntimeLongRunningToolStatusHTTPTransport{Endpoint: server.URL, Source: "agent-runtime", Destination: "tool-runtime", SharedSecret: secret, Now: func() time.Time { return now }}
	requestDigest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	status, err := transport.QueryLongRunningToolStatus(context.Background(), RuntimeLongRunningToolStatusQuery{
		Version: RuntimeLongRunningToolStatusVersion, Source: "agent-runtime", Destination: "tool-runtime", QueryID: "query-http", InvocationID: "inv-http", WaitID: "wait-http", ToolName: "wait_for_external_result", RequestDigest: requestDigest, IssuedAt: now,
	})
	if err != nil {
		t.Fatalf("HTTP status roundtrip 失败: %v", err)
	}
	if status.Status != RuntimeLongRunningToolStatusSucceeded || !status.ResultAvailable || lookupCalls != 1 {
		t.Fatalf("HTTP status 结果错误: status=%#v calls=%d", status, lookupCalls)
	}

	query, err := SignRuntimeLongRunningToolStatusQuery(RuntimeLongRunningToolStatusQuery{
		Version: RuntimeLongRunningToolStatusVersion, Source: "agent-runtime", Destination: "tool-runtime", QueryID: "query-tampered", InvocationID: "inv-http", WaitID: "wait-http", ToolName: "wait_for_external_result", RequestDigest: requestDigest, IssuedAt: now,
	}, secret)
	if err != nil {
		t.Fatal(err)
	}
	query.WaitID = "other-wait"
	body, _ := json.Marshal(query)
	req, _ := http.NewRequest(http.MethodPost, server.URL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Abot-Tool-Status-Version", "1")
	req.Header.Set("X-Abot-Tool-Status-Signature", query.Signature)
	req.Header.Set("Idempotency-Key", query.QueryID)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || !strings.Contains(string(responseBody), "认证") {
		t.Fatalf("篡改 query 必须返回 401: status=%d body=%s", response.StatusCode, responseBody)
	}

	stale := query
	stale.WaitID = "wait-http"
	stale.IssuedAt = now.Add(-2 * time.Hour)
	stale, err = SignRuntimeLongRunningToolStatusQuery(stale, secret)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(stale)
	req, _ = http.NewRequest(http.MethodPost, server.URL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Abot-Tool-Status-Version", "1")
	req.Header.Set("X-Abot-Tool-Status-Signature", stale.Signature)
	req.Header.Set("Idempotency-Key", stale.QueryID)
	response, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("过期 query 必须返回 401: status=%d", response.StatusCode)
	}
}

func TestRuntimeLongRunningToolStatusRejectsResultDigestMismatch(t *testing.T) {
	now := time.Now().UTC()
	status := RuntimeLongRunningToolStatus{
		Version: RuntimeLongRunningToolStatusVersion, Source: "agent-runtime", Destination: "tool-runtime", QueryID: "q", InvocationID: "i", WaitID: "w", ToolName: "tool", RequestDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: RuntimeLongRunningToolStatusSucceeded, Result: map[string]any{"ok": true}, ResultDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ObservedAt: now,
	}
	if _, err := NormalizeRuntimeLongRunningToolStatus(status); !errors.Is(err, ErrRuntimeLongRunningToolStatusAuth) {
		t.Fatalf("result digest 冲突必须 fail-closed 为 auth 错误: %v", err)
	}
}
