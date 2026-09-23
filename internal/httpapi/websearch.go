package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	configsvc "Abot/internal/config"
	"Abot/internal/websearch"
)

func (s *Server) listWebSearchServices(writer http.ResponseWriter, request *http.Request) {
	manager, err := s.requireWebSearchManager()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := manager.ListServices(request.Context())
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"services": items})
}

func (s *Server) saveWebSearchService(writer http.ResponseWriter, request *http.Request) {
	manager, err := s.requireWebSearchManager()
	if err != nil {
		writeError(writer, err)
		return
	}
	var input websearch.ServiceInput
	if err := decodeJSON(writer, request, &input); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if request.Method == http.MethodPut {
		input.ID = request.PathValue("id")
	}
	item, err := manager.SaveService(request.Context(), input)
	if err != nil {
		writeError(writer, err)
		return
	}
	status := http.StatusOK
	if request.Method == http.MethodPost {
		status = http.StatusCreated
	}
	writeJSON(writer, status, item)
}

func (s *Server) deleteWebSearchService(writer http.ResponseWriter, request *http.Request) {
	manager, err := s.requireWebSearchManager()
	if err != nil {
		writeError(writer, err)
		return
	}
	id := strings.TrimSpace(request.PathValue("id"))
	if s.config != nil {
		profiles, listErr := s.config.ListProfiles(request.Context())
		if listErr != nil {
			writeError(writer, listErr)
			return
		}
		for _, profile := range profiles {
			if containsWebSearchService(profile.Values["ai.web_search.service_ids"], id) {
				writeJSON(writer, http.StatusConflict, map[string]string{"error": "该服务仍被配置文件引用，请先从网页搜索优先级中移除"})
				return
			}
		}
	}
	if err := manager.DeleteService(request.Context(), id); err != nil {
		writeError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func containsWebSearchService(value any, id string) bool {
	if id == "" {
		return false
	}
	switch items := value.(type) {
	case []string:
		for _, item := range items {
			if strings.TrimSpace(item) == id {
				return true
			}
		}
	case []any:
		for _, item := range items {
			if text, ok := item.(string); ok && strings.TrimSpace(text) == id {
				return true
			}
		}
	}
	return false
}

func (s *Server) testWebSearchService(writer http.ResponseWriter, request *http.Request) {
	manager, err := s.requireWebSearchManager()
	if err != nil {
		writeError(writer, err)
		return
	}
	settings := configsvc.SystemSettings{}
	if s.config != nil {
		settings, err = s.config.GetSystemSettings(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
	}
	result, err := manager.SearchWithBudget(request.Context(), []string{request.PathValue("id")}, "", "", websearch.SearchRequest{
		Query: "Abot web search connectivity test", MaxResults: 1,
	}, searchBudget(settings))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (s *Server) getWebSearchUsage(writer http.ResponseWriter, request *http.Request) {
	manager, err := s.requireWebSearchManager()
	if err != nil {
		writeError(writer, err)
		return
	}
	settings := configsvc.SystemSettings{}
	if s.config != nil {
		settings, err = s.config.GetSystemSettings(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
	}
	summary, err := manager.UsageSummary(request.Context(), searchBudget(settings))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, summary)
}

func searchBudget(settings configsvc.SystemSettings) websearch.Budget {
	if settings.WebSearchDailyCallLimit == 0 {
		settings.WebSearchDailyCallLimit = 100
	}
	if settings.WebSearchMaxCallsPerInvocation == 0 {
		settings.WebSearchMaxCallsPerInvocation = 8
	}
	if settings.WebSearchAlertPercent == 0 {
		settings.WebSearchAlertPercent = 80
	}
	return websearch.Budget{
		DailyCallLimit:        int64(settings.WebSearchDailyCallLimit),
		MaxCallsPerInvocation: settings.WebSearchMaxCallsPerInvocation,
		AlertPercent:          settings.WebSearchAlertPercent,
	}
}

func (s *Server) requireWebSearchManager() (*websearch.Manager, error) {
	if s == nil || s.webSearch == nil {
		return nil, errors.New("网页搜索服务未装配")
	}
	return s.webSearch, nil
}
