package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"Abot/internal/agent"
)

const (
	maxGroupHistoryEntryRunes = 500
	maxGroupContextRunes      = 12000
)

// SetGroupImageCaptioner 安装单张群图片转述能力；失败时历史仍保留图片占位符。
func (m *Manager) SetGroupImageCaptioner(caption func(context.Context, string, agent.Attachment) (string, error)) {
	m.mu.Lock()
	m.groupImageCaption = caption
	m.mu.Unlock()
}

// recordGroupContext 将群聊背景写入持久来源状态。未唤醒消息也会记录，但指令和
// 审批控制不会进入自然语言上下文。
func (m *Manager) recordGroupContext(ctx context.Context, message Message, settings ExtensionConfig) {
	if !isGroupChat(message.ChatType) || message.Control != nil {
		return
	}
	// 群上下文开关决定是否读取背景；记录开关只决定未唤醒消息是否进入背景。
	// 被 @、引用或“全量参与”策略接纳的消息属于当前话题，即使不记录未唤醒消息，
	// 在启用上下文时也必须保留，避免模型只能看到未唤醒的旁枝内容。
	unaddressed := !message.Mentioned && strings.TrimSpace(message.ReplyToMessageID) == "" && !m.groupMessageAllowed(message)
	if !settings.GroupContextEnabled && !settings.RecordUnaddressedMessages {
		return
	}
	if unaddressed && !settings.RecordUnaddressedMessages {
		return
	}
	if _, command := parseBotCommand(message.Text); command {
		return
	}
	text := strings.TrimSpace(message.Text)
	for index, attachment := range message.Attachments {
		if index >= 2 {
			break
		}
		mimeType := attachment.MIMEType
		if attachment.Ref != nil && mimeType == "" {
			mimeType = attachment.Ref.MIMEType
		}
		if !strings.HasPrefix(mimeType, "image/") {
			continue
		}
		caption := "[图片]"
		m.mu.RLock()
		captioner := m.groupImageCaption
		m.mu.RUnlock()
		if settings.GroupImageCaption && settings.GroupImageCaptionModel != "" && captioner != nil {
			captionCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			result, err := captioner(captionCtx, settings.GroupImageCaptionModel, attachment)
			cancel()
			if err != nil {
				slog.Warn("群图片转述失败", "source", messageSource(message), "error", err)
			} else if strings.TrimSpace(result) != "" {
				caption = "[图片：" + boundedGroupText(result, 300) + "]"
			}
		}
		text += " " + caption
	}
	if strings.TrimSpace(text) == "" {
		return
	}
	name := boundedGroupText(message.AutoName, 64)
	if name == "" {
		name = boundedGroupText(message.UserID, 64)
	}
	// 展示时间使用当前来源配置的 IANA 时区，存储时间仍统一为 UTC，避免 WebUI
	// 与群背景出现“日志是一个时区、上下文又是另一个时区”的混淆。
	now := time.Now().UTC()
	if timezone := strings.TrimSpace(settings.QuietHoursTimezone); timezone != "" {
		if location, locationErr := time.LoadLocation(timezone); locationErr == nil {
			now = now.In(location)
		}
	}
	entry := ContextEntry{
		ID: runtimeInboxID(message), BotID: message.AdapterID, Source: messageSource(message), UserID: message.UserID,
		Text: fmt.Sprintf("[%s/%s] %s", name, now.Format("15:04:05"), boundedGroupText(text, maxGroupHistoryEntryRunes)), CreatedAt: now.UTC(),
	}
	limit := settings.GroupMessageMaxCount
	if limit <= 0 || limit > 300 {
		limit = 300
	}
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository == nil {
		return
	}
	if err := repository.AppendContext(ctx, entry, limit); err != nil {
		slog.Warn("保存群聊上下文失败", "source", entry.Source, "error", err)
	}
}

func (m *Manager) clearGroupContext(source string) {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository == nil || strings.TrimSpace(source) == "" {
		return
	}
	if err := repository.DeleteContext(m.runtimeContext(), source); err != nil {
		slog.Warn("清理群聊上下文失败", "source", source, "error", err)
	}
}

// groupContextText 只读取同一 UMO 的持久背景，并再次限制字符数以保护模型上下文。
func (m *Manager) groupContextText(ctx context.Context, message Message, settings ExtensionConfig) string {
	if !isGroupChat(message.ChatType) || !settings.GroupContextEnabled {
		return ""
	}
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository == nil {
		return ""
	}
	limit := settings.GroupMessageMaxCount
	if limit <= 0 || limit > 300 {
		limit = 300
	}
	items, err := repository.ListContext(ctx, messageSource(message), limit)
	if err != nil {
		slog.Warn("读取群聊上下文失败", "source", messageSource(message), "error", err)
		return ""
	}
	selected := make([]string, 0, len(items))
	used := 0
	for index := len(items) - 1; index >= 0; index-- {
		if items[index].ID == runtimeInboxID(message) {
			continue
		}
		count := utf8.RuneCountInString(items[index].Text)
		if used+count > maxGroupContextRunes {
			break
		}
		selected = append(selected, items[index].Text)
		used += count
	}
	for left, right := 0, len(selected)-1; left < right; left, right = left+1, right-1 {
		selected[left], selected[right] = selected[right], selected[left]
	}
	return strings.Join(selected, "\n")
}

// boundedGroupText 按 Unicode 字符截断平台文本，避免切开中文或多字节表情。
func boundedGroupText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	return string([]rune(value)[:limit])
}
