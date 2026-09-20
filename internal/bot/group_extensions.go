package bot

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"
	"unicode/utf8"

	"Abot/internal/agent"
)

const (
	maxGroupHistorySources    = 64
	maxGroupHistoryEntryRunes = 500
	maxGroupContextRunes      = 12000
)

// SetGroupImageCaptioner 安装单张群图片转述能力；失败时历史仍保留图片占位符。
func (m *Manager) SetGroupImageCaptioner(caption func(context.Context, string, agent.Attachment) (string, error)) {
	m.mu.Lock()
	m.groupImageCaption = caption
	m.mu.Unlock()
}

// recordGroupContext 记录所有群消息，包括没有唤醒机器人的消息；内存只保留有限来源和文本。
func (m *Manager) recordGroupContext(ctx context.Context, message Message, settings ExtensionConfig) {
	if !isGroupChat(message.ChatType) || (!settings.GroupContextEnabled && !settings.ProactiveReplyEnabled) || message.Control != nil {
		return
	}
	if _, command := parseBotCommand(message.Text); command {
		return
	}
	text := strings.TrimSpace(message.Text)
	if len(message.Attachments) > 0 {
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
	}
	if text == "" {
		return
	}
	// 昵称和文本来自第三方平台，截断后作为低信任背景提供，不赋予工具授权。
	name := boundedGroupText(message.AutoName, 64)
	if name == "" {
		name = boundedGroupText(message.UserID, 64)
	}
	entry := fmt.Sprintf("[%s/%s] %s", name, time.Now().Format("15:04:05"), boundedGroupText(text, maxGroupHistoryEntryRunes))
	source := messageSource(message)
	limit := settings.GroupMessageMaxCount
	if limit <= 0 || limit > 300 {
		limit = 300
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.groupHistory == nil {
		m.groupHistory = make(map[string][]string)
	}
	if _, exists := m.groupHistory[source]; !exists {
		if len(m.groupHistoryOrder) >= maxGroupHistorySources {
			oldest := m.groupHistoryOrder[0]
			delete(m.groupHistory, oldest)
			m.groupHistoryOrder = m.groupHistoryOrder[1:]
		}
		m.groupHistoryOrder = append(m.groupHistoryOrder, source)
	}
	items := append(m.groupHistory[source], entry)
	if len(items) > limit {
		items = items[len(items)-limit:]
	}
	m.groupHistory[source] = items
}

// groupContextText 在同一 UMO 内读取最近历史；内容长度再限一次以保护模型上下文。
func (m *Manager) groupContextText(message Message, settings ExtensionConfig) string {
	if !isGroupChat(message.ChatType) || (!settings.GroupContextEnabled && !settings.ProactiveReplyEnabled) {
		return ""
	}
	m.mu.RLock()
	items := append([]string(nil), m.groupHistory[messageSource(message)]...)
	m.mu.RUnlock()
	limit := settings.GroupMessageMaxCount
	if limit <= 0 || limit > 300 {
		limit = 300
	}
	if len(items) > limit {
		items = items[len(items)-limit:]
	}
	var selected []string
	used := 0
	for index := len(items) - 1; index >= 0; index-- {
		count := utf8.RuneCountInString(items[index])
		if used+count > maxGroupContextRunes {
			break
		}
		selected = append(selected, items[index])
		used += count
	}
	for left, right := 0, len(selected)-1; left < right; left, right = left+1, right-1 {
		selected[left], selected[right] = selected[right], selected[left]
	}
	return strings.Join(selected, "\n")
}

// shouldProactiveReply 仅在未触发普通处理的群消息上使用概率和白名单。
func shouldProactiveReply(message Message, settings ExtensionConfig) bool {
	if !settings.ProactiveReplyEnabled || settings.ProactiveReplyMethod != "possibility_reply" || !isGroupChat(message.ChatType) || message.Mentioned || (strings.TrimSpace(message.Text) == "" && len(message.Attachments) == 0) {
		return false
	}
	if _, command := parseBotCommand(message.Text); command {
		return false
	}
	if len(settings.ProactiveReplyWhitelist) > 0 {
		allowed := false
		for _, item := range settings.ProactiveReplyWhitelist {
			if item == messageSource(message) || item == message.ChatID {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	return settings.ProactiveReplyProbability > 0 && rand.Float64() < settings.ProactiveReplyProbability
}

// boundedGroupText 按 Unicode 字符截断平台文本，避免切开中文或多字节表情。
func boundedGroupText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	return string([]rune(value)[:limit])
}
