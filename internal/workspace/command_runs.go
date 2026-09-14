package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"Abot/internal/artifact"
)

const commandRunUnknownAfterRestart = "进程控制句柄在服务重启后丢失，禁止自动重跑"

// GetCommandRun returns the durable checkpoint for one command attempt. The
// repository is optional so older Workspace embedders continue to work.
func (s *Service) GetCommandRun(ctx context.Context, id string) (CommandRun, error) {
	repository, ok := s.repository.(CommandRunRepository)
	if !ok {
		return CommandRun{}, ErrUnsupported
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return CommandRun{}, fmt.Errorf("%w: command run id 不能为空", ErrInvalidRequest)
	}
	return repository.GetCommandRun(ctx, id)
}

// ListCommandRuns lists recent command checkpoints for a workspace. An empty
// workspace ID returns all rows and is intended only for startup recovery.
func (s *Service) ListCommandRuns(ctx context.Context, workspaceID string) ([]CommandRun, error) {
	repository, ok := s.repository.(CommandRunRepository)
	if !ok {
		return nil, ErrUnsupported
	}
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID != "" {
		if _, err := s.Get(ctx, workspaceID); err != nil {
			return nil, err
		}
	}
	return repository.ListCommandRuns(ctx, workspaceID)
}

// PurgeExpiredCommandOutput removes only replayable output chunks whose
// immutable retention deadline has elapsed. CommandRun metadata remains
// queryable and keeps the bounded result summary, byte counters, exit code and
// digest. Cleanup is safe to call repeatedly and from multiple workers.
func (s *Service) PurgeExpiredCommandOutput(ctx context.Context, now time.Time) (CommandOutputPurgeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	repository, ok := commandRunRepository(s)
	if !ok {
		return CommandOutputPurgeResult{}, ErrUnsupported
	}
	retentionRepository, ok := s.repository.(CommandOutputRetentionRepository)
	if !ok {
		// Older embedders can continue to run; they simply do not opt into
		// destructive output cleanup until their repository implements the
		// atomic extension.
		return CommandOutputPurgeResult{}, nil
	}
	runs, err := repository.ListCommandRuns(ctx, "")
	if err != nil {
		return CommandOutputPurgeResult{}, err
	}
	policy := s.commandOutputRetentionPolicy()
	result := CommandOutputPurgeResult{Scanned: len(runs)}
	for _, run := range runs {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if !run.Status.Terminal() || run.OutputRetentionState == CommandOutputRetentionPurged {
			continue
		}
		deadline := run.OutputRetainedUntil
		if deadline == nil && run.FinishedAt != nil && policy.MaxAge > 0 {
			// Legacy terminal rows have no persisted deadline. Derive one from
			// their immutable finish time for this cleanup pass; the purge itself
			// still uses the repository CAS, so two workers cannot both delete it.
			derived := run.FinishedAt.Add(policy.MaxAge)
			deadline = &derived
		}
		if deadline == nil || deadline.After(now) {
			continue
		}
		updated := run
		updated.OutputRetentionState = CommandOutputRetentionPurged
		updated.OutputRetainedUntil = cloneTimePointer(deadline)
		updated.OutputPurgedAt = &now
		updated.UpdatedAt = now
		updated.Revision = run.Revision + 1
		_, purgeErr := retentionRepository.PurgeCommandOutput(ctx, run.ID, run.Revision, updated)
		if purgeErr != nil {
			if errors.Is(purgeErr, ErrCommandRunConflict) {
				// Another worker may have purged or advanced this checkpoint. It
				// is safe to continue; the next pass can inspect the winner.
				continue
			}
			return result, purgeErr
		}
		result.Purged++
		result.Bytes += run.StoredBytes
		// cleanup intentionally emits no lifecycle event, avoiding a duplicate
		// command.completed notification for a past process.
	}
	return result, nil
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copyValue := value.UTC()
	return &copyValue
}

// ListCommandOutput returns a bounded replay page for a durable command
// attempt. The cursor is the CommandRun sequence, not a timestamp.
func (s *Service) ListCommandOutput(ctx context.Context, commandRunID string, after uint64, limit int) (CommandOutputPage, error) {
	commandRunID = strings.TrimSpace(commandRunID)
	if commandRunID == "" {
		return CommandOutputPage{}, fmt.Errorf("%w: command run id 不能为空", ErrInvalidRequest)
	}
	if limit <= 0 {
		limit = maxCommandOutputPageLimit
	}
	if limit > maxCommandOutputPageLimit {
		return CommandOutputPage{}, fmt.Errorf("%w: 单页最多返回 %d 个输出 chunk", ErrInvalidRequest, maxCommandOutputPageLimit)
	}
	run, err := s.GetCommandRun(ctx, commandRunID)
	if err != nil {
		return CommandOutputPage{}, err
	}
	repository, ok := s.repository.(CommandOutputRepository)
	if !ok {
		return CommandOutputPage{}, ErrUnsupported
	}
	items, err := repository.ListCommandOutputChunks(ctx, commandRunID, after, limit+1)
	if err != nil {
		return CommandOutputPage{}, err
	}
	retentionState := run.OutputRetentionState
	if retentionState == "" && run.Status.Terminal() {
		// Rows written before retention metadata was introduced are treated as
		// retained until an explicit cleanup pass migrates their state.
		retentionState = CommandOutputRetentionRetained
	}
	page := CommandOutputPage{Chunks: items, NextAfter: after, Truncated: run.OutputTruncated, RetentionState: retentionState}
	if retentionState == CommandOutputRetentionPurged {
		page.Chunks = nil
		page.NextAfter = after
		page.HasMore = false
		return page, nil
	}
	if len(items) > limit {
		page.HasMore = true
		page.Chunks = items[:limit]
	}
	if len(page.Chunks) > 0 {
		page.NextAfter = page.Chunks[len(page.Chunks)-1].Sequence
	}
	return page, nil
}

// DownloadCommandOutput writes every durably captured chunk for a command in
// sequence order. Text is a merged stdout/stderr stream; NDJSON preserves the
// chunk metadata and is therefore the lossless interchange format. The
// download is deliberately bounded and never retries or re-executes a command.
func (s *Service) DownloadCommandOutput(ctx context.Context, commandRunID string, format CommandOutputDownloadFormat, writer io.Writer) (CommandOutputDownloadResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	commandRunID = strings.TrimSpace(commandRunID)
	if commandRunID == "" {
		return CommandOutputDownloadResult{}, fmt.Errorf("%w: command run id 不能为空", ErrInvalidRequest)
	}
	if writer == nil {
		return CommandOutputDownloadResult{}, fmt.Errorf("%w: 下载目标不能为空", ErrInvalidRequest)
	}
	format = CommandOutputDownloadFormat(strings.ToLower(strings.TrimSpace(string(format))))
	if format == "" {
		format = CommandOutputDownloadNDJSON
	}
	if format != CommandOutputDownloadText && format != CommandOutputDownloadNDJSON {
		return CommandOutputDownloadResult{}, fmt.Errorf("%w: 不支持的命令输出下载格式 %q", ErrInvalidRequest, format)
	}
	run, err := s.GetCommandRun(ctx, commandRunID)
	if err != nil {
		return CommandOutputDownloadResult{}, err
	}
	if run.OutputRetentionState == CommandOutputRetentionPurged {
		return CommandOutputDownloadResult{Truncated: run.OutputTruncated}, ErrCommandOutputExpired
	}
	if _, ok := s.repository.(CommandOutputRepository); !ok {
		return CommandOutputDownloadResult{}, ErrUnsupported
	}

	result := CommandOutputDownloadResult{Truncated: run.OutputTruncated}
	var after uint64
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		page, pageErr := s.ListCommandOutput(ctx, commandRunID, after, maxCommandOutputPageLimit)
		if pageErr != nil {
			return result, pageErr
		}
		if page.Truncated {
			result.Truncated = true
		}
		previous := after
		for _, chunk := range page.Chunks {
			if err := chunk.Validate(); err != nil {
				return result, fmt.Errorf("%w: 输出 chunk %d 无效: %v", ErrCommandRunConflict, chunk.Sequence, err)
			}
			if chunk.CommandRunID != commandRunID || chunk.Sequence <= previous {
				return result, fmt.Errorf("%w: 输出 chunk sequence 不连续", ErrCommandRunConflict)
			}
			previous = chunk.Sequence
			if chunk.Truncated {
				result.Truncated = true
			}
			var data []byte
			if format == CommandOutputDownloadText {
				data = []byte(chunk.Data)
			} else {
				data, err = json.Marshal(chunk)
				if err != nil {
					return result, err
				}
				data = append(data, '\n')
			}
			if int64(len(data)) > maxCommandOutputDownloadBytes-result.Bytes {
				return result, ErrCommandOutputDownloadTooLarge
			}
			if err := writeCommandOutputBytes(writer, data); err != nil {
				return result, err
			}
			result.Bytes += int64(len(data))
			result.Chunks++
		}
		if !page.HasMore {
			return result, nil
		}
		if len(page.Chunks) == 0 || page.NextAfter <= after {
			return result, fmt.Errorf("%w: 输出分页游标未前进", ErrCommandRunConflict)
		}
		after = page.NextAfter
	}
}

func writeCommandOutputBytes(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if written < 0 || written > len(data) {
			return io.ErrShortWrite
		}
		if written == 0 && err == nil {
			return io.ErrShortWrite
		}
		data = data[written:]
		if err != nil {
			return err
		}
	}
	return nil
}

type commandChunkCollector struct {
	mu      sync.Mutex
	chunks  []CommandOutputChunk
	dropped bool
}

func (c *commandChunkCollector) add(chunk CommandChunk) {
	if c == nil || strings.TrimSpace(chunk.CommandRunID) == "" || chunk.Sequence == 0 || chunk.Data == "" {
		return
	}
	item := CommandOutputChunk{
		CommandRunID: chunk.CommandRunID,
		Sequence:     chunk.Sequence,
		Stream:       chunk.Stream,
		Offset:       chunk.Offset,
		Data:         chunk.Data,
		ByteLength:   int64(len([]byte(chunk.Data))),
		CapturedAt:   chunk.Timestamp,
		Truncated:    chunk.Truncated,
	}
	if item.CapturedAt.IsZero() {
		item.CapturedAt = time.Now().UTC()
	}
	if err := item.Validate(); err != nil {
		c.mu.Lock()
		c.dropped = true
		c.mu.Unlock()
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// The transport's bounded output is far below this guard. It protects the
	// command process from an accidentally unbounded custom observer.
	if len(c.chunks) >= 4096 {
		c.dropped = true
		return
	}
	c.chunks = append(c.chunks, item)
}

func (c *commandChunkCollector) snapshot() ([]CommandOutputChunk, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	items := append([]CommandOutputChunk(nil), c.chunks...)
	return items, c.dropped
}

func (s *Service) persistCommandOutputChunks(ctx context.Context, chunks []CommandOutputChunk) error {
	repository, ok := s.repository.(CommandOutputRepository)
	if !ok {
		return nil
	}
	for _, chunk := range chunks {
		if err := repository.CreateCommandOutputChunk(ctx, chunk); err != nil {
			return err
		}
	}
	return nil
}

// projectCommandOutputArtifact stores the bounded, observed command stream as
// a durable Artifact when an application installs an ArtifactWriter. The
// process outcome is already known at this point; projection failures are
// recorded as metadata and never trigger a command retry.
func (s *Service) projectCommandOutputArtifact(ctx context.Context, run CommandRun, result CommandResult, chunks []CommandOutputChunk) CommandRun {
	writer := s.artifactWriterFunc()
	if writer == nil || strings.TrimSpace(run.UserID) == "" || run.OutputArtifact != nil {
		return run
	}
	data := commandOutputArtifactBytes(result, chunks)
	producerID := run.ID
	item, err := writer(ctx, artifact.PutRequest{
		UserID: run.UserID, ConversationID: run.ConversationID, InvocationID: run.InvocationID,
		ProducerType: "command_log", ProducerID: producerID, Kind: artifact.KindCommandLog,
		Name: "command-" + run.ID + ".log", MIMEType: "text/plain",
		Metadata: map[string]any{"command_run_id": run.ID, "output_digest": digestText(string(data)), "output_truncated": result.Truncated || result.Unknown},
	}, bytes.NewReader(data))
	if err != nil {
		run.ArtifactError = "命令日志 Artifact 投影失败"
		return run
	}
	if item.Status != artifact.StatusReady {
		run.ArtifactError = "命令日志 Artifact 未达到可读状态"
		return run
	}
	ref := item.Ref()
	run.OutputArtifact = &ref
	return run
}

func commandOutputArtifactBytes(result CommandResult, chunks []CommandOutputChunk) []byte {
	if len(chunks) == 0 {
		return []byte(result.Output)
	}
	ordered := append([]CommandOutputChunk(nil), chunks...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Sequence < ordered[j].Sequence })
	var buffer bytes.Buffer
	for _, chunk := range ordered {
		_, _ = buffer.WriteString(chunk.Data)
		if int64(buffer.Len()) >= maxCommandOutputDownloadBytes {
			break
		}
	}
	data := buffer.Bytes()
	if int64(len(data)) > maxCommandOutputDownloadBytes {
		return append([]byte(nil), data[:maxCommandOutputDownloadBytes]...)
	}
	return append([]byte(nil), data...)
}

func (s *Service) projectOperationDiffArtifact(ctx context.Context, operation Operation) Operation {
	writer := s.artifactWriterFunc()
	if writer == nil || strings.TrimSpace(operation.UserID) == "" || strings.TrimSpace(operation.Diff) == "" || operation.DiffArtifact != nil {
		return operation
	}
	item, err := writer(ctx, artifact.PutRequest{
		UserID: operation.UserID, ConversationID: operation.ConversationID, InvocationID: operation.InvocationID,
		ProducerType: "workspace_diff", ProducerID: operation.ID, Kind: artifact.KindDiff,
		Name: "diff-" + operation.ID + ".patch", MIMEType: "text/x-diff",
		Metadata: map[string]any{"operation_id": operation.ID, "path": operation.Path},
	}, strings.NewReader(operation.Diff))
	if err != nil {
		operation.ArtifactError = "diff Artifact 投影失败"
		return operation
	}
	if item.Status != artifact.StatusReady {
		operation.ArtifactError = "diff Artifact 未达到可读状态"
		return operation
	}
	ref := item.Ref()
	operation.DiffArtifact = &ref
	return operation
}

func commandRunRepository(s *Service) (CommandRunRepository, bool) {
	if s == nil || s.repository == nil {
		return nil, false
	}
	repository, ok := s.repository.(CommandRunRepository)
	return repository, ok
}

func (s *Service) registerCommandRunCancel(id string, cancel context.CancelFunc) {
	if s == nil || cancel == nil {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	s.commandRunMu.Lock()
	if s.commandRunCancels == nil {
		s.commandRunCancels = make(map[string]context.CancelFunc)
	}
	s.commandRunCancels[id] = cancel
	s.commandRunMu.Unlock()
}

func (s *Service) unregisterCommandRunCancel(id string) {
	if s == nil {
		return
	}
	s.commandRunMu.Lock()
	delete(s.commandRunCancels, strings.TrimSpace(id))
	s.commandRunMu.Unlock()
}

func (s *Service) commandRunCancel(id string) context.CancelFunc {
	if s == nil {
		return nil
	}
	s.commandRunMu.RLock()
	cancel := s.commandRunCancels[strings.TrimSpace(id)]
	s.commandRunMu.RUnlock()
	return cancel
}

const commandRunCancelWait = 5 * time.Second

// CancelCommandRun requests cancellation of one command attempt without
// relying on the caller's HTTP/stream context. A live local process receives
// the same process-group cancellation as Invocation cancellation; if the
// control handle is missing or does not reach a terminal checkpoint in the
// bounded wait, the attempt is marked unknown and is never replayed.
func (s *Service) CancelCommandRun(ctx context.Context, id, reason string) (CommandRun, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return CommandRun{}, fmt.Errorf("%w: command run id 不能为空", ErrInvalidRequest)
	}
	run, err := s.GetCommandRun(ctx, id)
	if err != nil {
		return CommandRun{}, err
	}
	if run.Status.Terminal() {
		return run, nil
	}
	reason = truncateText(strings.TrimSpace(reason), 4096)
	if reason == "" {
		reason = "用户请求取消命令"
	}

	// A queued run has no process context yet. Serialize this branch with
	// approval so a racing approval cannot turn the same revision into a
	// starting attempt after cancellation was committed.
	if run.Status == CommandRunQueued {
		s.operationMu.Lock()
		current, getErr := s.GetCommandRun(context.WithoutCancel(ctx), id)
		if getErr != nil {
			s.operationMu.Unlock()
			return CommandRun{}, getErr
		}
		if current.Status.Terminal() {
			s.operationMu.Unlock()
			return current, nil
		}
		if current.Status == CommandRunQueued {
			updated, transitionErr := s.transitionCommandRun(context.WithoutCancel(ctx), current, CommandRunCancelled, CommandOutcomeCancelled, CommandResult{ExitCode: -1}, errors.New(reason))
			if transitionErr == nil {
				transitionErr = s.projectCommandRunOperationLocked(context.WithoutCancel(ctx), updated, OperationCancelled, CommandOutcomeCancelled, reason)
			}
			s.operationMu.Unlock()
			return updated, transitionErr
		}
		run = current
		s.operationMu.Unlock()
	}

	if run.Status.Terminal() {
		return run, nil
	}
	if cancel := s.commandRunCancel(id); cancel != nil {
		cancel()
		return s.waitForCommandRunTerminal(ctx, id, reason)
	}

	// A starting/running row without a local cancel handle means this process
	// cannot be proven controllable (for example after a worker restart).
	updated, transitionErr := s.markCommandRunUnknown(context.WithoutCancel(ctx), run, reason+"：进程控制句柄不可用")
	if transitionErr != nil {
		if errors.Is(transitionErr, ErrCommandRunConflict) {
			if latest, latestErr := s.GetCommandRun(context.WithoutCancel(ctx), id); latestErr == nil && latest.Status.Terminal() {
				return latest, nil
			}
		}
		return CommandRun{}, transitionErr
	}
	s.operationMu.Lock()
	operationErr := s.projectCommandRunOperationLocked(context.WithoutCancel(ctx), updated, OperationUnknown, CommandOutcomeUnknown, reason+"：进程控制句柄不可用")
	s.operationMu.Unlock()
	return updated, operationErr
}

func (s *Service) waitForCommandRunTerminal(ctx context.Context, id, reason string) (CommandRun, error) {
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commandRunCancelWait)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		run, err := s.GetCommandRun(waitCtx, id)
		if err != nil {
			return CommandRun{}, err
		}
		if run.Status.Terminal() {
			return run, nil
		}
		select {
		case <-waitCtx.Done():
			latest, latestErr := s.GetCommandRun(context.WithoutCancel(ctx), id)
			if latestErr != nil {
				return CommandRun{}, latestErr
			}
			if latest.Status.Terminal() {
				return latest, nil
			}
			updated, transitionErr := s.markCommandRunUnknown(context.WithoutCancel(ctx), latest, reason+"：取消未在限定时间内确认")
			if transitionErr != nil {
				if errors.Is(transitionErr, ErrCommandRunConflict) {
					if current, currentErr := s.GetCommandRun(context.WithoutCancel(ctx), id); currentErr == nil && current.Status.Terminal() {
						return current, nil
					}
				}
				return CommandRun{}, transitionErr
			}
			s.operationMu.Lock()
			operationErr := s.projectCommandRunOperationLocked(context.WithoutCancel(ctx), updated, OperationUnknown, CommandOutcomeUnknown, reason+"：取消未在限定时间内确认")
			s.operationMu.Unlock()
			return updated, operationErr
		case <-ticker.C:
		}
	}
}

// projectCommandRunOperationLocked keeps the Operation projection aligned
// with an out-of-band command cancellation. The caller must hold operationMu.
func (s *Service) projectCommandRunOperationLocked(ctx context.Context, run CommandRun, status OperationStatus, outcome CommandRunOutcome, reason string) error {
	operation, err := s.repository.GetOperation(ctx, run.OperationID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if operation.Status.Terminal() {
		return nil
	}
	copyCommandRunMetadata(&operation, run)
	operation.Status = status
	operation.CommandOutcome = string(outcome)
	operation.UpdatedAt = time.Now().UTC()
	if status == OperationCancelled {
		operation.Result = reason
		operation.Unknown = false
	} else if status == OperationUnknown {
		operation.Unknown = true
		operation.Error = reason
	}
	if err := s.repository.SaveOperation(ctx, operation); err != nil {
		return err
	}
	s.observerOperation(ctx, operation)
	return nil
}

func newCommandRun(operation Operation, item Workspace) CommandRun {
	now := time.Now().UTC()
	capabilities := DefaultCommandExecutionCapabilities(item)
	executor := capabilities.Executor
	mode := "shell"
	if operation.TTY != nil {
		mode = "pty"
		// CreateOperation currently admits PTY only for local workspaces. Keep
		// this capability change tied to the immutable request rather than to a
		// later runtime flag.
		capabilities.PTY = executionCapability(ExecutionCapabilitySupported, "本地命令使用受控伪终端；输出以 terminal 单流记录")
	}
	return CommandRun{
		ID:             newID("command-run"),
		UserID:         operation.UserID,
		WorkspaceID:    operation.WorkspaceID,
		ConversationID: operation.ConversationID,
		InvocationID:   operation.InvocationID,
		ToolCallID:     operation.ToolCallID,
		OperationID:    operation.ID,
		Executor:       executor,
		Mode:           mode,
		Capabilities:   capabilities,
		CommandPreview: truncateText(operation.Command, 4096),
		CWD:            operation.CWD,
		TimeoutMS:      int64(operation.Timeout) * int64(time.Second/time.Millisecond),
		TTY:            cloneTTYSpec(operation.TTY),
		Status:         CommandRunQueued,
		Revision:       1,
		QueuedAt:       now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

// prepareCommandRun creates the immutable admission checkpoint before an
// Operation is persisted. It is a no-op for legacy repositories without the
// optional command-run extension.
func (s *Service) prepareCommandRun(ctx context.Context, operation Operation, item Workspace) (CommandRun, bool, bool, error) {
	repository, ok := commandRunRepository(s)
	if !ok || operation.Type != OperationCommand {
		return CommandRun{}, false, false, nil
	}
	if id := strings.TrimSpace(operation.CommandRunID); id != "" {
		run, err := repository.GetCommandRun(ctx, id)
		if err == nil {
			if run.OperationID != operation.ID || run.WorkspaceID != operation.WorkspaceID {
				return CommandRun{}, false, false, fmt.Errorf("%w: command run 不属于当前 operation", ErrCommandRunConflict)
			}
			return run, true, false, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return CommandRun{}, false, false, err
		}
	}
	run := newCommandRun(operation, item)
	if err := run.Validate(); err != nil {
		return CommandRun{}, false, false, err
	}
	if err := repository.CreateCommandRun(ctx, run); err != nil {
		return CommandRun{}, false, false, err
	}
	return run, true, true, nil
}

func commandRunOutcomeForResult(result CommandResult, err error) (CommandRunStatus, CommandRunOutcome) {
	if result.Unknown {
		return CommandRunUnknown, CommandOutcomeUnknown
	}
	if result.TimedOut {
		return CommandRunTerminated, CommandOutcomeTimedOut
	}
	if result.StartFailed {
		return CommandRunStartFailed, CommandOutcomeRuntimeError
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return CommandRunCancelled, CommandOutcomeCancelled
		}
		if result.ExitCode >= 0 {
			return CommandRunExited, CommandOutcomeNonzero
		}
		if result.Started {
			return CommandRunTerminated, CommandOutcomeSignaled
		}
		return CommandRunStartFailed, CommandOutcomeRuntimeError
	}
	if result.ExitCode != 0 {
		return CommandRunExited, CommandOutcomeNonzero
	}
	return CommandRunExited, CommandOutcomeSuccess
}

func (s *Service) transitionCommandRun(ctx context.Context, run CommandRun, status CommandRunStatus, outcome CommandRunOutcome, result CommandResult, runErr error) (CommandRun, error) {
	repository, ok := commandRunRepository(s)
	if !ok {
		return run, nil
	}
	if run.Status.Terminal() {
		return run, nil
	}
	if !validCommandRunStatus(status) || !validCommandRunOutcome(outcome) {
		return CommandRun{}, fmt.Errorf("%w: 无效的命令运行状态转换", ErrCommandRunConflict)
	}
	if !commandRunTransitionAllowed(run.Status, status) {
		return CommandRun{}, fmt.Errorf("%w: %s -> %s", ErrCommandRunConflict, run.Status, status)
	}
	next := run
	next.Status = status
	if outcome != "" {
		next.Outcome = outcome
	}
	if status.Terminal() && result.ExitCode >= -1 {
		value := result.ExitCode
		next.ExitCode = &value
	}
	if status.Terminal() {
		preserveOutput := status == CommandRunUnknown && result.Unknown && result.Output == "" && result.StdoutBytes == 0 && result.StderrBytes == 0 && run.OutputDigest != ""
		if !preserveOutput {
			next.StdoutBytes = result.StdoutBytes
			next.StderrBytes = result.StderrBytes
			next.StoredBytes = int64(len(result.Output))
			next.OutputDigest = digestText(result.Output)
			next.OutputTruncated = result.Truncated
		}
	}
	if runErr != nil {
		next.Error = truncateText(runErr.Error(), 4096)
	} else {
		next.Error = ""
	}
	now := time.Now().UTC()
	if status == CommandRunRunning && next.StartedAt == nil {
		next.StartedAt = &now
	}
	if status.Terminal() {
		next.FinishedAt = &now
		// A terminal checkpoint gets one immutable retention deadline. A
		// purged/legacy state is never resurrected by a repeated transition.
		if next.OutputRetentionState != CommandOutputRetentionPurged {
			next.OutputRetentionState = CommandOutputRetentionRetained
			if next.OutputRetainedUntil == nil {
				if policy := s.commandOutputRetentionPolicy(); policy.MaxAge > 0 {
					deadline := now.Add(policy.MaxAge)
					next.OutputRetainedUntil = &deadline
				}
			}
		}
	}
	next.UpdatedAt = now
	next.Revision = run.Revision + 1
	if err := next.Validate(); err != nil {
		return CommandRun{}, err
	}
	saved, err := repository.UpdateCommandRun(ctx, next, run.Revision)
	if err != nil {
		return CommandRun{}, err
	}
	s.notifyCommandRun(ctx, saved)
	return saved, nil
}

func commandRunTransitionAllowed(from, to CommandRunStatus) bool {
	switch from {
	case CommandRunQueued:
		return to == CommandRunStarting || to == CommandRunCancelled || to == CommandRunUnknown
	case CommandRunStarting:
		return to == CommandRunRunning || to == CommandRunStartFailed || to == CommandRunCancelled || to == CommandRunTerminated || to == CommandRunUnknown
	case CommandRunRunning:
		return to == CommandRunExited || to == CommandRunTerminated || to == CommandRunStartFailed || to == CommandRunCancelled || to == CommandRunUnknown
	default:
		return false
	}
}

func (s *Service) markCommandRunUnknown(ctx context.Context, run CommandRun, reason string) (CommandRun, error) {
	if strings.TrimSpace(reason) == "" {
		reason = commandRunUnknownAfterRestart
	}
	return s.transitionCommandRun(ctx, run, CommandRunUnknown, CommandOutcomeUnknown, CommandResult{ExitCode: -1, Unknown: true}, errors.New(reason))
}

func copyCommandRunMetadata(operation *Operation, run CommandRun) {
	operation.CommandRunID = run.ID
	if strings.TrimSpace(run.UserID) != "" {
		operation.UserID = run.UserID
	}
	if run.ExitCode != nil {
		value := *run.ExitCode
		operation.ExitCode = &value
	}
	operation.CommandOutcome = string(run.Outcome)
	operation.OutputDigest = run.OutputDigest
	operation.OutputTruncated = run.OutputTruncated
	operation.OutputArtifact = cloneArtifactRef(run.OutputArtifact)
	operation.ArtifactError = run.ArtifactError
	operation.Unknown = run.Status == CommandRunUnknown || run.Outcome == CommandOutcomeUnknown
	if run.FinishedAt != nil {
		if run.StartedAt != nil {
			operation.DurationMS = run.FinishedAt.Sub(*run.StartedAt).Milliseconds()
			if operation.DurationMS < 0 {
				operation.DurationMS = 0
			}
		}
	}
}

func cloneArtifactRef(value *artifact.ArtifactRef) *artifact.ArtifactRef {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

// finalizeOperationFromCommandRun makes repeated approval requests safe. A
// terminal checkpoint is authoritative: no second process is started.
func (s *Service) finalizeOperationFromCommandRun(ctx context.Context, operation Operation, run CommandRun) (Operation, bool, error) {
	if !run.Status.Terminal() {
		return operation, false, nil
	}
	copyCommandRunMetadata(&operation, run)
	switch run.Status {
	case CommandRunExited:
		operation.Status = OperationCompleted
		if run.Outcome == CommandOutcomeNonzero {
			operation.Result = "命令已退出（非零退出码）；请查看命令输出和验证证据"
		} else if strings.TrimSpace(operation.Result) == "" {
			operation.Result = "命令已完成；输出请查看命令事件"
		}
	case CommandRunStartFailed:
		operation.Status = OperationFailed
	case CommandRunCancelled:
		operation.Status = OperationCancelled
	case CommandRunTerminated:
		operation.Status = OperationFailed
	case CommandRunUnknown:
		operation.Status = OperationUnknown
	}
	if strings.TrimSpace(run.Error) != "" {
		operation.Error = run.Error
	}
	operation.UpdatedAt = time.Now().UTC()
	if err := s.repository.SaveOperation(ctx, operation); err != nil {
		return Operation{}, true, err
	}
	s.observerOperation(ctx, operation)
	return operation, true, nil
}

func (s *Service) blockOperationForActiveCommandRun(ctx context.Context, operation Operation, run CommandRun) (Operation, error) {
	updated, markErr := s.markCommandRunUnknown(context.WithoutCancel(ctx), run, "命令进程已在服务内失去控制，禁止自动重跑")
	if markErr != nil && !errors.Is(markErr, ErrCommandRunConflict) {
		return Operation{}, markErr
	}
	if updated.ID != "" {
		run = updated
	}
	operation.Status = OperationUnknown
	operation.Unknown = true
	operation.CommandOutcome = string(CommandOutcomeUnknown)
	operation.CommandRunID = run.ID
	operation.Error = commandRunUnknownAfterRestart
	operation.UpdatedAt = time.Now().UTC()
	if err := s.repository.SaveOperation(ctx, operation); err != nil {
		return Operation{}, err
	}
	s.observerOperation(ctx, operation)
	return operation, fmt.Errorf("%w: 命令运行状态未知，禁止重跑", ErrOperationState)
}

func (s *Service) markOperationUnknown(ctx context.Context, operation Operation, cause error) (Operation, error) {
	operation.Status = OperationUnknown
	operation.Unknown = true
	operation.CommandOutcome = string(CommandOutcomeUnknown)
	if cause != nil {
		operation.Error = cause.Error()
	}
	operation.UpdatedAt = time.Now().UTC()
	if err := s.repository.SaveOperation(ctx, operation); err != nil {
		return Operation{}, err
	}
	s.observerOperation(ctx, operation)
	return operation, cause
}

func (s *Service) cancelQueuedCommandRun(ctx context.Context, operation Operation, reason string) error {
	if operation.Type != OperationCommand || strings.TrimSpace(operation.CommandRunID) == "" {
		return nil
	}
	run, err := s.GetCommandRun(ctx, operation.CommandRunID)
	if errors.Is(err, ErrUnsupported) || errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if run.Status != CommandRunQueued {
		return nil
	}
	_, err = s.transitionCommandRun(ctx, run, CommandRunCancelled, CommandOutcomeCancelled, CommandResult{ExitCode: -1}, errors.New(strings.TrimSpace(reason)))
	return err
}

// ReconcileCommandRuns closes non-terminal checkpoints left by a previous
// process. There is deliberately no attempt to inspect or rerun the old PID.
func (s *Service) ReconcileCommandRuns(ctx context.Context) error {
	repository, ok := commandRunRepository(s)
	if !ok {
		return nil
	}
	runs, err := repository.ListCommandRuns(ctx, "")
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.Status != CommandRunStarting && run.Status != CommandRunRunning {
			continue
		}
		updated, updateErr := s.markCommandRunUnknown(context.WithoutCancel(ctx), run, commandRunUnknownAfterRestart)
		if updateErr != nil {
			if errors.Is(updateErr, ErrCommandRunConflict) {
				continue
			}
			return updateErr
		}
		operation, operationErr := s.repository.GetOperation(context.WithoutCancel(ctx), updated.OperationID)
		if errors.Is(operationErr, ErrNotFound) {
			continue
		}
		if operationErr != nil {
			return operationErr
		}
		if operation.Status != OperationPending && operation.Status != OperationPrepared && operation.Status != OperationApproved && operation.Status != OperationRunning {
			continue
		}
		operation.Status = OperationUnknown
		operation.Unknown = true
		operation.CommandOutcome = string(CommandOutcomeUnknown)
		operation.Error = commandRunUnknownAfterRestart
		operation.UpdatedAt = time.Now().UTC()
		if saveErr := s.repository.SaveOperation(context.WithoutCancel(ctx), operation); saveErr != nil {
			return saveErr
		}
	}
	return nil
}
