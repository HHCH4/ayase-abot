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
	"gorm.io/gorm/clause"
)

// runtimeConfigDirectoryRebindPlanRow stores only the immutable invocation
// boundary, route set and capability snapshot. Invocation text, attachments,
// tool arguments and credentials remain in their own protected stores.
type runtimeConfigDirectoryRebindPlanRow struct {
	ID                   string `gorm:"primaryKey;size:240"`
	Source               string `gorm:"index;size:160;not null"`
	InvocationID         string `gorm:"index;size:512;not null"`
	BoundaryStatus       string `gorm:"index;size:32;not null"`
	ConfigSnapshotDigest string `gorm:"size:71;not null"`
	ConfigFanoutID       string `gorm:"index;size:240"`
	Destinations         string `gorm:"type:text;not null"`
	Targets              string `gorm:"type:text;not null"`
	CapabilitiesDigest   string `gorm:"size:71;not null"`
	IdempotencyKey       string `gorm:"index;size:200"`
	Status               string `gorm:"index;size:32;not null"`
	Revision             int64  `gorm:"not null"`
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

func (runtimeConfigDirectoryRebindPlanRow) TableName() string {
	return "abot_runtime_config_directory_rebind_plans"
}

func runtimeConfigDirectoryRebindPlanFromRow(row runtimeConfigDirectoryRebindPlanRow) (agentruntime.RuntimeConfigDirectoryRebindPlan, error) {
	var destinations []string
	if err := json.Unmarshal([]byte(row.Destinations), &destinations); err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindPlan{}, fmt.Errorf("解析 Runtime 配置目录 rebind destinations 失败: %w", err)
	}
	var targets []agentruntime.RuntimeConfigDirectoryRebindTarget
	if err := json.Unmarshal([]byte(row.Targets), &targets); err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindPlan{}, fmt.Errorf("解析 Runtime 配置目录 rebind targets 失败: %w", err)
	}
	plan := agentruntime.RuntimeConfigDirectoryRebindPlan{
		ID: row.ID, Source: row.Source, InvocationID: row.InvocationID, BoundaryStatus: agentruntime.InvocationStatus(row.BoundaryStatus),
		ConfigSnapshotDigest: row.ConfigSnapshotDigest, ConfigFanoutID: row.ConfigFanoutID, Destinations: destinations, Targets: targets,
		CapabilitiesDigest: row.CapabilitiesDigest, Status: agentruntime.RuntimeConfigDirectoryRebindStatus(row.Status), Revision: row.Revision,
		IdempotencyKey: row.IdempotencyKey, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
	return plan.Normalize(time.Now().UTC())
}

func runtimeConfigDirectoryRebindPlanToRow(plan agentruntime.RuntimeConfigDirectoryRebindPlan) (*runtimeConfigDirectoryRebindPlanRow, error) {
	normalized, err := plan.Normalize(time.Now().UTC())
	if err != nil {
		return nil, err
	}
	destinations, err := json.Marshal(normalized.Destinations)
	if err != nil {
		return nil, fmt.Errorf("编码 Runtime 配置目录 rebind destinations 失败: %w", err)
	}
	targets, err := json.Marshal(normalized.Targets)
	if err != nil {
		return nil, fmt.Errorf("编码 Runtime 配置目录 rebind targets 失败: %w", err)
	}
	return &runtimeConfigDirectoryRebindPlanRow{
		ID: normalized.ID, Source: normalized.Source, InvocationID: normalized.InvocationID, BoundaryStatus: string(normalized.BoundaryStatus),
		ConfigSnapshotDigest: normalized.ConfigSnapshotDigest, ConfigFanoutID: normalized.ConfigFanoutID, Destinations: string(destinations), Targets: string(targets),
		CapabilitiesDigest: normalized.CapabilitiesDigest, IdempotencyKey: normalized.IdempotencyKey, Status: string(normalized.Status), Revision: normalized.Revision,
		CreatedAt: normalized.CreatedAt, UpdatedAt: normalized.UpdatedAt,
	}, nil
}

func (r *runtimeRepository) EnqueueRuntimeConfigDirectoryRebindPlan(ctx context.Context, plan agentruntime.RuntimeConfigDirectoryRebindPlan) (agentruntime.RuntimeConfigDirectoryRebindPlan, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	normalized, err := plan.Normalize(time.Now().UTC())
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindPlan{}, err
	}
	var saved agentruntime.RuntimeConfigDirectoryRebindPlan
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var existingRow runtimeConfigDirectoryRebindPlanRow
		findErr := tx.Where("id = ?", normalized.ID).First(&existingRow).Error
		if findErr == nil {
			existing, decodeErr := runtimeConfigDirectoryRebindPlanFromRow(existingRow)
			if decodeErr != nil {
				return decodeErr
			}
			if !existing.Matches(normalized) {
				return agentruntime.ErrRuntimeConfigDirectoryRebindConflict
			}
			saved = existing
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		if normalized.IdempotencyKey != "" {
			var idempotentRow runtimeConfigDirectoryRebindPlanRow
			idempotentErr := tx.Where("source = ? AND invocation_id = ? AND idempotency_key = ?", normalized.Source, normalized.InvocationID, normalized.IdempotencyKey).First(&idempotentRow).Error
			if idempotentErr == nil {
				existing, decodeErr := runtimeConfigDirectoryRebindPlanFromRow(idempotentRow)
				if decodeErr != nil {
					return decodeErr
				}
				if !existing.Matches(normalized) {
					return agentruntime.ErrRuntimeConfigDirectoryRebindConflict
				}
				saved = existing
				return nil
			}
			if !errors.Is(idempotentErr, gorm.ErrRecordNotFound) {
				return idempotentErr
			}
		}
		row, rowErr := runtimeConfigDirectoryRebindPlanToRow(normalized)
		if rowErr != nil {
			return rowErr
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 1 {
			saved = normalized
			return nil
		}
		// A concurrent writer won the same stable ID. Read it back and apply
		// the same immutable comparison as the fast path above.
		if err := tx.Where("id = ?", normalized.ID).First(&existingRow).Error; err != nil {
			return err
		}
		existing, decodeErr := runtimeConfigDirectoryRebindPlanFromRow(existingRow)
		if decodeErr != nil {
			return decodeErr
		}
		if !existing.Matches(normalized) {
			return agentruntime.ErrRuntimeConfigDirectoryRebindConflict
		}
		saved = existing
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindPlan{}, err
	}
	return saved, nil
}

func (r *runtimeRepository) GetRuntimeConfigDirectoryRebindPlan(ctx context.Context, id string) (agentruntime.RuntimeConfigDirectoryRebindPlan, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var row runtimeConfigDirectoryRebindPlanRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeConfigDirectoryRebindPlan{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeConfigDirectoryRebindPlan{}, err
	}
	return runtimeConfigDirectoryRebindPlanFromRow(row)
}

func (r *runtimeRepository) ListRuntimeConfigDirectoryRebindPlans(ctx context.Context, source, invocationID string, status agentruntime.RuntimeConfigDirectoryRebindStatus, limit int) ([]agentruntime.RuntimeConfigDirectoryRebindPlan, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if status != "" && !status.ValidForHTTP() {
		return nil, agentruntime.ErrInvalidRuntimeConfigDirectoryRebind
	}
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	query := r.db.WithContext(ctx).Model(&runtimeConfigDirectoryRebindPlanRow{})
	if source = strings.TrimSpace(source); source != "" {
		query = query.Where("source = ?", source)
	}
	if invocationID = strings.TrimSpace(invocationID); invocationID != "" {
		query = query.Where("invocation_id = ?", invocationID)
	}
	if status != "" {
		query = query.Where("status = ?", string(status))
	}
	var rows []runtimeConfigDirectoryRebindPlanRow
	if err := query.Order("created_at ASC").Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]agentruntime.RuntimeConfigDirectoryRebindPlan, 0, len(rows))
	for _, row := range rows {
		item, err := runtimeConfigDirectoryRebindPlanFromRow(row)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

var _ agentruntime.RuntimeConfigDirectoryRebindPlanRepository = (*runtimeRepository)(nil)
