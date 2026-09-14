package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Abot/internal/agent"
)

func signedRuntimeConfigDeliveryEnvelope(t *testing.T, source, destination, invocationID, expectedDigest, key string, projection RuntimeConfigDeliveryProjection, now time.Time) (RuntimeConfigDeliveryEnvelope, RuntimeConfigDeliveryProjection) {
	t.Helper()
	outbox, err := NewRuntimeConfigDeliveryOutbox(source, destination, invocationID, expectedDigest, key, projection, now)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := outbox.Envelope(now)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err = SignRuntimeConfigDeliveryEnvelope(envelope, []byte("config-delivery-test-secret-32-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	return envelope, projection
}

func TestRuntimeConfigDeliveryEnvelopeIsSignedMetadataOnlyAndRetrySafe(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	old := migrationSnapshot(t, "old-config", "demo", "model")
	target := migrationSnapshot(t, "new-config", "demo", "model")
	projection, err := parseRuntimeDeliverySnapshot(target)
	if err != nil {
		t.Fatal(err)
	}
	envelope, _ := signedRuntimeConfigDeliveryEnvelope(t, "runtime-a", "runtime-b", "inv-config-delivery", "sha256:"+strings.Repeat("a", 64), "delivery-1", projection, now)
	if err := VerifyRuntimeConfigDeliveryEnvelope(envelope, []byte("config-delivery-test-secret-32-bytes")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(mustMarshalRuntimeConfigProjection(projection), "new-config") {
		// The projection contains only a digest of instruction text; this guard
		// documents the metadata-only contract without relying on implementation
		// details of the fixture helper.
		t.Fatal("配置投影不应包含指令正文")
	}
	retry := envelope
	retry.IssuedAt = now.Add(time.Second)
	retry.Signature = ""
	retry, err = SignRuntimeConfigDeliveryEnvelope(retry, []byte("config-delivery-test-secret-32-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if retry.DeliveryID != envelope.DeliveryID || retry.Signature == envelope.Signature {
		t.Fatal("重试必须保持 delivery identity 并刷新签名")
	}
	if old == target {
		t.Fatal("fixture 必须产生不同配置")
	}
}

func parseRuntimeDeliverySnapshot(encoded string) (RuntimeConfigDeliveryProjection, error) {
	return agent.ParseRuntimeConfigSnapshot(encoded)
}

func TestMemoryRuntimeConfigDeliveryInboxIsMonotonicAndTransactional(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	old := migrationSnapshot(t, "config-a", "demo", "model")
	target := migrationSnapshot(t, "config-b", "demo", "model")
	next := migrationSnapshot(t, "config-c", "demo", "model")
	oldDigest := runtimeSnapshotDigestForTest(old)
	targetProjection := runtimeProjectionForTest(target)
	nextProjection := runtimeProjectionForTest(next)
	repo := NewMemoryRepository()
	envelope, projection := signedRuntimeConfigDeliveryEnvelope(t, "runtime-a", "runtime-b", "inv-config-inbox", oldDigest, "key-1", targetProjection, now)
	if duplicate, err := repo.AcceptRuntimeConfigDelivery(ctx, envelope, projection); err != nil || duplicate {
		t.Fatalf("首次 inbox 接收失败 duplicate=%v err=%v", duplicate, err)
	}
	retry := envelope
	retry.IssuedAt = now.Add(time.Second)
	retry.Signature = ""
	retry, _ = SignRuntimeConfigDeliveryEnvelope(retry, []byte("config-delivery-test-secret-32-bytes"))
	if duplicate, err := repo.AcceptRuntimeConfigDelivery(ctx, retry, projection); err != nil || !duplicate {
		t.Fatalf("刷新签名重试应返回 duplicate=%v err=%v", duplicate, err)
	}
	nextEnvelope, _ := signedRuntimeConfigDeliveryEnvelope(t, "runtime-a", "runtime-b", "inv-config-inbox", runtimeSnapshotDigestForTest(target), "key-2", nextProjection, now.Add(2*time.Second))
	if duplicate, err := repo.AcceptRuntimeConfigDelivery(ctx, nextEnvelope, nextProjection); err != nil || duplicate {
		t.Fatalf("单调新版本接收失败 duplicate=%v err=%v", duplicate, err)
	}
	if _, err := repo.AcceptRuntimeConfigDelivery(ctx, envelope, projection); !errors.Is(err, ErrRuntimeConfigDeliveryStale) {
		t.Fatalf("旧版本应被拒绝，实际=%v", err)
	}

	transactionEnvelope, transactionProjection := signedRuntimeConfigDeliveryEnvelope(t, "runtime-a", "runtime-b", "inv-config-txn", oldDigest, "txn-1", targetProjection, now)
	transactionRepo := any(repo).(RuntimeConfigDeliveryTransactionRepository)
	if duplicate, err := transactionRepo.PrepareRuntimeConfigDelivery(ctx, transactionEnvelope, transactionProjection); err != nil || duplicate {
		t.Fatalf("prepare 失败 duplicate=%v err=%v", duplicate, err)
	}
	if _, err := repo.GetRuntimeConfigDelivery(ctx, transactionEnvelope.InvocationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("prepare 阶段不应暴露 inbox: %v", err)
	}
	if duplicate, err := transactionRepo.PrepareRuntimeConfigDelivery(ctx, transactionEnvelope, transactionProjection); err != nil || !duplicate {
		t.Fatalf("重复 prepare 应幂等 duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := transactionRepo.CommitRuntimeConfigDelivery(ctx, transactionEnvelope); err != nil || duplicate {
		t.Fatalf("commit 失败 duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := transactionRepo.CommitRuntimeConfigDelivery(ctx, transactionEnvelope); err != nil || !duplicate {
		t.Fatalf("重复 commit 应幂等 duplicate=%v err=%v", duplicate, err)
	}
	if _, err := repo.GetRuntimeConfigDelivery(ctx, transactionEnvelope.InvocationID); err != nil {
		t.Fatalf("commit 后 inbox 缺失: %v", err)
	}
}

func TestRuntimeConfigDeliveryHTTPTransactionAbortIsIdempotent(t *testing.T) {
	ctx := context.Background()
	secret := []byte("config-http-delivery-secret-32-bytes")
	now := time.Now().UTC().Truncate(time.Microsecond)
	old := migrationSnapshot(t, "config-http-abort-old", "demo", "model")
	target := migrationSnapshot(t, "config-http-abort-target", "demo", "model")
	projection := runtimeProjectionForTest(target)
	repo := NewMemoryRepository()
	receiver, err := NewRuntimeConfigDeliveryReceiver("runtime-a", "runtime-b", secret, repo)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now }
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/config/prepare":
			receiver.PrepareHandler().ServeHTTP(writer, request)
		case "/config/abort":
			receiver.AbortHandler().ServeHTTP(writer, request)
		case "/config/commit":
			receiver.CommitHandler().ServeHTTP(writer, request)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	transport, err := NewRuntimeConfigDeliveryHTTPTransactionalTransport(server.URL+"/config", "runtime-a", "runtime-b", secret, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	envelope, _ := signedRuntimeConfigDeliveryEnvelope(t, "runtime-a", "runtime-b", "inv-config-http-abort", runtimeSnapshotDigestForTest(old), "abort-http-1", projection, now)
	if _, err := transport.Prepare(ctx, envelope, projection); err != nil {
		t.Fatalf("HTTP prepare 失败: %v", err)
	}
	aborted, err := transport.Abort(ctx, envelope, projection)
	if err != nil || aborted.Phase != RuntimeConfigDeliveryTransactionPhaseAborted || aborted.Duplicate {
		t.Fatalf("HTTP abort 失败: %#v err=%v", aborted, err)
	}
	aborted, err = transport.Abort(ctx, envelope, projection)
	if err != nil || !aborted.Duplicate {
		t.Fatalf("HTTP 重复 abort 必须幂等: %#v err=%v", aborted, err)
	}
	transaction, err := repo.GetRuntimeConfigDeliveryTransaction(ctx, envelope.DeliveryID)
	if err != nil || transaction.Status != RuntimeConfigDeliveryTransactionAborted {
		t.Fatalf("HTTP abort 状态错误: %#v err=%v", transaction, err)
	}
	if _, err := transport.Commit(ctx, envelope, projection); !strings.Contains(err.Error(), "HTTP") {
		t.Fatalf("aborted transaction commit 应失败: %v", err)
	}
}

func TestMemoryRuntimeConfigDeliveryAbortIsIdempotentAndNeverPublishes(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	old := migrationSnapshot(t, "config-abort-old", "demo", "model")
	target := migrationSnapshot(t, "config-abort-target", "demo", "model")
	projection := runtimeProjectionForTest(target)
	repo := NewMemoryRepository()
	envelope, projection := signedRuntimeConfigDeliveryEnvelope(t, "runtime-a", "runtime-b", "inv-config-abort", runtimeSnapshotDigestForTest(old), "abort-1", projection, now)
	txnRepo := any(repo).(RuntimeConfigDeliveryTransactionRepository)
	aborter := any(repo).(RuntimeConfigDeliveryAbortableTransactionRepository)
	if duplicate, err := txnRepo.PrepareRuntimeConfigDelivery(ctx, envelope, projection); err != nil || duplicate {
		t.Fatalf("prepare 失败: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := aborter.AbortRuntimeConfigDelivery(ctx, envelope); err != nil || duplicate {
		t.Fatalf("首次 abort 失败: duplicate=%v err=%v", duplicate, err)
	}
	transaction, err := repo.GetRuntimeConfigDeliveryTransaction(ctx, envelope.DeliveryID)
	if err != nil || transaction.Status != RuntimeConfigDeliveryTransactionAborted {
		t.Fatalf("abort 状态错误: %#v err=%v", transaction, err)
	}
	if _, err := repo.GetRuntimeConfigDelivery(ctx, envelope.InvocationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("abort 不得发布 config inbox: %v", err)
	}
	if duplicate, err := aborter.AbortRuntimeConfigDelivery(ctx, envelope); err != nil || !duplicate {
		t.Fatalf("重复 abort 必须幂等: duplicate=%v err=%v", duplicate, err)
	}
	if _, err := txnRepo.CommitRuntimeConfigDelivery(ctx, envelope); !errors.Is(err, ErrConflict) {
		t.Fatalf("aborted transaction 不得 commit: %v", err)
	}
}

func TestMemoryRuntimeConfigDeliveryOutboxLeaseRetryAndAtomicMigration(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	old := migrationSnapshot(t, "atomic-a", "demo", "model")
	target := migrationSnapshot(t, "atomic-b", "demo", "model")
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-config-atomic", UserID: "user", ConversationID: "conversation", SessionID: "conversation", ConfigSnapshot: old, Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	oldDigest := runtimeSnapshotDigestForTest(old)
	targetDigest := runtimeSnapshotDigestForTest(target)
	commit := RuntimeConfigMigrationCommit{InvocationID: invocation.ID, ExpectedDigest: oldDigest, NewSnapshot: target, IdempotencyKey: "atomic-1", Event: AgentEvent{ID: RuntimeConfigMigrationEventID(invocation.ID, oldDigest, targetDigest, "atomic-1"), Type: EventRuntimeConfigMigrated, Timestamp: now}}
	committed, event, outbox, err := repo.CommitRuntimeConfigMigrationWithDelivery(ctx, commit, "runtime-a", "runtime-b")
	if err != nil {
		t.Fatal(err)
	}
	if committed.ConfigSnapshotDigest != targetDigest || event.Type != EventRuntimeConfigMigrated || outbox.Status != RuntimeConfigDeliveryOutboxQueued {
		t.Fatalf("原子迁移结果错误: invocation=%#v event=%#v outbox=%#v", committed, event, outbox)
	}
	repeated, repeatedEvent, repeatedOutbox, err := repo.CommitRuntimeConfigMigrationWithDelivery(ctx, commit, "runtime-a", "runtime-b")
	if err != nil || repeated.ConfigSnapshotDigest != targetDigest || repeatedEvent.ID != event.ID || repeatedOutbox.ID != outbox.ID {
		t.Fatalf("原子迁移重复请求不幂等: invocation=%#v event=%#v outbox=%#v err=%v", repeated, repeatedEvent, repeatedOutbox, err)
	}
	outboxRepo := any(repo).(RuntimeConfigDeliveryOutboxRepository)
	claimAt := now.Add(time.Second)
	claimed, ok, err := outboxRepo.ClaimRuntimeConfigDeliveryOutbox(ctx, "config-worker", claimAt, time.Minute)
	if err != nil || !ok || claimed.Attempt != 1 || claimed.Status != RuntimeConfigDeliveryOutboxProcessing {
		t.Fatalf("outbox claim 错误: %#v ok=%v err=%v", claimed, ok, err)
	}
	if done, err := outboxRepo.CompleteRuntimeConfigDeliveryOutbox(ctx, claimed.ID, "wrong-worker", claimAt); err != nil || done {
		t.Fatalf("错误 owner 不应完成 outbox: done=%v err=%v", done, err)
	}
	if retried, err := outboxRepo.RetryRuntimeConfigDeliveryOutbox(ctx, claimed.ID, claimed.LeaseOwner, claimAt, "api_key=secret-value"); err != nil || !retried {
		t.Fatalf("outbox retry 失败: retried=%v err=%v", retried, err)
	}
	queued, err := outboxRepo.GetRuntimeConfigDeliveryOutbox(ctx, claimed.ID)
	if err != nil || queued.Status != RuntimeConfigDeliveryOutboxQueued || strings.Contains(queued.LastError, "secret-value") || !strings.Contains(queued.LastError, "[REDACTED]") {
		t.Fatalf("outbox retry 未正确脱敏: %#v err=%v", queued, err)
	}
}

type configDeliveryTransportFunc func(context.Context, RuntimeConfigDeliveryEnvelope, RuntimeConfigDeliveryProjection) (RuntimeConfigDeliveryReceipt, error)

func (f configDeliveryTransportFunc) Deliver(ctx context.Context, envelope RuntimeConfigDeliveryEnvelope, projection RuntimeConfigDeliveryProjection) (RuntimeConfigDeliveryReceipt, error) {
	return f(ctx, envelope, projection)
}

func TestCoordinatorDispatchesRuntimeConfigDeliveryOutbox(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	old := migrationSnapshot(t, "dispatch-old", "demo", "model")
	target := migrationSnapshot(t, "dispatch-target", "demo", "model")
	oldDigest := runtimeSnapshotDigestForTest(old)
	targetDigest := runtimeSnapshotDigestForTest(target)
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-config-dispatch", UserID: "user", ConversationID: "conversation", SessionID: "conversation", ConfigSnapshot: old, Status: InvocationWaitingUser, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	_, _, outbox, err := repo.CommitRuntimeConfigMigrationWithDelivery(ctx, RuntimeConfigMigrationCommit{
		InvocationID: invocation.ID, ExpectedDigest: oldDigest, NewSnapshot: target, IdempotencyKey: "dispatch-1",
		Event: AgentEvent{ID: RuntimeConfigMigrationEventID(invocation.ID, oldDigest, targetDigest, "dispatch-1"), Type: EventRuntimeConfigMigrated, Timestamp: now},
	}, "runtime-a", "runtime-b")
	if err != nil {
		t.Fatal(err)
	}
	var gotEnvelope RuntimeConfigDeliveryEnvelope
	coordinator := &Coordinator{repo: repo, configDeliverySource: "runtime-a", configDeliveryDestination: "runtime-b", configOutboxWorkerID: "config-dispatch-worker", configDeliveryTransport: configDeliveryTransportFunc(func(_ context.Context, envelope RuntimeConfigDeliveryEnvelope, projection RuntimeConfigDeliveryProjection) (RuntimeConfigDeliveryReceipt, error) {
		gotEnvelope = envelope
		if projection.Version != outbox.SnapshotVersion {
			return RuntimeConfigDeliveryReceipt{}, ErrConflict
		}
		return RuntimeConfigDeliveryReceipt{Version: envelope.Version, DeliveryID: envelope.DeliveryID, InvocationID: envelope.InvocationID, ExpectedDigest: envelope.ExpectedDigest, SnapshotDigest: envelope.SnapshotDigest, SnapshotVersion: envelope.SnapshotVersion}, nil
	})}
	coordinator.DispatchDueRuntimeDeliveries(ctx, now.Add(time.Second))
	completed, err := repo.GetRuntimeConfigDeliveryOutbox(ctx, outbox.ID)
	if err != nil || completed.Status != RuntimeConfigDeliveryOutboxCompleted {
		t.Fatalf("配置 outbox 调度未完成: %#v err=%v", completed, err)
	}
	if gotEnvelope.DeliveryID != outbox.DeliveryID || gotEnvelope.IssuedAt.IsZero() || gotEnvelope.Signature != "" {
		t.Fatalf("调度器应把未签名 identity 交给 transport: %#v", gotEnvelope)
	}
}

func runtimeSnapshotDigestForTest(encoded string) string {
	return agent.RuntimeConfigSnapshotDigest(encoded)
}

func runtimeProjectionForTest(encoded string) RuntimeConfigDeliveryProjection {
	projection, err := agent.ParseRuntimeConfigSnapshot(encoded)
	if err != nil {
		panic(err)
	}
	return projection
}
