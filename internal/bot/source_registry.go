package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// MessageSource 是一个已经从平台消息入口观察到的 UMO。
//
// 它和 Conversation 有意分开：AstrBot 的来源下拉框展示的是“收到过消息的
// 来源”，而不是“已经进入 AI 并创建了会话的来源”。这样未唤醒、被权限规则
// 拦截或仅发送内置指令的消息，也能进入自定义规则的可选目录。
type MessageSource struct {
	Source      string    `json:"source"`
	AdapterID   string    `json:"adapter_id"`
	Platform    string    `json:"platform"`
	MessageType string    `json:"message_type"`
	SessionID   string    `json:"session_id"`
	UserID      string    `json:"user_id"`
	AutoName    string    `json:"auto_name,omitempty"`
	FirstSeenAt time.Time `json:"first_seen_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

// MessageSourceRegistry 是平台来源目录的持久化扩展接口。
//
// Repository 保持旧的最小契约不变；只有支持来源目录的实现才需要实现此
// 接口，因此内存嵌入方和已有测试不会被迫增加无关方法。
type MessageSourceRegistry interface {
	UpsertMessageSource(context.Context, MessageSource) error
	ListMessageSources(context.Context, string) ([]MessageSource, error)
}

// recordMessageSource 在消息处理的最早阶段登记 UMO，不能放在唤醒词、权限或
// 命令分流之后，否则 WebUI 会再次退化为“只有进过 AI 的会话才看得到”。
func (m *Manager) recordMessageSource(ctx context.Context, message Message) {
	if m == nil || m.repository == nil {
		return
	}
	registry, ok := m.repository.(MessageSourceRegistry)
	if !ok {
		return
	}
	adapterID := strings.TrimSpace(message.AdapterID)
	platform := strings.TrimSpace(string(message.Platform))
	sessionID := strings.TrimSpace(messageSessionID(message))
	if (adapterID == "" && platform == "") || sessionID == "" {
		// 平台握手、心跳或测试构造的空事件没有稳定 UMO，不写入来源目录。
		return
	}
	now := time.Now().UTC()
	if adapterID == "" {
		adapterID = platform
	}
	autoName := strings.TrimSpace(message.AutoName)
	if autoName == "" {
		autoName = defaultSourceAutoName(message, sessionID)
	}
	item := MessageSource{
		Source:      messageSource(message),
		AdapterID:   adapterID,
		Platform:    platform,
		MessageType: messageUMOMessageType(message),
		SessionID:   sessionID,
		UserID:      strings.TrimSpace(message.UserID),
		AutoName:    autoName,
		LastSeenAt:  now,
	}
	if err := registry.UpsertMessageSource(ctx, item); err != nil {
		// 来源目录是管理台辅助数据，登记失败不能阻断平台消息；但必须打日志，
		// 让部署排查时能区分“没有消息”与“目录写入失败”。
		slog.Warn("登记机器人消息来源失败", "source", item.Source, "adapter_id", adapterID, "platform", platform, "error", err)
	}
}

// defaultSourceAutoName 为没有昵称元数据的平台提供稳定的可读兜底名；用户
// 通过 /name 设置的手工别名会在展示层优先于这个自动名称。
func defaultSourceAutoName(message Message, sessionID string) string {
	if messageUMOMessageType(message) == "GroupMessage" {
		return fmt.Sprintf("群聊 %s", sessionID)
	}
	userID := strings.TrimSpace(message.UserID)
	if userID == "" {
		userID = sessionID
	}
	if messageUMOMessageType(message) == "FriendMessage" {
		return fmt.Sprintf("用户 %s", userID)
	}
	return fmt.Sprintf("会话 %s", sessionID)
}
