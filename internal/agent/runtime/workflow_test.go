package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"Abot/internal/workspace"
)

func TestTaskPlanRunnableStepsAndTransitions(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	plan := TaskPlan{ID: "plan-runnable", InvocationID: "inv-runnable", Revision: 1, Status: PlanPending, Steps: []PlanStep{
		{ID: "verify", Title: "验证", Status: PlanStepPending, DependsOn: []string{"edit"}},
		{ID: "edit", Title: "修改", Status: PlanStepPending},
	}, CreatedAt: now, UpdatedAt: now}
	first, err := plan.NextRunnableStep()
	if err != nil || first.ID != "edit" {
		t.Fatalf("应只返回依赖满足的 edit: %#v err=%v", first, err)
	}
	started, err := plan.StartNextStep(now)
	if err != nil {
		t.Fatal(err)
	}
	if started.CurrentStepID != "edit" || started.Status != PlanInProgress {
		t.Fatalf("应先启动 edit: %#v", started)
	}
	if _, err := started.StartNextStep(now.Add(time.Second)); !errors.Is(err, ErrPlanStepActive) {
		t.Fatalf("已有 active step 时应阻止并行启动: %v", err)
	}
	completed, err := started.CompleteStep("edit", []EvidenceRef{{Kind: "verification", Ref: "verify-edit"}}, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	next, err := completed.NextRunnableStep()
	if err != nil || next.ID != "verify" {
		t.Fatalf("edit 完成后应解锁 verify: %#v err=%v", next, err)
	}
	if _, err := completed.StartStep("missing", now); !errors.Is(err, ErrPlanStepNotFound) {
		t.Fatalf("缺失步骤应被拒绝: %v", err)
	}
	if _, err := completed.StartStep("edit", now); !errors.Is(err, ErrPlanStepNotReady) {
		t.Fatalf("已完成步骤不应再次启动: %v", err)
	}
}

func TestDefaultWorkflowPlanIsConservativeAndDependencyOrdered(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	mutation, ok := defaultWorkflowPlan(Invocation{ID: "inv-default", WorkspaceID: "workspace", CreatedAt: now}, TaskContract{InvocationID: "inv-default", TaskType: TaskBuild, MutationAllowed: true, ValidationRequired: true})
	if !ok || len(mutation.Steps) != 3 || mutation.Steps[0].ID != "inspect" || mutation.Steps[1].ID != "edit" || mutation.Steps[2].ID != "verify" {
		t.Fatalf("变更任务应生成 inspect->edit->verify: %#v ok=%v", mutation, ok)
	}
	if len(mutation.Steps[1].DependsOn) != 1 || mutation.Steps[1].DependsOn[0] != "inspect" || len(mutation.Steps[2].DependsOn) != 1 || mutation.Steps[2].DependsOn[0] != "edit" {
		t.Fatalf("默认计划依赖不正确: %#v", mutation.Steps)
	}
	if err := mutation.Validate(); err != nil {
		t.Fatalf("默认变更计划应通过验证: %v", err)
	}
	inspect, ok := defaultWorkflowPlan(Invocation{ID: "inv-inspect", WorkspaceID: "workspace", CreatedAt: now}, TaskContract{InvocationID: "inv-inspect", TaskType: TaskInspect})
	if !ok || len(inspect.Steps) != 1 || inspect.Steps[0].ID != "inspect" {
		t.Fatalf("工作区只读任务应只生成 inspect: %#v ok=%v", inspect, ok)
	}
	if _, ok := defaultWorkflowPlan(Invocation{ID: "inv-answer", WorkspaceID: "workspace", CreatedAt: now}, TaskContract{InvocationID: "inv-answer", TaskType: TaskAnswer}); ok {
		t.Fatal("普通 answer 不应强行生成 coding workflow")
	}
	if _, ok := defaultWorkflowPlan(Invocation{ID: "inv-inspect-no-workspace", CreatedAt: now}, TaskContract{InvocationID: "inv-inspect-no-workspace", TaskType: TaskInspect}); ok {
		t.Fatal("没有工作区的 inspect 不应伪造文件检查步骤")
	}
}

func TestTaskPlanRejectsFinishedStepWithUnfinishedDependency(t *testing.T) {
	plan := TaskPlan{ID: "plan-invalid-dependency", InvocationID: "inv", Revision: 1, Status: PlanInProgress, CurrentStepID: "child", Steps: []PlanStep{
		{ID: "parent", Title: "父步骤", Status: PlanStepPending},
		{ID: "child", Title: "子步骤", Status: PlanStepInProgress, DependsOn: []string{"parent"}},
	}}
	if err := plan.Validate(); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("in_progress 步骤依赖 pending 应拒绝: %v", err)
	}
}

func TestTaskPlanStepCountIsBounded(t *testing.T) {
	steps := make([]PlanStep, MaxTaskPlanSteps+1)
	for index := range steps {
		steps[index] = PlanStep{ID: "step-" + string(rune('a'+index%26)) + "-" + fmt.Sprint(index), Title: "步骤", Status: PlanStepPending}
	}
	plan := TaskPlan{ID: "plan-too-large", InvocationID: "inv", Revision: 1, Status: PlanPending, Steps: steps}
	if !errors.Is(plan.Validate(), ErrInvalidPlan) {
		t.Fatalf("计划步骤数超过边界应拒绝")
	}
}

func newWorkflowTestCoordinator(t *testing.T, plan TaskPlan) *Coordinator {
	t.Helper()
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: plan.InvocationID, UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateTaskPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	return &Coordinator{repo: repo, started: true, rootCtx: context.Background(), subscribers: make(map[string]map[chan AgentEvent]struct{})}
}

func TestExecuteTaskPlanRunsDependenciesAndPersistsEvidence(t *testing.T) {
	now := time.Now().UTC()
	plan := TaskPlan{ID: "plan-execute", InvocationID: "inv-execute", Revision: 1, Status: PlanPending, Steps: []PlanStep{
		{ID: "verify", Title: "验证", Status: PlanStepPending, DependsOn: []string{"edit"}},
		{ID: "edit", Title: "修改", Status: PlanStepPending},
	}, CreatedAt: now, UpdatedAt: now}
	coordinator := newWorkflowTestCoordinator(t, plan)
	var executed []string
	result, err := coordinator.ExecuteTaskPlan(context.Background(), plan.InvocationID, PlanExecutionPolicy{MaxStepExecutions: 10, MaxRuntimeRetries: 0, MaxVerificationRepairs: 0}, func(_ context.Context, step PlanStep) (PlanStepExecutionResult, error) {
		executed = append(executed, step.ID)
		return PlanStepExecutionResult{Evidence: []EvidenceRef{{Kind: "event", Ref: "e-" + step.ID}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(executed, []string{"edit", "verify"}) {
		t.Fatalf("步骤执行顺序不稳定: %#v", executed)
	}
	if result.StepExecutions != 2 || result.Repairs != 0 || result.Plan.Status != PlanCompleted {
		t.Fatalf("执行结果=%#v", result)
	}
	persisted, err := coordinator.GetTaskPlan(context.Background(), plan.InvocationID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Revision != 5 || persisted.Status != PlanCompleted {
		t.Fatalf("每个状态转换都应通过 CAS 持久化: %#v", persisted)
	}
	for _, step := range persisted.Steps {
		if step.Status != PlanStepCompleted || len(step.Evidence) != 1 {
			t.Fatalf("步骤证据未保存: %#v", persisted.Steps)
		}
	}
	events, err := coordinator.repo.ListEvents(context.Background(), plan.InvocationID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var planEvents int
	for _, event := range events {
		if event.Type == EventPlanUpdated {
			planEvents++
		}
	}
	if planEvents != 4 {
		t.Fatalf("开始/完成每一步都应有 plan.updated，实际=%d events=%#v", planEvents, events)
	}
}

func TestExecuteTaskPlanUsesBoundedSafeRepair(t *testing.T) {
	now := time.Now().UTC()
	plan := TaskPlan{ID: "plan-repair", InvocationID: "inv-repair", Revision: 1, Status: PlanPending, Steps: []PlanStep{{ID: "verify", Title: "验证", Status: PlanStepPending}}, CreatedAt: now, UpdatedAt: now}
	coordinator := newWorkflowTestCoordinator(t, plan)
	attempts := 0
	result, err := coordinator.ExecuteTaskPlan(context.Background(), plan.InvocationID, PlanExecutionPolicy{MaxStepExecutions: 10, MaxRuntimeRetries: 0, MaxVerificationRepairs: 1}, func(_ context.Context, step PlanStep) (PlanStepExecutionResult, error) {
		attempts++
		if attempts == 1 {
			return PlanStepExecutionResult{Failure: &WorkflowFailure{Code: "tests_failed", Domain: WorkflowFailureVerification, Message: "回归失败", Retryable: true, RetrySafe: true}}, nil
		}
		if step.RepairAttempts != 1 {
			t.Fatalf("第二次执行应看到 repair_attempts=1，实际=%d", step.RepairAttempts)
		}
		return PlanStepExecutionResult{Evidence: []EvidenceRef{{Kind: "verification", Ref: "verification-2"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || result.Repairs != 1 || result.Plan.Status != PlanCompleted {
		t.Fatalf("安全修正结果=%#v attempts=%d", result, attempts)
	}
	persisted, _ := coordinator.GetTaskPlan(context.Background(), plan.InvocationID)
	if persisted.Steps[0].RepairAttempts != 1 || persisted.Steps[0].LastFailureCode != "" {
		t.Fatalf("成功后应清理上一次失败标记但保留重试计数: %#v", persisted.Steps[0])
	}
}

func TestExecuteTaskPlanBlocksUnsafeOrExhaustedFailure(t *testing.T) {
	tests := []struct {
		name       string
		failure    WorkflowFailure
		policy     PlanExecutionPolicy
		executions int
	}{
		{name: "unsafe", failure: WorkflowFailure{Code: "write_unknown", Domain: WorkflowFailureTool, Message: "副作用状态未知", Retryable: true, RetrySafe: false}, policy: PlanExecutionPolicy{MaxStepExecutions: 10, MaxRuntimeRetries: 3, MaxVerificationRepairs: 3}, executions: 1},
		{name: "exhausted", failure: WorkflowFailure{Code: "verify_failed", Domain: WorkflowFailureVerification, Message: "仍失败", Retryable: true, RetrySafe: true}, policy: PlanExecutionPolicy{MaxStepExecutions: 10, MaxRuntimeRetries: 0, MaxVerificationRepairs: 1}, executions: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			plan := TaskPlan{ID: "plan-" + tc.name, InvocationID: "inv-" + tc.name, Revision: 1, Status: PlanPending, Steps: []PlanStep{{ID: "verify", Title: "验证", Status: PlanStepPending}}, CreatedAt: now, UpdatedAt: now}
			coordinator := newWorkflowTestCoordinator(t, plan)
			attempts := 0
			result, err := coordinator.ExecuteTaskPlan(context.Background(), plan.InvocationID, tc.policy, func(context.Context, PlanStep) (PlanStepExecutionResult, error) {
				attempts++
				return PlanStepExecutionResult{Failure: &tc.failure}, nil
			})
			if !errors.Is(err, ErrWorkflowBlocked) {
				t.Fatalf("应返回 WorkflowBlocked，err=%v", err)
			}
			if attempts != tc.executions || result.Plan.Status != PlanBlocked || result.Plan.Steps[0].Status != PlanStepBlocked {
				t.Fatalf("阻塞结果=%#v attempts=%d", result, attempts)
			}
			if !strings.Contains(result.Plan.Blocker, tc.failure.Code) {
				t.Fatalf("阻塞原因应保留分类 code: %#v", result.Plan)
			}
		})
	}
}

func TestExecuteTaskPlanDoesNotRerunUnknownActiveStep(t *testing.T) {
	now := time.Now().UTC()
	plan := TaskPlan{ID: "plan-active", InvocationID: "inv-active", Revision: 1, Status: PlanInProgress, CurrentStepID: "step", Steps: []PlanStep{{ID: "step", Title: "副作用", Status: PlanStepInProgress}}, CreatedAt: now, UpdatedAt: now}
	coordinator := newWorkflowTestCoordinator(t, plan)
	called := false
	result, err := coordinator.ExecuteTaskPlan(context.Background(), plan.InvocationID, PlanExecutionPolicy{}, func(context.Context, PlanStep) (PlanStepExecutionResult, error) {
		called = true
		return PlanStepExecutionResult{Evidence: []EvidenceRef{{Kind: "event", Ref: "unexpected"}}}, nil
	})
	if !errors.Is(err, ErrWorkflowActiveStep) || called || result.Failure == nil || result.Failure.Code != "active_step_unknown" {
		t.Fatalf("active step 不应被重跑: result=%#v err=%v called=%v", result, err, called)
	}
}

func TestPlanExecutionPolicyZeroValueAndExplicitZeroRetries(t *testing.T) {
	defaults, err := (PlanExecutionPolicy{}).Normalize()
	if err != nil || defaults != DefaultPlanExecutionPolicy() {
		t.Fatalf("零值应使用默认策略: %#v err=%v", defaults, err)
	}
	policy, err := (PlanExecutionPolicy{MaxStepExecutions: 10, MaxRuntimeRetries: 0, MaxVerificationRepairs: 0}).Normalize()
	if err != nil || policy.MaxRuntimeRetries != 0 || policy.MaxVerificationRepairs != 0 {
		t.Fatalf("显式零重试预算不应被替换成默认值: %#v err=%v", policy, err)
	}
	for _, invalid := range []PlanExecutionPolicy{{MaxStepExecutions: -1}, {MaxStepExecutions: MaxPlanStepExecutions + 1}, {MaxStepExecutions: 10, MaxRuntimeRetries: -1}, {MaxStepExecutions: 10, MaxVerificationRepairs: MaxPlanRetries + 1}} {
		if _, err := invalid.Normalize(); !errors.Is(err, ErrInvalidWorkflowPolicy) {
			t.Fatalf("无效策略应被拒绝: %#v err=%v", invalid, err)
		}
	}
}

func TestWorkflowFailureNormalizationRedactsAndStabilizesDomain(t *testing.T) {
	failure := normalizeWorkflowFailure(&WorkflowFailure{Code: "  custom ", Domain: "not-known", Message: "api_key=secret-value", Retryable: true, RetrySafe: true})
	if failure.Code != "custom" || failure.Domain != WorkflowFailureUnknown || strings.Contains(failure.Message, "secret-value") {
		t.Fatalf("failure 归一化不安全: %#v", failure)
	}
}

func TestApplyModelPlanEventPersistsOnlyStructuredValidatedPlans(t *testing.T) {
	now := time.Now().UTC()
	plan := TaskPlan{ID: "plan-model", InvocationID: "inv-model", Revision: 1, Status: PlanPending, Steps: []PlanStep{}, CreatedAt: now, UpdatedAt: now}
	coordinator := newWorkflowTestCoordinator(t, plan)
	applied, err := coordinator.applyModelPlanEvent(context.Background(), plan.InvocationID, map[string]any{"plan": []any{"legacy-step"}})
	if err != nil || applied {
		t.Fatalf("非结构化 plan 只能保留为观测: applied=%v err=%v", applied, err)
	}
	applied, err = coordinator.applyModelPlanEvent(context.Background(), plan.InvocationID, map[string]any{"plan": map[string]any{
		"steps": []map[string]any{{"id": "verify", "title": "验证", "status": "pending"}, {"id": "edit", "title": "修改", "status": "pending"}},
	}})
	if err != nil || !applied {
		t.Fatalf("结构化 plan 应持久化: applied=%v err=%v", applied, err)
	}
	persisted, err := coordinator.GetTaskPlan(context.Background(), plan.InvocationID)
	if err != nil || persisted.Revision != 2 || len(persisted.Steps) != 2 {
		t.Fatalf("模型 plan 未持久化: %#v err=%v", persisted, err)
	}
	// Repeating an identical model proposal must not create a revision storm.
	applied, err = coordinator.applyModelPlanEvent(context.Background(), plan.InvocationID, map[string]any{"plan": map[string]any{
		"steps": []map[string]any{{"id": "verify", "title": "验证", "status": "pending"}, {"id": "edit", "title": "修改", "status": "pending"}},
	}})
	if err != nil || !applied {
		t.Fatalf("重复结构化 plan 仍应被识别为已处理: applied=%v err=%v", applied, err)
	}
	repeated, _ := coordinator.GetTaskPlan(context.Background(), plan.InvocationID)
	if repeated.Revision != persisted.Revision {
		t.Fatalf("重复 plan 不应递增 revision: before=%d after=%d", persisted.Revision, repeated.Revision)
	}
}

func TestEnsurePlanStepStartedAdmitsOnlyDependencyReadyStep(t *testing.T) {
	now := time.Now().UTC()
	plan := TaskPlan{ID: "plan-auto-start", InvocationID: "inv-auto-start", Revision: 1, Status: PlanPending, Steps: []PlanStep{
		{ID: "inspect", Title: "读取代码", Status: PlanStepPending, UpdatedAt: now},
		{ID: "verify", Title: "运行测试", Status: PlanStepPending, DependsOn: []string{"inspect"}, UpdatedAt: now},
	}, CreatedAt: now, UpdatedAt: now}
	coordinator := newWorkflowTestCoordinator(t, plan)
	if err := coordinator.ensurePlanStepStarted(context.Background(), plan.InvocationID, "test admission"); err != nil {
		t.Fatal(err)
	}
	started, err := coordinator.GetTaskPlan(context.Background(), plan.InvocationID)
	if err != nil {
		t.Fatal(err)
	}
	if started.CurrentStepID != "inspect" || started.Status != PlanInProgress || started.Steps[0].Status != PlanStepInProgress || started.Steps[1].Status != PlanStepPending {
		t.Fatalf("应只自动启动第一个就绪步骤: %#v", started)
	}
	revision := started.Revision
	if err := coordinator.ensurePlanStepStarted(context.Background(), plan.InvocationID, "duplicate admission"); err != nil {
		t.Fatal(err)
	}
	repeated, _ := coordinator.GetTaskPlan(context.Background(), plan.InvocationID)
	if repeated.Revision != revision || repeated.CurrentStepID != "inspect" {
		t.Fatalf("重复自动接线不应改写活动步骤: before=%#v after=%#v", started, repeated)
	}
}

func TestValidateWorkspaceOperationAdmissionFencesDefaultCodingWorkflow(t *testing.T) {
	now := time.Now().UTC()
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-operation-gate", UserID: "user", ConversationID: "conversation", WorkspaceID: "workspace", SessionID: "conversation", Message: "修改代码", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	contract := InitialTaskContract(invocation.ID, invocation.Message, now)
	if err := repo.CreateTaskContract(context.Background(), contract); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateTaskPlan(context.Background(), initialTaskPlan(invocation.ID, now)); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, started: true, rootCtx: context.Background(), subscribers: make(map[string]map[chan AgentEvent]struct{})}
	if err := coordinator.ensureDefaultWorkflowPlan(context.Background(), invocation, contract); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.ensurePlanStepStarted(context.Background(), invocation.ID, "test inspect admission"); err != nil {
		t.Fatal(err)
	}
	write := workspace.OperationRequest{WorkspaceID: invocation.WorkspaceID, InvocationID: invocation.ID, Type: workspace.OperationWriteFile, Path: "main.go", Content: "package main\n"}
	if err := coordinator.ValidateWorkspaceOperationAdmission(context.Background(), write); !errors.Is(err, ErrConflict) {
		t.Fatalf("inspect 步骤不应允许写入: %v", err)
	}

	plan, err := coordinator.GetTaskPlan(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	completedInspect, err := plan.CompleteStep("inspect", []EvidenceRef{{Kind: "tool_call", Ref: "tool-read"}}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.UpdateTaskPlan(context.Background(), invocation.ID, completedInspect, "test inspect complete"); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.ensurePlanStepStarted(context.Background(), invocation.ID, "test edit admission"); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.ValidateWorkspaceOperationAdmission(context.Background(), write); err != nil {
		t.Fatalf("edit 步骤应允许写入: %v", err)
	}

	plan, err = coordinator.GetTaskPlan(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	completedEdit, err := plan.CompleteStep("edit", []EvidenceRef{{Kind: "operation", Ref: "operation-write"}}, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.UpdateTaskPlan(context.Background(), invocation.ID, completedEdit, "test edit complete"); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.ensurePlanStepStarted(context.Background(), invocation.ID, "test verify admission"); err != nil {
		t.Fatal(err)
	}
	command := workspace.OperationRequest{WorkspaceID: invocation.WorkspaceID, InvocationID: invocation.ID, Type: workspace.OperationCommand, Command: "go test ./...", CWD: ".", Timeout: 60}
	if err := coordinator.ValidateWorkspaceOperationAdmission(context.Background(), command); err != nil {
		t.Fatalf("verify 步骤应允许命令: %v", err)
	}
	if err := coordinator.ValidateWorkspaceOperationAdmission(context.Background(), write); !errors.Is(err, ErrConflict) {
		t.Fatalf("verify 步骤不应允许新的写入: %v", err)
	}
}

func TestApplyModelPlanEventPreservesCompletedEvidenceAndRejectsInvalidDependencies(t *testing.T) {
	now := time.Now().UTC()
	completed := PlanStep{ID: "inspect", Title: "检查", Status: PlanStepCompleted, Evidence: []EvidenceRef{{Kind: "event", Ref: "e-inspect"}}}
	plan := TaskPlan{ID: "plan-model-preserve", InvocationID: "inv-model-preserve", Revision: 1, Status: PlanPending, Steps: []PlanStep{completed, {ID: "edit", Title: "修改", Status: PlanStepPending, DependsOn: []string{"inspect"}}}, CreatedAt: now, UpdatedAt: now}
	coordinator := newWorkflowTestCoordinator(t, plan)
	if _, err := coordinator.applyModelPlanEvent(context.Background(), plan.InvocationID, map[string]any{"plan": map[string]any{
		"steps": []map[string]any{{"id": "edit", "title": "修改", "status": "pending", "depends_on": []string{"inspect"}}},
	}}); err != nil {
		t.Fatal(err)
	}
	persisted, err := coordinator.GetTaskPlan(context.Background(), plan.InvocationID)
	if err != nil || len(persisted.Steps) != 2 || persisted.Steps[0].ID != "edit" {
		// The model order is retained, but completed evidence must still exist.
		t.Fatalf("模型缩短 plan 后步骤未保留: %#v err=%v", persisted, err)
	}
	var foundEvidence bool
	for _, step := range persisted.Steps {
		if step.ID == "inspect" && len(step.Evidence) == 1 && step.Status == PlanStepCompleted {
			foundEvidence = true
		}
	}
	if !foundEvidence {
		t.Fatalf("已完成步骤 evidence 不可被模型删除: %#v", persisted.Steps)
	}
	_, err = coordinator.applyModelPlanEvent(context.Background(), plan.InvocationID, map[string]any{"plan": map[string]any{
		"steps": []map[string]any{{"id": "bad", "title": "错误", "status": "in_progress", "depends_on": []string{"edit"}}, {"id": "edit", "title": "修改", "status": "pending"}},
	}})
	if !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("in_progress 依赖 pending 的模型 plan 应拒绝: %v", err)
	}
}

func TestApplyMappedPlanEventsFiltersPersistedStructuredPlanOnly(t *testing.T) {
	now := time.Now().UTC()
	plan := TaskPlan{ID: "plan-filter", InvocationID: "inv-filter", Revision: 1, Status: PlanPending, Steps: []PlanStep{}, CreatedAt: now, UpdatedAt: now}
	coordinator := newWorkflowTestCoordinator(t, plan)
	mapped, err := coordinator.applyMappedPlanEvents(context.Background(), plan.InvocationID, []AgentEvent{
		{Type: EventPlanUpdated, Data: map[string]any{"plan": map[string]any{"steps": []map[string]any{{"id": "step", "title": "步骤", "status": "pending"}}}}},
		{Type: EventPlanUpdated, Data: map[string]any{"plan": []any{"diagnostic"}}},
		{Type: EventAssistantMessage, Data: map[string]any{"text": "继续"}},
	})
	if err != nil || len(mapped) != 2 || mapped[0].Type != EventPlanUpdated || mapped[1].Type != EventAssistantMessage {
		t.Fatalf("只应过滤已落库的结构化 plan: mapped=%#v err=%v", mapped, err)
	}
	persisted, _ := coordinator.GetTaskPlan(context.Background(), plan.InvocationID)
	if len(persisted.Steps) != 1 || persisted.Revision != 2 {
		t.Fatalf("结构化 plan 未落库: %#v", persisted)
	}
}

func TestApplyModelPlanEventRedactsBoundedFailureMetadata(t *testing.T) {
	now := time.Now().UTC()
	plan := TaskPlan{ID: "plan-model-redact", InvocationID: "inv-model-redact", Revision: 1, Status: PlanPending, Steps: []PlanStep{}, CreatedAt: now, UpdatedAt: now}
	coordinator := newWorkflowTestCoordinator(t, plan)
	if _, err := coordinator.applyModelPlanEvent(context.Background(), plan.InvocationID, map[string]any{"plan": map[string]any{
		"steps": []map[string]any{{"id": "verify", "title": "验证", "status": "blocked", "blocker": "api_key=top-secret"}},
	}}); err != nil {
		t.Fatal(err)
	}
	persisted, err := coordinator.GetTaskPlan(context.Background(), plan.InvocationID)
	if err != nil || len(persisted.Steps) != 1 {
		t.Fatalf("模型 plan 未持久化: %#v err=%v", persisted, err)
	}
	if strings.Contains(persisted.Steps[0].Blocker, "top-secret") {
		t.Fatalf("模型 plan 的 blocker 不应落明文密钥: %#v", persisted.Steps[0])
	}
}

func TestDeriveWorkflowPhaseFromDurableFacts(t *testing.T) {
	base := Invocation{ID: "inv-phase", Status: InvocationRunning}
	if got := deriveWorkflowPhase(base, nil, nil, nil, nil); got != WorkflowPhaseUnderstanding {
		t.Fatalf("空运行应处于 understanding，实际=%s", got)
	}
	plan := &TaskPlan{Status: PlanPending, Steps: []PlanStep{{ID: "inspect", Title: "读取代码", Status: PlanStepPending}}}
	if got := deriveWorkflowPhase(base, plan, nil, nil, nil); got != WorkflowPhasePlanning {
		t.Fatalf("有 pending plan 应处于 planning，实际=%s", got)
	}
	plan = &TaskPlan{Status: PlanInProgress, CurrentStepID: "verify", Steps: []PlanStep{{ID: "verify", Title: "运行测试", Status: PlanStepInProgress}}}
	if got := deriveWorkflowPhase(base, plan, nil, nil, nil); got != WorkflowPhaseVerifying {
		t.Fatalf("验证 active step 应处于 verifying，实际=%s", got)
	}
	if got := deriveWorkflowPhase(base, nil, []ApprovalRef{{ID: "approval"}}, nil, nil); got != WorkflowPhaseWaitingApproval {
		t.Fatalf("pending approval 应处于 waiting_approval，实际=%s", got)
	}
	if got := deriveWorkflowPhase(Invocation{ID: "inv-phase", Status: InvocationCompleted}, nil, nil, nil, nil); got != WorkflowPhaseReporting {
		t.Fatalf("终态应处于 reporting，实际=%s", got)
	}
	if got := deriveWorkflowPhase(base, nil, nil, nil, []AgentEvent{{Type: EventInstructionConflict}}); got != WorkflowPhaseDiscoveringInstructions {
		t.Fatalf("instruction conflict 应处于 discovering_instructions，实际=%s", got)
	}
}

func TestRuntimeSnapshotRejectsUnknownWorkflowPhase(t *testing.T) {
	snapshot := RuntimeSnapshot{InvocationID: "inv-phase", Revision: 1, Phase: string(InvocationRunning), WorkflowPhase: WorkflowPhase("not-a-phase")}
	if err := snapshot.Validate(); !errors.Is(err, ErrInvalidRuntimeSnapshot) {
		t.Fatalf("未知 workflow phase 应拒绝: %v", err)
	}
}

func TestRebuildRuntimeSnapshotPersistsWorkflowPhase(t *testing.T) {
	now := time.Now().UTC()
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-phase-snapshot", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	plan := TaskPlan{ID: "plan-phase-snapshot", InvocationID: invocation.ID, Revision: 1, Status: PlanInProgress, CurrentStepID: "verify", Steps: []PlanStep{{ID: "verify", Title: "运行测试", Status: PlanStepInProgress}}, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateTaskPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	snapshot, err := coordinator.RebuildRuntimeSnapshot(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.WorkflowPhase != WorkflowPhaseVerifying {
		t.Fatalf("Runtime Snapshot 未投影 workflow phase: %#v", snapshot)
	}
	projection := projectRuntimeSnapshot(snapshot)
	if projection.WorkflowPhase != string(WorkflowPhaseVerifying) {
		t.Fatalf("Kernel projection 未携带 workflow phase: %#v", projection)
	}
}

func TestRebuildRuntimeSnapshotPersistsWorkflowCheckpoint(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Date(2026, 9, 12, 11, 0, 0, 0, time.UTC)
	invocation := Invocation{ID: "inv-workflow-snapshot", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	for _, event := range []AgentEvent{
		{ID: "workflow-tool-request", InvocationID: invocation.ID, Type: EventToolRequested, Timestamp: now, Data: map[string]any{"call_id": "wait-snapshot", "name": "wait_for_result"}},
		{ID: "workflow-tool-waiting", InvocationID: invocation.ID, Type: EventInvocationWaiting, Timestamp: now.Add(time.Second), Data: map[string]any{"reason": "tool", "tool_call_ids": []string{"wait-snapshot"}, "step_id": "inspect"}},
	} {
		if _, err := repo.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	snapshot, err := coordinator.RebuildRuntimeSnapshot(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("重建的 workflow checkpoint 应可验证: %v; %#v", err, snapshot.Workflow)
	}
	if snapshot.Workflow.Status != WorkflowCheckpointWaiting || len(snapshot.Workflow.ActiveBoundaryIDs) != 1 || snapshot.Workflow.ActiveBoundaryIDs[0] != "boundary:inv-workflow-snapshot:2" {
		t.Fatalf("snapshot 未保存等待边界: %#v", snapshot.Workflow)
	}
	projection := projectRuntimeSnapshot(snapshot)
	if projection.WorkflowStatus != string(WorkflowCheckpointWaiting) || projection.WorkflowBoundaryID != "boundary:inv-workflow-snapshot:2" || projection.WorkflowBoundaryKind != string(WorkflowBoundaryTool) {
		t.Fatalf("Kernel projection 未携带 workflow checkpoint: %#v", projection)
	}
}
