package sqlite

import (
	"context"
	"errors"
	"time"

	"Abot/internal/memory"
	"gorm.io/gorm"
)

// memoryRow 保存长期记忆文本及其来源；删除时使用 DELETE，避免留下可恢复的软删除记录。
type memoryRow struct {
	ID             string `gorm:"primaryKey;size:100"`
	AppName        string `gorm:"index;size:100;not null"`
	UserID         string `gorm:"index;size:300;not null"`
	ConversationID string `gorm:"index;size:100"`
	SourceEventID  string `gorm:"index;size:300"`
	Author         string `gorm:"size:100"`
	Source         string `gorm:"size:32;not null"`
	Content        string `gorm:"type:text;not null"`
	Tags           string `gorm:"size:1000"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (memoryRow) TableName() string { return "abot_memories" }

type memoryRepository struct {
	db *gorm.DB
}

func (r *memoryRepository) List(ctx context.Context, userID, appName, query string, limit int) ([]memory.Item, error) {
	db := r.db.WithContext(ctx).Where("user_id = ? AND app_name = ?", userID, appName)
	if query != "" {
		// instr 使用普通子串匹配，不把用户输入当作 LIKE 通配符或 SQL 片段。
		db = db.Where("instr(lower(content), lower(?)) > 0 OR instr(lower(tags), lower(?)) > 0", query, query)
	}
	var rows []memoryRow
	if err := db.Order("created_at DESC, id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]memory.Item, 0, len(rows))
	for _, row := range rows {
		items = append(items, memoryFromRow(row))
	}
	return items, nil
}

func (r *memoryRepository) Get(ctx context.Context, userID, appName, id string) (memory.Item, error) {
	var row memoryRow
	if err := r.db.WithContext(ctx).Where("user_id = ? AND app_name = ? AND id = ?", userID, appName, id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return memory.Item{}, memory.ErrNotFound
		}
		return memory.Item{}, err
	}
	return memoryFromRow(row), nil
}

func (r *memoryRepository) Save(ctx context.Context, item memory.Item) error {
	row := memoryRow{
		ID: item.ID, AppName: item.AppName, UserID: item.UserID, ConversationID: item.ConversationID,
		SourceEventID: item.SourceEventID, Author: item.Author, Source: item.Source, Content: item.Content,
		Tags: item.Tags, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
	if row.Source == "" {
		row.Source = "manual"
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var old memoryRow
		if err := tx.Where("id = ?", row.ID).First(&old).Error; err == nil {
			if old.AppName != row.AppName || old.UserID != row.UserID {
				return memory.ErrConflict
			}
			row.CreatedAt = old.CreatedAt
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return tx.Save(&row).Error
	})
}

func (r *memoryRepository) Delete(ctx context.Context, userID, appName, id string) error {
	result := r.db.WithContext(ctx).Where("user_id = ? AND app_name = ? AND id = ?", userID, appName, id).Delete(&memoryRow{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return memory.ErrNotFound
	}
	return nil
}

func (r *memoryRepository) Clear(ctx context.Context, userID, appName string) error {
	return r.db.WithContext(ctx).Where("user_id = ? AND app_name = ?", userID, appName).Delete(&memoryRow{}).Error
}

func (r *memoryRepository) DeleteByConversation(ctx context.Context, conversationID string) error {
	return r.db.WithContext(ctx).Where("conversation_id = ?", conversationID).Delete(&memoryRow{}).Error
}

func (r *memoryRepository) Count(ctx context.Context, userID, appName string) (int, error) {
	var count int64
	if err := r.db.WithContext(ctx).Model(&memoryRow{}).Where("user_id = ? AND app_name = ?", userID, appName).Count(&count).Error; err != nil {
		return 0, err
	}
	return int(count), nil
}

func memoryFromRow(row memoryRow) memory.Item {
	return memory.Item{
		ID: row.ID, AppName: row.AppName, UserID: row.UserID, ConversationID: row.ConversationID,
		SourceEventID: row.SourceEventID, Author: row.Author, Source: row.Source, Content: row.Content,
		Tags: row.Tags, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}
