package runtime

import (
	"context"
	"sort"
	"strings"
	"time"
)

func (r *MemoryRepository) EnqueueRuntimeConfigDirectoryOutbox(_ context.Context, item RuntimeConfigDirectoryOutbox) (RuntimeConfigDirectoryOutbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := nowUTC()
	normalized, err := item.normalize(now)
	if err != nil {
		return RuntimeConfigDirectoryOutbox{}, err
	}
	if r.configDirectoryOutbox == nil {
		r.configDirectoryOutbox = make(map[string]RuntimeConfigDirectoryOutbox)
	}
	if existing, ok := r.configDirectoryOutbox[normalized.ID]; ok {
		if !existing.Matches(normalized) {
			return RuntimeConfigDirectoryOutbox{}, ErrConflict
		}
		return cloneRuntimeConfigDirectoryOutbox(existing), nil
	}
	for _, existing := range r.configDirectoryOutbox {
		if existing.DeliveryID == normalized.DeliveryID {
			if !existing.Matches(normalized) {
				return RuntimeConfigDirectoryOutbox{}, ErrConflict
			}
			return cloneRuntimeConfigDirectoryOutbox(existing), nil
		}
	}
	if normalized.Status != RuntimeConfigDirectoryOutboxQueued || normalized.Attempt != 0 {
		return RuntimeConfigDirectoryOutbox{}, ErrConflict
	}
	r.configDirectoryOutbox[normalized.ID] = cloneRuntimeConfigDirectoryOutbox(normalized)
	return cloneRuntimeConfigDirectoryOutbox(normalized), nil
}

func (r *MemoryRepository) GetRuntimeConfigDirectoryOutbox(_ context.Context, id string) (RuntimeConfigDirectoryOutbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.configDirectoryOutbox[strings.TrimSpace(id)]
	if !ok {
		return RuntimeConfigDirectoryOutbox{}, ErrNotFound
	}
	return cloneRuntimeConfigDirectoryOutbox(item), nil
}

func (r *MemoryRepository) ListRuntimeConfigDirectoryOutbox(_ context.Context, kind RuntimeConfigDirectoryKind, entryID string, status RuntimeConfigDirectoryOutboxStatus, limit int) ([]RuntimeConfigDirectoryOutbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kind = RuntimeConfigDirectoryKind(strings.TrimSpace(strings.ToLower(string(kind))))
	entryID = strings.TrimSpace(entryID)
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	items := make([]RuntimeConfigDirectoryOutbox, 0, limit)
	for _, item := range r.configDirectoryOutbox {
		if kind != "" && item.Kind != kind {
			continue
		}
		if entryID != "" && item.EntryID != entryID {
			continue
		}
		if status != "" && item.Status != status {
			continue
		}
		items = append(items, cloneRuntimeConfigDirectoryOutbox(item))
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

func (r *MemoryRepository) ClaimRuntimeConfigDirectoryOutbox(_ context.Context, owner string, now time.Time, ttl time.Duration) (RuntimeConfigDirectoryOutbox, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeConfigDirectoryOutboxOwner || ttl <= 0 || ttl > MaxRuntimeConfigDirectoryOutboxLease {
		return RuntimeConfigDirectoryOutbox{}, false, ErrConflict
	}
	if now.IsZero() {
		now = nowUTC()
	} else {
		now = now.UTC()
	}
	items := make([]RuntimeConfigDirectoryOutbox, 0, len(r.configDirectoryOutbox))
	for _, item := range r.configDirectoryOutbox {
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
		item, ok := r.configDirectoryOutbox[candidate.ID]
		if !ok || item.Status == RuntimeConfigDirectoryOutboxCompleted || item.Status == RuntimeConfigDirectoryOutboxFailed {
			continue
		}
		if item.Status == RuntimeConfigDirectoryOutboxQueued && item.AvailableAt.After(now) {
			continue
		}
		if item.Status == RuntimeConfigDirectoryOutboxProcessing {
			if item.LeaseOwner == "" || item.LeaseExpiresAt == nil || item.LeaseExpiresAt.IsZero() {
				item.Status = RuntimeConfigDirectoryOutboxFailed
				item.LastError = "config directory outbox processing 元数据无效"
				item.LeaseOwner = ""
				item.LeaseExpiresAt = nil
				item.Revision++
				item.UpdatedAt = now
				r.configDirectoryOutbox[item.ID] = item
				continue
			}
			if item.LeaseExpiresAt.After(now) {
				continue
			}
		}
		if item.Attempt >= MaxRuntimeConfigDirectoryOutboxAttempts {
			item.Status = RuntimeConfigDirectoryOutboxFailed
			item.LastError = "Runtime config directory outbox 达到最大投递次数"
			item.LeaseOwner = ""
			item.LeaseExpiresAt = nil
			item.Revision++
			item.UpdatedAt = now
			r.configDirectoryOutbox[item.ID] = item
			continue
		}
		item.Attempt++
		item.Status = RuntimeConfigDirectoryOutboxProcessing
		expires := now.Add(ttl)
		item.LeaseOwner = owner
		item.LeaseExpiresAt = &expires
		item.Revision++
		item.UpdatedAt = now
		r.configDirectoryOutbox[item.ID] = item
		return cloneRuntimeConfigDirectoryOutbox(item), true, nil
	}
	return RuntimeConfigDirectoryOutbox{}, false, nil
}

func (r *MemoryRepository) CompleteRuntimeConfigDirectoryOutbox(_ context.Context, id, owner string, now time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeConfigDirectoryOutboxOwner {
		return false, ErrConflict
	}
	item, ok := r.configDirectoryOutbox[strings.TrimSpace(id)]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != RuntimeConfigDirectoryOutboxProcessing || item.LeaseOwner != owner {
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
	item.Status = RuntimeConfigDirectoryOutboxCompleted
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	item.Revision++
	item.UpdatedAt = now
	r.configDirectoryOutbox[item.ID] = item
	return true, nil
}

func (r *MemoryRepository) RetryRuntimeConfigDirectoryOutbox(_ context.Context, id, owner string, now time.Time, message string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeConfigDirectoryOutboxOwner {
		return false, ErrConflict
	}
	item, ok := r.configDirectoryOutbox[strings.TrimSpace(id)]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != RuntimeConfigDirectoryOutboxProcessing || item.LeaseOwner != owner {
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
	if len(item.LastError) > MaxRuntimeConfigDirectoryOutboxError {
		item.LastError = item.LastError[:MaxRuntimeConfigDirectoryOutboxError]
	}
	if item.Attempt >= MaxRuntimeConfigDirectoryOutboxAttempts {
		item.Status = RuntimeConfigDirectoryOutboxFailed
	} else {
		item.Status = RuntimeConfigDirectoryOutboxQueued
		item.AvailableAt = now.Add(runtimeConfigDirectoryOutboxBackoff(item.Attempt))
	}
	item.Revision++
	item.UpdatedAt = now
	r.configDirectoryOutbox[item.ID] = item
	return true, nil
}

var _ RuntimeConfigDirectoryOutboxRepository = (*MemoryRepository)(nil)
