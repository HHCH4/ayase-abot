package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"Abot/internal/agent"
	"Abot/internal/agent/eval"
	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/provider"
	"Abot/internal/storage/sqlite"
)

func TestEvalRunCompareAndGateAPI(t *testing.T) {
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
	now := time.Now().UTC()
	invocation := agentruntime.Invocation{ID: "inv-eval-api", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: agentruntime.InvocationCompleted, CreatedAt: now, UpdatedAt: now}
	repo := store.RuntimeRepository()
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	for index, eventType := range []string{agentruntime.EventInvocationStarted, agentruntime.EventInvocationCompleted} {
		if _, err := repo.AppendEvent(context.Background(), agentruntime.AgentEvent{ID: "eval-event-" + string(rune('1'+index)), InvocationID: invocation.ID, Type: eventType, Timestamp: now.Add(time.Duration(index) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	server := NewServer(nil, nil)
	server.SetRuntimeCoordinator(coordinator)
	definition := `{"id":"runtime-safe","version":"1","expected_outcome":"completed"}`
	create := func() agentruntime.EvalRun {
		response := callHTTP(server.Handler(), http.MethodPost, "/api/v1/evals/runs", `{"invocation_id":"`+invocation.ID+`","case":`+definition+`}`)
		if response.Code != http.StatusCreated {
			t.Fatalf("创建 EvalRun 状态码=%d body=%s", response.Code, response.Body.String())
		}
		var run agentruntime.EvalRun
		if err := json.Unmarshal(response.Body.Bytes(), &run); err != nil {
			t.Fatal(err)
		}
		if run.ID == "" || run.Status != agentruntime.EvalRunCompleted || !run.Passed || len(run.Result) == 0 {
			t.Fatalf("EvalRun 响应=%#v", run)
		}
		return run
	}
	base, candidate := create(), create()
	detail := callHTTP(server.Handler(), http.MethodGet, "/api/v1/evals/runs/"+base.ID, "")
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"status":"completed"`) || !strings.Contains(detail.Body.String(), `"result":{"case_id":"runtime-safe"`) {
		t.Fatalf("EvalRun 详情响应=%d body=%s", detail.Code, detail.Body.String())
	}
	list := callHTTP(server.Handler(), http.MethodGet, "/api/v1/evals/runs?invocation_id="+invocation.ID, "")
	var listed struct {
		Runs []agentruntime.EvalRun `json:"runs"`
	}
	if list.Code != http.StatusOK || json.Unmarshal(list.Body.Bytes(), &listed) != nil || len(listed.Runs) != 2 {
		t.Fatalf("EvalRun 列表响应=%d body=%s", list.Code, list.Body.String())
	}
	comparison := callHTTP(server.Handler(), http.MethodGet, "/api/v1/evals/compare?base="+base.ID+"&candidate="+candidate.ID, "")
	if comparison.Code != http.StatusOK || !strings.Contains(comparison.Body.String(), `"comparable":true`) || !strings.Contains(comparison.Body.String(), `"pass_rate_delta":0`) {
		t.Fatalf("Eval compare 响应=%d body=%s", comparison.Code, comparison.Body.String())
	}
	gate := callHTTP(server.Handler(), http.MethodPost, "/api/v1/evals/gate", `{"candidate_run_ids":["`+candidate.ID+`"],"baseline_run_ids":["`+base.ID+`"],"policy":{"min_pass_rate":1,"max_hard_failures":0,"require_baseline":true,"fail_on_regression":true}}`)
	if gate.Code != http.StatusOK || !strings.Contains(gate.Body.String(), `"passed":true`) {
		t.Fatalf("Eval gate 响应=%d body=%s", gate.Code, gate.Body.String())
	}
	badCompare := callHTTP(server.Handler(), http.MethodPost, "/api/v1/evals/runs", `{"invocation_id":"`+invocation.ID+`","case":{"id":"runtime-safe","version":"2","expected_outcome":"completed"}}`)
	if badCompare.Code != http.StatusCreated {
		t.Fatalf("创建新 case version 状态码=%d body=%s", badCompare.Code, badCompare.Body.String())
	}
	var changed agentruntime.EvalRun
	if err := json.Unmarshal(badCompare.Body.Bytes(), &changed); err != nil {
		t.Fatal(err)
	}
	incompatible := callHTTP(server.Handler(), http.MethodGet, "/api/v1/evals/compare?base="+base.ID+"&candidate="+changed.ID, "")
	if incompatible.Code != http.StatusConflict {
		t.Fatalf("不同 case version compare 应冲突，状态码=%d body=%s", incompatible.Code, incompatible.Body.String())
	}
	duplicateIDs := callHTTP(server.Handler(), http.MethodPost, "/api/v1/evals/gate", `{"candidate_run_ids":["`+candidate.ID+`","`+candidate.ID+`"]}`)
	if duplicateIDs.Code != http.StatusBadRequest {
		t.Fatalf("重复 candidate run id 应拒绝，状态码=%d body=%s", duplicateIDs.Code, duplicateIDs.Body.String())
	}
}

func TestEvalSuiteAPIIsBoundedStableAndPersistsRuns(t *testing.T) {
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
	for _, id := range []string{"inv-suite-z", "inv-suite-a"} {
		invocation := agentruntime.Invocation{ID: id, UserID: "user", ConversationID: id, SessionID: id, Status: agentruntime.InvocationCompleted, CreatedAt: now, UpdatedAt: now}
		if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
			t.Fatal(err)
		}
		for index, eventType := range []string{agentruntime.EventInvocationStarted, agentruntime.EventInvocationCompleted} {
			if _, err := repo.AppendEvent(context.Background(), agentruntime.AgentEvent{ID: id + "-event-" + string(rune('1'+index)), InvocationID: id, Type: eventType, Timestamp: now.Add(time.Duration(index) * time.Second)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	server := NewServer(nil, nil)
	server.SetRuntimeCoordinator(coordinator)
	body := `{"cases":[{"invocation_id":"inv-suite-z","case":{"id":"z-case","version":"1","expected_outcome":"completed"}},{"invocation_id":"inv-suite-a","case":{"id":"a-case","version":"1","expected_outcome":"completed"}}],"policy":{"min_pass_rate":1,"max_hard_failures":0}}`
	response := callHTTP(server.Handler(), http.MethodPost, "/api/v1/evals/suites", body)
	if response.Code != http.StatusCreated {
		t.Fatalf("suite 状态码=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Runs []agentruntime.EvalRun `json:"runs"`
		Gate eval.GateResult        `json:"gate"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Gate.Passed || len(payload.Runs) != 2 || payload.Runs[0].CaseID != "a-case" || payload.Runs[1].CaseID != "z-case" {
		t.Fatalf("suite 应稳定排序并通过 gate: %#v", payload)
	}
	for _, run := range payload.Runs {
		if run.Kind != "deterministic_suite" || run.Status != agentruntime.EvalRunCompleted || len(run.Result) == 0 {
			t.Fatalf("suite run 未持久化为不可变完成记录: %#v", run)
		}
	}

	invalid := `{"cases":[{"invocation_id":"inv-suite-a","case":{"id":"bad","version":"1","assertions":[{"id":"missing-type","kind":"event_absent"}]}}]}`
	bad := callHTTP(server.Handler(), http.MethodPost, "/api/v1/evals/suites", invalid)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("无效 suite case 应返回 400，状态码=%d body=%s", bad.Code, bad.Body.String())
	}
}
