package runtime

// This file contains the source-side durable cursor for configuration
// directory body delivery.  The cursor keeps the bounded public entry in the
// local store so a retry does not need to reread a mutable profile/persona;
// it never carries credentials, bindings, or any other Runtime payload.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	MaxRuntimeConfigDirectoryOutboxAttempts = 8
	MaxRuntimeConfigDirectoryOutboxLease    = 10 * time.Minute
	MaxRuntimeConfigDirectoryOutboxOwner    = 160
	MaxRuntimeConfigDirectoryOutboxID       = 240
	MaxRuntimeConfigDirectoryOutboxError    = 4096
)

// RuntimeConfigDirectoryOutboxStatus is independent from the destination
// catalog. A source cursor may be processing while the destination has
// already accepted the same delivery; reconciliation closes that window.
type RuntimeConfigDirectoryOutboxStatus string

const (
	RuntimeConfigDirectoryOutboxQueued     RuntimeConfigDirectoryOutboxStatus = "queued"
	RuntimeConfigDirectoryOutboxProcessing RuntimeConfigDirectoryOutboxStatus = "processing"
	RuntimeConfigDirectoryOutboxCompleted  RuntimeConfigDirectoryOutboxStatus = "completed"
	RuntimeConfigDirectoryOutboxFailed     RuntimeConfigDirectoryOutboxStatus = "failed"
)

func (s RuntimeConfigDirectoryOutboxStatus) terminal() bool {
	return s == RuntimeConfigDirectoryOutboxCompleted || s == RuntimeConfigDirectoryOutboxFailed
}

// RuntimeConfigDirectoryOutbox is the source-owned durable delivery cursor.
// Entry is private API data and is never encoded as part of a generic outbox
// listing. The entry itself is still bounded and public-only by Normalize.
type RuntimeConfigDirectoryOutbox struct {
	ID             string                             `json:"id"`
	FanoutID       string                             `json:"fanout_id,omitempty"`
	Source         string                             `json:"source"`
	Destination    string                             `json:"destination"`
	DeliveryID     string                             `json:"delivery_id"`
	Kind           RuntimeConfigDirectoryKind         `json:"kind"`
	EntryID        string                             `json:"entry_id"`
	EntryRevision  int                                `json:"entry_revision"`
	PreviousDigest string                             `json:"previous_digest,omitempty"`
	BodyDigest     string                             `json:"body_digest"`
	CorrelationID  string                             `json:"correlation_id,omitempty"`
	IdempotencyKey string                             `json:"idempotency_key,omitempty"`
	Entry          RuntimeConfigDirectoryEntry        `json:"-"`
	Status         RuntimeConfigDirectoryOutboxStatus `json:"status"`
	Attempt        int                                `json:"attempt"`
	Revision       int64                              `json:"revision"`
	AvailableAt    time.Time                          `json:"available_at"`
	LeaseOwner     string                             `json:"-"`
	LeaseExpiresAt *time.Time                         `json:"-"`
	LastError      string                             `json:"last_error,omitempty"`
	CreatedAt      time.Time                          `json:"created_at"`
	UpdatedAt      time.Time                          `json:"updated_at"`
}

// RuntimeConfigDirectoryOutboxID is stable across retries and process
// restarts. DeliveryID already binds the route/body identity, but the
// explicit prefix keeps this cursor distinct from the destination ledger.
func RuntimeConfigDirectoryOutboxID(source, destination, deliveryID string) string {
	material := strings.Join([]string{strings.TrimSpace(source), strings.TrimSpace(destination), strings.TrimSpace(deliveryID)}, "\x00")
	sum := sha256.Sum256([]byte(material))
	return "runtime-config-directory-outbox-" + hex.EncodeToString(sum[:16])
}

func cloneRuntimeConfigDirectoryOutbox(item RuntimeConfigDirectoryOutbox) RuntimeConfigDirectoryOutbox {
	item.Entry = cloneRuntimeConfigDirectoryEntry(item.Entry)
	if item.LeaseExpiresAt != nil {
		value := item.LeaseExpiresAt.UTC()
		item.LeaseExpiresAt = &value
	}
	return item
}

func runtimeConfigDirectoryOutboxBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

// RuntimeConfigDirectoryOutboxBackoff exposes the bounded retry schedule to a
// host scheduler without exposing any private body data.
func RuntimeConfigDirectoryOutboxBackoff(attempt int) time.Duration {
	return runtimeConfigDirectoryOutboxBackoff(attempt)
}

func (o RuntimeConfigDirectoryOutbox) normalize(now time.Time) (RuntimeConfigDirectoryOutbox, error) {
	o.ID = strings.TrimSpace(o.ID)
	o.FanoutID = strings.TrimSpace(o.FanoutID)
	o.Source = strings.TrimSpace(o.Source)
	o.Destination = strings.TrimSpace(o.Destination)
	o.DeliveryID = strings.TrimSpace(o.DeliveryID)
	o.Kind = RuntimeConfigDirectoryKind(strings.TrimSpace(strings.ToLower(string(o.Kind))))
	o.EntryID = strings.TrimSpace(o.EntryID)
	o.PreviousDigest = strings.TrimSpace(strings.ToLower(o.PreviousDigest))
	o.BodyDigest = strings.TrimSpace(strings.ToLower(o.BodyDigest))
	o.CorrelationID = strings.TrimSpace(o.CorrelationID)
	o.IdempotencyKey = strings.TrimSpace(o.IdempotencyKey)
	o.LeaseOwner = strings.TrimSpace(o.LeaseOwner)
	o.LastError = SanitizeRuntimeString(strings.TrimSpace(o.LastError))

	if o.Source == "" || o.Destination == "" || o.DeliveryID == "" || o.EntryID == "" || o.EntryRevision <= 0 {
		return RuntimeConfigDirectoryOutbox{}, fmt.Errorf("%w: directory outbox metadata 不完整", ErrInvalidRuntimeConfigDirectory)
	}
	if len(o.ID) > MaxRuntimeConfigDirectoryOutboxID || len(o.FanoutID) > MaxRuntimeConfigDirectoryFanoutID || len(o.Source) > MaxRuntimeConfigDirectorySourceLength || len(o.Destination) > MaxRuntimeConfigDirectoryTargetLength || len(o.DeliveryID) > MaxRuntimeConfigDirectoryDeliveryID || len(o.CorrelationID) > MaxRuntimeConfigDirectoryCorrelation || len(o.IdempotencyKey) > MaxRuntimeConfigDirectoryIdempotency || len(o.LeaseOwner) > MaxRuntimeConfigDirectoryOutboxOwner {
		return RuntimeConfigDirectoryOutbox{}, fmt.Errorf("%w: directory outbox metadata 超出长度限制", ErrInvalidRuntimeConfigDirectory)
	}
	if !runtimeConfigDirectoryDigestValid(o.BodyDigest) || (o.PreviousDigest != "" && !runtimeConfigDirectoryDigestValid(o.PreviousDigest)) {
		return RuntimeConfigDirectoryOutbox{}, fmt.Errorf("%w: directory outbox digest 无效", ErrInvalidRuntimeConfigDirectory)
	}
	entry, err := o.Entry.Normalize()
	if err != nil {
		return RuntimeConfigDirectoryOutbox{}, err
	}
	if entry.Kind != o.Kind || entry.ID != o.EntryID || entry.Revision != o.EntryRevision {
		return RuntimeConfigDirectoryOutbox{}, fmt.Errorf("%w: directory outbox entry identity 不一致", ErrRuntimeConfigDirectoryAuth)
	}
	bodyDigest, err := RuntimeConfigDirectoryEntryDigest(entry)
	if err != nil {
		return RuntimeConfigDirectoryOutbox{}, err
	}
	if bodyDigest != o.BodyDigest {
		return RuntimeConfigDirectoryOutbox{}, fmt.Errorf("%w: directory outbox body digest 不一致", ErrRuntimeConfigDirectoryAuth)
	}
	expectedDeliveryID := RuntimeConfigDirectoryDeliveryID(o.Source, o.Destination, o.Kind, o.EntryID, o.EntryRevision, o.PreviousDigest, o.BodyDigest, o.IdempotencyKey)
	if o.DeliveryID != expectedDeliveryID || o.ID != RuntimeConfigDirectoryOutboxID(o.Source, o.Destination, o.DeliveryID) {
		return RuntimeConfigDirectoryOutbox{}, fmt.Errorf("%w: directory outbox identity 不一致", ErrRuntimeConfigDirectoryAuth)
	}
	if o.Attempt < 0 || o.Attempt > MaxRuntimeConfigDirectoryOutboxAttempts {
		return RuntimeConfigDirectoryOutbox{}, fmt.Errorf("%w: directory outbox attempt 无效", ErrInvalidRuntimeConfigDirectory)
	}
	if o.Status == "" {
		o.Status = RuntimeConfigDirectoryOutboxQueued
	}
	switch o.Status {
	case RuntimeConfigDirectoryOutboxQueued, RuntimeConfigDirectoryOutboxProcessing, RuntimeConfigDirectoryOutboxCompleted, RuntimeConfigDirectoryOutboxFailed:
	default:
		return RuntimeConfigDirectoryOutbox{}, fmt.Errorf("%w: directory outbox status %q 不受支持", ErrInvalidRuntimeConfigDirectory, o.Status)
	}
	if o.Status == RuntimeConfigDirectoryOutboxProcessing && (o.LeaseOwner == "" || o.LeaseExpiresAt == nil) {
		return RuntimeConfigDirectoryOutbox{}, fmt.Errorf("%w: processing directory outbox 必须带租约", ErrInvalidRuntimeConfigDirectory)
	}
	if len(o.LastError) > MaxRuntimeConfigDirectoryOutboxError {
		o.LastError = o.LastError[:MaxRuntimeConfigDirectoryOutboxError]
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
	if o.Status.terminal() || o.Status == RuntimeConfigDirectoryOutboxQueued {
		o.LeaseOwner = ""
		o.LeaseExpiresAt = nil
	}
	if o.Status == RuntimeConfigDirectoryOutboxProcessing {
		if o.LeaseExpiresAt == nil || o.LeaseExpiresAt.IsZero() || o.LeaseExpiresAt.After(now.Add(MaxRuntimeConfigDirectoryOutboxLease)) {
			return RuntimeConfigDirectoryOutbox{}, fmt.Errorf("%w: directory outbox lease 无效", ErrInvalidRuntimeConfigDirectory)
		}
		expires := o.LeaseExpiresAt.UTC()
		o.LeaseExpiresAt = &expires
	}
	o.Entry = cloneRuntimeConfigDirectoryEntry(entry)
	return o, nil
}

// Normalize validates the immutable body and canonical identity before it is
// accepted by a storage adapter.
func (o RuntimeConfigDirectoryOutbox) Normalize(now time.Time) (RuntimeConfigDirectoryOutbox, error) {
	return o.normalize(now)
}

// NewRuntimeConfigDirectoryOutbox creates a queued source cursor for one
// public profile/persona entry. It does not contact a destination.
func NewRuntimeConfigDirectoryOutbox(source, destination string, entry RuntimeConfigDirectoryEntry, previousDigest, correlationID, idempotencyKey string, now time.Time) (RuntimeConfigDirectoryOutbox, error) {
	entry, err := entry.Normalize()
	if err != nil {
		return RuntimeConfigDirectoryOutbox{}, err
	}
	bodyDigest, err := RuntimeConfigDirectoryEntryDigest(entry)
	if err != nil {
		return RuntimeConfigDirectoryOutbox{}, err
	}
	source = strings.TrimSpace(source)
	destination = strings.TrimSpace(destination)
	previousDigest = strings.TrimSpace(strings.ToLower(previousDigest))
	correlationID = strings.TrimSpace(correlationID)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	deliveryID := RuntimeConfigDirectoryDeliveryID(source, destination, entry.Kind, entry.ID, entry.Revision, previousDigest, bodyDigest, idempotencyKey)
	item := RuntimeConfigDirectoryOutbox{
		ID: RuntimeConfigDirectoryOutboxID(source, destination, deliveryID), Source: source, Destination: destination,
		DeliveryID: deliveryID, Kind: entry.Kind, EntryID: entry.ID, EntryRevision: entry.Revision,
		PreviousDigest: previousDigest, BodyDigest: bodyDigest, CorrelationID: correlationID,
		IdempotencyKey: idempotencyKey, Entry: cloneRuntimeConfigDirectoryEntry(entry), Status: RuntimeConfigDirectoryOutboxQueued,
		AvailableAt: now, CreatedAt: now, UpdatedAt: now,
	}
	return item.normalize(now)
}

// Envelope creates a fresh-attempt envelope while preserving the immutable
// publication timestamp and DeliveryID. Only IssuedAt changes between tries.
func (o RuntimeConfigDirectoryOutbox) Envelope(now time.Time) (RuntimeConfigDirectoryEnvelope, error) {
	normalized, err := o.normalize(now)
	if err != nil {
		return RuntimeConfigDirectoryEnvelope{}, err
	}
	issuedAt := now
	if issuedAt.IsZero() {
		issuedAt = time.Now().UTC()
	} else {
		issuedAt = issuedAt.UTC()
	}
	envelope, err := NewRuntimeConfigDirectoryEnvelope(normalized.Source, normalized.Destination, normalized.Entry, normalized.PreviousDigest, normalized.CorrelationID, normalized.IdempotencyKey, normalized.CreatedAt)
	if err != nil {
		return RuntimeConfigDirectoryEnvelope{}, err
	}
	envelope.IssuedAt = issuedAt
	envelope.Signature = ""
	return envelope.Normalize()
}

// Matches compares only the immutable source/body identity. Queue state is
// intentionally excluded so an idempotent enqueue can return the live cursor.
func (o RuntimeConfigDirectoryOutbox) Matches(other RuntimeConfigDirectoryOutbox) bool {
	left, leftErr := o.normalize(time.Time{})
	right, rightErr := other.normalize(time.Time{})
	if leftErr != nil || rightErr != nil {
		return false
	}
	leftDigest, leftDigestErr := RuntimeConfigDirectoryEntryDigest(left.Entry)
	rightDigest, rightDigestErr := RuntimeConfigDirectoryEntryDigest(right.Entry)
	return leftDigestErr == nil && rightDigestErr == nil && left.ID == right.ID && left.Source == right.Source && left.Destination == right.Destination && left.DeliveryID == right.DeliveryID && left.Kind == right.Kind && left.EntryID == right.EntryID && left.EntryRevision == right.EntryRevision && left.PreviousDigest == right.PreviousDigest && left.BodyDigest == right.BodyDigest && left.CorrelationID == right.CorrelationID && left.IdempotencyKey == right.IdempotencyKey && leftDigest == rightDigest
}

// RuntimeConfigDirectoryReconciler is optional. A source worker can first ask
// the destination for proof after a response-loss window; old transports can
// omit it and rely on the receiver's idempotent Deliver path.
type RuntimeConfigDirectoryReconciler interface {
	ReconcileRuntimeConfigDirectory(context.Context, RuntimeConfigDirectoryEnvelope, RuntimeConfigDirectoryEntry) (RuntimeConfigDirectoryStatus, error)
}

// RuntimeConfigDirectoryOutboxRepository is the optional source-side durable
// cursor for public profile/persona body delivery. Built-in Memory/SQLite
// stores provide lease/CAS/retry semantics; older embedders can keep using
// the explicit one-shot transport entry point.
type RuntimeConfigDirectoryOutboxRepository interface {
	EnqueueRuntimeConfigDirectoryOutbox(context.Context, RuntimeConfigDirectoryOutbox) (RuntimeConfigDirectoryOutbox, error)
	GetRuntimeConfigDirectoryOutbox(context.Context, string) (RuntimeConfigDirectoryOutbox, error)
	ListRuntimeConfigDirectoryOutbox(context.Context, RuntimeConfigDirectoryKind, string, RuntimeConfigDirectoryOutboxStatus, int) ([]RuntimeConfigDirectoryOutbox, error)
	ClaimRuntimeConfigDirectoryOutbox(context.Context, string, time.Time, time.Duration) (RuntimeConfigDirectoryOutbox, bool, error)
	CompleteRuntimeConfigDirectoryOutbox(context.Context, string, string, time.Time) (bool, error)
	RetryRuntimeConfigDirectoryOutbox(context.Context, string, string, time.Time, string) (bool, error)
}

// RuntimeConfigDirectoryStatus is the signed, metadata-only destination proof
// used by a durable source cursor. No entry body is returned by this status.
type RuntimeConfigDirectoryStatus struct {
	Version     int                               `json:"version"`
	Source      string                            `json:"source"`
	Destination string                            `json:"destination"`
	DeliveryID  string                            `json:"delivery_id"`
	Kind        RuntimeConfigDirectoryKind        `json:"kind"`
	EntryID     string                            `json:"entry_id"`
	Revision    int                               `json:"revision"`
	BodyDigest  string                            `json:"body_digest"`
	Phase       RuntimeConfigDirectoryStatusPhase `json:"phase"`
	Found       bool                              `json:"found"`
	ReceivedAt  time.Time                         `json:"received_at,omitempty"`
	Signature   string                            `json:"signature,omitempty"`
}

type RuntimeConfigDirectoryStatusPhase string

const (
	RuntimeConfigDirectoryStatusAbsent   RuntimeConfigDirectoryStatusPhase = "absent"
	RuntimeConfigDirectoryStatusAccepted RuntimeConfigDirectoryStatusPhase = "accepted"
)

var (
	ErrInvalidRuntimeConfigDirectoryStatus = errors.New("Runtime 配置目录 status 无效")
	ErrRuntimeConfigDirectoryStatusAuth    = errors.New("Runtime 配置目录 status 认证失败")
)

func (s RuntimeConfigDirectoryStatus) normalize() (RuntimeConfigDirectoryStatus, error) {
	s.Source = strings.TrimSpace(s.Source)
	s.Destination = strings.TrimSpace(s.Destination)
	s.DeliveryID = strings.TrimSpace(s.DeliveryID)
	s.Kind = RuntimeConfigDirectoryKind(strings.TrimSpace(strings.ToLower(string(s.Kind))))
	s.EntryID = strings.TrimSpace(s.EntryID)
	s.BodyDigest = strings.TrimSpace(strings.ToLower(s.BodyDigest))
	s.Signature = strings.TrimSpace(strings.ToLower(s.Signature))
	if s.Version != RuntimeConfigDirectoryEnvelopeVersion || s.Source == "" || s.Destination == "" || s.DeliveryID == "" || !s.Kind.valid() || s.EntryID == "" || s.Revision <= 0 || !runtimeConfigDirectoryDigestValid(s.BodyDigest) {
		return RuntimeConfigDirectoryStatus{}, fmt.Errorf("%w: status metadata 不完整", ErrInvalidRuntimeConfigDirectoryStatus)
	}
	if len(s.Source) > MaxRuntimeConfigDirectorySourceLength || len(s.Destination) > MaxRuntimeConfigDirectoryTargetLength || len(s.DeliveryID) > MaxRuntimeConfigDirectoryDeliveryID || len(s.EntryID) > MaxRuntimeConfigDirectoryIDLength {
		return RuntimeConfigDirectoryStatus{}, fmt.Errorf("%w: status metadata 超出长度限制", ErrInvalidRuntimeConfigDirectoryStatus)
	}
	if err := validateRuntimeConfigDirectoryID(s.EntryID); err != nil {
		return RuntimeConfigDirectoryStatus{}, fmt.Errorf("%w: %v", ErrInvalidRuntimeConfigDirectoryStatus, err)
	}
	switch s.Phase {
	case RuntimeConfigDirectoryStatusAbsent:
		if s.Found || !s.ReceivedAt.IsZero() {
			return RuntimeConfigDirectoryStatus{}, fmt.Errorf("%w: absent status 不能携带 found/received_at", ErrInvalidRuntimeConfigDirectoryStatus)
		}
	case RuntimeConfigDirectoryStatusAccepted:
		if !s.Found || s.ReceivedAt.IsZero() {
			return RuntimeConfigDirectoryStatus{}, fmt.Errorf("%w: accepted status 必须携带证明", ErrInvalidRuntimeConfigDirectoryStatus)
		}
	default:
		return RuntimeConfigDirectoryStatus{}, fmt.Errorf("%w: phase %q 不受支持", ErrInvalidRuntimeConfigDirectoryStatus, s.Phase)
	}
	if s.Signature != "" && !isRuntimeEventDeliveryDigest(s.Signature) {
		return RuntimeConfigDirectoryStatus{}, fmt.Errorf("%w: signature 无效", ErrInvalidRuntimeConfigDirectoryStatus)
	}
	if !s.ReceivedAt.IsZero() {
		s.ReceivedAt = s.ReceivedAt.UTC()
	}
	return s, nil
}

func (s RuntimeConfigDirectoryStatus) Normalize() (RuntimeConfigDirectoryStatus, error) {
	return s.normalize()
}

func NewRuntimeConfigDirectoryStatus(envelope RuntimeConfigDirectoryEnvelope, phase RuntimeConfigDirectoryStatusPhase, found bool, receivedAt time.Time) (RuntimeConfigDirectoryStatus, error) {
	normalized, err := envelope.Normalize()
	if err != nil {
		return RuntimeConfigDirectoryStatus{}, err
	}
	if phase == RuntimeConfigDirectoryStatusAbsent {
		found = false
		receivedAt = time.Time{}
	} else {
		found = true
	}
	return (RuntimeConfigDirectoryStatus{Version: normalized.Version, Source: normalized.Source, Destination: normalized.Destination, DeliveryID: normalized.DeliveryID, Kind: normalized.Kind, EntryID: normalized.EntryID, Revision: normalized.Revision, BodyDigest: normalized.BodyDigest, Phase: phase, Found: found, ReceivedAt: receivedAt}).normalize()
}

func (s RuntimeConfigDirectoryStatus) canonicalUnsigned() ([]byte, error) {
	normalized, err := s.normalize()
	if err != nil {
		return nil, err
	}
	normalized.Signature = ""
	return jsonMarshalRuntimeConfigDirectoryStatus(normalized)
}

func jsonMarshalRuntimeConfigDirectoryStatus(status RuntimeConfigDirectoryStatus) ([]byte, error) {
	// Kept in a tiny helper so status signing cannot accidentally marshal an
	// unnormalized caller-provided struct in a future extension.
	return json.Marshal(status)
}

func SignRuntimeConfigDirectoryStatus(status RuntimeConfigDirectoryStatus, secret []byte) (RuntimeConfigDirectoryStatus, error) {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return RuntimeConfigDirectoryStatus{}, err
	}
	canonical, err := status.canonicalUnsigned()
	if err != nil {
		return RuntimeConfigDirectoryStatus{}, err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	status.Signature = hex.EncodeToString(mac.Sum(nil))
	return status.normalize()
}

func VerifyRuntimeConfigDirectoryStatus(status RuntimeConfigDirectoryStatus, secret []byte) error {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return err
	}
	normalized, err := status.normalize()
	if err != nil {
		return err
	}
	if normalized.Signature == "" {
		return fmt.Errorf("%w: 缺少 signature", ErrRuntimeConfigDirectoryStatusAuth)
	}
	canonical, err := normalized.canonicalUnsigned()
	if err != nil {
		return err
	}
	provided, err := hex.DecodeString(normalized.Signature)
	if err != nil {
		return fmt.Errorf("%w: signature 编码无效", ErrRuntimeConfigDirectoryStatusAuth)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return fmt.Errorf("%w: HMAC 不匹配", ErrRuntimeConfigDirectoryStatusAuth)
	}
	return nil
}

func (s RuntimeConfigDirectoryStatus) ValidateAgainst(envelope RuntimeConfigDirectoryEnvelope) error {
	normalized, err := envelope.Normalize()
	if err != nil {
		return err
	}
	status, err := s.normalize()
	if err != nil {
		return err
	}
	if status.Version != normalized.Version || status.Source != normalized.Source || status.Destination != normalized.Destination || status.DeliveryID != normalized.DeliveryID || status.Kind != normalized.Kind || status.EntryID != normalized.EntryID || status.Revision != normalized.Revision || status.BodyDigest != normalized.BodyDigest {
		return fmt.Errorf("%w: status metadata 不一致", ErrRuntimeConfigDirectoryStatusAuth)
	}
	return nil
}
