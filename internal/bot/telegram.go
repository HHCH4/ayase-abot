package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
)

const (
	telegramAPIBase       = "https://api.telegram.org"
	telegramPollTimeout   = 25
	telegramMessageLimit  = 4096
	platformAttachmentMax = 10 << 20
	platformAttachmentSum = 20 << 20
)

// telegramPlatform 使用官方 getUpdates 长轮询，不要求 Abot 暴露公网端口。
type telegramPlatform struct {
	bot      Bot
	client   *http.Client
	base     string
	username string
	selfID   int64
}

type telegramEnvelope[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
}

type telegramUpdate struct {
	UpdateID            int64                      `json:"update_id"`
	Message             *telegramMessage           `json:"message"`
	EditedMessage       *telegramMessage           `json:"edited_message"`
	CallbackQuery       *telegramCallbackQuery     `json:"callback_query"`
	MessageReaction     *telegramMessageReaction   `json:"message_reaction"`
	ChatMemberUpdated   *telegramChatMemberUpdated `json:"chat_member"`
	MyChatMemberUpdated *telegramChatMemberUpdated `json:"my_chat_member"`
}

type telegramMessage struct {
	MessageID       int64             `json:"message_id"`
	From            *telegramUser     `json:"from"`
	Chat            telegramChat      `json:"chat"`
	Text            string            `json:"text"`
	Caption         string            `json:"caption"`
	Entities        []telegramEntity  `json:"entities"`
	CaptionEntities []telegramEntity  `json:"caption_entities"`
	Photo           []telegramPhoto   `json:"photo"`
	Document        *telegramDocument `json:"document"`
	Voice           *telegramVoice    `json:"voice"`
	Audio           *telegramAudio    `json:"audio"`
	ReplyToMessage  *telegramMessage  `json:"reply_to_message"`
}

type telegramUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

type telegramEntity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

type telegramCallbackQuery struct {
	ID      string           `json:"id"`
	From    *telegramUser    `json:"from"`
	Data    string           `json:"data"`
	Message *telegramMessage `json:"message"`
}

// telegramMessageReaction 和 telegramChatMemberUpdated 只保留统一事件需要的
// 元数据，不把平台完整 payload 带进业务层。
type telegramMessageReaction struct {
	Chat        telegramChat  `json:"chat"`
	MessageID   int64         `json:"message_id"`
	User        *telegramUser `json:"user"`
	OldReaction []any         `json:"old_reaction"`
	NewReaction []any         `json:"new_reaction"`
}

type telegramChatMemberUpdated struct {
	Chat          telegramChat       `json:"chat"`
	From          *telegramUser      `json:"from"`
	OldChatMember telegramChatMember `json:"old_chat_member"`
	NewChatMember telegramChatMember `json:"new_chat_member"`
}

type telegramChatMember struct {
	User   *telegramUser `json:"user"`
	Status string        `json:"status"`
}

type telegramChat struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	Username string `json:"username"`
}

type telegramPhoto struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size"`
}

type telegramDocument struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MIMEType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

type telegramVoice struct {
	FileID   string `json:"file_id"`
	MIMEType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

type telegramAudio struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MIMEType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

type telegramFile struct {
	FilePath string `json:"file_path"`
}

// newTelegramPlatform 创建 Telegram 连接实例；base 只在单元测试中覆盖官方地址。
func newTelegramPlatform(item Bot) (*telegramPlatform, error) {
	if err := item.Validate(); err != nil {
		return nil, err
	}
	return &telegramPlatform{bot: item, client: &http.Client{Timeout: 35 * time.Second}, base: telegramAPIBase}, nil
}

// runEvents 是 Telegram 长轮询的统一事件循环。
func (p *telegramPlatform) runEvents(ctx context.Context, handler EventHandler) error {
	if p.username == "" {
		// Gateways and test doubles may omit getMe; without a known username only
		// Telegram's explicit bare bot-command entity is accepted as a trigger.
		var result telegramEnvelope[telegramUser]
		if err := p.call(ctx, "getMe", nil, &result); err == nil && result.OK {
			p.username = strings.TrimSpace(result.Result.Username)
			p.selfID = result.Result.ID
		}
	}
	connected := false
	defer func() {
		if !connected {
			return
		}
		if err := handler(context.WithoutCancel(ctx), p.lifecycleEvent(PlatformEventPlatformDisconnected)); err != nil {
			slog.Warn("Telegram 断开事件处理失败", "adapter_id", p.bot.ID, "event_type", PlatformEventPlatformDisconnected, "error", err)
		}
	}()
	var offset int64
	for {
		updates, err := p.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if !connected {
			connected = true
			if handlerErr := handler(ctx, p.lifecycleEvent(PlatformEventPlatformConnected)); handlerErr != nil {
				slog.Warn("Telegram 连接事件处理失败", "adapter_id", p.bot.ID, "event_type", PlatformEventPlatformConnected, "error", handlerErr)
			}
		}
		for _, update := range updates {
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
			if update.Message == nil && update.EditedMessage == nil && update.CallbackQuery == nil && update.MessageReaction == nil && update.ChatMemberUpdated == nil && update.MyChatMemberUpdated == nil {
				continue
			}
			event, err := p.eventFromUpdate(ctx, update)
			if err != nil {
				// 附件下载失败不应让 Telegram 长轮询整体退出；文字仍然可以交给 Agent。
				if update.CallbackQuery != nil {
					_ = p.answerCallbackQuery(ctx, update.CallbackQuery.ID)
				}
				continue
			}
			handlerErr := handler(ctx, event)
			if update.CallbackQuery != nil {
				// Always dismiss Telegram's callback spinner, even when the command
				// itself is rejected by the Manager.
				_ = p.answerCallbackQuery(ctx, update.CallbackQuery.ID)
			}
			if handlerErr != nil {
				// 单条消息失败由 Manager 记录，平台连接继续服务其他聊天。
				continue
			}
		}
	}
}

// lifecycleEvent 只反映 getUpdates 已经成功返回后的真实在线状态，不把 getMe
// 或轮询循环启动误当成平台连接成功。
func (p *telegramPlatform) lifecycleEvent(eventType PlatformEventType) PlatformEvent {
	now := time.Now().UTC()
	return PlatformEvent{ID: newRuntimeID(string(eventType), p.bot.ID, fmt.Sprint(now.UnixNano())), BotID: p.bot.ID, Platform: TypeTelegram, EventType: eventType, NativeCapabilities: p.Capabilities(), OccurredAt: now, ReceivedAt: now}
}

// RunEvents 将 Telegram 更新转换为统一 PlatformEvent；连接生命周期由 Manager
// 负责记录，消息和审批回调则共用同一个事件审计入口。
func (p *telegramPlatform) RunEvents(ctx context.Context, handler EventHandler) error {
	return p.runEvents(ctx, handler)
}

// eventFromUpdate 将 Telegram 的消息、审批回调、反应和成员变更映射为
// PlatformEvent；编辑消息按新的消息事件重新进入去重/聚合链。
func (p *telegramPlatform) eventFromUpdate(ctx context.Context, update telegramUpdate) (PlatformEvent, error) {
	if update.CallbackQuery != nil {
		message, err := p.messageFromUpdate(ctx, update)
		if err != nil {
			return PlatformEvent{}, err
		}
		return platformEventFromMessage(message, PlatformEventApprovalCallback, p.Capabilities()), nil
	}
	if update.Message != nil || update.EditedMessage != nil {
		if update.Message == nil {
			update.Message = update.EditedMessage
		}
		message, err := p.messageFromUpdate(ctx, update)
		if err != nil {
			return PlatformEvent{}, err
		}
		return platformEventFromMessage(message, PlatformEventMessageCreated, p.Capabilities()), nil
	}
	if reaction := update.MessageReaction; reaction != nil {
		chatType := reaction.Chat.Type
		userID := ""
		if reaction.User != nil {
			userID = strconv.FormatInt(reaction.User.ID, 10)
		}
		message := Message{AdapterID: p.bot.ID, Platform: TypeTelegram, UserID: userID, ChatID: strconv.FormatInt(reaction.Chat.ID, 10), ChatType: chatType}
		eventType := PlatformEventReactionRemoved
		if len(reaction.NewReaction) > 0 {
			eventType = PlatformEventReactionAdded
		}
		now := time.Now().UTC()
		return PlatformEvent{ID: fmt.Sprintf("telegram-update:%d", update.UpdateID), BotID: p.bot.ID, Platform: TypeTelegram, SourceUMO: messageSource(message), ChatType: chatType, ChatID: message.ChatID, UserID: userID, EventType: eventType, MessageID: strconv.FormatInt(reaction.MessageID, 10), NativeCapabilities: p.Capabilities(), OccurredAt: now, ReceivedAt: now}, nil
	}
	member := update.ChatMemberUpdated
	if member == nil {
		member = update.MyChatMemberUpdated
	}
	if member != nil {
		userID := ""
		if member.NewChatMember.User != nil {
			userID = strconv.FormatInt(member.NewChatMember.User.ID, 10)
		} else if member.From != nil {
			userID = strconv.FormatInt(member.From.ID, 10)
		}
		message := Message{AdapterID: p.bot.ID, Platform: TypeTelegram, UserID: userID, ChatID: strconv.FormatInt(member.Chat.ID, 10), ChatType: member.Chat.Type}
		eventType := PlatformEventMemberLeft
		if member.NewChatMember.Status == "member" || member.NewChatMember.Status == "administrator" || member.NewChatMember.Status == "creator" {
			eventType = PlatformEventMemberJoined
		}
		now := time.Now().UTC()
		return PlatformEvent{ID: fmt.Sprintf("telegram-update:%d", update.UpdateID), BotID: p.bot.ID, Platform: TypeTelegram, SourceUMO: messageSource(message), ChatType: message.ChatType, ChatID: message.ChatID, UserID: userID, EventType: eventType, NativeCapabilities: p.Capabilities(), OccurredAt: now, ReceivedAt: now}, nil
	}
	return PlatformEvent{}, errors.New("Telegram 更新类型不支持")
}

// Capabilities 声明 Telegram 适配器可以执行的抽象动作；富文本由表达层先
// 转为安全 HTML，适配器不接收模型原始 Markdown。
func (p *telegramPlatform) Capabilities() PlatformCapabilities {
	return PlatformCapabilities{PlainText: true, RichText: true, Reply: true, Mention: true, Reaction: true, Image: true, Audio: true, File: true, Recall: true, MaxTextLength: telegramMessageLimit}
}

// Test 调用 getMe，能同时验证 Token 和 Telegram API 可达性。
func (p *telegramPlatform) Test(ctx context.Context) error {
	var result telegramEnvelope[telegramUser]
	if err := p.call(ctx, "getMe", nil, &result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("Telegram 认证失败: %s", result.Description)
	}
	return nil
}

// DispatchAction 将 Bot Runtime 的抽象动作投影为 Telegram Bot API；只有完成
// API 调用并拿到消息结果后才向上层返回成功。
func (p *telegramPlatform) DispatchAction(ctx context.Context, action PlatformAction) (PlatformActionResult, error) {
	switch strings.TrimSpace(action.Type) {
	case "send_text":
		if action.Approval != nil {
			return p.dispatchApproval(ctx, action.Message, *action.Approval)
		}
		if action.UserInput != nil {
			return p.dispatchSendText(ctx, action.Message, userInputOptionsText(*action.UserInput))
		}
		return p.dispatchSendText(ctx, action.Message, action.Text)
	case "send_image", "send_audio", "send_file":
		return p.dispatchSendAttachment(ctx, action)
	case "recall_message":
		messageID, err := strconv.ParseInt(strings.TrimSpace(action.TargetID), 10, 64)
		if err != nil || messageID <= 0 {
			return PlatformActionResult{}, errors.New("Telegram 撤回消息 ID 无效")
		}
		var result telegramEnvelope[bool]
		if err := p.call(ctx, "deleteMessage", map[string]any{"chat_id": action.Message.ChatID, "message_id": messageID}, &result); err != nil {
			return PlatformActionResult{}, err
		}
		if !result.OK {
			return PlatformActionResult{}, fmt.Errorf("Telegram 撤回消息失败: %s", result.Description)
		}
		return PlatformActionResult{MessageID: action.TargetID}, nil
	case "add_reaction":
		messageID, err := strconv.ParseInt(strings.TrimSpace(action.TargetID), 10, 64)
		if err != nil || messageID <= 0 || strings.TrimSpace(action.Emoji) == "" {
			return PlatformActionResult{}, errors.New("Telegram 表情动作参数无效")
		}
		var result telegramEnvelope[bool]
		if err := p.call(ctx, "setMessageReaction", map[string]any{"chat_id": action.Message.ChatID, "message_id": messageID, "reaction": []map[string]string{{"type": "emoji", "emoji": action.Emoji}}}, &result); err != nil {
			return PlatformActionResult{}, err
		}
		if !result.OK {
			return PlatformActionResult{}, fmt.Errorf("Telegram 添加表情失败: %s", result.Description)
		}
		return PlatformActionResult{MessageID: action.TargetID}, nil
	default:
		return PlatformActionResult{}, fmt.Errorf("Telegram 不支持抽象动作 %q", action.Type)
	}
}

// dispatchSendAttachment 使用 Telegram multipart API 发送媒体；附件必须由
// 上游在动作执行前取得，适配器不会把一个不可读的 Artifact ref 伪装成成功。
func (p *telegramPlatform) dispatchSendAttachment(ctx context.Context, action PlatformAction) (PlatformActionResult, error) {
	if action.Attachment == nil || len(action.Attachment.Data) == 0 {
		return PlatformActionResult{}, errors.New("Telegram 媒体动作缺少附件数据")
	}
	fieldName := "photo"
	method := "sendPhoto"
	switch strings.TrimSpace(action.Type) {
	case "send_audio":
		fieldName = "audio"
		method = "sendAudio"
	case "send_file":
		fieldName = "document"
		method = "sendDocument"
	}
	fields := map[string]string{"chat_id": strings.TrimSpace(action.Message.ChatID)}
	if fields["chat_id"] == "" {
		return PlatformActionResult{}, errors.New("Telegram 媒体目标聊天 ID 为空")
	}
	if strings.TrimSpace(action.Text) != "" {
		fields["caption"] = action.Text
	}
	if action.Message.ReplyQuote {
		if replyID, err := strconv.ParseInt(action.Message.ReplyMessageID, 10, 64); err == nil && replyID > 0 {
			replyBytes, _ := json.Marshal(map[string]any{"message_id": replyID, "allow_sending_without_reply": true})
			fields["reply_parameters"] = string(replyBytes)
		}
	}
	return p.callMultipart(ctx, method, fields, fieldName, cleanFileName(action.Attachment.Name), normalizeFileMIME(action.Attachment.Name, action.Attachment.MIMEType), action.Attachment.Data)
}

func (p *telegramPlatform) dispatchSendText(ctx context.Context, message Message, text string) (PlatformActionResult, error) {
	chatID := strings.TrimSpace(message.ChatID)
	if chatID == "" {
		return PlatformActionResult{}, errors.New("Telegram 目标聊天 ID 为空")
	}
	limit := telegramMessageLimit
	if message.ReplyMention && isGroupChat(message.ChatType) {
		limit -= 16
	}
	chunks := splitText(text, limit)
	for index, chunk := range chunks {
		payload := map[string]any{"chat_id": chatID, "text": chunk}
		if message.TextFormat == "html" {
			payload["parse_mode"] = "HTML"
		}
		// Telegram 使用官方 reply_parameters；@用户以 HTML 链接标记，普通正文转义后避免注入标记。
		if index == 0 && message.ReplyQuote {
			if replyID, parseErr := strconv.ParseInt(message.ReplyMessageID, 10, 64); parseErr == nil && replyID > 0 {
				payload["reply_parameters"] = map[string]any{"message_id": replyID, "allow_sending_without_reply": true}
			}
		}
		if index == 0 && message.ReplyMention && isGroupChat(message.ChatType) {
			if userID, parseErr := strconv.ParseInt(message.UserID, 10, 64); parseErr == nil && userID > 0 {
				body := html.EscapeString(chunk)
				if message.TextFormat == "html" {
					body = chunk
				}
				payload["text"] = fmt.Sprintf(`<a href="tg://user?id=%d">@用户</a> %s`, userID, body)
				payload["parse_mode"] = "HTML"
			}
		}
		var result telegramEnvelope[telegramMessage]
		if err := p.call(ctx, "sendMessage", payload, &result); err != nil {
			return PlatformActionResult{}, err
		}
		if !result.OK {
			return PlatformActionResult{}, fmt.Errorf("Telegram 发送消息失败: %s", result.Description)
		}
		lastID := ""
		if result.Result.MessageID > 0 {
			lastID = strconv.FormatInt(result.Result.MessageID, 10)
		}
		if index == len(chunks)-1 {
			return PlatformActionResult{MessageID: lastID}, nil
		}
	}
	return PlatformActionResult{}, nil
}

// dispatchApproval 将审批提示作为结构化平台动作渲染；动作计划已经在
// Bot Runtime 中落库，这里只负责调用 Telegram 的原生按钮接口。
func (p *telegramPlatform) dispatchApproval(ctx context.Context, message Message, prompt ApprovalPrompt) (PlatformActionResult, error) {
	chatID := strings.TrimSpace(message.ChatID)
	if chatID == "" {
		return PlatformActionResult{}, errors.New("Telegram 目标聊天 ID 为空")
	}
	toolName := strings.TrimSpace(prompt.ToolName)
	if toolName == "" {
		toolName = "当前操作"
	}
	hint := strings.TrimSpace(prompt.Hint)
	if hint == "" {
		hint = "需要确认后才能继续。"
	}
	choices := prompt.Choices
	if len(choices) == 0 {
		choices = agentruntime.DefaultApprovalChoices()
	}
	callbackPrefix := "abot:approval:" + strings.TrimSpace(prompt.ApprovalID) + ":"
	text := fmt.Sprintf("需要确认：%s\n%s", toolName, hint)
	if prompt.ExpiresAt != nil {
		text += "\n有效期至：" + prompt.ExpiresAt.UTC().Format(time.RFC3339)
	}
	text += "\n请选择一个选项："
	keyboard := make([]map[string]string, 0, len(choices))
	for _, choice := range choices {
		keyboard = append(keyboard, map[string]string{"text": choice.Label, "callback_data": callbackPrefix + choice.ID})
	}
	payload := map[string]any{
		"chat_id":      chatID,
		"text":         text,
		"reply_markup": map[string]any{"inline_keyboard": [][]map[string]string{keyboard}},
	}
	var result telegramEnvelope[telegramMessage]
	if err := p.call(ctx, "sendMessage", payload, &result); err != nil {
		return PlatformActionResult{}, err
	}
	if !result.OK {
		return PlatformActionResult{}, fmt.Errorf("Telegram 发送审批消息失败: %s", result.Description)
	}
	return PlatformActionResult{MessageID: strconv.FormatInt(result.Result.MessageID, 10)}, nil
}

// Close 通过取消 Run 的 context 结束请求；这里无需持有额外长连接。
func (p *telegramPlatform) Close() error { return nil }

func (p *telegramPlatform) getUpdates(ctx context.Context, offset int64) ([]telegramUpdate, error) {
	payload := map[string]any{"timeout": telegramPollTimeout, "allowed_updates": []string{"message", "edited_message", "callback_query", "message_reaction", "chat_member", "my_chat_member"}}
	if offset > 0 {
		payload["offset"] = offset
	}
	var result telegramEnvelope[[]telegramUpdate]
	if err := p.call(ctx, "getUpdates", payload, &result); err != nil {
		return nil, err
	}
	if !result.OK {
		return nil, fmt.Errorf("Telegram 拉取消息失败: %s", result.Description)
	}
	return result.Result, nil
}

func (p *telegramPlatform) messageFromUpdate(ctx context.Context, update telegramUpdate) (Message, error) {
	if update.CallbackQuery != nil {
		return p.messageFromCallback(update.CallbackQuery)
	}
	item := update.Message
	if item == nil {
		return Message{}, errors.New("Telegram 更新不包含消息")
	}
	userID := strconv.FormatInt(item.Chat.ID, 10)
	if item.From != nil && item.From.ID != 0 {
		userID = strconv.FormatInt(item.From.ID, 10)
	}
	message := Message{
		ID:             strconv.FormatInt(update.UpdateID, 10),
		AdapterID:      p.bot.ID,
		Platform:       TypeTelegram,
		UserID:         userID,
		ChatID:         strconv.FormatInt(item.Chat.ID, 10),
		ChatType:       item.Chat.Type,
		AutoName:       telegramMessageAutoName(item, userID),
		Text:           strings.TrimSpace(item.Text),
		Mentioned:      p.telegramMessageMentioned(item),
		ReplyMessageID: strconv.FormatInt(item.MessageID, 10),
	}
	if item.ReplyToMessage != nil && item.ReplyToMessage.MessageID > 0 {
		message.ReplyToMessageID = strconv.FormatInt(item.ReplyToMessage.MessageID, 10)
	}
	if item.From != nil {
		message.IsSelf = p.selfID != 0 && item.From.ID == p.selfID
	}
	if message.Text == "" {
		message.Text = strings.TrimSpace(item.Caption)
	}
	// 单独 @机器人 是等待下一句的触发动作，不把用户名当作模型问题。
	if isGroupChat(message.ChatType) && message.Mentioned && p.username != "" && strings.EqualFold(message.Text, "@"+p.username) {
		message.Text = ""
	}
	if len(item.Photo) > 0 {
		photo := item.Photo[len(item.Photo)-1]
		if attachment, err := p.downloadFile(ctx, photo.FileID, fmt.Sprintf("telegram-photo-%d.jpg", item.MessageID), "image/jpeg"); err == nil {
			message.Attachments = appendTelegramAttachment(message.Attachments, attachment)
		}
	}
	if item.Document != nil {
		name := cleanFileName(item.Document.FileName)
		if name == "" {
			name = fmt.Sprintf("telegram-file-%d", item.MessageID)
		}
		mimeType := normalizeFileMIME(name, item.Document.MIMEType)
		if attachment, err := p.downloadFile(ctx, item.Document.FileID, name, mimeType); err == nil {
			message.Attachments = appendTelegramAttachment(message.Attachments, attachment)
		}
	}
	if item.Voice != nil {
		mimeType := normalizeFileMIME("telegram-voice.ogg", item.Voice.MIMEType)
		if mimeType == "application/octet-stream" {
			mimeType = "audio/ogg"
		}
		if attachment, err := p.downloadFile(ctx, item.Voice.FileID, fmt.Sprintf("telegram-voice-%d.ogg", item.MessageID), mimeType); err == nil {
			message.Attachments = appendTelegramAttachment(message.Attachments, attachment)
		}
	}
	if item.Audio != nil {
		name := cleanFileName(item.Audio.FileName)
		if name == "" {
			name = fmt.Sprintf("telegram-audio-%d", item.MessageID)
		}
		mimeType := normalizeFileMIME(name, item.Audio.MIMEType)
		if attachment, err := p.downloadFile(ctx, item.Audio.FileID, name, mimeType); err == nil {
			message.Attachments = appendTelegramAttachment(message.Attachments, attachment)
		}
	}
	if message.Text == "" && len(message.Attachments) == 0 && !message.Mentioned {
		return Message{}, errors.New("Telegram 更新不包含文本或支持的附件")
	}
	return message, nil
}

func appendTelegramAttachment(items []agent.Attachment, attachment agent.Attachment) []agent.Attachment {
	if len(attachment.Data) == 0 || attachmentBytes(items)+int64(len(attachment.Data)) > platformAttachmentSum {
		return items
	}
	return append(items, attachment)
}

func (p *telegramPlatform) messageFromCallback(callback *telegramCallbackQuery) (Message, error) {
	if callback == nil || callback.Message == nil || strings.TrimSpace(callback.ID) == "" {
		return Message{}, errors.New("Telegram 审批回调不完整")
	}
	parts := strings.Split(callback.Data, ":")
	if len(parts) != 4 || parts[0] != "abot" || parts[1] != "approval" || strings.TrimSpace(parts[2]) == "" {
		return Message{}, errors.New("Telegram 审批回调格式无效")
	}
	choiceID := strings.TrimSpace(parts[3])
	if choiceID == "" {
		return Message{}, errors.New("Telegram 审批决定无效")
	}
	decision := choiceID != "reject"
	userID := strconv.FormatInt(callback.Message.Chat.ID, 10)
	if callback.From != nil && callback.From.ID != 0 {
		userID = strconv.FormatInt(callback.From.ID, 10)
	}
	return Message{
		ID: "callback:" + callback.ID, AdapterID: p.bot.ID, Platform: TypeTelegram, UserID: userID,
		ChatID: strconv.FormatInt(callback.Message.Chat.ID, 10), ChatType: callback.Message.Chat.Type,
		AutoName:  telegramCallbackAutoName(callback, userID),
		Mentioned: true, Control: &MessageControl{Kind: "approval", ApprovalID: parts[2], ChoiceID: choiceID, Decision: decision, CallbackID: callback.ID},
	}, nil
}

// telegramMessageAutoName 提取 Telegram 的群标题、频道用户名或用户昵称；
// 这些信息只作为来源目录的自动名称，来源键仍严格使用 UMO 三段式格式。
func telegramMessageAutoName(item *telegramMessage, userID string) string {
	if item == nil {
		return ""
	}
	chatType := strings.ToLower(strings.TrimSpace(item.Chat.Type))
	if chatType == "group" || chatType == "supergroup" || chatType == "channel" {
		if title := strings.TrimSpace(item.Chat.Title); title != "" {
			return title
		}
		if username := strings.TrimSpace(item.Chat.Username); username != "" {
			return "@" + username
		}
		return "群聊 " + strings.TrimSpace(strconv.FormatInt(item.Chat.ID, 10))
	}
	if item.From != nil {
		name := strings.TrimSpace(strings.Join([]string{item.From.FirstName, item.From.LastName}, " "))
		if name != "" {
			return name
		}
		if username := strings.TrimSpace(item.From.Username); username != "" {
			return "@" + username
		}
	}
	if userID != "" {
		return "用户 " + userID
	}
	return ""
}

// telegramCallbackAutoName 使用回调发送者优先补齐审批按钮产生的消息来源名。
func telegramCallbackAutoName(callback *telegramCallbackQuery, userID string) string {
	if callback == nil || callback.Message == nil {
		return ""
	}
	item := callback.Message
	if item.Chat.Title != "" || item.Chat.Username != "" {
		return telegramMessageAutoName(item, userID)
	}
	if callback.From != nil {
		name := strings.TrimSpace(strings.Join([]string{callback.From.FirstName, callback.From.LastName}, " "))
		if name != "" {
			return name
		}
		if username := strings.TrimSpace(callback.From.Username); username != "" {
			return "@" + username
		}
	}
	return telegramMessageAutoName(item, userID)
}

func (p *telegramPlatform) telegramMessageMentioned(item *telegramMessage) bool {
	if item == nil {
		return false
	}
	for _, entity := range item.Entities {
		if telegramEntityTargetsBot(item.Text, entity, p.username) {
			return true
		}
	}
	for _, entity := range item.CaptionEntities {
		if telegramEntityTargetsBot(item.Caption, entity, p.username) {
			return true
		}
	}
	return false
}

func telegramEntityTargetsBot(text string, entity telegramEntity, username string) bool {
	if entity.Type != "mention" && entity.Type != "bot_command" {
		return false
	}
	value := strings.TrimSpace(telegramEntityText(text, entity))
	if value == "" {
		return false
	}
	if strings.TrimSpace(username) == "" {
		// Without getMe we can still safely recognize a bare bot command, but
		// an unqualified @mention may target another person and an addressed
		// command cannot be matched to this bot. Keep group admission closed
		// until the target is known.
		return entity.Type == "bot_command" && !strings.Contains(value, "@")
	}
	if entity.Type == "bot_command" {
		at := strings.IndexByte(value, '@')
		if at < 0 {
			// A bare command is a direct bot command in Telegram's message
			// entity model; accepting it also makes /status usable in groups.
			return true
		}
		return strings.EqualFold(strings.TrimPrefix(value[at:], "@"), strings.TrimSpace(username))
	}
	return strings.EqualFold(strings.TrimPrefix(value, "@"), strings.TrimSpace(username))
}

// Telegram entity offsets are UTF-16 code-unit offsets, not Go byte offsets.
func telegramEntityText(text string, entity telegramEntity) string {
	if entity.Offset < 0 || entity.Length <= 0 {
		return ""
	}
	units := utf16.Encode([]rune(text))
	start := entity.Offset
	end := start + entity.Length
	if start >= len(units) || end > len(units) || end <= start {
		return ""
	}
	return string(utf16.Decode(units[start:end]))
}

func (p *telegramPlatform) answerCallbackQuery(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return nil
	}
	var result telegramEnvelope[bool]
	return p.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id}, &result)
}

func (p *telegramPlatform) downloadFile(ctx context.Context, fileID, name, mimeType string) (agent.Attachment, error) {
	var result telegramEnvelope[telegramFile]
	if err := p.call(ctx, "getFile", map[string]any{"file_id": fileID}, &result); err != nil {
		return agent.Attachment{}, err
	}
	if !result.OK || result.Result.FilePath == "" {
		return agent.Attachment{}, fmt.Errorf("Telegram 获取附件路径失败: %s", result.Description)
	}
	fileURL := strings.TrimRight(p.base, "/") + "/file/bot" + url.PathEscape(p.bot.TelegramToken) + "/" + result.Result.FilePath
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return agent.Attachment{}, err
	}
	response, err := p.client.Do(request)
	if err != nil {
		return agent.Attachment{}, fmt.Errorf("Telegram 下载附件失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return agent.Attachment{}, fmt.Errorf("Telegram 下载附件失败: HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, platformAttachmentMax+1))
	if err != nil {
		return agent.Attachment{}, fmt.Errorf("读取 Telegram 附件失败: %w", err)
	}
	if int64(len(data)) > platformAttachmentMax {
		return agent.Attachment{}, fmt.Errorf("Telegram 附件超过 %d MB", platformAttachmentMax>>20)
	}
	return agent.Attachment{Name: name, MIMEType: mimeType, Data: data}, nil
}

func (p *telegramPlatform) call(ctx context.Context, method string, payload map[string]any, target any) error {
	endpoint := strings.TrimRight(p.base, "/") + "/bot" + url.PathEscape(p.bot.TelegramToken) + "/" + method
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("Telegram 请求编码失败: %w", err)
	}
	// Telegram 请求和响应都保留原文，便于排查平台协议、参数和返回值问题。
	slog.Info("Telegram 请求", "adapter_id", p.bot.ID, "method", method, "endpoint", endpoint, "payload", string(body))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("Telegram 请求创建失败: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		slog.Error("Telegram 请求失败", "adapter_id", p.bot.ID, "method", method, "endpoint", endpoint, "payload", string(body), "error", err)
		return fmt.Errorf("Telegram API 请求失败: %w", err)
	}
	defer response.Body.Close()
	const responseLimit = 8 << 20
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	truncated := len(responseBody) > responseLimit
	if truncated {
		responseBody = responseBody[:responseLimit]
	}
	// 先记录完整可读响应，再按原有协议规则处理状态码和 JSON。
	slog.Info("Telegram 响应", "adapter_id", p.bot.ID, "method", method, "endpoint", endpoint, "status", response.StatusCode, "body", string(responseBody), "body_truncated", truncated)
	if readErr != nil {
		return fmt.Errorf("读取 Telegram API 响应失败: %w", readErr)
	}
	if truncated {
		return fmt.Errorf("Telegram API 响应超过 %d MB", responseLimit>>20)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("Telegram API 返回 HTTP %d", response.StatusCode)
	}
	if err := json.Unmarshal(responseBody, target); err != nil {
		return fmt.Errorf("Telegram API 响应无效: %w", err)
	}
	return nil
}

func (p *telegramPlatform) callMultipart(ctx context.Context, method string, fields map[string]string, fileField, fileName, mimeType string, data []byte) (PlatformActionResult, error) {
	endpoint := strings.TrimRight(p.base, "/") + "/bot" + url.PathEscape(p.bot.TelegramToken) + "/" + method
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			return PlatformActionResult{}, fmt.Errorf("Telegram multipart 字段写入失败: %w", err)
		}
	}
	if fileName == "" {
		fileName = "attachment"
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, fileField, fileName))
	header.Set("Content-Type", mimeType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return PlatformActionResult{}, fmt.Errorf("Telegram multipart 文件字段创建失败: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return PlatformActionResult{}, fmt.Errorf("Telegram multipart 文件写入失败: %w", err)
	}
	if err := writer.Close(); err != nil {
		return PlatformActionResult{}, fmt.Errorf("Telegram multipart 请求收尾失败: %w", err)
	}
	slog.Info("Telegram 请求", "adapter_id", p.bot.ID, "method", method, "endpoint", endpoint, "fields", fields, "file_name", fileName, "mime_type", mimeType, "file_bytes", len(data))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return PlatformActionResult{}, fmt.Errorf("Telegram multipart 请求创建失败: %w", err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := p.client.Do(request)
	if err != nil {
		slog.Error("Telegram 请求失败", "adapter_id", p.bot.ID, "method", method, "endpoint", endpoint, "fields", fields, "file_name", fileName, "error", err)
		return PlatformActionResult{}, fmt.Errorf("Telegram API 请求失败: %w", err)
	}
	defer response.Body.Close()
	const responseLimit = 8 << 20
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	truncated := len(responseBody) > responseLimit
	if truncated {
		responseBody = responseBody[:responseLimit]
	}
	slog.Info("Telegram 响应", "adapter_id", p.bot.ID, "method", method, "endpoint", endpoint, "status", response.StatusCode, "body", string(responseBody), "body_truncated", truncated)
	if readErr != nil {
		return PlatformActionResult{}, fmt.Errorf("读取 Telegram API 响应失败: %w", readErr)
	}
	if truncated {
		return PlatformActionResult{}, fmt.Errorf("Telegram API 响应超过 %d MB", responseLimit>>20)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return PlatformActionResult{}, fmt.Errorf("Telegram API 返回 HTTP %d", response.StatusCode)
	}
	var result telegramEnvelope[telegramMessage]
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return PlatformActionResult{}, fmt.Errorf("Telegram API 响应无效: %w", err)
	}
	if !result.OK {
		return PlatformActionResult{}, fmt.Errorf("Telegram 媒体发送失败: %s", result.Description)
	}
	messageID := ""
	if result.Result.MessageID > 0 {
		messageID = strconv.FormatInt(result.Result.MessageID, 10)
	}
	return PlatformActionResult{MessageID: messageID, Raw: string(responseBody)}, nil
}

func splitText(text string, limit int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if limit <= 0 {
		limit = telegramMessageLimit
	}
	runes := []rune(text)
	result := make([]string, 0, (len(runes)+limit-1)/limit)
	for len(runes) > 0 {
		size := limit
		if len(runes) < size {
			size = len(runes)
		} else {
			// 适配器只做最终长度兜底，优先在段落、句末或空白边界切开，
			// 避免把表达层已经组织好的完整语义从句中间截断。
			for index := size; index > 0 && index >= size-240; index-- {
				if strings.ContainsRune("\n。！？!?；;，,、 \t", runes[index-1]) {
					size = index
					break
				}
			}
		}
		part := strings.TrimSpace(string(runes[:size]))
		if part != "" {
			result = append(result, part)
		}
		runes = runes[size:]
	}
	return result
}

func cleanFileName(name string) string {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	if index := strings.LastIndex(name, "/"); index >= 0 {
		name = name[index+1:]
	}
	return strings.TrimSpace(name)
}

func normalizeFileMIME(name, value string) string {
	if mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(value)); err == nil && mediaType != "" {
		return strings.ToLower(mediaType)
	}
	if guessed := mime.TypeByExtension(filepath.Ext(name)); guessed != "" {
		if mediaType, _, err := mime.ParseMediaType(guessed); err == nil && mediaType != "" {
			return strings.ToLower(mediaType)
		}
	}
	return "application/octet-stream"
}

func attachmentBytes(items []agent.Attachment) int64 {
	var total int64
	for _, item := range items {
		total += int64(len(item.Data))
	}
	return total
}
