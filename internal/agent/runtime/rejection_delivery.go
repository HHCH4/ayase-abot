package runtime

// This file contains the first cross-service approval-rejection boundary.
// Rejection is an intent, not a command: only bounded identifiers, a digest
// of the human reason and authenticated timestamps cross the Runtime
// boundary. The source persists the intent in an outbox and the destination
// applies it through an idempotent prepare/commit inbox. The protocol is
// deliberately not advertised as a distributed transaction; Workspace
// process control and a remote coordinator remain separate concerns.

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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	RuntimeApprovalRejectionDeliveryEnvelopeVersion       = 1
	MaxRuntimeApprovalRejectionDeliveryEnvelopeBytes      = 32 << 10
	MaxRuntimeApprovalRejectionDeliveryResponseBytes      = 32 << 10
	MaxRuntimeApprovalRejectionDeliveryIDLength           = 220
	MaxRuntimeApprovalRejectionDeliveryApprovalIDLength   = 220
	MaxRuntimeApprovalRejectionDeliveryInvocationIDLength = 512
	MaxRuntimeApprovalRejectionDeliveryReasonDigestLength = 64
	MaxRuntimeApprovalRejectionDeliveryReasonBytes        = 16 << 10
	MaxRuntimeApprovalRejectionDeliveryOwnerLength        = 160
	MaxRuntimeApprovalRejectionDeliveryAttempts           = 8
	MaxRuntimeApprovalRejectionDeliveryLease              = 10 * time.Minute
	DefaultRuntimeApprovalRejectionDeliveryMaxAge         = 10 * time.Minute
	DefaultRuntimeApprovalRejectionDeliveryFutureSkew     = 30 * time.Second
	DefaultRuntimeApprovalRejectionDeliveryTransactionTTL = 10 * time.Minute
)

var (
	ErrInvalidRuntimeApprovalRejectionDelivery = errors.New("Runtime approval rejection delivery 无效")
	ErrRuntimeApprovalRejectionDeliveryAuth    = errors.New("Runtime approval rejection delivery 认证失败")
	ErrRuntimeApprovalRejectionDeliveryStale   = errors.New("Runtime approval rejection delivery 已过期")
)

// RuntimeApprovalRejectionDeliveryEnvelope carries only the immutable
// rejection identity. DecisionAt is the durable decision time; IssuedAt is a
// fresh per-attempt timestamp so a queued intent can be retried without
// widening the receiver replay window. Neither the human reason nor the
// operation body is sent across the boundary.
type RuntimeApprovalRejectionDeliveryEnvelope struct {
	Version      int       `json:"version"`
	Source       string    `json:"source"`
	Destination  string    `json:"destination"`
	DeliveryID   string    `json:"delivery_id"`
	ApprovalID   string    `json:"approval_id"`
	InvocationID string    `json:"invocation_id"`
	ToolCallID   string    `json:"tool_call_id,omitempty"`
	OperationID  string    `json:"operation_id,omitempty"`
	Decision     string    `json:"decision"`
	ReasonDigest string    `json:"reason_digest"`
	DecisionAt   time.Time `json:"decision_at"`
	IssuedAt     time.Time `json:"issued_at"`
	Signature    string    `json:"signature,omitempty"`
}

func (e RuntimeApprovalRejectionDeliveryEnvelope) normalize() (RuntimeApprovalRejectionDeliveryEnvelope, error) {
	e.Source = strings.TrimSpace(e.Source)
	e.Destination = strings.TrimSpace(e.Destination)
	e.DeliveryID = strings.TrimSpace(e.DeliveryID)
	e.ApprovalID = strings.TrimSpace(e.ApprovalID)
	e.InvocationID = strings.TrimSpace(e.InvocationID)
	e.ToolCallID = strings.TrimSpace(e.ToolCallID)
	e.OperationID = strings.TrimSpace(e.OperationID)
	e.Decision = strings.TrimSpace(strings.ToLower(e.Decision))
	e.ReasonDigest = strings.TrimSpace(strings.ToLower(e.ReasonDigest))
	e.Signature = strings.TrimSpace(strings.ToLower(e.Signature))
	if e.Version != RuntimeApprovalRejectionDeliveryEnvelopeVersion {
		return RuntimeApprovalRejectionDeliveryEnvelope{}, fmt.Errorf("%w: version %d 不受支持", ErrInvalidRuntimeApprovalRejectionDelivery, e.Version)
	}
	if e.Source == "" || e.Destination == "" || e.DeliveryID == "" || e.ApprovalID == "" || e.InvocationID == "" || e.Decision != string(ApprovalRejected) || e.DecisionAt.IsZero() || e.IssuedAt.IsZero() {
		return RuntimeApprovalRejectionDeliveryEnvelope{}, fmt.Errorf("%w: 必填 metadata 不完整", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	if len(e.Source) > MaxRuntimeEventDeliverySourceLength || len(e.Destination) > MaxRuntimeEventDeliveryTargetLength || len(e.DeliveryID) > MaxRuntimeApprovalRejectionDeliveryIDLength || len(e.ApprovalID) > MaxRuntimeApprovalRejectionDeliveryApprovalIDLength || len(e.InvocationID) > MaxRuntimeApprovalRejectionDeliveryInvocationIDLength || len(e.ToolCallID) > MaxRuntimeApprovalRejectionDeliveryInvocationIDLength || len(e.OperationID) > MaxRuntimeApprovalRejectionDeliveryInvocationIDLength {
		return RuntimeApprovalRejectionDeliveryEnvelope{}, fmt.Errorf("%w: metadata 超出长度限制", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	if !isRuntimeEventDeliveryDigest(e.ReasonDigest) || len(e.ReasonDigest) != MaxRuntimeApprovalRejectionDeliveryReasonDigestLength {
		return RuntimeApprovalRejectionDeliveryEnvelope{}, fmt.Errorf("%w: reason_digest 无效", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	if e.Signature != "" && !isRuntimeEventDeliveryDigest(e.Signature) {
		return RuntimeApprovalRejectionDeliveryEnvelope{}, fmt.Errorf("%w: signature 无效", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	e.DecisionAt = e.DecisionAt.UTC()
	e.IssuedAt = e.IssuedAt.UTC()
	return e, nil
}

func (e RuntimeApprovalRejectionDeliveryEnvelope) Validate() error {
	_, err := e.normalize()
	return err
}

func (e RuntimeApprovalRejectionDeliveryEnvelope) Normalize() (RuntimeApprovalRejectionDeliveryEnvelope, error) {
	return e.normalize()
}

func NormalizeRuntimeApprovalRejectionDeliveryEnvelope(e RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeApprovalRejectionDeliveryEnvelope, error) {
	return e.normalize()
}

func RuntimeApprovalRejectionDeliveryReasonDigest(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if len([]byte(reason)) > MaxRuntimeApprovalRejectionDeliveryReasonBytes {
		return "", fmt.Errorf("%w: rejection reason 超过 %d 字节上限", ErrInvalidRuntimeApprovalRejectionDelivery, MaxRuntimeApprovalRejectionDeliveryReasonBytes)
	}
	if !utf8.ValidString(reason) {
		return "", fmt.Errorf("%w: rejection reason 不是有效 UTF-8", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	// The digest is of the scrubbed reason. This makes the metadata safe to
	// retain even when an older caller accidentally supplied a credential-shaped
	// fragment; the actual reason still never crosses the service boundary.
	reason = SanitizeRuntimeString(reason)
	sum := sha256.Sum256([]byte(reason))
	return hex.EncodeToString(sum[:]), nil
}

func runtimeApprovalRejectionDeliveryID(source, destination, approvalID, invocationID, toolCallID, operationID, reasonDigest string) string {
	material := strings.Join([]string{strings.TrimSpace(source), strings.TrimSpace(destination), strings.TrimSpace(approvalID), strings.TrimSpace(invocationID), strings.TrimSpace(toolCallID), strings.TrimSpace(operationID), strings.TrimSpace(reasonDigest), string(ApprovalRejected)}, "\x00")
	sum := sha256.Sum256([]byte(material))
	return "approval-rejection:" + hex.EncodeToString(sum[:])
}

func RuntimeApprovalRejectionDeliveryID(source, destination, approvalID, invocationID, toolCallID, operationID, reasonDigest string) string {
	return runtimeApprovalRejectionDeliveryID(source, destination, approvalID, invocationID, toolCallID, operationID, reasonDigest)
}

// RuntimeApprovalRejectionDeliveryOutbox is the source-side durable cursor.
// It contains no reason text and is safe to expose only as operational
// metadata. The immutable identity is deterministic so duplicate enqueue is
// a conflict rather than a second remote rejection.
type RuntimeApprovalRejectionDeliveryOutbox struct {
	ID             string                                       `json:"id"`
	GroupID        string                                       `json:"group_id,omitempty"`
	ApprovalID     string                                       `json:"approval_id"`
	InvocationID   string                                       `json:"invocation_id"`
	ToolCallID     string                                       `json:"tool_call_id,omitempty"`
	OperationID    string                                       `json:"operation_id,omitempty"`
	Source         string                                       `json:"source"`
	Destination    string                                       `json:"destination"`
	DeliveryID     string                                       `json:"delivery_id"`
	Decision       string                                       `json:"decision"`
	ReasonDigest   string                                       `json:"reason_digest"`
	DecisionAt     time.Time                                    `json:"decision_at"`
	Status         RuntimeApprovalRejectionDeliveryOutboxStatus `json:"status"`
	Attempt        int                                          `json:"attempt"`
	Revision       int64                                        `json:"revision"`
	AvailableAt    time.Time                                    `json:"available_at"`
	LeaseOwner     string                                       `json:"-"`
	LeaseExpiresAt *time.Time                                   `json:"-"`
	LastError      string                                       `json:"last_error,omitempty"`
	CreatedAt      time.Time                                    `json:"created_at"`
	UpdatedAt      time.Time                                    `json:"updated_at"`
}

type RuntimeApprovalRejectionDeliveryOutboxStatus string

const (
	RuntimeApprovalRejectionDeliveryOutboxQueued     RuntimeApprovalRejectionDeliveryOutboxStatus = "queued"
	RuntimeApprovalRejectionDeliveryOutboxProcessing RuntimeApprovalRejectionDeliveryOutboxStatus = "processing"
	RuntimeApprovalRejectionDeliveryOutboxCompleted  RuntimeApprovalRejectionDeliveryOutboxStatus = "completed"
	RuntimeApprovalRejectionDeliveryOutboxFailed     RuntimeApprovalRejectionDeliveryOutboxStatus = "failed"
)

func (s RuntimeApprovalRejectionDeliveryOutboxStatus) terminal() bool {
	return s == RuntimeApprovalRejectionDeliveryOutboxCompleted || s == RuntimeApprovalRejectionDeliveryOutboxFailed
}

func RuntimeApprovalRejectionDeliveryOutboxID(source, destination, deliveryID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(source) + "\x00" + strings.TrimSpace(destination) + "\x00" + strings.TrimSpace(deliveryID)))
	return "approval-rejection-outbox-" + hex.EncodeToString(sum[:16])
}

func cloneRuntimeApprovalRejectionDeliveryOutbox(item RuntimeApprovalRejectionDeliveryOutbox) RuntimeApprovalRejectionDeliveryOutbox {
	if item.LeaseExpiresAt != nil {
		value := item.LeaseExpiresAt.UTC()
		item.LeaseExpiresAt = &value
	}
	return item
}

func (o RuntimeApprovalRejectionDeliveryOutbox) normalize(now time.Time) (RuntimeApprovalRejectionDeliveryOutbox, error) {
	o.ID = strings.TrimSpace(o.ID)
	o.GroupID = strings.TrimSpace(o.GroupID)
	o.ApprovalID = strings.TrimSpace(o.ApprovalID)
	o.InvocationID = strings.TrimSpace(o.InvocationID)
	o.ToolCallID = strings.TrimSpace(o.ToolCallID)
	o.OperationID = strings.TrimSpace(o.OperationID)
	o.Source = strings.TrimSpace(o.Source)
	o.Destination = strings.TrimSpace(o.Destination)
	o.DeliveryID = strings.TrimSpace(o.DeliveryID)
	o.Decision = strings.TrimSpace(strings.ToLower(o.Decision))
	o.ReasonDigest = strings.TrimSpace(strings.ToLower(o.ReasonDigest))
	o.LeaseOwner = strings.TrimSpace(o.LeaseOwner)
	o.LastError = SanitizeRuntimeString(strings.TrimSpace(o.LastError))
	if o.ID == "" || o.ApprovalID == "" || o.InvocationID == "" || o.Source == "" || o.Destination == "" || o.DeliveryID == "" || o.DecisionAt.IsZero() || o.Decision != string(ApprovalRejected) {
		return RuntimeApprovalRejectionDeliveryOutbox{}, fmt.Errorf("%w: outbox metadata 不完整", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	if len(o.ID) > MaxRuntimeApprovalRejectionDeliveryIDLength || len(o.GroupID) > MaxRuntimeDeliveryGroupIDLength || len(o.ApprovalID) > MaxRuntimeApprovalRejectionDeliveryApprovalIDLength || len(o.InvocationID) > MaxRuntimeApprovalRejectionDeliveryInvocationIDLength || len(o.ToolCallID) > MaxRuntimeApprovalRejectionDeliveryInvocationIDLength || len(o.OperationID) > MaxRuntimeApprovalRejectionDeliveryInvocationIDLength || len(o.Source) > MaxRuntimeEventDeliverySourceLength || len(o.Destination) > MaxRuntimeEventDeliveryTargetLength || len(o.DeliveryID) > MaxRuntimeApprovalRejectionDeliveryIDLength {
		return RuntimeApprovalRejectionDeliveryOutbox{}, fmt.Errorf("%w: outbox metadata 超出长度限制", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	if o.ID != RuntimeApprovalRejectionDeliveryOutboxID(o.Source, o.Destination, o.DeliveryID) || o.DeliveryID != runtimeApprovalRejectionDeliveryID(o.Source, o.Destination, o.ApprovalID, o.InvocationID, o.ToolCallID, o.OperationID, o.ReasonDigest) {
		return RuntimeApprovalRejectionDeliveryOutbox{}, fmt.Errorf("%w: outbox identity 不一致", ErrRuntimeApprovalRejectionDeliveryAuth)
	}
	if !isRuntimeEventDeliveryDigest(o.ReasonDigest) {
		return RuntimeApprovalRejectionDeliveryOutbox{}, fmt.Errorf("%w: outbox reason_digest 无效", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	if len(o.LeaseOwner) > MaxRuntimeApprovalRejectionDeliveryOwnerLength || o.Attempt < 0 || o.Attempt > MaxRuntimeApprovalRejectionDeliveryAttempts {
		return RuntimeApprovalRejectionDeliveryOutbox{}, fmt.Errorf("%w: outbox lease/attempt 无效", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	if o.Status == "" {
		o.Status = RuntimeApprovalRejectionDeliveryOutboxQueued
	}
	switch o.Status {
	case RuntimeApprovalRejectionDeliveryOutboxQueued, RuntimeApprovalRejectionDeliveryOutboxProcessing, RuntimeApprovalRejectionDeliveryOutboxCompleted, RuntimeApprovalRejectionDeliveryOutboxFailed:
	default:
		return RuntimeApprovalRejectionDeliveryOutbox{}, fmt.Errorf("%w: outbox status %q 不受支持", ErrInvalidRuntimeApprovalRejectionDelivery, o.Status)
	}
	if o.Status == RuntimeApprovalRejectionDeliveryOutboxProcessing && (o.LeaseOwner == "" || o.LeaseExpiresAt == nil) {
		return RuntimeApprovalRejectionDeliveryOutbox{}, fmt.Errorf("%w: processing outbox 必须带租约", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	if len(o.LastError) > 4096 {
		o.LastError = o.LastError[:4096]
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	o.DecisionAt = o.DecisionAt.UTC()
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
	if o.Status.terminal() || o.Status == RuntimeApprovalRejectionDeliveryOutboxQueued {
		o.LeaseOwner = ""
		o.LeaseExpiresAt = nil
	}
	if o.Status == RuntimeApprovalRejectionDeliveryOutboxProcessing {
		if o.LeaseExpiresAt == nil || o.LeaseExpiresAt.IsZero() || o.LeaseExpiresAt.After(now.Add(MaxRuntimeApprovalRejectionDeliveryLease)) {
			return RuntimeApprovalRejectionDeliveryOutbox{}, fmt.Errorf("%w: outbox lease 无效", ErrInvalidRuntimeApprovalRejectionDelivery)
		}
		expires := o.LeaseExpiresAt.UTC()
		o.LeaseExpiresAt = &expires
	}
	return o, nil
}

func (o RuntimeApprovalRejectionDeliveryOutbox) Normalize(now time.Time) (RuntimeApprovalRejectionDeliveryOutbox, error) {
	return o.normalize(now)
}

func runtimeApprovalRejectionDeliveryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

func RuntimeApprovalRejectionDeliveryBackoff(attempt int) time.Duration {
	return runtimeApprovalRejectionDeliveryBackoff(attempt)
}

func NewRuntimeApprovalRejectionDeliveryOutbox(source, destination string, approval Approval, invocation Invocation, reason string, decisionAt time.Time) (RuntimeApprovalRejectionDeliveryOutbox, error) {
	if strings.TrimSpace(approval.ID) == "" || strings.TrimSpace(invocation.ID) == "" || strings.TrimSpace(approval.InvocationID) != strings.TrimSpace(invocation.ID) {
		return RuntimeApprovalRejectionDeliveryOutbox{}, fmt.Errorf("%w: approval/invocation metadata 不一致", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	if decisionAt.IsZero() {
		decisionAt = time.Now().UTC()
	} else {
		decisionAt = decisionAt.UTC()
	}
	digest, err := RuntimeApprovalRejectionDeliveryReasonDigest(reason)
	if err != nil {
		return RuntimeApprovalRejectionDeliveryOutbox{}, err
	}
	source = strings.TrimSpace(source)
	destination = strings.TrimSpace(destination)
	deliveryID := runtimeApprovalRejectionDeliveryID(source, destination, approval.ID, invocation.ID, approval.ToolCallID, approval.OperationID, digest)
	item := RuntimeApprovalRejectionDeliveryOutbox{
		ID: RuntimeApprovalRejectionDeliveryOutboxID(source, destination, deliveryID), ApprovalID: strings.TrimSpace(approval.ID), InvocationID: strings.TrimSpace(invocation.ID), ToolCallID: strings.TrimSpace(approval.ToolCallID), OperationID: strings.TrimSpace(approval.OperationID), Source: source, Destination: destination, DeliveryID: deliveryID, Decision: string(ApprovalRejected), ReasonDigest: digest, DecisionAt: decisionAt, Status: RuntimeApprovalRejectionDeliveryOutboxQueued, AvailableAt: decisionAt, CreatedAt: decisionAt, UpdatedAt: decisionAt,
	}
	return item.normalize(decisionAt)
}

func (o RuntimeApprovalRejectionDeliveryOutbox) Envelope(now time.Time) (RuntimeApprovalRejectionDeliveryEnvelope, error) {
	normalized, err := o.normalize(now)
	if err != nil {
		return RuntimeApprovalRejectionDeliveryEnvelope{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	return (RuntimeApprovalRejectionDeliveryEnvelope{Version: RuntimeApprovalRejectionDeliveryEnvelopeVersion, Source: normalized.Source, Destination: normalized.Destination, DeliveryID: normalized.DeliveryID, ApprovalID: normalized.ApprovalID, InvocationID: normalized.InvocationID, ToolCallID: normalized.ToolCallID, OperationID: normalized.OperationID, Decision: normalized.Decision, ReasonDigest: normalized.ReasonDigest, DecisionAt: normalized.DecisionAt, IssuedAt: now}).normalize()
}

func (o RuntimeApprovalRejectionDeliveryOutbox) Matches(other RuntimeApprovalRejectionDeliveryOutbox) bool {
	return o.ID == other.ID && o.ApprovalID == other.ApprovalID && o.InvocationID == other.InvocationID && o.ToolCallID == other.ToolCallID && o.OperationID == other.OperationID && o.Source == other.Source && o.Destination == other.Destination && o.DeliveryID == other.DeliveryID && o.Decision == other.Decision && o.ReasonDigest == other.ReasonDigest && o.DecisionAt.UTC().Equal(other.DecisionAt.UTC())
}

// RuntimeApprovalRejectionDeliveryOutboxRepository is the optional durable
// source cursor. Implementations must claim with a lease and keep retries
// idempotent; a source crash after remote commit is expected.
type RuntimeApprovalRejectionDeliveryOutboxRepository interface {
	EnqueueRuntimeApprovalRejectionDeliveryOutbox(context.Context, RuntimeApprovalRejectionDeliveryOutbox) (RuntimeApprovalRejectionDeliveryOutbox, error)
	GetRuntimeApprovalRejectionDeliveryOutbox(context.Context, string) (RuntimeApprovalRejectionDeliveryOutbox, error)
	ListRuntimeApprovalRejectionDeliveryOutbox(context.Context, string, RuntimeApprovalRejectionDeliveryOutboxStatus, int) ([]RuntimeApprovalRejectionDeliveryOutbox, error)
	ClaimRuntimeApprovalRejectionDeliveryOutbox(context.Context, string, time.Time, time.Duration) (RuntimeApprovalRejectionDeliveryOutbox, bool, error)
	CompleteRuntimeApprovalRejectionDeliveryOutbox(context.Context, string, string, time.Time) (bool, error)
	RetryRuntimeApprovalRejectionDeliveryOutbox(context.Context, string, string, time.Time, string) (bool, error)
}

// RuntimeApprovalRejectionDeliveryInboxRecord retains only the authenticated
// metadata at the destination.
type RuntimeApprovalRejectionDeliveryInboxRecord struct {
	Version      int       `json:"version"`
	DeliveryID   string    `json:"delivery_id"`
	ApprovalID   string    `json:"approval_id"`
	InvocationID string    `json:"invocation_id"`
	ToolCallID   string    `json:"tool_call_id,omitempty"`
	OperationID  string    `json:"operation_id,omitempty"`
	Source       string    `json:"source"`
	Destination  string    `json:"destination"`
	Decision     string    `json:"decision"`
	ReasonDigest string    `json:"reason_digest"`
	DecisionAt   time.Time `json:"decision_at"`
	Signature    string    `json:"signature"`
	ReceivedAt   time.Time `json:"received_at"`
}

func RuntimeApprovalRejectionDeliveryInboxRecordFromEnvelope(e RuntimeApprovalRejectionDeliveryEnvelope, receivedAt time.Time) RuntimeApprovalRejectionDeliveryInboxRecord {
	return RuntimeApprovalRejectionDeliveryInboxRecord{Version: e.Version, DeliveryID: e.DeliveryID, ApprovalID: e.ApprovalID, InvocationID: e.InvocationID, ToolCallID: e.ToolCallID, OperationID: e.OperationID, Source: e.Source, Destination: e.Destination, Decision: e.Decision, ReasonDigest: e.ReasonDigest, DecisionAt: e.DecisionAt.UTC(), Signature: e.Signature, ReceivedAt: receivedAt.UTC()}
}

func (r RuntimeApprovalRejectionDeliveryInboxRecord) Matches(e RuntimeApprovalRejectionDeliveryEnvelope) bool {
	return r.Version == e.Version && r.DeliveryID == e.DeliveryID && r.ApprovalID == e.ApprovalID && r.InvocationID == e.InvocationID && r.ToolCallID == e.ToolCallID && r.OperationID == e.OperationID && r.Source == e.Source && r.Destination == e.Destination && r.Decision == e.Decision && r.ReasonDigest == e.ReasonDigest && r.DecisionAt.UTC().Equal(e.DecisionAt.UTC())
}

type RuntimeApprovalRejectionDeliveryInbox interface {
	AcceptRuntimeApprovalRejectionDelivery(context.Context, RuntimeApprovalRejectionDeliveryEnvelope) (bool, error)
	GetRuntimeApprovalRejectionDelivery(context.Context, string) (RuntimeApprovalRejectionDeliveryInboxRecord, error)
}

// RuntimeApprovalRejectionDeliveryApplyRepository is an explicit destination
// capability for applying a rejected intent to the local Workspace ledger.
// Implementations must atomically apply the idempotent Operation/queued
// CommandRun transition with the inbox (or transaction commit). It is kept
// separate from RuntimeApprovalRejectionDeliveryInbox so a metadata-only
// receiver cannot accidentally claim to have cancelled a Workspace sidecar.
type RuntimeApprovalRejectionDeliveryApplyRepository interface {
	AcceptRuntimeApprovalRejectionDeliveryAndApply(context.Context, RuntimeApprovalRejectionDeliveryEnvelope) (bool, error)
	CommitRuntimeApprovalRejectionDeliveryAndApply(context.Context, RuntimeApprovalRejectionDeliveryEnvelope) (bool, error)
}

type RuntimeApprovalRejectionDeliveryTransactionStatus string

const (
	RuntimeApprovalRejectionDeliveryTransactionPrepared  RuntimeApprovalRejectionDeliveryTransactionStatus = "prepared"
	RuntimeApprovalRejectionDeliveryTransactionCommitted RuntimeApprovalRejectionDeliveryTransactionStatus = "committed"
	RuntimeApprovalRejectionDeliveryTransactionAborted   RuntimeApprovalRejectionDeliveryTransactionStatus = "aborted"
)

type RuntimeApprovalRejectionDeliveryTransaction struct {
	Version      int                                               `json:"version"`
	DeliveryID   string                                            `json:"delivery_id"`
	ApprovalID   string                                            `json:"approval_id"`
	InvocationID string                                            `json:"invocation_id"`
	ToolCallID   string                                            `json:"tool_call_id,omitempty"`
	OperationID  string                                            `json:"operation_id,omitempty"`
	Source       string                                            `json:"source"`
	Destination  string                                            `json:"destination"`
	Decision     string                                            `json:"decision"`
	ReasonDigest string                                            `json:"reason_digest"`
	DecisionAt   time.Time                                         `json:"decision_at"`
	IssuedAt     time.Time                                         `json:"issued_at"`
	Signature    string                                            `json:"-"`
	Status       RuntimeApprovalRejectionDeliveryTransactionStatus `json:"status"`
	ExpiresAt    time.Time                                         `json:"expires_at"`
	CreatedAt    time.Time                                         `json:"created_at"`
	UpdatedAt    time.Time                                         `json:"updated_at"`
}

func (t RuntimeApprovalRejectionDeliveryTransactionStatus) terminal() bool {
	return t == RuntimeApprovalRejectionDeliveryTransactionCommitted || t == RuntimeApprovalRejectionDeliveryTransactionAborted
}

func newRuntimeApprovalRejectionDeliveryTransaction(e RuntimeApprovalRejectionDeliveryEnvelope, now time.Time) (RuntimeApprovalRejectionDeliveryTransaction, error) {
	normalized, err := e.normalize()
	if err != nil {
		return RuntimeApprovalRejectionDeliveryTransaction{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	return RuntimeApprovalRejectionDeliveryTransaction{Version: normalized.Version, DeliveryID: normalized.DeliveryID, ApprovalID: normalized.ApprovalID, InvocationID: normalized.InvocationID, ToolCallID: normalized.ToolCallID, OperationID: normalized.OperationID, Source: normalized.Source, Destination: normalized.Destination, Decision: normalized.Decision, ReasonDigest: normalized.ReasonDigest, DecisionAt: normalized.DecisionAt, IssuedAt: normalized.IssuedAt, Signature: normalized.Signature, Status: RuntimeApprovalRejectionDeliveryTransactionPrepared, ExpiresAt: now.Add(DefaultRuntimeApprovalRejectionDeliveryTransactionTTL), CreatedAt: now, UpdatedAt: now}, nil
}

func NewRuntimeApprovalRejectionDeliveryTransaction(e RuntimeApprovalRejectionDeliveryEnvelope, now time.Time) (RuntimeApprovalRejectionDeliveryTransaction, error) {
	return newRuntimeApprovalRejectionDeliveryTransaction(e, now)
}

func (t RuntimeApprovalRejectionDeliveryTransaction) envelope() RuntimeApprovalRejectionDeliveryEnvelope {
	return RuntimeApprovalRejectionDeliveryEnvelope{Version: t.Version, DeliveryID: t.DeliveryID, ApprovalID: t.ApprovalID, InvocationID: t.InvocationID, ToolCallID: t.ToolCallID, OperationID: t.OperationID, Source: t.Source, Destination: t.Destination, Decision: t.Decision, ReasonDigest: t.ReasonDigest, DecisionAt: t.DecisionAt, IssuedAt: t.IssuedAt, Signature: t.Signature}
}

func (t RuntimeApprovalRejectionDeliveryTransaction) Envelope() RuntimeApprovalRejectionDeliveryEnvelope {
	return t.envelope()
}

func (t RuntimeApprovalRejectionDeliveryTransaction) Normalize(now time.Time) (RuntimeApprovalRejectionDeliveryTransaction, error) {
	e, err := t.envelope().normalize()
	if err != nil {
		return RuntimeApprovalRejectionDeliveryTransaction{}, err
	}
	if t.Status == "" {
		t.Status = RuntimeApprovalRejectionDeliveryTransactionPrepared
	}
	switch t.Status {
	case RuntimeApprovalRejectionDeliveryTransactionPrepared, RuntimeApprovalRejectionDeliveryTransactionCommitted, RuntimeApprovalRejectionDeliveryTransactionAborted:
	default:
		return RuntimeApprovalRejectionDeliveryTransaction{}, fmt.Errorf("%w: transaction status 无效", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if t.ExpiresAt.IsZero() {
		t.ExpiresAt = now.Add(DefaultRuntimeApprovalRejectionDeliveryTransactionTTL)
	} else {
		t.ExpiresAt = t.ExpiresAt.UTC()
	}
	if t.Status == RuntimeApprovalRejectionDeliveryTransactionPrepared && !t.ExpiresAt.After(now) {
		return RuntimeApprovalRejectionDeliveryTransaction{}, ErrRuntimeApprovalRejectionDeliveryStale
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	} else {
		t.CreatedAt = t.CreatedAt.UTC()
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = now
	} else {
		t.UpdatedAt = t.UpdatedAt.UTC()
	}
	t.Version, t.DeliveryID, t.ApprovalID, t.InvocationID, t.ToolCallID, t.OperationID, t.Source, t.Destination, t.Decision, t.ReasonDigest, t.DecisionAt, t.IssuedAt, t.Signature = e.Version, e.DeliveryID, e.ApprovalID, e.InvocationID, e.ToolCallID, e.OperationID, e.Source, e.Destination, e.Decision, e.ReasonDigest, e.DecisionAt, e.IssuedAt, e.Signature
	return t, nil
}

func (t RuntimeApprovalRejectionDeliveryTransaction) Matches(e RuntimeApprovalRejectionDeliveryEnvelope) bool {
	normalized, err := e.normalize()
	if err != nil {
		return false
	}
	return t.Version == normalized.Version && t.DeliveryID == normalized.DeliveryID && t.ApprovalID == normalized.ApprovalID && t.InvocationID == normalized.InvocationID && t.ToolCallID == normalized.ToolCallID && t.OperationID == normalized.OperationID && t.Source == normalized.Source && t.Destination == normalized.Destination && t.Decision == normalized.Decision && t.ReasonDigest == normalized.ReasonDigest && t.DecisionAt.UTC().Equal(normalized.DecisionAt.UTC())
}

type RuntimeApprovalRejectionDeliveryTransactionRepository interface {
	PrepareRuntimeApprovalRejectionDelivery(context.Context, RuntimeApprovalRejectionDeliveryEnvelope) (bool, error)
	CommitRuntimeApprovalRejectionDelivery(context.Context, RuntimeApprovalRejectionDeliveryEnvelope) (bool, error)
}

// RuntimeApprovalRejectionDeliveryAbortableTransactionRepository is the
// optional destination-side compensation boundary. Aborting a prepared
// rejection never applies a Workspace sidecar transition.
type RuntimeApprovalRejectionDeliveryAbortableTransactionRepository interface {
	AbortRuntimeApprovalRejectionDelivery(context.Context, RuntimeApprovalRejectionDeliveryEnvelope) (bool, error)
}

func (e RuntimeApprovalRejectionDeliveryEnvelope) canonicalUnsigned() ([]byte, error) {
	normalized, err := e.normalize()
	if err != nil {
		return nil, err
	}
	normalized.Signature = ""
	return json.Marshal(normalized)
}

func SignRuntimeApprovalRejectionDeliveryEnvelope(e RuntimeApprovalRejectionDeliveryEnvelope, secret []byte) (RuntimeApprovalRejectionDeliveryEnvelope, error) {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return RuntimeApprovalRejectionDeliveryEnvelope{}, err
	}
	canonical, err := e.canonicalUnsigned()
	if err != nil {
		return RuntimeApprovalRejectionDeliveryEnvelope{}, err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	e.Signature = hex.EncodeToString(mac.Sum(nil))
	return e.normalize()
}

func VerifyRuntimeApprovalRejectionDeliveryEnvelope(e RuntimeApprovalRejectionDeliveryEnvelope, secret []byte) error {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return err
	}
	normalized, err := e.normalize()
	if err != nil {
		return err
	}
	if normalized.Signature == "" {
		return fmt.Errorf("%w: 缺少 signature", ErrRuntimeApprovalRejectionDeliveryAuth)
	}
	canonical, err := normalized.canonicalUnsigned()
	if err != nil {
		return err
	}
	provided, err := hex.DecodeString(normalized.Signature)
	if err != nil {
		return fmt.Errorf("%w: signature 编码无效", ErrRuntimeApprovalRejectionDeliveryAuth)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return fmt.Errorf("%w: HMAC 不匹配", ErrRuntimeApprovalRejectionDeliveryAuth)
	}
	return nil
}

func validateRuntimeApprovalRejectionDeliveryTimestamp(e RuntimeApprovalRejectionDeliveryEnvelope, now time.Time, maxAge, futureSkew time.Duration) error {
	if maxAge <= 0 || futureSkew < 0 {
		return fmt.Errorf("%w: timestamp policy 无效", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	timestamp := e.IssuedAt.UTC()
	if timestamp.After(now.Add(futureSkew)) {
		return fmt.Errorf("%w: issued_at 位于允许未来窗口之外", ErrRuntimeApprovalRejectionDeliveryAuth)
	}
	if now.Sub(timestamp) > maxAge {
		return fmt.Errorf("%w: issued_at 已过期", ErrRuntimeApprovalRejectionDeliveryStale)
	}
	if e.DecisionAt.After(now.Add(futureSkew)) {
		return fmt.Errorf("%w: decision_at 位于允许未来窗口之外", ErrRuntimeApprovalRejectionDeliveryAuth)
	}
	return nil
}

type RuntimeApprovalRejectionDeliveryReceipt struct {
	Version      int    `json:"version"`
	DeliveryID   string `json:"delivery_id"`
	ApprovalID   string `json:"approval_id"`
	InvocationID string `json:"invocation_id"`
	ToolCallID   string `json:"tool_call_id,omitempty"`
	OperationID  string `json:"operation_id,omitempty"`
	Decision     string `json:"decision"`
	ReasonDigest string `json:"reason_digest"`
	Phase        string `json:"phase,omitempty"`
	Duplicate    bool   `json:"duplicate"`
}

const (
	RuntimeApprovalRejectionDeliveryTransactionPhasePrepared  = "prepared"
	RuntimeApprovalRejectionDeliveryTransactionPhaseCommitted = "committed"
	RuntimeApprovalRejectionDeliveryTransactionPhaseAborted   = "aborted"
)

func (r RuntimeApprovalRejectionDeliveryReceipt) ValidateAgainst(e RuntimeApprovalRejectionDeliveryEnvelope) error {
	normalized, err := e.normalize()
	if err != nil {
		return err
	}
	if r.Version != normalized.Version || strings.TrimSpace(r.DeliveryID) != normalized.DeliveryID || strings.TrimSpace(r.ApprovalID) != normalized.ApprovalID || strings.TrimSpace(r.InvocationID) != normalized.InvocationID || strings.TrimSpace(r.ToolCallID) != normalized.ToolCallID || strings.TrimSpace(r.OperationID) != normalized.OperationID || strings.TrimSpace(strings.ToLower(r.Decision)) != normalized.Decision || strings.TrimSpace(strings.ToLower(r.ReasonDigest)) != normalized.ReasonDigest {
		return fmt.Errorf("%w: receipt metadata 不一致", ErrRuntimeApprovalRejectionDeliveryAuth)
	}
	return nil
}

func (r RuntimeApprovalRejectionDeliveryReceipt) ValidateAgainstPhase(e RuntimeApprovalRejectionDeliveryEnvelope, phase string) error {
	if err := r.ValidateAgainst(e); err != nil {
		return err
	}
	if strings.TrimSpace(r.Phase) != strings.TrimSpace(phase) {
		return fmt.Errorf("%w: receipt phase 不一致", ErrRuntimeApprovalRejectionDeliveryAuth)
	}
	return nil
}

type RuntimeApprovalRejectionDeliveryTransport interface {
	Deliver(context.Context, RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeApprovalRejectionDeliveryReceipt, error)
}

// RuntimeApprovalRejectionDeliveryReconciler is an optional source transport
// capability for querying a destination rejection receipt without resending
// the intent.
type RuntimeApprovalRejectionDeliveryReconciler interface {
	ReconcileApprovalRejectionDelivery(context.Context, RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeDeliveryStatus, error)
}

type RuntimeApprovalRejectionDeliveryTransactionalTransport interface {
	Prepare(context.Context, RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeApprovalRejectionDeliveryReceipt, error)
	Commit(context.Context, RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeApprovalRejectionDeliveryReceipt, error)
}

// RuntimeApprovalRejectionDeliveryAbortableTransport is an optional
// compensation capability for transactional rejection transports.
type RuntimeApprovalRejectionDeliveryAbortableTransport interface {
	Abort(context.Context, RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeApprovalRejectionDeliveryReceipt, error)
}

type RuntimeApprovalRejectionDeliveryTransactionalOptIn interface {
	RuntimeApprovalRejectionDeliveryTransactionsEnabled() bool
}

// RuntimeApprovalRejectionDeliveryReceiver is an opt-in HTTP receiver. The
// one-phase endpoint and the prepare/commit endpoints share authentication but
// cannot be confused because transaction routes require an explicit phase
// header.
type RuntimeApprovalRejectionDeliveryReceiver struct {
	Source                     string
	Destination                string
	SharedSecret               []byte
	Inbox                      RuntimeApprovalRejectionDeliveryInbox
	MaxAge                     time.Duration
	MaxFutureSkew              time.Duration
	MaxBodyBytes               int64
	Now                        func() time.Time
	applyWorkspaceCancellation bool
}

func NewRuntimeApprovalRejectionDeliveryReceiver(source, destination string, secret []byte, inbox RuntimeApprovalRejectionDeliveryInbox) (*RuntimeApprovalRejectionDeliveryReceiver, error) {
	if strings.TrimSpace(source) == "" || len(strings.TrimSpace(source)) > MaxRuntimeEventDeliverySourceLength || strings.TrimSpace(destination) == "" || len(strings.TrimSpace(destination)) > MaxRuntimeEventDeliveryTargetLength {
		return nil, fmt.Errorf("%w: source/destination 无效", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return nil, err
	}
	if inbox == nil {
		return nil, fmt.Errorf("%w: inbox 不能为空", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	return &RuntimeApprovalRejectionDeliveryReceiver{Source: strings.TrimSpace(source), Destination: strings.TrimSpace(destination), SharedSecret: append([]byte(nil), secret...), Inbox: inbox, MaxAge: DefaultRuntimeApprovalRejectionDeliveryMaxAge, MaxFutureSkew: DefaultRuntimeApprovalRejectionDeliveryFutureSkew, MaxBodyBytes: MaxRuntimeApprovalRejectionDeliveryEnvelopeBytes, Now: time.Now}, nil
}

// SetWorkspaceCancellationEnabled explicitly opts the receiver into applying
// the signed rejection to a destination Workspace Operation. The opt-in is
// rejected at setup time when the supplied repository cannot provide the
// atomic inbox/sidecar boundary, preventing a false "accepted" receipt.
func (r *RuntimeApprovalRejectionDeliveryReceiver) SetWorkspaceCancellationEnabled(enabled bool) error {
	if r == nil {
		return fmt.Errorf("%w: receiver 为空", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	if enabled {
		if _, ok := r.Inbox.(RuntimeApprovalRejectionDeliveryApplyRepository); !ok {
			return fmt.Errorf("%w: receiver repository 未提供 Workspace 原子应用能力", ErrInvalidRuntimeApprovalRejectionDelivery)
		}
	}
	r.applyWorkspaceCancellation = enabled
	return nil
}

func (r *RuntimeApprovalRejectionDeliveryReceiver) Handler() http.Handler { return r }

func (r *RuntimeApprovalRejectionDeliveryReceiver) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if r == nil {
		writeRuntimeApprovalRejectionDeliveryError(writer, http.StatusInternalServerError, ErrInvalidRuntimeApprovalRejectionDelivery)
		return
	}
	if request != nil && request.URL != nil && strings.HasSuffix(strings.TrimRight(request.URL.Path, "/"), "/status") {
		r.ServeStatusHTTP(writer, request)
		return
	}
	if request.Method != http.MethodPost {
		writeRuntimeApprovalRejectionDeliveryError(writer, http.StatusMethodNotAllowed, fmt.Errorf("%w: 只支持 POST", ErrInvalidRuntimeApprovalRejectionDelivery))
		return
	}
	e, err := readRuntimeApprovalRejectionDeliveryEnvelope(request, r.MaxBodyBytes)
	if err != nil {
		writeRuntimeApprovalRejectionDeliveryError(writer, http.StatusBadRequest, err)
		return
	}
	if err := r.authenticate(e); err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, ErrInvalidRuntimeApprovalRejectionDelivery) {
			status = http.StatusBadRequest
		}
		writeRuntimeApprovalRejectionDeliveryError(writer, status, err)
		return
	}
	duplicate, err := r.accept(request.Context(), e)
	if err != nil {
		writeRuntimeApprovalRejectionDeliveryError(writer, http.StatusConflict, err)
		return
	}
	receipt := RuntimeApprovalRejectionDeliveryReceipt{Version: e.Version, DeliveryID: e.DeliveryID, ApprovalID: e.ApprovalID, InvocationID: e.InvocationID, ToolCallID: e.ToolCallID, OperationID: e.OperationID, Decision: e.Decision, ReasonDigest: e.ReasonDigest, Duplicate: duplicate}
	writeRuntimeApprovalRejectionDeliveryJSON(writer, http.StatusOK, receipt)
}

func (r *RuntimeApprovalRejectionDeliveryReceiver) accept(ctx context.Context, e RuntimeApprovalRejectionDeliveryEnvelope) (bool, error) {
	if r.applyWorkspaceCancellation {
		applier, ok := r.Inbox.(RuntimeApprovalRejectionDeliveryApplyRepository)
		if !ok {
			return false, fmt.Errorf("%w: receiver repository 未提供 Workspace 原子应用能力", ErrInvalidRuntimeApprovalRejectionDelivery)
		}
		return applier.AcceptRuntimeApprovalRejectionDeliveryAndApply(ctx, e)
	}
	return r.Inbox.AcceptRuntimeApprovalRejectionDelivery(ctx, e)
}

func (r *RuntimeApprovalRejectionDeliveryReceiver) authenticate(e RuntimeApprovalRejectionDeliveryEnvelope) error {
	if e.Source != r.Source || e.Destination != r.Destination {
		return fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeApprovalRejectionDeliveryAuth)
	}
	if err := VerifyRuntimeApprovalRejectionDeliveryEnvelope(e, r.SharedSecret); err != nil {
		return err
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	return validateRuntimeApprovalRejectionDeliveryTimestamp(e, now, r.MaxAge, r.MaxFutureSkew)
}

func (r *RuntimeApprovalRejectionDeliveryReceiver) PrepareHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		r.ServeTransactionHTTP(writer, request, RuntimeApprovalRejectionDeliveryTransactionPhasePrepared)
	})
}

func (r *RuntimeApprovalRejectionDeliveryReceiver) CommitHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		r.ServeTransactionHTTP(writer, request, RuntimeApprovalRejectionDeliveryTransactionPhaseCommitted)
	})
}

// AbortHandler exposes the authenticated compensation endpoint for a
// prepared rejection intent.
func (r *RuntimeApprovalRejectionDeliveryReceiver) AbortHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		r.ServeTransactionHTTP(writer, request, RuntimeApprovalRejectionDeliveryTransactionPhaseAborted)
	})
}

func (r *RuntimeApprovalRejectionDeliveryReceiver) ServeTransactionHTTP(writer http.ResponseWriter, request *http.Request, phase string) {
	if r == nil {
		writeRuntimeApprovalRejectionDeliveryError(writer, http.StatusInternalServerError, ErrInvalidRuntimeApprovalRejectionDelivery)
		return
	}
	if request.Method != http.MethodPost {
		writeRuntimeApprovalRejectionDeliveryError(writer, http.StatusMethodNotAllowed, fmt.Errorf("%w: 只支持 POST", ErrInvalidRuntimeApprovalRejectionDelivery))
		return
	}
	if phase != RuntimeApprovalRejectionDeliveryTransactionPhasePrepared && phase != RuntimeApprovalRejectionDeliveryTransactionPhaseCommitted && phase != RuntimeApprovalRejectionDeliveryTransactionPhaseAborted {
		writeRuntimeApprovalRejectionDeliveryError(writer, http.StatusBadRequest, ErrInvalidRuntimeApprovalRejectionDelivery)
		return
	}
	if strings.TrimSpace(request.Header.Get("X-Abot-Approval-Rejection-Transaction-Phase")) != phase {
		writeRuntimeApprovalRejectionDeliveryError(writer, http.StatusBadRequest, fmt.Errorf("%w: transaction phase header 与 route 不一致", ErrInvalidRuntimeApprovalRejectionDelivery))
		return
	}
	txnRepo, ok := r.Inbox.(RuntimeApprovalRejectionDeliveryTransactionRepository)
	if !ok {
		writeRuntimeApprovalRejectionDeliveryError(writer, http.StatusNotImplemented, fmt.Errorf("%w: receiver 未启用 transaction repository", ErrInvalidRuntimeApprovalRejectionDelivery))
		return
	}
	e, err := readRuntimeApprovalRejectionDeliveryEnvelopePayload(request, r.MaxBodyBytes)
	if err != nil {
		writeRuntimeApprovalRejectionDeliveryError(writer, http.StatusBadRequest, err)
		return
	}
	if err := r.authenticate(e); err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, ErrInvalidRuntimeApprovalRejectionDelivery) {
			status = http.StatusBadRequest
		}
		writeRuntimeApprovalRejectionDeliveryError(writer, status, err)
		return
	}
	var duplicate bool
	switch phase {
	case RuntimeApprovalRejectionDeliveryTransactionPhasePrepared:
		duplicate, err = txnRepo.PrepareRuntimeApprovalRejectionDelivery(request.Context(), e)
	case RuntimeApprovalRejectionDeliveryTransactionPhaseCommitted:
		if r.applyWorkspaceCancellation {
			applier, applyOK := r.Inbox.(RuntimeApprovalRejectionDeliveryApplyRepository)
			if !applyOK {
				writeRuntimeApprovalRejectionDeliveryError(writer, http.StatusNotImplemented, fmt.Errorf("%w: receiver repository 未提供 Workspace 原子应用能力", ErrInvalidRuntimeApprovalRejectionDelivery))
				return
			}
			duplicate, err = applier.CommitRuntimeApprovalRejectionDeliveryAndApply(request.Context(), e)
		} else {
			duplicate, err = txnRepo.CommitRuntimeApprovalRejectionDelivery(request.Context(), e)
		}
	case RuntimeApprovalRejectionDeliveryTransactionPhaseAborted:
		aborter, abortOK := r.Inbox.(RuntimeApprovalRejectionDeliveryAbortableTransactionRepository)
		if !abortOK {
			writeRuntimeApprovalRejectionDeliveryError(writer, http.StatusNotImplemented, fmt.Errorf("%w: receiver 未启用 transaction abort repository", ErrInvalidRuntimeApprovalRejectionDelivery))
			return
		}
		duplicate, err = aborter.AbortRuntimeApprovalRejectionDelivery(request.Context(), e)
	}
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, ErrNotFound) {
			status = http.StatusNotFound
		}
		writeRuntimeApprovalRejectionDeliveryError(writer, status, err)
		return
	}
	receipt := RuntimeApprovalRejectionDeliveryReceipt{Version: e.Version, DeliveryID: e.DeliveryID, ApprovalID: e.ApprovalID, InvocationID: e.InvocationID, ToolCallID: e.ToolCallID, OperationID: e.OperationID, Decision: e.Decision, ReasonDigest: e.ReasonDigest, Phase: phase, Duplicate: duplicate}
	writeRuntimeApprovalRejectionDeliveryJSON(writer, http.StatusOK, receipt)
}

func readRuntimeApprovalRejectionDeliveryEnvelope(request *http.Request, configuredMax int64) (RuntimeApprovalRejectionDeliveryEnvelope, error) {
	if request == nil || request.Body == nil {
		return RuntimeApprovalRejectionDeliveryEnvelope{}, fmt.Errorf("%w: 请求 body 为空", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	maxBody := configuredMax
	if maxBody <= 0 || maxBody > MaxRuntimeApprovalRejectionDeliveryEnvelopeBytes {
		maxBody = MaxRuntimeApprovalRejectionDeliveryEnvelopeBytes
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBody+1))
	if err != nil || int64(len(body)) > maxBody {
		return RuntimeApprovalRejectionDeliveryEnvelope{}, fmt.Errorf("%w: 请求 body 超过上限或读取失败", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var e RuntimeApprovalRejectionDeliveryEnvelope
	if err := decoder.Decode(&e); err != nil {
		return RuntimeApprovalRejectionDeliveryEnvelope{}, fmt.Errorf("%w: JSON 无效", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeApprovalRejectionDeliveryEnvelope{}, fmt.Errorf("%w: 请求包含多个 JSON 文档", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	e, err = e.normalize()
	if err != nil {
		return RuntimeApprovalRejectionDeliveryEnvelope{}, err
	}
	if err := validateRuntimeApprovalRejectionDeliveryHeaders(request, e); err != nil {
		return RuntimeApprovalRejectionDeliveryEnvelope{}, err
	}
	return e, nil
}

type runtimeApprovalRejectionDeliveryEnvelopePayload struct {
	Envelope RuntimeApprovalRejectionDeliveryEnvelope `json:"envelope"`
}

func readRuntimeApprovalRejectionDeliveryEnvelopePayload(request *http.Request, configuredMax int64) (RuntimeApprovalRejectionDeliveryEnvelope, error) {
	if request == nil || request.Body == nil {
		return RuntimeApprovalRejectionDeliveryEnvelope{}, fmt.Errorf("%w: 请求 body 为空", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	maxBody := configuredMax
	if maxBody <= 0 || maxBody > MaxRuntimeApprovalRejectionDeliveryResponseBytes {
		maxBody = MaxRuntimeApprovalRejectionDeliveryResponseBytes
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBody+1))
	if err != nil || int64(len(body)) > maxBody {
		return RuntimeApprovalRejectionDeliveryEnvelope{}, fmt.Errorf("%w: transaction request 超过上限或读取失败", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var payload runtimeApprovalRejectionDeliveryEnvelopePayload
	if err := decoder.Decode(&payload); err != nil {
		return RuntimeApprovalRejectionDeliveryEnvelope{}, fmt.Errorf("%w: transaction JSON 无效", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeApprovalRejectionDeliveryEnvelope{}, fmt.Errorf("%w: transaction 包含多个 JSON 文档", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	e, err := payload.Envelope.normalize()
	if err != nil {
		return RuntimeApprovalRejectionDeliveryEnvelope{}, err
	}
	if err := validateRuntimeApprovalRejectionDeliveryHeaders(request, e); err != nil {
		return RuntimeApprovalRejectionDeliveryEnvelope{}, err
	}
	return e, nil
}

func validateRuntimeApprovalRejectionDeliveryHeaders(request *http.Request, e RuntimeApprovalRejectionDeliveryEnvelope) error {
	if request == nil {
		return fmt.Errorf("%w: request 为空", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	if header := strings.TrimSpace(request.Header.Get("X-Abot-Approval-Rejection-Delivery-Version")); header != "" && header != strconv.Itoa(e.Version) {
		return fmt.Errorf("%w: header version 与 body 不一致", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	if header := strings.TrimSpace(strings.ToLower(request.Header.Get("X-Abot-Approval-Rejection-Delivery-Signature"))); header != "" && header != e.Signature {
		return fmt.Errorf("%w: header signature 与 body 不一致", ErrRuntimeApprovalRejectionDeliveryAuth)
	}
	if header := strings.TrimSpace(request.Header.Get("Idempotency-Key")); header != "" && header != e.DeliveryID {
		return fmt.Errorf("%w: idempotency key 与 delivery_id 不一致", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	return nil
}

func writeRuntimeApprovalRejectionDeliveryJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeRuntimeApprovalRejectionDeliveryError(writer http.ResponseWriter, status int, err error) {
	message := "Runtime approval rejection delivery failed"
	if err != nil {
		message = strings.TrimSpace(SanitizeRuntimeString(err.Error()))
		if len(message) > 512 {
			message = message[:512]
		}
	}
	writeRuntimeApprovalRejectionDeliveryJSON(writer, status, map[string]any{"error": message})
}

type RuntimeApprovalRejectionDeliveryHTTPTransport struct {
	Endpoint      string
	Source        string
	Destination   string
	SharedSecret  []byte
	Client        *http.Client
	MaxBodyBytes  int64
	Now           func() time.Time
	transactional bool
}

func NewRuntimeApprovalRejectionDeliveryHTTPTransport(endpoint, source, destination string, secret []byte, client *http.Client) (*RuntimeApprovalRejectionDeliveryHTTPTransport, error) {
	endpoint = strings.TrimSpace(endpoint)
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("%w: endpoint 必须是 http/https URL", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	if strings.TrimSpace(source) == "" || len(strings.TrimSpace(source)) > MaxRuntimeEventDeliverySourceLength || strings.TrimSpace(destination) == "" || len(strings.TrimSpace(destination)) > MaxRuntimeEventDeliveryTargetLength {
		return nil, fmt.Errorf("%w: source/destination 无效", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &RuntimeApprovalRejectionDeliveryHTTPTransport{Endpoint: endpoint, Source: strings.TrimSpace(source), Destination: strings.TrimSpace(destination), SharedSecret: append([]byte(nil), secret...), Client: client, MaxBodyBytes: MaxRuntimeApprovalRejectionDeliveryResponseBytes, Now: time.Now}, nil
}

func NewRuntimeApprovalRejectionDeliveryHTTPTransactionalTransport(endpoint, source, destination string, secret []byte, client *http.Client) (*RuntimeApprovalRejectionDeliveryHTTPTransport, error) {
	transport, err := NewRuntimeApprovalRejectionDeliveryHTTPTransport(endpoint, source, destination, secret, client)
	if err != nil {
		return nil, err
	}
	transport.transactional = true
	return transport, nil
}

func (t *RuntimeApprovalRejectionDeliveryHTTPTransport) RuntimeApprovalRejectionDeliveryTransactionsEnabled() bool {
	return t != nil && t.transactional
}

func (t *RuntimeApprovalRejectionDeliveryHTTPTransport) Deliver(ctx context.Context, e RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeApprovalRejectionDeliveryReceipt, error) {
	return t.deliverPhase(ctx, e, "")
}

func (t *RuntimeApprovalRejectionDeliveryHTTPTransport) Prepare(ctx context.Context, e RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeApprovalRejectionDeliveryReceipt, error) {
	return t.deliverPhase(ctx, e, RuntimeApprovalRejectionDeliveryTransactionPhasePrepared)
}

func (t *RuntimeApprovalRejectionDeliveryHTTPTransport) Commit(ctx context.Context, e RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeApprovalRejectionDeliveryReceipt, error) {
	return t.deliverPhase(ctx, e, RuntimeApprovalRejectionDeliveryTransactionPhaseCommitted)
}

func (t *RuntimeApprovalRejectionDeliveryHTTPTransport) Abort(ctx context.Context, e RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeApprovalRejectionDeliveryReceipt, error) {
	return t.deliverPhase(ctx, e, RuntimeApprovalRejectionDeliveryTransactionPhaseAborted)
}

func (t *RuntimeApprovalRejectionDeliveryHTTPTransport) deliverPhase(ctx context.Context, e RuntimeApprovalRejectionDeliveryEnvelope, phase string) (RuntimeApprovalRejectionDeliveryReceipt, error) {
	if t == nil {
		return RuntimeApprovalRejectionDeliveryReceipt{}, fmt.Errorf("%w: transport 为空", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	normalized, err := e.normalize()
	if err != nil {
		return RuntimeApprovalRejectionDeliveryReceipt{}, err
	}
	if normalized.Source != t.Source || normalized.Destination != t.Destination {
		return RuntimeApprovalRejectionDeliveryReceipt{}, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeApprovalRejectionDeliveryAuth)
	}
	if normalized.IssuedAt.IsZero() {
		normalized.IssuedAt = time.Now().UTC()
		if t.Now != nil {
			normalized.IssuedAt = t.Now().UTC()
		}
	}
	signed, err := SignRuntimeApprovalRejectionDeliveryEnvelope(normalized, t.SharedSecret)
	if err != nil {
		return RuntimeApprovalRejectionDeliveryReceipt{}, err
	}
	var body []byte
	if phase == "" {
		body, err = json.Marshal(signed)
	} else {
		body, err = json.Marshal(runtimeApprovalRejectionDeliveryEnvelopePayload{Envelope: signed})
	}
	if err != nil || len(body) > MaxRuntimeApprovalRejectionDeliveryEnvelopeBytes {
		return RuntimeApprovalRejectionDeliveryReceipt{}, fmt.Errorf("%w: request 编码或大小无效", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	endpoint := t.Endpoint
	if phase != "" {
		u, parseErr := url.Parse(endpoint)
		if parseErr != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return RuntimeApprovalRejectionDeliveryReceipt{}, fmt.Errorf("%w: endpoint 无效", ErrInvalidRuntimeApprovalRejectionDelivery)
		}
		pathSegment := ""
		switch phase {
		case RuntimeApprovalRejectionDeliveryTransactionPhasePrepared:
			pathSegment = "prepare"
		case RuntimeApprovalRejectionDeliveryTransactionPhaseCommitted:
			pathSegment = "commit"
		case RuntimeApprovalRejectionDeliveryTransactionPhaseAborted:
			pathSegment = "abort"
		default:
			return RuntimeApprovalRejectionDeliveryReceipt{}, fmt.Errorf("%w: transaction phase 无效", ErrInvalidRuntimeApprovalRejectionDelivery)
		}
		u.Path = strings.TrimRight(u.Path, "/") + "/" + pathSegment
		endpoint = u.String()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return RuntimeApprovalRejectionDeliveryReceipt{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Abot-Approval-Rejection-Delivery-Version", strconv.Itoa(signed.Version))
	request.Header.Set("X-Abot-Approval-Rejection-Delivery-Signature", signed.Signature)
	request.Header.Set("Idempotency-Key", signed.DeliveryID)
	if phase != "" {
		request.Header.Set("X-Abot-Approval-Rejection-Transaction-Phase", phase)
	}
	response, err := t.Client.Do(request)
	if err != nil {
		return RuntimeApprovalRejectionDeliveryReceipt{}, fmt.Errorf("Runtime approval rejection delivery 请求失败: %w", err)
	}
	defer response.Body.Close()
	maxBody := t.MaxBodyBytes
	if maxBody <= 0 || maxBody > MaxRuntimeApprovalRejectionDeliveryResponseBytes {
		maxBody = MaxRuntimeApprovalRejectionDeliveryResponseBytes
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil || int64(len(responseBody)) > maxBody {
		return RuntimeApprovalRejectionDeliveryReceipt{}, fmt.Errorf("%w: response 超过上限或读取失败", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(SanitizeRuntimeString(string(responseBody)))
		if len(message) > 512 {
			message = message[:512]
		}
		return RuntimeApprovalRejectionDeliveryReceipt{}, fmt.Errorf("Runtime approval rejection delivery 返回 HTTP %d: %s", response.StatusCode, message)
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	var receipt RuntimeApprovalRejectionDeliveryReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return RuntimeApprovalRejectionDeliveryReceipt{}, fmt.Errorf("%w: receipt JSON 无效", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeApprovalRejectionDeliveryReceipt{}, fmt.Errorf("%w: receipt 包含多个 JSON 文档", ErrInvalidRuntimeApprovalRejectionDelivery)
	}
	if phase == "" {
		err = receipt.ValidateAgainst(signed)
	} else {
		err = receipt.ValidateAgainstPhase(signed, phase)
	}
	if err != nil {
		return RuntimeApprovalRejectionDeliveryReceipt{}, err
	}
	return receipt, nil
}

var _ RuntimeApprovalRejectionDeliveryAbortableTransport = (*RuntimeApprovalRejectionDeliveryHTTPTransport)(nil)
