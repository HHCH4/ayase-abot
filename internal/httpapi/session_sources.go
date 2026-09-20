package httpapi

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"Abot/internal/bot"
	"Abot/internal/conversation"
)

// sessionSourceView 是规则编辑器使用的来源目录项；来源本身保持 UMO 原值不变。
// 目录既包含消息入口登记的来源，也兼容升级前已经存在的历史对话来源。
type sessionSourceView struct {
	Source      string              `json:"source"`
	SourceName  string              `json:"source_name,omitempty"`
	AutoName    string              `json:"auto_name,omitempty"`
	UserID      string              `json:"user_id"`
	Platform    string              `json:"platform,omitempty"`
	MessageType string              `json:"message_type,omitempty"`
	SessionID   string              `json:"session_id,omitempty"`
	Status      conversation.Status `json:"status"`
	HasRule     bool                `json:"has_rule"`
	FirstSeenAt time.Time           `json:"first_seen_at,omitempty"`
	LastSeenAt  time.Time           `json:"last_seen_at,omitempty"`
	UpdatedAt   time.Time           `json:"updated_at"`
}

// listSessionSources 合并独立来源目录、历史对话和现有规则，保证来源下拉框
// 不再只依赖“已经进入 AI 的对话”，并为每个 UMO 标记是否已经配置规则。
func (s *Server) listSessionSources(writer http.ResponseWriter, request *http.Request) {
	query := strings.TrimSpace(request.URL.Query().Get("q"))
	bySource := make(map[string]sessionSourceView)
	var nameResolver bot.SourceNameRepository
	if s.sourceRegistry != nil {
		if resolver, ok := s.sourceRegistry.(bot.SourceNameRepository); ok {
			nameResolver = resolver
		}
		items, err := s.sourceRegistry.ListMessageSources(request.Context(), query)
		if err != nil {
			writeError(writer, err)
			return
		}
		for _, item := range items {
			view := sessionSourceView{
				Source: item.Source, AutoName: item.AutoName, UserID: item.UserID,
				Platform: item.Platform, MessageType: item.MessageType, SessionID: item.SessionID,
				Status: conversation.StatusActive, FirstSeenAt: item.FirstSeenAt,
				LastSeenAt: item.LastSeenAt, UpdatedAt: item.LastSeenAt,
			}
			if nameResolver != nil {
				name, nameErr := nameResolver.GetSourceName(request.Context(), item.Source)
				if nameErr != nil {
					writeError(writer, nameErr)
					return
				}
				view.SourceName = strings.TrimSpace(name)
			}
			mergeSessionSource(bySource, view)
		}
		if lister, ok := s.sourceRegistry.(bot.SourceNameLister); ok {
			aliases, listErr := lister.ListSourceNames(request.Context(), query)
			if listErr != nil {
				writeError(writer, listErr)
				return
			}
			for _, alias := range aliases {
				view := parseSessionSource(alias.Source)
				view.SourceName = strings.TrimSpace(alias.Name)
				view.Status = conversation.StatusActive
				view.UpdatedAt = alias.UpdatedAt
				view.LastSeenAt = alias.UpdatedAt
				mergeSessionSource(bySource, view)
			}
		}
	}

	// 历史版本没有独立来源表；合并 Conversation 使升级前收到过消息的
	// UMO 仍然可见，同时补上归档状态和最后活动时间。
	if s.conversations != nil {
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
		for _, item := range items {
			if strings.TrimSpace(item.Source) == "" {
				continue
			}
			view := parseSessionSource(item.Source)
			view.SourceName = strings.TrimSpace(item.SourceName)
			view.UserID = item.UserID
			view.Status = item.Status
			view.UpdatedAt = item.UpdatedAt
			view.LastSeenAt = item.UpdatedAt
			view.FirstSeenAt = item.CreatedAt
			mergeSessionSource(bySource, view)
		}
	}

	if s.sessionRules != nil {
		rules, err := s.sessionRules.List(request.Context(), "")
		if err != nil {
			writeError(writer, err)
			return
		}
		for _, item := range rules {
			if source := strings.TrimSpace(item.Source); source != "" {
				view, exists := bySource[source]
				if !exists {
					view = parseSessionSource(source)
				}
				view.Source = source
				view.HasRule = true
				bySource[source] = view
			}
		}
	}

	result := make([]sessionSourceView, 0, len(bySource))
	for _, item := range bySource {
		if !sessionSourceMatches(item, query) {
			continue
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i], result[j]
		if !left.UpdatedAt.Equal(right.UpdatedAt) {
			return left.UpdatedAt.After(right.UpdatedAt)
		}
		return strings.ToLower(left.Source) < strings.ToLower(right.Source)
	})
	writeJSON(writer, http.StatusOK, map[string]any{"sources": result, "count": len(result)})
}

// parseSessionSource 把 UMO 三段拆成平台、消息类型和 Session ID，不能改变
// 原始 source，否则规则主键会和消息入口计算结果不一致。
func parseSessionSource(source string) sessionSourceView {
	parts := strings.SplitN(strings.TrimSpace(source), ":", 3)
	view := sessionSourceView{Source: strings.TrimSpace(source)}
	if len(parts) > 0 {
		view.Platform = parts[0]
	}
	if len(parts) > 1 {
		view.MessageType = parts[1]
	}
	if len(parts) > 2 {
		view.SessionID = parts[2]
	}
	return view
}

// mergeSessionSource 按最后活动时间合并多个来源事实，并始终保留手工名称、
// 自动名称和首次观察时间，避免历史对话覆盖消息入口的新鲜状态。
func mergeSessionSource(target map[string]sessionSourceView, candidate sessionSourceView) {
	source := strings.TrimSpace(candidate.Source)
	if source == "" {
		return
	}
	candidate.Source = source
	current, exists := target[source]
	if !exists {
		target[source] = candidate
		return
	}
	if current.SourceName == "" {
		current.SourceName = candidate.SourceName
	}
	if current.AutoName == "" {
		current.AutoName = candidate.AutoName
	}
	if current.Platform == "" || current.MessageType == "" || current.SessionID == "" {
		parsed := parseSessionSource(source)
		if current.Platform == "" {
			current.Platform = parsed.Platform
		}
		if current.MessageType == "" {
			current.MessageType = parsed.MessageType
		}
		if current.SessionID == "" {
			current.SessionID = parsed.SessionID
		}
	}
	if current.UserID == "" {
		current.UserID = candidate.UserID
	}
	if current.FirstSeenAt.IsZero() || (!candidate.FirstSeenAt.IsZero() && candidate.FirstSeenAt.Before(current.FirstSeenAt)) {
		current.FirstSeenAt = candidate.FirstSeenAt
	}
	if candidate.LastSeenAt.After(current.LastSeenAt) {
		current.LastSeenAt = candidate.LastSeenAt
	}
	if candidate.UpdatedAt.After(current.UpdatedAt) {
		current.Status = candidate.Status
		current.UserID = candidate.UserID
		current.UpdatedAt = candidate.UpdatedAt
	}
	if current.Status == "" {
		current.Status = candidate.Status
	}
	target[source] = current
}

func sessionSourceMatches(item sessionSourceView, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}
	searchable := strings.ToLower(strings.Join([]string{
		item.Source, item.SourceName, item.AutoName, item.UserID, item.Platform,
		item.MessageType, item.SessionID,
	}, " "))
	return strings.Contains(searchable, query)
}
