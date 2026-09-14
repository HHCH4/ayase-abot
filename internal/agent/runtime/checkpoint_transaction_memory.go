package runtime

import (
	"context"
	"time"
)

func (r *MemoryRepository) acceptRuntimeCheckpointDeliveryLocked(normalized RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection, receivedAt time.Time) (bool, error) {
	if existing, ok := r.checkpointInbox[normalized.InvocationID]; ok {
		if existing.Matches(normalized, projection) {
			return true, nil
		}
		if existing.Source != normalized.Source || existing.Destination != normalized.Destination {
			return false, ErrConflict
		}
		if normalized.SnapshotRevision < existing.SnapshotRevision || (normalized.SnapshotRevision == existing.SnapshotRevision && normalized.EventSequence <= existing.EventSequence) {
			if normalized.SnapshotRevision < existing.SnapshotRevision {
				return false, ErrRuntimeCheckpointDeliveryStale
			}
			return false, ErrConflict
		}
		if normalized.EventSequence < existing.EventSequence {
			return false, ErrRuntimeCheckpointDeliveryStale
		}
	}
	if r.checkpointInbox == nil {
		r.checkpointInbox = make(map[string]RuntimeCheckpointDeliveryInboxRecord)
	}
	r.checkpointInbox[normalized.InvocationID] = runtimeCheckpointDeliveryInboxRecord(normalized, projection, receivedAt)
	return false, nil
}

// PrepareRuntimeCheckpointDelivery durably stages a checkpoint projection at
// the destination. It is idempotent by DeliveryID and intentionally does not
// update the visible checkpoint inbox until CommitRuntimeCheckpointDelivery.
func (r *MemoryRepository) PrepareRuntimeCheckpointDelivery(_ context.Context, envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection) (bool, error) {
	normalized, normalizedProjection, err := normalizeRuntimeCheckpointDeliveryPair(envelope, projection)
	if err != nil {
		return false, err
	}
	now := nowUTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.checkpointTxns == nil {
		r.checkpointTxns = make(map[string]RuntimeCheckpointDeliveryTransaction)
	}
	if existing, ok := r.checkpointTxns[normalized.DeliveryID]; ok {
		if !existing.Matches(normalized, normalizedProjection) {
			return false, ErrConflict
		}
		if existing.Status == RuntimeCheckpointDeliveryTransactionCommitted {
			return true, nil
		}
		if existing.Status != RuntimeCheckpointDeliveryTransactionPrepared {
			return false, ErrConflict
		}
		if existing.ExpiresAt.After(now) {
			return true, nil
		}
		// An expired prepare can be refreshed only with the same immutable
		// DeliveryID/projection. A fresh signed envelope becomes the commit
		// source for this new prepare window.
	}
	prepared, err := newRuntimeCheckpointDeliveryTransaction(normalized, normalizedProjection, now)
	if err != nil {
		return false, err
	}
	r.checkpointTxns[normalized.DeliveryID] = cloneRuntimeCheckpointDeliveryTransaction(prepared)
	return false, nil
}

// CommitRuntimeCheckpointDelivery atomically publishes a prepared projection
// to the destination inbox. The stored prepare envelope is used for the final
// inbox record so refreshed commit timestamps/signatures remain idempotent.
func (r *MemoryRepository) CommitRuntimeCheckpointDelivery(_ context.Context, envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection) (bool, error) {
	normalized, normalizedProjection, err := normalizeRuntimeCheckpointDeliveryPair(envelope, projection)
	if err != nil {
		return false, err
	}
	now := nowUTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	transaction, ok := r.checkpointTxns[normalized.DeliveryID]
	if !ok {
		return false, ErrNotFound
	}
	if !transaction.Matches(normalized, normalizedProjection) {
		return false, ErrConflict
	}
	if transaction.Status == RuntimeCheckpointDeliveryTransactionCommitted {
		return true, nil
	}
	if transaction.Status != RuntimeCheckpointDeliveryTransactionPrepared {
		return false, ErrConflict
	}
	if !transaction.ExpiresAt.After(now) {
		return false, ErrRuntimeCheckpointDeliveryStale
	}
	preparedEnvelope := RuntimeCheckpointDeliveryEnvelope{
		Version: transaction.Version, Source: transaction.Source, Destination: transaction.Destination,
		DeliveryID: transaction.DeliveryID, InvocationID: transaction.InvocationID,
		SnapshotRevision: transaction.SnapshotRevision, EventSequence: transaction.EventSequence,
		SnapshotDigest: transaction.SnapshotDigest, Timestamp: transaction.Timestamp, Signature: transaction.Signature,
	}
	duplicate, err := r.acceptRuntimeCheckpointDeliveryLocked(preparedEnvelope, transaction.Projection, now)
	if err != nil {
		return false, err
	}
	transaction.Status = RuntimeCheckpointDeliveryTransactionCommitted
	transaction.UpdatedAt = now
	r.checkpointTxns[normalized.DeliveryID] = cloneRuntimeCheckpointDeliveryTransaction(transaction)
	return duplicate, nil
}

// GetRuntimeCheckpointDeliveryTransaction is a diagnostic/test hook. It
// returns a defensive copy and never exposes the projection through generic
// JSON because the projection field is intentionally tagged json:"-".
func (r *MemoryRepository) GetRuntimeCheckpointDeliveryTransaction(_ context.Context, deliveryID string) (RuntimeCheckpointDeliveryTransaction, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.checkpointTxns[deliveryID]
	if !ok {
		return RuntimeCheckpointDeliveryTransaction{}, ErrNotFound
	}
	return cloneRuntimeCheckpointDeliveryTransaction(item), nil
}

var _ RuntimeCheckpointDeliveryTransactionRepository = (*MemoryRepository)(nil)
