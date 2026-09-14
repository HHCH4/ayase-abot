package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type workflowContinuationFixture struct {
	repo        *MemoryRepository
	coordinator *Coordinator
	source      Invocation
	plan        TaskPlan
}

func newWorkflowContinuationFixture(t *testing.T, id string) workflowContinuationFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	source := Invocation{
		ID: "source-" + id, UserID: "user", BotID: "bot", ConversationID: "conversation-" + id,
		WorkspaceID: "workspace", TargetPath: "/workspace/project", SessionID: "session-" + id,
		ProviderID: "provider", ModelID: "model", ConfigSnapshot: `{"version":1,"provider":"provider"}`,
		Message: "原始任务正文不得复制", Status: InvocationFailed,
		Error:     "runtime_interrupted: 服务重启时运行中的 invocation 没有可安全恢复的 checkpoint",
		CreatedAt: now, UpdatedAt: now,
	}
	repo := NewMemoryRepository()
	if err := repo.CreateInvocation(ctx, source); err != nil {
		t.Fatal(err)
	}
	plan := TaskPlan{
		ID: "plan-" + source.ID, InvocationID: source.ID, Revision: 3, Status: PlanBlocked,
		Blocker: "runtime_interrupted: 无法确认活动步骤副作用", CreatedAt: now, UpdatedAt: now,
		Steps: []PlanStep{
			{ID: "inspect", Title: "检查", Status: PlanStepCompleted, Evidence: []EvidenceRef{{Kind: "event", Ref: "inspect-done"}}, UpdatedAt: now},
			{ID: "edit", Title: "修改", Status: PlanStepBlocked, DependsOn: []string{"inspect"}, Blocker: "runtime_interrupted: 无法确认活动步骤副作用", RepairAttempts: 2, UpdatedAt: now},
			{ID: "verify", Title: "验证", Status: PlanStepPending, DependsOn: []string{"edit"}, UpdatedAt: now},
		},
	}
	if err := repo.CreateTaskPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, AgentEvent{ID: "recovery-plan-" + id, InvocationID: source.ID, Type: EventPlanUpdated, Timestamp: now, Data: map[string]any{"reason": "runtime_recovery"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, AgentEvent{ID: "recovery-failed-" + id, InvocationID: source.ID, Type: EventInvocationFailed, Timestamp: now, Data: map[string]any{"reason": "runtime_recovery", "error": source.Error}}); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{
		repo: repo, started: true, rootCtx: context.Background(),
		runs: make(map[string]runHandle), subscribers: make(map[string]map[chan AgentEvent]struct{}),
	}
	return workflowContinuationFixture{repo: repo, coordinator: coordinator, source: source, plan: plan}
}

func TestWorkflowContinuationRetryIsImmutableAndIdempotent(t *testing.T) {
	fixture := newWorkflowContinuationFixture(t, "retry")
	ctx := context.Background()
	request := WorkflowContinuationRequest{ExpectedPlanRevision: 3, Decision: WorkflowContinuationRetryStep, StepID: "edit", Message: "继续剩余步骤", IdempotencyKey: "continue-retry"}
	child, err := fixture.coordinator.ContinueInterruptedWorkflow(ctx, fixture.source.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	if child.ID == fixture.source.ID || child.ParentInvocationID != fixture.source.ID || child.Status != InvocationQueued || child.Message != request.Message {
		t.Fatalf("child metadata 错误: %#v", child)
	}
	if child.IdempotencyKey == request.IdempotencyKey || child.IdempotencyKey == "" {
		t.Fatalf("child 不应直接暴露/复用用户幂等键: %#v", child)
	}
	storedSource, err := fixture.repo.GetInvocation(ctx, fixture.source.ID)
	if err != nil || storedSource.Status != InvocationFailed || storedSource.Message != fixture.source.Message {
		t.Fatalf("source 必须保持 terminal immutable: %#v err=%v", storedSource, err)
	}
	childPlan, err := fixture.repo.GetTaskPlan(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if childPlan.ID != "plan-"+child.ID || childPlan.Revision != 1 || childPlan.Status != PlanPending || childPlan.CurrentStepID != "" {
		t.Fatalf("retry child plan metadata 错误: %#v", childPlan)
	}
	if childPlan.Steps[0].Status != PlanStepCompleted || len(childPlan.Steps[0].Evidence) != 1 || childPlan.Steps[1].Status != PlanStepPending || childPlan.Steps[1].Blocker != "" || childPlan.Steps[1].RepairAttempts != 2 {
		t.Fatalf("retry 应保留已完成证据并清除目标 blocker: %#v", childPlan)
	}
	sourceEvents, err := fixture.repo.ListEvents(ctx, fixture.source.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	childEvents, err := fixture.repo.ListEvents(ctx, child.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(sourceEvents) != 3 || len(childEvents) != 1 || sourceEvents[2].Type != EventWorkflowContinuationRequested || childEvents[0].Type != EventInvocationQueued {
		t.Fatalf("continuation 审计事件错误: source=%#v child=%#v", sourceEvents, childEvents)
	}
	repeated, err := fixture.coordinator.ContinueInterruptedWorkflow(ctx, fixture.source.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.ID != child.ID {
		t.Fatalf("重复请求必须返回同一 child: first=%s repeated=%s", child.ID, repeated.ID)
	}
	sourceEvents, _ = fixture.repo.ListEvents(ctx, fixture.source.ID, 0, 20)
	childEvents, _ = fixture.repo.ListEvents(ctx, child.ID, 0, 20)
	if len(sourceEvents) != 3 || len(childEvents) != 1 {
		t.Fatalf("重复请求不得追加事件: source=%d child=%d", len(sourceEvents), len(childEvents))
	}
}

func TestWorkflowContinuationCompleteRequiresEvidenceAndDoesNotExecute(t *testing.T) {
	fixture := newWorkflowContinuationFixture(t, "complete")
	ctx := context.Background()
	withoutEvidence := WorkflowContinuationRequest{ExpectedPlanRevision: 3, Decision: WorkflowContinuationCompleteStep, StepID: "edit", IdempotencyKey: "complete-missing"}
	if _, err := fixture.coordinator.ContinueInterruptedWorkflow(ctx, fixture.source.ID, withoutEvidence); !errors.Is(err, ErrInvalidWorkflowContinuation) {
		t.Fatalf("缺失 evidence 应拒绝: %v", err)
	}
	if invocations, err := fixture.repo.ListInvocations(ctx, "", nil); err != nil || len(invocations) != 1 {
		t.Fatalf("非法 complete 不得创建 child: %#v err=%v", invocations, err)
	}
	request := WorkflowContinuationRequest{ExpectedPlanRevision: 3, Decision: WorkflowContinuationCompleteStep, StepID: "edit", IdempotencyKey: "complete-ok", Evidence: []EvidenceRef{{Kind: "verification", Ref: "external-check-42", Summary: "外部检查已确认"}}}
	child, err := fixture.coordinator.ContinueInterruptedWorkflow(ctx, fixture.source.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fixture.repo.GetTaskPlan(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Steps[1].Status != PlanStepCompleted || plan.Steps[1].Blocker != "" || len(plan.Steps[1].Evidence) != 1 || plan.Steps[1].Evidence[0].Ref != "external-check-42" || plan.Steps[2].Status != PlanStepPending {
		t.Fatalf("complete child plan 错误: %#v", plan)
	}
	if plan.Status != PlanPending {
		t.Fatalf("后续 verify 尚未执行时 plan 应保持 pending: %#v", plan)
	}
	if events, err := fixture.repo.ListEvents(ctx, child.ID, 0, 20); err != nil || len(events) != 1 {
		t.Fatalf("complete 不应执行工具，只应有 queued 事件: %#v err=%v", events, err)
	}
}

func TestWorkflowContinuationRejectsStaleOrUnprovenSourceWithoutWrites(t *testing.T) {
	ctx := context.Background()
	fixture := newWorkflowContinuationFixture(t, "gates")
	if _, err := fixture.coordinator.ContinueInterruptedWorkflow(ctx, fixture.source.ID, WorkflowContinuationRequest{ExpectedPlanRevision: 2, Decision: WorkflowContinuationRetryStep, StepID: "edit", IdempotencyKey: "stale"}); !errors.Is(err, ErrWorkflowContinuationConflict) {
		t.Fatalf("过期 revision 应冲突: %v", err)
	}
	active := Invocation{ID: "active-gates", UserID: fixture.source.UserID, ConversationID: fixture.source.ConversationID, SessionID: "active-session", Status: InvocationQueued, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := fixture.repo.CreateInvocation(ctx, active); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.coordinator.ContinueInterruptedWorkflow(ctx, fixture.source.ID, WorkflowContinuationRequest{ExpectedPlanRevision: 3, Decision: WorkflowContinuationRetryStep, StepID: "edit", IdempotencyKey: "active"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("同 conversation active invocation 应冲突: %v", err)
	}
	fixture2 := newWorkflowContinuationFixture(t, "no-proof")
	fixture2.repo.mu.Lock()
	fixture2.eventsForTestDeleteRecoveryFailure()
	fixture2.repo.mu.Unlock()
	if _, err := fixture2.coordinator.ContinueInterruptedWorkflow(ctx, fixture2.source.ID, WorkflowContinuationRequest{ExpectedPlanRevision: 3, Decision: WorkflowContinuationRetryStep, StepID: "edit", IdempotencyKey: "no-proof"}); !errors.Is(err, ErrWorkflowContinuationSource) {
		t.Fatalf("缺少 recovery proof 应拒绝: %v", err)
	}
	invocations, err := fixture2.repo.ListInvocations(ctx, "", nil)
	if err != nil || len(invocations) != 1 {
		t.Fatalf("gate 失败不得创建 child: %#v err=%v", invocations, err)
	}
}

func (f *workflowContinuationFixture) eventsForTestDeleteRecoveryFailure() {
	events := f.repo.events[f.source.ID]
	kept := events[:0]
	for _, event := range events {
		if event.Type != EventInvocationFailed {
			kept = append(kept, event)
		}
	}
	f.repo.events[f.source.ID] = kept
}

func TestWorkflowContinuationMemoryConcurrentIdempotencyHasOneWinner(t *testing.T) {
	fixture := newWorkflowContinuationFixture(t, "concurrent")
	ctx := context.Background()
	request := WorkflowContinuationRequest{ExpectedPlanRevision: 3, Decision: WorkflowContinuationRetryStep, StepID: "edit", IdempotencyKey: "same-key"}
	const callers = 12
	results := make(chan Invocation, callers)
	errorsCh := make(chan error, callers)
	var wait sync.WaitGroup
	for index := 0; index < callers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			child, err := fixture.coordinator.ContinueInterruptedWorkflow(ctx, fixture.source.ID, request)
			if err != nil {
				errorsCh <- err
				return
			}
			results <- child
		}()
	}
	wait.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		t.Fatalf("并发相同 continuation 不应失败: %v", err)
	}
	var first Invocation
	for child := range results {
		if first.ID == "" {
			first = child
		}
		if child.ID != first.ID {
			t.Fatalf("并发请求返回不同 child: first=%s got=%s", first.ID, child.ID)
		}
	}
	items, err := fixture.repo.ListInvocations(ctx, "", nil)
	if err != nil || len(items) != 2 {
		t.Fatalf("并发提交必须只有 source+one child: %#v err=%v", items, err)
	}
	if events, err := fixture.repo.ListEvents(ctx, fixture.source.ID, 0, 20); err != nil || len(events) != 3 {
		t.Fatalf("并发提交 source 审计事件应只有一条 continuation: %#v err=%v", events, err)
	}
}
