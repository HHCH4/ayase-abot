package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

type runtimeConfigDirectoryRebindPlanPayload struct {
	Source                       string   `json:"source"`
	InvocationID                 string   `json:"invocation_id"`
	Destinations                 []string `json:"destinations"`
	ExpectedConfigSnapshotDigest string   `json:"expected_config_snapshot_digest"`
	ConfigFanoutID               string   `json:"config_fanout_id,omitempty"`
	IdempotencyKey               string   `json:"idempotency_key,omitempty"`
}

type runtimeConfigDirectoryRebindConfirmationPayload struct {
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type runtimeConfigDirectoryRebindApplyPayload struct {
	ConfirmationID string `json:"confirmation_id"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// decodeRuntimeConfigDirectoryRebindJSONWithLimit is deliberately stricter
// than the legacy request decoder: confirmation/apply commands are state
// changing and must contain exactly one JSON document.  Accepting a second
// document could otherwise make proxies and the application disagree about
// which command was submitted.
func decodeRuntimeConfigDirectoryRebindJSONWithLimit(writer http.ResponseWriter, request *http.Request, target any, limit int64) error {
	if request == nil || request.Body == nil {
		return fmt.Errorf("请求体不能为空")
	}
	request.Body = http.MaxBytesReader(writer, request.Body, limit)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("请求包含多个 JSON 文档")
		}
		return fmt.Errorf("请求包含额外内容: %w", err)
	}
	return nil
}

func (s *Server) createRuntimeConfigDirectoryRebindPlan(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload runtimeConfigDirectoryRebindPlanPayload
	if err := decodeJSONWithLimit(writer, request, &payload, 32<<10); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	idempotencyKey := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(payload.IdempotencyKey)
	}
	item, err := runtime.CreateRuntimeConfigDirectoryRebindPlan(request.Context(), payload.Source, payload.InvocationID, payload.Destinations, payload.ExpectedConfigSnapshotDigest, payload.ConfigFanoutID, idempotencyKey, time.Now().UTC())
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, item)
}

func (s *Server) listRuntimeConfigDirectoryRebindPlans(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	status := agentruntime.RuntimeConfigDirectoryRebindStatus(strings.TrimSpace(request.URL.Query().Get("status")))
	if status != "" && !status.ValidForHTTP() {
		writeError(writer, fmt.Errorf("%w: rebind status 无效", agentruntime.ErrInvalidRuntimeConfigDirectoryRebind))
		return
	}
	limit := 0
	if raw := strings.TrimSpace(request.URL.Query().Get("limit")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 1 || parsed > 5000 {
			writeError(writer, fmt.Errorf("%w: limit 必须是 1-5000 的整数", agentruntime.ErrInvalidRuntimeConfigDirectoryRebind))
			return
		}
		limit = parsed
	}
	items, err := runtime.ListRuntimeConfigDirectoryRebindPlans(request.Context(), strings.TrimSpace(request.URL.Query().Get("source")), strings.TrimSpace(request.URL.Query().Get("invocation_id")), status, limit)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, items)
}

func (s *Server) getRuntimeConfigDirectoryRebindPlan(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	id := strings.TrimSpace(request.PathValue("id"))
	if id == "" {
		writeError(writer, fmt.Errorf("%w: rebind plan ID 不能为空", agentruntime.ErrInvalidRuntimeConfigDirectoryRebind))
		return
	}
	item, err := runtime.GetRuntimeConfigDirectoryRebindPlan(request.Context(), id)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) confirmRuntimeConfigDirectoryRebindPlan(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload runtimeConfigDirectoryRebindConfirmationPayload
	if err := decodeRuntimeConfigDirectoryRebindJSONWithLimit(writer, request, &payload, 8<<10); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	idempotencyKey := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(payload.IdempotencyKey)
	}
	item, err := runtime.ConfirmRuntimeConfigDirectoryRebind(request.Context(), request.PathValue("id"), idempotencyKey, time.Now().UTC())
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, item)
}

func (s *Server) applyRuntimeConfigDirectoryRebindPlan(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload runtimeConfigDirectoryRebindApplyPayload
	if err := decodeRuntimeConfigDirectoryRebindJSONWithLimit(writer, request, &payload, 8<<10); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	idempotencyKey := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(payload.IdempotencyKey)
	}
	item, err := runtime.ApplyRuntimeConfigDirectoryRebind(request.Context(), request.PathValue("id"), payload.ConfirmationID, idempotencyKey, time.Now().UTC())
	if err != nil {
		writeError(writer, err)
		return
	}
	status := http.StatusAccepted
	if item.Status == agentruntime.RuntimeConfigDirectoryRebindApplyCompleted {
		status = http.StatusOK
	}
	writeJSON(writer, status, item)
}

func (s *Server) listRuntimeConfigDirectoryRebindApplies(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	status := agentruntime.RuntimeConfigDirectoryRebindApplyStatus(strings.TrimSpace(request.URL.Query().Get("status")))
	if status != "" {
		switch status {
		case agentruntime.RuntimeConfigDirectoryRebindApplyQueued, agentruntime.RuntimeConfigDirectoryRebindApplyProcessing, agentruntime.RuntimeConfigDirectoryRebindApplyCompleted, agentruntime.RuntimeConfigDirectoryRebindApplyFailed:
		default:
			writeError(writer, agentruntime.ErrInvalidRuntimeConfigDirectoryRebindApply)
			return
		}
	}
	limit := 0
	if raw := strings.TrimSpace(request.URL.Query().Get("limit")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 1 || parsed > 5000 {
			writeError(writer, fmt.Errorf("%w: limit 必须是 1-5000 的整数", agentruntime.ErrInvalidRuntimeConfigDirectoryRebindApply))
			return
		}
		limit = parsed
	}
	items, err := runtime.ListRuntimeConfigDirectoryRebindApplies(request.Context(), request.PathValue("id"), "", status, limit)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, items)
}

func (s *Server) confirmRuntimeConfigDirectoryRebindMultiPlan(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload runtimeConfigDirectoryRebindConfirmationPayload
	if err := decodeRuntimeConfigDirectoryRebindJSONWithLimit(writer, request, &payload, 8<<10); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	idempotencyKey := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(payload.IdempotencyKey)
	}
	item, err := runtime.ConfirmRuntimeConfigDirectoryRebindMulti(request.Context(), request.PathValue("id"), idempotencyKey, time.Now().UTC())
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, item)
}

func (s *Server) applyRuntimeConfigDirectoryRebindMultiPlan(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload runtimeConfigDirectoryRebindApplyPayload
	if err := decodeRuntimeConfigDirectoryRebindJSONWithLimit(writer, request, &payload, 8<<10); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	idempotencyKey := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(payload.IdempotencyKey)
	}
	item, err := runtime.ApplyRuntimeConfigDirectoryRebindMulti(request.Context(), request.PathValue("id"), payload.ConfirmationID, idempotencyKey, time.Now().UTC())
	if err != nil {
		writeError(writer, err)
		return
	}
	status := http.StatusAccepted
	if item.Status == agentruntime.RuntimeConfigDirectoryRebindMultiApplyCompleted {
		status = http.StatusOK
	}
	writeJSON(writer, status, item)
}

func (s *Server) listRuntimeConfigDirectoryRebindMultiApplies(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	status := agentruntime.RuntimeConfigDirectoryRebindMultiApplyStatus(strings.TrimSpace(request.URL.Query().Get("status")))
	if status != "" && !status.ValidForHTTP() {
		writeError(writer, agentruntime.ErrInvalidRuntimeConfigDirectoryRebindMultiApply)
		return
	}
	limit := 0
	if raw := strings.TrimSpace(request.URL.Query().Get("limit")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 1 || parsed > 5000 {
			writeError(writer, fmt.Errorf("%w: limit 必须是 1-5000 的整数", agentruntime.ErrInvalidRuntimeConfigDirectoryRebindMultiApply))
			return
		}
		limit = parsed
	}
	items, err := runtime.ListRuntimeConfigDirectoryRebindMultiApplies(request.Context(), request.PathValue("id"), "", status, limit)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, items)
}

func (s *Server) getRuntimeConfigDirectoryRebindMultiApply(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := runtime.ReconcileRuntimeConfigDirectoryRebindMultiApply(request.Context(), request.PathValue("id"), time.Now().UTC())
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}
