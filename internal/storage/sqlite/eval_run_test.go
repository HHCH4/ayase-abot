package sqlite

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

func TestRuntimeRepositoryPersistsEvalRunSnapshot(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	invocation := agentruntime.Invocation{ID: "inv-eval-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "conversation", Status: agentruntime.InvocationCompleted, CreatedAt: now, UpdatedAt: now}
	if err := store.RuntimeRepository().CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	repo, ok := store.RuntimeRepository().(agentruntime.EvalRunRepository)
	if !ok {
		t.Fatal("SQLite runtime repository 未实现 EvalRunRepository")
	}
	run := agentruntime.EvalRun{
		ID: "eval-sqlite", InvocationID: invocation.ID, CaseID: "case", CaseVersion: "1", Kind: "deterministic", Status: agentruntime.EvalRunCompleted,
		Passed: true, AssertionCount: 2, Definition: json.RawMessage(`{"id":"case","version":"1"}`), Result: json.RawMessage(`{"case_id":"case","case_version":"1","passed":true}`), CreatedAt: now, FinishedAt: &now,
	}
	if err := repo.CreateEvalRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetEvalRun(ctx, run.ID)
	if err != nil || got.CaseID != run.CaseID || string(got.Result) != string(run.Result) || got.FinishedAt == nil {
		t.Fatalf("SQLite EvalRun=%#v err=%v", got, err)
	}
	if err := repo.CreateEvalRun(ctx, run); err == nil {
		t.Fatal("重复 EvalRun ID 应被数据库拒绝")
	}
	items, err := repo.ListEvalRuns(ctx, invocation.ID)
	if err != nil || len(items) != 1 {
		t.Fatalf("EvalRun 列表=%#v err=%v", items, err)
	}
}
