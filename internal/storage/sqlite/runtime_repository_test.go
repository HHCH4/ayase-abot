package sqlite

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/provider"
)

func TestRuntimeRepositoryPersistsTaskPlanWithCAS(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo := store.RuntimeRepository()
	now := time.Now().UTC()
	invocation := agentruntime.Invocation{ID: "inv-plan-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	planRepo, ok := repo.(agentruntime.TaskPlanRepository)
	if !ok {
		t.Fatal("SQLite runtime repository 未实现 TaskPlanRepository")
	}
	plan := agentruntime.TaskPlan{ID: "plan-sqlite", InvocationID: invocation.ID, Revision: 1, Status: agentruntime.PlanPending, Steps: []agentruntime.PlanStep{}, CreatedAt: now, UpdatedAt: now}
	if err := planRepo.CreateTaskPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	updated := agentruntime.TaskPlan{ID: plan.ID, InvocationID: plan.InvocationID, Revision: 2, Status: agentruntime.PlanInProgress, CurrentStepID: "step-1", Steps: []agentruntime.PlanStep{{ID: "step-1", Title: "检查", Status: agentruntime.PlanStepInProgress}}, CreatedAt: now, UpdatedAt: now.Add(time.Second)}
	got, err := planRepo.UpdateTaskPlan(context.Background(), updated, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 2 || got.CurrentStepID != "step-1" {
		t.Fatalf("更新后的计划=%#v", got)
	}
	if _, err := planRepo.UpdateTaskPlan(context.Background(), updated, 1); err != agentruntime.ErrConflict {
		t.Fatalf("旧 revision 应返回 ErrConflict，实际=%v", err)
	}
	persisted, err := planRepo.GetTaskPlan(context.Background(), invocation.ID)
	if err != nil || persisted.Status != agentruntime.PlanInProgress {
		t.Fatalf("读取持久化计划=%#v err=%v", persisted, err)
	}
}

func TestRuntimeRepositoryPersistsImmutableWorkspaceBaseline(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	repo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-baseline-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "conversation", WorkspaceID: "workspace", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	baselineRepo, ok := repo.(agentruntime.WorktreeBaselineRepository)
	if !ok {
		t.Fatal("SQLite Runtime Repository 未实现 WorktreeBaselineRepository")
	}
	want := agentruntime.WorktreeBaseline{InvocationID: invocation.ID, WorkspaceID: "workspace", TargetPath: "internal", RepositoryType: "git", HeadRevision: "abc", Branch: "main", StatusDigest: "sha256:baseline", ChangedPaths: []agentruntime.PathStatus{{Path: "b.go", Status: " M"}, {Path: "a.go", Status: "??"}}, StatusKnown: true, Revision: 1, CapturedAt: now}
	saved, err := baselineRepo.SaveWorkspaceBaseline(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ChangedPaths[0].Path != "a.go" || saved.CapturedAt.IsZero() {
		t.Fatalf("SQLite baseline 未规范化保存: %#v", saved)
	}
	got, err := baselineRepo.GetWorkspaceBaseline(ctx, invocation.ID)
	if err != nil || got.StatusDigest != want.StatusDigest || len(got.ChangedPaths) != 2 || got.ChangedPaths[0].Path != "a.go" || got.StatusKnown != want.StatusKnown {
		t.Fatalf("SQLite baseline 读取不正确: %#v err=%v", got, err)
	}
	if _, err := baselineRepo.SaveWorkspaceBaseline(ctx, want); err != nil {
		t.Fatalf("相同 SQLite baseline 应幂等: %v", err)
	}
	want.StatusDigest = "sha256:changed"
	if _, err := baselineRepo.SaveWorkspaceBaseline(ctx, want); err != agentruntime.ErrConflict {
		t.Fatalf("不同 SQLite baseline 不应覆盖: %v", err)
	}
}

func TestRuntimeRepositoryWorkspaceBaselineConcurrentIdempotency(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	repo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-baseline-concurrent", UserID: "user", ConversationID: "conversation", SessionID: "conversation", WorkspaceID: "workspace", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	baselineRepo := repo.(agentruntime.WorktreeBaselineRepository)
	baseline := agentruntime.WorktreeBaseline{InvocationID: invocation.ID, WorkspaceID: invocation.WorkspaceID, RepositoryType: "git", StatusDigest: "sha256:concurrent", StatusKnown: true, Revision: 1, CapturedAt: now}
	start := make(chan struct{})
	results := make(chan error, 12)
	var group sync.WaitGroup
	for index := 0; index < 12; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, saveErr := baselineRepo.SaveWorkspaceBaseline(ctx, baseline)
			results <- saveErr
		}()
	}
	close(start)
	group.Wait()
	close(results)
	for saveErr := range results {
		if saveErr != nil {
			t.Fatalf("相同 baseline 并发保存不应产生冲突: %v", saveErr)
		}
	}
	got, err := baselineRepo.GetWorkspaceBaseline(ctx, invocation.ID)
	if err != nil || got.StatusDigest != baseline.StatusDigest {
		t.Fatalf("并发保存后的 baseline=%#v err=%v", got, err)
	}
}

func TestRuntimeRepositoryPersistsTaskContractHistoryWithCAS(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo := store.RuntimeRepository()
	now := time.Now().UTC().Truncate(time.Microsecond)
	invocation := agentruntime.Invocation{ID: "inv-contract-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Message: "实现功能", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	contractRepo, ok := repo.(agentruntime.TaskContractRepository)
	if !ok {
		t.Fatal("SQLite repository 未实现 TaskContractRepository")
	}
	first := agentruntime.InitialTaskContract(invocation.ID, invocation.Message, now)
	if err := contractRepo.CreateTaskContract(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Version = 2
	second.Goal = "实现功能并验证"
	second.AcceptanceCriteria = []agentruntime.Criterion{{ID: "build", Description: "构建通过"}}
	second.UpdatedAt = now.Add(time.Second)
	got, err := contractRepo.UpdateTaskContract(context.Background(), second, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 || got.Goal != second.Goal {
		t.Fatalf("更新后的 contract=%#v", got)
	}
	if _, err := contractRepo.UpdateTaskContract(context.Background(), second, 1); err != agentruntime.ErrConflict {
		t.Fatalf("过期 contract 更新应冲突，实际=%v", err)
	}
	latest, err := contractRepo.GetTaskContract(context.Background(), invocation.ID)
	if err != nil || latest.Version != 2 || len(latest.AcceptanceCriteria) != 1 {
		t.Fatalf("最新 contract=%#v err=%v", latest, err)
	}
	history, err := contractRepo.ListTaskContracts(context.Background(), invocation.ID)
	if err != nil || len(history) != 2 || history[0].Version != 1 || history[1].Version != 2 {
		t.Fatalf("contract 历史=%#v err=%v", history, err)
	}
}

func TestRuntimeRepositoryPersistsContextManifest(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo := store.RuntimeRepository()
	now := time.Now().UTC()
	invocation := agentruntime.Invocation{ID: "inv-manifest-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	manifestRepo, ok := repo.(agentruntime.ContextManifestRepository)
	if !ok {
		t.Fatal("SQLite repository 未实现 ContextManifestRepository")
	}
	manifest := agentruntime.ContextManifest{ID: "manifest-sqlite", InvocationID: invocation.ID, ModelCallID: "inv-manifest-sqlite:model:1", BuilderVersion: "context-builder-v1", EstimatorVersion: "heuristic-v1", EstimatorName: "abot-heuristic", EstimateQuality: "calibrated", EstimateMultiplier: 1.25, Model: "demo", ContextWindow: 1000, EstimatedInput: 10, Included: []agentruntime.ContextManifestItem{{ID: "contract:v1", Kind: "task_contract", Priority: "P0", TokenEstimate: 10}}, Digest: "digest", CreatedAt: now}
	if err := manifestRepo.SaveContextManifest(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	got, err := manifestRepo.GetContextManifest(context.Background(), manifest.ModelCallID)
	if err != nil || got.InvocationID != invocation.ID || got.EstimatorName != manifest.EstimatorName || got.EstimateQuality != manifest.EstimateQuality || got.EstimateMultiplier != manifest.EstimateMultiplier || len(got.Included) != 1 {
		t.Fatalf("SQLite manifest=%#v err=%v", got, err)
	}
	items, err := manifestRepo.ListContextManifests(context.Background(), invocation.ID)
	if err != nil || len(items) != 1 {
		t.Fatalf("SQLite manifest 列表=%#v err=%v", items, err)
	}
}

func TestRuntimeRepositoryPersistsRuntimeSnapshotWithRevision(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	now := time.Now().UTC()
	repo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-snapshot-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: agentruntime.InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	snapshotRepo, ok := repo.(agentruntime.RuntimeSnapshotRepository)
	if !ok {
		t.Fatal("SQLite runtime repository 未实现 RuntimeSnapshotRepository")
	}
	want := agentruntime.RuntimeSnapshot{InvocationID: invocation.ID, Revision: 1, ContractVersion: 2, PlanRevision: 3, Phase: string(agentruntime.InvocationRunning), ActivePlanStep: &agentruntime.PlanStepRef{ID: "verify", Title: "验证"}, PendingApprovals: []agentruntime.ApprovalRef{{ID: "approval-1", ToolName: "guarded"}}, Blockers: []agentruntime.Blocker{{Code: "tool_failed", Message: "失败"}}, Budget: agentruntime.BudgetStatus{ContextWindow: 10000, EstimatedInput: 200}, GeneratedAt: now}
	saved, err := snapshotRepo.SaveRuntimeSnapshot(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 1 {
		t.Fatalf("保存 snapshot revision=%d", saved.Revision)
	}
	got, err := snapshotRepo.GetRuntimeSnapshot(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContractVersion != 2 || got.PlanRevision != 3 || got.ActivePlanStep == nil || got.ActivePlanStep.ID != "verify" || len(got.PendingApprovals) != 1 || got.Budget.EstimatedInput != 200 {
		t.Fatalf("SQLite snapshot=%#v", got)
	}
	want.Revision = 1
	if _, err := snapshotRepo.SaveRuntimeSnapshot(ctx, want); err != agentruntime.ErrConflict {
		t.Fatalf("旧 snapshot revision 应冲突，实际=%v", err)
	}
}

func TestRuntimeRepositoryCommitsRuntimeSnapshotAndEventAtomically(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	now := time.Date(2026, 9, 12, 13, 0, 0, 0, time.UTC)
	repo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-snapshot-commit-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	commitRepo, ok := repo.(agentruntime.RuntimeSnapshotCommitRepository)
	if !ok {
		t.Fatal("SQLite runtime repository 未实现 RuntimeSnapshotCommitRepository")
	}
	first := agentruntime.RuntimeSnapshot{InvocationID: invocation.ID, Revision: 1, Phase: string(agentruntime.InvocationRunning), GeneratedAt: now}
	saved, event, err := commitRepo.CommitRuntimeSnapshot(ctx, first, agentruntime.AgentEvent{ID: "snapshot-event-sqlite-1", InvocationID: invocation.ID, Type: agentruntime.EventRuntimeSnapshot, Timestamp: now})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 1 || event.Sequence != 1 {
		t.Fatalf("首个 snapshot/event 状态错误: snapshot=%#v event=%#v", saved, event)
	}
	events, err := repo.ListEvents(ctx, invocation.ID, 0, 10)
	if err != nil || len(events) != 1 || events[0].ID != event.ID || events[0].Type != agentruntime.EventRuntimeSnapshot {
		t.Fatalf("snapshot 事件未与快照同提交: events=%#v err=%v", events, err)
	}
	eventOutboxRepo, ok := repo.(agentruntime.RuntimeEventOutboxRepository)
	if !ok {
		t.Fatal("SQLite Runtime Repository 未实现 RuntimeEventOutboxRepository")
	}
	outboxItems, err := eventOutboxRepo.ListRuntimeEventOutbox(ctx, invocation.ID, "", 10)
	if err != nil || len(outboxItems) != 1 || outboxItems[0].EventID != event.ID {
		t.Fatalf("snapshot 成功提交后应有对应 event outbox: items=%#v err=%v", outboxItems, err)
	}

	// The duplicate event primary key is inserted after the snapshot row. The
	// transaction must roll back the newer snapshot when that final write fails.
	second := first
	second.Revision = 2
	second.Phase = string(agentruntime.InvocationCompleted)
	if _, _, err := commitRepo.CommitRuntimeSnapshot(ctx, second, agentruntime.AgentEvent{ID: event.ID, InvocationID: invocation.ID, Type: agentruntime.EventRuntimeSnapshot, Timestamp: now.Add(time.Second)}); err == nil {
		t.Fatal("重复事件主键应使 snapshot atomic commit 失败")
	}
	snapshotRepo := repo.(agentruntime.RuntimeSnapshotRepository)
	current, err := snapshotRepo.GetRuntimeSnapshot(ctx, invocation.ID)
	if err != nil || current.Revision != 1 || current.Phase != string(agentruntime.InvocationRunning) {
		t.Fatalf("事件失败后 snapshot 不应变更: snapshot=%#v err=%v", current, err)
	}
	events, err = repo.ListEvents(ctx, invocation.ID, 0, 10)
	if err != nil || len(events) != 1 || events[0].ID != event.ID {
		t.Fatalf("事件失败后不应追加新事件: events=%#v err=%v", events, err)
	}
	outboxItems, err = eventOutboxRepo.ListRuntimeEventOutbox(ctx, invocation.ID, "", 10)
	if err != nil || len(outboxItems) != 1 || outboxItems[0].EventID != event.ID {
		t.Fatalf("snapshot 事务失败后不应留下新的 event outbox: items=%#v err=%v", outboxItems, err)
	}
}

func TestRuntimeRepositoryPersistsWorkingSetWithCAS(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	now := time.Now().UTC()
	repo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-working-set-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	workingSetRepo, ok := repo.(agentruntime.WorkingSetRepository)
	if !ok {
		t.Fatal("SQLite runtime repository 未实现 WorkingSetRepository")
	}
	want := agentruntime.WorkingSet{InvocationID: invocation.ID, Revision: 1, Items: []agentruntime.WorkingSetItem{{ID: "slice", Kind: "file_slice", SourceRef: "file:a", Priority: 1, RelevanceScore: 0.4, TokenEstimate: 4, ContentDigest: "sha", Freshness: agent.WorkingSetFresh, CreatedAt: now, ValidatedAt: now}}}
	if _, err := workingSetRepo.ReplaceWorkingSet(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := workingSetRepo.GetWorkingSet(ctx, invocation.ID)
	if err != nil || len(got.Items) != 1 || got.Items[0].ContentDigest != "sha" {
		t.Fatalf("SQLite working set=%#v err=%v", got, err)
	}
	want.Revision = 1
	if _, err := workingSetRepo.ReplaceWorkingSet(ctx, want); err != agentruntime.ErrConflict {
		t.Fatalf("旧 Working Set revision 应冲突，实际=%v", err)
	}
}

func TestRuntimeRepositoryPersistsVerificationRunWithCAS(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	now := time.Now().UTC()
	repo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-verification-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: agentruntime.InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	verificationRepo, ok := repo.(agentruntime.VerificationRepository)
	if !ok {
		t.Fatal("SQLite runtime repository 未实现 VerificationRepository")
	}
	want := agentruntime.VerificationRun{ID: "verification-sqlite", InvocationID: invocation.ID, PlanStepID: "verify", Kind: "command", Command: "go test", Status: agentruntime.VerificationRunning, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := verificationRepo.CreateVerificationRun(ctx, want); err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	want.Status = agentruntime.VerificationPassed
	want.ExitCode = &exitCode
	want.Summary = "通过"
	want.Revision = 2
	got, err := verificationRepo.UpdateVerificationRun(ctx, want, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 2 || got.Status != agentruntime.VerificationPassed || got.ExitCode == nil {
		t.Fatalf("SQLite verification=%#v", got)
	}
	if _, err := verificationRepo.UpdateVerificationRun(ctx, want, 1); err != agentruntime.ErrConflict {
		t.Fatalf("旧 verification revision 应冲突，实际=%v", err)
	}
}

func TestRuntimeRepositoryPersistsImmutableToolSetSnapshot(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	now := time.Now().UTC()
	repo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-toolset-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	snapshotRepo, ok := repo.(agentruntime.ToolSetSnapshotRepository)
	if !ok {
		t.Fatal("SQLite repository 未实现 ToolSetSnapshotRepository")
	}
	want := agentruntime.ToolSetSnapshot{ID: "toolset-sqlite", InvocationID: invocation.ID, CatalogRevision: "catalog-1", PolicyRevision: "policy-1", ModelProfile: "profile-1", Tools: []agentruntime.ToolRef{{ID: "builtin.calculate", ModelName: "calculate", Version: "1.0.0", SchemaDigest: "schema-1"}}, Digest: "digest-1", CreatedAt: now}
	saved, err := snapshotRepo.SaveToolSetSnapshot(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID != want.ID || len(saved.Tools) != 1 {
		t.Fatalf("保存 toolset=%#v", saved)
	}
	got, err := snapshotRepo.GetToolSetSnapshot(ctx, invocation.ID)
	if err != nil || got.Digest != want.Digest || got.Tools[0].ModelName != "calculate" {
		t.Fatalf("读取 toolset=%#v err=%v", got, err)
	}
	if _, err := snapshotRepo.SaveToolSetSnapshot(ctx, agentruntime.ToolSetSnapshot{ID: "toolset-sqlite-new", InvocationID: invocation.ID, CatalogRevision: "catalog-2", Tools: want.Tools, Digest: "digest-2", CreatedAt: now}); !errors.Is(err, agentruntime.ErrToolSnapshotConflict) {
		t.Fatalf("不同 digest 应阻止静默替换，err=%v", err)
	}
}

func TestRuntimeRepositoryPersistsImmutableModelCapabilitySnapshot(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	now := time.Now().UTC()
	repo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-capability-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	snapshotRepo, ok := repo.(agentruntime.ModelCapabilitySnapshotRepository)
	if !ok {
		t.Fatal("SQLite repository 未实现 ModelCapabilitySnapshotRepository")
	}
	profile := provider.DefaultCapabilities(provider.Provider{ID: "demo", Protocol: provider.ProtocolOpenAICompatible}, provider.Model{ID: "model", ContextWindow: 8192})
	negotiated, err := provider.Negotiate(profile, provider.ModelRequirements{}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := agentruntime.ModelCapabilitySnapshot{ID: "capability-sqlite", InvocationID: invocation.ID, Result: negotiated, CreatedAt: now}
	saved, err := snapshotRepo.SaveModelCapabilitySnapshot(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID != want.ID || saved.Result.ProfileSnapshotID == "" {
		t.Fatalf("保存能力 snapshot=%#v", saved)
	}
	got, err := snapshotRepo.GetModelCapabilitySnapshot(ctx, invocation.ID)
	if err != nil || got.Result.ProfileSnapshotID != want.Result.ProfileSnapshotID || got.Result.RequestPlan.Streaming != want.Result.RequestPlan.Streaming {
		t.Fatalf("读取能力 snapshot=%#v err=%v", got, err)
	}
	negotiated.RequestPlan.Streaming = !negotiated.RequestPlan.Streaming
	if _, err := snapshotRepo.SaveModelCapabilitySnapshot(ctx, agentruntime.ModelCapabilitySnapshot{ID: "capability-new", InvocationID: invocation.ID, Result: negotiated, CreatedAt: now}); !errors.Is(err, agentruntime.ErrCapabilitySnapshotConflict) {
		t.Fatalf("不同能力计划应阻止静默替换，err=%v", err)
	}
}

func TestRuntimeRepositoryPersistsInstructionSnapshotSet(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	now := time.Now().UTC()
	repo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-instruction-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	snapshotRepo, ok := repo.(agentruntime.InstructionSnapshotRepository)
	if !ok {
		t.Fatal("SQLite runtime repository 未实现 InstructionSnapshotRepository")
	}
	saved, err := snapshotRepo.ReplaceInstructionSnapshotSet(ctx, agentruntime.InstructionSnapshotSet{
		InvocationID: invocation.ID, TargetPath: "src/main.go",
		Snapshots: []agentruntime.InstructionSnapshot{{Path: "AGENTS.md", ScopePath: ".", Source: "agents", ContentDigest: "sha-1", Content: "run tests", Priority: 1, LoadedAt: now}},
	})
	if err != nil || saved.Revision != 1 {
		t.Fatalf("保存指令快照=%#v err=%v", saved, err)
	}
	got, err := snapshotRepo.GetInstructionSnapshotSet(ctx, invocation.ID)
	if err != nil || got.TargetPath != "src/main.go" || len(got.Snapshots) != 1 || got.Snapshots[0].Content != "run tests" {
		t.Fatalf("读取指令快照=%#v err=%v", got, err)
	}
	updated, err := snapshotRepo.ReplaceInstructionSnapshotSet(ctx, agentruntime.InstructionSnapshotSet{InvocationID: invocation.ID, Snapshots: []agentruntime.InstructionSnapshot{}})
	if err != nil || updated.Revision != 2 || len(updated.Snapshots) != 0 {
		t.Fatalf("替换指令快照=%#v err=%v", updated, err)
	}
}

func TestRuntimeRepositoryPersistsInvocationAttachments(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开 SQLite Store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	now := time.Now().UTC().Truncate(time.Microsecond)
	want := agentruntime.Invocation{
		ID: "invocation-attachments", UserID: "user-1", IdempotencyKey: "attachment-request", BotID: "bot-1",
		ConversationID: "conversation-1", WorkspaceID: "workspace-1", SessionID: "session-1", Message: "看这张图",
		Attachments: []agent.Attachment{{Name: "screenshot.png", MIMEType: "image/png", Data: []byte{0, 1, 2, 255}}},
		Status:      agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.RuntimeRepository().CreateInvocation(context.Background(), want); err != nil {
		t.Fatalf("写入 Invocation 失败: %v", err)
	}
	got, err := store.RuntimeRepository().GetInvocation(context.Background(), want.ID)
	if err != nil {
		t.Fatalf("读取 Invocation 失败: %v", err)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].Name != want.Attachments[0].Name || got.Attachments[0].MIMEType != want.Attachments[0].MIMEType {
		t.Fatalf("附件元数据未恢复: %#v", got.Attachments)
	}
	if got.WorkspaceID != want.WorkspaceID {
		t.Fatalf("工作区关联未恢复: got=%q want=%q", got.WorkspaceID, want.WorkspaceID)
	}
	if got.IdempotencyKey != want.IdempotencyKey {
		t.Fatalf("幂等键未恢复: got=%q want=%q", got.IdempotencyKey, want.IdempotencyKey)
	}
	if string(got.Attachments[0].Data) != string(want.Attachments[0].Data) {
		t.Fatalf("附件数据未恢复: got=%v want=%v", got.Attachments[0].Data, want.Attachments[0].Data)
	}
	byKey, err := store.RuntimeRepository().(agentruntime.InvocationIdempotencyRepository).GetInvocationByIdempotencyKey(context.Background(), want.UserID, want.ConversationID, want.IdempotencyKey)
	if err != nil || byKey.ID != want.ID {
		t.Fatalf("按幂等键读取失败: invocation=%#v err=%v", byKey, err)
	}
}

func TestRuntimeRepositoryPersistsArtifactAttachmentRefsWithoutInlineBytes(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开 SQLite Store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	now := time.Now().UTC().Truncate(time.Microsecond)
	want := agentruntime.Invocation{
		ID: "invocation-artifact-ref", UserID: "user-1", ConversationID: "conversation-1", SessionID: "session-1", Message: "读取引用",
		Attachments: []agent.Attachment{{Name: "note.txt", MIMEType: "text/plain", Ref: &agent.AttachmentRef{ID: "artifact-1", Version: 2, Digest: "sha256:" + strings.Repeat("a", 64), Kind: "input_attachment", MIMEType: "text/plain", Size: 12, Name: "note.txt"}}},
		Status:      agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.RuntimeRepository().CreateInvocation(context.Background(), want); err != nil {
		t.Fatalf("写入带 Artifact ref 的 Invocation 失败: %v", err)
	}
	got, err := store.RuntimeRepository().GetInvocation(context.Background(), want.ID)
	if err != nil {
		t.Fatalf("读取带 Artifact ref 的 Invocation 失败: %v", err)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].Ref == nil || got.Attachments[0].Ref.ID != "artifact-1" || got.Attachments[0].Ref.Version != 2 {
		t.Fatalf("Artifact ref 未恢复: %#v", got.Attachments)
	}
	if len(got.Attachments[0].Data) != 0 || got.Attachments[0].Ref.Digest != want.Attachments[0].Ref.Digest {
		t.Fatalf("持久化 ref 不应包含 inline bytes 或丢失 digest: %#v", got.Attachments[0])
	}
}

func TestRuntimeRepositoryPersistsInvocationResumeHandoffIdempotently(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo := store.RuntimeRepository()
	now := time.Now().UTC()
	invocation := agentruntime.Invocation{ID: "invocation-resume-handoff", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	handoffRepo, ok := repo.(agentruntime.InvocationResumeRepository)
	if !ok {
		t.Fatal("SQLite runtime repository 未实现 InvocationResumeRepository")
	}
	want := agentruntime.InvocationResume{ID: "resume-handoff-1", InvocationID: invocation.ID, WaitID: "wait-1", Name: "external_wait", ResponseJSON: []byte(`{"output":"ready"}`), RequestDigest: "sha256:resume-1", CreatedAt: now}
	saved, err := handoffRepo.SaveInvocationResume(context.Background(), want)
	if err != nil || saved.WaitID != want.WaitID || string(saved.ResponseJSON) != string(want.ResponseJSON) {
		t.Fatalf("保存 resume handoff=%#v err=%v", saved, err)
	}
	duplicate, err := handoffRepo.SaveInvocationResume(context.Background(), want)
	if err != nil || duplicate.ID != saved.ID {
		t.Fatalf("相同 resume handoff 应幂等: duplicate=%#v err=%v", duplicate, err)
	}
	if _, err := handoffRepo.SaveInvocationResume(context.Background(), agentruntime.InvocationResume{ID: "resume-handoff-2", InvocationID: invocation.ID, WaitID: "wait-1", Name: "external_wait", ResponseJSON: []byte(`{"output":"different"}`), RequestDigest: "sha256:resume-2", CreatedAt: now}); !errors.Is(err, agentruntime.ErrConflict) {
		t.Fatalf("不同 digest 不应覆盖 pending handoff: %v", err)
	}
	loaded, err := handoffRepo.GetInvocationResume(context.Background(), invocation.ID)
	if err != nil || loaded.RequestDigest != want.RequestDigest {
		t.Fatalf("读取 resume handoff=%#v err=%v", loaded, err)
	}
	if err := handoffRepo.DeleteInvocationResume(context.Background(), invocation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := handoffRepo.GetInvocationResume(context.Background(), invocation.ID); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("删除后应返回 not found: %v", err)
	}
}

func TestRuntimeRepositoryPersistsInvocationResumeOutboxWithLeasesAndRetries(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo := store.RuntimeRepository()
	outboxRepo, ok := repo.(agentruntime.InvocationResumeOutboxRepository)
	if !ok {
		t.Fatal("SQLite runtime repository 未实现 InvocationResumeOutboxRepository")
	}
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	invocation := agentruntime.Invocation{ID: "invocation-resume-outbox", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	base := agentruntime.InvocationResumeOutbox{
		ID: "sqlite-resume-outbox-1", InvocationID: invocation.ID, WaitID: "wait-1", Name: "external_wait",
		ResponseJSON: []byte(`{"output":"ready"}`), RequestDigest: "sha256:sqlite-1", Status: agentruntime.InvocationResumeOutboxQueued,
		AvailableAt: now, CreatedAt: now, UpdatedAt: now,
	}
	saved, err := outboxRepo.EnqueueInvocationResumeOutbox(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := outboxRepo.EnqueueInvocationResumeOutbox(context.Background(), base)
	if err != nil || duplicate.ID != saved.ID || duplicate.Revision != saved.Revision {
		t.Fatalf("SQLite outbox 同 digest 写入应幂等: duplicate=%#v err=%v", duplicate, err)
	}
	claimed, ok, err := outboxRepo.ClaimInvocationResumeOutbox(context.Background(), invocation.ID, "worker-a", now, time.Minute)
	if err != nil || !ok || claimed.Attempt != 1 || claimed.Status != agentruntime.InvocationResumeOutboxProcessing {
		t.Fatalf("SQLite 首次 claim 失败: item=%#v ok=%v err=%v", claimed, ok, err)
	}
	if _, ok, err := outboxRepo.ClaimInvocationResumeOutbox(context.Background(), invocation.ID, "worker-b", now.Add(30*time.Second), time.Minute); err != nil || ok {
		t.Fatalf("SQLite live lease 不应被抢占: ok=%v err=%v", ok, err)
	}
	if retried, err := outboxRepo.RetryInvocationResumeOutbox(context.Background(), invocation.ID, "worker-a", now, "temporary failure"); err != nil || !retried {
		t.Fatalf("SQLite owner 重试失败: retried=%v err=%v", retried, err)
	}
	if _, ok, err := outboxRepo.ClaimInvocationResumeOutbox(context.Background(), invocation.ID, "worker-b", now.Add(500*time.Millisecond), time.Minute); err != nil || ok {
		t.Fatalf("SQLite 退避窗口内不应 claim: ok=%v err=%v", ok, err)
	}
	claimed, ok, err = outboxRepo.ClaimInvocationResumeOutbox(context.Background(), invocation.ID, "worker-b", now.Add(2*time.Second), time.Minute)
	if err != nil || !ok || claimed.Attempt != 2 || claimed.LeaseOwner != "worker-b" {
		t.Fatalf("SQLite 退避后接管失败: item=%#v ok=%v err=%v", claimed, ok, err)
	}
	if completed, err := outboxRepo.CompleteInvocationResumeOutbox(context.Background(), invocation.ID, "worker-a", now.Add(3*time.Second)); err != nil || completed {
		t.Fatalf("SQLite 旧 owner 不应完成: completed=%v err=%v", completed, err)
	}
	if completed, err := outboxRepo.CompleteInvocationResumeOutbox(context.Background(), invocation.ID, "worker-b", now.Add(3*time.Second)); err != nil || !completed {
		t.Fatalf("SQLite 当前 owner 完成失败: completed=%v err=%v", completed, err)
	}
	loaded, err := outboxRepo.GetInvocationResumeOutbox(context.Background(), invocation.ID)
	if err != nil || loaded.Status != agentruntime.InvocationResumeOutboxCompleted || loaded.Attempt != 2 || loaded.LeaseOwner != "" {
		t.Fatalf("SQLite 完成状态错误: item=%#v err=%v", loaded, err)
	}
	if _, ok, err := outboxRepo.ClaimInvocationResumeOutbox(context.Background(), invocation.ID, "worker-c", now.Add(time.Hour), time.Minute); err != nil || ok {
		t.Fatalf("SQLite completed outbox 不应重放: ok=%v err=%v", ok, err)
	}
}

func TestRuntimeRepositoryInvocationResumeOutboxLeaseExpiryFailsClosed(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	repo := store.RuntimeRepository()
	outboxRepo, ok := repo.(agentruntime.InvocationResumeOutboxRepository)
	if !ok {
		t.Fatal("SQLite runtime repository 未实现 InvocationResumeOutboxRepository")
	}
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	invocation := agentruntime.Invocation{ID: "invocation-resume-outbox-expiry", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	if _, err := outboxRepo.EnqueueInvocationResumeOutbox(ctx, agentruntime.InvocationResumeOutbox{
		InvocationID: invocation.ID, WaitID: "wait-expiry", Name: "external_wait", ResponseJSON: []byte(`{"ok":true}`), RequestDigest: "sha256:expiry", AvailableAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := outboxRepo.ClaimInvocationResumeOutbox(ctx, invocation.ID, "worker-a", now, time.Minute); err != nil || !claimed {
		t.Fatalf("SQLite 首次 claim 失败: claimed=%v err=%v", claimed, err)
	}
	if completed, err := outboxRepo.CompleteInvocationResumeOutbox(ctx, invocation.ID, "worker-a", now.Add(time.Minute)); err != nil || completed {
		t.Fatalf("SQLite 租约到期后旧 worker 不应完成: completed=%v err=%v", completed, err)
	}
	if retried, err := outboxRepo.RetryInvocationResumeOutbox(ctx, invocation.ID, "worker-a", now.Add(time.Minute), "late"); err != nil || retried {
		t.Fatalf("SQLite 租约到期后旧 worker 不应重试: retried=%v err=%v", retried, err)
	}
	current, err := outboxRepo.GetInvocationResumeOutbox(ctx, invocation.ID)
	if err != nil || current.Status != agentruntime.InvocationResumeOutboxProcessing || current.Revision != 2 {
		t.Fatalf("SQLite 迟到操作不应改变 processing 记录: item=%#v err=%v", current, err)
	}
	if _, claimed, err := outboxRepo.ClaimInvocationResumeOutbox(ctx, invocation.ID, "worker-b", now.Add(time.Minute), time.Minute); err != nil || !claimed {
		t.Fatalf("SQLite 租约到期后新 worker 应可接管: claimed=%v err=%v", claimed, err)
	}
	if completed, err := outboxRepo.CompleteInvocationResumeOutbox(ctx, invocation.ID, "worker-a", now.Add(time.Minute+time.Second)); err != nil || completed {
		t.Fatalf("SQLite 接管后旧 worker 不应完成: completed=%v err=%v", completed, err)
	}
	if retried, err := outboxRepo.RetryInvocationResumeOutbox(ctx, invocation.ID, "worker-a", now.Add(time.Minute+time.Second), "late"); err != nil || retried {
		t.Fatalf("SQLite 接管后旧 worker 不应重试: retried=%v err=%v", retried, err)
	}
	if completed, err := outboxRepo.CompleteInvocationResumeOutbox(ctx, invocation.ID, "worker-b", now.Add(time.Minute+time.Second)); err != nil || !completed {
		t.Fatalf("SQLite 当前 worker 应能完成接管后的记录: completed=%v err=%v", completed, err)
	}
}

func TestRuntimeRepositoryInvocationResumeOutboxLeaseBoundsAndMalformedMetadata(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	repo := store.RuntimeRepository()
	outboxRepo, ok := repo.(agentruntime.InvocationResumeOutboxRepository)
	if !ok {
		t.Fatal("SQLite runtime repository 未实现 InvocationResumeOutboxRepository")
	}
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	invocation := agentruntime.Invocation{ID: "invocation-resume-outbox-bounds", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	base := agentruntime.InvocationResumeOutbox{InvocationID: invocation.ID, WaitID: "wait-bounds", Name: "external_wait", ResponseJSON: []byte(`{"ok":true}`), RequestDigest: "sha256:bounds", AvailableAt: now, CreatedAt: now}
	if _, err := outboxRepo.EnqueueInvocationResumeOutbox(ctx, agentruntime.InvocationResumeOutbox{
		InvocationID: base.InvocationID, WaitID: base.WaitID, Name: base.Name, ResponseJSON: base.ResponseJSON, RequestDigest: base.RequestDigest,
		Status: agentruntime.InvocationResumeOutboxProcessing, CreatedAt: now, LeaseExpiresAt: func() *time.Time { value := now.Add(time.Minute); return &value }(),
	}); !errors.Is(err, agentruntime.ErrInvalidResume) {
		t.Fatalf("SQLite 缺少 processing owner 必须拒绝: %v", err)
	}
	if _, err := outboxRepo.EnqueueInvocationResumeOutbox(ctx, base); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := outboxRepo.ClaimInvocationResumeOutbox(ctx, invocation.ID, strings.Repeat("x", agentruntime.MaxInvocationResumeOutboxOwnerLength+1), now, time.Minute); !errors.Is(err, agentruntime.ErrConflict) || claimed {
		t.Fatalf("SQLite 过长 owner 必须拒绝: claimed=%v err=%v", claimed, err)
	}
	if _, claimed, err := outboxRepo.ClaimInvocationResumeOutbox(ctx, invocation.ID, "bounded", now, agentruntime.MaxInvocationResumeOutboxLease+time.Nanosecond); !errors.Is(err, agentruntime.ErrConflict) || claimed {
		t.Fatalf("SQLite 过长 lease 必须拒绝: claimed=%v err=%v", claimed, err)
	}
}

func TestRuntimeRepositoryCommitInvocationResumeIsAtomic(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	repo := store.RuntimeRepository()
	commitRepo, ok := repo.(agentruntime.InvocationResumeCommitRepository)
	if !ok {
		t.Fatal("SQLite runtime repository 未实现 InvocationResumeCommitRepository")
	}
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	invocation := agentruntime.Invocation{ID: "invocation-resume-commit", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now, LeaseOwner: "stale-worker"}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	resume := agentruntime.InvocationResume{ID: "resume-commit", InvocationID: invocation.ID, WaitID: "wait-1", Name: "external_wait", ResponseJSON: []byte(`{"output":"ready"}`), RequestDigest: "sha256:commit", CreatedAt: now}
	commit := agentruntime.InvocationResumeCommit{
		InvocationID: invocation.ID, FromStatus: agentruntime.InvocationWaitingTool, ToStatus: agentruntime.InvocationQueued,
		Resume: resume,
		Outbox: agentruntime.InvocationResumeOutbox{ID: "outbox-commit", InvocationID: invocation.ID, WaitID: resume.WaitID, Name: resume.Name, ResponseJSON: resume.ResponseJSON, RequestDigest: resume.RequestDigest, Status: agentruntime.InvocationResumeOutboxQueued, CreatedAt: now, UpdatedAt: now, AvailableAt: now},
		Event:  agentruntime.AgentEvent{ID: "event-resume-commit", InvocationID: invocation.ID, Type: agentruntime.EventInvocationResumed, Timestamp: now, Data: map[string]any{"reason": "tool", "request_digest": resume.RequestDigest}},
	}
	saved, event, err := commitRepo.CommitInvocationResume(ctx, commit)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != agentruntime.InvocationQueued || saved.LeaseOwner != "" || event.Sequence != 1 {
		t.Fatalf("atomic commit 后 invocation/event 状态错误: invocation=%#v event=%#v", saved, event)
	}
	handoffRepo := repo.(agentruntime.InvocationResumeRepository)
	handoff, err := handoffRepo.GetInvocationResume(ctx, invocation.ID)
	if err != nil || handoff.RequestDigest != resume.RequestDigest {
		t.Fatalf("atomic commit 未保存 handoff: handoff=%#v err=%v", handoff, err)
	}
	outboxRepo := repo.(agentruntime.InvocationResumeOutboxRepository)
	outbox, err := outboxRepo.GetInvocationResumeOutbox(ctx, invocation.ID)
	if err != nil || outbox.Status != agentruntime.InvocationResumeOutboxQueued || outbox.RequestDigest != resume.RequestDigest {
		t.Fatalf("atomic commit 未保存 queued outbox: outbox=%#v err=%v", outbox, err)
	}
	events, err := repo.ListEvents(ctx, invocation.ID, 0, 10)
	if err != nil || len(events) != 1 || events[0].ID != event.ID {
		t.Fatalf("atomic commit 事件不完整: events=%#v err=%v", events, err)
	}
	eventOutbox := repo.(agentruntime.RuntimeEventOutboxRepository)
	outboxItems, err := eventOutbox.ListRuntimeEventOutbox(ctx, invocation.ID, agentruntime.RuntimeEventOutboxQueued, 10)
	if err != nil || len(outboxItems) != 1 || outboxItems[0].EventID != event.ID {
		t.Fatalf("atomic commit 必须登记事件 outbox: outbox=%#v err=%v", outboxItems, err)
	}

	// A duplicate event primary key forces the final insert to fail. The
	// transaction must roll back the earlier handoff/outbox writes and CAS.
	rollbackInvocation := agentruntime.Invocation{ID: "invocation-resume-commit-rollback", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, rollbackInvocation); err != nil {
		t.Fatal(err)
	}
	duplicateEvent := agentruntime.AgentEvent{ID: "event-resume-rollback", InvocationID: rollbackInvocation.ID, Type: agentruntime.EventInvocationWaiting, Timestamp: now, Data: map[string]any{"reason": "tool"}}
	if _, err := repo.AppendEvent(ctx, duplicateEvent); err != nil {
		t.Fatal(err)
	}
	rollback := commit
	rollback.InvocationID = rollbackInvocation.ID
	rollback.Resume = agentruntime.InvocationResume{ID: "rollback-resume", InvocationID: rollbackInvocation.ID, WaitID: "wait-rollback", Name: "external_wait", ResponseJSON: []byte(`{"output":"rollback"}`), RequestDigest: "sha256:rollback", CreatedAt: now}
	rollback.Outbox = agentruntime.InvocationResumeOutbox{ID: "rollback-outbox", InvocationID: rollbackInvocation.ID, WaitID: rollback.Resume.WaitID, Name: rollback.Resume.Name, ResponseJSON: rollback.Resume.ResponseJSON, RequestDigest: rollback.Resume.RequestDigest, Status: agentruntime.InvocationResumeOutboxQueued, CreatedAt: now, UpdatedAt: now, AvailableAt: now}
	rollback.Event = agentruntime.AgentEvent{ID: duplicateEvent.ID, InvocationID: rollbackInvocation.ID, Type: agentruntime.EventInvocationResumed, Timestamp: now, Data: map[string]any{"reason": "tool", "request_digest": rollback.Resume.RequestDigest}}
	if _, _, err := commitRepo.CommitInvocationResume(ctx, rollback); err == nil {
		t.Fatal("重复事件主键应使 atomic commit 失败")
	}
	current, err := repo.GetInvocation(ctx, rollbackInvocation.ID)
	if err != nil || current.Status != agentruntime.InvocationWaitingTool {
		t.Fatalf("rollback 后 invocation 不应变更: current=%#v err=%v", current, err)
	}
	if _, err := handoffRepo.GetInvocationResume(ctx, rollbackInvocation.ID); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("rollback 后不应留下 handoff: %v", err)
	}
	if _, err := outboxRepo.GetInvocationResumeOutbox(ctx, rollbackInvocation.ID); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("rollback 后不应留下 outbox: %v", err)
	}
	events, err = repo.ListEvents(ctx, rollbackInvocation.ID, 0, 10)
	if err != nil || len(events) != 1 || events[0].ID != duplicateEvent.ID {
		t.Fatalf("rollback 后只应保留原事件: events=%#v err=%v", events, err)
	}
}

func TestRuntimeRepositoryCommitApprovalResumeIsAtomic(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	repo := store.RuntimeRepository()
	commitRepo, ok := repo.(agentruntime.ApprovalResumeCommitRepository)
	if !ok {
		t.Fatal("SQLite runtime repository 未实现 ApprovalResumeCommitRepository")
	}
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	invocation := agentruntime.Invocation{ID: "invocation-approval-commit", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingApproval, ActiveApprovalID: "approval-commit", CreatedAt: now, UpdatedAt: now, LeaseOwner: "stale-worker"}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	toolRepo := repo.(agentruntime.ToolCallRepository)
	toolCall := agentruntime.ToolCall{ID: "toolcall-approval-commit", InvocationID: invocation.ID, ConversationID: invocation.ConversationID, ToolName: "workspace_request_patch", OriginalCallID: "call-original", ConfirmationCallID: "call-confirmation", ApprovalID: "approval-commit", Status: agentruntime.ToolCallWaitingApproval, CreatedAt: now, UpdatedAt: now}
	if err := toolRepo.CreateToolCall(ctx, toolCall); err != nil {
		t.Fatal(err)
	}
	approval := agentruntime.Approval{ID: "approval-commit", InvocationID: invocation.ID, ConversationID: invocation.ConversationID, ToolCallID: toolCall.ID, ToolName: toolCall.ToolName, OriginalCallID: toolCall.OriginalCallID, ConfirmationCallID: toolCall.ConfirmationCallID, Status: agentruntime.ApprovalPending, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	resume := agentruntime.InvocationResume{ID: "resume-approval-commit", InvocationID: invocation.ID, WaitID: approval.ConfirmationCallID, Name: "adk_tool_confirmation", ResponseJSON: []byte(`{"confirmed":true}`), RequestDigest: "sha256:approval-commit", CreatedAt: now}
	commit := agentruntime.ApprovalResumeCommit{
		ApprovalID: approval.ID, ApprovalFromStatus: agentruntime.ApprovalPending, ApprovalToStatus: agentruntime.ApprovalApproved,
		InvocationID: invocation.ID, InvocationFromStatus: agentruntime.InvocationWaitingApproval, InvocationToStatus: agentruntime.InvocationQueued,
		ToolCallID: toolCall.ID, ToolCallStatus: agentruntime.ToolCallApproved, Reason: "approved",
		Resume: resume,
		Outbox: agentruntime.InvocationResumeOutbox{ID: "outbox-approval-commit", InvocationID: invocation.ID, WaitID: resume.WaitID, Name: resume.Name, ResponseJSON: resume.ResponseJSON, RequestDigest: resume.RequestDigest, Status: agentruntime.InvocationResumeOutboxQueued, CreatedAt: now, UpdatedAt: now, AvailableAt: now},
		Events: []agentruntime.AgentEvent{
			{ID: "event-approval-resolved", InvocationID: invocation.ID, Type: agentruntime.EventApprovalResolved, Timestamp: now, Data: map[string]any{"approval_id": approval.ID, "confirmed": true}},
			{ID: "event-approval-resumed", InvocationID: invocation.ID, Type: agentruntime.EventInvocationResumed, Timestamp: now, Data: map[string]any{"approval_id": approval.ID, "confirmed": true, "request_digest": resume.RequestDigest}},
		},
	}
	savedApproval, savedInvocation, events, err := commitRepo.CommitApprovalResume(ctx, commit)
	if err != nil {
		t.Fatal(err)
	}
	if savedApproval.Status != agentruntime.ApprovalApproved || savedInvocation.Status != agentruntime.InvocationQueued || savedInvocation.ActiveApprovalID != "" || savedInvocation.LeaseOwner != "" || len(events) != 2 || events[0].Sequence != 1 || events[1].Sequence != 2 {
		t.Fatalf("approval atomic commit 状态错误: approval=%#v invocation=%#v events=%#v", savedApproval, savedInvocation, events)
	}
	gotToolCall, err := toolRepo.GetToolCall(ctx, toolCall.ID)
	if err != nil || gotToolCall.Status != agentruntime.ToolCallApproved {
		t.Fatalf("ToolCall 未与 approval 同提交更新: toolcall=%#v err=%v", gotToolCall, err)
	}
	if _, err := repo.(agentruntime.InvocationResumeRepository).GetInvocationResume(ctx, invocation.ID); err != nil {
		t.Fatalf("approval atomic commit 未保存 handoff: %v", err)
	}
	if _, err := repo.(agentruntime.InvocationResumeOutboxRepository).GetInvocationResumeOutbox(ctx, invocation.ID); err != nil {
		t.Fatalf("approval atomic commit 未保存 outbox: %v", err)
	}
	eventOutbox := repo.(agentruntime.RuntimeEventOutboxRepository)
	outboxItems, err := eventOutbox.ListRuntimeEventOutbox(ctx, invocation.ID, agentruntime.RuntimeEventOutboxQueued, 10)
	if err != nil || len(outboxItems) != len(events) {
		t.Fatalf("approval atomic commit 必须为所有审计事件登记 outbox: outbox=%#v err=%v", outboxItems, err)
	}
	if _, _, _, err := commitRepo.CommitApprovalResume(ctx, commit); !errors.Is(err, agentruntime.ErrConflict) {
		t.Fatalf("已 queued/approved 的 approval 不应重复 commit: %v", err)
	}

	// Force the final event insert to violate its primary key. Everything
	// written before it must roll back, including Approval/ToolCall/Invocation
	// and both private delivery records.
	rollbackInvocation := agentruntime.Invocation{ID: "invocation-approval-rollback", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingApproval, ActiveApprovalID: "approval-rollback", CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, rollbackInvocation); err != nil {
		t.Fatal(err)
	}
	rollbackToolCall := agentruntime.ToolCall{ID: "toolcall-approval-rollback", InvocationID: rollbackInvocation.ID, ConversationID: rollbackInvocation.ConversationID, ToolName: "workspace_request_patch", OriginalCallID: "call-original-rollback", ConfirmationCallID: "call-confirmation-rollback", ApprovalID: "approval-rollback", Status: agentruntime.ToolCallWaitingApproval, CreatedAt: now, UpdatedAt: now}
	if err := toolRepo.CreateToolCall(ctx, rollbackToolCall); err != nil {
		t.Fatal(err)
	}
	rollbackApproval := agentruntime.Approval{ID: "approval-rollback", InvocationID: rollbackInvocation.ID, ConversationID: rollbackInvocation.ConversationID, ToolCallID: rollbackToolCall.ID, ToolName: rollbackToolCall.ToolName, OriginalCallID: rollbackToolCall.OriginalCallID, ConfirmationCallID: rollbackToolCall.ConfirmationCallID, Status: agentruntime.ApprovalPending, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateApproval(ctx, rollbackApproval); err != nil {
		t.Fatal(err)
	}
	duplicateEvent := agentruntime.AgentEvent{ID: "event-approval-rollback", InvocationID: rollbackInvocation.ID, Type: agentruntime.EventInvocationWaiting, Timestamp: now, Data: map[string]any{"reason": "approval"}}
	if _, err := repo.AppendEvent(ctx, duplicateEvent); err != nil {
		t.Fatal(err)
	}
	rollback := commit
	rollback.ApprovalID = rollbackApproval.ID
	rollback.InvocationID = rollbackInvocation.ID
	rollback.ToolCallID = rollbackToolCall.ID
	rollback.Resume = agentruntime.InvocationResume{ID: "rollback-resume", InvocationID: rollbackInvocation.ID, WaitID: rollbackApproval.ConfirmationCallID, Name: "adk_tool_confirmation", ResponseJSON: []byte(`{"confirmed":true}`), RequestDigest: "sha256:approval-rollback", CreatedAt: now}
	rollback.Outbox = agentruntime.InvocationResumeOutbox{ID: "rollback-outbox", InvocationID: rollbackInvocation.ID, WaitID: rollback.Resume.WaitID, Name: rollback.Resume.Name, ResponseJSON: rollback.Resume.ResponseJSON, RequestDigest: rollback.Resume.RequestDigest, Status: agentruntime.InvocationResumeOutboxQueued, CreatedAt: now, UpdatedAt: now, AvailableAt: now}
	rollback.Events = []agentruntime.AgentEvent{{ID: duplicateEvent.ID, InvocationID: rollbackInvocation.ID, Type: agentruntime.EventApprovalResolved, Timestamp: now, Data: map[string]any{"approval_id": rollbackApproval.ID, "confirmed": true}}}
	if _, _, _, err := commitRepo.CommitApprovalResume(ctx, rollback); err == nil {
		t.Fatal("重复事件主键应使 approval atomic commit 失败")
	}
	currentApproval, err := repo.GetApproval(ctx, rollbackApproval.ID)
	if err != nil || currentApproval.Status != agentruntime.ApprovalPending {
		t.Fatalf("rollback 后 approval 不应变更: approval=%#v err=%v", currentApproval, err)
	}
	currentInvocation, err := repo.GetInvocation(ctx, rollbackInvocation.ID)
	if err != nil || currentInvocation.Status != agentruntime.InvocationWaitingApproval {
		t.Fatalf("rollback 后 invocation 不应变更: invocation=%#v err=%v", currentInvocation, err)
	}
	currentToolCall, err := toolRepo.GetToolCall(ctx, rollbackToolCall.ID)
	if err != nil || currentToolCall.Status != agentruntime.ToolCallWaitingApproval {
		t.Fatalf("rollback 后 toolcall 不应变更: toolcall=%#v err=%v", currentToolCall, err)
	}
	if _, err := repo.(agentruntime.InvocationResumeRepository).GetInvocationResume(ctx, rollbackInvocation.ID); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("rollback 后不应留下 approval handoff: %v", err)
	}
	if _, err := repo.(agentruntime.InvocationResumeOutboxRepository).GetInvocationResumeOutbox(ctx, rollbackInvocation.ID); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("rollback 后不应留下 approval outbox: %v", err)
	}
	events, err = repo.ListEvents(ctx, rollbackInvocation.ID, 0, 10)
	if err != nil || len(events) != 1 || events[0].ID != duplicateEvent.ID {
		t.Fatalf("rollback 后只应保留原事件: events=%#v err=%v", events, err)
	}
}

func TestRuntimeRepositoryInvocationLeaseIsAtomic(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo := store.RuntimeRepository()
	leaseRepo, ok := repo.(agentruntime.InvocationLeaseRepository)
	if !ok {
		t.Fatal("SQLite runtime repository 未实现 InvocationLeaseRepository")
	}
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	invocation := agentruntime.Invocation{ID: "inv-lease-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	if acquired, err := leaseRepo.AcquireInvocationLease(context.Background(), invocation.ID, "worker-a", now, time.Minute); err != nil || !acquired {
		t.Fatalf("首次抢占租约失败: acquired=%v err=%v", acquired, err)
	}
	if acquired, err := leaseRepo.AcquireInvocationLease(context.Background(), invocation.ID, "worker-b", now.Add(30*time.Second), time.Minute); err != nil || acquired {
		t.Fatalf("SQLite 未过期租约不应被抢占: acquired=%v err=%v", acquired, err)
	}
	if ok, err := repo.TransitionInvocation(context.Background(), invocation.ID, agentruntime.InvocationQueued, agentruntime.InvocationRunning, ""); err != nil || !ok {
		t.Fatalf("进入 running 失败: ok=%v err=%v", ok, err)
	}
	if renewed, err := leaseRepo.RenewInvocationLease(context.Background(), invocation.ID, "worker-a", now.Add(30*time.Second), time.Minute); err != nil || !renewed {
		t.Fatalf("running 租约续租失败: renewed=%v err=%v", renewed, err)
	}
	if released, err := leaseRepo.ReleaseInvocationLease(context.Background(), invocation.ID, "worker-b"); err != nil || released {
		t.Fatalf("非持有者不应释放 SQLite 租约: released=%v err=%v", released, err)
	}
	if acquired, err := leaseRepo.AcquireInvocationLease(context.Background(), invocation.ID, "worker-b", now.Add(2*time.Minute), time.Minute); err != nil || !acquired {
		t.Fatalf("SQLite 过期租约应可接管: acquired=%v err=%v", acquired, err)
	}
	got, err := repo.GetInvocation(context.Background(), invocation.ID)
	if err != nil || got.LeaseOwner != "worker-b" || got.LeaseExpiresAt == nil {
		t.Fatalf("SQLite 接管后的租约状态错误: got=%#v err=%v", got, err)
	}
}

func TestRuntimeRepositoryPersistsToolCallAudit(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开 SQLite Store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	invocation := agentruntime.Invocation{ID: "invocation-tool-call", ConversationID: "conversation-tool-call", Status: agentruntime.InvocationQueued, CreatedAt: now, UpdatedAt: now}
	repo := store.RuntimeRepository()
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatalf("写入 Invocation 失败: %v", err)
	}
	want := agentruntime.ToolCall{
		ID: "tool-call-1", InvocationID: invocation.ID, ConversationID: invocation.ConversationID,
		ToolName: "workspace_request_patch", OriginalCallID: "original-1", ConfirmationCallID: "confirmation-1",
		OperationID: "operation-1", ApprovalID: "approval-1", Args: map[string]any{"path": "notes.txt", "new_text": "after"},
		Status: agentruntime.ToolCallCompleted, CreatedAt: now, UpdatedAt: now,
	}
	toolRepo := repo.(agentruntime.ToolCallRepository)
	if err := toolRepo.CreateToolCall(ctx, want); err != nil {
		t.Fatalf("写入 ToolCall 失败: %v", err)
	}
	got, err := toolRepo.GetToolCall(ctx, want.ID)
	if err != nil {
		t.Fatalf("读取 ToolCall 失败: %v", err)
	}
	if got.InvocationID != want.InvocationID || got.OriginalCallID != want.OriginalCallID || got.ConfirmationCallID != want.ConfirmationCallID || got.OperationID != want.OperationID || got.ApprovalID != want.ApprovalID || got.Status != want.Status {
		t.Fatalf("ToolCall 关联或状态未恢复: got=%#v want=%#v", got, want)
	}
	if got.Args["path"] != "notes.txt" || got.Args["new_text"] != "after" {
		t.Fatalf("ToolCall 参数未恢复: %#v", got.Args)
	}
	items, err := toolRepo.ListToolCalls(ctx, invocation.ID)
	if err != nil || len(items) != 1 || items[0].ID != want.ID {
		t.Fatalf("按 Invocation 查询 ToolCall 失败: items=%#v err=%v", items, err)
	}
}
