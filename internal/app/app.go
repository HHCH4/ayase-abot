package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/artifact"
	"Abot/internal/bootstrap"
	"Abot/internal/bot"
	"Abot/internal/builtintool/basic"
	configsvc "Abot/internal/config"
	"Abot/internal/conversation"
	"Abot/internal/httpapi"
	memorysvc "Abot/internal/memory"
	"Abot/internal/provider"
	"Abot/internal/provider/gemini"
	"Abot/internal/provider/openai"
	"Abot/internal/storage/sqlite"
	"Abot/internal/workspace"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/tool"
)

// Main 负责进程生命周期，业务装配细节集中在 Run 中。
func Main() {
	opts := bootstrap.Parse(os.Args[1:])
	if err := Run(opts); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("Abot 退出", "error", err)
		os.Exit(1)
	}
}

// Run 启动 SQLite、供应商注册表、Agent 内核和 HTTP/WebUI 服务。
func Run(opts bootstrap.Options) error {
	// LevelVar 让系统设置页可以在不重启进程的情况下切换日志等级。
	logLevel := new(slog.LevelVar)
	logLevel.Set(parseLevel(opts.LogLevel))
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)

	store, err := sqlite.Open(opts.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	dataDir := opts.DataDir
	if strings.TrimSpace(dataDir) == "" {
		dataDir = "./data"
	}
	artifactObjectStore, err := artifact.NewLocalContentStore(filepath.Join(dataDir, "artifacts"))
	if err != nil {
		return err
	}
	artifactService, err := artifact.NewService(store.ArtifactRepository(), artifactObjectStore)
	if err != nil {
		return err
	}

	adapters := map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: openai.NewAdapter(nil),
		provider.ProtocolGemini:           gemini.NewAdapter(nil),
	}
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), adapters)
	if err != nil {
		return err
	}
	configService, err := configsvc.NewService(store.ConfigRepository(), func(_ context.Context, providerID, _ string) error {
		if providerID == "" {
			return nil
		}
		if _, err := registry.Get(providerID); err != nil {
			return fmt.Errorf("默认供应商 %q 不存在", providerID)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := configService.EnsureDefault(context.Background()); err != nil {
		return err
	}
	personaService, err := configsvc.NewPersonaService(store.PersonaRepository())
	if err != nil {
		return err
	}
	if err := personaService.EnsureDefault(context.Background()); err != nil {
		return err
	}
	configService.SetSystemSettingsApplier(func(_ context.Context, settings configsvc.SystemSettings) error {
		logLevel.Set(parseLevel(settings.LogLevel))
		return applyArtifactMaintenancePolicy(artifactService, settings)
	})
	if settings, settingsErr := configService.GetSystemSettings(context.Background()); settingsErr != nil {
		return settingsErr
	} else {
		logLevel.Set(parseLevel(settings.LogLevel))
		if err := applyArtifactMaintenancePolicy(artifactService, settings); err != nil {
			return err
		}
	}
	workspaceService, err := workspace.NewService(store.WorkspaceRepository())
	if err != nil {
		return err
	}
	workspaceService.SetArtifactWriter(func(ctx context.Context, request artifact.PutRequest, reader io.Reader) (artifact.Artifact, error) {
		return artifactService.Put(ctx, request, reader)
	})
	remoteTargetService, err := workspace.NewRemoteTargetService(store.RemoteTargetRepository())
	if err != nil {
		return err
	}
	// 项目只保存远程主机 ID；实际 SSH 凭据在执行边界由远程主机服务解析。
	workspaceService.SetRemoteTargetResolver(remoteTargetService.Get)
	remoteTargetService.SetWorkspaceUsageCounter(workspaceService.CountByRemoteTarget)
	conversationService, err := conversation.NewService(store.ConversationRepository(), store.SessionService(), "abot")
	if err != nil {
		return err
	}
	conversationService.SetArtifactDeletionHook(artifactService.DeleteConversation)
	// 工作区删除和操作审批都服从对话生命周期：归档后不能继续批准操作，
	// 仍有关联对话时不能删除工作区。
	workspaceService.SetConversationPolicy(conversationService.CountByWorkspace, conversationService.IsActiveByID)
	longMemory, err := memorysvc.NewService(store.MemoryRepository(), "abot")
	if err != nil {
		return err
	}
	runtimeRepo := store.RuntimeRepository()
	basicTools, err := basic.Tools()
	if err != nil {
		return err
	}
	toolRegistry := agent.NewToolRegistry()
	// Assigned immediately after Kernel construction. The observer itself runs
	// on Runtime-owned turns, so it can publish digest-only conflict events
	// without coupling the Kernel to the concrete Coordinator type.
	var runtimeCoordinator *agentruntime.Coordinator
	validateInstructionSnapshot := func(validateCtx context.Context, invocationID string) error {
		invocation, getErr := runtimeRepo.GetInvocation(validateCtx, invocationID)
		if getErr != nil {
			return getErr
		}
		snapshotRepo, ok := runtimeRepo.(agentruntime.InstructionSnapshotRepository)
		if !ok {
			return nil
		}
		stored, getErr := snapshotRepo.GetInstructionSnapshotSet(validateCtx, invocationID)
		if errors.Is(getErr, agentruntime.ErrNotFound) {
			return nil
		}
		if getErr != nil {
			return getErr
		}
		if strings.TrimSpace(invocation.WorkspaceID) == "" {
			if len(stored.Snapshots) == 0 {
				return nil
			}
			return fmt.Errorf("Invocation 已取消 Workspace，但仍存在项目指令快照")
		}
		current, discoverErr := workspaceService.DiscoverInstructions(validateCtx, invocation.WorkspaceID, stored.TargetPath)
		if discoverErr != nil {
			return discoverErr
		}
		if !instructionSnapshotsMatch(stored.Snapshots, current) {
			if runtimeCoordinator != nil {
				changed := make([]agentruntime.InstructionSnapshot, 0, len(current))
				for _, item := range current {
					changed = append(changed, agentruntime.InstructionSnapshot{Path: item.Path, ScopePath: item.ScopePath, Source: item.Source, ContentDigest: item.ContentDigest, Priority: item.Priority})
				}
				if recordErr := runtimeCoordinator.RecordInstructionConflict(validateCtx, invocationID, stored.Snapshots, changed, "project_instruction_changed_before_approval"); recordErr != nil {
					return recordErr
				}
			}
			return fmt.Errorf("项目指令文件或 digest 已变化")
		}
		return nil
	}
	reconfirmInstructionSnapshot := func(reconfirmCtx context.Context, invocationID string) (agentruntime.InstructionSnapshotSet, error) {
		invocation, getErr := runtimeRepo.GetInvocation(reconfirmCtx, invocationID)
		if getErr != nil {
			return agentruntime.InstructionSnapshotSet{}, getErr
		}
		snapshotRepo, ok := runtimeRepo.(agentruntime.InstructionSnapshotRepository)
		if !ok {
			return agentruntime.InstructionSnapshotSet{}, errors.New("当前 Runtime 存储未启用 InstructionSnapshot")
		}
		stored, getErr := snapshotRepo.GetInstructionSnapshotSet(reconfirmCtx, invocationID)
		if getErr != nil && !errors.Is(getErr, agentruntime.ErrNotFound) {
			return agentruntime.InstructionSnapshotSet{}, getErr
		}
		targetPath := stored.TargetPath
		if strings.TrimSpace(invocation.WorkspaceID) == "" {
			return snapshotRepo.ReplaceInstructionSnapshotSet(reconfirmCtx, agentruntime.InstructionSnapshotSet{InvocationID: invocationID, TargetPath: targetPath, Snapshots: nil})
		}
		current, discoverErr := workspaceService.DiscoverInstructions(reconfirmCtx, invocation.WorkspaceID, targetPath)
		if discoverErr != nil {
			return agentruntime.InstructionSnapshotSet{}, discoverErr
		}
		snapshots := make([]agentruntime.InstructionSnapshot, 0, len(current))
		for _, item := range current {
			snapshots = append(snapshots, agentruntime.InstructionSnapshot{
				Path: item.Path, ScopePath: item.ScopePath, Source: item.Source, ContentDigest: item.ContentDigest,
				Content: item.Content, Priority: item.Priority, LoadedAt: time.Now().UTC(),
			})
		}
		return snapshotRepo.ReplaceInstructionSnapshotSet(reconfirmCtx, agentruntime.InstructionSnapshotSet{InvocationID: invocationID, TargetPath: targetPath, Snapshots: snapshots})
	}

	kernel, err := agent.NewKernel(agent.Config{
		AppName:        "abot",
		SessionService: store.SessionService(),
		Providers:      registry,
		Conversations:  conversationService,
		Instruction:    "你是 Abot，一个可靠、简洁、遵守用户意图的中文 AI 助手。",
		Tools:          basicTools,
		ToolRegistry:   toolRegistry,
		WorkspaceTools: func(ctx context.Context, workspaceID string) ([]tool.Tool, error) {
			return workspaceService.Tools(ctx, workspaceID)
		},
		WorkspaceToolsForConversation: func(ctx context.Context, workspaceID, conversationID string) ([]tool.Tool, error) {
			return workspaceService.Tools(ctx, workspaceID, conversationID)
		},
		WorkspaceToolsForConversationWithPolicy: func(ctx context.Context, workspaceID, conversationID string, policy agent.WorkspacePolicy) ([]tool.Tool, error) {
			return workspaceService.ToolsWithPolicy(ctx, workspaceID, conversationID, workspace.ToolPolicy{
				Enabled: policy.Enabled, ReadEnabled: policy.ReadEnabled, WriteEnabled: policy.WriteEnabled,
				ExecEnabled: policy.ExecEnabled, GitEnabled: policy.GitEnabled, CommandTimeoutSeconds: policy.CommandTimeoutSeconds,
			})
		},
		ProjectInstructionResolver: func(ctx context.Context, workspaceID, targetPath string) ([]agent.ProjectInstruction, error) {
			snapshots, err := workspaceService.DiscoverInstructions(ctx, workspaceID, targetPath)
			if err != nil {
				return nil, err
			}
			result := make([]agent.ProjectInstruction, 0, len(snapshots))
			for _, snapshot := range snapshots {
				result = append(result, agent.ProjectInstruction{
					Path: snapshot.Path, ScopePath: snapshot.ScopePath, Source: snapshot.Source,
					ContentDigest: snapshot.ContentDigest, Content: snapshot.Content, Priority: snapshot.Priority,
				})
			}
			return result, nil
		},
		ProjectInstructionObserver: func(ctx context.Context, invocationID, targetPath string, items []agent.ProjectInstruction) error {
			repository, ok := runtimeRepo.(agentruntime.InstructionSnapshotRepository)
			if !ok {
				return nil
			}
			previous, previousErr := repository.GetInstructionSnapshotSet(ctx, invocationID)
			if previousErr != nil && !errors.Is(previousErr, agentruntime.ErrNotFound) {
				return previousErr
			}
			snapshots := make([]agentruntime.InstructionSnapshot, 0, len(items))
			for _, item := range items {
				snapshots = append(snapshots, agentruntime.InstructionSnapshot{
					Path: item.Path, ScopePath: item.ScopePath, Source: item.Source,
					ContentDigest: item.ContentDigest, Content: item.Content, Priority: item.Priority,
					LoadedAt: time.Now().UTC(),
				})
			}
			if previousErr == nil && (strings.TrimSpace(previous.TargetPath) != strings.TrimSpace(targetPath) || !runtimeInstructionSnapshotsMatch(previous.Snapshots, snapshots)) && runtimeCoordinator != nil {
				if err := runtimeCoordinator.RecordInstructionConflict(ctx, invocationID, previous.Snapshots, snapshots, "project_instruction_changed"); err != nil {
					return err
				}
			}
			_, err := repository.ReplaceInstructionSnapshotSet(ctx, agentruntime.InstructionSnapshotSet{InvocationID: invocationID, TargetPath: strings.TrimSpace(targetPath), Snapshots: snapshots})
			return err
		},
		TaskContractResolver: func(ctx context.Context, invocationID string) (agent.TaskContractProjection, error) {
			repository, ok := runtimeRepo.(agentruntime.TaskContractRepository)
			if !ok {
				return agent.TaskContractProjection{}, fmt.Errorf("当前 Runtime 存储未启用 TaskContract")
			}
			contract, err := repository.GetTaskContract(ctx, invocationID)
			if errors.Is(err, agentruntime.ErrNotFound) {
				invocation, invocationErr := runtimeRepo.GetInvocation(ctx, invocationID)
				if invocationErr != nil {
					return agent.TaskContractProjection{}, invocationErr
				}
				contract = agentruntime.InitialTaskContract(invocation.ID, invocation.Message, invocation.CreatedAt)
				err = repository.CreateTaskContract(ctx, contract)
				if errors.Is(err, agentruntime.ErrConflict) {
					contract, err = repository.GetTaskContract(ctx, invocationID)
				}
			}
			if err != nil {
				return agent.TaskContractProjection{}, err
			}
			criteria := make([]agent.TaskCriterionProjection, 0, len(contract.AcceptanceCriteria))
			for _, criterion := range contract.AcceptanceCriteria {
				criteria = append(criteria, agent.TaskCriterionProjection{ID: criterion.ID, Description: criterion.Description})
			}
			constraints := make([]string, 0, len(contract.Constraints))
			for _, constraint := range contract.Constraints {
				constraints = append(constraints, constraint.Description)
			}
			externalActions := make([]string, 0, len(contract.ExternalActions))
			for _, action := range contract.ExternalActions {
				state := "denied"
				if action.Allowed {
					state = "allowed"
				}
				externalActions = append(externalActions, state+": "+action.Action)
			}
			return agent.TaskContractProjection{
				InvocationID: contract.InvocationID, Version: contract.Version, TaskType: string(contract.TaskType), Goal: contract.Goal,
				RequestedOutcome: contract.RequestedOutcome, AcceptanceCriteria: criteria, Constraints: constraints,
				NonGoals: contract.NonGoals, MutationAllowed: contract.MutationAllowed,
				ValidationRequired: contract.ValidationRequired, ExternalActions: externalActions,
				SourceMessageIDs: contract.SourceMessageIDs,
			}, nil
		},
		MemoryService: longMemory,
		MemoryServiceResolver: func(_ context.Context, runtime agent.RuntimeOptions) adkmemory.Service {
			return longMemory.WithLimit(runtime.MemoryMaxResults)
		},
		MemoryTools: func(_ context.Context, _ string) ([]tool.Tool, error) {
			return longMemory.Tools()
		},
		AttachmentResolver: func(ctx context.Context, userID string, ref agent.AttachmentRef) (io.ReadCloser, error) {
			item, err := artifactService.Get(ctx, userID, ref.ID)
			if err != nil {
				return nil, err
			}
			if item.Status != artifact.StatusReady || item.Version != ref.Version {
				return nil, fmt.Errorf("artifact ref 版本或状态不匹配")
			}
			if strings.TrimSpace(ref.Digest) != "" && ref.Digest != item.Digest {
				return nil, fmt.Errorf("artifact ref digest 不匹配")
			}
			if ref.Size > 0 && ref.Size != item.Size {
				return nil, fmt.Errorf("artifact ref size 不匹配")
			}
			reader, _, _, err := artifactService.Open(ctx, userID, item.ID, artifact.ByteRange{})
			return reader, err
		},
		AttachmentListResolver: func(ctx context.Context, userID, invocationID string) ([]agent.Attachment, error) {
			item, err := runtimeRepo.GetInvocation(ctx, strings.TrimSpace(invocationID))
			if err != nil {
				return nil, err
			}
			if item.UserID != strings.TrimSpace(userID) {
				return nil, errors.New("当前用户无权读取该任务附件")
			}
			attachments := make([]agent.Attachment, len(item.Attachments))
			for index, attachment := range item.Attachments {
				if attachment.Ref == nil || len(attachment.Data) > 0 {
					return nil, fmt.Errorf("第 %d 个任务附件不是受保护的 Artifact ref", index+1)
				}
				attachments[index] = attachment
				ref := *attachment.Ref
				attachments[index].Ref = &ref
			}
			return attachments, nil
		},
		RuntimeConfigResolver: func(ctx context.Context, botID, userID, conversationID string) (agent.RuntimeOptions, error) {
			runtime, err := configService.Resolve(ctx, botID, conversationID)
			if err != nil {
				return agent.RuntimeOptions{}, err
			}
			runtime, err = resolvePersonaRuntime(ctx, personaService, botID, conversationID, runtime)
			if err != nil {
				return agent.RuntimeOptions{}, err
			}
			settings, settingsErr := configService.GetSystemSettings(ctx)
			if settingsErr != nil {
				return agent.RuntimeOptions{}, settingsErr
			}
			return agent.RuntimeOptions{
				AIEnabled: runtime.AIEnabled, ProviderID: runtime.ProviderID, ModelID: runtime.ModelID,
				AITemperature: runtime.AITemperature, AITopP: runtime.AITopP, AIMaxOutputTokens: runtime.AIMaxOutputTokens, AIRequestRetries: runtime.AIRequestRetries,
				PersonaID: runtime.PersonaID, Instruction: runtime.Instruction,
				CompactionEnabled: runtime.CompactionEnabled, CompactionRatio: runtime.CompactionRatio,
				CompactionSafetyTokens: runtime.CompactionSafetyTokens, CompactionRetentionEvents: runtime.CompactionRetentionEvents,
				CompactionInterval: runtime.CompactionInterval, CompactionOverlap: runtime.CompactionOverlap,
				CompactionUnknownWindowTokens: runtime.CompactionUnknownWindowTokens, AgentMaxToolCalls: runtime.AgentMaxToolCalls, ToolSchemaBudgetTokens: runtime.ToolSchemaBudgetTokens,
				WorkspaceEnabled: runtime.WorkspaceEnabled, WorkspaceReadEnabled: runtime.WorkspaceReadEnabled,
				WorkspaceWriteEnabled: runtime.WorkspaceWriteEnabled, WorkspaceExecEnabled: runtime.WorkspaceExecEnabled,
				WorkspaceGitEnabled: runtime.WorkspaceGitEnabled, WorkspaceCommandTimeoutSecs: runtime.WorkspaceCommandTimeoutSecs,
				MessageStreamingEnabled: runtime.MessageStreamingEnabled, MessagePromptPrefix: runtime.MessagePromptPrefix,
				MemoryEnabled: runtime.MemoryEnabled, MemoryAutoRetrieve: runtime.MemoryAutoRetrieve, MemoryMaxResults: runtime.MemoryMaxResults,
				ModalFallbackEnabled: settings.ModalFallbackEnabled, ModalFallbackProviderID: settings.ModalFallbackProviderID,
				ModalFallbackVisionModel: settings.ModalFallbackVisionModel, ModalFallbackAudioModel: settings.ModalFallbackAudioModel,
			}, nil
		},
		EnableCompaction: true,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runtimeCoordinator, err = agentruntime.NewCoordinator(kernel, runtimeRepo)
	if err != nil {
		return err
	}
	runtimeCoordinator.SetWorkspaceBaselineResolver(func(baselineCtx context.Context, workspaceID, targetPath string) (agentruntime.WorktreeBaseline, error) {
		captured, captureErr := workspaceService.CaptureBaseline(baselineCtx, workspaceID)
		if captureErr != nil {
			return agentruntime.WorktreeBaseline{}, captureErr
		}
		paths := make([]agentruntime.PathStatus, 0, len(captured.ChangedPaths))
		for _, item := range captured.ChangedPaths {
			paths = append(paths, agentruntime.PathStatus{Path: item.Path, Status: item.Status})
		}
		return agentruntime.WorktreeBaseline{
			WorkspaceID: workspaceID, TargetPath: strings.TrimSpace(targetPath), RepositoryType: captured.RepositoryType,
			HeadRevision: captured.HeadRevision, Branch: captured.Branch, StatusDigest: captured.StatusDigest,
			ChangedPaths: paths, StatusKnown: captured.StatusKnown, Truncated: captured.Truncated, CapturedAt: captured.CapturedAt,
		}, nil
	})
	runtimeCoordinator.SetWorkspaceOperationResolver(func(operationCtx context.Context, workspaceID, invocationID string) ([]workspace.Operation, error) {
		items, listErr := workspaceService.ListOperations(operationCtx, workspaceID)
		if listErr != nil {
			return nil, listErr
		}
		filtered := make([]workspace.Operation, 0, len(items))
		for _, item := range items {
			if strings.TrimSpace(item.InvocationID) == strings.TrimSpace(invocationID) {
				filtered = append(filtered, item)
			}
		}
		return filtered, nil
	})
	// Approved verification commands are projected back into the Runtime plan
	// after the Workspace operation is durably saved. The callback is
	// metadata-only and never executes a second command.
	workspaceService.SetOperationAdmissionValidator(func(admissionCtx context.Context, request workspace.OperationRequest) error {
		return runtimeCoordinator.ValidateWorkspaceOperationAdmission(admissionCtx, request)
	})
	workspaceService.SetOperationObserver(func(observerCtx context.Context, operation workspace.Operation) {
		runtimeCoordinator.RecordWorkspaceOperation(observerCtx, operation)
	})
	// CommandRun is an independent, durable execution checkpoint. Runtime only
	// projects its metadata into the Invocation event stream; it never retries
	// a process from this callback.
	workspaceService.SetCommandRunObserver(func(observerCtx context.Context, run workspace.CommandRun) {
		runtimeCoordinator.RecordCommandRun(observerCtx, run)
	})
	if err := workspaceService.ReconcileCommandRuns(ctx); err != nil {
		return fmt.Errorf("恢复命令运行检查点失败: %w", err)
	}
	// CommandRun metadata is durable, while replayable output follows the
	// bounded workspace retention policy. Cleanup is startup-triggered and
	// idempotent; it never re-executes a command or emits a duplicate lifecycle
	// event for an already completed process.
	if _, err := workspaceService.PurgeExpiredCommandOutput(ctx, time.Now().UTC()); err != nil {
		return fmt.Errorf("清理过期命令日志失败: %w", err)
	}
	// Artifact 维护同样是启动触发的幂等回收：关闭未完成上传、重试对象删除、
	// 回收无引用对象。它只影响可回收空间，不影响已有内容的正确性，因此失败
	// 记录警告而不阻止启动，下一次启动或显式维护请求会重试。
	if result, maintenanceErr := artifactService.RunMaintenance(ctx, time.Now().UTC()); maintenanceErr != nil {
		slog.Warn("Artifact 维护未完成", "error", maintenanceErr)
	} else {
		slog.Info("Artifact 维护完成",
			"uploads_failed", result.UploadSweep.UploadsFailed,
			"deletions_closed", result.UploadSweep.DeletionsClosed,
			"objects_deleted", result.GarbageCollection.Deleted,
			"bytes_reclaimed", result.GarbageCollection.ReclaimedBytes)
	}
	if _, ok := runtimeRepo.(agentruntime.InstructionSnapshotRepository); ok {
		runtimeCoordinator.SetInstructionSnapshotValidator(validateInstructionSnapshot)
		runtimeCoordinator.SetInstructionSnapshotReconfirmer(reconfirmInstructionSnapshot)
	}
	workspaceService.SetInstructionWriteValidator(validateInstructionSnapshot)
	// SQLite implements ApprovalRejectionCommitRepository, so Runtime owns the
	// Operation/queued-CommandRun rejection in the same local transaction. Keep
	// the callback only for older embedders whose repository cannot provide that
	// cross-table boundary.
	if _, atomicRejection := runtimeRepo.(agentruntime.ApprovalRejectionCommitRepository); !atomicRejection {
		runtimeCoordinator.SetApprovalDecisionHandler(func(decisionCtx context.Context, approval agentruntime.Approval, approved bool) error {
			if approved || approval.OperationID == "" {
				return nil
			}
			_, err := workspaceService.RejectOperation(decisionCtx, approval.OperationID)
			return err
		})
	}
	runtimeCoordinator.SetApprovalCancellationHandler(func(cancelCtx context.Context, approval agentruntime.Approval) error {
		if approval.OperationID == "" {
			return nil
		}
		_, err := workspaceService.CancelOperation(cancelCtx, approval.OperationID)
		return err
	})
	runtimeCoordinator.SetApprovalExpiryHandler(func(expireCtx context.Context, approval agentruntime.Approval) error {
		if approval.OperationID == "" {
			return nil
		}
		_, err := workspaceService.ExpireOperation(expireCtx, approval.OperationID)
		return err
	})
	if err := runtimeCoordinator.Start(ctx); err != nil {
		return err
	}
	defer func() { _ = runtimeCoordinator.Close() }()

	botManager, err := bot.NewManager(ctx, store.BotRepository(), kernel, conversationService)
	if err != nil {
		return err
	}
	botManager.SetRequestTimeoutResolver(func(ctx context.Context) (time.Duration, error) {
		settings, err := configService.GetSystemSettings(ctx)
		if err != nil {
			return 0, err
		}
		return time.Duration(settings.RequestTimeoutSeconds) * time.Second, nil
	})
	botManager.SetRuntimeCoordinator(runtimeCoordinator)
	// Chat commands read and (where supported) change configuration through the
	// same services the WebUI uses.
	commandBridge := &commandRuntimeBridge{
		providers: registry, config: configService, personas: personaService,
		workspaces: workspaceService, conversations: conversationService,
	}
	botManager.SetCommandRuntimeInfo(commandBridge)
	botManager.SetCommandRuntimeAdmin(commandBridge)
	botManager.SetAttachmentStorer(func(storeCtx context.Context, request bot.AttachmentStoreRequest) (agent.Attachment, error) {
		input := request.Attachment
		if input.Ref != nil {
			if len(input.Data) > 0 {
				return agent.Attachment{}, fmt.Errorf("附件不能同时包含 ref 和 inline data")
			}
			return input, nil
		}
		if len(input.Data) == 0 {
			return agent.Attachment{}, fmt.Errorf("附件内容不能为空")
		}
		stored, err := artifactService.Put(storeCtx, artifact.PutRequest{
			UserID: request.UserID, ConversationID: request.ConversationID,
			ProducerType: request.ProducerType, ProducerID: request.ProducerID,
			Kind: artifact.KindInputAttachment, Name: input.Name, MIMEType: input.MIMEType,
			MaxBytes: 10 << 20,
		}, bytes.NewReader(input.Data))
		if err != nil {
			return agent.Attachment{}, err
		}
		ref := agent.AttachmentRef{ID: stored.ID, Version: stored.Version, Digest: stored.Digest, Kind: string(stored.Kind), MIMEType: stored.MIMEType, Size: stored.Size, Name: stored.Name}
		return agent.Attachment{Name: stored.Name, MIMEType: stored.MIMEType, Ref: &ref}, nil
	})
	defer func() { _ = botManager.Close() }()
	if err := botManager.Start(ctx); err != nil {
		return err
	}

	server := httpapi.NewServerWithServices(registry, kernel, workspaceService, conversationService, botManager)
	server.SetToolRegistry(toolRegistry)
	server.SetRuntimeCoordinator(runtimeCoordinator)
	server.SetConfigService(configService)
	server.SetPersonaService(personaService)
	server.SetRemoteTargetService(remoteTargetService)
	server.SetMemoryService(longMemory)
	server.SetArtifactService(artifactService)
	httpServer := &http.Server{
		Addr:              opts.HTTPAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	slog.Info("Abot WebUI 已启动", "addr", opts.HTTPAddr, "data_dir", opts.DataDir)
	return httpServer.ListenAndServe()
}

// resolvePersonaRuntime applies the catalog selection layer without changing
// legacy profiles that only contain persona.system_prompt.
func resolvePersonaRuntime(ctx context.Context, service *configsvc.PersonaService, botID, conversationID string, runtime configsvc.Runtime) (configsvc.Runtime, error) {
	if service == nil {
		return runtime, nil
	}
	fallbackID := runtime.PersonaID
	// New/default profiles retain an empty persona.id for old-schema
	// compatibility. If the instruction is still the built-in default, follow
	// the current catalog default rather than hard-coding the bootstrap ID.
	if fallbackID == "" && runtime.Instruction == configsvc.DefaultPersonaInstruction {
		var err error
		fallbackID, err = service.DefaultPersonaID(ctx)
		if err != nil {
			return configsvc.Runtime{}, err
		}
	}
	persona, selected, err := service.ResolveSelection(ctx, botID, conversationID, fallbackID)
	if err != nil {
		return configsvc.Runtime{}, err
	}
	if selected {
		runtime.PersonaID = persona.ID
		runtime.Instruction = persona.Instruction
	}
	return runtime, nil
}

func instructionSnapshotsMatch(stored []agentruntime.InstructionSnapshot, current []workspace.InstructionSnapshot) bool {
	if len(stored) != len(current) {
		return false
	}
	for index, snapshot := range stored {
		item := current[index]
		if snapshot.Path != item.Path || snapshot.ScopePath != item.ScopePath || snapshot.Source != item.Source || snapshot.ContentDigest != item.ContentDigest || snapshot.Priority != item.Priority {
			return false
		}
	}
	return true
}

func runtimeInstructionSnapshotsMatch(left, right []agentruntime.InstructionSnapshot) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		a, b := left[index], right[index]
		if a.Path != b.Path || a.ScopePath != b.ScopePath || a.Source != b.Source || a.ContentDigest != b.ContentDigest || a.Priority != b.Priority {
			return false
		}
	}
	return true
}

// applyArtifactMaintenancePolicy 把系统设置映射为本地 Artifact 维护策略。
// 范围校验由配置服务负责，这里只做单位换算，其余字段保留存储层默认值。
func applyArtifactMaintenancePolicy(service *artifact.Service, settings configsvc.SystemSettings) error {
	if service == nil {
		return nil
	}
	policy := artifact.DefaultMaintenancePolicy()
	policy.QuotaBytes = settings.ArtifactQuotaBytes
	if settings.ArtifactStaleUploadSeconds > 0 {
		policy.StaleUploadAge = time.Duration(settings.ArtifactStaleUploadSeconds) * time.Second
	}
	return service.SetMaintenancePolicy(policy)
}

func parseLevel(value string) slog.Level {
	switch value {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
