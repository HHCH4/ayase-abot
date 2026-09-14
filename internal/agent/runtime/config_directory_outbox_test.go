package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type runtimeConfigDirectoryTransportFunc func(context.Context, RuntimeConfigDirectoryEnvelope, RuntimeConfigDirectoryEntry) (RuntimeConfigDirectoryReceipt, error)

func (f runtimeConfigDirectoryTransportFunc) Deliver(ctx context.Context, envelope RuntimeConfigDirectoryEnvelope, entry RuntimeConfigDirectoryEntry) (RuntimeConfigDirectoryReceipt, error) {
	return f(ctx, envelope, entry)
}

type runtimeConfigDirectoryReconcilerFunc struct {
	statusFn  func(RuntimeConfigDirectoryEnvelope, RuntimeConfigDirectoryEntry) (RuntimeConfigDirectoryStatus, error)
	deliverFn func(context.Context, RuntimeConfigDirectoryEnvelope, RuntimeConfigDirectoryEntry) (RuntimeConfigDirectoryReceipt, error)
}

func (f runtimeConfigDirectoryReconcilerFunc) Deliver(ctx context.Context, envelope RuntimeConfigDirectoryEnvelope, entry RuntimeConfigDirectoryEntry) (RuntimeConfigDirectoryReceipt, error) {
	return f.deliverFn(ctx, envelope, entry)
}

func (f runtimeConfigDirectoryReconcilerFunc) ReconcileRuntimeConfigDirectory(_ context.Context, envelope RuntimeConfigDirectoryEnvelope, entry RuntimeConfigDirectoryEntry) (RuntimeConfigDirectoryStatus, error) {
	return f.statusFn(envelope, entry)
}

func TestRuntimeConfigDirectoryOutboxMemoryLeaseRetryAndDefensiveIdentity(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 23, 0, 0, 0, time.UTC)
	entry := configDirectoryProfile(t, "profile-outbox", 1, "one")
	item, err := NewRuntimeConfigDirectoryOutbox("runtime-a", "runtime-b", entry, "", "corr-outbox", "key-outbox", now)
	if err != nil {
		t.Fatal(err)
	}
	repo := NewMemoryRepository()
	saved, err := repo.EnqueueRuntimeConfigDirectoryOutbox(ctx, item)
	if err != nil || saved.ID != item.ID || saved.Status != RuntimeConfigDirectoryOutboxQueued {
		t.Fatalf("首次入队失败: %#v err=%v", saved, err)
	}
	duplicate, err := repo.EnqueueRuntimeConfigDirectoryOutbox(ctx, item)
	if err != nil || duplicate.ID != item.ID || duplicate.Attempt != 0 {
		t.Fatalf("重复入队必须返回现有 cursor: %#v err=%v", duplicate, err)
	}
	duplicate.Entry.Values["safe.value"] = "mutated"
	stored, err := repo.GetRuntimeConfigDirectoryOutbox(ctx, item.ID)
	if err != nil || stored.Entry.Values["safe.value"] != "one" {
		t.Fatalf("outbox Get 必须返回正文防御性副本: %#v err=%v", stored, err)
	}

	claimed, ok, err := repo.ClaimRuntimeConfigDirectoryOutbox(ctx, "worker-a", now, 30*time.Second)
	if err != nil || !ok || claimed.Status != RuntimeConfigDirectoryOutboxProcessing || claimed.Attempt != 1 || claimed.LeaseOwner != "worker-a" {
		t.Fatalf("首次 claim 错误: %#v ok=%v err=%v", claimed, ok, err)
	}
	if completed, err := repo.CompleteRuntimeConfigDirectoryOutbox(ctx, item.ID, "worker-b", now.Add(time.Second)); err != nil || completed {
		t.Fatalf("错误 owner 不得完成 cursor: completed=%v err=%v", completed, err)
	}
	if retried, err := repo.RetryRuntimeConfigDirectoryOutbox(ctx, item.ID, "worker-a", now.Add(time.Second), "Authorization: Bearer secret api_key=plain-secret"); err != nil || !retried {
		t.Fatalf("首次 retry 失败: retried=%v err=%v", retried, err)
	}
	stored, err = repo.GetRuntimeConfigDirectoryOutbox(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != RuntimeConfigDirectoryOutboxQueued || stored.Attempt != 1 || !stored.AvailableAt.Equal(now.Add(2*time.Second)) {
		t.Fatalf("retry 应按 attempt=1 退避: %#v", stored)
	}
	if strings.Contains(stored.LastError, "secret") || strings.Contains(stored.LastError, "plain-secret") || !strings.Contains(stored.LastError, "[REDACTED]") {
		t.Fatalf("retry 错误必须脱敏: %q", stored.LastError)
	}
	if _, ok, err := repo.ClaimRuntimeConfigDirectoryOutbox(ctx, "worker-b", now.Add(1500*time.Millisecond), time.Second); err != nil || ok {
		t.Fatalf("退避窗口内不应重新 claim: ok=%v err=%v", ok, err)
	}
	claimed, ok, err = repo.ClaimRuntimeConfigDirectoryOutbox(ctx, "worker-b", now.Add(2*time.Second), time.Second)
	if err != nil || !ok || claimed.Attempt != 2 || claimed.LeaseOwner != "worker-b" {
		t.Fatalf("退避到期后应由新 worker 接管: %#v ok=%v err=%v", claimed, ok, err)
	}
	if completed, err := repo.CompleteRuntimeConfigDirectoryOutbox(ctx, item.ID, "worker-b", now.Add(4*time.Second)); err != nil || completed {
		t.Fatalf("过期 lease 不得完成: completed=%v err=%v", completed, err)
	}
	if completed, err := repo.CompleteRuntimeConfigDirectoryOutbox(ctx, item.ID, "worker-b", now.Add(2500*time.Millisecond)); err != nil || !completed {
		t.Fatalf("有效 lease 应可完成: completed=%v err=%v", completed, err)
	}
	stored, err = repo.GetRuntimeConfigDirectoryOutbox(ctx, item.ID)
	if err != nil || stored.Status != RuntimeConfigDirectoryOutboxCompleted || stored.LeaseOwner != "" {
		t.Fatalf("完成后状态/lease 错误: %#v err=%v", stored, err)
	}
	if _, err := repo.EnqueueRuntimeConfigDirectoryOutbox(ctx, item); err != nil {
		t.Fatalf("终态 cursor 仍应支持幂等 enqueue: %v", err)
	}

	other := entry
	other.Values = map[string]any{"safe.value": "other"}
	conflicting := item
	conflicting.Entry = other
	if _, err := repo.EnqueueRuntimeConfigDirectoryOutbox(ctx, conflicting); !errors.Is(err, ErrRuntimeConfigDirectoryAuth) && !errors.Is(err, ErrInvalidRuntimeConfigDirectory) {
		t.Fatalf("cursor body 被篡改必须拒绝: %v", err)
	}
}

func TestRuntimeConfigDirectoryOutboxEnvelopePreservesPublicationIdentity(t *testing.T) {
	now := time.Date(2026, 9, 13, 23, 10, 0, 0, time.UTC)
	entry := configDirectoryProfile(t, "profile-envelope", 3, "stable")
	item, err := NewRuntimeConfigDirectoryOutbox("runtime-a", "runtime-b", entry, "", "corr-envelope", "key-envelope", now)
	if err != nil {
		t.Fatal(err)
	}
	first, err := item.Envelope(now)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := item.Envelope(now.Add(2 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if first.DeliveryID != retry.DeliveryID || !first.Timestamp.Equal(retry.Timestamp) || first.IssuedAt.Equal(retry.IssuedAt) || first.BodyDigest != retry.BodyDigest {
		t.Fatalf("重试不得改变 publication identity: first=%#v retry=%#v", first, retry)
	}
	if first.Signature != "" || retry.Signature != "" {
		t.Fatal("outbox Envelope 只应生成待签名 envelope")
	}
}

func TestRuntimeConfigDirectoryStatusReconciliationIsMetadataOnly(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 23, 20, 0, 0, time.UTC)
	secret := []byte(configDirectoryTestSecret)
	repo := NewMemoryRepository()
	receiver, err := NewRuntimeConfigDirectoryReceiver("runtime-a", "runtime-b", secret, repo)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now }
	server := httptest.NewServer(receiver.Handler())
	defer server.Close()
	transport, err := NewRuntimeConfigDirectoryHTTPTransport(server.URL, "runtime-a", "runtime-b", secret, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	entry := configDirectoryProfile(t, "profile-status", 1, "proof")
	envelope, err := NewRuntimeConfigDirectoryEnvelope("runtime-a", "runtime-b", entry, "", "corr-status", "key-status", now)
	if err != nil {
		t.Fatal(err)
	}
	absent, err := transport.ReconcileRuntimeConfigDirectory(ctx, envelope, entry)
	if err != nil || absent.Phase != RuntimeConfigDirectoryStatusAbsent || absent.Found {
		t.Fatalf("未接收正文应返回 absent proof: %#v err=%v", absent, err)
	}
	if encoded := absent.Signature; encoded == "" {
		t.Fatal("status proof 必须签名")
	}
	if _, err := transport.Deliver(ctx, envelope, entry); err != nil {
		t.Fatal(err)
	}
	accepted, err := transport.ReconcileRuntimeConfigDirectory(ctx, envelope, entry)
	if err != nil || accepted.Phase != RuntimeConfigDirectoryStatusAccepted || !accepted.Found || accepted.ReceivedAt.IsZero() {
		t.Fatalf("已接收正文应返回 accepted proof: %#v err=%v", accepted, err)
	}
	if err := accepted.ValidateAgainst(envelope); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRuntimeConfigDirectoryStatus(accepted, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := transport.ReconcileRuntimeConfigDirectory(ctx, envelope, configDirectoryProfile(t, "profile-status", 2, "different")); !errors.Is(err, ErrRuntimeConfigDirectoryAuth) {
		t.Fatalf("status 查询的 entry identity 不一致必须拒绝: %v", err)
	}

	// The status route is explicit and must not return the body even when it is
	// accepted. A direct request also proves the receiver path is mounted by the
	// suffix-aware handler without a second body endpoint.
	signed, err := SignRuntimeConfigDirectoryEnvelope(envelope, secret)
	if err != nil {
		t.Fatal(err)
	}
	body, err := jsonMarshalForTest(signed)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, server.URL+"/status", strings.NewReader(body))
	request.Header.Set("X-Abot-Config-Directory-Version", "1")
	request.Header.Set("X-Abot-Config-Directory-Signature", signed.Signature)
	request.Header.Set("Idempotency-Key", signed.DeliveryID)
	response := httptest.NewRecorder()
	receiver.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "instruction") || strings.Contains(response.Body.String(), "values") {
		t.Fatalf("status response 必须 metadata-only: code=%d body=%s", response.Code, response.Body.String())
	}
}

func jsonMarshalForTest(value any) (string, error) {
	encoded, err := json.Marshal(value)
	return string(encoded), err
}

func TestCoordinatorDirectoryOutboxUsesStatusProofBeforeDeliver(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 23, 30, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	firstEntry := configDirectoryProfile(t, "profile-proof", 1, "already-accepted")
	secondEntry := configDirectoryProfile(t, "profile-send", 1, "must-send")
	first, err := NewRuntimeConfigDirectoryOutbox("runtime-a", "runtime-b", firstEntry, "", "corr-proof", "key-proof", now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewRuntimeConfigDirectoryOutbox("runtime-a", "runtime-b", secondEntry, "", "corr-send", "key-send", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeConfigDirectoryOutbox(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeConfigDirectoryOutbox(ctx, second); err != nil {
		t.Fatal(err)
	}
	var proofCalls atomic.Int32
	var deliverCalls atomic.Int32
	transport := runtimeConfigDirectoryReconcilerFunc{
		statusFn: func(envelope RuntimeConfigDirectoryEnvelope, _ RuntimeConfigDirectoryEntry) (RuntimeConfigDirectoryStatus, error) {
			proofCalls.Add(1)
			if envelope.EntryID == first.EntryID {
				return NewRuntimeConfigDirectoryStatus(envelope, RuntimeConfigDirectoryStatusAccepted, true, now)
			}
			return NewRuntimeConfigDirectoryStatus(envelope, RuntimeConfigDirectoryStatusAbsent, false, time.Time{})
		},
		deliverFn: func(_ context.Context, envelope RuntimeConfigDirectoryEnvelope, _ RuntimeConfigDirectoryEntry) (RuntimeConfigDirectoryReceipt, error) {
			deliverCalls.Add(1)
			return RuntimeConfigDirectoryReceipt{Version: envelope.Version, Source: envelope.Source, Destination: envelope.Destination, DeliveryID: envelope.DeliveryID, Kind: envelope.Kind, EntryID: envelope.EntryID, Revision: envelope.Revision, BodyDigest: envelope.BodyDigest, ReceivedAt: now}, nil
		},
	}
	coordinator := &Coordinator{repo: repo, configDirectoryTransport: transport, configDirectorySource: "runtime-a", configDirectoryDestination: "runtime-b", configDirectoryOutboxWorkerID: "directory-worker"}
	coordinator.DispatchDueRuntimeDeliveries(ctx, now)
	proof, err := repo.GetRuntimeConfigDirectoryOutbox(ctx, first.ID)
	if err != nil || proof.Status != RuntimeConfigDirectoryOutboxCompleted {
		t.Fatalf("accepted status proof 应直接收口 cursor: %#v err=%v", proof, err)
	}
	sent, err := repo.GetRuntimeConfigDirectoryOutbox(ctx, second.ID)
	if err != nil || sent.Status != RuntimeConfigDirectoryOutboxCompleted {
		t.Fatalf("absent proof 后应执行一次投递并完成: %#v err=%v", sent, err)
	}
	if proofCalls.Load() != 2 || deliverCalls.Load() != 1 {
		t.Fatalf("proof/deliver 次数错误，accepted 不应重复发送: proof=%d deliver=%d", proofCalls.Load(), deliverCalls.Load())
	}
	coordinator.DispatchDueRuntimeDeliveries(ctx, now.Add(time.Minute))
	if deliverCalls.Load() != 1 {
		t.Fatalf("completed cursor 不应在后续轮次重复投递: %d", deliverCalls.Load())
	}
}
