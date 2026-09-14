package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func signedEventTransactionEnvelope(t *testing.T, event AgentEvent, outbox RuntimeEventOutbox, timestamp time.Time) RuntimeEventDeliveryEnvelope {
	t.Helper()
	envelope, err := NewRuntimeEventDeliveryEnvelope("runtime-a", "runtime-b", outbox, event)
	if err != nil {
		t.Fatal(err)
	}
	envelope.Timestamp = timestamp.UTC()
	envelope, err = SignRuntimeEventDeliveryEnvelope(envelope, []byte(testRuntimeEventDeliverySecret))
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func TestMemoryRuntimeEventDeliveryPrepareCommitIsIdempotent(t *testing.T) {
	ctx := context.Background()
	event, outbox, now := runtimeEventDeliveryFixture(t)
	repo := NewMemoryRepository()
	txnRepo, ok := any(repo).(RuntimeEventDeliveryTransactionRepository)
	if !ok {
		t.Fatal("Memory repository 未实现 event transaction repository")
	}
	envelope := signedEventTransactionEnvelope(t, event, outbox, now)
	if _, err := txnRepo.CommitRuntimeEventDelivery(ctx, envelope); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未 prepare 的 commit 必须拒绝: %v", err)
	}
	duplicate, err := txnRepo.PrepareRuntimeEventDelivery(ctx, envelope)
	if err != nil || duplicate {
		t.Fatalf("首次 prepare 错误: duplicate=%v err=%v", duplicate, err)
	}
	duplicate, err = txnRepo.PrepareRuntimeEventDelivery(ctx, envelope)
	if err != nil || !duplicate {
		t.Fatalf("重复 prepare 必须幂等: duplicate=%v err=%v", duplicate, err)
	}
	if _, err := repo.GetRuntimeEventDeliveryTransaction(ctx, envelope.DeliveryID); err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	_, inboxVisible := repo.eventInbox[envelope.EventID]
	repo.mu.Unlock()
	if inboxVisible {
		t.Fatal("prepare 阶段不得提前暴露 event inbox")
	}
	duplicate, err = txnRepo.CommitRuntimeEventDelivery(ctx, envelope)
	if err != nil || duplicate {
		t.Fatalf("首次 commit 错误: duplicate=%v err=%v", duplicate, err)
	}
	duplicate, err = txnRepo.CommitRuntimeEventDelivery(ctx, envelope)
	if err != nil || !duplicate {
		t.Fatalf("重复 commit 必须幂等: duplicate=%v err=%v", duplicate, err)
	}
	transaction, err := repo.GetRuntimeEventDeliveryTransaction(ctx, envelope.DeliveryID)
	if err != nil || transaction.Status != RuntimeEventDeliveryTransactionCommitted {
		t.Fatalf("commit 状态错误: %#v err=%v", transaction, err)
	}
	repo.mu.Lock()
	stored, inboxVisible := repo.eventInbox[envelope.EventID]
	repo.mu.Unlock()
	if !inboxVisible || stored.EventDigest != envelope.EventDigest {
		t.Fatalf("commit 后 event inbox 缺失: %#v", stored)
	}
	refreshed := envelope
	refreshed.Signature = ""
	refreshed, err = SignRuntimeEventDeliveryEnvelope(refreshed, []byte(testRuntimeEventDeliverySecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := txnRepo.PrepareRuntimeEventDelivery(ctx, refreshed); err != nil {
		t.Fatalf("相同 immutable metadata 的签名刷新不应冲突: %v", err)
	}
	conflict := envelope
	conflict.Sequence++
	if _, err := txnRepo.PrepareRuntimeEventDelivery(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("同 delivery 不同 event metadata 必须冲突: %v", err)
	}

	repo.mu.Lock()
	transaction = repo.eventTxns[envelope.DeliveryID]
	transaction.Status = RuntimeEventDeliveryTransactionPrepared
	transaction.ExpiresAt = now.Add(-time.Second)
	repo.eventTxns[envelope.DeliveryID] = transaction
	repo.mu.Unlock()
	if _, err := txnRepo.CommitRuntimeEventDelivery(ctx, envelope); !errors.Is(err, ErrRuntimeEventDeliveryStale) {
		t.Fatalf("过期 transaction 必须拒绝 commit: %v", err)
	}
}

func TestMemoryRuntimeEventDeliveryIssuedAtRefreshesExpiredPrepare(t *testing.T) {
	ctx := context.Background()
	event, outbox, now := runtimeEventDeliveryFixture(t)
	repo := NewMemoryRepository()
	txnRepo := any(repo).(RuntimeEventDeliveryTransactionRepository)
	legacy := signedEventTransactionEnvelope(t, event, outbox, now)
	if duplicate, err := txnRepo.PrepareRuntimeEventDelivery(ctx, legacy); err != nil || duplicate {
		t.Fatalf("首次 v1 prepare 错误: duplicate=%v err=%v", duplicate, err)
	}
	repo.mu.Lock()
	transaction := repo.eventTxns[legacy.DeliveryID]
	transaction.Status = RuntimeEventDeliveryTransactionPrepared
	transaction.ExpiresAt = now.Add(-time.Second)
	repo.eventTxns[legacy.DeliveryID] = transaction
	repo.mu.Unlock()

	fresh, err := RefreshRuntimeEventDeliveryEnvelope(legacy, now.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	fresh, err = SignRuntimeEventDeliveryEnvelope(fresh, []byte(testRuntimeEventDeliverySecret))
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := txnRepo.PrepareRuntimeEventDelivery(ctx, fresh); err != nil || duplicate {
		t.Fatalf("过期 prepare 应允许同一 immutable event 用 v2 issued_at 刷新: duplicate=%v err=%v", duplicate, err)
	}
	stored, err := repo.GetRuntimeEventDeliveryTransaction(ctx, legacy.DeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != RuntimeEventDeliveryEnvelopeVersionIssuedAt || !stored.Timestamp.Equal(event.Timestamp) || !stored.IssuedAt.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("刷新后的 transaction metadata 错误: %#v", stored)
	}
	if duplicate, err := txnRepo.CommitRuntimeEventDelivery(ctx, fresh); err != nil || duplicate {
		t.Fatalf("刷新后的 v2 prepare 应可 commit: duplicate=%v err=%v", duplicate, err)
	}
	repo.mu.Lock()
	inbox, ok := repo.eventInbox[event.ID]
	repo.mu.Unlock()
	if !ok || inbox.Version != RuntimeEventDeliveryEnvelopeVersionIssuedAt || !inbox.IssuedAt.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("v2 commit 应写入 issued_at metadata: ok=%v inbox=%#v", ok, inbox)
	}
}

func TestMemoryRuntimeEventDeliveryAbortIsIdempotentAndNeverPublishes(t *testing.T) {
	ctx := context.Background()
	event, outbox, now := runtimeEventDeliveryFixture(t)
	repo := NewMemoryRepository()
	txnRepo := any(repo).(RuntimeEventDeliveryTransactionRepository)
	aborter := any(repo).(RuntimeEventDeliveryAbortableTransactionRepository)
	envelope := signedEventTransactionEnvelope(t, event, outbox, now)
	if duplicate, err := txnRepo.PrepareRuntimeEventDelivery(ctx, envelope); err != nil || duplicate {
		t.Fatalf("prepare 失败: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := aborter.AbortRuntimeEventDelivery(ctx, envelope); err != nil || duplicate {
		t.Fatalf("首次 abort 失败: duplicate=%v err=%v", duplicate, err)
	}
	transaction, err := repo.GetRuntimeEventDeliveryTransaction(ctx, envelope.DeliveryID)
	if err != nil || transaction.Status != RuntimeEventDeliveryTransactionAborted {
		t.Fatalf("abort 状态错误: %#v err=%v", transaction, err)
	}
	if _, err := repo.GetRuntimeEventDelivery(ctx, envelope.EventID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("abort 不得发布 event inbox: %v", err)
	}
	if duplicate, err := aborter.AbortRuntimeEventDelivery(ctx, envelope); err != nil || !duplicate {
		t.Fatalf("重复 abort 必须幂等: duplicate=%v err=%v", duplicate, err)
	}
	if _, err := txnRepo.CommitRuntimeEventDelivery(ctx, envelope); !errors.Is(err, ErrConflict) {
		t.Fatalf("aborted transaction 不得 commit: %v", err)
	}
	conflict := envelope
	conflict.Sequence++
	if _, err := aborter.AbortRuntimeEventDelivery(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("abort 必须校验 immutable metadata: %v", err)
	}
}

type eventTransactionalTransportFunc struct {
	build   func(RuntimeEventOutbox, AgentEvent) (RuntimeEventDeliveryEnvelope, error)
	prepare func(context.Context, RuntimeEventDeliveryEnvelope) (RuntimeEventDeliveryReceipt, error)
	commit  func(context.Context, RuntimeEventDeliveryEnvelope) (RuntimeEventDeliveryReceipt, error)
}

func (f eventTransactionalTransportFunc) Deliver(context.Context, RuntimeEventOutbox, AgentEvent) error {
	return errors.New("one-phase delivery 不应被调用")
}

func (f eventTransactionalTransportFunc) BuildEnvelope(outbox RuntimeEventOutbox, event AgentEvent) (RuntimeEventDeliveryEnvelope, error) {
	return f.build(outbox, event)
}

func (f eventTransactionalTransportFunc) Prepare(ctx context.Context, envelope RuntimeEventDeliveryEnvelope) (RuntimeEventDeliveryReceipt, error) {
	return f.prepare(ctx, envelope)
}

func (f eventTransactionalTransportFunc) Commit(ctx context.Context, envelope RuntimeEventDeliveryEnvelope) (RuntimeEventDeliveryReceipt, error) {
	return f.commit(ctx, envelope)
}

func TestCoordinatorUsesIdempotentEventPrepareCommit(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	invocation := eventOutboxInvocation(t, repo, "inv-event-transaction-dispatch")
	now := time.Now().UTC().Truncate(time.Millisecond)
	event, err := repo.AppendEvent(ctx, AgentEvent{ID: "event-transaction-dispatch", InvocationID: invocation.ID, Type: EventRuntimeNotice, Timestamp: now})
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := NewRuntimeEventOutbox(event)
	if err != nil {
		t.Fatal(err)
	}
	var prepareCalls, commitCalls atomic.Int32
	var firstCommit atomic.Bool
	transport := eventTransactionalTransportFunc{
		build: func(outbox RuntimeEventOutbox, got AgentEvent) (RuntimeEventDeliveryEnvelope, error) {
			return NewRuntimeEventDeliveryEnvelope("runtime-a", "runtime-b", outbox, got)
		},
		prepare: func(_ context.Context, envelope RuntimeEventDeliveryEnvelope) (RuntimeEventDeliveryReceipt, error) {
			prepareCalls.Add(1)
			return RuntimeEventDeliveryReceipt{Version: envelope.Version, DeliveryID: envelope.DeliveryID, EventID: envelope.EventID, InvocationID: envelope.InvocationID, Sequence: envelope.Sequence, Type: envelope.Type, EventDigest: envelope.EventDigest, Phase: RuntimeEventDeliveryTransactionPhasePrepared}, nil
		},
		commit: func(_ context.Context, envelope RuntimeEventDeliveryEnvelope) (RuntimeEventDeliveryReceipt, error) {
			commitCalls.Add(1)
			receipt := RuntimeEventDeliveryReceipt{Version: envelope.Version, DeliveryID: envelope.DeliveryID, EventID: envelope.EventID, InvocationID: envelope.InvocationID, Sequence: envelope.Sequence, Type: envelope.Type, EventDigest: envelope.EventDigest, Phase: RuntimeEventDeliveryTransactionPhaseCommitted}
			if !firstCommit.Swap(true) {
				return receipt, errors.New("remote commit accepted before source acknowledgement")
			}
			return receipt, nil
		},
	}
	coordinator := &Coordinator{repo: repo, eventOutboxWorkerID: "event-transaction-worker"}
	coordinator.SetRuntimeEventDeliveryTransport(transport)
	coordinator.dispatchDueRuntimeEventOutbox(ctx, now)
	item, err := repo.GetRuntimeEventOutbox(ctx, outbox.ID)
	if err != nil || item.Status != RuntimeEventOutboxQueued || !item.AvailableAt.After(now) {
		t.Fatalf("第一次 commit 失败后必须保留可重试 outbox: %#v err=%v", item, err)
	}
	coordinator.dispatchDueRuntimeEventOutbox(ctx, item.AvailableAt.Add(time.Nanosecond))
	item, err = repo.GetRuntimeEventOutbox(ctx, outbox.ID)
	if err != nil || item.Status != RuntimeEventOutboxCompleted {
		t.Fatalf("第二次 prepare+commit 应完成 outbox: %#v err=%v", item, err)
	}
	if prepareCalls.Load() != 2 || commitCalls.Load() != 2 {
		t.Fatalf("prepare/commit 必须在崩溃窗口安全重放: prepare=%d commit=%d", prepareCalls.Load(), commitCalls.Load())
	}
}

func TestRuntimeEventDeliveryHTTPTransactionIsAuthenticatedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	event, outbox, now := runtimeEventDeliveryFixture(t)
	repo := NewMemoryRepository()
	receiver, err := NewRuntimeEventDeliveryReceiver("runtime-a", "runtime-b", []byte(testRuntimeEventDeliverySecret), repo)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now.Add(time.Second) }
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/events/prepare":
			receiver.ServeTransactionHTTP(writer, request, RuntimeEventDeliveryTransactionPhasePrepared)
		case "/events/commit":
			receiver.ServeTransactionHTTP(writer, request, RuntimeEventDeliveryTransactionPhaseCommitted)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	transport, err := NewRuntimeEventDeliveryHTTPTransactionalTransport(server.URL+"/events", "runtime-a", "runtime-b", []byte(testRuntimeEventDeliverySecret), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	envelope := signedEventTransactionEnvelope(t, event, outbox, now)
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	wrongPhase := httptest.NewRequest(http.MethodPost, "/events/prepare", bytes.NewReader(body))
	wrongPhase.Header.Set("X-Abot-Event-Transaction-Phase", RuntimeEventDeliveryTransactionPhaseCommitted)
	response := httptest.NewRecorder()
	receiver.ServeTransactionHTTP(response, wrongPhase, RuntimeEventDeliveryTransactionPhasePrepared)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("route/header phase 不一致必须拒绝: status=%d body=%s", response.Code, response.Body.String())
	}
	envelope.Signature = ""
	prepared, err := transport.Prepare(ctx, envelope)
	if err != nil || prepared.Phase != RuntimeEventDeliveryTransactionPhasePrepared {
		t.Fatalf("HTTP prepare 错误: %#v err=%v", prepared, err)
	}
	committed, err := transport.Commit(ctx, envelope)
	if err != nil || committed.Phase != RuntimeEventDeliveryTransactionPhaseCommitted {
		t.Fatalf("HTTP commit 错误: %#v err=%v", committed, err)
	}
	duplicate, err := transport.Commit(ctx, envelope)
	if err != nil || !duplicate.Duplicate {
		t.Fatalf("HTTP 重复 commit 必须幂等: %#v err=%v", duplicate, err)
	}
	repo.mu.Lock()
	_, inboxVisible := repo.eventInbox[event.ID]
	repo.mu.Unlock()
	if !inboxVisible {
		t.Fatal("HTTP commit 后 destination inbox 缺失")
	}
}

func TestRuntimeEventDeliveryHTTPTransactionAbortIsIdempotent(t *testing.T) {
	ctx := context.Background()
	event, outbox, now := runtimeEventDeliveryFixture(t)
	repo := NewMemoryRepository()
	receiver, err := NewRuntimeEventDeliveryReceiver("runtime-a", "runtime-b", []byte(testRuntimeEventDeliverySecret), repo)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now.Add(time.Second) }
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/events/prepare":
			receiver.PrepareHandler().ServeHTTP(writer, request)
		case "/events/abort":
			receiver.AbortHandler().ServeHTTP(writer, request)
		case "/events/commit":
			receiver.CommitHandler().ServeHTTP(writer, request)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	transport, err := NewRuntimeEventDeliveryHTTPTransactionalTransport(server.URL+"/events", "runtime-a", "runtime-b", []byte(testRuntimeEventDeliverySecret), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	envelope := signedEventTransactionEnvelope(t, event, outbox, now)
	if _, err := transport.Prepare(ctx, envelope); err != nil {
		t.Fatalf("HTTP prepare 失败: %v", err)
	}
	aborted, err := transport.Abort(ctx, envelope)
	if err != nil || aborted.Phase != RuntimeEventDeliveryTransactionPhaseAborted || aborted.Duplicate {
		t.Fatalf("HTTP abort 失败: %#v err=%v", aborted, err)
	}
	aborted, err = transport.Abort(ctx, envelope)
	if err != nil || !aborted.Duplicate {
		t.Fatalf("HTTP 重复 abort 必须幂等: %#v err=%v", aborted, err)
	}
	transaction, err := repo.GetRuntimeEventDeliveryTransaction(ctx, envelope.DeliveryID)
	if err != nil || transaction.Status != RuntimeEventDeliveryTransactionAborted {
		t.Fatalf("HTTP abort 状态错误: %#v err=%v", transaction, err)
	}
	if _, err := transport.Commit(ctx, envelope); err == nil {
		t.Fatalf("aborted transaction 不得 commit: %v", err)
	}
}

func TestCoordinatorHTTPEventTransactionEndToEnd(t *testing.T) {
	ctx := context.Background()
	sourceRepo := NewMemoryRepository()
	destinationRepo := NewMemoryRepository()
	invocation := eventOutboxInvocation(t, sourceRepo, "inv-event-http-transaction")
	now := time.Now().UTC().Truncate(time.Millisecond)
	event, err := sourceRepo.AppendEvent(ctx, AgentEvent{ID: "event-http-transaction", InvocationID: invocation.ID, Type: EventRuntimeNotice, Timestamp: now, Data: map[string]any{"private": "body"}})
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := NewRuntimeEventOutbox(event)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewRuntimeEventDeliveryReceiver("runtime-a", "runtime-b", []byte(testRuntimeEventDeliverySecret), destinationRepo)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now.Add(time.Second) }
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/events/prepare":
			receiver.ServeTransactionHTTP(writer, request, RuntimeEventDeliveryTransactionPhasePrepared)
		case "/events/commit":
			receiver.ServeTransactionHTTP(writer, request, RuntimeEventDeliveryTransactionPhaseCommitted)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	transport, err := NewRuntimeEventDeliveryHTTPTransactionalTransport(server.URL+"/events", "runtime-a", "runtime-b", []byte(testRuntimeEventDeliverySecret), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: sourceRepo, eventOutboxWorkerID: "event-http-transaction-worker"}
	coordinator.SetRuntimeEventDeliveryTransport(transport)
	coordinator.dispatchDueRuntimeEventOutbox(ctx, now)
	outbox, err = sourceRepo.GetRuntimeEventOutbox(ctx, outbox.ID)
	if err != nil || outbox.Status != RuntimeEventOutboxCompleted {
		t.Fatalf("Coordinator HTTP transaction 未完成 source outbox: %#v err=%v", outbox, err)
	}
	transaction, err := destinationRepo.GetRuntimeEventDeliveryTransaction(ctx, outbox.ID)
	if err != nil || transaction.Status != RuntimeEventDeliveryTransactionCommitted {
		t.Fatalf("destination transaction 未提交: %#v err=%v", transaction, err)
	}
	destinationRepo.mu.Lock()
	_, inboxVisible := destinationRepo.eventInbox[event.ID]
	destinationRepo.mu.Unlock()
	if !inboxVisible {
		t.Fatal("destination event inbox 未收到 event")
	}
}

func TestCoordinatorHTTPEventTransactionIssuedAtReplaysDelayedEvent(t *testing.T) {
	ctx := context.Background()
	sourceRepo := NewMemoryRepository()
	destinationRepo := NewMemoryRepository()
	now := time.Date(2026, 9, 13, 14, 0, 0, 0, time.UTC)
	invocation := eventOutboxInvocation(t, sourceRepo, "inv-event-http-transaction-delayed")
	event, err := sourceRepo.AppendEvent(ctx, AgentEvent{
		ID: "event-http-transaction-delayed", InvocationID: invocation.ID, Type: EventRuntimeNotice,
		Timestamp: now.Add(-2 * time.Hour), Data: map[string]any{"private": "old body"},
	})
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := NewRuntimeEventOutbox(event)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewRuntimeEventDeliveryReceiver("runtime-a", "runtime-b", []byte(testRuntimeEventDeliverySecret), destinationRepo)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now }
	receiver.MaxAge = time.Minute
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/events/prepare":
			receiver.ServeTransactionHTTP(writer, request, RuntimeEventDeliveryTransactionPhasePrepared)
		case "/events/commit":
			receiver.ServeTransactionHTTP(writer, request, RuntimeEventDeliveryTransactionPhaseCommitted)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	transport, err := NewRuntimeEventDeliveryHTTPTransactionalTransportWithIssuedAt(server.URL+"/events", "runtime-a", "runtime-b", []byte(testRuntimeEventDeliverySecret), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	transport.Now = func() time.Time { return now }
	coordinator := &Coordinator{repo: sourceRepo, eventOutboxWorkerID: "event-http-transaction-delayed-worker"}
	coordinator.SetRuntimeEventDeliveryTransport(transport)
	coordinator.dispatchDueRuntimeEventOutbox(ctx, now)
	storedOutbox, err := sourceRepo.GetRuntimeEventOutbox(ctx, outbox.ID)
	if err != nil || storedOutbox.Status != RuntimeEventOutboxCompleted {
		t.Fatalf("v2 transactional transport 应完成滞留 event outbox: %#v err=%v", storedOutbox, err)
	}
	transaction, err := destinationRepo.GetRuntimeEventDeliveryTransaction(ctx, outbox.ID)
	if err != nil || transaction.Status != RuntimeEventDeliveryTransactionCommitted || transaction.Version != RuntimeEventDeliveryEnvelopeVersionIssuedAt || !transaction.Timestamp.Equal(event.Timestamp) || !transaction.IssuedAt.Equal(now) {
		t.Fatalf("destination v2 transaction metadata 错误: %#v err=%v", transaction, err)
	}
}

func TestCoordinatorHTTPEventTransportKeepsOnePhaseCompatibilityByDefault(t *testing.T) {
	ctx := context.Background()
	sourceRepo := NewMemoryRepository()
	destinationRepo := NewMemoryRepository()
	invocation := eventOutboxInvocation(t, sourceRepo, "inv-event-http-one-phase")
	now := time.Now().UTC().Truncate(time.Millisecond)
	event, err := sourceRepo.AppendEvent(ctx, AgentEvent{ID: "event-http-one-phase", InvocationID: invocation.ID, Type: EventRuntimeNotice, Timestamp: now})
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := NewRuntimeEventOutbox(event)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewRuntimeEventDeliveryReceiver("runtime-a", "runtime-b", []byte(testRuntimeEventDeliverySecret), destinationRepo)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(receiver)
	defer server.Close()
	transport, err := NewRuntimeEventDeliveryHTTPTransport(server.URL, "runtime-a", "runtime-b", []byte(testRuntimeEventDeliverySecret), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if transport.RuntimeEventDeliveryTransactionsEnabled() {
		t.Fatal("旧 HTTP transport 默认不得隐式启用 prepare/commit")
	}
	coordinator := &Coordinator{repo: sourceRepo, eventOutboxWorkerID: "event-http-one-phase-worker"}
	coordinator.SetRuntimeEventDeliveryTransport(transport)
	coordinator.dispatchDueRuntimeEventOutbox(ctx, now)
	outbox, err = sourceRepo.GetRuntimeEventOutbox(ctx, outbox.ID)
	if err != nil || outbox.Status != RuntimeEventOutboxCompleted {
		t.Fatalf("旧 HTTP receiver 仅提供 /event-delivery 时仍应完成 outbox: %#v err=%v", outbox, err)
	}
	destinationRepo.mu.Lock()
	_, inboxVisible := destinationRepo.eventInbox[event.ID]
	destinationRepo.mu.Unlock()
	if !inboxVisible {
		t.Fatal("旧 HTTP transport 未投递 event")
	}
}
