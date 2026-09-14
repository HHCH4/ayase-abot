package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"Abot/internal/agent/eval"
	agentruntime "Abot/internal/agent/runtime"
)

type evalSuiteCasePayload struct {
	InvocationID string         `json:"invocation_id"`
	Case         *eval.EvalCase `json:"case"`
}

type evalSuitePayload struct {
	Cases          []evalSuiteCasePayload `json:"cases"`
	BaselineRunIDs []string               `json:"baseline_run_ids,omitempty"`
	Policy         eval.GatePolicy        `json:"policy"`
}

// runEvalSuite evaluates a bounded batch of durable projections, then stores
// one immutable EvalRun per case. All case definitions and projections are
// loaded and validated before the first run is persisted, so malformed input
// cannot create a partial suite.
func (s *Server) runEvalSuite(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload evalSuitePayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if len(payload.Cases) == 0 || len(payload.Cases) > eval.MaxSuiteCases {
		writeError(writer, fmt.Errorf("%w: cases 数量必须在 1 到 %d 之间", agentruntime.ErrInvalidEvalRun, eval.MaxSuiteCases))
		return
	}

	cases := make([]eval.SuiteCase, 0, len(payload.Cases))
	definitions := make(map[string]eval.EvalCase, len(payload.Cases))
	invocationIDs := make(map[string]string, len(payload.Cases))
	for index, item := range payload.Cases {
		invocationID := strings.TrimSpace(item.InvocationID)
		if invocationID == "" || item.Case == nil {
			writeError(writer, fmt.Errorf("%w: case[%d] 需要 invocation_id 和 case", agentruntime.ErrInvalidEvalRun, index))
			return
		}
		definition := *item.Case
		definition.ID = strings.TrimSpace(definition.ID)
		definition.Version = strings.TrimSpace(definition.Version)
		if err := definition.Validate(); err != nil {
			writeError(writer, fmt.Errorf("%w: case[%d]: %v", agentruntime.ErrInvalidEvalRun, index, err))
			return
		}
		key := evalSuiteResultKey(definition.ID, definition.Version)
		if _, exists := definitions[key]; exists {
			writeError(writer, fmt.Errorf("%w: case 重复 %s", agentruntime.ErrInvalidEvalRun, key))
			return
		}
		input, getErr := runtime.GetEvalInput(request.Context(), invocationID)
		if getErr != nil {
			writeError(writer, getErr)
			return
		}
		definitions[key] = definition
		invocationIDs[key] = input.Invocation.ID
		cases = append(cases, eval.SuiteCase{Case: definition, Input: eval.Input{Invocation: input.Invocation, Events: input.Events, ToolCalls: input.ToolCalls, Approvals: input.Approvals}})
	}

	baseline, err := s.loadEvalResults(request, payload.BaselineRunIDs)
	if err != nil {
		writeError(writer, err)
		return
	}
	suite, err := eval.EvaluateSuite(cases, baseline, payload.Policy)
	if err != nil {
		writeError(writer, fmt.Errorf("%w: %v", agentruntime.ErrInvalidEvalRun, err))
		return
	}

	runs := make([]agentruntime.EvalRun, 0, len(suite.Results))
	for _, result := range suite.Results {
		key := evalSuiteResultKey(result.CaseID, result.CaseVersion)
		definition, ok := definitions[key]
		if !ok {
			writeError(writer, fmt.Errorf("%w: result 缺少 case definition %s", agentruntime.ErrInvalidEvalRun, key))
			return
		}
		definitionJSON, marshalErr := json.Marshal(definition)
		if marshalErr != nil {
			writeError(writer, fmt.Errorf("序列化 EvalCase 失败: %w", marshalErr))
			return
		}
		resultJSON, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			writeError(writer, fmt.Errorf("序列化 EvalResult 失败: %w", marshalErr))
			return
		}
		failed, hardFailed := 0, 0
		for _, assertion := range result.Assertions {
			if !assertion.Passed {
				failed++
				if assertion.Hard {
					hardFailed++
				}
			}
		}
		run, createErr := runtime.CreateEvalRun(request.Context(), agentruntime.EvalRun{
			InvocationID: invocationIDs[key], CaseID: result.CaseID, CaseVersion: result.CaseVersion,
			Kind: "deterministic_suite", Status: agentruntime.EvalRunCompleted, Passed: result.Passed, HardFailure: result.HardFailure,
			AssertionCount: len(result.Assertions), FailedAssertions: failed, HardFailedAssertions: hardFailed,
			Definition: definitionJSON, Result: resultJSON,
		})
		if createErr != nil {
			writeError(writer, createErr)
			return
		}
		runs = append(runs, run)
	}
	// Results are already sorted by EvaluateSuite; keep an explicit stable
	// order on persisted records even if a future Runtime repository changes
	// its timestamp/id generation strategy.
	sort.SliceStable(runs, func(i, j int) bool {
		return evalSuiteResultKey(runs[i].CaseID, runs[i].CaseVersion) < evalSuiteResultKey(runs[j].CaseID, runs[j].CaseVersion)
	})
	writeJSON(writer, http.StatusCreated, map[string]any{"runs": runs, "gate": suite.Gate})
}

func (s *Server) loadEvalResults(request *http.Request, ids []string) ([]eval.Result, error) {
	if len(ids) > eval.MaxSuiteCases {
		return nil, fmt.Errorf("%w: baseline_run_ids 数量不能超过 %d", agentruntime.ErrInvalidEvalRun, eval.MaxSuiteCases)
	}
	results := make([]eval.Result, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, rawID := range ids {
		id := strings.TrimSpace(rawID)
		if id == "" {
			return nil, fmt.Errorf("%w: baseline run id 不能为空", agentruntime.ErrInvalidEvalRun)
		}
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("%w: baseline run id 重复 %s", agentruntime.ErrInvalidEvalRun, id)
		}
		seen[id] = struct{}{}
		run, err := s.requireRuntimeEvalRun(request, id)
		if err != nil {
			return nil, err
		}
		result, err := decodeEvalResult(run)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (s *Server) requireRuntimeEvalRun(request *http.Request, id string) (agentruntime.EvalRun, error) {
	runtime, err := s.requireRuntime()
	if err != nil {
		return agentruntime.EvalRun{}, err
	}
	return runtime.GetEvalRun(request.Context(), id)
}

func evalSuiteResultKey(id, version string) string {
	return strings.TrimSpace(id) + "@" + strings.TrimSpace(version)
}
