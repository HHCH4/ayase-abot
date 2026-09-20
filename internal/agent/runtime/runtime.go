// Package runtime contains the product-level lifecycle around the ADK runner.
//
// ADK owns model/tool execution and session history. This package owns the
// durable task (Invocation), the product event stream, cancellation and the
// approval resume boundary.
package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"Abot/internal/agent"
	"Abot/internal/provider"
	"Abot/internal/workspace"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

var (
	ErrNotFound                   = errors.New("运行时对象不存在")
	ErrConflict                   = errors.New("运行时状态冲突")
	ErrNotStarted                 = errors.New("Agent Runtime 尚未启动")
	ErrAlreadyResolved            = errors.New("审批已经处理")
	ErrApprovalExpired            = errors.New("审批已过期")
	ErrResumeMismatch             = errors.New("恢复输入与等待状态不匹配")
	ErrInvalidResume              = errors.New("恢复请求无效")
	ErrEventReplayLimit           = errors.New("运行时事件回放超过上限")
	ErrEventReplayInvalid         = errors.New("运行时事件回放顺序无效")
	ErrInvalidWorkingSet          = agent.ErrInvalidWorkingSet
	ErrToolSnapshotConflict       = agent.ErrToolSnapshotConflict
	ErrCapabilitySnapshotConflict = provider.ErrCapabilitySnapshotConflict
)

var (
	runtimeSecretAssignmentPattern = regexp.MustCompile(`(?i)(api[_-]?key|access[_-]?token|refresh[_-]?token|password|passwd|secret|authorization|private[_-]?key)\s*[:=]\s*(?:Bearer\s+)?([^\s,;]+)`)
	runtimeBearerPattern           = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
)

// postTerminalAuditContextKey is intentionally private. Only Runtime's
// durable-evidence bridge can mark an event as a late audit fact; callers
// cannot bypass the terminal Invocation event fence by supplying a context
// value of their own.
type postTerminalAuditContextKey struct{}

func withPostTerminalAudit(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, postTerminalAuditContextKey{}, true)
}

func isPostTerminalAudit(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	marked, _ := ctx.Value(postTerminalAuditContextKey{}).(bool)
	return marked
}

// compactionFailureEvent builds the metadata-only event emitted when ADK
// cannot persist/materialize a compaction summary. The upstream error is
// intentionally reduced to its concrete type: error strings can contain
// prompts, URLs or credentials and are not part of the Runtime event contract.
func compactionFailureEvent(invocationID string, cause error, consecutive int, now time.Time) AgentEvent {
	invocationID = strings.TrimSpace(invocationID)
	if consecutive < 1 {
		consecutive = 1
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	details := map[string]any{"consecutive_failures": consecutive}
	if cause != nil {
		details["error_type"] = fmt.Sprintf("%T", cause)
	}
	return AgentEvent{
		ID: newID("event"), InvocationID: invocationID, Type: EventContextCompactionFailed, Timestamp: now,
		Data: map[string]any{
			"code": "context_compaction_failed", "category": "context",
			"message": "上下文压缩失败，已保留已有会话事件", "retryable": true,
			"consecutive_failures": consecutive, "details": details,
		},
	}
}

// InvocationStatus describes the durable lifecycle of one user task.
type InvocationStatus string

const (
	InvocationQueued          InvocationStatus = "queued"
	InvocationRunning         InvocationStatus = "running"
	InvocationWaitingApproval InvocationStatus = "waiting_approval"
	InvocationWaitingTool     InvocationStatus = "waiting_tool"
	InvocationWaitingUser     InvocationStatus = "waiting_user"
	InvocationCancelling      InvocationStatus = "cancelling"
	InvocationCompleted       InvocationStatus = "completed"
	InvocationFailed          InvocationStatus = "failed"
	InvocationCancelled       InvocationStatus = "cancelled"
	InvocationExpired         InvocationStatus = "expired"
)

func (s InvocationStatus) Terminal() bool {
	return s == InvocationCompleted || s == InvocationFailed || s == InvocationCancelled || s == InvocationExpired
}

// Invocation is the durable product task envelope. The ADK session ID is
// deliberately retained separately so a task can be resumed without replaying
// the original user text.
type Invocation struct {
	ID             string `json:"id"`
	UserID         string `json:"user_id"`
	IdempotencyKey string `json:"-"`
	BotID          string `json:"bot_id,omitempty"`
	ConversationID string `json:"conversation_id"`
	WorkspaceID    string `json:"workspace_id,omitempty"`
	TargetPath     string `json:"target_path,omitempty"`
	SessionID      string `json:"session_id"`
	// ParentInvocationID is set only for an explicit workflow continuation;
	// terminal source invocations remain immutable and are never reopened.
	ParentInvocationID             string                       `json:"parent_invocation_id,omitempty"`
	ContinuationDecision           WorkflowContinuationDecision `json:"continuation_decision,omitempty"`
	ContinuationSourcePlanRevision int64                        `json:"continuation_source_plan_revision,omitempty"`
	ProviderID                     string                       `json:"provider_id,omitempty"`
	ModelID                        string                       `json:"model_id,omitempty"`
	// ConfigSnapshot is the canonical, metadata-only effective runtime
	// configuration captured before the first model call. It is deliberately
	// hidden from the public Invocation envelope; ConfigSnapshotDigest is the
	// safe audit projection exposed to clients.
	ConfigSnapshot       string `json:"-"`
	ConfigSnapshotDigest string `json:"config_snapshot_digest,omitempty"`
	Message              string `json:"message,omitempty"`
	// 群历史是低信任背景，持久化供排队任务重启恢复，但不公开为任务目标。
	GroupContext string `json:"-"`
	// 主动回复标记随任务持久化，确保恢复后仍保持无工具权限。
	Proactive bool `json:"-"`
	// Attachments are persisted with the accepted input so a queued
	// invocation recovered after restart is semantically identical. They are
	// intentionally excluded from the API envelope; future Artifact refs can
	// replace inline bytes without changing the resume contract.
	Attachments      []agent.Attachment `json:"-"`
	Status           InvocationStatus   `json:"status"`
	Error            string             `json:"error,omitempty"`
	ActiveApprovalID string             `json:"active_approval_id,omitempty"`
	// Lease fields are runtime-internal and deliberately excluded from the API
	// envelope. A worker must hold the short lease before entering the ADK
	// loop; a lost lease is treated as an unknown side-effect boundary.
	LeaseOwner     string     `json:"-"`
	LeaseExpiresAt *time.Time `json:"-"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
}

// AgentEvent is the stable product event protocol. Data is deliberately a
// JSON-shaped map so clients do not need to understand ADK internals.
type AgentEvent struct {
	ID           string         `json:"id"`
	InvocationID string         `json:"invocation_id"`
	Sequence     int64          `json:"sequence"`
	Type         string         `json:"type"`
	Timestamp    time.Time      `json:"timestamp"`
	Data         map[string]any `json:"data,omitempty"`
}

const (
	EventInvocationQueued              = "invocation.queued"
	EventInvocationStarted             = "invocation.started"
	EventInvocationWaiting             = "invocation.waiting"
	EventInvocationResumed             = "invocation.resumed"
	EventInvocationCancelling          = "invocation.cancelling"
	EventInvocationCompleted           = "invocation.completed"
	EventInvocationFailed              = "invocation.failed"
	EventInvocationCancelled           = "invocation.cancelled"
	EventInvocationExpired             = "invocation.expired"
	EventAssistantDelta                = "assistant.delta"
	EventAssistantMessage              = "assistant.message"
	EventUserMessage                   = "user.message"
	EventToolRequested                 = "tool.requested"
	EventToolStarted                   = "tool.started"
	EventToolOutput                    = "tool.output"
	EventToolCompleted                 = "tool.completed"
	EventToolFailed                    = "tool.failed"
	EventModelStarted                  = "model.started"
	EventModelCompleted                = "model.completed"
	EventModelRetrying                 = "model.retrying"
	EventModelFailed                   = "model.failed"
	EventApprovalRequested             = "approval.requested"
	EventApprovalResolved              = "approval.resolved"
	EventApprovalExpired               = "approval.expired"
	EventUserInputRequested            = "user_input.requested"
	EventUserInputResolved             = "user_input.resolved"
	EventContextCompacted              = "context.compacted"
	EventContextCompactionFailed       = "context.compaction_failed"
	EventContextManifest               = "context.manifest"
	EventRuntimeSnapshot               = "runtime.snapshot"
	EventRuntimeConfigSnapshot         = "runtime.config_snapshot"
	EventRuntimeConfigChanged          = "runtime.config_changed"
	EventRuntimeConfigMigrated         = "runtime.config_migrated"
	EventToolSetSnapshot               = "toolset.snapshot_created"
	EventToolSetInvalidated            = "toolset.snapshot_invalidated"
	EventModelCapabilitiesResolved     = "model.capabilities_resolved"
	EventModelRequestDowngraded        = "model.request_downgraded"
	EventModelIncompatible             = "model.incompatible"
	EventModelMigrationRequired        = "model.migration_required"
	EventCommandOutput                 = "command.output"
	EventCommandQueued                 = "command.queued"
	EventCommandStarted                = "command.started"
	EventCommandCompleted              = "command.completed"
	EventCommandFailed                 = "command.failed"
	EventCommandUnknown                = "command.unknown"
	EventUsageUpdated                  = "usage.updated"
	EventPlanUpdated                   = "plan.updated"
	EventTaskContractUpdated           = "task.contract.updated"
	EventInstructionConflict           = "instruction.conflict"
	EventInstructionReconfirmed        = "instruction.reconfirmed"
	EventWorkingSetUpdated             = "working_set.updated"
	EventRepositoryBaseline            = "repository.baseline_captured"
	EventVerificationUpdated           = "verification.updated"
	EventArtifactUpdated               = "artifact.updated"
	EventRuntimeNotice                 = "runtime.notice"
	EventADK                           = "adk.event"
	EventWorkflowContinuationRequested = "workflow.continuation_requested"
)

type ApprovalStatus string

const (
	ApprovalPending   ApprovalStatus = "pending"
	ApprovalApproved  ApprovalStatus = "approved"
	ApprovalRejected  ApprovalStatus = "rejected"
	ApprovalExpired   ApprovalStatus = "expired"
	ApprovalCancelled ApprovalStatus = "cancelled"
)

type ToolCallStatus string

const (
	ToolCallRequested       ToolCallStatus = "requested"
	ToolCallRunning         ToolCallStatus = "running"
	ToolCallWaitingApproval ToolCallStatus = "waiting_approval"
	ToolCallApproved        ToolCallStatus = "approved"
	ToolCallCompleted       ToolCallStatus = "completed"
	ToolCallFailed          ToolCallStatus = "failed"
	ToolCallRejected        ToolCallStatus = "rejected"
	ToolCallExpired         ToolCallStatus = "expired"
	ToolCallCancelled       ToolCallStatus = "cancelled"
	ToolCallDenied          ToolCallStatus = "denied"
)

func (s ToolCallStatus) Terminal() bool {
	return s == ToolCallCompleted || s == ToolCallFailed || s == ToolCallRejected || s == ToolCallExpired || s == ToolCallCancelled || s == ToolCallDenied
}

// ToolCall is the durable product record for one model-issued function call.
// It is deliberately separate from Operation (the side effect) and Approval
// (the user's decision), even when all three are created in one ADK turn.
type ToolCall struct {
	ID                 string         `json:"id"`
	InvocationID       string         `json:"invocation_id"`
	ConversationID     string         `json:"conversation_id,omitempty"`
	ToolName           string         `json:"tool_name"`
	OriginalCallID     string         `json:"original_call_id"`
	ConfirmationCallID string         `json:"confirmation_call_id,omitempty"`
	OperationID        string         `json:"operation_id,omitempty"`
	ApprovalID         string         `json:"approval_id,omitempty"`
	Args               map[string]any `json:"args,omitempty"`
	Status             ToolCallStatus `json:"status"`
	Error              string         `json:"error,omitempty"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
	FinishedAt         *time.Time     `json:"finished_at,omitempty"`
}

// Approval stores both ADK call IDs. The confirmation call ID is the only ID
// accepted by the resume protocol; the original call ID is retained for audit.
type Approval struct {
	ID                  string           `json:"id"`
	InvocationID        string           `json:"invocation_id"`
	ConversationID      string           `json:"conversation_id,omitempty"`
	ToolCallID          string           `json:"tool_call_id,omitempty"`
	TaskContractVersion int64            `json:"task_contract_version,omitempty"`
	ToolName            string           `json:"tool_name"`
	OperationID         string           `json:"operation_id,omitempty"`
	OriginalCallID      string           `json:"original_call_id"`
	ConfirmationCallID  string           `json:"confirmation_call_id"`
	Args                map[string]any   `json:"args,omitempty"`
	Hint                string           `json:"hint,omitempty"`
	Choices             []ApprovalChoice `json:"choices,omitempty"`
	Status              ApprovalStatus   `json:"status"`
	DecisionReason      string           `json:"decision_reason,omitempty"`
	ExpiresAt           *time.Time       `json:"expires_at,omitempty"`
	CreatedAt           time.Time        `json:"created_at"`
	UpdatedAt           time.Time        `json:"updated_at"`
	ResolvedAt          *time.Time       `json:"resolved_at,omitempty"`
}

// ApprovalChoice 是一次人机交互中可被明确选择的选项。
// Approved 只描述该选项是否允许当前工具继续执行；ChoiceID 会随恢复请求
// 一并进入审计数据，避免把按钮选择重新压成自然语言关键词或丢失为单一 bool。
type ApprovalChoice struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Approved bool   `json:"approved"`
}

// DefaultApprovalChoices 是当前内置 Workspace 工具的最小交互集合。返回副本
// 的意图是避免调用方修改全局默认选项，造成不同审批请求之间相互污染。
func DefaultApprovalChoices() []ApprovalChoice {
	return []ApprovalChoice{
		{ID: "approve", Label: "允许一次", Approved: true},
		{ID: "reject", Label: "拒绝", Approved: false},
	}
}

// UserInputKind 描述一次需要用户回答的普通交互问题。它和 Approval
// 明确分离：Approval 只处理工具授权，UserInput 才负责单选、多选和文本回答。
type UserInputKind string

const (
	UserInputText         UserInputKind = "text"
	UserInputSingleSelect UserInputKind = "single_select"
	UserInputMultiSelect  UserInputKind = "multi_select"
)

// UserInputOption 是用户问题中的一个可选答案。ID 会进入恢复 payload，
// Label/Description 只用于展示，避免下游根据自然语言重新猜测用户选择。
type UserInputOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Recommended bool   `json:"recommended,omitempty"`
}

// UserInputRequest 是 ADK RequestInput 的产品层投影。Payload 采用受限的
// JSON 形状，既可以由工作流直接提供，也可以由其他内置 Agent 能力生成。
type UserInputRequest struct {
	ID            string            `json:"id"`
	InvocationID  string            `json:"invocation_id"`
	Title         string            `json:"title,omitempty"`
	Question      string            `json:"question"`
	Kind          UserInputKind     `json:"kind"`
	Options       []UserInputOption `json:"options,omitempty"`
	AllowMultiple bool              `json:"allow_multiple,omitempty"`
	AllowFreeform bool              `json:"allow_freeform,omitempty"`
	AllowSkip     bool              `json:"allow_skip,omitempty"`
	MinSelections int               `json:"min_selections,omitempty"`
	MaxSelections int               `json:"max_selections,omitempty"`
	Step          int               `json:"step,omitempty"`
	TotalSteps    int               `json:"total_steps,omitempty"`
	Placeholder   string            `json:"placeholder,omitempty"`
	Payload       map[string]any    `json:"payload,omitempty"`
}

// InvocationResumeItem is one bounded response for an ADK long-running tool
// call. WaitID is the FunctionCall ID emitted in EventInvocationWaiting; it is
// never inferred from caller text. Name is optional and is checked against the
// durable tool event when present.
type InvocationResumeItem struct {
	WaitID   string         `json:"wait_id"`
	Name     string         `json:"name,omitempty"`
	Response map[string]any `json:"response"`
}

// InvocationResumeRequest is the bounded, provider-neutral input used to
// answer one or more ADK long-running tool calls. The original wait_id/name/
// response fields remain the compatible single-call form; Responses is the
// atomic multi-call form for a boundary that contains several wait IDs. A
// request must use exactly one form.
type InvocationResumeRequest struct {
	WaitID    string                 `json:"wait_id"`
	Name      string                 `json:"name,omitempty"`
	Response  map[string]any         `json:"response"`
	Responses []InvocationResumeItem `json:"responses,omitempty"`
	// BoundaryID is an optional durable workflow boundary identifier. When
	// supplied, Runtime verifies that the request addresses the current ADK
	// waiting boundary instead of relying only on the newest wait IDs.
	BoundaryID     string `json:"boundary_id,omitempty"`
	IdempotencyKey string `json:"-"`
	// The following fields are Runtime-internal proof metadata. They are set
	// only by the explicit external Tool Runtime reconciliation path and are
	// emitted as bounded audit metadata, never as a response payload.
	statusQueryIDs     []string
	statusResultDigest []string
}

// InvocationResume is the private durable handoff for a queued tool resume.
// ResponseJSON is bounded structured data and is never included in an
// Invocation API envelope or AgentEvent; it exists only to bridge the crash
// window between waiting_tool -> queued and the next Runner activation.
type InvocationResume struct {
	ID            string    `json:"-"`
	InvocationID  string    `json:"-"`
	WaitID        string    `json:"wait_id"`
	Name          string    `json:"name"`
	ResponseJSON  []byte    `json:"-"`
	RequestDigest string    `json:"request_digest"`
	CreatedAt     time.Time `json:"created_at"`
}

// InvocationResumeOutbox is the durable delivery record for one accepted
// resume. Unlike the private handoff above, an outbox item retains its
// delivery state, attempt count and short lease so another Coordinator can
// safely claim an item after a worker disappears. ResponseJSON remains
// private bounded data; only its request digest is exposed in AgentEvent.
type InvocationResumeOutbox struct {
	ID             string                       `json:"-"`
	InvocationID   string                       `json:"invocation_id"`
	WaitID         string                       `json:"wait_id"`
	Name           string                       `json:"name"`
	ResponseJSON   []byte                       `json:"-"`
	RequestDigest  string                       `json:"request_digest"`
	Status         InvocationResumeOutboxStatus `json:"status"`
	Attempt        int                          `json:"attempt"`
	Revision       int64                        `json:"revision"`
	AvailableAt    time.Time                    `json:"available_at"`
	LeaseOwner     string                       `json:"-"`
	LeaseExpiresAt *time.Time                   `json:"-"`
	LastError      string                       `json:"last_error,omitempty"`
	CreatedAt      time.Time                    `json:"created_at"`
	UpdatedAt      time.Time                    `json:"updated_at"`
}

// InvocationResumeOutboxStatus is intentionally independent from Invocation
// status. An invocation may remain queued while an outbox delivery is claimed
// or may reach a terminal state after a delivery lease is lost.
type InvocationResumeOutboxStatus string

const (
	InvocationResumeOutboxQueued      InvocationResumeOutboxStatus = "queued"
	InvocationResumeOutboxProcessing  InvocationResumeOutboxStatus = "processing"
	InvocationResumeOutboxCompleted   InvocationResumeOutboxStatus = "completed"
	InvocationResumeOutboxFailed      InvocationResumeOutboxStatus = "failed"
	MaxInvocationResumeOutboxAttempts                              = 8
	// MaxInvocationResumeOutboxOwnerLength keeps the private resume worker
	// identity bounded before it reaches a storage index or diagnostic field.
	MaxInvocationResumeOutboxOwnerLength = 160
	// MaxInvocationResumeOutboxLease prevents a resume delivery from being
	// pinned indefinitely. The coordinator uses InvocationLeaseTTL (2m).
	MaxInvocationResumeOutboxLease = 10 * time.Minute
)

func (s InvocationResumeOutboxStatus) terminal() bool {
	return s == InvocationResumeOutboxCompleted || s == InvocationResumeOutboxFailed
}

// RuntimeEventOutbox is the durable delivery cursor for one AgentEvent. The
// event body is intentionally not copied here: a consumer claims this
// metadata record and fetches the immutable event by EventID before handing it
// to its transport. Keeping the body in the event log avoids a second copy of
// prompts, command output or other user data while preserving an atomic
// event→delivery boundary in the built-in repositories.
type RuntimeEventOutbox struct {
	ID             string                   `json:"id"`
	GroupID        string                   `json:"group_id,omitempty"`
	InvocationID   string                   `json:"invocation_id"`
	EventID        string                   `json:"event_id"`
	Sequence       int64                    `json:"sequence"`
	Type           string                   `json:"type"`
	Status         RuntimeEventOutboxStatus `json:"status"`
	Attempt        int                      `json:"attempt"`
	Revision       int64                    `json:"revision"`
	AvailableAt    time.Time                `json:"available_at"`
	LeaseOwner     string                   `json:"-"`
	LeaseExpiresAt *time.Time               `json:"-"`
	LastError      string                   `json:"last_error,omitempty"`
	CreatedAt      time.Time                `json:"created_at"`
	UpdatedAt      time.Time                `json:"updated_at"`
}

// RuntimeEventOutboxStatus is independent from Invocation status. A
// terminal Invocation can still have queued audit delivery, and delivery
// completion must never reopen or mutate that Invocation.
type RuntimeEventOutboxStatus string

const (
	RuntimeEventOutboxQueued      RuntimeEventOutboxStatus = "queued"
	RuntimeEventOutboxProcessing  RuntimeEventOutboxStatus = "processing"
	RuntimeEventOutboxCompleted   RuntimeEventOutboxStatus = "completed"
	RuntimeEventOutboxFailed      RuntimeEventOutboxStatus = "failed"
	MaxRuntimeEventOutboxAttempts                          = 8
	// MaxRuntimeEventOutboxOwnerLength keeps a worker identity bounded before
	// it reaches a storage index or an audit log.
	MaxRuntimeEventOutboxOwnerLength = 160
	// MaxRuntimeEventOutboxLease prevents a caller from pinning a delivery for
	// an unbounded period. The Coordinator uses a much shorter 30s lease.
	MaxRuntimeEventOutboxLease = 10 * time.Minute
)

func (s RuntimeEventOutboxStatus) terminal() bool {
	return s == RuntimeEventOutboxCompleted || s == RuntimeEventOutboxFailed
}

// Normalize validates one event delivery cursor and fills deterministic
// defaults. It deliberately accepts only metadata and bounds the retry
// surface so a malformed external consumer cannot create an unbounded queue.
func (o RuntimeEventOutbox) Normalize(now time.Time) (RuntimeEventOutbox, error) {
	o.ID = strings.TrimSpace(o.ID)
	o.GroupID = strings.TrimSpace(o.GroupID)
	o.InvocationID = strings.TrimSpace(o.InvocationID)
	o.EventID = strings.TrimSpace(o.EventID)
	o.Type = strings.TrimSpace(o.Type)
	o.LeaseOwner = strings.TrimSpace(o.LeaseOwner)
	o.LastError = sanitizeRuntimeString(strings.TrimSpace(o.LastError))
	if o.ID == "" || o.InvocationID == "" || o.EventID == "" || o.Type == "" || o.Sequence <= 0 {
		return RuntimeEventOutbox{}, fmt.Errorf("%w: event outbox metadata 不完整", ErrConflict)
	}
	if len(o.ID) > 128 || len(o.GroupID) > MaxRuntimeDeliveryGroupIDLength || len(o.EventID) > 512 || len(o.Type) > 80 {
		return RuntimeEventOutbox{}, fmt.Errorf("%w: event outbox metadata 超出长度限制", ErrConflict)
	}
	if len(o.LeaseOwner) > MaxRuntimeEventOutboxOwnerLength {
		return RuntimeEventOutbox{}, fmt.Errorf("%w: event outbox lease owner 超出长度限制", ErrConflict)
	}
	if o.Attempt < 0 || o.Attempt > MaxRuntimeEventOutboxAttempts {
		return RuntimeEventOutbox{}, fmt.Errorf("%w: event outbox attempt 超出范围", ErrConflict)
	}
	if o.Status == "" {
		o.Status = RuntimeEventOutboxQueued
	}
	switch o.Status {
	case RuntimeEventOutboxQueued, RuntimeEventOutboxProcessing, RuntimeEventOutboxCompleted, RuntimeEventOutboxFailed:
	default:
		return RuntimeEventOutbox{}, fmt.Errorf("%w: event outbox status %q 不受支持", ErrConflict, o.Status)
	}
	if o.Status == RuntimeEventOutboxProcessing && (o.LeaseOwner == "" || o.LeaseExpiresAt == nil) {
		return RuntimeEventOutbox{}, fmt.Errorf("%w: processing event outbox 必须带租约", ErrConflict)
	}
	if len(o.LastError) > 4096 {
		o.LastError = o.LastError[:4096]
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if o.AvailableAt.IsZero() {
		o.AvailableAt = now
	} else {
		o.AvailableAt = o.AvailableAt.UTC()
	}
	if o.CreatedAt.IsZero() {
		o.CreatedAt = now
	} else {
		o.CreatedAt = o.CreatedAt.UTC()
	}
	if o.UpdatedAt.IsZero() {
		o.UpdatedAt = now
	} else {
		o.UpdatedAt = o.UpdatedAt.UTC()
	}
	if o.Revision <= 0 {
		o.Revision = 1
	}
	if o.Status.terminal() {
		o.LeaseOwner = ""
		o.LeaseExpiresAt = nil
	} else if o.Status == RuntimeEventOutboxQueued {
		// A queued row is claimable metadata, never an active lease. Clearing
		// stale lease fields here makes retries and migrations idempotent.
		o.LeaseOwner = ""
		o.LeaseExpiresAt = nil
	}
	return o, nil
}

// NewRuntimeEventOutbox builds the metadata cursor associated with one
// persisted event. The ID is a digest-derived key so unusual but valid event
// IDs do not overflow the storage primary-key column; EventID remains the
// authoritative lookup key and is also unique in the built-in stores.
func NewRuntimeEventOutbox(event AgentEvent) (RuntimeEventOutbox, error) {
	event.ID = strings.TrimSpace(event.ID)
	event.InvocationID = strings.TrimSpace(event.InvocationID)
	event.Type = strings.TrimSpace(event.Type)
	if event.ID == "" || event.InvocationID == "" || event.Type == "" || event.Sequence <= 0 {
		return RuntimeEventOutbox{}, fmt.Errorf("%w: event outbox 需要已持久化事件", ErrConflict)
	}
	digest := sha256.Sum256([]byte(event.ID))
	item := RuntimeEventOutbox{
		ID: "event-outbox-" + fmt.Sprintf("%x", digest[:16]), InvocationID: event.InvocationID,
		EventID: event.ID, Sequence: event.Sequence, Type: event.Type,
		Status:      RuntimeEventOutboxQueued,
		AvailableAt: event.Timestamp, CreatedAt: event.Timestamp, UpdatedAt: event.Timestamp,
	}
	return item.Normalize(event.Timestamp)
}

func runtimeEventOutboxBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

// RuntimeEventOutboxBackoff exposes the same bounded retry schedule to a
// storage implementation without duplicating policy across packages.
func RuntimeEventOutboxBackoff(attempt int) time.Duration {
	return runtimeEventOutboxBackoff(attempt)
}

// SanitizeRuntimeString is the storage-facing wrapper used when an outbox
// retry records a short diagnostic. It strips credential-shaped fragments but
// does not persist the original provider error body.
func SanitizeRuntimeString(value string) string {
	return sanitizeRuntimeString(value)
}

// SanitizeAgentEvent returns a defensive, metadata-safe copy for an external
// outbox consumer. The durable event log remains unchanged; this only ensures
// a handler cannot accidentally forward credential-shaped fields from a
// legacy/custom repository.
func SanitizeAgentEvent(event AgentEvent) AgentEvent {
	if event.Data != nil {
		event.Data = sanitizeRuntimeMap(event.Data)
	}
	return event
}

// Normalize validates the private outbox payload and fills deterministic
// defaults. It never accepts an unbounded response or an unknown status.
func (o InvocationResumeOutbox) Normalize(now time.Time) (InvocationResumeOutbox, error) {
	o.ID = strings.TrimSpace(o.ID)
	o.InvocationID = strings.TrimSpace(o.InvocationID)
	o.WaitID = strings.TrimSpace(o.WaitID)
	o.Name = strings.TrimSpace(o.Name)
	o.RequestDigest = strings.TrimSpace(o.RequestDigest)
	o.LeaseOwner = strings.TrimSpace(o.LeaseOwner)
	o.LastError = strings.TrimSpace(o.LastError)
	if o.InvocationID == "" || o.WaitID == "" || o.Name == "" || o.RequestDigest == "" {
		return InvocationResumeOutbox{}, ErrInvalidResume
	}
	if len(o.LeaseOwner) > MaxInvocationResumeOutboxOwnerLength {
		return InvocationResumeOutbox{}, fmt.Errorf("%w: resume outbox lease owner 超出长度限制", ErrInvalidResume)
	}
	if len(o.ResponseJSON) > maxInvocationResumeResponseBytes {
		return InvocationResumeOutbox{}, fmt.Errorf("%w: outbox response 超过 %d 字节", ErrInvalidResume, maxInvocationResumeResponseBytes)
	}
	if o.Attempt < 0 || o.Attempt > MaxInvocationResumeOutboxAttempts {
		return InvocationResumeOutbox{}, fmt.Errorf("%w: outbox attempt 超出范围", ErrInvalidResume)
	}
	if o.Status == "" {
		o.Status = InvocationResumeOutboxQueued
	}
	switch o.Status {
	case InvocationResumeOutboxQueued, InvocationResumeOutboxProcessing, InvocationResumeOutboxCompleted, InvocationResumeOutboxFailed:
	default:
		return InvocationResumeOutbox{}, fmt.Errorf("%w: outbox status %q 不受支持", ErrInvalidResume, o.Status)
	}
	if len(o.LastError) > 4096 {
		o.LastError = o.LastError[:4096]
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if o.CreatedAt.IsZero() {
		o.CreatedAt = now
	} else {
		o.CreatedAt = o.CreatedAt.UTC()
	}
	if o.UpdatedAt.IsZero() {
		o.UpdatedAt = now
	} else {
		o.UpdatedAt = o.UpdatedAt.UTC()
	}
	if o.AvailableAt.IsZero() {
		o.AvailableAt = now
	} else {
		o.AvailableAt = o.AvailableAt.UTC()
	}
	if o.LeaseExpiresAt != nil {
		expires := o.LeaseExpiresAt.UTC()
		o.LeaseExpiresAt = &expires
	}
	if o.Revision <= 0 {
		o.Revision = 1
	}
	if o.Status == InvocationResumeOutboxProcessing {
		if o.LeaseOwner == "" || o.LeaseExpiresAt == nil {
			return InvocationResumeOutbox{}, fmt.Errorf("%w: processing resume outbox 必须带有效租约", ErrInvalidResume)
		}
		if o.LeaseExpiresAt.After(now.Add(MaxInvocationResumeOutboxLease)) {
			return InvocationResumeOutbox{}, fmt.Errorf("%w: resume outbox lease 超出时长限制", ErrInvalidResume)
		}
	}
	if o.Status.terminal() {
		o.LeaseOwner = ""
		o.LeaseExpiresAt = nil
	} else if o.Status == InvocationResumeOutboxQueued {
		// A queued row is claimable metadata, never an active lease. Clearing
		// stale fields makes retries and migrations idempotent.
		o.LeaseOwner = ""
		o.LeaseExpiresAt = nil
	}
	return o, nil
}

// Repository is the persistence boundary for the coordinator. SQLite is the
// production implementation; a small in-memory implementation is included for
// deterministic unit tests and embedders.
type Repository interface {
	CreateInvocation(context.Context, Invocation) error
	GetInvocation(context.Context, string) (Invocation, error)
	ListInvocations(context.Context, string, []InvocationStatus) ([]Invocation, error)
	TransitionInvocation(context.Context, string, InvocationStatus, InvocationStatus, string) (bool, error)
	UpdateInvocation(context.Context, Invocation) error
	AppendEvent(context.Context, AgentEvent) (AgentEvent, error)
	ListEvents(context.Context, string, int64, int) ([]AgentEvent, error)
	CreateApproval(context.Context, Approval) error
	GetApproval(context.Context, string) (Approval, error)
	ListApprovals(context.Context, string, ApprovalStatus) ([]Approval, error)
	ResolveApproval(context.Context, string, ApprovalStatus, string) (Approval, bool, error)
}

// AgentEventRepository is optional so an outbox consumer can fetch the
// immutable event body after claiming a metadata cursor. Implementations must
// return a defensive copy; the event log remains the only persisted payload.
type AgentEventRepository interface {
	GetAgentEvent(context.Context, string) (AgentEvent, error)
}

// RuntimeEventOutboxRepository is optional for compatibility with embedders.
// SQLite and Memory atomically create one delivery cursor with every event
// append, then expose claim/complete/retry operations for an external,
// idempotent transport. This is an at-least-once boundary: a consumer must
// deduplicate by EventID because a process can crash after transport success
// and before acknowledgement.
type RuntimeEventOutboxRepository interface {
	EnqueueRuntimeEventOutbox(context.Context, RuntimeEventOutbox) (RuntimeEventOutbox, error)
	GetRuntimeEventOutbox(context.Context, string) (RuntimeEventOutbox, error)
	ListRuntimeEventOutbox(context.Context, string, RuntimeEventOutboxStatus, int) ([]RuntimeEventOutbox, error)
	ClaimRuntimeEventOutbox(context.Context, string, time.Time, time.Duration) (RuntimeEventOutbox, bool, error)
	CompleteRuntimeEventOutbox(context.Context, string, string, time.Time) (bool, error)
	RetryRuntimeEventOutbox(context.Context, string, string, time.Time, string) (bool, error)
}

// RuntimeDeliveryGroupRepository is the optional source-side coordination
// ledger for local commits that create more than one delivery cursor. It is
// intentionally additive so older embedders can keep implementing the core
// Repository without a migration; built-in Memory and SQLite repositories
// provide the durable implementation.
type RuntimeDeliveryGroupRepository interface {
	EnqueueRuntimeDeliveryGroup(context.Context, RuntimeDeliveryGroup) (RuntimeDeliveryGroup, error)
	GetRuntimeDeliveryGroup(context.Context, string) (RuntimeDeliveryGroup, error)
	ListRuntimeDeliveryGroups(context.Context, string, RuntimeDeliveryGroupStatus, int) ([]RuntimeDeliveryGroup, error)
	ClaimRuntimeDeliveryGroup(context.Context, string, time.Time, time.Duration) (RuntimeDeliveryGroup, bool, error)
	CompleteRuntimeDeliveryGroup(context.Context, string, string, time.Time) (bool, error)
	RetryRuntimeDeliveryGroup(context.Context, string, string, time.Time, string) (bool, error)
	MarkRuntimeDeliveryGroupMember(context.Context, string, int64, RuntimeDeliveryKind, string, string, RuntimeDeliveryGroupMemberStatus, string, time.Time) (RuntimeDeliveryGroup, bool, error)
}

// RuntimeDeliveryGroupByIDClaimer is an optional refinement of the group
// ledger.  A scheduler that has already inspected one group can claim that
// exact row instead of racing a global "next due" claim and accidentally
// borrowing another group's lease.  Older embedders may omit this interface;
// the coordinator then keeps its metadata-only reconciliation fallback.
type RuntimeDeliveryGroupByIDClaimer interface {
	ClaimRuntimeDeliveryGroupByID(context.Context, string, string, time.Time, time.Duration) (RuntimeDeliveryGroup, bool, error)
}

// RuntimeDeliveryGroupDeferrer releases a group coordination lease without
// consuming a retry attempt.  It is used while one or more family outboxes
// are still pending; the family dispatcher owns delivery retries, while the
// group ledger only coordinates their terminal state.
type RuntimeDeliveryGroupDeferrer interface {
	DeferRuntimeDeliveryGroup(context.Context, string, string, time.Time, time.Time, string) (bool, error)
}

// RuntimeDeliveryGroupFenceIssuer is an optional source-side monotonic epoch
// allocator.  A destination only accepts a newer revision for the same
// source/destination/invocation scope; older embedders can omit it and keep
// the legacy unfenced settlement protocol.
type RuntimeDeliveryGroupFenceIssuer interface {
	IssueRuntimeDeliveryGroupFence(context.Context, string, string, string, string, time.Time) (RuntimeDeliveryGroupFence, error)
	GetRuntimeDeliveryGroupFence(context.Context, string) (RuntimeDeliveryGroupFence, error)
}

// RuntimeDeliveryGroupFenceBinder atomically attaches an issued fence to an
// existing source group. It is separate from the issuer so a repository can
// reject a stale group revision without mutating the ledger.
type RuntimeDeliveryGroupFenceBinder interface {
	BindRuntimeDeliveryGroupFence(context.Context, string, int64, RuntimeDeliveryGroupFence, time.Time) (RuntimeDeliveryGroup, error)
}

// RuntimeDeliveryGroupSettlementCommitRepository is the stronger local
// outcome boundary.  When a terminal member mark also makes the aggregate
// group terminal, built-in Memory/SQLite repositories can upsert the stable
// remote settlement intent in the same lock/transaction.  It is deliberately
// optional: older embedders retain MarkRuntimeDeliveryGroupMember plus the
// reconciliation fallback and must not be mistaken for an atomic boundary.
type RuntimeDeliveryGroupSettlementCommitRepository interface {
	MarkRuntimeDeliveryGroupMemberWithSettlement(context.Context, string, int64, RuntimeDeliveryKind, string, string, RuntimeDeliveryGroupMemberStatus, string, time.Time) (RuntimeDeliveryGroup, bool, *RuntimeDeliveryGroupSettlement, error)
}

// RuntimeCheckpointDeliveryOutboxRepository is the source-side durable
// cursor for metadata-only Runtime checkpoint delivery. SQLite and Memory
// implement the bounded claim/lease/retry contract; older embedders can omit
// it and keep the checkpoint source/client protocol as a separately managed
// integration.
type RuntimeCheckpointDeliveryOutboxRepository interface {
	EnqueueRuntimeCheckpointDeliveryOutbox(context.Context, RuntimeCheckpointDeliveryOutbox) (RuntimeCheckpointDeliveryOutbox, error)
	GetRuntimeCheckpointDeliveryOutbox(context.Context, string) (RuntimeCheckpointDeliveryOutbox, error)
	ListRuntimeCheckpointDeliveryOutbox(context.Context, string, RuntimeCheckpointDeliveryOutboxStatus, int) ([]RuntimeCheckpointDeliveryOutbox, error)
	ClaimRuntimeCheckpointDeliveryOutbox(context.Context, string, time.Time, time.Duration) (RuntimeCheckpointDeliveryOutbox, bool, error)
	CompleteRuntimeCheckpointDeliveryOutbox(context.Context, string, string, time.Time) (bool, error)
	RetryRuntimeCheckpointDeliveryOutbox(context.Context, string, string, time.Time, string) (bool, error)
}

// RuntimeEventOutboxConsistencyReader is an optional source-side consistency
// barrier. A checkpoint carrying event high-water N may only be sent after
// every event cursor through N on the same invocation has been completed. The
// capability is optional so older embedders keep their existing at-least-once
// behavior instead of being forced into an incompatible repository change.
type RuntimeEventOutboxConsistencyReader interface {
	RuntimeEventOutboxReadyThrough(context.Context, string, int64) (bool, error)
}

// RuntimeCheckpointDeliveryOutboxDeferrer releases a claimed checkpoint
// cursor without consuming a delivery attempt. It is used when the event
// high-water barrier is not ready; a normal retry would exhaust the bounded
// attempt budget while merely waiting for an earlier event cursor.
type RuntimeCheckpointDeliveryOutboxDeferrer interface {
	DeferRuntimeCheckpointDeliveryOutbox(context.Context, string, string, time.Time, time.Time, string) (bool, error)
}

// RuntimeCheckpointDeliveryTransport is the explicit destination adapter for
// a claimed checkpoint cursor. Implementations must be idempotent because a
// source worker may crash after the receiver commits but before the source
// lease acknowledgement.
type RuntimeCheckpointDeliveryTransport interface {
	Deliver(context.Context, RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error)
}

// RuntimeCheckpointDeliveryTransactionalTransport is an optional, stronger
// destination adapter.  Prepare records the exact metadata-only projection at
// the destination and Commit makes that prepared projection visible to the
// destination inbox.  Both calls must be idempotent by DeliveryID so a source
// worker crash between either phase can safely replay the phase sequence.
// It is deliberately optional: older transports continue using Deliver and
// retain the existing at-least-once inbox contract.
type RuntimeCheckpointDeliveryTransactionalTransport interface {
	Prepare(context.Context, RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error)
	Commit(context.Context, RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error)
}

// RuntimeCheckpointDeliveryAbortableTransport is an optional compensation
// capability for transactional checkpoint transports. Abort only releases a
// prepared destination record; it never attempts to roll back a committed
// checkpoint.
type RuntimeCheckpointDeliveryAbortableTransport interface {
	Abort(context.Context, RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error)
}

// RuntimeCheckpointDeliveryTransactionalOptIn lets a transport expose the
// stronger prepare/commit methods without forcing existing deployments to
// upgrade their receiver endpoints.  A transport that does not implement the
// marker remains transactional by default; concrete adapters that need
// backwards-compatible one-phase behavior can explicitly return false.
type RuntimeCheckpointDeliveryTransactionalOptIn interface {
	RuntimeCheckpointDeliveryTransactionsEnabled() bool
}

// RuntimeCheckpointDeliveryTransactionRepository is the destination-side
// durable prepare/commit ledger used by a transactional checkpoint receiver.
// Implementations must retain only the bounded projection and envelope
// metadata; prompt, tool arguments and command output remain source-local.
type RuntimeCheckpointDeliveryTransactionRepository interface {
	PrepareRuntimeCheckpointDelivery(context.Context, RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection) (duplicate bool, err error)
	CommitRuntimeCheckpointDelivery(context.Context, RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection) (duplicate bool, err error)
}

// RuntimeCheckpointDeliveryAbortableTransactionRepository is the optional
// destination-side compensation boundary. It remains separate from the
// prepare/commit interface for backwards compatibility.
type RuntimeCheckpointDeliveryAbortableTransactionRepository interface {
	AbortRuntimeCheckpointDelivery(context.Context, RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection) (duplicate bool, err error)
}

// RuntimeConfigDeliveryOutboxRepository is the source-side durable cursor for
// metadata-only configuration migration delivery.  It is intentionally
// separate from the event/checkpoint cursors so a configuration lock cannot be
// acknowledged by the wrong consumer.
type RuntimeConfigDeliveryOutboxRepository interface {
	EnqueueRuntimeConfigDeliveryOutbox(context.Context, RuntimeConfigDeliveryOutbox) (RuntimeConfigDeliveryOutbox, error)
	GetRuntimeConfigDeliveryOutbox(context.Context, string) (RuntimeConfigDeliveryOutbox, error)
	ListRuntimeConfigDeliveryOutbox(context.Context, string, RuntimeConfigDeliveryOutboxStatus, int) ([]RuntimeConfigDeliveryOutbox, error)
	ClaimRuntimeConfigDeliveryOutbox(context.Context, string, time.Time, time.Duration) (RuntimeConfigDeliveryOutbox, bool, error)
	CompleteRuntimeConfigDeliveryOutbox(context.Context, string, string, time.Time) (bool, error)
	RetryRuntimeConfigDeliveryOutbox(context.Context, string, string, time.Time, string) (bool, error)
}

// RuntimeConfigMigrationDeliveryCommitRepository is the strong source-side
// boundary for a remote configuration lock.  Implementations must update the
// Invocation snapshot, migration event/event-outbox and config-delivery
// outbox in one local transaction; split writes are not safe for a lock.
type RuntimeConfigMigrationDeliveryCommitRepository interface {
	CommitRuntimeConfigMigrationWithDelivery(context.Context, RuntimeConfigMigrationCommit, string, string) (Invocation, AgentEvent, RuntimeConfigDeliveryOutbox, error)
}

// RuntimeDeliveryAttemptRepository is the optional source-side phase journal
// shared by event/checkpoint/config/rejection delivery. Built-in Memory and
// SQLite repositories persist it; older embedders can omit the interface and
// retain the existing at-least-once cursor behavior.
//
// A journal row is metadata-only and is never a substitute for the durable
// outbox cursor. It records the last destination phase so a source restart can
// resume a prepared transaction with Commit or acknowledge an already
// accepted destination without blindly replaying the payload.
// (The interface itself is declared in delivery_attempt.go beside its domain
// types; this comment keeps the repository capability visible with the other
// Runtime extension points.)

// RuntimeEventDeliveryInboxRepository is the optional receiver-side durable
// idempotency boundary for signed cross-service event envelopes. SQLite and
// Memory implement it; older embedders may keep using their own inbox.
type RuntimeEventDeliveryInboxRepository interface {
	RuntimeEventDeliveryInbox
}

// InvocationIdempotencyRepository is optional so existing embedders can keep
// implementing Repository without a migration. SQLite and MemoryRepository
// implement it; the coordinator falls back to active-conversation protection
// when a custom repository does not.
type InvocationIdempotencyRepository interface {
	GetInvocationByIdempotencyKey(context.Context, string, string, string) (Invocation, error)
}

// RuntimeConfigMigrationCommit describes the explicit user-confirmed
// migration of a waiting Invocation to the Runtime configuration currently
// resolved by the Kernel. The client supplies only the expected old digest;
// the destination Runtime computes and validates NewSnapshot itself.
type RuntimeConfigMigrationCommit struct {
	InvocationID   string
	ExpectedDigest string
	NewSnapshot    string
	IdempotencyKey string
	Event          AgentEvent
}

// RuntimeConfigMigrationCommitRepository is the stronger local persistence
// boundary for a config migration. Implementations must CAS the old digest,
// update the hidden snapshot, append the metadata-only migration event and
// create its Event outbox cursor in one transaction. Older embedders can omit
// it; the Coordinator then fails closed rather than performing a split write.
type RuntimeConfigMigrationCommitRepository interface {
	CommitRuntimeConfigMigration(context.Context, RuntimeConfigMigrationCommit) (Invocation, AgentEvent, error)
}

// InvocationResumeRepository is optional for compatibility with embedders.
// SQLite and MemoryRepository persist one pending handoff per Invocation so a
// process restart cannot turn an accepted tool response into a fresh replay of
// the original user message.
type InvocationResumeRepository interface {
	SaveInvocationResume(context.Context, InvocationResume) (InvocationResume, error)
	GetInvocationResume(context.Context, string) (InvocationResume, error)
	DeleteInvocationResume(context.Context, string) error
}

// InvocationResumeOutboxRepository is optional for compatibility with
// embedders. SQLite and MemoryRepository implement durable claim/complete/
// retry semantics; a Coordinator falls back to the existing in-process launch
// path when an older repository does not provide this interface.
type InvocationResumeOutboxRepository interface {
	EnqueueInvocationResumeOutbox(context.Context, InvocationResumeOutbox) (InvocationResumeOutbox, error)
	GetInvocationResumeOutbox(context.Context, string) (InvocationResumeOutbox, error)
	ClaimInvocationResumeOutbox(context.Context, string, string, time.Time, time.Duration) (InvocationResumeOutbox, bool, error)
	CompleteInvocationResumeOutbox(context.Context, string, string, time.Time) (bool, error)
	RetryInvocationResumeOutbox(context.Context, string, string, time.Time, string) (bool, error)
	DeleteInvocationResumeOutbox(context.Context, string) error
}

// InvocationResumeCommit describes the cross-entity write that accepts one
// tool resume. SQLite/Memory implement it as one transaction over the private
// handoff, resume outbox, Invocation CAS and resumed event. Older embedders
// can omit the optional interface and retain the compatible best-effort path.
type InvocationResumeCommit struct {
	InvocationID string
	FromStatus   InvocationStatus
	ToStatus     InvocationStatus
	Message      string
	Resume       InvocationResume
	Outbox       InvocationResumeOutbox
	Event        AgentEvent
}

type InvocationResumeCommitRepository interface {
	CommitInvocationResume(context.Context, InvocationResumeCommit) (Invocation, AgentEvent, error)
}

// ApprovalResumeCommit describes the confirmation hand-off across the
// Runtime-owned entities. Workspace Operation sidecars are intentionally not
// part of this repository transaction; rejection uses the optional
// ApprovalRejectionCommitRepository below when the storage implementation can
// include the local sidecar in the same transaction.
// SQLite/Memory commit the Runtime-owned Approval, ToolCall, Invocation,
// private handoff/outbox and ordered audit events together.
type ApprovalResumeCommit struct {
	ApprovalID           string
	ApprovalFromStatus   ApprovalStatus
	ApprovalToStatus     ApprovalStatus
	InvocationID         string
	InvocationFromStatus InvocationStatus
	InvocationToStatus   InvocationStatus
	ToolCallID           string
	ToolCallStatus       ToolCallStatus
	Reason               string
	Resume               InvocationResume
	Outbox               InvocationResumeOutbox
	// RejectionDelivery is populated only for a configured cross-service
	// rejection-intent route. Implementations that support the stronger
	// ApprovalRejectionDeliveryCommitRepository persist it in the same local
	// transaction as the Runtime decision.
	RejectionDelivery *RuntimeApprovalRejectionDeliveryOutbox `json:"-"`
	Events            []AgentEvent
}

type ApprovalResumeCommitRepository interface {
	CommitApprovalResume(context.Context, ApprovalResumeCommit) (Approval, Invocation, []AgentEvent, error)
}

// ApprovalRejectionCommitRepository is the stronger local rejection boundary.
// It has the same Runtime commit payload as ApprovalResumeCommit, but the
// implementation must also reject the Workspace Operation referenced by the
// Approval and cancel a still-queued CommandRun in the same durable
// transaction. The method is optional so older embedders retain the
// sidecar-first compatibility path.
type ApprovalRejectionCommitRepository interface {
	CommitApprovalRejection(context.Context, ApprovalResumeCommit) (Approval, Invocation, []AgentEvent, error)
}

// ApprovalRejectionDeliveryCommitRepository extends the local rejection
// transaction with the source-side cross-service intent outbox. It is a
// separate optional interface so older repositories cannot accidentally claim
// atomicity they do not provide.
type ApprovalRejectionDeliveryCommitRepository interface {
	CommitApprovalRejectionWithDelivery(context.Context, ApprovalResumeCommit, RuntimeApprovalRejectionDeliveryOutbox) (Approval, Invocation, []AgentEvent, error)
}

// InterruptedInvocationCommit describes the local restart-recovery boundary.
// When an Invocation was running without a live execution lease, Runtime must
// make the unknown side-effect boundary explicit: the Invocation becomes
// terminal and its active TaskPlan step becomes blocked in the same durable
// commit. Plan is the proposed blocked plan with its current revision; the
// repository assigns the next revision and persists the supplied audit events
// atomically. A nil Plan is valid for invocations that had no active plan step.
type InterruptedInvocationCommit struct {
	InvocationID string
	FromStatus   InvocationStatus
	ToStatus     InvocationStatus
	Message      string
	Plan         *TaskPlan
	Events       []AgentEvent
}

// InterruptedInvocationCommitRepository is optional for compatibility with
// embedders. SQLite and Memory implement it so restart recovery cannot leave a
// terminal Invocation paired with an apparently runnable plan step.
type InterruptedInvocationCommitRepository interface {
	CommitInterruptedInvocation(context.Context, InterruptedInvocationCommit) (Invocation, *TaskPlan, []AgentEvent, error)
}

// InvocationLeaseRepository is optional for compatibility with embedders.
// Production SQLite and the deterministic MemoryRepository implement an
// atomic short lease so multiple processes cannot execute one Invocation at
// the same time. Lease metadata is never exposed to clients.
type InvocationLeaseRepository interface {
	AcquireInvocationLease(context.Context, string, string, time.Time, time.Duration) (bool, error)
	RenewInvocationLease(context.Context, string, string, time.Time, time.Duration) (bool, error)
	ReleaseInvocationLease(context.Context, string, string) (bool, error)
}

// ToolCallRepository is optional for compatibility with embedders that only
// need the Invocation/Event core. The production SQLite repository and the
// in-memory test repository implement it.
type ToolCallRepository interface {
	CreateToolCall(context.Context, ToolCall) error
	GetToolCall(context.Context, string) (ToolCall, error)
	ListToolCalls(context.Context, string) ([]ToolCall, error)
	UpdateToolCall(context.Context, ToolCall) error
}

// TaskPlanRepository is optional for compatibility with embedders that only
// need the original Invocation/Event/Approval core. The SQLite and memory
// repositories implement it for the P1 structured-plan slice.
type TaskPlanRepository interface {
	CreateTaskPlan(context.Context, TaskPlan) error
	GetTaskPlan(context.Context, string) (TaskPlan, error)
	UpdateTaskPlan(context.Context, TaskPlan, int64) (TaskPlan, error)
}

// WorkflowContinuationCommitRepository is the stronger local boundary for
// user-confirmed continuation of a Runtime-recovered workflow. Built-in
// repositories commit source audit, child Invocation, child TaskPlan and
// child queued event(s) atomically; older embedders must fail closed.
type WorkflowContinuationCommitRepository interface {
	CommitWorkflowContinuation(context.Context, WorkflowContinuationCommit) (Invocation, TaskPlan, []AgentEvent, error)
}

// TaskContractRepository is optional for embedders that only need the
// Invocation/Event core. Implementations keep every accepted version so a
// later user correction never destroys the original task boundary.
type TaskContractRepository interface {
	CreateTaskContract(context.Context, TaskContract) error
	GetTaskContract(context.Context, string) (TaskContract, error)
	ListTaskContracts(context.Context, string) ([]TaskContract, error)
	UpdateTaskContract(context.Context, TaskContract, int64) (TaskContract, error)
}

// TaskContractSaver is a compatibility helper for small embedders that want
// to persist a contract without exposing the coordinator's CAS API.
type TaskContractSaver interface {
	SaveTaskContract(context.Context, TaskContract) error
}

// ContextManifest and its item types are defined at the Kernel boundary and
// aliased here so Runtime persistence can stay provider-neutral without an
// import cycle.
type ContextManifest = agent.ContextManifest
type ContextManifestItem = agent.ContextManifestItem
type ContextExcludedItem = agent.ContextExcludedItem

type ContextManifestRepository interface {
	SaveContextManifest(context.Context, ContextManifest) error
	GetContextManifest(context.Context, string) (ContextManifest, error)
	ListContextManifests(context.Context, string) ([]ContextManifest, error)
}

type ToolSetSnapshot = agent.ToolSetSnapshot
type ToolRef = agent.ToolRef
type ToolSelectionExclusion = agent.ToolSelectionExclusion

// ToolSetSnapshotRepository persists the immutable descriptor set selected
// for an Invocation. Implementations must return ErrConflict when a later
// run attempts to replace it with a different digest.
type ToolSetSnapshotRepository interface {
	GetToolSetSnapshot(context.Context, string) (ToolSetSnapshot, error)
	SaveToolSetSnapshot(context.Context, ToolSetSnapshot) (ToolSetSnapshot, error)
}

type ModelCapabilitySnapshot = provider.CapabilitySnapshot

type ModelCapabilitySnapshotRepository interface {
	GetModelCapabilitySnapshot(context.Context, string) (ModelCapabilitySnapshot, error)
	SaveModelCapabilitySnapshot(context.Context, ModelCapabilitySnapshot) (ModelCapabilitySnapshot, error)
}

type WorkingSet = agent.WorkingSet
type WorkingSetItem = agent.WorkingSetItem
type FileSlice = agent.FileSlice
type WorkingSetExcluded = agent.WorkingSetExcluded
type WorkingSetSelection = agent.WorkingSetSelection

type WorkingSetRepository interface {
	GetWorkingSet(context.Context, string) (WorkingSet, error)
	ReplaceWorkingSet(context.Context, WorkingSet) (WorkingSet, error)
}

type VerificationRepository interface {
	CreateVerificationRun(context.Context, VerificationRun) error
	GetVerificationRun(context.Context, string) (VerificationRun, error)
	ListVerificationRuns(context.Context, string) ([]VerificationRun, error)
	UpdateVerificationRun(context.Context, VerificationRun, int64) (VerificationRun, error)
}

type RuntimeSnapshotRepository interface {
	GetRuntimeSnapshot(context.Context, string) (RuntimeSnapshot, error)
	SaveRuntimeSnapshot(context.Context, RuntimeSnapshot) (RuntimeSnapshot, error)
}

// RuntimeSnapshotCommitRepository is an optional stronger persistence
// boundary. Implementations commit the rebuilt Runtime Snapshot and its
// runtime.snapshot timeline event together, then return the exact rows that
// may be published to subscribers. Older embedders can keep implementing only
// RuntimeSnapshotRepository and use the compatible best-effort event path.
type RuntimeSnapshotCommitRepository interface {
	CommitRuntimeSnapshot(context.Context, RuntimeSnapshot, AgentEvent) (RuntimeSnapshot, AgentEvent, error)
}

// RuntimeSnapshotCheckpointCommitRepository is the stronger local boundary
// used when a checkpoint transport is configured. It commits the Snapshot,
// its runtime.snapshot event/outbox cursor and the checkpoint outbox in one
// transaction. The source/destination are explicit routing metadata; the
// transport secret remains owned by the transport implementation.
type RuntimeSnapshotCheckpointCommitRepository interface {
	CommitRuntimeSnapshotWithCheckpoint(context.Context, RuntimeSnapshot, AgentEvent, string, string) (RuntimeSnapshot, AgentEvent, RuntimeCheckpointDeliveryOutbox, error)
}

// WorktreeBaselineRepository persists the immutable repository fact captured
// at Invocation admission. It is optional for compatibility with small
// embedders that only need the original Invocation/Event core.
type WorktreeBaselineRepository interface {
	GetWorkspaceBaseline(context.Context, string) (WorktreeBaseline, error)
	SaveWorkspaceBaseline(context.Context, WorktreeBaseline) (WorktreeBaseline, error)
}

// WorkspaceOperationResolver loads the durable operations belonging to one
// Invocation. It is intentionally a read-only projection hook: Runtime uses
// it to explain which paths were touched, while the Workspace service remains
// the owner of operation bodies and side effects.
type WorkspaceOperationResolver func(context.Context, string, string) ([]workspace.Operation, error)

// RuntimeEventOutboxHandler receives an immutable event after the Coordinator
// claims its durable delivery cursor. The handler should publish using
// EventID as its idempotency key and must not mutate Runtime state. Returning
// an error schedules a bounded retry; a successful handler is acknowledged
// separately, so a crash between the two operations intentionally remains
// at-least-once.
type RuntimeEventOutboxHandler func(context.Context, RuntimeEventOutbox, AgentEvent) error

// WorkspaceBaselineRepository is a descriptive alias retained for callers
// that use “workspace” rather than “worktree” in their domain vocabulary.
type WorkspaceBaselineRepository = WorktreeBaselineRepository

// InstructionSnapshot is a bounded, digest-addressed record of the project
// instructions used for one Invocation. The content is intentionally kept
// bounded here so an approval can be revalidated without trusting stale model
// context; larger content can move to Artifact storage later.
type InstructionSnapshot struct {
	Path          string    `json:"path"`
	ScopePath     string    `json:"scope_path"`
	Source        string    `json:"source"`
	ContentDigest string    `json:"content_digest"`
	Content       string    `json:"content,omitempty"`
	Priority      int       `json:"priority"`
	LoadedAt      time.Time `json:"loaded_at"`
}

// InstructionSnapshotSet is the latest instruction view used by an
// Invocation. Replacing the set is atomic; the previous digest remains
// auditable through the preceding plan/approval/event records.
type InstructionSnapshotSet struct {
	InvocationID string                `json:"invocation_id"`
	TargetPath   string                `json:"target_path,omitempty"`
	Revision     int64                 `json:"revision"`
	Snapshots    []InstructionSnapshot `json:"snapshots"`
	UpdatedAt    time.Time             `json:"updated_at"`
}

// InstructionSnapshotRepository is optional so existing embedders can keep
// implementing the original Runtime Repository interface.
type InstructionSnapshotRepository interface {
	GetInstructionSnapshotSet(context.Context, string) (InstructionSnapshotSet, error)
	ReplaceInstructionSnapshotSet(context.Context, InstructionSnapshotSet) (InstructionSnapshotSet, error)
}

// InstructionSnapshotValidator is invoked immediately before an approval is
// resolved. It must only re-read and compare facts; it must never execute a
// side effect.
type InstructionSnapshotValidator func(context.Context, string) error

// InstructionSnapshotReconfirmer explicitly accepts the currently discovered
// project instructions for a pending approval. It is deliberately separate
// from the validator so a read-only check can never mutate the approval's
// trust boundary by accident.
type InstructionSnapshotReconfirmer func(context.Context, string) (InstructionSnapshotSet, error)

// Coordinator executes tasks on an application-owned context. It never uses a
// request context for the actual Agent run, so a browser disconnect does not
// cancel the model/tool loop.
type Coordinator struct {
	kernel *agent.Kernel
	repo   Repository

	mu             sync.Mutex
	admissionMu    sync.Mutex
	approvalMu     sync.Mutex
	planMu         sync.Mutex
	contractMu     sync.Mutex
	workingSetMu   sync.Mutex
	verificationMu sync.Mutex
	snapshotMu     sync.Mutex
	appendMu       sync.Mutex
	rootCtx        context.Context
	rootCancel     context.CancelFunc
	runs           map[string]runHandle
	runSequence    uint64
	subscribers    map[string]map[chan AgentEvent]struct{}
	started        bool
	closed         bool
	// approvalDecision is invoked for the small amount of domain state that
	// lives outside Runtime (currently legacy Workspace Operation status).
	// It runs before the Approval CAS for rejection so a failed sidecar update
	// cannot leave an apparently resolved approval with a pending operation.
	approvalDecision                func(context.Context, Approval, bool) error
	approvalCancelled               func(context.Context, Approval) error
	approvalExpired                 func(context.Context, Approval) error
	instructionValidator            InstructionSnapshotValidator
	instructionReconfirmer          InstructionSnapshotReconfirmer
	baselineResolver                WorktreeBaselineResolver
	operationResolver               WorkspaceOperationResolver
	approvalTTL                     time.Duration
	expiryOnce                      sync.Once
	resumeOutboxOnce                sync.Once
	deliveryOutboxOnce              sync.Once
	eventOutboxHandler              RuntimeEventOutboxHandler
	eventDeliveryTransport          RuntimeEventDeliveryTransport
	eventOutboxWorkerID             string
	deliveryGroupWorkerID           string
	deliveryGroupSettlementWorkerID string
	deliveryCompensationWorkerID    string
	deliveryGroupTransport          RuntimeDeliveryGroupTransactionalTransport
	checkpointDeliveryTransport     RuntimeCheckpointDeliveryTransport
	checkpointDeliverySource        string
	checkpointDeliveryDestination   string
	checkpointOutboxWorkerID        string
	configDeliveryTransport         RuntimeConfigDeliveryTransport
	configDeliverySource            string
	configDeliveryDestination       string
	configOutboxWorkerID            string
	configDirectoryTransport        RuntimeConfigDirectoryTransport
	configDirectorySource           string
	configDirectoryDestination      string
	configDirectoryOutboxWorkerID   string
	rebindApplyTransport            RuntimeConfigDirectoryRebindApplyTransport
	rebindApplyTransports           map[string]RuntimeConfigDirectoryRebindApplyTransport
	rebindApplySource               string
	rebindApplyDestination          string
	rebindApplyWorkerID             string
	rebindRouteCatalog              RuntimeConfigDirectoryRebindRouteCatalog
	rejectionDeliveryTransport      RuntimeApprovalRejectionDeliveryTransport
	rejectionDeliverySource         string
	rejectionDeliveryDestination    string
	rejectionOutboxWorkerID         string
	toolStatusResolver              RuntimeLongRunningToolStatusResolver
	toolStatusSource                string
	toolStatusDestination           string
}

// runHandle protects the replacement window between a paused worker and its
// queued resume. Comparing the generation in the deferred cleanup prevents an
// older worker from deleting the cancel handle of a newer resume worker.
type runHandle struct {
	cancel     context.CancelFunc
	generation uint64
}

const (
	// InvocationLeaseTTL is intentionally short enough for another process to
	// recover an abandoned task, yet long enough to cover a normal provider
	// request between heartbeats.
	InvocationLeaseTTL      = 2 * time.Minute
	InvocationLeaseInterval = 30 * time.Second
	// runtimeEventPageSize matches the largest page accepted by the built-in
	// repositories. Long-running workflows can exceed one page, so recovery
	// code must walk pages by sequence instead of silently seeing only the
	// first 5,000 events.
	runtimeEventPageSize = 5000
	// maxRuntimeEventReplay keeps a hostile or unbounded event stream from
	// turning a recovery request into an unbounded allocation. Callers receive
	// ErrEventReplayLimit rather than a partial, executable state projection.
	maxRuntimeEventReplay = 100000
)

// SetApprovalDecisionHandler lets older embedders mirror a rejected approval
// into a domain-specific prepared operation when their Repository does not
// implement ApprovalRejectionCommitRepository. The callback must not execute
// a newly approved side effect; the resumed tool handler owns that step.
func (c *Coordinator) SetApprovalDecisionHandler(handler func(context.Context, Approval, bool) error) {
	c.mu.Lock()
	c.approvalDecision = handler
	c.mu.Unlock()
}

// SetApprovalCancellationHandler mirrors Invocation cancellation into a
// prepared side-effect record without treating it as a user rejection.
func (c *Coordinator) SetApprovalCancellationHandler(handler func(context.Context, Approval) error) {
	c.mu.Lock()
	c.approvalCancelled = handler
	c.mu.Unlock()
}

// SetApprovalExpiryHandler mirrors an expired approval into a prepared
// side-effect record. Like the other sidecar hooks it must only update
// durable state; it must never execute the operation.
func (c *Coordinator) SetApprovalExpiryHandler(handler func(context.Context, Approval) error) {
	c.mu.Lock()
	c.approvalExpired = handler
	c.mu.Unlock()
}

// SetInstructionSnapshotValidator installs the read-only check used before a
// pending approval is committed. A changed instruction file makes the
// approval stale and leaves the approval/operation unresolved for an explicit
// retry or new invocation.
func (c *Coordinator) SetInstructionSnapshotValidator(validator InstructionSnapshotValidator) {
	c.mu.Lock()
	c.instructionValidator = validator
	c.mu.Unlock()
}

// SetInstructionSnapshotReconfirmer installs the explicit user-confirmation
// path used after an instruction conflict. The callback must only replace the
// bounded snapshot; it must not approve or execute a tool.
func (c *Coordinator) SetInstructionSnapshotReconfirmer(reconfirmer InstructionSnapshotReconfirmer) {
	c.mu.Lock()
	c.instructionReconfirmer = reconfirmer
	c.mu.Unlock()
}

// SetWorktreeBaselineResolver installs the read-only capture used when a new
// Invocation names a workspace. The resolver must not mutate the worktree or
// execute a user-requested command.
func (c *Coordinator) SetWorktreeBaselineResolver(resolver WorktreeBaselineResolver) {
	c.mu.Lock()
	c.baselineResolver = resolver
	c.mu.Unlock()
}

// SetWorkspaceBaselineResolver is the naming counterpart used by app
// assembly code; both setters update the same resolver.
func (c *Coordinator) SetWorkspaceBaselineResolver(resolver WorktreeBaselineResolver) {
	c.SetWorktreeBaselineResolver(resolver)
}

// SetWorkspaceOperationResolver installs the read-only operation lookup used
// by completion reports. The resolver must return only metadata needed for
// attribution or rely on Runtime's projection to discard operation bodies.
func (c *Coordinator) SetWorkspaceOperationResolver(resolver WorkspaceOperationResolver) {
	c.mu.Lock()
	c.operationResolver = resolver
	c.mu.Unlock()
}

// SetRuntimeEventOutboxHandler enables optional asynchronous delivery of the
// durable event outbox. Without a handler, repositories still expose the
// claim/ack API for a separately managed consumer. Setting the handler after
// Start is supported and starts the loop exactly once.
func (c *Coordinator) SetRuntimeEventOutboxHandler(handler RuntimeEventOutboxHandler) {
	c.mu.Lock()
	c.eventOutboxHandler = handler
	// A direct handler is intentionally independent from the signed transport
	// adapter. Clearing the transport prevents a previously configured remote
	// reconciler from being reused after a host switches back to a local hook.
	c.eventDeliveryTransport = nil
	started := c.started && !c.closed
	c.mu.Unlock()
	if started && handler != nil {
		c.startRuntimeEventOutboxLoop()
	}
}

// SetRuntimeEventDeliveryTransport adapts a signed cross-service transport to
// the existing durable outbox loop. The transport is optional and must be
// explicitly configured; nil disables the adapter while preserving the
// repository cursor for separately managed consumers.
func (c *Coordinator) SetRuntimeEventDeliveryTransport(transport RuntimeEventDeliveryTransport) {
	if transport == nil {
		c.mu.Lock()
		c.eventDeliveryTransport = nil
		c.eventOutboxHandler = nil
		c.mu.Unlock()
		return
	}
	handler := RuntimeEventOutboxHandler(func(ctx context.Context, outbox RuntimeEventOutbox, event AgentEvent) error {
		transactional, transactionOK := transport.(RuntimeEventDeliveryTransactionalTransport)
		if marker, marked := transport.(RuntimeEventDeliveryTransactionalOptIn); marked && !marker.RuntimeEventDeliveryTransactionsEnabled() {
			transactionOK = false
		}
		if transactionOK {
			envelope, err := transactional.BuildEnvelope(outbox, event)
			if err != nil {
				return err
			}
			prepared, err := transactional.Prepare(ctx, envelope)
			if err != nil {
				return err
			}
			if err := prepared.ValidateAgainstPhase(envelope, RuntimeEventDeliveryTransactionPhasePrepared); err != nil {
				return err
			}
			committed, err := transactional.Commit(ctx, envelope)
			if err != nil {
				return err
			}
			return committed.ValidateAgainstPhase(envelope, RuntimeEventDeliveryTransactionPhaseCommitted)
		}
		return transport.Deliver(ctx, outbox, event)
	})
	c.mu.Lock()
	c.eventDeliveryTransport = transport
	c.eventOutboxHandler = handler
	started := c.started && !c.closed
	c.mu.Unlock()
	if started {
		c.startRuntimeEventOutboxLoop()
	}
}

// SetRuntimeCheckpointDeliveryTransport enables the explicit checkpoint
// outbox loop. The source and destination are signed into every envelope;
// passing an empty route or nil transport disables this optional path. It is
// deliberately independent from RuntimeEventDeliveryTransport so event
// delivery and checkpoint delivery cannot accidentally share idempotency
// keys or payload policies.
func (c *Coordinator) SetRuntimeCheckpointDeliveryTransport(source, destination string, transport RuntimeCheckpointDeliveryTransport) {
	c.mu.Lock()
	c.checkpointDeliverySource = strings.TrimSpace(source)
	c.checkpointDeliveryDestination = strings.TrimSpace(destination)
	c.checkpointDeliveryTransport = transport
	started := c.started && !c.closed
	c.mu.Unlock()
	if started && transport != nil && strings.TrimSpace(source) != "" && strings.TrimSpace(destination) != "" {
		c.startRuntimeCheckpointOutboxLoop()
	}
}

// SetRuntimeConfigDeliveryTransport enables the metadata-only cross-Runtime
// configuration lock outbox. The route is explicit and independent from event
// and checkpoint delivery; it never starts a Worker at the destination.
func (c *Coordinator) SetRuntimeConfigDeliveryTransport(source, destination string, transport RuntimeConfigDeliveryTransport) {
	c.mu.Lock()
	c.configDeliverySource = strings.TrimSpace(source)
	c.configDeliveryDestination = strings.TrimSpace(destination)
	c.configDeliveryTransport = transport
	started := c.started && !c.closed
	c.mu.Unlock()
	if started && transport != nil && strings.TrimSpace(source) != "" && strings.TrimSpace(destination) != "" {
		c.startRuntimeDeliveryOutboxLoop()
	}
}

// SetRuntimeConfigDirectoryTransport enables the source-side durable
// configuration-directory body outbox. It is deliberately separate from the
// metadata-only config lock transport and must be explicitly route-bound.
func (c *Coordinator) SetRuntimeConfigDirectoryTransport(source, destination string, transport RuntimeConfigDirectoryTransport) {
	c.mu.Lock()
	c.configDirectorySource = strings.TrimSpace(source)
	c.configDirectoryDestination = strings.TrimSpace(destination)
	c.configDirectoryTransport = transport
	started := c.started && !c.closed
	c.mu.Unlock()
	if started && transport != nil && strings.TrimSpace(source) != "" && strings.TrimSpace(destination) != "" {
		c.startRuntimeDeliveryOutboxLoop()
	}
}

// SetRuntimeDeliveryGroupTransactionTransport enables the opt-in remote group
// settlement adapter. The adapter carries only signed group/member metadata;
// family payloads remain on their existing delivery channels. Settlement is
// attempted after the local group reaches a terminal state and is therefore
// an outcome/compensation ledger, not an unqualified cross-service atomic
// transaction claim.
func (c *Coordinator) SetRuntimeDeliveryGroupTransactionTransport(transport RuntimeDeliveryGroupTransactionalTransport) {
	c.mu.Lock()
	c.deliveryGroupTransport = transport
	started := c.started && !c.closed
	c.mu.Unlock()
	if started && transport != nil {
		c.startRuntimeDeliveryOutboxLoop()
	}
}

// SetRuntimeApprovalRejectionDeliveryTransport enables the opt-in,
// metadata-only rejection-intent outbox. The source/destination route is
// signed into every envelope and is also used to reject a stale cursor after
// configuration changes. A transport is not enough on its own: ResolveApproval
// requires both a durable rejection outbox and an atomic local rejection
// commit, otherwise it fails closed before changing approval state.
func (c *Coordinator) SetRuntimeApprovalRejectionDeliveryTransport(source, destination string, transport RuntimeApprovalRejectionDeliveryTransport) {
	c.mu.Lock()
	c.rejectionDeliverySource = strings.TrimSpace(source)
	c.rejectionDeliveryDestination = strings.TrimSpace(destination)
	c.rejectionDeliveryTransport = transport
	started := c.started && !c.closed
	c.mu.Unlock()
	if started && transport != nil && strings.TrimSpace(source) != "" && strings.TrimSpace(destination) != "" {
		c.startRuntimeApprovalRejectionDeliveryOutboxLoop()
	}
}

func (c *Coordinator) runtimeCheckpointDeliveryConfig() (source, destination string, transport RuntimeCheckpointDeliveryTransport, enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	source = c.checkpointDeliverySource
	destination = c.checkpointDeliveryDestination
	transport = c.checkpointDeliveryTransport
	enabled = transport != nil && source != "" && destination != ""
	return source, destination, transport, enabled
}

func (c *Coordinator) runtimeConfigDeliveryConfig() (source, destination string, transport RuntimeConfigDeliveryTransport, enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	source = c.configDeliverySource
	destination = c.configDeliveryDestination
	transport = c.configDeliveryTransport
	enabled = transport != nil && source != "" && destination != ""
	return source, destination, transport, enabled
}

func (c *Coordinator) runtimeConfigDirectoryDeliveryConfig() (source, destination string, transport RuntimeConfigDirectoryTransport, enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	source = c.configDirectorySource
	destination = c.configDirectoryDestination
	transport = c.configDirectoryTransport
	enabled = transport != nil && source != "" && destination != ""
	return source, destination, transport, enabled
}

func (c *Coordinator) runtimeApprovalRejectionDeliveryConfig() (source, destination string, transport RuntimeApprovalRejectionDeliveryTransport, enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	source = c.rejectionDeliverySource
	destination = c.rejectionDeliveryDestination
	transport = c.rejectionDeliveryTransport
	enabled = transport != nil && source != "" && destination != ""
	return source, destination, transport, enabled
}

func NewCoordinator(kernel *agent.Kernel, repo Repository) (*Coordinator, error) {
	if kernel == nil {
		return nil, errors.New("Agent Kernel 不能为空")
	}
	if repo == nil {
		return nil, errors.New("运行时 Repository 不能为空")
	}
	coordinator := &Coordinator{
		kernel:                          kernel,
		repo:                            repo,
		runs:                            make(map[string]runHandle),
		subscribers:                     make(map[string]map[chan AgentEvent]struct{}),
		approvalTTL:                     24 * time.Hour,
		eventOutboxWorkerID:             newID("event-outbox-worker"),
		deliveryGroupWorkerID:           newID("runtime-delivery-group-worker"),
		deliveryGroupSettlementWorkerID: newID("runtime-delivery-group-settlement-worker"),
		deliveryCompensationWorkerID:    newID("runtime-delivery-compensation-worker"),
		checkpointOutboxWorkerID:        newID("checkpoint-outbox-worker"),
		configOutboxWorkerID:            newID("runtime-config-outbox-worker"),
		configDirectoryOutboxWorkerID:   newID("runtime-config-directory-outbox-worker"),
		rebindApplyWorkerID:             newID("runtime-rebind-apply-worker"),
		rebindApplyTransports:           make(map[string]RuntimeConfigDirectoryRebindApplyTransport),
		rejectionOutboxWorkerID:         newID("approval-rejection-outbox-worker"),
		toolStatusSource:                DefaultRuntimeLongRunningToolStatusSource,
		toolStatusDestination:           DefaultRuntimeLongRunningToolStatusDestination,
	}
	kernel.SetToolBudgetObserver(func(ctx context.Context, invocationID string, used, limit int) {
		if strings.TrimSpace(invocationID) == "" {
			return
		}
		_, _ = coordinator.appendAndPublish(context.WithoutCancel(ctx), AgentEvent{
			ID: newID("event"), InvocationID: strings.TrimSpace(invocationID), Type: EventRuntimeNotice, Timestamp: time.Now().UTC(),
			Data: map[string]any{"code": "tool_budget_exhausted", "used": used, "limit": limit},
		})
	})
	kernel.SetContextRecoveryObserver(func(ctx context.Context, invocationID string, notice agent.ContextRecoveryNotice) {
		invocationID = strings.TrimSpace(invocationID)
		if invocationID == "" {
			return
		}
		data := map[string]any{
			"code": "context_limit_exceeded", "attempt": notice.Attempt, "recovered": notice.Recovered,
		}
		if modelCallID := strings.TrimSpace(notice.ModelCallID); modelCallID != "" {
			data["model_call_id"] = modelCallID
		}
		_, _ = coordinator.appendAndPublish(context.WithoutCancel(ctx), AgentEvent{
			ID: newID("event"), InvocationID: invocationID, Type: EventRuntimeNotice, Timestamp: time.Now().UTC(), Data: data,
		})
	})
	kernel.SetCompactionFailureObserver(func(ctx context.Context, invocationID string, compactionErr error, consecutive int) {
		event := compactionFailureEvent(invocationID, compactionErr, consecutive, time.Now().UTC())
		if event.InvocationID == "" {
			return
		}
		_, _ = coordinator.appendAndPublish(context.WithoutCancel(ctx), event)
	})
	kernel.SetCompactionUsageObserver(func(ctx context.Context, invocationID string, usage map[string]any) {
		invocationID = strings.TrimSpace(invocationID)
		if invocationID == "" {
			return
		}
		data := map[string]any{"scope": "compaction"}
		if usage != nil {
			data["usage"] = usage
		}
		_, _ = coordinator.appendAndPublish(context.WithoutCancel(ctx), AgentEvent{
			ID: newID("event"), InvocationID: invocationID, Type: EventUsageUpdated, Timestamp: time.Now().UTC(), Data: data,
		})
	})
	kernel.SetModalFallbackUsageObserver(func(ctx context.Context, invocationID string, metadata map[string]any) {
		invocationID = strings.TrimSpace(invocationID)
		if invocationID == "" {
			return
		}
		data := map[string]any{"scope": "modal_fallback"}
		for key, value := range metadata {
			if key == "scope" {
				continue
			}
			data[key] = value
		}
		_, _ = coordinator.appendAndPublish(context.WithoutCancel(ctx), AgentEvent{
			ID: newID("event"), InvocationID: invocationID, Type: EventUsageUpdated, Timestamp: time.Now().UTC(), Data: data,
		})
	})
	kernel.SetModalFallbackOutputObserver(func(ctx context.Context, invocationID, text string, metadata map[string]any) {
		invocationID = strings.TrimSpace(invocationID)
		text = strings.TrimSpace(text)
		if invocationID == "" || text == "" {
			return
		}
		data := map[string]any{"text": text, "scope": "modal_fallback", "source": "modal_fallback"}
		for key, value := range metadata {
			if key == "text" || key == "scope" || key == "source" {
				continue
			}
			data[key] = value
		}
		_, _ = coordinator.appendAndPublish(context.WithoutCancel(ctx), AgentEvent{
			ID: newID("event"), InvocationID: invocationID, Type: EventAssistantMessage, Timestamp: time.Now().UTC(), Data: data,
		})
	})
	kernel.SetToolExecutionObserver(func(ctx context.Context, record agent.ToolExecutionRecord) {
		invocationID := workspace.InvocationIDFromContext(ctx)
		// The ADK context does not expose the product Invocation ID directly;
		// workspace runs carry it through the same private context key used by
		// the command observer. Standalone Kernel callers simply skip this audit.
		if invocationID == "" {
			return
		}
		data := map[string]any{
			"code": "tool_execution", "tool_id": record.ToolID, "version": record.Version,
			"model_name": record.ModelName, "call_id": record.CallID, "outcome": record.Outcome,
			"duration_ms": record.Duration.Milliseconds(), "result_bytes": record.ResultBytes,
			"result_truncated": record.ResultTruncated,
		}
		if record.ErrorCode != "" {
			data["error_code"] = record.ErrorCode
		}
		_, _ = coordinator.appendAndPublish(context.WithoutCancel(ctx), AgentEvent{ID: newID("event"), InvocationID: invocationID, Type: EventRuntimeNotice, Timestamp: record.FinishedAt, Data: data})
	})
	kernel.SetContextManifestObserver(func(ctx context.Context, manifest ContextManifest) {
		manifestRepo, ok := coordinator.repo.(ContextManifestRepository)
		if !ok {
			return
		}
		if err := manifestRepo.SaveContextManifest(context.WithoutCancel(ctx), manifest); err != nil {
			return
		}
		_, _ = coordinator.appendAndPublish(context.WithoutCancel(ctx), AgentEvent{
			ID: newID("event"), InvocationID: manifest.InvocationID, Type: EventContextManifest, Timestamp: manifest.CreatedAt,
			Data: map[string]any{"manifest_id": manifest.ID, "model_call_id": manifest.ModelCallID, "digest": manifest.Digest, "estimated_input": manifest.EstimatedInput, "context_window": manifest.ContextWindow, "output_reserve": manifest.OutputReserve, "safety_reserve": manifest.SafetyReserve, "warnings": manifest.Warnings},
		})
	})
	kernel.SetToolSetSnapshotObserver(func(ctx context.Context, snapshot ToolSetSnapshot) error {
		repo, ok := coordinator.repo.(ToolSetSnapshotRepository)
		if !ok {
			return nil
		}
		invocationID := strings.TrimSpace(snapshot.InvocationID)
		if invocationID == "" {
			return errors.New("ToolSet Snapshot 缺少 invocation_id")
		}
		if existing, err := repo.GetToolSetSnapshot(context.WithoutCancel(ctx), invocationID); err == nil {
			if existing.Digest != snapshot.Digest {
				return fmt.Errorf("%w: invocation=%s old=%s new=%s", agent.ErrToolSnapshotConflict, invocationID, existing.Digest, snapshot.Digest)
			}
			return nil
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		saved, err := repo.SaveToolSetSnapshot(context.WithoutCancel(ctx), snapshot)
		if err != nil {
			return err
		}
		_, err = coordinator.appendAndPublish(context.WithoutCancel(ctx), AgentEvent{ID: newID("event"), InvocationID: invocationID, Type: EventToolSetSnapshot, Timestamp: saved.CreatedAt, Data: map[string]any{"snapshot_id": saved.ID, "digest": saved.Digest, "catalog_revision": saved.CatalogRevision, "tool_count": len(saved.Tools)}})
		return err
	})
	kernel.SetModelCapabilityObserver(func(ctx context.Context, snapshot ModelCapabilitySnapshot) error {
		repo, ok := coordinator.repo.(ModelCapabilitySnapshotRepository)
		if !ok {
			return nil
		}
		invocationID := strings.TrimSpace(snapshot.InvocationID)
		if invocationID == "" {
			return errors.New("模型能力 Snapshot 缺少 invocation_id")
		}
		if existing, err := repo.GetModelCapabilitySnapshot(context.WithoutCancel(ctx), invocationID); err == nil {
			if existing.Result.ProfileSnapshotID != snapshot.Result.ProfileSnapshotID || existing.Result.RequestPlan != snapshot.Result.RequestPlan {
				return fmt.Errorf("%w: invocation=%s old=%s new=%s", ErrCapabilitySnapshotConflict, invocationID, existing.Result.ProfileSnapshotID, snapshot.Result.ProfileSnapshotID)
			}
			return nil
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		saved, err := repo.SaveModelCapabilitySnapshot(context.WithoutCancel(ctx), snapshot)
		if err != nil {
			return err
		}
		_, err = coordinator.appendAndPublish(context.WithoutCancel(ctx), AgentEvent{ID: newID("event"), InvocationID: invocationID, Type: EventModelCapabilitiesResolved, Timestamp: saved.CreatedAt, Data: map[string]any{"snapshot_id": saved.ID, "profile_snapshot_id": saved.Result.ProfileSnapshotID, "compatible": saved.Result.Compatible, "warnings": saved.Result.Warnings}})
		if err != nil {
			return err
		}
		if len(saved.Result.DisabledFeatures) > 0 {
			_, err = coordinator.appendAndPublish(context.WithoutCancel(ctx), AgentEvent{ID: newID("event"), InvocationID: invocationID, Type: EventModelRequestDowngraded, Timestamp: saved.CreatedAt, Data: map[string]any{"snapshot_id": saved.ID, "disabled_features": saved.Result.DisabledFeatures, "warnings": saved.Result.Warnings}})
		}
		return err
	})
	kernel.SetConfigSnapshotObserver(func(ctx context.Context, invocationID, encoded string) error {
		invocationID = strings.TrimSpace(invocationID)
		if invocationID == "" {
			return errors.New("Runtime 配置快照缺少 invocation_id")
		}
		parsed, err := agent.ParseRuntimeConfigSnapshot(encoded)
		if err != nil {
			return err
		}
		invocation, err := coordinator.repo.GetInvocation(ctx, invocationID)
		if err != nil {
			return err
		}
		if strings.TrimSpace(invocation.ConfigSnapshot) != "" {
			if err := agent.ValidateRuntimeConfigSnapshot(invocation.ConfigSnapshot, parsed); err != nil {
				return err
			}
			return nil
		}
		invocation.ConfigSnapshot = strings.TrimSpace(encoded)
		invocation.ConfigSnapshotDigest = agent.RuntimeConfigSnapshotDigest(encoded)
		if invocation.ConfigSnapshotDigest == "" {
			return agent.ErrInvalidRuntimeConfigSnapshot
		}
		if err := coordinator.repo.UpdateInvocation(context.WithoutCancel(ctx), invocation); err != nil {
			return err
		}
		_, err = coordinator.appendAndPublish(context.WithoutCancel(ctx), AgentEvent{
			ID: newID("event"), InvocationID: invocationID, Type: EventRuntimeConfigSnapshot, Timestamp: time.Now().UTC(),
			Data: map[string]any{"digest": invocation.ConfigSnapshotDigest, "version": parsed.Version, "provider_id": parsed.ProviderID, "model_id": parsed.ModelID, "tool_catalog_revision": parsed.ToolCatalogRevision},
		})
		return err
	})
	// Runtime owns the authoritative snapshot projection. Installing this
	// resolver here prevents an application from constructing a divergent
	// in-memory recovery view.
	kernel.SetRuntimeSnapshotResolver(func(ctx context.Context, invocationID string) (agent.RuntimeSnapshotProjection, error) {
		snapshotRepo, ok := coordinator.repo.(RuntimeSnapshotRepository)
		if !ok {
			return agent.RuntimeSnapshotProjection{}, errors.New("当前 Runtime 存储未启用 RuntimeSnapshot")
		}
		snapshot, err := snapshotRepo.GetRuntimeSnapshot(ctx, strings.TrimSpace(invocationID))
		if err != nil {
			return agent.RuntimeSnapshotProjection{}, err
		}
		return projectRuntimeSnapshot(snapshot), nil
	})
	kernel.SetWorkingSetResolver(func(ctx context.Context, invocationID string) ([]agent.WorkingSetItem, error) {
		workingSetRepo, ok := coordinator.repo.(WorkingSetRepository)
		if !ok {
			return nil, nil
		}
		workingSet, err := workingSetRepo.GetWorkingSet(ctx, strings.TrimSpace(invocationID))
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return append([]agent.WorkingSetItem(nil), workingSet.Items...), nil
	})
	return coordinator, nil
}

// SetApprovalTTL changes the default lifetime for newly created approvals.
// A non-positive value disables the expiry scanner while explicit ExpiresAt
// values are still honored when a caller persists them.
func (c *Coordinator) SetApprovalTTL(ttl time.Duration) {
	c.mu.Lock()
	c.approvalTTL = ttl
	c.mu.Unlock()
}

// Start installs the application lifecycle context and requeues tasks that
// were accepted but never started. Waiting approvals remain waiting and are
// resumed only after an explicit user decision.
func (c *Coordinator) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrConflict
	}
	if c.started {
		c.mu.Unlock()
		return nil
	}
	c.rootCtx, c.rootCancel = context.WithCancel(ctx)
	c.started = true
	c.mu.Unlock()
	if err := c.reconcileInterrupted(ctx); err != nil {
		return err
	}
	c.startApprovalExpiryLoop()

	active, err := c.repo.ListInvocations(ctx, "", []InvocationStatus{InvocationQueued})
	if err != nil {
		return fmt.Errorf("恢复排队任务失败: %w", err)
	}
	for _, invocation := range active {
		var workspaceID *string
		if strings.TrimSpace(invocation.WorkspaceID) != "" {
			value := invocation.WorkspaceID
			workspaceID = &value
		}
		c.refreshRuntimeSnapshot(ctx, invocation.ID)
		request := agent.ChatRequest{
			UserID: invocation.UserID, IdempotencyKey: invocation.IdempotencyKey, BotID: invocation.BotID, ConversationID: invocation.ConversationID,
			SessionID: invocation.SessionID, ProviderID: invocation.ProviderID, ModelID: invocation.ModelID, WorkspaceID: workspaceID, TargetPath: invocation.TargetPath,
			Message: invocation.Message, GroupContext: invocation.GroupContext, Proactive: invocation.Proactive, Attachments: cloneAttachments(invocation.Attachments), ConfigSnapshot: invocation.ConfigSnapshot, Stream: true,
		}
		if resume, resumeErr := c.resumeContentForInvocation(ctx, invocation.ID); resumeErr != nil {
			message := "runtime_recovery: 读取待恢复工具响应失败: " + resumeErr.Error()
			if ok, transitionErr := c.repo.TransitionInvocation(context.WithoutCancel(ctx), invocation.ID, InvocationQueued, InvocationFailed, message); transitionErr != nil {
				return transitionErr
			} else if ok {
				c.emitTerminal(context.WithoutCancel(ctx), invocation.ID, EventInvocationFailed, map[string]any{"error": message, "reason": "resume_handoff_read_failed"})
			}
			continue
		} else if resume != nil {
			request.ResumeContent = resume
			request.Message = ""
			request.GroupContext = ""
			request.Attachments = nil
		}
		c.launch(invocation.ID, request)
	}
	c.startResumeOutboxLoop()
	c.startRuntimeEventOutboxLoop()
	c.startRuntimeCheckpointOutboxLoop()
	c.startRuntimeApprovalRejectionDeliveryOutboxLoop()
	return nil
}

// reconcileInterrupted makes a restart observable and safe. A worker that was
// in the middle of a model/tool turn has no durable checkpoint in ADK, so it
// is failed conservatively rather than replaying a potentially duplicated
// side effect. Cancelling is terminalized without pretending the HTTP caller
// caused it.
func (c *Coordinator) reconcileInterrupted(ctx context.Context) error {
	items, err := c.repo.ListInvocations(ctx, "", []InvocationStatus{InvocationRunning, InvocationCancelling})
	if err != nil {
		return fmt.Errorf("恢复中断任务失败: %w", err)
	}
	leaseRepo, hasLease := c.repo.(InvocationLeaseRepository)
	now := time.Now().UTC()
	for _, item := range items {
		// A different process may still own a live lease during a fast restart.
		// Never fail that task from the recovery scanner; the owner will either
		// renew it or eventually lose it and make the next restart safe to handle.
		if hasLease && item.Status == InvocationRunning && item.LeaseOwner != "" && item.LeaseExpiresAt != nil && item.LeaseExpiresAt.After(now) {
			continue
		}
		status := InvocationFailed
		message := "runtime_interrupted: 服务重启时运行中的 invocation 没有可安全恢复的 checkpoint"
		if item.Status == InvocationCancelling {
			status = InvocationCancelled
			message = "服务重启时完成取消"
		}
		// SQLite/Memory can close the Invocation and any active plan step under
		// one durable boundary. Do not attempt a best-effort two-write repair in
		// the compatibility path: an older embedder may not provide the stronger
		// repository contract, so it keeps the historical Invocation-only fence.
		if atomicRepo, atomicOK := c.repo.(InterruptedInvocationCommitRepository); atomicOK {
			var blockedPlan *TaskPlan
			if planRepo, planOK := c.repo.(TaskPlanRepository); planOK {
				plan, planErr := planRepo.GetTaskPlan(ctx, item.ID)
				switch {
				case planErr == nil && plan.Status == PlanInProgress && strings.TrimSpace(plan.CurrentStepID) != "":
					blocker := "runtime_interrupted: 服务重启时无法确认上次执行是否产生副作用"
					blocked, blockErr := plan.BlockStep(plan.CurrentStepID, blocker, now)
					if blockErr != nil {
						return fmt.Errorf("恢复中断任务的活动步骤失败: %w", blockErr)
					}
					blockedPlan = &blocked
				case planErr != nil && !errors.Is(planErr, ErrNotFound):
					return fmt.Errorf("读取中断任务计划失败: %w", planErr)
				}
			}
			events := make([]AgentEvent, 0, 2)
			if blockedPlan != nil {
				events = append(events, AgentEvent{
					ID: newID("event"), InvocationID: item.ID, Type: EventPlanUpdated, Timestamp: now,
					Data: map[string]any{"reason": "runtime_recovery"},
				})
			}
			events = append(events, AgentEvent{
				ID: newID("event"), InvocationID: item.ID, Type: terminalEventForStatus(status), Timestamp: now,
				Data: map[string]any{"error": message, "reason": "runtime_recovery"},
			})
			_, _, storedEvents, commitErr := atomicRepo.CommitInterruptedInvocation(ctx, InterruptedInvocationCommit{
				InvocationID: item.ID, FromStatus: item.Status, ToStatus: status, Message: message,
				Plan: blockedPlan, Events: events,
			})
			if commitErr != nil {
				// A concurrent recovery may have won the CAS after ListInvocations.
				// Treat that race as an idempotent no-op, but surface all other
				// failures so startup cannot silently lose the recovery fence.
				if errors.Is(commitErr, ErrConflict) {
					current, getErr := c.repo.GetInvocation(ctx, item.ID)
					if getErr != nil {
						return getErr
					}
					if current.Status != item.Status {
						continue
					}
				}
				return commitErr
			}
			for _, storedEvent := range storedEvents {
				c.publishStoredEvent(storedEvent)
			}
			c.refreshRuntimeSnapshot(context.WithoutCancel(ctx), item.ID)
			continue
		}
		if ok, transitionErr := c.repo.TransitionInvocation(ctx, item.ID, item.Status, status, message); transitionErr != nil {
			return transitionErr
		} else if ok {
			if hasLease && item.LeaseOwner != "" {
				_, _ = leaseRepo.ReleaseInvocationLease(context.WithoutCancel(ctx), item.ID, item.LeaseOwner)
			}
			c.refreshRuntimeSnapshot(context.WithoutCancel(ctx), item.ID)
			c.emitTerminal(ctx, item.ID, terminalEventForStatus(status), map[string]any{"error": message, "reason": "runtime_recovery"})
		}
	}
	return nil
}

func (c *Coordinator) startApprovalExpiryLoop() {
	c.expiryOnce.Do(func() {
		c.mu.Lock()
		root := c.rootCtx
		ttl := c.approvalTTL
		c.mu.Unlock()
		if root == nil || ttl <= 0 {
			return
		}
		interval := ttl / 4
		if interval < time.Second {
			interval = time.Second
		}
		if interval > time.Minute {
			interval = time.Minute
		}
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-root.Done():
					return
				case now := <-ticker.C:
					_ = c.ExpireApprovals(context.WithoutCancel(root), now.UTC())
				}
			}
		}()
	})
}

// startResumeOutboxLoop periodically re-dispatches only due durable resume
// records. Normal queued invocations are launched by Start; this loop exists
// for the retry/lease-expiry path where the first Runner could not claim or
// finish a resume delivery. The local run map prevents replacing an active
// worker while the repository lease remains the cross-process authority.
func (c *Coordinator) startResumeOutboxLoop() {
	c.resumeOutboxOnce.Do(func() {
		c.mu.Lock()
		root := c.rootCtx
		c.mu.Unlock()
		if root == nil {
			return
		}
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-root.Done():
					return
				case now := <-ticker.C:
					c.dispatchDueResumeOutbox(context.WithoutCancel(root), now.UTC())
				}
			}
		}()
	})
}

func (c *Coordinator) dispatchDueResumeOutbox(ctx context.Context, now time.Time) {
	outboxRepo, ok := c.repo.(InvocationResumeOutboxRepository)
	if !ok {
		return
	}
	invocations, err := c.repo.ListInvocations(ctx, "", []InvocationStatus{InvocationQueued})
	if err != nil {
		return
	}
	for _, invocation := range invocations {
		item, getErr := outboxRepo.GetInvocationResumeOutbox(ctx, invocation.ID)
		if getErr != nil {
			continue
		}
		if item.Status != InvocationResumeOutboxQueued && item.Status != InvocationResumeOutboxProcessing {
			continue
		}
		if item.Status == InvocationResumeOutboxQueued && item.AvailableAt.After(now) {
			continue
		}
		if item.Status == InvocationResumeOutboxProcessing && item.LeaseExpiresAt != nil && item.LeaseExpiresAt.After(now) {
			continue
		}
		c.mu.Lock()
		_, active := c.runs[invocation.ID]
		closed := c.closed
		c.mu.Unlock()
		if active || closed {
			continue
		}
		var workspaceID *string
		if strings.TrimSpace(invocation.WorkspaceID) != "" {
			value := invocation.WorkspaceID
			workspaceID = &value
		}
		request := agent.ChatRequest{
			UserID: invocation.UserID, IdempotencyKey: invocation.IdempotencyKey, BotID: invocation.BotID, ConversationID: invocation.ConversationID,
			SessionID: invocation.SessionID, ProviderID: invocation.ProviderID, ModelID: invocation.ModelID, WorkspaceID: workspaceID, TargetPath: invocation.TargetPath,
			ConfigSnapshot: invocation.ConfigSnapshot, Proactive: invocation.Proactive, Stream: true,
		}
		if resume, resumeErr := c.resumeContentForInvocation(ctx, invocation.ID); resumeErr != nil {
			continue
		} else if resume != nil {
			request.ResumeContent = resume
		}
		c.launch(invocation.ID, request)
	}
}

// startRuntimeEventOutboxLoop asks the shared delivery scheduler to start. Event
// and checkpoint cursors intentionally share one lifecycle loop so a burst of
// one delivery kind cannot create an unbounded number of independent workers.
func (c *Coordinator) startRuntimeEventOutboxLoop() {
	c.startRuntimeDeliveryOutboxLoop()
}

// beginRuntimeDeliveryAttempt starts or resumes the shared source-side phase
// journal.  The journal is optional for compatibility, but built-in
// repositories make it durable so a process crash can resume from the latest
// destination proof instead of blindly replaying a whole transaction.
func (c *Coordinator) beginRuntimeDeliveryAttempt(ctx context.Context, kind RuntimeDeliveryKind, source, destination, deliveryID, invocationID, outboxID string, outboxRevision int64, attempt int, owner string, now time.Time) (RuntimeDeliveryAttempt, bool, error) {
	repo, ok := c.repo.(RuntimeDeliveryAttemptRepository)
	if !ok {
		return RuntimeDeliveryAttempt{}, false, nil
	}
	candidate, err := NewRuntimeDeliveryAttempt(kind, source, destination, deliveryID, invocationID, outboxID, outboxRevision, attempt, owner, now, 30*time.Second)
	if err != nil {
		return RuntimeDeliveryAttempt{}, true, err
	}
	item, err := repo.BeginRuntimeDeliveryAttempt(ctx, candidate, now, 30*time.Second)
	if err != nil {
		return RuntimeDeliveryAttempt{}, true, err
	}
	return item, true, nil
}

func (c *Coordinator) advanceRuntimeDeliveryAttempt(ctx context.Context, attempt RuntimeDeliveryAttempt, owner string, phase RuntimeDeliveryAttemptPhase, now time.Time, message string) (RuntimeDeliveryAttempt, bool, error) {
	repo, ok := c.repo.(RuntimeDeliveryAttemptRepository)
	if !ok {
		return attempt, false, nil
	}
	updated, duplicate, err := repo.AdvanceRuntimeDeliveryAttempt(ctx, attempt.ID, owner, attempt.Revision, phase, now, message)
	if err != nil {
		return RuntimeDeliveryAttempt{}, false, err
	}
	return updated, duplicate, nil
}

// ListRuntimeDeliveryAttempts exposes a bounded, metadata-only recovery view
// for API/UI consumers. It never returns the underlying event, projection,
// rejection reason or configuration payload.
func (c *Coordinator) ListRuntimeDeliveryAttempts(ctx context.Context, invocationID string, kind RuntimeDeliveryKind, limit int) ([]RuntimeDeliveryAttempt, error) {
	if c == nil || c.repo == nil {
		return nil, ErrRuntimeDeliveryAttemptUnavailable
	}
	repo, ok := c.repo.(RuntimeDeliveryAttemptRepository)
	if !ok {
		return nil, ErrRuntimeDeliveryAttemptUnavailable
	}
	return repo.ListRuntimeDeliveryAttempts(ctx, strings.TrimSpace(invocationID), kind, limit)
}

// ListRuntimeDeliveryCompensations exposes only the metadata needed to inspect
// durable Abort work. Payloads remain behind their family-specific source
// outboxes and lease ownership is never returned to API callers.
func (c *Coordinator) ListRuntimeDeliveryCompensations(ctx context.Context, invocationID string, kind RuntimeDeliveryKind, status RuntimeDeliveryCompensationStatus, limit int) ([]RuntimeDeliveryCompensation, error) {
	if c == nil || c.repo == nil {
		return nil, ErrRuntimeDeliveryCompensationUnavailable
	}
	repo, ok := c.repo.(RuntimeDeliveryCompensationRepository)
	if !ok {
		return nil, ErrRuntimeDeliveryCompensationUnavailable
	}
	return repo.ListRuntimeDeliveryCompensations(ctx, strings.TrimSpace(invocationID), kind, status, limit)
}

// GetRuntimeDeliveryGroup exposes the bounded source-side coordination
// ledger. It never returns any event, checkpoint, rejection or configuration
// payload; callers receive only group/member metadata and lifecycle state.
func (c *Coordinator) GetRuntimeDeliveryGroup(ctx context.Context, id string) (RuntimeDeliveryGroup, error) {
	if c == nil || c.repo == nil {
		return RuntimeDeliveryGroup{}, ErrRuntimeDeliveryGroupUnavailable
	}
	repo, ok := c.repo.(RuntimeDeliveryGroupRepository)
	if !ok {
		return RuntimeDeliveryGroup{}, ErrRuntimeDeliveryGroupUnavailable
	}
	return repo.GetRuntimeDeliveryGroup(ctx, strings.TrimSpace(id))
}

// ListRuntimeDeliveryGroups returns a bounded, metadata-only view for one
// Invocation. A caller may filter by lifecycle status for reconciliation UI.
func (c *Coordinator) ListRuntimeDeliveryGroups(ctx context.Context, invocationID string, status RuntimeDeliveryGroupStatus, limit int) ([]RuntimeDeliveryGroup, error) {
	if c == nil || c.repo == nil {
		return nil, ErrRuntimeDeliveryGroupUnavailable
	}
	repo, ok := c.repo.(RuntimeDeliveryGroupRepository)
	if !ok {
		return nil, ErrRuntimeDeliveryGroupUnavailable
	}
	return repo.ListRuntimeDeliveryGroups(ctx, strings.TrimSpace(invocationID), status, limit)
}

// ListRuntimeDeliveryGroupSettlements exposes the bounded source-side retry
// ledger for remote group outcomes. It is metadata-only and intentionally
// separate from the local group status so a remote outage cannot reopen the
// source group or consume its local coordination budget.
func (c *Coordinator) ListRuntimeDeliveryGroupSettlements(ctx context.Context, invocationID string, status RuntimeDeliveryGroupSettlementStatus, limit int) ([]RuntimeDeliveryGroupSettlement, error) {
	if c == nil || c.repo == nil {
		return nil, ErrRuntimeDeliveryGroupSettlementUnavailable
	}
	repo, ok := c.repo.(RuntimeDeliveryGroupSettlementRepository)
	if !ok {
		return nil, ErrRuntimeDeliveryGroupSettlementUnavailable
	}
	return repo.ListRuntimeDeliveryGroupSettlements(ctx, strings.TrimSpace(invocationID), status, limit)
}

// ListRuntimeDeliveryGroupSagas exposes the source-side decision journal as a
// bounded metadata-only view. It never returns a settlement lease or payload.
func (c *Coordinator) ListRuntimeDeliveryGroupSagas(ctx context.Context, invocationID string, state RuntimeDeliveryGroupSagaState, limit int) ([]RuntimeDeliveryGroupSaga, error) {
	if c == nil || c.repo == nil {
		return nil, ErrRuntimeDeliveryGroupSagaUnavailable
	}
	repo, ok := c.repo.(RuntimeDeliveryGroupSagaRepository)
	if !ok {
		return nil, ErrRuntimeDeliveryGroupSagaUnavailable
	}
	return repo.ListRuntimeDeliveryGroupSagas(ctx, strings.TrimSpace(invocationID), state, limit)
}

// markRuntimeDeliveryGroupMember records a successfully acknowledged source
// cursor in its coordination ledger. The group revision is read immediately
// before the CAS update; a concurrent member update is retried a few times so
// one dispatcher cannot lose another member's progress. This hook is best
// effort for legacy repositories/groups and never changes outbox delivery
// success into a failure.
func (c *Coordinator) markRuntimeDeliveryGroupMember(ctx context.Context, groupID string, kind RuntimeDeliveryKind, outboxID, deliveryID string, status RuntimeDeliveryGroupMemberStatus, message string, now time.Time) {
	groupID = strings.TrimSpace(groupID)
	if c == nil || c.repo == nil || groupID == "" {
		return
	}
	repo, ok := c.repo.(RuntimeDeliveryGroupRepository)
	if !ok {
		return
	}
	_, _, settlementEnabled := c.runtimeDeliveryGroupSettlementConfig()
	settlementMarker, canAtomicallyEnqueueSettlement := repo.(RuntimeDeliveryGroupSettlementCommitRepository)
	for attempt := 0; attempt < 3; attempt++ {
		group, err := repo.GetRuntimeDeliveryGroup(context.WithoutCancel(ctx), groupID)
		if err != nil {
			return
		}
		if settlementEnabled && canAtomicallyEnqueueSettlement {
			_, _, _, err = settlementMarker.MarkRuntimeDeliveryGroupMemberWithSettlement(context.WithoutCancel(ctx), groupID, group.Revision, kind, outboxID, deliveryID, status, message, now)
		} else {
			_, _, err = repo.MarkRuntimeDeliveryGroupMember(context.WithoutCancel(ctx), groupID, group.Revision, kind, outboxID, deliveryID, status, message, now)
		}
		if err == nil {
			return
		}
		if !errors.Is(err, ErrConflict) {
			return
		}
	}
}

// ReconcileRuntimeDeliveryGroups mirrors terminal source-cursor state into
// the group ledger after a scheduler pass.  A group lease is taken only after
// at least one pending member is observed in a terminal source-outbox state;
// this keeps the family dispatchers responsible for transport retries and
// prevents a group that is merely waiting from burning its own retry budget.
// The operation is deliberately bounded and metadata-only: it never copies a
// delivery payload and it does not claim a cross-service atomic commit.
func (c *Coordinator) ReconcileRuntimeDeliveryGroups(ctx context.Context, now time.Time) {
	if c == nil || c.repo == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	repo, ok := c.repo.(RuntimeDeliveryGroupRepository)
	if !ok {
		return
	}
	groups, err := repo.ListRuntimeDeliveryGroups(context.WithoutCancel(ctx), "", "", 5000)
	if err != nil {
		return
	}
	c.mu.Lock()
	owner := strings.TrimSpace(c.deliveryGroupWorkerID)
	c.mu.Unlock()
	if owner == "" {
		// Test/legacy coordinators may be built as a struct literal.  A stable,
		// bounded fallback keeps reconciliation available without requiring a
		// constructor-only migration.
		owner = "runtime-delivery-group-reconciler"
	}
	claimer, targetedClaim := repo.(RuntimeDeliveryGroupByIDClaimer)
	deferrer, canDefer := repo.(RuntimeDeliveryGroupDeferrer)
	for _, listed := range groups {
		if listed.Status == RuntimeDeliveryGroupCompleted || listed.Status == RuntimeDeliveryGroupFailed {
			c.enqueueRuntimeDeliveryGroupSettlement(context.WithoutCancel(ctx), listed, now)
			continue
		}
		// Inspect first.  Claiming a group with no terminal member would turn a
		// normal pending wait into an artificial group attempt.
		hasTerminalMember := false
		for _, member := range listed.Members {
			if member.Status != RuntimeDeliveryGroupMemberPending {
				continue
			}
			status, _, found := c.runtimeDeliveryGroupMemberState(context.WithoutCancel(ctx), member)
			if found && status != RuntimeDeliveryGroupMemberPending {
				hasTerminalMember = true
				break
			}
		}
		if !hasTerminalMember {
			continue
		}

		group := listed
		if targetedClaim {
			claimed, won, claimErr := claimer.ClaimRuntimeDeliveryGroupByID(context.WithoutCancel(ctx), listed.ID, owner, now, time.Minute)
			if claimErr != nil || !won {
				continue
			}
			group = claimed
		} else {
			// Older repositories do not expose an exact-row claim.  Preserve the
			// previous best-effort metadata reconciliation rather than using a
			// global claim that could lease an unrelated group.
			for _, member := range listed.Members {
				if member.Status != RuntimeDeliveryGroupMemberPending {
					continue
				}
				status, message, found := c.runtimeDeliveryGroupMemberState(context.WithoutCancel(ctx), member)
				if found && status != RuntimeDeliveryGroupMemberPending {
					c.markRuntimeDeliveryGroupMember(context.WithoutCancel(ctx), listed.ID, member.Kind, member.OutboxID, member.DeliveryID, status, message, now)
				}
			}
			continue
		}

		pendingObserved := false
		for _, member := range group.Members {
			if member.Status != RuntimeDeliveryGroupMemberPending {
				continue
			}
			status, message, found := c.runtimeDeliveryGroupMemberState(context.WithoutCancel(ctx), member)
			if !found || status == RuntimeDeliveryGroupMemberPending {
				pendingObserved = true
				continue
			}
			c.markRuntimeDeliveryGroupMember(context.WithoutCancel(ctx), group.ID, member.Kind, member.OutboxID, member.DeliveryID, status, message, now)
		}

		latest, getErr := repo.GetRuntimeDeliveryGroup(context.WithoutCancel(ctx), group.ID)
		if getErr != nil || latest.Status != RuntimeDeliveryGroupProcessing {
			continue
		}
		if !pendingObserved {
			// A concurrent mark can win the last member between the inspection and
			// the CAS hook.  Re-read terminal source states once so a stale group
			// cannot remain processing forever.
			for _, member := range latest.Members {
				if member.Status != RuntimeDeliveryGroupMemberPending {
					continue
				}
				status, message, found := c.runtimeDeliveryGroupMemberState(context.WithoutCancel(ctx), member)
				if found && status != RuntimeDeliveryGroupMemberPending {
					c.markRuntimeDeliveryGroupMember(context.WithoutCancel(ctx), latest.ID, member.Kind, member.OutboxID, member.DeliveryID, status, message, now)
				} else {
					pendingObserved = true
				}
			}
			latest, getErr = repo.GetRuntimeDeliveryGroup(context.WithoutCancel(ctx), group.ID)
			if getErr != nil || latest.Status != RuntimeDeliveryGroupProcessing {
				continue
			}
		}
		if pendingObserved && canDefer {
			// Do not use RetryRuntimeDeliveryGroup here: the group attempt budget
			// belongs to actual coordination failures, not to an outbox that is
			// still legitimately processing in another worker.
			_, _ = deferrer.DeferRuntimeDeliveryGroup(context.WithoutCancel(ctx), latest.ID, owner, now, now.Add(time.Second), "等待成员 outbox 进入终态")
		}
		if latest.Status == RuntimeDeliveryGroupCompleted || latest.Status == RuntimeDeliveryGroupFailed {
			c.enqueueRuntimeDeliveryGroupSettlement(context.WithoutCancel(ctx), latest, now)
		}
	}
	// A synchronous reconciliation call remains useful to embedders and tests:
	// enqueue terminal work, then make one bounded durable dispatch pass. The
	// queue itself is still the recovery source for later scheduler ticks.
	c.dispatchDueRuntimeDeliveryGroupSettlementsBounded(context.WithoutCancel(ctx), now, 32)
}

func (c *Coordinator) runtimeDeliveryGroupSettlementConfig() (RuntimeDeliveryGroupTransactionalTransport, string, bool) {
	if c == nil {
		return nil, "", false
	}
	c.mu.Lock()
	transport := c.deliveryGroupTransport
	owner := strings.TrimSpace(c.deliveryGroupSettlementWorkerID)
	closed := c.closed
	c.mu.Unlock()
	if transport == nil || closed {
		return nil, "", false
	}
	if marker, ok := transport.(RuntimeDeliveryGroupTransactionalOptIn); ok && !marker.RuntimeDeliveryGroupTransactionsEnabled() {
		return nil, "", false
	}
	if owner == "" {
		owner = "runtime-delivery-group-settlement-worker"
	}
	return transport, owner, true
}

// enqueueRuntimeDeliveryGroupSettlement turns a terminal source group into a
// durable remote outcome work item. Built-in repositories use this queue; an
// older embedder without the optional repository keeps the historical
// one-shot best-effort path for compatibility.
func (c *Coordinator) enqueueRuntimeDeliveryGroupSettlement(ctx context.Context, group RuntimeDeliveryGroup, now time.Time) {
	_, _, enabled := c.runtimeDeliveryGroupSettlementConfig()
	if !enabled {
		return
	}
	settlementRepo, durable := c.repo.(RuntimeDeliveryGroupSettlementRepository)
	if !durable {
		_ = c.settleRuntimeDeliveryGroupRemoteOutcome(context.WithoutCancel(ctx), group, now)
		return
	}
	settlement, err := NewRuntimeDeliveryGroupSettlement(group, now)
	if err != nil {
		return
	}
	if sagaRepo, sagaEnabled := c.repo.(RuntimeDeliveryGroupSagaRepository); sagaEnabled {
		// Register the immutable business decision before queueing execution
		// work. Reconciliation repairs the tiny local window if the process exits
		// between these two metadata writes.
		if saga, sagaErr := NewRuntimeDeliveryGroupSaga(group, now); sagaErr == nil {
			if _, enqueueErr := sagaRepo.EnqueueRuntimeDeliveryGroupSaga(context.WithoutCancel(ctx), saga); enqueueErr != nil {
				return
			}
		} else {
			return
		}
	}
	_, _ = settlementRepo.EnqueueRuntimeDeliveryGroupSettlement(context.WithoutCancel(ctx), settlement)
}

func (c *Coordinator) advanceRuntimeDeliveryGroupSettlementProgress(ctx context.Context, item RuntimeDeliveryGroupSettlement, owner string, stage RuntimeDeliveryGroupSettlementStage, remotePhase RuntimeDeliveryGroupSettlementRemotePhase, alertCode RuntimeDeliveryGroupSettlementAlertCode, message string, now time.Time) (RuntimeDeliveryGroupSettlement, error) {
	progressRepo, ok := c.repo.(RuntimeDeliveryGroupSettlementProgressRepository)
	if !ok {
		return item, nil
	}
	updated, _, err := progressRepo.AdvanceRuntimeDeliveryGroupSettlement(context.WithoutCancel(ctx), item.ID, owner, item.Revision, stage, remotePhase, alertCode, message, now)
	if err != nil {
		return item, err
	}
	return updated, nil
}

func (c *Coordinator) processRuntimeDeliveryGroupSettlementClaim(ctx context.Context, settlementRepo RuntimeDeliveryGroupSettlementRepository, groupRepo RuntimeDeliveryGroupRepository, item RuntimeDeliveryGroupSettlement, owner string, now time.Time, transport RuntimeDeliveryGroupTransactionalTransport, envelope *RuntimeDeliveryGroupEnvelope, status *RuntimeDeliveryGroupRemoteStatus) {
	group, getErr := groupRepo.GetRuntimeDeliveryGroup(context.WithoutCancel(ctx), item.GroupID)
	if getErr != nil {
		if errors.Is(getErr, ErrNotFound) {
			item, _ = c.advanceRuntimeDeliveryGroupSettlementProgress(ctx, item, owner, RuntimeDeliveryGroupSettlementStageStatus, "", RuntimeDeliveryGroupSettlementAlertSourceMissing, "source delivery group 不存在", now)
			_, _ = settlementRepo.FailRuntimeDeliveryGroupSettlement(context.WithoutCancel(ctx), item.ID, owner, now, "source delivery group 不存在")
		} else {
			item, _ = c.advanceRuntimeDeliveryGroupSettlementProgress(ctx, item, owner, RuntimeDeliveryGroupSettlementStageStatus, "", RuntimeDeliveryGroupSettlementAlertForError(getErr), getErr.Error(), now)
			_, _ = settlementRepo.RetryRuntimeDeliveryGroupSettlement(context.WithoutCancel(ctx), item.ID, owner, now, getErr.Error())
		}
		return
	}
	if group.Status != item.Outcome || (group.Status != RuntimeDeliveryGroupCompleted && group.Status != RuntimeDeliveryGroupFailed) {
		item, _ = c.advanceRuntimeDeliveryGroupSettlementProgress(ctx, item, owner, RuntimeDeliveryGroupSettlementStageStatus, "", RuntimeDeliveryGroupSettlementAlertSourceConflict, "source group 终态与 settlement 不一致", now)
		_, _ = settlementRepo.FailRuntimeDeliveryGroupSettlement(context.WithoutCancel(ctx), item.ID, owner, now, "source group 终态与 settlement 不一致")
		return
	}
	saga, sagaEnabled, sagaErr := c.ensureRuntimeDeliveryGroupSaga(ctx, group, item.Phase, now)
	if sagaErr != nil {
		message := "saga journal 初始化失败: " + sagaErr.Error()
		item, _ = c.advanceRuntimeDeliveryGroupSettlementProgress(ctx, item, owner, RuntimeDeliveryGroupSettlementStageStatus, "", RuntimeDeliveryGroupSettlementAlertForError(sagaErr), message, now)
		if runtimeDeliveryGroupSagaPermanentError(sagaErr) {
			_, _ = settlementRepo.FailRuntimeDeliveryGroupSettlement(context.WithoutCancel(ctx), item.ID, owner, now, message)
		} else {
			_, _ = settlementRepo.RetryRuntimeDeliveryGroupSettlement(context.WithoutCancel(ctx), item.ID, owner, now, message)
		}
		return
	}
	if sagaEnabled {
		switch saga.State {
		case RuntimeDeliveryGroupSagaCommitted:
			if saga.Decision != RuntimeDeliveryGroupSagaCommit {
				_, _ = settlementRepo.FailRuntimeDeliveryGroupSettlement(context.WithoutCancel(ctx), item.ID, owner, now, "saga terminal decision 不一致")
				return
			}
			_, _ = settlementRepo.CompleteRuntimeDeliveryGroupSettlement(context.WithoutCancel(ctx), item.ID, owner, now)
			return
		case RuntimeDeliveryGroupSagaAborted:
			if saga.Decision != RuntimeDeliveryGroupSagaAbort {
				_, _ = settlementRepo.FailRuntimeDeliveryGroupSettlement(context.WithoutCancel(ctx), item.ID, owner, now, "saga terminal decision 不一致")
				return
			}
			_, _ = settlementRepo.CompleteRuntimeDeliveryGroupSettlement(context.WithoutCancel(ctx), item.ID, owner, now)
			return
		case RuntimeDeliveryGroupSagaCompensationRequired, RuntimeDeliveryGroupSagaFailed:
			_, _ = settlementRepo.FailRuntimeDeliveryGroupSettlement(context.WithoutCancel(ctx), item.ID, owner, now, saga.LastError)
			return
		}
	}
	progress := func(stage RuntimeDeliveryGroupSettlementStage, remotePhase RuntimeDeliveryGroupSettlementRemotePhase, alertCode RuntimeDeliveryGroupSettlementAlertCode, message string) error {
		var progressErr error
		item, progressErr = c.advanceRuntimeDeliveryGroupSettlementProgress(ctx, item, owner, stage, remotePhase, alertCode, message, now)
		if progressErr != nil || !sagaEnabled {
			return progressErr
		}
		sagaState, sagaRemotePhase := runtimeDeliveryGroupSagaProgressForSettlement(saga, item, stage, remotePhase)
		updated, _, sagaAdvanceErr := c.repo.(RuntimeDeliveryGroupSagaRepository).AdvanceRuntimeDeliveryGroupSaga(context.WithoutCancel(ctx), saga.ID, saga.Revision, sagaState, sagaRemotePhase, message, now)
		if sagaAdvanceErr != nil {
			return sagaAdvanceErr
		}
		saga = updated
		return nil
	}
	var settleErr error
	if envelope != nil {
		settleErr = c.settleRuntimeDeliveryGroupRemoteOutcomeWithEnvelopeStatus(context.WithoutCancel(ctx), group, now, transport, *envelope, status, progress)
	} else {
		settleErr = c.settleRuntimeDeliveryGroupRemoteOutcomeWithProgress(context.WithoutCancel(ctx), group, now, progress)
	}
	if settleErr == nil {
		if sagaEnabled {
			terminalState := runtimeDeliveryGroupSagaTerminalForDecision(saga.Decision)
			terminalRemote := runtimeDeliveryGroupSagaTerminalRemoteForDecision(saga.Decision)
			if saga.State != terminalState {
				updated, _, sagaAdvanceErr := c.repo.(RuntimeDeliveryGroupSagaRepository).AdvanceRuntimeDeliveryGroupSaga(context.WithoutCancel(ctx), saga.ID, saga.Revision, terminalState, terminalRemote, "", now)
				if sagaAdvanceErr != nil {
					message := "saga terminal 收口失败: " + sagaAdvanceErr.Error()
					_, _ = settlementRepo.RetryRuntimeDeliveryGroupSettlement(context.WithoutCancel(ctx), item.ID, owner, now, message)
					return
				}
				saga = updated
			}
		}
		_, _ = settlementRepo.CompleteRuntimeDeliveryGroupSettlement(context.WithoutCancel(ctx), item.ID, owner, now)
		return
	}
	progressErr := progress(item.Stage, "", RuntimeDeliveryGroupSettlementAlertForError(settleErr), settleErr.Error())
	if progressErr != nil {
		settleErr = errors.Join(settleErr, progressErr)
	}
	if sagaEnabled {
		var targetState RuntimeDeliveryGroupSagaState
		var targetRemote RuntimeDeliveryGroupSettlementRemotePhase
		switch {
		case errors.Is(settleErr, ErrRuntimeDeliveryGroupSettlementRemoteConflict):
			targetState = RuntimeDeliveryGroupSagaCompensationRequired
			targetRemote = runtimeDeliveryGroupSagaOppositeRemoteForDecision(saga.Decision)
		case runtimeDeliveryGroupSettlementPermanentError(settleErr):
			targetState = RuntimeDeliveryGroupSagaFailed
		}
		if targetState != "" && saga.State != RuntimeDeliveryGroupSagaCompensationRequired && saga.State != RuntimeDeliveryGroupSagaFailed {
			updated, _, sagaAdvanceErr := c.repo.(RuntimeDeliveryGroupSagaRepository).AdvanceRuntimeDeliveryGroupSaga(context.WithoutCancel(ctx), saga.ID, saga.Revision, targetState, targetRemote, settleErr.Error(), now)
			if sagaAdvanceErr != nil {
				settleErr = errors.Join(settleErr, sagaAdvanceErr)
			} else {
				saga = updated
			}
		}
	}
	if runtimeDeliveryGroupSettlementPermanentError(settleErr) {
		_, _ = settlementRepo.FailRuntimeDeliveryGroupSettlement(context.WithoutCancel(ctx), item.ID, owner, now, settleErr.Error())
		return
	}
	_, _ = settlementRepo.RetryRuntimeDeliveryGroupSettlement(context.WithoutCancel(ctx), item.ID, owner, now, settleErr.Error())
}

type runtimeDeliveryGroupSettlementClaim struct {
	item     RuntimeDeliveryGroupSettlement
	group    RuntimeDeliveryGroup
	envelope RuntimeDeliveryGroupEnvelope
}

func (c *Coordinator) dispatchDueRuntimeDeliveryGroupSettlementsBatch(ctx context.Context, now time.Time, maxClaims int, settlementRepo RuntimeDeliveryGroupSettlementRepository, groupRepo RuntimeDeliveryGroupRepository, transport RuntimeDeliveryGroupTransactionalTransport, owner string) {
	batcher := transport.(RuntimeDeliveryGroupBatchReconciler)
	claims := make([]runtimeDeliveryGroupSettlementClaim, 0, maxClaims)
	for attempt := 0; attempt < maxClaims; attempt++ {
		item, claimed, err := settlementRepo.ClaimRuntimeDeliveryGroupSettlement(ctx, owner, now, 30*time.Second)
		if err != nil || !claimed {
			break
		}
		group, getErr := groupRepo.GetRuntimeDeliveryGroup(context.WithoutCancel(ctx), item.GroupID)
		if getErr != nil || group.Status != item.Outcome || (group.Status != RuntimeDeliveryGroupCompleted && group.Status != RuntimeDeliveryGroupFailed) {
			c.processRuntimeDeliveryGroupSettlementClaim(ctx, settlementRepo, groupRepo, item, owner, now, transport, nil, nil)
			continue
		}
		group, getErr = c.ensureRuntimeDeliveryGroupFence(ctx, group, now, transport)
		if getErr != nil {
			c.processRuntimeDeliveryGroupSettlementClaim(ctx, settlementRepo, groupRepo, item, owner, now, transport, nil, nil)
			continue
		}
		envelope, envelopeErr := transport.BuildRuntimeDeliveryGroupEnvelope(group, now)
		if envelopeErr != nil {
			c.processRuntimeDeliveryGroupSettlementClaim(ctx, settlementRepo, groupRepo, item, owner, now, transport, nil, nil)
			continue
		}
		claims = append(claims, runtimeDeliveryGroupSettlementClaim{item: item, group: group, envelope: envelope})
	}
	if len(claims) == 0 {
		return
	}
	envelopes := make([]RuntimeDeliveryGroupEnvelope, len(claims))
	for index := range claims {
		envelopes[index] = claims[index].envelope
	}
	statuses, batchErr := batcher.ReconcileRuntimeDeliveryGroups(context.WithoutCancel(ctx), envelopes)
	if batchErr != nil || len(statuses) != len(claims) {
		if batchErr == nil {
			batchErr = fmt.Errorf("%w: batch status 数量不匹配", ErrRuntimeDeliveryGroupTransactionAuth)
		}
		for _, claim := range claims {
			// Fall back to the single status path when the remote has not yet
			// deployed /status/batch. A malformed custom batcher is treated as a
			// normal bounded transport error and never partially acknowledged.
			c.processRuntimeDeliveryGroupSettlementClaim(ctx, settlementRepo, groupRepo, claim.item, owner, now, transport, nil, nil)
		}
		return
	}
	for index, claim := range claims {
		status := statuses[index]
		c.processRuntimeDeliveryGroupSettlementClaim(ctx, settlementRepo, groupRepo, claim.item, owner, now, transport, &claim.envelope, &status)
	}
}

// dispatchDueRuntimeDeliveryGroupSettlementsBounded drains the source-side
// remote outcome queue with the same lease/CAS discipline as family outboxes.
// A transient transport failure schedules a bounded retry; an identity or
// opposite-terminal-phase conflict becomes a terminal settlement failure but
// never mutates the already terminal source group.
func (c *Coordinator) dispatchDueRuntimeDeliveryGroupSettlementsBounded(ctx context.Context, now time.Time, maxClaims int) {
	if c == nil || c.repo == nil || maxClaims <= 0 {
		return
	}
	settlementRepo, ok := c.repo.(RuntimeDeliveryGroupSettlementRepository)
	if !ok {
		return
	}
	_, owner, enabled := c.runtimeDeliveryGroupSettlementConfig()
	if !enabled || owner == "" {
		return
	}
	groupRepo, groupRepoOK := c.repo.(RuntimeDeliveryGroupRepository)
	if !groupRepoOK {
		return
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	transport, _, transportEnabled := c.runtimeDeliveryGroupSettlementConfig()
	if transportEnabled {
		if _, batcher := transport.(RuntimeDeliveryGroupBatchReconciler); batcher {
			c.dispatchDueRuntimeDeliveryGroupSettlementsBatch(ctx, now, maxClaims, settlementRepo, groupRepo, transport, owner)
			return
		}
	}
	for attempt := 0; attempt < maxClaims; attempt++ {
		item, claimed, err := settlementRepo.ClaimRuntimeDeliveryGroupSettlement(ctx, owner, now, 30*time.Second)
		if err != nil || !claimed {
			return
		}
		c.processRuntimeDeliveryGroupSettlementClaim(ctx, settlementRepo, groupRepo, item, owner, now, transport, nil, nil)
	}
}

func runtimeDeliveryGroupSettlementPermanentError(err error) bool {
	return errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalidRuntimeDeliveryGroupTransaction) || errors.Is(err, ErrRuntimeDeliveryGroupTransactionAuth) || errors.Is(err, ErrRuntimeDeliveryGroupTransactionStale) || errors.Is(err, ErrInvalidRuntimeDeliveryGroupSettlement) || errors.Is(err, ErrRuntimeDeliveryFenceConflict) || errors.Is(err, ErrRuntimeDeliveryFenceStale) || errors.Is(err, ErrRuntimeDeliveryFenceRequired) || errors.Is(err, ErrRuntimeDeliveryFenceUnavailable)
}

// ensureRuntimeDeliveryGroupFence binds a source-issued monotonic epoch only
// when the selected remote transport explicitly requires it. The operation
// is idempotent: a concurrent coordinator may win the group CAS first, in
// which case the already-bound group is reloaded and reused.
func (c *Coordinator) ensureRuntimeDeliveryGroupFence(ctx context.Context, group RuntimeDeliveryGroup, now time.Time, transport RuntimeDeliveryGroupTransactionalTransport) (RuntimeDeliveryGroup, error) {
	requirement, ok := transport.(RuntimeDeliveryGroupFenceRequirement)
	if !ok || !requirement.RuntimeDeliveryGroupFenceRequired() {
		return group, nil
	}
	if group.FenceRevision > 0 {
		if err := group.ValidateFence(); err != nil {
			return RuntimeDeliveryGroup{}, err
		}
		return group, nil
	}
	if c == nil || c.repo == nil {
		return RuntimeDeliveryGroup{}, ErrRuntimeDeliveryFenceUnavailable
	}
	issuer, issuerOK := c.repo.(RuntimeDeliveryGroupFenceIssuer)
	binder, binderOK := c.repo.(RuntimeDeliveryGroupFenceBinder)
	groupRepo, groupRepoOK := c.repo.(RuntimeDeliveryGroupRepository)
	if !issuerOK || !binderOK || !groupRepoOK {
		return RuntimeDeliveryGroup{}, ErrRuntimeDeliveryFenceUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	fence, err := issuer.IssueRuntimeDeliveryGroupFence(context.WithoutCancel(ctx), group.Source, group.Destination, group.InvocationID, group.ID, now)
	if err != nil {
		return RuntimeDeliveryGroup{}, err
	}
	bound, err := binder.BindRuntimeDeliveryGroupFence(context.WithoutCancel(ctx), group.ID, group.Revision, fence, now)
	if err == nil {
		return bound, nil
	}
	if errors.Is(err, ErrConflict) {
		latest, getErr := groupRepo.GetRuntimeDeliveryGroup(context.WithoutCancel(ctx), group.ID)
		if getErr == nil && latest.FenceRevision > 0 {
			if validateErr := latest.ValidateFence(); validateErr == nil {
				return latest, nil
			}
		}
	}
	return RuntimeDeliveryGroup{}, err
}

// settleRuntimeDeliveryGroupRemoteOutcome executes one bounded, idempotent
// remote outcome attempt. The source group is already terminal; this helper
// only settles the destination metadata ledger and never changes source state.
func (c *Coordinator) settleRuntimeDeliveryGroupRemoteOutcome(ctx context.Context, group RuntimeDeliveryGroup, now time.Time) error {
	return c.settleRuntimeDeliveryGroupRemoteOutcomeWithProgress(ctx, group, now, nil)
}

func (c *Coordinator) settleRuntimeDeliveryGroupRemoteOutcomeWithProgress(ctx context.Context, group RuntimeDeliveryGroup, now time.Time, progress func(RuntimeDeliveryGroupSettlementStage, RuntimeDeliveryGroupSettlementRemotePhase, RuntimeDeliveryGroupSettlementAlertCode, string) error) error {
	transport, _, enabled := c.runtimeDeliveryGroupSettlementConfig()
	if !enabled {
		return ErrRuntimeDeliveryGroupSettlementUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	group, err := c.ensureRuntimeDeliveryGroupFence(ctx, group, now, transport)
	if err != nil {
		return err
	}
	envelope, err := transport.BuildRuntimeDeliveryGroupEnvelope(group, now)
	if err != nil {
		return err
	}
	return c.settleRuntimeDeliveryGroupRemoteOutcomeWithEnvelopeStatus(ctx, group, now, transport, envelope, nil, progress)
}

func (c *Coordinator) settleRuntimeDeliveryGroupRemoteOutcomeWithEnvelopeStatus(ctx context.Context, group RuntimeDeliveryGroup, now time.Time, transport RuntimeDeliveryGroupTransactionalTransport, envelope RuntimeDeliveryGroupEnvelope, resolvedStatus *RuntimeDeliveryGroupRemoteStatus, progress func(RuntimeDeliveryGroupSettlementStage, RuntimeDeliveryGroupSettlementRemotePhase, RuntimeDeliveryGroupSettlementAlertCode, string) error) error {
	if transport == nil {
		return ErrRuntimeDeliveryGroupSettlementUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	advance := func(stage RuntimeDeliveryGroupSettlementStage, remotePhase RuntimeDeliveryGroupSettlementRemotePhase) error {
		if progress == nil {
			return nil
		}
		return progress(stage, remotePhase, "", "")
	}
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	status := resolvedStatus
	if status == nil {
		if reconciler, ok := transport.(RuntimeDeliveryGroupReconciler); ok {
			if err := advance(RuntimeDeliveryGroupSettlementStageStatus, ""); err != nil {
				return err
			}
			resolved, statusErr := reconciler.ReconcileRuntimeDeliveryGroup(settleCtx, envelope)
			if statusErr == nil {
				if err := resolved.ValidateAgainst(envelope); err != nil {
					return err
				}
				status = &resolved
				remotePhase := RuntimeDeliveryGroupSettlementRemotePhase(resolved.Phase)
				if !resolved.Found {
					remotePhase = RuntimeDeliveryGroupSettlementRemoteAbsent
				}
				if err := advance(RuntimeDeliveryGroupSettlementStageStatus, remotePhase); err != nil {
					return err
				}
				switch {
				case group.Status == RuntimeDeliveryGroupCompleted && resolved.Phase == RuntimeDeliveryGroupTransactionCommitted:
					return nil
				case group.Status == RuntimeDeliveryGroupCompleted && resolved.Phase == RuntimeDeliveryGroupTransactionAborted:
					return fmt.Errorf("%w: %w: completed source group 的 remote settlement 已 aborted", ErrConflict, ErrRuntimeDeliveryGroupSettlementRemoteConflict)
				case group.Status == RuntimeDeliveryGroupFailed && resolved.Phase == RuntimeDeliveryGroupTransactionAborted:
					return nil
				case group.Status == RuntimeDeliveryGroupFailed && resolved.Phase == RuntimeDeliveryGroupTransactionCommitted:
					// A committed remote group is immutable. Do not issue abort or
					// retry blindly; retain the discrepancy as a failed settlement.
					return fmt.Errorf("%w: %w: failed source group 的 remote settlement 已 committed", ErrConflict, ErrRuntimeDeliveryGroupSettlementRemoteConflict)
				}
			} else if !errors.Is(statusErr, ErrRuntimeDeliveryGroupTransactionUnavailable) {
				return statusErr
			}
		}
	} else {
		if err := status.ValidateAgainst(envelope); err != nil {
			return err
		}
		remotePhase := RuntimeDeliveryGroupSettlementRemotePhase(status.Phase)
		if !status.Found {
			remotePhase = RuntimeDeliveryGroupSettlementRemoteAbsent
		}
		if err := advance(RuntimeDeliveryGroupSettlementStageStatus, remotePhase); err != nil {
			return err
		}
		switch {
		case group.Status == RuntimeDeliveryGroupCompleted && status.Phase == RuntimeDeliveryGroupTransactionCommitted:
			return nil
		case group.Status == RuntimeDeliveryGroupCompleted && status.Phase == RuntimeDeliveryGroupTransactionAborted:
			return fmt.Errorf("%w: %w: completed source group 的 remote settlement 已 aborted", ErrConflict, ErrRuntimeDeliveryGroupSettlementRemoteConflict)
		case group.Status == RuntimeDeliveryGroupFailed && status.Phase == RuntimeDeliveryGroupTransactionAborted:
			return nil
		case group.Status == RuntimeDeliveryGroupFailed && status.Phase == RuntimeDeliveryGroupTransactionCommitted:
			return fmt.Errorf("%w: %w: failed source group 的 remote settlement 已 committed", ErrConflict, ErrRuntimeDeliveryGroupSettlementRemoteConflict)
		}
	}
	if err := advance(RuntimeDeliveryGroupSettlementStagePreparing, ""); err != nil {
		return err
	}
	prepared, err := transport.PrepareRuntimeDeliveryGroup(settleCtx, envelope)
	if err != nil {
		return err
	}
	if err := prepared.ValidateAgainstPhase(envelope, "prepare"); err != nil {
		return err
	}
	if err := advance(RuntimeDeliveryGroupSettlementStagePrepared, RuntimeDeliveryGroupSettlementRemotePrepared); err != nil {
		return err
	}
	if group.Status == RuntimeDeliveryGroupFailed {
		if err := advance(RuntimeDeliveryGroupSettlementStageAborting, ""); err != nil {
			return err
		}
		aborted, err := transport.AbortRuntimeDeliveryGroup(settleCtx, envelope)
		if err != nil {
			return err
		}
		if err := aborted.ValidateAgainstPhase(envelope, "abort"); err != nil {
			return err
		}
		return advance(RuntimeDeliveryGroupSettlementStageAborting, RuntimeDeliveryGroupSettlementRemoteAborted)
	}
	if err := advance(RuntimeDeliveryGroupSettlementStageCommitting, ""); err != nil {
		return err
	}
	committed, err := transport.CommitRuntimeDeliveryGroup(settleCtx, envelope)
	if err != nil {
		return err
	}
	if err := committed.ValidateAgainstPhase(envelope, "commit"); err != nil {
		return err
	}
	return advance(RuntimeDeliveryGroupSettlementStageCommitting, RuntimeDeliveryGroupSettlementRemoteCommitted)
}

// refreshRuntimeDeliveryGroups is kept as a private compatibility name for
// the existing dispatcher loop; all new callers should use the explicit
// ReconcileRuntimeDeliveryGroups entry point.
func (c *Coordinator) refreshRuntimeDeliveryGroups(ctx context.Context, now time.Time) {
	c.ReconcileRuntimeDeliveryGroups(ctx, now)
}

func (c *Coordinator) runtimeDeliveryGroupMemberState(ctx context.Context, member RuntimeDeliveryGroupMember) (RuntimeDeliveryGroupMemberStatus, string, bool) {
	if c == nil || c.repo == nil {
		return RuntimeDeliveryGroupMemberPending, "", false
	}
	mapStatus := func(completed, failed bool, message string) (RuntimeDeliveryGroupMemberStatus, string, bool) {
		switch {
		case completed:
			return RuntimeDeliveryGroupMemberCompleted, "", true
		case failed:
			return RuntimeDeliveryGroupMemberFailed, message, true
		default:
			return RuntimeDeliveryGroupMemberPending, "", true
		}
	}
	switch member.Kind {
	case RuntimeDeliveryKindEvent:
		repo, ok := c.repo.(RuntimeEventOutboxRepository)
		if !ok {
			return RuntimeDeliveryGroupMemberPending, "", false
		}
		item, err := repo.GetRuntimeEventOutbox(ctx, member.OutboxID)
		if err != nil {
			return RuntimeDeliveryGroupMemberPending, "", false
		}
		return mapStatus(item.Status == RuntimeEventOutboxCompleted, item.Status == RuntimeEventOutboxFailed, item.LastError)
	case RuntimeDeliveryKindCheckpoint:
		repo, ok := c.repo.(RuntimeCheckpointDeliveryOutboxRepository)
		if !ok {
			return RuntimeDeliveryGroupMemberPending, "", false
		}
		item, err := repo.GetRuntimeCheckpointDeliveryOutbox(ctx, member.OutboxID)
		if err != nil {
			return RuntimeDeliveryGroupMemberPending, "", false
		}
		return mapStatus(item.Status == RuntimeCheckpointDeliveryOutboxCompleted, item.Status == RuntimeCheckpointDeliveryOutboxFailed, item.LastError)
	case RuntimeDeliveryKindConfig:
		repo, ok := c.repo.(RuntimeConfigDeliveryOutboxRepository)
		if !ok {
			return RuntimeDeliveryGroupMemberPending, "", false
		}
		item, err := repo.GetRuntimeConfigDeliveryOutbox(ctx, member.OutboxID)
		if err != nil {
			return RuntimeDeliveryGroupMemberPending, "", false
		}
		return mapStatus(item.Status == RuntimeConfigDeliveryOutboxCompleted, item.Status == RuntimeConfigDeliveryOutboxFailed, item.LastError)
	case RuntimeDeliveryKindRejection:
		repo, ok := c.repo.(RuntimeApprovalRejectionDeliveryOutboxRepository)
		if !ok {
			return RuntimeDeliveryGroupMemberPending, "", false
		}
		item, err := repo.GetRuntimeApprovalRejectionDeliveryOutbox(ctx, member.OutboxID)
		if err != nil {
			return RuntimeDeliveryGroupMemberPending, "", false
		}
		return mapStatus(item.Status == RuntimeApprovalRejectionDeliveryOutboxCompleted, item.Status == RuntimeApprovalRejectionDeliveryOutboxFailed, item.LastError)
	default:
		return RuntimeDeliveryGroupMemberPending, "", false
	}
}

func runtimeDeliveryAttemptDestinationDone(phase RuntimeDeliveryAttemptPhase) bool {
	return phase == RuntimeDeliveryAttemptAccepted || phase == RuntimeDeliveryAttemptCommitted || phase == RuntimeDeliveryAttemptCompleted
}

// runtimeCheckpointEventBarrier checks whether a checkpoint's declared event
// high-water mark is already visible through the same source/destination
// route. It is intentionally opt-in: if an older repository cannot release a
// claim without consuming an attempt, the coordinator keeps its historical
// behavior instead of risking retry-budget exhaustion while waiting.
func (c *Coordinator) runtimeCheckpointEventBarrier(ctx context.Context, item RuntimeCheckpointDeliveryOutbox) (applies bool, ready bool, err error) {
	if c == nil || c.repo == nil {
		return false, false, nil
	}
	if _, ok := c.repo.(RuntimeCheckpointDeliveryOutboxDeferrer); !ok {
		return false, false, nil
	}
	reader, ok := c.repo.(RuntimeEventOutboxConsistencyReader)
	if !ok {
		return false, false, nil
	}
	c.mu.Lock()
	eventTransport := c.eventDeliveryTransport
	eventHandler := c.eventOutboxHandler
	c.mu.Unlock()
	if eventTransport == nil {
		if eventHandler == nil || item.Source != "local" || item.Destination != "event-handler" {
			return false, false, nil
		}
	} else {
		routeProvider, hasRoute := eventTransport.(RuntimeEventDeliveryRouteProvider)
		if !hasRoute {
			return false, false, nil
		}
		eventSource, eventDestination := routeProvider.RuntimeEventDeliveryRoute()
		if strings.TrimSpace(eventSource) != strings.TrimSpace(item.Source) || strings.TrimSpace(eventDestination) != strings.TrimSpace(item.Destination) {
			return false, false, nil
		}
	}
	ready, err = reader.RuntimeEventOutboxReadyThrough(ctx, item.InvocationID, item.EventSequence)
	return true, ready, err
}

// dispatchDueRuntimeEventOutbox delivers the historical bounded event batch.
// The shared scheduler calls the one-item variant below to preserve
// round-robin fairness across event and checkpoint cursors.
func (c *Coordinator) dispatchDueRuntimeEventOutbox(ctx context.Context, now time.Time) {
	c.dispatchDueRuntimeEventOutboxBounded(ctx, now, 32)
}

func (c *Coordinator) dispatchDueRuntimeEventOutboxBounded(ctx context.Context, now time.Time, maxClaims int) {
	outboxRepo, ok := c.repo.(RuntimeEventOutboxRepository)
	if !ok {
		return
	}
	eventRepo, ok := c.repo.(AgentEventRepository)
	if !ok {
		return
	}
	c.mu.Lock()
	handler := c.eventOutboxHandler
	eventTransport := c.eventDeliveryTransport
	owner := c.eventOutboxWorkerID
	closed := c.closed
	c.mu.Unlock()
	if handler == nil || closed || strings.TrimSpace(owner) == "" {
		return
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if maxClaims <= 0 {
		return
	}
	routeSource, routeDestination := "local", "event-handler"
	if routeProvider, ok := eventTransport.(RuntimeEventDeliveryRouteProvider); ok {
		if source, destination := routeProvider.RuntimeEventDeliveryRoute(); strings.TrimSpace(source) != "" && strings.TrimSpace(destination) != "" {
			routeSource, routeDestination = strings.TrimSpace(source), strings.TrimSpace(destination)
		}
	}
	for attempt := 0; attempt < maxClaims; attempt++ {
		item, claimed, err := outboxRepo.ClaimRuntimeEventOutbox(ctx, owner, now, 30*time.Second)
		if err != nil || !claimed {
			return
		}
		journal, journalEnabled, journalErr := c.beginRuntimeDeliveryAttempt(ctx, RuntimeDeliveryKindEvent, routeSource, routeDestination, item.ID, item.InvocationID, item.ID, item.Revision, item.Attempt, owner, now)
		if journalErr != nil {
			_, _ = outboxRepo.RetryRuntimeEventOutbox(context.WithoutCancel(ctx), item.ID, owner, now, journalErr.Error())
			continue
		}
		if journalEnabled && runtimeDeliveryAttemptDestinationDone(journal.Phase) {
			if journal.Phase != RuntimeDeliveryAttemptCompleted {
				var advanceErr error
				journal, _, advanceErr = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptCompleted, now, "")
				if advanceErr != nil {
					_, _ = outboxRepo.RetryRuntimeEventOutbox(context.WithoutCancel(ctx), item.ID, owner, now, advanceErr.Error())
					continue
				}
			}
			if completed, _ := outboxRepo.CompleteRuntimeEventOutbox(context.WithoutCancel(ctx), item.ID, owner, now); completed {
				c.markRuntimeDeliveryGroupMember(ctx, item.GroupID, RuntimeDeliveryKindEvent, item.ID, item.EventID, RuntimeDeliveryGroupMemberCompleted, "", now)
			}
			continue
		}
		if journalEnabled && journal.Phase == RuntimeDeliveryAttemptPrepared && eventTransport == nil {
			_, _, _ = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptFailed, now, "event handler 不支持 prepared 恢复")
			_, _ = outboxRepo.RetryRuntimeEventOutbox(context.WithoutCancel(ctx), item.ID, owner, now, "event handler 不支持 prepared 恢复")
			continue
		}
		event, getErr := eventRepo.GetAgentEvent(ctx, item.EventID)
		if getErr != nil {
			if journalEnabled {
				journal, _, _ = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptFailed, now, fmt.Sprintf("读取事件失败: %T", getErr))
			}
			_, _ = outboxRepo.RetryRuntimeEventOutbox(context.WithoutCancel(ctx), item.ID, owner, now, fmt.Sprintf("读取事件失败: %T", getErr))
			continue
		}
		if eventTransport != nil {
			transactional, transactionalOK := eventTransport.(RuntimeEventDeliveryTransactionalTransport)
			if optIn, hasOptIn := eventTransport.(RuntimeEventDeliveryTransactionalOptIn); hasOptIn && !optIn.RuntimeEventDeliveryTransactionsEnabled() {
				transactionalOK = false
			}
			abortable, abortableOK := eventTransport.(RuntimeEventDeliveryAbortableTransport)
			statusPrepared := false
			if reconciler, hasReconciler := eventTransport.(RuntimeEventDeliveryReconciler); hasReconciler {
				status, statusErr := reconciler.ReconcileEventDelivery(context.WithoutCancel(ctx), item, event)
				if statusErr != nil && !errors.Is(statusErr, ErrRuntimeDeliveryStatusUnavailable) {
					if journalEnabled {
						_, _, _ = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptFailed, now, statusErr.Error())
					}
					_, _ = outboxRepo.RetryRuntimeEventOutbox(context.WithoutCancel(ctx), item.ID, owner, now, statusErr.Error())
					continue
				}
				if statusErr == nil {
					var handled, destinationDone bool
					journal, handled, destinationDone, statusErr = c.reconcileRuntimeDeliveryStatus(context.WithoutCancel(ctx), journal, journalEnabled, owner, transactionalOK, status, now)
					if statusErr != nil {
						if journalEnabled {
							_, _, _ = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptFailed, now, statusErr.Error())
						}
						_, _ = outboxRepo.RetryRuntimeEventOutbox(context.WithoutCancel(ctx), item.ID, owner, now, statusErr.Error())
						continue
					}
					if destinationDone {
						if completed, _ := outboxRepo.CompleteRuntimeEventOutbox(context.WithoutCancel(ctx), item.ID, owner, now); completed {
							c.markRuntimeDeliveryGroupMember(ctx, item.GroupID, RuntimeDeliveryKindEvent, item.ID, item.EventID, RuntimeDeliveryGroupMemberCompleted, "", now)
						}
						continue
					}
					statusPrepared = handled && status.Phase == RuntimeDeliveryStatusPrepared
				}
			}
			deliveryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			var deliveryErr error
			var preparedEnvelope RuntimeEventDeliveryEnvelope
			preparedForAbort := journalEnabled && journal.Phase == RuntimeDeliveryAttemptPrepared
			if transactionalOK {
				var envelope RuntimeEventDeliveryEnvelope
				if statusPrepared {
					if builder, ok := eventTransport.(RuntimeEventDeliveryReconciliationEnvelopeBuilder); ok {
						envelope, deliveryErr = builder.BuildRuntimeEventDeliveryReconciliationEnvelope(item, event, now)
					} else {
						envelope, deliveryErr = transactional.BuildEnvelope(item, event)
					}
				} else {
					envelope, deliveryErr = transactional.BuildEnvelope(item, event)
				}
				if deliveryErr == nil && (!journalEnabled || journal.Phase != RuntimeDeliveryAttemptPrepared) {
					var prepared RuntimeEventDeliveryReceipt
					prepared, deliveryErr = transactional.Prepare(deliveryCtx, envelope)
					if deliveryErr == nil {
						deliveryErr = prepared.ValidateAgainstPhase(envelope, RuntimeEventDeliveryTransactionPhasePrepared)
						if deliveryErr == nil {
							preparedForAbort = true
						}
					}
					if deliveryErr == nil && journalEnabled {
						journal, _, deliveryErr = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptPrepared, now, "")
					}
				}
				if deliveryErr == nil {
					var committed RuntimeEventDeliveryReceipt
					committed, deliveryErr = transactional.Commit(deliveryCtx, envelope)
					if deliveryErr == nil {
						deliveryErr = committed.ValidateAgainstPhase(envelope, RuntimeEventDeliveryTransactionPhaseCommitted)
					}
					if deliveryErr == nil && journalEnabled {
						journal, _, deliveryErr = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptCommitted, now, "")
					}
				}
				preparedEnvelope = envelope
			} else {
				var receipt RuntimeEventDeliveryReceipt
				receiptErr := error(nil)
				// The one-phase transport remains the compatibility path. Status
				// reconciliation above may have released the cursor already; an
				// absent proof simply reaches this idempotent send.
				receiptErr = eventTransport.Deliver(deliveryCtx, item, event)
				if receiptErr == nil {
					// Legacy Deliver has no receipt return value. The transport
					// contract itself is the acknowledgement boundary.
					_ = receipt
				}
				deliveryErr = receiptErr
				if deliveryErr == nil && journalEnabled {
					journal, _, deliveryErr = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptAccepted, now, "")
				}
			}
			cancel()
			if deliveryErr != nil {
				if abortableOK && transactionalOK && preparedForAbort && item.Attempt >= MaxRuntimeEventOutboxAttempts {
					// Persist the Abort intent before acknowledging the source retry. The
					// immediate call below remains a latency optimization; the durable row
					// closes the crash window between Prepare and that best-effort call.
					deliveryErr = runtimeDeliveryCompensationEnqueueFailure(deliveryErr, c.enqueueRuntimeDeliveryCompensation(context.WithoutCancel(ctx), RuntimeDeliveryKindEvent, routeSource, routeDestination, item.ID, item.InvocationID, item.ID, item.GroupID, item.Attempt, now))
				}
				if abortableOK && transactionalOK && preparedEnvelope.DeliveryID != "" {
					compensateRuntimeDeliveryTransaction(ctx, item.Attempt >= MaxRuntimeEventOutboxAttempts, preparedForAbort, func(compensationCtx context.Context) error {
						receipt, abortErr := abortable.Abort(compensationCtx, preparedEnvelope)
						if abortErr != nil {
							return abortErr
						}
						return receipt.ValidateAgainstPhase(preparedEnvelope, RuntimeEventDeliveryTransactionPhaseAborted)
					})
				}
				if journalEnabled {
					_, _, _ = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptFailed, now, deliveryErr.Error())
				}
				_, _ = outboxRepo.RetryRuntimeEventOutbox(context.WithoutCancel(ctx), item.ID, owner, now, deliveryErr.Error())
				continue
			}
			if journalEnabled {
				journal, _, deliveryErr = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptCompleted, now, "")
				if deliveryErr != nil {
					_, _ = outboxRepo.RetryRuntimeEventOutbox(context.WithoutCancel(ctx), item.ID, owner, now, deliveryErr.Error())
					continue
				}
			}
			if completed, _ := outboxRepo.CompleteRuntimeEventOutbox(context.WithoutCancel(ctx), item.ID, owner, now); completed {
				c.markRuntimeDeliveryGroupMember(ctx, item.GroupID, RuntimeDeliveryKindEvent, item.ID, item.EventID, RuntimeDeliveryGroupMemberCompleted, "", now)
			}
			continue
		}
		deliveryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		handlerErr := handler(deliveryCtx, item, event)
		cancel()
		if handlerErr != nil {
			if journalEnabled {
				journal, _, _ = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptFailed, now, handlerErr.Error())
			}
			// Persist only a short, secret-scrubbed error summary. The event body
			// itself remains available for a later idempotent retry.
			_, _ = outboxRepo.RetryRuntimeEventOutbox(context.WithoutCancel(ctx), item.ID, owner, now, handlerErr.Error())
			continue
		}
		if journalEnabled {
			journal, _, err = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptAccepted, now, "")
			if err == nil {
				journal, _, err = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptCompleted, now, "")
			}
			if err != nil {
				_, _ = outboxRepo.RetryRuntimeEventOutbox(context.WithoutCancel(ctx), item.ID, owner, now, err.Error())
				continue
			}
		}
		if completed, _ := outboxRepo.CompleteRuntimeEventOutbox(context.WithoutCancel(ctx), item.ID, owner, now); completed {
			c.markRuntimeDeliveryGroupMember(ctx, item.GroupID, RuntimeDeliveryKindEvent, item.ID, item.EventID, RuntimeDeliveryGroupMemberCompleted, "", now)
		}
	}
}

// startRuntimeCheckpointOutboxLoop asks the shared delivery scheduler to
// start. The scheduler checks the explicit checkpoint route/transport before
// claiming a cursor, so an unconfigured host remains a no-op.
func (c *Coordinator) startRuntimeCheckpointOutboxLoop() {
	c.startRuntimeDeliveryOutboxLoop()
}

// startRuntimeApprovalRejectionDeliveryOutboxLoop shares the bounded
// scheduler lifecycle with event and checkpoint delivery. A rejection intent
// is only claimed when both the route and transport are explicitly configured.
func (c *Coordinator) startRuntimeApprovalRejectionDeliveryOutboxLoop() {
	c.startRuntimeDeliveryOutboxLoop()
}

// startRuntimeDeliveryOutboxLoop starts one shared source-side delivery
// worker. Event and checkpoint outboxes keep separate leases and worker IDs,
// but the scheduler alternates one bounded claim from each kind. This gives a
// checkpoint a chance to progress even when an event backlog is continuously
// available, and a single process cannot accidentally start duplicate loops
// when transports are installed before and after Coordinator.Start.
func (c *Coordinator) startRuntimeDeliveryOutboxLoop() {
	c.mu.Lock()
	root := c.rootCtx
	closed := c.closed
	c.mu.Unlock()
	if root == nil || closed || !c.runtimeDeliveryOutboxConfigured() {
		return
	}
	c.deliveryOutboxOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-root.Done():
					return
				case now := <-ticker.C:
					c.DispatchDueRuntimeDeliveries(context.WithoutCancel(root), now.UTC())
				}
			}
		}()
	})
}

// runtimeDeliveryOutboxConfigured reports whether at least one explicit
// cross-service source delivery path is ready. It is deliberately evaluated
// each time a setter asks the loop to start: hosts may install transports
// after Coordinator.Start without restarting the process.
func (c *Coordinator) runtimeDeliveryOutboxConfigured() bool {
	if c == nil || c.repo == nil {
		return false
	}
	c.mu.Lock()
	eventHandler := c.eventOutboxHandler
	checkpointTransport := c.checkpointDeliveryTransport
	checkpointSource := c.checkpointDeliverySource
	checkpointDestination := c.checkpointDeliveryDestination
	configTransport := c.configDeliveryTransport
	configSource := c.configDeliverySource
	configDestination := c.configDeliveryDestination
	configDirectoryTransport := c.configDirectoryTransport
	configDirectorySource := c.configDirectorySource
	configDirectoryDestination := c.configDirectoryDestination
	rebindApplyTransport := c.rebindApplyTransport
	rebindApplyRoutesConfigured := len(c.rebindApplyTransports) > 0
	rebindApplySource := c.rebindApplySource
	rebindApplyDestination := c.rebindApplyDestination
	rejectionTransport := c.rejectionDeliveryTransport
	rejectionSource := c.rejectionDeliverySource
	rejectionDestination := c.rejectionDeliveryDestination
	deliveryGroupTransport := c.deliveryGroupTransport
	c.mu.Unlock()
	if eventHandler != nil {
		if _, outboxOK := c.repo.(RuntimeEventOutboxRepository); outboxOK {
			if _, eventOK := c.repo.(AgentEventRepository); eventOK {
				return true
			}
		}
	}
	if checkpointTransport != nil && checkpointSource != "" && checkpointDestination != "" {
		if _, outboxOK := c.repo.(RuntimeCheckpointDeliveryOutboxRepository); outboxOK {
			return true
		}
	}
	if configTransport != nil && configSource != "" && configDestination != "" {
		if _, outboxOK := c.repo.(RuntimeConfigDeliveryOutboxRepository); outboxOK {
			return true
		}
	}
	if configDirectoryTransport != nil && configDirectorySource != "" && configDirectoryDestination != "" {
		if _, outboxOK := c.repo.(RuntimeConfigDirectoryOutboxRepository); outboxOK {
			return true
		}
	}
	if (rebindApplyTransport != nil && rebindApplySource != "" && rebindApplyDestination != "") || rebindApplyRoutesConfigured {
		if _, applyOK := c.repo.(RuntimeConfigDirectoryRebindApplyRepository); applyOK {
			return true
		}
	}
	if rejectionTransport != nil && rejectionSource != "" && rejectionDestination != "" {
		if _, outboxOK := c.repo.(RuntimeApprovalRejectionDeliveryOutboxRepository); outboxOK {
			return true
		}
	}
	if deliveryGroupTransport != nil {
		if marker, marked := deliveryGroupTransport.(RuntimeDeliveryGroupTransactionalOptIn); !marked || marker.RuntimeDeliveryGroupTransactionsEnabled() {
			if _, settlementOK := c.repo.(RuntimeDeliveryGroupSettlementRepository); settlementOK {
				if _, groupOK := c.repo.(RuntimeDeliveryGroupRepository); groupOK {
					return true
				}
			}
		}
	}
	if c.runtimeDeliveryCompensationConfigured() {
		return true
	}
	return false
}

// DispatchDueRuntimeDeliveries is the synchronous entry point for a host
// scheduler. It uses the same bounded round-robin path as the background
// worker, so external schedulers do not need to know the two cursor schemas or
// accidentally bypass lease/receipt validation.
func (c *Coordinator) DispatchDueRuntimeDeliveries(ctx context.Context, now time.Time) {
	if c == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	// One claim per kind per round prevents a hot event stream from starving
	// checkpoint state propagation. Each underlying dispatcher still applies
	// the same lease, retry, bounded body and receipt checks as its standalone
	// compatibility method.
	for round := 0; round < 32; round++ {
		c.dispatchDueRuntimeEventOutboxBounded(ctx, now, 1)
		c.dispatchDueRuntimeCheckpointOutboxBounded(ctx, now, 1)
		c.dispatchDueRuntimeConfigOutboxBounded(ctx, now, 1)
		c.dispatchDueRuntimeConfigDirectoryOutboxBounded(ctx, now, 1)
		c.dispatchDueRuntimeConfigDirectoryRebindAppliesBounded(ctx, now, 1)
		c.dispatchDueRuntimeApprovalRejectionOutboxBounded(ctx, now, 1)
		c.dispatchDueRuntimeCompensationsBounded(ctx, now, 1)
	}
	c.refreshRuntimeDeliveryGroups(ctx, now)
}

func (c *Coordinator) dispatchDueRuntimeApprovalRejectionOutbox(ctx context.Context, now time.Time) {
	c.dispatchDueRuntimeApprovalRejectionOutboxBounded(ctx, now, 32)
}

func (c *Coordinator) dispatchDueRuntimeApprovalRejectionOutboxBounded(ctx context.Context, now time.Time, maxClaims int) {
	repo, ok := c.repo.(RuntimeApprovalRejectionDeliveryOutboxRepository)
	if !ok {
		return
	}
	c.mu.Lock()
	transport := c.rejectionDeliveryTransport
	source := c.rejectionDeliverySource
	destination := c.rejectionDeliveryDestination
	owner := c.rejectionOutboxWorkerID
	closed := c.closed
	c.mu.Unlock()
	if transport == nil || source == "" || destination == "" || owner == "" || closed || maxClaims <= 0 {
		return
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	for attempt := 0; attempt < maxClaims; attempt++ {
		item, claimed, err := repo.ClaimRuntimeApprovalRejectionDeliveryOutbox(ctx, owner, now, 30*time.Second)
		if err != nil || !claimed {
			return
		}
		if item.Source != source || item.Destination != destination {
			_, _ = repo.RetryRuntimeApprovalRejectionDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, "approval rejection outbox route changed")
			continue
		}
		journal, journalEnabled, journalErr := c.beginRuntimeDeliveryAttempt(ctx, RuntimeDeliveryKindRejection, source, destination, item.DeliveryID, item.InvocationID, item.ID, item.Revision, item.Attempt, owner, now)
		if journalErr != nil {
			_, _ = repo.RetryRuntimeApprovalRejectionDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, journalErr.Error())
			continue
		}
		if journalEnabled && runtimeDeliveryAttemptDestinationDone(journal.Phase) {
			if journal.Phase != RuntimeDeliveryAttemptCompleted {
				journal, _, journalErr = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptCompleted, now, "")
				if journalErr != nil {
					_, _ = repo.RetryRuntimeApprovalRejectionDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, journalErr.Error())
					continue
				}
			}
			if completed, _ := repo.CompleteRuntimeApprovalRejectionDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now); completed {
				c.markRuntimeDeliveryGroupMember(ctx, item.GroupID, RuntimeDeliveryKindRejection, item.ID, item.DeliveryID, RuntimeDeliveryGroupMemberCompleted, "", now)
			}
			continue
		}
		envelope, envelopeErr := item.Envelope(now)
		if envelopeErr != nil {
			if journalEnabled {
				_, _, _ = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptFailed, now, envelopeErr.Error())
			}
			_, _ = repo.RetryRuntimeApprovalRejectionDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, envelopeErr.Error())
			continue
		}
		transactional, transactionalOK := transport.(RuntimeApprovalRejectionDeliveryTransactionalTransport)
		if optIn, hasOptIn := transport.(RuntimeApprovalRejectionDeliveryTransactionalOptIn); hasOptIn && !optIn.RuntimeApprovalRejectionDeliveryTransactionsEnabled() {
			transactionalOK = false
		}
		abortable, abortableOK := transport.(RuntimeApprovalRejectionDeliveryAbortableTransport)
		if reconciler, hasReconciler := transport.(RuntimeApprovalRejectionDeliveryReconciler); hasReconciler {
			status, statusErr := reconciler.ReconcileApprovalRejectionDelivery(context.WithoutCancel(ctx), envelope)
			if statusErr != nil && !errors.Is(statusErr, ErrRuntimeDeliveryStatusUnavailable) {
				if journalEnabled {
					_, _, _ = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptFailed, now, statusErr.Error())
				}
				_, _ = repo.RetryRuntimeApprovalRejectionDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, statusErr.Error())
				continue
			}
			if statusErr == nil {
				var handled, destinationDone bool
				journal, handled, destinationDone, statusErr = c.reconcileRuntimeDeliveryStatus(context.WithoutCancel(ctx), journal, journalEnabled, owner, transactionalOK, status, now)
				if statusErr != nil {
					if journalEnabled {
						_, _, _ = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptFailed, now, statusErr.Error())
					}
					_, _ = repo.RetryRuntimeApprovalRejectionDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, statusErr.Error())
					continue
				}
				if destinationDone {
					if completed, _ := repo.CompleteRuntimeApprovalRejectionDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now); completed {
						c.markRuntimeDeliveryGroupMember(ctx, item.GroupID, RuntimeDeliveryKindRejection, item.ID, item.DeliveryID, RuntimeDeliveryGroupMemberCompleted, "", now)
					}
					continue
				}
				_ = handled
			}
		}
		deliveryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		var deliveryErr error
		preparedForAbort := journalEnabled && journal.Phase == RuntimeDeliveryAttemptPrepared
		if transactionalOK {
			if !journalEnabled || journal.Phase != RuntimeDeliveryAttemptPrepared {
				var prepareReceipt RuntimeApprovalRejectionDeliveryReceipt
				prepareReceipt, deliveryErr = transactional.Prepare(deliveryCtx, envelope)
				if deliveryErr == nil {
					deliveryErr = prepareReceipt.ValidateAgainstPhase(envelope, RuntimeApprovalRejectionDeliveryTransactionPhasePrepared)
					if deliveryErr == nil {
						preparedForAbort = true
					}
				}
				if deliveryErr == nil && journalEnabled {
					journal, _, deliveryErr = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptPrepared, now, "")
				}
			}
			if deliveryErr == nil {
				var commitReceipt RuntimeApprovalRejectionDeliveryReceipt
				commitReceipt, deliveryErr = transactional.Commit(deliveryCtx, envelope)
				if deliveryErr == nil {
					deliveryErr = commitReceipt.ValidateAgainstPhase(envelope, RuntimeApprovalRejectionDeliveryTransactionPhaseCommitted)
				}
				if deliveryErr == nil && journalEnabled {
					journal, _, deliveryErr = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptCommitted, now, "")
				}
			}
		} else {
			var receipt RuntimeApprovalRejectionDeliveryReceipt
			receipt, deliveryErr = transport.Deliver(deliveryCtx, envelope)
			if deliveryErr == nil {
				deliveryErr = receipt.ValidateAgainst(envelope)
			}
			if deliveryErr == nil && journalEnabled {
				journal, _, deliveryErr = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptAccepted, now, "")
			}
		}
		cancel()
		if deliveryErr != nil {
			if abortableOK && transactionalOK && preparedForAbort && item.Attempt >= MaxRuntimeApprovalRejectionDeliveryAttempts {
				deliveryErr = runtimeDeliveryCompensationEnqueueFailure(deliveryErr, c.enqueueRuntimeDeliveryCompensation(context.WithoutCancel(ctx), RuntimeDeliveryKindRejection, source, destination, item.DeliveryID, item.InvocationID, item.ID, item.GroupID, item.Attempt, now))
			}
			if abortableOK && transactionalOK && envelope.DeliveryID != "" {
				compensateRuntimeDeliveryTransaction(ctx, item.Attempt >= MaxRuntimeApprovalRejectionDeliveryAttempts, preparedForAbort, func(compensationCtx context.Context) error {
					receipt, abortErr := abortable.Abort(compensationCtx, envelope)
					if abortErr != nil {
						return abortErr
					}
					return receipt.ValidateAgainstPhase(envelope, RuntimeApprovalRejectionDeliveryTransactionPhaseAborted)
				})
			}
			if journalEnabled {
				_, _, _ = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptFailed, now, deliveryErr.Error())
			}
			_, _ = repo.RetryRuntimeApprovalRejectionDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, deliveryErr.Error())
			continue
		}
		if journalEnabled {
			journal, _, journalErr = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptCompleted, now, "")
			if journalErr != nil {
				_, _ = repo.RetryRuntimeApprovalRejectionDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, journalErr.Error())
				continue
			}
		}
		if completed, _ := repo.CompleteRuntimeApprovalRejectionDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now); completed {
			c.markRuntimeDeliveryGroupMember(ctx, item.GroupID, RuntimeDeliveryKindRejection, item.ID, item.DeliveryID, RuntimeDeliveryGroupMemberCompleted, "", now)
		}
	}
}

// dispatchDueRuntimeCheckpointOutbox claims and sends the historical bounded
// checkpoint batch. The shared scheduler calls the one-item variant below to
// preserve round-robin fairness across event and checkpoint cursors. The
// projection is already metadata-only and immutable; the transport creates a
// fresh signed timestamp for each attempt. A crash between receiver commit
// and Complete is therefore safe because the destination inbox is idempotent.
func (c *Coordinator) dispatchDueRuntimeCheckpointOutbox(ctx context.Context, now time.Time) {
	c.dispatchDueRuntimeCheckpointOutboxBounded(ctx, now, 32)
}

func (c *Coordinator) dispatchDueRuntimeCheckpointOutboxBounded(ctx context.Context, now time.Time, maxClaims int) {
	repo, ok := c.repo.(RuntimeCheckpointDeliveryOutboxRepository)
	if !ok {
		return
	}
	c.mu.Lock()
	transport := c.checkpointDeliveryTransport
	source := c.checkpointDeliverySource
	destination := c.checkpointDeliveryDestination
	owner := c.checkpointOutboxWorkerID
	closed := c.closed
	c.mu.Unlock()
	if transport == nil || source == "" || destination == "" || owner == "" || closed {
		return
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if maxClaims <= 0 {
		return
	}
	for attempt := 0; attempt < maxClaims; attempt++ {
		item, claimed, err := repo.ClaimRuntimeCheckpointDeliveryOutbox(ctx, owner, now, 30*time.Second)
		if err != nil || !claimed {
			return
		}
		if item.Source != source || item.Destination != destination {
			_, _ = repo.RetryRuntimeCheckpointDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, "checkpoint outbox route changed")
			continue
		}
		if applies, ready, barrierErr := c.runtimeCheckpointEventBarrier(context.WithoutCancel(ctx), item); applies && (!ready || barrierErr != nil) {
			message := "checkpoint 等待 event high-water 完成"
			if barrierErr != nil {
				message = fmt.Sprintf("读取 event high-water 失败: %T", barrierErr)
			}
			deferAt := now.Add(time.Second)
			if barrierErr != nil {
				deferAt = now.Add(RuntimeCheckpointDeliveryOutboxBackoff(item.Attempt))
			}
			deferrer, hasDeferrer := repo.(RuntimeCheckpointDeliveryOutboxDeferrer)
			if hasDeferrer {
				_, _ = deferrer.DeferRuntimeCheckpointDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, deferAt, message)
			}
			continue
		}
		journal, journalEnabled, journalErr := c.beginRuntimeDeliveryAttempt(ctx, RuntimeDeliveryKindCheckpoint, source, destination, item.DeliveryID, item.InvocationID, item.ID, item.Revision, item.Attempt, owner, now)
		if journalErr != nil {
			_, _ = repo.RetryRuntimeCheckpointDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, journalErr.Error())
			continue
		}
		if journalEnabled && runtimeDeliveryAttemptDestinationDone(journal.Phase) {
			if journal.Phase != RuntimeDeliveryAttemptCompleted {
				journal, _, journalErr = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptCompleted, now, "")
				if journalErr != nil {
					_, _ = repo.RetryRuntimeCheckpointDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, journalErr.Error())
					continue
				}
			}
			if completed, _ := repo.CompleteRuntimeCheckpointDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now); completed {
				c.markRuntimeDeliveryGroupMember(ctx, item.GroupID, RuntimeDeliveryKindCheckpoint, item.ID, item.DeliveryID, RuntimeDeliveryGroupMemberCompleted, "", now)
			}
			continue
		}
		envelope, envelopeErr := item.Envelope(now)
		if envelopeErr != nil {
			if journalEnabled {
				_, _, _ = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptFailed, now, envelopeErr.Error())
			}
			_, _ = repo.RetryRuntimeCheckpointDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, envelopeErr.Error())
			continue
		}
		transactional, transactionalOK := transport.(RuntimeCheckpointDeliveryTransactionalTransport)
		if transactionalOK {
			if optIn, hasOptIn := transport.(RuntimeCheckpointDeliveryTransactionalOptIn); hasOptIn && !optIn.RuntimeCheckpointDeliveryTransactionsEnabled() {
				transactionalOK = false
			}
		}
		abortable, abortableOK := transport.(RuntimeCheckpointDeliveryAbortableTransport)
		if reconciler, hasReconciler := transport.(RuntimeCheckpointDeliveryReconciler); hasReconciler {
			status, statusErr := reconciler.ReconcileCheckpointDelivery(context.WithoutCancel(ctx), envelope, item.Projection)
			if statusErr != nil && !errors.Is(statusErr, ErrRuntimeDeliveryStatusUnavailable) {
				if journalEnabled {
					_, _, _ = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptFailed, now, statusErr.Error())
				}
				_, _ = repo.RetryRuntimeCheckpointDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, statusErr.Error())
				continue
			}
			if statusErr == nil {
				var handled, destinationDone bool
				journal, handled, destinationDone, statusErr = c.reconcileRuntimeDeliveryStatus(context.WithoutCancel(ctx), journal, journalEnabled, owner, transactionalOK, status, now)
				if statusErr != nil {
					if journalEnabled {
						_, _, _ = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptFailed, now, statusErr.Error())
					}
					_, _ = repo.RetryRuntimeCheckpointDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, statusErr.Error())
					continue
				}
				if destinationDone {
					if completed, _ := repo.CompleteRuntimeCheckpointDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now); completed {
						c.markRuntimeDeliveryGroupMember(ctx, item.GroupID, RuntimeDeliveryKindCheckpoint, item.ID, item.DeliveryID, RuntimeDeliveryGroupMemberCompleted, "", now)
					}
					continue
				}
				_ = handled // prepared proof is consumed by the transaction branch below
			}
		}
		deliveryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		var deliveryErr error
		preparedForAbort := journalEnabled && journal.Phase == RuntimeDeliveryAttemptPrepared
		if transactionalOK {
			if !journalEnabled || journal.Phase != RuntimeDeliveryAttemptPrepared {
				var prepareReceipt RuntimeCheckpointDeliveryReceipt
				prepareReceipt, deliveryErr = transactional.Prepare(deliveryCtx, envelope, item.Projection)
				if deliveryErr == nil {
					deliveryErr = prepareReceipt.ValidateAgainstPhase(envelope, RuntimeCheckpointDeliveryTransactionPhasePrepared)
					if deliveryErr == nil {
						preparedForAbort = true
					}
				}
				if deliveryErr == nil && journalEnabled {
					journal, _, deliveryErr = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptPrepared, now, "")
				}
			}
			if deliveryErr == nil {
				var commitReceipt RuntimeCheckpointDeliveryReceipt
				commitReceipt, deliveryErr = transactional.Commit(deliveryCtx, envelope, item.Projection)
				if deliveryErr == nil {
					deliveryErr = commitReceipt.ValidateAgainstPhase(envelope, RuntimeCheckpointDeliveryTransactionPhaseCommitted)
				}
				if deliveryErr == nil && journalEnabled {
					journal, _, deliveryErr = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptCommitted, now, "")
				}
			}
		} else {
			var receipt RuntimeCheckpointDeliveryReceipt
			receipt, deliveryErr = transport.Deliver(deliveryCtx, envelope, item.Projection)
			if deliveryErr == nil {
				deliveryErr = receipt.ValidateAgainst(envelope)
			}
			if deliveryErr == nil && journalEnabled {
				journal, _, deliveryErr = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptAccepted, now, "")
			}
		}
		cancel()
		if deliveryErr != nil {
			if abortableOK && transactionalOK && preparedForAbort && item.Attempt >= MaxRuntimeCheckpointDeliveryOutboxAttempts {
				deliveryErr = runtimeDeliveryCompensationEnqueueFailure(deliveryErr, c.enqueueRuntimeDeliveryCompensation(context.WithoutCancel(ctx), RuntimeDeliveryKindCheckpoint, source, destination, item.DeliveryID, item.InvocationID, item.ID, item.GroupID, item.Attempt, now))
			}
			if abortableOK && transactionalOK && envelope.DeliveryID != "" {
				compensateRuntimeDeliveryTransaction(ctx, item.Attempt >= MaxRuntimeCheckpointDeliveryOutboxAttempts, preparedForAbort, func(compensationCtx context.Context) error {
					receipt, abortErr := abortable.Abort(compensationCtx, envelope, item.Projection)
					if abortErr != nil {
						return abortErr
					}
					return receipt.ValidateAgainstPhase(envelope, RuntimeCheckpointDeliveryTransactionPhaseAborted)
				})
			}
			if journalEnabled {
				_, _, _ = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptFailed, now, deliveryErr.Error())
			}
			_, _ = repo.RetryRuntimeCheckpointDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, deliveryErr.Error())
			continue
		}
		if journalEnabled {
			journal, _, journalErr = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptCompleted, now, "")
			if journalErr != nil {
				_, _ = repo.RetryRuntimeCheckpointDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, journalErr.Error())
				continue
			}
		}
		if completed, _ := repo.CompleteRuntimeCheckpointDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now); completed {
			c.markRuntimeDeliveryGroupMember(ctx, item.GroupID, RuntimeDeliveryKindCheckpoint, item.ID, item.DeliveryID, RuntimeDeliveryGroupMemberCompleted, "", now)
		}
	}
}

// dispatchDueRuntimeConfigOutbox sends the bounded metadata-only migration
// cursor. The destination receiver decides whether to use one-phase Inbox
// acceptance or its optional Prepare/Commit ledger; no destination Worker is
// started by this path.
func (c *Coordinator) dispatchDueRuntimeConfigOutbox(ctx context.Context, now time.Time) {
	c.dispatchDueRuntimeConfigOutboxBounded(ctx, now, 32)
}

func (c *Coordinator) dispatchDueRuntimeConfigOutboxBounded(ctx context.Context, now time.Time, maxClaims int) {
	repo, ok := c.repo.(RuntimeConfigDeliveryOutboxRepository)
	if !ok {
		return
	}
	c.mu.Lock()
	transport := c.configDeliveryTransport
	source := c.configDeliverySource
	destination := c.configDeliveryDestination
	owner := c.configOutboxWorkerID
	closed := c.closed
	c.mu.Unlock()
	if transport == nil || source == "" || destination == "" || owner == "" || closed || maxClaims <= 0 {
		return
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	for attempt := 0; attempt < maxClaims; attempt++ {
		item, claimed, err := repo.ClaimRuntimeConfigDeliveryOutbox(ctx, owner, now, 30*time.Second)
		if err != nil || !claimed {
			return
		}
		if item.Source != source || item.Destination != destination {
			_, _ = repo.RetryRuntimeConfigDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, "Runtime config outbox route changed")
			continue
		}
		journal, journalEnabled, journalErr := c.beginRuntimeDeliveryAttempt(ctx, RuntimeDeliveryKindConfig, source, destination, item.DeliveryID, item.InvocationID, item.ID, item.Revision, item.Attempt, owner, now)
		if journalErr != nil {
			_, _ = repo.RetryRuntimeConfigDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, journalErr.Error())
			continue
		}
		if journalEnabled && runtimeDeliveryAttemptDestinationDone(journal.Phase) {
			if journal.Phase != RuntimeDeliveryAttemptCompleted {
				journal, _, journalErr = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptCompleted, now, "")
				if journalErr != nil {
					_, _ = repo.RetryRuntimeConfigDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, journalErr.Error())
					continue
				}
			}
			if completed, _ := repo.CompleteRuntimeConfigDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now); completed {
				c.markRuntimeDeliveryGroupMember(ctx, item.GroupID, RuntimeDeliveryKindConfig, item.ID, item.DeliveryID, RuntimeDeliveryGroupMemberCompleted, "", now)
			}
			continue
		}
		envelope, envelopeErr := item.Envelope(now)
		if envelopeErr != nil {
			if journalEnabled {
				_, _, _ = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptFailed, now, envelopeErr.Error())
			}
			_, _ = repo.RetryRuntimeConfigDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, envelopeErr.Error())
			continue
		}
		transactional, transactionalOK := transport.(RuntimeConfigDeliveryTransactionalTransport)
		if optIn, hasOptIn := transport.(RuntimeConfigDeliveryTransactionalOptIn); hasOptIn && !optIn.RuntimeConfigDeliveryTransactionsEnabled() {
			transactionalOK = false
		}
		abortable, abortableOK := transport.(RuntimeConfigDeliveryAbortableTransport)
		if reconciler, hasReconciler := transport.(RuntimeConfigDeliveryReconciler); hasReconciler {
			status, statusErr := reconciler.ReconcileConfigDelivery(context.WithoutCancel(ctx), envelope, item.Projection)
			if statusErr != nil && !errors.Is(statusErr, ErrRuntimeDeliveryStatusUnavailable) {
				if journalEnabled {
					_, _, _ = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptFailed, now, statusErr.Error())
				}
				_, _ = repo.RetryRuntimeConfigDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, statusErr.Error())
				continue
			}
			if statusErr == nil {
				var handled, destinationDone bool
				journal, handled, destinationDone, statusErr = c.reconcileRuntimeDeliveryStatus(context.WithoutCancel(ctx), journal, journalEnabled, owner, transactionalOK, status, now)
				if statusErr != nil {
					if journalEnabled {
						_, _, _ = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptFailed, now, statusErr.Error())
					}
					_, _ = repo.RetryRuntimeConfigDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, statusErr.Error())
					continue
				}
				if destinationDone {
					if completed, _ := repo.CompleteRuntimeConfigDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now); completed {
						c.markRuntimeDeliveryGroupMember(ctx, item.GroupID, RuntimeDeliveryKindConfig, item.ID, item.DeliveryID, RuntimeDeliveryGroupMemberCompleted, "", now)
					}
					continue
				}
				_ = handled
			}
		}
		deliveryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		var deliveryErr error
		preparedForAbort := journalEnabled && journal.Phase == RuntimeDeliveryAttemptPrepared
		if transactionalOK {
			if !journalEnabled || journal.Phase != RuntimeDeliveryAttemptPrepared {
				var prepareReceipt RuntimeConfigDeliveryReceipt
				prepareReceipt, deliveryErr = transactional.Prepare(deliveryCtx, envelope, item.Projection)
				if deliveryErr == nil {
					deliveryErr = prepareReceipt.ValidateAgainstPhase(envelope, RuntimeConfigDeliveryTransactionPhasePrepared)
					if deliveryErr == nil {
						preparedForAbort = true
					}
				}
				if deliveryErr == nil && journalEnabled {
					journal, _, deliveryErr = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptPrepared, now, "")
				}
			}
			if deliveryErr == nil {
				var commitReceipt RuntimeConfigDeliveryReceipt
				commitReceipt, deliveryErr = transactional.Commit(deliveryCtx, envelope, item.Projection)
				if deliveryErr == nil {
					deliveryErr = commitReceipt.ValidateAgainstPhase(envelope, RuntimeConfigDeliveryTransactionPhaseCommitted)
				}
				if deliveryErr == nil && journalEnabled {
					journal, _, deliveryErr = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptCommitted, now, "")
				}
			}
		} else {
			var receipt RuntimeConfigDeliveryReceipt
			receipt, deliveryErr = transport.Deliver(deliveryCtx, envelope, item.Projection)
			if deliveryErr == nil {
				deliveryErr = receipt.ValidateAgainst(envelope)
			}
			if deliveryErr == nil && journalEnabled {
				journal, _, deliveryErr = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptAccepted, now, "")
			}
		}
		cancel()
		if deliveryErr != nil {
			if abortableOK && transactionalOK && preparedForAbort && item.Attempt >= MaxRuntimeConfigDeliveryAttempts {
				deliveryErr = runtimeDeliveryCompensationEnqueueFailure(deliveryErr, c.enqueueRuntimeDeliveryCompensation(context.WithoutCancel(ctx), RuntimeDeliveryKindConfig, source, destination, item.DeliveryID, item.InvocationID, item.ID, item.GroupID, item.Attempt, now))
			}
			if abortableOK && transactionalOK && envelope.DeliveryID != "" {
				compensateRuntimeDeliveryTransaction(ctx, item.Attempt >= MaxRuntimeConfigDeliveryAttempts, preparedForAbort, func(compensationCtx context.Context) error {
					receipt, abortErr := abortable.Abort(compensationCtx, envelope, item.Projection)
					if abortErr != nil {
						return abortErr
					}
					return receipt.ValidateAgainstPhase(envelope, RuntimeConfigDeliveryTransactionPhaseAborted)
				})
			}
			if journalEnabled {
				_, _, _ = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptFailed, now, deliveryErr.Error())
			}
			_, _ = repo.RetryRuntimeConfigDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, deliveryErr.Error())
			continue
		}
		if journalEnabled {
			journal, _, journalErr = c.advanceRuntimeDeliveryAttempt(context.WithoutCancel(ctx), journal, owner, RuntimeDeliveryAttemptCompleted, now, "")
			if journalErr != nil {
				_, _ = repo.RetryRuntimeConfigDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, journalErr.Error())
				continue
			}
		}
		if completed, _ := repo.CompleteRuntimeConfigDeliveryOutbox(context.WithoutCancel(ctx), item.ID, owner, now); completed {
			c.markRuntimeDeliveryGroupMember(ctx, item.GroupID, RuntimeDeliveryKindConfig, item.ID, item.DeliveryID, RuntimeDeliveryGroupMemberCompleted, "", now)
		}
	}
}

// dispatchDueRuntimeConfigDirectoryOutboxBounded drains the source-side
// public profile/persona body cursors. A destination status proof is checked
// first when the transport supports reconciliation; an unavailable status
// endpoint falls back to the receiver's idempotent Deliver path.
func (c *Coordinator) dispatchDueRuntimeConfigDirectoryOutboxBounded(ctx context.Context, now time.Time, maxClaims int) {
	repo, ok := c.repo.(RuntimeConfigDirectoryOutboxRepository)
	if !ok {
		return
	}
	c.mu.Lock()
	transport := c.configDirectoryTransport
	source := c.configDirectorySource
	destination := c.configDirectoryDestination
	owner := c.configDirectoryOutboxWorkerID
	closed := c.closed
	c.mu.Unlock()
	if transport == nil || source == "" || destination == "" || owner == "" || closed || maxClaims <= 0 {
		return
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for attempt := 0; attempt < maxClaims; attempt++ {
		item, claimed, err := repo.ClaimRuntimeConfigDirectoryOutbox(ctx, owner, now, 30*time.Second)
		if err != nil || !claimed {
			return
		}
		if item.Source != source || item.Destination != destination {
			_, _ = repo.RetryRuntimeConfigDirectoryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, "Runtime config directory outbox route changed")
			continue
		}
		envelope, envelopeErr := item.Envelope(now)
		if envelopeErr != nil {
			_, _ = repo.RetryRuntimeConfigDirectoryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, envelopeErr.Error())
			continue
		}
		if reconciler, hasReconciler := transport.(RuntimeConfigDirectoryReconciler); hasReconciler {
			status, statusErr := reconciler.ReconcileRuntimeConfigDirectory(context.WithoutCancel(ctx), envelope, item.Entry)
			if statusErr != nil && !errors.Is(statusErr, ErrRuntimeConfigDirectoryUnavailable) {
				_, _ = repo.RetryRuntimeConfigDirectoryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, statusErr.Error())
				continue
			}
			if statusErr == nil {
				if validateErr := status.ValidateAgainst(envelope); validateErr != nil {
					_, _ = repo.RetryRuntimeConfigDirectoryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, validateErr.Error())
					continue
				}
				if status.Phase == RuntimeConfigDirectoryStatusAccepted && status.Found {
					if completed, _ := repo.CompleteRuntimeConfigDirectoryOutbox(context.WithoutCancel(ctx), item.ID, owner, now); !completed {
						_, _ = repo.RetryRuntimeConfigDirectoryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, "directory outbox status proof 收口失败")
					}
					continue
				}
			}
		}
		deliveryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		receipt, deliveryErr := transport.Deliver(deliveryCtx, envelope, item.Entry)
		cancel()
		if deliveryErr == nil {
			deliveryErr = receipt.ValidateAgainst(envelope)
		}
		if deliveryErr != nil {
			_, _ = repo.RetryRuntimeConfigDirectoryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, deliveryErr.Error())
			continue
		}
		if completed, _ := repo.CompleteRuntimeConfigDirectoryOutbox(context.WithoutCancel(ctx), item.ID, owner, now); !completed {
			_, _ = repo.RetryRuntimeConfigDirectoryOutbox(context.WithoutCancel(ctx), item.ID, owner, now, "directory outbox 完成确认失败")
		}
	}
}

func (c *Coordinator) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	if c.rootCancel != nil {
		c.rootCancel()
	}
	for _, handle := range c.runs {
		if handle.cancel != nil {
			handle.cancel()
		}
	}
	for id, subscribers := range c.subscribers {
		for ch := range subscribers {
			close(ch)
		}
		delete(c.subscribers, id)
	}
	c.mu.Unlock()
	return nil
}

func (c *Coordinator) StartInvocation(ctx context.Context, request agent.ChatRequest) (Invocation, error) {
	if err := c.ensureStarted(); err != nil {
		return Invocation{}, err
	}
	request.UserID = strings.TrimSpace(request.UserID)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	request.ConversationID = strings.TrimSpace(request.ConversationID)
	request.SessionID = strings.TrimSpace(request.SessionID)
	if request.SessionID == "" {
		request.SessionID = request.ConversationID
	}
	if request.UserID == "" {
		return Invocation{}, errors.New("user_id 不能为空")
	}
	if request.ConversationID == "" || request.SessionID == "" {
		return Invocation{}, errors.New("conversation_id 不能为空")
	}
	if strings.TrimSpace(request.Message) == "" && request.ResumeContent == nil && len(request.Attachments) == 0 {
		return Invocation{}, errors.New("消息内容不能为空")
	}
	// 群历史只接收有界内容，避免持久化任务和模型上下文被群消息无限放大。
	if len(request.GroupContext) > 64*1024 {
		return Invocation{}, errors.New("群聊背景超出长度上限")
	}
	// Serialize the read/check/create admission window. The persistent store's
	// invocation lease protects execution after admission; this lock closes the
	// duplicate active invocation race for the current process.
	c.admissionMu.Lock()
	defer c.admissionMu.Unlock()
	if request.IdempotencyKey != "" {
		if lookup, ok := c.repo.(InvocationIdempotencyRepository); ok {
			existing, lookupErr := lookup.GetInvocationByIdempotencyKey(ctx, request.UserID, request.ConversationID, request.IdempotencyKey)
			if lookupErr == nil {
				return existing, nil
			}
			if !errors.Is(lookupErr, ErrNotFound) {
				return Invocation{}, lookupErr
			}
		}
	}
	active, err := c.repo.ListInvocations(ctx, request.UserID, []InvocationStatus{
		InvocationQueued, InvocationRunning, InvocationWaitingApproval, InvocationWaitingTool, InvocationWaitingUser, InvocationCancelling,
	})
	if err != nil {
		return Invocation{}, err
	}
	for _, existing := range active {
		if existing.ConversationID == request.ConversationID {
			return Invocation{}, fmt.Errorf("%w: 当前对话已有运行中的 invocation %s", ErrConflict, existing.ID)
		}
	}
	now := time.Now().UTC()
	workspaceID := ""
	if request.WorkspaceID != nil {
		workspaceID = strings.TrimSpace(*request.WorkspaceID)
	}
	var baseline *WorktreeBaseline
	if workspaceID != "" {
		c.mu.Lock()
		resolver := c.baselineResolver
		c.mu.Unlock()
		if resolver != nil {
			if _, ok := c.repo.(WorktreeBaselineRepository); !ok {
				return Invocation{}, errors.New("当前 Runtime 存储未启用 WorktreeBaseline")
			}
			captured, captureErr := resolver(ctx, workspaceID, request.TargetPath)
			if captureErr != nil {
				return Invocation{}, fmt.Errorf("捕获工作区基线失败: %w", captureErr)
			}
			captured.InvocationID = "pending"
			captured.WorkspaceID = workspaceID
			captured.TargetPath = strings.TrimSpace(request.TargetPath)
			normalized, normalizeErr := captured.Normalize(now)
			if normalizeErr != nil {
				return Invocation{}, fmt.Errorf("工作区基线无效: %w", normalizeErr)
			}
			baseline = &normalized
		}
	}
	item := Invocation{
		ID: newID("invocation"), UserID: request.UserID, IdempotencyKey: request.IdempotencyKey, BotID: request.BotID,
		ConversationID: request.ConversationID, WorkspaceID: workspaceID, TargetPath: strings.TrimSpace(request.TargetPath), SessionID: request.SessionID,
		ProviderID: strings.TrimSpace(request.ProviderID), ModelID: strings.TrimSpace(request.ModelID),
		Message: request.Message, GroupContext: request.GroupContext, Proactive: request.Proactive, Attachments: cloneAttachments(request.Attachments),
		Status: InvocationQueued, CreatedAt: now, UpdatedAt: now,
	}
	if err := c.repo.CreateInvocation(ctx, item); err != nil {
		return Invocation{}, err
	}
	if baseline != nil {
		baseline.InvocationID = item.ID
		baseline.WorkspaceID = item.WorkspaceID
		baseline.TargetPath = item.TargetPath
		normalizedBaseline, normalizeErr := baseline.Normalize(now)
		if normalizeErr != nil {
			_, _ = c.repo.TransitionInvocation(context.WithoutCancel(ctx), item.ID, item.Status, InvocationFailed, "工作区基线无效，任务未启动")
			return Invocation{}, normalizeErr
		}
		baseline = &normalizedBaseline
		baselineRepo := c.repo.(WorktreeBaselineRepository)
		if _, saveErr := baselineRepo.SaveWorkspaceBaseline(ctx, *baseline); saveErr != nil {
			message := "保存工作区基线失败: " + saveErr.Error()
			_, _ = c.repo.TransitionInvocation(context.WithoutCancel(ctx), item.ID, item.Status, InvocationFailed, message)
			return Invocation{}, fmt.Errorf("%s", message)
		}
	}
	if planRepo, ok := c.repo.(TaskPlanRepository); ok {
		if err := planRepo.CreateTaskPlan(ctx, initialTaskPlan(item.ID, now)); err != nil {
			return Invocation{}, fmt.Errorf("创建任务计划失败: %w", err)
		}
	}
	if _, err := c.appendAndPublish(ctx, AgentEvent{ID: newID("event"), InvocationID: item.ID, Type: EventInvocationQueued, Timestamp: now, Data: map[string]any{"conversation_id": item.ConversationID}}); err != nil {
		return Invocation{}, err
	}
	if baseline != nil {
		if _, err := c.appendAndPublish(ctx, AgentEvent{ID: newID("event"), InvocationID: item.ID, Type: EventRepositoryBaseline, Timestamp: baseline.CapturedAt, Data: map[string]any{
			"workspace_id": baseline.WorkspaceID, "target_path": baseline.TargetPath, "repository_type": baseline.RepositoryType,
			"head_revision": baseline.HeadRevision, "branch": baseline.Branch, "status_digest": baseline.StatusDigest,
			"changed_path_count": len(baseline.ChangedPaths), "status_known": baseline.StatusKnown, "truncated": baseline.Truncated,
		}}); err != nil {
			return Invocation{}, err
		}
	}
	contract := InitialTaskContract(item.ID, item.Message, now)
	if contractRepo, ok := c.repo.(TaskContractRepository); ok {
		if err := contractRepo.CreateTaskContract(ctx, contract); err != nil {
			return Invocation{}, fmt.Errorf("创建任务契约失败: %w", err)
		}
		if _, err := c.appendAndPublish(ctx, AgentEvent{ID: newID("event"), InvocationID: item.ID, Type: EventTaskContractUpdated, Timestamp: contract.UpdatedAt, Data: map[string]any{"contract": contract, "reason": "initial"}}); err != nil {
			return Invocation{}, err
		}
	}
	// TaskPlan and TaskContract are optional compatibility extensions. When a
	// host enables TaskPlan without persisting Contract history, use the same
	// deterministic admission contract in-memory so coding tasks still receive
	// their inspect→edit→verify evidence boundary.
	if err := c.ensureDefaultWorkflowPlan(ctx, item, contract); err != nil {
		return Invocation{}, fmt.Errorf("创建默认工作流计划失败: %w", err)
	}
	if _, snapshotOK := c.repo.(RuntimeSnapshotRepository); snapshotOK {
		if _, snapshotErr := c.RebuildRuntimeSnapshot(ctx, item.ID); snapshotErr != nil {
			return Invocation{}, fmt.Errorf("创建 Runtime Snapshot 失败: %w", snapshotErr)
		}
	}
	c.launch(item.ID, request)
	return item, nil
}

func (c *Coordinator) GetInvocation(ctx context.Context, id string) (Invocation, error) {
	return c.repo.GetInvocation(ctx, strings.TrimSpace(id))
}

// GetWorkspaceBaseline returns the immutable admission-time worktree fact for
// one Invocation. It never re-scans the workspace, so a report remains tied
// to the state that preceded the task.
func (c *Coordinator) GetWorkspaceBaseline(ctx context.Context, invocationID string) (WorktreeBaseline, error) {
	if c == nil || c.repo == nil {
		return WorktreeBaseline{}, errors.New("Runtime Coordinator 不能为空")
	}
	invocationID = strings.TrimSpace(invocationID)
	if _, err := c.repo.GetInvocation(ctx, invocationID); err != nil {
		return WorktreeBaseline{}, err
	}
	repo, ok := c.repo.(WorktreeBaselineRepository)
	if !ok {
		return WorktreeBaseline{}, errors.New("当前 Runtime 存储未启用 WorktreeBaseline")
	}
	return repo.GetWorkspaceBaseline(ctx, invocationID)
}

// GetWorktreeBaseline is the repository-oriented alias for
// GetWorkspaceBaseline.
func (c *Coordinator) GetWorktreeBaseline(ctx context.Context, invocationID string) (WorktreeBaseline, error) {
	return c.GetWorkspaceBaseline(ctx, invocationID)
}

// GetEvalInput returns the durable, metadata-only projection used by a
// deterministic evaluation. It validates the Invocation first and reads all
// available events in sequence order; no model or tool is executed.
func (c *Coordinator) GetEvalInput(ctx context.Context, invocationID string) (EvalInput, error) {
	invocationID = strings.TrimSpace(invocationID)
	invocation, err := c.repo.GetInvocation(ctx, invocationID)
	if err != nil {
		return EvalInput{}, err
	}
	events, err := c.listAllInvocationEvents(ctx, invocationID, 10000)
	if err != nil {
		return EvalInput{}, err
	}
	input := EvalInput{Invocation: invocation, Events: events}
	if toolRepo, ok := c.repo.(ToolCallRepository); ok {
		input.ToolCalls, err = toolRepo.ListToolCalls(ctx, invocationID)
		if err != nil {
			return EvalInput{}, err
		}
	}
	input.Approvals, err = c.repo.ListApprovals(ctx, invocationID, "")
	if err != nil {
		return EvalInput{}, err
	}
	return input, nil
}

// CreateEvalRun persists one deterministic evaluation result. Runs are
// immutable: callers cannot overwrite an existing ID or silently change the
// case definition/result after it has been recorded.
func (c *Coordinator) CreateEvalRun(ctx context.Context, run EvalRun) (EvalRun, error) {
	run.InvocationID = strings.TrimSpace(run.InvocationID)
	if _, err := c.repo.GetInvocation(ctx, run.InvocationID); err != nil {
		return EvalRun{}, err
	}
	repo, ok := c.repo.(EvalRunRepository)
	if !ok {
		return EvalRun{}, errors.New("当前 Runtime 存储未启用 EvalRun")
	}
	if strings.TrimSpace(run.ID) == "" {
		run.ID = newID("eval")
	}
	if strings.TrimSpace(run.Kind) == "" {
		run.Kind = "deterministic"
	}
	if run.Status == "" {
		run.Status = EvalRunCompleted
	}
	if run.CreatedAt.IsZero() {
		run.CreatedAt = time.Now().UTC()
	}
	if run.Status == EvalRunCompleted && run.FinishedAt == nil {
		finished := time.Now().UTC()
		run.FinishedAt = &finished
	}
	if err := run.Validate(); err != nil {
		return EvalRun{}, err
	}
	if err := repo.CreateEvalRun(ctx, run); err != nil {
		return EvalRun{}, err
	}
	return run, nil
}

func (c *Coordinator) GetEvalRun(ctx context.Context, id string) (EvalRun, error) {
	repo, ok := c.repo.(EvalRunRepository)
	if !ok {
		return EvalRun{}, errors.New("当前 Runtime 存储未启用 EvalRun")
	}
	return repo.GetEvalRun(ctx, strings.TrimSpace(id))
}

func (c *Coordinator) ListEvalRuns(ctx context.Context, invocationID string) ([]EvalRun, error) {
	invocationID = strings.TrimSpace(invocationID)
	if invocationID != "" {
		if _, err := c.repo.GetInvocation(ctx, invocationID); err != nil {
			return nil, err
		}
	}
	repo, ok := c.repo.(EvalRunRepository)
	if !ok {
		return nil, errors.New("当前 Runtime 存储未启用 EvalRun")
	}
	return repo.ListEvalRuns(ctx, invocationID)
}

// GetTaskPlan returns the durable plan for one Invocation. Older invocations
// created before the P1 migration receive an empty initial plan lazily, so a
// newly exposed API does not make historical tasks unreadable.
func (c *Coordinator) GetTaskPlan(ctx context.Context, invocationID string) (TaskPlan, error) {
	invocationID = strings.TrimSpace(invocationID)
	invocation, err := c.repo.GetInvocation(ctx, invocationID)
	if err != nil {
		return TaskPlan{}, err
	}
	planRepo, ok := c.repo.(TaskPlanRepository)
	if !ok {
		return TaskPlan{}, errors.New("当前 Runtime 存储未启用 TaskPlan")
	}
	plan, err := planRepo.GetTaskPlan(ctx, invocationID)
	if err == nil {
		return plan, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return TaskPlan{}, err
	}
	plan = initialTaskPlan(invocation.ID, invocation.CreatedAt)
	if createErr := planRepo.CreateTaskPlan(ctx, plan); createErr != nil {
		if errors.Is(createErr, ErrConflict) {
			return planRepo.GetTaskPlan(ctx, invocationID)
		}
		return TaskPlan{}, createErr
	}
	return plan, nil
}

// ensureDefaultWorkflowPlan installs the deterministic evidence boundary for
// a coding contract when the repository exposes TaskPlan. It is safe to call
// after admission and after a contract correction: an existing non-empty or
// explicitly blocked plan is never replaced, and terminal invocations are
// left untouched so a late contract edit cannot reopen execution.
func (c *Coordinator) ensureDefaultWorkflowPlan(ctx context.Context, invocation Invocation, contract TaskContract) error {
	if c == nil || invocation.Status.Terminal() {
		return nil
	}
	planRepo, ok := c.repo.(TaskPlanRepository)
	if !ok {
		return nil
	}
	current, err := planRepo.GetTaskPlan(ctx, invocation.ID)
	if errors.Is(err, ErrNotFound) {
		current, err = c.GetTaskPlan(ctx, invocation.ID)
	}
	if err != nil {
		return err
	}
	if len(current.Steps) > 0 || current.Status != PlanPending || strings.TrimSpace(current.Blocker) != "" {
		return nil
	}
	workflowPlan, shouldCreate := defaultWorkflowPlan(invocation, contract)
	if !shouldCreate {
		return nil
	}
	workflowPlan.ID = current.ID
	workflowPlan.Revision = current.Revision
	workflowPlan.CreatedAt = current.CreatedAt
	_, err = c.UpdateTaskPlan(ctx, invocation.ID, workflowPlan, "default coding workflow")
	return err
}

// UpdateTaskPlan applies a compare-and-swap update and records the new plan
// as an AgentEvent. The caller must send the revision it last observed; this
// prevents a stale WebUI tab from silently discarding a newer update.
func (c *Coordinator) UpdateTaskPlan(ctx context.Context, invocationID string, proposed TaskPlan, reason string) (TaskPlan, error) {
	invocationID = strings.TrimSpace(invocationID)
	if invocationID == "" {
		return TaskPlan{}, fmt.Errorf("%w: invocation_id 不能为空", ErrInvalidPlan)
	}
	invocation, err := c.repo.GetInvocation(ctx, invocationID)
	if err != nil {
		return TaskPlan{}, err
	}
	if invocation.Status.Terminal() && !isPostTerminalAudit(ctx) {
		return TaskPlan{}, ErrConflict
	}
	planRepo, ok := c.repo.(TaskPlanRepository)
	if !ok {
		return TaskPlan{}, errors.New("当前 Runtime 存储未启用 TaskPlan")
	}
	c.planMu.Lock()
	defer c.planMu.Unlock()
	current, err := c.GetTaskPlan(ctx, invocationID)
	if err != nil {
		return TaskPlan{}, err
	}
	if proposed.Revision != current.Revision {
		return TaskPlan{}, fmt.Errorf("%w: plan revision=%d，当前 revision=%d", ErrConflict, proposed.Revision, current.Revision)
	}
	proposed.ID = current.ID
	proposed.InvocationID = invocationID
	proposed.Revision = current.Revision + 1
	if proposed.CreatedAt.IsZero() {
		proposed.CreatedAt = current.CreatedAt
	}
	proposed, err = proposed.Normalize(time.Now().UTC())
	if err != nil {
		return TaskPlan{}, err
	}
	saved, err := planRepo.UpdateTaskPlan(ctx, proposed, current.Revision)
	if err != nil {
		return TaskPlan{}, err
	}
	data := map[string]any{"plan": saved}
	if reason = strings.TrimSpace(reason); reason != "" {
		data["reason"] = reason
	}
	if _, err := c.appendAndPublish(ctx, AgentEvent{ID: newID("event"), InvocationID: invocationID, Type: EventPlanUpdated, Timestamp: saved.UpdatedAt, Data: data}); err != nil {
		return TaskPlan{}, err
	}
	return saved, nil
}

// GetTaskContract returns the latest durable contract for an Invocation.
// Historical invocations created before the contract migration receive a
// deterministic version-one projection of their original message.
func (c *Coordinator) GetTaskContract(ctx context.Context, invocationID string) (TaskContract, error) {
	invocationID = strings.TrimSpace(invocationID)
	invocation, err := c.repo.GetInvocation(ctx, invocationID)
	if err != nil {
		return TaskContract{}, err
	}
	contractRepo, ok := c.repo.(TaskContractRepository)
	if !ok {
		return TaskContract{}, errors.New("当前 Runtime 存储未启用 TaskContract")
	}
	contract, err := contractRepo.GetTaskContract(ctx, invocationID)
	if err == nil {
		return contract, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return TaskContract{}, err
	}
	contract = InitialTaskContract(invocation.ID, invocation.Message, invocation.CreatedAt)
	if createErr := contractRepo.CreateTaskContract(ctx, contract); createErr != nil {
		if errors.Is(createErr, ErrConflict) {
			return contractRepo.GetTaskContract(ctx, invocationID)
		}
		return TaskContract{}, createErr
	}
	return contract, nil
}

// ListTaskContracts returns all immutable contract versions in ascending
// version order. It is used by audit and recovery tooling, not by prompt
// construction (which only needs the latest version).
func (c *Coordinator) ListTaskContracts(ctx context.Context, invocationID string) ([]TaskContract, error) {
	invocationID = strings.TrimSpace(invocationID)
	if _, err := c.repo.GetInvocation(ctx, invocationID); err != nil {
		return nil, err
	}
	contractRepo, ok := c.repo.(TaskContractRepository)
	if !ok {
		return nil, errors.New("当前 Runtime 存储未启用 TaskContract")
	}
	items, err := contractRepo.ListTaskContracts(ctx, invocationID)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		contract, createErr := c.GetTaskContract(ctx, invocationID)
		if createErr != nil {
			return nil, createErr
		}
		return []TaskContract{contract}, nil
	}
	return items, nil
}

// UpdateTaskContract appends a new contract version using compare-and-swap.
// A stale editor cannot silently overwrite a user's newer clarification.
func (c *Coordinator) UpdateTaskContract(ctx context.Context, invocationID string, proposed TaskContract, reason string) (TaskContract, error) {
	invocationID = strings.TrimSpace(invocationID)
	if invocationID == "" {
		return TaskContract{}, fmt.Errorf("%w: invocation_id 不能为空", ErrInvalidTaskContract)
	}
	invocation, err := c.repo.GetInvocation(ctx, invocationID)
	if err != nil {
		return TaskContract{}, err
	}
	contractRepo, ok := c.repo.(TaskContractRepository)
	if !ok {
		return TaskContract{}, errors.New("当前 Runtime 存储未启用 TaskContract")
	}
	c.contractMu.Lock()
	defer c.contractMu.Unlock()
	current, err := c.GetTaskContract(ctx, invocationID)
	if err != nil {
		return TaskContract{}, err
	}
	if proposed.Version != current.Version {
		return TaskContract{}, fmt.Errorf("%w: contract version=%d，当前 version=%d", ErrConflict, proposed.Version, current.Version)
	}
	proposed.InvocationID = invocationID
	proposed.Version = current.Version + 1
	if len(proposed.SourceMessageIDs) == 0 {
		proposed.SourceMessageIDs = append([]string(nil), current.SourceMessageIDs...)
	}
	if proposed.UpdatedAt.IsZero() {
		proposed.UpdatedAt = time.Now().UTC()
	}
	if err := proposed.Validate(); err != nil {
		return TaskContract{}, err
	}
	saved, err := contractRepo.UpdateTaskContract(ctx, proposed, current.Version)
	if err != nil {
		return TaskContract{}, err
	}
	data := map[string]any{"contract": saved}
	if reason = strings.TrimSpace(reason); reason != "" {
		data["reason"] = reason
	}
	if _, err := c.appendAndPublish(ctx, AgentEvent{ID: newID("event"), InvocationID: invocationID, Type: EventTaskContractUpdated, Timestamp: saved.UpdatedAt, Data: data}); err != nil {
		return TaskContract{}, err
	}
	if err := c.ensureDefaultWorkflowPlan(ctx, invocation, saved); err != nil {
		return TaskContract{}, fmt.Errorf("更新任务契约后创建默认工作流计划失败: %w", err)
	}
	return saved, nil
}

// GetInstructionSnapshotSet returns the latest project instruction view for
// an Invocation. Historical invocations without a snapshot return a stable
// empty set after lazy initialization by the Kernel observer.
func (c *Coordinator) GetInstructionSnapshotSet(ctx context.Context, invocationID string) (InstructionSnapshotSet, error) {
	if _, err := c.repo.GetInvocation(ctx, strings.TrimSpace(invocationID)); err != nil {
		return InstructionSnapshotSet{}, err
	}
	repo, ok := c.repo.(InstructionSnapshotRepository)
	if !ok {
		return InstructionSnapshotSet{}, errors.New("当前 Runtime 存储未启用 InstructionSnapshot")
	}
	return repo.GetInstructionSnapshotSet(ctx, strings.TrimSpace(invocationID))
}

// RecordInstructionConflict records a digest-only project instruction change
// detected while an Invocation is waiting for approval or about to resume.
// The event intentionally excludes instruction bodies: callers can inspect
// the bounded snapshot separately, while the event remains safe to expose in
// the normal Runtime timeline.
func (c *Coordinator) RecordInstructionConflict(ctx context.Context, invocationID string, previous, current []InstructionSnapshot, reason string) error {
	invocationID = strings.TrimSpace(invocationID)
	if invocationID == "" {
		return fmt.Errorf("%w: invocation_id 不能为空", ErrConflict)
	}
	if _, err := c.repo.GetInvocation(ctx, invocationID); err != nil {
		return err
	}
	project := func(items []InstructionSnapshot) []map[string]any {
		result := make([]map[string]any, 0, len(items))
		for _, item := range items {
			result = append(result, map[string]any{
				"path": item.Path, "scope_path": item.ScopePath, "source": item.Source,
				"priority": item.Priority, "content_digest": item.ContentDigest,
			})
		}
		return result
	}
	data := map[string]any{
		"previous_revision": 0,
		"previous":          project(previous),
		"current":           project(current),
		"reason":            strings.TrimSpace(reason),
	}
	if stored, err := c.GetInstructionSnapshotSet(ctx, invocationID); err == nil {
		data["previous_revision"] = stored.Revision
	}
	_, err := c.appendAndPublish(ctx, AgentEvent{
		ID: newID("event"), InvocationID: invocationID, Type: EventInstructionConflict,
		Timestamp: time.Now().UTC(), Data: data,
	})
	return err
}

// ReconfirmInstructions replaces the instruction snapshot only after the
// caller explicitly acknowledges the conflict. The approval remains pending;
// the caller must submit the normal approval decision again afterwards.
func (c *Coordinator) ReconfirmInstructions(ctx context.Context, invocationID string) (InstructionSnapshotSet, error) {
	c.approvalMu.Lock()
	defer c.approvalMu.Unlock()
	invocationID = strings.TrimSpace(invocationID)
	invocation, err := c.repo.GetInvocation(ctx, invocationID)
	if err != nil {
		return InstructionSnapshotSet{}, err
	}
	if invocation.Status != InvocationWaitingApproval {
		return InstructionSnapshotSet{}, fmt.Errorf("%w: 仅等待审批的 invocation 可以重新确认项目指令", ErrConflict)
	}
	c.mu.Lock()
	reconfirmer := c.instructionReconfirmer
	c.mu.Unlock()
	if reconfirmer == nil {
		return InstructionSnapshotSet{}, errors.New("当前 Runtime 未启用项目指令重新确认")
	}
	snapshot, err := reconfirmer(ctx, invocationID)
	if err != nil {
		return InstructionSnapshotSet{}, err
	}
	if snapshot.InvocationID != invocationID {
		return InstructionSnapshotSet{}, fmt.Errorf("%w: 重新确认快照的 invocation_id 不匹配", ErrConflict)
	}
	if _, err := c.appendAndPublish(ctx, AgentEvent{ID: newID("event"), InvocationID: invocationID, Type: EventInstructionReconfirmed, Timestamp: time.Now().UTC(), Data: map[string]any{"revision": snapshot.Revision, "snapshot_count": len(snapshot.Snapshots)}}); err != nil {
		return InstructionSnapshotSet{}, err
	}
	return snapshot, nil
}

// validateResumeSnapshots checks immutable provider/tool facts before an
// approval decision is committed. A changed catalog is an explicit migration
// boundary: the approval remains pending and no resumed model/tool turn is
// launched. Only identifiers and digests are emitted, never provider secrets
// or tool schemas.
func (c *Coordinator) validateResumeSnapshots(ctx context.Context, invocation Invocation) error {
	invocationID := strings.TrimSpace(invocation.ID)
	if invocationID == "" || c.kernel == nil {
		return nil
	}
	if storedConfig := strings.TrimSpace(invocation.ConfigSnapshot); storedConfig != "" {
		var workspaceID *string
		if strings.TrimSpace(invocation.WorkspaceID) != "" {
			value := invocation.WorkspaceID
			workspaceID = &value
		}
		_, currentConfig, currentErr := c.kernel.ResolveRuntimeConfigSnapshot(ctx, agent.ChatRequest{
			UserID: invocation.UserID, BotID: invocation.BotID, InvocationID: invocation.ID, ConversationID: invocation.ConversationID,
			SessionID: invocation.SessionID, ProviderID: invocation.ProviderID, ModelID: invocation.ModelID,
			WorkspaceID: workspaceID, TargetPath: invocation.TargetPath,
		})
		if currentErr != nil {
			return fmt.Errorf("%w: 当前 Runtime 配置无法解析", ErrConflict)
		}
		currentSnapshot, parseErr := agent.ParseRuntimeConfigSnapshot(currentConfig)
		if parseErr != nil {
			return fmt.Errorf("%w: 当前 Runtime 配置快照无效", ErrConflict)
		}
		if validateErr := agent.ValidateRuntimeConfigSnapshot(storedConfig, currentSnapshot); validateErr != nil {
			_, _ = c.appendAndPublish(context.WithoutCancel(ctx), AgentEvent{
				ID: newID("event"), InvocationID: invocationID, Type: EventRuntimeConfigChanged, Timestamp: time.Now().UTC(),
				Data: map[string]any{"stored_digest": agent.RuntimeConfigSnapshotDigest(storedConfig), "current_digest": agent.RuntimeConfigSnapshotDigest(currentConfig), "reason": "runtime_config_changed"},
			})
			return fmt.Errorf("%w: Runtime 配置快照已变化，请迁移或重新发起任务", ErrConflict)
		}
	}
	if capabilityRepo, ok := c.repo.(ModelCapabilitySnapshotRepository); ok {
		if snapshot, err := capabilityRepo.GetModelCapabilitySnapshot(ctx, invocationID); err == nil {
			current, currentErr := c.kernel.CurrentModelCapabilityProfile(ctx, invocation.ProviderID, invocation.ModelID)
			if currentErr != nil {
				return fmt.Errorf("%w: 当前模型不可用: %v", ErrConflict, currentErr)
			}
			currentID := current.SnapshotID()
			if currentID != snapshot.Result.ProfileSnapshotID {
				_, _ = c.appendAndPublish(context.WithoutCancel(ctx), AgentEvent{
					ID: newID("event"), InvocationID: invocationID, Type: EventModelMigrationRequired, Timestamp: time.Now().UTC(),
					Data: map[string]any{"provider_id": invocation.ProviderID, "model_id": invocation.ModelID, "previous_profile_snapshot_id": snapshot.Result.ProfileSnapshotID, "current_profile_snapshot_id": currentID, "reason": "capability_profile_changed"},
				})
				return fmt.Errorf("%w: 模型能力 Snapshot 已变化，请迁移或重新发起任务", ErrConflict)
			}
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	if toolRepo, ok := c.repo.(ToolSetSnapshotRepository); ok {
		if snapshot, err := toolRepo.GetToolSetSnapshot(ctx, invocationID); err == nil {
			currentRevision := c.kernel.CurrentToolCatalogRevision()
			if currentRevision != "" && currentRevision != snapshot.CatalogRevision {
				_, _ = c.appendAndPublish(context.WithoutCancel(ctx), AgentEvent{
					ID: newID("event"), InvocationID: invocationID, Type: EventToolSetInvalidated, Timestamp: time.Now().UTC(),
					Data: map[string]any{"previous_catalog_revision": snapshot.CatalogRevision, "current_catalog_revision": currentRevision, "snapshot_id": snapshot.ID, "reason": "tool_catalog_changed"},
				})
				return fmt.Errorf("%w: ToolSet Snapshot 已失效，请重新选择工具", ErrConflict)
			}
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	return nil
}

func (c *Coordinator) ListInvocations(ctx context.Context, userID string, statuses []InvocationStatus) ([]Invocation, error) {
	return c.repo.ListInvocations(ctx, strings.TrimSpace(userID), statuses)
}

func (c *Coordinator) ListEvents(ctx context.Context, invocationID string, after int64, limit int) ([]AgentEvent, error) {
	if _, err := c.repo.GetInvocation(ctx, invocationID); err != nil {
		return nil, err
	}
	return c.repo.ListEvents(ctx, strings.TrimSpace(invocationID), after, limit)
}

// listAllInvocationEvents reads a bounded, sequence-ordered event stream for
// internal projections. Repository ListEvents intentionally caps one query at
// 5,000 rows; using that method only once makes a long workflow appear to have
// no current wait boundary once the first page is full. This helper keeps
// asking for the next page until the stream is exhausted, while rejecting
// stalled/out-of-order custom repositories and refusing to return a partial
// executable projection at the hard cap.
func (c *Coordinator) listAllInvocationEvents(ctx context.Context, invocationID string, maxEvents int) ([]AgentEvent, error) {
	if c == nil || c.repo == nil {
		return nil, errors.New("Runtime Coordinator 不能为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	invocationID = strings.TrimSpace(invocationID)
	if invocationID == "" {
		return nil, ErrNotFound
	}
	if maxEvents <= 0 || maxEvents > maxRuntimeEventReplay {
		maxEvents = maxRuntimeEventReplay
	}
	result := make([]AgentEvent, 0, minInt(maxEvents, runtimeEventPageSize))
	var after int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(result) >= maxEvents {
			// A full final page is not enough to know whether the stream ended.
			// Probe one row so an exact-boundary stream succeeds while a larger
			// stream fails closed instead of returning an incomplete checkpoint.
			probe, err := c.repo.ListEvents(ctx, invocationID, after, 1)
			if err != nil {
				return nil, err
			}
			if len(probe) > 0 {
				return nil, fmt.Errorf("%w: invocation=%s max=%d", ErrEventReplayLimit, invocationID, maxEvents)
			}
			return result, nil
		}
		remaining := maxEvents - len(result)
		limit := runtimeEventPageSize
		if remaining < limit {
			limit = remaining
		}
		page, err := c.repo.ListEvents(ctx, invocationID, after, limit)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return result, nil
		}
		for _, event := range page {
			if event.Sequence <= after {
				return nil, fmt.Errorf("%w: invocation=%s after=%d sequence=%d", ErrEventReplayInvalid, invocationID, after, event.Sequence)
			}
			result = append(result, event)
			after = event.Sequence
			if len(result) > maxEvents {
				return nil, fmt.Errorf("%w: invocation=%s max=%d", ErrEventReplayLimit, invocationID, maxEvents)
			}
		}
	}
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func (c *Coordinator) GetApproval(ctx context.Context, id string) (Approval, error) {
	return c.repo.GetApproval(ctx, strings.TrimSpace(id))
}

func (c *Coordinator) ListApprovals(ctx context.Context, invocationID string, status ApprovalStatus) ([]Approval, error) {
	return c.repo.ListApprovals(ctx, strings.TrimSpace(invocationID), status)
}

// ListToolCalls returns the durable model/tool audit records for an Invocation.
// It is intentionally exposed separately from AgentEvent replay so clients can
// render the current tool lifecycle without reconstructing state from a stream.
func (c *Coordinator) ListToolCalls(ctx context.Context, invocationID string) ([]ToolCall, error) {
	if _, err := c.repo.GetInvocation(ctx, strings.TrimSpace(invocationID)); err != nil {
		return nil, err
	}
	repo, ok := c.repo.(ToolCallRepository)
	if !ok {
		return nil, errors.New("当前 Runtime 存储未启用 ToolCall 审计")
	}
	return repo.ListToolCalls(ctx, strings.TrimSpace(invocationID))
}

func (c *Coordinator) ListContextManifests(ctx context.Context, invocationID string) ([]ContextManifest, error) {
	invocationID = strings.TrimSpace(invocationID)
	if _, err := c.repo.GetInvocation(ctx, invocationID); err != nil {
		return nil, err
	}
	manifestRepo, ok := c.repo.(ContextManifestRepository)
	if !ok {
		return nil, errors.New("当前 Runtime 存储未启用 ContextManifest")
	}
	return manifestRepo.ListContextManifests(ctx, invocationID)
}

// GetToolSetSnapshot returns the fixed descriptor versions for an Invocation.
func (c *Coordinator) GetToolSetSnapshot(ctx context.Context, invocationID string) (ToolSetSnapshot, error) {
	invocationID = strings.TrimSpace(invocationID)
	if _, err := c.repo.GetInvocation(ctx, invocationID); err != nil {
		return ToolSetSnapshot{}, err
	}
	repo, ok := c.repo.(ToolSetSnapshotRepository)
	if !ok {
		return ToolSetSnapshot{}, errors.New("当前 Runtime 存储未启用 ToolSet Snapshot")
	}
	return repo.GetToolSetSnapshot(ctx, invocationID)
}

func (c *Coordinator) GetModelCapabilitySnapshot(ctx context.Context, invocationID string) (ModelCapabilitySnapshot, error) {
	invocationID = strings.TrimSpace(invocationID)
	if _, err := c.repo.GetInvocation(ctx, invocationID); err != nil {
		return ModelCapabilitySnapshot{}, err
	}
	repo, ok := c.repo.(ModelCapabilitySnapshotRepository)
	if !ok {
		return ModelCapabilitySnapshot{}, errors.New("当前 Runtime 存储未启用模型能力 Snapshot")
	}
	return repo.GetModelCapabilitySnapshot(ctx, invocationID)
}

// GetWorkingSet returns the current versioned candidate set. Historical
// invocations receive an empty revision-one set lazily.
func (c *Coordinator) GetWorkingSet(ctx context.Context, invocationID string) (WorkingSet, error) {
	invocationID = strings.TrimSpace(invocationID)
	invocation, err := c.repo.GetInvocation(ctx, invocationID)
	if err != nil {
		return WorkingSet{}, err
	}
	workingSetRepo, ok := c.repo.(WorkingSetRepository)
	if !ok {
		return WorkingSet{}, errors.New("当前 Runtime 存储未启用 Working Set")
	}
	workingSet, err := workingSetRepo.GetWorkingSet(ctx, invocationID)
	if err == nil {
		return workingSet, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return WorkingSet{}, err
	}
	workingSet = WorkingSet{InvocationID: invocation.ID, Revision: 1, Items: []WorkingSetItem{}, UpdatedAt: invocation.CreatedAt}
	if _, createErr := workingSetRepo.ReplaceWorkingSet(ctx, workingSet); createErr != nil {
		if errors.Is(createErr, ErrConflict) {
			return workingSetRepo.GetWorkingSet(ctx, invocationID)
		}
		return WorkingSet{}, createErr
	}
	return workingSet, nil
}

// UpdateWorkingSet replaces the metadata set with a compare-and-swap
// revision and emits an auditable event. Content is never accepted here.
func (c *Coordinator) UpdateWorkingSet(ctx context.Context, invocationID string, proposed WorkingSet, reason string) (WorkingSet, error) {
	invocationID = strings.TrimSpace(invocationID)
	if invocationID == "" {
		return WorkingSet{}, fmt.Errorf("%w: invocation_id 不能为空", ErrInvalidWorkingSet)
	}
	if _, err := c.repo.GetInvocation(ctx, invocationID); err != nil {
		return WorkingSet{}, err
	}
	workingSetRepo, ok := c.repo.(WorkingSetRepository)
	if !ok {
		return WorkingSet{}, errors.New("当前 Runtime 存储未启用 Working Set")
	}
	c.workingSetMu.Lock()
	defer c.workingSetMu.Unlock()
	current, err := c.GetWorkingSet(ctx, invocationID)
	if err != nil {
		return WorkingSet{}, err
	}
	if proposed.Revision != current.Revision {
		return WorkingSet{}, fmt.Errorf("%w: working set revision=%d，当前 revision=%d", ErrConflict, proposed.Revision, current.Revision)
	}
	proposed.InvocationID = invocationID
	proposed.Revision = current.Revision + 1
	proposed.UpdatedAt = time.Now().UTC()
	saved, err := workingSetRepo.ReplaceWorkingSet(ctx, proposed)
	if err != nil {
		return WorkingSet{}, err
	}
	data := map[string]any{"working_set": saved}
	if reason = strings.TrimSpace(reason); reason != "" {
		data["reason"] = reason
	}
	if _, err := c.appendAndPublish(ctx, AgentEvent{ID: newID("event"), InvocationID: invocationID, Type: EventWorkingSetUpdated, Timestamp: saved.UpdatedAt, Data: data}); err != nil {
		return WorkingSet{}, err
	}
	c.refreshRuntimeSnapshot(ctx, invocationID)
	return saved, nil
}

func (c *Coordinator) GetVerificationRun(ctx context.Context, id string) (VerificationRun, error) {
	repo, ok := c.repo.(VerificationRepository)
	if !ok {
		return VerificationRun{}, errors.New("当前 Runtime 存储未启用 VerificationRun")
	}
	return repo.GetVerificationRun(ctx, strings.TrimSpace(id))
}

func (c *Coordinator) ListVerificationRuns(ctx context.Context, invocationID string) ([]VerificationRun, error) {
	invocationID = strings.TrimSpace(invocationID)
	if _, err := c.repo.GetInvocation(ctx, invocationID); err != nil {
		return nil, err
	}
	repo, ok := c.repo.(VerificationRepository)
	if !ok {
		return nil, errors.New("当前 Runtime 存储未启用 VerificationRun")
	}
	return repo.ListVerificationRuns(ctx, invocationID)
}

// ValidateWorkspaceOperationAdmission enforces the default coding workflow at
// the Workspace boundary. Runtime-bound mutations may only be prepared while
// the durable plan is in its editing step; commands may only be prepared while
// it is verifying. The check is intentionally read-only and is repeated by
// Workspace immediately before approval, closing the gap between model tool
// dispatch, a user's approval click and the actual side effect.
//
// Invocations without a TaskPlan repository, or with a legacy empty plan, are
// left compatible with older embedders. The application wires this validator
// only for its Runtime-backed Workspace service.
func (c *Coordinator) ValidateWorkspaceOperationAdmission(ctx context.Context, request workspace.OperationRequest) error {
	if c == nil || c.repo == nil || strings.TrimSpace(request.InvocationID) == "" {
		return nil
	}
	request.InvocationID = strings.TrimSpace(request.InvocationID)
	request.WorkspaceID = strings.TrimSpace(request.WorkspaceID)
	invocation, err := c.repo.GetInvocation(ctx, request.InvocationID)
	if err != nil {
		return err
	}
	if invocation.Status.Terminal() {
		return fmt.Errorf("%w: invocation 已终态，不能准备新的工作区操作", ErrConflict)
	}
	if request.WorkspaceID != "" && strings.TrimSpace(invocation.WorkspaceID) != "" && request.WorkspaceID != strings.TrimSpace(invocation.WorkspaceID) {
		return fmt.Errorf("%w: operation workspace_id 与 invocation 不匹配", ErrConflict)
	}
	if invocation.Status != InvocationQueued && invocation.Status != InvocationRunning && invocation.Status != InvocationWaitingApproval {
		return fmt.Errorf("%w: invocation 当前状态 %s 不允许准备工作区操作", ErrConflict, invocation.Status)
	}
	planRepo, ok := c.repo.(TaskPlanRepository)
	if !ok {
		return nil
	}
	plan, err := planRepo.GetTaskPlan(ctx, request.InvocationID)
	if errors.Is(err, ErrNotFound) {
		contract := InitialTaskContract(invocation.ID, invocation.Message, invocation.CreatedAt)
		if contractRepo, contractOK := c.repo.(TaskContractRepository); contractOK {
			if stored, contractErr := contractRepo.GetTaskContract(ctx, request.InvocationID); contractErr == nil {
				contract = stored
			} else if !errors.Is(contractErr, ErrNotFound) {
				return contractErr
			}
		}
		if ensureErr := c.ensureDefaultWorkflowPlan(ctx, invocation, contract); ensureErr != nil {
			return ensureErr
		}
		plan, err = planRepo.GetTaskPlan(ctx, request.InvocationID)
	}
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	if len(plan.Steps) == 0 {
		return nil
	}
	if plan.Status == PlanBlocked {
		return fmt.Errorf("%w: 任务计划已阻塞: %s", ErrConflict, firstNonEmpty(plan.Blocker, "没有可继续的步骤"))
	}
	if plan.Status == PlanCompleted {
		return fmt.Errorf("%w: 任务计划已完成，不能准备新的工作区操作", ErrConflict)
	}
	if plan.CurrentStepID == "" && plan.Status == PlanPending {
		if startErr := c.ensurePlanStepStarted(ctx, request.InvocationID, "workspace operation admission"); startErr != nil {
			return startErr
		}
		plan, err = planRepo.GetTaskPlan(ctx, request.InvocationID)
		if err != nil {
			return err
		}
	}
	step := plan.CurrentStep()
	if step == nil {
		return fmt.Errorf("%w: 任务计划没有活动步骤", ErrConflict)
	}
	requiredPhase := WorkflowPhaseEditing
	if request.Type == workspace.OperationCommand {
		requiredPhase = WorkflowPhaseVerifying
	} else {
		switch request.Type {
		case workspace.OperationWriteFile, workspace.OperationMakeDirectory, workspace.OperationPatchFile, workspace.OperationPatchSet, workspace.OperationDeletePath:
		default:
			return nil
		}
	}
	actualPhase := workflowPhaseForStep(*step)
	if actualPhase != requiredPhase {
		return fmt.Errorf("%w: 当前步骤 %s (%s) 不允许 %s 操作，需要 workflow phase=%s，当前=%s", ErrConflict, step.ID, step.Title, request.Type, requiredPhase, actualPhase)
	}
	return nil
}

// RecordWorkspaceOperation projects an approved command operation into a
// VerificationRun when the active plan step is clearly a verification step,
// and projects successful file operations as evidence for an editing step.
// Workspace remains Runtime-agnostic; the application installs this callback
// on workspace.Service. The deterministic operation-derived ID makes command
// retries and duplicate post-commit notifications idempotent.
func (c *Coordinator) RecordWorkspaceOperation(ctx context.Context, operation workspace.Operation) {
	if c == nil || strings.TrimSpace(operation.InvocationID) == "" || strings.TrimSpace(operation.ID) == "" {
		return
	}
	if operation.Type != workspace.OperationCommand {
		c.recordCompletedWorkspaceOperation(ctx, operation)
		return
	}
	verificationStatus := VerificationStatus("")
	switch operation.Status {
	case workspace.OperationCompleted:
		if operation.CommandOutcome == string(workspace.CommandOutcomeNonzero) || operation.CommandOutcome == string(workspace.CommandOutcomeTimedOut) || operation.CommandOutcome == string(workspace.CommandOutcomeRuntimeError) || operation.CommandOutcome == string(workspace.CommandOutcomeSignaled) {
			verificationStatus = VerificationFailed
		} else if operation.CommandOutcome == string(workspace.CommandOutcomeUnknown) || operation.Unknown {
			verificationStatus = VerificationUnknown
		} else {
			verificationStatus = VerificationPassed
		}
	case workspace.OperationFailed:
		verificationStatus = VerificationFailed
	case workspace.OperationUnknown:
		verificationStatus = VerificationUnknown
	case workspace.OperationStale:
		verificationStatus = VerificationBlocked
	case workspace.OperationCancelled, workspace.OperationExpired:
		verificationStatus = VerificationCancelled
	default:
		return
	}
	plan, err := c.GetTaskPlan(context.WithoutCancel(ctx), operation.InvocationID)
	if err != nil {
		return
	}
	step := plan.CurrentStep()
	if step == nil || workflowPhaseForStep(*step) != WorkflowPhaseVerifying {
		return
	}
	summary := sanitizeRuntimeString(strings.TrimSpace(operation.Result))
	if summary == "" {
		summary = sanitizeRuntimeString(strings.TrimSpace(operation.Error))
	}
	if len(summary) > 4096 {
		summary = summary[:4096]
	}
	command := sanitizeRuntimeString(strings.TrimSpace(operation.Command))
	if len(command) > 4096 {
		command = command[:4096]
	}
	var exitCode *int
	if operation.ExitCode != nil {
		value := *operation.ExitCode
		exitCode = &value
	}
	outputRef := commandRunOutputRef(operation.CommandRunID)
	if operation.OutputArtifact != nil && strings.TrimSpace(operation.OutputArtifact.ID) != "" {
		outputRef = "artifact:" + operation.OutputArtifact.ID
	}
	item := VerificationRun{
		ID: operationVerificationID(operation.ID), InvocationID: operation.InvocationID, PlanStepID: step.ID,
		Kind: "command", Command: command, Status: verificationStatus, ExitCode: exitCode,
		Summary: summary, OutputRef: outputRef, OutputDigest: strings.TrimSpace(operation.OutputDigest),
	}
	if _, err := c.CreateVerificationRun(context.WithoutCancel(ctx), item); err == nil || errors.Is(err, ErrConflict) {
		return
	} else {
		// The command side effect is already committed. A projection failure
		// must remain visible without changing that side-effect result.
		_, _ = c.appendAndPublish(context.WithoutCancel(ctx), AgentEvent{
			ID: newID("event"), InvocationID: operation.InvocationID, Type: EventRuntimeNotice, Timestamp: time.Now().UTC(),
			Data: map[string]any{"code": "verification_projection_failed", "operation_id": operation.ID, "reason": sanitizeRuntimeString(err.Error())},
		})
	}
}

// RecordCommandRun projects the independent command checkpoint into the
// Invocation event stream. It is metadata-only: the observer cannot start,
// retry, or otherwise mutate the process represented by the checkpoint.
func (c *Coordinator) RecordCommandRun(ctx context.Context, run workspace.CommandRun) {
	if c == nil || strings.TrimSpace(run.InvocationID) == "" || strings.TrimSpace(run.ID) == "" {
		return
	}
	eventType := EventCommandQueued
	switch run.Status {
	case workspace.CommandRunStarting, workspace.CommandRunRunning:
		eventType = EventCommandStarted
	case workspace.CommandRunExited:
		eventType = EventCommandCompleted
	case workspace.CommandRunUnknown:
		eventType = EventCommandUnknown
	case workspace.CommandRunStartFailed, workspace.CommandRunTerminated, workspace.CommandRunCancelled:
		eventType = EventCommandFailed
	}
	data := map[string]any{
		"command_run_id": run.ID, "operation_id": run.OperationID, "executor": run.Executor, "mode": run.Mode,
		"cwd": sanitizeRuntimeString(run.CWD), "timeout_ms": run.TimeoutMS, "status": string(run.Status),
		"outcome": string(run.Outcome), "stdout_bytes": run.StdoutBytes, "stderr_bytes": run.StderrBytes,
		"stored_bytes": run.StoredBytes, "output_digest": run.OutputDigest, "output_truncated": run.OutputTruncated,
		"revision": run.Revision,
	}
	if run.OutputArtifact != nil {
		data["output_artifact"] = *run.OutputArtifact
	}
	if strings.TrimSpace(run.ArtifactError) != "" {
		data["artifact_error"] = sanitizeRuntimeString(run.ArtifactError)
	}
	if run.Capabilities != nil {
		// The snapshot is metadata-only and was captured before approval. Keep
		// it on lifecycle events so SSE/replay consumers can render the same
		// safety disclosure without fetching a second resource.
		data["capabilities"] = *run.Capabilities
	}
	if run.ExitCode != nil {
		data["exit_code"] = *run.ExitCode
	}
	if strings.TrimSpace(run.Error) != "" {
		data["error"] = sanitizeRuntimeString(run.Error)
	}
	_, _ = c.appendAndPublish(context.WithoutCancel(ctx), AgentEvent{
		ID: newID("event"), InvocationID: run.InvocationID, Type: eventType, Timestamp: run.UpdatedAt, Data: data,
	})
}

func commandRunOutputRef(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	return "command-run:" + id
}

// recordCompletedWorkspaceOperation completes an editing plan step for a
// successful file-side operation. Commands are deliberately handled by the
// VerificationRun path above: a non-zero/unknown command result is evidence
// for verification, not proof that an edit step completed.
func (c *Coordinator) recordCompletedWorkspaceOperation(ctx context.Context, operation workspace.Operation) {
	switch operation.Type {
	case workspace.OperationWriteFile, workspace.OperationMakeDirectory, workspace.OperationPatchFile, workspace.OperationPatchSet, workspace.OperationDeletePath:
		// supported editing operations
	default:
		return
	}
	if operation.Status != workspace.OperationCompleted {
		return
	}
	baseCtx := context.WithoutCancel(ctx)
	invocation, err := c.repo.GetInvocation(baseCtx, strings.TrimSpace(operation.InvocationID))
	if err != nil {
		return
	}
	auditCtx := baseCtx
	if invocation.Status.Terminal() {
		auditCtx = withPostTerminalAudit(baseCtx)
	}
	plan, err := c.GetTaskPlan(auditCtx, operation.InvocationID)
	if err != nil {
		return
	}
	step := plan.CurrentStep()
	if step == nil || workflowPhaseForStep(*step) != WorkflowPhaseEditing {
		return
	}
	summary := sanitizeRuntimeString(strings.TrimSpace(operation.Result))
	if summary == "" {
		summary = sanitizeRuntimeString(strings.TrimSpace(operation.Error))
	}
	if len(summary) > 4096 {
		summary = summary[:4096]
	}
	_ = c.completePlanStepEvidence(auditCtx, operation.InvocationID, step.ID, EvidenceRef{Kind: "operation", Ref: operation.ID, Summary: summary}, "workspace operation completed")
}

// recordWorkflowToolEvidence is the read-side counterpart of
// recordCompletedWorkspaceOperation. A successful, read-only Workspace tool
// call is sufficient evidence for an inspection step; the call ID is the
// durable reference, while response bodies stay in the owning event/session
// stores.
func (c *Coordinator) recordWorkflowToolEvidence(ctx context.Context, invocationID string, event AgentEvent) {
	if c == nil || event.Type != EventToolCompleted || event.Data == nil {
		return
	}
	name := strings.TrimSpace(eventString(event.Data, "name"))
	if !isReadOnlyWorkspaceTool(name) {
		return
	}
	callID := strings.TrimSpace(eventString(event.Data, "tool_call_id", "call_id"))
	if callID == "" {
		return
	}
	baseCtx := context.WithoutCancel(ctx)
	invocation, err := c.repo.GetInvocation(baseCtx, strings.TrimSpace(invocationID))
	if err != nil {
		return
	}
	auditCtx := baseCtx
	if invocation.Status.Terminal() {
		auditCtx = withPostTerminalAudit(baseCtx)
	}
	plan, err := c.GetTaskPlan(auditCtx, invocationID)
	if err != nil {
		return
	}
	step := plan.CurrentStep()
	if step == nil || workflowPhaseForStep(*step) != WorkflowPhaseInspecting {
		return
	}
	_ = c.completePlanStepEvidence(auditCtx, invocationID, step.ID, EvidenceRef{Kind: "tool_call", Ref: callID, Summary: name + " completed"}, "workspace inspection completed")
}

func isReadOnlyWorkspaceTool(name string) bool {
	switch strings.TrimSpace(name) {
	case "workspace_list_files", "workspace_read_file", "workspace_glob", "workspace_search_text", "workspace_read_range", "workspace_read_many", "workspace_stat", "workspace_git_status", "workspace_git_diff", "workspace_git_log":
		return true
	default:
		return false
	}
}

// completePlanStepEvidence performs a bounded CAS retry. A concurrent worker
// may have completed the same step with another durable fact; in that case
// the step is already safely advanced and this projection is a no-op.
func (c *Coordinator) completePlanStepEvidence(ctx context.Context, invocationID, stepID string, evidence EvidenceRef, reason string) error {
	for attempt := 0; attempt < 3; attempt++ {
		plan, err := c.GetTaskPlan(ctx, invocationID)
		if err != nil {
			return err
		}
		step := plan.CurrentStep()
		if step == nil || step.ID != strings.TrimSpace(stepID) || step.Status != PlanStepInProgress {
			return nil
		}
		completed, err := plan.CompleteStep(step.ID, []EvidenceRef{evidence}, time.Now().UTC())
		if err != nil {
			return err
		}
		if _, err := c.UpdateTaskPlan(ctx, invocationID, completed, reason); err != nil {
			if errors.Is(err, ErrConflict) {
				continue
			}
			return err
		}
		return c.ensurePlanStepStarted(ctx, invocationID, reason+": next step admitted")
	}
	return ErrConflict
}

func operationVerificationID(operationID string) string {
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return newID("verification-operation")
	}
	return "verification-operation-" + operationID
}

func (c *Coordinator) CreateVerificationRun(ctx context.Context, item VerificationRun) (VerificationRun, error) {
	c.verificationMu.Lock()
	defer c.verificationMu.Unlock()
	item.InvocationID = strings.TrimSpace(item.InvocationID)
	invocation, err := c.repo.GetInvocation(ctx, item.InvocationID)
	if err != nil {
		return VerificationRun{}, err
	}
	// Verification may arrive after the worker has already committed the
	// Invocation terminal state. Preserve that fact as an audit projection,
	// while keeping the normal event fence for every other late mutation.
	auditCtx := ctx
	if invocation.Status.Terminal() {
		auditCtx = withPostTerminalAudit(ctx)
	}
	repo, ok := c.repo.(VerificationRepository)
	if !ok {
		return VerificationRun{}, errors.New("当前 Runtime 存储未启用 VerificationRun")
	}
	if item.ID == "" {
		item.ID = newID("verification")
	}
	if item.Revision <= 0 {
		item.Revision = 1
	}
	if item.Status == "" {
		item.Status = VerificationPending
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	}
	if item.UpdatedAt.IsZero() {
		item.UpdatedAt = item.CreatedAt
	}
	if item.Status == VerificationRunning && item.StartedAt == nil {
		started := item.UpdatedAt
		item.StartedAt = &started
	}
	if item.Status.Terminal() && item.FinishedAt == nil {
		finished := item.UpdatedAt
		item.FinishedAt = &finished
	}
	if err := c.validateVerificationPlanLink(auditCtx, item); err != nil {
		return VerificationRun{}, err
	}
	if err := repo.CreateVerificationRun(auditCtx, item); err != nil {
		return VerificationRun{}, err
	}
	if _, err := c.appendAndPublish(auditCtx, AgentEvent{ID: newID("event"), InvocationID: item.InvocationID, Type: EventVerificationUpdated, Timestamp: item.UpdatedAt, Data: map[string]any{"verification": item, "reason": "created"}}); err != nil {
		return VerificationRun{}, err
	}
	if err := c.linkVerificationEvidence(auditCtx, item, "created"); err != nil {
		return VerificationRun{}, err
	}
	c.refreshRuntimeSnapshot(auditCtx, item.InvocationID)
	return item, nil
}

func (c *Coordinator) UpdateVerificationRun(ctx context.Context, item VerificationRun, expectedRevision int64, reason string) (VerificationRun, error) {
	c.verificationMu.Lock()
	defer c.verificationMu.Unlock()
	repo, ok := c.repo.(VerificationRepository)
	if !ok {
		return VerificationRun{}, errors.New("当前 Runtime 存储未启用 VerificationRun")
	}
	current, err := repo.GetVerificationRun(ctx, strings.TrimSpace(item.ID))
	if err != nil {
		return VerificationRun{}, err
	}
	invo, err := c.repo.GetInvocation(ctx, current.InvocationID)
	if err != nil {
		return VerificationRun{}, err
	}
	auditCtx := ctx
	if invo.Status.Terminal() {
		auditCtx = withPostTerminalAudit(ctx)
	}
	if current.Revision != expectedRevision {
		return VerificationRun{}, fmt.Errorf("%w: verification revision=%d，当前 revision=%d", ErrConflict, expectedRevision, current.Revision)
	}
	item.InvocationID = current.InvocationID
	item.Revision = expectedRevision + 1
	if strings.TrimSpace(item.PlanStepID) == "" {
		item.PlanStepID = current.PlanStepID
	}
	if strings.TrimSpace(item.Kind) == "" {
		item.Kind = current.Kind
	}
	if strings.TrimSpace(item.Command) == "" {
		item.Command = current.Command
	}
	if item.Status == "" {
		item.Status = current.Status
	}
	if item.ExitCode == nil {
		item.ExitCode = current.ExitCode
	}
	if strings.TrimSpace(item.Summary) == "" {
		item.Summary = current.Summary
	}
	if strings.TrimSpace(item.OutputRef) == "" {
		item.OutputRef = current.OutputRef
	}
	if strings.TrimSpace(item.OutputDigest) == "" {
		item.OutputDigest = current.OutputDigest
	}
	if item.StartedAt == nil {
		item.StartedAt = current.StartedAt
	}
	if item.FinishedAt == nil {
		item.FinishedAt = current.FinishedAt
	}
	if item.UpdatedAt.IsZero() {
		item.UpdatedAt = time.Now().UTC()
	}
	if item.Status == VerificationRunning && item.StartedAt == nil {
		started := item.UpdatedAt
		item.StartedAt = &started
	}
	if item.Status.Terminal() && item.FinishedAt == nil {
		finished := item.UpdatedAt
		item.FinishedAt = &finished
	}
	if err := c.validateVerificationPlanLink(auditCtx, item); err != nil {
		return VerificationRun{}, err
	}
	saved, err := repo.UpdateVerificationRun(auditCtx, item, expectedRevision)
	if err != nil {
		return VerificationRun{}, err
	}
	data := map[string]any{"verification": saved}
	if reason = strings.TrimSpace(reason); reason != "" {
		data["reason"] = reason
	}
	if _, err := c.appendAndPublish(auditCtx, AgentEvent{ID: newID("event"), InvocationID: saved.InvocationID, Type: EventVerificationUpdated, Timestamp: saved.UpdatedAt, Data: data}); err != nil {
		return VerificationRun{}, err
	}
	if err := c.linkVerificationEvidence(auditCtx, saved, reason); err != nil {
		return VerificationRun{}, err
	}
	c.refreshRuntimeSnapshot(auditCtx, saved.InvocationID)
	return saved, nil
}

// validateVerificationPlanLink checks the precondition for the automatic
// VerificationRun -> TaskPlan evidence bridge. Only a passed verification may
// complete a plan step; failed/unknown results remain durable evidence for a
// later, explicitly authorized repair decision.
func (c *Coordinator) validateVerificationPlanLink(ctx context.Context, item VerificationRun) error {
	stepID := strings.TrimSpace(item.PlanStepID)
	if item.Status != VerificationPassed || stepID == "" {
		return nil
	}
	if _, ok := c.repo.(TaskPlanRepository); !ok {
		return fmt.Errorf("%w: 当前 Runtime 未启用 TaskPlan", ErrVerificationPlanLink)
	}
	plan, err := c.GetTaskPlan(ctx, item.InvocationID)
	if err != nil {
		return fmt.Errorf("%w: 读取计划失败: %v", ErrVerificationPlanLink, err)
	}
	for _, step := range plan.Steps {
		if step.ID != stepID {
			continue
		}
		if step.Status == PlanStepCompleted {
			for _, evidence := range step.Evidence {
				if evidence.Kind == "verification" && evidence.Ref == item.ID {
					return nil
				}
			}
			return fmt.Errorf("%w: 步骤 %q 已完成但没有当前 verification 证据", ErrVerificationPlanLink, stepID)
		}
		if plan.CurrentStepID != stepID || step.Status != PlanStepInProgress {
			return fmt.Errorf("%w: 步骤 %q 必须是当前 in_progress 步骤", ErrVerificationPlanLink, stepID)
		}
		return nil
	}
	return fmt.Errorf("%w: 步骤 %q 不存在", ErrVerificationPlanLink, stepID)
}

// linkVerificationEvidence completes the active plan step after the
// VerificationRun itself is durable. The verification ID is therefore a
// stable, replayable evidence reference instead of a copied command output.
func (c *Coordinator) linkVerificationEvidence(ctx context.Context, item VerificationRun, reason string) error {
	if item.Status != VerificationPassed || strings.TrimSpace(item.PlanStepID) == "" {
		return nil
	}
	plan, err := c.GetTaskPlan(ctx, item.InvocationID)
	if err != nil {
		return fmt.Errorf("%w: 读取计划失败: %v", ErrVerificationPlanLink, err)
	}
	stepID := strings.TrimSpace(item.PlanStepID)
	for _, step := range plan.Steps {
		if step.ID != stepID {
			continue
		}
		if step.Status == PlanStepCompleted {
			// A retry-safe duplicate notification is a no-op only when the exact
			// verification ID is already present as evidence.
			for _, evidence := range step.Evidence {
				if evidence.Kind == "verification" && evidence.Ref == item.ID {
					return nil
				}
			}
			return fmt.Errorf("%w: 步骤 %q 已被其他证据完成", ErrVerificationPlanLink, stepID)
		}
		if plan.CurrentStepID != stepID || step.Status != PlanStepInProgress {
			return fmt.Errorf("%w: 步骤 %q 在保存验证结果后不再是活动步骤", ErrVerificationPlanLink, stepID)
		}
		evidence := EvidenceRef{Kind: "verification", Ref: item.ID, Summary: sanitizeRuntimeString(strings.TrimSpace(item.Summary))}
		completed, completeErr := plan.CompleteStep(stepID, []EvidenceRef{evidence}, time.Now().UTC())
		if completeErr != nil {
			return fmt.Errorf("%w: 完成步骤失败: %v", ErrVerificationPlanLink, completeErr)
		}
		linkReason := "verification passed"
		if reason = strings.TrimSpace(reason); reason != "" {
			linkReason += ": " + reason
		}
		if _, err := c.UpdateTaskPlan(ctx, item.InvocationID, completed, linkReason); err != nil {
			return fmt.Errorf("%w: 保存计划证据失败: %v", ErrVerificationPlanLink, err)
		}
		return c.ensurePlanStepStarted(ctx, item.InvocationID, "verification evidence linked: next step admitted")
	}
	return fmt.Errorf("%w: 步骤 %q 不存在", ErrVerificationPlanLink, stepID)
}

// GetRuntimeSnapshot returns the latest durable recovery projection. Callers
// that need to refresh it should use RebuildRuntimeSnapshot first.
func (c *Coordinator) GetRuntimeSnapshot(ctx context.Context, invocationID string) (RuntimeSnapshot, error) {
	invocationID = strings.TrimSpace(invocationID)
	if _, err := c.repo.GetInvocation(ctx, invocationID); err != nil {
		return RuntimeSnapshot{}, err
	}
	snapshotRepo, ok := c.repo.(RuntimeSnapshotRepository)
	if !ok {
		return RuntimeSnapshot{}, errors.New("当前 Runtime 存储未启用 RuntimeSnapshot")
	}
	return snapshotRepo.GetRuntimeSnapshot(ctx, invocationID)
}

func (c *Coordinator) refreshRuntimeSnapshot(ctx context.Context, invocationID string) {
	if _, ok := c.repo.(RuntimeSnapshotRepository); !ok {
		return
	}
	_, _ = c.RebuildRuntimeSnapshot(context.WithoutCancel(ctx), invocationID)
}

func projectRuntimeSnapshot(snapshot RuntimeSnapshot) agent.RuntimeSnapshotProjection {
	projection := agent.RuntimeSnapshotProjection{
		InvocationID: snapshot.InvocationID, Revision: snapshot.Revision, ContractVersion: snapshot.ContractVersion,
		PlanRevision: snapshot.PlanRevision, Phase: snapshot.Phase, WorkflowPhase: string(snapshot.WorkflowPhase), WorkingSetRevision: snapshot.WorkingSetRevision,
		Budget: agent.RuntimeBudgetProjection{ContextWindow: snapshot.Budget.ContextWindow, OutputReserve: snapshot.Budget.OutputReserve, SafetyReserve: snapshot.Budget.SafetyReserve, EstimatedInput: snapshot.Budget.EstimatedInput, ToolCallsUsed: snapshot.Budget.ToolCallsUsed, ToolCallsLimit: snapshot.Budget.ToolCallsLimit, BudgetExhausted: snapshot.Budget.BudgetExhausted},
	}
	projection.WorkflowStatus = string(snapshot.Workflow.Status)
	projection.WorkflowBranchID = snapshot.Workflow.CurrentBranchID
	projection.WorkflowActiveBoundaries = append(projection.WorkflowActiveBoundaries, snapshot.Workflow.ActiveBoundaryIDs...)
	if len(snapshot.Workflow.ActiveBoundaryIDs) > 0 {
		activeID := snapshot.Workflow.ActiveBoundaryIDs[0]
		for _, boundary := range snapshot.Workflow.Boundaries {
			if boundary.ID != activeID {
				continue
			}
			projection.WorkflowBoundaryID = boundary.ID
			projection.WorkflowBoundaryKind = string(boundary.Kind)
			projection.WorkflowBoundaryStatus = string(boundary.Status)
			projection.WorkflowBoundaryWaitIDs = append(projection.WorkflowBoundaryWaitIDs, boundary.PendingWaitIDs...)
			break
		}
	}
	if snapshot.ActivePlanStep != nil {
		projection.ActivePlanStepID = snapshot.ActivePlanStep.ID
		projection.ActivePlanStepTitle = snapshot.ActivePlanStep.Title
	}
	for _, item := range snapshot.PendingApprovals {
		projection.PendingApprovals = append(projection.PendingApprovals, item.ID)
	}
	for _, item := range snapshot.AppliedChangeSets {
		projection.AppliedChangeSets = append(projection.AppliedChangeSets, item.ID)
	}
	for _, item := range snapshot.VerificationRuns {
		value := item.ID
		if strings.TrimSpace(item.Status) != "" {
			value += " (" + strings.TrimSpace(item.Status) + ")"
		}
		if strings.TrimSpace(item.Summary) != "" {
			value += ": " + strings.TrimSpace(item.Summary)
		}
		projection.VerificationRuns = append(projection.VerificationRuns, value)
	}
	for _, item := range snapshot.OpenQuestions {
		projection.OpenQuestions = append(projection.OpenQuestions, item.ID+": "+item.Text)
	}
	for _, item := range snapshot.Blockers {
		projection.Blockers = append(projection.Blockers, item.Code+": "+item.Message)
	}
	for _, item := range snapshot.UnknownStates {
		projection.UnknownStates = append(projection.UnknownStates, item.Code+": "+item.Message)
	}
	return projection
}

// RebuildRuntimeSnapshot deterministically projects current durable state into
// a compact recovery block. It never accepts arbitrary model-authored state.
func (c *Coordinator) RebuildRuntimeSnapshot(ctx context.Context, invocationID string) (RuntimeSnapshot, error) {
	invocationID = strings.TrimSpace(invocationID)
	invocation, err := c.repo.GetInvocation(ctx, invocationID)
	if err != nil {
		return RuntimeSnapshot{}, err
	}
	snapshotRepo, ok := c.repo.(RuntimeSnapshotRepository)
	if !ok {
		return RuntimeSnapshot{}, errors.New("当前 Runtime 存储未启用 RuntimeSnapshot")
	}
	c.snapshotMu.Lock()
	defer c.snapshotMu.Unlock()
	current, currentErr := snapshotRepo.GetRuntimeSnapshot(ctx, invocationID)
	if currentErr != nil && !errors.Is(currentErr, ErrNotFound) {
		return RuntimeSnapshot{}, currentErr
	}
	snapshot := RuntimeSnapshot{InvocationID: invocationID, Revision: current.Revision + 1, Phase: string(invocation.Status), GeneratedAt: time.Now().UTC()}
	var taskPlan *TaskPlan
	var verifications []VerificationRun
	var durableEvents []AgentEvent
	if contractRepo, contractOK := c.repo.(TaskContractRepository); contractOK {
		if contract, contractErr := contractRepo.GetTaskContract(ctx, invocationID); contractErr == nil {
			snapshot.ContractVersion = contract.Version
		} else if !errors.Is(contractErr, ErrNotFound) {
			return RuntimeSnapshot{}, contractErr
		}
	}
	if planRepo, planOK := c.repo.(TaskPlanRepository); planOK {
		if plan, planErr := planRepo.GetTaskPlan(ctx, invocationID); planErr == nil {
			planCopy := plan
			taskPlan = &planCopy
			snapshot.PlanRevision = plan.Revision
			if step := plan.CurrentStep(); step != nil {
				snapshot.ActivePlanStep = &PlanStepRef{ID: step.ID, Title: step.Title}
			}
			if plan.Status == PlanBlocked && strings.TrimSpace(plan.Blocker) != "" {
				snapshot.Blockers = append(snapshot.Blockers, Blocker{Code: "plan_blocked", Message: plan.Blocker})
			}
		} else if !errors.Is(planErr, ErrNotFound) {
			return RuntimeSnapshot{}, planErr
		}
	}
	if workingSetRepo, workingSetOK := c.repo.(WorkingSetRepository); workingSetOK {
		if workingSet, workingSetErr := workingSetRepo.GetWorkingSet(ctx, invocationID); workingSetErr == nil {
			snapshot.WorkingSetRevision = workingSet.Revision
		} else if !errors.Is(workingSetErr, ErrNotFound) {
			return RuntimeSnapshot{}, workingSetErr
		}
	}
	if verificationRepo, verificationOK := c.repo.(VerificationRepository); verificationOK {
		if verificationItems, verificationErr := verificationRepo.ListVerificationRuns(ctx, invocationID); verificationErr == nil {
			verifications = append([]VerificationRun(nil), verificationItems...)
			for _, verification := range verificationItems {
				snapshot.VerificationRuns = append(snapshot.VerificationRuns, VerificationRef{ID: verification.ID, Status: string(verification.Status), Summary: verification.Summary})
			}
		} else {
			return RuntimeSnapshot{}, verificationErr
		}
	}
	approvals, approvalErr := c.repo.ListApprovals(ctx, invocationID, ApprovalPending)
	if approvalErr != nil {
		return RuntimeSnapshot{}, approvalErr
	}
	for _, approval := range approvals {
		snapshot.PendingApprovals = append(snapshot.PendingApprovals, ApprovalRef{ID: approval.ID, ToolName: approval.ToolName, OperationID: approval.OperationID})
	}
	if toolRepo, toolOK := c.repo.(ToolCallRepository); toolOK {
		toolCalls, toolErr := toolRepo.ListToolCalls(ctx, invocationID)
		if toolErr != nil {
			return RuntimeSnapshot{}, toolErr
		}
		snapshot.Budget.ToolCallsUsed = len(toolCalls)
		for _, toolCall := range toolCalls {
			if toolCall.Status == ToolCallFailed {
				snapshot.Blockers = append(snapshot.Blockers, Blocker{Code: "tool_failed", Message: toolCall.Error})
			}
		}
	}
	events, eventsErr := c.listAllInvocationEvents(ctx, invocationID, maxRuntimeEventReplay)
	if eventsErr != nil {
		return RuntimeSnapshot{}, eventsErr
	}
	durableEvents = events
	for _, event := range events {
		if event.Type != EventRuntimeNotice || event.Data == nil {
			continue
		}
		if code, _ := event.Data["code"].(string); code == "tool_budget_exhausted" {
			if used, ok := runtimeNoticeInt(event.Data["used"]); ok && used >= 0 {
				snapshot.Budget.ToolCallsUsed = used
			}
			if limit, ok := runtimeNoticeInt(event.Data["limit"]); ok && limit >= 0 {
				snapshot.Budget.ToolCallsLimit = limit
			}
			snapshot.Budget.BudgetExhausted = true
		}
	}
	if manifestRepo, manifestOK := c.repo.(ContextManifestRepository); manifestOK {
		if manifests, manifestErr := manifestRepo.ListContextManifests(ctx, invocationID); manifestErr == nil && len(manifests) > 0 {
			last := manifests[len(manifests)-1]
			snapshot.Budget.ContextWindow = last.ContextWindow
			snapshot.Budget.OutputReserve = last.OutputReserve
			snapshot.Budget.SafetyReserve = last.SafetyReserve
			snapshot.Budget.EstimatedInput = last.EstimatedInput
			snapshot.Budget.LastManifestID = last.ID
			if len(last.Warnings) > 0 {
				snapshot.Budget.BudgetExhausted = true
			}
		}
	}
	if invocation.Status == InvocationFailed && strings.TrimSpace(invocation.Error) != "" {
		snapshot.Blockers = append(snapshot.Blockers, Blocker{Code: "invocation_failed", Message: invocation.Error})
	}
	snapshot.Workflow = deriveWorkflowCheckpoint(invocation, taskPlan, durableEvents)
	snapshot.WorkflowPhase = deriveWorkflowPhase(invocation, taskPlan, snapshot.PendingApprovals, verifications, durableEvents)
	event := AgentEvent{ID: newID("event"), InvocationID: invocationID, Type: EventRuntimeSnapshot, Timestamp: snapshot.GeneratedAt, Data: map[string]any{"snapshot": snapshot, "revision": snapshot.Revision}}
	checkpointSource, checkpointDestination, _, checkpointEnabled := c.runtimeCheckpointDeliveryConfig()
	if checkpointEnabled {
		if _, ok := c.repo.(RuntimeCheckpointDeliveryOutboxRepository); !ok {
			return RuntimeSnapshot{}, fmt.Errorf("当前 Runtime 存储未启用 Runtime checkpoint delivery outbox")
		}
	}
	if checkpointRepo, checkpointOK := snapshotRepo.(RuntimeSnapshotCheckpointCommitRepository); checkpointEnabled && checkpointOK {
		saved, storedEvent, _, commitErr := checkpointRepo.CommitRuntimeSnapshotWithCheckpoint(ctx, snapshot, event, checkpointSource, checkpointDestination)
		if commitErr != nil {
			return RuntimeSnapshot{}, commitErr
		}
		c.publishStoredEvent(storedEvent)
		return saved, nil
	}
	if atomicRepo, atomicOK := snapshotRepo.(RuntimeSnapshotCommitRepository); atomicOK {
		saved, storedEvent, commitErr := atomicRepo.CommitRuntimeSnapshot(ctx, snapshot, event)
		if commitErr != nil {
			return RuntimeSnapshot{}, commitErr
		}
		if checkpointEnabled {
			if err := c.enqueueRuntimeCheckpointDelivery(ctx, saved, storedEvent, checkpointSource, checkpointDestination); err != nil {
				return RuntimeSnapshot{}, err
			}
		}
		c.publishStoredEvent(storedEvent)
		return saved, nil
	}
	saved, err := snapshotRepo.SaveRuntimeSnapshot(ctx, snapshot)
	if err != nil {
		return RuntimeSnapshot{}, err
	}
	event.Timestamp = saved.GeneratedAt
	event.Data = map[string]any{"snapshot": saved, "revision": saved.Revision}
	storedEvent, appendErr := c.appendAndPublish(ctx, event)
	if appendErr != nil {
		return RuntimeSnapshot{}, appendErr
	}
	if checkpointEnabled {
		if err := c.enqueueRuntimeCheckpointDelivery(ctx, saved, storedEvent, checkpointSource, checkpointDestination); err != nil {
			return RuntimeSnapshot{}, err
		}
	}
	return saved, nil
}

func (c *Coordinator) enqueueRuntimeCheckpointDelivery(ctx context.Context, snapshot RuntimeSnapshot, event AgentEvent, source, destination string) error {
	repo, ok := c.repo.(RuntimeCheckpointDeliveryOutboxRepository)
	if !ok {
		return fmt.Errorf("当前 Runtime 存储未启用 Runtime checkpoint delivery outbox")
	}
	item, err := NewRuntimeCheckpointDeliveryOutbox(source, destination, snapshot, event.Sequence)
	if err != nil {
		return err
	}
	_, err = repo.EnqueueRuntimeCheckpointDeliveryOutbox(ctx, item)
	return err
}

func runtimeNoticeInt(value any) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, true
	case int64:
		return int(number), true
	case float64:
		return int(number), number >= 0
	default:
		return 0, false
	}
}

// Subscribe registers a live event consumer. The returned backlog was read
// after registration; callers deduplicate by sequence when combining it with
// live events. The channel is closed by Unsubscribe or Coordinator.Close.
func (c *Coordinator) Subscribe(ctx context.Context, invocationID string, after int64) ([]AgentEvent, <-chan AgentEvent, func(), error) {
	if _, err := c.repo.GetInvocation(ctx, invocationID); err != nil {
		return nil, nil, nil, err
	}
	ch := make(chan AgentEvent, 64)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, nil, nil, ErrConflict
	}
	if c.subscribers[invocationID] == nil {
		c.subscribers[invocationID] = make(map[chan AgentEvent]struct{})
	}
	c.subscribers[invocationID][ch] = struct{}{}
	c.mu.Unlock()
	backlog, err := c.repo.ListEvents(ctx, invocationID, after, 500)
	if err != nil {
		c.unsubscribe(invocationID, ch)
		return nil, nil, nil, err
	}
	return backlog, ch, func() { c.unsubscribe(invocationID, ch) }, nil
}

func (c *Coordinator) CancelInvocation(ctx context.Context, id string) (Invocation, error) {
	c.approvalMu.Lock()
	defer c.approvalMu.Unlock()
	id = strings.TrimSpace(id)
	item, err := c.repo.GetInvocation(ctx, id)
	if err != nil {
		return Invocation{}, err
	}
	if item.Status.Terminal() {
		return item, nil
	}
	terminalized := false
	if item.Status == InvocationRunning {
		if ok, transitionErr := c.repo.TransitionInvocation(ctx, id, item.Status, InvocationCancelling, "用户取消"); transitionErr != nil {
			return Invocation{}, transitionErr
		} else if !ok {
			return c.repo.GetInvocation(ctx, id)
		}
		_, _ = c.appendAndPublish(ctx, AgentEvent{ID: newID("event"), InvocationID: id, Type: EventInvocationCancelling, Timestamp: time.Now().UTC(), Data: map[string]any{"reason": "用户取消"}})
	} else if ok, transitionErr := c.repo.TransitionInvocation(ctx, id, item.Status, InvocationCancelled, "用户取消"); transitionErr != nil {
		return Invocation{}, transitionErr
	} else if !ok {
		return c.repo.GetInvocation(ctx, id)
	} else {
		terminalized = true
	}
	c.mu.Lock()
	if handle, ok := c.runs[id]; ok && handle.cancel != nil {
		handle.cancel()
	}
	c.mu.Unlock()
	// Close every pending approval before publishing the terminal Invocation
	// event. A cancellation must not leave an approval card that can later
	// resurrect a cancelled task or a prepared workspace operation.
	c.approvalsOnCancel(ctx, id)
	if terminalized {
		c.deleteInvocationResume(context.WithoutCancel(ctx), id)
		c.deleteInvocationResumeOutbox(context.WithoutCancel(ctx), id)
	}
	if current, getErr := c.repo.GetInvocation(ctx, id); getErr == nil && current.Status == InvocationCancelling {
		if ok, _ := c.repo.TransitionInvocation(ctx, id, InvocationCancelling, InvocationCancelled, "用户取消"); ok {
			terminalized = true
		}
	}
	if terminalized {
		c.deleteInvocationResumeOutbox(context.WithoutCancel(ctx), id)
		c.refreshRuntimeSnapshot(context.WithoutCancel(ctx), id)
		c.emitTerminal(ctx, id, EventInvocationCancelled, map[string]any{"reason": "用户取消"})
	}
	return c.repo.GetInvocation(ctx, id)
}

func validInvocationTransition(from, to InvocationStatus) bool {
	if from == to {
		return false
	}
	switch from {
	case InvocationQueued:
		return to == InvocationRunning || to == InvocationCancelling || to == InvocationCancelled || to == InvocationFailed
	case InvocationRunning:
		return to == InvocationWaitingApproval || to == InvocationWaitingTool || to == InvocationWaitingUser || to == InvocationQueued || to == InvocationCompleted || to == InvocationFailed || to == InvocationCancelling || to == InvocationCancelled
	case InvocationWaitingApproval, InvocationWaitingTool, InvocationWaitingUser:
		return to == InvocationQueued || to == InvocationCancelling || to == InvocationCancelled || to == InvocationExpired || to == InvocationFailed
	case InvocationCancelling:
		return to == InvocationCancelled || to == InvocationFailed
	default:
		return false
	}
}

func terminalEventForStatus(status InvocationStatus) string {
	switch status {
	case InvocationCompleted:
		return EventInvocationCompleted
	case InvocationCancelled:
		return EventInvocationCancelled
	case InvocationExpired:
		return EventInvocationExpired
	default:
		return EventInvocationFailed
	}
}

func (c *Coordinator) approvalsOnCancel(ctx context.Context, invocationID string) {
	items, err := c.repo.ListApprovals(ctx, invocationID, ApprovalPending)
	if err != nil {
		return
	}
	c.mu.Lock()
	cancelHandler := c.approvalCancelled
	c.mu.Unlock()
	for _, item := range items {
		if cancelHandler != nil {
			if err := cancelHandler(ctx, item); err != nil {
				continue
			}
		}
		resolved, changed, err := c.repo.ResolveApproval(ctx, item.ID, ApprovalCancelled, "Invocation 已取消")
		if err != nil || !changed {
			continue
		}
		c.updateToolCallStatus(context.WithoutCancel(ctx), resolved.ToolCallID, ToolCallCancelled, "Invocation 已取消")
		_, _ = c.appendAndPublish(context.WithoutCancel(ctx), AgentEvent{
			ID: newID("event"), InvocationID: invocationID, Type: EventApprovalResolved,
			Timestamp: time.Now().UTC(), Data: map[string]any{
				"approval_id": resolved.ID, "confirmed": false, "decision": string(ApprovalCancelled),
			},
		})
	}
}

// ExpireApprovals closes every pending approval whose deadline has elapsed.
// It is safe to call from a scheduler or synchronously before resolving an
// approval; the approval repository CAS makes repeated scans idempotent.
func (c *Coordinator) ExpireApprovals(ctx context.Context, now time.Time) error {
	c.approvalMu.Lock()
	defer c.approvalMu.Unlock()
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	items, err := c.repo.ListApprovals(ctx, "", ApprovalPending)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.ExpiresAt == nil || item.ExpiresAt.After(now) {
			continue
		}
		if err := c.expireApprovalLocked(ctx, item, now); err != nil {
			return err
		}
	}
	return nil
}

func (c *Coordinator) expireApprovalLocked(ctx context.Context, item Approval, now time.Time) error {
	c.mu.Lock()
	expiryHandler := c.approvalExpired
	c.mu.Unlock()
	if expiryHandler != nil {
		if err := expiryHandler(ctx, item); err != nil {
			return err
		}
	}
	resolved, changed, err := c.repo.ResolveApproval(ctx, item.ID, ApprovalExpired, "审批已过期")
	if err != nil {
		return err
	}
	if !changed {
		if resolved.Status == ApprovalExpired {
			return nil
		}
		return nil
	}
	c.updateToolCallStatus(context.WithoutCancel(ctx), resolved.ToolCallID, ToolCallExpired, "审批已过期")
	withoutCancel := context.WithoutCancel(ctx)
	_, _ = c.appendAndPublish(withoutCancel, AgentEvent{
		ID: newID("event"), InvocationID: item.InvocationID, Type: EventApprovalExpired,
		Timestamp: now, Data: map[string]any{"approval_id": resolved.ID, "decision": string(ApprovalExpired), "reason": "审批已过期"},
	})
	_, _ = c.appendAndPublish(withoutCancel, AgentEvent{
		ID: newID("event"), InvocationID: item.InvocationID, Type: EventApprovalResolved,
		Timestamp: now, Data: map[string]any{"approval_id": resolved.ID, "confirmed": false, "decision": string(ApprovalExpired), "reason": "审批已过期"},
	})
	invocation, getErr := c.repo.GetInvocation(withoutCancel, item.InvocationID)
	if getErr != nil {
		return getErr
	}
	if invocation.Status.Terminal() {
		return nil
	}
	if ok, transitionErr := c.repo.TransitionInvocation(withoutCancel, invocation.ID, invocation.Status, InvocationExpired, "approval_expired: 审批已超过有效期"); transitionErr != nil {
		return transitionErr
	} else if ok {
		c.refreshRuntimeSnapshot(withoutCancel, invocation.ID)
		c.emitTerminal(withoutCancel, invocation.ID, EventInvocationExpired, map[string]any{"error": "approval_expired: 审批已超过有效期", "approval_id": resolved.ID})
	}
	return nil
}

// ResolveApproval performs an atomic decision and schedules the original task
// for ADK resume using the confirmation call ID, never the original tool ID.
func (c *Coordinator) ResolveApproval(ctx context.Context, approvalID string, approved bool, reason string) (Approval, error) {
	choiceID := "reject"
	if approved {
		choiceID = "approve"
	}
	return c.ResolveApprovalChoice(ctx, approvalID, choiceID, reason)
}

// ResolveApprovalChoice resolves the exact option displayed to the user. The
// choice is validated against the durable approval record before any state
// transition, so a client cannot invent a permissive option by sending a
// boolean or arbitrary text.
func (c *Coordinator) ResolveApprovalChoice(ctx context.Context, approvalID, choiceID, reason string) (Approval, error) {
	c.approvalMu.Lock()
	defer c.approvalMu.Unlock()
	approval, err := c.repo.GetApproval(ctx, strings.TrimSpace(approvalID))
	if err != nil {
		return Approval{}, err
	}
	choices := normalizeApprovalChoices(approval.Choices)
	choiceID = strings.TrimSpace(choiceID)
	var choice ApprovalChoice
	for _, candidate := range choices {
		if candidate.ID == choiceID {
			choice = candidate
			break
		}
	}
	if choice.ID == "" {
		return Approval{}, fmt.Errorf("%w: 选项 %q 不属于该审批", ErrConflict, choiceID)
	}
	approved := choice.Approved
	status := ApprovalRejected
	if approved {
		status = ApprovalApproved
	}
	if approval.Status != ApprovalPending {
		if approval.Status == status {
			return approval, nil
		}
		return Approval{}, fmt.Errorf("%w: 当前状态为 %s，不能改为 %s", ErrAlreadyResolved, approval.Status, status)
	}
	if approval.ExpiresAt != nil && !time.Now().UTC().Before(approval.ExpiresAt.UTC()) {
		if err := c.expireApprovalLocked(context.WithoutCancel(ctx), approval, time.Now().UTC()); err != nil {
			return Approval{}, err
		}
		return Approval{}, ErrApprovalExpired
	}
	invocation, err := c.repo.GetInvocation(ctx, approval.InvocationID)
	if err != nil {
		return Approval{}, err
	}
	if invocation.Status.Terminal() {
		return Approval{}, fmt.Errorf("%w: invocation 已结束", ErrConflict)
	}
	if err := c.validateResumeSnapshots(ctx, invocation); err != nil {
		return Approval{}, err
	}
	if approval.TaskContractVersion > 0 {
		if contractRepo, ok := c.repo.(TaskContractRepository); ok {
			currentContract, contractErr := contractRepo.GetTaskContract(ctx, approval.InvocationID)
			if contractErr != nil {
				return Approval{}, contractErr
			}
			if currentContract.Version != approval.TaskContractVersion {
				return Approval{}, fmt.Errorf("%w: 审批基于任务契约 v%d，当前为 v%d，请重新评估", ErrConflict, approval.TaskContractVersion, currentContract.Version)
			}
		}
	}
	c.mu.Lock()
	instructionValidator := c.instructionValidator
	c.mu.Unlock()
	if instructionValidator != nil {
		if err := instructionValidator(ctx, approval.InvocationID); err != nil {
			return Approval{}, fmt.Errorf("%w: 项目指令快照已失效: %v", ErrConflict, err)
		}
	}
	if strings.TrimSpace(approval.ConfirmationCallID) == "" {
		return Approval{}, fmt.Errorf("%w: approval 缺少 confirmation_call_id，无法安全恢复", ErrConflict)
	}
	// Build the exact confirmation response before changing the Approval or
	// Invocation state. SQLite/Memory can persist this bounded private handoff
	// so a crash after the CAS cannot turn an accepted decision into a replay
	// of the original user message. Custom repositories keep the existing
	// in-process launch path for compatibility.
	resumePayload := map[string]any{"choice_id": choice.ID}
	if approval.OperationID != "" {
		resumePayload["operation_id"] = approval.OperationID
		resumePayload["invocation_id"] = invocation.ID
		resumePayload["tool_call_id"] = approval.OriginalCallID
	}
	resumeResponse := map[string]any{"confirmed": approved, "payload": resumePayload}
	resumeFingerprint, encodedResume, fingerprintErr := invocationResumeFingerprint(InvocationResumeRequest{
		WaitID: approval.ConfirmationCallID, Name: toolconfirmation.FunctionCallName, Response: resumeResponse,
	})
	if fingerprintErr != nil {
		return Approval{}, fingerprintErr
	}
	resumeHandoff := InvocationResume{
		ID: newID("resume"), InvocationID: invocation.ID, WaitID: approval.ConfirmationCallID,
		Name: toolconfirmation.FunctionCallName, ResponseJSON: encodedResume, RequestDigest: resumeFingerprint, CreatedAt: time.Now().UTC(),
	}
	resumeOutbox := InvocationResumeOutbox{
		ID: resumeHandoff.ID, InvocationID: invocation.ID, WaitID: resumeHandoff.WaitID, Name: resumeHandoff.Name,
		ResponseJSON: append([]byte(nil), resumeHandoff.ResponseJSON...), RequestDigest: resumeHandoff.RequestDigest,
		Status: InvocationResumeOutboxQueued, AvailableAt: resumeHandoff.CreatedAt, CreatedAt: resumeHandoff.CreatedAt, UpdatedAt: resumeHandoff.CreatedAt,
	}
	var rejectionDelivery *RuntimeApprovalRejectionDeliveryOutbox
	if !approved {
		source, destination, _, enabled := c.runtimeApprovalRejectionDeliveryConfig()
		if enabled {
			if _, ok := c.repo.(RuntimeApprovalRejectionDeliveryOutboxRepository); !ok {
				return Approval{}, fmt.Errorf("%w: 已配置跨服务拒绝投递，但 Repository 未提供 durable outbox", ErrConflict)
			}
			if _, ok := c.repo.(ApprovalRejectionDeliveryCommitRepository); !ok {
				return Approval{}, fmt.Errorf("%w: 已配置跨服务拒绝投递，但 Repository 未提供原子拒绝提交", ErrConflict)
			}
			item, outboxErr := NewRuntimeApprovalRejectionDeliveryOutbox(source, destination, approval, invocation, reason, time.Now().UTC())
			if outboxErr != nil {
				return Approval{}, outboxErr
			}
			rejectionDelivery = &item
		}
	}
	toolStatus := ToolCallRejected
	if approved {
		toolStatus = ToolCallApproved
	}
	resumeCommit := ApprovalResumeCommit{
		ApprovalID: approval.ID, ApprovalFromStatus: ApprovalPending, ApprovalToStatus: status,
		InvocationID: invocation.ID, InvocationFromStatus: invocation.Status, InvocationToStatus: InvocationQueued,
		ToolCallID: approval.ToolCallID, ToolCallStatus: toolStatus, Reason: reason,
		Resume: resumeHandoff, Outbox: resumeOutbox, RejectionDelivery: rejectionDelivery,
		Events: []AgentEvent{
			{ID: newID("event"), InvocationID: invocation.ID, Type: EventApprovalResolved, Timestamp: time.Now().UTC(), Data: map[string]any{"approval_id": approval.ID, "confirmed": approved, "choice_id": choice.ID, "reason": reason}},
			{ID: newID("event"), InvocationID: invocation.ID, Type: EventInvocationResumed, Timestamp: time.Now().UTC(), Data: map[string]any{"approval_id": approval.ID, "confirmed": approved, "choice_id": choice.ID, "request_digest": resumeFingerprint}},
		},
	}
	atomicCommitted := false
	if approved {
		if atomicRepo, ok := c.repo.(ApprovalResumeCommitRepository); ok {
			committedApproval, committedInvocation, storedEvents, commitErr := atomicRepo.CommitApprovalResume(ctx, resumeCommit)
			if commitErr != nil {
				return Approval{}, commitErr
			}
			approval = committedApproval
			invocation = committedInvocation
			for _, storedEvent := range storedEvents {
				c.publishStoredEvent(storedEvent)
			}
			atomicCommitted = true
		}
	} else if resumeCommit.RejectionDelivery != nil {
		atomicRepo, ok := c.repo.(ApprovalRejectionDeliveryCommitRepository)
		if !ok {
			return Approval{}, fmt.Errorf("%w: Repository 未提供拒绝投递原子提交", ErrConflict)
		}
		committedApproval, committedInvocation, storedEvents, commitErr := atomicRepo.CommitApprovalRejectionWithDelivery(ctx, resumeCommit, *resumeCommit.RejectionDelivery)
		if commitErr != nil {
			return Approval{}, commitErr
		}
		approval = committedApproval
		invocation = committedInvocation
		for _, storedEvent := range storedEvents {
			c.publishStoredEvent(storedEvent)
		}
		atomicCommitted = true
	} else if atomicRepo, ok := c.repo.(ApprovalRejectionCommitRepository); ok {
		// A capable local repository owns the Workspace sidecar boundary. Do not
		// invoke the legacy callback first: doing so would make a later Runtime
		// CAS/event failure leave Operation= rejected while Approval is pending.
		committedApproval, committedInvocation, storedEvents, commitErr := atomicRepo.CommitApprovalRejection(ctx, resumeCommit)
		if commitErr != nil {
			return Approval{}, commitErr
		}
		approval = committedApproval
		invocation = committedInvocation
		for _, storedEvent := range storedEvents {
			c.publishStoredEvent(storedEvent)
		}
		atomicCommitted = true
	}
	if !atomicCommitted {
		if !approved {
			c.mu.Lock()
			decisionHandler := c.approvalDecision
			c.mu.Unlock()
			if decisionHandler != nil {
				if err := decisionHandler(ctx, approval, false); err != nil {
					return Approval{}, err
				}
			}
		}
		if _, supportsHandoff := c.repo.(InvocationResumeRepository); supportsHandoff {
			if err := c.saveInvocationResume(ctx, resumeHandoff); err != nil {
				return Approval{}, err
			}
		}
		if err := c.enqueueInvocationResumeOutbox(ctx, resumeHandoff); err != nil {
			return Approval{}, err
		}
		approval, changed, err := c.repo.ResolveApproval(ctx, strings.TrimSpace(approvalID), status, reason)
		if err != nil {
			return Approval{}, err
		}
		if !changed {
			if approval.Status == status {
				return approval, nil
			}
			return Approval{}, fmt.Errorf("%w: 当前状态为 %s，不能改为 %s", ErrAlreadyResolved, approval.Status, status)
		}
		c.updateToolCallStatus(ctx, approval.ToolCallID, toolStatus, reason)
		if ok, transitionErr := c.repo.TransitionInvocation(ctx, invocation.ID, invocation.Status, InvocationQueued, ""); transitionErr != nil {
			return Approval{}, transitionErr
		} else if !ok {
			return Approval{}, ErrConflict
		}
		// A resumed invocation no longer has an outstanding approval. Clearing the
		// pointer before launching the new run keeps the durable task envelope
		// truthful even if the process exits during the resumed turn.
		invocation.Status = InvocationQueued
		invocation.ActiveApprovalID = ""
		// The worker that produced this approval has released its execution lease;
		// do not write the stale owner back while preparing the queued resume.
		invocation.LeaseOwner = ""
		invocation.LeaseExpiresAt = nil
		_ = c.repo.UpdateInvocation(ctx, invocation)
		_, _ = c.appendAndPublish(ctx, AgentEvent{
			ID: newID("event"), InvocationID: invocation.ID, Type: EventApprovalResolved,
			Timestamp: time.Now().UTC(), Data: map[string]any{
				"approval_id": approval.ID, "confirmed": approved, "reason": reason,
			},
		})
		_, _ = c.appendAndPublish(ctx, AgentEvent{
			ID: newID("event"), InvocationID: invocation.ID, Type: EventInvocationResumed,
			Timestamp: time.Now().UTC(), Data: map[string]any{"approval_id": approval.ID, "confirmed": approved, "request_digest": resumeFingerprint},
		})
	}
	c.refreshRuntimeSnapshot(context.WithoutCancel(ctx), invocation.ID)
	resume := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
		Name: toolconfirmation.FunctionCallName, ID: approval.ConfirmationCallID,
		Response: resumeResponse,
	}}}}
	var workspaceID *string
	if strings.TrimSpace(invocation.WorkspaceID) != "" {
		value := invocation.WorkspaceID
		workspaceID = &value
	}
	c.launch(invocation.ID, agent.ChatRequest{
		UserID: invocation.UserID, BotID: invocation.BotID, ConversationID: invocation.ConversationID,
		SessionID: invocation.SessionID, ProviderID: invocation.ProviderID, ModelID: invocation.ModelID, WorkspaceID: workspaceID, TargetPath: invocation.TargetPath,
		ConfigSnapshot: invocation.ConfigSnapshot, Proactive: invocation.Proactive, ResumeContent: resume, Stream: true,
	})
	return approval, nil
}

const (
	maxInvocationResumeResponseBytes = 256 << 10
	maxInvocationResumeItems         = 16
)

func (c *Coordinator) resumeContentForInvocation(ctx context.Context, invocationID string) (*genai.Content, error) {
	invocationID = strings.TrimSpace(invocationID)
	// The outbox is the recovery source of truth after the private handoff has
	// already been acknowledged or after a process crashed between enqueue and
	// the next Runner activation. Only non-terminal records can be replayed.
	if repo, ok := c.repo.(InvocationResumeOutboxRepository); ok {
		item, err := repo.GetInvocationResumeOutbox(ctx, invocationID)
		if err == nil {
			if item.Status != InvocationResumeOutboxQueued && item.Status != InvocationResumeOutboxProcessing {
				return nil, nil
			}
			return invocationResumeContent(InvocationResume{
				InvocationID: item.InvocationID, WaitID: item.WaitID, Name: item.Name, ResponseJSON: item.ResponseJSON,
			})
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	// Older repositories only persist the one-row handoff. Keep that path as
	// a compatibility fallback when no outbox record exists.
	if repo, ok := c.repo.(InvocationResumeRepository); ok {
		item, err := repo.GetInvocationResume(ctx, invocationID)
		if err == nil {
			return invocationResumeContent(item)
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	return nil, nil
}

func invocationResumeContent(item InvocationResume) (*genai.Content, error) {
	if strings.TrimSpace(item.WaitID) == "" || strings.TrimSpace(item.Name) == "" || len(item.ResponseJSON) > maxInvocationResumeResponseBytes {
		return nil, fmt.Errorf("%w: durable resume handoff 字段无效", ErrInvalidResume)
	}
	responses, err := decodeInvocationResumeResponses(item)
	if err != nil {
		return nil, err
	}
	parts := make([]*genai.Part, 0, len(responses))
	for _, response := range responses {
		parts = append(parts, &genai.Part{FunctionResponse: &genai.FunctionResponse{
			Name: response.Name, ID: response.WaitID, Response: response.Response,
		}})
	}
	return &genai.Content{Role: genai.RoleUser, Parts: parts}, nil
}

func decodeInvocationResumeResponses(item InvocationResume) ([]InvocationResumeItem, error) {
	if len(item.ResponseJSON) == 0 {
		return []InvocationResumeItem{{WaitID: strings.TrimSpace(item.WaitID), Name: strings.TrimSpace(item.Name), Response: map[string]any{}}}, nil
	}
	var batch []InvocationResumeItem
	if first := strings.TrimSpace(string(item.ResponseJSON)); strings.HasPrefix(first, "[") {
		if err := json.Unmarshal(item.ResponseJSON, &batch); err != nil {
			return nil, fmt.Errorf("%w: durable resume response 无法解析: %v", ErrInvalidResume, err)
		}
		if len(batch) == 0 || len(batch) > maxInvocationResumeItems {
			return nil, fmt.Errorf("%w: durable resume response 数量超出范围", ErrInvalidResume)
		}
	} else {
		var response map[string]any
		if err := json.Unmarshal(item.ResponseJSON, &response); err != nil {
			return nil, fmt.Errorf("%w: durable resume response 无法解析: %v", ErrInvalidResume, err)
		}
		batch = []InvocationResumeItem{{WaitID: strings.TrimSpace(item.WaitID), Name: strings.TrimSpace(item.Name), Response: response}}
	}
	seen := make(map[string]struct{}, len(batch))
	for index := range batch {
		batch[index].WaitID = strings.TrimSpace(batch[index].WaitID)
		batch[index].Name = strings.TrimSpace(batch[index].Name)
		if batch[index].WaitID == "" || batch[index].Name == "" || batch[index].Response == nil {
			return nil, fmt.Errorf("%w: durable resume response 第 %d 项字段无效", ErrInvalidResume, index+1)
		}
		if _, ok := seen[batch[index].WaitID]; ok {
			return nil, fmt.Errorf("%w: durable resume response wait_id 重复", ErrInvalidResume)
		}
		seen[batch[index].WaitID] = struct{}{}
	}
	return batch, nil
}

func normalizeInvocationResumeItems(request InvocationResumeRequest) ([]InvocationResumeItem, error) {
	request.WaitID = strings.TrimSpace(request.WaitID)
	request.Name = strings.TrimSpace(request.Name)
	items := make([]InvocationResumeItem, 0, len(request.Responses)+1)
	if request.WaitID != "" || request.Name != "" || request.Response != nil {
		if len(request.Responses) > 0 {
			return nil, fmt.Errorf("%w: 单个 response 不能与 responses 同时提供", ErrInvalidResume)
		}
		items = append(items, InvocationResumeItem{WaitID: request.WaitID, Name: request.Name, Response: request.Response})
	} else {
		items = append(items, request.Responses...)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%w: 至少提供一个 wait_id/response", ErrInvalidResume)
	}
	if len(items) > maxInvocationResumeItems {
		return nil, fmt.Errorf("%w: 最多同时恢复 %d 个 wait ID", ErrInvalidResume, maxInvocationResumeItems)
	}
	seen := make(map[string]struct{}, len(items))
	totalBytes := 0
	for index := range items {
		items[index].WaitID = strings.TrimSpace(items[index].WaitID)
		items[index].Name = strings.TrimSpace(items[index].Name)
		if items[index].WaitID == "" {
			return nil, fmt.Errorf("%w: 第 %d 项 wait_id 不能为空", ErrInvalidResume, index+1)
		}
		if _, ok := seen[items[index].WaitID]; ok {
			return nil, fmt.Errorf("%w: wait_id=%s 重复", ErrInvalidResume, items[index].WaitID)
		}
		seen[items[index].WaitID] = struct{}{}
		if items[index].Response == nil {
			return nil, fmt.Errorf("%w: wait_id=%s 的 response 必须是 JSON 对象", ErrInvalidResume, items[index].WaitID)
		}
		encoded, err := json.Marshal(items[index].Response)
		if err != nil {
			return nil, fmt.Errorf("%w: wait_id=%s 的 response 必须是 JSON 对象: %v", ErrInvalidResume, items[index].WaitID, err)
		}
		if len(encoded) > maxInvocationResumeResponseBytes {
			return nil, fmt.Errorf("%w: wait_id=%s 的 response 超过 %d 字节上限", ErrInvalidResume, items[index].WaitID, maxInvocationResumeResponseBytes)
		}
		totalBytes += len(encoded)
		if totalBytes > maxInvocationResumeResponseBytes {
			return nil, fmt.Errorf("%w: response 总大小超过 %d 字节上限", ErrInvalidResume, maxInvocationResumeResponseBytes)
		}
	}
	return items, nil
}

func resumeEventData(items []InvocationResumeItem, fingerprint string, boundaryIDs ...string) map[string]any {
	data := map[string]any{"reason": "tool", "wait_id": items[0].WaitID, "tool_name": items[0].Name, "request_digest": fingerprint}
	if len(boundaryIDs) > 0 && strings.TrimSpace(boundaryIDs[0]) != "" {
		data["boundary_id"] = strings.TrimSpace(boundaryIDs[0])
	}
	if len(items) > 1 {
		waitIDs := make([]string, 0, len(items))
		toolNames := make([]string, 0, len(items))
		for _, item := range items {
			waitIDs = append(waitIDs, item.WaitID)
			toolNames = append(toolNames, item.Name)
		}
		data["wait_ids"] = waitIDs
		data["tool_names"] = toolNames
	}
	return data
}

// resumeUserInputEventData 保留用户回答的边界类型和请求 ID，避免把普通
// 问题的回答误记成工具恢复或审批决定。
func resumeUserInputEventData(items []InvocationResumeItem, fingerprint string, boundaryIDs ...string) map[string]any {
	data := map[string]any{"reason": "user", "wait_id": items[0].WaitID, "name": items[0].Name, "request_digest": fingerprint}
	if len(boundaryIDs) > 0 && strings.TrimSpace(boundaryIDs[0]) != "" {
		data["boundary_id"] = strings.TrimSpace(boundaryIDs[0])
	}
	if len(items) > 1 {
		waitIDs := make([]string, 0, len(items))
		for _, item := range items {
			waitIDs = append(waitIDs, item.WaitID)
		}
		data["wait_ids"] = waitIDs
	}
	return data
}

func resumeEventDataWithStatusProof(items []InvocationResumeItem, fingerprint string, request InvocationResumeRequest, boundaryIDs ...string) map[string]any {
	data := resumeEventData(items, fingerprint, boundaryIDs...)
	if len(request.statusQueryIDs) == 0 && len(request.statusResultDigest) == 0 {
		return data
	}
	data["resume_source"] = "external_tool_status"
	if len(request.statusQueryIDs) == 1 {
		data["status_query_id"] = request.statusQueryIDs[0]
	} else if len(request.statusQueryIDs) > 1 {
		data["status_query_ids"] = append([]string(nil), request.statusQueryIDs...)
	}
	if len(request.statusResultDigest) == 1 {
		data["result_digest"] = request.statusResultDigest[0]
	} else if len(request.statusResultDigest) > 1 {
		data["result_digests"] = append([]string(nil), request.statusResultDigest...)
	}
	return data
}

func resumeUserInputEventDataWithStatusProof(items []InvocationResumeItem, fingerprint string, request InvocationResumeRequest, boundaryIDs ...string) map[string]any {
	data := resumeUserInputEventData(items, fingerprint, boundaryIDs...)
	if len(request.statusQueryIDs) == 0 && len(request.statusResultDigest) == 0 {
		return data
	}
	data["resume_source"] = "external_tool_status"
	if len(request.statusQueryIDs) == 1 {
		data["status_query_id"] = request.statusQueryIDs[0]
	} else if len(request.statusQueryIDs) > 1 {
		data["status_query_ids"] = append([]string(nil), request.statusQueryIDs...)
	}
	if len(request.statusResultDigest) == 1 {
		data["result_digest"] = request.statusResultDigest[0]
	} else if len(request.statusResultDigest) > 1 {
		data["result_digests"] = append([]string(nil), request.statusResultDigest...)
	}
	return data
}

func (c *Coordinator) saveInvocationResume(ctx context.Context, item InvocationResume) error {
	repo, ok := c.repo.(InvocationResumeRepository)
	if !ok {
		return nil
	}
	if _, err := repo.SaveInvocationResume(ctx, item); err != nil {
		if errors.Is(err, ErrConflict) {
			return fmt.Errorf("%w: wait_id=%s 已存在不同恢复请求", ErrResumeMismatch, item.WaitID)
		}
		return err
	}
	return nil
}

func (c *Coordinator) enqueueInvocationResumeOutbox(ctx context.Context, item InvocationResume) error {
	repo, ok := c.repo.(InvocationResumeOutboxRepository)
	if !ok {
		return nil
	}
	if strings.TrimSpace(item.InvocationID) == "" || len(item.ResponseJSON) > maxInvocationResumeResponseBytes {
		return ErrInvalidResume
	}
	_, err := repo.EnqueueInvocationResumeOutbox(ctx, InvocationResumeOutbox{
		ID: item.ID, InvocationID: item.InvocationID, WaitID: item.WaitID, Name: item.Name,
		ResponseJSON: append([]byte(nil), item.ResponseJSON...), RequestDigest: item.RequestDigest,
		Status: InvocationResumeOutboxQueued, AvailableAt: item.CreatedAt, CreatedAt: item.CreatedAt, UpdatedAt: item.CreatedAt,
	})
	return err
}

func (c *Coordinator) claimInvocationResumeOutbox(ctx context.Context, invocationID, owner string) (InvocationResumeOutbox, bool, error) {
	repo, ok := c.repo.(InvocationResumeOutboxRepository)
	if !ok {
		return InvocationResumeOutbox{}, false, nil
	}
	return repo.ClaimInvocationResumeOutbox(ctx, strings.TrimSpace(invocationID), strings.TrimSpace(owner), time.Now().UTC(), InvocationLeaseTTL)
}

func (c *Coordinator) completeInvocationResumeOutbox(ctx context.Context, invocationID, owner string) {
	repo, ok := c.repo.(InvocationResumeOutboxRepository)
	if !ok {
		return
	}
	_, _ = repo.CompleteInvocationResumeOutbox(ctx, strings.TrimSpace(invocationID), strings.TrimSpace(owner), time.Now().UTC())
}

func (c *Coordinator) retryInvocationResumeOutbox(ctx context.Context, invocationID, owner, message string) {
	repo, ok := c.repo.(InvocationResumeOutboxRepository)
	if !ok {
		return
	}
	_, _ = repo.RetryInvocationResumeOutbox(ctx, strings.TrimSpace(invocationID), strings.TrimSpace(owner), time.Now().UTC(), message)
}

func (c *Coordinator) deleteInvocationResumeOutbox(ctx context.Context, invocationID string) {
	repo, ok := c.repo.(InvocationResumeOutboxRepository)
	if !ok {
		return
	}
	_ = repo.DeleteInvocationResumeOutbox(ctx, strings.TrimSpace(invocationID))
}

func (c *Coordinator) pendingResumeMatches(ctx context.Context, invocationID, fingerprint string) (bool, error) {
	repo, ok := c.repo.(InvocationResumeRepository)
	if !ok {
		return false, nil
	}
	item, err := repo.GetInvocationResume(ctx, strings.TrimSpace(invocationID))
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(item.RequestDigest) == strings.TrimSpace(fingerprint), nil
}

func (c *Coordinator) deleteInvocationResume(ctx context.Context, invocationID string) {
	repo, ok := c.repo.(InvocationResumeRepository)
	if !ok {
		return
	}
	_ = repo.DeleteInvocationResume(ctx, strings.TrimSpace(invocationID))
}

// ResumeInvocation answers one durable ADK long-running tool boundary. The
// response is deliberately submitted as a FunctionResponse rather than a new
// user message, so ADK can rehydrate the paused workflow from its Session.
// Only the FunctionCall ID recorded in the latest waiting_tool boundary is
// accepted. A successful request changes waiting_tool -> queued before the
// worker is launched; a repeated request with the same fingerprint is
// idempotent and returns the current Invocation without launching a second
// worker.
func (c *Coordinator) ResumeInvocation(ctx context.Context, invocationID string, request InvocationResumeRequest) (Invocation, error) {
	if c == nil || c.repo == nil {
		return Invocation{}, errors.New("Runtime Coordinator 不能为空")
	}
	if err := c.ensureStarted(); err != nil {
		return Invocation{}, err
	}
	invocationID = strings.TrimSpace(invocationID)
	request.WaitID = strings.TrimSpace(request.WaitID)
	request.Name = strings.TrimSpace(request.Name)
	request.BoundaryID = strings.TrimSpace(request.BoundaryID)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if invocationID == "" {
		return Invocation{}, fmt.Errorf("%w: invocation_id 不能为空", ErrInvalidResume)
	}
	resumeItems, err := normalizeInvocationResumeItems(request)
	if err != nil {
		return Invocation{}, err
	}
	if len(resumeItems) == 1 {
		request.WaitID = resumeItems[0].WaitID
		request.Name = resumeItems[0].Name
		request.Response = resumeItems[0].Response
		request.Responses = nil
	} else {
		request.WaitID = ""
		request.Name = ""
		request.Response = nil
		request.Responses = resumeItems
	}
	fingerprint, encodedResponse, err := invocationResumeFingerprint(request)
	if err != nil {
		return Invocation{}, err
	}

	// Cancellation, approval resolution and tool resume all compete for the
	// same waiting boundary. Serialising the read/CAS/event hand-off keeps a
	// cancellation from winning after a resume has already been accepted.
	c.approvalMu.Lock()
	defer c.approvalMu.Unlock()
	invocation, err := c.repo.GetInvocation(ctx, invocationID)
	if err != nil {
		return Invocation{}, err
	}
	if invocation.Status != InvocationWaitingTool && invocation.Status != InvocationWaitingUser {
		matched, matchErr := c.hasResumeFingerprint(ctx, invocationID, fingerprint)
		if matchErr != nil {
			return Invocation{}, matchErr
		}
		if matched {
			return invocation, nil
		}
		if invocation.Status.Terminal() {
			return Invocation{}, fmt.Errorf("%w: invocation 已结束", ErrConflict)
		}
		return Invocation{}, fmt.Errorf("%w: invocation 当前状态为 %s", ErrResumeMismatch, invocation.Status)
	}
	if err := c.validateResumeSnapshots(ctx, invocation); err != nil {
		return Invocation{}, err
	}

	waitingStatus := invocation.Status
	boundaryKind := WorkflowBoundaryTool
	var toolNames map[string]string
	var resolvedBoundaryID string
	if waitingStatus == InvocationWaitingUser {
		boundaryKind = WorkflowBoundaryUser
		toolNames, resolvedBoundaryID, err = c.resolveWorkflowUserBoundary(ctx, invocation, request.BoundaryID)
	} else {
		toolNames, resolvedBoundaryID, err = c.resolveWorkflowToolBoundary(ctx, invocation, request.BoundaryID)
	}
	if err != nil {
		return Invocation{}, err
	}
	if len(resumeItems) != len(toolNames) {
		return Invocation{}, fmt.Errorf("%w: 当前等待边界有 %d 个待恢复项，必须在同一请求中提交全部 responses", ErrResumeMismatch, len(toolNames))
	}
	resolvedItems := make([]InvocationResumeItem, 0, len(resumeItems))
	seenWaitIDs := make(map[string]struct{}, len(resumeItems))
	for _, item := range resumeItems {
		if _, duplicate := seenWaitIDs[item.WaitID]; duplicate {
			return Invocation{}, fmt.Errorf("%w: wait_id=%s 重复", ErrInvalidResume, item.WaitID)
		}
		seenWaitIDs[item.WaitID] = struct{}{}
		toolName, ok := toolNames[item.WaitID]
		if !ok {
			return Invocation{}, fmt.Errorf("%w: wait_id=%s 不属于当前等待边界", ErrResumeMismatch, item.WaitID)
		}
		if item.Name != "" && toolName != "" && item.Name != toolName {
			return Invocation{}, fmt.Errorf("%w: wait_id=%s 的工具名称不匹配", ErrResumeMismatch, item.WaitID)
		}
		if toolName == "" {
			toolName = item.Name
		}
		if toolName == "" {
			return Invocation{}, fmt.Errorf("%w: wait_id=%s 的等待边界缺少工具名称", ErrResumeMismatch, item.WaitID)
		}
		item.Name = toolName
		resolvedItems = append(resolvedItems, item)
	}
	// The request digest intentionally reflects the caller's canonical input,
	// while the private payload must carry the durable tool names resolved from
	// the waiting boundary. This keeps retries with the same caller payload
	// idempotent even when Name was omitted, without handing ADK an unnamed
	// FunctionResponse after a restart.
	if len(resolvedItems) > 1 {
		encodedResponse, err = json.Marshal(resolvedItems)
		if err != nil {
			return Invocation{}, fmt.Errorf("%w: 恢复 response 无法编码: %v", ErrInvalidResume, err)
		}
		if len(encodedResponse) > maxInvocationResumeResponseBytes {
			return Invocation{}, fmt.Errorf("%w: response 总大小超过 %d 字节上限", ErrInvalidResume, maxInvocationResumeResponseBytes)
		}
	}
	resumeHandoff := InvocationResume{
		ID: newID("resume"), InvocationID: invocationID, WaitID: resolvedItems[0].WaitID, Name: resolvedItems[0].Name,
		ResponseJSON: encodedResponse, RequestDigest: fingerprint, CreatedAt: time.Now().UTC(),
	}
	resumeOutbox := InvocationResumeOutbox{
		ID: resumeHandoff.ID, InvocationID: invocationID, WaitID: resumeHandoff.WaitID, Name: resumeHandoff.Name,
		ResponseJSON: append([]byte(nil), resumeHandoff.ResponseJSON...), RequestDigest: resumeHandoff.RequestDigest,
		Status: InvocationResumeOutboxQueued, AvailableAt: resumeHandoff.CreatedAt, CreatedAt: resumeHandoff.CreatedAt, UpdatedAt: resumeHandoff.CreatedAt,
	}
	atomicCommitted := false
	if atomicRepo, ok := c.repo.(InvocationResumeCommitRepository); ok {
		committed, storedEvent, commitErr := atomicRepo.CommitInvocationResume(ctx, InvocationResumeCommit{
			InvocationID: invocationID, FromStatus: waitingStatus, ToStatus: InvocationQueued,
			Resume: resumeHandoff, Outbox: resumeOutbox,
			Event: AgentEvent{
				ID: newID("event"), InvocationID: invocationID, Type: EventInvocationResumed, Timestamp: time.Now().UTC(),
				Data: func() map[string]any {
					if boundaryKind == WorkflowBoundaryUser {
						return resumeUserInputEventDataWithStatusProof(resolvedItems, fingerprint, request, resolvedBoundaryID)
					}
					return resumeEventDataWithStatusProof(resolvedItems, fingerprint, request, resolvedBoundaryID)
				}(),
			},
		})
		if commitErr != nil {
			if !errors.Is(commitErr, ErrConflict) {
				return Invocation{}, commitErr
			}
			current, getErr := c.repo.GetInvocation(ctx, invocationID)
			if getErr != nil {
				return Invocation{}, getErr
			}
			matched, matchErr := c.hasResumeFingerprint(ctx, invocationID, fingerprint)
			if boundaryKind == WorkflowBoundaryUser {
				matched, matchErr = c.hasResumeFingerprintForReason(ctx, invocationID, fingerprint, "user")
			}
			if matchErr != nil {
				return Invocation{}, matchErr
			} else if matched {
				return current, nil
			}
			if matched, matchErr := c.pendingResumeMatches(ctx, invocationID, fingerprint); matchErr != nil {
				return Invocation{}, matchErr
			} else if matched {
				return current, nil
			}
			if current.Status == InvocationWaitingTool || current.Status == InvocationWaitingUser {
				return Invocation{}, fmt.Errorf("%w: invocation 已有不同的恢复请求", ErrResumeMismatch)
			}
			if current.Status.Terminal() {
				return Invocation{}, fmt.Errorf("%w: invocation 已结束", ErrConflict)
			}
			return Invocation{}, ErrConflict
		}
		invocation = committed
		c.publishStoredEvent(storedEvent)
		atomicCommitted = true
	}
	if !atomicCommitted {
		// Compatibility path for older embedders that implement Repository but
		// have not added the optional cross-entity commit operation yet.
		if err := c.saveInvocationResume(ctx, resumeHandoff); err != nil {
			return Invocation{}, err
		}
		if err := c.enqueueInvocationResumeOutbox(ctx, resumeHandoff); err != nil {
			return Invocation{}, err
		}

		transitioned, err := c.repo.TransitionInvocation(ctx, invocationID, waitingStatus, InvocationQueued, "")
		if err != nil {
			return Invocation{}, err
		}
		if !transitioned {
			current, getErr := c.repo.GetInvocation(ctx, invocationID)
			if getErr != nil {
				return Invocation{}, getErr
			}
			matched, matchErr := c.hasResumeFingerprint(ctx, invocationID, fingerprint)
			if boundaryKind == WorkflowBoundaryUser {
				matched, matchErr = c.hasResumeFingerprintForReason(ctx, invocationID, fingerprint, "user")
			}
			if matchErr != nil {
				return Invocation{}, matchErr
			} else if matched {
				return current, nil
			}
			if matched, matchErr := c.pendingResumeMatches(ctx, invocationID, fingerprint); matchErr != nil {
				return Invocation{}, matchErr
			} else if matched {
				return current, nil
			}
			// The CAS lost to cancellation or another accepted resume. Keep the
			// durable outbox: a concurrent process may already own the matching
			// delivery, and its digest makes the duplicate request idempotent. Only
			// remove the legacy one-row handoff when the current invocation is known
			// to be terminal.
			c.deleteInvocationResume(context.WithoutCancel(ctx), invocationID)
			if current.Status.Terminal() {
				c.deleteInvocationResumeOutbox(context.WithoutCancel(ctx), invocationID)
			}
			return Invocation{}, ErrConflict
		}

		// Waiting workers release their lease before returning, but clearing any
		// stale owner here makes a recovered task safe to resume after a crash.
		invocation.Status = InvocationQueued
		invocation.Error = ""
		invocation.LeaseOwner = ""
		invocation.LeaseExpiresAt = nil
		if err := c.repo.UpdateInvocation(ctx, invocation); err != nil {
			message := "恢复状态持久化失败: " + err.Error()
			_, _ = c.repo.TransitionInvocation(context.WithoutCancel(ctx), invocationID, InvocationQueued, InvocationFailed, message)
			c.deleteInvocationResume(context.WithoutCancel(ctx), invocationID)
			c.deleteInvocationResumeOutbox(context.WithoutCancel(ctx), invocationID)
			c.emitTerminal(context.WithoutCancel(ctx), invocationID, EventInvocationFailed, map[string]any{"error": message, "reason": "resume_persist_failed"})
			return Invocation{}, err
		}
		if _, err := c.appendAndPublish(ctx, AgentEvent{
			ID: newID("event"), InvocationID: invocationID, Type: EventInvocationResumed, Timestamp: time.Now().UTC(),
			Data: func() map[string]any {
				if boundaryKind == WorkflowBoundaryUser {
					return resumeUserInputEventDataWithStatusProof(resolvedItems, fingerprint, request, resolvedBoundaryID)
				}
				return resumeEventDataWithStatusProof(resolvedItems, fingerprint, request, resolvedBoundaryID)
			}(),
		}); err != nil {
			message := "恢复事件持久化失败: " + err.Error()
			_, _ = c.repo.TransitionInvocation(context.WithoutCancel(ctx), invocationID, InvocationQueued, InvocationFailed, message)
			c.deleteInvocationResume(context.WithoutCancel(ctx), invocationID)
			c.deleteInvocationResumeOutbox(context.WithoutCancel(ctx), invocationID)
			c.emitTerminal(context.WithoutCancel(ctx), invocationID, EventInvocationFailed, map[string]any{"error": message, "reason": "resume_event_persist_failed"})
			return Invocation{}, err
		}
	}
	c.refreshRuntimeSnapshot(context.WithoutCancel(ctx), invocationID)

	resume, err := invocationResumeContent(InvocationResume{WaitID: resolvedItems[0].WaitID, Name: resolvedItems[0].Name, ResponseJSON: encodedResponse})
	if err != nil {
		return Invocation{}, err
	}
	var workspaceID *string
	if strings.TrimSpace(invocation.WorkspaceID) != "" {
		value := invocation.WorkspaceID
		workspaceID = &value
	}
	c.launch(invocationID, agent.ChatRequest{
		UserID: invocation.UserID, BotID: invocation.BotID, ConversationID: invocation.ConversationID,
		SessionID: invocation.SessionID, ProviderID: invocation.ProviderID, ModelID: invocation.ModelID,
		WorkspaceID: workspaceID, TargetPath: invocation.TargetPath, ConfigSnapshot: invocation.ConfigSnapshot, Proactive: invocation.Proactive, ResumeContent: resume, Stream: true,
	})
	return invocation, nil
}

// invocationResumeFingerprint canonicalises only bounded structured input. It
// is stored as a digest in the event stream, never as a copy of the user's
// payload, so idempotency and audit remain useful without duplicating arbitrary
// data into Runtime metadata.
func invocationResumeFingerprint(request InvocationResumeRequest) (string, []byte, error) {
	request.BoundaryID = strings.TrimSpace(request.BoundaryID)
	items, err := normalizeInvocationResumeItems(request)
	if err != nil {
		return "", nil, err
	}
	var responseEncoded []byte
	if len(items) == 1 {
		responseEncoded, err = json.Marshal(items[0].Response)
	} else {
		responseEncoded, err = json.Marshal(items)
	}
	if err != nil {
		return "", nil, fmt.Errorf("%w: response 必须是 JSON 对象: %v", ErrInvalidResume, err)
	}
	if len(responseEncoded) > maxInvocationResumeResponseBytes {
		return "", nil, fmt.Errorf("%w: response 总大小超过 %d 字节上限", ErrInvalidResume, maxInvocationResumeResponseBytes)
	}
	var value any
	if len(items) == 1 {
		value = struct {
			WaitID         string         `json:"wait_id"`
			Name           string         `json:"name"`
			Response       map[string]any `json:"response"`
			BoundaryID     string         `json:"boundary_id,omitempty"`
			IdempotencyKey string         `json:"idempotency_key,omitempty"`
		}{WaitID: items[0].WaitID, Name: items[0].Name, Response: items[0].Response, BoundaryID: request.BoundaryID, IdempotencyKey: request.IdempotencyKey}
	} else {
		value = struct {
			Responses      []InvocationResumeItem `json:"responses"`
			BoundaryID     string                 `json:"boundary_id,omitempty"`
			IdempotencyKey string                 `json:"idempotency_key,omitempty"`
		}{Responses: items, BoundaryID: request.BoundaryID, IdempotencyKey: request.IdempotencyKey}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", nil, fmt.Errorf("%w: response 必须是 JSON 对象: %v", ErrInvalidResume, err)
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", digest[:]), responseEncoded, nil
}

func (c *Coordinator) hasResumeFingerprint(ctx context.Context, invocationID, fingerprint string) (bool, error) {
	return c.hasResumeFingerprintForReason(ctx, invocationID, fingerprint, "tool")
}

func (c *Coordinator) hasResumeFingerprintForReason(ctx context.Context, invocationID, fingerprint, reason string) (bool, error) {
	events, err := c.listAllInvocationEvents(ctx, invocationID, maxRuntimeEventReplay)
	if err != nil {
		return false, err
	}
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != EventInvocationResumed || event.Data == nil {
			continue
		}
		if eventReason, _ := event.Data["reason"].(string); strings.ToLower(strings.TrimSpace(eventReason)) != strings.ToLower(strings.TrimSpace(reason)) {
			continue
		}
		if digest, _ := event.Data["request_digest"].(string); digest == fingerprint {
			return true, nil
		}
	}
	return false, nil
}

// waitingToolBoundary returns the tool name for every still-pending long
// running call in the latest durable waiting boundary. Names come from the
// persisted tool.requested event; they are not trusted from a resume request.
func (c *Coordinator) waitingToolBoundary(ctx context.Context, invocationID string) (map[string]string, error) {
	events, err := c.listAllInvocationEvents(ctx, invocationID, maxRuntimeEventReplay)
	if err != nil {
		return nil, err
	}
	return waitingToolBoundaryFromEvents(events)
}

// waitingUserBoundary 从同一条 durable event stream 恢复当前用户问题，进程
// 重启后不依赖内存中的 UI 卡片。请求 ID 来自 ADK InterruptID，不能由调用方
// 在恢复请求中临时伪造。
func (c *Coordinator) waitingUserBoundary(ctx context.Context, invocationID string) (map[string]string, error) {
	events, err := c.listAllInvocationEvents(ctx, invocationID, maxRuntimeEventReplay)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string)
	pending := make(map[string]struct{})
	for _, event := range events {
		if event.Data == nil {
			continue
		}
		switch event.Type {
		case EventUserInputRequested:
			requestID := eventString(event.Data, "request_id", "wait_id", "id")
			if requestID != "" {
				names[requestID] = workflow.WorkflowInputFunctionCallName
			}
		case EventInvocationWaiting:
			if strings.ToLower(strings.TrimSpace(eventString(event.Data, "reason"))) != "user" {
				continue
			}
			pending = make(map[string]struct{})
			for _, id := range resumeStringList(event.Data["wait_ids"]) {
				pending[id] = struct{}{}
			}
			if id := eventString(event.Data, "wait_id", "request_id"); id != "" {
				pending[id] = struct{}{}
			}
		case EventUserInputResolved, EventInvocationResumed:
			reason := strings.ToLower(strings.TrimSpace(eventString(event.Data, "reason")))
			if event.Type == EventInvocationResumed && reason != "user" {
				continue
			}
			if id := eventString(event.Data, "wait_id", "request_id"); id != "" {
				delete(pending, id)
			}
			for _, id := range resumeStringList(event.Data["wait_ids"]) {
				delete(pending, id)
			}
		}
	}
	result := make(map[string]string, len(pending))
	for id := range pending {
		name := names[id]
		if name == "" {
			name = workflow.WorkflowInputFunctionCallName
		}
		result[id] = name
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("%w: 当前没有待回答的问题", ErrResumeMismatch)
	}
	return result, nil
}

func waitingToolBoundaryFromEvents(events []AgentEvent) (map[string]string, error) {
	names := make(map[string]string)
	pending := make(map[string]struct{})
	for _, event := range events {
		if event.Data == nil {
			continue
		}
		switch event.Type {
		case EventToolRequested:
			callID, _ := event.Data["call_id"].(string)
			name, _ := event.Data["name"].(string)
			if callID = strings.TrimSpace(callID); callID != "" {
				names[callID] = strings.TrimSpace(name)
			}
		case EventInvocationWaiting:
			reason, _ := event.Data["reason"].(string)
			if reason != "tool" {
				continue
			}
			pending = make(map[string]struct{})
			for _, id := range resumeStringList(event.Data["tool_call_ids"]) {
				pending[id] = struct{}{}
			}
		case EventInvocationResumed:
			reason, _ := event.Data["reason"].(string)
			if reason == "tool" {
				if waitID, _ := event.Data["wait_id"].(string); strings.TrimSpace(waitID) != "" {
					delete(pending, strings.TrimSpace(waitID))
				}
				for _, waitID := range resumeStringList(event.Data["wait_ids"]) {
					delete(pending, waitID)
				}
			}
		}
	}
	result := make(map[string]string, len(pending))
	for id := range pending {
		result[id] = names[id]
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("%w: 当前没有可恢复的长运行工具", ErrResumeMismatch)
	}
	return result, nil
}

func workflowBoundaryWaitSet(boundary WorkflowBoundary) map[string]struct{} {
	result := make(map[string]struct{}, len(boundary.PendingWaitIDs))
	for _, id := range boundary.PendingWaitIDs {
		result[strings.TrimSpace(id)] = struct{}{}
	}
	return result
}

func workflowWaitSetEqual(left map[string]struct{}, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for _, id := range right {
		if _, ok := left[strings.TrimSpace(id)]; !ok {
			return false
		}
	}
	return true
}

// resolveWorkflowToolBoundary binds a resume to the state-machine boundary
// reconstructed from the same event stream used by waitingToolBoundary. An
// explicit BoundaryID is accepted only when it is the current ADK waiting
// set; this prevents a caller from resuming an older branch whose side effect
// may no longer be represented by the live Session.
func (c *Coordinator) resolveWorkflowToolBoundary(ctx context.Context, invocation Invocation, boundaryID string) (map[string]string, string, error) {
	events, err := c.listAllInvocationEvents(ctx, invocation.ID, maxRuntimeEventReplay)
	if err != nil {
		return nil, "", err
	}
	names, err := waitingToolBoundaryFromEvents(events)
	if err != nil {
		return nil, "", err
	}
	checkpoint := deriveWorkflowCheckpoint(invocation, nil, events)
	if boundaryID != "" {
		boundary, found := func() (WorkflowBoundary, bool) {
			for _, candidate := range checkpoint.Boundaries {
				if candidate.ID == boundaryID {
					return candidate, true
				}
			}
			return WorkflowBoundary{}, false
		}()
		if !found || boundary.Kind != WorkflowBoundaryTool || boundary.Status != WorkflowBoundaryWaiting {
			return nil, "", fmt.Errorf("%w: boundary_id=%s 不属于当前可恢复工具边界", ErrResumeMismatch, boundaryID)
		}
		if !workflowWaitSetEqual(workflowBoundaryWaitSet(boundary), mapKeys(names)) {
			return nil, "", fmt.Errorf("%w: boundary_id=%s 不是当前 ADK 等待边界", ErrResumeMismatch, boundaryID)
		}
		return names, boundary.ID, nil
	}
	for index := len(checkpoint.Boundaries) - 1; index >= 0; index-- {
		boundary := checkpoint.Boundaries[index]
		if boundary.Kind != WorkflowBoundaryTool || boundary.Status != WorkflowBoundaryWaiting {
			continue
		}
		if workflowWaitSetEqual(workflowBoundaryWaitSet(boundary), mapKeys(names)) {
			return names, boundary.ID, nil
		}
	}
	return names, "", nil
}

// resolveWorkflowUserBoundary 将用户问题绑定到当前 workflow user boundary，
// 与工具恢复使用同样的 boundary CAS 约束，防止回答旧问题或其他分支。
func (c *Coordinator) resolveWorkflowUserBoundary(ctx context.Context, invocation Invocation, boundaryID string) (map[string]string, string, error) {
	names, err := c.waitingUserBoundary(ctx, invocation.ID)
	if err != nil {
		return nil, "", err
	}
	events, err := c.listAllInvocationEvents(ctx, invocation.ID, maxRuntimeEventReplay)
	if err != nil {
		return nil, "", err
	}
	checkpoint := deriveWorkflowCheckpoint(invocation, nil, events)
	if boundaryID != "" {
		for _, boundary := range checkpoint.Boundaries {
			if boundary.ID != boundaryID {
				continue
			}
			if boundary.Kind != WorkflowBoundaryUser || boundary.Status != WorkflowBoundaryWaiting || !workflowWaitSetEqual(workflowBoundaryWaitSet(boundary), mapKeys(names)) {
				return nil, "", fmt.Errorf("%w: boundary_id=%s 不属于当前用户问题边界", ErrResumeMismatch, boundaryID)
			}
			return names, boundary.ID, nil
		}
		return nil, "", fmt.Errorf("%w: boundary_id=%s 不存在", ErrResumeMismatch, boundaryID)
	}
	for index := len(checkpoint.Boundaries) - 1; index >= 0; index-- {
		boundary := checkpoint.Boundaries[index]
		if boundary.Kind == WorkflowBoundaryUser && boundary.Status == WorkflowBoundaryWaiting && workflowWaitSetEqual(workflowBoundaryWaitSet(boundary), mapKeys(names)) {
			return names, boundary.ID, nil
		}
	}
	return names, "", nil
}

func mapKeys(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func resumeStringList(value any) []string {
	var raw []string
	switch values := value.(type) {
	case []string:
		raw = values
	case []any:
		for _, item := range values {
			if value, ok := item.(string); ok {
				raw = append(raw, value)
			}
		}
	}
	result := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, value := range raw {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func (c *Coordinator) ensureStarted() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrConflict
	}
	if !c.started || c.rootCtx == nil {
		return ErrNotStarted
	}
	return nil
}

func (c *Coordinator) launch(id string, request agent.ChatRequest) {
	request.InvocationID = strings.TrimSpace(id)
	c.mu.Lock()
	// A coordinator built by a storage-only embedder may expose durable
	// continuation creation without an execution kernel. Persist the child in
	// queued state and let a fully assembled worker claim it later; never start
	// a goroutine that would panic on a nil kernel.
	if c.closed || c.rootCtx == nil || c.kernel == nil {
		c.mu.Unlock()
		return
	}
	if _, exists := c.runs[request.InvocationID]; exists {
		c.mu.Unlock()
		return
	}
	if c.runs == nil {
		c.runs = make(map[string]runHandle)
	}
	ctx, cancel := context.WithCancel(c.rootCtx)
	c.runSequence++
	generation := c.runSequence
	c.runs[id] = runHandle{cancel: cancel, generation: generation}
	c.mu.Unlock()
	go func() {
		defer func() {
			c.mu.Lock()
			if current, ok := c.runs[id]; ok && current.generation == generation {
				delete(c.runs, id)
			}
			c.mu.Unlock()
		}()
		c.run(ctx, id, request)
	}()
}

func (c *Coordinator) run(ctx context.Context, id string, request agent.ChatRequest) {
	baseCtx := ctx
	leaseOwner := newID("worker")
	leaseLost := atomic.Bool{}
	var leaseRepo InvocationLeaseRepository
	releaseLease := func() {}
	if candidate, ok := c.repo.(InvocationLeaseRepository); ok {
		leaseRepo = candidate
		acquired, err := leaseRepo.AcquireInvocationLease(ctx, id, leaseOwner, time.Now().UTC(), InvocationLeaseTTL)
		if err != nil || !acquired {
			return
		}
		leaseCtx, cancelLease := context.WithCancel(ctx)
		ctx = leaseCtx
		go func() {
			ticker := time.NewTicker(InvocationLeaseInterval)
			defer ticker.Stop()
			for {
				select {
				case <-leaseCtx.Done():
					return
				case now := <-ticker.C:
					ok, renewErr := leaseRepo.RenewInvocationLease(context.WithoutCancel(baseCtx), id, leaseOwner, now.UTC(), InvocationLeaseTTL)
					if renewErr != nil || !ok {
						leaseLost.Store(true)
						cancelLease()
						return
					}
				}
			}
		}()
		releaseLease = func() {
			cancelLease()
			_, _ = leaseRepo.ReleaseInvocationLease(context.WithoutCancel(baseCtx), id, leaseOwner)
		}
		defer releaseLease()
	}
	resumeOutbox, resumeOutboxClaimed, resumeOutboxErr := c.claimInvocationResumeOutbox(ctx, id, leaseOwner)
	if resumeOutboxErr != nil {
		// Keep the Invocation queued when the delivery store is temporarily
		// unavailable. A later process restart can retry the same durable item;
		// executing without a claimed handoff would risk losing the response.
		return
	}
	if resumeOutboxClaimed && request.ResumeContent == nil {
		resume, resumeErr := invocationResumeContent(InvocationResume{
			InvocationID: resumeOutbox.InvocationID, WaitID: resumeOutbox.WaitID, Name: resumeOutbox.Name, ResponseJSON: resumeOutbox.ResponseJSON,
		})
		if resumeErr != nil {
			c.retryInvocationResumeOutbox(context.WithoutCancel(baseCtx), id, leaseOwner, resumeErr.Error())
			return
		}
		request.ResumeContent = resume
		request.Message = ""
		request.Attachments = nil
	}
	if ok, err := c.repo.TransitionInvocation(ctx, id, InvocationQueued, InvocationRunning, ""); err != nil || !ok {
		if resumeOutboxClaimed {
			c.retryInvocationResumeOutbox(context.WithoutCancel(baseCtx), id, leaseOwner, "queued -> running 状态迁移失败")
		}
		return
	}
	if resumeOutboxClaimed {
		// The worker now owns the Invocation's running lease and the ADK request
		// is about to be submitted. A later restart must not replay this private
		// response after the running boundary has become non-replayable.
		c.completeInvocationResumeOutbox(context.WithoutCancel(baseCtx), id, leaseOwner)
	}
	if request.ResumeContent != nil {
		// The handoff is no longer needed once this worker owns the running
		// lease. If the process dies before this point, Start() still has the
		// durable response and can rebuild the exact FunctionResponse.
		c.deleteInvocationResume(context.WithoutCancel(baseCtx), id)
	}
	started := time.Now().UTC()
	if invocation, err := c.repo.GetInvocation(ctx, id); err == nil {
		invocation.StartedAt = &started
		_ = c.repo.UpdateInvocation(ctx, invocation)
	}
	_, _ = c.appendAndPublish(ctx, AgentEvent{ID: newID("event"), InvocationID: id, Type: EventInvocationStarted, Timestamp: started, Data: map[string]any{"session_id": request.SessionID}})
	c.refreshRuntimeSnapshot(ctx, id)

	waitingApproval := false
	waitingTool := false
	waitingUser := false
	capabilityEvidenceSeen := make(map[string]struct{})
	kernelCtx := workspace.WithInvocationID(ctx, id)
	kernelCtx = workspace.WithUserID(kernelCtx, request.UserID)
	kernelCtx = workspace.WithCommandObserver(kernelCtx, func(chunk workspace.CommandChunk) {
		_, _ = c.appendAndPublish(context.WithoutCancel(ctx), AgentEvent{
			ID: newID("event"), InvocationID: id, Type: EventCommandOutput,
			Timestamp: chunk.Timestamp, Data: map[string]any{
				"command_run_id": chunk.CommandRunID, "sequence": chunk.Sequence, "offset": chunk.Offset,
				"stream": chunk.Stream, "data": sanitizeRuntimeString(chunk.Data), "truncated": chunk.Truncated,
			},
		})
	})
	for event, runErr := range c.kernel.Run(kernelCtx, request) {
		if runErr != nil {
			if leaseLost.Load() {
				c.finish(context.WithoutCancel(baseCtx), id, InvocationFailed, "runtime_lease_lost: 执行租约丢失，无法确认上次执行是否产生副作用")
				return
			}
			if ctx.Err() != nil {
				c.finish(context.WithoutCancel(baseCtx), id, InvocationCancelled, ctx.Err().Error())
				return
			}
			var budgetErr *agent.ContextBudgetError
			if errors.As(runErr, &budgetErr) {
				_, _ = c.appendAndPublish(context.WithoutCancel(ctx), AgentEvent{ID: newID("event"), InvocationID: id, Type: EventRuntimeNotice, Timestamp: time.Now().UTC(), Data: map[string]any{"code": "context_budget_exhausted", "model": budgetErr.Model, "context_window": budgetErr.ContextWindow, "estimated_input": budgetErr.Estimated, "output_reserve": budgetErr.OutputReserve, "safety_reserve": budgetErr.SafetyReserve}})
			}
			c.recordRuntimeCapabilityEvidence(context.WithoutCancel(ctx), id, runtimeCapabilityEvidenceFromError(runErr, request), capabilityEvidenceSeen)
			c.finish(ctx, id, InvocationFailed, runErr.Error())
			return
		}
		if event == nil {
			continue
		}
		mapped, approvalData := MapSessionEvent(id, event)
		mapped, planErr := c.applyMappedPlanEvents(ctx, id, mapped)
		if planErr != nil {
			c.finish(ctx, id, InvocationFailed, planErr.Error())
			return
		}
		// A structured model plan is a durable proposal, not an instruction to
		// execute side effects. Once it is accepted, advance only the first
		// dependency-ready step so snapshots and approval/verification bridges
		// have an explicit active boundary. The actual tool remains owned by
		// ADK/Workspace policy and is never invoked by this transition.
		if err := c.ensurePlanStepStarted(ctx, id, "runtime plan step admission"); err != nil {
			c.finish(ctx, id, InvocationFailed, err.Error())
			return
		}
		if len(approvalData) > 1 {
			// A single Invocation currently has one approval resume boundary. Do
			// not expose several independently resolvable cards that could race
			// the same ADK session and produce parallel side effects.
			c.finish(ctx, id, InvocationFailed, "同一轮暂不支持多个并行审批")
			return
		}
		if len(approvalData) > 0 {
			approvalEvidence := make([]provider.CapabilityObservation, 0, len(approvalData))
			approvalRoute := ""
			if len(mapped) > 0 {
				approvalRoute, _ = mapped[0].Data["model_route"].(string)
			}
			for range approvalData {
				approvalEvidence = append(approvalEvidence, provider.CapabilityObservation{Feature: "tool_calling", State: provider.SupportSupported, Confidence: 0.95, Route: approvalRoute, Reason: "runtime tool confirmation request observed"})
			}
			c.recordRuntimeCapabilityEvidence(context.WithoutCancel(ctx), id, approvalEvidence, capabilityEvidenceSeen)
			if err := c.persistApprovalBoundary(ctx, id, mapped, approvalData); err != nil {
				c.finish(ctx, id, InvocationFailed, err.Error())
				return
			}
			waitingApproval = true
			c.refreshRuntimeSnapshot(ctx, id)
		} else {
			for index := range mapped {
				item := &mapped[index]
				if err := c.recordToolCallEvent(ctx, id, item); err != nil {
					c.finish(ctx, id, InvocationFailed, err.Error())
					return
				}
				if item.Type == EventInvocationWaiting {
					if eventString(item.Data, "reason") == "user" {
						waitingUser = true
					} else {
						waitingTool = true
					}
				}
				if _, err := c.appendAndPublish(ctx, *item); err != nil {
					c.finish(ctx, id, InvocationFailed, err.Error())
					return
				}
				c.recordWorkflowToolEvidence(ctx, id, *item)
				c.recordRuntimeCapabilityEvidence(context.WithoutCancel(ctx), id, runtimeCapabilityEvidenceFromEvent(*item), capabilityEvidenceSeen)
				if item.Type == EventUsageUpdated && usageEventScope(*item) == "" {
					c.recordManifestUsage(context.WithoutCancel(ctx), id, item.Data)
				}
			}
		}
	}
	if waitingApproval {
		// Release before returning so an immediate approval decision can launch
		// the queued resume without racing the previous worker's defer.
		releaseLease()
		return
	}
	if waitingTool {
		if ok, err := c.repo.TransitionInvocation(ctx, id, InvocationRunning, InvocationWaitingTool, ""); err != nil || !ok {
			if err != nil {
				c.finish(ctx, id, InvocationFailed, err.Error())
			}
		} else {
			c.refreshRuntimeSnapshot(ctx, id)
		}
		releaseLease()
		return
	}
	if waitingUser {
		if ok, err := c.repo.TransitionInvocation(ctx, id, InvocationRunning, InvocationWaitingUser, ""); err != nil || !ok {
			if err != nil {
				c.finish(ctx, id, InvocationFailed, err.Error())
			}
		} else {
			c.refreshRuntimeSnapshot(ctx, id)
		}
		releaseLease()
		return
	}
	if ctx.Err() != nil {
		if leaseLost.Load() {
			c.finish(context.WithoutCancel(baseCtx), id, InvocationFailed, "runtime_lease_lost: 执行租约丢失，无法确认上次执行是否产生副作用")
			return
		}
		c.finish(context.WithoutCancel(baseCtx), id, InvocationCancelled, ctx.Err().Error())
		return
	}
	c.finish(ctx, id, InvocationCompleted, "")
}

// persistApprovalBoundary serializes the small hand-off between the ADK
// confirmation event and an HTTP approval decision. The decision lock prevents
// a client that receives approval.requested immediately from racing the
// producer before waiting_approval is durable.
func (c *Coordinator) persistApprovalBoundary(ctx context.Context, id string, mapped []AgentEvent, approvalData []approvalData) error {
	c.approvalMu.Lock()
	defer c.approvalMu.Unlock()
	toolRepo, hasToolRepo := c.repo.(ToolCallRepository)
	approvalIDs := make(map[string]string, len(approvalData))
	toolCallIDs := make(map[string]string, len(approvalData))
	for _, data := range approvalData {
		createdAt := time.Now().UTC()
		c.mu.Lock()
		ttl := c.approvalTTL
		c.mu.Unlock()
		var expiresAt *time.Time
		if ttl > 0 {
			deadline := createdAt.Add(ttl)
			expiresAt = &deadline
		}
		approval := Approval{
			ID: newID("approval"), InvocationID: id, OperationID: data.OperationID, ToolName: data.ToolName,
			OriginalCallID: data.OriginalCallID, ConfirmationCallID: data.ConfirmationCallID,
			Args: data.Args, Hint: data.Hint, Choices: normalizeApprovalChoices(data.Choices), Status: ApprovalPending, ExpiresAt: expiresAt,
			CreatedAt: createdAt, UpdatedAt: createdAt,
		}
		if contractRepo, ok := c.repo.(TaskContractRepository); ok {
			if contract, contractErr := contractRepo.GetTaskContract(ctx, id); contractErr == nil {
				approval.TaskContractVersion = contract.Version
			} else if !errors.Is(contractErr, ErrNotFound) {
				return contractErr
			}
		}
		if invocation, err := c.repo.GetInvocation(ctx, id); err == nil {
			approval.ConversationID = invocation.ConversationID
		}
		if hasToolRepo {
			// ADK may expose the original tool call and the confirmation call as
			// two adjacent events. Reuse the original durable ToolCall instead of
			// creating a second audit record for the same model-issued call.
			var toolCall *ToolCall
			if existing, listErr := toolRepo.ListToolCalls(ctx, id); listErr != nil {
				return listErr
			} else {
				for index := range existing {
					if existing[index].OriginalCallID == data.OriginalCallID {
						candidate := existing[index]
						toolCall = &candidate
						break
					}
				}
			}
			if toolCall == nil {
				created := ToolCall{ID: newID("toolcall"), InvocationID: id, ConversationID: approval.ConversationID, ToolName: data.ToolName, OriginalCallID: data.OriginalCallID, ConfirmationCallID: data.ConfirmationCallID, OperationID: data.OperationID, Args: data.Args, Status: ToolCallWaitingApproval, CreatedAt: createdAt, UpdatedAt: createdAt}
				if err := toolRepo.CreateToolCall(ctx, created); err != nil {
					return err
				}
				toolCall = &created
			} else {
				if toolCall.ConversationID == "" {
					toolCall.ConversationID = approval.ConversationID
				}
				toolCall.ToolName = data.ToolName
				toolCall.ConfirmationCallID = data.ConfirmationCallID
				toolCall.OperationID = data.OperationID
				toolCall.Args = data.Args
				if !toolCall.Status.Terminal() {
					toolCall.Status = ToolCallWaitingApproval
				}
				toolCall.UpdatedAt = createdAt
				if err := toolRepo.UpdateToolCall(ctx, *toolCall); err != nil {
					return err
				}
			}
			toolCallIDs[data.ConfirmationCallID] = toolCall.ID
			toolCallIDs[data.OriginalCallID] = toolCall.ID
			approval.ToolCallID = toolCall.ID
		}
		if err := c.repo.CreateApproval(ctx, approval); err != nil {
			return err
		}
		approvalIDs[data.ConfirmationCallID] = approval.ID
		toolCallID := toolCallIDs[data.ConfirmationCallID]
		if hasToolRepo && toolCallID != "" {
			if toolCall, toolErr := toolRepo.GetToolCall(ctx, toolCallID); toolErr == nil {
				toolCall.ApprovalID = approval.ID
				if err := toolRepo.UpdateToolCall(ctx, toolCall); err != nil {
					return err
				}
			}
		}
		if invocation, err := c.repo.GetInvocation(ctx, id); err == nil {
			invocation.ActiveApprovalID = approval.ID
			if err := c.repo.UpdateInvocation(ctx, invocation); err != nil {
				return err
			}
		}
	}
	for _, item := range mapped {
		if originalCallID, ok := item.Data["original_call_id"].(string); ok {
			if toolCallID := toolCallIDs[originalCallID]; toolCallID != "" {
				item.Data["tool_call_id"] = toolCallID
			}
		}
		if callID, ok := item.Data["call_id"].(string); ok {
			if toolCallID := toolCallIDs[callID]; toolCallID != "" {
				item.Data["tool_call_id"] = toolCallID
			}
		}
		if item.Type == EventApprovalRequested {
			if callID, ok := item.Data["approval_call_id"].(string); ok {
				if approvalID := approvalIDs[callID]; approvalID != "" {
					data := make(map[string]any, len(item.Data)+2)
					for key, value := range item.Data {
						data[key] = value
					}
					data["approval_id"] = approvalID
					if toolCallID := toolCallIDs[callID]; toolCallID != "" {
						data["tool_call_id"] = toolCallID
					}
					if approval, approvalErr := c.repo.GetApproval(ctx, approvalID); approvalErr == nil {
						if approval.ExpiresAt != nil {
							data["expires_at"] = approval.ExpiresAt
						}
						data["choices"] = approval.Choices
					}
					item.Data = data
				}
			}
		}
		if _, err := c.appendAndPublish(ctx, item); err != nil {
			return err
		}
	}
	if ok, err := c.repo.TransitionInvocation(ctx, id, InvocationRunning, InvocationWaitingApproval, ""); err != nil {
		return err
	} else if !ok {
		return ErrConflict
	}
	_, err := c.appendAndPublish(ctx, AgentEvent{ID: newID("event"), InvocationID: id, Type: EventInvocationWaiting, Timestamp: time.Now().UTC(), Data: map[string]any{"reason": "approval"}})
	return err
}

func (c *Coordinator) recordToolCallEvent(ctx context.Context, invocationID string, event *AgentEvent) error {
	if event == nil {
		return nil
	}
	if event.Type == EventToolRequested && event.Data != nil {
		// Keep the external status query identity available even for legacy
		// repositories that do not implement ToolCallRepository. The digest is
		// derived before adding local audit IDs and is safe to persist in the
		// metadata-only event projection.
		requestDigest, digestErr := runtimeLongRunningToolRequestDigest(invocationID, event.Data)
		if digestErr != nil {
			return digestErr
		}
		event.Data["request_digest"] = requestDigest
	}
	repo, ok := c.repo.(ToolCallRepository)
	if !ok || event.Data == nil {
		return nil
	}
	callID, _ := event.Data["call_id"].(string)
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return nil
	}
	items, err := repo.ListToolCalls(ctx, invocationID)
	if err != nil {
		return err
	}
	var current *ToolCall
	for index := range items {
		if items[index].OriginalCallID == callID {
			current = &items[index]
			break
		}
	}
	if current == nil && event.Type == EventToolRequested {
		now := time.Now().UTC()
		name, _ := event.Data["name"].(string)
		args, _ := event.Data["args"].(map[string]any)
		conversationID := ""
		if invocation, getErr := c.repo.GetInvocation(ctx, invocationID); getErr == nil {
			conversationID = invocation.ConversationID
		}
		created := ToolCall{ID: newID("toolcall"), InvocationID: invocationID, ConversationID: conversationID, ToolName: name, OriginalCallID: callID, Args: args, Status: ToolCallRequested, CreatedAt: now, UpdatedAt: now}
		if err := repo.CreateToolCall(ctx, created); err != nil {
			return err
		}
		current = &created
	}
	if current == nil {
		return nil
	}
	event.Data["tool_call_id"] = current.ID
	if current.Status == ToolCallRejected || current.Status == ToolCallExpired || current.Status == ToolCallCancelled {
		return nil
	}
	status := current.Status
	switch event.Type {
	case EventToolStarted:
		status = ToolCallRunning
	case EventToolCompleted:
		status = ToolCallCompleted
	case EventToolFailed:
		status = ToolCallFailed
	}
	if status != current.Status || event.Type == EventToolFailed {
		current.Status = status
		if value, ok := event.Data["error"].(string); ok {
			current.Error = value
		}
		return repo.UpdateToolCall(ctx, *current)
	}
	return nil
}

const maxModelPlanPayloadBytes = 256 * 1024

func (c *Coordinator) applyMappedPlanEvents(ctx context.Context, invocationID string, mapped []AgentEvent) ([]AgentEvent, error) {
	if len(mapped) == 0 {
		return mapped, nil
	}
	filtered := make([]AgentEvent, 0, len(mapped))
	for _, event := range mapped {
		if event.Type != EventPlanUpdated {
			filtered = append(filtered, event)
			continue
		}
		applied, err := c.applyModelPlanEvent(ctx, invocationID, event.Data)
		if err != nil {
			return nil, err
		}
		// A structured plan is persisted through UpdateTaskPlan, which emits
		// the authoritative plan.updated event. Avoid appending a second,
		// model-shaped event for the same change; non-structured observations
		// remain visible to clients for compatibility.
		if !applied {
			filtered = append(filtered, event)
		}
	}
	return filtered, nil
}

// applyModelPlanEvent promotes only an explicitly structured plan object from
// an ADK output. Legacy/diagnostic values such as a string or []string remain
// ordinary plan.updated observations and are not allowed to replace durable
// state. Structured objects are normalized against the current plan and pass
// the same dependency/CAS validation as HTTP updates.
func (c *Coordinator) applyModelPlanEvent(ctx context.Context, invocationID string, data map[string]any) (bool, error) {
	if c == nil || data == nil {
		return false, nil
	}
	raw, exists := data["plan"]
	if !exists || raw == nil {
		return false, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return false, fmt.Errorf("解析模型计划失败: %w", err)
	}
	if len(encoded) > maxModelPlanPayloadBytes {
		return false, fmt.Errorf("%w: 模型计划超过 %d 字节", ErrInvalidPlan, maxModelPlanPayloadBytes)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil || object == nil {
		// Keep old non-object plan observations visible without treating them
		// as executable durable state.
		return false, nil
	}
	stepsRaw, hasSteps := object["steps"]
	if !hasSteps {
		return false, nil
	}
	var candidate struct {
		ID            string     `json:"id"`
		InvocationID  string     `json:"invocation_id"`
		Revision      int64      `json:"revision"`
		Status        PlanStatus `json:"status"`
		CurrentStepID string     `json:"current_step_id"`
		Blocker       string     `json:"blocker"`
		Steps         []PlanStep `json:"steps"`
		CreatedAt     time.Time  `json:"created_at"`
	}
	if err := json.Unmarshal(encoded, &candidate); err != nil {
		return false, fmt.Errorf("%w: 模型计划结构无效: %v", ErrInvalidPlan, err)
	}
	if len(bytes.TrimSpace(stepsRaw)) == 0 || len(candidate.Steps) == 0 {
		// An empty plan is useful as a model diagnostic but does not replace a
		// previously durable plan or create a meaningless completed state.
		return false, nil
	}
	current, err := c.GetTaskPlan(ctx, invocationID)
	if err != nil {
		return false, err
	}
	previous := make(map[string]PlanStep, len(current.Steps))
	for _, step := range current.Steps {
		previous[step.ID] = step
	}
	present := make(map[string]struct{}, len(candidate.Steps))
	for index := range candidate.Steps {
		step := &candidate.Steps[index]
		step.ID = strings.TrimSpace(step.ID)
		step.Title = sanitizeRuntimeString(strings.TrimSpace(step.Title))
		step.Description = sanitizeRuntimeString(strings.TrimSpace(step.Description))
		step.Blocker = sanitizeRuntimeString(strings.TrimSpace(step.Blocker))
		step.LastFailureCode = sanitizeRuntimeString(strings.TrimSpace(step.LastFailureCode))
		step.LastFailure = sanitizeRuntimeString(strings.TrimSpace(step.LastFailure))
		for dependencyIndex := range step.DependsOn {
			step.DependsOn[dependencyIndex] = strings.TrimSpace(step.DependsOn[dependencyIndex])
		}
		for evidenceIndex := range step.Evidence {
			step.Evidence[evidenceIndex].Kind = sanitizeRuntimeString(strings.TrimSpace(step.Evidence[evidenceIndex].Kind))
			step.Evidence[evidenceIndex].Ref = sanitizeRuntimeString(strings.TrimSpace(step.Evidence[evidenceIndex].Ref))
			step.Evidence[evidenceIndex].Summary = sanitizeRuntimeString(strings.TrimSpace(step.Evidence[evidenceIndex].Summary))
		}
		present[step.ID] = struct{}{}
		if old, ok := previous[strings.TrimSpace(step.ID)]; ok {
			// Omitted status/title/evidence fields mean “retain known durable
			// state”; an explicit pending status can still request a repair.
			if step.Status == "" {
				step.Status = old.Status
			}
			if strings.TrimSpace(step.Title) == "" {
				step.Title = old.Title
			}
			if step.DependsOn == nil {
				step.DependsOn = append([]string(nil), old.DependsOn...)
			}
			if step.Status == PlanStepCompleted && len(step.Evidence) == 0 {
				step.Evidence = append([]EvidenceRef(nil), old.Evidence...)
			}
			if step.RepairAttempts == 0 && old.RepairAttempts > 0 {
				step.RepairAttempts = old.RepairAttempts
			}
		}
	}
	for _, old := range current.Steps {
		if old.Status != PlanStepCompleted && old.Status != PlanStepInProgress {
			continue
		}
		if _, ok := present[old.ID]; ok {
			continue
		}
		if old.Status == PlanStepInProgress {
			return false, fmt.Errorf("%w: 模型计划不能删除执行中的步骤 %q", ErrInvalidPlan, old.ID)
		}
		// Completed evidence is immutable; retain it even if the model sends
		// a shortened replacement plan.
		candidate.Steps = append(candidate.Steps, old)
	}
	candidate.ID = current.ID
	candidate.InvocationID = invocationID
	candidate.CurrentStepID = strings.TrimSpace(candidate.CurrentStepID)
	candidate.Blocker = sanitizeRuntimeString(strings.TrimSpace(candidate.Blocker))
	candidate.Revision = current.Revision
	candidate.CreatedAt = current.CreatedAt
	proposed := TaskPlan{ID: candidate.ID, InvocationID: candidate.InvocationID, Revision: candidate.Revision, Status: candidate.Status, CurrentStepID: candidate.CurrentStepID, Blocker: candidate.Blocker, Steps: candidate.Steps, CreatedAt: candidate.CreatedAt}
	proposed, err = proposed.Normalize(time.Now().UTC())
	if err != nil {
		return false, err
	}
	if plansSemanticallyEqual(current, proposed) {
		return true, nil
	}
	_, err = c.UpdateTaskPlan(ctx, invocationID, proposed, "model structured plan")
	return true, err
}

func plansSemanticallyEqual(left, right TaskPlan) bool {
	// TaskPlan values are commonly read from a shared idempotency payload. Copy
	// the backing step slices before clearing volatile timestamps; mutating a
	// caller-owned slice here would race concurrent SQLite idempotency checks.
	left.Steps = append([]PlanStep(nil), left.Steps...)
	right.Steps = append([]PlanStep(nil), right.Steps...)
	left.ID, left.InvocationID, left.Revision, left.CreatedAt, left.UpdatedAt = "", "", 0, time.Time{}, time.Time{}
	right.ID, right.InvocationID, right.Revision, right.CreatedAt, right.UpdatedAt = "", "", 0, time.Time{}, time.Time{}
	for index := range left.Steps {
		left.Steps[index].UpdatedAt = time.Time{}
	}
	for index := range right.Steps {
		right.Steps[index].UpdatedAt = time.Time{}
	}
	return reflect.DeepEqual(left, right)
}

// PlansSemanticallyEqual exposes the immutable plan-shape comparison to
// storage adapters. Runtime-owned identifiers, revisions and timestamps are
// intentionally ignored; step state, dependencies, blockers and evidence
// remain part of the comparison.
func PlansSemanticallyEqual(left, right TaskPlan) bool {
	return plansSemanticallyEqual(left, right)
}

func (c *Coordinator) updateToolCallStatus(ctx context.Context, toolCallID string, status ToolCallStatus, message string) {
	if strings.TrimSpace(toolCallID) == "" {
		return
	}
	repo, ok := c.repo.(ToolCallRepository)
	if !ok {
		return
	}
	item, err := repo.GetToolCall(ctx, toolCallID)
	if err != nil {
		return
	}
	item.Status = status
	if message != "" {
		item.Error = message
	}
	_ = repo.UpdateToolCall(ctx, item)
}

// recordManifestUsage backfills the most recent metadata manifest with the
// provider's prompt-token count when ADK exposes it. Missing or malformed
// usage is intentionally left unknown; it must never be coerced to zero.
func (c *Coordinator) recordManifestUsage(ctx context.Context, invocationID string, data map[string]any) {
	manifestRepo, ok := c.repo.(ContextManifestRepository)
	if !ok || data == nil || usageEventScope(AgentEvent{Data: data}) != "" {
		return
	}
	items, err := manifestRepo.ListContextManifests(ctx, invocationID)
	if err != nil || len(items) == 0 {
		return
	}
	actual, ok := promptTokenCount(eventUsagePayload(data))
	if !ok {
		return
	}
	modelCallID, _ := data["model_call_id"].(string)
	modelCallID = strings.TrimSpace(modelCallID)
	index := -1
	if modelCallID != "" {
		for candidate := range items {
			if items[candidate].ModelCallID == modelCallID {
				index = candidate
				break
			}
		}
	} else {
		// Older ADK/provider events may not carry the correlation metadata. In
		// that compatibility path, attach usage to the newest manifest without
		// an observation first; never overwrite a known sample by guessing.
		for candidate := len(items) - 1; candidate >= 0; candidate-- {
			if items[candidate].ActualInput == nil {
				index = candidate
				break
			}
		}
	}
	if index < 0 {
		return
	}
	estimated := items[index].EstimatedInput
	items[index].ActualInput = &actual
	if err := manifestRepo.SaveContextManifest(ctx, items[index]); err != nil {
		return
	}
	// Only feed the model-level calibrator when the runtime supplied an exact
	// model-call correlation. The legacy “newest unobserved manifest” fallback
	// remains useful for display, but is too ambiguous to tune a provider
	// estimator safely.
	if modelCallID == "" || c.kernel == nil {
		return
	}
	invocation, err := c.repo.GetInvocation(ctx, invocationID)
	if err != nil {
		return
	}
	_ = c.kernel.RecordContextUsageCalibration(context.WithoutCancel(ctx), invocation.ProviderID, invocation.ModelID, estimated, actual)
}

func (c *Coordinator) recordRuntimeCapabilityEvidence(ctx context.Context, invocationID string, observations []provider.CapabilityObservation, seen map[string]struct{}) {
	if c == nil || c.kernel == nil || len(observations) == 0 {
		return
	}
	if seen == nil {
		seen = make(map[string]struct{})
	}
	filtered := make([]provider.CapabilityObservation, 0, len(observations))
	for _, observation := range observations {
		key := strings.TrimSpace(observation.Feature) + "\x00" + string(observation.State) + "\x00" + strings.TrimSpace(observation.Route)
		if _, exists := seen[key]; exists {
			continue
		}
		if strings.TrimSpace(observation.Feature) == "" {
			continue
		}
		seen[key] = struct{}{}
		filtered = append(filtered, observation)
	}
	if len(filtered) == 0 {
		return
	}
	invocation, err := c.repo.GetInvocation(ctx, invocationID)
	if err != nil {
		return
	}
	if err := c.kernel.RecordRuntimeCapabilityObservations(ctx, invocation.ProviderID, invocation.ModelID, filtered); err != nil {
		// Capability evidence is diagnostic feedback. It must never turn a
		// completed model response into a Runtime failure if the optional
		// observation store is unavailable.
		return
	}
}

func runtimeCapabilityEvidenceFromEvent(event AgentEvent) []provider.CapabilityObservation {
	state := provider.SupportSupported
	confidence := 0.85
	route, _ := event.Data["model_route"].(string)
	switch event.Type {
	case EventAssistantDelta:
		return []provider.CapabilityObservation{{Feature: "streaming", State: state, Confidence: 0.95, Route: route, Reason: "runtime stream delta observed"}}
	case EventAssistantMessage:
		return []provider.CapabilityObservation{{Feature: "text_generation", State: state, Confidence: confidence, Route: route, Reason: "runtime text response observed"}}
	case EventToolRequested:
		return []provider.CapabilityObservation{{Feature: "tool_calling", State: state, Confidence: 0.95, Route: route, Reason: "runtime function call observed"}}
	case EventUsageUpdated:
		return []provider.CapabilityObservation{{Feature: "usage", State: state, Confidence: 0.8, Route: route, Reason: "runtime usage metadata observed"}}
	default:
		return nil
	}
}

func runtimeCapabilityEvidenceFromError(err error, request agent.ChatRequest) []provider.CapabilityObservation {
	if err == nil {
		return nil
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if message == "" || containsAny(message, "timeout", "timed out", "rate limit", "http 408", "http 409", "http 429", "http 500", "http 502", "http 503", "http 504", "temporarily", "connection reset", "network", "context canceled", "context deadline") {
		return nil
	}
	reason := "stable provider capability rejection: " + sanitizeRuntimeString(err.Error())
	if containsAny(message, "json schema", "json_schema", "response_json_schema", "responsejsonschema", "structured schema", "structured_schema", "schema dialect") {
		return []provider.CapabilityObservation{{Feature: "structured_output_schema", State: provider.SupportUnsupported, Confidence: 0.8, Reason: reason}}
	}
	if containsAny(message, "response_format", "response format", "structured output", "structured_output", "response mimetype", "response_mime_type") {
		return []provider.CapabilityObservation{{Feature: "structured_output", State: provider.SupportUnsupported, Confidence: 0.8, Reason: reason}}
	}
	if containsAny(message, "parallel_tool_calls", "parallel tool calls", "parallel tools") {
		return []provider.CapabilityObservation{{Feature: "parallel_tool_calls", State: provider.SupportUnsupported, Confidence: 0.8, Reason: reason}}
	}
	if containsAny(message, "tool_choice", "tool choice", "function tool", "function_call", "function call", "tools", "tool calling", "tool_calling") {
		return []provider.CapabilityObservation{{Feature: "tool_calling", State: provider.SupportUnsupported, Confidence: 0.8, Reason: reason}}
	}
	if request.Stream && containsAny(message, "stream", "sse", "event-stream", "event stream") {
		return []provider.CapabilityObservation{{Feature: "streaming", State: provider.SupportUnsupported, Confidence: 0.8, Reason: reason}}
	}
	if containsAny(message, "reasoning", "thinking", "thought") {
		return []provider.CapabilityObservation{{Feature: "reasoning_effort", State: provider.SupportUnsupported, Confidence: 0.8, Reason: reason}}
	}
	if containsAny(message, "input_image", "image_url", "image input", "vision input", "multimodal", "image modality") {
		return []provider.CapabilityObservation{{Feature: "images", State: provider.SupportUnsupported, Confidence: 0.8, Reason: reason}}
	}
	if containsAny(message, "input_file", "file_data", "file input", "file upload", "document input") {
		return []provider.CapabilityObservation{{Feature: "input_files", State: provider.SupportUnsupported, Confidence: 0.8, Reason: reason}}
	}
	return nil
}

func containsAny(value string, markers ...string) bool {
	for _, marker := range markers {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func promptTokenCount(value any) (int, bool) {
	fields := normalizeUsageMap(value)
	if fields == nil {
		return 0, false
	}
	return usageNumber(fields, usageAliases("prompt_tokens")...)
}

// reconcilePlanOnTerminal closes the plan-side lifecycle boundary before an
// Invocation becomes terminal. A successful Invocation is not evidence that
// an inspect/edit/verify workflow completed: if a worker stopped before the
// active step produced durable evidence, leave an explicit blocked plan rather
// than exposing an in_progress step that can never be resumed safely. The
// helper is deliberately best-effort for legacy repositories without a
// TaskPlan; callers retain the Invocation terminal result and can surface a
// metadata-only reconciliation notice when a repository fails.
func (c *Coordinator) reconcilePlanOnTerminal(ctx context.Context, invocation Invocation, status InvocationStatus) error {
	if c == nil || c.repo == nil || !status.Terminal() {
		return nil
	}
	planRepo, ok := c.repo.(TaskPlanRepository)
	if !ok {
		return nil
	}
	plan, err := planRepo.GetTaskPlan(ctx, invocation.ID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(plan.Steps) == 0 || plan.Status == PlanCompleted || plan.Status == PlanBlocked {
		return nil
	}
	code := "invocation_" + string(status) + "_before_plan_complete"
	blocker := "计划在 Invocation " + string(status) + " 前未获得完成证据"
	if len(code) > 160 {
		code = code[:160]
	}
	if len(blocker) > 4096 {
		blocker = blocker[:4096]
	}
	if plan.CurrentStepID != "" {
		blocked, blockErr := plan.BlockStep(plan.CurrentStepID, code+": "+blocker, time.Now().UTC())
		if blockErr != nil {
			return blockErr
		}
		plan = blocked
	} else {
		// A pending plan with no admitted step can still occur when the model
		// returned before producing a tool/verification event. Keep every step
		// pending for audit, but block the plan as a whole so it cannot look
		// resumable without a new explicit Invocation.
		plan.Blocker = code + ": " + blocker
		plan.Status = PlanBlocked
		plan.UpdatedAt = time.Now().UTC()
	}
	_, err = c.UpdateTaskPlan(context.WithoutCancel(ctx), invocation.ID, plan, "invocation terminal without plan evidence")
	return err
}

func (c *Coordinator) finish(ctx context.Context, id string, status InvocationStatus, message string) {
	invocation, err := c.repo.GetInvocation(ctx, id)
	if err != nil {
		return
	}
	if invocation.Status.Terminal() {
		return
	}
	auditCtx := context.WithoutCancel(ctx)
	if reconcileErr := c.reconcilePlanOnTerminal(auditCtx, invocation, status); reconcileErr != nil {
		// The Invocation terminal transition remains authoritative. Keep a
		// bounded diagnostic event for a repository that could not close the
		// optional plan projection; never copy the underlying error text into
		// the user-visible timeline.
		_, _ = c.appendAndPublish(auditCtx, AgentEvent{ID: newID("event"), InvocationID: id, Type: EventRuntimeNotice, Timestamp: time.Now().UTC(), Data: map[string]any{
			"code": "plan_terminal_reconciliation_failed", "error_type": fmt.Sprintf("%T", reconcileErr),
		}})
	}
	if ok, transitionErr := c.repo.TransitionInvocation(auditCtx, id, invocation.Status, status, message); transitionErr != nil || !ok {
		return
	}
	// Runtime evidence is persisted while the task runs, but profile mutation
	// is deferred until the Invocation is terminal so approval recovery keeps
	// using the immutable capability snapshot selected at start.
	if c.kernel != nil && invocation.ProviderID != "" && invocation.ModelID != "" {
		_ = c.kernel.ReconcileRuntimeCapabilityEvidence(auditCtx, invocation.ProviderID, invocation.ModelID)
	}
	// Snapshot must be rebuilt after the terminal state is durable but before
	// the terminal event is emitted. EventRuntimeSnapshot is explicitly allowed
	// after terminalization so replay still sees the final recovery facts.
	c.refreshRuntimeSnapshot(auditCtx, id)
	typeName := terminalEventForStatus(status)
	c.emitTerminal(auditCtx, id, typeName, map[string]any{"error": message})
}

func (c *Coordinator) emitTerminal(ctx context.Context, id, eventType string, data map[string]any) {
	_, _ = c.appendAndPublish(ctx, AgentEvent{ID: newID("event"), InvocationID: id, Type: eventType, Timestamp: time.Now().UTC(), Data: data})
	if invocation, err := c.repo.GetInvocation(ctx, id); err == nil {
		now := time.Now().UTC()
		invocation.FinishedAt = &now
		_ = c.repo.UpdateInvocation(ctx, invocation)
	}
}

func (c *Coordinator) appendAndPublish(ctx context.Context, event AgentEvent) (AgentEvent, error) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	if !isTerminalEvent(event.Type) {
		if invocation, err := c.repo.GetInvocation(ctx, event.InvocationID); err != nil {
			return AgentEvent{}, err
		} else if invocation.Status.Terminal() && event.Type != EventRuntimeSnapshot && !isCancellationApprovalEvent(event) && !isPostTerminalAudit(ctx) {
			return AgentEvent{}, ErrConflict
		}
	}
	c.appendMu.Lock()
	stored, err := c.repo.AppendEvent(ctx, event)
	c.appendMu.Unlock()
	if err != nil {
		return AgentEvent{}, err
	}
	c.publishStoredEvent(stored)
	return stored, nil
}

func (c *Coordinator) publishStoredEvent(stored AgentEvent) {
	c.mu.Lock()
	for ch := range c.subscribers[stored.InvocationID] {
		select {
		case ch <- stored:
		default:
			// A slow observer can always replay from SQLite; never let it block
			// the model/tool execution loop.
		}
	}
	c.mu.Unlock()
}

func isTerminalEvent(eventType string) bool {
	return eventType == EventInvocationCompleted || eventType == EventInvocationFailed || eventType == EventInvocationCancelled || eventType == EventInvocationExpired
}

func isCancellationApprovalEvent(event AgentEvent) bool {
	if event.Type != EventApprovalResolved || event.Data == nil {
		return false
	}
	decision, _ := event.Data["decision"].(string)
	return decision == string(ApprovalCancelled)
}

func (c *Coordinator) unsubscribe(invocationID string, ch chan AgentEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if subscribers := c.subscribers[invocationID]; subscribers != nil {
		if _, ok := subscribers[ch]; ok {
			delete(subscribers, ch)
			close(ch)
		}
		if len(subscribers) == 0 {
			delete(c.subscribers, invocationID)
		}
	}
}

type approvalData struct {
	ToolName           string
	OperationID        string
	OriginalCallID     string
	ConfirmationCallID string
	Args               map[string]any
	Hint               string
	Choices            []ApprovalChoice
}

var approvalChoiceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// normalizeApprovalChoices 固定审批为“允许一次/拒绝”两个互斥选项。
// 普通业务选项必须走 UserInputRequest，否则用户可能把多选答案误当成
// 工具授权，造成审批语义和实际副作用不一致。
func normalizeApprovalChoices(_ []ApprovalChoice) []ApprovalChoice {
	return DefaultApprovalChoices()
}

// approvalChoicesFromValue 从确认 payload 读取结构化选项。缺少 approved 字段
// 的外部数据会被忽略，因为 Runtime 不能根据文案猜测该选项是否允许副作用。
func approvalChoicesFromValue(value any) []ApprovalChoice {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var wire []struct {
		ID       string `json:"id"`
		Label    string `json:"label"`
		Approved *bool  `json:"approved"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		return nil
	}
	items := make([]ApprovalChoice, 0, len(wire))
	for _, item := range wire {
		if item.Approved == nil {
			continue
		}
		items = append(items, ApprovalChoice{ID: item.ID, Label: item.Label, Approved: *item.Approved})
	}
	if len(items) == 0 {
		return nil
	}
	return normalizeApprovalChoices(items)
}

// approvalChoicesFromConfirmation 允许工具在 confirmation payload 中声明多选
// 交互，同时为旧工具补上默认的允许/拒绝选项。
func approvalChoicesFromConfirmation(call *genai.FunctionCall) []ApprovalChoice {
	// 工具授权固定为二元审批。多选、单选和自由文本属于 RequestInput
	// 的普通用户问题，不能把任意 payload 中的 choices 混进授权卡片，
	// 否则用户会误以为选择某个业务选项就等同于批准副作用。
	return DefaultApprovalChoices()
}

// userInputRequestFromADK 将 ADK 的 RequestInput 转成稳定的产品协议。
// Payload 里的字段只负责描述交互，不会被当作自然语言指令执行；选项 ID
// 和回答值会在恢复时再次按当前请求校验。
func userInputRequestFromADK(invocationID string, input *session.RequestInput) UserInputRequest {
	request := UserInputRequest{
		ID:            strings.TrimSpace(input.InterruptID),
		InvocationID:  strings.TrimSpace(invocationID),
		Question:      strings.TrimSpace(input.Message),
		Kind:          UserInputText,
		AllowFreeform: true,
	}
	payload := make(map[string]any)
	if input.Payload != nil {
		if encoded, err := json.Marshal(input.Payload); err == nil {
			_ = json.Unmarshal(encoded, &payload)
		}
	}
	request.Payload = payload
	stringValue := func(keys ...string) string {
		for _, key := range keys {
			if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}
	boolValue := func(keys ...string) (bool, bool) {
		for _, key := range keys {
			if value, ok := payload[key].(bool); ok {
				return value, true
			}
		}
		return false, false
	}
	intValue := func(keys ...string) int {
		for _, key := range keys {
			switch value := payload[key].(type) {
			case int:
				return value
			case float64:
				return int(value)
			}
		}
		return 0
	}
	if value := stringValue("title", "step_title", "section"); value != "" {
		request.Title = value
	}
	if value := stringValue("question", "prompt", "message"); value != "" {
		request.Question = value
	}
	if value := stringValue("placeholder"); value != "" {
		request.Placeholder = value
	}
	kind := strings.ToLower(strings.TrimSpace(stringValue("kind", "type", "selection_type")))
	switch kind {
	case "multi", "multiple", "multiselect", "multi_select", "checkbox", "checkboxes":
		request.Kind = UserInputMultiSelect
	case "single", "single_select", "radio", "choice", "select":
		request.Kind = UserInputSingleSelect
	case "text", "input", "freeform":
		request.Kind = UserInputText
	}
	if rawOptions, exists := payload["options"]; exists {
		encoded, err := json.Marshal(rawOptions)
		if err == nil {
			var wire []struct {
				ID          string `json:"id"`
				Label       string `json:"label"`
				Description string `json:"description"`
				Recommended bool   `json:"recommended"`
			}
			if json.Unmarshal(encoded, &wire) == nil {
				for index, item := range wire {
					label := strings.TrimSpace(item.Label)
					if label == "" || len(request.Options) >= 16 {
						continue
					}
					optionID := strings.TrimSpace(item.ID)
					if !approvalChoiceIDPattern.MatchString(optionID) {
						optionID = fmt.Sprintf("option-%d", index+1)
					}
					duplicate := false
					for _, existing := range request.Options {
						if existing.ID == optionID {
							duplicate = true
							break
						}
					}
					if duplicate {
						continue
					}
					request.Options = append(request.Options, UserInputOption{ID: optionID, Label: label, Description: strings.TrimSpace(item.Description), Recommended: item.Recommended})
				}
			}
		}
	}
	if len(request.Options) > 0 && request.Kind == UserInputText {
		request.Kind = UserInputSingleSelect
	}
	if request.Kind == UserInputMultiSelect {
		request.AllowMultiple = true
	}
	if value, exists := boolValue("allow_multiple", "multiple"); exists {
		request.AllowMultiple = value
		if value {
			request.Kind = UserInputMultiSelect
		}
	}
	if value, exists := boolValue("allow_freeform", "allow_other", "custom_answer"); exists {
		request.AllowFreeform = value
	}
	if value, exists := boolValue("allow_skip", "skippable"); exists {
		request.AllowSkip = value
	}
	request.MinSelections = intValue("min_selections", "min_choices")
	request.MaxSelections = intValue("max_selections", "max_choices")
	if request.MinSelections < 0 {
		request.MinSelections = 0
	}
	if request.MaxSelections < 0 {
		request.MaxSelections = 0
	}
	if request.MaxSelections > 16 {
		request.MaxSelections = 16
	}
	request.Step = intValue("step", "step_number")
	request.TotalSteps = intValue("total_steps", "steps")
	if request.Step < 0 {
		request.Step = 0
	}
	if request.TotalSteps < 0 {
		request.TotalSteps = 0
	}
	if request.Question == "" {
		request.Question = "请提供输入"
	}
	return request
}

// userInputRequestData 生成 SSE、Bot 和审计事件共用的 JSON 数据。
func userInputRequestData(request UserInputRequest) map[string]any {
	encoded, err := json.Marshal(request)
	if err != nil {
		return map[string]any{"request_id": request.ID, "question": request.Question, "kind": request.Kind}
	}
	data := make(map[string]any)
	if err := json.Unmarshal(encoded, &data); err != nil {
		return map[string]any{"request_id": request.ID, "question": request.Question, "kind": request.Kind}
	}
	data["request_id"] = request.ID
	data["wait_id"] = request.ID
	data["name"] = workflow.WorkflowInputFunctionCallName
	return data
}

// MapSessionEvent translates one ADK event into one or more stable product
// events. The original event is retained as a JSON-shaped value for diagnostics
// and for clients that need details not yet promoted into the protocol.
func MapSessionEvent(invocationID string, event *session.Event) ([]AgentEvent, []approvalData) {
	if event == nil {
		return nil, nil
	}
	raw := make(map[string]any)
	if encoded, err := json.Marshal(event); err == nil {
		_ = json.Unmarshal(encoded, &raw)
	}
	raw = sanitizeRuntimeMap(raw)
	base := map[string]any{"adk_event_id": event.ID, "author": event.Author, "partial": event.Partial, "final": event.IsFinalResponse(), "raw": raw}
	if event.Content != nil {
		base["role"] = event.Content.Role
	}
	if route := eventModelRoute(event); route != "" {
		base["model_route"] = route
	}
	result := make([]AgentEvent, 0, 3)
	approvals := make([]approvalData, 0)
	add := func(eventType string, data map[string]any) {
		merged := make(map[string]any, len(base)+len(data))
		for key, value := range base {
			merged[key] = value
		}
		for key, value := range data {
			merged[key] = value
		}
		result = append(result, AgentEvent{ID: newID("event"), InvocationID: invocationID, Type: eventType, Timestamp: event.Timestamp, Data: merged})
	}
	if event.RequestedInput != nil {
		// RequestInput 是 ADK 工作流的正式暂停协议；它不能落入普通工具调用
		// 分支，否则前端会把“请选择”误显示成外部工具等待。
		request := userInputRequestFromADK(invocationID, event.RequestedInput)
		add(EventUserInputRequested, userInputRequestData(request))
		add(EventInvocationWaiting, map[string]any{
			"reason": "user", "wait_id": request.ID, "name": workflow.WorkflowInputFunctionCallName,
			"request_id": request.ID, "question": request.Question, "kind": request.Kind,
		})
		return result, approvals
	}
	if event.Actions.Compaction != nil {
		add(EventContextCompacted, map[string]any{"compaction": event.Actions.Compaction})
	}
	if event.UsageMetadata != nil {
		// Keep the provider usage object intact. In particular, do not infer a
		// zero TotalTokenCount to mean zero usage (some compaction/provider
		// responses only populate the component counters).
		usageData := map[string]any{"usage": event.UsageMetadata}
		if event.Actions.Compaction != nil {
			usageData["scope"] = "compaction"
		}
		if callID := eventModelCallID(event); callID != "" {
			usageData["model_call_id"] = callID
		}
		add(EventUsageUpdated, usageData)
	}
	if len(event.Actions.ArtifactDelta) > 0 {
		add(EventArtifactUpdated, map[string]any{"artifact_delta": event.Actions.ArtifactDelta})
	}
	if event.Output != nil {
		outputData := map[string]any{"output": event.Output}
		// Workflow/tool adapters may include their own call correlation in the
		// generic Output value. Promote those keys when present while retaining
		// the complete structured payload above.
		if value, ok := event.Output.(map[string]any); ok {
			for _, key := range []string{"call_id", "tool_call_id", "name"} {
				if correlation, exists := value[key]; exists {
					outputData[key] = correlation
				}
			}
		}
		add(EventToolOutput, outputData)
		// Workflow agents may expose a structured plan through Event.Output.
		// Only promote an explicitly named plan; all other workflow metadata
		// remains available in the raw ADK event below.
		if value, ok := event.Output.(map[string]any); ok {
			if plan, exists := value["plan"]; exists {
				add(EventPlanUpdated, map[string]any{"plan": plan})
			}
		}
	}
	if event.Content == nil || len(event.Content.Parts) == 0 {
		if len(event.LongRunningToolIDs) > 0 {
			add(EventInvocationWaiting, map[string]any{"reason": "tool", "tool_call_ids": event.LongRunningToolIDs})
		} else if len(result) == 0 {
			add(EventADK, nil)
		}
		return result, approvals
	}
	for _, part := range event.Content.Parts {
		if part == nil {
			continue
		}
		if part.Text != "" {
			typeName := EventAssistantMessage
			if event.Author == "user" || event.Content.Role == genai.RoleUser {
				typeName = EventUserMessage
			} else if event.Partial {
				typeName = EventAssistantDelta
			}
			add(typeName, map[string]any{"text": part.Text})
		}
		if call := part.FunctionCall; call != nil {
			if call.Name == toolconfirmation.FunctionCallName {
				original, err := toolconfirmation.OriginalCallFrom(call)
				if err != nil {
					add(EventToolFailed, map[string]any{"call_id": call.ID, "name": call.Name, "error": err.Error()})
					continue
				}
				hint := "需要批准后才能执行该操作"
				switch value := call.Args["toolConfirmation"].(type) {
				case map[string]any:
					if text, ok := value["hint"].(string); ok && strings.TrimSpace(text) != "" {
						hint = text
					}
				case toolconfirmation.ToolConfirmation:
					if strings.TrimSpace(value.Hint) != "" {
						hint = value.Hint
					}
				case *toolconfirmation.ToolConfirmation:
					if value != nil && strings.TrimSpace(value.Hint) != "" {
						hint = value.Hint
					}
				}
				operationID := confirmationOperationID(call)
				choices := approvalChoicesFromConfirmation(call)
				approvalEventData := map[string]any{
					"approval_call_id": call.ID, "original_call_id": original.ID,
					"tool_name": original.Name, "args": sanitizeRuntimeValue(original.Args), "hint": hint, "choices": choices,
				}
				if operationID != "" {
					approvalEventData["operation_id"] = operationID
				}
				add(EventApprovalRequested, approvalEventData)
				approvals = append(approvals, approvalData{ToolName: original.Name, OperationID: operationID, OriginalCallID: original.ID, ConfirmationCallID: call.ID, Args: sanitizeRuntimeMap(original.Args), Hint: hint, Choices: choices})
			} else {
				add(EventToolRequested, map[string]any{"call_id": call.ID, "name": call.Name, "args": sanitizeRuntimeValue(call.Args)})
				add(EventToolStarted, map[string]any{"call_id": call.ID, "name": call.Name})
			}
		}
		if response := part.FunctionResponse; response != nil {
			if response.Name == workflow.WorkflowInputFunctionCallName {
				// 普通问题的回答是 workflow 的正式恢复结果，不是工具完成
				// 事件；单独记录 request_id 便于 UI 恢复未完成的问题卡片。
				add(EventUserInputResolved, map[string]any{
					"request_id": response.ID, "wait_id": response.ID,
					"response": sanitizeRuntimeValue(response.Response),
				})
			} else if response.Name == toolconfirmation.FunctionCallName {
				confirmed, _ := response.Response["confirmed"].(bool)
				resolvedData := map[string]any{"confirmation_call_id": response.ID, "confirmed": confirmed}
				if payload, ok := response.Response["payload"].(map[string]any); ok {
					if choiceID, ok := payload["choice_id"].(string); ok && strings.TrimSpace(choiceID) != "" {
						resolvedData["choice_id"] = strings.TrimSpace(choiceID)
					}
				}
				add(EventApprovalResolved, resolvedData)
			} else {
				if response.Response != nil {
					if toolError, exists := response.Response["error"]; exists && toolError != nil {
						add(EventToolFailed, map[string]any{"call_id": response.ID, "name": response.Name, "error": sanitizeRuntimeValue(toolError), "response": sanitizeRuntimeValue(response.Response)})
						continue
					}
				}
				add(EventToolCompleted, map[string]any{"call_id": response.ID, "name": response.Name, "response": sanitizeRuntimeValue(response.Response)})
			}
		}
	}
	if len(event.LongRunningToolIDs) > 0 && len(approvals) == 0 {
		// A confirmation event is represented by approval.requested above. Any
		// other ADK long-running marker is a durable waiting_tool boundary.
		add(EventInvocationWaiting, map[string]any{"reason": "tool", "tool_call_ids": event.LongRunningToolIDs})
	}
	if len(result) == 0 {
		add(EventADK, nil)
	}
	return result, approvals
}

// eventModelCallID extracts only the runtime-generated correlation marker
// added by modelCallMetadataLLM. Provider payloads and model text are never
// used as an identifier, so an upstream response cannot forge a manifest
// lookup by echoing arbitrary content.
func eventModelCallID(event *session.Event) string {
	if event == nil {
		return ""
	}
	for _, key := range []string{"abot_model_call_id", "model_call_id", "modelCallID"} {
		if value, ok := event.CustomMetadata[key].(string); ok {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	if value, ok := event.Output.(map[string]any); ok {
		if callID, ok := value["model_call_id"].(string); ok {
			return strings.TrimSpace(callID)
		}
	}
	return ""
}

func eventModelRoute(event *session.Event) string {
	if event == nil {
		return ""
	}
	for _, key := range []string{"openai_wire_format", "model_route", "route"} {
		value, ok := event.CustomMetadata[key].(string)
		if !ok {
			continue
		}
		normalized := strings.ToLower(strings.TrimSpace(value))
		switch normalized {
		case "auto", "chat", "responses", "gemini":
			return normalized
		}
	}
	return ""
}

func confirmationOperationID(call *genai.FunctionCall) string {
	if call == nil || call.Args == nil {
		return ""
	}
	value, ok := call.Args["toolConfirmation"]
	if !ok {
		return ""
	}
	var payload any
	switch confirmation := value.(type) {
	case toolconfirmation.ToolConfirmation:
		payload = confirmation.Payload
	case *toolconfirmation.ToolConfirmation:
		if confirmation != nil {
			payload = confirmation.Payload
		}
	case map[string]any:
		payload = confirmation["payload"]
		if payload == nil {
			payload = confirmation["Payload"]
		}
	}
	switch data := payload.(type) {
	case map[string]any:
		if id, ok := data["operation_id"].(string); ok {
			return strings.TrimSpace(id)
		}
	case map[string]string:
		return strings.TrimSpace(data["operation_id"])
	}
	return ""
}

var idCounter atomic.Uint64

func newID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), idCounter.Add(1))
}

func cloneAttachments(source []agent.Attachment) []agent.Attachment {
	if len(source) == 0 {
		return nil
	}
	result := make([]agent.Attachment, len(source))
	for index, item := range source {
		result[index] = item
		result[index].Data = append([]byte(nil), item.Data...)
		if item.Ref != nil {
			ref := *item.Ref
			// Preview is a UI convenience projection and may contain extracted
			// user content. Runtime's durable invocation envelope is intentionally
			// metadata-only; callers can fetch a preview through the Artifact
			// boundary when they explicitly need it.
			ref.Preview = ""
			result[index].Ref = &ref
		}
	}
	return result
}

// sanitizeRuntimeValue is the last defensive boundary before ADK-derived
// values enter durable AgentEvent/Approval/ToolCall data. It keeps useful
// non-secret fields such as paths and exit codes, but replaces values whose
// field names conventionally carry credentials. This is intentionally
// metadata-only and does not attempt to inspect hidden model reasoning or
// guess secrets embedded in arbitrary prose.
func sanitizeRuntimeValue(value any) any {
	switch item := value.(type) {
	case map[string]any:
		return sanitizeRuntimeMap(item)
	case []any:
		result := make([]any, len(item))
		for index, nested := range item {
			result[index] = sanitizeRuntimeValue(nested)
		}
		return result
	case map[string]string:
		result := make(map[string]string, len(item))
		for key, nested := range item {
			if isSensitiveRuntimeKey(key) {
				result[key] = "[REDACTED]"
			} else {
				result[key] = sanitizeRuntimeString(nested)
			}
		}
		return result
	case string:
		return sanitizeRuntimeString(item)
	default:
		// Structs and typed maps are converted through JSON so callers do not
		// accidentally bypass the recursive key check.
		encoded, err := json.Marshal(value)
		if err != nil {
			return "[UNAVAILABLE]"
		}
		var normalized any
		if err := json.Unmarshal(encoded, &normalized); err != nil {
			return "[UNAVAILABLE]"
		}
		if _, isMap := normalized.(map[string]any); isMap {
			return sanitizeRuntimeValue(normalized)
		}
		if _, isList := normalized.([]any); isList {
			return sanitizeRuntimeValue(normalized)
		}
		return normalized
	}
}

func sanitizeRuntimeMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	result := make(map[string]any, len(value))
	for key, nested := range value {
		if isSensitiveRuntimeKey(key) {
			result[key] = "[REDACTED]"
			continue
		}
		result[key] = sanitizeRuntimeValue(nested)
	}
	return result
}

func isSensitiveRuntimeKey(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"))
	for _, marker := range []string{"password", "passwd", "secret", "api_key", "apikey", "authorization", "cookie", "private_key", "access_token", "refresh_token", "credential"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func sanitizeRuntimeString(value string) string {
	if strings.TrimSpace(value) == "" {
		return value
	}
	value = runtimeSecretAssignmentPattern.ReplaceAllString(value, "$1=[REDACTED]")
	return runtimeBearerPattern.ReplaceAllString(value, "Bearer [REDACTED]")
}
