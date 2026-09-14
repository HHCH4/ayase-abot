package runtime

import (
	"fmt"
	"strings"
	"time"
)

// RuntimeEventDeliveryTransactionStatus is the destination-side state of a
// two-phase event delivery. A prepared transaction is not visible through the
// normal event inbox until Commit succeeds.
type RuntimeEventDeliveryTransactionStatus string

const (
	RuntimeEventDeliveryTransactionPrepared   RuntimeEventDeliveryTransactionStatus = "prepared"
	RuntimeEventDeliveryTransactionCommitted  RuntimeEventDeliveryTransactionStatus = "committed"
	RuntimeEventDeliveryTransactionAborted    RuntimeEventDeliveryTransactionStatus = "aborted"
	DefaultRuntimeEventDeliveryTransactionTTL                                       = 10 * time.Minute
)

// RuntimeEventDeliveryTransaction stores the bounded signed metadata
// captured during prepare. It deliberately contains no event body, prompt,
// tool arguments or diagnostic payload.
type RuntimeEventDeliveryTransaction struct {
	Version      int                                   `json:"version"`
	DeliveryID   string                                `json:"delivery_id"`
	EventID      string                                `json:"event_id"`
	Source       string                                `json:"source"`
	Destination  string                                `json:"destination"`
	InvocationID string                                `json:"invocation_id"`
	Sequence     int64                                 `json:"sequence"`
	Type         string                                `json:"type"`
	Timestamp    time.Time                             `json:"timestamp"`
	IssuedAt     time.Time                             `json:"issued_at,omitempty"`
	EventDigest  string                                `json:"event_digest"`
	Signature    string                                `json:"-"`
	Status       RuntimeEventDeliveryTransactionStatus `json:"status"`
	ExpiresAt    time.Time                             `json:"expires_at"`
	CreatedAt    time.Time                             `json:"created_at"`
	UpdatedAt    time.Time                             `json:"updated_at"`
}

func cloneRuntimeEventDeliveryTransaction(item RuntimeEventDeliveryTransaction) RuntimeEventDeliveryTransaction {
	return item
}

func newRuntimeEventDeliveryTransaction(envelope RuntimeEventDeliveryEnvelope, now time.Time) (RuntimeEventDeliveryTransaction, error) {
	normalized, err := NormalizeRuntimeEventDeliveryEnvelope(envelope)
	if err != nil {
		return RuntimeEventDeliveryTransaction{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	return RuntimeEventDeliveryTransaction{
		Version: normalized.Version, DeliveryID: normalized.DeliveryID, EventID: normalized.EventID,
		Source: normalized.Source, Destination: normalized.Destination, InvocationID: normalized.InvocationID,
		Sequence: normalized.Sequence, Type: normalized.Type, Timestamp: normalized.Timestamp, IssuedAt: normalized.IssuedAt,
		EventDigest: normalized.EventDigest, Signature: normalized.Signature,
		Status: RuntimeEventDeliveryTransactionPrepared, ExpiresAt: now.Add(DefaultRuntimeEventDeliveryTransactionTTL),
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

// NewRuntimeEventDeliveryTransaction creates a prepared destination record
// after the receiver has authenticated the envelope. It is exported for
// custom inbox implementations and deterministic tests.
func NewRuntimeEventDeliveryTransaction(envelope RuntimeEventDeliveryEnvelope, now time.Time) (RuntimeEventDeliveryTransaction, error) {
	return newRuntimeEventDeliveryTransaction(envelope, now)
}

func (t RuntimeEventDeliveryTransaction) envelope() RuntimeEventDeliveryEnvelope {
	return RuntimeEventDeliveryEnvelope{
		Version: t.Version, Source: t.Source, Destination: t.Destination, DeliveryID: t.DeliveryID,
		EventID: t.EventID, InvocationID: t.InvocationID, Sequence: t.Sequence, Type: t.Type,
		Timestamp: t.Timestamp, IssuedAt: t.IssuedAt, EventDigest: t.EventDigest, Signature: t.Signature,
	}
}

// Envelope returns the signed metadata captured during prepare. It is used by
// storage implementations when atomically publishing a committed transaction
// to their normal inbox.
func (t RuntimeEventDeliveryTransaction) Envelope() RuntimeEventDeliveryEnvelope {
	return t.envelope()
}

func (t RuntimeEventDeliveryTransaction) normalize(now time.Time) (RuntimeEventDeliveryTransaction, error) {
	normalized, err := NormalizeRuntimeEventDeliveryEnvelope(t.envelope())
	if err != nil {
		return RuntimeEventDeliveryTransaction{}, err
	}
	if t.Status == "" {
		t.Status = RuntimeEventDeliveryTransactionPrepared
	}
	switch t.Status {
	case RuntimeEventDeliveryTransactionPrepared, RuntimeEventDeliveryTransactionCommitted, RuntimeEventDeliveryTransactionAborted:
	default:
		return RuntimeEventDeliveryTransaction{}, fmt.Errorf("%w: event transaction status %q 不受支持", ErrInvalidRuntimeEventDelivery, t.Status)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if t.ExpiresAt.IsZero() {
		t.ExpiresAt = now.Add(DefaultRuntimeEventDeliveryTransactionTTL)
	} else {
		t.ExpiresAt = t.ExpiresAt.UTC()
	}
	if t.ExpiresAt.Before(now) && t.Status == RuntimeEventDeliveryTransactionPrepared {
		return RuntimeEventDeliveryTransaction{}, fmt.Errorf("%w: event transaction 已过期", ErrRuntimeEventDeliveryStale)
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
	t.Version, t.DeliveryID, t.EventID = normalized.Version, normalized.DeliveryID, normalized.EventID
	t.Source, t.Destination, t.InvocationID = normalized.Source, normalized.Destination, normalized.InvocationID
	t.Sequence, t.Type, t.Timestamp, t.IssuedAt, t.EventDigest, t.Signature = normalized.Sequence, normalized.Type, normalized.Timestamp, normalized.IssuedAt, normalized.EventDigest, normalized.Signature
	return t, nil
}

// Normalize validates a transaction and returns a defensive copy.
func (t RuntimeEventDeliveryTransaction) Normalize(now time.Time) (RuntimeEventDeliveryTransaction, error) {
	return t.normalize(now)
}

// Matches compares immutable event identity and ignores wire-only version,
// IssuedAt and HMAC signature. Authentication happens at the receiver before
// this ledger is reached; ignoring those fields lets a retry refresh its
// freshness proof while retaining the same event metadata.
func (t RuntimeEventDeliveryTransaction) Matches(envelope RuntimeEventDeliveryEnvelope) bool {
	normalized, err := NormalizeRuntimeEventDeliveryEnvelope(envelope)
	if err != nil {
		return false
	}
	return strings.TrimSpace(t.DeliveryID) == normalized.DeliveryID &&
		strings.TrimSpace(t.EventID) == normalized.EventID && strings.TrimSpace(t.Source) == normalized.Source &&
		strings.TrimSpace(t.Destination) == normalized.Destination && strings.TrimSpace(t.InvocationID) == normalized.InvocationID &&
		t.Sequence == normalized.Sequence && strings.TrimSpace(t.Type) == normalized.Type &&
		t.Timestamp.UTC().Equal(normalized.Timestamp.UTC()) &&
		strings.TrimSpace(strings.ToLower(t.EventDigest)) == normalized.EventDigest
}

// RuntimeEventDeliveryReceipt acknowledges one exact transaction phase.
// Phase is omitted by the legacy one-phase Deliver path.
type RuntimeEventDeliveryReceipt struct {
	Version      int    `json:"version"`
	DeliveryID   string `json:"delivery_id"`
	EventID      string `json:"event_id"`
	InvocationID string `json:"invocation_id"`
	Sequence     int64  `json:"sequence"`
	Type         string `json:"type"`
	EventDigest  string `json:"event_digest"`
	Phase        string `json:"phase,omitempty"`
	Duplicate    bool   `json:"duplicate"`
}

const (
	RuntimeEventDeliveryTransactionPhasePrepared  = "prepared"
	RuntimeEventDeliveryTransactionPhaseCommitted = "committed"
	RuntimeEventDeliveryTransactionPhaseAborted   = "aborted"
)

// ValidateAgainst proves that a transport acknowledged the exact event
// cursor that was claimed. Coordinator repeats this check for custom
// in-process adapters before releasing the source outbox lease.
func (r RuntimeEventDeliveryReceipt) ValidateAgainst(envelope RuntimeEventDeliveryEnvelope) error {
	normalized, err := NormalizeRuntimeEventDeliveryEnvelope(envelope)
	if err != nil {
		return err
	}
	if r.Version != normalized.Version || strings.TrimSpace(r.DeliveryID) != normalized.DeliveryID ||
		strings.TrimSpace(r.EventID) != normalized.EventID || strings.TrimSpace(r.InvocationID) != normalized.InvocationID ||
		r.Sequence != normalized.Sequence || strings.TrimSpace(r.Type) != normalized.Type ||
		strings.TrimSpace(strings.ToLower(r.EventDigest)) != normalized.EventDigest {
		return fmt.Errorf("%w: receipt metadata 不一致", ErrRuntimeEventDeliveryAuth)
	}
	return nil
}

func (r RuntimeEventDeliveryReceipt) ValidateAgainstPhase(envelope RuntimeEventDeliveryEnvelope, phase string) error {
	if err := r.ValidateAgainst(envelope); err != nil {
		return err
	}
	if strings.TrimSpace(r.Phase) != strings.TrimSpace(phase) {
		return fmt.Errorf("%w: receipt phase %q 与期望 %q 不一致", ErrRuntimeEventDeliveryAuth, r.Phase, phase)
	}
	return nil
}
