package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMemoryResumeOutboxIsIdempotentLeasedAndRetryable(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	invocation := Invocation{ID: "inv-resume-outbox", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	base := InvocationResumeOutbox{
		ID: "resume-outbox-1", InvocationID: invocation.ID, WaitID: "wait-1", Name: "external_wait",
		ResponseJSON: []byte(`{"output":"ready"}`), RequestDigest: "sha256:resume-1", Status: InvocationResumeOutboxQueued,
		AvailableAt: now, CreatedAt: now, UpdatedAt: now,
	}
	saved, err := repo.EnqueueInvocationResumeOutbox(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := repo.EnqueueInvocationResumeOutbox(context.Background(), base)
	if err != nil || duplicate.ID != saved.ID || duplicate.Revision != saved.Revision {
		t.Fatalf("同 digest 的 outbox 写入应幂等: duplicate=%#v err=%v", duplicate, err)
	}
	claimed, ok, err := repo.ClaimInvocationResumeOutbox(context.Background(), invocation.ID, "worker-a", now, time.Minute)
	if err != nil || !ok || claimed.Attempt != 1 || claimed.Status != InvocationResumeOutboxProcessing || claimed.LeaseOwner != "worker-a" {
		t.Fatalf("首次 claim 应得到 processing 租约: item=%#v ok=%v err=%v", claimed, ok, err)
	}
	if _, ok, err := repo.ClaimInvocationResumeOutbox(context.Background(), invocation.ID, "worker-b", now.Add(30*time.Second), time.Minute); err != nil || ok {
		t.Fatalf("live lease 不应被其他 worker 抢占: ok=%v err=%v", ok, err)
	}
	if retried, err := repo.RetryInvocationResumeOutbox(context.Background(), invocation.ID, "worker-b", now, "wrong owner"); err != nil || retried {
		t.Fatalf("非 owner 不应重试 outbox: retried=%v err=%v", retried, err)
	}
	if retried, err := repo.RetryInvocationResumeOutbox(context.Background(), invocation.ID, "worker-a", now, "temporary failure"); err != nil || !retried {
		t.Fatalf("owner 应能安排 outbox 重试: retried=%v err=%v", retried, err)
	}
	if _, ok, err := repo.ClaimInvocationResumeOutbox(context.Background(), invocation.ID, "worker-b", now.Add(500*time.Millisecond), time.Minute); err != nil || ok {
		t.Fatalf("退避窗口内不应再次 claim: ok=%v err=%v", ok, err)
	}
	claimed, ok, err = repo.ClaimInvocationResumeOutbox(context.Background(), invocation.ID, "worker-b", now.Add(2*time.Second), time.Minute)
	if err != nil || !ok || claimed.Attempt != 2 || claimed.LeaseOwner != "worker-b" {
		t.Fatalf("退避后应可由其他 worker 接管: item=%#v ok=%v err=%v", claimed, ok, err)
	}
	if completed, err := repo.CompleteInvocationResumeOutbox(context.Background(), invocation.ID, "worker-a", now.Add(3*time.Second)); err != nil || completed {
		t.Fatalf("旧 owner 不应完成新租约: completed=%v err=%v", completed, err)
	}
	if completed, err := repo.CompleteInvocationResumeOutbox(context.Background(), invocation.ID, "worker-b", now.Add(3*time.Second)); err != nil || !completed {
		t.Fatalf("当前 owner 应能完成 outbox: completed=%v err=%v", completed, err)
	}
	loaded, err := repo.GetInvocationResumeOutbox(context.Background(), invocation.ID)
	if err != nil || loaded.Status != InvocationResumeOutboxCompleted || loaded.LeaseOwner != "" || loaded.Attempt != 2 {
		t.Fatalf("完成后的 outbox 状态错误: item=%#v err=%v", loaded, err)
	}
	if _, ok, err := repo.ClaimInvocationResumeOutbox(context.Background(), invocation.ID, "worker-c", now.Add(time.Hour), time.Minute); err != nil || ok {
		t.Fatalf("completed outbox 不应再次投递: ok=%v err=%v", ok, err)
	}
	second := base
	second.ID = "resume-outbox-2"
	second.RequestDigest = "sha256:resume-2"
	second.WaitID = "wait-2"
	second.AvailableAt = now.Add(time.Hour)
	if _, err := repo.EnqueueInvocationResumeOutbox(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	newest, err := repo.GetInvocationResumeOutbox(context.Background(), invocation.ID)
	if err != nil || newest.ID != second.ID {
		t.Fatalf("不同 digest 应形成新的 delivery history: item=%#v err=%v", newest, err)
	}
}

func TestMemoryResumeOutboxClaimIsSingleWinner(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	invocation := Invocation{ID: "inv-resume-outbox-race", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueInvocationResumeOutbox(context.Background(), InvocationResumeOutbox{
		ID: "resume-outbox-race", InvocationID: invocation.ID, WaitID: "wait-1", Name: "external_wait",
		ResponseJSON: []byte(`{"ok":true}`), RequestDigest: "sha256:race", AvailableAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	const workers = 24
	var group sync.WaitGroup
	group.Add(workers)
	results := make(chan bool, workers)
	for index := 0; index < workers; index++ {
		go func(index int) {
			defer group.Done()
			_, claimed, err := repo.ClaimInvocationResumeOutbox(context.Background(), invocation.ID, "worker-"+string(rune('a'+index)), now, time.Minute)
			if err != nil {
				t.Errorf("并发 claim 失败: %v", err)
			}
			results <- claimed
		}(index)
	}
	group.Wait()
	close(results)
	winners := 0
	for claimed := range results {
		if claimed {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("并发 claim 必须只有一个 winner，实际=%d", winners)
	}
}

func TestMemoryResumeOutboxLeaseExpiryFailsClosed(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	invocation := Invocation{ID: "inv-resume-outbox-expiry", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueInvocationResumeOutbox(context.Background(), InvocationResumeOutbox{
		InvocationID: invocation.ID, WaitID: "wait-expiry", Name: "external_wait", ResponseJSON: []byte(`{"ok":true}`), RequestDigest: "sha256:expiry", AvailableAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := repo.ClaimInvocationResumeOutbox(context.Background(), invocation.ID, "worker-a", now, time.Minute); err != nil || !claimed {
		t.Fatalf("首次 claim 失败: claimed=%v err=%v", claimed, err)
	}
	if completed, err := repo.CompleteInvocationResumeOutbox(context.Background(), invocation.ID, "worker-a", now.Add(time.Minute)); err != nil || completed {
		t.Fatalf("租约到期后旧 worker 不应完成: completed=%v err=%v", completed, err)
	}
	if retried, err := repo.RetryInvocationResumeOutbox(context.Background(), invocation.ID, "worker-a", now.Add(time.Minute), "late"); err != nil || retried {
		t.Fatalf("租约到期后旧 worker 不应重试: retried=%v err=%v", retried, err)
	}
	current, err := repo.GetInvocationResumeOutbox(context.Background(), invocation.ID)
	if err != nil || current.Status != InvocationResumeOutboxProcessing || current.Revision != 2 {
		t.Fatalf("迟到操作不应改变 processing 记录: item=%#v err=%v", current, err)
	}
	if _, claimed, err := repo.ClaimInvocationResumeOutbox(context.Background(), invocation.ID, "worker-b", now.Add(time.Minute), time.Minute); err != nil || !claimed {
		t.Fatalf("租约到期后新 worker 应可接管: claimed=%v err=%v", claimed, err)
	}
	if completed, err := repo.CompleteInvocationResumeOutbox(context.Background(), invocation.ID, "worker-a", now.Add(time.Minute+time.Second)); err != nil || completed {
		t.Fatalf("接管后旧 worker 不应完成: completed=%v err=%v", completed, err)
	}
	if retried, err := repo.RetryInvocationResumeOutbox(context.Background(), invocation.ID, "worker-a", now.Add(time.Minute+time.Second), "late"); err != nil || retried {
		t.Fatalf("接管后旧 worker 不应重试: retried=%v err=%v", retried, err)
	}
	if completed, err := repo.CompleteInvocationResumeOutbox(context.Background(), invocation.ID, "worker-b", now.Add(time.Minute+time.Second)); err != nil || !completed {
		t.Fatalf("当前 worker 应能完成接管后的记录: completed=%v err=%v", completed, err)
	}
}

func TestMemoryResumeOutboxLeaseBoundsAndMalformedMetadata(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	invocation := Invocation{ID: "inv-resume-outbox-bounds", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	base := InvocationResumeOutbox{InvocationID: invocation.ID, WaitID: "wait-bounds", Name: "external_wait", ResponseJSON: []byte(`{"ok":true}`), RequestDigest: "sha256:bounds", AvailableAt: now, CreatedAt: now}
	if _, err := repo.EnqueueInvocationResumeOutbox(context.Background(), InvocationResumeOutbox{
		InvocationID: base.InvocationID, WaitID: base.WaitID, Name: base.Name, ResponseJSON: base.ResponseJSON, RequestDigest: base.RequestDigest,
		Status: InvocationResumeOutboxProcessing, CreatedAt: now, LeaseExpiresAt: func() *time.Time { value := now.Add(time.Minute); return &value }(),
	}); !errors.Is(err, ErrInvalidResume) {
		t.Fatalf("缺少 processing owner 必须拒绝: %v", err)
	}
	if _, err := repo.EnqueueInvocationResumeOutbox(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := repo.ClaimInvocationResumeOutbox(context.Background(), invocation.ID, strings.Repeat("x", MaxInvocationResumeOutboxOwnerLength+1), now, time.Minute); !errors.Is(err, ErrConflict) || claimed {
		t.Fatalf("过长 owner 必须拒绝: claimed=%v err=%v", claimed, err)
	}
	if _, claimed, err := repo.ClaimInvocationResumeOutbox(context.Background(), invocation.ID, "bounded", now, MaxInvocationResumeOutboxLease+time.Nanosecond); !errors.Is(err, ErrConflict) || claimed {
		t.Fatalf("过长 lease 必须拒绝: claimed=%v err=%v", claimed, err)
	}
}

func TestMemoryCommitInvocationResumeIsAtomic(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	invocation := Invocation{ID: "inv-resume-commit", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now, LeaseOwner: "stale-worker"}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	resume := InvocationResume{ID: "resume-commit", InvocationID: invocation.ID, WaitID: "wait-1", Name: "external_wait", ResponseJSON: []byte(`{"output":"ready"}`), RequestDigest: "sha256:commit", CreatedAt: now}
	commit := InvocationResumeCommit{
		InvocationID: invocation.ID, FromStatus: InvocationWaitingTool, ToStatus: InvocationQueued,
		Resume: resume,
		Outbox: InvocationResumeOutbox{ID: "outbox-commit", InvocationID: invocation.ID, WaitID: resume.WaitID, Name: resume.Name, ResponseJSON: resume.ResponseJSON, RequestDigest: resume.RequestDigest, Status: InvocationResumeOutboxQueued, CreatedAt: now, UpdatedAt: now, AvailableAt: now},
		Event:  AgentEvent{ID: "event-resume-commit", InvocationID: invocation.ID, Type: EventInvocationResumed, Timestamp: now, Data: map[string]any{"reason": "tool", "request_digest": resume.RequestDigest}},
	}
	saved, event, err := repo.CommitInvocationResume(ctx, commit)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != InvocationQueued || saved.LeaseOwner != "" || event.Sequence != 1 {
		t.Fatalf("atomic commit 后 invocation/event 状态错误: invocation=%#v event=%#v", saved, event)
	}
	handoff, err := repo.GetInvocationResume(ctx, invocation.ID)
	if err != nil || handoff.RequestDigest != resume.RequestDigest {
		t.Fatalf("atomic commit 未保存 handoff: handoff=%#v err=%v", handoff, err)
	}
	outbox, err := repo.GetInvocationResumeOutbox(ctx, invocation.ID)
	if err != nil || outbox.Status != InvocationResumeOutboxQueued || outbox.RequestDigest != resume.RequestDigest {
		t.Fatalf("atomic commit 未保存 queued outbox: outbox=%#v err=%v", outbox, err)
	}
	events, err := repo.ListEvents(ctx, invocation.ID, 0, 10)
	if err != nil || len(events) != 1 || events[0].ID != event.ID {
		t.Fatalf("atomic commit 事件不完整: events=%#v err=%v", events, err)
	}
	eventOutbox, err := repo.ListRuntimeEventOutbox(ctx, invocation.ID, RuntimeEventOutboxQueued, 10)
	if err != nil || len(eventOutbox) != 1 || eventOutbox[0].EventID != event.ID {
		t.Fatalf("atomic commit 必须登记事件 outbox: outbox=%#v err=%v", eventOutbox, err)
	}
	if _, _, err := repo.CommitInvocationResume(ctx, commit); !errors.Is(err, ErrConflict) {
		t.Fatalf("已 queued 的 invocation 不应再次 commit: %v", err)
	}
	events, err = repo.ListEvents(ctx, invocation.ID, 0, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("重复 commit 不应追加事件: events=%#v err=%v", events, err)
	}

	conflictInvocation := Invocation{ID: "inv-resume-commit-conflict", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, conflictInvocation); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SaveInvocationResume(ctx, InvocationResume{ID: "existing-resume", InvocationID: conflictInvocation.ID, WaitID: "wait-existing", Name: "external_wait", ResponseJSON: []byte(`{"output":"old"}`), RequestDigest: "sha256:old", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	conflicting := commit
	conflicting.InvocationID = conflictInvocation.ID
	conflicting.Resume = InvocationResume{ID: "new-resume", InvocationID: conflictInvocation.ID, WaitID: "wait-new", Name: "external_wait", ResponseJSON: []byte(`{"output":"new"}`), RequestDigest: "sha256:new", CreatedAt: now}
	conflicting.Outbox = InvocationResumeOutbox{ID: "new-outbox", InvocationID: conflictInvocation.ID, WaitID: conflicting.Resume.WaitID, Name: conflicting.Resume.Name, ResponseJSON: conflicting.Resume.ResponseJSON, RequestDigest: conflicting.Resume.RequestDigest, Status: InvocationResumeOutboxQueued, CreatedAt: now, UpdatedAt: now, AvailableAt: now}
	conflicting.Event = AgentEvent{ID: "new-event", InvocationID: conflictInvocation.ID, Type: EventInvocationResumed, Timestamp: now, Data: map[string]any{"reason": "tool", "request_digest": conflicting.Resume.RequestDigest}}
	if _, _, err := repo.CommitInvocationResume(ctx, conflicting); !errors.Is(err, ErrConflict) {
		t.Fatalf("已有不同 handoff 时应拒绝 atomic commit: %v", err)
	}
	current, err := repo.GetInvocation(ctx, conflictInvocation.ID)
	if err != nil || current.Status != InvocationWaitingTool {
		t.Fatalf("冲突 commit 不应改变 invocation: current=%#v err=%v", current, err)
	}
	if _, err := repo.GetInvocationResumeOutbox(ctx, conflictInvocation.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("冲突 commit 不应留下 outbox: %v", err)
	}
	if events, err := repo.ListEvents(ctx, conflictInvocation.ID, 0, 10); err != nil || len(events) != 0 {
		t.Fatalf("冲突 commit 不应追加事件: events=%#v err=%v", events, err)
	}
}

func TestMemoryCommitApprovalResumeIsAtomic(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	invocation := Invocation{ID: "inv-approval-commit", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingApproval, ActiveApprovalID: "approval-commit", CreatedAt: now, UpdatedAt: now, LeaseOwner: "stale-worker"}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	toolCall := ToolCall{ID: "toolcall-approval-commit", InvocationID: invocation.ID, ConversationID: invocation.ConversationID, ToolName: "workspace_request_patch", OriginalCallID: "call-original", ConfirmationCallID: "call-confirmation", ApprovalID: "approval-commit", Status: ToolCallWaitingApproval, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateToolCall(ctx, toolCall); err != nil {
		t.Fatal(err)
	}
	approval := Approval{ID: "approval-commit", InvocationID: invocation.ID, ConversationID: invocation.ConversationID, ToolCallID: toolCall.ID, ToolName: toolCall.ToolName, OriginalCallID: toolCall.OriginalCallID, ConfirmationCallID: toolCall.ConfirmationCallID, Status: ApprovalPending, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	resume := InvocationResume{ID: "resume-approval-commit", InvocationID: invocation.ID, WaitID: approval.ConfirmationCallID, Name: "adk_tool_confirmation", ResponseJSON: []byte(`{"confirmed":true}`), RequestDigest: "sha256:approval-commit", CreatedAt: now}
	commit := ApprovalResumeCommit{
		ApprovalID: approval.ID, ApprovalFromStatus: ApprovalPending, ApprovalToStatus: ApprovalApproved,
		InvocationID: invocation.ID, InvocationFromStatus: InvocationWaitingApproval, InvocationToStatus: InvocationQueued,
		ToolCallID: toolCall.ID, ToolCallStatus: ToolCallApproved, Reason: "approved",
		Resume: resume,
		Outbox: InvocationResumeOutbox{ID: "outbox-approval-commit", InvocationID: invocation.ID, WaitID: resume.WaitID, Name: resume.Name, ResponseJSON: resume.ResponseJSON, RequestDigest: resume.RequestDigest, Status: InvocationResumeOutboxQueued, CreatedAt: now, UpdatedAt: now, AvailableAt: now},
		Events: []AgentEvent{
			{ID: "event-approval-resolved", InvocationID: invocation.ID, Type: EventApprovalResolved, Timestamp: now, Data: map[string]any{"approval_id": approval.ID, "confirmed": true}},
			{ID: "event-approval-resumed", InvocationID: invocation.ID, Type: EventInvocationResumed, Timestamp: now, Data: map[string]any{"approval_id": approval.ID, "confirmed": true, "request_digest": resume.RequestDigest}},
		},
	}
	savedApproval, savedInvocation, events, err := repo.CommitApprovalResume(ctx, commit)
	if err != nil {
		t.Fatal(err)
	}
	if savedApproval.Status != ApprovalApproved || savedInvocation.Status != InvocationQueued || savedInvocation.ActiveApprovalID != "" || savedInvocation.LeaseOwner != "" || len(events) != 2 || events[0].Sequence != 1 || events[1].Sequence != 2 {
		t.Fatalf("approval atomic commit 状态错误: approval=%#v invocation=%#v events=%#v", savedApproval, savedInvocation, events)
	}
	gotToolCall, err := repo.GetToolCall(ctx, toolCall.ID)
	if err != nil || gotToolCall.Status != ToolCallApproved {
		t.Fatalf("ToolCall 未与 approval 同提交更新: toolcall=%#v err=%v", gotToolCall, err)
	}
	if _, err := repo.GetInvocationResume(ctx, invocation.ID); err != nil {
		t.Fatalf("approval atomic commit 未保存 handoff: %v", err)
	}
	if _, err := repo.GetInvocationResumeOutbox(ctx, invocation.ID); err != nil {
		t.Fatalf("approval atomic commit 未保存 outbox: %v", err)
	}
	eventOutbox, err := repo.ListRuntimeEventOutbox(ctx, invocation.ID, RuntimeEventOutboxQueued, 10)
	if err != nil || len(eventOutbox) != len(events) {
		t.Fatalf("approval atomic commit 必须为所有审计事件登记 outbox: outbox=%#v err=%v", eventOutbox, err)
	}
	if _, _, _, err := repo.CommitApprovalResume(ctx, commit); !errors.Is(err, ErrConflict) {
		t.Fatalf("已 queued/approved 的 approval 不应重复 commit: %v", err)
	}
}

func TestResumeContentFallsBackToOutboxWhenPrivateHandoffIsMissing(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-resume-outbox-fallback", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueInvocationResumeOutbox(context.Background(), InvocationResumeOutbox{
		InvocationID: invocation.ID, WaitID: "wait-fallback", Name: "external_wait", ResponseJSON: []byte(`{"output":"from outbox"}`), RequestDigest: "sha256:fallback", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo}
	content, err := coordinator.resumeContentForInvocation(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if content == nil || len(content.Parts) != 1 || content.Parts[0].FunctionResponse == nil || content.Parts[0].FunctionResponse.ID != "wait-fallback" {
		t.Fatalf("应从 outbox 恢复 FunctionResponse: %#v", content)
	}
	if got := content.Parts[0].FunctionResponse.Response["output"]; got != "from outbox" {
		t.Fatalf("outbox payload 未恢复: %#v", content.Parts[0].FunctionResponse.Response)
	}
	if err := repo.DeleteInvocationResumeOutbox(context.Background(), invocation.ID); err != nil {
		t.Fatal(err)
	}
	if remaining, err := coordinator.resumeContentForInvocation(context.Background(), invocation.ID); err != nil || remaining != nil {
		t.Fatalf("删除 outbox 后应无可恢复内容: content=%#v err=%v", remaining, err)
	}
}

func TestResumeContentDoesNotReplayCompletedOutboxWithStaleHandoff(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-resume-outbox-completed", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	outbox, err := repo.EnqueueInvocationResumeOutbox(context.Background(), InvocationResumeOutbox{
		InvocationID: invocation.ID, WaitID: "wait-completed", Name: "external_wait", ResponseJSON: []byte(`{"ok":true}`), RequestDigest: "sha256:completed", AvailableAt: now, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := repo.ClaimInvocationResumeOutbox(context.Background(), invocation.ID, "worker", now, time.Minute); err != nil || !claimed {
		t.Fatalf("准备完成态 outbox 的 claim 失败: claimed=%v err=%v", claimed, err)
	}
	if completed, err := repo.CompleteInvocationResumeOutbox(context.Background(), invocation.ID, "worker", now.Add(time.Second)); err != nil || !completed {
		t.Fatalf("准备完成态 outbox 的 complete 失败: completed=%v err=%v", completed, err)
	}
	if _, err := repo.SaveInvocationResume(context.Background(), InvocationResume{
		ID: "stale-handoff", InvocationID: invocation.ID, WaitID: outbox.WaitID, Name: outbox.Name,
		ResponseJSON: outbox.ResponseJSON, RequestDigest: outbox.RequestDigest, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo}
	content, err := coordinator.resumeContentForInvocation(context.Background(), invocation.ID)
	if err != nil || content != nil {
		t.Fatalf("completed outbox 即使存在旧 handoff 也不能重放: content=%#v err=%v", content, err)
	}
}
