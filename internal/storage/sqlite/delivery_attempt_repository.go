package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	agentruntime "Abot/internal/agent/runtime"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// runtimeDeliveryAttemptRow is the source-side phase journal for all
// cross-runtime outboxes. It stores only bounded routing/identity metadata;
// payloads remain in their type-specific outbox and event/snapshot stores.
type runtimeDeliveryAttemptRow struct {
	ID             string     `gorm:"primaryKey;size:240"`
	Kind           string     `gorm:"index;size:40;not null"`
	Source         string     `gorm:"index;size:160;not null"`
	Destination    string     `gorm:"index;size:160;not null"`
	DeliveryID     string     `gorm:"index;size:512;not null"`
	InvocationID   string     `gorm:"index;size:512;not null"`
	OutboxID       string     `gorm:"index;size:512;not null"`
	OutboxRevision int64      `gorm:"not null"`
	Attempt        int        `gorm:"not null"`
	Phase          string     `gorm:"index;size:32;not null"`
	LeaseOwner     string     `gorm:"index;size:160"`
	LeaseExpiresAt *time.Time `gorm:"index"`
	LastError      string     `gorm:"type:text"`
	Revision       int64      `gorm:"not null"`
	CreatedAt      time.Time  `gorm:"index"`
	UpdatedAt      time.Time
	CompletedAt    *time.Time `gorm:"index"`
}

func (runtimeDeliveryAttemptRow) TableName() string {
	return "abot_agent_runtime_delivery_attempts"
}

func runtimeDeliveryAttemptFromRow(row runtimeDeliveryAttemptRow) (agentruntime.RuntimeDeliveryAttempt, error) {
	item := agentruntime.RuntimeDeliveryAttempt{
		ID: row.ID, Kind: agentruntime.RuntimeDeliveryKind(row.Kind), Source: row.Source, Destination: row.Destination,
		DeliveryID: row.DeliveryID, InvocationID: row.InvocationID, OutboxID: row.OutboxID,
		OutboxRevision: row.OutboxRevision, Attempt: row.Attempt, Phase: agentruntime.RuntimeDeliveryAttemptPhase(row.Phase),
		LeaseOwner: row.LeaseOwner, LeaseExpiresAt: cloneTimePtr(row.LeaseExpiresAt), LastError: row.LastError,
		Revision: row.Revision, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, CompletedAt: cloneTimePtr(row.CompletedAt),
	}
	validationNow := row.UpdatedAt
	if validationNow.IsZero() {
		validationNow = row.CreatedAt
	}
	if validationNow.IsZero() {
		validationNow = time.Now().UTC()
	}
	return item.Normalize(validationNow)
}

func runtimeDeliveryAttemptToRow(item agentruntime.RuntimeDeliveryAttempt) (*runtimeDeliveryAttemptRow, error) {
	normalized, err := item.Normalize(item.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &runtimeDeliveryAttemptRow{
		ID: normalized.ID, Kind: string(normalized.Kind), Source: normalized.Source, Destination: normalized.Destination,
		DeliveryID: normalized.DeliveryID, InvocationID: normalized.InvocationID, OutboxID: normalized.OutboxID,
		OutboxRevision: normalized.OutboxRevision, Attempt: normalized.Attempt, Phase: string(normalized.Phase),
		LeaseOwner: normalized.LeaseOwner, LeaseExpiresAt: cloneTimePtr(normalized.LeaseExpiresAt), LastError: normalized.LastError,
		Revision: normalized.Revision, CreatedAt: normalized.CreatedAt, UpdatedAt: normalized.UpdatedAt, CompletedAt: cloneTimePtr(normalized.CompletedAt),
	}, nil
}

func (r *runtimeRepository) BeginRuntimeDeliveryAttempt(ctx context.Context, candidate agentruntime.RuntimeDeliveryAttempt, now time.Time, ttl time.Duration) (agentruntime.RuntimeDeliveryAttempt, error) {
	normalized, err := candidate.Normalize(now)
	if err != nil {
		return agentruntime.RuntimeDeliveryAttempt{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var result agentruntime.RuntimeDeliveryAttempt
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row runtimeDeliveryAttemptRow
		findErr := tx.Where("id = ?", normalized.ID).First(&row).Error
		if errors.Is(findErr, gorm.ErrRecordNotFound) {
			newRow, rowErr := runtimeDeliveryAttemptToRow(normalized)
			if rowErr != nil {
				return rowErr
			}
			created := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(newRow)
			if created.Error != nil {
				return created.Error
			}
			if created.RowsAffected == 1 {
				result = normalized
				return nil
			}
			findErr = tx.Where("id = ?", normalized.ID).First(&row).Error
		}
		if findErr != nil {
			return findErr
		}
		existing, decodeErr := runtimeDeliveryAttemptFromRow(row)
		if decodeErr != nil {
			return decodeErr
		}
		merged, mergeErr := agentruntime.BeginRuntimeDeliveryAttempt(&existing, normalized, now, ttl)
		if mergeErr != nil {
			return mergeErr
		}
		if merged.Revision == existing.Revision && merged.Phase == existing.Phase && merged.Attempt == existing.Attempt && merged.OutboxRevision == existing.OutboxRevision && merged.LeaseOwner == existing.LeaseOwner {
			result = merged
			return nil
		}
		updatedRow, rowErr := runtimeDeliveryAttemptToRow(merged)
		if rowErr != nil {
			return rowErr
		}
		updates := map[string]any{
			"outbox_revision": updatedRow.OutboxRevision, "attempt": updatedRow.Attempt, "phase": updatedRow.Phase,
			"lease_owner": updatedRow.LeaseOwner, "lease_expires_at": updatedRow.LeaseExpiresAt,
			"last_error": updatedRow.LastError, "revision": updatedRow.Revision, "updated_at": updatedRow.UpdatedAt,
			"completed_at": updatedRow.CompletedAt,
		}
		updated := tx.Model(&runtimeDeliveryAttemptRow{}).Where("id = ? AND revision = ?", existing.ID, existing.Revision).Updates(updates)
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return agentruntime.ErrConflict
		}
		result = merged
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeDeliveryAttempt{}, err
	}
	return result, nil
}

func (r *runtimeRepository) AdvanceRuntimeDeliveryAttempt(ctx context.Context, id, owner string, expectedRevision int64, phase agentruntime.RuntimeDeliveryAttemptPhase, now time.Time, message string) (agentruntime.RuntimeDeliveryAttempt, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return agentruntime.RuntimeDeliveryAttempt{}, false, agentruntime.ErrInvalidRuntimeDeliveryAttempt
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var result agentruntime.RuntimeDeliveryAttempt
	duplicate := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row runtimeDeliveryAttemptRow
		if findErr := tx.Where("id = ?", id).First(&row).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		existing, decodeErr := runtimeDeliveryAttemptFromRow(row)
		if decodeErr != nil {
			return decodeErr
		}
		updated, same, advanceErr := agentruntime.AdvanceRuntimeDeliveryAttempt(existing, owner, expectedRevision, phase, now, message)
		if advanceErr != nil {
			return advanceErr
		}
		result = updated
		duplicate = same
		if same {
			return nil
		}
		updatedRow, rowErr := runtimeDeliveryAttemptToRow(updated)
		if rowErr != nil {
			return rowErr
		}
		updates := map[string]any{
			"phase": updatedRow.Phase, "lease_owner": updatedRow.LeaseOwner, "lease_expires_at": updatedRow.LeaseExpiresAt,
			"last_error": updatedRow.LastError, "revision": updatedRow.Revision, "updated_at": updatedRow.UpdatedAt,
			"completed_at": updatedRow.CompletedAt,
		}
		committed := tx.Model(&runtimeDeliveryAttemptRow{}).Where("id = ? AND revision = ?", existing.ID, existing.Revision).Updates(updates)
		if committed.Error != nil {
			return committed.Error
		}
		if committed.RowsAffected != 1 {
			return agentruntime.ErrConflict
		}
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeDeliveryAttempt{}, false, err
	}
	return result, duplicate, nil
}

func (r *runtimeRepository) GetRuntimeDeliveryAttempt(ctx context.Context, id string) (agentruntime.RuntimeDeliveryAttempt, error) {
	var row runtimeDeliveryAttemptRow
	err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return agentruntime.RuntimeDeliveryAttempt{}, agentruntime.ErrNotFound
	}
	if err != nil {
		return agentruntime.RuntimeDeliveryAttempt{}, err
	}
	return runtimeDeliveryAttemptFromRow(row)
}

func (r *runtimeRepository) ListRuntimeDeliveryAttempts(ctx context.Context, invocationID string, kind agentruntime.RuntimeDeliveryKind, limit int) ([]agentruntime.RuntimeDeliveryAttempt, error) {
	invocationID = strings.TrimSpace(invocationID)
	if invocationID != "" {
		var invocation invocationRow
		if err := r.db.WithContext(ctx).Where("id = ?", invocationID).First(&invocation).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, agentruntime.ErrNotFound
		} else if err != nil {
			return nil, err
		}
	}
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	query := r.db.WithContext(ctx).Model(&runtimeDeliveryAttemptRow{})
	if invocationID != "" {
		query = query.Where("invocation_id = ?", invocationID)
	}
	if kind != "" {
		query = query.Where("kind = ?", string(kind))
	}
	var rows []runtimeDeliveryAttemptRow
	if err := query.Order("created_at ASC").Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]agentruntime.RuntimeDeliveryAttempt, 0, len(rows))
	for _, row := range rows {
		item, err := runtimeDeliveryAttemptFromRow(row)
		if err != nil {
			return nil, fmt.Errorf("解析 delivery attempt %s 失败: %w", row.ID, err)
		}
		items = append(items, item)
	}
	return items, nil
}

var _ agentruntime.RuntimeDeliveryAttemptRepository = (*runtimeRepository)(nil)
