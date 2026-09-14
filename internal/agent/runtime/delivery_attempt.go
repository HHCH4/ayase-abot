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

// RuntimeDeliveryKind identifies the source outbox family represented by an
// attempt journal.  The journal is deliberately separate from the four
// payload cursors so a phase record cannot be acknowledged by the wrong
// dispatcher.
type RuntimeDeliveryKind string

const (
	RuntimeDeliveryKindEvent      RuntimeDeliveryKind = "event"
	RuntimeDeliveryKindCheckpoint RuntimeDeliveryKind = "checkpoint"
	RuntimeDeliveryKindConfig     RuntimeDeliveryKind = "config"
	RuntimeDeliveryKindRejection  RuntimeDeliveryKind = "approval_rejection"
)

// RuntimeDeliveryAttemptPhase is the source-side recovery state.  The
// destination outcome phases (accepted/committed) are terminal proofs for the
// transport, while completed means the source outbox acknowledgement was also
// recorded.  A crash can therefore resume from the latest durable phase.
type RuntimeDeliveryAttemptPhase string

const (
	RuntimeDeliveryAttemptStarted   RuntimeDeliveryAttemptPhase = "started"
	RuntimeDeliveryAttemptPrepared  RuntimeDeliveryAttemptPhase = "prepared"
	RuntimeDeliveryAttemptAccepted  RuntimeDeliveryAttemptPhase = "accepted"
	RuntimeDeliveryAttemptCommitted RuntimeDeliveryAttemptPhase = "committed"
	RuntimeDeliveryAttemptFailed    RuntimeDeliveryAttemptPhase = "failed"
	RuntimeDeliveryAttemptCompleted RuntimeDeliveryAttemptPhase = "completed"
)

const (
	MaxRuntimeDeliveryAttemptIDLength          = 240
	MaxRuntimeDeliveryAttemptKindLength        = 40
	MaxRuntimeDeliveryAttemptSourceLength      = MaxRuntimeEventDeliverySourceLength
	MaxRuntimeDeliveryAttemptDestinationLength = MaxRuntimeEventDeliveryTargetLength
	MaxRuntimeDeliveryAttemptDeliveryIDLength  = 512
	MaxRuntimeDeliveryAttemptInvocationLength  = 512
	MaxRuntimeDeliveryAttemptOutboxIDLength    = 512
	MaxRuntimeDeliveryAttemptOwnerLength       = 160
	MaxRuntimeDeliveryAttemptErrorLength       = 4096
	MaxRuntimeDeliveryAttemptAttempts          = 8
	MaxRuntimeDeliveryAttemptLease             = 10 * time.Minute
)

var (
	ErrInvalidRuntimeDeliveryAttempt     = errors.New("Runtime delivery attempt journal 无效")
	ErrRuntimeDeliveryAttemptUnavailable = errors.New("Runtime delivery attempt journal 未配置")
)

func validRuntimeDeliveryKind(kind RuntimeDeliveryKind) bool {
	switch kind {
	case RuntimeDeliveryKindEvent, RuntimeDeliveryKindCheckpoint, RuntimeDeliveryKindConfig, RuntimeDeliveryKindRejection:
		return true
	default:
		return false
	}
}

func validRuntimeDeliveryAttemptPhase(phase RuntimeDeliveryAttemptPhase) bool {
	switch phase {
	case RuntimeDeliveryAttemptStarted, RuntimeDeliveryAttemptPrepared, RuntimeDeliveryAttemptAccepted,
		RuntimeDeliveryAttemptCommitted, RuntimeDeliveryAttemptFailed, RuntimeDeliveryAttemptCompleted:
		return true
	default:
		return false
	}
}

func runtimeDeliveryAttemptDestinationTerminal(phase RuntimeDeliveryAttemptPhase) bool {
	return phase == RuntimeDeliveryAttemptAccepted || phase == RuntimeDeliveryAttemptCommitted || phase == RuntimeDeliveryAttemptCompleted
}

// RuntimeDeliveryAttempt is metadata-only.  It never stores an event body,
// checkpoint projection, rejection reason or configuration content.  Outbox
// payloads remain owned by their original source cursor.
type RuntimeDeliveryAttempt struct {
	ID             string                      `json:"id"`
	Kind           RuntimeDeliveryKind         `json:"kind"`
	Source         string                      `json:"source"`
	Destination    string                      `json:"destination"`
	DeliveryID     string                      `json:"delivery_id"`
	InvocationID   string                      `json:"invocation_id"`
	OutboxID       string                      `json:"outbox_id"`
	OutboxRevision int64                       `json:"outbox_revision"`
	Attempt        int                         `json:"attempt"`
	Phase          RuntimeDeliveryAttemptPhase `json:"phase"`
	LeaseOwner     string                      `json:"-"`
	LeaseExpiresAt *time.Time                  `json:"-"`
	LastError      string                      `json:"last_error,omitempty"`
	Revision       int64                       `json:"revision"`
	CreatedAt      time.Time                   `json:"created_at"`
	UpdatedAt      time.Time                   `json:"updated_at"`
	CompletedAt    *time.Time                  `json:"completed_at,omitempty"`
}

// RuntimeDeliveryAttemptID derives one stable journal row for a route and
// immutable delivery identity.  Outbox retries must reuse this ID; a changed
// route or delivery identity is a conflict rather than a second transaction.
func RuntimeDeliveryAttemptID(kind RuntimeDeliveryKind, source, destination, deliveryID string) string {
	material := strings.Join([]string{strings.TrimSpace(string(kind)), strings.TrimSpace(source), strings.TrimSpace(destination), strings.TrimSpace(deliveryID)}, "\x00")
	sum := sha256.Sum256([]byte(material))
	return "runtime-delivery-attempt-" + hex.EncodeToString(sum[:16])
}

func (a RuntimeDeliveryAttempt) Normalize(now time.Time) (RuntimeDeliveryAttempt, error) {
	a.ID = strings.TrimSpace(a.ID)
	a.Kind = RuntimeDeliveryKind(strings.TrimSpace(string(a.Kind)))
	a.Source = strings.TrimSpace(a.Source)
	a.Destination = strings.TrimSpace(a.Destination)
	a.DeliveryID = strings.TrimSpace(a.DeliveryID)
	a.InvocationID = strings.TrimSpace(a.InvocationID)
	a.OutboxID = strings.TrimSpace(a.OutboxID)
	a.LeaseOwner = strings.TrimSpace(a.LeaseOwner)
	a.LastError = SanitizeRuntimeString(strings.TrimSpace(a.LastError))
	if a.ID == "" || a.Kind == "" || a.Source == "" || a.Destination == "" || a.DeliveryID == "" || a.InvocationID == "" || a.OutboxID == "" {
		return RuntimeDeliveryAttempt{}, fmt.Errorf("%w: metadata 不完整", ErrInvalidRuntimeDeliveryAttempt)
	}
	if !validRuntimeDeliveryKind(a.Kind) {
		return RuntimeDeliveryAttempt{}, fmt.Errorf("%w: kind %q 不受支持", ErrInvalidRuntimeDeliveryAttempt, a.Kind)
	}
	if a.ID != RuntimeDeliveryAttemptID(a.Kind, a.Source, a.Destination, a.DeliveryID) {
		return RuntimeDeliveryAttempt{}, fmt.Errorf("%w: ID 不是稳定派生值", ErrInvalidRuntimeDeliveryAttempt)
	}
	if len(a.ID) > MaxRuntimeDeliveryAttemptIDLength || len(a.Kind) > MaxRuntimeDeliveryAttemptKindLength || len(a.Source) > MaxRuntimeDeliveryAttemptSourceLength || len(a.Destination) > MaxRuntimeDeliveryAttemptDestinationLength || len(a.DeliveryID) > MaxRuntimeDeliveryAttemptDeliveryIDLength || len(a.InvocationID) > MaxRuntimeDeliveryAttemptInvocationLength || len(a.OutboxID) > MaxRuntimeDeliveryAttemptOutboxIDLength || len(a.LeaseOwner) > MaxRuntimeDeliveryAttemptOwnerLength {
		return RuntimeDeliveryAttempt{}, fmt.Errorf("%w: metadata 超出长度限制", ErrInvalidRuntimeDeliveryAttempt)
	}
	if a.OutboxRevision <= 0 || a.Attempt <= 0 || a.Attempt > MaxRuntimeDeliveryAttemptAttempts {
		return RuntimeDeliveryAttempt{}, fmt.Errorf("%w: outbox revision/attempt 无效", ErrInvalidRuntimeDeliveryAttempt)
	}
	if a.Phase == "" {
		a.Phase = RuntimeDeliveryAttemptStarted
	}
	if !validRuntimeDeliveryAttemptPhase(a.Phase) {
		return RuntimeDeliveryAttempt{}, fmt.Errorf("%w: phase %q 不受支持", ErrInvalidRuntimeDeliveryAttempt, a.Phase)
	}
	if len(a.LastError) > MaxRuntimeDeliveryAttemptErrorLength {
		a.LastError = a.LastError[:MaxRuntimeDeliveryAttemptErrorLength]
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	} else {
		a.CreatedAt = a.CreatedAt.UTC()
	}
	if a.UpdatedAt.IsZero() {
		a.UpdatedAt = now
	} else {
		a.UpdatedAt = a.UpdatedAt.UTC()
	}
	if a.Revision <= 0 {
		a.Revision = 1
	}
	if a.LeaseExpiresAt != nil {
		expires := a.LeaseExpiresAt.UTC()
		a.LeaseExpiresAt = &expires
		if expires.After(now.Add(MaxRuntimeDeliveryAttemptLease)) {
			return RuntimeDeliveryAttempt{}, fmt.Errorf("%w: lease 超出时长限制", ErrInvalidRuntimeDeliveryAttempt)
		}
	}
	if a.Phase == RuntimeDeliveryAttemptCompleted {
		a.LeaseOwner = ""
		a.LeaseExpiresAt = nil
		if a.CompletedAt == nil {
			finished := a.UpdatedAt
			a.CompletedAt = &finished
		} else {
			finished := a.CompletedAt.UTC()
			a.CompletedAt = &finished
		}
	} else if a.Phase == RuntimeDeliveryAttemptFailed {
		a.LeaseOwner = ""
		a.LeaseExpiresAt = nil
		a.CompletedAt = nil
	} else if a.LeaseOwner == "" || a.LeaseExpiresAt == nil || a.LeaseExpiresAt.IsZero() {
		return RuntimeDeliveryAttempt{}, fmt.Errorf("%w: 活动态必须带有效 lease", ErrInvalidRuntimeDeliveryAttempt)
	}
	return a, nil
}

// NewRuntimeDeliveryAttempt creates the initial source-side journal row after
// a durable outbox claim has won.  The caller supplies the outbox revision and
// attempt so a stale worker cannot later advance a newer claim.
func NewRuntimeDeliveryAttempt(kind RuntimeDeliveryKind, source, destination, deliveryID, invocationID, outboxID string, outboxRevision int64, attempt int, owner string, now time.Time, ttl time.Duration) (RuntimeDeliveryAttempt, error) {
	if ttl <= 0 || ttl > MaxRuntimeDeliveryAttemptLease {
		return RuntimeDeliveryAttempt{}, fmt.Errorf("%w: lease 时长无效", ErrInvalidRuntimeDeliveryAttempt)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	expires := now.Add(ttl)
	return (RuntimeDeliveryAttempt{
		ID: RuntimeDeliveryAttemptID(kind, source, destination, deliveryID), Kind: kind,
		Source: strings.TrimSpace(source), Destination: strings.TrimSpace(destination), DeliveryID: strings.TrimSpace(deliveryID),
		InvocationID: strings.TrimSpace(invocationID), OutboxID: strings.TrimSpace(outboxID), OutboxRevision: outboxRevision,
		Attempt: attempt, Phase: RuntimeDeliveryAttemptStarted, LeaseOwner: strings.TrimSpace(owner), LeaseExpiresAt: &expires,
		CreatedAt: now, UpdatedAt: now,
	}).Normalize(now)
}

// MatchesIdentity ignores phase/lease/revision fields and is used to reject a
// forged delivery cursor that reuses a stable attempt ID for another payload.
func (a RuntimeDeliveryAttempt) MatchesIdentity(candidate RuntimeDeliveryAttempt) bool {
	return a.ID == candidate.ID && a.Kind == candidate.Kind && a.Source == candidate.Source && a.Destination == candidate.Destination && a.DeliveryID == candidate.DeliveryID && a.InvocationID == candidate.InvocationID && a.OutboxID == candidate.OutboxID
}

func runtimeDeliveryAttemptTransitionAllowed(from, to RuntimeDeliveryAttemptPhase) bool {
	if from == to {
		return true
	}
	switch from {
	case RuntimeDeliveryAttemptStarted:
		return to == RuntimeDeliveryAttemptPrepared || to == RuntimeDeliveryAttemptAccepted || to == RuntimeDeliveryAttemptFailed
	case RuntimeDeliveryAttemptPrepared:
		return to == RuntimeDeliveryAttemptCommitted || to == RuntimeDeliveryAttemptFailed
	case RuntimeDeliveryAttemptAccepted, RuntimeDeliveryAttemptCommitted:
		return to == RuntimeDeliveryAttemptCompleted
	case RuntimeDeliveryAttemptFailed, RuntimeDeliveryAttemptCompleted:
		return false
	default:
		return false
	}
}

// BeginRuntimeDeliveryAttempt merges a newly claimed outbox row into an
// existing journal row.  An expired owner may be replaced, but immutable
// identity and attempt monotonicity remain strict.  A prepared/accepted/
// committed phase is preserved so recovery can resume at the safe boundary.
func BeginRuntimeDeliveryAttempt(existing *RuntimeDeliveryAttempt, candidate RuntimeDeliveryAttempt, now time.Time, ttl time.Duration) (RuntimeDeliveryAttempt, error) {
	if ttl <= 0 || ttl > MaxRuntimeDeliveryAttemptLease {
		return RuntimeDeliveryAttempt{}, fmt.Errorf("%w: lease 时长无效", ErrInvalidRuntimeDeliveryAttempt)
	}
	normalizedCandidate, err := candidate.Normalize(now)
	if err != nil {
		return RuntimeDeliveryAttempt{}, err
	}
	if existing == nil {
		return normalizedCandidate, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	current, err := existing.Normalize(now)
	if err != nil {
		return RuntimeDeliveryAttempt{}, err
	}
	if !current.MatchesIdentity(normalizedCandidate) {
		return RuntimeDeliveryAttempt{}, ErrConflict
	}
	if current.Phase == RuntimeDeliveryAttemptCompleted {
		return current, nil
	}
	if current.LeaseOwner != "" && current.LeaseOwner != normalizedCandidate.LeaseOwner && current.LeaseExpiresAt != nil && current.LeaseExpiresAt.After(now) {
		return RuntimeDeliveryAttempt{}, ErrConflict
	}
	if normalizedCandidate.Attempt < current.Attempt {
		return RuntimeDeliveryAttempt{}, ErrConflict
	}
	if normalizedCandidate.Attempt == current.Attempt && current.LeaseOwner != "" && current.LeaseOwner != normalizedCandidate.LeaseOwner {
		return RuntimeDeliveryAttempt{}, ErrConflict
	}
	if current.Phase == RuntimeDeliveryAttemptFailed {
		if normalizedCandidate.Attempt <= current.Attempt {
			return RuntimeDeliveryAttempt{}, ErrConflict
		}
		current.Phase = RuntimeDeliveryAttemptStarted
		current.LastError = ""
		current.CompletedAt = nil
	}
	// A candidate with a newer outbox claim takes the lease while retaining a
	// destination proof already reached by an earlier worker.
	current.Attempt = normalizedCandidate.Attempt
	current.OutboxRevision = normalizedCandidate.OutboxRevision
	current.LeaseOwner = normalizedCandidate.LeaseOwner
	expires := now.Add(ttl)
	current.LeaseExpiresAt = &expires
	current.UpdatedAt = now
	current.Revision++
	return current.Normalize(now)
}

// AdvanceRuntimeDeliveryAttempt applies one CAS phase transition. It returns
// duplicate=true when the requested phase was already durably recorded.
func AdvanceRuntimeDeliveryAttempt(item RuntimeDeliveryAttempt, owner string, expectedRevision int64, phase RuntimeDeliveryAttemptPhase, now time.Time, message string) (RuntimeDeliveryAttempt, bool, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	current, err := item.Normalize(now)
	if err != nil {
		return RuntimeDeliveryAttempt{}, false, err
	}
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeDeliveryAttemptOwnerLength || expectedRevision <= 0 {
		return RuntimeDeliveryAttempt{}, false, ErrConflict
	}
	if current.Revision != expectedRevision {
		return RuntimeDeliveryAttempt{}, false, ErrConflict
	}
	if current.Phase == phase {
		return current, true, nil
	}
	if !runtimeDeliveryAttemptTransitionAllowed(current.Phase, phase) {
		return RuntimeDeliveryAttempt{}, false, ErrConflict
	}
	if current.LeaseOwner != owner || current.LeaseExpiresAt == nil || !current.LeaseExpiresAt.After(now) {
		return RuntimeDeliveryAttempt{}, false, ErrConflict
	}
	current.Phase = phase
	current.LastError = ""
	if phase == RuntimeDeliveryAttemptFailed {
		current.LastError = SanitizeRuntimeString(strings.TrimSpace(message))
		if len(current.LastError) > MaxRuntimeDeliveryAttemptErrorLength {
			current.LastError = current.LastError[:MaxRuntimeDeliveryAttemptErrorLength]
		}
	}
	current.UpdatedAt = now
	current.Revision++
	if phase == RuntimeDeliveryAttemptCompleted {
		finished := now
		current.CompletedAt = &finished
	}
	normalized, normalizeErr := current.Normalize(now)
	return normalized, false, normalizeErr
}

// RuntimeDeliveryAttemptRepository is the source-side durable phase journal.
// Implementations must enforce identity, lease owner and revision CAS in the
// storage boundary, not only in Coordinator memory.
type RuntimeDeliveryAttemptRepository interface {
	BeginRuntimeDeliveryAttempt(context.Context, RuntimeDeliveryAttempt, time.Time, time.Duration) (RuntimeDeliveryAttempt, error)
	AdvanceRuntimeDeliveryAttempt(context.Context, string, string, int64, RuntimeDeliveryAttemptPhase, time.Time, string) (RuntimeDeliveryAttempt, bool, error)
	GetRuntimeDeliveryAttempt(context.Context, string) (RuntimeDeliveryAttempt, error)
	ListRuntimeDeliveryAttempts(context.Context, string, RuntimeDeliveryKind, int) ([]RuntimeDeliveryAttempt, error)
}
