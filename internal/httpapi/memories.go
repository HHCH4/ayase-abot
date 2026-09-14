package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	memorysvc "Abot/internal/memory"
)

type memoryPayload struct {
	UserID         string `json:"user_id"`
	ConversationID string `json:"conversation_id"`
	Content        string `json:"content"`
	Tags           string `json:"tags"`
}

type memoryUpdatePayload struct {
	Content string `json:"content"`
	Tags    string `json:"tags"`
}

func (s *Server) requireMemories() (*memorysvc.Service, error) {
	if s.memories == nil {
		return nil, errors.New("长期记忆服务尚未装配")
	}
	return s.memories, nil
}

func memoryUserID(request *http.Request) string {
	return strings.TrimSpace(request.URL.Query().Get("user_id"))
}

func (s *Server) listMemories(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireMemories()
	if err != nil {
		writeError(writer, err)
		return
	}
	limit := 0
	if value := strings.TrimSpace(request.URL.Query().Get("limit")); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 {
			writeError(writer, fmt.Errorf("%w: limit 必须是正整数", memorysvc.ErrInvalidRequest))
			return
		}
	}
	items, err := service.List(request.Context(), memoryUserID(request), request.URL.Query().Get("q"), limit)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"memories": items})
}

func (s *Server) getMemory(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireMemories()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := service.Get(request.Context(), memoryUserID(request), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) createMemory(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireMemories()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload memoryPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if conversationID := strings.TrimSpace(payload.ConversationID); conversationID != "" && s.conversations != nil {
		if _, err := s.conversations.Get(request.Context(), payload.UserID, conversationID); err != nil {
			writeError(writer, fmt.Errorf("记忆关联的对话无效: %w", err))
			return
		}
	}
	item, err := service.Save(request.Context(), memorysvc.Item{
		UserID: payload.UserID, ConversationID: payload.ConversationID, Content: payload.Content,
		Tags: payload.Tags, Author: "user", Source: "manual",
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, item)
}

func (s *Server) updateMemory(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireMemories()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload memoryUpdatePayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	item, err := service.Update(request.Context(), memoryUserID(request), request.PathValue("id"), payload.Content, payload.Tags)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) deleteMemory(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireMemories()
	if err != nil {
		writeError(writer, err)
		return
	}
	if err := service.Delete(request.Context(), memoryUserID(request), request.PathValue("id")); err != nil {
		writeError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) clearMemories(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireMemories()
	if err != nil {
		writeError(writer, err)
		return
	}
	if err := service.Clear(request.Context(), memoryUserID(request)); err != nil {
		writeError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}
