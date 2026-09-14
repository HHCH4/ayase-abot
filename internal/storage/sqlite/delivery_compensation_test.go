package sqlite

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

func TestSQLiteRuntimeDeliveryCompensationIsDurableAndCASProtected(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	now := time.Now().UTC().Add(-time.Second)
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-sqlite-compensation", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	compRepo, ok := repo.(agentruntime.RuntimeDeliveryCompensationRepository)
	if !ok {
		_ = store.Close()
		t.Fatal("SQLite Runtime Repository 未实现 RuntimeDeliveryCompensationRepository")
	}
	item, err := agentruntime.NewRuntimeDeliveryCompensation(agentruntime.RuntimeDeliveryKindEvent, "runtime-a", "runtime-b", "sqlite-compensation-delivery", invocation.ID, "sqlite-event-outbox", "sqlite-group", 8, now)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	const workers = 8
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
			_ = store.Close()
			t.Fatalf("SQLite 相同 compensation 并发入队失败: %v", enqueueErr)
		}
	}
	loaded, err := compRepo.GetRuntimeDeliveryCompensation(ctx, item.ID)
	if err != nil || loaded.ID != item.ID || loaded.LeaseOwner != "" {
		_ = store.Close()
		t.Fatalf("SQLite compensation 入队读取错误: %#v err=%v", loaded, err)
	}
	claimed, ok, err := compRepo.ClaimRuntimeDeliveryCompensation(ctx, "sqlite-worker-a", now.Add(2*time.Second), time.Minute)
	if err != nil || !ok || claimed.Attempt != 1 || claimed.Status != agentruntime.RuntimeDeliveryCompensationProcessing {
		_ = store.Close()
		t.Fatalf("SQLite compensation claim 失败: %#v ok=%v err=%v", claimed, ok, err)
	}
	if completed, err := compRepo.CompleteRuntimeDeliveryCompensation(ctx, claimed.ID, "wrong-owner", now.Add(2*time.Second)); err != nil || completed {
		_ = store.Close()
		t.Fatalf("SQLite 错误 owner 不应完成 compensation: completed=%v err=%v", completed, err)
	}
	if deferred, err := compRepo.DeferRuntimeDeliveryCompensation(ctx, claimed.ID, "sqlite-worker-a", now.Add(2*time.Second), now.Add(3*time.Second), "source still processing"); err != nil || !deferred {
		_ = store.Close()
		t.Fatalf("SQLite defer 失败: deferred=%v err=%v", deferred, err)
	}
	deferredItem, err := compRepo.GetRuntimeDeliveryCompensation(ctx, item.ID)
	if err != nil || deferredItem.Status != agentruntime.RuntimeDeliveryCompensationQueued || deferredItem.Attempt != 0 {
		_ = store.Close()
		t.Fatalf("SQLite defer 不应消耗 retry budget: %#v err=%v", deferredItem, err)
	}
	claimed, ok, err = compRepo.ClaimRuntimeDeliveryCompensation(ctx, "sqlite-worker-a", now.Add(4*time.Second), time.Minute)
	if err != nil || !ok || claimed.Attempt != 1 {
		_ = store.Close()
		t.Fatalf("SQLite defer 后重新 claim 失败: %#v ok=%v err=%v", claimed, ok, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	compRepo = store.RuntimeRepository().(agentruntime.RuntimeDeliveryCompensationRepository)
	loaded, err = compRepo.GetRuntimeDeliveryCompensation(ctx, item.ID)
	if err != nil || loaded.Status != agentruntime.RuntimeDeliveryCompensationProcessing || loaded.LeaseOwner != "sqlite-worker-a" {
		t.Fatalf("SQLite 重启后 processing lease 不可读: %#v err=%v", loaded, err)
	}
	taken, ok, err := compRepo.ClaimRuntimeDeliveryCompensation(ctx, "sqlite-worker-b", now.Add(64*time.Second), time.Minute)
	if err != nil || !ok || taken.LeaseOwner != "sqlite-worker-b" || taken.Attempt != 2 {
		t.Fatalf("SQLite 过期 lease 接管失败: %#v ok=%v err=%v", taken, ok, err)
	}
	if completed, err := compRepo.CompleteRuntimeDeliveryCompensation(ctx, taken.ID, "sqlite-worker-b", now.Add(64*time.Second)); err != nil || !completed {
		t.Fatalf("SQLite compensation 完成失败: completed=%v err=%v", completed, err)
	}
	loaded, err = compRepo.GetRuntimeDeliveryCompensation(ctx, item.ID)
	if err != nil || loaded.Status != agentruntime.RuntimeDeliveryCompensationCompleted || loaded.CompletedAt == nil {
		t.Fatalf("SQLite completed 状态错误: %#v err=%v", loaded, err)
	}
	items, err := compRepo.ListRuntimeDeliveryCompensations(ctx, invocation.ID, agentruntime.RuntimeDeliveryKindEvent, agentruntime.RuntimeDeliveryCompensationCompleted, 10)
	if err != nil || len(items) != 1 || items[0].ID != item.ID {
		t.Fatalf("SQLite compensation list 错误: %#v err=%v", items, err)
	}
	if _, err := compRepo.GetRuntimeDeliveryCompensation(ctx, "missing-compensation"); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("缺失 compensation 应返回 ErrNotFound: %v", err)
	}
}
