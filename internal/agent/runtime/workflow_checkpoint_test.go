package runtime

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func validWorkflowCheckpointFixture() WorkflowCheckpoint {
	return WorkflowCheckpoint{
		Status:            WorkflowCheckpointWaiting,
		CurrentBranchID:   "root",
		ActiveBoundaryIDs: []string{"boundary:inv:2"},
		Branches:          []WorkflowBranch{{ID: "root", Status: WorkflowBranchWaiting, HeadBoundaryID: "boundary:inv:2", StartedSequence: 0, UpdatedSequence: 2}},
		Boundaries: []WorkflowBoundary{{
			ID: "boundary:inv:2", Sequence: 2, Kind: WorkflowBoundaryTool, Status: WorkflowBoundaryWaiting, BranchID: "root",
			WaitIDs: []string{"wait-1"}, PendingWaitIDs: []string{"wait-1"}, CreatedAt: time.Unix(2, 0).UTC(),
		}},
	}
}

func TestWorkflowCheckpointValidateReferencesAndBounds(t *testing.T) {
	checkpoint := validWorkflowCheckpointFixture()
	if err := checkpoint.Validate(); err != nil {
		t.Fatalf("有效 workflow checkpoint 不应被拒绝: %v", err)
	}

	invalid := checkpoint
	invalid.ActiveBoundaryIDs = []string{"boundary:inv:2", "boundary:inv:2"}
	if err := invalid.Validate(); !errors.Is(err, ErrInvalidWorkflowCheckpoint) {
		t.Fatalf("重复 active boundary 应拒绝: %v", err)
	}

	invalid = checkpoint
	invalid.Boundaries[0].PendingWaitIDs = []string{"unknown-wait"}
	if err := invalid.Validate(); !errors.Is(err, ErrInvalidWorkflowCheckpoint) {
		t.Fatalf("不属于 wait_ids 的 pending ID 应拒绝: %v", err)
	}

	invalid = checkpoint
	invalid.Boundaries = append(invalid.Boundaries, invalid.Boundaries[0])
	if err := invalid.Validate(); !errors.Is(err, ErrInvalidWorkflowCheckpoint) {
		t.Fatalf("重复 boundary 应拒绝: %v", err)
	}
}

func TestDeriveWorkflowCheckpointTracksResumedToolAndBranch(t *testing.T) {
	now := time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)
	invocation := Invocation{ID: "inv-checkpoint", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	plan := &TaskPlan{CurrentStepID: "edit"}
	events := []AgentEvent{
		{ID: "tool-request-1", Sequence: 1, Type: EventToolRequested, Timestamp: now, Data: map[string]any{"call_id": "wait-1", "name": "wait_for_alpha"}},
		{ID: "waiting-1", Sequence: 2, Type: EventInvocationWaiting, Timestamp: now.Add(time.Second), Data: map[string]any{"reason": "tool", "tool_call_ids": []string{"wait-1"}, "step_id": "edit"}},
		{ID: "resumed-1", Sequence: 3, Type: EventInvocationResumed, Timestamp: now.Add(2 * time.Second), Data: map[string]any{"reason": "tool", "wait_id": "wait-1", "request_digest": "sha256:one"}},
		{ID: "waiting-2", Sequence: 4, Type: EventInvocationWaiting, Timestamp: now.Add(3 * time.Second), Data: map[string]any{"reason": "tool", "tool_call_ids": []string{"wait-2"}, "branch_id": "branch-child", "parent_branch_id": "root"}},
	}

	checkpoint := deriveWorkflowCheckpoint(invocation, plan, events)
	if err := checkpoint.Validate(); err != nil {
		t.Fatalf("派生 checkpoint 应满足约束: %v; %#v", err, checkpoint)
	}
	if checkpoint.Status != WorkflowCheckpointWaiting || checkpoint.CurrentBranchID != "branch-child" {
		t.Fatalf("当前 workflow 状态/分支错误: %#v", checkpoint)
	}
	if len(checkpoint.ActiveBoundaryIDs) != 1 || checkpoint.ActiveBoundaryIDs[0] != "boundary:inv-checkpoint:4" {
		t.Fatalf("当前 active boundary 错误: %#v", checkpoint.ActiveBoundaryIDs)
	}
	if len(checkpoint.Branches) != 2 || checkpoint.Branches[1].ID != "branch-child" || checkpoint.Branches[1].ParentBranchID != "root" {
		t.Fatalf("分支父子关系未保留: %#v", checkpoint.Branches)
	}
	var resolved, waiting WorkflowBoundary
	for _, boundary := range checkpoint.Boundaries {
		switch boundary.ID {
		case "boundary:inv-checkpoint:2":
			resolved = boundary
		case "boundary:inv-checkpoint:4":
			waiting = boundary
		}
	}
	if resolved.Status != WorkflowBoundaryResolved || resolved.RequestDigest != "sha256:one" || resolved.Outcome != "resumed" {
		t.Fatalf("已恢复 boundary 状态错误: %#v", resolved)
	}
	if waiting.Status != WorkflowBoundaryWaiting || len(waiting.PendingWaitIDs) != 1 || waiting.PendingWaitIDs[0] != "wait-2" {
		t.Fatalf("等待 boundary 状态错误: %#v", waiting)
	}
	second := deriveWorkflowCheckpoint(invocation, plan, events)
	if !reflect.DeepEqual(checkpoint, second) {
		t.Fatalf("同一 durable event stream 的 checkpoint 必须确定性一致:\nfirst=%#v\nsecond=%#v", checkpoint, second)
	}
}

func TestDeriveWorkflowCheckpointApprovalOutcomeAndTerminalStatus(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	invocation := Invocation{ID: "inv-approval-checkpoint", Status: InvocationCompleted, CreatedAt: now, UpdatedAt: now.Add(4 * time.Second)}
	events := []AgentEvent{
		{ID: "approval-request", Sequence: 1, Type: EventApprovalRequested, Timestamp: now, Data: map[string]any{"approval_id": "approval-1"}},
		{ID: "approval-waiting", Sequence: 2, Type: EventInvocationWaiting, Timestamp: now.Add(time.Second), Data: map[string]any{"reason": "approval"}},
		{ID: "approval-resolved", Sequence: 3, Type: EventApprovalResolved, Timestamp: now.Add(2 * time.Second), Data: map[string]any{"approval_id": "approval-1", "confirmed": false}},
		{ID: "invocation-completed", Sequence: 4, Type: EventInvocationCompleted, Timestamp: now.Add(3 * time.Second)},
	}
	checkpoint := deriveWorkflowCheckpoint(invocation, nil, events)
	if err := checkpoint.Validate(); err != nil {
		t.Fatalf("审批终态 checkpoint 应满足约束: %v; %#v", err, checkpoint)
	}
	if checkpoint.Status != WorkflowCheckpointCompleted || len(checkpoint.ActiveBoundaryIDs) != 0 {
		t.Fatalf("审批终态状态错误: %#v", checkpoint)
	}
	if len(checkpoint.Boundaries) != 1 || checkpoint.Boundaries[0].Status != WorkflowBoundaryResolved || checkpoint.Boundaries[0].Outcome != "rejected" {
		t.Fatalf("审批结果未记录: %#v", checkpoint.Boundaries)
	}
	if checkpoint.Branches[0].Status != WorkflowBranchCompleted {
		t.Fatalf("终态分支状态错误: %#v", checkpoint.Branches)
	}
}

func TestResolveWorkflowToolBoundaryRequiresCurrentBoundary(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-boundary-resolve", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	for _, event := range []AgentEvent{
		{ID: "requested", InvocationID: invocation.ID, Type: EventToolRequested, Timestamp: now, Data: map[string]any{"call_id": "wait-current", "name": "wait_for_result"}},
		{ID: "waiting", InvocationID: invocation.ID, Type: EventInvocationWaiting, Timestamp: now.Add(time.Second), Data: map[string]any{"reason": "tool", "tool_call_ids": []string{"wait-current"}}},
	} {
		if _, err := repo.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	coordinator := &Coordinator{repo: repo}
	names, boundaryID, err := coordinator.resolveWorkflowToolBoundary(ctx, invocation, "boundary:inv-boundary-resolve:2")
	if err != nil || boundaryID != "boundary:inv-boundary-resolve:2" || names["wait-current"] != "wait_for_result" {
		t.Fatalf("当前 boundary 应可解析: names=%#v id=%q err=%v", names, boundaryID, err)
	}
	if _, _, err := coordinator.resolveWorkflowToolBoundary(ctx, invocation, "boundary:inv-boundary-resolve:1"); !errors.Is(err, ErrResumeMismatch) {
		t.Fatalf("非等待 boundary 应拒绝: %v", err)
	}
}

func TestRuntimeSnapshotStoresWorkflowCheckpointDefensively(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-snapshot-workflow", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	want := RuntimeSnapshot{InvocationID: invocation.ID, Revision: 1, Phase: string(invocation.Status), Workflow: validWorkflowCheckpointFixture(), GeneratedAt: now}
	if _, err := repo.SaveRuntimeSnapshot(ctx, want); err != nil {
		t.Fatal(err)
	}
	want.Workflow.Boundaries[0].PendingWaitIDs[0] = "mutated"
	got, err := repo.GetRuntimeSnapshot(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow.Boundaries[0].PendingWaitIDs[0] != "wait-1" {
		t.Fatalf("RuntimeSnapshot workflow slice 不应暴露内部引用: %#v", got.Workflow)
	}
}
