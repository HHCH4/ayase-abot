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

// runtimeDeliveryGroupFenceRow is the destination/source durable monotonic
// epoch ledger. It contains no payload and is kept separate from group
// transactions so a newer group can fence an older one.
type runtimeDeliveryGroupFenceRow struct {
	FenceID      string `gorm:"primaryKey;size:220"`
	Source       string `gorm:"index;size:160;not null"`
	Destination  string `gorm:"index;size:160;not null"`
	InvocationID string `gorm:"index;size:512;not null"`
	GroupID      string `gorm:"index;size:220;not null"`
	Revision     int64  `gorm:"not null"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (runtimeDeliveryGroupFenceRow) TableName() string {
	return "abot_agent_runtime_delivery_group_fences"
}

func runtimeDeliveryGroupFenceFromRow(row runtimeDeliveryGroupFenceRow) (agentruntime.RuntimeDeliveryGroupFence, error) {
	item, err := agentruntime.NewRuntimeDeliveryGroupFence(row.Source, row.Destination, row.InvocationID, row.GroupID, row.Revision, row.UpdatedAt)
	if err != nil {
		return agentruntime.RuntimeDeliveryGroupFence{}, err
	}
	item.CreatedAt = row.CreatedAt.UTC()
	item.UpdatedAt = row.UpdatedAt.UTC()
	return item, nil
}

func runtimeDeliveryGroupFenceToRow(item agentruntime.RuntimeDeliveryGroupFence) (*runtimeDeliveryGroupFenceRow, error) {
	normalized, err := item.Normalize(time.Now().UTC())
	if err != nil {
		return nil, err
	}
	return &runtimeDeliveryGroupFenceRow{FenceID: normalized.FenceID, Source: normalized.Source, Destination: normalized.Destination, InvocationID: normalized.InvocationID, GroupID: normalized.GroupID, Revision: normalized.Revision, CreatedAt: normalized.CreatedAt, UpdatedAt: normalized.UpdatedAt}, nil
}

// ensureRuntimeDeliveryGroupFenceTx applies the monotonic fence check in the
// caller's transaction. Prepare may create/advance the row; Commit and Abort
// call this only after their group transaction was found, so an unknown
// group cannot claim a revision.
func ensureRuntimeDeliveryGroupFenceTx(tx *gorm.DB, envelope agentruntime.RuntimeDeliveryGroupEnvelope, now time.Time, allowCreate bool) error {
	if envelope.FenceRevision == 0 {
		return nil
	}
	fenceID := agentruntime.RuntimeDeliveryGroupFenceID(envelope.Source, envelope.Destination, envelope.InvocationID)
	if envelope.FenceID != fenceID {
		return agentruntime.ErrInvalidRuntimeDeliveryFence
	}
	var row runtimeDeliveryGroupFenceRow
	findErr := tx.Where("fence_id = ?", fenceID).First(&row).Error
	if errors.Is(findErr, gorm.ErrRecordNotFound) {
		if !allowCreate {
			return agentruntime.ErrRuntimeDeliveryFenceConflict
		}
		fence, err := agentruntime.NewRuntimeDeliveryGroupFence(envelope.Source, envelope.Destination, envelope.InvocationID, envelope.GroupID, envelope.FenceRevision, now)
		if err != nil {
			return err
		}
		created, err := runtimeDeliveryGroupFenceToRow(fence)
		if err != nil {
			return err
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(created)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 1 {
			return nil
		}
		if err := tx.Where("fence_id = ?", fenceID).First(&row).Error; err != nil {
			return err
		}
	} else if findErr != nil {
		return findErr
	}
	if row.Source != envelope.Source || row.Destination != envelope.Destination || row.InvocationID != envelope.InvocationID {
		return agentruntime.ErrRuntimeDeliveryFenceConflict
	}
	switch {
	case envelope.FenceRevision < row.Revision:
		return agentruntime.ErrRuntimeDeliveryFenceStale
	case envelope.FenceRevision == row.Revision:
		if row.GroupID != envelope.GroupID {
			return agentruntime.ErrRuntimeDeliveryFenceConflict
		}
		return nil
	default:
		fence, err := agentruntime.NewRuntimeDeliveryGroupFence(envelope.Source, envelope.Destination, envelope.InvocationID, envelope.GroupID, envelope.FenceRevision, now)
		if err != nil {
			return err
		}
		updated, err := runtimeDeliveryGroupFenceToRow(fence)
		if err != nil {
			return err
		}
		updated.CreatedAt = row.CreatedAt
		result := tx.Model(&runtimeDeliveryGroupFenceRow{}).Where("fence_id = ? AND revision = ?", fenceID, row.Revision).Updates(map[string]any{"group_id": updated.GroupID, "revision": updated.Revision, "updated_at": updated.UpdatedAt})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return agentruntime.ErrRuntimeDeliveryFenceConflict
		}
		return nil
	}
}

func (r *runtimeRepository) IssueRuntimeDeliveryGroupFence(ctx context.Context, source, destination, invocationID, groupID string, now time.Time) (agentruntime.RuntimeDeliveryGroupFence, error) {
	source = strings.TrimSpace(source)
	destination = strings.TrimSpace(destination)
	invocationID = strings.TrimSpace(invocationID)
	groupID = strings.TrimSpace(groupID)
	if source == "" || destination == "" || invocationID == "" || groupID == "" {
		return agentruntime.RuntimeDeliveryGroupFence{}, agentruntime.ErrInvalidRuntimeDeliveryFence
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	fenceID := agentruntime.RuntimeDeliveryGroupFenceID(source, destination, invocationID)
	var result agentruntime.RuntimeDeliveryGroupFence
	err := withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var row runtimeDeliveryGroupFenceRow
		findErr := tx.Where("fence_id = ?", fenceID).First(&row).Error
		if errors.Is(findErr, gorm.ErrRecordNotFound) {
			fence, err := agentruntime.NewRuntimeDeliveryGroupFence(source, destination, invocationID, groupID, 1, now)
			if err != nil {
				return err
			}
			created, err := runtimeDeliveryGroupFenceToRow(fence)
			if err != nil {
				return err
			}
			createResult := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(created)
			if createResult.Error != nil {
				return createResult.Error
			}
			if createResult.RowsAffected == 1 {
				result = fence
				return nil
			}
			// Another issuer won the insert. Re-read its row and apply the
			// same idempotent/new-group logic below instead of surfacing a
			// uniqueness error to the caller.
			if err := tx.Where("fence_id = ?", fenceID).First(&row).Error; err != nil {
				return err
			}
			findErr = nil
		}
		if findErr != nil {
			return findErr
		}
		if row.Source != source || row.Destination != destination || row.InvocationID != invocationID {
			return agentruntime.ErrRuntimeDeliveryFenceConflict
		}
		if row.GroupID == groupID {
			var err error
			result, err = runtimeDeliveryGroupFenceFromRow(row)
			return err
		}
		if row.Revision >= agentruntime.MaxRuntimeDeliveryFenceRevision {
			return agentruntime.ErrRuntimeDeliveryFenceConflict
		}
		fence, err := agentruntime.NewRuntimeDeliveryGroupFence(source, destination, invocationID, groupID, row.Revision+1, now)
		if err != nil {
			return err
		}
		updated, err := runtimeDeliveryGroupFenceToRow(fence)
		if err != nil {
			return err
		}
		updated.CreatedAt = row.CreatedAt
		update := tx.Model(&runtimeDeliveryGroupFenceRow{}).Where("fence_id = ? AND revision = ?", fenceID, row.Revision).Updates(map[string]any{"group_id": updated.GroupID, "revision": updated.Revision, "updated_at": updated.UpdatedAt})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return agentruntime.ErrRuntimeDeliveryFenceConflict
		}
		result = fence
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeDeliveryGroupFence{}, err
	}
	return result, nil
}

func (r *runtimeRepository) GetRuntimeDeliveryGroupFence(ctx context.Context, fenceID string) (agentruntime.RuntimeDeliveryGroupFence, error) {
	var row runtimeDeliveryGroupFenceRow
	if err := r.db.WithContext(ctx).Where("fence_id = ?", strings.TrimSpace(fenceID)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeDeliveryGroupFence{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeDeliveryGroupFence{}, err
	}
	return runtimeDeliveryGroupFenceFromRow(row)
}

func (r *runtimeRepository) BindRuntimeDeliveryGroupFence(ctx context.Context, groupID string, expectedRevision int64, fence agentruntime.RuntimeDeliveryGroupFence, now time.Time) (agentruntime.RuntimeDeliveryGroup, error) {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" || expectedRevision <= 0 {
		return agentruntime.RuntimeDeliveryGroup{}, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var result agentruntime.RuntimeDeliveryGroup
	err := withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var groupRow runtimeDeliveryGroupRow
		if err := tx.Where("id = ?", groupID).First(&groupRow).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return err
		}
		group, err := runtimeDeliveryGroupFromRow(groupRow)
		if err != nil {
			return err
		}
		if group.Revision != expectedRevision {
			return agentruntime.ErrConflict
		}
		if group.FenceID != "" {
			if group.FenceID == strings.TrimSpace(fence.FenceID) && group.FenceRevision == fence.Revision {
				result = group
				return nil
			}
			return agentruntime.ErrRuntimeDeliveryFenceConflict
		}
		normalizedFence, err := fence.Normalize(now)
		if err != nil {
			return err
		}
		if normalizedFence.GroupID != group.ID || normalizedFence.Source != group.Source || normalizedFence.Destination != group.Destination || normalizedFence.InvocationID != group.InvocationID {
			return agentruntime.ErrRuntimeDeliveryFenceConflict
		}
		var fenceRow runtimeDeliveryGroupFenceRow
		if err := tx.Where("fence_id = ?", normalizedFence.FenceID).First(&fenceRow).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return agentruntime.ErrRuntimeDeliveryFenceConflict
			}
			return err
		}
		storedFence, err := runtimeDeliveryGroupFenceFromRow(fenceRow)
		if err != nil {
			return err
		}
		if !storedFence.MatchesIdentity(normalizedFence) {
			return agentruntime.ErrRuntimeDeliveryFenceConflict
		}
		bound, err := agentruntime.BindRuntimeDeliveryGroupFence(group, normalizedFence, now)
		if err != nil {
			return err
		}
		bound.Revision = group.Revision + 1
		bound.UpdatedAt = now
		row, err := runtimeDeliveryGroupToRow(bound)
		if err != nil {
			return err
		}
		updates := map[string]any{"fence_id": row.FenceID, "fence_revision": row.FenceRevision, "revision": row.Revision, "updated_at": row.UpdatedAt}
		updated := tx.Model(&runtimeDeliveryGroupRow{}).Where("id = ? AND revision = ?", group.ID, expectedRevision).Updates(updates)
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return agentruntime.ErrConflict
		}
		result = bound
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeDeliveryGroup{}, err
	}
	return result, nil
}

var _ agentruntime.RuntimeDeliveryGroupFenceIssuer = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeDeliveryGroupFenceBinder = (*runtimeRepository)(nil)
