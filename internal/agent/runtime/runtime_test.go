package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Abot/internal/agent"
	"Abot/internal/provider"
	"Abot/internal/workspace"
	adkagent "google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

func TestMemoryRepositorySequencesAndCAS(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	if err := repo.CreateInvocation(context.Background(), Invocation{ID: "inv-1", UserID: "u", ConversationID: "c", SessionID: "c", Status: InvocationQueued, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	first, err := repo.AppendEvent(context.Background(), AgentEvent{ID: "evt-1", InvocationID: "inv-1", Type: EventInvocationStarted})
	if err != nil || first.Sequence != 1 {
		t.Fatalf("first event = %#v, err=%v", first, err)
	}
	second, err := repo.AppendEvent(context.Background(), AgentEvent{ID: "evt-2", InvocationID: "inv-1", Type: EventInvocationCompleted})
	if err != nil || second.Sequence != 2 {
		t.Fatalf("second event = %#v, err=%v", second, err)
	}
	if ok, err := repo.TransitionInvocation(context.Background(), "inv-1", InvocationQueued, InvocationRunning, ""); err != nil || !ok {
		t.Fatalf("queued -> running = %v, err=%v", ok, err)
	}
	if ok, err := repo.TransitionInvocation(context.Background(), "inv-1", InvocationQueued, InvocationCompleted, ""); err != nil || ok {
		t.Fatalf("stale transition should fail: %v, err=%v", ok, err)
	}
}

func TestCoordinatorDurableInvocationStripsAttachmentPreview(t *testing.T) {
	kernel, err := agent.NewKernel(agent.Config{
		AppName:        "runtime-attachment-preview",
		SessionService: session.InMemoryService(),
		Providers: func() *provider.Registry {
			registry, registryErr := provider.NewRegistry(context.Background(), &runtimeProviderRepository{providers: map[string]provider.Provider{}}, map[provider.Protocol]provider.Adapter{})
			if registryErr != nil {
				t.Fatalf("创建 provider registry 失败: %v", registryErr)
			}
			return registry
		}(),
	})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewMemoryRepository()
	coordinator, err := NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	ref := agent.AttachmentRef{ID: "artifact-1", Version: 1, Digest: "sha256:content", Kind: "input_attachment", MIMEType: "text/plain", Size: 7, Name: "note.txt", Preview: "private content"}
	invocation, err := coordinator.StartInvocation(context.Background(), agent.ChatRequest{
		UserID: "user-1", ConversationID: "conversation-1", SessionID: "conversation-1", Message: "读取附件",
		Attachments: []agent.Attachment{{Name: ref.Name, MIMEType: ref.MIMEType, Ref: &ref}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if invocation.Attachments[0].Ref == nil || invocation.Attachments[0].Ref.Preview != "" {
		t.Fatalf("返回的 durable invocation 不应携带 preview: %#v", invocation.Attachments)
	}
	stored, err := repo.GetInvocation(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Attachments[0].Ref == nil || stored.Attachments[0].Ref.Preview != "" {
		t.Fatalf("持久化 invocation 不应携带 preview: %#v", stored.Attachments)
	}
}

func TestMemoryWorkspaceBaselineIsImmutableAndDefensive(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC().Truncate(time.Microsecond)
	invocation := Invocation{ID: "inv-baseline-memory", UserID: "user", ConversationID: "conversation", SessionID: "conversation", WorkspaceID: "workspace", Status: InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	want := WorktreeBaseline{InvocationID: invocation.ID, WorkspaceID: invocation.WorkspaceID, RepositoryType: "git", HeadRevision: "abc", Branch: "main", StatusDigest: "sha256:baseline", ChangedPaths: []PathStatus{{Path: "z.go", Status: " M"}, {Path: "a.go", Status: "??"}}, StatusKnown: true, Revision: 1, CapturedAt: now}
	saved, err := repo.SaveWorkspaceBaseline(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ChangedPaths[0].Path != "a.go" || saved.ChangedPaths[1].Path != "z.go" {
		t.Fatalf("保存时应稳定排序 changed paths: %#v", saved.ChangedPaths)
	}
	saved.ChangedPaths[0].Path = "mutated"
	got, err := repo.GetWorkspaceBaseline(context.Background(), invocation.ID)
	if err != nil || got.ChangedPaths[0].Path != "a.go" {
		t.Fatalf("读取 baseline 不应暴露内部 slice: %#v err=%v", got, err)
	}
	if _, err := repo.SaveWorkspaceBaseline(context.Background(), want); err != nil {
		t.Fatalf("相同 baseline 重复保存应幂等: %v", err)
	}
	changed := want
	changed.StatusDigest = "sha256:other"
	if _, err := repo.SaveWorkspaceBaseline(context.Background(), changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("不同 baseline 不应覆盖旧事实: %v", err)
	}
}

func TestWorktreeBaselineNormalizeCanonicalizesAndRejectsEscape(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	baseline, err := (WorktreeBaseline{
		InvocationID: "inv-normalize-baseline", RepositoryType: "git", StatusDigest: "sha256:baseline",
		ChangedPaths: []PathStatus{{Path: "src/../README.md", Status: " M"}}, CapturedAt: now,
	}).Normalize(now)
	if err != nil || len(baseline.ChangedPaths) != 1 || baseline.ChangedPaths[0].Path != "README.md" {
		t.Fatalf("baseline path 应规范化: %#v err=%v", baseline, err)
	}
	if _, err := (WorktreeBaseline{
		InvocationID: "inv-invalid-baseline", RepositoryType: "git", StatusDigest: "sha256:baseline",
		ChangedPaths: []PathStatus{{Path: "src/../../outside", Status: "??"}}, CapturedAt: now,
	}).Normalize(now); !errors.Is(err, ErrInvalidWorktreeBaseline) {
		t.Fatalf("越界 baseline path 应拒绝: %v", err)
	}
}

func TestCoordinatorCapturesWorkspaceBaselineAtAdmission(t *testing.T) {
	registry, err := provider.NewRegistry(context.Background(), &runtimeProviderRepository{providers: map[string]provider.Provider{}}, map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{AppName: "baseline-admission", SessionService: session.InMemoryService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewMemoryRepository()
	coordinator, err := NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := coordinator.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	coordinator.SetWorkspaceBaselineResolver(func(context.Context, string, string) (WorktreeBaseline, error) {
		return WorktreeBaseline{RepositoryType: "git", HeadRevision: "head-1", Branch: "main", StatusDigest: "sha256:admission", ChangedPaths: []PathStatus{{Path: "existing.go", Status: " M"}}, StatusKnown: true, CapturedAt: time.Now().UTC()}, nil
	})
	workspaceID := "workspace-baseline"
	invocation, err := coordinator.StartInvocation(ctx, agent.ChatRequest{UserID: "user", ConversationID: "baseline-conversation", SessionID: "baseline-conversation", WorkspaceID: &workspaceID, Message: "记录基线", Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := coordinator.GetWorkspaceBaseline(ctx, invocation.ID)
	if err != nil || baseline.InvocationID != invocation.ID || baseline.WorkspaceID != workspaceID || baseline.StatusDigest != "sha256:admission" || len(baseline.ChangedPaths) != 1 {
		t.Fatalf("Invocation admission 未保存基线: %#v err=%v", baseline, err)
	}
	events, err := repo.ListEvents(ctx, invocation.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Type == EventRepositoryBaseline {
			found = true
			if event.Data["status_digest"] != baseline.StatusDigest || event.Data["changed_path_count"] != 1 {
				t.Fatalf("baseline 事件只应包含一致的 metadata: %#v", event.Data)
			}
		}
	}
	if !found {
		t.Fatalf("缺少 %s 事件: %#v", EventRepositoryBaseline, events)
	}
}

func TestCoordinatorInstallsDefaultCodingPlanAtAdmission(t *testing.T) {
	registry, err := provider.NewRegistry(context.Background(), &runtimeProviderRepository{providers: map[string]provider.Provider{}}, map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{AppName: "default-plan-admission", SessionService: session.InMemoryService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewMemoryRepository()
	coordinator, err := NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	workspaceID := "workspace-default-plan"
	invocation, err := coordinator.StartInvocation(context.Background(), agent.ChatRequest{
		UserID: "user", ConversationID: "default-plan-conversation", SessionID: "default-plan-conversation",
		WorkspaceID: &workspaceID, Message: "修复并通过测试", Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := coordinator.GetTaskPlan(context.Background(), invocation.ID)
	if err != nil || len(plan.Steps) != 3 || plan.Steps[0].ID != "inspect" || plan.Steps[1].ID != "edit" || plan.Steps[2].ID != "verify" {
		t.Fatalf("admission 应安装默认 coding plan: %#v err=%v", plan, err)
	}
}

func TestCoordinatorInstallsDefaultPlanWhenContractRepositoryIsOptional(t *testing.T) {
	registry, err := provider.NewRegistry(context.Background(), &runtimeProviderRepository{providers: map[string]provider.Provider{}}, map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{AppName: "default-plan-compat", SessionService: session.InMemoryService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	memoryRepo := NewMemoryRepository()
	repo := &planOnlyRuntimeRepository{Repository: memoryRepo, plans: memoryRepo}
	coordinator, err := NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	invocation, err := coordinator.StartInvocation(context.Background(), agent.ChatRequest{UserID: "user", ConversationID: "default-plan-compat-conversation", SessionID: "default-plan-compat-conversation", Message: "修复并通过测试", Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := coordinator.GetTaskPlan(context.Background(), invocation.ID)
	if err != nil || len(plan.Steps) != 3 || plan.Steps[0].ID != "inspect" || plan.Steps[1].ID != "edit" || plan.Steps[2].ID != "verify" {
		t.Fatalf("仅启用 TaskPlan 的兼容 Repository 也应安装默认 coding plan: %#v err=%v", plan, err)
	}
}

// planOnlyRuntimeRepository intentionally exposes TaskPlan without exposing
// TaskContract. It models an older embedder that opted into the plan extension
// before the Contract repository was added.
type planOnlyRuntimeRepository struct {
	Repository
	plans TaskPlanRepository
}

func (r *planOnlyRuntimeRepository) CreateTaskPlan(ctx context.Context, item TaskPlan) error {
	return r.plans.CreateTaskPlan(ctx, item)
}

func (r *planOnlyRuntimeRepository) GetTaskPlan(ctx context.Context, invocationID string) (TaskPlan, error) {
	return r.plans.GetTaskPlan(ctx, invocationID)
}

func (r *planOnlyRuntimeRepository) UpdateTaskPlan(ctx context.Context, item TaskPlan, expectedRevision int64) (TaskPlan, error) {
	return r.plans.UpdateTaskPlan(ctx, item, expectedRevision)
}

func TestMemoryInvocationLeaseIsExclusiveAndExpires(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	if err := repo.CreateInvocation(context.Background(), Invocation{ID: "inv-lease", UserID: "u", ConversationID: "c", SessionID: "c", Status: InvocationQueued, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if acquired, err := repo.AcquireInvocationLease(context.Background(), "inv-lease", "worker-a", now, time.Minute); err != nil || !acquired {
		t.Fatalf("首次抢占租约失败: acquired=%v err=%v", acquired, err)
	}
	if acquired, err := repo.AcquireInvocationLease(context.Background(), "inv-lease", "worker-b", now.Add(30*time.Second), time.Minute); err != nil || acquired {
		t.Fatalf("未过期租约不应被其他 worker 抢占: acquired=%v err=%v", acquired, err)
	}
	if renewed, err := repo.RenewInvocationLease(context.Background(), "inv-lease", "worker-a", now.Add(30*time.Second), time.Minute); err != nil || renewed {
		t.Fatalf("queued invocation 不应被续租为 running: renewed=%v err=%v", renewed, err)
	}
	if released, err := repo.ReleaseInvocationLease(context.Background(), "inv-lease", "worker-b"); err != nil || released {
		t.Fatalf("非持有者不应释放租约: released=%v err=%v", released, err)
	}
	if acquired, err := repo.AcquireInvocationLease(context.Background(), "inv-lease", "worker-b", now.Add(2*time.Minute), time.Minute); err != nil || !acquired {
		t.Fatalf("过期租约应可被接管: acquired=%v err=%v", acquired, err)
	}
	item, err := repo.GetInvocation(context.Background(), "inv-lease")
	if err != nil || item.LeaseOwner != "worker-b" || item.LeaseExpiresAt == nil {
		t.Fatalf("接管后的租约状态错误: item=%#v err=%v", item, err)
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "lease_owner") || strings.Contains(string(encoded), "lease_expires_at") {
		t.Fatalf("租约内部字段不应进入 API JSON: %s", encoded)
	}
	concurrent := Invocation{ID: "inv-lease-concurrent", UserID: "u", ConversationID: "c2", SessionID: "c2", Status: InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), concurrent); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan bool, 16)
	var group sync.WaitGroup
	for index := 0; index < 16; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			acquired, err := repo.AcquireInvocationLease(context.Background(), concurrent.ID, fmt.Sprintf("worker-%d", index), now, time.Minute)
			if err != nil {
				t.Errorf("并发抢租约失败: %v", err)
			}
			results <- acquired
		}(index)
	}
	close(start)
	group.Wait()
	close(results)
	acquiredCount := 0
	for acquired := range results {
		if acquired {
			acquiredCount++
		}
	}
	if acquiredCount != 1 {
		t.Fatalf("同一 Invocation 并发抢租约应恰好成功一次，实际=%d", acquiredCount)
	}
}

func TestReconcileInterruptedRespectsLiveLeaseAndFailsExpiredRun(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	item := Invocation{ID: "inv-recovery-lease", UserID: "u", ConversationID: "c", SessionID: "c", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	expires := now.Add(time.Minute)
	item.LeaseOwner = "other-process"
	item.LeaseExpiresAt = &expires
	if err := repo.UpdateInvocation(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	if err := coordinator.reconcileInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, err := repo.GetInvocation(context.Background(), item.ID)
	if err != nil || current.Status != InvocationRunning {
		t.Fatalf("活动租约不应被恢复扫描失败: current=%#v err=%v", current, err)
	}
	expired := time.Now().UTC().Add(-time.Second)
	current.LeaseExpiresAt = &expired
	if err := repo.UpdateInvocation(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.reconcileInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, err = repo.GetInvocation(context.Background(), item.ID)
	if err != nil || current.Status != InvocationFailed || current.LeaseOwner != "" || current.LeaseExpiresAt != nil {
		t.Fatalf("过期租约应保守失败并释放: current=%#v err=%v", current, err)
	}
}

func TestMemoryRepositoryFindsInvocationByIdempotencyKey(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	want := Invocation{ID: "inv-idempotent", UserID: "user", ConversationID: "conversation", SessionID: "conversation", IdempotencyKey: "request-1", Status: InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetInvocationByIdempotencyKey(context.Background(), want.UserID, want.ConversationID, want.IdempotencyKey)
	if err != nil || got.ID != want.ID {
		t.Fatalf("按幂等键查找 invocation=%#v err=%v", got, err)
	}
	if _, err := repo.GetInvocationByIdempotencyKey(context.Background(), want.UserID, want.ConversationID, "missing"); err != ErrNotFound {
		t.Fatalf("缺失幂等键应返回 ErrNotFound，实际=%v", err)
	}
}

func TestTaskPlanRevisionAndInvariants(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-plan", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	plan := initialTaskPlan(invocation.ID, now)
	if err := repo.CreateTaskPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	current, err := coordinator.GetTaskPlan(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision != 1 || current.Status != PlanPending {
		t.Fatalf("初始计划=%#v", current)
	}
	updated, err := coordinator.UpdateTaskPlan(context.Background(), invocation.ID, TaskPlan{
		Revision: current.Revision,
		Steps:    []PlanStep{{ID: "inspect", Title: "检查代码", Status: PlanStepInProgress}},
	}, "开始勘察")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 2 || updated.Status != PlanInProgress || updated.CurrentStepID != "inspect" {
		t.Fatalf("更新后的计划=%#v", updated)
	}
	if _, err := coordinator.UpdateTaskPlan(context.Background(), invocation.ID, TaskPlan{Revision: current.Revision, Steps: updated.Steps}, "stale"); !errors.Is(err, ErrConflict) {
		t.Fatalf("旧 revision 应冲突，实际=%v", err)
	}
	events, err := repo.ListEvents(context.Background(), invocation.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != EventPlanUpdated {
		t.Fatalf("计划更新事件=%#v", events)
	}
	completed, err := coordinator.UpdateTaskPlan(context.Background(), invocation.ID, TaskPlan{
		Revision: updated.Revision,
		Steps:    []PlanStep{{ID: "inspect", Title: "检查代码", Status: PlanStepCompleted, Evidence: []EvidenceRef{{Kind: "event", Ref: "evt-1"}}}},
	}, "完成勘察")
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != PlanCompleted {
		t.Fatalf("有证据的完成步骤应使计划完成，实际=%s", completed.Status)
	}
}

func TestTaskContractVersionCASAndAudit(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-contract", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Message: "修复缓存并增加回归测试", Status: InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	initial, err := coordinator.GetTaskContract(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Version != 1 || initial.TaskType != TaskChange || !initial.MutationAllowed || !initial.ValidationRequired {
		t.Fatalf("初始任务契约不正确: %#v", initial)
	}
	updated, err := coordinator.UpdateTaskContract(context.Background(), invocation.ID, TaskContract{
		Version: initial.Version, TaskType: TaskBuild, Goal: "修复缓存并增加回归测试", RequestedOutcome: "代码变更和可重复的回归测试",
		AcceptanceCriteria: []Criterion{{ID: "tests", Description: "目标测试通过"}}, Constraints: []Constraint{{Description: "保持公开 API 兼容", Source: "user"}},
		NonGoals: []string{"不提交 git"}, MutationAllowed: true, ValidationRequired: true, ExternalActions: []ExternalActionRule{{Action: "git push", Allowed: false, Reason: "未授权"}},
	}, "用户补充完成条件")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != 2 || updated.TaskType != TaskBuild || len(updated.AcceptanceCriteria) != 1 || updated.SourceMessageIDs[0] != "invocation:"+invocation.ID {
		t.Fatalf("更新后的任务契约不正确: %#v", updated)
	}
	if _, err := coordinator.UpdateTaskContract(context.Background(), invocation.ID, TaskContract{Version: initial.Version, TaskType: TaskBuild, Goal: "过期编辑", MutationAllowed: true, ValidationRequired: true}, "stale"); !errors.Is(err, ErrConflict) {
		t.Fatalf("旧 contract version 应冲突，实际=%v", err)
	}
	history, err := coordinator.ListTaskContracts(context.Background(), invocation.ID)
	if err != nil || len(history) != 2 || history[0].Version != 1 || history[1].Version != 2 {
		t.Fatalf("任务契约历史=%#v err=%v", history, err)
	}
	events, err := repo.ListEvents(context.Background(), invocation.ID, 0, 20)
	if err != nil {
		t.Fatalf("读取任务契约更新事件失败: %v", err)
	}
	var contractEvents, planEvents int
	for _, event := range events {
		switch event.Type {
		case EventTaskContractUpdated:
			contractEvents++
		case EventPlanUpdated:
			planEvents++
		}
	}
	if contractEvents != 1 || planEvents != 1 {
		t.Fatalf("任务契约更新应同时记录契约和默认计划事件=%#v", events)
	}
	if err := repo.CreateApproval(context.Background(), Approval{ID: "approval-contract-version", InvocationID: invocation.ID, ToolName: "guarded", OriginalCallID: "call", ConfirmationCallID: "confirm", TaskContractVersion: updated.Version, Status: ApprovalPending, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.UpdateTaskContract(context.Background(), invocation.ID, TaskContract{Version: updated.Version, TaskType: TaskBuild, Goal: "再次补充", MutationAllowed: true, ValidationRequired: true}, "再补充"); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.ResolveApproval(context.Background(), "approval-contract-version", true, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("任务契约变化后审批应重新评估，实际=%v", err)
	}
	approval, err := repo.GetApproval(context.Background(), "approval-contract-version")
	if err != nil || approval.Status != ApprovalPending {
		t.Fatalf("契约变化不应提前决议审批=%#v err=%v", approval, err)
	}
}

func TestUpdateTaskContractInstallsDefaultPlanWhenTypeBecomesCoding(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-contract-plan-migration", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Message: "请先看看这个目录", Status: InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	initial, err := coordinator.GetTaskContract(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if initial.TaskType != TaskInspect {
		t.Fatalf("测试前置应为只读契约，实际=%s", initial.TaskType)
	}
	updated, err := coordinator.UpdateTaskContract(context.Background(), invocation.ID, TaskContract{
		Version: initial.Version, TaskType: TaskBuild, Goal: "实现并验证目录改动", RequestedOutcome: "代码变更和测试结果",
		MutationAllowed: true, ValidationRequired: true, SourceMessageIDs: initial.SourceMessageIDs,
	}, "用户将只读请求升级为实现任务")
	if err != nil {
		t.Fatal(err)
	}
	if updated.TaskType != TaskBuild || updated.Version != initial.Version+1 {
		t.Fatalf("更新后的 coding 契约=%#v", updated)
	}
	plan, err := coordinator.GetTaskPlan(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 3 || plan.Steps[0].ID != "inspect" || plan.Steps[1].ID != "edit" || plan.Steps[2].ID != "verify" {
		t.Fatalf("契约升级后应安装 inspect→edit→verify 默认计划=%#v", plan)
	}
}

func TestTaskContractValidationAndClassificationAreConservative(t *testing.T) {
	if got := ClassifyTask("请检查这个 diff"); got != TaskInspect {
		t.Fatalf("只读检查应分类为 inspect，实际=%s", got)
	}
	if got := ClassifyTask("请诊断为什么请求失败"); got != TaskDiagnose {
		t.Fatalf("诊断请求应分类为 diagnose，实际=%s", got)
	}
	contract := InitialTaskContract("inv-classify", "请检查代码", time.Now())
	if contract.MutationAllowed {
		t.Fatal("检查任务默认不应允许修改")
	}
	contract.AcceptanceCriteria = []Criterion{{ID: "same", Description: "a"}, {ID: "same", Description: "b"}}
	if !errors.Is(contract.Validate(), ErrInvalidTaskContract) {
		t.Fatal("重复验收条件 ID 应被拒绝")
	}
	contract = InitialTaskContract("inv-change", "请修复问题", time.Now())
	contract.MutationAllowed = false
	if !errors.Is(contract.Validate(), ErrInvalidTaskContract) {
		t.Fatal("修改任务关闭 mutation_allowed 应被拒绝")
	}
}

func TestResolveApprovalBlocksChangedModelCapabilitySnapshot(t *testing.T) {
	now := time.Now().UTC()
	providerRepo := &runtimeProviderRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", BaseURL: "http://127.0.0.1:1", Protocol: provider.ProtocolOpenAICompatible,
			Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true}}},
	}}
	registry, err := provider.NewRegistry(context.Background(), providerRepo, map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: &runtimeApprovalAdapter{model: &runtimeApprovalModel{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{AppName: "snapshot-invalidation", SessionService: session.InMemoryService(), Providers: registry, Instruction: "test"})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-capability-change", UserID: "user", ConversationID: "conversation", SessionID: "conversation", ProviderID: "demo", ModelID: "model", Status: InvocationWaitingApproval, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateApproval(context.Background(), Approval{ID: "approval-capability-change", InvocationID: invocation.ID, ToolName: "guarded", OriginalCallID: "call", ConfirmationCallID: "confirm", Status: ApprovalPending, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	current, err := kernel.CurrentModelCapabilityProfile(context.Background(), invocation.ProviderID, invocation.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SaveModelCapabilitySnapshot(context.Background(), ModelCapabilitySnapshot{ID: "cap-snapshot", InvocationID: invocation.ID, Result: provider.NegotiationResult{ProfileSnapshotID: current.SnapshotID(), Profile: current, Compatible: true}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	changed := current
	changed.Streaming = provider.Support{State: provider.SupportDegraded, Source: "user_override"}
	providerRepo.providers["demo"] = provider.Provider{ID: "demo", Name: "demo", BaseURL: "http://127.0.0.1:1", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true, Capabilities: &changed}}}
	if err := registry.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Coordinator{kernel: kernel, repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}).ResolveApproval(context.Background(), "approval-capability-change", true, "approve"); !errors.Is(err, ErrConflict) {
		t.Fatalf("能力 Snapshot 变化后应阻止恢复，实际=%v", err)
	}
	approval, err := repo.GetApproval(context.Background(), "approval-capability-change")
	if err != nil || approval.Status != ApprovalPending {
		t.Fatalf("阻止恢复不应决议审批=%#v err=%v", approval, err)
	}
	events, err := repo.ListEvents(context.Background(), invocation.ID, 0, 20)
	if err != nil || len(events) != 1 || events[0].Type != EventModelMigrationRequired {
		t.Fatalf("应记录 model.migration_required 事件=%#v err=%v", events, err)
	}
}

func TestResolveApprovalBlocksChangedRuntimeConfigSnapshot(t *testing.T) {
	providerRepo := &runtimeProviderRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", BaseURL: "http://127.0.0.1:1", Protocol: provider.ProtocolOpenAICompatible,
			Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true}}},
	}}
	registry, err := provider.NewRegistry(context.Background(), providerRepo, map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: &runtimeApprovalAdapter{model: &runtimeApprovalModel{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	instruction := "stable instruction"
	kernel, err := agent.NewKernel(agent.Config{
		AppName: "runtime-config-change", SessionService: session.InMemoryService(), Providers: registry,
		RuntimeConfigResolver: func(context.Context, string, string, string) (agent.RuntimeOptions, error) {
			return agent.RuntimeOptions{AIEnabled: true, ProviderID: "demo", ModelID: "model", Instruction: instruction}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, encoded, err := kernel.ResolveRuntimeConfigSnapshot(context.Background(), agent.ChatRequest{UserID: "user", ConversationID: "conversation", SessionID: "conversation", ProviderID: "demo", ModelID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-runtime-config-change", UserID: "user", ConversationID: "conversation", SessionID: "conversation", ProviderID: "demo", ModelID: "model", ConfigSnapshot: encoded, Status: InvocationWaitingApproval, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateApproval(context.Background(), Approval{ID: "approval-runtime-config-change", InvocationID: invocation.ID, ToolName: "guarded", OriginalCallID: "call", ConfirmationCallID: "confirm", Status: ApprovalPending, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	instruction = "changed instruction"
	coordinator := &Coordinator{kernel: kernel, repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	if _, err := coordinator.ResolveApproval(context.Background(), "approval-runtime-config-change", true, "approve"); !errors.Is(err, ErrConflict) {
		t.Fatalf("Runtime 配置变化后应阻止审批恢复，实际=%v", err)
	}
	approval, err := repo.GetApproval(context.Background(), "approval-runtime-config-change")
	if err != nil || approval.Status != ApprovalPending {
		t.Fatalf("配置变化不应决议审批=%#v err=%v", approval, err)
	}
	events, err := repo.ListEvents(context.Background(), invocation.ID, 0, 20)
	if err != nil || len(events) != 1 || events[0].Type != EventRuntimeConfigChanged {
		t.Fatalf("应记录 runtime.config_changed 事件=%#v err=%v", events, err)
	}
}

func TestResolveApprovalBlocksChangedToolCatalogSnapshot(t *testing.T) {
	now := time.Now().UTC()
	providerRepo := &runtimeProviderRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", BaseURL: "http://127.0.0.1:1", Protocol: provider.ProtocolOpenAICompatible,
			Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true}}},
	}}
	registry, err := provider.NewRegistry(context.Background(), providerRepo, map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: &runtimeApprovalAdapter{model: &runtimeApprovalModel{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	toolRegistry := agent.NewToolRegistry()
	first, err := toolRegistry.RegisterRuntimeTool(snapshotToolTestDouble{}, agent.ToolSourceBuiltin)
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{AppName: "tool-snapshot-invalidation", SessionService: session.InMemoryService(), Providers: registry, ToolRegistry: toolRegistry, Instruction: "test"})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-tool-change", UserID: "user", ConversationID: "conversation", SessionID: "conversation", ProviderID: "demo", ModelID: "model", Status: InvocationWaitingApproval, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateApproval(context.Background(), Approval{ID: "approval-tool-change", InvocationID: invocation.ID, ToolName: first.ModelName, OriginalCallID: "call", ConfirmationCallID: "confirm", Status: ApprovalPending, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SaveToolSetSnapshot(context.Background(), ToolSetSnapshot{ID: "tool-snapshot", InvocationID: invocation.ID, CatalogRevision: toolRegistry.CatalogRevision(), Tools: []ToolRef{{ID: first.ID, ModelName: first.ModelName, Version: first.Version, SchemaDigest: first.SchemaDigest}}, Digest: "digest", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := toolRegistry.RegisterRuntimeTool(anotherExecutableToolTestDouble{}, agent.ToolSourceBuiltin); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{kernel: kernel, repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	if _, err := coordinator.ResolveApproval(context.Background(), "approval-tool-change", true, "approve"); !errors.Is(err, ErrConflict) {
		t.Fatalf("工具目录变化后应阻止恢复，实际=%v", err)
	}
	approval, err := repo.GetApproval(context.Background(), "approval-tool-change")
	if err != nil || approval.Status != ApprovalPending {
		t.Fatalf("阻止恢复不应决议审批=%#v err=%v", approval, err)
	}
	events, err := repo.ListEvents(context.Background(), invocation.ID, 0, 20)
	if err != nil || len(events) != 1 || events[0].Type != EventToolSetInvalidated {
		t.Fatalf("应记录 toolset.snapshot_invalidated 事件=%#v err=%v", events, err)
	}
}

func TestReconfirmInstructionsReplacesSnapshotWithoutResolvingApproval(t *testing.T) {
	now := time.Now().UTC()
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-reconfirm", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationWaitingApproval, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateApproval(context.Background(), Approval{ID: "approval-reconfirm", InvocationID: invocation.ID, Status: ApprovalPending, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	coordinator.SetInstructionSnapshotReconfirmer(func(_ context.Context, invocationID string) (InstructionSnapshotSet, error) {
		return repo.ReplaceInstructionSnapshotSet(context.Background(), InstructionSnapshotSet{InvocationID: invocationID, TargetPath: "src", Snapshots: []InstructionSnapshot{{Path: "AGENTS.md", ScopePath: ".", Source: "workspace", ContentDigest: "new"}}})
	})
	snapshot, err := coordinator.ReconfirmInstructions(context.Background(), invocation.ID)
	if err != nil || snapshot.Revision != 1 || len(snapshot.Snapshots) != 1 || snapshot.Snapshots[0].ContentDigest != "new" {
		t.Fatalf("重新确认快照=%#v err=%v", snapshot, err)
	}
	approval, err := repo.GetApproval(context.Background(), "approval-reconfirm")
	if err != nil || approval.Status != ApprovalPending {
		t.Fatalf("重新确认不应自动决议审批=%#v err=%v", approval, err)
	}
	events, err := repo.ListEvents(context.Background(), invocation.ID, 0, 20)
	if err != nil || len(events) != 1 || events[0].Type != EventInstructionReconfirmed {
		t.Fatalf("应记录 instruction.reconfirmed 事件=%#v err=%v", events, err)
	}
}

type anotherExecutableToolTestDouble struct{}

type snapshotToolTestDouble struct{}

func (snapshotToolTestDouble) Name() string        { return "snapshot_first" }
func (snapshotToolTestDouble) Description() string { return "test" }
func (snapshotToolTestDouble) IsLongRunning() bool { return false }
func (snapshotToolTestDouble) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: "snapshot_first"}
}
func (snapshotToolTestDouble) ProcessRequest(_ adkagent.Context, _ *adkmodel.LLMRequest) error {
	return nil
}
func (snapshotToolTestDouble) Run(_ adkagent.Context, _ any) (map[string]any, error) {
	return map[string]any{"ok": true}, nil
}

func (anotherExecutableToolTestDouble) Name() string        { return "another" }
func (anotherExecutableToolTestDouble) Description() string { return "test" }
func (anotherExecutableToolTestDouble) IsLongRunning() bool { return false }
func (anotherExecutableToolTestDouble) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: "another"}
}
func (anotherExecutableToolTestDouble) ProcessRequest(_ adkagent.Context, _ *adkmodel.LLMRequest) error {
	return nil
}
func (anotherExecutableToolTestDouble) Run(_ adkagent.Context, _ any) (map[string]any, error) {
	return map[string]any{"ok": true}, nil
}

func TestMemoryRepositoryPersistsContextManifestByModelCall(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	if err := repo.CreateInvocation(context.Background(), Invocation{ID: "inv-manifest", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationQueued, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	manifest := ContextManifest{ID: "manifest-1", InvocationID: "inv-manifest", ModelCallID: "inv-manifest:model:1", BuilderVersion: "context-builder-v1", EstimatorVersion: "heuristic-v1", Model: "demo", EstimatedInput: 42, Digest: "digest", CreatedAt: now}
	if err := repo.SaveContextManifest(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetContextManifest(context.Background(), manifest.ModelCallID)
	if err != nil || got.Digest != manifest.Digest {
		t.Fatalf("读取 manifest=%#v err=%v", got, err)
	}
	items, err := repo.ListContextManifests(context.Background(), manifest.InvocationID)
	if err != nil || len(items) != 1 || items[0].ModelCallID != manifest.ModelCallID {
		t.Fatalf("manifest 列表=%#v err=%v", items, err)
	}
}

func TestCoordinatorRebuildsRuntimeSnapshotFromDurableState(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-snapshot", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Message: "修复并验证", Status: InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateTaskPlan(context.Background(), initialTaskPlan(invocation.ID, now)); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateTaskContract(context.Background(), InitialTaskContract(invocation.ID, invocation.Message, now)); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	first, err := coordinator.RebuildRuntimeSnapshot(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != 1 || first.Phase != string(InvocationQueued) || first.ContractVersion != 1 || first.PlanRevision != 1 {
		t.Fatalf("初始 Runtime Snapshot=%#v", first)
	}
	if _, err := repo.TransitionInvocation(context.Background(), invocation.ID, InvocationQueued, InvocationRunning, ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateApproval(context.Background(), Approval{ID: "approval-snapshot", InvocationID: invocation.ID, ToolName: "guarded", OriginalCallID: "call", ConfirmationCallID: "confirm", Status: ApprovalPending, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateToolCall(context.Background(), ToolCall{ID: "tool-snapshot", InvocationID: invocation.ID, ToolName: "guarded", OriginalCallID: "call", Status: ToolCallFailed, Error: "验证失败", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	second, err := coordinator.RebuildRuntimeSnapshot(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision != 2 || second.Phase != string(InvocationRunning) || len(second.PendingApprovals) != 1 || len(second.Blockers) != 1 || second.Blockers[0].Code != "tool_failed" {
		t.Fatalf("更新后的 Runtime Snapshot=%#v", second)
	}
	events, err := repo.ListEvents(context.Background(), invocation.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var snapshotEvents int
	for _, event := range events {
		if event.Type == EventRuntimeSnapshot {
			snapshotEvents++
		}
	}
	if snapshotEvents != 2 {
		t.Fatalf("Runtime Snapshot 事件数量=%d，事件=%#v", snapshotEvents, events)
	}
}

func TestRuntimeSnapshotTerminalRefreshIsReplayable(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-snapshot-terminal", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	coordinator.finish(context.Background(), invocation.ID, InvocationCompleted, "")
	snapshot, err := repo.GetRuntimeSnapshot(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Phase != string(InvocationCompleted) {
		t.Fatalf("终态快照 phase=%q", snapshot.Phase)
	}
	events, err := repo.ListEvents(context.Background(), invocation.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != EventRuntimeSnapshot || events[1].Type != EventInvocationCompleted {
		t.Fatalf("终态快照/终态事件顺序=%#v", events)
	}
}

func TestFinishBlocksIncompletePlanBeforeTerminalEvent(t *testing.T) {
	tests := []struct {
		name       string
		status     InvocationStatus
		planStatus PlanStatus
		current    string
		wantCode   string
	}{
		{name: "active step", status: InvocationCompleted, planStatus: PlanInProgress, current: "inspect", wantCode: "invocation_completed_before_plan_complete"},
		{name: "pending plan", status: InvocationCancelled, planStatus: PlanPending, wantCode: "invocation_cancelled_before_plan_complete"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := NewMemoryRepository()
			now := time.Now().UTC()
			invocation := Invocation{ID: "inv-terminal-plan-" + strings.ReplaceAll(test.name, " ", "-"), UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
			if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
				t.Fatal(err)
			}
			steps := []PlanStep{{ID: "inspect", Title: "检查工作区", Status: PlanStepPending, UpdatedAt: now}, {ID: "edit", Title: "修改文件", Status: PlanStepPending, DependsOn: []string{"inspect"}, UpdatedAt: now}}
			if test.current != "" {
				steps[0].Status = PlanStepInProgress
			}
			plan := TaskPlan{ID: "plan-" + invocation.ID, InvocationID: invocation.ID, Revision: 1, Status: test.planStatus, CurrentStepID: test.current, Steps: steps, CreatedAt: now, UpdatedAt: now}
			if err := repo.CreateTaskPlan(context.Background(), plan); err != nil {
				t.Fatal(err)
			}
			coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
			coordinator.finish(context.Background(), invocation.ID, test.status, "")

			current, err := repo.GetInvocation(context.Background(), invocation.ID)
			if err != nil || current.Status != test.status {
				t.Fatalf("Invocation 应进入终态: %#v err=%v", current, err)
			}
			updated, err := repo.GetTaskPlan(context.Background(), invocation.ID)
			if err != nil {
				t.Fatal(err)
			}
			if updated.Status != PlanBlocked || updated.CurrentStepID != "" || !strings.Contains(updated.Blocker, test.wantCode) {
				t.Fatalf("终态前未安全收口计划: %#v", updated)
			}
			if test.current != "" && updated.Steps[0].Status != PlanStepBlocked {
				t.Fatalf("活动步骤应标记 blocked: %#v", updated.Steps)
			}
			events, err := repo.ListEvents(context.Background(), invocation.ID, 0, 20)
			if err != nil {
				t.Fatal(err)
			}
			planSequence, terminalSequence := int64(0), int64(0)
			for _, event := range events {
				switch event.Type {
				case EventPlanUpdated:
					planSequence = event.Sequence
				case terminalEventForStatus(test.status):
					terminalSequence = event.Sequence
				}
			}
			if planSequence == 0 || terminalSequence == 0 || planSequence >= terminalSequence {
				t.Fatalf("计划收口必须先于终态事件: %#v", events)
			}
			report, err := coordinator.GetCompletionReport(context.Background(), invocation.ID)
			if err != nil {
				t.Fatal(err)
			}
			if report.Status != WorkflowResultBlocked {
				t.Fatalf("缺少计划证据时不能报告 completed: %#v", report)
			}
		})
	}
}

func TestCoordinatorBackfillsManifestPromptUsage(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-usage", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	manifest := ContextManifest{ID: "manifest-usage", InvocationID: invocation.ID, ModelCallID: "model-usage", BuilderVersion: "context-builder-v1", EstimatorVersion: "heuristic-v1", Model: "demo", EstimatedInput: 20, Digest: "digest", CreatedAt: now}
	if err := repo.SaveContextManifest(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	coordinator.recordManifestUsage(context.Background(), invocation.ID, map[string]any{"usage": map[string]any{"promptTokenCount": float64(17)}})
	got, err := repo.GetContextManifest(context.Background(), manifest.ModelCallID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ActualInput == nil || *got.ActualInput != 17 {
		t.Fatalf("Manifest actual usage=%#v", got.ActualInput)
	}
	coordinator.recordManifestUsage(context.Background(), invocation.ID, map[string]any{"usage": map[string]any{"promptTokenCount": -1}})
	got, _ = repo.GetContextManifest(context.Background(), manifest.ModelCallID)
	if got.ActualInput == nil || *got.ActualInput != 17 {
		t.Fatalf("非法 usage 不应覆盖已知值=%#v", got.ActualInput)
	}
}

func TestCoordinatorBackfillsManifestUsageAcrossProviderShapes(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-usage-provider-shapes", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	manifest := ContextManifest{ID: "manifest-usage-provider-shapes", InvocationID: invocation.ID, ModelCallID: "model-usage-provider-shapes", BuilderVersion: "context-builder-v1", EstimatorVersion: "heuristic-v1", Model: "demo", EstimatedInput: 20, Digest: "digest", CreatedAt: now}
	if err := repo.SaveContextManifest(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	coordinator.recordManifestUsage(context.Background(), invocation.ID, map[string]any{"usageMetadata": map[string]any{"input_tokens": "17"}})
	got, err := repo.GetContextManifest(context.Background(), manifest.ModelCallID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ActualInput == nil || *got.ActualInput != 17 {
		t.Fatalf("OpenAI input_tokens 未回填 Manifest: %#v", got.ActualInput)
	}
	// A fractional value must remain unknown and must not erase a valid sample.
	coordinator.recordManifestUsage(context.Background(), invocation.ID, map[string]any{"usage": map[string]any{"prompt_tokens": 17.5}})
	got, _ = repo.GetContextManifest(context.Background(), manifest.ModelCallID)
	if got.ActualInput == nil || *got.ActualInput != 17 {
		t.Fatalf("非法 provider usage 不应覆盖已知 Manifest: %#v", got.ActualInput)
	}
}

func TestMemoryRepositoryPersistsWorkingSetWithCAS(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-working-set", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	initial, err := coordinator.GetWorkingSet(context.Background(), invocation.ID)
	if err != nil || initial.Revision != 1 {
		t.Fatalf("初始 Working Set=%#v err=%v", initial, err)
	}
	updated, err := coordinator.UpdateWorkingSet(context.Background(), invocation.ID, WorkingSet{Revision: initial.Revision, Items: []WorkingSetItem{{ID: "slice", Kind: "file_slice", SourceRef: "file:a", Priority: 1, RelevanceScore: 0.8, TokenEstimate: 5, ContentDigest: "sha", Freshness: agent.WorkingSetFresh, CreatedAt: now, ValidatedAt: now}}}, "读取入口")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 2 || len(updated.Items) != 1 {
		t.Fatalf("更新 Working Set=%#v", updated)
	}
	if _, err := coordinator.UpdateWorkingSet(context.Background(), invocation.ID, WorkingSet{Revision: initial.Revision}, "stale"); !errors.Is(err, ErrConflict) {
		t.Fatalf("旧 Working Set revision 应冲突，实际=%v", err)
	}
	snapshot, err := coordinator.RebuildRuntimeSnapshot(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.WorkingSetRevision != updated.Revision {
		t.Fatalf("snapshot working_set_revision=%d, working set=%d", snapshot.WorkingSetRevision, updated.Revision)
	}
}

func TestCoordinatorPersistsVerificationEvidenceWithCAS(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-verification", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	run, err := coordinator.CreateVerificationRun(context.Background(), VerificationRun{InvocationID: invocation.ID, Kind: "command", PlanStepID: "verify", Command: "go test ./...", Status: VerificationRunning})
	if err != nil {
		t.Fatal(err)
	}
	if run.Revision != 1 || run.Status != VerificationRunning {
		t.Fatalf("创建 verification=%#v", run)
	}
	exitCode := 1
	updated, err := coordinator.UpdateVerificationRun(context.Background(), VerificationRun{ID: run.ID, Kind: run.Kind, PlanStepID: run.PlanStepID, Command: run.Command, Status: VerificationFailed, ExitCode: &exitCode, Summary: "测试失败"}, 1, "记录失败")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 2 || updated.Status != VerificationFailed || updated.ExitCode == nil || *updated.ExitCode != 1 {
		t.Fatalf("更新 verification=%#v", updated)
	}
	if _, err := coordinator.UpdateVerificationRun(context.Background(), VerificationRun{ID: run.ID, Kind: run.Kind, Status: VerificationPassed}, 1, "stale"); !errors.Is(err, ErrConflict) {
		t.Fatalf("旧 verification revision 应冲突，实际=%v", err)
	}
	snapshot, err := coordinator.RebuildRuntimeSnapshot(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.VerificationRuns) != 1 || snapshot.VerificationRuns[0].Status != string(VerificationFailed) || snapshot.VerificationRuns[0].Summary != "测试失败" {
		t.Fatalf("snapshot verification refs=%#v", snapshot.VerificationRuns)
	}
}

func TestVerificationPassedCompletesActivePlanStepWithEvidence(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-verification-link", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	plan := TaskPlan{ID: "plan-verification-link", InvocationID: invocation.ID, Revision: 1, Status: PlanInProgress, CurrentStepID: "verify", Steps: []PlanStep{{ID: "verify", Title: "运行验证", Status: PlanStepInProgress, UpdatedAt: now}}, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateTaskPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	run, err := coordinator.CreateVerificationRun(context.Background(), VerificationRun{InvocationID: invocation.ID, PlanStepID: "verify", Kind: "command", Command: "go test ./...", Status: VerificationRunning})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := coordinator.UpdateVerificationRun(context.Background(), VerificationRun{ID: run.ID, Status: VerificationPassed, Summary: "通过"}, run.Revision, "测试通过")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != VerificationPassed || updated.PlanStepID != "verify" || updated.Kind != "command" || updated.Command != "go test ./..." {
		t.Fatalf("更新应保留验证元数据: %#v", updated)
	}
	if updated.StartedAt == nil || updated.FinishedAt == nil {
		t.Fatalf("running/passed 验证应补齐生命周期时间戳: %#v", updated)
	}
	linked, err := coordinator.GetTaskPlan(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if linked.Status != PlanCompleted || linked.CurrentStepID != "" || linked.Revision != 2 {
		t.Fatalf("验证通过后计划应完成当前步骤: %#v", linked)
	}
	if len(linked.Steps) != 1 || linked.Steps[0].Status != PlanStepCompleted || len(linked.Steps[0].Evidence) != 1 || linked.Steps[0].Evidence[0].Kind != "verification" || linked.Steps[0].Evidence[0].Ref != run.ID {
		t.Fatalf("计划应保存 verification evidence: %#v", linked.Steps)
	}
	events, err := repo.ListEvents(context.Background(), invocation.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	var planUpdates int
	for _, event := range events {
		if event.Type == EventPlanUpdated {
			planUpdates++
		}
	}
	if planUpdates != 1 {
		t.Fatalf("验证桥接应只产生一次权威 plan.updated，events=%#v", events)
	}
}

func TestVerificationEvidenceAdmitsNextRunnablePlanStep(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-verification-next", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	plan := TaskPlan{ID: "plan-verification-next", InvocationID: invocation.ID, Revision: 1, Status: PlanInProgress, CurrentStepID: "verify", Steps: []PlanStep{
		{ID: "verify", Title: "运行测试", Status: PlanStepInProgress, UpdatedAt: now},
		{ID: "report", Title: "报告结果", Status: PlanStepPending, DependsOn: []string{"verify"}, UpdatedAt: now},
	}, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateTaskPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	if _, err := coordinator.CreateVerificationRun(context.Background(), VerificationRun{ID: "verification-next", InvocationID: invocation.ID, PlanStepID: "verify", Kind: "command", Command: "go test ./...", Status: VerificationPassed, Summary: "通过"}); err != nil {
		t.Fatal(err)
	}
	updated, err := coordinator.GetTaskPlan(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.CurrentStepID != "report" || updated.Status != PlanInProgress || updated.Steps[0].Status != PlanStepCompleted || updated.Steps[1].Status != PlanStepInProgress {
		t.Fatalf("验证证据完成后应自动接续下一个依赖就绪步骤: %#v", updated)
	}
	if updated.Revision != 3 {
		t.Fatalf("完成当前步骤并接续下一步骤应各产生一次 CAS 修订: %#v", updated)
	}
}

func TestLateVerificationDoesNotReopenTerminalInvocation(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-verification-terminal", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationCompleted, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	plan := TaskPlan{ID: "plan-verification-terminal", InvocationID: invocation.ID, Revision: 1, Status: PlanInProgress, CurrentStepID: "verify", Steps: []PlanStep{
		{ID: "verify", Title: "运行测试", Status: PlanStepInProgress, UpdatedAt: now},
		{ID: "report", Title: "报告结果", Status: PlanStepPending, DependsOn: []string{"verify"}, UpdatedAt: now},
	}, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateTaskPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	if _, err := coordinator.UpdateTaskPlan(context.Background(), invocation.ID, plan, "late direct edit"); !errors.Is(err, ErrConflict) {
		t.Fatalf("终态 invocation 不应接受普通计划更新: %v", err)
	}
	unchanged, err := coordinator.GetTaskPlan(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Revision != plan.Revision || unchanged.CurrentStepID != plan.CurrentStepID {
		t.Fatalf("被拒绝的终态计划更新不能落盘: %#v", unchanged)
	}
	if _, err := coordinator.CreateVerificationRun(context.Background(), VerificationRun{ID: "verification-terminal", InvocationID: invocation.ID, PlanStepID: "verify", Kind: "command", Status: VerificationPassed}); err != nil {
		t.Fatal(err)
	}
	updated, err := coordinator.GetTaskPlan(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.CurrentStepID != "" || updated.Status != PlanPending || updated.Steps[0].Status != PlanStepCompleted || updated.Steps[1].Status != PlanStepPending {
		t.Fatalf("终态 invocation 的迟到验证不应重新打开后续步骤: %#v", updated)
	}
}

func TestVerificationPassedRequiresActivePlanStep(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-verification-link-invalid", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	plan := TaskPlan{ID: "plan-verification-link-invalid", InvocationID: invocation.ID, Revision: 1, Status: PlanPending, Steps: []PlanStep{{ID: "verify", Title: "运行验证", Status: PlanStepPending, UpdatedAt: now}}, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateTaskPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	_, err := coordinator.CreateVerificationRun(context.Background(), VerificationRun{ID: "verification-invalid", InvocationID: invocation.ID, PlanStepID: "verify", Kind: "command", Status: VerificationPassed})
	if !errors.Is(err, ErrVerificationPlanLink) {
		t.Fatalf("非活动计划步骤不应被验证通过自动完成: %v", err)
	}
	if _, getErr := repo.GetVerificationRun(context.Background(), "verification-invalid"); !errors.Is(getErr, ErrNotFound) {
		t.Fatalf("桥接前置校验失败时不应写入 VerificationRun: %v", getErr)
	}
}

func TestRecordWorkspaceOperationProjectsVerificationAndIsIdempotent(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-operation-verification", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	plan := TaskPlan{ID: "plan-operation-verification", InvocationID: invocation.ID, Revision: 1, Status: PlanInProgress, CurrentStepID: "tests", Steps: []PlanStep{{ID: "tests", Title: "运行测试", Status: PlanStepInProgress, UpdatedAt: now}}, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateTaskPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	exitCode := 0
	operation := workspace.Operation{ID: "operation-test-1", InvocationID: invocation.ID, Type: workspace.OperationCommand, Command: "go test ./...", Status: workspace.OperationCompleted, Result: "通过", ExitCode: &exitCode, OutputDigest: "digest-tests", DurationMS: 42}
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			coordinator.RecordWorkspaceOperation(context.Background(), operation)
		}()
	}
	workers.Wait()
	runs, err := coordinator.ListVerificationRuns(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != operationVerificationID(operation.ID) || runs[0].Status != VerificationPassed || runs[0].PlanStepID != "tests" {
		t.Fatalf("命令结果应投影为唯一 verification: %#v", runs)
	}
	if runs[0].ExitCode == nil || *runs[0].ExitCode != 0 || runs[0].OutputDigest != operation.OutputDigest || runs[0].Command != operation.Command {
		t.Fatalf("命令元数据应完整投影到 verification: %#v", runs[0])
	}
	linked, err := coordinator.GetTaskPlan(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if linked.Status != PlanCompleted || len(linked.Steps[0].Evidence) != 1 || linked.Steps[0].Evidence[0].Ref != runs[0].ID {
		t.Fatalf("verification 应完成测试步骤并保存 evidence: %#v", linked)
	}
}

func TestRecordCommandRunProjectsLifecycleMetadataEvents(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-command-events", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	exitCode := 0
	for _, run := range []workspace.CommandRun{
		{ID: "run-queued", InvocationID: invocation.ID, OperationID: "operation-command-events", Executor: "local", Mode: "shell", Capabilities: workspace.DefaultCommandExecutionCapabilities(workspace.Workspace{Type: workspace.TypeLocal}), Status: workspace.CommandRunQueued, Revision: 1, UpdatedAt: now},
		{ID: "run-started", InvocationID: invocation.ID, OperationID: "operation-command-events", Executor: "local", Mode: "shell", Status: workspace.CommandRunRunning, Revision: 3, UpdatedAt: now.Add(time.Millisecond)},
		{ID: "run-completed", InvocationID: invocation.ID, OperationID: "operation-command-events", Executor: "local", Mode: "shell", Status: workspace.CommandRunExited, Outcome: workspace.CommandOutcomeSuccess, ExitCode: &exitCode, StdoutBytes: 2, StoredBytes: 2, OutputDigest: "digest", Revision: 4, UpdatedAt: now.Add(2 * time.Millisecond)},
	} {
		coordinator.RecordCommandRun(context.Background(), run)
	}
	events, err := repo.ListEvents(context.Background(), invocation.ID, 0, 20)
	if err != nil || len(events) != 3 {
		t.Fatalf("CommandRun 生命周期事件数量不正确: %#v err=%v", events, err)
	}
	if events[0].Type != EventCommandQueued || events[1].Type != EventCommandStarted || events[2].Type != EventCommandCompleted {
		t.Fatalf("CommandRun 生命周期事件类型不正确: %#v", events)
	}
	if events[2].Data["command_run_id"] != "run-completed" || events[2].Data["exit_code"] != 0 || events[2].Data["output_digest"] != "digest" {
		t.Fatalf("CommandRun 事件缺少元数据: %#v", events[2])
	}
	if capabilities, ok := events[0].Data["capabilities"].(workspace.CommandExecutionCapabilities); !ok || capabilities.IsolationLevel != "l0_host_process" || capabilities.Network.State != workspace.ExecutionCapabilityUnsupported {
		t.Fatalf("CommandRun 事件必须携带能力披露: %#v", events[0].Data["capabilities"])
	}
}

func TestRecordWorkspaceOperationMapsKnownNonzeroCommandToFailedVerification(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-operation-nonzero", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	plan := TaskPlan{ID: "plan-operation-nonzero", InvocationID: invocation.ID, Revision: 1, Status: PlanInProgress, CurrentStepID: "verify", Steps: []PlanStep{{ID: "verify", Title: "运行测试", Status: PlanStepInProgress, UpdatedAt: now}}, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateTaskPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	exitCode := 3
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	coordinator.RecordWorkspaceOperation(context.Background(), workspace.Operation{ID: "operation-nonzero", InvocationID: invocation.ID, Type: workspace.OperationCommand, Command: "go test ./...", Status: workspace.OperationCompleted, CommandOutcome: string(workspace.CommandOutcomeNonzero), ExitCode: &exitCode, Result: "失败", Error: "退出码 3"})
	runs, err := coordinator.ListVerificationRuns(context.Background(), invocation.ID)
	if err != nil || len(runs) != 1 || runs[0].Status != VerificationFailed || runs[0].OutputRef != "" {
		t.Fatalf("已知非零命令应生成 failed verification: %#v err=%v", runs, err)
	}
}

func TestRecordWorkspaceOperationKeepsPlanActiveOnFailureOrUnknown(t *testing.T) {
	for _, status := range []workspace.OperationStatus{workspace.OperationFailed, workspace.OperationUnknown} {
		t.Run(string(status), func(t *testing.T) {
			repo := NewMemoryRepository()
			now := time.Now().UTC()
			invocation := Invocation{ID: "inv-operation-" + string(status), UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
			if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
				t.Fatal(err)
			}
			plan := TaskPlan{ID: "plan-operation-" + string(status), InvocationID: invocation.ID, Revision: 1, Status: PlanInProgress, CurrentStepID: "tests", Steps: []PlanStep{{ID: "tests", Title: "运行测试", Status: PlanStepInProgress, UpdatedAt: now}}, CreatedAt: now, UpdatedAt: now}
			if err := repo.CreateTaskPlan(context.Background(), plan); err != nil {
				t.Fatal(err)
			}
			coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
			coordinator.RecordWorkspaceOperation(context.Background(), workspace.Operation{ID: "operation-failure-" + string(status), InvocationID: invocation.ID, Type: workspace.OperationCommand, Command: "go test ./...", Status: status, Error: "验证失败"})
			runs, err := coordinator.ListVerificationRuns(context.Background(), invocation.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) != 1 || runs[0].Status == VerificationPassed {
				t.Fatalf("失败/unknown 应保留为非通过 verification: %#v", runs)
			}
			linked, err := coordinator.GetTaskPlan(context.Background(), invocation.ID)
			if err != nil {
				t.Fatal(err)
			}
			if linked.Status != PlanInProgress || linked.CurrentStepID != "tests" || linked.Steps[0].Status != PlanStepInProgress {
				t.Fatalf("失败/unknown 不应自动完成活动步骤: %#v", linked)
			}
		})
	}
}

func TestRecordWorkspaceOperationCompletesEditingStepAndAdmitsVerification(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-operation-editing", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	plan := TaskPlan{ID: "plan-operation-editing", InvocationID: invocation.ID, Revision: 1, Status: PlanInProgress, CurrentStepID: "edit", Steps: []PlanStep{
		{ID: "edit", Title: "修改代码", Status: PlanStepInProgress, UpdatedAt: now},
		{ID: "verify", Title: "运行测试验证", Status: PlanStepPending, DependsOn: []string{"edit"}, UpdatedAt: now},
	}, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateTaskPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	coordinator.RecordWorkspaceOperation(context.Background(), workspace.Operation{ID: "operation-edit-1", InvocationID: invocation.ID, Type: workspace.OperationPatchSet, Status: workspace.OperationCompleted, Result: "已应用 ChangeSet"})
	updated, err := coordinator.GetTaskPlan(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.CurrentStepID != "verify" || updated.Status != PlanInProgress || updated.Steps[0].Status != PlanStepCompleted || updated.Steps[1].Status != PlanStepInProgress {
		t.Fatalf("成功编辑操作应完成 edit 并接续 verify: %#v", updated)
	}
	if len(updated.Steps[0].Evidence) != 1 || updated.Steps[0].Evidence[0].Kind != "operation" || updated.Steps[0].Evidence[0].Ref != "operation-edit-1" {
		t.Fatalf("编辑操作 evidence 不正确: %#v", updated.Steps[0].Evidence)
	}
}

func TestRecordWorkflowToolEvidenceCompletesInspectionStep(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-tool-inspection", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	plan := TaskPlan{ID: "plan-tool-inspection", InvocationID: invocation.ID, Revision: 1, Status: PlanInProgress, CurrentStepID: "inspect", Steps: []PlanStep{
		{ID: "inspect", Title: "inspect repository", Status: PlanStepInProgress, UpdatedAt: now},
		{ID: "edit", Title: "修改实现", Status: PlanStepPending, DependsOn: []string{"inspect"}, UpdatedAt: now},
	}, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateTaskPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	coordinator.recordWorkflowToolEvidence(context.Background(), invocation.ID, AgentEvent{Type: EventToolCompleted, Data: map[string]any{
		"name": "workspace_read_file", "tool_call_id": "tool-read-1", "response": map[string]any{"path": "main.go"},
	}})
	updated, err := coordinator.GetTaskPlan(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.CurrentStepID != "edit" || updated.Status != PlanInProgress || updated.Steps[0].Status != PlanStepCompleted || updated.Steps[1].Status != PlanStepInProgress {
		t.Fatalf("成功只读工具应完成 inspect 并接续 edit: %#v", updated)
	}
	if len(updated.Steps[0].Evidence) != 1 || updated.Steps[0].Evidence[0].Kind != "tool_call" || updated.Steps[0].Evidence[0].Ref != "tool-read-1" {
		t.Fatalf("检查工具 evidence 不正确: %#v", updated.Steps[0].Evidence)
	}
}

func TestCompletionReportIsConservativeAndDeterministic(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-completion-report", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationCompleted, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	contract := InitialTaskContract(invocation.ID, "修复并验证", now)
	contract.AcceptanceCriteria = []Criterion{
		{ID: "tests", Description: "测试通过", Evidence: []EvidenceRef{{Kind: "verification", Ref: "verification-report"}}},
		{ID: "review", Description: "完成最终审阅"},
	}
	if err := repo.CreateTaskContract(context.Background(), contract); err != nil {
		t.Fatal(err)
	}
	plan := TaskPlan{ID: "plan-completion-report", InvocationID: invocation.ID, Revision: 1, Status: PlanCompleted, Steps: []PlanStep{{ID: "edit", Title: "修改代码", Status: PlanStepCompleted, Evidence: []EvidenceRef{{Kind: "operation", Ref: "operation-2"}, {Kind: "operation", Ref: "operation-1"}}, UpdatedAt: now}}, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateTaskPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	if _, err := coordinator.CreateVerificationRun(context.Background(), VerificationRun{ID: "verification-report", InvocationID: invocation.ID, Kind: "command", Status: VerificationPassed, Summary: "测试通过"}); err != nil {
		t.Fatal(err)
	}
	report, err := coordinator.GetCompletionReport(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != WorkflowResultPartial || report.Summary == "" || report.ContractVersion != 1 || report.PlanRevision != 1 {
		t.Fatalf("缺少 criterion evidence 时应保守报告 partial: %#v", report)
	}
	if len(report.Criteria) != 2 || report.Criteria[0].ID != "review" || report.Criteria[0].Status != CriterionPending || report.Criteria[1].ID != "tests" || report.Criteria[1].Status != CriterionSatisfied {
		t.Fatalf("criteria 应按 ID 稳定排序并保守判断: %#v", report.Criteria)
	}
	if len(report.ChangedOperationIDs) != 2 || report.ChangedOperationIDs[0] != "operation-1" || report.ChangedOperationIDs[1] != "operation-2" {
		t.Fatalf("变更操作引用应去重并稳定排序: %#v", report.ChangedOperationIDs)
	}
	reportAgain, err := coordinator.GetCompletionReport(context.Background(), invocation.ID)
	if err != nil || reportAgain.Status != report.Status || reportAgain.Criteria[0].ID != report.Criteria[0].ID || reportAgain.ChangedOperationIDs[0] != report.ChangedOperationIDs[0] {
		t.Fatalf("重复读取报告应保持确定性: %#v err=%v", reportAgain, err)
	}
}

func TestCompletionReportBlocksUnknownVerificationAndWaitsForApproval(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-completion-blocked", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationCompleted, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	plan := TaskPlan{ID: "plan-completion-blocked", InvocationID: invocation.ID, Revision: 1, Status: PlanBlocked, Blocker: "命令状态未知", Steps: []PlanStep{{ID: "verify", Title: "验证", Status: PlanStepBlocked, Blocker: "命令状态未知", UpdatedAt: now}}, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateTaskPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	if _, err := coordinator.CreateVerificationRun(context.Background(), VerificationRun{ID: "verification-unknown", InvocationID: invocation.ID, Kind: "command", Status: VerificationUnknown, Summary: "远端未确认退出状态"}); err != nil {
		t.Fatal(err)
	}
	report, err := coordinator.GetCompletionReport(context.Background(), invocation.ID)
	if err != nil || report.Status != WorkflowResultBlocked || len(report.UnknownStates) != 1 || len(report.RemainingSteps) != 1 {
		t.Fatalf("未知验证应阻塞完成报告: %#v err=%v", report, err)
	}
	invocation.Status = InvocationWaitingApproval
	if err := repo.UpdateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	waiting, err := coordinator.GetCompletionReport(context.Background(), invocation.ID)
	if err != nil || waiting.Status != WorkflowResultWaiting {
		t.Fatalf("等待审批应显示 waiting: %#v err=%v", waiting, err)
	}
}

func TestCompletionReportNeverClaimsCompletedWithPendingEvidence(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-completion-pending", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationCompleted, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	plan := TaskPlan{ID: "plan-completion-pending", InvocationID: invocation.ID, Revision: 1, Status: PlanCompleted, Steps: []PlanStep{{ID: "done", Title: "完成", Status: PlanStepCompleted, Evidence: []EvidenceRef{{Kind: "event", Ref: "done"}}, UpdatedAt: now}}, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateTaskPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	if _, err := coordinator.CreateVerificationRun(context.Background(), VerificationRun{ID: "verification-pending", InvocationID: invocation.ID, Kind: "command", Status: VerificationPending, Summary: "尚未执行"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateApproval(context.Background(), Approval{ID: "approval-pending", InvocationID: invocation.ID, ToolName: "write", Status: ApprovalPending, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	report, err := coordinator.GetCompletionReport(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != WorkflowResultBlocked || len(report.PendingApprovals) != 1 || len(report.Blockers) < 2 {
		t.Fatalf("待审批/待验证状态不能被报告为 completed: %#v", report)
	}
}

func TestCompletionReportBlocksWorkspaceWithoutAdmissionBaseline(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-completion-no-baseline", UserID: "user", ConversationID: "conversation", SessionID: "conversation", WorkspaceID: "workspace", Status: InvocationCompleted, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	report, err := coordinator.GetCompletionReport(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != WorkflowResultBlocked || len(report.UnknownStates) != 1 || report.UnknownStates[0].Code != "baseline_missing" {
		t.Fatalf("缺少工作区 admission baseline 必须阻断归因: %#v", report)
	}
}

func TestCompletionReportProjectsAgentChangesAndOverlapConservatively(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-agent-changes", UserID: "user", ConversationID: "conversation", SessionID: "conversation", WorkspaceID: "workspace", Status: InvocationCompleted, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	baseline, err := (WorktreeBaseline{InvocationID: invocation.ID, WorkspaceID: invocation.WorkspaceID, RepositoryType: "git", HeadRevision: "head-1", Branch: "main", StatusDigest: "sha256:baseline", ChangedPaths: []PathStatus{{Path: "README.md", Status: " M"}}, StatusKnown: true, CapturedAt: now}).Normalize(now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SaveWorkspaceBaseline(context.Background(), baseline); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	coordinator.SetWorkspaceOperationResolver(func(context.Context, string, string) ([]workspace.Operation, error) {
		return []workspace.Operation{
			{ID: "operation-command", InvocationID: invocation.ID, Type: workspace.OperationCommand, Status: workspace.OperationCompleted},
			{ID: "operation-patch", InvocationID: invocation.ID, Type: workspace.OperationPatchSet, Status: workspace.OperationCompleted, Patches: []workspace.FilePatch{{Path: "z.go"}, {Path: "README.md"}}},
			{ID: "operation-rejected", InvocationID: invocation.ID, Type: workspace.OperationPatchFile, Path: "ignored.go", Status: workspace.OperationRejected},
		}, nil
	})
	coordinator.SetWorktreeBaselineResolver(func(context.Context, string, string) (WorktreeBaseline, error) {
		return WorktreeBaseline{RepositoryType: "git", HeadRevision: "head-1", Branch: "main", StatusDigest: "sha256:current", ChangedPaths: []PathStatus{{Path: "README.md", Status: "MM"}, {Path: "formatter.go", Status: "??"}, {Path: "z.go", Status: " M"}}, StatusKnown: true, CapturedAt: now.Add(time.Minute)}, nil
	})
	report, err := coordinator.GetCompletionReport(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.AgentChanges == nil || report.AgentChanges.Status != AgentChangeSetPartial {
		t.Fatalf("命令存在时变更归因必须为 partial: %#v", report.AgentChanges)
	}
	if len(report.AgentChanges.Paths) != 2 || report.AgentChanges.Paths[0].Path != "README.md" || report.AgentChanges.Paths[1].Path != "z.go" {
		t.Fatalf("变更路径应稳定排序并排除未完成操作: %#v", report.AgentChanges.Paths)
	}
	if report.AgentChanges.Paths[0].BaselineStatus != " M" || report.AgentChanges.Paths[0].CurrentStatus != "MM" {
		t.Fatalf("已有修改重叠/终态状态未投影: %#v", report.AgentChanges.Paths[0])
	}
	if len(report.AgentChanges.OverlapPaths) != 1 || report.AgentChanges.OverlapPaths[0] != "README.md" {
		t.Fatalf("重叠路径应仅包含 admission 时已有文件: %#v", report.AgentChanges.OverlapPaths)
	}
	if len(report.AgentChanges.UnattributedPaths) != 1 || report.AgentChanges.UnattributedPaths[0] != "formatter.go" {
		t.Fatalf("终态新增且未被 operation 记录的路径应单独提示: %#v", report.AgentChanges.UnattributedPaths)
	}
	if len(report.AgentChanges.OperationIDs) != 1 || report.AgentChanges.OperationIDs[0] != "operation-patch" {
		t.Fatalf("变更 operation 引用不正确: %#v", report.AgentChanges.OperationIDs)
	}
	if !containsUnknownCode(report.UnknownStates, "agent_changes_partial") {
		t.Fatalf("partial 归因必须显式进入 unknown_states: %#v", report.UnknownStates)
	}
}

func TestCompletionReportMarksAgentChangesUnknownWhenBaselineIncomplete(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-agent-changes-unknown", UserID: "user", ConversationID: "conversation", SessionID: "conversation", WorkspaceID: "workspace", Status: InvocationCompleted, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	baseline := WorktreeBaseline{InvocationID: invocation.ID, WorkspaceID: invocation.WorkspaceID, RepositoryType: "git", StatusDigest: "sha256:baseline", ChangedPaths: []PathStatus{{Path: "a.go", Status: "??"}}, StatusKnown: false, Truncated: true, CapturedAt: now}
	if _, err := repo.SaveWorkspaceBaseline(context.Background(), baseline); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	coordinator.SetWorkspaceOperationResolver(func(context.Context, string, string) ([]workspace.Operation, error) {
		return []workspace.Operation{{ID: "operation-a", InvocationID: invocation.ID, Type: workspace.OperationWriteFile, Path: "a.go", Status: workspace.OperationCompleted}}, nil
	})
	report, err := coordinator.GetCompletionReport(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.AgentChanges == nil || report.AgentChanges.Status != AgentChangeSetUnknown {
		t.Fatalf("不完整 baseline 不得声称归因已知: %#v", report.AgentChanges)
	}
	if !containsUnknownCode(report.UnknownStates, "baseline_status_unknown") || !containsUnknownCode(report.UnknownStates, "agent_changes_unknown") {
		t.Fatalf("不完整 baseline 必须保留两个边界提示: %#v", report.UnknownStates)
	}
}

func TestCompletionReportDoesNotIgnoreApprovalStoreFailure(t *testing.T) {
	memoryRepo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-completion-approval-error", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationCompleted, CreatedAt: now, UpdatedAt: now}
	if err := memoryRepo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: &failingApprovalRepository{Repository: memoryRepo}, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	report, err := coordinator.GetCompletionReport(context.Background(), invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != WorkflowResultBlocked || !containsUnknownCode(report.UnknownStates, "approval_unavailable") {
		t.Fatalf("审批存储失败不得被静默视为无待处理审批: %#v", report)
	}
}

func containsUnknownCode(items []UnknownState, code string) bool {
	for _, item := range items {
		if item.Code == code {
			return true
		}
	}
	return false
}

type failingApprovalRepository struct {
	Repository
}

func (r *failingApprovalRepository) ListApprovals(context.Context, string, ApprovalStatus) ([]Approval, error) {
	return nil, errors.New("approval store unavailable")
}

func TestInstructionSnapshotSetReplacementAndApprovalRevalidation(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-instructions", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationWaitingApproval, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	first, err := repo.ReplaceInstructionSnapshotSet(context.Background(), InstructionSnapshotSet{
		InvocationID: invocation.ID, TargetPath: "src/main.go",
		Snapshots: []InstructionSnapshot{{Path: "AGENTS.md", ScopePath: ".", Source: "agents", ContentDigest: "digest-1", LoadedAt: now}},
	})
	if err != nil || first.Revision != 1 {
		t.Fatalf("初次保存指令快照=%#v err=%v", first, err)
	}
	second, err := repo.ReplaceInstructionSnapshotSet(context.Background(), InstructionSnapshotSet{InvocationID: invocation.ID, TargetPath: "src/main.go", Snapshots: []InstructionSnapshot{}})
	if err != nil || second.Revision != 2 || len(second.Snapshots) != 0 {
		t.Fatalf("替换空指令快照=%#v err=%v", second, err)
	}
	if _, err := repo.ReplaceInstructionSnapshotSet(context.Background(), InstructionSnapshotSet{InvocationID: invocation.ID, Revision: 1}); !errors.Is(err, ErrConflict) {
		t.Fatalf("旧快照 revision 应冲突，实际=%v", err)
	}
	if err := repo.CreateApproval(context.Background(), Approval{ID: "approval-instructions", InvocationID: invocation.ID, ToolName: "guarded", OriginalCallID: "call", ConfirmationCallID: "confirm", Status: ApprovalPending, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	coordinator.SetInstructionSnapshotValidator(func(context.Context, string) error { return errors.New("digest changed") })
	if _, err := coordinator.ResolveApproval(context.Background(), "approval-instructions", true, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("指令变化应阻止审批恢复，实际=%v", err)
	}
	approval, err := repo.GetApproval(context.Background(), "approval-instructions")
	if err != nil || approval.Status != ApprovalPending {
		t.Fatalf("被阻止的审批不应提前 resolved: %#v err=%v", approval, err)
	}
}

func TestRecordInstructionConflictEmitsDigestOnlyEvent(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-instruction-conflict", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationWaitingApproval, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReplaceInstructionSnapshotSet(context.Background(), InstructionSnapshotSet{
		InvocationID: invocation.ID,
		Snapshots:    []InstructionSnapshot{{Path: "AGENTS.md", ScopePath: ".", Source: "agents", ContentDigest: "old", Content: "secret old instruction", Priority: 1, LoadedAt: now}},
	}); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	if err := coordinator.RecordInstructionConflict(context.Background(), invocation.ID,
		[]InstructionSnapshot{{Path: "AGENTS.md", ScopePath: ".", Source: "agents", ContentDigest: "old", Content: "secret old instruction", Priority: 1}},
		[]InstructionSnapshot{{Path: "AGENTS.md", ScopePath: ".", Source: "agents", ContentDigest: "new", Content: "secret new instruction", Priority: 1}}, "changed"); err != nil {
		t.Fatal(err)
	}
	events, err := repo.ListEvents(context.Background(), invocation.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != EventInstructionConflict {
		t.Fatalf("instruction conflict event=%#v", events)
	}
	encoded, err := json.Marshal(events[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret old") || strings.Contains(string(encoded), "secret new") {
		t.Fatalf("冲突事件不应包含指令正文: %s", encoded)
	}
	if !strings.Contains(string(encoded), "old") || !strings.Contains(string(encoded), "new") {
		t.Fatalf("冲突事件应保留 digest: %s", encoded)
	}
}

func TestCoordinatorExpiresApprovalWithoutExecutingSideEffect(t *testing.T) {
	repo := NewMemoryRepository()
	created := time.Now().UTC().Add(-time.Minute)
	if err := repo.CreateInvocation(context.Background(), Invocation{
		ID: "inv-expire", UserID: "user", ConversationID: "conversation", SessionID: "conversation",
		Status: InvocationWaitingApproval, CreatedAt: created, UpdatedAt: created,
	}); err != nil {
		t.Fatal(err)
	}
	deadline := created.Add(10 * time.Second)
	if err := repo.CreateApproval(context.Background(), Approval{
		ID: "approval-expire", InvocationID: "inv-expire", ToolName: "guarded", OriginalCallID: "call-1",
		ConfirmationCallID: "confirm-1", Status: ApprovalPending, ExpiresAt: &deadline, CreatedAt: created, UpdatedAt: created,
	}); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	var mirrored bool
	coordinator.SetApprovalExpiryHandler(func(context.Context, Approval) error {
		mirrored = true
		return nil
	})
	if err := coordinator.ExpireApprovals(context.Background(), created.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	approval, err := repo.GetApproval(context.Background(), "approval-expire")
	if err != nil || approval.Status != ApprovalExpired {
		t.Fatalf("过期审批状态=%s err=%v", approval.Status, err)
	}
	invocation, err := repo.GetInvocation(context.Background(), "inv-expire")
	if err != nil || invocation.Status != InvocationExpired {
		t.Fatalf("过期 invocation 状态=%s err=%v", invocation.Status, err)
	}
	if !mirrored {
		t.Fatal("过期审批没有调用 sidecar 状态同步")
	}
	events, err := repo.ListEvents(context.Background(), "inv-expire", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var expired, terminal int
	for _, event := range events {
		if event.Type == EventApprovalExpired {
			expired++
		}
		if event.Type == EventInvocationExpired {
			terminal++
		}
	}
	if expired != 1 || terminal != 1 {
		t.Fatalf("过期事件数量 approval=%d terminal=%d events=%#v", expired, terminal, events)
	}
	if err := coordinator.ExpireApprovals(context.Background(), created.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	events, _ = repo.ListEvents(context.Background(), "inv-expire", 0, 100)
	terminal = 0
	for _, event := range events {
		if event.Type == EventInvocationExpired {
			terminal++
		}
	}
	if terminal != 1 {
		t.Fatalf("重复过期不应追加终态事件: %#v", events)
	}
}

func TestCoordinatorCancelClosesPendingApprovalOnce(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	if err := repo.CreateInvocation(context.Background(), Invocation{
		ID: "inv-cancel", UserID: "user", ConversationID: "conversation", SessionID: "conversation",
		Status: InvocationWaitingApproval, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateApproval(context.Background(), Approval{
		ID: "approval-cancel", InvocationID: "inv-cancel", ToolName: "guarded", OriginalCallID: "call-1",
		ConfirmationCallID: "confirm-1", Status: ApprovalPending, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo, subscribers: make(map[string]map[chan AgentEvent]struct{})}
	if _, err := coordinator.CancelInvocation(context.Background(), "inv-cancel"); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.CancelInvocation(context.Background(), "inv-cancel"); err != nil {
		t.Fatal(err)
	}
	approval, err := repo.GetApproval(context.Background(), "approval-cancel")
	if err != nil || approval.Status != ApprovalCancelled {
		t.Fatalf("取消后 approval 状态=%s err=%v", approval.Status, err)
	}
	events, err := repo.ListEvents(context.Background(), "inv-cancel", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	terminal := 0
	for _, event := range events {
		if event.Type == EventInvocationCancelled {
			terminal++
		}
	}
	if terminal != 1 {
		t.Fatalf("重复取消不应追加多个终态事件: %#v", events)
	}
}

func TestMapSessionEventExtractsApprovalIDs(t *testing.T) {
	event := &session.Event{ID: "adk-1", InvocationID: "adk-inv", Author: "abot_assistant", Timestamp: time.Now().UTC(), LLMResponse: adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
		ID: "confirm-1", Name: toolconfirmation.FunctionCallName, Args: map[string]any{
			"toolConfirmation":     map[string]any{"hint": "确认修改", "payload": map[string]any{"operation_id": "op-1"}},
			"originalFunctionCall": map[string]any{"id": "tool-1", "name": "workspace_request_patch", "args": map[string]any{"path": "a.txt", "api_key": "secret-value"}},
		},
	}}}}}}
	events, approvals := MapSessionEvent("inv-1", event)
	if len(events) != 1 || events[0].Type != EventApprovalRequested {
		t.Fatalf("mapped events = %#v", events)
	}
	if len(approvals) != 1 || approvals[0].ConfirmationCallID != "confirm-1" || approvals[0].OriginalCallID != "tool-1" || approvals[0].ToolName != "workspace_request_patch" || approvals[0].OperationID != "op-1" {
		t.Fatalf("approval data = %#v", approvals)
	}
	if got, _ := events[0].Data["operation_id"].(string); got != "op-1" {
		t.Fatalf("approval event operation_id = %q", got)
	}
	args, _ := events[0].Data["args"].(map[string]any)
	if args["api_key"] != "[REDACTED]" || strings.Contains(fmt.Sprint(events[0].Data["raw"]), "secret-value") {
		t.Fatalf("审批事件不应保存敏感参数: %#v", events[0].Data)
	}
}

func TestMapSessionEventRedactsToolResponseSecrets(t *testing.T) {
	event := &session.Event{ID: "adk-sensitive", InvocationID: "adk-inv", Author: "abot_assistant", Timestamp: time.Now().UTC(), LLMResponse: adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
		ID: "call-sensitive", Name: "workspace_read", Args: map[string]any{"path": "a.txt", "password": "do-not-store", "nested": map[string]any{"access_token": "also-secret"}},
	}}}}}}
	events, _ := MapSessionEvent("inv-sensitive", event)
	if len(events) < 1 || events[0].Type != EventToolRequested {
		t.Fatalf("工具请求事件缺失: %#v", events)
	}
	encoded, _ := json.Marshal(events)
	if strings.Contains(string(encoded), "do-not-store") || strings.Contains(string(encoded), "also-secret") {
		t.Fatalf("工具事件泄露敏感值: %s", encoded)
	}
	args, _ := events[0].Data["args"].(map[string]any)
	if args["password"] != "[REDACTED]" {
		t.Fatalf("敏感字段应保留键但隐藏值: %#v", args)
	}
	if got := sanitizeRuntimeString("Authorization: Bearer abc.def\nAPI_KEY=plain-secret"); strings.Contains(got, "abc.def") || strings.Contains(got, "plain-secret") {
		t.Fatalf("日志中的常见密钥格式未脱敏: %q", got)
	}
}

func TestMapSessionEventPromotesUsagePlanAndArtifacts(t *testing.T) {
	event := &session.Event{
		ID: "adk-usage", InvocationID: "adk-inv", Author: "abot_assistant", Timestamp: time.Now().UTC(),
		LLMResponse: adkmodel.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 12, CandidatesTokenCount: 7}},
		Actions:     session.EventActions{ArtifactDelta: map[string]int64{"report.txt": 2}},
		Output:      map[string]any{"plan": []any{"verify"}},
	}
	events, _ := MapSessionEvent("inv-1", event)
	types := make(map[string]bool, len(events))
	for _, item := range events {
		types[item.Type] = true
	}
	for _, eventType := range []string{EventUsageUpdated, EventArtifactUpdated, EventPlanUpdated} {
		if !types[eventType] {
			t.Fatalf("缺少 %s 事件: %#v", eventType, events)
		}
	}
	if usage, ok := events[0].Data["usage"]; !ok || usage == nil {
		t.Fatalf("usage 事件未保留 usage metadata: %#v", events[0])
	}
}

func TestMapSessionEventCarriesModelCallCorrelationForUsage(t *testing.T) {
	event := &session.Event{
		ID: "adk-usage-correlation", InvocationID: "adk-inv", Author: "abot_assistant", Timestamp: time.Now().UTC(),
		LLMResponse: adkmodel.LLMResponse{
			UsageMetadata:  &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 8},
			CustomMetadata: map[string]any{"abot_model_call_id": "invocation:model:2"},
		},
	}
	events, _ := MapSessionEvent("inv-1", event)
	if len(events) != 1 || events[0].Type != EventUsageUpdated || events[0].Data["model_call_id"] != "invocation:model:2" {
		t.Fatalf("usage 事件未保留安全的 model-call correlation: %#v", events)
	}
}

func TestMapSessionEventCarriesModelRouteForRuntimeEvidence(t *testing.T) {
	event := &session.Event{
		ID: "adk-route", InvocationID: "adk-inv", Author: "abot_assistant", Timestamp: time.Now().UTC(),
		CustomMetadata: map[string]any{"openai_wire_format": "responses"},
		Content:        genai.NewContentFromText("ok", genai.RoleModel),
	}
	events, _ := MapSessionEvent("inv-route", event)
	if len(events) != 1 || events[0].Data["model_route"] != "responses" {
		t.Fatalf("事件未保留受限 model route: %#v", events)
	}
	evidence := runtimeCapabilityEvidenceFromEvent(events[0])
	if len(evidence) != 1 || evidence[0].Route != "responses" {
		t.Fatalf("运行证据未继承 model route: %#v", evidence)
	}
	// Unknown route metadata is ignored rather than persisted as an arbitrary
	// partition key supplied by an upstream response.
	unknown := &session.Event{ID: "adk-unknown-route", CustomMetadata: map[string]any{"route": "attacker"}, Content: genai.NewContentFromText("ok", genai.RoleModel)}
	unknownEvents, _ := MapSessionEvent("inv-route", unknown)
	if len(unknownEvents) != 1 {
		t.Fatalf("未知 route 事件数量不正确: %#v", unknownEvents)
	}
	if _, exists := unknownEvents[0].Data["model_route"]; exists {
		t.Fatalf("未知 route 不应进入事件 metadata: %#v", unknownEvents[0].Data)
	}
}

func TestCoordinatorRecordManifestUsageCorrelatesAndDoesNotGuessKnownSamples(t *testing.T) {
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-usage-correlation", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationRunning, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	firstActual := 11
	if err := repo.SaveContextManifest(context.Background(), ContextManifest{ID: "manifest-1", InvocationID: invocation.ID, ModelCallID: "call-1", Model: "model", EstimatedInput: 10, ActualInput: &firstActual, Digest: "digest-1"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveContextManifest(context.Background(), ContextManifest{ID: "manifest-2", InvocationID: invocation.ID, ModelCallID: "call-2", Model: "model", EstimatedInput: 20, Digest: "digest-2"}); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo}
	coordinator.recordManifestUsage(context.Background(), invocation.ID, map[string]any{"model_call_id": "call-2", "usage": map[string]any{"promptTokenCount": 22}})
	first, err := repo.GetContextManifest(context.Background(), "call-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.GetContextManifest(context.Background(), "call-2")
	if err != nil {
		t.Fatal(err)
	}
	if first.ActualInput == nil || *first.ActualInput != firstActual || second.ActualInput == nil || *second.ActualInput != 22 {
		t.Fatalf("usage 应按 model-call correlation 回填: first=%#v second=%#v", first.ActualInput, second.ActualInput)
	}
	// A legacy event without a correlation marker must select only the newest
	// manifest lacking an observation; it must never overwrite the known first
	// sample merely because it is the newest event in the stream.
	coordinator.recordManifestUsage(context.Background(), invocation.ID, map[string]any{"usage": map[string]any{"promptTokenCount": 99}})
	first, _ = repo.GetContextManifest(context.Background(), "call-1")
	second, _ = repo.GetContextManifest(context.Background(), "call-2")
	if *first.ActualInput != firstActual || *second.ActualInput != 22 {
		t.Fatalf("缺少 correlation 时不应覆盖已知 usage: first=%v second=%v", *first.ActualInput, *second.ActualInput)
	}
	coordinator.recordManifestUsage(context.Background(), invocation.ID, map[string]any{"scope": "modal_fallback", "usage": map[string]any{"promptTokenCount": 88}})
	first, _ = repo.GetContextManifest(context.Background(), "call-1")
	second, _ = repo.GetContextManifest(context.Background(), "call-2")
	if *first.ActualInput != firstActual || *second.ActualInput != 22 {
		t.Fatalf("模态降级 usage 不应回填主模型上下文 manifest: first=%v second=%v", *first.ActualInput, *second.ActualInput)
	}
}

func TestRuntimeCapabilityEvidenceClassifiesStableErrorsOnly(t *testing.T) {
	stable := runtimeCapabilityEvidenceFromError(errors.New(`HTTP 400: field response_format is unsupported; api_key=secret-value`), agent.ChatRequest{})
	if len(stable) != 1 || stable[0].Feature != "structured_output" || stable[0].State != provider.SupportUnsupported {
		t.Fatalf("稳定 structured-output 拒绝分类不正确: %#v", stable)
	}
	if strings.Contains(stable[0].Reason, "secret-value") {
		t.Fatalf("运行证据 reason 泄露密钥: %#v", stable[0])
	}
	transient := runtimeCapabilityEvidenceFromError(errors.New("HTTP 429: temporarily unavailable"), agent.ChatRequest{Stream: true})
	if len(transient) != 0 {
		t.Fatalf("限流瞬时错误不应形成 unsupported 证据: %#v", transient)
	}
	stream := runtimeCapabilityEvidenceFromError(errors.New("HTTP 400: stream field unsupported"), agent.ChatRequest{Stream: true})
	if len(stream) != 1 || stream[0].Feature != "streaming" {
		t.Fatalf("streaming 拒绝分类不正确: %#v", stream)
	}
	schema := runtimeCapabilityEvidenceFromError(errors.New("HTTP 400: responseJsonSchema is not supported"), agent.ChatRequest{})
	if len(schema) != 1 || schema[0].Feature != "structured_output_schema" || schema[0].State != provider.SupportUnsupported {
		t.Fatalf("schema 子集拒绝不应降级整个 structured output: %#v", schema)
	}
	parallel := runtimeCapabilityEvidenceFromError(errors.New("HTTP 400: parallel_tool_calls is unsupported"), agent.ChatRequest{})
	if len(parallel) != 1 || parallel[0].Feature != "parallel_tool_calls" {
		t.Fatalf("parallel tools 拒绝分类不正确: %#v", parallel)
	}
	image := runtimeCapabilityEvidenceFromError(errors.New("HTTP 400: input_image is not supported"), agent.ChatRequest{})
	if len(image) != 1 || image[0].Feature != "images" {
		t.Fatalf("图片输入拒绝分类不正确: %#v", image)
	}
	file := runtimeCapabilityEvidenceFromError(errors.New("HTTP 400: input_file is not supported"), agent.ChatRequest{})
	if len(file) != 1 || file[0].Feature != "input_files" {
		t.Fatalf("文件输入拒绝分类不正确: %#v", file)
	}
}

func TestMapSessionEventPromotesStructuredToolOutput(t *testing.T) {
	event := &session.Event{
		ID: "adk-tool-output", InvocationID: "adk-inv", Author: "workspace", Timestamp: time.Now().UTC(),
		Output: map[string]any{"call_id": "tool-1", "name": "workspace_request_command", "stdout": "ok\n"},
	}
	events, _ := MapSessionEvent("inv-1", event)
	var output AgentEvent
	for _, item := range events {
		if item.Type == EventToolOutput {
			output = item
			break
		}
	}
	if output.Type != EventToolOutput {
		t.Fatalf("缺少 tool.output 事件: %#v", events)
	}
	if output.Data["call_id"] != "tool-1" || output.Data["name"] != "workspace_request_command" {
		t.Fatalf("tool.output 未保留调用关联: %#v", output.Data)
	}
}

func TestCoordinatorApprovalResumesOriginalTool(t *testing.T) {
	repo := &runtimeProviderRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", BaseURL: "http://127.0.0.1:1", Protocol: provider.ProtocolOpenAICompatible,
			Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true}}},
	}}
	model := &runtimeApprovalModel{}
	registry, err := provider.NewRegistry(context.Background(), repo, map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: &runtimeApprovalAdapter{model: model},
	})
	if err != nil {
		t.Fatal(err)
	}
	var executed atomic.Int32
	guarded, err := functiontool.New(functiontool.Config{
		Name: "guarded_write", Description: "test guarded write", RequireConfirmation: true,
	}, func(ctx adkagent.Context, args runtimeApprovalArgs) (runtimeApprovalResult, error) {
		if confirmation := ctx.ToolConfirmation(); confirmation == nil || !confirmation.Confirmed {
			executed.Store(-1)
			return runtimeApprovalResult{}, nil
		}
		executed.Add(1)
		return runtimeApprovalResult{Status: "applied", Path: args.Path}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{
		AppName: "runtime-approval-test", SessionService: session.InMemoryService(), Providers: registry,
		Tools: []tool.Tool{guarded}, Instruction: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := NewMemoryRepository()
	coordinator, err := NewCoordinator(kernel, repository)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := coordinator.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	invocation, err := coordinator.StartInvocation(ctx, agent.ChatRequest{
		UserID: "user", ConversationID: "conversation", SessionID: "conversation",
		ProviderID: "demo", ModelID: "model", Message: "write", Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, live, unsubscribe, err := coordinator.Subscribe(ctx, invocation.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	var approval Approval
	deadline := time.After(3 * time.Second)
	for approval.ID == "" {
		select {
		case event := <-live:
			if event.Type == EventApprovalRequested {
				approvalID, _ := event.Data["approval_id"].(string)
				approval, err = coordinator.GetApproval(ctx, approvalID)
				if err != nil {
					t.Fatal(err)
				}
			}
		case <-deadline:
			t.Fatal("等待原生审批超时")
		}
	}
	if approval.OriginalCallID != "original-call" || approval.ConfirmationCallID == "" {
		t.Fatalf("审批没有保留双 call ID: %#v", approval)
	}
	if _, err := coordinator.ResolveApproval(ctx, approval.ID, true, "test approve"); err != nil {
		t.Fatal(err)
	}
	deadline = time.After(3 * time.Second)
	for {
		item, getErr := coordinator.GetInvocation(ctx, invocation.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if item.Status.Terminal() {
			if item.Status != InvocationCompleted {
				t.Fatalf("批准恢复后的 invocation 状态 = %s, error=%s", item.Status, item.Error)
			}
			break
		}
		select {
		case <-deadline:
			events, _ := repository.ListEvents(ctx, invocation.ID, 0, 100)
			t.Fatalf("批准恢复未完成，当前状态 %s，events=%#v，model_calls=%d", item.Status, events, model.calls())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if executed.Load() != 1 || model.calls() != 2 {
		t.Fatalf("批准恢复执行次数=%d，模型调用次数=%d，期望各为 1/2", executed.Load(), model.calls())
	}
	manifests, err := repository.ListContextManifests(ctx, invocation.ID)
	if err != nil || len(manifests) < 2 {
		t.Fatalf("每次模型调用都应保存 Context Manifest，manifests=%#v err=%v", manifests, err)
	}
	for _, manifest := range manifests {
		if manifest.InvocationID != invocation.ID || manifest.Digest == "" || manifest.EstimatedInput <= 0 {
			t.Fatalf("Context Manifest 元数据不完整: %#v", manifest)
		}
	}
}

func TestCoordinatorRecoversQueuedApprovalHandoff(t *testing.T) {
	model := &runtimeApprovalModel{}
	registry, err := provider.NewRegistry(context.Background(), &runtimeProviderRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true}}},
	}}, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: &runtimeApprovalAdapter{model: model}})
	if err != nil {
		t.Fatal(err)
	}
	var executed atomic.Int32
	guarded, err := functiontool.New(functiontool.Config{
		Name: "guarded_write", Description: "test guarded write", RequireConfirmation: true,
	}, func(ctx adkagent.Context, args runtimeApprovalArgs) (runtimeApprovalResult, error) {
		if confirmation := ctx.ToolConfirmation(); confirmation == nil || !confirmation.Confirmed {
			executed.Store(-1)
			return runtimeApprovalResult{}, nil
		}
		executed.Add(1)
		return runtimeApprovalResult{Status: "applied", Path: args.Path}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{
		AppName: "runtime-approval-handoff-test", SessionService: session.InMemoryService(), Providers: registry,
		Tools: []tool.Tool{guarded}, Instruction: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewMemoryRepository()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start(ctx); err != nil {
		t.Fatal(err)
	}
	invocation, err := first.StartInvocation(ctx, agent.ChatRequest{
		UserID: "user", ConversationID: "approval-handoff-conversation", SessionID: "approval-handoff-conversation",
		ProviderID: "demo", ModelID: "model", Message: "write", Stream: true,
	})
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	approvalID := ""
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && approvalID == "" {
		items, listErr := repo.ListApprovals(ctx, invocation.ID, ApprovalPending)
		if listErr != nil {
			first.Close()
			t.Fatal(listErr)
		}
		if len(items) > 0 {
			approvalID = items[0].ID
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if approvalID == "" {
		first.Close()
		t.Fatal("等待审批 handoff 测试超时")
	}
	// CreateApproval happens before the invocation CAS inside the serialized
	// approval boundary. Wait for the whole boundary to become durable before
	// simulating a coordinator crash; observing the repository directly here
	// bypasses the approval lock used by the public resolution path.
	boundaryDurable := false
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := repo.GetInvocation(ctx, invocation.ID)
		if getErr != nil {
			first.Close()
			t.Fatal(getErr)
		}
		if current.Status == InvocationWaitingApproval && current.ActiveApprovalID == approvalID {
			boundaryDurable = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !boundaryDurable {
		first.Close()
		t.Fatal("审批边界未完整持久化")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	// Resolve after the original coordinator has closed. The decision is
	// durable, but launch is intentionally a no-op; the next Coordinator must
	// consume the private handoff instead of replaying the original message.
	if _, err := first.ResolveApproval(ctx, approvalID, true, "crash-window approve"); err != nil {
		t.Fatal(err)
	}
	queued, err := repo.GetInvocation(ctx, invocation.ID)
	if err != nil || queued.Status != InvocationQueued {
		t.Fatalf("审批恢复应留下 queued invocation: %#v err=%v", queued, err)
	}
	handoffRepo, ok := interface{}(repo).(InvocationResumeRepository)
	if !ok {
		t.Fatal("Memory repository 未实现 InvocationResumeRepository")
	}
	handoff, err := handoffRepo.GetInvocationResume(ctx, invocation.ID)
	if err != nil || handoff.WaitID == "" || handoff.RequestDigest == "" {
		t.Fatalf("审批恢复 handoff 未持久化: %#v err=%v", handoff, err)
	}

	second, err := NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := second.Start(ctx); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := second.GetInvocation(ctx, invocation.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if current.Status.Terminal() {
			if current.Status != InvocationCompleted {
				t.Fatalf("审批 handoff 恢复终态错误: %#v", current)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, err := second.GetInvocation(ctx, invocation.ID)
	if err != nil || current.Status != InvocationCompleted {
		t.Fatalf("审批 handoff 恢复未完成: %#v err=%v", current, err)
	}
	if executed.Load() != 1 || model.calls() != 2 {
		t.Fatalf("审批 handoff 不应重放原始消息: executed=%d model_calls=%d", executed.Load(), model.calls())
	}
	if _, err := handoffRepo.GetInvocationResume(ctx, invocation.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Runner 接管后应清理审批 handoff: %v", err)
	}
	outboxRepo, ok := any(repo).(InvocationResumeOutboxRepository)
	if !ok {
		t.Fatal("MemoryRepository 未实现 InvocationResumeOutboxRepository")
	}
	outbox, err := outboxRepo.GetInvocationResumeOutbox(ctx, invocation.ID)
	if err != nil || outbox.Status != InvocationResumeOutboxCompleted || outbox.Attempt != 1 {
		t.Fatalf("审批恢复应完成一次 durable outbox 投递: item=%#v err=%v", outbox, err)
	}
}

func TestCoordinatorRejectsApprovalWithoutExecutingTool(t *testing.T) {
	model := &runtimeApprovalModel{}
	registry, err := provider.NewRegistry(context.Background(), &runtimeProviderRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true}}},
	}}, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: &runtimeApprovalAdapter{model: model}})
	if err != nil {
		t.Fatal(err)
	}
	var executed atomic.Int32
	guarded, err := functiontool.New(functiontool.Config{
		Name: "guarded_write", Description: "test guarded write", RequireConfirmation: true,
	}, func(_ adkagent.Context, args runtimeApprovalArgs) (runtimeApprovalResult, error) {
		executed.Add(1)
		return runtimeApprovalResult{Status: "applied", Path: args.Path}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{
		AppName: "runtime-reject-test", SessionService: session.InMemoryService(), Providers: registry,
		Tools: []tool.Tool{guarded}, Instruction: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewMemoryRepository()
	coordinator, err := NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := coordinator.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	invocation, err := coordinator.StartInvocation(ctx, agent.ChatRequest{
		UserID: "user", ConversationID: "reject-conversation", SessionID: "reject-conversation",
		ProviderID: "demo", ModelID: "model", Message: "write", Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var approval Approval
	deadline := time.Now().Add(3 * time.Second)
	for approval.ID == "" && time.Now().Before(deadline) {
		items, listErr := coordinator.ListApprovals(ctx, invocation.ID, ApprovalPending)
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(items) > 0 {
			approval = items[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if approval.ID == "" {
		t.Fatal("等待拒绝测试审批超时")
	}
	if _, err := coordinator.ResolveApproval(ctx, approval.ID, false, "用户拒绝"); err != nil {
		t.Fatal(err)
	}
	for time.Now().Before(deadline) {
		item, getErr := coordinator.GetInvocation(ctx, invocation.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if item.Status.Terminal() {
			if item.Status != InvocationCompleted {
				t.Fatalf("拒绝后的 invocation 状态=%s error=%s", item.Status, item.Error)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	item, err := coordinator.GetInvocation(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !item.Status.Terminal() {
		t.Fatalf("拒绝恢复未终止，状态=%s", item.Status)
	}
	resolved, err := coordinator.GetApproval(ctx, approval.ID)
	if err != nil || resolved.Status != ApprovalRejected {
		t.Fatalf("拒绝后的 approval 状态=%s err=%v", resolved.Status, err)
	}
	if executed.Load() != 0 {
		t.Fatalf("拒绝后工具不应执行，执行次数=%d", executed.Load())
	}
}

type atomicRejectionTestRepository struct {
	*MemoryRepository
	calls atomic.Int32
}

func (r *atomicRejectionTestRepository) CommitApprovalRejection(ctx context.Context, commit ApprovalResumeCommit) (Approval, Invocation, []AgentEvent, error) {
	r.calls.Add(1)
	return r.MemoryRepository.CommitApprovalResume(ctx, commit)
}

func TestCoordinatorUsesAtomicRejectionCommitWithoutLegacySidecarCallback(t *testing.T) {
	model := &runtimeApprovalModel{}
	registry, err := provider.NewRegistry(context.Background(), &runtimeProviderRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true}}},
	}}, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: &runtimeApprovalAdapter{model: model}})
	if err != nil {
		t.Fatal(err)
	}
	var executed atomic.Int32
	guarded, err := functiontool.New(functiontool.Config{
		Name: "guarded_write", Description: "test guarded write", RequireConfirmation: true,
	}, func(_ adkagent.Context, args runtimeApprovalArgs) (runtimeApprovalResult, error) {
		executed.Add(1)
		return runtimeApprovalResult{Status: "applied", Path: args.Path}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{
		AppName: "runtime-atomic-reject-test", SessionService: session.InMemoryService(), Providers: registry,
		Instruction: "test", Tools: []tool.Tool{guarded},
	})
	if err != nil {
		t.Fatal(err)
	}
	repo := &atomicRejectionTestRepository{MemoryRepository: NewMemoryRepository()}
	coordinator, err := NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	var legacyHandlerCalls atomic.Int32
	coordinator.SetApprovalDecisionHandler(func(context.Context, Approval, bool) error {
		legacyHandlerCalls.Add(1)
		return errors.New("legacy sidecar callback should not run")
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := coordinator.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	invocation, err := coordinator.StartInvocation(ctx, agent.ChatRequest{
		UserID: "user", ConversationID: "atomic-reject-conversation", SessionID: "atomic-reject-conversation",
		ProviderID: "demo", ModelID: "model", Message: "write", Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var approval Approval
	deadline := time.Now().Add(3 * time.Second)
	for approval.ID == "" && time.Now().Before(deadline) {
		items, listErr := coordinator.ListApprovals(ctx, invocation.ID, ApprovalPending)
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(items) > 0 {
			approval = items[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if approval.ID == "" {
		t.Fatal("等待原子拒绝测试审批超时")
	}
	if _, err := coordinator.ResolveApproval(ctx, approval.ID, false, "用户拒绝"); err != nil {
		t.Fatal(err)
	}
	if repo.calls.Load() != 1 {
		t.Fatalf("应调用一次原子拒绝提交，实际=%d", repo.calls.Load())
	}
	if legacyHandlerCalls.Load() != 0 {
		t.Fatalf("原子拒绝路径不应调用旧 sidecar callback，实际=%d", legacyHandlerCalls.Load())
	}
	if executed.Load() != 0 {
		t.Fatalf("原子拒绝后工具不应执行，执行次数=%d", executed.Load())
	}
}

func TestCoordinatorWorkspaceApprovalPreparesOperationBeforeResume(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workspaceRepo := &runtimeWorkspaceRepository{items: map[string]workspace.Workspace{
		"workspace-1": {ID: "workspace-1", Name: "test", Type: workspace.TypeLocal, RootPath: root, Enabled: true, Status: workspace.StatusReady},
	}, operations: make(map[string]workspace.Operation)}
	workspaceService, err := workspace.NewService(workspaceRepo)
	if err != nil {
		t.Fatal(err)
	}
	model := &runtimeWorkspaceModel{}
	registry, err := provider.NewRegistry(context.Background(), &runtimeProviderRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true}}},
	}}, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: &runtimeApprovalAdapter{model: model}})
	if err != nil {
		t.Fatal(err)
	}
	workspaceTools, err := workspaceService.ToolsWithPolicy(context.Background(), "workspace-1", "conversation-1", workspace.ToolPolicy{Enabled: true, ReadEnabled: true, WriteEnabled: true, ExecEnabled: true, GitEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{
		AppName: "runtime-workspace-test", SessionService: session.InMemoryService(), Providers: registry,
		Instruction: "test", WorkspaceTools: func(context.Context, string) ([]tool.Tool, error) { return workspaceTools, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	runtimeRepo := NewMemoryRepository()
	coordinator, err := NewCoordinator(kernel, runtimeRepo)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.SetApprovalDecisionHandler(func(ctx context.Context, approval Approval, approved bool) error {
		if approved || approval.OperationID == "" {
			return nil
		}
		_, err := workspaceService.RejectOperation(ctx, approval.OperationID)
		return err
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := coordinator.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	workspaceID := "workspace-1"
	invocation, err := coordinator.StartInvocation(ctx, agent.ChatRequest{
		UserID: "user", ConversationID: "conversation-1", SessionID: "conversation-1", ProviderID: "demo", ModelID: "model",
		WorkspaceID: &workspaceID, Message: "请修改文件", Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	backlog, live, unsubscribe, err := coordinator.Subscribe(ctx, invocation.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	var approval Approval
	consumeApproval := func(event AgentEvent) {
		if event.Type != EventApprovalRequested || approval.ID != "" {
			return
		}
		approvalID, _ := event.Data["approval_id"].(string)
		approval, _ = coordinator.GetApproval(ctx, approvalID)
	}
	for _, event := range backlog {
		consumeApproval(event)
	}
	deadline := time.After(3 * time.Second)
	for approval.ID == "" {
		select {
		case event, open := <-live:
			if !open {
				t.Fatal("审批事件流提前关闭")
			}
			consumeApproval(event)
		case <-deadline:
			events, _ := runtimeRepo.ListEvents(ctx, invocation.ID, 0, 100)
			item, _ := coordinator.GetInvocation(ctx, invocation.ID)
			t.Fatalf("等待工作区审批超时，状态=%s events=%#v model_calls=%d", item.Status, events, model.calls())
		}
	}
	if approval.OperationID == "" {
		t.Fatalf("Approval 未关联已准备 Operation: %#v", approval)
	}
	operation, err := workspaceRepo.GetOperation(ctx, approval.OperationID)
	if err != nil {
		t.Fatalf("读取准备操作失败: %v", err)
	}
	if (operation.Status != workspace.OperationPending && operation.Status != workspace.OperationPrepared) || operation.Diff == "" {
		t.Fatalf("审批前操作未处于可审阅 prepared/diff 状态: %#v", operation)
	}
	if _, err := coordinator.ResolveApproval(ctx, approval.ID, true, "test approve"); err != nil {
		t.Fatalf("批准失败: %v", err)
	}
	deadline = time.After(3 * time.Second)
	for {
		item, getErr := coordinator.GetInvocation(ctx, invocation.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if item.Status.Terminal() {
			if item.Status != InvocationCompleted {
				t.Fatalf("工作区审批恢复后的状态=%s error=%s", item.Status, item.Error)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("工作区审批恢复超时，状态=%s", item.Status)
		case <-time.After(10 * time.Millisecond):
		}
	}
	content, err := os.ReadFile(filepath.Join(root, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "after\n" {
		item, _ := coordinator.GetInvocation(ctx, invocation.ID)
		events, _ := runtimeRepo.ListEvents(ctx, invocation.ID, 0, 100)
		op, _ := workspaceRepo.GetOperation(ctx, approval.OperationID)
		t.Fatalf("批准后文件内容=%q status=%s error=%s op=%#v events=%#v model_calls=%d", content, item.Status, item.Error, op, events, model.calls())
	}
	operation, err = workspaceRepo.GetOperation(ctx, approval.OperationID)
	if err != nil || operation.Status != workspace.OperationCompleted {
		t.Fatalf("批准后 Operation 状态=%s err=%v", operation.Status, err)
	}
	toolCalls, err := runtimeRepo.ListToolCalls(ctx, invocation.ID)
	if err != nil || len(toolCalls) != 1 {
		t.Fatalf("ToolCall 审计记录数量=%d err=%v", len(toolCalls), err)
	}
	if approval.ToolCallID == "" || toolCalls[0].ID != approval.ToolCallID || toolCalls[0].Status != ToolCallCompleted {
		t.Fatalf("ToolCall/Approval 关联或终态不正确: approval=%#v tool_calls=%#v", approval, toolCalls)
	}
}

func TestCoordinatorDefaultWorkflowRunsInspectEditVerifyWithAdmissionGate(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workspaceRepo := &runtimeWorkspaceRepository{items: map[string]workspace.Workspace{
		"workflow-workspace": {ID: "workflow-workspace", Name: "workflow", Type: workspace.TypeLocal, RootPath: root, Enabled: true, Status: workspace.StatusReady},
	}, operations: make(map[string]workspace.Operation)}
	workspaceService, err := workspace.NewService(workspaceRepo)
	if err != nil {
		t.Fatal(err)
	}
	model := &runtimeFullWorkflowModel{}
	registry, err := provider.NewRegistry(context.Background(), &runtimeProviderRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true}}},
	}}, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: &runtimeApprovalAdapter{model: model}})
	if err != nil {
		t.Fatal(err)
	}
	workspaceTools, err := workspaceService.ToolsWithPolicy(context.Background(), "workflow-workspace", "workflow-conversation", workspace.ToolPolicy{Enabled: true, ReadEnabled: true, WriteEnabled: true, ExecEnabled: true, GitEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{
		AppName: "runtime-full-workflow-test", SessionService: session.InMemoryService(), Providers: registry,
		Instruction: "test", WorkspaceTools: func(context.Context, string) ([]tool.Tool, error) { return workspaceTools, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	runtimeRepo := NewMemoryRepository()
	coordinator, err := NewCoordinator(kernel, runtimeRepo)
	if err != nil {
		t.Fatal(err)
	}
	workspaceService.SetOperationAdmissionValidator(coordinator.ValidateWorkspaceOperationAdmission)
	workspaceService.SetOperationObserver(func(observerCtx context.Context, operation workspace.Operation) {
		coordinator.RecordWorkspaceOperation(observerCtx, operation)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := coordinator.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	workspaceID := "workflow-workspace"
	invocation, err := coordinator.StartInvocation(ctx, agent.ChatRequest{
		UserID: "user", ConversationID: "workflow-conversation", SessionID: "workflow-conversation", ProviderID: "demo", ModelID: "model",
		WorkspaceID: &workspaceID, Message: "请修改文件并运行验证", Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitApproval := func() Approval {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			items, listErr := coordinator.ListApprovals(ctx, invocation.ID, ApprovalPending)
			if listErr != nil {
				t.Fatal(listErr)
			}
			if len(items) > 0 {
				return items[0]
			}
			time.Sleep(10 * time.Millisecond)
		}
		item, _ := coordinator.GetInvocation(ctx, invocation.ID)
		events, _ := runtimeRepo.ListEvents(ctx, invocation.ID, 0, 200)
		t.Fatalf("等待 workflow 审批超时: status=%s events=%#v model_calls=%d", item.Status, events, model.calls())
		return Approval{}
	}
	first := waitApproval()
	if first.ToolName != "workspace_request_patch" {
		t.Fatalf("首个审批应为 editing patch: %#v", first)
	}
	if _, err := coordinator.ResolveApproval(ctx, first.ID, true, "approve patch"); err != nil {
		t.Fatal(err)
	}
	second := waitApproval()
	if second.ToolName != "workspace_request_command" {
		t.Fatalf("第二个审批应为 verifying command: %#v", second)
	}
	if _, err := coordinator.ResolveApproval(ctx, second.ID, true, "approve verify"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		item, getErr := coordinator.GetInvocation(ctx, invocation.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if item.Status.Terminal() {
			if item.Status != InvocationCompleted {
				t.Fatalf("完整 workflow 终态=%s error=%s", item.Status, item.Error)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	item, err := coordinator.GetInvocation(ctx, invocation.ID)
	if err != nil || item.Status != InvocationCompleted {
		t.Fatalf("完整 workflow 未完成: item=%#v err=%v", item, err)
	}
	plan, err := coordinator.GetTaskPlan(ctx, invocation.ID)
	if err != nil || plan.Status != PlanCompleted || len(plan.Steps) != 3 {
		t.Fatalf("inspect→edit→verify 计划未完成: plan=%#v err=%v", plan, err)
	}
	for _, step := range plan.Steps {
		if step.Status != PlanStepCompleted || len(step.Evidence) == 0 {
			t.Fatalf("计划步骤缺少完成证据: %#v", plan.Steps)
		}
	}
	verifications, err := coordinator.ListVerificationRuns(ctx, invocation.ID)
	if err != nil || len(verifications) != 1 || verifications[0].Status != VerificationPassed {
		t.Fatalf("验证证据未闭环: verifications=%#v err=%v", verifications, err)
	}
	content, err := os.ReadFile(filepath.Join(root, "notes.txt"))
	if err != nil || string(content) != "after\n" {
		t.Fatalf("批准后的文件内容不正确: %q err=%v", content, err)
	}
	operations, err := workspaceService.ListOperations(ctx, workspaceID)
	if err != nil || len(operations) != 2 {
		t.Fatalf("工作流应仅持久化一次 patch 和一次 command: operations=%#v err=%v", operations, err)
	}
	events, err := runtimeRepo.ListEvents(ctx, invocation.ID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	var rejectedEarlyWrite bool
	for _, event := range events {
		if event.Type == EventToolFailed && strings.Contains(fmt.Sprint(event.Data["error"]), "工作流操作门禁拒绝") {
			rejectedEarlyWrite = true
			break
		}
	}
	if !rejectedEarlyWrite {
		t.Fatalf("首个越过 inspect 的写入应被 workflow admission 拒绝: events=%#v", events)
	}
}

func TestCoordinatorResumesWaitingToolAcrossCoordinatorRestart(t *testing.T) {
	model := &runtimeWaitingToolModel{}
	longTool, err := functiontool.New(functiontool.Config{
		Name:          "wait_for_external_result",
		Description:   "等待外部系统返回结果",
		IsLongRunning: true,
	}, func(_ adkagent.Context, _ struct{}) (map[string]string, error) {
		return map[string]string{"status": "pending"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := provider.NewRegistry(context.Background(), &runtimeProviderRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true}}},
	}}, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: &runtimeApprovalAdapter{model: model}})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{
		AppName: "runtime-waiting-tool-test", SessionService: session.InMemoryService(), Providers: registry,
		Tools: []tool.Tool{longTool}, Instruction: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewMemoryRepository()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start(ctx); err != nil {
		t.Fatal(err)
	}
	invocation, err := first.StartInvocation(ctx, agent.ChatRequest{
		UserID: "user", ConversationID: "waiting-tool-conversation", SessionID: "waiting-tool-conversation",
		ProviderID: "demo", ModelID: "model", Message: "等待外部结果", Stream: true,
	})
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	waitID := ""
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := first.GetInvocation(ctx, invocation.ID)
		if getErr != nil {
			first.Close()
			t.Fatal(getErr)
		}
		if current.Status == InvocationWaitingTool {
			events, listErr := repo.ListEvents(ctx, invocation.ID, 0, 200)
			if listErr != nil {
				first.Close()
				t.Fatal(listErr)
			}
			for _, event := range events {
				if event.Type != EventInvocationWaiting || event.Data == nil {
					continue
				}
				if reason, _ := event.Data["reason"].(string); reason != "tool" {
					continue
				}
				ids := resumeStringList(event.Data["tool_call_ids"])
				if len(ids) > 0 {
					waitID = ids[0]
				}
			}
			if waitID != "" {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if waitID == "" {
		current, _ := first.GetInvocation(ctx, invocation.ID)
		events, _ := repo.ListEvents(ctx, invocation.ID, 0, 200)
		first.Close()
		t.Fatalf("等待长运行工具超时: status=%s events=%#v model_calls=%d", current.Status, events, model.calls())
	}
	if model.calls() != 2 {
		t.Fatalf("暂停前应把 pending 工具结果反馈给模型一次: calls=%d", model.calls())
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := second.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if current, err := second.GetInvocation(ctx, invocation.ID); err != nil || current.Status != InvocationWaitingTool {
		t.Fatalf("重启后等待状态应保持: invocation=%#v err=%v", current, err)
	}
	if _, err := second.ResumeInvocation(ctx, invocation.ID, InvocationResumeRequest{WaitID: "unknown-wait-id", Name: "wait_for_external_result", Response: map[string]any{"output": "approved"}, IdempotencyKey: "resume-1"}); !errors.Is(err, ErrResumeMismatch) {
		t.Fatalf("未知 wait_id 应被拒绝: %v", err)
	}
	if _, err := second.ResumeInvocation(ctx, invocation.ID, InvocationResumeRequest{WaitID: waitID, Name: "other_tool", Response: map[string]any{"output": "approved"}, IdempotencyKey: "resume-name-mismatch"}); !errors.Is(err, ErrResumeMismatch) {
		t.Fatalf("工具名冲突应被拒绝: %v", err)
	}
	if _, err := second.ResumeInvocation(ctx, invocation.ID, InvocationResumeRequest{WaitID: waitID, Name: "wait_for_external_result", Response: map[string]any{"blob": strings.Repeat("x", maxInvocationResumeResponseBytes)}, IdempotencyKey: "resume-too-large"}); !errors.Is(err, ErrInvalidResume) {
		t.Fatalf("超大结构化响应应被拒绝: %v", err)
	}
	resumed, err := second.ResumeInvocation(ctx, invocation.ID, InvocationResumeRequest{WaitID: waitID, Name: "wait_for_external_result", Response: map[string]any{"output": "approved"}, IdempotencyKey: "resume-1"})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != InvocationQueued {
		t.Fatalf("恢复应先返回 queued: %#v", resumed)
	}
	// The same request is idempotent even while the resumed worker is starting.
	if duplicate, err := second.ResumeInvocation(ctx, invocation.ID, InvocationResumeRequest{WaitID: waitID, Name: "wait_for_external_result", Response: map[string]any{"output": "approved"}, IdempotencyKey: "resume-1"}); err != nil || duplicate.ID != invocation.ID {
		t.Fatalf("重复恢复请求应幂等: invocation=%#v err=%v", duplicate, err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := second.GetInvocation(ctx, invocation.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if current.Status.Terminal() {
			if current.Status != InvocationCompleted {
				events, _ := repo.ListEvents(ctx, invocation.ID, 0, 300)
				t.Fatalf("恢复后 invocation 终态错误: %#v events=%#v", current, events)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, err := second.GetInvocation(ctx, invocation.ID)
	if err != nil || current.Status != InvocationCompleted {
		t.Fatalf("长运行工具恢复未完成: invocation=%#v err=%v", current, err)
	}
	if model.calls() != 3 {
		t.Fatalf("重复恢复不应再次调用模型: calls=%d", model.calls())
	}
	events, err := repo.ListEvents(ctx, invocation.ID, 0, 300)
	if err != nil {
		t.Fatal(err)
	}
	resumedEvents := 0
	for _, event := range events {
		if event.Type == EventInvocationResumed && event.Data != nil && event.Data["reason"] == "tool" {
			resumedEvents++
			if event.Data["wait_id"] != waitID || event.Data["request_digest"] == "" {
				t.Fatalf("恢复事件缺少稳定关联信息: %#v", event)
			}
		}
	}
	if resumedEvents != 1 {
		t.Fatalf("重复恢复只应产生一个恢复事件: %d events=%#v", resumedEvents, events)
	}
	outboxRepo, ok := any(repo).(InvocationResumeOutboxRepository)
	if !ok {
		t.Fatal("MemoryRepository 未实现 InvocationResumeOutboxRepository")
	}
	outbox, err := outboxRepo.GetInvocationResumeOutbox(ctx, invocation.ID)
	if err != nil || outbox.Status != InvocationResumeOutboxCompleted || outbox.Attempt != 1 {
		t.Fatalf("真实 ResumeInvocation 应完成一次 durable outbox 投递: item=%#v err=%v", outbox, err)
	}
}

func TestCoordinatorRecoversQueuedDurableResumeHandoff(t *testing.T) {
	model := &runtimeWaitingToolModel{}
	longTool, err := functiontool.New(functiontool.Config{
		Name: "wait_for_external_result", Description: "等待外部系统返回结果", IsLongRunning: true,
	}, func(_ adkagent.Context, _ struct{}) (map[string]string, error) {
		return map[string]string{"status": "pending"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := provider.NewRegistry(context.Background(), &runtimeProviderRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true}}},
	}}, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: &runtimeApprovalAdapter{model: model}})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{
		AppName: "runtime-resume-handoff-test", SessionService: session.InMemoryService(), Providers: registry,
		Tools: []tool.Tool{longTool}, Instruction: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewMemoryRepository()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start(ctx); err != nil {
		t.Fatal(err)
	}
	invocation, err := first.StartInvocation(ctx, agent.ChatRequest{
		UserID: "user", ConversationID: "resume-handoff-conversation", SessionID: "resume-handoff-conversation",
		ProviderID: "demo", ModelID: "model", Message: "等待外部结果", Stream: true,
	})
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	waitID := ""
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := first.GetInvocation(ctx, invocation.ID)
		if getErr != nil {
			first.Close()
			t.Fatal(getErr)
		}
		if current.Status == InvocationWaitingTool {
			events, listErr := repo.ListEvents(ctx, invocation.ID, 0, 200)
			if listErr != nil {
				first.Close()
				t.Fatal(listErr)
			}
			for _, event := range events {
				if event.Type != EventInvocationWaiting || event.Data == nil || event.Data["reason"] != "tool" {
					continue
				}
				ids := resumeStringList(event.Data["tool_call_ids"])
				if len(ids) > 0 {
					waitID = ids[0]
				}
			}
			if waitID != "" {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if waitID == "" {
		current, _ := first.GetInvocation(ctx, invocation.ID)
		events, _ := repo.ListEvents(ctx, invocation.ID, 0, 200)
		first.Close()
		t.Fatalf("等待长运行工具超时: status=%s events=%#v", current.Status, events)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	resumeRequest := InvocationResumeRequest{WaitID: waitID, Name: "wait_for_external_result", Response: map[string]any{"output": "ready"}, IdempotencyKey: "handoff-1"}
	fingerprint, encoded, err := invocationResumeFingerprint(resumeRequest)
	if err != nil {
		t.Fatal(err)
	}
	handoffRepo, ok := any(repo).(InvocationResumeRepository)
	if !ok {
		t.Fatal("MemoryRepository 未实现 InvocationResumeRepository")
	}
	if _, err := handoffRepo.SaveInvocationResume(ctx, InvocationResume{ID: "resume-handoff", InvocationID: invocation.ID, WaitID: waitID, Name: resumeRequest.Name, ResponseJSON: encoded, RequestDigest: fingerprint}); err != nil {
		t.Fatal(err)
	}
	if ok, err := repo.TransitionInvocation(ctx, invocation.ID, InvocationWaitingTool, InvocationQueued, ""); err != nil || !ok {
		t.Fatalf("模拟恢复 handoff 入队失败: ok=%v err=%v", ok, err)
	}
	if _, err := repo.AppendEvent(ctx, AgentEvent{ID: "resume-handoff-event", InvocationID: invocation.ID, Type: EventInvocationResumed, Timestamp: time.Now().UTC(), Data: map[string]any{"reason": "tool", "wait_id": waitID, "tool_name": resumeRequest.Name, "request_digest": fingerprint}}); err != nil {
		t.Fatal(err)
	}
	second, err := NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := second.Start(ctx); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := second.GetInvocation(ctx, invocation.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if current.Status.Terminal() {
			if current.Status != InvocationCompleted {
				t.Fatalf("durable resume handoff 终态错误: %#v", current)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, err := second.GetInvocation(ctx, invocation.ID)
	if err != nil || current.Status != InvocationCompleted {
		t.Fatalf("重启后 queued handoff 未继续: invocation=%#v err=%v calls=%d", current, err, model.calls())
	}
	if model.calls() != 3 {
		t.Fatalf("重启恢复应只补一次模型调用: calls=%d", model.calls())
	}
	if _, err := handoffRepo.GetInvocationResume(ctx, invocation.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("worker 获得 running lease 后应清理 handoff: %v", err)
	}
}

func TestInvocationResumeBatchIsBoundedAndRehydratesMultipleResponses(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-batch-resume", UserID: "user", ConversationID: "batch-conversation", SessionID: "batch-conversation", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	for _, event := range []AgentEvent{
		{ID: "batch-tool-a", InvocationID: invocation.ID, Type: EventToolRequested, Timestamp: now, Data: map[string]any{"call_id": "wait-a", "name": "wait_for_alpha"}},
		{ID: "batch-tool-b", InvocationID: invocation.ID, Type: EventToolRequested, Timestamp: now.Add(time.Millisecond), Data: map[string]any{"call_id": "wait-b", "name": "wait_for_beta"}},
		{ID: "batch-waiting", InvocationID: invocation.ID, Type: EventInvocationWaiting, Timestamp: now.Add(2 * time.Millisecond), Data: map[string]any{"reason": "tool", "tool_call_ids": []string{"wait-a", "wait-b"}}},
	} {
		if _, err := repo.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}

	request := InvocationResumeRequest{
		Responses: []InvocationResumeItem{
			{WaitID: " wait-a ", Response: map[string]any{"status": "ready", "value": 1}},
			{WaitID: "wait-b", Name: "wait_for_beta", Response: map[string]any{"status": "ready", "value": 2}},
		},
		IdempotencyKey: "batch-1",
	}
	items, err := normalizeInvocationResumeItems(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].WaitID != "wait-a" || items[1].WaitID != "wait-b" {
		t.Fatalf("批量 wait ID 应规范化且保持稳定顺序: %#v", items)
	}
	if items[0].Name != "" {
		t.Fatalf("未提供的工具名不应凭空补全: %#v", items[0])
	}
	digest, encoded, err := invocationResumeFingerprint(request)
	if err != nil || digest == "" || len(encoded) == 0 {
		t.Fatalf("批量恢复应生成有界摘要和载荷: digest=%q bytes=%d err=%v", digest, len(encoded), err)
	}
	if strings.HasPrefix(strings.TrimSpace(string(encoded)), "{") {
		t.Fatalf("批量恢复私有载荷应为数组: %s", encoded)
	}
	var fingerprintItems []InvocationResumeItem
	if err := json.Unmarshal(encoded, &fingerprintItems); err != nil || len(fingerprintItems) != 2 || fingerprintItems[0].WaitID != "wait-a" || fingerprintItems[1].WaitID != "wait-b" {
		t.Fatalf("批量恢复摘要载荷结构错误: %#v err=%v", fingerprintItems, err)
	}

	// Resolve names only from the durable waiting boundary, then build the exact
	// multi-part FunctionResponse that a resumed ADK Session receives.
	coordinator := &Coordinator{repo: repo}
	toolNames, err := coordinator.waitingToolBoundary(ctx, invocation.ID)
	if err != nil || toolNames["wait-a"] != "wait_for_alpha" || toolNames["wait-b"] != "wait_for_beta" {
		t.Fatalf("等待边界工具名未从事件恢复: %#v err=%v", toolNames, err)
	}
	resolved := []InvocationResumeItem{
		{WaitID: "wait-a", Name: toolNames["wait-a"], Response: items[0].Response},
		{WaitID: "wait-b", Name: toolNames["wait-b"], Response: items[1].Response},
	}
	encoded, err = json.Marshal(resolved)
	if err != nil {
		t.Fatal(err)
	}
	content, err := invocationResumeContent(InvocationResume{WaitID: resolved[0].WaitID, Name: resolved[0].Name, ResponseJSON: encoded})
	if err != nil {
		t.Fatal(err)
	}
	if content.Role != genai.RoleUser || len(content.Parts) != 2 {
		t.Fatalf("批量恢复应生成两个 user FunctionResponse part: %#v", content)
	}
	for index, want := range resolved {
		part := content.Parts[index]
		if part == nil || part.FunctionResponse == nil || part.FunctionResponse.ID != want.WaitID || part.FunctionResponse.Name != want.Name {
			t.Fatalf("FunctionResponse[%d] 关联错误: %#v", index, part)
		}
		if got, _ := part.FunctionResponse.Response["value"].(float64); got != float64(index+1) {
			t.Fatalf("FunctionResponse[%d] payload 错误: %#v", index, part.FunctionResponse.Response)
		}
	}
	eventData := resumeEventData(resolved, digest)
	if eventData["wait_id"] != "wait-a" || eventData["request_digest"] != digest {
		t.Fatalf("批量恢复事件应保留首项兼容字段: %#v", eventData)
	}
	if got := resumeStringList(eventData["wait_ids"]); len(got) != 2 || got[0] != "wait-a" || got[1] != "wait-b" {
		t.Fatalf("批量恢复事件应记录完整 wait_ids: %#v", eventData)
	}
	if got := resumeStringList(eventData["tool_names"]); len(got) != 2 || got[0] != "wait_for_alpha" || got[1] != "wait_for_beta" {
		t.Fatalf("批量恢复事件应记录完整工具名: %#v", eventData)
	}

	if _, err := repo.AppendEvent(ctx, AgentEvent{ID: "batch-resumed-a", InvocationID: invocation.ID, Type: EventInvocationResumed, Timestamp: now.Add(3 * time.Millisecond), Data: map[string]any{"reason": "tool", "wait_id": "wait-a", "wait_ids": []string{"wait-a"}}}); err != nil {
		t.Fatal(err)
	}
	remaining, err := coordinator.waitingToolBoundary(ctx, invocation.ID)
	if err != nil || len(remaining) != 1 || remaining["wait-b"] != "wait_for_beta" {
		t.Fatalf("部分恢复后只应剩余 wait-b: %#v err=%v", remaining, err)
	}

	if _, err := normalizeInvocationResumeItems(InvocationResumeRequest{WaitID: "wait-a", Response: map[string]any{}, Responses: request.Responses}); !errors.Is(err, ErrInvalidResume) {
		t.Fatalf("单个 response 与 responses 混用应拒绝: %v", err)
	}
	if _, err := normalizeInvocationResumeItems(InvocationResumeRequest{Responses: []InvocationResumeItem{{WaitID: "wait-a", Response: map[string]any{}}, {WaitID: "wait-a", Response: map[string]any{}}}}); !errors.Is(err, ErrInvalidResume) {
		t.Fatalf("批量重复 wait ID 应拒绝: %v", err)
	}
	overLimit := make([]InvocationResumeItem, maxInvocationResumeItems+1)
	for index := range overLimit {
		overLimit[index] = InvocationResumeItem{WaitID: fmt.Sprintf("wait-%d", index), Response: map[string]any{}}
	}
	if _, err := normalizeInvocationResumeItems(InvocationResumeRequest{Responses: overLimit}); !errors.Is(err, ErrInvalidResume) {
		t.Fatalf("批量 wait ID 数量超限应拒绝: %v", err)
	}
}

func TestCoordinatorResumesMultipleWaitingToolsAtomically(t *testing.T) {
	model := &runtimeBatchWaitingToolModel{}
	alpha, err := functiontool.New(functiontool.Config{
		Name: "wait_for_alpha", Description: "等待 alpha 外部结果", IsLongRunning: true,
	}, func(_ adkagent.Context, _ struct{}) (map[string]string, error) {
		return map[string]string{"status": "pending", "tool": "alpha"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	beta, err := functiontool.New(functiontool.Config{
		Name: "wait_for_beta", Description: "等待 beta 外部结果", IsLongRunning: true,
	}, func(_ adkagent.Context, _ struct{}) (map[string]string, error) {
		return map[string]string{"status": "pending", "tool": "beta"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := provider.NewRegistry(context.Background(), &runtimeProviderRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true}}},
	}}, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: &runtimeApprovalAdapter{model: model}})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{
		AppName: "runtime-batch-waiting-tool-test", SessionService: session.InMemoryService(), Providers: registry,
		Tools: []tool.Tool{alpha, beta}, Instruction: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewMemoryRepository()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator, err := NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	if err := coordinator.Start(ctx); err != nil {
		t.Fatal(err)
	}
	invocation, err := coordinator.StartInvocation(ctx, agent.ChatRequest{
		UserID: "user", ConversationID: "batch-waiting-conversation", SessionID: "batch-waiting-conversation",
		ProviderID: "demo", ModelID: "model", Message: "同时等待两个系统", Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitIDs := []string{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := coordinator.GetInvocation(ctx, invocation.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if current.Status == InvocationWaitingTool {
			events, listErr := repo.ListEvents(ctx, invocation.ID, 0, 200)
			if listErr != nil {
				t.Fatal(listErr)
			}
			for _, event := range events {
				if event.Type == EventInvocationWaiting && event.Data != nil && event.Data["reason"] == "tool" {
					waitIDs = resumeStringList(event.Data["tool_call_ids"])
				}
			}
			if len(waitIDs) == 2 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(waitIDs) != 2 {
		current, _ := coordinator.GetInvocation(ctx, invocation.ID)
		events, _ := repo.ListEvents(ctx, invocation.ID, 0, 300)
		t.Fatalf("两个长运行工具应形成同一 waiting 边界: status=%s ids=%v events=%#v model_calls=%d", current.Status, waitIDs, events, model.calls())
	}
	if model.calls() != 2 {
		t.Fatalf("批量等待在恢复前应只完成两次模型调用: %d", model.calls())
	}
	toolNames, err := coordinator.waitingToolBoundary(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	responses := make([]InvocationResumeItem, 0, len(waitIDs))
	for index, waitID := range waitIDs {
		name := toolNames[waitID]
		item := InvocationResumeItem{WaitID: waitID, Response: map[string]any{"status": "ready", "source": name}}
		if index > 0 {
			item.Name = name
		}
		responses = append(responses, item)
	}
	resumed, err := coordinator.ResumeInvocation(ctx, invocation.ID, InvocationResumeRequest{Responses: responses, IdempotencyKey: "batch-resume-1"})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != InvocationQueued {
		t.Fatalf("批量恢复应先进入 queued: %#v", resumed)
	}
	if _, err := coordinator.ResumeInvocation(ctx, invocation.ID, InvocationResumeRequest{Responses: responses, IdempotencyKey: "batch-resume-1"}); err != nil {
		t.Fatalf("相同批量恢复应幂等: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := coordinator.GetInvocation(ctx, invocation.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if current.Status.Terminal() {
			if current.Status != InvocationCompleted {
				events, _ := repo.ListEvents(ctx, invocation.ID, 0, 300)
				t.Fatalf("批量恢复终态错误: %#v events=%#v", current, events)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, err := coordinator.GetInvocation(ctx, invocation.ID)
	if err != nil || current.Status != InvocationCompleted {
		t.Fatalf("两个长运行工具恢复后未完成: invocation=%#v err=%v calls=%d", current, err, model.calls())
	}
	if model.calls() != 3 {
		t.Fatalf("批量恢复不应重复调用模型: %d", model.calls())
	}
	events, err := repo.ListEvents(ctx, invocation.ID, 0, 300)
	if err != nil {
		t.Fatal(err)
	}
	resumedEvents := 0
	for _, event := range events {
		if event.Type != EventInvocationResumed || event.Data == nil || event.Data["reason"] != "tool" {
			continue
		}
		resumedEvents++
		if got := resumeStringList(event.Data["wait_ids"]); len(got) != 2 || got[0] != waitIDs[0] || got[1] != waitIDs[1] {
			t.Fatalf("批量恢复事件应保留完整 wait_ids: %#v", event)
		}
	}
	if resumedEvents != 1 {
		t.Fatalf("批量重复恢复只应产生一个恢复事件: %d events=%#v", resumedEvents, events)
	}
}

func TestCoordinatorResumesSequentialWaitingToolBoundaries(t *testing.T) {
	model := &runtimeSequentialWaitingToolModel{}
	longTool, err := functiontool.New(functiontool.Config{
		Name: "wait_for_external_result", Description: "等待外部结果", IsLongRunning: true,
	}, func(_ adkagent.Context, _ struct{}) (map[string]string, error) {
		return map[string]string{"status": "pending"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := provider.NewRegistry(context.Background(), &runtimeProviderRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true}}},
	}}, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: &runtimeApprovalAdapter{model: model}})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{
		AppName: "runtime-sequential-waiting-tool-test", SessionService: session.InMemoryService(), Providers: registry,
		Tools: []tool.Tool{longTool}, Instruction: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewMemoryRepository()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator, err := NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	if err := coordinator.Start(ctx); err != nil {
		t.Fatal(err)
	}
	invocation, err := coordinator.StartInvocation(ctx, agent.ChatRequest{
		UserID: "user", ConversationID: "sequential-waiting-conversation", SessionID: "sequential-waiting-conversation",
		ProviderID: "demo", ModelID: "model", Message: "先后等待两个结果", Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	latestWaitingIDs := func() []string {
		events, _ := repo.ListEvents(ctx, invocation.ID, 0, 500)
		ids := []string(nil)
		for _, event := range events {
			if event.Type == EventInvocationWaiting && event.Data != nil && event.Data["reason"] == "tool" {
				ids = resumeStringList(event.Data["tool_call_ids"])
			}
		}
		return ids
	}
	waitFirst := ""
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := coordinator.GetInvocation(ctx, invocation.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if current.Status == InvocationWaitingTool {
			ids := latestWaitingIDs()
			if len(ids) == 1 {
				waitFirst = ids[0]
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if waitFirst == "" || model.calls() != 2 {
		t.Fatalf("第一个等待边界未稳定: wait_id=%q calls=%d", waitFirst, model.calls())
	}
	if _, err := coordinator.ResumeInvocation(ctx, invocation.ID, InvocationResumeRequest{WaitID: waitFirst, Response: map[string]any{"value": "first"}, IdempotencyKey: "sequential-resume-1"}); err != nil {
		t.Fatal(err)
	}

	waitSecond := ""
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := coordinator.GetInvocation(ctx, invocation.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if current.Status == InvocationWaitingTool {
			ids := latestWaitingIDs()
			if len(ids) == 1 && ids[0] != waitFirst {
				waitSecond = ids[0]
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if waitSecond == "" {
		current, _ := coordinator.GetInvocation(ctx, invocation.ID)
		events, _ := repo.ListEvents(ctx, invocation.ID, 0, 500)
		t.Fatalf("第一个恢复后应进入第二个等待边界: status=%s events=%#v calls=%d", current.Status, events, model.calls())
	}
	if _, err := coordinator.ResumeInvocation(ctx, invocation.ID, InvocationResumeRequest{WaitID: waitSecond, Response: map[string]any{"value": "second"}, IdempotencyKey: "sequential-resume-2"}); err != nil {
		t.Fatal(err)
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := coordinator.GetInvocation(ctx, invocation.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if current.Status.Terminal() {
			if current.Status != InvocationCompleted {
				events, _ := repo.ListEvents(ctx, invocation.ID, 0, 500)
				t.Fatalf("多边界恢复终态错误: %#v events=%#v", current, events)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, err := coordinator.GetInvocation(ctx, invocation.ID)
	if err != nil || current.Status != InvocationCompleted {
		t.Fatalf("第二个等待边界恢复后未完成: invocation=%#v err=%v calls=%d", current, err, model.calls())
	}
	if model.calls() != 5 {
		t.Fatalf("两个连续等待边界应各恢复一次且不重放原消息: calls=%d", model.calls())
	}
	events, err := repo.ListEvents(ctx, invocation.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	resumed := make(map[string]bool)
	for _, event := range events {
		if event.Type == EventInvocationResumed && event.Data != nil && event.Data["reason"] == "tool" {
			if waitID, _ := event.Data["wait_id"].(string); waitID != "" {
				resumed[waitID] = true
			}
		}
	}
	if len(resumed) != 2 || !resumed[waitFirst] || !resumed[waitSecond] {
		t.Fatalf("两个等待边界应各有一个恢复事件: resumed=%#v events=%#v", resumed, events)
	}
}

type runtimeWaitingToolModel struct {
	mu    sync.Mutex
	count int
}

type runtimeBatchWaitingToolModel struct {
	mu    sync.Mutex
	count int
}

func (m *runtimeBatchWaitingToolModel) Name() string { return "runtime-batch-waiting-tool-model" }

func (m *runtimeBatchWaitingToolModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count
}

func (m *runtimeBatchWaitingToolModel) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.mu.Lock()
		m.count++
		count := m.count
		m.mu.Unlock()
		if count == 1 {
			yield(&adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
				{FunctionCall: &genai.FunctionCall{ID: "runtime-batch-call-a", Name: "wait_for_alpha", Args: map[string]any{}}},
				{FunctionCall: &genai.FunctionCall{ID: "runtime-batch-call-b", Name: "wait_for_beta", Args: map[string]any{}}},
			}}}, nil)
			return
		}
		if count == 2 {
			yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText("等待两个外部结果", genai.RoleModel)}, nil)
			return
		}
		yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText("两个外部结果均已收到", genai.RoleModel)}, nil)
	}
}

type runtimeSequentialWaitingToolModel struct {
	mu    sync.Mutex
	count int
}

func (m *runtimeSequentialWaitingToolModel) Name() string {
	return "runtime-sequential-waiting-tool-model"
}

func (m *runtimeSequentialWaitingToolModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count
}

func (m *runtimeSequentialWaitingToolModel) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.mu.Lock()
		m.count++
		count := m.count
		m.mu.Unlock()
		switch count {
		case 1:
			yield(&adkmodel.LLMResponse{Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "runtime-sequential-call-1", Name: "wait_for_external_result", Args: map[string]any{}}}},
			}}, nil)
		case 2:
			yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText("等待第一个外部结果", genai.RoleModel)}, nil)
		case 3:
			yield(&adkmodel.LLMResponse{Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "runtime-sequential-call-2", Name: "wait_for_external_result", Args: map[string]any{}}}},
			}}, nil)
		case 4:
			yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText("等待第二个外部结果", genai.RoleModel)}, nil)
		default:
			yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText("两个外部结果均已收到", genai.RoleModel)}, nil)
		}
	}
}

func (m *runtimeWaitingToolModel) Name() string { return "runtime-waiting-tool-model" }

func (m *runtimeWaitingToolModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count
}

func (m *runtimeWaitingToolModel) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.mu.Lock()
		m.count++
		count := m.count
		m.mu.Unlock()
		if count == 1 {
			yield(&adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID: "runtime-wait-call", Name: "wait_for_external_result", Args: map[string]any{},
			}}}}}, nil)
			return
		}
		if count == 2 {
			yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText("等待外部结果", genai.RoleModel)}, nil)
			return
		}
		yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText("已收到外部结果", genai.RoleModel)}, nil)
	}
}

type runtimeWorkspaceModel struct {
	mu    sync.Mutex
	count int
}

type runtimeFullWorkflowModel struct {
	mu    sync.Mutex
	count int
}

func (m *runtimeFullWorkflowModel) Name() string { return "runtime-full-workflow-model" }

func (m *runtimeFullWorkflowModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count
}

func (m *runtimeFullWorkflowModel) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.mu.Lock()
		m.count++
		count := m.count
		m.mu.Unlock()
		var content *genai.Content
		switch count {
		case 1:
			content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID: "workflow-early-write", Name: "workspace_request_patch", Args: map[string]any{"path": "notes.txt", "old_text": "before", "new_text": "after"},
			}}}}
		case 2:
			content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID: "workflow-inspect", Name: "workspace_read_file", Args: map[string]any{"path": "notes.txt"},
			}}}}
		case 3:
			content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID: "workflow-write", Name: "workspace_request_patch", Args: map[string]any{"path": "notes.txt", "old_text": "before", "new_text": "after"},
			}}}}
		case 4:
			content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID: "workflow-command", Name: "workspace_request_command", Args: map[string]any{"command": "printf verify-ok", "cwd": ".", "timeout_seconds": 5},
			}}}}
		default:
			content = genai.NewContentFromText("已完成检查、修改和验证", genai.RoleModel)
		}
		yield(&adkmodel.LLMResponse{Content: content}, nil)
	}
}

func (m *runtimeWorkspaceModel) Name() string { return "runtime-workspace-model" }

func (m *runtimeWorkspaceModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count
}

func (m *runtimeWorkspaceModel) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.mu.Lock()
		m.count++
		count := m.count
		m.mu.Unlock()
		if count == 1 {
			yield(&adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID: "workspace-original-call", Name: "workspace_request_patch", Args: map[string]any{"path": "notes.txt", "old_text": "before", "new_text": "after"},
			}}}}}, nil)
			return
		}
		yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText("已完成", genai.RoleModel)}, nil)
	}
}

type runtimeWorkspaceRepository struct {
	mu         sync.Mutex
	items      map[string]workspace.Workspace
	operations map[string]workspace.Operation
}

func (r *runtimeWorkspaceRepository) List(context.Context) ([]workspace.Workspace, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]workspace.Workspace, 0, len(r.items))
	for _, item := range r.items {
		items = append(items, item)
	}
	return items, nil
}

func (r *runtimeWorkspaceRepository) Get(_ context.Context, id string) (workspace.Workspace, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.items[id]
	if !ok {
		return workspace.Workspace{}, workspace.ErrNotFound
	}
	return item, nil
}

func (r *runtimeWorkspaceRepository) Save(_ context.Context, item workspace.Workspace) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[item.ID] = item
	return nil
}

func (r *runtimeWorkspaceRepository) Delete(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.items, id)
	return nil
}

func (r *runtimeWorkspaceRepository) ListOperations(_ context.Context, workspaceID string) ([]workspace.Operation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]workspace.Operation, 0, len(r.operations))
	for _, item := range r.operations {
		if workspaceID == "" || item.WorkspaceID == workspaceID {
			items = append(items, item)
		}
	}
	return items, nil
}

func (r *runtimeWorkspaceRepository) GetOperation(_ context.Context, id string) (workspace.Operation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.operations[id]
	if !ok {
		return workspace.Operation{}, workspace.ErrNotFound
	}
	return item, nil
}

func (r *runtimeWorkspaceRepository) SaveOperation(_ context.Context, item workspace.Operation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.operations[item.ID] = item
	return nil
}

type runtimeApprovalArgs struct {
	Path string `json:"path"`
}

type runtimeApprovalResult struct {
	Status string `json:"status"`
	Path   string `json:"path"`
}

type runtimeApprovalModel struct {
	mu    sync.Mutex
	count int
}

func (m *runtimeApprovalModel) Name() string { return "runtime-approval-model" }

func (m *runtimeApprovalModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count
}

func (m *runtimeApprovalModel) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.mu.Lock()
		m.count++
		count := m.count
		m.mu.Unlock()
		if count == 1 {
			yield(&adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID: "original-call", Name: "guarded_write", Args: map[string]any{"path": "README.md"},
			}}}}}, nil)
			return
		}
		yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText("已完成", genai.RoleModel)}, nil)
	}
}

type runtimeApprovalAdapter struct{ model adkmodel.LLM }

func (a *runtimeApprovalAdapter) BuildModel(context.Context, provider.Provider, string) (adkmodel.LLM, error) {
	return a.model, nil
}
func (a *runtimeApprovalAdapter) TestConnection(context.Context, provider.Provider) (provider.ProbeResult, error) {
	return provider.ProbeResult{}, nil
}
func (a *runtimeApprovalAdapter) DiscoverModels(context.Context, provider.Provider) ([]provider.Model, error) {
	return nil, nil
}

type runtimeProviderRepository struct {
	providers map[string]provider.Provider
}

func (r *runtimeProviderRepository) List(context.Context) ([]provider.Provider, error) {
	items := make([]provider.Provider, 0, len(r.providers))
	for _, item := range r.providers {
		items = append(items, item)
	}
	return items, nil
}
func (r *runtimeProviderRepository) Get(_ context.Context, id string) (provider.Provider, error) {
	item, ok := r.providers[id]
	if !ok {
		return provider.Provider{}, provider.ErrNotFound
	}
	return item, nil
}
func (r *runtimeProviderRepository) Save(_ context.Context, item provider.Provider) error {
	r.providers[item.ID] = item
	return nil
}
func (r *runtimeProviderRepository) Delete(_ context.Context, id string) error {
	delete(r.providers, id)
	return nil
}
func (r *runtimeProviderRepository) GetDefault(context.Context) (provider.DefaultRef, error) {
	return provider.DefaultRef{}, nil
}
func (r *runtimeProviderRepository) SetDefault(context.Context, provider.DefaultRef) error {
	return nil
}
