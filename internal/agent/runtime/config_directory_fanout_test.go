package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRuntimeConfigDirectoryFanoutPlanIsStableAndAtomic(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	entry := configDirectoryProfile(t, "profile-fanout", 1, "fanout")
	destinations := []string{"runtime-c", "runtime-a", "runtime-b"}
	plan, children, err := NewRuntimeConfigDirectoryFanoutPlan("runtime-source", destinations, entry, "", "fanout-correlation", "fanout-idempotency", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 3 || plan.Status != RuntimeConfigDirectoryFanoutQueued || plan.PendingCount != 3 {
		t.Fatalf("fanout 初始摘要错误: %#v children=%d", plan, len(children))
	}
	for index, destination := range []string{"runtime-a", "runtime-b", "runtime-c"} {
		if plan.Destinations[index] != destination || children[index].Destination != destination || plan.OutboxIDs[index] != children[index].ID {
			t.Fatalf("fanout route 未稳定排序: plan=%#v child=%#v", plan, children)
		}
	}
	reordered, reorderedChildren, err := NewRuntimeConfigDirectoryFanoutPlan("runtime-source", []string{"runtime-b", "runtime-c", "runtime-a"}, entry, "", "fanout-correlation", "fanout-idempotency", now)
	if err != nil || reordered.ID != plan.ID {
		t.Fatalf("目的端顺序不应改变稳定 plan ID: first=%#v second=%#v err=%v", plan, reordered, err)
	}
	for index := range children {
		if reorderedChildren[index].ID != children[index].ID {
			t.Fatalf("目的端顺序不应改变 child ID: first=%#v second=%#v", children, reorderedChildren)
		}
	}

	repo := NewMemoryRepository()
	saved, err := repo.EnqueueRuntimeConfigDirectoryFanout(context.Background(), plan, children)
	if err != nil || !saved.Matches(plan) {
		t.Fatalf("fanout 原子登记失败: %#v err=%v", saved, err)
	}
	duplicate, err := repo.EnqueueRuntimeConfigDirectoryFanout(context.Background(), reordered, reorderedChildren)
	if err != nil || duplicate.ID != plan.ID || duplicate.PendingCount != 3 {
		t.Fatalf("fanout 重复登记不应创建第二个 parent: %#v err=%v", duplicate, err)
	}
	children[0].Entry.Values["safe.value"] = "mutated"
	storedChild, err := repo.GetRuntimeConfigDirectoryOutbox(context.Background(), plan.OutboxIDs[0])
	if err != nil || storedChild.Entry.Values["safe.value"] != "fanout" {
		t.Fatalf("fanout child 必须保留防御性正文副本: %#v err=%v", storedChild, err)
	}
	returned := saved
	returned.Destinations[0] = "mutated"
	storedPlan, err := repo.GetRuntimeConfigDirectoryFanout(context.Background(), plan.ID)
	if err != nil || storedPlan.Destinations[0] != "runtime-a" {
		t.Fatalf("fanout parent 必须返回防御性 route 副本: %#v err=%v", storedPlan, err)
	}

	badChildren := append([]RuntimeConfigDirectoryOutbox(nil), reorderedChildren...)
	badChildren[1].BodyDigest = "sha256:" + "f" + strings.Repeat("f", 63)
	if _, err := repo.EnqueueRuntimeConfigDirectoryFanout(context.Background(), reordered, badChildren); err == nil {
		t.Fatal("child digest 不一致时必须整批拒绝")
	}
	if _, err := repo.GetRuntimeConfigDirectoryFanout(context.Background(), reordered.ID); err != nil {
		t.Fatalf("失败的 fanout 登记不得破坏已有 parent: %v", err)
	}
	duplicateRoutePlan := reordered
	duplicateRoutePlan.OutboxIDs = append([]string(nil), reordered.OutboxIDs...)
	duplicateRoutePlan.OutboxIDs[1] = duplicateRoutePlan.OutboxIDs[0]
	if _, err := duplicateRoutePlan.Normalize(now); err == nil {
		t.Fatal("fanout 不得接受重复 child outbox ID")
	}
}

func TestMemoryRuntimeConfigDirectoryFanoutReconciliationAndConcurrentIdempotency(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	entry := configDirectoryProfile(t, "profile-fanout-reconcile", 1, "fanout")
	plan, children, err := NewRuntimeConfigDirectoryFanoutPlan("runtime-source", []string{"runtime-a", "runtime-b", "runtime-c"}, entry, "", "corr", "idempotency", now)
	if err != nil {
		t.Fatal(err)
	}
	repo := NewMemoryRepository()
	var wait sync.WaitGroup
	for index := 0; index < 8; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := repo.EnqueueRuntimeConfigDirectoryFanout(ctx, plan, children); err != nil {
				t.Errorf("并发幂等登记失败: %v", err)
			}
		}()
	}
	wait.Wait()
	if items, err := repo.ListRuntimeConfigDirectoryFanouts(ctx, "runtime-source", "", 10); err != nil || len(items) != 1 {
		t.Fatalf("并发登记不应创建多个 fanout parent: items=%#v err=%v", items, err)
	}
	claimed, ok, err := repo.ClaimRuntimeConfigDirectoryOutbox(ctx, "worker-a", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim 第一个 child 失败: %#v ok=%v err=%v", claimed, ok, err)
	}
	if _, err := repo.CompleteRuntimeConfigDirectoryOutbox(ctx, claimed.ID, "worker-a", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	partial, err := repo.ReconcileRuntimeConfigDirectoryFanout(ctx, plan.ID, now.Add(time.Second))
	if err != nil || partial.Status != RuntimeConfigDirectoryFanoutPartial || partial.CompletedCount != 1 || partial.PendingCount != 2 {
		t.Fatalf("一个 child 完成后应为 partial: %#v err=%v", partial, err)
	}
	claimed, ok, err = repo.ClaimRuntimeConfigDirectoryOutbox(ctx, "worker-b", now.Add(2*time.Second), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim 第二个 child 失败: %#v ok=%v err=%v", claimed, ok, err)
	}
	if _, err := repo.RetryRuntimeConfigDirectoryOutbox(ctx, claimed.ID, "worker-b", now.Add(3*time.Second), "secret=fanout-secret"); err != nil {
		t.Fatal(err)
	}
	failedPartial, err := repo.ReconcileRuntimeConfigDirectoryFanout(ctx, plan.ID, now.Add(3*time.Second))
	if err != nil || failedPartial.Status != RuntimeConfigDirectoryFanoutPartial || failedPartial.FailedCount != 0 {
		t.Fatalf("queued retry child 仍应是 partial/pending: %#v err=%v", failedPartial, err)
	}
	// Drain the remaining children: one retryable child is completed, and the
	// retried child is eventually marked failed at the bounded attempt limit.
	claimed, ok, err = repo.ClaimRuntimeConfigDirectoryOutbox(ctx, "worker-c", now.Add(5*time.Second), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim 第三个 child 失败: %#v ok=%v err=%v", claimed, ok, err)
	}
	if _, err := repo.CompleteRuntimeConfigDirectoryOutbox(ctx, claimed.ID, "worker-c", now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReconcileRuntimeConfigDirectoryFanout(ctx, plan.ID, now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	at := now.Add(10 * time.Second)
	for attempt := 0; attempt < MaxRuntimeConfigDirectoryOutboxAttempts; attempt++ {
		claimed, ok, err = repo.ClaimRuntimeConfigDirectoryOutbox(ctx, "worker-retry", at, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			continue
		}
		if _, err := repo.RetryRuntimeConfigDirectoryOutbox(ctx, claimed.ID, "worker-retry", at.Add(time.Second), "remote failure"); err != nil {
			t.Fatal(err)
		}
		at = at.Add(RuntimeConfigDirectoryOutboxBackoff(claimed.Attempt) + 2*time.Second)
	}
	final, err := repo.ReconcileRuntimeConfigDirectoryFanout(ctx, plan.ID, now.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != RuntimeConfigDirectoryFanoutFailed || final.CompletedCount != 2 || final.FailedCount != 1 || final.PendingCount != 0 || final.LastError != "fanout child delivery failed" {
		t.Fatalf("失败 child 收口后应为 metadata-only failed: %#v", final)
	}
	if strings.Contains(final.LastError, "remote failure") {
		t.Fatal("fanout parent 不应复制 child 错误正文")
	}
	corruptChild := repo.configDirectoryOutbox[plan.OutboxIDs[0]]
	corruptChild.FanoutID = ""
	if _, err := final.ReconcileSummary([]RuntimeConfigDirectoryOutbox{corruptChild}, now.Add(11*time.Minute)); err == nil {
		t.Fatal("child fanout 归属被篡改时必须 fail-closed")
	}
}

func TestMemoryRuntimeConfigDirectoryFanoutDoesNotReuseChildAcrossPlans(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	entry := configDirectoryProfile(t, "profile-fanout-owner", 1, "owner")
	repo := NewMemoryRepository()
	direct, err := NewRuntimeConfigDirectoryOutbox("runtime-source", "runtime-a", entry, "", "owner-correlation", "owner-key", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeConfigDirectoryOutbox(ctx, direct); err != nil {
		t.Fatal(err)
	}
	firstPlan, firstChildren, err := NewRuntimeConfigDirectoryFanoutPlan("runtime-source", []string{"runtime-a", "runtime-b"}, entry, "", "owner-correlation", "owner-key", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeConfigDirectoryFanout(ctx, firstPlan, firstChildren); err != nil {
		t.Fatalf("已有同 identity child 可以被同一 fanout 接管: %v", err)
	}
	owned, err := repo.GetRuntimeConfigDirectoryOutbox(ctx, firstChildren[0].ID)
	if err != nil || owned.FanoutID != firstPlan.ID {
		t.Fatalf("child 未记录 fanout owner: %#v err=%v", owned, err)
	}
	secondPlan, secondChildren, err := NewRuntimeConfigDirectoryFanoutPlan("runtime-source", []string{"runtime-a"}, entry, "", "owner-correlation", "owner-key", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeConfigDirectoryFanout(ctx, secondPlan, secondChildren); err == nil {
		t.Fatal("child 不得被两个不同 fanout parent 复用")
	}
}
