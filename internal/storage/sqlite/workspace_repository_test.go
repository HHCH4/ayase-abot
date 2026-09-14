package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"Abot/internal/artifact"
	"Abot/internal/workspace"
)

func TestWorkspaceCommandRunRoundTripAndCAS(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	repository := store.WorkspaceRepository()
	operation := workspace.Operation{ID: "operation-command-run", WorkspaceID: "workspace-command-run", Type: workspace.OperationCommand, Command: "printf hi", CWD: ".", Timeout: 5, Status: workspace.OperationPrepared, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := repository.SaveOperation(ctx, operation); err != nil {
		t.Fatal(err)
	}
	run := workspace.CommandRun{ID: "command-run-roundtrip", WorkspaceID: operation.WorkspaceID, InvocationID: "inv-command-run", OperationID: operation.ID, Executor: "local", Mode: "shell", Capabilities: workspace.DefaultCommandExecutionCapabilities(workspace.Workspace{Type: workspace.TypeLocal}), CommandPreview: "printf hi", CWD: ".", TimeoutMS: 5000, Status: workspace.CommandRunQueued, Revision: 1, QueuedAt: time.Now().UTC(), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	runRepo, ok := repository.(workspace.CommandRunRepository)
	if !ok {
		t.Fatal("SQLite workspace repository should implement CommandRunRepository")
	}
	if err := runRepo.CreateCommandRun(ctx, run); err != nil {
		t.Fatalf("创建 command run 失败: %v", err)
	}
	loaded, err := runRepo.GetCommandRun(ctx, run.ID)
	if err != nil || loaded.Status != workspace.CommandRunQueued || loaded.TimeoutMS != 5000 || loaded.Capabilities == nil || loaded.Capabilities.ProfileID != "host-process-l0" {
		t.Fatalf("读取 command run 不正确: %#v err=%v", loaded, err)
	}
	loaded.Status = workspace.CommandRunRunning
	loaded.Revision = 2
	started := time.Now().UTC()
	loaded.StartedAt = &started
	updated, err := runRepo.UpdateCommandRun(ctx, loaded, 1)
	if err != nil || updated.Revision != 2 || updated.Status != workspace.CommandRunRunning {
		t.Fatalf("CAS 更新 command run 失败: %#v err=%v", updated, err)
	}
	mutated := updated
	mutated.Capabilities = workspace.DefaultCommandExecutionCapabilities(workspace.Workspace{Type: workspace.TypeSSH})
	mutated.Revision = 3
	if _, err := runRepo.UpdateCommandRun(ctx, mutated, 2); !errors.Is(err, workspace.ErrCommandRunConflict) {
		t.Fatalf("admission 能力快照被改写时应返回冲突: %v", err)
	}
	if _, err := runRepo.UpdateCommandRun(ctx, loaded, 1); !errors.Is(err, workspace.ErrCommandRunConflict) {
		t.Fatalf("过期 revision 应返回 ErrCommandRunConflict: %v", err)
	}
	chunk := workspace.CommandOutputChunk{CommandRunID: run.ID, Sequence: 1, Stream: "stdout", Offset: 0, Data: "hi", ByteLength: 2, CapturedAt: time.Now().UTC()}
	outputRepo, ok := repository.(workspace.CommandOutputRepository)
	if !ok {
		t.Fatal("SQLite workspace repository should implement CommandOutputRepository")
	}
	if err := outputRepo.CreateCommandOutputChunk(ctx, chunk); err != nil {
		t.Fatalf("保存 command output chunk 失败: %v", err)
	}
	if err := outputRepo.CreateCommandOutputChunk(ctx, chunk); err != nil {
		t.Fatalf("重复保存相同 chunk 应幂等: %v", err)
	}
	items, err := outputRepo.ListCommandOutputChunks(ctx, run.ID, 0, 10)
	if err != nil || len(items) != 1 || items[0].Data != "hi" || items[0].ByteLength != 2 {
		t.Fatalf("读取 command output chunk 不正确: %#v err=%v", items, err)
	}
	finished := time.Now().UTC().Add(-2 * time.Hour)
	retainedUntil := time.Now().UTC().Add(-time.Hour)
	loaded.Status = workspace.CommandRunExited
	loaded.Outcome = workspace.CommandOutcomeSuccess
	loaded.ExitCode = func() *int { value := 0; return &value }()
	loaded.StoredBytes = 2
	loaded.OutputDigest = "digest-before-purge"
	loaded.OutputRetentionState = workspace.CommandOutputRetentionRetained
	loaded.OutputRetainedUntil = &retainedUntil
	loaded.FinishedAt = &finished
	loaded.UpdatedAt = finished
	loaded.Revision = 3
	if _, err := runRepo.UpdateCommandRun(ctx, loaded, 2); err != nil {
		t.Fatalf("保存终态保留元数据失败: %v", err)
	}
	loaded = func() workspace.CommandRun {
		value, getErr := runRepo.GetCommandRun(ctx, run.ID)
		if getErr != nil {
			t.Fatalf("读取终态失败: %v", getErr)
		}
		return value
	}()
	retentionRepo, ok := repository.(workspace.CommandOutputRetentionRepository)
	if !ok {
		t.Fatal("SQLite workspace repository should implement CommandOutputRetentionRepository")
	}
	purged := loaded
	purged.OutputRetentionState = workspace.CommandOutputRetentionPurged
	purged.OutputPurgedAt = func() *time.Time { value := time.Now().UTC(); return &value }()
	purged.UpdatedAt = time.Now().UTC()
	purged.Revision = loaded.Revision + 1
	if _, err := retentionRepo.PurgeCommandOutput(ctx, run.ID, loaded.Revision, purged); err != nil {
		t.Fatalf("原子清理 command output 失败: %v", err)
	}
	retained, err := runRepo.GetCommandRun(ctx, run.ID)
	if err != nil || retained.OutputRetentionState != workspace.CommandOutputRetentionPurged || retained.StoredBytes != 2 || retained.OutputDigest != "digest-before-purge" {
		t.Fatalf("清理后元数据不正确: %#v err=%v", retained, err)
	}
	items, err = outputRepo.ListCommandOutputChunks(ctx, run.ID, 0, 10)
	if err != nil || len(items) != 0 {
		t.Fatalf("原子清理后不应保留 output chunks: %#v err=%v", items, err)
	}
	runs, err := runRepo.ListCommandRuns(ctx, run.WorkspaceID)
	if err != nil || len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("按工作区读取 command run 不正确: %#v err=%v", runs, err)
	}
	if err := runRepo.DeleteCommandRun(ctx, run.ID); err != nil {
		t.Fatalf("删除 command run 失败: %v", err)
	}
	leftovers, err := outputRepo.ListCommandOutputChunks(ctx, run.ID, 0, 10)
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("删除 command run 后不应遗留输出 chunk: %#v err=%v", leftovers, err)
	}
}

func TestWorkspaceArtifactRefsRoundTrip(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	now := time.Now().UTC()
	repository := store.WorkspaceRepository()
	diffRef := &artifact.ArtifactRef{ID: "artifact-diff", Version: 2, Digest: "sha256:" + strings.Repeat("a", 64), Kind: artifact.KindDiff, MIMEType: "text/x-diff", Size: 4, Name: "diff.patch"}
	outputRef := &artifact.ArtifactRef{ID: "artifact-log", Version: 2, Digest: "sha256:" + strings.Repeat("b", 64), Kind: artifact.KindCommandLog, MIMEType: "text/plain", Size: 3, Name: "command.log"}
	operation := workspace.Operation{ID: "operation-artifact-refs", UserID: "user-1", WorkspaceID: "workspace-artifact-refs", InvocationID: "inv-artifact-refs", Type: workspace.OperationCommand, Diff: "diff", DiffArtifact: diffRef, OutputArtifact: outputRef, ArtifactError: "", Status: workspace.OperationPrepared, CreatedAt: now, UpdatedAt: now}
	if err := repository.SaveOperation(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	loadedOperation, err := repository.GetOperation(context.Background(), operation.ID)
	if err != nil || loadedOperation.UserID != operation.UserID || loadedOperation.DiffArtifact == nil || loadedOperation.DiffArtifact.ID != diffRef.ID || loadedOperation.OutputArtifact == nil || loadedOperation.OutputArtifact.ID != outputRef.ID {
		t.Fatalf("Operation Artifact refs 往返失败: %#v err=%v", loadedOperation, err)
	}
	run := workspace.CommandRun{ID: "command-run-artifact-ref", UserID: "user-1", WorkspaceID: operation.WorkspaceID, ConversationID: "conversation-1", InvocationID: operation.InvocationID, OperationID: operation.ID, Executor: "local", Mode: "shell", Capabilities: workspace.DefaultCommandExecutionCapabilities(workspace.Workspace{Type: workspace.TypeLocal}), OutputArtifact: outputRef, CommandPreview: "printf", CWD: ".", TimeoutMS: 5000, Status: workspace.CommandRunQueued, Revision: 1, QueuedAt: now, CreatedAt: now, UpdatedAt: now}
	runRepo, ok := repository.(workspace.CommandRunRepository)
	if !ok {
		t.Fatal("SQLite workspace repository should implement CommandRunRepository")
	}
	if err := runRepo.CreateCommandRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	loadedRun, err := runRepo.GetCommandRun(context.Background(), run.ID)
	if err != nil || loadedRun.UserID != run.UserID || loadedRun.OutputArtifact == nil || loadedRun.OutputArtifact.ID != outputRef.ID {
		t.Fatalf("CommandRun Artifact ref 往返失败: %#v err=%v", loadedRun, err)
	}
}

func TestMalformedCommandCapabilitiesFailClosed(t *testing.T) {
	legacy := unmarshalCommandExecutionCapabilities("", "local")
	if legacy == nil || legacy.ProfileID != "host-process-l0" {
		t.Fatalf("legacy local command 应使用可解释的保守 profile: %#v", legacy)
	}
	corrupt := unmarshalCommandExecutionCapabilities(`{"profile_id":"host-process-l0","isolation_level":"l0_host_process"}`, "local")
	if corrupt == nil || corrupt.ProfileID != "unknown-executor" || corrupt.IsolationLevel != "unknown" || corrupt.Network.State != "unknown" {
		t.Fatalf("损坏 snapshot 必须完全 fail-closed: %#v", corrupt)
	}
}

func TestWorkspaceCommandRunTTYSpecRoundTripAndCAS(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	repository := store.WorkspaceRepository()
	spec := &workspace.TTYSpec{Enabled: true, Term: "xterm-256color", Rows: 40, Cols: 120}
	now := time.Now().UTC()
	operation := workspace.Operation{ID: "operation-tty-roundtrip", WorkspaceID: "workspace-tty", Type: workspace.OperationCommand, Command: "read line", CWD: ".", Timeout: 5, TTY: spec, Status: workspace.OperationPrepared, CreatedAt: now, UpdatedAt: now}
	if err := repository.SaveOperation(ctx, operation); err != nil {
		t.Fatal(err)
	}
	loadedOperation, err := repository.GetOperation(ctx, operation.ID)
	if err != nil || loadedOperation.TTY == nil || loadedOperation.TTY.Rows != 40 || loadedOperation.TTY.Cols != 120 || loadedOperation.TTY.Term != spec.Term {
		t.Fatalf("Operation TTY spec 往返失败: %#v err=%v", loadedOperation, err)
	}
	capabilities := workspace.DefaultCommandExecutionCapabilities(workspace.Workspace{Type: workspace.TypeLocal})
	capabilities.PTY = workspace.ExecutionCapability{State: workspace.ExecutionCapabilitySupported, Detail: "test"}
	run := workspace.CommandRun{ID: "command-run-tty-roundtrip", WorkspaceID: operation.WorkspaceID, OperationID: operation.ID, Executor: "local", Mode: "pty", Capabilities: capabilities, CommandPreview: operation.Command, CWD: ".", TimeoutMS: 5000, TTY: spec, Status: workspace.CommandRunQueued, Revision: 1, QueuedAt: now, CreatedAt: now, UpdatedAt: now}
	runRepo, ok := repository.(workspace.CommandRunRepository)
	if !ok {
		t.Fatal("SQLite workspace repository should implement CommandRunRepository")
	}
	if err := runRepo.CreateCommandRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	loadedRun, err := runRepo.GetCommandRun(ctx, run.ID)
	if err != nil || loadedRun.TTY == nil || loadedRun.TTY.Rows != 40 || loadedRun.TTY.Cols != 120 || loadedRun.Capabilities.PTY.State != workspace.ExecutionCapabilitySupported {
		t.Fatalf("CommandRun TTY spec 往返失败: %#v err=%v", loadedRun, err)
	}
	loadedRun.Revision = 2
	loadedRun.Status = workspace.CommandRunRunning
	loadedRun.TTY.Rows = 41
	if _, err := runRepo.UpdateCommandRun(ctx, loadedRun, 1); !errors.Is(err, workspace.ErrCommandRunConflict) {
		t.Fatalf("TTY spec 修改应被 CAS 拒绝: %v", err)
	}
}
