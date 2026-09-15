package bot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTelegramTestAndSend(t *testing.T) {
	var sent []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(request.URL.Path, "/getMe"):
			_, _ = writer.Write([]byte(`{"ok":true,"result":{"id":123}}`))
		case strings.HasSuffix(request.URL.Path, "/sendMessage"):
			var payload struct {
				ChatID string `json:"chat_id"`
				Text   string `json:"text"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Errorf("解析 sendMessage 请求失败: %v", err)
			}
			if payload.ChatID != "42" {
				t.Errorf("chat_id = %q，期望 42", payload.ChatID)
			}
			sent = append(sent, payload.Text)
			_, _ = writer.Write([]byte(`{"ok":true,"result":{}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	platform := &telegramPlatform{
		bot:    Bot{ID: "telegram", Name: "测试", Type: TypeTelegram, TelegramToken: "123:secret"},
		client: server.Client(), base: server.URL,
	}
	if err := platform.Test(context.Background()); err != nil {
		t.Fatalf("Telegram getMe 测试失败: %v", err)
	}
	if err := platform.Send(context.Background(), Message{ChatID: "42"}, strings.Repeat("中", telegramMessageLimit+1)); err != nil {
		t.Fatalf("Telegram 发送失败: %v", err)
	}
	if len(sent) != 2 || len([]rune(sent[0])) != telegramMessageLimit || len([]rune(sent[1])) != 1 {
		t.Fatalf("Telegram 分片结果不正确: %d 个分片、长度=%d/%d", len(sent), len([]rune(sent[0])), len([]rune(sent[1])))
	}
}

func TestTelegramMessageFromUpdateDownloadsVoiceAndAudio(t *testing.T) {
	files := map[string][]byte{
		"voice-id": []byte("ogg voice"),
		"audio-id": []byte("mp3 audio"),
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(request.URL.Path, "/getFile") {
			var payload struct {
				FileID string `json:"file_id"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Errorf("解析 getFile 请求失败: %v", err)
				return
			}
			if _, ok := files[payload.FileID]; !ok {
				writer.WriteHeader(http.StatusBadRequest)
				_, _ = writer.Write([]byte(`{"ok":false,"description":"unknown file"}`))
				return
			}
			_, _ = writer.Write([]byte(`{"ok":true,"result":{"file_path":"` + payload.FileID + `"}}`))
			return
		}
		if strings.Contains(request.URL.Path, "/file/bot") {
			for id, data := range files {
				if strings.HasSuffix(request.URL.Path, "/"+id) {
					writer.Header().Set("Content-Type", "application/octet-stream")
					_, _ = writer.Write(data)
					return
				}
			}
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()

	platform := &telegramPlatform{
		bot:    Bot{ID: "telegram", Name: "测试", Type: TypeTelegram, TelegramToken: "123:secret"},
		client: server.Client(), base: server.URL,
	}
	message, err := platform.messageFromUpdate(context.Background(), telegramUpdate{
		UpdateID: 77,
		Message: &telegramMessage{
			MessageID: 9, From: &telegramUser{ID: 1001}, Chat: telegramChat{ID: 42, Type: "private"},
			Voice: &telegramVoice{FileID: "voice-id", MIMEType: "audio/ogg"},
			Audio: &telegramAudio{FileID: "audio-id", FileName: "song.mp3"},
		},
	})
	if err != nil {
		t.Fatalf("解析 Telegram 音频更新失败: %v", err)
	}
	if len(message.Attachments) != 2 {
		t.Fatalf("音频附件数量=%d，期望 2: %#v", len(message.Attachments), message.Attachments)
	}
	if message.Attachments[0].Name != "telegram-voice-9.ogg" || message.Attachments[0].MIMEType != "audio/ogg" || string(message.Attachments[0].Data) != "ogg voice" {
		t.Fatalf("voice 附件解析错误: %#v", message.Attachments[0])
	}
	if message.Attachments[1].Name != "song.mp3" || message.Attachments[1].MIMEType != "audio/mpeg" || string(message.Attachments[1].Data) != "mp3 audio" {
		t.Fatalf("audio 附件解析错误: %#v", message.Attachments[1])
	}
}

func TestTelegramApprovalCallbackAndMentionTargeting(t *testing.T) {
	platform := &telegramPlatform{username: "abot"}
	message, err := platform.messageFromCallback(&telegramCallbackQuery{
		ID: "callback-1", From: &telegramUser{ID: 1001}, Data: "abot:approval:approval-1:approve",
		Message: &telegramMessage{Chat: telegramChat{ID: 42, Type: "group"}},
	})
	if err != nil || message.Control == nil || !message.Control.Decision || message.Control.ApprovalID != "approval-1" {
		t.Fatalf("Telegram 审批回调解析错误: message=%#v err=%v", message, err)
	}
	if platform.telegramMessageMentioned(&telegramMessage{Text: "/status", Entities: []telegramEntity{{Type: "bot_command", Offset: 0, Length: 7}}}) != true {
		t.Fatal("群聊中的裸 bot command 应可触发命令")
	}
	if platform.telegramMessageMentioned(&telegramMessage{Text: "/status@other", Entities: []telegramEntity{{Type: "bot_command", Offset: 0, Length: 13}}}) {
		t.Fatal("指向其他机器人的 command 不应触发当前机器人")
	}
	if !platform.telegramMessageMentioned(&telegramMessage{Text: "你好 @abot", Entities: []telegramEntity{{Type: "mention", Offset: 3, Length: 5}}}) {
		t.Fatal("指向当前机器人的 mention 未识别")
	}
}

func TestTelegramSendApprovalUsesInlineKeyboard(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("解析审批消息失败: %v", err)
		}
		_, _ = writer.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer server.Close()
	platform := &telegramPlatform{bot: Bot{TelegramToken: "token"}, client: server.Client(), base: server.URL}
	if err := platform.SendApproval(context.Background(), Message{ChatID: "42"}, ApprovalPrompt{ApprovalID: "approval-1", ToolName: "write_file", Hint: "即将修改文件"}); err != nil {
		t.Fatalf("发送 Telegram 审批消息失败: %v", err)
	}
	markup, ok := payload["reply_markup"].(map[string]any)
	if !ok {
		t.Fatalf("审批消息缺少 inline keyboard: %#v", payload)
	}
	encoded, _ := json.Marshal(markup)
	if !strings.Contains(string(encoded), "abot:approval:approval-1:approve") || !strings.Contains(string(encoded), "abot:approval:approval-1:reject") {
		t.Fatalf("inline keyboard callback data 错误: %s", encoded)
	}
}
