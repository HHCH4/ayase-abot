package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	agentruntime "Abot/internal/agent/runtime"
)

func (s *Server) listInvocationDeliveryGroupSettlements(writer http.ResponseWriter, request *http.Request) {
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
	status := agentruntime.RuntimeDeliveryGroupSettlementStatus(strings.TrimSpace(request.URL.Query().Get("status")))
	if status != "" {
		switch status {
		case agentruntime.RuntimeDeliveryGroupSettlementQueued, agentruntime.RuntimeDeliveryGroupSettlementProcessing,
			agentruntime.RuntimeDeliveryGroupSettlementCompleted, agentruntime.RuntimeDeliveryGroupSettlementFailed:
		default:
			writeError(writer, fmt.Errorf("delivery group settlement status 无效"))
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
	items, err := runtime.ListRuntimeDeliveryGroupSettlements(request.Context(), invocationID, status, limit)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, items)
}
