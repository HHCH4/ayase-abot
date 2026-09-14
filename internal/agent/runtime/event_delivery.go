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
	"strconv"
	"strings"
	"time"
)

const (
	// RuntimeEventDeliveryEnvelopeVersion is the legacy wire contract. Version
	// 1 is retained for existing receivers: its Timestamp doubles as the
	// replay timestamp.
	RuntimeEventDeliveryEnvelopeVersion = 1
	// RuntimeEventDeliveryEnvelopeVersionIssuedAt separates the immutable
	// source event timestamp from the per-attempt replay timestamp. It is an
	// explicit opt-in so a legacy receiver that only understands version 1 is
	// never sent an unknown field accidentally.
	RuntimeEventDeliveryEnvelopeVersionIssuedAt = 2
	// Keep the transport body small: event contents remain in the source event
	// log and are not copied into this envelope.
	MaxRuntimeEventDeliveryEnvelopeBytes  = 64 << 10
	MaxRuntimeEventDeliveryDigestBytes    = 512 << 10
	MaxRuntimeEventDeliverySecretBytes    = 4 << 10
	MinRuntimeEventDeliverySecretBytes    = 16
	MaxRuntimeEventDeliverySourceLength   = 160
	MaxRuntimeEventDeliveryTargetLength   = 160
	MaxRuntimeEventDeliveryIDLength       = 220
	MaxRuntimeEventDeliveryEventIDLength  = 512
	MaxRuntimeEventDeliveryTypeLength     = 80
	DefaultRuntimeEventDeliveryMaxAge     = 10 * time.Minute
	DefaultRuntimeEventDeliveryFutureSkew = 30 * time.Second
)

var (
	ErrInvalidRuntimeEventDelivery = errors.New("Runtime event delivery envelope 无效")
	ErrRuntimeEventDeliveryAuth    = errors.New("Runtime event delivery 认证失败")
	ErrRuntimeEventDeliveryStale   = errors.New("Runtime event delivery 已过期")
)

// RuntimeEventDeliveryEnvelope carries immutable event identity and a digest
// of the source event. The event body is intentionally not copied: consumers
// either fetch it through an authenticated source API or use the metadata
// projection for telemetry. Timestamp is the source event's immutable time.
// Version 2 additionally signs IssuedAt, the time this delivery attempt was
// created, so a durable outbox can deliver an old event without widening the
// replay window. HMAC covers every field except Signature, so retries cannot
// change routing or ordering metadata.
type RuntimeEventDeliveryEnvelope struct {
	Version      int       `json:"version"`
	Source       string    `json:"source"`
	Destination  string    `json:"destination"`
	DeliveryID   string    `json:"delivery_id"`
	EventID      string    `json:"event_id"`
	InvocationID string    `json:"invocation_id"`
	Sequence     int64     `json:"sequence"`
	Type         string    `json:"type"`
	Timestamp    time.Time `json:"timestamp"`
	IssuedAt     time.Time `json:"issued_at,omitempty"`
	EventDigest  string    `json:"event_digest"`
	Signature    string    `json:"signature,omitempty"`
}

// RuntimeEventDeliveryTransport is the optional cross-service delivery hook
// used by Coordinator's durable event outbox. Implementations must be
// idempotent: a successful transport call can be repeated before the source
// outbox acknowledgement is committed.
type RuntimeEventDeliveryTransport interface {
	Deliver(context.Context, RuntimeEventOutbox, AgentEvent) error
}

// RuntimeEventDeliveryTransactionalTransport is an optional stronger
// destination adapter. BuildEnvelope binds the claimed outbox cursor to the
// immutable source event; Prepare records that signed metadata at the
// destination and Commit makes it visible through the destination inbox.
// Both phases must be idempotent by DeliveryID so a source worker crash can
// replay either phase without duplicating the event.
type RuntimeEventDeliveryTransactionalTransport interface {
	BuildEnvelope(RuntimeEventOutbox, AgentEvent) (RuntimeEventDeliveryEnvelope, error)
	Prepare(context.Context, RuntimeEventDeliveryEnvelope) (RuntimeEventDeliveryReceipt, error)
	Commit(context.Context, RuntimeEventDeliveryEnvelope) (RuntimeEventDeliveryReceipt, error)
}

// RuntimeEventDeliveryAbortableTransport is an optional compensation
// capability for transactional transports. Abort can only move a destination
// transaction from prepared to aborted; a committed event is never undone.
// Keeping this capability separate preserves compatibility with existing
// Prepare/Commit adapters during rolling upgrades.
type RuntimeEventDeliveryAbortableTransport interface {
	Abort(context.Context, RuntimeEventDeliveryEnvelope) (RuntimeEventDeliveryReceipt, error)
}

// RuntimeEventDeliveryTransactionalOptIn lets a concrete transport expose
// Prepare/Commit methods while keeping the legacy one-phase constructor
// backwards compatible. Unmarked transactional implementations remain
// enabled by default for custom in-process adapters.
type RuntimeEventDeliveryTransactionalOptIn interface {
	RuntimeEventDeliveryTransactionsEnabled() bool
}

// RuntimeEventDeliveryTransactionRepository is the destination-side durable
// prepare/commit ledger. It stores only signed event metadata; the event body
// remains in the source event log and is never copied into the transaction.
type RuntimeEventDeliveryTransactionRepository interface {
	PrepareRuntimeEventDelivery(context.Context, RuntimeEventDeliveryEnvelope) (duplicate bool, err error)
	CommitRuntimeEventDelivery(context.Context, RuntimeEventDeliveryEnvelope) (duplicate bool, err error)
}

// RuntimeEventDeliveryAbortableTransactionRepository is the optional
// destination-side compensation boundary. It is deliberately additive to the
// prepare/commit repository so older receivers remain source-compatible.
type RuntimeEventDeliveryAbortableTransactionRepository interface {
	AbortRuntimeEventDelivery(context.Context, RuntimeEventDeliveryEnvelope) (duplicate bool, err error)
}

// RuntimeEventDeliveryInbox is the receiver-side idempotency boundary. A
// true return means the envelope was already accepted with identical
// metadata; a conflicting EventID must return ErrConflict.
type RuntimeEventDeliveryInbox interface {
	AcceptRuntimeEventDelivery(context.Context, RuntimeEventDeliveryEnvelope) (duplicate bool, err error)
}

// RuntimeEventDeliveryInboxReader is the optional destination read boundary
// used by the authenticated status endpoint. Keeping the reader separate
// preserves compatibility with older custom inbox implementations that only
// support one-phase acceptance.
type RuntimeEventDeliveryInboxReader interface {
	GetRuntimeEventDelivery(context.Context, string) (RuntimeEventDeliveryInboxRecord, error)
}

// RuntimeEventDeliveryReconciler is an optional source transport capability.
// It returns a signed destination proof without sending the event body again.
// The Coordinator uses it only after claiming the source outbox cursor.
type RuntimeEventDeliveryReconciler interface {
	ReconcileEventDelivery(context.Context, RuntimeEventOutbox, AgentEvent) (RuntimeDeliveryStatus, error)
}

// RuntimeEventDeliveryRouteProvider lets the source journal retain the
// transport's explicit route instead of the local-handler compatibility
// labels. It is optional for custom transports.
type RuntimeEventDeliveryRouteProvider interface {
	RuntimeEventDeliveryRoute() (source, destination string)
}

// RuntimeEventDeliveryReconciliationEnvelopeBuilder is an optional helper
// for transactional transports. It creates a fresh v2 envelope when a status
// query discovers a prepared destination record after the source replay
// window, while preserving the legacy BuildEnvelope contract for first sends.
type RuntimeEventDeliveryReconciliationEnvelopeBuilder interface {
	BuildRuntimeEventDeliveryReconciliationEnvelope(RuntimeEventOutbox, AgentEvent, time.Time) (RuntimeEventDeliveryEnvelope, error)
}

// RuntimeEventDeliveryInboxRecord is the metadata retained by an inbox
// implementation. It contains no event body, prompt, tool argument or
// credential.
type RuntimeEventDeliveryInboxRecord struct {
	Version      int       `json:"version"`
	EventID      string    `json:"event_id"`
	DeliveryID   string    `json:"delivery_id"`
	Source       string    `json:"source"`
	Destination  string    `json:"destination"`
	InvocationID string    `json:"invocation_id"`
	Sequence     int64     `json:"sequence"`
	Type         string    `json:"type"`
	Timestamp    time.Time `json:"timestamp"`
	IssuedAt     time.Time `json:"issued_at,omitempty"`
	EventDigest  string    `json:"event_digest"`
	Signature    string    `json:"signature"`
	ReceivedAt   time.Time `json:"received_at"`
}

func runtimeEventDeliveryInboxRecord(envelope RuntimeEventDeliveryEnvelope, receivedAt time.Time) RuntimeEventDeliveryInboxRecord {
	return RuntimeEventDeliveryInboxRecord{
		Version: envelope.Version, EventID: envelope.EventID, DeliveryID: envelope.DeliveryID, Source: envelope.Source,
		Destination: envelope.Destination, InvocationID: envelope.InvocationID, Sequence: envelope.Sequence,
		Type: envelope.Type, Timestamp: envelope.Timestamp.UTC(), IssuedAt: envelope.IssuedAt.UTC(), EventDigest: envelope.EventDigest, Signature: envelope.Signature,
		ReceivedAt: receivedAt.UTC(),
	}
}

// RuntimeEventDeliveryInboxRecordFromEnvelope builds the metadata row shared
// by Memory and SQLite inbox implementations.
func RuntimeEventDeliveryInboxRecordFromEnvelope(envelope RuntimeEventDeliveryEnvelope, receivedAt time.Time) RuntimeEventDeliveryInboxRecord {
	return runtimeEventDeliveryInboxRecord(envelope, receivedAt)
}

func (r RuntimeEventDeliveryInboxRecord) matches(envelope RuntimeEventDeliveryEnvelope) bool {
	// Version, IssuedAt and Signature are wire/authentication details. A v2
	// retry may refresh IssuedAt (and therefore its signature), while a v1
	// retry may still arrive during a rolling upgrade. The event identity and
	// digest remain immutable and are the idempotency boundary.
	return r.EventID == envelope.EventID && r.DeliveryID == envelope.DeliveryID && r.Source == envelope.Source &&
		r.Destination == envelope.Destination && r.InvocationID == envelope.InvocationID && r.Sequence == envelope.Sequence &&
		r.Type == envelope.Type && r.Timestamp.Equal(envelope.Timestamp) && r.EventDigest == envelope.EventDigest
}

// Matches reports whether an existing inbox row is the exact same immutable
// delivery. It is exported for storage implementations while keeping the
// comparison rules in one package.
func (r RuntimeEventDeliveryInboxRecord) Matches(envelope RuntimeEventDeliveryEnvelope) bool {
	return r.matches(envelope)
}

func (e RuntimeEventDeliveryEnvelope) normalize() (RuntimeEventDeliveryEnvelope, error) {
	e.Source = strings.TrimSpace(e.Source)
	e.Destination = strings.TrimSpace(e.Destination)
	e.DeliveryID = strings.TrimSpace(e.DeliveryID)
	e.EventID = strings.TrimSpace(e.EventID)
	e.InvocationID = strings.TrimSpace(e.InvocationID)
	e.Type = strings.TrimSpace(e.Type)
	e.EventDigest = strings.TrimSpace(strings.ToLower(e.EventDigest))
	e.Signature = strings.TrimSpace(strings.ToLower(e.Signature))
	switch e.Version {
	case RuntimeEventDeliveryEnvelopeVersion:
		if !e.IssuedAt.IsZero() {
			return RuntimeEventDeliveryEnvelope{}, fmt.Errorf("%w: version 1 不能携带 issued_at", ErrInvalidRuntimeEventDelivery)
		}
	case RuntimeEventDeliveryEnvelopeVersionIssuedAt:
		if e.IssuedAt.IsZero() {
			return RuntimeEventDeliveryEnvelope{}, fmt.Errorf("%w: version 2 必须携带 issued_at", ErrInvalidRuntimeEventDelivery)
		}
	default:
		return RuntimeEventDeliveryEnvelope{}, fmt.Errorf("%w: version %d 不受支持", ErrInvalidRuntimeEventDelivery, e.Version)
	}
	if e.Source == "" || e.Destination == "" || e.DeliveryID == "" || e.EventID == "" || e.InvocationID == "" || e.Type == "" || e.Sequence <= 0 || e.Timestamp.IsZero() {
		return RuntimeEventDeliveryEnvelope{}, fmt.Errorf("%w: 必填 metadata 不完整", ErrInvalidRuntimeEventDelivery)
	}
	if len(e.Source) > MaxRuntimeEventDeliverySourceLength || len(e.Destination) > MaxRuntimeEventDeliveryTargetLength || len(e.DeliveryID) > MaxRuntimeEventDeliveryIDLength || len(e.EventID) > MaxRuntimeEventDeliveryEventIDLength || len(e.InvocationID) > MaxRuntimeEventDeliveryEventIDLength || len(e.Type) > MaxRuntimeEventDeliveryTypeLength {
		return RuntimeEventDeliveryEnvelope{}, fmt.Errorf("%w: metadata 超出长度限制", ErrInvalidRuntimeEventDelivery)
	}
	if !isRuntimeEventDeliveryDigest(e.EventDigest) {
		return RuntimeEventDeliveryEnvelope{}, fmt.Errorf("%w: event_digest 无效", ErrInvalidRuntimeEventDelivery)
	}
	if e.Signature != "" && !isRuntimeEventDeliveryDigest(e.Signature) {
		return RuntimeEventDeliveryEnvelope{}, fmt.Errorf("%w: signature 无效", ErrInvalidRuntimeEventDelivery)
	}
	e.Timestamp = e.Timestamp.UTC()
	if !e.IssuedAt.IsZero() {
		e.IssuedAt = e.IssuedAt.UTC()
	}
	return e, nil
}

func (e RuntimeEventDeliveryEnvelope) Validate() error {
	_, err := e.normalize()
	return err
}

// NormalizeRuntimeEventDeliveryEnvelope returns a trimmed, validated copy for
// storage implementations and custom receivers.
func NormalizeRuntimeEventDeliveryEnvelope(envelope RuntimeEventDeliveryEnvelope) (RuntimeEventDeliveryEnvelope, error) {
	return envelope.normalize()
}

// RuntimeEventDeliveryReplayTimestamp returns the timestamp subject to the
// receiver/source replay window. Legacy v1 envelopes use their immutable
// event Timestamp; v2 envelopes use the signed per-attempt IssuedAt.
func RuntimeEventDeliveryReplayTimestamp(envelope RuntimeEventDeliveryEnvelope) time.Time {
	if !envelope.IssuedAt.IsZero() {
		return envelope.IssuedAt.UTC()
	}
	return envelope.Timestamp.UTC()
}

func isRuntimeEventDeliveryDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateRuntimeEventDeliverySecret(secret []byte) error {
	if len(secret) < MinRuntimeEventDeliverySecretBytes || len(secret) > MaxRuntimeEventDeliverySecretBytes {
		return fmt.Errorf("%w: shared secret 长度必须在 %d-%d 字节之间", ErrInvalidRuntimeEventDelivery, MinRuntimeEventDeliverySecretBytes, MaxRuntimeEventDeliverySecretBytes)
	}
	return nil
}

// RuntimeEventDeliveryEventDigest computes the stable digest used to bind a
// transport envelope to the immutable source event. The sanitized event is
// bounded before hashing to prevent an accidental unbounded transport job.
func RuntimeEventDeliveryEventDigest(event AgentEvent) (string, error) {
	encoded, err := json.Marshal(SanitizeAgentEvent(event))
	if err != nil {
		return "", fmt.Errorf("%w: event digest 编码失败: %v", ErrInvalidRuntimeEventDelivery, err)
	}
	if len(encoded) > MaxRuntimeEventDeliveryDigestBytes {
		return "", fmt.Errorf("%w: event digest 输入超过 %d 字节上限", ErrInvalidRuntimeEventDelivery, MaxRuntimeEventDeliveryDigestBytes)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// NewRuntimeEventDeliveryEnvelope binds one claimed outbox cursor to the
// immutable event that it represents. It rejects forged cursors before a
// transport request is created.
func NewRuntimeEventDeliveryEnvelope(source, destination string, outbox RuntimeEventOutbox, event AgentEvent) (RuntimeEventDeliveryEnvelope, error) {
	if strings.TrimSpace(event.ID) == "" || strings.TrimSpace(event.InvocationID) == "" || strings.TrimSpace(event.Type) == "" || event.Sequence <= 0 || event.Timestamp.IsZero() {
		return RuntimeEventDeliveryEnvelope{}, fmt.Errorf("%w: source event metadata 不完整", ErrInvalidRuntimeEventDelivery)
	}
	normalized, err := outbox.Normalize(event.Timestamp)
	if err != nil {
		return RuntimeEventDeliveryEnvelope{}, err
	}
	expectedOutbox, err := NewRuntimeEventOutbox(event)
	if err != nil {
		return RuntimeEventDeliveryEnvelope{}, err
	}
	if normalized.ID != expectedOutbox.ID {
		return RuntimeEventDeliveryEnvelope{}, fmt.Errorf("%w: outbox ID 不是 EventID 派生的规范值", ErrInvalidRuntimeEventDelivery)
	}
	if normalized.EventID != strings.TrimSpace(event.ID) || normalized.InvocationID != strings.TrimSpace(event.InvocationID) || normalized.Sequence != event.Sequence || normalized.Type != strings.TrimSpace(event.Type) {
		return RuntimeEventDeliveryEnvelope{}, fmt.Errorf("%w: outbox 与 event metadata 不匹配", ErrInvalidRuntimeEventDelivery)
	}
	digest, err := RuntimeEventDeliveryEventDigest(event)
	if err != nil {
		return RuntimeEventDeliveryEnvelope{}, err
	}
	return (RuntimeEventDeliveryEnvelope{
		Version: RuntimeEventDeliveryEnvelopeVersion, Source: strings.TrimSpace(source), Destination: strings.TrimSpace(destination),
		DeliveryID: normalized.ID, EventID: normalized.EventID, InvocationID: normalized.InvocationID,
		Sequence: normalized.Sequence, Type: normalized.Type, Timestamp: event.Timestamp.UTC(), EventDigest: digest,
	}).normalize()
}

// NewRuntimeEventDeliveryEnvelopeWithIssuedAt creates the opt-in v2 envelope
// used when an old source event must be delivered after the legacy replay
// window. Timestamp remains bound to the immutable event; IssuedAt is the
// bounded freshness proof for this delivery attempt.
func NewRuntimeEventDeliveryEnvelopeWithIssuedAt(source, destination string, outbox RuntimeEventOutbox, event AgentEvent, issuedAt time.Time) (RuntimeEventDeliveryEnvelope, error) {
	if issuedAt.IsZero() {
		return RuntimeEventDeliveryEnvelope{}, fmt.Errorf("%w: issued_at 不能为空", ErrInvalidRuntimeEventDelivery)
	}
	envelope, err := NewRuntimeEventDeliveryEnvelope(source, destination, outbox, event)
	if err != nil {
		return RuntimeEventDeliveryEnvelope{}, err
	}
	envelope.Version = RuntimeEventDeliveryEnvelopeVersionIssuedAt
	envelope.IssuedAt = issuedAt.UTC()
	return envelope.normalize()
}

// RefreshRuntimeEventDeliveryEnvelope upgrades a valid envelope to v2 (or
// refreshes its existing v2 IssuedAt) while preserving the immutable event
// identity and clearing the old signature. Callers must sign the returned
// value before sending it.
func RefreshRuntimeEventDeliveryEnvelope(envelope RuntimeEventDeliveryEnvelope, issuedAt time.Time) (RuntimeEventDeliveryEnvelope, error) {
	if issuedAt.IsZero() {
		return RuntimeEventDeliveryEnvelope{}, fmt.Errorf("%w: issued_at 不能为空", ErrInvalidRuntimeEventDelivery)
	}
	normalized, err := NormalizeRuntimeEventDeliveryEnvelope(envelope)
	if err != nil {
		return RuntimeEventDeliveryEnvelope{}, err
	}
	normalized.Version = RuntimeEventDeliveryEnvelopeVersionIssuedAt
	normalized.IssuedAt = issuedAt.UTC()
	normalized.Signature = ""
	return normalized.normalize()
}

func (e RuntimeEventDeliveryEnvelope) canonicalUnsigned() ([]byte, error) {
	normalized, err := e.normalize()
	if err != nil {
		return nil, err
	}
	normalized.Signature = ""
	return json.Marshal(normalized)
}

// SignRuntimeEventDeliveryEnvelope returns a copy with a lowercase hex HMAC.
func SignRuntimeEventDeliveryEnvelope(envelope RuntimeEventDeliveryEnvelope, secret []byte) (RuntimeEventDeliveryEnvelope, error) {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return RuntimeEventDeliveryEnvelope{}, err
	}
	canonical, err := envelope.canonicalUnsigned()
	if err != nil {
		return RuntimeEventDeliveryEnvelope{}, err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	envelope.Signature = hex.EncodeToString(mac.Sum(nil))
	return envelope.normalize()
}

// VerifyRuntimeEventDeliveryEnvelope authenticates an already normalized
// envelope in constant time. Timestamp freshness is checked separately so a
// caller can choose a deployment-specific replay window.
func VerifyRuntimeEventDeliveryEnvelope(envelope RuntimeEventDeliveryEnvelope, secret []byte) error {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return err
	}
	normalized, err := envelope.normalize()
	if err != nil {
		return err
	}
	if normalized.Signature == "" {
		return fmt.Errorf("%w: 缺少 signature", ErrRuntimeEventDeliveryAuth)
	}
	canonical, err := normalized.canonicalUnsigned()
	if err != nil {
		return err
	}
	provided, err := hex.DecodeString(normalized.Signature)
	if err != nil {
		return fmt.Errorf("%w: signature 编码无效", ErrRuntimeEventDeliveryAuth)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return fmt.Errorf("%w: HMAC 不匹配", ErrRuntimeEventDeliveryAuth)
	}
	return nil
}

func validateRuntimeEventDeliveryTimestamp(timestamp, now time.Time, maxAge, maxFutureSkew time.Duration) error {
	if maxAge <= 0 || maxFutureSkew < 0 {
		return fmt.Errorf("%w: timestamp policy 无效", ErrInvalidRuntimeEventDelivery)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	timestamp = timestamp.UTC()
	if timestamp.After(now.Add(maxFutureSkew)) {
		return fmt.Errorf("%w: timestamp 位于允许未来窗口之外", ErrRuntimeEventDeliveryAuth)
	}
	if now.Sub(timestamp) > maxAge {
		return fmt.Errorf("%w: timestamp 已过期", ErrRuntimeEventDeliveryAuth)
	}
	return nil
}

// RuntimeEventDeliveryHTTPTransport posts signed metadata to one receiver.
// It is deliberately not enabled by default; application wiring must provide
// an endpoint and shared secret explicitly.
type RuntimeEventDeliveryHTTPTransport struct {
	Endpoint     string
	Source       string
	Destination  string
	SharedSecret []byte
	Client       *http.Client
	// Now is injectable for deterministic tests and hosts with an explicit
	// clock. It is used only by the opt-in v2 fresh-timestamp path.
	Now            func() time.Time
	transactional  bool
	freshTimestamp bool
}

func NewRuntimeEventDeliveryHTTPTransport(endpoint, source, destination string, secret []byte, client *http.Client) (*RuntimeEventDeliveryHTTPTransport, error) {
	endpoint = strings.TrimSpace(endpoint)
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("%w: delivery endpoint 必须是 http/https URL", ErrInvalidRuntimeEventDelivery)
	}
	if strings.TrimSpace(source) == "" || len(strings.TrimSpace(source)) > MaxRuntimeEventDeliverySourceLength || strings.TrimSpace(destination) == "" || len(strings.TrimSpace(destination)) > MaxRuntimeEventDeliveryTargetLength {
		return nil, fmt.Errorf("%w: source/destination 无效", ErrInvalidRuntimeEventDelivery)
	}
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &RuntimeEventDeliveryHTTPTransport{Endpoint: endpoint, Source: strings.TrimSpace(source), Destination: strings.TrimSpace(destination), SharedSecret: append([]byte(nil), secret...), Client: client, Now: time.Now}, nil
}

// NewRuntimeEventDeliveryHTTPTransportWithIssuedAt opts an HTTP transport
// into the v2 envelope that refreshes the signed replay timestamp on every
// attempt. The original constructor remains v1-compatible for receivers that
// have not upgraded their decoder and timestamp policy.
func NewRuntimeEventDeliveryHTTPTransportWithIssuedAt(endpoint, source, destination string, secret []byte, client *http.Client) (*RuntimeEventDeliveryHTTPTransport, error) {
	transport, err := NewRuntimeEventDeliveryHTTPTransport(endpoint, source, destination, secret, client)
	if err != nil {
		return nil, err
	}
	transport.freshTimestamp = true
	return transport, nil
}

// NewRuntimeEventDeliveryHTTPTransactionalTransport opts an HTTP transport
// into the prepare/commit event protocol. The original constructor remains
// one-phase so existing receivers exposing only /event-delivery continue to
// work until both sides explicitly upgrade.
func NewRuntimeEventDeliveryHTTPTransactionalTransport(endpoint, source, destination string, secret []byte, client *http.Client) (*RuntimeEventDeliveryHTTPTransport, error) {
	transport, err := NewRuntimeEventDeliveryHTTPTransport(endpoint, source, destination, secret, client)
	if err != nil {
		return nil, err
	}
	transport.transactional = true
	return transport, nil
}

// NewRuntimeEventDeliveryHTTPTransactionalTransportWithIssuedAt is the
// explicit v2 counterpart of NewRuntimeEventDeliveryHTTPTransactionalTransport.
func NewRuntimeEventDeliveryHTTPTransactionalTransportWithIssuedAt(endpoint, source, destination string, secret []byte, client *http.Client) (*RuntimeEventDeliveryHTTPTransport, error) {
	transport, err := NewRuntimeEventDeliveryHTTPTransportWithIssuedAt(endpoint, source, destination, secret, client)
	if err != nil {
		return nil, err
	}
	transport.transactional = true
	return transport, nil
}

func (t *RuntimeEventDeliveryHTTPTransport) RuntimeEventDeliveryTransactionsEnabled() bool {
	return t != nil && t.transactional
}

func (t *RuntimeEventDeliveryHTTPTransport) RuntimeEventDeliveryRoute() (string, string) {
	if t == nil {
		return "", ""
	}
	return t.Source, t.Destination
}

// BuildEnvelope exposes the same deterministic identity binding used by the
// legacy Deliver path so a Coordinator can run the optional two-phase flow.
func (t *RuntimeEventDeliveryHTTPTransport) BuildEnvelope(outbox RuntimeEventOutbox, event AgentEvent) (RuntimeEventDeliveryEnvelope, error) {
	return t.BuildEnvelopeAt(outbox, event, time.Time{})
}

// BuildEnvelopeAt is the deterministic construction hook used by tests and
// hosts that own a clock. An explicit issuedAt always requests the v2
// contract; otherwise the transport's opt-in fresh-timestamp setting decides
// whether the current clock is used.
func (t *RuntimeEventDeliveryHTTPTransport) BuildEnvelopeAt(outbox RuntimeEventOutbox, event AgentEvent, issuedAt time.Time) (RuntimeEventDeliveryEnvelope, error) {
	if t == nil {
		return RuntimeEventDeliveryEnvelope{}, fmt.Errorf("%w: transport 为空", ErrInvalidRuntimeEventDelivery)
	}
	if issuedAt.IsZero() && t.freshTimestamp {
		if t.Now != nil {
			issuedAt = t.Now().UTC()
		} else {
			issuedAt = time.Now().UTC()
		}
	}
	if !issuedAt.IsZero() {
		return NewRuntimeEventDeliveryEnvelopeWithIssuedAt(t.Source, t.Destination, outbox, event, issuedAt)
	}
	return NewRuntimeEventDeliveryEnvelope(t.Source, t.Destination, outbox, event)
}

func (t *RuntimeEventDeliveryHTTPTransport) BuildRuntimeEventDeliveryReconciliationEnvelope(outbox RuntimeEventOutbox, event AgentEvent, issuedAt time.Time) (RuntimeEventDeliveryEnvelope, error) {
	return t.BuildEnvelopeAt(outbox, event, issuedAt)
}

func (t *RuntimeEventDeliveryHTTPTransport) Deliver(ctx context.Context, outbox RuntimeEventOutbox, event AgentEvent) error {
	if t == nil {
		return fmt.Errorf("%w: transport 为空", ErrInvalidRuntimeEventDelivery)
	}
	envelope, err := t.BuildEnvelope(outbox, event)
	if err != nil {
		return err
	}
	envelope, err = SignRuntimeEventDeliveryEnvelope(envelope, t.SharedSecret)
	if err != nil {
		return err
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("%w: envelope 编码失败", ErrInvalidRuntimeEventDelivery)
	}
	if len(body) > MaxRuntimeEventDeliveryEnvelopeBytes {
		return fmt.Errorf("%w: envelope 超过 %d 字节上限", ErrInvalidRuntimeEventDelivery, MaxRuntimeEventDeliveryEnvelopeBytes)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Abot-Event-Delivery-Version", strconv.Itoa(envelope.Version))
	request.Header.Set("X-Abot-Event-Delivery-Signature", envelope.Signature)
	request.Header.Set("Idempotency-Key", envelope.EventID)
	response, err := t.Client.Do(request)
	if err != nil {
		return fmt.Errorf("Runtime event delivery 请求失败: %w", err)
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, MaxRuntimeEventDeliveryEnvelopeBytes))
	if readErr != nil {
		return fmt.Errorf("Runtime event delivery 响应读取失败: %w", readErr)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(SanitizeRuntimeString(string(responseBody)))
		if len(message) > 512 {
			message = message[:512]
		}
		if message == "" {
			message = "receiver returned no diagnostic"
		}
		return fmt.Errorf("Runtime event delivery 返回 HTTP %d: %s", response.StatusCode, message)
	}
	return nil
}

// RuntimeEventDeliveryReceiver verifies a signed envelope and delegates
// idempotency to an inbox repository. It never accepts event content from the
// wire, so a receiver cannot accidentally persist a second copy of a prompt
// or command output.
type RuntimeEventDeliveryReceiver struct {
	Source        string
	Destination   string
	SharedSecret  []byte
	Inbox         RuntimeEventDeliveryInbox
	MaxAge        time.Duration
	MaxFutureSkew time.Duration
	MaxBodyBytes  int64
	Now           func() time.Time
}

func NewRuntimeEventDeliveryReceiver(source, destination string, secret []byte, inbox RuntimeEventDeliveryInbox) (*RuntimeEventDeliveryReceiver, error) {
	if strings.TrimSpace(source) == "" || len(strings.TrimSpace(source)) > MaxRuntimeEventDeliverySourceLength || strings.TrimSpace(destination) == "" || len(strings.TrimSpace(destination)) > MaxRuntimeEventDeliveryTargetLength {
		return nil, fmt.Errorf("%w: source/destination 无效", ErrInvalidRuntimeEventDelivery)
	}
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return nil, err
	}
	if inbox == nil {
		return nil, fmt.Errorf("%w: inbox 不能为空", ErrInvalidRuntimeEventDelivery)
	}
	return &RuntimeEventDeliveryReceiver{Source: strings.TrimSpace(source), Destination: strings.TrimSpace(destination), SharedSecret: append([]byte(nil), secret...), Inbox: inbox, MaxAge: DefaultRuntimeEventDeliveryMaxAge, MaxFutureSkew: DefaultRuntimeEventDeliveryFutureSkew, MaxBodyBytes: MaxRuntimeEventDeliveryEnvelopeBytes, Now: time.Now}, nil
}

func (r *RuntimeEventDeliveryReceiver) Handler() http.Handler { return r }

func (r *RuntimeEventDeliveryReceiver) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if r == nil {
		writeRuntimeEventDeliveryError(writer, http.StatusInternalServerError, ErrInvalidRuntimeEventDelivery)
		return
	}
	if request != nil && request.URL != nil && strings.HasSuffix(strings.TrimRight(request.URL.Path, "/"), "/status") {
		r.ServeStatusHTTP(writer, request)
		return
	}
	if request.Method != http.MethodPost {
		writeRuntimeEventDeliveryError(writer, http.StatusMethodNotAllowed, fmt.Errorf("%w: 只支持 POST", ErrInvalidRuntimeEventDelivery))
		return
	}
	maxBody := r.MaxBodyBytes
	if maxBody <= 0 || maxBody > MaxRuntimeEventDeliveryEnvelopeBytes {
		maxBody = MaxRuntimeEventDeliveryEnvelopeBytes
	}
	if request.Body == nil {
		writeRuntimeEventDeliveryError(writer, http.StatusBadRequest, fmt.Errorf("%w: 请求 body 为空", ErrInvalidRuntimeEventDelivery))
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBody+1))
	if err != nil {
		writeRuntimeEventDeliveryError(writer, http.StatusBadRequest, fmt.Errorf("%w: 读取请求失败", ErrInvalidRuntimeEventDelivery))
		return
	}
	if int64(len(body)) > maxBody {
		writeRuntimeEventDeliveryError(writer, http.StatusRequestEntityTooLarge, fmt.Errorf("%w: 请求超过 %d 字节上限", ErrInvalidRuntimeEventDelivery, maxBody))
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var envelope RuntimeEventDeliveryEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		writeRuntimeEventDeliveryError(writer, http.StatusBadRequest, fmt.Errorf("%w: JSON 无效", ErrInvalidRuntimeEventDelivery))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeRuntimeEventDeliveryError(writer, http.StatusBadRequest, fmt.Errorf("%w: 请求包含多个 JSON 文档", ErrInvalidRuntimeEventDelivery))
		return
	}
	normalized, err := envelope.normalize()
	if err != nil {
		writeRuntimeEventDeliveryError(writer, http.StatusBadRequest, err)
		return
	}
	if header := strings.TrimSpace(request.Header.Get("X-Abot-Event-Delivery-Version")); header != "" && header != strconv.Itoa(normalized.Version) {
		writeRuntimeEventDeliveryError(writer, http.StatusBadRequest, fmt.Errorf("%w: header version 与 body 不一致", ErrInvalidRuntimeEventDelivery))
		return
	}
	if header := strings.TrimSpace(strings.ToLower(request.Header.Get("X-Abot-Event-Delivery-Signature"))); header != "" && header != normalized.Signature {
		writeRuntimeEventDeliveryError(writer, http.StatusUnauthorized, fmt.Errorf("%w: header signature 与 body 不一致", ErrRuntimeEventDeliveryAuth))
		return
	}
	if header := strings.TrimSpace(request.Header.Get("Idempotency-Key")); header != "" && header != normalized.EventID {
		writeRuntimeEventDeliveryError(writer, http.StatusBadRequest, fmt.Errorf("%w: idempotency key 与 event_id 不一致", ErrInvalidRuntimeEventDelivery))
		return
	}
	if normalized.Source != r.Source || normalized.Destination != r.Destination {
		writeRuntimeEventDeliveryError(writer, http.StatusForbidden, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeEventDeliveryAuth))
		return
	}
	if err := VerifyRuntimeEventDeliveryEnvelope(normalized, r.SharedSecret); err != nil {
		writeRuntimeEventDeliveryError(writer, http.StatusUnauthorized, err)
		return
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	if err := validateRuntimeEventDeliveryTimestamp(RuntimeEventDeliveryReplayTimestamp(normalized), now, r.MaxAge, r.MaxFutureSkew); err != nil {
		writeRuntimeEventDeliveryError(writer, http.StatusUnauthorized, err)
		return
	}
	duplicate, err := r.Inbox.AcceptRuntimeEventDelivery(request.Context(), normalized)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			writeRuntimeEventDeliveryError(writer, http.StatusConflict, err)
			return
		}
		writeRuntimeEventDeliveryError(writer, http.StatusInternalServerError, err)
		return
	}
	writeRuntimeEventDeliveryJSON(writer, http.StatusOK, map[string]any{"accepted": true, "duplicate": duplicate, "event_id": normalized.EventID})
}

func writeRuntimeEventDeliveryError(writer http.ResponseWriter, status int, err error) {
	message := "Runtime event delivery 请求失败"
	if err != nil {
		message = strings.TrimSpace(SanitizeRuntimeString(err.Error()))
	}
	if len(message) > 512 {
		message = message[:512]
	}
	if message == "" {
		message = "Runtime event delivery 请求失败"
	}
	writeRuntimeEventDeliveryJSON(writer, status, map[string]any{"error": message})
}

func writeRuntimeEventDeliveryJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
