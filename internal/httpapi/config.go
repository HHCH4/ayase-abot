package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	configsvc "Abot/internal/config"
)

type configProfilePayload struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	Values configsvc.Values `json:"values"`
}

type configImportPayload struct {
	Version   int              `json:"version"`
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Revision  int              `json:"revision"`
	Values    configsvc.Values `json:"values"`
	Overwrite bool             `json:"overwrite"`
}

type configBindingPayload struct {
	ProfileID string `json:"profile_id"`
}

func (s *Server) requireConfig() (*configsvc.Service, error) {
	if s.config == nil {
		return nil, errors.New("配置中心服务尚未装配")
	}
	return s.config, nil
}

func (s *Server) configSchema(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConfig()
	if err != nil {
		writeError(writer, err)
		return
	}
	schema := service.Schema()
	// 动态选项来自当前供应商目录，Schema 本身仍然由后端统一提供。
	for index := range schema.Fields {
		switch schema.Fields[index].OptionSource {
		case "providers":
			if s.providers == nil {
				continue
			}
			options := make([]configsvc.SchemaOption, 0)
			for _, item := range s.providers.List() {
				options = append(options, configsvc.SchemaOption{Value: item.ID, Label: item.Name})
			}
			schema.Fields[index].Options = options
		case "personas":
			if s.personas == nil {
				continue
			}
			items, listErr := s.personas.ListPersonas(request.Context())
			if listErr != nil {
				writeError(writer, listErr)
				return
			}
			options := make([]configsvc.SchemaOption, 0, len(items)+1)
			options = append(options, configsvc.SchemaOption{Value: "", Label: "使用配置中的系统提示词"})
			for _, item := range items {
				if item.Enabled {
					options = append(options, configsvc.SchemaOption{Value: item.ID, Label: item.Name})
				}
			}
			schema.Fields[index].Options = options
		}
	}
	writeJSON(writer, http.StatusOK, schema)
}

func (s *Server) listConfigProfiles(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConfig()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := service.ListProfiles(request.Context())
	if err != nil {
		writeError(writer, err)
		return
	}
	defaultID, err := service.DefaultProfileID(request.Context())
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"profiles": items, "default_profile_id": defaultID})
}

func (s *Server) createConfigProfile(writer http.ResponseWriter, request *http.Request) {
	s.saveConfigProfile(writer, request, "")
}

func (s *Server) updateConfigProfile(writer http.ResponseWriter, request *http.Request) {
	s.saveConfigProfile(writer, request, request.PathValue("id"))
}

func (s *Server) saveConfigProfile(writer http.ResponseWriter, request *http.Request, pathID string) {
	service, err := s.requireConfig()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload configProfilePayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if pathID != "" {
		if payload.ID != "" && payload.ID != pathID {
			writeError(writer, errors.New("路径中的配置文件 ID 与请求体不一致"))
			return
		}
		payload.ID = pathID
	}
	if payload.Values == nil && payload.ID != "" {
		current, getErr := service.GetProfile(request.Context(), payload.ID)
		if getErr != nil {
			writeError(writer, getErr)
			return
		}
		payload.Values = current.Values
	}
	item, err := service.SaveProfile(request.Context(), payload.ID, payload.Name, payload.Values)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) getConfigProfile(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConfig()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := service.GetProfile(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) listConfigRevisions(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConfig()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := service.ListRevisions(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"revisions": items})
}

func (s *Server) deleteConfigProfile(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConfig()
	if err != nil {
		writeError(writer, err)
		return
	}
	if err := service.DeleteProfile(request.Context(), request.PathValue("id")); err != nil {
		writeError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) validateConfigProfile(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConfig()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload configProfilePayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if strings.TrimSpace(payload.Name) == "" && payload.ID != "" {
		if current, getErr := service.GetProfile(request.Context(), payload.ID); getErr == nil {
			payload.Name = current.Name
		}
	}
	if err := service.ValidateDraft(request.Context(), payload.ID, payload.Name, payload.Values); err != nil {
		writeJSON(writer, http.StatusOK, map[string]any{"valid": false, "error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"valid": true})
}

func (s *Server) exportConfigProfile(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConfig()
	if err != nil {
		writeError(writer, err)
		return
	}
	value, err := service.Export(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writer.Header().Set("Content-Disposition", `attachment; filename="abot-config-profile.json"`)
	writeJSON(writer, http.StatusOK, value)
}

func (s *Server) importConfigProfile(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConfig()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload configImportPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if payload.Version != 0 && payload.Version != configsvc.SchemaVersion {
		writeError(writer, fmt.Errorf("%w: 配置文件版本 %d 不受支持", configsvc.ErrInvalidRequest, payload.Version))
		return
	}
	item, err := service.Import(request.Context(), payload.ID, payload.Name, payload.Values, payload.Overwrite)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) copyConfigProfile(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConfig()
	if err != nil {
		writeError(writer, err)
		return
	}
	current, err := service.GetProfile(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload struct {
		Name string `json:"name"`
	}
	if request.ContentLength != 0 {
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, fmt.Errorf("请求体无效: %w", err))
			return
		}
	}
	name := strings.TrimSpace(payload.Name)
	if name == "" {
		name = current.Name + " 副本"
	}
	item, err := service.SaveProfile(request.Context(), "", name, current.Values)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, item)
}

func (s *Server) setDefaultConfigProfile(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConfig()
	if err != nil {
		writeError(writer, err)
		return
	}
	if err := service.SetDefaultProfile(request.Context(), request.PathValue("id")); err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"default_profile_id": request.PathValue("id")})
}

func (s *Server) getSystemSettings(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConfig()
	if err != nil {
		writeError(writer, err)
		return
	}
	settings, err := service.GetSystemSettings(request.Context())
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"log_level": settings.LogLevel, "request_timeout_seconds": settings.RequestTimeoutSeconds,
		"schema": configsvc.SystemSchema().Fields,
	})
}

func (s *Server) setSystemSettings(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConfig()
	if err != nil {
		writeError(writer, err)
		return
	}
	var settings configsvc.SystemSettings
	if err := decodeJSON(writer, request, &settings); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	result, err := service.SaveSystemSettings(request.Context(), settings)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (s *Server) bindBotConfigProfile(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConfig()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload configBindingPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if err := service.Bind(request.Context(), configsvc.BindingBot, request.PathValue("id"), payload.ProfileID); err != nil {
		writeError(writer, err)
		return
	}
	profileID, err := service.Binding(request.Context(), configsvc.BindingBot, request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"bot_id": request.PathValue("id"), "profile_id": profileID})
}

func (s *Server) getBotConfigProfile(writer http.ResponseWriter, request *http.Request) {
	s.getConfigBinding(writer, request, configsvc.BindingBot, "bot_id")
}

func (s *Server) bindConversationConfigProfile(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireConfig()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload configBindingPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if err := service.Bind(request.Context(), configsvc.BindingConversation, request.PathValue("id"), payload.ProfileID); err != nil {
		writeError(writer, err)
		return
	}
	profileID, err := service.Binding(request.Context(), configsvc.BindingConversation, request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"conversation_id": request.PathValue("id"), "profile_id": profileID})
}

func (s *Server) getConversationConfigProfile(writer http.ResponseWriter, request *http.Request) {
	s.getConfigBinding(writer, request, configsvc.BindingConversation, "conversation_id")
}

func (s *Server) getConfigBinding(writer http.ResponseWriter, request *http.Request, scope configsvc.BindingScope, idKey string) {
	service, err := s.requireConfig()
	if err != nil {
		writeError(writer, err)
		return
	}
	profileID, err := service.Binding(request.Context(), scope, request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{idKey: request.PathValue("id"), "profile_id": profileID})
}
