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
	item := sessionrule.Rule{Source: source, ProcessEnabled: true, LLMEnabled: true, TTSEnabled: false, FollowProfile: true, KnowledgeTopK: 5}
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
	}
	if payload.LLMEnabled != nil {
		item.LLMEnabled = *payload.LLMEnabled
	}
	if payload.TTSEnabled != nil {
		item.TTSEnabled = *payload.TTSEnabled
	}
	if payload.Note != nil {
		item.Note = *payload.Note
	}
	if payload.ChatModel != nil {
		item.ChatModel = *payload.ChatModel
	}
	if payload.STTModel != nil {
		item.STTModel = *payload.STTModel
	}
	if payload.TTSModel != nil {
		item.TTSModel = *payload.TTSModel
	}
	if payload.FollowProfile != nil {
		item.FollowProfile = *payload.FollowProfile
	}
	if payload.ProfileID != nil {
		item.ProfileID = *payload.ProfileID
	}
	if payload.PersonaID != nil {
		item.PersonaID = *payload.PersonaID
	}
	if payload.DisabledPlugins != nil {
		item.DisabledPlugins = *payload.DisabledPlugins
	}
	if payload.KnowledgeBases != nil {
		item.KnowledgeBases = *payload.KnowledgeBases
	}
	if payload.KnowledgeTopK != nil {
		item.KnowledgeTopK = *payload.KnowledgeTopK
	}
	if payload.KnowledgeRerank != nil {
		item.KnowledgeRerank = *payload.KnowledgeRerank
	}
}
