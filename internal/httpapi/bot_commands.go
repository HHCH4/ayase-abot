package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"Abot/internal/bot"
)

// This file exposes the chat command catalog, its per-bot policy overrides, the
// delegated group administrators and the audit trail. The console reads the
// same descriptors the chat dispatcher uses.

type commandPolicyPayload struct {
	Permission string `json:"permission"`
	Enabled    *bool  `json:"enabled"`
}

type groupAdminPayload struct {
	ChatID string `json:"chat_id"`
	UserID string `json:"user_id"`
}

func (s *Server) listBotCommands(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	service, err := s.requireBots()
	if err != nil {
		writeError(writer, err)
		return
	}
	commands, err := service.ListCommands(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"commands": commands})
}

func (s *Server) updateBotCommandPolicy(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	service, err := s.requireBots()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload commandPolicyPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	permission := bot.Permission(strings.TrimSpace(payload.Permission))
	if permission == "" {
		writeError(writer, fmt.Errorf("%w: 权限级别不能为空", bot.ErrInvalid))
		return
	}
	// Enabled is a pointer so an omitted field cannot silently disable a command.
	enabled := true
	if payload.Enabled != nil {
		enabled = *payload.Enabled
	}
	// Multi-word commands arrive as one path segment ("admin add"); ServeMux
	// has already decoded the separator.
	commandID := strings.TrimSpace(request.PathValue("commandID"))
	if err := service.UpdateCommandPolicy(request.Context(), request.PathValue("id"), bot.CommandPolicy{
		CommandID: commandID, Permission: permission, Enabled: enabled,
	}); err != nil {
		writeError(writer, err)
		return
	}
	commands, err := service.ListCommands(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"commands": commands})
}

func (s *Server) listBotCommandAudits(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireBots()
	if err != nil {
		writeError(writer, err)
		return
	}
	limit := 100
	if raw := strings.TrimSpace(request.URL.Query().Get("limit")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 1 || parsed > 500 {
			writeError(writer, fmt.Errorf("%w: limit 必须在 1-500 之间", bot.ErrInvalid))
			return
		}
		limit = parsed
	}
	entries, err := service.ListCommandAudits(request.Context(), request.PathValue("id"), limit)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"audits": entries})
}

func (s *Server) listBotGroupAdmins(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireBots()
	if err != nil {
		writeError(writer, err)
		return
	}
	chatID := strings.TrimSpace(request.URL.Query().Get("chat_id"))
	if chatID == "" {
		writeError(writer, fmt.Errorf("%w: chat_id 不能为空", bot.ErrInvalid))
		return
	}
	admins, err := service.ListGroupAdmins(request.Context(), request.PathValue("id"), chatID)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"admins": admins})
}

func (s *Server) addBotGroupAdmin(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireBots()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload groupAdminPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if err := service.AddGroupAdmin(request.Context(), bot.GroupAdmin{
		AdapterID: request.PathValue("id"), ChatID: payload.ChatID, UserID: payload.UserID, AddedBy: "webui",
	}); err != nil {
		writeError(writer, err)
		return
	}
	admins, err := service.ListGroupAdmins(request.Context(), request.PathValue("id"), payload.ChatID)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"admins": admins})
}

func (s *Server) removeBotGroupAdmin(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireBots()
	if err != nil {
		writeError(writer, err)
		return
	}
	chatID := strings.TrimSpace(request.URL.Query().Get("chat_id"))
	if chatID == "" {
		writeError(writer, fmt.Errorf("%w: chat_id 不能为空", bot.ErrInvalid))
		return
	}
	if err := service.RemoveGroupAdmin(request.Context(), request.PathValue("id"), chatID, request.PathValue("userID")); err != nil {
		writeError(writer, err)
		return
	}
	admins, err := service.ListGroupAdmins(request.Context(), request.PathValue("id"), chatID)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"admins": admins})
}
