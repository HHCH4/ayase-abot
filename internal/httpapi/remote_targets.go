package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"Abot/internal/workspace"
)

// remoteTargetPayload 只在写入接口接收密码；读取接口不会把密码返回给浏览器。
type remoteTargetPayload struct {
	ID                 string                    `json:"id"`
	Name               string                    `json:"name"`
	Transport          workspace.RemoteTransport `json:"transport"`
	Host               string                    `json:"host"`
	Port               int                       `json:"port"`
	User               string                    `json:"user"`
	AuthType           workspace.AuthType        `json:"auth_type"`
	KeyPath            string                    `json:"key_path"`
	Password           *string                   `json:"password"`
	HostKeyFingerprint string                    `json:"host_key_fingerprint"`
	Enabled            *bool                     `json:"enabled"`
}

// remoteTargetView 是远程主机的公开表示，私钥内容和密码永远不会出现在响应中。
type remoteTargetView struct {
	ID                 string                       `json:"id"`
	Name               string                       `json:"name"`
	Transport          workspace.RemoteTransport    `json:"transport"`
	Host               string                       `json:"host"`
	Port               int                          `json:"port"`
	User               string                       `json:"user"`
	AuthType           workspace.AuthType           `json:"auth_type"`
	KeyPath            string                       `json:"key_path,omitempty"`
	PasswordConfigured bool                         `json:"password_configured"`
	HostKeyFingerprint string                       `json:"host_key_fingerprint,omitempty"`
	Enabled            bool                         `json:"enabled"`
	Status             workspace.RemoteTargetStatus `json:"status"`
	StatusMessage      string                       `json:"status_message,omitempty"`
	LastCheckedAt      any                          `json:"last_checked_at,omitempty"`
	CreatedAt          any                          `json:"created_at,omitempty"`
	UpdatedAt          any                          `json:"updated_at,omitempty"`
}

func (s *Server) requireRemoteTargets() (*workspace.RemoteTargetService, error) {
	if s.remoteTargets == nil {
		return nil, errors.New("远程主机服务尚未装配")
	}
	return s.remoteTargets, nil
}

func (s *Server) listRemoteTargets(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireRemoteTargets()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := service.List(request.Context())
	if err != nil {
		writeError(writer, err)
		return
	}
	views := make([]remoteTargetView, 0, len(items))
	for _, item := range items {
		views = append(views, publicRemoteTarget(item))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"remote_targets": views})
}

func (s *Server) getRemoteTarget(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireRemoteTargets()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := service.Get(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, publicRemoteTarget(item))
}

func (s *Server) createRemoteTarget(writer http.ResponseWriter, request *http.Request) {
	s.saveRemoteTarget(writer, request, "")
}

func (s *Server) updateRemoteTarget(writer http.ResponseWriter, request *http.Request) {
	s.saveRemoteTarget(writer, request, request.PathValue("id"))
}

func (s *Server) saveRemoteTarget(writer http.ResponseWriter, request *http.Request, pathID string) {
	service, err := s.requireRemoteTargets()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload remoteTargetPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if pathID != "" {
		if payload.ID != "" && payload.ID != pathID {
			writeError(writer, errors.New("路径中的远程主机 ID 与请求体不一致"))
			return
		}
		payload.ID = pathID
	}
	enabled := true
	if payload.ID != "" {
		if old, getErr := service.Get(request.Context(), payload.ID); getErr == nil {
			enabled = old.Enabled
		}
	}
	if payload.Enabled != nil {
		enabled = *payload.Enabled
	}
	item, err := service.Save(request.Context(), workspace.RemoteTargetSaveRequest{
		Target: workspace.RemoteTarget{
			ID: payload.ID, Name: payload.Name, Transport: payload.Transport, Host: payload.Host, Port: payload.Port,
			User: payload.User, AuthType: payload.AuthType, KeyPath: payload.KeyPath,
			HostKeyFingerprint: strings.TrimSpace(payload.HostKeyFingerprint), Enabled: enabled,
		},
		Password: payload.Password,
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, publicRemoteTarget(item))
}

func (s *Server) deleteRemoteTarget(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireRemoteTargets()
	if err != nil {
		writeError(writer, err)
		return
	}
	if err := service.Delete(request.Context(), request.PathValue("id")); err != nil {
		writeError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) previewTestRemoteTarget(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireRemoteTargets()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload remoteTargetPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	result, err := service.TestPreview(request.Context(), remoteTargetSaveRequest(payload))
	writeRemoteTestResult(writer, result, err)
}

func (s *Server) testRemoteTarget(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireRemoteTargets()
	if err != nil {
		writeError(writer, err)
		return
	}
	result, err := service.Test(request.Context(), request.PathValue("id"))
	writeRemoteTestResult(writer, result, err)
}

func (s *Server) listRemoteDirectories(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireRemoteTargets()
	if err != nil {
		writeError(writer, err)
		return
	}
	listing, err := service.ListDirectories(request.Context(), request.PathValue("id"), request.URL.Query().Get("path"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, listing)
}

func remoteTargetSaveRequest(payload remoteTargetPayload) workspace.RemoteTargetSaveRequest {
	return workspace.RemoteTargetSaveRequest{
		Target: workspace.RemoteTarget{
			ID: payload.ID, Name: payload.Name, Transport: payload.Transport, Host: payload.Host, Port: payload.Port,
			User: payload.User, AuthType: payload.AuthType, KeyPath: payload.KeyPath,
			HostKeyFingerprint: strings.TrimSpace(payload.HostKeyFingerprint),
		},
		Password: payload.Password,
	}
}

func writeRemoteTestResult(writer http.ResponseWriter, result workspace.TestResult, err error) {
	if err != nil && !errors.Is(err, workspace.ErrRemoteTargetUnconfirmed) {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func publicRemoteTarget(item workspace.RemoteTarget) remoteTargetView {
	return remoteTargetView{
		ID: item.ID, Name: item.Name, Transport: item.Transport, Host: item.Host, Port: item.Port, User: item.User,
		AuthType: item.AuthType, KeyPath: item.KeyPath, PasswordConfigured: item.PasswordConfigured,
		HostKeyFingerprint: item.HostKeyFingerprint, Enabled: item.Enabled, Status: item.Status,
		StatusMessage: item.StatusMessage, LastCheckedAt: item.LastCheckedAt, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
}
