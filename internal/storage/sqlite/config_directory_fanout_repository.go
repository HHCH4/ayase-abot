package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	agentruntime "Abot/internal/agent/runtime"
	"gorm.io/gorm"
)

type runtimeConfigDirectoryFanoutRow struct {
	ID             string `gorm:"primaryKey;size:240"`
	Source         string `gorm:"index;size:160;not null"`
	Kind           string `gorm:"index;size:16;not null"`
	EntryID        string `gorm:"index;size:128;not null"`
	EntryRevision  int    `gorm:"not null"`
	PreviousDigest string `gorm:"size:71"`
	BodyDigest     string `gorm:"size:71;not null"`
	CorrelationID  string `gorm:"size:512"`
	IdempotencyKey string `gorm:"size:200"`
	Destinations   string `gorm:"type:text;not null"`
	OutboxIDs      string `gorm:"type:text;not null"`
	Status         string `gorm:"index;size:32;not null"`
	PendingCount   int    `gorm:"not null"`
	CompletedCount int    `gorm:"not null"`
	FailedCount    int    `gorm:"not null"`
	Revision       int64  `gorm:"not null"`
	LastError      string `gorm:"type:text"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (runtimeConfigDirectoryFanoutRow) TableName() string {
	return "abot_runtime_config_directory_fanouts"
}

func runtimeConfigDirectoryFanoutFromRow(row runtimeConfigDirectoryFanoutRow) (agentruntime.RuntimeConfigDirectoryFanoutPlan, error) {
	var destinations, outboxIDs []string
	if err := json.Unmarshal([]byte(row.Destinations), &destinations); err != nil {
		return agentruntime.RuntimeConfigDirectoryFanoutPlan{}, fmt.Errorf("解析 Runtime 配置目录 fanout destinations 失败: %w", err)
	}
	if err := json.Unmarshal([]byte(row.OutboxIDs), &outboxIDs); err != nil {
		return agentruntime.RuntimeConfigDirectoryFanoutPlan{}, fmt.Errorf("解析 Runtime 配置目录 fanout outbox IDs 失败: %w", err)
	}
	plan := agentruntime.RuntimeConfigDirectoryFanoutPlan{
		ID: row.ID, Source: row.Source, Kind: agentruntime.RuntimeConfigDirectoryKind(row.Kind), EntryID: row.EntryID,
		EntryRevision: row.EntryRevision, PreviousDigest: row.PreviousDigest, BodyDigest: row.BodyDigest,
		CorrelationID: row.CorrelationID, IdempotencyKey: row.IdempotencyKey, Destinations: destinations, OutboxIDs: outboxIDs,
		Status: agentruntime.RuntimeConfigDirectoryFanoutStatus(row.Status), PendingCount: row.PendingCount, CompletedCount: row.CompletedCount,
		FailedCount: row.FailedCount, Revision: row.Revision, LastError: row.LastError, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
	return plan.Normalize(time.Now().UTC())
}

func runtimeConfigDirectoryFanoutToRow(plan agentruntime.RuntimeConfigDirectoryFanoutPlan) (*runtimeConfigDirectoryFanoutRow, error) {
	normalized, err := plan.Normalize(time.Now().UTC())
	if err != nil {
		return nil, err
	}
	destinations, err := json.Marshal(normalized.Destinations)
	if err != nil {
		return nil, fmt.Errorf("编码 Runtime 配置目录 fanout destinations 失败: %w", err)
	}
	outboxIDs, err := json.Marshal(normalized.OutboxIDs)
	if err != nil {
		return nil, fmt.Errorf("编码 Runtime 配置目录 fanout outbox IDs 失败: %w", err)
	}
	return &runtimeConfigDirectoryFanoutRow{
		ID: normalized.ID, Source: normalized.Source, Kind: string(normalized.Kind), EntryID: normalized.EntryID, EntryRevision: normalized.EntryRevision,
		PreviousDigest: normalized.PreviousDigest, BodyDigest: normalized.BodyDigest, CorrelationID: normalized.CorrelationID, IdempotencyKey: normalized.IdempotencyKey,
		Destinations: string(destinations), OutboxIDs: string(outboxIDs), Status: string(normalized.Status), PendingCount: normalized.PendingCount,
		CompletedCount: normalized.CompletedCount, FailedCount: normalized.FailedCount, Revision: normalized.Revision, LastError: normalized.LastError,
		CreatedAt: normalized.CreatedAt, UpdatedAt: normalized.UpdatedAt,
	}, nil
}

func validateSQLiteRuntimeConfigDirectoryFanoutChildren(plan agentruntime.RuntimeConfigDirectoryFanoutPlan, children []agentruntime.RuntimeConfigDirectoryOutbox, now time.Time) (map[string]agentruntime.RuntimeConfigDirectoryOutbox, error) {
	if len(children) != len(plan.Destinations) {
		return nil, agentruntime.ErrConflict
	}
	byID := make(map[string]agentruntime.RuntimeConfigDirectoryOutbox, len(children))
	for _, child := range children {
		normalized, err := child.Normalize(now)
		if err != nil {
			return nil, err
		}
		if normalized.FanoutID != plan.ID || normalized.Status != agentruntime.RuntimeConfigDirectoryOutboxQueued || normalized.Attempt != 0 || normalized.Source != plan.Source || normalized.Kind != plan.Kind || normalized.EntryID != plan.EntryID || normalized.EntryRevision != plan.EntryRevision || normalized.PreviousDigest != plan.PreviousDigest || normalized.BodyDigest != plan.BodyDigest || normalized.CorrelationID != plan.CorrelationID || normalized.IdempotencyKey != plan.IdempotencyKey {
			return nil, agentruntime.ErrConflict
		}
		if _, exists := byID[normalized.ID]; exists {
			return nil, agentruntime.ErrConflict
		}
		byID[normalized.ID] = normalized
	}
	for index, destination := range plan.Destinations {
		child, ok := byID[plan.OutboxIDs[index]]
		if !ok || child.Destination != destination {
			return nil, agentruntime.ErrConflict
		}
	}
	return byID, nil
}

func (r *runtimeRepository) EnqueueRuntimeConfigDirectoryFanout(ctx context.Context, plan agentruntime.RuntimeConfigDirectoryFanoutPlan, children []agentruntime.RuntimeConfigDirectoryOutbox) (agentruntime.RuntimeConfigDirectoryFanoutPlan, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now().UTC()
	normalizedPlan, err := plan.Normalize(now)
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryFanoutPlan{}, err
	}
	childByID, err := validateSQLiteRuntimeConfigDirectoryFanoutChildren(normalizedPlan, children, now)
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryFanoutPlan{}, err
	}
	var saved agentruntime.RuntimeConfigDirectoryFanoutPlan
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var existing runtimeConfigDirectoryFanoutRow
		findErr := tx.Where("id = ?", normalizedPlan.ID).First(&existing).Error
		if findErr == nil {
			stored, decodeErr := runtimeConfigDirectoryFanoutFromRow(existing)
			if decodeErr != nil {
				return decodeErr
			}
			if !stored.Matches(normalizedPlan) {
				return agentruntime.ErrConflict
			}
			saved = stored
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		for _, child := range childByID {
			var existingChild runtimeConfigDirectoryOutboxRow
			childErr := tx.Where("id = ?", child.ID).First(&existingChild).Error
			if childErr == nil {
				stored, decodeErr := runtimeConfigDirectoryOutboxFromRow(existingChild)
				if decodeErr != nil {
					return decodeErr
				}
				if stored.FanoutID != "" && stored.FanoutID != normalizedPlan.ID {
					return agentruntime.ErrConflict
				}
				if !stored.Matches(child) {
					return agentruntime.ErrConflict
				}
				if stored.FanoutID == "" {
					if err := tx.Model(&runtimeConfigDirectoryOutboxRow{}).Where("id = ?", stored.ID).Update("fanout_id", normalizedPlan.ID).Error; err != nil {
						return err
					}
				}
				continue
			}
			if !errors.Is(childErr, gorm.ErrRecordNotFound) {
				return childErr
			}
			var sameDelivery runtimeConfigDirectoryOutboxRow
			identityErr := tx.Where("delivery_id = ?", child.DeliveryID).First(&sameDelivery).Error
			if identityErr == nil {
				stored, decodeErr := runtimeConfigDirectoryOutboxFromRow(sameDelivery)
				if decodeErr != nil {
					return decodeErr
				}
				if stored.FanoutID != "" && stored.FanoutID != normalizedPlan.ID {
					return agentruntime.ErrConflict
				}
				if !stored.Matches(child) {
					return agentruntime.ErrConflict
				}
				if stored.FanoutID == "" {
					if err := tx.Model(&runtimeConfigDirectoryOutboxRow{}).Where("id = ?", stored.ID).Update("fanout_id", normalizedPlan.ID).Error; err != nil {
						return err
					}
				}
				continue
			}
			if !errors.Is(identityErr, gorm.ErrRecordNotFound) {
				return identityErr
			}
			row, rowErr := runtimeConfigDirectoryOutboxToRow(child)
			if rowErr != nil {
				return rowErr
			}
			if err := tx.Create(row).Error; err != nil {
				return err
			}
		}
		row, rowErr := runtimeConfigDirectoryFanoutToRow(normalizedPlan)
		if rowErr != nil {
			return rowErr
		}
		if err := tx.Create(row).Error; err != nil {
			return err
		}
		saved = normalizedPlan
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryFanoutPlan{}, err
	}
	return saved, nil
}

func (r *runtimeRepository) GetRuntimeConfigDirectoryFanout(ctx context.Context, id string) (agentruntime.RuntimeConfigDirectoryFanoutPlan, error) {
	var row runtimeConfigDirectoryFanoutRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeConfigDirectoryFanoutPlan{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeConfigDirectoryFanoutPlan{}, err
	}
	return runtimeConfigDirectoryFanoutFromRow(row)
}

func (r *runtimeRepository) ListRuntimeConfigDirectoryFanouts(ctx context.Context, source string, status agentruntime.RuntimeConfigDirectoryFanoutStatus, limit int) ([]agentruntime.RuntimeConfigDirectoryFanoutPlan, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	query := r.db.WithContext(ctx).Model(&runtimeConfigDirectoryFanoutRow{})
	if source = strings.TrimSpace(source); source != "" {
		query = query.Where("source = ?", source)
	}
	if status != "" {
		query = query.Where("status = ?", string(status))
	}
	var rows []runtimeConfigDirectoryFanoutRow
	if err := query.Order("created_at ASC").Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]agentruntime.RuntimeConfigDirectoryFanoutPlan, 0, len(rows))
	for _, row := range rows {
		item, err := runtimeConfigDirectoryFanoutFromRow(row)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (r *runtimeRepository) ReconcileRuntimeConfigDirectoryFanout(ctx context.Context, id string, now time.Time) (agentruntime.RuntimeConfigDirectoryFanoutPlan, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var saved agentruntime.RuntimeConfigDirectoryFanoutPlan
	err := withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var row runtimeConfigDirectoryFanoutRow
		if err := tx.Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return err
		}
		plan, err := runtimeConfigDirectoryFanoutFromRow(row)
		if err != nil {
			return err
		}
		var childRows []runtimeConfigDirectoryOutboxRow
		if err := tx.Where("id IN ?", plan.OutboxIDs).Find(&childRows).Error; err != nil {
			return err
		}
		children := make([]agentruntime.RuntimeConfigDirectoryOutbox, 0, len(childRows))
		for _, childRow := range childRows {
			child, childErr := runtimeConfigDirectoryOutboxFromRow(childRow)
			if childErr != nil {
				return childErr
			}
			children = append(children, child)
		}
		reconciled, summaryErr := plan.ReconcileSummary(children, now)
		if summaryErr != nil {
			return summaryErr
		}
		updated, rowErr := runtimeConfigDirectoryFanoutToRow(reconciled)
		if rowErr != nil {
			return rowErr
		}
		result := tx.Model(&runtimeConfigDirectoryFanoutRow{}).Where("id = ? AND revision = ?", row.ID, row.Revision).Updates(map[string]any{
			"status": updated.Status, "pending_count": updated.PendingCount, "completed_count": updated.CompletedCount, "failed_count": updated.FailedCount,
			"last_error": updated.LastError, "revision": updated.Revision, "updated_at": updated.UpdatedAt,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return agentruntime.ErrConflict
		}
		saved = reconciled
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryFanoutPlan{}, err
	}
	return saved, nil
}

var _ agentruntime.RuntimeConfigDirectoryFanoutRepository = (*runtimeRepository)(nil)
