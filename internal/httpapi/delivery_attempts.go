package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	agentruntime "Abot/internal/agent/runtime"
)

func (s *Server) listInvocationDeliveryAttempts(writer http.ResponseWriter, request *http.Request) {
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
	kind := agentruntime.RuntimeDeliveryKind(strings.TrimSpace(request.URL.Query().Get("kind")))
	if kind != "" {
		switch kind {
		case agentruntime.RuntimeDeliveryKindEvent, agentruntime.RuntimeDeliveryKindCheckpoint, agentruntime.RuntimeDeliveryKindConfig, agentruntime.RuntimeDeliveryKindRejection:
		default:
			writeError(writer, fmt.Errorf("delivery kind 无效"))
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
	items, err := runtime.ListRuntimeDeliveryAttempts(request.Context(), invocationID, kind, limit)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, items)
}
