package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// reconcileRuntimeDeliveryStatus applies an authenticated destination proof
// to the source-side attempt journal. It deliberately never treats an
// unauthenticated or unavailable status as success. An absent proof falls
// through to the normal idempotent delivery path; a prepared proof is only
// useful when the source transport supports Commit.
func (c *Coordinator) reconcileRuntimeDeliveryStatus(ctx context.Context, journal RuntimeDeliveryAttempt, journalEnabled bool, owner string, transactional bool, status RuntimeDeliveryStatus, now time.Time) (RuntimeDeliveryAttempt, bool, bool, error) {
	if err := status.Validate(); err != nil {
		return RuntimeDeliveryAttempt{}, false, false, err
	}
	if journalEnabled && !status.Matches(journal.Kind, journal.Source, journal.Destination, journal.DeliveryID, journal.InvocationID) {
		return RuntimeDeliveryAttempt{}, false, false, fmt.Errorf("%w: status metadata 不一致 kind=%q source=%q destination=%q delivery_id=%q invocation_id=%q expected_kind=%q expected_source=%q expected_destination=%q expected_delivery_id=%q expected_invocation_id=%q", ErrRuntimeDeliveryStatusAuth, status.Kind, status.Source, status.Destination, status.DeliveryID, status.InvocationID, journal.Kind, journal.Source, journal.Destination, journal.DeliveryID, journal.InvocationID)
	}
	if status.Phase == RuntimeDeliveryStatusAbsent || !status.Found {
		return journal, false, false, nil
	}
	if status.Phase == RuntimeDeliveryStatusPrepared {
		if !transactional {
			return journal, false, false, nil
		}
		if journalEnabled && journal.Phase == RuntimeDeliveryAttemptStarted {
			updated, _, err := c.advanceRuntimeDeliveryAttempt(ctx, journal, owner, RuntimeDeliveryAttemptPrepared, now, "")
			if err != nil {
				return RuntimeDeliveryAttempt{}, false, false, err
			}
			journal = updated
		}
		return journal, true, false, nil
	}

	// Accepted means the destination inbox is visible. Committed means the
	// destination prepare/commit ledger is terminal. Both are safe proofs for
	// releasing the source cursor; the journal records the stronger committed
	// phase when the current source attempt was already prepared.
	if status.Phase != RuntimeDeliveryStatusAccepted && status.Phase != RuntimeDeliveryStatusCommitted {
		return journal, false, false, nil
	}
	if journalEnabled {
		var err error
		switch journal.Phase {
		case RuntimeDeliveryAttemptStarted:
			if status.Phase == RuntimeDeliveryStatusCommitted {
				journal, _, err = c.advanceRuntimeDeliveryAttempt(ctx, journal, owner, RuntimeDeliveryAttemptPrepared, now, "")
				if err == nil {
					journal, _, err = c.advanceRuntimeDeliveryAttempt(ctx, journal, owner, RuntimeDeliveryAttemptCommitted, now, "")
				}
			} else {
				journal, _, err = c.advanceRuntimeDeliveryAttempt(ctx, journal, owner, RuntimeDeliveryAttemptAccepted, now, "")
			}
		case RuntimeDeliveryAttemptPrepared:
			// A visible inbox is sufficient proof even if the transaction row
			// was compacted or the source switched delivery modes.
			journal, _, err = c.advanceRuntimeDeliveryAttempt(ctx, journal, owner, RuntimeDeliveryAttemptCommitted, now, "")
		case RuntimeDeliveryAttemptAccepted, RuntimeDeliveryAttemptCommitted:
		default:
			return RuntimeDeliveryAttempt{}, false, false, errors.New("Runtime delivery status 与 attempt phase 不一致")
		}
		if err != nil {
			return RuntimeDeliveryAttempt{}, false, false, err
		}
		journal, _, err = c.advanceRuntimeDeliveryAttempt(ctx, journal, owner, RuntimeDeliveryAttemptCompleted, now, "")
		if err != nil {
			return RuntimeDeliveryAttempt{}, false, false, err
		}
	}
	return journal, true, true, nil
}
