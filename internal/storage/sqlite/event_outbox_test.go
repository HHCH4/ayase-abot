package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

func TestRuntimeRepositoryEventOutboxIsDurableAndAtLeastOnce(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-event-outbox-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	event, err := repo.AppendEvent(ctx, agentruntime.AgentEvent{ID: "event-outbox-sqlite", InvocationID: invocation.ID, Type: agentruntime.EventRuntimeNotice, Timestamp: now, Data: map[string]any{"code": "delivery", "api_key": "secret-value"}})
	if err != nil {
		t.Fatal(err)
	}
	outboxRepo, ok := repo.(agentruntime.RuntimeEventOutboxRepository)
	if !ok {
		t.Fatal("SQLite Runtime Repository 未实现 RuntimeEventOutboxRepository")
	}
	eventRepo, ok := repo.(agentruntime.AgentEventRepository)
	if !ok {
		t.Fatal("SQLite Runtime Repository 未实现 AgentEventRepository")
	}
	item, err := outboxRepo.GetRuntimeEventOutbox(ctx, sqliteEventOutboxID(t, event.ID))
	if err != nil || item.EventID != event.ID || item.Sequence != 1 || item.Status != agentruntime.RuntimeEventOutboxQueued {
		t.Fatalf("SQLite event outbox 初始状态错误: item=%#v err=%v", item, err)
	}
	fetched, err := eventRepo.GetAgentEvent(ctx, event.ID)
	if err != nil || fetched.Data["api_key"] != "[REDACTED]" {
		t.Fatalf("SQLite outbox 读取事件应脱敏: event=%#v err=%v", fetched, err)
	}
	if _, err := outboxRepo.EnqueueRuntimeEventOutbox(ctx, item); err != nil {
		t.Fatalf("重复登记同一事件应幂等: %v", err)
	}
	forged := item
	forged.ID = "forged"
	if _, err := outboxRepo.EnqueueRuntimeEventOutbox(ctx, forged); !errors.Is(err, agentruntime.ErrConflict) {
		t.Fatalf("新 outbox 不应接受非规范 ID，得到 %v", err)
	}

	claimed, ok, err := outboxRepo.ClaimRuntimeEventOutbox(ctx, "sqlite-worker-a", item.AvailableAt.Add(time.Nanosecond), time.Minute)
	if err != nil || !ok || claimed.Attempt != 1 || claimed.Status != agentruntime.RuntimeEventOutboxProcessing {
		t.Fatalf("SQLite claim 状态错误: item=%#v ok=%v err=%v", claimed, ok, err)
	}
	if done, err := outboxRepo.CompleteRuntimeEventOutbox(ctx, claimed.ID, "sqlite-worker-b", now); err != nil || done {
		t.Fatalf("错误 owner 不应完成 SQLite outbox: done=%v err=%v", done, err)
	}
	if retried, err := outboxRepo.RetryRuntimeEventOutbox(ctx, claimed.ID, claimed.LeaseOwner, claimed.UpdatedAt, "api_key=secret-value"); err != nil || !retried {
		t.Fatalf("SQLite outbox 应支持有界重试: retried=%v err=%v", retried, err)
	}
	queued, err := outboxRepo.GetRuntimeEventOutbox(ctx, claimed.ID)
	if err != nil || queued.Status != agentruntime.RuntimeEventOutboxQueued || strings.Contains(queued.LastError, "secret-value") || !strings.Contains(queued.LastError, "[REDACTED]") {
		t.Fatalf("SQLite 重试错误摘要未脱敏: item=%#v err=%v", queued, err)
	}
	var count int64
	if err := store.db.Model(&runtimeEventOutboxRow{}).Where("event_id = ?", event.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("同一事件应只有一条 outbox，得到 %d", count)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo = store.RuntimeRepository()
	outboxRepo = repo.(agentruntime.RuntimeEventOutboxRepository)
	queued, err = outboxRepo.GetRuntimeEventOutbox(ctx, claimed.ID)
	if err != nil || queued.Status != agentruntime.RuntimeEventOutboxQueued || queued.Attempt != 1 {
		t.Fatalf("重启后 SQLite outbox 未保留 queued/attempt: item=%#v err=%v", queued, err)
	}
	claimedAgain, ok, err := outboxRepo.ClaimRuntimeEventOutbox(ctx, "sqlite-worker-b", queued.AvailableAt.Add(time.Nanosecond), time.Minute)
	if err != nil || !ok || claimedAgain.Attempt != 2 {
		t.Fatalf("退避到期后 SQLite outbox 未能接管: item=%#v ok=%v err=%v", claimedAgain, ok, err)
	}
	if done, err := outboxRepo.CompleteRuntimeEventOutbox(ctx, claimedAgain.ID, "sqlite-worker-b", queued.AvailableAt.Add(time.Second)); err != nil || !done {
		t.Fatalf("SQLite 第二次交付未完成: done=%v err=%v", done, err)
	}
	completed, err := outboxRepo.GetRuntimeEventOutbox(ctx, claimedAgain.ID)
	if err != nil || completed.Status != agentruntime.RuntimeEventOutboxCompleted || completed.LeaseOwner != "" {
		t.Fatalf("SQLite completed 状态错误: item=%#v err=%v", completed, err)
	}
	if _, ok, err := outboxRepo.ClaimRuntimeEventOutbox(ctx, "sqlite-worker-c", queued.AvailableAt.Add(2*time.Hour), time.Minute); err != nil || ok {
		t.Fatalf("SQLite completed outbox 不应再次 claim: ok=%v err=%v", ok, err)
	}
}

func TestRuntimeRepositoryEventOutboxLeaseExpiryRejectsLateOwner(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	repo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-event-outbox-expiry-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	event, err := repo.AppendEvent(ctx, agentruntime.AgentEvent{ID: "event-outbox-expiry-sqlite", InvocationID: invocation.ID, Type: agentruntime.EventRuntimeNotice, Timestamp: now})
	if err != nil {
		t.Fatal(err)
	}
	outboxRepo := repo.(agentruntime.RuntimeEventOutboxRepository)
	claimed, ok, err := outboxRepo.ClaimRuntimeEventOutbox(ctx, "late-owner", now, time.Minute)
	if err != nil || !ok || claimed.EventID != event.ID || claimed.LeaseExpiresAt == nil {
		t.Fatalf("首次 SQLite claim 失败: item=%#v ok=%v err=%v", claimed, ok, err)
	}
	expired := claimed.LeaseExpiresAt.UTC()
	if done, err := outboxRepo.CompleteRuntimeEventOutbox(ctx, claimed.ID, claimed.LeaseOwner, expired); err != nil || done {
		t.Fatalf("SQLite 租约到期后旧 owner 不应完成: done=%v err=%v", done, err)
	}
	if retried, err := outboxRepo.RetryRuntimeEventOutbox(ctx, claimed.ID, claimed.LeaseOwner, expired, "late"); err != nil || retried {
		t.Fatalf("SQLite 租约到期后旧 owner 不应重试: retried=%v err=%v", retried, err)
	}
	reclaimed, ok, err := outboxRepo.ClaimRuntimeEventOutbox(ctx, "new-owner", expired, time.Minute)
	if err != nil || !ok || reclaimed.LeaseOwner != "new-owner" || reclaimed.Attempt != 2 {
		t.Fatalf("SQLite 过期租约应可被新 owner 接管: item=%#v ok=%v err=%v", reclaimed, ok, err)
	}
	if done, err := outboxRepo.CompleteRuntimeEventOutbox(ctx, reclaimed.ID, "late-owner", expired.Add(time.Second)); err != nil || done {
		t.Fatalf("SQLite 旧 owner 不应越过 revision 完成新租约: done=%v err=%v", done, err)
	}
	if done, err := outboxRepo.CompleteRuntimeEventOutbox(ctx, reclaimed.ID, "new-owner", expired.Add(time.Second)); err != nil || !done {
		t.Fatalf("SQLite 新 owner 应能在租约内完成: done=%v err=%v", done, err)
	}
	if _, ok, err := outboxRepo.ClaimRuntimeEventOutbox(ctx, strings.Repeat("x", agentruntime.MaxRuntimeEventOutboxOwnerLength+1), expired, time.Minute); !errors.Is(err, agentruntime.ErrConflict) || ok {
		t.Fatalf("SQLite 过长 owner 应被拒绝: ok=%v err=%v", ok, err)
	}
	if _, ok, err := outboxRepo.ClaimRuntimeEventOutbox(ctx, "bounded", expired, agentruntime.MaxRuntimeEventOutboxLease+time.Nanosecond); !errors.Is(err, agentruntime.ErrConflict) || ok {
		t.Fatalf("SQLite 过长 lease 应被拒绝: ok=%v err=%v", ok, err)
	}
}

func TestRuntimeRepositoryEventAppendRollbackDoesNotLeaveOutbox(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	now := time.Now().UTC()
	repo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-event-outbox-rollback", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	event := agentruntime.AgentEvent{ID: "event-event-outbox-rollback", InvocationID: invocation.ID, Type: agentruntime.EventRuntimeNotice, Timestamp: now}
	if _, err := repo.AppendEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, event); err == nil {
		t.Fatal("重复事件追加应失败")
	}
	outboxRepo := repo.(agentruntime.RuntimeEventOutboxRepository)
	items, err := outboxRepo.ListRuntimeEventOutbox(ctx, invocation.ID, "", 20)
	if err != nil || len(items) != 1 || items[0].EventID != event.ID {
		t.Fatalf("重复事件失败后不得留下第二条 outbox: items=%#v err=%v", items, err)
	}
	if _, err := outboxRepo.GetRuntimeEventOutbox(ctx, "missing-event-outbox"); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("不存在的 outbox 应返回 ErrNotFound: %v", err)
	}
}

func TestRuntimeRepositoryEventAppendRejectsOrphanInvocation(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	_, err = store.RuntimeRepository().AppendEvent(context.Background(), agentruntime.AgentEvent{
		ID: "event-orphan", InvocationID: "missing-invocation", Type: agentruntime.EventRuntimeNotice,
	})
	if !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("不存在 Invocation 的事件应被拒绝为 ErrNotFound: %v", err)
	}
	var count int64
	if err := store.db.Model(&runtimeEventOutboxRow{}).Where("event_id = ?", "event-orphan").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("孤儿事件失败后不应留下 outbox: %d", count)
	}
}

func sqliteEventOutboxID(t *testing.T, eventID string) string {
	t.Helper()
	item, err := agentruntime.NewRuntimeEventOutbox(agentruntime.AgentEvent{ID: eventID, InvocationID: "invocation", Sequence: 1, Type: agentruntime.EventRuntimeNotice, Timestamp: time.Unix(0, 0).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	return item.ID
}
