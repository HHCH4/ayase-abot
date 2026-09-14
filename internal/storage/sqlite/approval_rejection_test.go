package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/workspace"
)

type sqliteApprovalRejectionFixture struct {
	repo       agentruntime.Repository
	workspace  workspace.Repository
	invocation agentruntime.Invocation
	approval   agentruntime.Approval
	operation  workspace.Operation
	run        workspace.CommandRun
	commit     agentruntime.ApprovalResumeCommit
}

func newSQLiteApprovalRejectionFixture(t *testing.T, id string) sqliteApprovalRejectionFixture {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	repo := store.RuntimeRepository()
	workspaceRepo := store.WorkspaceRepository()
	invocation := agentruntime.Invocation{
		ID: id, UserID: "user", ConversationID: id, SessionID: id, Status: agentruntime.InvocationWaitingApproval,
		ActiveApprovalID: "approval-" + id, LeaseOwner: "worker-stale", CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	toolCall := agentruntime.ToolCall{
		ID: "toolcall-" + id, InvocationID: id, ConversationID: id, ToolName: "workspace_request_command",
		OriginalCallID: "call-original-" + id, ConfirmationCallID: "call-confirmation-" + id,
		Status: agentruntime.ToolCallWaitingApproval, CreatedAt: now, UpdatedAt: now,
	}
	toolRepo := repo.(agentruntime.ToolCallRepository)
	if err := toolRepo.CreateToolCall(ctx, toolCall); err != nil {
		t.Fatal(err)
	}
	operation := workspace.Operation{
		ID: "operation-" + id, WorkspaceID: "workspace-" + id, InvocationID: id, ToolCallID: toolCall.ID,
		Type: workspace.OperationCommand, CommandRunID: "run-" + id, Command: "printf no-side-effect", CWD: ".",
		Timeout: 5, Status: workspace.OperationPrepared, CreatedAt: now, UpdatedAt: now,
	}
	if err := workspaceRepo.SaveOperation(ctx, operation); err != nil {
		t.Fatal(err)
	}
	run := workspace.CommandRun{
		ID: operation.CommandRunID, WorkspaceID: operation.WorkspaceID, ConversationID: id, InvocationID: id,
		ToolCallID: toolCall.ID, OperationID: operation.ID, Executor: "local", Mode: "shell", CommandPreview: operation.Command,
		CWD: ".", TimeoutMS: 5000, Status: workspace.CommandRunQueued, Revision: 1,
		QueuedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	runRepo := workspaceRepo.(workspace.CommandRunRepository)
	if err := runRepo.CreateCommandRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	approval := agentruntime.Approval{
		ID: "approval-" + id, InvocationID: id, ConversationID: id, ToolCallID: toolCall.ID,
		ToolName: toolCall.ToolName, OperationID: operation.ID, OriginalCallID: toolCall.OriginalCallID,
		ConfirmationCallID: toolCall.ConfirmationCallID, Status: agentruntime.ApprovalPending, CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	resume := agentruntime.InvocationResume{
		ID: "resume-" + id, InvocationID: id, WaitID: approval.ConfirmationCallID, Name: "adk_tool_confirmation",
		ResponseJSON: []byte(`{"confirmed":false}`), RequestDigest: "sha256:" + id, CreatedAt: now,
	}
	commit := agentruntime.ApprovalResumeCommit{
		ApprovalID: approval.ID, ApprovalFromStatus: agentruntime.ApprovalPending, ApprovalToStatus: agentruntime.ApprovalRejected,
		InvocationID: id, InvocationFromStatus: agentruntime.InvocationWaitingApproval, InvocationToStatus: agentruntime.InvocationQueued,
		ToolCallID: toolCall.ID, ToolCallStatus: agentruntime.ToolCallRejected, Reason: "用户拒绝",
		Resume: resume,
		Outbox: agentruntime.InvocationResumeOutbox{
			ID: "outbox-" + id, InvocationID: id, WaitID: resume.WaitID, Name: resume.Name, ResponseJSON: resume.ResponseJSON,
			RequestDigest: resume.RequestDigest, Status: agentruntime.InvocationResumeOutboxQueued, CreatedAt: now, UpdatedAt: now, AvailableAt: now,
		},
		Events: []agentruntime.AgentEvent{
			{ID: "event-approval-resolved-" + id, InvocationID: id, Type: agentruntime.EventApprovalResolved, Timestamp: now, Data: map[string]any{"approval_id": approval.ID, "confirmed": false}},
			{ID: "event-invocation-resumed-" + id, InvocationID: id, Type: agentruntime.EventInvocationResumed, Timestamp: now, Data: map[string]any{"approval_id": approval.ID, "confirmed": false, "request_digest": resume.RequestDigest}},
		},
	}
	return sqliteApprovalRejectionFixture{repo: repo, workspace: workspaceRepo, invocation: invocation, approval: approval, operation: operation, run: run, commit: commit}
}

func TestRuntimeRepositoryCommitsApprovalRejectionWithWorkspaceSidecarAtomically(t *testing.T) {
	fixture := newSQLiteApprovalRejectionFixture(t, "inv-approval-rejection")
	ctx := context.Background()
	commitRepo, ok := fixture.repo.(agentruntime.ApprovalRejectionCommitRepository)
	if !ok {
		t.Fatal("SQLite runtime repository 未实现 ApprovalRejectionCommitRepository")
	}
	savedApproval, savedInvocation, events, err := commitRepo.CommitApprovalRejection(ctx, fixture.commit)
	if err != nil {
		t.Fatal(err)
	}
	if savedApproval.Status != agentruntime.ApprovalRejected || savedInvocation.Status != agentruntime.InvocationQueued || savedInvocation.ActiveApprovalID != "" || savedInvocation.LeaseOwner != "" {
		t.Fatalf("拒绝提交未收口 Runtime 状态: approval=%#v invocation=%#v", savedApproval, savedInvocation)
	}
	if len(events) != 3 || events[0].Type != agentruntime.EventCommandFailed || events[1].Type != agentruntime.EventApprovalResolved || events[2].Type != agentruntime.EventInvocationResumed {
		t.Fatalf("拒绝提交事件顺序错误: %#v", events)
	}
	if events[0].Data["status"] != string(workspace.CommandRunCancelled) || events[0].Data["outcome"] != string(workspace.CommandOutcomeCancelled) {
		t.Fatalf("命令取消审计缺少终态元数据: %#v", events[0])
	}
	capabilities, ok := events[0].Data["capabilities"].(workspace.CommandExecutionCapabilities)
	if !ok || capabilities.ProfileID != "host-process-l0" || capabilities.IsolationLevel != "l0_host_process" || capabilities.Network.State != workspace.ExecutionCapabilityUnsupported {
		t.Fatalf("审批拒绝旁路事件必须携带保守能力披露: %#v", events[0].Data["capabilities"])
	}
	persistedEvents, err := fixture.repo.ListEvents(ctx, fixture.invocation.ID, 0, 20)
	if err != nil || len(persistedEvents) != 3 {
		t.Fatalf("拒绝事件持久化读取失败: events=%#v err=%v", persistedEvents, err)
	}
	persistedCapabilities, ok := persistedEvents[0].Data["capabilities"].(map[string]any)
	if !ok || persistedCapabilities["profile_id"] != "host-process-l0" || persistedCapabilities["isolation_level"] != "l0_host_process" {
		t.Fatalf("拒绝事件重放必须保留能力披露: %#v", persistedEvents[0].Data["capabilities"])
	}
	operation, err := fixture.workspace.GetOperation(ctx, fixture.operation.ID)
	if err != nil || operation.Status != workspace.OperationRejected || operation.Result != "用户已拒绝" {
		t.Fatalf("Workspace Operation 未与 Runtime 同提交拒绝: operation=%#v err=%v", operation, err)
	}
	runRepo := fixture.workspace.(workspace.CommandRunRepository)
	run, err := runRepo.GetCommandRun(ctx, fixture.run.ID)
	if err != nil || run.Status != workspace.CommandRunCancelled || run.Outcome != workspace.CommandOutcomeCancelled || run.Revision != fixture.run.Revision+1 || run.FinishedAt == nil || !strings.Contains(run.Error, "用户已拒绝") {
		t.Fatalf("排队 CommandRun 未与 Operation 同提交取消: run=%#v err=%v", run, err)
	}
	if _, err := fixture.repo.(agentruntime.InvocationResumeRepository).GetInvocationResume(ctx, fixture.invocation.ID); err != nil {
		t.Fatalf("拒绝 handoff 未保存: %v", err)
	}
	if _, err := fixture.repo.(agentruntime.InvocationResumeOutboxRepository).GetInvocationResumeOutbox(ctx, fixture.invocation.ID); err != nil {
		t.Fatalf("拒绝 outbox 未保存: %v", err)
	}
}

func TestRuntimeRepositoryRollsBackApprovalRejectionSidecarOnRuntimeEventConflict(t *testing.T) {
	fixture := newSQLiteApprovalRejectionFixture(t, "inv-approval-rejection-rollback")
	ctx := context.Background()
	if _, err := fixture.repo.AppendEvent(ctx, agentruntime.AgentEvent{ID: "existing-rejection-event", InvocationID: fixture.invocation.ID, Type: agentruntime.EventInvocationWaiting}); err != nil {
		t.Fatal(err)
	}
	fixture.commit.Events[0].ID = "existing-rejection-event"
	commitRepo := fixture.repo.(agentruntime.ApprovalRejectionCommitRepository)
	if _, _, _, err := commitRepo.CommitApprovalRejection(ctx, fixture.commit); err == nil {
		t.Fatal("Runtime 事件冲突应回滚拒绝 sidecar")
	}
	approval, err := fixture.repo.GetApproval(ctx, fixture.approval.ID)
	if err != nil || approval.Status != agentruntime.ApprovalPending {
		t.Fatalf("回滚后 Approval 不应变更: approval=%#v err=%v", approval, err)
	}
	invocation, err := fixture.repo.GetInvocation(ctx, fixture.invocation.ID)
	if err != nil || invocation.Status != agentruntime.InvocationWaitingApproval || invocation.ActiveApprovalID == "" {
		t.Fatalf("回滚后 Invocation 不应变更: invocation=%#v err=%v", invocation, err)
	}
	operation, err := fixture.workspace.GetOperation(ctx, fixture.operation.ID)
	if err != nil || operation.Status != workspace.OperationPrepared {
		t.Fatalf("回滚后 Workspace Operation 不应拒绝: operation=%#v err=%v", operation, err)
	}
	run, err := fixture.workspace.(workspace.CommandRunRepository).GetCommandRun(ctx, fixture.run.ID)
	if err != nil || run.Status != workspace.CommandRunQueued || run.Revision != fixture.run.Revision {
		t.Fatalf("回滚后 CommandRun 不应取消: run=%#v err=%v", run, err)
	}
	if _, err := fixture.repo.(agentruntime.InvocationResumeRepository).GetInvocationResume(ctx, fixture.invocation.ID); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("回滚后不应留下 handoff: %v", err)
	}
	if _, err := fixture.repo.(agentruntime.InvocationResumeOutboxRepository).GetInvocationResumeOutbox(ctx, fixture.invocation.ID); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("回滚后不应留下 outbox: %v", err)
	}
	events, err := fixture.repo.ListEvents(ctx, fixture.invocation.ID, 0, 20)
	if err != nil || len(events) != 1 || events[0].ID != "existing-rejection-event" {
		t.Fatalf("回滚后不应追加拒绝事件: events=%#v err=%v", events, err)
	}
}

func TestRuntimeRepositoryRejectsInvalidWorkspaceSidecarWithoutMutation(t *testing.T) {
	fixture := newSQLiteApprovalRejectionFixture(t, "inv-approval-rejection-invalid")
	ctx := context.Background()
	operation := fixture.operation
	operation.Status = workspace.OperationCompleted
	if err := fixture.workspace.SaveOperation(ctx, operation); err != nil {
		t.Fatal(err)
	}
	commitRepo := fixture.repo.(agentruntime.ApprovalRejectionCommitRepository)
	if _, _, _, err := commitRepo.CommitApprovalRejection(ctx, fixture.commit); !errors.Is(err, workspace.ErrOperationState) {
		t.Fatalf("已完成 Operation 应拒绝原子拒绝: %v", err)
	}
	approval, err := fixture.repo.GetApproval(ctx, fixture.approval.ID)
	if err != nil || approval.Status != agentruntime.ApprovalPending {
		t.Fatalf("非法 sidecar 不应改变 Approval: %#v err=%v", approval, err)
	}
	invocation, err := fixture.repo.GetInvocation(ctx, fixture.invocation.ID)
	if err != nil || invocation.Status != agentruntime.InvocationWaitingApproval {
		t.Fatalf("非法 sidecar 不应改变 Invocation: %#v err=%v", invocation, err)
	}
	run, err := fixture.workspace.(workspace.CommandRunRepository).GetCommandRun(ctx, fixture.run.ID)
	if err != nil || run.Status != workspace.CommandRunQueued {
		t.Fatalf("非法 sidecar 不应取消 CommandRun: %#v err=%v", run, err)
	}
}
