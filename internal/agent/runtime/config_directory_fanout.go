package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	MaxRuntimeConfigDirectoryFanoutDestinations = 32
	MaxRuntimeConfigDirectoryFanoutID           = 240
	MaxRuntimeConfigDirectoryFanoutError        = 4096
)

// RuntimeConfigDirectoryFanoutStatus is the source-side aggregate state for
// one immutable public entry projected to several Runtime destinations. The
// child directory outboxes remain the source of delivery truth.
type RuntimeConfigDirectoryFanoutStatus string

const (
	RuntimeConfigDirectoryFanoutQueued    RuntimeConfigDirectoryFanoutStatus = "queued"
	RuntimeConfigDirectoryFanoutPartial   RuntimeConfigDirectoryFanoutStatus = "partial"
	RuntimeConfigDirectoryFanoutCompleted RuntimeConfigDirectoryFanoutStatus = "completed"
	RuntimeConfigDirectoryFanoutFailed    RuntimeConfigDirectoryFanoutStatus = "failed"
)

func (s RuntimeConfigDirectoryFanoutStatus) valid() bool {
	switch s {
	case RuntimeConfigDirectoryFanoutQueued, RuntimeConfigDirectoryFanoutPartial, RuntimeConfigDirectoryFanoutCompleted, RuntimeConfigDirectoryFanoutFailed:
		return true
	default:
		return false
	}
}

func (s RuntimeConfigDirectoryFanoutStatus) ValidForHTTP() bool {
	return s.valid()
}

func (s RuntimeConfigDirectoryFanoutStatus) terminal() bool {
	return s == RuntimeConfigDirectoryFanoutCompleted || s == RuntimeConfigDirectoryFanoutFailed
}

// RuntimeConfigDirectoryFanoutPlan is metadata-only. It intentionally does
// not duplicate the public entry body; each child RuntimeConfigDirectoryOutbox
// owns its bounded defensive copy. This keeps aggregate queries safe and
// allows one destination to retry without reopening another destination.
type RuntimeConfigDirectoryFanoutPlan struct {
	ID             string                             `json:"id"`
	Source         string                             `json:"source"`
	Kind           RuntimeConfigDirectoryKind         `json:"kind"`
	EntryID        string                             `json:"entry_id"`
	EntryRevision  int                                `json:"entry_revision"`
	PreviousDigest string                             `json:"previous_digest,omitempty"`
	BodyDigest     string                             `json:"body_digest"`
	CorrelationID  string                             `json:"correlation_id,omitempty"`
	IdempotencyKey string                             `json:"idempotency_key,omitempty"`
	Destinations   []string                           `json:"destinations"`
	OutboxIDs      []string                           `json:"outbox_ids"`
	Status         RuntimeConfigDirectoryFanoutStatus `json:"status"`
	PendingCount   int                                `json:"pending_count"`
	CompletedCount int                                `json:"completed_count"`
	FailedCount    int                                `json:"failed_count"`
	Revision       int64                              `json:"revision"`
	LastError      string                             `json:"last_error,omitempty"`
	CreatedAt      time.Time                          `json:"created_at"`
	UpdatedAt      time.Time                          `json:"updated_at"`
}

func RuntimeConfigDirectoryFanoutID(source string, destinations []string, kind RuntimeConfigDirectoryKind, entryID string, revision int, previousDigest, bodyDigest, correlationID, idempotencyKey string) string {
	routes := append([]string(nil), destinations...)
	for index := range routes {
		routes[index] = strings.TrimSpace(routes[index])
	}
	sort.Strings(routes)
	material := strings.Join(append([]string{strings.TrimSpace(source), string(kind), strings.TrimSpace(entryID), fmt.Sprintf("%d", revision), strings.TrimSpace(strings.ToLower(previousDigest)), strings.TrimSpace(strings.ToLower(bodyDigest)), strings.TrimSpace(correlationID), strings.TrimSpace(idempotencyKey)}, routes...), "\x00")
	sum := sha256.Sum256([]byte(material))
	return "runtime-config-directory-fanout-" + hex.EncodeToString(sum[:16])
}

type runtimeConfigDirectoryFanoutRoute struct {
	destination string
	outboxID    string
}

func normalizeRuntimeConfigDirectoryFanoutRoutes(destinations, outboxIDs []string) ([]string, []string, error) {
	if len(destinations) == 0 || len(destinations) > MaxRuntimeConfigDirectoryFanoutDestinations {
		return nil, nil, fmt.Errorf("%w: fanout destination 数量必须是 1-%d", ErrInvalidRuntimeConfigDirectory, MaxRuntimeConfigDirectoryFanoutDestinations)
	}
	if len(destinations) != len(outboxIDs) {
		return nil, nil, fmt.Errorf("%w: fanout destination/outbox 数量不一致", ErrInvalidRuntimeConfigDirectory)
	}
	routes := make([]runtimeConfigDirectoryFanoutRoute, len(destinations))
	seenOutboxIDs := make(map[string]struct{}, len(destinations))
	for index := range destinations {
		destination := strings.TrimSpace(destinations[index])
		outboxID := strings.TrimSpace(outboxIDs[index])
		if destination == "" || len(destination) > MaxRuntimeConfigDirectoryTargetLength || outboxID == "" || len(outboxID) > MaxRuntimeConfigDirectoryOutboxID {
			return nil, nil, fmt.Errorf("%w: fanout route metadata 无效", ErrInvalidRuntimeConfigDirectory)
		}
		if _, exists := seenOutboxIDs[outboxID]; exists {
			return nil, nil, fmt.Errorf("%w: fanout outbox ID 不能重复", ErrInvalidRuntimeConfigDirectory)
		}
		seenOutboxIDs[outboxID] = struct{}{}
		routes[index] = runtimeConfigDirectoryFanoutRoute{destination: destination, outboxID: outboxID}
	}
	sort.Slice(routes, func(left, right int) bool { return routes[left].destination < routes[right].destination })
	normalizedDestinations := make([]string, len(routes))
	normalizedOutboxIDs := make([]string, len(routes))
	for index, route := range routes {
		if index > 0 && routes[index-1].destination == route.destination {
			return nil, nil, fmt.Errorf("%w: fanout destination 不能重复", ErrInvalidRuntimeConfigDirectory)
		}
		normalizedDestinations[index] = route.destination
		normalizedOutboxIDs[index] = route.outboxID
	}
	return normalizedDestinations, normalizedOutboxIDs, nil
}

func (p RuntimeConfigDirectoryFanoutPlan) normalize(now time.Time) (RuntimeConfigDirectoryFanoutPlan, error) {
	p.ID = strings.TrimSpace(p.ID)
	p.Source = strings.TrimSpace(p.Source)
	p.Kind = RuntimeConfigDirectoryKind(strings.TrimSpace(strings.ToLower(string(p.Kind))))
	p.EntryID = strings.TrimSpace(p.EntryID)
	p.PreviousDigest = strings.TrimSpace(strings.ToLower(p.PreviousDigest))
	p.BodyDigest = strings.TrimSpace(strings.ToLower(p.BodyDigest))
	p.CorrelationID = strings.TrimSpace(p.CorrelationID)
	p.IdempotencyKey = strings.TrimSpace(p.IdempotencyKey)
	p.LastError = SanitizeRuntimeString(strings.TrimSpace(p.LastError))
	if p.Source == "" || len(p.Source) > MaxRuntimeConfigDirectorySourceLength || !p.Kind.valid() || p.EntryID == "" || p.EntryRevision <= 0 || !runtimeConfigDirectoryDigestValid(p.BodyDigest) || (p.PreviousDigest != "" && !runtimeConfigDirectoryDigestValid(p.PreviousDigest)) {
		return RuntimeConfigDirectoryFanoutPlan{}, fmt.Errorf("%w: fanout immutable metadata 不完整", ErrInvalidRuntimeConfigDirectory)
	}
	if len(p.ID) == 0 || len(p.ID) > MaxRuntimeConfigDirectoryFanoutID || len(p.EntryID) > MaxRuntimeConfigDirectoryIDLength || len(p.CorrelationID) > MaxRuntimeConfigDirectoryCorrelation || len(p.IdempotencyKey) > MaxRuntimeConfigDirectoryIdempotency {
		return RuntimeConfigDirectoryFanoutPlan{}, fmt.Errorf("%w: fanout metadata 超出长度限制", ErrInvalidRuntimeConfigDirectory)
	}
	if err := validateRuntimeConfigDirectoryID(p.EntryID); err != nil {
		return RuntimeConfigDirectoryFanoutPlan{}, fmt.Errorf("%w: %v", ErrInvalidRuntimeConfigDirectory, err)
	}
	destinations, outboxIDs, err := normalizeRuntimeConfigDirectoryFanoutRoutes(p.Destinations, p.OutboxIDs)
	if err != nil {
		return RuntimeConfigDirectoryFanoutPlan{}, err
	}
	p.Destinations = destinations
	p.OutboxIDs = outboxIDs
	expectedID := RuntimeConfigDirectoryFanoutID(p.Source, p.Destinations, p.Kind, p.EntryID, p.EntryRevision, p.PreviousDigest, p.BodyDigest, p.CorrelationID, p.IdempotencyKey)
	if p.ID != expectedID {
		return RuntimeConfigDirectoryFanoutPlan{}, fmt.Errorf("%w: fanout plan identity 不一致", ErrRuntimeConfigDirectoryAuth)
	}
	if p.Status == "" {
		p.Status = RuntimeConfigDirectoryFanoutQueued
	}
	if !p.Status.valid() {
		return RuntimeConfigDirectoryFanoutPlan{}, fmt.Errorf("%w: fanout status %q 不受支持", ErrInvalidRuntimeConfigDirectory, p.Status)
	}
	if p.PendingCount < 0 || p.CompletedCount < 0 || p.FailedCount < 0 {
		return RuntimeConfigDirectoryFanoutPlan{}, fmt.Errorf("%w: fanout summary 不能为负数", ErrInvalidRuntimeConfigDirectory)
	}
	if p.PendingCount+p.CompletedCount+p.FailedCount == 0 && p.Status == RuntimeConfigDirectoryFanoutQueued {
		p.PendingCount = len(p.Destinations)
	}
	if p.PendingCount+p.CompletedCount+p.FailedCount != len(p.Destinations) {
		return RuntimeConfigDirectoryFanoutPlan{}, fmt.Errorf("%w: fanout summary 与 destination 数量不一致", ErrInvalidRuntimeConfigDirectory)
	}
	if len(p.LastError) > MaxRuntimeConfigDirectoryFanoutError {
		p.LastError = p.LastError[:MaxRuntimeConfigDirectoryFanoutError]
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if p.Revision <= 0 {
		p.Revision = 1
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	} else {
		p.CreatedAt = p.CreatedAt.UTC()
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = p.CreatedAt
	} else {
		p.UpdatedAt = p.UpdatedAt.UTC()
	}
	return p, nil
}

func (p RuntimeConfigDirectoryFanoutPlan) Normalize(now time.Time) (RuntimeConfigDirectoryFanoutPlan, error) {
	return p.normalize(now)
}

func cloneRuntimeConfigDirectoryFanoutPlan(plan RuntimeConfigDirectoryFanoutPlan) RuntimeConfigDirectoryFanoutPlan {
	plan.Destinations = append([]string(nil), plan.Destinations...)
	plan.OutboxIDs = append([]string(nil), plan.OutboxIDs...)
	return plan
}

// Matches compares the immutable plan identity and route membership. Queue
// state and aggregate counters are intentionally excluded for idempotent
// enqueue and retry-safe reconciliation.
func (p RuntimeConfigDirectoryFanoutPlan) Matches(other RuntimeConfigDirectoryFanoutPlan) bool {
	left, leftErr := p.normalize(time.Time{})
	right, rightErr := other.normalize(time.Time{})
	if leftErr != nil || rightErr != nil {
		return false
	}
	return left.ID == right.ID && left.Source == right.Source && left.Kind == right.Kind && left.EntryID == right.EntryID && left.EntryRevision == right.EntryRevision && left.PreviousDigest == right.PreviousDigest && left.BodyDigest == right.BodyDigest && left.CorrelationID == right.CorrelationID && left.IdempotencyKey == right.IdempotencyKey && equalRuntimeConfigDirectoryStrings(left.Destinations, right.Destinations) && equalRuntimeConfigDirectoryStrings(left.OutboxIDs, right.OutboxIDs)
}

func equalRuntimeConfigDirectoryStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// NewRuntimeConfigDirectoryFanoutPlan creates one metadata-only parent and
// one independent durable child cursor per destination. All destinations are
// normalized before IDs are derived, so callers cannot change the plan by
// reordering the input list.
func NewRuntimeConfigDirectoryFanoutPlan(source string, destinations []string, entry RuntimeConfigDirectoryEntry, previousDigest, correlationID, idempotencyKey string, now time.Time) (RuntimeConfigDirectoryFanoutPlan, []RuntimeConfigDirectoryOutbox, error) {
	entry, err := entry.Normalize()
	if err != nil {
		return RuntimeConfigDirectoryFanoutPlan{}, nil, err
	}
	bodyDigest, err := RuntimeConfigDirectoryEntryDigest(entry)
	if err != nil {
		return RuntimeConfigDirectoryFanoutPlan{}, nil, err
	}
	source = strings.TrimSpace(source)
	previousDigest = strings.TrimSpace(strings.ToLower(previousDigest))
	correlationID = strings.TrimSpace(correlationID)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if len(destinations) == 0 || len(destinations) > MaxRuntimeConfigDirectoryFanoutDestinations {
		return RuntimeConfigDirectoryFanoutPlan{}, nil, fmt.Errorf("%w: fanout destination 数量必须是 1-%d", ErrInvalidRuntimeConfigDirectory, MaxRuntimeConfigDirectoryFanoutDestinations)
	}
	normalizedDestinations := make([]string, len(destinations))
	for index := range destinations {
		normalizedDestinations[index] = strings.TrimSpace(destinations[index])
	}
	sort.Strings(normalizedDestinations)
	for index := range normalizedDestinations {
		if normalizedDestinations[index] == "" || len(normalizedDestinations[index]) > MaxRuntimeConfigDirectoryTargetLength || (index > 0 && normalizedDestinations[index-1] == normalizedDestinations[index]) {
			return RuntimeConfigDirectoryFanoutPlan{}, nil, fmt.Errorf("%w: fanout destination 无效或重复", ErrInvalidRuntimeConfigDirectory)
		}
	}
	planID := RuntimeConfigDirectoryFanoutID(source, normalizedDestinations, entry.Kind, entry.ID, entry.Revision, previousDigest, bodyDigest, correlationID, idempotencyKey)
	children := make([]RuntimeConfigDirectoryOutbox, 0, len(normalizedDestinations))
	outboxIDs := make([]string, 0, len(normalizedDestinations))
	for _, destination := range normalizedDestinations {
		child, childErr := NewRuntimeConfigDirectoryOutbox(source, destination, entry, previousDigest, correlationID, idempotencyKey, now)
		if childErr != nil {
			return RuntimeConfigDirectoryFanoutPlan{}, nil, childErr
		}
		child.FanoutID = planID
		children = append(children, child)
		outboxIDs = append(outboxIDs, child.ID)
	}
	plan := RuntimeConfigDirectoryFanoutPlan{
		ID: planID, Source: source, Kind: entry.Kind, EntryID: entry.ID, EntryRevision: entry.Revision,
		PreviousDigest: previousDigest, BodyDigest: bodyDigest, CorrelationID: correlationID, IdempotencyKey: idempotencyKey,
		Destinations: normalizedDestinations, OutboxIDs: outboxIDs, Status: RuntimeConfigDirectoryFanoutQueued,
		PendingCount: len(normalizedDestinations), CreatedAt: now, UpdatedAt: now,
	}
	normalizedPlan, normalizeErr := plan.normalize(now)
	return normalizedPlan, children, normalizeErr
}

// ReconcileSummary derives aggregate state from child cursors without
// mutating the plan. Repositories call it inside their own lock/transaction.
func (p RuntimeConfigDirectoryFanoutPlan) ReconcileSummary(children []RuntimeConfigDirectoryOutbox, now time.Time) (RuntimeConfigDirectoryFanoutPlan, error) {
	plan, err := p.normalize(now)
	if err != nil {
		return RuntimeConfigDirectoryFanoutPlan{}, err
	}
	byID := make(map[string]RuntimeConfigDirectoryOutbox, len(children))
	expectedDestinations := make(map[string]string, len(plan.OutboxIDs))
	for index, childID := range plan.OutboxIDs {
		expectedDestinations[childID] = plan.Destinations[index]
	}
	for _, child := range children {
		normalized, normalizeErr := child.Normalize(now)
		if normalizeErr != nil {
			return RuntimeConfigDirectoryFanoutPlan{}, normalizeErr
		}
		expectedDestination, expected := expectedDestinations[normalized.ID]
		if !expected || normalized.FanoutID != plan.ID || normalized.Destination != expectedDestination || normalized.Source != plan.Source || normalized.Kind != plan.Kind || normalized.EntryID != plan.EntryID || normalized.EntryRevision != plan.EntryRevision || normalized.PreviousDigest != plan.PreviousDigest || normalized.BodyDigest != plan.BodyDigest || normalized.CorrelationID != plan.CorrelationID || normalized.IdempotencyKey != plan.IdempotencyKey {
			return RuntimeConfigDirectoryFanoutPlan{}, fmt.Errorf("%w: fanout child immutable identity 不一致", ErrConflict)
		}
		if _, duplicate := byID[normalized.ID]; duplicate {
			return RuntimeConfigDirectoryFanoutPlan{}, fmt.Errorf("%w: fanout child outbox ID 重复", ErrConflict)
		}
		byID[normalized.ID] = normalized
	}
	if len(byID) != len(plan.OutboxIDs) {
		return RuntimeConfigDirectoryFanoutPlan{}, fmt.Errorf("%w: fanout child 数量不一致", ErrConflict)
	}
	completed, failed, pending := 0, 0, 0
	for _, childID := range plan.OutboxIDs {
		child, ok := byID[childID]
		if !ok {
			return RuntimeConfigDirectoryFanoutPlan{}, fmt.Errorf("%w: fanout child outbox 缺失", ErrConflict)
		}
		switch child.Status {
		case RuntimeConfigDirectoryOutboxCompleted:
			completed++
		case RuntimeConfigDirectoryOutboxFailed:
			failed++
		case RuntimeConfigDirectoryOutboxQueued, RuntimeConfigDirectoryOutboxProcessing:
			pending++
		default:
			return RuntimeConfigDirectoryFanoutPlan{}, fmt.Errorf("%w: fanout child status 无效", ErrConflict)
		}
	}
	status := RuntimeConfigDirectoryFanoutQueued
	switch {
	case failed > 0 && pending == 0:
		status = RuntimeConfigDirectoryFanoutFailed
	case completed == len(plan.Destinations):
		status = RuntimeConfigDirectoryFanoutCompleted
	case completed+failed > 0:
		status = RuntimeConfigDirectoryFanoutPartial
	}
	previousStatus, previousPending, previousCompleted, previousFailed, previousError := plan.Status, plan.PendingCount, plan.CompletedCount, plan.FailedCount, plan.LastError
	plan.Status = status
	plan.PendingCount = pending
	plan.CompletedCount = completed
	plan.FailedCount = failed
	if failed > 0 {
		plan.LastError = "fanout child delivery failed"
	} else {
		plan.LastError = ""
	}
	if previousStatus != plan.Status || previousPending != plan.PendingCount || previousCompleted != plan.CompletedCount || previousFailed != plan.FailedCount || previousError != plan.LastError {
		plan.Revision++
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	plan.UpdatedAt = now.UTC()
	return plan.normalize(now)
}

// RuntimeConfigDirectoryFanoutRepository is the optional atomic source-side
// parent/child boundary. Built-in Memory/SQLite implementations insert the
// parent and all children in one local transaction; older embedders must opt
// in rather than silently creating a partially registered fan-out.
type RuntimeConfigDirectoryFanoutRepository interface {
	EnqueueRuntimeConfigDirectoryFanout(context.Context, RuntimeConfigDirectoryFanoutPlan, []RuntimeConfigDirectoryOutbox) (RuntimeConfigDirectoryFanoutPlan, error)
	GetRuntimeConfigDirectoryFanout(context.Context, string) (RuntimeConfigDirectoryFanoutPlan, error)
	ListRuntimeConfigDirectoryFanouts(context.Context, string, RuntimeConfigDirectoryFanoutStatus, int) ([]RuntimeConfigDirectoryFanoutPlan, error)
	ReconcileRuntimeConfigDirectoryFanout(context.Context, string, time.Time) (RuntimeConfigDirectoryFanoutPlan, error)
}
