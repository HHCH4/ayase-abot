package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const deliveryGroupTestSecret = "0123456789abcdef0123456789abcdef"

func testRuntimeDeliveryGroup(t *testing.T, status RuntimeDeliveryGroupStatus) RuntimeDeliveryGroup {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	members := []RuntimeDeliveryGroupMember{
		{Kind: RuntimeDeliveryKindEvent, OutboxID: "event-group-test", DeliveryID: "event-group-test", Status: RuntimeDeliveryGroupMemberCompleted},
		{Kind: RuntimeDeliveryKindConfig, OutboxID: "config-group-test", DeliveryID: "config-group-test", Status: RuntimeDeliveryGroupMemberCompleted},
	}
	if status == RuntimeDeliveryGroupFailed {
		members[1].Status = RuntimeDeliveryGroupMemberFailed
		members[1].LastError = "远端拒绝"
	}
	group, err := NewRuntimeDeliveryGroup("runtime-a", "runtime-b", "inv-group-txn", members, now)
	if err != nil {
		t.Fatal(err)
	}
	group.Status = status
	group.UpdatedAt = now
	if status == RuntimeDeliveryGroupFailed {
		group.CompletedAt = nil
		group.LastError = "远端拒绝"
	}
	return group
}

func TestRuntimeDeliveryGroupEnvelopeIsStableAndSigned(t *testing.T) {
	group := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupCompleted)
	envelope, err := NewRuntimeDeliveryGroupEnvelope(group, group.CreatedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	signed, err := SignRuntimeDeliveryGroupEnvelope(envelope, []byte(deliveryGroupTestSecret))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRuntimeDeliveryGroupEnvelope(signed, []byte(deliveryGroupTestSecret)); err != nil {
		t.Fatal(err)
	}
	if signed.GroupID != group.ID || signed.MembersDigest == "" || len(signed.Members) != 2 {
		t.Fatalf("签名 envelope metadata 不完整: %#v", signed)
	}
	forged := signed
	forged.Members = append([]RuntimeDeliveryGroupMemberEnvelope(nil), signed.Members...)
	forged.Members[0].OutboxID = "forged"
	if err := VerifyRuntimeDeliveryGroupEnvelope(forged, []byte(deliveryGroupTestSecret)); err == nil {
		t.Fatal("篡改 group member 后不应通过认证")
	}
	if RuntimeDeliveryGroupMembersDigest([]RuntimeDeliveryGroupMemberEnvelope{{Kind: signed.Members[1].Kind, OutboxID: signed.Members[1].OutboxID, DeliveryID: signed.Members[1].DeliveryID}, {Kind: signed.Members[0].Kind, OutboxID: signed.Members[0].OutboxID, DeliveryID: signed.Members[0].DeliveryID}}) != signed.MembersDigest {
		t.Fatal("member 顺序不应影响 digest")
	}
}

func TestMemoryRuntimeDeliveryGroupTransactionLifecycle(t *testing.T) {
	repo := NewMemoryRepository()
	group := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupCompleted)
	envelope, err := NewRuntimeDeliveryGroupEnvelope(group, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	signed, err := SignRuntimeDeliveryGroupEnvelope(envelope, []byte(deliveryGroupTestSecret))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if duplicate, err := repo.PrepareRuntimeDeliveryGroup(ctx, signed); err != nil || duplicate {
		t.Fatalf("首次 prepare 异常: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := repo.PrepareRuntimeDeliveryGroup(ctx, signed); err != nil || !duplicate {
		t.Fatalf("重复 prepare 未幂等: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := repo.CommitRuntimeDeliveryGroup(ctx, signed); err != nil || duplicate {
		t.Fatalf("首次 commit 异常: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := repo.CommitRuntimeDeliveryGroup(ctx, signed); err != nil || !duplicate {
		t.Fatalf("重复 commit 未幂等: duplicate=%v err=%v", duplicate, err)
	}
	if _, err := repo.AbortRuntimeDeliveryGroup(ctx, signed); !errors.Is(err, ErrConflict) {
		t.Fatalf("committed group 不应允许 abort: %v", err)
	}
	transaction, err := repo.GetRuntimeDeliveryGroupTransaction(ctx, group.ID)
	if err != nil || transaction.Status != RuntimeDeliveryGroupTransactionCommitted {
		t.Fatalf("group transaction 状态不正确: %#v err=%v", transaction, err)
	}

	failed := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupFailed)
	failed.InvocationID = "inv-group-txn-failed"
	failed.ID = RuntimeDeliveryGroupID(failed.Source, failed.Destination, failed.InvocationID, failed.Members)
	failedEnvelope, err := NewRuntimeDeliveryGroupEnvelope(failed, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	failedSigned, err := SignRuntimeDeliveryGroupEnvelope(failedEnvelope, []byte(deliveryGroupTestSecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PrepareRuntimeDeliveryGroup(ctx, failedSigned); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := repo.AbortRuntimeDeliveryGroup(ctx, failedSigned); err != nil || duplicate {
		t.Fatalf("首次 abort 异常: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := repo.AbortRuntimeDeliveryGroup(ctx, failedSigned); err != nil || !duplicate {
		t.Fatalf("重复 abort 未幂等: duplicate=%v err=%v", duplicate, err)
	}
}

type testRuntimeDeliveryGroupTransport struct {
	prepare atomic.Int32
	commit  atomic.Int32
	abort   atomic.Int32
}

func (t *testRuntimeDeliveryGroupTransport) BuildRuntimeDeliveryGroupEnvelope(group RuntimeDeliveryGroup, now time.Time) (RuntimeDeliveryGroupEnvelope, error) {
	return NewRuntimeDeliveryGroupEnvelope(group, now)
}
func (t *testRuntimeDeliveryGroupTransport) PrepareRuntimeDeliveryGroup(_ context.Context, envelope RuntimeDeliveryGroupEnvelope) (RuntimeDeliveryGroupTransactionReceipt, error) {
	t.prepare.Add(1)
	return RuntimeDeliveryGroupTransactionReceipt{Version: envelope.Version, GroupID: envelope.GroupID, InvocationID: envelope.InvocationID, MembersDigest: envelope.MembersDigest, Phase: "prepare"}, nil
}
func (t *testRuntimeDeliveryGroupTransport) CommitRuntimeDeliveryGroup(_ context.Context, envelope RuntimeDeliveryGroupEnvelope) (RuntimeDeliveryGroupTransactionReceipt, error) {
	t.commit.Add(1)
	return RuntimeDeliveryGroupTransactionReceipt{Version: envelope.Version, GroupID: envelope.GroupID, InvocationID: envelope.InvocationID, MembersDigest: envelope.MembersDigest, Phase: "commit"}, nil
}
func (t *testRuntimeDeliveryGroupTransport) AbortRuntimeDeliveryGroup(_ context.Context, envelope RuntimeDeliveryGroupEnvelope) (RuntimeDeliveryGroupTransactionReceipt, error) {
	t.abort.Add(1)
	return RuntimeDeliveryGroupTransactionReceipt{Version: envelope.Version, GroupID: envelope.GroupID, InvocationID: envelope.InvocationID, MembersDigest: envelope.MembersDigest, Phase: "abort"}, nil
}

func TestCoordinatorSettlesTerminalRuntimeDeliveryGroupRemotely(t *testing.T) {
	repo := NewMemoryRepository()
	completed := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupCompleted)
	if _, err := repo.EnqueueRuntimeDeliveryGroup(context.Background(), completed); err != nil {
		t.Fatal(err)
	}
	failed := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupFailed)
	failed.InvocationID = "inv-group-txn-failed"
	failed.ID = RuntimeDeliveryGroupID(failed.Source, failed.Destination, failed.InvocationID, failed.Members)
	if _, err := repo.EnqueueRuntimeDeliveryGroup(context.Background(), failed); err != nil {
		t.Fatal(err)
	}
	transport := &testRuntimeDeliveryGroupTransport{}
	coordinator := &Coordinator{repo: repo, deliveryGroupTransport: transport}
	coordinator.ReconcileRuntimeDeliveryGroups(context.Background(), time.Now().UTC())
	if transport.prepare.Load() != 2 || transport.commit.Load() != 1 || transport.abort.Load() != 1 {
		t.Fatalf("terminal group remote settle 调用不正确: prepare=%d commit=%d abort=%d", transport.prepare.Load(), transport.commit.Load(), transport.abort.Load())
	}
}

func TestRuntimeDeliveryGroupReceiverAndHTTPTransport(t *testing.T) {
	repo := NewMemoryRepository()
	receiver, err := NewRuntimeDeliveryGroupReceiver("runtime-a", "runtime-b", []byte(deliveryGroupTestSecret), repo)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(receiver.Handler())
	defer server.Close()
	transport, err := NewRuntimeDeliveryGroupHTTPTransactionalTransport(server.URL+"/api/v1/runtime/delivery-group", "runtime-a", "runtime-b", []byte(deliveryGroupTestSecret), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	group := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupCompleted)
	envelope, err := transport.BuildRuntimeDeliveryGroupEnvelope(group, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := transport.PrepareRuntimeDeliveryGroup(context.Background(), envelope)
	if err != nil || prepared.Phase != "prepare" {
		t.Fatalf("HTTP prepare 异常: %#v err=%v", prepared, err)
	}
	committed, err := transport.CommitRuntimeDeliveryGroup(context.Background(), envelope)
	if err != nil || committed.Phase != "commit" {
		t.Fatalf("HTTP commit 异常: %#v err=%v", committed, err)
	}
	status, err := transport.ReconcileRuntimeDeliveryGroup(context.Background(), envelope)
	if err != nil || status.Phase != RuntimeDeliveryGroupTransactionCommitted || !status.Found {
		t.Fatalf("HTTP status 对账异常: %#v err=%v", status, err)
	}
	if _, err := transport.AbortRuntimeDeliveryGroup(context.Background(), envelope); !errors.Is(err, ErrConflict) {
		t.Fatalf("committed group HTTP abort 应拒绝: %v", err)
	}
}

func TestRuntimeDeliveryGroupBatchStatusReconciliationIsBoundedAndOrdered(t *testing.T) {
	repo := NewMemoryRepository()
	receiver, err := NewRuntimeDeliveryGroupReceiver("runtime-a", "runtime-b", []byte(deliveryGroupTestSecret), repo)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(receiver.Handler())
	defer server.Close()
	transport, err := NewRuntimeDeliveryGroupHTTPTransactionalTransport(server.URL+"/api/v1/runtime/delivery-group", "runtime-a", "runtime-b", []byte(deliveryGroupTestSecret), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	first := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupCompleted)
	second := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupCompleted)
	second.InvocationID = "inv-group-txn-batch-2"
	second.ID = RuntimeDeliveryGroupID(second.Source, second.Destination, second.InvocationID, second.Members)
	firstEnvelope, err := transport.BuildRuntimeDeliveryGroupEnvelope(first, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	secondEnvelope, err := transport.BuildRuntimeDeliveryGroupEnvelope(second, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.PrepareRuntimeDeliveryGroup(context.Background(), firstEnvelope); err != nil {
		t.Fatal(err)
	}
	statuses, err := transport.ReconcileRuntimeDeliveryGroups(context.Background(), []RuntimeDeliveryGroupEnvelope{firstEnvelope, secondEnvelope})
	if err != nil || len(statuses) != 2 {
		t.Fatalf("batch status 请求失败: %#v err=%v", statuses, err)
	}
	if statuses[0].Phase != RuntimeDeliveryGroupTransactionPrepared || !statuses[0].Found || statuses[1].Phase != RuntimeDeliveryGroupTransactionStatus(RuntimeDeliveryGroupSettlementRemoteAbsent) || statuses[1].Found {
		t.Fatalf("batch status 顺序/phase 错误: %#v", statuses)
	}
	if _, err := transport.CommitRuntimeDeliveryGroup(context.Background(), firstEnvelope); err != nil {
		t.Fatal(err)
	}
	statuses, err = transport.ReconcileRuntimeDeliveryGroups(context.Background(), []RuntimeDeliveryGroupEnvelope{firstEnvelope, secondEnvelope})
	if err != nil || statuses[0].Phase != RuntimeDeliveryGroupTransactionCommitted || statuses[1].Phase != RuntimeDeliveryGroupTransactionStatus(RuntimeDeliveryGroupSettlementRemoteAbsent) {
		t.Fatalf("batch terminal status 错误: %#v err=%v", statuses, err)
	}
	if _, err := transport.ReconcileRuntimeDeliveryGroups(context.Background(), []RuntimeDeliveryGroupEnvelope{firstEnvelope, firstEnvelope}); !errors.Is(err, ErrInvalidRuntimeDeliveryGroupTransaction) {
		t.Fatalf("重复 group_id 的 batch 应拒绝: %v", err)
	}
	requestBody, err := json.Marshal(RuntimeDeliveryGroupStatusBatchRequest{Envelopes: []RuntimeDeliveryGroupEnvelope{firstEnvelope, secondEnvelope}})
	if err != nil {
		t.Fatal(err)
	}
	badHeader := httptest.NewRequest("POST", "/status/batch", bytes.NewReader(requestBody))
	badHeader.Header.Set("X-Abot-Delivery-Group-Batch-Count", "1")
	badHeaderRecorder := httptest.NewRecorder()
	receiver.BatchStatusHandler().ServeHTTP(badHeaderRecorder, badHeader)
	if badHeaderRecorder.Code != 400 {
		t.Fatalf("错误 batch count 应拒绝: status=%d body=%s", badHeaderRecorder.Code, badHeaderRecorder.Body.String())
	}
}

type staticRuntimeDeliveryGroupMemberResolver struct {
	phase RuntimeDeliveryGroupTransactionStatus
}

func (r staticRuntimeDeliveryGroupMemberResolver) RuntimeDeliveryGroupMemberPhase(context.Context, RuntimeDeliveryGroupMemberEnvelope) (RuntimeDeliveryGroupTransactionStatus, error) {
	return r.phase, nil
}

func TestRuntimeDeliveryGroupReceiverCanRequireCommittedMembers(t *testing.T) {
	repo := NewMemoryRepository()
	receiver, err := NewRuntimeDeliveryGroupReceiver("runtime-a", "runtime-b", []byte(deliveryGroupTestSecret), repo)
	if err != nil {
		t.Fatal(err)
	}
	receiver.RequireCommittedMembers = true
	receiver.MemberPhaseResolver = staticRuntimeDeliveryGroupMemberResolver{phase: RuntimeDeliveryGroupTransactionPrepared}
	group := testRuntimeDeliveryGroup(t, RuntimeDeliveryGroupCompleted)
	envelope, err := NewRuntimeDeliveryGroupEnvelope(group, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	envelope, err = SignRuntimeDeliveryGroupEnvelope(envelope, []byte(deliveryGroupTestSecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PrepareRuntimeDeliveryGroup(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
	commitRequest := httptest.NewRequest("POST", "/commit", bytes.NewReader(mustMarshalRuntimeDeliveryGroupTest(t, envelope)))
	commitRequest.Header.Set("X-Abot-Delivery-Group-Phase", "commit")
	commitRequest.Header.Set("X-Abot-Delivery-Group-Signature", envelope.Signature)
	commit := httptest.NewRecorder()
	receiver.CommitHandler().ServeHTTP(commit, commitRequest)
	if commit.Code != 409 {
		t.Fatalf("未 committed member 应被 group gate 拒绝: status=%d body=%s", commit.Code, commit.Body.String())
	}
	receiver.MemberPhaseResolver = staticRuntimeDeliveryGroupMemberResolver{phase: RuntimeDeliveryGroupTransactionCommitted}
	commitRequest = httptest.NewRequest("POST", "/commit", bytes.NewReader(mustMarshalRuntimeDeliveryGroupTest(t, envelope)))
	commitRequest.Header.Set("X-Abot-Delivery-Group-Phase", "commit")
	commitRequest.Header.Set("X-Abot-Delivery-Group-Signature", envelope.Signature)
	commit = httptest.NewRecorder()
	receiver.CommitHandler().ServeHTTP(commit, commitRequest)
	if commit.Code != 200 {
		t.Fatalf("committed member gate 不应阻断 commit: status=%d body=%s", commit.Code, commit.Body.String())
	}
}

func mustMarshalRuntimeDeliveryGroupTest(t *testing.T, envelope RuntimeDeliveryGroupEnvelope) []byte {
	t.Helper()
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
