package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRuntimeDeliveryCompensationDomainIsMetadataOnlyAndBounded(t *testing.T) {
	now := time.Date(2026, 9, 13, 11, 0, 0, 0, time.UTC)
	item, err := NewRuntimeDeliveryCompensation(RuntimeDeliveryKindEvent, "runtime-a", "runtime-b", "delivery-1", "inv-compensation-domain", "outbox-1", "group-1", 8, now)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	body := string(encoded)
	for _, forbidden := range []string{"lease_owner", "lease_expires_at", "prompt", "projection", "reason"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("compensation metadata 泄露字段 %q: %s", forbidden, body)
		}
	}
	if item.ID != RuntimeDeliveryCompensationID(item.Kind, item.Source, item.Destination, item.DeliveryID) || item.Status != RuntimeDeliveryCompensationQueued || item.Attempt != 0 {
		t.Fatalf("稳定 identity/status 错误: %#v", item)
	}
	invalid := item
	invalid.ID = "forged"
	if _, err := invalid.Normalize(now); !errors.Is(err, ErrInvalidRuntimeDeliveryCompensation) {
		t.Fatalf("伪造 ID 应拒绝: %v", err)
	}
	invalid = item
	invalid.SourceAttempt = MaxRuntimeDeliveryCompensationAttempts - 1
	if _, err := invalid.Normalize(now); !errors.Is(err, ErrInvalidRuntimeDeliveryCompensation) {
		t.Fatalf("非最终 source attempt 应拒绝: %v", err)
	}
	invalid.SourceAttempt = MaxRuntimeDeliveryCompensationAttempts + 1
	if _, err := invalid.Normalize(now); !errors.Is(err, ErrInvalidRuntimeDeliveryCompensation) {
		t.Fatalf("超出 source attempt 应拒绝: %v", err)
	}
	invalid = item
	invalid.Status = RuntimeDeliveryCompensationProcessing
	if _, err := invalid.Normalize(now); !errors.Is(err, ErrInvalidRuntimeDeliveryCompensation) {
		t.Fatalf("无 lease 的 processing 应拒绝: %v", err)
	}
}

func TestRuntimeDeliveryCompensationEnqueueFailureIsSanitized(t *testing.T) {
	err := runtimeDeliveryCompensationEnqueueFailure(errors.New("commit failed"), errors.New("api_key=secret-value"))
	if err == nil || strings.Contains(err.Error(), "secret-value") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("compensation enqueue 错误必须脱敏且保留主错误: %v", err)
	}
}

func TestMemoryRuntimeDeliveryCompensationEnqueueClaimRetryAndExpiry(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 11, 1, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-compensation-memory", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	item, err := NewRuntimeDeliveryCompensation(RuntimeDeliveryKindConfig, "runtime-a", "runtime-b", "delivery-memory", invocation.ID, "config-outbox", "group-memory", 8, now)
	if err != nil {
		t.Fatal(err)
	}
	compRepo := RuntimeDeliveryCompensationRepository(repo)
	const workers = 16
	results := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, enqueueErr := compRepo.EnqueueRuntimeDeliveryCompensation(ctx, item)
			results <- enqueueErr
		}()
	}
	wg.Wait()
	close(results)
	for enqueueErr := range results {
		if enqueueErr != nil {
			t.Fatalf("相同 identity 并发入队不应失败: %v", enqueueErr)
		}
	}
	items, err := compRepo.ListRuntimeDeliveryCompensations(ctx, invocation.ID, RuntimeDeliveryKindConfig, "", 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("并发入队必须只有一个 row: %#v err=%v", items, err)
	}
	claimed, ok, err := compRepo.ClaimRuntimeDeliveryCompensation(ctx, "worker-a", now, time.Minute)
	if err != nil || !ok || claimed.Attempt != 1 || claimed.Status != RuntimeDeliveryCompensationProcessing {
		t.Fatalf("claim 失败: %#v ok=%v err=%v", claimed, ok, err)
	}
	if completed, err := compRepo.CompleteRuntimeDeliveryCompensation(ctx, claimed.ID, "wrong-worker", now); err != nil || completed {
		t.Fatalf("错误 owner 不应完成 compensation: completed=%v err=%v", completed, err)
	}
	if retried, err := compRepo.RetryRuntimeDeliveryCompensation(ctx, claimed.ID, "worker-a", now, "temporary network issue"); err != nil || !retried {
		t.Fatalf("retry 失败: retried=%v err=%v", retried, err)
	}
	if _, ok, err := compRepo.ClaimRuntimeDeliveryCompensation(ctx, "worker-b", now, time.Minute); err != nil || ok {
		t.Fatalf("退避窗口内不应再次 claim: ok=%v err=%v", ok, err)
	}
	claimed, ok, err = compRepo.ClaimRuntimeDeliveryCompensation(ctx, "worker-b", now.Add(2*time.Second), time.Minute)
	if err != nil || !ok || claimed.Attempt != 2 {
		t.Fatalf("退避后 claim 失败: %#v ok=%v err=%v", claimed, ok, err)
	}
	if deferred, err := compRepo.DeferRuntimeDeliveryCompensation(ctx, claimed.ID, "worker-b", now.Add(2*time.Second), now.Add(5*time.Second), "source still processing"); err != nil || !deferred {
		t.Fatalf("defer 失败: deferred=%v err=%v", deferred, err)
	}
	deferredItem, err := compRepo.GetRuntimeDeliveryCompensation(ctx, item.ID)
	if err != nil || deferredItem.Attempt != 1 || deferredItem.Status != RuntimeDeliveryCompensationQueued {
		t.Fatalf("defer 不应消耗 compensation retry budget: %#v err=%v", deferredItem, err)
	}
	if _, ok, err := compRepo.ClaimRuntimeDeliveryCompensation(ctx, "worker-c", now.Add(4*time.Second), time.Minute); err != nil || ok {
		t.Fatalf("defer 后在 available_at 前不应 claim: ok=%v err=%v", ok, err)
	}
	claimed, ok, err = compRepo.ClaimRuntimeDeliveryCompensation(ctx, "worker-c", now.Add(6*time.Second), time.Minute)
	if err != nil || !ok {
		t.Fatalf("defer 后应可重新 claim: %#v ok=%v err=%v", claimed, ok, err)
	}
	if completed, err := compRepo.CompleteRuntimeDeliveryCompensation(ctx, claimed.ID, "worker-c", now.Add(6*time.Second)); err != nil || !completed {
		t.Fatalf("最终完成失败: completed=%v err=%v", completed, err)
	}
	stored, err := compRepo.GetRuntimeDeliveryCompensation(ctx, item.ID)
	if err != nil || stored.Status != RuntimeDeliveryCompensationCompleted || stored.CompletedAt == nil || stored.LeaseOwner != "" {
		t.Fatalf("完成后的 compensation 状态错误: %#v err=%v", stored, err)
	}
}

func TestMemoryRuntimeDeliveryCompensationExpiredLeaseCanBeTakenOver(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 11, 2, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	item, err := NewRuntimeDeliveryCompensation(RuntimeDeliveryKindRejection, "runtime-a", "runtime-b", "delivery-expiry", "inv-compensation-expiry", "rejection-outbox", "", 8, now)
	if err != nil {
		t.Fatal(err)
	}
	compRepo := RuntimeDeliveryCompensationRepository(repo)
	if _, err := compRepo.EnqueueRuntimeDeliveryCompensation(ctx, item); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := compRepo.ClaimRuntimeDeliveryCompensation(ctx, "worker-a", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("初始 claim 失败: %#v ok=%v err=%v", claimed, ok, err)
	}
	if _, ok, err := compRepo.ClaimRuntimeDeliveryCompensation(ctx, "worker-b", now.Add(30*time.Second), time.Minute); err != nil || ok {
		t.Fatalf("未过期 lease 不应被接管: ok=%v err=%v", ok, err)
	}
	taken, ok, err := compRepo.ClaimRuntimeDeliveryCompensation(ctx, "worker-b", now.Add(61*time.Second), time.Minute)
	if err != nil || !ok || taken.LeaseOwner != "worker-b" || taken.Attempt != 2 {
		t.Fatalf("过期 lease 接管失败: %#v ok=%v err=%v", taken, ok, err)
	}
	if completed, err := compRepo.CompleteRuntimeDeliveryCompensation(ctx, taken.ID, "worker-a", now.Add(61*time.Second)); err != nil || completed {
		t.Fatalf("旧 owner 不应完成被接管的 lease: completed=%v err=%v", completed, err)
	}
	if deferred, err := compRepo.DeferRuntimeDeliveryCompensation(ctx, taken.ID, "worker-b", now.Add(122*time.Second), now.Add(123*time.Second), "late worker"); err != nil || deferred {
		t.Fatalf("过期 owner 不应迟到 defer: deferred=%v err=%v", deferred, err)
	}
}
