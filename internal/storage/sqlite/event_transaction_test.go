package sqlite

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

func TestSQLiteRuntimeEventDeliveryPrepareCommitIsDurableAndAtomic(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Millisecond)
	event := agentruntime.AgentEvent{ID: "sqlite-event-transaction", InvocationID: "sqlite-event-transaction-invocation", Sequence: 1, Type: agentruntime.EventRuntimeNotice, Timestamp: now, Data: map[string]any{"private": "body"}}
	outbox, err := agentruntime.NewRuntimeEventOutbox(event)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := agentruntime.NewRuntimeEventDeliveryEnvelope("runtime-a", "runtime-b", outbox, event)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err = agentruntime.SignRuntimeEventDeliveryEnvelope(envelope, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	txnRepo, ok := store.RuntimeRepository().(agentruntime.RuntimeEventDeliveryTransactionRepository)
	if !ok {
		_ = store.Close()
		t.Fatal("SQLite runtime repository 未实现 event transaction repository")
	}
	duplicate, err := txnRepo.PrepareRuntimeEventDelivery(ctx, envelope)
	if err != nil || duplicate {
		t.Fatalf("首次 prepare 错误: duplicate=%v err=%v", duplicate, err)
	}
	var preparedRow runtimeEventDeliveryTransactionRow
	if err := store.db.Where("delivery_id = ?", envelope.DeliveryID).First(&preparedRow).Error; err != nil {
		t.Fatal(err)
	}
	if preparedRow.Status != string(agentruntime.RuntimeEventDeliveryTransactionPrepared) || preparedRow.EventID != envelope.EventID || preparedRow.Signature != envelope.Signature {
		t.Fatalf("prepare ledger 持久化错误: %#v", preparedRow)
	}
	transactionReader := store.RuntimeRepository().(interface {
		GetRuntimeEventDeliveryTransaction(context.Context, string) (agentruntime.RuntimeEventDeliveryTransaction, error)
	})
	if _, err := transactionReader.GetRuntimeEventDeliveryTransaction(ctx, envelope.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	txnRepo = store.RuntimeRepository().(agentruntime.RuntimeEventDeliveryTransactionRepository)
	duplicate, err = txnRepo.PrepareRuntimeEventDelivery(ctx, envelope)
	if err != nil || !duplicate {
		t.Fatalf("重启后 prepare 应幂等: duplicate=%v err=%v", duplicate, err)
	}
	duplicate, err = txnRepo.CommitRuntimeEventDelivery(ctx, envelope)
	if err != nil || duplicate {
		t.Fatalf("首次 commit 错误: duplicate=%v err=%v", duplicate, err)
	}
	duplicate, err = txnRepo.CommitRuntimeEventDelivery(ctx, envelope)
	if err != nil || !duplicate {
		t.Fatalf("重复 commit 应幂等: duplicate=%v err=%v", duplicate, err)
	}
	transaction, err := txnRepo.(interface {
		GetRuntimeEventDeliveryTransaction(context.Context, string) (agentruntime.RuntimeEventDeliveryTransaction, error)
	}).GetRuntimeEventDeliveryTransaction(ctx, envelope.DeliveryID)
	if err != nil || transaction.Status != agentruntime.RuntimeEventDeliveryTransactionCommitted {
		t.Fatalf("commit 状态错误: %#v err=%v", transaction, err)
	}
	if _, err := store.RuntimeRepository().(agentruntime.RuntimeEventDeliveryInbox).AcceptRuntimeEventDelivery(ctx, envelope); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if _, err := store.RuntimeRepository().(agentruntime.RuntimeEventDeliveryTransactionRepository).CommitRuntimeEventDelivery(ctx, envelope); err != nil {
		t.Fatalf("重启后 committed transaction 应仍可幂等: %v", err)
	}
}

func TestSQLiteRuntimeEventDeliveryAbortIsDurableAndNeverPublishes(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Millisecond)
	event := agentruntime.AgentEvent{ID: "sqlite-event-transaction-abort", InvocationID: "sqlite-event-transaction-abort-invocation", Sequence: 1, Type: agentruntime.EventRuntimeNotice, Timestamp: now}
	outbox, err := agentruntime.NewRuntimeEventOutbox(event)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := agentruntime.NewRuntimeEventDeliveryEnvelope("runtime-a", "runtime-b", outbox, event)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err = agentruntime.SignRuntimeEventDeliveryEnvelope(envelope, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	txnRepo := store.RuntimeRepository().(agentruntime.RuntimeEventDeliveryTransactionRepository)
	aborter := store.RuntimeRepository().(agentruntime.RuntimeEventDeliveryAbortableTransactionRepository)
	if duplicate, err := txnRepo.PrepareRuntimeEventDelivery(ctx, envelope); err != nil || duplicate {
		t.Fatalf("prepare 失败: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := aborter.AbortRuntimeEventDelivery(ctx, envelope); err != nil || duplicate {
		t.Fatalf("首次 abort 失败: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := aborter.AbortRuntimeEventDelivery(ctx, envelope); err != nil || !duplicate {
		t.Fatalf("重复 abort 必须幂等: duplicate=%v err=%v", duplicate, err)
	}
	transaction, err := txnRepo.(interface {
		GetRuntimeEventDeliveryTransaction(context.Context, string) (agentruntime.RuntimeEventDeliveryTransaction, error)
	}).GetRuntimeEventDeliveryTransaction(ctx, envelope.DeliveryID)
	if err != nil || transaction.Status != agentruntime.RuntimeEventDeliveryTransactionAborted {
		t.Fatalf("abort 状态未持久化: %#v err=%v", transaction, err)
	}
	if _, err := store.RuntimeRepository().(agentruntime.RuntimeEventDeliveryInboxReader).GetRuntimeEventDelivery(ctx, event.ID); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("abort 不得写入 event inbox: %v", err)
	}
	if _, err := txnRepo.CommitRuntimeEventDelivery(ctx, envelope); !errors.Is(err, agentruntime.ErrConflict) {
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
	restarted := store.RuntimeRepository().(agentruntime.RuntimeEventDeliveryAbortableTransactionRepository)
	if duplicate, err := restarted.AbortRuntimeEventDelivery(ctx, envelope); err != nil || !duplicate {
		t.Fatalf("重启后 aborted transaction 应幂等: duplicate=%v err=%v", duplicate, err)
	}
}

func TestSQLiteRuntimeEventDeliveryPrepareIsConcurrentIdempotent(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	now := time.Now().UTC().Truncate(time.Millisecond)
	event := agentruntime.AgentEvent{ID: "sqlite-event-transaction-concurrent", InvocationID: "sqlite-event-transaction-concurrent-invocation", Sequence: 1, Type: agentruntime.EventRuntimeNotice, Timestamp: now}
	outbox, err := agentruntime.NewRuntimeEventOutbox(event)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := agentruntime.NewRuntimeEventDeliveryEnvelope("runtime-a", "runtime-b", outbox, event)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("0123456789abcdef0123456789abcdef")
	envelope, err = agentruntime.SignRuntimeEventDeliveryEnvelope(envelope, secret)
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository().(agentruntime.RuntimeEventDeliveryTransactionRepository)
	start := make(chan struct{})
	results := make(chan struct {
		duplicate bool
		err       error
	}, 12)
	var group sync.WaitGroup
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			duplicate, prepareErr := repo.PrepareRuntimeEventDelivery(ctx, envelope)
			results <- struct {
				duplicate bool
				err       error
			}{duplicate: duplicate, err: prepareErr}
		}()
	}
	close(start)
	group.Wait()
	close(results)
	firstCount := 0
	for result := range results {
		if result.err != nil && !errors.Is(result.err, agentruntime.ErrConflict) {
			t.Fatalf("相同 envelope 并发 prepare 不应失败: %v", result.err)
		}
		if result.err == nil && !result.duplicate {
			firstCount++
		}
	}
	if firstCount != 1 {
		t.Fatalf("并发 prepare 只能有一个首次写入，得到 %d", firstCount)
	}
}

func TestSQLiteRuntimeEventDeliveryIssuedAtRefreshesExpiredPrepare(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	now := time.Now().UTC().Truncate(time.Millisecond)
	event := agentruntime.AgentEvent{ID: "sqlite-event-transaction-issued-at", InvocationID: "sqlite-event-transaction-issued-at-invocation", Sequence: 1, Type: agentruntime.EventRuntimeNotice, Timestamp: now.Add(-2 * time.Hour)}
	outbox, err := agentruntime.NewRuntimeEventOutbox(event)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := agentruntime.NewRuntimeEventDeliveryEnvelope("runtime-a", "runtime-b", outbox, event)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("0123456789abcdef0123456789abcdef")
	legacy, err = agentruntime.SignRuntimeEventDeliveryEnvelope(legacy, secret)
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository().(agentruntime.RuntimeEventDeliveryTransactionRepository)
	if duplicate, err := repo.PrepareRuntimeEventDelivery(ctx, legacy); err != nil || duplicate {
		t.Fatalf("首次 prepare 错误: duplicate=%v err=%v", duplicate, err)
	}
	var row runtimeEventDeliveryTransactionRow
	if err := store.db.Where("delivery_id = ?", legacy.DeliveryID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	row.Status = string(agentruntime.RuntimeEventDeliveryTransactionPrepared)
	row.ExpiresAt = now.Add(-time.Second)
	if err := store.db.Save(&row).Error; err != nil {
		t.Fatal(err)
	}
	fresh, err := agentruntime.RefreshRuntimeEventDeliveryEnvelope(legacy, now)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err = agentruntime.SignRuntimeEventDeliveryEnvelope(fresh, secret)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := repo.PrepareRuntimeEventDelivery(ctx, fresh); err != nil || duplicate {
		t.Fatalf("过期 prepare 应允许 v2 issued_at 刷新: duplicate=%v err=%v", duplicate, err)
	}
	transaction, err := repo.(interface {
		GetRuntimeEventDeliveryTransaction(context.Context, string) (agentruntime.RuntimeEventDeliveryTransaction, error)
	}).GetRuntimeEventDeliveryTransaction(ctx, legacy.DeliveryID)
	if err != nil || transaction.Version != agentruntime.RuntimeEventDeliveryEnvelopeVersionIssuedAt || !transaction.Timestamp.Equal(event.Timestamp) || !transaction.IssuedAt.Equal(now) {
		t.Fatalf("SQLite 刷新 transaction metadata 错误: %#v err=%v", transaction, err)
	}
	if duplicate, err := repo.CommitRuntimeEventDelivery(ctx, fresh); err != nil || duplicate {
		t.Fatalf("刷新后的 v2 transaction 应可 commit: duplicate=%v err=%v", duplicate, err)
	}
	var inboxRow runtimeEventDeliveryInboxRow
	if err := store.db.Where("event_id = ?", event.ID).First(&inboxRow).Error; err != nil {
		t.Fatal(err)
	}
	if inboxRow.Version != agentruntime.RuntimeEventDeliveryEnvelopeVersionIssuedAt || !inboxRow.Timestamp.Equal(event.Timestamp) || !inboxRow.IssuedAt.Equal(now) {
		t.Fatalf("SQLite inbox 应持久化 immutable timestamp 与 issued_at: %#v", inboxRow)
	}
}
