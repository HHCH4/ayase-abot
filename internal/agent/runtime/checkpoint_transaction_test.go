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

func signedCheckpointTransactionEnvelope(t *testing.T, snapshot RuntimeSnapshot, eventSequence int64, timestamp time.Time) (RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection) {
	t.Helper()
	envelope, projection, err := NewRuntimeCheckpointDeliveryEnvelope("runtime-a", "runtime-b", snapshot, eventSequence)
	if err != nil {
		t.Fatal(err)
	}
	envelope.Timestamp = timestamp.UTC()
	envelope, err = SignRuntimeCheckpointDeliveryEnvelope(envelope, []byte("checkpoint-transaction-secret"))
	if err != nil {
		t.Fatal(err)
	}
	return envelope, projection
}

func TestMemoryRuntimeCheckpointDeliveryPrepareCommitIsIdempotent(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	repo := NewMemoryRepository()
	snapshot := checkpointDeliveryTestSnapshot(t, "inv-checkpoint-transaction", now)
	envelope, projection := signedCheckpointTransactionEnvelope(t, snapshot, 3, now)
	txnRepo, ok := any(repo).(RuntimeCheckpointDeliveryTransactionRepository)
	if !ok {
		t.Fatal("Memory repository 未实现 checkpoint transaction repository")
	}
	duplicate, err := txnRepo.PrepareRuntimeCheckpointDelivery(ctx, envelope, projection)
	if err != nil || duplicate {
		t.Fatalf("首次 prepare 错误: duplicate=%v err=%v", duplicate, err)
	}
	duplicate, err = txnRepo.PrepareRuntimeCheckpointDelivery(ctx, envelope, projection)
	if err != nil || !duplicate {
		t.Fatalf("重复 prepare 必须幂等: duplicate=%v err=%v", duplicate, err)
	}
	transaction, err := repo.GetRuntimeCheckpointDeliveryTransaction(ctx, envelope.DeliveryID)
	if err != nil || transaction.Status != RuntimeCheckpointDeliveryTransactionPrepared {
		t.Fatalf("prepare 状态错误: %#v err=%v", transaction, err)
	}
	if _, err := repo.GetRuntimeCheckpointDelivery(ctx, envelope.InvocationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("prepare 阶段不得提前暴露 inbox: %v", err)
	}
	duplicate, err = txnRepo.CommitRuntimeCheckpointDelivery(ctx, envelope, projection)
	if err != nil || duplicate {
		t.Fatalf("首次 commit 错误: duplicate=%v err=%v", duplicate, err)
	}
	duplicate, err = txnRepo.CommitRuntimeCheckpointDelivery(ctx, envelope, projection)
	if err != nil || !duplicate {
		t.Fatalf("重复 commit 必须幂等: duplicate=%v err=%v", duplicate, err)
	}
	transaction, err = repo.GetRuntimeCheckpointDeliveryTransaction(ctx, envelope.DeliveryID)
	if err != nil || transaction.Status != RuntimeCheckpointDeliveryTransactionCommitted {
		t.Fatalf("commit 状态错误: %#v err=%v", transaction, err)
	}
	stored, err := repo.GetRuntimeCheckpointDelivery(ctx, envelope.InvocationID)
	if err != nil || stored.SnapshotDigest != envelope.SnapshotDigest {
		t.Fatalf("commit 后 inbox 缺失: %#v err=%v", stored, err)
	}
	transaction.Projection.Workflow.Boundaries[0].PendingWaitIDs[0] = "mutated"
	again, err := repo.GetRuntimeCheckpointDeliveryTransaction(ctx, envelope.DeliveryID)
	if err != nil || again.Projection.Workflow.Boundaries[0].PendingWaitIDs[0] == "mutated" {
		t.Fatalf("transaction projection 必须防御性复制: %#v err=%v", again, err)
	}
	conflict := projection
	conflict.Budget.ToolCallsUsed++
	if _, err := txnRepo.PrepareRuntimeCheckpointDelivery(ctx, envelope, conflict); !errors.Is(err, ErrRuntimeCheckpointDeliveryAuth) {
		t.Fatalf("同 delivery 不同 projection 必须拒绝: %v", err)
	}
}

func TestMemoryRuntimeCheckpointDeliveryTransactionExpiryAndCommitRequiresPrepare(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	repo := NewMemoryRepository()
	snapshot := checkpointDeliveryTestSnapshot(t, "inv-checkpoint-transaction-expiry", now)
	envelope, projection := signedCheckpointTransactionEnvelope(t, snapshot, 2, now)
	txnRepo := any(repo).(RuntimeCheckpointDeliveryTransactionRepository)
	if _, err := txnRepo.CommitRuntimeCheckpointDelivery(ctx, envelope, projection); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未 prepare 的 commit 必须拒绝: %v", err)
	}
	if _, err := txnRepo.PrepareRuntimeCheckpointDelivery(ctx, envelope, projection); err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	transaction := repo.checkpointTxns[envelope.DeliveryID]
	transaction.ExpiresAt = now.Add(-time.Second)
	repo.checkpointTxns[envelope.DeliveryID] = transaction
	repo.mu.Unlock()
	if _, err := txnRepo.CommitRuntimeCheckpointDelivery(ctx, envelope, projection); !errors.Is(err, ErrRuntimeCheckpointDeliveryStale) {
		t.Fatalf("过期 transaction 必须拒绝 commit: %v", err)
	}
}

func TestMemoryRuntimeCheckpointDeliveryAbortIsIdempotentAndNeverPublishes(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	repo := NewMemoryRepository()
	snapshot := checkpointDeliveryTestSnapshot(t, "inv-checkpoint-transaction-abort", now)
	envelope, projection := signedCheckpointTransactionEnvelope(t, snapshot, 2, now)
	txnRepo := any(repo).(RuntimeCheckpointDeliveryTransactionRepository)
	aborter := any(repo).(RuntimeCheckpointDeliveryAbortableTransactionRepository)
	if duplicate, err := txnRepo.PrepareRuntimeCheckpointDelivery(ctx, envelope, projection); err != nil || duplicate {
		t.Fatalf("prepare 失败: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := aborter.AbortRuntimeCheckpointDelivery(ctx, envelope, projection); err != nil || duplicate {
		t.Fatalf("首次 abort 失败: duplicate=%v err=%v", duplicate, err)
	}
	transaction, err := repo.GetRuntimeCheckpointDeliveryTransaction(ctx, envelope.DeliveryID)
	if err != nil || transaction.Status != RuntimeCheckpointDeliveryTransactionAborted {
		t.Fatalf("abort 状态错误: %#v err=%v", transaction, err)
	}
	if _, err := repo.GetRuntimeCheckpointDelivery(ctx, envelope.InvocationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("abort 不得发布 checkpoint inbox: %v", err)
	}
	if duplicate, err := aborter.AbortRuntimeCheckpointDelivery(ctx, envelope, projection); err != nil || !duplicate {
		t.Fatalf("重复 abort 必须幂等: duplicate=%v err=%v", duplicate, err)
	}
	if _, err := txnRepo.CommitRuntimeCheckpointDelivery(ctx, envelope, projection); !errors.Is(err, ErrConflict) {
		t.Fatalf("aborted transaction 不得 commit: %v", err)
	}
	conflict := projection
	conflict.Budget.ToolCallsUsed++
	if _, err := aborter.AbortRuntimeCheckpointDelivery(ctx, envelope, conflict); !errors.Is(err, ErrRuntimeCheckpointDeliveryAuth) {
		t.Fatalf("abort 必须校验 projection identity: %v", err)
	}
}

type checkpointTransactionalTransportFunc struct {
	deliver func(context.Context, RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error)
	prepare func(context.Context, RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error)
	commit  func(context.Context, RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error)
}

func (f checkpointTransactionalTransportFunc) Deliver(ctx context.Context, envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
	if f.deliver == nil {
		return RuntimeCheckpointDeliveryReceipt{}, errors.New("one-phase delivery 不应被调用")
	}
	return f.deliver(ctx, envelope, projection)
}

func (f checkpointTransactionalTransportFunc) Prepare(ctx context.Context, envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
	return f.prepare(ctx, envelope, projection)
}

func (f checkpointTransactionalTransportFunc) Commit(ctx context.Context, envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
	return f.commit(ctx, envelope, projection)
}

func TestCoordinatorUsesIdempotentCheckpointPrepareCommit(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-checkpoint-transaction-dispatch", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	_, _, outbox, err := repo.CommitRuntimeSnapshotWithCheckpoint(ctx, checkpointDeliveryTestSnapshot(t, invocation.ID, now), AgentEvent{ID: "checkpoint-transaction-dispatch-event", InvocationID: invocation.ID, Type: EventRuntimeSnapshot, Timestamp: now}, "runtime-a", "runtime-b")
	if err != nil {
		t.Fatal(err)
	}
	var prepareCalls, commitCalls atomic.Int32
	var firstCommit atomic.Bool
	coordinator := &Coordinator{repo: repo, checkpointOutboxWorkerID: "checkpoint-transaction-worker", checkpointDeliverySource: "runtime-a", checkpointDeliveryDestination: "runtime-b"}
	coordinator.checkpointDeliveryTransport = checkpointTransactionalTransportFunc{
		prepare: func(_ context.Context, envelope RuntimeCheckpointDeliveryEnvelope, _ RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
			prepareCalls.Add(1)
			return RuntimeCheckpointDeliveryReceipt{Version: envelope.Version, DeliveryID: envelope.DeliveryID, InvocationID: envelope.InvocationID, SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence, SnapshotDigest: envelope.SnapshotDigest, Phase: RuntimeCheckpointDeliveryTransactionPhasePrepared}, nil
		},
		commit: func(_ context.Context, envelope RuntimeCheckpointDeliveryEnvelope, _ RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
			commitCalls.Add(1)
			receipt := RuntimeCheckpointDeliveryReceipt{Version: envelope.Version, DeliveryID: envelope.DeliveryID, InvocationID: envelope.InvocationID, SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence, SnapshotDigest: envelope.SnapshotDigest, Phase: RuntimeCheckpointDeliveryTransactionPhaseCommitted}
			if !firstCommit.Swap(true) {
				return receipt, errors.New("remote commit accepted before source acknowledgement")
			}
			return receipt, nil
		},
	}
	coordinator.dispatchDueRuntimeCheckpointOutbox(ctx, now)
	queued, err := repo.GetRuntimeCheckpointDeliveryOutbox(ctx, outbox.ID)
	if err != nil || queued.Status != RuntimeCheckpointDeliveryOutboxQueued || !queued.AvailableAt.After(now) {
		t.Fatalf("第一次 commit 失败后必须保留可重试 outbox: %#v err=%v", queued, err)
	}
	coordinator.dispatchDueRuntimeCheckpointOutbox(ctx, queued.AvailableAt.Add(time.Nanosecond))
	completed, err := repo.GetRuntimeCheckpointDeliveryOutbox(ctx, outbox.ID)
	if err != nil || completed.Status != RuntimeCheckpointDeliveryOutboxCompleted {
		t.Fatalf("第二次 prepare+commit 应完成 outbox: %#v err=%v", completed, err)
	}
	if prepareCalls.Load() != 2 || commitCalls.Load() != 2 {
		t.Fatalf("prepare/commit 必须在崩溃窗口安全重放: prepare=%d commit=%d", prepareCalls.Load(), commitCalls.Load())
	}
}

func TestRuntimeCheckpointDeliveryHTTPTransactionIsAuthenticatedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	repo := NewMemoryRepository()
	receiver, err := NewRuntimeCheckpointDeliveryReceiver("runtime-a", "runtime-b", []byte("checkpoint-transaction-secret"), repo)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now.Add(time.Second) }
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/checkpoint/prepare":
			receiver.ServeTransactionHTTP(writer, request, RuntimeCheckpointDeliveryTransactionPhasePrepared)
		case "/checkpoint/commit":
			receiver.ServeTransactionHTTP(writer, request, RuntimeCheckpointDeliveryTransactionPhaseCommitted)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	transport, err := NewRuntimeCheckpointDeliveryHTTPTransactionalTransport(server.URL+"/checkpoint", "runtime-a", "runtime-b", []byte("checkpoint-transaction-secret"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := checkpointDeliveryTestSnapshot(t, "inv-checkpoint-http-transaction", now)
	envelope, projection := signedCheckpointTransactionEnvelope(t, snapshot, 2, now)
	payload, err := json.Marshal(RuntimeCheckpointDeliverySourceResponse{Envelope: envelope, Projection: projection})
	if err != nil {
		t.Fatal(err)
	}
	wrongPhaseRequest := httptest.NewRequest(http.MethodPost, "/checkpoint/prepare", bytes.NewReader(payload))
	wrongPhaseRequest.Header.Set("X-Abot-Checkpoint-Transaction-Phase", RuntimeCheckpointDeliveryTransactionPhaseCommitted)
	wrongPhaseResponse := httptest.NewRecorder()
	receiver.ServeTransactionHTTP(wrongPhaseResponse, wrongPhaseRequest, RuntimeCheckpointDeliveryTransactionPhasePrepared)
	if wrongPhaseResponse.Code != http.StatusBadRequest {
		t.Fatalf("route/header phase 不一致必须拒绝: status=%d body=%s", wrongPhaseResponse.Code, wrongPhaseResponse.Body.String())
	}
	// The transport signs again immediately before each phase; the unsigned
	// identity passed here is what a source outbox supplies.
	envelope.Signature = ""
	prepared, err := transport.Prepare(ctx, envelope, projection)
	if err != nil || prepared.Phase != RuntimeCheckpointDeliveryTransactionPhasePrepared {
		t.Fatalf("HTTP prepare 错误: %#v err=%v", prepared, err)
	}
	committed, err := transport.Commit(ctx, envelope, projection)
	if err != nil || committed.Phase != RuntimeCheckpointDeliveryTransactionPhaseCommitted {
		t.Fatalf("HTTP commit 错误: %#v err=%v", committed, err)
	}
	duplicate, err := transport.Commit(ctx, envelope, projection)
	if err != nil || !duplicate.Duplicate {
		t.Fatalf("HTTP 重复 commit 必须幂等: %#v err=%v", duplicate, err)
	}
	if _, err := repo.GetRuntimeCheckpointDelivery(ctx, snapshot.InvocationID); err != nil {
		t.Fatalf("HTTP commit 后 destination inbox 缺失: %v", err)
	}
}

func TestRuntimeCheckpointDeliveryHTTPTransactionAbortIsIdempotent(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	repo := NewMemoryRepository()
	receiver, err := NewRuntimeCheckpointDeliveryReceiver("runtime-a", "runtime-b", []byte("checkpoint-transaction-secret"), repo)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now.Add(time.Second) }
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/checkpoint/prepare":
			receiver.PrepareHandler().ServeHTTP(writer, request)
		case "/checkpoint/abort":
			receiver.AbortHandler().ServeHTTP(writer, request)
		case "/checkpoint/commit":
			receiver.CommitHandler().ServeHTTP(writer, request)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	transport, err := NewRuntimeCheckpointDeliveryHTTPTransactionalTransport(server.URL+"/checkpoint", "runtime-a", "runtime-b", []byte("checkpoint-transaction-secret"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := checkpointDeliveryTestSnapshot(t, "inv-checkpoint-http-abort", now)
	envelope, projection := signedCheckpointTransactionEnvelope(t, snapshot, 2, now)
	if _, err := transport.Prepare(ctx, envelope, projection); err != nil {
		t.Fatalf("HTTP prepare 失败: %v", err)
	}
	aborted, err := transport.Abort(ctx, envelope, projection)
	if err != nil || aborted.Phase != RuntimeCheckpointDeliveryTransactionPhaseAborted || aborted.Duplicate {
		t.Fatalf("HTTP abort 失败: %#v err=%v", aborted, err)
	}
	aborted, err = transport.Abort(ctx, envelope, projection)
	if err != nil || !aborted.Duplicate {
		t.Fatalf("HTTP 重复 abort 必须幂等: %#v err=%v", aborted, err)
	}
	transaction, err := repo.GetRuntimeCheckpointDeliveryTransaction(ctx, envelope.DeliveryID)
	if err != nil || transaction.Status != RuntimeCheckpointDeliveryTransactionAborted {
		t.Fatalf("HTTP abort 状态错误: %#v err=%v", transaction, err)
	}
	if _, err := transport.Commit(ctx, envelope, projection); err == nil {
		t.Fatalf("aborted transaction 不得 commit: %v", err)
	}
}

func TestCoordinatorHTTPCheckpointTransactionEndToEnd(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	sourceRepo := NewMemoryRepository()
	destinationRepo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-checkpoint-http-e2e", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := sourceRepo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	receiver, err := NewRuntimeCheckpointDeliveryReceiver("runtime-a", "runtime-b", []byte("checkpoint-transaction-secret"), destinationRepo)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now.Add(time.Second) }
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/checkpoint/prepare":
			receiver.ServeTransactionHTTP(writer, request, RuntimeCheckpointDeliveryTransactionPhasePrepared)
		case "/checkpoint/commit":
			receiver.ServeTransactionHTTP(writer, request, RuntimeCheckpointDeliveryTransactionPhaseCommitted)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	transport, err := NewRuntimeCheckpointDeliveryHTTPTransactionalTransport(server.URL+"/checkpoint", "runtime-a", "runtime-b", []byte("checkpoint-transaction-secret"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, _, outbox, err := sourceRepo.CommitRuntimeSnapshotWithCheckpoint(ctx, checkpointDeliveryTestSnapshot(t, invocation.ID, now), AgentEvent{ID: "checkpoint-http-e2e-event", InvocationID: invocation.ID, Type: EventRuntimeSnapshot, Timestamp: now}, "runtime-a", "runtime-b")
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: sourceRepo, checkpointOutboxWorkerID: "checkpoint-http-e2e-worker", checkpointDeliverySource: "runtime-a", checkpointDeliveryDestination: "runtime-b", checkpointDeliveryTransport: transport}
	coordinator.dispatchDueRuntimeCheckpointOutbox(ctx, now)
	completed, err := sourceRepo.GetRuntimeCheckpointDeliveryOutbox(ctx, outbox.ID)
	if err != nil || completed.Status != RuntimeCheckpointDeliveryOutboxCompleted {
		t.Fatalf("Coordinator HTTP transaction 未完成 source outbox: %#v err=%v", completed, err)
	}
	transactionRepo := any(destinationRepo).(interface {
		GetRuntimeCheckpointDeliveryTransaction(context.Context, string) (RuntimeCheckpointDeliveryTransaction, error)
	})
	transaction, err := transactionRepo.GetRuntimeCheckpointDeliveryTransaction(ctx, outbox.DeliveryID)
	if err != nil || transaction.Status != RuntimeCheckpointDeliveryTransactionCommitted {
		t.Fatalf("destination transaction 未提交: %#v err=%v", transaction, err)
	}
	if _, err := destinationRepo.GetRuntimeCheckpointDelivery(ctx, invocation.ID); err != nil {
		t.Fatalf("destination inbox 未收到 checkpoint: %v", err)
	}
}

func TestCoordinatorHTTPTransportKeepsOnePhaseCompatibilityByDefault(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	sourceRepo := NewMemoryRepository()
	destinationRepo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-checkpoint-http-one-phase", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := sourceRepo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	secret := []byte("checkpoint-transaction-secret")
	receiver, err := NewRuntimeCheckpointDeliveryReceiver("runtime-a", "runtime-b", secret, destinationRepo)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now.Add(time.Second) }
	server := httptest.NewServer(receiver)
	defer server.Close()
	transport, err := NewRuntimeCheckpointDeliveryHTTPTransport(server.URL, "runtime-a", "runtime-b", secret, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if transport.RuntimeCheckpointDeliveryTransactionsEnabled() {
		t.Fatal("旧 HTTP transport 默认不得隐式启用 prepare/commit")
	}
	_, _, outbox, err := sourceRepo.CommitRuntimeSnapshotWithCheckpoint(ctx, checkpointDeliveryTestSnapshot(t, invocation.ID, now), AgentEvent{ID: "checkpoint-http-one-phase-event", InvocationID: invocation.ID, Type: EventRuntimeSnapshot, Timestamp: now}, "runtime-a", "runtime-b")
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: sourceRepo, checkpointOutboxWorkerID: "checkpoint-http-one-phase-worker", checkpointDeliverySource: "runtime-a", checkpointDeliveryDestination: "runtime-b", checkpointDeliveryTransport: transport}
	coordinator.dispatchDueRuntimeCheckpointOutbox(ctx, now)
	completed, err := sourceRepo.GetRuntimeCheckpointDeliveryOutbox(ctx, outbox.ID)
	if err != nil || completed.Status != RuntimeCheckpointDeliveryOutboxCompleted {
		t.Fatalf("旧 HTTP receiver 仅提供 /checkpoint 时仍应完成 outbox: %#v err=%v", completed, err)
	}
	if _, err := destinationRepo.GetRuntimeCheckpointDelivery(ctx, invocation.ID); err != nil {
		t.Fatalf("旧 HTTP transport 未投递 checkpoint: %v", err)
	}
}
