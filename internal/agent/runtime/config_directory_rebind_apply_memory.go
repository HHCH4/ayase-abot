package runtime

import (
	"context"
	"sort"
	"strings"
	"time"
)

func (r *MemoryRepository) CreateRuntimeConfigDirectoryRebindConfirmation(ctx context.Context, plan RuntimeConfigDirectoryRebindPlan, idempotencyKey string, now time.Time) (RuntimeConfigDirectoryRebindConfirmation, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return RuntimeConfigDirectoryRebindConfirmation{}, err
		}
	}
	if r == nil {
		return RuntimeConfigDirectoryRebindConfirmation{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	if now.IsZero() {
		now = nowUTC()
	} else {
		now = now.UTC()
	}
	plan, err := plan.Normalize(now)
	if err != nil {
		return RuntimeConfigDirectoryRebindConfirmation{}, err
	}
	if plan.Status != RuntimeConfigDirectoryRebindValidated {
		return RuntimeConfigDirectoryRebindConfirmation{}, ErrRuntimeConfigDirectoryRebindConflict
	}
	confirmation, err := NewRuntimeConfigDirectoryRebindConfirmation(plan, idempotencyKey, now)
	if err != nil {
		return RuntimeConfigDirectoryRebindConfirmation{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rebindConfirmations == nil {
		r.rebindConfirmations = make(map[string]RuntimeConfigDirectoryRebindConfirmation)
	}
	if existing, ok := r.rebindConfirmations[confirmation.ID]; ok {
		if !runtimeConfigDirectoryRebindConfirmationMatches(existing, confirmation) {
			return RuntimeConfigDirectoryRebindConfirmation{}, ErrRuntimeConfigDirectoryRebindConfirmationConflict
		}
		if existing.Status == RuntimeConfigDirectoryRebindConfirmationConfirmed && !existing.ExpiresAt.After(now) {
			existing.Status = RuntimeConfigDirectoryRebindConfirmationExpired
			existing.Revision++
			existing = cloneRuntimeConfigDirectoryRebindConfirmation(existing)
			r.rebindConfirmations[existing.ID] = existing
			return RuntimeConfigDirectoryRebindConfirmation{}, ErrRuntimeConfigDirectoryRebindConfirmationExpired
		}
		return cloneRuntimeConfigDirectoryRebindConfirmation(existing), nil
	}
	// A plan has one active confirmation at a time. A new idempotency key may
	// not race an existing queued/processing apply for the same target.
	for _, existing := range r.rebindConfirmations {
		if existing.PlanID == confirmation.PlanID && existing.Status == RuntimeConfigDirectoryRebindConfirmationConfirmed && existing.ExpiresAt.After(now) {
			return RuntimeConfigDirectoryRebindConfirmation{}, ErrRuntimeConfigDirectoryRebindConfirmationConflict
		}
	}
	r.rebindConfirmations[confirmation.ID] = cloneRuntimeConfigDirectoryRebindConfirmation(confirmation)
	return cloneRuntimeConfigDirectoryRebindConfirmation(confirmation), nil
}

func runtimeConfigDirectoryRebindConfirmationMatches(left, right RuntimeConfigDirectoryRebindConfirmation) bool {
	left, leftErr := left.Normalize(time.Time{})
	right, rightErr := right.Normalize(time.Time{})
	if leftErr != nil || rightErr != nil {
		return false
	}
	return left.ID == right.ID && left.PlanID == right.PlanID && left.InvocationID == right.InvocationID && left.Source == right.Source && left.Destination == right.Destination && left.ConfigSnapshotDigest == right.ConfigSnapshotDigest && left.CapabilitiesDigest == right.CapabilitiesDigest && left.IdempotencyKey == right.IdempotencyKey
}

func (r *MemoryRepository) GetRuntimeConfigDirectoryRebindConfirmation(ctx context.Context, id string) (RuntimeConfigDirectoryRebindConfirmation, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return RuntimeConfigDirectoryRebindConfirmation{}, err
		}
	}
	if r == nil {
		return RuntimeConfigDirectoryRebindConfirmation{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.rebindConfirmations[strings.TrimSpace(id)]
	if !ok {
		return RuntimeConfigDirectoryRebindConfirmation{}, ErrNotFound
	}
	return cloneRuntimeConfigDirectoryRebindConfirmation(item), nil
}

func (r *MemoryRepository) CommitRuntimeConfigDirectoryRebindApply(ctx context.Context, confirmationID string, apply RuntimeConfigDirectoryRebindApply, now time.Time) (RuntimeConfigDirectoryRebindApply, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return RuntimeConfigDirectoryRebindApply{}, err
		}
	}
	if r == nil {
		return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	if now.IsZero() {
		now = nowUTC()
	} else {
		now = now.UTC()
	}
	apply, err := apply.Normalize(now)
	if err != nil {
		return RuntimeConfigDirectoryRebindApply{}, err
	}
	confirmationID = strings.TrimSpace(confirmationID)
	if confirmationID == "" || confirmationID != apply.ConfirmationID {
		return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindConfirmationConflict
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rebindApplies == nil {
		r.rebindApplies = make(map[string]RuntimeConfigDirectoryRebindApply)
	}
	if existing, ok := r.rebindApplies[apply.ID]; ok {
		if !existing.MatchesIdentity(apply) {
			return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindApplyConflict
		}
		return cloneRuntimeConfigDirectoryRebindApply(existing), nil
	}
	confirmation, ok := r.rebindConfirmations[confirmationID]
	if !ok {
		return RuntimeConfigDirectoryRebindApply{}, ErrNotFound
	}
	if confirmation.Status == RuntimeConfigDirectoryRebindConfirmationConfirmed && !confirmation.ExpiresAt.After(now) {
		confirmation.Status = RuntimeConfigDirectoryRebindConfirmationExpired
		confirmation.Revision++
		r.rebindConfirmations[confirmation.ID] = cloneRuntimeConfigDirectoryRebindConfirmation(confirmation)
		return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindConfirmationExpired
	}
	if confirmation.Status != RuntimeConfigDirectoryRebindConfirmationConfirmed {
		return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindConfirmationConflict
	}
	confirmationDigest, err := RuntimeConfigDirectoryRebindConfirmationDigest(confirmation)
	if err != nil || confirmationDigest != apply.ConfirmationDigest {
		return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindConfirmationConflict
	}
	if confirmation.PlanID != apply.PlanID || confirmation.InvocationID != apply.InvocationID || confirmation.Source != apply.Source || confirmation.Destination != apply.Destination || confirmation.ConfigSnapshotDigest != apply.ConfigSnapshotDigest {
		return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindConfirmationConflict
	}
	for _, existing := range r.rebindApplies {
		if existing.PlanID != apply.PlanID || existing.Destination != apply.Destination {
			continue
		}
		if existing.SnapshotRevision == apply.SnapshotRevision && existing.EventSequence == apply.EventSequence && existing.SnapshotDigest == apply.SnapshotDigest && existing.Status != RuntimeConfigDirectoryRebindApplyFailed {
			return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindApplyConflict
		}
	}
	consumedAt := now
	confirmation.Status = RuntimeConfigDirectoryRebindConfirmationConsumed
	confirmation.ConsumedAt = &consumedAt
	confirmation.Revision++
	r.rebindConfirmations[confirmation.ID] = cloneRuntimeConfigDirectoryRebindConfirmation(confirmation)
	r.rebindApplies[apply.ID] = cloneRuntimeConfigDirectoryRebindApply(apply)
	return cloneRuntimeConfigDirectoryRebindApply(apply), nil
}

func (r *MemoryRepository) EnqueueRuntimeConfigDirectoryRebindApply(ctx context.Context, item RuntimeConfigDirectoryRebindApply) (RuntimeConfigDirectoryRebindApply, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return RuntimeConfigDirectoryRebindApply{}, err
		}
	}
	if r == nil {
		return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	now := nowUTC()
	normalized, err := item.Normalize(now)
	if err != nil {
		return RuntimeConfigDirectoryRebindApply{}, err
	}
	if normalized.Status != RuntimeConfigDirectoryRebindApplyQueued || normalized.Attempt != 0 {
		return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindApplyConflict
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rebindApplies == nil {
		r.rebindApplies = make(map[string]RuntimeConfigDirectoryRebindApply)
	}
	if existing, ok := r.rebindApplies[normalized.ID]; ok {
		if !existing.MatchesIdentity(normalized) {
			return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindApplyConflict
		}
		return cloneRuntimeConfigDirectoryRebindApply(existing), nil
	}
	r.rebindApplies[normalized.ID] = cloneRuntimeConfigDirectoryRebindApply(normalized)
	return cloneRuntimeConfigDirectoryRebindApply(normalized), nil
}

func (r *MemoryRepository) GetRuntimeConfigDirectoryRebindApply(ctx context.Context, id string) (RuntimeConfigDirectoryRebindApply, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return RuntimeConfigDirectoryRebindApply{}, err
		}
	}
	if r == nil {
		return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.rebindApplies[strings.TrimSpace(id)]
	if !ok {
		return RuntimeConfigDirectoryRebindApply{}, ErrNotFound
	}
	return cloneRuntimeConfigDirectoryRebindApply(item), nil
}

func (r *MemoryRepository) ListRuntimeConfigDirectoryRebindApplies(ctx context.Context, planID, invocationID string, status RuntimeConfigDirectoryRebindApplyStatus, limit int) ([]RuntimeConfigDirectoryRebindApply, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if r == nil {
		return nil, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	planID = strings.TrimSpace(planID)
	invocationID = strings.TrimSpace(invocationID)
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]RuntimeConfigDirectoryRebindApply, 0, limit)
	for _, item := range r.rebindApplies {
		if planID != "" && item.PlanID != planID {
			continue
		}
		if invocationID != "" && item.InvocationID != invocationID {
			continue
		}
		if status != "" && item.Status != status {
			continue
		}
		items = append(items, cloneRuntimeConfigDirectoryRebindApply(item))
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

func (r *MemoryRepository) ClaimRuntimeConfigDirectoryRebindApply(ctx context.Context, owner string, now time.Time, ttl time.Duration) (RuntimeConfigDirectoryRebindApply, bool, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return RuntimeConfigDirectoryRebindApply{}, false, err
		}
	}
	if r == nil {
		return RuntimeConfigDirectoryRebindApply{}, false, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeConfigDirectoryRebindApplyOwnerLength || ttl <= 0 || ttl > MaxRuntimeConfigDirectoryRebindApplyLease {
		return RuntimeConfigDirectoryRebindApply{}, false, ErrRuntimeConfigDirectoryRebindApplyConflict
	}
	if now.IsZero() {
		now = nowUTC()
	} else {
		now = now.UTC()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]RuntimeConfigDirectoryRebindApply, 0, len(r.rebindApplies))
	for _, item := range r.rebindApplies {
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
		item, ok := r.rebindApplies[candidate.ID]
		if !ok || item.Status.terminal() {
			continue
		}
		if item.Status == RuntimeConfigDirectoryRebindApplyQueued && item.AvailableAt.After(now) {
			continue
		}
		if item.Status == RuntimeConfigDirectoryRebindApplyProcessing {
			if item.LeaseOwner == "" || item.LeaseExpiresAt == nil || item.LeaseExpiresAt.IsZero() {
				item.Status = RuntimeConfigDirectoryRebindApplyFailed
				item.LastError = "rebind apply processing 元数据无效"
				item.LeaseOwner = ""
				item.LeaseExpiresAt = nil
				item.Revision++
				item.UpdatedAt = now
				r.rebindApplies[item.ID] = item
				continue
			}
			if item.LeaseExpiresAt.After(now) {
				continue
			}
		}
		if item.Attempt >= MaxRuntimeConfigDirectoryRebindApplyAttempts {
			item.Status = RuntimeConfigDirectoryRebindApplyFailed
			item.LastError = "Runtime rebind apply 达到最大投递次数"
			item.LeaseOwner = ""
			item.LeaseExpiresAt = nil
			item.Revision++
			item.UpdatedAt = now
			r.rebindApplies[item.ID] = item
			continue
		}
		item.Attempt++
		item.Status = RuntimeConfigDirectoryRebindApplyProcessing
		expires := now.Add(ttl)
		item.LeaseOwner = owner
		item.LeaseExpiresAt = &expires
		item.Revision++
		item.UpdatedAt = now
		r.rebindApplies[item.ID] = item
		return cloneRuntimeConfigDirectoryRebindApply(item), true, nil
	}
	return RuntimeConfigDirectoryRebindApply{}, false, nil
}

func (r *MemoryRepository) CompleteRuntimeConfigDirectoryRebindApply(ctx context.Context, id, owner string, now time.Time) (bool, error) {
	return r.mutateRuntimeConfigDirectoryRebindApply(ctx, id, owner, now, func(item *RuntimeConfigDirectoryRebindApply) error {
		finished := now.UTC()
		item.Status = RuntimeConfigDirectoryRebindApplyCompleted
		item.CompletedAt = &finished
		return nil
	})
}

func (r *MemoryRepository) RetryRuntimeConfigDirectoryRebindApply(ctx context.Context, id, owner string, now time.Time, message string) (bool, error) {
	return r.mutateRuntimeConfigDirectoryRebindApply(ctx, id, owner, now, func(item *RuntimeConfigDirectoryRebindApply) error {
		item.LastError = SanitizeRuntimeString(strings.TrimSpace(message))
		if len(item.LastError) > MaxRuntimeConfigDirectoryRebindApplyErrorLength {
			item.LastError = item.LastError[:MaxRuntimeConfigDirectoryRebindApplyErrorLength]
		}
		if item.Attempt >= MaxRuntimeConfigDirectoryRebindApplyAttempts {
			item.Status = RuntimeConfigDirectoryRebindApplyFailed
		} else {
			item.Status = RuntimeConfigDirectoryRebindApplyQueued
			item.AvailableAt = now.UTC().Add(RuntimeConfigDirectoryRebindApplyBackoff(item.Attempt))
		}
		return nil
	})
}

func (r *MemoryRepository) FailRuntimeConfigDirectoryRebindApply(ctx context.Context, id, owner string, now time.Time, message string) (bool, error) {
	return r.mutateRuntimeConfigDirectoryRebindApply(ctx, id, owner, now, func(item *RuntimeConfigDirectoryRebindApply) error {
		item.Status = RuntimeConfigDirectoryRebindApplyFailed
		item.LastError = SanitizeRuntimeString(strings.TrimSpace(message))
		if len(item.LastError) > MaxRuntimeConfigDirectoryRebindApplyErrorLength {
			item.LastError = item.LastError[:MaxRuntimeConfigDirectoryRebindApplyErrorLength]
		}
		return nil
	})
}

func (r *MemoryRepository) mutateRuntimeConfigDirectoryRebindApply(ctx context.Context, id, owner string, now time.Time, mutate func(*RuntimeConfigDirectoryRebindApply) error) (bool, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return false, err
		}
	}
	if r == nil {
		return false, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeConfigDirectoryRebindApplyOwnerLength {
		return false, ErrRuntimeConfigDirectoryRebindApplyConflict
	}
	if now.IsZero() {
		now = nowUTC()
	} else {
		now = now.UTC()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.rebindApplies[strings.TrimSpace(id)]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != RuntimeConfigDirectoryRebindApplyProcessing || item.LeaseOwner != owner || item.LeaseExpiresAt == nil || !item.LeaseExpiresAt.After(now) {
		return false, nil
	}
	if err := mutate(&item); err != nil {
		return false, err
	}
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	item.Revision++
	item.UpdatedAt = now
	r.rebindApplies[item.ID] = item
	return true, nil
}

func (r *MemoryRepository) AcceptRuntimeConfigDirectoryRebind(ctx context.Context, envelope RuntimeConfigDirectoryRebindApplyEnvelope, projection RuntimeCheckpointProjection) (bool, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return false, err
		}
	}
	if r == nil {
		return false, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	normalized, projection, err := normalizeRuntimeConfigDirectoryRebindApplyPair(envelope, projection)
	if err != nil {
		return false, err
	}
	record := RuntimeConfigDirectoryRebindInboxRecordFromEnvelope(normalized, projection, nowUTC())
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rebindApplyInbox == nil {
		r.rebindApplyInbox = make(map[string]RuntimeConfigDirectoryRebindInboxRecord)
	}
	if existing, ok := r.rebindApplyInbox[normalized.ApplyID]; ok {
		if !existing.Matches(normalized, projection) {
			return false, ErrRuntimeConfigDirectoryRebindApplyConflict
		}
		return true, nil
	}
	r.rebindApplyInbox[normalized.ApplyID] = cloneRuntimeConfigDirectoryRebindInboxRecord(record)
	return false, nil
}

func (r *MemoryRepository) GetRuntimeConfigDirectoryRebind(ctx context.Context, id string) (RuntimeConfigDirectoryRebindInboxRecord, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return RuntimeConfigDirectoryRebindInboxRecord{}, err
		}
	}
	if r == nil {
		return RuntimeConfigDirectoryRebindInboxRecord{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.rebindApplyInbox[strings.TrimSpace(id)]
	if !ok {
		return RuntimeConfigDirectoryRebindInboxRecord{}, ErrNotFound
	}
	return cloneRuntimeConfigDirectoryRebindInboxRecord(item), nil
}

var _ RuntimeConfigDirectoryRebindConfirmationRepository = (*MemoryRepository)(nil)
var _ RuntimeConfigDirectoryRebindApplyRepository = (*MemoryRepository)(nil)
var _ RuntimeConfigDirectoryRebindApplyCommitRepository = (*MemoryRepository)(nil)
var _ RuntimeConfigDirectoryRebindInbox = (*MemoryRepository)(nil)
