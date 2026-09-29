package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
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
	"Abot/internal/document"
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

// applySessionRuleRuntime 把来源规则投影到配置中心的运行时快照。规则只覆盖
// 已明确标记的字段，未标记字段继续继承系统、配置文件和机器人级设置。
func applySessionRuleRuntime(runtime *configsvc.Runtime, rule sessionrule.Rule) {
	if runtime == nil {
		return
	}
	if rule.HasOverride(sessionrule.OverridePrivateMode) {
		runtime.Extensions.PrivateMode = rule.PrivateMode
	}
	if rule.HasOverride(sessionrule.OverrideGroupParticipationMode) {
		runtime.Extensions.GroupParticipationMode = rule.GroupParticipationMode
	}
	if rule.HasOverride(sessionrule.OverrideRecordUnaddressedMessages) {
		runtime.Extensions.RecordUnaddressedMessages = rule.RecordUnaddressedMessages
	}
	if rule.HasOverride(sessionrule.OverrideReactionEnabled) {
		runtime.Extensions.ReactionEnabled = rule.ReactionEnabled
	}
	if rule.HasOverride(sessionrule.OverrideGroupContextEnabled) {
		runtime.Extensions.GroupContextEnabled = rule.GroupContextEnabled
	}
	if rule.HasOverride(sessionrule.OverrideProactiveEnabled) {
		runtime.Extensions.ProactiveEnabled = rule.ProactiveEnabled
	}
	if rule.HasOverride(sessionrule.OverridePrivateHourlyReplyLimit) {
		runtime.Extensions.PrivateHourlyReplyLimit = rule.PrivateHourlyReplyLimit
	}
	if rule.HasOverride(sessionrule.OverrideGroupHourlyReplyLimit) {
		runtime.Extensions.GroupHourlyReplyLimit = rule.GroupHourlyReplyLimit
	}
	if rule.HasOverride(sessionrule.OverrideQuietHoursTimezone) {
		runtime.Extensions.QuietHoursTimezone = rule.QuietHoursTimezone
	}
	if rule.HasOverride(sessionrule.OverrideQuietHoursStart) {
		runtime.Extensions.QuietHoursStart = rule.QuietHoursStart
	}
	if rule.HasOverride(sessionrule.OverrideQuietHoursEnd) {
		runtime.Extensions.QuietHoursEnd = rule.QuietHoursEnd
	}
	if rule.HasOverride(sessionrule.OverrideEmergencyBypassQuietHours) {
		runtime.Extensions.EmergencyBypassQuietHours = rule.EmergencyBypassQuietHours
	}
	if rule.HasOverride(sessionrule.OverrideReplyQuote) {
		runtime.Platform.ReplyQuote = rule.ReplyQuote
	}
	if rule.HasOverride(sessionrule.OverridePrivateReplyQuote) {
		runtime.Platform.PrivateReplyQuote = rule.PrivateReplyQuote
	}
	if rule.HasOverride(sessionrule.OverrideFollowUpEnabled) {
		runtime.Extensions.FollowUpEnabled = rule.FollowUpEnabled
	}
	if rule.HasOverride(sessionrule.OverrideRelationEnabled) {
		runtime.Extensions.RelationEnabled = rule.RelationEnabled
	}
	if rule.HasOverride(sessionrule.OverrideRuntimeEnabled) {
		runtime.Extensions.RuntimeEnabled = rule.RuntimeEnabled
	}
	if rule.HasOverride(sessionrule.OverrideRuntimeMaxConcurrency) {
		runtime.Extensions.RuntimeMaxConcurrency = rule.RuntimeMaxConcurrency
	}
	if rule.HasOverride(sessionrule.OverrideSourceQueueLimit) {
		runtime.Extensions.SourceQueueLimit = rule.SourceQueueLimit
	}
	if rule.HasOverride(sessionrule.OverrideTurnWaitMilliseconds) {
		runtime.Extensions.TurnWaitMilliseconds = rule.TurnWaitMilliseconds
	}
	if rule.HasOverride(sessionrule.OverrideGroupTurnWaitMilliseconds) {
		runtime.Extensions.GroupTurnWaitMilliseconds = rule.GroupTurnWaitMilliseconds
	}
	if rule.HasOverride(sessionrule.OverrideAttachmentWaitMilliseconds) {
		runtime.Extensions.AttachmentWaitMilliseconds = rule.AttachmentWaitMilliseconds
	}
	if rule.HasOverride(sessionrule.OverrideMaxTurnMessages) {
		runtime.Extensions.MaxTurnMessages = rule.MaxTurnMessages
	}
	if rule.HasOverride(sessionrule.OverrideGroupMessageMaxCount) {
		runtime.Extensions.GroupMessageMaxCount = rule.GroupMessageMaxCount
	}
	if rule.HasOverride(sessionrule.OverrideGroupImageCaption) {
		runtime.Extensions.GroupImageCaption = rule.GroupImageCaption
	}
	if rule.HasOverride(sessionrule.OverrideGroupImageCaptionModel) {
		runtime.Extensions.GroupImageCaptionModel = rule.GroupImageCaptionModel
	}
	if rule.HasOverride(sessionrule.OverrideProactiveDegree) {
		runtime.Extensions.ProactiveDegree = rule.ProactiveDegree
	}
	if rule.HasOverride(sessionrule.OverrideCooldownSeconds) {
		runtime.Extensions.CooldownSeconds = rule.CooldownSeconds
	}
	if rule.HasOverride(sessionrule.OverrideHeartbeatSeconds) {
		runtime.Extensions.HeartbeatSeconds = rule.HeartbeatSeconds
	}
	if rule.HasOverride(sessionrule.OverrideRelationRetentionSeconds) {
		runtime.Extensions.RelationRetentionSeconds = rule.RelationRetentionSeconds
	}
	if rule.HasOverride(sessionrule.OverrideFollowUpMax) {
		runtime.Extensions.FollowUpMax = rule.FollowUpMax
	}
	if rule.HasOverride(sessionrule.OverrideFollowUpMaxRetries) {
		runtime.Extensions.FollowUpMaxRetries = rule.FollowUpMaxRetries
	}
	if rule.HasOverride(sessionrule.OverrideFollowUpRetryDelaySeconds) {
		runtime.Extensions.FollowUpRetryDelaySeconds = rule.FollowUpRetryDelaySeconds
	}
	if rule.HasOverride(sessionrule.OverrideFollowUpMaxDelaySeconds) {
		runtime.Extensions.FollowUpMaxDelaySeconds = rule.FollowUpMaxDelaySeconds
	}
	if rule.HasOverride(sessionrule.OverrideFollowUpAllowedSources) {
		runtime.Extensions.FollowUpAllowedSources = append([]string(nil), rule.FollowUpAllowedSources...)
	}
	if rule.HasOverride(sessionrule.OverrideExpressionEnabled) {
		runtime.Extensions.ExpressionEnabled = rule.ExpressionEnabled
	}
	if rule.HasOverride(sessionrule.OverrideExpressionMaxSegments) {
		runtime.Extensions.ExpressionMaxSegments = rule.ExpressionMaxSegments
	}
	if rule.HasOverride(sessionrule.OverrideExpressionLongThreshold) {
		runtime.Extensions.ExpressionLongThreshold = rule.ExpressionLongThreshold
	}
	if rule.HasOverride(sessionrule.OverrideExpressionDelayMilliseconds) {
		runtime.Extensions.ExpressionDelayMilliseconds = rule.ExpressionDelayMilliseconds
	}
	if rule.HasOverride(sessionrule.OverrideReplyMention) {
		runtime.Platform.ReplyMention = rule.ReplyMention
	}
	if rule.HasOverride(sessionrule.OverrideAgentOnDemandEnabled) {
		runtime.Extensions.AgentOnDemandEnabled = rule.AgentOnDemandEnabled
	}
	if rule.HasOverride(sessionrule.OverrideAllowedReadOnlyTools) {
		runtime.Extensions.AllowedReadOnlyTools = append([]string(nil), rule.AllowedReadOnlyTools...)
	}
	if rule.HasOverride(sessionrule.OverrideToolBudget) {
		runtime.Extensions.ToolBudget = rule.ToolBudget
	}
	if rule.HasOverride(sessionrule.OverrideMessageStyle) {
		runtime.Extensions.MessageStyle = rule.MessageStyle
	}
	if rule.HasOverride(sessionrule.OverrideActionPermissions) {
		runtime.Extensions.ActionPermissions = cloneBoolMap(rule.ActionPermissions)
	}
}

// applyBotRuntimeToRuntime 把机器人页面的显式覆盖同步到 Agent Runtime 快照。
// 消息入口和 Agent 入口必须使用同一优先级，否则 WebUI 看似保存成功而实际
// Agent 仍沿用配置文件，尤其会导致工具预算、安静时区和子 Agent 开关失效。
func applyBotRuntimeToRuntime(runtime *configsvc.Runtime, override bot.BotRuntimeConfig) {
	if runtime == nil {
		return
	}
	value := override.NormalizeRuntimeConfig()
	applyBool := func(target *bool, source *bool) {
		if source != nil {
			*target = *source
		}
	}
	applyInt := func(target *int, source *int) {
		if source != nil {
			*target = *source
		}
	}
	applyString := func(target *string, source *string) {
		if source != nil {
			*target = *source
		}
	}
	applyBool(&runtime.Extensions.RuntimeEnabled, value.RuntimeEnabled)
	applyInt(&runtime.Extensions.RuntimeMaxConcurrency, value.RuntimeMaxConcurrency)
	applyInt(&runtime.Extensions.SourceQueueLimit, value.SourceQueueLimit)
	applyInt(&runtime.Extensions.TurnWaitMilliseconds, value.TurnWaitMilliseconds)
	applyInt(&runtime.Extensions.GroupTurnWaitMilliseconds, value.GroupTurnWaitMilliseconds)
	applyInt(&runtime.Extensions.AttachmentWaitMilliseconds, value.AttachmentWaitMilliseconds)
	applyInt(&runtime.Extensions.MaxTurnMessages, value.MaxTurnMessages)
	applyString(&runtime.Extensions.PrivateMode, value.PrivateMode)
	applyString(&runtime.Extensions.GroupParticipationMode, value.GroupParticipationMode)
	applyBool(&runtime.Extensions.RecordUnaddressedMessages, value.RecordUnaddressedMessages)
	applyBool(&runtime.Extensions.ReactionEnabled, value.ReactionEnabled)
	applyBool(&runtime.Extensions.GroupContextEnabled, value.GroupContextEnabled)
	applyInt(&runtime.Extensions.GroupMessageMaxCount, value.GroupMessageMaxCount)
	applyBool(&runtime.Extensions.GroupImageCaption, value.GroupImageCaption)
	applyString(&runtime.Extensions.GroupImageCaptionModel, value.GroupImageCaptionModel)
	applyBool(&runtime.Extensions.ProactiveEnabled, value.ProactiveEnabled)
	applyString(&runtime.Extensions.ProactiveDegree, value.ProactiveDegree)
	applyInt(&runtime.Extensions.CooldownSeconds, value.CooldownSeconds)
	applyInt(&runtime.Extensions.PrivateHourlyReplyLimit, value.PrivateHourlyReplyLimit)
	applyInt(&runtime.Extensions.GroupHourlyReplyLimit, value.GroupHourlyReplyLimit)
	applyInt(&runtime.Extensions.HeartbeatSeconds, value.HeartbeatSeconds)
	applyString(&runtime.Extensions.QuietHoursTimezone, value.QuietHoursTimezone)
	applyString(&runtime.Extensions.QuietHoursStart, value.QuietHoursStart)
	applyString(&runtime.Extensions.QuietHoursEnd, value.QuietHoursEnd)
	applyBool(&runtime.Extensions.EmergencyBypassQuietHours, value.EmergencyBypassQuietHours)
	applyBool(&runtime.Extensions.RelationEnabled, value.RelationEnabled)
	applyInt(&runtime.Extensions.RelationRetentionSeconds, value.RelationRetentionSeconds)
	applyBool(&runtime.Extensions.FollowUpEnabled, value.FollowUpEnabled)
	applyInt(&runtime.Extensions.FollowUpMax, value.FollowUpMax)
	applyInt(&runtime.Extensions.FollowUpMaxRetries, value.FollowUpMaxRetries)
	applyInt(&runtime.Extensions.FollowUpRetryDelaySeconds, value.FollowUpRetryDelaySeconds)
	applyInt(&runtime.Extensions.FollowUpMaxDelaySeconds, value.FollowUpMaxDelaySeconds)
	applyBool(&runtime.Extensions.ExpressionEnabled, value.ExpressionEnabled)
	applyInt(&runtime.Extensions.ExpressionMaxSegments, value.ExpressionMaxSegments)
	applyInt(&runtime.Extensions.ExpressionLongThreshold, value.ExpressionLongThreshold)
	applyInt(&runtime.Extensions.ExpressionDelayMilliseconds, value.ExpressionDelayMilliseconds)
	applyBool(&runtime.Extensions.AgentOnDemandEnabled, value.AgentOnDemandEnabled)
	applyInt(&runtime.Extensions.ToolBudget, value.ToolBudget)
	applyString(&runtime.Extensions.MessageStyle, value.MessageStyle)
	applyBool(&runtime.Platform.ReplyMention, value.ReplyMention)
	applyBool(&runtime.Platform.ReplyQuote, value.ReplyQuote)
	applyBool(&runtime.Platform.PrivateReplyQuote, value.PrivateReplyQuote)
	if value.FollowUpAllowedSources != nil {
		runtime.Extensions.FollowUpAllowedSources = append([]string(nil), value.FollowUpAllowedSources...)
	}
	if value.AllowedReadOnlyTools != nil {
		runtime.Extensions.AllowedReadOnlyTools = append([]string(nil), value.AllowedReadOnlyTools...)
	}
	if value.ActionPermissions != nil {
		runtime.Extensions.ActionPermissions = cloneBoolMap(value.ActionPermissions)
	}
}

// applyBotExtensionToRuntime 把消息入口已经解析完成的扩展策略回写到 Runtime。
// Follow-up 创建发生在消息入口之外，但仍必须使用同一份“配置文件 -> 机器人 ->
// 会话来源”结果，不能因为从调度器进入就重新退回配置文件默认值。
func applyBotExtensionToRuntime(runtime *configsvc.Runtime, extension bot.ExtensionConfig) {
	if runtime == nil {
		return
	}
	runtime.Extensions.RuntimeEnabled = extension.RuntimeEnabled
	runtime.Extensions.RuntimeMaxConcurrency = extension.RuntimeMaxConcurrency
	runtime.Extensions.SourceQueueLimit = extension.SourceQueueLimit
	runtime.Extensions.TurnWaitMilliseconds = extension.TurnWaitMilliseconds
	runtime.Extensions.GroupTurnWaitMilliseconds = extension.GroupTurnWaitMilliseconds
	runtime.Extensions.AttachmentWaitMilliseconds = extension.AttachmentWaitMilliseconds
	runtime.Extensions.MaxTurnMessages = extension.MaxTurnMessages
	runtime.Extensions.PrivateMode = extension.PrivateMode
	runtime.Extensions.GroupParticipationMode = extension.GroupParticipationMode
	runtime.Extensions.RecordUnaddressedMessages = extension.RecordUnaddressedMessages
	runtime.Extensions.ReactionEnabled = extension.ReactionEnabled
	runtime.Extensions.RelationEnabled = extension.RelationEnabled
	runtime.Extensions.AgentOnDemandEnabled = extension.AgentOnDemandEnabled
	runtime.Extensions.GroupContextEnabled = extension.GroupContextEnabled
	runtime.Extensions.GroupMessageMaxCount = extension.GroupMessageMaxCount
	runtime.Extensions.GroupImageCaption = extension.GroupImageCaption
	runtime.Extensions.GroupImageCaptionModel = extension.GroupImageCaptionModel
	runtime.Extensions.ExpressionEnabled = extension.ExpressionEnabled
	runtime.Extensions.ExpressionMaxSegments = extension.ExpressionMaxSegments
	runtime.Extensions.ExpressionLongThreshold = extension.ExpressionLongThreshold
	runtime.Extensions.ExpressionDelayMilliseconds = extension.ExpressionDelayMilliseconds
	runtime.Extensions.ProactiveEnabled = extension.ProactiveEnabled
	runtime.Extensions.ProactiveDegree = extension.ProactiveDegree
	runtime.Extensions.CooldownSeconds = extension.CooldownSeconds
	runtime.Extensions.PrivateHourlyReplyLimit = extension.PrivateHourlyReplyLimit
	runtime.Extensions.GroupHourlyReplyLimit = extension.GroupHourlyReplyLimit
	runtime.Extensions.HeartbeatSeconds = extension.HeartbeatSeconds
	runtime.Extensions.QuietHoursTimezone = extension.QuietHoursTimezone
	runtime.Extensions.QuietHoursStart = extension.QuietHoursStart
	runtime.Extensions.QuietHoursEnd = extension.QuietHoursEnd
	runtime.Extensions.EmergencyBypassQuietHours = extension.EmergencyBypassQuietHours
	runtime.Extensions.RelationRetentionSeconds = extension.RelationRetentionSeconds
	runtime.Extensions.FollowUpEnabled = extension.FollowUpEnabled
	runtime.Extensions.FollowUpMax = extension.FollowUpMax
	runtime.Extensions.FollowUpMaxRetries = extension.FollowUpMaxRetries
	runtime.Extensions.FollowUpRetryDelaySeconds = extension.FollowUpRetryDelaySeconds
	runtime.Extensions.FollowUpMaxDelaySeconds = extension.FollowUpMaxDelaySeconds
	runtime.Extensions.FollowUpAllowedSources = append([]string(nil), extension.FollowUpAllowedSources...)
	runtime.Extensions.AllowedReadOnlyTools = append([]string(nil), extension.AllowedReadOnlyTools...)
	runtime.Extensions.ToolBudget = extension.ToolBudget
	runtime.Extensions.ActionPermissions = cloneBoolMap(extension.ActionPermissions)
	runtime.Extensions.MessageStyle = extension.MessageStyle
}

// applySessionRuleMessageConfig 是消息入口的同一份会话覆盖投影。它必须在
// 机器人级显式覆盖之后执行，保证配置优先级严格遵循“机器人 -> 会话”。
func applySessionRuleMessageConfig(config *bot.MessageConfig, rule sessionrule.Rule) {
	if config == nil {
		return
	}
	if rule.HasOverride(sessionrule.OverridePrivateMode) {
		config.Extensions.PrivateMode = rule.PrivateMode
	}
	if rule.HasOverride(sessionrule.OverrideGroupParticipationMode) {
		config.Extensions.GroupParticipationMode = rule.GroupParticipationMode
	}
	if rule.HasOverride(sessionrule.OverrideRecordUnaddressedMessages) {
		config.Extensions.RecordUnaddressedMessages = rule.RecordUnaddressedMessages
	}
	if rule.HasOverride(sessionrule.OverrideReactionEnabled) {
		config.Extensions.ReactionEnabled = rule.ReactionEnabled
	}
	if rule.HasOverride(sessionrule.OverrideGroupContextEnabled) {
		config.Extensions.GroupContextEnabled = rule.GroupContextEnabled
	}
	if rule.HasOverride(sessionrule.OverrideProactiveEnabled) {
		config.Extensions.ProactiveEnabled = rule.ProactiveEnabled
	}
	if rule.HasOverride(sessionrule.OverridePrivateHourlyReplyLimit) {
		config.Extensions.PrivateHourlyReplyLimit = rule.PrivateHourlyReplyLimit
	}
	if rule.HasOverride(sessionrule.OverrideGroupHourlyReplyLimit) {
		config.Extensions.GroupHourlyReplyLimit = rule.GroupHourlyReplyLimit
	}
	if rule.HasOverride(sessionrule.OverrideQuietHoursTimezone) {
		config.Extensions.QuietHoursTimezone = rule.QuietHoursTimezone
	}
	if rule.HasOverride(sessionrule.OverrideQuietHoursStart) {
		config.Extensions.QuietHoursStart = rule.QuietHoursStart
	}
	if rule.HasOverride(sessionrule.OverrideQuietHoursEnd) {
		config.Extensions.QuietHoursEnd = rule.QuietHoursEnd
	}
	if rule.HasOverride(sessionrule.OverrideEmergencyBypassQuietHours) {
		config.Extensions.EmergencyBypassQuietHours = rule.EmergencyBypassQuietHours
	}
	if rule.HasOverride(sessionrule.OverrideReplyQuote) {
		config.Platform.ReplyQuote = rule.ReplyQuote
	}
	if rule.HasOverride(sessionrule.OverridePrivateReplyQuote) {
		config.Platform.PrivateReplyQuote = rule.PrivateReplyQuote
	}
	if rule.HasOverride(sessionrule.OverrideFollowUpEnabled) {
		config.Extensions.FollowUpEnabled = rule.FollowUpEnabled
	}
	if rule.HasOverride(sessionrule.OverrideRelationEnabled) {
		config.Extensions.RelationEnabled = rule.RelationEnabled
	}
	if rule.HasOverride(sessionrule.OverrideRuntimeEnabled) {
		config.Extensions.RuntimeEnabled = rule.RuntimeEnabled
	}
	if rule.HasOverride(sessionrule.OverrideRuntimeMaxConcurrency) {
		config.Extensions.RuntimeMaxConcurrency = rule.RuntimeMaxConcurrency
	}
	if rule.HasOverride(sessionrule.OverrideSourceQueueLimit) {
		config.Extensions.SourceQueueLimit = rule.SourceQueueLimit
	}
	if rule.HasOverride(sessionrule.OverrideTurnWaitMilliseconds) {
		config.Extensions.TurnWaitMilliseconds = rule.TurnWaitMilliseconds
	}
	if rule.HasOverride(sessionrule.OverrideGroupTurnWaitMilliseconds) {
		config.Extensions.GroupTurnWaitMilliseconds = rule.GroupTurnWaitMilliseconds
	}
	if rule.HasOverride(sessionrule.OverrideAttachmentWaitMilliseconds) {
		config.Extensions.AttachmentWaitMilliseconds = rule.AttachmentWaitMilliseconds
	}
	if rule.HasOverride(sessionrule.OverrideMaxTurnMessages) {
		config.Extensions.MaxTurnMessages = rule.MaxTurnMessages
	}
	if rule.HasOverride(sessionrule.OverrideGroupMessageMaxCount) {
		config.Extensions.GroupMessageMaxCount = rule.GroupMessageMaxCount
	}
	if rule.HasOverride(sessionrule.OverrideGroupImageCaption) {
		config.Extensions.GroupImageCaption = rule.GroupImageCaption
	}
	if rule.HasOverride(sessionrule.OverrideGroupImageCaptionModel) {
		config.Extensions.GroupImageCaptionModel = rule.GroupImageCaptionModel
	}
	if rule.HasOverride(sessionrule.OverrideProactiveDegree) {
		config.Extensions.ProactiveDegree = rule.ProactiveDegree
	}
	if rule.HasOverride(sessionrule.OverrideCooldownSeconds) {
		config.Extensions.CooldownSeconds = rule.CooldownSeconds
	}
	if rule.HasOverride(sessionrule.OverrideHeartbeatSeconds) {
		config.Extensions.HeartbeatSeconds = rule.HeartbeatSeconds
	}
	if rule.HasOverride(sessionrule.OverrideRelationRetentionSeconds) {
		config.Extensions.RelationRetentionSeconds = rule.RelationRetentionSeconds
	}
	if rule.HasOverride(sessionrule.OverrideFollowUpMax) {
		config.Extensions.FollowUpMax = rule.FollowUpMax
	}
	if rule.HasOverride(sessionrule.OverrideFollowUpMaxRetries) {
		config.Extensions.FollowUpMaxRetries = rule.FollowUpMaxRetries
	}
	if rule.HasOverride(sessionrule.OverrideFollowUpRetryDelaySeconds) {
		config.Extensions.FollowUpRetryDelaySeconds = rule.FollowUpRetryDelaySeconds
	}
	if rule.HasOverride(sessionrule.OverrideFollowUpMaxDelaySeconds) {
		config.Extensions.FollowUpMaxDelaySeconds = rule.FollowUpMaxDelaySeconds
	}
	if rule.HasOverride(sessionrule.OverrideFollowUpAllowedSources) {
		config.Extensions.FollowUpAllowedSources = append([]string(nil), rule.FollowUpAllowedSources...)
	}
	if rule.HasOverride(sessionrule.OverrideExpressionEnabled) {
		config.Extensions.ExpressionEnabled = rule.ExpressionEnabled
	}
	if rule.HasOverride(sessionrule.OverrideExpressionMaxSegments) {
		config.Extensions.ExpressionMaxSegments = rule.ExpressionMaxSegments
	}
	if rule.HasOverride(sessionrule.OverrideExpressionLongThreshold) {
		config.Extensions.ExpressionLongThreshold = rule.ExpressionLongThreshold
	}
	if rule.HasOverride(sessionrule.OverrideExpressionDelayMilliseconds) {
		config.Extensions.ExpressionDelayMilliseconds = rule.ExpressionDelayMilliseconds
	}
	if rule.HasOverride(sessionrule.OverrideReplyMention) {
		config.Platform.ReplyMention = rule.ReplyMention
	}
	if rule.HasOverride(sessionrule.OverrideAgentOnDemandEnabled) {
		config.Extensions.AgentOnDemandEnabled = rule.AgentOnDemandEnabled
	}
	if rule.HasOverride(sessionrule.OverrideAllowedReadOnlyTools) {
		config.Extensions.AllowedReadOnlyTools = append([]string(nil), rule.AllowedReadOnlyTools...)
	}
	if rule.HasOverride(sessionrule.OverrideToolBudget) {
		config.Extensions.ToolBudget = rule.ToolBudget
	}
	if rule.HasOverride(sessionrule.OverrideMessageStyle) {
		config.Extensions.MessageStyle = rule.MessageStyle
	}
	if rule.HasOverride(sessionrule.OverrideActionPermissions) {
		config.Extensions.ActionPermissions = cloneBoolMap(rule.ActionPermissions)
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
	// 先声明父 Runtime 指针，检索编排器在真正执行时才通过闭包投影事件；
	// 这样初始化顺序不会让检索服务持有另一套事件总线。
	var runtimeCoordinator *agentruntime.Coordinator
	// 统一检索编排器复用现有记忆、会话、网页和工作区服务；它只输出有来源的
	// EvidenceItem，主 Agent 通过内置 retrieval_search 工具按当前权限调用。
	retrievalOrchestrator := agent.NewRetrievalOrchestrator(agent.RetrievalOrchestratorOptions{
		EventSink: func(eventCtx context.Context, invocationID, eventType string, data map[string]any) error {
			if runtimeCoordinator == nil {
				return nil
			}
			return runtimeCoordinator.RecordRuntimeEvent(eventCtx, invocationID, eventType, data)
		},
	})
	for _, adapter := range []agent.RetrievalSourceAdapter{
		memoryRetrievalAdapter{service: longMemory},
		conversationRetrievalAdapter{service: conversationService},
		webRetrievalAdapter{manager: webSearchManager},
		workspaceRetrievalAdapter{service: workspaceService},
		artifactDocumentRetrievalAdapter{service: artifactService, kind: agent.RetrievalSourceStructured},
		artifactDocumentRetrievalAdapter{service: artifactService, kind: agent.RetrievalSourceKnowledge},
	} {
		if registerErr := retrievalOrchestrator.RegisterAdapter(adapter); registerErr != nil {
			return registerErr
		}
	}
	runtimeRepo := store.RuntimeRepository()
	basicTools, err := basic.Tools()
	if err != nil {
		return err
	}
	toolRegistry := agent.NewToolRegistry()
	// botManager 在 Kernel 的 RuntimeConfigResolver 建立后才完成装配，先声明变量
	// 让机器人级子 Agent、工具预算覆盖在实际 Invocation 启动时生效。
	var botManager *bot.Manager
	// 子 Agent Manager 在 Kernel 构造后启动；检索工具闭包先捕获指针，
	// 真正创建工具发生在 Invocation 运行时，此时 Manager 已经完成启动。
	var subAgentManager *agent.SubAgentManager
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
		RetrievalToolFactory: func(_ context.Context, runtime agent.RuntimeOptions, userID, conversationID, sessionID, invocationID string) (tool.Tool, error) {
			allowed := []agent.RetrievalSourceKind{agent.RetrievalSourceConversation, agent.RetrievalSourceWorkspace}
			if runtime.MemoryEnabled {
				allowed = append(allowed, agent.RetrievalSourceMemory)
			}
			if runtime.WebSearchEnabled {
				allowed = append(allowed, agent.RetrievalSourceWeb)
			}
			// 结构化文件和知识库适配器都在 Artifact 所有权、会话范围和大小
			// 边界内工作；是否有可用来源由请求的显式 SourceIDs 决定。
			allowed = append(allowed, agent.RetrievalSourceStructured, agent.RetrievalSourceKnowledge)
			return agent.NewManagedRetrievalTool(retrievalOrchestrator, subAgentManager, invocationID, userID, conversationID, sessionID, runtime, allowed)
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
		DerivedArtifactWriter: func(ctx context.Context, userID, conversationID, invocationID string, parent agent.AttachmentRef, image document.Image) (agent.AttachmentRef, error) {
			if len(image.Data) == 0 {
				return agent.AttachmentRef{}, errors.New("文档图片为空，不能创建派生 Artifact")
			}
			name := strings.TrimSpace(image.Name)
			if name == "" {
				name = "document-image"
			}
			producerID := strings.Join([]string{strings.TrimSpace(invocationID), strings.TrimSpace(parent.ID), strings.TrimSpace(parent.Digest), strings.TrimSpace(image.Locator)}, "|")
			expiresAt := time.Now().UTC().Add(24 * time.Hour)
			stored, err := artifactService.Put(ctx, artifact.PutRequest{
				UserID: userID, ConversationID: conversationID, InvocationID: invocationID,
				ProducerType: "document_image_derived", ProducerID: producerID,
				Kind: artifact.KindImage, Name: name, MIMEType: image.MIMEType,
				Metadata: map[string]any{
					"parent_artifact_id": parent.ID, "parent_artifact_digest": parent.Digest,
					"source_locator": image.Locator, "derived": true,
				}, ExpiresAt: &expiresAt, MaxBytes: 8 << 20,
			}, bytes.NewReader(image.Data))
			if err != nil {
				return agent.AttachmentRef{}, err
			}
			return agent.AttachmentRef{ID: stored.ID, Version: stored.Version, Digest: stored.Digest, Kind: string(stored.Kind), MIMEType: stored.MIMEType, Size: stored.Size, Name: stored.Name}, nil
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
		RuntimeActionHandler: func(actionCtx context.Context, request agent.RuntimeActionRequest) (string, error) {
			if botManager == nil {
				return "", errors.New("Bot Runtime 动作宿主尚未启动")
			}
			delivery := request.Delivery
			if delivery == nil {
				return "", errors.New("Runtime 动作缺少平台投递目标")
			}
			message := bot.Message{
				ID: delivery.MessageID, AdapterID: request.BotID, Platform: bot.Type(delivery.Platform),
				UserID: delivery.UserID, ChatID: delivery.ChatID, ChatType: delivery.ChatType,
				ReplyMessageID: delivery.ReplyMessageID, RuntimeConversationID: request.ConversationID,
				RuntimeTurnID: delivery.TurnID, SourceUMO: delivery.SourceUMO, Text: request.Message,
			}
			result, err := botManager.DispatchAction(actionCtx, bot.PlatformAction{Type: request.Type, Message: message, Payload: request.Payload})
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(result.Raw) != "" {
				return result.Raw, nil
			}
			return result.MessageID, nil
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
			// 先解析来源规则选中的配置文件；机器人级覆盖必须在这里之后应用，
			// 否则“会话规则选择配置文件”会把机器人页面保存的 Runtime 覆盖掉。
			var botRuntimeOverride bot.BotRuntimeConfig
			rule, found, ruleErr := sessionRuleService.Resolve(ctx, source)
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
			}
			if botManager != nil {
				if configuredBot, botErr := botManager.Get(botID); botErr == nil {
					botRuntimeOverride = configuredBot.RuntimeConfig.NormalizeRuntimeConfig()
					applyBotRuntimeToRuntime(&runtime, botRuntimeOverride)
				}
			}
			// 会话规则优先于机器人级 Runtime 覆盖；它只能覆盖已明确配置的字段，
			// 未设置的字段继续沿用刚刚解析的机器人配置。
			if found {
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
				if rule.HasOverride(sessionrule.OverrideSubAgentEnabled) {
					enabled := rule.SubAgentEnabled
					conversationSubAgentEnabled = &enabled
				}
				applySessionRuleRuntime(&runtime, rule)
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
			subAgentOptions := subAgentRuntimeSettings(settings)
			// 会话规则的只读工具和预算覆盖必须同步作用于 Agent Runtime，
			// 不能只在 Bot 消息入口显示为已保存。
			if runtime.Extensions.ToolBudget > 0 {
				runtime.AgentMaxToolCalls = runtime.Extensions.ToolBudget
			}
			if runtime.Extensions.AllowedReadOnlyTools != nil {
				subAgentOptions.AllowedTools = append([]string(nil), runtime.Extensions.AllowedReadOnlyTools...)
			}
			if conversationSubAgentEnabled == nil && botRuntimeOverride.SubAgentEnabled != nil {
				subAgentEnabled = *botRuntimeOverride.SubAgentEnabled
			}
			subAgentEnabledValue := subAgentEnabled
			return agent.RuntimeOptions{
				AIEnabled: runtime.AIEnabled, ProviderID: runtime.ProviderID, ModelID: runtime.ModelID,
				Timezone:      runtime.Extensions.QuietHoursTimezone,
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
				SubAgent:        subAgentOptions,
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
	// 文档图片和后续检索都通过同一个固定 worker 的 Group/Run Manager；
	// Manager 不设置独立短超时，取消和总时限只来自父 Invocation Context。
	subAgentManager = agent.NewSubAgentManager(kernel, agent.SubAgentManagerOptions{
		Store: store.SubAgentRuntimeRepository(),
		// 子 Agent 状态进入父 Invocation 的统一事件流，WebUI/SSE 可以看到
		// 任务树变化，同时 Bot 平台不会收到这些内部过程消息。
		EventSink: func(eventCtx context.Context, invocationID, eventType string, data map[string]any) error {
			return runtimeCoordinator.RecordRuntimeEvent(eventCtx, invocationID, eventType, data)
		},
		LifecycleSink: func(eventCtx context.Context, invocationID, groupID string, waiting bool) error {
			if waiting {
				return runtimeCoordinator.SetInvocationWaitingForSubagents(eventCtx, invocationID, groupID)
			}
			return runtimeCoordinator.ResumeInvocationFromSubagents(eventCtx, invocationID, groupID)
		},
	})
	kernel.SetSubAgentManager(subAgentManager)
	if err := subAgentManager.Start(ctx); err != nil {
		return fmt.Errorf("启动通用子 Agent Manager 失败: %w", err)
	}
	defer func() { _ = subAgentManager.Close() }()
	go runRuntimeMaintenanceLoop(ctx, subAgentManager, retrievalOrchestrator)

	botManager, err = bot.NewManager(ctx, botRepository, kernel, conversationService)
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
	runtimeStateRepository := store.BotRuntimeRepository()
	botManager.SetRuntimeStateRepository(runtimeStateRepository)
	botManager.SetRuntimeFollowUpCreator(func(actionCtx context.Context, action bot.PlatformAction) (string, error) {
		return createBotRuntimeFollowUp(actionCtx, action, scheduleService, configService, botManager, kernel)
	})
	if cleaner, ok := runtimeStateRepository.(bot.RuntimeConversationStateCleaner); ok {
		conversationService.SetRuntimeDeletionHook(func(cleanupCtx context.Context, userID, conversationID string) error {
			// 对话物理删除前先失效检索缓存，避免短 TTL 内存证据继续被
			// 同一用户的后续请求命中；Runtime 表清理仍由同一事务完成。
			retrievalOrchestrator.Invalidate(agent.RetrievalRequest{
				UserID: userID, ConversationID: conversationID,
				SourceKinds: []agent.RetrievalSourceKind{
					agent.RetrievalSourceMemory, agent.RetrievalSourceKnowledge,
					agent.RetrievalSourceConversation, agent.RetrievalSourceWeb,
					agent.RetrievalSourceWorkspace, agent.RetrievalSourceStructured,
				},
			})
			return cleaner.DeleteConversationState(cleanupCtx, conversationID)
		})
	}
	// 平台级管理员和唤醒词与 Agent 配置共用同一份配置文件解析结果，避免 WebUI 保存后消息入口仍使用旧逻辑。
	botManager.SetMessageConfigResolver(func(resolveCtx context.Context, botID, conversationID, sourceUMO string) (bot.MessageConfig, error) {
		runtime, resolveErr := configService.Resolve(resolveCtx, botID, conversationID)
		if resolveErr != nil {
			return bot.MessageConfig{}, resolveErr
		}
		resolved := bot.MessageConfig{
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
				RuntimeEnabled: runtime.Extensions.RuntimeEnabled, RuntimeMaxConcurrency: runtime.Extensions.RuntimeMaxConcurrency,
				SourceQueueLimit: runtime.Extensions.SourceQueueLimit, TurnWaitMilliseconds: runtime.Extensions.TurnWaitMilliseconds,
				GroupTurnWaitMilliseconds: runtime.Extensions.GroupTurnWaitMilliseconds, AttachmentWaitMilliseconds: runtime.Extensions.AttachmentWaitMilliseconds,
				MaxTurnMessages: runtime.Extensions.MaxTurnMessages, PrivateMode: runtime.Extensions.PrivateMode,
				GroupParticipationMode: runtime.Extensions.GroupParticipationMode, RecordUnaddressedMessages: runtime.Extensions.RecordUnaddressedMessages,
				ReactionEnabled: runtime.Extensions.ReactionEnabled, RelationEnabled: runtime.Extensions.RelationEnabled,
				AgentOnDemandEnabled: runtime.Extensions.AgentOnDemandEnabled,
				GroupContextEnabled:  runtime.Extensions.GroupContextEnabled, GroupMessageMaxCount: runtime.Extensions.GroupMessageMaxCount,
				GroupImageCaption: runtime.Extensions.GroupImageCaption, GroupImageCaptionModel: runtime.Extensions.GroupImageCaptionModel,
				ExpressionEnabled: runtime.Extensions.ExpressionEnabled, ExpressionMaxSegments: runtime.Extensions.ExpressionMaxSegments,
				ExpressionLongThreshold: runtime.Extensions.ExpressionLongThreshold, ExpressionDelayMilliseconds: runtime.Extensions.ExpressionDelayMilliseconds,
				ProactiveEnabled: runtime.Extensions.ProactiveEnabled, ProactiveDegree: runtime.Extensions.ProactiveDegree,
				CooldownSeconds: runtime.Extensions.CooldownSeconds, PrivateHourlyReplyLimit: runtime.Extensions.PrivateHourlyReplyLimit, GroupHourlyReplyLimit: runtime.Extensions.GroupHourlyReplyLimit,
				HeartbeatSeconds: runtime.Extensions.HeartbeatSeconds, QuietHoursTimezone: runtime.Extensions.QuietHoursTimezone, QuietHoursStart: runtime.Extensions.QuietHoursStart, QuietHoursEnd: runtime.Extensions.QuietHoursEnd,
				EmergencyBypassQuietHours: runtime.Extensions.EmergencyBypassQuietHours, RelationRetentionSeconds: runtime.Extensions.RelationRetentionSeconds,
				FollowUpEnabled: runtime.Extensions.FollowUpEnabled, FollowUpMax: runtime.Extensions.FollowUpMax,
				FollowUpMaxRetries: runtime.Extensions.FollowUpMaxRetries, FollowUpRetryDelaySeconds: runtime.Extensions.FollowUpRetryDelaySeconds, FollowUpMaxDelaySeconds: runtime.Extensions.FollowUpMaxDelaySeconds,
				FollowUpAllowedSources: append([]string(nil), runtime.Extensions.FollowUpAllowedSources...),
				AllowedReadOnlyTools:   append([]string(nil), runtime.Extensions.AllowedReadOnlyTools...), ToolBudget: runtime.Extensions.ToolBudget,
				ActionPermissions: cloneBoolMap(runtime.Extensions.ActionPermissions), MessageStyle: runtime.Extensions.MessageStyle,
			},
		}
		// 机器人页面的显式覆盖位于配置文件和会话规则之上；读取失败时不放宽权限。
		if configuredBot, botErr := botManager.Get(botID); botErr == nil {
			resolved.ApplyBotRuntimeConfig(configuredBot.RuntimeConfig)
		}
		// 会话规则优先按稳定 UMO 命中。首次消息可能还没有 Conversation，不能
		// 依赖会话表反查来源，否则来源级唤醒和群聊准入会延迟到第二条消息。
		source := strings.TrimSpace(sourceUMO)
		if source == "" {
			source = strings.TrimSpace(conversationID)
			if item, conversationErr := conversationService.GetByID(resolveCtx, conversationID); conversationErr == nil && strings.TrimSpace(item.Source) != "" {
				source = strings.TrimSpace(item.Source)
			}
		}
		if source != "" {
			rule, found, ruleErr := sessionRuleService.Resolve(resolveCtx, source)
			if ruleErr != nil && !errors.Is(ruleErr, sessionrule.ErrNotFound) {
				return bot.MessageConfig{}, ruleErr
			}
			if found {
				applySessionRuleMessageConfig(&resolved, rule)
			}
		}
		return resolved, nil
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
		followUpSnapshot, snapshotErr := decodeBotRuntimeFollowUpSnapshot(task.ConfigSnapshot)
		if snapshotErr != nil {
			return "", snapshotErr
		}
		platform := ""
		chatType := "private"
		if strings.Contains(task.SourceUMO, ":GroupMessage:") {
			chatType = "group"
		}
		var proactiveTools []string
		proactiveToolBudget := 0
		providerID := strings.TrimSpace(followUpSnapshot.ProviderID)
		modelID := strings.TrimSpace(followUpSnapshot.ModelID)
		configuredBot, botErr := botManager.Get(task.AdapterID)
		if botErr == nil {
			platform = string(configuredBot.Type)
		}
		// Follow-up 触发时重新解析来源配置，包含会话规则和机器人级覆盖；
		// 任务快照只保留创建时事实，当前权限收紧必须立即生效。
		currentMessageConfig, configErr := botManager.ResolveMessageConfig(taskCtx, bot.Message{
			AdapterID: task.AdapterID, Platform: bot.Type(platform), UserID: userID, ChatID: task.ChatID,
			ChatType: chatType, SourceUMO: task.SourceUMO, RuntimeConversationID: conversationID,
		})
		if configErr != nil {
			return "", fmt.Errorf("读取 Follow-up 当前权限失败: %w", configErr)
		}
		currentRuntime, runtimeErr := kernel.ResolveRuntimeOptions(taskCtx, agent.ChatRequest{
			UserID: userID, BotID: task.AdapterID, ConversationID: conversationID, SessionID: conversationID,
			ProviderID: strings.TrimSpace(followUpSnapshot.ProviderID), ModelID: strings.TrimSpace(followUpSnapshot.ModelID), Proactive: true,
		})
		if runtimeErr != nil {
			return "", fmt.Errorf("读取 Follow-up 当前 Agent 配置失败: %w", runtimeErr)
		}
		runtimeOverride, runtimeOverrideErr := resolveFollowUpRuntimeOverride(followUpSnapshot, currentRuntime, currentMessageConfig)
		if runtimeOverrideErr != nil {
			return "", fmt.Errorf("读取 Follow-up 运行参数快照失败: %w", runtimeOverrideErr)
		}
		proactiveTools = append([]string(nil), currentMessageConfig.Extensions.AllowedReadOnlyTools...)
		proactiveToolBudget = currentMessageConfig.Extensions.ToolBudget
		if len(followUpSnapshot.AllowedReadOnlyTools) > 0 {
			// 创建时的工具集合是任务事实；当前配置若进一步收紧白名单，只取
			// 两者交集，不能因为任务恢复而重新获得已撤销的工具。
			if len(proactiveTools) > 0 {
				proactiveTools = intersectRuntimeToolNames(followUpSnapshot.AllowedReadOnlyTools, proactiveTools)
			} else {
				proactiveTools = append([]string(nil), followUpSnapshot.AllowedReadOnlyTools...)
			}
		}
		if followUpSnapshot.ToolBudget > 0 && (proactiveToolBudget <= 0 || followUpSnapshot.ToolBudget < proactiveToolBudget) {
			proactiveToolBudget = followUpSnapshot.ToolBudget
		}
		if proactiveToolBudget > 0 && (runtimeOverride.AgentMaxToolCalls <= 0 || proactiveToolBudget < runtimeOverride.AgentMaxToolCalls) {
			runtimeOverride.AgentMaxToolCalls = proactiveToolBudget
		}
		// 机器人消息入口的工具白名单是当前来源的最终安全边界；RuntimeOverride
		// 只保存创建时语义，执行时再次取交集并把同一份列表传给 Invocation。
		runtimeOverride.SubAgent.AllowedTools = intersectRuntimeToolNames(runtimeOverride.SubAgent.AllowedTools, proactiveTools)
		providerID = strings.TrimSpace(runtimeOverride.ProviderID)
		modelID = strings.TrimSpace(runtimeOverride.ModelID)
		// Follow-up 继续走统一 BotDelivery，重启恢复和最终表达层才能知道平台、聊天类型和引用边界。
		invocation, startErr := runtimeCoordinator.StartInvocation(taskCtx, agent.ChatRequest{
			UserID: userID, BotID: task.AdapterID, ConversationID: conversationID, SessionID: conversationID,
			ProviderID: providerID, ModelID: modelID, Message: task.Request, QueueIfBusy: true, Proactive: true, AllowedTools: proactiveTools, ToolBudget: proactiveToolBudget, Stream: true,
			RuntimeOverride: runtimeOverride,
			BotDelivery:     &agent.BotDeliveryTarget{Platform: platform, ChatID: task.ChatID, ChatType: chatType, UserID: userID, MessageID: task.ID, SourceUMO: task.SourceUMO},
		})
		if startErr != nil {
			return "", startErr
		}
		if task.AdapterID != "" && task.ChatID != "" {
			// 调度器只有在 Invocation 终态且最终文本已经成功投递后才算本次
			// Follow-up 完成；不能因为“启动了模型”就提前确认成功。
			if deliverErr := deliverScheduledResult(taskCtx, runtimeCoordinator, conversationService, botManager, task, invocation.ID, userID, conversationID); deliverErr != nil {
				return invocation.ID, deliverErr
			}
		}
		return invocation.ID, nil
	})

	server := httpapi.NewServerWithServices(registry, kernel, workspaceService, conversationService, botManager)
	server.SetToolRegistry(toolRegistry)
	server.SetRuntimeCoordinator(runtimeCoordinator)
	server.SetSubAgentRuntimeStore(store.SubAgentRuntimeRepository())
	server.SetRetrievalOrchestrator(retrievalOrchestrator)
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
func deliverScheduledResult(ctx context.Context, coordinator *agentruntime.Coordinator, conversations *conversation.Service, bots *bot.Manager, task schedule.Task, invocationID, userID, conversationID string) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		invocation, err := coordinator.GetInvocation(ctx, invocationID)
		if err == nil && invocation.Status.Terminal() {
			if invocation.Status != agentruntime.InvocationCompleted {
				if strings.TrimSpace(invocation.Error) == "" {
					return fmt.Errorf("Follow-up Agent 任务未完成: %s", invocation.Status)
				}
				return errors.New(invocation.Error)
			}
			text := invocation.Error
			if messages, messageErr := conversations.Messages(ctx, userID, conversationID); messageErr == nil {
				for index := len(messages) - 1; index >= 0; index-- {
					if messages[index].Role == "assistant" && strings.TrimSpace(messages[index].Text) != "" {
						text = messages[index].Text
						break
					}
				}
			}
			if strings.TrimSpace(text) == "" {
				text = "未来任务已完成，但没有可投递的文本结果。"
			}
			platform := bot.Type("")
			if configuredBot, botErr := bots.Get(task.AdapterID); botErr == nil {
				platform = configuredBot.Type
			}
			chatType := "private"
			if strings.Contains(task.SourceUMO, ":GroupMessage:") {
				chatType = "group"
			}
			if err := bots.Send(ctx, bot.Message{AdapterID: task.AdapterID, Platform: platform, ChatID: task.ChatID, ChatType: chatType, UserID: userID, SourceUMO: task.SourceUMO, ID: task.ID, IsProactive: true}, text); err != nil {
				return fmt.Errorf("Follow-up 结果投递失败: %w", err)
			}
			return nil
		}
		if err != nil && !errors.Is(err, agentruntime.ErrNotFound) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
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

// cloneBoolMap 复制动作权限快照，避免配置解析结果在消息入口被并发修改。
func cloneBoolMap(value map[string]bool) map[string]bool {
	if value == nil {
		return nil
	}
	result := make(map[string]bool, len(value))
	for key, enabled := range value {
		result[key] = enabled
	}
	return result
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

// runRuntimeMaintenanceLoop 定期回收子 Agent 终态元数据和检索短 TTL 缓存。
// 清理只针对已结束记录，任何仍可能被父任务引用的 Group/Run 都保留。
func runRuntimeMaintenanceLoop(ctx context.Context, manager *agent.SubAgentManager, retrieval *agent.RetrievalOrchestrator) {
	if manager == nil && retrieval == nil {
		return
	}
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			maintenanceCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			if manager != nil {
				if count, err := manager.Cleanup(maintenanceCtx, now.UTC().Add(-24*time.Hour)); err != nil {
					slog.Warn("子 Agent 终态清理失败", "error", err)
				} else if count > 0 {
					slog.Info("子 Agent 终态清理完成", "groups", count)
				}
			}
			if retrieval != nil {
				if count := retrieval.CleanupExpired(now.UTC()); count > 0 {
					slog.Info("检索缓存清理完成", "entries", count)
				}
			}
			cancel()
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

// botRuntimeFollowUpSnapshot 是 Follow-up 创建时保存的无秘密运行策略。它不保存
// API Key、提示词正文或平台地址，只固定模型路由、工具白名单、预算和时区等会改变
// 任务语义的字段；执行时仍会重新经过当前权限边界校验。
type botRuntimeFollowUpSnapshot struct {
	Version int `json:"version"`
	// RuntimeOptionsJSON 保存创建时已经解析完成的无秘密 Agent 运行参数；调度器
	// 恢复时以它为语义基线，再与当前安全收紧策略取交集，避免只保存几个字段。
	RuntimeOptionsJSON     string                     `json:"runtime_options_json,omitempty"`
	ProviderID             string                     `json:"provider_id,omitempty"`
	ModelID                string                     `json:"model_id,omitempty"`
	PersonaID              string                     `json:"persona_id,omitempty"`
	AIEnabled              bool                       `json:"ai_enabled"`
	AIExecutionMode        string                     `json:"ai_execution_mode,omitempty"`
	CompactionEnabled      bool                       `json:"compaction_enabled"`
	CompactionRatio        float64                    `json:"compaction_ratio,omitempty"`
	CompactionSafetyTokens int                        `json:"compaction_safety_tokens,omitempty"`
	AgentMaxToolCalls      int                        `json:"agent_max_tool_calls,omitempty"`
	ToolSchemaBudgetTokens int                        `json:"tool_schema_budget_tokens,omitempty"`
	WebSearchEnabled       bool                       `json:"web_search_enabled"`
	WebSearchServiceIDs    []string                   `json:"web_search_service_ids,omitempty"`
	AllowedReadOnlyTools   []string                   `json:"allowed_read_only_tools,omitempty"`
	ToolBudget             int                        `json:"tool_budget,omitempty"`
	Timezone               string                     `json:"timezone,omitempty"`
	QuietHoursStart        string                     `json:"quiet_hours_start,omitempty"`
	QuietHoursEnd          string                     `json:"quiet_hours_end,omitempty"`
	QuietHoursTimezone     string                     `json:"quiet_hours_timezone,omitempty"`
	MessageStyle           string                     `json:"message_style,omitempty"`
	SubAgentEnabled        bool                       `json:"subagent_enabled"`
	SubAgent               configsvc.SubAgentSettings `json:"subagent"`
}

func encodeBotRuntimeFollowUpSnapshot(runtime configsvc.Runtime, settings configsvc.SystemSettings, effective *agent.RuntimeOptions) (string, error) {
	snapshot := botRuntimeFollowUpSnapshot{
		Version: 1, ProviderID: strings.TrimSpace(runtime.ProviderID), ModelID: strings.TrimSpace(runtime.ModelID),
		PersonaID: strings.TrimSpace(runtime.PersonaID), AIEnabled: runtime.AIEnabled, AIExecutionMode: strings.TrimSpace(runtime.AIExecutionMode),
		CompactionEnabled: runtime.CompactionEnabled, CompactionRatio: runtime.CompactionRatio,
		CompactionSafetyTokens: runtime.CompactionSafetyTokens, AgentMaxToolCalls: runtime.AgentMaxToolCalls,
		ToolSchemaBudgetTokens: runtime.ToolSchemaBudgetTokens, WebSearchEnabled: runtime.WebSearchEnabled,
		WebSearchServiceIDs:  append([]string(nil), runtime.WebSearchServiceIDs...),
		AllowedReadOnlyTools: append([]string(nil), runtime.Extensions.AllowedReadOnlyTools...), ToolBudget: runtime.Extensions.ToolBudget,
		Timezone: runtime.Extensions.QuietHoursTimezone, QuietHoursStart: runtime.Extensions.QuietHoursStart,
		QuietHoursEnd: runtime.Extensions.QuietHoursEnd, QuietHoursTimezone: runtime.Extensions.QuietHoursTimezone,
		MessageStyle: runtime.Extensions.MessageStyle, SubAgentEnabled: settings.IsSubAgentEnabled(),
		SubAgent: settings.SubAgent,
	}
	if effective != nil {
		runtimeOptionsJSON, runtimeOptionsErr := agent.EncodeRuntimeOptionsOverride(*effective)
		if runtimeOptionsErr != nil {
			return "", runtimeOptionsErr
		}
		snapshot.RuntimeOptionsJSON = runtimeOptionsJSON
		// Follow-up 创建时记录真正生效的内置 Agent 选择；执行时仍会重新
		// 经过当前权限收紧，但不能因为系统默认变化而丢失创建时事实。
		if strings.TrimSpace(effective.ProviderID) != "" {
			snapshot.ProviderID = strings.TrimSpace(effective.ProviderID)
		}
		if strings.TrimSpace(effective.ModelID) != "" {
			snapshot.ModelID = strings.TrimSpace(effective.ModelID)
		}
		snapshot.PersonaID = strings.TrimSpace(effective.PersonaID)
		snapshot.CompactionEnabled = effective.CompactionEnabled
		snapshot.CompactionRatio = effective.CompactionRatio
		snapshot.CompactionSafetyTokens = effective.CompactionSafetyTokens
		snapshot.AgentMaxToolCalls = effective.AgentMaxToolCalls
		snapshot.ToolSchemaBudgetTokens = effective.ToolSchemaBudgetTokens
		snapshot.WebSearchEnabled = effective.WebSearchEnabled
		snapshot.WebSearchServiceIDs = append([]string(nil), effective.WebSearchServiceIDs...)
		snapshot.SubAgentEnabled = effective.SubAgentsEnabled()
		snapshot.SubAgent = configsvc.SubAgentSettings{
			ProviderID: effective.SubAgent.ProviderID, ModelID: effective.SubAgent.ModelID,
			ReasoningEffort: effective.SubAgent.ReasoningEffort, Temperature: effective.SubAgent.Temperature,
			TopP: effective.SubAgent.TopP, MaxOutputTokens: effective.SubAgent.MaxOutputTokens,
			MaxConcurrency: effective.SubAgent.MaxConcurrency, InputBudgetBytes: effective.SubAgent.InputBudgetBytes,
			OutputBudgetBytes: effective.SubAgent.OutputBudgetBytes, AllowedTools: append([]string(nil), effective.SubAgent.AllowedTools...),
		}
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return "", fmt.Errorf("编码 Follow-up 配置快照失败: %w", err)
	}
	if len(encoded) > 20000 {
		return "", errors.New("Follow-up 配置快照超过 20000 字节")
	}
	return string(encoded), nil
}

func decodeBotRuntimeFollowUpSnapshot(encoded string) (botRuntimeFollowUpSnapshot, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return botRuntimeFollowUpSnapshot{}, nil
	}
	var snapshot botRuntimeFollowUpSnapshot
	if err := json.Unmarshal([]byte(encoded), &snapshot); err != nil {
		return botRuntimeFollowUpSnapshot{}, fmt.Errorf("解析 Follow-up 配置快照失败: %w", err)
	}
	if snapshot.Version != 1 {
		return botRuntimeFollowUpSnapshot{}, fmt.Errorf("不支持的 Follow-up 配置快照版本 %d", snapshot.Version)
	}
	return snapshot, nil
}

// resolveFollowUpRuntimeOverride 恢复 Follow-up 创建时的完整运行参数，并把
// 当前配置中可能收紧的能力再次压回去。语义参数沿用创建时快照，权限参数取
// 创建时与当前值的交集；这样模型切换不会意外获得任务创建后被撤销的工具权限。
func resolveFollowUpRuntimeOverride(snapshot botRuntimeFollowUpSnapshot, current agent.RuntimeOptions, messageConfig bot.MessageConfig) (*agent.RuntimeOptions, error) {
	value := current
	if strings.TrimSpace(snapshot.RuntimeOptionsJSON) != "" {
		decoded, err := agent.DecodeRuntimeOptionsOverride(snapshot.RuntimeOptionsJSON)
		if err != nil {
			return nil, err
		}
		value = decoded
	} else {
		// 兼容已经落库但尚未带完整 RuntimeOptionsJSON 的旧 Follow-up 快照；
		// 这里只恢复文档已有的有限字段，新任务全部走上面的完整快照路径。
		if snapshot.ProviderID != "" {
			value.ProviderID = snapshot.ProviderID
		}
		if snapshot.ModelID != "" {
			value.ModelID = snapshot.ModelID
		}
		if snapshot.PersonaID != "" {
			value.PersonaID = snapshot.PersonaID
		}
		value.AIEnabled = snapshot.AIEnabled
		value.CompactionEnabled = snapshot.CompactionEnabled
		if snapshot.CompactionRatio > 0 {
			value.CompactionRatio = snapshot.CompactionRatio
		}
		if snapshot.CompactionSafetyTokens > 0 {
			value.CompactionSafetyTokens = snapshot.CompactionSafetyTokens
		}
		if snapshot.AgentMaxToolCalls > 0 {
			value.AgentMaxToolCalls = snapshot.AgentMaxToolCalls
		}
		if snapshot.ToolSchemaBudgetTokens > 0 {
			value.ToolSchemaBudgetTokens = snapshot.ToolSchemaBudgetTokens
		}
		value.WebSearchEnabled = snapshot.WebSearchEnabled
		value.WebSearchServiceIDs = append([]string(nil), snapshot.WebSearchServiceIDs...)
		value.SubAgentEnabled = boolPointer(snapshot.SubAgentEnabled)
		value.SubAgent = subAgentOptionsFromSettings(snapshot.SubAgent)
	}
	// 系统指令不在快照中保存，Kernel 恢复时会使用当前人格的指令；这里也
	// 保留当前配置的身份信息，避免 Follow-up 删除人格后带着空提示词运行。
	value.Instruction = current.Instruction
	if strings.TrimSpace(value.ProviderID) == "" {
		value.ProviderID = current.ProviderID
	}
	if strings.TrimSpace(value.ModelID) == "" {
		value.ModelID = current.ModelID
	}
	value.AIEnabled = value.AIEnabled && current.AIEnabled
	value.WorkspaceEnabled = value.WorkspaceEnabled && current.WorkspaceEnabled
	value.WorkspaceReadEnabled = value.WorkspaceReadEnabled && current.WorkspaceReadEnabled
	value.WorkspaceWriteEnabled = value.WorkspaceWriteEnabled && current.WorkspaceWriteEnabled
	value.WorkspaceExecEnabled = value.WorkspaceExecEnabled && current.WorkspaceExecEnabled
	value.WorkspaceGitEnabled = value.WorkspaceGitEnabled && current.WorkspaceGitEnabled
	if current.WorkspaceCommandTimeoutSecs > 0 && (value.WorkspaceCommandTimeoutSecs <= 0 || current.WorkspaceCommandTimeoutSecs < value.WorkspaceCommandTimeoutSecs) {
		value.WorkspaceCommandTimeoutSecs = current.WorkspaceCommandTimeoutSecs
	}
	value.MemoryEnabled = value.MemoryEnabled && current.MemoryEnabled
	value.MemoryAutoRetrieve = value.MemoryAutoRetrieve && current.MemoryAutoRetrieve
	if current.MemoryMaxResults > 0 && (value.MemoryMaxResults <= 0 || current.MemoryMaxResults < value.MemoryMaxResults) {
		value.MemoryMaxResults = current.MemoryMaxResults
	}
	value.WebSearchEnabled = value.WebSearchEnabled && current.WebSearchEnabled
	if value.WebSearchEnabled {
		value.WebSearchServiceIDs = intersectRuntimeToolNames(value.WebSearchServiceIDs, current.WebSearchServiceIDs)
		value.WebSearchDailyCallLimit = minPositiveRuntimeLimit(value.WebSearchDailyCallLimit, current.WebSearchDailyCallLimit)
		value.WebSearchMaxCallsPerInvocation = minPositiveRuntimeLimit(value.WebSearchMaxCallsPerInvocation, current.WebSearchMaxCallsPerInvocation)
		value.WebSearchAlertPercent = minPositiveRuntimeLimit(value.WebSearchAlertPercent, current.WebSearchAlertPercent)
	}
	value.ModalFallbackEnabled = value.ModalFallbackEnabled && current.ModalFallbackEnabled
	if strings.TrimSpace(value.ModalFallbackProviderID) == "" {
		value.ModalFallbackProviderID = current.ModalFallbackProviderID
	}
	if strings.TrimSpace(value.ModalFallbackVisionModel) == "" {
		value.ModalFallbackVisionModel = current.ModalFallbackVisionModel
	}
	if strings.TrimSpace(value.ModalFallbackAudioModel) == "" {
		value.ModalFallbackAudioModel = current.ModalFallbackAudioModel
	}
	value.AgentMaxToolCalls = minPositiveRuntimeLimit(value.AgentMaxToolCalls, current.AgentMaxToolCalls)
	value.ToolSchemaBudgetTokens = minPositiveRuntimeLimit(value.ToolSchemaBudgetTokens, current.ToolSchemaBudgetTokens)
	currentAllowed := messageConfig.Extensions.AllowedReadOnlyTools
	value.SubAgent.AllowedTools = intersectRuntimeToolNames(value.SubAgent.AllowedTools, currentAllowed)
	if len(current.SubAgent.AllowedTools) > 0 {
		value.SubAgent.AllowedTools = intersectRuntimeToolNames(value.SubAgent.AllowedTools, current.SubAgent.AllowedTools)
	}
	if len(value.SubAgent.AllowedTools) == 0 && len(currentAllowed) > 0 {
		value.SubAgent.AllowedTools = nil
	}
	value.SubAgent.MaxConcurrency = minPositiveRuntimeLimit(value.SubAgent.MaxConcurrency, current.SubAgent.MaxConcurrency)
	value.SubAgent.MaxOutputTokens = minPositiveRuntimeLimit(value.SubAgent.MaxOutputTokens, current.SubAgent.MaxOutputTokens)
	value.SubAgent.InputBudgetBytes = minPositiveRuntimeLimit(value.SubAgent.InputBudgetBytes, current.SubAgent.InputBudgetBytes)
	value.SubAgent.OutputBudgetBytes = minPositiveRuntimeLimit(value.SubAgent.OutputBudgetBytes, current.SubAgent.OutputBudgetBytes)
	enabled := value.SubAgentsEnabled() && current.SubAgentsEnabled()
	value.SubAgentEnabled = &enabled
	return &value, nil
}

func boolPointer(value bool) *bool {
	return &value
}

func subAgentOptionsFromSettings(value configsvc.SubAgentSettings) agent.SubAgentOptions {
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

func minPositiveRuntimeLimit(created, current int) int {
	if created <= 0 {
		return current
	}
	if current <= 0 || current < created {
		return current
	}
	return created
}

func intersectRuntimeToolNames(created, current []string) []string {
	allowed := make(map[string]struct{}, len(current))
	for _, name := range current {
		name = strings.TrimSpace(name)
		if name != "" {
			allowed[name] = struct{}{}
		}
	}
	result := make([]string, 0, len(created))
	seen := make(map[string]struct{}, len(created))
	for _, name := range created {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if len(allowed) > 0 {
			if _, ok := allowed[name]; !ok {
				continue
			}
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// createBotRuntimeFollowUp 把模型提交的结构化动作转换为持久调度任务。来源、
// 用户和会话全部取自动作边界，模型只能填写任务内容和时间，不能伪造投递目标。
func createBotRuntimeFollowUp(ctx context.Context, action bot.PlatformAction, service *schedule.Service, configService *configsvc.Service, botManager *bot.Manager, kernel *agent.Kernel) (string, error) {
	if service == nil || configService == nil || botManager == nil || kernel == nil {
		return "", errors.New("Follow-up 调度服务未装配")
	}
	message := action.Message
	botID := strings.TrimSpace(message.AdapterID)
	conversationID := strings.TrimSpace(message.RuntimeConversationID)
	source := strings.TrimSpace(message.SourceUMO)
	if botID == "" || conversationID == "" || source == "" || strings.TrimSpace(message.ChatID) == "" {
		return "", errors.New("Follow-up 缺少明确的机器人、会话、来源或聊天目标")
	}
	runtime, err := configService.Resolve(ctx, botID, conversationID)
	if err != nil {
		return "", fmt.Errorf("读取 Follow-up 配置失败: %w", err)
	}
	// Follow-up 创建与普通入站消息使用同一解析器，确保机器人级覆盖和会话来源规则
	// 在任务创建时就真正生效，而不是只在 WebUI 中显示为已保存。
	resolvedMessageConfig, configErr := botManager.ResolveMessageConfig(ctx, message)
	if configErr != nil {
		return "", fmt.Errorf("读取 Follow-up 消息配置失败: %w", configErr)
	}
	applyBotExtensionToRuntime(&runtime, resolvedMessageConfig.Extensions)
	settings, settingsErr := configService.GetSystemSettings(ctx)
	if settingsErr != nil {
		return "", fmt.Errorf("读取 Follow-up 系统配置失败: %w", settingsErr)
	}
	effectiveRuntime, effectiveErr := kernel.ResolveRuntimeOptions(ctx, agent.ChatRequest{UserID: message.UserID, BotID: botID, ConversationID: conversationID, SessionID: conversationID})
	if effectiveErr != nil {
		return "", fmt.Errorf("读取 Follow-up Agent 配置失败: %w", effectiveErr)
	}
	if !runtime.Extensions.FollowUpEnabled {
		return "", errors.New("当前机器人已关闭 Follow-up")
	}
	if len(runtime.Extensions.FollowUpAllowedSources) > 0 && !runtimeStringListContains(runtime.Extensions.FollowUpAllowedSources, source) {
		return "", errors.New("当前消息来源不允许创建 Follow-up")
	}
	maxFollowUps := runtime.Extensions.FollowUpMax
	if maxFollowUps <= 0 {
		maxFollowUps = 16
	}
	followUpID := ""
	if idempotencyKey := strings.TrimSpace(runtimeActionPayloadString(action.Payload, "idempotency_key")); idempotencyKey != "" {
		followUpID = "runtime-follow-up-" + idempotencyKey
	}
	activeCount, err := service.CountActive(ctx, botID)
	if err != nil {
		return "", fmt.Errorf("读取现有 Follow-up 失败: %w", err)
	}
	if followUpID != "" {
		// 幂等重试需要排除已经存在的同一任务；只读取这一条，不再扫描全部任务。
		if existing, getErr := service.Get(ctx, followUpID); getErr == nil && existing.AdapterID == botID {
			switch existing.Status {
			case schedule.StatusPending, schedule.StatusScheduled, schedule.StatusRunning, schedule.StatusWaiting:
				activeCount--
			}
		} else if getErr != nil && !errors.Is(getErr, schedule.ErrNotFound) {
			return "", fmt.Errorf("读取 Follow-up 幂等任务失败: %w", getErr)
		}
	}
	if activeCount >= maxFollowUps {
		return "", fmt.Errorf("当前机器人 Follow-up 已达到上限 %d", maxFollowUps)
	}
	payload := action.Payload
	request := strings.TrimSpace(runtimeActionPayloadString(payload, "request"))
	if request == "" {
		return "", errors.New("Follow-up request 不能为空")
	}
	name := strings.TrimSpace(runtimeActionPayloadString(payload, "name"))
	if name == "" {
		name = "Bot Runtime 跟进任务"
	}
	goal := strings.TrimSpace(runtimeActionPayloadString(payload, "goal"))
	if goal == "" {
		goal = request
	}
	mode := schedule.Mode(strings.ToLower(strings.TrimSpace(runtimeActionPayloadString(payload, "mode"))))
	if mode == "" {
		mode = schedule.ModeOnce
	}
	now := time.Now().UTC()
	var startAt *time.Time
	if dueAt := strings.TrimSpace(runtimeActionPayloadString(payload, "due_at")); dueAt != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, dueAt)
		if parseErr != nil {
			parsed, parseErr = time.Parse(time.RFC3339, dueAt)
		}
		if parseErr != nil {
			return "", fmt.Errorf("Follow-up due_at 必须是 RFC3339: %w", parseErr)
		}
		parsed = parsed.UTC()
		startAt = &parsed
	} else if mode == schedule.ModeOnce {
		// 模型没有给出明确时间时只安排一个很短但非零的延迟，避免在
		// 当前模型调用尚未结束时重入同一会话。
		fallback := now.Add(time.Minute)
		startAt = &fallback
	}
	if startAt != nil && !startAt.After(now) {
		fallback := now.Add(time.Second)
		startAt = &fallback
	}
	quietStart := runtimeActionPayloadString(payload, "quiet_hours_start")
	quietEnd := runtimeActionPayloadString(payload, "quiet_hours_end")
	if strings.TrimSpace(quietStart) == "" {
		quietStart = runtime.Extensions.QuietHoursStart
	}
	if strings.TrimSpace(quietEnd) == "" {
		quietEnd = runtime.Extensions.QuietHoursEnd
	}
	maxRetries := runtimeActionPayloadInt(payload, "max_retries")
	if !runtimeActionPayloadPresent(payload, "max_retries") {
		maxRetries = runtime.Extensions.FollowUpMaxRetries
	}
	retryDelaySeconds := runtimeActionPayloadInt(payload, "retry_delay_seconds")
	if !runtimeActionPayloadPresent(payload, "retry_delay_seconds") {
		retryDelaySeconds = runtime.Extensions.FollowUpRetryDelaySeconds
	}
	maxDelaySeconds := runtimeActionPayloadInt(payload, "max_delay_seconds")
	if !runtimeActionPayloadPresent(payload, "max_delay_seconds") {
		maxDelaySeconds = runtime.Extensions.FollowUpMaxDelaySeconds
	}
	task := schedule.Task{
		Name: name, Request: request, SourceUMO: source, OriginTurnID: message.RuntimeTurnID,
		Kind: "runtime_follow_up", Goal: goal, Recurrence: string(mode), DeliveryPolicy: "bot_runtime",
		Mode: mode, StartAt: startAt, IntervalSeconds: runtimeActionPayloadInt(payload, "interval_seconds"),
		Weekday: runtimeActionPayloadInt(payload, "weekday"), MonthDay: runtimeActionPayloadInt(payload, "month_day"),
		TimeOfDay: runtimeActionPayloadString(payload, "time_of_day"), Cron: runtimeActionPayloadString(payload, "cron"),
		UserID: message.UserID, ConversationID: conversationID, AdapterID: botID, ChatID: message.ChatID,
		Status: schedule.StatusScheduled, MaxRetries: maxRetries,
		RetryDelaySeconds: retryDelaySeconds, MaxDelaySeconds: maxDelaySeconds,
		QuietHoursStart: quietStart, QuietHoursEnd: quietEnd,
		Timezone: runtimeActionPayloadString(payload, "timezone"),
	}
	// 调度器任务 ID 使用动作幂等键派生；动作在网络成功但本地确认丢失后
	// 重试时，Service.Save 会更新同一任务，避免重复主动消息。
	task.ID = followUpID
	if strings.TrimSpace(task.Timezone) == "" {
		task.Timezone = runtime.Extensions.QuietHoursTimezone
	}
	configSnapshot, snapshotErr := encodeBotRuntimeFollowUpSnapshot(runtime, settings, &effectiveRuntime)
	if snapshotErr != nil {
		return "", snapshotErr
	}
	task.ConfigSnapshot = configSnapshot
	saved, err := service.Save(ctx, task)
	if err != nil {
		return "", err
	}
	slog.Info("Bot Runtime Follow-up 已创建", "follow_up_id", saved.ID, "adapter_id", botID, "source", source, "mode", saved.Mode)
	return saved.ID, nil
}

func runtimeActionPayloadString(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	value, ok := payload[key]
	if !ok || value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func runtimeActionPayloadInt(payload map[string]any, key string) int {
	if payload == nil {
		return 0
	}
	value, ok := payload[key]
	if !ok || value == nil {
		return 0
	}
	switch number := value.(type) {
	case int:
		return number
	case int32:
		return int(number)
	case int64:
		return int(number)
	case float64:
		return int(number)
	case json.Number:
		parsed, _ := number.Int64()
		return int(parsed)
	default:
		parsed, _ := strconv.Atoi(strings.TrimSpace(fmt.Sprint(value)))
		return parsed
	}
}

func runtimeActionPayloadPresent(payload map[string]any, key string) bool {
	if payload == nil {
		return false
	}
	_, ok := payload[key]
	return ok
}

func runtimeStringListContains(values []string, target string) bool {
	target = strings.TrimSpace(target)
	for _, value := range values {
		if strings.TrimSpace(value) == target {
			return true
		}
	}
	return false
}
