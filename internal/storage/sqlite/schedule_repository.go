package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"Abot/internal/schedule"
	"gorm.io/gorm"
)

// scheduledTaskRow 保存未来任务的周期参数和最近一次运行结果。
type scheduledTaskRow struct {
	ID                string `gorm:"primaryKey;size:100"`
	Name              string `gorm:"size:200;not null"`
	Request           string `gorm:"type:text;not null"`
	SourceUMO         string `gorm:"index;size:500"`
	OriginTurnID      string `gorm:"index;size:220"`
	Kind              string `gorm:"size:48"`
	Goal              string `gorm:"type:text"`
	Recurrence        string `gorm:"size:300"`
	ConfigSnapshot    string `gorm:"type:text"`
	DeliveryPolicy    string `gorm:"type:text"`
	RetryPolicy       string `gorm:"type:text"`
	ResultRef         string `gorm:"size:500"`
	Mode              string `gorm:"size:32;not null"`
	StartAt           *time.Time
	IntervalSeconds   int
	Weekday           int
	MonthDay          int
	TimeOfDay         string     `gorm:"size:5"`
	Cron              string     `gorm:"size:200"`
	UserID            string     `gorm:"index;size:200;not null"`
	ConversationID    string     `gorm:"index;size:100"`
	AdapterID         string     `gorm:"size:100"`
	ChatID            string     `gorm:"size:300"`
	Status            string     `gorm:"index;size:32;not null"`
	NextRunAt         *time.Time `gorm:"index"`
	LastRunAt         *time.Time
	LastInvocationID  string `gorm:"size:100"`
	LastError         string `gorm:"type:text"`
	RetryCount        int
	MaxRetries        int
	RetryDelaySeconds int
	MaxDelaySeconds   int
	QuietHoursStart   string `gorm:"size:5"`
	QuietHoursEnd     string `gorm:"size:5"`
	Timezone          string `gorm:"size:80"`
	Running           bool   `gorm:"index"`
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func (scheduledTaskRow) TableName() string { return "abot_bot_follow_ups" }

type scheduleRepository struct{ db *gorm.DB }

// migrateLegacyScheduledTasks 将旧未来任务表迁移到 Bot Runtime 的 Follow-up 表，
// 仅复制可验证的公共字段，新增的策略字段由运行时按默认值补齐。
func migrateLegacyScheduledTasks(db *gorm.DB) error {
	if db == nil || !db.Migrator().HasTable("abot_scheduled_tasks") {
		return nil
	}
	if err := db.Exec(`
		INSERT OR IGNORE INTO abot_bot_follow_ups
		(id, name, request, mode, start_at, interval_seconds, weekday, month_day, time_of_day, cron,
		 user_id, conversation_id, adapter_id, chat_id, status, next_run_at, last_run_at, last_invocation_id,
		 last_error, running, created_at, updated_at, kind, goal)
		SELECT id, name, request, mode, start_at, interval_seconds, weekday, month_day, time_of_day, cron,
		 user_id, conversation_id, adapter_id, chat_id, status, next_run_at, last_run_at, last_invocation_id,
		 last_error, running, created_at, updated_at, 'scheduled', request
		FROM abot_scheduled_tasks`).Error; err != nil {
		return fmt.Errorf("迁移旧未来任务到 Follow-up 失败: %w", err)
	}
	// 破坏性运行时重构不保留双轨表，避免后续管理台同时看到两份任务。
	if err := db.Exec("UPDATE abot_bot_follow_ups SET status = ? WHERE status = ?", string(schedule.StatusScheduled), "active").Error; err != nil {
		return fmt.Errorf("迁移 Follow-up 生命周期状态失败: %w", err)
	}
	return db.Migrator().DropTable("abot_scheduled_tasks")
}

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

// CountActive 使用数据库聚合完成 Bot 级 Follow-up 上限检查，不把历史任务
// 全部加载到 Go 内存；任务状态由持久化层统一判定为未终态集合。
func (r *scheduleRepository) CountActive(ctx context.Context, botID string) (int, error) {
	var count int64
	query := r.db.WithContext(ctx).Model(&scheduledTaskRow{}).
		Where("status IN ?", []string{
			string(schedule.StatusPending), string(schedule.StatusScheduled),
			string(schedule.StatusRunning), string(schedule.StatusWaiting),
		})
	if strings.TrimSpace(botID) != "" {
		query = query.Where("adapter_id = ?", strings.TrimSpace(botID))
	}
	if err := query.Count(&count).Error; err != nil {
		return 0, err
	}
	return int(count), nil
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
		ID: item.ID, Name: item.Name, Request: item.Request, SourceUMO: item.SourceUMO, OriginTurnID: item.OriginTurnID, Kind: item.Kind, Goal: item.Goal,
		Recurrence: item.Recurrence, ConfigSnapshot: item.ConfigSnapshot, DeliveryPolicy: item.DeliveryPolicy, RetryPolicy: item.RetryPolicy, ResultRef: item.ResultRef,
		Mode: string(item.Mode), StartAt: item.StartAt,
		IntervalSeconds: item.IntervalSeconds, Weekday: item.Weekday, MonthDay: item.MonthDay, TimeOfDay: item.TimeOfDay,
		Cron: item.Cron, UserID: item.UserID, ConversationID: item.ConversationID, AdapterID: item.AdapterID, ChatID: item.ChatID,
		Status: string(item.Status), NextRunAt: item.NextRunAt, LastRunAt: item.LastRunAt, LastInvocationID: item.LastInvocationID,
		LastError: item.LastError, RetryCount: item.RetryCount, MaxRetries: item.MaxRetries, RetryDelaySeconds: item.RetryDelaySeconds, MaxDelaySeconds: item.MaxDelaySeconds,
		QuietHoursStart: item.QuietHoursStart, QuietHoursEnd: item.QuietHoursEnd, Timezone: item.Timezone, Running: item.Running, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
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
	// 运行进程崩溃后恢复超过租约的 Follow-up；执行中的长任务会由调度器心跳
	// 持续刷新 updated_at，不会因为模型运行时间长而被重复领取。
	leaseCutoff := now.UTC().Add(-2 * time.Minute)
	if err := r.db.WithContext(ctx).Model(&scheduledTaskRow{}).Where("status = ? AND running = ? AND updated_at < ?", string(schedule.StatusRunning), true, leaseCutoff).Updates(map[string]any{"running": false, "status": string(schedule.StatusScheduled), "next_run_at": now.UTC(), "last_error": "运行租约已过期，任务已恢复排队", "updated_at": now.UTC()}).Error; err != nil {
		return nil, err
	}
	var rows []scheduledTaskRow
	if err := r.db.WithContext(ctx).Where("status IN ? AND running = ? AND next_run_at IS NOT NULL AND next_run_at <= ?", []string{string(schedule.StatusScheduled), string(schedule.StatusPending)}, false, now.UTC()).Order("next_run_at ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	claimed := make([]schedule.Task, 0, len(rows))
	for _, row := range rows {
		result := r.db.WithContext(ctx).Model(&scheduledTaskRow{}).Where("id = ? AND running = ? AND status IN ?", row.ID, false, []string{string(schedule.StatusScheduled), string(schedule.StatusPending)}).Updates(map[string]any{"running": true, "status": string(schedule.StatusRunning)})
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

func (r *scheduleRepository) UpdateRun(ctx context.Context, id string, running bool, invocationID, lastError string, lastRunAt, nextRunAt *time.Time, retryCount int, status schedule.Status) error {
	updates := map[string]any{
		"running": running, "last_invocation_id": strings.TrimSpace(invocationID), "last_error": strings.TrimSpace(lastError),
		"last_run_at": lastRunAt, "next_run_at": nextRunAt, "retry_count": retryCount, "status": string(status), "updated_at": time.Now().UTC(),
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
		ID: row.ID, Name: row.Name, Request: row.Request, SourceUMO: row.SourceUMO, OriginTurnID: row.OriginTurnID, Kind: row.Kind, Goal: row.Goal,
		Recurrence: row.Recurrence, ConfigSnapshot: row.ConfigSnapshot, DeliveryPolicy: row.DeliveryPolicy, RetryPolicy: row.RetryPolicy, ResultRef: row.ResultRef,
		Mode: schedule.Mode(row.Mode), StartAt: row.StartAt,
		IntervalSeconds: row.IntervalSeconds, Weekday: row.Weekday, MonthDay: row.MonthDay, TimeOfDay: row.TimeOfDay,
		Cron: row.Cron, UserID: row.UserID, ConversationID: row.ConversationID, AdapterID: row.AdapterID, ChatID: row.ChatID,
		Status: schedule.Status(row.Status), NextRunAt: row.NextRunAt, LastRunAt: row.LastRunAt, LastInvocationID: row.LastInvocationID,
		LastError: row.LastError, RetryCount: row.RetryCount, MaxRetries: row.MaxRetries, RetryDelaySeconds: row.RetryDelaySeconds, MaxDelaySeconds: row.MaxDelaySeconds,
		QuietHoursStart: row.QuietHoursStart, QuietHoursEnd: row.QuietHoursEnd, Timezone: row.Timezone, Running: row.Running, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}
