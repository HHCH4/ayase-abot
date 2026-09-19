package httpapi

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/logging"
)

func (s *Server) dataStats(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	rangeDays := 1
	switch strings.TrimSpace(request.URL.Query().Get("range")) {
	case "3d", "3":
		rangeDays = 3
	case "7d", "7", "1w":
		rangeDays = 7
	}
	now := time.Now().UTC()
	start := now.Add(-time.Duration(rangeDays) * 24 * time.Hour)
	items, err := runtime.ListInvocations(request.Context(), "", nil)
	if err != nil {
		writeError(writer, err)
		return
	}
	type modelStat struct {
		Model       string  `json:"model"`
		Calls       int     `json:"calls"`
		Tokens      int     `json:"tokens"`
		SuccessRate float64 `json:"success_rate"`
	}
	byModel := make(map[string]*modelStat)
	byDay := make(map[string]int)
	stats := struct {
		InvocationCount int     `json:"invocation_count"`
		MessageCount    int     `json:"message_count"`
		ModelCalls      int     `json:"model_calls"`
		TotalTokens     int     `json:"total_tokens"`
		SuccessCount    int     `json:"success_count"`
		FailedCount     int     `json:"failed_count"`
		AvgResponseMS   float64 `json:"avg_response_ms"`
	}{}
	responseMillis := int64(0)
	for _, item := range items {
		if item.CreatedAt.Before(start) || item.CreatedAt.After(now.Add(time.Minute)) {
			continue
		}
		stats.InvocationCount++
		stats.MessageCount++
		day := item.CreatedAt.UTC().Format("2006-01-02")
		byDay[day]++
		if item.Status == agentruntime.InvocationCompleted {
			stats.SuccessCount++
		} else if item.Status == agentruntime.InvocationFailed {
			stats.FailedCount++
		}
		if item.FinishedAt != nil {
			responseMillis += item.FinishedAt.Sub(item.CreatedAt).Milliseconds()
		}
		trace, traceErr := runtime.GetInvocationTrace(request.Context(), item.ID)
		if traceErr != nil {
			continue
		}
		stats.ModelCalls += trace.Metrics.ModelCalls
		if trace.Usage.TotalTokens.Known {
			stats.TotalTokens += trace.Usage.TotalTokens.Value
		}
		modelID := strings.TrimSpace(item.ModelID)
		if modelID == "" {
			modelID = "未指定模型"
		}
		model := byModel[modelID]
		if model == nil {
			model = &modelStat{Model: modelID}
			byModel[modelID] = model
		}
		model.Calls += trace.Metrics.ModelCalls
		if trace.Usage.TotalTokens.Known {
			model.Tokens += trace.Usage.TotalTokens.Value
		}
		if item.Status == agentruntime.InvocationCompleted {
			model.SuccessRate++
		}
	}
	if stats.InvocationCount > 0 {
		stats.AvgResponseMS = float64(responseMillis) / float64(stats.InvocationCount)
	}
	modelRanking := make([]modelStat, 0, len(byModel))
	for _, item := range byModel {
		if item.Calls > 0 {
			item.SuccessRate /= float64(item.Calls)
		} else {
			item.SuccessRate = 0
		}
		modelRanking = append(modelRanking, *item)
	}
	sort.Slice(modelRanking, func(i, j int) bool {
		if modelRanking[i].Calls != modelRanking[j].Calls {
			return modelRanking[i].Calls > modelRanking[j].Calls
		}
		return modelRanking[i].Model < modelRanking[j].Model
	})
	trend := make([]map[string]any, 0, rangeDays)
	for day := start.Truncate(24 * time.Hour); !day.After(now); day = day.Add(24 * time.Hour) {
		key := day.Format("2006-01-02")
		trend = append(trend, map[string]any{"date": key, "messages": byDay[key]})
	}
	platformInstances := 0
	if s.providers != nil {
		platformInstances = len(s.providers.List())
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"range_days": rangeDays, "overview": stats, "message_trend": trend, "model_ranking": modelRanking,
		"platform_instances": platformInstances,
	})
}

func (s *Server) dataConversations(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConversations()
	if err != nil {
		writeError(writer, err)
		return
	}
	userID := strings.TrimSpace(request.URL.Query().Get("user_id"))
	if userID == "" {
		userID = "webui-user"
	}
	items, err := service.List(request.Context(), userID, "*", true)
	if err != nil {
		writeError(writer, err)
		return
	}
	query := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("q")))
	status := strings.TrimSpace(request.URL.Query().Get("status"))
	filtered := make([]any, 0, len(items))
	for _, item := range items {
		if status != "" && string(item.Status) != status {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(item.Title), query) && !strings.Contains(strings.ToLower(item.ID), query) {
			continue
		}
		filtered = append(filtered, item)
	}
	page, pageSize := pageValues(request)
	start := (page - 1) * pageSize
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + pageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	writeJSON(writer, http.StatusOK, map[string]any{"conversations": filtered[start:end], "page": page, "page_size": pageSize, "total": len(filtered)})
}

func (s *Server) dataTraces(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := runtime.ListInvocations(request.Context(), strings.TrimSpace(request.URL.Query().Get("user_id")), nil)
	if err != nil {
		writeError(writer, err)
		return
	}
	query := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("q")))
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if query != "" && !strings.Contains(strings.ToLower(item.ID), query) && !strings.Contains(strings.ToLower(item.ConversationID), query) && !strings.Contains(strings.ToLower(item.Message), query) {
			continue
		}
		result = append(result, map[string]any{
			"id": item.ID, "invocation_id": item.ID, "conversation_id": item.ConversationID, "user_id": item.UserID,
			"status": item.Status, "provider_id": item.ProviderID, "model_id": item.ModelID, "message": item.Message,
			"created_at": item.CreatedAt, "updated_at": item.UpdatedAt,
		})
	}
	page, pageSize := pageValues(request)
	start := (page - 1) * pageSize
	if start > len(result) {
		start = len(result)
	}
	end := start + pageSize
	if end > len(result) {
		end = len(result)
	}
	writeJSON(writer, http.StatusOK, map[string]any{"traces": result[start:end], "page": page, "page_size": pageSize, "total": len(result)})
}

func (s *Server) dataLogs(writer http.ResponseWriter, request *http.Request) {
	if s.logs == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"logs": []logging.Entry{}})
		return
	}
	level := request.URL.Query().Get("level")
	writeJSON(writer, http.StatusOK, map[string]any{"logs": s.logs.List(level, 500), "realtime": true})
}

func pageValues(request *http.Request) (int, int) {
	page, _ := strconv.Atoi(request.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(request.URL.Query().Get("page_size"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	return page, pageSize
}
