package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

func TestServerMountsOptInRuntimeEventDeliveryReceiver(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	event := agentruntime.AgentEvent{ID: "http-delivery-event", InvocationID: "http-delivery-invocation", Sequence: 1, Type: agentruntime.EventRuntimeNotice, Timestamp: now}
	outbox, err := agentruntime.NewRuntimeEventOutbox(event)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := agentruntime.NewRuntimeEventDeliveryEnvelope("runtime-a", "audit-b", outbox, event)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err = agentruntime.SignRuntimeEventDeliveryEnvelope(envelope, secret)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := agentruntime.NewRuntimeEventDeliveryReceiver("runtime-a", "audit-b", secret, agentruntime.NewMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now.Add(time.Minute) }
	server := NewServer(nil, nil)
	server.SetRuntimeEventDeliveryReceiver(receiver)
	server.SetRuntimeEventDeliveryTransactionReceiver(receiver)
	handler := server.Handler()

	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/event-delivery", bytes.NewReader(body))
	request.Header.Set("X-Abot-Event-Delivery-Version", "1")
	request.Header.Set("X-Abot-Event-Delivery-Signature", envelope.Signature)
	request.Header.Set("Idempotency-Key", envelope.EventID)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"accepted":true`) {
		t.Fatalf("挂载的 receiver 应接受 envelope: status=%d body=%s", response.Code, response.Body.String())
	}

	transactionBody, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/runtime/event-delivery/prepare", bytes.NewReader(transactionBody))
	request.Header.Set("X-Abot-Event-Delivery-Version", "1")
	request.Header.Set("X-Abot-Event-Delivery-Signature", envelope.Signature)
	request.Header.Set("X-Abot-Event-Transaction-Phase", agentruntime.RuntimeEventDeliveryTransactionPhasePrepared)
	request.Header.Set("Idempotency-Key", envelope.EventID)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"phase":"prepared"`) {
		t.Fatalf("挂载的 event prepare 应接受 envelope: status=%d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/runtime/event-delivery/commit", bytes.NewReader(transactionBody))
	request.Header.Set("X-Abot-Event-Delivery-Version", "1")
	request.Header.Set("X-Abot-Event-Delivery-Signature", envelope.Signature)
	request.Header.Set("X-Abot-Event-Transaction-Phase", agentruntime.RuntimeEventDeliveryTransactionPhaseCommitted)
	request.Header.Set("Idempotency-Key", envelope.EventID)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"phase":"committed"`) {
		t.Fatalf("挂载的 event commit 应提交 envelope: status=%d body=%s", response.Code, response.Body.String())
	}

	// Use a fresh delivery identity to prove the separately mounted abort route
	// can release a prepared transaction without touching the committed one.
	abortEvent := event
	abortEvent.ID = "http-delivery-abort-event"
	abortEvent.Timestamp = now.Add(time.Second)
	abortOutbox, err := agentruntime.NewRuntimeEventOutbox(abortEvent)
	if err != nil {
		t.Fatal(err)
	}
	abortEnvelope, err := agentruntime.NewRuntimeEventDeliveryEnvelope("runtime-a", "audit-b", abortOutbox, abortEvent)
	if err != nil {
		t.Fatal(err)
	}
	abortEnvelope, err = agentruntime.SignRuntimeEventDeliveryEnvelope(abortEnvelope, secret)
	if err != nil {
		t.Fatal(err)
	}
	abortBody, err := json.Marshal(abortEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/runtime/event-delivery/prepare", bytes.NewReader(abortBody))
	request.Header.Set("X-Abot-Event-Delivery-Version", "1")
	request.Header.Set("X-Abot-Event-Delivery-Signature", abortEnvelope.Signature)
	request.Header.Set("X-Abot-Event-Transaction-Phase", agentruntime.RuntimeEventDeliveryTransactionPhasePrepared)
	request.Header.Set("Idempotency-Key", abortEnvelope.EventID)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"phase":"prepared"`) {
		t.Fatalf("挂载的 event abort fixture prepare 失败: status=%d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/runtime/event-delivery/abort", bytes.NewReader(abortBody))
	request.Header.Set("X-Abot-Event-Delivery-Version", "1")
	request.Header.Set("X-Abot-Event-Delivery-Signature", abortEnvelope.Signature)
	request.Header.Set("X-Abot-Event-Transaction-Phase", agentruntime.RuntimeEventDeliveryTransactionPhaseAborted)
	request.Header.Set("Idempotency-Key", abortEnvelope.EventID)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"phase":"aborted"`) {
		t.Fatalf("挂载的 event abort 应释放 prepared transaction: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestServerMountsOptInRuntimeEventDeliverySource(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	repo := agentruntime.NewMemoryRepository()
	invocation := agentruntime.Invocation{ID: "http-source-invocation", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	event, err := repo.AppendEvent(context.Background(), agentruntime.AgentEvent{ID: "http-source-event", InvocationID: invocation.ID, Type: agentruntime.EventRuntimeNotice, Timestamp: now, Data: map[string]any{"code": "source"}})
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := agentruntime.NewRuntimeEventOutbox(event)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := agentruntime.NewRuntimeEventDeliveryEnvelope("runtime-a", "audit-b", outbox, event)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err = agentruntime.SignRuntimeEventDeliveryEnvelope(envelope, secret)
	if err != nil {
		t.Fatal(err)
	}
	source, err := agentruntime.NewRuntimeEventDeliverySource("runtime-a", "audit-b", secret, repo)
	if err != nil {
		t.Fatal(err)
	}
	source.Now = func() time.Time { return now.Add(time.Minute) }
	server := NewServer(nil, nil)
	server.SetRuntimeEventDeliverySource(source)
	handler := server.Handler()
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/event-delivery/source", bytes.NewReader(body))
	request.Header.Set("X-Abot-Event-Delivery-Version", "1")
	request.Header.Set("X-Abot-Event-Delivery-Signature", envelope.Signature)
	request.Header.Set("Idempotency-Key", envelope.EventID)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"event_id":"`+event.ID+`"`) {
		t.Fatalf("挂载的 source 应返回 event: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestServerMountsOptInRuntimeCheckpointDeliveryEndpoints(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	repo := agentruntime.NewMemoryRepository()
	invocation := agentruntime.Invocation{ID: "http-checkpoint-invocation", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(context.Background(), agentruntime.AgentEvent{ID: "http-checkpoint-event", InvocationID: invocation.ID, Type: agentruntime.EventRuntimeNotice, Timestamp: now, Data: map[string]any{"code": "checkpoint"}}); err != nil {
		t.Fatal(err)
	}
	snapshot := agentruntime.RuntimeSnapshot{InvocationID: invocation.ID, Revision: 1, Phase: string(agentruntime.InvocationWaitingTool), WorkflowPhase: agentruntime.WorkflowPhaseWaitingTool, GeneratedAt: now}
	if _, err := repo.SaveRuntimeSnapshot(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	envelope, projection, err := agentruntime.NewRuntimeCheckpointDeliveryEnvelope("runtime-a", "runtime-b", snapshot, 1)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err = agentruntime.SignRuntimeCheckpointDeliveryEnvelope(envelope, secret)
	if err != nil {
		t.Fatal(err)
	}
	source, err := agentruntime.NewRuntimeCheckpointDeliverySource("runtime-a", "runtime-b", secret, repo)
	if err != nil {
		t.Fatal(err)
	}
	source.Now = func() time.Time { return now.Add(time.Minute) }
	receiver, err := agentruntime.NewRuntimeCheckpointDeliveryReceiver("runtime-a", "runtime-b", secret, agentruntime.NewMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now.Add(time.Minute) }

	server := NewServer(nil, nil)
	server.SetRuntimeCheckpointDeliverySource(source)
	server.SetRuntimeCheckpointDeliveryReceiver(receiver)
	server.SetRuntimeCheckpointDeliveryTransactionReceiver(receiver)
	handler := server.Handler()

	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/checkpoint-delivery/source", bytes.NewReader(body))
	request.Header.Set("X-Abot-Checkpoint-Delivery-Version", "1")
	request.Header.Set("X-Abot-Checkpoint-Delivery-Signature", envelope.Signature)
	request.Header.Set("Idempotency-Key", envelope.DeliveryID)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"invocation_id":"`+invocation.ID+`"`) {
		t.Fatalf("挂载的 checkpoint source 应返回 projection: status=%d body=%s", response.Code, response.Body.String())
	}

	payload, err := json.Marshal(agentruntime.RuntimeCheckpointDeliverySourceResponse{Envelope: envelope, Projection: projection})
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/runtime/checkpoint-delivery", bytes.NewReader(payload))
	request.Header.Set("X-Abot-Checkpoint-Delivery-Version", "1")
	request.Header.Set("X-Abot-Checkpoint-Delivery-Signature", envelope.Signature)
	request.Header.Set("Idempotency-Key", envelope.DeliveryID)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"duplicate":false`) {
		t.Fatalf("挂载的 checkpoint receiver 应接受 projection: status=%d body=%s", response.Code, response.Body.String())
	}

	transactionPayload, err := json.Marshal(agentruntime.RuntimeCheckpointDeliverySourceResponse{Envelope: envelope, Projection: projection})
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/runtime/checkpoint-delivery/prepare", bytes.NewReader(transactionPayload))
	request.Header.Set("X-Abot-Checkpoint-Delivery-Version", "1")
	request.Header.Set("X-Abot-Checkpoint-Delivery-Signature", envelope.Signature)
	request.Header.Set("X-Abot-Checkpoint-Transaction-Phase", agentruntime.RuntimeCheckpointDeliveryTransactionPhasePrepared)
	request.Header.Set("Idempotency-Key", envelope.DeliveryID)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"phase":"prepared"`) {
		t.Fatalf("挂载的 checkpoint prepare 应接受 projection: status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/runtime/checkpoint-delivery/commit", bytes.NewReader(transactionPayload))
	request.Header.Set("X-Abot-Checkpoint-Delivery-Version", "1")
	request.Header.Set("X-Abot-Checkpoint-Delivery-Signature", envelope.Signature)
	request.Header.Set("X-Abot-Checkpoint-Transaction-Phase", agentruntime.RuntimeCheckpointDeliveryTransactionPhaseCommitted)
	request.Header.Set("Idempotency-Key", envelope.DeliveryID)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"phase":"committed"`) {
		t.Fatalf("挂载的 checkpoint commit 应提交 projection: status=%d body=%s", response.Code, response.Body.String())
	}
}
