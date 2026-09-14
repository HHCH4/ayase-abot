package runtime

import (
	"context"
	"sort"
	"strings"
	"time"
)

func cloneRuntimeDeliveryAttempt(item RuntimeDeliveryAttempt) RuntimeDeliveryAttempt {
	if item.LeaseExpiresAt != nil {
		expires := item.LeaseExpiresAt.UTC()
		item.LeaseExpiresAt = &expires
	}
	if item.CompletedAt != nil {
		completed := item.CompletedAt.UTC()
		item.CompletedAt = &completed
	}
	return item
}

func (r *MemoryRepository) BeginRuntimeDeliveryAttempt(_ context.Context, candidate RuntimeDeliveryAttempt, now time.Time, ttl time.Duration) (RuntimeDeliveryAttempt, error) {
	normalized, err := candidate.Normalize(now)
	if err != nil {
		return RuntimeDeliveryAttempt{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deliveryAttempts == nil {
		r.deliveryAttempts = make(map[string]RuntimeDeliveryAttempt)
	}
	if existing, ok := r.deliveryAttempts[normalized.ID]; ok {
		merged, mergeErr := BeginRuntimeDeliveryAttempt(&existing, normalized, now, ttl)
		if mergeErr != nil {
			return RuntimeDeliveryAttempt{}, mergeErr
		}
		r.deliveryAttempts[normalized.ID] = cloneRuntimeDeliveryAttempt(merged)
		return cloneRuntimeDeliveryAttempt(merged), nil
	}
	merged, mergeErr := BeginRuntimeDeliveryAttempt(nil, normalized, now, ttl)
	if mergeErr != nil {
		return RuntimeDeliveryAttempt{}, mergeErr
	}
	r.deliveryAttempts[normalized.ID] = cloneRuntimeDeliveryAttempt(merged)
	return cloneRuntimeDeliveryAttempt(merged), nil
}

func (r *MemoryRepository) AdvanceRuntimeDeliveryAttempt(_ context.Context, id, owner string, expectedRevision int64, phase RuntimeDeliveryAttemptPhase, now time.Time, message string) (RuntimeDeliveryAttempt, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id = strings.TrimSpace(id)
	item, ok := r.deliveryAttempts[id]
	if !ok {
		return RuntimeDeliveryAttempt{}, false, ErrNotFound
	}
	updated, duplicate, err := AdvanceRuntimeDeliveryAttempt(item, owner, expectedRevision, phase, now, message)
	if err != nil {
		return RuntimeDeliveryAttempt{}, false, err
	}
	if !duplicate {
		r.deliveryAttempts[id] = cloneRuntimeDeliveryAttempt(updated)
	}
	return cloneRuntimeDeliveryAttempt(updated), duplicate, nil
}

func (r *MemoryRepository) GetRuntimeDeliveryAttempt(_ context.Context, id string) (RuntimeDeliveryAttempt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.deliveryAttempts[strings.TrimSpace(id)]
	if !ok {
		return RuntimeDeliveryAttempt{}, ErrNotFound
	}
	return cloneRuntimeDeliveryAttempt(item), nil
}

func (r *MemoryRepository) ListRuntimeDeliveryAttempts(_ context.Context, invocationID string, kind RuntimeDeliveryKind, limit int) ([]RuntimeDeliveryAttempt, error) {
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
	items := make([]RuntimeDeliveryAttempt, 0, limit)
	for _, item := range r.deliveryAttempts {
		if invocationID != "" && item.InvocationID != invocationID {
			continue
		}
		if kind != "" && item.Kind != kind {
			continue
		}
		items = append(items, cloneRuntimeDeliveryAttempt(item))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

var _ RuntimeDeliveryAttemptRepository = (*MemoryRepository)(nil)
