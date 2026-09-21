package bot

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// platformMessageAllowed 在模型和附件入库之前执行平台白名单与事件过滤。
func (m *Manager) platformMessageAllowed(message Message, config MessageConfig) bool {
	policy := config.Platform
	if policy.IgnoreBotSelfMessage && message.IsSelf || policy.IgnoreAtAll && message.AtAll {
		return false
	}
	if !policy.WhitelistEnabled || len(policy.WhitelistIDs) == 0 {
		return true
	}
	for _, value := range policy.WhitelistIDs {
		value = strings.TrimSpace(value)
		if value != "" && (value == messageSource(message) || value == message.ChatID || value == message.UserID) {
			return true
		}
	}
	// 只有已配置的平台管理员可绕过白名单，普通群成员不能靠提示词声明身份。
	bot, err := m.Get(strings.TrimSpace(message.AdapterID))
	admin := err == nil && isGlobalAdmin(bot, message.UserID) || containsUserID(config.AdminUserIDs, message.UserID)
	if admin && (isGroupChat(message.ChatType) && policy.WhitelistAdminGroup || !isGroupChat(message.ChatType) && policy.WhitelistAdminPrivate) {
		return true
	}
	if policy.WhitelistLog {
		slog.Info("机器人消息未通过平台白名单", "source", messageSource(message), "user_id", message.UserID)
	}
	return false
}

// blockedByPattern 仅检查显式配置的正则，不猜测用户意图或授予任何执行权限。
func blockedByPattern(text string, patterns []string) bool {
	for _, pattern := range patterns {
		if len(pattern) == 0 || len(pattern) > 256 {
			continue
		}
		if matcher, err := regexp.Compile(pattern); err == nil && matcher.MatchString(text) {
			return true
		}
	}
	return false
}

// sendPlatformPreAck 只对 Telegram 接受的普通消息调用可选预回应，失败不影响入队。
func (m *Manager) sendPlatformPreAck(ctx context.Context, message Message, policy PlatformConfig) {
	if !policy.TelegramPreAckEnabled || message.Platform != TypeTelegram {
		return
	}
	m.mu.RLock()
	entry, running := m.runtimes[message.AdapterID]
	m.mu.RUnlock()
	if !running {
		return
	}
	ack, ok := entry.platform.(interface {
		PreAcknowledge(context.Context, Message, string) error
	})
	if !ok {
		return
	}
	ackCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ack.PreAcknowledge(ackCtx, message, policy.TelegramPreAckEmoji); err != nil {
		slog.Warn("Telegram 预回应表情失败", "adapter_id", message.AdapterID, "chat_id", message.ChatID, "error", err)
	}
}

// mentionWaitKey 用群聊天键与发言者 ID 限定下一条消息，其他成员不能搭便车。
func mentionWaitKey(message Message) string {
	return chatBindingKey(message) + "\x00" + strings.TrimSpace(message.UserID)
}

// startMentionWait 保存一分钟的等待窗口，并清理过期项控制内存占用。
func (m *Manager) startMentionWait(message Message) {
	now := time.Now()
	m.mu.Lock()
	if m.mentionWait == nil {
		m.mentionWait = make(map[string]time.Time)
	}
	for key, expires := range m.mentionWait {
		if !expires.After(now) {
			delete(m.mentionWait, key)
		}
	}
	if len(m.mentionWait) < 512 {
		m.mentionWait[mentionWaitKey(message)] = now.Add(time.Minute)
	}
	m.mu.Unlock()
}

// consumeMentionWait 仅让同一成员的下一条非空消息使用一次等待资格。
func (m *Manager) consumeMentionWait(message Message) bool {
	key := mentionWaitKey(message)
	m.mu.Lock()
	expires := m.mentionWait[key]
	delete(m.mentionWait, key)
	m.mu.Unlock()
	return expires.After(time.Now())
}

// waitPlatformRateLimit 为普通模型消息维护滑动时间窗，管理命令与审批不受阻塞。
func (m *Manager) waitPlatformRateLimit(ctx context.Context, message Message, settings PlatformConfig) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	window := time.Duration(settings.RateLimitSeconds) * time.Second
	count := settings.RateLimitCount
	if window <= 0 || count <= 0 {
		return true
	}
	key := chatBindingKey(message)
	for {
		now := time.Now()
		m.mu.Lock()
		if m.rateWindows == nil {
			m.rateWindows = make(map[string][]time.Time)
		}
		if _, exists := m.rateWindows[key]; !exists {
			if len(m.rateOrder) >= 256 {
				delete(m.rateWindows, m.rateOrder[0])
				m.rateOrder = m.rateOrder[1:]
			}
			m.rateOrder = append(m.rateOrder, key)
		}
		entries := m.rateWindows[key][:0]
		for _, timestamp := range m.rateWindows[key] {
			if now.Sub(timestamp) < window {
				entries = append(entries, timestamp)
			}
		}
		if len(entries) < count {
			m.rateWindows[key] = append(entries, now)
			m.mu.Unlock()
			return true
		}
		wait := window - now.Sub(entries[0])
		m.rateWindows[key] = entries
		m.mu.Unlock()
		if settings.RateLimitStrategy == "discard" {
			slog.Info("机器人消息触发平台速率限制，已丢弃", "source", messageSource(message))
			return false
		}
		// stall 仅等待当前窗口到期；上下文取消时立即退出。
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}
