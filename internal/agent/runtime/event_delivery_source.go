package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// The source response contains one sanitized AgentEvent in addition to a
	// small metadata envelope. Keep it bounded independently from the signed
	// delivery envelope so a source cannot accidentally turn this endpoint into
	// an unbounded event export API.
	MaxRuntimeEventDeliverySourceResponseBytes = MaxRuntimeEventDeliveryDigestBytes + 16*1024
)

// RuntimeEventDeliverySourceResponse is the authenticated source-side
// response for a metadata envelope. The request still carries only the
// immutable event identity and digest; the response is read only after the
// source has authenticated the same envelope and looked up EventID locally.
// The event is sanitized before it crosses the boundary.
type RuntimeEventDeliverySourceResponse struct {
	Version     int        `json:"version"`
	DeliveryID  string     `json:"delivery_id"`
	EventID     string     `json:"event_id"`
	EventDigest string     `json:"event_digest"`
	Event       AgentEvent `json:"event"`
}

// RuntimeEventDeliverySource exposes a narrowly scoped authenticated event
// lookup endpoint. It is intentionally separate from the ingestion receiver:
// a destination may accept a metadata envelope without ever receiving event
// contents, while an explicitly authorized consumer can fetch one immutable
// event by its signed EventID and verify the digest locally.
type RuntimeEventDeliverySource struct {
	Source        string
	Destination   string
	SharedSecret  []byte
	Events        AgentEventRepository
	MaxAge        time.Duration
	MaxFutureSkew time.Duration
	MaxBodyBytes  int64
	MaxResponse   int64
	Now           func() time.Time
}

func NewRuntimeEventDeliverySource(source, destination string, secret []byte, events AgentEventRepository) (*RuntimeEventDeliverySource, error) {
	if strings.TrimSpace(source) == "" || len(strings.TrimSpace(source)) > MaxRuntimeEventDeliverySourceLength || strings.TrimSpace(destination) == "" || len(strings.TrimSpace(destination)) > MaxRuntimeEventDeliveryTargetLength {
		return nil, fmt.Errorf("%w: source/destination 无效", ErrInvalidRuntimeEventDelivery)
	}
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return nil, err
	}
	if events == nil {
		return nil, fmt.Errorf("%w: event repository 不能为空", ErrInvalidRuntimeEventDelivery)
	}
	return &RuntimeEventDeliverySource{
		Source: strings.TrimSpace(source), Destination: strings.TrimSpace(destination),
		SharedSecret: append([]byte(nil), secret...), Events: events,
		MaxAge: DefaultRuntimeEventDeliveryMaxAge, MaxFutureSkew: DefaultRuntimeEventDeliveryFutureSkew,
		MaxBodyBytes: MaxRuntimeEventDeliveryEnvelopeBytes, MaxResponse: MaxRuntimeEventDeliverySourceResponseBytes,
		Now: time.Now,
	}, nil
}

func (s *RuntimeEventDeliverySource) Handler() http.Handler { return s }

func (s *RuntimeEventDeliverySource) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if s == nil {
		writeRuntimeEventDeliveryError(writer, http.StatusInternalServerError, ErrInvalidRuntimeEventDelivery)
		return
	}
	if request.Method != http.MethodPost {
		writeRuntimeEventDeliveryError(writer, http.StatusMethodNotAllowed, fmt.Errorf("%w: 只支持 POST", ErrInvalidRuntimeEventDelivery))
		return
	}
	envelope, err := readRuntimeEventDeliveryEnvelope(request, s.MaxBodyBytes)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrRuntimeEventDeliveryAuth) {
			status = http.StatusUnauthorized
		}
		writeRuntimeEventDeliveryError(writer, status, err)
		return
	}
	if envelope.Source != s.Source || envelope.Destination != s.Destination {
		writeRuntimeEventDeliveryError(writer, http.StatusForbidden, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeEventDeliveryAuth))
		return
	}
	if err := VerifyRuntimeEventDeliveryEnvelope(envelope, s.SharedSecret); err != nil {
		writeRuntimeEventDeliveryError(writer, http.StatusUnauthorized, err)
		return
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	if err := validateRuntimeEventDeliveryTimestamp(RuntimeEventDeliveryReplayTimestamp(envelope), now, s.MaxAge, s.MaxFutureSkew); err != nil {
		writeRuntimeEventDeliveryError(writer, http.StatusUnauthorized, err)
		return
	}
	event, err := s.Events.GetAgentEvent(request.Context(), envelope.EventID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Do not reveal whether arbitrary EventIDs exist to an unauthorized or
			// misrouted caller. The envelope was authenticated, so 404 is safe for
			// a valid but already-purged event.
			writeRuntimeEventDeliveryError(writer, http.StatusNotFound, ErrNotFound)
			return
		}
		writeRuntimeEventDeliveryError(writer, http.StatusInternalServerError, err)
		return
	}
	event = SanitizeAgentEvent(event)
	if strings.TrimSpace(event.ID) != envelope.EventID || strings.TrimSpace(event.InvocationID) != envelope.InvocationID || event.Sequence != envelope.Sequence || strings.TrimSpace(event.Type) != envelope.Type || !event.Timestamp.UTC().Equal(envelope.Timestamp.UTC()) {
		writeRuntimeEventDeliveryError(writer, http.StatusConflict, fmt.Errorf("%w: source event metadata 与 envelope 不一致", ErrRuntimeEventDeliveryAuth))
		return
	}
	digest, err := RuntimeEventDeliveryEventDigest(event)
	if err != nil {
		writeRuntimeEventDeliveryError(writer, http.StatusInternalServerError, err)
		return
	}
	if digest != envelope.EventDigest {
		writeRuntimeEventDeliveryError(writer, http.StatusConflict, fmt.Errorf("%w: source event digest 不一致", ErrRuntimeEventDeliveryAuth))
		return
	}
	response := RuntimeEventDeliverySourceResponse{Version: envelope.Version, DeliveryID: envelope.DeliveryID, EventID: envelope.EventID, EventDigest: digest, Event: event}
	body, err := json.Marshal(response)
	if err != nil {
		writeRuntimeEventDeliveryError(writer, http.StatusInternalServerError, fmt.Errorf("%w: source response 编码失败", ErrInvalidRuntimeEventDelivery))
		return
	}
	maxResponse := s.MaxResponse
	if maxResponse <= 0 || maxResponse > MaxRuntimeEventDeliverySourceResponseBytes {
		maxResponse = MaxRuntimeEventDeliverySourceResponseBytes
	}
	if int64(len(body)) > maxResponse {
		writeRuntimeEventDeliveryError(writer, http.StatusRequestEntityTooLarge, fmt.Errorf("%w: source response 超过 %d 字节上限", ErrInvalidRuntimeEventDelivery, maxResponse))
		return
	}
	writeRuntimeEventDeliveryJSON(writer, http.StatusOK, response)
}

// RuntimeEventDeliveryHTTPSourceClient fetches one event from an explicitly
// configured source. It re-signs the envelope instead of trusting a caller's
// signature, then verifies response identity and digest before returning the
// sanitized event to the consumer.
type RuntimeEventDeliveryHTTPSourceClient struct {
	Endpoint     string
	Source       string
	Destination  string
	SharedSecret []byte
	Client       *http.Client
	MaxBodyBytes int64
	// Now is injectable for deterministic tests and explicit host clocks. It
	// is used by the opt-in v2 refresh path only.
	Now            func() time.Time
	freshTimestamp bool
}

func NewRuntimeEventDeliveryHTTPSourceClient(endpoint, source, destination string, secret []byte, client *http.Client) (*RuntimeEventDeliveryHTTPSourceClient, error) {
	transport, err := NewRuntimeEventDeliveryHTTPTransport(endpoint, source, destination, secret, client)
	if err != nil {
		return nil, err
	}
	return &RuntimeEventDeliveryHTTPSourceClient{
		Endpoint: transport.Endpoint, Source: transport.Source, Destination: transport.Destination,
		SharedSecret: append([]byte(nil), transport.SharedSecret...), Client: transport.Client,
		MaxBodyBytes: MaxRuntimeEventDeliverySourceResponseBytes, Now: time.Now,
	}, nil
}

// NewRuntimeEventDeliveryHTTPSourceClientWithIssuedAt is the explicit v2
// source/client counterpart. It refreshes the signed replay timestamp while
// keeping the immutable event timestamp and digest unchanged.
func NewRuntimeEventDeliveryHTTPSourceClientWithIssuedAt(endpoint, source, destination string, secret []byte, client *http.Client) (*RuntimeEventDeliveryHTTPSourceClient, error) {
	result, err := NewRuntimeEventDeliveryHTTPSourceClient(endpoint, source, destination, secret, client)
	if err != nil {
		return nil, err
	}
	result.freshTimestamp = true
	return result, nil
}

func (c *RuntimeEventDeliveryHTTPSourceClient) FetchEvent(ctx context.Context, envelope RuntimeEventDeliveryEnvelope) (AgentEvent, error) {
	if c == nil {
		return AgentEvent{}, fmt.Errorf("%w: source client 为空", ErrInvalidRuntimeEventDelivery)
	}
	normalized, err := NormalizeRuntimeEventDeliveryEnvelope(envelope)
	if err != nil {
		return AgentEvent{}, err
	}
	if normalized.Source != c.Source || normalized.Destination != c.Destination {
		return AgentEvent{}, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeEventDeliveryAuth)
	}
	if c.freshTimestamp {
		issuedAt := time.Now().UTC()
		if c.Now != nil {
			issuedAt = c.Now().UTC()
		}
		normalized, err = RefreshRuntimeEventDeliveryEnvelope(normalized, issuedAt)
		if err != nil {
			return AgentEvent{}, err
		}
	}
	// Re-sign the canonical metadata. This lets a caller pass an unsigned
	// envelope from a durable outbox while ensuring the wire signature covers
	// exactly the fields that this client validated.
	signed, err := SignRuntimeEventDeliveryEnvelope(normalized, c.SharedSecret)
	if err != nil {
		return AgentEvent{}, err
	}
	body, err := json.Marshal(signed)
	if err != nil {
		return AgentEvent{}, fmt.Errorf("%w: source request 编码失败", ErrInvalidRuntimeEventDelivery)
	}
	if len(body) > MaxRuntimeEventDeliveryEnvelopeBytes {
		return AgentEvent{}, fmt.Errorf("%w: source request 超过 %d 字节上限", ErrInvalidRuntimeEventDelivery, MaxRuntimeEventDeliveryEnvelopeBytes)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return AgentEvent{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Abot-Event-Delivery-Version", strconv.Itoa(signed.Version))
	request.Header.Set("X-Abot-Event-Delivery-Signature", signed.Signature)
	request.Header.Set("Idempotency-Key", signed.EventID)
	response, err := c.Client.Do(request)
	if err != nil {
		return AgentEvent{}, fmt.Errorf("Runtime event source 请求失败: %w", err)
	}
	defer response.Body.Close()
	maxBody := c.MaxBodyBytes
	if maxBody <= 0 || maxBody > MaxRuntimeEventDeliverySourceResponseBytes {
		maxBody = MaxRuntimeEventDeliverySourceResponseBytes
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return AgentEvent{}, fmt.Errorf("Runtime event source 响应读取失败: %w", err)
	}
	if int64(len(responseBody)) > maxBody {
		return AgentEvent{}, fmt.Errorf("%w: source response 超过 %d 字节上限", ErrInvalidRuntimeEventDelivery, maxBody)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(SanitizeRuntimeString(string(responseBody)))
		if len(message) > 512 {
			message = message[:512]
		}
		if message == "" {
			message = "source returned no diagnostic"
		}
		return AgentEvent{}, fmt.Errorf("Runtime event source 返回 HTTP %d: %s", response.StatusCode, message)
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	var payload RuntimeEventDeliverySourceResponse
	if err := decoder.Decode(&payload); err != nil {
		return AgentEvent{}, fmt.Errorf("%w: source response JSON 无效", ErrInvalidRuntimeEventDelivery)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return AgentEvent{}, fmt.Errorf("%w: source response 包含多个 JSON 文档", ErrInvalidRuntimeEventDelivery)
	}
	if payload.Version != signed.Version || strings.TrimSpace(payload.DeliveryID) != signed.DeliveryID || strings.TrimSpace(payload.EventID) != signed.EventID || strings.TrimSpace(strings.ToLower(payload.EventDigest)) != signed.EventDigest {
		return AgentEvent{}, fmt.Errorf("%w: source response metadata 不一致", ErrRuntimeEventDeliveryAuth)
	}
	event := SanitizeAgentEvent(payload.Event)
	if strings.TrimSpace(event.ID) != signed.EventID || strings.TrimSpace(event.InvocationID) != signed.InvocationID || event.Sequence != signed.Sequence || strings.TrimSpace(event.Type) != signed.Type || !event.Timestamp.UTC().Equal(signed.Timestamp.UTC()) {
		return AgentEvent{}, fmt.Errorf("%w: source response event metadata 不一致", ErrRuntimeEventDeliveryAuth)
	}
	digest, err := RuntimeEventDeliveryEventDigest(event)
	if err != nil {
		return AgentEvent{}, err
	}
	if digest != signed.EventDigest {
		return AgentEvent{}, fmt.Errorf("%w: source response event digest 不一致", ErrRuntimeEventDeliveryAuth)
	}
	return event, nil
}

// readRuntimeEventDeliveryEnvelope performs the common bounded, strict JSON
// and header consistency checks shared by source and receiver endpoints.
func readRuntimeEventDeliveryEnvelope(request *http.Request, configuredMax int64) (RuntimeEventDeliveryEnvelope, error) {
	if request == nil || request.Body == nil {
		return RuntimeEventDeliveryEnvelope{}, fmt.Errorf("%w: 请求 body 为空", ErrInvalidRuntimeEventDelivery)
	}
	maxBody := configuredMax
	if maxBody <= 0 || maxBody > MaxRuntimeEventDeliveryEnvelopeBytes {
		maxBody = MaxRuntimeEventDeliveryEnvelopeBytes
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBody+1))
	if err != nil {
		return RuntimeEventDeliveryEnvelope{}, fmt.Errorf("%w: 读取请求失败", ErrInvalidRuntimeEventDelivery)
	}
	if int64(len(body)) > maxBody {
		return RuntimeEventDeliveryEnvelope{}, fmt.Errorf("%w: 请求超过 %d 字节上限", ErrInvalidRuntimeEventDelivery, maxBody)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var envelope RuntimeEventDeliveryEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return RuntimeEventDeliveryEnvelope{}, fmt.Errorf("%w: JSON 无效", ErrInvalidRuntimeEventDelivery)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeEventDeliveryEnvelope{}, fmt.Errorf("%w: 请求包含多个 JSON 文档", ErrInvalidRuntimeEventDelivery)
	}
	normalized, err := NormalizeRuntimeEventDeliveryEnvelope(envelope)
	if err != nil {
		return RuntimeEventDeliveryEnvelope{}, err
	}
	if header := strings.TrimSpace(request.Header.Get("X-Abot-Event-Delivery-Version")); header != "" && header != strconv.Itoa(normalized.Version) {
		return RuntimeEventDeliveryEnvelope{}, fmt.Errorf("%w: header version 与 body 不一致", ErrInvalidRuntimeEventDelivery)
	}
	if header := strings.TrimSpace(strings.ToLower(request.Header.Get("X-Abot-Event-Delivery-Signature"))); header != "" && header != normalized.Signature {
		return RuntimeEventDeliveryEnvelope{}, fmt.Errorf("%w: header signature 与 body 不一致", ErrRuntimeEventDeliveryAuth)
	}
	if header := strings.TrimSpace(request.Header.Get("Idempotency-Key")); header != "" && header != normalized.EventID {
		return RuntimeEventDeliveryEnvelope{}, fmt.Errorf("%w: idempotency key 与 event_id 不一致", ErrInvalidRuntimeEventDelivery)
	}
	return normalized, nil
}
