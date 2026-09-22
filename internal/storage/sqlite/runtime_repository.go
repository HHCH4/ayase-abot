package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/workspace"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type invocationRow struct {
	ID                             string `gorm:"primaryKey;size:100"`
	UserID                         string `gorm:"index;size:200;not null"`
	IdempotencyKey                 string `gorm:"index;size:200"`
	BotID                          string `gorm:"index;size:100"`
	ConversationID                 string `gorm:"index;size:100;not null"`
	WorkspaceID                    string `gorm:"index;size:100"`
	TargetPath                     string `gorm:"size:1000"`
	SessionID                      string `gorm:"index;size:100;not null"`
	ParentInvocationID             string `gorm:"index;size:100"`
	ContinuationDecision           string `gorm:"size:32"`
	ContinuationSourcePlanRevision int64  `gorm:"index"`
	ProviderID                     string `gorm:"size:100"`
	ModelID                        string `gorm:"size:300"`
	ConfigSnapshot                 string `gorm:"type:text"`
	Message                        string `gorm:"type:text"`
	// 群历史与主动回复权限随任务保存，避免重启后恢复时语义改变。
	GroupContext     string `gorm:"type:text"`
	BotDeliveryJSON  string `gorm:"type:text"`
	Proactive        bool
	AttachmentsJSON  string `gorm:"type:text"`
	Status           string `gorm:"index;size:40;not null"`
	Error            string `gorm:"type:text"`
	ActiveApprovalID string `gorm:"index;size:100"`
	LeaseOwner       string `gorm:"index;size:160"`
	LeaseExpiresAt   *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
	StartedAt        *time.Time
	FinishedAt       *time.Time
}

func (invocationRow) TableName() string { return "abot_agent_invocations" }

type invocationResumeRow struct {
	ID            string `gorm:"primaryKey;size:160"`
	InvocationID  string `gorm:"uniqueIndex;size:100;not null"`
	WaitID        string `gorm:"size:200;not null"`
	Name          string `gorm:"size:200;not null"`
	ResponseJSON  string `gorm:"type:text;not null"`
	RequestDigest string `gorm:"size:128;not null"`
	CreatedAt     time.Time
}

func (invocationResumeRow) TableName() string { return "abot_agent_invocation_resumes" }

type invocationResumeOutboxRow struct {
	ID             string     `gorm:"primaryKey;size:220"`
	InvocationID   string     `gorm:"index:idx_resume_outbox_invocation_created,priority:1;index:idx_resume_outbox_digest,priority:1;size:100;not null"`
	WaitID         string     `gorm:"size:200;not null"`
	Name           string     `gorm:"size:200;not null"`
	ResponseJSON   string     `gorm:"type:text;not null"`
	RequestDigest  string     `gorm:"index:idx_resume_outbox_digest,priority:2;size:128;not null"`
	Status         string     `gorm:"index;size:32;not null"`
	Attempt        int        `gorm:"not null"`
	Revision       int64      `gorm:"not null"`
	AvailableAt    time.Time  `gorm:"index"`
	LeaseOwner     string     `gorm:"index;size:160"`
	LeaseExpiresAt *time.Time `gorm:"index"`
	LastError      string     `gorm:"type:text"`
	CreatedAt      time.Time  `gorm:"index:idx_resume_outbox_invocation_created,priority:2"`
	UpdatedAt      time.Time
}

func (invocationResumeOutboxRow) TableName() string { return "abot_agent_invocation_resume_outbox" }

type worktreeBaselineRow struct {
	ID             string `gorm:"primaryKey;size:100"`
	InvocationID   string `gorm:"uniqueIndex;size:100;not null"`
	WorkspaceID    string `gorm:"index;size:256"`
	TargetPath     string `gorm:"size:4096"`
	RepositoryType string `gorm:"size:64;not null"`
	HeadRevision   string `gorm:"size:256"`
	Branch         string `gorm:"size:512"`
	StatusDigest   string `gorm:"size:256;not null"`
	ChangedJSON    string `gorm:"type:text;not null"`
	StatusKnown    bool   `gorm:"not null"`
	Truncated      bool   `gorm:"not null"`
	Revision       int64  `gorm:"not null"`
	CapturedAt     time.Time
}

func (worktreeBaselineRow) TableName() string { return "abot_agent_worktree_baselines" }

type agentEventRow struct {
	ID           string    `gorm:"primaryKey;size:100"`
	InvocationID string    `gorm:"index:idx_agent_events_invocation_sequence,unique;size:100;not null"`
	Sequence     int64     `gorm:"index:idx_agent_events_invocation_sequence,unique;not null"`
	Type         string    `gorm:"index;size:80;not null"`
	Timestamp    time.Time `gorm:"index"`
	DataJSON     string    `gorm:"type:text"`
}

func (agentEventRow) TableName() string { return "abot_agent_events" }

// runtimeEventOutboxRow stores only the delivery cursor. The immutable event
// body remains in abot_agent_events and is fetched by EventID after a worker
// claims this row, avoiding a second copy of prompt/output data.
type runtimeEventOutboxRow struct {
	ID             string     `gorm:"primaryKey;size:128"`
	GroupID        string     `gorm:"index;size:220"`
	InvocationID   string     `gorm:"index:idx_runtime_event_outbox_invocation,priority:1;size:100;not null"`
	EventID        string     `gorm:"uniqueIndex;size:512;not null"`
	Sequence       int64      `gorm:"index;not null"`
	Type           string     `gorm:"index;size:80;not null"`
	Status         string     `gorm:"index;size:32;not null"`
	Attempt        int        `gorm:"not null"`
	Revision       int64      `gorm:"not null"`
	AvailableAt    time.Time  `gorm:"index"`
	LeaseOwner     string     `gorm:"index;size:160"`
	LeaseExpiresAt *time.Time `gorm:"index"`
	LastError      string     `gorm:"type:text"`
	CreatedAt      time.Time  `gorm:"index:idx_runtime_event_outbox_invocation,priority:2"`
	UpdatedAt      time.Time
}

func (runtimeEventOutboxRow) TableName() string { return "abot_agent_event_outbox" }

// runtimeEventDeliveryInboxRow is the receiver-side idempotency ledger for
// signed cross-service event metadata. The event body remains in the source
// runtime event log and is never duplicated here.
type runtimeEventDeliveryInboxRow struct {
	Version      int    `gorm:"not null"`
	EventID      string `gorm:"primaryKey;size:512"`
	DeliveryID   string `gorm:"size:220;not null"`
	Source       string `gorm:"index;size:160;not null"`
	Destination  string `gorm:"index;size:160;not null"`
	InvocationID string `gorm:"index;size:512;not null"`
	Sequence     int64  `gorm:"not null"`
	Type         string `gorm:"size:80;not null"`
	Timestamp    time.Time
	IssuedAt     time.Time
	EventDigest  string `gorm:"size:64;not null"`
	Signature    string `gorm:"size:64;not null"`
	ReceivedAt   time.Time
}

func (runtimeEventDeliveryInboxRow) TableName() string { return "abot_agent_event_delivery_inbox" }

// runtimeCheckpointDeliveryInboxRow stores the newest metadata-only
// projection received from a remote Runtime. ProjectionJSON intentionally
// contains no prompt, tool arguments or free-form diagnostic text.
type runtimeCheckpointDeliveryInboxRow struct {
	Version          int    `gorm:"not null"`
	InvocationID     string `gorm:"primaryKey;size:512"`
	DeliveryID       string `gorm:"size:220;not null"`
	Source           string `gorm:"index;size:160;not null"`
	Destination      string `gorm:"index;size:160;not null"`
	SnapshotRevision int64  `gorm:"not null"`
	EventSequence    int64  `gorm:"not null"`
	SnapshotDigest   string `gorm:"size:64;not null"`
	Signature        string `gorm:"size:64;not null"`
	ProjectionJSON   string `gorm:"type:text;not null"`
	ReceivedAt       time.Time
}

func (runtimeCheckpointDeliveryInboxRow) TableName() string {
	return "abot_agent_checkpoint_delivery_inbox"
}

// runtimeCheckpointDeliveryOutboxRow keeps a bounded metadata-only
// projection for source-side retries. It is separate from the event outbox so
// checkpoint delivery can be claimed/acked without consuming ordinary event
// cursors.
type runtimeCheckpointDeliveryOutboxRow struct {
	ID               string     `gorm:"primaryKey;size:220"`
	GroupID          string     `gorm:"index;size:220"`
	InvocationID     string     `gorm:"index:idx_runtime_checkpoint_outbox_invocation,priority:1;size:512;not null"`
	Source           string     `gorm:"index;size:160;not null"`
	Destination      string     `gorm:"index;size:160;not null"`
	DeliveryID       string     `gorm:"index;size:220;not null"`
	SnapshotRevision int64      `gorm:"not null"`
	EventSequence    int64      `gorm:"not null"`
	SnapshotDigest   string     `gorm:"size:64;not null"`
	ProjectionJSON   string     `gorm:"type:text;not null"`
	Status           string     `gorm:"index;size:32;not null"`
	Attempt          int        `gorm:"not null"`
	Revision         int64      `gorm:"not null"`
	AvailableAt      time.Time  `gorm:"index"`
	LeaseOwner       string     `gorm:"index;size:160"`
	LeaseExpiresAt   *time.Time `gorm:"index"`
	LastError        string     `gorm:"type:text"`
	CreatedAt        time.Time  `gorm:"index:idx_runtime_checkpoint_outbox_invocation,priority:2"`
	UpdatedAt        time.Time
}

func (runtimeCheckpointDeliveryOutboxRow) TableName() string {
	return "abot_agent_checkpoint_delivery_outbox"
}

type approvalRow struct {
	ID                  string `gorm:"primaryKey;size:100"`
	InvocationID        string `gorm:"index;size:100;not null"`
	ConversationID      string `gorm:"index;size:100"`
	ToolCallID          string `gorm:"index;size:100"`
	TaskContractVersion int64  `gorm:"index"`
	ToolName            string `gorm:"size:200;not null"`
	OperationID         string `gorm:"index;size:100"`
	OriginalCallID      string `gorm:"index;size:200;not null"`
	ConfirmationCallID  string `gorm:"uniqueIndex;size:200;not null"`
	ArgsJSON            string `gorm:"type:text"`
	Hint                string `gorm:"type:text"`
	ChoicesJSON         string `gorm:"type:text"`
	Status              string `gorm:"index;size:32;not null"`
	DecisionReason      string `gorm:"type:text"`
	CreatedAt           time.Time
	UpdatedAt           time.Time
	ExpiresAt           *time.Time
	ResolvedAt          *time.Time
}

func (approvalRow) TableName() string { return "abot_agent_approvals" }

type toolCallRow struct {
	ID                 string `gorm:"primaryKey;size:100"`
	InvocationID       string `gorm:"index;size:100;not null"`
	ConversationID     string `gorm:"index;size:100"`
	ToolName           string `gorm:"size:200;not null"`
	OriginalCallID     string `gorm:"index;size:200;not null"`
	ConfirmationCallID string `gorm:"index;size:200"`
	OperationID        string `gorm:"index;size:100"`
	ApprovalID         string `gorm:"index;size:100"`
	ArgsJSON           string `gorm:"type:text"`
	Status             string `gorm:"index;size:32;not null"`
	Error              string `gorm:"type:text"`
	CreatedAt          time.Time
	UpdatedAt          time.Time
	FinishedAt         *time.Time
}

func (toolCallRow) TableName() string { return "abot_agent_tool_calls" }

type taskPlanRow struct {
	ID            string `gorm:"primaryKey;size:100"`
	InvocationID  string `gorm:"uniqueIndex;size:100;not null"`
	Revision      int64  `gorm:"not null"`
	Status        string `gorm:"index;size:32;not null"`
	CurrentStepID string `gorm:"size:100"`
	Blocker       string `gorm:"type:text"`
	StepsJSON     string `gorm:"type:text"`
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (taskPlanRow) TableName() string { return "abot_agent_task_plans" }

type taskContractRow struct {
	ID                   string `gorm:"primaryKey;size:100"`
	InvocationID         string `gorm:"index:idx_agent_task_contract_versions,unique;size:100;not null"`
	Version              int64  `gorm:"index:idx_agent_task_contract_versions,unique;not null"`
	TaskType             string `gorm:"index;size:32;not null"`
	Goal                 string `gorm:"type:text;not null"`
	RequestedOutcome     string `gorm:"type:text"`
	MutationAllowed      bool   `gorm:"not null"`
	ValidationRequired   bool   `gorm:"not null"`
	AcceptanceJSON       string `gorm:"type:text"`
	ConstraintsJSON      string `gorm:"type:text"`
	NonGoalsJSON         string `gorm:"type:text"`
	ExternalActionsJSON  string `gorm:"type:text"`
	SourceMessageIDsJSON string `gorm:"type:text"`
	UpdatedAt            time.Time
}

func (taskContractRow) TableName() string { return "abot_agent_task_contracts" }

type instructionSnapshotSetRow struct {
	ID            string `gorm:"primaryKey;size:100"`
	InvocationID  string `gorm:"uniqueIndex;size:100;not null"`
	TargetPath    string `gorm:"size:1000"`
	Revision      int64  `gorm:"not null"`
	SnapshotsJSON string `gorm:"type:text"`
	UpdatedAt     time.Time
}

func (instructionSnapshotSetRow) TableName() string { return "abot_agent_instruction_snapshots" }

type contextManifestRow struct {
	ID                 string  `gorm:"primaryKey;size:160"`
	InvocationID       string  `gorm:"index;size:100;not null"`
	ModelCallID        string  `gorm:"uniqueIndex;size:160;not null"`
	BuilderVersion     string  `gorm:"size:80;not null"`
	EstimatorVersion   string  `gorm:"size:80;not null"`
	EstimatorName      string  `gorm:"size:120"`
	EstimateQuality    string  `gorm:"size:32"`
	EstimateMultiplier float64 `gorm:"not null;default:1"`
	Model              string  `gorm:"size:300"`
	ToolSetSnapshotID  string  `gorm:"size:180"`
	ToolSetDigest      string  `gorm:"size:128"`
	ContextWindow      int     `gorm:"not null"`
	OutputReserve      int     `gorm:"not null"`
	SafetyReserve      int     `gorm:"not null"`
	EstimatedInput     int     `gorm:"not null"`
	ActualInput        *int
	IncludedJSON       string `gorm:"type:text"`
	ExcludedJSON       string `gorm:"type:text"`
	WarningsJSON       string `gorm:"type:text"`
	Digest             string `gorm:"index;size:128;not null"`
	CreatedAt          time.Time
}

func (contextManifestRow) TableName() string { return "abot_agent_context_manifests" }

type workingSetRow struct {
	ID           string `gorm:"primaryKey;size:100"`
	InvocationID string `gorm:"uniqueIndex;size:100;not null"`
	Revision     int64  `gorm:"not null"`
	ItemsJSON    string `gorm:"type:text;not null"`
	UpdatedAt    time.Time
}

func (workingSetRow) TableName() string { return "abot_agent_working_sets" }

type verificationRunRow struct {
	ID           string `gorm:"primaryKey;size:100"`
	InvocationID string `gorm:"index;size:100;not null"`
	PlanStepID   string `gorm:"index;size:100"`
	Kind         string `gorm:"size:80;not null"`
	Command      string `gorm:"type:text"`
	Status       string `gorm:"index;size:32;not null"`
	ExitCode     *int
	Summary      string `gorm:"type:text"`
	OutputRef    string `gorm:"size:4096"`
	OutputDigest string `gorm:"size:256"`
	Revision     int64  `gorm:"not null"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
}

func (verificationRunRow) TableName() string { return "abot_agent_verification_runs" }

type runtimeSnapshotRow struct {
	ID              string `gorm:"primaryKey;size:100"`
	InvocationID    string `gorm:"uniqueIndex;size:100;not null"`
	Revision        int64  `gorm:"not null"`
	ContractVersion int64  `gorm:"not null"`
	PlanRevision    int64  `gorm:"not null"`
	Phase           string `gorm:"size:40;not null"`
	SnapshotJSON    string `gorm:"type:text;not null"`
	GeneratedAt     time.Time
}

func (runtimeSnapshotRow) TableName() string { return "abot_agent_runtime_snapshots" }

type toolSetSnapshotRow struct {
	ID              string `gorm:"primaryKey;size:160"`
	InvocationID    string `gorm:"uniqueIndex;size:100;not null"`
	CatalogRevision string `gorm:"size:128;not null"`
	PolicyRevision  string `gorm:"size:128"`
	ModelProfile    string `gorm:"size:300"`
	ToolsJSON       string `gorm:"type:text;not null"`
	ExcludedJSON    string `gorm:"type:text"`
	Digest          string `gorm:"index;size:128;not null"`
	CreatedAt       time.Time
}

func (toolSetSnapshotRow) TableName() string { return "abot_agent_toolset_snapshots" }

type modelCapabilitySnapshotRow struct {
	ID           string `gorm:"primaryKey;size:180"`
	InvocationID string `gorm:"uniqueIndex;size:100;not null"`
	SnapshotJSON string `gorm:"type:text;not null"`
	ProfileID    string `gorm:"size:180;not null"`
	CreatedAt    time.Time
}

func (modelCapabilitySnapshotRow) TableName() string { return "abot_agent_model_capability_snapshots" }

type evalRunRow struct {
	ID                   string `gorm:"primaryKey;size:160"`
	InvocationID         string `gorm:"index;size:100;not null"`
	CaseID               string `gorm:"index;size:160;not null"`
	CaseVersion          string `gorm:"size:80;not null"`
	Kind                 string `gorm:"size:40;not null"`
	Status               string `gorm:"index;size:32;not null"`
	Passed               bool   `gorm:"not null"`
	HardFailure          bool   `gorm:"not null"`
	AssertionCount       int    `gorm:"not null"`
	FailedAssertions     int    `gorm:"not null"`
	HardFailedAssertions int    `gorm:"not null"`
	DefinitionJSON       string `gorm:"type:text;not null"`
	ResultJSON           string `gorm:"type:text;not null"`
	CreatedAt            time.Time
	FinishedAt           *time.Time
}

func (evalRunRow) TableName() string { return "abot_agent_eval_runs" }

type runtimeRepository struct{ db *gorm.DB }

func invocationResumeOutboxFromRow(row invocationResumeOutboxRow) agentruntime.InvocationResumeOutbox {
	return agentruntime.InvocationResumeOutbox{
		ID: row.ID, InvocationID: row.InvocationID, WaitID: row.WaitID, Name: row.Name,
		ResponseJSON: []byte(row.ResponseJSON), RequestDigest: row.RequestDigest,
		Status: agentruntime.InvocationResumeOutboxStatus(row.Status), Attempt: row.Attempt,
		Revision: row.Revision, AvailableAt: row.AvailableAt, LeaseOwner: row.LeaseOwner,
		LeaseExpiresAt: cloneTimePtr(row.LeaseExpiresAt), LastError: row.LastError,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func invocationResumeOutboxToRow(item agentruntime.InvocationResumeOutbox) *invocationResumeOutboxRow {
	return &invocationResumeOutboxRow{
		ID: item.ID, InvocationID: item.InvocationID, WaitID: item.WaitID, Name: item.Name,
		ResponseJSON: string(item.ResponseJSON), RequestDigest: item.RequestDigest,
		Status: string(item.Status), Attempt: item.Attempt, Revision: item.Revision,
		AvailableAt: item.AvailableAt, LeaseOwner: item.LeaseOwner,
		LeaseExpiresAt: cloneTimePtr(item.LeaseExpiresAt), LastError: item.LastError,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
}

func runtimeEventOutboxFromRow(row runtimeEventOutboxRow) agentruntime.RuntimeEventOutbox {
	return agentruntime.RuntimeEventOutbox{
		ID: row.ID, GroupID: row.GroupID, InvocationID: row.InvocationID, EventID: row.EventID,
		Sequence: row.Sequence, Type: row.Type,
		Status: agentruntime.RuntimeEventOutboxStatus(row.Status), Attempt: row.Attempt,
		Revision: row.Revision, AvailableAt: row.AvailableAt, LeaseOwner: row.LeaseOwner,
		LeaseExpiresAt: cloneTimePtr(row.LeaseExpiresAt), LastError: row.LastError,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func runtimeEventOutboxToRow(item agentruntime.RuntimeEventOutbox) *runtimeEventOutboxRow {
	return &runtimeEventOutboxRow{
		ID: item.ID, GroupID: item.GroupID, InvocationID: item.InvocationID, EventID: item.EventID,
		Sequence: item.Sequence, Type: item.Type, Status: string(item.Status),
		Attempt: item.Attempt, Revision: item.Revision, AvailableAt: item.AvailableAt,
		LeaseOwner: item.LeaseOwner, LeaseExpiresAt: cloneTimePtr(item.LeaseExpiresAt),
		LastError: item.LastError, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
}

func runtimeEventDeliveryInboxFromRow(row runtimeEventDeliveryInboxRow) agentruntime.RuntimeEventDeliveryInboxRecord {
	return agentruntime.RuntimeEventDeliveryInboxRecord{
		Version: row.Version, EventID: row.EventID, DeliveryID: row.DeliveryID, Source: row.Source, Destination: row.Destination,
		InvocationID: row.InvocationID, Sequence: row.Sequence, Type: row.Type, Timestamp: row.Timestamp, IssuedAt: row.IssuedAt, EventDigest: row.EventDigest,
		Signature: row.Signature, ReceivedAt: row.ReceivedAt,
	}
}

func runtimeEventDeliveryInboxToRow(item agentruntime.RuntimeEventDeliveryInboxRecord) *runtimeEventDeliveryInboxRow {
	return &runtimeEventDeliveryInboxRow{
		Version: item.Version, EventID: item.EventID, DeliveryID: item.DeliveryID, Source: item.Source, Destination: item.Destination,
		InvocationID: item.InvocationID, Sequence: item.Sequence, Type: item.Type, Timestamp: item.Timestamp, IssuedAt: item.IssuedAt, EventDigest: item.EventDigest,
		Signature: item.Signature, ReceivedAt: item.ReceivedAt,
	}
}

func runtimeCheckpointDeliveryInboxFromRow(row runtimeCheckpointDeliveryInboxRow) (agentruntime.RuntimeCheckpointDeliveryInboxRecord, error) {
	var projection agentruntime.RuntimeCheckpointProjection
	if err := json.Unmarshal([]byte(row.ProjectionJSON), &projection); err != nil {
		return agentruntime.RuntimeCheckpointDeliveryInboxRecord{}, err
	}
	return agentruntime.RuntimeCheckpointDeliveryInboxRecord{
		Version: row.Version, DeliveryID: row.DeliveryID, Source: row.Source, Destination: row.Destination,
		InvocationID: row.InvocationID, SnapshotRevision: row.SnapshotRevision, EventSequence: row.EventSequence,
		SnapshotDigest: row.SnapshotDigest, Signature: row.Signature, Projection: projection, ReceivedAt: row.ReceivedAt,
	}, nil
}

func runtimeCheckpointDeliveryInboxToRow(item agentruntime.RuntimeCheckpointDeliveryInboxRecord) (*runtimeCheckpointDeliveryInboxRow, error) {
	encoded, err := json.Marshal(item.Projection)
	if err != nil {
		return nil, err
	}
	if len(encoded) > agentruntime.MaxRuntimeCheckpointDeliveryProjectionBytes {
		return nil, fmt.Errorf("%w: projection 超过字节上限", agentruntime.ErrInvalidRuntimeCheckpointDelivery)
	}
	return &runtimeCheckpointDeliveryInboxRow{
		Version: item.Version, InvocationID: item.InvocationID, DeliveryID: item.DeliveryID, Source: item.Source,
		Destination: item.Destination, SnapshotRevision: item.SnapshotRevision, EventSequence: item.EventSequence,
		SnapshotDigest: item.SnapshotDigest, Signature: item.Signature, ProjectionJSON: string(encoded), ReceivedAt: item.ReceivedAt,
	}, nil
}

func runtimeCheckpointDeliveryOutboxFromRow(row runtimeCheckpointDeliveryOutboxRow) (agentruntime.RuntimeCheckpointDeliveryOutbox, error) {
	var projection agentruntime.RuntimeCheckpointProjection
	if err := json.Unmarshal([]byte(row.ProjectionJSON), &projection); err != nil {
		return agentruntime.RuntimeCheckpointDeliveryOutbox{}, err
	}
	return agentruntime.RuntimeCheckpointDeliveryOutbox{
		ID: row.ID, GroupID: row.GroupID, InvocationID: row.InvocationID, Source: row.Source, Destination: row.Destination,
		DeliveryID: row.DeliveryID, SnapshotRevision: row.SnapshotRevision, EventSequence: row.EventSequence,
		SnapshotDigest: row.SnapshotDigest, Projection: projection,
		Status: agentruntime.RuntimeCheckpointDeliveryOutboxStatus(row.Status), Attempt: row.Attempt,
		Revision: row.Revision, AvailableAt: row.AvailableAt, LeaseOwner: row.LeaseOwner,
		LeaseExpiresAt: cloneTimePtr(row.LeaseExpiresAt), LastError: row.LastError,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

func runtimeCheckpointDeliveryOutboxToRow(item agentruntime.RuntimeCheckpointDeliveryOutbox) (*runtimeCheckpointDeliveryOutboxRow, error) {
	encoded, err := json.Marshal(item.Projection)
	if err != nil {
		return nil, err
	}
	if len(encoded) > agentruntime.MaxRuntimeCheckpointDeliveryProjectionBytes {
		return nil, fmt.Errorf("%w: checkpoint outbox projection 超过字节上限", agentruntime.ErrInvalidRuntimeCheckpointDelivery)
	}
	return &runtimeCheckpointDeliveryOutboxRow{
		ID: item.ID, GroupID: item.GroupID, InvocationID: item.InvocationID, Source: item.Source, Destination: item.Destination,
		DeliveryID: item.DeliveryID, SnapshotRevision: item.SnapshotRevision, EventSequence: item.EventSequence,
		SnapshotDigest: item.SnapshotDigest, ProjectionJSON: string(encoded), Status: string(item.Status),
		Attempt: item.Attempt, Revision: item.Revision, AvailableAt: item.AvailableAt, LeaseOwner: item.LeaseOwner,
		LeaseExpiresAt: cloneTimePtr(item.LeaseExpiresAt), LastError: item.LastError,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}, nil
}

func createRuntimeEventOutboxTx(tx *gorm.DB, event agentruntime.AgentEvent) error {
	item, err := agentruntime.NewRuntimeEventOutbox(event)
	if err != nil {
		return err
	}
	return tx.Create(runtimeEventOutboxToRow(item)).Error
}

func cloneTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := value.UTC()
	return &copy
}

func sqliteResumeOutboxBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

func (r *runtimeRepository) SaveInvocationResume(ctx context.Context, item agentruntime.InvocationResume) (agentruntime.InvocationResume, error) {
	item.InvocationID = strings.TrimSpace(item.InvocationID)
	item.WaitID = strings.TrimSpace(item.WaitID)
	item.Name = strings.TrimSpace(item.Name)
	item.RequestDigest = strings.TrimSpace(item.RequestDigest)
	if item.InvocationID == "" || item.WaitID == "" || item.Name == "" || item.RequestDigest == "" || len(item.ResponseJSON) > 256<<10 {
		return agentruntime.InvocationResume{}, agentruntime.ErrInvalidResume
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	} else {
		item.CreatedAt = item.CreatedAt.UTC()
	}
	var saved agentruntime.InvocationResume
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var invocation invocationRow
		if err := tx.Where("id = ?", item.InvocationID).First(&invocation).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return err
		}
		var existing invocationResumeRow
		findErr := tx.Where("invocation_id = ?", item.InvocationID).First(&existing).Error
		if findErr == nil {
			if existing.WaitID != item.WaitID || existing.Name != item.Name || existing.RequestDigest != item.RequestDigest || existing.ResponseJSON != string(item.ResponseJSON) {
				return agentruntime.ErrConflict
			}
			saved = invocationResumeFromRow(existing)
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		row := &invocationResumeRow{ID: item.ID, InvocationID: item.InvocationID, WaitID: item.WaitID, Name: item.Name, ResponseJSON: string(item.ResponseJSON), RequestDigest: item.RequestDigest, CreatedAt: item.CreatedAt}
		if strings.TrimSpace(row.ID) == "" {
			row.ID = item.InvocationID + "-resume"
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row).Error; err != nil {
			return err
		}
		var persisted invocationResumeRow
		if err := tx.Where("invocation_id = ?", item.InvocationID).First(&persisted).Error; err != nil {
			return err
		}
		if persisted.WaitID != item.WaitID || persisted.Name != item.Name || persisted.RequestDigest != item.RequestDigest || persisted.ResponseJSON != string(item.ResponseJSON) {
			return agentruntime.ErrConflict
		}
		saved = invocationResumeFromRow(persisted)
		return nil
	})
	if err != nil {
		return agentruntime.InvocationResume{}, err
	}
	return saved, nil
}

// CommitInvocationResume atomically accepts one tool resume across the
// private handoff, durable outbox, Invocation CAS and resumed event. The
// event is inserted in the same SQLite transaction; the Coordinator publishes
// the returned row only after the commit succeeds.
func (r *runtimeRepository) CommitInvocationResume(ctx context.Context, commit agentruntime.InvocationResumeCommit) (agentruntime.Invocation, agentruntime.AgentEvent, error) {
	invocationID := strings.TrimSpace(commit.InvocationID)
	if invocationID == "" || strings.TrimSpace(commit.Resume.InvocationID) != invocationID || strings.TrimSpace(commit.Outbox.InvocationID) != invocationID {
		return agentruntime.Invocation{}, agentruntime.AgentEvent{}, agentruntime.ErrInvalidResume
	}
	if (commit.FromStatus != agentruntime.InvocationWaitingTool && commit.FromStatus != agentruntime.InvocationWaitingUser) || commit.ToStatus != agentruntime.InvocationQueued || !validRuntimeInvocationTransition(commit.FromStatus, commit.ToStatus) {
		return agentruntime.Invocation{}, agentruntime.AgentEvent{}, agentruntime.ErrConflict
	}
	resume := commit.Resume
	resume.InvocationID = invocationID
	resume.WaitID = strings.TrimSpace(resume.WaitID)
	resume.Name = strings.TrimSpace(resume.Name)
	resume.RequestDigest = strings.TrimSpace(resume.RequestDigest)
	if resume.WaitID == "" || resume.Name == "" || resume.RequestDigest == "" || len(resume.ResponseJSON) > 256<<10 {
		return agentruntime.Invocation{}, agentruntime.AgentEvent{}, agentruntime.ErrInvalidResume
	}
	outbox := commit.Outbox
	outbox.InvocationID = invocationID
	outbox.WaitID = strings.TrimSpace(outbox.WaitID)
	outbox.Name = strings.TrimSpace(outbox.Name)
	outbox.RequestDigest = strings.TrimSpace(outbox.RequestDigest)
	if outbox.RequestDigest != resume.RequestDigest || outbox.WaitID != resume.WaitID || outbox.Name != resume.Name || string(outbox.ResponseJSON) != string(resume.ResponseJSON) {
		return agentruntime.Invocation{}, agentruntime.AgentEvent{}, agentruntime.ErrConflict
	}
	now := time.Now().UTC()
	normalizedOutbox, err := outbox.Normalize(now)
	if err != nil {
		return agentruntime.Invocation{}, agentruntime.AgentEvent{}, err
	}
	if resume.CreatedAt.IsZero() {
		resume.CreatedAt = now
	} else {
		resume.CreatedAt = resume.CreatedAt.UTC()
	}
	event := commit.Event
	event.InvocationID = invocationID
	if strings.TrimSpace(event.ID) == "" {
		event.ID = "event-resume-" + resume.RequestDigest
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = now
	} else {
		event.Timestamp = event.Timestamp.UTC()
	}
	if event.Type == "" {
		event.Type = agentruntime.EventInvocationResumed
	}
	var savedInvocation agentruntime.Invocation
	var savedEvent agentruntime.AgentEvent
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var invocation invocationRow
		if findErr := tx.Where("id = ?", invocationID).First(&invocation).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		if agentruntime.InvocationStatus(invocation.Status) != commit.FromStatus {
			return agentruntime.ErrConflict
		}
		var existingResume invocationResumeRow
		findErr := tx.Where("invocation_id = ?", invocationID).First(&existingResume).Error
		if findErr == nil {
			if existingResume.WaitID != resume.WaitID || existingResume.Name != resume.Name || existingResume.RequestDigest != resume.RequestDigest || existingResume.ResponseJSON != string(resume.ResponseJSON) {
				return agentruntime.ErrConflict
			}
		} else if errors.Is(findErr, gorm.ErrRecordNotFound) {
			row := &invocationResumeRow{ID: resume.ID, InvocationID: invocationID, WaitID: resume.WaitID, Name: resume.Name, ResponseJSON: string(resume.ResponseJSON), RequestDigest: resume.RequestDigest, CreatedAt: resume.CreatedAt}
			if row.ID == "" {
				row.ID = invocationID + "-resume"
			}
			if createErr := tx.Create(row).Error; createErr != nil {
				return createErr
			}
		} else {
			return findErr
		}
		var existingOutbox invocationResumeOutboxRow
		findErr = tx.Where("invocation_id = ? AND request_digest = ?", invocationID, normalizedOutbox.RequestDigest).First(&existingOutbox).Error
		if findErr == nil {
			if existingOutbox.WaitID != normalizedOutbox.WaitID || existingOutbox.Name != normalizedOutbox.Name || existingOutbox.ResponseJSON != string(normalizedOutbox.ResponseJSON) {
				return agentruntime.ErrConflict
			}
		} else if errors.Is(findErr, gorm.ErrRecordNotFound) {
			row := invocationResumeOutboxToRow(normalizedOutbox)
			if row.ID == "" {
				row.ID = invocationID + "-resume-outbox-" + normalizedOutbox.RequestDigest
			}
			if createErr := tx.Create(row).Error; createErr != nil {
				return createErr
			}
		} else {
			return findErr
		}
		updates := map[string]any{"status": string(commit.ToStatus), "error": commit.Message, "lease_owner": "", "lease_expires_at": nil, "updated_at": now}
		updated := tx.Model(&invocationRow{}).Where("id = ? AND status = ?", invocationID, string(commit.FromStatus)).Updates(updates)
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return agentruntime.ErrConflict
		}
		var last agentEventRow
		lastErr := tx.Where("invocation_id = ?", invocationID).Order("sequence DESC").First(&last).Error
		if lastErr != nil && !errors.Is(lastErr, gorm.ErrRecordNotFound) {
			return lastErr
		}
		event.Sequence = last.Sequence + 1
		data, marshalErr := json.Marshal(event.Data)
		if marshalErr != nil {
			return marshalErr
		}
		if createErr := tx.Create(&agentEventRow{ID: event.ID, InvocationID: invocationID, Sequence: event.Sequence, Type: event.Type, Timestamp: event.Timestamp, DataJSON: string(data)}).Error; createErr != nil {
			return createErr
		}
		if outboxErr := createRuntimeEventOutboxTx(tx, event); outboxErr != nil {
			return outboxErr
		}
		invocation.Status = string(commit.ToStatus)
		invocation.Error = commit.Message
		invocation.LeaseOwner = ""
		invocation.LeaseExpiresAt = nil
		invocation.UpdatedAt = now
		savedInvocation = invocationFromRow(invocation)
		savedEvent = event
		return nil
	})
	if err != nil {
		return agentruntime.Invocation{}, agentruntime.AgentEvent{}, err
	}
	return savedInvocation, savedEvent, nil
}

// CommitApprovalResume atomically accepts a confirmation across the
// Runtime-owned entities. Workspace Operation sidecars are intentionally
// outside this repository transaction; rejection uses
// CommitApprovalRejection when the local sidecar must be included.
func (r *runtimeRepository) CommitApprovalResume(ctx context.Context, commit agentruntime.ApprovalResumeCommit) (agentruntime.Approval, agentruntime.Invocation, []agentruntime.AgentEvent, error) {
	return r.commitApprovalResume(ctx, commit, false)
}

// CommitApprovalRejection atomically resolves a rejected confirmation with the
// Workspace Operation referenced by the Approval. A queued CommandRun is
// cancelled in the same transaction, and its lifecycle event is included in
// the returned ordered Runtime events. This method deliberately never touches
// a starting/running process: an already-active side effect remains governed
// by the Workspace command-run recovery rules.
func (r *runtimeRepository) CommitApprovalRejection(ctx context.Context, commit agentruntime.ApprovalResumeCommit) (agentruntime.Approval, agentruntime.Invocation, []agentruntime.AgentEvent, error) {
	if commit.ApprovalToStatus != agentruntime.ApprovalRejected || commit.ToolCallStatus != agentruntime.ToolCallRejected {
		return agentruntime.Approval{}, agentruntime.Invocation{}, nil, agentruntime.ErrConflict
	}
	return r.commitApprovalResume(ctx, commit, true)
}

func (r *runtimeRepository) commitApprovalResume(ctx context.Context, commit agentruntime.ApprovalResumeCommit, rejectWorkspaceSidecar bool) (agentruntime.Approval, agentruntime.Invocation, []agentruntime.AgentEvent, error) {
	approvalID := strings.TrimSpace(commit.ApprovalID)
	invocationID := strings.TrimSpace(commit.InvocationID)
	if approvalID == "" || invocationID == "" || strings.TrimSpace(commit.Resume.InvocationID) != invocationID || strings.TrimSpace(commit.Outbox.InvocationID) != invocationID {
		return agentruntime.Approval{}, agentruntime.Invocation{}, nil, agentruntime.ErrInvalidResume
	}
	if commit.ApprovalFromStatus != agentruntime.ApprovalPending || (commit.ApprovalToStatus != agentruntime.ApprovalApproved && commit.ApprovalToStatus != agentruntime.ApprovalRejected) {
		return agentruntime.Approval{}, agentruntime.Invocation{}, nil, agentruntime.ErrConflict
	}
	if commit.InvocationFromStatus != agentruntime.InvocationWaitingApproval || commit.InvocationToStatus != agentruntime.InvocationQueued || !validRuntimeInvocationTransition(commit.InvocationFromStatus, commit.InvocationToStatus) {
		return agentruntime.Approval{}, agentruntime.Invocation{}, nil, agentruntime.ErrConflict
	}
	resume := commit.Resume
	resume.InvocationID = invocationID
	resume.WaitID = strings.TrimSpace(resume.WaitID)
	resume.Name = strings.TrimSpace(resume.Name)
	resume.RequestDigest = strings.TrimSpace(resume.RequestDigest)
	if resume.WaitID == "" || resume.Name == "" || resume.RequestDigest == "" || len(resume.ResponseJSON) > 256<<10 {
		return agentruntime.Approval{}, agentruntime.Invocation{}, nil, agentruntime.ErrInvalidResume
	}
	outbox := commit.Outbox
	outbox.InvocationID = invocationID
	outbox.WaitID = strings.TrimSpace(outbox.WaitID)
	outbox.Name = strings.TrimSpace(outbox.Name)
	outbox.RequestDigest = strings.TrimSpace(outbox.RequestDigest)
	if outbox.RequestDigest != resume.RequestDigest || outbox.WaitID != resume.WaitID || outbox.Name != resume.Name || string(outbox.ResponseJSON) != string(resume.ResponseJSON) {
		return agentruntime.Approval{}, agentruntime.Invocation{}, nil, agentruntime.ErrConflict
	}
	now := time.Now().UTC()
	normalizedOutbox, err := outbox.Normalize(now)
	if err != nil {
		return agentruntime.Approval{}, agentruntime.Invocation{}, nil, err
	}
	var normalizedRejectionDelivery *agentruntime.RuntimeApprovalRejectionDeliveryOutbox
	if commit.RejectionDelivery != nil {
		if commit.ApprovalToStatus != agentruntime.ApprovalRejected || commit.ToolCallStatus != agentruntime.ToolCallRejected {
			return agentruntime.Approval{}, agentruntime.Invocation{}, nil, agentruntime.ErrConflict
		}
		delivery := *commit.RejectionDelivery
		if strings.TrimSpace(delivery.ApprovalID) != approvalID || strings.TrimSpace(delivery.InvocationID) != invocationID || strings.TrimSpace(delivery.ToolCallID) != strings.TrimSpace(commit.ToolCallID) {
			return agentruntime.Approval{}, agentruntime.Invocation{}, nil, agentruntime.ErrConflict
		}
		computedReasonDigest, digestErr := agentruntime.RuntimeApprovalRejectionDeliveryReasonDigest(commit.Reason)
		if digestErr != nil || strings.TrimSpace(strings.ToLower(delivery.ReasonDigest)) != computedReasonDigest {
			return agentruntime.Approval{}, agentruntime.Invocation{}, nil, agentruntime.ErrConflict
		}
		normalized, deliveryErr := delivery.Normalize(now)
		if deliveryErr != nil {
			return agentruntime.Approval{}, agentruntime.Invocation{}, nil, deliveryErr
		}
		normalizedRejectionDelivery = &normalized
	}
	if resume.CreatedAt.IsZero() {
		resume.CreatedAt = now
	} else {
		resume.CreatedAt = resume.CreatedAt.UTC()
	}
	if len(commit.Events) == 0 || len(commit.Events) > 8 {
		return agentruntime.Approval{}, agentruntime.Invocation{}, nil, agentruntime.ErrConflict
	}
	var savedApproval agentruntime.Approval
	var savedInvocation agentruntime.Invocation
	savedEvents := make([]agentruntime.AgentEvent, 0, len(commit.Events))
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var approval approvalRow
		if findErr := tx.Where("id = ?", approvalID).First(&approval).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		if approval.InvocationID != invocationID || agentruntime.ApprovalStatus(approval.Status) != commit.ApprovalFromStatus {
			return agentruntime.ErrConflict
		}
		// Re-check the immutable approval/tool metadata inside the same
		// transaction that writes the rejection outbox. The caller-side
		// validation protects normal requests, but this row-level check keeps a
		// stale or forged delivery from being committed if approval metadata
		// changes between the read and the CAS.
		if normalizedRejectionDelivery != nil && (strings.TrimSpace(approval.ToolCallID) != normalizedRejectionDelivery.ToolCallID || strings.TrimSpace(approval.OperationID) != normalizedRejectionDelivery.OperationID) {
			return agentruntime.ErrConflict
		}
		var invocation invocationRow
		if findErr := tx.Where("id = ?", invocationID).First(&invocation).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		if agentruntime.InvocationStatus(invocation.Status) != commit.InvocationFromStatus {
			return agentruntime.ErrConflict
		}
		if normalizedRejectionDelivery != nil {
			if err := createRuntimeApprovalRejectionDeliveryOutboxTx(tx, *normalizedRejectionDelivery); err != nil {
				return err
			}
		}
		var toolCall toolCallRow
		hasToolCall := strings.TrimSpace(commit.ToolCallID) != ""
		if hasToolCall {
			if findErr := tx.Where("id = ?", strings.TrimSpace(commit.ToolCallID)).First(&toolCall).Error; findErr != nil {
				if errors.Is(findErr, gorm.ErrRecordNotFound) {
					return agentruntime.ErrNotFound
				}
				return findErr
			}
			if toolCall.InvocationID != invocationID || (commit.ToolCallStatus != agentruntime.ToolCallApproved && commit.ToolCallStatus != agentruntime.ToolCallRejected) {
				return agentruntime.ErrConflict
			}
		}
		var sidecarEvents []agentruntime.AgentEvent
		if rejectWorkspaceSidecar {
			event, rejectErr := rejectWorkspaceOperationTx(tx, approval, invocationID, now)
			if rejectErr != nil {
				return rejectErr
			}
			if event != nil {
				sidecarEvents = append(sidecarEvents, *event)
			}
		}
		var existingResume invocationResumeRow
		findErr := tx.Where("invocation_id = ?", invocationID).First(&existingResume).Error
		if findErr == nil {
			if existingResume.WaitID != resume.WaitID || existingResume.Name != resume.Name || existingResume.RequestDigest != resume.RequestDigest || existingResume.ResponseJSON != string(resume.ResponseJSON) {
				return agentruntime.ErrConflict
			}
		} else if errors.Is(findErr, gorm.ErrRecordNotFound) {
			row := &invocationResumeRow{ID: resume.ID, InvocationID: invocationID, WaitID: resume.WaitID, Name: resume.Name, ResponseJSON: string(resume.ResponseJSON), RequestDigest: resume.RequestDigest, CreatedAt: resume.CreatedAt}
			if row.ID == "" {
				row.ID = invocationID + "-resume"
			}
			if createErr := tx.Create(row).Error; createErr != nil {
				return createErr
			}
		} else {
			return findErr
		}
		var existingOutbox invocationResumeOutboxRow
		findErr = tx.Where("invocation_id = ? AND request_digest = ?", invocationID, normalizedOutbox.RequestDigest).First(&existingOutbox).Error
		if findErr == nil {
			if existingOutbox.WaitID != normalizedOutbox.WaitID || existingOutbox.Name != normalizedOutbox.Name || existingOutbox.ResponseJSON != string(normalizedOutbox.ResponseJSON) {
				return agentruntime.ErrConflict
			}
		} else if errors.Is(findErr, gorm.ErrRecordNotFound) {
			row := invocationResumeOutboxToRow(normalizedOutbox)
			if row.ID == "" {
				row.ID = invocationID + "-resume-outbox-" + normalizedOutbox.RequestDigest
			}
			if createErr := tx.Create(row).Error; createErr != nil {
				return createErr
			}
		} else {
			return findErr
		}
		resolvedAt := now
		updated := tx.Model(&approvalRow{}).Where("id = ? AND status = ?", approvalID, string(commit.ApprovalFromStatus)).Updates(map[string]any{
			"status": string(commit.ApprovalToStatus), "decision_reason": strings.TrimSpace(commit.Reason), "updated_at": now, "resolved_at": &resolvedAt,
		})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return agentruntime.ErrConflict
		}
		if hasToolCall {
			finishedAt := (*time.Time)(nil)
			if commit.ToolCallStatus.Terminal() {
				value := now
				finishedAt = &value
			}
			updates := map[string]any{"status": string(commit.ToolCallStatus), "updated_at": now, "finished_at": finishedAt}
			if strings.TrimSpace(commit.Reason) != "" {
				updates["error"] = strings.TrimSpace(commit.Reason)
			}
			updated = tx.Model(&toolCallRow{}).Where("id = ?", strings.TrimSpace(commit.ToolCallID)).Updates(updates)
			if updated.Error != nil {
				return updated.Error
			}
			if updated.RowsAffected != 1 {
				return agentruntime.ErrConflict
			}
		}
		updated = tx.Model(&invocationRow{}).Where("id = ? AND status = ?", invocationID, string(commit.InvocationFromStatus)).Updates(map[string]any{
			"status": string(commit.InvocationToStatus), "error": "", "active_approval_id": "", "lease_owner": "", "lease_expires_at": nil, "updated_at": now,
		})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return agentruntime.ErrConflict
		}
		eventInputs := commit.Events
		if len(sidecarEvents) > 0 {
			eventInputs = append(append([]agentruntime.AgentEvent(nil), sidecarEvents...), eventInputs...)
		}
		if len(eventInputs) == 0 || len(eventInputs) > 8 {
			return agentruntime.ErrConflict
		}
		var last agentEventRow
		lastErr := tx.Where("invocation_id = ?", invocationID).Order("sequence DESC").First(&last).Error
		if lastErr != nil && !errors.Is(lastErr, gorm.ErrRecordNotFound) {
			return lastErr
		}
		sequence := last.Sequence
		persistedEventOutboxes := make([]agentruntime.RuntimeEventOutbox, 0, len(eventInputs))
		for _, input := range eventInputs {
			event := input
			event.InvocationID = invocationID
			if strings.TrimSpace(event.ID) == "" {
				event.ID = "event-approval-" + resume.RequestDigest
			}
			if event.Timestamp.IsZero() {
				event.Timestamp = now
			} else {
				event.Timestamp = event.Timestamp.UTC()
			}
			if strings.TrimSpace(event.Type) == "" {
				return agentruntime.ErrConflict
			}
			sequence++
			event.Sequence = sequence
			data, marshalErr := json.Marshal(event.Data)
			if marshalErr != nil {
				return marshalErr
			}
			if createErr := tx.Create(&agentEventRow{ID: event.ID, InvocationID: invocationID, Sequence: sequence, Type: event.Type, Timestamp: event.Timestamp, DataJSON: string(data)}).Error; createErr != nil {
				return createErr
			}
			eventOutbox, outboxErr := agentruntime.NewRuntimeEventOutbox(event)
			if outboxErr != nil {
				return outboxErr
			}
			if outboxErr := createRuntimeEventOutboxTx(tx, event); outboxErr != nil {
				return outboxErr
			}
			persistedEventOutboxes = append(persistedEventOutboxes, eventOutbox)
			savedEvents = append(savedEvents, event)
		}
		if normalizedRejectionDelivery != nil {
			members := make([]agentruntime.RuntimeDeliveryGroupMember, 0, len(persistedEventOutboxes)+1)
			for index, outbox := range persistedEventOutboxes {
				members = append(members, agentruntime.RuntimeDeliveryGroupMember{Kind: agentruntime.RuntimeDeliveryKindEvent, OutboxID: outbox.ID, DeliveryID: savedEvents[index].ID})
			}
			members = append(members, agentruntime.RuntimeDeliveryGroupMember{Kind: agentruntime.RuntimeDeliveryKindRejection, OutboxID: normalizedRejectionDelivery.ID, DeliveryID: normalizedRejectionDelivery.DeliveryID})
			deliveryGroup, groupErr := agentruntime.NewRuntimeDeliveryGroup(normalizedRejectionDelivery.Source, normalizedRejectionDelivery.Destination, invocationID, members, now)
			if groupErr != nil {
				return groupErr
			}
			if err := ensureRuntimeDeliveryGroupTx(tx, deliveryGroup); err != nil {
				return err
			}
			for _, outbox := range persistedEventOutboxes {
				if updateErr := tx.Model(&runtimeEventOutboxRow{}).Where("id = ?", outbox.ID).Update("group_id", deliveryGroup.ID).Error; updateErr != nil {
					return updateErr
				}
			}
			if updateErr := tx.Model(&runtimeApprovalRejectionDeliveryOutboxRow{}).Where("id = ?", normalizedRejectionDelivery.ID).Update("group_id", deliveryGroup.ID).Error; updateErr != nil {
				return updateErr
			}
		}
		var persistedApproval approvalRow
		if readErr := tx.Where("id = ?", approvalID).First(&persistedApproval).Error; readErr != nil {
			return readErr
		}
		var persistedInvocation invocationRow
		if readErr := tx.Where("id = ?", invocationID).First(&persistedInvocation).Error; readErr != nil {
			return readErr
		}
		savedApproval = approvalFromRow(persistedApproval)
		savedInvocation = invocationFromRow(persistedInvocation)
		return nil
	})
	if err != nil {
		return agentruntime.Approval{}, agentruntime.Invocation{}, nil, err
	}
	return savedApproval, savedInvocation, savedEvents, nil
}

// rejectWorkspaceOperationTx mirrors workspace.Service.RejectOperation while
// staying inside the Runtime transaction. The operation row is the source of
// truth for the sidecar; a queued CommandRun is cancelled as part of the same
// write. No process control is attempted for starting/running rows because a
// lost control handle is an unknown side-effect boundary, not a safe retry.
func rejectWorkspaceOperationTx(tx *gorm.DB, approval approvalRow, invocationID string, now time.Time) (*agentruntime.AgentEvent, error) {
	operationID := strings.TrimSpace(approval.OperationID)
	if operationID == "" {
		return nil, nil
	}
	var operation workspaceOperationRow
	if err := tx.Where("id = ?", operationID).First(&operation).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, agentruntime.ErrNotFound
		}
		return nil, err
	}
	if strings.TrimSpace(operation.InvocationID) != "" && strings.TrimSpace(operation.InvocationID) != strings.TrimSpace(invocationID) {
		return nil, agentruntime.ErrConflict
	}
	if strings.TrimSpace(operation.ToolCallID) != "" && strings.TrimSpace(approval.ToolCallID) != "" && strings.TrimSpace(operation.ToolCallID) != strings.TrimSpace(approval.ToolCallID) {
		return nil, agentruntime.ErrConflict
	}
	switch workspace.OperationStatus(operation.Status) {
	case workspace.OperationRejected:
		// A Runtime retry may find a sidecar that was written by an older
		// sidecar-first process. Keep the retry idempotent and still repair a
		// queued CommandRun below if one was left behind.
	case workspace.OperationPending, workspace.OperationPrepared:
		updated := tx.Model(&workspaceOperationRow{}).Where("id = ? AND status IN ?", operationID, []string{string(workspace.OperationPending), string(workspace.OperationPrepared)}).Updates(map[string]any{
			"status": string(workspace.OperationRejected), "result": "用户已拒绝", "updated_at": now,
		})
		if updated.Error != nil {
			return nil, updated.Error
		}
		if updated.RowsAffected != 1 {
			return nil, agentruntime.ErrConflict
		}
	default:
		return nil, workspace.ErrOperationState
	}
	if workspace.OperationType(operation.Type) != workspace.OperationCommand || strings.TrimSpace(operation.CommandRunID) == "" {
		return nil, nil
	}
	var run workspaceCommandRunRow
	if err := tx.Where("id = ?", strings.TrimSpace(operation.CommandRunID)).First(&run).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// workspace.Service treats an absent optional CommandRun as a
			// legacy operation and still rejects the operation itself.
			return nil, nil
		}
		return nil, err
	}
	if strings.TrimSpace(run.OperationID) != "" && strings.TrimSpace(run.OperationID) != operationID {
		return nil, agentruntime.ErrConflict
	}
	if strings.TrimSpace(run.InvocationID) != "" && strings.TrimSpace(run.InvocationID) != strings.TrimSpace(invocationID) {
		return nil, agentruntime.ErrConflict
	}
	if strings.TrimSpace(run.ToolCallID) != "" && strings.TrimSpace(approval.ToolCallID) != "" && strings.TrimSpace(run.ToolCallID) != strings.TrimSpace(approval.ToolCallID) {
		return nil, agentruntime.ErrConflict
	}
	if workspace.CommandRunStatus(run.Status) != workspace.CommandRunQueued {
		return nil, nil
	}
	revision := run.Revision + 1
	finishedAt := now
	retentionState := workspace.CommandOutputRetentionState(run.OutputRetentionState)
	retainedUntil := run.OutputRetainedUntil
	if retentionState != workspace.CommandOutputRetentionPurged {
		retentionState = workspace.CommandOutputRetentionRetained
		if retainedUntil == nil {
			deadline := now.Add(workspace.DefaultCommandOutputRetentionAge)
			retainedUntil = &deadline
		}
	}
	exitCode := -1
	const cancelReason = "用户已拒绝命令执行"
	updated := tx.Model(&workspaceCommandRunRow{}).Where("id = ? AND revision = ? AND status = ?", run.ID, run.Revision, string(workspace.CommandRunQueued)).Updates(map[string]any{
		"status": string(workspace.CommandRunCancelled), "outcome": string(workspace.CommandOutcomeCancelled), "exit_code": exitCode,
		"error": cancelReason, "revision": revision, "finished_at": finishedAt, "updated_at": now,
		"stdout_bytes": 0, "stderr_bytes": 0, "stored_bytes": 0, "output_digest": "", "output_truncated": false,
		"output_retention_state": string(retentionState), "output_retained_until": retainedUntil,
	})
	if updated.Error != nil {
		return nil, updated.Error
	}
	if updated.RowsAffected != 1 {
		return nil, agentruntime.ErrConflict
	}
	data := map[string]any{
		"command_run_id": run.ID, "operation_id": operationID, "executor": run.Executor, "mode": run.Mode,
		"cwd": run.CWD, "timeout_ms": run.TimeoutMS, "status": string(workspace.CommandRunCancelled),
		"outcome": string(workspace.CommandOutcomeCancelled), "stdout_bytes": run.StdoutBytes, "stderr_bytes": run.StderrBytes,
		"stored_bytes": run.StoredBytes, "output_digest": run.OutputDigest, "output_truncated": run.OutputTruncated,
		"revision": revision, "exit_code": exitCode, "error": cancelReason,
	}
	if capabilities := unmarshalCommandExecutionCapabilities(run.CapabilitiesJSON, run.Executor); capabilities != nil {
		// Keep the sidecar-generated lifecycle event identical to the normal
		// CommandRun projection, including its conservative safety disclosure.
		data["capabilities"] = *capabilities
	}
	return &agentruntime.AgentEvent{
		ID: fmt.Sprintf("event-rejection-command-%s-%d", run.ID, revision), InvocationID: invocationID,
		Type: agentruntime.EventCommandFailed, Timestamp: now,
		Data: data,
	}, nil
}

// applyRuntimeApprovalRejectionWorkspaceTx applies a destination-side
// rejection intent without requiring the source Approval row to exist in this
// Runtime database. The existing sidecar helper deliberately receives only
// the immutable operation/tool/invocation identifiers from the signed
// envelope; it never copies the human rejection reason across the boundary.
func applyRuntimeApprovalRejectionWorkspaceTx(tx *gorm.DB, envelope agentruntime.RuntimeApprovalRejectionDeliveryEnvelope, now time.Time) error {
	if strings.TrimSpace(envelope.OperationID) == "" {
		return nil
	}
	_, err := rejectWorkspaceOperationTx(tx, approvalRow{OperationID: envelope.OperationID, ToolCallID: envelope.ToolCallID}, envelope.InvocationID, now)
	return err
}

func (r *runtimeRepository) GetInvocationResume(ctx context.Context, invocationID string) (agentruntime.InvocationResume, error) {
	var row invocationResumeRow
	if err := r.db.WithContext(ctx).Where("invocation_id = ?", strings.TrimSpace(invocationID)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.InvocationResume{}, agentruntime.ErrNotFound
		}
		return agentruntime.InvocationResume{}, err
	}
	return invocationResumeFromRow(row), nil
}

func (r *runtimeRepository) DeleteInvocationResume(ctx context.Context, invocationID string) error {
	result := r.db.WithContext(ctx).Where("invocation_id = ?", strings.TrimSpace(invocationID)).Delete(&invocationResumeRow{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return agentruntime.ErrNotFound
	}
	return nil
}

// EnqueueInvocationResumeOutbox stores the private resume payload and its
// delivery metadata. The composite invocation/digest key makes accepted
// retries idempotent while still allowing a later waiting boundary to create
// a new delivery for the same Invocation.
func (r *runtimeRepository) EnqueueInvocationResumeOutbox(ctx context.Context, item agentruntime.InvocationResumeOutbox) (agentruntime.InvocationResumeOutbox, error) {
	normalized, err := item.Normalize(time.Now().UTC())
	if err != nil {
		return agentruntime.InvocationResumeOutbox{}, err
	}
	var saved agentruntime.InvocationResumeOutbox
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var invocation invocationRow
		if findErr := tx.Where("id = ?", normalized.InvocationID).First(&invocation).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		var existing invocationResumeOutboxRow
		findErr := tx.Where("invocation_id = ? AND request_digest = ?", normalized.InvocationID, normalized.RequestDigest).First(&existing).Error
		if findErr == nil {
			if existing.WaitID != normalized.WaitID || existing.Name != normalized.Name || existing.ResponseJSON != string(normalized.ResponseJSON) {
				return agentruntime.ErrConflict
			}
			saved = invocationResumeOutboxFromRow(existing)
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		row := invocationResumeOutboxToRow(normalized)
		if row.ID == "" {
			row.ID = normalized.InvocationID + "-resume-outbox-" + normalized.RequestDigest
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row).Error; err != nil {
			return err
		}
		var persisted invocationResumeOutboxRow
		if err := tx.Where("invocation_id = ? AND request_digest = ?", normalized.InvocationID, normalized.RequestDigest).First(&persisted).Error; err != nil {
			return err
		}
		if persisted.WaitID != normalized.WaitID || persisted.Name != normalized.Name || persisted.ResponseJSON != string(normalized.ResponseJSON) {
			return agentruntime.ErrConflict
		}
		saved = invocationResumeOutboxFromRow(persisted)
		return nil
	})
	if err != nil {
		return agentruntime.InvocationResumeOutbox{}, err
	}
	return saved, nil
}

func (r *runtimeRepository) GetInvocationResumeOutbox(ctx context.Context, invocationID string) (agentruntime.InvocationResumeOutbox, error) {
	var row invocationResumeOutboxRow
	if err := r.db.WithContext(ctx).Where("invocation_id = ?", strings.TrimSpace(invocationID)).Order("created_at DESC, id DESC").First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.InvocationResumeOutbox{}, agentruntime.ErrNotFound
		}
		return agentruntime.InvocationResumeOutbox{}, err
	}
	return invocationResumeOutboxFromRow(row), nil
}

// ClaimInvocationResumeOutbox atomically claims the newest eligible delivery.
// Expired processing leases are reclaimable; live leases are never stolen.
func (r *runtimeRepository) ClaimInvocationResumeOutbox(ctx context.Context, invocationID, owner string, now time.Time, ttl time.Duration) (agentruntime.InvocationResumeOutbox, bool, error) {
	invocationID = strings.TrimSpace(invocationID)
	owner = strings.TrimSpace(owner)
	if invocationID == "" || owner == "" || len(owner) > agentruntime.MaxInvocationResumeOutboxOwnerLength || ttl <= 0 || ttl > agentruntime.MaxInvocationResumeOutboxLease {
		return agentruntime.InvocationResumeOutbox{}, false, agentruntime.ErrConflict
	}
	now = now.UTC()
	expires := now.Add(ttl)
	var claimed agentruntime.InvocationResumeOutbox
	claimedOK := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var invocation invocationRow
		if findErr := tx.Where("id = ?", invocationID).First(&invocation).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		var row invocationResumeOutboxRow
		query := tx.Where("invocation_id = ? AND ((status = ? AND available_at <= ?) OR (status = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?)))", invocationID, string(agentruntime.InvocationResumeOutboxQueued), now, string(agentruntime.InvocationResumeOutboxProcessing), now).Order("created_at DESC, id DESC").First(&row)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return nil
		}
		if query.Error != nil {
			return query.Error
		}
		if row.Status == string(agentruntime.InvocationResumeOutboxProcessing) && (strings.TrimSpace(row.LeaseOwner) == "" || row.LeaseExpiresAt == nil) {
			updated := tx.Model(&invocationResumeOutboxRow{}).Where("id = ? AND revision = ?", row.ID, row.Revision).Updates(map[string]any{
				"status": string(agentruntime.InvocationResumeOutboxFailed), "lease_owner": "", "lease_expires_at": nil,
				"last_error": "resume outbox processing 元数据无效", "revision": row.Revision + 1, "updated_at": now,
			})
			return updated.Error
		}
		if row.Attempt >= agentruntime.MaxInvocationResumeOutboxAttempts {
			updated := tx.Model(&invocationResumeOutboxRow{}).Where("id = ? AND revision = ?", row.ID, row.Revision).Updates(map[string]any{
				"status": string(agentruntime.InvocationResumeOutboxFailed), "lease_owner": "", "lease_expires_at": nil,
				"last_error": "resume outbox 达到最大投递次数", "revision": row.Revision + 1, "updated_at": now,
			})
			return updated.Error
		}
		result := tx.Model(&invocationResumeOutboxRow{}).Where("id = ? AND revision = ? AND ((status = ? AND available_at <= ?) OR (status = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?)))", row.ID, row.Revision, string(agentruntime.InvocationResumeOutboxQueued), now, string(agentruntime.InvocationResumeOutboxProcessing), now).Updates(map[string]any{
			"status": string(agentruntime.InvocationResumeOutboxProcessing), "attempt": row.Attempt + 1,
			"lease_owner": owner, "lease_expires_at": &expires, "revision": row.Revision + 1, "updated_at": now,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}
		row.Status = string(agentruntime.InvocationResumeOutboxProcessing)
		row.Attempt++
		row.LeaseOwner = owner
		row.LeaseExpiresAt = &expires
		row.Revision++
		row.UpdatedAt = now
		claimed = invocationResumeOutboxFromRow(row)
		claimedOK = true
		return nil
	})
	if err != nil {
		return agentruntime.InvocationResumeOutbox{}, false, err
	}
	return claimed, claimedOK, nil
}

func (r *runtimeRepository) CompleteInvocationResumeOutbox(ctx context.Context, invocationID, owner string, now time.Time) (bool, error) {
	invocationID = strings.TrimSpace(invocationID)
	owner = strings.TrimSpace(owner)
	if invocationID == "" || owner == "" || len(owner) > agentruntime.MaxInvocationResumeOutboxOwnerLength {
		return false, agentruntime.ErrConflict
	}
	var row invocationResumeOutboxRow
	query := r.db.WithContext(ctx).Where("invocation_id = ? AND status = ? AND lease_owner = ?", invocationID, string(agentruntime.InvocationResumeOutboxProcessing), owner).Order("created_at DESC, id DESC").First(&row)
	if errors.Is(query.Error, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if query.Error != nil {
		return false, query.Error
	}
	now = now.UTC()
	result := r.db.WithContext(ctx).Model(&invocationResumeOutboxRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", row.ID, row.Revision, string(agentruntime.InvocationResumeOutboxProcessing), owner, now).Updates(map[string]any{
		"status": string(agentruntime.InvocationResumeOutboxCompleted), "lease_owner": "", "lease_expires_at": nil, "revision": row.Revision + 1, "updated_at": now,
	})
	return result.RowsAffected == 1, result.Error
}

func (r *runtimeRepository) RetryInvocationResumeOutbox(ctx context.Context, invocationID, owner string, now time.Time, message string) (bool, error) {
	invocationID = strings.TrimSpace(invocationID)
	owner = strings.TrimSpace(owner)
	if invocationID == "" || owner == "" || len(owner) > agentruntime.MaxInvocationResumeOutboxOwnerLength {
		return false, agentruntime.ErrConflict
	}
	var row invocationResumeOutboxRow
	query := r.db.WithContext(ctx).Where("invocation_id = ? AND status = ? AND lease_owner = ?", invocationID, string(agentruntime.InvocationResumeOutboxProcessing), owner).Order("created_at DESC, id DESC").First(&row)
	if errors.Is(query.Error, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if query.Error != nil {
		return false, query.Error
	}
	now = now.UTC()
	message = strings.TrimSpace(message)
	if len(message) > 4096 {
		message = message[:4096]
	}
	status := agentruntime.InvocationResumeOutboxQueued
	availableAt := now.Add(sqliteResumeOutboxBackoff(row.Attempt))
	if row.Attempt >= agentruntime.MaxInvocationResumeOutboxAttempts {
		status = agentruntime.InvocationResumeOutboxFailed
		availableAt = now
	}
	result := r.db.WithContext(ctx).Model(&invocationResumeOutboxRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", row.ID, row.Revision, string(agentruntime.InvocationResumeOutboxProcessing), owner, now).Updates(map[string]any{
		"status": string(status), "available_at": availableAt, "lease_owner": "", "lease_expires_at": nil,
		"last_error": message, "revision": row.Revision + 1, "updated_at": now,
	})
	return result.RowsAffected == 1, result.Error
}

func (r *runtimeRepository) DeleteInvocationResumeOutbox(ctx context.Context, invocationID string) error {
	result := r.db.WithContext(ctx).Where("invocation_id = ?", strings.TrimSpace(invocationID)).Delete(&invocationResumeOutboxRow{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return agentruntime.ErrNotFound
	}
	return nil
}

func (r *runtimeRepository) CreateInvocation(ctx context.Context, item agentruntime.Invocation) error {
	return r.db.WithContext(ctx).Create(invocationToRow(item)).Error
}

func (r *runtimeRepository) GetInvocation(ctx context.Context, id string) (agentruntime.Invocation, error) {
	var row invocationRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.Invocation{}, agentruntime.ErrNotFound
		}
		return agentruntime.Invocation{}, err
	}
	return invocationFromRow(row), nil
}

func (r *runtimeRepository) GetWorkspaceBaseline(ctx context.Context, invocationID string) (agentruntime.WorktreeBaseline, error) {
	var row worktreeBaselineRow
	if err := r.db.WithContext(ctx).Where("invocation_id = ?", strings.TrimSpace(invocationID)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.WorktreeBaseline{}, agentruntime.ErrNotFound
		}
		return agentruntime.WorktreeBaseline{}, err
	}
	return worktreeBaselineFromRow(row), nil
}

func (r *runtimeRepository) GetWorktreeBaseline(ctx context.Context, invocationID string) (agentruntime.WorktreeBaseline, error) {
	return r.GetWorkspaceBaseline(ctx, invocationID)
}

func (r *runtimeRepository) SaveWorkspaceBaseline(ctx context.Context, item agentruntime.WorktreeBaseline) (agentruntime.WorktreeBaseline, error) {
	normalized, err := item.Normalize(time.Now().UTC())
	if err != nil {
		return agentruntime.WorktreeBaseline{}, err
	}
	encoded, err := json.Marshal(normalized.ChangedPaths)
	if err != nil {
		return agentruntime.WorktreeBaseline{}, err
	}
	var saved agentruntime.WorktreeBaseline
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var invocation invocationRow
		if findErr := tx.Where("id = ?", normalized.InvocationID).First(&invocation).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		var existing worktreeBaselineRow
		findErr := tx.Where("invocation_id = ?", normalized.InvocationID).First(&existing).Error
		if findErr == nil {
			current := worktreeBaselineFromRow(existing)
			if !agentruntime.EqualWorktreeBaseline(current, normalized) {
				return agentruntime.ErrConflict
			}
			saved = current
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		row := &worktreeBaselineRow{
			ID: normalized.InvocationID + "-baseline", InvocationID: normalized.InvocationID,
			WorkspaceID: normalized.WorkspaceID, TargetPath: normalized.TargetPath,
			RepositoryType: normalized.RepositoryType, HeadRevision: normalized.HeadRevision, Branch: normalized.Branch,
			StatusDigest: normalized.StatusDigest, ChangedJSON: string(encoded), StatusKnown: normalized.StatusKnown,
			Truncated: normalized.Truncated, Revision: normalized.Revision, CapturedAt: normalized.CapturedAt,
		}
		if createErr := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row).Error; createErr != nil {
			return createErr
		}
		// Another process may have inserted the same immutable baseline between
		// the read and insert. Read the winner and apply the same equality check
		// instead of leaking a driver-specific unique error.
		var persisted worktreeBaselineRow
		if readErr := tx.Where("invocation_id = ?", normalized.InvocationID).First(&persisted).Error; readErr != nil {
			return readErr
		}
		saved = worktreeBaselineFromRow(persisted)
		if !agentruntime.EqualWorktreeBaseline(saved, normalized) {
			return agentruntime.ErrConflict
		}
		return nil
	})
	if err != nil {
		return agentruntime.WorktreeBaseline{}, err
	}
	return saved, nil
}

func (r *runtimeRepository) SaveWorktreeBaseline(ctx context.Context, item agentruntime.WorktreeBaseline) (agentruntime.WorktreeBaseline, error) {
	return r.SaveWorkspaceBaseline(ctx, item)
}

func (r *runtimeRepository) GetInvocationByIdempotencyKey(ctx context.Context, userID, conversationID, key string) (agentruntime.Invocation, error) {
	var row invocationRow
	query := r.db.WithContext(ctx).Where("user_id = ? AND conversation_id = ? AND idempotency_key = ?", strings.TrimSpace(userID), strings.TrimSpace(conversationID), strings.TrimSpace(key)).Order("created_at ASC").First(&row)
	if err := query.Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.Invocation{}, agentruntime.ErrNotFound
		}
		return agentruntime.Invocation{}, err
	}
	return invocationFromRow(row), nil
}

// AcquireInvocationLease atomically grants a short execution lease to one
// worker. A queued invocation may be claimed, and an already-running lease
// may be refreshed by its current owner. An unexpired lease held by another
// worker never gets overwritten.
func (r *runtimeRepository) AcquireInvocationLease(ctx context.Context, id, owner string, now time.Time, ttl time.Duration) (bool, error) {
	id = strings.TrimSpace(id)
	owner = strings.TrimSpace(owner)
	if id == "" || owner == "" || ttl <= 0 {
		return false, agentruntime.ErrConflict
	}
	now = now.UTC()
	expires := now.Add(ttl)
	result := r.db.WithContext(ctx).Model(&invocationRow{}).
		Where("id = ? AND status IN ? AND (lease_owner = '' OR lease_owner IS NULL OR lease_owner = ? OR lease_expires_at IS NULL OR lease_expires_at <= ?)", id, []string{string(agentruntime.InvocationQueued), string(agentruntime.InvocationRunning)}, owner, now).
		Updates(map[string]any{"lease_owner": owner, "lease_expires_at": &expires, "updated_at": now})
	return result.RowsAffected == 1, result.Error
}

func (r *runtimeRepository) RenewInvocationLease(ctx context.Context, id, owner string, now time.Time, ttl time.Duration) (bool, error) {
	id = strings.TrimSpace(id)
	owner = strings.TrimSpace(owner)
	if id == "" || owner == "" || ttl <= 0 {
		return false, agentruntime.ErrConflict
	}
	now = now.UTC()
	expires := now.Add(ttl)
	result := r.db.WithContext(ctx).Model(&invocationRow{}).
		Where("id = ? AND lease_owner = ? AND status = ? AND lease_expires_at > ?", id, owner, string(agentruntime.InvocationRunning), now).
		Updates(map[string]any{"lease_expires_at": &expires, "updated_at": now})
	return result.RowsAffected == 1, result.Error
}

func (r *runtimeRepository) ReleaseInvocationLease(ctx context.Context, id, owner string) (bool, error) {
	id = strings.TrimSpace(id)
	owner = strings.TrimSpace(owner)
	if id == "" || owner == "" {
		return false, agentruntime.ErrConflict
	}
	result := r.db.WithContext(ctx).Model(&invocationRow{}).
		Where("id = ? AND lease_owner = ?", id, owner).
		Updates(map[string]any{"lease_owner": "", "lease_expires_at": nil, "updated_at": time.Now().UTC()})
	return result.RowsAffected == 1, result.Error
}

func (r *runtimeRepository) ListInvocations(ctx context.Context, userID string, statuses []agentruntime.InvocationStatus) ([]agentruntime.Invocation, error) {
	query := r.db.WithContext(ctx).Order("created_at ASC")
	if strings.TrimSpace(userID) != "" {
		query = query.Where("user_id = ?", strings.TrimSpace(userID))
	}
	if len(statuses) > 0 {
		values := make([]string, 0, len(statuses))
		for _, status := range statuses {
			values = append(values, string(status))
		}
		query = query.Where("status IN ?", values)
	}
	var rows []invocationRow
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]agentruntime.Invocation, 0, len(rows))
	for _, row := range rows {
		result = append(result, invocationFromRow(row))
	}
	return result, nil
}

func (r *runtimeRepository) TransitionInvocation(ctx context.Context, id string, from, to agentruntime.InvocationStatus, message string) (bool, error) {
	if !validRuntimeInvocationTransition(from, to) {
		return false, agentruntime.ErrConflict
	}
	updates := map[string]any{"status": string(to), "updated_at": time.Now().UTC(), "error": message}
	if to.Terminal() {
		now := time.Now().UTC()
		updates["finished_at"] = &now
	}
	result := r.db.WithContext(ctx).Model(&invocationRow{}).Where("id = ? AND status = ?", strings.TrimSpace(id), string(from)).Updates(updates)
	return result.RowsAffected == 1, result.Error
}

// CommitInterruptedInvocation closes an abandoned Invocation and, when the
// caller supplied an active plan, blocks that step in the same SQLite
// transaction. The terminal and plan.updated events are inserted before the
// transaction is committed; Coordinator publishes them only after success.
func (r *runtimeRepository) CommitInterruptedInvocation(ctx context.Context, commit agentruntime.InterruptedInvocationCommit) (agentruntime.Invocation, *agentruntime.TaskPlan, []agentruntime.AgentEvent, error) {
	invocationID := strings.TrimSpace(commit.InvocationID)
	if invocationID == "" || !commit.ToStatus.Terminal() || !validRuntimeInvocationTransition(commit.FromStatus, commit.ToStatus) {
		return agentruntime.Invocation{}, nil, nil, agentruntime.ErrConflict
	}
	if len(commit.Events) == 0 {
		return agentruntime.Invocation{}, nil, nil, agentruntime.ErrConflict
	}
	terminalType := interruptedTerminalEventType(commit.ToStatus)
	terminalSeen := false
	planSeen := false
	for index, event := range commit.Events {
		eventType := strings.TrimSpace(event.Type)
		if eventType == "" {
			return agentruntime.Invocation{}, nil, nil, agentruntime.ErrConflict
		}
		switch eventType {
		case agentruntime.EventPlanUpdated:
			if commit.Plan == nil || planSeen {
				return agentruntime.Invocation{}, nil, nil, agentruntime.ErrConflict
			}
			planSeen = true
		case agentruntime.EventInvocationCompleted, agentruntime.EventInvocationFailed, agentruntime.EventInvocationCancelled, agentruntime.EventInvocationExpired:
			if eventType != terminalType || terminalSeen || index != len(commit.Events)-1 {
				return agentruntime.Invocation{}, nil, nil, agentruntime.ErrConflict
			}
			terminalSeen = true
		}
	}
	if !terminalSeen || (commit.Plan != nil && !planSeen) || (commit.Plan == nil && planSeen) {
		return agentruntime.Invocation{}, nil, nil, agentruntime.ErrConflict
	}

	now := time.Now().UTC()
	var savedInvocation agentruntime.Invocation
	var savedPlan *agentruntime.TaskPlan
	var savedEvents []agentruntime.AgentEvent
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var invocation invocationRow
		if findErr := tx.Where("id = ?", invocationID).First(&invocation).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		if agentruntime.InvocationStatus(invocation.Status) != commit.FromStatus {
			return agentruntime.ErrConflict
		}

		var normalizedPlan *agentruntime.TaskPlan
		if commit.Plan != nil {
			var currentRow taskPlanRow
			if findErr := tx.Where("invocation_id = ?", invocationID).First(&currentRow).Error; findErr != nil {
				if errors.Is(findErr, gorm.ErrRecordNotFound) {
					return agentruntime.ErrNotFound
				}
				return findErr
			}
			currentPlan := taskPlanFromRow(currentRow)
			if commit.Plan.Revision != currentPlan.Revision {
				return agentruntime.ErrConflict
			}
			proposed := *commit.Plan
			proposed.ID = currentPlan.ID
			proposed.InvocationID = invocationID
			proposed.Revision = currentPlan.Revision + 1
			proposed.CreatedAt = currentPlan.CreatedAt
			normalized, normalizeErr := proposed.Normalize(now)
			if normalizeErr != nil {
				return normalizeErr
			}
			normalizedPlan = &normalized
			steps, marshalErr := json.Marshal(normalized.Steps)
			if marshalErr != nil {
				return marshalErr
			}
			updated := tx.Model(&taskPlanRow{}).Where("invocation_id = ? AND revision = ?", invocationID, currentPlan.Revision).Updates(map[string]any{
				"revision": normalized.Revision, "status": string(normalized.Status), "current_step_id": normalized.CurrentStepID,
				"blocker": normalized.Blocker, "steps_json": string(steps), "updated_at": normalized.UpdatedAt,
			})
			if updated.Error != nil {
				return updated.Error
			}
			if updated.RowsAffected != 1 {
				return agentruntime.ErrConflict
			}
		}

		finishedAt := now
		updates := map[string]any{
			"status": string(commit.ToStatus), "error": strings.TrimSpace(commit.Message),
			"lease_owner": "", "lease_expires_at": nil, "updated_at": now, "finished_at": &finishedAt,
		}
		updated := tx.Model(&invocationRow{}).Where("id = ? AND status = ?", invocationID, string(commit.FromStatus)).Updates(updates)
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return agentruntime.ErrConflict
		}
		invocation.Status = string(commit.ToStatus)
		invocation.Error = strings.TrimSpace(commit.Message)
		invocation.LeaseOwner = ""
		invocation.LeaseExpiresAt = nil
		invocation.UpdatedAt = now
		invocation.FinishedAt = &finishedAt
		savedInvocation = invocationFromRow(invocation)
		if normalizedPlan != nil {
			copyPlan := *normalizedPlan
			copyPlan.Steps = append([]agentruntime.PlanStep(nil), normalizedPlan.Steps...)
			savedPlan = &copyPlan
		}

		var last agentEventRow
		lastErr := tx.Where("invocation_id = ?", invocationID).Order("sequence DESC").First(&last).Error
		if lastErr != nil && !errors.Is(lastErr, gorm.ErrRecordNotFound) {
			return lastErr
		}
		prepared := make([]agentruntime.AgentEvent, len(commit.Events))
		seenIDs := make(map[string]struct{}, len(commit.Events))
		for index, event := range commit.Events {
			event.InvocationID = invocationID
			event.ID = strings.TrimSpace(event.ID)
			if event.ID == "" {
				event.ID = "recovery-event-" + invocationID
			}
			if _, duplicate := seenIDs[event.ID]; duplicate {
				return agentruntime.ErrConflict
			}
			seenIDs[event.ID] = struct{}{}
			var existing agentEventRow
			findErr := tx.Where("id = ?", event.ID).First(&existing).Error
			if findErr == nil {
				return agentruntime.ErrConflict
			}
			if !errors.Is(findErr, gorm.ErrRecordNotFound) {
				return findErr
			}
			if event.Timestamp.IsZero() {
				event.Timestamp = now
			} else {
				event.Timestamp = event.Timestamp.UTC()
			}
			if event.Type == agentruntime.EventPlanUpdated {
				if normalizedPlan == nil {
					return agentruntime.ErrConflict
				}
				data := map[string]any{"plan": *normalizedPlan}
				if reason, ok := event.Data["reason"]; ok {
					data["reason"] = reason
				}
				event.Data = data
			}
			event.Sequence = last.Sequence + int64(index) + 1
			data, marshalErr := json.Marshal(event.Data)
			if marshalErr != nil {
				return marshalErr
			}
			if createErr := tx.Create(&agentEventRow{ID: event.ID, InvocationID: invocationID, Sequence: event.Sequence, Type: event.Type, Timestamp: event.Timestamp, DataJSON: string(data)}).Error; createErr != nil {
				return createErr
			}
			if outboxErr := createRuntimeEventOutboxTx(tx, event); outboxErr != nil {
				return outboxErr
			}
			prepared[index] = event
		}
		savedEvents = prepared
		return nil
	})
	if err != nil {
		return agentruntime.Invocation{}, nil, nil, err
	}
	return savedInvocation, savedPlan, savedEvents, nil
}

func interruptedTerminalEventType(status agentruntime.InvocationStatus) string {
	switch status {
	case agentruntime.InvocationCompleted:
		return agentruntime.EventInvocationCompleted
	case agentruntime.InvocationCancelled:
		return agentruntime.EventInvocationCancelled

	case agentruntime.InvocationExpired:
		return agentruntime.EventInvocationExpired
	default:
		return agentruntime.EventInvocationFailed
	}
}

func validRuntimeInvocationTransition(from, to agentruntime.InvocationStatus) bool {
	if from == to {
		return false
	}
	switch from {
	case agentruntime.InvocationQueued:
		return to == agentruntime.InvocationRunning || to == agentruntime.InvocationCancelling || to == agentruntime.InvocationCancelled || to == agentruntime.InvocationFailed
	case agentruntime.InvocationRunning:
		return to == agentruntime.InvocationWaitingApproval || to == agentruntime.InvocationWaitingTool || to == agentruntime.InvocationWaitingUser || to == agentruntime.InvocationWaitingSubagents || to == agentruntime.InvocationQueued || to == agentruntime.InvocationCompleted || to == agentruntime.InvocationFailed || to == agentruntime.InvocationCancelling || to == agentruntime.InvocationCancelled
	case agentruntime.InvocationWaitingApproval, agentruntime.InvocationWaitingTool, agentruntime.InvocationWaitingUser:
		return to == agentruntime.InvocationQueued || to == agentruntime.InvocationCancelling || to == agentruntime.InvocationCancelled || to == agentruntime.InvocationExpired || to == agentruntime.InvocationFailed
	case agentruntime.InvocationWaitingSubagents:
		return to == agentruntime.InvocationRunning || to == agentruntime.InvocationQueued || to == agentruntime.InvocationCancelling || to == agentruntime.InvocationCancelled || to == agentruntime.InvocationExpired || to == agentruntime.InvocationFailed
	case agentruntime.InvocationCancelling:
		return to == agentruntime.InvocationCancelled || to == agentruntime.InvocationFailed
	default:
		return false
	}
}

func (r *runtimeRepository) UpdateInvocation(ctx context.Context, item agentruntime.Invocation) error {
	item.UpdatedAt = time.Now().UTC()
	return r.db.WithContext(ctx).Save(invocationToRow(item)).Error
}

// CommitRuntimeConfigMigration atomically replaces the hidden Invocation
// configuration snapshot, appends the metadata-only migration event and
// creates its Event outbox cursor. The old digest is part of the CAS boundary;
// a stale client cannot silently migrate a newer snapshot.
func (r *runtimeRepository) CommitRuntimeConfigMigration(ctx context.Context, commit agentruntime.RuntimeConfigMigrationCommit) (agentruntime.Invocation, agentruntime.AgentEvent, error) {
	now := time.Now().UTC()
	normalized, newSnapshot, newDigest, err := agentruntime.NormalizeRuntimeConfigMigrationCommit(commit, now)
	if err != nil {
		return agentruntime.Invocation{}, agentruntime.AgentEvent{}, err
	}
	var savedInvocation agentruntime.Invocation
	var savedEvent agentruntime.AgentEvent
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row invocationRow
		if findErr := tx.Where("id = ?", normalized.InvocationID).First(&row).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		status := agentruntime.InvocationStatus(row.Status)
		if !agentruntime.IsRuntimeConfigMigrationWaitingStatus(status) {
			return agentruntime.ErrConflict
		}
		storedDigest := agent.RuntimeConfigSnapshotDigest(row.ConfigSnapshot)
		// A retry after the transaction committed is idempotent by the
		// deterministic event ID. It is safe only when the Invocation already
		// points to exactly the same new digest and immutable event metadata.
		var existingEvent agentEventRow
		findEventErr := tx.Where("id = ?", normalized.Event.ID).First(&existingEvent).Error
		if findEventErr == nil {
			event := eventFromRow(existingEvent)
			if event.InvocationID != normalized.InvocationID || event.Type != agentruntime.EventRuntimeConfigMigrated || !agentruntime.RuntimeConfigMigrationEventMatches(event, normalized.ExpectedDigest, newDigest, normalized.IdempotencyKey) || storedDigest != newDigest {
				return agentruntime.ErrConflict
			}
			savedInvocation = invocationFromRow(row)
			savedEvent = event
			return nil
		}
		if !errors.Is(findEventErr, gorm.ErrRecordNotFound) {
			return findEventErr
		}
		if storedDigest == "" || storedDigest != normalized.ExpectedDigest {
			return agentruntime.ErrConflict
		}
		previousSnapshot, previousErr := agent.ParseRuntimeConfigSnapshot(row.ConfigSnapshot)
		if previousErr != nil {
			return agentruntime.ErrConflict
		}
		data, dataErr := agentruntime.RuntimeConfigMigrationEventData(normalized.InvocationID, storedDigest, newDigest, normalized.IdempotencyKey, previousSnapshot, newSnapshot)
		if dataErr != nil {
			return dataErr
		}
		event := normalized.Event
		event.InvocationID = normalized.InvocationID
		event.Type = agentruntime.EventRuntimeConfigMigrated
		event.Data = data
		var last agentEventRow
		lastErr := tx.Where("invocation_id = ?", normalized.InvocationID).Order("sequence DESC").First(&last).Error
		if lastErr != nil && !errors.Is(lastErr, gorm.ErrRecordNotFound) {
			return lastErr
		}
		event.Sequence = last.Sequence + 1
		encodedData, marshalErr := json.Marshal(event.Data)
		if marshalErr != nil {
			return marshalErr
		}
		if createErr := tx.Create(&agentEventRow{ID: event.ID, InvocationID: event.InvocationID, Sequence: event.Sequence, Type: event.Type, Timestamp: event.Timestamp, DataJSON: string(encodedData)}).Error; createErr != nil {
			return createErr
		}
		if outboxErr := createRuntimeEventOutboxTx(tx, event); outboxErr != nil {
			return outboxErr
		}
		result := tx.Model(&invocationRow{}).Where("id = ? AND config_snapshot = ?", normalized.InvocationID, row.ConfigSnapshot).Updates(map[string]any{
			"config_snapshot": normalized.NewSnapshot,
			"updated_at":      now,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return agentruntime.ErrConflict
		}
		row.ConfigSnapshot = normalized.NewSnapshot
		row.UpdatedAt = now
		savedInvocation = invocationFromRow(row)
		savedEvent = event
		return nil
	})
	if err != nil {
		return agentruntime.Invocation{}, agentruntime.AgentEvent{}, err
	}
	return savedInvocation, savedEvent, nil
}

func (r *runtimeRepository) AppendEvent(ctx context.Context, event agentruntime.AgentEvent) (agentruntime.AgentEvent, error) {
	event.InvocationID = strings.TrimSpace(event.InvocationID)
	event.ID = strings.TrimSpace(event.ID)
	event.Type = strings.TrimSpace(event.Type)
	if event.InvocationID == "" || event.Type == "" {
		return agentruntime.AgentEvent{}, agentruntime.ErrConflict
	}
	if event.ID == "" {
		event.ID = fmt.Sprintf("event-%d", time.Now().UnixNano())
	}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var invocation invocationRow
		if findErr := tx.Where("id = ?", event.InvocationID).First(&invocation).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		var last agentEventRow
		queryErr := tx.Where("invocation_id = ?", event.InvocationID).Order("sequence DESC").First(&last).Error
		if queryErr != nil && !errors.Is(queryErr, gorm.ErrRecordNotFound) {
			return queryErr
		}
		event.Sequence = last.Sequence + 1
		if event.Timestamp.IsZero() {
			event.Timestamp = time.Now().UTC()
		}
		data, marshalErr := json.Marshal(event.Data)
		if marshalErr != nil {
			return marshalErr
		}
		if createErr := tx.Create(&agentEventRow{ID: event.ID, InvocationID: event.InvocationID, Sequence: event.Sequence, Type: event.Type, Timestamp: event.Timestamp, DataJSON: string(data)}).Error; createErr != nil {
			return createErr
		}
		return createRuntimeEventOutboxTx(tx, event)
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.AgentEvent{}, agentruntime.ErrNotFound
		}
		return agentruntime.AgentEvent{}, err
	}
	return event, nil
}

func (r *runtimeRepository) GetAgentEvent(ctx context.Context, eventID string) (agentruntime.AgentEvent, error) {
	var row agentEventRow
	err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(eventID)).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return agentruntime.AgentEvent{}, agentruntime.ErrNotFound
	}
	if err != nil {
		return agentruntime.AgentEvent{}, err
	}
	return agentruntime.SanitizeAgentEvent(eventFromRow(row)), nil
}

// EnqueueRuntimeEventOutbox is idempotent for one immutable EventID. Normal
// AppendEvent calls already insert the row in their transaction; this method
// exists for migration/reconciliation tooling and verifies that a caller
// cannot enqueue an orphan or a cursor for a different event.
func (r *runtimeRepository) EnqueueRuntimeEventOutbox(ctx context.Context, item agentruntime.RuntimeEventOutbox) (agentruntime.RuntimeEventOutbox, error) {
	normalized, err := item.Normalize(time.Now().UTC())
	if err != nil {
		return agentruntime.RuntimeEventOutbox{}, err
	}
	var saved agentruntime.RuntimeEventOutbox
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var event agentEventRow
		if findErr := tx.Where("id = ?", normalized.EventID).First(&event).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		if event.InvocationID != normalized.InvocationID || event.Sequence != normalized.Sequence || event.Type != normalized.Type {
			return agentruntime.ErrConflict
		}
		expected, expectedErr := agentruntime.NewRuntimeEventOutbox(eventFromRow(event))
		if expectedErr != nil {
			return expectedErr
		}
		var existing runtimeEventOutboxRow
		findErr := tx.Where("event_id = ?", normalized.EventID).First(&existing).Error
		if findErr == nil {
			if existing.ID != normalized.ID {
				return agentruntime.ErrConflict
			}
			saved = runtimeEventOutboxFromRow(existing)
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		if normalized.ID != expected.ID || normalized.Status != agentruntime.RuntimeEventOutboxQueued || normalized.Attempt != 0 {
			return agentruntime.ErrConflict
		}
		if createErr := tx.Create(runtimeEventOutboxToRow(normalized)).Error; createErr != nil {
			return createErr
		}
		saved = normalized
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeEventOutbox{}, err
	}
	return saved, nil
}

func (r *runtimeRepository) GetRuntimeEventOutbox(ctx context.Context, id string) (agentruntime.RuntimeEventOutbox, error) {
	var row runtimeEventOutboxRow
	err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return agentruntime.RuntimeEventOutbox{}, agentruntime.ErrNotFound
	}
	if err != nil {
		return agentruntime.RuntimeEventOutbox{}, err
	}
	return runtimeEventOutboxFromRow(row), nil
}

func (r *runtimeRepository) ListRuntimeEventOutbox(ctx context.Context, invocationID string, status agentruntime.RuntimeEventOutboxStatus, limit int) ([]agentruntime.RuntimeEventOutbox, error) {
	invocationID = strings.TrimSpace(invocationID)
	if invocationID != "" {
		var invocation invocationRow
		if err := r.db.WithContext(ctx).Where("id = ?", invocationID).First(&invocation).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, agentruntime.ErrNotFound
		} else if err != nil {
			return nil, err
		}
	}
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	query := r.db.WithContext(ctx).Model(&runtimeEventOutboxRow{})
	if invocationID != "" {
		query = query.Where("invocation_id = ?", invocationID)
	}
	if status != "" {
		query = query.Where("status = ?", string(status))
	}
	var rows []runtimeEventOutboxRow
	if err := query.Order("created_at ASC").Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]agentruntime.RuntimeEventOutbox, 0, len(rows))
	for _, row := range rows {
		result = append(result, runtimeEventOutboxFromRow(row))
	}
	return result, nil
}

// RuntimeEventOutboxReadyThrough is the SQLite implementation of the
// checkpoint event high-water barrier. It treats every non-completed cursor
// through the requested sequence as a blocker, including failed cursors whose
// destination visibility can no longer be proven.
func (r *runtimeRepository) RuntimeEventOutboxReadyThrough(ctx context.Context, invocationID string, sequence int64) (bool, error) {
	invocationID = strings.TrimSpace(invocationID)
	if invocationID == "" || sequence <= 0 {
		return false, agentruntime.ErrConflict
	}
	var invocation invocationRow
	if err := r.db.WithContext(ctx).Where("id = ?", invocationID).First(&invocation).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return false, agentruntime.ErrNotFound
	} else if err != nil {
		return false, err
	}
	var total int64
	if err := r.db.WithContext(ctx).Model(&runtimeEventOutboxRow{}).Where("invocation_id = ? AND sequence <= ?", invocationID, sequence).Count(&total).Error; err != nil {
		return false, err
	}
	if total != sequence {
		return false, nil
	}
	var pending int64
	if err := r.db.WithContext(ctx).Model(&runtimeEventOutboxRow{}).Where("invocation_id = ? AND sequence <= ? AND status <> ?", invocationID, sequence, string(agentruntime.RuntimeEventOutboxCompleted)).Count(&pending).Error; err != nil {
		return false, err
	}
	return pending == 0, nil
}

func (r *runtimeRepository) ClaimRuntimeEventOutbox(ctx context.Context, owner string, now time.Time, ttl time.Duration) (agentruntime.RuntimeEventOutbox, bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeEventOutboxOwnerLength || ttl <= 0 || ttl > agentruntime.MaxRuntimeEventOutboxLease {
		return agentruntime.RuntimeEventOutbox{}, false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	for round := 0; round < 4; round++ {
		var rows []runtimeEventOutboxRow
		if err := r.db.WithContext(ctx).Where("(status = ? AND available_at <= ?) OR (status = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?))", string(agentruntime.RuntimeEventOutboxQueued), now, string(agentruntime.RuntimeEventOutboxProcessing), now).Order("available_at ASC").Order("created_at ASC").Order("id ASC").Limit(64).Find(&rows).Error; err != nil {
			return agentruntime.RuntimeEventOutbox{}, false, err
		}
		if len(rows) == 0 {
			return agentruntime.RuntimeEventOutbox{}, false, nil
		}
		for _, row := range rows {
			if row.Attempt >= agentruntime.MaxRuntimeEventOutboxAttempts {
				result := r.db.WithContext(ctx).Model(&runtimeEventOutboxRow{}).Where("id = ? AND revision = ? AND status IN (?, ?)", row.ID, row.Revision, string(agentruntime.RuntimeEventOutboxQueued), string(agentruntime.RuntimeEventOutboxProcessing)).Updates(map[string]any{
					"status": string(agentruntime.RuntimeEventOutboxFailed), "lease_owner": "", "lease_expires_at": nil,
					"last_error": "event outbox 达到最大投递次数", "revision": row.Revision + 1, "updated_at": now,
				})
				if result.Error != nil {
					return agentruntime.RuntimeEventOutbox{}, false, result.Error
				}
				continue
			}
			expires := now.Add(ttl)
			result := r.db.WithContext(ctx).Model(&runtimeEventOutboxRow{}).Where("id = ? AND revision = ? AND ((status = ? AND available_at <= ?) OR (status = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?)))", row.ID, row.Revision, string(agentruntime.RuntimeEventOutboxQueued), now, string(agentruntime.RuntimeEventOutboxProcessing), now).Updates(map[string]any{
				"status": string(agentruntime.RuntimeEventOutboxProcessing), "attempt": row.Attempt + 1, "revision": row.Revision + 1,
				"lease_owner": owner, "lease_expires_at": &expires, "updated_at": now,
			})
			if result.Error != nil {
				return agentruntime.RuntimeEventOutbox{}, false, result.Error
			}
			if result.RowsAffected != 1 {
				continue
			}
			row.Status = string(agentruntime.RuntimeEventOutboxProcessing)
			row.Attempt++
			row.Revision++
			row.LeaseOwner = owner
			row.LeaseExpiresAt = &expires
			row.UpdatedAt = now
			return runtimeEventOutboxFromRow(row), true, nil
		}
	}
	return agentruntime.RuntimeEventOutbox{}, false, nil
}

func (r *runtimeRepository) CompleteRuntimeEventOutbox(ctx context.Context, id, owner string, now time.Time) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeEventOutboxOwnerLength {
		return false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var row runtimeEventOutboxRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return false, agentruntime.ErrNotFound
	} else if err != nil {
		return false, err
	}
	result := r.db.WithContext(ctx).Model(&runtimeEventOutboxRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", row.ID, row.Revision, string(agentruntime.RuntimeEventOutboxProcessing), owner, now).Updates(map[string]any{
		"status": string(agentruntime.RuntimeEventOutboxCompleted), "lease_owner": "", "lease_expires_at": nil,
		"revision": row.Revision + 1, "updated_at": now,
	})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func (r *runtimeRepository) RetryRuntimeEventOutbox(ctx context.Context, id, owner string, now time.Time, message string) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeEventOutboxOwnerLength {
		return false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var row runtimeEventOutboxRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return false, agentruntime.ErrNotFound
	} else if err != nil {
		return false, err
	}
	message = agentruntime.SanitizeRuntimeString(message)
	if len(message) > 4096 {
		message = message[:4096]
	}
	status := agentruntime.RuntimeEventOutboxQueued
	availableAt := now.Add(agentruntime.RuntimeEventOutboxBackoff(row.Attempt))
	if row.Attempt >= agentruntime.MaxRuntimeEventOutboxAttempts {
		status = agentruntime.RuntimeEventOutboxFailed
		availableAt = row.AvailableAt
	}
	result := r.db.WithContext(ctx).Model(&runtimeEventOutboxRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", row.ID, row.Revision, string(agentruntime.RuntimeEventOutboxProcessing), owner, now).Updates(map[string]any{
		"status": string(status), "available_at": availableAt, "lease_owner": "", "lease_expires_at": nil,
		"last_error": message, "revision": row.Revision + 1, "updated_at": now,
	})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// EnqueueRuntimeCheckpointDeliveryOutbox accepts a cursor only when its
// metadata-only projection matches the currently stored Snapshot revision.
// Snapshot commits use the stronger CommitRuntimeSnapshotWithCheckpoint
// method below; this entry point exists for reconciliation and migration.
func (r *runtimeRepository) EnqueueRuntimeCheckpointDeliveryOutbox(ctx context.Context, item agentruntime.RuntimeCheckpointDeliveryOutbox) (agentruntime.RuntimeCheckpointDeliveryOutbox, error) {
	normalized, err := item.Normalize(time.Now().UTC())
	if err != nil {
		return agentruntime.RuntimeCheckpointDeliveryOutbox{}, err
	}
	var saved agentruntime.RuntimeCheckpointDeliveryOutbox
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var snapshotRow runtimeSnapshotRow
		if findErr := tx.Where("invocation_id = ?", normalized.InvocationID).First(&snapshotRow).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		var snapshot agentruntime.RuntimeSnapshot
		if err := json.Unmarshal([]byte(snapshotRow.SnapshotJSON), &snapshot); err != nil {
			return err
		}
		if snapshot.Revision != normalized.SnapshotRevision {
			return agentruntime.ErrConflict
		}
		var checkpointEvent agentEventRow
		eventErr := tx.Where("invocation_id = ? AND sequence = ?", normalized.InvocationID, normalized.EventSequence).First(&checkpointEvent).Error
		if errors.Is(eventErr, gorm.ErrRecordNotFound) {
			return agentruntime.ErrConflict
		}
		if eventErr != nil {
			return eventErr
		}
		expected, expectedErr := agentruntime.NewRuntimeCheckpointDeliveryOutbox(normalized.Source, normalized.Destination, snapshot, normalized.EventSequence)
		if expectedErr != nil {
			return expectedErr
		}
		if normalized.ID != expected.ID || normalized.DeliveryID != expected.DeliveryID || normalized.SnapshotDigest != expected.SnapshotDigest || !normalized.ProjectionMatches(expected) {
			return agentruntime.ErrConflict
		}
		var existing runtimeCheckpointDeliveryOutboxRow
		findErr := tx.Where("id = ?", normalized.ID).First(&existing).Error
		if findErr == nil {
			stored, decodeErr := runtimeCheckpointDeliveryOutboxFromRow(existing)
			if decodeErr != nil {
				return decodeErr
			}
			if !stored.Matches(normalized) {
				return agentruntime.ErrConflict
			}
			saved = stored
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		var same runtimeCheckpointDeliveryOutboxRow
		identityErr := tx.Where("invocation_id = ? AND source = ? AND destination = ? AND snapshot_revision = ? AND event_sequence = ?", normalized.InvocationID, normalized.Source, normalized.Destination, normalized.SnapshotRevision, normalized.EventSequence).First(&same).Error
		if identityErr == nil {
			stored, decodeErr := runtimeCheckpointDeliveryOutboxFromRow(same)
			if decodeErr != nil {
				return decodeErr
			}
			if !stored.Matches(normalized) {
				return agentruntime.ErrConflict
			}
			saved = stored
			return nil
		}
		if !errors.Is(identityErr, gorm.ErrRecordNotFound) {
			return identityErr
		}
		if normalized.Status != agentruntime.RuntimeCheckpointDeliveryOutboxQueued || normalized.Attempt != 0 {
			return agentruntime.ErrConflict
		}
		row, rowErr := runtimeCheckpointDeliveryOutboxToRow(normalized)
		if rowErr != nil {
			return rowErr
		}
		if createErr := tx.Create(row).Error; createErr != nil {
			return createErr
		}
		saved = normalized
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeCheckpointDeliveryOutbox{}, err
	}
	return saved, nil
}

func (r *runtimeRepository) GetRuntimeCheckpointDeliveryOutbox(ctx context.Context, id string) (agentruntime.RuntimeCheckpointDeliveryOutbox, error) {
	var row runtimeCheckpointDeliveryOutboxRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeCheckpointDeliveryOutbox{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeCheckpointDeliveryOutbox{}, err
	}
	item, err := runtimeCheckpointDeliveryOutboxFromRow(row)
	if err != nil {
		return agentruntime.RuntimeCheckpointDeliveryOutbox{}, err
	}
	if _, err := item.Normalize(time.Now().UTC()); err != nil {
		return agentruntime.RuntimeCheckpointDeliveryOutbox{}, err
	}
	return item, nil
}

func (r *runtimeRepository) ListRuntimeCheckpointDeliveryOutbox(ctx context.Context, invocationID string, status agentruntime.RuntimeCheckpointDeliveryOutboxStatus, limit int) ([]agentruntime.RuntimeCheckpointDeliveryOutbox, error) {
	invocationID = strings.TrimSpace(invocationID)
	if invocationID != "" {
		var invocation invocationRow
		if err := r.db.WithContext(ctx).Where("id = ?", invocationID).First(&invocation).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, agentruntime.ErrNotFound
		} else if err != nil {
			return nil, err
		}
	}
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	query := r.db.WithContext(ctx).Model(&runtimeCheckpointDeliveryOutboxRow{})
	if invocationID != "" {
		query = query.Where("invocation_id = ?", invocationID)
	}
	if status != "" {
		query = query.Where("status = ?", string(status))
	}
	var rows []runtimeCheckpointDeliveryOutboxRow
	if err := query.Order("available_at ASC").Order("created_at ASC").Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]agentruntime.RuntimeCheckpointDeliveryOutbox, 0, len(rows))
	for _, row := range rows {
		item, err := runtimeCheckpointDeliveryOutboxFromRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func (r *runtimeRepository) ClaimRuntimeCheckpointDeliveryOutbox(ctx context.Context, owner string, now time.Time, ttl time.Duration) (agentruntime.RuntimeCheckpointDeliveryOutbox, bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeCheckpointDeliveryOutboxOwnerLength || ttl <= 0 || ttl > agentruntime.MaxRuntimeCheckpointDeliveryOutboxLease {
		return agentruntime.RuntimeCheckpointDeliveryOutbox{}, false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	for round := 0; round < 4; round++ {
		var rows []runtimeCheckpointDeliveryOutboxRow
		if err := r.db.WithContext(ctx).Where("(status = ? AND available_at <= ?) OR (status = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?))", string(agentruntime.RuntimeCheckpointDeliveryOutboxQueued), now, string(agentruntime.RuntimeCheckpointDeliveryOutboxProcessing), now).Order("available_at ASC").Order("created_at ASC").Order("id ASC").Limit(64).Find(&rows).Error; err != nil {
			return agentruntime.RuntimeCheckpointDeliveryOutbox{}, false, err
		}
		if len(rows) == 0 {
			return agentruntime.RuntimeCheckpointDeliveryOutbox{}, false, nil
		}
		for _, row := range rows {
			if row.Status == string(agentruntime.RuntimeCheckpointDeliveryOutboxProcessing) && (strings.TrimSpace(row.LeaseOwner) == "" || row.LeaseExpiresAt == nil || row.LeaseExpiresAt.IsZero()) {
				result := r.db.WithContext(ctx).Model(&runtimeCheckpointDeliveryOutboxRow{}).Where("id = ? AND revision = ? AND status = ?", row.ID, row.Revision, string(agentruntime.RuntimeCheckpointDeliveryOutboxProcessing)).Updates(map[string]any{
					"status": string(agentruntime.RuntimeCheckpointDeliveryOutboxFailed), "lease_owner": "", "lease_expires_at": nil,
					"last_error": "checkpoint outbox processing 元数据无效", "revision": row.Revision + 1, "updated_at": now,
				})
				if result.Error != nil {
					return agentruntime.RuntimeCheckpointDeliveryOutbox{}, false, result.Error
				}
				continue
			}
			if row.Attempt >= agentruntime.MaxRuntimeCheckpointDeliveryOutboxAttempts {
				result := r.db.WithContext(ctx).Model(&runtimeCheckpointDeliveryOutboxRow{}).Where("id = ? AND revision = ? AND status IN (?, ?)", row.ID, row.Revision, string(agentruntime.RuntimeCheckpointDeliveryOutboxQueued), string(agentruntime.RuntimeCheckpointDeliveryOutboxProcessing)).Updates(map[string]any{
					"status": string(agentruntime.RuntimeCheckpointDeliveryOutboxFailed), "lease_owner": "", "lease_expires_at": nil,
					"last_error": "checkpoint outbox 达到最大投递次数", "revision": row.Revision + 1, "updated_at": now,
				})
				if result.Error != nil {
					return agentruntime.RuntimeCheckpointDeliveryOutbox{}, false, result.Error
				}
				continue
			}
			expires := now.Add(ttl)
			result := r.db.WithContext(ctx).Model(&runtimeCheckpointDeliveryOutboxRow{}).Where("id = ? AND revision = ? AND ((status = ? AND available_at <= ?) OR (status = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?)))", row.ID, row.Revision, string(agentruntime.RuntimeCheckpointDeliveryOutboxQueued), now, string(agentruntime.RuntimeCheckpointDeliveryOutboxProcessing), now).Updates(map[string]any{
				"status": string(agentruntime.RuntimeCheckpointDeliveryOutboxProcessing), "attempt": row.Attempt + 1, "revision": row.Revision + 1,
				"lease_owner": owner, "lease_expires_at": &expires, "updated_at": now,
			})
			if result.Error != nil {
				return agentruntime.RuntimeCheckpointDeliveryOutbox{}, false, result.Error
			}
			if result.RowsAffected != 1 {
				continue
			}
			row.Status = string(agentruntime.RuntimeCheckpointDeliveryOutboxProcessing)
			row.Attempt++
			row.Revision++
			row.LeaseOwner = owner
			row.LeaseExpiresAt = &expires
			row.UpdatedAt = now
			item, decodeErr := runtimeCheckpointDeliveryOutboxFromRow(row)
			if decodeErr != nil {
				return agentruntime.RuntimeCheckpointDeliveryOutbox{}, false, decodeErr
			}
			return item, true, nil
		}
	}
	return agentruntime.RuntimeCheckpointDeliveryOutbox{}, false, nil
}

func (r *runtimeRepository) CompleteRuntimeCheckpointDeliveryOutbox(ctx context.Context, id, owner string, now time.Time) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeCheckpointDeliveryOutboxOwnerLength {
		return false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var row runtimeCheckpointDeliveryOutboxRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return false, agentruntime.ErrNotFound
	} else if err != nil {
		return false, err
	}
	result := r.db.WithContext(ctx).Model(&runtimeCheckpointDeliveryOutboxRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", row.ID, row.Revision, string(agentruntime.RuntimeCheckpointDeliveryOutboxProcessing), owner, now).Updates(map[string]any{
		"status": string(agentruntime.RuntimeCheckpointDeliveryOutboxCompleted), "lease_owner": "", "lease_expires_at": nil,
		"revision": row.Revision + 1, "updated_at": now,
	})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func (r *runtimeRepository) RetryRuntimeCheckpointDeliveryOutbox(ctx context.Context, id, owner string, now time.Time, message string) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeCheckpointDeliveryOutboxOwnerLength {
		return false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var row runtimeCheckpointDeliveryOutboxRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return false, agentruntime.ErrNotFound
	} else if err != nil {
		return false, err
	}
	message = agentruntime.SanitizeRuntimeString(message)
	if len(message) > 4096 {
		message = message[:4096]
	}
	status := agentruntime.RuntimeCheckpointDeliveryOutboxQueued
	availableAt := now.Add(agentruntime.RuntimeCheckpointDeliveryOutboxBackoff(row.Attempt))
	if row.Attempt >= agentruntime.MaxRuntimeCheckpointDeliveryOutboxAttempts {
		status = agentruntime.RuntimeCheckpointDeliveryOutboxFailed
		availableAt = row.AvailableAt
	}
	result := r.db.WithContext(ctx).Model(&runtimeCheckpointDeliveryOutboxRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", row.ID, row.Revision, string(agentruntime.RuntimeCheckpointDeliveryOutboxProcessing), owner, now).Updates(map[string]any{
		"status": string(status), "available_at": availableAt, "lease_owner": "", "lease_expires_at": nil,
		"last_error": message, "revision": row.Revision + 1, "updated_at": now,
	})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// DeferRuntimeCheckpointDeliveryOutbox releases a claim while preserving the
// delivery attempt budget. It prevents a checkpoint that is waiting on the
// event high-water barrier from exhausting its retries without ever calling a
// destination transport.
func (r *runtimeRepository) DeferRuntimeCheckpointDeliveryOutbox(ctx context.Context, id, owner string, now, availableAt time.Time, message string) (bool, error) {
	id = strings.TrimSpace(id)
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeCheckpointDeliveryOutboxOwnerLength {
		return false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if availableAt.IsZero() {
		availableAt = now
	} else {
		availableAt = availableAt.UTC()
	}
	if availableAt.Before(now) {
		availableAt = now
	}
	if availableAt.After(now.Add(agentruntime.MaxRuntimeCheckpointDeliveryOutboxLease)) {
		return false, agentruntime.ErrConflict
	}
	message = agentruntime.SanitizeRuntimeString(strings.TrimSpace(message))
	if len(message) > 4096 {
		message = message[:4096]
	}
	var row runtimeCheckpointDeliveryOutboxRow
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return false, agentruntime.ErrNotFound
	} else if err != nil {
		return false, err
	}
	attempt := row.Attempt
	if attempt > 0 {
		attempt--
	}
	result := r.db.WithContext(ctx).Model(&runtimeCheckpointDeliveryOutboxRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", row.ID, row.Revision, string(agentruntime.RuntimeCheckpointDeliveryOutboxProcessing), owner, now).Updates(map[string]any{
		"status": string(agentruntime.RuntimeCheckpointDeliveryOutboxQueued), "attempt": attempt, "available_at": availableAt,
		"lease_owner": "", "lease_expires_at": nil, "last_error": message,
		"revision": row.Revision + 1, "updated_at": now,
	})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// AcceptRuntimeEventDelivery persists signed envelope metadata exactly once.
// A retry with identical fields is acknowledged as a duplicate; reusing an
// EventID for different metadata is a conflict and cannot overwrite history.
func (r *runtimeRepository) AcceptRuntimeEventDelivery(ctx context.Context, envelope agentruntime.RuntimeEventDeliveryEnvelope) (bool, error) {
	normalized, err := agentruntime.NormalizeRuntimeEventDeliveryEnvelope(envelope)
	if err != nil {
		return false, err
	}
	var duplicate bool
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var acceptErr error
		duplicate, acceptErr = acceptRuntimeEventDeliveryTx(tx, normalized, time.Now().UTC())
		return acceptErr
	})
	return duplicate, err
}

// GetRuntimeEventDelivery returns only the authenticated event metadata kept
// in the destination inbox. It is used by the signed reconciliation status
// endpoint; event bodies remain owned by the source event log.
func (r *runtimeRepository) GetRuntimeEventDelivery(ctx context.Context, eventID string) (agentruntime.RuntimeEventDeliveryInboxRecord, error) {
	var row runtimeEventDeliveryInboxRow
	if err := r.db.WithContext(ctx).Where("event_id = ?", strings.TrimSpace(eventID)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeEventDeliveryInboxRecord{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeEventDeliveryInboxRecord{}, err
	}
	return runtimeEventDeliveryInboxFromRow(row), nil
}

// AcceptRuntimeCheckpointDelivery persists the newest metadata-only
// checkpoint for an Invocation. The update is monotonic in snapshot revision
// and event high-water mark, so a delayed worker cannot roll back recovery
// state after a newer checkpoint has been accepted.
func (r *runtimeRepository) AcceptRuntimeCheckpointDelivery(ctx context.Context, envelope agentruntime.RuntimeCheckpointDeliveryEnvelope, projection agentruntime.RuntimeCheckpointProjection) (bool, error) {
	normalized, normalizedProjection, err := agentruntime.NormalizeRuntimeCheckpointDeliveryPair(envelope, projection)
	if err != nil {
		return false, err
	}
	duplicate := false
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var acceptErr error
		duplicate, acceptErr = acceptRuntimeCheckpointDeliveryTx(tx, normalized, normalizedProjection, time.Now().UTC())
		return acceptErr
	})
	if err != nil {
		return false, err
	}
	return duplicate, nil
}

func (r *runtimeRepository) GetRuntimeCheckpointDelivery(ctx context.Context, invocationID string) (agentruntime.RuntimeCheckpointDeliveryInboxRecord, error) {
	var row runtimeCheckpointDeliveryInboxRow
	if err := r.db.WithContext(ctx).Where("invocation_id = ?", strings.TrimSpace(invocationID)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeCheckpointDeliveryInboxRecord{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeCheckpointDeliveryInboxRecord{}, err
	}
	item, err := runtimeCheckpointDeliveryInboxFromRow(row)
	if err != nil {
		return agentruntime.RuntimeCheckpointDeliveryInboxRecord{}, err
	}
	if err := item.Projection.Validate(); err != nil {
		return agentruntime.RuntimeCheckpointDeliveryInboxRecord{}, err
	}
	return item, nil
}

func (r *runtimeRepository) ListEvents(ctx context.Context, invocationID string, after int64, limit int) ([]agentruntime.AgentEvent, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	var rows []agentEventRow
	if err := r.db.WithContext(ctx).Where("invocation_id = ? AND sequence > ?", strings.TrimSpace(invocationID), after).Order("sequence ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]agentruntime.AgentEvent, 0, len(rows))
	for _, row := range rows {
		result = append(result, eventFromRow(row))
	}
	return result, nil
}

func (r *runtimeRepository) CreateApproval(ctx context.Context, item agentruntime.Approval) error {
	data, err := json.Marshal(item.Args)
	if err != nil {
		return err
	}
	choices, err := json.Marshal(item.Choices)
	if err != nil {
		return err
	}
	return r.db.WithContext(ctx).Create(&approvalRow{
		ID: item.ID, InvocationID: item.InvocationID, ConversationID: item.ConversationID,
		ToolCallID: item.ToolCallID, TaskContractVersion: item.TaskContractVersion, ToolName: item.ToolName, OperationID: item.OperationID, OriginalCallID: item.OriginalCallID, ConfirmationCallID: item.ConfirmationCallID,
		ArgsJSON: string(data), Hint: item.Hint, ChoicesJSON: string(choices), Status: string(item.Status), DecisionReason: item.DecisionReason,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, ExpiresAt: item.ExpiresAt, ResolvedAt: item.ResolvedAt,
	}).Error
}

func (r *runtimeRepository) GetApproval(ctx context.Context, id string) (agentruntime.Approval, error) {
	var row approvalRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.Approval{}, agentruntime.ErrNotFound
		}
		return agentruntime.Approval{}, err
	}
	return approvalFromRow(row), nil
}

func (r *runtimeRepository) ListApprovals(ctx context.Context, invocationID string, status agentruntime.ApprovalStatus) ([]agentruntime.Approval, error) {
	query := r.db.WithContext(ctx).Order("created_at ASC")
	if strings.TrimSpace(invocationID) != "" {
		query = query.Where("invocation_id = ?", strings.TrimSpace(invocationID))
	}
	if status != "" {
		query = query.Where("status = ?", string(status))
	}
	var rows []approvalRow
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]agentruntime.Approval, 0, len(rows))
	for _, row := range rows {
		result = append(result, approvalFromRow(row))
	}
	return result, nil
}

func (r *runtimeRepository) ResolveApproval(ctx context.Context, id string, status agentruntime.ApprovalStatus, reason string) (agentruntime.Approval, bool, error) {
	now := time.Now().UTC()
	result := r.db.WithContext(ctx).Model(&approvalRow{}).Where("id = ? AND status = ?", strings.TrimSpace(id), string(agentruntime.ApprovalPending)).Updates(map[string]any{
		"status": string(status), "decision_reason": reason, "updated_at": now, "resolved_at": &now,
	})
	if result.Error != nil {
		return agentruntime.Approval{}, false, result.Error
	}
	item, err := r.GetApproval(ctx, id)
	if err != nil {
		return agentruntime.Approval{}, false, err
	}
	return item, result.RowsAffected == 1, nil
}

func (r *runtimeRepository) CreateToolCall(ctx context.Context, item agentruntime.ToolCall) error {
	args, err := json.Marshal(item.Args)
	if err != nil {
		return err
	}
	return r.db.WithContext(ctx).Create(&toolCallRow{
		ID: item.ID, InvocationID: item.InvocationID, ConversationID: item.ConversationID, ToolName: item.ToolName,
		OriginalCallID: item.OriginalCallID, ConfirmationCallID: item.ConfirmationCallID, OperationID: item.OperationID,
		ApprovalID: item.ApprovalID, ArgsJSON: string(args), Status: string(item.Status), Error: item.Error,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, FinishedAt: item.FinishedAt,
	}).Error
}

func (r *runtimeRepository) GetToolCall(ctx context.Context, id string) (agentruntime.ToolCall, error) {
	var row toolCallRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.ToolCall{}, agentruntime.ErrNotFound
		}
		return agentruntime.ToolCall{}, err
	}
	return toolCallFromRow(row), nil
}

func (r *runtimeRepository) ListToolCalls(ctx context.Context, invocationID string) ([]agentruntime.ToolCall, error) {
	query := r.db.WithContext(ctx).Order("created_at ASC")
	if strings.TrimSpace(invocationID) != "" {
		query = query.Where("invocation_id = ?", strings.TrimSpace(invocationID))
	}
	var rows []toolCallRow
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]agentruntime.ToolCall, 0, len(rows))
	for _, row := range rows {
		result = append(result, toolCallFromRow(row))
	}
	return result, nil
}

func (r *runtimeRepository) UpdateToolCall(ctx context.Context, item agentruntime.ToolCall) error {
	item.UpdatedAt = time.Now().UTC()
	if item.Status.Terminal() && item.FinishedAt == nil {
		now := item.UpdatedAt
		item.FinishedAt = &now
	}
	args, err := json.Marshal(item.Args)
	if err != nil {
		return err
	}
	result := r.db.WithContext(ctx).Model(&toolCallRow{}).Where("id = ?", item.ID).Updates(map[string]any{
		"confirmation_call_id": item.ConfirmationCallID, "operation_id": item.OperationID, "approval_id": item.ApprovalID,
		"args_json": string(args), "status": string(item.Status), "error": item.Error,
		"updated_at": item.UpdatedAt, "finished_at": item.FinishedAt,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return agentruntime.ErrNotFound
	}
	return nil
}

func (r *runtimeRepository) CreateTaskPlan(ctx context.Context, item agentruntime.TaskPlan) error {
	normalized, err := item.Normalize(time.Now().UTC())
	if err != nil {
		return err
	}
	steps, err := json.Marshal(normalized.Steps)
	if err != nil {
		return err
	}
	return r.db.WithContext(ctx).Create(&taskPlanRow{
		ID: normalized.ID, InvocationID: normalized.InvocationID, Revision: normalized.Revision,
		Status: string(normalized.Status), CurrentStepID: normalized.CurrentStepID, Blocker: normalized.Blocker,
		StepsJSON: string(steps), CreatedAt: normalized.CreatedAt, UpdatedAt: normalized.UpdatedAt,
	}).Error
}

func (r *runtimeRepository) GetTaskPlan(ctx context.Context, invocationID string) (agentruntime.TaskPlan, error) {
	var row taskPlanRow
	if err := r.db.WithContext(ctx).Where("invocation_id = ?", strings.TrimSpace(invocationID)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.TaskPlan{}, agentruntime.ErrNotFound
		}
		return agentruntime.TaskPlan{}, err
	}
	return taskPlanFromRow(row), nil
}

func (r *runtimeRepository) UpdateTaskPlan(ctx context.Context, item agentruntime.TaskPlan, expectedRevision int64) (agentruntime.TaskPlan, error) {
	normalized, err := item.Normalize(time.Now().UTC())
	if err != nil {
		return agentruntime.TaskPlan{}, err
	}
	steps, err := json.Marshal(normalized.Steps)
	if err != nil {
		return agentruntime.TaskPlan{}, err
	}
	result := r.db.WithContext(ctx).Model(&taskPlanRow{}).Where("invocation_id = ? AND revision = ?", normalized.InvocationID, expectedRevision).Updates(map[string]any{
		"revision": normalized.Revision, "status": string(normalized.Status), "current_step_id": normalized.CurrentStepID,
		"blocker": normalized.Blocker, "steps_json": string(steps), "updated_at": normalized.UpdatedAt,
	})
	if result.Error != nil {
		return agentruntime.TaskPlan{}, result.Error
	}
	if result.RowsAffected == 0 {
		if _, getErr := r.GetTaskPlan(ctx, normalized.InvocationID); errors.Is(getErr, agentruntime.ErrNotFound) {
			return agentruntime.TaskPlan{}, agentruntime.ErrNotFound
		}
		return agentruntime.TaskPlan{}, agentruntime.ErrConflict
	}
	return r.GetTaskPlan(ctx, normalized.InvocationID)
}

func (r *runtimeRepository) CreateTaskContract(ctx context.Context, item agentruntime.TaskContract) error {
	if item.Version <= 0 {
		item.Version = 1
	}
	if item.UpdatedAt.IsZero() {
		item.UpdatedAt = time.Now().UTC()
	} else {
		item.UpdatedAt = item.UpdatedAt.UTC()
	}
	if err := item.Validate(); err != nil {
		return err
	}
	row, err := taskContractToRow(item)
	if err != nil {
		return err
	}
	return r.db.WithContext(ctx).Create(row).Error
}

// SaveTaskContract appends a version for compatibility with simple embedders.
// New Runtime code should use UpdateTaskContract so the expected version is
// checked atomically in the database.
func (r *runtimeRepository) SaveTaskContract(ctx context.Context, item agentruntime.TaskContract) error {
	if item.Version <= 0 {
		var current agentruntime.TaskContract
		current, err := r.GetTaskContract(ctx, item.InvocationID)
		if errors.Is(err, agentruntime.ErrNotFound) {
			item.Version = 1
		} else if err != nil {
			return err
		} else {
			item.Version = current.Version + 1
		}
	}
	if item.Version == 1 {
		if _, err := r.GetTaskContract(ctx, item.InvocationID); errors.Is(err, agentruntime.ErrNotFound) {
			return r.CreateTaskContract(ctx, item)
		}
	}
	return func() error {
		current, err := r.GetTaskContract(ctx, item.InvocationID)
		if err != nil {
			return err
		}
		_, err = r.UpdateTaskContract(ctx, item, current.Version)
		return err
	}()
}

func (r *runtimeRepository) GetTaskContract(ctx context.Context, invocationID string) (agentruntime.TaskContract, error) {
	var row taskContractRow
	query := r.db.WithContext(ctx).Where("invocation_id = ?", strings.TrimSpace(invocationID)).Order("version DESC").First(&row)
	if err := query.Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.TaskContract{}, agentruntime.ErrNotFound
		}
		return agentruntime.TaskContract{}, err
	}
	return taskContractFromRow(row), nil
}

func (r *runtimeRepository) ListTaskContracts(ctx context.Context, invocationID string) ([]agentruntime.TaskContract, error) {
	var rows []taskContractRow
	if err := r.db.WithContext(ctx).Where("invocation_id = ?", strings.TrimSpace(invocationID)).Order("version ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]agentruntime.TaskContract, 0, len(rows))
	for _, row := range rows {
		result = append(result, taskContractFromRow(row))
	}
	return result, nil
}

func (r *runtimeRepository) UpdateTaskContract(ctx context.Context, item agentruntime.TaskContract, expectedVersion int64) (agentruntime.TaskContract, error) {
	if item.Version != expectedVersion+1 {
		return agentruntime.TaskContract{}, agentruntime.ErrConflict
	}
	if item.UpdatedAt.IsZero() {
		item.UpdatedAt = time.Now().UTC()
	} else {
		item.UpdatedAt = item.UpdatedAt.UTC()
	}
	if err := item.Validate(); err != nil {
		return agentruntime.TaskContract{}, err
	}
	row, err := taskContractToRow(item)
	if err != nil {
		return agentruntime.TaskContract{}, err
	}
	// The unique (invocation_id, version) constraint plus the latest-version
	// predicate make a stale editor fail without overwriting history.
	result := r.db.WithContext(ctx).Exec(
		`INSERT INTO abot_agent_task_contracts
		(id, invocation_id, version, task_type, goal, requested_outcome, mutation_allowed, validation_required, acceptance_json, constraints_json, non_goals_json, external_actions_json, source_message_ids_json, updated_at)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		WHERE EXISTS (SELECT 1 FROM abot_agent_task_contracts WHERE invocation_id = ? AND version = ?)
		AND NOT EXISTS (SELECT 1 FROM abot_agent_task_contracts WHERE invocation_id = ? AND version >= ?)`,
		row.ID, row.InvocationID, row.Version, row.TaskType, row.Goal, row.RequestedOutcome, row.MutationAllowed, row.ValidationRequired,
		row.AcceptanceJSON, row.ConstraintsJSON, row.NonGoalsJSON, row.ExternalActionsJSON, row.SourceMessageIDsJSON, row.UpdatedAt,
		row.InvocationID, expectedVersion, row.InvocationID, row.Version,
	)
	if result.Error != nil {
		return agentruntime.TaskContract{}, result.Error
	}
	if result.RowsAffected != 1 {
		if _, getErr := r.GetTaskContract(ctx, item.InvocationID); errors.Is(getErr, agentruntime.ErrNotFound) {
			return agentruntime.TaskContract{}, agentruntime.ErrNotFound
		}
		return agentruntime.TaskContract{}, agentruntime.ErrConflict
	}
	return item, nil
}

func (r *runtimeRepository) SaveContextManifest(ctx context.Context, item agentruntime.ContextManifest) error {
	if err := item.Validate(); err != nil {
		return err
	}
	row, err := contextManifestToRow(item)
	if err != nil {
		return err
	}
	// A model call ID is deterministic within an Invocation run. Replacing the
	// row makes retries idempotent without creating duplicate timeline entries.
	return r.db.WithContext(ctx).Save(row).Error
}

func (r *runtimeRepository) GetContextManifest(ctx context.Context, modelCallID string) (agentruntime.ContextManifest, error) {
	var row contextManifestRow
	if err := r.db.WithContext(ctx).Where("model_call_id = ?", strings.TrimSpace(modelCallID)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.ContextManifest{}, agentruntime.ErrNotFound
		}
		return agentruntime.ContextManifest{}, err
	}
	return contextManifestFromRow(row), nil
}

func (r *runtimeRepository) ListContextManifests(ctx context.Context, invocationID string) ([]agentruntime.ContextManifest, error) {
	var rows []contextManifestRow
	if err := r.db.WithContext(ctx).Where("invocation_id = ?", strings.TrimSpace(invocationID)).Order("created_at ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]agentruntime.ContextManifest, 0, len(rows))
	for _, row := range rows {
		result = append(result, contextManifestFromRow(row))
	}
	return result, nil
}

func (r *runtimeRepository) GetWorkingSet(ctx context.Context, invocationID string) (agentruntime.WorkingSet, error) {
	var row workingSetRow
	if err := r.db.WithContext(ctx).Where("invocation_id = ?", strings.TrimSpace(invocationID)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.WorkingSet{}, agentruntime.ErrNotFound
		}
		return agentruntime.WorkingSet{}, err
	}
	var items []agentruntime.WorkingSetItem
	if strings.TrimSpace(row.ItemsJSON) != "" {
		if err := json.Unmarshal([]byte(row.ItemsJSON), &items); err != nil {
			return agentruntime.WorkingSet{}, err
		}
	}
	return agentruntime.WorkingSet{InvocationID: row.InvocationID, Revision: row.Revision, Items: items, UpdatedAt: row.UpdatedAt}, nil
}

func (r *runtimeRepository) ReplaceWorkingSet(ctx context.Context, item agentruntime.WorkingSet) (agentruntime.WorkingSet, error) {
	item.InvocationID = strings.TrimSpace(item.InvocationID)
	if _, err := r.GetInvocation(ctx, item.InvocationID); err != nil {
		return agentruntime.WorkingSet{}, err
	}
	var result agentruntime.WorkingSet
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current workingSetRow
		findErr := tx.Where("invocation_id = ?", item.InvocationID).First(&current).Error
		if findErr != nil && !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		if item.Revision <= 0 {
			item.Revision = current.Revision + 1
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) && item.Revision <= current.Revision {
			return agentruntime.ErrConflict
		}
		if item.UpdatedAt.IsZero() {
			item.UpdatedAt = time.Now().UTC()
		} else {
			item.UpdatedAt = item.UpdatedAt.UTC()
		}
		if err := item.Validate(); err != nil {
			return err
		}
		encoded, err := json.Marshal(item.Items)
		if err != nil {
			return err
		}
		row := workingSetRow{ID: "working-set-" + item.InvocationID, InvocationID: item.InvocationID, Revision: item.Revision, ItemsJSON: string(encoded), UpdatedAt: item.UpdatedAt}
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		result = item
		return nil
	})
	if err != nil {
		return agentruntime.WorkingSet{}, err
	}
	return result, nil
}

func (r *runtimeRepository) CreateVerificationRun(ctx context.Context, item agentruntime.VerificationRun) error {
	if item.Revision <= 0 {
		item.Revision = 1
	}
	if item.Status == "" {
		item.Status = agentruntime.VerificationPending
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	}
	if item.UpdatedAt.IsZero() {
		item.UpdatedAt = item.CreatedAt
	}
	if err := item.Validate(); err != nil {
		return err
	}
	return r.db.WithContext(ctx).Create(&verificationRunRow{ID: item.ID, InvocationID: item.InvocationID, PlanStepID: item.PlanStepID, Kind: item.Kind, Command: item.Command, Status: string(item.Status), ExitCode: item.ExitCode, Summary: item.Summary, OutputRef: item.OutputRef, OutputDigest: item.OutputDigest, Revision: item.Revision, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, StartedAt: item.StartedAt, FinishedAt: item.FinishedAt}).Error
}

func (r *runtimeRepository) GetVerificationRun(ctx context.Context, id string) (agentruntime.VerificationRun, error) {
	var row verificationRunRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.VerificationRun{}, agentruntime.ErrNotFound
		}
		return agentruntime.VerificationRun{}, err
	}
	return verificationRunFromRow(row), nil
}

func (r *runtimeRepository) ListVerificationRuns(ctx context.Context, invocationID string) ([]agentruntime.VerificationRun, error) {
	var rows []verificationRunRow
	if err := r.db.WithContext(ctx).Where("invocation_id = ?", strings.TrimSpace(invocationID)).Order("created_at ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]agentruntime.VerificationRun, 0, len(rows))
	for _, row := range rows {
		result = append(result, verificationRunFromRow(row))
	}
	return result, nil
}

func (r *runtimeRepository) UpdateVerificationRun(ctx context.Context, item agentruntime.VerificationRun, expectedRevision int64) (agentruntime.VerificationRun, error) {
	if item.Revision != expectedRevision+1 {
		return agentruntime.VerificationRun{}, agentruntime.ErrConflict
	}
	if item.UpdatedAt.IsZero() {
		item.UpdatedAt = time.Now().UTC()
	} else {
		item.UpdatedAt = item.UpdatedAt.UTC()
	}
	if err := item.Validate(); err != nil {
		return agentruntime.VerificationRun{}, err
	}
	result := r.db.WithContext(ctx).Model(&verificationRunRow{}).Where("id = ? AND revision = ?", item.ID, expectedRevision).Updates(map[string]any{"plan_step_id": item.PlanStepID, "kind": item.Kind, "command": item.Command, "status": string(item.Status), "exit_code": item.ExitCode, "summary": item.Summary, "output_ref": item.OutputRef, "output_digest": item.OutputDigest, "revision": item.Revision, "updated_at": item.UpdatedAt, "started_at": item.StartedAt, "finished_at": item.FinishedAt})
	if result.Error != nil {
		return agentruntime.VerificationRun{}, result.Error
	}
	if result.RowsAffected != 1 {
		if _, err := r.GetVerificationRun(ctx, item.ID); errors.Is(err, agentruntime.ErrNotFound) {
			return agentruntime.VerificationRun{}, agentruntime.ErrNotFound
		}
		return agentruntime.VerificationRun{}, agentruntime.ErrConflict
	}
	return r.GetVerificationRun(ctx, item.ID)
}

func (r *runtimeRepository) GetRuntimeSnapshot(ctx context.Context, invocationID string) (agentruntime.RuntimeSnapshot, error) {
	var row runtimeSnapshotRow
	if err := r.db.WithContext(ctx).Where("invocation_id = ?", strings.TrimSpace(invocationID)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeSnapshot{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeSnapshot{}, err
	}
	var snapshot agentruntime.RuntimeSnapshot
	if err := json.Unmarshal([]byte(row.SnapshotJSON), &snapshot); err != nil {
		return agentruntime.RuntimeSnapshot{}, err
	}
	return snapshot, nil
}

func (r *runtimeRepository) SaveRuntimeSnapshot(ctx context.Context, item agentruntime.RuntimeSnapshot) (agentruntime.RuntimeSnapshot, error) {
	if item.GeneratedAt.IsZero() {
		item.GeneratedAt = time.Now().UTC()
	} else {
		item.GeneratedAt = item.GeneratedAt.UTC()
	}
	var result agentruntime.RuntimeSnapshot
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current runtimeSnapshotRow
		findErr := tx.Where("invocation_id = ?", item.InvocationID).First(&current).Error
		if findErr != nil && !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		if item.Revision <= 0 {
			item.Revision = current.Revision + 1
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) && item.Revision <= current.Revision {
			return agentruntime.ErrConflict
		}
		if err := item.Validate(); err != nil {
			return err
		}
		encoded, marshalErr := json.Marshal(item)
		if marshalErr != nil {
			return marshalErr
		}
		row := runtimeSnapshotRow{ID: "snapshot-" + item.InvocationID, InvocationID: item.InvocationID, Revision: item.Revision, ContractVersion: item.ContractVersion, PlanRevision: item.PlanRevision, Phase: item.Phase, SnapshotJSON: string(encoded), GeneratedAt: item.GeneratedAt}
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		result = item
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeSnapshot{}, err
	}
	return result, nil
}

// CommitRuntimeSnapshot persists the Runtime Snapshot and its runtime.snapshot
// event in one SQLite transaction. The event is inserted after the snapshot so
// a primary-key or serialization failure exercises the rollback boundary;
// subscribers only receive the returned event after the transaction commits.
func (r *runtimeRepository) CommitRuntimeSnapshot(ctx context.Context, item agentruntime.RuntimeSnapshot, event agentruntime.AgentEvent) (agentruntime.RuntimeSnapshot, agentruntime.AgentEvent, error) {
	savedSnapshot, savedEvent, _, err := r.commitRuntimeSnapshotWithCheckpoint(ctx, item, event, "", "")
	return savedSnapshot, savedEvent, err
}

// CommitRuntimeSnapshotWithCheckpoint adds a checkpoint outbox row to the
// same SQLite transaction as the Snapshot, runtime.snapshot event and event
// outbox cursor. The projection is created only after the transaction assigns
// the canonical event sequence.
func (r *runtimeRepository) CommitRuntimeSnapshotWithCheckpoint(ctx context.Context, item agentruntime.RuntimeSnapshot, event agentruntime.AgentEvent, source, destination string) (agentruntime.RuntimeSnapshot, agentruntime.AgentEvent, agentruntime.RuntimeCheckpointDeliveryOutbox, error) {
	return r.commitRuntimeSnapshotWithCheckpoint(ctx, item, event, source, destination)
}

func (r *runtimeRepository) commitRuntimeSnapshotWithCheckpoint(ctx context.Context, item agentruntime.RuntimeSnapshot, event agentruntime.AgentEvent, source, destination string) (agentruntime.RuntimeSnapshot, agentruntime.AgentEvent, agentruntime.RuntimeCheckpointDeliveryOutbox, error) {
	item.InvocationID = strings.TrimSpace(item.InvocationID)
	if item.InvocationID == "" {
		return agentruntime.RuntimeSnapshot{}, agentruntime.AgentEvent{}, agentruntime.RuntimeCheckpointDeliveryOutbox{}, agentruntime.ErrInvalidRuntimeSnapshot
	}
	if event.InvocationID != "" && strings.TrimSpace(event.InvocationID) != item.InvocationID {
		return agentruntime.RuntimeSnapshot{}, agentruntime.AgentEvent{}, agentruntime.RuntimeCheckpointDeliveryOutbox{}, agentruntime.ErrConflict
	}
	if item.GeneratedAt.IsZero() {
		item.GeneratedAt = time.Now().UTC()
	} else {
		item.GeneratedAt = item.GeneratedAt.UTC()
	}
	event.InvocationID = item.InvocationID
	if strings.TrimSpace(event.ID) == "" {
		event.ID = fmt.Sprintf("event-snapshot-%d", time.Now().UnixNano())
	}
	if event.Type == "" {
		event.Type = agentruntime.EventRuntimeSnapshot
	}
	if event.Type != agentruntime.EventRuntimeSnapshot {
		return agentruntime.RuntimeSnapshot{}, agentruntime.AgentEvent{}, agentruntime.RuntimeCheckpointDeliveryOutbox{}, agentruntime.ErrConflict
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = item.GeneratedAt
	} else {
		event.Timestamp = event.Timestamp.UTC()
	}
	// Keep the event payload canonical and bounded to the exact snapshot being
	// committed. This also prevents a caller-owned map from being persisted.
	event.Data = map[string]any{"snapshot": item, "revision": item.Revision}

	var savedSnapshot agentruntime.RuntimeSnapshot
	var savedEvent agentruntime.AgentEvent
	var savedCheckpointOutbox agentruntime.RuntimeCheckpointDeliveryOutbox
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var invocation invocationRow
		if findErr := tx.Where("id = ?", item.InvocationID).First(&invocation).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		var current runtimeSnapshotRow
		findErr := tx.Where("invocation_id = ?", item.InvocationID).First(&current).Error
		if findErr != nil && !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		if item.Revision <= 0 {
			item.Revision = current.Revision + 1
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) && item.Revision <= current.Revision {
			return agentruntime.ErrConflict
		}
		if err := item.Validate(); err != nil {
			return err
		}
		// Revision may have been filled from the current row above, so refresh
		// the canonical event payload before serializing it.
		event.Data = map[string]any{"snapshot": item, "revision": item.Revision}
		var last agentEventRow
		lastErr := tx.Where("invocation_id = ?", item.InvocationID).Order("sequence DESC").First(&last).Error
		if lastErr != nil && !errors.Is(lastErr, gorm.ErrRecordNotFound) {
			return lastErr
		}
		event.Sequence = last.Sequence + 1
		encoded, marshalErr := json.Marshal(item)
		if marshalErr != nil {
			return marshalErr
		}
		row := runtimeSnapshotRow{ID: "snapshot-" + item.InvocationID, InvocationID: item.InvocationID, Revision: item.Revision, ContractVersion: item.ContractVersion, PlanRevision: item.PlanRevision, Phase: item.Phase, SnapshotJSON: string(encoded), GeneratedAt: item.GeneratedAt}
		if saveErr := tx.Save(&row).Error; saveErr != nil {
			return saveErr
		}
		data, marshalErr := json.Marshal(event.Data)
		if marshalErr != nil {
			return marshalErr
		}
		if createErr := tx.Create(&agentEventRow{ID: event.ID, InvocationID: item.InvocationID, Sequence: event.Sequence, Type: event.Type, Timestamp: event.Timestamp, DataJSON: string(data)}).Error; createErr != nil {
			return createErr
		}
		eventOutbox, outboxErr := agentruntime.NewRuntimeEventOutbox(event)
		if outboxErr != nil {
			return outboxErr
		}
		if createErr := tx.Create(runtimeEventOutboxToRow(eventOutbox)).Error; createErr != nil {
			return createErr
		}
		if strings.TrimSpace(source) != "" || strings.TrimSpace(destination) != "" {
			if strings.TrimSpace(source) == "" || strings.TrimSpace(destination) == "" {
				return fmt.Errorf("%w: checkpoint source/destination 不完整", agentruntime.ErrInvalidRuntimeCheckpointDelivery)
			}
			checkpointOutbox, checkpointErr := agentruntime.NewRuntimeCheckpointDeliveryOutbox(source, destination, item, event.Sequence)
			if checkpointErr != nil {
				return checkpointErr
			}
			deliveryGroup, groupErr := agentruntime.NewRuntimeDeliveryGroup(source, destination, item.InvocationID, []agentruntime.RuntimeDeliveryGroupMember{
				{Kind: agentruntime.RuntimeDeliveryKindEvent, OutboxID: eventOutbox.ID, DeliveryID: event.ID},
				{Kind: agentruntime.RuntimeDeliveryKindCheckpoint, OutboxID: checkpointOutbox.ID, DeliveryID: checkpointOutbox.DeliveryID},
			}, item.GeneratedAt)
			if groupErr != nil {
				return groupErr
			}
			eventOutbox.GroupID = deliveryGroup.ID
			checkpointOutbox.GroupID = deliveryGroup.ID
			checkpointRow, rowErr := runtimeCheckpointDeliveryOutboxToRow(checkpointOutbox)
			if rowErr != nil {
				return rowErr
			}
			if createErr := tx.Create(checkpointRow).Error; createErr != nil {
				return createErr
			}
			if updateErr := tx.Model(&runtimeEventOutboxRow{}).Where("id = ?", eventOutbox.ID).Update("group_id", deliveryGroup.ID).Error; updateErr != nil {
				return updateErr
			}
			groupRow, groupRowErr := runtimeDeliveryGroupToRow(deliveryGroup)
			if groupRowErr != nil {
				return groupRowErr
			}
			if createErr := tx.Create(groupRow).Error; createErr != nil {
				return createErr
			}
			savedCheckpointOutbox = checkpointOutbox
		}
		savedSnapshot = item
		savedEvent = event
		return nil
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeSnapshot{}, agentruntime.AgentEvent{}, agentruntime.RuntimeCheckpointDeliveryOutbox{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeSnapshot{}, agentruntime.AgentEvent{}, agentruntime.RuntimeCheckpointDeliveryOutbox{}, err
	}
	return savedSnapshot, savedEvent, savedCheckpointOutbox, nil
}

func (r *runtimeRepository) GetToolSetSnapshot(ctx context.Context, invocationID string) (agentruntime.ToolSetSnapshot, error) {
	var row toolSetSnapshotRow
	if err := r.db.WithContext(ctx).Where("invocation_id = ?", strings.TrimSpace(invocationID)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.ToolSetSnapshot{}, agentruntime.ErrNotFound
		}
		return agentruntime.ToolSetSnapshot{}, err
	}
	return toolSetSnapshotFromRow(row), nil
}

func (r *runtimeRepository) SaveToolSetSnapshot(ctx context.Context, item agentruntime.ToolSetSnapshot) (agentruntime.ToolSetSnapshot, error) {
	item.InvocationID = strings.TrimSpace(item.InvocationID)
	if _, err := r.GetInvocation(ctx, item.InvocationID); err != nil {
		return agentruntime.ToolSetSnapshot{}, err
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	} else {
		item.CreatedAt = item.CreatedAt.UTC()
	}
	if err := item.Validate(); err != nil {
		return agentruntime.ToolSetSnapshot{}, err
	}
	var result agentruntime.ToolSetSnapshot
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current toolSetSnapshotRow
		findErr := tx.Where("invocation_id = ?", item.InvocationID).First(&current).Error
		if findErr != nil && !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		if findErr == nil {
			if current.Digest != item.Digest {
				return agentruntime.ErrToolSnapshotConflict
			}
			result = toolSetSnapshotFromRow(current)
			return nil
		}
		tools, marshalErr := json.Marshal(item.Tools)
		if marshalErr != nil {
			return marshalErr
		}
		excluded, marshalErr := json.Marshal(item.Excluded)
		if marshalErr != nil {
			return marshalErr
		}
		row := toolSetSnapshotRow{ID: item.ID, InvocationID: item.InvocationID, CatalogRevision: item.CatalogRevision, PolicyRevision: item.PolicyRevision, ModelProfile: item.ModelProfile, ToolsJSON: string(tools), ExcludedJSON: string(excluded), Digest: item.Digest, CreatedAt: item.CreatedAt}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		result = item
		return nil
	})
	if err != nil {
		return agentruntime.ToolSetSnapshot{}, err
	}
	return result, nil
}

func (r *runtimeRepository) GetModelCapabilitySnapshot(ctx context.Context, invocationID string) (agentruntime.ModelCapabilitySnapshot, error) {
	var row modelCapabilitySnapshotRow
	if err := r.db.WithContext(ctx).Where("invocation_id = ?", strings.TrimSpace(invocationID)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.ModelCapabilitySnapshot{}, agentruntime.ErrNotFound
		}
		return agentruntime.ModelCapabilitySnapshot{}, err
	}
	var snapshot agentruntime.ModelCapabilitySnapshot
	if err := json.Unmarshal([]byte(row.SnapshotJSON), &snapshot); err != nil {
		return agentruntime.ModelCapabilitySnapshot{}, err
	}
	return snapshot, nil
}

func (r *runtimeRepository) SaveModelCapabilitySnapshot(ctx context.Context, item agentruntime.ModelCapabilitySnapshot) (agentruntime.ModelCapabilitySnapshot, error) {
	item.InvocationID = strings.TrimSpace(item.InvocationID)
	if _, err := r.GetInvocation(ctx, item.InvocationID); err != nil {
		return agentruntime.ModelCapabilitySnapshot{}, err
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	} else {
		item.CreatedAt = item.CreatedAt.UTC()
	}
	if err := item.Validate(); err != nil {
		return agentruntime.ModelCapabilitySnapshot{}, err
	}
	var result agentruntime.ModelCapabilitySnapshot
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current modelCapabilitySnapshotRow
		findErr := tx.Where("invocation_id = ?", item.InvocationID).First(&current).Error
		if findErr != nil && !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		if findErr == nil {
			var existing agentruntime.ModelCapabilitySnapshot
			if err := json.Unmarshal([]byte(current.SnapshotJSON), &existing); err != nil {
				return err
			}
			if existing.Result.ProfileSnapshotID != item.Result.ProfileSnapshotID || existing.Result.RequestPlan != item.Result.RequestPlan {
				return agentruntime.ErrCapabilitySnapshotConflict
			}
			result = existing
			return nil
		}
		encoded, marshalErr := json.Marshal(item)
		if marshalErr != nil {
			return marshalErr
		}
		row := modelCapabilitySnapshotRow{ID: item.ID, InvocationID: item.InvocationID, SnapshotJSON: string(encoded), ProfileID: item.Result.ProfileSnapshotID, CreatedAt: item.CreatedAt}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		result = item
		return nil
	})
	if err != nil {
		return agentruntime.ModelCapabilitySnapshot{}, err
	}
	return result, nil
}

func (r *runtimeRepository) CreateEvalRun(ctx context.Context, item agentruntime.EvalRun) error {
	item.InvocationID = strings.TrimSpace(item.InvocationID)
	if _, err := r.GetInvocation(ctx, item.InvocationID); err != nil {
		return err
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	} else {
		item.CreatedAt = item.CreatedAt.UTC()
	}
	if item.FinishedAt != nil {
		finished := item.FinishedAt.UTC()
		item.FinishedAt = &finished
	}
	if err := item.Validate(); err != nil {
		return err
	}
	definition := append([]byte(nil), item.Definition...)
	result := append([]byte(nil), item.Result...)
	return r.db.WithContext(ctx).Create(&evalRunRow{
		ID: item.ID, InvocationID: item.InvocationID, CaseID: item.CaseID, CaseVersion: item.CaseVersion,
		Kind: item.Kind, Status: string(item.Status), Passed: item.Passed, HardFailure: item.HardFailure,
		AssertionCount: item.AssertionCount, FailedAssertions: item.FailedAssertions, HardFailedAssertions: item.HardFailedAssertions,
		DefinitionJSON: string(definition), ResultJSON: string(result), CreatedAt: item.CreatedAt, FinishedAt: item.FinishedAt,
	}).Error
}

func (r *runtimeRepository) GetEvalRun(ctx context.Context, id string) (agentruntime.EvalRun, error) {
	var row evalRunRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.EvalRun{}, agentruntime.ErrNotFound
		}
		return agentruntime.EvalRun{}, err
	}
	return evalRunFromRow(row), nil
}

func (r *runtimeRepository) ListEvalRuns(ctx context.Context, invocationID string) ([]agentruntime.EvalRun, error) {
	query := r.db.WithContext(ctx).Order("created_at ASC, id ASC")
	if invocationID = strings.TrimSpace(invocationID); invocationID != "" {
		query = query.Where("invocation_id = ?", invocationID)
	}
	var rows []evalRunRow
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]agentruntime.EvalRun, 0, len(rows))
	for _, row := range rows {
		result = append(result, evalRunFromRow(row))
	}
	return result, nil
}

func (r *runtimeRepository) GetInstructionSnapshotSet(ctx context.Context, invocationID string) (agentruntime.InstructionSnapshotSet, error) {
	var row instructionSnapshotSetRow
	if err := r.db.WithContext(ctx).Where("invocation_id = ?", strings.TrimSpace(invocationID)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.InstructionSnapshotSet{}, agentruntime.ErrNotFound
		}
		return agentruntime.InstructionSnapshotSet{}, err
	}
	return instructionSnapshotSetFromRow(row), nil
}

func (r *runtimeRepository) ReplaceInstructionSnapshotSet(ctx context.Context, item agentruntime.InstructionSnapshotSet) (agentruntime.InstructionSnapshotSet, error) {
	item.InvocationID = strings.TrimSpace(item.InvocationID)
	item.TargetPath = strings.TrimSpace(item.TargetPath)
	if item.InvocationID == "" {
		return agentruntime.InstructionSnapshotSet{}, fmt.Errorf("%w: invocation_id 不能为空", agentruntime.ErrConflict)
	}
	var result agentruntime.InstructionSnapshotSet
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current instructionSnapshotSetRow
		findErr := tx.Where("invocation_id = ?", item.InvocationID).First(&current).Error
		if findErr != nil && !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		if item.Revision <= 0 {
			item.Revision = current.Revision + 1
			if item.Revision == 1 && errors.Is(findErr, gorm.ErrRecordNotFound) {
				item.Revision = 1
			}
		} else if !errors.Is(findErr, gorm.ErrRecordNotFound) && item.Revision <= current.Revision {
			return agentruntime.ErrConflict
		}
		if item.UpdatedAt.IsZero() {
			item.UpdatedAt = time.Now().UTC()
		} else {
			item.UpdatedAt = item.UpdatedAt.UTC()
		}
		encoded, marshalErr := json.Marshal(item.Snapshots)
		if marshalErr != nil {
			return marshalErr
		}
		row := instructionSnapshotSetRow{ID: "instructions-" + item.InvocationID, InvocationID: item.InvocationID, TargetPath: item.TargetPath, Revision: item.Revision, SnapshotsJSON: string(encoded), UpdatedAt: item.UpdatedAt}
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		result = item
		return nil
	})
	if err != nil {
		return agentruntime.InstructionSnapshotSet{}, err
	}
	return result, nil
}

func invocationToRow(item agentruntime.Invocation) *invocationRow {
	attachments, _ := json.Marshal(item.Attachments)
	delivery, _ := json.Marshal(item.BotDelivery)
	return &invocationRow{ID: item.ID, UserID: item.UserID, IdempotencyKey: item.IdempotencyKey, BotID: item.BotID, ConversationID: item.ConversationID, WorkspaceID: item.WorkspaceID, TargetPath: item.TargetPath, SessionID: item.SessionID, ParentInvocationID: item.ParentInvocationID, ContinuationDecision: string(item.ContinuationDecision), ContinuationSourcePlanRevision: item.ContinuationSourcePlanRevision, ProviderID: item.ProviderID, ModelID: item.ModelID, ConfigSnapshot: item.ConfigSnapshot, Message: item.Message, GroupContext: item.GroupContext, BotDeliveryJSON: string(delivery), Proactive: item.Proactive, AttachmentsJSON: string(attachments), Status: string(item.Status), Error: item.Error, ActiveApprovalID: item.ActiveApprovalID, LeaseOwner: item.LeaseOwner, LeaseExpiresAt: item.LeaseExpiresAt, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, StartedAt: item.StartedAt, FinishedAt: item.FinishedAt}
}

func invocationFromRow(row invocationRow) agentruntime.Invocation {
	var attachments []agent.Attachment
	var delivery *agent.BotDeliveryTarget
	if strings.TrimSpace(row.BotDeliveryJSON) != "" && strings.TrimSpace(row.BotDeliveryJSON) != "null" {
		_ = json.Unmarshal([]byte(row.BotDeliveryJSON), &delivery)
	}
	if strings.TrimSpace(row.AttachmentsJSON) != "" {
		_ = json.Unmarshal([]byte(row.AttachmentsJSON), &attachments)
	}
	return agentruntime.Invocation{ID: row.ID, UserID: row.UserID, IdempotencyKey: row.IdempotencyKey, BotID: row.BotID, ConversationID: row.ConversationID, WorkspaceID: row.WorkspaceID, TargetPath: row.TargetPath, SessionID: row.SessionID, ParentInvocationID: row.ParentInvocationID, ContinuationDecision: agentruntime.WorkflowContinuationDecision(row.ContinuationDecision), ContinuationSourcePlanRevision: row.ContinuationSourcePlanRevision, ProviderID: row.ProviderID, ModelID: row.ModelID, ConfigSnapshot: row.ConfigSnapshot, ConfigSnapshotDigest: agent.RuntimeConfigSnapshotDigest(row.ConfigSnapshot), Message: row.Message, GroupContext: row.GroupContext, BotDelivery: delivery, Proactive: row.Proactive, Attachments: attachments, Status: agentruntime.InvocationStatus(row.Status), Error: row.Error, ActiveApprovalID: row.ActiveApprovalID, LeaseOwner: row.LeaseOwner, LeaseExpiresAt: row.LeaseExpiresAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt}
}

func invocationResumeFromRow(row invocationResumeRow) agentruntime.InvocationResume {
	return agentruntime.InvocationResume{
		ID: row.ID, InvocationID: row.InvocationID, WaitID: row.WaitID, Name: row.Name,
		ResponseJSON: []byte(row.ResponseJSON), RequestDigest: row.RequestDigest, CreatedAt: row.CreatedAt,
	}
}

func worktreeBaselineFromRow(row worktreeBaselineRow) agentruntime.WorktreeBaseline {
	var paths []agentruntime.PathStatus
	if strings.TrimSpace(row.ChangedJSON) != "" {
		_ = json.Unmarshal([]byte(row.ChangedJSON), &paths)
	}
	return agentruntime.WorktreeBaseline{
		InvocationID: row.InvocationID, WorkspaceID: row.WorkspaceID, TargetPath: row.TargetPath,
		RepositoryType: row.RepositoryType, HeadRevision: row.HeadRevision, Branch: row.Branch,
		StatusDigest: row.StatusDigest, ChangedPaths: paths, StatusKnown: row.StatusKnown,
		Truncated: row.Truncated, Revision: row.Revision, CapturedAt: row.CapturedAt,
	}
}

func eventFromRow(row agentEventRow) agentruntime.AgentEvent {
	data := make(map[string]any)
	if strings.TrimSpace(row.DataJSON) != "" {
		_ = json.Unmarshal([]byte(row.DataJSON), &data)
	}
	return agentruntime.AgentEvent{ID: row.ID, InvocationID: row.InvocationID, Sequence: row.Sequence, Type: row.Type, Timestamp: row.Timestamp, Data: data}
}

func approvalFromRow(row approvalRow) agentruntime.Approval {
	args := make(map[string]any)
	if strings.TrimSpace(row.ArgsJSON) != "" {
		_ = json.Unmarshal([]byte(row.ArgsJSON), &args)
	}
	var choices []agentruntime.ApprovalChoice
	if strings.TrimSpace(row.ChoicesJSON) != "" {
		_ = json.Unmarshal([]byte(row.ChoicesJSON), &choices)
	}
	if len(choices) == 0 {
		choices = agentruntime.DefaultApprovalChoices()
	}
	return agentruntime.Approval{ID: row.ID, InvocationID: row.InvocationID, ConversationID: row.ConversationID, ToolCallID: row.ToolCallID, TaskContractVersion: row.TaskContractVersion, ToolName: row.ToolName, OperationID: row.OperationID, OriginalCallID: row.OriginalCallID, ConfirmationCallID: row.ConfirmationCallID, Args: args, Hint: row.Hint, Choices: choices, Status: agentruntime.ApprovalStatus(row.Status), DecisionReason: row.DecisionReason, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, ResolvedAt: row.ResolvedAt}
}

func toolCallFromRow(row toolCallRow) agentruntime.ToolCall {
	args := make(map[string]any)
	if strings.TrimSpace(row.ArgsJSON) != "" {
		_ = json.Unmarshal([]byte(row.ArgsJSON), &args)
	}
	return agentruntime.ToolCall{ID: row.ID, InvocationID: row.InvocationID, ConversationID: row.ConversationID, ToolName: row.ToolName, OriginalCallID: row.OriginalCallID, ConfirmationCallID: row.ConfirmationCallID, OperationID: row.OperationID, ApprovalID: row.ApprovalID, Args: args, Status: agentruntime.ToolCallStatus(row.Status), Error: row.Error, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, FinishedAt: row.FinishedAt}
}

func taskPlanFromRow(row taskPlanRow) agentruntime.TaskPlan {
	steps := make([]agentruntime.PlanStep, 0)
	if strings.TrimSpace(row.StepsJSON) != "" {
		_ = json.Unmarshal([]byte(row.StepsJSON), &steps)
	}
	return agentruntime.TaskPlan{
		ID: row.ID, InvocationID: row.InvocationID, Revision: row.Revision,
		Status: agentruntime.PlanStatus(row.Status), CurrentStepID: row.CurrentStepID, Blocker: row.Blocker,
		Steps: steps, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func taskContractToRow(item agentruntime.TaskContract) (*taskContractRow, error) {
	acceptance, err := json.Marshal(item.AcceptanceCriteria)
	if err != nil {
		return nil, err
	}
	constraints, err := json.Marshal(item.Constraints)
	if err != nil {
		return nil, err
	}
	nonGoals, err := json.Marshal(item.NonGoals)
	if err != nil {
		return nil, err
	}
	externalActions, err := json.Marshal(item.ExternalActions)
	if err != nil {
		return nil, err
	}
	sourceMessageIDs, err := json.Marshal(item.SourceMessageIDs)
	if err != nil {
		return nil, err
	}
	return &taskContractRow{
		ID: item.InvocationID + "-contract-" + fmt.Sprint(item.Version), InvocationID: item.InvocationID, Version: item.Version,
		TaskType: string(item.TaskType), Goal: item.Goal, RequestedOutcome: item.RequestedOutcome,
		MutationAllowed: item.MutationAllowed, ValidationRequired: item.ValidationRequired,
		AcceptanceJSON: string(acceptance), ConstraintsJSON: string(constraints), NonGoalsJSON: string(nonGoals),
		ExternalActionsJSON: string(externalActions), SourceMessageIDsJSON: string(sourceMessageIDs), UpdatedAt: item.UpdatedAt,
	}, nil
}

func taskContractFromRow(row taskContractRow) agentruntime.TaskContract {
	var acceptance []agentruntime.Criterion
	var constraints []agentruntime.Constraint
	var nonGoals []string
	var externalActions []agentruntime.ExternalActionRule
	var sourceMessageIDs []string
	_ = json.Unmarshal([]byte(row.AcceptanceJSON), &acceptance)
	_ = json.Unmarshal([]byte(row.ConstraintsJSON), &constraints)
	_ = json.Unmarshal([]byte(row.NonGoalsJSON), &nonGoals)
	_ = json.Unmarshal([]byte(row.ExternalActionsJSON), &externalActions)
	_ = json.Unmarshal([]byte(row.SourceMessageIDsJSON), &sourceMessageIDs)
	return agentruntime.TaskContract{InvocationID: row.InvocationID, Version: row.Version, TaskType: agentruntime.TaskType(row.TaskType), Goal: row.Goal, RequestedOutcome: row.RequestedOutcome, AcceptanceCriteria: acceptance, Constraints: constraints, NonGoals: nonGoals, MutationAllowed: row.MutationAllowed, ValidationRequired: row.ValidationRequired, ExternalActions: externalActions, SourceMessageIDs: sourceMessageIDs, UpdatedAt: row.UpdatedAt}
}

func contextManifestToRow(item agentruntime.ContextManifest) (*contextManifestRow, error) {
	included, err := json.Marshal(item.Included)
	if err != nil {
		return nil, err
	}
	excluded, err := json.Marshal(item.Excluded)
	if err != nil {
		return nil, err
	}
	warnings, err := json.Marshal(item.Warnings)
	if err != nil {
		return nil, err
	}
	return &contextManifestRow{
		ID: item.ID, InvocationID: item.InvocationID, ModelCallID: item.ModelCallID,
		BuilderVersion: item.BuilderVersion, EstimatorVersion: item.EstimatorVersion, EstimatorName: item.EstimatorName, EstimateQuality: item.EstimateQuality, EstimateMultiplier: item.EstimateMultiplier, Model: item.Model,
		ToolSetSnapshotID: item.ToolSetSnapshotID, ToolSetDigest: item.ToolSetDigest,
		ContextWindow: item.ContextWindow, OutputReserve: item.OutputReserve, SafetyReserve: item.SafetyReserve,
		EstimatedInput: item.EstimatedInput, ActualInput: item.ActualInput, IncludedJSON: string(included),
		ExcludedJSON: string(excluded), WarningsJSON: string(warnings), Digest: item.Digest, CreatedAt: item.CreatedAt,
	}, nil
}

func contextManifestFromRow(row contextManifestRow) agentruntime.ContextManifest {
	var included []agentruntime.ContextManifestItem
	var excluded []agentruntime.ContextExcludedItem
	var warnings []string
	_ = json.Unmarshal([]byte(row.IncludedJSON), &included)
	_ = json.Unmarshal([]byte(row.ExcludedJSON), &excluded)
	_ = json.Unmarshal([]byte(row.WarningsJSON), &warnings)
	return agentruntime.ContextManifest{ID: row.ID, InvocationID: row.InvocationID, ModelCallID: row.ModelCallID, BuilderVersion: row.BuilderVersion, EstimatorVersion: row.EstimatorVersion, EstimatorName: row.EstimatorName, EstimateQuality: row.EstimateQuality, EstimateMultiplier: row.EstimateMultiplier, Model: row.Model, ToolSetSnapshotID: row.ToolSetSnapshotID, ToolSetDigest: row.ToolSetDigest, ContextWindow: row.ContextWindow, OutputReserve: row.OutputReserve, SafetyReserve: row.SafetyReserve, EstimatedInput: row.EstimatedInput, ActualInput: row.ActualInput, Included: included, Excluded: excluded, Warnings: warnings, Digest: row.Digest, CreatedAt: row.CreatedAt}
}

func verificationRunFromRow(row verificationRunRow) agentruntime.VerificationRun {
	return agentruntime.VerificationRun{ID: row.ID, InvocationID: row.InvocationID, PlanStepID: row.PlanStepID, Kind: row.Kind, Command: row.Command, Status: agentruntime.VerificationStatus(row.Status), ExitCode: row.ExitCode, Summary: row.Summary, OutputRef: row.OutputRef, OutputDigest: row.OutputDigest, Revision: row.Revision, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt}
}

func evalRunFromRow(row evalRunRow) agentruntime.EvalRun {
	return agentruntime.EvalRun{
		ID: row.ID, InvocationID: row.InvocationID, CaseID: row.CaseID, CaseVersion: row.CaseVersion,
		Kind: row.Kind, Status: agentruntime.EvalRunStatus(row.Status), Passed: row.Passed, HardFailure: row.HardFailure,
		AssertionCount: row.AssertionCount, FailedAssertions: row.FailedAssertions, HardFailedAssertions: row.HardFailedAssertions,
		Definition: json.RawMessage(row.DefinitionJSON), Result: json.RawMessage(row.ResultJSON), CreatedAt: row.CreatedAt, FinishedAt: row.FinishedAt,
	}
}

func instructionSnapshotSetFromRow(row instructionSnapshotSetRow) agentruntime.InstructionSnapshotSet {
	snapshots := make([]agentruntime.InstructionSnapshot, 0)
	if strings.TrimSpace(row.SnapshotsJSON) != "" {
		_ = json.Unmarshal([]byte(row.SnapshotsJSON), &snapshots)
	}
	return agentruntime.InstructionSnapshotSet{InvocationID: row.InvocationID, TargetPath: row.TargetPath, Revision: row.Revision, Snapshots: snapshots, UpdatedAt: row.UpdatedAt}
}

func toolSetSnapshotFromRow(row toolSetSnapshotRow) agentruntime.ToolSetSnapshot {
	var tools []agentruntime.ToolRef
	var excluded []agentruntime.ToolSelectionExclusion
	_ = json.Unmarshal([]byte(row.ToolsJSON), &tools)
	_ = json.Unmarshal([]byte(row.ExcludedJSON), &excluded)
	return agentruntime.ToolSetSnapshot{ID: row.ID, InvocationID: row.InvocationID, CatalogRevision: row.CatalogRevision, PolicyRevision: row.PolicyRevision, ModelProfile: row.ModelProfile, Tools: tools, Excluded: excluded, Digest: row.Digest, CreatedAt: row.CreatedAt}
}

// RuntimeRepository exposes the durable Agent Runtime tables without leaking
// the GORM handle into the rest of the application.
func (s *Store) RuntimeRepository() agentruntime.Repository { return &runtimeRepository{db: s.db} }
