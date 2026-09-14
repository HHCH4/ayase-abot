package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"Abot/internal/workspace"
	"github.com/gorilla/websocket"
)

type workspacePayload struct {
	ID                 string             `json:"id"`
	Name               string             `json:"name"`
	Type               workspace.Type     `json:"type"`
	RootPath           string             `json:"root_path"`
	RemoteTargetID     string             `json:"remote_target_id"`
	Host               string             `json:"host"`
	Port               int                `json:"port"`
	User               string             `json:"user"`
	AuthType           workspace.AuthType `json:"auth_type"`
	KeyPath            string             `json:"key_path"`
	Password           *string            `json:"password"`
	HostKeyFingerprint string             `json:"host_key_fingerprint"`
	Enabled            *bool              `json:"enabled"`
}

type operationPayload struct {
	ConversationID string                  `json:"conversation_id"`
	Type           workspace.OperationType `json:"type"`
	Path           string                  `json:"path"`
	Content        string                  `json:"content"`
	OldText        string                  `json:"old_text"`
	NewText        string                  `json:"new_text"`
	Command        string                  `json:"command"`
	CWD            string                  `json:"cwd"`
	TimeoutSeconds int                     `json:"timeout_seconds"`
	TTY            *workspace.TTYSpec      `json:"tty,omitempty"`
}

type readManyWorkspacePayload struct {
	Paths []string `json:"paths"`
}

type commandRunCancelPayload struct {
	Reason string `json:"reason"`
}

type commandPTYFrame struct {
	Type         string `json:"type"`
	CommandRunID string `json:"command_run_id,omitempty"`
	Sequence     uint64 `json:"sequence,omitempty"`
	Offset       int64  `json:"offset,omitempty"`
	Stream       string `json:"stream,omitempty"`
	Data         string `json:"data,omitempty"`
	RequestSeq   uint64 `json:"request_seq,omitempty"`
	Rows         uint16 `json:"rows,omitempty"`
	Cols         uint16 `json:"cols,omitempty"`
	Signal       string `json:"signal,omitempty"`
	Lease        string `json:"lease,omitempty"`
	Writer       bool   `json:"writer,omitempty"`
	Status       string `json:"status,omitempty"`
	Error        string `json:"error,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"`
}

type commandPTYWriter struct {
	connection *websocket.Conn
	mu         sync.Mutex
}

func (writer *commandPTYWriter) send(frame commandPTYFrame) error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	_ = writer.connection.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return writer.connection.WriteJSON(frame)
}

func (s *Server) requireWorkspaces() (*workspace.Service, error) {
	if s.workspaces == nil {
		return nil, errors.New("工作区服务尚未装配")
	}
	return s.workspaces, nil
}

// listLocalDirectories 只服务于项目创建向导，返回本机可进入的目录，不暴露文件内容。
func (s *Server) listLocalDirectories(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	listing, err := service.ListLocalDirectories(request.Context(), request.URL.Query().Get("path"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, listing)
}

func (s *Server) listWorkspaces(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := service.List(request.Context())
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"workspaces": items})
}

func (s *Server) getWorkspace(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
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

func (s *Server) createWorkspace(writer http.ResponseWriter, request *http.Request) {
	s.saveWorkspace(writer, request, "")
}

func (s *Server) updateWorkspace(writer http.ResponseWriter, request *http.Request) {
	s.saveWorkspace(writer, request, request.PathValue("id"))
}

func (s *Server) saveWorkspace(writer http.ResponseWriter, request *http.Request, pathID string) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload workspacePayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if pathID != "" {
		if payload.ID != "" && payload.ID != pathID {
			writeError(writer, errors.New("路径中的工作区 ID 与请求体不一致"))
			return
		}
		payload.ID = pathID
	}
	enabled := true
	if pathID != "" {
		if old, getErr := service.Get(request.Context(), pathID); getErr == nil {
			enabled = old.Enabled
		}
	}
	if payload.Enabled != nil {
		enabled = *payload.Enabled
	}
	item, err := service.Save(request.Context(), workspace.SaveRequest{Workspace: workspace.Workspace{
		ID: payload.ID, Name: payload.Name, Type: payload.Type, RootPath: payload.RootPath,
		RemoteTargetID: payload.RemoteTargetID,
		Host:           payload.Host, Port: payload.Port, User: payload.User, AuthType: payload.AuthType,
		KeyPath: payload.KeyPath, HostKeyFingerprint: payload.HostKeyFingerprint, Enabled: enabled,
	}, Password: payload.Password})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) deleteWorkspace(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	if deleteError := service.Delete(request.Context(), request.PathValue("id")); deleteError != nil {
		writeError(writer, deleteError)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) testWorkspace(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	result, err := service.Test(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (s *Server) previewTestWorkspace(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload workspacePayload
	if decodeError := decodeJSON(writer, request, &payload); decodeError != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", decodeError))
		return
	}
	result, err := service.TestPreview(request.Context(), workspace.SaveRequest{Workspace: workspace.Workspace{
		ID: payload.ID, Name: payload.Name, Type: payload.Type, RootPath: payload.RootPath,
		RemoteTargetID: payload.RemoteTargetID,
		Host:           payload.Host, Port: payload.Port, User: payload.User, AuthType: payload.AuthType,
		KeyPath: payload.KeyPath, HostKeyFingerprint: payload.HostKeyFingerprint, Enabled: true,
	}, Password: payload.Password})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (s *Server) listWorkspaceFiles(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := service.ListFiles(request.Context(), request.PathValue("id"), request.URL.Query().Get("path"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"files": items})
}

func (s *Server) readWorkspaceFile(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	filePath := request.URL.Query().Get("path")
	content, err := service.ReadFile(request.Context(), request.PathValue("id"), filePath)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"path": filePath, "content": content})
}

func (s *Server) searchWorkspace(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	query := request.URL.Query()
	options := workspace.SearchOptions{
		Mode: workspace.SearchMode(query.Get("mode")),
		Glob: query.Get("glob"),
	}
	caseInsensitiveRaw := strings.TrimSpace(query.Get("case_insensitive"))
	caseSensitiveRaw := strings.TrimSpace(query.Get("case_sensitive"))
	var caseInsensitiveSet bool
	if caseInsensitiveRaw != "" {
		parsed, parseErr := strconv.ParseBool(caseInsensitiveRaw)
		if parseErr != nil {
			writeError(writer, fmt.Errorf("%w: case_insensitive 必须是布尔值", workspace.ErrInvalidRequest))
			return
		}
		options.CaseInsensitive = parsed
		caseInsensitiveSet = true
	}
	if caseSensitiveRaw != "" {
		parsed, parseErr := strconv.ParseBool(caseSensitiveRaw)
		if parseErr != nil {
			writeError(writer, fmt.Errorf("%w: case_sensitive 必须是布尔值", workspace.ErrInvalidRequest))
			return
		}
		if caseInsensitiveSet && options.CaseInsensitive == parsed {
			writeError(writer, fmt.Errorf("%w: case_insensitive 与 case_sensitive 冲突", workspace.ErrInvalidRequest))
			return
		}
		options.CaseInsensitive = !parsed
	}
	if raw := strings.TrimSpace(query.Get("context_lines")); raw != "" {
		options.ContextLines, err = strconv.Atoi(raw)
		if err != nil {
			writeError(writer, fmt.Errorf("%w: context_lines 必须是整数", workspace.ErrInvalidRequest))
			return
		}
	}
	if raw := strings.TrimSpace(query.Get("max_hits")); raw != "" {
		options.MaxHits, err = strconv.Atoi(raw)
		if err != nil {
			writeError(writer, fmt.Errorf("%w: max_hits 必须是整数", workspace.ErrInvalidRequest))
			return
		}
	}
	matches, err := service.SearchWithOptions(request.Context(), request.PathValue("id"), query.Get("path"), query.Get("q"), options)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"matches": matches})
}

func (s *Server) globWorkspace(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	result, err := service.Glob(request.Context(), request.PathValue("id"), request.URL.Query().Get("pattern"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (s *Server) readWorkspaceRange(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	startLine, endLine := 0, 0
	if value := request.URL.Query().Get("start_line"); value != "" {
		parsed, scanErr := strconv.Atoi(value)
		if scanErr != nil {
			writeError(writer, fmt.Errorf("start_line 无效: %w", scanErr))
			return
		}
		startLine = parsed
	}
	if value := request.URL.Query().Get("end_line"); value != "" {
		parsed, scanErr := strconv.Atoi(value)
		if scanErr != nil {
			writeError(writer, fmt.Errorf("end_line 无效: %w", scanErr))
			return
		}
		endLine = parsed
	}
	includeLineNumbers := request.URL.Query().Get("include_line_numbers") == "true"
	result, err := service.ReadRange(request.Context(), request.PathValue("id"), request.URL.Query().Get("path"), startLine, endLine, includeLineNumbers)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (s *Server) readWorkspaceMany(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload readManyWorkspacePayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	result, err := service.ReadMany(request.Context(), request.PathValue("id"), payload.Paths)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (s *Server) workspaceStat(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	stat, err := service.Stat(request.Context(), request.PathValue("id"), request.URL.Query().Get("path"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, stat)
}

func (s *Server) workspaceGitStatus(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	result, err := service.GitStatus(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (s *Server) workspaceGitDiff(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	result, err := service.GitDiff(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (s *Server) workspaceGitLog(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	result, err := service.GitLog(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (s *Server) createWorkspaceOperation(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload operationPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	userID := strings.TrimSpace(request.Header.Get("X-Abot-User-ID"))
	if userID == "" {
		userID = strings.TrimSpace(request.URL.Query().Get("user_id"))
	}
	operation, err := service.CreateOperation(request.Context(), workspace.OperationRequest{
		WorkspaceID: request.PathValue("id"), UserID: userID, ConversationID: payload.ConversationID, Type: payload.Type, Path: payload.Path,
		Content: payload.Content, OldText: payload.OldText, NewText: payload.NewText, Command: payload.Command, CWD: payload.CWD, Timeout: payload.TimeoutSeconds, TTY: payload.TTY,
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, operation)
}

func (s *Server) listWorkspaceOperations(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := service.ListOperations(request.Context(), request.URL.Query().Get("workspace_id"))
	conversationID := strings.TrimSpace(request.URL.Query().Get("conversation_id"))
	if err == nil && conversationID != "" {
		filtered := make([]workspace.Operation, 0, len(items))
		for _, item := range items {
			if item.ConversationID == conversationID {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"operations": items})
}

func (s *Server) listWorkspaceCommandRuns(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := service.ListCommandRuns(request.Context(), request.URL.Query().Get("workspace_id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	if invocationID := strings.TrimSpace(request.URL.Query().Get("invocation_id")); invocationID != "" {
		filtered := make([]workspace.CommandRun, 0, len(items))
		for _, item := range items {
			if item.InvocationID == invocationID {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	if operationID := strings.TrimSpace(request.URL.Query().Get("operation_id")); operationID != "" {
		filtered := make([]workspace.CommandRun, 0, len(items))
		for _, item := range items {
			if item.OperationID == operationID {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	writeJSON(writer, http.StatusOK, map[string]any{"command_runs": items})
}

func (s *Server) getWorkspaceCommandRun(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := service.GetCommandRun(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

// attachWorkspaceCommandPTY exposes the in-process PTY only as an explicit
// WebSocket stream. Durable terminal chunks are replayed first; live chunks
// are best-effort and reconnects use the sequence cursor to avoid duplicates.
func (s *Server) attachWorkspaceCommandPTY(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	runID := strings.TrimSpace(request.PathValue("id"))
	run, err := service.GetCommandRun(request.Context(), runID)
	if err != nil {
		writeError(writer, err)
		return
	}
	if run.TTY == nil || !run.TTY.Enabled || run.Mode != "pty" {
		writeError(writer, workspace.ErrPTYUnavailable)
		return
	}
	if !run.Status.Terminal() && run.Status != workspace.CommandRunRunning {
		writeError(writer, fmt.Errorf("%w: 当前状态为 %s", workspace.ErrOperationState, run.Status))
		return
	}
	writerRequested, parseErr := parsePTYWriterRequest(request)
	if parseErr != nil {
		writeError(writer, parseErr)
		return
	}
	lease := strings.TrimSpace(request.URL.Query().Get("lease"))
	if writerRequested {
		lease, err = service.AcquireCommandPTYWriter(request.Context(), runID, lease)
		if err != nil {
			writeError(writer, err)
			return
		}
	}

	upgrader := websocket.Upgrader{
		ReadBufferSize:  4 * 1024,
		WriteBufferSize: 64 * 1024,
		CheckOrigin: func(req *http.Request) bool {
			origin := strings.TrimSpace(req.Header.Get("Origin"))
			if origin == "" {
				return true
			}
			parsed, parseOriginErr := url.Parse(origin)
			return parseOriginErr == nil && parsed.Host != "" && parsed.Host == req.Host
		},
	}
	connection, err := upgrader.Upgrade(writer, request, nil)
	if err != nil {
		if writerRequested {
			service.ReleaseCommandPTYWriter(runID, lease)
		}
		return
	}
	defer connection.Close()
	if writerRequested {
		defer service.ReleaseCommandPTYWriter(runID, lease)
	}
	ptyWriter := &commandPTYWriter{connection: connection}
	if err := ptyWriter.send(commandPTYFrame{Type: "attached", CommandRunID: runID, Writer: writerRequested, Lease: lease, Status: string(run.Status)}); err != nil {
		return
	}

	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	streamErr := make(chan error, 1)
	go func() {
		streamErr <- s.streamWorkspaceCommandPTY(ctx, service, runID, ptyWriter)
	}()
	controlErr := s.readWorkspaceCommandPTY(ctx, service, runID, lease, writerRequested, ptyWriter)
	cancel()
	connection.Close()
	select {
	case streamError := <-streamErr:
		if controlErr == nil {
			controlErr = streamError
		}
	case <-time.After(500 * time.Millisecond):
	}
	// The connection has already been upgraded, so JSON error bodies are no
	// longer possible. Errors are sent as bounded frames by the read/stream
	// loops; keep this final value for structured server logging only.
	if controlErr != nil && !errors.Is(controlErr, context.Canceled) {
		slog.Debug("PTY WebSocket 已结束", "command_run_id", runID, "error", controlErr)
	}
}

func parsePTYWriterRequest(request *http.Request) (bool, error) {
	raw := strings.TrimSpace(request.URL.Query().Get("writer"))
	if raw == "" {
		raw = strings.TrimSpace(request.URL.Query().Get("mode"))
	}
	if raw == "" {
		raw = strings.TrimSpace(request.Header.Get("X-Abot-PTY-Writer"))
	}
	if raw == "" {
		return false, nil
	}
	parsed, err := strconv.ParseBool(raw)
	if strings.EqualFold(raw, "write") {
		parsed, err = true, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: writer 必须是布尔值或 write", workspace.ErrInvalidRequest)
	}
	return parsed, nil
}

func (s *Server) streamWorkspaceCommandPTY(ctx context.Context, service *workspace.Service, runID string, writer *commandPTYWriter) error {
	live, release, subscribeErr := service.SubscribeCommandPTYOutput(runID)
	if subscribeErr != nil && !errors.Is(subscribeErr, workspace.ErrPTYUnavailable) {
		return subscribeErr
	}
	if release != nil {
		defer release()
	}
	var after uint64
	terminalIdle := 0
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	flush := func() (bool, error) {
		page, err := service.ListCommandOutput(ctx, runID, after, 200)
		if err != nil {
			if errors.Is(err, workspace.ErrUnsupported) {
				return false, nil
			}
			return false, err
		}
		wrote := false
		for _, chunk := range page.Chunks {
			if chunk.Sequence <= after {
				continue
			}
			if err := writer.send(commandPTYFrame{Type: "chunk", CommandRunID: runID, Sequence: chunk.Sequence, Offset: chunk.Offset, Stream: chunk.Stream, Data: base64.StdEncoding.EncodeToString([]byte(chunk.Data)), Truncated: chunk.Truncated}); err != nil {
				return wrote, err
			}
			after = chunk.Sequence
			wrote = true
		}
		return wrote, nil
	}
	for {
		wrote, err := flush()
		if err != nil {
			return err
		}
		run, runErr := service.GetCommandRun(ctx, runID)
		if runErr != nil {
			return runErr
		}
		if run.Status.Terminal() && !pageHasMore(service, ctx, runID, after) {
			if wrote {
				terminalIdle = 0
			} else if live == nil {
				terminalIdle++
				if terminalIdle >= 4 {
					_ = writer.send(commandPTYFrame{Type: "done", CommandRunID: runID, Sequence: after, Status: string(run.Status)})
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case chunk, ok := <-live:
			if !ok {
				live = nil
				continue
			}
			if chunk.Sequence <= after {
				continue
			}
			if err := writer.send(commandPTYFrame{Type: "chunk", CommandRunID: runID, Sequence: chunk.Sequence, Offset: chunk.Offset, Stream: chunk.Stream, Data: base64.StdEncoding.EncodeToString([]byte(chunk.Data)), Truncated: chunk.Truncated}); err != nil {
				return err
			}
			after = chunk.Sequence
		case <-ticker.C:
		}
	}
}

func pageHasMore(service *workspace.Service, ctx context.Context, runID string, after uint64) bool {
	page, err := service.ListCommandOutput(ctx, runID, after, 1)
	return err == nil && page.HasMore
}

func (s *Server) readWorkspaceCommandPTY(ctx context.Context, service *workspace.Service, runID, lease string, writerRequested bool, writer *commandPTYWriter) error {
	connection := writer.connection
	var lastRequestSeq uint64
	for {
		_, message, err := connection.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		var frame commandPTYFrame
		decoder := json.NewDecoder(bytes.NewReader(message))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&frame); err != nil {
			_ = writer.send(commandPTYFrame{Type: "error", Error: fmt.Sprintf("PTY 控制帧无效: %v", err)})
			return fmt.Errorf("%w: PTY 控制帧无效", workspace.ErrInvalidRequest)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			_ = writer.send(commandPTYFrame{Type: "error", Error: "PTY 控制帧只能包含一个 JSON 对象"})
			return fmt.Errorf("%w: PTY 控制帧包含多余内容", workspace.ErrInvalidRequest)
		}
		if frame.RequestSeq == 0 || frame.RequestSeq <= lastRequestSeq {
			_ = writer.send(commandPTYFrame{Type: "error", Error: "PTY request_seq 必须严格递增"})
			return fmt.Errorf("%w: PTY request_seq 无序", workspace.ErrInvalidRequest)
		}
		lastRequestSeq = frame.RequestSeq
		switch frame.Type {
		case "detach":
			_ = writer.send(commandPTYFrame{Type: "detached", CommandRunID: runID, Sequence: frame.RequestSeq})
			return nil
		case "stdin":
			if !writerRequested || strings.TrimSpace(frame.Lease) != lease {
				return sendPTYControlError(writer, workspace.ErrPTYWriterDenied)
			}
			data, decodeErr := base64.StdEncoding.DecodeString(frame.Data)
			if decodeErr != nil {
				return sendPTYControlError(writer, fmt.Errorf("%w: stdin 必须是 base64", workspace.ErrInvalidRequest))
			}
			if _, writeErr := service.WriteCommandPTY(ctx, runID, lease, data); writeErr != nil {
				return sendPTYControlError(writer, writeErr)
			}
		case "resize":
			if !writerRequested || strings.TrimSpace(frame.Lease) != lease {
				return sendPTYControlError(writer, workspace.ErrPTYWriterDenied)
			}
			if resizeErr := service.ResizeCommandPTY(ctx, runID, frame.Rows, frame.Cols); resizeErr != nil {
				return sendPTYControlError(writer, resizeErr)
			}
		case "signal":
			if !writerRequested || strings.TrimSpace(frame.Lease) != lease {
				return sendPTYControlError(writer, workspace.ErrPTYWriterDenied)
			}
			if signalErr := service.SignalCommandPTY(ctx, runID, lease, frame.Signal); signalErr != nil {
				return sendPTYControlError(writer, signalErr)
			}
		default:
			return sendPTYControlError(writer, fmt.Errorf("%w: 未知 PTY 控制类型 %q", workspace.ErrInvalidRequest, frame.Type))
		}
	}
}

func sendPTYControlError(writer *commandPTYWriter, err error) error {
	_ = writer.send(commandPTYFrame{Type: "error", Error: err.Error()})
	return err
}

func (s *Server) getWorkspaceCommandOutput(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	var after uint64
	if raw := strings.TrimSpace(request.URL.Query().Get("after")); raw != "" {
		after, err = strconv.ParseUint(raw, 10, 64)
		if err != nil {
			writeError(writer, fmt.Errorf("%w: after 必须是非负整数", workspace.ErrInvalidRequest))
			return
		}
	}
	limit := 0
	if raw := strings.TrimSpace(request.URL.Query().Get("limit")); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil {
			writeError(writer, fmt.Errorf("%w: limit 必须是整数", workspace.ErrInvalidRequest))
			return
		}
	}
	page, err := service.ListCommandOutput(request.Context(), request.PathValue("id"), after, limit)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, page)
}

func (s *Server) downloadWorkspaceCommandOutput(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	runID := request.PathValue("id")
	rawFormat := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("format")))
	if rawFormat == "" {
		rawFormat = string(workspace.CommandOutputDownloadNDJSON)
	}
	format := workspace.CommandOutputDownloadFormat(rawFormat)
	if format != workspace.CommandOutputDownloadText && format != workspace.CommandOutputDownloadNDJSON {
		writeError(writer, fmt.Errorf("%w: 不支持的命令输出下载格式 %q", workspace.ErrInvalidRequest, rawFormat))
		return
	}
	// Resolve the run and optional output extension before sending binary
	// headers, so unknown IDs and repositories get a normal JSON error.
	run, err := service.GetCommandRun(request.Context(), runID)
	if err != nil {
		writeError(writer, err)
		return
	}
	page, err := service.ListCommandOutput(request.Context(), runID, 0, 1)
	if err != nil {
		writeError(writer, err)
		return
	}
	if page.RetentionState == workspace.CommandOutputRetentionPurged {
		writeError(writer, workspace.ErrCommandOutputExpired)
		return
	}
	extension := "jsonl"
	contentType := "application/x-ndjson; charset=utf-8"
	if format == workspace.CommandOutputDownloadText {
		extension = "log"
		contentType = "text/plain; charset=utf-8"
	}
	filenameID := safeCommandRunFilename(runID)
	writer.Header().Set("Content-Type", contentType)
	writer.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"command-%s.%s\"", filenameID, extension))
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Abot-Output-Format", string(format))
	writer.Header().Set("X-Abot-Output-Truncated", strconv.FormatBool(run.OutputTruncated))
	writer.WriteHeader(http.StatusOK)
	if _, err := service.DownloadCommandOutput(request.Context(), runID, format, writer); err != nil {
		// Headers are already committed for a streaming response. The command
		// checkpoint remains authoritative; log the transport error instead of
		// attempting to append a JSON error body to a binary download.
		slog.Debug("命令输出下载中断", "command_run_id", runID, "error", err)
	}
}

func safeCommandRunFilename(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character >= '0' && character <= '9', character == '.', character == '-', character == '_':
			builder.WriteRune(character)
		default:
			builder.WriteByte('_')
		}
		if builder.Len() >= 80 {
			break
		}
	}
	if builder.Len() == 0 {
		return "run"
	}
	return builder.String()
}

func (s *Server) cancelWorkspaceCommandRun(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	reason := strings.TrimSpace(request.URL.Query().Get("reason"))
	if request.ContentLength > 0 {
		var payload commandRunCancelPayload
		if decodeErr := decodeJSON(writer, request, &payload); decodeErr != nil {
			writeError(writer, fmt.Errorf("请求体无效: %w", decodeErr))
			return
		}
		if strings.TrimSpace(payload.Reason) != "" {
			reason = strings.TrimSpace(payload.Reason)
		}
	}
	run, err := service.CancelCommandRun(request.Context(), request.PathValue("id"), reason)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, run)
}

func (s *Server) approveWorkspaceOperation(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	operation, err := service.ApproveOperation(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, operation)
}

func (s *Server) rejectWorkspaceOperation(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireWorkspaces()
	if err != nil {
		writeError(writer, err)
		return
	}
	operation, err := service.RejectOperation(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, operation)
}
