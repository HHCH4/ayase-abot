package runtime

import (
	"context"
	"sort"
	"strings"
	"time"
)

// enqueueRuntimeDeliveryGroupSettlementLocked is the transaction-local form
// used by the group-member terminal marker. The caller owns r.mu.
func (r *MemoryRepository) enqueueRuntimeDeliveryGroupSettlementLocked(item RuntimeDeliveryGroupSettlement, now time.Time) (RuntimeDeliveryGroupSettlement, error) {
	normalized, err := item.Normalize(now)
	if err != nil {
		return RuntimeDeliveryGroupSettlement{}, err
	}
	if r.deliveryGroupSettlements == nil {
		r.deliveryGroupSettlements = make(map[string]RuntimeDeliveryGroupSettlement)
	}
	if existing, ok := r.deliveryGroupSettlements[normalized.ID]; ok {
		if !existing.MatchesIdentity(normalized) {
			return RuntimeDeliveryGroupSettlement{}, ErrConflict
		}
		return cloneRuntimeDeliveryGroupSettlement(existing), nil
	}
	r.deliveryGroupSettlements[normalized.ID] = cloneRuntimeDeliveryGroupSettlement(normalized)
	return cloneRuntimeDeliveryGroupSettlement(normalized), nil
}

func (r *MemoryRepository) EnqueueRuntimeDeliveryGroupSettlement(_ context.Context, item RuntimeDeliveryGroupSettlement) (RuntimeDeliveryGroupSettlement, error) {
	now := nowUTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enqueueRuntimeDeliveryGroupSettlementLocked(item, now)
}

func (r *MemoryRepository) GetRuntimeDeliveryGroupSettlement(_ context.Context, id string) (RuntimeDeliveryGroupSettlement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.deliveryGroupSettlements[strings.TrimSpace(id)]
	if !ok {
		return RuntimeDeliveryGroupSettlement{}, ErrNotFound
	}
	return cloneRuntimeDeliveryGroupSettlement(item), nil
}

func (r *MemoryRepository) ListRuntimeDeliveryGroupSettlements(_ context.Context, invocationID string, status RuntimeDeliveryGroupSettlementStatus, limit int) ([]RuntimeDeliveryGroupSettlement, error) {
	invocationID = strings.TrimSpace(invocationID)
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	if invocationID != "" {
		r.mu.Lock()
		_, exists := r.invocations[invocationID]
		r.mu.Unlock()
		if !exists {
			return nil, ErrNotFound
		}
	}
	if status != "" && !validRuntimeDeliveryGroupSettlementStatus(status) {
		return nil, ErrInvalidRuntimeDeliveryGroupSettlement
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]RuntimeDeliveryGroupSettlement, 0, len(r.deliveryGroupSettlements))
	for _, item := range r.deliveryGroupSettlements {
		if invocationID != "" && item.InvocationID != invocationID {
			continue
		}
		if status != "" && item.Status != status {
			continue
		}
		items = append(items, cloneRuntimeDeliveryGroupSettlement(item))
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

func (r *MemoryRepository) ClaimRuntimeDeliveryGroupSettlement(_ context.Context, owner string, now time.Time, ttl time.Duration) (RuntimeDeliveryGroupSettlement, bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeDeliveryGroupSettlementOwnerLength || ttl <= 0 || ttl > MaxRuntimeDeliveryGroupSettlementLease {
		return RuntimeDeliveryGroupSettlement{}, false, ErrConflict
	}
	now = normalizeRuntimeDeliveryGroupTime(now)
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]RuntimeDeliveryGroupSettlement, 0, len(r.deliveryGroupSettlements))
	for _, item := range r.deliveryGroupSettlements {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].AvailableAt.Equal(items[j].AvailableAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].AvailableAt.Before(items[j].AvailableAt)
	})
	for _, candidate := range items {
		item, ok := r.deliveryGroupSettlements[candidate.ID]
		if !ok || item.Status == RuntimeDeliveryGroupSettlementCompleted || item.Status == RuntimeDeliveryGroupSettlementFailed {
			continue
		}
		if item.Status == RuntimeDeliveryGroupSettlementQueued && item.AvailableAt.After(now) {
			continue
		}
		if item.Status == RuntimeDeliveryGroupSettlementProcessing && item.LeaseExpiresAt != nil && item.LeaseExpiresAt.After(now) {
			continue
		}
		if item.Attempt >= MaxRuntimeDeliveryGroupSettlementAttempts {
			item.Status = RuntimeDeliveryGroupSettlementFailed
			item.Stage = RuntimeDeliveryGroupSettlementStageFailed
			item.LastError = "Runtime delivery group settlement 达到最大投递次数"
			item.AlertCode = RuntimeDeliveryGroupSettlementAlertRetryExhausted
			if item.AlertCount < MaxRuntimeDeliveryGroupSettlementAlertCount {
				item.AlertCount++
			}
			alertAt := now
			item.AlertAt = &alertAt
			item.LeaseOwner = ""
			item.LeaseExpiresAt = nil
			item.CompletedAt = nil
			item.Revision++
			item.UpdatedAt = now
			r.deliveryGroupSettlements[item.ID] = item
			continue
		}
		expires := now.Add(ttl)
		item.Status = RuntimeDeliveryGroupSettlementProcessing
		item.Stage = RuntimeDeliveryGroupSettlementStageStatus
		item.Attempt++
		item.Revision++
		item.LeaseOwner = owner
		item.LeaseExpiresAt = &expires
		item.CompletedAt = nil
		item.UpdatedAt = now
		r.deliveryGroupSettlements[item.ID] = item
		return cloneRuntimeDeliveryGroupSettlement(item), true, nil
	}
	return RuntimeDeliveryGroupSettlement{}, false, nil
}

func (r *MemoryRepository) CompleteRuntimeDeliveryGroupSettlement(_ context.Context, id, owner string, now time.Time) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeDeliveryGroupSettlementOwnerLength {
		return false, ErrConflict
	}
	now = normalizeRuntimeDeliveryGroupTime(now)
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.deliveryGroupSettlements[strings.TrimSpace(id)]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != RuntimeDeliveryGroupSettlementProcessing || item.LeaseOwner != owner || item.LeaseExpiresAt == nil || !item.LeaseExpiresAt.After(now) {
		return false, nil
	}
	finished := now
	item.Status = RuntimeDeliveryGroupSettlementCompleted
	item.Stage = RuntimeDeliveryGroupSettlementStageCompleted
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	item.CompletedAt = &finished
	item.Revision++
	item.UpdatedAt = now
	r.deliveryGroupSettlements[item.ID] = item
	return true, nil
}

func (r *MemoryRepository) RetryRuntimeDeliveryGroupSettlement(_ context.Context, id, owner string, now time.Time, message string) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeDeliveryGroupSettlementOwnerLength {
		return false, ErrConflict
	}
	now = normalizeRuntimeDeliveryGroupTime(now)
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.deliveryGroupSettlements[strings.TrimSpace(id)]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != RuntimeDeliveryGroupSettlementProcessing || item.LeaseOwner != owner || item.LeaseExpiresAt == nil || !item.LeaseExpiresAt.After(now) {
		return false, nil
	}
	item.LastError = SanitizeRuntimeString(strings.TrimSpace(message))
	if len(item.LastError) > MaxRuntimeDeliveryGroupSettlementErrorLength {
		item.LastError = item.LastError[:MaxRuntimeDeliveryGroupSettlementErrorLength]
	}
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	item.CompletedAt = nil
	if item.Attempt >= MaxRuntimeDeliveryGroupSettlementAttempts {
		item.Status = RuntimeDeliveryGroupSettlementFailed
		item.Stage = RuntimeDeliveryGroupSettlementStageFailed
		item.AlertCode = RuntimeDeliveryGroupSettlementAlertRetryExhausted
		if item.AlertCount < MaxRuntimeDeliveryGroupSettlementAlertCount {
			item.AlertCount++
		}
		alertAt := now
		item.AlertAt = &alertAt
	} else {
		item.Status = RuntimeDeliveryGroupSettlementQueued
		item.Stage = RuntimeDeliveryGroupSettlementStageQueued
		item.AvailableAt = now.Add(RuntimeDeliveryGroupSettlementBackoff(item.Attempt))
	}
	item.Revision++
	item.UpdatedAt = now
	r.deliveryGroupSettlements[item.ID] = item
	return true, nil
}

func (r *MemoryRepository) FailRuntimeDeliveryGroupSettlement(_ context.Context, id, owner string, now time.Time, message string) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeDeliveryGroupSettlementOwnerLength {
		return false, ErrConflict
	}
	now = normalizeRuntimeDeliveryGroupTime(now)
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.deliveryGroupSettlements[strings.TrimSpace(id)]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != RuntimeDeliveryGroupSettlementProcessing || item.LeaseOwner != owner || item.LeaseExpiresAt == nil || !item.LeaseExpiresAt.After(now) {
		return false, nil
	}
	item.Status = RuntimeDeliveryGroupSettlementFailed
	item.Stage = RuntimeDeliveryGroupSettlementStageFailed
	item.LastError = SanitizeRuntimeString(strings.TrimSpace(message))
	if len(item.LastError) > MaxRuntimeDeliveryGroupSettlementErrorLength {
		item.LastError = item.LastError[:MaxRuntimeDeliveryGroupSettlementErrorLength]
	}
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	item.CompletedAt = nil
	item.Revision++
	item.UpdatedAt = now
	r.deliveryGroupSettlements[item.ID] = item
	return true, nil
}

func (r *MemoryRepository) AdvanceRuntimeDeliveryGroupSettlement(_ context.Context, id, owner string, expectedRevision int64, stage RuntimeDeliveryGroupSettlementStage, remotePhase RuntimeDeliveryGroupSettlementRemotePhase, alertCode RuntimeDeliveryGroupSettlementAlertCode, message string, now time.Time) (RuntimeDeliveryGroupSettlement, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id = strings.TrimSpace(id)
	item, ok := r.deliveryGroupSettlements[id]
	if !ok {
		return RuntimeDeliveryGroupSettlement{}, false, ErrNotFound
	}
	updated, duplicate, err := AdvanceRuntimeDeliveryGroupSettlement(item, owner, expectedRevision, stage, remotePhase, alertCode, message, now)
	if err != nil {
		return RuntimeDeliveryGroupSettlement{}, false, err
	}
	if !duplicate {
		r.deliveryGroupSettlements[id] = cloneRuntimeDeliveryGroupSettlement(updated)
	}
	return cloneRuntimeDeliveryGroupSettlement(updated), duplicate, nil
}

var _ RuntimeDeliveryGroupSettlementRepository = (*MemoryRepository)(nil)
