package artifact

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// This file implements the optional maintenance extensions for the in-memory
// repository so unit and HTTP tests exercise the same lifecycle as SQLite.

var (
	_ UsageRepository                 = (*MemoryRepository)(nil)
	_ ActiveReferenceRepository       = (*MemoryRepository)(nil)
	_ StaleArtifactRepository         = (*MemoryRepository)(nil)
	_ ExpiredArtifactRepository       = (*MemoryRepository)(nil)
	_ ObjectDeletionRepository        = (*MemoryRepository)(nil)
	_ PendingObjectDeletionRepository = (*MemoryRepository)(nil)
)

func (repository *MemoryRepository) StoredUsage(_ context.Context) (StoredUsage, error) {
	repository.mu.RLock()
	defer repository.mu.RUnlock()
	usage := StoredUsage{}
	seen := make(map[string]struct{})
	for _, item := range repository.items {
		if item.Status == StatusFailed {
			continue
		}
		usage.Artifacts++
		key := strings.TrimSpace(item.StorageKey)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		usage.Objects++
		usage.Bytes += item.Size
	}
	return usage, nil
}

func (repository *MemoryRepository) CountActiveByStorageKey(_ context.Context, storageKey string) (int, error) {
	repository.mu.RLock()
	defer repository.mu.RUnlock()
	count := 0
	for _, item := range repository.items {
		if item.StorageKey != storageKey {
			continue
		}
		if item.Status == StatusFailed || item.Status == StatusDeleting {
			continue
		}
		count++
	}
	return count, nil
}

func (repository *MemoryRepository) ListStaleUploading(_ context.Context, before time.Time, limit int) ([]Artifact, error) {
	return repository.listStale(StatusUploading, before, limit), nil
}

func (repository *MemoryRepository) ListStaleDeleting(_ context.Context, before time.Time, limit int) ([]Artifact, error) {
	return repository.listStale(StatusDeleting, before, limit), nil
}

// ListExpiredArtifacts 按生产仓储相同的规则筛选已过期记录，并保留稳定排序，
// 便于测试和分批维护在多次运行之间持续推进。
func (repository *MemoryRepository) ListExpiredArtifacts(_ context.Context, now, legacyInputBefore time.Time, limit int) ([]Artifact, error) {
	if limit <= 0 {
		limit = maxSweepBatch
	}
	now = now.UTC()
	legacyInputBefore = legacyInputBefore.UTC()
	repository.mu.RLock()
	defer repository.mu.RUnlock()
	result := make([]Artifact, 0)
	for _, item := range repository.items {
		if item.Status != StatusReady && item.Status != StatusQuarantined {
			continue
		}
		expired := item.ExpiresAt != nil && !item.ExpiresAt.After(now)
		legacyExpired := item.ExpiresAt == nil && item.Kind == KindInputAttachment && !legacyInputBefore.IsZero() && !item.CreatedAt.After(legacyInputBefore)
		if !expired && !legacyExpired {
			continue
		}
		result = append(result, cloneArtifact(item))
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i], result[j]
		leftExpiry, rightExpiry := left.CreatedAt, right.CreatedAt
		if left.ExpiresAt != nil {
			leftExpiry = *left.ExpiresAt
		}
		if right.ExpiresAt != nil {
			rightExpiry = *right.ExpiresAt
		}
		if leftExpiry.Equal(rightExpiry) {
			return left.ID < right.ID
		}
		return leftExpiry.Before(rightExpiry)
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (repository *MemoryRepository) listStale(status Status, before time.Time, limit int) []Artifact {
	if limit <= 0 {
		limit = maxSweepBatch
	}
	cutoff := before.UTC()
	repository.mu.RLock()
	defer repository.mu.RUnlock()
	result := make([]Artifact, 0)
	for _, item := range repository.items {
		if item.Status != status || item.UpdatedAt.After(cutoff) {
			continue
		}
		result = append(result, cloneArtifact(item))
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].UpdatedAt.Equal(result[j].UpdatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].UpdatedAt.Before(result[j].UpdatedAt)
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

func (repository *MemoryRepository) EnqueueObjectDeletion(_ context.Context, storageKey string, now time.Time) error {
	key := strings.TrimSpace(storageKey)
	if key == "" {
		return fmt.Errorf("%w: storage key 不能为空", ErrInvalidRequest)
	}
	now = now.UTC()
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.objectDeletions == nil {
		repository.objectDeletions = make(map[string]ObjectDeletion)
	}
	if _, exists := repository.objectDeletions[key]; exists {
		return nil
	}
	repository.objectDeletions[key] = ObjectDeletion{StorageKey: key, RequestedAt: now, NextAttemptAt: now}
	return nil
}

func (repository *MemoryRepository) ListDueObjectDeletions(_ context.Context, now time.Time, limit int) ([]ObjectDeletion, error) {
	if limit <= 0 {
		limit = maxObjectDeletionBatch
	}
	cutoff := now.UTC()
	repository.mu.RLock()
	defer repository.mu.RUnlock()
	result := make([]ObjectDeletion, 0, len(repository.objectDeletions))
	for _, deletion := range repository.objectDeletions {
		if deletion.NextAttemptAt.After(cutoff) {
			continue
		}
		result = append(result, deletion)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].NextAttemptAt.Equal(result[j].NextAttemptAt) {
			return result[i].StorageKey < result[j].StorageKey
		}
		return result[i].NextAttemptAt.Before(result[j].NextAttemptAt)
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (repository *MemoryRepository) CompleteObjectDeletion(_ context.Context, storageKey string) error {
	key := strings.TrimSpace(storageKey)
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if _, exists := repository.objectDeletions[key]; !exists {
		return ErrNotFound
	}
	delete(repository.objectDeletions, key)
	return nil
}

func (repository *MemoryRepository) RescheduleObjectDeletion(_ context.Context, deletion ObjectDeletion, _ time.Time) error {
	key := strings.TrimSpace(deletion.StorageKey)
	repository.mu.Lock()
	defer repository.mu.Unlock()
	current, exists := repository.objectDeletions[key]
	if !exists {
		return ErrNotFound
	}
	current.Attempts = deletion.Attempts
	current.NextAttemptAt = deletion.NextAttemptAt.UTC()
	current.LastError = deletion.LastError
	repository.objectDeletions[key] = current
	return nil
}

func (repository *MemoryRepository) HasObjectDeletion(_ context.Context, storageKey string) (bool, error) {
	repository.mu.RLock()
	defer repository.mu.RUnlock()
	_, exists := repository.objectDeletions[strings.TrimSpace(storageKey)]
	return exists, nil
}
