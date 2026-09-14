package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"Abot/internal/conversation"
	"Abot/internal/workspace"
)

type conversationPayload struct {
	ID          string `json:"id"`
	UserID      string `json:"user_id"`
	WorkspaceID string `json:"workspace_id"`
	Title       string `json:"title"`
}

func (s *Server) requireConversations() (*conversation.Service, error) {
	if s.conversations == nil {
		return nil, errors.New("对话服务尚未装配")
	}
	return s.conversations, nil
}

func conversationUserID(request *http.Request) string {
	return strings.TrimSpace(request.URL.Query().Get("user_id"))
}

func (s *Server) listConversations(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConversations()
	if err != nil {
		writeError(writer, err)
		return
	}
	userID := conversationUserID(request)
	includeArchived := true
	if value := strings.TrimSpace(request.URL.Query().Get("include_archived")); value != "" {
		includeArchived, err = strconv.ParseBool(value)
		if err != nil {
			writeError(writer, fmt.Errorf("include_archived 参数无效"))
			return
		}
	}
	workspaceID := strings.TrimSpace(request.URL.Query().Get("workspace_id"))
	if workspaceID == "" {
		// 通用列表默认返回用户的普通和工作区对话；传入具体 ID 时才过滤工作区。
		workspaceID = "*"
	}
	items, err := service.List(request.Context(), userID, workspaceID, includeArchived)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"conversations": items})
}

func (s *Server) listWorkspaceConversations(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConversations()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := service.List(request.Context(), conversationUserID(request), request.PathValue("id"), true)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"conversations": items})
}

func (s *Server) createConversation(writer http.ResponseWriter, request *http.Request) {
	s.createConversationInWorkspace(writer, request, "")
}

func (s *Server) createWorkspaceConversation(writer http.ResponseWriter, request *http.Request) {
	s.createConversationInWorkspace(writer, request, strings.TrimSpace(request.PathValue("id")))
}

func (s *Server) createConversationInWorkspace(writer http.ResponseWriter, request *http.Request, pathWorkspaceID string) {
	service, err := s.requireConversations()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload conversationPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	workspaceID := strings.TrimSpace(payload.WorkspaceID)
	if pathWorkspaceID != "" {
		if workspaceID != "" && workspaceID != pathWorkspaceID {
			writeError(writer, errors.New("路径中的工作区 ID 与请求体不一致"))
			return
		}
		workspaceID = pathWorkspaceID
	}
	if workspaceID != "" {
		if s.workspaces == nil {
			writeError(writer, errors.New("工作区服务尚未装配"))
			return
		}
		item, getErr := s.workspaces.Get(request.Context(), workspaceID)
		if getErr != nil {
			writeError(writer, getErr)
			return
		}
		if !item.Enabled {
			writeError(writer, fmt.Errorf("%w: 工作区已禁用，不能创建新对话", workspace.ErrDisabled))
			return
		}
	}
	item, err := service.Create(request.Context(), conversation.CreateRequest{
		ID: payload.ID, UserID: payload.UserID, WorkspaceID: workspaceID, Title: payload.Title,
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, item)
}

func (s *Server) getConversation(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConversations()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := service.Get(request.Context(), conversationUserID(request), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) listConversationMessages(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConversations()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := service.Messages(request.Context(), conversationUserID(request), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"messages": items})
}

func (s *Server) getConversationContext(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConversations()
	if err != nil {
		writeError(writer, err)
		return
	}
	status, err := service.ContextStatus(request.Context(), conversationUserID(request), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, status)
}

func (s *Server) archiveConversation(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConversations()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := service.Archive(request.Context(), conversationUserID(request), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) unarchiveConversation(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConversations()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := service.Unarchive(request.Context(), conversationUserID(request), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) deleteConversation(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConversations()
	if err != nil {
		writeError(writer, err)
		return
	}
	if err := service.Delete(request.Context(), conversationUserID(request), request.PathValue("id")); err != nil {
		writeError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}
