package httpapi

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"Abot/internal/conversation"
)

// sessionSourceView 是规则编辑器使用的来源目录项；来源本身保持 UMO 原值不变。
type sessionSourceView struct {
	Source      string              `json:"source"`
	SourceName  string              `json:"source_name,omitempty"`
	UserID      string              `json:"user_id"`
	Platform    string              `json:"platform,omitempty"`
	MessageType string              `json:"message_type,omitempty"`
	SessionID   string              `json:"session_id,omitempty"`
	Status      conversation.Status `json:"status"`
	UpdatedAt   time.Time           `json:"updated_at"`
}

// listSessionSources 从已经产生过消息的会话中提取去重后的 UMO，供规则页面选择。
func (s *Server) listSessionSources(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConversations()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := service.ListAll(request.Context(), "*", true)
	if err != nil {
		writeError(writer, err)
		return
	}
	query := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("q")))
	bySource := make(map[string]sessionSourceView)
	for _, item := range items {
		source := strings.TrimSpace(item.Source)
		if source == "" {
			// WebUI 普通对话没有 UMO，不应出现在平台会话规则的来源目录里。
			continue
		}
		parts := strings.SplitN(source, ":", 3)
		view := sessionSourceView{
			Source: source, SourceName: item.SourceName, UserID: item.UserID,
			Status: item.Status, UpdatedAt: item.UpdatedAt,
		}
		if len(parts) > 0 {
			view.Platform = parts[0]
		}
		if len(parts) > 1 {
			view.MessageType = parts[1]
		}
		if len(parts) > 2 {
			view.SessionID = parts[2]
		}
		if query != "" {
			searchable := strings.ToLower(strings.Join([]string{view.Source, view.SourceName, view.Platform, view.MessageType, view.SessionID}, " "))
			if !strings.Contains(searchable, query) {
				continue
			}
		}
		// 同一来源可能有多条内部对话，保留最近一次出现的那条元数据。
		if current, ok := bySource[source]; !ok || item.UpdatedAt.After(current.UpdatedAt) {
			bySource[source] = view
		} else if ok && current.SourceName == "" && view.SourceName != "" {
			bySource[source] = view
		}
	}
	result := make([]sessionSourceView, 0, len(bySource))
	for _, item := range bySource {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i].Source) < strings.ToLower(result[j].Source)
	})
	writeJSON(writer, http.StatusOK, map[string]any{"sources": result, "count": len(result)})
}
