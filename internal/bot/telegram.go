package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
	bot    Bot
	client *http.Client
	base   string
}

type telegramEnvelope[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
}

type telegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *telegramMessage `json:"message"`
}

type telegramMessage struct {
	MessageID int64             `json:"message_id"`
	From      *telegramUser     `json:"from"`
	Chat      telegramChat      `json:"chat"`
	Text      string            `json:"text"`
	Caption   string            `json:"caption"`
	Photo     []telegramPhoto   `json:"photo"`
	Document  *telegramDocument `json:"document"`
}

type telegramUser struct {
	ID int64 `json:"id"`
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
			if update.Message == nil {
				continue
			}
			message, err := p.messageFromUpdate(ctx, update)
			if err != nil {
				// 附件下载失败不应让 Telegram 长轮询整体退出；文字仍然可以交给 Agent。
				continue
			}
			if handlerErr := handler(ctx, message); handlerErr != nil {
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

// Close 通过取消 Run 的 context 结束请求；这里无需持有额外长连接。
func (p *telegramPlatform) Close() error { return nil }

func (p *telegramPlatform) getUpdates(ctx context.Context, offset int64) ([]telegramUpdate, error) {
	payload := map[string]any{"timeout": telegramPollTimeout, "allowed_updates": []string{"message"}}
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
	item := update.Message
	userID := strconv.FormatInt(item.Chat.ID, 10)
	if item.From != nil && item.From.ID != 0 {
		userID = strconv.FormatInt(item.From.ID, 10)
	}
	message := Message{
		ID:       strconv.FormatInt(update.UpdateID, 10),
		Platform: TypeTelegram,
		UserID:   userID,
		ChatID:   strconv.FormatInt(item.Chat.ID, 10),
		ChatType: item.Chat.Type,
		Text:     strings.TrimSpace(item.Text),
	}
	if message.Text == "" {
		message.Text = strings.TrimSpace(item.Caption)
	}
	if len(item.Photo) > 0 {
		photo := item.Photo[len(item.Photo)-1]
		if attachment, err := p.downloadFile(ctx, photo.FileID, fmt.Sprintf("telegram-photo-%d.jpg", item.MessageID), "image/jpeg"); err == nil {
			message.Attachments = append(message.Attachments, attachment)
		}
	}
	if item.Document != nil {
		name := cleanFileName(item.Document.FileName)
		if name == "" {
			name = fmt.Sprintf("telegram-file-%d", item.MessageID)
		}
		mimeType := normalizeFileMIME(name, item.Document.MIMEType)
		if attachment, err := p.downloadFile(ctx, item.Document.FileID, name, mimeType); err == nil {
			if attachmentBytes(message.Attachments)+int64(len(attachment.Data)) <= platformAttachmentSum {
				message.Attachments = append(message.Attachments, attachment)
			}
		}
	}
	if message.Text == "" && len(message.Attachments) == 0 {
		return Message{}, errors.New("Telegram 更新不包含文本或支持的附件")
	}
	return message, nil
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
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("Telegram 请求创建失败: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("Telegram API 请求失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("Telegram API 返回 HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(target); err != nil {
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
