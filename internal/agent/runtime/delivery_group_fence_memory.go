package runtime

import (
	"context"
	"strings"
	"time"
)

func (r *MemoryRepository) IssueRuntimeDeliveryGroupFence(_ context.Context, source, destination, invocationID, groupID string, now time.Time) (RuntimeDeliveryGroupFence, error) {
	source = strings.TrimSpace(source)
	destination = strings.TrimSpace(destination)
	invocationID = strings.TrimSpace(invocationID)
	groupID = strings.TrimSpace(groupID)
	if source == "" || destination == "" || invocationID == "" || groupID == "" {
		return RuntimeDeliveryGroupFence{}, ErrInvalidRuntimeDeliveryFence
	}
	now = normalizeRuntimeDeliveryGroupTime(now)
	fenceID := RuntimeDeliveryGroupFenceID(source, destination, invocationID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deliveryGroupFences == nil {
		r.deliveryGroupFences = make(map[string]RuntimeDeliveryGroupFence)
	}
	if existing, ok := r.deliveryGroupFences[fenceID]; ok {
		if existing.Source != source || existing.Destination != destination || existing.InvocationID != invocationID {
			return RuntimeDeliveryGroupFence{}, ErrRuntimeDeliveryFenceConflict
		}
		// Issuing the same group again is an idempotent retry. A different
		// group receives the next epoch, so a crashed old worker becomes stale
		// as soon as the new group is prepared.
		if existing.GroupID == groupID {
			return cloneRuntimeDeliveryGroupFence(existing), nil
		}
		if existing.Revision >= MaxRuntimeDeliveryFenceRevision {
			return RuntimeDeliveryGroupFence{}, ErrRuntimeDeliveryFenceConflict
		}
		fence, err := NewRuntimeDeliveryGroupFence(source, destination, invocationID, groupID, existing.Revision+1, now)
		if err != nil {
			return RuntimeDeliveryGroupFence{}, err
		}
		fence.CreatedAt = existing.CreatedAt
		r.deliveryGroupFences[fenceID] = cloneRuntimeDeliveryGroupFence(fence)
		return cloneRuntimeDeliveryGroupFence(fence), nil
	}
	fence, err := NewRuntimeDeliveryGroupFence(source, destination, invocationID, groupID, 1, now)
	if err != nil {
		return RuntimeDeliveryGroupFence{}, err
	}
	r.deliveryGroupFences[fenceID] = cloneRuntimeDeliveryGroupFence(fence)
	return cloneRuntimeDeliveryGroupFence(fence), nil
}

func (r *MemoryRepository) GetRuntimeDeliveryGroupFence(_ context.Context, fenceID string) (RuntimeDeliveryGroupFence, error) {
	fenceID = strings.TrimSpace(fenceID)
	r.mu.Lock()
	defer r.mu.Unlock()
	fence, ok := r.deliveryGroupFences[fenceID]
	if !ok {
		return RuntimeDeliveryGroupFence{}, ErrNotFound
	}
	return cloneRuntimeDeliveryGroupFence(fence), nil
}

func (r *MemoryRepository) BindRuntimeDeliveryGroupFence(_ context.Context, groupID string, expectedRevision int64, fence RuntimeDeliveryGroupFence, now time.Time) (RuntimeDeliveryGroup, error) {
	groupID = strings.TrimSpace(groupID)
	now = normalizeRuntimeDeliveryGroupTime(now)
	if groupID == "" || expectedRevision <= 0 {
		return RuntimeDeliveryGroup{}, ErrConflict
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	group, ok := r.deliveryGroups[groupID]
	if !ok {
		return RuntimeDeliveryGroup{}, ErrNotFound
	}
	if group.Revision != expectedRevision {
		return RuntimeDeliveryGroup{}, ErrConflict
	}
	if group.FenceID != "" {
		if group.FenceID == fence.FenceID && group.FenceRevision == fence.Revision {
			return cloneRuntimeDeliveryGroup(group), nil
		}
		return RuntimeDeliveryGroup{}, ErrRuntimeDeliveryFenceConflict
	}
	normalizedFence, err := fence.Normalize(now)
	if err != nil || normalizedFence.GroupID != group.ID {
		if err != nil {
			return RuntimeDeliveryGroup{}, err
		}
		return RuntimeDeliveryGroup{}, ErrRuntimeDeliveryFenceConflict
	}
	if issued, ok := r.deliveryGroupFences[normalizedFence.FenceID]; !ok || !issued.MatchesIdentity(normalizedFence) {
		return RuntimeDeliveryGroup{}, ErrRuntimeDeliveryFenceConflict
	}
	bound, err := BindRuntimeDeliveryGroupFence(group, normalizedFence, now)
	if err != nil {
		return RuntimeDeliveryGroup{}, err
	}
	bound.Revision = group.Revision + 1
	bound.UpdatedAt = now
	r.deliveryGroups[group.ID] = cloneRuntimeDeliveryGroup(bound)
	return cloneRuntimeDeliveryGroup(bound), nil
}

// acceptRuntimeDeliveryGroupFenceLocked performs the destination-side
// monotonic check. The caller owns r.mu. allowCreate is true for Prepare and
// for Commit/Abort after the corresponding transaction has been found; this
// lets a transaction row recovered from an older backup recreate its fence
// row without allowing an unknown group to advance the scope.
func (r *MemoryRepository) acceptRuntimeDeliveryGroupFenceLocked(envelope RuntimeDeliveryGroupEnvelope, now time.Time, allowCreate bool) error {
	if envelope.FenceRevision == 0 {
		return nil
	}
	fenceID := RuntimeDeliveryGroupFenceID(envelope.Source, envelope.Destination, envelope.InvocationID)
	if envelope.FenceID != fenceID {
		return ErrInvalidRuntimeDeliveryFence
	}
	if r.deliveryGroupFences == nil {
		r.deliveryGroupFences = make(map[string]RuntimeDeliveryGroupFence)
	}
	existing, ok := r.deliveryGroupFences[fenceID]
	if !ok {
		if !allowCreate {
			return ErrRuntimeDeliveryFenceConflict
		}
		fence, err := NewRuntimeDeliveryGroupFence(envelope.Source, envelope.Destination, envelope.InvocationID, envelope.GroupID, envelope.FenceRevision, now)
		if err != nil {
			return err
		}
		r.deliveryGroupFences[fenceID] = cloneRuntimeDeliveryGroupFence(fence)
		return nil
	}
	if existing.Source != envelope.Source || existing.Destination != envelope.Destination || existing.InvocationID != envelope.InvocationID {
		return ErrRuntimeDeliveryFenceConflict
	}
	switch {
	case envelope.FenceRevision < existing.Revision:
		return ErrRuntimeDeliveryFenceStale
	case envelope.FenceRevision == existing.Revision:
		if existing.GroupID != envelope.GroupID {
			return ErrRuntimeDeliveryFenceConflict
		}
		return nil
	default:
		fence, err := NewRuntimeDeliveryGroupFence(envelope.Source, envelope.Destination, envelope.InvocationID, envelope.GroupID, envelope.FenceRevision, now)
		if err != nil {
			return err
		}
		fence.CreatedAt = existing.CreatedAt
		r.deliveryGroupFences[fenceID] = cloneRuntimeDeliveryGroupFence(fence)
		return nil
	}
}

var _ RuntimeDeliveryGroupFenceIssuer = (*MemoryRepository)(nil)
var _ RuntimeDeliveryGroupFenceBinder = (*MemoryRepository)(nil)
