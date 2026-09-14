package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

func (s *Server) listRuntimeConfigDirectoryFanouts(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	status := agentruntime.RuntimeConfigDirectoryFanoutStatus(strings.TrimSpace(request.URL.Query().Get("status")))
	if status != "" && !status.ValidForHTTP() {
		writeError(writer, fmt.Errorf("Runtime config directory fanout status 无效"))
		return
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
	items, err := runtime.ListRuntimeConfigDirectoryFanouts(request.Context(), strings.TrimSpace(request.URL.Query().Get("source")), status, limit)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, items)
}

func (s *Server) getRuntimeConfigDirectoryFanout(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	id := strings.TrimSpace(request.PathValue("id"))
	if id == "" {
		writeError(writer, fmt.Errorf("fanout plan ID 不能为空"))
		return
	}
	item, err := runtime.ReconcileRuntimeConfigDirectoryFanout(request.Context(), id, time.Now().UTC())
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}
