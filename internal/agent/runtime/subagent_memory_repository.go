package runtime

import (
	"context"
	"sort"
	"strings"
	"time"
)

func newMemorySubAgentRepository() *MemoryRepository {
	return newMemoryRepository()
}

// CreateSubAgentGroup 在同一把锁下写入 group 和所有子节点，避免 API 读到
// 只有 group 没有 runs 的半成品编排记录。
func (r *MemoryRepository) CreateSubAgentGroup(_ context.Context, group SubAgentGroup, runs []SubAgentRun) error {
	if r == nil {
		return ErrConflict
	}
	now := nowUTC()
	normalizedGroup, err := normalizeSubAgentGroup(group, now)
	if err != nil {
		return err
	}
	if len(runs) != normalizedGroup.ExpectedCount {
		return ErrConflict
	}
	normalizedRuns := make([]SubAgentRun, 0, len(runs))
	seenOrdinals := make(map[int]struct{}, len(runs))
	seenIDs := make(map[string]struct{}, len(runs))
	for _, run := range runs {
		normalized, normalizeErr := normalizeSubAgentRun(run, now)
		if normalizeErr != nil {
			return normalizeErr
		}
		if normalized.GroupID != normalizedGroup.ID || normalized.InvocationID != normalizedGroup.InvocationID {
			return ErrConflict
		}
		if _, ok := seenOrdinals[normalized.Ordinal]; ok {
			return ErrConflict
		}
		if _, ok := seenIDs[normalized.ID]; ok {
			return ErrConflict
		}
		seenOrdinals[normalized.Ordinal] = struct{}{}
		seenIDs[normalized.ID] = struct{}{}
		normalizedRuns = append(normalizedRuns, normalized)
	}
	if err := ValidateSubAgentRunDependencies(normalizedRuns); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.subAgentGroups[normalizedGroup.ID]; exists {
		return ErrConflict
	}
	for _, run := range normalizedRuns {
		if _, exists := r.subAgentRuns[run.ID]; exists {
			return ErrConflict
		}
	}
	r.subAgentGroups[normalizedGroup.ID] = cloneSubAgentGroup(normalizedGroup)
	for _, run := range normalizedRuns {
		r.subAgentRuns[run.ID] = cloneSubAgentRun(run)
	}
	return nil
}

func (r *MemoryRepository) GetSubAgentGroup(_ context.Context, id string) (SubAgentGroup, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	group, ok := r.subAgentGroups[strings.TrimSpace(id)]
	if !ok {
		return SubAgentGroup{}, ErrNotFound
	}
	return cloneSubAgentGroup(group), nil
}

func (r *MemoryRepository) ListSubAgentGroups(_ context.Context, invocationID string, status SubAgentGroupStatus, limit int) ([]SubAgentGroup, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit <= 0 || limit > MaxSubAgentListLimit {
		limit = MaxSubAgentListLimit
	}
	invocationID = strings.TrimSpace(invocationID)
	items := make([]SubAgentGroup, 0, limit)
	for _, group := range r.subAgentGroups {
		if invocationID != "" && group.InvocationID != invocationID {
			continue
		}
		if status != "" && group.Status != status {
			continue
		}
		items = append(items, cloneSubAgentGroup(group))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (r *MemoryRepository) UpdateSubAgentGroup(_ context.Context, group SubAgentGroup) error {
	now := nowUTC()
	normalized, err := normalizeSubAgentGroup(group, now)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.subAgentGroups[normalized.ID]
	if !ok {
		return ErrNotFound
	}
	normalized.CreatedAt = current.CreatedAt
	if normalized.StartedAt.IsZero() {
		normalized.StartedAt = current.StartedAt
	}
	if normalized.FinishedAt.IsZero() && current.FinishedAt.After(time.Time{}) {
		normalized.FinishedAt = current.FinishedAt
	}
	normalized.UpdatedAt = now
	r.subAgentGroups[normalized.ID] = cloneSubAgentGroup(normalized)
	return nil
}

func (r *MemoryRepository) GetSubAgentRun(_ context.Context, id string) (SubAgentRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.subAgentRuns[strings.TrimSpace(id)]
	if !ok {
		return SubAgentRun{}, ErrNotFound
	}
	return cloneSubAgentRun(run), nil
}

func (r *MemoryRepository) ListSubAgentRuns(_ context.Context, groupID string, status SubAgentRunStatus, limit int) ([]SubAgentRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	groupID = strings.TrimSpace(groupID)
	if groupID != "" {
		if _, ok := r.subAgentGroups[groupID]; !ok {
			return nil, ErrNotFound
		}
	}
	if limit <= 0 || limit > MaxSubAgentListLimit {
		limit = MaxSubAgentListLimit
	}
	items := make([]SubAgentRun, 0, limit)
	for _, run := range r.subAgentRuns {
		if groupID != "" && run.GroupID != groupID {
			continue
		}
		if status != "" && run.Status != status {
			continue
		}
		items = append(items, cloneSubAgentRun(run))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Ordinal == items[j].Ordinal {
			return items[i].ID < items[j].ID
		}
		return items[i].Ordinal < items[j].Ordinal
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (r *MemoryRepository) ClaimSubAgentRun(_ context.Context, id, owner string, now time.Time, ttl time.Duration) (SubAgentRun, bool, error) {
	id = strings.TrimSpace(id)
	owner = strings.TrimSpace(owner)
	if id == "" || owner == "" || ttl <= 0 || ttl > maxSubAgentLease {
		return SubAgentRun{}, false, ErrConflict
	}
	if now.IsZero() {
		now = nowUTC()
	} else {
		now = now.UTC()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.subAgentRuns[id]
	if !ok {
		return SubAgentRun{}, false, ErrNotFound
	}
	if run.Status.terminal() {
		return cloneSubAgentRun(run), false, nil
	}
	if run.Status == SubAgentRunRunning && run.LeaseExpiresAt.After(now) {
		return cloneSubAgentRun(run), false, nil
	}
	run.Status = SubAgentRunRunning
	run.Attempt++
	run.LeaseOwner = owner
	run.LeaseExpiresAt = now.Add(ttl)
	run.StartedAt = now
	run.UpdatedAt = now
	r.subAgentRuns[id] = cloneSubAgentRun(run)
	return cloneSubAgentRun(run), true, nil
}

// RenewSubAgentRunLease 延长执行租约。租约只是防止进程恢复时重复 claim，
// 不会给通用子 Agent 增加总执行时限。
func (r *MemoryRepository) RenewSubAgentRunLease(_ context.Context, id, owner string, now time.Time, ttl time.Duration) (bool, error) {
	if r == nil || strings.TrimSpace(id) == "" || strings.TrimSpace(owner) == "" || ttl <= 0 || ttl > maxSubAgentLease {
		return false, ErrConflict
	}
	if now.IsZero() {
		now = nowUTC()
	} else {
		now = now.UTC()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.subAgentRuns[strings.TrimSpace(id)]
	if !ok {
		return false, ErrNotFound
	}
	if run.Status != SubAgentRunRunning || run.LeaseOwner != strings.TrimSpace(owner) || run.LeaseExpiresAt.Before(now) {
		return false, nil
	}
	run.LeaseExpiresAt = now.Add(ttl)
	run.UpdatedAt = now
	r.subAgentRuns[run.ID] = cloneSubAgentRun(run)
	return true, nil
}

func (r *MemoryRepository) CompleteSubAgentRun(_ context.Context, id, owner string, status SubAgentRunStatus, resultText, errorCode, message string, now time.Time) (bool, error) {
	if !status.terminal() {
		return false, ErrConflict
	}
	if now.IsZero() {
		now = nowUTC()
	} else {
		now = now.UTC()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.subAgentRuns[strings.TrimSpace(id)]
	if !ok {
		return false, ErrNotFound
	}
	if run.Status != SubAgentRunRunning || run.LeaseOwner != strings.TrimSpace(owner) || run.LeaseExpiresAt.Before(now) {
		return false, nil
	}
	if len(resultText) > maxSubAgentOutputBytes {
		resultText = LimitSubAgentText(resultText, maxSubAgentOutputBytes)
	}
	run.Status = status
	run.ResultText = resultText
	run.ResultDigest = digestSubAgentText(resultText)
	run.ErrorCode = strings.TrimSpace(errorCode)
	run.Error = boundedSubAgentErrorString(message)
	run.LeaseOwner = ""
	run.LeaseExpiresAt = time.Time{}
	run.FinishedAt = now
	run.UpdatedAt = now
	r.subAgentRuns[run.ID] = cloneSubAgentRun(run)
	return true, nil
}

func (r *MemoryRepository) TransitionSubAgentRun(_ context.Context, id string, from, to SubAgentRunStatus, message string, now time.Time) (bool, error) {
	if !validSubAgentRunTransition(from, to) {
		return false, ErrConflict
	}
	if now.IsZero() {
		now = nowUTC()
	} else {
		now = now.UTC()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.subAgentRuns[strings.TrimSpace(id)]
	if !ok {
		return false, ErrNotFound
	}
	if run.Status != from {
		return false, nil
	}
	run.Status = to
	run.Error = boundedSubAgentErrorString(message)
	run.UpdatedAt = now
	if to.terminal() {
		run.FinishedAt = now
		run.LeaseOwner = ""
		run.LeaseExpiresAt = time.Time{}
	}
	r.subAgentRuns[run.ID] = cloneSubAgentRun(run)
	return true, nil
}

func validSubAgentRunTransition(from, to SubAgentRunStatus) bool {
	if from == to {
		return false
	}
	switch from {
	case SubAgentRunQueued:
		return to == SubAgentRunRunning || to == SubAgentRunFailed || to == SubAgentRunCancelled || to == SubAgentRunExpired
	case SubAgentRunRunning:
		return to == SubAgentRunCompleted || to == SubAgentRunFailed || to == SubAgentRunCancelled || to == SubAgentRunExpired
	default:
		return false
	}
}

func boundedSubAgentErrorString(message string) string {
	message = strings.TrimSpace(message)
	return LimitSubAgentText(message, maxSubAgentErrorBytes)
}

func (r *MemoryRepository) SaveSubAgentEvidence(_ context.Context, groupID string, items []EvidenceItem) error {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return ErrConflict
	}
	now := nowUTC()
	normalized := make([]EvidenceItem, 0, len(items))
	for _, item := range items {
		item, err := normalizeEvidence(item, now)
		if err != nil {
			return err
		}
		normalized = append(normalized, cloneSubAgentEvidence(item))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.subAgentGroups[groupID]; !ok {
		return ErrNotFound
	}
	r.subAgentEvidence[groupID] = normalized
	return nil
}

func (r *MemoryRepository) ListSubAgentEvidence(_ context.Context, groupID string, limit int, after string) ([]EvidenceItem, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	groupID = strings.TrimSpace(groupID)
	if _, ok := r.subAgentGroups[groupID]; !ok {
		return nil, ErrNotFound
	}
	if limit <= 0 || limit > maxRetrievalTopK {
		limit = maxRetrievalTopK
	}
	items := r.subAgentEvidence[groupID]
	result := make([]EvidenceItem, 0, len(items))
	for _, item := range items {
		if after != "" && item.EvidenceID <= after {
			continue
		}
		result = append(result, cloneSubAgentEvidence(item))
		if len(result) >= limit {
			break
		}
	}
	return result, nil
}

func (r *MemoryRepository) PruneSubAgentRecords(_ context.Context, before time.Time, limit int) (int, error) {
	if r == nil {
		return 0, ErrConflict
	}
	if before.IsZero() {
		before = nowUTC()
	}
	if limit <= 0 {
		limit = maxSubAgentGroups * maxSubAgentRuns
	}
	if limit > MaxSubAgentListLimit {
		limit = MaxSubAgentListLimit
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, limit)
	for id, group := range r.subAgentGroups {
		if !group.Status.terminal() || group.FinishedAt.IsZero() || !group.FinishedAt.Before(before) {
			continue
		}
		ids = append(ids, id)
		if len(ids) >= limit {
			break
		}
	}
	for _, groupID := range ids {
		delete(r.subAgentGroups, groupID)
		delete(r.subAgentEvidence, groupID)
		for runID, run := range r.subAgentRuns {
			if run.GroupID == groupID {
				delete(r.subAgentRuns, runID)
			}
		}
	}
	return len(ids), nil
}
