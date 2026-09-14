package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
)

func TestSQLiteRuntimeConfigDeliveryInboxTransactionAndOutboxAreDurable(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	repo := store.RuntimeRepository()
	old := sqliteMigrationSnapshot(t, "config-sqlite-old", "demo", "model")
	target := sqliteMigrationSnapshot(t, "config-sqlite-target", "demo", "model")
	next := sqliteMigrationSnapshot(t, "config-sqlite-next", "demo", "model")
	oldDigest := agent.RuntimeConfigSnapshotDigest(old)
	targetDigest := agent.RuntimeConfigSnapshotDigest(target)
	targetProjection, err := agent.ParseRuntimeConfigSnapshot(target)
	if err != nil {
		t.Fatal(err)
	}
	nextProjection, err := agent.ParseRuntimeConfigSnapshot(next)
	if err != nil {
		t.Fatal(err)
	}

	outbox, err := agentruntime.NewRuntimeConfigDeliveryOutbox("runtime-a", "runtime-b", "inv-config-outbox-sqlite", oldDigest, "outbox-1", targetProjection, now)
	if err != nil {
		t.Fatal(err)
	}
	outboxRepo := repo.(agentruntime.RuntimeConfigDeliveryOutboxRepository)
	saved, err := outboxRepo.EnqueueRuntimeConfigDeliveryOutbox(ctx, outbox)
	if err != nil || !saved.Matches(outbox) {
		t.Fatalf("SQLite config outbox enqueue 失败: %#v err=%v", saved, err)
	}
	if _, err := outboxRepo.EnqueueRuntimeConfigDeliveryOutbox(ctx, outbox); err != nil {
		t.Fatalf("相同 config outbox 重复 enqueue 应幂等: %v", err)
	}
	claimed, ok, err := outboxRepo.ClaimRuntimeConfigDeliveryOutbox(ctx, "config-worker-a", now, time.Minute)
	if err != nil || !ok || claimed.Attempt != 1 || claimed.Status != agentruntime.RuntimeConfigDeliveryOutboxProcessing {
		t.Fatalf("SQLite config outbox claim 错误: %#v ok=%v err=%v", claimed, ok, err)
	}
	if retried, err := outboxRepo.RetryRuntimeConfigDeliveryOutbox(ctx, claimed.ID, claimed.LeaseOwner, now, "authorization=secret-value"); err != nil || !retried {
		t.Fatalf("SQLite config outbox retry 失败: retried=%v err=%v", retried, err)
	}
	queued, err := outboxRepo.GetRuntimeConfigDeliveryOutbox(ctx, claimed.ID)
	if err != nil || queued.Status != agentruntime.RuntimeConfigDeliveryOutboxQueued || strings.Contains(queued.LastError, "secret-value") || !strings.Contains(queued.LastError, "[REDACTED]") {
		t.Fatalf("SQLite config outbox 脱敏/退避错误: %#v err=%v", queued, err)
	}

	makeSigned := func(invocationID, expectedDigest, key string, projection agent.RuntimeConfigSnapshot, at time.Time) agentruntime.RuntimeConfigDeliveryEnvelope {
		item, makeErr := agentruntime.NewRuntimeConfigDeliveryOutbox("runtime-a", "runtime-b", invocationID, expectedDigest, key, projection, at)
		if makeErr != nil {
			t.Fatal(makeErr)
		}
		envelope, makeErr := item.Envelope(at)
		if makeErr != nil {
			t.Fatal(makeErr)
		}
		envelope, makeErr = agentruntime.SignRuntimeConfigDeliveryEnvelope(envelope, []byte("config-delivery-sqlite-secret-32-bytes"))
		if makeErr != nil {
			t.Fatal(makeErr)
		}
		return envelope
	}
	envelope := makeSigned("inv-config-inbox-sqlite", oldDigest, "inbox-1", targetProjection, now)
	inboxRepo := repo.(agentruntime.RuntimeConfigDeliveryInbox)
	if duplicate, err := inboxRepo.AcceptRuntimeConfigDelivery(ctx, envelope, targetProjection); err != nil || duplicate {
		t.Fatalf("SQLite config inbox 首次接收失败 duplicate=%v err=%v", duplicate, err)
	}
	retry := envelope
	retry.IssuedAt = now.Add(time.Second)
	retry.Signature = ""
	retry, err = agentruntime.SignRuntimeConfigDeliveryEnvelope(retry, []byte("config-delivery-sqlite-secret-32-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := inboxRepo.AcceptRuntimeConfigDelivery(ctx, retry, targetProjection); err != nil || !duplicate {
		t.Fatalf("SQLite config inbox 刷新签名重试错误 duplicate=%v err=%v", duplicate, err)
	}
	nextEnvelope := makeSigned("inv-config-inbox-sqlite", targetDigest, "inbox-2", nextProjection, now.Add(2*time.Second))
	if duplicate, err := inboxRepo.AcceptRuntimeConfigDelivery(ctx, nextEnvelope, nextProjection); err != nil || duplicate {
		t.Fatalf("SQLite config inbox 单调更新失败 duplicate=%v err=%v", duplicate, err)
	}
	if _, err := inboxRepo.AcceptRuntimeConfigDelivery(ctx, envelope, targetProjection); !errors.Is(err, agentruntime.ErrRuntimeConfigDeliveryStale) {
		t.Fatalf("SQLite 旧 config inbox 应拒绝，实际=%v", err)
	}

	txnProjection := targetProjection
	txnEnvelope := makeSigned("inv-config-transaction-sqlite", oldDigest, "txn-1", txnProjection, now)
	txnRepo := repo.(agentruntime.RuntimeConfigDeliveryTransactionRepository)
	if duplicate, err := txnRepo.PrepareRuntimeConfigDelivery(ctx, txnEnvelope, txnProjection); err != nil || duplicate {
		t.Fatalf("SQLite config prepare 失败 duplicate=%v err=%v", duplicate, err)
	}
	if _, err := inboxRepo.GetRuntimeConfigDelivery(ctx, txnEnvelope.InvocationID); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("SQLite prepare 阶段不应暴露 inbox: %v", err)
	}
	if duplicate, err := txnRepo.CommitRuntimeConfigDelivery(ctx, txnEnvelope); err != nil || duplicate {
		t.Fatalf("SQLite config commit 失败 duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := txnRepo.CommitRuntimeConfigDelivery(ctx, txnEnvelope); err != nil || !duplicate {
		t.Fatalf("SQLite config 重复 commit 应幂等 duplicate=%v err=%v", duplicate, err)
	}

	invocation := agentruntime.Invocation{ID: "inv-config-delivery-atomic-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "conversation", ConfigSnapshot: old, Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	migrationRepo := repo.(agentruntime.RuntimeConfigMigrationDeliveryCommitRepository)
	commit := agentruntime.RuntimeConfigMigrationCommit{InvocationID: invocation.ID, ExpectedDigest: oldDigest, NewSnapshot: target, IdempotencyKey: "atomic-1", Event: agentruntime.AgentEvent{ID: agentruntime.RuntimeConfigMigrationEventID(invocation.ID, oldDigest, targetDigest, "atomic-1"), Type: agentruntime.EventRuntimeConfigMigrated, Timestamp: now}}
	committed, event, delivery, err := migrationRepo.CommitRuntimeConfigMigrationWithDelivery(ctx, commit, "runtime-a", "runtime-b")
	if err != nil || committed.ConfigSnapshotDigest != targetDigest || event.Type != agentruntime.EventRuntimeConfigMigrated || delivery.Status != agentruntime.RuntimeConfigDeliveryOutboxQueued {
		t.Fatalf("SQLite 配置迁移+delivery 原子提交失败: invocation=%#v event=%#v delivery=%#v err=%v", committed, event, delivery, err)
	}
	groupRepo := repo.(agentruntime.RuntimeDeliveryGroupRepository)
	groups, err := groupRepo.ListRuntimeDeliveryGroups(ctx, invocation.ID, "", 10)
	if err != nil || len(groups) != 1 || len(groups[0].Members) != 2 {
		t.Fatalf("SQLite 配置迁移未注册 delivery group: %#v err=%v", groups, err)
	}
	storedDelivery, err := repo.(agentruntime.RuntimeConfigDeliveryOutboxRepository).GetRuntimeConfigDeliveryOutbox(ctx, delivery.ID)
	if err != nil || storedDelivery.GroupID != groups[0].ID {
		t.Fatalf("SQLite config outbox 未保存 group ID: %#v group=%s err=%v", storedDelivery, groups[0].ID, err)
	}
	if _, _, _, err := migrationRepo.CommitRuntimeConfigMigrationWithDelivery(ctx, commit, "runtime-a", "runtime-b"); err != nil {
		t.Fatalf("SQLite 配置迁移+delivery 重试应幂等: %v", err)
	}
	groups, err = groupRepo.ListRuntimeDeliveryGroups(ctx, invocation.ID, "", 10)
	if err != nil || len(groups) != 1 {
		t.Fatalf("SQLite 配置迁移重复请求不应创建第二个 group: %#v err=%v", groups, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	reopened := store.RuntimeRepository()
	reopenedOutboxRepo := reopened.(agentruntime.RuntimeConfigDeliveryOutboxRepository)
	got, err := reopenedOutboxRepo.GetRuntimeConfigDeliveryOutbox(ctx, delivery.ID)
	if err != nil || got.Status != agentruntime.RuntimeConfigDeliveryOutboxQueued || !got.Matches(delivery) {
		t.Fatalf("SQLite 重启后 config outbox 错误: %#v err=%v", got, err)
	}
	invocationAfter, err := reopened.GetInvocation(ctx, invocation.ID)
	if err != nil || invocationAfter.ConfigSnapshotDigest != targetDigest {
		t.Fatalf("SQLite 重启后配置快照错误: %#v err=%v", invocationAfter, err)
	}
}

func TestSQLiteRuntimeConfigDeliveryAbortIsDurableAndNeverPublishes(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	old := sqliteMigrationSnapshot(t, "config-abort-old-sqlite", "demo", "model")
	target := sqliteMigrationSnapshot(t, "config-abort-target-sqlite", "demo", "model")
	projection, err := agent.ParseRuntimeConfigSnapshot(target)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	outbox, err := agentruntime.NewRuntimeConfigDeliveryOutbox("runtime-a", "runtime-b", "inv-config-abort-sqlite", agent.RuntimeConfigSnapshotDigest(old), "abort-1", projection, now)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	envelope, err := outbox.Envelope(now)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	envelope, err = agentruntime.SignRuntimeConfigDeliveryEnvelope(envelope, []byte("config-delivery-sqlite-secret-32-bytes"))
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	txnRepo := repo.(agentruntime.RuntimeConfigDeliveryTransactionRepository)
	aborter := repo.(agentruntime.RuntimeConfigDeliveryAbortableTransactionRepository)
	if duplicate, err := txnRepo.PrepareRuntimeConfigDelivery(ctx, envelope, projection); err != nil || duplicate {
		_ = store.Close()
		t.Fatalf("prepare 失败: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := aborter.AbortRuntimeConfigDelivery(ctx, envelope); err != nil || duplicate {
		_ = store.Close()
		t.Fatalf("首次 abort 失败: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := aborter.AbortRuntimeConfigDelivery(ctx, envelope); err != nil || !duplicate {
		_ = store.Close()
		t.Fatalf("重复 abort 必须幂等: duplicate=%v err=%v", duplicate, err)
	}
	transaction, err := repo.(interface {
		GetRuntimeConfigDeliveryTransaction(context.Context, string) (agentruntime.RuntimeConfigDeliveryTransaction, error)
	}).GetRuntimeConfigDeliveryTransaction(ctx, envelope.DeliveryID)
	if err != nil || transaction.Status != agentruntime.RuntimeConfigDeliveryTransactionAborted {
		_ = store.Close()
		t.Fatalf("abort 状态未持久化: %#v err=%v", transaction, err)
	}
	if _, err := repo.(agentruntime.RuntimeConfigDeliveryInbox).GetRuntimeConfigDelivery(ctx, envelope.InvocationID); !errors.Is(err, agentruntime.ErrNotFound) {
		_ = store.Close()
		t.Fatalf("abort 不得写入 config inbox: %v", err)
	}
	if _, err := txnRepo.CommitRuntimeConfigDelivery(ctx, envelope); !errors.Is(err, agentruntime.ErrConflict) {
		_ = store.Close()
		t.Fatalf("aborted transaction 不得 commit: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	restarted := store.RuntimeRepository().(agentruntime.RuntimeConfigDeliveryAbortableTransactionRepository)
	if duplicate, err := restarted.AbortRuntimeConfigDelivery(ctx, envelope); err != nil || !duplicate {
		t.Fatalf("重启后 aborted transaction 应幂等: duplicate=%v err=%v", duplicate, err)
	}
}
