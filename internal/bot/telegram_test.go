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
