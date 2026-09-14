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

type runtimeConfigDirectoryRebindMultiConfirmationRow struct {
	ID                   string     `gorm:"primaryKey;size:240"`
	PlanID               string     `gorm:"index;size:240;not null"`
	InvocationID         string     `gorm:"index;size:512;not null"`
	Source               string     `gorm:"index;size:160;not null"`
	DestinationsJSON     string     `gorm:"type:text;not null"`
	ConfigSnapshotDigest string     `gorm:"size:71;not null"`
	CapabilitiesDigest   string     `gorm:"size:71;not null"`
	Status               string     `gorm:"index;size:32;not null"`
	Revision             int64      `gorm:"not null"`
	ExpiresAt            time.Time  `gorm:"index;not null"`
	CreatedAt            time.Time  `gorm:"index"`
	ConfirmedAt          time.Time  `gorm:"not null"`
	ConsumedAt           *time.Time `gorm:"index"`
	UpdatedAt            time.Time
	IdempotencyKey       string `gorm:"index;size:200;not null"`
}

func (runtimeConfigDirectoryRebindMultiConfirmationRow) TableName() string {
	return "abot_runtime_config_directory_rebind_multi_confirmations"
}

func runtimeConfigDirectoryRebindMultiConfirmationFromRow(row runtimeConfigDirectoryRebindMultiConfirmationRow) (agentruntime.RuntimeConfigDirectoryRebindMultiConfirmation, error) {
	var destinations []string
	if err := json.Unmarshal([]byte(row.DestinationsJSON), &destinations); err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindMultiConfirmation{}, fmt.Errorf("解析 Runtime multi rebind confirmation destinations 失败: %w", err)
	}
	item := agentruntime.RuntimeConfigDirectoryRebindMultiConfirmation{
		ID: row.ID, PlanID: row.PlanID, InvocationID: row.InvocationID, Source: row.Source,
		Destinations: destinations, ConfigSnapshotDigest: row.ConfigSnapshotDigest, CapabilitiesDigest: row.CapabilitiesDigest,
		Status: agentruntime.RuntimeConfigDirectoryRebindMultiConfirmationStatus(row.Status), Revision: row.Revision,
		ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt, ConfirmedAt: row.ConfirmedAt,
		ConsumedAt: cloneTimePtr(row.ConsumedAt), IdempotencyKey: row.IdempotencyKey,
	}
	now := row.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return item.Normalize(now)
}

func runtimeConfigDirectoryRebindMultiConfirmationToRow(item agentruntime.RuntimeConfigDirectoryRebindMultiConfirmation, now time.Time) (*runtimeConfigDirectoryRebindMultiConfirmationRow, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	normalized, err := item.Normalize(now)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(normalized.Destinations)
	if err != nil {
		return nil, fmt.Errorf("编码 Runtime multi rebind confirmation destinations 失败: %w", err)
	}
	return &runtimeConfigDirectoryRebindMultiConfirmationRow{
		ID: normalized.ID, PlanID: normalized.PlanID, InvocationID: normalized.InvocationID, Source: normalized.Source,
		DestinationsJSON: string(encoded), ConfigSnapshotDigest: normalized.ConfigSnapshotDigest,
		CapabilitiesDigest: normalized.CapabilitiesDigest, Status: string(normalized.Status), Revision: normalized.Revision,
		ExpiresAt: normalized.ExpiresAt, CreatedAt: normalized.CreatedAt, ConfirmedAt: normalized.ConfirmedAt,
		ConsumedAt: cloneTimePtr(normalized.ConsumedAt), UpdatedAt: normalized.ConfirmedAt, IdempotencyKey: normalized.IdempotencyKey,
	}, nil
}

type runtimeConfigDirectoryRebindMultiApplyRow struct {
	ID                   string     `gorm:"primaryKey;size:260"`
	PlanID               string     `gorm:"index;size:240;not null"`
	ConfirmationID       string     `gorm:"index;size:240;not null"`
	ConfirmationDigest   string     `gorm:"size:64;not null"`
	Source               string     `gorm:"index;size:160;not null"`
	InvocationID         string     `gorm:"index;size:512;not null"`
	BoundaryStatus       string     `gorm:"index;size:40;not null"`
	ConfigSnapshotDigest string     `gorm:"size:71;not null"`
	CapabilitiesDigest   string     `gorm:"size:71;not null"`
	SnapshotRevision     int64      `gorm:"not null"`
	EventSequence        int64      `gorm:"not null"`
	SnapshotDigest       string     `gorm:"size:64;not null"`
	DestinationsJSON     string     `gorm:"type:text;not null"`
	ChildApplyIDsJSON    string     `gorm:"type:text;not null"`
	Status               string     `gorm:"index;size:32;not null"`
	PendingCount         int        `gorm:"not null"`
	ProcessingCount      int        `gorm:"not null"`
	CompletedCount       int        `gorm:"not null"`
	FailedCount          int        `gorm:"not null"`
	LastError            string     `gorm:"type:text"`
	Revision             int64      `gorm:"not null"`
	CreatedAt            time.Time  `gorm:"index"`
	UpdatedAt            time.Time  `gorm:"index"`
	CompletedAt          *time.Time `gorm:"index"`
	IdempotencyKey       string     `gorm:"index;size:200;not null"`
}

func (runtimeConfigDirectoryRebindMultiApplyRow) TableName() string {
	return "abot_runtime_config_directory_rebind_multi_applies"
}

func runtimeConfigDirectoryRebindMultiApplyFromRow(row runtimeConfigDirectoryRebindMultiApplyRow) (agentruntime.RuntimeConfigDirectoryRebindMultiApply, error) {
	var destinations, childIDs []string
	if err := json.Unmarshal([]byte(row.DestinationsJSON), &destinations); err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindMultiApply{}, fmt.Errorf("解析 Runtime multi rebind destinations 失败: %w", err)
	}
	if err := json.Unmarshal([]byte(row.ChildApplyIDsJSON), &childIDs); err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindMultiApply{}, fmt.Errorf("解析 Runtime multi rebind child IDs 失败: %w", err)
	}
	item := agentruntime.RuntimeConfigDirectoryRebindMultiApply{
		ID: row.ID, PlanID: row.PlanID, ConfirmationID: row.ConfirmationID, ConfirmationDigest: row.ConfirmationDigest,
		Source: row.Source, InvocationID: row.InvocationID, BoundaryStatus: agentruntime.InvocationStatus(row.BoundaryStatus),
		ConfigSnapshotDigest: row.ConfigSnapshotDigest, CapabilitiesDigest: row.CapabilitiesDigest,
		SnapshotRevision: row.SnapshotRevision, EventSequence: row.EventSequence, SnapshotDigest: row.SnapshotDigest,
		Destinations: destinations, ChildApplyIDs: childIDs, Status: agentruntime.RuntimeConfigDirectoryRebindMultiApplyStatus(row.Status),
		PendingCount: row.PendingCount, ProcessingCount: row.ProcessingCount, CompletedCount: row.CompletedCount, FailedCount: row.FailedCount,
		LastError: row.LastError, Revision: row.Revision, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		CompletedAt: cloneTimePtr(row.CompletedAt), IdempotencyKey: row.IdempotencyKey,
	}
	now := row.UpdatedAt
	if now.IsZero() {
		now = row.CreatedAt
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return item.Normalize(now)
}

func runtimeConfigDirectoryRebindMultiApplyToRow(item agentruntime.RuntimeConfigDirectoryRebindMultiApply, now time.Time) (*runtimeConfigDirectoryRebindMultiApplyRow, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	normalized, err := item.Normalize(now)
	if err != nil {
		return nil, err
	}
	destinations, err := json.Marshal(normalized.Destinations)
	if err != nil {
		return nil, fmt.Errorf("编码 Runtime multi rebind destinations 失败: %w", err)
	}
	childIDs, err := json.Marshal(normalized.ChildApplyIDs)
	if err != nil {
		return nil, fmt.Errorf("编码 Runtime multi rebind child IDs 失败: %w", err)
	}
	return &runtimeConfigDirectoryRebindMultiApplyRow{
		ID: normalized.ID, PlanID: normalized.PlanID, ConfirmationID: normalized.ConfirmationID, ConfirmationDigest: normalized.ConfirmationDigest,
		Source: normalized.Source, InvocationID: normalized.InvocationID, BoundaryStatus: string(normalized.BoundaryStatus),
		ConfigSnapshotDigest: normalized.ConfigSnapshotDigest, CapabilitiesDigest: normalized.CapabilitiesDigest,
		SnapshotRevision: normalized.SnapshotRevision, EventSequence: normalized.EventSequence, SnapshotDigest: normalized.SnapshotDigest,
		DestinationsJSON: string(destinations), ChildApplyIDsJSON: string(childIDs), Status: string(normalized.Status),
		PendingCount: normalized.PendingCount, ProcessingCount: normalized.ProcessingCount, CompletedCount: normalized.CompletedCount, FailedCount: normalized.FailedCount,
		LastError: normalized.LastError, Revision: normalized.Revision, CreatedAt: normalized.CreatedAt, UpdatedAt: normalized.UpdatedAt,
		CompletedAt: cloneTimePtr(normalized.CompletedAt), IdempotencyKey: normalized.IdempotencyKey,
	}, nil
}

func runtimeConfigDirectoryRebindMultiConfirmationIdentityMatches(left, right agentruntime.RuntimeConfigDirectoryRebindMultiConfirmation) bool {
	left, leftErr := left.Normalize(time.Time{})
	right, rightErr := right.Normalize(time.Time{})
	if leftErr != nil || rightErr != nil {
		return false
	}
	if left.ID != right.ID || left.PlanID != right.PlanID || left.InvocationID != right.InvocationID || left.Source != right.Source || left.ConfigSnapshotDigest != right.ConfigSnapshotDigest || left.CapabilitiesDigest != right.CapabilitiesDigest || left.IdempotencyKey != right.IdempotencyKey || len(left.Destinations) != len(right.Destinations) {
		return false
	}
	for index := range left.Destinations {
		if left.Destinations[index] != right.Destinations[index] {
			return false
		}
	}
	return true
}

func (r *runtimeRepository) CreateRuntimeConfigDirectoryRebindMultiConfirmation(ctx context.Context, plan agentruntime.RuntimeConfigDirectoryRebindPlan, idempotencyKey string, now time.Time) (agentruntime.RuntimeConfigDirectoryRebindMultiConfirmation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	plan, err := plan.Normalize(now)
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindMultiConfirmation{}, err
	}
	if plan.Status != agentruntime.RuntimeConfigDirectoryRebindValidated || len(plan.Destinations) < 2 {
		return agentruntime.RuntimeConfigDirectoryRebindMultiConfirmation{}, agentruntime.ErrRuntimeConfigDirectoryRebindMultiConfirmationConflict
	}
	confirmation, err := agentruntime.NewRuntimeConfigDirectoryRebindMultiConfirmation(plan, idempotencyKey, now)
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindMultiConfirmation{}, err
	}
	var saved agentruntime.RuntimeConfigDirectoryRebindMultiConfirmation
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var existingRow runtimeConfigDirectoryRebindMultiConfirmationRow
		findErr := tx.Where("id = ?", confirmation.ID).First(&existingRow).Error
		if findErr == nil {
			existing, decodeErr := runtimeConfigDirectoryRebindMultiConfirmationFromRow(existingRow)
			if decodeErr != nil {
				return decodeErr
			}
			if !runtimeConfigDirectoryRebindMultiConfirmationIdentityMatches(existing, confirmation) {
				return agentruntime.ErrRuntimeConfigDirectoryRebindMultiConfirmationConflict
			}
			if existing.Status == agentruntime.RuntimeConfigDirectoryRebindMultiConfirmationConfirmed && !existing.ExpiresAt.After(now) {
				result := tx.Model(&runtimeConfigDirectoryRebindMultiConfirmationRow{}).Where("id = ? AND revision = ? AND status = ?", existing.ID, existing.Revision, string(agentruntime.RuntimeConfigDirectoryRebindMultiConfirmationConfirmed)).Updates(map[string]any{"status": string(agentruntime.RuntimeConfigDirectoryRebindMultiConfirmationExpired), "revision": existing.Revision + 1, "updated_at": now})
				if result.Error != nil {
					return result.Error
				}
				return agentruntime.ErrRuntimeConfigDirectoryRebindMultiConfirmationExpired
			}
			saved = existing
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		var activeRows []runtimeConfigDirectoryRebindMultiConfirmationRow
		if err := tx.Where("plan_id = ? AND status = ?", confirmation.PlanID, string(agentruntime.RuntimeConfigDirectoryRebindMultiConfirmationConfirmed)).Order("created_at ASC").Find(&activeRows).Error; err != nil {
			return err
		}
		for _, activeRow := range activeRows {
			active, decodeErr := runtimeConfigDirectoryRebindMultiConfirmationFromRow(activeRow)
			if decodeErr != nil {
				return decodeErr
			}
			if !active.ExpiresAt.After(now) {
				_ = tx.Model(&runtimeConfigDirectoryRebindMultiConfirmationRow{}).Where("id = ? AND revision = ? AND status = ?", active.ID, active.Revision, string(agentruntime.RuntimeConfigDirectoryRebindMultiConfirmationConfirmed)).Updates(map[string]any{"status": string(agentruntime.RuntimeConfigDirectoryRebindMultiConfirmationExpired), "revision": active.Revision + 1, "updated_at": now})
				continue
			}
			return agentruntime.ErrRuntimeConfigDirectoryRebindMultiConfirmationConflict
		}
		row, rowErr := runtimeConfigDirectoryRebindMultiConfirmationToRow(confirmation, now)
		if rowErr != nil {
			return rowErr
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 1 {
			saved = confirmation
			return nil
		}
		if err := tx.Where("id = ?", confirmation.ID).First(&existingRow).Error; err != nil {
			return err
		}
		existing, decodeErr := runtimeConfigDirectoryRebindMultiConfirmationFromRow(existingRow)
		if decodeErr != nil {
			return decodeErr
		}
		if !runtimeConfigDirectoryRebindMultiConfirmationIdentityMatches(existing, confirmation) {
			return agentruntime.ErrRuntimeConfigDirectoryRebindMultiConfirmationConflict
		}
		saved = existing
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindMultiConfirmation{}, err
	}
	return saved, nil
}

func (r *runtimeRepository) GetRuntimeConfigDirectoryRebindMultiConfirmation(ctx context.Context, id string) (agentruntime.RuntimeConfigDirectoryRebindMultiConfirmation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var row runtimeConfigDirectoryRebindMultiConfirmationRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeConfigDirectoryRebindMultiConfirmation{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeConfigDirectoryRebindMultiConfirmation{}, err
	}
	return runtimeConfigDirectoryRebindMultiConfirmationFromRow(row)
}

func runtimeConfigDirectoryRebindMultiApplyChildMatches(parent agentruntime.RuntimeConfigDirectoryRebindMultiApply, child agentruntime.RuntimeConfigDirectoryRebindApply, index int) bool {
	return child.ParentID == parent.ID && child.ID == parent.ChildApplyIDs[index] && child.Destination == parent.Destinations[index] && child.PlanID == parent.PlanID && child.ConfirmationID == parent.ConfirmationID && child.ConfirmationDigest == parent.ConfirmationDigest && child.Source == parent.Source && child.InvocationID == parent.InvocationID && child.BoundaryStatus == parent.BoundaryStatus && child.ConfigSnapshotDigest == parent.ConfigSnapshotDigest && child.SnapshotRevision == parent.SnapshotRevision && child.EventSequence == parent.EventSequence && child.SnapshotDigest == parent.SnapshotDigest && child.IdempotencyKey == parent.IdempotencyKey
}

func runtimeConfigDirectoryRebindMultiApplyChildrenValid(parent agentruntime.RuntimeConfigDirectoryRebindMultiApply, children []agentruntime.RuntimeConfigDirectoryRebindApply) error {
	if len(children) != len(parent.ChildApplyIDs) {
		return agentruntime.ErrRuntimeConfigDirectoryRebindMultiApplyConflict
	}
	for index, child := range children {
		if !runtimeConfigDirectoryRebindMultiApplyChildMatches(parent, child, index) {
			return agentruntime.ErrRuntimeConfigDirectoryRebindMultiApplyConflict
		}
	}
	return nil
}

func (r *runtimeRepository) CommitRuntimeConfigDirectoryRebindMultiApply(ctx context.Context, confirmationID string, parent agentruntime.RuntimeConfigDirectoryRebindMultiApply, children []agentruntime.RuntimeConfigDirectoryRebindApply, now time.Time) (agentruntime.RuntimeConfigDirectoryRebindMultiApply, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	parent, err := parent.Normalize(now)
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindMultiApply{}, err
	}
	confirmationID = strings.TrimSpace(confirmationID)
	if confirmationID == "" || confirmationID != parent.ConfirmationID {
		return agentruntime.RuntimeConfigDirectoryRebindMultiApply{}, agentruntime.ErrRuntimeConfigDirectoryRebindMultiConfirmationConflict
	}
	normalizedChildren := make([]agentruntime.RuntimeConfigDirectoryRebindApply, len(children))
	for index, child := range children {
		normalizedChildren[index], err = child.Normalize(now)
		if err != nil {
			return agentruntime.RuntimeConfigDirectoryRebindMultiApply{}, err
		}
	}
	if err := runtimeConfigDirectoryRebindMultiApplyChildrenValid(parent, normalizedChildren); err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindMultiApply{}, err
	}
	var saved agentruntime.RuntimeConfigDirectoryRebindMultiApply
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var existingParentRow runtimeConfigDirectoryRebindMultiApplyRow
		findErr := tx.Where("id = ?", parent.ID).First(&existingParentRow).Error
		if findErr == nil {
			existingParent, decodeErr := runtimeConfigDirectoryRebindMultiApplyFromRow(existingParentRow)
			if decodeErr != nil {
				return decodeErr
			}
			if !existingParent.MatchesIdentity(parent) {
				return agentruntime.ErrRuntimeConfigDirectoryRebindMultiApplyConflict
			}
			for _, childID := range parent.ChildApplyIDs {
				var childRow runtimeConfigDirectoryRebindApplyRow
				if err := tx.Where("id = ?", childID).First(&childRow).Error; err != nil {
					return agentruntime.ErrRuntimeConfigDirectoryRebindMultiApplyConflict
				}
				child, decodeErr := runtimeConfigDirectoryRebindApplyFromRow(childRow)
				if decodeErr != nil {
					return decodeErr
				}
				index := -1
				for candidate := range parent.ChildApplyIDs {
					if parent.ChildApplyIDs[candidate] == childID {
						index = candidate
						break
					}
				}
				if index < 0 || !runtimeConfigDirectoryRebindMultiApplyChildMatches(parent, child, index) {
					return agentruntime.ErrRuntimeConfigDirectoryRebindMultiApplyConflict
				}
			}
			saved = existingParent
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		var confirmationRow runtimeConfigDirectoryRebindMultiConfirmationRow
		if err := tx.Where("id = ?", confirmationID).First(&confirmationRow).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return err
		}
		confirmation, decodeErr := runtimeConfigDirectoryRebindMultiConfirmationFromRow(confirmationRow)
		if decodeErr != nil {
			return decodeErr
		}
		if confirmation.Status == agentruntime.RuntimeConfigDirectoryRebindMultiConfirmationConfirmed && !confirmation.ExpiresAt.After(now) {
			result := tx.Model(&runtimeConfigDirectoryRebindMultiConfirmationRow{}).Where("id = ? AND revision = ? AND status = ?", confirmation.ID, confirmation.Revision, string(agentruntime.RuntimeConfigDirectoryRebindMultiConfirmationConfirmed)).Updates(map[string]any{"status": string(agentruntime.RuntimeConfigDirectoryRebindMultiConfirmationExpired), "revision": confirmation.Revision + 1, "updated_at": now})
			if result.Error != nil {
				return result.Error
			}
			return agentruntime.ErrRuntimeConfigDirectoryRebindMultiConfirmationExpired
		}
		if confirmation.Status != agentruntime.RuntimeConfigDirectoryRebindMultiConfirmationConfirmed {
			return agentruntime.ErrRuntimeConfigDirectoryRebindMultiConfirmationConflict
		}
		confirmationDigest, digestErr := agentruntime.RuntimeConfigDirectoryRebindMultiConfirmationDigest(confirmation)
		if digestErr != nil || confirmationDigest != parent.ConfirmationDigest || confirmation.PlanID != parent.PlanID || confirmation.InvocationID != parent.InvocationID || confirmation.Source != parent.Source || confirmation.ConfigSnapshotDigest != parent.ConfigSnapshotDigest || confirmation.CapabilitiesDigest != parent.CapabilitiesDigest || len(confirmation.Destinations) != len(parent.Destinations) {
			return agentruntime.ErrRuntimeConfigDirectoryRebindMultiConfirmationConflict
		}
		for index := range parent.Destinations {
			if confirmation.Destinations[index] != parent.Destinations[index] {
				return agentruntime.ErrRuntimeConfigDirectoryRebindMultiConfirmationConflict
			}
		}
		for index, child := range normalizedChildren {
			var existingChild runtimeConfigDirectoryRebindApplyRow
			if err := tx.Where("id = ?", child.ID).First(&existingChild).Error; err == nil {
				return agentruntime.ErrRuntimeConfigDirectoryRebindMultiApplyConflict
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			var pendingCount int64
			if err := tx.Model(&runtimeConfigDirectoryRebindApplyRow{}).Where("plan_id = ? AND destination = ? AND snapshot_revision = ? AND event_sequence = ? AND snapshot_digest = ? AND status <> ?", child.PlanID, child.Destination, child.SnapshotRevision, child.EventSequence, child.SnapshotDigest, string(agentruntime.RuntimeConfigDirectoryRebindApplyFailed)).Count(&pendingCount).Error; err != nil {
				return err
			}
			if pendingCount > 0 {
				return agentruntime.ErrRuntimeConfigDirectoryRebindMultiApplyConflict
			}
			_ = index
		}
		parentRow, rowErr := runtimeConfigDirectoryRebindMultiApplyToRow(parent, now)
		if rowErr != nil {
			return rowErr
		}
		consumedAt := now
		result := tx.Model(&runtimeConfigDirectoryRebindMultiConfirmationRow{}).Where("id = ? AND revision = ? AND status = ?", confirmation.ID, confirmation.Revision, string(agentruntime.RuntimeConfigDirectoryRebindMultiConfirmationConfirmed)).Updates(map[string]any{"status": string(agentruntime.RuntimeConfigDirectoryRebindMultiConfirmationConsumed), "consumed_at": &consumedAt, "revision": confirmation.Revision + 1, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return agentruntime.ErrRuntimeConfigDirectoryRebindMultiConfirmationConflict
		}
		if err := tx.Create(parentRow).Error; err != nil {
			return err
		}
		for _, child := range normalizedChildren {
			row, rowErr := runtimeConfigDirectoryRebindApplyToRow(child, now)
			if rowErr != nil {
				return rowErr
			}
			if err := tx.Create(row).Error; err != nil {
				return err
			}
		}
		saved = parent
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindMultiApply{}, err
	}
	return saved, nil
}

func (r *runtimeRepository) GetRuntimeConfigDirectoryRebindMultiApply(ctx context.Context, id string) (agentruntime.RuntimeConfigDirectoryRebindMultiApply, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var row runtimeConfigDirectoryRebindMultiApplyRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeConfigDirectoryRebindMultiApply{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeConfigDirectoryRebindMultiApply{}, err
	}
	return runtimeConfigDirectoryRebindMultiApplyFromRow(row)
}

func (r *runtimeRepository) ListRuntimeConfigDirectoryRebindMultiApplies(ctx context.Context, planID, invocationID string, status agentruntime.RuntimeConfigDirectoryRebindMultiApplyStatus, limit int) ([]agentruntime.RuntimeConfigDirectoryRebindMultiApply, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	if status != "" && !status.ValidForHTTP() {
		return nil, agentruntime.ErrInvalidRuntimeConfigDirectoryRebindMultiApply
	}
	query := r.db.WithContext(ctx).Model(&runtimeConfigDirectoryRebindMultiApplyRow{})
	if planID = strings.TrimSpace(planID); planID != "" {
		query = query.Where("plan_id = ?", planID)
	}
	if invocationID = strings.TrimSpace(invocationID); invocationID != "" {
		query = query.Where("invocation_id = ?", invocationID)
	}
	if status != "" {
		query = query.Where("status = ?", string(status))
	}
	var rows []runtimeConfigDirectoryRebindMultiApplyRow
	if err := query.Order("updated_at ASC").Order("created_at ASC").Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]agentruntime.RuntimeConfigDirectoryRebindMultiApply, 0, len(rows))
	for _, row := range rows {
		item, err := runtimeConfigDirectoryRebindMultiApplyFromRow(row)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (r *runtimeRepository) ReconcileRuntimeConfigDirectoryRebindMultiApply(ctx context.Context, id string, now time.Time) (agentruntime.RuntimeConfigDirectoryRebindMultiApply, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var result agentruntime.RuntimeConfigDirectoryRebindMultiApply
	var reconcileErr error
	err := withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var row runtimeConfigDirectoryRebindMultiApplyRow
		if err := tx.Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return err
		}
		parent, err := runtimeConfigDirectoryRebindMultiApplyFromRow(row)
		if err != nil {
			return err
		}
		pending, processing, completed, failed := 0, 0, 0, 0
		lastError := ""
		invalid := false
		for index, childID := range parent.ChildApplyIDs {
			var childRow runtimeConfigDirectoryRebindApplyRow
			if err := tx.Where("id = ?", childID).First(&childRow).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					failed++
					invalid = true
					if lastError == "" {
						lastError = "multi rebind child 缺失或 identity 漂移"
					}
					continue
				}
				return err
			}
			child, decodeErr := runtimeConfigDirectoryRebindApplyFromRow(childRow)
			if decodeErr != nil {
				return decodeErr
			}
			if !runtimeConfigDirectoryRebindMultiApplyChildMatches(parent, child, index) {
				failed++
				invalid = true
				if lastError == "" {
					lastError = "multi rebind child 缺失或 identity 漂移"
				}
				continue
			}
			switch child.Status {
			case agentruntime.RuntimeConfigDirectoryRebindApplyQueued:
				pending++
			case agentruntime.RuntimeConfigDirectoryRebindApplyProcessing:
				processing++
			case agentruntime.RuntimeConfigDirectoryRebindApplyCompleted:
				completed++
			case agentruntime.RuntimeConfigDirectoryRebindApplyFailed:
				failed++
				if lastError == "" {
					lastError = child.LastError
				}
			default:
				failed++
				invalid = true
			}
		}
		candidate := parent
		candidate.PendingCount, candidate.ProcessingCount, candidate.CompletedCount, candidate.FailedCount = pending, processing, completed, failed
		candidate.Status = agentruntime.RuntimeConfigDirectoryRebindMultiApplyStatusForCounts(pending, processing, completed, failed)
		if lastError != "" {
			candidate.LastError = lastError
		}
		changed := candidate.Status != parent.Status || candidate.PendingCount != parent.PendingCount || candidate.ProcessingCount != parent.ProcessingCount || candidate.CompletedCount != parent.CompletedCount || candidate.FailedCount != parent.FailedCount || candidate.LastError != parent.LastError
		if changed {
			candidate.Revision++
			candidate.UpdatedAt = now
			if candidate.Status == agentruntime.RuntimeConfigDirectoryRebindMultiApplyCompleted {
				finished := now
				candidate.CompletedAt = &finished
			} else {
				candidate.CompletedAt = nil
			}
			candidate, err = candidate.Normalize(now)
			if err != nil {
				return err
			}
			parentRow, rowErr := runtimeConfigDirectoryRebindMultiApplyToRow(candidate, now)
			if rowErr != nil {
				return rowErr
			}
			update := tx.Model(&runtimeConfigDirectoryRebindMultiApplyRow{}).Where("id = ? AND revision = ?", parent.ID, parent.Revision).Updates(map[string]any{
				"status": parentRow.Status, "pending_count": parentRow.PendingCount, "processing_count": parentRow.ProcessingCount,
				"completed_count": parentRow.CompletedCount, "failed_count": parentRow.FailedCount, "last_error": parentRow.LastError,
				"revision": parentRow.Revision, "updated_at": parentRow.UpdatedAt, "completed_at": parentRow.CompletedAt,
			})
			if update.Error != nil {
				return update.Error
			}
			if update.RowsAffected != 1 {
				return agentruntime.ErrRuntimeConfigDirectoryRebindMultiApplyConflict
			}
			parent = candidate
		}
		result = parent
		if invalid {
			reconcileErr = agentruntime.ErrRuntimeConfigDirectoryRebindMultiApplyConflict
		}
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindMultiApply{}, err
	}
	return result, reconcileErr
}

func (r *runtimeRepository) ReleaseRuntimeConfigDirectoryRebindApply(ctx context.Context, id, owner string, now time.Time) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeConfigDirectoryRebindApplyOwnerLength {
		return false, agentruntime.ErrRuntimeConfigDirectoryRebindApplyConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var changed bool
	err := withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var row runtimeConfigDirectoryRebindApplyRow
		if err := tx.Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return err
		}
		if row.Status != string(agentruntime.RuntimeConfigDirectoryRebindApplyProcessing) || strings.TrimSpace(row.LeaseOwner) != owner || row.LeaseExpiresAt == nil || !row.LeaseExpiresAt.After(now) {
			return nil
		}
		attempt := row.Attempt
		if attempt > 0 {
			attempt--
		}
		update := tx.Model(&runtimeConfigDirectoryRebindApplyRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", row.ID, row.Revision, string(agentruntime.RuntimeConfigDirectoryRebindApplyProcessing), owner, now).Updates(map[string]any{
			"status": string(agentruntime.RuntimeConfigDirectoryRebindApplyQueued), "attempt": attempt, "available_at": now,
			"lease_owner": "", "lease_expires_at": nil, "revision": row.Revision + 1, "updated_at": now,
		})
		if update.Error != nil {
			return update.Error
		}
		changed = update.RowsAffected == 1
		return nil
	})
	return changed, err
}
