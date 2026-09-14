package eval

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// WorkflowCase binds a versioned deterministic EvalCase to an executor. The
// executor is intentionally injected by the caller so tests can use a
// scripted/mock model and an isolated fixture without making the evaluator
// know how a Kernel or Coordinator is assembled.
//
// Execute must return only the durable metadata projection consumed by Eval;
// it must not expose prompt text, model output, file contents, or secrets to
// the evaluator.
type WorkflowCase struct {
	Case    EvalCase
	Execute func(context.Context) (Input, error)
}

var (
	ErrInvalidWorkflowCase = errors.New("mock workflow case 无效")
	ErrWorkflowExecution   = errors.New("mock workflow case 执行失败")
)

// EvaluateWorkflowSuite executes a bounded, deterministic workflow suite and
// then applies the same model-free assertions and release gate as
// EvaluateSuite. All case definitions are validated before any executor is
// called; this prevents a malformed case from producing a partially evaluated
// suite. Execution order is sorted by case ID/version, making fixture runs
// reproducible even when callers assemble the input in a different order.
func EvaluateWorkflowSuite(ctx context.Context, cases []WorkflowCase, baseline []Result, policy GatePolicy) (SuiteResult, error) {
	if len(cases) == 0 || len(cases) > MaxSuiteCases {
		return SuiteResult{}, fmt.Errorf("%w: case 数量必须在 1 到 %d 之间", ErrInvalidWorkflowCase, MaxSuiteCases)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	type indexedCase struct {
		key  string
		item WorkflowCase
	}
	validated := make([]indexedCase, 0, len(cases))
	seen := make(map[string]struct{}, len(cases))
	for index, item := range cases {
		if err := item.Case.Validate(); err != nil {
			return SuiteResult{}, fmt.Errorf("%w: case[%d]: %v", ErrInvalidWorkflowCase, index, err)
		}
		if item.Execute == nil {
			return SuiteResult{}, fmt.Errorf("%w: case[%d] 缺少 execute", ErrInvalidWorkflowCase, index)
		}
		key := resultKey(Result{CaseID: item.Case.ID, CaseVersion: item.Case.Version})
		if strings.TrimSpace(key) == "@" || strings.HasPrefix(key, "@") || strings.HasSuffix(key, "@") {
			// EvalCase.Validate already checks both fields. Keep this guard local
			// to the workflow wrapper so future key normalization cannot make two
			// malformed cases appear distinct.
			return SuiteResult{}, fmt.Errorf("%w: case[%d] key 无效", ErrInvalidWorkflowCase, index)
		}
		if _, exists := seen[key]; exists {
			return SuiteResult{}, fmt.Errorf("%w: case %s 重复", ErrInvalidWorkflowCase, key)
		}
		seen[key] = struct{}{}
		validated = append(validated, indexedCase{key: key, item: item})
	}
	sort.SliceStable(validated, func(i, j int) bool { return validated[i].key < validated[j].key })

	workflowCases := make([]SuiteCase, 0, len(validated))
	for _, item := range validated {
		select {
		case <-ctx.Done():
			return SuiteResult{}, fmt.Errorf("%w: case %s: %w", ErrWorkflowExecution, item.key, ctx.Err())
		default:
		}
		input, err := item.item.Execute(ctx)
		if err != nil {
			return SuiteResult{}, fmt.Errorf("%w: case %s: %w", ErrWorkflowExecution, item.key, err)
		}
		workflowCases = append(workflowCases, SuiteCase{Case: item.item.Case, Input: input})
	}
	return EvaluateSuite(workflowCases, baseline, policy)
}
