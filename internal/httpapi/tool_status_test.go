package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/provider"
	"Abot/internal/storage/sqlite"
)

func TestInvocationToolStatusAPIIsReadOnlyAndBoundToWaitID(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	providers, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{SessionService: store.SessionService(), Providers: providers})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	now := time.Now().UTC()
	invocation := agentruntime.Invocation{ID: "http-tool-status", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(context.Background(), agentruntime.AgentEvent{ID: "http-tool-request", InvocationID: invocation.ID, Type: agentruntime.EventToolRequested, Timestamp: now, Data: map[string]any{
		"call_id": "wait-http", "name": "wait_for_external_result", "args": map[string]any{"job_id": "job-http"},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(context.Background(), agentruntime.AgentEvent{ID: "http-tool-waiting", InvocationID: invocation.ID, Type: agentruntime.EventInvocationWaiting, Timestamp: now.Add(time.Second), Data: map[string]any{
		"reason": "tool", "tool_call_ids": []string{"wait-http"},
	}}); err != nil {
		t.Fatal(err)
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	if err := coordinator.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	coordinator.SetRuntimeLongRunningToolStatusResolver(agentruntime.RuntimeLongRunningToolStatusResolverFunc(func(_ context.Context, _ agentruntime.RuntimeLongRunningToolStatusQuery) (agentruntime.RuntimeLongRunningToolStatus, error) {
		return agentruntime.RuntimeLongRunningToolStatus{Status: agentruntime.RuntimeLongRunningToolStatusUnknown, Reason: "external timeout", ObservedAt: now}, nil
	}))
	server := NewServer(nil, nil)
	server.SetRuntimeCoordinator(coordinator)
	response := callHTTP(server.Handler(), http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/tool-status?wait_id=wait-http", "")
	if response.Code != http.StatusOK {
		t.Fatalf("tool status API 状态码=%d body=%s", response.Code, response.Body.String())
	}
	var status agentruntime.RuntimeLongRunningToolStatus
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Status != agentruntime.RuntimeLongRunningToolStatusUnknown || status.WaitID != "wait-http" {
		t.Fatalf("tool status API 响应错误: %#v", status)
	}
	current, err := coordinator.GetInvocation(context.Background(), invocation.ID)
	if err != nil || current.Status != agentruntime.InvocationWaitingTool {
		t.Fatalf("tool status API 不得改变 Invocation: current=%#v err=%v", current, err)
	}
	wrong := callHTTP(server.Handler(), http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/tool-status?wait_id=wrong", "")
	if wrong.Code != http.StatusConflict || !strings.Contains(wrong.Body.String(), "目标") {
		t.Fatalf("错误 wait_id 应返回 409: status=%d body=%s", wrong.Code, wrong.Body.String())
	}
	post := callHTTP(server.Handler(), http.MethodPost, "/api/v1/invocations/"+invocation.ID+"/tool-status", `{"wait_id":"wait-http"}`)
	if post.Code != http.StatusOK {
		t.Fatalf("POST tool status API 应兼容: status=%d body=%s", post.Code, post.Body.String())
	}
	reconcile := callHTTP(server.Handler(), http.MethodPost, "/api/v1/invocations/"+invocation.ID+"/tool-status/reconcile", `{"wait_id":"wait-http"}`)
	if reconcile.Code != http.StatusOK || !strings.Contains(reconcile.Body.String(), "unknown_requires_manual_recovery") {
		t.Fatalf("未知状态的显式 reconcile 应保持只读: status=%d body=%s", reconcile.Code, reconcile.Body.String())
	}
	current, err = coordinator.GetInvocation(context.Background(), invocation.ID)
	if err != nil || current.Status != agentruntime.InvocationWaitingTool {
		t.Fatalf("未知状态 reconcile 不应改变 Invocation: current=%#v err=%v", current, err)
	}
	batch := callHTTP(server.Handler(), http.MethodPost, "/api/v1/invocations/"+invocation.ID+"/tool-status/reconcile", `{"wait_ids":["wait-http"]}`)
	if batch.Code != http.StatusOK || !strings.Contains(batch.Body.String(), "unproven_status_no_resume") {
		t.Fatalf("批量 reconcile 应返回保守状态: status=%d body=%s", batch.Code, batch.Body.String())
	}
}
