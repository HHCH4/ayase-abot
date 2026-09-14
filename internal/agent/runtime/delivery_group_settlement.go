package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RuntimeDeliveryGroupSettlement is the source-side durable work item for a
// remote group outcome. It contains only the immutable group identity and the
// bounded retry/lease metadata; all family payloads remain in their own
// outboxes and are never copied here.
type RuntimeDeliveryGroupSettlement struct {
	ID             string                                    `json:"id"`
	GroupID        string                                    `json:"group_id"`
	InvocationID   string                                    `json:"invocation_id"`
	Source         string                                    `json:"source"`
	Destination    string                                    `json:"destination"`
	Outcome        RuntimeDeliveryGroupStatus                `json:"outcome"`
	Phase          RuntimeDeliveryGroupSettlementPhase       `json:"phase"`
	Status         RuntimeDeliveryGroupSettlementStatus      `json:"status"`
	Stage          RuntimeDeliveryGroupSettlementStage       `json:"stage"`
	RemotePhase    RuntimeDeliveryGroupSettlementRemotePhase `json:"remote_phase,omitempty"`
	AlertCode      RuntimeDeliveryGroupSettlementAlertCode   `json:"alert_code,omitempty"`
	AlertCount     int                                       `json:"alert_count,omitempty"`
	AlertAt        *time.Time                                `json:"alert_at,omitempty"`
	LastStatusAt   *time.Time                                `json:"last_status_at,omitempty"`
	Attempt        int                                       `json:"attempt"`
	Revision       int64                                     `json:"revision"`
	AvailableAt    time.Time                                 `json:"available_at"`
	LeaseOwner     string                                    `json:"-"`
	LeaseExpiresAt *time.Time                                `json:"-"`
	LastError      string                                    `json:"last_error,omitempty"`
	CreatedAt      time.Time                                 `json:"created_at"`
	UpdatedAt      time.Time                                 `json:"updated_at"`
	CompletedAt    *time.Time                                `json:"completed_at,omitempty"`
}

type RuntimeDeliveryGroupSettlementPhase string

const (
	RuntimeDeliveryGroupSettlementCommit RuntimeDeliveryGroupSettlementPhase = "commit"
	RuntimeDeliveryGroupSettlementAbort  RuntimeDeliveryGroupSettlementPhase = "abort"
)

type RuntimeDeliveryGroupSettlementStatus string

const (
	RuntimeDeliveryGroupSettlementQueued     RuntimeDeliveryGroupSettlementStatus = "queued"
	RuntimeDeliveryGroupSettlementProcessing RuntimeDeliveryGroupSettlementStatus = "processing"
	RuntimeDeliveryGroupSettlementCompleted  RuntimeDeliveryGroupSettlementStatus = "completed"
	RuntimeDeliveryGroupSettlementFailed     RuntimeDeliveryGroupSettlementStatus = "failed"
)

// RuntimeDeliveryGroupSettlementStage is the latest bounded remote phase
// observed by the source worker. It is separate from Status: Status describes
// the durable queue lifecycle, while Stage explains where a claimed item was
// when the last attempt yielded or failed.
type RuntimeDeliveryGroupSettlementStage string

const (
	RuntimeDeliveryGroupSettlementStageQueued     RuntimeDeliveryGroupSettlementStage = "queued"
	RuntimeDeliveryGroupSettlementStageStatus     RuntimeDeliveryGroupSettlementStage = "status"
	RuntimeDeliveryGroupSettlementStagePreparing  RuntimeDeliveryGroupSettlementStage = "preparing"
	RuntimeDeliveryGroupSettlementStagePrepared   RuntimeDeliveryGroupSettlementStage = "prepared"
	RuntimeDeliveryGroupSettlementStageCommitting RuntimeDeliveryGroupSettlementStage = "committing"
	RuntimeDeliveryGroupSettlementStageAborting   RuntimeDeliveryGroupSettlementStage = "aborting"
	RuntimeDeliveryGroupSettlementStageCompleted  RuntimeDeliveryGroupSettlementStage = "completed"
	RuntimeDeliveryGroupSettlementStageFailed     RuntimeDeliveryGroupSettlementStage = "failed"
)

type RuntimeDeliveryGroupSettlementRemotePhase string

const (
	RuntimeDeliveryGroupSettlementRemoteAbsent    RuntimeDeliveryGroupSettlementRemotePhase = "absent"
	RuntimeDeliveryGroupSettlementRemotePrepared  RuntimeDeliveryGroupSettlementRemotePhase = "prepared"
	RuntimeDeliveryGroupSettlementRemoteCommitted RuntimeDeliveryGroupSettlementRemotePhase = "committed"
	RuntimeDeliveryGroupSettlementRemoteAborted   RuntimeDeliveryGroupSettlementRemotePhase = "aborted"
)

// Alert codes are stable metadata-only categories. Error text remains a
// bounded, sanitized LastError and is never used as an API-controlled code.
type RuntimeDeliveryGroupSettlementAlertCode string

const (
	RuntimeDeliveryGroupSettlementAlertSourceMissing     RuntimeDeliveryGroupSettlementAlertCode = "source_missing"
	RuntimeDeliveryGroupSettlementAlertSourceConflict    RuntimeDeliveryGroupSettlementAlertCode = "source_conflict"
	RuntimeDeliveryGroupSettlementAlertRemoteUnavailable RuntimeDeliveryGroupSettlementAlertCode = "remote_unavailable"
	RuntimeDeliveryGroupSettlementAlertRemoteConflict    RuntimeDeliveryGroupSettlementAlertCode = "remote_conflict"
	RuntimeDeliveryGroupSettlementAlertProtocolRejected  RuntimeDeliveryGroupSettlementAlertCode = "protocol_rejected"
	RuntimeDeliveryGroupSettlementAlertRemoteError       RuntimeDeliveryGroupSettlementAlertCode = "remote_error"
	RuntimeDeliveryGroupSettlementAlertRetryExhausted    RuntimeDeliveryGroupSettlementAlertCode = "retry_exhausted"
)

const (
	MaxRuntimeDeliveryGroupSettlementIDLength         = 220
	MaxRuntimeDeliveryGroupSettlementOwnerLength      = MaxRuntimeDeliveryGroupOwnerLength
	MaxRuntimeDeliveryGroupSettlementErrorLength      = MaxRuntimeDeliveryGroupErrorLength
	MaxRuntimeDeliveryGroupSettlementAttempts         = 8
	MaxRuntimeDeliveryGroupSettlementLease            = 10 * time.Minute
	MaxRuntimeDeliveryGroupSettlementInvocationLength = MaxRuntimeDeliveryGroupInvocationLength
	MaxRuntimeDeliveryGroupSettlementAlertCount       = 64
	MaxRuntimeDeliveryGroupSettlementAlertCodeLength  = 48
)

var (
	ErrInvalidRuntimeDeliveryGroupSettlement     = errors.New("Runtime delivery group settlement 无效")
	ErrRuntimeDeliveryGroupSettlementUnavailable = errors.New("Runtime delivery group settlement 未配置")
	// ErrRuntimeDeliveryGroupSettlementRemoteConflict identifies a destination
	// terminal phase that is opposite to the immutable source decision. It is
	// kept separate from generic ErrConflict so the source saga can enter the
	// explicit compensation_required state without classifying auth/identity
	// conflicts as business compensation.
	ErrRuntimeDeliveryGroupSettlementRemoteConflict = errors.New("Runtime delivery group settlement 远端终态冲突")
)

func validRuntimeDeliveryGroupSettlementPhase(phase RuntimeDeliveryGroupSettlementPhase) bool {
	return phase == RuntimeDeliveryGroupSettlementCommit || phase == RuntimeDeliveryGroupSettlementAbort
}

func validRuntimeDeliveryGroupSettlementStatus(status RuntimeDeliveryGroupSettlementStatus) bool {
	switch status {
	case RuntimeDeliveryGroupSettlementQueued, RuntimeDeliveryGroupSettlementProcessing,
		RuntimeDeliveryGroupSettlementCompleted, RuntimeDeliveryGroupSettlementFailed:
		return true
	default:
		return false
	}
}

func validRuntimeDeliveryGroupSettlementStage(stage RuntimeDeliveryGroupSettlementStage) bool {
	switch stage {
	case RuntimeDeliveryGroupSettlementStageQueued, RuntimeDeliveryGroupSettlementStageStatus,
		RuntimeDeliveryGroupSettlementStagePreparing, RuntimeDeliveryGroupSettlementStagePrepared,
		RuntimeDeliveryGroupSettlementStageCommitting, RuntimeDeliveryGroupSettlementStageAborting,
		RuntimeDeliveryGroupSettlementStageCompleted, RuntimeDeliveryGroupSettlementStageFailed:
		return true
	default:
		return false
	}
}

func validRuntimeDeliveryGroupSettlementRemotePhase(phase RuntimeDeliveryGroupSettlementRemotePhase) bool {
	switch phase {
	case "", RuntimeDeliveryGroupSettlementRemoteAbsent, RuntimeDeliveryGroupSettlementRemotePrepared,
		RuntimeDeliveryGroupSettlementRemoteCommitted, RuntimeDeliveryGroupSettlementRemoteAborted:
		return true
	default:
		return false
	}
}

func validRuntimeDeliveryGroupSettlementAlertCode(code RuntimeDeliveryGroupSettlementAlertCode) bool {
	switch code {
	case "", RuntimeDeliveryGroupSettlementAlertSourceMissing, RuntimeDeliveryGroupSettlementAlertSourceConflict,
		RuntimeDeliveryGroupSettlementAlertRemoteUnavailable, RuntimeDeliveryGroupSettlementAlertRemoteConflict,
		RuntimeDeliveryGroupSettlementAlertProtocolRejected, RuntimeDeliveryGroupSettlementAlertRemoteError,
		RuntimeDeliveryGroupSettlementAlertRetryExhausted:
		return true
	default:
		return false
	}
}

// RuntimeDeliveryGroupSettlementID is stable for a group and desired remote
// terminal phase. A source group cannot change its terminal outcome, but the
// phase remains part of the key so a malformed caller can never alias commit
// and abort work items.
func RuntimeDeliveryGroupSettlementID(groupID string, phase RuntimeDeliveryGroupSettlementPhase) string {
	material := strings.Join([]string{strings.TrimSpace(groupID), strings.TrimSpace(string(phase))}, "\x00")
	sum := sha256.Sum256([]byte(material))
	return "runtime-delivery-group-settlement-" + hex.EncodeToString(sum[:16])
}

func cloneRuntimeDeliveryGroupSettlement(item RuntimeDeliveryGroupSettlement) RuntimeDeliveryGroupSettlement {
	if item.LeaseExpiresAt != nil {
		value := item.LeaseExpiresAt.UTC()
		item.LeaseExpiresAt = &value
	}
	if item.CompletedAt != nil {
		value := item.CompletedAt.UTC()
		item.CompletedAt = &value
	}
	if item.AlertAt != nil {
		value := item.AlertAt.UTC()
		item.AlertAt = &value
	}
	if item.LastStatusAt != nil {
		value := item.LastStatusAt.UTC()
		item.LastStatusAt = &value
	}
	return item
}

func (s RuntimeDeliveryGroupSettlement) normalize(now time.Time) (RuntimeDeliveryGroupSettlement, error) {
	s.ID = strings.TrimSpace(s.ID)
	s.GroupID = strings.TrimSpace(s.GroupID)
	s.InvocationID = strings.TrimSpace(s.InvocationID)
	s.Source = strings.TrimSpace(s.Source)
	s.Destination = strings.TrimSpace(s.Destination)
	s.LeaseOwner = strings.TrimSpace(s.LeaseOwner)
	s.LastError = SanitizeRuntimeString(strings.TrimSpace(s.LastError))
	s.Stage = RuntimeDeliveryGroupSettlementStage(strings.TrimSpace(string(s.Stage)))
	s.RemotePhase = RuntimeDeliveryGroupSettlementRemotePhase(strings.TrimSpace(strings.ToLower(string(s.RemotePhase))))
	s.AlertCode = RuntimeDeliveryGroupSettlementAlertCode(strings.TrimSpace(strings.ToLower(string(s.AlertCode))))
	if s.ID == "" || s.GroupID == "" || s.InvocationID == "" || s.Source == "" || s.Destination == "" {
		return RuntimeDeliveryGroupSettlement{}, fmt.Errorf("%w: metadata 不完整", ErrInvalidRuntimeDeliveryGroupSettlement)
	}
	if len(s.ID) > MaxRuntimeDeliveryGroupSettlementIDLength || len(s.GroupID) > MaxRuntimeDeliveryGroupIDLength || len(s.InvocationID) > MaxRuntimeDeliveryGroupSettlementInvocationLength || len(s.Source) > MaxRuntimeDeliveryAttemptSourceLength || len(s.Destination) > MaxRuntimeDeliveryAttemptDestinationLength || len(s.LeaseOwner) > MaxRuntimeDeliveryGroupSettlementOwnerLength {
		return RuntimeDeliveryGroupSettlement{}, fmt.Errorf("%w: metadata 超出长度限制", ErrInvalidRuntimeDeliveryGroupSettlement)
	}
	if s.ID != RuntimeDeliveryGroupSettlementID(s.GroupID, s.Phase) {
		return RuntimeDeliveryGroupSettlement{}, fmt.Errorf("%w: ID 不是稳定派生值", ErrInvalidRuntimeDeliveryGroupSettlement)
	}
	if !validRuntimeDeliveryGroupStatus(s.Outcome) || (s.Outcome != RuntimeDeliveryGroupCompleted && s.Outcome != RuntimeDeliveryGroupFailed) {
		return RuntimeDeliveryGroupSettlement{}, fmt.Errorf("%w: outcome 必须是 completed/failed", ErrInvalidRuntimeDeliveryGroupSettlement)
	}
	if !validRuntimeDeliveryGroupSettlementPhase(s.Phase) {
		return RuntimeDeliveryGroupSettlement{}, fmt.Errorf("%w: phase %q 不受支持", ErrInvalidRuntimeDeliveryGroupSettlement, s.Phase)
	}
	if (s.Outcome == RuntimeDeliveryGroupCompleted && s.Phase != RuntimeDeliveryGroupSettlementCommit) || (s.Outcome == RuntimeDeliveryGroupFailed && s.Phase != RuntimeDeliveryGroupSettlementAbort) {
		return RuntimeDeliveryGroupSettlement{}, fmt.Errorf("%w: outcome 与 phase 不匹配", ErrInvalidRuntimeDeliveryGroupSettlement)
	}
	if s.Status == "" {
		s.Status = RuntimeDeliveryGroupSettlementQueued
	}
	if !validRuntimeDeliveryGroupSettlementStatus(s.Status) {
		return RuntimeDeliveryGroupSettlement{}, fmt.Errorf("%w: status %q 不受支持", ErrInvalidRuntimeDeliveryGroupSettlement, s.Status)
	}
	if s.Stage == "" {
		switch s.Status {
		case RuntimeDeliveryGroupSettlementQueued:
			s.Stage = RuntimeDeliveryGroupSettlementStageQueued
		case RuntimeDeliveryGroupSettlementProcessing:
			s.Stage = RuntimeDeliveryGroupSettlementStageStatus
		case RuntimeDeliveryGroupSettlementCompleted:
			s.Stage = RuntimeDeliveryGroupSettlementStageCompleted
		case RuntimeDeliveryGroupSettlementFailed:
			s.Stage = RuntimeDeliveryGroupSettlementStageFailed
		}
	}
	if !validRuntimeDeliveryGroupSettlementStage(s.Stage) {
		return RuntimeDeliveryGroupSettlement{}, fmt.Errorf("%w: stage %q 不受支持", ErrInvalidRuntimeDeliveryGroupSettlement, s.Stage)
	}
	if !validRuntimeDeliveryGroupSettlementRemotePhase(s.RemotePhase) {
		return RuntimeDeliveryGroupSettlement{}, fmt.Errorf("%w: remote phase %q 不受支持", ErrInvalidRuntimeDeliveryGroupSettlement, s.RemotePhase)
	}
	if !validRuntimeDeliveryGroupSettlementAlertCode(s.AlertCode) || len(s.AlertCode) > MaxRuntimeDeliveryGroupSettlementAlertCodeLength {
		return RuntimeDeliveryGroupSettlement{}, fmt.Errorf("%w: alert code 不受支持", ErrInvalidRuntimeDeliveryGroupSettlement)
	}
	if s.AlertCount < 0 || s.AlertCount > MaxRuntimeDeliveryGroupSettlementAlertCount {
		return RuntimeDeliveryGroupSettlement{}, fmt.Errorf("%w: alert count 无效", ErrInvalidRuntimeDeliveryGroupSettlement)
	}
	if s.AlertAt != nil {
		value := s.AlertAt.UTC()
		s.AlertAt = &value
	}
	if s.LastStatusAt != nil {
		value := s.LastStatusAt.UTC()
		s.LastStatusAt = &value
	}
	if s.Attempt < 0 || s.Attempt > MaxRuntimeDeliveryGroupSettlementAttempts || s.Revision < 0 {
		return RuntimeDeliveryGroupSettlement{}, fmt.Errorf("%w: attempt/revision 无效", ErrInvalidRuntimeDeliveryGroupSettlement)
	}
	if len(s.LastError) > MaxRuntimeDeliveryGroupSettlementErrorLength {
		s.LastError = s.LastError[:MaxRuntimeDeliveryGroupSettlementErrorLength]
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if s.AvailableAt.IsZero() {
		s.AvailableAt = now
	} else {
		s.AvailableAt = s.AvailableAt.UTC()
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	} else {
		s.CreatedAt = s.CreatedAt.UTC()
	}
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = now
	} else {
		s.UpdatedAt = s.UpdatedAt.UTC()
	}
	if s.Revision <= 0 {
		s.Revision = 1
	}
	if s.LeaseExpiresAt != nil {
		value := s.LeaseExpiresAt.UTC()
		s.LeaseExpiresAt = &value
	}
	if s.CompletedAt != nil {
		value := s.CompletedAt.UTC()
		s.CompletedAt = &value
	}
	switch s.Status {
	case RuntimeDeliveryGroupSettlementQueued, RuntimeDeliveryGroupSettlementFailed, RuntimeDeliveryGroupSettlementCompleted:
		s.LeaseOwner = ""
		s.LeaseExpiresAt = nil
	case RuntimeDeliveryGroupSettlementProcessing:
		if s.LeaseOwner == "" || s.LeaseExpiresAt == nil || s.LeaseExpiresAt.IsZero() {
			return RuntimeDeliveryGroupSettlement{}, fmt.Errorf("%w: processing settlement 必须带租约", ErrInvalidRuntimeDeliveryGroupSettlement)
		}
		if s.LeaseExpiresAt.After(now.Add(MaxRuntimeDeliveryGroupSettlementLease)) {
			return RuntimeDeliveryGroupSettlement{}, fmt.Errorf("%w: settlement lease 超出时长限制", ErrInvalidRuntimeDeliveryGroupSettlement)
		}
	}
	if s.Status == RuntimeDeliveryGroupSettlementCompleted {
		if s.Stage != RuntimeDeliveryGroupSettlementStageCompleted {
			return RuntimeDeliveryGroupSettlement{}, fmt.Errorf("%w: completed settlement 的 stage 必须为 completed", ErrInvalidRuntimeDeliveryGroupSettlement)
		}
		if s.CompletedAt == nil {
			finished := s.UpdatedAt
			s.CompletedAt = &finished
		}
	} else if s.CompletedAt != nil {
		return RuntimeDeliveryGroupSettlement{}, fmt.Errorf("%w: 非 completed settlement 不能带 completed_at", ErrInvalidRuntimeDeliveryGroupSettlement)
	}
	if s.Status == RuntimeDeliveryGroupSettlementFailed && s.Stage != RuntimeDeliveryGroupSettlementStageFailed {
		return RuntimeDeliveryGroupSettlement{}, fmt.Errorf("%w: failed settlement 的 stage 必须为 failed", ErrInvalidRuntimeDeliveryGroupSettlement)
	}
	if s.Status == RuntimeDeliveryGroupSettlementQueued && s.Stage != RuntimeDeliveryGroupSettlementStageQueued {
		return RuntimeDeliveryGroupSettlement{}, fmt.Errorf("%w: queued settlement 的 stage 必须为 queued", ErrInvalidRuntimeDeliveryGroupSettlement)
	}
	if s.Status == RuntimeDeliveryGroupSettlementProcessing && (s.Stage == RuntimeDeliveryGroupSettlementStageQueued || s.Stage == RuntimeDeliveryGroupSettlementStageCompleted || s.Stage == RuntimeDeliveryGroupSettlementStageFailed) {
		return RuntimeDeliveryGroupSettlement{}, fmt.Errorf("%w: processing settlement 的 stage 无效", ErrInvalidRuntimeDeliveryGroupSettlement)
	}
	return s, nil
}

func (s RuntimeDeliveryGroupSettlement) Normalize(now time.Time) (RuntimeDeliveryGroupSettlement, error) {
	return s.normalize(now)
}

func NewRuntimeDeliveryGroupSettlement(group RuntimeDeliveryGroup, now time.Time) (RuntimeDeliveryGroupSettlement, error) {
	normalizedGroup, err := group.Normalize(now)
	if err != nil {
		return RuntimeDeliveryGroupSettlement{}, err
	}
	if normalizedGroup.Status != RuntimeDeliveryGroupCompleted && normalizedGroup.Status != RuntimeDeliveryGroupFailed {
		return RuntimeDeliveryGroupSettlement{}, fmt.Errorf("%w: 只有终态 group 才能创建 settlement", ErrInvalidRuntimeDeliveryGroupSettlement)
	}
	phase := RuntimeDeliveryGroupSettlementAbort
	if normalizedGroup.Status == RuntimeDeliveryGroupCompleted {
		phase = RuntimeDeliveryGroupSettlementCommit
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	return (RuntimeDeliveryGroupSettlement{
		ID: RuntimeDeliveryGroupSettlementID(normalizedGroup.ID, phase), GroupID: normalizedGroup.ID,
		InvocationID: normalizedGroup.InvocationID, Source: normalizedGroup.Source, Destination: normalizedGroup.Destination,
		Outcome: normalizedGroup.Status, Phase: phase, Status: RuntimeDeliveryGroupSettlementQueued,
		AvailableAt: now, CreatedAt: now, UpdatedAt: now, Revision: 1,
	}).normalize(now)
}

func (s RuntimeDeliveryGroupSettlement) MatchesIdentity(other RuntimeDeliveryGroupSettlement) bool {
	left, leftErr := s.normalize(time.Time{})
	right, rightErr := other.normalize(time.Time{})
	if leftErr != nil || rightErr != nil {
		return false
	}
	return left.ID == right.ID && left.GroupID == right.GroupID && left.InvocationID == right.InvocationID && left.Source == right.Source && left.Destination == right.Destination && left.Outcome == right.Outcome && left.Phase == right.Phase
}

func RuntimeDeliveryGroupSettlementBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

func runtimeDeliveryGroupSettlementStageTransitionAllowed(from, to RuntimeDeliveryGroupSettlementStage) bool {
	if from == to {
		return true
	}
	switch from {
	case RuntimeDeliveryGroupSettlementStageQueued:
		return to == RuntimeDeliveryGroupSettlementStageStatus
	case RuntimeDeliveryGroupSettlementStageStatus:
		return to == RuntimeDeliveryGroupSettlementStagePreparing || to == RuntimeDeliveryGroupSettlementStagePrepared
	case RuntimeDeliveryGroupSettlementStagePreparing:
		return to == RuntimeDeliveryGroupSettlementStagePrepared
	case RuntimeDeliveryGroupSettlementStagePrepared:
		return to == RuntimeDeliveryGroupSettlementStageCommitting || to == RuntimeDeliveryGroupSettlementStageAborting
	case RuntimeDeliveryGroupSettlementStageCommitting, RuntimeDeliveryGroupSettlementStageAborting,
		RuntimeDeliveryGroupSettlementStageCompleted, RuntimeDeliveryGroupSettlementStageFailed:
		return false
	default:
		return false
	}
}

func mergeRuntimeDeliveryGroupSettlementRemotePhase(current, next RuntimeDeliveryGroupSettlementRemotePhase) (RuntimeDeliveryGroupSettlementRemotePhase, error) {
	if !validRuntimeDeliveryGroupSettlementRemotePhase(next) {
		return "", fmt.Errorf("%w: remote phase %q 不受支持", ErrInvalidRuntimeDeliveryGroupSettlement, next)
	}
	if next == "" || current == next {
		return current, nil
	}
	if current == "" || current == RuntimeDeliveryGroupSettlementRemoteAbsent {
		return next, nil
	}
	if current == RuntimeDeliveryGroupSettlementRemotePrepared && (next == RuntimeDeliveryGroupSettlementRemoteCommitted || next == RuntimeDeliveryGroupSettlementRemoteAborted) {
		return next, nil
	}
	// A prepared destination must never appear to roll back to absent, and a
	// terminal remote phase cannot fork to its opposite terminal phase.
	if next == RuntimeDeliveryGroupSettlementRemoteAbsent ||
		(current == RuntimeDeliveryGroupSettlementRemoteCommitted && next != RuntimeDeliveryGroupSettlementRemoteCommitted) ||
		(current == RuntimeDeliveryGroupSettlementRemoteAborted && next != RuntimeDeliveryGroupSettlementRemoteAborted) {
		return "", fmt.Errorf("%w: remote phase 从 %q 回退或分叉到 %q", ErrConflict, current, next)
	}
	return "", fmt.Errorf("%w: remote phase 从 %q 分叉到 %q", ErrConflict, current, next)
}

// AdvanceRuntimeDeliveryGroupSettlement records one bounded progress update
// under the claimed lease. Repeating the exact update is idempotent.
func AdvanceRuntimeDeliveryGroupSettlement(item RuntimeDeliveryGroupSettlement, owner string, expectedRevision int64, stage RuntimeDeliveryGroupSettlementStage, remotePhase RuntimeDeliveryGroupSettlementRemotePhase, alertCode RuntimeDeliveryGroupSettlementAlertCode, message string, now time.Time) (RuntimeDeliveryGroupSettlement, bool, error) {
	now = normalizeRuntimeDeliveryGroupTime(now)
	current, err := item.Normalize(now)
	if err != nil {
		return RuntimeDeliveryGroupSettlement{}, false, err
	}
	owner = strings.TrimSpace(owner)
	stage = RuntimeDeliveryGroupSettlementStage(strings.TrimSpace(string(stage)))
	remotePhase = RuntimeDeliveryGroupSettlementRemotePhase(strings.TrimSpace(strings.ToLower(string(remotePhase))))
	alertCode = RuntimeDeliveryGroupSettlementAlertCode(strings.TrimSpace(strings.ToLower(string(alertCode))))
	if owner == "" || len(owner) > MaxRuntimeDeliveryGroupSettlementOwnerLength || expectedRevision <= 0 || current.Revision != expectedRevision {
		return RuntimeDeliveryGroupSettlement{}, false, ErrConflict
	}
	if current.Status != RuntimeDeliveryGroupSettlementProcessing || current.LeaseOwner != owner || current.LeaseExpiresAt == nil || !current.LeaseExpiresAt.After(now) {
		return RuntimeDeliveryGroupSettlement{}, false, ErrConflict
	}
	if !validRuntimeDeliveryGroupSettlementStage(stage) || stage == RuntimeDeliveryGroupSettlementStageQueued || stage == RuntimeDeliveryGroupSettlementStageCompleted || stage == RuntimeDeliveryGroupSettlementStageFailed {
		return RuntimeDeliveryGroupSettlement{}, false, fmt.Errorf("%w: progress stage 不受支持", ErrInvalidRuntimeDeliveryGroupSettlement)
	}
	if !runtimeDeliveryGroupSettlementStageTransitionAllowed(current.Stage, stage) {
		return RuntimeDeliveryGroupSettlement{}, false, ErrConflict
	}
	mergedRemote, err := mergeRuntimeDeliveryGroupSettlementRemotePhase(current.RemotePhase, remotePhase)
	if err != nil {
		return RuntimeDeliveryGroupSettlement{}, false, err
	}
	if !validRuntimeDeliveryGroupSettlementAlertCode(alertCode) {
		return RuntimeDeliveryGroupSettlement{}, false, fmt.Errorf("%w: alert code 不受支持", ErrInvalidRuntimeDeliveryGroupSettlement)
	}
	message = SanitizeRuntimeString(strings.TrimSpace(message))
	if len(message) > MaxRuntimeDeliveryGroupSettlementErrorLength {
		message = message[:MaxRuntimeDeliveryGroupSettlementErrorLength]
	}
	updated := current
	updated.Stage = stage
	updated.RemotePhase = mergedRemote
	if remotePhase != "" {
		statusAt := now
		updated.LastStatusAt = &statusAt
	}
	if alertCode != "" {
		updated.AlertCode = alertCode
		if updated.AlertCount < MaxRuntimeDeliveryGroupSettlementAlertCount {
			updated.AlertCount++
		}
		alertAt := now
		updated.AlertAt = &alertAt
	}
	if message != "" {
		updated.LastError = message
	}
	if updated.Stage == current.Stage && updated.RemotePhase == current.RemotePhase && updated.AlertCode == current.AlertCode && updated.AlertCount == current.AlertCount && updated.LastError == current.LastError && ((updated.LastStatusAt == nil && current.LastStatusAt == nil) || (updated.LastStatusAt != nil && current.LastStatusAt != nil && updated.LastStatusAt.Equal(*current.LastStatusAt))) {
		return current, true, nil
	}
	updated.Revision++
	updated.UpdatedAt = now
	normalized, normalizeErr := updated.Normalize(now)
	return normalized, false, normalizeErr
}

// RuntimeDeliveryGroupSettlementAlertForError maps errors to stable
// metadata-only categories for UI and alerting consumers.
func RuntimeDeliveryGroupSettlementAlertForError(err error) RuntimeDeliveryGroupSettlementAlertCode {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, ErrRuntimeDeliveryGroupTransactionUnavailable), errors.Is(err, ErrRuntimeDeliveryGroupSettlementUnavailable), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return RuntimeDeliveryGroupSettlementAlertRemoteUnavailable
	case errors.Is(err, ErrRuntimeDeliveryGroupTransactionAuth), errors.Is(err, ErrRuntimeDeliveryGroupTransactionStale), errors.Is(err, ErrInvalidRuntimeDeliveryGroupTransaction), errors.Is(err, ErrInvalidRuntimeDeliveryGroupSettlement):
		return RuntimeDeliveryGroupSettlementAlertProtocolRejected
	case errors.Is(err, ErrConflict):
		return RuntimeDeliveryGroupSettlementAlertRemoteConflict
	default:
		return RuntimeDeliveryGroupSettlementAlertRemoteError
	}
}

// RuntimeDeliveryGroupSettlementRepository is the source-side durable queue
// for remote group outcomes. It is optional for compatibility; when absent,
// the coordinator retains its historical one-shot best-effort path.
type RuntimeDeliveryGroupSettlementRepository interface {
	EnqueueRuntimeDeliveryGroupSettlement(context.Context, RuntimeDeliveryGroupSettlement) (RuntimeDeliveryGroupSettlement, error)
	GetRuntimeDeliveryGroupSettlement(context.Context, string) (RuntimeDeliveryGroupSettlement, error)
	ListRuntimeDeliveryGroupSettlements(context.Context, string, RuntimeDeliveryGroupSettlementStatus, int) ([]RuntimeDeliveryGroupSettlement, error)
	ClaimRuntimeDeliveryGroupSettlement(context.Context, string, time.Time, time.Duration) (RuntimeDeliveryGroupSettlement, bool, error)
	CompleteRuntimeDeliveryGroupSettlement(context.Context, string, string, time.Time) (bool, error)
	RetryRuntimeDeliveryGroupSettlement(context.Context, string, string, time.Time, string) (bool, error)
	FailRuntimeDeliveryGroupSettlement(context.Context, string, string, time.Time, string) (bool, error)
}

// RuntimeDeliveryGroupSettlementProgressRepository is optional so older
// embedders can keep the queue lifecycle while newer stores expose fine-
// grained remote stage and alert metadata.
type RuntimeDeliveryGroupSettlementProgressRepository interface {
	AdvanceRuntimeDeliveryGroupSettlement(context.Context, string, string, int64, RuntimeDeliveryGroupSettlementStage, RuntimeDeliveryGroupSettlementRemotePhase, RuntimeDeliveryGroupSettlementAlertCode, string, time.Time) (RuntimeDeliveryGroupSettlement, bool, error)
}
