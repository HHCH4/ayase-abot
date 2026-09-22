package runtime

// This file defines the metadata-only cross-Runtime configuration lock.  A
// lock carries the canonical RuntimeConfigSnapshot (which contains digests,
// identifiers and scalar policy only), never the source Runtime's secrets or
// user-authored text.  The destination may opt into local snapshot matching;
// without that explicit resolver the receiver remains a metadata-only inbox.

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

	"Abot/internal/agent"
)

const (
	RuntimeConfigDeliveryEnvelopeVersion       = 1
	RuntimeConfigDeliveryDecision              = "migrate"
	MaxRuntimeConfigDeliveryEnvelopeBytes      = 64 << 10
	MaxRuntimeConfigDeliveryProjectionBytes    = agent.MaxRuntimeConfigSnapshotBytes
	MaxRuntimeConfigDeliveryResponseBytes      = 96 << 10
	MaxRuntimeConfigDeliveryIDLength           = 220
	MaxRuntimeConfigDeliveryInvocationLength   = 512
	MaxRuntimeConfigDeliveryOwnerLength        = 160
	MaxRuntimeConfigDeliveryAttempts           = 8
	MaxRuntimeConfigDeliveryLease              = 10 * time.Minute
	DefaultRuntimeConfigDeliveryMaxAge         = 10 * time.Minute
	DefaultRuntimeConfigDeliveryFutureSkew     = 30 * time.Second
	DefaultRuntimeConfigDeliveryTransactionTTL = 10 * time.Minute
)

var (
	ErrInvalidRuntimeConfigDelivery = errors.New("Runtime 配置投递无效")
	ErrRuntimeConfigDeliveryAuth    = errors.New("Runtime 配置投递认证失败")
	ErrRuntimeConfigDeliveryStale   = errors.New("Runtime 配置投递已过期")
)

// RuntimeConfigDeliveryEnvelope binds one source-side migration to a target
// metadata snapshot.  Timestamp is the immutable migration time and IssuedAt
// is refreshed for every retry so a durable outbox can safely cross a replay
// window without changing the migration identity.
type RuntimeConfigDeliveryEnvelope struct {
	Version         int       `json:"version"`
	Source          string    `json:"source"`
	Destination     string    `json:"destination"`
	DeliveryID      string    `json:"delivery_id"`
	InvocationID    string    `json:"invocation_id"`
	ExpectedDigest  string    `json:"expected_digest"`
	SnapshotDigest  string    `json:"snapshot_digest"`
	SnapshotVersion int       `json:"snapshot_version"`
	Decision        string    `json:"decision"`
	IdempotencyKey  string    `json:"idempotency_key,omitempty"`
	Timestamp       time.Time `json:"timestamp"`
	IssuedAt        time.Time `json:"issued_at"`
	Signature       string    `json:"signature,omitempty"`
}

// RuntimeConfigDeliveryProjection is deliberately an alias: the source
// RuntimeConfigSnapshot is already a validated metadata-only projection.  The
// alias keeps storage and transport code provider-neutral while allowing a
// caller to use the agent package's canonical parser and digest implementation.
type RuntimeConfigDeliveryProjection = agent.RuntimeConfigSnapshot

// RuntimeConfigDeliveryOutbox is the source-side durable cursor.  Projection
// is hidden from generic API JSON but retained, bounded and immutable so a
// retry does not need to trust a mutable configuration directory.
type RuntimeConfigDeliveryOutbox struct {
	ID              string                            `json:"id"`
	GroupID         string                            `json:"group_id,omitempty"`
	InvocationID    string                            `json:"invocation_id"`
	Source          string                            `json:"source"`
	Destination     string                            `json:"destination"`
	DeliveryID      string                            `json:"delivery_id"`
	ExpectedDigest  string                            `json:"expected_digest"`
	SnapshotDigest  string                            `json:"snapshot_digest"`
	SnapshotVersion int                               `json:"snapshot_version"`
	IdempotencyKey  string                            `json:"idempotency_key,omitempty"`
	Projection      RuntimeConfigDeliveryProjection   `json:"-"`
	Status          RuntimeConfigDeliveryOutboxStatus `json:"status"`
	Attempt         int                               `json:"attempt"`
	Revision        int64                             `json:"revision"`
	AvailableAt     time.Time                         `json:"available_at"`
	LeaseOwner      string                            `json:"-"`
	LeaseExpiresAt  *time.Time                        `json:"-"`
	LastError       string                            `json:"last_error,omitempty"`
	CreatedAt       time.Time                         `json:"created_at"`
	UpdatedAt       time.Time                         `json:"updated_at"`
}

type RuntimeConfigDeliveryOutboxStatus string

const (
	RuntimeConfigDeliveryOutboxQueued     RuntimeConfigDeliveryOutboxStatus = "queued"
	RuntimeConfigDeliveryOutboxProcessing RuntimeConfigDeliveryOutboxStatus = "processing"
	RuntimeConfigDeliveryOutboxCompleted  RuntimeConfigDeliveryOutboxStatus = "completed"
	RuntimeConfigDeliveryOutboxFailed     RuntimeConfigDeliveryOutboxStatus = "failed"
)

func (s RuntimeConfigDeliveryOutboxStatus) terminal() bool {
	return s == RuntimeConfigDeliveryOutboxCompleted || s == RuntimeConfigDeliveryOutboxFailed
}

func runtimeConfigSnapshotDigestValid(value string) bool {
	value = strings.TrimSpace(strings.ToLower(value))
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func RuntimeConfigDeliveryID(source, destination, invocationID, expectedDigest, snapshotDigest, idempotencyKey string) string {
	material := strings.Join([]string{
		strings.TrimSpace(source), strings.TrimSpace(destination), strings.TrimSpace(invocationID),
		strings.TrimSpace(expectedDigest), strings.TrimSpace(snapshotDigest), strings.TrimSpace(idempotencyKey), RuntimeConfigDeliveryDecision,
	}, "\x00")
	sum := sha256.Sum256([]byte(material))
	return "runtime-config:" + hex.EncodeToString(sum[:])
}

func RuntimeConfigDeliveryOutboxID(source, destination, deliveryID string) string {
	material := strings.TrimSpace(source) + "\x00" + strings.TrimSpace(destination) + "\x00" + strings.TrimSpace(deliveryID)
	sum := sha256.Sum256([]byte(material))
	return "runtime-config-outbox-" + hex.EncodeToString(sum[:16])
}

func cloneRuntimeConfigDeliveryProjection(projection RuntimeConfigDeliveryProjection) RuntimeConfigDeliveryProjection {
	return projection
}

func cloneRuntimeConfigDeliveryOutbox(item RuntimeConfigDeliveryOutbox) RuntimeConfigDeliveryOutbox {
	item.Projection = cloneRuntimeConfigDeliveryProjection(item.Projection)
	if item.LeaseExpiresAt != nil {
		value := item.LeaseExpiresAt.UTC()
		item.LeaseExpiresAt = &value
	}
	return item
}

func (o RuntimeConfigDeliveryOutbox) normalize(now time.Time) (RuntimeConfigDeliveryOutbox, error) {
	o.ID = strings.TrimSpace(o.ID)
	o.GroupID = strings.TrimSpace(o.GroupID)
	o.InvocationID = strings.TrimSpace(o.InvocationID)
	o.Source = strings.TrimSpace(o.Source)
	o.Destination = strings.TrimSpace(o.Destination)
	o.DeliveryID = strings.TrimSpace(o.DeliveryID)
	o.ExpectedDigest = strings.TrimSpace(strings.ToLower(o.ExpectedDigest))
	o.SnapshotDigest = strings.TrimSpace(strings.ToLower(o.SnapshotDigest))
	o.IdempotencyKey = strings.TrimSpace(o.IdempotencyKey)
	o.LeaseOwner = strings.TrimSpace(o.LeaseOwner)
	o.LastError = SanitizeRuntimeString(strings.TrimSpace(o.LastError))
	if o.ID == "" || o.InvocationID == "" || o.Source == "" || o.Destination == "" || o.DeliveryID == "" || o.SnapshotVersion <= 0 {
		return RuntimeConfigDeliveryOutbox{}, fmt.Errorf("%w: outbox metadata 不完整", ErrInvalidRuntimeConfigDelivery)
	}
	if len(o.ID) > MaxRuntimeConfigDeliveryIDLength || len(o.GroupID) > MaxRuntimeDeliveryGroupIDLength || len(o.InvocationID) > MaxRuntimeConfigDeliveryInvocationLength || len(o.Source) > MaxRuntimeEventDeliverySourceLength || len(o.Destination) > MaxRuntimeEventDeliveryTargetLength || len(o.DeliveryID) > MaxRuntimeConfigDeliveryIDLength || len(o.IdempotencyKey) > maxRuntimeConfigMigrationIdempotencyKeyLength {
		return RuntimeConfigDeliveryOutbox{}, fmt.Errorf("%w: outbox metadata 超出长度限制", ErrInvalidRuntimeConfigDelivery)
	}
	if !runtimeConfigSnapshotDigestValid(o.ExpectedDigest) || !runtimeConfigSnapshotDigestValid(o.SnapshotDigest) {
		return RuntimeConfigDeliveryOutbox{}, fmt.Errorf("%w: digest 无效", ErrInvalidRuntimeConfigDelivery)
	}
	if o.ExpectedDigest == o.SnapshotDigest {
		return RuntimeConfigDeliveryOutbox{}, fmt.Errorf("%w: 迁移前后 digest 不能相同", ErrInvalidRuntimeConfigDelivery)
	}
	if o.DeliveryID != RuntimeConfigDeliveryID(o.Source, o.Destination, o.InvocationID, o.ExpectedDigest, o.SnapshotDigest, o.IdempotencyKey) || o.ID != RuntimeConfigDeliveryOutboxID(o.Source, o.Destination, o.DeliveryID) {
		return RuntimeConfigDeliveryOutbox{}, fmt.Errorf("%w: outbox identity 不一致", ErrRuntimeConfigDeliveryAuth)
	}
	if len(o.LeaseOwner) > MaxRuntimeConfigDeliveryOwnerLength || o.Attempt < 0 || o.Attempt > MaxRuntimeConfigDeliveryAttempts {
		return RuntimeConfigDeliveryOutbox{}, fmt.Errorf("%w: outbox lease/attempt 无效", ErrInvalidRuntimeConfigDelivery)
	}
	projection, err := agent.ParseRuntimeConfigSnapshot(mustMarshalRuntimeConfigProjection(o.Projection))
	if err != nil {
		return RuntimeConfigDeliveryOutbox{}, err
	}
	projectionDigest := agent.RuntimeConfigSnapshotDigest(mustMarshalRuntimeConfigProjection(projection))
	if projectionDigest != o.SnapshotDigest {
		return RuntimeConfigDeliveryOutbox{}, fmt.Errorf("%w: projection digest 不一致", ErrRuntimeConfigDeliveryAuth)
	}
	o.Projection = projection
	if o.Status == "" {
		o.Status = RuntimeConfigDeliveryOutboxQueued
	}
	switch o.Status {
	case RuntimeConfigDeliveryOutboxQueued, RuntimeConfigDeliveryOutboxProcessing, RuntimeConfigDeliveryOutboxCompleted, RuntimeConfigDeliveryOutboxFailed:
	default:
		return RuntimeConfigDeliveryOutbox{}, fmt.Errorf("%w: outbox status 不受支持", ErrInvalidRuntimeConfigDelivery)
	}
	if o.Status == RuntimeConfigDeliveryOutboxProcessing && (o.LeaseOwner == "" || o.LeaseExpiresAt == nil) {
		return RuntimeConfigDeliveryOutbox{}, fmt.Errorf("%w: processing outbox 必须带租约", ErrInvalidRuntimeConfigDelivery)
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
	if o.Status.terminal() || o.Status == RuntimeConfigDeliveryOutboxQueued {
		o.LeaseOwner = ""
		o.LeaseExpiresAt = nil
	}
	if o.Status == RuntimeConfigDeliveryOutboxProcessing {
		if o.LeaseExpiresAt == nil || o.LeaseExpiresAt.IsZero() || o.LeaseExpiresAt.After(now.Add(MaxRuntimeConfigDeliveryLease)) {
			return RuntimeConfigDeliveryOutbox{}, fmt.Errorf("%w: outbox lease 无效", ErrInvalidRuntimeConfigDelivery)
		}
		expires := o.LeaseExpiresAt.UTC()
		o.LeaseExpiresAt = &expires
	}
	return o, nil
}

func (o RuntimeConfigDeliveryOutbox) Normalize(now time.Time) (RuntimeConfigDeliveryOutbox, error) {
	return o.normalize(now)
}

func mustMarshalRuntimeConfigProjection(projection RuntimeConfigDeliveryProjection) string {
	encoded, _ := json.Marshal(projection)
	return string(encoded)
}

func RuntimeConfigDeliveryOutboxBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

// NewRuntimeConfigDeliveryOutbox creates the immutable source cursor for a
// committed local migration.  It validates that the projection digest is the
// target digest before any storage adapter can persist it.
func NewRuntimeConfigDeliveryOutbox(source, destination, invocationID, expectedDigest, idempotencyKey string, projection RuntimeConfigDeliveryProjection, now time.Time) (RuntimeConfigDeliveryOutbox, error) {
	encoded := mustMarshalRuntimeConfigProjection(projection)
	snapshot, err := agent.ParseRuntimeConfigSnapshot(encoded)
	if err != nil {
		return RuntimeConfigDeliveryOutbox{}, err
	}
	targetDigest := agent.RuntimeConfigSnapshotDigest(encoded)
	if targetDigest == "" {
		return RuntimeConfigDeliveryOutbox{}, fmt.Errorf("%w: projection digest 无效", ErrInvalidRuntimeConfigDelivery)
	}
	source = strings.TrimSpace(source)
	destination = strings.TrimSpace(destination)
	invocationID = strings.TrimSpace(invocationID)
	expectedDigest = strings.TrimSpace(strings.ToLower(expectedDigest))
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	item := RuntimeConfigDeliveryOutbox{
		ID:           RuntimeConfigDeliveryOutboxID(source, destination, RuntimeConfigDeliveryID(source, destination, invocationID, expectedDigest, targetDigest, idempotencyKey)),
		InvocationID: invocationID, Source: source, Destination: destination,
		DeliveryID:     RuntimeConfigDeliveryID(source, destination, invocationID, expectedDigest, targetDigest, idempotencyKey),
		ExpectedDigest: expectedDigest, SnapshotDigest: targetDigest, SnapshotVersion: snapshot.Version,
		IdempotencyKey: idempotencyKey, Projection: snapshot, Status: RuntimeConfigDeliveryOutboxQueued,
		AvailableAt: now, CreatedAt: now, UpdatedAt: now,
	}
	return item.normalize(now)
}

// Envelope creates an unsigned fresh-attempt identity.  The transport signs
// it immediately before sending, so a queued item does not expire while idle.
func (o RuntimeConfigDeliveryOutbox) Envelope(now time.Time) (RuntimeConfigDeliveryEnvelope, error) {
	normalized, err := o.normalize(now)
	if err != nil {
		return RuntimeConfigDeliveryEnvelope{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	return (RuntimeConfigDeliveryEnvelope{
		Version: RuntimeConfigDeliveryEnvelopeVersion, Source: normalized.Source, Destination: normalized.Destination,
		DeliveryID: normalized.DeliveryID, InvocationID: normalized.InvocationID, ExpectedDigest: normalized.ExpectedDigest,
		SnapshotDigest: normalized.SnapshotDigest, SnapshotVersion: normalized.SnapshotVersion, Decision: RuntimeConfigDeliveryDecision,
		IdempotencyKey: normalized.IdempotencyKey, Timestamp: normalized.CreatedAt.UTC(), IssuedAt: now,
	}).normalize()
}

func (o RuntimeConfigDeliveryOutbox) Matches(other RuntimeConfigDeliveryOutbox) bool {
	left, leftErr := o.normalize(time.Time{})
	right, rightErr := other.normalize(time.Time{})
	if leftErr != nil || rightErr != nil {
		return false
	}
	return left.ID == right.ID && left.InvocationID == right.InvocationID && left.Source == right.Source && left.Destination == right.Destination && left.DeliveryID == right.DeliveryID && left.ExpectedDigest == right.ExpectedDigest && left.SnapshotDigest == right.SnapshotDigest && left.SnapshotVersion == right.SnapshotVersion && left.IdempotencyKey == right.IdempotencyKey && agent.RuntimeConfigSnapshotDigest(mustMarshalRuntimeConfigProjection(left.Projection)) == agent.RuntimeConfigSnapshotDigest(mustMarshalRuntimeConfigProjection(right.Projection))
}

func (e RuntimeConfigDeliveryEnvelope) normalize() (RuntimeConfigDeliveryEnvelope, error) {
	e.Source = strings.TrimSpace(e.Source)
	e.Destination = strings.TrimSpace(e.Destination)
	e.DeliveryID = strings.TrimSpace(e.DeliveryID)
	e.InvocationID = strings.TrimSpace(e.InvocationID)
	e.ExpectedDigest = strings.TrimSpace(strings.ToLower(e.ExpectedDigest))
	e.SnapshotDigest = strings.TrimSpace(strings.ToLower(e.SnapshotDigest))
	e.Decision = strings.TrimSpace(strings.ToLower(e.Decision))
	e.IdempotencyKey = strings.TrimSpace(e.IdempotencyKey)
	e.Signature = strings.TrimSpace(strings.ToLower(e.Signature))
	if e.Version != RuntimeConfigDeliveryEnvelopeVersion {
		return RuntimeConfigDeliveryEnvelope{}, fmt.Errorf("%w: version %d 不受支持", ErrInvalidRuntimeConfigDelivery, e.Version)
	}
	if e.Source == "" || e.Destination == "" || e.DeliveryID == "" || e.InvocationID == "" || e.ExpectedDigest == "" || e.SnapshotDigest == "" || e.SnapshotVersion <= 0 || e.Decision != RuntimeConfigDeliveryDecision || e.Timestamp.IsZero() || e.IssuedAt.IsZero() {
		return RuntimeConfigDeliveryEnvelope{}, fmt.Errorf("%w: 必填 metadata 不完整", ErrInvalidRuntimeConfigDelivery)
	}
	if len(e.Source) > MaxRuntimeEventDeliverySourceLength || len(e.Destination) > MaxRuntimeEventDeliveryTargetLength || len(e.DeliveryID) > MaxRuntimeConfigDeliveryIDLength || len(e.InvocationID) > MaxRuntimeConfigDeliveryInvocationLength || len(e.IdempotencyKey) > maxRuntimeConfigMigrationIdempotencyKeyLength {
		return RuntimeConfigDeliveryEnvelope{}, fmt.Errorf("%w: metadata 超出长度限制", ErrInvalidRuntimeConfigDelivery)
	}
	if !runtimeConfigSnapshotDigestValid(e.ExpectedDigest) || !runtimeConfigSnapshotDigestValid(e.SnapshotDigest) || e.ExpectedDigest == e.SnapshotDigest {
		return RuntimeConfigDeliveryEnvelope{}, fmt.Errorf("%w: digest 无效", ErrInvalidRuntimeConfigDelivery)
	}
	if e.DeliveryID != RuntimeConfigDeliveryID(e.Source, e.Destination, e.InvocationID, e.ExpectedDigest, e.SnapshotDigest, e.IdempotencyKey) {
		return RuntimeConfigDeliveryEnvelope{}, fmt.Errorf("%w: delivery_id 不匹配 metadata", ErrRuntimeConfigDeliveryAuth)
	}
	if e.Signature != "" && !isRuntimeEventDeliveryDigest(e.Signature) {
		return RuntimeConfigDeliveryEnvelope{}, fmt.Errorf("%w: signature 无效", ErrInvalidRuntimeConfigDelivery)
	}
	e.Timestamp = e.Timestamp.UTC()
	e.IssuedAt = e.IssuedAt.UTC()
	return e, nil
}

func (e RuntimeConfigDeliveryEnvelope) Validate() error {
	_, err := e.normalize()
	return err
}

func NormalizeRuntimeConfigDeliveryEnvelope(e RuntimeConfigDeliveryEnvelope) (RuntimeConfigDeliveryEnvelope, error) {
	return e.normalize()
}

func (e RuntimeConfigDeliveryEnvelope) canonicalUnsigned() ([]byte, error) {
	normalized, err := e.normalize()
	if err != nil {
		return nil, err
	}
	normalized.Signature = ""
	return json.Marshal(normalized)
}

func SignRuntimeConfigDeliveryEnvelope(e RuntimeConfigDeliveryEnvelope, secret []byte) (RuntimeConfigDeliveryEnvelope, error) {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return RuntimeConfigDeliveryEnvelope{}, err
	}
	canonical, err := e.canonicalUnsigned()
	if err != nil {
		return RuntimeConfigDeliveryEnvelope{}, err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	e.Signature = hex.EncodeToString(mac.Sum(nil))
	return e.normalize()
}

func VerifyRuntimeConfigDeliveryEnvelope(e RuntimeConfigDeliveryEnvelope, secret []byte) error {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return err
	}
	normalized, err := e.normalize()
	if err != nil {
		return err
	}
	if normalized.Signature == "" {
		return fmt.Errorf("%w: 缺少 signature", ErrRuntimeConfigDeliveryAuth)
	}
	canonical, err := normalized.canonicalUnsigned()
	if err != nil {
		return err
	}
	provided, err := hex.DecodeString(normalized.Signature)
	if err != nil {
		return fmt.Errorf("%w: signature 编码无效", ErrRuntimeConfigDeliveryAuth)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return fmt.Errorf("%w: HMAC 不匹配", ErrRuntimeConfigDeliveryAuth)
	}
	return nil
}

func validateRuntimeConfigDeliveryTimestamp(timestamp, now time.Time, maxAge, futureSkew time.Duration) error {
	if maxAge <= 0 || futureSkew < 0 {
		return fmt.Errorf("%w: timestamp policy 无效", ErrInvalidRuntimeConfigDelivery)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	timestamp = timestamp.UTC()
	if timestamp.After(now.Add(futureSkew)) {
		return fmt.Errorf("%w: timestamp 位于允许未来窗口之外", ErrRuntimeConfigDeliveryAuth)
	}
	if now.Sub(timestamp) > maxAge {
		return fmt.Errorf("%w: timestamp 已过期", ErrRuntimeConfigDeliveryStale)
	}
	return nil
}

// RuntimeConfigDeliveryInboxRecord is the metadata retained at the
// destination. Projection is safe to persist because RuntimeConfigSnapshot
// contains no secret-bearing fields or free-form prompt text.
type RuntimeConfigDeliveryInboxRecord struct {
	Version         int                             `json:"version"`
	DeliveryID      string                          `json:"delivery_id"`
	Source          string                          `json:"source"`
	Destination     string                          `json:"destination"`
	InvocationID    string                          `json:"invocation_id"`
	ExpectedDigest  string                          `json:"expected_digest"`
	SnapshotDigest  string                          `json:"snapshot_digest"`
	SnapshotVersion int                             `json:"snapshot_version"`
	IdempotencyKey  string                          `json:"idempotency_key,omitempty"`
	Projection      RuntimeConfigDeliveryProjection `json:"projection"`
	Signature       string                          `json:"signature"`
	ReceivedAt      time.Time                       `json:"received_at"`
}

func RuntimeConfigDeliveryInboxRecordFromEnvelope(e RuntimeConfigDeliveryEnvelope, projection RuntimeConfigDeliveryProjection, receivedAt time.Time) RuntimeConfigDeliveryInboxRecord {
	return RuntimeConfigDeliveryInboxRecord{Version: e.Version, DeliveryID: e.DeliveryID, Source: e.Source, Destination: e.Destination, InvocationID: e.InvocationID, ExpectedDigest: e.ExpectedDigest, SnapshotDigest: e.SnapshotDigest, SnapshotVersion: e.SnapshotVersion, IdempotencyKey: e.IdempotencyKey, Projection: cloneRuntimeConfigDeliveryProjection(projection), Signature: e.Signature, ReceivedAt: receivedAt.UTC()}
}

func (r RuntimeConfigDeliveryInboxRecord) Matches(e RuntimeConfigDeliveryEnvelope, projection RuntimeConfigDeliveryProjection) bool {
	// Signature and IssuedAt are attempt-scoped. A retry is expected to carry a
	// fresh HMAC while retaining the immutable delivery identity, so duplicate
	// detection must never compare the old signature.
	if r.Version != e.Version || r.DeliveryID != e.DeliveryID || r.Source != e.Source || r.Destination != e.Destination || r.InvocationID != e.InvocationID || r.ExpectedDigest != e.ExpectedDigest || r.SnapshotDigest != e.SnapshotDigest || r.SnapshotVersion != e.SnapshotVersion || r.IdempotencyKey != e.IdempotencyKey {
		return false
	}
	return agent.RuntimeConfigSnapshotDigest(mustMarshalRuntimeConfigProjection(r.Projection)) == agent.RuntimeConfigSnapshotDigest(mustMarshalRuntimeConfigProjection(projection))
}

// MatchesIdentity compares immutable delivery metadata while ignoring the
// attempt-scoped signature. The projection digest is already carried by the
// envelope and can be checked separately when the source has the projection.
func (r RuntimeConfigDeliveryInboxRecord) MatchesIdentity(e RuntimeConfigDeliveryEnvelope) bool {
	return r.Version == e.Version && r.DeliveryID == e.DeliveryID && r.Source == e.Source && r.Destination == e.Destination && r.InvocationID == e.InvocationID && r.ExpectedDigest == e.ExpectedDigest && r.SnapshotDigest == e.SnapshotDigest && r.SnapshotVersion == e.SnapshotVersion && r.IdempotencyKey == e.IdempotencyKey
}

// RuntimeConfigDeliveryInbox is the destination-side idempotency boundary.
type RuntimeConfigDeliveryInbox interface {
	AcceptRuntimeConfigDelivery(context.Context, RuntimeConfigDeliveryEnvelope, RuntimeConfigDeliveryProjection) (duplicate bool, err error)
	GetRuntimeConfigDelivery(context.Context, string) (RuntimeConfigDeliveryInboxRecord, error)
}

// RuntimeConfigDeliveryReconciler is an optional source transport capability
// for querying the destination lock without resending its snapshot.
type RuntimeConfigDeliveryReconciler interface {
	ReconcileConfigDelivery(context.Context, RuntimeConfigDeliveryEnvelope, RuntimeConfigDeliveryProjection) (RuntimeDeliveryStatus, error)
}

// RuntimeConfigDeliveryTransactionRepository is the optional durable
// prepare/commit ledger. Commit publishes the projection to the destination
// inbox in the same local transaction.
type RuntimeConfigDeliveryTransactionRepository interface {
	PrepareRuntimeConfigDelivery(context.Context, RuntimeConfigDeliveryEnvelope, RuntimeConfigDeliveryProjection) (duplicate bool, err error)
	CommitRuntimeConfigDelivery(context.Context, RuntimeConfigDeliveryEnvelope) (duplicate bool, err error)
}

// RuntimeConfigDeliveryAbortableTransactionRepository is the optional
// destination-side compensation boundary. Abort is metadata-only and never
// applies a configuration snapshot to the destination inbox.
type RuntimeConfigDeliveryAbortableTransactionRepository interface {
	AbortRuntimeConfigDelivery(context.Context, RuntimeConfigDeliveryEnvelope) (duplicate bool, err error)
}

type RuntimeConfigDeliveryTransport interface {
	Deliver(context.Context, RuntimeConfigDeliveryEnvelope, RuntimeConfigDeliveryProjection) (RuntimeConfigDeliveryReceipt, error)
}

type RuntimeConfigDeliveryTransactionalTransport interface {
	Prepare(context.Context, RuntimeConfigDeliveryEnvelope, RuntimeConfigDeliveryProjection) (RuntimeConfigDeliveryReceipt, error)
	Commit(context.Context, RuntimeConfigDeliveryEnvelope, RuntimeConfigDeliveryProjection) (RuntimeConfigDeliveryReceipt, error)
}

// RuntimeConfigDeliveryAbortableTransport is an optional compensation
// capability for transactional configuration transports.
type RuntimeConfigDeliveryAbortableTransport interface {
	Abort(context.Context, RuntimeConfigDeliveryEnvelope, RuntimeConfigDeliveryProjection) (RuntimeConfigDeliveryReceipt, error)
}

type RuntimeConfigDeliveryTransactionalOptIn interface {
	RuntimeConfigDeliveryTransactionsEnabled() bool
}

type RuntimeConfigDeliveryReceipt struct {
	Version         int    `json:"version"`
	DeliveryID      string `json:"delivery_id"`
	InvocationID    string `json:"invocation_id"`
	ExpectedDigest  string `json:"expected_digest"`
	SnapshotDigest  string `json:"snapshot_digest"`
	SnapshotVersion int    `json:"snapshot_version"`
	Phase           string `json:"phase,omitempty"`
	Duplicate       bool   `json:"duplicate"`
}

const (
	RuntimeConfigDeliveryTransactionPhasePrepared  = "prepared"
	RuntimeConfigDeliveryTransactionPhaseCommitted = "committed"
	RuntimeConfigDeliveryTransactionPhaseAborted   = "aborted"
)

func (r RuntimeConfigDeliveryReceipt) ValidateAgainst(e RuntimeConfigDeliveryEnvelope) error {
	normalized, err := NormalizeRuntimeConfigDeliveryEnvelope(e)
	if err != nil {
		return err
	}
	if r.Version != normalized.Version || strings.TrimSpace(r.DeliveryID) != normalized.DeliveryID || strings.TrimSpace(r.InvocationID) != normalized.InvocationID || strings.TrimSpace(strings.ToLower(r.ExpectedDigest)) != normalized.ExpectedDigest || strings.TrimSpace(strings.ToLower(r.SnapshotDigest)) != normalized.SnapshotDigest || r.SnapshotVersion != normalized.SnapshotVersion {
		return fmt.Errorf("%w: receipt metadata 不一致", ErrRuntimeConfigDeliveryAuth)
	}
	return nil
}

func (r RuntimeConfigDeliveryReceipt) ValidateAgainstPhase(e RuntimeConfigDeliveryEnvelope, phase string) error {
	if err := r.ValidateAgainst(e); err != nil {
		return err
	}
	if strings.TrimSpace(r.Phase) != strings.TrimSpace(phase) {
		return fmt.Errorf("%w: receipt phase 不一致", ErrRuntimeConfigDeliveryAuth)
	}
	return nil
}

// RuntimeConfigDeliverySnapshotResolver is an explicit destination-side
// local lock check. It must return the destination's canonical metadata-only
// snapshot for the same invocation. The receiver never executes a tool or
// starts a Worker while resolving it.
type RuntimeConfigDeliverySnapshotResolver func(context.Context, string) (RuntimeConfigDeliveryProjection, error)

// RuntimeConfigDeliveryReceiver verifies a signed projection and accepts it
// through the destination inbox. LocalSnapshotResolver is optional for a
// metadata-only receiver; RequireLocalSnapshot makes absence of that resolver
// fail closed when a deployment needs an actual version lock.
type RuntimeConfigDeliveryReceiver struct {
	Source                string
	Destination           string
	SharedSecret          []byte
	Inbox                 RuntimeConfigDeliveryInbox
	LocalSnapshotResolver RuntimeConfigDeliverySnapshotResolver
	RequireLocalSnapshot  bool
	MaxAge                time.Duration
	MaxFutureSkew         time.Duration
	MaxBodyBytes          int64
	Now                   func() time.Time
}

func NewRuntimeConfigDeliveryReceiver(source, destination string, secret []byte, inbox RuntimeConfigDeliveryInbox) (*RuntimeConfigDeliveryReceiver, error) {
	if strings.TrimSpace(source) == "" || strings.TrimSpace(destination) == "" {
		return nil, fmt.Errorf("%w: source/destination 无效", ErrInvalidRuntimeConfigDelivery)
	}
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return nil, err
	}
	if inbox == nil {
		return nil, fmt.Errorf("%w: inbox 不能为空", ErrInvalidRuntimeConfigDelivery)
	}
	return &RuntimeConfigDeliveryReceiver{Source: strings.TrimSpace(source), Destination: strings.TrimSpace(destination), SharedSecret: append([]byte(nil), secret...), Inbox: inbox, MaxAge: DefaultRuntimeConfigDeliveryMaxAge, MaxFutureSkew: DefaultRuntimeConfigDeliveryFutureSkew, MaxBodyBytes: MaxRuntimeConfigDeliveryResponseBytes, Now: time.Now}, nil
}

func (r *RuntimeConfigDeliveryReceiver) Handler() http.Handler { return r }

type runtimeConfigDeliveryPayload struct {
	Envelope   RuntimeConfigDeliveryEnvelope   `json:"envelope"`
	Projection RuntimeConfigDeliveryProjection `json:"projection"`
}

func readRuntimeConfigDeliveryPayload(request *http.Request, configuredMax int64) (runtimeConfigDeliveryPayload, error) {
	if request == nil || request.Body == nil {
		return runtimeConfigDeliveryPayload{}, fmt.Errorf("%w: 请求 body 为空", ErrInvalidRuntimeConfigDelivery)
	}
	maxBody := configuredMax
	if maxBody <= 0 || maxBody > MaxRuntimeConfigDeliveryResponseBytes {
		maxBody = MaxRuntimeConfigDeliveryResponseBytes
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBody+1))
	if err != nil || int64(len(body)) > maxBody {
		return runtimeConfigDeliveryPayload{}, fmt.Errorf("%w: 请求 body 超过上限或读取失败", ErrInvalidRuntimeConfigDelivery)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var payload runtimeConfigDeliveryPayload
	if err := decoder.Decode(&payload); err != nil {
		return runtimeConfigDeliveryPayload{}, fmt.Errorf("%w: JSON 无效", ErrInvalidRuntimeConfigDelivery)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return runtimeConfigDeliveryPayload{}, fmt.Errorf("%w: 请求包含多个 JSON 文档", ErrInvalidRuntimeConfigDelivery)
	}
	return payload, nil
}

func (r *RuntimeConfigDeliveryReceiver) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if r == nil {
		writeRuntimeConfigDeliveryError(writer, http.StatusInternalServerError, ErrInvalidRuntimeConfigDelivery)
		return
	}
	if request != nil && request.URL != nil && strings.HasSuffix(strings.TrimRight(request.URL.Path, "/"), "/status") {
		r.ServeStatusHTTP(writer, request)
		return
	}
	if request.Method != http.MethodPost {
		writeRuntimeConfigDeliveryError(writer, http.StatusMethodNotAllowed, fmt.Errorf("%w: 只支持 POST", ErrInvalidRuntimeConfigDelivery))
		return
	}
	payload, err := readRuntimeConfigDeliveryPayload(request, r.MaxBodyBytes)
	if err != nil {
		writeRuntimeConfigDeliveryError(writer, http.StatusBadRequest, err)
		return
	}
	envelope, err := NormalizeRuntimeConfigDeliveryEnvelope(payload.Envelope)
	if err != nil {
		writeRuntimeConfigDeliveryError(writer, http.StatusBadRequest, err)
		return
	}
	if envelope.Source != r.Source || envelope.Destination != r.Destination {
		writeRuntimeConfigDeliveryError(writer, http.StatusForbidden, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeConfigDeliveryAuth))
		return
	}
	if err := VerifyRuntimeConfigDeliveryEnvelope(envelope, r.SharedSecret); err != nil {
		writeRuntimeConfigDeliveryError(writer, http.StatusUnauthorized, err)
		return
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	if err := validateRuntimeConfigDeliveryTimestamp(envelope.IssuedAt, now, r.MaxAge, r.MaxFutureSkew); err != nil {
		writeRuntimeConfigDeliveryError(writer, http.StatusUnauthorized, err)
		return
	}
	projection, err := agent.ParseRuntimeConfigSnapshot(mustMarshalRuntimeConfigProjection(payload.Projection))
	if err != nil {
		writeRuntimeConfigDeliveryError(writer, http.StatusBadRequest, err)
		return
	}
	if projection.Version != envelope.SnapshotVersion || agent.RuntimeConfigSnapshotDigest(mustMarshalRuntimeConfigProjection(projection)) != envelope.SnapshotDigest {
		writeRuntimeConfigDeliveryError(writer, http.StatusConflict, fmt.Errorf("%w: projection digest/version 不一致", ErrRuntimeConfigDeliveryAuth))
		return
	}
	if r.RequireLocalSnapshot && r.LocalSnapshotResolver == nil {
		writeRuntimeConfigDeliveryError(writer, http.StatusConflict, fmt.Errorf("%w: 未配置本地快照解析器", ErrRuntimeConfigDeliveryAuth))
		return
	}
	if r.LocalSnapshotResolver != nil {
		local, resolveErr := r.LocalSnapshotResolver(request.Context(), envelope.InvocationID)
		if resolveErr != nil {
			writeRuntimeConfigDeliveryError(writer, http.StatusConflict, fmt.Errorf("%w: 本地配置版本不可用", ErrRuntimeConfigDeliveryAuth))
			return
		}
		localProjection, normalizeErr := normalizeRuntimeConfigDeliveryProjection(local)
		if normalizeErr != nil || agentRuntimeConfigSnapshotDigest(localProjection) != envelope.SnapshotDigest {
			writeRuntimeConfigDeliveryError(writer, http.StatusConflict, fmt.Errorf("%w: 本地配置版本不匹配", ErrRuntimeConfigDeliveryAuth))
			return
		}
	}
	duplicate, err := r.Inbox.AcceptRuntimeConfigDelivery(request.Context(), envelope, projection)
	if err != nil {
		writeRuntimeConfigDeliveryError(writer, http.StatusConflict, err)
		return
	}
	receipt := RuntimeConfigDeliveryReceipt{Version: envelope.Version, DeliveryID: envelope.DeliveryID, InvocationID: envelope.InvocationID, ExpectedDigest: envelope.ExpectedDigest, SnapshotDigest: envelope.SnapshotDigest, SnapshotVersion: envelope.SnapshotVersion, Duplicate: duplicate}
	writeRuntimeConfigDeliveryJSON(writer, http.StatusOK, receipt)
}

// validateRuntimeConfigDeliveryRequest performs the common authentication,
// replay, projection and optional local-version checks used by one-phase and
// two-phase handlers. It deliberately returns only the bounded projection.
func (r *RuntimeConfigDeliveryReceiver) validateRuntimeConfigDeliveryRequest(request *http.Request) (RuntimeConfigDeliveryEnvelope, RuntimeConfigDeliveryProjection, error) {
	return r.validateRuntimeConfigDeliveryRequestWithLocalSnapshot(request, true)
}

// validateRuntimeConfigDeliveryRequestWithLocalSnapshot keeps authentication
// and projection checks identical across phases. Compensation deliberately
// skips the optional local snapshot lock: a destination changing versions
// after prepare must still be able to release the stale remote lock.
func (r *RuntimeConfigDeliveryReceiver) validateRuntimeConfigDeliveryRequestWithLocalSnapshot(request *http.Request, checkLocalSnapshot bool) (RuntimeConfigDeliveryEnvelope, RuntimeConfigDeliveryProjection, error) {
	payload, err := readRuntimeConfigDeliveryPayload(request, r.MaxBodyBytes)
	if err != nil {
		return RuntimeConfigDeliveryEnvelope{}, RuntimeConfigDeliveryProjection{}, err
	}
	envelope, err := NormalizeRuntimeConfigDeliveryEnvelope(payload.Envelope)
	if err != nil {
		return RuntimeConfigDeliveryEnvelope{}, RuntimeConfigDeliveryProjection{}, err
	}
	if envelope.Source != r.Source || envelope.Destination != r.Destination {
		return RuntimeConfigDeliveryEnvelope{}, RuntimeConfigDeliveryProjection{}, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeConfigDeliveryAuth)
	}
	if err := VerifyRuntimeConfigDeliveryEnvelope(envelope, r.SharedSecret); err != nil {
		return RuntimeConfigDeliveryEnvelope{}, RuntimeConfigDeliveryProjection{}, err
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	if err := validateRuntimeConfigDeliveryTimestamp(envelope.IssuedAt, now, r.MaxAge, r.MaxFutureSkew); err != nil {
		return RuntimeConfigDeliveryEnvelope{}, RuntimeConfigDeliveryProjection{}, err
	}
	projection, err := agent.ParseRuntimeConfigSnapshot(mustMarshalRuntimeConfigProjection(payload.Projection))
	if err != nil {
		return RuntimeConfigDeliveryEnvelope{}, RuntimeConfigDeliveryProjection{}, err
	}
	if projection.Version != envelope.SnapshotVersion || agent.RuntimeConfigSnapshotDigest(mustMarshalRuntimeConfigProjection(projection)) != envelope.SnapshotDigest {
		return RuntimeConfigDeliveryEnvelope{}, RuntimeConfigDeliveryProjection{}, fmt.Errorf("%w: projection digest/version 不一致", ErrRuntimeConfigDeliveryAuth)
	}
	if checkLocalSnapshot && r.RequireLocalSnapshot && r.LocalSnapshotResolver == nil {
		return RuntimeConfigDeliveryEnvelope{}, RuntimeConfigDeliveryProjection{}, fmt.Errorf("%w: 未配置本地快照解析器", ErrRuntimeConfigDeliveryAuth)
	}
	if checkLocalSnapshot && r.LocalSnapshotResolver != nil {
		local, resolveErr := r.LocalSnapshotResolver(request.Context(), envelope.InvocationID)
		if resolveErr != nil {
			return RuntimeConfigDeliveryEnvelope{}, RuntimeConfigDeliveryProjection{}, fmt.Errorf("%w: 本地配置版本不可用", ErrRuntimeConfigDeliveryAuth)
		}
		localProjection, normalizeErr := normalizeRuntimeConfigDeliveryProjection(local)
		if normalizeErr != nil || agentRuntimeConfigSnapshotDigest(localProjection) != envelope.SnapshotDigest {
			return RuntimeConfigDeliveryEnvelope{}, RuntimeConfigDeliveryProjection{}, fmt.Errorf("%w: 本地配置版本不匹配", ErrRuntimeConfigDeliveryAuth)
		}
	}
	return envelope, projection, nil
}

func runtimeConfigDeliveryHTTPStatus(err error) int {
	if errors.Is(err, ErrRuntimeConfigDeliveryAuth) {
		return http.StatusUnauthorized
	}
	if errors.Is(err, ErrRuntimeConfigDeliveryStale) {
		return http.StatusUnauthorized
	}
	if errors.Is(err, ErrConflict) {
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

func (r *RuntimeConfigDeliveryReceiver) transactionHandler(phase string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if r == nil {
			writeRuntimeConfigDeliveryError(writer, http.StatusInternalServerError, ErrInvalidRuntimeConfigDelivery)
			return
		}
		if request.Method != http.MethodPost {
			writeRuntimeConfigDeliveryError(writer, http.StatusMethodNotAllowed, fmt.Errorf("%w: 只支持 POST", ErrInvalidRuntimeConfigDelivery))
			return
		}
		if header := strings.TrimSpace(request.Header.Get("X-Abot-Config-Delivery-Phase")); header != phase {
			writeRuntimeConfigDeliveryError(writer, http.StatusBadRequest, fmt.Errorf("%w: phase header 不一致", ErrInvalidRuntimeConfigDelivery))
			return
		}
		repository, ok := r.Inbox.(RuntimeConfigDeliveryTransactionRepository)
		if !ok {
			writeRuntimeConfigDeliveryError(writer, http.StatusNotImplemented, fmt.Errorf("%w: receiver 未提供事务仓储", ErrInvalidRuntimeConfigDelivery))
			return
		}
		checkLocalSnapshot := phase != RuntimeConfigDeliveryTransactionPhaseAborted
		envelope, projection, err := r.validateRuntimeConfigDeliveryRequestWithLocalSnapshot(request, checkLocalSnapshot)
		if err != nil {
			writeRuntimeConfigDeliveryError(writer, runtimeConfigDeliveryHTTPStatus(err), err)
			return
		}
		var duplicate bool
		switch phase {
		case RuntimeConfigDeliveryTransactionPhasePrepared:
			duplicate, err = repository.PrepareRuntimeConfigDelivery(request.Context(), envelope, projection)
		case RuntimeConfigDeliveryTransactionPhaseCommitted:
			duplicate, err = repository.CommitRuntimeConfigDelivery(request.Context(), envelope)
		case RuntimeConfigDeliveryTransactionPhaseAborted:
			aborter, abortOK := r.Inbox.(RuntimeConfigDeliveryAbortableTransactionRepository)
			if !abortOK {
				writeRuntimeConfigDeliveryError(writer, http.StatusNotImplemented, fmt.Errorf("%w: receiver 未提供事务 abort 仓储", ErrInvalidRuntimeConfigDelivery))
				return
			}
			duplicate, err = aborter.AbortRuntimeConfigDelivery(request.Context(), envelope)
		}
		if err != nil {
			writeRuntimeConfigDeliveryError(writer, runtimeConfigDeliveryHTTPStatus(err), err)
			return
		}
		receipt := RuntimeConfigDeliveryReceipt{Version: envelope.Version, DeliveryID: envelope.DeliveryID, InvocationID: envelope.InvocationID, ExpectedDigest: envelope.ExpectedDigest, SnapshotDigest: envelope.SnapshotDigest, SnapshotVersion: envelope.SnapshotVersion, Phase: phase, Duplicate: duplicate}
		writeRuntimeConfigDeliveryJSON(writer, http.StatusOK, receipt)
	})
}

func (r *RuntimeConfigDeliveryReceiver) PrepareHandler() http.Handler {
	return r.transactionHandler(RuntimeConfigDeliveryTransactionPhasePrepared)
}

func (r *RuntimeConfigDeliveryReceiver) CommitHandler() http.Handler {
	return r.transactionHandler(RuntimeConfigDeliveryTransactionPhaseCommitted)
}

// AbortHandler exposes the authenticated compensation endpoint for a
// prepared configuration lock.
func (r *RuntimeConfigDeliveryReceiver) AbortHandler() http.Handler {
	return r.transactionHandler(RuntimeConfigDeliveryTransactionPhaseAborted)
}

// RuntimeConfigDeliveryHTTPTransport sends one signed projection to a
// receiver.  The transactional constructor is opt-in for rolling upgrades.
type RuntimeConfigDeliveryHTTPTransport struct {
	Endpoint      string
	Source        string
	Destination   string
	SharedSecret  []byte
	Client        *http.Client
	MaxBodyBytes  int64
	transactional bool
}

func NewRuntimeConfigDeliveryHTTPTransport(endpoint, source, destination string, secret []byte, client *http.Client) (*RuntimeConfigDeliveryHTTPTransport, error) {
	endpoint = strings.TrimSpace(endpoint)
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("%w: endpoint 必须是 http/https URL", ErrInvalidRuntimeConfigDelivery)
	}
	if strings.TrimSpace(source) == "" || strings.TrimSpace(destination) == "" {
		return nil, fmt.Errorf("%w: source/destination 无效", ErrInvalidRuntimeConfigDelivery)
	}
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &RuntimeConfigDeliveryHTTPTransport{Endpoint: endpoint, Source: strings.TrimSpace(source), Destination: strings.TrimSpace(destination), SharedSecret: append([]byte(nil), secret...), Client: client, MaxBodyBytes: MaxRuntimeConfigDeliveryResponseBytes}, nil
}

func NewRuntimeConfigDeliveryHTTPTransactionalTransport(endpoint, source, destination string, secret []byte, client *http.Client) (*RuntimeConfigDeliveryHTTPTransport, error) {
	transport, err := NewRuntimeConfigDeliveryHTTPTransport(endpoint, source, destination, secret, client)
	if err != nil {
		return nil, err
	}
	transport.transactional = true
	return transport, nil
}

func (t *RuntimeConfigDeliveryHTTPTransport) RuntimeConfigDeliveryTransactionsEnabled() bool {
	return t != nil && t.transactional
}

func (t *RuntimeConfigDeliveryHTTPTransport) Deliver(ctx context.Context, envelope RuntimeConfigDeliveryEnvelope, projection RuntimeConfigDeliveryProjection) (RuntimeConfigDeliveryReceipt, error) {
	return t.do(ctx, "", envelope, projection)
}

func (t *RuntimeConfigDeliveryHTTPTransport) Prepare(ctx context.Context, envelope RuntimeConfigDeliveryEnvelope, projection RuntimeConfigDeliveryProjection) (RuntimeConfigDeliveryReceipt, error) {
	return t.do(ctx, RuntimeConfigDeliveryTransactionPhasePrepared, envelope, projection)
}

func (t *RuntimeConfigDeliveryHTTPTransport) Commit(ctx context.Context, envelope RuntimeConfigDeliveryEnvelope, projection RuntimeConfigDeliveryProjection) (RuntimeConfigDeliveryReceipt, error) {
	return t.do(ctx, RuntimeConfigDeliveryTransactionPhaseCommitted, envelope, projection)
}

func (t *RuntimeConfigDeliveryHTTPTransport) Abort(ctx context.Context, envelope RuntimeConfigDeliveryEnvelope, projection RuntimeConfigDeliveryProjection) (RuntimeConfigDeliveryReceipt, error) {
	return t.do(ctx, RuntimeConfigDeliveryTransactionPhaseAborted, envelope, projection)
}

func (t *RuntimeConfigDeliveryHTTPTransport) do(ctx context.Context, phase string, envelope RuntimeConfigDeliveryEnvelope, projection RuntimeConfigDeliveryProjection) (RuntimeConfigDeliveryReceipt, error) {
	if t == nil {
		return RuntimeConfigDeliveryReceipt{}, fmt.Errorf("%w: transport 为空", ErrInvalidRuntimeConfigDelivery)
	}
	normalized, err := NormalizeRuntimeConfigDeliveryEnvelope(envelope)
	if err != nil {
		return RuntimeConfigDeliveryReceipt{}, err
	}
	if normalized.Source != t.Source || normalized.Destination != t.Destination {
		return RuntimeConfigDeliveryReceipt{}, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeConfigDeliveryAuth)
	}
	parsed, err := agent.ParseRuntimeConfigSnapshot(mustMarshalRuntimeConfigProjection(projection))
	if err != nil {
		return RuntimeConfigDeliveryReceipt{}, err
	}
	if parsed.Version != normalized.SnapshotVersion || agent.RuntimeConfigSnapshotDigest(mustMarshalRuntimeConfigProjection(parsed)) != normalized.SnapshotDigest {
		return RuntimeConfigDeliveryReceipt{}, fmt.Errorf("%w: projection digest/version 不一致", ErrRuntimeConfigDeliveryAuth)
	}
	signed, err := SignRuntimeConfigDeliveryEnvelope(normalized, t.SharedSecret)
	if err != nil {
		return RuntimeConfigDeliveryReceipt{}, err
	}
	body, err := json.Marshal(runtimeConfigDeliveryPayload{Envelope: signed, Projection: parsed})
	if err != nil || len(body) > MaxRuntimeConfigDeliveryResponseBytes {
		return RuntimeConfigDeliveryReceipt{}, fmt.Errorf("%w: request 编码超过上限", ErrInvalidRuntimeConfigDelivery)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint := t.Endpoint
	if phase == RuntimeConfigDeliveryTransactionPhasePrepared {
		endpoint = strings.TrimRight(endpoint, "/") + "/prepare"
	} else if phase == RuntimeConfigDeliveryTransactionPhaseCommitted {
		endpoint = strings.TrimRight(endpoint, "/") + "/commit"
	} else if phase == RuntimeConfigDeliveryTransactionPhaseAborted {
		endpoint = strings.TrimRight(endpoint, "/") + "/abort"
	} else if phase != "" {
		return RuntimeConfigDeliveryReceipt{}, fmt.Errorf("%w: transaction phase 无效", ErrInvalidRuntimeConfigDelivery)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return RuntimeConfigDeliveryReceipt{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Abot-Config-Delivery-Version", strconv.Itoa(signed.Version))
	request.Header.Set("X-Abot-Config-Delivery-Signature", signed.Signature)
	request.Header.Set("Idempotency-Key", signed.DeliveryID)
	if phase != "" {
		request.Header.Set("X-Abot-Config-Delivery-Phase", phase)
	}
	response, err := t.Client.Do(request)
	if err != nil {
		return RuntimeConfigDeliveryReceipt{}, fmt.Errorf("Runtime config delivery 请求失败: %w", err)
	}
	defer response.Body.Close()
	maxBody := t.MaxBodyBytes
	if maxBody <= 0 || maxBody > MaxRuntimeConfigDeliveryResponseBytes {
		maxBody = MaxRuntimeConfigDeliveryResponseBytes
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil || int64(len(responseBody)) > maxBody {
		return RuntimeConfigDeliveryReceipt{}, fmt.Errorf("%w: response 超过字节上限或读取失败", ErrInvalidRuntimeConfigDelivery)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(SanitizeRuntimeString(string(responseBody)))
		if len(message) > 512 {
			message = message[:512]
		}
		return RuntimeConfigDeliveryReceipt{}, fmt.Errorf("Runtime config delivery 返回 HTTP %d: %s", response.StatusCode, message)
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	var receipt RuntimeConfigDeliveryReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return RuntimeConfigDeliveryReceipt{}, fmt.Errorf("%w: receipt JSON 无效", ErrInvalidRuntimeConfigDelivery)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeConfigDeliveryReceipt{}, fmt.Errorf("%w: receipt 包含多个 JSON 文档", ErrInvalidRuntimeConfigDelivery)
	}
	if err := receipt.ValidateAgainst(signed); err != nil {
		return RuntimeConfigDeliveryReceipt{}, err
	}
	if phase != "" && strings.TrimSpace(receipt.Phase) != phase {
		return RuntimeConfigDeliveryReceipt{}, fmt.Errorf("%w: receipt phase 不一致", ErrRuntimeConfigDeliveryAuth)
	}
	return receipt, nil
}

var _ RuntimeConfigDeliveryAbortableTransport = (*RuntimeConfigDeliveryHTTPTransport)(nil)

func writeRuntimeConfigDeliveryJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeRuntimeConfigDeliveryError(writer http.ResponseWriter, status int, err error) {
	message := "Runtime config delivery failed"
	if err != nil {
		message = strings.TrimSpace(SanitizeRuntimeString(err.Error()))
		if len(message) > 512 {
			message = message[:512]
		}
	}
	writeRuntimeConfigDeliveryJSON(writer, status, map[string]any{"error": message})
}
