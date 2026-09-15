package sqlite

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"Abot/internal/bot"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// This file persists the chat-command permission model: delegated group
// administrators, per-bot command policy overrides and the audit trail. Global
// administrators live on the bot row itself because only the WebUI writes them.

type botGroupAdminRow struct {
	AdapterID string `gorm:"primaryKey;size:64"`
	ChatID    string `gorm:"primaryKey;size:128"`
	UserID    string `gorm:"primaryKey;size:64"`
	AddedBy   string `gorm:"size:64"`
	CreatedAt time.Time
}

func (botGroupAdminRow) TableName() string { return "abot_bot_group_admins" }

type botCommandPolicyRow struct {
	AdapterID  string `gorm:"primaryKey;size:64"`
	CommandID  string `gorm:"primaryKey;size:64"`
	Permission string `gorm:"size:32;not null"`
	Enabled    bool
	UpdatedAt  time.Time
}

func (botCommandPolicyRow) TableName() string { return "abot_bot_command_policies" }

type botCommandAuditRow struct {
	ID        string    `gorm:"primaryKey;size:64"`
	AdapterID string    `gorm:"index;size:64;not null"`
	ChatID    string    `gorm:"index;size:128"`
	UserID    string    `gorm:"size:64"`
	Command   string    `gorm:"size:64"`
	Action    string    `gorm:"index;size:48;not null"`
	Target    string    `gorm:"size:128"`
	Result    string    `gorm:"size:64;not null"`
	CreatedAt time.Time `gorm:"index"`
}

func (botCommandAuditRow) TableName() string { return "abot_bot_command_audits" }

var (
	_ bot.GroupAdminRepository    = (*botRepository)(nil)
	_ bot.CommandPolicyRepository = (*botRepository)(nil)
	_ bot.CommandAuditRepository  = (*botRepository)(nil)
)

// ListGroupAdmins returns the delegated administrators of one group chat,
// ordered for stable display.
func (r *botRepository) ListGroupAdmins(ctx context.Context, adapterID, chatID string) ([]string, error) {
	adapterID = strings.TrimSpace(adapterID)
	chatID = strings.TrimSpace(chatID)
	if adapterID == "" || chatID == "" {
		return nil, nil
	}
	var rows []botGroupAdminRow
	if err := r.db.WithContext(nonNilContext(ctx)).
		Where("adapter_id = ? AND chat_id = ?", adapterID, chatID).
		Order("user_id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]string, 0, len(rows))
	for _, row := range rows {
		result = append(result, row.UserID)
	}
	return result, nil
}

// AddGroupAdmin is idempotent: re-adding an existing administrator keeps the
// original attribution and timestamp.
func (r *botRepository) AddGroupAdmin(ctx context.Context, admin bot.GroupAdmin) error {
	adapterID := strings.TrimSpace(admin.AdapterID)
	chatID := strings.TrimSpace(admin.ChatID)
	userID := strings.TrimSpace(admin.UserID)
	if adapterID == "" || chatID == "" || userID == "" {
		return errors.New("群管理员缺少适配器、聊天或用户标识")
	}
	createdAt := admin.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	row := botGroupAdminRow{AdapterID: adapterID, ChatID: chatID, UserID: userID, AddedBy: strings.TrimSpace(admin.AddedBy), CreatedAt: createdAt}
	return r.db.WithContext(nonNilContext(ctx)).Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error
}

func (r *botRepository) RemoveGroupAdmin(ctx context.Context, adapterID, chatID, userID string) error {
	result := r.db.WithContext(nonNilContext(ctx)).
		Where("adapter_id = ? AND chat_id = ? AND user_id = ?", strings.TrimSpace(adapterID), strings.TrimSpace(chatID), strings.TrimSpace(userID)).
		Delete(&botGroupAdminRow{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// ListCommandPolicies returns the per-bot overrides.
func (r *botRepository) ListCommandPolicies(ctx context.Context, adapterID string) ([]bot.CommandPolicy, error) {
	adapterID = strings.TrimSpace(adapterID)
	if adapterID == "" {
		return nil, nil
	}
	var rows []botCommandPolicyRow
	if err := r.db.WithContext(nonNilContext(ctx)).
		Where("adapter_id = ?", adapterID).
		Order("command_id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]bot.CommandPolicy, 0, len(rows))
	for _, row := range rows {
		result = append(result, bot.CommandPolicy{CommandID: row.CommandID, Permission: bot.Permission(row.Permission), Enabled: row.Enabled})
	}
	return result, nil
}

func (r *botRepository) SaveCommandPolicy(ctx context.Context, adapterID string, policy bot.CommandPolicy) error {
	adapterID = strings.TrimSpace(adapterID)
	commandID := strings.TrimSpace(policy.CommandID)
	if adapterID == "" || commandID == "" {
		return errors.New("指令策略缺少适配器或指令标识")
	}
	row := botCommandPolicyRow{AdapterID: adapterID, CommandID: commandID, Permission: string(policy.Permission), Enabled: policy.Enabled, UpdatedAt: time.Now().UTC()}
	return r.db.WithContext(nonNilContext(ctx)).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "adapter_id"}, {Name: "command_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"permission", "enabled", "updated_at"}),
	}).Create(&row).Error
}

func (r *botRepository) AppendCommandAudit(ctx context.Context, entry bot.CommandAudit) error {
	id := strings.TrimSpace(entry.ID)
	if id == "" {
		return errors.New("审计记录缺少 ID")
	}
	createdAt := entry.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	row := botCommandAuditRow{
		ID: id, AdapterID: strings.TrimSpace(entry.AdapterID), ChatID: strings.TrimSpace(entry.ChatID),
		UserID: strings.TrimSpace(entry.UserID), Command: strings.TrimSpace(entry.Command),
		Action: strings.TrimSpace(entry.Action), Target: strings.TrimSpace(entry.Target),
		Result: strings.TrimSpace(entry.Result), CreatedAt: createdAt,
	}
	return r.db.WithContext(nonNilContext(ctx)).Create(&row).Error
}

// ListCommandAudits returns the newest entries first, bounded by limit.
func (r *botRepository) ListCommandAudits(ctx context.Context, adapterID string, limit int) ([]bot.CommandAudit, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := r.db.WithContext(nonNilContext(ctx)).Model(&botCommandAuditRow{})
	if adapterID = strings.TrimSpace(adapterID); adapterID != "" {
		query = query.Where("adapter_id = ?", adapterID)
	}
	var rows []botCommandAuditRow
	if err := query.Order("created_at DESC, id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]bot.CommandAudit, 0, len(rows))
	for _, row := range rows {
		result = append(result, bot.CommandAudit{
			ID: row.ID, AdapterID: row.AdapterID, ChatID: row.ChatID, UserID: row.UserID,
			Command: row.Command, Action: row.Action, Target: row.Target, Result: row.Result, CreatedAt: row.CreatedAt,
		})
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result, nil
}
