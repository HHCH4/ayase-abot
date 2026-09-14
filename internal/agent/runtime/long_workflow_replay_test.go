package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestLongWorkflowReplayPaginatesCurrentWaitingBoundary(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Date(2026, 9, 12, 14, 0, 0, 0, time.UTC)
	invocation := Invocation{
		ID: "inv-long-workflow-replay", UserID: "user", ConversationID: "conversation", SessionID: "conversation",
		Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}

	// Fill more than one repository page before the current wait. A single
	// ListEvents(..., 5000) call would stop before the tool.requested event and
	// incorrectly report that no resumable boundary exists.
	for index := 0; index < runtimeEventPageSize+7; index++ {
		if _, err := repo.AppendEvent(ctx, AgentEvent{
			ID: fmt.Sprintf("progress-%d", index), InvocationID: invocation.ID, Type: EventRuntimeNotice,
			Timestamp: now.Add(time.Duration(index) * time.Millisecond), Data: map[string]any{"code": "progress"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.AppendEvent(ctx, AgentEvent{
		ID: "long-tool-request", InvocationID: invocation.ID, Type: EventToolRequested,
		Timestamp: now.Add(time.Hour), Data: map[string]any{"call_id": "long-wait", "name": "wait_for_external_result"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, AgentEvent{
		ID: "long-tool-waiting", InvocationID: invocation.ID, Type: EventInvocationWaiting,
		Timestamp: now.Add(time.Hour + time.Second), Data: map[string]any{"reason": "tool", "tool_call_ids": []string{"long-wait"}},
	}); err != nil {
		t.Fatal(err)
	}

	coordinator := &Coordinator{repo: repo}
	names, boundaryID, err := coordinator.resolveWorkflowToolBoundary(ctx, invocation, "")
	if err != nil {
		t.Fatalf("跨页读取后应解析当前等待边界: %v", err)
	}
	if names["long-wait"] != "wait_for_external_result" || boundaryID != "boundary:"+invocation.ID+":5009" {
		t.Fatalf("跨页边界解析错误: names=%#v boundary=%q", names, boundaryID)
	}

	snapshot, err := coordinator.RebuildRuntimeSnapshot(ctx, invocation.ID)
	if err != nil {
		t.Fatalf("跨页事件应能重建 Runtime Snapshot: %v", err)
	}
	if snapshot.Workflow.Status != WorkflowCheckpointWaiting || len(snapshot.Workflow.ActiveBoundaryIDs) != 1 || snapshot.Workflow.ActiveBoundaryIDs[0] != boundaryID {
		t.Fatalf("跨页 Snapshot 未保留当前边界: %#v", snapshot.Workflow)
	}
}

func TestLongWorkflowReplayFailsClosedAtBound(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-long-workflow-bound", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 4; index++ {
		if _, err := repo.AppendEvent(ctx, AgentEvent{ID: fmt.Sprintf("bound-%d", index), InvocationID: invocation.ID, Type: EventRuntimeNotice, Timestamp: now, Data: map[string]any{"code": "progress"}}); err != nil {
			t.Fatal(err)
		}
	}
	coordinator := &Coordinator{repo: repo}
	if events, err := coordinator.listAllInvocationEvents(ctx, invocation.ID, 3); !errors.Is(err, ErrEventReplayLimit) || events != nil {
		t.Fatalf("超过回放上限必须失败关闭且不返回部分事件: events=%#v err=%v", events, err)
	}
}
