package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	agentruntime "Abot/internal/agent/runtime"
)

// listInvocationSubAgents 返回主任务下的子 Agent 编排记录，前端可以据此展示
// 图片解析、检索和汇总的实时生命周期，而不需要读取内部模型上下文。
func (s *Server) listInvocationSubAgents(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	if _, err := runtime.GetInvocation(request.Context(), request.PathValue("id")); err != nil {
		writeError(writer, err)
		return
	}
	status := agentruntime.SubAgentGroupStatus(strings.TrimSpace(request.URL.Query().Get("status")))
	if status != "" && !validSubAgentGroupStatus(status) {
		writeError(writer, agentruntime.ErrConflict)
		return
	}
	limit := parseSubAgentLimit(request.URL.Query().Get("limit"), 64)
	items, err := runtime.ListSubAgentGroups(request.Context(), request.PathValue("id"), status, limit)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, items)
}

type subAgentGroupDetailResponse struct {
	Group    agentruntime.SubAgentGroup  `json:"group"`
	Runs     []agentruntime.SubAgentRun  `json:"runs"`
	Evidence []agentruntime.EvidenceItem `json:"evidence,omitempty"`
}

func (s *Server) getSubAgentGroup(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	group, err := runtime.GetSubAgentGroup(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	runs, err := runtime.ListSubAgentRuns(request.Context(), group.ID, "", 64)
	if err != nil {
		writeError(writer, err)
		return
	}
	evidence, err := runtime.ListSubAgentEvidence(request.Context(), group.ID, 50, "")
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, subAgentGroupDetailResponse{Group: group, Runs: runs, Evidence: evidence})
}

func (s *Server) listSubAgentEvidence(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	limit := parseSubAgentLimit(request.URL.Query().Get("limit"), 50)
	items, err := runtime.ListSubAgentEvidence(request.Context(), request.PathValue("id"), limit, strings.TrimSpace(request.URL.Query().Get("after")))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, items)
}

func (s *Server) runRetrieval(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload agentruntime.RetrievalRequest
	if err := decodeSingleJSONWithLimit(writer, request, &payload, 64<<10); err != nil {
		writeError(writer, err)
		return
	}
	result, err := runtime.RunRetrieval(request.Context(), payload)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

type builtInSubAgentGroupResponse struct {
	Results []agentruntime.BuiltInSubAgentTaskResult `json:"results"`
}

// runBuiltInSubAgentGroup 暴露受限的内置文本子 Agent 编排入口。HTTP 层只
// 负责解码和返回结果，实际 profile、并发、超时、Prompt 大小和模型权限校验由 Runtime 完成。
func (s *Server) runBuiltInSubAgentGroup(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload agentruntime.BuiltInSubAgentGroupRequest
	if err := decodeSingleJSONWithLimit(writer, request, &payload, 640<<10); err != nil {
		writeError(writer, err)
		return
	}
	results, err := runtime.RunBuiltInSubAgentGroup(request.Context(), payload)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, builtInSubAgentGroupResponse{Results: results})
}

func validSubAgentGroupStatus(status agentruntime.SubAgentGroupStatus) bool {
	switch status {
	case agentruntime.SubAgentGroupQueued, agentruntime.SubAgentGroupRunning, agentruntime.SubAgentGroupCompleted, agentruntime.SubAgentGroupPartial, agentruntime.SubAgentGroupFailed, agentruntime.SubAgentGroupCancelled, agentruntime.SubAgentGroupExpired:
		return true
	default:
		return false
	}
}

func parseSubAgentLimit(raw string, fallback int) int {
	limit, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || limit <= 0 {
		return fallback
	}
	if limit > 32 {
		return 32
	}
	return limit
}
