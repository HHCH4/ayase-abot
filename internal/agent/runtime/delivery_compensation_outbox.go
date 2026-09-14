package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RuntimeDeliveryCompensation is a source-owned durable work item for
// releasing a destination transaction that was prepared but could not be
// completed. It contains only immutable delivery identity; payloads remain in
// the family-specific source outbox.
type RuntimeDeliveryCompensation struct {
	ID             string                            `json:"id"`
	GroupID        string                            `json:"group_id,omitempty"`
	Kind           RuntimeDeliveryKind               `json:"kind"`
	Source         string                            `json:"source"`
	Destination    string                            `json:"destination"`
	DeliveryID     string                            `json:"delivery_id"`
	InvocationID   string                            `json:"invocation_id"`
	OutboxID       string                            `json:"outbox_id"`
	SourceAttempt  int                               `json:"source_attempt"`
	Status         RuntimeDeliveryCompensationStatus `json:"status"`
	Attempt        int                               `json:"attempt"`
	Revision       int64                             `json:"revision"`
	AvailableAt    time.Time                         `json:"available_at"`
	LeaseOwner     string                            `json:"-"`
	LeaseExpiresAt *time.Time                        `json:"-"`
	LastError      string                            `json:"last_error,omitempty"`
	CreatedAt      time.Time                         `json:"created_at"`
	UpdatedAt      time.Time                         `json:"updated_at"`
	CompletedAt    *time.Time                        `json:"completed_at,omitempty"`
}

type RuntimeDeliveryCompensationStatus string

const (
	RuntimeDeliveryCompensationQueued     RuntimeDeliveryCompensationStatus = "queued"
	RuntimeDeliveryCompensationProcessing RuntimeDeliveryCompensationStatus = "processing"
	RuntimeDeliveryCompensationCompleted  RuntimeDeliveryCompensationStatus = "completed"
	RuntimeDeliveryCompensationFailed     RuntimeDeliveryCompensationStatus = "failed"
)

const (
	MaxRuntimeDeliveryCompensationIDLength          = 240
	MaxRuntimeDeliveryCompensationGroupIDLength     = MaxRuntimeDeliveryGroupIDLength
	MaxRuntimeDeliveryCompensationKindLength        = MaxRuntimeDeliveryAttemptKindLength
	MaxRuntimeDeliveryCompensationSourceLength      = MaxRuntimeDeliveryAttemptSourceLength
	MaxRuntimeDeliveryCompensationDestinationLength = MaxRuntimeDeliveryAttemptDestinationLength
	MaxRuntimeDeliveryCompensationDeliveryIDLength  = MaxRuntimeDeliveryAttemptDeliveryIDLength
	MaxRuntimeDeliveryCompensationInvocationLength  = MaxRuntimeDeliveryAttemptInvocationLength
	MaxRuntimeDeliveryCompensationOutboxIDLength    = MaxRuntimeDeliveryAttemptOutboxIDLength
	MaxRuntimeDeliveryCompensationOwnerLength       = MaxRuntimeDeliveryAttemptOwnerLength
	MaxRuntimeDeliveryCompensationErrorLength       = MaxRuntimeDeliveryAttemptErrorLength
	MaxRuntimeDeliveryCompensationAttempts          = MaxRuntimeDeliveryAttemptAttempts
	MaxRuntimeDeliveryCompensationLease             = MaxRuntimeDeliveryAttemptLease
)

var (
	ErrInvalidRuntimeDeliveryCompensation     = errors.New("Runtime delivery compensation 无效")
	ErrRuntimeDeliveryCompensationUnavailable = errors.New("Runtime delivery compensation 未配置")
)

func validRuntimeDeliveryCompensationStatus(status RuntimeDeliveryCompensationStatus) bool {
	switch status {
	case RuntimeDeliveryCompensationQueued, RuntimeDeliveryCompensationProcessing,
		RuntimeDeliveryCompensationCompleted, RuntimeDeliveryCompensationFailed:
		return true
	default:
		return false
	}
}

func (s RuntimeDeliveryCompensationStatus) terminal() bool {
	return s == RuntimeDeliveryCompensationCompleted || s == RuntimeDeliveryCompensationFailed
}

// RuntimeDeliveryCompensationID is stable for one immutable delivery
// identity. A source retry never creates another compensation transaction.
func RuntimeDeliveryCompensationID(kind RuntimeDeliveryKind, source, destination, deliveryID string) string {
	material := strings.Join([]string{strings.TrimSpace(string(kind)), strings.TrimSpace(source), strings.TrimSpace(destination), strings.TrimSpace(deliveryID)}, "\x00")
	sum := sha256.Sum256([]byte(material))
	return "runtime-delivery-compensation-" + hex.EncodeToString(sum[:16])
}

func cloneRuntimeDeliveryCompensation(item RuntimeDeliveryCompensation) RuntimeDeliveryCompensation {
	if item.LeaseExpiresAt != nil {
		value := item.LeaseExpiresAt.UTC()
		item.LeaseExpiresAt = &value
	}
	if item.CompletedAt != nil {
		value := item.CompletedAt.UTC()
		item.CompletedAt = &value
	}
	return item
}

func normalizeRuntimeDeliveryCompensationTime(value, fallback time.Time) time.Time {
	if value.IsZero() {
		value = fallback
	}
	if value.IsZero() {
		value = time.Now().UTC()
	}
	return value.UTC()
}

func (c RuntimeDeliveryCompensation) Normalize(now time.Time) (RuntimeDeliveryCompensation, error) {
	c.ID = strings.TrimSpace(c.ID)
	c.GroupID = strings.TrimSpace(c.GroupID)
	c.Kind = RuntimeDeliveryKind(strings.TrimSpace(string(c.Kind)))
	c.Source = strings.TrimSpace(c.Source)
	c.Destination = strings.TrimSpace(c.Destination)
	c.DeliveryID = strings.TrimSpace(c.DeliveryID)
	c.InvocationID = strings.TrimSpace(c.InvocationID)
	c.OutboxID = strings.TrimSpace(c.OutboxID)
	c.LeaseOwner = strings.TrimSpace(c.LeaseOwner)
	c.LastError = SanitizeRuntimeString(strings.TrimSpace(c.LastError))
	if c.ID == "" || c.Kind == "" || c.Source == "" || c.Destination == "" || c.DeliveryID == "" || c.InvocationID == "" || c.OutboxID == "" || c.SourceAttempt <= 0 {
		return RuntimeDeliveryCompensation{}, fmt.Errorf("%w: metadata 不完整", ErrInvalidRuntimeDeliveryCompensation)
	}
	if !validRuntimeDeliveryKind(c.Kind) {
		return RuntimeDeliveryCompensation{}, fmt.Errorf("%w: kind %q 不受支持", ErrInvalidRuntimeDeliveryCompensation, c.Kind)
	}
	if c.ID != RuntimeDeliveryCompensationID(c.Kind, c.Source, c.Destination, c.DeliveryID) {
		return RuntimeDeliveryCompensation{}, fmt.Errorf("%w: ID 不是稳定派生值", ErrInvalidRuntimeDeliveryCompensation)
	}
	if len(c.ID) > MaxRuntimeDeliveryCompensationIDLength || len(c.GroupID) > MaxRuntimeDeliveryCompensationGroupIDLength || len(c.Kind) > MaxRuntimeDeliveryCompensationKindLength || len(c.Source) > MaxRuntimeDeliveryCompensationSourceLength || len(c.Destination) > MaxRuntimeDeliveryCompensationDestinationLength || len(c.DeliveryID) > MaxRuntimeDeliveryCompensationDeliveryIDLength || len(c.InvocationID) > MaxRuntimeDeliveryCompensationInvocationLength || len(c.OutboxID) > MaxRuntimeDeliveryCompensationOutboxIDLength || len(c.LeaseOwner) > MaxRuntimeDeliveryCompensationOwnerLength {
		return RuntimeDeliveryCompensation{}, fmt.Errorf("%w: metadata 超出长度限制", ErrInvalidRuntimeDeliveryCompensation)
	}
	if c.SourceAttempt != MaxRuntimeDeliveryCompensationAttempts || c.Attempt < 0 || c.Attempt > MaxRuntimeDeliveryCompensationAttempts || c.Revision < 0 {
		return RuntimeDeliveryCompensation{}, fmt.Errorf("%w: attempt/revision 无效", ErrInvalidRuntimeDeliveryCompensation)
	}
	if c.Status == "" {
		c.Status = RuntimeDeliveryCompensationQueued
	}
	if !validRuntimeDeliveryCompensationStatus(c.Status) {
		return RuntimeDeliveryCompensation{}, fmt.Errorf("%w: status %q 不受支持", ErrInvalidRuntimeDeliveryCompensation, c.Status)
	}
	if len(c.LastError) > MaxRuntimeDeliveryCompensationErrorLength {
		c.LastError = c.LastError[:MaxRuntimeDeliveryCompensationErrorLength]
	}
	now = normalizeRuntimeDeliveryCompensationTime(now, time.Time{})
	c.AvailableAt = normalizeRuntimeDeliveryCompensationTime(c.AvailableAt, now)
	c.CreatedAt = normalizeRuntimeDeliveryCompensationTime(c.CreatedAt, now)
	c.UpdatedAt = normalizeRuntimeDeliveryCompensationTime(c.UpdatedAt, now)
	if c.Revision <= 0 {
		c.Revision = 1
	}
	if c.LeaseExpiresAt != nil {
		expires := c.LeaseExpiresAt.UTC()
		if expires.IsZero() || expires.After(now.Add(MaxRuntimeDeliveryCompensationLease)) {
			return RuntimeDeliveryCompensation{}, fmt.Errorf("%w: lease 无效", ErrInvalidRuntimeDeliveryCompensation)
		}
		c.LeaseExpiresAt = &expires
	}
	if c.Status.terminal() || c.Status == RuntimeDeliveryCompensationQueued {
		c.LeaseOwner = ""
		c.LeaseExpiresAt = nil
	}
	if c.Status == RuntimeDeliveryCompensationProcessing {
		if c.LeaseOwner == "" || c.LeaseExpiresAt == nil || c.LeaseExpiresAt.IsZero() {
			return RuntimeDeliveryCompensation{}, fmt.Errorf("%w: processing compensation 必须带租约", ErrInvalidRuntimeDeliveryCompensation)
		}
	}
	if c.Status == RuntimeDeliveryCompensationCompleted {
		if c.CompletedAt == nil {
			finished := c.UpdatedAt
			c.CompletedAt = &finished
		} else {
			finished := c.CompletedAt.UTC()
			c.CompletedAt = &finished
		}
	} else if c.CompletedAt != nil {
		return RuntimeDeliveryCompensation{}, fmt.Errorf("%w: 非 completed compensation 不能带 completed_at", ErrInvalidRuntimeDeliveryCompensation)
	}
	return c, nil
}

func NewRuntimeDeliveryCompensation(kind RuntimeDeliveryKind, source, destination, deliveryID, invocationID, outboxID, groupID string, sourceAttempt int, now time.Time) (RuntimeDeliveryCompensation, error) {
	now = normalizeRuntimeDeliveryCompensationTime(now, time.Time{})
	return (RuntimeDeliveryCompensation{
		ID: RuntimeDeliveryCompensationID(kind, source, destination, deliveryID), GroupID: strings.TrimSpace(groupID),
		Kind: kind, Source: strings.TrimSpace(source), Destination: strings.TrimSpace(destination), DeliveryID: strings.TrimSpace(deliveryID),
		InvocationID: strings.TrimSpace(invocationID), OutboxID: strings.TrimSpace(outboxID), SourceAttempt: sourceAttempt,
		Status: RuntimeDeliveryCompensationQueued, AvailableAt: now, CreatedAt: now, UpdatedAt: now, Revision: 1,
	}).Normalize(now)
}

func (c RuntimeDeliveryCompensation) MatchesIdentity(other RuntimeDeliveryCompensation) bool {
	left, leftErr := c.Normalize(time.Time{})
	right, rightErr := other.Normalize(time.Time{})
	if leftErr != nil || rightErr != nil {
		return false
	}
	return left.ID == right.ID && left.GroupID == right.GroupID && left.Kind == right.Kind && left.Source == right.Source && left.Destination == right.Destination && left.DeliveryID == right.DeliveryID && left.InvocationID == right.InvocationID && left.OutboxID == right.OutboxID && left.SourceAttempt == right.SourceAttempt
}

func RuntimeDeliveryCompensationBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

// DeferRuntimeDeliveryCompensation releases a claim without consuming the
// retry budget. It is used while the source outbox is still transitioning to
// its terminal failed state.
func DeferRuntimeDeliveryCompensation(item RuntimeDeliveryCompensation, owner string, now, availableAt time.Time, message string) (RuntimeDeliveryCompensation, bool, error) {
	now = normalizeRuntimeDeliveryCompensationTime(now, time.Time{})
	current, err := item.Normalize(now)
	if err != nil {
		return RuntimeDeliveryCompensation{}, false, err
	}
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeDeliveryCompensationOwnerLength || current.Status != RuntimeDeliveryCompensationProcessing || current.LeaseOwner != owner || current.LeaseExpiresAt == nil || !current.LeaseExpiresAt.After(now) {
		return RuntimeDeliveryCompensation{}, false, ErrConflict
	}
	availableAt = normalizeRuntimeDeliveryCompensationTime(availableAt, now)
	if availableAt.Before(now) {
		availableAt = now
	}
	message = SanitizeRuntimeString(strings.TrimSpace(message))
	if len(message) > MaxRuntimeDeliveryCompensationErrorLength {
		message = message[:MaxRuntimeDeliveryCompensationErrorLength]
	}
	current.Status = RuntimeDeliveryCompensationQueued
	current.LeaseOwner = ""
	current.LeaseExpiresAt = nil
	// Claim increments Attempt before the source-state gate runs. Deferral is
	// not a delivery attempt, so release that bookkeeping increment just like
	// the other source-side outboxes do.
	if current.Attempt > 0 {
		current.Attempt--
	}
	current.AvailableAt = availableAt
	current.LastError = message
	current.UpdatedAt = now
	current.Revision++
	normalized, err := current.Normalize(now)
	if err != nil {
		return RuntimeDeliveryCompensation{}, false, err
	}
	return normalized, true, nil
}

func completeRuntimeDeliveryCompensation(item RuntimeDeliveryCompensation, owner string, now time.Time) (RuntimeDeliveryCompensation, bool, error) {
	now = normalizeRuntimeDeliveryCompensationTime(now, time.Time{})
	current, err := item.Normalize(now)
	if err != nil {
		return RuntimeDeliveryCompensation{}, false, err
	}
	owner = strings.TrimSpace(owner)
	if current.Status == RuntimeDeliveryCompensationCompleted {
		return current, true, nil
	}
	if owner == "" || len(owner) > MaxRuntimeDeliveryCompensationOwnerLength || current.Status != RuntimeDeliveryCompensationProcessing || current.LeaseOwner != owner || current.LeaseExpiresAt == nil || !current.LeaseExpiresAt.After(now) {
		return RuntimeDeliveryCompensation{}, false, ErrConflict
	}
	finished := now
	current.Status = RuntimeDeliveryCompensationCompleted
	current.LeaseOwner = ""
	current.LeaseExpiresAt = nil
	current.CompletedAt = &finished
	current.UpdatedAt = now
	current.Revision++
	normalized, err := current.Normalize(now)
	if err != nil {
		return RuntimeDeliveryCompensation{}, false, err
	}
	return normalized, true, nil
}

// CompleteRuntimeDeliveryCompensation applies the terminal success
// transition under a caller-owned lease. Storage adapters use this helper to
// keep Memory and SQLite state rules identical.
func CompleteRuntimeDeliveryCompensation(item RuntimeDeliveryCompensation, owner string, now time.Time) (RuntimeDeliveryCompensation, bool, error) {
	return completeRuntimeDeliveryCompensation(item, owner, now)
}

func retryRuntimeDeliveryCompensation(item RuntimeDeliveryCompensation, owner string, now time.Time, message string) (RuntimeDeliveryCompensation, bool, error) {
	now = normalizeRuntimeDeliveryCompensationTime(now, time.Time{})
	current, err := item.Normalize(now)
	if err != nil {
		return RuntimeDeliveryCompensation{}, false, err
	}
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeDeliveryCompensationOwnerLength || current.Status != RuntimeDeliveryCompensationProcessing || current.LeaseOwner != owner || current.LeaseExpiresAt == nil || !current.LeaseExpiresAt.After(now) {
		return RuntimeDeliveryCompensation{}, false, ErrConflict
	}
	message = SanitizeRuntimeString(strings.TrimSpace(message))
	if len(message) > MaxRuntimeDeliveryCompensationErrorLength {
		message = message[:MaxRuntimeDeliveryCompensationErrorLength]
	}
	current.LastError = message
	current.LeaseOwner = ""
	current.LeaseExpiresAt = nil
	current.CompletedAt = nil
	if current.Attempt >= MaxRuntimeDeliveryCompensationAttempts {
		current.Status = RuntimeDeliveryCompensationFailed
	} else {
		current.Status = RuntimeDeliveryCompensationQueued
		current.AvailableAt = now.Add(RuntimeDeliveryCompensationBackoff(current.Attempt))
	}
	current.UpdatedAt = now
	current.Revision++
	normalized, err := current.Normalize(now)
	if err != nil {
		return RuntimeDeliveryCompensation{}, false, err
	}
	return normalized, true, nil
}

// RetryRuntimeDeliveryCompensation releases a claim and applies the bounded
// retry/backoff schedule.
func RetryRuntimeDeliveryCompensation(item RuntimeDeliveryCompensation, owner string, now time.Time, message string) (RuntimeDeliveryCompensation, bool, error) {
	return retryRuntimeDeliveryCompensation(item, owner, now, message)
}

func failRuntimeDeliveryCompensation(item RuntimeDeliveryCompensation, owner string, now time.Time, message string) (RuntimeDeliveryCompensation, bool, error) {
	now = normalizeRuntimeDeliveryCompensationTime(now, time.Time{})
	current, err := item.Normalize(now)
	if err != nil {
		return RuntimeDeliveryCompensation{}, false, err
	}
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeDeliveryCompensationOwnerLength || current.Status != RuntimeDeliveryCompensationProcessing || current.LeaseOwner != owner || current.LeaseExpiresAt == nil || !current.LeaseExpiresAt.After(now) {
		return RuntimeDeliveryCompensation{}, false, ErrConflict
	}
	message = SanitizeRuntimeString(strings.TrimSpace(message))
	if len(message) > MaxRuntimeDeliveryCompensationErrorLength {
		message = message[:MaxRuntimeDeliveryCompensationErrorLength]
	}
	current.Status = RuntimeDeliveryCompensationFailed
	current.LeaseOwner = ""
	current.LeaseExpiresAt = nil
	current.CompletedAt = nil
	current.LastError = message
	current.UpdatedAt = now
	current.Revision++
	normalized, err := current.Normalize(now)
	if err != nil {
		return RuntimeDeliveryCompensation{}, false, err
	}
	return normalized, true, nil
}

// FailRuntimeDeliveryCompensation records a terminal compensation failure.
func FailRuntimeDeliveryCompensation(item RuntimeDeliveryCompensation, owner string, now time.Time, message string) (RuntimeDeliveryCompensation, bool, error) {
	return failRuntimeDeliveryCompensation(item, owner, now, message)
}

// RuntimeDeliveryCompensationRepository is the source-side durable queue for
// best-effort Abort work. Implementations must enforce identity, lease owner,
// expiry and CAS at their storage boundary.
type RuntimeDeliveryCompensationRepository interface {
	EnqueueRuntimeDeliveryCompensation(context.Context, RuntimeDeliveryCompensation) (RuntimeDeliveryCompensation, error)
	GetRuntimeDeliveryCompensation(context.Context, string) (RuntimeDeliveryCompensation, error)
	ListRuntimeDeliveryCompensations(context.Context, string, RuntimeDeliveryKind, RuntimeDeliveryCompensationStatus, int) ([]RuntimeDeliveryCompensation, error)
	ClaimRuntimeDeliveryCompensation(context.Context, string, time.Time, time.Duration) (RuntimeDeliveryCompensation, bool, error)
	CompleteRuntimeDeliveryCompensation(context.Context, string, string, time.Time) (bool, error)
	RetryRuntimeDeliveryCompensation(context.Context, string, string, time.Time, string) (bool, error)
	FailRuntimeDeliveryCompensation(context.Context, string, string, time.Time, string) (bool, error)
	DeferRuntimeDeliveryCompensation(context.Context, string, string, time.Time, time.Time, string) (bool, error)
}
