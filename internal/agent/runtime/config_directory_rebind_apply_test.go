package runtime

import (
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

type rebindApplyTransportFunc func(context.Context, RuntimeConfigDirectoryRebindApplyEnvelope, RuntimeCheckpointProjection) (RuntimeConfigDirectoryRebindApplyReceipt, error)

func (f rebindApplyTransportFunc) Deliver(ctx context.Context, envelope RuntimeConfigDirectoryRebindApplyEnvelope, projection RuntimeCheckpointProjection) (RuntimeConfigDirectoryRebindApplyReceipt, error) {
	return f(ctx, envelope, projection)
}

type rebindApplyReconcileTransport struct {
	deliver   rebindApplyTransportFunc
	reconcile func(context.Context, RuntimeConfigDirectoryRebindApplyEnvelope, RuntimeCheckpointProjection) (RuntimeConfigDirectoryRebindApplyStatusProof, error)
}

func (t rebindApplyReconcileTransport) Deliver(ctx context.Context, envelope RuntimeConfigDirectoryRebindApplyEnvelope, projection RuntimeCheckpointProjection) (RuntimeConfigDirectoryRebindApplyReceipt, error) {
	return t.deliver(ctx, envelope, projection)
}

func (t rebindApplyReconcileTransport) ReconcileRuntimeConfigDirectoryRebindApply(ctx context.Context, envelope RuntimeConfigDirectoryRebindApplyEnvelope, projection RuntimeCheckpointProjection) (RuntimeConfigDirectoryRebindApplyStatusProof, error) {
	return t.reconcile(ctx, envelope, projection)
}

func newRebindApplyFixture(t *testing.T) (context.Context, *MemoryRepository, *Coordinator, RuntimeConfigDirectoryRebindPlan, RuntimeSnapshot, time.Time) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 16, 0, 0, 0, time.UTC)
	snapshotText, digest := rebindTestSnapshot(t, "rebind-apply")
	invocation := Invocation{ID: "inv-rebind-apply", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, ConfigSnapshot: snapshotText, ConfigSnapshotDigest: digest, CreatedAt: now, UpdatedAt: now}
	repo := NewMemoryRepository()
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, AgentEvent{ID: "rebind-apply-event", InvocationID: invocation.ID, Type: EventInvocationWaiting, Timestamp: now}); err != nil {
		t.Fatal(err)
	}
	snapshot := checkpointDeliveryTestSnapshot(t, invocation.ID, now)
	if _, err := repo.SaveRuntimeSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	plan, err := NewRuntimeConfigDirectoryRebindPlan("runtime-source", invocation, []string{"runtime-target"}, []RuntimeConfigDirectoryRebindCapability{{Destination: "runtime-target", Revision: 1, SupportsRebind: true, SupportsCheckpoint: true, SupportsResume: true, SupportsConfigMaterialization: true}}, "", digest, "plan-key", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeConfigDirectoryRebindPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	coordinator := newRebindTestCoordinator(t, repo)
	return ctx, repo, coordinator, plan, snapshot, now
}

func TestRuntimeConfigDirectoryRebindApplyConfirmationAndCompositeCommit(t *testing.T) {
	ctx, repo, coordinator, plan, snapshot, now := newRebindApplyFixture(t)
	confirmation, err := coordinator.ConfirmRuntimeConfigDirectoryRebind(ctx, plan.ID, "confirm-key", now)
	if err != nil {
		t.Fatalf("confirmation 创建失败: %v", err)
	}
	repeated, err := coordinator.ConfirmRuntimeConfigDirectoryRebind(ctx, plan.ID, "confirm-key", now.Add(time.Second))
	if err != nil || repeated.ID != confirmation.ID {
		t.Fatalf("相同 confirmation 幂等失败: %#v err=%v", repeated, err)
	}
	encoded, err := json.Marshal(confirmation)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "confirm-key") {
		t.Fatalf("confirmation 不得泄露幂等键: %s", encoded)
	}
	apply, err := coordinator.ApplyRuntimeConfigDirectoryRebind(ctx, plan.ID, confirmation.ID, "apply-key", now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("apply enqueue 失败: %v", err)
	}
	if apply.Status != RuntimeConfigDirectoryRebindApplyQueued || apply.SnapshotRevision != snapshot.Revision || apply.EventSequence != 1 || apply.Projection.InvocationID != plan.InvocationID {
		t.Fatalf("apply 元数据错误: %#v", apply)
	}
	encoded, err = json.Marshal(apply)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "apply-key") || strings.Contains(string(encoded), "secret verification output") {
		t.Fatalf("apply API 不能泄露幂等键或自由文本: %s", encoded)
	}
	confirmationAfter, err := repo.GetRuntimeConfigDirectoryRebindConfirmation(ctx, confirmation.ID)
	if err != nil || confirmationAfter.Status != RuntimeConfigDirectoryRebindConfirmationConsumed {
		t.Fatalf("confirmation 必须与 apply 原子消费: %#v err=%v", confirmationAfter, err)
	}
	repeatedApply, err := repo.CommitRuntimeConfigDirectoryRebindApply(ctx, confirmation.ID, apply, now.Add(3*time.Second))
	if err != nil || repeatedApply.ID != apply.ID {
		t.Fatalf("相同 apply 重复提交必须幂等: %#v err=%v", repeatedApply, err)
	}
	repeatedThroughCoordinator, err := coordinator.ApplyRuntimeConfigDirectoryRebind(ctx, plan.ID, confirmation.ID, "apply-key", now.Add(3*time.Second))
	if err != nil || repeatedThroughCoordinator.ID != apply.ID {
		t.Fatalf("相同 apply 通过 Coordinator 重试也必须幂等: %#v err=%v", repeatedThroughCoordinator, err)
	}
	if _, err := coordinator.ApplyRuntimeConfigDirectoryRebind(ctx, plan.ID, confirmation.ID, "another-key", now.Add(3*time.Second)); !errors.Is(err, ErrRuntimeConfigDirectoryRebindConfirmationConflict) {
		t.Fatalf("confirmation 不得重复消费: %v", err)
	}
}

func TestRuntimeConfigDirectoryRebindApplyDispatchReceiptAndInboxAreIdempotent(t *testing.T) {
	ctx, repo, coordinator, plan, _, now := newRebindApplyFixture(t)
	confirmation, err := coordinator.ConfirmRuntimeConfigDirectoryRebind(ctx, plan.ID, "confirm-dispatch", now)
	if err != nil {
		t.Fatal(err)
	}
	apply, err := coordinator.ApplyRuntimeConfigDirectoryRebind(ctx, plan.ID, confirmation.ID, "apply-dispatch", now)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	coordinator.SetRuntimeConfigDirectoryRebindApplyTransport("runtime-source", "runtime-target", rebindApplyTransportFunc(func(_ context.Context, envelope RuntimeConfigDirectoryRebindApplyEnvelope, projection RuntimeCheckpointProjection) (RuntimeConfigDirectoryRebindApplyReceipt, error) {
		calls.Add(1)
		duplicate, acceptErr := repo.AcceptRuntimeConfigDirectoryRebind(ctx, envelope, projection)
		if acceptErr != nil {
			return RuntimeConfigDirectoryRebindApplyReceipt{}, acceptErr
		}
		return RuntimeConfigDirectoryRebindApplyReceipt{Version: RuntimeConfigDirectoryRebindApplyReceiptVersion, Source: envelope.Source, Destination: envelope.Destination, PlanID: envelope.PlanID, ApplyID: envelope.ApplyID, InvocationID: envelope.InvocationID, SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence, SnapshotDigest: envelope.SnapshotDigest, Phase: RuntimeConfigDirectoryRebindApplyReceiptAccepted, Duplicate: duplicate, IssuedAt: now}, nil
	}))
	coordinator.DispatchDueRuntimeDeliveries(ctx, now.Add(time.Second))
	completed, err := coordinator.GetRuntimeConfigDirectoryRebindApply(ctx, apply.ID)
	if err != nil || completed.Status != RuntimeConfigDirectoryRebindApplyCompleted {
		t.Fatalf("receipt-backed dispatch 未完成: %#v err=%v", completed, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("首次 dispatch 应只调用一次 transport: %d", calls.Load())
	}
	envelope, err := completed.Envelope(now.Add(2 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := repo.AcceptRuntimeConfigDirectoryRebind(ctx, envelope, completed.Projection)
	if err != nil || !duplicate {
		t.Fatalf("目标 inbox 对重复请求必须幂等: duplicate=%v err=%v", duplicate, err)
	}
	tampered := completed.Projection
	tampered.Budget.ToolCallsUsed++
	if _, err := repo.AcceptRuntimeConfigDirectoryRebind(ctx, envelope, tampered); !errors.Is(err, ErrRuntimeConfigDirectoryRebindApplyAuth) {
		t.Fatalf("目标 inbox 必须拒绝 projection 篡改: %v", err)
	}
}

func TestRuntimeConfigDirectoryRebindApplySourceDriftFailsClosed(t *testing.T) {
	ctx, repo, coordinator, plan, _, now := newRebindApplyFixture(t)
	confirmation, err := coordinator.ConfirmRuntimeConfigDirectoryRebind(ctx, plan.ID, "confirm-drift", now)
	if err != nil {
		t.Fatal(err)
	}
	apply, err := coordinator.ApplyRuntimeConfigDirectoryRebind(ctx, plan.ID, confirmation.ID, "apply-drift", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.TransitionInvocation(ctx, plan.InvocationID, InvocationWaitingTool, InvocationCancelled, "drift"); err != nil {
		t.Fatal(err)
	}
	var called atomic.Int32
	coordinator.SetRuntimeConfigDirectoryRebindApplyTransport("runtime-source", "runtime-target", rebindApplyTransportFunc(func(context.Context, RuntimeConfigDirectoryRebindApplyEnvelope, RuntimeCheckpointProjection) (RuntimeConfigDirectoryRebindApplyReceipt, error) {
		called.Add(1)
		return RuntimeConfigDirectoryRebindApplyReceipt{}, nil
	}))
	coordinator.DispatchDueRuntimeDeliveries(ctx, now.Add(time.Second))
	failed, err := coordinator.GetRuntimeConfigDirectoryRebindApply(ctx, apply.ID)
	if err != nil || failed.Status != RuntimeConfigDirectoryRebindApplyFailed || called.Load() != 0 {
		t.Fatalf("source drift 必须 fail closed 且不发送: %#v calls=%d err=%v", failed, called.Load(), err)
	}
}

func TestMemoryRuntimeConfigDirectoryRebindConfirmationExpiresAndClaimLeaseRecovers(t *testing.T) {
	ctx, repo, _, plan, _, now := newRebindApplyFixture(t)
	confirmRepo := any(repo).(RuntimeConfigDirectoryRebindConfirmationRepository)
	confirmation, err := confirmRepo.CreateRuntimeConfigDirectoryRebindConfirmation(ctx, plan, "expiry-key", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := confirmRepo.CreateRuntimeConfigDirectoryRebindConfirmation(ctx, plan, "expiry-key", confirmation.ExpiresAt); !errors.Is(err, ErrRuntimeConfigDirectoryRebindConfirmationExpired) {
		t.Fatalf("过期 confirmation 必须显式失败: %v", err)
	}
	applyRepo := any(repo).(RuntimeConfigDirectoryRebindApplyRepository)
	confirmation, err = confirmRepo.CreateRuntimeConfigDirectoryRebindConfirmation(ctx, plan, "lease-key", now)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := newRebindTestCoordinator(t, repo)
	apply, err := coordinator.ApplyRuntimeConfigDirectoryRebind(ctx, plan.ID, confirmation.ID, "lease-apply", now)
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := applyRepo.ClaimRuntimeConfigDirectoryRebindApply(ctx, "worker-a", now, time.Minute)
	if err != nil || !ok || claimed.ID != apply.ID {
		t.Fatalf("apply claim 错误: %#v ok=%v err=%v", claimed, ok, err)
	}
	if done, err := applyRepo.CompleteRuntimeConfigDirectoryRebindApply(ctx, claimed.ID, "worker-b", now); err != nil || done {
		t.Fatalf("错误 owner 不得完成 apply: done=%v err=%v", done, err)
	}
	if done, err := applyRepo.CompleteRuntimeConfigDirectoryRebindApply(ctx, claimed.ID, claimed.LeaseOwner, claimed.LeaseExpiresAt.Add(-time.Nanosecond)); err != nil || !done {
		t.Fatalf("有效 owner 应可完成 apply: done=%v err=%v", done, err)
	}
}

func TestRuntimeConfigDirectoryRebindApplyHTTPReceiverVerifiesAndDeduplicates(t *testing.T) {
	ctx, repo, coordinator, plan, _, now := newRebindApplyFixture(t)
	confirmation, err := coordinator.ConfirmRuntimeConfigDirectoryRebind(ctx, plan.ID, "receiver-confirm", now)
	if err != nil {
		t.Fatal(err)
	}
	apply, err := coordinator.ApplyRuntimeConfigDirectoryRebind(ctx, plan.ID, confirmation.ID, "receiver-apply", now)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := apply.Envelope(now)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("rebind-receiver-secret-012345678901234567890123")
	signed, err := SignRuntimeConfigDirectoryRebindApplyEnvelope(envelope, secret)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewRuntimeConfigDirectoryRebindApplyReceiver("runtime-source", "runtime-target", secret, repo)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now.Add(time.Second) }
	requestBody, err := json.Marshal(RuntimeConfigDirectoryRebindApplyRequest{Envelope: signed, Projection: apply.Projection})
	if err != nil {
		t.Fatal(err)
	}
	call := func(body []byte) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/config-directory/rebind-apply", strings.NewReader(string(body)))
		response := httptest.NewRecorder()
		receiver.ServeHTTP(response, request)
		return response
	}
	first := call(requestBody)
	if first.Code != http.StatusOK {
		t.Fatalf("receiver 首次接受失败: status=%d body=%s", first.Code, first.Body.String())
	}
	var receipt RuntimeConfigDirectoryRebindApplyReceipt
	if err := json.Unmarshal(first.Body.Bytes(), &receipt); err != nil || receipt.Duplicate {
		t.Fatalf("receiver 首次 receipt 错误: %#v err=%v", receipt, err)
	}
	if err := VerifyRuntimeConfigDirectoryRebindApplyReceipt(receipt, secret); err != nil {
		t.Fatalf("receiver receipt 签名无效: %v", err)
	}
	second := call(requestBody)
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), `"duplicate":true`) {
		t.Fatalf("receiver 重复请求必须返回 duplicate=true: status=%d body=%s", second.Code, second.Body.String())
	}
	tampered := signed
	tampered.SnapshotDigest = strings.Repeat("0", 64)
	tamperedRequest, err := json.Marshal(RuntimeConfigDirectoryRebindApplyRequest{Envelope: tampered, Projection: apply.Projection})
	if err != nil {
		t.Fatal(err)
	}
	if response := call(tamperedRequest); response.Code != http.StatusUnauthorized && response.Code != http.StatusBadRequest {
		t.Fatalf("篡改 envelope 必须拒绝: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestRuntimeConfigDirectoryRebindApplyStatusReceiverReturnsAbsentAndAccepted(t *testing.T) {
	ctx, repo, coordinator, plan, _, now := newRebindApplyFixture(t)
	confirmation, err := coordinator.ConfirmRuntimeConfigDirectoryRebind(ctx, plan.ID, "status-confirm", now)
	if err != nil {
		t.Fatal(err)
	}
	apply, err := coordinator.ApplyRuntimeConfigDirectoryRebind(ctx, plan.ID, confirmation.ID, "status-apply", now)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := apply.Envelope(now)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("rebind-status-secret-012345678901234567890123")
	receiver, err := NewRuntimeConfigDirectoryRebindApplyReceiver("runtime-source", "runtime-target", secret, repo)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now.Add(time.Second) }
	statusRequest := func(signed RuntimeConfigDirectoryRebindApplyEnvelope) *httptest.ResponseRecorder {
		body, marshalErr := json.Marshal(signed)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/config-directory/rebind-apply/status", strings.NewReader(string(body)))
		request.Header.Set("X-Abot-Rebind-Apply-Version", "1")
		request.Header.Set("X-Abot-Rebind-Apply-Signature", signed.Signature)
		request.Header.Set("Idempotency-Key", signed.ApplyID)
		response := httptest.NewRecorder()
		receiver.ServeStatusHTTP(response, request)
		return response
	}
	signed, err := SignRuntimeConfigDirectoryRebindApplyEnvelope(envelope, secret)
	if err != nil {
		t.Fatal(err)
	}
	absentResponse := statusRequest(signed)
	if absentResponse.Code != http.StatusOK {
		t.Fatalf("absent status 请求失败: %d %s", absentResponse.Code, absentResponse.Body.String())
	}
	var absent RuntimeConfigDirectoryRebindApplyStatusProof
	if err := json.Unmarshal(absentResponse.Body.Bytes(), &absent); err != nil {
		t.Fatal(err)
	}
	if absent.Phase != RuntimeConfigDirectoryRebindApplyStatusAbsent || absent.Found {
		t.Fatalf("未接收时必须返回 absent proof: %#v", absent)
	}
	if err := VerifyRuntimeConfigDirectoryRebindApplyStatusProof(absent, secret); err != nil {
		t.Fatalf("absent proof 签名无效: %v", err)
	}
	if err := absent.ValidateAgainst(signed); err != nil {
		t.Fatalf("absent proof identity 错误: %v", err)
	}
	if _, err := repo.AcceptRuntimeConfigDirectoryRebind(ctx, signed, apply.Projection); err != nil {
		t.Fatal(err)
	}
	acceptedResponse := statusRequest(signed)
	if acceptedResponse.Code != http.StatusOK {
		t.Fatalf("accepted status 请求失败: %d %s", acceptedResponse.Code, acceptedResponse.Body.String())
	}
	var accepted RuntimeConfigDirectoryRebindApplyStatusProof
	if err := json.Unmarshal(acceptedResponse.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted.Phase != RuntimeConfigDirectoryRebindApplyStatusAccepted || !accepted.Found {
		t.Fatalf("已接收时必须返回 accepted proof: %#v", accepted)
	}
	if err := VerifyRuntimeConfigDirectoryRebindApplyStatusProof(accepted, secret); err != nil {
		t.Fatalf("accepted proof 签名无效: %v", err)
	}
	tampered := accepted
	tampered.SnapshotRevision++
	if err := tampered.ValidateAgainst(signed); !errors.Is(err, ErrRuntimeConfigDirectoryRebindApplyStatusAuth) {
		t.Fatalf("status proof identity 篡改必须拒绝: %v", err)
	}
	tampered = accepted
	tampered.Signature = strings.Repeat("0", 64)
	if err := VerifyRuntimeConfigDirectoryRebindApplyStatusProof(tampered, secret); !errors.Is(err, ErrRuntimeConfigDirectoryRebindApplyStatusAuth) {
		t.Fatalf("status proof 签名篡改必须拒绝: %v", err)
	}
}

func TestRuntimeConfigDirectoryRebindApplyDispatchReconcilesBeforeDeliver(t *testing.T) {
	ctx, repo, coordinator, plan, _, now := newRebindApplyFixture(t)
	confirmation, err := coordinator.ConfirmRuntimeConfigDirectoryRebind(ctx, plan.ID, "status-dispatch-confirm", now)
	if err != nil {
		t.Fatal(err)
	}
	apply, err := coordinator.ApplyRuntimeConfigDirectoryRebind(ctx, plan.ID, confirmation.ID, "status-dispatch-apply", now)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := apply.Envelope(now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AcceptRuntimeConfigDirectoryRebind(ctx, envelope, apply.Projection); err != nil {
		t.Fatal(err)
	}
	var statusCalls, deliverCalls atomic.Int32
	transport := rebindApplyReconcileTransport{
		deliver: func(context.Context, RuntimeConfigDirectoryRebindApplyEnvelope, RuntimeCheckpointProjection) (RuntimeConfigDirectoryRebindApplyReceipt, error) {
			deliverCalls.Add(1)
			return RuntimeConfigDirectoryRebindApplyReceipt{}, errors.New("Deliver 不应在 accepted proof 后调用")
		},
		reconcile: func(_ context.Context, envelope RuntimeConfigDirectoryRebindApplyEnvelope, _ RuntimeCheckpointProjection) (RuntimeConfigDirectoryRebindApplyStatusProof, error) {
			statusCalls.Add(1)
			return NewRuntimeConfigDirectoryRebindApplyStatusProof(envelope, RuntimeConfigDirectoryRebindApplyStatusAccepted, true, now, now)
		},
	}
	coordinator.SetRuntimeConfigDirectoryRebindApplyTransport("runtime-source", "runtime-target", transport)
	coordinator.DispatchDueRuntimeDeliveries(ctx, now.Add(time.Second))
	completed, err := coordinator.GetRuntimeConfigDirectoryRebindApply(ctx, apply.ID)
	if err != nil || completed.Status != RuntimeConfigDirectoryRebindApplyCompleted {
		t.Fatalf("accepted proof 应直接完成 source apply: %#v err=%v", completed, err)
	}
	if statusCalls.Load() < 1 || deliverCalls.Load() != 0 {
		t.Fatalf("应先 status 对账且不重复 Deliver: status=%d deliver=%d", statusCalls.Load(), deliverCalls.Load())
	}
}

func TestRuntimeConfigDirectoryRebindApplyDispatchRejectsInvalidStatusProof(t *testing.T) {
	ctx, _, coordinator, plan, _, now := newRebindApplyFixture(t)
	confirmation, err := coordinator.ConfirmRuntimeConfigDirectoryRebind(ctx, plan.ID, "status-invalid-confirm", now)
	if err != nil {
		t.Fatal(err)
	}
	apply, err := coordinator.ApplyRuntimeConfigDirectoryRebind(ctx, plan.ID, confirmation.ID, "status-invalid-apply", now)
	if err != nil {
		t.Fatal(err)
	}
	var deliverCalls atomic.Int32
	transport := rebindApplyReconcileTransport{
		deliver: func(context.Context, RuntimeConfigDirectoryRebindApplyEnvelope, RuntimeCheckpointProjection) (RuntimeConfigDirectoryRebindApplyReceipt, error) {
			deliverCalls.Add(1)
			return RuntimeConfigDirectoryRebindApplyReceipt{}, errors.New("invalid status proof 后不得 Deliver")
		},
		reconcile: func(context.Context, RuntimeConfigDirectoryRebindApplyEnvelope, RuntimeCheckpointProjection) (RuntimeConfigDirectoryRebindApplyStatusProof, error) {
			return RuntimeConfigDirectoryRebindApplyStatusProof{Version: RuntimeConfigDirectoryRebindApplyStatusVersion}, nil
		},
	}
	coordinator.SetRuntimeConfigDirectoryRebindApplyTransport("runtime-source", "runtime-target", transport)
	coordinator.DispatchDueRuntimeDeliveries(ctx, now.Add(time.Second))
	failed, err := coordinator.GetRuntimeConfigDirectoryRebindApply(ctx, apply.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != RuntimeConfigDirectoryRebindApplyFailed || deliverCalls.Load() != 0 {
		t.Fatalf("无效 status proof 必须 fail-closed 且不发送: %#v deliver=%d", failed, deliverCalls.Load())
	}
}

func TestRuntimeConfigDirectoryRebindApplyDispatchFallsBackOnAbsentOrUnavailableStatus(t *testing.T) {
	tests := []struct {
		name        string
		statusErr   error
		statusPhase RuntimeConfigDirectoryRebindApplyStatusPhase
	}{
		{name: "absent", statusPhase: RuntimeConfigDirectoryRebindApplyStatusAbsent},
		{name: "unavailable", statusErr: ErrRuntimeConfigDirectoryRebindApplyStatusUnavailable},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, repo, coordinator, plan, _, now := newRebindApplyFixture(t)
			confirmation, err := coordinator.ConfirmRuntimeConfigDirectoryRebind(ctx, plan.ID, "fallback-confirm-"+testCase.name, now)
			if err != nil {
				t.Fatal(err)
			}
			apply, err := coordinator.ApplyRuntimeConfigDirectoryRebind(ctx, plan.ID, confirmation.ID, "fallback-apply-"+testCase.name, now)
			if err != nil {
				t.Fatal(err)
			}
			var statusCalls, deliverCalls atomic.Int32
			transport := rebindApplyReconcileTransport{
				deliver: func(_ context.Context, envelope RuntimeConfigDirectoryRebindApplyEnvelope, projection RuntimeCheckpointProjection) (RuntimeConfigDirectoryRebindApplyReceipt, error) {
					deliverCalls.Add(1)
					duplicate, acceptErr := repo.AcceptRuntimeConfigDirectoryRebind(ctx, envelope, projection)
					if acceptErr != nil {
						return RuntimeConfigDirectoryRebindApplyReceipt{}, acceptErr
					}
					return RuntimeConfigDirectoryRebindApplyReceipt{Version: RuntimeConfigDirectoryRebindApplyReceiptVersion, Source: envelope.Source, Destination: envelope.Destination, PlanID: envelope.PlanID, ApplyID: envelope.ApplyID, InvocationID: envelope.InvocationID, SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence, SnapshotDigest: envelope.SnapshotDigest, Phase: RuntimeConfigDirectoryRebindApplyReceiptAccepted, Duplicate: duplicate, IssuedAt: now}, nil
				},
				reconcile: func(_ context.Context, envelope RuntimeConfigDirectoryRebindApplyEnvelope, _ RuntimeCheckpointProjection) (RuntimeConfigDirectoryRebindApplyStatusProof, error) {
					statusCalls.Add(1)
					if testCase.statusErr != nil {
						return RuntimeConfigDirectoryRebindApplyStatusProof{}, testCase.statusErr
					}
					return NewRuntimeConfigDirectoryRebindApplyStatusProof(envelope, testCase.statusPhase, false, time.Time{}, now)
				},
			}
			coordinator.SetRuntimeConfigDirectoryRebindApplyTransport("runtime-source", "runtime-target", transport)
			coordinator.DispatchDueRuntimeDeliveries(ctx, now.Add(time.Second))
			completed, err := coordinator.GetRuntimeConfigDirectoryRebindApply(ctx, apply.ID)
			if err != nil || completed.Status != RuntimeConfigDirectoryRebindApplyCompleted {
				t.Fatalf("absent/unavailable status 应回退 Deliver: %#v err=%v", completed, err)
			}
			if statusCalls.Load() < 1 || deliverCalls.Load() != 1 {
				t.Fatalf("status fallback 次数错误: status=%d deliver=%d", statusCalls.Load(), deliverCalls.Load())
			}
		})
	}
}

func TestRuntimeConfigDirectoryRebindApplyHTTPTransportReconcilesStatus(t *testing.T) {
	ctx, repo, coordinator, plan, _, now := newRebindApplyFixture(t)
	confirmation, err := coordinator.ConfirmRuntimeConfigDirectoryRebind(ctx, plan.ID, "http-status-confirm", now)
	if err != nil {
		t.Fatal(err)
	}
	apply, err := coordinator.ApplyRuntimeConfigDirectoryRebind(ctx, plan.ID, confirmation.ID, "http-status-apply", now)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := apply.Envelope(now)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("rebind-http-status-secret-012345678901234567")
	receiver, err := NewRuntimeConfigDirectoryRebindApplyReceiver("runtime-source", "runtime-target", secret, repo)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now.Add(time.Second) }
	server := httptest.NewServer(receiver.StatusHandler())
	defer server.Close()
	transport, err := NewRuntimeConfigDirectoryRebindApplyHTTPTransport(server.URL+"/api/v1/runtime/config-directory/rebind-apply", "runtime-source", "runtime-target", secret, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	absent, err := transport.ReconcileRuntimeConfigDirectoryRebindApply(ctx, envelope, apply.Projection)
	if err != nil {
		t.Fatalf("HTTP status absent 对账失败: %v", err)
	}
	if absent.Phase != RuntimeConfigDirectoryRebindApplyStatusAbsent || absent.Found {
		t.Fatalf("HTTP status absent proof 错误: %#v", absent)
	}
	if _, err := repo.AcceptRuntimeConfigDirectoryRebind(ctx, envelope, apply.Projection); err != nil {
		t.Fatal(err)
	}
	accepted, err := transport.ReconcileRuntimeConfigDirectoryRebindApply(ctx, envelope, apply.Projection)
	if err != nil {
		t.Fatalf("HTTP status accepted 对账失败: %v", err)
	}
	if accepted.Phase != RuntimeConfigDirectoryRebindApplyStatusAccepted || !accepted.Found {
		t.Fatalf("HTTP status accepted proof 错误: %#v", accepted)
	}
}
