package runtime

import (
	"context"
	"strings"
)

// AbortRuntimeEventDelivery releases a prepared event transaction without
// publishing it to the destination inbox. The transition is idempotent for an
// already-aborted transaction and refuses to undo a committed event.
func (r *MemoryRepository) AbortRuntimeEventDelivery(_ context.Context, envelope RuntimeEventDeliveryEnvelope) (bool, error) {
	normalized, err := NormalizeRuntimeEventDeliveryEnvelope(envelope)
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	transaction, ok := r.eventTxns[normalized.DeliveryID]
	if !ok {
		return false, ErrNotFound
	}
	if !transaction.Matches(normalized) {
		return false, ErrConflict
	}
	switch transaction.Status {
	case RuntimeEventDeliveryTransactionAborted:
		return true, nil
	case RuntimeEventDeliveryTransactionCommitted:
		return false, ErrConflict
	case RuntimeEventDeliveryTransactionPrepared:
		transaction.Status = RuntimeEventDeliveryTransactionAborted
		transaction.UpdatedAt = nowUTC()
		r.eventTxns[normalized.DeliveryID] = cloneRuntimeEventDeliveryTransaction(transaction)
		return false, nil
	default:
		return false, ErrConflict
	}
}

// AbortRuntimeCheckpointDelivery releases a prepared checkpoint transaction
// without changing the visible checkpoint inbox.
func (r *MemoryRepository) AbortRuntimeCheckpointDelivery(_ context.Context, envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection) (bool, error) {
	normalized, normalizedProjection, err := normalizeRuntimeCheckpointDeliveryPair(envelope, projection)
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	transaction, ok := r.checkpointTxns[normalized.DeliveryID]
	if !ok {
		return false, ErrNotFound
	}
	if !transaction.Matches(normalized, normalizedProjection) {
		return false, ErrConflict
	}
	switch transaction.Status {
	case RuntimeCheckpointDeliveryTransactionAborted:
		return true, nil
	case RuntimeCheckpointDeliveryTransactionCommitted:
		return false, ErrConflict
	case RuntimeCheckpointDeliveryTransactionPrepared:
		transaction.Status = RuntimeCheckpointDeliveryTransactionAborted
		transaction.UpdatedAt = nowUTC()
		r.checkpointTxns[normalized.DeliveryID] = cloneRuntimeCheckpointDeliveryTransaction(transaction)
		return false, nil
	default:
		return false, ErrConflict
	}
}

// AbortRuntimeConfigDelivery releases a prepared configuration lock. The
// stored projection remains private and is never published to the inbox.
func (r *MemoryRepository) AbortRuntimeConfigDelivery(_ context.Context, envelope RuntimeConfigDeliveryEnvelope) (bool, error) {
	normalized, err := NormalizeRuntimeConfigDeliveryEnvelope(envelope)
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	transaction, ok := r.configDeliveryTxns[normalized.DeliveryID]
	if !ok {
		return false, ErrNotFound
	}
	if !transaction.Matches(normalized, transaction.Projection) {
		return false, ErrConflict
	}
	switch transaction.Status {
	case RuntimeConfigDeliveryTransactionAborted:
		return true, nil
	case RuntimeConfigDeliveryTransactionCommitted:
		return false, ErrConflict
	case RuntimeConfigDeliveryTransactionPrepared:
		transaction.Status = RuntimeConfigDeliveryTransactionAborted
		transaction.UpdatedAt = nowUTC()
		r.configDeliveryTxns[normalized.DeliveryID] = cloneRuntimeConfigDeliveryTransaction(transaction)
		return false, nil
	default:
		return false, ErrConflict
	}
}

// AbortRuntimeApprovalRejectionDelivery releases a prepared rejection intent
// without applying it to the destination inbox or Workspace sidecar.
func (r *MemoryRepository) AbortRuntimeApprovalRejectionDelivery(_ context.Context, envelope RuntimeApprovalRejectionDeliveryEnvelope) (bool, error) {
	normalized, err := NormalizeRuntimeApprovalRejectionDeliveryEnvelope(envelope)
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	transaction, ok := r.rejectionTxns[strings.TrimSpace(normalized.DeliveryID)]
	if !ok {
		return false, ErrNotFound
	}
	if !transaction.Matches(normalized) {
		return false, ErrConflict
	}
	switch transaction.Status {
	case RuntimeApprovalRejectionDeliveryTransactionAborted:
		return true, nil
	case RuntimeApprovalRejectionDeliveryTransactionCommitted:
		return false, ErrConflict
	case RuntimeApprovalRejectionDeliveryTransactionPrepared:
		transaction.Status = RuntimeApprovalRejectionDeliveryTransactionAborted
		transaction.UpdatedAt = nowUTC()
		r.rejectionTxns[normalized.DeliveryID] = transaction
		return false, nil
	default:
		return false, ErrConflict
	}
}

var _ RuntimeEventDeliveryAbortableTransactionRepository = (*MemoryRepository)(nil)
var _ RuntimeCheckpointDeliveryAbortableTransactionRepository = (*MemoryRepository)(nil)
var _ RuntimeConfigDeliveryAbortableTransactionRepository = (*MemoryRepository)(nil)
var _ RuntimeApprovalRejectionDeliveryAbortableTransactionRepository = (*MemoryRepository)(nil)
