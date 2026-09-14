package runtime

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// RuntimeCheckpointDeliveryEnvelopeVersion is bumped when the signed
	// checkpoint metadata contract changes incompatibly.
	RuntimeCheckpointDeliveryEnvelopeVersion = 1
	// A checkpoint is a metadata projection, not a general snapshot export.
	// Keep its wire limits independent from the event body/source API.
	MaxRuntimeCheckpointDeliveryEnvelopeBytes    = 64 << 10
	MaxRuntimeCheckpointDeliveryProjectionBytes  = 256 << 10
	MaxRuntimeCheckpointDeliveryResponseBytes    = 320 << 10
	MaxRuntimeCheckpointDeliveryIDLength         = 220
	MaxRuntimeCheckpointDeliveryInvocationLength = 512
	MaxRuntimeCheckpointDeliveryPhaseLength      = 80
	MaxRuntimeCheckpointDeliveryCodeLength       = 160
	MaxRuntimeCheckpointDeliveryRefCount         = 128
	DefaultRuntimeCheckpointDeliveryMaxAge       = 10 * time.Minute
	DefaultRuntimeCheckpointDeliveryFutureSkew   = 30 * time.Second
)

var (
	ErrInvalidRuntimeCheckpointDelivery = errors.New("Runtime checkpoint delivery 无效")
	ErrRuntimeCheckpointDeliveryAuth    = errors.New("Runtime checkpoint delivery 认证失败")
	ErrRuntimeCheckpointDeliveryStale   = errors.New("Runtime checkpoint delivery 已过期")
)

// RuntimeCheckpointDeliveryEnvelope signs only the immutable checkpoint
// identity. The checkpoint projection is sent separately and is verified by
// SnapshotDigest; this prevents a metadata envelope from silently changing
// the state it claims to represent.
type RuntimeCheckpointDeliveryEnvelope struct {
	Version          int       `json:"version"`
	Source           string    `json:"source"`
	Destination      string    `json:"destination"`
	DeliveryID       string    `json:"delivery_id"`
	InvocationID     string    `json:"invocation_id"`
	SnapshotRevision int64     `json:"snapshot_revision"`
	EventSequence    int64     `json:"event_sequence"`
	SnapshotDigest   string    `json:"snapshot_digest"`
	Timestamp        time.Time `json:"timestamp"`
	Signature        string    `json:"signature,omitempty"`
}

// RuntimeCheckpointWorkflowProjection keeps the branch/boundary information
// required for recovery while omitting user/model prose.
type RuntimeCheckpointWorkflowProjection struct {
	Status            WorkflowCheckpointStatus              `json:"status,omitempty"`
	CurrentBranchID   string                                `json:"current_branch_id,omitempty"`
	ActiveBoundaryIDs []string                              `json:"active_boundary_ids,omitempty"`
	Branches          []RuntimeCheckpointBranchProjection   `json:"branches,omitempty"`
	Boundaries        []RuntimeCheckpointBoundaryProjection `json:"boundaries,omitempty"`
}

type RuntimeCheckpointBranchProjection struct {
	ID              string               `json:"id"`
	ParentBranchID  string               `json:"parent_branch_id,omitempty"`
	HeadBoundaryID  string               `json:"head_boundary_id,omitempty"`
	Status          WorkflowBranchStatus `json:"status"`
	StartedSequence int64                `json:"started_sequence"`
	UpdatedSequence int64                `json:"updated_sequence"`
}

type RuntimeCheckpointBoundaryProjection struct {
	ID               string                 `json:"id"`
	Sequence         int64                  `json:"sequence"`
	Kind             WorkflowBoundaryKind   `json:"kind"`
	Status           WorkflowBoundaryStatus `json:"status"`
	BranchID         string                 `json:"branch_id"`
	ParentBoundaryID string                 `json:"parent_boundary_id,omitempty"`
	StepID           string                 `json:"step_id,omitempty"`
	WaitIDs          []string               `json:"wait_ids,omitempty"`
	PendingWaitIDs   []string               `json:"pending_wait_ids,omitempty"`
	RequestDigest    string                 `json:"request_digest,omitempty"`
	Outcome          string                 `json:"outcome,omitempty"`
	CreatedAt        time.Time              `json:"created_at"`
	ResolvedAt       *time.Time             `json:"resolved_at,omitempty"`
}

// RuntimeCheckpointProjection is the metadata-only, provider-neutral state
// that another Runtime may use to decide whether it can continue an
// Invocation. It deliberately excludes blocker messages, question text,
// tool names, command text, prompt content and verification summaries.
type RuntimeCheckpointProjection struct {
	InvocationID       string                                    `json:"invocation_id"`
	SnapshotRevision   int64                                     `json:"snapshot_revision"`
	EventSequence      int64                                     `json:"event_sequence"`
	ContractVersion    int64                                     `json:"contract_version,omitempty"`
	PlanRevision       int64                                     `json:"plan_revision,omitempty"`
	Phase              string                                    `json:"phase"`
	WorkflowPhase      WorkflowPhase                             `json:"workflow_phase,omitempty"`
	Workflow           RuntimeCheckpointWorkflowProjection       `json:"workflow"`
	ActivePlanStepID   string                                    `json:"active_plan_step_id,omitempty"`
	PendingApprovals   []RuntimeCheckpointRef                    `json:"pending_approvals,omitempty"`
	AppliedChangeSets  []RuntimeCheckpointChangeProjection       `json:"applied_change_sets,omitempty"`
	VerificationRuns   []RuntimeCheckpointVerificationProjection `json:"verification_runs,omitempty"`
	WorkingSetRevision int64                                     `json:"working_set_revision,omitempty"`
	Budget             RuntimeCheckpointBudgetProjection         `json:"budget"`
	BlockerCodes       []string                                  `json:"blocker_codes,omitempty"`
	UnknownStateCodes  []string                                  `json:"unknown_state_codes,omitempty"`
	GeneratedAt        time.Time                                 `json:"generated_at"`
}

type RuntimeCheckpointRef struct {
	ID          string `json:"id"`
	OperationID string `json:"operation_id,omitempty"`
}

type RuntimeCheckpointChangeProjection struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision,omitempty"`
	Status   string `json:"status,omitempty"`
}

type RuntimeCheckpointVerificationProjection struct {
	ID     string `json:"id"`
	Status string `json:"status,omitempty"`
}

type RuntimeCheckpointBudgetProjection struct {
	ContextWindow   int    `json:"context_window,omitempty"`
	OutputReserve   int    `json:"output_reserve,omitempty"`
	SafetyReserve   int    `json:"safety_reserve,omitempty"`
	EstimatedInput  int    `json:"estimated_input,omitempty"`
	ToolCallsUsed   int    `json:"tool_calls_used,omitempty"`
	ToolCallsLimit  int    `json:"tool_calls_limit,omitempty"`
	LastManifestID  string `json:"last_manifest_id,omitempty"`
	BudgetExhausted bool   `json:"budget_exhausted,omitempty"`
}

// RuntimeCheckpointDeliverySourceRepository proves both the snapshot and its
// event high-water mark come from the same source Runtime.
type RuntimeCheckpointDeliverySourceRepository interface {
	RuntimeSnapshotRepository
	ListEvents(context.Context, string, int64, int) ([]AgentEvent, error)
}

// RuntimeCheckpointDeliveryInbox is the destination-side monotonic commit
// boundary. Implementations must retain only the bounded projection and
// metadata, never the source Runtime's prompt or tool payloads.
type RuntimeCheckpointDeliveryInbox interface {
	AcceptRuntimeCheckpointDelivery(context.Context, RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection) (duplicate bool, err error)
	GetRuntimeCheckpointDelivery(context.Context, string) (RuntimeCheckpointDeliveryInboxRecord, error)
}

// RuntimeCheckpointDeliveryReconciler is an optional source transport
// capability. It queries destination metadata without resending the bounded
// projection, allowing a source worker to recover after a remote commit.
type RuntimeCheckpointDeliveryReconciler interface {
	ReconcileCheckpointDelivery(context.Context, RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection) (RuntimeDeliveryStatus, error)
}

type RuntimeCheckpointDeliveryInboxRecord struct {
	Version          int                         `json:"version"`
	DeliveryID       string                      `json:"delivery_id"`
	Source           string                      `json:"source"`
	Destination      string                      `json:"destination"`
	InvocationID     string                      `json:"invocation_id"`
	SnapshotRevision int64                       `json:"snapshot_revision"`
	EventSequence    int64                       `json:"event_sequence"`
	SnapshotDigest   string                      `json:"snapshot_digest"`
	Signature        string                      `json:"signature"`
	Projection       RuntimeCheckpointProjection `json:"projection"`
	ReceivedAt       time.Time                   `json:"received_at"`
}

// RuntimeCheckpointDeliveryOutbox is the source-side durable cursor for one
// metadata-only checkpoint. Unlike the event outbox, it retains the bounded
// projection because a checkpoint is a versioned state image rather than an
// immutable event body that can be fetched by EventID. The projection is
// intentionally hidden from generic JSON/API responses and is only exposed to
// the configured checkpoint transport.
type RuntimeCheckpointDeliveryOutbox struct {
	ID               string                                `json:"id"`
	GroupID          string                                `json:"group_id,omitempty"`
	InvocationID     string                                `json:"invocation_id"`
	Source           string                                `json:"source"`
	Destination      string                                `json:"destination"`
	DeliveryID       string                                `json:"delivery_id"`
	SnapshotRevision int64                                 `json:"snapshot_revision"`
	EventSequence    int64                                 `json:"event_sequence"`
	SnapshotDigest   string                                `json:"snapshot_digest"`
	Projection       RuntimeCheckpointProjection           `json:"-"`
	Status           RuntimeCheckpointDeliveryOutboxStatus `json:"status"`
	Attempt          int                                   `json:"attempt"`
	Revision         int64                                 `json:"revision"`
	AvailableAt      time.Time                             `json:"available_at"`
	LeaseOwner       string                                `json:"-"`
	LeaseExpiresAt   *time.Time                            `json:"-"`
	LastError        string                                `json:"last_error,omitempty"`
	CreatedAt        time.Time                             `json:"created_at"`
	UpdatedAt        time.Time                             `json:"updated_at"`
}

// RuntimeCheckpointDeliveryOutboxStatus is independent from the Invocation
// and Snapshot states. A newer checkpoint may be committed while an older
// delivery is still processing; the destination inbox enforces monotonicity.
type RuntimeCheckpointDeliveryOutboxStatus string

const (
	RuntimeCheckpointDeliveryOutboxQueued         RuntimeCheckpointDeliveryOutboxStatus = "queued"
	RuntimeCheckpointDeliveryOutboxProcessing     RuntimeCheckpointDeliveryOutboxStatus = "processing"
	RuntimeCheckpointDeliveryOutboxCompleted      RuntimeCheckpointDeliveryOutboxStatus = "completed"
	RuntimeCheckpointDeliveryOutboxFailed         RuntimeCheckpointDeliveryOutboxStatus = "failed"
	MaxRuntimeCheckpointDeliveryOutboxAttempts                                          = 8
	MaxRuntimeCheckpointDeliveryOutboxOwnerLength                                       = 160
	MaxRuntimeCheckpointDeliveryOutboxLease                                             = 10 * time.Minute
)

func (s RuntimeCheckpointDeliveryOutboxStatus) terminal() bool {
	return s == RuntimeCheckpointDeliveryOutboxCompleted || s == RuntimeCheckpointDeliveryOutboxFailed
}

// RuntimeCheckpointDeliveryOutboxID derives a stable storage key from the
// route and checkpoint identity. Including source/destination prevents two
// destinations from accidentally sharing a cursor while preserving retry
// idempotency for one route.
func RuntimeCheckpointDeliveryOutboxID(source, destination, deliveryID string) string {
	material := strings.TrimSpace(source) + "\x00" + strings.TrimSpace(destination) + "\x00" + strings.TrimSpace(deliveryID)
	sum := sha256.Sum256([]byte(material))
	return "checkpoint-outbox-" + hex.EncodeToString(sum[:16])
}

func cloneRuntimeCheckpointDeliveryOutbox(item RuntimeCheckpointDeliveryOutbox) RuntimeCheckpointDeliveryOutbox {
	item.Projection = cloneRuntimeCheckpointProjection(item.Projection)
	if item.LeaseExpiresAt != nil {
		value := item.LeaseExpiresAt.UTC()
		item.LeaseExpiresAt = &value
	}
	return item
}

func (o RuntimeCheckpointDeliveryOutbox) normalize(now time.Time) (RuntimeCheckpointDeliveryOutbox, error) {
	o.ID = strings.TrimSpace(o.ID)
	o.GroupID = strings.TrimSpace(o.GroupID)
	o.InvocationID = strings.TrimSpace(o.InvocationID)
	o.Source = strings.TrimSpace(o.Source)
	o.Destination = strings.TrimSpace(o.Destination)
	o.DeliveryID = strings.TrimSpace(o.DeliveryID)
	o.SnapshotDigest = strings.TrimSpace(strings.ToLower(o.SnapshotDigest))
	o.LeaseOwner = strings.TrimSpace(o.LeaseOwner)
	o.LastError = sanitizeRuntimeString(strings.TrimSpace(o.LastError))
	if o.ID == "" || o.InvocationID == "" || o.Source == "" || o.Destination == "" || o.DeliveryID == "" || o.SnapshotRevision <= 0 || o.EventSequence <= 0 {
		return RuntimeCheckpointDeliveryOutbox{}, fmt.Errorf("%w: checkpoint outbox metadata 不完整", ErrInvalidRuntimeCheckpointDelivery)
	}
	if len(o.ID) > MaxRuntimeCheckpointDeliveryIDLength || len(o.GroupID) > MaxRuntimeDeliveryGroupIDLength || len(o.InvocationID) > MaxRuntimeCheckpointDeliveryInvocationLength || len(o.Source) > MaxRuntimeEventDeliverySourceLength || len(o.Destination) > MaxRuntimeEventDeliveryTargetLength || len(o.DeliveryID) > MaxRuntimeCheckpointDeliveryIDLength {
		return RuntimeCheckpointDeliveryOutbox{}, fmt.Errorf("%w: checkpoint outbox metadata 超出长度限制", ErrInvalidRuntimeCheckpointDelivery)
	}
	if !isRuntimeEventDeliveryDigest(o.SnapshotDigest) {
		return RuntimeCheckpointDeliveryOutbox{}, fmt.Errorf("%w: checkpoint outbox digest 无效", ErrInvalidRuntimeCheckpointDelivery)
	}
	if len(o.LeaseOwner) > MaxRuntimeCheckpointDeliveryOutboxOwnerLength {
		return RuntimeCheckpointDeliveryOutbox{}, fmt.Errorf("%w: checkpoint outbox lease owner 超出长度限制", ErrInvalidRuntimeCheckpointDelivery)
	}
	if o.Attempt < 0 || o.Attempt > MaxRuntimeCheckpointDeliveryOutboxAttempts {
		return RuntimeCheckpointDeliveryOutbox{}, fmt.Errorf("%w: checkpoint outbox attempt 超出范围", ErrInvalidRuntimeCheckpointDelivery)
	}
	if o.Projection.InvocationID != o.InvocationID || o.Projection.SnapshotRevision != o.SnapshotRevision || o.Projection.EventSequence != o.EventSequence {
		return RuntimeCheckpointDeliveryOutbox{}, fmt.Errorf("%w: checkpoint outbox projection identity 不一致", ErrRuntimeCheckpointDeliveryAuth)
	}
	if err := o.Projection.Validate(); err != nil {
		return RuntimeCheckpointDeliveryOutbox{}, err
	}
	digest, err := RuntimeCheckpointProjectionDigest(o.Projection)
	if err != nil || digest != o.SnapshotDigest {
		return RuntimeCheckpointDeliveryOutbox{}, fmt.Errorf("%w: checkpoint outbox projection digest 不一致", ErrRuntimeCheckpointDeliveryAuth)
	}
	if o.Status == "" {
		o.Status = RuntimeCheckpointDeliveryOutboxQueued
	}
	switch o.Status {
	case RuntimeCheckpointDeliveryOutboxQueued, RuntimeCheckpointDeliveryOutboxProcessing, RuntimeCheckpointDeliveryOutboxCompleted, RuntimeCheckpointDeliveryOutboxFailed:
	default:
		return RuntimeCheckpointDeliveryOutbox{}, fmt.Errorf("%w: checkpoint outbox status %q 不受支持", ErrInvalidRuntimeCheckpointDelivery, o.Status)
	}
	if o.Status == RuntimeCheckpointDeliveryOutboxProcessing && (o.LeaseOwner == "" || o.LeaseExpiresAt == nil) {
		return RuntimeCheckpointDeliveryOutbox{}, fmt.Errorf("%w: processing checkpoint outbox 必须带租约", ErrInvalidRuntimeCheckpointDelivery)
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
	if o.Status == RuntimeCheckpointDeliveryOutboxProcessing {
		if o.LeaseExpiresAt == nil || o.LeaseExpiresAt.IsZero() {
			return RuntimeCheckpointDeliveryOutbox{}, fmt.Errorf("%w: processing checkpoint outbox lease 无效", ErrInvalidRuntimeCheckpointDelivery)
		}
		expires := o.LeaseExpiresAt.UTC()
		o.LeaseExpiresAt = &expires
		if expires.After(now.Add(MaxRuntimeCheckpointDeliveryOutboxLease)) {
			return RuntimeCheckpointDeliveryOutbox{}, fmt.Errorf("%w: checkpoint outbox lease 超出时长限制", ErrInvalidRuntimeCheckpointDelivery)
		}
	}
	if o.Revision <= 0 {
		o.Revision = 1
	}
	if o.Status.terminal() {
		o.LeaseOwner = ""
		o.LeaseExpiresAt = nil
	} else if o.Status == RuntimeCheckpointDeliveryOutboxQueued {
		o.LeaseOwner = ""
		o.LeaseExpiresAt = nil
	}
	return o, nil
}

// Normalize validates and defaults a checkpoint outbox cursor for storage
// adapters. It is exported so custom repositories can apply the same bounds.
func (o RuntimeCheckpointDeliveryOutbox) Normalize(now time.Time) (RuntimeCheckpointDeliveryOutbox, error) {
	return o.normalize(now)
}

func runtimeCheckpointDeliveryOutboxBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

// RuntimeCheckpointDeliveryOutboxBackoff exposes the bounded retry schedule
// for storage implementations and deterministic tests.
func RuntimeCheckpointDeliveryOutboxBackoff(attempt int) time.Duration {
	return runtimeCheckpointDeliveryOutboxBackoff(attempt)
}

// NewRuntimeCheckpointDeliveryOutbox creates a queued cursor from the exact
// Snapshot and event high-water used to build a checkpoint projection.
func NewRuntimeCheckpointDeliveryOutbox(source, destination string, snapshot RuntimeSnapshot, eventSequence int64) (RuntimeCheckpointDeliveryOutbox, error) {
	envelope, projection, err := NewRuntimeCheckpointDeliveryEnvelope(source, destination, snapshot, eventSequence)
	if err != nil {
		return RuntimeCheckpointDeliveryOutbox{}, err
	}
	now := snapshot.GeneratedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	item := RuntimeCheckpointDeliveryOutbox{
		ID:           RuntimeCheckpointDeliveryOutboxID(envelope.Source, envelope.Destination, envelope.DeliveryID),
		InvocationID: envelope.InvocationID, Source: envelope.Source, Destination: envelope.Destination,
		DeliveryID: envelope.DeliveryID, SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence,
		SnapshotDigest: envelope.SnapshotDigest, Projection: projection, Status: RuntimeCheckpointDeliveryOutboxQueued,
		AvailableAt: now, CreatedAt: now, UpdatedAt: now,
	}
	return item.normalize(now)
}

// Envelope creates an unsigned, fresh-timestamp identity for a retry. The
// transport signs this value immediately before sending so a queued cursor
// cannot expire merely because its source worker was offline.
func (o RuntimeCheckpointDeliveryOutbox) Envelope(now time.Time) (RuntimeCheckpointDeliveryEnvelope, error) {
	normalized, err := o.normalize(now)
	if err != nil {
		return RuntimeCheckpointDeliveryEnvelope{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	return RuntimeCheckpointDeliveryEnvelope{
		Version: RuntimeCheckpointDeliveryEnvelopeVersion, Source: normalized.Source, Destination: normalized.Destination,
		DeliveryID: normalized.DeliveryID, InvocationID: normalized.InvocationID, SnapshotRevision: normalized.SnapshotRevision,
		EventSequence: normalized.EventSequence, SnapshotDigest: normalized.SnapshotDigest, Timestamp: now,
	}.normalize()
}

// Matches reports whether a stored cursor is the exact same route and
// projection. Retry metadata is deliberately ignored so an idempotent
// enqueue cannot reset a live lease or attempt counter.
func (o RuntimeCheckpointDeliveryOutbox) Matches(other RuntimeCheckpointDeliveryOutbox) bool {
	return o.ID == other.ID && o.InvocationID == other.InvocationID && o.Source == other.Source && o.Destination == other.Destination && o.DeliveryID == other.DeliveryID && o.SnapshotRevision == other.SnapshotRevision && o.EventSequence == other.EventSequence && o.SnapshotDigest == other.SnapshotDigest && func() bool {
		left, leftErr := RuntimeCheckpointProjectionDigest(o.Projection)
		right, rightErr := RuntimeCheckpointProjectionDigest(other.Projection)
		return leftErr == nil && rightErr == nil && left == right
	}()
}

// ProjectionMatches compares only the metadata-only projection digest. It is
// used by storage adapters when validating a cursor against a freshly loaded
// Snapshot without comparing retry/lease fields.
func (o RuntimeCheckpointDeliveryOutbox) ProjectionMatches(other RuntimeCheckpointDeliveryOutbox) bool {
	left, leftErr := RuntimeCheckpointProjectionDigest(o.Projection)
	right, rightErr := RuntimeCheckpointProjectionDigest(other.Projection)
	return leftErr == nil && rightErr == nil && left == right
}

func runtimeCheckpointDeliveryInboxRecord(envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection, receivedAt time.Time) RuntimeCheckpointDeliveryInboxRecord {
	return RuntimeCheckpointDeliveryInboxRecord{
		Version: envelope.Version, DeliveryID: envelope.DeliveryID, Source: envelope.Source,
		Destination: envelope.Destination, InvocationID: envelope.InvocationID,
		SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence,
		SnapshotDigest: envelope.SnapshotDigest, Signature: envelope.Signature,
		Projection: cloneRuntimeCheckpointProjection(projection), ReceivedAt: receivedAt.UTC(),
	}
}

func RuntimeCheckpointDeliveryInboxRecordFromEnvelope(envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection, receivedAt time.Time) RuntimeCheckpointDeliveryInboxRecord {
	return runtimeCheckpointDeliveryInboxRecord(envelope, projection, receivedAt)
}

func (r RuntimeCheckpointDeliveryInboxRecord) Matches(envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection) bool {
	if !r.MatchesIdentity(envelope) {
		return false
	}
	digest, err := RuntimeCheckpointProjectionDigest(projection)
	if err != nil || digest != envelope.SnapshotDigest {
		return false
	}
	storedDigest, err := RuntimeCheckpointProjectionDigest(r.Projection)
	return err == nil && storedDigest == digest
}

// MatchesIdentity compares the immutable route and checkpoint identity. The
// signature is intentionally excluded because every retry may refresh its
// timestamp and HMAC while retaining the same delivery ID and digest.
func (r RuntimeCheckpointDeliveryInboxRecord) MatchesIdentity(envelope RuntimeCheckpointDeliveryEnvelope) bool {
	return r.Version == envelope.Version && r.DeliveryID == envelope.DeliveryID && r.Source == envelope.Source &&
		r.Destination == envelope.Destination && r.InvocationID == envelope.InvocationID &&
		r.SnapshotRevision == envelope.SnapshotRevision && r.EventSequence == envelope.EventSequence &&
		r.SnapshotDigest == envelope.SnapshotDigest
}

func (e RuntimeCheckpointDeliveryEnvelope) normalize() (RuntimeCheckpointDeliveryEnvelope, error) {
	e.Source = strings.TrimSpace(e.Source)
	e.Destination = strings.TrimSpace(e.Destination)
	e.DeliveryID = strings.TrimSpace(e.DeliveryID)
	e.InvocationID = strings.TrimSpace(e.InvocationID)
	e.SnapshotDigest = strings.TrimSpace(strings.ToLower(e.SnapshotDigest))
	e.Signature = strings.TrimSpace(strings.ToLower(e.Signature))
	if e.Version != RuntimeCheckpointDeliveryEnvelopeVersion {
		return RuntimeCheckpointDeliveryEnvelope{}, fmt.Errorf("%w: version %d 不受支持", ErrInvalidRuntimeCheckpointDelivery, e.Version)
	}
	if e.Source == "" || e.Destination == "" || e.DeliveryID == "" || e.InvocationID == "" || e.SnapshotRevision <= 0 || e.EventSequence < 0 || e.Timestamp.IsZero() {
		return RuntimeCheckpointDeliveryEnvelope{}, fmt.Errorf("%w: 必填 metadata 不完整", ErrInvalidRuntimeCheckpointDelivery)
	}
	if len(e.Source) > MaxRuntimeEventDeliverySourceLength || len(e.Destination) > MaxRuntimeEventDeliveryTargetLength || len(e.DeliveryID) > MaxRuntimeCheckpointDeliveryIDLength || len(e.InvocationID) > MaxRuntimeCheckpointDeliveryInvocationLength {
		return RuntimeCheckpointDeliveryEnvelope{}, fmt.Errorf("%w: metadata 超出长度限制", ErrInvalidRuntimeCheckpointDelivery)
	}
	if !isRuntimeEventDeliveryDigest(e.SnapshotDigest) {
		return RuntimeCheckpointDeliveryEnvelope{}, fmt.Errorf("%w: snapshot_digest 无效", ErrInvalidRuntimeCheckpointDelivery)
	}
	if e.Signature != "" && !isRuntimeEventDeliveryDigest(e.Signature) {
		return RuntimeCheckpointDeliveryEnvelope{}, fmt.Errorf("%w: signature 无效", ErrInvalidRuntimeCheckpointDelivery)
	}
	e.Timestamp = e.Timestamp.UTC()
	return e, nil
}

func (e RuntimeCheckpointDeliveryEnvelope) Validate() error {
	_, err := e.normalize()
	return err
}

func NormalizeRuntimeCheckpointDeliveryEnvelope(envelope RuntimeCheckpointDeliveryEnvelope) (RuntimeCheckpointDeliveryEnvelope, error) {
	return envelope.normalize()
}

func (e RuntimeCheckpointDeliveryEnvelope) canonicalUnsigned() ([]byte, error) {
	normalized, err := e.normalize()
	if err != nil {
		return nil, err
	}
	normalized.Signature = ""
	return json.Marshal(normalized)
}

func SignRuntimeCheckpointDeliveryEnvelope(envelope RuntimeCheckpointDeliveryEnvelope, secret []byte) (RuntimeCheckpointDeliveryEnvelope, error) {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return RuntimeCheckpointDeliveryEnvelope{}, err
	}
	canonical, err := envelope.canonicalUnsigned()
	if err != nil {
		return RuntimeCheckpointDeliveryEnvelope{}, err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	envelope.Signature = hex.EncodeToString(mac.Sum(nil))
	return envelope.normalize()
}

func VerifyRuntimeCheckpointDeliveryEnvelope(envelope RuntimeCheckpointDeliveryEnvelope, secret []byte) error {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return err
	}
	normalized, err := envelope.normalize()
	if err != nil {
		return err
	}
	if normalized.Signature == "" {
		return fmt.Errorf("%w: 缺少 signature", ErrRuntimeCheckpointDeliveryAuth)
	}
	canonical, err := normalized.canonicalUnsigned()
	if err != nil {
		return err
	}
	provided, err := hex.DecodeString(normalized.Signature)
	if err != nil {
		return fmt.Errorf("%w: signature 编码无效", ErrRuntimeCheckpointDeliveryAuth)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return fmt.Errorf("%w: HMAC 不匹配", ErrRuntimeCheckpointDeliveryAuth)
	}
	return nil
}

// RuntimeCheckpointProjectionDigest hashes the canonical metadata-only
// projection. Callers must validate the projection before using this digest.
func RuntimeCheckpointProjectionDigest(projection RuntimeCheckpointProjection) (string, error) {
	if err := projection.Validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(projection)
	if err != nil {
		return "", fmt.Errorf("%w: projection 编码失败", ErrInvalidRuntimeCheckpointDelivery)
	}
	if len(encoded) > MaxRuntimeCheckpointDeliveryProjectionBytes {
		return "", fmt.Errorf("%w: projection 超过 %d 字节上限", ErrInvalidRuntimeCheckpointDelivery, MaxRuntimeCheckpointDeliveryProjectionBytes)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func runtimeCheckpointDeliveryID(invocationID string, revision, eventSequence int64, digest string) string {
	material := fmt.Sprintf("%s\x00%d\x00%d\x00%s", strings.TrimSpace(invocationID), revision, eventSequence, strings.TrimSpace(digest))
	sum := sha256.Sum256([]byte(material))
	return "checkpoint:" + hex.EncodeToString(sum[:])
}

// NewRuntimeCheckpointDeliveryEnvelope derives a deterministic delivery ID
// from one snapshot and its event high-water mark. It never includes the
// snapshot body or a secret in the ID.
func NewRuntimeCheckpointDeliveryEnvelope(source, destination string, snapshot RuntimeSnapshot, eventSequence int64) (RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection, error) {
	if err := snapshot.Validate(); err != nil {
		return RuntimeCheckpointDeliveryEnvelope{}, RuntimeCheckpointProjection{}, err
	}
	projection, err := NewRuntimeCheckpointProjection(snapshot, eventSequence)
	if err != nil {
		return RuntimeCheckpointDeliveryEnvelope{}, RuntimeCheckpointProjection{}, err
	}
	digest, err := RuntimeCheckpointProjectionDigest(projection)
	if err != nil {
		return RuntimeCheckpointDeliveryEnvelope{}, RuntimeCheckpointProjection{}, err
	}
	source = strings.TrimSpace(source)
	destination = strings.TrimSpace(destination)
	if source == "" || destination == "" {
		return RuntimeCheckpointDeliveryEnvelope{}, RuntimeCheckpointProjection{}, fmt.Errorf("%w: source/destination 不能为空", ErrInvalidRuntimeCheckpointDelivery)
	}
	timestamp := snapshot.GeneratedAt.UTC()
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}
	envelope := RuntimeCheckpointDeliveryEnvelope{
		Version: RuntimeCheckpointDeliveryEnvelopeVersion, Source: source, Destination: destination,
		DeliveryID:   runtimeCheckpointDeliveryID(snapshot.InvocationID, snapshot.Revision, eventSequence, digest),
		InvocationID: snapshot.InvocationID, SnapshotRevision: snapshot.Revision, EventSequence: eventSequence,
		SnapshotDigest: digest, Timestamp: timestamp,
	}
	normalized, err := envelope.normalize()
	if err != nil {
		return RuntimeCheckpointDeliveryEnvelope{}, RuntimeCheckpointProjection{}, err
	}
	return normalized, projection, nil
}

// NewRuntimeCheckpointProjection removes all free-form text from a Runtime
// Snapshot while preserving the branch/boundary metadata needed for a safe
// cross-service recovery decision.
func NewRuntimeCheckpointProjection(snapshot RuntimeSnapshot, eventSequence int64) (RuntimeCheckpointProjection, error) {
	if err := snapshot.Validate(); err != nil {
		return RuntimeCheckpointProjection{}, err
	}
	if eventSequence < 0 {
		return RuntimeCheckpointProjection{}, fmt.Errorf("%w: event_sequence 不能为负数", ErrInvalidRuntimeCheckpointDelivery)
	}
	projection := RuntimeCheckpointProjection{
		InvocationID: snapshot.InvocationID, SnapshotRevision: snapshot.Revision, EventSequence: eventSequence,
		ContractVersion: snapshot.ContractVersion, PlanRevision: snapshot.PlanRevision, Phase: strings.TrimSpace(snapshot.Phase),
		WorkflowPhase: snapshot.WorkflowPhase, ActivePlanStepID: strings.TrimSpace(func() string {
			if snapshot.ActivePlanStep == nil {
				return ""
			}
			return snapshot.ActivePlanStep.ID
		}()), WorkingSetRevision: snapshot.WorkingSetRevision,
		Budget: RuntimeCheckpointBudgetProjection{
			ContextWindow: snapshot.Budget.ContextWindow, OutputReserve: snapshot.Budget.OutputReserve,
			SafetyReserve: snapshot.Budget.SafetyReserve, EstimatedInput: snapshot.Budget.EstimatedInput,
			ToolCallsUsed: snapshot.Budget.ToolCallsUsed, ToolCallsLimit: snapshot.Budget.ToolCallsLimit,
			LastManifestID: strings.TrimSpace(snapshot.Budget.LastManifestID), BudgetExhausted: snapshot.Budget.BudgetExhausted,
		}, GeneratedAt: snapshot.GeneratedAt.UTC(),
	}
	if snapshot.GeneratedAt.IsZero() {
		projection.GeneratedAt = time.Unix(0, 0).UTC()
	}
	projection.Workflow = runtimeCheckpointWorkflowProjection(snapshot.Workflow)
	for _, item := range snapshot.PendingApprovals {
		if id := strings.TrimSpace(item.ID); id != "" {
			projection.PendingApprovals = append(projection.PendingApprovals, RuntimeCheckpointRef{ID: id, OperationID: strings.TrimSpace(item.OperationID)})
		}
	}
	for _, item := range snapshot.AppliedChangeSets {
		if id := strings.TrimSpace(item.ID); id != "" {
			projection.AppliedChangeSets = append(projection.AppliedChangeSets, RuntimeCheckpointChangeProjection{ID: id, Revision: item.Revision, Status: checkpointMetadataCode(item.Status)})
		}
	}
	for _, item := range snapshot.VerificationRuns {
		if id := strings.TrimSpace(item.ID); id != "" {
			projection.VerificationRuns = append(projection.VerificationRuns, RuntimeCheckpointVerificationProjection{ID: id, Status: checkpointMetadataCode(item.Status)})
		}
	}
	for _, item := range snapshot.Blockers {
		if code := checkpointMetadataCode(item.Code); code != "" {
			projection.BlockerCodes = appendUniqueCheckpointCode(projection.BlockerCodes, code)
		}
	}
	for _, item := range snapshot.UnknownStates {
		if code := checkpointMetadataCode(item.Code); code != "" {
			projection.UnknownStateCodes = appendUniqueCheckpointCode(projection.UnknownStateCodes, code)
		}
	}
	sort.SliceStable(projection.PendingApprovals, func(i, j int) bool { return projection.PendingApprovals[i].ID < projection.PendingApprovals[j].ID })
	sort.SliceStable(projection.AppliedChangeSets, func(i, j int) bool { return projection.AppliedChangeSets[i].ID < projection.AppliedChangeSets[j].ID })
	sort.SliceStable(projection.VerificationRuns, func(i, j int) bool { return projection.VerificationRuns[i].ID < projection.VerificationRuns[j].ID })
	sort.Strings(projection.BlockerCodes)
	sort.Strings(projection.UnknownStateCodes)
	if err := projection.Validate(); err != nil {
		return RuntimeCheckpointProjection{}, err
	}
	return projection, nil
}

func runtimeCheckpointWorkflowProjection(workflow WorkflowCheckpoint) RuntimeCheckpointWorkflowProjection {
	projection := RuntimeCheckpointWorkflowProjection{Status: workflow.Status, CurrentBranchID: strings.TrimSpace(workflow.CurrentBranchID), ActiveBoundaryIDs: append([]string(nil), workflow.ActiveBoundaryIDs...)}
	sort.Strings(projection.ActiveBoundaryIDs)
	for _, branch := range workflow.Branches {
		projection.Branches = append(projection.Branches, RuntimeCheckpointBranchProjection{ID: branch.ID, ParentBranchID: branch.ParentBranchID, HeadBoundaryID: branch.HeadBoundaryID, Status: branch.Status, StartedSequence: branch.StartedSequence, UpdatedSequence: branch.UpdatedSequence})
	}
	sort.SliceStable(projection.Branches, func(i, j int) bool {
		if projection.Branches[i].StartedSequence != projection.Branches[j].StartedSequence {
			return projection.Branches[i].StartedSequence < projection.Branches[j].StartedSequence
		}
		return projection.Branches[i].ID < projection.Branches[j].ID
	})
	for _, boundary := range workflow.Boundaries {
		copyBoundary := RuntimeCheckpointBoundaryProjection{ID: boundary.ID, Sequence: boundary.Sequence, Kind: boundary.Kind, Status: boundary.Status, BranchID: boundary.BranchID, ParentBoundaryID: boundary.ParentBoundaryID, StepID: boundary.StepID, WaitIDs: append([]string(nil), boundary.WaitIDs...), PendingWaitIDs: append([]string(nil), boundary.PendingWaitIDs...), RequestDigest: boundary.RequestDigest, Outcome: checkpointOutcome(boundary.Outcome), CreatedAt: boundary.CreatedAt.UTC()}
		if boundary.ResolvedAt != nil {
			value := boundary.ResolvedAt.UTC()
			copyBoundary.ResolvedAt = &value
		}
		projection.Boundaries = append(projection.Boundaries, copyBoundary)
	}
	sort.SliceStable(projection.Boundaries, func(i, j int) bool {
		if projection.Boundaries[i].Sequence != projection.Boundaries[j].Sequence {
			return projection.Boundaries[i].Sequence < projection.Boundaries[j].Sequence
		}
		return projection.Boundaries[i].ID < projection.Boundaries[j].ID
	})
	return projection
}

func checkpointOutcome(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "partial", "resumed", "approved", "rejected", "completed", "cancelled", "failed", "waiting", "unknown":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "unknown"
	}
}

func checkpointMetadataCode(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) > MaxRuntimeCheckpointDeliveryCodeLength {
		return "unknown"
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' || r == ':' {
			continue
		}
		return "unknown"
	}
	return value
}

func appendUniqueCheckpointCode(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func (p RuntimeCheckpointProjection) Validate() error {
	if strings.TrimSpace(p.InvocationID) == "" || len(p.InvocationID) > MaxRuntimeCheckpointDeliveryInvocationLength {
		return fmt.Errorf("%w: invocation_id 无效", ErrInvalidRuntimeCheckpointDelivery)
	}
	if p.SnapshotRevision <= 0 || p.EventSequence < 0 {
		return fmt.Errorf("%w: revision/sequence 无效", ErrInvalidRuntimeCheckpointDelivery)
	}
	if strings.TrimSpace(p.Phase) == "" || len(p.Phase) > MaxRuntimeCheckpointDeliveryPhaseLength {
		return fmt.Errorf("%w: phase 无效", ErrInvalidRuntimeCheckpointDelivery)
	}
	if p.WorkflowPhase != "" && !validWorkflowPhase(p.WorkflowPhase) {
		return fmt.Errorf("%w: workflow_phase 不受支持", ErrInvalidRuntimeCheckpointDelivery)
	}
	if p.ContractVersion < 0 || p.PlanRevision < 0 || p.WorkingSetRevision < 0 {
		return fmt.Errorf("%w: revision 不能为负数", ErrInvalidRuntimeCheckpointDelivery)
	}
	if err := validateWorkflowID(p.ActivePlanStepID, "active_plan_step_id", false); err != nil {
		return err
	}
	if err := validateWorkflowID(p.Budget.LastManifestID, "last_manifest_id", false); err != nil {
		return err
	}
	if p.Budget.ContextWindow < 0 || p.Budget.OutputReserve < 0 || p.Budget.SafetyReserve < 0 || p.Budget.EstimatedInput < 0 || p.Budget.ToolCallsUsed < 0 || p.Budget.ToolCallsLimit < 0 {
		return fmt.Errorf("%w: budget 不能为负数", ErrInvalidRuntimeCheckpointDelivery)
	}
	workflow := WorkflowCheckpoint{Status: p.Workflow.Status, CurrentBranchID: p.Workflow.CurrentBranchID, ActiveBoundaryIDs: append([]string(nil), p.Workflow.ActiveBoundaryIDs...)}
	for _, branch := range p.Workflow.Branches {
		workflow.Branches = append(workflow.Branches, WorkflowBranch{ID: branch.ID, ParentBranchID: branch.ParentBranchID, HeadBoundaryID: branch.HeadBoundaryID, Status: branch.Status, StartedSequence: branch.StartedSequence, UpdatedSequence: branch.UpdatedSequence})
	}
	for _, boundary := range p.Workflow.Boundaries {
		workflow.Boundaries = append(workflow.Boundaries, WorkflowBoundary{ID: boundary.ID, Sequence: boundary.Sequence, Kind: boundary.Kind, Status: boundary.Status, BranchID: boundary.BranchID, ParentBoundaryID: boundary.ParentBoundaryID, StepID: boundary.StepID, WaitIDs: append([]string(nil), boundary.WaitIDs...), PendingWaitIDs: append([]string(nil), boundary.PendingWaitIDs...), RequestDigest: boundary.RequestDigest, Outcome: boundary.Outcome, CreatedAt: boundary.CreatedAt, ResolvedAt: boundary.ResolvedAt})
	}
	if err := workflow.Validate(); err != nil {
		return fmt.Errorf("%w: workflow: %v", ErrInvalidRuntimeCheckpointDelivery, err)
	}
	if len(p.PendingApprovals) > MaxRuntimeCheckpointDeliveryRefCount || len(p.AppliedChangeSets) > MaxRuntimeCheckpointDeliveryRefCount || len(p.VerificationRuns) > MaxRuntimeCheckpointDeliveryRefCount || len(p.BlockerCodes) > MaxRuntimeCheckpointDeliveryRefCount || len(p.UnknownStateCodes) > MaxRuntimeCheckpointDeliveryRefCount {
		return fmt.Errorf("%w: projection 引用数量超过上限", ErrInvalidRuntimeCheckpointDelivery)
	}
	seen := make(map[string]struct{}, len(p.PendingApprovals))
	for _, item := range p.PendingApprovals {
		if err := validateWorkflowID(item.ID, "pending_approval.id", true); err != nil {
			return err
		}
		if err := validateWorkflowID(item.OperationID, "pending_approval.operation_id", false); err != nil {
			return err
		}
		if _, ok := seen[item.ID]; ok {
			return fmt.Errorf("%w: pending approval 重复", ErrInvalidRuntimeCheckpointDelivery)
		}
		seen[item.ID] = struct{}{}
	}
	for _, item := range p.AppliedChangeSets {
		if err := validateWorkflowID(item.ID, "change_set.id", true); err != nil {
			return err
		}
		if item.Revision < 0 || (item.Status != "" && checkpointMetadataCode(item.Status) == "unknown") {
			return fmt.Errorf("%w: change_set metadata 无效", ErrInvalidRuntimeCheckpointDelivery)
		}
	}
	for _, item := range p.VerificationRuns {
		if err := validateWorkflowID(item.ID, "verification.id", true); err != nil {
			return err
		}
		if item.Status != "" && checkpointMetadataCode(item.Status) == "unknown" {
			return fmt.Errorf("%w: verification metadata 无效", ErrInvalidRuntimeCheckpointDelivery)
		}
	}
	for _, code := range append(append([]string(nil), p.BlockerCodes...), p.UnknownStateCodes...) {
		if code == "" || checkpointMetadataCode(code) == "unknown" {
			return fmt.Errorf("%w: metadata code 无效", ErrInvalidRuntimeCheckpointDelivery)
		}
	}
	if p.GeneratedAt.IsZero() {
		return fmt.Errorf("%w: generated_at 不能为空", ErrInvalidRuntimeCheckpointDelivery)
	}
	return nil
}

func normalizeRuntimeCheckpointDeliveryPair(envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection, error) {
	normalized, err := NormalizeRuntimeCheckpointDeliveryEnvelope(envelope)
	if err != nil {
		return RuntimeCheckpointDeliveryEnvelope{}, RuntimeCheckpointProjection{}, err
	}
	if err := projection.Validate(); err != nil {
		return RuntimeCheckpointDeliveryEnvelope{}, RuntimeCheckpointProjection{}, err
	}
	if projection.InvocationID != normalized.InvocationID || projection.SnapshotRevision != normalized.SnapshotRevision || projection.EventSequence != normalized.EventSequence {
		return RuntimeCheckpointDeliveryEnvelope{}, RuntimeCheckpointProjection{}, fmt.Errorf("%w: projection metadata 不一致", ErrRuntimeCheckpointDeliveryAuth)
	}
	digest, err := RuntimeCheckpointProjectionDigest(projection)
	if err != nil {
		return RuntimeCheckpointDeliveryEnvelope{}, RuntimeCheckpointProjection{}, err
	}
	if digest != normalized.SnapshotDigest {
		return RuntimeCheckpointDeliveryEnvelope{}, RuntimeCheckpointProjection{}, fmt.Errorf("%w: projection digest 不一致", ErrRuntimeCheckpointDeliveryAuth)
	}
	return normalized, cloneRuntimeCheckpointProjection(projection), nil
}

// NormalizeRuntimeCheckpointDeliveryPair validates and returns defensive
// copies of the signed identity and its metadata-only projection. Storage
// implementations use this helper before applying their monotonic CAS.
func NormalizeRuntimeCheckpointDeliveryPair(envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection, error) {
	return normalizeRuntimeCheckpointDeliveryPair(envelope, projection)
}

func cloneRuntimeCheckpointProjection(item RuntimeCheckpointProjection) RuntimeCheckpointProjection {
	item.PendingApprovals = append([]RuntimeCheckpointRef(nil), item.PendingApprovals...)
	item.AppliedChangeSets = append([]RuntimeCheckpointChangeProjection(nil), item.AppliedChangeSets...)
	item.VerificationRuns = append([]RuntimeCheckpointVerificationProjection(nil), item.VerificationRuns...)
	item.BlockerCodes = append([]string(nil), item.BlockerCodes...)
	item.UnknownStateCodes = append([]string(nil), item.UnknownStateCodes...)
	item.Workflow.ActiveBoundaryIDs = append([]string(nil), item.Workflow.ActiveBoundaryIDs...)
	item.Workflow.Branches = append([]RuntimeCheckpointBranchProjection(nil), item.Workflow.Branches...)
	item.Workflow.Boundaries = append([]RuntimeCheckpointBoundaryProjection(nil), item.Workflow.Boundaries...)
	for index := range item.Workflow.Boundaries {
		item.Workflow.Boundaries[index].WaitIDs = append([]string(nil), item.Workflow.Boundaries[index].WaitIDs...)
		item.Workflow.Boundaries[index].PendingWaitIDs = append([]string(nil), item.Workflow.Boundaries[index].PendingWaitIDs...)
		if item.Workflow.Boundaries[index].ResolvedAt != nil {
			value := *item.Workflow.Boundaries[index].ResolvedAt
			item.Workflow.Boundaries[index].ResolvedAt = &value
		}
	}
	return item
}

func (r RuntimeCheckpointDeliveryInboxRecord) clone() RuntimeCheckpointDeliveryInboxRecord {
	r.Projection = cloneRuntimeCheckpointProjection(r.Projection)
	return r
}

func readRuntimeCheckpointDeliveryEnvelope(request *http.Request, configuredMax int64) (RuntimeCheckpointDeliveryEnvelope, error) {
	if request == nil || request.Body == nil {
		return RuntimeCheckpointDeliveryEnvelope{}, fmt.Errorf("%w: 请求 body 为空", ErrInvalidRuntimeCheckpointDelivery)
	}
	maxBody := configuredMax
	if maxBody <= 0 || maxBody > MaxRuntimeCheckpointDeliveryEnvelopeBytes {
		maxBody = MaxRuntimeCheckpointDeliveryEnvelopeBytes
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBody+1))
	if err != nil {
		return RuntimeCheckpointDeliveryEnvelope{}, fmt.Errorf("%w: 读取请求失败", ErrInvalidRuntimeCheckpointDelivery)
	}
	if int64(len(body)) > maxBody {
		return RuntimeCheckpointDeliveryEnvelope{}, fmt.Errorf("%w: 请求超过 %d 字节上限", ErrInvalidRuntimeCheckpointDelivery, maxBody)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var envelope RuntimeCheckpointDeliveryEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return RuntimeCheckpointDeliveryEnvelope{}, fmt.Errorf("%w: JSON 无效", ErrInvalidRuntimeCheckpointDelivery)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeCheckpointDeliveryEnvelope{}, fmt.Errorf("%w: 请求包含多个 JSON 文档", ErrInvalidRuntimeCheckpointDelivery)
	}
	normalized, err := NormalizeRuntimeCheckpointDeliveryEnvelope(envelope)
	if err != nil {
		return RuntimeCheckpointDeliveryEnvelope{}, err
	}
	if header := strings.TrimSpace(request.Header.Get("X-Abot-Checkpoint-Delivery-Version")); header != "" && header != strconv.Itoa(normalized.Version) {
		return RuntimeCheckpointDeliveryEnvelope{}, fmt.Errorf("%w: header version 与 body 不一致", ErrInvalidRuntimeCheckpointDelivery)
	}
	if header := strings.TrimSpace(strings.ToLower(request.Header.Get("X-Abot-Checkpoint-Delivery-Signature"))); header != "" && header != normalized.Signature {
		return RuntimeCheckpointDeliveryEnvelope{}, fmt.Errorf("%w: header signature 与 body 不一致", ErrRuntimeCheckpointDeliveryAuth)
	}
	if header := strings.TrimSpace(request.Header.Get("Idempotency-Key")); header != "" && header != normalized.DeliveryID {
		return RuntimeCheckpointDeliveryEnvelope{}, fmt.Errorf("%w: idempotency key 与 delivery_id 不一致", ErrInvalidRuntimeCheckpointDelivery)
	}
	return normalized, nil
}

func validateRuntimeCheckpointDeliveryTimestamp(timestamp, now time.Time, maxAge, maxFutureSkew time.Duration) error {
	if maxAge <= 0 || maxFutureSkew < 0 {
		return fmt.Errorf("%w: timestamp policy 无效", ErrInvalidRuntimeCheckpointDelivery)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	timestamp = timestamp.UTC()
	if timestamp.After(now.Add(maxFutureSkew)) {
		return fmt.Errorf("%w: timestamp 位于允许未来窗口之外", ErrRuntimeCheckpointDeliveryAuth)
	}
	if now.Sub(timestamp) > maxAge {
		return fmt.Errorf("%w: timestamp 已过期", ErrRuntimeCheckpointDeliveryAuth)
	}
	return nil
}

type RuntimeCheckpointDeliverySourceResponse struct {
	Envelope   RuntimeCheckpointDeliveryEnvelope `json:"envelope"`
	Projection RuntimeCheckpointProjection       `json:"projection"`
}

// RuntimeCheckpointDeliverySource exposes one exact, authenticated snapshot
// revision. It refuses to serve a snapshot whose event high-water mark no
// longer matches the signed request.
type RuntimeCheckpointDeliverySource struct {
	Source        string
	Destination   string
	SharedSecret  []byte
	Repository    RuntimeCheckpointDeliverySourceRepository
	MaxAge        time.Duration
	MaxFutureSkew time.Duration
	MaxBodyBytes  int64
	MaxResponse   int64
	Now           func() time.Time
}

func NewRuntimeCheckpointDeliverySource(source, destination string, secret []byte, repository RuntimeCheckpointDeliverySourceRepository) (*RuntimeCheckpointDeliverySource, error) {
	if strings.TrimSpace(source) == "" || len(strings.TrimSpace(source)) > MaxRuntimeEventDeliverySourceLength || strings.TrimSpace(destination) == "" || len(strings.TrimSpace(destination)) > MaxRuntimeEventDeliveryTargetLength {
		return nil, fmt.Errorf("%w: source/destination 无效", ErrInvalidRuntimeCheckpointDelivery)
	}
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return nil, err
	}
	if repository == nil {
		return nil, fmt.Errorf("%w: repository 不能为空", ErrInvalidRuntimeCheckpointDelivery)
	}
	return &RuntimeCheckpointDeliverySource{Source: strings.TrimSpace(source), Destination: strings.TrimSpace(destination), SharedSecret: append([]byte(nil), secret...), Repository: repository, MaxAge: DefaultRuntimeCheckpointDeliveryMaxAge, MaxFutureSkew: DefaultRuntimeCheckpointDeliveryFutureSkew, MaxBodyBytes: MaxRuntimeCheckpointDeliveryEnvelopeBytes, MaxResponse: MaxRuntimeCheckpointDeliveryResponseBytes, Now: time.Now}, nil
}

func (s *RuntimeCheckpointDeliverySource) Handler() http.Handler { return s }

func (s *RuntimeCheckpointDeliverySource) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if s == nil {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusInternalServerError, ErrInvalidRuntimeCheckpointDelivery)
		return
	}
	if request.Method != http.MethodPost {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusMethodNotAllowed, fmt.Errorf("%w: 只支持 POST", ErrInvalidRuntimeCheckpointDelivery))
		return
	}
	envelope, err := readRuntimeCheckpointDeliveryEnvelope(request, s.MaxBodyBytes)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrRuntimeCheckpointDeliveryAuth) {
			status = http.StatusUnauthorized
		}
		writeRuntimeCheckpointDeliveryError(writer, status, err)
		return
	}
	if envelope.Source != s.Source || envelope.Destination != s.Destination {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusForbidden, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeCheckpointDeliveryAuth))
		return
	}
	if err := VerifyRuntimeCheckpointDeliveryEnvelope(envelope, s.SharedSecret); err != nil {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusUnauthorized, err)
		return
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	if err := validateRuntimeCheckpointDeliveryTimestamp(envelope.Timestamp, now, s.MaxAge, s.MaxFutureSkew); err != nil {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusUnauthorized, err)
		return
	}
	snapshot, err := s.Repository.GetRuntimeSnapshot(request.Context(), envelope.InvocationID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeRuntimeCheckpointDeliveryError(writer, http.StatusNotFound, ErrNotFound)
		} else {
			writeRuntimeCheckpointDeliveryError(writer, http.StatusInternalServerError, err)
		}
		return
	}
	if snapshot.Revision != envelope.SnapshotRevision || snapshot.InvocationID != envelope.InvocationID {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusConflict, fmt.Errorf("%w: snapshot revision 不一致", ErrRuntimeCheckpointDeliveryAuth))
		return
	}
	eventSequence, err := runtimeCheckpointLastEventSequence(request.Context(), s.Repository, envelope.InvocationID)
	if err != nil {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusInternalServerError, err)
		return
	}
	if eventSequence != envelope.EventSequence {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusConflict, fmt.Errorf("%w: event high-water mark 不一致", ErrRuntimeCheckpointDeliveryAuth))
		return
	}
	projection, err := NewRuntimeCheckpointProjection(snapshot, eventSequence)
	if err != nil {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusInternalServerError, err)
		return
	}
	digest, err := RuntimeCheckpointProjectionDigest(projection)
	if err != nil {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusInternalServerError, err)
		return
	}
	if digest != envelope.SnapshotDigest {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusConflict, fmt.Errorf("%w: snapshot digest 不一致", ErrRuntimeCheckpointDeliveryAuth))
		return
	}
	response := RuntimeCheckpointDeliverySourceResponse{Envelope: envelope, Projection: projection}
	body, err := json.Marshal(response)
	if err != nil {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusInternalServerError, err)
		return
	}
	maxResponse := s.MaxResponse
	if maxResponse <= 0 || maxResponse > MaxRuntimeCheckpointDeliveryResponseBytes {
		maxResponse = MaxRuntimeCheckpointDeliveryResponseBytes
	}
	if int64(len(body)) > maxResponse {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusRequestEntityTooLarge, fmt.Errorf("%w: response 超过 %d 字节上限", ErrInvalidRuntimeCheckpointDelivery, maxResponse))
		return
	}
	writeRuntimeCheckpointDeliveryJSON(writer, http.StatusOK, response)
}

func runtimeCheckpointLastEventSequence(ctx context.Context, repository RuntimeCheckpointDeliverySourceRepository, invocationID string) (int64, error) {
	var after int64
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		page, err := repository.ListEvents(ctx, invocationID, after, runtimeEventPageSize)
		if err != nil {
			return 0, err
		}
		if len(page) == 0 {
			return after, nil
		}
		for _, event := range page {
			if event.Sequence <= after {
				return 0, fmt.Errorf("%w: event sequence 不递增", ErrInvalidRuntimeCheckpointDelivery)
			}
			after = event.Sequence
			count++
			if count > maxRuntimeEventReplay {
				return 0, fmt.Errorf("%w: event replay 超过上限", ErrEventReplayLimit)
			}
		}
	}
}

// RuntimeCheckpointDeliveryHTTPSourceClient fetches one exact checkpoint and
// verifies both the signed identity and the projection digest before returning
// it to a consumer.
type RuntimeCheckpointDeliveryHTTPSourceClient struct {
	Endpoint     string
	Source       string
	Destination  string
	SharedSecret []byte
	Client       *http.Client
	MaxBodyBytes int64
}

func NewRuntimeCheckpointDeliveryHTTPSourceClient(endpoint, source, destination string, secret []byte, client *http.Client) (*RuntimeCheckpointDeliveryHTTPSourceClient, error) {
	endpoint = strings.TrimSpace(endpoint)
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("%w: endpoint 必须是 http/https URL", ErrInvalidRuntimeCheckpointDelivery)
	}
	if strings.TrimSpace(source) == "" || strings.TrimSpace(destination) == "" {
		return nil, fmt.Errorf("%w: source/destination 无效", ErrInvalidRuntimeCheckpointDelivery)
	}
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &RuntimeCheckpointDeliveryHTTPSourceClient{Endpoint: endpoint, Source: strings.TrimSpace(source), Destination: strings.TrimSpace(destination), SharedSecret: append([]byte(nil), secret...), Client: client, MaxBodyBytes: MaxRuntimeCheckpointDeliveryResponseBytes}, nil
}

func (c *RuntimeCheckpointDeliveryHTTPSourceClient) FetchCheckpoint(ctx context.Context, envelope RuntimeCheckpointDeliveryEnvelope) (RuntimeCheckpointProjection, error) {
	if c == nil {
		return RuntimeCheckpointProjection{}, fmt.Errorf("%w: source client 为空", ErrInvalidRuntimeCheckpointDelivery)
	}
	normalized, err := NormalizeRuntimeCheckpointDeliveryEnvelope(envelope)
	if err != nil {
		return RuntimeCheckpointProjection{}, err
	}
	if normalized.Source != c.Source || normalized.Destination != c.Destination {
		return RuntimeCheckpointProjection{}, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeCheckpointDeliveryAuth)
	}
	signed, err := SignRuntimeCheckpointDeliveryEnvelope(normalized, c.SharedSecret)
	if err != nil {
		return RuntimeCheckpointProjection{}, err
	}
	body, err := json.Marshal(signed)
	if err != nil {
		return RuntimeCheckpointProjection{}, fmt.Errorf("%w: request 编码失败", ErrInvalidRuntimeCheckpointDelivery)
	}
	if len(body) > MaxRuntimeCheckpointDeliveryEnvelopeBytes {
		return RuntimeCheckpointProjection{}, fmt.Errorf("%w: request 超过字节上限", ErrInvalidRuntimeCheckpointDelivery)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return RuntimeCheckpointProjection{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Abot-Checkpoint-Delivery-Version", strconv.Itoa(signed.Version))
	request.Header.Set("X-Abot-Checkpoint-Delivery-Signature", signed.Signature)
	request.Header.Set("Idempotency-Key", signed.DeliveryID)
	response, err := c.Client.Do(request)
	if err != nil {
		return RuntimeCheckpointProjection{}, fmt.Errorf("Runtime checkpoint source 请求失败: %w", err)
	}
	defer response.Body.Close()
	maxBody := c.MaxBodyBytes
	if maxBody <= 0 || maxBody > MaxRuntimeCheckpointDeliveryResponseBytes {
		maxBody = MaxRuntimeCheckpointDeliveryResponseBytes
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return RuntimeCheckpointProjection{}, fmt.Errorf("Runtime checkpoint source 响应读取失败: %w", err)
	}
	if int64(len(responseBody)) > maxBody {
		return RuntimeCheckpointProjection{}, fmt.Errorf("%w: response 超过字节上限", ErrInvalidRuntimeCheckpointDelivery)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(SanitizeRuntimeString(string(responseBody)))
		if len(message) > 512 {
			message = message[:512]
		}
		if message == "" {
			message = "source returned no diagnostic"
		}
		return RuntimeCheckpointProjection{}, fmt.Errorf("Runtime checkpoint source 返回 HTTP %d: %s", response.StatusCode, message)
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	var payload RuntimeCheckpointDeliverySourceResponse
	if err := decoder.Decode(&payload); err != nil {
		return RuntimeCheckpointProjection{}, fmt.Errorf("%w: response JSON 无效", ErrInvalidRuntimeCheckpointDelivery)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeCheckpointProjection{}, fmt.Errorf("%w: response 包含多个 JSON 文档", ErrInvalidRuntimeCheckpointDelivery)
	}
	returned, err := NormalizeRuntimeCheckpointDeliveryEnvelope(payload.Envelope)
	if err != nil {
		return RuntimeCheckpointProjection{}, err
	}
	if returned != signed {
		return RuntimeCheckpointProjection{}, fmt.Errorf("%w: response envelope 不一致", ErrRuntimeCheckpointDeliveryAuth)
	}
	if err := VerifyRuntimeCheckpointDeliveryEnvelope(returned, c.SharedSecret); err != nil {
		return RuntimeCheckpointProjection{}, err
	}
	if err := payload.Projection.Validate(); err != nil {
		return RuntimeCheckpointProjection{}, err
	}
	if payload.Projection.InvocationID != signed.InvocationID || payload.Projection.SnapshotRevision != signed.SnapshotRevision || payload.Projection.EventSequence != signed.EventSequence {
		return RuntimeCheckpointProjection{}, fmt.Errorf("%w: response projection metadata 不一致", ErrRuntimeCheckpointDeliveryAuth)
	}
	digest, err := RuntimeCheckpointProjectionDigest(payload.Projection)
	if err != nil {
		return RuntimeCheckpointProjection{}, err
	}
	if digest != signed.SnapshotDigest {
		return RuntimeCheckpointProjection{}, fmt.Errorf("%w: response projection digest 不一致", ErrRuntimeCheckpointDeliveryAuth)
	}
	return cloneRuntimeCheckpointProjection(payload.Projection), nil
}

type RuntimeCheckpointDeliveryReceipt struct {
	Version          int    `json:"version"`
	DeliveryID       string `json:"delivery_id"`
	InvocationID     string `json:"invocation_id"`
	SnapshotRevision int64  `json:"snapshot_revision"`
	EventSequence    int64  `json:"event_sequence"`
	SnapshotDigest   string `json:"snapshot_digest"`
	// Phase is populated by the optional prepare/commit transport.  It is
	// omitted by the legacy one-phase Deliver path for wire compatibility.
	Phase     string `json:"phase,omitempty"`
	Duplicate bool   `json:"duplicate"`
}

const (
	RuntimeCheckpointDeliveryTransactionPhasePrepared  = "prepared"
	RuntimeCheckpointDeliveryTransactionPhaseCommitted = "committed"
	RuntimeCheckpointDeliveryTransactionPhaseAborted   = "aborted"
)

// ValidateAgainst proves that a transport acknowledged the exact checkpoint
// that was claimed. A transport implementation normally performs this check
// while decoding its HTTP response, but the Coordinator repeats it so a
// custom in-process adapter cannot accidentally acknowledge a different
// delivery and release the source lease.
func (r RuntimeCheckpointDeliveryReceipt) ValidateAgainst(envelope RuntimeCheckpointDeliveryEnvelope) error {
	normalized, err := NormalizeRuntimeCheckpointDeliveryEnvelope(envelope)
	if err != nil {
		return err
	}
	if r.Version != normalized.Version || strings.TrimSpace(r.DeliveryID) != normalized.DeliveryID || strings.TrimSpace(r.InvocationID) != normalized.InvocationID || r.SnapshotRevision != normalized.SnapshotRevision || r.EventSequence != normalized.EventSequence || strings.TrimSpace(strings.ToLower(r.SnapshotDigest)) != normalized.SnapshotDigest {
		return fmt.Errorf("%w: receipt metadata 不一致", ErrRuntimeCheckpointDeliveryAuth)
	}
	return nil
}

func (r RuntimeCheckpointDeliveryReceipt) ValidateAgainstPhase(envelope RuntimeCheckpointDeliveryEnvelope, phase string) error {
	if err := r.ValidateAgainst(envelope); err != nil {
		return err
	}
	if strings.TrimSpace(r.Phase) != strings.TrimSpace(phase) {
		return fmt.Errorf("%w: receipt phase %q 与期望 %q 不一致", ErrRuntimeCheckpointDeliveryAuth, r.Phase, phase)
	}
	return nil
}

// RuntimeCheckpointDeliveryReceiver accepts a signed projection at the
// destination. Monotonic revision/sequence checks live in the inbox so an
// HTTP retry cannot overwrite a newer checkpoint.
type RuntimeCheckpointDeliveryReceiver struct {
	Source        string
	Destination   string
	SharedSecret  []byte
	Inbox         RuntimeCheckpointDeliveryInbox
	MaxAge        time.Duration
	MaxFutureSkew time.Duration
	MaxBodyBytes  int64
	Now           func() time.Time
}

func NewRuntimeCheckpointDeliveryReceiver(source, destination string, secret []byte, inbox RuntimeCheckpointDeliveryInbox) (*RuntimeCheckpointDeliveryReceiver, error) {
	if strings.TrimSpace(source) == "" || strings.TrimSpace(destination) == "" {
		return nil, fmt.Errorf("%w: source/destination 无效", ErrInvalidRuntimeCheckpointDelivery)
	}
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return nil, err
	}
	if inbox == nil {
		return nil, fmt.Errorf("%w: inbox 不能为空", ErrInvalidRuntimeCheckpointDelivery)
	}
	return &RuntimeCheckpointDeliveryReceiver{Source: strings.TrimSpace(source), Destination: strings.TrimSpace(destination), SharedSecret: append([]byte(nil), secret...), Inbox: inbox, MaxAge: DefaultRuntimeCheckpointDeliveryMaxAge, MaxFutureSkew: DefaultRuntimeCheckpointDeliveryFutureSkew, MaxBodyBytes: MaxRuntimeCheckpointDeliveryResponseBytes, Now: time.Now}, nil
}

func (r *RuntimeCheckpointDeliveryReceiver) Handler() http.Handler { return r }

func (r *RuntimeCheckpointDeliveryReceiver) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if r == nil {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusInternalServerError, ErrInvalidRuntimeCheckpointDelivery)
		return
	}
	if request != nil && request.URL != nil && strings.HasSuffix(strings.TrimRight(request.URL.Path, "/"), "/status") {
		r.ServeStatusHTTP(writer, request)
		return
	}
	if request.Method != http.MethodPost {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusMethodNotAllowed, fmt.Errorf("%w: 只支持 POST", ErrInvalidRuntimeCheckpointDelivery))
		return
	}
	maxBody := r.MaxBodyBytes
	if maxBody <= 0 || maxBody > MaxRuntimeCheckpointDeliveryResponseBytes {
		maxBody = MaxRuntimeCheckpointDeliveryResponseBytes
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBody+1))
	if err != nil || int64(len(body)) > maxBody {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusBadRequest, fmt.Errorf("%w: 请求 body 超过上限或读取失败", ErrInvalidRuntimeCheckpointDelivery))
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var payload RuntimeCheckpointDeliverySourceResponse
	if err := decoder.Decode(&payload); err != nil {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusBadRequest, fmt.Errorf("%w: request JSON 无效", ErrInvalidRuntimeCheckpointDelivery))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusBadRequest, fmt.Errorf("%w: request 包含多个 JSON 文档", ErrInvalidRuntimeCheckpointDelivery))
		return
	}
	envelope, err := NormalizeRuntimeCheckpointDeliveryEnvelope(payload.Envelope)
	if err != nil {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusBadRequest, err)
		return
	}
	if envelope.Source != r.Source || envelope.Destination != r.Destination {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusForbidden, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeCheckpointDeliveryAuth))
		return
	}
	if err := VerifyRuntimeCheckpointDeliveryEnvelope(envelope, r.SharedSecret); err != nil {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusUnauthorized, err)
		return
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	if err := validateRuntimeCheckpointDeliveryTimestamp(envelope.Timestamp, now, r.MaxAge, r.MaxFutureSkew); err != nil {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusUnauthorized, err)
		return
	}
	if err := payload.Projection.Validate(); err != nil {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusBadRequest, err)
		return
	}
	if payload.Projection.InvocationID != envelope.InvocationID || payload.Projection.SnapshotRevision != envelope.SnapshotRevision || payload.Projection.EventSequence != envelope.EventSequence {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusConflict, fmt.Errorf("%w: projection metadata 不一致", ErrRuntimeCheckpointDeliveryAuth))
		return
	}
	digest, err := RuntimeCheckpointProjectionDigest(payload.Projection)
	if err != nil || digest != envelope.SnapshotDigest {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusConflict, fmt.Errorf("%w: projection digest 不一致", ErrRuntimeCheckpointDeliveryAuth))
		return
	}
	duplicate, err := r.Inbox.AcceptRuntimeCheckpointDelivery(request.Context(), envelope, payload.Projection)
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, ErrRuntimeCheckpointDeliveryStale) {
			status = http.StatusConflict
		}
		writeRuntimeCheckpointDeliveryError(writer, status, err)
		return
	}
	receipt := RuntimeCheckpointDeliveryReceipt{Version: envelope.Version, DeliveryID: envelope.DeliveryID, InvocationID: envelope.InvocationID, SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence, SnapshotDigest: envelope.SnapshotDigest, Duplicate: duplicate}
	writeRuntimeCheckpointDeliveryJSON(writer, http.StatusOK, receipt)
}

// RuntimeCheckpointDeliveryHTTPTransport pushes a signed projection to a
// receiver. It is an optional hook; no Coordinator path is enabled by this
// constructor until a host explicitly wires it to a checkpoint outbox.
type RuntimeCheckpointDeliveryHTTPTransport struct {
	Endpoint      string
	Source        string
	Destination   string
	SharedSecret  []byte
	Client        *http.Client
	MaxBodyBytes  int64
	transactional bool
}

func NewRuntimeCheckpointDeliveryHTTPTransport(endpoint, source, destination string, secret []byte, client *http.Client) (*RuntimeCheckpointDeliveryHTTPTransport, error) {
	endpoint = strings.TrimSpace(endpoint)
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("%w: endpoint 必须是 http/https URL", ErrInvalidRuntimeCheckpointDelivery)
	}
	if strings.TrimSpace(source) == "" || strings.TrimSpace(destination) == "" {
		return nil, fmt.Errorf("%w: source/destination 无效", ErrInvalidRuntimeCheckpointDelivery)
	}
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &RuntimeCheckpointDeliveryHTTPTransport{Endpoint: endpoint, Source: strings.TrimSpace(source), Destination: strings.TrimSpace(destination), SharedSecret: append([]byte(nil), secret...), Client: client, MaxBodyBytes: MaxRuntimeCheckpointDeliveryResponseBytes}, nil
}

// NewRuntimeCheckpointDeliveryHTTPTransactionalTransport opts an HTTP
// transport into the prepare/commit checkpoint protocol.  The original
// constructor intentionally remains one-phase so existing receivers exposing
// only /checkpoint keep working until both sides are upgraded.
func NewRuntimeCheckpointDeliveryHTTPTransactionalTransport(endpoint, source, destination string, secret []byte, client *http.Client) (*RuntimeCheckpointDeliveryHTTPTransport, error) {
	transport, err := NewRuntimeCheckpointDeliveryHTTPTransport(endpoint, source, destination, secret, client)
	if err != nil {
		return nil, err
	}
	transport.transactional = true
	return transport, nil
}

func (t *RuntimeCheckpointDeliveryHTTPTransport) RuntimeCheckpointDeliveryTransactionsEnabled() bool {
	return t != nil && t.transactional
}

func (t *RuntimeCheckpointDeliveryHTTPTransport) Deliver(ctx context.Context, envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
	if t == nil {
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("%w: transport 为空", ErrInvalidRuntimeCheckpointDelivery)
	}
	normalized, err := NormalizeRuntimeCheckpointDeliveryEnvelope(envelope)
	if err != nil {
		return RuntimeCheckpointDeliveryReceipt{}, err
	}
	if normalized.Source != t.Source || normalized.Destination != t.Destination {
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeCheckpointDeliveryAuth)
	}
	if err := projection.Validate(); err != nil {
		return RuntimeCheckpointDeliveryReceipt{}, err
	}
	if projection.InvocationID != normalized.InvocationID || projection.SnapshotRevision != normalized.SnapshotRevision || projection.EventSequence != normalized.EventSequence {
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("%w: projection metadata 不一致", ErrRuntimeCheckpointDeliveryAuth)
	}
	digest, err := RuntimeCheckpointProjectionDigest(projection)
	if err != nil || digest != normalized.SnapshotDigest {
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("%w: projection digest 不一致", ErrRuntimeCheckpointDeliveryAuth)
	}
	signed, err := SignRuntimeCheckpointDeliveryEnvelope(normalized, t.SharedSecret)
	if err != nil {
		return RuntimeCheckpointDeliveryReceipt{}, err
	}
	body, err := json.Marshal(RuntimeCheckpointDeliverySourceResponse{Envelope: signed, Projection: projection})
	if err != nil {
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("%w: request 编码失败", ErrInvalidRuntimeCheckpointDelivery)
	}
	if len(body) > MaxRuntimeCheckpointDeliveryResponseBytes {
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("%w: request 超过字节上限", ErrInvalidRuntimeCheckpointDelivery)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.Endpoint, bytes.NewReader(body))
	if err != nil {
		return RuntimeCheckpointDeliveryReceipt{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Abot-Checkpoint-Delivery-Version", strconv.Itoa(signed.Version))
	request.Header.Set("X-Abot-Checkpoint-Delivery-Signature", signed.Signature)
	request.Header.Set("Idempotency-Key", signed.DeliveryID)
	response, err := t.Client.Do(request)
	if err != nil {
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("Runtime checkpoint delivery 请求失败: %w", err)
	}
	defer response.Body.Close()
	maxBody := t.MaxBodyBytes
	if maxBody <= 0 || maxBody > MaxRuntimeCheckpointDeliveryResponseBytes {
		maxBody = MaxRuntimeCheckpointDeliveryResponseBytes
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil || int64(len(responseBody)) > maxBody {
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("%w: response 超过字节上限或读取失败", ErrInvalidRuntimeCheckpointDelivery)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(SanitizeRuntimeString(string(responseBody)))
		if len(message) > 512 {
			message = message[:512]
		}
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("Runtime checkpoint delivery 返回 HTTP %d: %s", response.StatusCode, message)
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	var receipt RuntimeCheckpointDeliveryReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("%w: receipt JSON 无效", ErrInvalidRuntimeCheckpointDelivery)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("%w: receipt 包含多个 JSON 文档", ErrInvalidRuntimeCheckpointDelivery)
	}
	if receipt.Version != signed.Version || receipt.DeliveryID != signed.DeliveryID || receipt.InvocationID != signed.InvocationID || receipt.SnapshotRevision != signed.SnapshotRevision || receipt.EventSequence != signed.EventSequence || receipt.SnapshotDigest != signed.SnapshotDigest {
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("%w: receipt metadata 不一致", ErrRuntimeCheckpointDeliveryAuth)
	}
	return receipt, nil
}

func writeRuntimeCheckpointDeliveryJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeRuntimeCheckpointDeliveryError(writer http.ResponseWriter, status int, err error) {
	message := "Runtime checkpoint delivery failed"
	if err != nil {
		message = strings.TrimSpace(SanitizeRuntimeString(err.Error()))
		if len(message) > 512 {
			message = message[:512]
		}
	}
	writeRuntimeCheckpointDeliveryJSON(writer, status, map[string]any{"error": message})
}
