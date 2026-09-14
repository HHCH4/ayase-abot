package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/workspace"
)

func TestSQLiteRuntimeApprovalRejectionDeliveryOutboxPersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	now := time.Date(2026, 9, 13, 22, 0, 0, 0, time.UTC)
	invocation := agentruntime.Invocation{ID: "sqlite-rejection-invocation", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingApproval, CreatedAt: now, UpdatedAt: now}
	approval := agentruntime.Approval{ID: "sqlite-rejection-approval", InvocationID: invocation.ID, ConversationID: invocation.ConversationID, ToolCallID: "sqlite-rejection-tool", OperationID: "sqlite-rejection-operation", ToolName: "guarded_write", OriginalCallID: "original", ConfirmationCallID: "confirmation", Status: agentruntime.ApprovalRejected, CreatedAt: now, UpdatedAt: now}
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	outbox, err := agentruntime.NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", approval, invocation, "用户拒绝", now)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := repo.(agentruntime.RuntimeApprovalRejectionDeliveryOutboxRepository).EnqueueRuntimeApprovalRejectionDeliveryOutbox(ctx, outbox)
	if err != nil || !stored.Matches(outbox) {
		t.Fatalf("SQLite rejection outbox enqueue 错误: %#v err=%v", stored, err)
	}
	claimed, ok, err := repo.(agentruntime.RuntimeApprovalRejectionDeliveryOutboxRepository).ClaimRuntimeApprovalRejectionDeliveryOutbox(ctx, "sqlite-rejection-worker", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("SQLite rejection outbox claim 错误: %#v ok=%v err=%v", claimed, ok, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo = store.RuntimeRepository()
	loaded, err := repo.(agentruntime.RuntimeApprovalRejectionDeliveryOutboxRepository).GetRuntimeApprovalRejectionDeliveryOutbox(ctx, outbox.ID)
	if err != nil || loaded.Status != agentruntime.RuntimeApprovalRejectionDeliveryOutboxProcessing || loaded.Attempt != 1 || loaded.LeaseOwner != claimed.LeaseOwner {
		t.Fatalf("重启后 rejection outbox 租约未持久化: %#v err=%v", loaded, err)
	}
	if done, err := repo.(agentruntime.RuntimeApprovalRejectionDeliveryOutboxRepository).CompleteRuntimeApprovalRejectionDeliveryOutbox(ctx, outbox.ID, claimed.LeaseOwner, claimed.LeaseExpiresAt.Add(-time.Nanosecond)); err != nil || !done {
		t.Fatalf("SQLite rejection outbox complete 错误: done=%v err=%v", done, err)
	}
}

func TestSQLiteRuntimeApprovalRejectionDeliveryTransactionIsDurableAndConflictSafe(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo := store.RuntimeRepository().(agentruntime.RuntimeApprovalRejectionDeliveryInbox)
	transactionRepo := store.RuntimeRepository().(agentruntime.RuntimeApprovalRejectionDeliveryTransactionRepository)
	now := time.Now().UTC()
	invocation := agentruntime.Invocation{ID: "sqlite-rejection-inbox-invocation", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingApproval, CreatedAt: now, UpdatedAt: now}
	approval := agentruntime.Approval{ID: "sqlite-rejection-inbox-approval", InvocationID: invocation.ID, ToolCallID: "tool", OperationID: "operation", Status: agentruntime.ApprovalRejected, CreatedAt: now, UpdatedAt: now}
	if err := store.RuntimeRepository().CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	if err := store.RuntimeRepository().CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	outbox, err := agentruntime.NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", approval, invocation, "用户拒绝", now)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := outbox.Envelope(now)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("0123456789abcdef0123456789abcdef")
	envelope, err = agentruntime.SignRuntimeApprovalRejectionDeliveryEnvelope(envelope, secret)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := transactionRepo.PrepareRuntimeApprovalRejectionDelivery(ctx, envelope); err != nil || duplicate {
		t.Fatalf("首次 prepare 错误: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := transactionRepo.CommitRuntimeApprovalRejectionDelivery(ctx, envelope); err != nil || duplicate {
		t.Fatalf("首次 commit 错误: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := repo.AcceptRuntimeApprovalRejectionDelivery(ctx, envelope); err != nil || !duplicate {
		t.Fatalf("commit 后重复 inbox 应幂等: duplicate=%v err=%v", duplicate, err)
	}
	if _, err := repo.GetRuntimeApprovalRejectionDelivery(ctx, envelope.DeliveryID); err != nil {
		t.Fatalf("SQLite inbox 未持久化: %v", err)
	}
	conflict := envelope
	conflict.OperationID = "different-operation"
	conflict, err = agentruntime.SignRuntimeApprovalRejectionDeliveryEnvelope(conflict, secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transactionRepo.PrepareRuntimeApprovalRejectionDelivery(ctx, conflict); !errors.Is(err, agentruntime.ErrConflict) {
		t.Fatalf("同 delivery_id 的不同 metadata 必须冲突: %v", err)
	}
	var row runtimeApprovalRejectionDeliveryInboxRow
	if err := store.db.Where("delivery_id = ?", envelope.DeliveryID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row.ReasonDigest, "用户拒绝") || row.ReasonDigest != envelope.ReasonDigest {
		t.Fatalf("SQLite inbox 不应存 reason 原文: %#v", row)
	}
}

func TestSQLiteRuntimeApprovalRejectionDeliveryAbortIsDurableAndNeverPublishes(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	invo := agentruntime.Invocation{ID: "sqlite-rejection-abort-invocation", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingApproval, CreatedAt: now, UpdatedAt: now}
	approval := agentruntime.Approval{ID: "sqlite-rejection-abort-approval", InvocationID: invo.ID, ToolCallID: "tool", OperationID: "operation", Status: agentruntime.ApprovalRejected, CreatedAt: now, UpdatedAt: now}
	if err := store.RuntimeRepository().CreateInvocation(ctx, invo); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.RuntimeRepository().CreateApproval(ctx, approval); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	outbox, err := agentruntime.NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", approval, invo, "用户拒绝", now)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	envelope, err := outbox.Envelope(now)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	envelope, err = agentruntime.SignRuntimeApprovalRejectionDeliveryEnvelope(envelope, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	txnRepo := repo.(agentruntime.RuntimeApprovalRejectionDeliveryTransactionRepository)
	aborter := repo.(agentruntime.RuntimeApprovalRejectionDeliveryAbortableTransactionRepository)
	if duplicate, err := txnRepo.PrepareRuntimeApprovalRejectionDelivery(ctx, envelope); err != nil || duplicate {
		_ = store.Close()
		t.Fatalf("prepare 失败: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := aborter.AbortRuntimeApprovalRejectionDelivery(ctx, envelope); err != nil || duplicate {
		_ = store.Close()
		t.Fatalf("首次 abort 失败: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := aborter.AbortRuntimeApprovalRejectionDelivery(ctx, envelope); err != nil || !duplicate {
		_ = store.Close()
		t.Fatalf("重复 abort 必须幂等: duplicate=%v err=%v", duplicate, err)
	}
	transaction, err := repo.(interface {
		GetRuntimeApprovalRejectionDeliveryTransaction(context.Context, string) (agentruntime.RuntimeApprovalRejectionDeliveryTransaction, error)
	}).GetRuntimeApprovalRejectionDeliveryTransaction(ctx, envelope.DeliveryID)
	if err != nil || transaction.Status != agentruntime.RuntimeApprovalRejectionDeliveryTransactionAborted {
		_ = store.Close()
		t.Fatalf("abort 状态未持久化: %#v err=%v", transaction, err)
	}
	if _, err := repo.(agentruntime.RuntimeApprovalRejectionDeliveryInbox).GetRuntimeApprovalRejectionDelivery(ctx, envelope.DeliveryID); !errors.Is(err, agentruntime.ErrNotFound) {
		_ = store.Close()
		t.Fatalf("abort 不得写入 rejection inbox: %v", err)
	}
	if _, err := txnRepo.CommitRuntimeApprovalRejectionDelivery(ctx, envelope); !errors.Is(err, agentruntime.ErrConflict) {
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
	restarted := store.RuntimeRepository().(agentruntime.RuntimeApprovalRejectionDeliveryAbortableTransactionRepository)
	if duplicate, err := restarted.AbortRuntimeApprovalRejectionDelivery(ctx, envelope); err != nil || !duplicate {
		t.Fatalf("重启后 aborted transaction 应幂等: duplicate=%v err=%v", duplicate, err)
	}
}

func TestSQLiteRuntimeApprovalRejectionDeliveryAppliesWorkspaceSidecarAtomically(t *testing.T) {
	ctx := context.Background()
	fixture := newSQLiteApprovalRejectionFixture(t, "sqlite-rejection-apply-one-phase")
	applyRepo, ok := fixture.repo.(agentruntime.RuntimeApprovalRejectionDeliveryApplyRepository)
	if !ok {
		t.Fatal("SQLite runtime repository 未实现 Workspace rejection apply 能力")
	}
	now := time.Now().UTC()
	delivery, err := agentruntime.NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", fixture.approval, fixture.invocation, "用户拒绝", now)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := delivery.Envelope(now)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err = agentruntime.SignRuntimeApprovalRejectionDeliveryEnvelope(envelope, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := applyRepo.AcceptRuntimeApprovalRejectionDeliveryAndApply(ctx, envelope); err != nil || duplicate {
		t.Fatalf("首次 Workspace rejection apply 错误: duplicate=%v err=%v", duplicate, err)
	}
	operation, err := fixture.workspace.GetOperation(ctx, fixture.operation.ID)
	if err != nil || operation.Status != workspace.OperationRejected || operation.Result != "用户已拒绝" {
		t.Fatalf("目的端 Operation 未原子拒绝: operation=%#v err=%v", operation, err)
	}
	runRepo := fixture.workspace.(workspace.CommandRunRepository)
	run, err := runRepo.GetCommandRun(ctx, fixture.run.ID)
	if err != nil || run.Status != workspace.CommandRunCancelled || run.Outcome != workspace.CommandOutcomeCancelled || run.Revision != fixture.run.Revision+1 {
		t.Fatalf("目的端 queued CommandRun 未原子取消: run=%#v err=%v", run, err)
	}
	revision := run.Revision
	if duplicate, err := applyRepo.AcceptRuntimeApprovalRejectionDeliveryAndApply(ctx, envelope); err != nil || !duplicate {
		t.Fatalf("重复 Workspace rejection apply 应幂等: duplicate=%v err=%v", duplicate, err)
	}
	run, err = runRepo.GetCommandRun(ctx, fixture.run.ID)
	if err != nil || run.Revision != revision || run.Status != workspace.CommandRunCancelled {
		t.Fatalf("重复 apply 不应再次推进 CommandRun: run=%#v err=%v", run, err)
	}
	var missingEnvelope agentruntime.RuntimeApprovalRejectionDeliveryEnvelope
	if _, err := applyRepo.AcceptRuntimeApprovalRejectionDeliveryAndApply(ctx, func() agentruntime.RuntimeApprovalRejectionDeliveryEnvelope {
		missingApproval := fixture.approval
		missingApproval.OperationID = "missing-destination-operation"
		missing, createErr := agentruntime.NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", missingApproval, fixture.invocation, "用户拒绝", now)
		if createErr != nil {
			t.Fatal(createErr)
		}
		unsigned, envelopeErr := missing.Envelope(now)
		if envelopeErr != nil {
			t.Fatal(envelopeErr)
		}
		signedMissing, signErr := agentruntime.SignRuntimeApprovalRejectionDeliveryEnvelope(unsigned, []byte("0123456789abcdef0123456789abcdef"))
		if signErr != nil {
			t.Fatal(signErr)
		}
		missingEnvelope = signedMissing
		return signedMissing
	}()); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("目的端缺少 Operation 时必须 fail-closed: %v", err)
	}
	if _, err := fixture.repo.(agentruntime.RuntimeApprovalRejectionDeliveryInbox).GetRuntimeApprovalRejectionDelivery(ctx, missingEnvelope.DeliveryID); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("缺失 Operation 的 intent 不应写入 inbox: %v", err)
	}
}

func TestSQLiteRuntimeApprovalRejectionDeliveryTransactionalApplyIsAtomicAndIdempotent(t *testing.T) {
	ctx := context.Background()
	fixture := newSQLiteApprovalRejectionFixture(t, "sqlite-rejection-apply-transaction")
	transactionRepo := fixture.repo.(agentruntime.RuntimeApprovalRejectionDeliveryTransactionRepository)
	applyRepo := fixture.repo.(agentruntime.RuntimeApprovalRejectionDeliveryApplyRepository)
	now := time.Now().UTC()
	delivery, err := agentruntime.NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", fixture.approval, fixture.invocation, "用户拒绝", now)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := delivery.Envelope(now)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err = agentruntime.SignRuntimeApprovalRejectionDeliveryEnvelope(envelope, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := transactionRepo.PrepareRuntimeApprovalRejectionDelivery(ctx, envelope); err != nil || duplicate {
		t.Fatalf("首次 prepare 错误: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := applyRepo.CommitRuntimeApprovalRejectionDeliveryAndApply(ctx, envelope); err != nil || duplicate {
		t.Fatalf("首次 transactional apply 错误: duplicate=%v err=%v", duplicate, err)
	}
	operation, err := fixture.workspace.GetOperation(ctx, fixture.operation.ID)
	if err != nil || operation.Status != workspace.OperationRejected {
		t.Fatalf("transaction commit 未拒绝目的端 Operation: operation=%#v err=%v", operation, err)
	}
	if duplicate, err := applyRepo.CommitRuntimeApprovalRejectionDeliveryAndApply(ctx, envelope); err != nil || !duplicate {
		t.Fatalf("重复 transactional apply 应幂等: duplicate=%v err=%v", duplicate, err)
	}
	transactionReader := fixture.repo.(interface {
		GetRuntimeApprovalRejectionDeliveryTransaction(context.Context, string) (agentruntime.RuntimeApprovalRejectionDeliveryTransaction, error)
	})
	transaction, err := transactionReader.GetRuntimeApprovalRejectionDeliveryTransaction(ctx, envelope.DeliveryID)
	if err != nil || transaction.Status != agentruntime.RuntimeApprovalRejectionDeliveryTransactionCommitted {
		t.Fatalf("transaction ledger 未进入 committed: transaction=%#v err=%v", transaction, err)
	}
}

func TestSQLiteRuntimeApprovalRejectionDeliveryHTTPWorkspaceApplyIsExplicit(t *testing.T) {
	ctx := context.Background()
	fixture := newSQLiteApprovalRejectionFixture(t, "sqlite-rejection-apply-http")
	secret := []byte("0123456789abcdef0123456789abcdef")
	now := time.Now().UTC()
	receiver, err := agentruntime.NewRuntimeApprovalRejectionDeliveryReceiver("runtime-a", "runtime-b", secret, fixture.repo.(agentruntime.RuntimeApprovalRejectionDeliveryInbox))
	if err != nil {
		t.Fatal(err)
	}
	if err := receiver.SetWorkspaceCancellationEnabled(true); err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now }
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		receiver.ServeHTTP(writer, request)
	}))
	defer server.Close()
	transport, err := agentruntime.NewRuntimeApprovalRejectionDeliveryHTTPTransport(server.URL, "runtime-a", "runtime-b", secret, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	transport.Now = func() time.Time { return now }
	delivery, err := agentruntime.NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", fixture.approval, fixture.invocation, "用户拒绝", now)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := delivery.Envelope(now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.Deliver(ctx, envelope); err != nil {
		t.Fatal(err)
	}
	operation, err := fixture.workspace.GetOperation(ctx, fixture.operation.ID)
	if err != nil || operation.Status != workspace.OperationRejected {
		t.Fatalf("HTTP receiver 未触发目的端 Workspace apply: operation=%#v err=%v", operation, err)
	}
	run, err := fixture.workspace.(workspace.CommandRunRepository).GetCommandRun(ctx, fixture.run.ID)
	if err != nil || run.Status != workspace.CommandRunCancelled || run.Outcome != workspace.CommandOutcomeCancelled {
		t.Fatalf("HTTP apply 后 queued CommandRun 未取消: run=%#v err=%v", run, err)
	}

	// The wire body remains metadata-only even when sidecar application is on.
	encoded, err := json.Marshal(envelope)
	if err != nil || bytes.Contains(encoded, []byte("用户拒绝")) {
		t.Fatalf("HTTP rejection envelope 不应携带 reason 原文: %s", encoded)
	}
}

func TestSQLiteCommitApprovalRejectionWithDeliveryIsLocalAtomicBoundary(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo := store.RuntimeRepository()
	now := time.Now().UTC()
	invocation := agentruntime.Invocation{ID: "sqlite-atomic-rejection-invocation", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingApproval, CreatedAt: now, UpdatedAt: now}
	approval := agentruntime.Approval{ID: "sqlite-atomic-rejection-approval", InvocationID: invocation.ID, ToolCallID: "sqlite-atomic-rejection-tool", ToolName: "guarded_write", OriginalCallID: "original", ConfirmationCallID: "confirmation", Status: agentruntime.ApprovalPending, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	if err := repo.(agentruntime.ToolCallRepository).CreateToolCall(ctx, agentruntime.ToolCall{ID: approval.ToolCallID, InvocationID: invocation.ID, ToolName: approval.ToolName, OriginalCallID: approval.OriginalCallID, OperationID: approval.OperationID, ApprovalID: approval.ID, Status: agentruntime.ToolCallWaitingApproval, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	responseJSON := []byte(`{"confirmed":false}`)
	digest := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	resume := agentruntime.InvocationResume{ID: "sqlite-atomic-rejection-resume", InvocationID: invocation.ID, WaitID: approval.ConfirmationCallID, Name: "adk_tool_confirmation", ResponseJSON: responseJSON, RequestDigest: digest, CreatedAt: now}
	resumeOutbox := agentruntime.InvocationResumeOutbox{ID: "sqlite-atomic-rejection-resume-outbox", InvocationID: invocation.ID, WaitID: resume.WaitID, Name: resume.Name, ResponseJSON: responseJSON, RequestDigest: digest, Status: agentruntime.InvocationResumeOutboxQueued, AvailableAt: now, CreatedAt: now, UpdatedAt: now}
	delivery, err := agentruntime.NewRuntimeApprovalRejectionDeliveryOutbox("runtime-a", "runtime-b", approval, invocation, "用户拒绝", now)
	if err != nil {
		t.Fatal(err)
	}
	commit := agentruntime.ApprovalResumeCommit{ApprovalID: approval.ID, ApprovalFromStatus: agentruntime.ApprovalPending, ApprovalToStatus: agentruntime.ApprovalRejected, InvocationID: invocation.ID, InvocationFromStatus: agentruntime.InvocationWaitingApproval, InvocationToStatus: agentruntime.InvocationQueued, ToolCallID: approval.ToolCallID, ToolCallStatus: agentruntime.ToolCallRejected, Reason: "用户拒绝", Resume: resume, Outbox: resumeOutbox, Events: []agentruntime.AgentEvent{{ID: "sqlite-atomic-rejection-event", InvocationID: invocation.ID, Type: agentruntime.EventApprovalResolved, Timestamp: now}}}
	atomicRepo := repo.(agentruntime.ApprovalRejectionDeliveryCommitRepository)
	forged := delivery
	forged.OperationID = "forged-operation"
	forged.DeliveryID = agentruntime.RuntimeApprovalRejectionDeliveryID(forged.Source, forged.Destination, forged.ApprovalID, forged.InvocationID, forged.ToolCallID, forged.OperationID, forged.ReasonDigest)
	forged.ID = agentruntime.RuntimeApprovalRejectionDeliveryOutboxID(forged.Source, forged.Destination, forged.DeliveryID)
	forgedCommit := commit
	forgedCommit.RejectionDelivery = &forged
	if _, _, _, err := atomicRepo.CommitApprovalRejectionWithDelivery(ctx, forgedCommit, forged); !errors.Is(err, agentruntime.ErrConflict) {
		t.Fatalf("事务内 approval metadata 变化必须拒绝: %v", err)
	}
	unchangedApproval, err := repo.GetApproval(ctx, approval.ID)
	if err != nil || unchangedApproval.Status != agentruntime.ApprovalPending {
		t.Fatalf("metadata 冲突不应改变 Approval: %#v err=%v", unchangedApproval, err)
	}
	resolved, queued, _, err := atomicRepo.CommitApprovalRejectionWithDelivery(ctx, commit, delivery)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != agentruntime.ApprovalRejected || queued.Status != agentruntime.InvocationQueued {
		t.Fatalf("原子拒绝状态错误: approval=%s invocation=%s", resolved.Status, queued.Status)
	}
	if _, err := repo.(agentruntime.RuntimeApprovalRejectionDeliveryOutboxRepository).GetRuntimeApprovalRejectionDeliveryOutbox(ctx, delivery.ID); err != nil {
		t.Fatalf("原子拒绝未同时写入 outbox: %v", err)
	}
	storedDelivery, err := repo.(agentruntime.RuntimeApprovalRejectionDeliveryOutboxRepository).GetRuntimeApprovalRejectionDeliveryOutbox(ctx, delivery.ID)
	if err != nil || storedDelivery.GroupID == "" {
		t.Fatalf("SQLite rejection outbox 必须绑定 group: %#v err=%v", storedDelivery, err)
	}
	groups, err := repo.(agentruntime.RuntimeDeliveryGroupRepository).ListRuntimeDeliveryGroups(ctx, invocation.ID, agentruntime.RuntimeDeliveryGroupQueued, 10)
	if err != nil || len(groups) != 1 || len(groups[0].Members) != 2 || groups[0].ID != storedDelivery.GroupID {
		t.Fatalf("SQLite 拒绝提交应原子登记 event/rejection group: groups=%#v delivery=%#v err=%v", groups, storedDelivery, err)
	}
}
