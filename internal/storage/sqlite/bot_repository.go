package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"Abot/internal/bot"
	"gorm.io/gorm"
)

// botRow 保存平台连接配置；Token 保留在数据库和进程内，公开 API 只返回是否已配置。
type botRow struct {
	ID         string `gorm:"primaryKey;size:64"`
	Name       string `gorm:"size:200;not null"`
	Type       string `gorm:"size:32;not null"`
	Endpoint   string `gorm:"size:1000"`
	OneBotMode string `gorm:"size:32"`
	ListenHost string `gorm:"size:255"`
	ListenPort int
	ListenPath string `gorm:"size:500"`
	// OneBotFileRoot 保存当前机器人对应的附件宿主机目录；空值表示使用兼容兜底路径。
	OneBotFileRoot   string `gorm:"size:2000"`
	GroupTriggerMode string `gorm:"size:16"`
	// AdminUserIDsJSON stores the global administrators of this bot. It is a
	// JSON array so an existing row needs no migration step.
	AdminUserIDsJSON  string `gorm:"type:text"`
	TelegramToken     string `gorm:"size:4000"`
	OneBotAccessToken string `gorm:"size:4000"`
	Enabled           bool
	Status            string `gorm:"size:32;not null"`
	StatusMessage     string `gorm:"size:1000"`
	LastCheckedAt     *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func (botRow) TableName() string { return "abot_bots" }

// botSourceNameRow 保存 /name 为消息来源设置的显示名称；来源本身是稳定主键。
type botSourceNameRow struct {
	Source    string `gorm:"primaryKey;size:500"`
	Name      string `gorm:"size:300;not null"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (botSourceNameRow) TableName() string { return "abot_bot_source_names" }

// botMessageSourceRow 保存消息入口观察到的 UMO；它不依赖是否创建了对话，
// 因而能完整覆盖被唤醒规则拦截的消息和只执行内置指令的消息。
type botMessageSourceRow struct {
	Source      string `gorm:"primaryKey;size:500"`
	AdapterID   string `gorm:"size:64;index"`
	Platform    string `gorm:"size:32;index"`
	MessageType string `gorm:"size:64;index"`
	SessionID   string `gorm:"size:300;index"`
	UserID      string `gorm:"size:300;index"`
	AutoName    string `gorm:"size:300"`
	FirstSeenAt time.Time
	LastSeenAt  time.Time `gorm:"index"`
}

func (botMessageSourceRow) TableName() string { return "abot_bot_message_sources" }

type botRepository struct {
	db *gorm.DB
}

func (r *botRepository) List(ctx context.Context) ([]bot.Bot, error) {
	var rows []botRow
	if err := r.db.WithContext(ctx).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]bot.Bot, 0, len(rows))
	for _, row := range rows {
		result = append(result, botFromRow(row))
	}
	return result, nil
}

func (r *botRepository) Get(ctx context.Context, id string) (bot.Bot, error) {
	var row botRow
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return bot.Bot{}, bot.ErrNotFound
		}
		return bot.Bot{}, err
	}
	return botFromRow(row), nil
}

func (r *botRepository) Save(ctx context.Context, item bot.Bot) error {
	row := botRow{
		ID: item.ID, Name: item.Name, Type: string(item.Type), Endpoint: item.Endpoint,
		OneBotMode: item.OneBotMode, ListenHost: item.ListenHost, ListenPort: item.ListenPort, ListenPath: item.ListenPath, OneBotFileRoot: item.OneBotFileRoot,
		GroupTriggerMode: item.GroupTriggerMode,
		AdminUserIDsJSON: marshalBotAdminIDs(item.AdminUserIDs),
		TelegramToken:    item.TelegramToken, OneBotAccessToken: item.OneBotAccessToken,
		Enabled: item.Enabled, Status: string(item.Status), StatusMessage: item.StatusMessage,
		LastCheckedAt: item.LastCheckedAt, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
	return r.db.WithContext(ctx).Save(&row).Error
}

func (r *botRepository) Delete(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 删除机器人时同步清理配置绑定，防止已不存在的机器人形成孤儿引用。
		if err := tx.Where("scope = ? AND target_id = ?", "bot", id).Delete(&configBindingRow{}).Error; err != nil {
			return err
		}
		// 机器人删除后同步清理该 adapter 产生的来源别名，避免留下无法再管理的 UMO 元数据。
		if err := tx.Where("source LIKE ?", id+":%").Delete(&botSourceNameRow{}).Error; err != nil {
			return err
		}
		// 来源目录按 adapter_id 清理；UMO 的第一段就是 adapter ID，不能用
		// 中间包含字符串的模糊匹配，否则删除某个机器人时会误删其他来源。
		if err := tx.Where("adapter_id = ?", id).Delete(&botMessageSourceRow{}).Error; err != nil {
			return err
		}
		result := tx.Where("id = ?", id).Delete(&botRow{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return bot.ErrNotFound
		}
		return nil
	})
}

// GetSourceName 读取来源名称；没有配置名称时按空值返回，避免正常首次读取制造错误日志。
func (r *botRepository) GetSourceName(ctx context.Context, source string) (string, error) {
	var row botSourceNameRow
	err := r.db.WithContext(ctx).Where("source = ?", strings.TrimSpace(source)).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(row.Name), nil
}

// ListSourceNames 返回历史别名目录，兼容来源登记表启用前通过 /name 保存的 UMO。
func (r *botRepository) ListSourceNames(ctx context.Context, query string) ([]bot.SourceName, error) {
	db := r.db.WithContext(ctx)
	query = strings.TrimSpace(query)
	if query != "" {
		like := "%" + query + "%"
		db = db.Where("source LIKE ? OR name LIKE ?", like, like)
	}
	var rows []botSourceNameRow
	if err := db.Order("updated_at DESC").Order("source ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]bot.SourceName, 0, len(rows))
	for _, row := range rows {
		items = append(items, bot.SourceName{Source: row.Source, Name: row.Name, UpdatedAt: row.UpdatedAt})
	}
	return items, nil
}

// SetSourceName 保存来源名称；空名称用于显式清除旧别名，名称内容不进入其他配置表。
func (r *botRepository) SetSourceName(ctx context.Context, source, name string) error {
	source = strings.TrimSpace(source)
	name = strings.TrimSpace(name)
	if name == "" {
		return r.db.WithContext(ctx).Where("source = ?", source).Delete(&botSourceNameRow{}).Error
	}
	now := time.Now().UTC()
	row := botSourceNameRow{Source: source, Name: name, CreatedAt: now, UpdatedAt: now}
	var existing botSourceNameRow
	if err := r.db.WithContext(ctx).Where("source = ?", source).First(&existing).Error; err == nil {
		row.CreatedAt = existing.CreatedAt
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return r.db.WithContext(ctx).Save(&row).Error
}

// UpsertMessageSource 登记或刷新一个 UMO；FirstSeenAt 只在首次观察时写入，
// LastSeenAt 每次收到消息都会刷新，供 WebUI 排序和状态展示使用。
func (r *botRepository) UpsertMessageSource(ctx context.Context, item bot.MessageSource) error {
	item.Source = strings.TrimSpace(item.Source)
	if item.Source == "" {
		return errors.New("消息来源不能为空")
	}
	now := time.Now().UTC()
	if item.LastSeenAt.IsZero() {
		item.LastSeenAt = now
	}
	var existing botMessageSourceRow
	err := r.db.WithContext(ctx).Where("source = ?", item.Source).First(&existing).Error
	if err == nil {
		item.FirstSeenAt = existing.FirstSeenAt
	} else if errors.Is(err, gorm.ErrRecordNotFound) {
		if item.FirstSeenAt.IsZero() {
			item.FirstSeenAt = item.LastSeenAt
		}
	} else {
		return err
	}
	if item.FirstSeenAt.IsZero() {
		item.FirstSeenAt = now
	}
	row := botMessageSourceRow{
		Source: item.Source, AdapterID: strings.TrimSpace(item.AdapterID), Platform: strings.TrimSpace(item.Platform),
		MessageType: strings.TrimSpace(item.MessageType), SessionID: strings.TrimSpace(item.SessionID), UserID: strings.TrimSpace(item.UserID),
		AutoName: strings.TrimSpace(item.AutoName), FirstSeenAt: item.FirstSeenAt, LastSeenAt: item.LastSeenAt,
	}
	return r.db.WithContext(ctx).Save(&row).Error
}

// ListMessageSources 返回已观察到的 UMO，查询覆盖来源键、自动名称、平台、
// 消息类型、会话 ID 和用户 ID，满足来源选择器的检索习惯。
func (r *botRepository) ListMessageSources(ctx context.Context, query string) ([]bot.MessageSource, error) {
	db := r.db.WithContext(ctx)
	query = strings.TrimSpace(query)
	if query != "" {
		like := "%" + query + "%"
		db = db.Where("source LIKE ? OR auto_name LIKE ? OR adapter_id LIKE ? OR platform LIKE ? OR message_type LIKE ? OR session_id LIKE ? OR user_id LIKE ?", like, like, like, like, like, like, like)
	}
	var rows []botMessageSourceRow
	if err := db.Order("last_seen_at DESC").Order("source ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]bot.MessageSource, 0, len(rows))
	for _, row := range rows {
		items = append(items, bot.MessageSource{
			Source: row.Source, AdapterID: row.AdapterID, Platform: row.Platform, MessageType: row.MessageType,
			SessionID: row.SessionID, UserID: row.UserID, AutoName: row.AutoName,
			FirstSeenAt: row.FirstSeenAt, LastSeenAt: row.LastSeenAt,
		})
	}
	return items, nil
}

func botFromRow(row botRow) bot.Bot {
	return bot.Bot{
		ID: row.ID, Name: row.Name, Type: bot.Type(row.Type), Endpoint: row.Endpoint,
		OneBotMode: row.OneBotMode, ListenHost: row.ListenHost, ListenPort: row.ListenPort, ListenPath: row.ListenPath, OneBotFileRoot: row.OneBotFileRoot,
		GroupTriggerMode: row.GroupTriggerMode,
		AdminUserIDs:     unmarshalBotAdminIDs(row.AdminUserIDsJSON),
		TelegramToken:    row.TelegramToken, OneBotAccessToken: row.OneBotAccessToken,
		Enabled: row.Enabled, Status: bot.Status(row.Status), StatusMessage: row.StatusMessage,
		LastCheckedAt: row.LastCheckedAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

// marshalBotAdminIDs keeps the administrator list bounded and stable so a save
// round-trip does not reorder it.
func marshalBotAdminIDs(values []string) string {
	if len(values) == 0 {
		return ""
	}
	cleaned := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 64 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		cleaned = append(cleaned, value)
	}
	if len(cleaned) == 0 {
		return ""
	}
	encoded, err := json.Marshal(cleaned)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func unmarshalBotAdminIDs(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var result []string
	if err := json.Unmarshal([]byte(value), &result); err != nil {
		return nil
	}
	cleaned := make([]string, 0, len(result))
	for _, item := range result {
		if item = strings.TrimSpace(item); item != "" {
			cleaned = append(cleaned, item)
		}
	}
	if len(cleaned) == 0 {
		return nil
	}
	return cleaned
}
