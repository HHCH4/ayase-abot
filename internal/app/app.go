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
	"Abot/internal/logging"
	memorysvc "Abot/internal/memory"
	"Abot/internal/provider"
	"Abot/internal/provider/gemini"
	"Abot/internal/provider/openai"
	"Abot/internal/schedule"
	"Abot/internal/sessionrule"
	"Abot/internal/storage/sqlite"
	"Abot/internal/websearch"
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
	// 管理台日志页读取有界的完整内存快照，终端与管理台展示同一份结构化日志。
	logStore := logging.NewStore(2000)
	logger := slog.New(logging.NewHandler(logStore, slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})))
	slog.SetDefault(logger)

	store, err := sqlite.Open(opts.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	// 复用同一个机器人仓储实例装配来源目录、别名和机器人生命周期，确保
	// WebUI、消息入口与批量规则看到的是同一份 UMO 数据。
	botRepository := store.BotRepository()
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
	webSearchManager, err := websearch.NewManager(store.WebSearchRepository(), nil)
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
	sessionRuleService, err := sessionrule.NewService(store.SessionRuleRepository())
	if err != nil {
		return err
	}
	if sourceRegistry, ok := botRepository.(bot.MessageSourceRegistry); ok {
		// 批量规则需要对“已知来源”而不是仅对“已有规则”执行；这里保持
		// sessionrule 包只依赖窄回调，不把 SQLite 或 Bot 实现泄漏进去。
		sessionRuleService.SetSourceLister(func(ctx context.Context, query string) ([]string, error) {
			items, listErr := sourceRegistry.ListMessageSources(ctx, query)
			if listErr != nil {
				return nil, listErr
			}
			sources := make([]string, 0, len(items))
			for _, item := range items {
				if source := strings.TrimSpace(item.Source); source != "" {
					sources = append(sources, source)
				}
			}
			if aliasLister, aliasOK := botRepository.(bot.SourceNameLister); aliasOK {
				aliases, aliasErr := aliasLister.ListSourceNames(ctx, query)
				if aliasErr != nil {
					return nil, aliasErr
				}
				for _, alias := range aliases {
					if source := strings.TrimSpace(alias.Source); source != "" {
						sources = append(sources, source)
					}
				}
			}
			return sources, nil
		})
	}
	scheduleService, err := schedule.NewService(store.ScheduleRepository())
	if err != nil {
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
	// 会话列表复用 /name 的来源名称仓储，让聊天指令设置的别名能在 WebUI 中显示。
	if sourceNames, ok := botRepository.(conversation.SourceNameResolver); ok {
		conversationService.SetSourceNameResolver(sourceNames)
	}
	conversationService.SetArtifactDeletionHook(artifactService.DeleteConversation)
	// Chat commands may bind a workspace to a conversation; the conversation
	// service validates the target through this narrow hook instead of
	// depending on the workspace service.
	conversationService.SetWorkspaceValidator(func(validateCtx context.Context, workspaceID string) error {
		item, getErr := workspaceService.Get(validateCtx, strings.TrimSpace(workspaceID))
		if getErr != nil {
			return getErr
		}
		if !item.Enabled {
			return fmt.Errorf("工作区 %s 已停用", item.Name)
		}
		return nil
	})
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
		WebSearchToolFactory: func(_ context.Context, runtime agent.RuntimeOptions, invocationID, conversationID string) (tool.Tool, error) {
			return webSearchManager.NewTool(runtime.WebSearchServiceIDs, invocationID, conversationID, websearch.Budget{
				DailyCallLimit: int64(runtime.WebSearchDailyCallLimit), MaxCallsPerInvocation: runtime.WebSearchMaxCallsPerInvocation,
				AlertPercent: runtime.WebSearchAlertPercent,
			})
		},
		ToolRegistry: toolRegistry,
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
			// A per-conversation override wins over the resolved default, so a
			// chat can switch models without changing the bot or global config.
			source := conversationID
			var conversationSubAgentEnabled *bool
			if item, getErr := conversationService.Get(ctx, userID, conversationID); getErr == nil {
				if strings.TrimSpace(item.Source) != "" {
					source = item.Source
				}
				if override := strings.TrimSpace(item.ProviderID); override != "" {
					runtime.ProviderID = override
				}
				if override := strings.TrimSpace(item.ModelID); override != "" {
					runtime.ModelID = override
				}
				if item.SubAgentEnabled != nil {
					enabled := *item.SubAgentEnabled
					conversationSubAgentEnabled = &enabled
				}
			}
			// 会话规则优先于全局配置，但只能覆盖内置 Runtime 已支持的模型、人格和启停状态。
			rule, found, ruleErr := sessionRuleService.Resolve(ctx, source)
			if ruleErr == nil && !found && source != conversationID {
				// 兼容旧会话：升级前没有 Source 字段时，仍允许直接用 conversation ID 配规则。
				rule, found, ruleErr = sessionRuleService.Resolve(ctx, conversationID)
			}
			if ruleErr != nil {
				return agent.RuntimeOptions{}, ruleErr
			} else if found {
				if rule.HasOverride("profile_id") && rule.HasOverride("follow_profile") && rule.ProfileID != "" && rule.FollowProfile {
					profileRuntime, profileErr := configService.RuntimeForProfile(ctx, rule.ProfileID)
					if profileErr != nil {
						return agent.RuntimeOptions{}, profileErr
					}
					runtime = profileRuntime
				}
				if (rule.HasOverride("process_enabled") && !rule.ProcessEnabled) || (rule.HasOverride("llm_enabled") && !rule.LLMEnabled) {
					runtime.AIEnabled = false
				}
				// 模型覆盖放在配置文件切换之后，确保会话规则具有最终优先级。
				if rule.HasOverride("chat_model") && rule.ChatModel != "" {
					runtime.ModelID = rule.ChatModel
				}
				if rule.HasOverride("persona_id") && rule.PersonaID != "" {
					runtime.PersonaID = rule.PersonaID
				}
			}
			if runtime.AIExecutionMode != configsvc.BuiltinAIExecutionMode {
				return agent.RuntimeOptions{}, fmt.Errorf("仅支持内置 AI 执行方式")
			}
			runtime, err = resolvePersonaRuntime(ctx, personaService, botID, conversationID, runtime)
			if err != nil {
				return agent.RuntimeOptions{}, err
			}
			settings, settingsErr := configService.GetSystemSettings(ctx)
			if settingsErr != nil {
				return agent.RuntimeOptions{}, settingsErr
			}
			subAgentEnabled := settings.IsSubAgentEnabled()
			if conversationSubAgentEnabled != nil {
				subAgentEnabled = *conversationSubAgentEnabled
			}
			subAgentEnabledValue := subAgentEnabled
			return agent.RuntimeOptions{
				AIEnabled: runtime.AIEnabled, ProviderID: runtime.ProviderID, ModelID: runtime.ModelID,
				AITemperature: runtime.AITemperature, AIReasoningEffort: runtime.AIReasoningEffort, AITopP: runtime.AITopP, AIMaxOutputTokens: runtime.AIMaxOutputTokens, AIRequestRetries: runtime.AIRequestRetries,
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
				WebSearchEnabled: runtime.WebSearchEnabled, WebSearchServiceIDs: append([]string(nil), runtime.WebSearchServiceIDs...),
				WebSearchDailyCallLimit: settings.WebSearchDailyCallLimit, WebSearchMaxCallsPerInvocation: settings.WebSearchMaxCallsPerInvocation,
				WebSearchAlertPercent: settings.WebSearchAlertPercent,
				ModalFallbackEnabled:  settings.ModalFallbackEnabled, ModalFallbackProviderID: settings.ModalFallbackProviderID,
				ModalFallbackVisionModel: settings.ModalFallbackVisionModel, ModalFallbackAudioModel: settings.ModalFallbackAudioModel,
				SubAgentEnabled: &subAgentEnabledValue,
				SubAgent:        subAgentRuntimeSettings(settings),
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
	// 新版只保留通用 generic 子 Agent；清理旧 profile 的编排历史，避免旧的
	// 独立超时和职责标签继续影响管理台展示，但不触碰主任务或会话数据。
	// 父 Invocation 的总超时由配置中心统一决定；子 Agent 只继承这个 Context，
	// 不再在子任务层设置 120/300 秒的独立截止时间。
	runtimeCoordinator.SetInvocationTimeoutResolver(func(timeoutCtx context.Context, _ agentruntime.Invocation) (time.Duration, error) {
		settings, settingsErr := configService.GetSystemSettings(timeoutCtx)
		if settingsErr != nil {
			return 0, settingsErr
		}
		return time.Duration(settings.RequestTimeoutSeconds) * time.Second, nil
	})
	// 删除会话前先让 Runtime 终止该会话的排队和运行中任务，保护其附件引用直到任务收尾完成。
	conversationService.SetInvocationDeletionHook(func(deleteCtx context.Context, userID, conversationID string) error {
		if runtimeCoordinator == nil {
			return nil
		}
		return runtimeCoordinator.CancelConversationInvocations(deleteCtx, userID, conversationID)
	})
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
	// Artifact 维护先在启动时执行一次幂等回收：删除过期附件、关闭未完成上传、
	// 重试对象删除并回收无引用对象。它只影响可回收空间，不影响已有内容的正确性，
	// 因此失败记录警告而不阻止启动，后续定时循环或显式维护请求会重试。
	if result, maintenanceErr := artifactService.RunMaintenance(ctx, time.Now().UTC()); maintenanceErr != nil {
		slog.Warn("Artifact 维护未完成", "error", maintenanceErr)
	} else {
		slog.Info("Artifact 维护完成",
			"expired_deleted", result.ExpiredArtifacts.Deleted,
			"uploads_failed", result.UploadSweep.UploadsFailed,
			"deletions_closed", result.UploadSweep.DeletionsClosed,
			"objects_deleted", result.GarbageCollection.Deleted,
			"bytes_reclaimed", result.GarbageCollection.ReclaimedBytes)
	}
	// 启动维护只处理历史积压；定时循环负责后续过期附件和对象删除，避免
	// 进程长期运行时原始文档一直占用磁盘。每轮都有独立超时，不阻塞关停。
	go runArtifactMaintenanceLoop(ctx, artifactService)
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

	botManager, err := bot.NewManager(ctx, botRepository, kernel, conversationService)
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
	// 平台级管理员和唤醒词与 Agent 配置共用同一份配置文件解析结果，避免 WebUI 保存后消息入口仍使用旧逻辑。
	botManager.SetMessageConfigResolver(func(resolveCtx context.Context, botID, conversationID string) (bot.MessageConfig, error) {
		runtime, resolveErr := configService.Resolve(resolveCtx, botID, conversationID)
		if resolveErr != nil {
			return bot.MessageConfig{}, resolveErr
		}
		return bot.MessageConfig{
			AdminUserIDs:          append([]string(nil), runtime.PlatformAdminIDs...),
			WakeupWords:           append([]string(nil), runtime.WakeupWords...),
			PrivateRequiresWakeup: runtime.PrivateRequiresWakeup,
			// 平台设置按消息热读取，管理员、白名单、发送样式和限速同步生效。
			Platform: bot.PlatformConfig{
				UniqueSession: runtime.Platform.UniqueSession, ReplyPrefix: runtime.Platform.ReplyPrefix,
				ReplyMention: runtime.Platform.ReplyMention, ReplyQuote: runtime.Platform.ReplyQuote,
				PrivateReplyQuote: runtime.Platform.PrivateReplyQuote,
				WhitelistEnabled:  runtime.Platform.WhitelistEnabled, WhitelistIDs: append([]string(nil), runtime.Platform.WhitelistIDs...),
				WhitelistLog: runtime.Platform.WhitelistLog, WhitelistAdminGroup: runtime.Platform.WhitelistAdminGroup,
				WhitelistAdminPrivate: runtime.Platform.WhitelistAdminPrivate, RateLimitSeconds: runtime.Platform.RateLimitSeconds,
				RateLimitCount: runtime.Platform.RateLimitCount, RateLimitStrategy: runtime.Platform.RateLimitStrategy,
				IgnoreBotSelfMessage: runtime.Platform.IgnoreBotSelfMessage, IgnoreAtAll: runtime.Platform.IgnoreAtAll,
				DisableBuiltinCommands: runtime.Platform.DisableBuiltinCommands, NoPermissionReply: runtime.Platform.NoPermissionReply,
				EmptyMentionWaiting: runtime.Platform.EmptyMentionWaiting, EmptyMentionNeedReply: runtime.Platform.EmptyMentionNeedReply,
				BlockPatterns: append([]string(nil), runtime.Platform.BlockPatterns...), CheckResponse: runtime.Platform.CheckResponse,
				TelegramPreAckEnabled: runtime.Platform.TelegramPreAckEnabled, TelegramPreAckEmoji: runtime.Platform.TelegramPreAckEmoji,
			},
			// 扩展页设置必须接到实际 Bot 消息链路；列表复制避免配置草稿共享切片。
			Extensions: bot.ExtensionConfig{
				SegmentedReplyEnabled: runtime.Extensions.SegmentedReplyEnabled, SegmentOnlyLLM: runtime.Extensions.SegmentOnlyLLM,
				SegmentIntervalMethod: runtime.Extensions.SegmentIntervalMethod, SegmentInterval: runtime.Extensions.SegmentInterval,
				SegmentLogBase: runtime.Extensions.SegmentLogBase, SegmentWordsThreshold: runtime.Extensions.SegmentWordsThreshold,
				SegmentSplitMode: runtime.Extensions.SegmentSplitMode, SegmentRegex: runtime.Extensions.SegmentRegex,
				SegmentSplitWords: append([]string(nil), runtime.Extensions.SegmentSplitWords...), SegmentCleanupRegex: runtime.Extensions.SegmentCleanupRegex,
				GroupContextEnabled: runtime.Extensions.GroupContextEnabled, GroupMessageMaxCount: runtime.Extensions.GroupMessageMaxCount,
				GroupImageCaption: runtime.Extensions.GroupImageCaption, GroupImageCaptionModel: runtime.Extensions.GroupImageCaptionModel,
				ProactiveReplyEnabled: runtime.Extensions.ProactiveReplyEnabled, ProactiveReplyMethod: runtime.Extensions.ProactiveReplyMethod,
				ProactiveReplyProbability: runtime.Extensions.ProactiveReplyProbability, ProactiveReplyWhitelist: append([]string(nil), runtime.Extensions.ProactiveReplyWhitelist...),
			},
		}, nil
	})
	// Chat commands read and (where supported) change configuration through the
	// same services the WebUI uses.
	commandBridge := &commandRuntimeBridge{
		providers: registry, config: configService, personas: personaService,
		workspaces: workspaceService, conversations: conversationService, runtime: runtimeCoordinator,
	}
	botManager.SetCommandRuntimeInfo(commandBridge)
	botManager.SetCommandRuntimeAdmin(commandBridge)
	botManager.SetCommandRuntimeStats(commandBridge)
	botManager.SetCommandDashboardUpdater(commandBridge)
	// 群图片转述复用已配置的模型目录，消息入口仅接收有界文字结果。
	botManager.SetGroupImageCaptioner(func(captionCtx context.Context, modelID string, attachment agent.Attachment) (string, error) {
		return captionGroupImage(captionCtx, registry, modelID, attachment)
	})
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
	// 未来任务始终通过同一个内置 Coordinator 执行；可选平台投递仍复用已连接的机器人适配器。
	scheduleService.Start(ctx, func(taskCtx context.Context, task schedule.Task) (string, error) {
		userID := task.UserID
		if strings.TrimSpace(userID) == "" {
			userID = "scheduler"
		}
		conversationID := strings.TrimSpace(task.ConversationID)
		if conversationID == "" {
			created, createErr := conversationService.Create(taskCtx, conversation.CreateRequest{UserID: userID, Title: task.Name})
			if createErr != nil {
				return "", createErr
			}
			conversationID = created.ID
		}
		invocation, startErr := runtimeCoordinator.StartInvocation(taskCtx, agent.ChatRequest{
			UserID: userID, BotID: task.AdapterID, ConversationID: conversationID, SessionID: conversationID,
			Message: task.Request, Stream: true,
		})
		if startErr != nil {
			return "", startErr
		}
		if task.AdapterID != "" && task.ChatID != "" {
			go deliverScheduledResult(taskCtx, runtimeCoordinator, conversationService, botManager, task, invocation.ID, userID, conversationID)
		}
		return invocation.ID, nil
	})

	server := httpapi.NewServerWithServices(registry, kernel, workspaceService, conversationService, botManager)
	server.SetToolRegistry(toolRegistry)
	server.SetRuntimeCoordinator(runtimeCoordinator)
	server.SetConfigService(configService)
	server.SetPersonaService(personaService)
	server.SetRemoteTargetService(remoteTargetService)
	server.SetMemoryService(longMemory)
	server.SetArtifactService(artifactService)
	server.SetSessionRuleService(sessionRuleService)
	if sourceRegistry, ok := botRepository.(bot.MessageSourceRegistry); ok {
		server.SetMessageSourceRegistry(sourceRegistry)
	}
	server.SetScheduleService(scheduleService)
	server.SetWebSearchManager(webSearchManager)
	server.SetLogStore(logStore)
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

// deliverScheduledResult 等待未来任务的终态，再把最后一条内置 Agent 回复投递回目标平台。
// 轮询只读取持久化状态，不重新执行 invocation，也不会绕过平台发送边界。
func deliverScheduledResult(ctx context.Context, coordinator *agentruntime.Coordinator, conversations *conversation.Service, bots *bot.Manager, task schedule.Task, invocationID, userID, conversationID string) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		invocation, err := coordinator.GetInvocation(ctx, invocationID)
		if err == nil && invocation.Status.Terminal() {
			text := invocation.Error
			if invocation.Status == agentruntime.InvocationCompleted {
				if messages, messageErr := conversations.Messages(ctx, userID, conversationID); messageErr == nil {
					for index := len(messages) - 1; index >= 0; index-- {
						if messages[index].Role == "assistant" && strings.TrimSpace(messages[index].Text) != "" {
							text = messages[index].Text
							break
						}
					}
				}
			}
			if strings.TrimSpace(text) == "" {
				text = "未来任务已完成，但没有可投递的文本结果。"
			}
			_ = bots.Send(ctx, bot.Message{AdapterID: task.AdapterID, ChatID: task.ChatID, UserID: userID}, text)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
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
	if settings.ArtifactInputRetentionSeconds > 0 {
		policy.InputAttachmentRetentionPeriod = time.Duration(settings.ArtifactInputRetentionSeconds) * time.Second
	}
	return service.SetMaintenancePolicy(policy)
}

// runArtifactMaintenanceLoop 定期执行有界维护，过期输入附件会先删除元数据，
// 随后由对象删除 outbox 或孤儿对象回收完成磁盘清理。
func runArtifactMaintenanceLoop(ctx context.Context, service *artifact.Service) {
	if service == nil {
		return
	}
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			maintenanceCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			result, err := service.RunMaintenance(maintenanceCtx, time.Now().UTC())
			cancel()
			if err != nil {
				slog.Warn("Artifact 定时维护未完成", "error", err)
				continue
			}
			slog.Info("Artifact 定时维护完成",
				"expired_deleted", result.ExpiredArtifacts.Deleted,
				"expired_deferred", result.ExpiredArtifacts.Deferred,
				"objects_deleted", result.GarbageCollection.Deleted,
				"bytes_reclaimed", result.GarbageCollection.ReclaimedBytes)
		}
	}
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

// subAgentRuntimeSettings 将唯一的通用子 Agent 配置复制为无秘密运行参数；允许的工具
// 只作为候选白名单，Kernel 仍会在执行边界再次过滤写入和命令能力。
func subAgentRuntimeSettings(settings configsvc.SystemSettings) agent.SubAgentOptions {
	value := settings.SubAgent
	result := agent.SubAgentOptions{
		ProviderID: value.ProviderID, ModelID: value.ModelID, ReasoningEffort: value.ReasoningEffort,
		MaxOutputTokens: value.MaxOutputTokens, MaxConcurrency: value.MaxConcurrency,
		InputBudgetBytes: value.InputBudgetBytes, OutputBudgetBytes: value.OutputBudgetBytes,
		AllowedTools: append([]string(nil), value.AllowedTools...),
	}
	if value.Temperature != nil {
		number := *value.Temperature
		result.Temperature = &number
	}
	if value.TopP != nil {
		number := *value.TopP
		result.TopP = &number
	}
	return result
}
