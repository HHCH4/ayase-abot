package bot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestValidateBot(t *testing.T) {
	tests := []struct {
		name string
		item Bot
		want bool
	}{
		{name: "telegram", item: Bot{ID: "telegram-main", Name: "Telegram", Type: TypeTelegram, TelegramToken: "123:token"}, want: true},
		{name: "onebot reverse server", item: Bot{ID: "qq-server", Name: "QQ", Type: TypeOneBot11}, want: true},
		{name: "onebot client", item: Bot{ID: "qq", Name: "QQ", Type: TypeOneBot11, OneBotMode: OneBotModeClient, Endpoint: "ws://127.0.0.1:6700"}, want: true},
		{name: "legacy onebot client", item: Bot{ID: "qq-legacy", Name: "QQ", Type: TypeOneBot11, Endpoint: "ws://127.0.0.1:6700"}, want: true},
		{name: "bad id", item: Bot{ID: "Bad ID", Name: "QQ", Type: TypeOneBot11, OneBotMode: OneBotModeClient, Endpoint: "ws://127.0.0.1:6700"}, want: false},
		{name: "bad endpoint", item: Bot{ID: "qq", Name: "QQ", Type: TypeOneBot11, OneBotMode: OneBotModeClient, Endpoint: "http://127.0.0.1:6700"}, want: false},
		{name: "bad reverse port", item: Bot{ID: "qq-server", Name: "QQ", Type: TypeOneBot11, ListenPort: 65536}, want: false},
		{name: "missing token", item: Bot{ID: "telegram", Name: "Telegram", Type: TypeTelegram}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.item.Validate() == nil; got != test.want {
				t.Fatalf("Validate()=%v, want %v", got, test.want)
			}
		})
	}
}

func TestOneBotListenNetwork(t *testing.T) {
	if got := oneBotListenNetwork("0.0.0.0"); got != "tcp4" {
		t.Fatalf("IPv4 默认监听网络 = %q，期望 tcp4", got)
	}
	if got := oneBotListenNetwork("127.0.0.1"); got != "tcp4" {
		t.Fatalf("IPv4 回环监听网络 = %q，期望 tcp4", got)
	}
	if got := oneBotListenNetwork("::"); got != "tcp6" {
		t.Fatalf("IPv6 监听网络 = %q，期望 tcp6", got)
	}
}

func TestBindingForIncludesAdapterAndChat(t *testing.T) {
	firstUser, firstConversation := bindingFor(Message{AdapterID: "one", Platform: TypeTelegram, ChatType: "private", ChatID: "42"})
	secondUser, secondConversation := bindingFor(Message{AdapterID: "two", Platform: TypeTelegram, ChatType: "private", ChatID: "42"})
	if firstUser == secondUser || firstConversation == secondConversation {
		t.Fatal("不同 adapter 不应共享同一机器人会话键")
	}
}

func TestParseOneBotMessage(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("image bytes"))
	raw, err := json.Marshal([]map[string]any{
		{"type": "text", "data": map[string]any{"text": "请看 "}},
		{"type": "image", "data": map[string]any{"file": "base64://" + encoded, "url": ""}},
	})
	if err != nil {
		t.Fatal(err)
	}
	text, attachments := parseOneBotMessage(context.Background(), nil, raw)
	if text != "请看 " || len(attachments) != 1 || string(attachments[0].Data) != "image bytes" {
		t.Fatalf("text=%q attachments=%d", text, len(attachments))
	}
}

func TestSplitTextDoesNotBreakUTF8(t *testing.T) {
	chunks := splitText("你好世界", 2)
	if len(chunks) != 2 || chunks[0] != "你好" || chunks[1] != "世界" {
		t.Fatalf("chunks=%q", chunks)
	}
}

func TestRequestTimeoutResolver(t *testing.T) {
	manager := &Manager{}
	if timeout, err := manager.resolveRequestTimeout(context.Background()); err != nil || timeout != 5*time.Minute {
		t.Fatalf("没有解析器时的请求超时 = %v, err=%v", timeout, err)
	}
	manager.SetRequestTimeoutResolver(func(context.Context) (time.Duration, error) { return 42 * time.Second, nil })
	if timeout, err := manager.resolveRequestTimeout(context.Background()); err != nil || timeout != 42*time.Second {
		t.Fatalf("配置中心请求超时 = %v, err=%v", timeout, err)
	}
	resolverErr := errors.New("设置读取失败")
	manager.SetRequestTimeoutResolver(func(context.Context) (time.Duration, error) { return 0, resolverErr })
	if _, err := manager.resolveRequestTimeout(context.Background()); !errors.Is(err, resolverErr) {
		t.Fatalf("请求超时读取错误 = %v，期望保留原错误", err)
	}
}
