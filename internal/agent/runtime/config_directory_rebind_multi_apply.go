package runtime

// This file defines the additive multi-target rebind apply contract.  The
// existing single-target confirmation/apply types remain unchanged on the
// wire; a multi-target request owns one metadata-only parent and one ordinary
// signed apply child per destination.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	MaxRuntimeConfigDirectoryRebindMultiConfirmationIDLength = MaxRuntimeConfigDirectoryRebindConfirmationIDLength
	MaxRuntimeConfigDirectoryRebindMultiApplyIDLength        = MaxRuntimeConfigDirectoryRebindApplyIDLength
	MaxRuntimeConfigDirectoryRebindMultiApplyErrorLength     = MaxRuntimeConfigDirectoryRebindApplyErrorLength
)

var (
	ErrInvalidRuntimeConfigDirectoryRebindMultiConfirmation  = errors.New("Runtime 配置目录多目标 rebind 确认无效")
	ErrRuntimeConfigDirectoryRebindMultiConfirmationConflict = errors.New("Runtime 配置目录多目标 rebind 确认冲突")
	ErrRuntimeConfigDirectoryRebindMultiConfirmationExpired  = errors.New("Runtime 配置目录多目标 rebind 确认已过期")
	ErrInvalidRuntimeConfigDirectoryRebindMultiApply         = errors.New("Runtime 配置目录多目标 rebind apply 无效")
	ErrRuntimeConfigDirectoryRebindMultiApplyConflict        = errors.New("Runtime 配置目录多目标 rebind apply 冲突")
)

type RuntimeConfigDirectoryRebindMultiConfirmationStatus string

const (
	RuntimeConfigDirectoryRebindMultiConfirmationConfirmed RuntimeConfigDirectoryRebindMultiConfirmationStatus = "confirmed"
	RuntimeConfigDirectoryRebindMultiConfirmationConsumed  RuntimeConfigDirectoryRebindMultiConfirmationStatus = "consumed"
	RuntimeConfigDirectoryRebindMultiConfirmationExpired   RuntimeConfigDirectoryRebindMultiConfirmationStatus = "expired"
)

func (s RuntimeConfigDirectoryRebindMultiConfirmationStatus) valid() bool {
	return s == RuntimeConfigDirectoryRebindMultiConfirmationConfirmed || s == RuntimeConfigDirectoryRebindMultiConfirmationConsumed || s == RuntimeConfigDirectoryRebindMultiConfirmationExpired
}

// RuntimeConfigDirectoryRebindMultiConfirmation is an explicit one-time
// confirmation for a complete destination set.  It never contains a
// checkpoint projection or any user/provider payload.
type RuntimeConfigDirectoryRebindMultiConfirmation struct {
	ID                   string                                              `json:"id"`
	PlanID               string                                              `json:"plan_id"`
	InvocationID         string                                              `json:"invocation_id"`
	Source               string                                              `json:"source"`
	Destinations         []string                                            `json:"destinations"`
	ConfigSnapshotDigest string                                              `json:"config_snapshot_digest"`
	CapabilitiesDigest   string                                              `json:"capabilities_digest"`
	Status               RuntimeConfigDirectoryRebindMultiConfirmationStatus `json:"status"`
	Revision             int64                                               `json:"revision"`
	ExpiresAt            time.Time                                           `json:"expires_at"`
	CreatedAt            time.Time                                           `json:"created_at"`
	ConfirmedAt          time.Time                                           `json:"confirmed_at"`
	ConsumedAt           *time.Time                                          `json:"consumed_at,omitempty"`
	IdempotencyKey       string                                              `json:"-"`
}

func RuntimeConfigDirectoryRebindMultiConfirmationID(planID, idempotencyKey string) string {
	material := strings.TrimSpace(planID) + "\x00" + strings.TrimSpace(idempotencyKey)
	sum := sha256.Sum256([]byte(material))
	return "runtime-rebind-multi-confirmation-" + hex.EncodeToString(sum[:16])
}

func cloneRuntimeConfigDirectoryRebindMultiConfirmation(item RuntimeConfigDirectoryRebindMultiConfirmation) RuntimeConfigDirectoryRebindMultiConfirmation {
	item.Destinations = append([]string(nil), item.Destinations...)
	item.ExpiresAt = item.ExpiresAt.UTC()
	item.CreatedAt = item.CreatedAt.UTC()
	item.ConfirmedAt = item.ConfirmedAt.UTC()
	if item.ConsumedAt != nil {
		value := item.ConsumedAt.UTC()
		item.ConsumedAt = &value
	}
	return item
}

func (c RuntimeConfigDirectoryRebindMultiConfirmation) Normalize(now time.Time) (RuntimeConfigDirectoryRebindMultiConfirmation, error) {
	c.ID = strings.TrimSpace(c.ID)
	c.PlanID = strings.TrimSpace(c.PlanID)
	c.InvocationID = strings.TrimSpace(c.InvocationID)
	c.Source = strings.TrimSpace(c.Source)
	c.ConfigSnapshotDigest = strings.TrimSpace(strings.ToLower(c.ConfigSnapshotDigest))
	c.CapabilitiesDigest = strings.TrimSpace(strings.ToLower(c.CapabilitiesDigest))
	c.Status = RuntimeConfigDirectoryRebindMultiConfirmationStatus(strings.TrimSpace(strings.ToLower(string(c.Status))))
	c.IdempotencyKey = strings.TrimSpace(c.IdempotencyKey)
	destinations, err := normalizeRuntimeConfigDirectoryRebindMultiDestinations(c.Destinations)
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiConfirmation{}, err
	}
	c.Destinations = destinations
	if c.ID == "" || c.PlanID == "" || c.InvocationID == "" || c.Source == "" || c.IdempotencyKey == "" || !runtimeConfigSnapshotDigestValid(c.ConfigSnapshotDigest) || !isRuntimeEventDeliveryDigest(strings.TrimPrefix(c.CapabilitiesDigest, "sha256:")) {
		return RuntimeConfigDirectoryRebindMultiConfirmation{}, fmt.Errorf("%w: confirmation identity 不完整", ErrInvalidRuntimeConfigDirectoryRebindMultiConfirmation)
	}
	if c.ID != RuntimeConfigDirectoryRebindMultiConfirmationID(c.PlanID, c.IdempotencyKey) {
		return RuntimeConfigDirectoryRebindMultiConfirmation{}, fmt.Errorf("%w: confirmation ID 不是稳定派生值", ErrRuntimeConfigDirectoryRebindMultiConfirmationConflict)
	}
	if len(c.ID) > MaxRuntimeConfigDirectoryRebindMultiConfirmationIDLength || len(c.PlanID) > MaxRuntimeConfigDirectoryRebindID || len(c.InvocationID) > MaxRuntimeConfigDirectoryRebindInvocationID || len(c.Source) > MaxRuntimeConfigDirectorySourceLength || len(c.IdempotencyKey) > MaxRuntimeConfigDirectoryRebindConfirmationKeyLength {
		return RuntimeConfigDirectoryRebindMultiConfirmation{}, fmt.Errorf("%w: confirmation metadata 超出长度限制", ErrInvalidRuntimeConfigDirectoryRebindMultiConfirmation)
	}
	if !c.Status.valid() {
		return RuntimeConfigDirectoryRebindMultiConfirmation{}, fmt.Errorf("%w: confirmation status 不受支持", ErrInvalidRuntimeConfigDirectoryRebindMultiConfirmation)
	}
	if c.Revision <= 0 {
		c.Revision = 1
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	} else {
		c.CreatedAt = c.CreatedAt.UTC()
	}
	if c.ConfirmedAt.IsZero() {
		c.ConfirmedAt = c.CreatedAt
	} else {
		c.ConfirmedAt = c.ConfirmedAt.UTC()
	}
	if c.ExpiresAt.IsZero() {
		c.ExpiresAt = c.CreatedAt.Add(DefaultRuntimeConfigDirectoryRebindConfirmationTTL)
	} else {
		c.ExpiresAt = c.ExpiresAt.UTC()
	}
	if !c.ExpiresAt.After(c.CreatedAt) || c.ExpiresAt.After(c.CreatedAt.Add(MaxRuntimeConfigDirectoryRebindConfirmationLease)) {
		return RuntimeConfigDirectoryRebindMultiConfirmation{}, fmt.Errorf("%w: confirmation TTL 无效", ErrInvalidRuntimeConfigDirectoryRebindMultiConfirmation)
	}
	switch c.Status {
	case RuntimeConfigDirectoryRebindMultiConfirmationConfirmed:
		if c.ConsumedAt != nil {
			return RuntimeConfigDirectoryRebindMultiConfirmation{}, fmt.Errorf("%w: confirmed confirmation 不能带 consumed_at", ErrInvalidRuntimeConfigDirectoryRebindMultiConfirmation)
		}
	case RuntimeConfigDirectoryRebindMultiConfirmationConsumed:
		if c.ConsumedAt == nil || c.ConsumedAt.IsZero() {
			return RuntimeConfigDirectoryRebindMultiConfirmation{}, fmt.Errorf("%w: consumed confirmation 缺少 consumed_at", ErrInvalidRuntimeConfigDirectoryRebindMultiConfirmation)
		}
		value := c.ConsumedAt.UTC()
		c.ConsumedAt = &value
	case RuntimeConfigDirectoryRebindMultiConfirmationExpired:
		if c.ConsumedAt != nil {
			return RuntimeConfigDirectoryRebindMultiConfirmation{}, fmt.Errorf("%w: expired confirmation 不能带 consumed_at", ErrInvalidRuntimeConfigDirectoryRebindMultiConfirmation)
		}
	}
	return c, nil
}

func NewRuntimeConfigDirectoryRebindMultiConfirmation(plan RuntimeConfigDirectoryRebindPlan, idempotencyKey string, now time.Time) (RuntimeConfigDirectoryRebindMultiConfirmation, error) {
	plan, err := plan.Normalize(now)
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiConfirmation{}, err
	}
	if len(plan.Destinations) < 2 {
		return RuntimeConfigDirectoryRebindMultiConfirmation{}, fmt.Errorf("%w: multi confirmation 至少需要两个 destination", ErrInvalidRuntimeConfigDirectoryRebindMultiConfirmation)
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return RuntimeConfigDirectoryRebindMultiConfirmation{}, fmt.Errorf("%w: confirmation 必须提供幂等键", ErrInvalidRuntimeConfigDirectoryRebindMultiConfirmation)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	item := RuntimeConfigDirectoryRebindMultiConfirmation{
		ID: RuntimeConfigDirectoryRebindMultiConfirmationID(plan.ID, idempotencyKey), PlanID: plan.ID,
		InvocationID: plan.InvocationID, Source: plan.Source, Destinations: append([]string(nil), plan.Destinations...),
		ConfigSnapshotDigest: plan.ConfigSnapshotDigest, CapabilitiesDigest: plan.CapabilitiesDigest,
		Status: RuntimeConfigDirectoryRebindMultiConfirmationConfirmed, Revision: 1,
		ExpiresAt: now.Add(DefaultRuntimeConfigDirectoryRebindConfirmationTTL), CreatedAt: now, ConfirmedAt: now,
		IdempotencyKey: idempotencyKey,
	}
	return item.Normalize(now)
}

func RuntimeConfigDirectoryRebindMultiConfirmationDigest(item RuntimeConfigDirectoryRebindMultiConfirmation) (string, error) {
	normalized, err := item.Normalize(time.Time{})
	if err != nil {
		return "", err
	}
	material := struct {
		ID                   string   `json:"id"`
		PlanID               string   `json:"plan_id"`
		InvocationID         string   `json:"invocation_id"`
		Source               string   `json:"source"`
		Destinations         []string `json:"destinations"`
		ConfigSnapshotDigest string   `json:"config_snapshot_digest"`
		CapabilitiesDigest   string   `json:"capabilities_digest"`
	}{normalized.ID, normalized.PlanID, normalized.InvocationID, normalized.Source, normalized.Destinations, normalized.ConfigSnapshotDigest, normalized.CapabilitiesDigest}
	encoded, err := json.Marshal(material)
	if err != nil {
		return "", fmt.Errorf("%w: confirmation digest 编码失败", ErrInvalidRuntimeConfigDirectoryRebindMultiConfirmation)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func normalizeRuntimeConfigDirectoryRebindMultiDestinations(destinations []string) ([]string, error) {
	if len(destinations) < 2 || len(destinations) > MaxRuntimeConfigDirectoryRebindDestinations {
		return nil, fmt.Errorf("%w: destination 数量必须是 2-%d", ErrInvalidRuntimeConfigDirectoryRebindMultiApply, MaxRuntimeConfigDirectoryRebindDestinations)
	}
	result := make([]string, len(destinations))
	for index, destination := range destinations {
		destination = strings.TrimSpace(destination)
		if destination == "" || len(destination) > MaxRuntimeConfigDirectoryTargetLength {
			return nil, fmt.Errorf("%w: destination metadata 无效", ErrInvalidRuntimeConfigDirectoryRebindMultiApply)
		}
		result[index] = destination
	}
	sort.Strings(result)
	for index := 1; index < len(result); index++ {
		if result[index-1] == result[index] {
			return nil, fmt.Errorf("%w: destination 不能重复", ErrInvalidRuntimeConfigDirectoryRebindMultiApply)
		}
	}
	return result, nil
}

type RuntimeConfigDirectoryRebindMultiApplyStatus string

const (
	RuntimeConfigDirectoryRebindMultiApplyQueued     RuntimeConfigDirectoryRebindMultiApplyStatus = "queued"
	RuntimeConfigDirectoryRebindMultiApplyProcessing RuntimeConfigDirectoryRebindMultiApplyStatus = "processing"
	RuntimeConfigDirectoryRebindMultiApplyPartial    RuntimeConfigDirectoryRebindMultiApplyStatus = "partial"
	RuntimeConfigDirectoryRebindMultiApplyCompleted  RuntimeConfigDirectoryRebindMultiApplyStatus = "completed"
	RuntimeConfigDirectoryRebindMultiApplyFailed     RuntimeConfigDirectoryRebindMultiApplyStatus = "failed"
)

func (s RuntimeConfigDirectoryRebindMultiApplyStatus) valid() bool {
	switch s {
	case RuntimeConfigDirectoryRebindMultiApplyQueued, RuntimeConfigDirectoryRebindMultiApplyProcessing, RuntimeConfigDirectoryRebindMultiApplyPartial, RuntimeConfigDirectoryRebindMultiApplyCompleted, RuntimeConfigDirectoryRebindMultiApplyFailed:
		return true
	default:
		return false
	}
}

func (s RuntimeConfigDirectoryRebindMultiApplyStatus) terminal() bool {
	return s == RuntimeConfigDirectoryRebindMultiApplyCompleted || s == RuntimeConfigDirectoryRebindMultiApplyFailed
}

func (s RuntimeConfigDirectoryRebindMultiApplyStatus) ValidForHTTP() bool { return s.valid() }

// RuntimeConfigDirectoryRebindMultiApply is metadata-only parent state.  The
// projection is held by child rows and is never exposed through this type.
type RuntimeConfigDirectoryRebindMultiApply struct {
	ID                   string                                       `json:"id"`
	PlanID               string                                       `json:"plan_id"`
	ConfirmationID       string                                       `json:"confirmation_id"`
	ConfirmationDigest   string                                       `json:"confirmation_digest"`
	Source               string                                       `json:"source"`
	InvocationID         string                                       `json:"invocation_id"`
	BoundaryStatus       InvocationStatus                             `json:"boundary_status"`
	ConfigSnapshotDigest string                                       `json:"config_snapshot_digest"`
	CapabilitiesDigest   string                                       `json:"capabilities_digest"`
	SnapshotRevision     int64                                        `json:"snapshot_revision"`
	EventSequence        int64                                        `json:"event_sequence"`
	SnapshotDigest       string                                       `json:"snapshot_digest"`
	Destinations         []string                                     `json:"destinations"`
	ChildApplyIDs        []string                                     `json:"child_apply_ids"`
	Status               RuntimeConfigDirectoryRebindMultiApplyStatus `json:"status"`
	PendingCount         int                                          `json:"pending_count"`
	ProcessingCount      int                                          `json:"processing_count"`
	CompletedCount       int                                          `json:"completed_count"`
	FailedCount          int                                          `json:"failed_count"`
	LastError            string                                       `json:"last_error,omitempty"`
	Revision             int64                                        `json:"revision"`
	CreatedAt            time.Time                                    `json:"created_at"`
	UpdatedAt            time.Time                                    `json:"updated_at"`
	CompletedAt          *time.Time                                   `json:"completed_at,omitempty"`
	IdempotencyKey       string                                       `json:"-"`
}

func RuntimeConfigDirectoryRebindMultiApplyID(planID, confirmationID string, snapshotRevision, eventSequence int64, snapshotDigest, idempotencyKey string, destinations []string) string {
	routes, _ := normalizeRuntimeConfigDirectoryRebindMultiDestinations(destinations)
	material := strings.Join(append([]string{strings.TrimSpace(planID), strings.TrimSpace(confirmationID), strconv.FormatInt(snapshotRevision, 10), strconv.FormatInt(eventSequence, 10), strings.TrimSpace(strings.ToLower(snapshotDigest)), strings.TrimSpace(idempotencyKey)}, routes...), "\x00")
	sum := sha256.Sum256([]byte(material))
	return "runtime-rebind-multi-apply-" + hex.EncodeToString(sum[:16])
}

func RuntimeConfigDirectoryRebindMultiApplyChildID(parentID, destination string) string {
	material := strings.TrimSpace(parentID) + "\x00" + strings.TrimSpace(destination)
	sum := sha256.Sum256([]byte(material))
	return "runtime-rebind-multi-child-" + hex.EncodeToString(sum[:16])
}

func cloneRuntimeConfigDirectoryRebindMultiApply(item RuntimeConfigDirectoryRebindMultiApply) RuntimeConfigDirectoryRebindMultiApply {
	item.Destinations = append([]string(nil), item.Destinations...)
	item.ChildApplyIDs = append([]string(nil), item.ChildApplyIDs...)
	item.LastError = SanitizeRuntimeString(item.LastError)
	item.CreatedAt = item.CreatedAt.UTC()
	item.UpdatedAt = item.UpdatedAt.UTC()
	if item.CompletedAt != nil {
		value := item.CompletedAt.UTC()
		item.CompletedAt = &value
	}
	return item
}

func (p RuntimeConfigDirectoryRebindMultiApply) Normalize(now time.Time) (RuntimeConfigDirectoryRebindMultiApply, error) {
	p.ID = strings.TrimSpace(p.ID)
	p.PlanID = strings.TrimSpace(p.PlanID)
	p.ConfirmationID = strings.TrimSpace(p.ConfirmationID)
	p.ConfirmationDigest = strings.TrimSpace(strings.ToLower(p.ConfirmationDigest))
	p.Source = strings.TrimSpace(p.Source)
	p.InvocationID = strings.TrimSpace(p.InvocationID)
	p.BoundaryStatus = InvocationStatus(strings.TrimSpace(strings.ToLower(string(p.BoundaryStatus))))
	p.ConfigSnapshotDigest = strings.TrimSpace(strings.ToLower(p.ConfigSnapshotDigest))
	p.CapabilitiesDigest = strings.TrimSpace(strings.ToLower(p.CapabilitiesDigest))
	p.SnapshotDigest = strings.TrimSpace(strings.ToLower(p.SnapshotDigest))
	p.IdempotencyKey = strings.TrimSpace(p.IdempotencyKey)
	p.LastError = SanitizeRuntimeString(strings.TrimSpace(p.LastError))
	// Normalize is intentionally safe to call concurrently for idempotent
	// retries.  Clone caller-owned slices before canonicalizing their entries.
	p.ChildApplyIDs = append([]string(nil), p.ChildApplyIDs...)
	destinations, err := normalizeRuntimeConfigDirectoryRebindMultiDestinations(p.Destinations)
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, err
	}
	p.Destinations = destinations
	if len(p.ChildApplyIDs) != len(p.Destinations) {
		return RuntimeConfigDirectoryRebindMultiApply{}, fmt.Errorf("%w: child 数量与 destination 不一致", ErrInvalidRuntimeConfigDirectoryRebindMultiApply)
	}
	for index, childID := range p.ChildApplyIDs {
		childID = strings.TrimSpace(childID)
		if childID == "" || len(childID) > MaxRuntimeConfigDirectoryRebindApplyIDLength || childID != RuntimeConfigDirectoryRebindMultiApplyChildID(p.ID, p.Destinations[index]) {
			return RuntimeConfigDirectoryRebindMultiApply{}, fmt.Errorf("%w: child identity 不一致", ErrRuntimeConfigDirectoryRebindMultiApplyConflict)
		}
		p.ChildApplyIDs[index] = childID
	}
	if p.ID == "" || p.PlanID == "" || p.ConfirmationID == "" || p.ConfirmationDigest == "" || p.Source == "" || p.InvocationID == "" || !runtimeConfigSnapshotDigestValid(p.ConfigSnapshotDigest) || !isRuntimeEventDeliveryDigest(p.SnapshotDigest) || !isRuntimeEventDeliveryDigest(p.ConfirmationDigest) || !runtimeConfigSnapshotDigestValid(p.CapabilitiesDigest) || p.SnapshotRevision <= 0 || p.EventSequence < 0 || p.IdempotencyKey == "" {
		return RuntimeConfigDirectoryRebindMultiApply{}, fmt.Errorf("%w: parent identity 不完整", ErrInvalidRuntimeConfigDirectoryRebindMultiApply)
	}
	if p.ID != RuntimeConfigDirectoryRebindMultiApplyID(p.PlanID, p.ConfirmationID, p.SnapshotRevision, p.EventSequence, p.SnapshotDigest, p.IdempotencyKey, p.Destinations) {
		return RuntimeConfigDirectoryRebindMultiApply{}, fmt.Errorf("%w: parent ID 不是稳定派生值", ErrRuntimeConfigDirectoryRebindMultiApplyConflict)
	}
	if p.BoundaryStatus != InvocationWaitingTool && p.BoundaryStatus != InvocationWaitingApproval && p.BoundaryStatus != InvocationWaitingUser {
		return RuntimeConfigDirectoryRebindMultiApply{}, fmt.Errorf("%w: boundary 不受支持", ErrInvalidRuntimeConfigDirectoryRebindMultiApply)
	}
	if len(p.ID) > MaxRuntimeConfigDirectoryRebindMultiApplyIDLength || len(p.PlanID) > MaxRuntimeConfigDirectoryRebindApplyPlanIDLength || len(p.ConfirmationID) > MaxRuntimeConfigDirectoryRebindMultiConfirmationIDLength || len(p.Source) > MaxRuntimeConfigDirectorySourceLength || len(p.InvocationID) > MaxRuntimeConfigDirectoryRebindInvocationID || len(p.IdempotencyKey) > MaxRuntimeConfigDirectoryIdempotency {
		return RuntimeConfigDirectoryRebindMultiApply{}, fmt.Errorf("%w: parent metadata 超出长度限制", ErrInvalidRuntimeConfigDirectoryRebindMultiApply)
	}
	if p.Status == "" {
		p.Status = RuntimeConfigDirectoryRebindMultiApplyQueued
	}
	if !p.Status.valid() || p.PendingCount < 0 || p.ProcessingCount < 0 || p.CompletedCount < 0 || p.FailedCount < 0 || p.PendingCount+p.ProcessingCount+p.CompletedCount+p.FailedCount != len(p.Destinations) {
		return RuntimeConfigDirectoryRebindMultiApply{}, fmt.Errorf("%w: parent status/count 无效", ErrInvalidRuntimeConfigDirectoryRebindMultiApply)
	}
	if len(p.LastError) > MaxRuntimeConfigDirectoryRebindMultiApplyErrorLength {
		p.LastError = p.LastError[:MaxRuntimeConfigDirectoryRebindMultiApplyErrorLength]
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
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
	if p.Revision <= 0 {
		p.Revision = 1
	}
	if p.Status == RuntimeConfigDirectoryRebindMultiApplyCompleted {
		if p.CompletedCount != len(p.Destinations) || p.CompletedAt == nil {
			return RuntimeConfigDirectoryRebindMultiApply{}, fmt.Errorf("%w: completed parent count/time 无效", ErrInvalidRuntimeConfigDirectoryRebindMultiApply)
		}
		value := p.CompletedAt.UTC()
		p.CompletedAt = &value
	} else if p.CompletedAt != nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, fmt.Errorf("%w: 非 completed parent 不能带 completed_at", ErrInvalidRuntimeConfigDirectoryRebindMultiApply)
	}
	return p, nil
}

func (p RuntimeConfigDirectoryRebindMultiApply) MatchesIdentity(other RuntimeConfigDirectoryRebindMultiApply) bool {
	left, leftErr := p.Normalize(time.Time{})
	right, rightErr := other.Normalize(time.Time{})
	if leftErr != nil || rightErr != nil {
		return false
	}
	return left.ID == right.ID && left.PlanID == right.PlanID && left.ConfirmationID == right.ConfirmationID && left.ConfirmationDigest == right.ConfirmationDigest && left.Source == right.Source && left.InvocationID == right.InvocationID && left.BoundaryStatus == right.BoundaryStatus && left.ConfigSnapshotDigest == right.ConfigSnapshotDigest && left.CapabilitiesDigest == right.CapabilitiesDigest && left.SnapshotRevision == right.SnapshotRevision && left.EventSequence == right.EventSequence && left.SnapshotDigest == right.SnapshotDigest && equalRuntimeConfigDirectoryStrings(left.Destinations, right.Destinations) && equalRuntimeConfigDirectoryStrings(left.ChildApplyIDs, right.ChildApplyIDs) && left.IdempotencyKey == right.IdempotencyKey
}

func runtimeConfigDirectoryRebindMultiApplyDerivedStatus(pending, processing, completed, failed int) RuntimeConfigDirectoryRebindMultiApplyStatus {
	total := pending + processing + completed + failed
	if total > 0 && completed == total {
		return RuntimeConfigDirectoryRebindMultiApplyCompleted
	}
	if pending == 0 && processing == 0 && failed > 0 {
		return RuntimeConfigDirectoryRebindMultiApplyFailed
	}
	if completed > 0 || failed > 0 {
		return RuntimeConfigDirectoryRebindMultiApplyPartial
	}
	if processing > 0 {
		return RuntimeConfigDirectoryRebindMultiApplyProcessing
	}
	return RuntimeConfigDirectoryRebindMultiApplyQueued
}

// RuntimeConfigDirectoryRebindMultiApplyStatusForCounts derives the parent
// state from an observed child aggregate. Repositories use this same rule so
// Memory and SQLite cannot disagree after a restart.
func RuntimeConfigDirectoryRebindMultiApplyStatusForCounts(pending, processing, completed, failed int) RuntimeConfigDirectoryRebindMultiApplyStatus {
	return runtimeConfigDirectoryRebindMultiApplyDerivedStatus(pending, processing, completed, failed)
}

func NewRuntimeConfigDirectoryRebindMultiApply(plan RuntimeConfigDirectoryRebindPlan, confirmation RuntimeConfigDirectoryRebindMultiConfirmation, parentID string, snapshot RuntimeSnapshot, eventSequence int64, idempotencyKey string, now time.Time) (RuntimeConfigDirectoryRebindMultiApply, []RuntimeConfigDirectoryRebindApply, error) {
	plan, err := plan.Normalize(now)
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, nil, err
	}
	confirmation, err = confirmation.Normalize(now)
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, nil, err
	}
	if len(plan.Destinations) < 2 || len(plan.Destinations) != len(plan.Targets) {
		return RuntimeConfigDirectoryRebindMultiApply{}, nil, fmt.Errorf("%w: plan 不是多目标", ErrInvalidRuntimeConfigDirectoryRebindMultiApply)
	}
	if confirmation.Status != RuntimeConfigDirectoryRebindMultiConfirmationConfirmed || confirmation.PlanID != plan.ID || confirmation.InvocationID != plan.InvocationID || confirmation.Source != plan.Source || confirmation.ConfigSnapshotDigest != plan.ConfigSnapshotDigest || !equalRuntimeConfigDirectoryStrings(confirmation.Destinations, plan.Destinations) || confirmation.CapabilitiesDigest != plan.CapabilitiesDigest {
		return RuntimeConfigDirectoryRebindMultiApply{}, nil, fmt.Errorf("%w: confirmation 与 plan 不一致", ErrRuntimeConfigDirectoryRebindMultiConfirmationConflict)
	}
	parentID = strings.TrimSpace(parentID)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if parentID == "" || idempotencyKey == "" {
		return RuntimeConfigDirectoryRebindMultiApply{}, nil, fmt.Errorf("%w: parent/idempotency 不能为空", ErrInvalidRuntimeConfigDirectoryRebindMultiApply)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if err := snapshot.Validate(); err != nil || snapshot.InvocationID != plan.InvocationID {
		return RuntimeConfigDirectoryRebindMultiApply{}, nil, fmt.Errorf("%w: snapshot 与 plan 不一致", ErrRuntimeConfigDirectoryRebindMultiApplyConflict)
	}
	if eventSequence < 0 {
		return RuntimeConfigDirectoryRebindMultiApply{}, nil, fmt.Errorf("%w: event_sequence 不能为负数", ErrInvalidRuntimeConfigDirectoryRebindMultiApply)
	}
	projection, err := NewRuntimeCheckpointProjection(snapshot, eventSequence)
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, nil, err
	}
	snapshotDigest, err := RuntimeCheckpointProjectionDigest(projection)
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, nil, err
	}
	confirmationDigest, err := RuntimeConfigDirectoryRebindMultiConfirmationDigest(confirmation)
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, nil, err
	}
	expectedParentID := RuntimeConfigDirectoryRebindMultiApplyID(plan.ID, confirmation.ID, snapshot.Revision, eventSequence, snapshotDigest, idempotencyKey, plan.Destinations)
	if parentID != expectedParentID {
		return RuntimeConfigDirectoryRebindMultiApply{}, nil, fmt.Errorf("%w: parent ID 不是稳定派生值", ErrRuntimeConfigDirectoryRebindMultiApplyConflict)
	}
	children := make([]RuntimeConfigDirectoryRebindApply, len(plan.Destinations))
	childIDs := make([]string, len(plan.Destinations))
	for index, destination := range plan.Destinations {
		childID := RuntimeConfigDirectoryRebindMultiApplyChildID(parentID, destination)
		childIDs[index] = childID
		child := RuntimeConfigDirectoryRebindApply{
			ID: childID, ParentID: parentID, PlanID: plan.ID, ConfirmationID: confirmation.ID, ConfirmationDigest: confirmationDigest,
			Source: plan.Source, Destination: destination, InvocationID: plan.InvocationID, BoundaryStatus: plan.BoundaryStatus,
			ConfigSnapshotDigest: plan.ConfigSnapshotDigest, SnapshotRevision: snapshot.Revision, EventSequence: eventSequence,
			SnapshotDigest: snapshotDigest, Projection: projection, Status: RuntimeConfigDirectoryRebindApplyQueued, Attempt: 0, Revision: 1,
			AvailableAt: now, CreatedAt: now, UpdatedAt: now, IdempotencyKey: idempotencyKey,
		}
		children[index], err = child.Normalize(now)
		if err != nil {
			return RuntimeConfigDirectoryRebindMultiApply{}, nil, err
		}
	}
	parent := RuntimeConfigDirectoryRebindMultiApply{
		ID: parentID, PlanID: plan.ID, ConfirmationID: confirmation.ID, ConfirmationDigest: confirmationDigest,
		Source: plan.Source, InvocationID: plan.InvocationID, BoundaryStatus: plan.BoundaryStatus,
		ConfigSnapshotDigest: plan.ConfigSnapshotDigest, CapabilitiesDigest: plan.CapabilitiesDigest,
		SnapshotRevision: snapshot.Revision, EventSequence: eventSequence, SnapshotDigest: snapshotDigest,
		Destinations: append([]string(nil), plan.Destinations...), ChildApplyIDs: childIDs,
		Status: RuntimeConfigDirectoryRebindMultiApplyQueued, PendingCount: len(childIDs), Revision: 1,
		CreatedAt: now, UpdatedAt: now, IdempotencyKey: idempotencyKey,
	}
	parent, err = parent.Normalize(now)
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, nil, err
	}
	return parent, children, nil
}

type RuntimeConfigDirectoryRebindMultiConfirmationRepository interface {
	CreateRuntimeConfigDirectoryRebindMultiConfirmation(context.Context, RuntimeConfigDirectoryRebindPlan, string, time.Time) (RuntimeConfigDirectoryRebindMultiConfirmation, error)
	GetRuntimeConfigDirectoryRebindMultiConfirmation(context.Context, string) (RuntimeConfigDirectoryRebindMultiConfirmation, error)
}

type RuntimeConfigDirectoryRebindMultiApplyCommitRepository interface {
	CommitRuntimeConfigDirectoryRebindMultiApply(context.Context, string, RuntimeConfigDirectoryRebindMultiApply, []RuntimeConfigDirectoryRebindApply, time.Time) (RuntimeConfigDirectoryRebindMultiApply, error)
}

type RuntimeConfigDirectoryRebindMultiApplyRepository interface {
	GetRuntimeConfigDirectoryRebindMultiApply(context.Context, string) (RuntimeConfigDirectoryRebindMultiApply, error)
	ListRuntimeConfigDirectoryRebindMultiApplies(context.Context, string, string, RuntimeConfigDirectoryRebindMultiApplyStatus, int) ([]RuntimeConfigDirectoryRebindMultiApply, error)
	ReconcileRuntimeConfigDirectoryRebindMultiApply(context.Context, string, time.Time) (RuntimeConfigDirectoryRebindMultiApply, error)
}

// Release is used when a child is claimed before the route registry lookup
// finds a transport.  It returns the item to queued without spending an
// attempt, preserving the explicit "missing route stays queued" contract.
type RuntimeConfigDirectoryRebindApplyReleaseRepository interface {
	ReleaseRuntimeConfigDirectoryRebindApply(context.Context, string, string, time.Time) (bool, error)
}

func runtimeConfigDirectoryRebindMultiApplyParentIDForChild(item RuntimeConfigDirectoryRebindApply) string {
	return strings.TrimSpace(item.ParentID)
}

func runtimeConfigDirectoryRebindMultiApplyChildStatus(item RuntimeConfigDirectoryRebindApply) RuntimeConfigDirectoryRebindApplyStatus {
	return item.Status
}

func runtimeConfigDirectoryRebindMultiApplySort(items []RuntimeConfigDirectoryRebindMultiApply) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
			if items[i].CreatedAt.Equal(items[j].CreatedAt) {
				return items[i].ID < items[j].ID
			}
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		}
		return items[i].UpdatedAt.Before(items[j].UpdatedAt)
	})
}
