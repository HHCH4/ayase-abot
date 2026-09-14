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

// RuntimeDeliveryGroupSaga is a source-owned, metadata-only decision journal
// for one terminal delivery group. The settlement queue owns execution leases;
// this row owns the immutable commit/abort decision and monotonic remote fact.
type RuntimeDeliveryGroupSaga struct {
	ID           string                                    `json:"id"`
	GroupID      string                                    `json:"group_id"`
	InvocationID string                                    `json:"invocation_id"`
	Source       string                                    `json:"source"`
	Destination  string                                    `json:"destination"`
	Decision     RuntimeDeliveryGroupSagaDecision          `json:"decision"`
	State        RuntimeDeliveryGroupSagaState             `json:"state"`
	RemotePhase  RuntimeDeliveryGroupSettlementRemotePhase `json:"remote_phase,omitempty"`
	Revision     int64                                     `json:"revision"`
	LastError    string                                    `json:"last_error,omitempty"`
	CreatedAt    time.Time                                 `json:"created_at"`
	UpdatedAt    time.Time                                 `json:"updated_at"`
	CompletedAt  *time.Time                                `json:"completed_at,omitempty"`
}

type RuntimeDeliveryGroupSagaDecision string

const (
	RuntimeDeliveryGroupSagaCommit RuntimeDeliveryGroupSagaDecision = "commit"
	RuntimeDeliveryGroupSagaAbort  RuntimeDeliveryGroupSagaDecision = "abort"
)

type RuntimeDeliveryGroupSagaState string

const (
	RuntimeDeliveryGroupSagaDecided              RuntimeDeliveryGroupSagaState = "decided"
	RuntimeDeliveryGroupSagaPreparing            RuntimeDeliveryGroupSagaState = "preparing"
	RuntimeDeliveryGroupSagaPrepared             RuntimeDeliveryGroupSagaState = "prepared"
	RuntimeDeliveryGroupSagaCommitting           RuntimeDeliveryGroupSagaState = "committing"
	RuntimeDeliveryGroupSagaAborting             RuntimeDeliveryGroupSagaState = "aborting"
	RuntimeDeliveryGroupSagaCommitted            RuntimeDeliveryGroupSagaState = "committed"
	RuntimeDeliveryGroupSagaAborted              RuntimeDeliveryGroupSagaState = "aborted"
	RuntimeDeliveryGroupSagaCompensationRequired RuntimeDeliveryGroupSagaState = "compensation_required"
	RuntimeDeliveryGroupSagaFailed               RuntimeDeliveryGroupSagaState = "failed"
)

const (
	MaxRuntimeDeliveryGroupSagaIDLength          = 240
	MaxRuntimeDeliveryGroupSagaGroupIDLength     = MaxRuntimeDeliveryGroupIDLength
	MaxRuntimeDeliveryGroupSagaInvocationLength  = MaxRuntimeDeliveryGroupInvocationLength
	MaxRuntimeDeliveryGroupSagaSourceLength      = MaxRuntimeDeliveryAttemptSourceLength
	MaxRuntimeDeliveryGroupSagaDestinationLength = MaxRuntimeDeliveryAttemptDestinationLength
	MaxRuntimeDeliveryGroupSagaErrorLength       = MaxRuntimeDeliveryGroupErrorLength
)

var (
	ErrInvalidRuntimeDeliveryGroupSaga     = errors.New("Runtime delivery group saga 无效")
	ErrRuntimeDeliveryGroupSagaUnavailable = errors.New("Runtime delivery group saga 未配置")
)

func validRuntimeDeliveryGroupSagaDecision(decision RuntimeDeliveryGroupSagaDecision) bool {
	return decision == RuntimeDeliveryGroupSagaCommit || decision == RuntimeDeliveryGroupSagaAbort
}

func validRuntimeDeliveryGroupSagaState(state RuntimeDeliveryGroupSagaState) bool {
	switch state {
	case RuntimeDeliveryGroupSagaDecided, RuntimeDeliveryGroupSagaPreparing,
		RuntimeDeliveryGroupSagaPrepared, RuntimeDeliveryGroupSagaCommitting,
		RuntimeDeliveryGroupSagaAborting, RuntimeDeliveryGroupSagaCommitted,
		RuntimeDeliveryGroupSagaAborted, RuntimeDeliveryGroupSagaCompensationRequired,
		RuntimeDeliveryGroupSagaFailed:
		return true
	default:
		return false
	}
}

func RuntimeDeliveryGroupSagaID(groupID string, decision RuntimeDeliveryGroupSagaDecision) string {
	material := strings.TrimSpace(groupID) + "\x00" + strings.TrimSpace(string(decision))
	sum := sha256.Sum256([]byte(material))
	return "runtime-delivery-group-saga-" + hex.EncodeToString(sum[:16])
}

func cloneRuntimeDeliveryGroupSaga(item RuntimeDeliveryGroupSaga) RuntimeDeliveryGroupSaga {
	if item.CompletedAt != nil {
		value := item.CompletedAt.UTC()
		item.CompletedAt = &value
	}
	return item
}

func (s RuntimeDeliveryGroupSaga) normalize(now time.Time) (RuntimeDeliveryGroupSaga, error) {
	s.ID = strings.TrimSpace(s.ID)
	s.GroupID = strings.TrimSpace(s.GroupID)
	s.InvocationID = strings.TrimSpace(s.InvocationID)
	s.Source = strings.TrimSpace(s.Source)
	s.Destination = strings.TrimSpace(s.Destination)
	s.Decision = RuntimeDeliveryGroupSagaDecision(strings.TrimSpace(strings.ToLower(string(s.Decision))))
	s.State = RuntimeDeliveryGroupSagaState(strings.TrimSpace(strings.ToLower(string(s.State))))
	s.RemotePhase = RuntimeDeliveryGroupSettlementRemotePhase(strings.TrimSpace(strings.ToLower(string(s.RemotePhase))))
	s.LastError = SanitizeRuntimeString(strings.TrimSpace(s.LastError))
	if s.ID == "" || s.GroupID == "" || s.InvocationID == "" || s.Source == "" || s.Destination == "" || !validRuntimeDeliveryGroupSagaDecision(s.Decision) || !validRuntimeDeliveryGroupSagaState(s.State) {
		return RuntimeDeliveryGroupSaga{}, fmt.Errorf("%w: metadata 不完整", ErrInvalidRuntimeDeliveryGroupSaga)
	}
	if len(s.ID) > MaxRuntimeDeliveryGroupSagaIDLength || len(s.GroupID) > MaxRuntimeDeliveryGroupSagaGroupIDLength || len(s.InvocationID) > MaxRuntimeDeliveryGroupSagaInvocationLength || len(s.Source) > MaxRuntimeDeliveryGroupSagaSourceLength || len(s.Destination) > MaxRuntimeDeliveryGroupSagaDestinationLength {
		return RuntimeDeliveryGroupSaga{}, fmt.Errorf("%w: metadata 超出长度限制", ErrInvalidRuntimeDeliveryGroupSaga)
	}
	if s.ID != RuntimeDeliveryGroupSagaID(s.GroupID, s.Decision) {
		return RuntimeDeliveryGroupSaga{}, fmt.Errorf("%w: ID 不是稳定派生值", ErrInvalidRuntimeDeliveryGroupSaga)
	}
	if !validRuntimeDeliveryGroupSettlementRemotePhase(s.RemotePhase) {
		return RuntimeDeliveryGroupSaga{}, fmt.Errorf("%w: remote phase 不受支持", ErrInvalidRuntimeDeliveryGroupSaga)
	}
	if len(s.LastError) > MaxRuntimeDeliveryGroupSagaErrorLength {
		s.LastError = s.LastError[:MaxRuntimeDeliveryGroupSagaErrorLength]
	}
	if s.Revision < 0 {
		return RuntimeDeliveryGroupSaga{}, fmt.Errorf("%w: revision 无效", ErrInvalidRuntimeDeliveryGroupSaga)
	}
	if s.Revision == 0 {
		s.Revision = 1
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
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
	if s.CompletedAt != nil {
		value := s.CompletedAt.UTC()
		s.CompletedAt = &value
	}
	if s.State == RuntimeDeliveryGroupSagaCommitted || s.State == RuntimeDeliveryGroupSagaAborted {
		want := RuntimeDeliveryGroupSettlementRemoteCommitted
		if s.State == RuntimeDeliveryGroupSagaAborted {
			want = RuntimeDeliveryGroupSettlementRemoteAborted
		}
		if s.Decision == RuntimeDeliveryGroupSagaCommit && s.State == RuntimeDeliveryGroupSagaCommitted && s.RemotePhase != want {
			return RuntimeDeliveryGroupSaga{}, fmt.Errorf("%w: committed saga remote phase 不一致", ErrInvalidRuntimeDeliveryGroupSaga)
		}
		if s.Decision == RuntimeDeliveryGroupSagaAbort && s.State == RuntimeDeliveryGroupSagaAborted && s.RemotePhase != want {
			return RuntimeDeliveryGroupSaga{}, fmt.Errorf("%w: aborted saga remote phase 不一致", ErrInvalidRuntimeDeliveryGroupSaga)
		}
		if s.CompletedAt == nil {
			finished := s.UpdatedAt
			s.CompletedAt = &finished
		}
	} else if s.CompletedAt != nil {
		return RuntimeDeliveryGroupSaga{}, fmt.Errorf("%w: 非 terminal saga 不能带 completed_at", ErrInvalidRuntimeDeliveryGroupSaga)
	}
	if s.Decision == RuntimeDeliveryGroupSagaCommit && s.State == RuntimeDeliveryGroupSagaAborted {
		return RuntimeDeliveryGroupSaga{}, fmt.Errorf("%w: commit saga 不能 aborted", ErrInvalidRuntimeDeliveryGroupSaga)
	}
	if s.Decision == RuntimeDeliveryGroupSagaAbort && s.State == RuntimeDeliveryGroupSagaCommitted {
		return RuntimeDeliveryGroupSaga{}, fmt.Errorf("%w: abort saga 不能 committed", ErrInvalidRuntimeDeliveryGroupSaga)
	}
	return s, nil
}

func (s RuntimeDeliveryGroupSaga) Normalize(now time.Time) (RuntimeDeliveryGroupSaga, error) {
	return s.normalize(now)
}

func NewRuntimeDeliveryGroupSaga(group RuntimeDeliveryGroup, now time.Time) (RuntimeDeliveryGroupSaga, error) {
	group, err := group.Normalize(now)
	if err != nil {
		return RuntimeDeliveryGroupSaga{}, err
	}
	decision := RuntimeDeliveryGroupSagaAbort
	if group.Status == RuntimeDeliveryGroupCompleted {
		decision = RuntimeDeliveryGroupSagaCommit
	} else if group.Status != RuntimeDeliveryGroupFailed {
		return RuntimeDeliveryGroupSaga{}, fmt.Errorf("%w: 只有 terminal group 才能创建 saga", ErrInvalidRuntimeDeliveryGroupSaga)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	return (RuntimeDeliveryGroupSaga{
		ID: RuntimeDeliveryGroupSagaID(group.ID, decision), GroupID: group.ID,
		InvocationID: group.InvocationID, Source: group.Source, Destination: group.Destination,
		Decision: decision, State: RuntimeDeliveryGroupSagaDecided,
		RemotePhase: RuntimeDeliveryGroupSettlementRemoteAbsent, Revision: 1,
		CreatedAt: now, UpdatedAt: now,
	}).normalize(now)
}

func (s RuntimeDeliveryGroupSaga) MatchesIdentity(other RuntimeDeliveryGroupSaga) bool {
	left, leftErr := s.normalize(time.Time{})
	right, rightErr := other.normalize(time.Time{})
	if leftErr != nil || rightErr != nil {
		return false
	}
	return left.ID == right.ID && left.GroupID == right.GroupID && left.InvocationID == right.InvocationID && left.Source == right.Source && left.Destination == right.Destination && left.Decision == right.Decision
}

func runtimeDeliveryGroupSagaTransitionAllowed(decision RuntimeDeliveryGroupSagaDecision, from, to RuntimeDeliveryGroupSagaState) bool {
	if from == to {
		return true
	}
	if from == RuntimeDeliveryGroupSagaCommitted || from == RuntimeDeliveryGroupSagaAborted || from == RuntimeDeliveryGroupSagaCompensationRequired || from == RuntimeDeliveryGroupSagaFailed {
		return false
	}
	switch from {
	case RuntimeDeliveryGroupSagaDecided:
		if decision == RuntimeDeliveryGroupSagaCommit {
			return to == RuntimeDeliveryGroupSagaPreparing || to == RuntimeDeliveryGroupSagaPrepared || to == RuntimeDeliveryGroupSagaCommitting || to == RuntimeDeliveryGroupSagaCommitted || to == RuntimeDeliveryGroupSagaCompensationRequired || to == RuntimeDeliveryGroupSagaFailed
		}
		return to == RuntimeDeliveryGroupSagaPreparing || to == RuntimeDeliveryGroupSagaPrepared || to == RuntimeDeliveryGroupSagaAborting || to == RuntimeDeliveryGroupSagaAborted || to == RuntimeDeliveryGroupSagaCompensationRequired || to == RuntimeDeliveryGroupSagaFailed
	case RuntimeDeliveryGroupSagaPreparing:
		if decision == RuntimeDeliveryGroupSagaCommit {
			return to == RuntimeDeliveryGroupSagaPrepared || to == RuntimeDeliveryGroupSagaCommitting || to == RuntimeDeliveryGroupSagaCommitted || to == RuntimeDeliveryGroupSagaCompensationRequired || to == RuntimeDeliveryGroupSagaFailed
		}
		return to == RuntimeDeliveryGroupSagaPrepared || to == RuntimeDeliveryGroupSagaAborting || to == RuntimeDeliveryGroupSagaAborted || to == RuntimeDeliveryGroupSagaCompensationRequired || to == RuntimeDeliveryGroupSagaFailed
	case RuntimeDeliveryGroupSagaPrepared:
		if decision == RuntimeDeliveryGroupSagaCommit {
			return to == RuntimeDeliveryGroupSagaCommitting || to == RuntimeDeliveryGroupSagaCommitted || to == RuntimeDeliveryGroupSagaCompensationRequired || to == RuntimeDeliveryGroupSagaFailed
		}
		return to == RuntimeDeliveryGroupSagaAborting || to == RuntimeDeliveryGroupSagaAborted || to == RuntimeDeliveryGroupSagaCompensationRequired || to == RuntimeDeliveryGroupSagaFailed
	case RuntimeDeliveryGroupSagaCommitting:
		return decision == RuntimeDeliveryGroupSagaCommit && (to == RuntimeDeliveryGroupSagaCommitted || to == RuntimeDeliveryGroupSagaCompensationRequired || to == RuntimeDeliveryGroupSagaFailed)
	case RuntimeDeliveryGroupSagaAborting:
		return decision == RuntimeDeliveryGroupSagaAbort && (to == RuntimeDeliveryGroupSagaAborted || to == RuntimeDeliveryGroupSagaCompensationRequired || to == RuntimeDeliveryGroupSagaFailed)
	default:
		return false
	}
}

func mergeRuntimeDeliveryGroupSagaRemotePhase(current, next RuntimeDeliveryGroupSettlementRemotePhase) (RuntimeDeliveryGroupSettlementRemotePhase, error) {
	if !validRuntimeDeliveryGroupSettlementRemotePhase(next) {
		return "", fmt.Errorf("%w: remote phase 不受支持", ErrInvalidRuntimeDeliveryGroupSaga)
	}
	if next == "" || next == current {
		return current, nil
	}
	if current == "" || current == RuntimeDeliveryGroupSettlementRemoteAbsent {
		return next, nil
	}
	if current == RuntimeDeliveryGroupSettlementRemotePrepared && (next == RuntimeDeliveryGroupSettlementRemoteCommitted || next == RuntimeDeliveryGroupSettlementRemoteAborted) {
		return next, nil
	}
	return "", fmt.Errorf("%w: remote phase 从 %q 回退或分叉到 %q", ErrConflict, current, next)
}

// AdvanceRuntimeDeliveryGroupSaga applies one monotonic fact under an
// expected revision. The caller's settlement lease remains the execution
// authority; this CAS prevents stale workers from rewriting the decision log.
func AdvanceRuntimeDeliveryGroupSaga(item RuntimeDeliveryGroupSaga, expectedRevision int64, state RuntimeDeliveryGroupSagaState, remotePhase RuntimeDeliveryGroupSettlementRemotePhase, message string, now time.Time) (RuntimeDeliveryGroupSaga, bool, error) {
	now = normalizeRuntimeDeliveryGroupTime(now)
	current, err := item.Normalize(now)
	if err != nil {
		return RuntimeDeliveryGroupSaga{}, false, err
	}
	if expectedRevision <= 0 || current.Revision != expectedRevision {
		return RuntimeDeliveryGroupSaga{}, false, ErrConflict
	}
	state = RuntimeDeliveryGroupSagaState(strings.TrimSpace(strings.ToLower(string(state))))
	remotePhase = RuntimeDeliveryGroupSettlementRemotePhase(strings.TrimSpace(strings.ToLower(string(remotePhase))))
	if !validRuntimeDeliveryGroupSagaState(state) || !runtimeDeliveryGroupSagaTransitionAllowed(current.Decision, current.State, state) {
		return RuntimeDeliveryGroupSaga{}, false, ErrConflict
	}
	mergedRemote, err := mergeRuntimeDeliveryGroupSagaRemotePhase(current.RemotePhase, remotePhase)
	if err != nil {
		return RuntimeDeliveryGroupSaga{}, false, err
	}
	if state == RuntimeDeliveryGroupSagaCommitted {
		if current.Decision != RuntimeDeliveryGroupSagaCommit || mergedRemote != RuntimeDeliveryGroupSettlementRemoteCommitted {
			return RuntimeDeliveryGroupSaga{}, false, fmt.Errorf("%w: commit terminal identity 不一致", ErrInvalidRuntimeDeliveryGroupSaga)
		}
	}
	if state == RuntimeDeliveryGroupSagaAborted {
		if current.Decision != RuntimeDeliveryGroupSagaAbort || mergedRemote != RuntimeDeliveryGroupSettlementRemoteAborted {
			return RuntimeDeliveryGroupSaga{}, false, fmt.Errorf("%w: abort terminal identity 不一致", ErrInvalidRuntimeDeliveryGroupSaga)
		}
	}
	message = SanitizeRuntimeString(strings.TrimSpace(message))
	if len(message) > MaxRuntimeDeliveryGroupSagaErrorLength {
		message = message[:MaxRuntimeDeliveryGroupSagaErrorLength]
	}
	if current.State == state && current.RemotePhase == mergedRemote && current.LastError == message {
		return current, true, nil
	}
	updated := current
	updated.State = state
	updated.RemotePhase = mergedRemote
	if message != "" {
		updated.LastError = message
	}
	updated.Revision++
	updated.UpdatedAt = now
	if state == RuntimeDeliveryGroupSagaCommitted || state == RuntimeDeliveryGroupSagaAborted {
		finished := now
		updated.CompletedAt = &finished
	}
	normalized, normalizeErr := updated.normalize(now)
	return normalized, false, normalizeErr
}

// RuntimeDeliveryGroupSagaRepository stores the source-side decision journal.
type RuntimeDeliveryGroupSagaRepository interface {
	EnqueueRuntimeDeliveryGroupSaga(context.Context, RuntimeDeliveryGroupSaga) (RuntimeDeliveryGroupSaga, error)
	GetRuntimeDeliveryGroupSaga(context.Context, string) (RuntimeDeliveryGroupSaga, error)
	ListRuntimeDeliveryGroupSagas(context.Context, string, RuntimeDeliveryGroupSagaState, int) ([]RuntimeDeliveryGroupSaga, error)
	AdvanceRuntimeDeliveryGroupSaga(context.Context, string, int64, RuntimeDeliveryGroupSagaState, RuntimeDeliveryGroupSettlementRemotePhase, string, time.Time) (RuntimeDeliveryGroupSaga, bool, error)
}
