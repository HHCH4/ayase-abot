package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func checkpointDeliveryTestSnapshot(t *testing.T, invocationID string, now time.Time) RuntimeSnapshot {
	t.Helper()
	workflow := validWorkflowCheckpointFixture()
	workflow.CurrentBranchID = "root"
	return RuntimeSnapshot{
		InvocationID: invocationID, Revision: 1, ContractVersion: 3, PlanRevision: 2,
		Phase: string(InvocationWaitingTool), WorkflowPhase: WorkflowPhaseWaitingTool,
		Workflow: workflow, ActivePlanStep: &PlanStepRef{ID: "inspect", Title: "do not export this title"},
		PendingApprovals:   []ApprovalRef{{ID: "approval-1", ToolName: "secret_tool", OperationID: "op-1"}},
		AppliedChangeSets:  []ChangeSetRef{{ID: "change-1", Revision: 2, Status: "applied"}},
		VerificationRuns:   []VerificationRef{{ID: "verification-1", Status: "passed", Summary: "secret verification output"}},
		WorkingSetRevision: 4,
		Budget:             BudgetStatus{ContextWindow: 8192, OutputReserve: 512, SafetyReserve: 128, EstimatedInput: 300, ToolCallsUsed: 2, ToolCallsLimit: 10, LastManifestID: "manifest-1"},
		Blockers:           []Blocker{{Code: "runtime_blocked", Message: "secret blocker message"}},
		UnknownStates:      []UnknownState{{Code: "remote_unknown", Message: "secret unknown detail"}},
		GeneratedAt:        now.UTC(),
	}
}

func TestRuntimeCheckpointProjectionIsMetadataOnlyAndDeterministic(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 123456789, time.UTC)
	snapshot := checkpointDeliveryTestSnapshot(t, "inv-checkpoint-delivery", now)
	projection, err := NewRuntimeCheckpointProjection(snapshot, 7)
	if err != nil {
		t.Fatalf("projection 构造失败: %v", err)
	}
	if err := projection.Validate(); err != nil {
		t.Fatalf("projection 应满足约束: %v", err)
	}
	digest, err := RuntimeCheckpointProjectionDigest(projection)
	if err != nil || len(digest) != 64 {
		t.Fatalf("projection digest 错误: %q %v", digest, err)
	}
	encoded, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	body := string(encoded)
	for _, secret := range []string{"secret_tool", "secret verification output", "secret blocker message", "secret unknown detail", "do not export this title"} {
		if strings.Contains(body, secret) {
			t.Fatalf("metadata-only projection 泄露自由文本 %q: %s", secret, body)
		}
	}
	second, err := NewRuntimeCheckpointProjection(snapshot, 7)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := RuntimeCheckpointProjectionDigest(second)
	if err != nil || secondDigest != digest {
		t.Fatalf("相同 snapshot projection digest 必须稳定: %q/%q err=%v", digest, secondDigest, err)
	}
	envelope, _, err := NewRuntimeCheckpointDeliveryEnvelope("runtime-a", "runtime-b", snapshot, 7)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.DeliveryID == "" || envelope.SnapshotDigest != digest || envelope.Signature != "" {
		t.Fatalf("envelope metadata 错误: %#v", envelope)
	}
	secretKey := []byte("checkpoint-delivery-test-secret")
	signed, err := SignRuntimeCheckpointDeliveryEnvelope(envelope, secretKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRuntimeCheckpointDeliveryEnvelope(signed, secretKey); err != nil {
		t.Fatalf("签名 envelope 应可验证: %v", err)
	}
	signed.SnapshotDigest = strings.Repeat("0", 64)
	if err := VerifyRuntimeCheckpointDeliveryEnvelope(signed, secretKey); !errors.Is(err, ErrRuntimeCheckpointDeliveryAuth) {
		t.Fatalf("篡改 digest 必须拒绝: %v", err)
	}
}

func TestMemoryCheckpointDeliveryInboxIsMonotonicIdempotentAndDefensive(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Date(2026, 9, 13, 13, 0, 0, 0, time.UTC)
	snapshot := checkpointDeliveryTestSnapshot(t, "inv-checkpoint-inbox", now)
	envelope, projection, err := NewRuntimeCheckpointDeliveryEnvelope("runtime-a", "runtime-b", snapshot, 3)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("checkpoint-delivery-test-secret")
	envelope, err = SignRuntimeCheckpointDeliveryEnvelope(envelope, secret)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := repo.AcceptRuntimeCheckpointDelivery(ctx, envelope, projection)
	if err != nil || duplicate {
		t.Fatalf("首次 inbox 写入错误: duplicate=%v err=%v", duplicate, err)
	}
	duplicate, err = repo.AcceptRuntimeCheckpointDelivery(ctx, envelope, projection)
	if err != nil || !duplicate {
		t.Fatalf("相同 delivery 必须幂等: duplicate=%v err=%v", duplicate, err)
	}
	stored, err := repo.GetRuntimeCheckpointDelivery(ctx, envelope.InvocationID)
	if err != nil || stored.SnapshotRevision != 1 || stored.EventSequence != 3 {
		t.Fatalf("inbox 记录错误: %#v err=%v", stored, err)
	}
	stored.Projection.Workflow.ActiveBoundaryIDs[0] = "mutated"
	again, err := repo.GetRuntimeCheckpointDelivery(ctx, envelope.InvocationID)
	if err != nil || again.Projection.Workflow.ActiveBoundaryIDs[0] == "mutated" {
		t.Fatalf("inbox 必须返回防御性 projection: %#v err=%v", again, err)
	}
	older := envelope
	older.SnapshotRevision = 0
	if _, err := repo.AcceptRuntimeCheckpointDelivery(ctx, older, projection); !errors.Is(err, ErrInvalidRuntimeCheckpointDelivery) {
		t.Fatalf("无效旧 revision 必须拒绝: %v", err)
	}
	newSnapshot := snapshot
	newSnapshot.Revision = 2
	newSnapshot.GeneratedAt = now.Add(time.Second)
	newEnvelope, newProjection, err := NewRuntimeCheckpointDeliveryEnvelope("runtime-a", "runtime-b", newSnapshot, 4)
	if err != nil {
		t.Fatal(err)
	}
	newEnvelope, err = SignRuntimeCheckpointDeliveryEnvelope(newEnvelope, secret)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := repo.AcceptRuntimeCheckpointDelivery(ctx, newEnvelope, newProjection); err != nil || duplicate {
		t.Fatalf("更高 revision 必须接受: duplicate=%v err=%v", duplicate, err)
	}
	if _, err := repo.AcceptRuntimeCheckpointDelivery(ctx, envelope, projection); !errors.Is(err, ErrRuntimeCheckpointDeliveryStale) {
		t.Fatalf("迟到旧 revision 必须 fail-closed: %v", err)
	}
	conflict := newEnvelope
	conflict.SnapshotDigest = strings.Repeat("f", 64)
	if _, err := repo.AcceptRuntimeCheckpointDelivery(ctx, conflict, newProjection); !errors.Is(err, ErrRuntimeCheckpointDeliveryAuth) {
		t.Fatalf("projection digest 冲突必须拒绝: %v", err)
	}
}

func TestRuntimeCheckpointDeliverySourceAndHTTPClientVerifyHighWaterAndDigest(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Date(2026, 9, 13, 14, 0, 0, 0, time.UTC)
	invocation := Invocation{ID: "inv-checkpoint-source", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	for _, event := range []AgentEvent{
		{ID: "checkpoint-event-1", InvocationID: invocation.ID, Type: EventInvocationStarted, Timestamp: now},
		{ID: "checkpoint-event-2", InvocationID: invocation.ID, Type: EventInvocationWaiting, Timestamp: now.Add(time.Second), Data: map[string]any{"reason": "tool", "tool_call_ids": []string{"wait-1"}}},
	} {
		if _, err := repo.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := checkpointDeliveryTestSnapshot(t, invocation.ID, now)
	if _, err := repo.SaveRuntimeSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	secret := []byte("checkpoint-delivery-test-secret")
	source, err := NewRuntimeCheckpointDeliverySource("runtime-a", "runtime-b", secret, repo)
	if err != nil {
		t.Fatal(err)
	}
	source.Now = func() time.Time { return now.Add(time.Second) }
	server := httptest.NewServer(source)
	defer server.Close()
	client, err := NewRuntimeCheckpointDeliveryHTTPSourceClient(server.URL, "runtime-a", "runtime-b", secret, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	envelope, expected, err := NewRuntimeCheckpointDeliveryEnvelope("runtime-a", "runtime-b", snapshot, 2)
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.FetchCheckpoint(ctx, envelope)
	if err != nil {
		t.Fatalf("source/client 获取 checkpoint 失败: %v", err)
	}
	if got.InvocationID != expected.InvocationID || got.SnapshotRevision != 1 || got.EventSequence != 2 {
		t.Fatalf("source/client projection 错误: %#v", got)
	}
	badSequence := envelope
	badSequence.EventSequence = 1
	if _, err := client.FetchCheckpoint(ctx, badSequence); !strings.Contains(err.Error(), "HTTP 409") {
		t.Fatalf("旧 event high-water mark 必须返回冲突: %v", err)
	}
	badDigest := envelope
	badDigest.SnapshotDigest = strings.Repeat("0", 64)
	if _, err := client.FetchCheckpoint(ctx, badDigest); !strings.Contains(err.Error(), "HTTP 409") {
		t.Fatalf("错误 snapshot digest 必须返回冲突: %v", err)
	}
}

func TestRuntimeCheckpointDeliveryReceiverAndHTTPTransportAreIdempotent(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Date(2026, 9, 13, 15, 0, 0, 0, time.UTC)
	snapshot := checkpointDeliveryTestSnapshot(t, "inv-checkpoint-receiver", now)
	envelope, projection, err := NewRuntimeCheckpointDeliveryEnvelope("runtime-a", "runtime-b", snapshot, 9)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("checkpoint-delivery-test-secret")
	receiver, err := NewRuntimeCheckpointDeliveryReceiver("runtime-a", "runtime-b", secret, repo)
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
	receipt, err := transport.Deliver(ctx, envelope, projection)
	if err != nil || receipt.Duplicate {
		t.Fatalf("首次 checkpoint delivery 错误: %#v err=%v", receipt, err)
	}
	receipt, err = transport.Deliver(ctx, envelope, projection)
	if err != nil || !receipt.Duplicate {
		t.Fatalf("重复 checkpoint delivery 必须幂等: %#v err=%v", receipt, err)
	}
	stored, err := repo.GetRuntimeCheckpointDelivery(ctx, envelope.InvocationID)
	if err != nil || stored.SnapshotDigest != envelope.SnapshotDigest {
		t.Fatalf("receiver inbox 未保存 projection: %#v err=%v", stored, err)
	}
	wrongProjection := projection
	wrongProjection.EventSequence++
	if _, err := transport.Deliver(ctx, envelope, wrongProjection); !errors.Is(err, ErrRuntimeCheckpointDeliveryAuth) {
		t.Fatalf("transport 必须在发送前拒绝 projection metadata 冲突: %v", err)
	}
}

func TestRuntimeCheckpointDeliveryHTTPClientRejectsUnknownResponseFields(t *testing.T) {
	now := time.Date(2026, 9, 13, 16, 0, 0, 0, time.UTC)
	snapshot := checkpointDeliveryTestSnapshot(t, "inv-checkpoint-strict", now)
	envelope, projection, err := NewRuntimeCheckpointDeliveryEnvelope("runtime-a", "runtime-b", snapshot, 1)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("checkpoint-delivery-test-secret")
	signed, err := SignRuntimeCheckpointDeliveryEnvelope(envelope, secret)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{"envelope": signed, "projection": projection, "unexpected": true})
	}))
	defer server.Close()
	client, err := NewRuntimeCheckpointDeliveryHTTPSourceClient(server.URL, "runtime-a", "runtime-b", secret, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.FetchCheckpoint(context.Background(), envelope); !errors.Is(err, ErrInvalidRuntimeCheckpointDelivery) {
		t.Fatalf("未知 response 字段必须拒绝: %v", err)
	}
}
