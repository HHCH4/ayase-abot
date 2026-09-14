package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	agentruntime "Abot/internal/agent/runtime"
	"gorm.io/gorm"
)

// runtimeCheckpointDeliveryTransactionRow is the destination-side prepare
// ledger. It intentionally duplicates only bounded envelope metadata and the
// metadata-only projection; no source event body is copied.
type runtimeCheckpointDeliveryTransactionRow struct {
	Version          int    `gorm:"not null"`
	DeliveryID       string `gorm:"primaryKey;size:220"`
	Source           string `gorm:"index;size:160;not null"`
	Destination      string `gorm:"index;size:160;not null"`
	InvocationID     string `gorm:"index;size:512;not null"`
	SnapshotRevision int64  `gorm:"not null"`
	EventSequence    int64  `gorm:"not null"`
	SnapshotDigest   string `gorm:"size:64;not null"`
	Signature        string `gorm:"size:64;not null"`
	Timestamp        time.Time
	ProjectionJSON   string `gorm:"type:text;not null"`
	Status           string `gorm:"index;size:32;not null"`
	ExpiresAt        time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func (runtimeCheckpointDeliveryTransactionRow) TableName() string {
	return "abot_agent_checkpoint_delivery_transactions"
}

func runtimeCheckpointDeliveryTransactionFromRow(row runtimeCheckpointDeliveryTransactionRow) (agentruntime.RuntimeCheckpointDeliveryTransaction, error) {
	var projection agentruntime.RuntimeCheckpointProjection
	if err := json.Unmarshal([]byte(row.ProjectionJSON), &projection); err != nil {
		return agentruntime.RuntimeCheckpointDeliveryTransaction{}, err
	}
	return agentruntime.RuntimeCheckpointDeliveryTransaction{
		Version: row.Version, DeliveryID: row.DeliveryID, Source: row.Source, Destination: row.Destination,
		InvocationID: row.InvocationID, SnapshotRevision: row.SnapshotRevision, EventSequence: row.EventSequence,
		SnapshotDigest: row.SnapshotDigest, Signature: row.Signature, Timestamp: row.Timestamp,
		Projection: projection, Status: agentruntime.RuntimeCheckpointDeliveryTransactionStatus(row.Status),
		ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

func runtimeCheckpointDeliveryTransactionToRow(item agentruntime.RuntimeCheckpointDeliveryTransaction) (*runtimeCheckpointDeliveryTransactionRow, error) {
	encoded, err := json.Marshal(item.Projection)
	if err != nil {
		return nil, err
	}
	if len(encoded) > agentruntime.MaxRuntimeCheckpointDeliveryProjectionBytes {
		return nil, agentruntime.ErrInvalidRuntimeCheckpointDelivery
	}
	return &runtimeCheckpointDeliveryTransactionRow{
		Version: item.Version, DeliveryID: item.DeliveryID, Source: item.Source, Destination: item.Destination,
		InvocationID: item.InvocationID, SnapshotRevision: item.SnapshotRevision, EventSequence: item.EventSequence,
		SnapshotDigest: item.SnapshotDigest, Signature: item.Signature, Timestamp: item.Timestamp,
		ProjectionJSON: string(encoded), Status: string(item.Status), ExpiresAt: item.ExpiresAt,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}, nil
}

// acceptRuntimeCheckpointDeliveryTx is the transaction-local form of the
// normal monotonic inbox update. It is shared by direct delivery and the
// prepare/commit protocol so an old remote worker cannot roll back a newer
// projection.
func acceptRuntimeCheckpointDeliveryTx(tx *gorm.DB, normalized agentruntime.RuntimeCheckpointDeliveryEnvelope, projection agentruntime.RuntimeCheckpointProjection, receivedAt time.Time) (bool, error) {
	record := agentruntime.RuntimeCheckpointDeliveryInboxRecordFromEnvelope(normalized, projection, receivedAt)
	row, err := runtimeCheckpointDeliveryInboxToRow(record)
	if err != nil {
		return false, err
	}
	var existing runtimeCheckpointDeliveryInboxRow
	findErr := tx.Where("invocation_id = ?", normalized.InvocationID).First(&existing).Error
	if findErr == nil {
		existingRecord, decodeErr := runtimeCheckpointDeliveryInboxFromRow(existing)
		if decodeErr != nil {
			return false, decodeErr
		}
		if existingRecord.Matches(normalized, projection) {
			return true, nil
		}
		if existing.Source != normalized.Source || existing.Destination != normalized.Destination {
			return false, agentruntime.ErrConflict
		}
		if normalized.SnapshotRevision < existing.SnapshotRevision {
			return false, agentruntime.ErrRuntimeCheckpointDeliveryStale
		}
		if normalized.SnapshotRevision == existing.SnapshotRevision {
			return false, agentruntime.ErrConflict
		}
		if normalized.EventSequence < existing.EventSequence {
			return false, agentruntime.ErrRuntimeCheckpointDeliveryStale
		}
		if err := tx.Model(&runtimeCheckpointDeliveryInboxRow{}).Where("invocation_id = ? AND snapshot_revision = ?", normalized.InvocationID, existing.SnapshotRevision).Updates(map[string]any{
			"version": normalized.Version, "delivery_id": normalized.DeliveryID, "source": normalized.Source, "destination": normalized.Destination,
			"snapshot_revision": normalized.SnapshotRevision, "event_sequence": normalized.EventSequence, "snapshot_digest": normalized.SnapshotDigest,
			"signature": normalized.Signature, "projection_json": row.ProjectionJSON, "received_at": row.ReceivedAt,
		}).Error; err != nil {
			return false, err
		}
		return false, nil
	}
	if !errors.Is(findErr, gorm.ErrRecordNotFound) {
		return false, findErr
	}
	return false, tx.Create(row).Error
}

func (r *runtimeRepository) PrepareRuntimeCheckpointDelivery(ctx context.Context, envelope agentruntime.RuntimeCheckpointDeliveryEnvelope, projection agentruntime.RuntimeCheckpointProjection) (bool, error) {
	normalized, normalizedProjection, err := agentruntime.NormalizeRuntimeCheckpointDeliveryPair(envelope, projection)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	duplicate := false
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existingRow runtimeCheckpointDeliveryTransactionRow
		findErr := tx.Where("delivery_id = ?", normalized.DeliveryID).First(&existingRow).Error
		if findErr == nil {
			existing, decodeErr := runtimeCheckpointDeliveryTransactionFromRow(existingRow)
			if decodeErr != nil {
				return decodeErr
			}
			if !existing.Matches(normalized, normalizedProjection) {
				return agentruntime.ErrConflict
			}
			if existing.Status == agentruntime.RuntimeCheckpointDeliveryTransactionCommitted {
				duplicate = true
				return nil
			}
			if existing.Status != agentruntime.RuntimeCheckpointDeliveryTransactionPrepared {
				return agentruntime.ErrConflict
			}
			if existing.ExpiresAt.After(now) {
				duplicate = true
				return nil
			}
		}
		prepared, createErr := agentruntime.NewRuntimeCheckpointDeliveryTransaction(normalized, normalizedProjection, now)
		if createErr != nil {
			return createErr
		}
		row, rowErr := runtimeCheckpointDeliveryTransactionToRow(prepared)
		if rowErr != nil {
			return rowErr
		}
		if findErr == nil {
			return tx.Model(&runtimeCheckpointDeliveryTransactionRow{}).Where("delivery_id = ?", normalized.DeliveryID).Updates(map[string]any{
				"version": row.Version, "source": row.Source, "destination": row.Destination, "invocation_id": row.InvocationID,
				"snapshot_revision": row.SnapshotRevision, "event_sequence": row.EventSequence, "snapshot_digest": row.SnapshotDigest,
				"signature": row.Signature, "timestamp": row.Timestamp, "projection_json": row.ProjectionJSON,
				"status": row.Status, "expires_at": row.ExpiresAt, "created_at": row.CreatedAt, "updated_at": row.UpdatedAt,
			}).Error
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		return tx.Create(row).Error
	})
	return duplicate, err
}

func (r *runtimeRepository) CommitRuntimeCheckpointDelivery(ctx context.Context, envelope agentruntime.RuntimeCheckpointDeliveryEnvelope, projection agentruntime.RuntimeCheckpointProjection) (bool, error) {
	normalized, normalizedProjection, err := agentruntime.NormalizeRuntimeCheckpointDeliveryPair(envelope, projection)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	duplicate := false
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
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
		if transaction.Status == agentruntime.RuntimeCheckpointDeliveryTransactionCommitted {
			duplicate = true
			return nil
		}
		if transaction.Status != agentruntime.RuntimeCheckpointDeliveryTransactionPrepared {
			return agentruntime.ErrConflict
		}
		if !transaction.ExpiresAt.After(now) {
			return agentruntime.ErrRuntimeCheckpointDeliveryStale
		}
		preparedEnvelope := agentruntime.RuntimeCheckpointDeliveryEnvelope{
			Version: transaction.Version, Source: transaction.Source, Destination: transaction.Destination,
			DeliveryID: transaction.DeliveryID, InvocationID: transaction.InvocationID,
			SnapshotRevision: transaction.SnapshotRevision, EventSequence: transaction.EventSequence,
			SnapshotDigest: transaction.SnapshotDigest, Timestamp: transaction.Timestamp, Signature: transaction.Signature,
		}
		var acceptErr error
		duplicate, acceptErr = acceptRuntimeCheckpointDeliveryTx(tx, preparedEnvelope, transaction.Projection, now)
		if acceptErr != nil {
			return acceptErr
		}
		result := tx.Model(&runtimeCheckpointDeliveryTransactionRow{}).Where("delivery_id = ? AND status = ?", transaction.DeliveryID, string(agentruntime.RuntimeCheckpointDeliveryTransactionPrepared)).Updates(map[string]any{
			"status": string(agentruntime.RuntimeCheckpointDeliveryTransactionCommitted), "updated_at": now,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return agentruntime.ErrConflict
		}
		return nil
	})
	return duplicate, err
}

func (r *runtimeRepository) GetRuntimeCheckpointDeliveryTransaction(ctx context.Context, deliveryID string) (agentruntime.RuntimeCheckpointDeliveryTransaction, error) {
	var row runtimeCheckpointDeliveryTransactionRow
	if err := r.db.WithContext(ctx).Where("delivery_id = ?", strings.TrimSpace(deliveryID)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeCheckpointDeliveryTransaction{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeCheckpointDeliveryTransaction{}, err
	}
	item, err := runtimeCheckpointDeliveryTransactionFromRow(row)
	if err != nil {
		return agentruntime.RuntimeCheckpointDeliveryTransaction{}, err
	}
	return item, nil
}

var _ agentruntime.RuntimeCheckpointDeliveryTransactionRepository = (*runtimeRepository)(nil)
