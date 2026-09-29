package bot

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
)

// MessageConfig 是消息入口真正需要的平台级配置，避免 Bot 管理器依赖配置中心的内部类型。
type MessageConfig struct {
	AdminUserIDs          []string
	WakeupWords           []string
	PrivateRequiresWakeup bool
	Platform              PlatformConfig
	// Extensions 在每条消息和每次投递时解析，保证配置文件保存后立即生效。
	Extensions ExtensionConfig
}

// PlatformConfig 是 Bot 可以即时执行的平台策略快照。
type PlatformConfig struct {
	UniqueSession          bool
	ReplyPrefix            string
	ReplyMention           bool
	ReplyQuote             bool
	PrivateReplyQuote      bool
	WhitelistEnabled       bool
	WhitelistIDs           []string
	WhitelistLog           bool
	WhitelistAdminGroup    bool
	WhitelistAdminPrivate  bool
	RateLimitSeconds       int
	RateLimitCount         int
	RateLimitStrategy      string
	IgnoreBotSelfMessage   bool
	IgnoreAtAll            bool
	DisableBuiltinCommands bool
	NoPermissionReply      bool
	EmptyMentionWaiting    bool
	EmptyMentionNeedReply  bool
	BlockPatterns          []string
	CheckResponse          bool
	TelegramPreAckEnabled  bool
	TelegramPreAckEmoji    string
}

// ExtensionConfig 仅包含平台消息入口需要的扩展策略，Bot 包不依赖配置中心。
type ExtensionConfig struct {
	RuntimeEnabled             bool
	RuntimeMaxConcurrency      int
	SourceQueueLimit           int
	TurnWaitMilliseconds       int
	GroupTurnWaitMilliseconds  int
	AttachmentWaitMilliseconds int
	MaxTurnMessages            int
	PrivateMode                string
	GroupParticipationMode     string
	// RecordUnaddressedMessages 只影响未唤醒群消息是否进入持久背景，不会放宽回复条件。
	RecordUnaddressedMessages bool
	// ReactionEnabled 是表情回应的行为开关，平台动作仍受权限和能力检查约束。
	ReactionEnabled             bool
	RelationEnabled             bool
	AgentOnDemandEnabled        bool
	GroupContextEnabled         bool
	GroupMessageMaxCount        int
	GroupImageCaption           bool
	GroupImageCaptionModel      string
	ExpressionEnabled           bool
	ExpressionMaxSegments       int
	ExpressionLongThreshold     int
	ExpressionDelayMilliseconds int
	ProactiveEnabled            bool
	ProactiveDegree             string
	CooldownSeconds             int
	PrivateHourlyReplyLimit     int
	GroupHourlyReplyLimit       int
	HeartbeatSeconds            int
	QuietHoursTimezone          string
	QuietHoursStart             string
	QuietHoursEnd               string
	EmergencyBypassQuietHours   bool
	RelationRetentionSeconds    int
	FollowUpEnabled             bool
	FollowUpMax                 int
	FollowUpMaxRetries          int
	FollowUpRetryDelaySeconds   int
	FollowUpMaxDelaySeconds     int
	FollowUpAllowedSources      []string
	AllowedReadOnlyTools        []string
	ToolBudget                  int
	ActionPermissions           map[string]bool
	MessageStyle                string
}

// ApplyBotRuntimeConfig 把机器人页面显式填写的字段覆盖到已解析配置，空指针字段
// 保持继承结果不变；复制切片和映射避免一次请求修改全局配置快照。
func (config *MessageConfig) ApplyBotRuntimeConfig(runtime BotRuntimeConfig) {
	if config == nil {
		return
	}
	value := runtime.NormalizeRuntimeConfig()
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
	applyBool(&config.Extensions.RuntimeEnabled, value.RuntimeEnabled)
	applyInt(&config.Extensions.RuntimeMaxConcurrency, value.RuntimeMaxConcurrency)
	applyInt(&config.Extensions.SourceQueueLimit, value.SourceQueueLimit)
	applyInt(&config.Extensions.TurnWaitMilliseconds, value.TurnWaitMilliseconds)
	applyInt(&config.Extensions.GroupTurnWaitMilliseconds, value.GroupTurnWaitMilliseconds)
	applyInt(&config.Extensions.AttachmentWaitMilliseconds, value.AttachmentWaitMilliseconds)
	applyInt(&config.Extensions.MaxTurnMessages, value.MaxTurnMessages)
	applyString(&config.Extensions.PrivateMode, value.PrivateMode)
	applyString(&config.Extensions.GroupParticipationMode, value.GroupParticipationMode)
	applyBool(&config.Extensions.RecordUnaddressedMessages, value.RecordUnaddressedMessages)
	applyBool(&config.Extensions.ReactionEnabled, value.ReactionEnabled)
	applyBool(&config.Extensions.GroupContextEnabled, value.GroupContextEnabled)
	applyInt(&config.Extensions.GroupMessageMaxCount, value.GroupMessageMaxCount)
	applyBool(&config.Extensions.GroupImageCaption, value.GroupImageCaption)
	applyString(&config.Extensions.GroupImageCaptionModel, value.GroupImageCaptionModel)
	applyBool(&config.Extensions.ProactiveEnabled, value.ProactiveEnabled)
	applyString(&config.Extensions.ProactiveDegree, value.ProactiveDegree)
	applyInt(&config.Extensions.CooldownSeconds, value.CooldownSeconds)
	applyInt(&config.Extensions.PrivateHourlyReplyLimit, value.PrivateHourlyReplyLimit)
	applyInt(&config.Extensions.GroupHourlyReplyLimit, value.GroupHourlyReplyLimit)
	applyInt(&config.Extensions.HeartbeatSeconds, value.HeartbeatSeconds)
	applyString(&config.Extensions.QuietHoursTimezone, value.QuietHoursTimezone)
	applyString(&config.Extensions.QuietHoursStart, value.QuietHoursStart)
	applyString(&config.Extensions.QuietHoursEnd, value.QuietHoursEnd)
	applyBool(&config.Extensions.EmergencyBypassQuietHours, value.EmergencyBypassQuietHours)
	applyBool(&config.Extensions.RelationEnabled, value.RelationEnabled)
	applyInt(&config.Extensions.RelationRetentionSeconds, value.RelationRetentionSeconds)
	applyBool(&config.Extensions.FollowUpEnabled, value.FollowUpEnabled)
	applyInt(&config.Extensions.FollowUpMax, value.FollowUpMax)
	applyInt(&config.Extensions.FollowUpMaxRetries, value.FollowUpMaxRetries)
	applyInt(&config.Extensions.FollowUpRetryDelaySeconds, value.FollowUpRetryDelaySeconds)
	applyInt(&config.Extensions.FollowUpMaxDelaySeconds, value.FollowUpMaxDelaySeconds)
	applyBool(&config.Extensions.ExpressionEnabled, value.ExpressionEnabled)
	applyInt(&config.Extensions.ExpressionMaxSegments, value.ExpressionMaxSegments)
	applyInt(&config.Extensions.ExpressionLongThreshold, value.ExpressionLongThreshold)
	applyInt(&config.Extensions.ExpressionDelayMilliseconds, value.ExpressionDelayMilliseconds)
	applyBool(&config.Platform.ReplyMention, value.ReplyMention)
	applyBool(&config.Platform.ReplyQuote, value.ReplyQuote)
	applyBool(&config.Platform.PrivateReplyQuote, value.PrivateReplyQuote)
	applyBool(&config.Extensions.AgentOnDemandEnabled, value.AgentOnDemandEnabled)
	applyInt(&config.Extensions.ToolBudget, value.ToolBudget)
	applyString(&config.Extensions.MessageStyle, value.MessageStyle)
	if value.FollowUpAllowedSources != nil {
		config.Extensions.FollowUpAllowedSources = append([]string(nil), value.FollowUpAllowedSources...)
	}
	if value.AllowedReadOnlyTools != nil {
		config.Extensions.AllowedReadOnlyTools = append([]string(nil), value.AllowedReadOnlyTools...)
	}
	if value.ActionPermissions != nil {
		config.Extensions.ActionPermissions = make(map[string]bool, len(value.ActionPermissions))
		for key, enabled := range value.ActionPermissions {
			config.Extensions.ActionPermissions[key] = enabled
		}
	}
}

// MessageConfigResolver 按机器人、基础会话和稳定来源读取当前生效的平台配置。
// sourceUMO 在首次消息尚未创建 Conversation 时仍然可用，保证来源规则先于准入判断生效。
type MessageConfigResolver func(context.Context, string, string, string) (MessageConfig, error)

// SetMessageConfigResolver 安装平台消息配置读取器；未安装时使用安全默认值。
func (m *Manager) SetMessageConfigResolver(resolver MessageConfigResolver) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.messageConfigResolver = resolver
	m.mu.Unlock()
}

// ResolveMessageConfig 读取一条消息对应的当前配置，供消息入口和恢复任务共用同一套来源规则。
func (m *Manager) ResolveMessageConfig(ctx context.Context, message Message) (MessageConfig, error) {
	return m.resolveMessageConfig(ctx, message)
}

// resolveMessageConfig 读取一条消息对应的配置；空读取器表示调用方没有配置中心。
func (m *Manager) resolveMessageConfig(ctx context.Context, message Message) (MessageConfig, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	_, conversationID := bindingFor(message)
	m.mu.RLock()
	resolver := m.messageConfigResolver
	// /new 后优先读取当前会话绑定，避免仍使用旧基础会话的扩展配置。
	if current := strings.TrimSpace(m.activeConversations[chatBindingKey(message)]); current != "" {
		conversationID = current
	}
	m.mu.RUnlock()
	if resolver == nil {
		// 未装配配置中心时只使用收紧后的安全默认值，不绕过运行时权限检查。
		return MessageConfig{Platform: PlatformConfig{NoPermissionReply: true}, Extensions: ExtensionConfig{RuntimeEnabled: true, RuntimeMaxConcurrency: 4, SourceQueueLimit: 50, MaxTurnMessages: 12, PrivateMode: "responsive", GroupParticipationMode: "addressed_only", RelationEnabled: true, AgentOnDemandEnabled: true, ExpressionEnabled: true, ExpressionMaxSegments: 4}}, nil
	}
	return resolver(ctx, strings.TrimSpace(message.AdapterID), conversationID, messageSource(message))
}

// globalAdminForMessage 将 Bot 专属管理员与配置文件管理员合并，保持两种 WebUI 配置入口一致。
func (m *Manager) globalAdminForMessage(ctx context.Context, bot Bot, message Message) bool {
	if isGlobalAdmin(bot, message.UserID) {
		return true
	}
	config, err := m.resolveMessageConfig(ctx, message)
	if err != nil {
		// 配置读取失败时只回退到 Bot 专属管理员，不能因为故障意外放宽权限。
		slog.Warn("读取平台管理员配置失败", "adapter_id", message.AdapterID, "error", err)
		return false
	}
	return containsUserID(config.AdminUserIDs, message.UserID)
}

// globalAdminForUser 用于判断管理命令目标是否已经是全局管理员。
func (m *Manager) globalAdminForUser(ctx context.Context, bot Bot, message Message, userID string) bool {
	userID = strings.TrimSpace(userID)
	if isGlobalAdmin(bot, userID) {
		return true
	}
	config, err := m.resolveMessageConfig(ctx, message)
	if err != nil {
		slog.Warn("读取平台管理员配置失败", "adapter_id", message.AdapterID, "error", err)
		return false
	}
	return containsUserID(config.AdminUserIDs, userID)
}

func containsUserID(values []string, userID string) bool {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false
	}
	for _, value := range values {
		if strings.TrimSpace(value) == userID {
			return true
		}
	}
	return false
}

// globalAdminIDsForMessage 返回当前 Bot 和配置文件共同生效的管理员，供 /admin list 展示。
func (m *Manager) globalAdminIDsForMessage(ctx context.Context, bot Bot, message Message) []string {
	config, err := m.resolveMessageConfig(ctx, message)
	if err != nil {
		slog.Warn("读取平台管理员配置失败", "adapter_id", message.AdapterID, "error", err)
	}
	values := append(append([]string(nil), bot.AdminUserIDs...), config.AdminUserIDs...)
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

// messageAllowed 应用唤醒词、私聊唤醒和群聊触发规则，并在匹配唤醒词后去掉前缀。
func (m *Manager) messageAllowed(ctx context.Context, message *Message) (bool, error) {
	if message == nil {
		return false, errors.New("消息不能为空")
	}
	config, err := m.resolveMessageConfig(ctx, *message)
	if err != nil {
		return false, err
	}
	message.Text = strings.TrimSpace(message.Text)
	// 斜杠命令本身就是明确的唤醒动作，不能被普通消息唤醒策略拦截。
	if _, isCommand := parseBotCommand(message.Text); isCommand {
		return true, nil
	}
	if prefix, matched := matchedWakeupWord(message.Text, config.WakeupWords); matched {
		message.Text = strings.TrimSpace(strings.TrimPrefix(message.Text, prefix))
		return true, nil
	}
	if isGroupChat(message.ChatType) {
		// 来源规则可以把群聊收紧为“仅被点名”或“仅观察”；机器人级旧触发模式
		// 只作为没有来源规则时的默认值，不能覆盖更具体的来源配置。
		mode := strings.TrimSpace(config.Extensions.GroupParticipationMode)
		if mode == "addressed_only" || mode == "observe_only" {
			return message.Mentioned || strings.TrimSpace(message.ReplyToMessageID) != "", nil
		}
		return m.groupMessageAllowed(*message), nil
	}
	if config.PrivateRequiresWakeup && !message.Mentioned {
		return false, nil
	}
	return true, nil
}

// matchedWakeupWord 选择最长前缀，避免同时配置“/”和“/bot”时短前缀抢先匹配。
func matchedWakeupWord(text string, words []string) (string, bool) {
	text = strings.TrimSpace(text)
	candidates := make([]string, 0, len(words))
	for _, word := range words {
		word = strings.TrimSpace(word)
		if word != "" && strings.HasPrefix(text, word) {
			candidates = append(candidates, word)
		}
	}
	if len(candidates) == 0 {
		return "", false
	}
	sort.SliceStable(candidates, func(i, j int) bool { return len(candidates[i]) > len(candidates[j]) })
	return candidates[0], true
}
