package sqlite

import (
	"context"
	"errors"
	"strings"
	"time"

	agentruntime "Abot/internal/agent/runtime"
	"gorm.io/gorm"
)

// AbortRuntimeEventDelivery compensates a prepared event transaction. The
// status update is guarded by the current state so concurrent abort/commit
// calls have one winner and a committed event is never hidden again.
func (r *runtimeRepository) AbortRuntimeEventDelivery(ctx context.Context, envelope agentruntime.RuntimeEventDeliveryEnvelope) (bool, error) {
	normalized, err := agentruntime.NormalizeRuntimeEventDeliveryEnvelope(envelope)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	duplicate := false
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var row runtimeEventDeliveryTransactionRow
		if findErr := tx.Where("delivery_id = ?", normalized.DeliveryID).First(&row).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		transaction, decodeErr := runtimeEventDeliveryTransactionFromRow(row)
		if decodeErr != nil {
			return decodeErr
		}
		if !transaction.Matches(normalized) {
			return agentruntime.ErrConflict
		}
		switch transaction.Status {
		case agentruntime.RuntimeEventDeliveryTransactionAborted:
			duplicate = true
			return nil
		case agentruntime.RuntimeEventDeliveryTransactionCommitted:
			return agentruntime.ErrConflict
		case agentruntime.RuntimeEventDeliveryTransactionPrepared:
			result := tx.Model(&runtimeEventDeliveryTransactionRow{}).Where("delivery_id = ? AND status = ?", transaction.DeliveryID, string(agentruntime.RuntimeEventDeliveryTransactionPrepared)).Updates(map[string]any{
				"status": string(agentruntime.RuntimeEventDeliveryTransactionAborted), "updated_at": now,
			})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return agentruntime.ErrConflict
			}
			return nil
		default:
			return agentruntime.ErrConflict
		}
	})
	return duplicate, err
}

// AbortRuntimeCheckpointDelivery compensates a prepared checkpoint
// transaction without touching the visible checkpoint inbox.
func (r *runtimeRepository) AbortRuntimeCheckpointDelivery(ctx context.Context, envelope agentruntime.RuntimeCheckpointDeliveryEnvelope, projection agentruntime.RuntimeCheckpointProjection) (bool, error) {
	normalized, normalizedProjection, err := agentruntime.NormalizeRuntimeCheckpointDeliveryPair(envelope, projection)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	duplicate := false
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var row runtimeCheckpointDeliveryTransactionRow
		if findErr := tx.Where("delivery_id = ?", normalized.DeliveryID).First(&row).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		transaction, decodeErr := runtimeCheckpointDeliveryTransactionFromRow(row)
		if decodeErr != nil {
			return decodeErr
		}
		if !transaction.Matches(normalized, normalizedProjection) {
			return agentruntime.ErrConflict
		}
		switch transaction.Status {
		case agentruntime.RuntimeCheckpointDeliveryTransactionAborted:
			duplicate = true
			return nil
		case agentruntime.RuntimeCheckpointDeliveryTransactionCommitted:
			return agentruntime.ErrConflict
		case agentruntime.RuntimeCheckpointDeliveryTransactionPrepared:
			result := tx.Model(&runtimeCheckpointDeliveryTransactionRow{}).Where("delivery_id = ? AND status = ?", transaction.DeliveryID, string(agentruntime.RuntimeCheckpointDeliveryTransactionPrepared)).Updates(map[string]any{
				"status": string(agentruntime.RuntimeCheckpointDeliveryTransactionAborted), "updated_at": now,
			})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return agentruntime.ErrConflict
			}
			return nil
		default:
			return agentruntime.ErrConflict
		}
	})
	return duplicate, err
}

// AbortRuntimeConfigDelivery compensates a prepared configuration lock. The
// stored projection is used only to validate immutable identity; it is never
// published by this operation.
func (r *runtimeRepository) AbortRuntimeConfigDelivery(ctx context.Context, envelope agentruntime.RuntimeConfigDeliveryEnvelope) (bool, error) {
	normalized, err := agentruntime.NormalizeRuntimeConfigDeliveryEnvelope(envelope)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	duplicate := false
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var row runtimeConfigDeliveryTransactionRow
		if findErr := tx.Where("delivery_id = ?", normalized.DeliveryID).First(&row).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		transaction, decodeErr := runtimeConfigDeliveryTransactionFromRow(row)
		if decodeErr != nil {
			return decodeErr
		}
		if !transaction.Matches(normalized, transaction.Projection) {
			return agentruntime.ErrConflict
		}
		switch transaction.Status {
		case agentruntime.RuntimeConfigDeliveryTransactionAborted:
			duplicate = true
			return nil
		case agentruntime.RuntimeConfigDeliveryTransactionCommitted:
			return agentruntime.ErrConflict
		case agentruntime.RuntimeConfigDeliveryTransactionPrepared:
			result := tx.Model(&runtimeConfigDeliveryTransactionRow{}).Where("delivery_id = ? AND status = ?", transaction.DeliveryID, string(agentruntime.RuntimeConfigDeliveryTransactionPrepared)).Updates(map[string]any{
				"status": string(agentruntime.RuntimeConfigDeliveryTransactionAborted), "updated_at": now,
			})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return agentruntime.ErrConflict
			}
			return nil
		default:
			return agentruntime.ErrConflict
		}
	})
	return duplicate, err
}

// AbortRuntimeApprovalRejectionDelivery compensates a prepared rejection
// intent. It is deliberately separate from the Workspace-aware commit path.
func (r *runtimeRepository) AbortRuntimeApprovalRejectionDelivery(ctx context.Context, envelope agentruntime.RuntimeApprovalRejectionDeliveryEnvelope) (bool, error) {
	normalized, err := agentruntime.NormalizeRuntimeApprovalRejectionDeliveryEnvelope(envelope)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	duplicate := false
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var row runtimeApprovalRejectionDeliveryTransactionRow
		if findErr := tx.Where("delivery_id = ?", strings.TrimSpace(normalized.DeliveryID)).First(&row).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		transaction := runtimeApprovalRejectionDeliveryTransactionFromRow(row)
		if !transaction.Matches(normalized) {
			return agentruntime.ErrConflict
		}
		switch transaction.Status {
		case agentruntime.RuntimeApprovalRejectionDeliveryTransactionAborted:
			duplicate = true
			return nil
		case agentruntime.RuntimeApprovalRejectionDeliveryTransactionCommitted:
			return agentruntime.ErrConflict
		case agentruntime.RuntimeApprovalRejectionDeliveryTransactionPrepared:
			result := tx.Model(&runtimeApprovalRejectionDeliveryTransactionRow{}).Where("delivery_id = ? AND status = ?", transaction.DeliveryID, string(agentruntime.RuntimeApprovalRejectionDeliveryTransactionPrepared)).Updates(map[string]any{
				"status": string(agentruntime.RuntimeApprovalRejectionDeliveryTransactionAborted), "updated_at": now,
			})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return agentruntime.ErrConflict
			}
			return nil
		default:
			return agentruntime.ErrConflict
		}
	})
	return duplicate, err
}

var _ agentruntime.RuntimeEventDeliveryAbortableTransactionRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeCheckpointDeliveryAbortableTransactionRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigDeliveryAbortableTransactionRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeApprovalRejectionDeliveryAbortableTransactionRepository = (*runtimeRepository)(nil)
