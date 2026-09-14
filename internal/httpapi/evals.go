package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"Abot/internal/agent/eval"
	agentruntime "Abot/internal/agent/runtime"
)

type evalRunPayload struct {
	InvocationID string           `json:"invocation_id"`
	Case         *eval.EvalCase   `json:"case"`
	CaseID       string           `json:"case_id"`
	CaseVersion  string           `json:"case_version"`
	Expected     string           `json:"expected_outcome"`
	Assertions   []eval.Assertion `json:"assertions"`
}

func (s *Server) createEvalRun(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload evalRunPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	definition := eval.EvalCase{}
	if payload.Case != nil {
		definition = *payload.Case
	} else {
		definition = eval.EvalCase{ID: payload.CaseID, Version: payload.CaseVersion, ExpectedOutcome: payload.Expected, Assertions: payload.Assertions}
	}
	definition.ID = strings.TrimSpace(definition.ID)
	definition.Version = strings.TrimSpace(definition.Version)
	if err := definition.Validate(); err != nil {
		writeError(writer, fmt.Errorf("%w: %v", agentruntime.ErrInvalidEvalRun, err))
		return
	}
	input, err := runtime.GetEvalInput(request.Context(), payload.InvocationID)
	if err != nil {
		writeError(writer, err)
		return
	}
	result := eval.Evaluate(definition, eval.Input{Invocation: input.Invocation, Events: input.Events, ToolCalls: input.ToolCalls, Approvals: input.Approvals})
	definitionJSON, err := json.Marshal(definition)
	if err != nil {
		writeError(writer, fmt.Errorf("序列化 EvalCase 失败: %w", err))
		return
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		writeError(writer, fmt.Errorf("序列化 EvalResult 失败: %w", err))
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
	run, err := runtime.CreateEvalRun(request.Context(), agentruntime.EvalRun{
		InvocationID: input.Invocation.ID, CaseID: result.CaseID, CaseVersion: result.CaseVersion,
		Kind: "deterministic", Status: agentruntime.EvalRunCompleted, Passed: result.Passed, HardFailure: result.HardFailure,
		AssertionCount: len(result.Assertions), FailedAssertions: failed, HardFailedAssertions: hardFailed,
		Definition: definitionJSON, Result: resultJSON,
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	writer.Header().Set("Location", "/api/v1/evals/runs/"+run.ID)
	writeJSON(writer, http.StatusCreated, run)
}

func (s *Server) listEvalRuns(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := runtime.ListEvalRuns(request.Context(), request.URL.Query().Get("invocation_id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"runs": items})
}

func (s *Server) getEvalRun(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := runtime.GetEvalRun(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func decodeEvalResult(run agentruntime.EvalRun) (eval.Result, error) {
	if run.Status != agentruntime.EvalRunCompleted {
		return eval.Result{}, fmt.Errorf("%w: run %s 尚未完成", agentruntime.ErrInvalidEvalRun, run.ID)
	}
	if len(run.Result) == 0 {
		return eval.Result{}, fmt.Errorf("%w: run %s 缺少 result", agentruntime.ErrInvalidEvalRun, run.ID)
	}
	var result eval.Result
	if err := json.Unmarshal(run.Result, &result); err != nil {
		return eval.Result{}, fmt.Errorf("%w: run %s result 无效", agentruntime.ErrInvalidEvalRun, run.ID)
	}
	return result, nil
}

func (s *Server) compareEvalRuns(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	baseID, candidateID := strings.TrimSpace(request.URL.Query().Get("base")), strings.TrimSpace(request.URL.Query().Get("candidate"))
	if baseID == "" || candidateID == "" {
		writeError(writer, fmt.Errorf("%w: base 和 candidate 不能为空", agentruntime.ErrInvalidEvalRun))
		return
	}
	baseRun, err := runtime.GetEvalRun(request.Context(), baseID)
	if err != nil {
		writeError(writer, err)
		return
	}
	candidateRun, err := runtime.GetEvalRun(request.Context(), candidateID)
	if err != nil {
		writeError(writer, err)
		return
	}
	base, err := decodeEvalResult(baseRun)
	if err != nil {
		writeError(writer, err)
		return
	}
	candidate, err := decodeEvalResult(candidateRun)
	if err != nil {
		writeError(writer, err)
		return
	}
	comparison := eval.CompareResults(base, candidate)
	if !comparison.Comparable {
		writeError(writer, fmt.Errorf("%w: eval baseline 与 candidate 不兼容", agentruntime.ErrConflict))
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"base_run_id": baseRun.ID, "candidate_run_id": candidateRun.ID, "comparison": comparison})
}

type evalGatePayload struct {
	CandidateRunIDs []string        `json:"candidate_run_ids"`
	BaselineRunIDs  []string        `json:"baseline_run_ids"`
	Policy          eval.GatePolicy `json:"policy"`
}

func (s *Server) evaluateEvalGate(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload evalGatePayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if len(payload.CandidateRunIDs) == 0 || len(payload.CandidateRunIDs) > 100 || len(payload.BaselineRunIDs) > 100 {
		writeError(writer, fmt.Errorf("%w: candidate_run_ids 数量必须在 1 到 100 之间", agentruntime.ErrInvalidEvalRun))
		return
	}
	loadResults := func(ids []string) ([]eval.Result, error) {
		results := make([]eval.Result, 0, len(ids))
		seen := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id == "" {
				return nil, fmt.Errorf("%w: run id 不能为空", agentruntime.ErrInvalidEvalRun)
			}
			if _, ok := seen[id]; ok {
				return nil, fmt.Errorf("%w: run id 重复 %s", agentruntime.ErrInvalidEvalRun, id)
			}
			seen[id] = struct{}{}
			run, getErr := runtime.GetEvalRun(request.Context(), id)
			if getErr != nil {
				return nil, getErr
			}
			result, decodeErr := decodeEvalResult(run)
			if decodeErr != nil {
				return nil, decodeErr
			}
			results = append(results, result)
		}
		return results, nil
	}
	candidates, err := loadResults(payload.CandidateRunIDs)
	if err != nil {
		writeError(writer, err)
		return
	}
	baseline, err := loadResults(payload.BaselineRunIDs)
	if err != nil {
		writeError(writer, err)
		return
	}
	gate, err := eval.EvaluateReleaseGate(candidates, baseline, payload.Policy)
	if err != nil {
		writeError(writer, fmt.Errorf("%w: %v", agentruntime.ErrInvalidEvalRun, err))
		return
	}
	writeJSON(writer, http.StatusOK, gate)
}
