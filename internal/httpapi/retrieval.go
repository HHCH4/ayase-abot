package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"Abot/internal/agent"
)

type retrievalPayload struct {
	InvocationID                   string                      `json:"invocation_id,omitempty"`
	Query                          string                      `json:"query"`
	QueryType                      string                      `json:"query_type,omitempty"`
	UserID                         string                      `json:"user_id"`
	ConversationID                 string                      `json:"conversation_id,omitempty"`
	SessionID                      string                      `json:"session_id,omitempty"`
	SourceKinds                    []agent.RetrievalSourceKind `json:"source_kinds"`
	SourceIDs                      []string                    `json:"source_ids,omitempty"`
	Scope                          []string                    `json:"scope,omitempty"`
	Filters                        map[string]string           `json:"filters,omitempty"`
	TopK                           int                         `json:"top_k,omitempty"`
	RerankEnabled                  bool                        `json:"rerank_enabled,omitempty"`
	Freshness                      string                      `json:"freshness,omitempty"`
	MaxQueryCount                  int                         `json:"max_query_count,omitempty"`
	MaxResultBytes                 int64                       `json:"max_result_bytes,omitempty"`
	WebSearchDailyCallLimit        int                         `json:"web_search_daily_call_limit,omitempty"`
	WebSearchMaxCallsPerInvocation int                         `json:"web_search_max_calls_per_invocation,omitempty"`
	WebSearchAlertPercent          int                         `json:"web_search_alert_percent,omitempty"`
	PermissionDigest               string                      `json:"permission_digest,omitempty"`
	RequestedBy                    string                      `json:"requested_by,omitempty"`
}

func (p retrievalPayload) request() agent.RetrievalRequest {
	return agent.RetrievalRequest{
		InvocationID: p.InvocationID, Query: p.Query, QueryType: p.QueryType, UserID: p.UserID,
		ConversationID: p.ConversationID, SessionID: p.SessionID, SourceKinds: p.SourceKinds,
		SourceIDs: p.SourceIDs, Scope: p.Scope, Filters: p.Filters, TopK: p.TopK,
		RerankEnabled: p.RerankEnabled, Freshness: p.Freshness, MaxQueryCount: p.MaxQueryCount,
		MaxResultBytes: p.MaxResultBytes, WebSearchDailyCallLimit: p.WebSearchDailyCallLimit, WebSearchMaxCallsPerInvocation: p.WebSearchMaxCallsPerInvocation, WebSearchAlertPercent: p.WebSearchAlertPercent,
		PermissionDigest: p.PermissionDigest, RequestedBy: p.RequestedBy,
	}
}

func (s *Server) requireRetrieval() (*agent.RetrievalOrchestrator, error) {
	if s == nil || s.retrieval == nil {
		return nil, errors.New("检索编排器尚未装配")
	}
	return s.retrieval, nil
}

func (s *Server) searchRetrieval(writer http.ResponseWriter, request *http.Request) {
	s.runRetrievalRequest(writer, request, false)
}

func (s *Server) refreshRetrieval(writer http.ResponseWriter, request *http.Request) {
	s.runRetrievalRequest(writer, request, true)
}

func (s *Server) runRetrievalRequest(writer http.ResponseWriter, request *http.Request, refresh bool) {
	orchestrator, err := s.requireRetrieval()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload retrievalPayload
	if err := decodeSingleJSONWithLimit(writer, request, &payload, 128<<10); err != nil {
		writeError(writer, err)
		return
	}
	retrievalRequest := payload.request()
	if refresh {
		// 刷新先使同一用户/会话/权限范围内的缓存失效，再执行当前请求；
		// 不允许用刷新动作读取超出本次 SourceIDs 的数据。
		orchestrator.Invalidate(retrievalRequest)
	}
	result, searchErr := orchestrator.Search(request.Context(), retrievalRequest)
	if searchErr != nil && len(result.Items) == 0 {
		writeJSON(writer, http.StatusUnprocessableEntity, retrievalErrorPayload(result, searchErr))
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func retrievalErrorPayload(result agent.RetrievalResult, err error) map[string]any {
	payload := map[string]any{"result": result, "error": "检索未完成"}
	var runtimeErr *agent.SubAgentRuntimeError
	if errors.As(err, &runtimeErr) && runtimeErr != nil {
		payload["error"] = runtimeErr.HumanMessage
		payload["code"] = runtimeErr.Code
		payload["retryable"] = runtimeErr.Retryable
		if runtimeErr.RequestID != "" {
			payload["request_id"] = runtimeErr.RequestID
		}
		if runtimeErr.GroupID != "" {
			payload["group_id"] = runtimeErr.GroupID
		}
		if runtimeErr.RunID != "" {
			payload["run_id"] = runtimeErr.RunID
		}
		return payload
	}
	// 非 Runtime 适配器错误也要保持稳定的公开文本，详细原因留在服务端日志。
	payload["code"] = "subagent_retrieval_adapter_unavailable"
	payload["retryable"] = true
	return payload
}

func normalizeRetrievalSourceKinds(values []agent.RetrievalSourceKind) []agent.RetrievalSourceKind {
	result := make([]agent.RetrievalSourceKind, 0, len(values))
	seen := make(map[agent.RetrievalSourceKind]struct{}, len(values))
	for _, value := range values {
		value = agent.RetrievalSourceKind(strings.ToLower(strings.TrimSpace(string(value))))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
