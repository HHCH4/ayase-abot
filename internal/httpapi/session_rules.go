package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"Abot/internal/sessionrule"
)

// sessionRulePayload 使用指针字段支持编辑页只提交发生变化的开关。
type sessionRulePayload struct {
	Source          string    `json:"source"`
	ProcessEnabled  *bool     `json:"process_enabled"`
	LLMEnabled      *bool     `json:"llm_enabled"`
	TTSEnabled      *bool     `json:"tts_enabled"`
	Note            *string   `json:"note"`
	ChatModel       *string   `json:"chat_model"`
	STTModel        *string   `json:"stt_model"`
	TTSModel        *string   `json:"tts_model"`
	FollowProfile   *bool     `json:"follow_profile"`
	ProfileID       *string   `json:"profile_id"`
	PersonaID       *string   `json:"persona_id"`
	DisabledPlugins *[]string `json:"disabled_plugins"`
	KnowledgeBases  *[]string `json:"knowledge_bases"`
	KnowledgeTopK   *int      `json:"knowledge_top_k"`
	KnowledgeRerank *bool     `json:"knowledge_rerank"`
}

func (s *Server) requireSessionRules() (*sessionrule.Service, error) {
	if s.sessionRules == nil {
		return nil, errors.New("会话规则服务尚未装配")
	}
	return s.sessionRules, nil
}

func (s *Server) listSessionRules(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireSessionRules()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := service.List(request.Context(), request.URL.Query().Get("q"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"rules": items, "count": len(items)})
}

func (s *Server) createSessionRule(writer http.ResponseWriter, request *http.Request) {
	s.saveSessionRule(writer, request, "")
}

func (s *Server) updateSessionRule(writer http.ResponseWriter, request *http.Request) {
	s.saveSessionRule(writer, request, request.PathValue("source"))
}

func (s *Server) saveSessionRule(writer http.ResponseWriter, request *http.Request, pathSource string) {
	service, err := s.requireSessionRules()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload sessionRulePayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	source := strings.TrimSpace(payload.Source)
	if pathSource != "" {
		if source != "" && source != pathSource {
			writeError(writer, fmt.Errorf("%w: 路径中的会话来源与请求体不一致", sessionrule.ErrInvalidRequest))
			return
		}
		source = pathSource
	}
	// 新建规则显式使用空覆盖掩码；这样后续可以按单个规则项清除，而不是
	// 把所有默认值误当成用户覆盖。
	item := sessionrule.Rule{Source: source, ProcessEnabled: true, LLMEnabled: true, TTSEnabled: false, FollowProfile: true, KnowledgeTopK: 5, ConfiguredFields: []string{}}
	if pathSource != "" {
		item, err = service.Get(request.Context(), pathSource)
		if err != nil {
			writeError(writer, err)
			return
		}
	}
	mergeSessionRulePayload(&item, payload)
	saved, err := service.Save(request.Context(), item)
	if err != nil {
		writeError(writer, err)
		return
	}
	status := http.StatusOK
	if pathSource == "" {
		status = http.StatusCreated
	}
	writeJSON(writer, status, saved)
}

func (s *Server) getSessionRule(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireSessionRules()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := service.Get(request.Context(), request.PathValue("source"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) deleteSessionRule(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireSessionRules()
	if err != nil {
		writeError(writer, err)
		return
	}
	if err := service.Delete(request.Context(), request.PathValue("source")); err != nil {
		writeError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// resetSessionRuleField 清除指定来源的单个覆盖项；它不会删除消息来源目录，
// 只会在最后一个覆盖项也被清除时删除规则行。
func (s *Server) resetSessionRuleField(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireSessionRules()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if err := service.ResetField(request.Context(), request.PathValue("source"), payload.Key); err != nil {
		writeError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) batchSessionRules(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireSessionRules()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload sessionrule.BatchUpdate
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	items, err := service.ApplyBatch(request.Context(), payload)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"rules": items, "updated": len(items)})
}

type sessionRuleGroupPayload struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Members     []string `json:"members"`
}

func (s *Server) listSessionRuleGroups(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireSessionRules()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := service.ListGroups(request.Context())
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"groups": items})
}

func (s *Server) createSessionRuleGroup(writer http.ResponseWriter, request *http.Request) {
	s.saveSessionRuleGroup(writer, request, "")
}

func (s *Server) updateSessionRuleGroup(writer http.ResponseWriter, request *http.Request) {
	s.saveSessionRuleGroup(writer, request, request.PathValue("id"))
}

func (s *Server) saveSessionRuleGroup(writer http.ResponseWriter, request *http.Request, pathID string) {
	service, err := s.requireSessionRules()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload sessionRuleGroupPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if pathID != "" {
		payload.ID = pathID
	}
	item, err := service.SaveGroup(request.Context(), sessionrule.Group{ID: payload.ID, Name: payload.Name, Description: payload.Description, Members: payload.Members})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) deleteSessionRuleGroup(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireSessionRules()
	if err != nil {
		writeError(writer, err)
		return
	}
	if err := service.DeleteGroup(request.Context(), request.PathValue("id")); err != nil {
		writeError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func mergeSessionRulePayload(item *sessionrule.Rule, payload sessionRulePayload) {
	if payload.Source != "" {
		item.Source = strings.TrimSpace(payload.Source)
	}
	if payload.ProcessEnabled != nil {
		item.ProcessEnabled = *payload.ProcessEnabled
		item.MarkOverride("process_enabled")
	}
	if payload.LLMEnabled != nil {
		item.LLMEnabled = *payload.LLMEnabled
		item.MarkOverride("llm_enabled")
	}
	if payload.TTSEnabled != nil {
		item.TTSEnabled = *payload.TTSEnabled
		item.MarkOverride("tts_enabled")
	}
	if payload.Note != nil {
		item.Note = *payload.Note
		item.MarkOverride("note")
	}
	if payload.ChatModel != nil {
		item.ChatModel = *payload.ChatModel
		item.MarkOverride("chat_model")
	}
	if payload.STTModel != nil {
		item.STTModel = *payload.STTModel
		item.MarkOverride("stt_model")
	}
	if payload.TTSModel != nil {
		item.TTSModel = *payload.TTSModel
		item.MarkOverride("tts_model")
	}
	if payload.FollowProfile != nil {
		item.FollowProfile = *payload.FollowProfile
		item.MarkOverride("follow_profile")
	}
	if payload.ProfileID != nil {
		item.ProfileID = *payload.ProfileID
		item.MarkOverride("profile_id")
	}
	if payload.PersonaID != nil {
		item.PersonaID = *payload.PersonaID
		item.MarkOverride("persona_id")
	}
	if payload.DisabledPlugins != nil {
		item.DisabledPlugins = *payload.DisabledPlugins
		item.MarkOverride("disabled_plugins")
	}
	if payload.KnowledgeBases != nil {
		item.KnowledgeBases = *payload.KnowledgeBases
		item.MarkOverride("knowledge_bases")
	}
	if payload.KnowledgeTopK != nil {
		item.KnowledgeTopK = *payload.KnowledgeTopK
		item.MarkOverride("knowledge_top_k")
	}
	if payload.KnowledgeRerank != nil {
		item.KnowledgeRerank = *payload.KnowledgeRerank
		item.MarkOverride("knowledge_rerank")
	}
}
