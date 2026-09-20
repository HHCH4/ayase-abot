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
	// Extensions 在每条消息和每次投递时解析，保证配置文件保存后立即生效。
	Extensions ExtensionConfig
}

// ExtensionConfig 仅包含平台消息入口需要的扩展策略，Bot 包不依赖配置中心。
type ExtensionConfig struct {
	SegmentedReplyEnabled     bool
	SegmentOnlyLLM            bool
	SegmentIntervalMethod     string
	SegmentInterval           string
	SegmentLogBase            float64
	SegmentWordsThreshold     int
	SegmentSplitMode          string
	SegmentRegex              string
	SegmentSplitWords         []string
	SegmentCleanupRegex       string
	GroupContextEnabled       bool
	GroupMessageMaxCount      int
	GroupImageCaption         bool
	GroupImageCaptionModel    string
	ProactiveReplyEnabled     bool
	ProactiveReplyMethod      string
	ProactiveReplyProbability float64
	ProactiveReplyWhitelist   []string
}

// MessageConfigResolver 按机器人和基础会话读取当前生效的平台配置。
type MessageConfigResolver func(context.Context, string, string) (MessageConfig, error)

// SetMessageConfigResolver 安装平台消息配置读取器；未安装时保留原有 Bot 默认行为。
func (m *Manager) SetMessageConfigResolver(resolver MessageConfigResolver) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.messageConfigResolver = resolver
	m.mu.Unlock()
}

// resolveMessageConfig 读取一条消息对应的配置；空读取器表示嵌入式调用方没有配置中心。
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
		return MessageConfig{}, nil
	}
	return resolver(ctx, strings.TrimSpace(message.AdapterID), conversationID)
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
