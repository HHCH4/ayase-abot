package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"Abot/internal/agent"
)

// subAgentGroupLister 是 SQLite 仓储对任务树查询的可选扩展；保留可选接口，
// 使已有嵌入方只实现基本 Group/Run 存储时仍能正常启动 API。
type subAgentGroupLister interface {
	ListSubAgentGroups(context.Context, string, int) ([]agent.SubAgentGroup, error)
}

func (s *Server) requireSubAgentStore() (agent.SubAgentRuntimeStore, error) {
	if s.subAgents == nil {
		return nil, errors.New("子 Agent Runtime 存储尚未装配")
	}
	return s.subAgents, nil
}

func subAgentAPILimit(request *http.Request) int {
	limit, err := strconv.Atoi(strings.TrimSpace(request.URL.Query().Get("limit")))
	if err != nil || limit <= 0 {
		return 100
	}
	if limit > 256 {
		return 256
	}
	return limit
}

func subAgentAPIOffset(request *http.Request) int {
	offset, err := strconv.Atoi(strings.TrimSpace(request.URL.Query().Get("offset")))
	if err != nil || offset < 0 {
		return 0
	}
	if offset > 100000 {
		return 100000
	}
	return offset
}

func (s *Server) listInvocationSubAgents(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	store, err := s.requireSubAgentStore()
	if err != nil {
		writeError(writer, err)
		return
	}
	invocationID := request.PathValue("id")
	if _, err := runtime.GetInvocation(request.Context(), invocationID); err != nil {
		writeError(writer, err)
		return
	}
	lister, ok := store.(subAgentGroupLister)
	if !ok {
		writeError(writer, errors.New("当前 Runtime 存储不支持子 Agent Group 列表"))
		return
	}
	groups, err := lister.ListSubAgentGroups(request.Context(), invocationID, subAgentAPILimit(request))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"groups": groups})
}

func (s *Server) getSubAgentGroup(writer http.ResponseWriter, request *http.Request) {
	store, err := s.requireSubAgentStore()
	if err != nil {
		writeError(writer, err)
		return
	}
	group, err := store.GetSubAgentGroup(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	runs, err := store.ListSubAgentRuns(request.Context(), group.ID, subAgentAPILimit(request))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"group": group, "runs": runs})
}

func (s *Server) listSubAgentEvidence(writer http.ResponseWriter, request *http.Request) {
	store, err := s.requireSubAgentStore()
	if err != nil {
		writeError(writer, err)
		return
	}
	groupID := request.PathValue("id")
	if _, err := store.GetSubAgentGroup(request.Context(), groupID); err != nil {
		writeError(writer, err)
		return
	}
	runs, err := store.ListSubAgentRuns(request.Context(), groupID, 256)
	if err != nil {
		writeError(writer, err)
		return
	}
	// 证据正文只允许以有界 JSON 元数据写入 Run；未来适配器可以填充
	// evidence_json，当前没有证据时明确返回空数组，不能把普通模型文本冒充来源。
	evidence := make([]agent.EvidenceItem, 0)
	for _, run := range runs {
		raw := strings.TrimSpace(run.ResultMetadata["evidence_json"])
		if raw == "" {
			continue
		}
		var items []agent.EvidenceItem
		if err := json.Unmarshal([]byte(raw), &items); err != nil {
			continue
		}
		for _, item := range items {
			if strings.TrimSpace(item.EvidenceID) != "" && strings.TrimSpace(item.SourceKind) != "" && strings.TrimSpace(item.SourceID) != "" && strings.TrimSpace(item.Excerpt) != "" {
				evidence = append(evidence, item)
			}
		}
	}
	limit := subAgentAPILimit(request)
	offset := subAgentAPIOffset(request)
	if offset > len(evidence) {
		offset = len(evidence)
	}
	end := offset + limit
	if end > len(evidence) {
		end = len(evidence)
	}
	page := evidence[offset:end]
	hasMore := end < len(evidence)
	response := map[string]any{"evidence": page, "offset": offset, "limit": limit, "total": len(evidence), "has_more": hasMore}
	if hasMore {
		response["next_offset"] = end
	}
	writeJSON(writer, http.StatusOK, response)
}
