package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func eventOutboxInvocation(t *testing.T, repo *MemoryRepository, id string) Invocation {
	t.Helper()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	item := Invocation{ID: id, UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	return item
}

func TestMemoryRuntimeEventOutboxClaimRetryAndCompletion(t *testing.T) {
	repo := NewMemoryRepository()
	invocation := eventOutboxInvocation(t, repo, "inv-event-outbox-memory")
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	event, err := repo.AppendEvent(context.Background(), AgentEvent{
		ID: "event-outbox-memory", InvocationID: invocation.ID, Type: EventRuntimeNotice, Timestamp: now,
		Data: map[string]any{"code": "delivery", "api_key": "secret-value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	outboxRepo := RuntimeEventOutboxRepository(repo)
	item, err := outboxRepo.GetRuntimeEventOutbox(context.Background(), eventOutboxIDForTest(event.ID))
	if err != nil {
		t.Fatal(err)
	}
	if item.EventID != event.ID || item.InvocationID != invocation.ID || item.Sequence != 1 || item.Status != RuntimeEventOutboxQueued {
		t.Fatalf("事件 outbox 元数据错误: %#v", item)
	}
	fetched, err := repo.GetAgentEvent(context.Background(), event.ID)
	if err != nil || fetched.Data["api_key"] != "[REDACTED]" {
		t.Fatalf("事件正文应在读取交付时经过脱敏: event=%#v err=%v", fetched, err)
	}
	if _, err := outboxRepo.EnqueueRuntimeEventOutbox(context.Background(), item); err != nil {
		t.Fatalf("重复登记同一 EventID 应幂等: %v", err)
	}

	var wg sync.WaitGroup
	claims := make(chan RuntimeEventOutbox, 8)
	for index := 0; index < 8; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			claimed, ok, claimErr := outboxRepo.ClaimRuntimeEventOutbox(context.Background(), "worker-"+string(rune('a'+index)), now, time.Minute)
			if claimErr != nil {
				t.Errorf("并发 claim 失败: %v", claimErr)
				return
			}
			if ok {
				claims <- claimed
			}
		}(index)
	}
	wg.Wait()
	close(claims)
	var claimed RuntimeEventOutbox
	claimCount := 0
	for item := range claims {
		claimed = item
		claimCount++
	}
	if claimCount != 1 {
		t.Fatalf("同一 event outbox 只能有一个并发 claim，得到 %d", claimCount)
	}
	if claimed.Attempt != 1 || claimed.Status != RuntimeEventOutboxProcessing || claimed.LeaseOwner == "" {
		t.Fatalf("claim 后状态错误: %#v", claimed)
	}
	if completed, err := outboxRepo.CompleteRuntimeEventOutbox(context.Background(), claimed.ID, "other-worker", now); err != nil || completed {
		t.Fatalf("非租约 owner 不应完成 outbox: completed=%v err=%v", completed, err)
	}
	if retried, err := outboxRepo.RetryRuntimeEventOutbox(context.Background(), claimed.ID, claimed.LeaseOwner, now, "api_key=secret-value"); err != nil || !retried {
		t.Fatalf("租约 owner 应能安排重试: retried=%v err=%v", retried, err)
	}
	queued, err := outboxRepo.GetRuntimeEventOutbox(context.Background(), claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if queued.Status != RuntimeEventOutboxQueued || !queued.AvailableAt.After(now) || !strings.Contains(queued.LastError, "api_key=[REDACTED]") {
		t.Fatalf("重试状态或错误摘要不安全: %#v", queued)
	}
	if _, err := outboxRepo.EnqueueRuntimeEventOutbox(context.Background(), RuntimeEventOutbox{ID: "forged", InvocationID: invocation.ID, EventID: event.ID, Sequence: event.Sequence, Type: event.Type}); !errors.Is(err, ErrConflict) {
		t.Fatalf("新 outbox 不应接受非规范 ID，得到 %v", err)
	}
	if _, ok, err := outboxRepo.ClaimRuntimeEventOutbox(context.Background(), "worker-retry-too-early", now, time.Minute); err != nil || ok {
		t.Fatalf("退避期间不应再次 claim: ok=%v err=%v", ok, err)
	}
	claimedAgain, ok, err := outboxRepo.ClaimRuntimeEventOutbox(context.Background(), "worker-retry", queued.AvailableAt.Add(time.Nanosecond), time.Minute)
	if err != nil || !ok || claimedAgain.Attempt != 2 {
		t.Fatalf("退避到期后应接管并递增 attempt: item=%#v ok=%v err=%v", claimedAgain, ok, err)
	}
	if completed, err := outboxRepo.CompleteRuntimeEventOutbox(context.Background(), claimedAgain.ID, "worker-retry", queued.AvailableAt.Add(time.Second)); err != nil || !completed {
		t.Fatalf("第二次交付应能完成: completed=%v err=%v", completed, err)
	}
	completedItem, err := outboxRepo.GetRuntimeEventOutbox(context.Background(), claimedAgain.ID)
	if err != nil || completedItem.Status != RuntimeEventOutboxCompleted || completedItem.LeaseOwner != "" {
		t.Fatalf("完成后的 outbox 状态错误: item=%#v err=%v", completedItem, err)
	}
	if _, ok, err := outboxRepo.ClaimRuntimeEventOutbox(context.Background(), "worker-after-complete", now.Add(2*time.Hour), time.Minute); err != nil || ok {
		t.Fatalf("completed outbox 不应重投: ok=%v err=%v", ok, err)
	}
}

func TestMemoryRuntimeEventOutboxLeaseExpiryRejectsLateOwner(t *testing.T) {
	repo := NewMemoryRepository()
	invocation := eventOutboxInvocation(t, repo, "inv-event-outbox-expiry")
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	event, err := repo.AppendEvent(context.Background(), AgentEvent{ID: "event-outbox-expiry", InvocationID: invocation.ID, Type: EventRuntimeNotice, Timestamp: now})
	if err != nil {
		t.Fatal(err)
	}
	outboxRepo := RuntimeEventOutboxRepository(repo)
	claimed, ok, err := outboxRepo.ClaimRuntimeEventOutbox(context.Background(), "late-owner", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("首次 claim 失败: item=%#v ok=%v err=%v", claimed, ok, err)
	}
	if claimed.EventID != event.ID || claimed.LeaseExpiresAt == nil {
		t.Fatalf("claim 元数据错误: %#v", claimed)
	}
	expired := claimed.LeaseExpiresAt.UTC()
	if done, err := outboxRepo.CompleteRuntimeEventOutbox(context.Background(), claimed.ID, claimed.LeaseOwner, expired); err != nil || done {
		t.Fatalf("租约到期后旧 owner 不应完成: done=%v err=%v", done, err)
	}
	if retried, err := outboxRepo.RetryRuntimeEventOutbox(context.Background(), claimed.ID, claimed.LeaseOwner, expired, "late"); err != nil || retried {
		t.Fatalf("租约到期后旧 owner 不应重试: retried=%v err=%v", retried, err)
	}
	reclaimed, ok, err := outboxRepo.ClaimRuntimeEventOutbox(context.Background(), "new-owner", expired, time.Minute)
	if err != nil || !ok || reclaimed.LeaseOwner != "new-owner" || reclaimed.Attempt != 2 {
		t.Fatalf("过期租约应可被新 owner 接管: item=%#v ok=%v err=%v", reclaimed, ok, err)
	}
	if done, err := outboxRepo.CompleteRuntimeEventOutbox(context.Background(), reclaimed.ID, "late-owner", expired.Add(time.Second)); err != nil || done {
		t.Fatalf("旧 owner 不应越过 revision 完成新租约: done=%v err=%v", done, err)
	}
	if done, err := outboxRepo.CompleteRuntimeEventOutbox(context.Background(), reclaimed.ID, "new-owner", expired.Add(time.Second)); err != nil || !done {
		t.Fatalf("新 owner 应能在租约内完成: done=%v err=%v", done, err)
	}
	if _, ok, err := outboxRepo.ClaimRuntimeEventOutbox(context.Background(), strings.Repeat("x", MaxRuntimeEventOutboxOwnerLength+1), expired, time.Minute); !errors.Is(err, ErrConflict) || ok {
		t.Fatalf("过长 owner 应被拒绝: ok=%v err=%v", ok, err)
	}
	if _, ok, err := outboxRepo.ClaimRuntimeEventOutbox(context.Background(), "bounded", expired, MaxRuntimeEventOutboxLease+time.Nanosecond); !errors.Is(err, ErrConflict) || ok {
		t.Fatalf("过长 lease 应被拒绝: ok=%v err=%v", ok, err)
	}
}

func TestRuntimeEventOutboxNormalizeRejectsMalformedMetadata(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	base := RuntimeEventOutbox{ID: "outbox", InvocationID: "invocation", EventID: "event", Sequence: 1, Type: EventRuntimeNotice}
	tests := []struct {
		name   string
		mutate func(*RuntimeEventOutbox)
	}{
		{name: "missing id", mutate: func(item *RuntimeEventOutbox) { item.ID = "" }},
		{name: "missing invocation", mutate: func(item *RuntimeEventOutbox) { item.InvocationID = "" }},
		{name: "missing event", mutate: func(item *RuntimeEventOutbox) { item.EventID = "" }},
		{name: "missing type", mutate: func(item *RuntimeEventOutbox) { item.Type = "" }},
		{name: "invalid sequence", mutate: func(item *RuntimeEventOutbox) { item.Sequence = 0 }},
		{name: "invalid status", mutate: func(item *RuntimeEventOutbox) { item.Status = RuntimeEventOutboxStatus("unknown") }},
		{name: "attempt above limit", mutate: func(item *RuntimeEventOutbox) { item.Attempt = MaxRuntimeEventOutboxAttempts + 1 }},
		{name: "id too long", mutate: func(item *RuntimeEventOutbox) { item.ID = strings.Repeat("x", 129) }},
		{name: "event id too long", mutate: func(item *RuntimeEventOutbox) { item.EventID = strings.Repeat("x", 513) }},
		{name: "type too long", mutate: func(item *RuntimeEventOutbox) { item.Type = strings.Repeat("x", 81) }},
		{name: "owner too long", mutate: func(item *RuntimeEventOutbox) {
			item.LeaseOwner = strings.Repeat("x", MaxRuntimeEventOutboxOwnerLength+1)
		}},
		{name: "processing without owner", mutate: func(item *RuntimeEventOutbox) { item.Status = RuntimeEventOutboxProcessing }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := base
			test.mutate(&item)
			if _, err := item.Normalize(now); !errors.Is(err, ErrConflict) {
				t.Fatalf("malformed metadata should fail with ErrConflict, got %v", err)
			}
		})
	}

	leaseExpiry := now.Add(time.Minute)
	terminal, err := (RuntimeEventOutbox{
		ID: "outbox", InvocationID: "invocation", EventID: "event", Sequence: 1, Type: EventRuntimeNotice,
		Status: RuntimeEventOutboxCompleted, Attempt: 1, LeaseOwner: "worker", LeaseExpiresAt: &leaseExpiry,
	}).Normalize(now)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.LeaseOwner != "" || terminal.LeaseExpiresAt != nil {
		t.Fatalf("终态 outbox 不应保留租约: %#v", terminal)
	}
}

func TestCoordinatorDispatchesRuntimeEventOutboxWithAtLeastOnceBoundary(t *testing.T) {
	repo := NewMemoryRepository()
	invocation := eventOutboxInvocation(t, repo, "inv-event-outbox-dispatch")
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	if _, err := repo.AppendEvent(context.Background(), AgentEvent{ID: "event-outbox-dispatch", InvocationID: invocation.ID, Type: EventAssistantMessage, Timestamp: now, Data: map[string]any{"text": "hello"}}); err != nil {
		t.Fatal(err)
	}
	delivered := make(chan AgentEvent, 1)
	coordinator := &Coordinator{repo: repo, eventOutboxWorkerID: "dispatch-worker", eventOutboxHandler: func(_ context.Context, item RuntimeEventOutbox, event AgentEvent) error {
		if item.EventID != event.ID || event.ID != "event-outbox-dispatch" {
			return errors.New("event identity mismatch")
		}
		delivered <- event
		return nil
	}}
	coordinator.dispatchDueRuntimeEventOutbox(context.Background(), now.Add(time.Second))
	select {
	case event := <-delivered:
		if event.Data["text"] != "hello" {
			t.Fatalf("handler 收到的 event 正文错误: %#v", event)
		}
	default:
		t.Fatal("成功的 outbox 没有交付到 handler")
	}
	items, err := repo.ListRuntimeEventOutbox(context.Background(), invocation.ID, RuntimeEventOutboxCompleted, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("成功交付后应可查询 completed outbox: items=%#v err=%v", items, err)
	}

	failureEvent, err := repo.AppendEvent(context.Background(), AgentEvent{ID: "event-outbox-dispatch-failure", InvocationID: invocation.ID, Type: EventRuntimeNotice, Timestamp: now.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	coordinator.eventOutboxHandler = func(context.Context, RuntimeEventOutbox, AgentEvent) error {
		return errors.New("authorization=Bearer secret-value")
	}
	coordinator.dispatchDueRuntimeEventOutbox(context.Background(), now.Add(time.Second))
	failureOutbox, err := repo.GetRuntimeEventOutbox(context.Background(), eventOutboxIDForTest(failureEvent.ID))
	if err != nil {
		t.Fatal(err)
	}
	if failureOutbox.Status != RuntimeEventOutboxQueued || strings.Contains(failureOutbox.LastError, "secret-value") || !strings.Contains(failureOutbox.LastError, "[REDACTED]") {
		t.Fatalf("handler 失败应进入退避且脱敏: %#v", failureOutbox)
	}
}

func eventOutboxIDForTest(eventID string) string {
	item, err := NewRuntimeEventOutbox(AgentEvent{ID: eventID, InvocationID: "invocation", Sequence: 1, Type: EventRuntimeNotice, Timestamp: time.Unix(0, 0).UTC()})
	if err != nil {
		panic(err)
	}
	return item.ID
}
