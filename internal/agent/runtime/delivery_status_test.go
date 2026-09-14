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
	"sync/atomic"
	"testing"
	"time"
)

const deliveryStatusTestSecret = "delivery-status-test-secret-32-bytes"

func invokeRuntimeDeliveryStatusHandler(t *testing.T, handler http.Handler, body any, headers map[string]string) (int, RuntimeDeliveryStatus, string) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/status", bytes.NewReader(encoded))
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var status RuntimeDeliveryStatus
	_ = json.Unmarshal(response.Body.Bytes(), &status)
	return response.Code, status, response.Body.String()
}

func eventDeliveryStatusHeaders(envelope RuntimeEventDeliveryEnvelope) map[string]string {
	return map[string]string{
		"X-Abot-Event-Delivery-Version":   "2",
		"X-Abot-Event-Delivery-Signature": envelope.Signature,
		"Idempotency-Key":                 envelope.EventID,
	}
}

func checkpointDeliveryStatusHeaders(envelope RuntimeCheckpointDeliveryEnvelope) map[string]string {
	return map[string]string{
		"X-Abot-Checkpoint-Delivery-Version":   "1",
		"X-Abot-Checkpoint-Delivery-Signature": envelope.Signature,
		"Idempotency-Key":                      envelope.DeliveryID,
	}
}

func configDeliveryStatusHeaders(envelope RuntimeConfigDeliveryEnvelope) map[string]string {
	return map[string]string{
		"X-Abot-Config-Delivery-Version":   "1",
		"X-Abot-Config-Delivery-Signature": envelope.Signature,
		"Idempotency-Key":                  envelope.DeliveryID,
	}
}

func rejectionDeliveryStatusHeaders(envelope RuntimeApprovalRejectionDeliveryEnvelope) map[string]string {
	return map[string]string{
		"X-Abot-Approval-Rejection-Delivery-Version":   "1",
		"X-Abot-Approval-Rejection-Delivery-Signature": envelope.Signature,
		"Idempotency-Key": envelope.DeliveryID,
	}
}

func TestRuntimeDeliveryStatusSignsAndRejectsTampering(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	status, err := NewRuntimeDeliveryStatus(RuntimeDeliveryKindCheckpoint, "runtime-a", "runtime-b", "delivery-1", "inv-1", RuntimeDeliveryStatusAccepted, true, now)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := SignRuntimeDeliveryStatus(status, []byte(deliveryStatusTestSecret))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRuntimeDeliveryStatus(signed, []byte(deliveryStatusTestSecret)); err != nil {
		t.Fatalf("签名 status 应可验证: %v", err)
	}
	tampered := signed
	tampered.Phase = RuntimeDeliveryStatusCommitted
	if err := VerifyRuntimeDeliveryStatus(tampered, []byte(deliveryStatusTestSecret)); !errors.Is(err, ErrRuntimeDeliveryStatusAuth) {
		t.Fatalf("篡改 phase 必须拒绝: %v", err)
	}
	if _, err := NewRuntimeDeliveryStatus(RuntimeDeliveryKindCheckpoint, "runtime-a", "runtime-b", "delivery-1", "inv-1", RuntimeDeliveryStatusAbsent, true, now); err != nil {
		// The constructor normalizes an absent proof to found=false; this is
		// intentional and keeps callers from manufacturing an unsafe state.
		t.Fatal(err)
	}
	encoded, err := json.Marshal(signed)
	if err != nil || strings.Contains(string(encoded), "projection") || strings.Contains(string(encoded), "reason") {
		t.Fatalf("status 必须是 metadata-only: %s err=%v", encoded, err)
	}
}

func TestRuntimeDeliveryStatusReceiversReportAbsentPreparedCommitted(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	secret := []byte(deliveryStatusTestSecret)

	t.Run("event", func(t *testing.T) {
		event, outbox, _ := runtimeEventDeliveryFixture(t)
		envelope, err := NewRuntimeEventDeliveryEnvelopeWithIssuedAt("runtime-a", "runtime-b", outbox, event, now)
		if err != nil {
			t.Fatal(err)
		}
		envelope, err = SignRuntimeEventDeliveryEnvelope(envelope, secret)
		if err != nil {
			t.Fatal(err)
		}
		repo := NewMemoryRepository()
		receiver, err := NewRuntimeEventDeliveryReceiver("runtime-a", "runtime-b", secret, repo)
		if err != nil {
			t.Fatal(err)
		}
		receiver.Now = func() time.Time { return now.Add(time.Minute) }
		code, status, body := invokeRuntimeDeliveryStatusHandler(t, receiver.StatusHandler(), envelope, eventDeliveryStatusHeaders(envelope))
		if code != http.StatusOK || status.Phase != RuntimeDeliveryStatusAbsent || status.Found {
			t.Fatalf("event absent status 错误: code=%d status=%#v body=%s", code, status, body)
		}
		if err := VerifyRuntimeDeliveryStatus(status, secret); err != nil {
			t.Fatalf("event absent response signature 无效: %v", err)
		}
		if _, err := repo.PrepareRuntimeEventDelivery(ctx, envelope); err != nil {
			t.Fatal(err)
		}
		code, status, body = invokeRuntimeDeliveryStatusHandler(t, receiver.StatusHandler(), envelope, eventDeliveryStatusHeaders(envelope))
		if code != http.StatusOK || status.Phase != RuntimeDeliveryStatusPrepared {
			t.Fatalf("event prepared status 错误: code=%d status=%#v body=%s", code, status, body)
		}
		if _, err := repo.CommitRuntimeEventDelivery(ctx, envelope); err != nil {
			t.Fatal(err)
		}
		code, status, body = invokeRuntimeDeliveryStatusHandler(t, receiver.StatusHandler(), envelope, eventDeliveryStatusHeaders(envelope))
		if code != http.StatusOK || status.Phase != RuntimeDeliveryStatusCommitted {
			t.Fatalf("event committed status 错误: code=%d status=%#v body=%s", code, status, body)
		}
	})

	t.Run("checkpoint", func(t *testing.T) {
		snapshot := checkpointDeliveryTestSnapshot(t, "status-checkpoint", now)
		envelope, projection, err := NewRuntimeCheckpointDeliveryEnvelope("runtime-a", "runtime-b", snapshot, 1)
		if err != nil {
			t.Fatal(err)
		}
		envelope, err = SignRuntimeCheckpointDeliveryEnvelope(envelope, secret)
		if err != nil {
			t.Fatal(err)
		}
		repo := NewMemoryRepository()
		receiver, err := NewRuntimeCheckpointDeliveryReceiver("runtime-a", "runtime-b", secret, repo)
		if err != nil {
			t.Fatal(err)
		}
		receiver.Now = func() time.Time { return now.Add(time.Minute) }
		code, status, body := invokeRuntimeDeliveryStatusHandler(t, receiver.StatusHandler(), envelope, checkpointDeliveryStatusHeaders(envelope))
		if code != http.StatusOK || status.Phase != RuntimeDeliveryStatusAbsent {
			t.Fatalf("checkpoint absent status 错误: code=%d status=%#v body=%s", code, status, body)
		}
		if _, err := repo.PrepareRuntimeCheckpointDelivery(ctx, envelope, projection); err != nil {
			t.Fatal(err)
		}
		code, status, body = invokeRuntimeDeliveryStatusHandler(t, receiver.StatusHandler(), envelope, checkpointDeliveryStatusHeaders(envelope))
		if code != http.StatusOK || status.Phase != RuntimeDeliveryStatusPrepared {
			t.Fatalf("checkpoint prepared status 错误: code=%d status=%#v body=%s", code, status, body)
		}
		if _, err := repo.CommitRuntimeCheckpointDelivery(ctx, envelope, projection); err != nil {
			t.Fatal(err)
		}
		code, status, body = invokeRuntimeDeliveryStatusHandler(t, receiver.StatusHandler(), envelope, checkpointDeliveryStatusHeaders(envelope))
		if code != http.StatusOK || status.Phase != RuntimeDeliveryStatusCommitted {
			t.Fatalf("checkpoint committed status 错误: code=%d status=%#v body=%s", code, status, body)
		}
	})

	t.Run("config", func(t *testing.T) {
		old := migrationSnapshot(t, "status-old", "demo", "model")
		target := migrationSnapshot(t, "status-target", "demo", "model")
		projection := runtimeProjectionForTest(target)
		outbox, err := NewRuntimeConfigDeliveryOutbox("runtime-a", "runtime-b", "status-config", runtimeSnapshotDigestForTest(old), "status-key", projection, now)
		if err != nil {
			t.Fatal(err)
		}
		envelope, err := outbox.Envelope(now)
		if err != nil {
			t.Fatal(err)
		}
		envelope, err = SignRuntimeConfigDeliveryEnvelope(envelope, secret)
		if err != nil {
			t.Fatal(err)
		}
		repo := NewMemoryRepository()
		receiver, err := NewRuntimeConfigDeliveryReceiver("runtime-a", "runtime-b", secret, repo)
		if err != nil {
			t.Fatal(err)
		}
		receiver.Now = func() time.Time { return now.Add(time.Minute) }
		code, status, body := invokeRuntimeDeliveryStatusHandler(t, receiver.StatusHandler(), envelope, configDeliveryStatusHeaders(envelope))
		if code != http.StatusOK || status.Phase != RuntimeDeliveryStatusAbsent {
			t.Fatalf("config absent status 错误: code=%d status=%#v body=%s", code, status, body)
		}
		if _, err := repo.PrepareRuntimeConfigDelivery(ctx, envelope, projection); err != nil {
			t.Fatal(err)
		}
		code, status, body = invokeRuntimeDeliveryStatusHandler(t, receiver.StatusHandler(), envelope, configDeliveryStatusHeaders(envelope))
		if code != http.StatusOK || status.Phase != RuntimeDeliveryStatusPrepared {
			t.Fatalf("config prepared status 错误: code=%d status=%#v body=%s", code, status, body)
		}
		if _, err := repo.CommitRuntimeConfigDelivery(ctx, envelope); err != nil {
			t.Fatal(err)
		}
		code, status, body = invokeRuntimeDeliveryStatusHandler(t, receiver.StatusHandler(), envelope, configDeliveryStatusHeaders(envelope))
		if code != http.StatusOK || status.Phase != RuntimeDeliveryStatusCommitted {
			t.Fatalf("config committed status 错误: code=%d status=%#v body=%s", code, status, body)
		}
	})

	t.Run("approval-rejection", func(t *testing.T) {
		_, approval, invocation := rejectionDeliveryFixture(t, ApprovalRejected)
		outbox, err := NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", approval, invocation, "拒绝", now)
		if err != nil {
			t.Fatal(err)
		}
		envelope, err := outbox.Envelope(now)
		if err != nil {
			t.Fatal(err)
		}
		envelope, err = SignRuntimeApprovalRejectionDeliveryEnvelope(envelope, secret)
		if err != nil {
			t.Fatal(err)
		}
		repo := NewMemoryRepository()
		receiver, err := NewRuntimeApprovalRejectionDeliveryReceiver("runtime-a", "runtime-b", secret, repo)
		if err != nil {
			t.Fatal(err)
		}
		receiver.Now = func() time.Time { return now.Add(time.Minute) }
		code, status, body := invokeRuntimeDeliveryStatusHandler(t, receiver.StatusHandler(), envelope, rejectionDeliveryStatusHeaders(envelope))
		if code != http.StatusOK || status.Phase != RuntimeDeliveryStatusAbsent {
			t.Fatalf("rejection absent status 错误: code=%d status=%#v body=%s", code, status, body)
		}
		if _, err := repo.PrepareRuntimeApprovalRejectionDelivery(ctx, envelope); err != nil {
			t.Fatal(err)
		}
		code, status, body = invokeRuntimeDeliveryStatusHandler(t, receiver.StatusHandler(), envelope, rejectionDeliveryStatusHeaders(envelope))
		if code != http.StatusOK || status.Phase != RuntimeDeliveryStatusPrepared {
			t.Fatalf("rejection prepared status 错误: code=%d status=%#v body=%s", code, status, body)
		}
		if _, err := repo.CommitRuntimeApprovalRejectionDelivery(ctx, envelope); err != nil {
			t.Fatal(err)
		}
		code, status, body = invokeRuntimeDeliveryStatusHandler(t, receiver.StatusHandler(), envelope, rejectionDeliveryStatusHeaders(envelope))
		if code != http.StatusOK || status.Phase != RuntimeDeliveryStatusCommitted {
			t.Fatalf("rejection committed status 错误: code=%d status=%#v body=%s", code, status, body)
		}
	})
}

func TestRuntimeDeliveryStatusRejectsForgedResponse(t *testing.T) {
	event, outbox, now := runtimeEventDeliveryFixture(t)
	transport, err := NewRuntimeEventDeliveryHTTPTransport("https://receiver.example.test/events", "runtime-a", "runtime-b", []byte(deliveryStatusTestSecret), &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		var envelope RuntimeEventDeliveryEnvelope
		if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil {
			return nil, err
		}
		status, err := NewRuntimeDeliveryStatus(RuntimeDeliveryKindEvent, envelope.Source, envelope.Destination, envelope.DeliveryID, envelope.InvocationID, RuntimeDeliveryStatusAccepted, true, now)
		if err != nil {
			return nil, err
		}
		// Deliberately sign with another key; the transport must not trust the
		// request authentication as proof of the response.
		status, err = SignRuntimeDeliveryStatus(status, []byte("different-secret-32-bytes-long"))
		if err != nil {
			return nil, err
		}
		body, _ := json.Marshal(status)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, err = transport.ReconcileEventDelivery(context.Background(), outbox, event)
	if !errors.Is(err, ErrRuntimeDeliveryStatusAuth) {
		t.Fatalf("伪造 status 签名必须拒绝: %v", err)
	}
}

func TestRuntimeDeliveryHTTPTransportsAcceptSignedStatus(t *testing.T) {
	secret := []byte(deliveryStatusTestSecret)
	now := time.Date(2026, 9, 13, 14, 0, 0, 0, time.UTC)
	statusClient := func(kind RuntimeDeliveryKind) *http.Client {
		return &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != "/delivery/status" {
				t.Fatalf("status query path 错误: %s", request.URL.Path)
			}
			var identity struct {
				Source       string `json:"source"`
				Destination  string `json:"destination"`
				DeliveryID   string `json:"delivery_id"`
				InvocationID string `json:"invocation_id"`
			}
			if err := json.NewDecoder(request.Body).Decode(&identity); err != nil {
				return nil, err
			}
			status, err := NewRuntimeDeliveryStatus(kind, identity.Source, identity.Destination, identity.DeliveryID, identity.InvocationID, RuntimeDeliveryStatusAccepted, true, now)
			if err != nil {
				return nil, err
			}
			status, err = SignRuntimeDeliveryStatus(status, secret)
			if err != nil {
				return nil, err
			}
			body, err := json.Marshal(status)
			if err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
		})}
	}

	t.Run("event", func(t *testing.T) {
		event, outbox, _ := runtimeEventDeliveryFixture(t)
		transport, err := NewRuntimeEventDeliveryHTTPTransport("https://receiver.example.test/delivery", "runtime-a", "runtime-b", secret, statusClient(RuntimeDeliveryKindEvent))
		if err != nil {
			t.Fatal(err)
		}
		status, err := transport.ReconcileEventDelivery(context.Background(), outbox, event)
		if err != nil || status.Phase != RuntimeDeliveryStatusAccepted {
			t.Fatalf("event status transport 失败: %#v err=%v", status, err)
		}
	})

	t.Run("checkpoint", func(t *testing.T) {
		snapshot := checkpointDeliveryTestSnapshot(t, "transport-status-checkpoint", now)
		envelope, projection, err := NewRuntimeCheckpointDeliveryEnvelope("runtime-a", "runtime-b", snapshot, 1)
		if err != nil {
			t.Fatal(err)
		}
		transport, err := NewRuntimeCheckpointDeliveryHTTPTransport("https://receiver.example.test/delivery", "runtime-a", "runtime-b", secret, statusClient(RuntimeDeliveryKindCheckpoint))
		if err != nil {
			t.Fatal(err)
		}
		status, err := transport.ReconcileCheckpointDelivery(context.Background(), envelope, projection)
		if err != nil || status.Phase != RuntimeDeliveryStatusAccepted {
			t.Fatalf("checkpoint status transport 失败: %#v err=%v", status, err)
		}
	})

	t.Run("config", func(t *testing.T) {
		old := migrationSnapshot(t, "transport-status-old", "demo", "model")
		target := migrationSnapshot(t, "transport-status-target", "demo", "model")
		projection := runtimeProjectionForTest(target)
		outbox, err := NewRuntimeConfigDeliveryOutbox("runtime-a", "runtime-b", "transport-status-config", runtimeSnapshotDigestForTest(old), "transport-status", projection, now)
		if err != nil {
			t.Fatal(err)
		}
		envelope, err := outbox.Envelope(now)
		if err != nil {
			t.Fatal(err)
		}
		transport, err := NewRuntimeConfigDeliveryHTTPTransport("https://receiver.example.test/delivery", "runtime-a", "runtime-b", secret, statusClient(RuntimeDeliveryKindConfig))
		if err != nil {
			t.Fatal(err)
		}
		status, err := transport.ReconcileConfigDelivery(context.Background(), envelope, projection)
		if err != nil || status.Phase != RuntimeDeliveryStatusAccepted {
			t.Fatalf("config status transport 失败: %#v err=%v", status, err)
		}
	})

	t.Run("approval-rejection", func(t *testing.T) {
		_, approval, invocation := rejectionDeliveryFixture(t, ApprovalRejected)
		outbox, err := NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", approval, invocation, "拒绝", now)
		if err != nil {
			t.Fatal(err)
		}
		envelope, err := outbox.Envelope(now)
		if err != nil {
			t.Fatal(err)
		}
		transport, err := NewRuntimeApprovalRejectionDeliveryHTTPTransport("https://receiver.example.test/delivery", "runtime-a", "runtime-b", secret, statusClient(RuntimeDeliveryKindRejection))
		if err != nil {
			t.Fatal(err)
		}
		status, err := transport.ReconcileApprovalRejectionDelivery(context.Background(), envelope)
		if err != nil || status.Phase != RuntimeDeliveryStatusAccepted {
			t.Fatalf("rejection status transport 失败: %#v err=%v", status, err)
		}
	})
}

func TestCoordinatorReconcilesDestinationProofBeforeSending(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 13, 0, 0, 0, time.UTC)

	t.Run("event accepted", func(t *testing.T) {
		repo := NewMemoryRepository()
		invocation := eventOutboxInvocation(t, repo, "reconcile-event-invocation")
		event, err := repo.AppendEvent(ctx, AgentEvent{ID: "reconcile-event", InvocationID: invocation.ID, Type: EventRuntimeNotice, Timestamp: now})
		if err != nil {
			t.Fatal(err)
		}
		transport := &reconcilingEventTransport{phase: RuntimeDeliveryStatusAccepted}
		coordinator := &Coordinator{repo: repo, eventOutboxWorkerID: "reconcile-event-worker"}
		coordinator.SetRuntimeEventDeliveryTransport(transport)
		coordinator.dispatchDueRuntimeEventOutbox(ctx, now)
		outbox, err := repo.GetRuntimeEventOutbox(ctx, eventOutboxIDForTest(event.ID))
		if err != nil || outbox.Status != RuntimeEventOutboxCompleted {
			t.Fatalf("destination accepted proof 应完成 source outbox: %#v err=%v", outbox, err)
		}
		if transport.deliverCalls.Load() != 0 || transport.reconcileCalls.Load() != 1 {
			t.Fatalf("accepted proof 不应重复发送: deliver=%d reconcile=%d", transport.deliverCalls.Load(), transport.reconcileCalls.Load())
		}
	})

	t.Run("checkpoint prepared commit only", func(t *testing.T) {
		repo := NewMemoryRepository()
		invocation := Invocation{ID: "reconcile-checkpoint-invocation", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
		if err := repo.CreateInvocation(ctx, invocation); err != nil {
			t.Fatal(err)
		}
		snapshot := checkpointDeliveryTestSnapshot(t, invocation.ID, now)
		_, _, outbox, err := repo.CommitRuntimeSnapshotWithCheckpoint(ctx, snapshot, AgentEvent{ID: "reconcile-checkpoint-event", InvocationID: invocation.ID, Type: EventRuntimeSnapshot, Timestamp: now}, "runtime-a", "runtime-b")
		if err != nil {
			t.Fatal(err)
		}
		transport := &reconcilingCheckpointTransport{phase: RuntimeDeliveryStatusPrepared, transactional: true}
		coordinator := &Coordinator{repo: repo, checkpointOutboxWorkerID: "reconcile-checkpoint-worker", checkpointDeliverySource: "runtime-a", checkpointDeliveryDestination: "runtime-b", checkpointDeliveryTransport: transport}
		coordinator.dispatchDueRuntimeCheckpointOutbox(ctx, now)
		completed, err := repo.GetRuntimeCheckpointDeliveryOutbox(ctx, outbox.ID)
		if err != nil || completed.Status != RuntimeCheckpointDeliveryOutboxCompleted {
			t.Fatalf("prepared proof 经 commit 后应完成 source outbox: %#v err=%v", completed, err)
		}
		if transport.prepareCalls.Load() != 0 || transport.commitCalls.Load() != 1 || transport.reconcileCalls.Load() != 1 {
			t.Fatalf("prepared proof 应只调用 commit: prepare=%d commit=%d reconcile=%d", transport.prepareCalls.Load(), transport.commitCalls.Load(), transport.reconcileCalls.Load())
		}
	})

	t.Run("config committed", func(t *testing.T) {
		old := migrationSnapshot(t, "reconcile-config-old", "demo", "model")
		target := migrationSnapshot(t, "reconcile-config-target", "demo", "model")
		targetDigest := runtimeSnapshotDigestForTest(target)
		repo := NewMemoryRepository()
		invocation := Invocation{ID: "reconcile-config-invocation", UserID: "user", ConversationID: "conversation", SessionID: "session", ConfigSnapshot: old, Status: InvocationWaitingUser, CreatedAt: now, UpdatedAt: now}
		if err := repo.CreateInvocation(ctx, invocation); err != nil {
			t.Fatal(err)
		}
		_, _, outbox, err := repo.CommitRuntimeConfigMigrationWithDelivery(ctx, RuntimeConfigMigrationCommit{InvocationID: invocation.ID, ExpectedDigest: runtimeSnapshotDigestForTest(old), NewSnapshot: target, IdempotencyKey: "reconcile-config", Event: AgentEvent{ID: RuntimeConfigMigrationEventID(invocation.ID, runtimeSnapshotDigestForTest(old), targetDigest, "reconcile-config"), InvocationID: invocation.ID, Type: EventRuntimeConfigMigrated, Timestamp: now}}, "runtime-a", "runtime-b")
		if err != nil {
			t.Fatal(err)
		}
		transport := &reconcilingConfigTransport{phase: RuntimeDeliveryStatusCommitted}
		coordinator := &Coordinator{repo: repo, configOutboxWorkerID: "reconcile-config-worker", configDeliverySource: "runtime-a", configDeliveryDestination: "runtime-b", configDeliveryTransport: transport}
		coordinator.dispatchDueRuntimeConfigOutbox(ctx, now)
		completed, err := repo.GetRuntimeConfigDeliveryOutbox(ctx, outbox.ID)
		if err != nil || completed.Status != RuntimeConfigDeliveryOutboxCompleted || transport.deliverCalls.Load() != 0 {
			t.Fatalf("committed proof 应跳过 config send: outbox=%#v calls=%d err=%v", completed, transport.deliverCalls.Load(), err)
		}
	})

	t.Run("approval rejection accepted", func(t *testing.T) {
		repo, approval, invocation := rejectionDeliveryFixture(t, ApprovalRejected)
		outbox, err := NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", approval, invocation, "拒绝", now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.EnqueueRuntimeApprovalRejectionDeliveryOutbox(ctx, outbox); err != nil {
			t.Fatal(err)
		}
		transport := &reconcilingRejectionTransport{phase: RuntimeDeliveryStatusAccepted}
		coordinator := &Coordinator{repo: repo, rejectionOutboxWorkerID: "reconcile-rejection-worker", rejectionDeliverySource: "runtime-a", rejectionDeliveryDestination: "runtime-b", rejectionDeliveryTransport: transport}
		coordinator.dispatchDueRuntimeApprovalRejectionOutboxBounded(ctx, now, 1)
		completed, err := repo.GetRuntimeApprovalRejectionDeliveryOutbox(ctx, outbox.ID)
		if err != nil || completed.Status != RuntimeApprovalRejectionDeliveryOutboxCompleted || transport.deliverCalls.Load() != 0 {
			t.Fatalf("accepted rejection proof 应跳过 send: outbox=%#v calls=%d err=%v", completed, transport.deliverCalls.Load(), err)
		}
	})
}

type reconcilingEventTransport struct {
	phase          RuntimeDeliveryStatusPhase
	deliverCalls   atomic.Int32
	reconcileCalls atomic.Int32
}

func (t *reconcilingEventTransport) Deliver(context.Context, RuntimeEventOutbox, AgentEvent) error {
	t.deliverCalls.Add(1)
	return errors.New("delivery should have been reconciled")
}

func (t *reconcilingEventTransport) ReconcileEventDelivery(_ context.Context, outbox RuntimeEventOutbox, event AgentEvent) (RuntimeDeliveryStatus, error) {
	t.reconcileCalls.Add(1)
	return NewRuntimeDeliveryStatus(RuntimeDeliveryKindEvent, "runtime-a", "runtime-b", outbox.ID, event.InvocationID, t.phase, true, time.Now().UTC())
}

func (t *reconcilingEventTransport) RuntimeEventDeliveryRoute() (string, string) {
	return "runtime-a", "runtime-b"
}

type reconcilingCheckpointTransport struct {
	phase          RuntimeDeliveryStatusPhase
	transactional  bool
	deliverCalls   atomic.Int32
	prepareCalls   atomic.Int32
	commitCalls    atomic.Int32
	reconcileCalls atomic.Int32
}

func (t *reconcilingCheckpointTransport) Deliver(_ context.Context, envelope RuntimeCheckpointDeliveryEnvelope, _ RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
	t.deliverCalls.Add(1)
	return RuntimeCheckpointDeliveryReceipt{Version: envelope.Version, DeliveryID: envelope.DeliveryID, InvocationID: envelope.InvocationID, SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence, SnapshotDigest: envelope.SnapshotDigest}, nil
}

func (t *reconcilingCheckpointTransport) Prepare(_ context.Context, envelope RuntimeCheckpointDeliveryEnvelope, _ RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
	t.prepareCalls.Add(1)
	return RuntimeCheckpointDeliveryReceipt{Version: envelope.Version, DeliveryID: envelope.DeliveryID, InvocationID: envelope.InvocationID, SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence, SnapshotDigest: envelope.SnapshotDigest, Phase: RuntimeCheckpointDeliveryTransactionPhasePrepared}, nil
}

func (t *reconcilingCheckpointTransport) Commit(_ context.Context, envelope RuntimeCheckpointDeliveryEnvelope, _ RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
	t.commitCalls.Add(1)
	return RuntimeCheckpointDeliveryReceipt{Version: envelope.Version, DeliveryID: envelope.DeliveryID, InvocationID: envelope.InvocationID, SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence, SnapshotDigest: envelope.SnapshotDigest, Phase: RuntimeCheckpointDeliveryTransactionPhaseCommitted}, nil
}

func (t *reconcilingCheckpointTransport) RuntimeCheckpointDeliveryTransactionsEnabled() bool {
	return t.transactional
}

func (t *reconcilingCheckpointTransport) ReconcileCheckpointDelivery(_ context.Context, envelope RuntimeCheckpointDeliveryEnvelope, _ RuntimeCheckpointProjection) (RuntimeDeliveryStatus, error) {
	t.reconcileCalls.Add(1)
	return NewRuntimeDeliveryStatus(RuntimeDeliveryKindCheckpoint, envelope.Source, envelope.Destination, envelope.DeliveryID, envelope.InvocationID, t.phase, true, time.Now().UTC())
}

type reconcilingConfigTransport struct {
	phase        RuntimeDeliveryStatusPhase
	deliverCalls atomic.Int32
}

func (t *reconcilingConfigTransport) Deliver(_ context.Context, envelope RuntimeConfigDeliveryEnvelope, _ RuntimeConfigDeliveryProjection) (RuntimeConfigDeliveryReceipt, error) {
	t.deliverCalls.Add(1)
	return RuntimeConfigDeliveryReceipt{Version: envelope.Version, DeliveryID: envelope.DeliveryID, InvocationID: envelope.InvocationID, ExpectedDigest: envelope.ExpectedDigest, SnapshotDigest: envelope.SnapshotDigest, SnapshotVersion: envelope.SnapshotVersion}, nil
}

func (t *reconcilingConfigTransport) ReconcileConfigDelivery(_ context.Context, envelope RuntimeConfigDeliveryEnvelope, _ RuntimeConfigDeliveryProjection) (RuntimeDeliveryStatus, error) {
	return NewRuntimeDeliveryStatus(RuntimeDeliveryKindConfig, envelope.Source, envelope.Destination, envelope.DeliveryID, envelope.InvocationID, t.phase, true, time.Now().UTC())
}

type reconcilingRejectionTransport struct {
	phase        RuntimeDeliveryStatusPhase
	deliverCalls atomic.Int32
}

func (t *reconcilingRejectionTransport) Deliver(_ context.Context, envelope RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeApprovalRejectionDeliveryReceipt, error) {
	t.deliverCalls.Add(1)
	return RuntimeApprovalRejectionDeliveryReceipt{Version: envelope.Version, DeliveryID: envelope.DeliveryID, ApprovalID: envelope.ApprovalID, InvocationID: envelope.InvocationID, ToolCallID: envelope.ToolCallID, OperationID: envelope.OperationID, Decision: envelope.Decision, ReasonDigest: envelope.ReasonDigest}, nil
}

func (t *reconcilingRejectionTransport) ReconcileApprovalRejectionDelivery(_ context.Context, envelope RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeDeliveryStatus, error) {
	return NewRuntimeDeliveryStatus(RuntimeDeliveryKindRejection, envelope.Source, envelope.Destination, envelope.DeliveryID, envelope.InvocationID, t.phase, true, time.Now().UTC())
}

// Small local adapters keep status transport tests independent of a listening
// socket, while still exercising the complete HTTP encoding/decoding path.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
