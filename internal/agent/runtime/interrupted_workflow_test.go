package runtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func interruptedInvocationFixture(t *testing.T, repo *MemoryRepository, id string) (Invocation, TaskPlan) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	invocation := Invocation{
		ID: id, UserID: "user", ConversationID: id, SessionID: id,
		Status: InvocationRunning, LeaseOwner: "stale-worker", CreatedAt: now, UpdatedAt: now,
	}
	expires := now.Add(-time.Minute)
	invocation.LeaseExpiresAt = &expires
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	plan := TaskPlan{
		ID: "plan-" + id, InvocationID: id, Revision: 1, Status: PlanInProgress,
		CurrentStepID: "edit", Steps: []PlanStep{
			{ID: "inspect", Title: "检查", Status: PlanStepCompleted, Evidence: []EvidenceRef{{Kind: "event", Ref: "inspect-event"}}},
			{ID: "edit", Title: "编辑", Status: PlanStepInProgress},
			{ID: "verify", Title: "验证", Status: PlanStepPending, DependsOn: []string{"edit"}},
		}, CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.CreateTaskPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	return invocation, plan
}

func interruptedCommitFixture(t *testing.T, repo *MemoryRepository, id string) (Invocation, TaskPlan, InterruptedInvocationCommit) {
	t.Helper()
	invocation, plan := interruptedInvocationFixture(t, repo, id)
	blocked, err := plan.BlockStep(plan.CurrentStepID, "runtime_interrupted: 服务重启时无法确认上次执行是否产生副作用", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	commit := InterruptedInvocationCommit{
		InvocationID: id, FromStatus: InvocationRunning, ToStatus: InvocationFailed,
		Message: "runtime_interrupted: 服务重启时运行中的 invocation 没有可安全恢复的 checkpoint", Plan: &blocked,
		Events: []AgentEvent{
			{ID: "recovery-plan", Type: EventPlanUpdated, Data: map[string]any{"reason": "runtime_recovery"}},
			{ID: "recovery-terminal", Type: EventInvocationFailed, Data: map[string]any{"error": "runtime_interrupted", "reason": "runtime_recovery"}},
		},
	}
	return invocation, plan, commit
}

func TestReconcileInterruptedBlocksActivePlanAtomically(t *testing.T) {
	repo := NewMemoryRepository()
	invocation, initialPlan := interruptedInvocationFixture(t, repo, "inv-recovery-plan")
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	ctx := context.Background()
	if err := coordinator.reconcileInterrupted(ctx); err != nil {
		t.Fatal(err)
	}

	current, err := repo.GetInvocation(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != InvocationFailed || current.LeaseOwner != "" || current.LeaseExpiresAt != nil || !strings.Contains(current.Error, "runtime_interrupted") {
		t.Fatalf("中断 Invocation 未安全收口: %#v", current)
	}
	blocked, err := repo.GetTaskPlan(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Revision != initialPlan.Revision+1 || blocked.Status != PlanBlocked || blocked.CurrentStepID != "" || !strings.Contains(blocked.Blocker, "runtime_interrupted") {
		t.Fatalf("活动步骤未被阻断: %#v", blocked)
	}
	if step := blocked.CurrentStep(); step != nil {
		t.Fatalf("blocked 计划不应保留 current step: %#v", step)
	}
	for _, step := range blocked.Steps {
		if step.ID == "edit" && (step.Status != PlanStepBlocked || !strings.Contains(step.Blocker, "runtime_interrupted")) {
			t.Fatalf("活动步骤 blocker 不正确: %#v", step)
		}
	}
	events, err := repo.ListEvents(ctx, invocation.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 3 || events[0].Type != EventPlanUpdated || events[1].Type != EventInvocationFailed || events[2].Type != EventRuntimeSnapshot || events[0].Sequence != 1 || events[1].Sequence != 2 || events[2].Sequence != 3 {
		t.Fatalf("中断恢复事件顺序错误: %#v", events)
	}
	if _, ok := events[0].Data["plan"].(TaskPlan); !ok {
		t.Fatalf("plan.updated 应包含 Runtime 规范化后的计划: %#v", events[0].Data)
	}

	// A second scanner pass is idempotent: terminal Invocations are no longer
	// listed and must not receive duplicate recovery events.
	if err := coordinator.reconcileInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	repeated, err := repo.ListEvents(ctx, invocation.ID, 0, 20)
	if err != nil || len(repeated) != len(events) {
		t.Fatalf("重复恢复不应追加事件: %#v err=%v", repeated, err)
	}
}

func TestReconcileInterruptedCancellingInvocationBlocksPlan(t *testing.T) {
	repo := NewMemoryRepository()
	invocation, _ := interruptedInvocationFixture(t, repo, "inv-recovery-cancelling")
	invocation.Status = InvocationCancelling
	if err := repo.UpdateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	if err := coordinator.reconcileInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, err := repo.GetInvocation(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != InvocationCancelled || current.LeaseOwner != "" || current.LeaseExpiresAt != nil {
		t.Fatalf("cancelling Invocation 未安全收口: %#v", current)
	}
	plan, err := repo.GetTaskPlan(context.Background(), invocation.ID)
	if err != nil || plan.Status != PlanBlocked || plan.CurrentStepID != "" {
		t.Fatalf("cancelling Invocation 的活动步骤应阻断: %#v err=%v", plan, err)
	}
	events, err := repo.ListEvents(context.Background(), invocation.ID, 0, 20)
	if err != nil || len(events) < 3 || events[0].Type != EventPlanUpdated || events[1].Type != EventInvocationCancelled || events[2].Type != EventRuntimeSnapshot {
		t.Fatalf("cancelling 恢复事件错误: %#v err=%v", events, err)
	}
}

func TestMemoryCommitInterruptedInvocationRejectsMalformedEvents(t *testing.T) {
	cases := []struct {
		name   string
		events []AgentEvent
	}{
		{name: "empty", events: nil},
		{name: "missing_type", events: []AgentEvent{{ID: "missing-type"}}},
		{name: "missing_terminal", events: []AgentEvent{{ID: "plan-only", Type: EventPlanUpdated}}},
		{name: "wrong_terminal", events: []AgentEvent{{ID: "wrong-terminal", Type: EventInvocationCancelled}}},
		{name: "missing_plan_audit", events: []AgentEvent{{ID: "terminal-only", Type: EventInvocationFailed}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			repo := NewMemoryRepository()
			invocation, plan, commit := interruptedCommitFixture(t, repo, "inv-recovery-malformed-"+testCase.name)
			beforeInvocation, err := repo.GetInvocation(context.Background(), invocation.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforePlan, err := repo.GetTaskPlan(context.Background(), invocation.ID)
			if err != nil {
				t.Fatal(err)
			}
			commit.Events = testCase.events
			if _, _, _, err := repo.CommitInterruptedInvocation(context.Background(), commit); !errors.Is(err, ErrConflict) {
				t.Fatalf("非法事件应返回 ErrConflict: %v", err)
			}
			afterInvocation, err := repo.GetInvocation(context.Background(), invocation.ID)
			if err != nil {
				t.Fatal(err)
			}
			afterPlan, err := repo.GetTaskPlan(context.Background(), invocation.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(afterInvocation, beforeInvocation) || !reflect.DeepEqual(afterPlan, beforePlan) || plan.Revision != beforePlan.Revision {
				t.Fatalf("非法事件不得改变 Memory 状态: invocation before=%#v after=%#v plan before=%#v after=%#v", beforeInvocation, afterInvocation, beforePlan, afterPlan)
			}
		})
	}
}

func TestMemoryCommitInterruptedInvocationRollsBackOnEventConflict(t *testing.T) {
	repo := NewMemoryRepository()
	invocation, _, commit := interruptedCommitFixture(t, repo, "inv-recovery-memory-rollback")
	if _, err := repo.AppendEvent(context.Background(), AgentEvent{ID: "existing-event", InvocationID: invocation.ID, Type: EventRuntimeNotice}); err != nil {
		t.Fatal(err)
	}
	commit.Events[0].ID = "existing-event"
	beforeInvocation, err := repo.GetInvocation(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforePlan, err := repo.GetTaskPlan(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := repo.CommitInterruptedInvocation(context.Background(), commit); !errors.Is(err, ErrConflict) {
		t.Fatalf("重复事件 ID 应拒绝提交: %v", err)
	}
	afterInvocation, err := repo.GetInvocation(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterPlan, err := repo.GetTaskPlan(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterInvocation, beforeInvocation) || !reflect.DeepEqual(afterPlan, beforePlan) {
		t.Fatalf("Memory 冲突后不得部分写入: invocation before=%#v after=%#v plan before=%#v after=%#v", beforeInvocation, afterInvocation, beforePlan, afterPlan)
	}
	events, err := repo.ListEvents(context.Background(), invocation.ID, 0, 20)
	if err != nil || len(events) != 1 || events[0].ID != "existing-event" {
		t.Fatalf("Memory 冲突后事件不应追加: %#v err=%v", events, err)
	}
}
