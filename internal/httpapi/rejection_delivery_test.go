package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

func TestServerMountsRuntimeApprovalRejectionDeliveryEndpoints(t *testing.T) {
	repo := agentruntime.NewMemoryRepository()
	now := time.Now().UTC()
	invocation := agentruntime.Invocation{ID: "http-rejection-invocation", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingApproval, CreatedAt: now, UpdatedAt: now}
	approval := agentruntime.Approval{ID: "http-rejection-approval", InvocationID: invocation.ID, ToolCallID: "http-rejection-tool", OperationID: "http-rejection-operation", Status: agentruntime.ApprovalRejected, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(nil, invocation); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateApproval(nil, approval); err != nil {
		t.Fatal(err)
	}
	outbox, err := agentruntime.NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", approval, invocation, "用户拒绝", now)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := outbox.Envelope(now)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("0123456789abcdef0123456789abcdef")
	envelope, err = agentruntime.SignRuntimeApprovalRejectionDeliveryEnvelope(envelope, secret)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := agentruntime.NewRuntimeApprovalRejectionDeliveryReceiver("runtime-a", "runtime-b", secret, repo)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now }
	server := NewServer(nil, nil)
	server.SetRuntimeApprovalRejectionDeliveryReceiver(receiver)
	server.SetRuntimeApprovalRejectionDeliveryTransactionReceiver(receiver)
	handler := server.Handler()
	body, _ := json.Marshal(envelope)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/approval-rejection-delivery", bytes.NewReader(body))
	request.Header.Set("X-Abot-Approval-Rejection-Delivery-Version", "1")
	request.Header.Set("X-Abot-Approval-Rejection-Delivery-Signature", envelope.Signature)
	request.Header.Set("Idempotency-Key", envelope.DeliveryID)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "用户拒绝") {
		t.Fatalf("server rejection receiver 错误: status=%d body=%s", response.Code, response.Body.String())
	}
	payload, _ := json.Marshal(map[string]any{"envelope": envelope})
	prepare := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/approval-rejection-delivery/prepare", bytes.NewReader(payload))
	prepare.Header.Set("X-Abot-Approval-Rejection-Transaction-Phase", agentruntime.RuntimeApprovalRejectionDeliveryTransactionPhasePrepared)
	prepareResponse := httptest.NewRecorder()
	handler.ServeHTTP(prepareResponse, prepare)
	if prepareResponse.Code != http.StatusOK {
		t.Fatalf("server rejection prepare 错误: status=%d body=%s", prepareResponse.Code, prepareResponse.Body.String())
	}
	commit := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/approval-rejection-delivery/commit", bytes.NewReader(payload))
	commit.Header.Set("X-Abot-Approval-Rejection-Transaction-Phase", agentruntime.RuntimeApprovalRejectionDeliveryTransactionPhaseCommitted)
	commitResponse := httptest.NewRecorder()
	handler.ServeHTTP(commitResponse, commit)
	if commitResponse.Code != http.StatusOK {
		t.Fatalf("server rejection commit 错误: status=%d body=%s", commitResponse.Code, commitResponse.Body.String())
	}
}
