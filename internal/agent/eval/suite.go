package eval

import (
	"fmt"
	"sort"
	"strings"

	agentruntime "Abot/internal/agent/runtime"
)

const (
	// MaxSuiteCases keeps a synchronous deterministic suite bounded. Larger
	// suites belong to the future queued/long-running evaluator and must not
	// turn an HTTP request into an unbounded database scan.
	MaxSuiteCases = 100
)

var ErrInvalidSuite = fmt.Errorf("deterministic Eval suite 无效")

// SuiteCase binds a versioned EvalCase to a durable metadata projection. The
// projection contains no prompt, model response, file content or secret, and
// is copied before evaluation so callers cannot mutate the suite while it is
// running.
type SuiteCase struct {
	Case  EvalCase
	Input Input
}

// SuiteResult is a deterministic, stably ordered batch result. Gate is
// computed over the candidate results and optional baseline results supplied
// by the caller; persistence of individual EvalRuns remains the Runtime/API
// responsibility.
type SuiteResult struct {
	Results []Result   `json:"results"`
	Gate    GateResult `json:"gate"`
}

// EvaluateSuite validates every case before evaluating any of them. Case IDs
// must be unique because release-gate baseline matching is keyed by
// case_id/version; allowing duplicates would make the result ambiguous.
func EvaluateSuite(cases []SuiteCase, baseline []Result, policy GatePolicy) (SuiteResult, error) {
	if len(cases) == 0 || len(cases) > MaxSuiteCases {
		return SuiteResult{}, fmt.Errorf("%w: case 数量必须在 1 到 %d 之间", ErrInvalidSuite, MaxSuiteCases)
	}
	seen := make(map[string]struct{}, len(cases))
	prepared := make([]SuiteCase, len(cases))
	for index, item := range cases {
		if err := item.Case.Validate(); err != nil {
			return SuiteResult{}, fmt.Errorf("%w: case[%d]: %v", ErrInvalidSuite, index, err)
		}
		key := strings.TrimSpace(item.Case.ID) + "@" + strings.TrimSpace(item.Case.Version)
		if _, exists := seen[key]; exists {
			return SuiteResult{}, fmt.Errorf("%w: case 重复 %s", ErrInvalidSuite, key)
		}
		seen[key] = struct{}{}
		prepared[index] = cloneSuiteCase(item)
	}

	results := make([]Result, 0, len(prepared))
	for _, item := range prepared {
		results = append(results, Evaluate(item.Case, item.Input))
	}
	sort.SliceStable(results, func(i, j int) bool {
		left := strings.TrimSpace(results[i].CaseID) + "@" + strings.TrimSpace(results[i].CaseVersion)
		right := strings.TrimSpace(results[j].CaseID) + "@" + strings.TrimSpace(results[j].CaseVersion)
		return left < right
	})
	gate, err := EvaluateReleaseGate(results, baseline, policy)
	if err != nil {
		return SuiteResult{}, fmt.Errorf("%w: release gate: %v", ErrInvalidSuite, err)
	}
	return SuiteResult{Results: results, Gate: gate}, nil
}

func cloneSuiteCase(item SuiteCase) SuiteCase {
	result := item
	result.Case.Assertions = append([]Assertion(nil), item.Case.Assertions...)
	result.Input.Events = append([]agentruntime.AgentEvent(nil), item.Input.Events...)
	result.Input.ToolCalls = append([]agentruntime.ToolCall(nil), item.Input.ToolCalls...)
	result.Input.Approvals = append([]agentruntime.Approval(nil), item.Input.Approvals...)
	// Eval currently only reads event data. Clone the maps recursively so a
	// test fixture or caller cannot mutate nested metadata during evaluation.
	for index := range result.Input.Events {
		result.Input.Events[index].Data = cloneMetadataMap(item.Input.Events[index].Data)
	}
	return result
}

func cloneMetadataMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = cloneMetadataValue(value)
	}
	return result
}

func cloneMetadataValue(value any) any {
	switch item := value.(type) {
	case map[string]any:
		return cloneMetadataMap(item)
	case []any:
		result := make([]any, len(item))
		for index, child := range item {
			result[index] = cloneMetadataValue(child)
		}
		return result
	default:
		return value
	}
}
