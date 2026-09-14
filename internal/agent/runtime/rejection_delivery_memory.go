package runtime

import (
	"context"
	"sort"
	"strings"
	"time"
)

func rejectionApprovalKey(item RuntimeApprovalRejectionDeliveryOutbox) string {
	return item.Source + "\x00" + item.Destination + "\x00" + item.ApprovalID
}

func rejectionInboxApprovalKey(envelope RuntimeApprovalRejectionDeliveryEnvelope) string {
	return envelope.Source + "\x00" + envelope.Destination + "\x00" + envelope.ApprovalID
}

func (r *MemoryRepository) EnqueueRuntimeApprovalRejectionDeliveryOutbox(_ context.Context, item RuntimeApprovalRejectionDeliveryOutbox) (RuntimeApprovalRejectionDeliveryOutbox, error) {
	normalized, err := item.Normalize(nowUTC())
	if err != nil {
		return RuntimeApprovalRejectionDeliveryOutbox{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	approval, ok := r.approvals[normalized.ApprovalID]
	if !ok {
		return RuntimeApprovalRejectionDeliveryOutbox{}, ErrNotFound
	}
	if approval.Status != ApprovalRejected || approval.InvocationID != normalized.InvocationID || strings.TrimSpace(approval.ToolCallID) != normalized.ToolCallID || strings.TrimSpace(approval.OperationID) != normalized.OperationID {
		return RuntimeApprovalRejectionDeliveryOutbox{}, ErrConflict
	}
	if _, ok := r.invocations[normalized.InvocationID]; !ok {
		return RuntimeApprovalRejectionDeliveryOutbox{}, ErrNotFound
	}
	deliveryGroup, groupErr := NewRuntimeDeliveryGroup(normalized.Source, normalized.Destination, normalized.InvocationID, []RuntimeDeliveryGroupMember{{Kind: RuntimeDeliveryKindRejection, OutboxID: normalized.ID, DeliveryID: normalized.DeliveryID}}, normalized.DecisionAt)
	if groupErr != nil {
		return RuntimeApprovalRejectionDeliveryOutbox{}, groupErr
	}
	normalized.GroupID = deliveryGroup.ID
	if r.rejectionOutbox == nil {
		r.rejectionOutbox = make(map[string]RuntimeApprovalRejectionDeliveryOutbox)
	}
	if r.rejectionByApproval == nil {
		r.rejectionByApproval = make(map[string]string)
	}
	key := rejectionApprovalKey(normalized)
	if existingID, exists := r.rejectionByApproval[key]; exists {
		existing, found := r.rejectionOutbox[existingID]
		if found && existing.Matches(normalized) {
			if existing.GroupID == "" {
				existing.GroupID = deliveryGroup.ID
				r.rejectionOutbox[existingID] = existing
				if r.deliveryGroups == nil {
					r.deliveryGroups = make(map[string]RuntimeDeliveryGroup)
				}
				if _, groupExists := r.deliveryGroups[deliveryGroup.ID]; !groupExists {
					r.deliveryGroups[deliveryGroup.ID] = cloneRuntimeDeliveryGroup(deliveryGroup)
				}
			}
			return cloneRuntimeApprovalRejectionDeliveryOutbox(existing), nil
		}
		return RuntimeApprovalRejectionDeliveryOutbox{}, ErrConflict
	}
	if existing, exists := r.rejectionOutbox[normalized.ID]; exists {
		if !existing.Matches(normalized) {
			return RuntimeApprovalRejectionDeliveryOutbox{}, ErrConflict
		}
		if existing.GroupID == "" {
			existing.GroupID = deliveryGroup.ID
			r.rejectionOutbox[normalized.ID] = existing
		}
		return cloneRuntimeApprovalRejectionDeliveryOutbox(existing), nil
	}
	r.rejectionOutbox[normalized.ID] = cloneRuntimeApprovalRejectionDeliveryOutbox(normalized)
	r.rejectionByApproval[key] = normalized.ID
	if r.deliveryGroups == nil {
		r.deliveryGroups = make(map[string]RuntimeDeliveryGroup)
	}
	if _, exists := r.deliveryGroups[deliveryGroup.ID]; !exists {
		r.deliveryGroups[deliveryGroup.ID] = cloneRuntimeDeliveryGroup(deliveryGroup)
	}
	return cloneRuntimeApprovalRejectionDeliveryOutbox(normalized), nil
}

func (r *MemoryRepository) GetRuntimeApprovalRejectionDeliveryOutbox(_ context.Context, id string) (RuntimeApprovalRejectionDeliveryOutbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.rejectionOutbox[strings.TrimSpace(id)]
	if !ok {
		return RuntimeApprovalRejectionDeliveryOutbox{}, ErrNotFound
	}
	return cloneRuntimeApprovalRejectionDeliveryOutbox(item), nil
}

func (r *MemoryRepository) ListRuntimeApprovalRejectionDeliveryOutbox(_ context.Context, invocationID string, status RuntimeApprovalRejectionDeliveryOutboxStatus, limit int) ([]RuntimeApprovalRejectionDeliveryOutbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	invocationID = strings.TrimSpace(invocationID)
	if invocationID != "" {
		if _, ok := r.invocations[invocationID]; !ok {
			return nil, ErrNotFound
		}
	}
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	items := make([]RuntimeApprovalRejectionDeliveryOutbox, 0, limit)
	for _, item := range r.rejectionOutbox {
		if invocationID != "" && item.InvocationID != invocationID {
			continue
		}
		if status != "" && item.Status != status {
			continue
		}
		items = append(items, cloneRuntimeApprovalRejectionDeliveryOutbox(item))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].AvailableAt.Equal(items[j].AvailableAt) {
			if items[i].CreatedAt.Equal(items[j].CreatedAt) {
				return items[i].ID < items[j].ID
			}
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		}
		return items[i].AvailableAt.Before(items[j].AvailableAt)
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (r *MemoryRepository) ClaimRuntimeApprovalRejectionDeliveryOutbox(_ context.Context, owner string, now time.Time, ttl time.Duration) (RuntimeApprovalRejectionDeliveryOutbox, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeApprovalRejectionDeliveryOwnerLength || ttl <= 0 || ttl > MaxRuntimeApprovalRejectionDeliveryLease {
		return RuntimeApprovalRejectionDeliveryOutbox{}, false, ErrConflict
	}
	if now.IsZero() {
		now = nowUTC()
	} else {
		now = now.UTC()
	}
	items := make([]RuntimeApprovalRejectionDeliveryOutbox, 0, len(r.rejectionOutbox))
	for _, item := range r.rejectionOutbox {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].AvailableAt.Equal(items[j].AvailableAt) {
			if items[i].CreatedAt.Equal(items[j].CreatedAt) {
				return items[i].ID < items[j].ID
			}
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		}
		return items[i].AvailableAt.Before(items[j].AvailableAt)
	})
	for _, candidate := range items {
		item, ok := r.rejectionOutbox[candidate.ID]
		if !ok || item.Status.terminal() {
			continue
		}
		if item.Status == RuntimeApprovalRejectionDeliveryOutboxQueued && item.AvailableAt.After(now) {
			continue
		}
		if item.Status == RuntimeApprovalRejectionDeliveryOutboxProcessing {
			if item.LeaseOwner == "" || item.LeaseExpiresAt == nil || item.LeaseExpiresAt.IsZero() {
				item.Status = RuntimeApprovalRejectionDeliveryOutboxFailed
				item.LastError = "approval rejection outbox processing 元数据无效"
				item.LeaseOwner = ""
				item.LeaseExpiresAt = nil
				item.Revision++
				item.UpdatedAt = now
				r.rejectionOutbox[item.ID] = item
				continue
			}
			if item.LeaseExpiresAt.After(now) {
				continue
			}
		}
		if item.Attempt >= MaxRuntimeApprovalRejectionDeliveryAttempts {
			item.Status = RuntimeApprovalRejectionDeliveryOutboxFailed
			item.LeaseOwner = ""
			item.LeaseExpiresAt = nil
			item.LastError = "approval rejection outbox 达到最大投递次数"
			item.Revision++
			item.UpdatedAt = now
			r.rejectionOutbox[item.ID] = item
			continue
		}
		item.Attempt++
		item.Status = RuntimeApprovalRejectionDeliveryOutboxProcessing
		item.LeaseOwner = owner
		expires := now.Add(ttl)
		item.LeaseExpiresAt = &expires
		item.Revision++
		item.UpdatedAt = now
		r.rejectionOutbox[item.ID] = item
		return cloneRuntimeApprovalRejectionDeliveryOutbox(item), true, nil
	}
	return RuntimeApprovalRejectionDeliveryOutbox{}, false, nil
}

func (r *MemoryRepository) CompleteRuntimeApprovalRejectionDeliveryOutbox(_ context.Context, id, owner string, now time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeApprovalRejectionDeliveryOwnerLength {
		return false, ErrConflict
	}
	item, ok := r.rejectionOutbox[strings.TrimSpace(id)]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != RuntimeApprovalRejectionDeliveryOutboxProcessing || item.LeaseOwner != owner {
		return false, nil
	}
	if now.IsZero() {
		now = nowUTC()
	} else {
		now = now.UTC()
	}
	if item.LeaseExpiresAt == nil || !item.LeaseExpiresAt.After(now) {
		return false, nil
	}
	item.Status = RuntimeApprovalRejectionDeliveryOutboxCompleted
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	item.Revision++
	item.UpdatedAt = now
	r.rejectionOutbox[item.ID] = item
	return true, nil
}

func (r *MemoryRepository) RetryRuntimeApprovalRejectionDeliveryOutbox(_ context.Context, id, owner string, now time.Time, message string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeApprovalRejectionDeliveryOwnerLength {
		return false, ErrConflict
	}
	item, ok := r.rejectionOutbox[strings.TrimSpace(id)]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != RuntimeApprovalRejectionDeliveryOutboxProcessing || item.LeaseOwner != owner {
		return false, nil
	}
	if now.IsZero() {
		now = nowUTC()
	} else {
		now = now.UTC()
	}
	if item.LeaseExpiresAt == nil || !item.LeaseExpiresAt.After(now) {
		return false, nil
	}
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	item.LastError = SanitizeRuntimeString(strings.TrimSpace(message))
	if len(item.LastError) > 4096 {
		item.LastError = item.LastError[:4096]
	}
	if item.Attempt >= MaxRuntimeApprovalRejectionDeliveryAttempts {
		item.Status = RuntimeApprovalRejectionDeliveryOutboxFailed
	} else {
		item.Status = RuntimeApprovalRejectionDeliveryOutboxQueued
		item.AvailableAt = now.Add(RuntimeApprovalRejectionDeliveryBackoff(item.Attempt))
	}
	item.Revision++
	item.UpdatedAt = now
	r.rejectionOutbox[item.ID] = item
	return true, nil
}

func (r *MemoryRepository) AcceptRuntimeApprovalRejectionDelivery(_ context.Context, envelope RuntimeApprovalRejectionDeliveryEnvelope) (bool, error) {
	normalized, err := envelope.Normalize()
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.acceptRuntimeApprovalRejectionDeliveryLocked(normalized, nowUTC())
}

func (r *MemoryRepository) acceptRuntimeApprovalRejectionDeliveryLocked(envelope RuntimeApprovalRejectionDeliveryEnvelope, receivedAt time.Time) (bool, error) {
	if r.rejectionInbox == nil {
		r.rejectionInbox = make(map[string]RuntimeApprovalRejectionDeliveryInboxRecord)
	}
	if r.rejectionInboxByApproval == nil {
		r.rejectionInboxByApproval = make(map[string]string)
	}
	if existing, ok := r.rejectionInbox[envelope.DeliveryID]; ok {
		if existing.Matches(envelope) {
			return true, nil
		}
		return false, ErrConflict
	}
	approvalKey := rejectionInboxApprovalKey(envelope)
	if existingID, ok := r.rejectionInboxByApproval[approvalKey]; ok {
		if existing, exists := r.rejectionInbox[existingID]; exists && existing.Matches(envelope) {
			return true, nil
		}
		return false, ErrConflict
	}
	record := RuntimeApprovalRejectionDeliveryInboxRecordFromEnvelope(envelope, receivedAt)
	r.rejectionInbox[envelope.DeliveryID] = record
	r.rejectionInboxByApproval[approvalKey] = envelope.DeliveryID
	return false, nil
}

func (r *MemoryRepository) GetRuntimeApprovalRejectionDelivery(_ context.Context, deliveryID string) (RuntimeApprovalRejectionDeliveryInboxRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.rejectionInbox[strings.TrimSpace(deliveryID)]
	if !ok {
		return RuntimeApprovalRejectionDeliveryInboxRecord{}, ErrNotFound
	}
	return item, nil
}

func (r *MemoryRepository) PrepareRuntimeApprovalRejectionDelivery(_ context.Context, envelope RuntimeApprovalRejectionDeliveryEnvelope) (bool, error) {
	normalized, err := envelope.Normalize()
	if err != nil {
		return false, err
	}
	now := nowUTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rejectionTxns == nil {
		r.rejectionTxns = make(map[string]RuntimeApprovalRejectionDeliveryTransaction)
	}
	if existing, ok := r.rejectionTxns[normalized.DeliveryID]; ok {
		if !existing.Matches(normalized) {
			return false, ErrConflict
		}
		if existing.Status == RuntimeApprovalRejectionDeliveryTransactionCommitted {
			return true, nil
		}
		if existing.Status != RuntimeApprovalRejectionDeliveryTransactionPrepared {
			return false, ErrConflict
		}
		if existing.ExpiresAt.After(now) {
			return true, nil
		}
	}
	prepared, err := NewRuntimeApprovalRejectionDeliveryTransaction(normalized, now)
	if err != nil {
		return false, err
	}
	r.rejectionTxns[normalized.DeliveryID] = prepared
	return false, nil
}

func (r *MemoryRepository) CommitRuntimeApprovalRejectionDelivery(_ context.Context, envelope RuntimeApprovalRejectionDeliveryEnvelope) (bool, error) {
	normalized, err := envelope.Normalize()
	if err != nil {
		return false, err
	}
	now := nowUTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	transaction, ok := r.rejectionTxns[normalized.DeliveryID]
	if !ok {
		return false, ErrNotFound
	}
	if !transaction.Matches(normalized) {
		return false, ErrConflict
	}
	if transaction.Status == RuntimeApprovalRejectionDeliveryTransactionCommitted {
		return true, nil
	}
	if transaction.Status != RuntimeApprovalRejectionDeliveryTransactionPrepared {
		return false, ErrConflict
	}
	if !transaction.ExpiresAt.After(now) {
		return false, ErrRuntimeApprovalRejectionDeliveryStale
	}
	prepared := transaction.envelope()
	duplicate, err := r.acceptRuntimeApprovalRejectionDeliveryLocked(prepared, now)
	if err != nil {
		return false, err
	}
	transaction.Status = RuntimeApprovalRejectionDeliveryTransactionCommitted
	transaction.UpdatedAt = now
	r.rejectionTxns[normalized.DeliveryID] = transaction
	return duplicate, nil
}

func (r *MemoryRepository) GetRuntimeApprovalRejectionDeliveryTransaction(_ context.Context, deliveryID string) (RuntimeApprovalRejectionDeliveryTransaction, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.rejectionTxns[strings.TrimSpace(deliveryID)]
	if !ok {
		return RuntimeApprovalRejectionDeliveryTransaction{}, ErrNotFound
	}
	return item, nil
}

var _ RuntimeApprovalRejectionDeliveryOutboxRepository = (*MemoryRepository)(nil)
var _ RuntimeApprovalRejectionDeliveryInbox = (*MemoryRepository)(nil)
var _ RuntimeApprovalRejectionDeliveryTransactionRepository = (*MemoryRepository)(nil)

// CommitApprovalRejectionWithDelivery is the in-memory equivalent of the
// SQLite local atomic boundary. It reuses the existing all-or-nothing commit
// and stores the source cursor under the same repository lock.
func (r *MemoryRepository) CommitApprovalRejectionWithDelivery(ctx context.Context, commit ApprovalResumeCommit, delivery RuntimeApprovalRejectionDeliveryOutbox) (Approval, Invocation, []AgentEvent, error) {
	commit.RejectionDelivery = &delivery
	return r.CommitApprovalResume(ctx, commit)
}

var _ ApprovalRejectionDeliveryCommitRepository = (*MemoryRepository)(nil)
