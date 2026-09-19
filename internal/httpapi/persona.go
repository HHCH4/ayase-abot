package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	configsvc "Abot/internal/config"
)

type personaPayload struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
	Instruction string  `json:"instruction"`
	Enabled     *bool   `json:"enabled"`
}

type personaBindingPayload struct {
	PersonaID string `json:"persona_id"`
}

func (s *Server) requirePersonas() (*configsvc.PersonaService, error) {
	if s.personas == nil {
		return nil, errors.New("人格目录服务尚未装配")
	}
	return s.personas, nil
}

func (s *Server) listPersonas(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requirePersonas()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := service.ListPersonas(request.Context())
	if err != nil {
		writeError(writer, err)
		return
	}
	defaultID, err := service.DefaultPersonaID(request.Context())
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"personas": items, "default_persona_id": defaultID, "schema_version": configsvc.PersonaSchemaVersion})
}

func (s *Server) createPersona(writer http.ResponseWriter, request *http.Request) {
	s.savePersona(writer, request, "")
}

func (s *Server) updatePersona(writer http.ResponseWriter, request *http.Request) {
	s.savePersona(writer, request, request.PathValue("id"))
}

func (s *Server) savePersona(writer http.ResponseWriter, request *http.Request, pathID string) {
	service, err := s.requirePersonas()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload personaPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if pathID != "" {
		if payload.ID != "" && strings.TrimSpace(payload.ID) != pathID {
			writeError(writer, fmt.Errorf("%w: 路径中的人格 ID 与请求体不一致", configsvc.ErrInvalidRequest))
			return
		}
		payload.ID = pathID
		current, getErr := service.GetPersona(request.Context(), pathID)
		if getErr != nil {
			writeError(writer, getErr)
			return
		}
		if strings.TrimSpace(payload.Name) == "" {
			payload.Name = current.Name
		}
		description := ""
		if payload.Description == nil {
			description = current.Description
		} else {
			description = *payload.Description
		}
		if strings.TrimSpace(payload.Instruction) == "" {
			payload.Instruction = current.Instruction
		}
		item, saveErr := service.SavePersona(request.Context(), payload.ID, payload.Name, description, payload.Instruction, payload.Enabled)
		if saveErr != nil {
			writeError(writer, saveErr)
			return
		}
		writeJSON(writer, http.StatusOK, item)
		return
	}
	description := ""
	if payload.Description != nil {
		description = *payload.Description
	}
	item, err := service.SavePersona(request.Context(), payload.ID, payload.Name, description, payload.Instruction, payload.Enabled)
	if err != nil {
		writeError(writer, err)
		return
	}
	status := http.StatusOK
	if pathID == "" {
		status = http.StatusCreated
	}
	writeJSON(writer, status, item)
}

func (s *Server) getPersona(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requirePersonas()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := service.GetPersona(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) listPersonaRevisions(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requirePersonas()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := service.ListRevisions(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"revisions": items, "schema_version": configsvc.PersonaSchemaVersion})
}

// exportPersona 导出人格公开字段，不包含运行时密钥或会话内容。
func (s *Server) exportPersona(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requirePersonas()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := service.GetPersona(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writer.Header().Set("Content-Disposition", `attachment; filename="abot-persona.json"`)
	writeJSON(writer, http.StatusOK, map[string]any{"version": configsvc.PersonaSchemaVersion, "persona": item})
}

// importPersona 复用人格服务的校验和修订逻辑，避免导入文件绕过启停边界。
func (s *Server) importPersona(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requirePersonas()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload struct {
		Version int            `json:"version"`
		Persona personaPayload `json:"persona"`
	}
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if payload.Version != 0 && payload.Version != configsvc.PersonaSchemaVersion {
		writeError(writer, fmt.Errorf("%w: 人格文件版本不受支持", configsvc.ErrInvalidRequest))
		return
	}
	description := ""
	if payload.Persona.Description != nil {
		description = *payload.Persona.Description
	}
	item, err := service.SavePersona(request.Context(), payload.Persona.ID, payload.Persona.Name, description, payload.Persona.Instruction, payload.Persona.Enabled)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, item)
}

func (s *Server) deletePersona(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requirePersonas()
	if err != nil {
		writeError(writer, err)
		return
	}
	if err := service.DeletePersona(request.Context(), request.PathValue("id")); err != nil {
		writeError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) setDefaultPersona(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requirePersonas()
	if err != nil {
		writeError(writer, err)
		return
	}
	if err := service.SetDefaultPersona(request.Context(), request.PathValue("id")); err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"default_persona_id": request.PathValue("id")})
}

func (s *Server) bindBotPersona(writer http.ResponseWriter, request *http.Request) {
	s.bindPersona(writer, request, configsvc.BindingBot, "bot_id")
}

func (s *Server) getBotPersona(writer http.ResponseWriter, request *http.Request) {
	s.getPersonaBinding(writer, request, configsvc.BindingBot, "bot_id")
}

func (s *Server) bindConversationPersona(writer http.ResponseWriter, request *http.Request) {
	s.bindPersona(writer, request, configsvc.BindingConversation, "conversation_id")
}

func (s *Server) getConversationPersona(writer http.ResponseWriter, request *http.Request) {
	s.getPersonaBinding(writer, request, configsvc.BindingConversation, "conversation_id")
}

func (s *Server) bindPersona(writer http.ResponseWriter, request *http.Request, scope configsvc.BindingScope, idKey string) {
	service, err := s.requirePersonas()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload personaBindingPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	targetID := request.PathValue("id")
	if err := service.Bind(request.Context(), scope, targetID, payload.PersonaID); err != nil {
		writeError(writer, err)
		return
	}
	personaID, err := service.Binding(request.Context(), scope, targetID)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{idKey: targetID, "persona_id": personaID})
}

func (s *Server) getPersonaBinding(writer http.ResponseWriter, request *http.Request, scope configsvc.BindingScope, idKey string) {
	service, err := s.requirePersonas()
	if err != nil {
		writeError(writer, err)
		return
	}
	targetID := request.PathValue("id")
	personaID, err := service.Binding(request.Context(), scope, targetID)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{idKey: targetID, "persona_id": personaID})
}
