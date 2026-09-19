package sqlite

import (
	"context"
	"errors"
	"strings"
	"time"

	"Abot/internal/schedule"
	"gorm.io/gorm"
)

// scheduledTaskRow 保存未来任务的周期参数和最近一次运行结果。
type scheduledTaskRow struct {
	ID               string `gorm:"primaryKey;size:100"`
	Name             string `gorm:"size:200;not null"`
	Request          string `gorm:"type:text;not null"`
	Mode             string `gorm:"size:32;not null"`
	StartAt          *time.Time
	IntervalSeconds  int
	Weekday          int
	MonthDay         int
	TimeOfDay        string     `gorm:"size:5"`
	Cron             string     `gorm:"size:200"`
	UserID           string     `gorm:"index;size:200;not null"`
	ConversationID   string     `gorm:"index;size:100"`
	AdapterID        string     `gorm:"size:100"`
	ChatID           string     `gorm:"size:300"`
	Status           string     `gorm:"index;size:32;not null"`
	NextRunAt        *time.Time `gorm:"index"`
	LastRunAt        *time.Time
	LastInvocationID string `gorm:"size:100"`
	LastError        string `gorm:"type:text"`
	Running          bool   `gorm:"index"`
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func (scheduledTaskRow) TableName() string { return "abot_scheduled_tasks" }

type scheduleRepository struct{ db *gorm.DB }

func (r *scheduleRepository) List(ctx context.Context, status schedule.Status) ([]schedule.Task, error) {
	db := r.db.WithContext(ctx)
	if status != "" {
		db = db.Where("status = ?", string(status))
	}
	var rows []scheduledTaskRow
	if err := db.Order("next_run_at ASC, created_at DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]schedule.Task, 0, len(rows))
	for _, row := range rows {
		items = append(items, taskFromRow(row))
	}
	return items, nil
}

func (r *scheduleRepository) Get(ctx context.Context, id string) (schedule.Task, error) {
	var row scheduledTaskRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return schedule.Task{}, schedule.ErrNotFound
		}
		return schedule.Task{}, err
	}
	return taskFromRow(row), nil
}

func (r *scheduleRepository) Save(ctx context.Context, item schedule.Task) error {
	row := scheduledTaskRow{
		ID: item.ID, Name: item.Name, Request: item.Request, Mode: string(item.Mode), StartAt: item.StartAt,
		IntervalSeconds: item.IntervalSeconds, Weekday: item.Weekday, MonthDay: item.MonthDay, TimeOfDay: item.TimeOfDay,
		Cron: item.Cron, UserID: item.UserID, ConversationID: item.ConversationID, AdapterID: item.AdapterID, ChatID: item.ChatID,
		Status: string(item.Status), NextRunAt: item.NextRunAt, LastRunAt: item.LastRunAt, LastInvocationID: item.LastInvocationID,
		LastError: item.LastError, Running: item.Running, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
	return r.db.WithContext(ctx).Save(&row).Error
}

func (r *scheduleRepository) Delete(ctx context.Context, id string) error {
	result := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).Delete(&scheduledTaskRow{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return schedule.ErrNotFound
	}
	return nil
}

func (r *scheduleRepository) ClaimDue(ctx context.Context, now time.Time, limit int) ([]schedule.Task, error) {
	if limit < 1 {
		limit = 20
	}
	var rows []scheduledTaskRow
	if err := r.db.WithContext(ctx).Where("status = ? AND running = ? AND next_run_at IS NOT NULL AND next_run_at <= ?", string(schedule.StatusActive), false, now.UTC()).Order("next_run_at ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	claimed := make([]schedule.Task, 0, len(rows))
	for _, row := range rows {
		result := r.db.WithContext(ctx).Model(&scheduledTaskRow{}).Where("id = ? AND running = ? AND status = ?", row.ID, false, string(schedule.StatusActive)).Update("running", true)
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected == 1 {
			row.Running = true
			claimed = append(claimed, taskFromRow(row))
		}
	}
	return claimed, nil
}

func (r *scheduleRepository) UpdateRun(ctx context.Context, id string, running bool, invocationID, lastError string, lastRunAt, nextRunAt *time.Time, status schedule.Status) error {
	updates := map[string]any{
		"running": running, "last_invocation_id": strings.TrimSpace(invocationID), "last_error": strings.TrimSpace(lastError),
		"last_run_at": lastRunAt, "next_run_at": nextRunAt, "status": string(status), "updated_at": time.Now().UTC(),
	}
	result := r.db.WithContext(ctx).Model(&scheduledTaskRow{}).Where("id = ?", strings.TrimSpace(id)).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return schedule.ErrNotFound
	}
	return nil
}

func (r *scheduleRepository) SetStatus(ctx context.Context, id string, status schedule.Status) error {
	result := r.db.WithContext(ctx).Model(&scheduledTaskRow{}).Where("id = ?", strings.TrimSpace(id)).Updates(map[string]any{"status": string(status), "running": false, "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return schedule.ErrNotFound
	}
	return nil
}

func taskFromRow(row scheduledTaskRow) schedule.Task {
	return schedule.Task{
		ID: row.ID, Name: row.Name, Request: row.Request, Mode: schedule.Mode(row.Mode), StartAt: row.StartAt,
		IntervalSeconds: row.IntervalSeconds, Weekday: row.Weekday, MonthDay: row.MonthDay, TimeOfDay: row.TimeOfDay,
		Cron: row.Cron, UserID: row.UserID, ConversationID: row.ConversationID, AdapterID: row.AdapterID, ChatID: row.ChatID,
		Status: schedule.Status(row.Status), NextRunAt: row.NextRunAt, LastRunAt: row.LastRunAt, LastInvocationID: row.LastInvocationID,
		LastError: row.LastError, Running: row.Running, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}
