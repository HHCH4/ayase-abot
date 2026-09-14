package runtime

import (
	"context"
	"strings"
)

// PrepareRuntimeEventDelivery durably stages event metadata at the
// destination. It is idempotent by DeliveryID and does not update the visible
// event inbox until CommitRuntimeEventDelivery.
func (r *MemoryRepository) PrepareRuntimeEventDelivery(_ context.Context, envelope RuntimeEventDeliveryEnvelope) (bool, error) {
	normalized, err := NormalizeRuntimeEventDeliveryEnvelope(envelope)
	if err != nil {
		return false, err
	}
	now := nowUTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.eventTxns == nil {
		r.eventTxns = make(map[string]RuntimeEventDeliveryTransaction)
	}
	if existing, ok := r.eventTxns[normalized.DeliveryID]; ok {
		if !existing.Matches(normalized) {
			return false, ErrConflict
		}
		switch existing.Status {
		case RuntimeEventDeliveryTransactionCommitted:
			return true, nil
		case RuntimeEventDeliveryTransactionPrepared:
			if existing.ExpiresAt.After(now) {
				return true, nil
			}
			// An expired prepare can be refreshed only with the same immutable
			// DeliveryID/event metadata. Preserve the original creation time but
			// use the newly authenticated envelope for the next commit window.
		case RuntimeEventDeliveryTransactionAborted:
			return false, ErrConflict
		default:
			return false, ErrConflict
		}
	}
	prepared, err := newRuntimeEventDeliveryTransaction(normalized, now)
	if err != nil {
		return false, err
	}
	if existing, ok := r.eventTxns[normalized.DeliveryID]; ok && !existing.CreatedAt.IsZero() {
		prepared.CreatedAt = existing.CreatedAt
	}
	r.eventTxns[normalized.DeliveryID] = cloneRuntimeEventDeliveryTransaction(prepared)
	return false, nil
}

// CommitRuntimeEventDelivery atomically publishes the prepared envelope to
// the destination inbox. The stored prepare envelope is used for the final
// inbox record so a retry with refreshed signing material remains idempotent.
func (r *MemoryRepository) CommitRuntimeEventDelivery(_ context.Context, envelope RuntimeEventDeliveryEnvelope) (bool, error) {
	normalized, err := NormalizeRuntimeEventDeliveryEnvelope(envelope)
	if err != nil {
		return false, err
	}
	now := nowUTC()
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
	case RuntimeEventDeliveryTransactionCommitted:
		return true, nil
	case RuntimeEventDeliveryTransactionPrepared:
		if !transaction.ExpiresAt.After(now) {
			return false, ErrRuntimeEventDeliveryStale
		}
	default:
		return false, ErrConflict
	}
	duplicate, err := r.acceptRuntimeEventDeliveryLocked(transaction.envelope(), now)
	if err != nil {
		return false, err
	}
	transaction.Status = RuntimeEventDeliveryTransactionCommitted
	transaction.UpdatedAt = now
	r.eventTxns[normalized.DeliveryID] = cloneRuntimeEventDeliveryTransaction(transaction)
	return duplicate, nil
}

// GetRuntimeEventDeliveryTransaction is a diagnostic/test hook that returns a
// defensive metadata-only copy.
func (r *MemoryRepository) GetRuntimeEventDeliveryTransaction(_ context.Context, deliveryID string) (RuntimeEventDeliveryTransaction, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.eventTxns[strings.TrimSpace(deliveryID)]
	if !ok {
		return RuntimeEventDeliveryTransaction{}, ErrNotFound
	}
	return cloneRuntimeEventDeliveryTransaction(item), nil
}

var _ RuntimeEventDeliveryTransactionRepository = (*MemoryRepository)(nil)
