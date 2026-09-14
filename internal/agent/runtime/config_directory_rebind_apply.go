package runtime

// This file defines the receipt-backed, single-target rebind apply boundary.
// A rebind apply carries only the provider-neutral checkpoint projection; the
// source invocation, session contents, prompt, attachments, tools and secrets
// remain local to their owning Runtime.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	RuntimeConfigDirectoryRebindConfirmationVersion  = 1
	RuntimeConfigDirectoryRebindApplyEnvelopeVersion = 1
	RuntimeConfigDirectoryRebindApplyReceiptVersion  = 1

	MaxRuntimeConfigDirectoryRebindConfirmationIDLength    = 240
	MaxRuntimeConfigDirectoryRebindConfirmationOwnerLength = 160
	MaxRuntimeConfigDirectoryRebindConfirmationKeyLength   = MaxRuntimeConfigDirectoryIdempotency
	MaxRuntimeConfigDirectoryRebindConfirmationLease       = 24 * time.Hour
	DefaultRuntimeConfigDirectoryRebindConfirmationTTL     = 10 * time.Minute

	MaxRuntimeConfigDirectoryRebindApplyIDLength             = 260
	MaxRuntimeConfigDirectoryRebindApplyPlanIDLength         = MaxRuntimeConfigDirectoryRebindID
	MaxRuntimeConfigDirectoryRebindApplyConfirmationIDLength = MaxRuntimeConfigDirectoryRebindConfirmationIDLength
	MaxRuntimeConfigDirectoryRebindApplyOwnerLength          = 160
	MaxRuntimeConfigDirectoryRebindApplyErrorLength          = 4096
	MaxRuntimeConfigDirectoryRebindApplyAttempts             = 8
	MaxRuntimeConfigDirectoryRebindApplyLease                = 10 * time.Minute
	MaxRuntimeConfigDirectoryRebindApplyEnvelopeBytes        = 64 << 10
	MaxRuntimeConfigDirectoryRebindApplyProjectionBytes      = MaxRuntimeCheckpointDeliveryProjectionBytes
	MaxRuntimeConfigDirectoryRebindApplyResponseBytes        = 96 << 10
	DefaultRuntimeConfigDirectoryRebindApplyMaxAge           = 10 * time.Minute
	DefaultRuntimeConfigDirectoryRebindApplyFutureSkew       = 30 * time.Second
)

var (
	ErrInvalidRuntimeConfigDirectoryRebindConfirmation  = errors.New("Runtime 配置目录 rebind 确认无效")
	ErrRuntimeConfigDirectoryRebindConfirmationExpired  = errors.New("Runtime 配置目录 rebind 确认已过期")
	ErrRuntimeConfigDirectoryRebindConfirmationConflict = errors.New("Runtime 配置目录 rebind 确认冲突")
	ErrRuntimeConfigDirectoryRebindApplyUnavailable     = errors.New("Runtime 配置目录 rebind apply 未配置")
	ErrInvalidRuntimeConfigDirectoryRebindApply         = errors.New("Runtime 配置目录 rebind apply 无效")
	ErrRuntimeConfigDirectoryRebindApplyConflict        = errors.New("Runtime 配置目录 rebind apply 冲突")
	ErrRuntimeConfigDirectoryRebindApplyAuth            = errors.New("Runtime 配置目录 rebind apply 认证失败")
	ErrRuntimeConfigDirectoryRebindApplyStale           = errors.New("Runtime 配置目录 rebind apply 已过期")
)

// RuntimeConfigDirectoryRebindConfirmation is a durable record of an
// explicit user confirmation. The idempotency key is private: it is used only
// to derive the stable confirmation ID and is never returned in API JSON.
type RuntimeConfigDirectoryRebindConfirmation struct {
	ID                   string                                         `json:"id"`
	PlanID               string                                         `json:"plan_id"`
	InvocationID         string                                         `json:"invocation_id"`
	Source               string                                         `json:"source"`
	Destination          string                                         `json:"destination"`
	ConfigSnapshotDigest string                                         `json:"config_snapshot_digest"`
	CapabilitiesDigest   string                                         `json:"capabilities_digest"`
	Status               RuntimeConfigDirectoryRebindConfirmationStatus `json:"status"`
	Revision             int64                                          `json:"revision"`
	ExpiresAt            time.Time                                      `json:"expires_at"`
	CreatedAt            time.Time                                      `json:"created_at"`
	ConfirmedAt          time.Time                                      `json:"confirmed_at"`
	ConsumedAt           *time.Time                                     `json:"consumed_at,omitempty"`
	IdempotencyKey       string                                         `json:"-"`
}

type RuntimeConfigDirectoryRebindConfirmationStatus string

const (
	RuntimeConfigDirectoryRebindConfirmationConfirmed RuntimeConfigDirectoryRebindConfirmationStatus = "confirmed"
	RuntimeConfigDirectoryRebindConfirmationConsumed  RuntimeConfigDirectoryRebindConfirmationStatus = "consumed"
	RuntimeConfigDirectoryRebindConfirmationExpired   RuntimeConfigDirectoryRebindConfirmationStatus = "expired"
)

func (s RuntimeConfigDirectoryRebindConfirmationStatus) valid() bool {
	return s == RuntimeConfigDirectoryRebindConfirmationConfirmed || s == RuntimeConfigDirectoryRebindConfirmationConsumed || s == RuntimeConfigDirectoryRebindConfirmationExpired
}

func RuntimeConfigDirectoryRebindConfirmationID(planID, idempotencyKey string) string {
	material := strings.TrimSpace(planID) + "\x00" + strings.TrimSpace(idempotencyKey)
	sum := sha256.Sum256([]byte(material))
	return "runtime-rebind-confirmation-" + hex.EncodeToString(sum[:16])
}

func cloneRuntimeConfigDirectoryRebindConfirmation(item RuntimeConfigDirectoryRebindConfirmation) RuntimeConfigDirectoryRebindConfirmation {
	item.ExpiresAt = item.ExpiresAt.UTC()
	item.CreatedAt = item.CreatedAt.UTC()
	item.ConfirmedAt = item.ConfirmedAt.UTC()
	if item.ConsumedAt != nil {
		value := item.ConsumedAt.UTC()
		item.ConsumedAt = &value
	}
	return item
}

func (c RuntimeConfigDirectoryRebindConfirmation) Normalize(now time.Time) (RuntimeConfigDirectoryRebindConfirmation, error) {
	c.ID = strings.TrimSpace(c.ID)
	c.PlanID = strings.TrimSpace(c.PlanID)
	c.InvocationID = strings.TrimSpace(c.InvocationID)
	c.Source = strings.TrimSpace(c.Source)
	c.Destination = strings.TrimSpace(c.Destination)
	c.ConfigSnapshotDigest = strings.TrimSpace(strings.ToLower(c.ConfigSnapshotDigest))
	c.CapabilitiesDigest = strings.TrimSpace(strings.ToLower(c.CapabilitiesDigest))
	c.Status = RuntimeConfigDirectoryRebindConfirmationStatus(strings.TrimSpace(strings.ToLower(string(c.Status))))
	c.IdempotencyKey = strings.TrimSpace(c.IdempotencyKey)
	if c.ID == "" || c.PlanID == "" || c.InvocationID == "" || c.Source == "" || c.Destination == "" || c.IdempotencyKey == "" || !runtimeConfigSnapshotDigestValid(c.ConfigSnapshotDigest) || !isRuntimeEventDeliveryDigest(strings.TrimPrefix(c.CapabilitiesDigest, "sha256:")) {
		return RuntimeConfigDirectoryRebindConfirmation{}, fmt.Errorf("%w: confirmation identity 不完整", ErrInvalidRuntimeConfigDirectoryRebindConfirmation)
	}
	if c.ID != RuntimeConfigDirectoryRebindConfirmationID(c.PlanID, c.IdempotencyKey) {
		return RuntimeConfigDirectoryRebindConfirmation{}, fmt.Errorf("%w: confirmation ID 不是稳定派生值", ErrRuntimeConfigDirectoryRebindConfirmationConflict)
	}
	if len(c.ID) > MaxRuntimeConfigDirectoryRebindConfirmationIDLength || len(c.PlanID) > MaxRuntimeConfigDirectoryRebindID || len(c.InvocationID) > MaxRuntimeConfigDirectoryRebindInvocationID || len(c.Source) > MaxRuntimeConfigDirectorySourceLength || len(c.Destination) > MaxRuntimeConfigDirectoryTargetLength || len(c.IdempotencyKey) > MaxRuntimeConfigDirectoryRebindConfirmationKeyLength {
		return RuntimeConfigDirectoryRebindConfirmation{}, fmt.Errorf("%w: confirmation metadata 超出长度限制", ErrInvalidRuntimeConfigDirectoryRebindConfirmation)
	}
	if !c.Status.valid() {
		return RuntimeConfigDirectoryRebindConfirmation{}, fmt.Errorf("%w: confirmation status 不受支持", ErrInvalidRuntimeConfigDirectoryRebindConfirmation)
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
		return RuntimeConfigDirectoryRebindConfirmation{}, fmt.Errorf("%w: confirmation TTL 无效", ErrInvalidRuntimeConfigDirectoryRebindConfirmation)
	}
	switch c.Status {
	case RuntimeConfigDirectoryRebindConfirmationConfirmed:
		if c.ConsumedAt != nil {
			return RuntimeConfigDirectoryRebindConfirmation{}, fmt.Errorf("%w: confirmed confirmation 不能带 consumed_at", ErrInvalidRuntimeConfigDirectoryRebindConfirmation)
		}
	case RuntimeConfigDirectoryRebindConfirmationConsumed:
		if c.ConsumedAt == nil || c.ConsumedAt.IsZero() {
			return RuntimeConfigDirectoryRebindConfirmation{}, fmt.Errorf("%w: consumed confirmation 缺少 consumed_at", ErrInvalidRuntimeConfigDirectoryRebindConfirmation)
		}
		value := c.ConsumedAt.UTC()
		c.ConsumedAt = &value
	case RuntimeConfigDirectoryRebindConfirmationExpired:
		if c.ConsumedAt != nil {
			return RuntimeConfigDirectoryRebindConfirmation{}, fmt.Errorf("%w: expired confirmation 不能带 consumed_at", ErrInvalidRuntimeConfigDirectoryRebindConfirmation)
		}
	}
	return c, nil
}

func NewRuntimeConfigDirectoryRebindConfirmation(plan RuntimeConfigDirectoryRebindPlan, idempotencyKey string, now time.Time) (RuntimeConfigDirectoryRebindConfirmation, error) {
	plan, err := plan.Normalize(now)
	if err != nil {
		return RuntimeConfigDirectoryRebindConfirmation{}, err
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return RuntimeConfigDirectoryRebindConfirmation{}, fmt.Errorf("%w: confirmation 必须提供幂等键", ErrInvalidRuntimeConfigDirectoryRebindConfirmation)
	}
	if len(plan.Destinations) != 1 || len(plan.Targets) != 1 {
		return RuntimeConfigDirectoryRebindConfirmation{}, fmt.Errorf("%w: apply 首版只支持单目标 plan", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	item := RuntimeConfigDirectoryRebindConfirmation{
		ID: RuntimeConfigDirectoryRebindConfirmationID(plan.ID, idempotencyKey), PlanID: plan.ID,
		InvocationID: plan.InvocationID, Source: plan.Source, Destination: plan.Destinations[0],
		ConfigSnapshotDigest: plan.ConfigSnapshotDigest, CapabilitiesDigest: plan.CapabilitiesDigest,
		Status: RuntimeConfigDirectoryRebindConfirmationConfirmed, Revision: 1,
		ExpiresAt: now.Add(DefaultRuntimeConfigDirectoryRebindConfirmationTTL), CreatedAt: now, ConfirmedAt: now,
		IdempotencyKey: idempotencyKey,
	}
	return item.Normalize(now)
}

func RuntimeConfigDirectoryRebindConfirmationDigest(item RuntimeConfigDirectoryRebindConfirmation) (string, error) {
	normalized, err := item.Normalize(time.Time{})
	if err != nil {
		return "", err
	}
	material := struct {
		ID                   string `json:"id"`
		PlanID               string `json:"plan_id"`
		InvocationID         string `json:"invocation_id"`
		Source               string `json:"source"`
		Destination          string `json:"destination"`
		ConfigSnapshotDigest string `json:"config_snapshot_digest"`
		CapabilitiesDigest   string `json:"capabilities_digest"`
	}{normalized.ID, normalized.PlanID, normalized.InvocationID, normalized.Source, normalized.Destination, normalized.ConfigSnapshotDigest, normalized.CapabilitiesDigest}
	encoded, err := json.Marshal(material)
	if err != nil {
		return "", fmt.Errorf("%w: confirmation digest 编码失败", ErrInvalidRuntimeConfigDirectoryRebindConfirmation)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// RuntimeConfigDirectoryRebindApply is a source-owned durable work item. The
// checkpoint projection is private and only reaches an explicitly configured
// destination transport.
type RuntimeConfigDirectoryRebindApply struct {
	ID                   string                                  `json:"id"`
	ParentID             string                                  `json:"parent_id,omitempty"`
	PlanID               string                                  `json:"plan_id"`
	ConfirmationID       string                                  `json:"confirmation_id"`
	ConfirmationDigest   string                                  `json:"confirmation_digest"`
	Source               string                                  `json:"source"`
	Destination          string                                  `json:"destination"`
	InvocationID         string                                  `json:"invocation_id"`
	BoundaryStatus       InvocationStatus                        `json:"boundary_status"`
	ConfigSnapshotDigest string                                  `json:"config_snapshot_digest"`
	SnapshotRevision     int64                                   `json:"snapshot_revision"`
	EventSequence        int64                                   `json:"event_sequence"`
	SnapshotDigest       string                                  `json:"snapshot_digest"`
	Projection           RuntimeCheckpointProjection             `json:"-"`
	Status               RuntimeConfigDirectoryRebindApplyStatus `json:"status"`
	Attempt              int                                     `json:"attempt"`
	Revision             int64                                   `json:"revision"`
	AvailableAt          time.Time                               `json:"available_at"`
	LeaseOwner           string                                  `json:"-"`
	LeaseExpiresAt       *time.Time                              `json:"-"`
	LastError            string                                  `json:"last_error,omitempty"`
	CreatedAt            time.Time                               `json:"created_at"`
	UpdatedAt            time.Time                               `json:"updated_at"`
	CompletedAt          *time.Time                              `json:"completed_at,omitempty"`
	IdempotencyKey       string                                  `json:"-"`
}

type RuntimeConfigDirectoryRebindApplyStatus string

const (
	RuntimeConfigDirectoryRebindApplyQueued     RuntimeConfigDirectoryRebindApplyStatus = "queued"
	RuntimeConfigDirectoryRebindApplyProcessing RuntimeConfigDirectoryRebindApplyStatus = "processing"
	RuntimeConfigDirectoryRebindApplyCompleted  RuntimeConfigDirectoryRebindApplyStatus = "completed"
	RuntimeConfigDirectoryRebindApplyFailed     RuntimeConfigDirectoryRebindApplyStatus = "failed"
)

func (s RuntimeConfigDirectoryRebindApplyStatus) terminal() bool {
	return s == RuntimeConfigDirectoryRebindApplyCompleted || s == RuntimeConfigDirectoryRebindApplyFailed
}

func RuntimeConfigDirectoryRebindApplyID(planID, destination string, snapshotRevision, eventSequence int64, snapshotDigest, idempotencyKey string) string {
	material := strings.Join([]string{strings.TrimSpace(planID), strings.TrimSpace(destination), strconv.FormatInt(snapshotRevision, 10), strconv.FormatInt(eventSequence, 10), strings.TrimSpace(snapshotDigest), strings.TrimSpace(idempotencyKey)}, "\x00")
	sum := sha256.Sum256([]byte(material))
	return "runtime-rebind-apply-" + hex.EncodeToString(sum[:16])
}

func cloneRuntimeConfigDirectoryRebindApply(item RuntimeConfigDirectoryRebindApply) RuntimeConfigDirectoryRebindApply {
	item.Projection = cloneRuntimeCheckpointProjection(item.Projection)
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

func (a RuntimeConfigDirectoryRebindApply) Normalize(now time.Time) (RuntimeConfigDirectoryRebindApply, error) {
	a.ID = strings.TrimSpace(a.ID)
	a.ParentID = strings.TrimSpace(a.ParentID)
	a.PlanID = strings.TrimSpace(a.PlanID)
	a.ConfirmationID = strings.TrimSpace(a.ConfirmationID)
	a.ConfirmationDigest = strings.TrimSpace(strings.ToLower(a.ConfirmationDigest))
	a.Source = strings.TrimSpace(a.Source)
	a.Destination = strings.TrimSpace(a.Destination)
	a.InvocationID = strings.TrimSpace(a.InvocationID)
	a.BoundaryStatus = InvocationStatus(strings.TrimSpace(strings.ToLower(string(a.BoundaryStatus))))
	a.ConfigSnapshotDigest = strings.TrimSpace(strings.ToLower(a.ConfigSnapshotDigest))
	a.SnapshotDigest = strings.TrimSpace(strings.ToLower(a.SnapshotDigest))
	a.LeaseOwner = strings.TrimSpace(a.LeaseOwner)
	a.LastError = SanitizeRuntimeString(strings.TrimSpace(a.LastError))
	a.IdempotencyKey = strings.TrimSpace(a.IdempotencyKey)
	if a.ID == "" || a.PlanID == "" || a.ConfirmationID == "" || a.ConfirmationDigest == "" || a.Source == "" || a.Destination == "" || a.InvocationID == "" || a.ConfigSnapshotDigest == "" || a.SnapshotRevision <= 0 || a.EventSequence < 0 || a.SnapshotDigest == "" || a.IdempotencyKey == "" {
		return RuntimeConfigDirectoryRebindApply{}, fmt.Errorf("%w: apply identity 不完整", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	expectedID := RuntimeConfigDirectoryRebindApplyID(a.PlanID, a.Destination, a.SnapshotRevision, a.EventSequence, a.SnapshotDigest, a.IdempotencyKey)
	if a.ParentID != "" {
		expectedID = RuntimeConfigDirectoryRebindMultiApplyChildID(a.ParentID, a.Destination)
	}
	if a.ID != expectedID {
		return RuntimeConfigDirectoryRebindApply{}, fmt.Errorf("%w: apply ID 不是稳定派生值", ErrRuntimeConfigDirectoryRebindApplyConflict)
	}
	if !runtimeConfigSnapshotDigestValid(a.ConfigSnapshotDigest) || !isRuntimeEventDeliveryDigest(a.SnapshotDigest) || !isRuntimeEventDeliveryDigest(a.ConfirmationDigest) {
		return RuntimeConfigDirectoryRebindApply{}, fmt.Errorf("%w: apply digest 无效", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	if a.BoundaryStatus != InvocationWaitingTool && a.BoundaryStatus != InvocationWaitingApproval && a.BoundaryStatus != InvocationWaitingUser {
		return RuntimeConfigDirectoryRebindApply{}, fmt.Errorf("%w: apply boundary 不受支持", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	if len(a.ID) > MaxRuntimeConfigDirectoryRebindApplyIDLength || len(a.ParentID) > MaxRuntimeConfigDirectoryRebindApplyIDLength || len(a.PlanID) > MaxRuntimeConfigDirectoryRebindApplyPlanIDLength || len(a.ConfirmationID) > MaxRuntimeConfigDirectoryRebindApplyConfirmationIDLength || len(a.Source) > MaxRuntimeConfigDirectorySourceLength || len(a.Destination) > MaxRuntimeConfigDirectoryTargetLength || len(a.InvocationID) > MaxRuntimeConfigDirectoryRebindInvocationID || len(a.IdempotencyKey) > MaxRuntimeConfigDirectoryIdempotency || len(a.LeaseOwner) > MaxRuntimeConfigDirectoryRebindApplyOwnerLength {
		return RuntimeConfigDirectoryRebindApply{}, fmt.Errorf("%w: apply metadata 超出长度限制", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	if a.Attempt < 0 || a.Attempt > MaxRuntimeConfigDirectoryRebindApplyAttempts || a.Revision < 0 {
		return RuntimeConfigDirectoryRebindApply{}, fmt.Errorf("%w: apply attempt/revision 无效", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	if err := a.Projection.Validate(); err != nil {
		return RuntimeConfigDirectoryRebindApply{}, err
	}
	if a.Projection.InvocationID != a.InvocationID || a.Projection.SnapshotRevision != a.SnapshotRevision || a.Projection.EventSequence != a.EventSequence {
		return RuntimeConfigDirectoryRebindApply{}, fmt.Errorf("%w: projection identity 不一致", ErrRuntimeConfigDirectoryRebindApplyAuth)
	}
	digest, err := RuntimeCheckpointProjectionDigest(a.Projection)
	if err != nil || digest != a.SnapshotDigest {
		return RuntimeConfigDirectoryRebindApply{}, fmt.Errorf("%w: projection digest 不一致", ErrRuntimeConfigDirectoryRebindApplyAuth)
	}
	if a.Status == "" {
		a.Status = RuntimeConfigDirectoryRebindApplyQueued
	}
	switch a.Status {
	case RuntimeConfigDirectoryRebindApplyQueued, RuntimeConfigDirectoryRebindApplyProcessing, RuntimeConfigDirectoryRebindApplyCompleted, RuntimeConfigDirectoryRebindApplyFailed:
	default:
		return RuntimeConfigDirectoryRebindApply{}, fmt.Errorf("%w: apply status 不受支持", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if a.AvailableAt.IsZero() {
		a.AvailableAt = now
	} else {
		a.AvailableAt = a.AvailableAt.UTC()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	} else {
		a.CreatedAt = a.CreatedAt.UTC()
	}
	if a.UpdatedAt.IsZero() {
		a.UpdatedAt = now
	} else {
		a.UpdatedAt = a.UpdatedAt.UTC()
	}
	if a.Revision <= 0 {
		a.Revision = 1
	}
	if len(a.LastError) > MaxRuntimeConfigDirectoryRebindApplyErrorLength {
		a.LastError = a.LastError[:MaxRuntimeConfigDirectoryRebindApplyErrorLength]
	}
	if a.Status == RuntimeConfigDirectoryRebindApplyProcessing {
		if a.LeaseOwner == "" || a.LeaseExpiresAt == nil || a.LeaseExpiresAt.IsZero() || a.LeaseExpiresAt.After(now.Add(MaxRuntimeConfigDirectoryRebindApplyLease)) {
			return RuntimeConfigDirectoryRebindApply{}, fmt.Errorf("%w: processing apply lease 无效", ErrInvalidRuntimeConfigDirectoryRebindApply)
		}
		expires := a.LeaseExpiresAt.UTC()
		a.LeaseExpiresAt = &expires
	}
	if a.Status == RuntimeConfigDirectoryRebindApplyQueued || a.Status.terminal() {
		a.LeaseOwner = ""
		a.LeaseExpiresAt = nil
	}
	if a.Status == RuntimeConfigDirectoryRebindApplyCompleted {
		if a.CompletedAt == nil {
			finished := a.UpdatedAt
			a.CompletedAt = &finished
		} else {
			finished := a.CompletedAt.UTC()
			a.CompletedAt = &finished
		}
	} else if a.CompletedAt != nil {
		return RuntimeConfigDirectoryRebindApply{}, fmt.Errorf("%w: 非 completed apply 不能带 completed_at", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	return a, nil
}

func NewRuntimeConfigDirectoryRebindApply(plan RuntimeConfigDirectoryRebindPlan, confirmation RuntimeConfigDirectoryRebindConfirmation, snapshot RuntimeSnapshot, eventSequence int64, idempotencyKey string, now time.Time) (RuntimeConfigDirectoryRebindApply, error) {
	plan, err := plan.Normalize(now)
	if err != nil {
		return RuntimeConfigDirectoryRebindApply{}, err
	}
	confirmation, err = confirmation.Normalize(now)
	if err != nil {
		return RuntimeConfigDirectoryRebindApply{}, err
	}
	if len(plan.Destinations) != 1 || len(plan.Targets) != 1 {
		return RuntimeConfigDirectoryRebindApply{}, fmt.Errorf("%w: apply 首版只支持单目标 plan", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	if confirmation.PlanID != plan.ID || confirmation.InvocationID != plan.InvocationID || confirmation.Source != plan.Source || confirmation.Destination != plan.Destinations[0] || confirmation.ConfigSnapshotDigest != plan.ConfigSnapshotDigest || confirmation.Status != RuntimeConfigDirectoryRebindConfirmationConfirmed {
		return RuntimeConfigDirectoryRebindApply{}, fmt.Errorf("%w: confirmation 与 plan 不一致", ErrRuntimeConfigDirectoryRebindConfirmationConflict)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if err := snapshot.Validate(); err != nil || snapshot.InvocationID != plan.InvocationID {
		return RuntimeConfigDirectoryRebindApply{}, fmt.Errorf("%w: snapshot 与 plan 不一致", ErrRuntimeConfigDirectoryRebindApplyConflict)
	}
	if eventSequence < 0 {
		return RuntimeConfigDirectoryRebindApply{}, fmt.Errorf("%w: event_sequence 不能为负数", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	projection, err := NewRuntimeCheckpointProjection(snapshot, eventSequence)
	if err != nil {
		return RuntimeConfigDirectoryRebindApply{}, err
	}
	snapshotDigest, err := RuntimeCheckpointProjectionDigest(projection)
	if err != nil {
		return RuntimeConfigDirectoryRebindApply{}, err
	}
	confirmationDigest, err := RuntimeConfigDirectoryRebindConfirmationDigest(confirmation)
	if err != nil {
		return RuntimeConfigDirectoryRebindApply{}, err
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return RuntimeConfigDirectoryRebindApply{}, fmt.Errorf("%w: apply 必须提供幂等键", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	item := RuntimeConfigDirectoryRebindApply{
		ID:     RuntimeConfigDirectoryRebindApplyID(plan.ID, plan.Destinations[0], snapshot.Revision, eventSequence, snapshotDigest, idempotencyKey),
		PlanID: plan.ID, ConfirmationID: confirmation.ID, ConfirmationDigest: confirmationDigest,
		Source: plan.Source, Destination: plan.Destinations[0], InvocationID: plan.InvocationID,
		BoundaryStatus: plan.BoundaryStatus, ConfigSnapshotDigest: plan.ConfigSnapshotDigest,
		SnapshotRevision: snapshot.Revision, EventSequence: eventSequence, SnapshotDigest: snapshotDigest,
		Projection: projection, Status: RuntimeConfigDirectoryRebindApplyQueued, Attempt: 0, Revision: 1,
		AvailableAt: now, CreatedAt: now, UpdatedAt: now, IdempotencyKey: idempotencyKey,
	}
	return item.Normalize(now)
}

func (a RuntimeConfigDirectoryRebindApply) MatchesIdentity(other RuntimeConfigDirectoryRebindApply) bool {
	left, leftErr := a.Normalize(time.Time{})
	right, rightErr := other.Normalize(time.Time{})
	if leftErr != nil || rightErr != nil {
		return false
	}
	leftProjectionDigest, leftProjectionErr := RuntimeCheckpointProjectionDigest(left.Projection)
	rightProjectionDigest, rightProjectionErr := RuntimeCheckpointProjectionDigest(right.Projection)
	return leftProjectionErr == nil && rightProjectionErr == nil && left.ID == right.ID && left.ParentID == right.ParentID && left.PlanID == right.PlanID && left.ConfirmationID == right.ConfirmationID && left.ConfirmationDigest == right.ConfirmationDigest && left.Source == right.Source && left.Destination == right.Destination && left.InvocationID == right.InvocationID && left.BoundaryStatus == right.BoundaryStatus && left.ConfigSnapshotDigest == right.ConfigSnapshotDigest && left.SnapshotRevision == right.SnapshotRevision && left.EventSequence == right.EventSequence && left.SnapshotDigest == right.SnapshotDigest && left.SnapshotDigest == rightProjectionDigest && leftProjectionDigest == rightProjectionDigest && left.IdempotencyKey == right.IdempotencyKey
}

func RuntimeConfigDirectoryRebindApplyBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

type RuntimeConfigDirectoryRebindApplyEnvelope struct {
	Version              int              `json:"version"`
	Source               string           `json:"source"`
	Destination          string           `json:"destination"`
	PlanID               string           `json:"plan_id"`
	ApplyID              string           `json:"apply_id"`
	ConfirmationID       string           `json:"confirmation_id"`
	ConfirmationDigest   string           `json:"confirmation_digest"`
	InvocationID         string           `json:"invocation_id"`
	BoundaryStatus       InvocationStatus `json:"boundary_status"`
	ConfigSnapshotDigest string           `json:"config_snapshot_digest"`
	SnapshotRevision     int64            `json:"snapshot_revision"`
	EventSequence        int64            `json:"event_sequence"`
	SnapshotDigest       string           `json:"snapshot_digest"`
	Timestamp            time.Time        `json:"timestamp"`
	IssuedAt             time.Time        `json:"issued_at"`
	Signature            string           `json:"signature,omitempty"`
}

func (e RuntimeConfigDirectoryRebindApplyEnvelope) normalize() (RuntimeConfigDirectoryRebindApplyEnvelope, error) {
	e.Source = strings.TrimSpace(e.Source)
	e.Destination = strings.TrimSpace(e.Destination)
	e.PlanID = strings.TrimSpace(e.PlanID)
	e.ApplyID = strings.TrimSpace(e.ApplyID)
	e.ConfirmationID = strings.TrimSpace(e.ConfirmationID)
	e.ConfirmationDigest = strings.TrimSpace(strings.ToLower(e.ConfirmationDigest))
	e.InvocationID = strings.TrimSpace(e.InvocationID)
	e.BoundaryStatus = InvocationStatus(strings.TrimSpace(strings.ToLower(string(e.BoundaryStatus))))
	e.ConfigSnapshotDigest = strings.TrimSpace(strings.ToLower(e.ConfigSnapshotDigest))
	e.SnapshotDigest = strings.TrimSpace(strings.ToLower(e.SnapshotDigest))
	e.Signature = strings.TrimSpace(strings.ToLower(e.Signature))
	if e.Version != RuntimeConfigDirectoryRebindApplyEnvelopeVersion || e.Source == "" || e.Destination == "" || e.PlanID == "" || e.ApplyID == "" || e.ConfirmationID == "" || e.InvocationID == "" || e.SnapshotRevision <= 0 || e.EventSequence < 0 || e.Timestamp.IsZero() || e.IssuedAt.IsZero() || !runtimeConfigSnapshotDigestValid(e.ConfigSnapshotDigest) || !isRuntimeEventDeliveryDigest(e.SnapshotDigest) || !isRuntimeEventDeliveryDigest(e.ConfirmationDigest) {
		return RuntimeConfigDirectoryRebindApplyEnvelope{}, fmt.Errorf("%w: envelope metadata 不完整", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	if e.BoundaryStatus != InvocationWaitingTool && e.BoundaryStatus != InvocationWaitingApproval && e.BoundaryStatus != InvocationWaitingUser {
		return RuntimeConfigDirectoryRebindApplyEnvelope{}, fmt.Errorf("%w: envelope boundary 不受支持", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	if len(e.Source) > MaxRuntimeConfigDirectorySourceLength || len(e.Destination) > MaxRuntimeConfigDirectoryTargetLength || len(e.PlanID) > MaxRuntimeConfigDirectoryRebindID || len(e.ApplyID) > MaxRuntimeConfigDirectoryRebindApplyIDLength || len(e.ConfirmationID) > MaxRuntimeConfigDirectoryRebindConfirmationIDLength || len(e.InvocationID) > MaxRuntimeConfigDirectoryRebindInvocationID {
		return RuntimeConfigDirectoryRebindApplyEnvelope{}, fmt.Errorf("%w: envelope metadata 超出长度限制", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	if e.ApplyID != RuntimeConfigDirectoryRebindApplyID(e.PlanID, e.Destination, e.SnapshotRevision, e.EventSequence, e.SnapshotDigest, "") {
		// ApplyID includes a private idempotency key, so identity is checked by
		// the source queue and by the signed receipt rather than reconstructed
		// from wire data. The prefix/length check still prevents arbitrary IDs.
		if (!strings.HasPrefix(e.ApplyID, "runtime-rebind-apply-") && !strings.HasPrefix(e.ApplyID, "runtime-rebind-multi-child-")) || (len(e.ApplyID) != len("runtime-rebind-apply-")+32 && len(e.ApplyID) != len("runtime-rebind-multi-child-")+32) {
			return RuntimeConfigDirectoryRebindApplyEnvelope{}, fmt.Errorf("%w: apply_id 形状无效", ErrRuntimeConfigDirectoryRebindApplyAuth)
		}
	}
	if e.Signature != "" && !isRuntimeEventDeliveryDigest(e.Signature) {
		return RuntimeConfigDirectoryRebindApplyEnvelope{}, fmt.Errorf("%w: signature 无效", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	e.Timestamp = e.Timestamp.UTC()
	e.IssuedAt = e.IssuedAt.UTC()
	return e, nil
}

func NormalizeRuntimeConfigDirectoryRebindApplyEnvelope(e RuntimeConfigDirectoryRebindApplyEnvelope) (RuntimeConfigDirectoryRebindApplyEnvelope, error) {
	return e.normalize()
}

func (a RuntimeConfigDirectoryRebindApply) Envelope(now time.Time) (RuntimeConfigDirectoryRebindApplyEnvelope, error) {
	normalized, err := a.Normalize(now)
	if err != nil {
		return RuntimeConfigDirectoryRebindApplyEnvelope{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	return RuntimeConfigDirectoryRebindApplyEnvelope{
		Version: RuntimeConfigDirectoryRebindApplyEnvelopeVersion, Source: normalized.Source, Destination: normalized.Destination,
		PlanID: normalized.PlanID, ApplyID: normalized.ID, ConfirmationID: normalized.ConfirmationID, ConfirmationDigest: normalized.ConfirmationDigest,
		InvocationID: normalized.InvocationID, BoundaryStatus: normalized.BoundaryStatus, ConfigSnapshotDigest: normalized.ConfigSnapshotDigest,
		SnapshotRevision: normalized.SnapshotRevision, EventSequence: normalized.EventSequence, SnapshotDigest: normalized.SnapshotDigest,
		Timestamp: normalized.CreatedAt.UTC(), IssuedAt: now,
	}.normalize()
}

func (e RuntimeConfigDirectoryRebindApplyEnvelope) canonicalUnsigned() ([]byte, error) {
	normalized, err := e.normalize()
	if err != nil {
		return nil, err
	}
	normalized.Signature = ""
	return json.Marshal(normalized)
}

func SignRuntimeConfigDirectoryRebindApplyEnvelope(e RuntimeConfigDirectoryRebindApplyEnvelope, secret []byte) (RuntimeConfigDirectoryRebindApplyEnvelope, error) {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return RuntimeConfigDirectoryRebindApplyEnvelope{}, err
	}
	canonical, err := e.canonicalUnsigned()
	if err != nil {
		return RuntimeConfigDirectoryRebindApplyEnvelope{}, err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	e.Signature = hex.EncodeToString(mac.Sum(nil))
	return e.normalize()
}

func VerifyRuntimeConfigDirectoryRebindApplyEnvelope(e RuntimeConfigDirectoryRebindApplyEnvelope, secret []byte) error {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return err
	}
	normalized, err := e.normalize()
	if err != nil {
		return err
	}
	if normalized.Signature == "" {
		return fmt.Errorf("%w: 缺少 signature", ErrRuntimeConfigDirectoryRebindApplyAuth)
	}
	canonical, err := normalized.canonicalUnsigned()
	if err != nil {
		return err
	}
	provided, err := hex.DecodeString(normalized.Signature)
	if err != nil {
		return fmt.Errorf("%w: signature 编码无效", ErrRuntimeConfigDirectoryRebindApplyAuth)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return fmt.Errorf("%w: HMAC 不匹配", ErrRuntimeConfigDirectoryRebindApplyAuth)
	}
	return nil
}

func validateRuntimeConfigDirectoryRebindApplyTimestamp(timestamp, now time.Time, maxAge, futureSkew time.Duration) error {
	if maxAge <= 0 || futureSkew < 0 {
		return fmt.Errorf("%w: timestamp policy 无效", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	timestamp = timestamp.UTC()
	if timestamp.After(now.Add(futureSkew)) {
		return fmt.Errorf("%w: timestamp 位于允许未来窗口之外", ErrRuntimeConfigDirectoryRebindApplyAuth)
	}
	if now.Sub(timestamp) > maxAge {
		return fmt.Errorf("%w: timestamp 已过期", ErrRuntimeConfigDirectoryRebindApplyStale)
	}
	return nil
}

type RuntimeConfigDirectoryRebindApplyRequest struct {
	Envelope   RuntimeConfigDirectoryRebindApplyEnvelope `json:"envelope"`
	Projection RuntimeCheckpointProjection               `json:"projection"`
}

type RuntimeConfigDirectoryRebindApplyReceipt struct {
	Version          int       `json:"version"`
	Source           string    `json:"source"`
	Destination      string    `json:"destination"`
	PlanID           string    `json:"plan_id"`
	ApplyID          string    `json:"apply_id"`
	InvocationID     string    `json:"invocation_id"`
	SnapshotRevision int64     `json:"snapshot_revision"`
	EventSequence    int64     `json:"event_sequence"`
	SnapshotDigest   string    `json:"snapshot_digest"`
	Phase            string    `json:"phase"`
	Duplicate        bool      `json:"duplicate"`
	IssuedAt         time.Time `json:"issued_at"`
	Signature        string    `json:"signature"`
}

const RuntimeConfigDirectoryRebindApplyReceiptAccepted = "accepted"

func (r RuntimeConfigDirectoryRebindApplyReceipt) normalize() (RuntimeConfigDirectoryRebindApplyReceipt, error) {
	r.Source = strings.TrimSpace(r.Source)
	r.Destination = strings.TrimSpace(r.Destination)
	r.PlanID = strings.TrimSpace(r.PlanID)
	r.ApplyID = strings.TrimSpace(r.ApplyID)
	r.InvocationID = strings.TrimSpace(r.InvocationID)
	r.SnapshotDigest = strings.TrimSpace(strings.ToLower(r.SnapshotDigest))
	r.Phase = strings.TrimSpace(strings.ToLower(r.Phase))
	r.Signature = strings.TrimSpace(strings.ToLower(r.Signature))
	if r.Version != RuntimeConfigDirectoryRebindApplyReceiptVersion || r.Source == "" || r.Destination == "" || r.PlanID == "" || r.ApplyID == "" || r.InvocationID == "" || r.SnapshotRevision <= 0 || r.EventSequence < 0 || !isRuntimeEventDeliveryDigest(r.SnapshotDigest) || r.Phase != RuntimeConfigDirectoryRebindApplyReceiptAccepted || r.IssuedAt.IsZero() {
		return RuntimeConfigDirectoryRebindApplyReceipt{}, fmt.Errorf("%w: receipt metadata 不完整", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	if r.Signature != "" && !isRuntimeEventDeliveryDigest(r.Signature) {
		return RuntimeConfigDirectoryRebindApplyReceipt{}, fmt.Errorf("%w: receipt signature 无效", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	r.IssuedAt = r.IssuedAt.UTC()
	return r, nil
}

func (r RuntimeConfigDirectoryRebindApplyReceipt) canonicalUnsigned() ([]byte, error) {
	normalized, err := r.normalize()
	if err != nil {
		return nil, err
	}
	normalized.Signature = ""
	return json.Marshal(normalized)
}

func SignRuntimeConfigDirectoryRebindApplyReceipt(receipt RuntimeConfigDirectoryRebindApplyReceipt, secret []byte) (RuntimeConfigDirectoryRebindApplyReceipt, error) {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return RuntimeConfigDirectoryRebindApplyReceipt{}, err
	}
	canonical, err := receipt.canonicalUnsigned()
	if err != nil {
		return RuntimeConfigDirectoryRebindApplyReceipt{}, err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	receipt.Signature = hex.EncodeToString(mac.Sum(nil))
	return receipt.normalize()
}

func VerifyRuntimeConfigDirectoryRebindApplyReceipt(receipt RuntimeConfigDirectoryRebindApplyReceipt, secret []byte) error {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return err
	}
	normalized, err := receipt.normalize()
	if err != nil {
		return err
	}
	if normalized.Signature == "" {
		return fmt.Errorf("%w: receipt 缺少 signature", ErrRuntimeConfigDirectoryRebindApplyAuth)
	}
	canonical, err := normalized.canonicalUnsigned()
	if err != nil {
		return err
	}
	provided, err := hex.DecodeString(normalized.Signature)
	if err != nil {
		return fmt.Errorf("%w: receipt signature 编码无效", ErrRuntimeConfigDirectoryRebindApplyAuth)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return fmt.Errorf("%w: receipt HMAC 不匹配", ErrRuntimeConfigDirectoryRebindApplyAuth)
	}
	return nil
}

func (r RuntimeConfigDirectoryRebindApplyReceipt) ValidateAgainst(envelope RuntimeConfigDirectoryRebindApplyEnvelope) error {
	envelope, err := envelope.normalize()
	if err != nil {
		return err
	}
	receipt, err := r.normalize()
	if err != nil {
		return err
	}
	if receipt.Source != envelope.Source || receipt.Destination != envelope.Destination || receipt.PlanID != envelope.PlanID || receipt.ApplyID != envelope.ApplyID || receipt.InvocationID != envelope.InvocationID || receipt.SnapshotRevision != envelope.SnapshotRevision || receipt.EventSequence != envelope.EventSequence || receipt.SnapshotDigest != envelope.SnapshotDigest {
		return fmt.Errorf("%w: receipt identity 不一致", ErrRuntimeConfigDirectoryRebindApplyAuth)
	}
	return nil
}

type RuntimeConfigDirectoryRebindInboxRecord struct {
	Version              int                         `json:"version"`
	Source               string                      `json:"source"`
	Destination          string                      `json:"destination"`
	PlanID               string                      `json:"plan_id"`
	ApplyID              string                      `json:"apply_id"`
	ConfirmationID       string                      `json:"confirmation_id"`
	ConfirmationDigest   string                      `json:"confirmation_digest"`
	InvocationID         string                      `json:"invocation_id"`
	BoundaryStatus       InvocationStatus            `json:"boundary_status"`
	ConfigSnapshotDigest string                      `json:"config_snapshot_digest"`
	SnapshotRevision     int64                       `json:"snapshot_revision"`
	EventSequence        int64                       `json:"event_sequence"`
	SnapshotDigest       string                      `json:"snapshot_digest"`
	Projection           RuntimeCheckpointProjection `json:"projection"`
	Signature            string                      `json:"signature"`
	ReceivedAt           time.Time                   `json:"received_at"`
}

func cloneRuntimeConfigDirectoryRebindInboxRecord(item RuntimeConfigDirectoryRebindInboxRecord) RuntimeConfigDirectoryRebindInboxRecord {
	item.Projection = cloneRuntimeCheckpointProjection(item.Projection)
	item.ReceivedAt = item.ReceivedAt.UTC()
	return item
}

func RuntimeConfigDirectoryRebindInboxRecordFromEnvelope(envelope RuntimeConfigDirectoryRebindApplyEnvelope, projection RuntimeCheckpointProjection, receivedAt time.Time) RuntimeConfigDirectoryRebindInboxRecord {
	return RuntimeConfigDirectoryRebindInboxRecord{Version: envelope.Version, Source: envelope.Source, Destination: envelope.Destination, PlanID: envelope.PlanID, ApplyID: envelope.ApplyID, ConfirmationID: envelope.ConfirmationID, ConfirmationDigest: envelope.ConfirmationDigest, InvocationID: envelope.InvocationID, BoundaryStatus: envelope.BoundaryStatus, ConfigSnapshotDigest: envelope.ConfigSnapshotDigest, SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence, SnapshotDigest: envelope.SnapshotDigest, Projection: cloneRuntimeCheckpointProjection(projection), Signature: envelope.Signature, ReceivedAt: receivedAt.UTC()}
}

func (r RuntimeConfigDirectoryRebindInboxRecord) Matches(envelope RuntimeConfigDirectoryRebindApplyEnvelope, projection RuntimeCheckpointProjection) bool {
	storedDigest, storedErr := RuntimeCheckpointProjectionDigest(r.Projection)
	projectionDigest, projectionErr := RuntimeCheckpointProjectionDigest(projection)
	return storedErr == nil && projectionErr == nil && r.Version == envelope.Version && r.Source == envelope.Source && r.Destination == envelope.Destination && r.PlanID == envelope.PlanID && r.ApplyID == envelope.ApplyID && r.ConfirmationID == envelope.ConfirmationID && r.ConfirmationDigest == envelope.ConfirmationDigest && r.InvocationID == envelope.InvocationID && r.BoundaryStatus == envelope.BoundaryStatus && r.ConfigSnapshotDigest == envelope.ConfigSnapshotDigest && r.SnapshotRevision == envelope.SnapshotRevision && r.EventSequence == envelope.EventSequence && r.SnapshotDigest == envelope.SnapshotDigest && storedDigest == projectionDigest
}

func normalizeRuntimeConfigDirectoryRebindApplyPair(envelope RuntimeConfigDirectoryRebindApplyEnvelope, projection RuntimeCheckpointProjection) (RuntimeConfigDirectoryRebindApplyEnvelope, RuntimeCheckpointProjection, error) {
	normalized, err := envelope.normalize()
	if err != nil {
		return RuntimeConfigDirectoryRebindApplyEnvelope{}, RuntimeCheckpointProjection{}, err
	}
	if err := projection.Validate(); err != nil {
		return RuntimeConfigDirectoryRebindApplyEnvelope{}, RuntimeCheckpointProjection{}, err
	}
	encoded, err := json.Marshal(projection)
	if err != nil || len(encoded) > MaxRuntimeConfigDirectoryRebindApplyProjectionBytes {
		return RuntimeConfigDirectoryRebindApplyEnvelope{}, RuntimeCheckpointProjection{}, fmt.Errorf("%w: projection 超过字节上限", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	if projection.InvocationID != normalized.InvocationID || projection.SnapshotRevision != normalized.SnapshotRevision || projection.EventSequence != normalized.EventSequence {
		return RuntimeConfigDirectoryRebindApplyEnvelope{}, RuntimeCheckpointProjection{}, fmt.Errorf("%w: projection identity 不一致", ErrRuntimeConfigDirectoryRebindApplyAuth)
	}
	digest, err := RuntimeCheckpointProjectionDigest(projection)
	if err != nil || digest != normalized.SnapshotDigest {
		return RuntimeConfigDirectoryRebindApplyEnvelope{}, RuntimeCheckpointProjection{}, fmt.Errorf("%w: projection digest 不一致", ErrRuntimeConfigDirectoryRebindApplyAuth)
	}
	return normalized, cloneRuntimeCheckpointProjection(projection), nil
}

func NormalizeRuntimeConfigDirectoryRebindApplyPair(envelope RuntimeConfigDirectoryRebindApplyEnvelope, projection RuntimeCheckpointProjection) (RuntimeConfigDirectoryRebindApplyEnvelope, RuntimeCheckpointProjection, error) {
	return normalizeRuntimeConfigDirectoryRebindApplyPair(envelope, projection)
}

type RuntimeConfigDirectoryRebindConfirmationRepository interface {
	CreateRuntimeConfigDirectoryRebindConfirmation(context.Context, RuntimeConfigDirectoryRebindPlan, string, time.Time) (RuntimeConfigDirectoryRebindConfirmation, error)
	GetRuntimeConfigDirectoryRebindConfirmation(context.Context, string) (RuntimeConfigDirectoryRebindConfirmation, error)
}

type RuntimeConfigDirectoryRebindApplyRepository interface {
	EnqueueRuntimeConfigDirectoryRebindApply(context.Context, RuntimeConfigDirectoryRebindApply) (RuntimeConfigDirectoryRebindApply, error)
	GetRuntimeConfigDirectoryRebindApply(context.Context, string) (RuntimeConfigDirectoryRebindApply, error)
	ListRuntimeConfigDirectoryRebindApplies(context.Context, string, string, RuntimeConfigDirectoryRebindApplyStatus, int) ([]RuntimeConfigDirectoryRebindApply, error)
	ClaimRuntimeConfigDirectoryRebindApply(context.Context, string, time.Time, time.Duration) (RuntimeConfigDirectoryRebindApply, bool, error)
	CompleteRuntimeConfigDirectoryRebindApply(context.Context, string, string, time.Time) (bool, error)
	RetryRuntimeConfigDirectoryRebindApply(context.Context, string, string, time.Time, string) (bool, error)
	FailRuntimeConfigDirectoryRebindApply(context.Context, string, string, time.Time, string) (bool, error)
}

// RuntimeConfigDirectoryRebindApplyCommitRepository consumes a confirmed
// record and inserts the queued apply in one local transaction/lock.
type RuntimeConfigDirectoryRebindApplyCommitRepository interface {
	CommitRuntimeConfigDirectoryRebindApply(context.Context, string, RuntimeConfigDirectoryRebindApply, time.Time) (RuntimeConfigDirectoryRebindApply, error)
}

type RuntimeConfigDirectoryRebindInbox interface {
	AcceptRuntimeConfigDirectoryRebind(context.Context, RuntimeConfigDirectoryRebindApplyEnvelope, RuntimeCheckpointProjection) (bool, error)
	GetRuntimeConfigDirectoryRebind(context.Context, string) (RuntimeConfigDirectoryRebindInboxRecord, error)
}

type RuntimeConfigDirectoryRebindApplyTransport interface {
	Deliver(context.Context, RuntimeConfigDirectoryRebindApplyEnvelope, RuntimeCheckpointProjection) (RuntimeConfigDirectoryRebindApplyReceipt, error)
}

// RuntimeConfigDirectoryRebindApplyReceiver is an explicitly mounted,
// signed destination endpoint. It persists the projection only; a future
// Worker reattach implementation may consume the durable inbox separately.
type RuntimeConfigDirectoryRebindApplyReceiver struct {
	Source        string
	Destination   string
	SharedSecret  []byte
	Inbox         RuntimeConfigDirectoryRebindInbox
	MaxAge        time.Duration
	MaxFutureSkew time.Duration
	MaxBodyBytes  int64
	MaxResponse   int64
	Now           func() time.Time
}

func NewRuntimeConfigDirectoryRebindApplyReceiver(source, destination string, secret []byte, inbox RuntimeConfigDirectoryRebindInbox) (*RuntimeConfigDirectoryRebindApplyReceiver, error) {
	if strings.TrimSpace(source) == "" || strings.TrimSpace(destination) == "" {
		return nil, fmt.Errorf("%w: source/destination 无效", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return nil, err
	}
	if inbox == nil {
		return nil, fmt.Errorf("%w: inbox 不能为空", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	return &RuntimeConfigDirectoryRebindApplyReceiver{Source: strings.TrimSpace(source), Destination: strings.TrimSpace(destination), SharedSecret: append([]byte(nil), secret...), Inbox: inbox, MaxAge: DefaultRuntimeConfigDirectoryRebindApplyMaxAge, MaxFutureSkew: DefaultRuntimeConfigDirectoryRebindApplyFutureSkew, MaxBodyBytes: MaxRuntimeConfigDirectoryRebindApplyResponseBytes, MaxResponse: MaxRuntimeConfigDirectoryRebindApplyResponseBytes, Now: time.Now}, nil
}

func (r *RuntimeConfigDirectoryRebindApplyReceiver) Handler() http.Handler { return r }

func (r *RuntimeConfigDirectoryRebindApplyReceiver) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if r == nil {
		writeRuntimeConfigDirectoryRebindApplyError(writer, http.StatusInternalServerError, ErrInvalidRuntimeConfigDirectoryRebindApply)
		return
	}
	if request == nil || request.Method != http.MethodPost {
		writeRuntimeConfigDirectoryRebindApplyError(writer, http.StatusMethodNotAllowed, fmt.Errorf("%w: 只支持 POST", ErrInvalidRuntimeConfigDirectoryRebindApply))
		return
	}
	maxBody := r.MaxBodyBytes
	if maxBody <= 0 || maxBody > MaxRuntimeConfigDirectoryRebindApplyResponseBytes {
		maxBody = MaxRuntimeConfigDirectoryRebindApplyResponseBytes
	}
	if request.Body == nil {
		writeRuntimeConfigDirectoryRebindApplyError(writer, http.StatusBadRequest, fmt.Errorf("%w: request body 为空", ErrInvalidRuntimeConfigDirectoryRebindApply))
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBody+1))
	if err != nil || int64(len(body)) > maxBody {
		writeRuntimeConfigDirectoryRebindApplyError(writer, http.StatusBadRequest, fmt.Errorf("%w: request body 超过上限或读取失败", ErrInvalidRuntimeConfigDirectoryRebindApply))
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var payload RuntimeConfigDirectoryRebindApplyRequest
	if err := decoder.Decode(&payload); err != nil {
		writeRuntimeConfigDirectoryRebindApplyError(writer, http.StatusBadRequest, fmt.Errorf("%w: request JSON 无效", ErrInvalidRuntimeConfigDirectoryRebindApply))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeRuntimeConfigDirectoryRebindApplyError(writer, http.StatusBadRequest, fmt.Errorf("%w: request 包含多个 JSON 文档", ErrInvalidRuntimeConfigDirectoryRebindApply))
		return
	}
	envelope, projection, err := normalizeRuntimeConfigDirectoryRebindApplyPair(payload.Envelope, payload.Projection)
	if err != nil {
		writeRuntimeConfigDirectoryRebindApplyError(writer, http.StatusBadRequest, err)
		return
	}
	if envelope.Source != r.Source || envelope.Destination != r.Destination {
		writeRuntimeConfigDirectoryRebindApplyError(writer, http.StatusForbidden, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeConfigDirectoryRebindApplyAuth))
		return
	}
	if err := VerifyRuntimeConfigDirectoryRebindApplyEnvelope(envelope, r.SharedSecret); err != nil {
		writeRuntimeConfigDirectoryRebindApplyError(writer, http.StatusUnauthorized, err)
		return
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	if err := validateRuntimeConfigDirectoryRebindApplyTimestamp(envelope.IssuedAt, now, r.MaxAge, r.MaxFutureSkew); err != nil {
		writeRuntimeConfigDirectoryRebindApplyError(writer, http.StatusUnauthorized, err)
		return
	}
	duplicate, err := r.Inbox.AcceptRuntimeConfigDirectoryRebind(request.Context(), envelope, projection)
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, ErrRuntimeConfigDirectoryRebindApplyUnavailable) {
			status = http.StatusNotImplemented
		}
		writeRuntimeConfigDirectoryRebindApplyError(writer, status, err)
		return
	}
	receipt := RuntimeConfigDirectoryRebindApplyReceipt{Version: RuntimeConfigDirectoryRebindApplyReceiptVersion, Source: envelope.Source, Destination: envelope.Destination, PlanID: envelope.PlanID, ApplyID: envelope.ApplyID, InvocationID: envelope.InvocationID, SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence, SnapshotDigest: envelope.SnapshotDigest, Phase: RuntimeConfigDirectoryRebindApplyReceiptAccepted, Duplicate: duplicate, IssuedAt: now}
	receipt, err = SignRuntimeConfigDirectoryRebindApplyReceipt(receipt, r.SharedSecret)
	if err != nil {
		writeRuntimeConfigDirectoryRebindApplyError(writer, http.StatusInternalServerError, err)
		return
	}
	maxResponse := r.MaxResponse
	if maxResponse <= 0 || maxResponse > MaxRuntimeConfigDirectoryRebindApplyResponseBytes {
		maxResponse = MaxRuntimeConfigDirectoryRebindApplyResponseBytes
	}
	encoded, err := json.Marshal(receipt)
	if err != nil || int64(len(encoded)) > maxResponse {
		writeRuntimeConfigDirectoryRebindApplyError(writer, http.StatusInternalServerError, fmt.Errorf("%w: receipt 超过响应上限", ErrInvalidRuntimeConfigDirectoryRebindApply))
		return
	}
	writeRuntimeConfigDirectoryRebindApplyJSON(writer, http.StatusOK, receipt)
}

// RuntimeConfigDirectoryRebindApplyHTTPTransport signs and sends the bounded
// projection to an explicitly configured destination receiver.
type RuntimeConfigDirectoryRebindApplyHTTPTransport struct {
	Endpoint     string
	Source       string
	Destination  string
	SharedSecret []byte
	Client       *http.Client
	MaxBodyBytes int64
}

func NewRuntimeConfigDirectoryRebindApplyHTTPTransport(endpoint, source, destination string, secret []byte, client *http.Client) (*RuntimeConfigDirectoryRebindApplyHTTPTransport, error) {
	endpoint = strings.TrimSpace(endpoint)
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("%w: endpoint 必须是 http/https URL", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	if strings.TrimSpace(source) == "" || strings.TrimSpace(destination) == "" {
		return nil, fmt.Errorf("%w: source/destination 无效", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &RuntimeConfigDirectoryRebindApplyHTTPTransport{Endpoint: endpoint, Source: strings.TrimSpace(source), Destination: strings.TrimSpace(destination), SharedSecret: append([]byte(nil), secret...), Client: client, MaxBodyBytes: MaxRuntimeConfigDirectoryRebindApplyResponseBytes}, nil
}

func (t *RuntimeConfigDirectoryRebindApplyHTTPTransport) Deliver(ctx context.Context, envelope RuntimeConfigDirectoryRebindApplyEnvelope, projection RuntimeCheckpointProjection) (RuntimeConfigDirectoryRebindApplyReceipt, error) {
	if t == nil {
		return RuntimeConfigDirectoryRebindApplyReceipt{}, fmt.Errorf("%w: transport 为空", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	normalized, projection, err := normalizeRuntimeConfigDirectoryRebindApplyPair(envelope, projection)
	if err != nil {
		return RuntimeConfigDirectoryRebindApplyReceipt{}, err
	}
	if normalized.Source != t.Source || normalized.Destination != t.Destination {
		return RuntimeConfigDirectoryRebindApplyReceipt{}, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeConfigDirectoryRebindApplyAuth)
	}
	signed, err := SignRuntimeConfigDirectoryRebindApplyEnvelope(normalized, t.SharedSecret)
	if err != nil {
		return RuntimeConfigDirectoryRebindApplyReceipt{}, err
	}
	body, err := json.Marshal(RuntimeConfigDirectoryRebindApplyRequest{Envelope: signed, Projection: projection})
	if err != nil || len(body) > MaxRuntimeConfigDirectoryRebindApplyEnvelopeBytes {
		return RuntimeConfigDirectoryRebindApplyReceipt{}, fmt.Errorf("%w: request 超过字节上限", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.Endpoint, bytes.NewReader(body))
	if err != nil {
		return RuntimeConfigDirectoryRebindApplyReceipt{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Abot-Rebind-Apply-Version", strconv.Itoa(signed.Version))
	request.Header.Set("X-Abot-Rebind-Apply-Signature", signed.Signature)
	request.Header.Set("Idempotency-Key", signed.ApplyID)
	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return RuntimeConfigDirectoryRebindApplyReceipt{}, fmt.Errorf("Runtime rebind apply 请求失败: %w", err)
	}
	defer response.Body.Close()
	maxBody := t.MaxBodyBytes
	if maxBody <= 0 || maxBody > MaxRuntimeConfigDirectoryRebindApplyResponseBytes {
		maxBody = MaxRuntimeConfigDirectoryRebindApplyResponseBytes
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil || int64(len(responseBody)) > maxBody {
		return RuntimeConfigDirectoryRebindApplyReceipt{}, fmt.Errorf("%w: response 超过字节上限或读取失败", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(SanitizeRuntimeString(string(responseBody)))
		if len(message) > 512 {
			message = message[:512]
		}
		base := error(ErrRuntimeConfigDirectoryRebindApplyConflict)
		switch {
		case response.StatusCode == http.StatusNotImplemented:
			base = ErrRuntimeConfigDirectoryRebindApplyUnavailable
		case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
			base = ErrRuntimeConfigDirectoryRebindApplyAuth
		case response.StatusCode >= 500:
			base = nil
		}
		if base != nil {
			return RuntimeConfigDirectoryRebindApplyReceipt{}, fmt.Errorf("%w: Runtime rebind apply 返回 HTTP %d: %s", base, response.StatusCode, message)
		}
		return RuntimeConfigDirectoryRebindApplyReceipt{}, fmt.Errorf("Runtime rebind apply 返回 HTTP %d: %s", response.StatusCode, message)
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	var receipt RuntimeConfigDirectoryRebindApplyReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return RuntimeConfigDirectoryRebindApplyReceipt{}, fmt.Errorf("%w: receipt JSON 无效", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeConfigDirectoryRebindApplyReceipt{}, fmt.Errorf("%w: receipt 包含多个 JSON 文档", ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	if err := receipt.ValidateAgainst(signed); err != nil {
		return RuntimeConfigDirectoryRebindApplyReceipt{}, err
	}
	if err := VerifyRuntimeConfigDirectoryRebindApplyReceipt(receipt, t.SharedSecret); err != nil {
		return RuntimeConfigDirectoryRebindApplyReceipt{}, err
	}
	return receipt, nil
}

func writeRuntimeConfigDirectoryRebindApplyJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeRuntimeConfigDirectoryRebindApplyError(writer http.ResponseWriter, status int, err error) {
	message := "Runtime rebind apply failed"
	if err != nil {
		message = strings.TrimSpace(SanitizeRuntimeString(err.Error()))
		if len(message) > 512 {
			message = message[:512]
		}
	}
	writeRuntimeConfigDirectoryRebindApplyJSON(writer, status, map[string]any{"error": message})
}

var _ RuntimeConfigDirectoryRebindApplyTransport = (*RuntimeConfigDirectoryRebindApplyHTTPTransport)(nil)
