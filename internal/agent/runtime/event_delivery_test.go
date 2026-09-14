package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testRuntimeEventDeliverySecret = "0123456789abcdef0123456789abcdef"

func runtimeEventDeliveryFixture(t *testing.T) (AgentEvent, RuntimeEventOutbox, time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	event := AgentEvent{
		ID:           "delivery-event-1",
		InvocationID: "delivery-invocation-1",
		Sequence:     1,
		Type:         EventAssistantMessage,
		Timestamp:    now,
		Data: map[string]any{
			"text":    "top-secret prompt should stay in the source event log",
			"api_key": "top-secret-api-key",
		},
	}
	outbox, err := NewRuntimeEventOutbox(event)
	if err != nil {
		t.Fatal(err)
	}
	return event, outbox, now
}

func TestRuntimeEventDeliveryEnvelopeSignsAndRejectsTampering(t *testing.T) {
	event, outbox, now := runtimeEventDeliveryFixture(t)
	envelope, err := NewRuntimeEventDeliveryEnvelope("runtime-a", "audit-b", outbox, event)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Signature != "" || envelope.EventDigest == "" || envelope.Timestamp != now {
		t.Fatalf("unsigned envelope metadata 错误: %#v", envelope)
	}
	if err := envelope.Validate(); err != nil {
		t.Fatalf("unsigned envelope 应通过 metadata 校验: %v", err)
	}
	wantDigest, err := RuntimeEventDeliveryEventDigest(event)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.EventDigest != wantDigest || strings.Contains(envelope.EventDigest, "top-secret") {
		t.Fatalf("event digest 不应暴露 event body: %#v", envelope)
	}

	signed, err := SignRuntimeEventDeliveryEnvelope(envelope, []byte(testRuntimeEventDeliverySecret))
	if err != nil {
		t.Fatal(err)
	}
	if len(signed.Signature) != 64 || signed.Signature != strings.ToLower(signed.Signature) {
		t.Fatalf("signature 应为 lowercase SHA-256 HMAC: %q", signed.Signature)
	}
	if err := VerifyRuntimeEventDeliveryEnvelope(signed, []byte(testRuntimeEventDeliverySecret)); err != nil {
		t.Fatalf("签名 envelope 应验证通过: %v", err)
	}
	if err := VerifyRuntimeEventDeliveryEnvelope(signed, []byte("wrong-secret-0123456789")); !errors.Is(err, ErrRuntimeEventDeliveryAuth) {
		t.Fatalf("错误 shared secret 应返回认证错误: %v", err)
	}

	tampered := signed
	tampered.Sequence++
	if err := VerifyRuntimeEventDeliveryEnvelope(tampered, []byte(testRuntimeEventDeliverySecret)); !errors.Is(err, ErrRuntimeEventDeliveryAuth) {
		t.Fatalf("篡改 sequence 应破坏 HMAC: %v", err)
	}
	missingSignature := envelope
	if err := VerifyRuntimeEventDeliveryEnvelope(missingSignature, []byte(testRuntimeEventDeliverySecret)); !errors.Is(err, ErrRuntimeEventDeliveryAuth) {
		t.Fatalf("缺失 signature 应返回认证错误: %v", err)
	}
	if _, err := SignRuntimeEventDeliveryEnvelope(envelope, []byte("too-short")); !errors.Is(err, ErrInvalidRuntimeEventDelivery) {
		t.Fatalf("过短 shared secret 应被拒绝: %v", err)
	}

	invalid := envelope
	invalid.EventID = strings.Repeat("x", MaxRuntimeEventDeliveryEventIDLength+1)
	if err := invalid.Validate(); !errors.Is(err, ErrInvalidRuntimeEventDelivery) {
		t.Fatalf("过长 event id 应被拒绝: %v", err)
	}
}

func TestRuntimeEventDeliveryIssuedAtV2PreservesLegacyEnvelopeSemantics(t *testing.T) {
	event, outbox, now := runtimeEventDeliveryFixture(t)
	legacy, err := NewRuntimeEventDeliveryEnvelope("runtime-a", "audit-b", outbox, event)
	if err != nil {
		t.Fatal(err)
	}
	if got := RuntimeEventDeliveryReplayTimestamp(legacy); !got.Equal(event.Timestamp) {
		t.Fatalf("v1 replay timestamp 必须继续使用事件时间: got=%v want=%v", got, event.Timestamp)
	}

	fresh, err := NewRuntimeEventDeliveryEnvelopeWithIssuedAt("runtime-a", "audit-b", outbox, event, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Version != RuntimeEventDeliveryEnvelopeVersionIssuedAt || !fresh.Timestamp.Equal(event.Timestamp) || !fresh.IssuedAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("v2 必须同时保留 immutable timestamp 和 issued_at: %#v", fresh)
	}
	signed, err := SignRuntimeEventDeliveryEnvelope(fresh, []byte(testRuntimeEventDeliverySecret))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRuntimeEventDeliveryEnvelope(signed, []byte(testRuntimeEventDeliverySecret)); err != nil {
		t.Fatalf("v2 签名应验证通过: %v", err)
	}
	if got := RuntimeEventDeliveryReplayTimestamp(signed); !got.Equal(now.Add(time.Hour)) {
		t.Fatalf("v2 replay timestamp 必须使用 issued_at: %v", got)
	}

	invalidV1 := legacy
	invalidV1.IssuedAt = now
	if err := invalidV1.Validate(); !errors.Is(err, ErrInvalidRuntimeEventDelivery) {
		t.Fatalf("v1 携带 issued_at 应拒绝: %v", err)
	}
	invalidV2 := fresh
	invalidV2.IssuedAt = time.Time{}
	if err := invalidV2.Validate(); !errors.Is(err, ErrInvalidRuntimeEventDelivery) {
		t.Fatalf("v2 缺失 issued_at 应拒绝: %v", err)
	}

	refreshed, err := RefreshRuntimeEventDeliveryEnvelope(legacy, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Version != RuntimeEventDeliveryEnvelopeVersionIssuedAt || refreshed.Signature != "" || !refreshed.Timestamp.Equal(legacy.Timestamp) || !refreshed.IssuedAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("刷新 v1 envelope 不应改变事件身份: %#v", refreshed)
	}
}

type runtimeEventDeliveryRoundTripper struct {
	status       int
	responseBody string
	request      *http.Request
	body         []byte
}

func (t *runtimeEventDeliveryRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	t.request = request
	var err error
	t.body, err = io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: t.status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(t.responseBody)),
		Request:    request,
	}, nil
}

func TestRuntimeEventDeliveryHTTPTransportSendsOnlySignedMetadata(t *testing.T) {
	event, outbox, _ := runtimeEventDeliveryFixture(t)
	transportSpy := &runtimeEventDeliveryRoundTripper{status: http.StatusAccepted}
	transport, err := NewRuntimeEventDeliveryHTTPTransport("https://receiver.example.test/v1/events", "runtime-a", "audit-b", []byte(testRuntimeEventDeliverySecret), &http.Client{Transport: transportSpy})
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.Deliver(context.Background(), outbox, event); err != nil {
		t.Fatal(err)
	}
	if transportSpy.request == nil || transportSpy.request.Method != http.MethodPost {
		t.Fatalf("transport 应发送 POST: %#v", transportSpy.request)
	}
	if got := transportSpy.request.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type=%q", got)
	}
	if got := transportSpy.request.Header.Get("Idempotency-Key"); got != event.ID {
		t.Fatalf("Idempotency-Key=%q", got)
	}
	var envelope RuntimeEventDeliveryEnvelope
	if err := json.Unmarshal(transportSpy.body, &envelope); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRuntimeEventDeliveryEnvelope(envelope, []byte(testRuntimeEventDeliverySecret)); err != nil {
		t.Fatalf("transport body signature 无效: %v", err)
	}
	if envelope.Source != "runtime-a" || envelope.Destination != "audit-b" || envelope.EventID != event.ID {
		t.Fatalf("transport envelope metadata 错误: %#v", envelope)
	}
	if bytes.Contains(transportSpy.body, []byte("top-secret")) || bytes.Contains(transportSpy.body, []byte("api_key")) {
		t.Fatalf("transport body 不应包含 event 正文或 credential 字段: %s", transportSpy.body)
	}
	if transportSpy.request.Header.Get("X-Abot-Event-Delivery-Signature") != envelope.Signature {
		t.Fatal("header signature 必须与 body 一致")
	}

	transportSpy.status = http.StatusBadGateway
	transportSpy.responseBody = "api_key=receiver-secret"
	err = transport.Deliver(context.Background(), outbox, event)
	if err == nil || strings.Contains(err.Error(), "receiver-secret") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("非 2xx 响应应返回脱敏错误: %v", err)
	}
	if _, err := NewRuntimeEventDeliveryHTTPTransport("file:///tmp/events", "runtime-a", "audit-b", []byte(testRuntimeEventDeliverySecret), nil); !errors.Is(err, ErrInvalidRuntimeEventDelivery) {
		t.Fatalf("非 http(s) endpoint 应被拒绝: %v", err)
	}

	badOutbox := outbox
	badOutbox.EventID = "different-event"
	transportSpy.status = http.StatusAccepted
	if err := transport.Deliver(context.Background(), badOutbox, event); !errors.Is(err, ErrInvalidRuntimeEventDelivery) {
		t.Fatalf("outbox/event metadata 不匹配应在发送前拒绝: %v", err)
	}
	badOutbox = outbox
	badOutbox.ID = "forged-delivery-id"
	if err := transport.Deliver(context.Background(), badOutbox, event); !errors.Is(err, ErrInvalidRuntimeEventDelivery) {
		t.Fatalf("伪造 delivery cursor ID 应在发送前拒绝: %v", err)
	}
}

func TestRuntimeEventDeliveryIssuedAtTransportDeliversDelayedEvent(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	invocation := eventOutboxInvocation(t, repo, "inv-event-delayed")
	event, err := repo.AppendEvent(ctx, AgentEvent{
		ID: "event-delayed", InvocationID: invocation.ID, Type: EventRuntimeNotice,
		Timestamp: now.Add(-2 * time.Hour), Data: map[string]any{"message": "delayed event"},
	})
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := repo.GetRuntimeEventOutbox(ctx, eventOutboxIDForTest(event.ID))
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewRuntimeEventDeliveryReceiver("runtime-a", "audit-b", []byte(testRuntimeEventDeliverySecret), repo)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now }
	receiver.MaxAge = time.Minute
	server := httptest.NewServer(receiver)
	defer server.Close()

	legacy, err := NewRuntimeEventDeliveryHTTPTransport(server.URL, "runtime-a", "audit-b", []byte(testRuntimeEventDeliverySecret), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	legacy.Now = func() time.Time { return now }
	if err := legacy.Deliver(ctx, outbox, event); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("旧 v1 transport 对滞留事件应被 replay window 拒绝: %v", err)
	}

	fresh, err := NewRuntimeEventDeliveryHTTPTransportWithIssuedAt(server.URL, "runtime-a", "audit-b", []byte(testRuntimeEventDeliverySecret), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	fresh.Now = func() time.Time { return now }
	if err := fresh.Deliver(ctx, outbox, event); err != nil {
		t.Fatalf("v2 issued_at transport 应允许安全投递滞留事件: %v", err)
	}
	repo.mu.Lock()
	stored, ok := repo.eventInbox[event.ID]
	repo.mu.Unlock()
	if !ok || stored.Version != RuntimeEventDeliveryEnvelopeVersionIssuedAt || !stored.Timestamp.Equal(event.Timestamp) || !stored.IssuedAt.Equal(now) {
		t.Fatalf("receiver 应保存 immutable timestamp 与 v2 issued_at metadata: ok=%v row=%#v", ok, stored)
	}

	refreshed, err := fresh.BuildEnvelopeAt(outbox, event, now.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err = SignRuntimeEventDeliveryEnvelope(refreshed, []byte(testRuntimeEventDeliverySecret))
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := repo.AcceptRuntimeEventDelivery(ctx, refreshed)
	if err != nil || !duplicate {
		t.Fatalf("v2 issued_at 变化只应作为同一 immutable event 的幂等重放: duplicate=%v err=%v", duplicate, err)
	}
}

func signedRuntimeEventDeliveryEnvelope(t *testing.T, source, destination string, event AgentEvent, outbox RuntimeEventOutbox) RuntimeEventDeliveryEnvelope {
	t.Helper()
	envelope, err := NewRuntimeEventDeliveryEnvelope(source, destination, outbox, event)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := SignRuntimeEventDeliveryEnvelope(envelope, []byte(testRuntimeEventDeliverySecret))
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func deliveryRequest(t *testing.T, envelope RuntimeEventDeliveryEnvelope) *http.Request {
	t.Helper()
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/events", bytes.NewReader(body))
	request.Header.Set("X-Abot-Event-Delivery-Version", "1")
	request.Header.Set("X-Abot-Event-Delivery-Signature", envelope.Signature)
	request.Header.Set("Idempotency-Key", envelope.EventID)
	return request
}

func TestRuntimeEventDeliveryReceiverAuthenticatesAndDeduplicates(t *testing.T) {
	event, outbox, now := runtimeEventDeliveryFixture(t)
	inbox := NewMemoryRepository()
	receiver, err := NewRuntimeEventDeliveryReceiver("runtime-a", "audit-b", []byte(testRuntimeEventDeliverySecret), inbox)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now.Add(time.Minute) }
	receiver.MaxAge = 2 * time.Minute
	receiver.MaxFutureSkew = 5 * time.Second

	envelope := signedRuntimeEventDeliveryEnvelope(t, "runtime-a", "audit-b", event, outbox)
	response := httptest.NewRecorder()
	receiver.ServeHTTP(response, deliveryRequest(t, envelope))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"duplicate":false`) {
		t.Fatalf("首次投递应接受且非 duplicate: status=%d body=%s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	receiver.ServeHTTP(response, deliveryRequest(t, envelope))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"duplicate":true`) {
		t.Fatalf("重复投递应幂等: status=%d body=%s", response.Code, response.Body.String())
	}

	tampered := envelope
	if tampered.Signature[len(tampered.Signature)-1] == '0' {
		tampered.Signature = tampered.Signature[:len(tampered.Signature)-1] + "1"
	} else {
		tampered.Signature = tampered.Signature[:len(tampered.Signature)-1] + "0"
	}
	response = httptest.NewRecorder()
	receiver.ServeHTTP(response, deliveryRequest(t, tampered))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("篡改 signature 应返回 401，得到 %d", response.Code)
	}
	mismatchedKeyRequest := deliveryRequest(t, envelope)
	mismatchedKeyRequest.Header.Set("Idempotency-Key", "different-event")
	response = httptest.NewRecorder()
	receiver.ServeHTTP(response, mismatchedKeyRequest)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("不一致 Idempotency-Key 应返回 400，得到 %d", response.Code)
	}

	wrongRoute := signedRuntimeEventDeliveryEnvelope(t, "other-runtime", "audit-b", event, outbox)
	response = httptest.NewRecorder()
	receiver.ServeHTTP(response, deliveryRequest(t, wrongRoute))
	if response.Code != http.StatusForbidden {
		t.Fatalf("source 不匹配应返回 403，得到 %d", response.Code)
	}

	stale := envelope
	stale.Timestamp = now.Add(-3 * time.Minute)
	stale, err = SignRuntimeEventDeliveryEnvelope(stale, []byte(testRuntimeEventDeliverySecret))
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	receiver.ServeHTTP(response, deliveryRequest(t, stale))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("过期 timestamp 应返回 401，得到 %d", response.Code)
	}

	conflict := envelope
	conflict.Sequence++
	conflict, err = SignRuntimeEventDeliveryEnvelope(conflict, []byte(testRuntimeEventDeliverySecret))
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	receiver.ServeHTTP(response, deliveryRequest(t, conflict))
	if response.Code != http.StatusConflict {
		t.Fatalf("同 EventID 不同 metadata 应返回 409，得到 %d", response.Code)
	}

	unknownField := map[string]any{}
	if err := json.Unmarshal(mustJSON(t, envelope), &unknownField); err != nil {
		t.Fatal(err)
	}
	unknownField["unexpected"] = true
	unknownFieldBody := mustJSON(t, unknownField)
	response = httptest.NewRecorder()
	receiver.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/events", bytes.NewReader(unknownFieldBody)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("未知 JSON 字段应返回 400，得到 %d", response.Code)
	}
	response = httptest.NewRecorder()
	receiver.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/events", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("非 POST 应返回 405，得到 %d", response.Code)
	}

	future := envelope
	future.Timestamp = now.Add(2 * time.Minute)
	future, err = SignRuntimeEventDeliveryEnvelope(future, []byte(testRuntimeEventDeliverySecret))
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	receiver.ServeHTTP(response, deliveryRequest(t, future))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("超出 future skew 应返回 401，得到 %d", response.Code)
	}
	versionMismatch := deliveryRequest(t, envelope)
	versionMismatch.Header.Set("X-Abot-Event-Delivery-Version", "2")
	response = httptest.NewRecorder()
	receiver.ServeHTTP(response, versionMismatch)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("header version 不一致应返回 400，得到 %d", response.Code)
	}
	limited := *receiver
	limited.MaxBodyBytes = 64
	response = httptest.NewRecorder()
	limited.ServeHTTP(response, deliveryRequest(t, envelope))
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超过 receiver body 上限应返回 413，得到 %d", response.Code)
	}
}

type runtimeEventDeliveryTransportFunc func(context.Context, RuntimeEventOutbox, AgentEvent) error

func (f runtimeEventDeliveryTransportFunc) Deliver(ctx context.Context, outbox RuntimeEventOutbox, event AgentEvent) error {
	return f(ctx, outbox, event)
}

func TestCoordinatorAdaptsRuntimeEventDeliveryTransportToOutboxLoop(t *testing.T) {
	repo := NewMemoryRepository()
	invocation := eventOutboxInvocation(t, repo, "inv-event-delivery-adapter")
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	event, err := repo.AppendEvent(context.Background(), AgentEvent{ID: "event-delivery-adapter", InvocationID: invocation.ID, Type: EventRuntimeNotice, Timestamp: now})
	if err != nil {
		t.Fatal(err)
	}
	var delivered RuntimeEventDeliveryEnvelope
	coordinator := &Coordinator{repo: repo, eventOutboxWorkerID: "adapter-worker"}
	coordinator.SetRuntimeEventDeliveryTransport(runtimeEventDeliveryTransportFunc(func(_ context.Context, outbox RuntimeEventOutbox, got AgentEvent) error {
		var signErr error
		delivered, signErr = NewRuntimeEventDeliveryEnvelope("runtime-a", "audit-b", outbox, got)
		return signErr
	}))
	coordinator.dispatchDueRuntimeEventOutbox(context.Background(), now.Add(time.Second))
	if delivered.EventID != event.ID || delivered.DeliveryID == "" {
		t.Fatalf("Coordinator 应将 outbox event 交给 transport adapter: %#v", delivered)
	}
	item, err := repo.GetRuntimeEventOutbox(context.Background(), delivered.DeliveryID)
	if err != nil || item.Status != RuntimeEventOutboxCompleted {
		t.Fatalf("transport 成功后 outbox 应完成: item=%#v err=%v", item, err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
