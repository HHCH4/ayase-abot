package bot

import "context"

// newPlatform 根据持久化配置创建具体平台适配器，管理器不依赖平台实现细节。
func newPlatform(item Bot) (platform, error) {
	switch item.Type {
	case TypeTelegram:
		return newTelegramPlatform(item)
	case TypeOneBot11:
		return newOneBotPlatform(item)
	default:
		return nil, ErrUnsupported
	}
}

// compile-time check 保证两个内置平台始终实现统一连接接口。
var (
	_ platform = (*telegramPlatform)(nil)
	_ platform = (*oneBotPlatform)(nil)
	_ Handler  = func(context.Context, Message) error { return nil }
)
