package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"Abot/internal/websearch"
	"gorm.io/gorm"
)

type webSearchServiceRow struct {
	ID           string `gorm:"primaryKey;size:64"`
	Name         string `gorm:"size:100;not null"`
	Provider     string `gorm:"size:32;index;not null"`
	AccountGroup string `gorm:"size:100;index;not null"`
	APIKey       string `gorm:"size:4000;not null"`
	Enabled      bool   `gorm:"index;not null"`
	Priority     int    `gorm:"index;not null"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (webSearchServiceRow) TableName() string { return "abot_web_search_services" }

type webSearchUsageRow struct {
	ID             string `gorm:"primaryKey;size:64"`
	InvocationID   string `gorm:"index;size:128"`
	ConversationID string `gorm:"index;size:128"`
	ServiceID      string `gorm:"index;size:64;not null"`
	ServiceName    string `gorm:"size:100;not null"`
	Provider       string `gorm:"size:32;index;not null"`
	AccountGroup   string `gorm:"size:100;index;not null"`
	Query          string `gorm:"size:1000;not null"`
	Status         string `gorm:"size:16;index;not null"`
	Credits        int64
	InputTokens    int64
	OutputTokens   int64
	UsageKnown     bool
	ResultCount    int
	HTTPStatus     int
	RequestID      string `gorm:"size:128"`
	Error          string `gorm:"size:800"`
	DurationMS     int64
	CreatedAt      time.Time `gorm:"index;not null"`
}

func (webSearchUsageRow) TableName() string { return "abot_web_search_usage" }

type webSearchRepository struct{ db *gorm.DB }

var _ websearch.Repository = (*webSearchRepository)(nil)

func (repository *webSearchRepository) ListServices(ctx context.Context) ([]websearch.Service, error) {
	var rows []webSearchServiceRow
	if err := repository.db.WithContext(ctx).Order("priority ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]websearch.Service, 0, len(rows))
	for _, row := range rows {
		items = append(items, webSearchServiceFromRow(row))
	}
	return items, nil
}

func (repository *webSearchRepository) GetService(ctx context.Context, id string) (websearch.Service, error) {
	var row webSearchServiceRow
	if err := repository.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return websearch.Service{}, websearch.ErrNotFound
		}
		return websearch.Service{}, err
	}
	return webSearchServiceFromRow(row), nil
}

func (repository *webSearchRepository) SaveService(ctx context.Context, item websearch.Service) error {
	return repository.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var previous webSearchServiceRow
		findErr := tx.Where("id = ?", item.ID).First(&previous).Error
		if findErr != nil && !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		row := webSearchServiceRow{
			ID: item.ID, Name: item.Name, Provider: item.Provider, AccountGroup: item.AccountGroup,
			APIKey: item.APIKey, Enabled: item.Enabled, Priority: item.Priority,
			CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		}
		if findErr == nil {
			row.CreatedAt = previous.CreatedAt
		}
		if row.CreatedAt.IsZero() {
			row.CreatedAt = time.Now().UTC()
		}
		if row.UpdatedAt.IsZero() {
			row.UpdatedAt = row.CreatedAt
		}
		return tx.Save(&row).Error
	})
}

func (repository *webSearchRepository) DeleteService(ctx context.Context, id string) error {
	result := repository.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).Delete(&webSearchServiceRow{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return websearch.ErrNotFound
	}
	// 删除服务配置时保留用量记录，以便继续核对历史消耗。
	return nil
}

func (repository *webSearchRepository) ReserveUsage(ctx context.Context, item websearch.UsageRecord, budget websearch.Budget) (int64, error) {
	if strings.TrimSpace(item.ID) == "" {
		return 0, fmt.Errorf("网页搜索用量记录 ID 不能为空")
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	}
	row := webSearchUsageFromDomain(item)
	var dailyCalls int64
	err := repository.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 先插入待处理记录以取得 SQLite 写锁，再检查次数上限，避免并发搜索绕过硬性额度。
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		dayStart := time.Date(item.CreatedAt.UTC().Year(), item.CreatedAt.UTC().Month(), item.CreatedAt.UTC().Day(), 0, 0, 0, 0, time.UTC)
		query := tx.Model(&webSearchUsageRow{}).Where("created_at >= ?", dayStart)
		if err := query.Count(&dailyCalls).Error; err != nil {
			return err
		}
		if budget.DailyCallLimit > 0 && dailyCalls > budget.DailyCallLimit {
			return websearch.ErrDailyLimit
		}
		if budget.MaxCallsPerInvocation > 0 && strings.TrimSpace(item.InvocationID) != "" {
			var invocationCalls int64
			if err := tx.Model(&webSearchUsageRow{}).Where("invocation_id = ?", item.InvocationID).Count(&invocationCalls).Error; err != nil {
				return err
			}
			if invocationCalls > int64(budget.MaxCallsPerInvocation) {
				return websearch.ErrTaskLimit
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return dailyCalls, nil
}

func (repository *webSearchRepository) CompleteUsage(ctx context.Context, id string, item websearch.UsageRecord) error {
	updates := map[string]any{
		"status": item.Status, "credits": item.Usage.Credits,
		"input_tokens": item.Usage.InputTokens, "output_tokens": item.Usage.OutputTokens,
		"usage_known": item.Usage.Known, "result_count": item.ResultCount,
		"http_status": item.HTTPStatus, "request_id": item.RequestID,
		"error": item.Error, "duration_ms": item.DurationMS,
	}
	result := repository.db.WithContext(ctx).Model(&webSearchUsageRow{}).Where("id = ?", strings.TrimSpace(id)).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("网页搜索用量记录不存在: %s", id)
	}
	return nil
}

func (repository *webSearchRepository) ListUsage(ctx context.Context, since time.Time, limit int) ([]websearch.UsageRecord, error) {
	if limit <= 0 || limit > 10000 {
		limit = 10000
	}
	var rows []webSearchUsageRow
	if err := repository.db.WithContext(ctx).Where("created_at >= ?", since.UTC()).Order("created_at DESC, id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]websearch.UsageRecord, 0, len(rows))
	for _, row := range rows {
		items = append(items, webSearchUsageToDomain(row))
	}
	return items, nil
}

func (repository *webSearchRepository) AggregateUsage(ctx context.Context, dayStart, periodStart time.Time) (websearch.UsageAggregate, error) {
	var result websearch.UsageAggregate
	if err := repository.db.WithContext(ctx).Model(&webSearchUsageRow{}).
		Where("created_at >= ?", dayStart.UTC()).Count(&result.DailyCalls).Error; err != nil {
		return websearch.UsageAggregate{}, err
	}
	var rows []struct {
		ServiceID    string `gorm:"column:service_id"`
		ServiceName  string `gorm:"column:service_name"`
		Provider     string `gorm:"column:provider"`
		AccountGroup string `gorm:"column:account_group"`
		Calls        int64  `gorm:"column:calls"`
		Successes    int64  `gorm:"column:successes"`
		Failures     int64  `gorm:"column:failures"`
		Credits      int64  `gorm:"column:credits"`
		InputTokens  int64  `gorm:"column:input_tokens"`
		OutputTokens int64  `gorm:"column:output_tokens"`
		UnknownUsage int64  `gorm:"column:unknown_usage"`
	}
	query := repository.db.WithContext(ctx).Model(&webSearchUsageRow{}).
		Select("service_id, service_name, provider, account_group, COUNT(*) AS calls, "+
			"SUM(CASE WHEN status = 'success' THEN 1 ELSE 0 END) AS successes, "+
			"SUM(CASE WHEN status = 'success' THEN 0 ELSE 1 END) AS failures, "+
			"SUM(credits) AS credits, SUM(input_tokens) AS input_tokens, "+
			"SUM(output_tokens) AS output_tokens, "+
			"SUM(CASE WHEN usage_known = false THEN 1 ELSE 0 END) AS unknown_usage").
		Where("created_at >= ?", periodStart.UTC()).
		Group("service_id, service_name, provider, account_group")
	if err := query.Scan(&rows).Error; err != nil {
		return websearch.UsageAggregate{}, err
	}
	result.ByService = make([]websearch.ServiceUsage, 0, len(rows))
	for _, row := range rows {
		result.ByService = append(result.ByService, websearch.ServiceUsage{
			ServiceID: row.ServiceID, ServiceName: row.ServiceName, Provider: row.Provider,
			AccountGroup: row.AccountGroup, Calls: row.Calls, Successes: row.Successes,
			Failures: row.Failures, Credits: row.Credits, InputTokens: row.InputTokens,
			OutputTokens: row.OutputTokens, UnknownUsage: row.UnknownUsage,
		})
	}
	return result, nil
}

func webSearchServiceFromRow(row webSearchServiceRow) websearch.Service {
	return websearch.Service{
		ID: row.ID, Name: row.Name, Provider: row.Provider, AccountGroup: row.AccountGroup,
		APIKey: row.APIKey, Enabled: row.Enabled, Priority: row.Priority,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func webSearchUsageFromDomain(item websearch.UsageRecord) webSearchUsageRow {
	return webSearchUsageRow{
		ID: item.ID, InvocationID: item.InvocationID, ConversationID: item.ConversationID,
		ServiceID: item.ServiceID, ServiceName: item.ServiceName, Provider: item.Provider,
		AccountGroup: item.AccountGroup, Query: item.Query, Status: item.Status,
		Credits: item.Usage.Credits, InputTokens: item.Usage.InputTokens,
		OutputTokens: item.Usage.OutputTokens, UsageKnown: item.Usage.Known,
		ResultCount: item.ResultCount, HTTPStatus: item.HTTPStatus, RequestID: item.RequestID,
		Error: item.Error, DurationMS: item.DurationMS, CreatedAt: item.CreatedAt.UTC(),
	}
}

func webSearchUsageToDomain(row webSearchUsageRow) websearch.UsageRecord {
	return websearch.UsageRecord{
		ID: row.ID, InvocationID: row.InvocationID, ConversationID: row.ConversationID,
		ServiceID: row.ServiceID, ServiceName: row.ServiceName, Provider: row.Provider,
		AccountGroup: row.AccountGroup, Query: row.Query, Status: row.Status,
		Usage:       websearch.Usage{Credits: row.Credits, InputTokens: row.InputTokens, OutputTokens: row.OutputTokens, Known: row.UsageKnown},
		ResultCount: row.ResultCount, HTTPStatus: row.HTTPStatus, RequestID: row.RequestID,
		Error: row.Error, DurationMS: row.DurationMS, CreatedAt: row.CreatedAt,
	}
}
