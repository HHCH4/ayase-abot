package runtime

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"Abot/internal/agent"
)

func normalizeRuntimeConfigDeliveryPair(envelope RuntimeConfigDeliveryEnvelope, projection RuntimeConfigDeliveryProjection) (RuntimeConfigDeliveryEnvelope, RuntimeConfigDeliveryProjection, error) {
	normalized, err := NormalizeRuntimeConfigDeliveryEnvelope(envelope)
	if err != nil {
		return RuntimeConfigDeliveryEnvelope{}, RuntimeConfigDeliveryProjection{}, err
	}
	parsed, err := normalizeRuntimeConfigDeliveryProjection(projection)
	if err != nil {
		return RuntimeConfigDeliveryEnvelope{}, RuntimeConfigDeliveryProjection{}, err
	}
	if parsed.Version != normalized.SnapshotVersion || agentRuntimeConfigSnapshotDigest(parsed) != normalized.SnapshotDigest {
		return RuntimeConfigDeliveryEnvelope{}, RuntimeConfigDeliveryProjection{}, fmt.Errorf("%w: projection digest/version 不一致", ErrRuntimeConfigDeliveryAuth)
	}
	return normalized, parsed, nil
}

func cloneRuntimeConfigDeliveryInboxRecord(item RuntimeConfigDeliveryInboxRecord) RuntimeConfigDeliveryInboxRecord {
	item.Projection = cloneRuntimeConfigDeliveryProjection(item.Projection)
	return item
}

// acceptRuntimeConfigDeliveryLocked is the destination monotonic boundary.
// One invocation has one visible target snapshot per source/destination. A
// newer migration is accepted only when its expected digest equals the
// currently visible digest; an old or forked migration is rejected.
func (r *MemoryRepository) acceptRuntimeConfigDeliveryLocked(envelope RuntimeConfigDeliveryEnvelope, projection RuntimeConfigDeliveryProjection, receivedAt time.Time) (bool, error) {
	if r.configDeliveryInbox == nil {
		r.configDeliveryInbox = make(map[string]RuntimeConfigDeliveryInboxRecord)
	}
	if existing, ok := r.configDeliveryInbox[envelope.InvocationID]; ok {
		if existing.Matches(envelope, projection) {
			return true, nil
		}
		if existing.Source != envelope.Source || existing.Destination != envelope.Destination {
			return false, ErrConflict
		}
		if envelope.ExpectedDigest != existing.SnapshotDigest {
			return false, ErrRuntimeConfigDeliveryStale
		}
	}
	r.configDeliveryInbox[envelope.InvocationID] = cloneRuntimeConfigDeliveryInboxRecord(RuntimeConfigDeliveryInboxRecordFromEnvelope(envelope, projection, receivedAt))
	return false, nil
}

// AcceptRuntimeConfigDelivery persists a metadata-only configuration lock.
// The receiver performs HMAC/replay validation; this repository method keeps
// the monotonic and idempotent destination semantics under one mutex.
func (r *MemoryRepository) AcceptRuntimeConfigDelivery(_ context.Context, envelope RuntimeConfigDeliveryEnvelope, projection RuntimeConfigDeliveryProjection) (bool, error) {
	normalized, parsed, err := normalizeRuntimeConfigDeliveryPair(envelope, projection)
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.acceptRuntimeConfigDeliveryLocked(normalized, parsed, nowUTC())
}

func (r *MemoryRepository) GetRuntimeConfigDelivery(_ context.Context, invocationID string) (RuntimeConfigDeliveryInboxRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.configDeliveryInbox[strings.TrimSpace(invocationID)]
	if !ok {
		return RuntimeConfigDeliveryInboxRecord{}, ErrNotFound
	}
	return cloneRuntimeConfigDeliveryInboxRecord(item), nil
}

func (r *MemoryRepository) PrepareRuntimeConfigDelivery(_ context.Context, envelope RuntimeConfigDeliveryEnvelope, projection RuntimeConfigDeliveryProjection) (bool, error) {
	normalized, parsed, err := normalizeRuntimeConfigDeliveryPair(envelope, projection)
	if err != nil {
		return false, err
	}
	now := nowUTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.configDeliveryTxns == nil {
		r.configDeliveryTxns = make(map[string]RuntimeConfigDeliveryTransaction)
	}
	if existing, ok := r.configDeliveryTxns[normalized.DeliveryID]; ok {
		if !existing.Matches(normalized, parsed) {
			return false, ErrConflict
		}
		if existing.Status == RuntimeConfigDeliveryTransactionCommitted {
			return true, nil
		}
		if existing.Status != RuntimeConfigDeliveryTransactionPrepared {
			return false, ErrConflict
		}
		if existing.ExpiresAt.After(now) {
			return true, nil
		}
		// Expired prepares may be refreshed only for the same immutable delivery
		// identity. The new attempt retains the same projection and digest.
	}
	prepared, err := NewRuntimeConfigDeliveryTransaction(normalized, parsed, now)
	if err != nil {
		return false, err
	}
	r.configDeliveryTxns[normalized.DeliveryID] = cloneRuntimeConfigDeliveryTransaction(prepared)
	return false, nil
}

func (r *MemoryRepository) CommitRuntimeConfigDelivery(_ context.Context, envelope RuntimeConfigDeliveryEnvelope) (bool, error) {
	normalized, err := NormalizeRuntimeConfigDeliveryEnvelope(envelope)
	if err != nil {
		return false, err
	}
	now := nowUTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	transaction, ok := r.configDeliveryTxns[normalized.DeliveryID]
	if !ok {
		return false, ErrNotFound
	}
	if !transaction.Matches(normalized, transaction.Projection) {
		return false, ErrConflict
	}
	if transaction.Status == RuntimeConfigDeliveryTransactionCommitted {
		return true, nil
	}
	if transaction.Status != RuntimeConfigDeliveryTransactionPrepared {
		return false, ErrConflict
	}
	if !transaction.ExpiresAt.After(now) {
		return false, ErrRuntimeConfigDeliveryStale
	}
	preparedEnvelope := transaction.Envelope()
	duplicate, err := r.acceptRuntimeConfigDeliveryLocked(preparedEnvelope, transaction.Projection, now)
	if err != nil {
		return false, err
	}
	transaction.Status = RuntimeConfigDeliveryTransactionCommitted
	transaction.UpdatedAt = now
	r.configDeliveryTxns[normalized.DeliveryID] = cloneRuntimeConfigDeliveryTransaction(transaction)
	return duplicate, nil
}

func (r *MemoryRepository) GetRuntimeConfigDeliveryTransaction(_ context.Context, deliveryID string) (RuntimeConfigDeliveryTransaction, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.configDeliveryTxns[strings.TrimSpace(deliveryID)]
	if !ok {
		return RuntimeConfigDeliveryTransaction{}, ErrNotFound
	}
	return cloneRuntimeConfigDeliveryTransaction(item), nil
}

func (r *MemoryRepository) EnqueueRuntimeConfigDeliveryOutbox(_ context.Context, item RuntimeConfigDeliveryOutbox) (RuntimeConfigDeliveryOutbox, error) {
	normalized, err := item.Normalize(nowUTC())
	if err != nil {
		return RuntimeConfigDeliveryOutbox{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.configDeliveryOutbox == nil {
		r.configDeliveryOutbox = make(map[string]RuntimeConfigDeliveryOutbox)
	}
	if existing, ok := r.configDeliveryOutbox[normalized.ID]; ok {
		if !existing.Matches(normalized) {
			return RuntimeConfigDeliveryOutbox{}, ErrConflict
		}
		return cloneRuntimeConfigDeliveryOutbox(existing), nil
	}
	for _, existing := range r.configDeliveryOutbox {
		if existing.InvocationID == normalized.InvocationID && existing.Source == normalized.Source && existing.Destination == normalized.Destination && existing.ExpectedDigest == normalized.ExpectedDigest {
			if existing.SnapshotDigest != normalized.SnapshotDigest || existing.IdempotencyKey != normalized.IdempotencyKey {
				return RuntimeConfigDeliveryOutbox{}, ErrConflict
			}
			return cloneRuntimeConfigDeliveryOutbox(existing), nil
		}
	}
	if normalized.Status != RuntimeConfigDeliveryOutboxQueued || normalized.Attempt != 0 {
		return RuntimeConfigDeliveryOutbox{}, ErrConflict
	}
	r.configDeliveryOutbox[normalized.ID] = cloneRuntimeConfigDeliveryOutbox(normalized)
	return cloneRuntimeConfigDeliveryOutbox(normalized), nil
}

func (r *MemoryRepository) GetRuntimeConfigDeliveryOutbox(_ context.Context, id string) (RuntimeConfigDeliveryOutbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.configDeliveryOutbox[strings.TrimSpace(id)]
	if !ok {
		return RuntimeConfigDeliveryOutbox{}, ErrNotFound
	}
	return cloneRuntimeConfigDeliveryOutbox(item), nil
}

func (r *MemoryRepository) ListRuntimeConfigDeliveryOutbox(_ context.Context, invocationID string, status RuntimeConfigDeliveryOutboxStatus, limit int) ([]RuntimeConfigDeliveryOutbox, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]RuntimeConfigDeliveryOutbox, 0, limit)
	for _, item := range r.configDeliveryOutbox {
		if strings.TrimSpace(invocationID) != "" && item.InvocationID != strings.TrimSpace(invocationID) {
			continue
		}
		if status != "" && item.Status != status {
			continue
		}
		items = append(items, cloneRuntimeConfigDeliveryOutbox(item))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].AvailableAt.Equal(items[j].AvailableAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].AvailableAt.Before(items[j].AvailableAt)
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (r *MemoryRepository) ClaimRuntimeConfigDeliveryOutbox(_ context.Context, owner string, now time.Time, ttl time.Duration) (RuntimeConfigDeliveryOutbox, bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeConfigDeliveryOwnerLength || ttl <= 0 || ttl > MaxRuntimeConfigDeliveryLease {
		return RuntimeConfigDeliveryOutbox{}, false, ErrConflict
	}
	if now.IsZero() {
		now = nowUTC()
	} else {
		now = now.UTC()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]RuntimeConfigDeliveryOutbox, 0, len(r.configDeliveryOutbox))
	for _, item := range r.configDeliveryOutbox {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].AvailableAt.Equal(items[j].AvailableAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].AvailableAt.Before(items[j].AvailableAt)
	})
	for _, candidate := range items {
		item, ok := r.configDeliveryOutbox[candidate.ID]
		if !ok || item.Status.terminal() {
			continue
		}
		if item.Status == RuntimeConfigDeliveryOutboxQueued && item.AvailableAt.After(now) {
			continue
		}
		if item.Status == RuntimeConfigDeliveryOutboxProcessing && item.LeaseExpiresAt != nil && item.LeaseExpiresAt.After(now) {
			continue
		}
		if item.Attempt >= MaxRuntimeConfigDeliveryAttempts {
			item.Status = RuntimeConfigDeliveryOutboxFailed
			item.LeaseOwner = ""
			item.LeaseExpiresAt = nil
			item.LastError = "Runtime config delivery 达到最大投递次数"
			item.Revision++
			item.UpdatedAt = now
			r.configDeliveryOutbox[item.ID] = item
			continue
		}
		expires := now.Add(ttl)
		item.Status = RuntimeConfigDeliveryOutboxProcessing
		item.Attempt++
		item.Revision++
		item.LeaseOwner = owner
		item.LeaseExpiresAt = &expires
		item.UpdatedAt = now
		r.configDeliveryOutbox[item.ID] = item
		return cloneRuntimeConfigDeliveryOutbox(item), true, nil
	}
	return RuntimeConfigDeliveryOutbox{}, false, nil
}

func (r *MemoryRepository) CompleteRuntimeConfigDeliveryOutbox(_ context.Context, id, owner string, now time.Time) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeConfigDeliveryOwnerLength {
		return false, ErrConflict
	}
	if now.IsZero() {
		now = nowUTC()
	} else {
		now = now.UTC()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.configDeliveryOutbox[strings.TrimSpace(id)]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != RuntimeConfigDeliveryOutboxProcessing || item.LeaseOwner != owner || item.LeaseExpiresAt == nil || !item.LeaseExpiresAt.After(now) {
		return false, nil
	}
	item.Status = RuntimeConfigDeliveryOutboxCompleted
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	item.Revision++
	item.UpdatedAt = now
	r.configDeliveryOutbox[item.ID] = item
	return true, nil
}

func (r *MemoryRepository) RetryRuntimeConfigDeliveryOutbox(_ context.Context, id, owner string, now time.Time, message string) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeConfigDeliveryOwnerLength {
		return false, ErrConflict
	}
	if now.IsZero() {
		now = nowUTC()
	} else {
		now = now.UTC()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.configDeliveryOutbox[strings.TrimSpace(id)]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != RuntimeConfigDeliveryOutboxProcessing || item.LeaseOwner != owner || item.LeaseExpiresAt == nil || !item.LeaseExpiresAt.After(now) {
		return false, nil
	}
	item.LastError = SanitizeRuntimeString(strings.TrimSpace(message))
	if len(item.LastError) > 4096 {
		item.LastError = item.LastError[:4096]
	}
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	if item.Attempt >= MaxRuntimeConfigDeliveryAttempts {
		item.Status = RuntimeConfigDeliveryOutboxFailed
	} else {
		item.Status = RuntimeConfigDeliveryOutboxQueued
		item.AvailableAt = now.Add(RuntimeConfigDeliveryOutboxBackoff(item.Attempt))
	}
	item.Revision++
	item.UpdatedAt = now
	r.configDeliveryOutbox[item.ID] = item
	return true, nil
}

var _ RuntimeConfigDeliveryInbox = (*MemoryRepository)(nil)
var _ RuntimeConfigDeliveryTransactionRepository = (*MemoryRepository)(nil)
var _ RuntimeConfigDeliveryOutboxRepository = (*MemoryRepository)(nil)

// CommitRuntimeConfigMigrationWithDelivery keeps the local CAS, audit event,
// event cursor and cross-Runtime config cursor under one memory mutex. It is
// the in-memory equivalent of the SQLite transaction used by the coordinator
// when a remote lock route is enabled.
func (r *MemoryRepository) CommitRuntimeConfigMigrationWithDelivery(_ context.Context, commit RuntimeConfigMigrationCommit, source, destination string) (Invocation, AgentEvent, RuntimeConfigDeliveryOutbox, error) {
	now := nowUTC()
	// A caller-supplied event timestamp is the migration's logical clock.  Use
	// it for the outbox's initial availability as well, otherwise deterministic
	// callers (and replayed commits) can create a cursor that is not due yet.
	if !commit.Event.Timestamp.IsZero() {
		now = commit.Event.Timestamp.UTC()
	}
	source = strings.TrimSpace(source)
	destination = strings.TrimSpace(destination)
	if source == "" || destination == "" {
		return Invocation{}, AgentEvent{}, RuntimeConfigDeliveryOutbox{}, fmt.Errorf("%w: config delivery source/destination 不完整", ErrInvalidRuntimeConfigDelivery)
	}
	normalized, newSnapshot, newDigest, err := normalizeRuntimeConfigMigrationCommit(commit, now)
	if err != nil {
		return Invocation{}, AgentEvent{}, RuntimeConfigDeliveryOutbox{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	invocation, ok := r.invocations[normalized.InvocationID]
	if !ok {
		return Invocation{}, AgentEvent{}, RuntimeConfigDeliveryOutbox{}, ErrNotFound
	}
	if !runtimeConfigMigrationWaitingStatus(invocation.Status) {
		return Invocation{}, AgentEvent{}, RuntimeConfigDeliveryOutbox{}, ErrConflict
	}
	storedDigest := agentRuntimeConfigSnapshotDigestString(invocation.ConfigSnapshot)
	delivery, err := NewRuntimeConfigDeliveryOutbox(source, destination, invocation.ID, normalized.ExpectedDigest, normalized.IdempotencyKey, newSnapshot, now)
	if err != nil {
		return Invocation{}, AgentEvent{}, RuntimeConfigDeliveryOutbox{}, err
	}
	if existingEvent, exists := r.eventByIDLocked(normalized.Event.ID); exists {
		if existingEvent.InvocationID != normalized.InvocationID || existingEvent.Type != EventRuntimeConfigMigrated || !RuntimeConfigMigrationEventMatches(existingEvent, normalized.ExpectedDigest, newDigest, normalized.IdempotencyKey) || storedDigest != newDigest {
			return Invocation{}, AgentEvent{}, RuntimeConfigDeliveryOutbox{}, ErrConflict
		}
		existingOutbox, outboxExists := r.configDeliveryOutbox[delivery.ID]
		if !outboxExists || !existingOutbox.Matches(delivery) {
			return Invocation{}, AgentEvent{}, RuntimeConfigDeliveryOutbox{}, ErrConflict
		}
		existingEventOutbox, eventOutboxErr := NewRuntimeEventOutbox(existingEvent)
		if eventOutboxErr != nil {
			return Invocation{}, AgentEvent{}, RuntimeConfigDeliveryOutbox{}, eventOutboxErr
		}
		group, groupErr := NewRuntimeDeliveryGroup(source, destination, invocation.ID, []RuntimeDeliveryGroupMember{
			{Kind: RuntimeDeliveryKindEvent, OutboxID: existingEventOutbox.ID, DeliveryID: existingEvent.ID},
			{Kind: RuntimeDeliveryKindConfig, OutboxID: existingOutbox.ID, DeliveryID: existingOutbox.DeliveryID},
		}, existingEvent.Timestamp)
		if groupErr != nil {
			return Invocation{}, AgentEvent{}, RuntimeConfigDeliveryOutbox{}, groupErr
		}
		if _, groupErr = r.enqueueRuntimeDeliveryGroupLocked(group, now); groupErr != nil {
			return Invocation{}, AgentEvent{}, RuntimeConfigDeliveryOutbox{}, groupErr
		}
		existingOutbox.GroupID = group.ID
		r.configDeliveryOutbox[existingOutbox.ID] = cloneRuntimeConfigDeliveryOutbox(existingOutbox)
		if storedEventOutbox, ok := r.eventOutbox[existingEventOutbox.ID]; ok {
			storedEventOutbox.GroupID = group.ID
			r.eventOutbox[storedEventOutbox.ID] = cloneRuntimeEventOutbox(storedEventOutbox)
		}
		return invocation, cloneAgentEvent(existingEvent), cloneRuntimeConfigDeliveryOutbox(existingOutbox), nil
	}
	if storedDigest == "" || storedDigest != normalized.ExpectedDigest || newDigest == storedDigest {
		return Invocation{}, AgentEvent{}, RuntimeConfigDeliveryOutbox{}, ErrConflict
	}
	previousSnapshot, _, _, previousErr := normalizeRuntimeConfigMigrationSnapshot(invocation.ConfigSnapshot)
	if previousErr != nil {
		return Invocation{}, AgentEvent{}, RuntimeConfigDeliveryOutbox{}, ErrConflict
	}
	data, dataErr := runtimeConfigMigrationEventData(invocation.ID, storedDigest, newDigest, normalized.IdempotencyKey, previousSnapshot, newSnapshot)
	if dataErr != nil {
		return Invocation{}, AgentEvent{}, RuntimeConfigDeliveryOutbox{}, dataErr
	}
	event := normalized.Event
	event.InvocationID = invocation.ID
	event.Type = EventRuntimeConfigMigrated
	event.Data = data
	event.Sequence = int64(len(r.events[invocation.ID]) + 1)
	eventOutbox, outboxErr := NewRuntimeEventOutbox(event)
	if outboxErr != nil {
		return Invocation{}, AgentEvent{}, RuntimeConfigDeliveryOutbox{}, outboxErr
	}
	if existing, exists := r.configDeliveryOutbox[delivery.ID]; exists && !existing.Matches(delivery) {
		return Invocation{}, AgentEvent{}, RuntimeConfigDeliveryOutbox{}, ErrConflict
	}
	for _, existing := range r.configDeliveryOutbox {
		if existing.InvocationID == delivery.InvocationID && existing.Source == delivery.Source && existing.Destination == delivery.Destination && existing.ExpectedDigest == delivery.ExpectedDigest && !existing.Matches(delivery) {
			return Invocation{}, AgentEvent{}, RuntimeConfigDeliveryOutbox{}, ErrConflict
		}
	}
	deliveryGroup, groupErr := NewRuntimeDeliveryGroup(source, destination, invocation.ID, []RuntimeDeliveryGroupMember{
		{Kind: RuntimeDeliveryKindEvent, OutboxID: eventOutbox.ID, DeliveryID: event.ID},
		{Kind: RuntimeDeliveryKindConfig, OutboxID: delivery.ID, DeliveryID: delivery.DeliveryID},
	}, now)
	if groupErr != nil {
		return Invocation{}, AgentEvent{}, RuntimeConfigDeliveryOutbox{}, groupErr
	}
	if existing, exists := r.deliveryGroups[deliveryGroup.ID]; exists && !existing.MatchesIdentity(deliveryGroup) {
		return Invocation{}, AgentEvent{}, RuntimeConfigDeliveryOutbox{}, ErrConflict
	}
	eventOutbox.GroupID = deliveryGroup.ID
	delivery.GroupID = deliveryGroup.ID
	invocation.ConfigSnapshot = normalized.NewSnapshot
	invocation.ConfigSnapshotDigest = newDigest
	invocation.UpdatedAt = now
	r.invocations[invocation.ID] = invocation
	r.events[invocation.ID] = append(r.events[invocation.ID], cloneAgentEvent(event))
	r.eventOutbox[eventOutbox.ID] = cloneRuntimeEventOutbox(eventOutbox)
	if r.configDeliveryOutbox == nil {
		r.configDeliveryOutbox = make(map[string]RuntimeConfigDeliveryOutbox)
	}
	r.configDeliveryOutbox[delivery.ID] = cloneRuntimeConfigDeliveryOutbox(delivery)
	if r.deliveryGroups == nil {
		r.deliveryGroups = make(map[string]RuntimeDeliveryGroup)
	}
	r.deliveryGroups[deliveryGroup.ID] = cloneRuntimeDeliveryGroup(deliveryGroup)
	return invocation, cloneAgentEvent(event), cloneRuntimeConfigDeliveryOutbox(delivery), nil
}

func agentRuntimeConfigSnapshotDigestString(encoded string) string {
	return agent.RuntimeConfigSnapshotDigest(encoded)
}
