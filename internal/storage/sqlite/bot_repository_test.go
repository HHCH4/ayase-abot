package sqlite

import (
	"context"
	"testing"

	"Abot/internal/bot"
)

func TestBotRepositoryPersistsSecretsAndRuntimeState(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开 SQLite Store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	repo := store.BotRepository()
	item := bot.Bot{
		ID: "telegram-main", Name: "主 Telegram", Type: bot.TypeTelegram,
		TelegramToken: "telegram-secret", Enabled: true, Status: bot.StatusRunning,
	}
	if err := repo.Save(context.Background(), item); err != nil {
		t.Fatalf("保存机器人失败: %v", err)
	}
	got, err := repo.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("读取机器人失败: %v", err)
	}
	if got.TelegramToken != item.TelegramToken || got.Status != bot.StatusRunning || !got.Enabled {
		t.Fatalf("读取后的机器人配置不完整: %#v", got)
	}
	items, err := repo.List(context.Background())
	if err != nil || len(items) != 1 {
		t.Fatalf("机器人列表异常: len=%d err=%v", len(items), err)
	}
	if err := repo.Delete(context.Background(), item.ID); err != nil {
		t.Fatalf("删除机器人失败: %v", err)
	}
	if _, err := repo.Get(context.Background(), item.ID); err != bot.ErrNotFound {
		t.Fatalf("删除后读取错误 = %v，期望 ErrNotFound", err)
	}
}

func TestBotRepositoryPersistsOneBotReverseWebSocketConfig(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开 SQLite Store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	item := bot.Bot{
		ID: "qq-server", Name: "QQ 反向连接", Type: bot.TypeOneBot11,
		OneBotMode: bot.OneBotModeReverseServer, ListenHost: "0.0.0.0", ListenPort: 6199, ListenPath: "/ws",
		OneBotAccessToken: "onebot-secret", Enabled: true, Status: bot.StatusConfigured,
	}
	if err := store.BotRepository().Save(context.Background(), item); err != nil {
		t.Fatalf("保存 OneBot 反向连接失败: %v", err)
	}
	got, err := store.BotRepository().Get(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("读取 OneBot 反向连接失败: %v", err)
	}
	if got.OneBotMode != item.OneBotMode || got.ListenHost != item.ListenHost || got.ListenPort != item.ListenPort || got.ListenPath != item.ListenPath || got.OneBotAccessToken != item.OneBotAccessToken {
		t.Fatalf("OneBot 反向连接配置持久化不完整: %#v", got)
	}
}
