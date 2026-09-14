package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"Abot/internal/artifact"
)

var (
	ErrNotFound         = errors.New("工作区不存在")
	ErrInvalidRequest   = errors.New("工作区配置无效")
	ErrDisabled         = errors.New("工作区已禁用")
	ErrUnsupported      = errors.New("该工作区类型尚未支持")
	ErrOperationState   = errors.New("工作区操作状态无效")
	ErrHasConversations = errors.New("工作区仍有对话，不能删除")
)

type Type string

const (
	TypeLocal  Type = "local"
	TypeRemote Type = "remote"
	// TypeSSH 是旧版把连接参数内嵌到项目中的兼容类型；新项目使用 TypeRemote。
	TypeSSH    Type = "ssh"
	TypeWorker Type = "worker"
)

type Status string

const (
	StatusConfigured Status = "configured"
	StatusReady      Status = "ready"
	StatusError      Status = "error"
)

type AuthType string

const (
	AuthAgent    AuthType = "agent"
	AuthKeyFile  AuthType = "key-file"
	AuthPassword AuthType = "password"
)

// Workspace 是用户明确创建的项目工作区。RootPath 绝不由程序自动推断。
type Workspace struct {
	ID                 string     `json:"id"`
	Name               string     `json:"name"`
	Type               Type       `json:"type"`
	RootPath           string     `json:"root_path"`
	RemoteTargetID     string     `json:"remote_target_id,omitempty"`
	Host               string     `json:"host,omitempty"`
	Port               int        `json:"port,omitempty"`
	User               string     `json:"user,omitempty"`
	AuthType           AuthType   `json:"auth_type,omitempty"`
	KeyPath            string     `json:"key_path,omitempty"`
	Password           string     `json:"-"`
	PasswordConfigured bool       `json:"password_configured,omitempty"`
	HostKeyFingerprint string     `json:"host_key_fingerprint,omitempty"`
	Enabled            bool       `json:"enabled"`
	Status             Status     `json:"status"`
	StatusMessage      string     `json:"status_message,omitempty"`
	LastCheckedAt      *time.Time `json:"last_checked_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// SaveRequest 使用指针接收密码，以区分“保留旧密码”和“清空密码”。
type SaveRequest struct {
	Workspace Workspace
	Password  *string
}

type TestResult struct {
	OK          bool   `json:"ok"`
	Message     string `json:"message"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

type FileEntry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// FileStat 是单个项目路径的元信息；正文仍由 ReadFile 单独读取，避免状态接口意外返回大文件。
type FileStat struct {
	Path    string    `json:"path"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode,omitempty"`
	ModTime time.Time `json:"mod_time"`
}

// DirectoryEntry 是项目创建向导中的可选目录，只返回名称和规范化后的路径。
type DirectoryEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
}

// DirectoryListing 描述本机目录选择器当前所在位置。
type DirectoryListing struct {
	Path        string           `json:"path"`
	Parent      string           `json:"parent"`
	Directories []DirectoryEntry `json:"directories"`
}

type SearchMatch struct {
	Path          string              `json:"path"`
	Line          int                 `json:"line"`
	Preview       string              `json:"preview"`
	ContextBefore []SearchContextLine `json:"context_before,omitempty"`
	ContextAfter  []SearchContextLine `json:"context_after,omitempty"`
}

// SearchContextLine is a bounded neighbouring line included when a caller
// asks for search context. It deliberately carries the line number so clients
// do not need to guess offsets when adjacent matches overlap.
type SearchContextLine struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

// SearchOptions controls the bounded workspace search surface. The zero value
// keeps the historical behaviour: literal, case-sensitive search, no context,
// and the service-wide maximum hit count.
type SearchOptions struct {
	Mode            SearchMode `json:"mode,omitempty"`
	CaseInsensitive bool       `json:"case_insensitive,omitempty"`
	Glob            string     `json:"glob,omitempty"`
	ContextLines    int        `json:"context_lines,omitempty"`
	MaxHits         int        `json:"max_hits,omitempty"`
}

// SearchMode controls how the query is interpreted. Literal is the backwards
// compatible default; Regex uses the Go/RE2 syntax with linear-time matching.
// The same bounded query is validated before either local or SSH execution so
// a remote search cannot bypass the request limits.
type SearchMode string

const (
	SearchModeLiteral SearchMode = "literal"
	SearchModeRegex   SearchMode = "regex"
)

func (m SearchMode) valid() bool {
	return m == "" || m == SearchModeLiteral || m == SearchModeRegex
}

// PathStatus is the repository metadata captured before an Agent Invocation
// starts. It intentionally contains no file contents.
type PathStatus struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// WorktreeBaseline is an admission-time repository snapshot. Runtime wraps
// it with Invocation metadata before persisting the immutable audit record.
type WorktreeBaseline struct {
	RepositoryType string       `json:"repository_type"`
	HeadRevision   string       `json:"head_revision,omitempty"`
	Branch         string       `json:"branch,omitempty"`
	StatusDigest   string       `json:"status_digest"`
	ChangedPaths   []PathStatus `json:"changed_paths,omitempty"`
	StatusKnown    bool         `json:"status_known"`
	Truncated      bool         `json:"truncated,omitempty"`
	CapturedAt     time.Time    `json:"captured_at"`
}

// ReadRangeResult is a bounded, line-addressable file slice. ContentDigest
// always identifies the complete file that was read; it is therefore useful
// as a lightweight precondition when a later edit is prepared.
type ReadRangeResult struct {
	Path               string `json:"path"`
	StartLine          int    `json:"start_line"`
	EndLine            int    `json:"end_line"`
	TotalLines         int    `json:"total_lines"`
	IncludeLineNumbers bool   `json:"include_line_numbers"`
	ContentDigest      string `json:"content_digest"`
	Content            string `json:"content"`
	Truncated          bool   `json:"truncated,omitempty"`
}

// ReadManyFile keeps errors local to one requested path. A missing or binary
// file must not discard other successful reads from the same batch.
type ReadManyFile struct {
	Path          string `json:"path"`
	Content       string `json:"content,omitempty"`
	ContentDigest string `json:"content_digest,omitempty"`
	Bytes         int64  `json:"bytes,omitempty"`
	TotalLines    int    `json:"total_lines,omitempty"`
	Truncated     bool   `json:"truncated,omitempty"`
	Error         string `json:"error,omitempty"`
}

// ReadManyResult is bounded both per file and across the batch. Files retain
// request order so model context assembly remains deterministic.
type ReadManyResult struct {
	Files      []ReadManyFile `json:"files"`
	TotalBytes int64          `json:"total_bytes"`
	Truncated  bool           `json:"truncated,omitempty"`
}

// GlobResult contains deterministic, workspace-relative matches. Metadata is
// best-effort for remote workspaces because portable remote find lacks one
// stable stat format; paths and directory flags remain authoritative.
type GlobResult struct {
	Matches   []FileEntry `json:"matches"`
	Truncated bool        `json:"truncated,omitempty"`
}

type CommandResult struct {
	Output string `json:"output"`
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	// Terminal is the merged byte stream produced by a PTY-backed command.
	// Stdout/Stderr remain separate only for pipe-backed executions; a PTY
	// deliberately does not pretend that the streams can be reconstructed.
	Terminal      string `json:"terminal,omitempty"`
	ExitCode      int    `json:"exit_code"`
	Truncated     bool   `json:"truncated"`
	TimedOut      bool   `json:"timed_out,omitempty"`
	Unknown       bool   `json:"unknown,omitempty"`
	StartFailed   bool   `json:"start_failed,omitempty"`
	Started       bool   `json:"started,omitempty"`
	DurationMS    int64  `json:"duration_ms,omitempty"`
	StdoutBytes   int64  `json:"stdout_bytes,omitempty"`
	StderrBytes   int64  `json:"stderr_bytes,omitempty"`
	TerminalBytes int64  `json:"terminal_bytes,omitempty"`
}

// TTYSpec is an explicit request for a local pseudo-terminal. Presence of a
// spec is part of the immutable Operation/CommandRun admission record; a
// command cannot acquire a PTY implicitly during approval.
type TTYSpec struct {
	Enabled bool   `json:"enabled"`
	Term    string `json:"term,omitempty"`
	Rows    uint16 `json:"rows"`
	Cols    uint16 `json:"cols"`
}

const (
	defaultTTYTerm  = "xterm-256color"
	defaultTTYRows  = 24
	defaultTTYCols  = 80
	maxTTYDimension = 500
)

func (spec *TTYSpec) normalize() {
	if spec == nil {
		return
	}
	if strings.TrimSpace(spec.Term) == "" {
		spec.Term = defaultTTYTerm
	} else {
		spec.Term = strings.TrimSpace(spec.Term)
	}
	if spec.Rows == 0 {
		spec.Rows = defaultTTYRows
	}
	if spec.Cols == 0 {
		spec.Cols = defaultTTYCols
	}
}

func (spec *TTYSpec) Validate() error {
	if spec == nil {
		return nil
	}
	if !spec.Enabled {
		return fmt.Errorf("%w: TTY spec 必须显式 enabled", ErrInvalidRequest)
	}
	spec.normalize()
	if spec.Rows < 1 || spec.Rows > maxTTYDimension || spec.Cols < 1 || spec.Cols > maxTTYDimension {
		return fmt.Errorf("%w: TTY rows/cols 必须在 1 到 %d 之间", ErrInvalidRequest, maxTTYDimension)
	}
	if len(spec.Term) > 64 || strings.ContainsAny(spec.Term, "\r\n\x00") {
		return fmt.Errorf("%w: TTY term 无效", ErrInvalidRequest)
	}
	return nil
}

func EqualTTYSpec(left, right *TTYSpec) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

// CommandOutputChunk is the durable form of one observed stdout/stderr
// segment. The data is intentionally bounded by the transport reader; the
// command result still keeps only metadata and a bounded summary.
type CommandOutputChunk struct {
	CommandRunID string    `json:"command_run_id"`
	Sequence     uint64    `json:"sequence"`
	Stream       string    `json:"stream"`
	Offset       int64     `json:"offset"`
	Data         string    `json:"data"`
	ByteLength   int64     `json:"byte_length"`
	CapturedAt   time.Time `json:"captured_at"`
	Truncated    bool      `json:"truncated,omitempty"`
}

// CommandOutputPage is a sequence-based replay page. A page never uses wall
// clock timestamps as a cursor, so same-time chunks remain deterministic.
type CommandOutputPage struct {
	Chunks    []CommandOutputChunk `json:"chunks"`
	NextAfter uint64               `json:"next_after"`
	HasMore   bool                 `json:"has_more"`
	Truncated bool                 `json:"truncated,omitempty"`
	// RetentionState lets replay clients distinguish an empty output from a
	// deliberately purged log whose CommandRun metadata is still retained.
	RetentionState CommandOutputRetentionState `json:"retention_state,omitempty"`
}

// CommandOutputRetentionState describes the lifecycle of replayable command
// output independently from the CommandRun process lifecycle. CommandRun
// metadata is retained after output is purged so completion reports remain
// auditable without keeping unbounded log bodies forever.
type CommandOutputRetentionState string

const (
	CommandOutputRetentionPending  CommandOutputRetentionState = "pending"
	CommandOutputRetentionRetained CommandOutputRetentionState = "retained"
	CommandOutputRetentionPurged   CommandOutputRetentionState = "purged"
)

func validCommandOutputRetentionState(state CommandOutputRetentionState) bool {
	switch state {
	case "", CommandOutputRetentionPending, CommandOutputRetentionRetained, CommandOutputRetentionPurged:
		return true
	default:
		return false
	}
}

// CommandOutputRetentionPolicy controls automatic expiry of replayable
// output. A non-positive MaxAge disables automatic cleanup; the default is a
// bounded seven-day window. The policy is evaluated when a CommandRun reaches
// a terminal state, so changing it never shortens an already promised lease.
type CommandOutputRetentionPolicy struct {
	MaxAge time.Duration `json:"max_age"`
}

const DefaultCommandOutputRetentionAge = 7 * 24 * time.Hour

func DefaultCommandOutputRetentionPolicy() CommandOutputRetentionPolicy {
	return CommandOutputRetentionPolicy{MaxAge: DefaultCommandOutputRetentionAge}
}

// CommandOutputPurgeResult is metadata-only cleanup accounting. It never
// includes command output contents.
type CommandOutputPurgeResult struct {
	Scanned int   `json:"scanned"`
	Purged  int   `json:"purged"`
	Bytes   int64 `json:"bytes"`
}

// CommandOutputDownloadFormat controls the wire representation used by the
// full-log endpoint. Text is the merged stdout/stderr byte stream in command
// sequence order; NDJSON keeps each chunk's stream, offset and timestamp so
// callers can reconstruct a richer view without loading it into the model.
type CommandOutputDownloadFormat string

const (
	CommandOutputDownloadText   CommandOutputDownloadFormat = "text"
	CommandOutputDownloadNDJSON CommandOutputDownloadFormat = "ndjson"
)

// CommandOutputDownloadResult describes the bytes emitted by a successful
// download. It intentionally reports truncation from the durable run rather
// than pretending that a bounded capture was complete.
type CommandOutputDownloadResult struct {
	Bytes     int64 `json:"bytes"`
	Chunks    int   `json:"chunks"`
	Truncated bool  `json:"truncated"`
}

const (
	maxCommandOutputChunkBytes = 64 * 1024
	maxCommandOutputPageLimit  = 200
	// A normal command capture is bounded by the collector. This second guard
	// protects a custom repository/observer from making one HTTP response
	// unbounded while still allowing a genuinely large log to be downloaded.
	maxCommandOutputDownloadBytes int64 = 64 * 1024 * 1024
)

func (chunk CommandOutputChunk) Validate() error {
	if strings.TrimSpace(chunk.CommandRunID) == "" || chunk.Sequence == 0 {
		return fmt.Errorf("%w: command output chunk 缺少 command_run_id 或 sequence", ErrOperationState)
	}
	switch chunk.Stream {
	case "stdout", "stderr", "terminal", "system":
	default:
		return fmt.Errorf("%w: command output stream 无效", ErrOperationState)
	}
	if chunk.Offset < 0 || chunk.ByteLength < 0 || chunk.ByteLength != int64(len([]byte(chunk.Data))) {
		return fmt.Errorf("%w: command output offset/byte_length 无效", ErrOperationState)
	}
	if len([]byte(chunk.Data)) > maxCommandOutputChunkBytes {
		return fmt.Errorf("%w: command output chunk 过大", ErrOperationState)
	}
	if chunk.CapturedAt.IsZero() {
		return fmt.Errorf("%w: command output captured_at 不能为空", ErrOperationState)
	}
	return nil
}

type OperationType string
type OperationStatus string

const (
	OperationWriteFile     OperationType = "write_file"
	OperationMakeDirectory OperationType = "make_directory"
	OperationPatchFile     OperationType = "patch_file"
	OperationPatchSet      OperationType = "patch_set"
	OperationDeletePath    OperationType = "delete_path"
	OperationCommand       OperationType = "execute_command"

	// OperationPending is retained for operations written by the legacy HTTP
	// API. New Runtime operations start at prepared.
	OperationPending   OperationStatus = "pending"
	OperationPrepared  OperationStatus = "prepared"
	OperationApproved  OperationStatus = "approved"
	OperationRunning   OperationStatus = "running"
	OperationCompleted OperationStatus = "completed"
	OperationRejected  OperationStatus = "rejected"
	OperationCancelled OperationStatus = "cancelled"
	OperationExpired   OperationStatus = "expired"
	OperationStale     OperationStatus = "stale"
	OperationUnknown   OperationStatus = "unknown"
	OperationFailed    OperationStatus = "failed"
)

func (s OperationStatus) Terminal() bool {
	switch s {
	case OperationCompleted, OperationFailed, OperationRejected, OperationCancelled, OperationExpired, OperationStale, OperationUnknown:
		return true
	default:
		return false
	}
}

// Operation 保存需要用户确认的文件变更或命令。正文和精确补丁片段只在仓储内部保存，不进入公开 JSON。
type Operation struct {
	ID             string                `json:"id"`
	UserID         string                `json:"-"`
	WorkspaceID    string                `json:"workspace_id"`
	ConversationID string                `json:"conversation_id,omitempty"`
	InvocationID   string                `json:"invocation_id,omitempty"`
	ToolCallID     string                `json:"tool_call_id,omitempty"`
	CommandRunID   string                `json:"command_run_id,omitempty"`
	Type           OperationType         `json:"type"`
	Path           string                `json:"path,omitempty"`
	Content        string                `json:"-"`
	PatchOld       string                `json:"-"`
	PatchNew       string                `json:"-"`
	Patches        []FilePatch           `json:"-"`
	ExpectedDigest string                `json:"expected_digest,omitempty"`
	Command        string                `json:"command,omitempty"`
	CWD            string                `json:"cwd,omitempty"`
	Timeout        int                   `json:"timeout_seconds,omitempty"`
	TTY            *TTYSpec              `json:"tty,omitempty"`
	Preview        string                `json:"preview,omitempty"`
	Diff           string                `json:"diff,omitempty"`
	DiffArtifact   *artifact.ArtifactRef `json:"diff_artifact,omitempty"`
	OutputArtifact *artifact.ArtifactRef `json:"output_artifact,omitempty"`
	ArtifactError  string                `json:"artifact_error,omitempty"`
	Status         OperationStatus       `json:"status"`
	Result         string                `json:"result,omitempty"`
	Error          string                `json:"error,omitempty"`
	// Command outcome metadata is populated only after an execute_command
	// operation reaches a terminal state. Keeping the exit code as a pointer
	// distinguishes a real zero exit code from a non-command/unknown outcome.
	ExitCode        *int      `json:"exit_code,omitempty"`
	CommandOutcome  string    `json:"command_outcome,omitempty"`
	OutputDigest    string    `json:"output_digest,omitempty"`
	OutputTruncated bool      `json:"output_truncated,omitempty"`
	DurationMS      int64     `json:"duration_ms,omitempty"`
	TimedOut        bool      `json:"timed_out,omitempty"`
	Unknown         bool      `json:"unknown,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// CommandRunStatus is the independent lifecycle of one real command
// execution attempt. It deliberately does not reuse OperationStatus: an
// approved Operation can outlive a process attempt and a lost process handle
// must never be mistaken for a retryable pending approval.
type CommandRunStatus string

const (
	CommandRunQueued      CommandRunStatus = "queued"
	CommandRunStarting    CommandRunStatus = "starting"
	CommandRunRunning     CommandRunStatus = "running"
	CommandRunExited      CommandRunStatus = "exited"
	CommandRunTerminated  CommandRunStatus = "terminated"
	CommandRunStartFailed CommandRunStatus = "start_failed"
	CommandRunCancelled   CommandRunStatus = "cancelled"
	CommandRunUnknown     CommandRunStatus = "unknown"
)

func (s CommandRunStatus) Terminal() bool {
	switch s {
	case CommandRunExited, CommandRunTerminated, CommandRunStartFailed, CommandRunCancelled, CommandRunUnknown:
		return true
	default:
		return false
	}
}

// CommandRunOutcome separates process lifecycle from the result. In
// particular, a non-zero exit is a normal exited process with outcome
// nonzero, while an unknown outcome means the side-effect boundary cannot be
// proven after a disconnect, timeout, or lost control handle.
type CommandRunOutcome string

const (
	CommandOutcomeSuccess      CommandRunOutcome = "success"
	CommandOutcomeNonzero      CommandRunOutcome = "nonzero"
	CommandOutcomeSignaled     CommandRunOutcome = "signaled"
	CommandOutcomeCancelled    CommandRunOutcome = "cancelled"
	CommandOutcomeTimedOut     CommandRunOutcome = "timed_out"
	CommandOutcomeRuntimeError CommandRunOutcome = "runtime_error"
	CommandOutcomeUnknown      CommandRunOutcome = "unknown"
)

// ExecutionCapabilityState is intentionally per-capability rather than one
// sandboxed boolean.  A Workspace path boundary, for example, can be useful
// for file tools while a shell process still has access to the host user's
// other resources.  Callers must therefore disclose partial/unsupported
// guarantees instead of implying OS isolation from the executor kind alone.
type ExecutionCapabilityState string

const (
	ExecutionCapabilitySupported   ExecutionCapabilityState = "supported"
	ExecutionCapabilityPartial     ExecutionCapabilityState = "partial"
	ExecutionCapabilityUnsupported ExecutionCapabilityState = "unsupported"
	ExecutionCapabilityUnknown     ExecutionCapabilityState = "unknown"
)

// ExecutionCapability describes one independently evaluated execution
// guarantee. Detail is bounded, metadata-only text suitable for an approval
// preview and does not contain command arguments or environment values.
type ExecutionCapability struct {
	State  ExecutionCapabilityState `json:"state"`
	Detail string                   `json:"detail,omitempty"`
}

// CommandExecutionCapabilities is an immutable-at-admission disclosure for
// one CommandRun. It describes the guarantees of the selected executor; it
// does not claim that the command itself is safe. The current local executor
// is deliberately represented as L0 host-process execution.
type CommandExecutionCapabilities struct {
	ProfileID      string              `json:"profile_id"`
	ProfileVersion string              `json:"profile_version"`
	Executor       string              `json:"executor"`
	IsolationLevel string              `json:"isolation_level"`
	Filesystem     ExecutionCapability `json:"filesystem"`
	Network        ExecutionCapability `json:"network"`
	ProcessControl ExecutionCapability `json:"process_control"`
	Resources      ExecutionCapability `json:"resources"`
	Credentials    ExecutionCapability `json:"credentials"`
	PTY            ExecutionCapability `json:"pty"`
	Reattach       ExecutionCapability `json:"reattach"`
}

// EqualCommandExecutionCapabilities is used by persistence adapters to keep
// the admission snapshot immutable across later CAS transitions. A missing
// legacy snapshot is intentionally different from a concrete profile.
func EqualCommandExecutionCapabilities(left, right *CommandExecutionCapabilities) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func validExecutionCapabilityState(state ExecutionCapabilityState) bool {
	switch state {
	case ExecutionCapabilitySupported, ExecutionCapabilityPartial, ExecutionCapabilityUnsupported, ExecutionCapabilityUnknown:
		return true
	default:
		return false
	}
}

func (capability ExecutionCapability) Validate() error {
	if !validExecutionCapabilityState(capability.State) {
		return fmt.Errorf("%w: execution capability state %q 无效", ErrOperationState, capability.State)
	}
	if len([]byte(capability.Detail)) > 1000 {
		return fmt.Errorf("%w: execution capability detail 过长", ErrOperationState)
	}
	return nil
}

func (capabilities CommandExecutionCapabilities) Validate() error {
	if strings.TrimSpace(capabilities.ProfileID) == "" || strings.TrimSpace(capabilities.ProfileVersion) == "" || strings.TrimSpace(capabilities.Executor) == "" || strings.TrimSpace(capabilities.IsolationLevel) == "" {
		return fmt.Errorf("%w: execution capability profile 字段不能为空", ErrOperationState)
	}
	if len(capabilities.ProfileID) > 128 || len(capabilities.ProfileVersion) > 32 || len(capabilities.Executor) > 32 || len(capabilities.IsolationLevel) > 64 {
		return fmt.Errorf("%w: execution capability profile 字段过长", ErrOperationState)
	}
	for name, capability := range map[string]ExecutionCapability{
		"filesystem": capabilities.Filesystem, "network": capabilities.Network,
		"process_control": capabilities.ProcessControl, "resources": capabilities.Resources,
		"credentials": capabilities.Credentials, "pty": capabilities.PTY, "reattach": capabilities.Reattach,
	} {
		if err := capability.Validate(); err != nil {
			return fmt.Errorf("%w: %s", err, name)
		}
	}
	return nil
}

func executionCapability(state ExecutionCapabilityState, detail string) ExecutionCapability {
	return ExecutionCapability{State: state, Detail: detail}
}

// DefaultCommandExecutionCapabilities returns the profile captured for a new
// command. TypeRemote has already been resolved to TypeSSH at this boundary;
// an unknown type is fail-closed and never presented as isolated.
func DefaultCommandExecutionCapabilities(item Workspace) *CommandExecutionCapabilities {
	capabilities := &CommandExecutionCapabilities{
		ProfileID:      "unknown-executor",
		ProfileVersion: "1",
		Executor:       "unknown",
		IsolationLevel: "unknown",
		Filesystem:     executionCapability(ExecutionCapabilityUnknown, "未能验证执行器的文件系统边界"),
		Network:        executionCapability(ExecutionCapabilityUnknown, "未能验证执行器的网络出口限制"),
		ProcessControl: executionCapability(ExecutionCapabilityUnknown, "未能验证进程组和终止能力"),
		Resources:      executionCapability(ExecutionCapabilityUnknown, "未能验证 CPU、内存、磁盘和进程数量限制"),
		Credentials:    executionCapability(ExecutionCapabilityUnknown, "未能验证凭据注入和环境隔离"),
		PTY:            executionCapability(ExecutionCapabilityUnknown, "未能验证 PTY 能力"),
		Reattach:       executionCapability(ExecutionCapabilityUnknown, "未能验证服务重启后的进程接管能力"),
	}
	switch item.Type {
	case TypeLocal:
		capabilities.ProfileID = "host-process-l0"
		capabilities.Executor = "local"
		capabilities.IsolationLevel = "l0_host_process"
		capabilities.Filesystem = executionCapability(ExecutionCapabilityPartial, "文件工具和 CWD 受工作区边界约束；shell 仍可能访问当前用户可访问的其他路径")
		capabilities.Network = executionCapability(ExecutionCapabilityUnsupported, "未施加操作系统级网络出口限制")
		capabilities.ProcessControl = executionCapability(ExecutionCapabilityPartial, "使用本地进程组和 wall timeout；无法对后台后代提供强隔离保证")
		capabilities.Resources = executionCapability(ExecutionCapabilityPartial, "仅提供 wall timeout 和有界输出；未强制 CPU、内存、磁盘或进程数配额")
		capabilities.Credentials = executionCapability(ExecutionCapabilityUnsupported, "没有 secret broker；命令在当前 OS 用户上下文运行，HOME 可能指向宿主路径")
		capabilities.PTY = executionCapability(ExecutionCapabilityUnsupported, "当前 CommandRun 使用非交互 pipe；PTY 尚未实现")
		capabilities.Reattach = executionCapability(ExecutionCapabilityUnsupported, "服务重启后控制句柄不会接管旧进程，运行记录会保守标记 unknown")
	case TypeSSH, TypeRemote:
		capabilities.ProfileID = "ssh-host-process"
		capabilities.Executor = "ssh"
		capabilities.IsolationLevel = "ssh_account"
		capabilities.Filesystem = executionCapability(ExecutionCapabilityPartial, "工作区文件工具和 CWD wrapper 受边界约束；SSH shell 仍继承远端账号权限")
		capabilities.Network = executionCapability(ExecutionCapabilityUnsupported, "未对远端账号施加操作系统级网络出口限制")
		capabilities.ProcessControl = executionCapability(ExecutionCapabilityPartial, "尝试发送远程终止信号；断线或目标不支持时结果会保守标记 unknown")
		capabilities.Resources = executionCapability(ExecutionCapabilityPartial, "仅提供 wall timeout 和有界输出；远端 CPU、内存、磁盘或进程数未由 Abot 强制")
		capabilities.Credentials = executionCapability(ExecutionCapabilityUnsupported, "命令在远端 SSH 账号上下文运行，没有 Abot secret broker")
		capabilities.PTY = executionCapability(ExecutionCapabilityUnsupported, "当前 CommandRun 使用非交互 SSH session；PTY 尚未实现")
		capabilities.Reattach = executionCapability(ExecutionCapabilityUnsupported, "SSH session 断开后没有 Worker reattach，无法安全承诺进程接管")
	}
	return capabilities
}

// CommandRun is a durable checkpoint for one approved command process. It
// contains metadata and bounded output counters; live chunks continue through
// the Invocation AgentEvent stream and may also be replayed from the optional
// CommandOutputRepository.
type CommandRun struct {
	ID                   string                        `json:"id"`
	UserID               string                        `json:"-"`
	WorkspaceID          string                        `json:"workspace_id"`
	ConversationID       string                        `json:"conversation_id,omitempty"`
	InvocationID         string                        `json:"invocation_id,omitempty"`
	ToolCallID           string                        `json:"tool_call_id,omitempty"`
	OperationID          string                        `json:"operation_id"`
	Executor             string                        `json:"executor"`
	Mode                 string                        `json:"mode"`
	Capabilities         *CommandExecutionCapabilities `json:"capabilities,omitempty"`
	CommandPreview       string                        `json:"command_preview,omitempty"`
	CWD                  string                        `json:"cwd,omitempty"`
	TimeoutMS            int64                         `json:"timeout_ms,omitempty"`
	TTY                  *TTYSpec                      `json:"tty,omitempty"`
	Status               CommandRunStatus              `json:"status"`
	Outcome              CommandRunOutcome             `json:"outcome,omitempty"`
	ExitCode             *int                          `json:"exit_code,omitempty"`
	StdoutBytes          int64                         `json:"stdout_bytes,omitempty"`
	StderrBytes          int64                         `json:"stderr_bytes,omitempty"`
	StoredBytes          int64                         `json:"stored_bytes,omitempty"`
	OutputDigest         string                        `json:"output_digest,omitempty"`
	OutputArtifact       *artifact.ArtifactRef         `json:"output_artifact,omitempty"`
	ArtifactError        string                        `json:"artifact_error,omitempty"`
	OutputTruncated      bool                          `json:"output_truncated,omitempty"`
	OutputRetentionState CommandOutputRetentionState   `json:"output_retention_state,omitempty"`
	OutputRetainedUntil  *time.Time                    `json:"output_retained_until,omitempty"`
	OutputPurgedAt       *time.Time                    `json:"output_purged_at,omitempty"`
	Error                string                        `json:"error,omitempty"`
	Revision             int64                         `json:"revision"`
	QueuedAt             time.Time                     `json:"queued_at"`
	StartedAt            *time.Time                    `json:"started_at,omitempty"`
	FinishedAt           *time.Time                    `json:"finished_at,omitempty"`
	CreatedAt            time.Time                     `json:"created_at"`
	UpdatedAt            time.Time                     `json:"updated_at"`
}

var (
	ErrCommandRunConflict            = errors.New("命令运行状态冲突")
	ErrCommandOutputDownloadTooLarge = errors.New("命令输出下载超过上限")
	ErrCommandOutputExpired          = errors.New("命令输出已过保留期")
)

func validCommandRunStatus(status CommandRunStatus) bool {
	switch status {
	case CommandRunQueued, CommandRunStarting, CommandRunRunning, CommandRunExited, CommandRunTerminated, CommandRunStartFailed, CommandRunCancelled, CommandRunUnknown:
		return true
	default:
		return false
	}
}

func validCommandRunOutcome(outcome CommandRunOutcome) bool {
	switch outcome {
	case "", CommandOutcomeSuccess, CommandOutcomeNonzero, CommandOutcomeSignaled, CommandOutcomeCancelled, CommandOutcomeTimedOut, CommandOutcomeRuntimeError, CommandOutcomeUnknown:
		return true
	default:
		return false
	}
}

func (r CommandRun) Validate() error {
	if strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.WorkspaceID) == "" || strings.TrimSpace(r.OperationID) == "" {
		return fmt.Errorf("%w: id、workspace_id 和 operation_id 不能为空", ErrOperationState)
	}
	if !validCommandRunStatus(r.Status) {
		return fmt.Errorf("%w: command run status %q 无效", ErrOperationState, r.Status)
	}
	if !validCommandRunOutcome(r.Outcome) {
		return fmt.Errorf("%w: command run outcome %q 无效", ErrOperationState, r.Outcome)
	}
	if !validCommandOutputRetentionState(r.OutputRetentionState) {
		return fmt.Errorf("%w: command output retention state %q 无效", ErrOperationState, r.OutputRetentionState)
	}
	if r.Status.Terminal() && r.Outcome == "" {
		return fmt.Errorf("%w: 终态 command run 必须记录 outcome", ErrOperationState)
	}
	if r.Revision <= 0 {
		return fmt.Errorf("%w: command run revision 必须为正数", ErrOperationState)
	}
	if r.TimeoutMS < 0 || r.StdoutBytes < 0 || r.StderrBytes < 0 || r.StoredBytes < 0 {
		return fmt.Errorf("%w: command run 数值字段不能为负数", ErrOperationState)
	}
	if r.ExitCode != nil && (*r.ExitCode < -1 || *r.ExitCode > 255) {
		return fmt.Errorf("%w: command run exit_code 超出范围", ErrOperationState)
	}
	if len(r.CommandPreview) > 4096 || len(r.CWD) > 2000 || len(r.Error) > 4096 || len(r.ArtifactError) > 512 || len(r.OutputDigest) > 256 {
		return fmt.Errorf("%w: command run 文本字段过长", ErrOperationState)
	}
	if r.TTY != nil {
		copySpec := *r.TTY
		if err := copySpec.Validate(); err != nil {
			return err
		}
	}
	if r.Capabilities != nil {
		if err := r.Capabilities.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// CommandRunRepository is optional to preserve compatibility with old
// workspace embedders. Production SQLite and test repositories implement the
// CAS update methods; without it, Operation remains the legacy checkpoint.
type CommandRunRepository interface {
	CreateCommandRun(context.Context, CommandRun) error
	GetCommandRun(context.Context, string) (CommandRun, error)
	ListCommandRuns(context.Context, string) ([]CommandRun, error)
	UpdateCommandRun(context.Context, CommandRun, int64) (CommandRun, error)
	DeleteCommandRun(context.Context, string) error
}

// CommandOutputRepository is an optional second-level store for replayable
// command output. Invocation AgentEvent remains the live notification path;
// this repository allows large output to be fetched by CommandRun sequence
// without putting the whole log into a single event or Operation row.
type CommandOutputRepository interface {
	CreateCommandOutputChunk(context.Context, CommandOutputChunk) error
	ListCommandOutputChunks(context.Context, string, uint64, int) ([]CommandOutputChunk, error)
	DeleteCommandOutputChunks(context.Context, string) error
}

// CommandOutputRetentionRepository atomically removes replayable chunks and
// advances the CommandRun retention state. The expected revision prevents a
// cleanup worker from racing a terminal checkpoint update or another cleanup
// worker and leaving metadata that disagrees with the stored body.
type CommandOutputRetentionRepository interface {
	PurgeCommandOutput(context.Context, string, int64, CommandRun) (CommandRun, error)
}

type OperationRequest struct {
	WorkspaceID string `json:"workspace_id"`
	// UserID is installed by the authenticated HTTP boundary or Runtime
	// context. It is never accepted from model tool arguments and is used only
	// for ownership of derived Artifact projections.
	UserID         string        `json:"-"`
	ConversationID string        `json:"conversation_id,omitempty"`
	InvocationID   string        `json:"invocation_id,omitempty"`
	ToolCallID     string        `json:"tool_call_id,omitempty"`
	Type           OperationType `json:"type"`
	Path           string        `json:"path,omitempty"`
	Content        string        `json:"content,omitempty"`
	OldText        string        `json:"old_text,omitempty"`
	NewText        string        `json:"new_text,omitempty"`
	Patches        []FilePatch   `json:"patches,omitempty"`
	// ExpectedDigest is an optional caller-supplied CAS precondition. When it
	// is omitted, CreateOperation snapshots the current file digest and the
	// approval path checks that snapshot again before applying the change.
	ExpectedDigest string   `json:"expected_digest,omitempty"`
	Command        string   `json:"command,omitempty"`
	CWD            string   `json:"cwd,omitempty"`
	Timeout        int      `json:"timeout_seconds,omitempty"`
	TTY            *TTYSpec `json:"tty,omitempty"`
}

// FilePatch is one exact text replacement inside a ChangeSet. ExpectedDigest
// is filled by the host when omitted and checked again at approval time.
type FilePatch struct {
	Path           string `json:"path"`
	OldText        string `json:"old_text"`
	NewText        string `json:"new_text"`
	ExpectedDigest string `json:"expected_digest,omitempty"`
}

type Repository interface {
	List(context.Context) ([]Workspace, error)
	Get(context.Context, string) (Workspace, error)
	Save(context.Context, Workspace) error
	Delete(context.Context, string) error
	ListOperations(context.Context, string) ([]Operation, error)
	GetOperation(context.Context, string) (Operation, error)
	SaveOperation(context.Context, Operation) error
}

// ConversationCounter 用于阻止删除仍包含对话的工作区。
type ConversationCounter func(context.Context, string) (int, error)

// ConversationChecker 用于阻止已归档对话继续批准工作区操作。
type ConversationChecker func(context.Context, string) (bool, error)

// InstructionWriteValidator is a read-only last-mile guard for operations
// created by Agent Runtime. It must re-read the Invocation's project
// instruction snapshot and return an error when the snapshot is stale.
type InstructionWriteValidator func(context.Context, string) error

// OperationAdmissionValidator is a read-only workflow gate invoked before a
// Runtime-bound operation is prepared and again immediately before it is
// approved. It may inspect durable Runtime state, but must never execute a
// command or mutate the workspace. Operations without an Invocation ID do
// not invoke this validator and retain the legacy HTTP behavior.
type OperationAdmissionValidator func(context.Context, OperationRequest) error

// OperationObserver receives the final metadata record after an approved
// operation has been persisted. It is intentionally a notification hook: the
// observer must not execute another side effect or mutate the Operation. The
// callback is used by Agent Runtime to project command results into durable
// VerificationRun evidence without coupling this package to Runtime.
type OperationObserver func(context.Context, Operation)

// CommandRunObserver receives every durable CommandRun checkpoint after it is
// persisted. It is metadata-only and must never execute or retry a command.
type CommandRunObserver func(context.Context, CommandRun)

// ArtifactWriter is an optional projection hook. Workspace supplies bounded
// command/diff bytes; the application decides where to store them and returns
// only a safe metadata ref. It must not mutate the workspace or rerun a
// command.
type ArtifactWriter func(context.Context, artifact.PutRequest, io.Reader) (artifact.Artifact, error)

// Service 统一管理工作区配置、传输和待审批操作。
type Service struct {
	repository               Repository
	operationMu              sync.Mutex
	commandRunMu             sync.RWMutex
	commandRunCancels        map[string]context.CancelFunc
	ptyMu                    sync.RWMutex
	ptySessions              map[string]commandPTYHandle
	ptyWriterLeases          map[string]*ptyWriterLease
	ptySubscribers           map[string]map[chan CommandChunk]struct{}
	commandRunObserverMu     sync.RWMutex
	conversationCounter      ConversationCounter
	conversationChecker      ConversationChecker
	instructionValidator     InstructionWriteValidator
	operationAdmission       OperationAdmissionValidator
	operationObserver        OperationObserver
	commandRunObserver       CommandRunObserver
	artifactWriterMu         sync.RWMutex
	artifactWriter           ArtifactWriter
	remoteTargetResolver     RemoteTargetResolver
	commandOutputRetentionMu sync.RWMutex
	commandOutputRetention   CommandOutputRetentionPolicy
}

// RemoteTargetResolver 让工作区只保存远程主机引用，具体凭据由远程主机服务解析。
type RemoteTargetResolver func(context.Context, string) (RemoteTarget, error)

func NewService(repository Repository) (*Service, error) {
	if repository == nil {
		return nil, errors.New("工作区 Repository 不能为空")
	}
	return &Service{repository: repository, commandRunCancels: make(map[string]context.CancelFunc), ptySessions: make(map[string]commandPTYHandle), ptyWriterLeases: make(map[string]*ptyWriterLease), ptySubscribers: make(map[string]map[chan CommandChunk]struct{}), commandOutputRetention: DefaultCommandOutputRetentionPolicy()}, nil
}

// SetCommandOutputRetention changes the cleanup policy for newly completed
// CommandRuns. Existing runs keep the deadline captured at their terminal
// checkpoint, which makes retention auditable and avoids surprising users by
// shortening a previously advertised download window.
func (s *Service) SetCommandOutputRetention(policy CommandOutputRetentionPolicy) error {
	if s == nil {
		return errors.New("工作区 Service 不能为空")
	}
	if policy.MaxAge < 0 || policy.MaxAge > 365*24*time.Hour {
		return fmt.Errorf("%w: 日志保留时长必须在 0 到 365 天之间", ErrInvalidRequest)
	}
	s.commandOutputRetentionMu.Lock()
	s.commandOutputRetention = policy
	s.commandOutputRetentionMu.Unlock()
	return nil
}

func (s *Service) commandOutputRetentionPolicy() CommandOutputRetentionPolicy {
	if s == nil {
		return DefaultCommandOutputRetentionPolicy()
	}
	s.commandOutputRetentionMu.RLock()
	policy := s.commandOutputRetention
	s.commandOutputRetentionMu.RUnlock()
	return policy
}

// SetInstructionWriteValidator installs the final write-boundary check used
// immediately before an approved operation changes workspace state. Legacy
// operations without an Invocation ID are intentionally left untouched.
func (s *Service) SetInstructionWriteValidator(validator InstructionWriteValidator) {
	s.operationMu.Lock()
	s.instructionValidator = validator
	s.operationMu.Unlock()
}

// SetOperationAdmissionValidator installs the Runtime workflow gate for
// Invocation-bound operations. The validator is read under the same mutex as
// the other operation-boundary callbacks so replacing it is race-free.
func (s *Service) SetOperationAdmissionValidator(validator OperationAdmissionValidator) {
	if s == nil {
		return
	}
	s.operationMu.Lock()
	s.operationAdmission = validator
	s.operationMu.Unlock()
}

// SetOperationObserver installs a best-effort post-commit notification for
// approved operations. It is called while the operation lock is held and must
// therefore remain bounded and non-blocking; Runtime observers should only
// persist metadata and never call back into Workspace approval APIs.
func (s *Service) SetOperationObserver(observer OperationObserver) {
	s.operationMu.Lock()
	s.operationObserver = observer
	s.operationMu.Unlock()
}

// SetCommandRunObserver installs the metadata projection used by Runtime to
// publish command lifecycle events. The callback is invoked outside the
// observer mutex and may therefore perform bounded persistence.
func (s *Service) SetCommandRunObserver(observer CommandRunObserver) {
	s.commandRunObserverMu.Lock()
	s.commandRunObserver = observer
	s.commandRunObserverMu.Unlock()
}

// SetArtifactWriter installs the optional command-log/diff projection. A nil
// writer keeps legacy bounded inline summaries while preserving execution
// semantics.
func (s *Service) SetArtifactWriter(writer ArtifactWriter) {
	if s == nil {
		return
	}
	s.artifactWriterMu.Lock()
	s.artifactWriter = writer
	s.artifactWriterMu.Unlock()
}

func (s *Service) artifactWriterFunc() ArtifactWriter {
	if s == nil {
		return nil
	}
	s.artifactWriterMu.RLock()
	writer := s.artifactWriter
	s.artifactWriterMu.RUnlock()
	return writer
}

func (s *Service) notifyCommandRun(ctx context.Context, run CommandRun) {
	s.commandRunObserverMu.RLock()
	observer := s.commandRunObserver
	s.commandRunObserverMu.RUnlock()
	if observer != nil {
		observer(ctx, run)
	}
}

func (s *Service) List(ctx context.Context) ([]Workspace, error) {
	return s.repository.List(ctx)
}

func (s *Service) Get(ctx context.Context, id string) (Workspace, error) {
	return s.repository.Get(ctx, strings.TrimSpace(id))
}

// ListLocalDirectories 为项目创建向导浏览本机目录；它不创建目录，也不读取目录中的文件内容。
func (s *Service) ListLocalDirectories(ctx context.Context, requested string) (DirectoryListing, error) {
	select {
	case <-ctx.Done():
		return DirectoryListing{}, ctx.Err()
	default:
	}
	requested = strings.TrimSpace(requested)
	if requested == "" {
		requested = "."
	}
	absolute, err := filepath.Abs(requested)
	if err != nil {
		return DirectoryListing{}, fmt.Errorf("解析目录失败: %w", err)
	}
	realPath, err := filepath.EvalSymlinks(filepath.Clean(absolute))
	if err != nil {
		return DirectoryListing{}, fmt.Errorf("目录不存在或不可访问: %w", err)
	}
	info, err := os.Stat(realPath)
	if err != nil {
		return DirectoryListing{}, fmt.Errorf("读取目录失败: %w", err)
	}
	if !info.IsDir() {
		return DirectoryListing{}, fmt.Errorf("选择的路径不是目录")
	}
	entries, err := os.ReadDir(realPath)
	if err != nil {
		return DirectoryListing{}, fmt.Errorf("读取目录失败: %w", err)
	}
	listing := DirectoryListing{Path: realPath, Parent: filepath.Dir(realPath), Directories: make([]DirectoryEntry, 0, len(entries))}
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return DirectoryListing{}, ctx.Err()
		default:
		}
		if !entry.IsDir() {
			continue
		}
		entryPath := filepath.Join(realPath, entry.Name())
		resolvedPath, resolveErr := filepath.EvalSymlinks(entryPath)
		if resolveErr != nil {
			continue
		}
		entryInfo, statErr := os.Stat(resolvedPath)
		if statErr != nil || !entryInfo.IsDir() {
			continue
		}
		listing.Directories = append(listing.Directories, DirectoryEntry{Name: entry.Name(), Path: resolvedPath, IsDir: true})
	}
	return listing, nil
}

// SetConversationPolicy 注入对话生命周期检查，避免 Workspace 包依赖 Conversation 包。
func (s *Service) SetConversationPolicy(counter ConversationCounter, checker ConversationChecker) {
	s.conversationCounter = counter
	s.conversationChecker = checker
}

// SetRemoteTargetResolver 装配远程主机解析器，旧版 inline SSH 项目不依赖它。
func (s *Service) SetRemoteTargetResolver(resolver RemoteTargetResolver) {
	s.remoteTargetResolver = resolver
}

// CountByRemoteTarget 统计项目对远程主机的引用，供远程主机删除保护使用。
// 通过可选接口调用仓储，保留内存仓储和旧版嵌入方的兼容性。
func (s *Service) CountByRemoteTarget(ctx context.Context, targetID string) (int, error) {
	repository, ok := s.repository.(interface {
		CountByRemoteTarget(context.Context, string) (int, error)
	})
	if !ok {
		return 0, nil
	}
	return repository.CountByRemoteTarget(ctx, strings.TrimSpace(targetID))
}

func (s *Service) Save(ctx context.Context, request SaveRequest) (Workspace, error) {
	item := request.Workspace
	item.ID = strings.TrimSpace(item.ID)
	if item.ID == "" {
		// 项目 ID 是内部标识，不要求用户在创建项目时手填。
		item.ID = newID("workspace")
	}
	item.Name = strings.TrimSpace(item.Name)
	item.RootPath = strings.TrimSpace(item.RootPath)
	item.RemoteTargetID = strings.TrimSpace(item.RemoteTargetID)
	item.Host = strings.TrimSpace(item.Host)
	item.User = strings.TrimSpace(item.User)
	item.KeyPath = strings.TrimSpace(item.KeyPath)
	item.HostKeyFingerprint = strings.TrimSpace(item.HostKeyFingerprint)
	now := time.Now().UTC()
	old, getErr := s.repository.Get(ctx, item.ID)
	existing := getErr == nil
	switch {
	case getErr == nil:
		item.CreatedAt = old.CreatedAt
		if request.Password == nil {
			item.Password = old.Password
		}
	case errors.Is(getErr, ErrNotFound):
		item.CreatedAt = now
	default:
		return Workspace{}, getErr
	}
	if existing && (item.Type != old.Type || item.RootPath != old.RootPath || item.RemoteTargetID != old.RemoteTargetID) {
		return Workspace{}, fmt.Errorf("%w: 项目类型、目录和远程主机创建后不可修改，请新建项目", ErrInvalidRequest)
	}
	if item.Type == TypeRemote {
		if item.RemoteTargetID == "" {
			return Workspace{}, fmt.Errorf("%w: 远程项目必须选择远程主机", ErrInvalidRequest)
		}
		if s.remoteTargetResolver == nil {
			return Workspace{}, fmt.Errorf("%w: 远程主机服务尚未装配", ErrInvalidRequest)
		}
		target, targetErr := s.remoteTargetResolver(ctx, item.RemoteTargetID)
		if targetErr != nil {
			return Workspace{}, targetErr
		}
		if !target.Enabled {
			return Workspace{}, ErrRemoteTargetDisabled
		}
		if target.Status != RemoteTargetReady || strings.TrimSpace(target.HostKeyFingerprint) == "" {
			return Workspace{}, ErrRemoteTargetUnconfirmed
		}
	}
	if err := validateWorkspace(&item); err != nil {
		return Workspace{}, err
	}
	if request.Password != nil {
		item.Password = *request.Password
	}
	// 切换类型或认证方式时立即清理不再使用的秘密，避免旧凭据残留。
	if item.Type == TypeLocal || item.Type == TypeRemote {
		item.Host, item.User, item.KeyPath, item.Password, item.HostKeyFingerprint = "", "", "", "", ""
		item.Port = 0
		item.AuthType = ""
		if item.Type == TypeLocal {
			item.RemoteTargetID = ""
		}
	} else {
		if item.AuthType != AuthPassword {
			item.Password = ""
		}
		if item.AuthType != AuthKeyFile {
			item.KeyPath = ""
		}
	}
	item.PasswordConfigured = strings.TrimSpace(item.Password) != ""
	item.UpdatedAt = now
	if item.Status == "" || item.Status == StatusReady {
		item.Status = StatusConfigured
		item.StatusMessage = ""
	}
	if err := s.repository.Save(ctx, item); err != nil {
		return Workspace{}, err
	}
	return item, nil
}

func (s *Service) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if s.conversationCounter != nil {
		count, err := s.conversationCounter(ctx, id)
		if err != nil {
			return err
		}
		if count > 0 {
			return ErrHasConversations
		}
	}
	return s.repository.Delete(ctx, id)
}

// Resolve 返回可执行的工作区。未选择工作区时由调用方直接跳过，不存在隐式默认值。
func (s *Service) Resolve(ctx context.Context, id string) (Workspace, error) {
	item, err := s.Get(ctx, id)
	if err != nil {
		return Workspace{}, err
	}
	if !item.Enabled {
		return Workspace{}, ErrDisabled
	}
	if item.Type == TypeRemote {
		if s.remoteTargetResolver == nil {
			return Workspace{}, fmt.Errorf("%w: 远程主机服务尚未装配", ErrInvalidRequest)
		}
		target, targetErr := s.remoteTargetResolver(ctx, item.RemoteTargetID)
		if targetErr != nil {
			return Workspace{}, targetErr
		}
		if !target.Enabled {
			return Workspace{}, ErrRemoteTargetDisabled
		}
		if target.Status != RemoteTargetReady || strings.TrimSpace(target.HostKeyFingerprint) == "" {
			return Workspace{}, ErrRemoteTargetUnconfirmed
		}
		// 只在执行边界内合并连接参数，不把它们写回项目实体或返回 WebUI。
		item.Type = TypeSSH
		item.Host, item.Port, item.User, item.AuthType = target.Host, target.Port, target.User, target.AuthType
		item.KeyPath, item.Password, item.HostKeyFingerprint = target.KeyPath, target.Password, target.HostKeyFingerprint
	}
	if item.Type == TypeWorker {
		return Workspace{}, ErrUnsupported
	}
	return item, nil
}

func (s *Service) Test(ctx context.Context, id string) (TestResult, error) {
	stored, err := s.Get(ctx, id)
	if err != nil {
		return TestResult{}, err
	}
	item, err := s.Resolve(ctx, stored.ID)
	if err != nil {
		return TestResult{}, err
	}
	result, testErr := testWorkspace(ctx, item)
	now := time.Now().UTC()
	stored.LastCheckedAt = &now
	if testErr != nil {
		stored.Status = StatusError
		stored.StatusMessage = testErr.Error()
	} else {
		stored.Status = StatusReady
		stored.StatusMessage = result.Message
	}
	if saveErr := s.repository.Save(ctx, stored); saveErr != nil {
		// 连接测试和状态持久化必须同时成功，否则 WebUI 看到的状态会与实际运行状态分离。
		if testErr == nil {
			return TestResult{}, fmt.Errorf("保存工作区测试状态失败: %w", saveErr)
		}
		return result, errors.Join(testErr, fmt.Errorf("保存工作区测试状态失败: %w", saveErr))
	}
	return result, testErr
}

// TestPreview 测试尚未保存的表单配置，便于用户先验证目录或 SSH 连接。
func (s *Service) TestPreview(ctx context.Context, request SaveRequest) (TestResult, error) {
	item := request.Workspace
	item.ID = strings.TrimSpace(item.ID)
	if item.ID == "" {
		item.ID = "preview"
	}
	item.Name = strings.TrimSpace(item.Name)
	item.RootPath = strings.TrimSpace(item.RootPath)
	item.RemoteTargetID = strings.TrimSpace(item.RemoteTargetID)
	item.Host = strings.TrimSpace(item.Host)
	item.User = strings.TrimSpace(item.User)
	item.KeyPath = strings.TrimSpace(item.KeyPath)
	item.HostKeyFingerprint = strings.TrimSpace(item.HostKeyFingerprint)
	if request.Password != nil {
		item.Password = *request.Password
	} else if item.ID != "" {
		if old, err := s.repository.Get(ctx, item.ID); err == nil {
			item.Password = old.Password
		}
	}
	if item.Type == TypeRemote {
		if s.remoteTargetResolver == nil {
			return TestResult{}, fmt.Errorf("%w: 远程主机服务尚未装配", ErrInvalidRequest)
		}
		target, targetErr := s.remoteTargetResolver(ctx, item.RemoteTargetID)
		if targetErr != nil {
			return TestResult{}, targetErr
		}
		if !target.Enabled {
			return TestResult{}, ErrRemoteTargetDisabled
		}
		item.Type = TypeSSH
		item.Host, item.Port, item.User, item.AuthType = target.Host, target.Port, target.User, target.AuthType
		item.KeyPath, item.Password, item.HostKeyFingerprint = target.KeyPath, target.Password, target.HostKeyFingerprint
	}
	if err := validateWorkspace(&item); err != nil {
		return TestResult{}, err
	}
	return testWorkspace(ctx, item)
}

func validateWorkspace(item *Workspace) error {
	if !regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`).MatchString(item.ID) {
		return fmt.Errorf("%w: 工作区 ID 必须以小写字母开头，并且只能包含小写字母、数字、_、-", ErrInvalidRequest)
	}
	if item.Name == "" {
		return fmt.Errorf("%w: 工作区名称不能为空", ErrInvalidRequest)
	}
	if item.RootPath == "" {
		return fmt.Errorf("%w: 项目目录必须由用户指定", ErrInvalidRequest)
	}
	switch item.Type {
	case TypeLocal:
		absolute, err := filepath.Abs(item.RootPath)
		if err != nil || !filepath.IsAbs(item.RootPath) {
			return fmt.Errorf("%w: 本机项目目录必须是绝对路径", ErrInvalidRequest)
		}
		realPath, err := filepath.EvalSymlinks(filepath.Clean(absolute))
		if err != nil {
			return fmt.Errorf("%w: 本机项目目录不存在或不可访问: %v", ErrInvalidRequest, err)
		}
		info, err := os.Stat(realPath)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("%w: 本机项目路径必须是可访问的目录", ErrInvalidRequest)
		}
		item.RootPath = realPath
	case TypeRemote:
		if strings.TrimSpace(item.RemoteTargetID) == "" {
			return fmt.Errorf("%w: 远程项目必须选择远程主机", ErrInvalidRequest)
		}
		if !strings.HasPrefix(item.RootPath, "/") {
			return fmt.Errorf("%w: 远程项目目录必须是绝对路径", ErrInvalidRequest)
		}
	case TypeSSH:
		if !strings.HasPrefix(item.RootPath, "/") {
			return fmt.Errorf("%w: SSH 项目目录必须是绝对路径", ErrInvalidRequest)
		}
		if item.Host == "" || item.User == "" {
			return fmt.Errorf("%w: SSH 主机和用户不能为空", ErrInvalidRequest)
		}
		if item.Port == 0 {
			item.Port = 22
		}
		if item.Port < 1 || item.Port > 65535 {
			return fmt.Errorf("%w: SSH 端口无效", ErrInvalidRequest)
		}
		if item.AuthType == "" {
			item.AuthType = AuthAgent
		}
		if item.AuthType == AuthKeyFile && item.KeyPath == "" {
			return fmt.Errorf("%w: 密钥认证必须填写本机私钥路径", ErrInvalidRequest)
		}
		if item.AuthType != AuthAgent && item.AuthType != AuthKeyFile && item.AuthType != AuthPassword {
			return fmt.Errorf("%w: SSH 认证方式无效", ErrInvalidRequest)
		}
	case TypeWorker:
		return fmt.Errorf("%w: Worker 将在后续版本实现", ErrUnsupported)
	default:
		return fmt.Errorf("%w: 工作区类型无效", ErrInvalidRequest)
	}
	return nil
}

func newID(prefix string) string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(value[:])
}
