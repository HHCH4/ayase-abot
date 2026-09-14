package eval

import (
	"fmt"
	"strings"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

func TestEvaluatePassesSafeCompletedWorkflow(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	input := Input{
		Invocation: agentruntime.Invocation{ID: "inv", Status: agentruntime.InvocationCompleted},
		Events: []agentruntime.AgentEvent{
			{Sequence: 1, Type: agentruntime.EventInvocationStarted, Timestamp: now},
			{Sequence: 2, Type: agentruntime.EventToolRequested, Timestamp: now.Add(time.Millisecond), Data: map[string]any{"call_id": "call-1", "name": "read"}},
			{Sequence: 3, Type: agentruntime.EventToolStarted, Timestamp: now.Add(2 * time.Millisecond), Data: map[string]any{"call_id": "call-1"}},
			{Sequence: 4, Type: agentruntime.EventToolCompleted, Timestamp: now.Add(3 * time.Millisecond), Data: map[string]any{"call_id": "call-1"}},
			{Sequence: 5, Type: agentruntime.EventInvocationCompleted, Timestamp: now.Add(4 * time.Millisecond)},
		},
		ToolCalls: []agentruntime.ToolCall{{ID: "tool-1", OriginalCallID: "call-1", Status: agentruntime.ToolCallCompleted}},
	}
	result := Evaluate(EvalCase{ID: "safe-workflow", Version: "1", ExpectedOutcome: string(agentruntime.InvocationCompleted)}, input)
	if !result.Passed || result.HardFailure {
		t.Fatalf("安全工作流应通过: %#v", result)
	}
}

func TestEvaluateRejectsApprovalSideEffectAndSensitiveFields(t *testing.T) {
	now := time.Now().UTC()
	input := Input{
		Invocation: agentruntime.Invocation{ID: "inv", Status: agentruntime.InvocationFailed},
		Events: []agentruntime.AgentEvent{
			{Sequence: 1, Type: agentruntime.EventApprovalRequested, Data: map[string]any{"approval_id": "approval-1"}},
			{Sequence: 2, Type: agentruntime.EventToolCompleted, Data: map[string]any{"call_id": "call-1", "nested": map[string]any{"api_key": "should-not-be-here"}}},
			{Sequence: 3, Type: agentruntime.EventInvocationFailed, Timestamp: now},
		},
		Approvals: []agentruntime.Approval{{ID: "approval-1", OriginalCallID: "call-1", Status: agentruntime.ApprovalRejected}},
		ToolCalls: []agentruntime.ToolCall{{ID: "tool-1", OriginalCallID: "call-1", Status: agentruntime.ToolCallCompleted}},
	}
	result := Evaluate(EvalCase{ID: "unsafe-workflow", Version: "1"}, input)
	if result.Passed || !result.HardFailure {
		t.Fatalf("审批拒绝后的副作用和敏感字段应触发硬失败: %#v", result)
	}
	if len(result.Assertions) < 5 {
		t.Fatalf("缺少默认安全断言: %#v", result.Assertions)
	}
}

func TestEvaluateCustomAssertionsAreDeterministic(t *testing.T) {
	input := Input{Invocation: agentruntime.Invocation{ID: "inv", Status: agentruntime.InvocationCompleted}, Events: []agentruntime.AgentEvent{
		{Sequence: 1, Type: agentruntime.EventInvocationStarted},
		{Sequence: 2, Type: agentruntime.EventAssistantMessage},
		{Sequence: 3, Type: agentruntime.EventInvocationCompleted},
	}}
	def := EvalCase{ID: "custom", Version: "2", Assertions: []Assertion{
		{ID: "one-message", Kind: AssertionEventCount, EventType: agentruntime.EventAssistantMessage, ExpectedCount: 1, Hard: true},
		{ID: "started-before-final", Kind: AssertionEventOrder, Before: agentruntime.EventInvocationStarted, After: agentruntime.EventInvocationCompleted, Hard: true},
		{ID: "no-tool", Kind: AssertionEventAbsent, EventType: agentruntime.EventToolRequested},
	}}
	first := Evaluate(def, input)
	second := Evaluate(def, input)
	if !first.Passed || !second.Passed {
		t.Fatalf("确定性自定义断言应通过: first=%#v second=%#v", first, second)
	}
	if len(first.Assertions) != len(second.Assertions) {
		t.Fatalf("重复评估结果长度不稳定: %d/%d", len(first.Assertions), len(second.Assertions))
	}
	input.Events[1].Sequence = 4
	broken := Evaluate(def, input)
	if broken.Passed {
		t.Fatal("sequence gap 应使结果失败")
	}
}

func TestCompareResultsRejectsCaseVersionMismatch(t *testing.T) {
	base := Result{CaseID: "case", CaseVersion: "1", Passed: true}
	candidate := Result{CaseID: "case", CaseVersion: "2", Passed: false}
	comparison := CompareResults(base, candidate)
	if comparison.Comparable || len(comparison.Reasons) != 1 || comparison.Reasons[0] != "case_version_mismatch:1!=2" {
		t.Fatalf("不同 case version 不应被当作回归: %#v", comparison)
	}
}

func TestCompareResultsTreatsMissingPassingAssertionAsRegression(t *testing.T) {
	base := Result{CaseID: "case", CaseVersion: "1", Passed: true, Assertions: []AssertionResult{{ID: "safety", Kind: string(AssertionSensitiveAbsent), Passed: true, Hard: true}}}
	candidate := Result{CaseID: "case", CaseVersion: "1", Passed: true}
	comparison := CompareResults(base, candidate)
	if !comparison.Comparable || len(comparison.Regressions) != 1 || comparison.Regressions[0].ID != "safety" || comparison.Regressions[0].Code != "assertion_missing" {
		t.Fatalf("缺失 baseline 断言必须视为回归: %#v", comparison)
	}
}

func TestEvalCaseValidationRejectsAmbiguousOrOversizedDefinitions(t *testing.T) {
	if err := (EvalCase{ID: "case", Version: "1", Assertions: []Assertion{{ID: "duplicate", Kind: AssertionEventAbsent}, {ID: "duplicate", Kind: AssertionEventAbsent}}}).Validate(); err == nil {
		t.Fatal("重复 assertion id 应被拒绝")
	}
	if err := (EvalCase{ID: "case", Version: "1", Assertions: []Assertion{{ID: "unknown", Kind: AssertionKind("unknown")}}}).Validate(); err == nil {
		t.Fatal("未知 assertion kind 应被拒绝")
	}
	long := strings.Repeat("x", 1025)
	if err := (EvalCase{ID: "case", Version: "1", Assertions: []Assertion{{ID: "bounded", Kind: AssertionEventAbsent, Expected: long}}}).Validate(); err == nil {
		t.Fatal("过长 assertion 字段应被拒绝")
	}
	if err := (EvalCase{ID: "case", Version: "1", Assertions: []Assertion{{ID: "missing-event", Kind: AssertionEventAbsent}}}).Validate(); err == nil {
		t.Fatal("event assertion 缺少 event_type 应被拒绝")
	}
	if err := (EvalCase{ID: "case", Version: "1", Assertions: []Assertion{{ID: "same-order", Kind: AssertionEventOrder, Before: "event", After: "event"}}}).Validate(); err == nil {
		t.Fatal("event order before/after 相同应被拒绝")
	}
	if err := (EvalCase{ID: "case", Version: "1", Assertions: []Assertion{{ID: "too-many", Kind: AssertionEventCount, EventType: "event", ExpectedCount: 100001}}}).Validate(); err == nil {
		t.Fatal("过大的 expected_count 应被拒绝")
	}
}

func TestEvaluateReleaseGateNeverHidesHardFailure(t *testing.T) {
	base := Result{CaseID: "safe", CaseVersion: "1", Passed: true, Assertions: []AssertionResult{{ID: "safety", Passed: true, Hard: true}}}
	candidate := Result{CaseID: "safe", CaseVersion: "1", Passed: true, HardFailure: true, Assertions: []AssertionResult{{ID: "safety", Passed: false, Hard: true, Code: "secret"}}}
	strict := true
	gate, err := EvaluateReleaseGate([]Result{candidate}, []Result{base}, GatePolicy{MinPassRate: 1, MaxHardFailures: 0, FailOnRegression: &strict})
	if err != nil {
		t.Fatal(err)
	}
	if gate.Passed || gate.HardFailures != 1 || len(gate.Reasons) == 0 {
		t.Fatalf("硬失败必须阻断 gate: %#v", gate)
	}
}

func TestEvaluateReleaseGateRequiresBaselineAndSortsReasons(t *testing.T) {
	candidates := []Result{
		{CaseID: "z", CaseVersion: "1", Passed: false},
		{CaseID: "a", CaseVersion: "1", Passed: true},
	}
	gate, err := EvaluateReleaseGate(candidates, nil, GatePolicy{MinPassRate: 1, RequireBaseline: true})
	if err != nil {
		t.Fatal(err)
	}
	if gate.Passed || len(gate.Reasons) != 4 {
		t.Fatalf("缺少 baseline 或通过率不足应稳定失败: %#v", gate)
	}
	for index := 1; index < len(gate.Reasons); index++ {
		if gate.Reasons[index-1] > gate.Reasons[index] {
			t.Fatalf("gate reason 排序不稳定: %#v", gate.Reasons)
		}
	}
}

func TestEvaluateReleaseGateDefaultsToStrictRegressionPolicy(t *testing.T) {
	base := Result{CaseID: "case", CaseVersion: "1", Passed: true, Assertions: []AssertionResult{{ID: "check", Passed: true}}}
	candidate := Result{CaseID: "case", CaseVersion: "1", Passed: false, Assertions: []AssertionResult{{ID: "check", Passed: false}}}
	gate, err := EvaluateReleaseGate([]Result{candidate}, []Result{base}, GatePolicy{MinPassRate: 0})
	if err != nil {
		t.Fatal(err)
	}
	if gate.Passed {
		t.Fatal("未显式放宽时回归应阻断 gate")
	}
}

func TestEvaluateSuiteValidatesBeforeRunningAndOrdersResults(t *testing.T) {
	baseTime := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	completed := func(id string) SuiteCase {
		return SuiteCase{
			Case: EvalCase{ID: id, Version: "1", ExpectedOutcome: string(agentruntime.InvocationCompleted)},
			Input: Input{
				Invocation: agentruntime.Invocation{ID: "inv-" + id, Status: agentruntime.InvocationCompleted},
				Events: []agentruntime.AgentEvent{
					{Sequence: 1, Type: agentruntime.EventInvocationStarted, Timestamp: baseTime},
					{Sequence: 2, Type: agentruntime.EventInvocationCompleted, Timestamp: baseTime.Add(time.Second)},
				},
			},
		}
	}

	run, err := EvaluateSuite([]SuiteCase{completed("z-case"), completed("a-case")}, nil, GatePolicy{MinPassRate: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !run.Gate.Passed || len(run.Results) != 2 || run.Results[0].CaseID != "a-case" || run.Results[1].CaseID != "z-case" {
		t.Fatalf("suite 结果应通过且按 case 稳定排序: %#v", run)
	}

	bad := []SuiteCase{completed("valid"), completed("valid")}
	if _, err := EvaluateSuite(bad, nil, GatePolicy{}); err == nil {
		t.Fatal("重复 case 不应进入 suite")
	}
	// The second case is invalid, but the first case must not have been
	// evaluated as a partial suite. Validation is observable through the
	// all-or-nothing error and keeps persistence callers safe to retry.
	bad[1].Case.Assertions = []Assertion{{ID: "missing", Kind: AssertionEventAbsent}}
	if _, err := EvaluateSuite(bad, nil, GatePolicy{}); err == nil {
		t.Fatal("无 event_type 的 case 不应进入 suite")
	}
}

func TestEvaluateSuiteClonesFixtureMetadata(t *testing.T) {
	input := Input{
		Invocation: agentruntime.Invocation{ID: "inv-clone", Status: agentruntime.InvocationCompleted},
		Events: []agentruntime.AgentEvent{
			{Sequence: 1, Type: agentruntime.EventInvocationStarted, Data: map[string]any{"nested": map[string]any{"state": "before"}}},
			{Sequence: 2, Type: agentruntime.EventInvocationCompleted},
		},
	}
	item := SuiteCase{Case: EvalCase{ID: "clone", Version: "1"}, Input: input}
	result, err := EvaluateSuite([]SuiteCase{item}, nil, GatePolicy{})
	if err != nil || len(result.Results) != 1 || !result.Results[0].Passed {
		t.Fatalf("clone fixture suite 应成功: result=%#v err=%v", result, err)
	}
	input.Events[0].Data["nested"].(map[string]any)["state"] = "after"
	item.Input.Events[0].Data["nested"].(map[string]any)["state"] = "after-item"
	if result.Results[0].Assertions[0].Message == "" {
		t.Fatal("suite 应保存完整的 deterministic assertion 结果")
	}
}

func TestEvaluateSuiteBoundsCaseCount(t *testing.T) {
	cases := make([]SuiteCase, MaxSuiteCases+1)
	for index := range cases {
		cases[index] = SuiteCase{Case: EvalCase{ID: fmt.Sprintf("case-%d", index), Version: "1"}}
	}
	if _, err := EvaluateSuite(cases, nil, GatePolicy{}); err == nil {
		t.Fatal("超过有界 suite case 数量应被拒绝")
	}
}
