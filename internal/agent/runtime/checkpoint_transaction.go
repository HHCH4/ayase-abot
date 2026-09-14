package runtime

import (
	"fmt"
	"strings"
	"time"
)

// RuntimeCheckpointDeliveryTransactionStatus is the destination-side state
// of a two-phase checkpoint delivery.  A prepared transaction is not visible
// through the normal checkpoint inbox until Commit succeeds.
type RuntimeCheckpointDeliveryTransactionStatus string

const (
	RuntimeCheckpointDeliveryTransactionPrepared   RuntimeCheckpointDeliveryTransactionStatus = "prepared"
	RuntimeCheckpointDeliveryTransactionCommitted  RuntimeCheckpointDeliveryTransactionStatus = "committed"
	RuntimeCheckpointDeliveryTransactionAborted    RuntimeCheckpointDeliveryTransactionStatus = "aborted"
	DefaultRuntimeCheckpointDeliveryTransactionTTL                                            = 10 * time.Minute
)

// RuntimeCheckpointDeliveryTransaction stores one bounded projection and the
// signed envelope that authenticated its first prepare.  The original
// envelope is retained so a later commit can make the destination inbox
// idempotent even if the source has refreshed the timestamp/signature.
type RuntimeCheckpointDeliveryTransaction struct {
	Version          int                                        `json:"version"`
	DeliveryID       string                                     `json:"delivery_id"`
	Source           string                                     `json:"source"`
	Destination      string                                     `json:"destination"`
	InvocationID     string                                     `json:"invocation_id"`
	SnapshotRevision int64                                      `json:"snapshot_revision"`
	EventSequence    int64                                      `json:"event_sequence"`
	SnapshotDigest   string                                     `json:"snapshot_digest"`
	Signature        string                                     `json:"-"`
	Timestamp        time.Time                                  `json:"timestamp"`
	Projection       RuntimeCheckpointProjection                `json:"-"`
	Status           RuntimeCheckpointDeliveryTransactionStatus `json:"status"`
	ExpiresAt        time.Time                                  `json:"expires_at"`
	CreatedAt        time.Time                                  `json:"created_at"`
	UpdatedAt        time.Time                                  `json:"updated_at"`
}

func (t RuntimeCheckpointDeliveryTransactionStatus) terminal() bool {
	return t == RuntimeCheckpointDeliveryTransactionCommitted || t == RuntimeCheckpointDeliveryTransactionAborted
}

func cloneRuntimeCheckpointDeliveryTransaction(item RuntimeCheckpointDeliveryTransaction) RuntimeCheckpointDeliveryTransaction {
	item.Projection = cloneRuntimeCheckpointProjection(item.Projection)
	return item
}

func newRuntimeCheckpointDeliveryTransaction(envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection, now time.Time) (RuntimeCheckpointDeliveryTransaction, error) {
	normalized, normalizedProjection, err := normalizeRuntimeCheckpointDeliveryPair(envelope, projection)
	if err != nil {
		return RuntimeCheckpointDeliveryTransaction{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	return RuntimeCheckpointDeliveryTransaction{
		Version: normalized.Version, DeliveryID: normalized.DeliveryID, Source: normalized.Source,
		Destination: normalized.Destination, InvocationID: normalized.InvocationID,
		SnapshotRevision: normalized.SnapshotRevision, EventSequence: normalized.EventSequence,
		SnapshotDigest: normalized.SnapshotDigest, Signature: normalized.Signature,
		Timestamp: normalized.Timestamp, Projection: normalizedProjection,
		Status:    RuntimeCheckpointDeliveryTransactionPrepared,
		ExpiresAt: now.Add(DefaultRuntimeCheckpointDeliveryTransactionTTL), CreatedAt: now, UpdatedAt: now,
	}, nil
}

// NewRuntimeCheckpointDeliveryTransaction creates a prepared destination
// record after the receiver has authenticated the envelope.  It is exported
// for custom inbox implementations and deterministic tests.
func NewRuntimeCheckpointDeliveryTransaction(envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection, now time.Time) (RuntimeCheckpointDeliveryTransaction, error) {
	return newRuntimeCheckpointDeliveryTransaction(envelope, projection, now)
}

func (t RuntimeCheckpointDeliveryTransaction) normalize(now time.Time) (RuntimeCheckpointDeliveryTransaction, error) {
	envelope := RuntimeCheckpointDeliveryEnvelope{
		Version: t.Version, Source: t.Source, Destination: t.Destination, DeliveryID: t.DeliveryID,
		InvocationID: t.InvocationID, SnapshotRevision: t.SnapshotRevision, EventSequence: t.EventSequence,
		SnapshotDigest: t.SnapshotDigest, Timestamp: t.Timestamp, Signature: t.Signature,
	}
	normalized, projection, err := normalizeRuntimeCheckpointDeliveryPair(envelope, t.Projection)
	if err != nil {
		return RuntimeCheckpointDeliveryTransaction{}, err
	}
	if t.Status == "" {
		t.Status = RuntimeCheckpointDeliveryTransactionPrepared
	}
	switch t.Status {
	case RuntimeCheckpointDeliveryTransactionPrepared, RuntimeCheckpointDeliveryTransactionCommitted, RuntimeCheckpointDeliveryTransactionAborted:
	default:
		return RuntimeCheckpointDeliveryTransaction{}, fmt.Errorf("%w: checkpoint transaction status %q 不受支持", ErrInvalidRuntimeCheckpointDelivery, t.Status)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if t.ExpiresAt.IsZero() {
		t.ExpiresAt = now.Add(DefaultRuntimeCheckpointDeliveryTransactionTTL)
	} else {
		t.ExpiresAt = t.ExpiresAt.UTC()
	}
	if t.ExpiresAt.Before(now) && t.Status == RuntimeCheckpointDeliveryTransactionPrepared {
		return RuntimeCheckpointDeliveryTransaction{}, fmt.Errorf("%w: checkpoint transaction 已过期", ErrRuntimeCheckpointDeliveryStale)
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
	t.Version, t.Source, t.Destination, t.DeliveryID, t.InvocationID = normalized.Version, normalized.Source, normalized.Destination, normalized.DeliveryID, normalized.InvocationID
	t.SnapshotRevision, t.EventSequence, t.SnapshotDigest = normalized.SnapshotRevision, normalized.EventSequence, normalized.SnapshotDigest
	t.Signature, t.Timestamp, t.Projection = normalized.Signature, normalized.Timestamp, projection
	return t, nil
}

// Normalize validates the transaction and returns a defensive copy.
func (t RuntimeCheckpointDeliveryTransaction) Normalize(now time.Time) (RuntimeCheckpointDeliveryTransaction, error) {
	return t.normalize(now)
}

// Matches compares only immutable transaction identity and projection.  A
// retry may carry a fresh signed envelope, so timestamp/signature are not part
// of duplicate detection.
func (t RuntimeCheckpointDeliveryTransaction) Matches(envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection) bool {
	normalized, normalizedProjection, err := normalizeRuntimeCheckpointDeliveryPair(envelope, projection)
	if err != nil {
		return false
	}
	if t.Version != normalized.Version || strings.TrimSpace(t.DeliveryID) != normalized.DeliveryID || strings.TrimSpace(t.Source) != normalized.Source || strings.TrimSpace(t.Destination) != normalized.Destination || strings.TrimSpace(t.InvocationID) != normalized.InvocationID || t.SnapshotRevision != normalized.SnapshotRevision || t.EventSequence != normalized.EventSequence || strings.TrimSpace(strings.ToLower(t.SnapshotDigest)) != normalized.SnapshotDigest {
		return false
	}
	left, leftErr := RuntimeCheckpointProjectionDigest(t.Projection)
	right, rightErr := RuntimeCheckpointProjectionDigest(normalizedProjection)
	return leftErr == nil && rightErr == nil && left == right
}
