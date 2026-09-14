package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"Abot/internal/artifact"
	"Abot/internal/workspace"
	"gorm.io/gorm"
)

// workspaceRow 保存工作区连接信息；密码不会通过领域对象的 JSON 输出。
type workspaceRow struct {
	ID                 string `gorm:"primaryKey;size:64"`
	Name               string `gorm:"size:200;not null"`
	Type               string `gorm:"size:32;not null"`
	RootPath           string `gorm:"size:2000;not null"`
	RemoteTargetID     string `gorm:"index;size:64"`
	Host               string `gorm:"size:500"`
	Port               int
	User               string `gorm:"size:300"`
	AuthType           string `gorm:"size:32"`
	KeyPath            string `gorm:"size:2000"`
	Password           string `gorm:"size:4000"`
	HostKeyFingerprint string `gorm:"size:300"`
	Enabled            bool
	Status             string `gorm:"size:32"`
	StatusMessage      string `gorm:"size:1000"`
	LastCheckedAt      *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (workspaceRow) TableName() string { return "abot_workspaces" }

type workspaceOperationRow struct {
	ID                 string `gorm:"primaryKey;size:80"`
	UserID             string `gorm:"index;size:100"`
	WorkspaceID        string `gorm:"index;size:64;not null"`
	ConversationID     string `gorm:"index;size:100"`
	InvocationID       string `gorm:"index;size:100"`
	ToolCallID         string `gorm:"index;size:200"`
	CommandRunID       string `gorm:"index;size:100"`
	Type               string `gorm:"size:32;not null"`
	Path               string `gorm:"size:2000"`
	Content            string `gorm:"type:text"`
	PatchOld           string `gorm:"type:text"`
	PatchNew           string `gorm:"type:text"`
	PatchesJSON        string `gorm:"type:text"`
	ExpectedDigest     string `gorm:"size:128"`
	Command            string `gorm:"type:text"`
	CWD                string `gorm:"size:2000"`
	Timeout            int
	TTYJSON            string `gorm:"type:text"`
	Diff               string `gorm:"type:text"`
	DiffArtifactJSON   string `gorm:"type:text"`
	OutputArtifactJSON string `gorm:"type:text"`
	ArtifactError      string `gorm:"size:512"`
	Status             string `gorm:"index;size:32;not null"`
	Result             string `gorm:"type:text"`
	Error              string `gorm:"type:text"`
	ExitCode           *int
	CommandOutcome     string `gorm:"size:32"`
	OutputDigest       string `gorm:"size:128"`
	OutputTruncated    bool
	DurationMS         int64
	TimedOut           bool
	Unknown            bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (workspaceOperationRow) TableName() string { return "abot_workspace_operations" }

type workspaceCommandRunRow struct {
	ID                   string `gorm:"primaryKey;size:100"`
	UserID               string `gorm:"index;size:100"`
	WorkspaceID          string `gorm:"index;size:64;not null"`
	ConversationID       string `gorm:"index;size:100"`
	InvocationID         string `gorm:"index;size:100"`
	ToolCallID           string `gorm:"index;size:200"`
	OperationID          string `gorm:"index;size:80;not null"`
	Executor             string `gorm:"size:32;not null"`
	Mode                 string `gorm:"size:32;not null"`
	CapabilitiesJSON     string `gorm:"type:text"`
	CommandPreview       string `gorm:"type:text"`
	CWD                  string `gorm:"size:2000"`
	TimeoutMS            int64
	TTYJSON              string `gorm:"type:text"`
	Status               string `gorm:"index;size:32;not null"`
	Outcome              string `gorm:"size:32"`
	ExitCode             *int
	StdoutBytes          int64
	StderrBytes          int64
	StoredBytes          int64
	OutputDigest         string `gorm:"size:128"`
	OutputArtifactJSON   string `gorm:"type:text"`
	ArtifactError        string `gorm:"size:512"`
	OutputTruncated      bool
	OutputRetentionState string `gorm:"size:32"`
	OutputRetainedUntil  *time.Time
	OutputPurgedAt       *time.Time
	Error                string `gorm:"type:text"`
	Revision             int64  `gorm:"not null"`
	QueuedAt             time.Time
	StartedAt            *time.Time
	FinishedAt           *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

func (workspaceCommandRunRow) TableName() string { return "abot_workspace_command_runs" }

type workspaceCommandOutputChunkRow struct {
	CommandRunID string    `gorm:"primaryKey;size:100"`
	Sequence     uint64    `gorm:"primaryKey"`
	Stream       string    `gorm:"size:16;not null"`
	Offset       int64     `gorm:"not null"`
	Data         string    `gorm:"type:blob"`
	ByteLength   int64     `gorm:"not null"`
	CapturedAt   time.Time `gorm:"index"`
	Truncated    bool
}

func (workspaceCommandOutputChunkRow) TableName() string {
	return "abot_workspace_command_output_chunks"
}

type workspaceRepository struct {
	db *gorm.DB
}

func (r *workspaceRepository) List(ctx context.Context) ([]workspace.Workspace, error) {
	var rows []workspaceRow
	if err := r.db.WithContext(ctx).Order("created_at ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]workspace.Workspace, 0, len(rows))
	for _, row := range rows {
		result = append(result, workspaceFromRow(row))
	}
	return result, nil
}

func (r *workspaceRepository) Get(ctx context.Context, id string) (workspace.Workspace, error) {
	var row workspaceRow
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return workspace.Workspace{}, workspace.ErrNotFound
		}
		return workspace.Workspace{}, err
	}
	return workspaceFromRow(row), nil
}

func (r *workspaceRepository) Save(ctx context.Context, item workspace.Workspace) error {
	row := workspaceRow{
		ID: item.ID, Name: item.Name, Type: string(item.Type), RootPath: item.RootPath,
		RemoteTargetID: item.RemoteTargetID,
		Host:           item.Host, Port: item.Port, User: item.User, AuthType: string(item.AuthType),
		KeyPath: item.KeyPath, Password: item.Password, HostKeyFingerprint: item.HostKeyFingerprint,
		Enabled: item.Enabled, Status: string(item.Status), StatusMessage: item.StatusMessage,
		LastCheckedAt: item.LastCheckedAt, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
	return r.db.WithContext(ctx).Save(&row).Error
}

// CountByRemoteTarget 用于删除远程主机前检查仍在使用它的项目数量。
func (r *workspaceRepository) CountByRemoteTarget(ctx context.Context, targetID string) (int, error) {
	var count int64
	if err := r.db.WithContext(ctx).Model(&workspaceRow{}).Where("remote_target_id = ?", targetID).Count(&count).Error; err != nil {
		return 0, err
	}
	return int(count), nil
}

func (r *workspaceRepository) Delete(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 工作区删除前由 Service 确认没有对话；这里清理没有绑定对话的旧操作，避免留下孤儿记录。
		if err := tx.Where("workspace_id = ?", id).Delete(&workspaceOperationRow{}).Error; err != nil {
			return err
		}
		if err := tx.Exec("DELETE FROM abot_workspace_command_output_chunks WHERE command_run_id IN (SELECT id FROM abot_workspace_command_runs WHERE workspace_id = ?)", id).Error; err != nil {
			return err
		}
		if err := tx.Where("workspace_id = ?", id).Delete(&workspaceCommandRunRow{}).Error; err != nil {
			return err
		}
		result := tx.Where("id = ?", id).Delete(&workspaceRow{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return workspace.ErrNotFound
		}
		return nil
	})
}

func (r *workspaceRepository) ListOperations(ctx context.Context, workspaceID string) ([]workspace.Operation, error) {
	query := r.db.WithContext(ctx)
	if workspaceID != "" {
		query = query.Where("workspace_id = ?", workspaceID)
	}
	var rows []workspaceOperationRow
	if err := query.Order("created_at DESC").Limit(200).Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]workspace.Operation, 0, len(rows))
	for _, row := range rows {
		result = append(result, operationFromRow(row))
	}
	return result, nil
}

func (r *workspaceRepository) GetOperation(ctx context.Context, id string) (workspace.Operation, error) {
	var row workspaceOperationRow
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return workspace.Operation{}, workspace.ErrNotFound
		}
		return workspace.Operation{}, err
	}
	return operationFromRow(row), nil
}

func (r *workspaceRepository) SaveOperation(ctx context.Context, item workspace.Operation) error {
	row := workspaceOperationRow{
		ID: item.ID, UserID: item.UserID, WorkspaceID: item.WorkspaceID, ConversationID: item.ConversationID, InvocationID: item.InvocationID, ToolCallID: item.ToolCallID, CommandRunID: item.CommandRunID, Type: string(item.Type), Path: item.Path,
		Content: item.Content, PatchOld: item.PatchOld, PatchNew: item.PatchNew, Command: item.Command, CWD: item.CWD, Timeout: item.Timeout, TTYJSON: marshalTTYSpec(item.TTY),
		PatchesJSON: marshalPatches(item.Patches), ExpectedDigest: item.ExpectedDigest,
		Diff: item.Diff, DiffArtifactJSON: marshalArtifactRef(item.DiffArtifact), OutputArtifactJSON: marshalArtifactRef(item.OutputArtifact), ArtifactError: item.ArtifactError, Status: string(item.Status), Result: item.Result, Error: item.Error,
		ExitCode: item.ExitCode, CommandOutcome: item.CommandOutcome, OutputDigest: item.OutputDigest, OutputTruncated: item.OutputTruncated,
		DurationMS: item.DurationMS, TimedOut: item.TimedOut, Unknown: item.Unknown,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
	return r.db.WithContext(ctx).Save(&row).Error
}

func workspaceFromRow(row workspaceRow) workspace.Workspace {
	return workspace.Workspace{
		ID: row.ID, Name: row.Name, Type: workspace.Type(row.Type), RootPath: row.RootPath,
		RemoteTargetID: row.RemoteTargetID,
		Host:           row.Host, Port: row.Port, User: row.User, AuthType: workspace.AuthType(row.AuthType),
		KeyPath: row.KeyPath, Password: row.Password, PasswordConfigured: row.Password != "",
		HostKeyFingerprint: row.HostKeyFingerprint, Enabled: row.Enabled,
		Status: workspace.Status(row.Status), StatusMessage: row.StatusMessage,
		LastCheckedAt: row.LastCheckedAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func operationFromRow(row workspaceOperationRow) workspace.Operation {
	preview := truncateWorkspacePreview(row.Content)
	if preview == "" {
		// 非写入操作没有正文内容，补充可读摘要，避免审批列表只显示一行空白。
		switch workspace.OperationType(row.Type) {
		case workspace.OperationMakeDirectory:
			preview = "新建目录（批准后执行）"
		case workspace.OperationPatchFile:
			preview = "精确补丁已保存，批准时会再次校验"
		case workspace.OperationPatchSet:
			preview = "ChangeSet 已保存，批准时会再次校验"
		case workspace.OperationDeletePath:
			preview = "删除文件或空目录（批准后执行）"
		case workspace.OperationCommand:
			preview = truncateWorkspacePreview(row.Command)
		}
	}
	return workspace.Operation{
		ID: row.ID, UserID: row.UserID, WorkspaceID: row.WorkspaceID, ConversationID: row.ConversationID, InvocationID: row.InvocationID, ToolCallID: row.ToolCallID, CommandRunID: row.CommandRunID, Type: workspace.OperationType(row.Type),
		Path: row.Path, Content: row.Content, PatchOld: row.PatchOld, PatchNew: row.PatchNew, Command: row.Command, CWD: row.CWD,
		Patches: unmarshalPatches(row.PatchesJSON), ExpectedDigest: row.ExpectedDigest,
		Timeout: row.Timeout, TTY: unmarshalTTYSpec(row.TTYJSON), Preview: preview, Diff: row.Diff, DiffArtifact: unmarshalArtifactRef(row.DiffArtifactJSON), OutputArtifact: unmarshalArtifactRef(row.OutputArtifactJSON), ArtifactError: row.ArtifactError, Status: workspace.OperationStatus(row.Status), Result: row.Result,
		Error: row.Error, ExitCode: row.ExitCode, CommandOutcome: row.CommandOutcome, OutputDigest: row.OutputDigest, OutputTruncated: row.OutputTruncated,
		DurationMS: row.DurationMS, TimedOut: row.TimedOut, Unknown: row.Unknown, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func (r *workspaceRepository) CreateCommandRun(ctx context.Context, item workspace.CommandRun) error {
	if err := item.Validate(); err != nil {
		return err
	}
	return r.db.WithContext(ctx).Create(commandRunRowFromDomain(item)).Error
}

func (r *workspaceRepository) GetCommandRun(ctx context.Context, id string) (workspace.CommandRun, error) {
	var row workspaceCommandRunRow
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return workspace.CommandRun{}, workspace.ErrNotFound
		}
		return workspace.CommandRun{}, err
	}
	return commandRunFromRow(row), nil
}

func (r *workspaceRepository) ListCommandRuns(ctx context.Context, workspaceID string) ([]workspace.CommandRun, error) {
	query := r.db.WithContext(ctx)
	if workspaceID != "" {
		query = query.Where("workspace_id = ?", workspaceID)
	}
	var rows []workspaceCommandRunRow
	if err := query.Order("created_at DESC, id DESC").Limit(10000).Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]workspace.CommandRun, 0, len(rows))
	for _, row := range rows {
		result = append(result, commandRunFromRow(row))
	}
	return result, nil
}

func (r *workspaceRepository) UpdateCommandRun(ctx context.Context, item workspace.CommandRun, expectedRevision int64) (workspace.CommandRun, error) {
	if expectedRevision <= 0 || item.Revision != expectedRevision+1 {
		return workspace.CommandRun{}, workspace.ErrCommandRunConflict
	}
	if err := item.Validate(); err != nil {
		return workspace.CommandRun{}, err
	}
	current, err := r.GetCommandRun(ctx, item.ID)
	if err != nil {
		return workspace.CommandRun{}, err
	}
	if !workspace.EqualCommandExecutionCapabilities(current.Capabilities, item.Capabilities) {
		return workspace.CommandRun{}, fmt.Errorf("%w: admission 能力快照不可修改", workspace.ErrCommandRunConflict)
	}
	if !workspace.EqualTTYSpec(current.TTY, item.TTY) {
		return workspace.CommandRun{}, fmt.Errorf("%w: admission TTY spec 不可修改", workspace.ErrCommandRunConflict)
	}
	row := commandRunRowFromDomain(item)
	values := map[string]any{
		"user_id": row.UserID, "workspace_id": row.WorkspaceID, "conversation_id": row.ConversationID, "invocation_id": row.InvocationID,
		"tool_call_id": row.ToolCallID, "operation_id": row.OperationID, "executor": row.Executor, "mode": row.Mode,
		"capabilities_json": row.CapabilitiesJSON,
		"command_preview":   row.CommandPreview, "cwd": row.CWD, "timeout_ms": row.TimeoutMS, "tty_json": row.TTYJSON, "status": row.Status,
		"outcome": row.Outcome, "exit_code": row.ExitCode, "stdout_bytes": row.StdoutBytes, "stderr_bytes": row.StderrBytes,
		"stored_bytes": row.StoredBytes, "output_digest": row.OutputDigest, "output_artifact_json": row.OutputArtifactJSON, "artifact_error": row.ArtifactError, "output_truncated": row.OutputTruncated,
		"output_retention_state": row.OutputRetentionState, "output_retained_until": row.OutputRetainedUntil, "output_purged_at": row.OutputPurgedAt,
		"error": row.Error, "revision": row.Revision, "queued_at": row.QueuedAt, "started_at": row.StartedAt,
		"finished_at": row.FinishedAt, "created_at": row.CreatedAt, "updated_at": row.UpdatedAt,
	}
	result := r.db.WithContext(ctx).Model(&workspaceCommandRunRow{}).Where("id = ? AND revision = ?", item.ID, expectedRevision).Updates(values)
	if result.Error != nil {
		return workspace.CommandRun{}, result.Error
	}
	if result.RowsAffected == 0 {
		if _, err := r.GetCommandRun(ctx, item.ID); errors.Is(err, workspace.ErrNotFound) {
			return workspace.CommandRun{}, workspace.ErrNotFound
		}
		return workspace.CommandRun{}, workspace.ErrCommandRunConflict
	}
	return r.GetCommandRun(ctx, item.ID)
}

func (r *workspaceRepository) DeleteCommandRun(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("command_run_id = ?", id).Delete(&workspaceCommandOutputChunkRow{}).Error; err != nil {
			return err
		}
		result := tx.Where("id = ?", id).Delete(&workspaceCommandRunRow{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return workspace.ErrNotFound
		}
		return nil
	})
}

func (r *workspaceRepository) CreateCommandOutputChunk(ctx context.Context, item workspace.CommandOutputChunk) error {
	if err := item.Validate(); err != nil {
		return err
	}
	if _, err := r.GetCommandRun(ctx, item.CommandRunID); err != nil {
		return err
	}
	row := commandOutputChunkRowFromDomain(item)
	if err := r.db.WithContext(ctx).Create(&row).Error; err != nil {
		// Retries after a successful insert are idempotent only when all fields
		// match; a sequence collision with different bytes is an audit conflict.
		var existing workspaceCommandOutputChunkRow
		if getErr := r.db.WithContext(ctx).Where("command_run_id = ? AND sequence = ?", item.CommandRunID, item.Sequence).First(&existing).Error; getErr == nil {
			if commandOutputChunksEqual(commandOutputChunkFromRow(existing), item) {
				return nil
			}
			return workspace.ErrCommandRunConflict
		}
		return err
	}
	return nil
}

func (r *workspaceRepository) ListCommandOutputChunks(ctx context.Context, commandRunID string, after uint64, limit int) ([]workspace.CommandOutputChunk, error) {
	if limit <= 0 {
		return nil, workspace.ErrInvalidRequest
	}
	var rows []workspaceCommandOutputChunkRow
	if err := r.db.WithContext(ctx).Where("command_run_id = ? AND sequence > ?", commandRunID, after).Order("sequence ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]workspace.CommandOutputChunk, 0, len(rows))
	for _, row := range rows {
		result = append(result, commandOutputChunkFromRow(row))
	}
	return result, nil
}

func (r *workspaceRepository) DeleteCommandOutputChunks(ctx context.Context, commandRunID string) error {
	return r.db.WithContext(ctx).Where("command_run_id = ?", commandRunID).Delete(&workspaceCommandOutputChunkRow{}).Error
}

// PurgeCommandOutput atomically deletes replayable chunks and advances the
// retention metadata under the same revision check. A second cleanup worker
// therefore observes a conflict and cannot report a duplicate purge.
func (r *workspaceRepository) PurgeCommandOutput(ctx context.Context, id string, expectedRevision int64, item workspace.CommandRun) (workspace.CommandRun, error) {
	if expectedRevision <= 0 || item.Revision != expectedRevision+1 {
		return workspace.CommandRun{}, workspace.ErrCommandRunConflict
	}
	if err := item.Validate(); err != nil {
		return workspace.CommandRun{}, err
	}
	var loaded workspaceCommandRunRow
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("command_run_id = ?", id).Delete(&workspaceCommandOutputChunkRow{}).Error; err != nil {
			return err
		}
		row := commandRunRowFromDomain(item)
		values := map[string]any{
			"output_retention_state": row.OutputRetentionState, "output_retained_until": row.OutputRetainedUntil,
			"output_purged_at": row.OutputPurgedAt, "stored_bytes": row.StoredBytes, "output_digest": row.OutputDigest,
			"output_truncated": row.OutputTruncated, "revision": row.Revision, "updated_at": row.UpdatedAt,
		}
		updated := tx.Model(&workspaceCommandRunRow{}).Where("id = ? AND revision = ?", id, expectedRevision).Updates(values)
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected == 0 {
			var exists workspaceCommandRunRow
			if getErr := tx.Where("id = ?", id).First(&exists).Error; errors.Is(getErr, gorm.ErrRecordNotFound) {
				return workspace.ErrNotFound
			}
			return workspace.ErrCommandRunConflict
		}
		return tx.Where("id = ?", id).First(&loaded).Error
	})
	if err != nil {
		return workspace.CommandRun{}, err
	}
	return commandRunFromRow(loaded), nil
}

func commandRunRowFromDomain(item workspace.CommandRun) workspaceCommandRunRow {
	return workspaceCommandRunRow{
		ID: item.ID, UserID: item.UserID, WorkspaceID: item.WorkspaceID, ConversationID: item.ConversationID, InvocationID: item.InvocationID,
		ToolCallID: item.ToolCallID, OperationID: item.OperationID, Executor: item.Executor, Mode: item.Mode,
		CapabilitiesJSON: marshalCommandExecutionCapabilities(item.Capabilities),
		CommandPreview:   item.CommandPreview, CWD: item.CWD, TimeoutMS: item.TimeoutMS, TTYJSON: marshalTTYSpec(item.TTY), Status: string(item.Status), Outcome: string(item.Outcome),
		ExitCode: item.ExitCode, StdoutBytes: item.StdoutBytes, StderrBytes: item.StderrBytes, StoredBytes: item.StoredBytes,
		OutputDigest: item.OutputDigest, OutputArtifactJSON: marshalArtifactRef(item.OutputArtifact), ArtifactError: item.ArtifactError, OutputTruncated: item.OutputTruncated, OutputRetentionState: string(item.OutputRetentionState), OutputRetainedUntil: item.OutputRetainedUntil, OutputPurgedAt: item.OutputPurgedAt, Error: item.Error, Revision: item.Revision,
		QueuedAt: item.QueuedAt, StartedAt: item.StartedAt, FinishedAt: item.FinishedAt, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
}

func commandRunFromRow(row workspaceCommandRunRow) workspace.CommandRun {
	return workspace.CommandRun{
		ID: row.ID, UserID: row.UserID, WorkspaceID: row.WorkspaceID, ConversationID: row.ConversationID, InvocationID: row.InvocationID,
		ToolCallID: row.ToolCallID, OperationID: row.OperationID, Executor: row.Executor, Mode: row.Mode, Capabilities: unmarshalCommandExecutionCapabilities(row.CapabilitiesJSON, row.Executor),
		CommandPreview: row.CommandPreview, CWD: row.CWD, TimeoutMS: row.TimeoutMS, TTY: unmarshalTTYSpec(row.TTYJSON), Status: workspace.CommandRunStatus(row.Status), Outcome: workspace.CommandRunOutcome(row.Outcome),
		ExitCode: row.ExitCode, StdoutBytes: row.StdoutBytes, StderrBytes: row.StderrBytes, StoredBytes: row.StoredBytes,
		OutputDigest: row.OutputDigest, OutputArtifact: unmarshalArtifactRef(row.OutputArtifactJSON), ArtifactError: row.ArtifactError, OutputTruncated: row.OutputTruncated, OutputRetentionState: workspace.CommandOutputRetentionState(row.OutputRetentionState), OutputRetainedUntil: row.OutputRetainedUntil, OutputPurgedAt: row.OutputPurgedAt, Error: row.Error, Revision: row.Revision,
		QueuedAt: row.QueuedAt, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func marshalCommandExecutionCapabilities(value *workspace.CommandExecutionCapabilities) string {
	if value == nil {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		// The struct contains only scalar fields, so this is defensive. An
		// empty value is safer than writing a partial, non-parseable snapshot.
		return ""
	}
	return string(encoded)
}

func marshalArtifactRef(value *artifact.ArtifactRef) string {
	if value == nil {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func unmarshalArtifactRef(value string) *artifact.ArtifactRef {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	var ref artifact.ArtifactRef
	if err := json.Unmarshal([]byte(value), &ref); err != nil || strings.TrimSpace(ref.ID) == "" {
		return nil
	}
	return &ref
}

func marshalTTYSpec(value *workspace.TTYSpec) string {
	if value == nil {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func unmarshalTTYSpec(value string) *workspace.TTYSpec {
	if value == "" {
		return nil
	}
	var spec workspace.TTYSpec
	if err := json.Unmarshal([]byte(value), &spec); err != nil || spec.Validate() != nil {
		return nil
	}
	return &spec
}

func unmarshalCommandExecutionCapabilities(value, executor string) *workspace.CommandExecutionCapabilities {
	if value != "" {
		var capabilities workspace.CommandExecutionCapabilities
		if err := json.Unmarshal([]byte(value), &capabilities); err == nil && capabilities.Validate() == nil {
			return &capabilities
		}
		// A non-empty but malformed snapshot is different from a legacy row
		// that never had one. Do not infer any positive guarantee from corrupt
		// data; surface an entirely unknown profile instead.
		return workspace.DefaultCommandExecutionCapabilities(workspace.Workspace{Type: workspace.Type("unknown")})
	}
	// Rows written before capability disclosure have no snapshot. Deriving the
	// conservative profile from the immutable executor keeps old rows useful
	// while failing closed about isolation.
	itemType := workspace.Type("unknown")
	if executor == "local" {
		itemType = workspace.TypeLocal
	} else if executor == "ssh" {
		itemType = workspace.TypeSSH
	}
	return workspace.DefaultCommandExecutionCapabilities(workspace.Workspace{Type: itemType})
}

func commandOutputChunkRowFromDomain(item workspace.CommandOutputChunk) workspaceCommandOutputChunkRow {
	return workspaceCommandOutputChunkRow{
		CommandRunID: item.CommandRunID, Sequence: item.Sequence, Stream: item.Stream, Offset: item.Offset,
		Data: item.Data, ByteLength: item.ByteLength, CapturedAt: item.CapturedAt, Truncated: item.Truncated,
	}
}

func commandOutputChunkFromRow(row workspaceCommandOutputChunkRow) workspace.CommandOutputChunk {
	return workspace.CommandOutputChunk{
		CommandRunID: row.CommandRunID, Sequence: row.Sequence, Stream: row.Stream, Offset: row.Offset,
		Data: row.Data, ByteLength: row.ByteLength, CapturedAt: row.CapturedAt, Truncated: row.Truncated,
	}
}

func commandOutputChunksEqual(left, right workspace.CommandOutputChunk) bool {
	return left.CommandRunID == right.CommandRunID && left.Sequence == right.Sequence && left.Stream == right.Stream && left.Offset == right.Offset && left.Data == right.Data && left.ByteLength == right.ByteLength && left.CapturedAt.Equal(right.CapturedAt) && left.Truncated == right.Truncated
}

func marshalPatches(value []workspace.FilePatch) string {
	if len(value) == 0 {
		return ""
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(raw)
}

func unmarshalPatches(value string) []workspace.FilePatch {
	if value == "" {
		return nil
	}
	var result []workspace.FilePatch
	if err := json.Unmarshal([]byte(value), &result); err != nil {
		return nil
	}
	return result
}

func truncateWorkspacePreview(value string) string {
	runes := []rune(value)
	if len(runes) <= 4000 {
		return value
	}
	return string(runes[:4000]) + "…"
}
