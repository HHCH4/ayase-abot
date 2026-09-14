package sqlite

import (
	"context"
	"errors"
	"strings"
	"time"

	agentruntime "Abot/internal/agent/runtime"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type runtimeDeliveryGroupSettlementRow struct {
	ID             string     `gorm:"primaryKey;size:220"`
	GroupID        string     `gorm:"index;size:220;not null"`
	InvocationID   string     `gorm:"index;size:512;not null"`
	Source         string     `gorm:"index;size:160;not null"`
	Destination    string     `gorm:"index;size:160;not null"`
	Outcome        string     `gorm:"index;size:32;not null"`
	Phase          string     `gorm:"index;size:32;not null"`
	Status         string     `gorm:"index;size:32;not null"`
	Stage          string     `gorm:"index;size:32;not null"`
	RemotePhase    string     `gorm:"index;size:32"`
	AlertCode      string     `gorm:"index;size:48"`
	AlertCount     int        `gorm:"not null"`
	AlertAt        *time.Time `gorm:"index"`
	LastStatusAt   *time.Time `gorm:"index"`
	Attempt        int        `gorm:"not null"`
	Revision       int64      `gorm:"not null"`
	AvailableAt    time.Time  `gorm:"index"`
	LeaseOwner     string     `gorm:"index;size:160"`
	LeaseExpiresAt *time.Time `gorm:"index"`
	LastError      string     `gorm:"type:text"`
	CreatedAt      time.Time  `gorm:"index"`
	UpdatedAt      time.Time
	CompletedAt    *time.Time
}

func (runtimeDeliveryGroupSettlementRow) TableName() string {
	return "abot_agent_runtime_delivery_group_settlements"
}

func runtimeDeliveryGroupSettlementFromRow(row runtimeDeliveryGroupSettlementRow) (agentruntime.RuntimeDeliveryGroupSettlement, error) {
	item := agentruntime.RuntimeDeliveryGroupSettlement{
		ID: row.ID, GroupID: row.GroupID, InvocationID: row.InvocationID, Source: row.Source, Destination: row.Destination,
		Outcome: agentruntime.RuntimeDeliveryGroupStatus(row.Outcome), Phase: agentruntime.RuntimeDeliveryGroupSettlementPhase(row.Phase),
		Status: agentruntime.RuntimeDeliveryGroupSettlementStatus(row.Status), Stage: agentruntime.RuntimeDeliveryGroupSettlementStage(row.Stage),
		RemotePhase: agentruntime.RuntimeDeliveryGroupSettlementRemotePhase(row.RemotePhase), AlertCode: agentruntime.RuntimeDeliveryGroupSettlementAlertCode(row.AlertCode), AlertCount: row.AlertCount,
		AlertAt: cloneTimePtr(row.AlertAt), LastStatusAt: cloneTimePtr(row.LastStatusAt), Attempt: row.Attempt, Revision: row.Revision,
		AvailableAt: row.AvailableAt, LeaseOwner: row.LeaseOwner, LeaseExpiresAt: cloneTimePtr(row.LeaseExpiresAt),
		LastError: row.LastError, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, CompletedAt: cloneTimePtr(row.CompletedAt),
	}
	return item.Normalize(time.Now().UTC())
}

func runtimeDeliveryGroupSettlementToRow(item agentruntime.RuntimeDeliveryGroupSettlement) (*runtimeDeliveryGroupSettlementRow, error) {
	normalized, err := item.Normalize(time.Now().UTC())
	if err != nil {
		return nil, err
	}
	return &runtimeDeliveryGroupSettlementRow{
		ID: normalized.ID, GroupID: normalized.GroupID, InvocationID: normalized.InvocationID, Source: normalized.Source, Destination: normalized.Destination,
		Outcome: string(normalized.Outcome), Phase: string(normalized.Phase), Status: string(normalized.Status), Attempt: normalized.Attempt,
		Stage: string(normalized.Stage), RemotePhase: string(normalized.RemotePhase), AlertCode: string(normalized.AlertCode), AlertCount: normalized.AlertCount,
		AlertAt: cloneTimePtr(normalized.AlertAt), LastStatusAt: cloneTimePtr(normalized.LastStatusAt), Revision: normalized.Revision, AvailableAt: normalized.AvailableAt, LeaseOwner: normalized.LeaseOwner, LeaseExpiresAt: cloneTimePtr(normalized.LeaseExpiresAt),
		LastError: normalized.LastError, CreatedAt: normalized.CreatedAt, UpdatedAt: normalized.UpdatedAt, CompletedAt: cloneTimePtr(normalized.CompletedAt),
	}, nil
}

func (r *runtimeRepository) EnqueueRuntimeDeliveryGroupSettlement(ctx context.Context, item agentruntime.RuntimeDeliveryGroupSettlement) (agentruntime.RuntimeDeliveryGroupSettlement, error) {
	now := time.Now().UTC()
	normalized, err := item.Normalize(now)
	if err != nil {
		return agentruntime.RuntimeDeliveryGroupSettlement{}, err
	}
	row, err := runtimeDeliveryGroupSettlementToRow(normalized)
	if err != nil {
		return agentruntime.RuntimeDeliveryGroupSettlement{}, err
	}
	var saved agentruntime.RuntimeDeliveryGroupSettlement
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row)
		if result.Error != nil {
			return result.Error
		}
		var winner runtimeDeliveryGroupSettlementRow
		if findErr := tx.Where("id = ?", normalized.ID).First(&winner).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		stored, decodeErr := runtimeDeliveryGroupSettlementFromRow(winner)
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
		return agentruntime.RuntimeDeliveryGroupSettlement{}, err
	}
	return saved, nil
}

func (r *runtimeRepository) GetRuntimeDeliveryGroupSettlement(ctx context.Context, id string) (agentruntime.RuntimeDeliveryGroupSettlement, error) {
	var row runtimeDeliveryGroupSettlementRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeDeliveryGroupSettlement{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeDeliveryGroupSettlement{}, err
	}
	return runtimeDeliveryGroupSettlementFromRow(row)
}

func (r *runtimeRepository) ListRuntimeDeliveryGroupSettlements(ctx context.Context, invocationID string, status agentruntime.RuntimeDeliveryGroupSettlementStatus, limit int) ([]agentruntime.RuntimeDeliveryGroupSettlement, error) {
	invocationID = strings.TrimSpace(invocationID)
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	if invocationID != "" {
		var invocation invocationRow
		if err := r.db.WithContext(ctx).Where("id = ?", invocationID).First(&invocation).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, agentruntime.ErrNotFound
			}
			return nil, err
		}
	}
	query := r.db.WithContext(ctx).Model(&runtimeDeliveryGroupSettlementRow{})
	if invocationID != "" {
		query = query.Where("invocation_id = ?", invocationID)
	}
	if status != "" {
		if !validRuntimeDeliveryGroupSettlementStatusSQLite(status) {
			return nil, agentruntime.ErrInvalidRuntimeDeliveryGroupSettlement
		}
		query = query.Where("status = ?", string(status))
	}
	var rows []runtimeDeliveryGroupSettlementRow
	if err := query.Order("available_at ASC").Order("created_at ASC").Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]agentruntime.RuntimeDeliveryGroupSettlement, 0, len(rows))
	for _, row := range rows {
		item, decodeErr := runtimeDeliveryGroupSettlementFromRow(row)
		if decodeErr != nil {
			return nil, decodeErr
		}
		items = append(items, item)
	}
	return items, nil
}

func validRuntimeDeliveryGroupSettlementStatusSQLite(status agentruntime.RuntimeDeliveryGroupSettlementStatus) bool {
	switch status {
	case agentruntime.RuntimeDeliveryGroupSettlementQueued, agentruntime.RuntimeDeliveryGroupSettlementProcessing,
		agentruntime.RuntimeDeliveryGroupSettlementCompleted, agentruntime.RuntimeDeliveryGroupSettlementFailed:
		return true
	default:
		return false
	}
}

func minRuntimeDeliveryGroupSettlementAlertCount(value int) int {
	if value < 0 {
		return 0
	}
	if value > agentruntime.MaxRuntimeDeliveryGroupSettlementAlertCount {
		return agentruntime.MaxRuntimeDeliveryGroupSettlementAlertCount
	}
	return value
}

func (r *runtimeRepository) ClaimRuntimeDeliveryGroupSettlement(ctx context.Context, owner string, now time.Time, ttl time.Duration) (agentruntime.RuntimeDeliveryGroupSettlement, bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeDeliveryGroupSettlementOwnerLength || ttl <= 0 || ttl > agentruntime.MaxRuntimeDeliveryGroupSettlementLease {
		return agentruntime.RuntimeDeliveryGroupSettlement{}, false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var claimed agentruntime.RuntimeDeliveryGroupSettlement
	var won bool
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []runtimeDeliveryGroupSettlementRow
		if err := tx.Where("status IN (?, ?)", string(agentruntime.RuntimeDeliveryGroupSettlementQueued), string(agentruntime.RuntimeDeliveryGroupSettlementProcessing)).Order("available_at ASC").Order("created_at ASC").Order("id ASC").Limit(64).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			item, decodeErr := runtimeDeliveryGroupSettlementFromRow(row)
			if decodeErr != nil {
				return decodeErr
			}
			if item.Status == agentruntime.RuntimeDeliveryGroupSettlementQueued && item.AvailableAt.After(now) {
				continue
			}
			if item.Status == agentruntime.RuntimeDeliveryGroupSettlementProcessing && item.LeaseExpiresAt != nil && item.LeaseExpiresAt.After(now) {
				continue
			}
			if item.Attempt >= agentruntime.MaxRuntimeDeliveryGroupSettlementAttempts {
				result := tx.Model(&runtimeDeliveryGroupSettlementRow{}).Where("id = ? AND revision = ?", item.ID, item.Revision).Updates(map[string]any{
					"status": string(agentruntime.RuntimeDeliveryGroupSettlementFailed), "lease_owner": "", "lease_expires_at": nil,
					"stage": string(agentruntime.RuntimeDeliveryGroupSettlementStageFailed), "alert_code": string(agentruntime.RuntimeDeliveryGroupSettlementAlertRetryExhausted), "alert_count": minRuntimeDeliveryGroupSettlementAlertCount(item.AlertCount + 1), "alert_at": &now,
					"completed_at": nil, "last_error": "Runtime delivery group settlement 达到最大投递次数", "revision": item.Revision + 1, "updated_at": now,
				})
				if result.Error != nil {
					return result.Error
				}
				continue
			}
			expires := now.Add(ttl)
			where := tx.Model(&runtimeDeliveryGroupSettlementRow{}).Where("id = ? AND revision = ?", item.ID, item.Revision)
			if item.Status == agentruntime.RuntimeDeliveryGroupSettlementQueued {
				where = where.Where("status = ? AND available_at <= ?", string(agentruntime.RuntimeDeliveryGroupSettlementQueued), now)
			} else {
				where = where.Where("status = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?)", string(agentruntime.RuntimeDeliveryGroupSettlementProcessing), now)
			}
			result := where.Updates(map[string]any{
				"status": string(agentruntime.RuntimeDeliveryGroupSettlementProcessing), "attempt": item.Attempt + 1, "revision": item.Revision + 1,
				"stage": string(agentruntime.RuntimeDeliveryGroupSettlementStageStatus), "lease_owner": owner, "lease_expires_at": &expires, "completed_at": nil, "updated_at": now,
			})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				continue
			}
			var updated runtimeDeliveryGroupSettlementRow
			if err := tx.Where("id = ?", item.ID).First(&updated).Error; err != nil {
				return err
			}
			claimed, decodeErr = runtimeDeliveryGroupSettlementFromRow(updated)
			if decodeErr != nil {
				return decodeErr
			}
			won = true
			return nil
		}
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeDeliveryGroupSettlement{}, false, err
	}
	return claimed, won, nil
}

func (r *runtimeRepository) CompleteRuntimeDeliveryGroupSettlement(ctx context.Context, id, owner string, now time.Time) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeDeliveryGroupSettlementOwnerLength {
		return false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var row runtimeDeliveryGroupSettlementRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, agentruntime.ErrNotFound
		}
		return false, err
	}
	item, err := runtimeDeliveryGroupSettlementFromRow(row)
	if err != nil {
		return false, err
	}
	finished := now
	result := r.db.WithContext(ctx).Model(&runtimeDeliveryGroupSettlementRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", item.ID, item.Revision, string(agentruntime.RuntimeDeliveryGroupSettlementProcessing), owner, now).Updates(map[string]any{
		"status": string(agentruntime.RuntimeDeliveryGroupSettlementCompleted), "stage": string(agentruntime.RuntimeDeliveryGroupSettlementStageCompleted), "lease_owner": "", "lease_expires_at": nil,
		"completed_at": &finished, "revision": item.Revision + 1, "updated_at": now,
	})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func (r *runtimeRepository) RetryRuntimeDeliveryGroupSettlement(ctx context.Context, id, owner string, now time.Time, message string) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeDeliveryGroupSettlementOwnerLength {
		return false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var row runtimeDeliveryGroupSettlementRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, agentruntime.ErrNotFound
		}
		return false, err
	}
	item, err := runtimeDeliveryGroupSettlementFromRow(row)
	if err != nil {
		return false, err
	}
	message = agentruntime.SanitizeRuntimeString(strings.TrimSpace(message))
	if len(message) > agentruntime.MaxRuntimeDeliveryGroupSettlementErrorLength {
		message = message[:agentruntime.MaxRuntimeDeliveryGroupSettlementErrorLength]
	}
	status := agentruntime.RuntimeDeliveryGroupSettlementQueued
	availableAt := now.Add(agentruntime.RuntimeDeliveryGroupSettlementBackoff(item.Attempt))
	if item.Attempt >= agentruntime.MaxRuntimeDeliveryGroupSettlementAttempts {
		status = agentruntime.RuntimeDeliveryGroupSettlementFailed
		availableAt = item.AvailableAt
	}
	stage := agentruntime.RuntimeDeliveryGroupSettlementStageQueued
	alertCode := item.AlertCode
	alertCount := item.AlertCount
	alertAt := item.AlertAt
	if status == agentruntime.RuntimeDeliveryGroupSettlementFailed {
		stage = agentruntime.RuntimeDeliveryGroupSettlementStageFailed
		alertCode = agentruntime.RuntimeDeliveryGroupSettlementAlertRetryExhausted
		alertCount = minRuntimeDeliveryGroupSettlementAlertCount(item.AlertCount + 1)
		alertAt = &now
	}
	result := r.db.WithContext(ctx).Model(&runtimeDeliveryGroupSettlementRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", item.ID, item.Revision, string(agentruntime.RuntimeDeliveryGroupSettlementProcessing), owner, now).Updates(map[string]any{
		"status": string(status), "stage": string(stage), "available_at": availableAt, "lease_owner": "", "lease_expires_at": nil,
		"alert_code": string(alertCode), "alert_count": alertCount, "alert_at": cloneTimePtr(alertAt),
		"last_error": message, "completed_at": nil, "revision": item.Revision + 1, "updated_at": now,
	})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func (r *runtimeRepository) FailRuntimeDeliveryGroupSettlement(ctx context.Context, id, owner string, now time.Time, message string) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeDeliveryGroupSettlementOwnerLength {
		return false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var row runtimeDeliveryGroupSettlementRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, agentruntime.ErrNotFound
		}
		return false, err
	}
	item, err := runtimeDeliveryGroupSettlementFromRow(row)
	if err != nil {
		return false, err
	}
	message = agentruntime.SanitizeRuntimeString(strings.TrimSpace(message))
	if len(message) > agentruntime.MaxRuntimeDeliveryGroupSettlementErrorLength {
		message = message[:agentruntime.MaxRuntimeDeliveryGroupSettlementErrorLength]
	}
	result := r.db.WithContext(ctx).Model(&runtimeDeliveryGroupSettlementRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", item.ID, item.Revision, string(agentruntime.RuntimeDeliveryGroupSettlementProcessing), owner, now).Updates(map[string]any{
		"status": string(agentruntime.RuntimeDeliveryGroupSettlementFailed), "stage": string(agentruntime.RuntimeDeliveryGroupSettlementStageFailed), "lease_owner": "", "lease_expires_at": nil,
		"last_error": message, "completed_at": nil, "revision": item.Revision + 1, "updated_at": now,
	})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func (r *runtimeRepository) AdvanceRuntimeDeliveryGroupSettlement(ctx context.Context, id, owner string, expectedRevision int64, stage agentruntime.RuntimeDeliveryGroupSettlementStage, remotePhase agentruntime.RuntimeDeliveryGroupSettlementRemotePhase, alertCode agentruntime.RuntimeDeliveryGroupSettlementAlertCode, message string, now time.Time) (agentruntime.RuntimeDeliveryGroupSettlement, bool, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var row runtimeDeliveryGroupSettlementRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeDeliveryGroupSettlement{}, false, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeDeliveryGroupSettlement{}, false, err
	}
	item, err := runtimeDeliveryGroupSettlementFromRow(row)
	if err != nil {
		return agentruntime.RuntimeDeliveryGroupSettlement{}, false, err
	}
	updated, duplicate, err := agentruntime.AdvanceRuntimeDeliveryGroupSettlement(item, owner, expectedRevision, stage, remotePhase, alertCode, message, now)
	if err != nil {
		return agentruntime.RuntimeDeliveryGroupSettlement{}, false, err
	}
	if duplicate {
		return updated, true, nil
	}
	result := r.db.WithContext(ctx).Model(&runtimeDeliveryGroupSettlementRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", item.ID, item.Revision, string(agentruntime.RuntimeDeliveryGroupSettlementProcessing), strings.TrimSpace(owner), now).Updates(map[string]any{
		"stage": string(updated.Stage), "remote_phase": string(updated.RemotePhase), "alert_code": string(updated.AlertCode), "alert_count": updated.AlertCount,
		"alert_at": cloneTimePtr(updated.AlertAt), "last_status_at": cloneTimePtr(updated.LastStatusAt), "last_error": updated.LastError,
		"revision": updated.Revision, "updated_at": updated.UpdatedAt,
	})
	if result.Error != nil {
		return agentruntime.RuntimeDeliveryGroupSettlement{}, false, result.Error
	}
	if result.RowsAffected != 1 {
		return agentruntime.RuntimeDeliveryGroupSettlement{}, false, agentruntime.ErrConflict
	}
	return updated, false, nil
}

var _ agentruntime.RuntimeDeliveryGroupSettlementRepository = (*runtimeRepository)(nil)
