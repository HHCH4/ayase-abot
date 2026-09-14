package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// RuntimeDeliveryGroup is the durable, source-side coordination record for
// several delivery cursors created by one local business commit.  It is a
// 2PC-style local ledger: it makes membership and progress durable, but it
// does not claim that two independent destination services commit atomically.
type RuntimeDeliveryGroup struct {
	ID           string `json:"id"`
	InvocationID string `json:"invocation_id"`
	Source       string `json:"source"`
	Destination  string `json:"destination"`
	// FenceID/FenceRevision are optional until the destination explicitly
	// enables fencing.  They are not part of the stable group ID so legacy
	// groups remain readable, but once present they become immutable group
	// identity and are copied into the signed settlement envelope.
	FenceID        string                       `json:"fence_id,omitempty"`
	FenceRevision  int64                        `json:"fence_revision,omitempty"`
	Members        []RuntimeDeliveryGroupMember `json:"members"`
	Status         RuntimeDeliveryGroupStatus   `json:"status"`
	Attempt        int                          `json:"attempt"`
	Revision       int64                        `json:"revision"`
	AvailableAt    time.Time                    `json:"available_at"`
	LeaseOwner     string                       `json:"-"`
	LeaseExpiresAt *time.Time                   `json:"-"`
	LastError      string                       `json:"last_error,omitempty"`
	CreatedAt      time.Time                    `json:"created_at"`
	UpdatedAt      time.Time                    `json:"updated_at"`
	CompletedAt    *time.Time                   `json:"completed_at,omitempty"`
}

// RuntimeDeliveryGroupMember is intentionally metadata-only.  Payloads stay
// in their family-specific outbox and are never copied into the group row.
type RuntimeDeliveryGroupMember struct {
	Kind       RuntimeDeliveryKind              `json:"kind"`
	OutboxID   string                           `json:"outbox_id"`
	DeliveryID string                           `json:"delivery_id"`
	Status     RuntimeDeliveryGroupMemberStatus `json:"status"`
	LastError  string                           `json:"last_error,omitempty"`
	UpdatedAt  time.Time                        `json:"updated_at"`
}

type RuntimeDeliveryGroupStatus string

const (
	RuntimeDeliveryGroupQueued     RuntimeDeliveryGroupStatus = "queued"
	RuntimeDeliveryGroupProcessing RuntimeDeliveryGroupStatus = "processing"
	RuntimeDeliveryGroupCompleted  RuntimeDeliveryGroupStatus = "completed"
	RuntimeDeliveryGroupFailed     RuntimeDeliveryGroupStatus = "failed"
)

type RuntimeDeliveryGroupMemberStatus string

const (
	RuntimeDeliveryGroupMemberPending   RuntimeDeliveryGroupMemberStatus = "pending"
	RuntimeDeliveryGroupMemberCompleted RuntimeDeliveryGroupMemberStatus = "completed"
	RuntimeDeliveryGroupMemberFailed    RuntimeDeliveryGroupMemberStatus = "failed"
)

const (
	MaxRuntimeDeliveryGroupIDLength         = 220
	MaxRuntimeDeliveryGroupInvocationLength = 512
	// A group is deliberately small: a local commit may include several event
	// cursors plus one destination intent, but it must never become an
	// unbounded batch that can starve the regular outbox scheduler.
	MaxRuntimeDeliveryGroupMemberCount       = 16
	MaxRuntimeDeliveryGroupMemberIDLength    = 512
	MaxRuntimeDeliveryGroupMemberErrorLength = 4096
	MaxRuntimeDeliveryGroupOwnerLength       = 160
	MaxRuntimeDeliveryGroupErrorLength       = 4096
	MaxRuntimeDeliveryGroupAttempts          = 8
	MaxRuntimeDeliveryGroupLease             = 10 * time.Minute
)

var (
	ErrInvalidRuntimeDeliveryGroup     = errors.New("Runtime delivery group 无效")
	ErrRuntimeDeliveryGroupUnavailable = errors.New("Runtime delivery group 未配置")
)

func validRuntimeDeliveryGroupStatus(status RuntimeDeliveryGroupStatus) bool {
	switch status {
	case RuntimeDeliveryGroupQueued, RuntimeDeliveryGroupProcessing, RuntimeDeliveryGroupCompleted, RuntimeDeliveryGroupFailed:
		return true
	default:
		return false
	}
}

func validRuntimeDeliveryGroupMemberStatus(status RuntimeDeliveryGroupMemberStatus) bool {
	switch status {
	case RuntimeDeliveryGroupMemberPending, RuntimeDeliveryGroupMemberCompleted, RuntimeDeliveryGroupMemberFailed:
		return true
	default:
		return false
	}
}

func runtimeDeliveryGroupMemberIdentity(member RuntimeDeliveryGroupMember) string {
	return strings.Join([]string{string(member.Kind), strings.TrimSpace(member.OutboxID), strings.TrimSpace(member.DeliveryID)}, "\x00")
}

// RuntimeDeliveryGroupID is stable for one route, invocation and set of
// immutable outbox identities.  Member order does not affect the result.
func RuntimeDeliveryGroupID(source, destination, invocationID string, members []RuntimeDeliveryGroupMember) string {
	identities := make([]string, 0, len(members))
	for _, member := range members {
		identities = append(identities, runtimeDeliveryGroupMemberIdentity(member))
	}
	sort.Strings(identities)
	material := strings.Join([]string{strings.TrimSpace(source), strings.TrimSpace(destination), strings.TrimSpace(invocationID), strings.Join(identities, "\x01")}, "\x00")
	sum := sha256.Sum256([]byte(material))
	return "runtime-delivery-group-" + hex.EncodeToString(sum[:16])
}

func cloneRuntimeDeliveryGroupMember(member RuntimeDeliveryGroupMember) RuntimeDeliveryGroupMember {
	return member
}

func cloneRuntimeDeliveryGroup(item RuntimeDeliveryGroup) RuntimeDeliveryGroup {
	item.Members = append([]RuntimeDeliveryGroupMember(nil), item.Members...)
	if item.LeaseExpiresAt != nil {
		value := item.LeaseExpiresAt.UTC()
		item.LeaseExpiresAt = &value
	}
	if item.CompletedAt != nil {
		value := item.CompletedAt.UTC()
		item.CompletedAt = &value
	}
	return item
}

func (m RuntimeDeliveryGroupMember) normalize(now time.Time) (RuntimeDeliveryGroupMember, error) {
	m.Kind = RuntimeDeliveryKind(strings.TrimSpace(string(m.Kind)))
	m.OutboxID = strings.TrimSpace(m.OutboxID)
	m.DeliveryID = strings.TrimSpace(m.DeliveryID)
	m.LastError = SanitizeRuntimeString(strings.TrimSpace(m.LastError))
	if !validRuntimeDeliveryKind(m.Kind) || m.OutboxID == "" || m.DeliveryID == "" {
		return RuntimeDeliveryGroupMember{}, fmt.Errorf("%w: member metadata 不完整", ErrInvalidRuntimeDeliveryGroup)
	}
	if len(m.OutboxID) > MaxRuntimeDeliveryGroupMemberIDLength || len(m.DeliveryID) > MaxRuntimeDeliveryGroupMemberIDLength {
		return RuntimeDeliveryGroupMember{}, fmt.Errorf("%w: member identity 超出长度限制", ErrInvalidRuntimeDeliveryGroup)
	}
	if m.Status == "" {
		m.Status = RuntimeDeliveryGroupMemberPending
	}
	if !validRuntimeDeliveryGroupMemberStatus(m.Status) {
		return RuntimeDeliveryGroupMember{}, fmt.Errorf("%w: member status %q 不受支持", ErrInvalidRuntimeDeliveryGroup, m.Status)
	}
	if len(m.LastError) > MaxRuntimeDeliveryGroupMemberErrorLength {
		m.LastError = m.LastError[:MaxRuntimeDeliveryGroupMemberErrorLength]
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = now
	} else {
		m.UpdatedAt = m.UpdatedAt.UTC()
	}
	return m, nil
}

func (g RuntimeDeliveryGroup) normalize(now time.Time) (RuntimeDeliveryGroup, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	g.ID = strings.TrimSpace(g.ID)
	g.InvocationID = strings.TrimSpace(g.InvocationID)
	g.Source = strings.TrimSpace(g.Source)
	g.Destination = strings.TrimSpace(g.Destination)
	g.FenceID = strings.TrimSpace(g.FenceID)
	g.LeaseOwner = strings.TrimSpace(g.LeaseOwner)
	g.LastError = SanitizeRuntimeString(strings.TrimSpace(g.LastError))
	if g.ID == "" || g.InvocationID == "" || g.Source == "" || g.Destination == "" {
		return RuntimeDeliveryGroup{}, fmt.Errorf("%w: group metadata 不完整", ErrInvalidRuntimeDeliveryGroup)
	}
	if len(g.ID) > MaxRuntimeDeliveryGroupIDLength || len(g.InvocationID) > MaxRuntimeDeliveryGroupInvocationLength || len(g.Source) > MaxRuntimeDeliveryAttemptSourceLength || len(g.Destination) > MaxRuntimeDeliveryAttemptDestinationLength || len(g.LeaseOwner) > MaxRuntimeDeliveryGroupOwnerLength {
		return RuntimeDeliveryGroup{}, fmt.Errorf("%w: group metadata 超出长度限制", ErrInvalidRuntimeDeliveryGroup)
	}
	if (g.FenceID == "") != (g.FenceRevision == 0) || g.FenceRevision < 0 {
		return RuntimeDeliveryGroup{}, fmt.Errorf("%w: fence identity/revision 必须成对且有效", ErrInvalidRuntimeDeliveryGroup)
	}
	if g.FenceID != "" {
		if len(g.FenceID) > MaxRuntimeDeliveryFenceIDLength || g.FenceRevision < 1 || g.FenceRevision > MaxRuntimeDeliveryFenceRevision {
			return RuntimeDeliveryGroup{}, fmt.Errorf("%w: fence 超出范围", ErrInvalidRuntimeDeliveryGroup)
		}
		if g.FenceID != RuntimeDeliveryGroupFenceID(g.Source, g.Destination, g.InvocationID) {
			return RuntimeDeliveryGroup{}, fmt.Errorf("%w: fence_id 不是稳定派生值", ErrInvalidRuntimeDeliveryGroup)
		}
	}
	if len(g.Members) == 0 || len(g.Members) > MaxRuntimeDeliveryGroupMemberCount {
		return RuntimeDeliveryGroup{}, fmt.Errorf("%w: member 数量必须在 1-%d", ErrInvalidRuntimeDeliveryGroup, MaxRuntimeDeliveryGroupMemberCount)
	}
	normalizedMembers := make([]RuntimeDeliveryGroupMember, 0, len(g.Members))
	seen := make(map[string]struct{}, len(g.Members))
	for _, member := range g.Members {
		normalized, err := member.normalize(now)
		if err != nil {
			return RuntimeDeliveryGroup{}, err
		}
		identity := runtimeDeliveryGroupMemberIdentity(normalized)
		if _, exists := seen[identity]; exists {
			return RuntimeDeliveryGroup{}, fmt.Errorf("%w: member identity 重复", ErrInvalidRuntimeDeliveryGroup)
		}
		seen[identity] = struct{}{}
		normalizedMembers = append(normalizedMembers, normalized)
	}
	sort.Slice(normalizedMembers, func(i, j int) bool {
		return runtimeDeliveryGroupMemberIdentity(normalizedMembers[i]) < runtimeDeliveryGroupMemberIdentity(normalizedMembers[j])
	})
	g.Members = normalizedMembers
	if g.ID != RuntimeDeliveryGroupID(g.Source, g.Destination, g.InvocationID, g.Members) {
		return RuntimeDeliveryGroup{}, fmt.Errorf("%w: group ID 不是稳定派生值", ErrInvalidRuntimeDeliveryGroup)
	}
	if g.Status == "" {
		g.Status = RuntimeDeliveryGroupQueued
	}
	if !validRuntimeDeliveryGroupStatus(g.Status) {
		return RuntimeDeliveryGroup{}, fmt.Errorf("%w: group status %q 不受支持", ErrInvalidRuntimeDeliveryGroup, g.Status)
	}
	if g.Attempt < 0 || g.Attempt > MaxRuntimeDeliveryGroupAttempts || g.Revision < 0 {
		return RuntimeDeliveryGroup{}, fmt.Errorf("%w: group attempt/revision 无效", ErrInvalidRuntimeDeliveryGroup)
	}
	if len(g.LastError) > MaxRuntimeDeliveryGroupErrorLength {
		g.LastError = g.LastError[:MaxRuntimeDeliveryGroupErrorLength]
	}
	if g.AvailableAt.IsZero() {
		g.AvailableAt = now
	} else {
		g.AvailableAt = g.AvailableAt.UTC()
	}
	if g.CreatedAt.IsZero() {
		g.CreatedAt = now
	} else {
		g.CreatedAt = g.CreatedAt.UTC()
	}
	if g.UpdatedAt.IsZero() {
		g.UpdatedAt = now
	} else {
		g.UpdatedAt = g.UpdatedAt.UTC()
	}
	if g.Revision <= 0 {
		g.Revision = 1
	}
	if g.LeaseExpiresAt != nil {
		expires := g.LeaseExpiresAt.UTC()
		g.LeaseExpiresAt = &expires
	}
	if g.CompletedAt != nil {
		completed := g.CompletedAt.UTC()
		g.CompletedAt = &completed
	}
	switch g.Status {
	case RuntimeDeliveryGroupQueued, RuntimeDeliveryGroupCompleted, RuntimeDeliveryGroupFailed:
		g.LeaseOwner = ""
		g.LeaseExpiresAt = nil
	case RuntimeDeliveryGroupProcessing:
		if g.LeaseOwner == "" || g.LeaseExpiresAt == nil || g.LeaseExpiresAt.IsZero() {
			return RuntimeDeliveryGroup{}, fmt.Errorf("%w: processing group 必须带租约", ErrInvalidRuntimeDeliveryGroup)
		}
		if g.LeaseExpiresAt.After(now.Add(MaxRuntimeDeliveryGroupLease)) {
			return RuntimeDeliveryGroup{}, fmt.Errorf("%w: group lease 超出时长限制", ErrInvalidRuntimeDeliveryGroup)
		}
	}
	if g.Status == RuntimeDeliveryGroupCompleted {
		for _, member := range g.Members {
			if member.Status != RuntimeDeliveryGroupMemberCompleted {
				return RuntimeDeliveryGroup{}, fmt.Errorf("%w: completed group 仍有未完成 member", ErrInvalidRuntimeDeliveryGroup)
			}
		}
		if g.CompletedAt == nil {
			completed := g.UpdatedAt
			g.CompletedAt = &completed
		}
	} else if g.CompletedAt != nil {
		return RuntimeDeliveryGroup{}, fmt.Errorf("%w: 非 completed group 不能带 completed_at", ErrInvalidRuntimeDeliveryGroup)
	}
	return g, nil
}

func (g RuntimeDeliveryGroup) Normalize(now time.Time) (RuntimeDeliveryGroup, error) {
	return g.normalize(now)
}

func NewRuntimeDeliveryGroup(source, destination, invocationID string, members []RuntimeDeliveryGroupMember, now time.Time) (RuntimeDeliveryGroup, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	item := RuntimeDeliveryGroup{
		Source: strings.TrimSpace(source), Destination: strings.TrimSpace(destination), InvocationID: strings.TrimSpace(invocationID),
		Members: append([]RuntimeDeliveryGroupMember(nil), members...), Status: RuntimeDeliveryGroupQueued,
		AvailableAt: now, CreatedAt: now, UpdatedAt: now, Revision: 1,
	}
	item.ID = RuntimeDeliveryGroupID(item.Source, item.Destination, item.InvocationID, item.Members)
	return item.normalize(now)
}

func (g RuntimeDeliveryGroup) MatchesIdentity(other RuntimeDeliveryGroup) bool {
	left, leftErr := g.normalize(time.Time{})
	right, rightErr := other.normalize(time.Time{})
	if leftErr != nil || rightErr != nil {
		return false
	}
	if left.ID != right.ID || left.InvocationID != right.InvocationID || left.Source != right.Source || left.Destination != right.Destination || len(left.Members) != len(right.Members) {
		return false
	}
	// Fence binding is an additive source-side safety upgrade. A legacy retry
	// that omits the optional fields must still observe the same group; two
	// explicitly different fence bindings, however, are a conflict.
	if left.FenceID != "" && right.FenceID != "" && (left.FenceID != right.FenceID || left.FenceRevision != right.FenceRevision) {
		return false
	}
	for index := range left.Members {
		if runtimeDeliveryGroupMemberIdentity(left.Members[index]) != runtimeDeliveryGroupMemberIdentity(right.Members[index]) {
			return false
		}
	}
	return true
}

func RuntimeDeliveryGroupBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

func normalizeRuntimeDeliveryGroupTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}

func runtimeDeliveryGroupMembersCompleted(members []RuntimeDeliveryGroupMember) (allCompleted, anyFailed bool) {
	allCompleted = len(members) > 0
	for _, member := range members {
		switch member.Status {
		case RuntimeDeliveryGroupMemberCompleted:
		case RuntimeDeliveryGroupMemberFailed:
			anyFailed = true
			allCompleted = false
		default:
			allCompleted = false
		}
	}
	return allCompleted, anyFailed
}

// repairRuntimeDeliveryGroupAggregate closes the small crash window between
// persisting a member's terminal state and persisting the aggregate status.
// The transition is metadata-only and therefore must not consume a group
// delivery attempt. It is shared by both targeted and next-due claims so a
// legacy consumer cannot accidentally turn an already-finished group into a
// fresh processing attempt.
func repairRuntimeDeliveryGroupAggregate(item *RuntimeDeliveryGroup, now time.Time) bool {
	if item == nil || item.Status == RuntimeDeliveryGroupCompleted || item.Status == RuntimeDeliveryGroupFailed {
		return false
	}
	allCompleted, anyFailed := runtimeDeliveryGroupMembersCompleted(item.Members)
	if !anyFailed && !allCompleted {
		return false
	}
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	item.CompletedAt = nil
	if anyFailed {
		item.Status = RuntimeDeliveryGroupFailed
		if item.LastError == "" {
			for _, member := range item.Members {
				if member.Status == RuntimeDeliveryGroupMemberFailed {
					item.LastError = member.LastError
					break
				}
			}
		}
	} else {
		item.Status = RuntimeDeliveryGroupCompleted
		finished := now
		item.CompletedAt = &finished
	}
	item.Revision++
	item.UpdatedAt = now
	return true
}

// enqueueRuntimeDeliveryGroupLocked is shared by local atomic commits and
// the public Memory enqueue path. The caller owns r.mu.
func (r *MemoryRepository) enqueueRuntimeDeliveryGroupLocked(item RuntimeDeliveryGroup, now time.Time) (RuntimeDeliveryGroup, error) {
	normalized, err := item.normalize(now)
	if err != nil {
		return RuntimeDeliveryGroup{}, err
	}
	if r.deliveryGroups == nil {
		r.deliveryGroups = make(map[string]RuntimeDeliveryGroup)
	}
	if existing, ok := r.deliveryGroups[normalized.ID]; ok {
		if !existing.MatchesIdentity(normalized) {
			return RuntimeDeliveryGroup{}, ErrConflict
		}
		return cloneRuntimeDeliveryGroup(existing), nil
	}
	r.deliveryGroups[normalized.ID] = cloneRuntimeDeliveryGroup(normalized)
	return cloneRuntimeDeliveryGroup(normalized), nil
}

func (r *MemoryRepository) EnqueueRuntimeDeliveryGroup(_ context.Context, item RuntimeDeliveryGroup) (RuntimeDeliveryGroup, error) {
	now := normalizeRuntimeDeliveryGroupTime(time.Time{})
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enqueueRuntimeDeliveryGroupLocked(item, now)
}

func (r *MemoryRepository) GetRuntimeDeliveryGroup(_ context.Context, id string) (RuntimeDeliveryGroup, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.deliveryGroups[strings.TrimSpace(id)]
	if !ok {
		return RuntimeDeliveryGroup{}, ErrNotFound
	}
	return cloneRuntimeDeliveryGroup(item), nil
}

func (r *MemoryRepository) ListRuntimeDeliveryGroups(_ context.Context, invocationID string, status RuntimeDeliveryGroupStatus, limit int) ([]RuntimeDeliveryGroup, error) {
	invocationID = strings.TrimSpace(invocationID)
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if invocationID != "" {
		if _, ok := r.invocations[invocationID]; !ok {
			return nil, ErrNotFound
		}
	}
	items := make([]RuntimeDeliveryGroup, 0, len(r.deliveryGroups))
	for _, item := range r.deliveryGroups {
		if invocationID != "" && item.InvocationID != invocationID {
			continue
		}
		if status != "" && item.Status != status {
			continue
		}
		items = append(items, cloneRuntimeDeliveryGroup(item))
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

func (r *MemoryRepository) ClaimRuntimeDeliveryGroup(_ context.Context, owner string, now time.Time, ttl time.Duration) (RuntimeDeliveryGroup, bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeDeliveryGroupOwnerLength || ttl <= 0 || ttl > MaxRuntimeDeliveryGroupLease {
		return RuntimeDeliveryGroup{}, false, ErrConflict
	}
	now = normalizeRuntimeDeliveryGroupTime(now)
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]RuntimeDeliveryGroup, 0, len(r.deliveryGroups))
	for _, item := range r.deliveryGroups {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].AvailableAt.Equal(items[j].AvailableAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].AvailableAt.Before(items[j].AvailableAt)
	})
	for _, candidate := range items {
		item, ok := r.deliveryGroups[candidate.ID]
		if !ok || item.Status == RuntimeDeliveryGroupCompleted || item.Status == RuntimeDeliveryGroupFailed {
			continue
		}
		if repairRuntimeDeliveryGroupAggregate(&item, now) {
			r.deliveryGroups[item.ID] = item
			continue
		}
		if item.Status == RuntimeDeliveryGroupQueued && item.AvailableAt.After(now) {
			continue
		}
		if item.Status == RuntimeDeliveryGroupProcessing && item.LeaseExpiresAt != nil && item.LeaseExpiresAt.After(now) {
			continue
		}
		if item.Attempt >= MaxRuntimeDeliveryGroupAttempts {
			item.Status = RuntimeDeliveryGroupFailed
			item.LastError = "Runtime delivery group 达到最大投递次数"
			item.LeaseOwner = ""
			item.LeaseExpiresAt = nil
			item.Revision++
			item.UpdatedAt = now
			r.deliveryGroups[item.ID] = item
			continue
		}
		expires := now.Add(ttl)
		item.Status = RuntimeDeliveryGroupProcessing
		item.Attempt++
		item.Revision++
		item.LeaseOwner = owner
		item.LeaseExpiresAt = &expires
		item.UpdatedAt = now
		r.deliveryGroups[item.ID] = item
		return cloneRuntimeDeliveryGroup(item), true, nil
	}
	return RuntimeDeliveryGroup{}, false, nil
}

// ClaimRuntimeDeliveryGroupByID is the targeted counterpart to the bounded
// "next due" claim.  It lets the coordinator reconcile a group it has just
// inspected without taking a lease on an unrelated group that happened to be
// earlier in the queue.
func (r *MemoryRepository) ClaimRuntimeDeliveryGroupByID(_ context.Context, id, owner string, now time.Time, ttl time.Duration) (RuntimeDeliveryGroup, bool, error) {
	id = strings.TrimSpace(id)
	owner = strings.TrimSpace(owner)
	if id == "" || owner == "" || len(owner) > MaxRuntimeDeliveryGroupOwnerLength || ttl <= 0 || ttl > MaxRuntimeDeliveryGroupLease {
		return RuntimeDeliveryGroup{}, false, ErrConflict
	}
	now = normalizeRuntimeDeliveryGroupTime(now)
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.deliveryGroups[id]
	if !ok {
		return RuntimeDeliveryGroup{}, false, ErrNotFound
	}
	if item.Status == RuntimeDeliveryGroupCompleted || item.Status == RuntimeDeliveryGroupFailed {
		return cloneRuntimeDeliveryGroup(item), false, nil
	}
	// Repair a crash/legacy window where every member is already terminal but
	// the aggregate status was not persisted yet. This is a metadata-only
	// transition and intentionally does not consume a group attempt.
	if repairRuntimeDeliveryGroupAggregate(&item, now) {
		r.deliveryGroups[id] = item
		return cloneRuntimeDeliveryGroup(item), false, nil
	}
	if item.Status == RuntimeDeliveryGroupQueued && item.AvailableAt.After(now) {
		return cloneRuntimeDeliveryGroup(item), false, nil
	}
	if item.Status == RuntimeDeliveryGroupProcessing && item.LeaseExpiresAt != nil && item.LeaseExpiresAt.After(now) {
		return cloneRuntimeDeliveryGroup(item), false, nil
	}
	if item.Attempt >= MaxRuntimeDeliveryGroupAttempts {
		item.Status = RuntimeDeliveryGroupFailed
		item.LastError = "Runtime delivery group 达到最大投递次数"
		item.LeaseOwner = ""
		item.LeaseExpiresAt = nil
		item.CompletedAt = nil
		item.Revision++
		item.UpdatedAt = now
		r.deliveryGroups[id] = item
		return cloneRuntimeDeliveryGroup(item), false, nil
	}
	expires := now.Add(ttl)
	item.Status = RuntimeDeliveryGroupProcessing
	item.Attempt++
	item.Revision++
	item.LeaseOwner = owner
	item.LeaseExpiresAt = &expires
	item.CompletedAt = nil
	item.UpdatedAt = now
	r.deliveryGroups[id] = item
	return cloneRuntimeDeliveryGroup(item), true, nil
}

// DeferRuntimeDeliveryGroup releases a coordination lease while preserving
// the group retry budget.  Delivery attempts remain owned by the family
// outboxes; this path only waits for their next terminal state.
func (r *MemoryRepository) DeferRuntimeDeliveryGroup(_ context.Context, id, owner string, now, availableAt time.Time, message string) (bool, error) {
	id = strings.TrimSpace(id)
	owner = strings.TrimSpace(owner)
	if id == "" || owner == "" || len(owner) > MaxRuntimeDeliveryGroupOwnerLength {
		return false, ErrConflict
	}
	now = normalizeRuntimeDeliveryGroupTime(now)
	if availableAt.IsZero() {
		availableAt = now
	} else {
		availableAt = availableAt.UTC()
	}
	if availableAt.Before(now) {
		availableAt = now
	}
	if availableAt.After(now.Add(MaxRuntimeDeliveryGroupLease)) {
		return false, ErrConflict
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.deliveryGroups[id]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != RuntimeDeliveryGroupProcessing || item.LeaseOwner != owner {
		return false, nil
	}
	if item.LeaseExpiresAt == nil || !item.LeaseExpiresAt.After(now) {
		return false, nil
	}
	item.Status = RuntimeDeliveryGroupQueued
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	item.CompletedAt = nil
	if item.Attempt > 0 {
		item.Attempt--
	}
	item.AvailableAt = availableAt
	item.LastError = SanitizeRuntimeString(strings.TrimSpace(message))
	if len(item.LastError) > MaxRuntimeDeliveryGroupErrorLength {
		item.LastError = item.LastError[:MaxRuntimeDeliveryGroupErrorLength]
	}
	item.Revision++
	item.UpdatedAt = now
	r.deliveryGroups[id] = item
	return true, nil
}

func (r *MemoryRepository) CompleteRuntimeDeliveryGroup(_ context.Context, id, owner string, now time.Time) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeDeliveryGroupOwnerLength {
		return false, ErrConflict
	}
	now = normalizeRuntimeDeliveryGroupTime(now)
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.deliveryGroups[strings.TrimSpace(id)]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != RuntimeDeliveryGroupProcessing || item.LeaseOwner != owner || item.LeaseExpiresAt == nil || !item.LeaseExpiresAt.After(now) {
		return false, nil
	}
	allCompleted, anyFailed := runtimeDeliveryGroupMembersCompleted(item.Members)
	if anyFailed || !allCompleted {
		return false, ErrConflict
	}
	finished := now
	item.Status = RuntimeDeliveryGroupCompleted
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	item.CompletedAt = &finished
	item.Revision++
	item.UpdatedAt = now
	r.deliveryGroups[item.ID] = item
	return true, nil
}

func (r *MemoryRepository) RetryRuntimeDeliveryGroup(_ context.Context, id, owner string, now time.Time, message string) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > MaxRuntimeDeliveryGroupOwnerLength {
		return false, ErrConflict
	}
	now = normalizeRuntimeDeliveryGroupTime(now)
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.deliveryGroups[strings.TrimSpace(id)]
	if !ok {
		return false, ErrNotFound
	}
	if item.Status != RuntimeDeliveryGroupProcessing || item.LeaseOwner != owner || item.LeaseExpiresAt == nil || !item.LeaseExpiresAt.After(now) {
		return false, nil
	}
	item.LastError = SanitizeRuntimeString(strings.TrimSpace(message))
	if len(item.LastError) > MaxRuntimeDeliveryGroupErrorLength {
		item.LastError = item.LastError[:MaxRuntimeDeliveryGroupErrorLength]
	}
	item.LeaseOwner = ""
	item.LeaseExpiresAt = nil
	item.CompletedAt = nil
	if item.Attempt >= MaxRuntimeDeliveryGroupAttempts {
		item.Status = RuntimeDeliveryGroupFailed
	} else {
		item.Status = RuntimeDeliveryGroupQueued
		item.AvailableAt = now.Add(RuntimeDeliveryGroupBackoff(item.Attempt))
	}
	item.Revision++
	item.UpdatedAt = now
	r.deliveryGroups[item.ID] = item
	return true, nil
}

func (r *MemoryRepository) MarkRuntimeDeliveryGroupMember(_ context.Context, id string, expectedRevision int64, kind RuntimeDeliveryKind, outboxID, deliveryID string, status RuntimeDeliveryGroupMemberStatus, message string, now time.Time) (RuntimeDeliveryGroup, bool, error) {
	now = normalizeRuntimeDeliveryGroupTime(now)
	kind = RuntimeDeliveryKind(strings.TrimSpace(string(kind)))
	outboxID = strings.TrimSpace(outboxID)
	deliveryID = strings.TrimSpace(deliveryID)
	if !validRuntimeDeliveryKind(kind) || outboxID == "" || deliveryID == "" || !validRuntimeDeliveryGroupMemberStatus(status) || expectedRevision <= 0 {
		return RuntimeDeliveryGroup{}, false, ErrConflict
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.deliveryGroups[strings.TrimSpace(id)]
	if !ok {
		return RuntimeDeliveryGroup{}, false, ErrNotFound
	}
	if item.Revision != expectedRevision {
		return RuntimeDeliveryGroup{}, false, ErrConflict
	}
	if item.Status == RuntimeDeliveryGroupCompleted || item.Status == RuntimeDeliveryGroupFailed {
		for _, member := range item.Members {
			if member.Kind == kind && member.OutboxID == outboxID && member.DeliveryID == deliveryID && member.Status == status {
				return cloneRuntimeDeliveryGroup(item), true, nil
			}
		}
		return RuntimeDeliveryGroup{}, false, ErrConflict
	}
	memberIndex := -1
	for index, member := range item.Members {
		if member.Kind == kind && member.OutboxID == outboxID && member.DeliveryID == deliveryID {
			memberIndex = index
			break
		}
	}
	if memberIndex < 0 {
		return RuntimeDeliveryGroup{}, false, ErrNotFound
	}
	current := item.Members[memberIndex].Status
	if current == status {
		// A legacy row may already contain a terminal member while the group
		// itself is still processing (for example after a crash between the
		// member JSON update and the aggregate status update).  Re-running the
		// same idempotent mark repairs that aggregate without changing the
		// member's terminal state.
		allCompleted, anyFailed := runtimeDeliveryGroupMembersCompleted(item.Members)
		if anyFailed && item.Status != RuntimeDeliveryGroupFailed {
			item.Status = RuntimeDeliveryGroupFailed
			item.LeaseOwner = ""
			item.LeaseExpiresAt = nil
			item.CompletedAt = nil
			if item.LastError == "" {
				item.LastError = item.Members[memberIndex].LastError
			}
			item.Revision++
			item.UpdatedAt = now
			r.deliveryGroups[item.ID] = item
			return cloneRuntimeDeliveryGroup(item), false, nil
		}
		if allCompleted && item.Status != RuntimeDeliveryGroupCompleted {
			item.Status = RuntimeDeliveryGroupCompleted
			item.LeaseOwner = ""
			item.LeaseExpiresAt = nil
			finished := now
			item.CompletedAt = &finished
			item.Revision++
			item.UpdatedAt = now
			r.deliveryGroups[item.ID] = item
			return cloneRuntimeDeliveryGroup(item), false, nil
		}
		return cloneRuntimeDeliveryGroup(item), true, nil
	}
	if current != RuntimeDeliveryGroupMemberPending {
		return RuntimeDeliveryGroup{}, false, ErrConflict
	}
	item.Members[memberIndex].Status = status
	item.Members[memberIndex].LastError = SanitizeRuntimeString(strings.TrimSpace(message))
	if len(item.Members[memberIndex].LastError) > MaxRuntimeDeliveryGroupMemberErrorLength {
		item.Members[memberIndex].LastError = item.Members[memberIndex].LastError[:MaxRuntimeDeliveryGroupMemberErrorLength]
	}
	item.Members[memberIndex].UpdatedAt = now
	allCompleted, anyFailed := runtimeDeliveryGroupMembersCompleted(item.Members)
	if anyFailed {
		item.Status = RuntimeDeliveryGroupFailed
		item.LeaseOwner = ""
		item.LeaseExpiresAt = nil
		item.CompletedAt = nil
		if item.LastError == "" {
			item.LastError = item.Members[memberIndex].LastError
		}
	} else if allCompleted {
		item.Status = RuntimeDeliveryGroupCompleted
		item.LeaseOwner = ""
		item.LeaseExpiresAt = nil
		finished := now
		item.CompletedAt = &finished
	}
	item.Revision++
	item.UpdatedAt = now
	r.deliveryGroups[item.ID] = item
	return cloneRuntimeDeliveryGroup(item), false, nil
}

// MarkRuntimeDeliveryGroupMemberWithSettlement performs the terminal member
// CAS and the corresponding settlement upsert while holding the same memory
// lock. The regular marker above remains the compatibility path for callers
// that do not opt into durable remote settlement.
func (r *MemoryRepository) MarkRuntimeDeliveryGroupMemberWithSettlement(_ context.Context, id string, expectedRevision int64, kind RuntimeDeliveryKind, outboxID, deliveryID string, status RuntimeDeliveryGroupMemberStatus, message string, now time.Time) (RuntimeDeliveryGroup, bool, *RuntimeDeliveryGroupSettlement, error) {
	now = normalizeRuntimeDeliveryGroupTime(now)
	kind = RuntimeDeliveryKind(strings.TrimSpace(string(kind)))
	outboxID = strings.TrimSpace(outboxID)
	deliveryID = strings.TrimSpace(deliveryID)
	if !validRuntimeDeliveryKind(kind) || outboxID == "" || deliveryID == "" || !validRuntimeDeliveryGroupMemberStatus(status) || expectedRevision <= 0 {
		return RuntimeDeliveryGroup{}, false, nil, ErrConflict
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.deliveryGroups[strings.TrimSpace(id)]
	if !ok {
		return RuntimeDeliveryGroup{}, false, nil, ErrNotFound
	}
	if item.Revision != expectedRevision {
		return RuntimeDeliveryGroup{}, false, nil, ErrConflict
	}
	memberIndex := -1
	for index, member := range item.Members {
		if member.Kind == kind && member.OutboxID == outboxID && member.DeliveryID == deliveryID {
			memberIndex = index
			break
		}
	}
	if memberIndex < 0 {
		return RuntimeDeliveryGroup{}, false, nil, ErrNotFound
	}
	if item.Status == RuntimeDeliveryGroupCompleted || item.Status == RuntimeDeliveryGroupFailed {
		if item.Members[memberIndex].Status != status {
			return RuntimeDeliveryGroup{}, false, nil, ErrConflict
		}
		created, err := NewRuntimeDeliveryGroupSettlement(item, now)
		if err != nil {
			return RuntimeDeliveryGroup{}, false, nil, err
		}
		settlement, err := r.enqueueRuntimeDeliveryGroupSettlementLocked(created, now)
		if err != nil {
			return RuntimeDeliveryGroup{}, false, nil, err
		}
		return cloneRuntimeDeliveryGroup(item), true, &settlement, nil
	}
	current := item.Members[memberIndex].Status
	changed := false
	if current == status {
		allCompleted, anyFailed := runtimeDeliveryGroupMembersCompleted(item.Members)
		if anyFailed && item.Status != RuntimeDeliveryGroupFailed {
			item.Status = RuntimeDeliveryGroupFailed
			item.LeaseOwner = ""
			item.LeaseExpiresAt = nil
			item.CompletedAt = nil
			if item.LastError == "" {
				item.LastError = item.Members[memberIndex].LastError
			}
			changed = true
		} else if allCompleted && item.Status != RuntimeDeliveryGroupCompleted {
			item.Status = RuntimeDeliveryGroupCompleted
			item.LeaseOwner = ""
			item.LeaseExpiresAt = nil
			finished := now
			item.CompletedAt = &finished
			changed = true
		}
	} else {
		if current != RuntimeDeliveryGroupMemberPending {
			return RuntimeDeliveryGroup{}, false, nil, ErrConflict
		}
		item.Members[memberIndex].Status = status
		item.Members[memberIndex].LastError = SanitizeRuntimeString(strings.TrimSpace(message))
		if len(item.Members[memberIndex].LastError) > MaxRuntimeDeliveryGroupMemberErrorLength {
			item.Members[memberIndex].LastError = item.Members[memberIndex].LastError[:MaxRuntimeDeliveryGroupMemberErrorLength]
		}
		item.Members[memberIndex].UpdatedAt = now
		allCompleted, anyFailed := runtimeDeliveryGroupMembersCompleted(item.Members)
		if anyFailed {
			item.Status = RuntimeDeliveryGroupFailed
			item.LeaseOwner = ""
			item.LeaseExpiresAt = nil
			item.CompletedAt = nil
			if item.LastError == "" {
				item.LastError = item.Members[memberIndex].LastError
			}
		} else if allCompleted {
			item.Status = RuntimeDeliveryGroupCompleted
			item.LeaseOwner = ""
			item.LeaseExpiresAt = nil
			finished := now
			item.CompletedAt = &finished
		}
		changed = true
	}
	var settlement *RuntimeDeliveryGroupSettlement
	if changed {
		item.Revision++
		item.UpdatedAt = now
		if item.Status == RuntimeDeliveryGroupCompleted || item.Status == RuntimeDeliveryGroupFailed {
			created, err := NewRuntimeDeliveryGroupSettlement(item, now)
			if err != nil {
				return RuntimeDeliveryGroup{}, false, nil, err
			}
			stored, err := r.enqueueRuntimeDeliveryGroupSettlementLocked(created, now)
			if err != nil {
				return RuntimeDeliveryGroup{}, false, nil, err
			}
			settlement = &stored
		}
		r.deliveryGroups[item.ID] = item
	}
	if settlement == nil && (item.Status == RuntimeDeliveryGroupCompleted || item.Status == RuntimeDeliveryGroupFailed) {
		created, err := NewRuntimeDeliveryGroupSettlement(item, now)
		if err != nil {
			return RuntimeDeliveryGroup{}, false, nil, err
		}
		stored, err := r.enqueueRuntimeDeliveryGroupSettlementLocked(created, now)
		if err != nil {
			return RuntimeDeliveryGroup{}, false, nil, err
		}
		settlement = &stored
	}
	return cloneRuntimeDeliveryGroup(item), !changed, settlement, nil
}

var _ RuntimeDeliveryGroupRepository = (*MemoryRepository)(nil)
var _ RuntimeDeliveryGroupByIDClaimer = (*MemoryRepository)(nil)
var _ RuntimeDeliveryGroupDeferrer = (*MemoryRepository)(nil)
var _ RuntimeDeliveryGroupSettlementCommitRepository = (*MemoryRepository)(nil)
var _ RuntimeDeliveryGroupSagaRepository = (*MemoryRepository)(nil)
