package sqlite

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

func sqliteInterruptedInvocationFixture(t *testing.T, repo agentruntime.Repository, id string) (agentruntime.Invocation, agentruntime.TaskPlan) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	invocation := agentruntime.Invocation{
		ID: id, UserID: "user", ConversationID: id, SessionID: id,
		Status: agentruntime.InvocationRunning, LeaseOwner: "stale-worker", CreatedAt: now, UpdatedAt: now,
	}
	expires := now.Add(-time.Minute)
	invocation.LeaseExpiresAt = &expires
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	plan := agentruntime.TaskPlan{
		ID: "plan-" + id, InvocationID: id, Revision: 1, Status: agentruntime.PlanInProgress,
		CurrentStepID: "edit", Steps: []agentruntime.PlanStep{
			{ID: "inspect", Title: "检查", Status: agentruntime.PlanStepCompleted, Evidence: []agentruntime.EvidenceRef{{Kind: "event", Ref: "inspect-event"}}},
			{ID: "edit", Title: "编辑", Status: agentruntime.PlanStepInProgress},
			{ID: "verify", Title: "验证", Status: agentruntime.PlanStepPending, DependsOn: []string{"edit"}},
		}, CreatedAt: now, UpdatedAt: now,
	}
	planRepo, ok := repo.(agentruntime.TaskPlanRepository)
	if !ok {
		t.Fatal("SQLite runtime repository 未实现 TaskPlanRepository")
	}
	if err := planRepo.CreateTaskPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	return invocation, plan
}

func sqliteInterruptedCommitFixture(t *testing.T, repo agentruntime.Repository, id string) (agentruntime.Invocation, agentruntime.TaskPlan, agentruntime.InterruptedInvocationCommit) {
	t.Helper()
	invocation, plan := sqliteInterruptedInvocationFixture(t, repo, id)
	blocked, err := plan.BlockStep(plan.CurrentStepID, "runtime_interrupted: 服务重启时无法确认上次执行是否产生副作用", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	commit := agentruntime.InterruptedInvocationCommit{
		InvocationID: id, FromStatus: agentruntime.InvocationRunning, ToStatus: agentruntime.InvocationFailed,
		Message: "runtime_interrupted: 服务重启时运行中的 invocation 没有可安全恢复的 checkpoint", Plan: &blocked,
		Events: []agentruntime.AgentEvent{
			{ID: "recovery-plan", Type: agentruntime.EventPlanUpdated, Data: map[string]any{"reason": "runtime_recovery"}},
			{ID: "recovery-terminal", Type: agentruntime.EventInvocationFailed, Data: map[string]any{"error": "runtime_interrupted", "reason": "runtime_recovery"}},
		},
	}
	return invocation, plan, commit
}

func TestRuntimeRepositoryCommitsInterruptedInvocationAtomically(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	repo := store.RuntimeRepository()
	invocation, initialPlan, commit := sqliteInterruptedCommitFixture(t, repo, "inv-recovery-sqlite")
	commitRepo, ok := repo.(agentruntime.InterruptedInvocationCommitRepository)
	if !ok {
		t.Fatal("SQLite runtime repository 未实现 InterruptedInvocationCommitRepository")
	}
	savedInvocation, savedPlan, savedEvents, err := commitRepo.CommitInterruptedInvocation(ctx, commit)
	if err != nil {
		t.Fatal(err)
	}
	if savedInvocation.Status != agentruntime.InvocationFailed || savedInvocation.LeaseOwner != "" || savedInvocation.LeaseExpiresAt != nil || !strings.Contains(savedInvocation.Error, "runtime_interrupted") {
		t.Fatalf("SQLite 中断 Invocation 未安全收口: %#v", savedInvocation)
	}
	if savedPlan == nil || savedPlan.Revision != initialPlan.Revision+1 || savedPlan.Status != agentruntime.PlanBlocked || savedPlan.CurrentStepID != "" || !strings.Contains(savedPlan.Blocker, "runtime_interrupted") {
		t.Fatalf("SQLite 活动步骤未被阻断: %#v", savedPlan)
	}
	if len(savedEvents) != 2 || savedEvents[0].Sequence != 1 || savedEvents[1].Sequence != 2 || savedEvents[0].Type != agentruntime.EventPlanUpdated || savedEvents[1].Type != agentruntime.EventInvocationFailed {
		t.Fatalf("SQLite 中断恢复事件顺序错误: %#v", savedEvents)
	}
	persistedInvocation, err := repo.GetInvocation(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	planRepo := repo.(agentruntime.TaskPlanRepository)
	persistedPlan, err := planRepo.GetTaskPlan(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persistedInvocation.Status != agentruntime.InvocationFailed || persistedPlan.Status != agentruntime.PlanBlocked || persistedPlan.Revision != 2 {
		t.Fatalf("SQLite 提交后的持久化状态错误: invocation=%#v plan=%#v", persistedInvocation, persistedPlan)
	}
	events, err := repo.ListEvents(ctx, invocation.ID, 0, 20)
	if err != nil || len(events) != 2 || events[0].Type != agentruntime.EventPlanUpdated || events[1].Type != agentruntime.EventInvocationFailed {
		t.Fatalf("SQLite 提交后的事件错误: %#v err=%v", events, err)
	}
}

func TestRuntimeRepositoryRollsBackInterruptedInvocationOnEventConflict(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	repo := store.RuntimeRepository()
	invocation, _, commit := sqliteInterruptedCommitFixture(t, repo, "inv-recovery-sqlite-rollback")
	if _, err := repo.AppendEvent(ctx, agentruntime.AgentEvent{ID: "existing-event", InvocationID: invocation.ID, Type: agentruntime.EventRuntimeNotice}); err != nil {
		t.Fatal(err)
	}
	commit.Events[0].ID = "existing-event"
	beforeInvocation, err := repo.GetInvocation(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	planRepo := repo.(agentruntime.TaskPlanRepository)
	beforePlan, err := planRepo.GetTaskPlan(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	commitRepo := repo.(agentruntime.InterruptedInvocationCommitRepository)
	if _, _, _, err := commitRepo.CommitInterruptedInvocation(ctx, commit); !errors.Is(err, agentruntime.ErrConflict) {
		t.Fatalf("重复事件 ID 应拒绝提交: %v", err)
	}
	afterInvocation, err := repo.GetInvocation(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterPlan, err := planRepo.GetTaskPlan(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterInvocation, beforeInvocation) || !reflect.DeepEqual(afterPlan, beforePlan) {
		t.Fatalf("SQLite 冲突后不得部分写入: invocation before=%#v after=%#v plan before=%#v after=%#v", beforeInvocation, afterInvocation, beforePlan, afterPlan)
	}
	events, err := repo.ListEvents(ctx, invocation.ID, 0, 20)
	if err != nil || len(events) != 1 || events[0].ID != "existing-event" {
		t.Fatalf("SQLite 冲突后事件不应追加: %#v err=%v", events, err)
	}
}

func TestRuntimeRepositoryRejectsMalformedInterruptedEventsWithoutMutation(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	repo := store.RuntimeRepository()
	invocation, _, commit := sqliteInterruptedCommitFixture(t, repo, "inv-recovery-sqlite-malformed")
	commit.Events = []agentruntime.AgentEvent{{ID: "wrong-terminal", Type: agentruntime.EventInvocationCancelled}}
	beforeInvocation, err := repo.GetInvocation(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	planRepo := repo.(agentruntime.TaskPlanRepository)
	beforePlan, err := planRepo.GetTaskPlan(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	commitRepo := repo.(agentruntime.InterruptedInvocationCommitRepository)
	if _, _, _, err := commitRepo.CommitInterruptedInvocation(ctx, commit); !errors.Is(err, agentruntime.ErrConflict) {
		t.Fatalf("错误终态事件应返回 ErrConflict: %v", err)
	}
	afterInvocation, err := repo.GetInvocation(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterPlan, err := planRepo.GetTaskPlan(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterInvocation, beforeInvocation) || !reflect.DeepEqual(afterPlan, beforePlan) {
		t.Fatalf("非法事件不得改变 SQLite 状态: invocation before=%#v after=%#v plan before=%#v after=%#v", beforeInvocation, afterInvocation, beforePlan, afterPlan)
	}
}
