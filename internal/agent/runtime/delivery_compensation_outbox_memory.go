package runtime

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

func runtimeDeliveryCompensationContextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (r *MemoryRepository) EnqueueRuntimeDeliveryCompensation(ctx context.Context, item RuntimeDeliveryCompensation) (RuntimeDeliveryCompensation, error) {
	if err := runtimeDeliveryCompensationContextErr(ctx); err != nil {
		return RuntimeDeliveryCompensation{}, err
	}
	normalized, err := item.Normalize(time.Now().UTC())
	if err != nil {
		return RuntimeDeliveryCompensation{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deliveryCompensations == nil {
		r.deliveryCompensations = make(map[string]RuntimeDeliveryCompensation)
	}
	if existing, ok := r.deliveryCompensations[normalized.ID]; ok {
		if !existing.MatchesIdentity(normalized) {
			return RuntimeDeliveryCompensation{}, ErrConflict
		}
		return cloneRuntimeDeliveryCompensation(existing), nil
	}
	r.deliveryCompensations[normalized.ID] = cloneRuntimeDeliveryCompensation(normalized)
	return cloneRuntimeDeliveryCompensation(normalized), nil
}

func (r *MemoryRepository) GetRuntimeDeliveryCompensation(ctx context.Context, id string) (RuntimeDeliveryCompensation, error) {
	if err := runtimeDeliveryCompensationContextErr(ctx); err != nil {
		return RuntimeDeliveryCompensation{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.deliveryCompensations[strings.TrimSpace(id)]
	if !ok {
		return RuntimeDeliveryCompensation{}, ErrNotFound
	}
	return cloneRuntimeDeliveryCompensation(item), nil
}

func (r *MemoryRepository) ListRuntimeDeliveryCompensations(ctx context.Context, invocationID string, kind RuntimeDeliveryKind, status RuntimeDeliveryCompensationStatus, limit int) ([]RuntimeDeliveryCompensation, error) {
	if err := runtimeDeliveryCompensationContextErr(ctx); err != nil {
		return nil, err
	}
	invocationID = strings.TrimSpace(invocationID)
	kind = RuntimeDeliveryKind(strings.TrimSpace(string(kind)))
	status = RuntimeDeliveryCompensationStatus(strings.TrimSpace(string(status)))
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	if kind != "" && !validRuntimeDeliveryKind(kind) {
		return nil, ErrInvalidRuntimeDeliveryCompensation
	}
	if status != "" && !validRuntimeDeliveryCompensationStatus(status) {
		return nil, ErrInvalidRuntimeDeliveryCompensation
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if invocationID != "" {
		if _, ok := r.invocations[invocationID]; !ok {
			return nil, ErrNotFound
		}
	}
	items := make([]RuntimeDeliveryCompensation, 0, len(r.deliveryCompensations))
	for _, item := range r.deliveryCompensations {
		if invocationID != "" && item.InvocationID != invocationID {
			continue
		}
		if kind != "" && item.Kind != kind {
			continue
		}
		if status != "" && item.Status != status {
			continue
		}
		items = append(items, cloneRuntimeDeliveryCompensation(item))
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

func (r *MemoryRepository) ClaimRuntimeDeliveryCompensation(ctx context.Context, owner string, now time.Time, ttl time.Duration) (RuntimeDeliveryCompensation, bool, error) {
	if err := runtimeDeliveryCompensationContextErr(ctx); err != nil {
		return RuntimeDeliveryCompensation{}, false, err
	}
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeDeliveryCompensationOwnerLength || ttl <= 0 || ttl > MaxRuntimeDeliveryCompensationLease {
		return RuntimeDeliveryCompensation{}, false, ErrConflict
	}
	now = normalizeRuntimeDeliveryCompensationTime(now, time.Time{})
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]RuntimeDeliveryCompensation, 0, len(r.deliveryCompensations))
	for _, item := range r.deliveryCompensations {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].AvailableAt.Equal(items[j].AvailableAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].AvailableAt.Before(items[j].AvailableAt)
	})
	for _, candidate := range items {
		item, ok := r.deliveryCompensations[candidate.ID]
		if !ok || item.Status == RuntimeDeliveryCompensationCompleted || item.Status == RuntimeDeliveryCompensationFailed {
			continue
		}
		if item.Status == RuntimeDeliveryCompensationQueued && item.AvailableAt.After(now) {
			continue
		}
		if item.Status == RuntimeDeliveryCompensationProcessing && item.LeaseExpiresAt != nil && item.LeaseExpiresAt.After(now) {
			continue
		}
		if item.Attempt >= MaxRuntimeDeliveryCompensationAttempts {
			item.Status = RuntimeDeliveryCompensationFailed
			item.LeaseOwner = ""
			item.LeaseExpiresAt = nil
			item.LastError = "Runtime delivery compensation 达到最大投递次数"
			item.Revision++
			item.UpdatedAt = now
			r.deliveryCompensations[item.ID] = item
			continue
		}
		expires := now.Add(ttl)
		item.Status = RuntimeDeliveryCompensationProcessing
		item.Attempt++
		item.Revision++
		item.LeaseOwner = owner
		item.LeaseExpiresAt = &expires
		item.CompletedAt = nil
		item.UpdatedAt = now
		r.deliveryCompensations[item.ID] = item
		return cloneRuntimeDeliveryCompensation(item), true, nil
	}
	return RuntimeDeliveryCompensation{}, false, nil
}

func (r *MemoryRepository) CompleteRuntimeDeliveryCompensation(ctx context.Context, id, owner string, now time.Time) (bool, error) {
	if err := runtimeDeliveryCompensationContextErr(ctx); err != nil {
		return false, err
	}
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeDeliveryCompensationOwnerLength {
		return false, ErrConflict
	}
	now = normalizeRuntimeDeliveryCompensationTime(now, time.Time{})
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.deliveryCompensations[strings.TrimSpace(id)]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != RuntimeDeliveryCompensationProcessing || item.LeaseOwner != owner || item.LeaseExpiresAt == nil || !item.LeaseExpiresAt.After(now) {
		return false, nil
	}
	finished := now
	item.Status = RuntimeDeliveryCompensationCompleted
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	item.CompletedAt = &finished
	item.Revision++
	item.UpdatedAt = now
	r.deliveryCompensations[item.ID] = item
	return true, nil
}

func (r *MemoryRepository) RetryRuntimeDeliveryCompensation(ctx context.Context, id, owner string, now time.Time, message string) (bool, error) {
	if err := runtimeDeliveryCompensationContextErr(ctx); err != nil {
		return false, err
	}
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeDeliveryCompensationOwnerLength {
		return false, ErrConflict
	}
	now = normalizeRuntimeDeliveryCompensationTime(now, time.Time{})
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.deliveryCompensations[strings.TrimSpace(id)]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != RuntimeDeliveryCompensationProcessing || item.LeaseOwner != owner || item.LeaseExpiresAt == nil || !item.LeaseExpiresAt.After(now) {
		return false, nil
	}
	updated, _, err := retryRuntimeDeliveryCompensation(item, owner, now, message)
	if err != nil {
		return false, err
	}
	r.deliveryCompensations[item.ID] = updated
	return true, nil
}

func (r *MemoryRepository) FailRuntimeDeliveryCompensation(ctx context.Context, id, owner string, now time.Time, message string) (bool, error) {
	if err := runtimeDeliveryCompensationContextErr(ctx); err != nil {
		return false, err
	}
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeDeliveryCompensationOwnerLength {
		return false, ErrConflict
	}
	now = normalizeRuntimeDeliveryCompensationTime(now, time.Time{})
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.deliveryCompensations[strings.TrimSpace(id)]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != RuntimeDeliveryCompensationProcessing || item.LeaseOwner != owner || item.LeaseExpiresAt == nil || !item.LeaseExpiresAt.After(now) {
		return false, nil
	}
	updated, _, err := failRuntimeDeliveryCompensation(item, owner, now, message)
	if err != nil {
		return false, err
	}
	r.deliveryCompensations[item.ID] = updated
	return true, nil
}

func (r *MemoryRepository) DeferRuntimeDeliveryCompensation(ctx context.Context, id, owner string, now, availableAt time.Time, message string) (bool, error) {
	if err := runtimeDeliveryCompensationContextErr(ctx); err != nil {
		return false, err
	}
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeDeliveryCompensationOwnerLength {
		return false, ErrConflict
	}
	now = normalizeRuntimeDeliveryCompensationTime(now, time.Time{})
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.deliveryCompensations[strings.TrimSpace(id)]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != RuntimeDeliveryCompensationProcessing || item.LeaseOwner != owner || item.LeaseExpiresAt == nil || !item.LeaseExpiresAt.After(now) {
		return false, nil
	}
	updated, _, err := DeferRuntimeDeliveryCompensation(item, owner, now, availableAt, message)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			if item.Status != RuntimeDeliveryCompensationProcessing || item.LeaseOwner != owner {
				return false, nil
			}
		}
		return false, err
	}
	r.deliveryCompensations[item.ID] = updated
	return true, nil
}

var _ RuntimeDeliveryCompensationRepository = (*MemoryRepository)(nil)
