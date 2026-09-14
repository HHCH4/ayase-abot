package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/provider"
	"Abot/internal/storage/sqlite"
	"google.golang.org/adk/v2/session"
)

func TestWorkflowContinuationAPIIsStrictAndMetadataOnly(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registry, err := provider.NewRegistry(ctx, store.ProviderRepository(), map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{SessionService: session.InMemoryService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	now := time.Now().UTC().Truncate(time.Microsecond)
	source := agentruntime.Invocation{ID: "source-http-continuation", UserID: "user", BotID: "bot", ConversationID: "conversation-http", WorkspaceID: "workspace", TargetPath: "/workspace/project", SessionID: "session-http", ProviderID: "provider", ModelID: "model", ConfigSnapshot: `{"version":1}`, Message: "api_key=source-secret must not be echoed", Status: agentruntime.InvocationFailed, Error: "runtime_interrupted: restart recovery", CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, source); err != nil {
		t.Fatal(err)
	}
	plan := agentruntime.TaskPlan{ID: "plan-http-source", InvocationID: source.ID, Revision: 2, Status: agentruntime.PlanBlocked, Blocker: "runtime_interrupted: unknown side effect", Steps: []agentruntime.PlanStep{
		{ID: "inspect", Title: "检查", Status: agentruntime.PlanStepCompleted, Evidence: []agentruntime.EvidenceRef{{Kind: "event", Ref: "inspect-http"}}, UpdatedAt: now},
		{ID: "edit", Title: "修改", Status: agentruntime.PlanStepBlocked, DependsOn: []string{"inspect"}, Blocker: "runtime_interrupted: unknown side effect", UpdatedAt: now},
		{ID: "verify", Title: "验证", Status: agentruntime.PlanStepPending, DependsOn: []string{"edit"}, UpdatedAt: now},
	}, CreatedAt: now, UpdatedAt: now}
	if err := repo.(interface {
		CreateTaskPlan(context.Context, agentruntime.TaskPlan) error
	}).CreateTaskPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, agentruntime.AgentEvent{ID: "http-recovery-plan", InvocationID: source.ID, Type: agentruntime.EventPlanUpdated, Timestamp: now, Data: map[string]any{"reason": "runtime_recovery"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, agentruntime.AgentEvent{ID: "http-recovery-failed", InvocationID: source.ID, Type: agentruntime.EventInvocationFailed, Timestamp: now, Data: map[string]any{"reason": "runtime_recovery"}}); err != nil {
		t.Fatal(err)
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Start(ctx); err != nil {
		coordinator.Close()
		t.Fatal(err)
	}
	defer func() { _ = coordinator.Close() }()
	server := NewServer(nil, nil)
	server.SetRuntimeCoordinator(coordinator)
	handler := server.Handler()
	body := `{"expected_plan_revision":2,"decision":"retry_step","step_id":"edit","message":"继续剩余步骤","idempotency_key":"http-continuation"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/invocations/"+source.ID+"/continuations", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", "header-continuation")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusAccepted {
		t.Fatalf("continuation API 应返回 202: status=%d body=%s", response.Code, response.Body.String())
	}
	var child agentruntime.Invocation
	if err := json.Unmarshal(response.Body.Bytes(), &child); err != nil {
		t.Fatal(err)
	}
	if child.ID == "" || child.ParentInvocationID != source.ID || child.ContinuationDecision != agentruntime.WorkflowContinuationRetryStep || child.ContinuationSourcePlanRevision != 2 || child.Message != "继续剩余步骤" {
		t.Fatalf("HTTP child metadata 错误: %#v", child)
	}
	if strings.Contains(response.Body.String(), "source-secret") || strings.Contains(response.Body.String(), "header-continuation") || strings.Contains(response.Body.String(), "ConfigSnapshot") {
		t.Fatalf("HTTP 响应不得泄露 source 正文、幂等键或配置正文: %s", response.Body.String())
	}
	if location := response.Header().Get("Location"); location != "/api/v1/invocations/"+child.ID {
		t.Fatalf("Location 应指向 child: %q", location)
	}
	trailing := httptest.NewRequest(http.MethodPost, "/api/v1/invocations/"+source.ID+"/continuations", strings.NewReader(body+" {}"))
	trailingResponse := httptest.NewRecorder()
	handler.ServeHTTP(trailingResponse, trailing)
	if trailingResponse.Code != http.StatusBadRequest {
		t.Fatalf("尾部第二个 JSON 应返回 400: status=%d body=%s", trailingResponse.Code, trailingResponse.Body.String())
	}
	unknown := httptest.NewRequest(http.MethodPost, "/api/v1/invocations/"+source.ID+"/continuations", strings.NewReader(`{"expected_plan_revision":2,"decision":"retry_step","step_id":"edit","unknown":true}`))
	unknownResponse := httptest.NewRecorder()
	handler.ServeHTTP(unknownResponse, unknown)
	if unknownResponse.Code != http.StatusBadRequest {
		t.Fatalf("未知字段应返回 400: status=%d body=%s", unknownResponse.Code, unknownResponse.Body.String())
	}
	missingEvidence := httptest.NewRequest(http.MethodPost, "/api/v1/invocations/"+source.ID+"/continuations", strings.NewReader(`{"expected_plan_revision":2,"decision":"complete_step","step_id":"edit","idempotency_key":"missing-evidence"}`))
	missingResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingResponse, missingEvidence)
	if missingResponse.Code != http.StatusBadRequest {
		t.Fatalf("complete 缺失 evidence 应返回 400: status=%d body=%s", missingResponse.Code, missingResponse.Body.String())
	}
	if _, err := repo.GetInvocation(ctx, source.ID); err != nil || errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("source 必须仍然存在: %v", err)
	}
	// The continuation is launched asynchronously by the coordinator. Wait for
	// the child worker to finish before closing the SQLite store; otherwise the
	// worker can reopen/create SQLite files while t.TempDir is being removed,
	// which makes the race suite report a cleanup failure.
	waitWorkflowContinuationTerminal(t, repo, child.ID)
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	// Close cancels the worker context, but lease release and the final
	// goroutine cleanup run in defers. Give those defers a bounded settling
	// window before the deferred store.Close and t.TempDir cleanup execute.
	time.Sleep(50 * time.Millisecond)
}

func waitWorkflowContinuationTerminal(t *testing.T, repo agentruntime.Repository, invocationID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		invocation, err := repo.GetInvocation(context.Background(), invocationID)
		if err == nil && invocation.Status.Terminal() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("continuation child 未在期限内进入终态: id=%s status=%s err=%v", invocationID, invocation.Status, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
