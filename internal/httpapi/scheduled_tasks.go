package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"Abot/internal/schedule"
)

func (s *Server) requireSchedules() (*schedule.Service, error) {
	if s.schedules == nil {
		return nil, errors.New("未来任务服务尚未装配")
	}
	return s.schedules, nil
}

func (s *Server) listScheduledTasks(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireSchedules()
	if err != nil {
		writeError(writer, err)
		return
	}
	status := schedule.Status(strings.TrimSpace(request.URL.Query().Get("status")))
	items, err := service.List(request.Context(), status)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"tasks": items})
}

func (s *Server) createScheduledTask(writer http.ResponseWriter, request *http.Request) {
	s.saveScheduledTask(writer, request, "")
}

func (s *Server) updateScheduledTask(writer http.ResponseWriter, request *http.Request) {
	s.saveScheduledTask(writer, request, request.PathValue("id"))
}

func (s *Server) saveScheduledTask(writer http.ResponseWriter, request *http.Request, pathID string) {
	service, err := s.requireSchedules()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload schedule.Task
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if pathID != "" {
		if payload.ID != "" && payload.ID != pathID {
			writeError(writer, fmt.Errorf("%w: 路径中的任务 ID 与请求体不一致", schedule.ErrInvalidRequest))
			return
		}
		payload.ID = pathID
	}
	if strings.TrimSpace(payload.UserID) == "" {
		payload.UserID = "scheduler"
	}
	item, err := service.Save(request.Context(), payload)
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

func (s *Server) getScheduledTask(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireSchedules()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := service.Get(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) deleteScheduledTask(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireSchedules()
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

func (s *Server) pauseScheduledTask(writer http.ResponseWriter, request *http.Request) {
	s.setScheduledTaskStatus(writer, request, schedule.StatusPaused)
}

func (s *Server) resumeScheduledTask(writer http.ResponseWriter, request *http.Request) {
	s.setScheduledTaskStatus(writer, request, schedule.StatusActive)
}

func (s *Server) setScheduledTaskStatus(writer http.ResponseWriter, request *http.Request, status schedule.Status) {
	service, err := s.requireSchedules()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := service.SetStatus(request.Context(), request.PathValue("id"), status)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}
