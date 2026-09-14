package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ensureRuntimeDeliveryGroupSaga makes the decision journal self-healing for
// settlement rows created by older binaries or by a crash between the two
// metadata writes. The stable group/phase identity makes the repair idempotent.
func (c *Coordinator) ensureRuntimeDeliveryGroupSaga(ctx context.Context, group RuntimeDeliveryGroup, phase RuntimeDeliveryGroupSettlementPhase, now time.Time) (RuntimeDeliveryGroupSaga, bool, error) {
	if c == nil || c.repo == nil {
		return RuntimeDeliveryGroupSaga{}, false, nil
	}
	repo, ok := c.repo.(RuntimeDeliveryGroupSagaRepository)
	if !ok {
		return RuntimeDeliveryGroupSaga{}, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	decision := RuntimeDeliveryGroupSagaAbort
	if phase == RuntimeDeliveryGroupSettlementCommit {
		decision = RuntimeDeliveryGroupSagaCommit
	}
	wantID := RuntimeDeliveryGroupSagaID(group.ID, decision)
	saga, err := repo.GetRuntimeDeliveryGroupSaga(context.WithoutCancel(ctx), wantID)
	if err == nil {
		candidate, candidateErr := NewRuntimeDeliveryGroupSaga(group, now)
		if candidateErr != nil {
			return RuntimeDeliveryGroupSaga{}, true, candidateErr
		}
		if !saga.MatchesIdentity(candidate) {
			return RuntimeDeliveryGroupSaga{}, true, fmt.Errorf("%w: saga identity 与 source group 不一致", ErrConflict)
		}
		return saga, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return RuntimeDeliveryGroupSaga{}, true, err
	}
	created, err := NewRuntimeDeliveryGroupSaga(group, now)
	if err != nil {
		return RuntimeDeliveryGroupSaga{}, true, err
	}
	if created.Decision != decision {
		return RuntimeDeliveryGroupSaga{}, true, fmt.Errorf("%w: settlement phase 与 source decision 不一致", ErrConflict)
	}
	stored, err := repo.EnqueueRuntimeDeliveryGroupSaga(context.WithoutCancel(ctx), created)
	if err != nil {
		return RuntimeDeliveryGroupSaga{}, true, err
	}
	return stored, true, nil
}

func runtimeDeliveryGroupSagaTerminalForDecision(decision RuntimeDeliveryGroupSagaDecision) RuntimeDeliveryGroupSagaState {
	if decision == RuntimeDeliveryGroupSagaAbort {
		return RuntimeDeliveryGroupSagaAborted
	}
	return RuntimeDeliveryGroupSagaCommitted
}

func runtimeDeliveryGroupSagaTerminalRemoteForDecision(decision RuntimeDeliveryGroupSagaDecision) RuntimeDeliveryGroupSettlementRemotePhase {
	if decision == RuntimeDeliveryGroupSagaAbort {
		return RuntimeDeliveryGroupSettlementRemoteAborted
	}
	return RuntimeDeliveryGroupSettlementRemoteCommitted
}

func runtimeDeliveryGroupSagaOppositeRemoteForDecision(decision RuntimeDeliveryGroupSagaDecision) RuntimeDeliveryGroupSettlementRemotePhase {
	if decision == RuntimeDeliveryGroupSagaAbort {
		return RuntimeDeliveryGroupSettlementRemoteCommitted
	}
	return RuntimeDeliveryGroupSettlementRemoteAborted
}

// runtimeDeliveryGroupSagaProgressForSettlement translates the settlement
// execution cursor into the independent business journal. A worker may start
// from StageStatus after a restart, so a status/phase observation must never
// regress a saga already known to be preparing/prepared/committing.
func runtimeDeliveryGroupSagaProgressForSettlement(current RuntimeDeliveryGroupSaga, item RuntimeDeliveryGroupSettlement, stage RuntimeDeliveryGroupSettlementStage, remotePhase RuntimeDeliveryGroupSettlementRemotePhase) (RuntimeDeliveryGroupSagaState, RuntimeDeliveryGroupSettlementRemotePhase) {
	remotePhase = RuntimeDeliveryGroupSettlementRemotePhase(strings.TrimSpace(strings.ToLower(string(remotePhase))))
	var desired RuntimeDeliveryGroupSagaState
	switch remotePhase {
	case RuntimeDeliveryGroupSettlementRemoteCommitted:
		if current.Decision == RuntimeDeliveryGroupSagaCommit {
			desired = RuntimeDeliveryGroupSagaCommitted
		} else {
			desired = RuntimeDeliveryGroupSagaCompensationRequired
		}
	case RuntimeDeliveryGroupSettlementRemoteAborted:
		if current.Decision == RuntimeDeliveryGroupSagaAbort {
			desired = RuntimeDeliveryGroupSagaAborted
		} else {
			desired = RuntimeDeliveryGroupSagaCompensationRequired
		}
	case RuntimeDeliveryGroupSettlementRemotePrepared:
		desired = RuntimeDeliveryGroupSagaPrepared
	default:
		switch stage {
		case RuntimeDeliveryGroupSettlementStagePreparing:
			desired = RuntimeDeliveryGroupSagaPreparing
		case RuntimeDeliveryGroupSettlementStagePrepared:
			desired = RuntimeDeliveryGroupSagaPrepared
		case RuntimeDeliveryGroupSettlementStageCommitting:
			desired = RuntimeDeliveryGroupSagaCommitting
		case RuntimeDeliveryGroupSettlementStageAborting:
			desired = RuntimeDeliveryGroupSagaAborting
		case RuntimeDeliveryGroupSettlementStageFailed:
			desired = RuntimeDeliveryGroupSagaFailed
		default:
			desired = RuntimeDeliveryGroupSagaDecided
		}
	}
	if desired != current.State && !runtimeDeliveryGroupSagaTransitionAllowed(current.Decision, current.State, desired) {
		// The durable journal is authoritative when a freshly claimed queue row
		// has an older stage. Preserve it rather than turning a stale observation
		// into a backwards transition.
		desired = current.State
	}
	if remotePhase == RuntimeDeliveryGroupSettlementRemoteAbsent && current.RemotePhase != "" && current.RemotePhase != RuntimeDeliveryGroupSettlementRemoteAbsent {
		remotePhase = current.RemotePhase
	}
	return desired, remotePhase
}

func runtimeDeliveryGroupSagaPermanentError(err error) bool {
	return errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalidRuntimeDeliveryGroupSaga) || errors.Is(err, ErrInvalidRuntimeDeliveryGroupSettlement) || errors.Is(err, ErrInvalidRuntimeDeliveryGroupTransaction) || errors.Is(err, ErrRuntimeDeliveryGroupTransactionAuth) || errors.Is(err, ErrRuntimeDeliveryFenceConflict) || errors.Is(err, ErrRuntimeDeliveryFenceStale) || errors.Is(err, ErrRuntimeDeliveryFenceRequired)
}
