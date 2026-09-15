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
	ID               string `gorm:"primaryKey;size:64"`
	Name             string `gorm:"size:200;not null"`
	Type             string `gorm:"size:32;not null"`
	Endpoint         string `gorm:"size:1000"`
	OneBotMode       string `gorm:"size:32"`
	ListenHost       string `gorm:"size:255"`
	ListenPort       int
	ListenPath       string `gorm:"size:500"`
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
		OneBotMode: item.OneBotMode, ListenHost: item.ListenHost, ListenPort: item.ListenPort, ListenPath: item.ListenPath,
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

func botFromRow(row botRow) bot.Bot {
	return bot.Bot{
		ID: row.ID, Name: row.Name, Type: bot.Type(row.Type), Endpoint: row.Endpoint,
		OneBotMode: row.OneBotMode, ListenHost: row.ListenHost, ListenPort: row.ListenPort, ListenPath: row.ListenPath,
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
