package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryRuntimeSnapshotCommitIsAtomic(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Date(2026, 9, 12, 13, 0, 0, 0, time.UTC)
	invocation := Invocation{ID: "inv-snapshot-commit", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	commitRepo, ok := any(repo).(RuntimeSnapshotCommitRepository)
	if !ok {
		t.Fatal("Memory repository 未实现 RuntimeSnapshotCommitRepository")
	}
	first := RuntimeSnapshot{InvocationID: invocation.ID, Revision: 1, Phase: string(InvocationRunning), GeneratedAt: now}
	saved, event, err := commitRepo.CommitRuntimeSnapshot(ctx, first, AgentEvent{ID: "snapshot-event-1", InvocationID: invocation.ID, Type: EventRuntimeSnapshot, Timestamp: now})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 1 || event.Sequence != 1 {
		t.Fatalf("首个 snapshot/event 状态错误: snapshot=%#v event=%#v", saved, event)
	}
	if snapshot, ok := event.Data["snapshot"].(RuntimeSnapshot); !ok || snapshot.Revision != 1 {
		t.Fatalf("runtime.snapshot 事件未携带同 revision 快照: %#v", event.Data)
	}

	second := first
	second.Revision = 2
	second.Phase = string(InvocationCompleted)
	_, _, err = commitRepo.CommitRuntimeSnapshot(ctx, second, AgentEvent{ID: "snapshot-event-1", InvocationID: invocation.ID, Type: EventRuntimeSnapshot, Timestamp: now.Add(time.Second)})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("重复事件 ID 应返回 ErrConflict，实际=%v", err)
	}
	current, err := repo.GetRuntimeSnapshot(ctx, invocation.ID)
	if err != nil || current.Revision != 1 || current.Phase != string(InvocationRunning) {
		t.Fatalf("冲突后 snapshot 不应变更: snapshot=%#v err=%v", current, err)
	}
	events, err := repo.ListEvents(ctx, invocation.ID, 0, 10)
	if err != nil || len(events) != 1 || events[0].ID != "snapshot-event-1" {
		t.Fatalf("冲突后不应追加事件: events=%#v err=%v", events, err)
	}
}

func TestMemoryRuntimeSnapshotCommitRejectsNonSnapshotEventWithoutMutation(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-snapshot-event-type", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	commitRepo := any(repo).(RuntimeSnapshotCommitRepository)
	_, _, err := commitRepo.CommitRuntimeSnapshot(ctx, RuntimeSnapshot{InvocationID: invocation.ID, Revision: 1, Phase: string(InvocationRunning), GeneratedAt: now}, AgentEvent{ID: "not-a-snapshot", InvocationID: invocation.ID, Type: EventInvocationStarted, Timestamp: now})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("非 runtime.snapshot 事件应返回 ErrConflict，实际=%v", err)
	}
	if _, err := repo.GetRuntimeSnapshot(ctx, invocation.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("拒绝非 snapshot 事件后不应写入快照: %v", err)
	}
	if events, err := repo.ListEvents(ctx, invocation.ID, 0, 10); err != nil || len(events) != 0 {
		t.Fatalf("拒绝非 snapshot 事件后不应追加事件: events=%#v err=%v", events, err)
	}
}
