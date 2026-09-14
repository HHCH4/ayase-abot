package eval

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	agentruntime "Abot/internal/agent/runtime"
)

func TestEvaluateWorkflowSuiteValidatesBeforeExecutionAndSortsCases(t *testing.T) {
	var executed []string
	makeCase := func(id string) WorkflowCase {
		return WorkflowCase{
			Case: EvalCase{ID: id, Version: "1", ExpectedOutcome: string(agentruntime.InvocationCompleted)},
			Execute: func(context.Context) (Input, error) {
				executed = append(executed, id)
				return completedWorkflowInput("inv-" + id), nil
			},
		}
	}

	result, err := EvaluateWorkflowSuite(context.Background(), []WorkflowCase{makeCase("z-case"), makeCase("a-case")}, nil, GatePolicy{MinPassRate: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(executed, []string{"a-case", "z-case"}) {
		t.Fatalf("workflow executor 顺序不稳定: %#v", executed)
	}
	if len(result.Results) != 2 || result.Results[0].CaseID != "a-case" || result.Results[1].CaseID != "z-case" || !result.Gate.Passed {
		t.Fatalf("workflow suite 结果不正确: %#v", result)
	}

	called := false
	invalid := WorkflowCase{
		Case: EvalCase{ID: "invalid", Version: "1", Assertions: []Assertion{{ID: "missing-event", Kind: AssertionEventAbsent}}},
		Execute: func(context.Context) (Input, error) {
			called = true
			return Input{}, nil
		},
	}
	if _, err := EvaluateWorkflowSuite(context.Background(), []WorkflowCase{makeCase("valid"), invalid}, nil, GatePolicy{}); !errors.Is(err, ErrInvalidWorkflowCase) {
		t.Fatalf("无效 case 应在执行前被拒绝: %v", err)
	}
	if called {
		t.Fatal("定义校验失败时不应执行任何 workflow executor")
	}
}

func TestEvaluateWorkflowSuiteWrapsExecutionFailureAndHonorsContext(t *testing.T) {
	want := errors.New("fixture failed")
	_, err := EvaluateWorkflowSuite(context.Background(), []WorkflowCase{{
		Case:    EvalCase{ID: "failing", Version: "1"},
		Execute: func(context.Context) (Input, error) { return Input{}, want },
	}}, nil, GatePolicy{})
	if !errors.Is(err, ErrWorkflowExecution) || !errors.Is(err, want) {
		t.Fatalf("executor 错误应保留可判断 cause: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	_, err = EvaluateWorkflowSuite(ctx, []WorkflowCase{{
		Case: EvalCase{ID: "cancelled", Version: "1"},
		Execute: func(context.Context) (Input, error) {
			called = true
			return Input{}, nil
		},
	}}, nil, GatePolicy{})
	if !errors.Is(err, ErrWorkflowExecution) || !errors.Is(err, context.Canceled) || called {
		t.Fatalf("取消上下文应在 executor 前终止: err=%v called=%v", err, called)
	}
}

func TestEvaluateWorkflowSuiteRejectsDuplicateAndOversizedDefinitions(t *testing.T) {
	item := func(id string) WorkflowCase {
		return WorkflowCase{Case: EvalCase{ID: id, Version: "1"}, Execute: func(context.Context) (Input, error) { return Input{}, nil }}
	}
	if _, err := EvaluateWorkflowSuite(context.Background(), []WorkflowCase{item("same"), item("same")}, nil, GatePolicy{}); !errors.Is(err, ErrInvalidWorkflowCase) {
		t.Fatalf("重复 workflow case 应被拒绝: %v", err)
	}
	cases := make([]WorkflowCase, MaxSuiteCases+1)
	for index := range cases {
		cases[index] = item(fmt.Sprintf("case-%d", index))
	}
	if _, err := EvaluateWorkflowSuite(context.Background(), cases, nil, GatePolicy{}); !errors.Is(err, ErrInvalidWorkflowCase) {
		t.Fatalf("超出 suite 上限应被拒绝: %v", err)
	}
}

func completedWorkflowInput(id string) Input {
	return Input{
		Invocation: agentruntime.Invocation{ID: id, Status: agentruntime.InvocationCompleted},
		Events: []agentruntime.AgentEvent{
			{Sequence: 1, Type: agentruntime.EventInvocationStarted},
			{Sequence: 2, Type: agentruntime.EventInvocationCompleted},
		},
	}
}
