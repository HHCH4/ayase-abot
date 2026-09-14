package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestMemoryRepositoryPersistsImmutableEvalRun(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-eval-memory", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationCompleted, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(`{"case_id":"case","case_version":"1","passed":true}`)
	run := EvalRun{ID: "eval-memory", InvocationID: invocation.ID, CaseID: "case", CaseVersion: "1", Kind: "deterministic", Status: EvalRunCompleted, Passed: true, AssertionCount: 1, Result: result, Definition: json.RawMessage(`{"id":"case","version":"1"}`), CreatedAt: now, FinishedAt: &now}
	if err := repo.CreateEvalRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	result[2] = 'X'
	got, err := repo.GetEvalRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Result) != `{"case_id":"case","case_version":"1","passed":true}` {
		t.Fatalf("EvalRun JSON 应独立复制: %#v", got)
	}
	if err := repo.CreateEvalRun(context.Background(), run); err != ErrConflict {
		t.Fatalf("同 ID EvalRun 应不可覆盖，实际=%v", err)
	}
	items, err := repo.ListEvalRuns(context.Background(), invocation.ID)
	if err != nil || len(items) != 1 || items[0].ID != run.ID {
		t.Fatalf("EvalRun 列表=%#v err=%v", items, err)
	}
}

func TestCoordinatorEvalInputReadsDurableProjection(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-eval-input", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: InvocationCompleted, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(context.Background(), AgentEvent{ID: "event-start", InvocationID: invocation.ID, Type: EventInvocationStarted}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(context.Background(), AgentEvent{ID: "event-finished", InvocationID: invocation.ID, Type: EventInvocationCompleted}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateApproval(context.Background(), Approval{ID: "approval", InvocationID: invocation.ID, ToolName: "read", OriginalCallID: "call", ConfirmationCallID: "confirm", Status: ApprovalApproved}); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo}
	input, err := coordinator.GetEvalInput(context.Background(), invocation.ID)
	if err != nil || input.Invocation.ID != invocation.ID || len(input.Events) != 2 || len(input.Approvals) != 1 {
		t.Fatalf("EvalInput=%#v err=%v", input, err)
	}
	if _, err := coordinator.GetEvalInput(context.Background(), "missing"); err != ErrNotFound {
		t.Fatalf("缺失 invocation 应返回 ErrNotFound，实际=%v", err)
	}
}
