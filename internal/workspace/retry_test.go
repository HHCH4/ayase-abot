package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func newRetryFixture(t *testing.T, workspaceID string) (*Service, *memoryRepository) {
	t.Helper()
	ctx := context.Background()
	repository := newMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(ctx, SaveRequest{Workspace: Workspace{ID: workspaceID, Name: workspaceID, Type: TypeLocal, RootPath: t.TempDir(), Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	return service, repository
}

func markOperationUnknown(t *testing.T, repository *memoryRepository, operation Operation) Operation {
	t.Helper()
	ctx := context.Background()
	unknown := operation
	unknown.Status = OperationUnknown
	unknown.Unknown = true
	unknown.CommandOutcome = string(CommandOutcomeUnknown)
	unknown.Error = commandRunUnknownAfterRestart
	unknown.UpdatedAt = unknown.UpdatedAt.Add(0)
	if err := repository.SaveOperation(ctx, unknown); err != nil {
		t.Fatal(err)
	}
	return unknown
}

func TestRetryOperationCreatesUnapprovedCopy(t *testing.T) {
	ctx := context.Background()
	service, repository := newRetryFixture(t, "retry-basic")
	source, err := service.CreateOperation(ctx, OperationRequest{
		WorkspaceID: "retry-basic", UserID: "user-1", ConversationID: "conversation-1",
		InvocationID: "inv-1", ToolCallID: "call-1",
		Type: OperationCommand, Command: "printf retry", CWD: ".", Timeout: 5,
		TTY: &TTYSpec{Enabled: true, Rows: 30, Cols: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	markOperationUnknown(t, repository, source)

	retried, err := service.RetryOperation(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.ID == source.ID {
		t.Fatal("重试必须创建新操作，不能复用源操作")
	}
	if retried.Status != OperationPrepared {
		t.Fatalf("重试操作必须等待人工批准: %s", retried.Status)
	}
	if retried.RetryOf != source.ID {
		t.Fatalf("重试操作必须记录来源: %+v", retried)
	}
	if retried.Command != source.Command || retried.CWD != source.CWD || retried.Timeout != source.Timeout {
		t.Fatalf("重试必须保留命令参数: %+v", retried)
	}
	if !EqualTTYSpec(retried.TTY, source.TTY) {
		t.Fatalf("重试必须保留 TTY spec: %+v", retried.TTY)
	}
	if retried.ConversationID != source.ConversationID {
		t.Fatalf("重试必须保留对话归属: %+v", retried)
	}
	// 重试是用户决策而不是 Runtime 恢复，因此不能挂到已结束的 Invocation 上。
	if retried.InvocationID != "" || retried.ToolCallID != "" {
		t.Fatalf("重试不能继承 Invocation/ToolCall: %+v", retried)
	}
	if retried.CommandRunID == "" || retried.CommandRunID == source.CommandRunID {
		t.Fatalf("重试必须拥有独立的命令检查点: source=%s retry=%s", source.CommandRunID, retried.CommandRunID)
	}
	run, err := service.GetCommandRun(ctx, retried.CommandRunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != CommandRunQueued {
		t.Fatalf("重试不得自动执行: %s", run.Status)
	}
}

func TestRetryOperationIsIdempotentWhilePending(t *testing.T) {
	ctx := context.Background()
	service, repository := newRetryFixture(t, "retry-idempotent")
	source, err := service.CreateOperation(ctx, OperationRequest{
		WorkspaceID: "retry-idempotent", UserID: "user-1", Type: OperationCommand, Command: "printf once", CWD: ".", Timeout: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	markOperationUnknown(t, repository, source)

	first, err := service.RetryOperation(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.RetryOperation(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("等待中的重试必须复用同一个操作: %s vs %s", first.ID, second.ID)
	}

	// 上次重试被拒绝后，用户可以重新发起一次。
	rejected := second
	rejected.Status = OperationRejected
	if err := repository.SaveOperation(ctx, rejected); err != nil {
		t.Fatal(err)
	}
	third, err := service.RetryOperation(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if third.ID == first.ID {
		t.Fatal("终态的重试不应阻止再次重试")
	}
}

func TestRetryOperationRejectsStatesThatNeedNoRetry(t *testing.T) {
	ctx := context.Background()
	service, repository := newRetryFixture(t, "retry-states")
	source, err := service.CreateOperation(ctx, OperationRequest{
		WorkspaceID: "retry-states", UserID: "user-1", Type: OperationCommand, Command: "printf done", CWD: ".", Timeout: 5,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, status := range []OperationStatus{OperationCompleted, OperationRejected, OperationCancelled, OperationExpired, OperationStale, OperationPrepared} {
		updated := source
		updated.Status = status
		if status == OperationCompleted {
			updated.Unknown = false
		}
		if err := repository.SaveOperation(ctx, updated); err != nil {
			t.Fatal(err)
		}
		if _, err := service.RetryOperation(ctx, source.ID); !errors.Is(err, ErrOperationState) {
			t.Fatalf("状态 %s 不应允许重试, got %v", status, err)
		}
	}

	// failed 是明确失败，允许人工重试。
	failed := source
	failed.Status = OperationFailed
	if err := repository.SaveOperation(ctx, failed); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RetryOperation(ctx, source.ID); err != nil {
		t.Fatalf("失败的操作必须允许人工重试: %v", err)
	}
}

func TestRetryOperationRejectsNonCommandAndMissing(t *testing.T) {
	ctx := context.Background()
	service, repository := newRetryFixture(t, "retry-types")
	writeOperation, err := service.CreateOperation(ctx, OperationRequest{
		WorkspaceID: "retry-types", UserID: "user-1", Type: OperationWriteFile, Path: "notes.txt", Content: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	failed := writeOperation
	failed.Status = OperationFailed
	if err := repository.SaveOperation(ctx, failed); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RetryOperation(ctx, writeOperation.ID); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("非命令操作必须拒绝重试, got %v", err)
	}
	if _, err := service.RetryOperation(ctx, "operation-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的操作必须返回 ErrNotFound, got %v", err)
	}
	if _, err := service.RetryOperation(ctx, "  "); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("空 ID 必须被拒绝, got %v", err)
	}
}

// TestRetryOperationEndToEndAfterRestart is the regression for the single-machine
// profile: a command that lost its control handle across a restart is closed as
// unknown, never replayed automatically, and can only continue through an
// explicit human retry that is approved again.
func TestRetryOperationEndToEndAfterRestart(t *testing.T) {
	ctx := context.Background()
	service, repository := newRetryFixture(t, "retry-e2e")
	operation, err := service.CreateOperation(ctx, OperationRequest{
		WorkspaceID: "retry-e2e", UserID: "user-1", Type: OperationCommand, Command: "printf recovered", CWD: ".", Timeout: 5,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 模拟“执行中进程失去控制句柄”：把检查点留在 running，交给启动恢复路径。
	run, err := service.GetCommandRun(ctx, operation.CommandRunID)
	if err != nil {
		t.Fatal(err)
	}
	running := run
	running.Status = CommandRunRunning
	running.Revision = run.Revision + 1
	if _, err := repository.UpdateCommandRun(ctx, running, run.Revision); err != nil {
		t.Fatal(err)
	}
	if err := service.ReconcileCommandRuns(ctx); err != nil {
		t.Fatal(err)
	}

	operations, err := service.ListOperations(ctx, "retry-e2e")
	if err != nil {
		t.Fatal(err)
	}
	var afterRestart Operation
	for _, item := range operations {
		if item.ID == operation.ID {
			afterRestart = item
		}
	}
	if afterRestart.Status != OperationUnknown || !afterRestart.Unknown {
		t.Fatalf("重启后必须保守收口为 unknown: %+v", afterRestart)
	}
	if !strings.Contains(afterRestart.Error, "禁止自动重跑") {
		t.Fatalf("unknown 必须说明不会自动重跑: %q", afterRestart.Error)
	}
	recovered, err := service.GetCommandRun(ctx, operation.CommandRunID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != CommandRunUnknown {
		t.Fatalf("命令检查点必须收口为 unknown: %s", recovered.Status)
	}

	// 恢复路径本身不得启动任何新进程：此时仍然只有源操作的那一个检查点。
	if len(repository.commandRuns) != 1 {
		t.Fatalf("未知副作用不得被自动重放: %d 个检查点", len(repository.commandRuns))
	}

	retried, err := service.RetryOperation(ctx, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Status != OperationPrepared {
		t.Fatalf("重试必须等待批准: %s", retried.Status)
	}
	// 人工批准后才真正执行，并产生独立于源操作的结果。
	approved, err := service.ApproveOperation(ctx, retried.ID)
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != OperationCompleted || !strings.Contains(approved.Result, "recovered") {
		t.Fatalf("批准后的重试必须成功执行: %+v", approved)
	}
	original, err := service.GetCommandRun(ctx, operation.CommandRunID)
	if err != nil {
		t.Fatal(err)
	}
	if original.Status != CommandRunUnknown {
		t.Fatalf("重试不得改写源操作的终态: %s", original.Status)
	}
}
