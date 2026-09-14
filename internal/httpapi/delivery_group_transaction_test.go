package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

func TestRuntimeDeliveryGroupTransactionRoutesAreOptInAndAuthenticated(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	group, err := agentruntime.NewRuntimeDeliveryGroup("runtime-a", "runtime-b", "inv-group-http-api", []agentruntime.RuntimeDeliveryGroupMember{{Kind: agentruntime.RuntimeDeliveryKindEvent, OutboxID: "event-http-api", DeliveryID: "event-http-api"}}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := agentruntime.NewRuntimeDeliveryGroupEnvelope(group, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	envelope, err = agentruntime.SignRuntimeDeliveryGroupEnvelope(envelope, secret)
	if err != nil {
		t.Fatal(err)
	}
	withoutReceiver := NewServer(nil, nil).Handler()
	missing := httptest.NewRecorder()
	withoutReceiver.ServeHTTP(missing, httptest.NewRequest(http.MethodPost, "/api/v1/runtime/delivery-group/prepare", bytes.NewReader(mustJSON(t, envelope))))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("未显式挂载 group receiver 时应返回 404: status=%d body=%s", missing.Code, missing.Body.String())
	}

	repo := agentruntime.NewMemoryRepository()
	receiver, err := agentruntime.NewRuntimeDeliveryGroupReceiver("runtime-a", "runtime-b", secret, repo)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(nil, nil)
	server.SetRuntimeDeliveryGroupTransactionReceiver(receiver)
	handler := server.Handler()
	body := mustJSON(t, envelope)
	prepareRequest := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/delivery-group/prepare", bytes.NewReader(body))
	prepareRequest.Header.Set("Content-Type", "application/json")
	prepareRequest.Header.Set("X-Abot-Delivery-Group-Version", "1")
	prepareRequest.Header.Set("X-Abot-Delivery-Group-Signature", envelope.Signature)
	prepareRequest.Header.Set("X-Abot-Delivery-Group-Phase", "prepare")
	prepareRequest.Header.Set("Idempotency-Key", envelope.GroupID)
	prepared := httptest.NewRecorder()
	handler.ServeHTTP(prepared, prepareRequest)
	if prepared.Code != http.StatusOK {
		t.Fatalf("group prepare route 失败: status=%d body=%s", prepared.Code, prepared.Body.String())
	}

	statusRequest := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/delivery-group/status", bytes.NewReader(body))
	statusRequest.Header.Set("Content-Type", "application/json")
	status := httptest.NewRecorder()
	handler.ServeHTTP(status, statusRequest)
	if status.Code != http.StatusOK {
		t.Fatalf("group status route 失败: status=%d body=%s", status.Code, status.Body.String())
	}
	var remoteStatus agentruntime.RuntimeDeliveryGroupRemoteStatus
	if err := json.Unmarshal(status.Body.Bytes(), &remoteStatus); err != nil {
		t.Fatal(err)
	}
	if err := agentruntime.VerifyRuntimeDeliveryGroupStatus(remoteStatus, secret); err != nil {
		t.Fatal(err)
	}
	if remoteStatus.Phase != agentruntime.RuntimeDeliveryGroupTransactionPrepared || !remoteStatus.Found {
		t.Fatalf("group status 不正确: %#v", remoteStatus)
	}
	batchRequest := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/delivery-group/status/batch", bytes.NewReader(mustJSON(t, agentruntime.RuntimeDeliveryGroupStatusBatchRequest{Envelopes: []agentruntime.RuntimeDeliveryGroupEnvelope{envelope}})))
	batchRequest.Header.Set("Content-Type", "application/json")
	batchRequest.Header.Set("X-Abot-Delivery-Group-Version", "1")
	batchRequest.Header.Set("X-Abot-Delivery-Group-Batch-Count", "1")
	batch := httptest.NewRecorder()
	handler.ServeHTTP(batch, batchRequest)
	if batch.Code != http.StatusOK {
		t.Fatalf("group batch status route 失败: status=%d body=%s", batch.Code, batch.Body.String())
	}
	var batchResponse agentruntime.RuntimeDeliveryGroupStatusBatchResponse
	if err := json.Unmarshal(batch.Body.Bytes(), &batchResponse); err != nil {
		t.Fatal(err)
	}
	if len(batchResponse.Statuses) != 1 || batchResponse.Statuses[0].Phase != agentruntime.RuntimeDeliveryGroupTransactionPrepared {
		t.Fatalf("group batch status 不正确: %#v", batchResponse)
	}

}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
