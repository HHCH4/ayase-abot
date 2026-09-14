package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRuntimeEventDeliverySourceAuthenticatesAndReturnsSanitizedEvent(t *testing.T) {
	repo := NewMemoryRepository()
	invocation := eventOutboxInvocation(t, repo, "inv-event-source-memory")
	now := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	event, err := repo.AppendEvent(context.Background(), AgentEvent{
		ID: "event-source-memory", InvocationID: invocation.ID, Type: EventRuntimeNotice, Timestamp: now,
		Data: map[string]any{"code": "source-fetch", "api_key": "source-secret", "text": "authorized body"},
	})
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := repo.GetRuntimeEventOutbox(context.Background(), eventOutboxIDForTest(event.ID))
	if err != nil {
		t.Fatal(err)
	}
	envelope := signedRuntimeEventDeliveryEnvelope(t, "runtime-a", "audit-b", event, outbox)
	source, err := NewRuntimeEventDeliverySource("runtime-a", "audit-b", []byte(testRuntimeEventDeliverySecret), repo)
	if err != nil {
		t.Fatal(err)
	}
	source.Now = func() time.Time { return now.Add(time.Minute) }
	source.MaxAge = 2 * time.Minute
	source.MaxFutureSkew = 5 * time.Second
	response := httptest.NewRecorder()
	source.ServeHTTP(response, deliveryRequest(t, envelope))
	if response.Code != http.StatusOK {
		t.Fatalf("合法 source fetch 应成功: status=%d body=%s", response.Code, response.Body.String())
	}
	var payload RuntimeEventDeliverySourceResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Version != envelope.Version || payload.DeliveryID != envelope.DeliveryID || payload.EventID != event.ID || payload.EventDigest != envelope.EventDigest {
		t.Fatalf("source response metadata 错误: %#v", payload)
	}
	if payload.Event.ID != event.ID || payload.Event.Sequence != event.Sequence || payload.Event.InvocationID != event.InvocationID {
		t.Fatalf("source response event identity 错误: %#v", payload.Event)
	}
	if payload.Event.Data["api_key"] != "[REDACTED]" || strings.Contains(response.Body.String(), "source-secret") {
		t.Fatalf("source response 必须脱敏: %s", response.Body.String())
	}
	if payload.Event.Data["text"] != "authorized body" {
		t.Fatalf("合法 source fetch 应保留事件正文给已授权 destination: %#v", payload.Event.Data)
	}

	tampered := envelope
	tampered.EventDigest = strings.Repeat("0", 64)
	tampered, err = SignRuntimeEventDeliveryEnvelope(tampered, []byte(testRuntimeEventDeliverySecret))
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	source.ServeHTTP(response, deliveryRequest(t, tampered))
	if response.Code != http.StatusConflict {
		t.Fatalf("digest 不匹配应返回 409: status=%d body=%s", response.Code, response.Body.String())
	}

	stale := envelope
	stale.Timestamp = now.Add(-3 * time.Minute)
	stale, err = SignRuntimeEventDeliveryEnvelope(stale, []byte(testRuntimeEventDeliverySecret))
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	source.ServeHTTP(response, deliveryRequest(t, stale))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("过期 source fetch 应返回 401: status=%d", response.Code)
	}

	wrongRoute := signedRuntimeEventDeliveryEnvelope(t, "other-runtime", "audit-b", event, outbox)
	response = httptest.NewRecorder()
	source.ServeHTTP(response, deliveryRequest(t, wrongRoute))
	if response.Code != http.StatusForbidden {
		t.Fatalf("错误 source 应返回 403: status=%d", response.Code)
	}

	response = httptest.NewRecorder()
	source.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/source", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("source 非 POST 应返回 405: status=%d", response.Code)
	}
}

func TestRuntimeEventDeliveryHTTPSourceClientVerifiesResponse(t *testing.T) {
	event, outbox, _ := runtimeEventDeliveryFixture(t)
	envelope := signedRuntimeEventDeliveryEnvelope(t, "runtime-a", "audit-b", event, outbox)
	spy := &runtimeEventDeliveryRoundTripper{}
	client, err := NewRuntimeEventDeliveryHTTPSourceClient("https://source.example.test/v1/events", "runtime-a", "audit-b", []byte(testRuntimeEventDeliverySecret), &http.Client{Transport: spy})
	if err != nil {
		t.Fatal(err)
	}
	response := RuntimeEventDeliverySourceResponse{Version: envelope.Version, DeliveryID: envelope.DeliveryID, EventID: envelope.EventID, EventDigest: envelope.EventDigest, Event: SanitizeAgentEvent(event)}
	spy.status = http.StatusOK
	spy.responseBody = string(mustJSON(t, response))
	fetched, err := client.FetchEvent(context.Background(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	if fetched.ID != event.ID || fetched.Data["api_key"] != "[REDACTED]" {
		t.Fatalf("client 返回事件错误或未脱敏: %#v", fetched)
	}
	if spy.request == nil || spy.request.Method != http.MethodPost || spy.request.Header.Get("Idempotency-Key") != event.ID {
		t.Fatalf("source client 请求头/方法错误: %#v", spy.request)
	}
	var sent RuntimeEventDeliveryEnvelope
	if err := json.Unmarshal(spy.body, &sent); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRuntimeEventDeliveryEnvelope(sent, []byte(testRuntimeEventDeliverySecret)); err != nil {
		t.Fatalf("source client 应重新签名请求: %v", err)
	}
	if bytes.Contains(spy.body, []byte("top-secret")) || bytes.Contains(spy.body, []byte("api_key")) {
		t.Fatalf("source fetch request 不应携带事件正文: %s", spy.body)
	}

	badDigest := response
	badDigest.EventDigest = strings.Repeat("1", 64)
	spy.responseBody = string(mustJSON(t, badDigest))
	if _, err := client.FetchEvent(context.Background(), envelope); !errors.Is(err, ErrRuntimeEventDeliveryAuth) {
		t.Fatalf("response digest 不一致应 fail-closed: %v", err)
	}

	unknown := map[string]any{"version": response.Version, "delivery_id": response.DeliveryID, "event_id": response.EventID, "event_digest": response.EventDigest, "event": response.Event, "unexpected": true}
	spy.responseBody = string(mustJSON(t, unknown))
	if _, err := client.FetchEvent(context.Background(), envelope); !errors.Is(err, ErrInvalidRuntimeEventDelivery) {
		t.Fatalf("response 未知字段应拒绝: %v", err)
	}

	spy.status = http.StatusBadGateway
	spy.responseBody = "api_key=source-secret"
	if _, err := client.FetchEvent(context.Background(), envelope); err == nil || strings.Contains(err.Error(), "source-secret") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("source 非 2xx 应脱敏: %v", err)
	}
}

func TestRuntimeEventDeliveryHTTPSourceClientEndToEnd(t *testing.T) {
	repo := NewMemoryRepository()
	invocation := eventOutboxInvocation(t, repo, "inv-event-source-e2e")
	now := time.Date(2026, 9, 13, 11, 0, 0, 0, time.UTC)
	event, err := repo.AppendEvent(context.Background(), AgentEvent{ID: "event-source-e2e", InvocationID: invocation.ID, Type: EventAssistantMessage, Timestamp: now, Data: map[string]any{"text": "e2e body"}})
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := repo.GetRuntimeEventOutbox(context.Background(), eventOutboxIDForTest(event.ID))
	if err != nil {
		t.Fatal(err)
	}
	envelope := signedRuntimeEventDeliveryEnvelope(t, "runtime-a", "audit-b", event, outbox)
	source, err := NewRuntimeEventDeliverySource("runtime-a", "audit-b", []byte(testRuntimeEventDeliverySecret), repo)
	if err != nil {
		t.Fatal(err)
	}
	source.Now = func() time.Time { return now.Add(time.Minute) }
	source.MaxAge = 2 * time.Minute
	server := httptest.NewServer(source)
	defer server.Close()
	client, err := NewRuntimeEventDeliveryHTTPSourceClient(server.URL, "runtime-a", "audit-b", []byte(testRuntimeEventDeliverySecret), nil)
	if err != nil {
		t.Fatal(err)
	}
	fetched, err := client.FetchEvent(context.Background(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	if fetched.ID != event.ID || fetched.Data["text"] != "e2e body" {
		t.Fatalf("source/client 端到端结果错误: %#v", fetched)
	}
}

func TestRuntimeEventDeliveryIssuedAtSourceFetchesDelayedEvent(t *testing.T) {
	repo := NewMemoryRepository()
	invocation := eventOutboxInvocation(t, repo, "inv-event-source-delayed")
	now := time.Date(2026, 9, 13, 13, 0, 0, 0, time.UTC)
	event, err := repo.AppendEvent(context.Background(), AgentEvent{
		ID: "event-source-delayed", InvocationID: invocation.ID, Type: EventRuntimeNotice,
		Timestamp: now.Add(-2 * time.Hour), Data: map[string]any{"text": "old but immutable"},
	})
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := repo.GetRuntimeEventOutbox(context.Background(), eventOutboxIDForTest(event.ID))
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewRuntimeEventDeliverySource("runtime-a", "audit-b", []byte(testRuntimeEventDeliverySecret), repo)
	if err != nil {
		t.Fatal(err)
	}
	source.Now = func() time.Time { return now }
	source.MaxAge = time.Minute
	server := httptest.NewServer(source)
	defer server.Close()

	legacy := signedRuntimeEventDeliveryEnvelope(t, "runtime-a", "audit-b", event, outbox)
	if response := callRuntimeEventSource(t, server.URL, legacy); response.Code != http.StatusUnauthorized {
		t.Fatalf("v1 source fetch 对滞留事件应受 replay window 保护: status=%d body=%s", response.Code, response.Body.String())
	}

	fresh, err := NewRuntimeEventDeliveryHTTPSourceClientWithIssuedAt(server.URL, "runtime-a", "audit-b", []byte(testRuntimeEventDeliverySecret), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	fresh.Now = func() time.Time { return now }
	fetched, err := fresh.FetchEvent(context.Background(), legacy)
	if err != nil {
		t.Fatalf("v2 source client 应允许取回滞留事件: %v", err)
	}
	if fetched.ID != event.ID || fetched.Timestamp.UTC() != event.Timestamp.UTC() || fetched.Data["text"] != "old but immutable" {
		t.Fatalf("v2 source fetch 不应改变事件正文或 immutable timestamp: %#v", fetched)
	}
}

func callRuntimeEventSource(t *testing.T, endpoint string, envelope RuntimeEventDeliveryEnvelope) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Abot-Event-Delivery-Version", fmt.Sprint(envelope.Version))
	request.Header.Set("X-Abot-Event-Delivery-Signature", envelope.Signature)
	request.Header.Set("Idempotency-Key", envelope.EventID)
	client := http.DefaultClient
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	recorder := httptest.NewRecorder()
	recorder.Code = response.StatusCode
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	recorder.Body.Write(data)
	return recorder
}

func TestRuntimeEventDeliverySourceRejectsOversizedResponseAndMissingEvent(t *testing.T) {
	repo := NewMemoryRepository()
	event, outbox, now := runtimeEventDeliveryFixture(t)
	_ = eventOutboxInvocation(t, repo, event.InvocationID)
	stored, err := repo.AppendEvent(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err = repo.GetRuntimeEventOutbox(context.Background(), eventOutboxIDForTest(stored.ID))
	if err != nil {
		t.Fatal(err)
	}
	envelope := signedRuntimeEventDeliveryEnvelope(t, "runtime-a", "audit-b", stored, outbox)
	source, err := NewRuntimeEventDeliverySource("runtime-a", "audit-b", []byte(testRuntimeEventDeliverySecret), repo)
	if err != nil {
		t.Fatal(err)
	}
	source.Now = func() time.Time { return now.Add(time.Minute) }
	source.MaxResponse = 64
	response := httptest.NewRecorder()
	source.ServeHTTP(response, deliveryRequest(t, envelope))
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大 source response 应返回 413: status=%d", response.Code)
	}

	missing := envelope
	missing.EventID = "event-does-not-exist"
	missing, err = SignRuntimeEventDeliveryEnvelope(missing, []byte(testRuntimeEventDeliverySecret))
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	source.ServeHTTP(response, deliveryRequest(t, missing))
	if response.Code != http.StatusNotFound {
		t.Fatalf("source 缺失事件应返回 404: status=%d", response.Code)
	}

	if _, err := NewRuntimeEventDeliverySource("runtime-a", "audit-b", []byte(testRuntimeEventDeliverySecret), nil); !errors.Is(err, ErrInvalidRuntimeEventDelivery) {
		t.Fatalf("source 缺少事件仓储应拒绝: %v", err)
	}
}

// Ensure the source/client test continues to exercise the strict decoder when
// an HTTP client returns a valid response followed by trailing JSON.
func TestRuntimeEventDeliveryHTTPSourceClientRejectsTrailingJSON(t *testing.T) {
	event, outbox, _ := runtimeEventDeliveryFixture(t)
	envelope := signedRuntimeEventDeliveryEnvelope(t, "runtime-a", "audit-b", event, outbox)
	spy := &runtimeEventDeliveryRoundTripper{status: http.StatusOK}
	client, err := NewRuntimeEventDeliveryHTTPSourceClient("https://source.example.test/v1/events", "runtime-a", "audit-b", []byte(testRuntimeEventDeliverySecret), &http.Client{Transport: spy})
	if err != nil {
		t.Fatal(err)
	}
	response := RuntimeEventDeliverySourceResponse{Version: envelope.Version, DeliveryID: envelope.DeliveryID, EventID: envelope.EventID, EventDigest: envelope.EventDigest, Event: SanitizeAgentEvent(event)}
	spy.responseBody = string(mustJSON(t, response)) + " {}"
	if _, err := client.FetchEvent(context.Background(), envelope); !errors.Is(err, ErrInvalidRuntimeEventDelivery) {
		t.Fatalf("source response trailing JSON 应拒绝: %v", err)
	}
}
