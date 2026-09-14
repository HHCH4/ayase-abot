package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

func TestSQLiteRuntimeDeliveryAttemptIsDurableAndCASProtected(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	now := time.Date(2026, 9, 13, 23, 40, 0, 0, time.UTC)
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-sqlite-attempt", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	attemptRepo, ok := repo.(agentruntime.RuntimeDeliveryAttemptRepository)
	if !ok {
		_ = store.Close()
		t.Fatal("SQLite Runtime Repository 未实现 RuntimeDeliveryAttemptRepository")
	}
	candidate, err := agentruntime.NewRuntimeDeliveryAttempt(agentruntime.RuntimeDeliveryKindRejection, "runtime-a", "runtime-b", "delivery-sqlite", invocation.ID, "outbox-sqlite", 4, 1, "worker-a", now, time.Minute)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	started, err := attemptRepo.BeginRuntimeDeliveryAttempt(ctx, candidate, now, time.Minute)
	if err != nil || started.Phase != agentruntime.RuntimeDeliveryAttemptStarted {
		_ = store.Close()
		t.Fatalf("SQLite attempt begin 失败: %#v err=%v", started, err)
	}
	prepared, duplicate, err := attemptRepo.AdvanceRuntimeDeliveryAttempt(ctx, started.ID, "worker-a", started.Revision, agentruntime.RuntimeDeliveryAttemptPrepared, now.Add(time.Second), "")
	if err != nil || duplicate || prepared.Phase != agentruntime.RuntimeDeliveryAttemptPrepared {
		_ = store.Close()
		t.Fatalf("SQLite attempt prepared 失败: %#v duplicate=%v err=%v", prepared, duplicate, err)
	}
	if _, _, err := attemptRepo.AdvanceRuntimeDeliveryAttempt(ctx, prepared.ID, "wrong-worker", prepared.Revision, agentruntime.RuntimeDeliveryAttemptCommitted, now.Add(2*time.Second), ""); !errors.Is(err, agentruntime.ErrConflict) {
		_ = store.Close()
		t.Fatalf("SQLite 错误 owner 不应推进 attempt: %v", err)
	}
	committed, duplicate, err := attemptRepo.AdvanceRuntimeDeliveryAttempt(ctx, prepared.ID, "worker-a", prepared.Revision, agentruntime.RuntimeDeliveryAttemptCommitted, now.Add(2*time.Second), "")
	if err != nil || duplicate || committed.Phase != agentruntime.RuntimeDeliveryAttemptCommitted {
		_ = store.Close()
		t.Fatalf("SQLite committed 失败: %#v duplicate=%v err=%v", committed, duplicate, err)
	}
	if _, duplicate, err := attemptRepo.AdvanceRuntimeDeliveryAttempt(ctx, committed.ID, "worker-a", committed.Revision, agentruntime.RuntimeDeliveryAttemptCommitted, now.Add(3*time.Second), ""); err != nil || !duplicate {
		_ = store.Close()
		t.Fatalf("SQLite 同 phase 重放应幂等: duplicate=%v err=%v", duplicate, err)
	}
	if _, _, err := attemptRepo.AdvanceRuntimeDeliveryAttempt(ctx, committed.ID, "worker-a", committed.Revision-1, agentruntime.RuntimeDeliveryAttemptCompleted, now.Add(3*time.Second), ""); !errors.Is(err, agentruntime.ErrConflict) {
		_ = store.Close()
		t.Fatalf("SQLite 旧 revision 不应覆盖: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	attemptRepo = store.RuntimeRepository().(agentruntime.RuntimeDeliveryAttemptRepository)
	stored, err := attemptRepo.GetRuntimeDeliveryAttempt(ctx, candidate.ID)
	if err != nil || stored.Phase != agentruntime.RuntimeDeliveryAttemptCommitted || stored.LastError != "" {
		t.Fatalf("SQLite 重启后 attempt 不可读: %#v err=%v", stored, err)
	}
	items, err := attemptRepo.ListRuntimeDeliveryAttempts(ctx, invocation.ID, agentruntime.RuntimeDeliveryKindRejection, 10)
	if err != nil || len(items) != 1 || items[0].ID != candidate.ID {
		t.Fatalf("SQLite attempt 列表错误: %#v err=%v", items, err)
	}
	if _, err := attemptRepo.GetRuntimeDeliveryAttempt(ctx, "missing-attempt"); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("缺失 attempt 应返回 ErrNotFound: %v", err)
	}
	if strings.Contains(stored.LastError, "secret") {
		t.Fatal("attempt metadata 不应保留 secret")
	}
}
