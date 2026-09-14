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

// runtimeDeliveryGroupRow stores only the group ledger and member metadata;
// payloads remain in their family-specific outbox tables.
type runtimeDeliveryGroupRow struct {
	ID             string     `gorm:"primaryKey;size:220"`
	InvocationID   string     `gorm:"index:idx_runtime_delivery_group_invocation,priority:1;size:512;not null"`
	Source         string     `gorm:"index;size:160;not null"`
	Destination    string     `gorm:"index;size:160;not null"`
	FenceID        string     `gorm:"index;size:220"`
	FenceRevision  int64      `gorm:"index"`
	MembersJSON    string     `gorm:"type:text;not null"`
	Status         string     `gorm:"index;size:32;not null"`
	Attempt        int        `gorm:"not null"`
	Revision       int64      `gorm:"not null"`
	AvailableAt    time.Time  `gorm:"index"`
	LeaseOwner     string     `gorm:"index;size:160"`
	LeaseExpiresAt *time.Time `gorm:"index"`
	LastError      string     `gorm:"type:text"`
	CreatedAt      time.Time  `gorm:"index:idx_runtime_delivery_group_invocation,priority:2"`
	UpdatedAt      time.Time
	CompletedAt    *time.Time
}

func (runtimeDeliveryGroupRow) TableName() string {
	return "abot_agent_runtime_delivery_groups"
}

func runtimeDeliveryGroupFromRow(row runtimeDeliveryGroupRow) (agentruntime.RuntimeDeliveryGroup, error) {
	var members []agentruntime.RuntimeDeliveryGroupMember
	if err := json.Unmarshal([]byte(row.MembersJSON), &members); err != nil {
		return agentruntime.RuntimeDeliveryGroup{}, fmt.Errorf("decode runtime delivery group members: %w", err)
	}
	item := agentruntime.RuntimeDeliveryGroup{
		ID: row.ID, InvocationID: row.InvocationID, Source: row.Source, Destination: row.Destination,
		FenceID: row.FenceID, FenceRevision: row.FenceRevision,
		Members: members, Status: agentruntime.RuntimeDeliveryGroupStatus(row.Status), Attempt: row.Attempt,
		Revision: row.Revision, AvailableAt: row.AvailableAt, LeaseOwner: row.LeaseOwner,
		LeaseExpiresAt: cloneTimePtr(row.LeaseExpiresAt), LastError: row.LastError,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, CompletedAt: cloneTimePtr(row.CompletedAt),
	}
	return item.Normalize(time.Now().UTC())
}

func runtimeDeliveryGroupToRow(item agentruntime.RuntimeDeliveryGroup) (*runtimeDeliveryGroupRow, error) {
	normalized, err := item.Normalize(time.Now().UTC())
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(normalized.Members)
	if err != nil {
		return nil, err
	}
	if len(encoded) > 64<<10 {
		return nil, fmt.Errorf("%w: members 超过字节上限", agentruntime.ErrInvalidRuntimeDeliveryGroup)
	}
	return &runtimeDeliveryGroupRow{
		ID: normalized.ID, InvocationID: normalized.InvocationID, Source: normalized.Source, Destination: normalized.Destination,
		FenceID: normalized.FenceID, FenceRevision: normalized.FenceRevision,
		MembersJSON: string(encoded), Status: string(normalized.Status), Attempt: normalized.Attempt, Revision: normalized.Revision,
		AvailableAt: normalized.AvailableAt, LeaseOwner: normalized.LeaseOwner, LeaseExpiresAt: cloneTimePtr(normalized.LeaseExpiresAt),
		LastError: normalized.LastError, CreatedAt: normalized.CreatedAt, UpdatedAt: normalized.UpdatedAt, CompletedAt: cloneTimePtr(normalized.CompletedAt),
	}, nil
}

// ensureRuntimeDeliveryGroupTx inserts one immutable group identity inside an
// existing business transaction. Retries observe the winner and verify its
// member identities instead of silently accepting a different group payload.
func ensureRuntimeDeliveryGroupTx(tx *gorm.DB, item agentruntime.RuntimeDeliveryGroup) error {
	row, err := runtimeDeliveryGroupToRow(item)
	if err != nil {
		return err
	}
	var existing runtimeDeliveryGroupRow
	findErr := tx.Where("id = ?", row.ID).First(&existing).Error
	if findErr == nil {
		stored, decodeErr := runtimeDeliveryGroupFromRow(existing)
		if decodeErr != nil {
			return decodeErr
		}
		candidate, candidateErr := item.Normalize(time.Now().UTC())
		if candidateErr != nil {
			return candidateErr
		}
		if !stored.MatchesIdentity(candidate) {
			return agentruntime.ErrConflict
		}
		return nil
	}
	if !errors.Is(findErr, gorm.ErrRecordNotFound) {
		return findErr
	}
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 1 {
		return nil
	}
	if err := tx.Where("id = ?", row.ID).First(&existing).Error; err != nil {
		return err
	}
	stored, decodeErr := runtimeDeliveryGroupFromRow(existing)
	if decodeErr != nil {
		return decodeErr
	}
	if !stored.MatchesIdentity(item) {
		return agentruntime.ErrConflict
	}
	return nil
}

func (r *runtimeRepository) EnqueueRuntimeDeliveryGroup(ctx context.Context, item agentruntime.RuntimeDeliveryGroup) (agentruntime.RuntimeDeliveryGroup, error) {
	now := time.Now().UTC()
	normalized, err := item.Normalize(now)
	if err != nil {
		return agentruntime.RuntimeDeliveryGroup{}, err
	}
	row, err := runtimeDeliveryGroupToRow(normalized)
	if err != nil {
		return agentruntime.RuntimeDeliveryGroup{}, err
	}
	var saved agentruntime.RuntimeDeliveryGroup
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row)
		if result.Error != nil {
			return result.Error
		}
		var winner runtimeDeliveryGroupRow
		if findErr := tx.Where("id = ?", normalized.ID).First(&winner).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		stored, decodeErr := runtimeDeliveryGroupFromRow(winner)
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
		return agentruntime.RuntimeDeliveryGroup{}, err
	}
	return saved, nil
}

func (r *runtimeRepository) GetRuntimeDeliveryGroup(ctx context.Context, id string) (agentruntime.RuntimeDeliveryGroup, error) {
	var row runtimeDeliveryGroupRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeDeliveryGroup{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeDeliveryGroup{}, err
	}
	return runtimeDeliveryGroupFromRow(row)
}

func (r *runtimeRepository) ListRuntimeDeliveryGroups(ctx context.Context, invocationID string, status agentruntime.RuntimeDeliveryGroupStatus, limit int) ([]agentruntime.RuntimeDeliveryGroup, error) {
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
	query := r.db.WithContext(ctx).Model(&runtimeDeliveryGroupRow{})
	if invocationID != "" {
		query = query.Where("invocation_id = ?", invocationID)
	}
	if status != "" {
		if status != agentruntime.RuntimeDeliveryGroupQueued && status != agentruntime.RuntimeDeliveryGroupProcessing && status != agentruntime.RuntimeDeliveryGroupCompleted && status != agentruntime.RuntimeDeliveryGroupFailed {
			return nil, fmt.Errorf("%w: group status 无效", agentruntime.ErrInvalidRuntimeDeliveryGroup)
		}
		query = query.Where("status = ?", string(status))
	}
	var rows []runtimeDeliveryGroupRow
	if err := query.Order("available_at ASC").Order("created_at ASC").Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]agentruntime.RuntimeDeliveryGroup, 0, len(rows))
	for _, row := range rows {
		item, decodeErr := runtimeDeliveryGroupFromRow(row)
		if decodeErr != nil {
			return nil, decodeErr
		}
		items = append(items, item)
	}
	return items, nil
}

func (r *runtimeRepository) ClaimRuntimeDeliveryGroup(ctx context.Context, owner string, now time.Time, ttl time.Duration) (agentruntime.RuntimeDeliveryGroup, bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeDeliveryGroupOwnerLength || ttl <= 0 || ttl > agentruntime.MaxRuntimeDeliveryGroupLease {
		return agentruntime.RuntimeDeliveryGroup{}, false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var claimed agentruntime.RuntimeDeliveryGroup
	var won bool
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []runtimeDeliveryGroupRow
		// Include future queued rows as well as due rows so a legacy/crash row
		// whose members are already terminal can be repaired without first
		// consuming its group attempt. The due checks below still gate normal
		// lease acquisition.
		if err := tx.Where("status IN (?, ?)", string(agentruntime.RuntimeDeliveryGroupQueued), string(agentruntime.RuntimeDeliveryGroupProcessing)).Order("available_at ASC").Order("created_at ASC").Order("id ASC").Limit(64).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			item, decodeErr := runtimeDeliveryGroupFromRow(row)
			if decodeErr != nil {
				return decodeErr
			}
			if repairRuntimeDeliveryGroupAggregateForSQLite(&item, now) {
				result := tx.Model(&runtimeDeliveryGroupRow{}).Where("id = ? AND revision = ?", item.ID, row.Revision).Updates(map[string]any{
					"status": string(item.Status), "lease_owner": "", "lease_expires_at": nil,
					"last_error": item.LastError, "completed_at": cloneTimePtr(item.CompletedAt),
					"revision": item.Revision, "updated_at": item.UpdatedAt,
				})
				if result.Error != nil {
					return result.Error
				}
				// A concurrent member CAS may have won while this transaction was
				// reading the row. Do not claim or overwrite that newer state.
				if result.RowsAffected != 1 {
					continue
				}
				continue
			}
			if item.Status == agentruntime.RuntimeDeliveryGroupQueued && item.AvailableAt.After(now) {
				continue
			}
			if item.Status == agentruntime.RuntimeDeliveryGroupProcessing && item.LeaseExpiresAt != nil && item.LeaseExpiresAt.After(now) {
				continue
			}
			if item.Attempt >= agentruntime.MaxRuntimeDeliveryGroupAttempts {
				result := tx.Model(&runtimeDeliveryGroupRow{}).Where("id = ? AND revision = ?", item.ID, item.Revision).Updates(map[string]any{
					"status": string(agentruntime.RuntimeDeliveryGroupFailed), "lease_owner": "", "lease_expires_at": nil,
					"last_error": "Runtime delivery group 达到最大投递次数", "revision": item.Revision + 1, "updated_at": now,
				})
				if result.Error != nil {
					return result.Error
				}
				continue
			}
			expires := now.Add(ttl)
			result := tx.Model(&runtimeDeliveryGroupRow{}).Where("id = ? AND revision = ? AND ((status = ? AND available_at <= ?) OR (status = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?)))", item.ID, item.Revision, string(agentruntime.RuntimeDeliveryGroupQueued), now, string(agentruntime.RuntimeDeliveryGroupProcessing), now).Updates(map[string]any{
				"status": string(agentruntime.RuntimeDeliveryGroupProcessing), "attempt": item.Attempt + 1, "revision": item.Revision + 1,
				"lease_owner": owner, "lease_expires_at": &expires, "updated_at": now,
			})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				continue
			}
			var updated runtimeDeliveryGroupRow
			if err := tx.Where("id = ?", item.ID).First(&updated).Error; err != nil {
				return err
			}
			claimed, decodeErr = runtimeDeliveryGroupFromRow(updated)
			if decodeErr != nil {
				return decodeErr
			}
			won = true
			return nil
		}
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeDeliveryGroup{}, false, err
	}
	return claimed, won, nil
}

// ClaimRuntimeDeliveryGroupByID claims one exact group after a coordinator
// has inspected its member cursors.  The targeted CAS avoids taking a lease
// on an unrelated queued group merely because it sorted first.
func (r *runtimeRepository) ClaimRuntimeDeliveryGroupByID(ctx context.Context, id, owner string, now time.Time, ttl time.Duration) (agentruntime.RuntimeDeliveryGroup, bool, error) {
	id = strings.TrimSpace(id)
	owner = strings.TrimSpace(owner)
	if id == "" || owner == "" || len(owner) > agentruntime.MaxRuntimeDeliveryGroupOwnerLength || ttl <= 0 || ttl > agentruntime.MaxRuntimeDeliveryGroupLease {
		return agentruntime.RuntimeDeliveryGroup{}, false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var claimed agentruntime.RuntimeDeliveryGroup
	var won bool
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row runtimeDeliveryGroupRow
		if err := tx.Where("id = ?", id).First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return err
		}
		item, decodeErr := runtimeDeliveryGroupFromRow(row)
		if decodeErr != nil {
			return decodeErr
		}
		if item.Status == agentruntime.RuntimeDeliveryGroupCompleted || item.Status == agentruntime.RuntimeDeliveryGroupFailed {
			claimed = item
			return nil
		}
		// Repair a crash/legacy window where all members are terminal but the
		// aggregate status was not persisted yet.  No lease or attempt is
		// consumed by this metadata-only transition.
		allCompleted, anyFailed := runtimeDeliveryGroupMembersCompletedForSQLite(item.Members)
		if anyFailed || allCompleted {
			item.LeaseOwner = ""
			item.LeaseExpiresAt = nil
			item.CompletedAt = nil
			if anyFailed {
				item.Status = agentruntime.RuntimeDeliveryGroupFailed
				if item.LastError == "" {
					for _, member := range item.Members {
						if member.Status == agentruntime.RuntimeDeliveryGroupMemberFailed {
							item.LastError = member.LastError
							break
						}
					}
				}
			} else {
				item.Status = agentruntime.RuntimeDeliveryGroupCompleted
				finished := now
				item.CompletedAt = &finished
			}
			result := tx.Model(&runtimeDeliveryGroupRow{}).Where("id = ? AND revision = ?", item.ID, item.Revision).Updates(map[string]any{
				"status": string(item.Status), "lease_owner": "", "lease_expires_at": nil,
				"last_error": item.LastError, "completed_at": cloneTimePtr(item.CompletedAt), "revision": item.Revision + 1, "updated_at": now,
			})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 1 {
				item.Revision++
				item.UpdatedAt = now
				claimed = item
			}
			return nil
		}
		if item.Status == agentruntime.RuntimeDeliveryGroupQueued && item.AvailableAt.After(now) {
			claimed = item
			return nil
		}
		if item.Status == agentruntime.RuntimeDeliveryGroupProcessing && item.LeaseExpiresAt != nil && item.LeaseExpiresAt.After(now) {
			claimed = item
			return nil
		}
		if item.Attempt >= agentruntime.MaxRuntimeDeliveryGroupAttempts {
			result := tx.Model(&runtimeDeliveryGroupRow{}).Where("id = ? AND revision = ?", item.ID, item.Revision).Updates(map[string]any{
				"status": string(agentruntime.RuntimeDeliveryGroupFailed), "lease_owner": "", "lease_expires_at": nil,
				"last_error": "Runtime delivery group 达到最大投递次数", "completed_at": nil, "revision": item.Revision + 1, "updated_at": now,
			})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return nil
			}
			item.Status = agentruntime.RuntimeDeliveryGroupFailed
			item.LeaseOwner = ""
			item.LeaseExpiresAt = nil
			item.LastError = "Runtime delivery group 达到最大投递次数"
			item.CompletedAt = nil
			item.Revision++
			item.UpdatedAt = now
			claimed = item
			return nil
		}
		expires := now.Add(ttl)
		where := tx.Model(&runtimeDeliveryGroupRow{}).Where("id = ? AND revision = ?", item.ID, item.Revision)
		if item.Status == agentruntime.RuntimeDeliveryGroupQueued {
			where = where.Where("status = ? AND available_at <= ?", string(agentruntime.RuntimeDeliveryGroupQueued), now)
		} else {
			where = where.Where("status = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?)", string(agentruntime.RuntimeDeliveryGroupProcessing), now)
		}
		result := where.Updates(map[string]any{
			"status": string(agentruntime.RuntimeDeliveryGroupProcessing), "attempt": item.Attempt + 1, "revision": item.Revision + 1,
			"lease_owner": owner, "lease_expires_at": &expires, "completed_at": nil, "updated_at": now,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}
		var updated runtimeDeliveryGroupRow
		if err := tx.Where("id = ?", id).First(&updated).Error; err != nil {
			return err
		}
		var updatedErr error
		claimed, updatedErr = runtimeDeliveryGroupFromRow(updated)
		if updatedErr != nil {
			return updatedErr
		}
		won = true
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeDeliveryGroup{}, false, err
	}
	return claimed, won, nil
}

// DeferRuntimeDeliveryGroup releases a coordination lease without consuming
// a group retry attempt.  Family-specific source outboxes remain responsible
// for transport retries; this row only waits for their next state change.
func (r *runtimeRepository) DeferRuntimeDeliveryGroup(ctx context.Context, id, owner string, now, availableAt time.Time, message string) (bool, error) {
	id = strings.TrimSpace(id)
	owner = strings.TrimSpace(owner)
	if id == "" || owner == "" || len(owner) > agentruntime.MaxRuntimeDeliveryGroupOwnerLength {
		return false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if availableAt.IsZero() {
		availableAt = now
	} else {
		availableAt = availableAt.UTC()
	}
	if availableAt.Before(now) {
		availableAt = now
	}
	if availableAt.After(now.Add(agentruntime.MaxRuntimeDeliveryGroupLease)) {
		return false, agentruntime.ErrConflict
	}
	message = agentruntime.SanitizeRuntimeString(strings.TrimSpace(message))
	if len(message) > agentruntime.MaxRuntimeDeliveryGroupErrorLength {
		message = message[:agentruntime.MaxRuntimeDeliveryGroupErrorLength]
	}
	var row runtimeDeliveryGroupRow
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, agentruntime.ErrNotFound
		}
		return false, err
	}
	attempt := row.Attempt
	if attempt > 0 {
		attempt--
	}
	result := r.db.WithContext(ctx).Model(&runtimeDeliveryGroupRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", row.ID, row.Revision, string(agentruntime.RuntimeDeliveryGroupProcessing), owner, now).Updates(map[string]any{
		"status": string(agentruntime.RuntimeDeliveryGroupQueued), "attempt": attempt, "available_at": availableAt,
		"lease_owner": "", "lease_expires_at": nil, "completed_at": nil, "last_error": message,
		"revision": row.Revision + 1, "updated_at": now,
	})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func (r *runtimeRepository) CompleteRuntimeDeliveryGroup(ctx context.Context, id, owner string, now time.Time) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeDeliveryGroupOwnerLength {
		return false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var row runtimeDeliveryGroupRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, agentruntime.ErrNotFound
		}
		return false, err
	}
	item, err := runtimeDeliveryGroupFromRow(row)
	if err != nil {
		return false, err
	}
	allCompleted, anyFailed := runtimeDeliveryGroupMembersCompletedForSQLite(item.Members)
	if anyFailed || !allCompleted {
		return false, agentruntime.ErrConflict
	}
	finished := now
	result := r.db.WithContext(ctx).Model(&runtimeDeliveryGroupRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", item.ID, item.Revision, string(agentruntime.RuntimeDeliveryGroupProcessing), owner, now).Updates(map[string]any{
		"status": string(agentruntime.RuntimeDeliveryGroupCompleted), "lease_owner": "", "lease_expires_at": nil,
		"completed_at": &finished, "revision": item.Revision + 1, "updated_at": now,
	})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func (r *runtimeRepository) RetryRuntimeDeliveryGroup(ctx context.Context, id, owner string, now time.Time, message string) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeDeliveryGroupOwnerLength {
		return false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var row runtimeDeliveryGroupRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, agentruntime.ErrNotFound
		}
		return false, err
	}
	item, err := runtimeDeliveryGroupFromRow(row)
	if err != nil {
		return false, err
	}
	message = agentruntime.SanitizeRuntimeString(strings.TrimSpace(message))
	if len(message) > agentruntime.MaxRuntimeDeliveryGroupErrorLength {
		message = message[:agentruntime.MaxRuntimeDeliveryGroupErrorLength]
	}
	status := agentruntime.RuntimeDeliveryGroupQueued
	availableAt := now.Add(agentruntime.RuntimeDeliveryGroupBackoff(item.Attempt))
	if item.Attempt >= agentruntime.MaxRuntimeDeliveryGroupAttempts {
		status = agentruntime.RuntimeDeliveryGroupFailed
		availableAt = item.AvailableAt
	}
	result := r.db.WithContext(ctx).Model(&runtimeDeliveryGroupRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", item.ID, item.Revision, string(agentruntime.RuntimeDeliveryGroupProcessing), owner, now).Updates(map[string]any{
		"status": string(status), "available_at": availableAt, "lease_owner": "", "lease_expires_at": nil,
		"last_error": message, "completed_at": nil, "revision": item.Revision + 1, "updated_at": now,
	})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func (r *runtimeRepository) MarkRuntimeDeliveryGroupMember(ctx context.Context, id string, expectedRevision int64, kind agentruntime.RuntimeDeliveryKind, outboxID, deliveryID string, status agentruntime.RuntimeDeliveryGroupMemberStatus, message string, now time.Time) (agentruntime.RuntimeDeliveryGroup, bool, error) {
	if expectedRevision <= 0 || strings.TrimSpace(outboxID) == "" || strings.TrimSpace(deliveryID) == "" {
		return agentruntime.RuntimeDeliveryGroup{}, false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var saved agentruntime.RuntimeDeliveryGroup
	var duplicate bool
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row runtimeDeliveryGroupRow
		if err := tx.Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return err
		}
		item, decodeErr := runtimeDeliveryGroupFromRow(row)
		if decodeErr != nil {
			return decodeErr
		}
		if item.Revision != expectedRevision {
			return agentruntime.ErrConflict
		}
		memberIndex := -1
		for index, member := range item.Members {
			if member.Kind == kind && member.OutboxID == strings.TrimSpace(outboxID) && member.DeliveryID == strings.TrimSpace(deliveryID) {
				memberIndex = index
				break
			}
		}
		if memberIndex < 0 {
			return agentruntime.ErrNotFound
		}
		if item.Status == agentruntime.RuntimeDeliveryGroupCompleted || item.Status == agentruntime.RuntimeDeliveryGroupFailed {
			if item.Members[memberIndex].Status == status {
				saved, duplicate = item, true
				return nil
			}
			return agentruntime.ErrConflict
		}
		current := item.Members[memberIndex].Status
		if current == status {
			// Repair an aggregate left in processing by a legacy/crash window
			// while keeping a truly idempotent mark a no-op.
			allCompleted, anyFailed := runtimeDeliveryGroupMembersCompletedForSQLite(item.Members)
			if !anyFailed && !allCompleted {
				saved, duplicate = item, true
				return nil
			}
			if anyFailed && item.Status != agentruntime.RuntimeDeliveryGroupFailed {
				item.Status = agentruntime.RuntimeDeliveryGroupFailed
				item.LeaseOwner = ""
				item.LeaseExpiresAt = nil
				item.CompletedAt = nil
				if item.LastError == "" {
					item.LastError = item.Members[memberIndex].LastError
				}
			} else if allCompleted && item.Status != agentruntime.RuntimeDeliveryGroupCompleted {
				item.Status = agentruntime.RuntimeDeliveryGroupCompleted
				item.LeaseOwner = ""
				item.LeaseExpiresAt = nil
				finished := now
				item.CompletedAt = &finished
			} else {
				saved, duplicate = item, true
				return nil
			}
		} else {
			if current != agentruntime.RuntimeDeliveryGroupMemberPending {
				return agentruntime.ErrConflict
			}
			item.Members[memberIndex].Status = status
			item.Members[memberIndex].LastError = agentruntime.SanitizeRuntimeString(strings.TrimSpace(message))
			if len(item.Members[memberIndex].LastError) > agentruntime.MaxRuntimeDeliveryGroupMemberErrorLength {
				item.Members[memberIndex].LastError = item.Members[memberIndex].LastError[:agentruntime.MaxRuntimeDeliveryGroupMemberErrorLength]
			}
			item.Members[memberIndex].UpdatedAt = now
			allCompleted, anyFailed := runtimeDeliveryGroupMembersCompletedForSQLite(item.Members)
			if anyFailed {
				item.Status = agentruntime.RuntimeDeliveryGroupFailed
				item.LeaseOwner = ""
				item.LeaseExpiresAt = nil
				item.CompletedAt = nil
				if item.LastError == "" {
					item.LastError = item.Members[memberIndex].LastError
				}
			} else if allCompleted {
				item.Status = agentruntime.RuntimeDeliveryGroupCompleted
				item.LeaseOwner = ""
				item.LeaseExpiresAt = nil
				finished := now
				item.CompletedAt = &finished
			}
		}
		item.Revision++
		item.UpdatedAt = now
		encoded, marshalErr := json.Marshal(item.Members)
		if marshalErr != nil {
			return marshalErr
		}
		result := tx.Model(&runtimeDeliveryGroupRow{}).Where("id = ? AND revision = ?", item.ID, expectedRevision).Updates(map[string]any{
			"members_json": string(encoded), "status": string(item.Status), "lease_owner": item.LeaseOwner,
			"lease_expires_at": cloneTimePtr(item.LeaseExpiresAt), "last_error": item.LastError,
			"revision": item.Revision, "updated_at": item.UpdatedAt, "completed_at": cloneTimePtr(item.CompletedAt),
		})
		if marshalErr != nil {
			return marshalErr
		}
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return agentruntime.ErrConflict
		}
		saved = item
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeDeliveryGroup{}, false, err
	}
	return saved, duplicate, nil
}

func runtimeDeliveryGroupMembersCompletedForSQLite(members []agentruntime.RuntimeDeliveryGroupMember) (bool, bool) {
	allCompleted := len(members) > 0
	anyFailed := false
	for _, member := range members {
		switch member.Status {
		case agentruntime.RuntimeDeliveryGroupMemberCompleted:
		case agentruntime.RuntimeDeliveryGroupMemberFailed:
			anyFailed = true
			allCompleted = false
		default:
			allCompleted = false
		}
	}
	return allCompleted, anyFailed
}

// repairRuntimeDeliveryGroupAggregateForSQLite mirrors the Memory repair
// path. It intentionally performs no attempt increment: all member cursors
// are already terminal, so this is only aggregate bookkeeping recovery.
func repairRuntimeDeliveryGroupAggregateForSQLite(item *agentruntime.RuntimeDeliveryGroup, now time.Time) bool {
	if item == nil || item.Status == agentruntime.RuntimeDeliveryGroupCompleted || item.Status == agentruntime.RuntimeDeliveryGroupFailed {
		return false
	}
	allCompleted, anyFailed := runtimeDeliveryGroupMembersCompletedForSQLite(item.Members)
	if !anyFailed && !allCompleted {
		return false
	}
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	item.CompletedAt = nil
	if anyFailed {
		item.Status = agentruntime.RuntimeDeliveryGroupFailed
		if item.LastError == "" {
			for _, member := range item.Members {
				if member.Status == agentruntime.RuntimeDeliveryGroupMemberFailed {
					item.LastError = member.LastError
					break
				}
			}
		}
	} else {
		item.Status = agentruntime.RuntimeDeliveryGroupCompleted
		finished := now
		item.CompletedAt = &finished
	}
	item.Revision++
	item.UpdatedAt = now
	return true
}

var _ agentruntime.RuntimeDeliveryGroupRepository = (*runtimeRepository)(nil)
