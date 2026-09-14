package runtime

import (
	"fmt"
	"strings"
	"time"

	"Abot/internal/agent"
)

// RuntimeConfigDeliveryTransactionStatus is the destination-side state of a
// two-phase configuration lock.  A prepared projection is not visible in the
// normal inbox until Commit succeeds.
type RuntimeConfigDeliveryTransactionStatus string

const (
	RuntimeConfigDeliveryTransactionPrepared  RuntimeConfigDeliveryTransactionStatus = "prepared"
	RuntimeConfigDeliveryTransactionCommitted RuntimeConfigDeliveryTransactionStatus = "committed"
	RuntimeConfigDeliveryTransactionAborted   RuntimeConfigDeliveryTransactionStatus = "aborted"
)

// RuntimeConfigDeliveryTransaction stores the first authenticated envelope
// and its canonical metadata-only projection.  Retaining the prepare-time
// identity makes a later commit safe even if the source refreshes IssuedAt.
type RuntimeConfigDeliveryTransaction struct {
	Version         int                                    `json:"version"`
	DeliveryID      string                                 `json:"delivery_id"`
	Source          string                                 `json:"source"`
	Destination     string                                 `json:"destination"`
	InvocationID    string                                 `json:"invocation_id"`
	ExpectedDigest  string                                 `json:"expected_digest"`
	SnapshotDigest  string                                 `json:"snapshot_digest"`
	SnapshotVersion int                                    `json:"snapshot_version"`
	IdempotencyKey  string                                 `json:"idempotency_key,omitempty"`
	Signature       string                                 `json:"-"`
	Timestamp       time.Time                              `json:"timestamp"`
	IssuedAt        time.Time                              `json:"-"`
	Projection      RuntimeConfigDeliveryProjection        `json:"-"`
	Status          RuntimeConfigDeliveryTransactionStatus `json:"status"`
	ExpiresAt       time.Time                              `json:"expires_at"`
	CreatedAt       time.Time                              `json:"created_at"`
	UpdatedAt       time.Time                              `json:"updated_at"`
}

func (s RuntimeConfigDeliveryTransactionStatus) terminal() bool {
	return s == RuntimeConfigDeliveryTransactionCommitted || s == RuntimeConfigDeliveryTransactionAborted
}

func cloneRuntimeConfigDeliveryTransaction(item RuntimeConfigDeliveryTransaction) RuntimeConfigDeliveryTransaction {
	item.Projection = cloneRuntimeConfigDeliveryProjection(item.Projection)
	return item
}

func newRuntimeConfigDeliveryTransaction(envelope RuntimeConfigDeliveryEnvelope, projection RuntimeConfigDeliveryProjection, now time.Time) (RuntimeConfigDeliveryTransaction, error) {
	normalized, err := NormalizeRuntimeConfigDeliveryEnvelope(envelope)
	if err != nil {
		return RuntimeConfigDeliveryTransaction{}, err
	}
	parsed, err := normalizeRuntimeConfigDeliveryProjection(projection)
	if err != nil {
		return RuntimeConfigDeliveryTransaction{}, err
	}
	if parsed.Version != normalized.SnapshotVersion || agentRuntimeConfigSnapshotDigest(parsed) != normalized.SnapshotDigest {
		return RuntimeConfigDeliveryTransaction{}, fmt.Errorf("%w: projection digest/version 不一致", ErrRuntimeConfigDeliveryAuth)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	return RuntimeConfigDeliveryTransaction{
		Version: normalized.Version, DeliveryID: normalized.DeliveryID, Source: normalized.Source,
		Destination: normalized.Destination, InvocationID: normalized.InvocationID,
		ExpectedDigest: normalized.ExpectedDigest, SnapshotDigest: normalized.SnapshotDigest,
		SnapshotVersion: normalized.SnapshotVersion, IdempotencyKey: normalized.IdempotencyKey,
		Signature: normalized.Signature, Timestamp: normalized.Timestamp, IssuedAt: normalized.IssuedAt,
		Projection: parsed, Status: RuntimeConfigDeliveryTransactionPrepared,
		ExpiresAt: now.Add(DefaultRuntimeConfigDeliveryTransactionTTL), CreatedAt: now, UpdatedAt: now,
	}, nil
}

func NewRuntimeConfigDeliveryTransaction(envelope RuntimeConfigDeliveryEnvelope, projection RuntimeConfigDeliveryProjection, now time.Time) (RuntimeConfigDeliveryTransaction, error) {
	return newRuntimeConfigDeliveryTransaction(envelope, projection, now)
}

func normalizeRuntimeConfigDeliveryProjection(projection RuntimeConfigDeliveryProjection) (RuntimeConfigDeliveryProjection, error) {
	return agentRuntimeConfigSnapshotNormalize(projection)
}

func agentRuntimeConfigSnapshotNormalize(projection RuntimeConfigDeliveryProjection) (RuntimeConfigDeliveryProjection, error) {
	encoded, err := marshalRuntimeConfigDeliveryProjection(projection)
	if err != nil {
		return RuntimeConfigDeliveryProjection{}, err
	}
	return parseRuntimeConfigDeliveryProjection(encoded)
}

func marshalRuntimeConfigDeliveryProjection(projection RuntimeConfigDeliveryProjection) (string, error) {
	encoded := mustMarshalRuntimeConfigProjection(projection)
	if len(encoded) > MaxRuntimeConfigDeliveryProjectionBytes {
		return "", fmt.Errorf("%w: projection 超过字节上限", ErrInvalidRuntimeConfigDelivery)
	}
	return encoded, nil
}

func parseRuntimeConfigDeliveryProjection(encoded string) (RuntimeConfigDeliveryProjection, error) {
	return agent.ParseRuntimeConfigSnapshot(encoded)
}

func agentRuntimeConfigSnapshotDigest(projection RuntimeConfigDeliveryProjection) string {
	encoded, err := marshalRuntimeConfigDeliveryProjection(projection)
	if err != nil {
		return ""
	}
	return agent.RuntimeConfigSnapshotDigest(encoded)
}

func (t RuntimeConfigDeliveryTransaction) normalize(now time.Time) (RuntimeConfigDeliveryTransaction, error) {
	envelope := RuntimeConfigDeliveryEnvelope{
		Version: t.Version, Source: t.Source, Destination: t.Destination, DeliveryID: t.DeliveryID,
		InvocationID: t.InvocationID, ExpectedDigest: t.ExpectedDigest, SnapshotDigest: t.SnapshotDigest,
		SnapshotVersion: t.SnapshotVersion, IdempotencyKey: t.IdempotencyKey, Decision: RuntimeConfigDeliveryDecision,
		Timestamp: t.Timestamp, IssuedAt: t.IssuedAt, Signature: t.Signature,
	}
	normalized, err := NormalizeRuntimeConfigDeliveryEnvelope(envelope)
	if err != nil {
		return RuntimeConfigDeliveryTransaction{}, err
	}
	projection, err := normalizeRuntimeConfigDeliveryProjection(t.Projection)
	if err != nil {
		return RuntimeConfigDeliveryTransaction{}, err
	}
	if projection.Version != normalized.SnapshotVersion || agentRuntimeConfigSnapshotDigest(projection) != normalized.SnapshotDigest {
		return RuntimeConfigDeliveryTransaction{}, fmt.Errorf("%w: transaction projection 不一致", ErrRuntimeConfigDeliveryAuth)
	}
	if t.Status == "" {
		t.Status = RuntimeConfigDeliveryTransactionPrepared
	}
	switch t.Status {
	case RuntimeConfigDeliveryTransactionPrepared, RuntimeConfigDeliveryTransactionCommitted, RuntimeConfigDeliveryTransactionAborted:
	default:
		return RuntimeConfigDeliveryTransaction{}, fmt.Errorf("%w: transaction status 不受支持", ErrInvalidRuntimeConfigDelivery)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if t.ExpiresAt.IsZero() {
		t.ExpiresAt = now.Add(DefaultRuntimeConfigDeliveryTransactionTTL)
	} else {
		t.ExpiresAt = t.ExpiresAt.UTC()
	}
	if t.Status == RuntimeConfigDeliveryTransactionPrepared && t.ExpiresAt.Before(now) {
		return RuntimeConfigDeliveryTransaction{}, fmt.Errorf("%w: transaction 已过期", ErrRuntimeConfigDeliveryStale)
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
	t.ExpectedDigest, t.SnapshotDigest, t.SnapshotVersion, t.IdempotencyKey = normalized.ExpectedDigest, normalized.SnapshotDigest, normalized.SnapshotVersion, normalized.IdempotencyKey
	t.Signature, t.Timestamp, t.IssuedAt, t.Projection = normalized.Signature, normalized.Timestamp, normalized.IssuedAt, projection
	return t, nil
}

func (t RuntimeConfigDeliveryTransaction) Normalize(now time.Time) (RuntimeConfigDeliveryTransaction, error) {
	return t.normalize(now)
}

// Envelope reconstructs the authenticated prepare-time identity used by the
// destination commit. IssuedAt/signature are intentionally retained from the
// prepare record so commit does not widen the replay window or change the
// immutable migration identity.
func (t RuntimeConfigDeliveryTransaction) Envelope() RuntimeConfigDeliveryEnvelope {
	return RuntimeConfigDeliveryEnvelope{
		Version: t.Version, Source: t.Source, Destination: t.Destination,
		DeliveryID: t.DeliveryID, InvocationID: t.InvocationID,
		ExpectedDigest: t.ExpectedDigest, SnapshotDigest: t.SnapshotDigest,
		SnapshotVersion: t.SnapshotVersion, Decision: RuntimeConfigDeliveryDecision,
		IdempotencyKey: t.IdempotencyKey, Timestamp: t.Timestamp, IssuedAt: t.IssuedAt,
		Signature: t.Signature,
	}
}

func (t RuntimeConfigDeliveryTransaction) Matches(envelope RuntimeConfigDeliveryEnvelope, projection RuntimeConfigDeliveryProjection) bool {
	normalized, err := NormalizeRuntimeConfigDeliveryEnvelope(envelope)
	if err != nil {
		return false
	}
	normalizedProjection, err := normalizeRuntimeConfigDeliveryProjection(projection)
	if err != nil {
		return false
	}
	if t.Version != normalized.Version || strings.TrimSpace(t.DeliveryID) != normalized.DeliveryID || strings.TrimSpace(t.Source) != normalized.Source || strings.TrimSpace(t.Destination) != normalized.Destination || strings.TrimSpace(t.InvocationID) != normalized.InvocationID || strings.TrimSpace(t.ExpectedDigest) != normalized.ExpectedDigest || strings.TrimSpace(t.SnapshotDigest) != normalized.SnapshotDigest || t.SnapshotVersion != normalized.SnapshotVersion || strings.TrimSpace(t.IdempotencyKey) != normalized.IdempotencyKey {
		return false
	}
	return agentRuntimeConfigSnapshotDigest(t.Projection) == agentRuntimeConfigSnapshotDigest(normalizedProjection)
}
