package runtime

import (
	"context"
	"strings"
)

func cloneRuntimeDeliveryGroupTransaction(item RuntimeDeliveryGroupTransaction) RuntimeDeliveryGroupTransaction {
	item.Members = append([]RuntimeDeliveryGroupMemberEnvelope(nil), item.Members...)
	return item
}

func (r *MemoryRepository) PrepareRuntimeDeliveryGroup(_ context.Context, envelope RuntimeDeliveryGroupEnvelope) (bool, error) {
	normalized, err := NormalizeRuntimeDeliveryGroupEnvelope(envelope)
	if err != nil {
		return false, err
	}
	now := nowUTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deliveryGroupTxns == nil {
		r.deliveryGroupTxns = make(map[string]RuntimeDeliveryGroupTransaction)
	}
	if existing, ok := r.deliveryGroupTxns[normalized.GroupID]; ok {
		if !existing.Matches(normalized) {
			return false, ErrConflict
		}
		if err := r.acceptRuntimeDeliveryGroupFenceLocked(normalized, now, true); err != nil {
			return false, err
		}
		switch existing.Status {
		case RuntimeDeliveryGroupTransactionCommitted:
			return true, nil
		case RuntimeDeliveryGroupTransactionPrepared:
			if existing.ExpiresAt.After(now) {
				return true, nil
			}
		case RuntimeDeliveryGroupTransactionAborted:
			return false, ErrConflict
		default:
			return false, ErrConflict
		}
	}
	if err := r.acceptRuntimeDeliveryGroupFenceLocked(normalized, now, true); err != nil {
		return false, err
	}
	prepared, err := NewRuntimeDeliveryGroupTransaction(normalized, now)
	if err != nil {
		return false, err
	}
	if existing, ok := r.deliveryGroupTxns[normalized.GroupID]; ok && !existing.CreatedAt.IsZero() {
		prepared.CreatedAt = existing.CreatedAt
	}
	r.deliveryGroupTxns[normalized.GroupID] = cloneRuntimeDeliveryGroupTransaction(prepared)
	return false, nil
}

func (r *MemoryRepository) CommitRuntimeDeliveryGroup(_ context.Context, envelope RuntimeDeliveryGroupEnvelope) (bool, error) {
	normalized, err := NormalizeRuntimeDeliveryGroupEnvelope(envelope)
	if err != nil {
		return false, err
	}
	now := nowUTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	transaction, ok := r.deliveryGroupTxns[normalized.GroupID]
	if !ok {
		return false, ErrNotFound
	}
	if !transaction.Matches(normalized) {
		return false, ErrConflict
	}
	if err := r.acceptRuntimeDeliveryGroupFenceLocked(normalized, now, true); err != nil {
		return false, err
	}
	switch transaction.Status {
	case RuntimeDeliveryGroupTransactionCommitted:
		return true, nil
	case RuntimeDeliveryGroupTransactionPrepared:
		if !transaction.ExpiresAt.After(now) {
			return false, ErrRuntimeDeliveryGroupTransactionStale
		}
	default:
		return false, ErrConflict
	}
	transaction.Status = RuntimeDeliveryGroupTransactionCommitted
	transaction.UpdatedAt = now
	r.deliveryGroupTxns[normalized.GroupID] = cloneRuntimeDeliveryGroupTransaction(transaction)
	return false, nil
}

func (r *MemoryRepository) AbortRuntimeDeliveryGroup(_ context.Context, envelope RuntimeDeliveryGroupEnvelope) (bool, error) {
	normalized, err := NormalizeRuntimeDeliveryGroupEnvelope(envelope)
	if err != nil {
		return false, err
	}
	now := nowUTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	transaction, ok := r.deliveryGroupTxns[normalized.GroupID]
	if !ok {
		return false, ErrNotFound
	}
	if !transaction.Matches(normalized) {
		return false, ErrConflict
	}
	if err := r.acceptRuntimeDeliveryGroupFenceLocked(normalized, now, true); err != nil {
		return false, err
	}
	switch transaction.Status {
	case RuntimeDeliveryGroupTransactionAborted:
		return true, nil
	case RuntimeDeliveryGroupTransactionCommitted:
		return false, ErrConflict
	case RuntimeDeliveryGroupTransactionPrepared:
		transaction.Status = RuntimeDeliveryGroupTransactionAborted
		transaction.UpdatedAt = now
		r.deliveryGroupTxns[normalized.GroupID] = cloneRuntimeDeliveryGroupTransaction(transaction)
		return false, nil
	default:
		return false, ErrConflict
	}
}

func (r *MemoryRepository) GetRuntimeDeliveryGroupTransaction(_ context.Context, groupID string) (RuntimeDeliveryGroupTransaction, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.deliveryGroupTxns[strings.TrimSpace(groupID)]
	if !ok {
		return RuntimeDeliveryGroupTransaction{}, ErrNotFound
	}
	return cloneRuntimeDeliveryGroupTransaction(item), nil
}

// RuntimeDeliveryGroupMemberPhase lets the built-in receiver enforce an
// optional all-members-committed gate without exposing any payload.
func (r *MemoryRepository) RuntimeDeliveryGroupMemberPhase(_ context.Context, member RuntimeDeliveryGroupMemberEnvelope) (RuntimeDeliveryGroupTransactionStatus, error) {
	member, err := normalizeRuntimeDeliveryGroupMemberEnvelope(member)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var status RuntimeDeliveryGroupTransactionStatus
	switch member.Kind {
	case RuntimeDeliveryKindEvent:
		item, ok := r.eventTxns[member.DeliveryID]
		if !ok {
			return "", ErrNotFound
		}
		switch item.Status {
		case RuntimeEventDeliveryTransactionPrepared:
			status = RuntimeDeliveryGroupTransactionPrepared
		case RuntimeEventDeliveryTransactionCommitted:
			status = RuntimeDeliveryGroupTransactionCommitted
		case RuntimeEventDeliveryTransactionAborted:
			status = RuntimeDeliveryGroupTransactionAborted
		}
	case RuntimeDeliveryKindCheckpoint:
		item, ok := r.checkpointTxns[member.DeliveryID]
		if !ok {
			return "", ErrNotFound
		}
		switch item.Status {
		case RuntimeCheckpointDeliveryTransactionPrepared:
			status = RuntimeDeliveryGroupTransactionPrepared
		case RuntimeCheckpointDeliveryTransactionCommitted:
			status = RuntimeDeliveryGroupTransactionCommitted
		case RuntimeCheckpointDeliveryTransactionAborted:
			status = RuntimeDeliveryGroupTransactionAborted
		}
	case RuntimeDeliveryKindConfig:
		item, ok := r.configDeliveryTxns[member.DeliveryID]
		if !ok {
			return "", ErrNotFound
		}
		switch item.Status {
		case RuntimeConfigDeliveryTransactionPrepared:
			status = RuntimeDeliveryGroupTransactionPrepared
		case RuntimeConfigDeliveryTransactionCommitted:
			status = RuntimeDeliveryGroupTransactionCommitted
		case RuntimeConfigDeliveryTransactionAborted:
			status = RuntimeDeliveryGroupTransactionAborted
		}
	case RuntimeDeliveryKindRejection:
		item, ok := r.rejectionTxns[member.DeliveryID]
		if !ok {
			return "", ErrNotFound
		}
		switch item.Status {
		case RuntimeApprovalRejectionDeliveryTransactionPrepared:
			status = RuntimeDeliveryGroupTransactionPrepared
		case RuntimeApprovalRejectionDeliveryTransactionCommitted:
			status = RuntimeDeliveryGroupTransactionCommitted
		case RuntimeApprovalRejectionDeliveryTransactionAborted:
			status = RuntimeDeliveryGroupTransactionAborted
		}
	}
	if status == "" {
		return "", ErrConflict
	}
	return status, nil
}

var _ RuntimeDeliveryGroupTransactionRepository = (*MemoryRepository)(nil)
var _ RuntimeDeliveryGroupMemberPhaseResolver = (*MemoryRepository)(nil)
