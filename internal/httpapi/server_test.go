package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/bot"
	"Abot/internal/conversation"
	"Abot/internal/memory"
	"Abot/internal/provider"
	"Abot/internal/provider/openai"
	"Abot/internal/storage/sqlite"
	"Abot/internal/workspace"
	"github.com/gorilla/websocket"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

type httpCatalogTool struct{}

func (httpCatalogTool) Name() string        { return "read_catalog" }
func (httpCatalogTool) Description() string { return "读取目录" }
func (httpCatalogTool) IsLongRunning() bool { return false }
func (httpCatalogTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: "read_catalog"}
}

type httpCapabilityProbeAdapter struct{}

func (httpCapabilityProbeAdapter) BuildModel(context.Context, provider.Provider, string) (adkmodel.LLM, error) {
	return httpCapabilityProbeLLM{}, nil
}

func (httpCapabilityProbeAdapter) TestConnection(context.Context, provider.Provider) (provider.ProbeResult, error) {
	return provider.ProbeResult{Message: "ok"}, nil
}

func (httpCapabilityProbeAdapter) DiscoverModels(context.Context, provider.Provider) ([]provider.Model, error) {
	return nil, nil
}

func (httpCapabilityProbeAdapter) ProbeCapabilities(ctx context.Context, p provider.Provider, m provider.Model, options provider.CapabilityProbeOptions) (provider.CapabilityProbeResult, error) {
	profile := provider.DefaultCapabilities(p, m)
	profile.ToolCalling = provider.Support{State: provider.SupportUnknown, Source: "catalog"}
	profile.Streaming = provider.Support{State: provider.SupportUnknown, Source: "catalog"}
	profile.StructuredOutput = provider.Support{State: provider.SupportUnknown, Source: "catalog"}
	profile.Route = "test"
	result, err := provider.RunCapabilityProbe(ctx, httpCapabilityProbeLLM{}, profile, options)
	if err != nil || !options.IncludeTokenCount {
		return result, err
	}
	provider.ApplyTokenCountProbe(&result, profile.Route, provider.TokenCount{
		Tokens: 11, Name: "test-counter", Version: "v1", Source: "provider", Quality: "provider", Exact: true,
	}, nil)
	return result, nil
}

type httpCapabilityProbeLLM struct{}

func (httpCapabilityProbeLLM) Name() string { return "http-capability-probe" }

func (httpCapabilityProbeLLM) GenerateContent(_ context.Context, request *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if request != nil && request.Config != nil && len(request.Config.Tools) > 0 {
			yield(&adkmodel.LLMResponse{Content: genai.NewContentFromParts([]*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "abot_capability_probe_noop"}}}, genai.RoleModel)}, nil)
			return
		}
		value := "OK"
		if request != nil && request.Config != nil && request.Config.ResponseMIMEType == "application/json" {
			value = "{}"
		}
		if request != nil && request.Config != nil && request.Config.ResponseJsonSchema != nil {
			value = `{"ok":true}`
		}
		if request != nil && request.Config != nil && request.Config.ThinkingConfig != nil {
			yield(&adkmodel.LLMResponse{Content: genai.NewContentFromParts([]*genai.Part{{Thought: true}, genai.NewPartFromText(value)}, genai.RoleModel), UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 1, CandidatesTokenCount: 1, TotalTokenCount: 2, ThoughtsTokenCount: 1}}, nil)
			return
		}
		yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText(value, genai.RoleModel), UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 1, CandidatesTokenCount: 1, TotalTokenCount: 2}}, nil)
	}
}

func TestInvocationPlanAPIUsesRevisionCAS(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{SessionService: store.SessionService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	now := time.Now().UTC()
	invocation := agentruntime.Invocation{ID: "inv-plan-api", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	server := NewServer(nil, nil)
	server.SetRuntimeCoordinator(coordinator)
	handler := server.Handler()
	got := callHTTP(handler, http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/plan", "")
	if got.Code != http.StatusOK {
		t.Fatalf("读取计划状态码=%d body=%s", got.Code, got.Body.String())
	}
	var plan agentruntime.TaskPlan
	if err := json.Unmarshal(got.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	update := `{"revision":1,"steps":[{"id":"inspect","title":"检查","status":"in_progress"}],"reason":"开始"}`
	updated := callHTTP(handler, http.MethodPut, "/api/v1/invocations/"+invocation.ID+"/plan", update)
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"revision":2`) {
		t.Fatalf("更新计划响应=%d body=%s", updated.Code, updated.Body.String())
	}
	stale := callHTTP(handler, http.MethodPut, "/api/v1/invocations/"+invocation.ID+"/plan", update)
	if stale.Code != http.StatusConflict {
		t.Fatalf("旧版本计划状态码=%d body=%s", stale.Code, stale.Body.String())
	}
}

func TestInvocationResultAPIProjectsConservativeCompletionReport(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo := store.RuntimeRepository()
	now := time.Now().UTC()
	invocation := agentruntime.Invocation{ID: "inv-result-api", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: agentruntime.InvocationCompleted, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	contract := agentruntime.InitialTaskContract(invocation.ID, "修复问题", now)
	contract.AcceptanceCriteria = []agentruntime.Criterion{{ID: "tests", Description: "测试通过"}}
	contractRepo, ok := repo.(agentruntime.TaskContractRepository)
	if !ok {
		t.Fatal("SQLite Runtime Repository 未实现 TaskContractRepository")
	}
	if err := contractRepo.CreateTaskContract(context.Background(), contract); err != nil {
		t.Fatal(err)
	}
	plan := agentruntime.TaskPlan{ID: "plan-result-api", InvocationID: invocation.ID, Revision: 1, Status: agentruntime.PlanCompleted, Steps: []agentruntime.PlanStep{{ID: "done", Title: "已完成", Status: agentruntime.PlanStepCompleted, Evidence: []agentruntime.EvidenceRef{{Kind: "operation", Ref: "operation-api"}}, UpdatedAt: now}}, CreatedAt: now, UpdatedAt: now}
	planRepo, ok := repo.(agentruntime.TaskPlanRepository)
	if !ok {
		t.Fatal("SQLite Runtime Repository 未实现 TaskPlanRepository")
	}
	if err := planRepo.CreateTaskPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: openai.NewAdapter(nil)})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{SessionService: store.SessionService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	server := NewServer(nil, nil)
	server.SetRuntimeCoordinator(coordinator)
	response := callHTTP(server.Handler(), http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/result", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"partial"`) || !strings.Contains(response.Body.String(), `"criteria"`) {
		t.Fatalf("result API 应保守投影验收缺口: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestInvocationBaselineAPIProjectsPreexistingChanges(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo := store.RuntimeRepository()
	now := time.Now().UTC()
	invocation := agentruntime.Invocation{ID: "inv-baseline-api", UserID: "user", ConversationID: "conversation", SessionID: "conversation", WorkspaceID: "workspace", Status: agentruntime.InvocationCompleted, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	baselineRepo, ok := repo.(agentruntime.WorktreeBaselineRepository)
	if !ok {
		t.Fatal("SQLite Runtime Repository 未实现 WorktreeBaselineRepository")
	}
	if _, err := baselineRepo.SaveWorkspaceBaseline(context.Background(), agentruntime.WorktreeBaseline{InvocationID: invocation.ID, WorkspaceID: invocation.WorkspaceID, RepositoryType: "git", HeadRevision: "abc", Branch: "main", StatusDigest: "sha256:baseline", ChangedPaths: []agentruntime.PathStatus{{Path: "README.md", Status: " M"}}, StatusKnown: true, CapturedAt: now}); err != nil {
		t.Fatal(err)
	}
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{SessionService: store.SessionService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	response := callHTTP((&Server{runtime: coordinator}).Handler(), http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/baseline", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"repository_type":"git"`) || !strings.Contains(response.Body.String(), `"README.md"`) {
		t.Fatalf("baseline API 响应不正确: status=%d body=%s", response.Code, response.Body.String())
	}
	reportResponse := callHTTP((&Server{runtime: coordinator}).Handler(), http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/result", "")
	if reportResponse.Code != http.StatusOK || !strings.Contains(reportResponse.Body.String(), `"preexisting_changes"`) || !strings.Contains(reportResponse.Body.String(), `"baseline"`) {
		t.Fatalf("result API 未投影 baseline 归因: status=%d body=%s", reportResponse.Code, reportResponse.Body.String())
	}
}

func TestInvocationContractAPIUsesVersionCASAndKeepsHistory(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{SessionService: store.SessionService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	now := time.Now().UTC()
	invocation := agentruntime.Invocation{ID: "inv-contract-api", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Message: "请修复问题", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	server := NewServer(nil, nil)
	server.SetRuntimeCoordinator(coordinator)
	handler := server.Handler()
	initial := callHTTP(handler, http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/contract", "")
	if initial.Code != http.StatusOK {
		t.Fatalf("读取任务契约状态码=%d body=%s", initial.Code, initial.Body.String())
	}
	var contract agentruntime.TaskContract
	if err := json.Unmarshal(initial.Body.Bytes(), &contract); err != nil {
		t.Fatal(err)
	}
	if contract.Version != 1 || !contract.MutationAllowed {
		t.Fatalf("初始任务契约=%#v", contract)
	}
	update := `{"version":1,"task_type":"build","goal":"修复问题","requested_outcome":"修复并验证","mutation_allowed":true,"validation_required":true,"acceptance_criteria":[{"id":"tests","description":"目标测试通过"}],"constraints":[{"description":"不提交 git","source":"user"}],"non_goals":["不改无关文件"],"external_actions":[{"action":"git push","allowed":false}],"reason":"补充验收条件"}`
	updated := callHTTP(handler, http.MethodPut, "/api/v1/invocations/"+invocation.ID+"/contract", update)
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"version":2`) {
		t.Fatalf("更新任务契约响应=%d body=%s", updated.Code, updated.Body.String())
	}
	stale := callHTTP(handler, http.MethodPut, "/api/v1/invocations/"+invocation.ID+"/contract", update)
	if stale.Code != http.StatusConflict {
		t.Fatalf("过期任务契约状态码=%d body=%s", stale.Code, stale.Body.String())
	}
	history := callHTTP(handler, http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/contracts", "")
	if history.Code != http.StatusOK || !strings.Contains(history.Body.String(), `"version":1`) || !strings.Contains(history.Body.String(), `"version":2`) {
		t.Fatalf("任务契约历史响应=%d body=%s", history.Code, history.Body.String())
	}
}

func TestInvocationRuntimeSnapshotAPI(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{SessionService: store.SessionService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	now := time.Now().UTC()
	invocation := agentruntime.Invocation{ID: "inv-snapshot-api", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	if _, err := coordinator.RebuildRuntimeSnapshot(context.Background(), invocation.ID); err != nil {
		t.Fatal(err)
	}
	server := NewServer(nil, nil)
	server.SetRuntimeCoordinator(coordinator)
	got := callHTTP(server.Handler(), http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/runtime-snapshot", "")
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"phase":"queued"`) || !strings.Contains(got.Body.String(), `"revision":1`) {
		t.Fatalf("Runtime Snapshot API 响应=%d body=%s", got.Code, got.Body.String())
	}
}

func TestInvocationToolSetSnapshotAPI(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{SessionService: store.SessionService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	now := time.Now().UTC()
	invocation := agentruntime.Invocation{ID: "inv-toolset-api", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	snapshotRepo, ok := repo.(agentruntime.ToolSetSnapshotRepository)
	if !ok {
		t.Fatal("SQLite repository 未实现 ToolSetSnapshotRepository")
	}
	if _, err := snapshotRepo.SaveToolSetSnapshot(context.Background(), agentruntime.ToolSetSnapshot{ID: "toolset-api", InvocationID: invocation.ID, CatalogRevision: "catalog", Tools: []agentruntime.ToolRef{{ID: "builtin.calculate", ModelName: "calculate", Version: "1.0.0", SchemaDigest: "schema"}}, Digest: "digest", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(nil, nil)
	server.SetRuntimeCoordinator(func() *agentruntime.Coordinator {
		coordinator, coordinatorErr := agentruntime.NewCoordinator(kernel, repo)
		if coordinatorErr != nil {
			t.Fatal(coordinatorErr)
		}
		return coordinator
	}())
	got := callHTTP(server.Handler(), http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/toolset", "")
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"digest":"digest"`) || !strings.Contains(got.Body.String(), `"model_name":"calculate"`) {
		t.Fatalf("ToolSet Snapshot API 响应=%d body=%s", got.Code, got.Body.String())
	}
}

func TestToolCatalogAPIExposesOnlyPublicDescriptors(t *testing.T) {
	toolRegistry := agent.NewToolRegistry()
	if _, err := toolRegistry.RegisterRuntimeTool(httpCatalogTool{}, agent.ToolSourceBuiltin); err != nil {
		t.Fatal(err)
	}
	server := NewServer(nil, nil)
	server.SetToolRegistry(toolRegistry)
	list := callHTTP(server.Handler(), http.MethodGet, "/api/v1/tools?q=read&limit=10", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"model_name":"read_catalog"`) || strings.Contains(list.Body.String(), `"input_schema"`) {
		t.Fatalf("工具目录 API 响应=%d body=%s", list.Code, list.Body.String())
	}
	detail := callHTTP(server.Handler(), http.MethodGet, "/api/v1/tools/"+"builtin.read_catalog", "")
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"id":"builtin.read_catalog"`) {
		t.Fatalf("工具详情 API 响应=%d body=%s", detail.Code, detail.Body.String())
	}
}

func TestInvocationTraceAndUsageAPIExposeMetadataOnly(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{SessionService: store.SessionService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	now := time.Now().UTC()
	finished := now.Add(2 * time.Second)
	invocation := agentruntime.Invocation{ID: "inv-trace-api", UserID: "user", ConversationID: "conversation", SessionID: "conversation", ProviderID: "demo", ModelID: "model", Status: agentruntime.InvocationCompleted, CreatedAt: now, UpdatedAt: finished, FinishedAt: &finished}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(context.Background(), agentruntime.AgentEvent{ID: "trace-event", InvocationID: invocation.ID, Type: agentruntime.EventUsageUpdated, Timestamp: now.Add(time.Millisecond), Data: map[string]any{"usage": map[string]any{"promptTokenCount": 3, "candidatesTokenCount": 2, "totalTokenCount": 5, "prompt": "secret"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(context.Background(), agentruntime.AgentEvent{ID: "trace-compaction-event", InvocationID: invocation.ID, Type: agentruntime.EventUsageUpdated, Timestamp: now.Add(2 * time.Millisecond), Data: map[string]any{"scope": "compaction", "usage": map[string]any{"promptTokenCount": 7, "candidatesTokenCount": 1, "totalTokenCount": 8, "summary": "secret"}}}); err != nil {
		t.Fatal(err)
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	server := NewServer(nil, nil)
	server.SetRuntimeCoordinator(coordinator)
	trace := callHTTP(server.Handler(), http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/trace", "")
	if trace.Code != http.StatusOK || strings.Contains(trace.Body.String(), "secret") || !strings.Contains(trace.Body.String(), `"model_calls":1`) || !strings.Contains(trace.Body.String(), `"compaction_model_calls":1`) {
		t.Fatalf("Trace API 响应=%d body=%s", trace.Code, trace.Body.String())
	}
	usage := callHTTP(server.Handler(), http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/usage", "")
	if usage.Code != http.StatusOK || !strings.Contains(usage.Body.String(), `"known":true`) || !strings.Contains(usage.Body.String(), `"model_calls":1`) || !strings.Contains(usage.Body.String(), `"compaction"`) || strings.Contains(usage.Body.String(), "secret") {
		t.Fatalf("Usage API 响应=%d body=%s", usage.Code, usage.Body.String())
	}
}

func TestInvocationInstructionReconfirmAPIKeepsApprovalPending(t *testing.T) {
	repo := agentruntime.NewMemoryRepository()
	now := time.Now().UTC()
	invocation := agentruntime.Invocation{ID: "inv-instruction-reconfirm-api", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: agentruntime.InvocationWaitingApproval, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateApproval(context.Background(), agentruntime.Approval{ID: "approval-instruction-reconfirm-api", InvocationID: invocation.ID, Status: agentruntime.ApprovalPending, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	// Construct through the public constructor so the route exercises the same
	// lock and repository checks as production.
	// A nil Kernel is rejected by NewCoordinator; this API test does not need a
	// model, so use a minimal in-memory Kernel with an empty provider registry.
	providers, err := provider.NewRegistry(context.Background(), httpEmptyProviderRepository{}, map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{SessionService: session.InMemoryService(), Providers: providers})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	coordinator.SetInstructionSnapshotReconfirmer(func(_ context.Context, invocationID string) (agentruntime.InstructionSnapshotSet, error) {
		return repo.ReplaceInstructionSnapshotSet(context.Background(), agentruntime.InstructionSnapshotSet{InvocationID: invocationID, Snapshots: []agentruntime.InstructionSnapshot{{Path: "AGENTS.md", ContentDigest: "new"}}})
	})
	server := NewServer(nil, nil)
	server.SetRuntimeCoordinator(coordinator)
	response := callHTTP(server.Handler(), http.MethodPost, "/api/v1/invocations/"+invocation.ID+"/instructions/reconfirm", "{}")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"revision":1`) || !strings.Contains(response.Body.String(), `"content_digest":"new"`) {
		t.Fatalf("重新确认 API 响应=%d body=%s", response.Code, response.Body.String())
	}
	approval, err := repo.GetApproval(context.Background(), "approval-instruction-reconfirm-api")
	if err != nil || approval.Status != agentruntime.ApprovalPending {
		t.Fatalf("API 重新确认不应决议审批=%#v err=%v", approval, err)
	}
}

func TestInvocationResumeAPIValidatesWaitIDAndIsIdempotent(t *testing.T) {
	repo := agentruntime.NewMemoryRepository()
	now := time.Now().UTC()
	invocation := agentruntime.Invocation{ID: "inv-resume-api", UserID: "user", ConversationID: "resume-api-conversation", SessionID: "resume-api-conversation", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(context.Background(), agentruntime.AgentEvent{ID: "tool-requested-resume-api", InvocationID: invocation.ID, Type: agentruntime.EventToolRequested, Timestamp: now, Data: map[string]any{"call_id": "wait-api", "name": "wait_for_external_result"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(context.Background(), agentruntime.AgentEvent{ID: "tool-waiting-resume-api", InvocationID: invocation.ID, Type: agentruntime.EventInvocationWaiting, Timestamp: now.Add(time.Millisecond), Data: map[string]any{"reason": "tool", "tool_call_ids": []string{"wait-api"}}}); err != nil {
		t.Fatal(err)
	}
	providers, err := provider.NewRegistry(context.Background(), httpEmptyProviderRepository{}, map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{SessionService: session.InMemoryService(), Providers: providers})
	if err != nil {
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
	server := NewServer(nil, nil)
	server.SetRuntimeCoordinator(coordinator)
	handler := server.Handler()
	invalid := callHTTP(handler, http.MethodPost, "/api/v1/invocations/"+invocation.ID+"/resume", `{"wait_id":"wait-api","response":{},"unknown":true}`)
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), "请求体无效") {
		t.Fatalf("恢复 API 应拒绝未知字段: %d %s", invalid.Code, invalid.Body.String())
	}
	unknown := callHTTP(handler, http.MethodPost, "/api/v1/invocations/"+invocation.ID+"/resume", `{"wait_id":"unknown","name":"wait_for_external_result","response":{},"idempotency_key":"resume-api-1"}`)
	if unknown.Code != http.StatusConflict || !strings.Contains(unknown.Body.String(), "wait_id") {
		t.Fatalf("恢复 API 应拒绝未知 wait_id: %d %s", unknown.Code, unknown.Body.String())
	}
	accepted := callHTTP(handler, http.MethodPost, "/api/v1/invocations/"+invocation.ID+"/resume", `{"wait_id":"wait-api","name":"wait_for_external_result","response":{"output":"ok"},"boundary_id":"boundary:inv-resume-api:2","idempotency_key":"resume-api-1"}`)
	if accepted.Code != http.StatusAccepted || !strings.Contains(accepted.Body.String(), `"status":"queued"`) {
		t.Fatalf("恢复 API 响应异常: %d %s", accepted.Code, accepted.Body.String())
	}
	duplicate := callHTTP(handler, http.MethodPost, "/api/v1/invocations/"+invocation.ID+"/resume", `{"wait_id":"wait-api","name":"wait_for_external_result","response":{"output":"ok"},"boundary_id":"boundary:inv-resume-api:2","idempotency_key":"resume-api-1"}`)
	if duplicate.Code != http.StatusAccepted || !strings.Contains(duplicate.Body.String(), invocation.ID) {
		t.Fatalf("重复恢复 API 应幂等: %d %s", duplicate.Code, duplicate.Body.String())
	}
	events, err := repo.ListEvents(context.Background(), invocation.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	resumed := 0
	for _, event := range events {
		if event.Type == agentruntime.EventInvocationResumed && event.Data != nil && event.Data["reason"] == "tool" {
			resumed++
		}
	}
	if resumed != 1 {
		t.Fatalf("重复恢复不应追加第二个 resumed 事件: %d events=%#v", resumed, events)
	}

	batchInvocation := agentruntime.Invocation{ID: "inv-resume-api-batch", UserID: "user", ConversationID: "resume-api-batch-conversation", SessionID: "resume-api-batch-conversation", Status: agentruntime.InvocationWaitingTool, CreatedAt: now.Add(2 * time.Second), UpdatedAt: now.Add(2 * time.Second)}
	if err := repo.CreateInvocation(context.Background(), batchInvocation); err != nil {
		t.Fatal(err)
	}
	for _, event := range []agentruntime.AgentEvent{
		{ID: "tool-requested-resume-api-batch-a", InvocationID: batchInvocation.ID, Type: agentruntime.EventToolRequested, Timestamp: now, Data: map[string]any{"call_id": "wait-api-batch-a", "name": "wait_for_alpha"}},
		{ID: "tool-requested-resume-api-batch-b", InvocationID: batchInvocation.ID, Type: agentruntime.EventToolRequested, Timestamp: now.Add(time.Millisecond), Data: map[string]any{"call_id": "wait-api-batch-b", "name": "wait_for_beta"}},
		{ID: "tool-waiting-resume-api-batch", InvocationID: batchInvocation.ID, Type: agentruntime.EventInvocationWaiting, Timestamp: now.Add(2 * time.Millisecond), Data: map[string]any{"reason": "tool", "tool_call_ids": []string{"wait-api-batch-a", "wait-api-batch-b"}}},
	} {
		if _, err := repo.AppendEvent(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	partial := callHTTP(handler, http.MethodPost, "/api/v1/invocations/"+batchInvocation.ID+"/resume", `{"responses":[{"wait_id":"wait-api-batch-a","response":{"ok":true}}],"idempotency_key":"resume-api-batch-1"}`)
	if partial.Code != http.StatusConflict || !strings.Contains(partial.Body.String(), "必须") {
		t.Fatalf("批量等待只恢复一项应拒绝: %d %s", partial.Code, partial.Body.String())
	}
	batch := callHTTP(handler, http.MethodPost, "/api/v1/invocations/"+batchInvocation.ID+"/resume", `{"responses":[{"wait_id":"wait-api-batch-a","response":{"ok":true}},{"wait_id":"wait-api-batch-b","name":"wait_for_beta","response":{"ok":true}}],"boundary_id":"boundary:inv-resume-api-batch:3","idempotency_key":"resume-api-batch-1"}`)
	if batch.Code != http.StatusAccepted || !strings.Contains(batch.Body.String(), `"status":"queued"`) {
		t.Fatalf("批量恢复 API 响应异常: %d %s", batch.Code, batch.Body.String())
	}
	batchEvents, err := repo.ListEvents(context.Background(), batchInvocation.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var batchResumed bool
	for _, event := range batchEvents {
		if event.Type != agentruntime.EventInvocationResumed || event.Data == nil || event.Data["reason"] != "tool" {
			continue
		}
		ids := event.Data["wait_ids"]
		if got, ok := ids.([]string); ok && len(got) == 2 && got[0] == "wait-api-batch-a" && got[1] == "wait-api-batch-b" {
			batchResumed = true
		}
	}
	if !batchResumed {
		t.Fatalf("批量恢复事件应包含完整 wait_ids: %#v", batchEvents)
	}
}

type httpEmptyProviderRepository struct{}

func (httpEmptyProviderRepository) List(context.Context) ([]provider.Provider, error) {
	return nil, nil
}
func (httpEmptyProviderRepository) Get(context.Context, string) (provider.Provider, error) {
	return provider.Provider{}, provider.ErrNotFound
}
func (httpEmptyProviderRepository) Save(context.Context, provider.Provider) error { return nil }
func (httpEmptyProviderRepository) Delete(context.Context, string) error          { return nil }
func (httpEmptyProviderRepository) GetDefault(context.Context) (provider.DefaultRef, error) {
	return provider.DefaultRef{}, nil
}
func (httpEmptyProviderRepository) SetDefault(context.Context, provider.DefaultRef) error { return nil }

func TestInvocationWorkingSetAPIUsesRevisionCAS(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{SessionService: store.SessionService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	now := time.Now().UTC()
	invocation := agentruntime.Invocation{ID: "inv-working-set-api", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	handler := NewServer(nil, nil)
	handler.SetRuntimeCoordinator(coordinator)
	get := callHTTP(handler.Handler(), http.MethodGet, "/api/v1/invocations/"+invocation.ID+"/working-set", "")
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"revision":1`) {
		t.Fatalf("读取 Working Set 响应=%d body=%s", get.Code, get.Body.String())
	}
	update := `{"revision":1,"items":[{"id":"slice","kind":"file_slice","source_ref":"file:a","priority":1,"relevance_score":0.5,"token_estimate":5,"content_digest":"sha","freshness":"fresh"}],"reason":"读取"}`
	put := callHTTP(handler.Handler(), http.MethodPut, "/api/v1/invocations/"+invocation.ID+"/working-set", update)
	if put.Code != http.StatusOK || !strings.Contains(put.Body.String(), `"revision":2`) {
		t.Fatalf("更新 Working Set 响应=%d body=%s", put.Code, put.Body.String())
	}
	stale := callHTTP(handler.Handler(), http.MethodPut, "/api/v1/invocations/"+invocation.ID+"/working-set", update)
	if stale.Code != http.StatusConflict {
		t.Fatalf("旧 Working Set 状态码=%d body=%s", stale.Code, stale.Body.String())
	}
}

func TestInvocationVerificationAPIUsesRevisionCAS(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{SessionService: store.SessionService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	now := time.Now().UTC()
	invocation := agentruntime.Invocation{ID: "inv-verification-api", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: agentruntime.InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	server := NewServer(nil, nil)
	server.SetRuntimeCoordinator(coordinator)
	handler := server.Handler()
	created := callHTTP(handler, http.MethodPost, "/api/v1/invocations/"+invocation.ID+"/verifications", `{"kind":"command","command":"go test","status":"running"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("创建 verification 响应=%d body=%s", created.Code, created.Body.String())
	}
	var run agentruntime.VerificationRun
	if err := json.Unmarshal(created.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.Revision != 1 || run.Status != agentruntime.VerificationRunning {
		t.Fatalf("创建 verification=%#v", run)
	}
	updated := callHTTP(handler, http.MethodPut, "/api/v1/invocations/"+invocation.ID+"/verifications/"+run.ID, `{"revision":1,"kind":"command","status":"passed","summary":"通过"}`)
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"revision":2`) {
		t.Fatalf("更新 verification 响应=%d body=%s", updated.Code, updated.Body.String())
	}
	stale := callHTTP(handler, http.MethodPut, "/api/v1/invocations/"+invocation.ID+"/verifications/"+run.ID, `{"revision":1,"kind":"command","status":"failed"}`)
	if stale.Code != http.StatusConflict {
		t.Fatalf("旧 verification 状态码=%d body=%s", stale.Code, stale.Body.String())
	}
}

func TestProviderAPIHidesAPIKeyAndCompletesDefaultFlow(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开 SQLite Store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: openai.NewAdapter(nil),
	})
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	handler := NewServer(registry, nil).Handler()

	create := callHTTP(handler, http.MethodPost, "/api/v1/providers", `{"id":"demo","name":"演示供应商","base_url":"http://127.0.0.1:9999/v1","protocol":"openai-compatible","openai_format":"responses","token_count_protocol":"anthropic","api_key":"secret-key","models":[{"id":"demo-model","enabled":true}]}`)
	if create.Code != http.StatusOK {
		t.Fatalf("创建供应商状态码 = %d，响应 = %s", create.Code, create.Body.String())
	}
	if strings.Contains(create.Body.String(), "secret-key") || strings.Contains(create.Body.String(), `"api_key"`) {
		t.Fatalf("供应商响应泄露 API key: %s", create.Body.String())
	}
	var view providerView
	if err := json.Unmarshal(create.Body.Bytes(), &view); err != nil {
		t.Fatalf("解析供应商响应失败: %v", err)
	}
	if !view.APIKeyConfigured || view.OpenAIFormat != provider.OpenAIFormatResponses || view.TokenCountProtocol != provider.TokenCountProtocolAnthropic {
		t.Fatalf("供应商公开视图不正确: %#v", view)
	}

	defaultResponse := callHTTP(handler, http.MethodPut, "/api/v1/settings/default-model", `{"provider_id":"demo","model_id":"demo-model"}`)
	if defaultResponse.Code != http.StatusOK {
		t.Fatalf("设置默认模型状态码 = %d，响应 = %s", defaultResponse.Code, defaultResponse.Body.String())
	}
	get := callHTTP(handler, http.MethodGet, "/api/v1/providers/demo", "")
	if get.Code != http.StatusOK {
		t.Fatalf("读取供应商状态码 = %d，响应 = %s", get.Code, get.Body.String())
	}
	if err := json.Unmarshal(get.Body.Bytes(), &view); err != nil {
		t.Fatalf("解析读取响应失败: %v", err)
	}
	if !view.IsDefault || view.Models[0].ID != "demo-model" {
		t.Fatalf("设置默认后的公开视图不正确: %#v", view)
	}

	// 编辑请求不携带 api_key，验证 API 层和 Registry 层共同保留旧密钥。
	update := callHTTP(handler, http.MethodPut, "/api/v1/providers/demo", `{"name":"演示供应商更新","base_url":"http://127.0.0.1:9999/v1","protocol":"openai-compatible","openai_format":"responses","token_count_protocol":"anthropic","models":[{"id":"demo-model","enabled":true}]}`)
	if update.Code != http.StatusOK {
		t.Fatalf("更新供应商状态码 = %d，响应 = %s", update.Code, update.Body.String())
	}
	stored, err := store.ProviderRepository().Get(context.Background(), "demo")
	if err != nil {
		t.Fatalf("读取更新后的供应商失败: %v", err)
	}
	if stored.APIKey != "secret-key" {
		t.Fatalf("编辑后 API key = %q，期望保留旧密钥", stored.APIKey)
	}
	if stored.TokenCountProtocol != provider.TokenCountProtocolAnthropic {
		t.Fatalf("编辑后 token count 协议 = %q，期望保留 Anthropic 显式线路", stored.TokenCountProtocol)
	}

	page := callHTTP(handler, http.MethodGet, "/", "")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Abot 管理台") {
		t.Fatalf("内嵌 WebUI 首页不可用: status=%d body=%s", page.Code, page.Body.String())
	}
}

func TestProviderModelCapabilitiesAPIReturnsSafeProfile(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开 SQLite Store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	p := provider.Provider{ID: "cap-demo", Name: "能力测试", BaseURL: "https://example.invalid/v1", Protocol: provider.ProtocolOpenAICompatible}
	m := provider.Model{ID: "cap-model", DisplayName: "能力模型", Enabled: true, ContextWindow: 32768, MaxOutputTokens: 4096}
	profile := provider.DefaultCapabilities(p, m)
	profile.ToolCalling = provider.Support{State: provider.SupportDegraded, Source: "probe", Confidence: 0.6, Reason: "部分支持"}
	profile.SourceRevision = "probe-revision"
	m.Capabilities = &profile
	p.Models = []provider.Model{m}
	if _, err := registry.Save(context.Background(), provider.SaveRequest{Provider: p, APIKey: stringPtr("cap-secret")}); err != nil {
		t.Fatalf("保存能力测试供应商失败: %v", err)
	}

	response := callHTTP(NewServer(registry, nil).Handler(), http.MethodGet, "/api/v1/providers/cap-demo/models/cap-model/capabilities", "")
	if response.Code != http.StatusOK {
		t.Fatalf("读取模型能力状态码=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "cap-secret") || strings.Contains(response.Body.String(), "api_key") {
		t.Fatalf("模型能力响应泄露供应商密钥: %s", response.Body.String())
	}
	var got provider.ModelCapabilityProfile
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析模型能力响应失败: %v", err)
	}
	if got.ProviderID != "cap-demo" || got.ModelID != "cap-model" || got.ToolCalling.State != provider.SupportDegraded || got.SourceRevision != "probe-revision" {
		t.Fatalf("模型能力 Profile 不正确: %#v", got)
	}

	missing := callHTTP(NewServer(registry, nil).Handler(), http.MethodGet, "/api/v1/providers/cap-demo/models/missing/capabilities", "")
	if missing.Code != http.StatusConflict {
		t.Fatalf("不存在模型状态码=%d body=%s", missing.Code, missing.Body.String())
	}
}

func TestProviderCapabilityProfilesAPIExportsDeterministicMetadataOnlyProjection(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开 SQLite Store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	profile := provider.DefaultCapabilities(provider.Provider{ID: "alpha", Protocol: provider.ProtocolOpenAICompatible, OpenAIFormat: provider.OpenAIFormatChat}, provider.Model{ID: "explicit"})
	profile.ToolCalling = provider.Support{State: provider.SupportDegraded, Source: "probe"}
	if _, err := registry.Save(context.Background(), provider.SaveRequest{Provider: provider.Provider{
		ID: "zeta", Name: "Zeta", BaseURL: "https://zeta.invalid/v1", Protocol: provider.ProtocolOpenAICompatible,
		Models: []provider.Model{{ID: "z-model", Enabled: true}},
	}, APIKey: stringPtr("zeta-secret")}); err != nil {
		t.Fatalf("保存 zeta 供应商失败: %v", err)
	}
	if _, err := registry.Save(context.Background(), provider.SaveRequest{Provider: provider.Provider{
		ID: "alpha", Name: "Alpha", BaseURL: "https://alpha.invalid/v1", Protocol: provider.ProtocolOpenAICompatible, OpenAIFormat: provider.OpenAIFormatChat,
		Models: []provider.Model{{ID: "z-model", Enabled: true}, {ID: "explicit", Enabled: true, Capabilities: &profile}},
	}, APIKey: stringPtr("alpha-secret")}); err != nil {
		t.Fatalf("保存 alpha 供应商失败: %v", err)
	}

	response := callHTTP(NewServer(registry, nil).Handler(), http.MethodGet, "/api/v1/providers/capability-profiles", "")
	if response.Code != http.StatusOK {
		t.Fatalf("读取能力 profile 导出状态码=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "alpha-secret") || strings.Contains(response.Body.String(), "zeta-secret") || strings.Contains(response.Body.String(), "api_key") || strings.Contains(response.Body.String(), "base_url") {
		t.Fatalf("能力 profile 导出泄露供应商配置: %s", response.Body.String())
	}
	var payload struct {
		Profiles []provider.ModelCapabilityProfile `json:"profiles"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析能力 profile 导出失败: %v", err)
	}
	if len(payload.Profiles) != 3 || payload.Profiles[0].ProviderID != "alpha" || payload.Profiles[0].ModelID != "explicit" || payload.Profiles[0].ToolCalling.State != provider.SupportDegraded || payload.Profiles[2].ProviderID != "zeta" {
		t.Fatalf("能力 profile 导出排序或内容不正确: %#v", payload.Profiles)
	}
}

func TestProviderModelCapabilityOverrideCannotUpgradeUnknown(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开 SQLite Store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	p := provider.Provider{ID: "override-demo", Name: "override", BaseURL: "https://example.invalid/v1", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true}}}
	if _, err := registry.Save(context.Background(), provider.SaveRequest{Provider: p, APIKey: stringPtr("override-secret")}); err != nil {
		t.Fatalf("保存供应商失败: %v", err)
	}
	handler := NewServer(registry, nil).Handler()
	degraded := `{"tool_calling":"degraded"}`
	response := callHTTP(handler, http.MethodPut, "/api/v1/providers/override-demo/models/model/capability-overrides", degraded)
	if response.Code != http.StatusOK {
		t.Fatalf("保存能力 override 状态码=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "override-secret") || strings.Contains(response.Body.String(), "api_key") {
		t.Fatalf("能力 override 响应泄露密钥: %s", response.Body.String())
	}
	var profile provider.ModelCapabilityProfile
	if err := json.Unmarshal(response.Body.Bytes(), &profile); err != nil {
		t.Fatalf("解析能力 override 响应失败: %v", err)
	}
	if profile.ToolCalling.State != provider.SupportDegraded || profile.ToolCalling.Source != "user_override" {
		t.Fatalf("能力 override 未生效: %#v", profile.ToolCalling)
	}
	upgrade := callHTTP(handler, http.MethodPut, "/api/v1/providers/override-demo/models/model/capability-overrides", `{"images":"supported"}`)
	if upgrade.Code != http.StatusBadRequest {
		t.Fatalf("不应允许声明 supported，状态码=%d body=%s", upgrade.Code, upgrade.Body.String())
	}
	if strings.Contains(upgrade.Body.String(), "override-secret") {
		t.Fatalf("错误响应泄露密钥: %s", upgrade.Body.String())
	}
	schemaUnsupported := callHTTP(handler, http.MethodPut, "/api/v1/providers/override-demo/models/model/capability-overrides", `{"structured_output_schema":"unsupported"}`)
	if schemaUnsupported.Code != http.StatusOK {
		t.Fatalf("schema 保守 override 状态码=%d body=%s", schemaUnsupported.Code, schemaUnsupported.Body.String())
	}
	if !strings.Contains(schemaUnsupported.Body.String(), `"structured_output_schema":{"state":"unsupported"`) {
		t.Fatalf("schema override 未出现在 profile: %s", schemaUnsupported.Body.String())
	}
}

func TestProviderModelCapabilityProbeAPIUsesBoundedMetadataOnlyResult(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开 SQLite Store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: httpCapabilityProbeAdapter{},
	})
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	if _, err := registry.Save(context.Background(), provider.SaveRequest{
		Provider: provider.Provider{ID: "probe-api", Name: "探测 API", BaseURL: "https://example.invalid/v1", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", Enabled: true}}},
		APIKey:   stringPtr("probe-secret"),
	}); err != nil {
		t.Fatalf("保存探测供应商失败: %v", err)
	}

	response := callHTTP(NewServer(registry, nil).Handler(), http.MethodPost, "/api/v1/providers/probe-api/models/model/probe", `{"include_streaming":false,"include_token_count":true}`)
	if response.Code != http.StatusOK {
		t.Fatalf("能力探测 API 状态码=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "probe-secret") || strings.Contains(response.Body.String(), "Reply with OK") || strings.Contains(response.Body.String(), "abot_capability_probe_noop") {
		t.Fatalf("能力探测响应泄露密钥或探测正文: %s", response.Body.String())
	}
	var result provider.CapabilityProbeResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("解析能力探测响应失败: %v", err)
	}
	if result.Profile.ProviderID != "probe-api" || result.Profile.ModelID != "model" || result.Profile.Streaming.State != provider.SupportUnknown || !result.Profile.Tokenizer.Known {
		t.Fatalf("能力探测结果未应用显式开关: %#v", result.Profile)
	}
	if findHTTPProbeObservation(result.Observations, "token_count").State != provider.SupportSupported {
		t.Fatalf("HTTP token-count 选项未透传: %#v", result.Observations)
	}
	stored, err := registry.Get("probe-api")
	if err != nil {
		t.Fatalf("读取探测后的供应商失败: %v", err)
	}
	if stored.Models[0].Capabilities == nil || stored.Models[0].Capabilities.SourceRevision == "" {
		t.Fatalf("能力探测 profile 未持久化: %#v", stored.Models)
	}
	history := callHTTP(NewServer(registry, nil).Handler(), http.MethodGet, "/api/v1/providers/probe-api/models/model/capability-observations?limit=2", "")
	if history.Code != http.StatusOK || !strings.Contains(history.Body.String(), `"observations"`) || strings.Contains(history.Body.String(), "Reply with OK") || strings.Contains(history.Body.String(), "abot_capability_probe_noop") {
		t.Fatalf("能力 observation 历史查询不正确或泄露探测数据: status=%d body=%s", history.Code, history.Body.String())
	}
	invalid := callHTTP(NewServer(registry, nil).Handler(), http.MethodPost, "/api/v1/providers/probe-api/models/model/probe", `{"unexpected":true}`)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("能力探测应拒绝未知字段，状态码=%d body=%s", invalid.Code, invalid.Body.String())
	}
}

func TestProviderModelCapabilityProbeAPIExposesOptionalSubsets(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开 SQLite Store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: httpCapabilityProbeAdapter{},
	})
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	if _, err := registry.Save(context.Background(), provider.SaveRequest{
		Provider: provider.Provider{ID: "probe-subsets", Name: "探测子集", BaseURL: "https://example.invalid/v1", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", Enabled: true}}},
		APIKey:   stringPtr("probe-secret"),
	}); err != nil {
		t.Fatalf("保存探测供应商失败: %v", err)
	}

	response := callHTTP(NewServer(registry, nil).Handler(), http.MethodPost, "/api/v1/providers/probe-subsets/models/model/probe", `{"include_streaming":false,"include_tool_calling":false,"include_structured_json":false,"include_structured_schema":true,"include_reasoning":true,"include_images":true,"include_input_files":true}`)
	if response.Code != http.StatusOK {
		t.Fatalf("能力子集探测 API 状态码=%d body=%s", response.Code, response.Body.String())
	}
	var result provider.CapabilityProbeResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("解析能力子集探测结果失败: %v", err)
	}
	if result.Profile.Streaming.State != provider.SupportUnknown || result.Profile.Images.State != provider.SupportSupported || result.Profile.InputFiles.State != provider.SupportSupported || result.Profile.ReasoningEffort.State != provider.SupportSupported || result.Profile.StructuredOutputSchema.State != provider.SupportSupported {
		t.Fatalf("能力子集开关未按请求应用: %#v", result.Profile)
	}
	if findHTTPProbeObservation(result.Observations, "structured_output_schema").State != provider.SupportSupported {
		t.Fatalf("能力子集 schema 证据不正确: %#v", result.Observations)
	}
	if result.Profile.JSONSchemaDialect != "json-schema-subset" {
		t.Fatalf("能力子集 schema dialect 未持久化: %q", result.Profile.JSONSchemaDialect)
	}
}

func findHTTPProbeObservation(observations []provider.CapabilityObservation, feature string) provider.CapabilityObservation {
	for _, observation := range observations {
		if observation.Feature == feature {
			return observation
		}
	}
	return provider.CapabilityObservation{}
}

func stringPtr(value string) *string { return &value }

func TestStatusWorksBeforeOptionalServicesAreAttached(t *testing.T) {
	response := callHTTP(NewServer(nil, nil).Handler(), http.MethodGet, "/api/v1/status", "")
	if response.Code != http.StatusOK {
		t.Fatalf("未装配可选服务时读取状态失败: %d %s", response.Code, response.Body.String())
	}
	var status map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatalf("解析未装配服务的状态响应失败: %v", err)
	}
	if status["providers"] != float64(0) || status["workspaces"] != float64(0) {
		t.Fatalf("未装配服务时状态统计不正确: %#v", status)
	}
}

func TestPreviewDiscoverModelsWithoutSaving(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/models" {
			t.Errorf("模型探测路径 = %s，期望 /v1/models", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer preview-key" {
			t.Errorf("模型探测 Authorization = %q，期望使用表单密钥", request.Header.Get("Authorization"))
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"data":[{"id":"preview-model","display_name":"预览模型","context_window":262144,"max_output_tokens":32768}]}`))
	}))
	defer upstream.Close()

	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开 SQLite Store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: openai.NewAdapter(nil),
	})
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}

	response := callHTTP(NewServer(registry, nil).Handler(), http.MethodPost, "/api/v1/providers/preview/models/discover", `{"base_url":"`+upstream.URL+`/v1","protocol":"openai-compatible","openai_format":"auto","api_key":"preview-key"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("未保存供应商探测状态码 = %d，响应 = %s", response.Code, response.Body.String())
	}
	var result struct {
		Models []provider.Model `json:"models"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("解析未保存供应商探测响应失败: %v", err)
	}
	if len(result.Models) != 1 || result.Models[0].ID != "preview-model" || result.Models[0].ContextWindow != 262144 {
		t.Fatalf("未保存供应商探测结果不正确: %#v", result.Models)
	}
	if saved := registry.List(); len(saved) != 0 {
		t.Fatalf("未保存供应商探测不应写入供应商，实际保存了 %d 个", len(saved))
	}
}

func TestNormalizeChatAttachments(t *testing.T) {
	attachments, err := normalizeChatAttachments([]chatAttachmentPayload{{
		Name: `C:\Users\demo\截图.png`, Data: []byte{1, 2, 3},
	}, {
		Name: "说明.pdf", MIMEType: "application/pdf; charset=binary", Data: []byte{4},
	}})
	if err != nil {
		t.Fatalf("规范化聊天附件失败: %v", err)
	}
	if len(attachments) != 2 || attachments[0].Name != "截图.png" || attachments[0].MIMEType != "image/png" || attachments[1].MIMEType != "application/pdf" {
		t.Fatalf("聊天附件规范化结果不正确: %#v", attachments)
	}
	if _, err := normalizeChatAttachments(make([]chatAttachmentPayload, maxChatAttachments+1)); err == nil {
		t.Fatal("超出附件数量上限时应返回错误")
	}
}

func TestParseApprovalDecisionSupportsCanonicalAndLegacyShapes(t *testing.T) {
	approved := true
	if value, err := parseApprovalDecision(approvalResolvePayload{Decision: "approve"}); err != nil || !value {
		t.Fatalf("canonical approve = %v, err=%v", value, err)
	}
	if value, err := parseApprovalDecision(approvalResolvePayload{Decision: "reject"}); err != nil || value {
		t.Fatalf("canonical reject = %v, err=%v", value, err)
	}
	if value, err := parseApprovalDecision(approvalResolvePayload{Approved: &approved}); err != nil || !value {
		t.Fatalf("legacy approved = %v, err=%v", value, err)
	}
	if _, err := parseApprovalDecision(approvalResolvePayload{}); err == nil {
		t.Fatal("missing decision should fail")
	}
	if _, err := parseApprovalDecision(approvalResolvePayload{Decision: "approve", Approved: func() *bool { value := false; return &value }()}); err == nil {
		t.Fatal("conflicting decision should fail")
	}
}

func TestBotAPIHidesSecretsAndPreservesThemOnEdit(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开 SQLite Store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: openai.NewAdapter(nil),
	})
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	conversationService, err := conversation.NewService(store.ConversationRepository(), store.SessionService(), "abot")
	if err != nil {
		t.Fatalf("创建对话服务失败: %v", err)
	}
	kernel, err := agent.NewKernel(agent.Config{AppName: "abot", SessionService: store.SessionService(), Providers: registry})
	if err != nil {
		t.Fatalf("创建 Agent Kernel 失败: %v", err)
	}
	manager, err := bot.NewManager(context.Background(), store.BotRepository(), kernel, conversationService)
	if err != nil {
		t.Fatalf("创建机器人管理器失败: %v", err)
	}
	defer func() { _ = manager.Close() }()
	handler := NewServerWithServices(registry, kernel, nil, conversationService, manager).Handler()

	created := callHTTP(handler, http.MethodPost, "/api/v1/bots", `{"id":"telegram-main","name":"主 Telegram","type":"telegram","telegram_token":"telegram-secret","enabled":false}`)
	if created.Code != http.StatusOK {
		t.Fatalf("创建机器人状态码 = %d，响应 = %s", created.Code, created.Body.String())
	}
	if strings.Contains(created.Body.String(), "telegram-secret") || strings.Contains(created.Body.String(), `"telegram_token"`) {
		t.Fatalf("机器人响应泄露 Telegram Token: %s", created.Body.String())
	}
	var view botView
	if err := json.Unmarshal(created.Body.Bytes(), &view); err != nil {
		t.Fatalf("解析机器人响应失败: %v", err)
	}
	if !view.TelegramTokenConfigured || view.Enabled {
		t.Fatalf("机器人公开视图不正确: %#v", view)
	}

	reverse := callHTTP(handler, http.MethodPost, "/api/v1/bots", `{"id":"qq-server","name":"QQ 反向连接","type":"onebot11","onebot_mode":"reverse-server","listen_host":"0.0.0.0","listen_port":6199,"listen_path":"/ws","enabled":false}`)
	if reverse.Code != http.StatusOK {
		t.Fatalf("创建 OneBot 反向连接状态码 = %d，响应 = %s", reverse.Code, reverse.Body.String())
	}
	var reverseView botView
	if err := json.Unmarshal(reverse.Body.Bytes(), &reverseView); err != nil {
		t.Fatalf("解析 OneBot 反向连接响应失败: %v", err)
	}
	if reverseView.OneBotMode != "reverse-server" || reverseView.ListenHost != "0.0.0.0" || reverseView.ListenPort != 6199 || reverseView.ListenPath != "/ws" {
		t.Fatalf("OneBot 反向连接公开配置不正确: %#v", reverseView)
	}

	updated := callHTTP(handler, http.MethodPut, "/api/v1/bots/telegram-main", `{"name":"主 Telegram 更新","type":"telegram","enabled":false}`)
	if updated.Code != http.StatusOK {
		t.Fatalf("编辑机器人状态码 = %d，响应 = %s", updated.Code, updated.Body.String())
	}
	stored, err := store.BotRepository().Get(context.Background(), "telegram-main")
	if err != nil {
		t.Fatalf("读取编辑后的机器人失败: %v", err)
	}
	if stored.TelegramToken != "telegram-secret" {
		t.Fatalf("编辑后 Token = %q，期望保留旧 Token", stored.TelegramToken)
	}

	botTypes := callHTTP(handler, http.MethodGet, "/api/v1/bot-types", "")
	if botTypes.Code != http.StatusOK || !strings.Contains(botTypes.Body.String(), "onebot11") {
		t.Fatalf("机器人类型接口异常: %d %s", botTypes.Code, botTypes.Body.String())
	}
	deleted := callHTTP(handler, http.MethodDelete, "/api/v1/bots/telegram-main", "")
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("删除机器人状态码 = %d，响应 = %s", deleted.Code, deleted.Body.String())
	}
	if deleted := callHTTP(handler, http.MethodDelete, "/api/v1/bots/qq-server", ""); deleted.Code != http.StatusNoContent {
		t.Fatalf("删除 OneBot 机器人状态码 = %d，响应 = %s", deleted.Code, deleted.Body.String())
	}
}

func TestChatPayloadDecodesBase64Attachment(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(`{"message":"分析这个文件","attachments":[{"name":"说明.pdf","mime_type":"application/pdf","data":"AQID"}]}`))
	response := httptest.NewRecorder()
	var payload chatPayload
	if err := decodeJSONWithLimit(response, request, &payload, maxChatRequestBytes); err != nil {
		t.Fatalf("解析聊天附件请求失败: %v", err)
	}
	attachments, err := normalizeChatAttachments(payload.Attachments)
	if err != nil {
		t.Fatalf("规范化聊天附件请求失败: %v", err)
	}
	if len(attachments) != 1 || string(attachments[0].Data) != string([]byte{1, 2, 3}) {
		t.Fatalf("Base64 附件没有解码为原始字节: %#v", attachments)
	}
}

func TestWorkspaceAPIRequiresUserDirectoryAndApprovesWrite(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开 SQLite Store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: openai.NewAdapter(nil),
	})
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	workspaceService, err := workspace.NewService(store.WorkspaceRepository())
	if err != nil {
		t.Fatalf("创建工作区服务失败: %v", err)
	}
	handler := NewServer(registry, nil, workspaceService).Handler()

	missingRoot := callHTTP(handler, http.MethodPost, "/api/v1/workspaces", `{"id":"demo","name":"演示项目","type":"local"}`)
	if missingRoot.Code != http.StatusBadRequest {
		t.Fatalf("未指定目录状态码 = %d，响应 = %s", missingRoot.Code, missingRoot.Body.String())
	}

	root := t.TempDir()
	payload, _ := json.Marshal(map[string]any{"id": "demo", "name": "演示项目", "type": "local", "root_path": root, "enabled": true})
	created := callHTTP(handler, http.MethodPost, "/api/v1/workspaces", string(payload))
	if created.Code != http.StatusOK {
		t.Fatalf("创建工作区状态码 = %d，响应 = %s", created.Code, created.Body.String())
	}
	if strings.Contains(created.Body.String(), "default") {
		t.Fatalf("工作区响应不应包含默认工作区语义: %s", created.Body.String())
	}
	remote := callHTTP(handler, http.MethodPost, "/api/v1/workspaces", `{"id":"remote","name":"远程项目","type":"ssh","root_path":"/srv/project","host":"192.0.2.10","port":22,"user":"bot","auth_type":"password","password":"ssh-secret","enabled":true}`)
	if remote.Code != http.StatusOK {
		t.Fatalf("创建 SSH 工作区状态码 = %d，响应 = %s", remote.Code, remote.Body.String())
	}
	if strings.Contains(remote.Body.String(), "ssh-secret") || strings.Contains(remote.Body.String(), `"password":`) {
		t.Fatalf("SSH 工作区响应泄露密码: %s", remote.Body.String())
	}

	operationResponse := callHTTP(handler, http.MethodPost, "/api/v1/workspaces/demo/operations", `{"type":"write_file","path":"generated.txt","content":"approved"}`)
	if operationResponse.Code != http.StatusCreated {
		t.Fatalf("创建工作区操作状态码 = %d，响应 = %s", operationResponse.Code, operationResponse.Body.String())
	}
	var operation workspace.Operation
	if err := json.Unmarshal(operationResponse.Body.Bytes(), &operation); err != nil {
		t.Fatalf("解析工作区操作失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "generated.txt")); !os.IsNotExist(err) {
		t.Fatal("批准前不应写入文件")
	}
	approved := callHTTP(handler, http.MethodPost, "/api/v1/workspace-operations/"+operation.ID+"/approve", `{}`)
	if approved.Code != http.StatusOK {
		t.Fatalf("批准工作区操作状态码 = %d，响应 = %s", approved.Code, approved.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(root, "generated.txt"))
	if err != nil || string(data) != "approved" {
		t.Fatalf("批准后的文件内容不正确: data=%q err=%v", data, err)
	}
}

func TestWorkspaceCommandRunAPIListsAndGetsCheckpoint(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	workspaceService, err := workspace.NewService(store.WorkspaceRepository())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err := workspaceService.Save(context.Background(), workspace.SaveRequest{Workspace: workspace.Workspace{ID: "command-api", Name: "命令 API", Type: workspace.TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	operation, err := workspaceService.CreateOperation(context.Background(), workspace.OperationRequest{WorkspaceID: "command-api", InvocationID: "inv-command-api", Type: workspace.OperationCommand, Command: "printf api", Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer(nil, nil, workspaceService).Handler()
	listed := callHTTP(handler, http.MethodGet, "/api/v1/workspace-command-runs?workspace_id=command-api&invocation_id=inv-command-api", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), operation.CommandRunID) {
		t.Fatalf("command run 列表接口异常: %d %s", listed.Code, listed.Body.String())
	}
	fetched := callHTTP(handler, http.MethodGet, "/api/v1/workspace-command-runs/"+operation.CommandRunID, "")
	if fetched.Code != http.StatusOK || !strings.Contains(fetched.Body.String(), `"status":"queued"`) {
		t.Fatalf("command run 详情接口异常: %d %s", fetched.Code, fetched.Body.String())
	}
	var fetchedRun workspace.CommandRun
	if err := json.Unmarshal(fetched.Body.Bytes(), &fetchedRun); err != nil {
		t.Fatalf("解析 command run 详情失败: %v", err)
	}
	if fetchedRun.Capabilities == nil || fetchedRun.Capabilities.IsolationLevel != "l0_host_process" || fetchedRun.Capabilities.Network.State != workspace.ExecutionCapabilityUnsupported {
		t.Fatalf("command run 详情必须披露保守执行能力: %#v", fetchedRun.Capabilities)
	}
	if _, err := workspaceService.ApproveOperation(context.Background(), operation.ID); err != nil {
		t.Fatal(err)
	}
	output := callHTTP(handler, http.MethodGet, "/api/v1/workspace-command-runs/"+operation.CommandRunID+"/output?limit=1", "")
	if output.Code != http.StatusOK || !strings.Contains(output.Body.String(), `"chunks"`) || !strings.Contains(output.Body.String(), `"data":"`) {
		t.Fatalf("command run 输出分页接口异常: %d %s", output.Code, output.Body.String())
	}
	download := callHTTP(handler, http.MethodGet, "/api/v1/workspace-command-runs/"+operation.CommandRunID+"/output/download?format=text", "")
	if download.Code != http.StatusOK || download.Header().Get("Content-Type") != "text/plain; charset=utf-8" || !strings.Contains(download.Header().Get("Content-Disposition"), "attachment") || download.Body.String() != "api" {
		t.Fatalf("command run 文本下载接口异常: %d headers=%v body=%q", download.Code, download.Header(), download.Body.String())
	}
	structured := callHTTP(handler, http.MethodGet, "/api/v1/command-runs/"+operation.CommandRunID+"/output/download?format=ndjson", "")
	if structured.Code != http.StatusOK || structured.Header().Get("Content-Type") != "application/x-ndjson; charset=utf-8" || !strings.Contains(structured.Body.String(), `"command_run_id":"`+operation.CommandRunID+`"`) {
		t.Fatalf("command run NDJSON 下载接口异常: %d headers=%v body=%q", structured.Code, structured.Header(), structured.Body.String())
	}
	invalid := callHTTP(handler, http.MethodGet, "/api/v1/workspace-command-runs/"+operation.CommandRunID+"/output/download?format=xml", "")
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), "下载格式") {
		t.Fatalf("非法日志下载格式应返回 400: %d %s", invalid.Code, invalid.Body.String())
	}
	commandRepo, ok := store.WorkspaceRepository().(workspace.CommandRunRepository)
	if !ok {
		t.Fatal("SQLite workspace repository should implement CommandRunRepository")
	}
	run, err := commandRepo.GetCommandRun(context.Background(), operation.CommandRunID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().UTC().Add(-time.Hour)
	run.OutputRetentionState = workspace.CommandOutputRetentionRetained
	run.OutputRetainedUntil = &deadline
	run.Revision++
	if _, err := commandRepo.UpdateCommandRun(context.Background(), run, run.Revision-1); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaceService.PurgeExpiredCommandOutput(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	expired := callHTTP(handler, http.MethodGet, "/api/v1/workspace-command-runs/"+operation.CommandRunID+"/output/download?format=text", "")
	if expired.Code != http.StatusGone || !strings.Contains(expired.Body.String(), "过保留期") {
		t.Fatalf("过期日志下载应返回 410: %d %s", expired.Code, expired.Body.String())
	}
}

func TestWorkspaceCommandPTYWebSocketAttachLeaseReplayAndSequence(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	service, err := workspace.NewService(store.WorkspaceRepository())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err := service.Save(context.Background(), workspace.SaveRequest{Workspace: workspace.Workspace{ID: "pty-http", Name: "PTY HTTP", Type: workspace.TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	operation, err := service.CreateOperation(context.Background(), workspace.OperationRequest{
		WorkspaceID: "pty-http", Type: workspace.OperationCommand,
		Command: "read line; printf 'got:%s\\n' \"$line\"; sleep 1", Timeout: 5,
		TTY: &workspace.TTYSpec{Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	resultCh := make(chan error, 1)
	go func() {
		_, approveErr := service.ApproveOperation(context.Background(), operation.ID)
		resultCh <- approveErr
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		run, getErr := service.GetCommandRun(context.Background(), operation.CommandRunID)
		if getErr == nil && run.Status == workspace.CommandRunRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("PTY HTTP 命令未进入 running")
		}
		time.Sleep(5 * time.Millisecond)
	}
	httpServer := httptest.NewServer(NewServer(nil, nil, service).Handler())
	defer httpServer.Close()
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/api/v1/workspace-command-runs/" + operation.CommandRunID + "/pty?writer=true"
	connection, response, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if response != nil {
			t.Fatalf("PTY WebSocket writer 握手失败: %v status=%s", err, response.Status)
		}
		t.Fatal(err)
	}
	defer connection.Close()
	var attached commandPTYFrame
	if err := connection.ReadJSON(&attached); err != nil || attached.Type != "attached" || !attached.Writer || attached.Lease == "" {
		t.Fatalf("PTY attached 帧不正确: %#v err=%v", attached, err)
	}
	secondURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/api/v1/workspace-command-runs/" + operation.CommandRunID + "/pty?writer=true"
	if second, secondResponse, dialErr := websocket.DefaultDialer.Dial(secondURL, nil); dialErr == nil {
		_ = second.Close()
		t.Fatal("同一 CommandRun 不应允许第二个 writer")
	} else if secondResponse == nil || (secondResponse.StatusCode != http.StatusConflict && secondResponse.StatusCode != http.StatusInternalServerError) {
		t.Fatalf("第二个 writer 应返回冲突: err=%v response=%v", dialErr, secondResponse)
	}
	stdin := base64.StdEncoding.EncodeToString([]byte("hello\n"))
	if err := connection.WriteJSON(commandPTYFrame{Type: "stdin", Lease: attached.Lease, Data: stdin, RequestSeq: 1}); err != nil {
		t.Fatal(err)
	}
	// A duplicate sequence is rejected and the attach is closed, but the
	// command itself continues because WebSocket detach is not cancellation.
	if err := connection.WriteJSON(commandPTYFrame{Type: "stdin", Lease: attached.Lease, Data: stdin, RequestSeq: 1}); err != nil {
		t.Fatal(err)
	}
	var controlError commandPTYFrame
	for {
		var frame commandPTYFrame
		if err := connection.ReadJSON(&frame); err != nil {
			t.Fatalf("乱序控制帧读取失败: %#v err=%v", frame, err)
		}
		if frame.Type == "error" {
			controlError = frame
			break
		}
		if frame.Type != "chunk" && frame.Type != "attached" {
			t.Fatalf("乱序控制帧期间收到未知响应: %#v", frame)
		}
	}
	if !strings.Contains(controlError.Error, "严格递增") {
		t.Fatalf("乱序控制帧应返回严格递增错误: %#v", controlError)
	}
	_ = connection.Close()
	if approveErr := <-resultCh; approveErr != nil {
		t.Fatalf("WebSocket 断开不应取消命令: %v", approveErr)
	}
	finalRun, err := service.GetCommandRun(context.Background(), operation.CommandRunID)
	if err != nil || finalRun.Status != workspace.CommandRunExited {
		t.Fatalf("PTY 命令在断开后应正常结束: %#v err=%v", finalRun, err)
	}

	// A fresh read-only attach receives the durable terminal chunk exactly once
	// and then a bounded done frame.
	readURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/api/v1/command-runs/" + operation.CommandRunID + "/pty"
	reader, response, err := websocket.DefaultDialer.Dial(readURL, nil)
	if err != nil {
		if response != nil {
			t.Fatalf("PTY WebSocket replay 握手失败: %v status=%s", err, response.Status)
		}
		t.Fatal(err)
	}
	defer reader.Close()
	var replayAttached commandPTYFrame
	if err := reader.ReadJSON(&replayAttached); err != nil || replayAttached.Type != "attached" || replayAttached.Writer {
		t.Fatalf("只读 attach 帧不正确: %#v err=%v", replayAttached, err)
	}
	var replayed strings.Builder
	for {
		var replayChunk commandPTYFrame
		if err := reader.ReadJSON(&replayChunk); err != nil {
			t.Fatalf("读取 PTY durable replay 失败: %#v err=%v", replayChunk, err)
		}
		if replayChunk.Type == "done" {
			if replayChunk.Status != string(workspace.CommandRunExited) {
				t.Fatalf("PTY done 帧状态不正确: %#v", replayChunk)
			}
			break
		}
		if replayChunk.Type != "chunk" || replayChunk.Stream != "terminal" {
			t.Fatalf("PTY durable replay chunk 不正确: %#v", replayChunk)
		}
		decoded, decodeErr := base64.StdEncoding.DecodeString(replayChunk.Data)
		if decodeErr != nil {
			t.Fatalf("PTY replay 数据不是 base64: %v", decodeErr)
		}
		replayed.Write(decoded)
	}
	if !strings.Contains(replayed.String(), "got:hello") {
		t.Fatalf("PTY replay 数据不正确: %q", replayed.String())
	}
}

func TestWorkspaceCommandRunCancelAPIStopsQueuedAttempt(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	workspaceService, err := workspace.NewService(store.WorkspaceRepository())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err := workspaceService.Save(context.Background(), workspace.SaveRequest{Workspace: workspace.Workspace{ID: "command-cancel-api", Name: "取消 API", Type: workspace.TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	operation, err := workspaceService.CreateOperation(context.Background(), workspace.OperationRequest{WorkspaceID: "command-cancel-api", Type: workspace.OperationCommand, Command: "touch should-not-run", Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer(nil, nil, workspaceService).Handler()
	cancelled := callHTTP(handler, http.MethodPost, "/api/v1/workspace-command-runs/"+operation.CommandRunID+"/cancel", `{"reason":"用户取消"}`)
	if cancelled.Code != http.StatusOK || !strings.Contains(cancelled.Body.String(), `"status":"cancelled"`) || !strings.Contains(cancelled.Body.String(), `"outcome":"cancelled"`) {
		t.Fatalf("command run 取消接口异常: %d %s", cancelled.Code, cancelled.Body.String())
	}
	approved, approveErr := workspaceService.ApproveOperation(context.Background(), operation.ID)
	if approveErr != nil || approved.Status != workspace.OperationCancelled {
		t.Fatalf("取消后的批准不应执行命令: %#v err=%v", approved, approveErr)
	}
	if _, err := os.Stat(filepath.Join(root, "should-not-run")); !os.IsNotExist(err) {
		t.Fatalf("取消 API 后不应产生命令副作用: %v", err)
	}
}

func TestWorkspaceReadPlanningAPIsAreBoundedAndDeterministic(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	workspaceService, err := workspace.NewService(store.WorkspaceRepository())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal", "agent", "kernel.go"), []byte("package agent\nfunc Run() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaceService.Save(context.Background(), workspace.SaveRequest{Workspace: workspace.Workspace{ID: "planning", Name: "planning", Type: workspace.TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	handler := NewServer(nil, nil, workspaceService).Handler()
	rangeResponse := callHTTP(handler, http.MethodGet, "/api/v1/workspaces/planning/file-range?path=README.md&start_line=2&end_line=3&include_line_numbers=true", "")
	if rangeResponse.Code != http.StatusOK || !strings.Contains(rangeResponse.Body.String(), `"content":"2|two\n3|three\n"`) || !strings.Contains(rangeResponse.Body.String(), `"total_lines":3`) {
		t.Fatalf("file-range 响应=%d body=%s", rangeResponse.Code, rangeResponse.Body.String())
	}
	globResponse := callHTTP(handler, http.MethodGet, "/api/v1/workspaces/planning/glob?pattern=internal%2F%2A%2A%2F%2A.go", "")
	if globResponse.Code != http.StatusOK || !strings.Contains(globResponse.Body.String(), "internal/agent/kernel.go") {
		t.Fatalf("glob 响应=%d body=%s", globResponse.Code, globResponse.Body.String())
	}
	regexResponse := callHTTP(handler, http.MethodGet, "/api/v1/workspaces/planning/search?path=.&q=func%20%5B%5E%28%5D%2A%5C%28&mode=regex", "")
	if regexResponse.Code != http.StatusOK || !strings.Contains(regexResponse.Body.String(), "internal/agent/kernel.go") {
		t.Fatalf("regex search 响应=%d body=%s", regexResponse.Code, regexResponse.Body.String())
	}
	optionResponse := callHTTP(handler, http.MethodGet, "/api/v1/workspaces/planning/search?path=.&q=FUNC&mode=literal&case_insensitive=true&glob=internal%2F%2A%2A%2F%2A.go&context_lines=1&max_hits=1", "")
	if optionResponse.Code != http.StatusOK || !strings.Contains(optionResponse.Body.String(), "context_before") {
		t.Fatalf("搜索选项响应=%d body=%s", optionResponse.Code, optionResponse.Body.String())
	}
	invalidRegex := callHTTP(handler, http.MethodGet, "/api/v1/workspaces/planning/search?path=.&q=%5B&mode=regex", "")
	if invalidRegex.Code != http.StatusBadRequest || !strings.Contains(invalidRegex.Body.String(), "正则表达式无效") {
		t.Fatalf("非法 regex 应返回 400: %d %s", invalidRegex.Code, invalidRegex.Body.String())
	}
	invalidSearchOptions := callHTTP(handler, http.MethodGet, "/api/v1/workspaces/planning/search?path=.&q=func&max_hits=101", "")
	if invalidSearchOptions.Code != http.StatusBadRequest || !strings.Contains(invalidSearchOptions.Body.String(), "max_hits") {
		t.Fatalf("非法搜索上限应返回 400: %d %s", invalidSearchOptions.Code, invalidSearchOptions.Body.String())
	}
	invalidSearchBool := callHTTP(handler, http.MethodGet, "/api/v1/workspaces/planning/search?path=.&q=func&case_insensitive=maybe", "")
	if invalidSearchBool.Code != http.StatusBadRequest || !strings.Contains(invalidSearchBool.Body.String(), "case_insensitive") {
		t.Fatalf("非法大小写参数应返回 400: %d %s", invalidSearchBool.Code, invalidSearchBool.Body.String())
	}
	conflictingSearchBool := callHTTP(handler, http.MethodGet, "/api/v1/workspaces/planning/search?path=.&q=func&case_insensitive=true&case_sensitive=true", "")
	if conflictingSearchBool.Code != http.StatusBadRequest || !strings.Contains(conflictingSearchBool.Body.String(), "冲突") {
		t.Fatalf("冲突的大小写参数应返回 400: %d %s", conflictingSearchBool.Code, conflictingSearchBool.Body.String())
	}
	manyResponse := callHTTP(handler, http.MethodPost, "/api/v1/workspaces/planning/files/read-many", `{"paths":["README.md","missing.txt"]}`)
	if manyResponse.Code != http.StatusOK || !strings.Contains(manyResponse.Body.String(), `"content_digest":"sha256:`) || !strings.Contains(manyResponse.Body.String(), `"error":`) {
		t.Fatalf("read-many 响应=%d body=%s", manyResponse.Code, manyResponse.Body.String())
	}
	overRange := callHTTP(handler, http.MethodGet, "/api/v1/workspaces/planning/file-range?path=README.md&start_line=1&end_line=2001", "")
	if overRange.Code != http.StatusBadRequest {
		t.Fatalf("超大 file-range 状态码=%d body=%s", overRange.Code, overRange.Body.String())
	}
}

func TestConversationWorkspaceLifecycleAndPhysicalDelete(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开 SQLite Store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: openai.NewAdapter(nil),
	})
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	workspaceService, err := workspace.NewService(store.WorkspaceRepository())
	if err != nil {
		t.Fatalf("创建工作区服务失败: %v", err)
	}
	conversationService, err := conversation.NewService(store.ConversationRepository(), store.SessionService(), "abot")
	if err != nil {
		t.Fatalf("创建对话服务失败: %v", err)
	}
	workspaceService.SetConversationPolicy(conversationService.CountByWorkspace, conversationService.IsActiveByID)
	handler := NewServerWithServices(registry, nil, workspaceService, conversationService).Handler()

	root := t.TempDir()
	workspaceBody, _ := json.Marshal(map[string]any{"id": "project", "name": "项目", "type": "local", "root_path": root, "enabled": true})
	createdWorkspace := callHTTP(handler, http.MethodPost, "/api/v1/workspaces", string(workspaceBody))
	if createdWorkspace.Code != http.StatusOK {
		t.Fatalf("创建工作区失败: %d %s", createdWorkspace.Code, createdWorkspace.Body.String())
	}

	createConversation := func(title string) string {
		body, _ := json.Marshal(map[string]string{"user_id": "user-1", "title": title})
		response := callHTTP(handler, http.MethodPost, "/api/v1/workspaces/project/conversations", string(body))
		if response.Code != http.StatusCreated {
			t.Fatalf("创建工作区对话失败: %d %s", response.Code, response.Body.String())
		}
		var item conversation.Conversation
		if err := json.Unmarshal(response.Body.Bytes(), &item); err != nil {
			t.Fatalf("解析工作区对话失败: %v", err)
		}
		return item.ID
	}
	firstID := createConversation("第一个对话")
	secondID := createConversation("第二个对话")
	list := callHTTP(handler, http.MethodGet, "/api/v1/workspaces/project/conversations?user_id=user-1", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), firstID) || !strings.Contains(list.Body.String(), secondID) {
		t.Fatalf("工作区没有列出多个对话: %d %s", list.Code, list.Body.String())
	}

	activeDelete := callHTTP(handler, http.MethodDelete, "/api/v1/conversations/"+firstID+"?user_id=user-1", "")
	if activeDelete.Code != http.StatusConflict {
		t.Fatalf("活跃对话删除状态码 = %d，期望 %d，响应=%s", activeDelete.Code, http.StatusConflict, activeDelete.Body.String())
	}
	archive := callHTTP(handler, http.MethodPost, "/api/v1/conversations/"+firstID+"/archive?user_id=user-1", "{}")
	if archive.Code != http.StatusOK {
		t.Fatalf("归档对话失败: %d %s", archive.Code, archive.Body.String())
	}
	archivedOperation, err := workspaceService.CreateOperation(context.Background(), workspace.OperationRequest{
		WorkspaceID: "project", ConversationID: firstID, Type: workspace.OperationWriteFile, Path: "should-not-write.txt", Content: "x",
	})
	if err != nil {
		t.Fatalf("创建待删除对话操作失败: %v", err)
	}
	operations := callHTTP(handler, http.MethodGet, "/api/v1/workspace-operations?conversation_id="+firstID, "")
	if operations.Code != http.StatusOK || !strings.Contains(operations.Body.String(), firstID) {
		t.Fatalf("未找到对话操作: %d %s", operations.Code, operations.Body.String())
	}
	approvedArchived := callHTTP(handler, http.MethodPost, "/api/v1/workspace-operations/"+archivedOperation.ID+"/approve", "{}")
	if approvedArchived.Code != http.StatusConflict {
		t.Fatalf("归档对话的工作区操作批准状态码 = %d，期望 %d，响应=%s", approvedArchived.Code, http.StatusConflict, approvedArchived.Body.String())
	}
	archivedDelete := callHTTP(handler, http.MethodDelete, "/api/v1/conversations/"+firstID+"?user_id=user-1", "")
	if archivedDelete.Code != http.StatusNoContent {
		t.Fatalf("归档对话物理删除状态码 = %d，响应=%s", archivedDelete.Code, archivedDelete.Body.String())
	}
	missing := callHTTP(handler, http.MethodGet, "/api/v1/conversations/"+firstID+"?user_id=user-1", "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("删除后的对话读取状态码 = %d，期望 %d", missing.Code, http.StatusNotFound)
	}
	operations = callHTTP(handler, http.MethodGet, "/api/v1/workspace-operations?conversation_id="+firstID, "")
	if operations.Code != http.StatusOK || strings.Contains(operations.Body.String(), "should-not-write.txt") {
		t.Fatalf("删除对话后仍能读取其工作区操作: %d %s", operations.Code, operations.Body.String())
	}

	blockedWorkspaceDelete := callHTTP(handler, http.MethodDelete, "/api/v1/workspaces/project", "")
	if blockedWorkspaceDelete.Code != http.StatusConflict {
		t.Fatalf("仍有对话时删除工作区状态码 = %d，期望 %d", blockedWorkspaceDelete.Code, http.StatusConflict)
	}
	archiveSecond := callHTTP(handler, http.MethodPost, "/api/v1/conversations/"+secondID+"/archive?user_id=user-1", "{}")
	if archiveSecond.Code != http.StatusOK {
		t.Fatalf("归档第二个对话失败: %d %s", archiveSecond.Code, archiveSecond.Body.String())
	}
	if response := callHTTP(handler, http.MethodDelete, "/api/v1/conversations/"+secondID+"?user_id=user-1", ""); response.Code != http.StatusNoContent {
		t.Fatalf("删除第二个对话失败: %d %s", response.Code, response.Body.String())
	}
	if response := callHTTP(handler, http.MethodDelete, "/api/v1/workspaces/project", ""); response.Code != http.StatusNoContent {
		t.Fatalf("清空对话后删除工作区失败: %d %s", response.Code, response.Body.String())
	}
}

func TestMemoryAPIProvidesUserScopedPhysicalCRUD(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开 SQLite Store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()
	service, err := memory.NewService(store.MemoryRepository(), "abot")
	if err != nil {
		t.Fatalf("创建长期记忆服务失败: %v", err)
	}
	handler := NewServer(nil, nil)
	handler.SetMemoryService(service)
	router := handler.Handler()

	created := callHTTP(router, http.MethodPost, "/api/v1/memories", `{"user_id":"user-1","content":"用户偏好中文","tags":"偏好"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("创建长期记忆状态码 = %d，响应 = %s", created.Code, created.Body.String())
	}
	var item memory.Item
	if err := json.Unmarshal(created.Body.Bytes(), &item); err != nil {
		t.Fatalf("解析创建的长期记忆失败: %v", err)
	}
	if item.ID == "" || item.UserID != "user-1" {
		t.Fatalf("创建的长期记忆字段不正确: %#v", item)
	}

	list := callHTTP(router, http.MethodGet, "/api/v1/memories?user_id=user-1&q=中文", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), item.ID) {
		t.Fatalf("用户记忆列表异常: %d %s", list.Code, list.Body.String())
	}
	if other := callHTTP(router, http.MethodGet, "/api/v1/memories?user_id=user-2", ""); other.Code != http.StatusOK || strings.Contains(other.Body.String(), item.ID) {
		t.Fatalf("记忆列表越过用户边界: %d %s", other.Code, other.Body.String())
	}

	updated := callHTTP(router, http.MethodPut, "/api/v1/memories/"+item.ID+"?user_id=user-1", `{"content":"用户偏好简洁中文","tags":"偏好,语言"}`)
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), "简洁中文") {
		t.Fatalf("更新长期记忆失败: %d %s", updated.Code, updated.Body.String())
	}
	if forbidden := callHTTP(router, http.MethodDelete, "/api/v1/memories/"+item.ID+"?user_id=user-2", ""); forbidden.Code != http.StatusNotFound {
		t.Fatalf("跨用户删除记忆状态码 = %d，期望 %d", forbidden.Code, http.StatusNotFound)
	}
	if deleted := callHTTP(router, http.MethodDelete, "/api/v1/memories/"+item.ID+"?user_id=user-1", ""); deleted.Code != http.StatusNoContent {
		t.Fatalf("物理删除长期记忆失败: %d %s", deleted.Code, deleted.Body.String())
	}
	if missing := callHTTP(router, http.MethodGet, "/api/v1/memories/"+item.ID+"?user_id=user-1", ""); missing.Code != http.StatusNotFound {
		t.Fatalf("物理删除后长期记忆仍可读取: %d %s", missing.Code, missing.Body.String())
	}

	for _, content := range []string{"第一条", "第二条"} {
		response := callHTTP(router, http.MethodPost, "/api/v1/memories", `{"user_id":"user-1","content":"`+content+`"}`)
		if response.Code != http.StatusCreated {
			t.Fatalf("创建用于清空的长期记忆失败: %d %s", response.Code, response.Body.String())
		}
	}
	if cleared := callHTTP(router, http.MethodDelete, "/api/v1/memories?user_id=user-1", ""); cleared.Code != http.StatusNoContent {
		t.Fatalf("清空长期记忆失败: %d %s", cleared.Code, cleared.Body.String())
	}
	if list := callHTTP(router, http.MethodGet, "/api/v1/memories?user_id=user-1", ""); list.Code != http.StatusOK || strings.Contains(list.Body.String(), "第一条") || strings.Contains(list.Body.String(), "第二条") {
		t.Fatalf("清空后仍有长期记忆: %d %s", list.Code, list.Body.String())
	}
}

// callHTTP 用统一方式调用 API，避免测试用例重复处理请求体和响应体。
func callHTTP(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
