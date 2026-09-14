package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func rejectionDeliveryFixture(t *testing.T, status ApprovalStatus) (*MemoryRepository, Approval, Invocation) {
	t.Helper()
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Date(2026, 9, 13, 21, 0, 0, 0, time.UTC)
	invocation := Invocation{ID: "rejection-invocation", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingApproval, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	approval := Approval{ID: "rejection-approval", InvocationID: invocation.ID, ConversationID: invocation.ConversationID, ToolCallID: "rejection-tool-call", OperationID: "rejection-operation", ToolName: "guarded_write", OriginalCallID: "original-call", ConfirmationCallID: "confirmation-call", Status: status, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateToolCall(ctx, ToolCall{ID: approval.ToolCallID, InvocationID: invocation.ID, ToolName: approval.ToolName, OriginalCallID: approval.OriginalCallID, OperationID: approval.OperationID, ApprovalID: approval.ID, Status: ToolCallWaitingApproval, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return repo, approval, invocation
}

func rejectionResumeCommit(t *testing.T, repo *MemoryRepository, approval Approval, invocation Invocation, reason string, delivery *RuntimeApprovalRejectionDeliveryOutbox) ApprovalResumeCommit {
	t.Helper()
	response := map[string]any{"confirmed": false, "payload": map[string]any{"operation_id": approval.OperationID, "invocation_id": invocation.ID, "tool_call_id": approval.OriginalCallID}}
	fingerprint, encoded, err := invocationResumeFingerprint(InvocationResumeRequest{WaitID: approval.ConfirmationCallID, Name: "adk_tool_confirmation", Response: response})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	resume := InvocationResume{ID: "rejection-resume", InvocationID: invocation.ID, WaitID: approval.ConfirmationCallID, Name: "adk_tool_confirmation", ResponseJSON: encoded, RequestDigest: fingerprint, CreatedAt: now}
	outbox := InvocationResumeOutbox{ID: "rejection-resume-outbox", InvocationID: invocation.ID, WaitID: resume.WaitID, Name: resume.Name, ResponseJSON: encoded, RequestDigest: fingerprint, Status: InvocationResumeOutboxQueued, AvailableAt: now, CreatedAt: now, UpdatedAt: now}
	commit := ApprovalResumeCommit{ApprovalID: approval.ID, ApprovalFromStatus: ApprovalPending, ApprovalToStatus: ApprovalRejected, InvocationID: invocation.ID, InvocationFromStatus: invocation.Status, InvocationToStatus: InvocationQueued, ToolCallID: approval.ToolCallID, ToolCallStatus: ToolCallRejected, Reason: reason, Resume: resume, Outbox: outbox, RejectionDelivery: delivery, Events: []AgentEvent{{ID: "rejection-resolved-event", InvocationID: invocation.ID, Type: EventApprovalResolved, Timestamp: now, Data: map[string]any{"approval_id": approval.ID, "confirmed": false}}}}
	return commit
}

func TestRuntimeApprovalRejectionDeliveryEnvelopeIsSignedAndReasonFree(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	_, approval, invocation := rejectionDeliveryFixture(t, ApprovalRejected)
	now := time.Date(2026, 9, 13, 21, 1, 0, 0, time.UTC)
	outbox, err := NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", approval, invocation, "api_key=secret-value 用户拒绝", now)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := outbox.Envelope(now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	signed, err := SignRuntimeApprovalRejectionDeliveryEnvelope(envelope, secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRuntimeApprovalRejectionDeliveryEnvelope(signed, secret); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret-value") || strings.Contains(string(encoded), "用户拒绝") {
		t.Fatalf("拒绝 envelope 不应携带 reason 原文: %s", encoded)
	}
	tampered := signed
	tampered.OperationID = "other-operation"
	if err := VerifyRuntimeApprovalRejectionDeliveryEnvelope(tampered, secret); !errors.Is(err, ErrRuntimeApprovalRejectionDeliveryAuth) {
		t.Fatalf("篡改 metadata 必须拒绝: %v", err)
	}
	stale := signed
	if err := validateRuntimeApprovalRejectionDeliveryTimestamp(stale, now.Add(11*time.Minute), time.Minute, 0); !errors.Is(err, ErrRuntimeApprovalRejectionDeliveryStale) {
		t.Fatalf("过期 issued_at 必须拒绝: %v", err)
	}
}

func TestMemoryRuntimeApprovalRejectionDeliveryOutboxIsLeasedRetriedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	repo, approval, invocation := rejectionDeliveryFixture(t, ApprovalRejected)
	now := time.Date(2026, 9, 13, 21, 2, 0, 0, time.UTC)
	outbox, err := NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", approval, invocation, "用户拒绝", now)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := repo.EnqueueRuntimeApprovalRejectionDeliveryOutbox(ctx, outbox)
	if err != nil || !stored.Matches(outbox) {
		t.Fatalf("enqueue 失败: %#v err=%v", stored, err)
	}
	if _, err := repo.EnqueueRuntimeApprovalRejectionDeliveryOutbox(ctx, outbox); err != nil {
		t.Fatalf("重复 enqueue 应幂等: %v", err)
	}
	forged := outbox
	forged.OperationID = "forged"
	if _, err := repo.EnqueueRuntimeApprovalRejectionDeliveryOutbox(ctx, forged); !errors.Is(err, ErrRuntimeApprovalRejectionDeliveryAuth) {
		t.Fatalf("伪造 identity 必须拒绝: %v", err)
	}
	claimed, ok, err := repo.ClaimRuntimeApprovalRejectionDeliveryOutbox(ctx, "rejection-worker", now, time.Minute)
	if err != nil || !ok || claimed.Attempt != 1 || claimed.Status != RuntimeApprovalRejectionDeliveryOutboxProcessing {
		t.Fatalf("claim 错误: %#v ok=%v err=%v", claimed, ok, err)
	}
	if done, err := repo.CompleteRuntimeApprovalRejectionDeliveryOutbox(ctx, claimed.ID, "other-worker", now); err != nil || done {
		t.Fatalf("错误 worker 不应 complete: done=%v err=%v", done, err)
	}
	if retried, err := repo.RetryRuntimeApprovalRejectionDeliveryOutbox(ctx, claimed.ID, claimed.LeaseOwner, now, "authorization=secret-value"); err != nil || !retried {
		t.Fatalf("retry 失败: %v %v", retried, err)
	}
	queued, err := repo.GetRuntimeApprovalRejectionDeliveryOutbox(ctx, claimed.ID)
	if err != nil || queued.Status != RuntimeApprovalRejectionDeliveryOutboxQueued || strings.Contains(queued.LastError, "secret-value") || !strings.Contains(queued.LastError, "[REDACTED]") {
		t.Fatalf("retry 脱敏/状态错误: %#v err=%v", queued, err)
	}
	claimed, ok, err = repo.ClaimRuntimeApprovalRejectionDeliveryOutbox(ctx, "rejection-worker-2", queued.AvailableAt.Add(time.Nanosecond), time.Minute)
	if err != nil || !ok {
		t.Fatalf("退避后应可再次 claim: %#v ok=%v err=%v", claimed, ok, err)
	}
	if done, err := repo.CompleteRuntimeApprovalRejectionDeliveryOutbox(ctx, claimed.ID, claimed.LeaseOwner, claimed.LeaseExpiresAt.Add(-time.Nanosecond)); err != nil || !done {
		t.Fatalf("complete 失败: %v %v", done, err)
	}
}

func TestMemoryRuntimeApprovalRejectionDeliveryAbortIsIdempotentAndNeverPublishes(t *testing.T) {
	ctx := context.Background()
	repo, approval, invocation := rejectionDeliveryFixture(t, ApprovalRejected)
	now := time.Now().UTC().Truncate(time.Millisecond)
	outbox, err := NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", approval, invocation, "用户拒绝", now)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := outbox.Envelope(now)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err = SignRuntimeApprovalRejectionDeliveryEnvelope(envelope, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	txnRepo := any(repo).(RuntimeApprovalRejectionDeliveryTransactionRepository)
	aborter := any(repo).(RuntimeApprovalRejectionDeliveryAbortableTransactionRepository)
	if duplicate, err := txnRepo.PrepareRuntimeApprovalRejectionDelivery(ctx, envelope); err != nil || duplicate {
		t.Fatalf("prepare 失败: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := aborter.AbortRuntimeApprovalRejectionDelivery(ctx, envelope); err != nil || duplicate {
		t.Fatalf("首次 abort 失败: duplicate=%v err=%v", duplicate, err)
	}
	transaction, err := repo.GetRuntimeApprovalRejectionDeliveryTransaction(ctx, envelope.DeliveryID)
	if err != nil || transaction.Status != RuntimeApprovalRejectionDeliveryTransactionAborted {
		t.Fatalf("abort 状态错误: %#v err=%v", transaction, err)
	}
	if _, err := repo.GetRuntimeApprovalRejectionDelivery(ctx, envelope.DeliveryID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("abort 不得发布 rejection inbox: %v", err)
	}
	if duplicate, err := aborter.AbortRuntimeApprovalRejectionDelivery(ctx, envelope); err != nil || !duplicate {
		t.Fatalf("重复 abort 必须幂等: duplicate=%v err=%v", duplicate, err)
	}
	if _, err := txnRepo.CommitRuntimeApprovalRejectionDelivery(ctx, envelope); !errors.Is(err, ErrConflict) {
		t.Fatalf("aborted transaction 不得 commit: %v", err)
	}
}

func TestMemoryRuntimeApprovalRejectionDeliveryAtomicCommitPersistsOutbox(t *testing.T) {
	ctx := context.Background()
	repo, approval, invocation := rejectionDeliveryFixture(t, ApprovalPending)
	delivery, err := NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", approval, invocation, "用户拒绝", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	commit := rejectionResumeCommit(t, repo, approval, invocation, "用户拒绝", &delivery)
	resolved, queuedInvocation, _, err := repo.CommitApprovalRejectionWithDelivery(ctx, commit, delivery)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != ApprovalRejected || queuedInvocation.Status != InvocationQueued {
		t.Fatalf("本地拒绝状态错误: approval=%s invocation=%s", resolved.Status, queuedInvocation.Status)
	}
	stored, err := repo.GetRuntimeApprovalRejectionDeliveryOutbox(ctx, delivery.ID)
	if err != nil || stored.Status != RuntimeApprovalRejectionDeliveryOutboxQueued || !stored.Matches(delivery) {
		t.Fatalf("原子提交未持久化 rejection outbox: %#v err=%v", stored, err)
	}
	if stored.GroupID == "" {
		t.Fatal("拒绝 delivery 必须绑定 source-side delivery group")
	}
	groups, err := repo.ListRuntimeDeliveryGroups(ctx, invocation.ID, RuntimeDeliveryGroupQueued, 10)
	if err != nil || len(groups) != 1 || len(groups[0].Members) != 2 {
		t.Fatalf("拒绝提交应将 event/rejection 绑定到同一 group: groups=%#v err=%v", groups, err)
	}
	if groups[0].ID != stored.GroupID {
		t.Fatalf("rejection outbox 与 group ID 不一致: outbox=%q group=%q", stored.GroupID, groups[0].ID)
	}
}

func TestMemoryRuntimeApprovalRejectionDeliveryRejectsStaleApprovalMetadataAtomically(t *testing.T) {
	ctx := context.Background()
	repo, approval, invocation := rejectionDeliveryFixture(t, ApprovalPending)
	delivery, err := NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", approval, invocation, "用户拒绝", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	forged := delivery
	forged.OperationID = "forged-operation"
	forged.DeliveryID = RuntimeApprovalRejectionDeliveryID(forged.Source, forged.Destination, forged.ApprovalID, forged.InvocationID, forged.ToolCallID, forged.OperationID, forged.ReasonDigest)
	forged.ID = RuntimeApprovalRejectionDeliveryOutboxID(forged.Source, forged.Destination, forged.DeliveryID)
	commit := rejectionResumeCommit(t, repo, approval, invocation, "用户拒绝", &forged)
	if _, _, _, err := repo.CommitApprovalRejectionWithDelivery(ctx, commit, forged); !errors.Is(err, ErrConflict) {
		t.Fatalf("approval metadata 变化必须在提交前拒绝: %v", err)
	}
	currentApproval, err := repo.GetApproval(ctx, approval.ID)
	if err != nil || currentApproval.Status != ApprovalPending {
		t.Fatalf("失败的拒绝提交不应改变 Approval: %#v err=%v", currentApproval, err)
	}
	currentInvocation, err := repo.GetInvocation(ctx, invocation.ID)
	if err != nil || currentInvocation.Status != InvocationWaitingApproval {
		t.Fatalf("失败的拒绝提交不应改变 Invocation: %#v err=%v", currentInvocation, err)
	}
	if _, err := repo.GetRuntimeApprovalRejectionDeliveryOutbox(ctx, delivery.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("失败的拒绝提交不应写入 outbox: %v", err)
	}
}

func TestMemoryRuntimeApprovalRejectionDeliveryTransactionIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repo, approval, invocation := rejectionDeliveryFixture(t, ApprovalRejected)
	now := time.Now().UTC()
	outbox, err := NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", approval, invocation, "用户拒绝", now)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := outbox.Envelope(now)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := repo.PrepareRuntimeApprovalRejectionDelivery(ctx, envelope)
	if err != nil || prepared {
		t.Fatalf("首次 prepare 错误: duplicate=%v err=%v", prepared, err)
	}
	prepared, err = repo.PrepareRuntimeApprovalRejectionDelivery(ctx, envelope)
	if err != nil || !prepared {
		t.Fatalf("重复 prepare 应幂等: duplicate=%v err=%v", prepared, err)
	}
	committed, err := repo.CommitRuntimeApprovalRejectionDelivery(ctx, envelope)
	if err != nil || committed {
		t.Fatalf("首次 commit 错误: duplicate=%v err=%v", committed, err)
	}
	committed, err = repo.CommitRuntimeApprovalRejectionDelivery(ctx, envelope)
	if err != nil || !committed {
		t.Fatalf("重复 commit 应幂等: duplicate=%v err=%v", committed, err)
	}
	if _, err := repo.GetRuntimeApprovalRejectionDelivery(ctx, envelope.DeliveryID); err != nil {
		t.Fatalf("commit 应写入 inbox: %v", err)
	}
	conflict := envelope
	conflict.OperationID = "different"
	if _, err := repo.PrepareRuntimeApprovalRejectionDelivery(ctx, conflict); !errors.Is(err, ErrRuntimeApprovalRejectionDeliveryAuth) && !errors.Is(err, ErrConflict) {
		t.Fatalf("冲突 intent 必须拒绝: %v", err)
	}
}

type rejectionDeliveryTransactionalTransportFunc struct {
	prepare func(context.Context, RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeApprovalRejectionDeliveryReceipt, error)
	commit  func(context.Context, RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeApprovalRejectionDeliveryReceipt, error)
	deliver func(context.Context, RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeApprovalRejectionDeliveryReceipt, error)
}

func (f rejectionDeliveryTransactionalTransportFunc) Deliver(ctx context.Context, envelope RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeApprovalRejectionDeliveryReceipt, error) {
	if f.deliver == nil {
		return RuntimeApprovalRejectionDeliveryReceipt{}, errors.New("one-phase path should not be called")
	}
	return f.deliver(ctx, envelope)
}

func (f rejectionDeliveryTransactionalTransportFunc) Prepare(ctx context.Context, envelope RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeApprovalRejectionDeliveryReceipt, error) {
	return f.prepare(ctx, envelope)
}

func (f rejectionDeliveryTransactionalTransportFunc) Commit(ctx context.Context, envelope RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeApprovalRejectionDeliveryReceipt, error) {
	return f.commit(ctx, envelope)
}

func TestCoordinatorDispatchesRuntimeApprovalRejectionOutboxWithPrepareCommit(t *testing.T) {
	ctx := context.Background()
	repo, approval, invocation := rejectionDeliveryFixture(t, ApprovalRejected)
	now := time.Now().UTC()
	outbox, err := NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", approval, invocation, "用户拒绝", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeApprovalRejectionDeliveryOutbox(ctx, outbox); err != nil {
		t.Fatal(err)
	}
	var prepares, commits atomic.Int32
	coordinator := &Coordinator{repo: repo, rejectionOutboxWorkerID: "rejection-dispatch-worker", rejectionDeliverySource: "runtime-a", rejectionDeliveryDestination: "runtime-b"}
	coordinator.rejectionDeliveryTransport = rejectionDeliveryTransactionalTransportFunc{
		prepare: func(_ context.Context, envelope RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeApprovalRejectionDeliveryReceipt, error) {
			prepares.Add(1)
			return RuntimeApprovalRejectionDeliveryReceipt{Version: envelope.Version, DeliveryID: envelope.DeliveryID, ApprovalID: envelope.ApprovalID, InvocationID: envelope.InvocationID, ToolCallID: envelope.ToolCallID, OperationID: envelope.OperationID, Decision: envelope.Decision, ReasonDigest: envelope.ReasonDigest, Phase: RuntimeApprovalRejectionDeliveryTransactionPhasePrepared}, nil
		},
		commit: func(_ context.Context, envelope RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeApprovalRejectionDeliveryReceipt, error) {
			commits.Add(1)
			return RuntimeApprovalRejectionDeliveryReceipt{Version: envelope.Version, DeliveryID: envelope.DeliveryID, ApprovalID: envelope.ApprovalID, InvocationID: envelope.InvocationID, ToolCallID: envelope.ToolCallID, OperationID: envelope.OperationID, Decision: envelope.Decision, ReasonDigest: envelope.ReasonDigest, Phase: RuntimeApprovalRejectionDeliveryTransactionPhaseCommitted}, nil
		},
	}
	coordinator.dispatchDueRuntimeApprovalRejectionOutbox(ctx, now)
	if prepares.Load() != 1 || commits.Load() != 1 {
		t.Fatalf("prepare/commit 次数错误: %d/%d", prepares.Load(), commits.Load())
	}
	stored, err := repo.GetRuntimeApprovalRejectionDeliveryOutbox(ctx, outbox.ID)
	if err != nil || stored.Status != RuntimeApprovalRejectionDeliveryOutboxCompleted {
		t.Fatalf("dispatch 未完成 outbox: %#v err=%v", stored, err)
	}
}

func TestRuntimeApprovalRejectionDeliveryHTTPReceiverRejectsReasonBodyAndSupports2PC(t *testing.T) {
	ctx := context.Background()
	secret := []byte("0123456789abcdef0123456789abcdef")
	repo, approval, invocation := rejectionDeliveryFixture(t, ApprovalRejected)
	now := time.Now().UTC()
	outbox, err := NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", approval, invocation, "用户拒绝", now)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := outbox.Envelope(now)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := SignRuntimeApprovalRejectionDeliveryEnvelope(envelope, secret)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewRuntimeApprovalRejectionDeliveryReceiver("runtime-a", "runtime-b", secret, repo)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now }
	body, _ := json.Marshal(signed)
	request := httptest.NewRequest(http.MethodPost, "/rejection", bytes.NewReader(body))
	request.Header.Set("X-Abot-Approval-Rejection-Delivery-Version", "1")
	request.Header.Set("X-Abot-Approval-Rejection-Delivery-Signature", signed.Signature)
	request.Header.Set("Idempotency-Key", signed.DeliveryID)
	response := httptest.NewRecorder()
	receiver.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "用户拒绝") {
		t.Fatalf("one-phase receiver 错误: status=%d body=%s", response.Code, response.Body.String())
	}
	transactionBody, _ := json.Marshal(runtimeApprovalRejectionDeliveryEnvelopePayload{Envelope: signed})
	badHeaderRequest := httptest.NewRequest(http.MethodPost, "/rejection/prepare", bytes.NewReader(transactionBody))
	badHeaderRequest.Header.Set("X-Abot-Approval-Rejection-Transaction-Phase", RuntimeApprovalRejectionDeliveryTransactionPhasePrepared)
	badHeaderRequest.Header.Set("Idempotency-Key", "wrong-delivery")
	badHeaderResponse := httptest.NewRecorder()
	receiver.PrepareHandler().ServeHTTP(badHeaderResponse, badHeaderRequest)
	if badHeaderResponse.Code != http.StatusBadRequest {
		t.Fatalf("transaction idempotency header 不一致必须拒绝: status=%d body=%s", badHeaderResponse.Code, badHeaderResponse.Body.String())
	}
	prepareRequest := httptest.NewRequest(http.MethodPost, "/rejection/prepare", bytes.NewReader(transactionBody))
	prepareRequest.Header.Set("X-Abot-Approval-Rejection-Transaction-Phase", RuntimeApprovalRejectionDeliveryTransactionPhasePrepared)
	prepareResponse := httptest.NewRecorder()
	receiver.PrepareHandler().ServeHTTP(prepareResponse, prepareRequest)
	if prepareResponse.Code != http.StatusOK {
		t.Fatalf("prepare endpoint 错误: status=%d body=%s", prepareResponse.Code, prepareResponse.Body.String())
	}
	commitRequest := httptest.NewRequest(http.MethodPost, "/rejection/commit", bytes.NewReader(transactionBody))
	commitRequest.Header.Set("X-Abot-Approval-Rejection-Transaction-Phase", RuntimeApprovalRejectionDeliveryTransactionPhaseCommitted)
	commitResponse := httptest.NewRecorder()
	receiver.CommitHandler().ServeHTTP(commitResponse, commitRequest)
	if commitResponse.Code != http.StatusOK {
		t.Fatalf("commit endpoint 错误: status=%d body=%s", commitResponse.Code, commitResponse.Body.String())
	}
	if _, err := repo.GetRuntimeApprovalRejectionDelivery(ctx, signed.DeliveryID); err != nil {
		t.Fatalf("2PC commit 应保留 inbox: %v", err)
	}
}

func TestRuntimeApprovalRejectionDeliveryWorkspaceApplyRequiresExplicitCapability(t *testing.T) {
	repo, _, _ := rejectionDeliveryFixture(t, ApprovalRejected)
	receiver, err := NewRuntimeApprovalRejectionDeliveryReceiver("runtime-a", "runtime-b", []byte("0123456789abcdef0123456789abcdef"), repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := receiver.SetWorkspaceCancellationEnabled(true); !errors.Is(err, ErrInvalidRuntimeApprovalRejectionDelivery) {
		t.Fatalf("不具备 Workspace 原子应用能力的 receiver 必须拒绝启用: %v", err)
	}
}

func TestRuntimeApprovalRejectionDeliveryHTTPTransportRunsPrepareCommit(t *testing.T) {
	ctx := context.Background()
	secret := []byte("0123456789abcdef0123456789abcdef")
	repo, approval, invocation := rejectionDeliveryFixture(t, ApprovalRejected)
	now := time.Now().UTC()
	outbox, err := NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", approval, invocation, "用户拒绝", now)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewRuntimeApprovalRejectionDeliveryReceiver("runtime-a", "runtime-b", secret, repo)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return time.Now().UTC() }
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/prepare":
			receiver.PrepareHandler().ServeHTTP(writer, request)
		case "/commit":
			receiver.CommitHandler().ServeHTTP(writer, request)
		default:
			receiver.ServeHTTP(writer, request)
		}
	}))
	defer server.Close()
	transport, err := NewRuntimeApprovalRejectionDeliveryHTTPTransactionalTransport(server.URL, "runtime-a", "runtime-b", secret, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	transport.Now = func() time.Time { return time.Now().UTC() }
	envelope, err := outbox.Envelope(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := transport.Prepare(ctx, envelope)
	if err != nil || prepared.Phase != RuntimeApprovalRejectionDeliveryTransactionPhasePrepared {
		t.Fatalf("HTTP prepare 错误: %#v err=%v", prepared, err)
	}
	committed, err := transport.Commit(ctx, envelope)
	if err != nil || committed.Phase != RuntimeApprovalRejectionDeliveryTransactionPhaseCommitted {
		t.Fatalf("HTTP commit 错误: %#v err=%v", committed, err)
	}
	if _, err := repo.GetRuntimeApprovalRejectionDelivery(ctx, envelope.DeliveryID); err != nil {
		t.Fatalf("HTTP commit 未写入 destination inbox: %v", err)
	}
}

func TestRuntimeApprovalRejectionDeliveryHTTPTransportRunsAbort(t *testing.T) {
	ctx := context.Background()
	secret := []byte("0123456789abcdef0123456789abcdef")
	repo, approval, invocation := rejectionDeliveryFixture(t, ApprovalRejected)
	now := time.Now().UTC().Truncate(time.Millisecond)
	outbox, err := NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", approval, invocation, "用户拒绝", now)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewRuntimeApprovalRejectionDeliveryReceiver("runtime-a", "runtime-b", secret, repo)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now }
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/prepare":
			receiver.PrepareHandler().ServeHTTP(writer, request)
		case "/abort":
			receiver.AbortHandler().ServeHTTP(writer, request)
		case "/commit":
			receiver.CommitHandler().ServeHTTP(writer, request)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	transport, err := NewRuntimeApprovalRejectionDeliveryHTTPTransactionalTransport(server.URL, "runtime-a", "runtime-b", secret, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := outbox.Envelope(now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.Prepare(ctx, envelope); err != nil {
		t.Fatalf("HTTP prepare 失败: %v", err)
	}
	aborted, err := transport.Abort(ctx, envelope)
	if err != nil || aborted.Phase != RuntimeApprovalRejectionDeliveryTransactionPhaseAborted || aborted.Duplicate {
		t.Fatalf("HTTP abort 失败: %#v err=%v", aborted, err)
	}
	aborted, err = transport.Abort(ctx, envelope)
	if err != nil || !aborted.Duplicate {
		t.Fatalf("HTTP 重复 abort 必须幂等: %#v err=%v", aborted, err)
	}
	transaction, err := repo.GetRuntimeApprovalRejectionDeliveryTransaction(ctx, envelope.DeliveryID)
	if err != nil || transaction.Status != RuntimeApprovalRejectionDeliveryTransactionAborted {
		t.Fatalf("HTTP abort 状态错误: %#v err=%v", transaction, err)
	}
	if _, err := transport.Commit(ctx, envelope); err == nil {
		t.Fatal("aborted transaction 不得 commit")
	}
}
