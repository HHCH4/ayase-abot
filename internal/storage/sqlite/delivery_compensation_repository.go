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

type runtimeDeliveryCompensationRow struct {
	ID             string     `gorm:"primaryKey;size:240"`
	GroupID        string     `gorm:"index;size:220"`
	Kind           string     `gorm:"index;size:40;not null"`
	Source         string     `gorm:"index;size:160;not null"`
	Destination    string     `gorm:"index;size:160;not null"`
	DeliveryID     string     `gorm:"index;size:512;not null"`
	InvocationID   string     `gorm:"index;size:512;not null"`
	OutboxID       string     `gorm:"index;size:512;not null"`
	SourceAttempt  int        `gorm:"not null"`
	Status         string     `gorm:"index;size:32;not null"`
	Attempt        int        `gorm:"not null"`
	Revision       int64      `gorm:"not null"`
	AvailableAt    time.Time  `gorm:"index"`
	LeaseOwner     string     `gorm:"index;size:160"`
	LeaseExpiresAt *time.Time `gorm:"index"`
	LastError      string     `gorm:"type:text"`
	CreatedAt      time.Time  `gorm:"index"`
	UpdatedAt      time.Time
	CompletedAt    *time.Time `gorm:"index"`
}

func (runtimeDeliveryCompensationRow) TableName() string {
	return "abot_agent_runtime_delivery_compensations"
}

func runtimeDeliveryCompensationFromRow(row runtimeDeliveryCompensationRow) (agentruntime.RuntimeDeliveryCompensation, error) {
	item := agentruntime.RuntimeDeliveryCompensation{
		ID: row.ID, GroupID: row.GroupID, Kind: agentruntime.RuntimeDeliveryKind(row.Kind), Source: row.Source, Destination: row.Destination,
		DeliveryID: row.DeliveryID, InvocationID: row.InvocationID, OutboxID: row.OutboxID, SourceAttempt: row.SourceAttempt,
		Status: agentruntime.RuntimeDeliveryCompensationStatus(row.Status), Attempt: row.Attempt, Revision: row.Revision,
		AvailableAt: row.AvailableAt, LeaseOwner: row.LeaseOwner, LeaseExpiresAt: cloneTimePtr(row.LeaseExpiresAt), LastError: row.LastError,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, CompletedAt: cloneTimePtr(row.CompletedAt),
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

func runtimeDeliveryCompensationToRow(item agentruntime.RuntimeDeliveryCompensation) (*runtimeDeliveryCompensationRow, error) {
	normalized, err := item.Normalize(item.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &runtimeDeliveryCompensationRow{
		ID: normalized.ID, GroupID: normalized.GroupID, Kind: string(normalized.Kind), Source: normalized.Source, Destination: normalized.Destination,
		DeliveryID: normalized.DeliveryID, InvocationID: normalized.InvocationID, OutboxID: normalized.OutboxID, SourceAttempt: normalized.SourceAttempt,
		Status: string(normalized.Status), Attempt: normalized.Attempt, Revision: normalized.Revision, AvailableAt: normalized.AvailableAt,
		LeaseOwner: normalized.LeaseOwner, LeaseExpiresAt: cloneTimePtr(normalized.LeaseExpiresAt), LastError: normalized.LastError,
		CreatedAt: normalized.CreatedAt, UpdatedAt: normalized.UpdatedAt, CompletedAt: cloneTimePtr(normalized.CompletedAt),
	}, nil
}

func (r *runtimeRepository) EnqueueRuntimeDeliveryCompensation(ctx context.Context, item agentruntime.RuntimeDeliveryCompensation) (agentruntime.RuntimeDeliveryCompensation, error) {
	now := time.Now().UTC()
	normalized, err := item.Normalize(now)
	if err != nil {
		return agentruntime.RuntimeDeliveryCompensation{}, err
	}
	row, err := runtimeDeliveryCompensationToRow(normalized)
	if err != nil {
		return agentruntime.RuntimeDeliveryCompensation{}, err
	}
	var saved agentruntime.RuntimeDeliveryCompensation
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		if result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row); result.Error != nil {
			return result.Error
		}
		var winner runtimeDeliveryCompensationRow
		if findErr := tx.Where("id = ?", normalized.ID).First(&winner).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		stored, decodeErr := runtimeDeliveryCompensationFromRow(winner)
		if decodeErr != nil {
			return decodeErr
		}
		if !stored.MatchesIdentity(normalized) {
			return agentruntime.ErrConflict
		}
		saved = stored
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeDeliveryCompensation{}, err
	}
	return saved, nil
}

func (r *runtimeRepository) GetRuntimeDeliveryCompensation(ctx context.Context, id string) (agentruntime.RuntimeDeliveryCompensation, error) {
	var row runtimeDeliveryCompensationRow
	err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return agentruntime.RuntimeDeliveryCompensation{}, agentruntime.ErrNotFound
	}
	if err != nil {
		return agentruntime.RuntimeDeliveryCompensation{}, err
	}
	return runtimeDeliveryCompensationFromRow(row)
}

func (r *runtimeRepository) ListRuntimeDeliveryCompensations(ctx context.Context, invocationID string, kind agentruntime.RuntimeDeliveryKind, status agentruntime.RuntimeDeliveryCompensationStatus, limit int) ([]agentruntime.RuntimeDeliveryCompensation, error) {
	invocationID = strings.TrimSpace(invocationID)
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	if kind != "" {
		switch kind {
		case agentruntime.RuntimeDeliveryKindEvent, agentruntime.RuntimeDeliveryKindCheckpoint, agentruntime.RuntimeDeliveryKindConfig, agentruntime.RuntimeDeliveryKindRejection:
		default:
			return nil, agentruntime.ErrInvalidRuntimeDeliveryCompensation
		}
	}
	if status != "" {
		switch status {
		case agentruntime.RuntimeDeliveryCompensationQueued, agentruntime.RuntimeDeliveryCompensationProcessing, agentruntime.RuntimeDeliveryCompensationCompleted, agentruntime.RuntimeDeliveryCompensationFailed:
		default:
			return nil, agentruntime.ErrInvalidRuntimeDeliveryCompensation
		}
	}
	if invocationID != "" {
		var invocation invocationRow
		if err := r.db.WithContext(ctx).Where("id = ?", invocationID).First(&invocation).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, agentruntime.ErrNotFound
		} else if err != nil {
			return nil, err
		}
	}
	query := r.db.WithContext(ctx).Model(&runtimeDeliveryCompensationRow{})
	if invocationID != "" {
		query = query.Where("invocation_id = ?", invocationID)
	}
	if kind != "" {
		query = query.Where("kind = ?", string(kind))
	}
	if status != "" {
		query = query.Where("status = ?", string(status))
	}
	var rows []runtimeDeliveryCompensationRow
	if err := query.Order("available_at ASC").Order("created_at ASC").Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]agentruntime.RuntimeDeliveryCompensation, 0, len(rows))
	for _, row := range rows {
		item, decodeErr := runtimeDeliveryCompensationFromRow(row)
		if decodeErr != nil {
			return nil, fmt.Errorf("解析 delivery compensation %s 失败: %w", row.ID, decodeErr)
		}
		items = append(items, item)
	}
	return items, nil
}

func (r *runtimeRepository) ClaimRuntimeDeliveryCompensation(ctx context.Context, owner string, now time.Time, ttl time.Duration) (agentruntime.RuntimeDeliveryCompensation, bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeDeliveryCompensationOwnerLength || ttl <= 0 || ttl > agentruntime.MaxRuntimeDeliveryCompensationLease {
		return agentruntime.RuntimeDeliveryCompensation{}, false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var claimed agentruntime.RuntimeDeliveryCompensation
	var won bool
	err := withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var rows []runtimeDeliveryCompensationRow
		if err := tx.Where("status IN (?, ?)", string(agentruntime.RuntimeDeliveryCompensationQueued), string(agentruntime.RuntimeDeliveryCompensationProcessing)).Order("available_at ASC").Order("created_at ASC").Order("id ASC").Limit(64).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			item, decodeErr := runtimeDeliveryCompensationFromRow(row)
			if decodeErr != nil {
				return decodeErr
			}
			if item.Status == agentruntime.RuntimeDeliveryCompensationQueued && item.AvailableAt.After(now) {
				continue
			}
			if item.Status == agentruntime.RuntimeDeliveryCompensationProcessing && item.LeaseExpiresAt != nil && item.LeaseExpiresAt.After(now) {
				continue
			}
			if item.Attempt >= agentruntime.MaxRuntimeDeliveryCompensationAttempts {
				result := tx.Model(&runtimeDeliveryCompensationRow{}).Where("id = ? AND revision = ? AND status IN (?, ?)", item.ID, item.Revision, string(agentruntime.RuntimeDeliveryCompensationQueued), string(agentruntime.RuntimeDeliveryCompensationProcessing)).Updates(map[string]any{
					"status": string(agentruntime.RuntimeDeliveryCompensationFailed), "lease_owner": "", "lease_expires_at": nil,
					"last_error": "Runtime delivery compensation 达到最大投递次数", "revision": item.Revision + 1, "updated_at": now,
				})
				if result.Error != nil {
					return result.Error
				}
				continue
			}
			expires := now.Add(ttl)
			where := tx.Model(&runtimeDeliveryCompensationRow{}).Where("id = ? AND revision = ?", item.ID, item.Revision)
			if item.Status == agentruntime.RuntimeDeliveryCompensationQueued {
				where = where.Where("status = ? AND available_at <= ?", string(agentruntime.RuntimeDeliveryCompensationQueued), now)
			} else {
				where = where.Where("status = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?)", string(agentruntime.RuntimeDeliveryCompensationProcessing), now)
			}
			result := where.Updates(map[string]any{
				"status": string(agentruntime.RuntimeDeliveryCompensationProcessing), "attempt": item.Attempt + 1, "revision": item.Revision + 1,
				"lease_owner": owner, "lease_expires_at": &expires, "completed_at": nil, "updated_at": now,
			})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				continue
			}
			row.Status = string(agentruntime.RuntimeDeliveryCompensationProcessing)
			row.Attempt = item.Attempt + 1
			row.Revision = item.Revision + 1
			row.LeaseOwner = owner
			row.LeaseExpiresAt = &expires
			row.UpdatedAt = now
			claimed, decodeErr = runtimeDeliveryCompensationFromRow(row)
			if decodeErr != nil {
				return decodeErr
			}
			won = true
			return nil
		}
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeDeliveryCompensation{}, false, err
	}
	return claimed, won, nil
}

func (r *runtimeRepository) CompleteRuntimeDeliveryCompensation(ctx context.Context, id, owner string, now time.Time) (bool, error) {
	return r.mutateRuntimeDeliveryCompensation(ctx, id, owner, now, func(item agentruntime.RuntimeDeliveryCompensation, owner string, now time.Time) (agentruntime.RuntimeDeliveryCompensation, bool, error) {
		return agentruntime.CompleteRuntimeDeliveryCompensation(item, owner, now)
	})
}

func (r *runtimeRepository) RetryRuntimeDeliveryCompensation(ctx context.Context, id, owner string, now time.Time, message string) (bool, error) {
	return r.mutateRuntimeDeliveryCompensation(ctx, id, owner, now, func(item agentruntime.RuntimeDeliveryCompensation, owner string, now time.Time) (agentruntime.RuntimeDeliveryCompensation, bool, error) {
		return agentruntime.RetryRuntimeDeliveryCompensation(item, owner, now, message)
	})
}

func (r *runtimeRepository) FailRuntimeDeliveryCompensation(ctx context.Context, id, owner string, now time.Time, message string) (bool, error) {
	return r.mutateRuntimeDeliveryCompensation(ctx, id, owner, now, func(item agentruntime.RuntimeDeliveryCompensation, owner string, now time.Time) (agentruntime.RuntimeDeliveryCompensation, bool, error) {
		return agentruntime.FailRuntimeDeliveryCompensation(item, owner, now, message)
	})
}

func (r *runtimeRepository) DeferRuntimeDeliveryCompensation(ctx context.Context, id, owner string, now, availableAt time.Time, message string) (bool, error) {
	return r.mutateRuntimeDeliveryCompensation(ctx, id, owner, now, func(item agentruntime.RuntimeDeliveryCompensation, owner string, now time.Time) (agentruntime.RuntimeDeliveryCompensation, bool, error) {
		return agentruntime.DeferRuntimeDeliveryCompensation(item, owner, now, availableAt, message)
	})
}

func (r *runtimeRepository) mutateRuntimeDeliveryCompensation(ctx context.Context, id, owner string, now time.Time, mutate func(agentruntime.RuntimeDeliveryCompensation, string, time.Time) (agentruntime.RuntimeDeliveryCompensation, bool, error)) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeDeliveryCompensationOwnerLength {
		return false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	id = strings.TrimSpace(id)
	var changed bool
	err := withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var row runtimeDeliveryCompensationRow
		if findErr := tx.Where("id = ?", id).First(&row).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		item, decodeErr := runtimeDeliveryCompensationFromRow(row)
		if decodeErr != nil {
			return decodeErr
		}
		updated, transitioned, mutateErr := mutate(item, owner, now)
		if mutateErr != nil {
			if errors.Is(mutateErr, agentruntime.ErrConflict) && (item.Status != agentruntime.RuntimeDeliveryCompensationProcessing || item.LeaseOwner != owner || item.LeaseExpiresAt == nil || !item.LeaseExpiresAt.After(now)) {
				return nil
			}
			return mutateErr
		}
		// The domain helpers use the bool to report whether a transition was
		// applied. Complete also returns true for an already-completed row as an
		// idempotent duplicate; an unchanged revision identifies that case.
		if !transitioned || updated.Revision == item.Revision {
			return nil
		}
		updatedRow, rowErr := runtimeDeliveryCompensationToRow(updated)
		if rowErr != nil {
			return rowErr
		}
		result := tx.Model(&runtimeDeliveryCompensationRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", item.ID, item.Revision, string(agentruntime.RuntimeDeliveryCompensationProcessing), owner, now).Updates(map[string]any{
			"status": string(updatedRow.Status), "attempt": updatedRow.Attempt, "revision": updatedRow.Revision, "available_at": updatedRow.AvailableAt,
			"lease_owner": updatedRow.LeaseOwner, "lease_expires_at": updatedRow.LeaseExpiresAt, "last_error": updatedRow.LastError,
			"updated_at": updatedRow.UpdatedAt, "completed_at": updatedRow.CompletedAt,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return agentruntime.ErrConflict
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

var _ agentruntime.RuntimeDeliveryCompensationRepository = (*runtimeRepository)(nil)
