package runtime

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"Abot/internal/agent"
	"Abot/internal/provider"
)

// MemoryRepository is useful for deterministic coordinator tests and for small
// embedders that do not need persistence. Production uses SQLiteRepository.
type MemoryRepository struct {
	mu                         sync.Mutex
	invocations                map[string]Invocation
	events                     map[string][]AgentEvent
	approvals                  map[string]Approval
	toolCalls                  map[string]ToolCall
	plans                      map[string]TaskPlan
	contracts                  map[string][]TaskContract
	instructions               map[string]InstructionSnapshotSet
	manifests                  map[string][]ContextManifest
	workingSets                map[string]WorkingSet
	verifications              map[string][]VerificationRun
	snapshots                  map[string]RuntimeSnapshot
	toolSets                   map[string]ToolSetSnapshot
	capabilities               map[string]ModelCapabilitySnapshot
	evalRuns                   map[string]EvalRun
	baselines                  map[string]WorktreeBaseline
	resumes                    map[string]InvocationResume
	resumeOutbox               map[string][]InvocationResumeOutbox
	eventOutbox                map[string]RuntimeEventOutbox
	eventInbox                 map[string]RuntimeEventDeliveryInboxRecord
	eventTxns                  map[string]RuntimeEventDeliveryTransaction
	checkpointInbox            map[string]RuntimeCheckpointDeliveryInboxRecord
	checkpointOutbox           map[string]RuntimeCheckpointDeliveryOutbox
	checkpointTxns             map[string]RuntimeCheckpointDeliveryTransaction
	rejectionInbox             map[string]RuntimeApprovalRejectionDeliveryInboxRecord
	rejectionByApproval        map[string]string
	rejectionInboxByApproval   map[string]string
	rejectionOutbox            map[string]RuntimeApprovalRejectionDeliveryOutbox
	rejectionTxns              map[string]RuntimeApprovalRejectionDeliveryTransaction
	configDeliveryInbox        map[string]RuntimeConfigDeliveryInboxRecord
	configDeliveryOutbox       map[string]RuntimeConfigDeliveryOutbox
	configDeliveryTxns         map[string]RuntimeConfigDeliveryTransaction
	configDirectory            map[string]RuntimeConfigDirectoryRecord
	configDirectoryOutbox      map[string]RuntimeConfigDirectoryOutbox
	configDirectoryFanouts     map[string]RuntimeConfigDirectoryFanoutPlan
	configDirectoryRebindPlans map[string]RuntimeConfigDirectoryRebindPlan
	rebindConfirmations        map[string]RuntimeConfigDirectoryRebindConfirmation
	rebindMultiConfirmations   map[string]RuntimeConfigDirectoryRebindMultiConfirmation
	rebindApplies              map[string]RuntimeConfigDirectoryRebindApply
	rebindMultiApplies         map[string]RuntimeConfigDirectoryRebindMultiApply
	rebindApplyInbox           map[string]RuntimeConfigDirectoryRebindInboxRecord
	deliveryAttempts           map[string]RuntimeDeliveryAttempt
	deliveryGroups             map[string]RuntimeDeliveryGroup
	deliveryGroupTxns          map[string]RuntimeDeliveryGroupTransaction
	deliveryGroupFences        map[string]RuntimeDeliveryGroupFence
	deliveryGroupSettlements   map[string]RuntimeDeliveryGroupSettlement
	deliveryGroupSagas         map[string]RuntimeDeliveryGroupSaga
	deliveryCompensations      map[string]RuntimeDeliveryCompensation
	subAgentGroups             map[string]SubAgentGroup
	subAgentRuns               map[string]SubAgentRun
	subAgentEvidence           map[string][]EvidenceItem
}

func newMemoryRepository() *MemoryRepository {
	return &MemoryRepository{invocations: make(map[string]Invocation), events: make(map[string][]AgentEvent), approvals: make(map[string]Approval), toolCalls: make(map[string]ToolCall), plans: make(map[string]TaskPlan), contracts: make(map[string][]TaskContract), instructions: make(map[string]InstructionSnapshotSet), manifests: make(map[string][]ContextManifest), workingSets: make(map[string]WorkingSet), verifications: make(map[string][]VerificationRun), snapshots: make(map[string]RuntimeSnapshot), toolSets: make(map[string]ToolSetSnapshot), capabilities: make(map[string]ModelCapabilitySnapshot), evalRuns: make(map[string]EvalRun), baselines: make(map[string]WorktreeBaseline), resumes: make(map[string]InvocationResume), resumeOutbox: make(map[string][]InvocationResumeOutbox), eventOutbox: make(map[string]RuntimeEventOutbox), eventInbox: make(map[string]RuntimeEventDeliveryInboxRecord), eventTxns: make(map[string]RuntimeEventDeliveryTransaction), checkpointInbox: make(map[string]RuntimeCheckpointDeliveryInboxRecord), checkpointOutbox: make(map[string]RuntimeCheckpointDeliveryOutbox), checkpointTxns: make(map[string]RuntimeCheckpointDeliveryTransaction), rejectionInbox: make(map[string]RuntimeApprovalRejectionDeliveryInboxRecord), rejectionByApproval: make(map[string]string), rejectionInboxByApproval: make(map[string]string), rejectionOutbox: make(map[string]RuntimeApprovalRejectionDeliveryOutbox), rejectionTxns: make(map[string]RuntimeApprovalRejectionDeliveryTransaction), configDeliveryInbox: make(map[string]RuntimeConfigDeliveryInboxRecord), configDirectoryOutbox: make(map[string]RuntimeConfigDirectoryOutbox), configDeliveryTxns: make(map[string]RuntimeConfigDeliveryTransaction), configDirectory: make(map[string]RuntimeConfigDirectoryRecord), configDirectoryFanouts: make(map[string]RuntimeConfigDirectoryFanoutPlan), configDirectoryRebindPlans: make(map[string]RuntimeConfigDirectoryRebindPlan), rebindConfirmations: make(map[string]RuntimeConfigDirectoryRebindConfirmation), rebindMultiConfirmations: make(map[string]RuntimeConfigDirectoryRebindMultiConfirmation), rebindApplies: make(map[string]RuntimeConfigDirectoryRebindApply), rebindMultiApplies: make(map[string]RuntimeConfigDirectoryRebindMultiApply), rebindApplyInbox: make(map[string]RuntimeConfigDirectoryRebindInboxRecord), deliveryAttempts: make(map[string]RuntimeDeliveryAttempt), deliveryGroups: make(map[string]RuntimeDeliveryGroup), deliveryGroupTxns: make(map[string]RuntimeDeliveryGroupTransaction), deliveryGroupFences: make(map[string]RuntimeDeliveryGroupFence), deliveryGroupSettlements: make(map[string]RuntimeDeliveryGroupSettlement), deliveryGroupSagas: make(map[string]RuntimeDeliveryGroupSaga), deliveryCompensations: make(map[string]RuntimeDeliveryCompensation), subAgentGroups: make(map[string]SubAgentGroup), subAgentRuns: make(map[string]SubAgentRun), subAgentEvidence: make(map[string][]EvidenceItem)}
}

func NewMemoryRepository() *MemoryRepository {
	repository := newMemoryRepository()
	if repository.configDeliveryOutbox == nil {
		repository.configDeliveryOutbox = make(map[string]RuntimeConfigDeliveryOutbox)
	}
	return repository
}

func cloneInvocationResumeOutbox(item InvocationResumeOutbox) InvocationResumeOutbox {
	item.ResponseJSON = append([]byte(nil), item.ResponseJSON...)
	if item.LeaseExpiresAt != nil {
		value := *item.LeaseExpiresAt
		item.LeaseExpiresAt = &value
	}
	return item
}

func resumeOutboxBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

func cloneRuntimeEventOutbox(item RuntimeEventOutbox) RuntimeEventOutbox {
	if item.LeaseExpiresAt != nil {
		value := *item.LeaseExpiresAt
		item.LeaseExpiresAt = &value
	}
	return item
}

func cloneAgentEvent(item AgentEvent) AgentEvent {
	return SanitizeAgentEvent(item)
}

func (r *MemoryRepository) appendRuntimeEventOutboxLocked(event AgentEvent) error {
	if _, exists := r.eventOutbox[event.ID]; exists {
		return ErrConflict
	}
	item, err := NewRuntimeEventOutbox(event)
	if err != nil {
		return err
	}
	r.eventOutbox[item.ID] = cloneRuntimeEventOutbox(item)
	return nil
}

func (r *MemoryRepository) hasEventIDLocked(eventID string) bool {
	_, ok := r.eventByIDLocked(eventID)
	return ok
}

func (r *MemoryRepository) eventByIDLocked(eventID string) (AgentEvent, bool) {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return AgentEvent{}, false
	}
	for _, events := range r.events {
		for _, event := range events {
			if event.ID == eventID {
				return event, true
			}
		}
	}
	return AgentEvent{}, false
}

// EnqueueRuntimeEventOutbox is the compatibility entry point for consumers or
// migration tools. Normal event appends create this row under the same memory
// lock; this method therefore only accepts an already persisted event and is
// idempotent for the same EventID.
func (r *MemoryRepository) EnqueueRuntimeEventOutbox(_ context.Context, item RuntimeEventOutbox) (RuntimeEventOutbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	normalized, err := item.Normalize(nowUTC())
	if err != nil {
		return RuntimeEventOutbox{}, err
	}
	event, ok := r.eventByIDLocked(normalized.EventID)
	if !ok {
		return RuntimeEventOutbox{}, ErrNotFound
	}
	if existing, ok := r.eventOutbox[normalized.ID]; ok {
		if existing.EventID != normalized.EventID || existing.InvocationID != normalized.InvocationID || existing.Sequence != normalized.Sequence || existing.Type != normalized.Type {
			return RuntimeEventOutbox{}, ErrConflict
		}
		return cloneRuntimeEventOutbox(existing), nil
	}
	expected, err := NewRuntimeEventOutbox(event)
	if err != nil {
		return RuntimeEventOutbox{}, err
	}
	if normalized.ID != expected.ID {
		return RuntimeEventOutbox{}, ErrConflict
	}
	if normalized.Status != RuntimeEventOutboxQueued || normalized.Attempt != 0 {
		return RuntimeEventOutbox{}, fmt.Errorf("%w: 新 event outbox 必须从 queued/attempt=0 开始", ErrConflict)
	}
	for _, existing := range r.eventOutbox {
		if existing.EventID == normalized.EventID {
			return RuntimeEventOutbox{}, ErrConflict
		}
	}
	r.eventOutbox[normalized.ID] = cloneRuntimeEventOutbox(normalized)
	return cloneRuntimeEventOutbox(normalized), nil
}

func (r *MemoryRepository) GetRuntimeEventOutbox(_ context.Context, id string) (RuntimeEventOutbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.eventOutbox[strings.TrimSpace(id)]
	if !ok {
		return RuntimeEventOutbox{}, ErrNotFound
	}
	return cloneRuntimeEventOutbox(item), nil
}

func (r *MemoryRepository) ListRuntimeEventOutbox(_ context.Context, invocationID string, status RuntimeEventOutboxStatus, limit int) ([]RuntimeEventOutbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	invocationID = strings.TrimSpace(invocationID)
	if invocationID != "" {
		if _, ok := r.invocations[invocationID]; !ok {
			return nil, ErrNotFound
		}
	}
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	items := make([]RuntimeEventOutbox, 0, limit)
	for _, item := range r.eventOutbox {
		if invocationID != "" && item.InvocationID != invocationID {
			continue
		}
		if status != "" && item.Status != status {
			continue
		}
		items = append(items, cloneRuntimeEventOutbox(item))
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

// RuntimeEventOutboxReadyThrough reports whether every event cursor up to the
// supplied high-water mark has reached the durable completed state. Failed and
// processing cursors deliberately keep the barrier closed: publishing a
// checkpoint that claims those events would make the destination projection
// appear newer than its event stream.
func (r *MemoryRepository) RuntimeEventOutboxReadyThrough(_ context.Context, invocationID string, sequence int64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	invocationID = strings.TrimSpace(invocationID)
	if invocationID == "" || sequence <= 0 {
		return false, ErrConflict
	}
	if _, ok := r.invocations[invocationID]; !ok {
		return false, ErrNotFound
	}
	seen := int64(0)
	for _, item := range r.eventOutbox {
		if item.InvocationID != invocationID || item.Sequence > sequence {
			continue
		}
		seen++
		if item.Status != RuntimeEventOutboxCompleted {
			return false, nil
		}
	}
	// Event sequences are contiguous per invocation. A missing cursor is
	// treated as a blocker rather than allowing a checkpoint to leap over an
	// event that the destination can no longer observe.
	return seen == sequence, nil
}

func (r *MemoryRepository) ClaimRuntimeEventOutbox(_ context.Context, owner string, now time.Time, ttl time.Duration) (RuntimeEventOutbox, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeEventOutboxOwnerLength || ttl <= 0 || ttl > MaxRuntimeEventOutboxLease {
		return RuntimeEventOutbox{}, false, ErrConflict
	}
	if now.IsZero() {
		now = nowUTC()
	} else {
		now = now.UTC()
	}
	items := make([]RuntimeEventOutbox, 0, len(r.eventOutbox))
	for _, item := range r.eventOutbox {
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
		item, ok := r.eventOutbox[candidate.ID]
		if !ok || item.Status == RuntimeEventOutboxCompleted || item.Status == RuntimeEventOutboxFailed {
			continue
		}
		if item.Status == RuntimeEventOutboxQueued && item.AvailableAt.After(now) {
			continue
		}
		if item.Status == RuntimeEventOutboxProcessing && item.LeaseExpiresAt != nil && item.LeaseExpiresAt.After(now) {
			continue
		}
		if item.Attempt >= MaxRuntimeEventOutboxAttempts {
			item.Status = RuntimeEventOutboxFailed
			item.LastError = "event outbox 达到最大投递次数"
			item.LeaseOwner = ""
			item.LeaseExpiresAt = nil
			item.Revision++
			item.UpdatedAt = now
			r.eventOutbox[item.ID] = item
			continue
		}
		item.Attempt++
		item.Status = RuntimeEventOutboxProcessing
		item.LeaseOwner = owner
		expires := now.Add(ttl)
		item.LeaseExpiresAt = &expires
		item.Revision++
		item.UpdatedAt = now
		r.eventOutbox[item.ID] = item
		return cloneRuntimeEventOutbox(item), true, nil
	}
	return RuntimeEventOutbox{}, false, nil
}

func (r *MemoryRepository) CompleteRuntimeEventOutbox(_ context.Context, id, owner string, now time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id = strings.TrimSpace(id)
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeEventOutboxOwnerLength {
		return false, ErrConflict
	}
	item, ok := r.eventOutbox[id]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != RuntimeEventOutboxProcessing || item.LeaseOwner != owner {
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
	item.Status = RuntimeEventOutboxCompleted
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	item.Revision++
	item.UpdatedAt = now
	r.eventOutbox[id] = item
	return true, nil
}

func (r *MemoryRepository) RetryRuntimeEventOutbox(_ context.Context, id, owner string, now time.Time, message string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id = strings.TrimSpace(id)
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeEventOutboxOwnerLength {
		return false, ErrConflict
	}
	item, ok := r.eventOutbox[id]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != RuntimeEventOutboxProcessing || item.LeaseOwner != owner {
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
	item.LastError = sanitizeRuntimeString(strings.TrimSpace(message))
	if len(item.LastError) > 4096 {
		item.LastError = item.LastError[:4096]
	}
	if item.Attempt >= MaxRuntimeEventOutboxAttempts {
		item.Status = RuntimeEventOutboxFailed
	} else {
		item.Status = RuntimeEventOutboxQueued
		item.AvailableAt = now.Add(runtimeEventOutboxBackoff(item.Attempt))
	}
	item.Revision++
	item.UpdatedAt = now
	r.eventOutbox[id] = item
	return true, nil
}

// EnqueueRuntimeCheckpointDeliveryOutbox is the migration/repair entry point
// for a checkpoint cursor. Normal Snapshot commits insert the row while
// holding the same memory lock; this method verifies the cursor against the
// currently stored exact Snapshot before accepting it.
func (r *MemoryRepository) EnqueueRuntimeCheckpointDeliveryOutbox(_ context.Context, item RuntimeCheckpointDeliveryOutbox) (RuntimeCheckpointDeliveryOutbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	normalized, err := item.Normalize(nowUTC())
	if err != nil {
		return RuntimeCheckpointDeliveryOutbox{}, err
	}
	snapshot, ok := r.snapshots[normalized.InvocationID]
	if !ok {
		return RuntimeCheckpointDeliveryOutbox{}, ErrNotFound
	}
	if snapshot.Revision != normalized.SnapshotRevision {
		return RuntimeCheckpointDeliveryOutbox{}, ErrConflict
	}
	eventFound := false
	for _, event := range r.events[normalized.InvocationID] {
		if event.Sequence != normalized.EventSequence {
			continue
		}
		if event.InvocationID != normalized.InvocationID {
			return RuntimeCheckpointDeliveryOutbox{}, ErrConflict
		}
		eventFound = true
		break
	}
	if !eventFound {
		return RuntimeCheckpointDeliveryOutbox{}, ErrConflict
	}
	expected, err := NewRuntimeCheckpointDeliveryOutbox(normalized.Source, normalized.Destination, snapshot, normalized.EventSequence)
	if err != nil {
		return RuntimeCheckpointDeliveryOutbox{}, err
	}
	if normalized.ID != expected.ID || normalized.DeliveryID != expected.DeliveryID || normalized.SnapshotDigest != expected.SnapshotDigest || !normalized.ProjectionMatches(expected) {
		return RuntimeCheckpointDeliveryOutbox{}, ErrConflict
	}
	if existing, exists := r.checkpointOutbox[normalized.ID]; exists {
		if !existing.Matches(normalized) {
			return RuntimeCheckpointDeliveryOutbox{}, ErrConflict
		}
		return cloneRuntimeCheckpointDeliveryOutbox(existing), nil
	}
	for _, existing := range r.checkpointOutbox {
		if existing.InvocationID == normalized.InvocationID && existing.Source == normalized.Source && existing.Destination == normalized.Destination && existing.SnapshotRevision == normalized.SnapshotRevision && existing.EventSequence == normalized.EventSequence {
			return RuntimeCheckpointDeliveryOutbox{}, ErrConflict
		}
	}
	r.checkpointOutbox[normalized.ID] = cloneRuntimeCheckpointDeliveryOutbox(normalized)
	return cloneRuntimeCheckpointDeliveryOutbox(normalized), nil
}

func (r *MemoryRepository) GetRuntimeCheckpointDeliveryOutbox(_ context.Context, id string) (RuntimeCheckpointDeliveryOutbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.checkpointOutbox[strings.TrimSpace(id)]
	if !ok {
		return RuntimeCheckpointDeliveryOutbox{}, ErrNotFound
	}
	return cloneRuntimeCheckpointDeliveryOutbox(item), nil
}

func (r *MemoryRepository) ListRuntimeCheckpointDeliveryOutbox(_ context.Context, invocationID string, status RuntimeCheckpointDeliveryOutboxStatus, limit int) ([]RuntimeCheckpointDeliveryOutbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	invocationID = strings.TrimSpace(invocationID)
	if invocationID != "" {
		if _, ok := r.invocations[invocationID]; !ok {
			return nil, ErrNotFound
		}
	}
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	items := make([]RuntimeCheckpointDeliveryOutbox, 0, limit)
	for _, item := range r.checkpointOutbox {
		if invocationID != "" && item.InvocationID != invocationID {
			continue
		}
		if status != "" && item.Status != status {
			continue
		}
		items = append(items, cloneRuntimeCheckpointDeliveryOutbox(item))
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

func (r *MemoryRepository) ClaimRuntimeCheckpointDeliveryOutbox(_ context.Context, owner string, now time.Time, ttl time.Duration) (RuntimeCheckpointDeliveryOutbox, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeCheckpointDeliveryOutboxOwnerLength || ttl <= 0 || ttl > MaxRuntimeCheckpointDeliveryOutboxLease {
		return RuntimeCheckpointDeliveryOutbox{}, false, ErrConflict
	}
	if now.IsZero() {
		now = nowUTC()
	} else {
		now = now.UTC()
	}
	items := make([]RuntimeCheckpointDeliveryOutbox, 0, len(r.checkpointOutbox))
	for _, item := range r.checkpointOutbox {
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
		item, ok := r.checkpointOutbox[candidate.ID]
		if !ok || item.Status == RuntimeCheckpointDeliveryOutboxCompleted || item.Status == RuntimeCheckpointDeliveryOutboxFailed {
			continue
		}
		if item.Status == RuntimeCheckpointDeliveryOutboxQueued && item.AvailableAt.After(now) {
			continue
		}
		if item.Status == RuntimeCheckpointDeliveryOutboxProcessing {
			if item.LeaseOwner == "" || item.LeaseExpiresAt == nil || item.LeaseExpiresAt.IsZero() {
				item.Status = RuntimeCheckpointDeliveryOutboxFailed
				item.LastError = "checkpoint outbox processing 元数据无效"
				item.LeaseOwner = ""
				item.LeaseExpiresAt = nil
				item.Revision++
				item.UpdatedAt = now
				r.checkpointOutbox[item.ID] = item
				continue
			}
			if item.LeaseExpiresAt.After(now) {
				continue
			}
		}
		if item.Attempt >= MaxRuntimeCheckpointDeliveryOutboxAttempts {
			item.Status = RuntimeCheckpointDeliveryOutboxFailed
			item.LastError = "checkpoint outbox 达到最大投递次数"
			item.LeaseOwner = ""
			item.LeaseExpiresAt = nil
			item.Revision++
			item.UpdatedAt = now
			r.checkpointOutbox[item.ID] = item
			continue
		}
		item.Attempt++
		item.Status = RuntimeCheckpointDeliveryOutboxProcessing
		item.LeaseOwner = owner
		expires := now.Add(ttl)
		item.LeaseExpiresAt = &expires
		item.Revision++
		item.UpdatedAt = now
		r.checkpointOutbox[item.ID] = item
		return cloneRuntimeCheckpointDeliveryOutbox(item), true, nil
	}
	return RuntimeCheckpointDeliveryOutbox{}, false, nil
}

func (r *MemoryRepository) CompleteRuntimeCheckpointDeliveryOutbox(_ context.Context, id, owner string, now time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id = strings.TrimSpace(id)
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeCheckpointDeliveryOutboxOwnerLength {
		return false, ErrConflict
	}
	item, ok := r.checkpointOutbox[id]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != RuntimeCheckpointDeliveryOutboxProcessing || item.LeaseOwner != owner {
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
	item.Status = RuntimeCheckpointDeliveryOutboxCompleted
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	item.Revision++
	item.UpdatedAt = now
	r.checkpointOutbox[id] = item
	return true, nil
}

func (r *MemoryRepository) RetryRuntimeCheckpointDeliveryOutbox(_ context.Context, id, owner string, now time.Time, message string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id = strings.TrimSpace(id)
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeCheckpointDeliveryOutboxOwnerLength {
		return false, ErrConflict
	}
	item, ok := r.checkpointOutbox[id]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != RuntimeCheckpointDeliveryOutboxProcessing || item.LeaseOwner != owner {
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
	item.LastError = sanitizeRuntimeString(strings.TrimSpace(message))
	if len(item.LastError) > 4096 {
		item.LastError = item.LastError[:4096]
	}
	if item.Attempt >= MaxRuntimeCheckpointDeliveryOutboxAttempts {
		item.Status = RuntimeCheckpointDeliveryOutboxFailed
	} else {
		item.Status = RuntimeCheckpointDeliveryOutboxQueued
		item.AvailableAt = now.Add(runtimeCheckpointDeliveryOutboxBackoff(item.Attempt))
	}
	item.Revision++
	item.UpdatedAt = now
	r.checkpointOutbox[id] = item
	return true, nil
}

// DeferRuntimeCheckpointDeliveryOutbox releases a checkpoint lease without
// consuming one delivery attempt. It is used by the event high-water barrier
// while an earlier event cursor is still being delivered.
func (r *MemoryRepository) DeferRuntimeCheckpointDeliveryOutbox(_ context.Context, id, owner string, now, availableAt time.Time, message string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id = strings.TrimSpace(id)
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeCheckpointDeliveryOutboxOwnerLength {
		return false, ErrConflict
	}
	item, ok := r.checkpointOutbox[id]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != RuntimeCheckpointDeliveryOutboxProcessing || item.LeaseOwner != owner {
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
	if availableAt.IsZero() {
		availableAt = now
	} else {
		availableAt = availableAt.UTC()
	}
	if availableAt.Before(now) {
		availableAt = now
	}
	if availableAt.After(now.Add(MaxRuntimeCheckpointDeliveryOutboxLease)) {
		return false, ErrConflict
	}
	item.Status = RuntimeCheckpointDeliveryOutboxQueued
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	if item.Attempt > 0 {
		item.Attempt--
	}
	item.AvailableAt = availableAt
	item.LastError = sanitizeRuntimeString(strings.TrimSpace(message))
	if len(item.LastError) > 4096 {
		item.LastError = item.LastError[:4096]
	}
	item.Revision++
	item.UpdatedAt = now
	r.checkpointOutbox[id] = item
	return true, nil
}

// AcceptRuntimeEventDelivery stores only the signed envelope metadata and
// makes duplicate delivery idempotent. A reused EventID with different
// routing, sequence, digest or signature is a conflict, never an overwrite.
func (r *MemoryRepository) AcceptRuntimeEventDelivery(_ context.Context, envelope RuntimeEventDeliveryEnvelope) (bool, error) {
	normalized, err := envelope.normalize()
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.acceptRuntimeEventDeliveryLocked(normalized, nowUTC())
}

// GetRuntimeEventDelivery returns the bounded metadata row used by the
// authenticated destination status endpoint. The event body is never part of
// this read path.
func (r *MemoryRepository) GetRuntimeEventDelivery(_ context.Context, eventID string) (RuntimeEventDeliveryInboxRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.eventInbox[strings.TrimSpace(eventID)]
	if !ok {
		return RuntimeEventDeliveryInboxRecord{}, ErrNotFound
	}
	return item, nil
}

func (r *MemoryRepository) acceptRuntimeEventDeliveryLocked(normalized RuntimeEventDeliveryEnvelope, receivedAt time.Time) (bool, error) {
	if existing, ok := r.eventInbox[normalized.EventID]; ok {
		if !existing.matches(normalized) {
			return false, ErrConflict
		}
		return true, nil
	}
	if r.eventInbox == nil {
		r.eventInbox = make(map[string]RuntimeEventDeliveryInboxRecord)
	}
	r.eventInbox[normalized.EventID] = runtimeEventDeliveryInboxRecord(normalized, receivedAt)
	return false, nil
}

// AcceptRuntimeCheckpointDelivery stores the latest metadata-only checkpoint
// for one invocation. Replays of the exact immutable delivery are idempotent;
// older revisions and event watermarks cannot overwrite a newer projection.
func (r *MemoryRepository) AcceptRuntimeCheckpointDelivery(_ context.Context, envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection) (bool, error) {
	normalized, projection, err := normalizeRuntimeCheckpointDeliveryPair(envelope, projection)
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.acceptRuntimeCheckpointDeliveryLocked(normalized, projection, nowUTC())
}

func (r *MemoryRepository) GetRuntimeCheckpointDelivery(_ context.Context, invocationID string) (RuntimeCheckpointDeliveryInboxRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.checkpointInbox[strings.TrimSpace(invocationID)]
	if !ok {
		return RuntimeCheckpointDeliveryInboxRecord{}, ErrNotFound
	}
	return item.clone(), nil
}

// EnqueueInvocationResumeOutbox persists one private resume delivery. The
// request digest is the idempotency key, so retries cannot create another
// delivery for the same boundary and payload.
func (r *MemoryRepository) EnqueueInvocationResumeOutbox(_ context.Context, item InvocationResumeOutbox) (InvocationResumeOutbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	normalized, err := item.Normalize(nowUTC())
	if err != nil {
		return InvocationResumeOutbox{}, err
	}
	if _, ok := r.invocations[normalized.InvocationID]; !ok {
		return InvocationResumeOutbox{}, ErrNotFound
	}
	items := r.resumeOutbox[normalized.InvocationID]
	for _, existing := range items {
		if existing.RequestDigest != normalized.RequestDigest {
			continue
		}
		if existing.WaitID != normalized.WaitID || existing.Name != normalized.Name || string(existing.ResponseJSON) != string(normalized.ResponseJSON) {
			return InvocationResumeOutbox{}, ErrConflict
		}
		return cloneInvocationResumeOutbox(existing), nil
	}
	if normalized.ID == "" {
		normalized.ID = normalized.InvocationID + "-resume-outbox-" + normalized.RequestDigest
	}
	r.resumeOutbox[normalized.InvocationID] = append(items, cloneInvocationResumeOutbox(normalized))
	return cloneInvocationResumeOutbox(normalized), nil
}

// GetInvocationResumeOutbox returns the newest durable delivery record. It is
// used only for restart recovery and diagnostics; payload remains private.
func (r *MemoryRepository) GetInvocationResumeOutbox(_ context.Context, invocationID string) (InvocationResumeOutbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := r.resumeOutbox[strings.TrimSpace(invocationID)]
	if len(items) == 0 {
		return InvocationResumeOutbox{}, ErrNotFound
	}
	return cloneInvocationResumeOutbox(items[len(items)-1]), nil
}

// ClaimInvocationResumeOutbox grants a short lease to one eligible delivery.
// A processing record whose lease expired is eligible again; a live lease is
// never stolen.
func (r *MemoryRepository) ClaimInvocationResumeOutbox(_ context.Context, invocationID, owner string, now time.Time, ttl time.Duration) (InvocationResumeOutbox, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	invocationID = strings.TrimSpace(invocationID)
	owner = strings.TrimSpace(owner)
	if invocationID == "" || owner == "" || len(owner) > MaxInvocationResumeOutboxOwnerLength || ttl <= 0 || ttl > MaxInvocationResumeOutboxLease {
		return InvocationResumeOutbox{}, false, ErrConflict
	}
	if _, ok := r.invocations[invocationID]; !ok {
		return InvocationResumeOutbox{}, false, ErrNotFound
	}
	now = now.UTC()
	items := r.resumeOutbox[invocationID]
	for index := len(items) - 1; index >= 0; index-- {
		item := &items[index]
		if item.Status == InvocationResumeOutboxCompleted || item.Status == InvocationResumeOutboxFailed {
			continue
		}
		if item.Status == InvocationResumeOutboxProcessing {
			if item.LeaseOwner == "" || item.LeaseExpiresAt == nil {
				// A malformed processing row must never be replayed without a
				// verifiable lease. Terminally quarantine it instead.
				item.Status = InvocationResumeOutboxFailed
				item.LeaseOwner = ""
				item.LeaseExpiresAt = nil
				item.LastError = "resume outbox processing 元数据无效"
				item.Revision++
				item.UpdatedAt = now
				continue
			}
			if item.LeaseExpiresAt.After(now) {
				continue
			}
		}
		if item.Status == InvocationResumeOutboxQueued && item.AvailableAt.After(now) {
			continue
		}
		if item.Attempt >= MaxInvocationResumeOutboxAttempts {
			item.Status = InvocationResumeOutboxFailed
			item.LastError = "resume outbox 达到最大投递次数"
			item.Revision++
			item.UpdatedAt = now
			continue
		}
		item.Attempt++
		item.Status = InvocationResumeOutboxProcessing
		item.LeaseOwner = owner
		expires := now.Add(ttl)
		item.LeaseExpiresAt = &expires
		item.Revision++
		item.UpdatedAt = now
		return cloneInvocationResumeOutbox(*item), true, nil
	}
	return InvocationResumeOutbox{}, false, nil
}

// CompleteInvocationResumeOutbox acknowledges the latest processing delivery
// owned by owner. Completion is idempotent for a later retry that observes the
// terminal record.
func (r *MemoryRepository) CompleteInvocationResumeOutbox(_ context.Context, invocationID, owner string, now time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	invocationID = strings.TrimSpace(invocationID)
	owner = strings.TrimSpace(owner)
	if invocationID == "" || owner == "" || len(owner) > MaxInvocationResumeOutboxOwnerLength {
		return false, ErrConflict
	}
	items := r.resumeOutbox[invocationID]
	if len(items) == 0 {
		return false, nil
	}
	if now.IsZero() {
		now = nowUTC()
	} else {
		now = now.UTC()
	}
	for index := len(items) - 1; index >= 0; index-- {
		item := &items[index]
		if item.Status != InvocationResumeOutboxProcessing || item.LeaseOwner != owner {
			continue
		}
		if item.LeaseExpiresAt == nil || !item.LeaseExpiresAt.After(now) {
			return false, nil
		}
		item.Status = InvocationResumeOutboxCompleted
		item.LeaseOwner = ""
		item.LeaseExpiresAt = nil
		item.Revision++
		item.UpdatedAt = now
		return true, nil
	}
	return false, nil
}

// RetryInvocationResumeOutbox releases a processing lease and schedules a
// bounded retry. Once the attempt budget is exhausted the item is terminally
// failed and will never be replayed.
func (r *MemoryRepository) RetryInvocationResumeOutbox(_ context.Context, invocationID, owner string, now time.Time, message string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	invocationID = strings.TrimSpace(invocationID)
	owner = strings.TrimSpace(owner)
	if invocationID == "" || owner == "" || len(owner) > MaxInvocationResumeOutboxOwnerLength {
		return false, ErrConflict
	}
	items := r.resumeOutbox[invocationID]
	if len(items) == 0 {
		return false, nil
	}
	if now.IsZero() {
		now = nowUTC()
	} else {
		now = now.UTC()
	}
	message = strings.TrimSpace(message)
	if len(message) > 4096 {
		message = message[:4096]
	}
	for index := len(items) - 1; index >= 0; index-- {
		item := &items[index]
		if item.Status != InvocationResumeOutboxProcessing || item.LeaseOwner != owner {
			continue
		}
		if item.LeaseExpiresAt == nil || !item.LeaseExpiresAt.After(now) {
			return false, nil
		}
		item.LeaseOwner = ""
		item.LeaseExpiresAt = nil
		item.LastError = message
		if item.Attempt >= MaxInvocationResumeOutboxAttempts {
			item.Status = InvocationResumeOutboxFailed
		} else {
			item.Status = InvocationResumeOutboxQueued
			item.AvailableAt = now.Add(resumeOutboxBackoff(item.Attempt))
		}
		item.Revision++
		item.UpdatedAt = now
		return true, nil
	}
	return false, nil
}

func (r *MemoryRepository) DeleteInvocationResumeOutbox(_ context.Context, invocationID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	invocationID = strings.TrimSpace(invocationID)
	if _, ok := r.resumeOutbox[invocationID]; !ok {
		return ErrNotFound
	}
	delete(r.resumeOutbox, invocationID)
	return nil
}

func (r *MemoryRepository) SaveInvocationResume(_ context.Context, item InvocationResume) (InvocationResume, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item.InvocationID = strings.TrimSpace(item.InvocationID)
	item.WaitID = strings.TrimSpace(item.WaitID)
	item.Name = strings.TrimSpace(item.Name)
	item.RequestDigest = strings.TrimSpace(item.RequestDigest)
	if item.InvocationID == "" || item.WaitID == "" || item.Name == "" || item.RequestDigest == "" || len(item.ResponseJSON) > maxInvocationResumeResponseBytes {
		return InvocationResume{}, ErrInvalidResume
	}
	if _, ok := r.invocations[item.InvocationID]; !ok {
		return InvocationResume{}, ErrNotFound
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = nowUTC()
	}
	if existing, ok := r.resumes[item.InvocationID]; ok {
		if existing.WaitID != item.WaitID || existing.Name != item.Name || existing.RequestDigest != item.RequestDigest || string(existing.ResponseJSON) != string(item.ResponseJSON) {
			return InvocationResume{}, ErrConflict
		}
		return cloneInvocationResume(existing), nil
	}
	r.resumes[item.InvocationID] = cloneInvocationResume(item)
	return cloneInvocationResume(item), nil
}

// CommitInvocationResume keeps the accepted tool response, its durable
// delivery record, the Invocation CAS and the resumed event in one memory
// transaction. It mirrors the SQLite implementation and is intentionally
// optional at the Coordinator interface boundary for older embedders.
func (r *MemoryRepository) CommitInvocationResume(_ context.Context, commit InvocationResumeCommit) (Invocation, AgentEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	invocationID := strings.TrimSpace(commit.InvocationID)
	if invocationID == "" || strings.TrimSpace(commit.Resume.InvocationID) != invocationID || strings.TrimSpace(commit.Outbox.InvocationID) != invocationID {
		return Invocation{}, AgentEvent{}, ErrInvalidResume
	}
	if (commit.FromStatus != InvocationWaitingTool && commit.FromStatus != InvocationWaitingUser) || commit.ToStatus != InvocationQueued || !validInvocationTransition(commit.FromStatus, commit.ToStatus) {
		return Invocation{}, AgentEvent{}, ErrConflict
	}
	if strings.TrimSpace(commit.Resume.WaitID) == "" || strings.TrimSpace(commit.Resume.Name) == "" || strings.TrimSpace(commit.Resume.RequestDigest) == "" || len(commit.Resume.ResponseJSON) > maxInvocationResumeResponseBytes {
		return Invocation{}, AgentEvent{}, ErrInvalidResume
	}
	invocation, ok := r.invocations[invocationID]
	if !ok {
		return Invocation{}, AgentEvent{}, ErrNotFound
	}
	if invocation.Status != commit.FromStatus {
		return Invocation{}, AgentEvent{}, ErrConflict
	}
	if strings.TrimSpace(commit.Outbox.RequestDigest) != strings.TrimSpace(commit.Resume.RequestDigest) || strings.TrimSpace(commit.Outbox.WaitID) != strings.TrimSpace(commit.Resume.WaitID) || strings.TrimSpace(commit.Outbox.Name) != strings.TrimSpace(commit.Resume.Name) || string(commit.Outbox.ResponseJSON) != string(commit.Resume.ResponseJSON) {
		return Invocation{}, AgentEvent{}, ErrConflict
	}
	now := nowUTC()
	outbox, err := commit.Outbox.Normalize(now)
	if err != nil {
		return Invocation{}, AgentEvent{}, err
	}
	event := commit.Event
	event.InvocationID = invocationID
	if strings.TrimSpace(event.ID) == "" {
		event.ID = newID("event")
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = now
	}
	if event.Type == "" {
		event.Type = EventInvocationResumed
	}
	event.Type = strings.TrimSpace(event.Type)
	if event.Type == "" || r.hasEventIDLocked(event.ID) {
		return Invocation{}, AgentEvent{}, ErrConflict
	}
	event.Sequence = int64(len(r.events[invocationID]) + 1)
	eventOutbox, err := NewRuntimeEventOutbox(event)
	if err != nil {
		return Invocation{}, AgentEvent{}, err
	}
	if existing, exists := r.resumes[invocationID]; exists {
		if existing.WaitID != strings.TrimSpace(commit.Resume.WaitID) || existing.Name != strings.TrimSpace(commit.Resume.Name) || existing.RequestDigest != strings.TrimSpace(commit.Resume.RequestDigest) || string(existing.ResponseJSON) != string(commit.Resume.ResponseJSON) {
			return Invocation{}, AgentEvent{}, ErrConflict
		}
	} else {
		resume := commit.Resume
		resume.InvocationID = invocationID
		resume.WaitID = strings.TrimSpace(resume.WaitID)
		resume.Name = strings.TrimSpace(resume.Name)
		resume.RequestDigest = strings.TrimSpace(resume.RequestDigest)
		if resume.CreatedAt.IsZero() {
			resume.CreatedAt = now
		}
		r.resumes[invocationID] = cloneInvocationResume(resume)
	}
	items := r.resumeOutbox[invocationID]
	foundOutbox := false
	for _, existing := range items {
		if existing.RequestDigest != outbox.RequestDigest {
			continue
		}
		if existing.WaitID != outbox.WaitID || existing.Name != outbox.Name || string(existing.ResponseJSON) != string(outbox.ResponseJSON) {
			return Invocation{}, AgentEvent{}, ErrConflict
		}
		foundOutbox = true
		break
	}
	if !foundOutbox {
		if outbox.ID == "" {
			outbox.ID = invocationID + "-resume-outbox-" + outbox.RequestDigest
		}
		r.resumeOutbox[invocationID] = append(items, cloneInvocationResumeOutbox(outbox))
	}
	r.events[invocationID] = append(r.events[invocationID], event)
	r.eventOutbox[eventOutbox.ID] = cloneRuntimeEventOutbox(eventOutbox)
	invocation.Status = commit.ToStatus
	invocation.Error = commit.Message
	invocation.LeaseOwner = ""
	invocation.LeaseExpiresAt = nil
	invocation.UpdatedAt = now
	r.invocations[invocationID] = invocation
	return invocation, event, nil
}

// CommitApprovalResume atomically records an approved confirmation across the
// Runtime-owned entities. It is kept separate from CommitInvocationResume so
// the latter remains a small tool-boundary contract and older embedders can
// adopt either operation independently.
func (r *MemoryRepository) CommitApprovalResume(_ context.Context, commit ApprovalResumeCommit) (Approval, Invocation, []AgentEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	approvalID := strings.TrimSpace(commit.ApprovalID)
	invocationID := strings.TrimSpace(commit.InvocationID)
	if approvalID == "" || invocationID == "" || strings.TrimSpace(commit.Resume.InvocationID) != invocationID || strings.TrimSpace(commit.Outbox.InvocationID) != invocationID {
		return Approval{}, Invocation{}, nil, ErrInvalidResume
	}
	if commit.ApprovalFromStatus != ApprovalPending || (commit.ApprovalToStatus != ApprovalApproved && commit.ApprovalToStatus != ApprovalRejected) {
		return Approval{}, Invocation{}, nil, ErrConflict
	}
	if commit.InvocationFromStatus != InvocationWaitingApproval || commit.InvocationToStatus != InvocationQueued || !validInvocationTransition(commit.InvocationFromStatus, commit.InvocationToStatus) {
		return Approval{}, Invocation{}, nil, ErrConflict
	}
	resume := commit.Resume
	resume.InvocationID = invocationID
	resume.WaitID = strings.TrimSpace(resume.WaitID)
	resume.Name = strings.TrimSpace(resume.Name)
	resume.RequestDigest = strings.TrimSpace(resume.RequestDigest)
	if resume.WaitID == "" || resume.Name == "" || resume.RequestDigest == "" || len(resume.ResponseJSON) > maxInvocationResumeResponseBytes {
		return Approval{}, Invocation{}, nil, ErrInvalidResume
	}
	outbox := commit.Outbox
	outbox.InvocationID = invocationID
	outbox.WaitID = strings.TrimSpace(outbox.WaitID)
	outbox.Name = strings.TrimSpace(outbox.Name)
	outbox.RequestDigest = strings.TrimSpace(outbox.RequestDigest)
	if outbox.RequestDigest != resume.RequestDigest || outbox.WaitID != resume.WaitID || outbox.Name != resume.Name || string(outbox.ResponseJSON) != string(resume.ResponseJSON) {
		return Approval{}, Invocation{}, nil, ErrConflict
	}
	now := nowUTC()
	normalizedOutbox, err := outbox.Normalize(now)
	if err != nil {
		return Approval{}, Invocation{}, nil, err
	}
	var rejectionDelivery RuntimeApprovalRejectionDeliveryOutbox
	if commit.RejectionDelivery != nil {
		if commit.ApprovalToStatus != ApprovalRejected || commit.ToolCallStatus != ToolCallRejected {
			return Approval{}, Invocation{}, nil, ErrConflict
		}
		rejectionDelivery = *commit.RejectionDelivery
		if strings.TrimSpace(rejectionDelivery.ApprovalID) != approvalID || strings.TrimSpace(rejectionDelivery.InvocationID) != invocationID || strings.TrimSpace(rejectionDelivery.ToolCallID) != strings.TrimSpace(commit.ToolCallID) {
			return Approval{}, Invocation{}, nil, ErrConflict
		}
		if strings.TrimSpace(rejectionDelivery.ReasonDigest) == "" {
			return Approval{}, Invocation{}, nil, ErrInvalidResume
		}
		computedReasonDigest, digestErr := RuntimeApprovalRejectionDeliveryReasonDigest(commit.Reason)
		if digestErr != nil || computedReasonDigest != strings.TrimSpace(strings.ToLower(rejectionDelivery.ReasonDigest)) {
			return Approval{}, Invocation{}, nil, ErrConflict
		}
		normalizedDelivery, deliveryErr := rejectionDelivery.Normalize(now)
		if deliveryErr != nil {
			return Approval{}, Invocation{}, nil, deliveryErr
		}
		rejectionDelivery = normalizedDelivery
	}
	approval, ok := r.approvals[approvalID]
	if !ok {
		return Approval{}, Invocation{}, nil, ErrNotFound
	}
	if approval.InvocationID != invocationID || approval.Status != commit.ApprovalFromStatus {
		return Approval{}, Invocation{}, nil, ErrConflict
	}
	invocation, ok := r.invocations[invocationID]
	if !ok {
		return Approval{}, Invocation{}, nil, ErrNotFound
	}
	if invocation.Status != commit.InvocationFromStatus {
		return Approval{}, Invocation{}, nil, ErrConflict
	}
	if commit.RejectionDelivery != nil && (strings.TrimSpace(approval.ToolCallID) != rejectionDelivery.ToolCallID || strings.TrimSpace(approval.OperationID) != rejectionDelivery.OperationID) {
		return Approval{}, Invocation{}, nil, ErrConflict
	}
	if commit.ToolCallID != "" {
		toolCall, exists := r.toolCalls[strings.TrimSpace(commit.ToolCallID)]
		if !exists {
			return Approval{}, Invocation{}, nil, ErrNotFound
		}
		if toolCall.InvocationID != invocationID || (commit.ToolCallStatus != ToolCallApproved && commit.ToolCallStatus != ToolCallRejected) {
			return Approval{}, Invocation{}, nil, ErrConflict
		}
	}
	if commit.RejectionDelivery != nil {
		key := rejectionApprovalKey(rejectionDelivery)
		if existingID, exists := r.rejectionByApproval[key]; exists && existingID != rejectionDelivery.ID {
			return Approval{}, Invocation{}, nil, ErrConflict
		}
		if existing, exists := r.rejectionOutbox[rejectionDelivery.ID]; exists && !existing.Matches(rejectionDelivery) {
			return Approval{}, Invocation{}, nil, ErrConflict
		}
	}
	if existing, exists := r.resumes[invocationID]; exists {
		if existing.WaitID != resume.WaitID || existing.Name != resume.Name || existing.RequestDigest != resume.RequestDigest || string(existing.ResponseJSON) != string(resume.ResponseJSON) {
			return Approval{}, Invocation{}, nil, ErrConflict
		}
	} else {
		if resume.CreatedAt.IsZero() {
			resume.CreatedAt = now
		} else {
			resume.CreatedAt = resume.CreatedAt.UTC()
		}
	}
	items := r.resumeOutbox[invocationID]
	foundOutbox := false
	for _, existing := range items {
		if existing.RequestDigest != normalizedOutbox.RequestDigest {
			continue
		}
		if existing.WaitID != normalizedOutbox.WaitID || existing.Name != normalizedOutbox.Name || string(existing.ResponseJSON) != string(normalizedOutbox.ResponseJSON) {
			return Approval{}, Invocation{}, nil, ErrConflict
		}
		foundOutbox = true
		break
	}
	if !foundOutbox && normalizedOutbox.ID == "" {
		normalizedOutbox.ID = invocationID + "-resume-outbox-" + normalizedOutbox.RequestDigest
	}
	// Validate and normalize all events before mutating any map. This keeps the
	// memory implementation's all-or-nothing behaviour equivalent to SQLite.
	if len(commit.Events) == 0 || len(commit.Events) > 8 {
		return Approval{}, Invocation{}, nil, ErrConflict
	}
	existingEventIDs := make(map[string]struct{}, len(r.events[invocationID])+len(commit.Events))
	for _, existing := range r.events[invocationID] {
		existingEventIDs[existing.ID] = struct{}{}
	}
	events := make([]AgentEvent, len(commit.Events))
	for index, event := range commit.Events {
		event.InvocationID = invocationID
		if strings.TrimSpace(event.ID) == "" {
			event.ID = newID("event")
		}
		if _, duplicate := existingEventIDs[event.ID]; duplicate || r.hasEventIDLocked(event.ID) {
			return Approval{}, Invocation{}, nil, ErrConflict
		}
		existingEventIDs[event.ID] = struct{}{}
		if event.Timestamp.IsZero() {
			event.Timestamp = now
		} else {
			event.Timestamp = event.Timestamp.UTC()
		}
		if event.Type == "" {
			return Approval{}, Invocation{}, nil, ErrConflict
		}
		events[index] = event
	}
	// Build the event delivery cursors before mutating any repository map. A
	// malformed event or an unexpected cursor collision must leave the whole
	// approval/rejection commit untouched, matching SQLite's transaction
	// semantics.
	sequence := int64(0)
	for _, existing := range r.events[invocationID] {
		if existing.Sequence > sequence {
			sequence = existing.Sequence
		}
	}
	preparedOutboxes := make([]RuntimeEventOutbox, len(events))
	for index := range events {
		sequence++
		events[index].Sequence = sequence
		outbox, outboxErr := NewRuntimeEventOutbox(events[index])
		if outboxErr != nil {
			return Approval{}, Invocation{}, nil, outboxErr
		}
		if existing, exists := r.eventOutbox[outbox.ID]; exists {
			if existing.EventID != outbox.EventID || existing.InvocationID != outbox.InvocationID || existing.Sequence != outbox.Sequence || existing.Type != outbox.Type {
				return Approval{}, Invocation{}, nil, ErrConflict
			}
			return Approval{}, Invocation{}, nil, ErrConflict
		}
		preparedOutboxes[index] = outbox
	}
	var deliveryGroup RuntimeDeliveryGroup
	if commit.RejectionDelivery != nil {
		members := make([]RuntimeDeliveryGroupMember, 0, len(preparedOutboxes)+1)
		for index, outbox := range preparedOutboxes {
			members = append(members, RuntimeDeliveryGroupMember{Kind: RuntimeDeliveryKindEvent, OutboxID: outbox.ID, DeliveryID: events[index].ID})
		}
		members = append(members, RuntimeDeliveryGroupMember{Kind: RuntimeDeliveryKindRejection, OutboxID: rejectionDelivery.ID, DeliveryID: rejectionDelivery.DeliveryID})
		var groupErr error
		deliveryGroup, groupErr = NewRuntimeDeliveryGroup(rejectionDelivery.Source, rejectionDelivery.Destination, invocationID, members, now)
		if groupErr != nil {
			return Approval{}, Invocation{}, nil, groupErr
		}
		if existing, exists := r.deliveryGroups[deliveryGroup.ID]; exists && !existing.MatchesIdentity(deliveryGroup) {
			return Approval{}, Invocation{}, nil, ErrConflict
		}
		for index := range preparedOutboxes {
			preparedOutboxes[index].GroupID = deliveryGroup.ID
		}
		rejectionDelivery.GroupID = deliveryGroup.ID
	}
	if _, exists := r.resumes[invocationID]; !exists {
		r.resumes[invocationID] = cloneInvocationResume(resume)
	}
	if !foundOutbox {
		r.resumeOutbox[invocationID] = append(items, cloneInvocationResumeOutbox(normalizedOutbox))
	}
	approval.Status = commit.ApprovalToStatus
	approval.DecisionReason = strings.TrimSpace(commit.Reason)
	approval.UpdatedAt = now
	resolvedAt := now
	approval.ResolvedAt = &resolvedAt
	r.approvals[approvalID] = approval
	invocation.Status = commit.InvocationToStatus
	invocation.Error = ""
	invocation.ActiveApprovalID = ""
	invocation.LeaseOwner = ""
	invocation.LeaseExpiresAt = nil
	invocation.UpdatedAt = now
	r.invocations[invocationID] = invocation
	if commit.ToolCallID != "" {
		toolCall := r.toolCalls[strings.TrimSpace(commit.ToolCallID)]
		toolCall.Status = commit.ToolCallStatus
		if strings.TrimSpace(commit.Reason) != "" {
			toolCall.Error = strings.TrimSpace(commit.Reason)
		}
		toolCall.UpdatedAt = now
		if toolCall.Status.Terminal() {
			finishedAt := now
			toolCall.FinishedAt = &finishedAt
		}
		r.toolCalls[toolCall.ID] = toolCall
	}
	for index := range events {
		outbox := preparedOutboxes[index]
		r.events[invocationID] = append(r.events[invocationID], events[index])
		r.eventOutbox[outbox.ID] = cloneRuntimeEventOutbox(outbox)
	}
	if commit.RejectionDelivery != nil {
		if r.rejectionOutbox == nil {
			r.rejectionOutbox = make(map[string]RuntimeApprovalRejectionDeliveryOutbox)
		}
		if r.rejectionByApproval == nil {
			r.rejectionByApproval = make(map[string]string)
		}
		r.rejectionOutbox[rejectionDelivery.ID] = cloneRuntimeApprovalRejectionDeliveryOutbox(rejectionDelivery)
		r.rejectionByApproval[rejectionApprovalKey(rejectionDelivery)] = rejectionDelivery.ID
		if deliveryGroup.ID != "" {
			if r.deliveryGroups == nil {
				r.deliveryGroups = make(map[string]RuntimeDeliveryGroup)
			}
			r.deliveryGroups[deliveryGroup.ID] = cloneRuntimeDeliveryGroup(deliveryGroup)
		}
	}
	return approval, invocation, events, nil
}

func (r *MemoryRepository) GetInvocationResume(_ context.Context, invocationID string) (InvocationResume, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.resumes[strings.TrimSpace(invocationID)]
	if !ok {
		return InvocationResume{}, ErrNotFound
	}
	return cloneInvocationResume(item), nil
}

func (r *MemoryRepository) DeleteInvocationResume(_ context.Context, invocationID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	invocationID = strings.TrimSpace(invocationID)
	if _, ok := r.resumes[invocationID]; !ok {
		return ErrNotFound
	}
	delete(r.resumes, invocationID)
	return nil
}

func cloneInvocationResume(item InvocationResume) InvocationResume {
	item.ResponseJSON = append([]byte(nil), item.ResponseJSON...)
	return item
}

func (r *MemoryRepository) CreateInvocation(_ context.Context, item Invocation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.invocations[item.ID]; ok {
		return ErrConflict
	}
	normalizeInvocationConfigSnapshot(&item)
	r.invocations[item.ID] = item
	return nil
}

func (r *MemoryRepository) GetInvocation(_ context.Context, id string) (Invocation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.invocations[strings.TrimSpace(id)]
	if !ok {
		return Invocation{}, ErrNotFound
	}
	return item, nil
}

func (r *MemoryRepository) GetInvocationByIdempotencyKey(_ context.Context, userID, conversationID, key string) (Invocation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	userID = strings.TrimSpace(userID)
	conversationID = strings.TrimSpace(conversationID)
	key = strings.TrimSpace(key)
	if key == "" {
		return Invocation{}, ErrNotFound
	}
	for _, item := range r.invocations {
		if item.UserID == userID && item.ConversationID == conversationID && item.IdempotencyKey == key {
			return item, nil
		}
	}
	return Invocation{}, ErrNotFound
}

func (r *MemoryRepository) AcquireInvocationLease(_ context.Context, id, owner string, now time.Time, ttl time.Duration) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id = strings.TrimSpace(id)
	owner = strings.TrimSpace(owner)
	if id == "" || owner == "" || ttl <= 0 {
		return false, ErrConflict
	}
	item, ok := r.invocations[id]
	if !ok {
		return false, ErrNotFound
	}
	now = now.UTC()
	if item.Status != InvocationQueued && item.Status != InvocationRunning {
		return false, nil
	}
	if item.LeaseOwner != "" && item.LeaseOwner != owner && item.LeaseExpiresAt != nil && item.LeaseExpiresAt.After(now) {
		return false, nil
	}
	expires := now.Add(ttl)
	item.LeaseOwner = owner
	item.LeaseExpiresAt = &expires
	item.UpdatedAt = now
	r.invocations[id] = item
	return true, nil
}

func (r *MemoryRepository) RenewInvocationLease(_ context.Context, id, owner string, now time.Time, ttl time.Duration) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id = strings.TrimSpace(id)
	owner = strings.TrimSpace(owner)
	if id == "" || owner == "" || ttl <= 0 {
		return false, ErrConflict
	}
	item, ok := r.invocations[id]
	if !ok {
		return false, ErrNotFound
	}
	now = now.UTC()
	if item.Status != InvocationRunning || item.LeaseOwner != owner || item.LeaseExpiresAt == nil || !item.LeaseExpiresAt.After(now) {
		return false, nil
	}
	expires := now.Add(ttl)
	item.LeaseExpiresAt = &expires
	item.UpdatedAt = now
	r.invocations[id] = item
	return true, nil
}

func (r *MemoryRepository) ReleaseInvocationLease(_ context.Context, id, owner string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id = strings.TrimSpace(id)
	owner = strings.TrimSpace(owner)
	if id == "" || owner == "" {
		return false, ErrConflict
	}
	item, ok := r.invocations[id]
	if !ok {
		return false, ErrNotFound
	}
	if item.LeaseOwner != owner {
		return false, nil
	}
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	item.UpdatedAt = nowUTC()
	r.invocations[id] = item
	return true, nil
}

func (r *MemoryRepository) ListInvocations(_ context.Context, userID string, statuses []InvocationStatus) ([]Invocation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	allowed := make(map[InvocationStatus]bool, len(statuses))
	for _, status := range statuses {
		allowed[status] = true
	}
	result := make([]Invocation, 0, len(r.invocations))
	for _, item := range r.invocations {
		if userID != "" && item.UserID != userID {
			continue
		}
		if len(allowed) > 0 && !allowed[item.Status] {
			continue
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result, nil
}

func (r *MemoryRepository) TransitionInvocation(_ context.Context, id string, from, to InvocationStatus, message string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.invocations[strings.TrimSpace(id)]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != from {
		return false, nil
	}
	if !validInvocationTransition(from, to) {
		return false, ErrConflict
	}
	item.Status = to
	item.Error = message
	item.UpdatedAt = nowUTC()
	if to.Terminal() {
		now := nowUTC()
		item.FinishedAt = &now
	}
	r.invocations[item.ID] = item
	return true, nil
}

// CommitInterruptedInvocation closes an abandoned running Invocation and, when
// present, blocks its active TaskPlan step under the same memory lock. The
// event rows are prepared and validated before any map is mutated so callers
// observe the same all-or-nothing behavior as the SQLite implementation.
func (r *MemoryRepository) CommitInterruptedInvocation(_ context.Context, commit InterruptedInvocationCommit) (Invocation, *TaskPlan, []AgentEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	invocationID := strings.TrimSpace(commit.InvocationID)
	if invocationID == "" || !commit.ToStatus.Terminal() || !validInvocationTransition(commit.FromStatus, commit.ToStatus) {
		return Invocation{}, nil, nil, ErrConflict
	}
	current, ok := r.invocations[invocationID]
	if !ok {
		return Invocation{}, nil, nil, ErrNotFound
	}
	if current.Status != commit.FromStatus {
		return Invocation{}, nil, nil, ErrConflict
	}

	now := nowUTC()
	var blockedPlan *TaskPlan
	if commit.Plan != nil {
		stored, exists := r.plans[invocationID]
		if !exists {
			return Invocation{}, nil, nil, ErrNotFound
		}
		if commit.Plan.Revision != stored.Revision {
			return Invocation{}, nil, nil, ErrConflict
		}
		proposed := cloneTaskPlan(*commit.Plan)
		proposed.ID = stored.ID
		proposed.InvocationID = invocationID
		proposed.Revision = stored.Revision + 1
		proposed.CreatedAt = stored.CreatedAt
		normalized, normalizeErr := proposed.Normalize(now)
		if normalizeErr != nil {
			return Invocation{}, nil, nil, normalizeErr
		}
		blockedPlan = &normalized
	}
	if len(commit.Events) == 0 {
		return Invocation{}, nil, nil, ErrConflict
	}

	preparedEvents := make([]AgentEvent, len(commit.Events))
	preparedOutboxes := make([]RuntimeEventOutbox, len(commit.Events))
	seenIDs := make(map[string]struct{}, len(commit.Events))
	terminalType := terminalEventForStatus(commit.ToStatus)
	terminalSeen := false
	planSeen := false
	for index, event := range commit.Events {
		event.InvocationID = invocationID
		event.ID = strings.TrimSpace(event.ID)
		if event.ID == "" {
			event.ID = newID("event")
		}
		if _, duplicate := seenIDs[event.ID]; duplicate {
			return Invocation{}, nil, nil, ErrConflict
		}
		seenIDs[event.ID] = struct{}{}
		if r.hasEventIDLocked(event.ID) {
			return Invocation{}, nil, nil, ErrConflict
		}
		if event.Timestamp.IsZero() {
			event.Timestamp = now
		} else {
			event.Timestamp = event.Timestamp.UTC()
		}
		event.Type = strings.TrimSpace(event.Type)
		if event.Type == "" {
			return Invocation{}, nil, nil, ErrConflict
		}
		switch event.Type {
		case EventPlanUpdated:
			if blockedPlan == nil || planSeen {
				return Invocation{}, nil, nil, ErrConflict
			}
			planSeen = true
			data := map[string]any{"plan": cloneTaskPlan(*blockedPlan)}
			if reason, ok := event.Data["reason"]; ok {
				data["reason"] = reason
			}
			event.Data = data
		case EventInvocationCompleted, EventInvocationFailed, EventInvocationCancelled, EventInvocationExpired:
			if event.Type != terminalType || terminalSeen || index != len(commit.Events)-1 {
				return Invocation{}, nil, nil, ErrConflict
			}
			terminalSeen = true
		}
		if index == 0 {
			event.Sequence = int64(len(r.events[invocationID]) + 1)
		} else {
			event.Sequence = preparedEvents[index-1].Sequence + 1
		}
		outbox, outboxErr := NewRuntimeEventOutbox(event)
		if outboxErr != nil {
			return Invocation{}, nil, nil, outboxErr
		}
		preparedOutboxes[index] = outbox
		preparedEvents[index] = event
	}
	if !terminalSeen || (blockedPlan != nil && !planSeen) || (blockedPlan == nil && planSeen) {
		return Invocation{}, nil, nil, ErrConflict
	}

	current.Status = commit.ToStatus
	current.Error = sanitizeRuntimeString(strings.TrimSpace(commit.Message))
	current.UpdatedAt = now
	current.LeaseOwner = ""
	current.LeaseExpiresAt = nil
	finishedAt := now
	current.FinishedAt = &finishedAt
	r.invocations[invocationID] = current
	if blockedPlan != nil {
		r.plans[invocationID] = cloneTaskPlan(*blockedPlan)
	}
	r.events[invocationID] = append(r.events[invocationID], preparedEvents...)
	for _, outbox := range preparedOutboxes {
		r.eventOutbox[outbox.ID] = cloneRuntimeEventOutbox(outbox)
	}
	var resultPlan *TaskPlan
	if blockedPlan != nil {
		copyPlan := cloneTaskPlan(*blockedPlan)
		resultPlan = &copyPlan
	}
	return current, resultPlan, preparedEvents, nil
}

// CommitWorkflowContinuation atomically creates a child Invocation and plan
// while appending the source audit and child queued event. The source remains
// terminal; a deterministic child ID makes a repeated user decision return
// the original child instead of creating another runnable task.
func (r *MemoryRepository) CommitWorkflowContinuation(_ context.Context, commit WorkflowContinuationCommit) (Invocation, TaskPlan, []AgentEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	sourceID := strings.TrimSpace(commit.SourceInvocationID)
	child := commit.Child
	child.ID = strings.TrimSpace(child.ID)
	if sourceID == "" || child.ID == "" || child.Status != InvocationQueued || child.ParentInvocationID != sourceID || child.ContinuationDecision != commit.Decision || child.ContinuationSourcePlanRevision != commit.SourcePlanRevision || !validWorkflowContinuationDecision(commit.Decision) || commit.SourcePlanRevision <= 0 || strings.TrimSpace(commit.StepID) == "" {
		return Invocation{}, TaskPlan{}, nil, ErrConflict
	}
	source, ok := r.invocations[sourceID]
	if !ok {
		return Invocation{}, TaskPlan{}, nil, ErrNotFound
	}
	if source.Status != InvocationFailed || !strings.Contains(strings.ToLower(source.Error), "runtime_interrupted") {
		return Invocation{}, TaskPlan{}, nil, ErrConflict
	}
	if len(r.events[sourceID]) > maxRuntimeEventReplay || !HasWorkflowRecoveryProof(r.events[sourceID]) {
		return Invocation{}, TaskPlan{}, nil, ErrConflict
	}
	sourcePlan, ok := r.plans[sourceID]
	if !ok {
		return Invocation{}, TaskPlan{}, nil, ErrNotFound
	}
	if sourcePlan.Revision != commit.SourcePlanRevision {
		return Invocation{}, TaskPlan{}, nil, ErrConflict
	}
	commit.Child = child
	if err := ValidateWorkflowContinuationCommit(commit, source, sourcePlan); err != nil {
		return Invocation{}, TaskPlan{}, nil, err
	}
	// Reconstruct the expected child plan from the source and compare the
	// caller's payload after normalization; repository callers cannot bypass
	// the same evidence and blocker checks as the Coordinator.
	evidence := continuationEvidenceFromPlan(commit.ChildPlan, commit.StepID, commit.Decision)
	request := WorkflowContinuationRequest{ExpectedPlanRevision: commit.SourcePlanRevision, Decision: commit.Decision, StepID: commit.StepID, IdempotencyKey: commit.IdempotencyKey, Evidence: evidence}
	request, err := normalizeWorkflowContinuationRequest(sourceID, request)
	if err != nil {
		return Invocation{}, TaskPlan{}, nil, err
	}
	expectedPlan, err := workflowContinuationPlan(sourcePlan, child.ID, request, nowUTC())
	if err != nil || !plansSemanticallyEqual(expectedPlan, commit.ChildPlan) {
		if err != nil {
			return Invocation{}, TaskPlan{}, nil, err
		}
		return Invocation{}, TaskPlan{}, nil, ErrConflict
	}
	if existing, exists := r.invocations[child.ID]; exists {
		if !workflowContinuationInvocationMatches(existing, child) {
			return Invocation{}, TaskPlan{}, nil, ErrConflict
		}
		existingPlan, planExists := r.plans[child.ID]
		if !planExists || !plansSemanticallyEqual(existingPlan, commit.ChildPlan) {
			return Invocation{}, TaskPlan{}, nil, ErrConflict
		}
		return existing, cloneTaskPlan(existingPlan), nil, nil
	}
	for _, existing := range r.invocations {
		if existing.ParentInvocationID == sourceID && existing.ID != child.ID {
			return Invocation{}, TaskPlan{}, nil, ErrConflict
		}
	}
	for _, active := range r.invocations {
		if active.ID == sourceID || active.ID == child.ID {
			continue
		}
		if active.UserID == child.UserID && active.ConversationID == child.ConversationID && isActiveInvocationStatus(active.Status) {
			return Invocation{}, TaskPlan{}, nil, ErrConflict
		}
	}
	if child.UserID == "" || child.ConversationID == "" || child.SessionID == "" || child.Message == "" {
		return Invocation{}, TaskPlan{}, nil, ErrConflict
	}
	if len(commit.ChildEvents) != 1 {
		return Invocation{}, TaskPlan{}, nil, ErrConflict
	}
	if _, exists := r.plans[child.ID]; exists || r.hasEventIDLocked(commit.SourceEvent.ID) || r.hasEventIDLocked(commit.ChildEvents[0].ID) {
		return Invocation{}, TaskPlan{}, nil, ErrConflict
	}
	childPlan := cloneTaskPlan(commit.ChildPlan)
	childPlan.ID = strings.TrimSpace(childPlan.ID)
	childPlan.InvocationID = child.ID
	if childPlan.ID == "" || childPlan.InvocationID != child.ID {
		return Invocation{}, TaskPlan{}, nil, ErrConflict
	}
	if err := childPlan.Validate(); err != nil {
		return Invocation{}, TaskPlan{}, nil, err
	}
	child.Attachments = cloneAttachments(child.Attachments)
	normalizeInvocationConfigSnapshot(&child)
	r.invocations[child.ID] = child
	r.plans[child.ID] = childPlan

	storedEvents := make([]AgentEvent, 0, 2)
	store := func(event AgentEvent, invocationID string, sequence int64) error {
		event.InvocationID = invocationID
		event.ID = strings.TrimSpace(event.ID)
		event.Type = strings.TrimSpace(event.Type)
		if event.ID == "" || event.Type == "" || r.hasEventIDLocked(event.ID) {
			return ErrConflict
		}
		if event.Timestamp.IsZero() {
			event.Timestamp = nowUTC()
		} else {
			event.Timestamp = event.Timestamp.UTC()
		}
		event.Sequence = sequence
		outbox, outboxErr := NewRuntimeEventOutbox(event)
		if outboxErr != nil {
			return outboxErr
		}
		r.events[invocationID] = append(r.events[invocationID], cloneAgentEvent(event))
		r.eventOutbox[outbox.ID] = cloneRuntimeEventOutbox(outbox)
		storedEvents = append(storedEvents, cloneAgentEvent(event))
		return nil
	}
	if err := store(commit.SourceEvent, sourceID, int64(len(r.events[sourceID])+1)); err != nil {
		delete(r.invocations, child.ID)
		delete(r.plans, child.ID)
		delete(r.events, child.ID)
		return Invocation{}, TaskPlan{}, nil, err
	}
	if err := store(commit.ChildEvents[0], child.ID, 1); err != nil {
		delete(r.invocations, child.ID)
		delete(r.plans, child.ID)
		delete(r.events, child.ID)
		for _, event := range storedEvents {
			delete(r.eventOutbox, runtimeEventOutboxIDForEvent(event))
			if event.InvocationID == sourceID {
				r.events[sourceID] = r.events[sourceID][:len(r.events[sourceID])-1]
			}
		}
		return Invocation{}, TaskPlan{}, nil, err
	}
	return child, cloneTaskPlan(childPlan), storedEvents, nil
}

func workflowContinuationInvocationMatches(existing, expected Invocation) bool {
	return existing.ID == expected.ID && existing.ParentInvocationID == expected.ParentInvocationID && existing.UserID == expected.UserID && existing.BotID == expected.BotID && existing.ConversationID == expected.ConversationID && existing.WorkspaceID == expected.WorkspaceID && existing.TargetPath == expected.TargetPath && existing.SessionID == expected.SessionID && existing.ProviderID == expected.ProviderID && existing.ModelID == expected.ModelID && existing.Message == expected.Message && existing.IdempotencyKey == expected.IdempotencyKey && existing.ContinuationDecision == expected.ContinuationDecision && existing.ContinuationSourcePlanRevision == expected.ContinuationSourcePlanRevision && existing.ConfigSnapshot == expected.ConfigSnapshot && len(existing.Attachments) == 0 && len(expected.Attachments) == 0
}

func isActiveInvocationStatus(status InvocationStatus) bool {
	switch status {
	case InvocationQueued, InvocationRunning, InvocationWaitingApproval, InvocationWaitingTool, InvocationWaitingUser, InvocationWaitingSubagents, InvocationCancelling:
		return true
	default:
		return false
	}
}

func runtimeEventOutboxIDForEvent(event AgentEvent) string {
	item, err := NewRuntimeEventOutbox(event)
	if err != nil {
		return ""
	}
	return item.ID
}

func (r *MemoryRepository) UpdateInvocation(_ context.Context, item Invocation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.invocations[item.ID]; !ok {
		return ErrNotFound
	}
	normalizeInvocationConfigSnapshot(&item)
	item.UpdatedAt = nowUTC()
	r.invocations[item.ID] = item
	return nil
}

// CommitRuntimeConfigMigration updates the hidden Invocation snapshot and its
// metadata-only audit event/outbox cursor under one memory lock. This mirrors
// the SQLite transaction so a failed event/outbox write cannot leave a
// migrated snapshot without an audit trail.
func (r *MemoryRepository) CommitRuntimeConfigMigration(_ context.Context, commit RuntimeConfigMigrationCommit) (Invocation, AgentEvent, error) {
	now := nowUTC()
	normalized, newSnapshot, newDigest, err := normalizeRuntimeConfigMigrationCommit(commit, now)
	if err != nil {
		return Invocation{}, AgentEvent{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	invocation, ok := r.invocations[normalized.InvocationID]
	if !ok {
		return Invocation{}, AgentEvent{}, ErrNotFound
	}
	if !runtimeConfigMigrationWaitingStatus(invocation.Status) {
		return Invocation{}, AgentEvent{}, ErrConflict
	}
	storedDigest := agent.RuntimeConfigSnapshotDigest(invocation.ConfigSnapshot)
	if existing, exists := r.eventByIDLocked(normalized.Event.ID); exists {
		if existing.InvocationID != normalized.InvocationID || existing.Type != EventRuntimeConfigMigrated || !runtimeConfigMigrationEventMatches(existing, normalized.ExpectedDigest, newDigest, normalized.IdempotencyKey) {
			return Invocation{}, AgentEvent{}, ErrConflict
		}
		if storedDigest != newDigest {
			return Invocation{}, AgentEvent{}, ErrConflict
		}
		return invocation, cloneAgentEvent(existing), nil
	}
	if storedDigest == "" || storedDigest != normalized.ExpectedDigest {
		return Invocation{}, AgentEvent{}, ErrConflict
	}
	if newDigest == storedDigest {
		return invocation, AgentEvent{}, nil
	}
	if normalized.Event.Timestamp.IsZero() {
		normalized.Event.Timestamp = now
	}
	normalized.Event.Timestamp = normalized.Event.Timestamp.UTC()
	normalized.Event.Sequence = int64(len(r.events[invocation.ID]) + 1)
	previousSnapshot, _, _, previousErr := normalizeRuntimeConfigMigrationSnapshot(invocation.ConfigSnapshot)
	if previousErr != nil {
		return Invocation{}, AgentEvent{}, ErrConflict
	}
	data, dataErr := runtimeConfigMigrationEventData(invocation.ID, storedDigest, newDigest, normalized.IdempotencyKey, previousSnapshot, newSnapshot)
	if dataErr != nil {
		return Invocation{}, AgentEvent{}, dataErr
	}
	normalized.Event.Data = data
	if err := r.appendRuntimeEventOutboxLocked(normalized.Event); err != nil {
		return Invocation{}, AgentEvent{}, err
	}
	invocation.ConfigSnapshot = normalized.NewSnapshot
	invocation.ConfigSnapshotDigest = newDigest
	invocation.UpdatedAt = now
	r.invocations[invocation.ID] = invocation
	r.events[invocation.ID] = append(r.events[invocation.ID], cloneAgentEvent(normalized.Event))
	return invocation, cloneAgentEvent(normalized.Event), nil
}

func runtimeConfigMigrationEventMatches(event AgentEvent, expectedDigest, newDigest, idempotencyKey string) bool {
	if event.Data == nil {
		return false
	}
	from, _ := event.Data["from_digest"].(string)
	to, _ := event.Data["to_digest"].(string)
	key, _ := event.Data["idempotency_key"].(string)
	return strings.TrimSpace(from) == strings.TrimSpace(expectedDigest) && strings.TrimSpace(to) == strings.TrimSpace(newDigest) && strings.TrimSpace(key) == strings.TrimSpace(idempotencyKey)
}

func normalizeInvocationConfigSnapshot(item *Invocation) {
	if item == nil {
		return
	}
	item.ConfigSnapshot = strings.TrimSpace(item.ConfigSnapshot)
	if item.ConfigSnapshot == "" {
		item.ConfigSnapshotDigest = ""
		return
	}
	item.ConfigSnapshotDigest = agent.RuntimeConfigSnapshotDigest(item.ConfigSnapshot)
}

func (r *MemoryRepository) AppendEvent(_ context.Context, event AgentEvent) (AgentEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	event.InvocationID = strings.TrimSpace(event.InvocationID)
	event.ID = strings.TrimSpace(event.ID)
	event.Type = strings.TrimSpace(event.Type)
	if _, ok := r.invocations[event.InvocationID]; !ok {
		return AgentEvent{}, ErrNotFound
	}
	if event.ID == "" {
		event.ID = newID("event")
	}
	if event.Type == "" || r.hasEventIDLocked(event.ID) {
		return AgentEvent{}, ErrConflict
	}
	items := r.events[event.InvocationID]
	event.Sequence = int64(len(items) + 1)
	if event.Timestamp.IsZero() {
		event.Timestamp = nowUTC()
	}
	if err := r.appendRuntimeEventOutboxLocked(event); err != nil {
		return AgentEvent{}, err
	}
	r.events[event.InvocationID] = append(items, event)
	return event, nil
}

func (r *MemoryRepository) GetAgentEvent(_ context.Context, eventID string) (AgentEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	eventID = strings.TrimSpace(eventID)
	for _, events := range r.events {
		for _, event := range events {
			if event.ID == eventID {
				return cloneAgentEvent(event), nil
			}
		}
	}
	return AgentEvent{}, ErrNotFound
}

func (r *MemoryRepository) ListEvents(_ context.Context, invocationID string, after int64, limit int) ([]AgentEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.invocations[invocationID]; !ok {
		return nil, ErrNotFound
	}
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	result := make([]AgentEvent, 0, limit)
	for _, event := range r.events[invocationID] {
		if event.Sequence <= after {
			continue
		}
		result = append(result, event)
		if len(result) >= limit {
			break
		}
	}
	return result, nil
}

func (r *MemoryRepository) CreateApproval(_ context.Context, item Approval) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.approvals[item.ID]; ok {
		return ErrConflict
	}
	r.approvals[item.ID] = item
	return nil
}

func (r *MemoryRepository) GetApproval(_ context.Context, id string) (Approval, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.approvals[strings.TrimSpace(id)]
	if !ok {
		return Approval{}, ErrNotFound
	}
	return item, nil
}

func (r *MemoryRepository) ListApprovals(_ context.Context, invocationID string, status ApprovalStatus) ([]Approval, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]Approval, 0)
	for _, item := range r.approvals {
		if invocationID != "" && item.InvocationID != invocationID {
			continue
		}
		if status != "" && item.Status != status {
			continue
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result, nil
}

func (r *MemoryRepository) ResolveApproval(_ context.Context, id string, status ApprovalStatus, reason string) (Approval, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.approvals[strings.TrimSpace(id)]
	if !ok {
		return Approval{}, false, ErrNotFound
	}
	if item.Status != ApprovalPending {
		return item, false, nil
	}
	now := nowUTC()
	item.Status, item.DecisionReason, item.UpdatedAt, item.ResolvedAt = status, reason, now, &now
	r.approvals[item.ID] = item
	return item, true, nil
}

func (r *MemoryRepository) CreateToolCall(_ context.Context, item ToolCall) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.toolCalls[item.ID]; ok {
		return ErrConflict
	}
	r.toolCalls[item.ID] = item
	return nil
}

func (r *MemoryRepository) GetToolCall(_ context.Context, id string) (ToolCall, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.toolCalls[strings.TrimSpace(id)]
	if !ok {
		return ToolCall{}, ErrNotFound
	}
	return item, nil
}

func (r *MemoryRepository) ListToolCalls(_ context.Context, invocationID string) ([]ToolCall, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]ToolCall, 0)
	for _, item := range r.toolCalls {
		if strings.TrimSpace(invocationID) != "" && item.InvocationID != strings.TrimSpace(invocationID) {
			continue
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result, nil
}

func (r *MemoryRepository) UpdateToolCall(_ context.Context, item ToolCall) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.toolCalls[item.ID]; !ok {
		return ErrNotFound
	}
	item.UpdatedAt = nowUTC()
	if item.Status.Terminal() && item.FinishedAt == nil {
		now := nowUTC()
		item.FinishedAt = &now
	}
	r.toolCalls[item.ID] = item
	return nil
}

func (r *MemoryRepository) CreateTaskPlan(_ context.Context, item TaskPlan) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.invocations[item.InvocationID]; !ok {
		return ErrNotFound
	}
	if _, ok := r.plans[item.InvocationID]; ok {
		return ErrConflict
	}
	normalized, err := item.Normalize(nowUTC())
	if err != nil {
		return err
	}
	r.plans[normalized.InvocationID] = normalized
	return nil
}

func (r *MemoryRepository) GetTaskPlan(_ context.Context, invocationID string) (TaskPlan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.plans[strings.TrimSpace(invocationID)]
	if !ok {
		return TaskPlan{}, ErrNotFound
	}
	return cloneTaskPlan(item), nil
}

func (r *MemoryRepository) UpdateTaskPlan(_ context.Context, item TaskPlan, expectedRevision int64) (TaskPlan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.plans[strings.TrimSpace(item.InvocationID)]
	if !ok {
		return TaskPlan{}, ErrNotFound
	}
	if current.Revision != expectedRevision {
		return TaskPlan{}, ErrConflict
	}
	normalized, err := item.Normalize(nowUTC())
	if err != nil {
		return TaskPlan{}, err
	}
	r.plans[normalized.InvocationID] = normalized
	return cloneTaskPlan(normalized), nil
}

func cloneTaskPlan(item TaskPlan) TaskPlan {
	item.Steps = append([]PlanStep(nil), item.Steps...)
	for index := range item.Steps {
		item.Steps[index].DependsOn = append([]string(nil), item.Steps[index].DependsOn...)
		item.Steps[index].Evidence = append([]EvidenceRef(nil), item.Steps[index].Evidence...)
	}
	return item
}

func (r *MemoryRepository) CreateTaskContract(_ context.Context, item TaskContract) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.invocations[item.InvocationID]; !ok {
		return ErrNotFound
	}
	if item.Version <= 0 {
		item.Version = 1
	}
	if item.UpdatedAt.IsZero() {
		item.UpdatedAt = nowUTC()
	}
	if err := item.Validate(); err != nil {
		return err
	}
	if existing := r.contracts[item.InvocationID]; len(existing) > 0 {
		return ErrConflict
	}
	item = cloneTaskContract(item)
	r.contracts[item.InvocationID] = []TaskContract{item}
	return nil
}

func (r *MemoryRepository) SaveTaskContract(ctx context.Context, item TaskContract) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.invocations[item.InvocationID]; !ok {
		return ErrNotFound
	}
	if item.Version <= 0 {
		item.Version = 1
	}
	if item.UpdatedAt.IsZero() {
		item.UpdatedAt = nowUTC()
	}
	if err := item.Validate(); err != nil {
		return err
	}
	items := r.contracts[item.InvocationID]
	if len(items) == 0 {
		r.contracts[item.InvocationID] = []TaskContract{cloneTaskContract(item)}
		return nil
	}
	current := items[len(items)-1]
	if item.Version != current.Version+1 {
		return ErrConflict
	}
	r.contracts[item.InvocationID] = append(items, cloneTaskContract(item))
	return nil
}

func (r *MemoryRepository) GetTaskContract(_ context.Context, invocationID string) (TaskContract, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := r.contracts[strings.TrimSpace(invocationID)]
	if len(items) == 0 {
		return TaskContract{}, ErrNotFound
	}
	return cloneTaskContract(items[len(items)-1]), nil
}

func (r *MemoryRepository) ListTaskContracts(_ context.Context, invocationID string) ([]TaskContract, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := r.contracts[strings.TrimSpace(invocationID)]
	if len(items) == 0 {
		return nil, nil
	}
	result := make([]TaskContract, 0, len(items))
	for _, item := range items {
		result = append(result, cloneTaskContract(item))
	}
	return result, nil
}

func (r *MemoryRepository) UpdateTaskContract(_ context.Context, item TaskContract, expectedVersion int64) (TaskContract, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.invocations[item.InvocationID]; !ok {
		return TaskContract{}, ErrNotFound
	}
	items := r.contracts[strings.TrimSpace(item.InvocationID)]
	if len(items) == 0 {
		return TaskContract{}, ErrNotFound
	}
	current := items[len(items)-1]
	if current.Version != expectedVersion || item.Version != expectedVersion+1 {
		return TaskContract{}, ErrConflict
	}
	if err := item.Validate(); err != nil {
		return TaskContract{}, err
	}
	if item.UpdatedAt.IsZero() {
		item.UpdatedAt = nowUTC()
	}
	item = cloneTaskContract(item)
	r.contracts[item.InvocationID] = append(items, item)
	return cloneTaskContract(item), nil
}

func cloneTaskContract(item TaskContract) TaskContract {
	item.AcceptanceCriteria = append([]Criterion(nil), item.AcceptanceCriteria...)
	for index := range item.AcceptanceCriteria {
		item.AcceptanceCriteria[index].Evidence = append([]EvidenceRef(nil), item.AcceptanceCriteria[index].Evidence...)
	}
	item.Constraints = append([]Constraint(nil), item.Constraints...)
	item.NonGoals = append([]string(nil), item.NonGoals...)
	item.ExternalActions = append([]ExternalActionRule(nil), item.ExternalActions...)
	item.SourceMessageIDs = append([]string(nil), item.SourceMessageIDs...)
	return item
}

func (r *MemoryRepository) SaveContextManifest(_ context.Context, item ContextManifest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.invocations[item.InvocationID]; !ok {
		return ErrNotFound
	}
	if err := item.Validate(); err != nil {
		return err
	}
	items := r.manifests[item.InvocationID]
	for index := range items {
		if items[index].ModelCallID == item.ModelCallID {
			items[index] = item
			r.manifests[item.InvocationID] = items
			return nil
		}
	}
	r.manifests[item.InvocationID] = append(items, item)
	return nil
}

func (r *MemoryRepository) GetContextManifest(_ context.Context, modelCallID string) (ContextManifest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, items := range r.manifests {
		for _, item := range items {
			if item.ModelCallID == strings.TrimSpace(modelCallID) {
				return item, nil
			}
		}
	}
	return ContextManifest{}, ErrNotFound
}

func (r *MemoryRepository) ListContextManifests(_ context.Context, invocationID string) ([]ContextManifest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := r.manifests[strings.TrimSpace(invocationID)]
	result := append([]ContextManifest(nil), items...)
	return result, nil
}

func (r *MemoryRepository) GetWorkingSet(_ context.Context, invocationID string) (WorkingSet, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.workingSets[strings.TrimSpace(invocationID)]
	if !ok {
		return WorkingSet{}, ErrNotFound
	}
	return cloneWorkingSet(item), nil
}

func (r *MemoryRepository) ReplaceWorkingSet(_ context.Context, item WorkingSet) (WorkingSet, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item.InvocationID = strings.TrimSpace(item.InvocationID)
	if _, ok := r.invocations[item.InvocationID]; !ok {
		return WorkingSet{}, ErrNotFound
	}
	current := r.workingSets[item.InvocationID]
	if item.Revision <= 0 {
		item.Revision = current.Revision + 1
	}
	if current.Revision > 0 && item.Revision <= current.Revision {
		return WorkingSet{}, ErrConflict
	}
	if item.UpdatedAt.IsZero() {
		item.UpdatedAt = nowUTC()
	}
	if err := item.Validate(); err != nil {
		return WorkingSet{}, err
	}
	r.workingSets[item.InvocationID] = cloneWorkingSet(item)
	return cloneWorkingSet(item), nil
}

func cloneWorkingSet(item WorkingSet) WorkingSet {
	item.Items = append([]WorkingSetItem(nil), item.Items...)
	for index := range item.Items {
		item.Items[index].DependencyKeys = append([]string(nil), item.Items[index].DependencyKeys...)
		if item.Items[index].FileSlice != nil {
			slice := *item.Items[index].FileSlice
			item.Items[index].FileSlice = &slice
		}
	}
	return item
}

func (r *MemoryRepository) CreateVerificationRun(_ context.Context, item VerificationRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.invocations[item.InvocationID]; !ok {
		return ErrNotFound
	}
	if item.Revision <= 0 {
		item.Revision = 1
	}
	if item.Status == "" {
		item.Status = VerificationPending
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = nowUTC()
	}
	if item.UpdatedAt.IsZero() {
		item.UpdatedAt = item.CreatedAt
	}
	if err := item.Validate(); err != nil {
		return err
	}
	for _, existing := range r.verifications[item.InvocationID] {
		if existing.ID == item.ID {
			return ErrConflict
		}
	}
	r.verifications[item.InvocationID] = append(r.verifications[item.InvocationID], item)
	return nil
}

func (r *MemoryRepository) GetVerificationRun(_ context.Context, id string) (VerificationRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, items := range r.verifications {
		for _, item := range items {
			if item.ID == strings.TrimSpace(id) {
				return item, nil
			}
		}
	}
	return VerificationRun{}, ErrNotFound
}

func (r *MemoryRepository) ListVerificationRuns(_ context.Context, invocationID string) ([]VerificationRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := append([]VerificationRun(nil), r.verifications[strings.TrimSpace(invocationID)]...)
	sort.SliceStable(items, func(i, j int) bool { return items[i].CreatedAt.Before(items[j].CreatedAt) })
	return items, nil
}

func (r *MemoryRepository) UpdateVerificationRun(_ context.Context, item VerificationRun, expectedRevision int64) (VerificationRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := r.verifications[item.InvocationID]
	for index := range items {
		if items[index].ID != item.ID {
			continue
		}
		current := items[index]
		if current.Revision != expectedRevision {
			return VerificationRun{}, ErrConflict
		}
		item.Revision = expectedRevision + 1
		item.InvocationID = current.InvocationID
		item.CreatedAt = current.CreatedAt
		if item.UpdatedAt.IsZero() {
			item.UpdatedAt = nowUTC()
		}
		if err := item.Validate(); err != nil {
			return VerificationRun{}, err
		}
		items[index] = item
		r.verifications[item.InvocationID] = items
		return item, nil
	}
	return VerificationRun{}, ErrNotFound
}

func (r *MemoryRepository) GetRuntimeSnapshot(_ context.Context, invocationID string) (RuntimeSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.snapshots[strings.TrimSpace(invocationID)]
	if !ok {
		return RuntimeSnapshot{}, ErrNotFound
	}
	return cloneRuntimeSnapshot(item), nil
}

func (r *MemoryRepository) SaveRuntimeSnapshot(_ context.Context, item RuntimeSnapshot) (RuntimeSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.invocations[strings.TrimSpace(item.InvocationID)]; !ok {
		return RuntimeSnapshot{}, ErrNotFound
	}
	current := r.snapshots[item.InvocationID]
	if item.Revision <= 0 {
		item.Revision = current.Revision + 1
	}
	if current.Revision > 0 && item.Revision <= current.Revision {
		return RuntimeSnapshot{}, ErrConflict
	}
	if item.GeneratedAt.IsZero() {
		item.GeneratedAt = nowUTC()
	}
	if err := item.Validate(); err != nil {
		return RuntimeSnapshot{}, err
	}
	r.snapshots[item.InvocationID] = cloneRuntimeSnapshot(item)
	return cloneRuntimeSnapshot(item), nil
}

// CommitRuntimeSnapshot persists the snapshot and its timeline event under
// the same memory lock. Keeping the canonical event payload here means the
// in-memory implementation has the same all-or-nothing contract as SQLite.
func (r *MemoryRepository) CommitRuntimeSnapshot(ctx context.Context, item RuntimeSnapshot, event AgentEvent) (RuntimeSnapshot, AgentEvent, error) {
	saved, storedEvent, _, err := r.commitRuntimeSnapshotWithCheckpoint(ctx, item, event, "", "")
	return saved, storedEvent, err
}

// CommitRuntimeSnapshotWithCheckpoint extends the local Snapshot transaction
// with a checkpoint outbox cursor when an explicit source/destination route
// is configured. The checkpoint projection is created from the same Snapshot
// and event sequence while the memory lock is held.
func (r *MemoryRepository) CommitRuntimeSnapshotWithCheckpoint(ctx context.Context, item RuntimeSnapshot, event AgentEvent, source, destination string) (RuntimeSnapshot, AgentEvent, RuntimeCheckpointDeliveryOutbox, error) {
	return r.commitRuntimeSnapshotWithCheckpoint(ctx, item, event, source, destination)
}

func (r *MemoryRepository) commitRuntimeSnapshotWithCheckpoint(ctx context.Context, item RuntimeSnapshot, event AgentEvent, source, destination string) (RuntimeSnapshot, AgentEvent, RuntimeCheckpointDeliveryOutbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	invocationID := strings.TrimSpace(item.InvocationID)
	if invocationID == "" {
		return RuntimeSnapshot{}, AgentEvent{}, RuntimeCheckpointDeliveryOutbox{}, ErrInvalidRuntimeSnapshot
	}
	if event.InvocationID != "" && strings.TrimSpace(event.InvocationID) != invocationID {
		return RuntimeSnapshot{}, AgentEvent{}, RuntimeCheckpointDeliveryOutbox{}, ErrConflict
	}
	if _, ok := r.invocations[invocationID]; !ok {
		return RuntimeSnapshot{}, AgentEvent{}, RuntimeCheckpointDeliveryOutbox{}, ErrNotFound
	}

	item.InvocationID = invocationID
	current := r.snapshots[invocationID]
	if item.Revision <= 0 {
		item.Revision = current.Revision + 1
	}
	if current.Revision > 0 && item.Revision <= current.Revision {
		return RuntimeSnapshot{}, AgentEvent{}, RuntimeCheckpointDeliveryOutbox{}, ErrConflict
	}
	if item.GeneratedAt.IsZero() {
		item.GeneratedAt = nowUTC()
	} else {
		item.GeneratedAt = item.GeneratedAt.UTC()
	}
	if err := item.Validate(); err != nil {
		return RuntimeSnapshot{}, AgentEvent{}, RuntimeCheckpointDeliveryOutbox{}, err
	}

	event.InvocationID = invocationID
	if strings.TrimSpace(event.ID) == "" {
		event.ID = newID("event")
	}
	if event.Type == "" {
		event.Type = EventRuntimeSnapshot
	}
	if event.Type != EventRuntimeSnapshot {
		return RuntimeSnapshot{}, AgentEvent{}, RuntimeCheckpointDeliveryOutbox{}, ErrConflict
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = item.GeneratedAt
	} else {
		event.Timestamp = event.Timestamp.UTC()
	}
	// A runtime.snapshot event is a canonical recovery record. Do not retain
	// caller-owned maps or allow the event to describe a different revision.
	event.Data = map[string]any{"snapshot": cloneRuntimeSnapshot(item), "revision": item.Revision}
	for _, events := range r.events {
		for _, existing := range events {
			if existing.ID == event.ID {
				return RuntimeSnapshot{}, AgentEvent{}, RuntimeCheckpointDeliveryOutbox{}, ErrConflict
			}
		}
	}
	event.Sequence = int64(len(r.events[invocationID]) + 1)
	eventOutbox, outboxErr := NewRuntimeEventOutbox(event)
	if outboxErr != nil {
		return RuntimeSnapshot{}, AgentEvent{}, RuntimeCheckpointDeliveryOutbox{}, outboxErr
	}
	var checkpointOutbox RuntimeCheckpointDeliveryOutbox
	var deliveryGroup RuntimeDeliveryGroup
	if strings.TrimSpace(source) != "" || strings.TrimSpace(destination) != "" {
		if strings.TrimSpace(source) == "" || strings.TrimSpace(destination) == "" {
			return RuntimeSnapshot{}, AgentEvent{}, RuntimeCheckpointDeliveryOutbox{}, fmt.Errorf("%w: checkpoint source/destination 不完整", ErrInvalidRuntimeCheckpointDelivery)
		}
		checkpointOutbox, outboxErr = NewRuntimeCheckpointDeliveryOutbox(source, destination, item, event.Sequence)
		if outboxErr != nil {
			return RuntimeSnapshot{}, AgentEvent{}, RuntimeCheckpointDeliveryOutbox{}, outboxErr
		}
		if existing, exists := r.checkpointOutbox[checkpointOutbox.ID]; exists && !existing.Matches(checkpointOutbox) {
			return RuntimeSnapshot{}, AgentEvent{}, RuntimeCheckpointDeliveryOutbox{}, ErrConflict
		}
		deliveryGroup, outboxErr = NewRuntimeDeliveryGroup(source, destination, invocationID, []RuntimeDeliveryGroupMember{
			{Kind: RuntimeDeliveryKindEvent, OutboxID: eventOutbox.ID, DeliveryID: event.ID},
			{Kind: RuntimeDeliveryKindCheckpoint, OutboxID: checkpointOutbox.ID, DeliveryID: checkpointOutbox.DeliveryID},
		}, item.GeneratedAt)
		if outboxErr != nil {
			return RuntimeSnapshot{}, AgentEvent{}, RuntimeCheckpointDeliveryOutbox{}, outboxErr
		}
		if existing, exists := r.deliveryGroups[deliveryGroup.ID]; exists && !existing.MatchesIdentity(deliveryGroup) {
			return RuntimeSnapshot{}, AgentEvent{}, RuntimeCheckpointDeliveryOutbox{}, ErrConflict
		}
		eventOutbox.GroupID = deliveryGroup.ID
		checkpointOutbox.GroupID = deliveryGroup.ID
	}

	r.snapshots[invocationID] = cloneRuntimeSnapshot(item)
	r.events[invocationID] = append(r.events[invocationID], event)
	r.eventOutbox[eventOutbox.ID] = cloneRuntimeEventOutbox(eventOutbox)
	if checkpointOutbox.ID != "" {
		if r.checkpointOutbox == nil {
			r.checkpointOutbox = make(map[string]RuntimeCheckpointDeliveryOutbox)
		}
		r.checkpointOutbox[checkpointOutbox.ID] = cloneRuntimeCheckpointDeliveryOutbox(checkpointOutbox)
	}
	if deliveryGroup.ID != "" {
		if r.deliveryGroups == nil {
			r.deliveryGroups = make(map[string]RuntimeDeliveryGroup)
		}
		r.deliveryGroups[deliveryGroup.ID] = cloneRuntimeDeliveryGroup(deliveryGroup)
	}
	return cloneRuntimeSnapshot(item), event, cloneRuntimeCheckpointDeliveryOutbox(checkpointOutbox), nil
}

func (r *MemoryRepository) GetToolSetSnapshot(_ context.Context, invocationID string) (ToolSetSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.toolSets[strings.TrimSpace(invocationID)]
	if !ok {
		return ToolSetSnapshot{}, ErrNotFound
	}
	return cloneToolSetSnapshot(item), nil
}

func (r *MemoryRepository) SaveToolSetSnapshot(_ context.Context, item ToolSetSnapshot) (ToolSetSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.invocations[strings.TrimSpace(item.InvocationID)]; !ok {
		return ToolSetSnapshot{}, ErrNotFound
	}
	if err := item.Validate(); err != nil {
		return ToolSetSnapshot{}, err
	}
	current, ok := r.toolSets[item.InvocationID]
	if ok {
		if current.Digest != item.Digest {
			return ToolSetSnapshot{}, ErrToolSnapshotConflict
		}
		return cloneToolSetSnapshot(current), nil
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = nowUTC()
	}
	r.toolSets[item.InvocationID] = cloneToolSetSnapshot(item)
	return cloneToolSetSnapshot(item), nil
}

func (r *MemoryRepository) GetModelCapabilitySnapshot(_ context.Context, invocationID string) (ModelCapabilitySnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.capabilities[strings.TrimSpace(invocationID)]
	if !ok {
		return ModelCapabilitySnapshot{}, ErrNotFound
	}
	return cloneModelCapabilitySnapshot(item), nil
}

func (r *MemoryRepository) SaveModelCapabilitySnapshot(_ context.Context, item ModelCapabilitySnapshot) (ModelCapabilitySnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.invocations[strings.TrimSpace(item.InvocationID)]; !ok {
		return ModelCapabilitySnapshot{}, ErrNotFound
	}
	if err := item.Validate(); err != nil {
		return ModelCapabilitySnapshot{}, err
	}
	current, ok := r.capabilities[item.InvocationID]
	if ok {
		if current.Result.ProfileSnapshotID != item.Result.ProfileSnapshotID || current.Result.RequestPlan != item.Result.RequestPlan {
			return ModelCapabilitySnapshot{}, ErrCapabilitySnapshotConflict
		}
		return cloneModelCapabilitySnapshot(current), nil
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = nowUTC()
	}
	r.capabilities[item.InvocationID] = cloneModelCapabilitySnapshot(item)
	return cloneModelCapabilitySnapshot(item), nil
}

func cloneRuntimeSnapshot(item RuntimeSnapshot) RuntimeSnapshot {
	if item.ActivePlanStep != nil {
		step := *item.ActivePlanStep
		item.ActivePlanStep = &step
	}
	item.Workflow.ActiveBoundaryIDs = append([]string(nil), item.Workflow.ActiveBoundaryIDs...)
	item.Workflow.Branches = append([]WorkflowBranch(nil), item.Workflow.Branches...)
	item.Workflow.Boundaries = append([]WorkflowBoundary(nil), item.Workflow.Boundaries...)
	for index := range item.Workflow.Boundaries {
		item.Workflow.Boundaries[index].WaitIDs = append([]string(nil), item.Workflow.Boundaries[index].WaitIDs...)
		item.Workflow.Boundaries[index].PendingWaitIDs = append([]string(nil), item.Workflow.Boundaries[index].PendingWaitIDs...)
		if item.Workflow.Boundaries[index].ResolvedAt != nil {
			resolvedAt := *item.Workflow.Boundaries[index].ResolvedAt
			item.Workflow.Boundaries[index].ResolvedAt = &resolvedAt
		}
	}
	item.PendingApprovals = append([]ApprovalRef(nil), item.PendingApprovals...)
	item.AppliedChangeSets = append([]ChangeSetRef(nil), item.AppliedChangeSets...)
	item.VerificationRuns = append([]VerificationRef(nil), item.VerificationRuns...)
	item.OpenQuestions = append([]Question(nil), item.OpenQuestions...)
	item.Blockers = append([]Blocker(nil), item.Blockers...)
	item.UnknownStates = append([]UnknownState(nil), item.UnknownStates...)
	return item
}

func cloneToolSetSnapshot(item ToolSetSnapshot) ToolSetSnapshot {
	item.Tools = append([]ToolRef(nil), item.Tools...)
	item.Excluded = append([]ToolSelectionExclusion(nil), item.Excluded...)
	return item
}

func cloneModelCapabilitySnapshot(item ModelCapabilitySnapshot) ModelCapabilitySnapshot {
	item.Result.Profile.ToolChoiceModes = append([]string(nil), item.Result.Profile.ToolChoiceModes...)
	item.Result.Profile.UsageDetails = append([]string(nil), item.Result.Profile.UsageDetails...)
	item.Result.AllowedFeatures = append([]string(nil), item.Result.AllowedFeatures...)
	item.Result.Warnings = append([]string(nil), item.Result.Warnings...)
	item.Result.DisabledFeatures = append([]provider.DisabledFeature(nil), item.Result.DisabledFeatures...)
	return item
}

func (r *MemoryRepository) GetInstructionSnapshotSet(_ context.Context, invocationID string) (InstructionSnapshotSet, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.instructions[strings.TrimSpace(invocationID)]
	if !ok {
		return InstructionSnapshotSet{}, ErrNotFound
	}
	return cloneInstructionSnapshotSet(item), nil
}

func (r *MemoryRepository) ReplaceInstructionSnapshotSet(_ context.Context, item InstructionSnapshotSet) (InstructionSnapshotSet, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.invocations[strings.TrimSpace(item.InvocationID)]; !ok {
		return InstructionSnapshotSet{}, ErrNotFound
	}
	item.InvocationID = strings.TrimSpace(item.InvocationID)
	item.TargetPath = strings.TrimSpace(item.TargetPath)
	current := r.instructions[item.InvocationID]
	if item.Revision <= 0 {
		item.Revision = current.Revision + 1
		if item.Revision == 1 && current.Revision == 0 {
			item.Revision = 1
		}
	} else if current.Revision > 0 && item.Revision <= current.Revision {
		return InstructionSnapshotSet{}, ErrConflict
	}
	if item.UpdatedAt.IsZero() {
		item.UpdatedAt = nowUTC()
	} else {
		item.UpdatedAt = item.UpdatedAt.UTC()
	}
	r.instructions[item.InvocationID] = cloneInstructionSnapshotSet(item)
	return cloneInstructionSnapshotSet(item), nil
}

func cloneInstructionSnapshotSet(item InstructionSnapshotSet) InstructionSnapshotSet {
	item.Snapshots = append([]InstructionSnapshot(nil), item.Snapshots...)
	return item
}

func (r *MemoryRepository) CreateEvalRun(_ context.Context, item EvalRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.invocations[strings.TrimSpace(item.InvocationID)]; !ok {
		return ErrNotFound
	}
	if _, ok := r.evalRuns[item.ID]; ok {
		return ErrConflict
	}
	if err := item.Validate(); err != nil {
		return err
	}
	item.Definition = append([]byte(nil), item.Definition...)
	item.Result = append([]byte(nil), item.Result...)
	r.evalRuns[item.ID] = item
	return nil
}

func (r *MemoryRepository) GetEvalRun(_ context.Context, id string) (EvalRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.evalRuns[strings.TrimSpace(id)]
	if !ok {
		return EvalRun{}, ErrNotFound
	}
	return cloneEvalRun(item), nil
}

func (r *MemoryRepository) ListEvalRuns(_ context.Context, invocationID string) ([]EvalRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	invocationID = strings.TrimSpace(invocationID)
	result := make([]EvalRun, 0)
	for _, item := range r.evalRuns {
		if invocationID != "" && item.InvocationID != invocationID {
			continue
		}
		result = append(result, cloneEvalRun(item))
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result, nil
}

func cloneEvalRun(item EvalRun) EvalRun {
	item.Definition = append([]byte(nil), item.Definition...)
	item.Result = append([]byte(nil), item.Result...)
	if item.FinishedAt != nil {
		finished := *item.FinishedAt
		item.FinishedAt = &finished
	}
	return item
}

func (r *MemoryRepository) GetWorkspaceBaseline(_ context.Context, invocationID string) (WorktreeBaseline, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.baselines[strings.TrimSpace(invocationID)]
	if !ok {
		return WorktreeBaseline{}, ErrNotFound
	}
	return cloneWorktreeBaseline(item), nil
}

// GetWorktreeBaseline is a compatibility spelling for callers that use the
// repository-oriented name from the coding workflow document.
func (r *MemoryRepository) GetWorktreeBaseline(ctx context.Context, invocationID string) (WorktreeBaseline, error) {
	return r.GetWorkspaceBaseline(ctx, invocationID)
}

func (r *MemoryRepository) SaveWorkspaceBaseline(_ context.Context, item WorktreeBaseline) (WorktreeBaseline, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.invocations[strings.TrimSpace(item.InvocationID)]; !ok {
		return WorktreeBaseline{}, ErrNotFound
	}
	normalized, err := item.Normalize(nowUTC())
	if err != nil {
		return WorktreeBaseline{}, err
	}
	if existing, ok := r.baselines[normalized.InvocationID]; ok {
		if !sameWorktreeBaseline(existing, normalized) {
			return WorktreeBaseline{}, ErrConflict
		}
		return cloneWorktreeBaseline(existing), nil
	}
	r.baselines[normalized.InvocationID] = cloneWorktreeBaseline(normalized)
	return cloneWorktreeBaseline(normalized), nil
}

// SaveWorktreeBaseline is a compatibility spelling matching
// GetWorktreeBaseline.
func (r *MemoryRepository) SaveWorktreeBaseline(ctx context.Context, item WorktreeBaseline) (WorktreeBaseline, error) {
	return r.SaveWorkspaceBaseline(ctx, item)
}

func nowUTC() time.Time { return time.Now().UTC() }
