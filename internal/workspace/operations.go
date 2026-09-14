package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

func (s *Service) CreateOperation(ctx context.Context, request OperationRequest) (Operation, error) {
	item, err := s.Resolve(ctx, request.WorkspaceID)
	if err != nil {
		return Operation{}, err
	}
	request.Path = strings.TrimSpace(request.Path)
	request.UserID = strings.TrimSpace(request.UserID)
	request.Command = strings.TrimSpace(request.Command)
	request.CWD = strings.TrimSpace(request.CWD)
	request.ConversationID = strings.TrimSpace(request.ConversationID)
	request.InvocationID = strings.TrimSpace(request.InvocationID)
	request.ToolCallID = strings.TrimSpace(request.ToolCallID)
	if request.CWD == "" {
		request.CWD = "."
	}
	if request.Timeout == 0 {
		request.Timeout = 60
	}
	if request.Timeout < 1 || request.Timeout > 120 {
		return Operation{}, fmt.Errorf("%w: 命令超时必须在 1 到 120 秒之间", ErrInvalidRequest)
	}
	if request.TTY != nil {
		if request.Type != OperationCommand {
			return Operation{}, fmt.Errorf("%w: TTY 只能用于 execute_command", ErrInvalidRequest)
		}
		spec := *request.TTY
		if err := spec.Validate(); err != nil {
			return Operation{}, err
		}
		if item.Type != TypeLocal {
			return Operation{}, fmt.Errorf("%w: 当前仅支持本地工作区 PTY", ErrUnsupported)
		}
		request.TTY = &spec
	}
	if request.InvocationID != "" {
		s.operationMu.Lock()
		admissionValidator := s.operationAdmission
		s.operationMu.Unlock()
		if admissionValidator != nil {
			if admissionErr := admissionValidator(ctx, cloneOperationRequest(request)); admissionErr != nil {
				return Operation{}, fmt.Errorf("工作流操作门禁拒绝: %w", admissionErr)
			}
		}
	}

	switch request.Type {
	case OperationWriteFile:
		if request.Path == "" {
			return Operation{}, fmt.Errorf("%w: 写入路径不能为空", ErrInvalidRequest)
		}
		if len(request.Content) > maxFileBytes {
			return Operation{}, fmt.Errorf("%w: 写入内容不能超过 %d MB", ErrInvalidRequest, maxFileBytes>>20)
		}
		if item.Type == TypeLocal {
			if _, err := resolveLocalPath(item.RootPath, request.Path, false); err != nil {
				return Operation{}, err
			}
		} else if _, err := resolveRemotePath(item.RootPath, request.Path); err != nil {
			return Operation{}, err
		}
	case OperationMakeDirectory:
		if request.Path == "" || cleanRelativePath(request.Path) == "." {
			return Operation{}, fmt.Errorf("%w: 新建目录路径不能为空且不能是工作区根目录", ErrInvalidRequest)
		}
		if item.Type == TypeLocal {
			target, pathErr := resolveLocalPath(item.RootPath, request.Path, false)
			if pathErr != nil {
				return Operation{}, pathErr
			}
			if _, statErr := os.Lstat(target); statErr == nil {
				return Operation{}, fmt.Errorf("%w: 目录或文件已存在", ErrInvalidRequest)
			} else if !os.IsNotExist(statErr) {
				return Operation{}, fmt.Errorf("读取新建目录状态失败: %w", statErr)
			}
		} else if _, pathErr := resolveRemotePath(item.RootPath, request.Path); pathErr != nil {
			return Operation{}, pathErr
		}
	case OperationPatchFile:
		if request.Path == "" {
			return Operation{}, fmt.Errorf("%w: 补丁路径不能为空", ErrInvalidRequest)
		}
		if request.OldText == "" {
			return Operation{}, fmt.Errorf("%w: 精确补丁的 old_text 不能为空", ErrInvalidRequest)
		}
		if len(request.NewText) > maxFileBytes {
			return Operation{}, fmt.Errorf("%w: 补丁后的文件不能超过 %d MB", ErrInvalidRequest, maxFileBytes>>20)
		}
		if item.Type == TypeLocal {
			target, pathErr := resolveLocalPath(item.RootPath, request.Path, true)
			if pathErr != nil {
				return Operation{}, pathErr
			}
			before, readErr := readLimitedFile(target)
			if readErr != nil {
				return Operation{}, readErr
			}
			if _, patchErr := applyTextPatch(before, request.OldText, request.NewText); patchErr != nil {
				return Operation{}, patchErr
			}
		} else if _, pathErr := resolveRemotePath(item.RootPath, request.Path); pathErr != nil {
			return Operation{}, pathErr
		}
	case OperationPatchSet:
		if len(request.Patches) == 0 || len(request.Patches) > 100 {
			return Operation{}, fmt.Errorf("%w: ChangeSet 必须包含 1 到 100 个文件补丁", ErrInvalidRequest)
		}
		// A ChangeSet may contain several hunks for one file. Keep the
		// in-memory working copy per path so later hunks are validated against
		// the result of earlier hunks, while the first digest remains the CAS
		// precondition for the whole file.
		working := make(map[string]string, len(request.Patches))
		original := make(map[string]string, len(request.Patches))
		for index := range request.Patches {
			patch := &request.Patches[index]
			patch.Path = strings.TrimSpace(patch.Path)
			if patch.Path == "" || patch.OldText == "" {
				return Operation{}, fmt.Errorf("%w: 第 %d 个补丁路径和 old_text 不能为空", ErrInvalidRequest, index+1)
			}
			if len(patch.NewText) > maxFileBytes {
				return Operation{}, fmt.Errorf("%w: 第 %d 个补丁后的文件不能超过 %d MB", ErrInvalidRequest, index+1, maxFileBytes>>20)
			}
			if item.Type == TypeLocal {
				if _, pathErr := resolveLocalPath(item.RootPath, patch.Path, true); pathErr != nil {
					return Operation{}, pathErr
				}
			} else if _, pathErr := resolveRemotePath(item.RootPath, patch.Path); pathErr != nil {
				return Operation{}, pathErr
			}
			key := cleanRelativePath(patch.Path)
			before, exists := working[key]
			if !exists {
				var readErr error
				before, readErr = s.readOperationFile(ctx, item, patch.Path)
				if readErr != nil {
					return Operation{}, readErr
				}
				original[key] = before
				working[key] = before
			}
			after, patchErr := applyTextPatch(before, patch.OldText, patch.NewText)
			if patchErr != nil {
				return Operation{}, fmt.Errorf("第 %d 个补丁无效: %w", index+1, patchErr)
			}
			working[key] = after
			if patch.ExpectedDigest == "" && original[key] == before {
				patch.ExpectedDigest = digestText(original[key])
			} else if patch.ExpectedDigest != "" && digestText(original[key]) != patch.ExpectedDigest {
				return Operation{}, fmt.Errorf("%w: 第 %d 个补丁的 expected_digest 与当前文件不匹配", ErrOperationState, index+1)
			}
		}
	case OperationDeletePath:
		if request.Path == "" || cleanRelativePath(request.Path) == "." {
			return Operation{}, fmt.Errorf("%w: 删除路径不能为空且不能是工作区根目录", ErrInvalidRequest)
		}
		if item.Type == TypeLocal {
			target, pathErr := resolveLocalPath(item.RootPath, request.Path, true)
			if pathErr != nil {
				return Operation{}, pathErr
			}
			info, statErr := os.Lstat(target)
			if statErr != nil {
				return Operation{}, fmt.Errorf("读取删除目标状态失败: %w", statErr)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return Operation{}, fmt.Errorf("%w: 不允许删除符号链接", ErrInvalidRequest)
			}
		} else if _, pathErr := resolveRemotePath(item.RootPath, request.Path); pathErr != nil {
			return Operation{}, pathErr
		}
	case OperationCommand:
		if request.Command == "" {
			return Operation{}, fmt.Errorf("%w: 命令不能为空", ErrInvalidRequest)
		}
		if len(request.Command) > 10_000 {
			return Operation{}, fmt.Errorf("%w: 命令过长", ErrInvalidRequest)
		}
		if item.Type == TypeLocal {
			if _, err := resolveLocalPath(item.RootPath, request.CWD, true); err != nil {
				return Operation{}, err
			}
		} else if _, err := resolveRemotePath(item.RootPath, request.CWD); err != nil {
			return Operation{}, err
		}
	default:
		return Operation{}, fmt.Errorf("%w: 操作类型无效", ErrInvalidRequest)
	}

	now := time.Now().UTC()
	operation := Operation{
		ID: newID("operation"), UserID: request.UserID, WorkspaceID: item.ID, ConversationID: request.ConversationID, InvocationID: request.InvocationID, ToolCallID: request.ToolCallID, Type: request.Type,
		Path: request.Path, Content: request.Content, PatchOld: request.OldText, PatchNew: request.NewText, Command: request.Command,
		Patches:        append([]FilePatch(nil), request.Patches...),
		ExpectedDigest: request.ExpectedDigest,
		CWD:            request.CWD, Timeout: request.Timeout, TTY: cloneTTYSpec(request.TTY), RetryOf: request.RetryOf, Status: OperationPrepared,
		CreatedAt: now, UpdatedAt: now,
	}
	if operation.Type == OperationWriteFile {
		operation.Preview = truncateText(operation.Content, 4000)
		// 本机文件在创建申请时即可生成预览；远程文件在批准时再读取，避免申请阶段重复建立 SSH 连接。
		if item.Type == TypeLocal {
			operation.Diff = s.writeDiff(ctx, item, operation.Path, operation.Content)
		}
	}
	if operation.Type == OperationPatchFile {
		operation.Preview = fmt.Sprintf("精确替换 %d 个字符片段", len([]rune(operation.PatchOld)))
		// 本机可在提交申请时生成 diff；远程文件在批准时读取并再次校验，防止覆盖他人刚刚的修改。
		if item.Type == TypeLocal {
			before, readErr := s.readOperationFile(ctx, item, operation.Path)
			if readErr != nil {
				return Operation{}, readErr
			}
			operation.Content, err = applyTextPatch(before, operation.PatchOld, operation.PatchNew)
			if err != nil {
				return Operation{}, err
			}
			operation.Diff = unifiedTextDiff(operation.Path, before, operation.Content)
			if operation.ExpectedDigest == "" {
				operation.ExpectedDigest = digestText(before)
			}
		}
	}
	if operation.Type == OperationPatchSet {
		files := make(map[string]struct{}, len(operation.Patches))
		for _, patch := range operation.Patches {
			files[cleanRelativePath(patch.Path)] = struct{}{}
		}
		operation.Preview = fmt.Sprintf("ChangeSet：原子修改 %d 个文件、%d 个 hunk", len(files), len(operation.Patches))
		var diffs strings.Builder
		working := make(map[string]string, len(files))
		original := make(map[string]string, len(files))
		order := make([]string, 0, len(files))
		for index := range operation.Patches {
			patch := &operation.Patches[index]
			key := cleanRelativePath(patch.Path)
			before, exists := working[key]
			if !exists {
				var readErr error
				before, readErr = s.readOperationFile(ctx, item, patch.Path)
				if readErr != nil {
					return Operation{}, readErr
				}
				working[key] = before
				original[key] = before
				order = append(order, key)
			}
			after, patchErr := applyTextPatch(before, patch.OldText, patch.NewText)
			if patchErr != nil {
				return Operation{}, fmt.Errorf("第 %d 个补丁无效: %w", index+1, patchErr)
			}
			working[key] = after
			if patch.ExpectedDigest == "" {
				patch.ExpectedDigest = digestText(original[key])
			}
		}
		for _, key := range order {
			if diffs.Len() > 0 {
				diffs.WriteString("\n")
			}
			diffs.WriteString(unifiedTextDiff(key, original[key], working[key]))
		}
		operation.Diff = diffs.String()
	}
	if operation.Type == OperationMakeDirectory {
		operation.Preview = "新建目录（批准后执行）"
	}
	if operation.Type == OperationDeletePath {
		operation.Preview = "删除文件或空目录（批准后执行）"
	}
	if operation.Diff != "" {
		operation = s.projectOperationDiffArtifact(ctx, operation)
	}
	var commandRun CommandRun
	commandRunAvailable := false
	commandRunCreated := false
	if operation.Type == OperationCommand {
		commandRun, commandRunAvailable, commandRunCreated, err = s.prepareCommandRun(ctx, operation, item)
		if err != nil {
			return Operation{}, fmt.Errorf("创建命令运行检查点失败: %w", err)
		}
		if commandRunAvailable {
			operation.CommandRunID = commandRun.ID
		}
	}
	if err := s.repository.SaveOperation(ctx, operation); err != nil {
		if commandRunCreated {
			if repository, ok := commandRunRepository(s); ok {
				_ = repository.DeleteCommandRun(context.WithoutCancel(ctx), commandRun.ID)
			}
		}
		return Operation{}, err
	}
	if commandRunCreated {
		// Publish only after the Operation and its command checkpoint are both
		// durable. This prevents a failed Operation insert from leaving an
		// audit event that points at a deleted command run.
		s.notifyCommandRun(ctx, commandRun)
	}
	return operation, nil
}

func (s *Service) ListOperations(ctx context.Context, workspaceID string) ([]Operation, error) {
	if strings.TrimSpace(workspaceID) != "" {
		if _, err := s.Get(ctx, workspaceID); err != nil {
			return nil, err
		}
	}
	return s.repository.ListOperations(ctx, strings.TrimSpace(workspaceID))
}

func (s *Service) ApproveOperation(ctx context.Context, operationID string) (Operation, error) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	operation, err := s.repository.GetOperation(ctx, strings.TrimSpace(operationID))
	if err != nil {
		return Operation{}, err
	}
	if operation.Status == OperationCompleted || operation.Status == OperationFailed || operation.Status == OperationRejected || operation.Status == OperationCancelled || operation.Status == OperationExpired || operation.Status == OperationStale || operation.Status == OperationUnknown {
		return operation, nil
	}
	if operation.Status != OperationPending && operation.Status != OperationPrepared {
		return Operation{}, fmt.Errorf("%w: 当前状态为 %s", ErrOperationState, operation.Status)
	}
	if operation.ConversationID != "" && s.conversationChecker != nil {
		active, checkErr := s.conversationChecker(ctx, operation.ConversationID)
		if checkErr != nil {
			return Operation{}, checkErr
		}
		if !active {
			return Operation{}, fmt.Errorf("%w: 对话已归档，不能批准工作区操作", ErrOperationState)
		}
	}
	item, err := s.Resolve(ctx, operation.WorkspaceID)
	if err != nil {
		return Operation{}, err
	}
	if operation.InvocationID != "" {
		if s.operationAdmission != nil {
			if admissionErr := s.operationAdmission(ctx, operationRequestFromOperation(operation)); admissionErr != nil {
				operation.Status = OperationStale
				operation.Error = fmt.Sprintf("工作流步骤在批准前失效: %v", admissionErr)
				operation.UpdatedAt = time.Now().UTC()
				if saveErr := s.repository.SaveOperation(ctx, operation); saveErr != nil {
					return Operation{}, saveErr
				}
				return operation, fmt.Errorf("%w: %s", ErrOperationState, operation.Error)
			}
		}
		// The Runtime performs the same read-only check before resolving the
		// approval. Repeating it here closes the TOCTOU window between that
		// decision and the actual side effect, including resumed ADK calls.
		if validator := s.instructionValidator; validator != nil {
			if validateErr := validator(ctx, operation.InvocationID); validateErr != nil {
				operation.Status = OperationStale
				operation.Error = fmt.Errorf("项目指令快照在写入边界失效: %w", validateErr).Error()
				operation.UpdatedAt = time.Now().UTC()
				if saveErr := s.repository.SaveOperation(ctx, operation); saveErr != nil {
					return Operation{}, saveErr
				}
				return operation, fmt.Errorf("%w: %s", ErrOperationState, operation.Error)
			}
		}
	}
	var commandRun CommandRun
	commandRunEnabled := false
	commandRunCreated := false
	if operation.Type == OperationCommand {
		var prepared bool
		commandRun, prepared, commandRunCreated, err = s.prepareCommandRun(ctx, operation, item)
		if err != nil {
			return Operation{}, fmt.Errorf("准备命令运行检查点失败: %w", err)
		}
		commandRunEnabled = prepared && strings.TrimSpace(commandRun.ID) != ""
		if commandRunEnabled {
			operation.CommandRunID = commandRun.ID
			if finalized, handled, finalizeErr := s.finalizeOperationFromCommandRun(ctx, operation, commandRun); handled {
				return finalized, finalizeErr
			}
			if commandRun.Status == CommandRunStarting || commandRun.Status == CommandRunRunning {
				return s.blockOperationForActiveCommandRun(ctx, operation, commandRun)
			}
			if commandRun.Status != CommandRunQueued {
				return Operation{}, fmt.Errorf("%w: 命令运行检查点当前状态为 %s", ErrOperationState, commandRun.Status)
			}
		}
	}
	operation.Status = OperationApproved
	operation.UpdatedAt = time.Now().UTC()
	if err := s.repository.SaveOperation(ctx, operation); err != nil {
		return Operation{}, err
	}
	if commandRunCreated && commandRunEnabled && strings.TrimSpace(operation.CommandRunID) != "" {
		// A legacy Operation may acquire its first CommandRun during approval;
		// publish queued only after the Operation binding is durable. Existing
		// checkpoints were already published at creation time.
		s.notifyCommandRun(ctx, commandRun)
	}
	if commandRunEnabled {
		var transitionErr error
		commandRun, transitionErr = s.transitionCommandRun(ctx, commandRun, CommandRunStarting, "", CommandResult{}, nil)
		if transitionErr != nil {
			return s.markOperationUnknown(ctx, operation, fmt.Errorf("命令启动检查点写入失败: %w", transitionErr))
		}
	}
	operation.Status = OperationRunning
	operation.UpdatedAt = time.Now().UTC()
	if err := s.repository.SaveOperation(ctx, operation); err != nil {
		return Operation{}, err
	}
	if commandRunEnabled {
		var transitionErr error
		commandRun, transitionErr = s.transitionCommandRun(ctx, commandRun, CommandRunRunning, "", CommandResult{}, nil)
		if transitionErr != nil {
			return s.markOperationUnknown(ctx, operation, fmt.Errorf("命令运行检查点写入失败: %w", transitionErr))
		}
	}

	var result string
	var commandResult CommandResult
	var outputCollector *commandChunkCollector
	var persistedOutputChunks []CommandOutputChunk
	switch operation.Type {
	case OperationWriteFile:
		if operation.Diff == "" {
			operation.Diff = s.writeDiff(ctx, item, operation.Path, operation.Content)
		}
		err = writeWorkspaceFile(ctx, item, operation.Path, operation.Content)
		if err == nil {
			result = fmt.Sprintf("已写入 %s", operation.Path)
		}
	case OperationMakeDirectory:
		err = makeWorkspaceDirectory(ctx, item, operation.Path)
		if err == nil {
			result = fmt.Sprintf("已新建目录 %s", operation.Path)
		}
	case OperationPatchFile:
		var before string
		before, err = s.readOperationFile(ctx, item, operation.Path)
		if err == nil && operation.ExpectedDigest != "" && digestText(before) != operation.ExpectedDigest {
			err = fmt.Errorf("%w: 文件在批准前已发生变化", ErrOperationState)
		}
		if err == nil {
			operation.Content, err = applyTextPatch(before, operation.PatchOld, operation.PatchNew)
		}
		if err == nil {
			operation.Diff = unifiedTextDiff(operation.Path, before, operation.Content)
			err = writeWorkspaceFile(ctx, item, operation.Path, operation.Content)
		}
		if err == nil {
			result = fmt.Sprintf("已应用补丁 %s", operation.Path)
		}
	case OperationPatchSet:
		err = s.applyPatchSet(ctx, item, &operation)
		if err == nil {
			result = fmt.Sprintf("已原子应用 %d 个文件补丁", len(operation.Patches))
		}
	case OperationDeletePath:
		err = deleteWorkspacePath(ctx, item, operation.Path)
		if err == nil {
			result = fmt.Sprintf("已删除 %s", operation.Path)
		}
	case OperationCommand:
		runCtx := ctx
		if operation.InvocationID != "" {
			runCtx = WithInvocationID(runCtx, operation.InvocationID)
		}
		if commandRunEnabled {
			runCtx = WithCommandRunID(runCtx, commandRun.ID)
			if _, outputRepository := s.repository.(CommandOutputRepository); outputRepository {
				outputCollector = &commandChunkCollector{}
				runCtx = WithCommandChunkSink(runCtx, outputCollector.add)
			}
			commandCtx, commandCancel := context.WithCancel(runCtx)
			s.registerCommandRunCancel(commandRun.ID, commandCancel)
			defer func() {
				s.unregisterCommandRunCancel(commandRun.ID)
				commandCancel()
			}()
			runCtx = commandCtx
		}
		if operation.TTY != nil {
			commandRunID := ""
			if commandRunEnabled {
				commandRunID = commandRun.ID
			}
			commandResult, err = s.executeWorkspaceCommand(runCtx, item, operation.CWD, operation.Command, operation.Timeout, operation.TTY, commandRunID)
		} else {
			commandResult, err = executeWorkspaceCommand(runCtx, item, operation.CWD, operation.Command, operation.Timeout)
		}
		result = commandResult.Output
	default:
		err = fmt.Errorf("%w: 未知操作类型", ErrInvalidRequest)
	}
	if outputCollector != nil {
		chunks, dropped := outputCollector.snapshot()
		persistedOutputChunks = append([]CommandOutputChunk(nil), chunks...)
		persistErr := s.persistCommandOutputChunks(context.WithoutCancel(ctx), chunks)
		if dropped && persistErr == nil {
			persistErr = errors.New("命令输出 chunk 超出有界持久化容量")
		}
		if persistErr != nil {
			// The process has already crossed its side-effect boundary, but the
			// audit stream is incomplete. Mark the attempt unknown rather than
			// claiming a complete, replayable result or retrying the command.
			commandResult.Unknown = true
			outputError := fmt.Errorf("命令输出持久化失败: %w", persistErr)
			if err == nil {
				err = outputError
			} else {
				err = fmt.Errorf("%v；%w", err, outputError)
			}
		}
	}
	operation.UpdatedAt = time.Now().UTC()
	operation.Result = truncateText(result, maxOutputBytes)
	if operation.Type == OperationCommand {
		// Persist only bounded command outcome metadata. The bounded Result field
		// remains the ToolResult summary; replayable chunks are kept in the
		// optional CommandOutputRepository, and the digest lets Runtime bind
		// evidence without copying output into the plan or VerificationRun.
		exitCode := commandResult.ExitCode
		operation.ExitCode = &exitCode
		operation.OutputDigest = digestText(commandResult.Output)
		operation.OutputTruncated = commandResult.Truncated
		operation.DurationMS = commandResult.DurationMS
		operation.TimedOut = commandResult.TimedOut
		operation.Unknown = commandResult.Unknown
		_, commandOutcome := commandRunOutcomeForResult(commandResult, err)
		operation.CommandOutcome = string(commandOutcome)
	}
	if commandRunEnabled {
		commandRun = s.projectCommandOutputArtifact(context.WithoutCancel(ctx), commandRun, commandResult, persistedOutputChunks)
		status, outcome := commandRunOutcomeForResult(commandResult, err)
		var transitionErr error
		commandRun, transitionErr = s.transitionCommandRun(context.WithoutCancel(ctx), commandRun, status, outcome, commandResult, err)
		if transitionErr != nil {
			return s.markOperationUnknown(context.WithoutCancel(ctx), operation, fmt.Errorf("命令终态检查点写入失败: %w", transitionErr))
		}
		copyCommandRunMetadata(&operation, commandRun)
	}
	if operation.Diff != "" && operation.DiffArtifact == nil {
		operation = s.projectOperationDiffArtifact(context.WithoutCancel(ctx), operation)
	}
	if operation.Type == OperationCommand && operation.CommandOutcome == string(CommandOutcomeNonzero) && err != nil {
		// A process that exited with a non-zero code reached a known terminal
		// boundary. Keep the diagnostic on the Operation, but do not turn the
		// approval HTTP call into an infrastructure error; Runtime maps this
		// completed attempt to failed verification using CommandOutcome.
		operation.Error = err.Error()
		err = nil
	}
	if err != nil {
		switch {
		case commandResult.Unknown:
			operation.Status = OperationUnknown
		case operation.Type == OperationCommand && operation.CommandOutcome == string(CommandOutcomeCancelled):
			operation.Status = OperationCancelled
		case errors.Is(err, ErrOperationState):
			operation.Status = OperationStale
		default:
			operation.Status = OperationFailed
		}
		operation.Error = err.Error()
	} else {
		operation.Status = OperationCompleted
	}
	if saveErr := s.repository.SaveOperation(ctx, operation); saveErr != nil {
		return Operation{}, saveErr
	}
	if s.operationObserver != nil {
		// The final status and result are durable before notification. The
		// observer is best-effort and cannot change whether this side effect
		// succeeded; it may only project metadata such as VerificationRun.
		s.observerOperation(ctx, operation)
	}
	return operation, err
}

func cloneOperationRequest(request OperationRequest) OperationRequest {
	request.Patches = append([]FilePatch(nil), request.Patches...)
	request.TTY = cloneTTYSpec(request.TTY)
	return request
}

func operationRequestFromOperation(operation Operation) OperationRequest {
	return OperationRequest{
		WorkspaceID: operation.WorkspaceID, UserID: operation.UserID, ConversationID: operation.ConversationID,
		InvocationID: operation.InvocationID, ToolCallID: operation.ToolCallID,
		Type: operation.Type, Path: operation.Path, Content: operation.Content,
		OldText: operation.PatchOld, NewText: operation.PatchNew,
		Patches: append([]FilePatch(nil), operation.Patches...), ExpectedDigest: operation.ExpectedDigest,
		Command: operation.Command, CWD: operation.CWD, Timeout: operation.Timeout, TTY: cloneTTYSpec(operation.TTY),
		RetryOf: operation.RetryOf,
	}
}

func cloneTTYSpec(spec *TTYSpec) *TTYSpec {
	if spec == nil {
		return nil
	}
	copySpec := *spec
	return &copySpec
}

func (s *Service) observerOperation(ctx context.Context, operation Operation) {
	observer := s.operationObserver
	if observer == nil {
		return
	}
	observer(ctx, operation)
}

// readOperationFile 读取补丁申请对应的当前内容；它复用工作区传输层，保证本地和 SSH 行为一致。
func (s *Service) readOperationFile(ctx context.Context, item Workspace, relative string) (string, error) {
	if item.Type == TypeLocal {
		target, err := resolveLocalPath(item.RootPath, relative, true)
		if err != nil {
			return "", err
		}
		return readLimitedFile(target)
	}
	return readSSHFile(ctx, item, relative)
}

type preparedPatch struct {
	patch  FilePatch
	before string
	after  string
	target string
}

// applyPatchSet validates every input before changing the filesystem, writes
// local files through same-directory temp files, and rolls back already-renamed
// files if a later rename fails. Remote transports retain the same digest/CAS
// checks but cannot provide host-wide atomic rename across connections.
func (s *Service) applyPatchSet(ctx context.Context, item Workspace, operation *Operation) error {
	working := make(map[string]string, len(operation.Patches))
	original := make(map[string]string, len(operation.Patches))
	order := make([]string, 0, len(operation.Patches))
	paths := make(map[string]string, len(operation.Patches))
	for index, patch := range operation.Patches {
		key := cleanRelativePath(patch.Path)
		before, exists := working[key]
		if !exists {
			var err error
			before, err = s.readOperationFile(ctx, item, patch.Path)
			if err != nil {
				return err
			}
			working[key] = before
			original[key] = before
			paths[key] = patch.Path
			order = append(order, key)
			if patch.ExpectedDigest != "" && digestText(before) != patch.ExpectedDigest {
				return fmt.Errorf("%w: 文件 %s 在批准前已发生变化", ErrOperationState, patch.Path)
			}
		} else if patch.ExpectedDigest != "" && digestText(original[key]) != patch.ExpectedDigest {
			return fmt.Errorf("%w: 文件 %s 的 expected_digest 不一致", ErrOperationState, patch.Path)
		}
		after, err := applyTextPatch(before, patch.OldText, patch.NewText)
		if err != nil {
			return fmt.Errorf("第 %d 个补丁无效: %w", index+1, err)
		}
		working[key] = after
	}
	prepared := make([]preparedPatch, 0, len(order))
	for _, key := range order {
		target := ""
		var err error
		if item.Type == TypeLocal {
			target, err = resolveLocalPath(item.RootPath, paths[key], true)
			if err != nil {
				return err
			}
		}
		prepared = append(prepared, preparedPatch{patch: FilePatch{Path: paths[key]}, before: original[key], after: working[key], target: target})
	}
	if item.Type != TypeLocal {
		for _, change := range prepared {
			if err := writeWorkspaceFile(ctx, item, change.patch.Path, change.after); err != nil {
				return err
			}
		}
		return nil
	}

	type backup struct {
		target string
		data   []byte
		mode   os.FileMode
		exists bool
	}
	backups := make([]backup, 0, len(prepared))
	temps := make([]string, 0, len(prepared))
	cleanup := func() {
		for _, temp := range temps {
			_ = os.Remove(temp)
		}
	}
	for _, change := range prepared {
		info, statErr := os.Stat(change.target)
		if statErr != nil {
			cleanup()
			return statErr
		}
		data, readErr := os.ReadFile(change.target)
		if readErr != nil {
			cleanup()
			return readErr
		}
		file, createErr := os.CreateTemp(filepath.Dir(change.target), ".abot-patch-*")
		if createErr != nil {
			cleanup()
			return createErr
		}
		temp := file.Name()
		temps = append(temps, temp)
		if _, writeErr := file.WriteString(change.after); writeErr != nil {
			_ = file.Close()
			cleanup()
			return writeErr
		}
		if chmodErr := file.Chmod(info.Mode().Perm()); chmodErr != nil {
			_ = file.Close()
			cleanup()
			return chmodErr
		}
		if closeErr := file.Close(); closeErr != nil {
			cleanup()
			return closeErr
		}
		backups = append(backups, backup{target: change.target, data: data, mode: info.Mode().Perm(), exists: true})
	}
	committed := 0
	for index, change := range prepared {
		if err := os.Rename(temps[index], change.target); err != nil {
			for rollback := committed - 1; rollback >= 0; rollback-- {
				_ = os.WriteFile(backups[rollback].target, backups[rollback].data, backups[rollback].mode)
			}
			cleanup()
			return fmt.Errorf("原子提交 ChangeSet 失败: %w", err)
		}
		committed++
	}
	cleanup()
	return nil
}

func applyTextPatch(before, oldText, newText string) (string, error) {
	if oldText == "" {
		return "", fmt.Errorf("%w: 精确补丁的 old_text 不能为空", ErrInvalidRequest)
	}
	count := strings.Count(before, oldText)
	if count == 0 {
		return "", fmt.Errorf("%w: 补丁目标文本已不存在，文件可能已经变化", ErrOperationState)
	}
	if count > 1 {
		return "", fmt.Errorf("%w: 补丁目标文本匹配了 %d 处，拒绝不明确的修改", ErrInvalidRequest, count)
	}
	after := strings.Replace(before, oldText, newText, 1)
	if len(after) > maxFileBytes {
		return "", fmt.Errorf("%w: 补丁后的文件不能超过 %d MB", ErrInvalidRequest, maxFileBytes>>20)
	}
	return after, nil
}

func digestText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// writeDiff 尽力读取写入前的文本并生成统一 diff；读取失败不阻止用户审批实际写入。
func (s *Service) writeDiff(ctx context.Context, item Workspace, relative, content string) string {
	before := ""
	if item.Type == TypeLocal {
		target, err := resolveLocalPath(item.RootPath, relative, true)
		if err == nil {
			if value, readErr := readLimitedFile(target); readErr == nil {
				before = value
			}
		}
	} else if value, err := readSSHFile(ctx, item, relative); err == nil {
		before = value
	}
	return unifiedTextDiff(relative, before, content)
}

// unifiedTextDiff 生成受大小保护的文本差异，给 WebUI 审批和审计使用。
func unifiedTextDiff(relative, before, after string) string {
	if before == after {
		return ""
	}
	oldLines := diffLines(before)
	newLines := diffLines(after)
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix && oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}
	oldEnd := len(oldLines) - suffix
	newEnd := len(newLines) - suffix
	contextStart := prefix - 3
	if contextStart < 0 {
		contextStart = 0
	}
	contextEndOld := oldEnd + 3
	if contextEndOld > len(oldLines) {
		contextEndOld = len(oldLines)
	}
	contextEndNew := newEnd + 3
	if contextEndNew > len(newLines) {
		contextEndNew = len(newLines)
	}
	var builder strings.Builder
	builder.WriteString("--- a/")
	builder.WriteString(strings.TrimPrefix(strings.ReplaceAll(relative, "\\", "/"), "/"))
	builder.WriteString("\n+++ b/")
	builder.WriteString(strings.TrimPrefix(strings.ReplaceAll(relative, "\\", "/"), "/"))
	builder.WriteString("\n@@ -")
	builder.WriteString(fmt.Sprintf("%d,%d +%d,%d @@\n", contextStart+1, contextEndOld-contextStart, contextStart+1, contextEndNew-contextStart))
	for index := contextStart; index < prefix; index++ {
		builder.WriteString(" ")
		builder.WriteString(oldLines[index])
		builder.WriteByte('\n')
	}
	for index := prefix; index < oldEnd; index++ {
		builder.WriteString("-")
		builder.WriteString(oldLines[index])
		builder.WriteByte('\n')
	}
	for index := prefix; index < newEnd; index++ {
		builder.WriteString("+")
		builder.WriteString(newLines[index])
		builder.WriteByte('\n')
	}
	for index := oldEnd; index < contextEndOld; index++ {
		builder.WriteString(" ")
		builder.WriteString(oldLines[index])
		builder.WriteByte('\n')
	}
	return truncateText(builder.String(), maxOutputBytes)
}

func diffLines(value string) []string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.TrimSuffix(value, "\n")
	if value == "" {
		return []string{}
	}
	return strings.Split(value, "\n")
}

func (s *Service) RejectOperation(ctx context.Context, operationID string) (Operation, error) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	operation, err := s.repository.GetOperation(ctx, strings.TrimSpace(operationID))
	if err != nil {
		return Operation{}, err
	}
	if operation.Status == OperationRejected {
		return operation, nil
	}
	if operation.Status != OperationPending && operation.Status != OperationPrepared {
		return Operation{}, fmt.Errorf("%w: 当前状态为 %s", ErrOperationState, operation.Status)
	}
	operation.Status = OperationRejected
	operation.UpdatedAt = time.Now().UTC()
	operation.Result = "用户已拒绝"
	if err := s.cancelQueuedCommandRun(ctx, operation, "用户已拒绝命令执行"); err != nil {
		return Operation{}, err
	}
	if err := s.repository.SaveOperation(ctx, operation); err != nil {
		return Operation{}, err
	}
	return operation, nil
}

// CancelOperation closes a prepared operation when its parent Invocation is
// cancelled. It is intentionally separate from RejectOperation so audit
// consumers can distinguish a user decision from lifecycle cancellation.
func (s *Service) CancelOperation(ctx context.Context, operationID string) (Operation, error) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	operation, err := s.repository.GetOperation(ctx, strings.TrimSpace(operationID))
	if err != nil {
		return Operation{}, err
	}
	if operation.Status != OperationPending && operation.Status != OperationPrepared && operation.Status != OperationApproved {
		if operation.Status == OperationCancelled {
			return operation, nil
		}
		return Operation{}, fmt.Errorf("%w: 当前状态为 %s", ErrOperationState, operation.Status)
	}
	operation.Status = OperationCancelled
	operation.UpdatedAt = time.Now().UTC()
	operation.Result = "Invocation 已取消"
	if err := s.cancelQueuedCommandRun(ctx, operation, "Invocation 已取消命令执行"); err != nil {
		return Operation{}, err
	}
	if err := s.repository.SaveOperation(ctx, operation); err != nil {
		return Operation{}, err
	}
	return operation, nil
}

// ExpireOperation closes a prepared operation whose approval deadline elapsed.
// It is intentionally idempotent for repeated scheduler scans and never runs
// the underlying side effect.
func (s *Service) ExpireOperation(ctx context.Context, operationID string) (Operation, error) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	operation, err := s.repository.GetOperation(ctx, strings.TrimSpace(operationID))
	if err != nil {
		return Operation{}, err
	}
	if operation.Status == OperationExpired {
		return operation, nil
	}
	if operation.Status != OperationPending && operation.Status != OperationPrepared && operation.Status != OperationApproved {
		return Operation{}, fmt.Errorf("%w: 当前状态为 %s", ErrOperationState, operation.Status)
	}
	operation.Status = OperationExpired
	operation.UpdatedAt = time.Now().UTC()
	operation.Result = "审批已过期"
	if err := s.cancelQueuedCommandRun(ctx, operation, "审批已过期，命令执行未启动"); err != nil {
		return Operation{}, err
	}
	if err := s.repository.SaveOperation(ctx, operation); err != nil {
		return Operation{}, err
	}
	return operation, nil
}

func writeWorkspaceFile(ctx context.Context, item Workspace, relative, content string) error {
	if item.Type == TypeLocal {
		target, err := resolveLocalPath(item.RootPath, relative, false)
		if err != nil {
			return err
		}
		if info, statErr := os.Stat(target); statErr == nil && info.IsDir() {
			return errors.New("写入目标是目录")
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			return fmt.Errorf("写入文件失败: %w", err)
		}
		return nil
	}
	target, err := resolveRemotePath(item.RootPath, relative)
	if err != nil {
		return err
	}
	connection, _, err := dialSSH(ctx, item, false)
	if err != nil {
		return err
	}
	defer connection.Close()
	parent := path.Dir(target)
	quotedTarget := shellQuote(target)
	command := "root=$(realpath -- " + shellQuote(item.RootPath) + ") || exit 2; parent=$(realpath -- " + shellQuote(parent) + ") || exit 2; " +
		"case \"$parent\" in \"$root\"|\"$root\"/*) ;; *) echo '路径超出工作区' >&2; exit 3;; esac; " +
		"if [ -e " + quotedTarget + " ] || [ -L " + quotedTarget + " ]; then actual=$(realpath -- " + quotedTarget + ") || exit 2; case \"$actual\" in \"$root\"|\"$root\"/*) ;; *) echo '符号链接超出工作区' >&2; exit 3;; esac; fi; cat > " + quotedTarget
	_, err = runSSH(ctx, connection.client, command, content, 30*time.Second)
	return err
}

func makeWorkspaceDirectory(ctx context.Context, item Workspace, relative string) error {
	if item.Type == TypeLocal {
		return makeLocalDirectory(item.RootPath, relative)
	}
	target, err := resolveRemotePath(item.RootPath, relative)
	if err != nil {
		return err
	}
	connection, _, err := dialSSH(ctx, item, false)
	if err != nil {
		return err
	}
	defer connection.Close()
	quotedTarget := shellQuote(target)
	command := remoteCreateBoundaryCommand(item.RootPath, target,
		"if [ -e "+quotedTarget+" ] || [ -L "+quotedTarget+" ]; then echo '目标路径已存在' >&2; exit 4; fi; "+
			"mkdir -p -- "+quotedTarget+" || exit 4; actual=$(realpath -- "+quotedTarget+") || exit 2; "+
			"case \"$actual\" in \"$root\"|\"$root\"/*) ;; *) echo '路径超出工作区' >&2; exit 3;; esac")
	_, err = runSSH(ctx, connection.client, command, "", 30*time.Second)
	return err
}

// makeLocalDirectory 逐级创建目录，并拒绝穿过符号链接，避免 MkdirAll 把路径带到工作区外。
func makeLocalDirectory(root, relative string) error {
	target, err := resolveLocalPath(root, relative, false)
	if err != nil {
		return err
	}
	if _, statErr := os.Lstat(target); statErr == nil {
		return fmt.Errorf("%w: 目录或文件已存在", ErrOperationState)
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("读取新建目录状态失败: %w", statErr)
	}
	rootReal, err := resolveLocalPath(root, ".", true)
	if err != nil {
		return err
	}
	relative = cleanRelativePath(relative)
	current := rootReal
	for _, part := range strings.Split(filepath.ToSlash(relative), "/") {
		if part == "" || part == "." {
			continue
		}
		next := filepath.Join(current, filepath.FromSlash(part))
		info, statErr := os.Lstat(next)
		if os.IsNotExist(statErr) {
			if mkdirErr := os.Mkdir(next, 0o755); mkdirErr != nil {
				return fmt.Errorf("新建目录失败: %w", mkdirErr)
			}
			current = next
			continue
		}
		if statErr != nil {
			return fmt.Errorf("读取目录状态失败: %w", statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: 目录路径不能经过符号链接", ErrInvalidRequest)
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: 路径中的 %s 不是目录", ErrInvalidRequest, part)
		}
		current = next
	}
	if _, statErr := os.Stat(target); statErr != nil {
		return fmt.Errorf("新建目录后校验失败: %w", statErr)
	}
	return nil
}

func deleteWorkspacePath(ctx context.Context, item Workspace, relative string) error {
	if cleanRelativePath(relative) == "." {
		return fmt.Errorf("%w: 不能删除工作区根目录", ErrInvalidRequest)
	}
	if item.Type == TypeLocal {
		target, err := resolveLocalPath(item.RootPath, relative, true)
		if err != nil {
			return err
		}
		info, err := os.Lstat(target)
		if err != nil {
			return fmt.Errorf("读取删除目标状态失败: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: 不允许删除符号链接", ErrInvalidRequest)
		}
		if info.IsDir() {
			entries, readErr := os.ReadDir(target)
			if readErr != nil {
				return fmt.Errorf("读取待删除目录失败: %w", readErr)
			}
			if len(entries) > 0 {
				return fmt.Errorf("%w: 只允许删除空目录", ErrInvalidRequest)
			}
		}
		if removeErr := os.Remove(target); removeErr != nil {
			return fmt.Errorf("删除路径失败: %w", removeErr)
		}
		return nil
	}
	target, err := resolveRemotePath(item.RootPath, relative)
	if err != nil {
		return err
	}
	connection, _, err := dialSSH(ctx, item, false)
	if err != nil {
		return err
	}
	defer connection.Close()
	quotedTarget := shellQuote(target)
	command := remoteBoundaryCommand(item.RootPath, target,
		"if [ -L "+quotedTarget+" ]; then echo '不允许删除符号链接' >&2; exit 4; fi; "+
			"if [ \"$target\" = \"$root\" ]; then echo '不能删除工作区根目录' >&2; exit 4; fi; "+
			"if [ -d \"$target\" ]; then rmdir -- \"$target\"; else rm -- \"$target\"; fi")
	_, err = runSSH(ctx, connection.client, command, "", 30*time.Second)
	return err
}

// remoteCreateBoundaryCommand 校验新路径最近的现有父目录，允许创建尚不存在的多级目录。
func remoteCreateBoundaryCommand(root, target, command string) string {
	quotedRoot := shellQuote(root)
	quotedTarget := shellQuote(target)
	return "root=$(realpath -- " + quotedRoot + ") || exit 2; candidate=" + quotedTarget + "; parent=$(dirname -- \"$candidate\"); " +
		"while [ ! -e \"$parent\" ] && [ ! -L \"$parent\" ] && [ \"$parent\" != \"/\" ]; do parent=$(dirname -- \"$parent\"); done; " +
		"parent=$(realpath -- \"$parent\") || exit 2; case \"$parent\" in \"$root\"|\"$root\"/*) ;; *) echo '路径超出工作区' >&2; exit 3;; esac; " + command
}

func executeWorkspaceCommand(ctx context.Context, item Workspace, cwd, command string, timeoutSeconds int) (CommandResult, error) {
	return executeWorkspaceCommandForService(nil, ctx, item, cwd, command, timeoutSeconds, nil, "")
}

func (s *Service) executeWorkspaceCommand(ctx context.Context, item Workspace, cwd, command string, timeoutSeconds int, tty *TTYSpec, commandRunID string) (CommandResult, error) {
	return executeWorkspaceCommandForService(s, ctx, item, cwd, command, timeoutSeconds, tty, commandRunID)
}

func executeWorkspaceCommandForService(service *Service, ctx context.Context, item Workspace, cwd, command string, timeoutSeconds int, tty *TTYSpec, commandRunID string) (CommandResult, error) {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 60
	}
	timeout := time.Duration(timeoutSeconds) * time.Second
	if item.Type == TypeLocal {
		directory, err := resolveLocalPath(item.RootPath, cwd, true)
		if err != nil {
			return CommandResult{ExitCode: -1, StartFailed: true}, err
		}
		if tty != nil {
			if !ptySupported() {
				return CommandResult{ExitCode: -1, StartFailed: true}, ErrUnsupported
			}
			return startLocalPTYCommand(ctx, directory, command, *tty, timeout, commandRunID, service)
		}
		started := time.Now()
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.Command("sh", "-lc", command)
		cmd.Dir = directory
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		// 只传递构建工具常用的基础环境，避免把 Abot 的 API Key 等秘密交给项目命令。
		cmd.Env = workspaceCommandEnvironment()
		// 使用显式 pipe，避免 cmd.Wait 在读取 goroutine 排空前关闭
		// StdoutPipe/StderrPipe 的读取端，导致短命令的尾部输出丢失。
		stdoutRead, stdoutWrite, pipeErr := os.Pipe()
		if pipeErr != nil {
			return CommandResult{ExitCode: -1, StartFailed: true, DurationMS: time.Since(started).Milliseconds()}, fmt.Errorf("创建 stdout 管道失败: %w", pipeErr)
		}
		stderrRead, stderrWrite, pipeErr := os.Pipe()
		if pipeErr != nil {
			_ = stdoutRead.Close()
			_ = stdoutWrite.Close()
			return CommandResult{ExitCode: -1, StartFailed: true, DurationMS: time.Since(started).Milliseconds()}, fmt.Errorf("创建 stderr 管道失败: %w", pipeErr)
		}
		cmd.Stdout = stdoutWrite
		cmd.Stderr = stderrWrite
		if err = cmd.Start(); err != nil {
			_ = stdoutRead.Close()
			_ = stdoutWrite.Close()
			_ = stderrRead.Close()
			_ = stderrWrite.Close()
			return CommandResult{ExitCode: -1, StartFailed: true, DurationMS: time.Since(started).Milliseconds()}, fmt.Errorf("启动命令失败: %w", err)
		}
		// 父进程不写输出 pipe；启动后立即关闭写端，否则读端无法收到 EOF。
		_ = stdoutWrite.Close()
		_ = stderrWrite.Close()
		stdout := newObservedBuffer(ctx, "stdout")
		stderr := newObservedBuffer(ctx, "stderr")
		var drain sync.WaitGroup
		drain.Add(2)
		go drainCommandPipe(stdoutRead, stdout, &drain)
		go drainCommandPipe(stderrRead, stderr, &drain)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		waitErr := error(nil)
		timedOut := false
		unknown := false
		select {
		case waitErr = <-done:
		case <-runCtx.Done():
			timedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			select {
			case waitErr = <-done:
			case <-time.After(2 * time.Second):
				unknown = true
			}
		}
		if unknown {
			// 命令状态无法确认时主动解除读取阻塞，避免泄漏 drain goroutine。
			_ = stdoutRead.Close()
			_ = stderrRead.Close()
		}
		drain.Wait()
		_ = stdoutRead.Close()
		_ = stderrRead.Close()
		result := CommandResult{Output: stdout.String() + stderr.String(), Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitCode(waitErr), Truncated: stdout.Truncated() || stderr.Truncated(), TimedOut: timedOut, Unknown: unknown, Started: true, DurationMS: time.Since(started).Milliseconds(), StdoutBytes: stdout.BytesWritten(), StderrBytes: stderr.BytesWritten()}
		if unknown {
			return result, fmt.Errorf("命令结束状态未知")
		}
		if runCtx.Err() != nil {
			if timedOut {
				return result, fmt.Errorf("命令执行超时: %w", runCtx.Err())
			}
			return result, fmt.Errorf("命令执行已取消: %w", runCtx.Err())
		}
		if waitErr != nil {
			return result, fmt.Errorf("命令执行失败（退出码 %d）: %s", result.ExitCode, strings.TrimSpace(result.Output))
		}
		return result, nil
	}
	directory, err := resolveRemotePath(item.RootPath, cwd)
	if err != nil {
		return CommandResult{ExitCode: -1, StartFailed: true}, err
	}
	connection, _, err := dialSSH(ctx, item, false)
	if err != nil {
		return CommandResult{ExitCode: -1, StartFailed: true}, err
	}
	defer connection.Close()
	wrapped := remoteBoundaryCommand(item.RootPath, directory, "cd -- \"$target\" && "+command)
	return runSSH(ctx, connection.client, wrapped, "", timeout)
}

func workspaceCommandEnvironment() []string {
	keys := []string{"PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "SHELL", "TERM"}
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			result = append(result, key+"="+value)
		}
	}
	return result
}
