package runtime

import (
	"context"
	"sort"
	"strings"
	"time"
)

func (r *MemoryRepository) EnqueueRuntimeConfigDirectoryFanout(_ context.Context, plan RuntimeConfigDirectoryFanoutPlan, children []RuntimeConfigDirectoryOutbox) (RuntimeConfigDirectoryFanoutPlan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := nowUTC()
	normalizedPlan, err := plan.normalize(now)
	if err != nil {
		return RuntimeConfigDirectoryFanoutPlan{}, err
	}
	if len(children) != len(normalizedPlan.Destinations) {
		return RuntimeConfigDirectoryFanoutPlan{}, ErrConflict
	}
	if r.configDirectoryFanouts == nil {
		r.configDirectoryFanouts = make(map[string]RuntimeConfigDirectoryFanoutPlan)
	}
	if r.configDirectoryOutbox == nil {
		r.configDirectoryOutbox = make(map[string]RuntimeConfigDirectoryOutbox)
	}
	childByID := make(map[string]RuntimeConfigDirectoryOutbox, len(children))
	childDestinations := make(map[string]struct{}, len(children))
	for _, child := range children {
		normalizedChild, childErr := child.normalize(now)
		if childErr != nil {
			return RuntimeConfigDirectoryFanoutPlan{}, childErr
		}
		if normalizedChild.FanoutID != normalizedPlan.ID || normalizedChild.Status != RuntimeConfigDirectoryOutboxQueued || normalizedChild.Attempt != 0 || normalizedChild.Source != normalizedPlan.Source || normalizedChild.Kind != normalizedPlan.Kind || normalizedChild.EntryID != normalizedPlan.EntryID || normalizedChild.EntryRevision != normalizedPlan.EntryRevision || normalizedChild.PreviousDigest != normalizedPlan.PreviousDigest || normalizedChild.BodyDigest != normalizedPlan.BodyDigest || normalizedChild.CorrelationID != normalizedPlan.CorrelationID || normalizedChild.IdempotencyKey != normalizedPlan.IdempotencyKey {
			return RuntimeConfigDirectoryFanoutPlan{}, ErrConflict
		}
		if _, duplicate := childByID[normalizedChild.ID]; duplicate {
			return RuntimeConfigDirectoryFanoutPlan{}, ErrConflict
		}
		childByID[normalizedChild.ID] = normalizedChild
		childDestinations[normalizedChild.Destination] = struct{}{}
	}
	for index, destination := range normalizedPlan.Destinations {
		child, ok := childByID[normalizedPlan.OutboxIDs[index]]
		if !ok || child.Destination != destination {
			return RuntimeConfigDirectoryFanoutPlan{}, ErrConflict
		}
	}
	if len(childDestinations) != len(normalizedPlan.Destinations) {
		return RuntimeConfigDirectoryFanoutPlan{}, ErrConflict
	}
	if existing, ok := r.configDirectoryFanouts[normalizedPlan.ID]; ok {
		if !existing.Matches(normalizedPlan) {
			return RuntimeConfigDirectoryFanoutPlan{}, ErrConflict
		}
		return cloneRuntimeConfigDirectoryFanoutPlan(existing), nil
	}
	for _, child := range childByID {
		if existing, ok := r.configDirectoryOutbox[child.ID]; ok {
			if existing.FanoutID != "" && existing.FanoutID != normalizedPlan.ID {
				return RuntimeConfigDirectoryFanoutPlan{}, ErrConflict
			}
			if !existing.Matches(child) {
				return RuntimeConfigDirectoryFanoutPlan{}, ErrConflict
			}
			continue
		}
		for _, existing := range r.configDirectoryOutbox {
			if existing.DeliveryID == child.DeliveryID && !existing.Matches(child) {
				return RuntimeConfigDirectoryFanoutPlan{}, ErrConflict
			}
		}
	}
	for id, child := range childByID {
		if existing, exists := r.configDirectoryOutbox[id]; exists {
			if existing.FanoutID == "" {
				existing.FanoutID = normalizedPlan.ID
				r.configDirectoryOutbox[id] = cloneRuntimeConfigDirectoryOutbox(existing)
			}
			continue
		}
		if _, exists := r.configDirectoryOutbox[id]; !exists {
			r.configDirectoryOutbox[id] = cloneRuntimeConfigDirectoryOutbox(child)
		}
	}
	r.configDirectoryFanouts[normalizedPlan.ID] = cloneRuntimeConfigDirectoryFanoutPlan(normalizedPlan)
	return cloneRuntimeConfigDirectoryFanoutPlan(normalizedPlan), nil
}

func (r *MemoryRepository) GetRuntimeConfigDirectoryFanout(_ context.Context, id string) (RuntimeConfigDirectoryFanoutPlan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	plan, ok := r.configDirectoryFanouts[strings.TrimSpace(id)]
	if !ok {
		return RuntimeConfigDirectoryFanoutPlan{}, ErrNotFound
	}
	return cloneRuntimeConfigDirectoryFanoutPlan(plan), nil
}

func (r *MemoryRepository) ListRuntimeConfigDirectoryFanouts(_ context.Context, source string, status RuntimeConfigDirectoryFanoutStatus, limit int) ([]RuntimeConfigDirectoryFanoutPlan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	source = strings.TrimSpace(source)
	items := make([]RuntimeConfigDirectoryFanoutPlan, 0, limit)
	for _, plan := range r.configDirectoryFanouts {
		if source != "" && plan.Source != source {
			continue
		}
		if status != "" && plan.Status != status {
			continue
		}
		items = append(items, cloneRuntimeConfigDirectoryFanoutPlan(plan))
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].CreatedAt.Equal(items[right].CreatedAt) {
			return items[left].ID < items[right].ID
		}
		return items[left].CreatedAt.Before(items[right].CreatedAt)
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (r *MemoryRepository) ReconcileRuntimeConfigDirectoryFanout(_ context.Context, id string, now time.Time) (RuntimeConfigDirectoryFanoutPlan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id = strings.TrimSpace(id)
	plan, ok := r.configDirectoryFanouts[id]
	if !ok {
		return RuntimeConfigDirectoryFanoutPlan{}, ErrNotFound
	}
	children := make([]RuntimeConfigDirectoryOutbox, 0, len(plan.OutboxIDs))
	for _, childID := range plan.OutboxIDs {
		child, exists := r.configDirectoryOutbox[childID]
		if !exists {
			return RuntimeConfigDirectoryFanoutPlan{}, ErrConflict
		}
		children = append(children, child)
	}
	reconciled, err := plan.ReconcileSummary(children, now)
	if err != nil {
		return RuntimeConfigDirectoryFanoutPlan{}, err
	}
	r.configDirectoryFanouts[id] = cloneRuntimeConfigDirectoryFanoutPlan(reconciled)
	return cloneRuntimeConfigDirectoryFanoutPlan(reconciled), nil
}

var _ RuntimeConfigDirectoryFanoutRepository = (*MemoryRepository)(nil)
