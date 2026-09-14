package runtime

import (
	"context"
	"sort"
	"strings"
	"time"
)

func (r *MemoryRepository) EnqueueRuntimeDeliveryGroupSaga(_ context.Context, item RuntimeDeliveryGroupSaga) (RuntimeDeliveryGroupSaga, error) {
	now := nowUTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	normalized, err := item.Normalize(now)
	if err != nil {
		return RuntimeDeliveryGroupSaga{}, err
	}
	if r.deliveryGroupSagas == nil {
		r.deliveryGroupSagas = make(map[string]RuntimeDeliveryGroupSaga)
	}
	if existing, ok := r.deliveryGroupSagas[normalized.ID]; ok {
		if !existing.MatchesIdentity(normalized) {
			return RuntimeDeliveryGroupSaga{}, ErrConflict
		}
		return cloneRuntimeDeliveryGroupSaga(existing), nil
	}
	r.deliveryGroupSagas[normalized.ID] = cloneRuntimeDeliveryGroupSaga(normalized)
	return cloneRuntimeDeliveryGroupSaga(normalized), nil
}

func (r *MemoryRepository) GetRuntimeDeliveryGroupSaga(_ context.Context, id string) (RuntimeDeliveryGroupSaga, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.deliveryGroupSagas[strings.TrimSpace(id)]
	if !ok {
		return RuntimeDeliveryGroupSaga{}, ErrNotFound
	}
	return cloneRuntimeDeliveryGroupSaga(item), nil
}

func (r *MemoryRepository) ListRuntimeDeliveryGroupSagas(_ context.Context, invocationID string, state RuntimeDeliveryGroupSagaState, limit int) ([]RuntimeDeliveryGroupSaga, error) {
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
	state = RuntimeDeliveryGroupSagaState(strings.TrimSpace(strings.ToLower(string(state))))
	if state != "" && !validRuntimeDeliveryGroupSagaState(state) {
		return nil, ErrInvalidRuntimeDeliveryGroupSaga
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]RuntimeDeliveryGroupSaga, 0, len(r.deliveryGroupSagas))
	for _, item := range r.deliveryGroupSagas {
		if invocationID != "" && item.InvocationID != invocationID {
			continue
		}
		if state != "" && item.State != state {
			continue
		}
		items = append(items, cloneRuntimeDeliveryGroupSaga(item))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].UpdatedAt.Before(items[j].UpdatedAt)
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (r *MemoryRepository) AdvanceRuntimeDeliveryGroupSaga(_ context.Context, id string, expectedRevision int64, state RuntimeDeliveryGroupSagaState, remotePhase RuntimeDeliveryGroupSettlementRemotePhase, message string, now time.Time) (RuntimeDeliveryGroupSaga, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id = strings.TrimSpace(id)
	item, ok := r.deliveryGroupSagas[id]
	if !ok {
		return RuntimeDeliveryGroupSaga{}, false, ErrNotFound
	}
	updated, duplicate, err := AdvanceRuntimeDeliveryGroupSaga(item, expectedRevision, state, remotePhase, message, now)
	if err != nil {
		return RuntimeDeliveryGroupSaga{}, false, err
	}
	if !duplicate {
		r.deliveryGroupSagas[id] = cloneRuntimeDeliveryGroupSaga(updated)
	}
	return cloneRuntimeDeliveryGroupSaga(updated), duplicate, nil
}

var _ RuntimeDeliveryGroupSagaRepository = (*MemoryRepository)(nil)
