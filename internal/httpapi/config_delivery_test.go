package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
)

func httpConfigDeliverySnapshot(t *testing.T, text string) (string, agent.RuntimeConfigSnapshot) {
	t.Helper()
	encoded := `{"version":1,"agent_definition_version":"kernel-runtime-v2","app_name":"abot","provider_id":"demo","model_id":"model","provider_protocol":"openai-compatible","persona_id":"persona:default","instruction_digest":"sha256:` + strings.Repeat("0", 64) + `","options":{"ai_enabled":true}}`
	if text != "" {
		// Keep the fixture metadata-only while making each target digest distinct.
		encoded = strings.Replace(encoded, strings.Repeat("0", 64), strings.Repeat(text[:1], 64), 1)
	}
	projection, err := agent.ParseRuntimeConfigSnapshot(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return encoded, projection
}

func TestRuntimeConfigDeliveryHTTPRouteAndTransactionalReceiver(t *testing.T) {
	ctx := context.Background()
	secret := []byte("config-http-delivery-secret-32-bytes")
	oldEncoded, _ := httpConfigDeliverySnapshot(t, "a")
	_, target := httpConfigDeliverySnapshot(t, "b")
	oldDigest := agent.RuntimeConfigSnapshotDigest(oldEncoded)

	// The public Server route is deliberately opt-in; once installed, the
	// source HTTP adapter can exercise the exact production path.
	receiverRepo := agentruntime.NewMemoryRepository()
	receiver, err := agentruntime.NewRuntimeConfigDeliveryReceiver("runtime-a", "runtime-b", secret, receiverRepo)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(nil, nil)
	server.SetRuntimeConfigDeliveryReceiver(receiver)
	server.SetRuntimeConfigDeliveryTransactionReceiver(receiver)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	transport, err := agentruntime.NewRuntimeConfigDeliveryHTTPTransport(httpServer.URL+"/api/v1/runtime/config-delivery", "runtime-a", "runtime-b", secret, httpServer.Client())
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := agentruntime.NewRuntimeConfigDeliveryOutbox("runtime-a", "runtime-b", "inv-config-http", oldDigest, "http-1", target, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := outbox.Envelope(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := transport.Deliver(ctx, envelope, target)
	if err != nil || receipt.Duplicate {
		t.Fatalf("HTTP config one-phase 首次投递失败: receipt=%#v err=%v", receipt, err)
	}
	retry := envelope
	retry.IssuedAt = retry.IssuedAt.Add(time.Second)
	retry.Signature = ""
	retryReceipt, err := transport.Deliver(ctx, retry, target)
	if err != nil || !retryReceipt.Duplicate {
		t.Fatalf("HTTP config 重试应 duplicate: receipt=%#v err=%v", retryReceipt, err)
	}
	if _, err := receiverRepo.GetRuntimeConfigDelivery(ctx, envelope.InvocationID); err != nil {
		t.Fatalf("HTTP config receiver inbox 缺失: %v", err)
	}

	// A local lock resolver is checked before Inbox acceptance and fails closed
	// when the destination has a different local configuration version.
	strictRepo := agentruntime.NewMemoryRepository()
	strictReceiver, err := agentruntime.NewRuntimeConfigDeliveryReceiver("runtime-a", "runtime-b", secret, strictRepo)
	if err != nil {
		t.Fatal(err)
	}
	strictReceiver.RequireLocalSnapshot = true
	strictReceiver.LocalSnapshotResolver = func(context.Context, string) (agent.RuntimeConfigSnapshot, error) {
		_, local := httpConfigDeliverySnapshot(t, "c")
		return local, nil
	}
	strictServer := httptest.NewServer(strictReceiver.Handler())
	defer strictServer.Close()
	strictTransport, err := agentruntime.NewRuntimeConfigDeliveryHTTPTransport(strictServer.URL, "runtime-a", "runtime-b", secret, strictServer.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strictTransport.Deliver(ctx, envelope, target); err == nil {
		t.Fatalf("严格本地版本不匹配应失败，实际=%v", err)
	}

	// The two-phase endpoints are mounted by the same Server setter and keep
	// the destination inbox invisible until commit.
	txnRepo := agentruntime.NewMemoryRepository()
	txnReceiver, err := agentruntime.NewRuntimeConfigDeliveryReceiver("runtime-a", "runtime-b", secret, txnRepo)
	if err != nil {
		t.Fatal(err)
	}
	txnServer := httptest.NewServer(funcHandlerMux(t, txnReceiver))
	defer txnServer.Close()
	txnTransport, err := agentruntime.NewRuntimeConfigDeliveryHTTPTransactionalTransport(txnServer.URL, "runtime-a", "runtime-b", secret, txnServer.Client())
	if err != nil {
		t.Fatal(err)
	}
	txnOutbox, err := agentruntime.NewRuntimeConfigDeliveryOutbox("runtime-a", "runtime-b", "inv-config-http-txn", oldDigest, "txn-1", target, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	txnEnvelope, err := txnOutbox.Envelope(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if receipt, err := txnTransport.Prepare(ctx, txnEnvelope, target); err != nil || receipt.Phase != agentruntime.RuntimeConfigDeliveryTransactionPhasePrepared {
		t.Fatalf("HTTP config prepare 失败: receipt=%#v err=%v", receipt, err)
	}
	if _, err := txnRepo.GetRuntimeConfigDelivery(ctx, txnEnvelope.InvocationID); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("prepare 后不应暴露 inbox: %v", err)
	}
	if receipt, err := txnTransport.Commit(ctx, txnEnvelope, target); err != nil || receipt.Phase != agentruntime.RuntimeConfigDeliveryTransactionPhaseCommitted {
		t.Fatalf("HTTP config commit 失败: receipt=%#v err=%v", receipt, err)
	}
	if _, err := txnRepo.GetRuntimeConfigDelivery(ctx, txnEnvelope.InvocationID); err != nil {
		t.Fatalf("commit 后 inbox 缺失: %v", err)
	}
}

func funcHandlerMux(t *testing.T, receiver *agentruntime.RuntimeConfigDeliveryReceiver) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/prepare", receiver.PrepareHandler())
	mux.Handle("/commit", receiver.CommitHandler())
	return mux
}
