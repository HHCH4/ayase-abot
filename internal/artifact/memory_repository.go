package artifact

import (
	"context"
	"sort"
	"strings"
	"sync"
)

// MemoryRepository is used by unit/HTTP tests and by lightweight embedders.
// It copies metadata at every boundary so a caller cannot mutate an immutable
// artifact through a returned map or time pointer.
type MemoryRepository struct {
	mu              sync.RWMutex
	items           map[string]Artifact
	objectDeletions map[string]ObjectDeletion
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{items: make(map[string]Artifact), objectDeletions: make(map[string]ObjectDeletion)}
}

func (repository *MemoryRepository) Create(_ context.Context, item Artifact) error {
	if err := item.Validate(); err != nil {
		return err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if _, exists := repository.items[item.ID]; exists {
		return ErrConflict
	}
	repository.items[item.ID] = cloneArtifact(item)
	return nil
}

func (repository *MemoryRepository) Update(_ context.Context, item Artifact, expectedVersion int64) error {
	if err := item.Validate(); err != nil {
		return err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	current, exists := repository.items[item.ID]
	if !exists {
		return ErrNotFound
	}
	if expectedVersion > 0 && int64(current.Version) != expectedVersion {
		return ErrConflict
	}
	if item.Version != current.Version+1 {
		return ErrConflict
	}
	repository.items[item.ID] = cloneArtifact(item)
	return nil
}

func (repository *MemoryRepository) Get(_ context.Context, id string) (Artifact, error) {
	repository.mu.RLock()
	defer repository.mu.RUnlock()
	item, exists := repository.items[strings.TrimSpace(id)]
	if !exists {
		return Artifact{}, ErrNotFound
	}
	return cloneArtifact(item), nil
}

func (repository *MemoryRepository) List(_ context.Context, userID, conversationID string) ([]Artifact, error) {
	repository.mu.RLock()
	defer repository.mu.RUnlock()
	result := make([]Artifact, 0)
	for _, item := range repository.items {
		if item.UserID != strings.TrimSpace(userID) || (conversationID != "" && item.ConversationID != strings.TrimSpace(conversationID)) {
			continue
		}
		if item.Status == StatusDeleting || item.Status == StatusFailed {
			continue
		}
		result = append(result, cloneArtifact(item))
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result, nil
}

func (repository *MemoryRepository) FindReadyByProducerDigest(_ context.Context, producerType, producerID, digest string) (Artifact, error) {
	repository.mu.RLock()
	defer repository.mu.RUnlock()
	for _, item := range repository.items {
		if item.Status == StatusReady && item.ProducerType == producerType && item.ProducerID == producerID && item.Digest == digest {
			return cloneArtifact(item), nil
		}
	}
	return Artifact{}, ErrNotFound
}

func (repository *MemoryRepository) CountByStorageKey(_ context.Context, storageKey string) (int, error) {
	repository.mu.RLock()
	defer repository.mu.RUnlock()
	count := 0
	for _, item := range repository.items {
		if item.StorageKey == storageKey && item.Status != StatusFailed {
			count++
		}
	}
	return count, nil
}

func (repository *MemoryRepository) Delete(_ context.Context, id string) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if _, exists := repository.items[strings.TrimSpace(id)]; !exists {
		return ErrNotFound
	}
	delete(repository.items, strings.TrimSpace(id))
	return nil
}

func (repository *MemoryRepository) DeleteByConversation(_ context.Context, conversationID string) ([]Artifact, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	var result []Artifact
	for id, item := range repository.items {
		if item.ConversationID == strings.TrimSpace(conversationID) {
			result = append(result, cloneArtifact(item))
			delete(repository.items, id)
		}
	}
	return result, nil
}

func cloneArtifact(item Artifact) Artifact {
	item.Metadata = cloneMetadata(item.Metadata)
	item.ExpiresAt = cloneTime(item.ExpiresAt)
	item.CreatedAt = item.CreatedAt.UTC()
	item.UpdatedAt = item.UpdatedAt.UTC()
	return item
}
