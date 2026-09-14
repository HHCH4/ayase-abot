package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	agentruntime "Abot/internal/agent/runtime"
)

// listInvocationDeliveryGroupSagas returns only the source-owned decision
// journal. Saga rows intentionally have no lease owner, payload, or provider
// credentials; the endpoint is therefore safe to use for recovery dashboards.
func (s *Server) listInvocationDeliveryGroupSagas(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	invocationID := strings.TrimSpace(request.PathValue("id"))
	if invocationID == "" {
		writeError(writer, fmt.Errorf("invocation ID 不能为空"))
		return
	}
	state := agentruntime.RuntimeDeliveryGroupSagaState(strings.TrimSpace(request.URL.Query().Get("state")))
	if state != "" {
		switch state {
		case agentruntime.RuntimeDeliveryGroupSagaDecided, agentruntime.RuntimeDeliveryGroupSagaPreparing,
			agentruntime.RuntimeDeliveryGroupSagaPrepared, agentruntime.RuntimeDeliveryGroupSagaCommitting,
			agentruntime.RuntimeDeliveryGroupSagaAborting, agentruntime.RuntimeDeliveryGroupSagaCommitted,
			agentruntime.RuntimeDeliveryGroupSagaAborted, agentruntime.RuntimeDeliveryGroupSagaCompensationRequired,
			agentruntime.RuntimeDeliveryGroupSagaFailed:
		default:
			writeError(writer, fmt.Errorf("delivery group saga state 无效"))
			return
		}
	}
	limit := 0
	if raw := strings.TrimSpace(request.URL.Query().Get("limit")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 1 || parsed > 5000 {
			writeError(writer, fmt.Errorf("limit 必须是 1-5000 的整数"))
			return
		}
		limit = parsed
	}
	items, err := runtime.ListRuntimeDeliveryGroupSagas(request.Context(), invocationID, state, limit)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, items)
}
