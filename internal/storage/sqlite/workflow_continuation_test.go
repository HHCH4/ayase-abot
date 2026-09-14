package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

type sqliteWorkflowContinuationFixture struct {
	dataDir string
	source  agentruntime.Invocation
	plan    agentruntime.TaskPlan
}

func newSQLiteWorkflowContinuationFixture(t *testing.T) (*Store, sqliteWorkflowContinuationFixture) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	source := agentruntime.Invocation{
		ID: "source-sqlite-continuation", UserID: "user", BotID: "bot", ConversationID: "conversation-sqlite", WorkspaceID: "workspace", TargetPath: "/workspace/project", SessionID: "session-sqlite", ProviderID: "provider", ModelID: "model", ConfigSnapshot: `{"version":1,"provider":"provider"}`, Message: "私有原始正文", Status: agentruntime.InvocationFailed,
		Error: "runtime_interrupted: 服务重启时运行中的 invocation 没有可安全恢复的 checkpoint", CreatedAt: now, UpdatedAt: now,
	}
	repo := store.RuntimeRepository()
	if err := repo.CreateInvocation(ctx, source); err != nil {
		store.Close()
		t.Fatal(err)
	}
	plan := agentruntime.TaskPlan{
		ID: "plan-" + source.ID, InvocationID: source.ID, Revision: 4, Status: agentruntime.PlanBlocked, Blocker: "runtime_interrupted: 无法确认活动步骤副作用", CreatedAt: now, UpdatedAt: now,
		Steps: []agentruntime.PlanStep{
			{ID: "inspect", Title: "检查", Status: agentruntime.PlanStepCompleted, Evidence: []agentruntime.EvidenceRef{{Kind: "event", Ref: "sqlite-inspect"}}, UpdatedAt: now},
			{ID: "edit", Title: "修改", Status: agentruntime.PlanStepBlocked, DependsOn: []string{"inspect"}, Blocker: "runtime_interrupted: 无法确认活动步骤副作用", RepairAttempts: 1, UpdatedAt: now},
			{ID: "verify", Title: "验证", Status: agentruntime.PlanStepPending, DependsOn: []string{"edit"}, UpdatedAt: now},
		},
	}
	if err := repo.(interface {
		CreateTaskPlan(context.Context, agentruntime.TaskPlan) error
	}).CreateTaskPlan(ctx, plan); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, agentruntime.AgentEvent{ID: "sqlite-recovery-plan", InvocationID: source.ID, Type: agentruntime.EventPlanUpdated, Timestamp: now, Data: map[string]any{"reason": "runtime_recovery"}}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, agentruntime.AgentEvent{ID: "sqlite-recovery-failed", InvocationID: source.ID, Type: agentruntime.EventInvocationFailed, Timestamp: now, Data: map[string]any{"reason": "runtime_recovery", "error": source.Error}}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	storedPlan, err := repo.(interface {
		GetTaskPlan(context.Context, string) (agentruntime.TaskPlan, error)
	}).GetTaskPlan(ctx, source.ID)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	return store, sqliteWorkflowContinuationFixture{dataDir: dataDir, source: source, plan: storedPlan}
}

func sqliteContinuationChildID(sourceID string, revision int64, decision agentruntime.WorkflowContinuationDecision, stepID, key string) string {
	value := sourceID + "\x00" + formatInt64(revision) + "\x00" + string(decision) + "\x00" + stepID + "\x00" + key
	digest := sha256.Sum256([]byte(value))
	return "invocation-continuation-" + hex.EncodeToString(digest[:])[:32]
}

func formatInt64(value int64) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		index--
		digits[index] = '-'
	}
	return string(digits[index:])
}

func sqliteContinuationCommit(t *testing.T, fixture sqliteWorkflowContinuationFixture, key string) agentruntime.WorkflowContinuationCommit {
	t.Helper()
	decision := agentruntime.WorkflowContinuationRetryStep
	stepID := "edit"
	childID := sqliteContinuationChildID(fixture.source.ID, fixture.plan.Revision, decision, stepID, key)
	now := time.Now().UTC().Truncate(time.Microsecond)
	steps := append([]agentruntime.PlanStep(nil), fixture.plan.Steps...)
	for index := range steps {
		if steps[index].ID == stepID {
			steps[index].Status = agentruntime.PlanStepPending
			steps[index].Blocker = ""
		}
	}
	childPlan := agentruntime.TaskPlan{ID: "plan-" + childID, InvocationID: childID, Revision: 1, Status: agentruntime.PlanPending, Steps: steps, CreatedAt: now, UpdatedAt: now}
	if _, err := childPlan.Normalize(now); err != nil {
		t.Fatal(err)
	}
	child := agentruntime.Invocation{ID: childID, UserID: fixture.source.UserID, BotID: fixture.source.BotID, ConversationID: fixture.source.ConversationID, WorkspaceID: fixture.source.WorkspaceID, TargetPath: fixture.source.TargetPath, SessionID: fixture.source.SessionID, ProviderID: fixture.source.ProviderID, ModelID: fixture.source.ModelID, ConfigSnapshot: fixture.source.ConfigSnapshot, Message: "继续处理剩余步骤", Status: agentruntime.InvocationQueued, ParentInvocationID: fixture.source.ID, ContinuationDecision: decision, ContinuationSourcePlanRevision: fixture.plan.Revision, IdempotencyKey: "workflow-continuation:" + childID, CreatedAt: now, UpdatedAt: now}
	return agentruntime.WorkflowContinuationCommit{
		SourceInvocationID: fixture.source.ID, SourcePlanRevision: fixture.plan.Revision, Decision: decision, StepID: stepID, IdempotencyKey: key,
		Child: child, ChildPlan: childPlan,
		SourceEvent: agentruntime.AgentEvent{ID: "workflow-continuation-" + childID + "-requested", InvocationID: fixture.source.ID, Type: agentruntime.EventWorkflowContinuationRequested, Timestamp: now, Data: map[string]any{"child_invocation_id": childID, "decision": string(decision), "step_id": stepID, "source_plan_revision": fixture.plan.Revision}},
		ChildEvents: []agentruntime.AgentEvent{{ID: "workflow-continuation-" + childID + "-queued", InvocationID: childID, Type: agentruntime.EventInvocationQueued, Timestamp: now, Data: map[string]any{"conversation_id": child.ConversationID, "parent_invocation_id": fixture.source.ID, "continuation_decision": string(decision), "step_id": stepID}}},
	}
}

func TestSQLiteWorkflowContinuationIsAtomicDurableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store, fixture := newSQLiteWorkflowContinuationFixture(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(fixture.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository().(agentruntime.WorkflowContinuationCommitRepository)
	commit := sqliteContinuationCommit(t, fixture, "sqlite-retry")
	child, plan, events, err := repo.CommitWorkflowContinuation(ctx, commit)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if child.ID != commit.Child.ID || plan.ID != commit.ChildPlan.ID || len(events) != 2 {
		store.Close()
		t.Fatalf("SQLite continuation 首次提交错误: child=%#v plan=%#v events=%#v", child, plan, events)
	}
	repeated, repeatedPlan, repeatedEvents, err := repo.CommitWorkflowContinuation(ctx, commit)
	if err != nil || repeated.ID != child.ID || repeatedPlan.ID != plan.ID || len(repeatedEvents) != 0 {
		store.Close()
		t.Fatalf("SQLite continuation 重复提交应幂等: child=%#v plan=%#v events=%#v err=%v", repeated, repeatedPlan, repeatedEvents, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(fixture.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	childRepo := store.RuntimeRepository()
	storedChild, err := childRepo.GetInvocation(ctx, child.ID)
	if err != nil || storedChild.ParentInvocationID != fixture.source.ID || storedChild.ContinuationDecision != agentruntime.WorkflowContinuationRetryStep || storedChild.Message == "私有原始正文" {
		t.Fatalf("SQLite 重启后 child metadata 错误: %#v err=%v", storedChild, err)
	}
	storedPlan, err := childRepo.(interface {
		GetTaskPlan(context.Context, string) (agentruntime.TaskPlan, error)
	}).GetTaskPlan(ctx, child.ID)
	if err != nil || storedPlan.Steps[1].Status != agentruntime.PlanStepPending || storedPlan.Steps[1].Blocker != "" {
		t.Fatalf("SQLite 重启后 child plan 错误: %#v err=%v", storedPlan, err)
	}
	storedEvents, err := childRepo.ListEvents(ctx, child.ID, 0, 10)
	if err != nil || len(storedEvents) != 1 || storedEvents[0].Type != agentruntime.EventInvocationQueued {
		t.Fatalf("SQLite 重启后 child event 错误: %#v err=%v", storedEvents, err)
	}
	outbox := childRepo.(agentruntime.RuntimeEventOutboxRepository)
	queued, err := outbox.ListRuntimeEventOutbox(ctx, child.ID, agentruntime.RuntimeEventOutboxQueued, 10)
	if err != nil || len(queued) != 1 || queued[0].EventID != storedEvents[0].ID {
		t.Fatalf("SQLite child event outbox 不完整: %#v err=%v", queued, err)
	}
}

func TestSQLiteWorkflowContinuationRejectsForgedCommitAndConcurrentDuplicate(t *testing.T) {
	ctx := context.Background()
	store, fixture := newSQLiteWorkflowContinuationFixture(t)
	defer store.Close()
	repo := store.RuntimeRepository().(agentruntime.WorkflowContinuationCommitRepository)
	forged := sqliteContinuationCommit(t, fixture, "forged")
	forged.Child.Message = "contains api_key=secret"
	if _, _, _, err := repo.CommitWorkflowContinuation(ctx, forged); !errors.Is(err, agentruntime.ErrWorkflowContinuationConflict) {
		t.Fatalf("含敏感正文的 forged child 应拒绝且不落库: %v", err)
	}
	if _, err := store.RuntimeRepository().GetInvocation(ctx, forged.Child.ID); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("forged commit 不得创建 child: %v", err)
	}
	commit := sqliteContinuationCommit(t, fixture, "concurrent")
	const callers = 6
	var wait sync.WaitGroup
	errs := make(chan error, callers)
	for index := 0; index < callers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, _, _, err := repo.CommitWorkflowContinuation(ctx, commit)
			if err != nil {
				errs <- err
			}
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发同一 SQLite continuation 不应失败: %v", err)
	}
	items, err := store.RuntimeRepository().ListInvocations(ctx, fixture.source.UserID, nil)
	if err != nil || len(items) != 2 {
		t.Fatalf("并发 SQLite continuation 必须只有 source+one child: %#v err=%v", items, err)
	}
}
