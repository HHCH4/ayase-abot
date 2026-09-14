package runtime

import (
	"context"
	"sort"
	"strings"
)

// EnqueueRuntimeConfigDirectoryRebindPlan persists one validated, metadata-only
// plan. The stable ID and optional idempotency key make retries safe while the
// immutable comparison prevents a caller from reusing an existing identity for
// a different invocation boundary or route capability snapshot.
func (r *MemoryRepository) EnqueueRuntimeConfigDirectoryRebindPlan(ctx context.Context, plan RuntimeConfigDirectoryRebindPlan) (RuntimeConfigDirectoryRebindPlan, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return RuntimeConfigDirectoryRebindPlan{}, err
		}
	}
	if r == nil {
		return RuntimeConfigDirectoryRebindPlan{}, ErrRuntimeConfigDirectoryRebindUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	normalized, err := plan.normalize(nowUTC())
	if err != nil {
		return RuntimeConfigDirectoryRebindPlan{}, err
	}
	if r.configDirectoryRebindPlans == nil {
		r.configDirectoryRebindPlans = make(map[string]RuntimeConfigDirectoryRebindPlan)
	}
	if existing, ok := r.configDirectoryRebindPlans[normalized.ID]; ok {
		if !existing.Matches(normalized) {
			return RuntimeConfigDirectoryRebindPlan{}, ErrRuntimeConfigDirectoryRebindConflict
		}
		return cloneRuntimeConfigDirectoryRebindPlan(existing), nil
	}
	if normalized.IdempotencyKey != "" {
		for _, existing := range r.configDirectoryRebindPlans {
			if existing.Source != normalized.Source || existing.InvocationID != normalized.InvocationID || existing.IdempotencyKey != normalized.IdempotencyKey {
				continue
			}
			if !existing.Matches(normalized) {
				return RuntimeConfigDirectoryRebindPlan{}, ErrRuntimeConfigDirectoryRebindConflict
			}
			return cloneRuntimeConfigDirectoryRebindPlan(existing), nil
		}
	}
	r.configDirectoryRebindPlans[normalized.ID] = cloneRuntimeConfigDirectoryRebindPlan(normalized)
	return cloneRuntimeConfigDirectoryRebindPlan(normalized), nil
}

func (r *MemoryRepository) GetRuntimeConfigDirectoryRebindPlan(ctx context.Context, id string) (RuntimeConfigDirectoryRebindPlan, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return RuntimeConfigDirectoryRebindPlan{}, err
		}
	}
	if r == nil {
		return RuntimeConfigDirectoryRebindPlan{}, ErrRuntimeConfigDirectoryRebindUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	plan, ok := r.configDirectoryRebindPlans[strings.TrimSpace(id)]
	if !ok {
		return RuntimeConfigDirectoryRebindPlan{}, ErrNotFound
	}
	return cloneRuntimeConfigDirectoryRebindPlan(plan), nil
}

func (r *MemoryRepository) ListRuntimeConfigDirectoryRebindPlans(ctx context.Context, source, invocationID string, status RuntimeConfigDirectoryRebindStatus, limit int) ([]RuntimeConfigDirectoryRebindPlan, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if r == nil {
		return nil, ErrRuntimeConfigDirectoryRebindUnavailable
	}
	if status != "" && !status.valid() {
		return nil, ErrInvalidRuntimeConfigDirectoryRebind
	}
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	source = strings.TrimSpace(source)
	invocationID = strings.TrimSpace(invocationID)
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]RuntimeConfigDirectoryRebindPlan, 0, limit)
	for _, plan := range r.configDirectoryRebindPlans {
		if source != "" && plan.Source != source {
			continue
		}
		if invocationID != "" && plan.InvocationID != invocationID {
			continue
		}
		if status != "" && plan.Status != status {
			continue
		}
		items = append(items, cloneRuntimeConfigDirectoryRebindPlan(plan))
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

var _ RuntimeConfigDirectoryRebindPlanRepository = (*MemoryRepository)(nil)
