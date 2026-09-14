package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func (r *MemoryRepository) CreateRuntimeConfigDirectoryRebindMultiConfirmation(ctx context.Context, plan RuntimeConfigDirectoryRebindPlan, idempotencyKey string, now time.Time) (RuntimeConfigDirectoryRebindMultiConfirmation, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return RuntimeConfigDirectoryRebindMultiConfirmation{}, err
		}
	}
	if r == nil {
		return RuntimeConfigDirectoryRebindMultiConfirmation{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	if now.IsZero() {
		now = nowUTC()
	} else {
		now = now.UTC()
	}
	plan, err := plan.Normalize(now)
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiConfirmation{}, err
	}
	if plan.Status != RuntimeConfigDirectoryRebindValidated || len(plan.Destinations) < 2 {
		return RuntimeConfigDirectoryRebindMultiConfirmation{}, ErrRuntimeConfigDirectoryRebindMultiConfirmationConflict
	}
	confirmation, err := NewRuntimeConfigDirectoryRebindMultiConfirmation(plan, idempotencyKey, now)
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiConfirmation{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rebindMultiConfirmations == nil {
		r.rebindMultiConfirmations = make(map[string]RuntimeConfigDirectoryRebindMultiConfirmation)
	}
	if existing, ok := r.rebindMultiConfirmations[confirmation.ID]; ok {
		if !runtimeConfigDirectoryRebindMultiConfirmationMatches(existing, confirmation) {
			return RuntimeConfigDirectoryRebindMultiConfirmation{}, ErrRuntimeConfigDirectoryRebindMultiConfirmationConflict
		}
		if existing.Status == RuntimeConfigDirectoryRebindMultiConfirmationConfirmed && !existing.ExpiresAt.After(now) {
			existing.Status = RuntimeConfigDirectoryRebindMultiConfirmationExpired
			existing.Revision++
			existing = cloneRuntimeConfigDirectoryRebindMultiConfirmation(existing)
			r.rebindMultiConfirmations[existing.ID] = existing
			return RuntimeConfigDirectoryRebindMultiConfirmation{}, ErrRuntimeConfigDirectoryRebindMultiConfirmationExpired
		}
		return cloneRuntimeConfigDirectoryRebindMultiConfirmation(existing), nil
	}
	for _, existing := range r.rebindMultiConfirmations {
		if existing.PlanID == confirmation.PlanID && existing.Status == RuntimeConfigDirectoryRebindMultiConfirmationConfirmed && existing.ExpiresAt.After(now) {
			return RuntimeConfigDirectoryRebindMultiConfirmation{}, ErrRuntimeConfigDirectoryRebindMultiConfirmationConflict
		}
	}
	r.rebindMultiConfirmations[confirmation.ID] = cloneRuntimeConfigDirectoryRebindMultiConfirmation(confirmation)
	return cloneRuntimeConfigDirectoryRebindMultiConfirmation(confirmation), nil
}

func runtimeConfigDirectoryRebindMultiConfirmationMatches(left, right RuntimeConfigDirectoryRebindMultiConfirmation) bool {
	left, leftErr := left.Normalize(time.Time{})
	right, rightErr := right.Normalize(time.Time{})
	if leftErr != nil || rightErr != nil {
		return false
	}
	return left.ID == right.ID && left.PlanID == right.PlanID && left.InvocationID == right.InvocationID && left.Source == right.Source && left.ConfigSnapshotDigest == right.ConfigSnapshotDigest && left.CapabilitiesDigest == right.CapabilitiesDigest && left.IdempotencyKey == right.IdempotencyKey && equalRuntimeConfigDirectoryStrings(left.Destinations, right.Destinations)
}

func (r *MemoryRepository) GetRuntimeConfigDirectoryRebindMultiConfirmation(ctx context.Context, id string) (RuntimeConfigDirectoryRebindMultiConfirmation, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return RuntimeConfigDirectoryRebindMultiConfirmation{}, err
		}
	}
	if r == nil {
		return RuntimeConfigDirectoryRebindMultiConfirmation{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.rebindMultiConfirmations[strings.TrimSpace(id)]
	if !ok {
		return RuntimeConfigDirectoryRebindMultiConfirmation{}, ErrNotFound
	}
	return cloneRuntimeConfigDirectoryRebindMultiConfirmation(item), nil
}

func validateRuntimeConfigDirectoryRebindMultiCommitChildren(parent RuntimeConfigDirectoryRebindMultiApply, children []RuntimeConfigDirectoryRebindApply, now time.Time) (map[string]RuntimeConfigDirectoryRebindApply, error) {
	if len(children) != len(parent.Destinations) {
		return nil, fmt.Errorf("%w: child 数量不一致", ErrInvalidRuntimeConfigDirectoryRebindMultiApply)
	}
	byDestination := make(map[string]RuntimeConfigDirectoryRebindApply, len(children))
	for _, child := range children {
		child, err := child.Normalize(now)
		if err != nil {
			return nil, err
		}
		if child.ParentID != parent.ID || child.PlanID != parent.PlanID || child.ConfirmationID != parent.ConfirmationID || child.ConfirmationDigest != parent.ConfirmationDigest || child.Source != parent.Source || child.InvocationID != parent.InvocationID || child.BoundaryStatus != parent.BoundaryStatus || child.ConfigSnapshotDigest != parent.ConfigSnapshotDigest || child.SnapshotRevision != parent.SnapshotRevision || child.EventSequence != parent.EventSequence || child.SnapshotDigest != parent.SnapshotDigest || child.IdempotencyKey != parent.IdempotencyKey {
			return nil, fmt.Errorf("%w: child 与 parent identity 不一致", ErrRuntimeConfigDirectoryRebindMultiApplyConflict)
		}
		if _, exists := byDestination[child.Destination]; exists {
			return nil, fmt.Errorf("%w: child destination 重复", ErrRuntimeConfigDirectoryRebindMultiApplyConflict)
		}
		byDestination[child.Destination] = cloneRuntimeConfigDirectoryRebindApply(child)
	}
	for index, destination := range parent.Destinations {
		child, ok := byDestination[destination]
		if !ok || child.ID != parent.ChildApplyIDs[index] {
			return nil, fmt.Errorf("%w: child route/ID 不一致", ErrRuntimeConfigDirectoryRebindMultiApplyConflict)
		}
	}
	return byDestination, nil
}

func (r *MemoryRepository) CommitRuntimeConfigDirectoryRebindMultiApply(ctx context.Context, confirmationID string, parent RuntimeConfigDirectoryRebindMultiApply, children []RuntimeConfigDirectoryRebindApply, now time.Time) (RuntimeConfigDirectoryRebindMultiApply, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return RuntimeConfigDirectoryRebindMultiApply{}, err
		}
	}
	if r == nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	if now.IsZero() {
		now = nowUTC()
	} else {
		now = now.UTC()
	}
	parent, err := parent.Normalize(now)
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, err
	}
	confirmationID = strings.TrimSpace(confirmationID)
	if confirmationID == "" || confirmationID != parent.ConfirmationID {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindMultiConfirmationConflict
	}
	childrenByDestination, err := validateRuntimeConfigDirectoryRebindMultiCommitChildren(parent, children, now)
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rebindMultiApplies == nil {
		r.rebindMultiApplies = make(map[string]RuntimeConfigDirectoryRebindMultiApply)
	}
	if r.rebindApplies == nil {
		r.rebindApplies = make(map[string]RuntimeConfigDirectoryRebindApply)
	}
	if existing, ok := r.rebindMultiApplies[parent.ID]; ok {
		if !existing.MatchesIdentity(parent) {
			return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindMultiApplyConflict
		}
		for _, childID := range parent.ChildApplyIDs {
			stored, childOK := r.rebindApplies[childID]
			wanted := childrenByDestination[stored.Destination]
			if !childOK || !stored.MatchesIdentity(wanted) {
				return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindMultiApplyConflict
			}
		}
		return cloneRuntimeConfigDirectoryRebindMultiApply(existing), nil
	}
	confirmation, ok := r.rebindMultiConfirmations[confirmationID]
	if !ok {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrNotFound
	}
	if confirmation.Status == RuntimeConfigDirectoryRebindMultiConfirmationConfirmed && !confirmation.ExpiresAt.After(now) {
		confirmation.Status = RuntimeConfigDirectoryRebindMultiConfirmationExpired
		confirmation.Revision++
		r.rebindMultiConfirmations[confirmation.ID] = cloneRuntimeConfigDirectoryRebindMultiConfirmation(confirmation)
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindMultiConfirmationExpired
	}
	if confirmation.Status != RuntimeConfigDirectoryRebindMultiConfirmationConfirmed {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindMultiConfirmationConflict
	}
	confirmationDigest, err := RuntimeConfigDirectoryRebindMultiConfirmationDigest(confirmation)
	if err != nil || confirmationDigest != parent.ConfirmationDigest {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindMultiConfirmationConflict
	}
	if confirmation.PlanID != parent.PlanID || confirmation.InvocationID != parent.InvocationID || confirmation.Source != parent.Source || confirmation.ConfigSnapshotDigest != parent.ConfigSnapshotDigest || confirmation.CapabilitiesDigest != parent.CapabilitiesDigest || !equalRuntimeConfigDirectoryStrings(confirmation.Destinations, parent.Destinations) {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindMultiConfirmationConflict
	}
	for destination, child := range childrenByDestination {
		if existing, exists := r.rebindApplies[child.ID]; exists {
			if !existing.MatchesIdentity(child) {
				return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindMultiApplyConflict
			}
			return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindMultiApplyConflict
		}
		for _, existing := range r.rebindApplies {
			if existing.PlanID == parent.PlanID && existing.Destination == destination && existing.SnapshotRevision == parent.SnapshotRevision && existing.EventSequence == parent.EventSequence && existing.SnapshotDigest == parent.SnapshotDigest && existing.Status != RuntimeConfigDirectoryRebindApplyFailed {
				return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindMultiApplyConflict
			}
		}
	}
	consumedAt := now
	confirmation.Status = RuntimeConfigDirectoryRebindMultiConfirmationConsumed
	confirmation.ConsumedAt = &consumedAt
	confirmation.Revision++
	r.rebindMultiConfirmations[confirmation.ID] = cloneRuntimeConfigDirectoryRebindMultiConfirmation(confirmation)
	r.rebindMultiApplies[parent.ID] = cloneRuntimeConfigDirectoryRebindMultiApply(parent)
	for _, child := range childrenByDestination {
		r.rebindApplies[child.ID] = cloneRuntimeConfigDirectoryRebindApply(child)
	}
	return cloneRuntimeConfigDirectoryRebindMultiApply(parent), nil
}

func (r *MemoryRepository) GetRuntimeConfigDirectoryRebindMultiApply(ctx context.Context, id string) (RuntimeConfigDirectoryRebindMultiApply, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return RuntimeConfigDirectoryRebindMultiApply{}, err
		}
	}
	if r == nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.rebindMultiApplies[strings.TrimSpace(id)]
	if !ok {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrNotFound
	}
	return cloneRuntimeConfigDirectoryRebindMultiApply(item), nil
}

func (r *MemoryRepository) ListRuntimeConfigDirectoryRebindMultiApplies(ctx context.Context, planID, invocationID string, status RuntimeConfigDirectoryRebindMultiApplyStatus, limit int) ([]RuntimeConfigDirectoryRebindMultiApply, error) {
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
	if status != "" && !status.valid() {
		return nil, ErrInvalidRuntimeConfigDirectoryRebindMultiApply
	}
	planID = strings.TrimSpace(planID)
	invocationID = strings.TrimSpace(invocationID)
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]RuntimeConfigDirectoryRebindMultiApply, 0, limit)
	for _, item := range r.rebindMultiApplies {
		if planID != "" && item.PlanID != planID {
			continue
		}
		if invocationID != "" && item.InvocationID != invocationID {
			continue
		}
		if status != "" && item.Status != status {
			continue
		}
		items = append(items, cloneRuntimeConfigDirectoryRebindMultiApply(item))
	}
	runtimeConfigDirectoryRebindMultiApplySort(items)
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (r *MemoryRepository) ReconcileRuntimeConfigDirectoryRebindMultiApply(ctx context.Context, id string, now time.Time) (RuntimeConfigDirectoryRebindMultiApply, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return RuntimeConfigDirectoryRebindMultiApply{}, err
		}
	}
	if r == nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	if now.IsZero() {
		now = nowUTC()
	} else {
		now = now.UTC()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	parent, ok := r.rebindMultiApplies[strings.TrimSpace(id)]
	if !ok {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrNotFound
	}
	if parent.Status.terminal() {
		// A terminal parent is immutable except for repairing a missing child;
		// still validate the child set before returning a success projection.
	}
	pending, processing, completed, failed := 0, 0, 0, 0
	lastError := ""
	invalid := false
	for index, childID := range parent.ChildApplyIDs {
		child, childOK := r.rebindApplies[childID]
		if !childOK || child.ParentID != parent.ID || child.Destination != parent.Destinations[index] || child.PlanID != parent.PlanID || child.InvocationID != parent.InvocationID || child.SnapshotRevision != parent.SnapshotRevision || child.EventSequence != parent.EventSequence || child.SnapshotDigest != parent.SnapshotDigest {
			failed++
			invalid = true
			if lastError == "" {
				lastError = "multi rebind child 缺失或 identity 漂移"
			}
			continue
		}
		switch child.Status {
		case RuntimeConfigDirectoryRebindApplyQueued:
			pending++
		case RuntimeConfigDirectoryRebindApplyProcessing:
			processing++
		case RuntimeConfigDirectoryRebindApplyCompleted:
			completed++
		case RuntimeConfigDirectoryRebindApplyFailed:
			failed++
			if lastError == "" {
				lastError = child.LastError
			}
		default:
			failed++
			invalid = true
			if lastError == "" {
				lastError = "multi rebind child status 无效"
			}
		}
	}
	status := runtimeConfigDirectoryRebindMultiApplyDerivedStatus(pending, processing, completed, failed)
	candidate := parent
	candidate.PendingCount, candidate.ProcessingCount, candidate.CompletedCount, candidate.FailedCount = pending, processing, completed, failed
	candidate.Status = status
	if lastError != "" {
		candidate.LastError = lastError
	}
	changed := candidate.Status != parent.Status || candidate.PendingCount != parent.PendingCount || candidate.ProcessingCount != parent.ProcessingCount || candidate.CompletedCount != parent.CompletedCount || candidate.FailedCount != parent.FailedCount || candidate.LastError != parent.LastError
	if changed {
		candidate.Revision++
		candidate.UpdatedAt = now
		if status == RuntimeConfigDirectoryRebindMultiApplyCompleted {
			finished := now
			candidate.CompletedAt = &finished
		} else {
			candidate.CompletedAt = nil
		}
		candidate, err := candidate.Normalize(now)
		if err != nil {
			return RuntimeConfigDirectoryRebindMultiApply{}, err
		}
		r.rebindMultiApplies[candidate.ID] = cloneRuntimeConfigDirectoryRebindMultiApply(candidate)
		parent = candidate
	}
	if invalid {
		return cloneRuntimeConfigDirectoryRebindMultiApply(parent), ErrRuntimeConfigDirectoryRebindMultiApplyConflict
	}
	return cloneRuntimeConfigDirectoryRebindMultiApply(parent), nil
}

func (r *MemoryRepository) ReleaseRuntimeConfigDirectoryRebindApply(ctx context.Context, id, owner string, now time.Time) (bool, error) {
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
	item.Status = RuntimeConfigDirectoryRebindApplyQueued
	if item.Attempt > 0 {
		item.Attempt--
	}
	item.Revision++
	item.AvailableAt = now
	item.UpdatedAt = now
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	r.rebindApplies[item.ID] = cloneRuntimeConfigDirectoryRebindApply(item)
	return true, nil
}

var _ RuntimeConfigDirectoryRebindMultiConfirmationRepository = (*MemoryRepository)(nil)
var _ RuntimeConfigDirectoryRebindMultiApplyCommitRepository = (*MemoryRepository)(nil)
var _ RuntimeConfigDirectoryRebindMultiApplyRepository = (*MemoryRepository)(nil)
var _ RuntimeConfigDirectoryRebindApplyReleaseRepository = (*MemoryRepository)(nil)
