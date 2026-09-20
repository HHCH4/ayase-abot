package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"Abot/internal/agent"
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
}

type telegramEnvelope[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
}

type telegramUpdate struct {
	UpdateID      int64                  `json:"update_id"`
	Message       *telegramMessage       `json:"message"`
	CallbackQuery *telegramCallbackQuery `json:"callback_query"`
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
}

type telegramUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
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

type telegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
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

// Run 持续拉取更新，并保证 offset 只向前推进，避免同一条消息在一次运行中重复处理。
func (p *telegramPlatform) Run(ctx context.Context, handler Handler) error {
	if p.username == "" {
		// Gateways and test doubles may omit getMe; without a known username only
		// Telegram's explicit bare bot-command entity is accepted as a trigger.
		var result telegramEnvelope[telegramUser]
		if err := p.call(ctx, "getMe", nil, &result); err == nil && result.OK {
			p.username = strings.TrimSpace(result.Result.Username)
		}
	}
	var offset int64
	for {
		updates, err := p.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, update := range updates {
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
			if update.Message == nil && update.CallbackQuery == nil {
				continue
			}
			message, err := p.messageFromUpdate(ctx, update)
			if err != nil {
				// 附件下载失败不应让 Telegram 长轮询整体退出；文字仍然可以交给 Agent。
				if update.CallbackQuery != nil {
					_ = p.answerCallbackQuery(ctx, update.CallbackQuery.ID)
				}
				continue
			}
			handlerErr := handler(ctx, message)
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

// Send 将 Agent 回复按 Telegram 单条消息限制拆分后发回原聊天。
func (p *telegramPlatform) Send(ctx context.Context, message Message, text string) error {
	chatID := strings.TrimSpace(message.ChatID)
	if chatID == "" {
		return errors.New("Telegram 目标聊天 ID 为空")
	}
	for _, chunk := range splitText(text, telegramMessageLimit) {
		payload := map[string]any{"chat_id": chatID, "text": chunk}
		var result telegramEnvelope[telegramMessage]
		if err := p.call(ctx, "sendMessage", payload, &result); err != nil {
			return err
		}
		if !result.OK {
			return fmt.Errorf("Telegram 发送消息失败: %s", result.Description)
		}
	}
	return nil
}

func (p *telegramPlatform) SendApproval(ctx context.Context, message Message, prompt ApprovalPrompt) error {
	chatID := strings.TrimSpace(message.ChatID)
	if chatID == "" {
		return errors.New("Telegram 目标聊天 ID 为空")
	}
	toolName := strings.TrimSpace(prompt.ToolName)
	if toolName == "" {
		toolName = "当前操作"
	}
	hint := strings.TrimSpace(prompt.Hint)
	if hint == "" {
		hint = "需要确认后才能继续。"
	}
	callbackPrefix := "abot:approval:" + strings.TrimSpace(prompt.ApprovalID) + ":"
	text := fmt.Sprintf("需要审批：%s\n%s", toolName, hint)
	if prompt.ExpiresAt != nil {
		text += "\n有效期至：" + prompt.ExpiresAt.UTC().Format(time.RFC3339)
	}
	// Telegram 提供按钮，同时允许用户直接发送确认词，保证与 OneBot 文本交互一致。
	text += "\n也可以直接回复“批准”或“拒绝”。"
	payload := map[string]any{
		"chat_id": chatID,
		"text":    text,
		"reply_markup": map[string]any{"inline_keyboard": [][]map[string]string{{
			{"text": "批准", "callback_data": callbackPrefix + "approve"},
			{"text": "拒绝", "callback_data": callbackPrefix + "reject"},
		}}},
	}
	var result telegramEnvelope[telegramMessage]
	if err := p.call(ctx, "sendMessage", payload, &result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("Telegram 发送审批消息失败: %s", result.Description)
	}
	return nil
}

// Close 通过取消 Run 的 context 结束请求；这里无需持有额外长连接。
func (p *telegramPlatform) Close() error { return nil }

func (p *telegramPlatform) getUpdates(ctx context.Context, offset int64) ([]telegramUpdate, error) {
	payload := map[string]any{"timeout": telegramPollTimeout, "allowed_updates": []string{"message", "callback_query"}}
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
		ID:        strconv.FormatInt(update.UpdateID, 10),
		Platform:  TypeTelegram,
		UserID:    userID,
		ChatID:    strconv.FormatInt(item.Chat.ID, 10),
		ChatType:  item.Chat.Type,
		Text:      strings.TrimSpace(item.Text),
		Mentioned: p.telegramMessageMentioned(item),
	}
	if message.Text == "" {
		message.Text = strings.TrimSpace(item.Caption)
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
	if message.Text == "" && len(message.Attachments) == 0 {
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
	decision := false
	switch parts[3] {
	case "approve":
		decision = true
	case "reject":
	default:
		return Message{}, errors.New("Telegram 审批决定无效")
	}
	userID := strconv.FormatInt(callback.Message.Chat.ID, 10)
	if callback.From != nil && callback.From.ID != 0 {
		userID = strconv.FormatInt(callback.From.ID, 10)
	}
	return Message{
		ID: "callback:" + callback.ID, Platform: TypeTelegram, UserID: userID,
		ChatID: strconv.FormatInt(callback.Message.Chat.ID, 10), ChatType: callback.Message.Chat.Type,
		Mentioned: true, Control: &MessageControl{Kind: "approval", ApprovalID: parts[2], Decision: decision, CallbackID: callback.ID},
	}, nil
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

func splitText(text string, limit int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	runes := []rune(text)
	result := make([]string, 0, (len(runes)+limit-1)/limit)
	for len(runes) > 0 {
		size := limit
		if len(runes) < size {
			size = len(runes)
		}
		result = append(result, string(runes[:size]))
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
